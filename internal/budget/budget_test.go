// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

func policy(t *testing.T, js string) *gate.Policy {
	t.Helper()
	p, _, err := gate.ParsePolicy([]byte(js), false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// synth is a stand-in for the engine: entries whose change grows as lev*x + quad*x^2 per ms of own time added to a
// service, and services whose utilization grows in proportion to their own time, saturating at 1. It counts solves.
type synth struct {
	before map[string]float64            // entry -> mean before
	lev    map[string]map[string]float64 // service -> entry -> leverage
	quad   float64
	own    map[string]float64 // service -> own ms
	util   map[string]float64 // service -> utilization before
	solves int
}

func (s *synth) solve(req engine.Request) (*report.V2, error) {
	s.solves++
	r := &report.V2{}
	r.UtilizationLimit = req.MaxRho
	scale := req.LoadScale
	if scale == 0 {
		scale = 1
	}
	added := map[string]float64{}
	if req.Changes != nil {
		for _, it := range req.Changes.Items {
			added[it.Service] += it.Value.Lo
		}
	}
	mr := 0.0
	sat := false
	for _, name := range sortedKeys(s.own) {
		u0 := s.util[name] * scale
		mr = math.Max(mr, u0)
		row := report.Row{Name: name, OwnMs: s.own[name], Util: u0}
		if req.Changes != nil {
			u := u0 * (s.own[name] + added[name]) / s.own[name]
			row.UtilizationAfter = &change.Range{Lo: u, Hi: u}
			sat = sat || u >= 1
		}
		r.Rows = append(r.Rows, row)
	}
	if mr > req.MaxRho {
		return nil, &report.RefusalError{Code: contract.ModelUtilizationObserved, Err: errors.New("above the limit")}
	}
	r.Load = report.Load{EvaluatedAt: report.EvaluatedAtFitWindow, LoadScale: scale, MaxUtilizationBefore: mr}
	for _, e := range sortedKeys(s.before) {
		// the entry mean grows with load like a queue: before / (1 - 0.3 (scale - 1))
		en := report.Entry{Name: e, BeforeMs: s.before[e] / (1 - 0.3*(scale-1))}
		if req.Changes != nil && !sat {
			c := 0.0
			for svc, x := range added {
				c += s.lev[svc][e]*x + s.quad*x*x
			}
			en.ChangeMs = &change.Range{Lo: c, Hi: c}
		}
		r.Entries = append(r.Entries, en)
	}
	r.Bottle = "b"
	return r, nil
}

func sortedKeys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func chainSynth() *synth {
	return &synth{before: map[string]float64{"a": 45.22},
		lev:  map[string]map[string]float64{"a": {"a": 1}, "b": {"a": 1.15}, "c": {"a": 1.3}},
		own:  map[string]float64{"a": 7.8, "b": 19.9, "c": 10},
		util: map[string]float64{"a": 0.2, "b": 0.26, "c": 0.26}}
}

func find(t *testing.T, items []Item, svc, kind string) Item {
	t.Helper()
	for _, it := range items {
		if it.Service == svc && it.Kind == kind {
			return it
		}
	}
	t.Fatalf("no %s item for %s in %+v", kind, svc, items)
	return Item{}
}

// With a linear change, x_max and the safe budget have closed forms: lev*x = T, and lev*x + max(0.163 lev x,
// 0.0227 L) = T.
func TestBudgetLinear(t *testing.T) {
	s := chainSynth()
	p := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	b := &Budgeter{Policy: p, Solve: s.solve}
	items, truncated, err := b.Compute("", nil)
	if err != nil || truncated || len(items) != 3 {
		t.Fatalf("%v %v %+v", err, truncated, items)
	}
	if items[0].Service != "c" || items[2].Service != "a" {
		t.Errorf("items not ranked by leverage: %+v", items)
	}
	T := 0.05 * 45.22
	floor := 0.0227 * 45.22
	for svc, lev := range map[string]float64{"a": 1, "b": 1.15, "c": 1.3} {
		it := find(t, items, svc, KindOwnMs)
		wantMax := T / lev
		wantSafe := (T - floor) / lev
		if it.Max == nil || math.Abs(*it.Max-wantMax) > 2e-4 || *it.Max > wantMax {
			t.Errorf("%s: max %v, want %.5f", svc, it.Max, wantMax)
		}
		if it.Safe == nil || math.Abs(*it.Safe-wantSafe) > 2e-4 || *it.Safe > wantSafe {
			t.Errorf("%s: safe %v, want %.5f", svc, it.Safe, wantSafe)
		}
		if it.Binding != BindingPct || it.BindingEntry != "a" || it.Per != Per {
			t.Errorf("%s: binding %s at %s", svc, it.Binding, it.BindingEntry)
		}
		if it.AtLimit == nil || it.AtLimit.EntryChangeMs == nil || math.Abs(*it.AtLimit.EntryChangeMs-T) > 1e-3 {
			t.Errorf("%s: at limit %+v", svc, it.AtLimit)
		}
		// the safe budget is never x_max minus a band: the band is in entry ms, the budget in own ms
		if old := wantMax - floor; lev != 1 && math.Abs(old-wantSafe) < 1e-3 {
			t.Errorf("%s: the test cannot tell the safe budget from the subtraction", svc)
		}
	}
}

// Behind a fan-out the leverage is below 1: the safe budget must still keep change + band under T, which subtracting
// the band from x_max would not.
func TestSafeBudgetBehindAFanOut(t *testing.T) {
	s := &synth{before: map[string]float64{"front": 40}, lev: map[string]map[string]float64{"leaf": {"front": 0.4}},
		own: map[string]float64{"leaf": 5}, util: map[string]float64{"leaf": 0.1}}
	p := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	items, _, err := (&Budgeter{Policy: p, Solve: s.solve}).Compute("leaf", nil)
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	T, mb := 2.0, p.ModelBand.ModelBand()
	c := 0.4 * *it.Safe
	if c+mb.Ms(c, 40) > T+1e-9 {
		t.Fatalf("safe %v puts change + band at %v above %v", *it.Safe, c+mb.Ms(c, 40), T)
	}
	if sub := *it.Max - mb.Ms(T, 40); 0.4*sub+mb.Ms(0.4*sub, 40) <= T {
		t.Fatalf("the subtraction would have been safe too; the fixture shows nothing")
	}
	if *it.Safe > *it.Max {
		t.Fatalf("safe %v above max %v", *it.Safe, *it.Max)
	}
}

func TestBudgetBindingConstraints(t *testing.T) {
	// utilization binds first: a service at 0.75 with a small own time reaches 0.80 after +0.4 ms
	s := chainSynth()
	s.util["b"], s.own["b"] = 0.75, 6
	p := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	items, _, err := (&Budgeter{Policy: p, Solve: s.solve}).Compute("b", nil)
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	want := 6 * (0.80/0.75 - 1)
	if it.Binding != BindingUtilAfter || math.Abs(*it.Max-want) > 2e-4 || *it.Safe > *it.Max {
		t.Fatalf("%+v, want max %.5f bound by max_utilization_after", it, want)
	}
	if u := it.AtLimit.UtilizationAfter; u == nil || math.Abs(*u-0.80) > 1e-3 {
		t.Fatalf("at limit %+v", it.AtLimit)
	}
	// the utilization limit binds when the policy allows more than the engine's limit
	p2 := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 50, "max_utilization_after": 0.95}`)
	items, _, _ = (&Budgeter{Policy: p2, Solve: s.solve}).Compute("b", nil)
	if items[0].Binding != BindingLimit {
		t.Fatalf("%+v", items[0])
	}
	// a threshold in ms smaller than the percentage
	p3 := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5, "max_entry_mean_change_ms": 0.1}`)
	items, _, _ = (&Budgeter{Policy: p3, Solve: chainSynth().solve}).Compute("b", nil)
	if it := items[0]; it.Binding != BindingMs || math.Abs(*it.Max-0.1/1.15) > 2e-4 {
		t.Fatalf("%+v", it)
	}
	// no x is safe when the band's floor alone is over the threshold
	if it := items[0]; it.Safe != nil {
		t.Fatalf("safe %v with a threshold under the band's floor", *it.Safe)
	}
	// without a policy only the utilization limits bind, and there is no safe budget
	items, _, _ = (&Budgeter{Solve: chainSynth().solve}).Compute("b", nil)
	if it := items[0]; it.Binding != BindingUtilAfter || it.Safe != nil || math.Abs(*it.Max-19.9*(0.8/0.26-1)) > 2e-3 {
		t.Fatalf("no policy: %+v", it)
	}
	if a := Assumes(items, nil); len(a) != 2 || a[0] != AssumeMonotone || a[1] != AssumeNoPolicy {
		t.Fatalf("assumes %q", a)
	}
}

func TestBudgetSolvesAndConvexity(t *testing.T) {
	s := chainSynth()
	s.quad = 0.05
	p := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	b := &Budgeter{Policy: p, Solve: s.solve}
	if _, err := b.Base(); err != nil {
		t.Fatal(err)
	}
	s.solves = 0
	items, _, err := b.Compute("c", nil)
	if err != nil {
		t.Fatal(err)
	}
	// one leverage solve, then two searches of at most MaxSolves each
	if s.solves > 1+2*MaxSolves {
		t.Fatalf("%d solves", s.solves)
	}
	it := items[0]
	T := 0.05 * 45.22
	x := *it.Max
	if c := 1.3*x + 0.05*x*x; c > T || 1.3*(x+3e-4)+0.05*(x+3e-4)*(x+3e-4) <= T {
		t.Fatalf("max %v is not the root: change %v, T %v", x, c, T)
	}
}

func TestBudgetLoadScale(t *testing.T) {
	s := chainSynth()
	p := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`)
	items, _, err := (&Budgeter{Policy: p, Solve: s.solve}).Compute("", []string{KindLoadScale})
	if err != nil || len(items) != 1 {
		t.Fatal(err, items)
	}
	it := items[0]
	// the entry mean grows as 45.22 / (1 - 0.3 (k - 1)): +5% at k = 1 + (1 - 1/1.05) / 0.3
	want := 1 + (1-1/1.05)/0.3
	if it.Kind != KindLoadScale || it.Service != "b" || it.Binding != BindingPct || math.Abs(*it.Max-want) > 2e-4 {
		t.Fatalf("%+v, want %.5f", it, want)
	}
	if *it.Safe >= *it.Max || *it.Safe < 1 {
		t.Fatalf("safe %v", *it.Safe)
	}
	// with a loose threshold the validated ceiling caps the factor: 0.26 x k = 0.58
	p2 := policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 500}`)
	items, _, _ = (&Budgeter{Policy: p2, Solve: s.solve}).Compute("", []string{KindLoadScale})
	if it := items[0]; it.Binding != BindingUtilCeiling || math.Abs(*it.Max-0.58/0.26) > 2e-4 {
		t.Fatalf("%+v", it)
	}
	if a := Assumes(items, p2); len(a) != 2 || a[1] != AssumeHeadroom {
		t.Fatalf("assumes %q", a)
	}
}

// A budgeter at a load point (Budgeter.LoadScale, a what-if's judge packet) solves every request there: the baseline,
// the leverage and the own_ms searches at it, and a load_scale factor on top of it. At 0 or 1 the requests are the
// fit's window, unchanged.
func TestBudgeterLoadPoint(t *testing.T) {
	s := chainSynth()
	var seen []float64
	solve := func(req engine.Request) (*report.V2, error) {
		seen = append(seen, req.LoadScale)
		return s.solve(req)
	}
	for _, c := range []struct {
		scale float64
		want  []float64 // base, leverage, own_ms at 0.5, load_scale at 2
	}{{0, []float64{0, 0, 0, 2}}, {1, []float64{0, 0, 0, 2}}, {1.3, []float64{1.3, 1.3, 1.3, 2.6}},
		{0.7, []float64{0.7, 0.7, 0.7, 1.4}}} {
		seen = nil
		b := &Budgeter{Solve: solve, LoadScale: c.scale}
		base, err := b.Base()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Leverage([]string{"b"}); err != nil {
			t.Fatal(err)
		}
		for _, it := range []Item{{Service: "b", Kind: KindOwnMs}, {Kind: KindLoadScale}} {
			x := 0.5
			if it.Kind == KindLoadScale {
				x = 2
			}
			if _, err := b.at(&it, base, x); err != nil {
				t.Fatal(err)
			}
		}
		if fmt.Sprint(seen) != fmt.Sprint(c.want) {
			t.Errorf("load point %v: the requests were at %v, want %v", c.scale, seen, c.want)
		}
	}
}

func TestBudgetRefusals(t *testing.T) {
	b := &Budgeter{Solve: chainSynth().solve}
	var ge *gate.Error
	if _, _, err := b.Compute("", []string{KindExtraCalls}); !errors.As(err, &ge) || ge.Finding.Code != contract.ChangeUnsupportedOp {
		t.Fatalf("extra_calls: %v", err)
	}
	if _, _, err := b.Compute("nobody", nil); !errors.As(err, &ge) || ge.Finding.Code != contract.ChangeUnknownService {
		t.Fatalf("unknown service: %v", err)
	}
	if _, _, err := b.Compute("ext:203.0.113.9:5432", nil); !errors.As(err, &ge) || ge.Finding.Code != contract.ChangeUnknownService {
		t.Fatalf("a peer is not a service: %v", err)
	}
}

func TestBudgetTruncatesByLeverage(t *testing.T) {
	s := &synth{before: map[string]float64{"e": 100}, lev: map[string]map[string]float64{},
		own: map[string]float64{}, util: map[string]float64{}}
	for i := 0; i < 45; i++ {
		n := fmt.Sprintf("s%02d", i)
		s.lev[n] = map[string]float64{"e": float64(i+1) / 10}
		s.own[n], s.util[n] = 5, 0.1
	}
	items, truncated, err := (&Budgeter{Policy: policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_ms": 1}`),
		Solve: s.solve}).Compute("", nil)
	if err != nil || !truncated || len(items) != MaxItems || items[0].Service != "s44" || items[MaxItems-1].Service != "s05" {
		t.Fatalf("%v %v %d %s", err, truncated, len(items), items[0].Service)
	}
}

// modelSolver is the engine as rhobound-cli 1a2df09 computes it (infer, then the model with a --patch of own_ms
// items), written as report v2, so that the budget's searches can be checked on a real capture before and apart from
// the engine package.
func modelSolver(t *testing.T, capture string, workers map[string]int) Solver {
	t.Helper()
	d, err := otlp.ReadDir(capture)
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{KeptShare: 1, Workers: workers, Estimator: infer.EstimatorFIFO})
	if err != nil {
		t.Fatal(err)
	}
	topo := inf.Topology
	return func(req engine.Request) (*report.V2, error) {
		scale := req.LoadScale
		if scale == 0 {
			scale = 1
		}
		lam := inf.LamExtPerMs * scale
		base, err := model.AnalyticWith(topo, lam, nil, model.Default)
		if err != nil {
			return nil, err
		}
		if mr, _ := base.MaxRho(); !base.Stable || !(mr <= req.MaxRho) {
			return nil, &report.RefusalError{Code: contract.ModelUtilizationObserved, Err: errors.New("base load above the limit")}
		}
		var delta *model.Delta
		n := len(topo.Services)
		if req.Changes != nil && len(req.Changes.Items) > 0 {
			p := model.Patch{DS: make([]float64, n), Shift: make([]bool, n), Glue: make([]float64, n)}
			for _, it := range req.Changes.Items {
				i := topo.Index(it.Service)
				p.DS[i] += it.Value.Lo
				p.Shift[i] = true
			}
			if delta, err = model.ChangePatchWith(topo, lam, p, model.Default); err != nil {
				return nil, err
			}
		}
		rep := report.Build(capture, inf, 1, base, delta, nil)
		v := &report.V2{Report: *rep, UtilizationLimit: req.MaxRho}
		mr, _ := base.MaxRho()
		v.Load = report.Load{EvaluatedAt: report.EvaluatedAtFitWindow, LoadScale: scale, MaxUtilizationBefore: mr}
		for i := range v.Rows {
			if delta != nil {
				u := delta.Patched.Rho[i]
				if !delta.Stable {
					u = math.Inf(1)
				}
				v.Rows[i].UtilizationAfter = &change.Range{Lo: u, Hi: u}
			}
			row := v.Rows[i]
			if !row.Entry {
				continue
			}
			en := report.Entry{Name: row.Name, BeforeMs: row.ModelMs}
			if row.ChangeMs != nil {
				en.ChangeMs = &change.Range{Lo: *row.ChangeMs, Hi: *row.ChangeMs}
			}
			v.Entries = append(v.Entries, en)
		}
		return v, nil
	}
}

func repoFile(parts ...string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(append([]string{filepath.Dir(file), "..", ".."}, parts...)...)
}

// The budget goldens of the agent loop (ARCHITECTURE.md 8) on testdata/capture-chain, measured on the 1a2df09 binary: leverage of b on a by a +1 ms
// finite difference 1.1531 (`--patch b=+1ms` moves a by +1.1531 ms); with the workers pinned a=1, b=2, c=1 and a 5%
// threshold, x_max about 1.95 ms and the safe budget about 1.07 ms per visit of b.
func TestCaptureChainGoldens(t *testing.T) {
	solve := modelSolver(t, repoFile("testdata", "capture-chain"), map[string]int{"a": 1, "b": 2, "c": 1})
	b := &Budgeter{Policy: policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`), Solve: solve}
	lev, err := b.Leverage([]string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if got := lev["b"]["a"]; math.Abs(got-1.1531289150328945) > 1e-9 {
		t.Errorf("leverage of b on a %.10f, want 1.1531289150", got)
	}
	// the README's +6 ms on b moves a by +7.15 ms: a secant of about 1.19, a check of the solver, not of leverage
	r6, err := solve(engine.Request{Changes: b.ownMs("b", 6), MaxRho: engine.DefaultMaxRho})
	if err != nil || math.Abs(r6.Entries[0].ChangeMs.Lo-7.15) > 0.005 {
		t.Errorf("+6 ms on b: %v %v", err, r6.Entries[0].ChangeMs)
	}
	items, _, err := b.Compute("b", nil)
	if err != nil {
		t.Fatal(err)
	}
	it := items[0]
	t.Logf("b: max %.4f safe %.4f binding %s at %s, at limit %+v", *it.Max, *it.Safe, it.Binding, it.BindingEntry, *it.AtLimit.EntryChangeMs)
	if math.Abs(*it.Max-1.95) > 0.01 || math.Abs(*it.Safe-1.07) > 0.01 || it.Binding != BindingPct || it.BindingEntry != "a" {
		t.Errorf("b: max %v safe %v binding %s, want about 1.95 and 1.07", *it.Max, *it.Safe, it.Binding)
	}
	// the searches stop at their resolution: one step past each value crosses its constraint
	T := 0.05 * 45.2217
	for _, c := range []struct {
		x    float64
		safe bool
	}{{*it.Max, false}, {*it.Safe, true}} {
		ch := func(x float64) float64 {
			r, err := solve(engine.Request{Changes: b.ownMs("b", x), MaxRho: engine.DefaultMaxRho})
			if err != nil {
				t.Fatal(err)
			}
			v := r.Entries[0].ChangeMs.Lo
			if c.safe {
				v += report.DefaultModelBand().Ms(v, r.Entries[0].BeforeMs)
			}
			return v
		}
		if ch(c.x) > T || ch(c.x+3e-4) <= T {
			t.Errorf("value %v is not at the limit (safe %v): %v, %v against %v", c.x, c.safe, ch(c.x), ch(c.x+3e-4), T)
		}
	}
}

// The bisection converges and the entry change it reads is monotone in the added own time, on every service of the
// repository's captures that has an entry change (reported, as the monotonicity is an assumption the simulator test
// will settle).
func TestBisectionOnTestdataCaptures(t *testing.T) {
	for _, capture := range []string{
		repoFile("testdata", "capture-chain"),
		repoFile("internal", "infer", "testdata", "hops", "cache-30s"),
		repoFile("internal", "infer", "testdata", "counts", "shipped-20s"),
	} {
		solve := modelSolver(t, capture, nil)
		b := &Budgeter{Policy: policy(t, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5}`), Solve: solve}
		items, _, err := b.Compute("", nil)
		if err != nil {
			t.Fatalf("%s: %v", capture, err)
		}
		for _, it := range items {
			if it.Max == nil {
				continue
			}
			if it.Safe != nil && *it.Safe > *it.Max {
				t.Errorf("%s %s: safe %v above max %v", filepath.Base(capture), it.Service, *it.Safe, *it.Max)
			}
			prev := math.Inf(-1)
			for _, f := range []float64{0, 0.25, 0.5, 0.75, 1} {
				r, err := solve(engine.Request{Changes: b.ownMs(it.Service, f**it.Max), MaxRho: engine.DefaultMaxRho})
				if err != nil {
					t.Fatal(err)
				}
				worst := math.Inf(-1)
				for _, en := range r.Entries {
					if en.ChangeMs != nil {
						worst = math.Max(worst, en.ChangeMs.Lo)
					}
				}
				if worst < prev-1e-9 {
					t.Logf("%s %s: entry change not monotone at %v of max: %v after %v", filepath.Base(capture), it.Service, f, worst, prev)
				}
				prev = worst
			}
		}
	}
}
