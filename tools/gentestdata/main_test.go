// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// The loop capture checks with its count: kept share counted, 1, and no doubt but the digests' absence gone.
func TestLoop(t *testing.T) {
	out := filepath.Join(t.TempDir(), "loop")
	if err := run([]string{"loop", out}); err != nil {
		t.Fatal(err)
	}
	m, _, err := check.ManifestFromDir(out, check.Options{}.Limits)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "request-count.json"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := check.ParseRequestCount(b)
	if err != nil {
		t.Fatal(err)
	}
	rep, _, err := check.Run(m, check.Options{RequestCount: rc})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Verdict != contract.Admitted || rep.KeptShare != (contract.KeptShare{Value: 1, Source: contract.KeptCounted}) {
		t.Errorf("verdict %s, kept %+v, findings %+v", rep.Verdict, rep.KeptShare, rep.Findings)
	}
	if rc.Entries[0].Count != 3099 || rc.Source != "loadgen" || rc.Entries[0].Entry != "a" {
		t.Errorf("count %+v", rc)
	}
	if err := run([]string{"loop", out}); err == nil {
		t.Error("writing into a folder that is not empty")
	}
}

func TestFixturesTarget(t *testing.T) {
	out := t.TempDir()
	if err := run([]string{"fixtures", out}); err != nil {
		t.Fatal(err)
	}
	for _, f := range testgen.Fixtures() {
		if _, err := os.Stat(filepath.Join(out, f.Code)); (err == nil) != (f.Build != nil) {
			t.Errorf("%s: %v", f.Code, err)
		}
	}
	if run([]string{"nope", t.TempDir()}) == nil || run(nil) == nil {
		t.Error("a bad command line is accepted")
	}
}
