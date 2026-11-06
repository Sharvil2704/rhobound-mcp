// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

func TestJSONRequested(t *testing.T) {
	for args, want := range map[string]bool{
		"x --json": true, "x -json": true, "--json=true x": true, "--json=1": true,
		"x": false, "--json=false": false, "-- --json": false, "--jsonx": false, "json": false,
	} {
		if got := jsonRequested(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}

// Every refusal rhobound-cli printed in English now carries its code with --json (the message unchanged on standard
// error): each fixture of a code the inference or the model raises makes predict refuse with that code, or, for a
// warning, puts it in the fit's findings.
func TestRefusalCodesOfTheFitAndPredictFixtures(t *testing.T) {
	t.Setenv("RHOBOUND_ESTIMATOR", "")
	for _, f := range testgen.Fixtures() {
		if f.Stage != testgen.StageFit && f.Stage != testgen.StagePredict || f.Code == contract.LoadBeyondValidated {
			continue // load.beyond_validated is the gate's: the report shows the utilization
		}
		t.Run(f.Code, func(t *testing.T) {
			cs, err := f.Build(baseCapture(t), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var opts []string
			if len(cs.Workers) > 0 {
				var ws []string
				for s, n := range cs.Workers {
					ws = append(ws, s+"="+itoa(n))
				}
				sort.Strings(ws)
				opts = append(opts, "--workers", strings.Join(ws, ","))
			}
			if cs.Plain {
				opts = append(opts, "--plain")
			}
			args := append([]string{"predict", cs.Dir, "--json"}, opts...)
			if cs.Patch != "" {
				args = append(args, "--patch", cs.Patch)
			}
			out, errw, code := exec(t, args...)
			if code == 1 {
				var doc report.RefusalDoc
				if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Error.Code != f.Code ||
					!strings.Contains(errw, doc.Error.Reason[:min(len(doc.Error.Reason), 80)]) {
					t.Errorf("%s: refusal %s (stderr %q)", f.What, out, errw)
				}
				return
			}
			// a warning: the fit names it
			dir := t.TempDir()
			if _, errw, code := exec(t, append([]string{"fit", cs.Dir, "--out", dir}, opts...)...); code != 0 {
				t.Fatalf("fit: exit %d: %s", code, errw)
			}
			b, _ := os.ReadFile(filepath.Join(dir, fit.File))
			rec, err := fit.Unmarshal(b)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, x := range rec.Findings {
				found = found || x.Code == f.Code
			}
			if !found {
				t.Errorf("%s: predict exit %d, and the fit's findings do not name it: %+v", f.What, code, rec.Findings)
			}
		})
	}
}
