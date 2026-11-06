// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// The tests in this file and in scripted_test.go run the real rhobound CLI, built from this repository once per test
// binary (the first test that needs it builds it; TestMain removes it). The repository root is the first folder above
// this one whose go.mod declares the CLI's module.

const cliModule = "github.com/Sharvil2704/rhobound-mcp"

var real struct {
	once sync.Once
	dir  string // the build folder, removed by TestMain
	root string // the repository
	bin  string // the rhobound binary
	sha  string // its sha256
	skip string // why the real CLI cannot be used here
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if real.dir != "" {
		os.RemoveAll(real.dir)
	}
	os.Exit(code)
}

// repoRoot walks up from the working folder to the go.mod of the CLI's module.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && modulePath(b) == cliModule {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("no go.mod of " + cliModule + " above the test's folder")
		}
		dir = up
	}
}

// modulePath is the module path a go.mod declares.
func modulePath(gomod []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(gomod))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "module" {
			return strings.Trim(f[1], `"`)
		}
	}
	return ""
}

// realCLI builds the CLI, once. A machine without the go command skips the real-CLI tests; a build failure fails
// them.
func realCLI(t *testing.T) {
	t.Helper()
	real.once.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			real.skip = "the go command is not on PATH, so the CLI cannot be built"
			return
		}
		if real.root, real.err = repoRoot(); real.err != nil {
			return
		}
		if real.dir, real.err = os.MkdirTemp("", "rhobound-cli-*"); real.err != nil {
			return
		}
		real.bin = filepath.Join(real.dir, "rhobound")
		cmd := exec.Command(goBin, "build", "-o", real.bin, "./cmd/rhobound")
		cmd.Dir = real.root
		cmd.Env = append(os.Environ(), "GOWORK=off")
		if out, err := cmd.CombinedOutput(); err != nil {
			real.err = errors.New("go build ./cmd/rhobound: " + err.Error() + "\n" + string(out))
			return
		}
		b, err := os.ReadFile(real.bin)
		if err != nil {
			real.err = err
			return
		}
		sum := sha256.Sum256(b)
		real.sha = hex.EncodeToString(sum[:])
	})
	if real.skip != "" {
		t.Skip(real.skip)
	}
	if real.err != nil {
		t.Fatal(real.err)
	}
}

// recorder is a Runner that records every call with what the engine printed, for the parity checks.
type recorder struct {
	runner.Runner
	mu    sync.Mutex
	calls []recorded
}

type recorded struct {
	sub    string
	args   []string
	files  map[string][]byte
	stdout []byte
	exit   int
}

func (r *recorder) Run(ctx context.Context, sub string, args []string, files map[string][]byte) ([]byte, int, error) {
	out, exit, err := r.Runner.Run(ctx, sub, args, files)
	if err == nil {
		c := recorded{sub: sub, args: append([]string(nil), args...), files: map[string][]byte{}, stdout: out, exit: exit}
		for k, v := range files {
			c.files[k] = append([]byte(nil), v...)
		}
		r.mu.Lock()
		r.calls = append(r.calls, c)
		r.mu.Unlock()
	}
	return out, exit, err
}

// last returns the last call of a subcommand.
func (r *recorder) last(t *testing.T, sub string) recorded {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i].sub == sub {
			return r.calls[i]
		}
	}
	t.Fatalf("no %s call recorded", sub)
	return recorded{}
}

// realHarness is a server on the real CLI, a memory store and a pinned clock, with a client session.
type realHarness struct {
	*harness
	engine *runner.Exec
	rec    *recorder
	now    time.Time
}

// realServer starts a server whose configuration names the given captures and the policy, on the real CLI. The clock
// reads now; the engine's record store is private to the server.
func realServer(t *testing.T, captures map[string]string, policy []byte, now time.Time, protocol string) *realHarness {
	t.Helper()
	realCLI(t)
	dir := t.TempDir()
	for _, sub := range []string{"engine", "work"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(map[string]any{"schema": config.Schema,
		"engine":    map[string]string{"path": real.bin, "sha256": real.sha},
		"state_dir": "state", "captures": captures, "policies": map[string]string{"default": "policy.json"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := runner.NewExec(real.bin, real.sha, 2*time.Minute,
		[]string{"RHOBOUND_STATE=" + filepath.Join(dir, "engine")})
	if err != nil {
		t.Fatal(err)
	}
	engine.TempRoot = filepath.Join(dir, "work")
	rec := &recorder{Runner: engine}
	clock := func() time.Time { return now }
	h := &harness{t: t, store: state.NewMem(clock)}
	who := "local:alice"
	h.who = &who
	srv, err := New(Options{Config: c, Runner: rec, Store: h.store, Now: clock, LocalCaptures: true,
		WorkDir: filepath.Join(dir, "work"),
		Identity: func(context.Context) (auth.Identity, bool) {
			return auth.Identity{Principal: *h.who, Scopes: []string{auth.ScopeRead, auth.ScopeFit}}, true
		}})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	t.Cleanup(srv.WaitJobs)
	h.cs = connect(t, srv, protocol)
	return &realHarness{harness: h, engine: engine, rec: rec, now: now}
}

// twin runs the CLI twin of the last call of sub again, on its own, with the same arguments and input files, and
// returns what it printed (one canonical JSON document and a newline).
func (h *realHarness) twin(sub string) []byte {
	h.t.Helper()
	c := h.rec.last(h.t, sub)
	out, exit, err := h.engine.Run(context.Background(), c.sub, c.args, c.files)
	if err != nil || exit != c.exit {
		h.t.Fatalf("twin %s: exit %d (the server's call: %d), %v", sub, exit, c.exit, err)
	}
	if !bytes.Equal(out, c.stdout) {
		h.t.Errorf("twin %s printed other bytes on a second run:\n%s\n%s", sub, out, c.stdout)
	}
	return out
}

// parity checks a tool result against its CLI twin (ARCHITECTURE.md 5.5): the result without the server keys,
// re-serialized canonically, is byte for byte what the twin prints with --json. The result is read twice: from the text
// block, which keeps the bytes, and from structuredContent as the client decoded it.
func (h *realHarness) parity(tool, sub string, body []byte, sc any) {
	h.t.Helper()
	cli := bytes.TrimSuffix(h.twin(sub), []byte("\n"))
	if canon, err := Canonical(cli); err != nil || !bytes.Equal(canon, cli) {
		h.t.Errorf("%s: the twin's output is not canonical JSON: %v", tool, err)
	}
	if got := strip(h.t, body); got != string(cli) {
		h.t.Errorf("%s: parity (text) fails:\n got %s\nwant %s", tool, got, cli)
	}
	b, err := json.Marshal(sc)
	if err != nil {
		h.t.Fatal(err)
	}
	if got := strip(h.t, b); got != string(cli) {
		h.t.Errorf("%s: parity (structuredContent) fails:\n got %s\nwant %s", tool, got, cli)
	}
}
