// SPDX-License-Identifier: Apache-2.0

package depcheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

const module = "github.com/Sharvil2704/rhobound-mcp"

func goTool() string {
	if p, err := exec.LookPath("go"); err == nil {
		return p
	}
	return filepath.Join(runtime.GOROOT(), "bin", "go")
}

// go list -deps ./cmd/rhobound lists only the standard library and this module, never the nested mcp module.
func TestCLIIsStandardLibraryOnly(t *testing.T) {
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool(), "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./cmd/rhobound")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	n := 0
	for _, p := range strings.Fields(string(out)) {
		n++
		if p != module && (!strings.HasPrefix(p, module+"/") || strings.HasPrefix(p, module+"/mcp/") || p == module+"/mcp") {
			t.Errorf("rhobound depends on %s", p)
		}
	}
	if n == 0 {
		t.Error("go list printed no package of this module")
	}
}

// The root module's go.mod requires nothing and stays at go 1.23.
func TestModuleRequiresNothing(t *testing.T) {
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	goLine := ""
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && (f[0] == "require" || f[0] == "replace" || f[0] == "toolchain") {
			t.Errorf("go.mod: %s", l)
		}
		if len(f) > 0 && f[0] == "go" {
			goLine = strings.Join(f, " ")
		}
	}
	// CI takes its Go version from this line: a newer one would move it off Go 1.23 and let newer library calls in
	if goLine != "go 1.23" {
		t.Errorf("go.mod's go line is %q, want \"go 1.23\"", goLine)
	}
}
