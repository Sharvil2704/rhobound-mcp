// SPDX-License-Identifier: Apache-2.0

package schemas

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's directory to the first go.mod of the root module.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/Sharvil2704/rhobound-mcp\n") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod of github.com/Sharvil2704/rhobound-mcp above the test directory")
		}
		dir = parent
	}
}

// TestCopiesMatchPublished: every embedded file is a byte copy of the published schema of the same name.
func TestCopiesMatchPublished(t *testing.T) {
	pub := filepath.Join(repoRoot(t), "docs", "mcp", "schemas")
	names := Names()
	if len(names) == 0 {
		t.Fatal("no embedded schemas")
	}
	for _, n := range names {
		want, err := os.ReadFile(filepath.Join(pub, n))
		if err != nil {
			t.Errorf("%s: not published: %v", n, err)
			continue
		}
		got, _ := File(n)
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from docs/mcp/schemas/%s; copy the published file here", n, n)
		}
	}
}

// TestBundlesSelfContained: each tool input and the config schema bundle into a document with only local
// references, which resolves without a loader.
func TestBundlesSelfContained(t *testing.T) {
	cases := []struct{ file, ptr string }{
		{"context.v1.json", "/$defs/input"},
		{"budget.v1.json", "/$defs/input"},
		{"prediction.v1.json", "/$defs/input"},
		{"explain.v1.json", "/$defs/input"},
		{"fit-start.v1.json", "/$defs/input"},
		{"job-status.v1.json", "/$defs/input"},
		{"context.v1.json", "/$defs/output"},
		{"prediction.v1.json", "/$defs/output"},
		{"config.v1.json", ""},
	}
	for _, c := range cases {
		b, err := Bundle(c.file, c.ptr)
		if err != nil {
			t.Errorf("%s%s: %v", c.file, c.ptr, err)
			continue
		}
		if bytes.Contains(b, []byte(".json#")) || bytes.Contains(b, []byte(`"$id"`)) {
			t.Errorf("%s%s: bundle still refers to a file: %s", c.file, c.ptr, b)
		}
		if _, err := Resolve(b); err != nil {
			t.Errorf("%s%s: does not resolve: %v", c.file, c.ptr, err)
		}
		again, _ := Bundle(c.file, c.ptr)
		if !bytes.Equal(b, again) {
			t.Errorf("%s%s: bundle bytes are not stable", c.file, c.ptr)
		}
	}
}

// TestBundleValidates checks the bundled schemas accept and reject what the published ones do.
func TestBundleValidates(t *testing.T) {
	const fit = "fit_01JD3Q5X7Y8Z9A0B1C2D3E4F5G"
	cases := []struct {
		file string
		doc  string
		ok   bool
	}{
		{"context.v1.json", `{"fit_id":"` + fit + `"}`, true},
		{"context.v1.json", `{"capture":"loop","detail":"full"}`, true},
		{"context.v1.json", `{}`, false},
		{"context.v1.json", `{"fit_id":"` + fit + `","capture":"loop"}`, false},
		{"context.v1.json", `{"capture":"bad name"}`, false},
		{"context.v1.json", `{"fit_id":"fit_01JD3Q5X7Y8Z9A0B1C2D3E4F5I"}`, false}, // I is not Crockford
		{"context.v1.json", `{"capture":"loop","path":"/etc"}`, false},
		{"fit-start.v1.json", `{"capture":"loop","workers":{"a":1,"b":2,"c":1}}`, true},
		{"fit-start.v1.json", `{"capture":"loop","workers":{"a":0}}`, false},
		{"fit-start.v1.json", `{"ab":{"baseline":"old","candidate":"new"}}`, true},
		{"fit-start.v1.json", `{"ab":{"baseline":"old"}}`, false},
		{"prediction.v1.json", `{"fit_id":"` + fit + `","changes":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6}}]}`, true},
		{"prediction.v1.json", `{"fit_id":"` + fit + `","measured":[{"ab":"ab_01JD3Q5X7Y8Z9A0B1C2D3E4F5G"}]}`, true},
		{"prediction.v1.json", `{"fit_id":"` + fit + `"}`, false},
		{"prediction.v1.json", `{"fit_id":"` + fit + `","changes":[{"op":"own_ms","service":"b","value":1,"label":"measured-ab"}]}`, false},
		{"job-status.v1.json", `{"job_id":"job_01JD3Q5X7Y8Z9A0B1C2D3E4F5G","wait_s":21}`, false},
		{"explain.v1.json", `{"section":"scope"}`, true},
		{"explain.v1.json", `{"section":"scope","max_chars":100}`, false},
		{"budget.v1.json", `{"fit_id":"` + fit + `","kinds":["own_ms"]}`, true},
	}
	for _, c := range cases {
		b, err := Bundle(c.file, "/$defs/input")
		if err != nil {
			t.Fatal(err)
		}
		rs, err := Resolve(b)
		if err != nil {
			t.Fatal(err)
		}
		var v any
		if err := json.Unmarshal([]byte(c.doc), &v); err != nil {
			t.Fatal(err)
		}
		if err := rs.Validate(v); (err == nil) != c.ok {
			t.Errorf("%s %s: valid=%v, want %v (%v)", c.file, c.doc, err == nil, c.ok, err)
		}
	}
}

func TestBundleRejectsBadPointers(t *testing.T) {
	for _, ptr := range []string{"/properties/x", "/$defs/nope", "/$defs/a/b"} {
		if _, err := Bundle("context.v1.json", ptr); err == nil {
			t.Errorf("pointer %q: no error", ptr)
		}
	}
}
