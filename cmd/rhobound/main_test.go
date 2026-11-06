// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

const fixture = "../../testdata/capture-chain"

func exec(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := run(args, &out, &errw)
	return out.String(), errw.String(), code
}

func TestPredictText(t *testing.T) {
	out, errw, code := exec(t, "predict", fixture, "--patch", "b=+6ms")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	for _, want := range []string{"rhobound-cli: mean latency from traces", "a *", "b", "c", "change applied: b +6 ms", "scope"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestPredictJSONAndFlagsAfterTheArgument(t *testing.T) {
	out, errw, code := exec(t, "predict", fixture, "--json", "--patch", "b=+6ms")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep struct {
		Version  string `json:"version"`
		Requests int64  `json:"requests"`
		Services []struct {
			Name     string   `json:"name"`
			Entry    bool     `json:"entry"`
			ModelMs  float64  `json:"model_ms"`
			ChangeMs *float64 `json:"change_ms"`
			Util     float64  `json:"utilization"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if rep.Version != "rhobound-cli.report/v1" || rep.Requests < 2000 || len(rep.Services) != 3 {
		t.Fatalf("report: %+v", rep)
	}
	for _, s := range rep.Services {
		if s.ChangeMs == nil || s.ModelMs <= 0 || s.Util <= 0 || s.Util >= 1 {
			t.Errorf("service %+v", s)
		}
		if s.Name == "a" && (!s.Entry || *s.ChangeMs < 6) {
			t.Errorf("the entry should move by at least the 6 ms: %+v", s)
		}
		// c is downstream of the change: only through b's changed departure variability, a fraction of a ms
		if s.Name == "c" && math.Abs(*s.ChangeMs) > 0.3 {
			t.Errorf("c should hardly move: %+v", s)
		}
	}
}

// TestPredictFlagsGivenEmpty: --fit, --changes and --report did not exist at 1a2df09, which refused them; given with an
// empty value they are given all the same, so predict refuses them with a usage message and prints no report, never
// reading the capture as if they were absent. Given with a value and a capture, they are refused the same way: exit
// 1, nothing on standard output, and this build's usage message on standard error where 1a2df09 printed its own.
func TestPredictFlagsGivenEmpty(t *testing.T) {
	for _, args := range [][]string{
		{"predict", fixture, "--fit="}, {"predict", fixture, "--fit", ""}, {"predict", fixture, "--changes="},
		{"predict", fixture, "--report="}, {"predict", fixture, "--report", ""}, {"predict", "--fit="},
		{"predict", "--fit", fixture, "--changes="}, {"predict", "--fit", fixture, "--report="},
		{"predict", fixture, "--fit", "x.json"}, {"predict", fixture, "--changes", "x.json"},
		{"predict", fixture, "--report", "v1"}, {"predict", fixture, "--report", "v1", "--json"},
		{"predict", fixture, "--report", "v2"},
	} {
		out, errw, code := exec(t, args...)
		if code != 1 || out != "" || errw == "" {
			t.Errorf("%q: exit %d, stdout %.80q, stderr %q", args, code, out, errw)
		}
	}
}

func TestOutDirWritesBothFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	if _, errw, code := exec(t, "predict", fixture, "--out", dir); code != 0 {
		t.Fatalf("%s", errw)
	}
	for _, f := range []string{"report.txt", "report.json"} {
		if st, err := os.Stat(filepath.Join(dir, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestRefusesHighLoadAndBadPatch(t *testing.T) {
	_, errw, code := exec(t, "predict", fixture, "--max-rho", "0.2")
	if code != 1 || !strings.Contains(errw, "refusing to predict") {
		t.Errorf("exit %d, %q: should refuse above the utilization limit", code, errw)
	}
	_, errw, code = exec(t, "predict", fixture, "--patch", "b=+500ms")
	if code != 1 || !strings.Contains(errw, "unstable") && !strings.Contains(errw, "utilization") {
		t.Errorf("a change that saturates b should be refused: exit %d %q", code, errw)
	}
	for _, p := range []string{"nosuch=+1ms", "b=banana", "b"} {
		if _, errw, code := exec(t, "predict", fixture, "--patch", p); code != 1 || !strings.Contains(errw, "--patch") {
			t.Errorf("patch %q: exit %d %q", p, code, errw)
		}
	}
	if _, _, code := exec(t, "predict", fixture, "--max-rho", "2"); code != 1 {
		t.Error("--max-rho above 0.95 should be refused")
	}
}

func TestInferThenModelRoundTrip(t *testing.T) {
	topo, _, code := exec(t, "infer", fixture)
	if code != 0 {
		t.Fatal("infer failed")
	}
	path := filepath.Join(t.TempDir(), "topo.json")
	if err := os.WriteFile(path, []byte(topo), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := exec(t, "model", path, "--rps", "25", "--patch", "c=+20%")
	if code != 0 {
		t.Fatalf("model: %s", errw)
	}
	if !strings.Contains(out, "c") || !strings.Contains(out, "change applied") {
		t.Errorf("model output:\n%s", out)
	}
	if _, _, code := exec(t, "model", path, "--rps", "100000"); code != 1 {
		t.Error("an unstable load should fail")
	}
}

// `rhobound model` accepted --max-rho and --out and ignored both: a topology at 90% was evaluated at the default
// limit of 85%, and --out DIR wrote nothing. Its report showed 0.00 as what the traces saw, with no traces.
func TestModelLimitOutAndFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "topo.json")
	doc := `{"name":"m","entry":{"a":1},"services":[{"name":"a","s":5,"children":["b"]},{"name":"b","s":10}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errw, code := exec(t, "model", path, "--rps", "90"); code != 1 || !strings.Contains(errw, "refusing to evaluate") ||
		!strings.Contains(errw, "b would run at 90.0% utilization") || !strings.Contains(errw, "--max-rho") {
		t.Errorf("90%% at the default limit: exit %d, %q", code, errw)
	}
	out, errw, code := exec(t, "model", path, "--rps", "90", "--max-rho", "0.95")
	if code != 0 || !strings.Contains(out, "busiest   b at 90% utilization") {
		t.Errorf("90%% under --max-rho 0.95: exit %d, %s\n%s", code, errw, out)
	}
	if _, errw, code := exec(t, "model", path, "--rps", "80", "--patch", "b=+1ms"); code != 1 ||
		!strings.Contains(errw, "refusing the change") || !strings.Contains(errw, "b would run at 88.0% utilization") {
		t.Errorf("a change to 88%%: exit %d, %q", code, errw)
	}
	if _, errw, code := exec(t, "model", path, "--rps", "20", "--max-rho", "2"); code != 1 || !strings.Contains(errw, "--max-rho") {
		t.Errorf("--max-rho 2: exit %d, %q", code, errw)
	}
	for _, f := range [][]string{{"--workers", "b=2"}, {"--kept-share", "0.5"}} {
		if _, errw, code := exec(t, append([]string{"model", path, "--rps", "20"}, f...)...); code != 1 || !strings.Contains(errw, f[0]) {
			t.Errorf("%s: exit %d, %q", f[0], code, errw)
		}
	}
	outDir := filepath.Join(dir, "r")
	out, errw, code = exec(t, "model", path, "--rps", "20", "--out", outDir)
	if code != 0 || !strings.Contains(errw, "wrote") {
		t.Fatalf("--out: exit %d, %q", code, errw)
	}
	for _, f := range []string{"report.txt", "report.json"} {
		if st, err := os.Stat(filepath.Join(outDir, f)); err != nil || st.Size() == 0 {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, want := range []string{"mean latency of a topology", "topology  " + path, "load      20.0 requests/s"} {
		if !strings.Contains(out, want) {
			t.Errorf("model report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "window") || strings.Contains(out, "0 spans") {
		t.Errorf("a topology has no capture window:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") { // service rps pods x w own cs2 util wait seen-wait model seen err
		if f := strings.Fields(l); len(f) > 0 && (f[0] == "a" || f[0] == "b") {
			if f[0] == "a" {
				f = append(f[:1], f[2:]...) // the entry mark
			}
			if len(f) < 13 || f[9] != "-" || f[11] != "-" {
				t.Errorf("seen columns should be '-' in a topology's report: %q", l)
			}
		}
	}
}

func TestVersionHelpAndUnknown(t *testing.T) {
	if out, _, code := exec(t, "version"); code != 0 || !strings.HasPrefix(out, "rhobound ") {
		t.Errorf("version: %d %q", code, out)
	}
	if out, _, code := exec(t, "help"); code != 0 || !strings.Contains(out, "predict") {
		t.Errorf("help: %d", code)
	}
	if _, _, code := exec(t, "frobnicate"); code != 2 {
		t.Error("an unknown command should exit 2")
	}
	if _, _, code := exec(t); code != 2 {
		t.Error("no command should exit 2")
	}
	if _, errw, code := exec(t, "predict", "/no/such/capture"); code != 1 || errw == "" {
		t.Error("a missing capture should fail with a message")
	}
}

func TestPlainSwitch(t *testing.T) {
	refined, _, _ := exec(t, "predict", fixture, "--json")
	plain, _, code := exec(t, "predict", fixture, "--json", "--plain")
	if code != 0 {
		t.Fatal("--plain failed")
	}
	var a, b struct {
		Model string `json:"model"`
	}
	json.Unmarshal([]byte(refined), &a)
	json.Unmarshal([]byte(plain), &b)
	if !strings.HasPrefix(a.Model, "refined") || !strings.HasPrefix(b.Model, "basic") || refined == plain {
		t.Errorf("models: %q vs %q", a.Model, b.Model)
	}
}

// Glue (time after the calls that holds no worker) gets its own column, shown only when some service has it.
func TestGlueColumn(t *testing.T) {
	if out, _, _ := exec(t, "predict", fixture); strings.Contains(out, "glue") {
		t.Errorf("the fixture has no glue, so no glue column:\n%s", out)
	}
	path := filepath.Join(t.TempDir(), "topo.json")
	doc := `{"name":"g","entry":{"a":1},"services":[{"name":"a","s":5,"children":["b"],"glue":3},{"name":"b","s":10}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := exec(t, "model", path, "--rps", "20")
	if code != 0 || !strings.Contains(out, "glue ms") || !strings.Contains(out, "3.00") {
		t.Errorf("model (%d, %s):\n%s", code, errw, out)
	}
	js, _, _ := exec(t, "model", path, "--rps", "20", "--json")
	var rep struct {
		Services []struct {
			Name    string  `json:"name"`
			GlueMs  float64 `json:"glue_ms"`
			ModelMs float64 `json:"model_ms"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(js), &rep); err != nil || len(rep.Services) != 2 {
		t.Fatalf("json: %v\n%s", err, js)
	}
	if a := rep.Services[0]; a.Name != "a" || a.GlueMs != 3 {
		t.Errorf("a: %+v", a)
	}
}

// Uneven replica shares are marked in the pods column, the legend says the change keeps them, the JSON
// rows carry them, and the utilization limit sees the busy replica.
func TestReplicaSharesInTheReport(t *testing.T) {
	if out, _, _ := exec(t, "predict", fixture); strings.Contains(out, "pods~") {
		t.Errorf("the fixture has one pod per service, so no shares:\n%s", out)
	}
	path := filepath.Join(t.TempDir(), "topo.json")
	doc := `{"name":"s","entry":{"a":1},"services":[{"name":"a","s":2,"children":["b"]},
	 {"name":"b","s":10,"routing":"random","shares":[0.7,0.3]}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errw, code := exec(t, "model", path, "--rps", "60", "--patch", "b=+1ms")
	if code != 0 || !strings.Contains(out, "2~ x 1") || !strings.Contains(out, "kept at any load and after the change") ||
		!strings.Contains(out, "42% utilization") {
		t.Errorf("model (%d, %s):\n%s", code, errw, out)
	}
	js, _, _ := exec(t, "model", path, "--rps", "60", "--json")
	var rep struct {
		Services []struct {
			Name   string    `json:"name"`
			Shares []float64 `json:"replica_shares"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(js), &rep); err != nil || len(rep.Services) != 2 {
		t.Fatalf("json: %v\n%s", err, js)
	}
	if b := rep.Services[1]; b.Name != "b" || len(b.Shares) != 2 || b.Shares[0] != 0.7 || rep.Services[0].Shares != nil {
		t.Errorf("rows: %+v", rep.Services)
	}
	if _, _, code := exec(t, "model", path, "--rps", "150"); code != 1 {
		t.Error("the busy replica past saturation (105%, the service at 75%) should fail")
	}
}

// tspan is one span of a capture written by writeCapture: times in ms, ids as hex.
type tspan struct {
	svc, pod, id, parent string
	kind                 int
	start, end           float64
	trace                int
	peer                 string
}

// writeCapture writes spans as a capture folder (traces/spans.jsonl, one request per service and pod).
func writeCapture(t *testing.T, spans []tspan) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	by := map[[2]string][]tspan{}
	var keys [][2]string
	for _, sp := range spans {
		k := [2]string{sp.svc, sp.pod}
		if by[k] == nil {
			keys = append(keys, k)
		}
		by[k] = append(by[k], sp)
	}
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}},`+
			`{"key":"k8s.pod.name","value":{"stringValue":%q}}]},"scopeSpans":[{"spans":[`, k[0], k[1])
		for i, sp := range by[k] {
			if i > 0 {
				b.WriteByte(',')
			}
			ns := func(ms float64) int64 { return 1_700_000_000_000_000_000 + int64(ms*1e6) }
			fmt.Fprintf(&b, `{"traceId":"%032x","spanId":%q,"parentSpanId":%q,"kind":%d,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"`,
				sp.trace, sp.id, sp.parent, sp.kind, ns(sp.start), ns(sp.end))
			if sp.peer != "" {
				fmt.Fprintf(&b, `,"attributes":[{"key":"server.address","value":{"stringValue":%q}}]`, sp.peer)
			}
			b.WriteByte('}')
		}
		b.WriteString("]}]}]}\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

type reportChange struct {
	Services []struct {
		Name     string   `json:"name"`
		Workers  int      `json:"workers_per_pod"`
		ChangeMs *float64 `json:"change_ms"`
	} `json:"services"`
	Changes []struct {
		Service string  `json:"service"`
		DeltaMs float64 `json:"delta_ms"`
		GlueMs  float64 `json:"glue_delta_ms"`
	} `json:"changes"`
	Warnings []string `json:"warnings"`
}

// The code review's cap-skew0_1: fe calls be through a SERVER -> SERVER link after about 1 ms of work and works 1 ms after
// the call returns (glue). --patch fe=+100% doubles fe's whole own time, about +2 ms per visit; at 29a7ed9 it
// scaled the service time only and predicted +1.01 ms (094b99a: +2.01).
func TestPatchPercentScalesTheGlue(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	var spans []tspan
	at := 0.0
	for i := 1; i <= 3000; i++ {
		at += rng.ExpFloat64() * 1000 / 40
		pre, be := rng.ExpFloat64(), 4*rng.ExpFloat64()
		root := fmt.Sprintf("%016x", 2*i)
		spans = append(spans, tspan{"fe", "fe-0", root, "", 2, at, at + pre + be + 1, i, ""},
			tspan{"be", "be-0", fmt.Sprintf("%016x", 2*i+1), root, 2, at + pre, at + pre + be, i, ""})
	}
	capture := writeCapture(t, spans)
	out, errw, code := exec(t, "predict", capture, "--json", "--patch", "fe=+100%")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep reportChange
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	ch := rep.Changes[0]
	if math.Abs(ch.DeltaMs-2) > 0.15 || math.Abs(ch.GlueMs-1) > 0.05 {
		t.Errorf("fe +100%%: the change is %+.3f ms of which %+.3f ms glue; want about +2 and +1", ch.DeltaMs, ch.GlueMs)
	}
	for _, s := range rep.Services {
		if s.Name == "fe" && (s.ChangeMs == nil || *s.ChangeMs < ch.DeltaMs) {
			t.Errorf("fe's latency changes by %v ms, less than its own time (%+.3f ms)", s.ChangeMs, ch.DeltaMs)
		}
	}
	text, _, _ := exec(t, "predict", capture, "--patch", "fe=+100%")
	if !strings.Contains(text, "ms glue);") {
		t.Errorf("the change line does not show the glue part:\n%s", text)
	}
	// +N ms is extra work holding the worker: no glue part
	out, _, _ = exec(t, "predict", capture, "--json", "--patch", "fe=+1ms")
	rep = reportChange{}
	json.Unmarshal([]byte(out), &rep)
	if ch := rep.Changes[0]; ch.DeltaMs != 1 || ch.GlueMs != 0 {
		t.Errorf("fe +1ms: %+v", ch)
	}
	// -150% would make the glue negative where the service time is 0
	path := filepath.Join(t.TempDir(), "topo.json")
	if err := os.WriteFile(path, []byte(`{"entry":{"a":1},"services":[{"name":"a","s":0,"glue":2}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errw, code := exec(t, "model", path, "--rps", "10", "--patch", "a=-150%"); code != 1 || !strings.Contains(errw, "negative glue") {
		t.Errorf("a=-150%%: exit %d, %q", code, errw)
	}
}

// The code review's cap-rare: fe (400 visits) calls rare once. --workers rare=4 was ignored at 29a7ed9, silently: rare's
// queue has too few jobs to rebuild, and the count stayed 1 (shown as if the user had set it).
func TestWorkersForAServiceTooRareToRebuild(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	var spans []tspan
	at := 0.0
	for i := 1; i <= 400; i++ {
		at += rng.ExpFloat64() * 1000 / 20
		root := fmt.Sprintf("%016x", 3*i)
		if i == 7 {
			cl := fmt.Sprintf("%016x", 3*i+1)
			spans = append(spans, tspan{"fe", "fe-0", root, "", 2, at, at + 12, i, ""},
				tspan{"fe", "fe-0", cl, root, 3, at + 2, at + 9, i, "rare"},
				tspan{"rare", "rare-0", fmt.Sprintf("%016x", 3*i+2), cl, 2, at + 3, at + 8, i, ""})
			continue
		}
		spans = append(spans, tspan{"fe", "fe-0", root, "", 2, at, at + 3 + 5*rng.ExpFloat64(), i, ""})
	}
	out, errw, code := exec(t, "predict", writeCapture(t, spans), "--json", "--workers", "rare=4")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep reportChange
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	for _, s := range rep.Services {
		if s.Name == "rare" && s.Workers != 4 {
			t.Errorf("rare: %d workers per pod, want the 4 given", s.Workers)
		}
	}
	warned := false
	for _, w := range rep.Warnings {
		warned = warned || (strings.HasPrefix(w, "rare:") && strings.Contains(w, "--workers 4"))
	}
	if !warned {
		t.Errorf("no warning that --workers is taken as given for rare: %q", rep.Warnings)
	}
}

// A framework that opens a SERVER span inside the HTTP instrumentation's (Next.js, flagd's handler) and a client that
// opens a CLIENT span around the HTTP client's (.NET's gRPC client): fe -> be, each request with both. They are read
// as one request and one call, and the report says how many spans were merged, in the text, in the JSON and in
// infer's warning. Before, fe was a service calling itself and the capture was refused as a cycle.
func TestNestedSpansAreMergedAndReported(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	var spans []tspan
	at := 0.0
	const n = 1500
	for i := 1; i <= n; i++ {
		at += rng.ExpFloat64() * 1000 / 30
		x := 1 + 3*rng.ExpFloat64()
		id := func(k int) string { return fmt.Sprintf("%016x", 10*i+k) }
		spans = append(spans,
			tspan{"fe", "fe-0", id(1), "", 2, at, at + x + 6, i, ""},              // the HTTP instrumentation's SERVER span
			tspan{"fe", "fe-0", id(2), id(1), 2, at + 0.02, at + x + 5.98, i, ""}, // the framework's, inside it
			tspan{"fe", "fe-0", id(3), id(2), 3, at + x, at + x + 5, i, "be"},     // the gRPC client's CLIENT span
			tspan{"fe", "fe-0", id(4), id(3), 3, at + x + 0.03, at + x + 4.95, i, "be"},
			tspan{"be", "be-0", id(5), id(4), 2, at + x + 1, at + x + 4, i, ""})
	}
	capture := writeCapture(t, spans)
	out, errw, code := exec(t, "predict", capture, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep struct {
		Broken int64 `json:"broken_traces"`
		Nested []struct {
			Service string `json:"service"`
			Server  int64  `json:"server_spans"`
			Client  int64  `json:"client_spans"`
		} `json:"nested_spans_merged"`
		Services []struct {
			Name string `json:"name"`
		} `json:"services"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Nested) != 1 || rep.Nested[0].Service != "fe" || rep.Nested[0].Server != n || rep.Nested[0].Client != n {
		t.Errorf("nested_spans_merged: %+v, want fe with %d SERVER and %d CLIENT spans", rep.Nested, n, n)
	}
	if rep.Broken != 0 || len(rep.Services) != 2 {
		t.Errorf("%d broken traces, %d services (want 0 and 2: fe and be, no ext:be)", rep.Broken, len(rep.Services))
	}
	want := fmt.Sprintf("fe %d SERVER (the outer span kept), fe %d CLIENT (the span the callee's SERVER span is under kept, else the outermost)", n, n)
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
		t.Errorf("no warning with %q: %q", want, rep.Warnings)
	}
	text, _, _ := exec(t, "predict", capture)
	if !strings.Contains(text, "nested spans of one kind in one pod") || !strings.Contains(text, want) {
		t.Errorf("the text report does not name the merged spans:\n%s", text)
	}
	_, errw, code = exec(t, "infer", capture)
	if code != 0 || !strings.Contains(errw, "warning: nested spans of one kind in one pod") {
		t.Errorf("infer: exit %d, standard error %q", code, errw)
	}
	// a capture without nested spans has no field and no warning
	out, _, _ = exec(t, "predict", fixture, "--json")
	if strings.Contains(out, "nested_spans_merged") || strings.Contains(out, "nested spans") {
		t.Errorf("the example capture reports nested spans")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

// `predict CAPTURE > /dev/full` exited 0 with nothing written: the text report ignored its write errors.
func TestTextReportWriteErrors(t *testing.T) {
	var errw bytes.Buffer
	if code := run([]string{"predict", fixture}, failWriter{}, &errw); code != 1 || !strings.Contains(errw.String(), "no space left") {
		t.Errorf("predict: exit %d, %q", code, errw.String())
	}
	topo, _, _ := exec(t, "infer", fixture)
	path := filepath.Join(t.TempDir(), "topo.json")
	if err := os.WriteFile(path, []byte(topo), 0o644); err != nil {
		t.Fatal(err)
	}
	errw.Reset()
	if code := run([]string{"model", path, "--rps", "25"}, failWriter{}, &errw); code != 1 || !strings.Contains(errw.String(), "no space left") {
		t.Errorf("model: exit %d, %q", code, errw.String())
	}
	if f, err := os.OpenFile("/dev/full", os.O_WRONLY, 0); err == nil {
		defer f.Close()
		errw.Reset()
		if code := run([]string{"predict", fixture}, f, &errw); code != 1 {
			t.Errorf("predict > /dev/full: exit %d, %q", code, errw.String())
		}
	}
}

// shipped20s is the first 20 s of the OpenTelemetry Demo on a real cluster under its shipped load mix (see
// internal/infer/counts_real_test.go): the cart calls its Redis 1, 2 or 3 times per visit, the frontend serves endpoints
// of different call shapes.
const shipped20s = "../../internal/infer/testdata/counts/shipped-20s"

// Calls per visit that vary are accepted: the report carries the visits by calls and how the model takes them (the mean
// count; a mixture of call shapes), in the text, in the JSON and in the warnings, and infer's topology runs under
// `model`.
func TestVariableCallsInTheReport(t *testing.T) {
	out, errw, code := exec(t, "predict", shipped20s, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep struct {
		Services []struct {
			Name          string `json:"name"`
			VariableCalls []struct {
				Callee        string  `json:"callee"`
				MeanPerVisit  float64 `json:"mean_per_visit"`
				VisitsByCalls []struct {
					Calls  int   `json:"calls"`
					Visits int64 `json:"visits"`
				} `json:"visits_by_calls"`
				ModelledAs string `json:"modelled_as"`
			} `json:"variable_calls"`
			CallShapes []struct {
				Shape string  `json:"shape"`
				Share float64 `json:"share"`
			} `json:"call_shapes"`
		} `json:"services"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, s := range rep.Services {
		for _, v := range s.VariableCalls {
			seen[s.Name+" -> "+v.Callee+" "+v.ModelledAs] = true
			if s.Name == "cart" && (len(v.VisitsByCalls) != 3 || v.VisitsByCalls[2].Calls != 3 || math.Abs(v.MeanPerVisit-1.7246) > 1e-3) {
				t.Errorf("cart: %+v", v)
			}
		}
		if s.Name == "frontend" {
			// the front end stamps its spans on whole milliseconds: two cart calls that follow one another within a
			// millisecond are "cart then cart", not "cart x2" (at once); a recommendation and then four lookups likewise
			share := map[string]float64{}
			for _, sh := range s.CallShapes {
				share[sh.Shape] = sh.Share
			}
			if len(s.CallShapes) < 8 || s.CallShapes[0].Shape != "product-catalog" || share["cart then cart"] < 0.13 || share["cart x2"] > 0.01 ||
				share["recommendation then product-catalog x4"] < 0.095 || share["product-catalog x4 + recommendation"] > 0.01 {
				t.Errorf("frontend call shapes: %+v", s.CallShapes)
			}
		}
		if s.Name != "frontend" && len(s.CallShapes) != 0 {
			t.Errorf("%s has call shapes %+v", s.Name, s.CallShapes)
		}
	}
	for _, k := range []string{"cart -> ext:valkey-cart mean count", "checkout -> currency mean count", "frontend -> cart mixture of call shapes"} {
		if !seen[k] {
			t.Errorf("no %q in the variable calls of the JSON report: %v", k, seen)
		}
	}
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "cart -> ext:valkey-cart: calls per visit vary as {1: 122, 2: 20, 3: 65}; only their mean (1.72 per visit)")
	}) {
		t.Errorf("no warning for the cart's counts: %q", rep.Warnings)
	}
	text, _, code := exec(t, "predict", shipped20s)
	if code != 0 {
		t.Fatalf("text: exit %d", code)
	}
	for _, want := range []string{"calls per visit that vary (", "cart -> ext:valkey-cart 1.72 per visit (mean count)",
		"frontend -> cart 0.363 per visit (mixture of call shapes)", "shape:S:j", "shape:frontend:2"} {
		if !strings.Contains(text, want) {
			t.Errorf("the text report lacks %q", want)
		}
	}
	// a capture whose counts are fixed carries none of it
	plain, _, _ := exec(t, "predict", fixture, "--json")
	if strings.Contains(plain, "variable_calls") || strings.Contains(plain, "call_shapes") {
		t.Errorf("the example capture reports variable calls")
	}
	// the inferred topology, with its zero-time shape and stage nodes, runs under `model` and keeps the load
	topo, _, code := exec(t, "infer", shipped20s)
	if code != 0 {
		t.Fatalf("infer: exit %d", code)
	}
	path := filepath.Join(t.TempDir(), "topo.json")
	if err := os.WriteFile(path, []byte(topo), 0o644); err != nil {
		t.Fatal(err)
	}
	js, errw, code := exec(t, "model", path, "--rps", "27", "--patch", "cart=+1ms", "--json")
	if code != 0 {
		t.Fatalf("model: exit %d: %s", code, errw)
	}
	var m struct {
		Services []struct {
			Name      string   `json:"name"`
			RateRPS   float64  `json:"rate_rps"`
			ChangeMs  *float64 `json:"change_ms"`
			ModelMs   float64  `json:"model_ms"`
			AfterMs   *float64 `json:"after_ms"`
			ChangePct *float64 `json:"change_pct"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatal(err)
	}
	rate := map[string]float64{}
	change := map[string]float64{}
	for _, s := range m.Services {
		rate[s.Name] = s.RateRPS
		if s.ChangeMs != nil {
			change[s.Name] = *s.ChangeMs
		}
	}
	if r := rate["ext:valkey-cart"] / rate["cart"]; math.Abs(r-1.7246) > 1e-3 {
		t.Errorf("model: valkey per cart visit %.4f, want 1.7246", r)
	}
	// +1 ms on the cart's own time: the frontend's mean rises by 1 ms for each cart call of a visit, one after another
	// (0.363 per visit: 'cart' 8.0%, 'cart then cart' 13.9% of the visits, 'cart x2' 0.2%) and the cart calls of its checkouts
	// (a GetCart and an EmptyCart for each of the 4.5% of visits that check out: 0.09): 0.45 ms. (A front end whose
	// "cart then cart" read as "cart x2" would show a bit less.)
	if c := change["frontend"]; math.Abs(c-0.45) > 0.02 {
		t.Errorf("model: +1 ms on the cart changes the frontend by %.3f ms, want 0.45", c)
	}
}

// A chain a -> b -> c in which c's SERVER span is missing from every 50th trace (2%, below the limit of 5%): the capture is
// used, the warning names the edge, the row of c carries the visits it lost in the JSON and a "!" in the text with
// its legend, and the other rows are not marked. At 8% the refusal names the edge.
func TestLostSpansAreMarkedInTheReport(t *testing.T) {
	build := func(every int) (string, int) {
		rng := rand.New(rand.NewSource(21))
		var spans []tspan
		at, lost := 0.0, 0
		const n = 1500
		for i := 1; i <= n; i++ {
			at += rng.ExpFloat64() * 1000 / 30
			x := 1 + 3*rng.ExpFloat64()
			id := func(k int) string { return fmt.Sprintf("%016x", 10*i+k) }
			spans = append(spans,
				tspan{"a", "a-0", id(1), "", 2, at, at + x + 9, i, ""},
				tspan{"a", "a-0", id(2), id(1), 3, at + 0.5, at + x + 8, i, "b"},
				tspan{"b", "b-0", id(3), id(2), 2, at + 0.6, at + x + 7.9, i, ""},
				tspan{"b", "b-0", id(4), id(3), 3, at + 1, at + x + 6, i, "c"})
			if i%every == 0 {
				lost++
				continue
			}
			spans = append(spans, tspan{"c", "c-0", id(5), id(4), 2, at + 1.2, at + x + 5.8, i, ""})
		}
		return writeCapture(t, spans), lost
	}
	capture, lost := build(50)
	out, errw, code := exec(t, "predict", capture, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep struct {
		Broken   int64 `json:"broken_traces"`
		Services []struct {
			Name       string  `json:"name"`
			LostVisits int64   `json:"lost_visits"`
			LostShare  float64 `json:"lost_share"`
		} `json:"services"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Broken != int64(lost) {
		t.Errorf("%d broken traces, want %d", rep.Broken, lost)
	}
	for _, s := range rep.Services {
		switch {
		case s.Name == "c" && (s.LostVisits != int64(lost) || s.LostShare < 0.019 || s.LostShare > 0.021):
			t.Errorf("c: %d lost visits (share %.4f), want %d (2%%)", s.LostVisits, s.LostShare, lost)
		case s.Name != "c" && s.LostVisits != 0:
			t.Errorf("%s is marked with %d lost visits", s.Name, s.LostVisits)
		}
	}
	want := fmt.Sprintf("b -> c: %d of 1500 calls (2.0%%)", lost)
	if !slices.ContainsFunc(rep.Warnings, func(w string) bool { return strings.Contains(w, want) }) {
		t.Errorf("no warning with %q: %q", want, rep.Warnings)
	}
	text, _, _ := exec(t, "predict", capture)
	if !regexp.MustCompile(`(?m)^\s+c !\s`).MatchString(text) ||
		!strings.Contains(text, fmt.Sprintf("! lost spans, calls to this service had no span of it under them (visits lost, share of its visits: c %d (2.0%%))", lost)) {
		t.Errorf("the text report does not mark c:\n%s", text)
	}
	if regexp.MustCompile(`(?m)^\s+[ab] !`).MatchString(text) {
		t.Errorf("a or b is marked:\n%s", text)
	}
	_, errw, code = exec(t, "infer", capture)
	if code != 0 || !strings.Contains(errw, want) {
		t.Errorf("infer: exit %d, standard error %q", code, errw)
	}
	capture, lost = build(12)
	_, errw, code = exec(t, "predict", capture)
	want = fmt.Sprintf("have a call to a service with no span of that service under it (b -> c: %d of 1500 calls (8.3%%))", lost)
	if code != 1 || !strings.Contains(errw, want) {
		t.Errorf("8%%: exit %d, want a refusal with %q, got %q", code, want, errw)
	}
	// a capture that lost nothing has no mark and no field
	out, _, _ = exec(t, "predict", fixture, "--json")
	text, _, _ = exec(t, "predict", fixture)
	if strings.Contains(out, "lost_visits") || strings.Contains(text, "lost spans") {
		t.Errorf("the example capture reports lost spans")
	}
}

// Broken traces that are all of one kind of request: a calls d on 3% of the requests, d calls e, and e's span is missing on four
// of five of those. That is 2.4% of the traces (a warning, no refusal) and 80% of d's visits: d's row is marked, the legend
// says so and the JSON has the share. The other rows are not marked.
func TestVisitsInBrokenTracesAreMarkedInTheReport(t *testing.T) {
	rng := rand.New(rand.NewSource(33))
	var spans []tspan
	at := 0.0
	const n = 3000
	for i := 1; i <= n; i++ {
		at += rng.ExpFloat64() * 1000 / 40
		x := 1 + 2*rng.ExpFloat64()
		id := func(k int) string { return fmt.Sprintf("%016x", 20*i+k) }
		spans = append(spans,
			tspan{"a", "a-0", id(1), "", 2, at, at + x + 12, i, ""},
			tspan{"a", "a-0", id(2), id(1), 3, at + 0.5, at + x + 4, i, "b"},
			tspan{"b", "b-0", id(3), id(2), 2, at + 0.6, at + x + 3.9, i, ""})
		if i%30 != 0 {
			continue
		}
		spans = append(spans,
			tspan{"a", "a-0", id(4), id(1), 3, at + x + 5, at + x + 11, i, "d"},
			tspan{"d", "d-0", id(5), id(4), 2, at + x + 5.1, at + x + 10.9, i, ""},
			tspan{"d", "d-0", id(6), id(5), 3, at + x + 5.5, at + x + 10, i, "e"})
		if i%150 == 0 {
			spans = append(spans, tspan{"e", "e-0", id(7), id(6), 2, at + x + 5.6, at + x + 9.9, i, ""})
		}
	}
	capture := writeCapture(t, spans)
	out, errw, code := exec(t, "predict", capture, "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var rep struct {
		Broken   int64 `json:"broken_traces"`
		Services []struct {
			Name             string  `json:"name"`
			BrokenVisitShare float64 `json:"broken_visit_share"`
		} `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Broken != 80 {
		t.Errorf("%d broken traces, want 80", rep.Broken)
	}
	for _, s := range rep.Services {
		switch {
		case s.Name == "d" && (s.BrokenVisitShare < 0.79 || s.BrokenVisitShare > 0.81):
			t.Errorf("d: %.3f of its visits in broken traces, want 0.8", s.BrokenVisitShare)
		case s.Name != "d" && s.BrokenVisitShare != 0:
			t.Errorf("%s is marked (%.3f)", s.Name, s.BrokenVisitShare)
		}
	}
	text, _, _ := exec(t, "predict", capture)
	if !regexp.MustCompile(`(?m)^\s+d !\s`).MatchString(text) || !strings.Contains(text, "! many visits in traces that miss a span (share of its visits: d 80%)") ||
		!strings.Contains(text, "d: 80% of its 100 visits are in traces that miss a span") {
		t.Errorf("the text report does not mark d:\n%s", text)
	}
}

// The error for an unknown --patch service lists the services, not the zero-time nodes the inference adds for the call
// structure (stage:S:n, shape:S:n), which are not services.
func TestPatchErrorListsServicesNotStageNodes(t *testing.T) {
	_, errw, code := exec(t, "predict", shipped20s, "--patch", "nosuch=+1ms")
	if code == 0 || !strings.Contains(errw, "unknown service") || !strings.Contains(errw, "frontend") {
		t.Fatalf("exit %d: %s", code, errw)
	}
	if strings.Contains(errw, "stage:") || strings.Contains(errw, "shape:") {
		t.Errorf("the list of services holds the zero-time nodes: %s", errw)
	}
	// ... which a patch can still name, as before
	if _, errw, code := exec(t, "predict", shipped20s, "--patch", "frontend=+1ms"); code != 0 {
		t.Fatalf("patching frontend: exit %d: %s", code, errw)
	}
}

// --plain reads the wait that the network time of a call is cut against with the basic model too: the hops differ from the
// refined ones, a little where the callees are quiet, and only the hops. A callee the refined model takes batches into
// (frontend calls cart twice at once) has the batch's own queue in its refined wait: its hop may differ by up to 1%.
func TestPlainReadsTheHopWaitWithTheBasicModel(t *testing.T) {
	read := func(args ...string) []struct {
		Name string  `json:"name"`
		Hop  float64 `json:"hop"`
	} {
		out, errw, code := exec(t, append([]string{"infer", shipped20s}, args...)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errw)
		}
		var topo struct {
			Services []struct {
				Name string  `json:"name"`
				Hop  float64 `json:"hop"`
			} `json:"services"`
		}
		if err := json.Unmarshal([]byte(out), &topo); err != nil {
			t.Fatal(err)
		}
		return topo.Services
	}
	a, b := read(), read("--plain")
	if len(a) != len(b) {
		t.Fatalf("%d and %d services", len(a), len(b))
	}
	out, _, _ := exec(t, "infer", shipped20s)
	topo, err := model.ParseTopology([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	fed := model.BatchFed(topo)
	differ := 0
	for i := range a {
		tol := 1e-3
		if j := topo.Index(a[i].Name); j >= 0 && fed[j] {
			tol = 1e-2
		}
		if a[i].Name != b[i].Name || math.Abs(a[i].Hop-b[i].Hop) > tol*math.Max(a[i].Hop, 0.01) {
			t.Errorf("%s: hop %v refined, %v plain, want the same to 0.1%% (quiet callees)", a[i].Name, a[i].Hop, b[i].Hop)
		}
		if a[i].Hop != b[i].Hop {
			differ++
		}
	}
	if differ == 0 {
		t.Error("no hop differs between the refined and the basic model: --plain is not passed to the inference")
	}
}
