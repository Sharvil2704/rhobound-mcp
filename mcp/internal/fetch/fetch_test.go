// SPDX-License-Identifier: Apache-2.0

package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata/")

// the window of every test: 12:00 to 12:10 UTC; with the default margins the listing runs 11:58 to 12:17
var (
	t0  = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	t1  = t0.Add(10 * time.Minute)
	req = Request{Env: "prod", Start: t0, End: t1}
)

const prefix = "env=prod/"

// request is one OTLP/JSON export request, one line, with a SERVER span per start time.
func request(starts ...time.Time) []byte {
	var spans []string
	for i, s := range starts {
		spans = append(spans, fmt.Sprintf(
			`{"traceId":"%032x","spanId":"%016x","name":"GET /","kind":2,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"}`,
			s.UnixNano(), i+1, s.UnixNano(), s.Add(5*time.Millisecond).UnixNano()))
	}
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]},` +
		`"scopeSpans":[{"spans":[` + strings.Join(spans, ",") + "]}]}]}\n")
}

// gz writes a gzip member with one stored deflate block by hand, so that the golden manifest (sizes, ETags,
// sha256) does not depend on the compressor of the Go release that runs the test.
func gz(b []byte) []byte {
	if len(b) > 0xffff {
		panic("gz: one stored block holds at most 65,535 bytes")
	}
	out := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 1}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(b)))
	out = binary.LittleEndian.AppendUint16(out, ^uint16(len(b)))
	out = append(out, b...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(b))
	return binary.LittleEndian.AppendUint32(out, uint32(len(b)))
}

func TestFirstStartReadsATruncatedCompressedHead(t *testing.T) {
	var starts []time.Time
	for i := range 2000 {
		starts = append(starts, t0.Add(time.Duration(i)*time.Millisecond))
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(request(starts...))
	zw.Close()
	if buf.Len() <= 4096 {
		t.Fatalf("the object is only %d bytes compressed", buf.Len())
	}
	got, ok := firstStart(buf.Bytes()[:4096])
	if !ok || !got.Equal(t0) {
		t.Fatalf("first start %v %v, want %v", got, ok, t0)
	}
	if _, ok := firstStart(gz(request(t0))[:40]); ok {
		t.Fatal("a head cut before the first span start found one")
	}
	if got, ok := firstStart(request(t0)); !ok || !got.Equal(t0) {
		t.Fatalf("plain head: %v %v", got, ok)
	}
}

type putter interface{ Put(key string, data []byte) }

// fill writes perMinute objects into every UTC minute of [from, to), each holding one span that started 20 s
// before its minute (the export delay), under partitions written in loc.
func fill(p putter, from, to time.Time, perMinute int, loc *time.Location) {
	for m := from; m.Before(to); m = m.Add(time.Minute) {
		for j := range perMinute {
			k := prefix + objstore.Partition(m.In(loc)) + fmt.Sprintf("traces_%s-%03d.json.gz", m.Format("1504"), j)
			p.Put(k, gz(request(m.Add(-20*time.Second+time.Duration(j)*time.Millisecond))))
		}
	}
}

type dirPutter struct {
	t *testing.T
	d objstore.Dir
}

func (p dirPutter) Put(key string, data []byte) {
	if err := p.d.Put(key, data); err != nil {
		p.t.Fatal(err)
	}
}

// checker is a WindowChecker that reads the span starts itself, and records what it was asked.
type checker struct {
	mu    sync.Mutex
	calls [][]string
}

var allStarts = regexp.MustCompile(`"startTimeUnixNano":"([0-9]+)"`)

func (c *checker) LateSpans(ctx context.Context, dir string, start, end time.Time) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var names []string
	n := 0
	for _, e := range ents {
		names = append(names, e.Name())
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return 0, err
		}
		if strings.HasSuffix(e.Name(), ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return 0, err
			}
			if b, err = io.ReadAll(zr); err != nil {
				return 0, err
			}
		}
		for _, m := range allStarts.FindAllSubmatch(b, -1) {
			ns, _ := strconv.ParseInt(string(m[1]), 10, 64)
			if s := time.Unix(0, ns); !s.Before(start) && s.Before(end) {
				n++
			}
		}
	}
	c.mu.Lock()
	c.calls = append(c.calls, names)
	c.mu.Unlock()
	return n, nil
}

func cfgFor(st objstore.Store) EnvConfig { return EnvConfig{Store: st, Prefix: prefix} }

func refusal(t *testing.T, err error, code string) *RefusalError {
	t.Helper()
	var r *RefusalError
	if !errors.As(err, &r) {
		t.Fatalf("got %v, want the refusal %s", err, code)
	}
	if r.Refusal.Code != code || r.Finding.Code != code || r.Finding.Level != "refuse" {
		t.Fatalf("refusal %+v, want %s", r.Refusal, code)
	}
	if r.Refusal.Reason == "" || r.Refusal.Fix == "" || len(r.Refusal.Fix) > 300 || len(r.Refusal.Reason) > 600 {
		t.Fatalf("refusal %+v: reason and fix are required and bounded", r.Refusal)
	}
	for _, e := range r.Finding.Examples {
		if !nameRE.MatchString(e) {
			t.Fatalf("example %q is not a Name", e)
		}
	}
	return r
}

// emptyRoot checks that a failed fetch left nothing behind.
func emptyRoot(t *testing.T, root string) {
	t.Helper()
	ents, _ := os.ReadDir(root)
	if len(ents) != 0 {
		t.Fatalf("the work root holds %d entries after a refused fetch", len(ents))
	}
}

type manifestView struct {
	Schema string `json:"schema"`
	Source struct {
		Kind, Env    string
		PrefixDigest string `json:"prefix_digest"`
	} `json:"source"`
	Window   struct{ Start, End, Cut string } `json:"window"`
	MarginsS struct {
		Before, After    float64
		AfterDerivedFrom string `json:"after_derived_from"`
	} `json:"margins_s"`
	PartitionTimezone struct {
		Configured     string
		DerivedOffsetH int `json:"derived_offset_h"`
	} `json:"partition_timezone"`
	Objects []struct {
		Name, ETag, SHA256 string
		Size               int64
	} `json:"objects"`
	Sampling struct{ Evidence string } `json:"sampling"`
}

func readManifest(t *testing.T, res *Result) (manifestView, []byte) {
	t.Helper()
	b, err := os.ReadFile(res.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var m manifestView
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(res.ManifestPath) != res.Dir {
		t.Fatalf("manifest %s outside the capture folder %s", res.ManifestPath, res.Dir)
	}
	// every listed file is in the folder with its sha256, and nothing else is
	ents, _ := os.ReadDir(res.Dir)
	if len(ents) != len(m.Objects)+1 {
		t.Fatalf("folder holds %d entries for %d objects and the manifest", len(ents), len(m.Objects))
	}
	for _, o := range m.Objects {
		data, err := os.ReadFile(filepath.Join(res.Dir, o.Name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != o.SHA256 || int64(len(data)) != o.Size {
			t.Fatalf("%s: sha256 or size differs from the manifest", o.Name)
		}
	}
	return m, b
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file:\n%s", name, got)
	}
}

func TestFetchCleanWindow(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-5*time.Minute), t1.Add(15*time.Minute), 2, time.UTC)
	wc := &checker{}
	root := t.TempDir()
	res, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, wc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("findings on a clean window: %s", res.Findings)
	}
	man, raw := readManifest(t, res)
	if len(man.Objects) != 19*2 {
		t.Fatalf("%d objects, want 38 (minutes 11:58 to 12:16, two each)", len(man.Objects))
	}
	if first := man.Objects[0].Name; first != "year=2026_month=10_day=26_hour=11_minute=58_traces_1158-000.json.gz" {
		t.Fatalf("first file name %q", first)
	}
	sum := sha256.Sum256([]byte(prefix))
	if man.Schema != "rhobound.capture-manifest/v1" || man.Source.Kind != "s3" || man.Source.Env != "prod" ||
		man.Source.PrefixDigest != "sha256:"+hex.EncodeToString(sum[:]) || man.Window.Cut != "none" ||
		man.Window.Start != "2026-10-26T12:00:00Z" || man.MarginsS.Before != 120 || man.MarginsS.After != 420 ||
		man.PartitionTimezone.Configured != "UTC" || man.PartitionTimezone.DerivedOffsetH != 0 ||
		man.Sampling.Evidence != "assumed" {
		t.Fatalf("manifest %s", raw)
	}
	if strings.Contains(string(raw), "env=prod") {
		t.Fatal("the manifest names the prefix; only its digest may appear")
	}
	if len(wc.calls) != 1 || len(wc.calls[0]) != 2 || !strings.Contains(wc.calls[0][0], "minute=16") {
		t.Fatalf("checker calls %v, want one call with the two objects of the last margin minute", wc.calls)
	}
	if s := m.Stats(); s.Lists != 19 || s.Heads != 38 || s.Opens != 38 {
		t.Fatalf("store calls %+v", s)
	}
	golden(t, "manifest.golden.json", raw)
}

func TestFetchFromADirStore(t *testing.T) {
	d := objstore.Dir{Root: t.TempDir()}
	fill(dirPutter{t, d}, t0.Add(-3*time.Minute), t1.Add(8*time.Minute), 1, time.UTC)
	res, err := Window{}.Fetch(context.Background(), cfgFor(d), req, t.TempDir(), &checker{})
	if err != nil {
		t.Fatal(err)
	}
	man, _ := readManifest(t, res)
	if man.Source.Kind != "dir" || len(man.Objects) != 19 || man.Objects[0].ETag != "" {
		t.Fatalf("dir manifest: kind %q, %d objects", man.Source.Kind, len(man.Objects))
	}
}

func TestFetchRefusesAnOversizeListingBeforeAnyRead(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 2, time.UTC)
	cfg := cfgFor(m)
	cfg.MaxListedBytes = 1000
	root := t.TempDir()
	_, err := Window{}.Fetch(context.Background(), cfg, req, root, &checker{})
	r := refusal(t, err, "store.too_large")
	if r.Refusal.Numbers["cap_bytes"] != 1000 || r.Refusal.Numbers["listed_bytes"] <= 1000 {
		t.Fatalf("numbers %v", r.Refusal.Numbers)
	}
	if s := m.Stats(); s.Heads != 0 || s.Opens != 0 {
		t.Fatalf("store calls %+v: nothing may be read before the size check", s)
	}
	emptyRoot(t, root)
	recordRefusal(t, r)
}

func TestFetchRefusesUnreadableObjectsNamingEach(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 1, time.UTC)
	p := prefix + objstore.Partition(t0.Add(3*time.Minute))
	m.Put(p+"traces_a.json.zst", []byte{0x28, 0xb5, 0x2f, 0xfd})
	m.Put(p+"traces_b.json.zst", []byte{0x28, 0xb5, 0x2f, 0xfd})
	m.Put(p+"traces_c.binpb", []byte{0x0a})
	root := t.TempDir()
	_, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, &checker{})
	r := refusal(t, err, "store.unreadable_object")
	want := []string{"2026-10-26T12:03/traces_a.json.zst", "2026-10-26T12:03/traces_b.json.zst", "2026-10-26T12:03/traces_c.binpb"}
	if fmt.Sprint(r.Finding.Examples) != fmt.Sprint(want) || r.Refusal.Numbers["objects"] != 3 {
		t.Fatalf("examples %v numbers %v, want %v", r.Finding.Examples, r.Refusal.Numbers, want)
	}
	if s := m.Stats(); s.Heads != 0 || s.Opens != 0 {
		t.Fatalf("store calls %+v", s)
	}
	emptyRoot(t, root)
	recordRefusal(t, r)
}

func TestFetchRefusesGzipNamesThatAreNotGzip(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 1, time.UTC)
	p := prefix + objstore.Partition(t0.Add(4*time.Minute))
	m.Put(p+"traces_plain.json.gz", request(t0.Add(4*time.Minute)))
	m.Put(p+"traces_empty.json.gz", nil)
	m.Put(p+"traces_zipped.json", gz(request(t0.Add(4*time.Minute))))
	root := t.TempDir()
	_, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, &checker{})
	r := refusal(t, err, "store.compression_mismatch")
	if r.Refusal.Numbers["objects"] != 3 || len(r.Finding.Examples) != 3 {
		t.Fatalf("refusal %+v %v", r.Refusal, r.Finding.Examples)
	}
	if s := m.Stats(); s.Opens != 0 {
		t.Fatalf("an object was downloaded before the sniff refused: %+v", s)
	}
	emptyRoot(t, root)
	recordRefusal(t, r)
}

func TestFetchRefusesLocalTimePartitions(t *testing.T) {
	east, west := time.FixedZone("UTC+2", 2*3600), time.FixedZone("UTC-5", -5*3600)
	for _, c := range []struct {
		name     string
		loc      *time.Location
		from, to time.Time
		want     float64
	}{
		// only the window's traffic, under labels two hours ahead: the window's partitions are empty
		{"empty window, UTC+2", east, t0.Add(-2 * time.Minute), t1.Add(7 * time.Minute), 2},
		{"empty window, UTC-5", west, t0.Add(-2 * time.Minute), t1.Add(7 * time.Minute), -5},
		// hours of traffic: the window's partitions hold spans two hours older
		{"older spans in the window, UTC+2", east, t0.Add(-150 * time.Minute), t1.Add(10 * time.Minute), 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := objstore.NewMem()
			fill(m, c.from, c.to, 1, c.loc)
			root := t.TempDir()
			_, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, &checker{})
			r := refusal(t, err, "store.partition_tz_offset")
			if r.Refusal.Numbers["derived_offset_h"] != c.want {
				t.Fatalf("derived offset %v h, want %v", r.Refusal.Numbers["derived_offset_h"], c.want)
			}
			if s := m.Stats(); s.Opens != 0 {
				t.Fatalf("store calls %+v", s)
			}
			emptyRoot(t, root)
			if c.want == 2 && c.from.Equal(t0.Add(-150*time.Minute)) {
				recordRefusal(t, r)
			}
		})
	}
}

func TestFetchRefusesAnEmptyWindow(t *testing.T) {
	m := objstore.NewMem()
	// traffic three hours before, in UTC: the store is right, the window has nothing
	fill(m, t0.Add(-3*time.Hour), t0.Add(-170*time.Minute), 1, time.UTC)
	root := t.TempDir()
	_, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, &checker{})
	r := refusal(t, err, "trace.too_few_entries")
	if r.Refusal.Numbers["objects"] != 0 {
		t.Fatalf("numbers %v", r.Refusal.Numbers)
	}
	emptyRoot(t, root)
	_, err = Window{}.Fetch(context.Background(), cfgFor(objstore.NewMem()), req, root, &checker{})
	refusal(t, err, "trace.too_few_entries")
}

func TestFetchFindsALateObjectByExtendingOnce(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(14*time.Minute), 1, time.UTC)
	// the last margin minute (12:16) holds a span of the window: exports run later than the margin
	m.Put(prefix+objstore.Partition(t1.Add(6*time.Minute))+"traces_late1.json.gz", gz(request(t1.Add(-10*time.Second))))
	// an object of the window uploaded at 12:20, which the first listing (19 minute partitions) cannot see
	lateKey := prefix + objstore.Partition(t1.Add(10*time.Minute)) + "traces_late2.json.gz"
	m.PutLate(lateKey, gz(request(t1.Add(-5*time.Second))), 19)
	wc := &checker{}
	res, err := Window{}.Fetch(context.Background(), cfgFor(m), req, t.TempDir(), wc)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("findings %s: the extension's last minute is clean", res.Findings)
	}
	man, _ := readManifest(t, res)
	found := false
	for _, o := range man.Objects {
		found = found || strings.HasSuffix(o.Name, "minute=20_traces_late2.json.gz")
	}
	if !found || man.MarginsS.After != 840 || !strings.Contains(man.MarginsS.AfterDerivedFrom, "extended once") {
		t.Fatalf("late object found %v, margin after %v (%s)", found, man.MarginsS.After, man.MarginsS.AfterDerivedFrom)
	}
	if len(wc.calls) != 2 || !strings.Contains(wc.calls[1][0], "minute=23") {
		t.Fatalf("checker calls %v, want the last minute of the listing, then of the extension", wc.calls)
	}
	if s := m.Stats(); s.Lists != 19+7 {
		t.Fatalf("%d listings, want 19 and one extension of 7", s.Lists)
	}
}

func TestFetchDoubtsLateBatchesAfterTheExtension(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(14*time.Minute), 1, time.UTC)
	m.Put(prefix+objstore.Partition(t1.Add(6*time.Minute))+"traces_late1.json.gz", gz(request(t1.Add(-10*time.Second))))
	m.Put(prefix+objstore.Partition(t1.Add(13*time.Minute))+"traces_late3.json.gz",
		gz(request(t1.Add(-2*time.Second), t1.Add(-time.Second))))
	res, err := Window{}.Fetch(context.Background(), cfgFor(m), req, t.TempDir(), &checker{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings %s, want store.late_batches", res.Findings)
	}
	var f Finding
	if err := json.Unmarshal(res.Findings[0], &f); err != nil {
		t.Fatal(err)
	}
	if f.Code != "store.late_batches" || f.Level != "doubt" || f.Numbers["late_spans"] != 2 || f.Numbers["margin_after_s"] != 840 {
		t.Fatalf("finding %+v", f)
	}
	recordFinding(t, res.Findings[0])
}

func TestFetchNotesSmallBatchesWithTheirCost(t *testing.T) {
	m := objstore.NewMem()
	// one Collector exporting every 200 ms: 300 objects a minute
	fill(m, t0.Add(-2*time.Minute), t0.Add(3*time.Minute), 300, time.UTC)
	cfg := cfgFor(m)
	cfg.Retry, cfg.Batch, cfg.SDKDelay = 60*time.Second, 200*time.Millisecond, 5*time.Second
	// 12:00 to 12:01, listed 11:58 to 12:02 (end + 65.2 s, to the minute)
	short := Request{Env: "prod", Start: t0, End: t0.Add(time.Minute)}
	res, err := Window{Parallel: 4}.Fetch(context.Background(), cfg, short, t.TempDir(), &checker{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings %s", res.Findings)
	}
	var f Finding
	json.Unmarshal(res.Findings[0], &f)
	if f.Code != "store.small_batches" || f.Level != "note" || f.Numbers["objects_per_minute"] != 300 ||
		f.Numbers["put_requests_per_month"] != 12_960_000 || f.Numbers["put_cost_usd_per_month"] != 64.8 {
		t.Fatalf("finding %+v", f)
	}
	man, _ := readManifest(t, res)
	if man.MarginsS.After != 65.2 || man.MarginsS.AfterDerivedFrom != "retry 60 s + batch 0.2 s + sdk 5 s" {
		t.Fatalf("margins %+v", man.MarginsS)
	}
	recordFinding(t, res.Findings[0])
}

func TestFlattenedNamesNeverCollide(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 1, time.UTC)
	p := prefix + objstore.Partition(t0.Add(5*time.Minute))
	keys := []string{p + "a/b_c.json.gz", p + "a_b/c.json.gz", p + "a/b.c.json.gz", p + "a/b-c.json.gz"}
	for _, k := range keys {
		m.Put(k, gz(request(t0.Add(5*time.Minute))))
	}
	res, err := Window{}.Fetch(context.Background(), cfgFor(m), req, t.TempDir(), &checker{})
	if err != nil {
		t.Fatal(err)
	}
	man, _ := readManifest(t, res)
	seen := map[string]bool{}
	for _, o := range man.Objects {
		if seen[o.Name] || o.Name == ManifestName || strings.Count(o.Name, ".json") != 1 {
			t.Fatalf("file name %q repeats, or carries a second suffix", o.Name)
		}
		seen[o.Name] = true
	}
	if len(man.Objects) != 19+len(keys) {
		t.Fatalf("%d objects", len(man.Objects))
	}
	// an object whose name would be the manifest's
	taken := map[string]bool{ManifestName: true}
	objs := []*listed{{rel: "manifest.json", sfx: ".json"}}
	if err := assignNames(objs, taken); err != nil || objs[0].name == ManifestName {
		t.Fatalf("name %q, %v", objs[0].name, err)
	}
}

func TestFetchTakesARepeatedKeyOnce(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 1, time.UTC)
	m.RepeatInListing(prefix + objstore.Partition(t0) + "traces_1200-000.json.gz")
	res, err := Window{}.Fetch(context.Background(), cfgFor(m), req, t.TempDir(), &checker{})
	if err != nil {
		t.Fatal(err)
	}
	if man, _ := readManifest(t, res); len(man.Objects) != 19 {
		t.Fatalf("%d objects, want 19", len(man.Objects))
	}
	if s := m.Stats(); s.Opens != 19 {
		t.Fatalf("%d downloads, want 19", s.Opens)
	}
}

func TestASecondFetchNeverSeesTheFirstsFiles(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(30*time.Minute), 1, time.UTC)
	root := t.TempDir()
	a, err := Window{}.Fetch(context.Background(), cfgFor(m), req, root, &checker{})
	if err != nil {
		t.Fatal(err)
	}
	later := Request{Env: "prod", Start: t0.Add(15 * time.Minute), End: t1.Add(15 * time.Minute)}
	b, err := Window{}.Fetch(context.Background(), cfgFor(m), later, root, &checker{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Dir == b.Dir || filepath.Dir(a.Dir) != root || filepath.Dir(b.Dir) != root {
		t.Fatalf("folders %s and %s", a.Dir, b.Dir)
	}
	man, _ := readManifest(t, b) // checks that the folder holds exactly the manifest's files
	for _, o := range man.Objects {
		if strings.Contains(o.Name, "minute=05_") {
			t.Fatalf("the second window holds %s, an object of the first", o.Name)
		}
	}
	if st, err := os.Stat(b.Dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("capture folder mode %v, %v: want 0700", st.Mode().Perm(), err)
	}
}

// sizes is a store whose listing reports sizes one byte short: an object that changed after it was listed.
type sizes struct{ *objstore.Mem }

func (s sizes) List(ctx context.Context, prefix string) ([]objstore.Object, error) {
	got, err := s.Mem.List(ctx, prefix)
	for i := range got {
		got[i].Size--
	}
	return got, err
}

func TestFetchRefusesAnObjectThatChangedWhileFetched(t *testing.T) {
	m := objstore.NewMem()
	fill(m, t0.Add(-2*time.Minute), t1.Add(7*time.Minute), 1, time.UTC)
	root := t.TempDir()
	_, err := Window{}.Fetch(context.Background(), cfgFor(sizes{m}), req, root, &checker{})
	r := refusal(t, err, "store.manifest_mismatch")
	if r.Refusal.Numbers["read_bytes"] != r.Refusal.Numbers["listed_bytes"]+1 || r.Refusal.Numbers["objects"] != 19 ||
		r.Finding.Examples[0] != "2026-10-26T11:58/traces_1158-000.json.gz" {
		t.Fatalf("numbers %v", r.Refusal.Numbers)
	}
	emptyRoot(t, root)
	recordRefusal(t, r)
}

func TestCountCheckRefusesAStrayFile(t *testing.T) {
	dir := t.TempDir()
	objs := []*listed{{name: "a.json.gz"}, {name: "b.json.gz"}}
	for _, o := range objs {
		os.WriteFile(filepath.Join(dir, o.name), nil, 0o600)
	}
	if err := countFiles(dir, objs, "prod"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "c.json.gz"), nil, 0o600)
	r := refusal(t, countFiles(dir, objs, "prod"), "store.manifest_mismatch")
	if r.Refusal.Numbers["other_entries"] != 1 {
		t.Fatalf("numbers %v", r.Refusal.Numbers)
	}
	os.Remove(filepath.Join(dir, "c.json.gz"))
	os.Remove(filepath.Join(dir, "b.json.gz"))
	refusal(t, countFiles(dir, objs, "prod"), "store.manifest_mismatch")
}

func TestFetchRejectsBadRequests(t *testing.T) {
	m := objstore.NewMem()
	ctx, root := context.Background(), t.TempDir()
	for _, c := range []struct {
		cfg EnvConfig
		req Request
		wc  WindowChecker
	}{
		{EnvConfig{Store: m, Prefix: "env=prod"}, req, &checker{}},
		{cfgFor(m), Request{Env: "Prod Env", Start: t0, End: t1}, &checker{}},
		{cfgFor(m), Request{Env: "prod", Start: t1, End: t0}, &checker{}},
		{cfgFor(m), Request{Env: "prod", Start: t0, End: t0.Add(3 * time.Hour)}, &checker{}},
		{cfgFor(m), req, nil},
		{EnvConfig{Prefix: prefix}, req, &checker{}},
	} {
		if _, err := (Window{}).Fetch(ctx, c.cfg, c.req, root, c.wc); err == nil {
			t.Errorf("%+v accepted", c)
		} else if errors.As(err, new(*RefusalError)) {
			t.Errorf("%+v: a bad request is a tool error, not a data refusal: %v", c, err)
		}
	}
	if s := m.Stats(); s.Lists != 0 {
		t.Fatalf("a bad request listed the store")
	}
}

func TestExampleNames(t *testing.T) {
	for rel, want := range map[string]string{
		"year=2026/month=10/day=26/hour=12/minute=03/traces_a.json.zst": "2026-10-26T12:03/traces_a.json.zst",
		"traces/a.pb": "traces/a.pb",
		"year=2026/month=10/day=26/hour=12/minute=03/traces a.zst": "obj_",
		"x=1/y.parquet": "obj_",
	} {
		got := exampleName(rel)
		if got != want && !(want == "obj_" && strings.HasPrefix(got, "obj_") && len(got) == 12) {
			t.Errorf("exampleName(%q) = %q, want %q", rel, got, want)
		}
		if !nameRE.MatchString(got) {
			t.Errorf("%q is not a Name", got)
		}
	}
}

// The golden findings and refusals are the documents this package hands to the server; the schema tests
// validate them (findings against defs.v1.json Finding, refusals against defs.v1.json Refusal).
var (
	goldenMu       sync.Mutex
	goldenFindings = map[string]json.RawMessage{}
	goldenRefusals = map[string]json.RawMessage{}
)

func recordRefusal(t *testing.T, r *RefusalError) {
	f, _ := json.Marshal(r.Finding)
	goldenMu.Lock()
	defer goldenMu.Unlock()
	goldenRefusals[r.Refusal.Code] = r.RefusalJSON()
	goldenFindings[r.Refusal.Code] = f
}

func recordFinding(t *testing.T, f json.RawMessage) {
	var v Finding
	json.Unmarshal(f, &v)
	goldenMu.Lock()
	defer goldenMu.Unlock()
	goldenFindings[v.Code] = f
}

func sortedArray(m map[string]json.RawMessage) []byte {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	b.WriteString("[\n")
	for i, k := range keys {
		b.Write(m[k])
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]\n")
	return b.Bytes()
}

// recordedAsRegistered: every refusal and finding the fetcher built in this package's tests carries numbers its code
// lists in the registry, and store.too_large carries the registry's fix (the fetcher's caps are constants or
// configuration: no fix of its may tell the reader to raise one). It reads what the other tests recorded, so TestMain
// runs it after all of them, in whatever order they ran (go test -shuffle).
func recordedAsRegistered() []string {
	goldenMu.Lock()
	defer goldenMu.Unlock()
	var bad []string
	for _, docs := range []map[string]json.RawMessage{goldenFindings, goldenRefusals} {
		for code, raw := range docs {
			var d struct {
				Fix     string             `json:"fix"`
				Numbers map[string]float64 `json:"numbers"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", code, err))
				continue
			}
			reg, ok := registry.Lookup(code)
			if !ok {
				bad = append(bad, code+" is not registered")
				continue
			}
			for k := range d.Numbers {
				if !reg.HasNumber(k) {
					bad = append(bad, fmt.Sprintf("%s carries the number %s, which the registry does not list for it", code, k))
				}
			}
			if code == codeTooLarge && d.Fix != reg.Fix {
				bad = append(bad, fmt.Sprintf("store.too_large's fix %q is not the registry's %q", d.Fix, reg.Fix))
			}
		}
	}
	if len(goldenRefusals) > 0 && goldenRefusals[codeTooLarge] == nil {
		bad = append(bad, "no store.too_large refusal was recorded")
	}
	sort.Strings(bad)
	return bad
}

func TestMain(m *testing.M) {
	flag.Parse()
	code := m.Run()
	// with the whole package run, every recorded document is checked against the registry and compared with the golden
	// lists
	if code == 0 && flag.Lookup("test.run").Value.String() == "" {
		for _, b := range recordedAsRegistered() {
			fmt.Fprintln(os.Stderr, "recorded documents:", b)
			code = 1
		}
		for name, docs := range map[string]map[string]json.RawMessage{
			"findings.golden.json": goldenFindings, "refusals.golden.json": goldenRefusals} {
			got := sortedArray(docs)
			path := filepath.Join("testdata", name)
			if *update {
				os.WriteFile(path, got, 0o644)
			}
			if want, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				fmt.Fprintf(os.Stderr, "%s differs from the recorded documents (run with -update):\n%s", name, got)
				code = 1
			}
		}
	}
	os.Exit(code)
}
