// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// addOwnTime adds d ns of own time to every visit of svc before its first call (testgen.AddOwnTime).
func addOwnTime(c *testgen.Capture, svc string, d int64) {
	testgen.AddOwnTime(c, svc, func(int) int64 { return d })
}

// abArmDir writes one arm: the loop capture (image digests, request count) with edit applied.
func abArmDir(t *testing.T, name string, edit func(c *testgen.Capture)) (string, *testgen.Capture) {
	t.Helper()
	c, count := testgen.Loop(baseCapture(t))
	if edit != nil {
		edit(c)
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := c.Write(dir, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request-count.json"), count, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, c
}

// loadgenLog is the load generator's log of two arms: one row per entry span (a's SERVER spans), sent at its start and
// answered at its end plus a fixed 0.2 ms of network.
func loadgenLog(t *testing.T, a, b *testgen.Capture) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString(abdiff.LoadgenHeader + "\n")
	for k, c := range []*testgen.Capture{a, b} {
		arm := []string{"A", "B"}[k]
		seq := 0
		c.Visit(func(rs *testgen.ResourceSpans, s *testgen.Span) {
			if rs.Service() == "a" && s.KindOf() == otlp.KindServer {
				seq++
				st, en := s.Times()
				fmt.Fprintf(&sb, "%s,%d,%d,%d,200\n", arm, seq, st, en+200_000)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "loadgen.csv")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// diff-fit on generated A/B pairs of the base capture: +0.5 ms of own time per visit of b is measured as such and
// minted, then cited by a measured-ab item of a change set; a structural change, and spans that disagree with the load
// generator, are flagged; an arm whose traces hold half the requests the load generator sent is refused, and nothing is
// minted; a record the store cannot take is not left in DIR.
func TestDiffFit(t *testing.T) {
	inRepoRoot(t)
	aDir, aCap := abArmDir(t, "arm-a", nil)
	bDir, bCap := abArmDir(t, "arm-b", func(c *testgen.Capture) { addOwnTime(c, "b", 500_000) })
	out := t.TempDir()
	stdout, errw, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", bDir, "--loadgen-log", loadgenLog(t, aCap, bCap),
		"--out", out)
	if code != 0 {
		t.Fatalf("diff-fit: exit %d: %s", code, errw)
	}
	b, err := os.ReadFile(filepath.Join(out, abdiff.File))
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimSpace(stdout)
	rec, err := abdiff.Unmarshal(b)
	if err != nil || digest != contract.Digest(b) {
		t.Fatalf("record %v; printed %q, the file's digest is %s", err, digest, contract.Digest(b))
	}
	if _, ok := openRecords().AB(digest); !ok {
		t.Fatal("the record is not in the record store")
	}
	// the arms named: the record holds the names instead of the folders as given, and is otherwise the same
	if rec.Arms.Baseline.Capture != aDir || rec.Arms.Candidate.Capture != bDir {
		t.Errorf("arms %q, %q", rec.Arms.Baseline.Capture, rec.Arms.Candidate.Capture)
	}
	named := t.TempDir()
	if _, errw, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", bDir, "--loadgen-log", loadgenLog(t, aCap, bCap),
		"--out", named, "--baseline-name", "loop-a", "--candidate-name", "loop-b"); code != 0 {
		t.Fatalf("diff-fit with names: exit %d: %s", code, errw)
	}
	nb, _ := os.ReadFile(filepath.Join(named, abdiff.File))
	unnamed := strings.Replace(strings.Replace(string(nb), `"capture":"loop-a"`, `"capture":"`+aDir+`"`, 1),
		`"capture":"loop-b"`, `"capture":"`+bDir+`"`, 1)
	if nrec, err := abdiff.Unmarshal(nb); err != nil || nrec.Arms.Baseline.Capture != "loop-a" ||
		nrec.Arms.Candidate.Capture != "loop-b" || unnamed != string(b) {
		t.Errorf("named arms: %v\n%s", err, nb)
	}
	for _, bad := range [][]string{{"--baseline-name", "/srv/captures/a"}, {"--candidate-name", "b b"}, {"--candidate-name", ""},
		{"--baseline-name", "ext:arn:aws:lambda:us-east-1:" + strings.Repeat("1", 4) + strings.Repeat("2", 4) +
			strings.Repeat("3", 4) + ":function:pay"}, {"--candidate-name", "ext:x/arn:aws:y"}} {
		out, errw, code := exec(t, append([]string{"diff-fit", "--baseline", aDir, "--candidate", bDir, "--out", t.TempDir(), "--json"},
			bad...)...)
		if code != 1 || out != "" || !strings.HasPrefix(errw, "rhobound: "+bad[0]+": ") {
			t.Errorf("%v: exit %d, %q, %q", bad, code, out, errw)
		}
	}
	// the log found in the baseline's folder, as loadgen.csv, gives the same record
	lb, _ := os.ReadFile(loadgenLog(t, aCap, bCap))
	if err := os.WriteFile(filepath.Join(aDir, LoadgenFile), lb, 0o644); err != nil {
		t.Fatal(err)
	}
	again := t.TempDir()
	if out, errw, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", bDir, "--out", again); code != 0 ||
		strings.TrimSpace(out) != digest {
		t.Fatalf("diff-fit with %s in the baseline's folder: exit %d, %q: %s", LoadgenFile, code, out, errw)
	}
	os.Remove(filepath.Join(aDir, LoadgenFile))
	var own *abdiff.Service
	for k := range rec.Services {
		if rec.Services[k].Service == "b" {
			own = &rec.Services[k]
		}
	}
	if own == nil || math.Abs(own.OwnMs.Delta-0.5) > math.Max(3*own.OwnMs.SE, 0.02) || !(own.OwnMs.SE > 0) || own.CallsChanged ||
		own.VarianceChanged || !rec.SelfReportConsistent || rec.Arms.Baseline.Images["b"] == nil {
		t.Fatalf("b: %+v; findings %+v; consistent %v", own, rec.Findings, rec.SelfReportConsistent)
	}

	// the record's number, cited: the label is minted, the value widened by 2 standard errors
	f := fitOf(t, aDir, "--workers", "a=1,b=2,c=1")
	fb, _ := os.ReadFile(f)
	set := change.Set{Schema: change.Schema, Fit: contract.Digest(fb), Items: []change.Item{{Op: change.OpOwnMs, Service: "b",
		Value: change.Point(own.OwnMs.Delta), Label: change.LabelMeasuredAB, Evidence: []string{digest}}}}
	sj, _ := json.Marshal(set)
	cf := filepath.Join(t.TempDir(), "changes.json")
	os.WriteFile(cf, sj, 0o644)
	js, errw, code := exec(t, "predict", "--fit", f, "--changes", cf, "--json")
	var v report.V2
	if code != 0 || json.Unmarshal([]byte(js), &v) != nil {
		t.Fatalf("predict with the measured item: exit %d: %s", code, errw)
	}
	if len(v.RecordDigests) != 1 || v.RecordDigests[0] != digest || v.WeakestLabel != change.LabelMeasuredAB ||
		v.Items[0].Value.Hi-v.Items[0].Value.Lo != 4*own.OwnMs.SE {
		t.Errorf("record digests %v, weakest %q, value %+v", v.RecordDigests, v.WeakestLabel, v.Items[0].Value)
	}
	// the same number with another record's digest, or none, is not minted
	set.Items[0].Evidence = []string{"sha256:" + strings.Repeat("1", 64)}
	sj, _ = json.Marshal(set)
	os.WriteFile(cf, sj, 0o644)
	js, _, code = exec(t, "predict", "--fit", f, "--changes", cf, "--json")
	var doc report.RefusalDoc
	if code != 1 || json.Unmarshal([]byte(js), &doc) != nil || doc.Error.Code != contract.ChangeLabelNotMinted {
		t.Errorf("an unknown record: exit %d, %s", code, js)
	}

	// structure, self-report and sampling
	sDir, sCap := abArmDir(t, "arm-s", func(c *testgen.Capture) { testgen.BatchCalls(c, 2, 1) })
	half, _ := abArmDir(t, "arm-h", func(c *testgen.Capture) {
		c.Filter(func(_ *testgen.ResourceSpans, s *testgen.Span) bool { return testgen.TraceNumber(s.TraceID)%2 == 0 })
	})
	os.Remove(filepath.Join(half, "request-count.json"))
	// the spans say 5 ms more per request; the load generator, outside, saw nothing change
	slow, _ := abArmDir(t, "arm-slow", func(c *testgen.Capture) { addOwnTime(c, "b", 5_000_000) })
	for _, c := range []struct {
		name, cand, log string
		code            int
		want            string
	}{
		{"structure", sDir, loadgenLog(t, aCap, sCap), 0, contract.ProfileStructureChanged},
		{"self-report", slow, loadgenLog(t, aCap, aCap), 0, contract.ProfileSelfReportMismatch},
		{"sampled", half, loadgenLog(t, aCap, aCap), 1, contract.ABSampled},
	} {
		out := t.TempDir()
		js, errw, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", c.cand, "--loadgen-log", c.log, "--out", out, "--json")
		if code != c.code {
			t.Errorf("%s: exit %d: %s", c.name, code, errw)
			continue
		}
		if code == 1 {
			if json.Unmarshal([]byte(js), &doc) != nil || doc.Error.Code != c.want {
				t.Errorf("%s: %s", c.name, js)
			}
			if _, err := os.Stat(filepath.Join(out, abdiff.File)); err == nil {
				t.Errorf("%s: a refused run wrote a record", c.name)
			}
			continue
		}
		b, _ := os.ReadFile(filepath.Join(out, abdiff.File))
		r, err := abdiff.Unmarshal(b)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, f := range r.Findings {
			found = found || f.Code == c.want
		}
		if !found {
			t.Errorf("%s: findings %+v", c.name, r.Findings)
		}
	}
	// a refusal names an arm by its name, never by its folder
	js, errw, code = exec(t, "diff-fit", "--baseline", aDir, "--candidate", half, "--loadgen-log", loadgenLog(t, aCap, aCap),
		"--out", t.TempDir(), "--baseline-name", "loop-a", "--candidate-name", "loop-h", "--json")
	if code != 1 || json.Unmarshal([]byte(js), &doc) != nil || doc.Error.Code != contract.ABSampled || doc.Error.Subject != "loop-h" ||
		strings.Contains(js, half) {
		t.Errorf("a sampled arm, named: exit %d, %s %s", code, js, errw)
	}
	// a record the store cannot take is not minted, and not written either (last: the store is gone for this test)
	notDir := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(notDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RHOBOUND_STATE", notDir)
	unminted := t.TempDir()
	if out, _, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", bDir, "--loadgen-log", loadgenLog(t, aCap, bCap),
		"--out", unminted); code != 1 || out != "" {
		t.Errorf("diff-fit with no record store: exit %d, %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(unminted, abdiff.File)); err == nil {
		t.Errorf("diff-fit left %s that it did not mint", abdiff.File)
	}
}

// The A/B pairs of the variance test (testdata/mcp/loop, DATA.md 8): b's own-time variance changed with its mean kept is
// profile.variance_changed, with a delta within 2 standard errors of 0; a pure shift of b's own time is no variance
// change, and its delta is the shift within 2 standard errors, and above 0 by more than that. No other finding, and the
// other services do not move.
func TestDiffFitVarianceFixtures(t *testing.T) {
	inRepoRoot(t)
	for _, c := range []struct {
		dir      string
		shiftMs  float64
		variance bool
	}{
		{"testdata/mcp/loop/ab-variance", 0, true},
		{"testdata/mcp/loop/ab-shift", float64(testgen.ABShiftNs) / 1e6, false},
	} {
		out := t.TempDir()
		if _, errw, code := exec(t, "diff-fit", "--baseline", c.dir+"/baseline", "--candidate", c.dir+"/candidate", "--out", out); code != 0 {
			t.Fatalf("%s: exit %d: %s", c.dir, code, errw)
		}
		b, _ := os.ReadFile(filepath.Join(out, abdiff.File))
		rec, err := abdiff.Unmarshal(b)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if c.variance {
			want = contract.ProfileVarianceChanged + " b"
		}
		var got []string
		for _, f := range rec.Findings {
			got = append(got, f.Code+" "+f.Subject)
		}
		if strings.Join(got, ",") != want || !rec.SelfReportConsistent {
			t.Errorf("%s: findings %v, self-report consistent %v", c.dir, got, rec.SelfReportConsistent)
		}
		for _, s := range rec.Services {
			o := s.OwnMs
			switch {
			case s.Service == "b" && (s.VarianceChanged != c.variance || math.Abs(o.Delta-c.shiftMs) > 2*o.SE || !(o.SE > 0) ||
				s.CallsChanged || c.shiftMs > 0 && !(o.Delta > 2*o.SE)):
				t.Errorf("%s: b: own %+v, variance changed %v; want the shift %v ms", c.dir, o, s.VarianceChanged, c.shiftMs)
			case s.Service != "b" && (s.VarianceChanged || math.Abs(o.Delta) > 2*o.SE || s.CallsChanged):
				t.Errorf("%s: %s moved: own %+v, variance changed %v", c.dir, s.Service, o, s.VarianceChanged)
			}
		}
	}
}

// TestVersionOutsideTheRecordRule: a build stamped with a version a record may not hold (-X main.Version outside
// fit.VersionRule) would write fits and A/B records that every build refuses to read (fit.version), so fit and
// diff-fit refuse up front, stating the rule, and write nothing; a version of the rule fits, and the record reads.
func TestVersionOutsideTheRecordRule(t *testing.T) {
	inRepoRoot(t)
	defer func(v string) { Version = v }(Version)
	aDir, aCap := abArmDir(t, "arm-a", nil)
	bDir, bCap := abArmDir(t, "arm-b", nil)
	log := loadgenLog(t, aCap, bCap)
	for _, v := range []string{"ship:deadbeef", strings.Repeat("a", 65), "-x", "", "1 2"} {
		Version = v
		for _, args := range [][]string{
			{"fit", "testdata/capture-chain"},
			{"diff-fit", "--baseline", aDir, "--candidate", bDir, "--loadgen-log", log},
		} {
			out := t.TempDir()
			stdout, errw, code := exec(t, append(args, "--out", out, "--json")...)
			ents, _ := os.ReadDir(out)
			if code == 0 || len(ents) != 0 || !strings.Contains(errw, fit.VersionRule) {
				t.Errorf("version %q: %s: exit %d, %d files, %s %s", v, args[0], code, len(ents), stdout, errw)
			}
		}
	}
	for _, v := range []string{"build-x7", strings.Repeat("a", 64), "1.2.3+build~1_x"} {
		Version = v
		f := fitOf(t, "testdata/capture-chain")
		twinView(t, "context", "--fit", f)
	}
}
