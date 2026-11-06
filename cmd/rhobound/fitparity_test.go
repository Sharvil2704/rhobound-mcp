// SPDX-License-Identifier: Apache-2.0

package main

// Fit parity (docs/mcp/ARCHITECTURE.md 4.1): on one GOARCH, `rhobound predict CAPTURE --patch P --json`
// and `rhobound predict --fit F --patch P --report v1 --json` print the same bytes, where F is `rhobound fit CAPTURE`
// with the same options; and report v2, its v2 keys dropped (report.V2.V1), gives the v1 bytes. Both sides run here,
// on this machine, so the test holds on any GOARCH; the golden sweep pins the v1 side to 1a2df09.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// inRepoRoot runs the test from the repository root, where the captures' names are those the reports print. No test
// of this package runs in parallel.
func inRepoRoot(t *testing.T) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	t.Setenv("RHOBOUND_ESTIMATOR", "")
	t.Setenv("RHOBOUND_STATE", t.TempDir())
}

// fitOf runs `rhobound fit` and returns the record's path.
func fitOf(t *testing.T, capture string, opts ...string) string {
	t.Helper()
	dir := t.TempDir()
	out, errw, code := exec(t, append([]string{"fit", capture, "--out", dir}, opts...)...)
	if code != 0 {
		t.Fatalf("fit %s %v: exit %d: %s", capture, opts, code, errw)
	}
	p := filepath.Join(dir, fit.File)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != contract.Digest(b) || bytes.HasSuffix(b, []byte("\n")) {
		t.Fatalf("fit printed %q; the file's digest is %s", out, contract.Digest(b))
	}
	return p
}

func TestFitParity(t *testing.T) {
	inRepoRoot(t)
	// every entry span of capture-chain sampled at the threshold th:8, half the requests: the fit derives its kept
	// share from it (derived-th, 0.5), which predict CAPTURE is given as --kept-share (USAGE.md 11)
	th := baseCapture(t)
	th.Visit(func(rs *testgen.ResourceSpans, s *testgen.Span) {
		if rs.Service() == "a" && s.KindOf() == otlp.KindServer && s.ParentSpanID == "" {
			s.TraceState = "ot=th:8"
		}
	})
	thDir := filepath.Join(t.TempDir(), "th-chain")
	if err := th.Write(thDir, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		capture string
		patch   string
		opts    []string // options of both forms: fit takes them, predict CAPTURE takes them
		v1opts  []string // options of predict CAPTURE only: what the fit derives itself
	}{
		{"testdata/capture-chain", "b=+1ms", nil, nil},
		{"testdata/capture-chain", "b=+6ms", nil, nil},
		{"testdata/capture-chain", "", nil, nil},
		{"testdata/capture-chain", "b=3.5,c=-10%", []string{"--workers", "a=1,b=2,c=1"}, nil},
		{"testdata/capture-chain", "a=+2ms", []string{"--plain"}, nil},
		{"testdata/capture-chain", "b=+1ms", []string{"--kept-share", "0.5"}, nil},
		{"internal/infer/testdata/counts/shipped-20s", "cart=+1ms", nil, nil},
		{"internal/infer/testdata/hops/browse-45s", "frontend=+20%", nil, nil},
		{"internal/infer/testdata/hops/browse-45s", "frontend=+20%", []string{"--plain"}, nil},
		{"internal/infer/testdata/hops/cache-30s", "api=+2ms", nil, nil},
		{thDir, "b=+1ms", nil, []string{"--kept-share", "0.5"}},
	} {
		name := filepath.Base(c.capture) + "/" + c.patch + strings.Join(c.opts, "")
		t.Run(name, func(t *testing.T) {
			f := fitOf(t, c.capture, c.opts...)
			for _, asJSON := range []bool{true, false} {
				v1Args := append(append([]string{"predict", c.capture}, c.opts...), c.v1opts...)
				fitArgs := []string{"predict", "--fit", f, "--report", "v1"}
				if c.patch != "" {
					v1Args = append(v1Args, "--patch", c.patch)
					fitArgs = append(fitArgs, "--patch", c.patch)
				}
				if asJSON {
					v1Args, fitArgs = append(v1Args, "--json"), append(fitArgs, "--json")
				}
				want, werr, wcode := exec(t, v1Args...)
				got, gerr, gcode := exec(t, fitArgs...)
				if wcode != 0 || gcode != 0 {
					t.Fatalf("exit %d (%s) and %d (%s)", wcode, werr, gcode, gerr)
				}
				if got != want {
					t.Errorf("--json %v: predict --fit --report v1 differs from predict CAPTURE at %s", asJSON,
						firstDiff([]byte(got), []byte(want)))
				}
				if !asJSON {
					continue
				}
				// report v2, its v2 keys dropped, is report v1
				v2Args := []string{"predict", "--fit", f, "--json"}
				if c.patch != "" {
					v2Args = append(v2Args, "--patch", c.patch)
				}
				out, errw, code := exec(t, v2Args...)
				if code != 0 {
					t.Fatalf("predict --fit: exit %d: %s", code, errw)
				}
				var v2 report.V2
				dec := json.NewDecoder(strings.NewReader(out))
				dec.DisallowUnknownFields()
				if err := dec.Decode(&v2); err != nil {
					t.Fatal(err)
				}
				if v2.Version != report.VersionV2 || v2.Capture != c.capture {
					t.Errorf("v2: version %q, capture %q", v2.Version, v2.Capture)
				}
				if canon, err := contract.CanonicalizeJSON([]byte(out)); err != nil || string(canon)+"\n" != out {
					t.Error("report v2 is not canonical JSON and a newline")
				}
				var proj bytes.Buffer
				if err := v2.V1().WriteJSON(&proj); err != nil {
					t.Fatal(err)
				}
				if proj.String() != want {
					t.Errorf("the v1 projection of report v2 differs from report v1 at %s", firstDiff(proj.Bytes(), []byte(want)))
				}
			}
		})
	}
}

// A capture the inference refuses is refused by fit with the same code and message, and a fit of another schema
// version is refused by predict with fit.version.
func TestFitRefusals(t *testing.T) {
	inRepoRoot(t)
	capture := "internal/infer/testdata/nested/cart-client-in-client"
	_, want, _ := exec(t, "predict", capture)
	out, errw, code := exec(t, "fit", capture, "--out", t.TempDir(), "--json")
	var doc report.RefusalDoc
	if code != 1 || errw != want || json.Unmarshal([]byte(out), &doc) != nil || doc.Error.Code != contract.TraceTooFewEntries {
		t.Errorf("fit of a capture with 2 entries: exit %d, %q, %s", code, errw, out)
	}
	f := fitOf(t, "testdata/capture-chain")
	b, _ := os.ReadFile(f)
	other := filepath.Join(t.TempDir(), "fit.json")
	os.WriteFile(other, bytes.Replace(b, []byte(fit.Schema), []byte("rhobound.fit/v9"), 1), 0o644)
	out, _, code = exec(t, "predict", "--fit", other, "--json")
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc.Error.Code != contract.FitVersion {
		t.Errorf("a fit of another version: exit %d, %s", code, out)
	}
	for _, bad := range [][]string{
		{"predict", "--fit", f, "--workers", "b=2"},
		{"predict", "--fit", f, "--plain"},
		{"predict", "--fit", f, "--patch", "b=+1ms", "--changes", "x.json"},
		{"predict", "--fit", f, "--report", "v3"},
		{"predict", "testdata/capture-chain", "--report", "v1"},
		{"fit", "testdata/capture-chain"},
	} {
		if out, _, code := exec(t, append(bad, "--json")...); code != 1 || out != "" {
			t.Errorf("%v: exit %d, %q", bad, code, out)
		}
	}
	// --workers naming a service the capture does not have is refused, by fit as by predict
	for _, args := range [][]string{{"fit", "testdata/capture-chain", "--out", t.TempDir()}, {"predict", "testdata/capture-chain"}} {
		out, _, code := exec(t, append(args, "--workers", "a=1,nosuch=2", "--json")...)
		if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc.Error.Code != contract.ChangeUnknownService ||
			doc.Error.Subject != "nosuch" {
			t.Errorf("%v --workers nosuch=2: exit %d, %s", args, code, out)
		}
	}
}

// --name sets the capture name a fit records, and so report v1's capture field; without it the record holds the capture
// as given. The two records differ in that field only. A name that breaks the Name rule, or starts with /, is a usage
// error.
func TestFitName(t *testing.T) {
	inRepoRoot(t)
	const capture = "testdata/capture-chain"
	plain := fitOf(t, capture)
	named := fitOf(t, capture, "--name", "chain-prod")
	pb, _ := os.ReadFile(plain)
	nb, _ := os.ReadFile(named)
	p, err1 := fit.Unmarshal(pb)
	n, err2 := fit.Unmarshal(nb)
	if err1 != nil || err2 != nil || p.Capture != capture || n.Capture != "chain-prod" ||
		!bytes.Equal(bytes.Replace(nb, []byte(`"capture":"chain-prod"`), []byte(`"capture":"`+capture+`"`), 1), pb) {
		t.Fatalf("records: %v %v\n%s\n%s", err1, err2, pb, nb)
	}
	want, _, _ := exec(t, "predict", capture, "--json")
	got, errw, code := exec(t, "predict", "--fit", named, "--report", "v1", "--json")
	if code != 0 || got != strings.ReplaceAll(want, capture, "chain-prod") {
		t.Errorf("report v1 of the named fit: exit %d %s", code, errw)
	}
	// with a manifest, the name replaces the manifest's path
	dir := t.TempDir()
	if _, errw, code := exec(t, "check", capture, "--window", wideWindow, "--cut", "entry-start", "--out", dir); code != 0 {
		t.Fatalf("check --cut: exit %d %s", code, errw)
	}
	out := t.TempDir()
	if _, errw, code := exec(t, "fit", "--manifest", filepath.Join(dir, "manifest.json"), "--name", "team/chain-1", "--out", out); code != 0 {
		t.Fatalf("fit --manifest --name: exit %d %s", code, errw)
	}
	if b, _ := os.ReadFile(filepath.Join(out, fit.File)); !bytes.Contains(b, []byte(`"capture":"team/chain-1"`)) {
		t.Errorf("fit --manifest --name: %s", b)
	}
	// the names the MCP server's configuration takes and refuses (config.TestValidNameIsCheckName pins the same lists)
	account := strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	for _, good := range []string{"chain-prod", "team/chain-1", "ext:x", "a.b@c:d", "warn:x", "x1arn:x"} {
		if err := checkName("name", good); err != nil {
			t.Errorf("--name %q: %v", good, err)
		}
	}
	for _, bad := range []string{"", "/srv/captures/prod", "a b", strings.Repeat("x", 129), "prod\x1b[2J", "team/ext:db",
		"arn:aws:s3:::traces/prod", "team/arn:aws:x", "arn:probe", "team/arn:x", "team/ext:x",
		"ext:arn:aws:lambda:us-east-1:" + account + ":function:pay", "ext:x/arn:aws:y", "a//b", "a/./b", "a/../b",
		"x_arn:aws:y", "xarn:aws:y"} {
		out, errw, code := exec(t, "fit", capture, "--out", t.TempDir(), "--name", bad, "--json")
		if code != 1 || out != "" || !strings.HasPrefix(errw, "rhobound: --name: ") {
			t.Errorf("--name %q: exit %d, %q, %q", bad, code, out, errw)
		}
	}
}
