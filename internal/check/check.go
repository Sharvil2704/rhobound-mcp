// SPDX-License-Identifier: Apache-2.0

// Package check is the data contract's gate on a capture (docs/mcp/DATA.md 3): it sniffs every object, reads the
// capture under limits with every file's sha256 checked, and runs the structural detectors that name what would
// otherwise give a confident wrong answer (sampling, missing CLIENT spans, context that is not propagated, duplicate
// spans, and the rest of DATA.md 3.14). It also writes the canonical form of a capture (Cut). Every finding is a code of
// the registry in internal/contract.
package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// PodIdentityHost is the --pod-identity value that keys the pods of resources with no platform identity by host.name:
// for pre-fork servers, whose worker processes share one listening socket (DATA.md 3.4).
const PodIdentityHost = "host"

// Options are what check is told besides the capture.
type Options struct {
	Limits            otlp.Limits       // the reading limits; the zero value means otlp.DefaultLimits
	DeclaredKeptShare *float64          // --kept-share, a declaration cross-checked against derived shares
	RequestCount      *RequestCount     // an independent count of entry requests (DATA.md 6)
	DeclaredSamplers  map[string]string // SDK sampler by service (OTEL_TRACES_SAMPLER values)
	ExtraProbeRoutes  []string          // routes declared to be probes (http.route or rpc.method, optionally after a method)
	PodIdentity       string            // "" or PodIdentityHost
	// WindowStart and WindowEnd are the window `check --window` will cut, or both zero (WindowNanos says which windows
	// are). With them, trace.mixed_images counts the image digests of the resources that count in [WindowStart,
	// WindowEnd) only (inWindow, DATA.md 3.9): every resource the cut of the window keeps, and those that hold a span
	// overlapping it. An image a rollout replaced before the window, read because a trace file reaches back past it, then
	// does not refuse the cut; every other detector reads the capture as read.
	WindowStart, WindowEnd time.Time
}

// The files of a capture folder that are inputs of other kinds, never trace objects.
var otherInputs = map[string]bool{"manifest.json": true, "request-count.json": true, "peer-map.json": true}

// isObject reports whether a file of a capture's trace folder is one of its objects: a trace file, or an object the
// reader cannot parse (named, never skipped). In the capture folder itself, the other inputs are not objects.
func isObject(name string, inCaptureRoot bool) bool {
	if strings.HasPrefix(name, ".") || inCaptureRoot && otherInputs[name] {
		return false
	}
	return otlp.IsTraceFile(name) || IsUnreadableObject(name)
}

// ManifestFromDir builds the manifest of a local capture: a folder with traces/, a folder of trace files, or one trace
// file. Its window is left for Run to fill from the data (cut "none"); ignored counts the folder's files that are no
// objects, by suffix. An error is a usage error: no such path, or no objects in it.
func ManifestFromDir(capture string, lim otlp.Limits) (*contract.Manifest, map[string]int64, error) {
	if lim == (otlp.Limits{}) {
		lim = otlp.DefaultLimits
	}
	st, err := os.Stat(capture)
	if err != nil {
		return nil, nil, err
	}
	m := &contract.Manifest{Schema: contract.ManifestSchema, Source: contract.Source{Kind: "dir"},
		Window:   contract.Window{Cut: contract.CutNone},
		Sampling: contract.Sampling{Evidence: contract.KeptAssumed},
		Limits:   contract.Limits{MaxObjectBytes: lim.MaxObjectBytes, MaxCaptureBytes: lim.MaxCaptureBytes}}
	ignored := map[string]int64{}
	var files []string
	if !st.IsDir() {
		m.Dir = filepath.Dir(capture)
		m.Partial = true // the other files of its folder are not this capture's
		files = []string{capture}
	} else {
		m.Dir = capture
		tdir := otlp.TraceDir(capture)
		ents, err := os.ReadDir(tdir)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if isObject(e.Name(), tdir == capture) {
				files = append(files, filepath.Join(tdir, e.Name()))
			} else if !otherInputs[e.Name()] || tdir != capture {
				ignored["file "+suffix(e.Name())]++
			}
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("no trace files (*.jsonl, *.json, *.gz) under %s", capture)
	}
	for _, f := range files {
		sum, size, err := fileSHA256(f)
		if err != nil {
			return nil, nil, err
		}
		rel, err := filepath.Rel(m.Dir, f)
		if err != nil {
			return nil, nil, err
		}
		m.Objects = append(m.Objects, contract.Object{Name: filepath.ToSlash(rel), Size: size, SHA256: sum})
	}
	return m, ignored, nil
}

func suffix(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		if s, untrusted := contract.SafeName(name[i:]); !untrusted && len(s) <= 16 {
			return s
		}
	}
	return "(other)"
}

func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ReadManifest reads a capture manifest from a file; its objects are relative to the file's folder.
func ReadManifest(p string) (*contract.Manifest, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m, err := contract.ParseManifest(b)
	if err != nil {
		return nil, err
	}
	m.Dir = filepath.Dir(p)
	return m, nil
}

// Run checks the capture a manifest lists and returns the check report, and the capture as read (nil when it could not
// be read: a refusal at the sniff stage, a limit, a sha256 that differs, or a value that does not decode). The returned
// data is what a fit reads: identical duplicate spans removed (order otherwise kept), and pods keyed by host when
// Options.PodIdentity says so. Run fills the manifest's window when it is cut "none" and zero, its sampling evidence and
// its cluster dump record, so the report's manifest digest is that of the completed manifest. The error is a tool
// error: an I/O error, or a bad option.
func Run(m *contract.Manifest, opt Options) (*contract.CheckReport, *otlp.Data, error) {
	if opt.PodIdentity != "" && opt.PodIdentity != PodIdentityHost {
		return nil, nil, fmt.Errorf("--pod-identity %q: the only value is %q", opt.PodIdentity, PodIdentityHost)
	}
	if k := opt.DeclaredKeptShare; k != nil && !(*k > 0 && *k <= 1) {
		return nil, nil, fmt.Errorf("--kept-share must be in (0, 1], got %v", *k)
	}
	if s, e := opt.WindowStart, opt.WindowEnd; !(s.IsZero() && e.IsZero()) {
		if _, _, err := WindowNanos(s, e); err != nil {
			return nil, nil, err
		}
	}
	lim := opt.Limits
	if lim == (otlp.Limits{}) {
		lim = otlp.DefaultLimits
	}
	m.Limits = contract.Limits{MaxObjectBytes: lim.MaxObjectBytes, MaxCaptureBytes: lim.MaxCaptureBytes}
	kept := contract.KeptShare{Value: 1, Source: contract.KeptAssumed}
	if opt.DeclaredKeptShare != nil {
		kept = contract.KeptShare{Value: *opt.DeclaredKeptShare, Source: contract.KeptDeclared}
	}
	finish := func(fs []contract.Finding, ignored map[string]int64, d *otlp.Data) (*contract.CheckReport, *otlp.Data, error) {
		m.Sampling.Evidence = kept.Source
		if rc := opt.RequestCount; rc != nil && kept.Source == contract.KeptCounted {
			dg := rc.Digest
			m.Sampling.IndependentCount = &dg
		}
		if len(opt.DeclaredSamplers) > 0 {
			m.Sampling.DeclaredSDKSamplers = opt.DeclaredSamplers
		}
		return contract.NewCheckReport(fs, ignored, m.Digest(), kept), d, nil
	}

	// the objects against the folder
	refs, fs, err := objectRefs(m)
	if err != nil {
		return nil, nil, err
	}
	var total int64
	for _, o := range m.Objects {
		total += o.Size
		if o.Size > lim.MaxObjectBytes {
			fs = append(fs, contract.NewFinding(contract.StoreTooLarge).WithSubject(path.Base(o.Name)).WithNumber("bytes", float64(o.Size)).
				WithNumber("limit", float64(lim.MaxObjectBytes)))
		}
	}
	if total > lim.MaxCaptureBytes {
		fs = append(fs, contract.NewFinding(contract.StoreTooLarge).WithNumber("bytes", float64(total)).
			WithNumber("limit", float64(lim.MaxCaptureBytes)))
	}
	// The sniff stage sees the head of each object: a refusal there stops before the read, except for id encodings,
	// which the read classifies over the whole capture (a head may show one encoding of a mixed capture).
	for _, f := range Sniff(refs) {
		if !idCodes[f.Code] {
			fs = append(fs, f)
		}
	}
	if refused(fs) {
		return finish(fs, nil, nil)
	}

	// the read
	d, err := otlp.ReadFiles(refs, lim)
	if err != nil {
		f, ok := readFinding(err, m, refs)
		if !ok {
			return nil, nil, err
		}
		return finish(append(fs, f), nil, nil)
	}
	if m.Window.Cut == contract.CutNone && m.Window.Start.IsZero() {
		if s, e, ok := DataWindow(d); ok {
			m.Window.Start, m.Window.End = s, e
		}
	}
	a := newAnalysis(d, m, opt)
	if a.dump, _, a.peerNames, a.clusterFs, err = ClusterInputs(m); err != nil {
		return nil, nil, err
	}
	fs = append(fs, a.run()...)
	kept = a.kept
	if a.clusterDump != nil {
		m.ClusterDump = a.clusterDump
	}
	return finish(fs, d.Ignored, d)
}

// idCodes are the id-encoding refusals, which the read decides (analysis.readStats).
var idCodes = map[string]bool{contract.TraceIDsBase64: true, contract.TraceIDsMixed: true, contract.TraceTraceID64Bit: true}

func refused(fs []contract.Finding) bool {
	for _, f := range fs {
		if f.Level == contract.Refuse {
			return true
		}
	}
	return false
}

// objectRefs resolves the manifest's objects to files, checking that each exists with its listed size, and that the
// folders they are in hold no other trace object (the count of DATA.md 4.5).
func objectRefs(m *contract.Manifest) ([]otlp.FileRef, []contract.Finding, error) {
	var refs []otlp.FileRef
	var fs []contract.Finding
	listed := map[string]bool{}
	dirs := map[string]bool{}
	for i, o := range m.Objects {
		if err := contract.CheckObjectName(o.Name); err != nil {
			return nil, nil, err
		}
		listed[o.Name] = true
		dirs[path.Dir(o.Name)] = true
		p := filepath.Join(m.Dir, filepath.FromSlash(o.Name))
		st, err := os.Stat(p)
		mismatch := func(what string, n float64) {
			fs = append(fs, contract.NewFinding(contract.StoreManifestMismatch).WithSubject(path.Base(o.Name)).
				WithNumber("object", float64(i)).WithNumber(what, n))
		}
		switch {
		case errors.Is(err, os.ErrNotExist):
			mismatch("missing", 1)
			continue
		case err != nil:
			return nil, nil, err
		case st.Size() != o.Size:
			mismatch("size", float64(st.Size()))
			continue
		}
		refs = append(refs, otlp.FileRef{Path: p, SHA256: o.SHA256})
	}
	var ds []string
	for dir := range dirs {
		if !m.Partial {
			ds = append(ds, dir)
		}
	}
	sort.Strings(ds)
	for _, dir := range ds {
		ents, err := os.ReadDir(filepath.Join(m.Dir, filepath.FromSlash(dir)))
		if err != nil {
			continue
		}
		for _, e := range ents {
			rel := path.Join(dir, e.Name())
			if !e.IsDir() && !listed[rel] && isObject(e.Name(), dir == ".") && otlp.IsTraceFile(e.Name()) {
				fs = append(fs, contract.NewFinding(contract.StoreManifestMismatch).WithSubject(e.Name()).WithNumber("unlisted", 1))
			}
		}
	}
	return refs, fs, nil
}

// readFinding turns a refusal of the reader into its finding.
func readFinding(err error, m *contract.Manifest, refs []otlp.FileRef) (contract.Finding, bool) {
	var le *otlp.LimitError
	if errors.As(err, &le) {
		return contract.NewFinding(contract.InputTooLarge).WithNumber("limit", float64(le.Limit)), true
	}
	var oe *otlp.Error
	if !errors.As(err, &oe) {
		return contract.Finding{}, false
	}
	code := oe.Code
	if code == contract.TraceBadJSON {
		code = refineBadJSON(oe.Err)
	}
	f := contract.NewFinding(code).WithSubject(filepath.Base(oe.Path))
	for i, r := range refs {
		if r.Path == oe.Path {
			f = f.WithNumber("object", float64(i))
		}
	}
	if oe.Line > 0 {
		f = f.WithNumber("line", float64(oe.Line))
	}
	if oe.Value >= 0 && code != contract.StoreManifestMismatch && code != contract.StoreCompressionMismatch {
		f = f.WithNumber("value", float64(oe.Value))
	}
	return f, true
}

// refineBadJSON names a value that does not decode more precisely when the decoder says why: a span kind written as a
// string (the sniff stage names the proto3 enum names; a string seen only here is trace.kind_unknown).
func refineBadJSON(err error) string {
	var te *json.UnmarshalTypeError
	if errors.As(err, &te) && te.Value == "string" && strings.HasSuffix(te.Field, ".kind") {
		return contract.TraceKindUnknown
	}
	return contract.TraceBadJSON
}

// DataWindow is the window a capture's data spans: from its earliest span start to its latest span end, in UTC.
func DataWindow(d *otlp.Data) (time.Time, time.Time, bool) {
	if len(d.Spans) == 0 {
		return time.Time{}, time.Time{}, false
	}
	lo, hi := d.Spans[0].Start, d.Spans[0].End
	for i := range d.Spans {
		lo = min(lo, d.Spans[i].Start)
		hi = max(hi, d.Spans[i].End)
	}
	return time.Unix(0, int64(lo)).UTC(), time.Unix(0, int64(hi)).UTC(), true
}
