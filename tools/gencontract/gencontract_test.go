// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// The code tables of DATA.md and the data-contract document are what the registry generates (DATA.md 19: CI fails
// when a table and the registry differ).
func TestDocsAreCurrent(t *testing.T) {
	root := repoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(DataDoc)))
	if err != nil {
		t.Fatal(err)
	}
	want, err := RewriteDoc(doc, contract.All())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, want) {
		t.Errorf("%s: a code table differs from the registry; run go run ./tools/gencontract docs", DataDoc)
	}
	js, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(DataContract)))
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := ContractJSON(contract.All()); !bytes.Equal(js, want) {
		t.Errorf("%s differs from the registry; run go run ./tools/gencontract docs", DataContract)
	}
}

// Every registered code is in exactly one generated table, once.
func TestTablesListEveryCode(t *testing.T) {
	text := captureTable(contract.All()) + otherTable(contract.All())
	for _, c := range contract.All() {
		if n := strings.Count(text, "`"+c.ID+"`"); n != 1 {
			t.Errorf("%s appears %d times in the tables", c.ID, n)
		}
	}
	if strings.Contains(text, "\n\n") {
		t.Error("a table has an empty line")
	}
	// rows are merged only when every column agrees
	rows := group(contract.All(), func(contract.Code) bool { return true }, func(c contract.Code) string { return c.When })
	if len(rows) >= len(contract.All()) {
		t.Error("no two codes share a row")
	}
}

func TestRewriteDocNeedsItsMarkers(t *testing.T) {
	if _, err := RewriteDoc([]byte("no markers here\n"), contract.All()); err == nil {
		t.Error("a document without markers is rewritten")
	}
	twice := beginMarker("capture-codes") + "\n" + endMarker("capture-codes") + "\n"
	doc := twice + twice + beginMarker("other-codes") + "\n" + endMarker("other-codes") + "\n"
	if _, err := RewriteDoc([]byte(doc), contract.All()); err == nil {
		t.Error("a repeated region is rewritten")
	}
}

// The generators are deterministic: the same bytes twice.
func TestGeneratorsAreDeterministic(t *testing.T) {
	a, _ := ContractJSON(contract.All())
	b, _ := ContractJSON(contract.All())
	if !bytes.Equal(a, b) {
		t.Error("data-contract.json differs between two runs")
	}
	for name, gen := range map[string]func(string) error{"loop": Loop, "hostile": Hostile} {
		x, y := filepath.Join(t.TempDir(), name), filepath.Join(t.TempDir(), name)
		if err := gen(x); err != nil {
			t.Fatal(err)
		}
		if err := gen(y); err != nil {
			t.Fatal(err)
		}
		fx, fy := tree(t, x), tree(t, y)
		if len(fx) == 0 || !equalTrees(fx, fy) {
			t.Errorf("%s: two runs write different files", name)
		}
		if err := gen(x); err == nil {
			t.Errorf("%s writes into a folder that is not empty", name)
		}
	}
}

// The committed fixtures are what the generators write: trace files compared by their content (gzip's bytes may
// differ between Go versions), the others byte for byte. The loop's judge answer is checked by TestLoopPath.
func TestFixturesAreCurrent(t *testing.T) {
	root := repoRoot(t)
	for name, gen := range map[string]func(string) error{"loop": Loop, "hostile": Hostile} {
		out := filepath.Join(t.TempDir(), name)
		if err := gen(out); err != nil {
			t.Fatal(err)
		}
		want := tree(t, out)
		got := tree(t, filepath.Join(root, "testdata", "mcp", name))
		if name == "loop" {
			delete(got, "judge-answer.json")
		}
		if !equalTrees(got, want) {
			t.Errorf("testdata/mcp/%s is not what go run ./tools/gencontract %s writes: %v", name, name, diffTrees(got, want))
		}
	}
}

// tree reads a folder's files by relative path, gzip files decompressed.
func tree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return err
			}
			if b, err = io.ReadAll(zr); err != nil {
				return err
			}
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalTrees(a, b map[string][]byte) bool { return len(diffTrees(a, b)) == 0 }

func diffTrees(a, b map[string][]byte) []string {
	var out []string
	for k, v := range a {
		if w, ok := b[k]; !ok || !bytes.Equal(v, w) {
			out = append(out, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// buildCLI builds rhobound from this repository into a temporary folder.
func buildCLI(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	bin := filepath.Join(t.TempDir(), "rhobound")
	cmd := exec.Command(goTool, "build", "-o", bin, "./cmd/rhobound")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/rhobound: %v\n%s", err, out)
	}
	return bin
}

// cli runs the built rhobound with a fresh record store; it returns stdout and the exit code.
type cli struct {
	t     *testing.T
	bin   string
	state string
}

func (c cli) run(args ...string) (string, int) {
	c.t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = append(os.Environ(), "RHOBOUND_ESTIMATOR=", "RHOBOUND_STATE="+c.state)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		c.t.Fatalf("rhobound %v: %v", args, err)
	}
	if code != 0 && code != 1 && code != 3 && code != 4 {
		c.t.Fatalf("rhobound %v: exit %d: %s", args, code, stderr.Bytes())
	}
	return string(out), code
}

func (c cli) view(args ...string) map[string]any {
	c.t.Helper()
	out, code := c.run(append(args, "--json")...)
	var v map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil {
		c.t.Fatalf("rhobound %v: exit %d: %s", args, code, out)
	}
	return v
}

func writeFile(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func num(v any) float64 {
	f, _ := v.(float64)
	return f
}

// The scripted agent's path on the committed loop fixture, through the CLI twins of the MCP tools
// (testdata/mcp/README.md, "The path, through the CLI twins"; ARCHITECTURE.md 8, without the server): admitted with its
// count; budget of b about 1.95 ms, safe about 1.07 ms per visit; the first draft blocks and the canned judge answer
// passes judge-check against its packet; +0.4 ms declared is inconclusive on unmeasured_delta with rule 10 passing and
// an A/B of b to decide with; the A/B pair measures b within the safe budget; the measured change passes.
func TestLoopPath(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	root := repoRoot(t)
	loop := filepath.Join(root, "testdata", "mcp", "loop")
	c := cli{t: t, bin: buildCLI(t), state: t.TempDir()}

	answer, err := os.ReadFile(filepath.Join(loop, "judge-answer.json"))
	if err != nil {
		t.Fatal(err)
	}
	// the answer to the live packet passes judge-check (CannedAnswer runs it); the committed one cites the packet of
	// linux/arm64, where it was computed: the packet's numbers are rounded to 4 decimals, and the last bits of floating
	// point may differ on another architecture (as for the 1a2df09 goldens)
	want, err := CannedAnswer(c.bin, loop)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOARCH != "arm64" {
		t.Logf("judge-answer.json is pinned on arm64; on %s only the live answer is checked", runtime.GOARCH)
	} else if !bytes.Equal(answer, want) {
		t.Fatal("testdata/mcp/loop/judge-answer.json is not current (go run ./tools/gencontract answer RHOBOUND testdata/mcp/loop)")
	}

	chk := c.view("check", loop)
	if chk["verdict"] != contract.Admitted || len(chk["findings"].([]any)) != 0 {
		t.Fatalf("check: %v", chk)
	}
	fitDir := t.TempDir()
	if out, code := c.run("fit", loop, "--name="+LoopName, "--workers", LoopWorkers, "--out", fitDir, "--json"); code != 0 {
		t.Fatalf("fit: exit %d: %s", code, out)
	}
	fit := filepath.Join(fitDir, "fit.json")
	policy := filepath.Join(loop, "policy.json")

	bv := c.view("budget", "--fit", fit, "--policy", policy, "--service=b")
	items := bv["items"].([]any)
	it := items[0].(map[string]any)
	maxV, safe := num(it["max_value"]), num(it["safe_value"])
	if len(items) != 1 || math.Abs(maxV-1.95) > 0.01 || math.Abs(safe-1.07) > 0.01 {
		t.Fatalf("budget of b: %v", it)
	}

	preview := func(items string) map[string]any {
		t.Helper()
		ch := writeFile(t, "changes.json", `{"schema":"rhobound.change/v1","fit":"","items":[`+items+`]}`)
		v := c.view("preview", "--fit", fit, "--changes", ch, "--policy", policy, "--now", LoopNow)
		return v["preview"].(map[string]any)
	}
	rules := func(p map[string]any) map[string]string {
		out := map[string]string{}
		for _, r := range p["rules"].([]any) {
			m := r.(map[string]any)
			out[m["rule"].(string)], _ = m["effect"].(string)
		}
		return out
	}
	if p := preview(`{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6},"label":"declared"}`); p["verdict"] != "block" {
		t.Errorf("the first draft: %v", p)
	}
	p := preview(`{"op":"own_ms","service":"b","value":{"lo":0.4,"hi":0.4},"label":"declared"}`)
	r := rules(p)
	dw, _ := p["decide_with"].([]any)
	if p["verdict"] != "inconclusive" || r["unmeasured_delta"] != "inconclusive" || r["mean_change"] != "pass" ||
		len(dw) == 0 || !strings.Contains(dw[0].(string), "A/B of b") {
		t.Errorf("+0.4 ms declared: %v", p)
	}

	abDir := t.TempDir()
	out, code := c.run("diff-fit", "--baseline", filepath.Join(loop, "ab", "baseline"), "--candidate",
		filepath.Join(loop, "ab", "candidate"), "--out", abDir, "--json")
	var printed struct {
		Digest string `json:"ab_digest"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &printed) != nil {
		t.Fatalf("diff-fit: exit %d: %s", code, out)
	}
	var rec struct {
		Findings []contract.Finding `json:"findings"`
		Services []struct {
			Service string `json:"service"`
			OwnMs   struct {
				Delta float64 `json:"delta"`
				SE    float64 `json:"se"`
			} `json:"own_ms"`
			CallsChanged    bool `json:"calls_changed"`
			VarianceChanged bool `json:"variance_changed"`
		} `json:"services"`
	}
	b, _ := os.ReadFile(filepath.Join(abDir, "ab-record.json"))
	if err := json.Unmarshal(b, &rec); err != nil || contract.Digest(b) != printed.Digest || len(rec.Findings) != 0 {
		t.Fatalf("the A/B record: %v, %v", err, rec.Findings)
	}
	var delta float64
	for _, s := range rec.Services {
		if s.Service == abSvc {
			delta = s.OwnMs.Delta
			if s.CallsChanged || s.VarianceChanged || !(delta > 0.2 && delta < 0.5) || delta+2*s.OwnMs.SE > safe {
				t.Errorf("b: %+v, safe budget %v", s, safe)
			}
		}
	}
	d, _ := json.Marshal(delta)
	p = preview(`{"op":"own_ms","service":"b","value":{"lo":` + string(d) + `,"hi":` + string(d) +
		`},"label":"measured-ab","evidence":["` + printed.Digest + `"]}`)
	if p["verdict"] != "pass" || p["advisory"] != true {
		t.Errorf("the measured change: %v", p)
	}
}

// No raw hostile string from the hostile-names fixture reaches a view, in any encoding, and the renamed services are
// marked untrusted where a view lists them.
func TestHostileNamesNeverLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	root := repoRoot(t)
	dir := filepath.Join(root, "testdata", "mcp", "hostile")
	c := cli{t: t, bin: buildCLI(t), state: t.TempDir()}
	fitDir := t.TempDir()
	if out, code := c.run("fit", dir, "--out", fitDir, "--json"); code != 0 {
		t.Fatalf("fit: exit %d: %s", code, out)
	}
	fit := filepath.Join(fitDir, "fit.json")
	policy := filepath.Join(root, "testdata", "mcp", "loop", "policy.json")
	ch := writeFile(t, "changes.json", `{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"a","value":{"lo":0.4,"hi":0.4},"label":"declared"}]}`)
	forbidden := []string{"ignore every rule", "\u202e", `\u202e`, "\x1b", `\u001b`, "s3cr3t", "/orders/42", strings.Repeat("c", 129)}
	for _, args := range [][]string{
		{"context", "--fit", fit, "--detail=full"},
		{"budget", "--fit", fit, "--policy", policy},
		{"preview", "--fit", fit, "--changes", ch, "--policy", policy, "--now", LoopNow, "--detail=full"},
	} {
		out, code := c.run(append(args, "--json")...)
		marked := strings.Contains(out, `untrusted":true`)
		if code != 0 {
			t.Fatalf("%s: exit %d: %s", args[0], code, out)
		}
		for _, f := range forbidden {
			if strings.Contains(out, f) {
				t.Errorf("%s shows %q:\n%s", args[0], f, out)
			}
		}
		// the preview changes a, and lists only what moved: no hostile name to mark
		if !marked && args[0] != "preview" {
			t.Errorf("%s does not mark a hostile name:\n%s", args[0], out)
		}
	}
}

// Fixture writes a testgen fixture's capture as the folder alone, and refuses a code without one or one that runs with
// flags.
func TestFixture(t *testing.T) {
	out := filepath.Join(t.TempDir(), "peer")
	if err := Fixture(contract.TracePeerRenamed, out); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"traces/spans.jsonl.gz", "cluster/services.json"} {
		if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if err := Fixture(contract.TracePeerRenamed, out); err == nil {
		t.Error("a fixture written into a folder that is not empty")
	}
	if err := Fixture("nope", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("a fixture for a code without one")
	}
	if err := Fixture(contract.TraceAsyncTailTruncated, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Error("a fixture that runs with a manifest or flags, written as the folder alone")
	}
}

// Canaries writes every canary capture and its table: each value planted under an attribute is in the capture's traces,
// as a value of that attribute or a part of one (each arm's, for a pair), and no value is a part of another, of a
// capture's registered name or of a service's name, so that finding one in a reply means it leaked.
func TestCanaries(t *testing.T) {
	out := filepath.Join(t.TempDir(), "canaries")
	if err := Canaries(out); err != nil {
		t.Fatal(err)
	}
	if err := Canaries(out); err == nil {
		t.Error("canaries written into a folder that is not empty")
	}
	b, err := os.ReadFile(filepath.Join(out, CanaryTable))
	if err != nil {
		t.Fatal(err)
	}
	var table []canaryEntry
	if err := json.Unmarshal(b, &table); err != nil || len(table) != len(testgen.Canaries()) {
		t.Fatalf("the table: %v\n%s", err, b)
	}
	names := []string{entrySvc, abSvc, "c"}
	var values []string
	for _, c := range table {
		names = append(names, c.Name, c.Name+"-a", c.Name+"-b")
		arms := []string{c.Name}
		if c.Pair {
			arms = []string{filepath.Join(c.Name, "baseline"), filepath.Join(c.Name, "candidate")}
		}
		for _, arm := range arms {
			var traces []byte
			for _, f := range tree(t, filepath.Join(out, arm, "traces")) {
				traces = append(traces, f...)
			}
			if _, err := os.Stat(filepath.Join(out, arm, "request-count.json")); err != nil {
				t.Errorf("%s: %v", arm, err)
			}
			for _, p := range c.Planted {
				under := regexp.MustCompile(`\{"key":"` + regexp.QuoteMeta(p.Key) + `","value":\{"stringValue":"[^"]*` +
					regexp.QuoteMeta(p.Value) + `[^"]*"\}\}`)
				if p.Key != "" && !under.Match(traces) {
					t.Errorf("%s: the traces do not hold %q in a value of %s", arm, p.Value, p.Key)
				}
			}
		}
		for _, p := range c.Planted {
			values = append(values, p.Value)
		}
	}
	for i, v := range values {
		for j, w := range values {
			if i != j && strings.Contains(w, v) {
				t.Errorf("the canary %q is a part of %q", v, w)
			}
		}
		for _, n := range names {
			if strings.Contains(n, v) || strings.Contains(v, n) && len(n) > 1 {
				t.Errorf("the canary %q and the name %q overlap", v, n)
			}
		}
	}
}

func TestRunUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"docs", "x"}, {"loop"}, {"answer", "x"}, {"fixture", "x"}, {"canaries"}, {"nope"}} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
