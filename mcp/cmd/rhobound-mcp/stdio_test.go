// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// repoRoot walks up from the test's folder to the go.mod of the CLI's module.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(b))
			for sc.Scan() {
				if f := strings.Fields(sc.Text()); len(f) == 2 && f[0] == "module" && f[1] == "github.com/Sharvil2704/rhobound-mcp" {
					return dir
				}
			}
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod of the CLI's module above the test's folder")
		}
		dir = up
	}
}

// build runs go build in dir and returns the binary's path.
func build(t *testing.T, goBin, dir, pkg, out string) string {
	t.Helper()
	cmd := exec.Command(goBin, "build", "-o", out, pkg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// TestStdioRealCLI runs `rhobound-mcp stdio` as a child process, the way a coding agent's host starts it, on the real
// CLI and the loop fixture, in both protocol eras through the SDK client: fit_start with pinned workers, job_status,
// context, then a declared preview. A real process reads the real clock, so the fit of the 2023 capture is stale and
// the preview is inconclusive with fit_stale.
func TestStdioRealCLI(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	root := repoRoot(t)
	loop := filepath.Join(root, "testdata", "mcp", "loop")
	if _, err := os.Stat(filepath.Join(loop, "traces")); err != nil {
		t.Fatalf("the loop fixture: %v", err)
	}
	bin := t.TempDir()
	cli := build(t, goBin, root, "./cmd/rhobound", filepath.Join(bin, "rhobound"))
	server := build(t, goBin, ".", ".", filepath.Join(bin, "rhobound-mcp"))
	b, err := os.ReadFile(cli)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)

	for _, era := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(era, func(t *testing.T) {
			dir := t.TempDir()
			cfg, _ := json.Marshal(map[string]any{"schema": "rhobound-mcp.config/v1",
				"engine":    map[string]string{"path": cli, "sha256": hex.EncodeToString(sum[:])},
				"state_dir": "state", "captures": map[string]string{"loop": loop},
				"policies": map[string]string{"default": filepath.Join(loop, "policy.json")}})
			cfgPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			tr := &mcp.CommandTransport{Command: exec.Command(server, "stdio", "--config", cfgPath)}
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, tr,
				&mcp.ClientSessionOptions{ProtocolVersion: era})
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			if got := cs.InitializeResult().ProtocolVersion; got != era {
				t.Fatalf("negotiated %q, want %q", got, era)
			}
			call := func(name string, args map[string]any) map[string]any {
				t.Helper()
				res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
				if err != nil || res.IsError {
					t.Fatalf("%s: %v %+v", name, err, res)
				}
				return res.StructuredContent.(map[string]any)
			}
			job := call("fit_start", map[string]any{"capture": "loop", "workers": map[string]any{"a": 1, "b": 2, "c": 1}})
			st := call("job_status", map[string]any{"job_id": job["job_id"], "wait_s": 20})
			if st["state"] != "completed" {
				t.Fatalf("job: %v", st)
			}
			view := call("context", map[string]any{"fit_id": st["fit_id"]})
			if view["schema"] != "rhobound-mcp.context/v1" || view["fit_id"] != st["fit_id"] {
				t.Fatalf("context: %v", view)
			}
			pv := call("predict", map[string]any{"fit_id": st["fit_id"],
				"changes": []any{map[string]any{"op": "own_ms", "service": "b", "value": 0.4}}})
			p := pv["preview"].(map[string]any)
			stale := false
			for _, r := range p["rules"].([]any) {
				stale = stale || r.(map[string]any)["rule"] == "fit_stale"
			}
			if p["verdict"] != "inconclusive" || !stale {
				t.Errorf("preview at the real clock: %v", p)
			}
			// the state folder holds the server's records and the engine's store, private to the user
			for _, sub := range []string{"server", "engine", "work"} {
				if info, err := os.Stat(filepath.Join(dir, "state", sub)); err != nil || info.Mode().Perm() != 0o700 {
					t.Errorf("state/%s: %v", sub, err)
				}
			}
		})
	}
}
