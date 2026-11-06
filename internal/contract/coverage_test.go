// SPDX-License-Identifier: Apache-2.0

package contract_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// Every code gets its test, from the one registry (DATA.md 17, 19):
//   - a code that an input file or a component at run time raises (trigger input or runtime) is named by at least one
//     unit test somewhere in the repository, in either Go module: as a constant of this package, as its string (the
//     mcp module cannot import this package), or in a JSON golden under a testdata folder that a test compares with;
//   - a code a capture triggers has an injected fixture (internal/testgen checks that), except the codes of the S3
//     window fetcher, whose fixtures are its store fake's injected defects: those are named by a test too;
//   - a test, or a JSON fixture under a testdata folder, that names a code the registry does not hold fails.
//
// Codes marked later or research are exempt until they are built.
func TestEveryCodeHasATest(t *testing.T) {
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	consts := codeConstants(t, filepath.Join(root, "internal", "contract", "codes.go"))
	domains := map[string]bool{}
	for _, c := range contract.All() {
		domains[contract.Domain(c.ID)] = true
	}
	notCodes := map[string]bool{}
	k := otlp.Keep()
	for _, l := range [][]string{k.Resource, k.Span, k.EventKeys, k.Event} {
		for _, key := range l {
			notCodes[key] = true
		}
	}
	fileLike := regexp.MustCompile(`\.(json|jsonl|gz|txt|md|ya?ml|sh|go|zst|pb|binpb|parquet|csv|html|sha256|v[0-9]+)$`)
	looksLikeCode := func(s string) bool {
		return contract.Pattern.MatchString(s) && domains[contract.Domain(s)] && !notCodes[s] && !fileLike.MatchString(s)
	}

	named := map[string][]string{}   // code -> test files that name it
	unknown := map[string][]string{} // unregistered code -> where
	jsonCode := regexp.MustCompile(`"code"\s*:\s*"([^"]+)"`)
	self := filepath.Join(root, "internal", "contract")
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			switch e.Name() {
			case ".git", "node_modules", "target", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case strings.HasSuffix(p, "_test.go") && filepath.Dir(p) != self:
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				t.Errorf("%s does not parse, so the codes it names are not seen: %v", rel, err)
				return nil
			}
			local := contractImportName(f)
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if id, ok := x.X.(*ast.Ident); ok && local != "" && id.Name == local {
						if code, ok := consts[x.Sel.Name]; ok {
							named[code] = append(named[code], rel)
						}
					}
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(x.Value)
					if err != nil {
						return true
					}
					if _, ok := contract.Lookup(s); ok {
						named[s] = append(named[s], rel)
					} else if looksLikeCode(s) {
						unknown[s] = append(unknown[s], fset.Position(x.Pos()).String())
					}
				}
				return true
			})
		case strings.HasSuffix(p, ".json") && strings.Contains(filepath.ToSlash(rel), "testdata/"):
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// a golden or fixture a test compares with names the codes it holds
			for _, m := range jsonCode.FindAllStringSubmatch(string(b), -1) {
				if _, ok := contract.Lookup(m[1]); ok {
					named[m[1]] = append(named[m[1]], rel)
				} else if contract.Pattern.MatchString(m[1]) {
					unknown[m[1]] = append(unknown[m[1]], rel)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range sortedKeys(unknown) {
		t.Errorf("%s is not a registered code, but %s names it", code, strings.Join(unknown[code], ", "))
	}

	fetch := map[string]bool{}
	for _, f := range testgen.Fixtures() {
		if f.Stage == testgen.StageFetch {
			fetch[f.Code] = true
		}
	}
	missing := map[string][]string{} // domain -> codes with no test
	for _, c := range contract.All() {
		if c.Status == contract.StatusLater || c.Status == contract.StatusResearch {
			continue
		}
		if (c.Trigger != contract.Capture || fetch[c.ID]) && len(named[c.ID]) == 0 {
			d := contract.Domain(c.ID)
			missing[d] = append(missing[d], c.ID)
		}
	}
	for _, d := range sortedKeys(missing) {
		t.Errorf("no test names these %s codes: %s", d, strings.Join(missing[d], ", "))
	}
}

// codeConstants maps the names of the code constants of codes.go to their values.
func codeConstants(t *testing.T, path string) map[string]string {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						if _, ok := contract.Lookup(s); ok {
							out[name.Name] = s
						}
					}
				}
			}
		}
	}
	if len(out) != len(contract.All()) {
		t.Fatalf("codes.go defines %d code constants for %d registered codes", len(out), len(contract.All()))
	}
	return out
}

// contractImportName is the name a file imports this package under ("" when it does not).
func contractImportName(f *ast.File) string {
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "github.com/Sharvil2704/rhobound-mcp/internal/contract" {
			if imp.Name != nil {
				return imp.Name.Name
			}
			return "contract"
		}
	}
	return ""
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
