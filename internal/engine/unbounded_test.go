// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"math"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// lowerFit is handFit with the workers of the named services read as lower bounds (inferred, not from queueing: too
// few requests waited to read them), as the fit marks them (workers_lower_bound).
func lowerFit(t *testing.T, topo *model.Topology, lam float64, lower ...string) *fit.Record {
	t.Helper()
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	inf := &infer.Inferred{Topology: topo, LamExtPerMs: lam, WindowS: 600, Requests: 1000}
	for i, s := range topo.Services {
		_, entry := topo.Entry[i]
		inf.Stats = append(inf.Stats, infer.ServiceStats{Name: s.Name, Entry: entry,
			WorkersInferred: slices.Contains(lower, s.Name)})
	}
	r, err := fit.FromInferred(fit.Input{Capture: "hand"}, inf, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// chain3 is a calls b calls c, each one pod of one worker: at 0.1 requests per ms a is 10% busy, b 40%, c 50%.
func chain3() *model.Topology {
	return &model.Topology{Name: "chain3", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "b", S: 4, CS2: 1, Calls: []model.Call{{Child: 2, P: 1}}},
		{Name: "c", S: 5, CS2: 1},
	}}
}

// TestUnboundedWorkers: the entry's change with the queueing of the lower-bound services the change raises at its
// least (report.Unbounded, workers_unbounded), and rule 10 reading it (maze-kit lean/Rbproofs/Lanes/GateRules.lean,
// GR-1's second facet). c's workers are a lower bound, b's are identified; the threshold is 5% of a's mean.
func TestUnboundedWorkers(t *testing.T) {
	rec := lowerFit(t, chain3(), 0.1, "c")
	full := wfPolicy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	model := wfPolicy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5, "block_band": "model"}`)
	predict := func(items ...change.Item) *report.V2 {
		t.Helper()
		v, err := Predict(Request{Fit: rec, Changes: set(items...)})
		if err != nil {
			t.Fatal(err)
		}
		if !v.Rows[2].WorkersLower || v.Rows[1].WorkersLower {
			t.Fatalf("setup: lower-bound rows b %v, c %v", v.Rows[1].WorkersLower, v.Rows[2].WorkersLower)
		}
		v.FitWindow.End = wfNow.Add(-time.Hour) // a fresh fit: rule 3 does not fire
		return v
	}
	rule10 := func(v *report.V2, p *gate.Policy) (*gate.Result, gate.Rule) {
		res := gate.Evaluate(gate.Input{Report: v, Policy: p, Now: wfNow})
		return res, res.RuleRow("10")
	}

	// b raised alone: c's utilization does not move, so there is no range, and the block stands
	v := predict(item(change.OpOwnMs, "b", 1, 1))
	if en := wfEntry(t, v, "a"); en.Unbounded != nil {
		t.Errorf("b raised alone: workers_unbounded %+v", *en.Unbounded)
	}
	if res, r10 := rule10(v, full); res.Verdict != gate.Block || r10.Rule != gate.RuleMeanChange {
		t.Fatalf("b raised alone: %s, rule 10 %+v", res.Verdict, r10)
	}

	// GR-1's second facet: b raised, c raised by 0.0001 ms. The range is never above the change (here it is the change:
	// b's departures, smoother after its raise, shorten c's queue by more than c's raise lengthens it, and a pure delay
	// would lose that), and the block stands; without it (a report of the engine before it) it was inconclusive
	v = predict(item(change.OpOwnMs, "b", 1, 1), item(change.OpOwnMs, "c", 0.0001, 0.0001))
	en := wfEntry(t, v, "a")
	t.Logf("c raised negligibly: a before %.4f ms, change %+v, workers_unbounded %+v", en.BeforeMs, *en.ChangeMs,
		en.Unbounded)
	if u := en.Unbounded; u == nil || !slices.Equal(u.Services, []string{"c"}) || !(u.ChangeMs.Lo <= en.ChangeMs.Lo) ||
		!(u.ChangeMs.Hi <= en.ChangeMs.Hi) || !(en.ChangeMs.Lo-u.ChangeMs.Lo < 0.001) || !(u.BandMs.Lo < u.ChangeMs.Lo) {
		t.Fatalf("c raised negligibly: change %+v, workers_unbounded %+v", *en.ChangeMs, en.Unbounded)
	}
	for _, p := range []*gate.Policy{full, model} {
		if res, r10 := rule10(v, p); res.Verdict != gate.Block || res.ExitCode != gate.ExitBlock ||
			r10.Rule != gate.RuleMeanChange || slices.Contains(res.DecideWith, "workers:c") {
			t.Errorf("c raised negligibly, block_band %q: %s, rule 10 %+v, decide_with %q", p.BlockBand, res.Verdict, r10,
				res.DecideWith)
		}
	}
	v.Entries[0].Unbounded = nil
	if res, r10 := rule10(v, full); res.Verdict != gate.Inconclusive || r10.Rule != gate.RuleChangeUpperBound {
		t.Errorf("c raised negligibly, no range: %s, rule 10 %+v", res.Verdict, r10)
	}

	// a block that rests on c: c raised by 1 ms moves a's mean by several, of which c's own time is 1 ms. With c
	// unbounded (a pure delay at the end of the chain) the change is that 1 ms exactly, under the threshold
	v = predict(item(change.OpOwnMs, "c", 1, 1))
	en = wfEntry(t, v, "a")
	t.Logf("c raised by 1 ms: change %+v, workers_unbounded %+v", *en.ChangeMs, en.Unbounded)
	if u := en.Unbounded; u == nil || !slices.Equal(u.Services, []string{"c"}) || math.Abs(u.ChangeMs.Lo-1) > 1e-9 ||
		math.Abs(u.ChangeMs.Hi-1) > 1e-9 || !(en.ChangeMs.Lo > 3) {
		t.Fatalf("c raised by 1 ms: change %+v, workers_unbounded %+v", *en.ChangeMs, en.Unbounded)
	}
	for _, p := range []*gate.Policy{full, model} {
		if res, r10 := rule10(v, p); res.Verdict != gate.Inconclusive || r10.Rule != gate.RuleChangeUpperBound ||
			r10.Effect != gate.Inconclusive || !slices.Contains(res.DecideWith, "workers:c") {
			t.Errorf("c raised by 1 ms, block_band %q: %s, rule 10 %+v, decide_with %q", p.BlockBand, res.Verdict, r10,
				res.DecideWith)
		}
	}

	// c improved, b raised: a corner that lowers c takes no range, and the block stands (an improvement at a lower
	// bound is over-stated)
	v = predict(item(change.OpOwnMs, "b", 1, 1), item(change.OpOwnMs, "c", -0.2, -0.2))
	if en = wfEntry(t, v, "a"); en.Unbounded != nil {
		t.Errorf("c improved: workers_unbounded %+v", *en.Unbounded)
	}
	if res, r10 := rule10(v, full); res.Verdict != gate.Block || r10.Rule != gate.RuleMeanChange {
		t.Errorf("c improved: %s, rule 10 %+v", res.Verdict, r10)
	}

	// a range of c's own time: each corner takes the lesser of the change and the change unbounded, and the report is
	// the same bytes at any GOMAXPROCS
	items := []change.Item{item(change.OpOwnMs, "b", 0.5, 1), item(change.OpOwnMs, "c", 0.5, 1)}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	one := predict(items...)
	runtime.GOMAXPROCS(4)
	four := predict(items...)
	if !sameJSON(t, one, four) {
		t.Errorf("the report differs between GOMAXPROCS 1 and 4")
	}
	en = wfEntry(t, one, "a")
	if u := en.Unbounded; u == nil || !(u.ChangeMs.Lo <= en.ChangeMs.Lo) || !(u.ChangeMs.Hi <= en.ChangeMs.Hi) ||
		!(u.ChangeMs.Lo >= 0.5) || !(u.ChangeMs.Lo <= u.ChangeMs.Hi) {
		t.Errorf("a range: change %+v, workers_unbounded %+v", *en.ChangeMs, en.Unbounded)
	}
}
