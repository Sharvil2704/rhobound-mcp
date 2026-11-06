// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCopyMatchesPublished: the embedded registry is a byte copy of docs/mcp/data-contract.json, and every code in it
// has a level and a fix.
func TestCopyMatchesPublished(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(b), "module github.com/Sharvil2704/rhobound-mcp\n") {
			break
		}
		if filepath.Dir(dir) == dir {
			t.Fatal("no go.mod of github.com/Sharvil2704/rhobound-mcp above the test directory")
		}
		dir = filepath.Dir(dir)
	}
	want, err := os.ReadFile(filepath.Join(dir, "docs", "mcp", "data-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, want) {
		t.Error("data-contract.json differs from docs/mcp/data-contract.json; copy the published file here")
	}
	for code, c := range codes {
		if c.Fix == "" || c.Level != "refuse" && c.Level != "doubt" && c.Level != "note" {
			t.Errorf("%s: level %q, fix %q", code, c.Level, c.Fix)
		}
	}
	if c, ok := Lookup("trace.pod_churn"); !ok || c.Level != "doubt" || c.Fix == "" || !c.HasNumber("present_share") ||
		c.HasNumber("acct_x") {
		t.Errorf("trace.pod_churn: %+v %v", c, ok)
	}
	if _, ok := Lookup("nope.code"); ok {
		t.Error("an unregistered code is found")
	}
}
