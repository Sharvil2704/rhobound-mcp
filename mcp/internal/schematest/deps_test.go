// SPDX-License-Identifier: Apache-2.0

package schematest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const (
	mcpModule = "github.com/Sharvil2704/rhobound-mcp/mcp"
	sdk       = "github.com/modelcontextprotocol/go-sdk"
)

// goIn returns the mcp module's folder and a function that runs the go command there.
func goIn(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "mcp")
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	return dir, func(args ...string) string {
		t.Helper()
		cmd := exec.Command(goTool, args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go %v: %v", args, err)
		}
		return string(out)
	}
}

// The mcp module depends on the official MCP Go SDK and its transitive dependencies only (docs/mcp/ARCHITECTURE.md 3,
// docs/BUILD.md): go.mod requires the SDK and jsonschema-go directly, and every module a
// package of this module, its tests included, is built from is the SDK or one the SDK's module graph reaches. go.mod
// holds no replace, exclude or retract directive: a replace pointing at a fork keeps the module's path and version in
// `go list` and `go mod graph`, so the checks below would not see it.
func TestModuleDependencies(t *testing.T) {
	dir, run := goIn(t)

	b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	var direct []string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 0 && (f[0] == "replace" || f[0] == "exclude" || f[0] == "retract") {
			t.Errorf("go.mod: %s", l)
		}
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l), "require "))
		if len(f) == 2 && strings.Contains(f[0], ".") && !strings.Contains(l, "// indirect") {
			direct = append(direct, f[0]+"@"+f[1])
		}
	}
	sort.Strings(direct)
	if strings.Join(direct, " ") != "github.com/google/jsonschema-go@v0.4.3 github.com/modelcontextprotocol/go-sdk@v1.8.0" {
		t.Errorf("direct requirements %v", direct)
	}

	// the modules the SDK's graph reaches
	edges := map[string][]string{}
	for _, l := range strings.Split(run("mod", "graph"), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			edges[f[0]] = append(edges[f[0]], f[1])
		}
	}
	reach := map[string]bool{}
	var walk func(string)
	walk = func(m string) {
		path, _, _ := strings.Cut(m, "@")
		if reach[m] {
			return
		}
		reach[m], reach[path] = true, true
		for _, n := range edges[m] {
			walk(n)
		}
	}
	for _, n := range edges[mcpModule] {
		if strings.HasPrefix(n, sdk+"@") {
			walk(n)
		}
	}
	for _, m := range strings.Fields(run("list", "-deps", "-test", "-f", "{{with .Module}}{{.Path}}{{end}}", "./...")) {
		if m != mcpModule && !reach[m] {
			t.Errorf("a package is built from %s, which the SDK's module graph does not reach", m)
		}
	}
	for _, n := range edges[mcpModule] {
		if path, _, _ := strings.Cut(n, "@"); path != "go" && path != "toolchain" && !reach[n] {
			t.Errorf("go.mod requires %s, which the SDK's module graph does not reach at that version", n)
		}
	}
}

// Every module a package of this module is built from, its tests included, is listed at its version, with its
// licence, in mcp/THIRD-PARTY.md, which NOTICE points to.
func TestThirdPartyListed(t *testing.T) {
	dir, run := goIn(t)
	b, err := os.ReadFile(filepath.Join(dir, "THIRD-PARTY.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Split(l, "|"); len(f) > 3 {
			rows[strings.TrimSpace(f[1])+"@"+strings.TrimSpace(f[2])] = true
		}
	}
	for _, m := range strings.Split(run("list", "-deps", "-test", "-f", "{{with .Module}}{{.Path}}@{{.Version}}{{end}}", "./..."), "\n") {
		if m = strings.TrimSpace(m); m != "" && m != mcpModule+"@" && !rows[m] {
			t.Errorf("mcp/THIRD-PARTY.md does not list %s", m)
		}
	}
}
