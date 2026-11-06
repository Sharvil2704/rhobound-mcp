// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/schematest"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server"
)

// engine stands in for the rhobound binary: check admits, fit writes a fit record, context prints a view naming the
// fit by its digest.
const engine = `#!/bin/sh
sub=$1; shift
case $sub in
check)   echo '{"schema":"rhobound.check/v1","verdict":"admitted","findings":[]}' ;;
fit)     out=""
         while [ $# -gt 0 ]; do if [ "$1" = "--out" ]; then out=$2; shift; fi; shift; done
         printf '%s' '{"schema":"rhobound.fit/v1","window":{"start":"2026-10-26T11:50:00Z","end":"2026-10-26T11:59:40Z","window_s":580}}' > "$out/fit.json" ;;
context) d=$(sha256sum "$2" | cut -c1-64)
         printf '{"assumptions":[],"entries":[],"findings":[],"fit_digest":"sha256:%s","schema":"rhobound-mcp.context/v1","scope":"mean-only/v1","services":[],"truncated":false}\n' "$d" ;;
*)       exit 2 ;;
esac
`

// install writes the engine and a configuration naming it, and returns the configuration's path.
func install(t *testing.T, sha string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "rhobound")
	if err := os.WriteFile(bin, []byte(engine), 0o755); err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		s := sha256.Sum256([]byte(engine))
		sha = hex.EncodeToString(s[:])
	}
	if err := os.MkdirAll(filepath.Join(dir, "captures", "loop"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"schema":"rhobound-mcp.config/v1","engine":{"path":"` + bin + `","sha256":"` + sha + `"},
  "state_dir":"state","captures":{"loop":"captures/loop"},"policies":{"default":"policy.json"}}`
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestVersionJSON: rhobound-mcp version --json prints one canonical JSON line with the keys of rhobound version --json:
// name, version, go, goarch, and vcs when the go command stamped the source; it validates against version.v1.json.
func TestVersionJSON(t *testing.T) {
	schemas, err := schematest.LoadRepo()
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--json"}, {"-json=true"}} {
		var buf bytes.Buffer
		if err := version(args, &buf); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		b := []byte(strings.TrimSuffix(out, "\n"))
		c, err := server.Canonical(b)
		var v map[string]any
		if err != nil || !bytes.Equal(c, b) || !strings.HasSuffix(out, "\n") || json.Unmarshal(b, &v) != nil {
			t.Fatalf("%v: %q", args, out)
		}
		if err := schemas.Validate("version.v1.json", "", b); err != nil {
			t.Errorf("%v: %v", args, err)
		}
		if v["name"] != "rhobound-mcp" || v["version"] != Version || !strings.HasPrefix(v["go"].(string), "go") ||
			v["goarch"] != runtime.GOARCH {
			t.Errorf("%v: %v", args, v)
		}
		for k := range v {
			if !slices.Contains([]string{"name", "version", "go", "goarch", "vcs"}, k) {
				t.Errorf("%v: key %q", args, k)
			}
		}
	}
	var buf bytes.Buffer
	if err := version([]string{"--json=false"}, &buf); err != nil || buf.String() != "rhobound-mcp "+Version+"\n" {
		t.Errorf("--json=false: %v %q", err, buf.String())
	}
	if err := version([]string{"--yaml"}, &buf); err == nil || !strings.Contains(err.Error(), "version [--json]") {
		t.Errorf("a bad flag: %v", err)
	}
	stamped := &debug.BuildInfo{GoVersion: "go1.25.0", Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}, {Key: "vcs.time", Value: "2026-10-27T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"}}}
	j, _ := json.Marshal(versionOf(stamped))
	b, err := server.Canonical(j)
	want := `{"go":"go1.25.0","goarch":"` + runtime.GOARCH + `","name":"rhobound-mcp","vcs":{"modified":true,` +
		`"revision":"0123456789abcdef0123456789abcdef01234567","time":"2026-10-27T00:00:00Z"},"version":"` + Version + `"}`
	if err != nil || string(b) != want {
		t.Errorf("stamped: %s %v", b, err)
	}
	if err := schemas.Validate("version.v1.json", "", b); err != nil {
		t.Errorf("stamped: %v", err)
	}
	for _, bi := range []*debug.BuildInfo{nil, {GoVersion: "go1.25.0"}} {
		if v := versionOf(bi); v.VCS != nil || v.Name != "rhobound-mcp" || v.Go == "" {
			t.Errorf("unstamped %v: %+v", bi, v)
		}
	}
}

func TestRunUsage(t *testing.T) {
	var buf bytes.Buffer
	code := run(context.Background(), nil, &buf)
	line, _, _ := strings.Cut(strings.TrimPrefix(buf.String(), "usage: rhobound-mcp MODE [flags]\nmodes: "), "\n")
	for _, m := range []string{"http", "stdio", "version"} {
		if code != 2 || !strings.Contains(", "+line+", ", ", "+m+", ") {
			t.Errorf("no mode: %d, mode %s not listed in %q", code, m, buf.String())
		}
	}
	buf.Reset()
	if code := run(context.Background(), []string{"serve"}, &buf); code != 2 {
		t.Errorf("unknown mode: %d", code)
	}
	buf.Reset()
	if code := run(context.Background(), []string{"stdio"}, &buf); code != 1 || !strings.Contains(buf.String(), "--config") {
		t.Errorf("stdio without config: %d %q", code, buf.String())
	}
	defer func() {
		if recover() == nil {
			t.Error("a mode registered twice did not panic")
		}
	}()
	register("stdio", runStdio)
}

func TestLambdaArgs(t *testing.T) {
	for _, c := range []struct {
		api, handler string
		want         []string
	}{
		{"127.0.0.1:9001", "lambda", []string{"lambda"}},
		{"127.0.0.1:9001", "fit-worker", []string{"fit-worker"}},
		{"127.0.0.1:9001", "stdio", nil},
		{"127.0.0.1:9001", "", nil},
		{"", "lambda", nil},
	} {
		env := map[string]string{"AWS_LAMBDA_RUNTIME_API": c.api, "_HANDLER": c.handler}
		got := lambdaArgs(func(k string) string { return env[k] })
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%+v: %q", c, got)
		}
	}
}

// TestStdioEndToEnd runs the stdio mode on an in-memory transport with the real runner and a stand-in engine:
// fit_start, job_status, then context on the new fit.
func TestStdioEndToEnd(t *testing.T) {
	cfg := install(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ct, st := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() { done <- serveStdio(ctx, []string{"--config", cfg}, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		return res.StructuredContent.(map[string]any)
	}
	job := call("fit_start", map[string]any{"capture": "loop"})["job_id"].(string)
	st2 := call("job_status", map[string]any{"job_id": job, "wait_s": 10})
	if st2["state"] != "completed" {
		t.Fatalf("job: %v", st2)
	}
	view := call("context", map[string]any{"fit_id": st2["fit_id"]})
	if !strings.HasPrefix(view["fit_digest"].(string), "sha256:") || view["fit_id"] != st2["fit_id"] {
		t.Errorf("context: %v", view)
	}
	if _, ok := view["fit_age_s"]; !ok {
		t.Errorf("context has no fit_age_s: %v", view)
	}
	cs.Close()
	cancel()
	if err := <-done; err != nil && ctx.Err() == nil {
		t.Errorf("stdio mode: %v", err)
	}
	// The state folder holds the server's records and the engine's own store, both private.
	for _, sub := range []string{"server", "engine", "work"} {
		info, err := os.Stat(filepath.Join(filepath.Dir(cfg), "state", sub))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Errorf("state/%s: %v %v", sub, info, err)
		}
	}
}

func TestStdioRefusesWrongEngine(t *testing.T) {
	cfg := install(t, strings.Repeat("0", 64))
	ct, st := mcp.NewInMemoryTransports()
	_ = ct
	err := serveStdio(context.Background(), []string{"--config", cfg}, st)
	if err == nil || !strings.Contains(err.Error(), "server.engine_mismatch") {
		t.Errorf("wrong engine: %v", err)
	}
}

// TestHTTPMode starts the HTTP mode on a loopback port with a static token, lists the tools, and shuts down.
func TestHTTPMode(t *testing.T) {
	cfg := install(t, "")
	tok, sum, err := auth.NewStaticToken()
	if err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(filepath.Dir(cfg), "tokens.json")
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := os.WriteFile(tokens, []byte(`{"tokens":[{"sha256":"`+sum+`","repo":"shop","expires":"`+exp+`"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- serveHTTP(ctx, []string{"--config", cfg, "--listen", "127.0.0.1:0", "--static-tokens", tokens,
			"--resource", "https://mcp.example.com/mcp"}, func(a string) { addr <- a })
	}()
	var base string
	select {
	case a := <-addr:
		base = "http://" + a
	case err := <-done:
		t.Fatal(err)
	}
	hc := &http.Client{Transport: bearer{tok}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cs.ListTools(ctx, nil)
	if err != nil || len(res.Tools) != 6 {
		t.Errorf("tools: %v %v", res, err)
	}
	cs.Close()
	for path, want := range map[string]int{"/healthz": 200, "/.well-known/oauth-protected-resource": 200} {
		resp, err := http.Get(base + path)
		if err != nil || resp.StatusCode != want {
			t.Errorf("%s: %v %v", path, resp, err)
		} else {
			resp.Body.Close()
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if err := serveHTTP(context.Background(), []string{"--config", cfg}, nil); err == nil || !strings.Contains(err.Error(), "needs") {
		t.Errorf("HTTP without auth: %v", err)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}
