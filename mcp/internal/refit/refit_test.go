// SPDX-License-Identifier: Apache-2.0

package refit

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// The loop fixture's window (testdata/mcp/loop/request-count.json).
const (
	loopStart = "2023-11-14T22:13:20.032968448Z"
	loopEnd   = "2023-11-14T22:15:20.018616064Z"
)

func TestScannerCountsStartsInWindow(t *testing.T) {
	dir := t.TempDir()
	line := func(starts ...int64) []byte {
		var spans []string
		for _, s := range starts {
			spans = append(spans, fmt.Sprintf(`{"traceId":"01","spanId":"02","kind":2,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"}`, s, s+1000))
		}
		return []byte(`{"resourceSpans":[{"resource":{},"scopeSpans":[{"spans":[` + strings.Join(spans, ",") + `]}]}]}` + "\n")
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(line(100, 200, 300))
	w.Write(line(250))
	w.Close()
	os.WriteFile(filepath.Join(dir, "a.json.gz"), gz.Bytes(), 0o600)
	os.WriteFile(filepath.Join(dir, "b.json"), line(199, 400), 0o600)
	os.Mkdir(filepath.Join(dir, "sub"), 0o700)
	n, err := Scanner{}.LateSpans(context.Background(), dir, time.Unix(0, 200), time.Unix(0, 400))
	if err != nil || n != 3 { // 200, 300, 250; not 100, 199 or 400
		t.Fatalf("late spans %d, %v; want 3", n, err)
	}
}

func TestLastWindow(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 500, time.UTC)
	s, e := LastWindow(config.Env{}, now, 10*time.Minute, 0)
	if !e.Equal(now.Add(-7*time.Minute).Truncate(time.Second)) || !s.Equal(e.Add(-10*time.Minute)) {
		t.Errorf("default margin: %v .. %v", s, e)
	}
	s, e = LastWindow(config.Env{RetryS: 60, BatchS: 10, SDKDelayS: 5}, now, time.Hour, 0)
	if !e.Equal(now.Add(-75*time.Second).Truncate(time.Second)) || !s.Equal(e.Add(-time.Hour)) {
		t.Errorf("configured margin: %v .. %v", s, e)
	}
	if _, e := LastWindow(config.Env{}, now, time.Minute, 30*time.Second); !e.Equal(now.Add(-30 * time.Second).Truncate(time.Second)) {
		t.Errorf("explicit settle: %v", e)
	}
}

// engine builds the real CLI once per test binary.
var engine struct {
	once sync.Once
	bin  string
	err  error
}

func realEngine(t *testing.T) string {
	t.Helper()
	engine.once.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			engine.err = errors.New("no go on PATH")
			return
		}
		root, err := repoRoot()
		if err != nil {
			engine.err = err
			return
		}
		dir, err := os.MkdirTemp("", "rhobound-engine-*")
		if err != nil {
			engine.err = err
			return
		}
		engine.bin = filepath.Join(dir, "rhobound")
		cmd := exec.Command(goBin, "build", "-o", engine.bin, "./cmd/rhobound")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			engine.err = fmt.Errorf("go build ./cmd/rhobound: %v\n%s", err, out)
		}
	})
	if engine.err != nil {
		t.Skip(engine.err.Error())
	}
	return engine.bin
}

// repoRoot is the root module's folder: the go.mod above this package that declares the root module.
func repoRoot() (string, error) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/Sharvil2704/rhobound-mcp\n") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no root go.mod above the test")
		}
		dir = parent
	}
}

// layoutLoop writes the loop fixture's spans into root as the S3 exporter would: one gzip object per minute
// partition and batch, each span in the partition of its start minute.
func layoutLoop(t *testing.T, root string) {
	t.Helper()
	r, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(r, "testdata", "mcp", "loop", "traces")
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	byMinute := map[int64][]json.RawMessage{}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(e.Name(), ".gz") {
			gz, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			out.ReadFrom(gz)
			b = out.Bytes()
		}
		for _, line := range bytes.Split(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var req struct {
				ResourceSpans []struct {
					Resource   json.RawMessage `json:"resource"`
					ScopeSpans []struct {
						Scope json.RawMessage   `json:"scope,omitempty"`
						Spans []json.RawMessage `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			if err := json.Unmarshal(line, &req); err != nil {
				t.Fatal(err)
			}
			for _, rs := range req.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					groups := map[int64][]json.RawMessage{}
					for _, sp := range ss.Spans {
						var s struct {
							Start string `json:"startTimeUnixNano"`
						}
						json.Unmarshal(sp, &s)
						ns, _ := strconv.ParseInt(s.Start, 10, 64)
						m := ns / int64(time.Minute)
						groups[m] = append(groups[m], sp)
					}
					for m, spans := range groups {
						d := map[string]any{"resource": rs.Resource, "scopeSpans": []map[string]any{{"spans": spans}}}
						if len(ss.Scope) > 0 {
							d["scopeSpans"].([]map[string]any)[0]["scope"] = ss.Scope
						}
						b, _ := json.Marshal(d)
						byMinute[m] = append(byMinute[m], b)
					}
				}
			}
		}
	}
	for m, rss := range byMinute {
		tm := time.Unix(m*60, 0).UTC()
		part := filepath.Join(root, filepath.FromSlash(objstore.Partition(tm)))
		if err := os.MkdirAll(part, 0o700); err != nil {
			t.Fatal(err)
		}
		const k = 6
		for j := 0; j < k; j++ {
			var chunk []string
			for i := j; i < len(rss); i += k {
				chunk = append(chunk, string(rss[i]))
			}
			if len(chunk) == 0 {
				continue
			}
			var gz bytes.Buffer
			w := gzip.NewWriter(&gz)
			w.Write([]byte(`{"resourceSpans":[` + strings.Join(chunk, ",") + `]}` + "\n"))
			w.Close()
			name := fmt.Sprintf("traces_%s-%03d.json.gz", tm.Format("1504"), j)
			if err := os.WriteFile(filepath.Join(part, name), gz.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRunMakesTheBaselineFromADirStore(t *testing.T) {
	bin := realEngine(t)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	layoutLoop(t, store)
	b, _ := os.ReadFile(bin)
	sum := state.Digest(b)
	eng, err := runner.NewExec(bin, strings.TrimPrefix(sum, "sha256:"), 2*time.Minute, []string{"RHOBOUND_STATE=" + filepath.Join(root, "engine")})
	if err != nil {
		t.Fatal(err)
	}
	eng.TempRoot = filepath.Join(root, "work")
	os.MkdirAll(eng.TempRoot, 0o700)
	st, err := state.NewDir(filepath.Join(root, "server"), nil)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339Nano, loopStart)
	end, _ := time.Parse(time.RFC3339Nano, loopEnd)
	r, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(r, "testdata", "mcp", "loop", "request-count.json"))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Env: "loop", Config: config.Env{Store: "dir", Root: store, BatchS: 10, RetryS: 60, SDKDelayS: 5},
		Start: start, End: end, RequestCount: count, Runner: eng, State: st, Work: filepath.Join(root, "work"),
		Keep: filepath.Join(root, "kept")}
	ctx := context.Background()
	res, err := Run(ctx, opt)
	if err != nil {
		t.Fatalf("refit: %v", err)
	}
	if res.Verdict != "admitted" || !strings.HasPrefix(res.Fit, "sha256:") || res.Files != 18 || res.Previous != "" || len(res.Findings.Check) != 0 {
		t.Errorf("result: %+v", res)
	}
	if d, err := st.Baseline(ctx, "loop"); err != nil || d != res.Fit {
		t.Errorf("baseline %q, %v; want %s", d, err, res.Fit)
	}
	if fb, err := st.GetArtifact(ctx, res.Fit); err != nil || state.Digest(fb) != res.Fit {
		t.Errorf("artifact: %v", err)
	}
	// the kept fit names the environment, not a folder, and records the cut's manifest digest and the counted share
	kept, err := os.ReadFile(filepath.Join(root, "kept", "fit", "fit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fit struct {
		Capture   string `json:"capture"`
		Manifest  string `json:"manifest_digest"`
		KeptShare struct {
			Source string  `json:"source"`
			Value  float64 `json:"value"`
		} `json:"kept_share"`
	}
	if err := json.Unmarshal(kept, &fit); err != nil {
		t.Fatal(err)
	}
	if fit.Capture != "loop" || fit.Manifest != res.Manifest || fit.KeptShare.Source != "counted" || fit.KeptShare.Value != 1 {
		t.Errorf("fit record: %+v (cut manifest %s)", fit, res.Manifest)
	}

	// again: the previous baseline is reported and the pointer moves to the same digest
	res2, err := Run(ctx, opt)
	if err != nil || res2.Previous != res.Fit || res2.Fit != res.Fit {
		t.Errorf("second refit: %+v, %v", res2, err)
	}

	// without the count: admitted with the sampling doubt; the baseline still moves
	opt.RequestCount, opt.Keep = nil, ""
	res3, err := Run(ctx, opt)
	if err != nil || res3.Verdict != "admitted-with-doubts" {
		t.Fatalf("without a count: %+v, %v", res3, err)
	}
	var codes []string
	for _, raw := range res3.Findings.Check {
		var f struct{ Code string }
		json.Unmarshal(raw, &f)
		codes = append(codes, f.Code)
	}
	sort.Strings(codes)
	if strings.Join(codes, ",") != "trace.sampling_unverified" {
		t.Errorf("findings without a count: %v", codes)
	}

	// a window with no objects is a coded refusal from the fetcher, and the baseline stays
	opt.Start, opt.End = start.Add(24*time.Hour), end.Add(24*time.Hour)
	_, err = Run(ctx, opt)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Step != "fetch" || !bytes.Contains(refusal.Refusal, []byte(`"code":"trace.too_few_entries"`)) {
		t.Fatalf("empty window: %v", err)
	}
	if d, _ := st.Baseline(ctx, "loop"); d != res3.Fit {
		t.Errorf("the baseline moved on a refusal: %s", d)
	}

	// the work folder holds nothing of the runs
	ents, _ := os.ReadDir(filepath.Join(root, "work"))
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "refit-") {
			t.Errorf("work folder left behind: %s", e.Name())
		}
	}
}

func TestStoreForAndOptions(t *testing.T) {
	if _, err := StoreFor(config.Env{Store: "s3", Prefix: "env=prod/"}); err == nil {
		t.Error("s3 should not be in this build")
	}
	if s, err := StoreFor(config.Env{Store: "dir", Root: "/x"}); err != nil || s.(objstore.Dir).Root != "/x" {
		t.Errorf("dir: %v %v", s, err)
	}
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Error("empty options should fail")
	}
	if got := workersFlag(map[string]int{"b": 2, "a": 1}); got != "--workers=a=1,b=2" {
		t.Errorf("workers flag %q", got)
	}
}
