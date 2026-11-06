// SPDX-License-Identifier: Apache-2.0

package engine

// Regression tests for the Lean lane ChangeBand findings CB-3 and CB-4 (maze-kit lean/Rbproofs/Lanes/ChangeBand.lean),
// written by the workflow's verifier as reproductions against the real code and kept once fixed (CB-1 and CB-2:
// widening_test.go).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

var wfNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

func wfPolicy(t *testing.T, js string) *gate.Policy {
	t.Helper()
	p, fs, err := gate.ParsePolicy([]byte(js), false)
	if err != nil {
		t.Fatalf("policy %s: %v %v", js, err, fs)
	}
	return p
}

func wfEntry(t *testing.T, v *report.V2, name string) report.Entry {
	t.Helper()
	for _, e := range v.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %s", name)
	return report.Entry{}
}

func wfCodes(fs []contract.Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

// CB-1 (widening level): the finding's three constructs and its two AWS ranges.
func TestWFCB3RouteNegativeNotRefused(t *testing.T) {
	base := inferDir(t, routeCapture(t, 0, 1), false)
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	man := &contract.Manifest{Schema: contract.ManifestSchema, Window: contract.Window{Start: t0, End: t0.Add(4 * time.Minute),
		Cut: contract.CutNone}}
	rec, err := fit.FromInferred(fit.Input{Capture: "routes", Manifest: man}, base, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := rec.Inferred()
	if err != nil {
		t.Fatal(err)
	}
	topo := inf.Topology
	xi := topo.Index("x")
	S := topo.Services[xi].S
	ra, rb := inf.Stats[xi].Routes[0], inf.Stats[xi].Routes[1]
	t.Logf("x: S %.4f, route %s share %.4f own %.4f, route %s share %.4f own %.4f", S, ra.Route, ra.Share, ra.OwnMs,
		rb.Route, rb.Share, rb.OwnMs)
	parse := func(items string) *change.Set {
		s, err := change.Parse([]byte(`{"schema":"rhobound.change/v1","fit":"` + rec.Digest() + `","items":[` + items + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// control: one route item past the route's own time is refused
	ctrl := parse(fmt.Sprintf(`{"op":"own_ms","service":"x","route":"GET /a","value":%v,"label":"declared"}`, -(ra.OwnMs + 0.6)))
	t.Logf("control (route /a %v alone): Validate codes %v", -(ra.OwnMs + 0.6), wfCodes(ctrl.Validate(rec, nil)))
	if !slicesContains(wfCodes(ctrl.Validate(rec, nil)), contract.ChangeNegativeTime) {
		t.Fatalf("the control is not refused")
	}
	check := func(name, items string, route int) {
		s := parse(items)
		codes := wfCodes(s.Validate(rec, nil))
		ref := s.Refusal(rec, nil)
		res, err := s.Resolve(rec, nil)
		if err != nil {
			t.Fatal(err)
		}
		pl, perr := planCorners(topo, inf.Stats, res.Items)
		if perr != nil {
			t.Fatal(perr)
		}
		c := pl.corners[0].patch
		// d[route] = blend (DS) + the caller's edge adjustment (routePatch: adj = d[r] - blend for the caller of route r)
		owner := "a"
		if route == 1 {
			owner = "b"
		}
		ci := topo.Index(owner)
		d := c.DS[xi] + c.Edge[model.Edge{Caller: ci, Child: xi}]
		mu := inf.Stats[xi].Routes[route].OwnMs + d
		v, perr2 := Predict(Request{Fit: rec, Changes: res, Findings: s.Validate(rec, nil)})
		var re *report.RefusalError
		var chg string
		if perr2 == nil {
			en := wfEntry(t, v, owner)
			chg = fmt.Sprintf("%+v (before %.4f, after %+v)", *en.ChangeMs, en.BeforeMs, *en.AfterMs)
		}
		t.Logf("%s: Validate codes %v, Refusal %v; corner DS[x] %.4f, edge %s->x %.4f, route %s mean after = %.4f + %.4f = %.4f; "+
			"Predict err %v (coded refusal %v), entry %s change %s", name, codes, ref, c.DS[xi], owner,
			c.Edge[model.Edge{Caller: ci, Child: xi}], inf.Stats[xi].Routes[route].Route, inf.Stats[xi].Routes[route].OwnMs, d, mu,
			perr2, errors.As(perr2, &re), owner, chg)
		// fixed: the set that would make the route's own time negative is refused (change.negative_time)
		if !slicesContains(codes, contract.ChangeNegativeTime) || ref == nil {
			t.Errorf("%s: a set that makes route %s's own time %.4f ms is not refused", name,
				inf.Stats[xi].Routes[route].Route, mu)
		}
	}
	whole := -(ra.OwnMs + 0.4)
	if S+whole+ra.Share*(-0.2) < 0 {
		t.Fatalf("setup: whole-service time would be negative")
	}
	check("whole + route /a", fmt.Sprintf(`{"op":"own_ms","service":"x","value":%v,"label":"declared"},`+
		`{"op":"own_ms","service":"x","route":"GET /a","value":-0.2,"label":"declared"}`, whole), 0)
	each := -0.6 * rb.OwnMs
	check("two route /b items", fmt.Sprintf(`{"op":"own_ms","service":"x","route":"GET /b","value":%v,"label":"declared"},`+
		`{"op":"own_ms","service":"x","route":"GET /b","value":%v,"label":"declared"}`, each, each), 1)
}

// CB-4: a measured own_ms item is checked at its delta and applied at delta - 2 SE.
func TestWFCB4MeasuredLowCornerNegative(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	rec := &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: true, Findings: []contract.Finding{},
		Services: []abdiff.Service{{Service: "b", OwnMs: abdiff.Delta{Baseline: 4, Candidate: 0.05, Delta: -3.95, SE: 0.05}}}}
	d := "sha256:" + strings.Repeat("cd", 32)
	recs := records{d: rec}
	s := set(change.Item{Op: change.OpOwnMs, Service: "b", Value: change.Point(-3.95), Label: change.LabelMeasuredAB,
		Evidence: []string{d}})
	ref := s.Refusal(r, recs)
	fs := s.Validate(r, recs)
	res, err := s.Resolve(r, recs)
	if err != nil {
		t.Fatal(err)
	}
	v, perr := Predict(Request{Fit: r, Changes: res, Findings: fs})
	var re *report.RefusalError
	coded := errors.As(perr, &re)
	t.Logf("fit S(b) = 4; Refusal %v; Validate codes %v; resolved value %+v; Predict report nil %v, err %v; coded refusal %v",
		ref, wfCodes(fs), res.Items[0].Value, v == nil, perr, coded)
	// fixed: Validate checks the measured item at the corner Resolve applies (delta - 2 SE), so the set is refused with
	// change.negative_time instead of failing in Predict with an uncoded error
	if ref == nil || !slicesContains(wfCodes(fs), contract.ChangeNegativeTime) {
		t.Errorf("a measured item whose low corner makes the time negative is not refused (Predict err %v, coded %v)", perr, coded)
	}
}
