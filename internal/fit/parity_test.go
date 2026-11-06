// SPDX-License-Identifier: Apache-2.0

package fit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// captures are the repository's test captures, as the golden sweep of 1a2df09 runs them.
var captures = []string{
	"testdata/capture-chain",
	"internal/infer/testdata/counts/shipped-20s",
	"internal/infer/testdata/hops/browse-45s",
	"internal/infer/testdata/hops/cache-30s",
	"internal/infer/testdata/nested/cart-client-in-client",
	"internal/infer/testdata/nested/flagd-server-in-server",
	"internal/infer/testdata/nested/fraud-detection-consumer-in-consumer",
	"internal/infer/testdata/nested/frontend-server-in-server",
}

// reports solves an inference at its observed load as `rhobound predict` does and returns the v1 report's JSON and
// text.
func reports(t *testing.T, capture string, inf *infer.Inferred, kept float64, plain bool) (string, string) {
	t.Helper()
	cfg := model.Default
	if plain {
		cfg = model.Plain
	}
	base, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rep := report.Build(capture, inf, kept, base, nil, nil)
	var j, x bytes.Buffer
	if err := rep.WriteJSON(&j); err != nil {
		t.Fatal(err)
	}
	if err := rep.WriteText(&x); err != nil {
		t.Fatal(err)
	}
	return j.String(), x.String()
}

// A fit is lossless (ARCHITECTURE.md 4.1, fit parity at the package level): for every test capture and a set of
// options, the inference that `predict CAPTURE` makes (the reader's ReadDir, then infer) and the one a fit record
// gives back (check's data, Build, Marshal, Unmarshal, Inferred) write the same report, JSON and text, and the same
// record bytes. The comparison is made in the process, so it holds on any GOARCH.
func TestFitIsLossless(t *testing.T) {
	root := filepath.Join("..", "..")
	type opts struct {
		name    string
		workers map[string]int
		plain   bool
		kept    *float64
	}
	half := 0.5
	for i, capture := range captures {
		cases := []opts{{name: "default"}}
		switch i {
		case 0:
			cases = append(cases, opts{name: "plain", plain: true}, opts{name: "kept-0.5", kept: &half},
				opts{name: "workers", workers: map[string]int{"a": 1, "b": 2, "c": 1}})
		case 2:
			cases = append(cases, opts{name: "plain", plain: true})
		}
		for _, o := range cases {
			t.Run(filepath.Base(capture)+"/"+o.name, func(t *testing.T) {
				dir := filepath.Join(root, capture)
				kept := 1.0
				if o.kept != nil {
					kept = *o.kept
				}
				d1, err := otlp.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				inf1, err1 := infer.Run(d1, infer.Options{KeptShare: kept, Workers: o.workers, Plain: o.plain,
					Estimator: infer.EstimatorFIFO})

				m, _, err := check.ManifestFromDir(dir, otlp.Limits{})
				if err != nil {
					t.Fatal(err)
				}
				rep, d2, err := check.Run(m, check.Options{DeclaredKeptShare: o.kept})
				if err != nil || d2 == nil {
					t.Fatalf("check: %v (%v)", err, rep)
				}
				in := fit.Input{Capture: capture, Data: d2, Manifest: m, Check: rep,
					Options: fit.Options{Workers: o.workers, Plain: o.plain, KeptShare: o.kept, Estimator: infer.EstimatorFIFO}}
				rec, err2 := fit.Build(in, fit.NewTool("test"))
				if err1 != nil || err2 != nil {
					// a capture the inference refuses is refused by both, with the same message
					var r1, r2 *infer.Refusal
					if err1 == nil || err2 == nil || err1.Error() != err2.Error() || !errors.As(err1, &r1) || !errors.As(err2, &r2) ||
						r1.Code != r2.Code {
						t.Fatalf("predict: %v\nfit: %v", err1, err2)
					}
					return
				}
				if rec.KeptShare.Value != kept {
					t.Fatalf("the fit's kept share is %+v, want %v", rec.KeptShare, kept)
				}
				b, err := rec.Marshal()
				if err != nil {
					t.Fatal(err)
				}
				back, err := fit.Unmarshal(b)
				if err != nil {
					t.Fatal(err)
				}
				b2, err := back.Marshal()
				if err != nil || !bytes.Equal(b, b2) || back.Digest() != rec.Digest() {
					t.Fatalf("the record's bytes are not stable (%v)", err)
				}
				// the same record serialized again (indented, a trailing newline, a space) is not the bytes its
				// digest is of: refused as a record this version did not write
				var indented bytes.Buffer
				if err := json.Indent(&indented, b, "", "  "); err != nil {
					t.Fatal(err)
				}
				for _, again := range [][]byte{indented.Bytes(), append(append([]byte{}, b...), '\n'),
					append([]byte{b[0], ' '}, b[1:]...)} {
					if _, err := fit.Unmarshal(again); !errors.Is(err, fit.ErrVersion) {
						t.Errorf("a record serialized again is read: %v", err)
					}
				}
				inf2, err := back.Inferred()
				if err != nil {
					t.Fatal(err)
				}
				// the inference of predict CAPTURE, recorded with the same summary, is the same record
				same, err := fit.FromInferred(in, inf1, fit.NewTool("test"))
				if err != nil {
					t.Fatal(err)
				}
				if sb, _ := same.Marshal(); !bytes.Equal(sb, b) {
					t.Errorf("the inference from the reader and from check's data differ")
				}
				j1, x1 := reports(t, capture, inf1, kept, o.plain)
				j2, x2 := reports(t, back.Capture, inf2, back.KeptShare.Value, back.Options.Plain)
				if j1 != j2 {
					t.Errorf("JSON reports differ:\n%s\n%s", j1, j2)
				}
				if x1 != x2 {
					t.Errorf("text reports differ:\n%s\n%s", x1, x2)
				}
				// each call gives a fresh copy
				inf3, _ := back.Inferred()
				inf3.Topology.Services[0].S = -1
				inf3.Stats[0].Name = "changed"
				if inf4, _ := back.Inferred(); inf4.Topology.Services[0].S == -1 || inf4.Stats[0].Name == "changed" {
					t.Error("Inferred shares memory between calls")
				}
			})
		}
	}
}
