// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// wideWindow covers every capture of the repository.
const wideWindow = "2000-01-01T00:00:00Z,2100-01-01T00:00:00Z"

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCheckExitStatuses(t *testing.T) {
	root := repoRoot(t)
	out, errw, code := exec(t, "check", fixture)
	if code != 0 || !strings.HasPrefix(out, "check: admitted-with-doubts (rhobound.contract/v1.0)\n") ||
		!strings.Contains(out, "doubt  trace.sampling_unverified") || errw != "" {
		t.Errorf("a clean capture: exit %d\n%s%s", code, out, errw)
	}
	// a data refusal is 4, never 1 (the gate's block) or 2 (a tool error)
	out, _, code = exec(t, "check", filepath.Join(root, "internal/infer/testdata/nested/fraud-detection-consumer-in-consumer"))
	if code != 4 || !strings.Contains(out, "refuse trace.no_server_spans") {
		t.Errorf("a refused capture: exit %d\n%s", code, out)
	}
	for _, args := range [][]string{
		{"check"},
		{"check", filepath.Join(t.TempDir(), "missing")},
		{"check", t.TempDir()},
		{"check", fixture, "--kept-share", "2"},
		{"check", fixture, "--pod-identity", "process"},
		{"check", fixture, "--sampler", "a"},
		{"check", fixture, "--cut", "entry-start"},
		{"check", fixture, "--window", "yesterday,today", "--cut", "entry-start", "--out", t.TempDir()},
		{"check", fixture, "--window", wideWindow, "--cut", "span-end", "--out", t.TempDir()},
		{"check", fixture, "--manifest", "m.json"},
		{"check", "--manifest", filepath.Join(t.TempDir(), "m.json")},
		{"check", fixture, "--request-count", filepath.Join(t.TempDir(), "count.json")},
		{"check", fixture, "--no-such-flag"},
	} {
		if _, errw, code := exec(t, args...); code != 2 || !strings.HasPrefix(errw, "rhobound: ") {
			t.Errorf("%v: exit %d, %q (a usage or file system error is 2)", args, code, errw)
		}
	}
}

// --json prints the check report as canonical JSON (rhobound.check/v1).
func TestCheckJSON(t *testing.T) {
	out, _, code := exec(t, "check", fixture, "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	canon, err := contract.CanonicalizeJSON([]byte(out))
	if err != nil || string(canon)+"\n" != out {
		t.Fatalf("not canonical JSON: %v\n%s", err, out)
	}
	var r contract.CheckReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Schema != "rhobound.check/v1" || r.Verdict != "admitted-with-doubts" || r.Counts[contract.TraceImageDigestMissing] != 3 ||
		!strings.HasPrefix(r.Manifest, "sha256:") || r.KeptShare.Source != "assumed" {
		t.Errorf("%+v", r)
	}
	// the same capture, the same report
	if again, _, _ := exec(t, "check", fixture, "--json"); again != out {
		t.Error("two checks of one capture differ")
	}
}

// --write-manifest writes the capture's manifest; checking that manifest gives the same report; a request count next to
// it is read, and makes the capture's kept share counted.
func TestCheckManifestAndCount(t *testing.T) {
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join(fixture, "traces", "spans.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	first, errw, code := exec(t, "check", dir, "--write-manifest", "--json")
	if code != 0 || !strings.Contains(errw, "manifest.json") {
		t.Fatalf("exit %d %s", code, errw)
	}
	m, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := contract.ParseManifest(m); err != nil {
		t.Fatalf("the written manifest: %v", err)
	}
	second, _, code := exec(t, "check", "--manifest", filepath.Join(dir, "manifest.json"), "--json")
	if code != 0 || second != first {
		t.Errorf("checking the written manifest: exit %d\n%s\n%s", code, first, second)
	}
	base, err := testgen.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request-count.json"), testgen.RequestCount(base, 3099), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, code := exec(t, "check", dir, "--json")
	var r contract.CheckReport
	if code != 0 || json.Unmarshal([]byte(out), &r) != nil || r.Has(contract.TraceSamplingUnverified) ||
		r.KeptShare != (contract.KeptShare{Value: 1, Source: contract.KeptCounted}) {
		t.Errorf("with a request count: exit %d %s", code, out)
	}
	// a changed file against its manifest
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), b[:len(b)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	if out, _, code := exec(t, "check", "--manifest", filepath.Join(dir, "manifest.json")); code != 4 ||
		!strings.Contains(out, contract.StoreManifestMismatch) {
		t.Errorf("a changed file: exit %d\n%s", code, out)
	}
}

// goldenCaptures are the captures of the golden sweep (testdata/golden-1a2df09), with the change of their patch case.
func goldenCaptures(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "testdata", "golden-1a2df09", "arm64", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m goldenManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, c := range m.Cases {
		out[c.Capture] += ""
		for i, a := range c.Args {
			if a == "--patch" {
				out[c.Capture] = c.Args[i+1]
			}
		}
	}
	return out
}

// Cut neutrality (DATA.md 1 rule 8, and 17): for every capture of the repository, check --cut over a window that
// covers the whole capture, then predict on the canonical form, prints what predict prints on the raw capture, in the
// three forms of the golden sweep, apart from the capture's path. The one capture check refuses (no SERVER spans)
// cannot be cut, and predict refuses it too.
func TestCutNeutrality(t *testing.T) {
	root := repoRoot(t)
	caps := goldenCaptures(t)
	names := make([]string, 0, len(caps))
	for c := range caps {
		names = append(names, c)
	}
	sort.Strings(names)
	if len(names) != 8 {
		t.Fatalf("%d captures in the golden sweep", len(names))
	}
	for _, rel := range names {
		t.Run(rel, func(t *testing.T) {
			raw := filepath.Join(root, filepath.FromSlash(rel))
			out := filepath.Join(t.TempDir(), "canonical")
			_, errw, code := exec(t, "check", raw, "--window", wideWindow, "--cut", "entry-start", "--out", out)
			if strings.HasSuffix(rel, "fraud-detection-consumer-in-consumer") {
				if _, perr, pcode := exec(t, "predict", raw); code != 4 || pcode != 1 || !strings.Contains(perr, "no SERVER spans") {
					t.Errorf("check exit %d, predict exit %d", code, pcode)
				}
				return
			}
			if code != 0 {
				t.Fatalf("check --cut: exit %d %s", code, errw)
			}
			forms := [][]string{{}, {"--json"}, {"--patch", caps[rel], "--json"}}
			for _, form := range forms {
				wo, we, wc := exec(t, append([]string{"predict", raw}, form...)...)
				go_, ge, gc := exec(t, append([]string{"predict", out}, form...)...)
				norm := func(s, path string) string { return strings.ReplaceAll(s, path, "CAPTURE") }
				if wc != gc || norm(wo, raw) != norm(go_, out) || norm(we, raw) != norm(ge, out) {
					t.Errorf("predict %v: the canonical form gives exit %d, the raw capture %d; outputs differ: %v, %v", form, gc, wc,
						norm(wo, raw) != norm(go_, out), norm(we, raw) != norm(ge, out))
				}
			}
			// the canonical form checks as the capture did, with its own manifest
			first, _, _ := exec(t, "check", raw, "--json")
			again, _, code := exec(t, "check", "--manifest", filepath.Join(out, "manifest.json"), "--json")
			var a, b contract.CheckReport
			if code != 0 || json.Unmarshal([]byte(first), &a) != nil || json.Unmarshal([]byte(again), &b) != nil ||
				a.Verdict != b.Verdict || len(a.Findings) != len(b.Findings) {
				t.Errorf("re-check of the canonical form: exit %d\n%s\n%s", code, first, again)
			}
		})
	}
}

var (
	baseOnce sync.Once
	baseCap  *testgen.Capture
	baseErr  error
)

func baseCapture(t *testing.T) *testgen.Capture {
	baseOnce.Do(func() { baseCap, baseErr = testgen.Base() })
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	return baseCap.Clone()
}

// The fixtures of the codes that the fit and the model raise (today's refusals and warnings, which the fit codes):
// each one's capture makes today's CLI refuse or warn as the code says.
func TestFitAndPredictFixtures(t *testing.T) {
	for _, f := range testgen.Fixtures() {
		if f.Stage != testgen.StageFit && f.Stage != testgen.StagePredict {
			continue
		}
		t.Run(f.Code, func(t *testing.T) {
			cs, err := f.Build(baseCapture(t), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"predict", cs.Dir, "--json"}
			if cs.Patch != "" {
				args = append(args, "--patch", cs.Patch)
			}
			var ws []string
			for s, n := range cs.Workers {
				ws = append(ws, s+"="+itoa(n))
			}
			if len(ws) > 0 {
				sort.Strings(ws)
				args = append(args, "--workers", strings.Join(ws, ","))
			}
			if cs.Plain {
				args = append(args, "--plain")
			}
			out, errw, code := exec(t, args...)
			if cs.Want == "" {
				var rep struct {
					MaxRho float64 `json:"max_utilization"`
				}
				if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !(rep.MaxRho > 0.58 && rep.MaxRho <= 0.85) {
					t.Errorf("%s: exit %d, utilization %v; want above the validated 0.58 and within the limit\n%s", f.Code, code, rep.MaxRho, errw)
				}
				return
			}
			var warnings []string
			if code == 0 {
				var rep struct {
					Warnings []string `json:"warnings"`
				}
				_ = json.Unmarshal([]byte(out), &rep)
				warnings = rep.Warnings
			}
			if !strings.Contains(errw+strings.Join(warnings, "\n"), cs.Want) {
				t.Errorf("%s (%s): exit %d; neither the refusal nor a warning says %q\n%s", f.Code, f.What, code, cs.Want, errw)
			}
		})
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// The scripted agent's capture (testgen.Loop: image digests and a request count added) predicts what capture-chain
// does: neither addition moves a number.
func TestLoopCaptureMovesNoNumber(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loop")
	c, count := testgen.Loop(baseCapture(t))
	if err := c.Write(dir, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request-count.json"), count, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, form := range [][]string{{"--json"}, {"--patch", "b=+1ms", "--json"}, {"--workers", "a=1,b=2,c=1", "--json"}} {
		want, _, _ := exec(t, append([]string{"predict", fixture}, form...)...)
		got, _, code := exec(t, append([]string{"predict", dir}, form...)...)
		if code != 0 || strings.ReplaceAll(got, dir, "X") != strings.ReplaceAll(want, fixture, "X") {
			t.Errorf("predict %v: exit %d, the loop capture's report differs", form, code)
		}
	}
	out, _, code := exec(t, "check", dir)
	if code != 0 || !strings.HasPrefix(out, "check: admitted (") {
		t.Errorf("check: exit %d\n%s", code, out)
	}
}

// A request count given with --cut counts the window cut (DATA.md 4.3: cut, manifest, checks): the capture as read is
// checked without it, the cut is checked with it, the count is copied into the cut's folder, and the cut's manifest is
// the one the check completed, so that fit --manifest reads the count, is admitted with the kept share counted, and
// records the cut's digest. The loop capture, with a count of the minute in its middle; the folder as fetched has no
// count of its own.
func TestCheckCutWithCount(t *testing.T) {
	c, _ := testgen.Loop(baseCapture(t))
	raw := filepath.Join(t.TempDir(), "raw")
	if err := c.Write(raw, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	start, end := time.Date(2023, 11, 14, 22, 13, 50, 0, time.UTC), time.Date(2023, 11, 14, 22, 14, 50, 0, time.UTC)
	var n int
	c.Visit(func(rs *testgen.ResourceSpans, s *testgen.Span) {
		if a, _ := s.Times(); rs.Service() == "a" && s.KindOf() == otlp.KindServer && a >= uint64(start.UnixNano()) &&
			a < uint64(end.UnixNano()) {
			n++
		}
	})
	countDoc := func(s, e time.Time) string {
		return writeTemp(t, t.TempDir(), "count.json", []byte(fmt.Sprintf(`{"schema":"rhobound.request-count/v1",`+
			`"window":{"start":%q,"end":%q},"source":"loadgen","entries":[{"entry":"a","count":%d}]}`,
			s.Format(time.RFC3339), e.Format(time.RFC3339), n)))
	}
	count := countDoc(start, end)
	window := start.Format(time.RFC3339) + "," + end.Format(time.RFC3339)
	report := func(out string) *contract.CheckReport {
		var r contract.CheckReport
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return &r
	}

	// the capture as fetched: admitted, its sampling unverified; the count, against its whole window, is refused there
	out, errw, code := exec(t, "check", raw, "--write-manifest", "--json")
	if r := report(out); code != 0 || !r.Has(contract.TraceSamplingUnverified) {
		t.Fatalf("raw: exit %d %s %s", code, out, errw)
	}
	if out, _, code := exec(t, "check", raw, "--request-count", count, "--json"); code != 4 || !report(out).Has(contract.CountWindowMismatch) {
		t.Errorf("the minute's count against the whole capture: exit %d %s", code, out)
	}

	// the cut, checked with the count of its window
	cut := filepath.Join(t.TempDir(), "cut")
	out, errw, code = exec(t, "check", "--manifest", filepath.Join(raw, "manifest.json"), "--window", window, "--cut", "entry-start",
		"--request-count", count, "--out", cut, "--json")
	r := report(out)
	if code != 0 || r.Has(contract.TraceSamplingUnverified) || r.KeptShare != (contract.KeptShare{Value: 1, Source: contract.KeptCounted}) {
		t.Fatalf("cut with its count: exit %d %s %s", code, out, errw)
	}
	cb, _ := os.ReadFile(count)
	if b, err := os.ReadFile(filepath.Join(cut, "request-count.json")); err != nil || !bytes.Equal(b, cb) {
		t.Errorf("the count is not copied into the cut: %v", err)
	}
	mb, _ := os.ReadFile(filepath.Join(cut, "manifest.json"))
	m, err := contract.ParseManifest(mb)
	if err != nil || m.Digest() != r.Manifest || m.Sampling.Evidence != contract.KeptCounted || m.Sampling.IndependentCount == nil ||
		*m.Sampling.IndependentCount != contract.Digest(cb) || m.Window.Cut != contract.CutEntrySpanStart {
		t.Errorf("the cut's manifest: %v %s; the report's digest %s", err, mb, r.Manifest)
	}
	// the fit of the cut reads the count, holds no sampling doubt, and records the cut's digest
	fdir := t.TempDir()
	if _, errw, code := exec(t, "fit", "--manifest", filepath.Join(cut, "manifest.json"), "--out", fdir); code != 0 {
		t.Fatalf("fit --manifest: exit %d %s", code, errw)
	}
	fb, _ := os.ReadFile(filepath.Join(fdir, fit.File))
	f, err := fit.Unmarshal(fb)
	if err != nil || f.ManifestDigest != r.Manifest || f.KeptShare != (contract.KeptShare{Value: 1, Source: contract.KeptCounted}) ||
		f.Options.RequestCount != contract.Digest(cb) {
		t.Fatalf("fit of the cut: %v %s", err, fb)
	}
	for _, x := range f.Findings {
		if x.Code == contract.TraceSamplingUnverified || x.Code == contract.FitQueueRebuildSampled {
			t.Errorf("fit of the cut: %s", x.Code)
		}
	}

	// a count of another window refuses the cut, which then leaves nothing behind, whether its folder existed or not
	other := countDoc(start.Add(-time.Minute), end.Add(-time.Minute))
	for _, existed := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "cut")
		if existed {
			os.Mkdir(dir, 0o755)
		}
		out, _, code := exec(t, "check", "--manifest", filepath.Join(raw, "manifest.json"), "--window", window, "--cut", "entry-start",
			"--request-count", other, "--out", dir, "--json")
		ents, err := os.ReadDir(dir)
		if code != 4 || !report(out).Has(contract.CountWindowMismatch) || len(ents) != 0 || existed == (err != nil) {
			t.Errorf("a count of another window, folder existed %v: exit %d, %d files left, %v\n%s", existed, code, len(ents), err, out)
		}
	}
}

// check --window --cut on a capture whose trace file reaches back past a rollout of b (DATA.md 3.9): the old image's
// spans all end before the window, so the window is cut, and the check and the fit of its cut hold b's new image only.
// The capture as read, a window that a span of the old image overlaps, and a window that ends before b's new image
// starts but keeps it in the traces that call it before then, are refused with trace.mixed_images. --write-manifest
// writes the manifest of the capture as read only when the capture as read is admitted.
func TestCheckWindowAfterRollout(t *testing.T) {
	c := baseCapture(t)
	at := uint64(time.Unix(1_700_000_030, 0).UnixNano())
	_, lastEnd := testgen.Rollout(c, "b", at)
	var first, firstNew uint64
	c.Visit(func(rs *testgen.ResourceSpans, s *testgen.Span) {
		start, _ := s.Times()
		if first == 0 || start < first {
			first = start
		}
		if rs.Service() == "b" && rs.Attr("k8s.pod.name") != "b-old" && (firstNew == 0 || start < firstNew) {
			firstNew = start
		}
	})
	raw := t.TempDir()
	if err := c.Write(raw, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	rfc := func(n uint64) string { return time.Unix(0, int64(n)).UTC().Format(time.RFC3339Nano) }
	window := func(from uint64) string {
		return rfc(from) + "," + time.Unix(0, int64(from)).UTC().Add(time.Minute).Format(time.RFC3339Nano)
	}
	if out, _, code := exec(t, "check", raw); code != 4 || !strings.Contains(out, "refuse trace.mixed_images b") {
		t.Errorf("the capture as read: exit %d\n%s", code, out)
	}
	for name, w := range map[string]string{
		"a window the old image's last span ends in":    window(lastEnd),
		"a window that ends where b's new image starts": rfc(first) + "," + rfc(firstNew),
	} {
		cut := filepath.Join(t.TempDir(), "cut")
		out, _, code := exec(t, "check", raw, "--window", w, "--cut", "entry-start", "--out", cut)
		if ents, _ := os.ReadDir(cut); code != 4 || !strings.Contains(out, "refuse trace.mixed_images b") || len(ents) != 0 {
			t.Errorf("%s: exit %d, %d files cut\n%s", name, code, len(ents), out)
		}
	}
	cut := filepath.Join(t.TempDir(), "cut")
	out, errw, code := exec(t, "check", raw, "--window", window(at), "--cut", "entry-start", "--out", cut)
	if code != 0 || strings.Contains(out, contract.TraceMixedImages) {
		t.Fatalf("a window after the rollout: exit %d\n%s%s", code, out, errw)
	}
	if out, errw, code := exec(t, "check", "--manifest", filepath.Join(cut, "manifest.json")); code != 0 ||
		strings.Contains(out, contract.TraceMixedImages) {
		t.Errorf("the check of the cut: exit %d\n%s%s", code, out, errw)
	}
	fdir := t.TempDir()
	if _, errw, code := exec(t, "fit", "--manifest", filepath.Join(cut, "manifest.json"), "--out", fdir); code != 0 {
		t.Fatalf("fit of the cut: exit %d %s", code, errw)
	}
	fb, _ := os.ReadFile(filepath.Join(fdir, fit.File))
	f, err := fit.Unmarshal(fb)
	if err != nil || len(f.Images["b"]) != 1 || !strings.Contains(f.Images["b"][0], testgen.Digest("b")) {
		t.Fatalf("fit of the cut: %v, b's images %v", err, f.Images["b"])
	}
	for _, x := range f.Findings {
		if x.Code == contract.TraceMixedImages {
			t.Errorf("fit of the cut: %s", x.Code)
		}
	}
	// --write-manifest: the capture as read is refused, so its manifest is not written; the window is still cut
	again := filepath.Join(t.TempDir(), "cut")
	out, errw, code = exec(t, "check", raw, "--write-manifest", "--window", window(at), "--cut", "entry-start", "--out", again)
	if _, err := os.Stat(filepath.Join(raw, "manifest.json")); code != 0 || err == nil || strings.Contains(out, contract.TraceMixedImages) ||
		!strings.Contains(errw, "did not write "+filepath.Join(raw, "manifest.json")) || !strings.Contains(errw, "wrote the canonical capture to "+again) {
		t.Errorf("--write-manifest, a window after the rollout: exit %d, the capture's manifest %v\n%s%s", code, err, out, errw)
	}
	b1, _ := os.ReadFile(filepath.Join(cut, "manifest.json"))
	if b2, err := os.ReadFile(filepath.Join(again, "manifest.json")); err != nil || !bytes.Equal(b1, b2) {
		t.Errorf("--write-manifest changes the cut:\n%s\n%s", b1, b2)
	}
	// a window outside the times a span can hold is a tool error, whatever the flags
	for _, args := range [][]string{{}, {"--write-manifest"}} {
		args = append([]string{"check", raw, "--window", "1969-12-31T23:59:59Z,2100-01-01T00:00:00Z", "--cut", "entry-start",
			"--out", filepath.Join(t.TempDir(), "cut")}, args...)
		if _, errw, code := exec(t, args...); code != 2 || !strings.Contains(errw, "the times a span can hold") {
			t.Errorf("%v: exit %d %s", args, code, errw)
		}
	}
	if _, err := os.Stat(filepath.Join(raw, "manifest.json")); err == nil {
		t.Error("a tool error wrote the capture's manifest")
	}
}
