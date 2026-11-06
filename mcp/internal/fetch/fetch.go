// SPDX-License-Identifier: Apache-2.0

// Package fetch is the window fetcher of DATA.md 4.3. It maps an environment's trace-store prefix and a window to
// the minute partitions the Collector's S3 exporter wrote, refuses what the reader cannot use before any object is
// downloaded in full, downloads the rest in parallel into a fresh folder, and writes a preliminary capture
// manifest. It never parses spans: cutting by entry-span start, dropping duplicates and the structural checks need
// the reader and run afterwards in `rhobound check --cut`. The one question here that needs span start times (does
// the last margin minute still hold spans of the window?) goes through WindowChecker, which the caller implements.
package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
)

// EnvConfig is one environment's trace store, from server configuration (never from a tool argument).
type EnvConfig struct {
	Store objstore.Store
	// Prefix is the environment's prefix in the store; it ends in "/" ("" is the whole store).
	Prefix string
	// Retry, Batch and SDKDelay are the Collector exporter's retry time, its batch timeout and the SDK's export
	// delay; their sum is the margin read after the window. All zero means DefaultMarginAfter.
	Retry, Batch, SDKDelay time.Duration
	// MaxListedBytes caps the listing's total size, checked before anything is read; 0 means
	// DefaultMaxListedBytes.
	MaxListedBytes int64
}

// Request is one window of one environment.
type Request struct {
	Env        string // the environment's name, recorded in the manifest ("" for a capture that is no environment)
	Start, End time.Time
}

// Result is a fetched window: Dir holds one file per object and the preliminary manifest, ManifestPath. Findings
// are the doubts and notes (store.late_batches, store.small_batches), each a defs.v1.json Finding.
type Result struct {
	Dir, ManifestPath string
	Findings          []json.RawMessage
}

// WindowChecker answers the one question the fetcher cannot answer without parsing spans.
type WindowChecker interface {
	// LateSpans returns how many spans in the trace files under dir start inside [start, end).
	LateSpans(ctx context.Context, dir string, start, end time.Time) (int, error)
}

// Fetcher fetches one window into a fresh folder under workRoot.
type Fetcher interface {
	Fetch(ctx context.Context, cfg EnvConfig, req Request, workRoot string, wc WindowChecker) (*Result, error)
}

const (
	// MarginBefore is read before the window (DATA.md 3.9).
	MarginBefore = 2 * time.Minute
	// DefaultMarginAfter is the margin after the window when the export delays are not configured [D]: the
	// exporter's default retry of 300 s, a 10 s batch, a 5 s SDK delay, and slack.
	DefaultMarginAfter = 7 * time.Minute
	// DefaultMaxListedBytes caps a window's listing [D]. At the usual 10x gzip ratio it is about the reader's
	// 8 GiB cap on a capture's inflated bytes.
	DefaultMaxListedBytes = 1 << 30
	// MaxObjectBytes and MaxCaptureBytes are the reader's default caps on inflated bytes, recorded in the
	// manifest; an object listed above MaxObjectBytes cannot pass the reader and is refused here.
	MaxObjectBytes  = 1 << 30
	MaxCaptureBytes = 8 << 30
	// MaxWindow bounds a window, and so the number of partition listings [D].
	MaxWindow = 2 * time.Hour
	// SmallBatchesPerMinute is the store.small_batches threshold [D].
	SmallBatchesPerMinute = 30
	// putPerThousandUSD is the S3 PUT price per 1,000 requests used for the cost figure of store.small_batches
	// (us-east-1 list price, DEPLOY_AWS.md 12).
	putPerThousandUSD = 0.005
	minutesPerMonth   = 60 * 24 * 30
)

// ManifestName is the manifest's file name inside the capture folder.
const ManifestName = "manifest.json"

// Window is the fetcher. The zero value is ready to use.
type Window struct {
	Parallel   int   // concurrent downloads; 0 means 8
	SniffBytes int64 // bytes read from the start of every object before anything is downloaded; 0 means 4096
}

// Default is the fetcher with its default settings.
var Default Fetcher = Window{}

// listed is one object of the window.
type listed struct {
	objstore.Object
	rel, sfx, name, sha string
	read                int64 // bytes downloaded
	part                time.Time
	hasPart             bool
	start               time.Time
	hasStart            bool
}

var envRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Fetch fetches the window of DATA.md 4.3, steps 1-4 and the preliminary manifest of step 6: list the minute
// partitions of the window and its margins; refuse above the size cap; sniff every object's head (suffix,
// compression, the partition time zone); download into a fresh random folder; re-check the last margin minute
// for late batches, extending the listing once; count the files; write the manifest.
func (w Window) Fetch(ctx context.Context, cfg EnvConfig, req Request, workRoot string, wc WindowChecker) (*Result, error) {
	if cfg.Store == nil || wc == nil {
		return nil, errors.New("fetch: a store and a window checker are required")
	}
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		return nil, errors.New("fetch: the environment's prefix must end in /")
	}
	if req.Env != "" && !envRE.MatchString(req.Env) {
		return nil, errors.New("fetch: the environment name is not a configured name")
	}
	start, end := req.Start.UTC(), req.End.UTC()
	if !end.After(start) || end.Sub(start) > MaxWindow {
		return nil, fmt.Errorf("fetch: the window must end after it starts and last at most %v", MaxWindow)
	}
	after, afterFrom := marginAfter(cfg)
	maxListed := cfg.MaxListedBytes
	if maxListed <= 0 {
		maxListed = DefaultMaxListedBytes
	}
	subject := req.Env

	// partitions are whole minutes: the listing runs from the minute of start - before to the minute that
	// holds end + after, inclusive
	from, to := start.Add(-MarginBefore).Truncate(time.Minute), ceilMinute(end.Add(after))
	objs, err := w.list(ctx, cfg, from, to, nil)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, w.emptyWindow(ctx, cfg, subject, start, from, to)
	}
	if err := w.admit(ctx, cfg.Store, objs, objs, maxListed, subject); err != nil {
		return nil, err
	}
	if off, n, ok := partitionOffset(objs); ok && (off >= tzThreshold || off <= -tzThreshold) {
		return nil, tzRefusal(subject, off, n)
	}
	taken := map[string]bool{ManifestName: true}
	if err := assignNames(objs, taken); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp(workRoot, "capture-")
	if err != nil {
		return nil, err
	}
	res, err := w.fetchInto(ctx, cfg, req, dir, workRoot, wc, objs, taken, from, to, after, afterFrom, maxListed)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return res, nil
}

func (w Window) fetchInto(ctx context.Context, cfg EnvConfig, req Request, dir, workRoot string, wc WindowChecker,
	objs []*listed, taken map[string]bool, from, to time.Time, after time.Duration, afterFrom string,
	maxListed int64) (*Result, error) {
	start, end, subject := req.Start.UTC(), req.End.UTC(), req.Env
	if err := w.download(ctx, cfg.Store, dir, objs, subject); err != nil {
		return nil, err
	}
	smallBatches := smallBatchesFinding(objs, from, to, subject)

	// DATA.md 3.9: the last margin minute must hold no span that starts inside the window; if it does, extend
	// the listing once by the same margin and check the new last minute
	var findings []Finding
	afterTotal := after
	late, err := lateSpans(ctx, wc, workRoot, dir, inMinute(objs, to.Add(-time.Nanosecond)), start, end)
	if err != nil {
		return nil, err
	}
	if late > 0 {
		ext, err := w.list(ctx, cfg, to, ceilMinute(to.Add(after)), objs)
		if err != nil {
			return nil, err
		}
		if err := w.admit(ctx, cfg.Store, ext, append(append([]*listed{}, objs...), ext...), maxListed, subject); err != nil {
			return nil, err
		}
		if err := assignNames(ext, taken); err != nil {
			return nil, err
		}
		if err := w.download(ctx, cfg.Store, dir, ext, subject); err != nil {
			return nil, err
		}
		objs = append(objs, ext...)
		sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
		to, afterTotal = ceilMinute(to.Add(after)), 2*after
		afterFrom += ", extended once by the same margin"
		if late, err = lateSpans(ctx, wc, workRoot, dir, inMinute(ext, to.Add(-time.Nanosecond)), start, end); err != nil {
			return nil, err
		}
		if late > 0 {
			findings = append(findings, Finding{
				Code: codeLateBatches, Level: "doubt", Subject: subject,
				Numbers: map[string]float64{"late_spans": float64(late), "margin_after_s": afterTotal.Seconds()},
				Fix:     "raise the margin after the window (the Collector's retry, batch and SDK delays); check the Collector's export errors",
			})
		}
	}
	if smallBatches != nil {
		findings = append(findings, *smallBatches)
	}
	if err := countFiles(dir, objs, subject); err != nil {
		return nil, err
	}
	m := newManifest(cfg, req, objs, afterTotal, afterFrom)
	path := filepath.Join(dir, ManifestName)
	if err := os.WriteFile(path, m, 0o600); err != nil {
		return nil, err
	}
	res := &Result{Dir: dir, ManifestPath: path}
	for _, f := range findings {
		b, err := json.Marshal(f)
		if err != nil {
			return nil, err
		}
		res.Findings = append(res.Findings, b)
	}
	return res, nil
}

func ceilMinute(t time.Time) time.Time {
	if f := t.Truncate(time.Minute); !f.Equal(t) {
		return f.Add(time.Minute)
	}
	return t
}

func marginAfter(cfg EnvConfig) (time.Duration, string) {
	sum := cfg.Retry + cfg.Batch + cfg.SDKDelay
	if sum <= 0 {
		return DefaultMarginAfter, "default 7 min [D]"
	}
	return sum, fmt.Sprintf("retry %g s + batch %g s + sdk %g s",
		cfg.Retry.Seconds(), cfg.Batch.Seconds(), cfg.SDKDelay.Seconds())
}

// list lists the minute partitions [from, to) under the prefix, sorted by key, each key once; keys already in
// seen are left out.
func (w Window) list(ctx context.Context, cfg EnvConfig, from, to time.Time, seen []*listed) ([]*listed, error) {
	have := map[string]bool{}
	for _, o := range seen {
		have[o.Key] = true
	}
	byKey := map[string]*listed{}
	for m := from.Truncate(time.Minute); m.Before(to); m = m.Add(time.Minute) {
		got, err := cfg.Store.List(ctx, cfg.Prefix+objstore.Partition(m))
		if err != nil {
			return nil, err
		}
		for _, o := range got {
			if !strings.HasPrefix(o.Key, cfg.Prefix) {
				return nil, fmt.Errorf("fetch: the store listed a key outside the prefix")
			}
			if have[o.Key] {
				continue
			}
			if prev, ok := byKey[o.Key]; ok {
				// a listing that repeats a key (a page boundary read twice) is harmless; one that
				// shows two objects under one key is not
				if prev.Size != o.Size || prev.ETag != o.ETag {
					return nil, fmt.Errorf("fetch: the store listed one key twice with different contents")
				}
				continue
			}
			l := &listed{Object: o, rel: strings.TrimPrefix(o.Key, cfg.Prefix), sfx: suffix(o.Key)}
			l.part, l.hasPart = objstore.ParsePartition(l.rel)
			byKey[o.Key] = l
		}
	}
	out := make([]*listed, 0, len(byKey))
	for _, l := range byKey {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// admit runs every check that needs only the listing and the heads, in this order: the size caps (no read at
// all), the suffixes (no read), then the head of every object (compression, first span start). all is the whole
// window, for the total size.
func (w Window) admit(ctx context.Context, st objstore.Store, objs, all []*listed, maxListed int64, subject string) error {
	var total int64
	var big []*listed
	for _, o := range all {
		total += o.Size
	}
	for _, o := range objs {
		if o.Size > MaxObjectBytes {
			big = append(big, o)
		}
	}
	fixLarge := registryFix(codeTooLarge)
	if total > maxListed {
		return refuse(codeTooLarge, subject,
			fmt.Sprintf("the window's %d objects total %d bytes in the listing, over the cap of %d", len(all), total, maxListed),
			fixLarge, map[string]float64{"listed_bytes": float64(total), "cap_bytes": float64(maxListed),
				"objects": float64(len(all))}, nil)
	}
	if len(big) > 0 {
		return refuse(codeTooLarge, subject,
			fmt.Sprintf("%d objects are larger than the reader's cap of %d bytes per object", len(big), MaxObjectBytes),
			fixLarge, map[string]float64{"objects": float64(len(big)), "cap_bytes": MaxObjectBytes}, examples(big))
	}
	var bad []*listed
	for _, o := range objs {
		if o.sfx == "" {
			bad = append(bad, o)
		}
	}
	if len(bad) > 0 {
		return refuse(codeUnreadable, subject,
			fmt.Sprintf("%d of the window's %d objects have a suffix the reader does not parse (it reads .json, .jsonl and their .gz forms); the reader would skip them silently", len(bad), len(objs)),
			"export with marshaler: otlp_json and compression: gzip",
			map[string]float64{"objects": float64(len(bad))}, examples(bad))
	}
	sniff := w.SniffBytes
	if sniff <= 0 {
		sniff = 4096
	}
	var wrong []*listed
	for _, o := range objs {
		head, err := st.Head(ctx, o.Key, sniff)
		if err != nil {
			return err
		}
		gz := strings.HasSuffix(o.sfx, ".gz")
		if gz != isGzip(head) {
			wrong = append(wrong, o)
			continue
		}
		o.start, o.hasStart = firstStart(head)
	}
	if len(wrong) > 0 {
		return refuse(codeCompression, subject,
			fmt.Sprintf("%d objects are compressed otherwise than their names say (the reader inflates exactly the .gz names)", len(wrong)),
			"fix the exporter: compression: gzip with the .gz names it writes, and no other compression",
			map[string]float64{"objects": float64(len(wrong))}, examples(wrong))
	}
	return nil
}

func tzRefusal(subject string, off time.Duration, n int) *RefusalError {
	return refuse(codeTZOffset, subject,
		fmt.Sprintf("the partition minutes run %+.2f hours from the span starts they hold: the partitions are not in UTC", offsetHours(off)),
		"set s3_partition_timezone: UTC on the Collector's S3 exporter (its default is Local)",
		map[string]float64{"derived_offset_h": offsetHours(off), "objects_sampled": float64(n)}, nil)
}

// emptyWindow decides why the window holds no object. Partitions written in local time put the window's spans
// some whole hours away, so the window's first minute is probed at offsets of up to 14 hours each way: the first
// probe that finds an object with a span start either shows such an offset (refused as
// store.partition_tz_offset) or shows UTC partitions (the window is simply empty).
func (w Window) emptyWindow(ctx context.Context, cfg EnvConfig, subject string, start, from, to time.Time) error {
	sniff := w.SniffBytes
	if sniff <= 0 {
		sniff = 4096
	}
probes:
	for h := 1; h <= 14; h++ {
		for _, sign := range []int{-1, 1} {
			m := start.Add(time.Duration(sign*h) * time.Hour).Truncate(time.Minute)
			got, err := w.list(ctx, cfg, m, m.Add(time.Minute), nil)
			if err != nil {
				return err
			}
			for _, o := range got[:min(len(got), 3)] {
				head, err := cfg.Store.Head(ctx, o.Key, sniff)
				if err != nil {
					return err
				}
				o.start, o.hasStart = firstStart(head)
			}
			off, n, ok := partitionOffset(got)
			if !ok {
				continue
			}
			if off >= tzThreshold || off <= -tzThreshold {
				return tzRefusal(subject, off, n)
			}
			break probes
		}
	}
	return refuse(codeTooFewEntries, subject,
		fmt.Sprintf("the window's minute partitions, %s to %s with the margins, hold no objects", from.Format(time.RFC3339), to.Format(time.RFC3339)),
		"check that the environment's Collector writes to this prefix, or choose a window with traffic",
		map[string]float64{"objects": 0}, nil)
}

// download copies every object into dir under its file name, hashing as it writes, and checks each size against
// the listing. A size that differs is collected, not raced: the refusal names the first objects in key order.
func (w Window) download(ctx context.Context, st objstore.Store, dir string, objs []*listed, subject string) error {
	par := w.Parallel
	if par <= 0 {
		par = 8
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
		jobs  = make(chan *listed)
	)
	for range min(par, len(objs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for o := range jobs {
				if err := get(ctx, st, dir, o); err != nil {
					once.Do(func() { first = err; cancel() })
				}
			}
		}()
	}
feed:
	for _, o := range objs {
		select {
		case jobs <- o:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var changed []*listed
	for _, o := range objs {
		if o.read != o.Size {
			changed = append(changed, o)
		}
	}
	if len(changed) > 0 {
		c := changed[0]
		return refuse(codeMismatch, subject,
			fmt.Sprintf("%d objects read other sizes than the listing gave (the first: %d bytes, listed %d): they changed while the window was fetched", len(changed), c.read, c.Size),
			"re-fetch the window; never edit a capture in place",
			map[string]float64{"objects": float64(len(changed)), "listed_bytes": float64(c.Size), "read_bytes": float64(c.read)},
			examples(changed))
	}
	return nil
}

func get(ctx context.Context, st objstore.Store, dir string, o *listed) error {
	rc, err := st.Open(ctx, o.Key)
	if err != nil {
		return err
	}
	defer rc.Close()
	f, err := os.OpenFile(filepath.Join(dir, o.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	// one byte past the listed size is enough to see that the object grew
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(rc, o.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	o.read, o.sha = n, hex.EncodeToString(h.Sum(nil))
	return nil
}

func inMinute(objs []*listed, t time.Time) []*listed {
	m := t.Truncate(time.Minute)
	var out []*listed
	for _, o := range objs {
		if o.hasPart && o.part.Equal(m) {
			out = append(out, o)
		}
	}
	return out
}

// lateSpans asks the checker about the given objects only, through a probe folder of hard links beside dir.
func lateSpans(ctx context.Context, wc WindowChecker, workRoot, dir string, objs []*listed, start, end time.Time) (int, error) {
	if len(objs) == 0 {
		return 0, nil
	}
	probe, err := os.MkdirTemp(workRoot, "late-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(probe)
	for _, o := range objs {
		src, dst := filepath.Join(dir, o.name), filepath.Join(probe, o.name)
		if err := os.Link(src, dst); err != nil {
			if err := copyFile(src, dst); err != nil {
				return 0, err
			}
		}
	}
	return wc.LateSpans(ctx, probe, start, end)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// smallBatchesFinding counts objects per minute partition over [from, to). The keys do not say which Collector
// wrote an object, so a minute partition stands for one Collector-minute; with several Collectors on one prefix
// the note comes sooner than DATA.md's per-Collector threshold.
func smallBatchesFinding(objs []*listed, from, to time.Time, subject string) *Finding {
	per := map[time.Time]int{}
	for _, o := range objs {
		if o.hasPart {
			per[o.part]++
		}
	}
	var counts []int
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		counts = append(counts, per[m])
	}
	if len(counts) == 0 {
		return nil
	}
	sort.Ints(counts)
	med := counts[len(counts)/2]
	if med <= SmallBatchesPerMinute {
		return nil
	}
	puts := float64(med) * minutesPerMonth
	return &Finding{
		Code: codeSmallBatches, Level: "note", Subject: subject,
		Numbers: map[string]float64{
			"objects_per_minute":           float64(med),
			"put_requests_per_month":       puts,
			"put_cost_usd_per_month":       math.Round(puts/1000*putPerThousandUSD*100) / 100,
			"threshold_objects_per_minute": SmallBatchesPerMinute,
		},
		Fix: "raise the Collector's batch timeout to 5-10 s",
	}
}

// countFiles checks that dir holds exactly one regular file per object, under its name (DATA.md 4.3, step 4).
func countFiles(dir string, objs []*listed, subject string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, o := range objs {
		want[o.name] = true
	}
	n, stray := 0, 0
	for _, e := range ents {
		if e.Type().IsRegular() && want[e.Name()] {
			n++
		} else {
			stray++
		}
	}
	if n != len(objs) || stray > 0 {
		return refuse(codeMismatch, subject,
			fmt.Sprintf("the capture folder holds %d of the %d listed objects and %d other entries", n, len(objs), stray),
			"re-fetch the window; never edit a capture in place",
			map[string]float64{"files": float64(n), "objects": float64(len(objs)), "other_entries": float64(stray)}, nil)
	}
	return nil
}
