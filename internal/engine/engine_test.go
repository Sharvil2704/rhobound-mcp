// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// handFit is the fit record of a hand-written topology at an external rate lam (requests per ms); entries are its
// entry services. A delay node named stage:S:n or shape:S:j is a stage node, as the inference marks one; any other
// delay node is a peer.
func handFit(t *testing.T, topo *model.Topology, lam float64) *fit.Record {
	t.Helper()
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	inf := &infer.Inferred{Topology: topo, LamExtPerMs: lam, WindowS: 600, Requests: 1000}
	for i, s := range topo.Services {
		_, entry := topo.Entry[i]
		stage := s.Delay && (strings.HasPrefix(s.Name, "stage:") || strings.HasPrefix(s.Name, "shape:"))
		inf.Stats = append(inf.Stats, infer.ServiceStats{Name: s.Name, Entry: entry, External: s.Delay && !stage,
			Stage: stage})
	}
	r, err := fit.FromInferred(fit.Input{Capture: "hand"}, inf, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// chain is a calls b; b is one pod of `workers` workers with service time s.
func chain(s float64, workers int) *model.Topology {
	return &model.Topology{Name: "chain", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "b", S: s, CS2: 1, Servers: workers},
	}}
}

func set(items ...change.Item) *change.Set {
	return &change.Set{Schema: change.Schema, Items: items}
}

func item(op, svc string, lo, hi float64) change.Item {
	return change.Item{Op: op, Service: svc, Value: change.Range{Lo: lo, Hi: hi}, Label: change.LabelDeclared}
}

func entryChange(t *testing.T, v *report.V2, name string) change.Range {
	t.Helper()
	for _, e := range v.Entries {
		if e.Name == name {
			if e.ChangeMs == nil {
				t.Fatalf("%s: no change range", name)
			}
			return *e.ChangeMs
		}
	}
	t.Fatalf("no entry %s", name)
	return change.Range{}
}

// Corners are oriented by each operation's sign (ARCHITECTURE.md 4.5): a workers range mixed with an own_ms range on
// a service where both matter is evaluated at (own lo, workers hi) and (own hi, workers lo), never at all-low and
// all-high, and the range is that of a brute-force grid over both.
func TestCornersOrientedBySign(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	v, err := Predict(Request{Fit: r, Changes: set(item(change.OpOwnMs, "b", 1, 3), item(change.OpWorkers, "b", 2, 4))})
	if err != nil {
		t.Fatal(err)
	}
	if v.Corners != 2 {
		t.Errorf("%d corners, want 2", v.Corners)
	}
	got := entryChange(t, v, "a")
	point := func(own, w float64) float64 {
		p, err := Predict(Request{Fit: r, Changes: set(item(change.OpOwnMs, "b", own, own), item(change.OpWorkers, "b", w, w))})
		if err != nil {
			t.Fatal(err)
		}
		return entryChange(t, p, "a").Lo
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	var argLo, argHi [2]float64
	for _, own := range []float64{1, 1.5, 2, 2.5, 3} {
		for _, w := range []float64{2, 3, 4} {
			c := point(own, w)
			if c < lo {
				lo, argLo = c, [2]float64{own, w}
			}
			if c > hi {
				hi, argHi = c, [2]float64{own, w}
			}
		}
	}
	if argLo != [2]float64{1, 4} || argHi != [2]float64{3, 2} {
		t.Errorf("the grid's minimum is at %v and maximum at %v", argLo, argHi)
	}
	if math.Abs(got.Lo-lo) > 1e-12 || math.Abs(got.Hi-hi) > 1e-12 {
		t.Errorf("corners give %+v, the grid %v..%v", got, lo, hi)
	}
	// all-low and all-high would have given a narrower, wrong range
	if wrong := point(1, 2); wrong <= got.Lo {
		t.Errorf("(own lo, workers lo) %v is not above the range's low end %v", wrong, got.Lo)
	}
	// a point change has one corner, and the v1 keys of its rows are set; a range leaves them unset
	p, _ := Predict(Request{Fit: r, Changes: set(item(change.OpOwnMs, "b", 1, 1))})
	if p.Corners != 1 || p.Rows[0].ChangeMs == nil || len(p.Changes) != 1 || p.Changes[0].Spec != "+1ms" {
		t.Errorf("a point: %d corners, %v, %+v", p.Corners, p.Rows[0].ChangeMs, p.Changes)
	}
	if v.Rows[0].ChangeMs != nil || v.Changes != nil || v.Rows[0].ChangeRangeMs == nil || v.Rows[1].UtilizationAfter == nil {
		t.Error("a range: the v1 point keys are set, or the v2 row keys are not")
	}
	// the band: each end widened by the model band at its own change
	e := v.Entries[0]
	b := report.DefaultModelBand()
	if math.Abs(e.BandMs.Lo-(got.Lo-b.Ms(got.Lo, e.BeforeMs))) > 1e-12 || math.Abs(e.BandMs.Hi-(got.Hi+b.Ms(got.Hi, e.BeforeMs))) > 1e-12 ||
		e.ModelBandMs != b.Ms(math.Max(math.Abs(got.Lo), math.Abs(got.Hi)), e.BeforeMs) {
		t.Errorf("band %+v, model band %v, change %+v", e.BandMs, e.ModelBandMs, got)
	}
}

// Unsigned ranged items are enumerated at every corner of their own ranges; more than MaxUnsignedRanges is refused.
// No unsigned operation is applied in this version, so the engine refuses them by name first.
func TestEngineRefusals(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	var items []change.Item
	for i := 0; i < change.MaxUnsignedRanges+1; i++ {
		items = append(items, item(change.OpSetAsync, "a", 0, 1))
	}
	for _, c := range []struct {
		set  *change.Set
		code string
	}{
		{set(item(change.OpCS2, "b", 1, 2)), contract.ChangeUnsupportedOp},
		{set(item(change.OpOwnMs, "x", 1, 1)), contract.ChangeUnknownService},
	} {
		_, err := Predict(Request{Fit: r, Changes: c.set})
		var re *report.RefusalError
		if !errors.As(err, &re) || re.Code != c.code {
			t.Errorf("%+v: %v", c.set.Items[0], err)
		}
	}
	inf, _ := r.Inferred()
	plan, err := planCorners(inf.Topology, inf.Stats, items)
	var re *report.RefusalError
	if !errors.As(err, &re) || re.Code != contract.ChangeUnsupportedOp {
		t.Errorf("unsigned items: %v (%d corners)", err, len(plan.corners))
	}
	// the base load above the limit: model.utilization_observed, rhobound-cli's message
	_, err = Predict(Request{Fit: r, MaxRho: 0.3})
	if !errors.As(err, &re) || re.Code != contract.ModelUtilizationObserved ||
		!strings.HasPrefix(err.Error(), "refusing to predict: at the observed load b would run at 40.0% utilization") {
		t.Errorf("base above the limit: %v", err)
	}
	// a change that saturates is reported, not refused; the v1 form refuses it as predict CAPTURE does
	req := Request{Fit: r, Changes: set(item(change.OpOwnMs, "b", 7, 7))}
	v, err := Predict(req)
	if err != nil || v.Entries[0].ChangeMs != nil || v.Rows[1].UtilizationAfter == nil || v.Rows[1].UtilizationAfter.Lo < 1 {
		t.Errorf("a saturating change: %v, %+v", err, v.Rows[1].UtilizationAfter)
	}
	if err := AfterLimit(req); !errors.As(err, &re) || re.Code != contract.ModelUtilizationAfter ||
		!strings.HasPrefix(err.Error(), "refusing the change: after it b would pass 100% utilization") {
		t.Errorf("AfterLimit: %v", err)
	}
}

// The load point and the other operations: a what-if load scale, a load_scale item on the entry, a workers item.
func TestLoadAndOperations(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	base, _ := Predict(Request{Fit: r})
	if base.Load.EvaluatedAt != report.EvaluatedAtFitWindow || base.Corners != 0 || base.Entries[0].ChangeMs != nil ||
		base.Load.MaxUtilizationAfter != nil || len(base.Items) != 0 || base.WeakestLabel != "" {
		t.Errorf("baseline: %+v", base.Load)
	}
	what, _ := Predict(Request{Fit: r, LoadScale: 1.5})
	if what.Load.EvaluatedAt != report.EvaluatedAtWhatIf || math.Abs(what.Load.MaxUtilizationBefore-0.6) > 1e-12 {
		t.Errorf("what-if: %+v", what.Load)
	}
	// load_scale on the only entry is the what-if load, after the change only
	sc, _ := Predict(Request{Fit: r, Changes: set(item(change.OpLoadScale, "a", 1.5, 1.5))})
	if d := entryChange(t, sc, "a").Lo; math.Abs(d-(what.Entries[0].BeforeMs-base.Entries[0].BeforeMs)) > 1e-9 {
		t.Errorf("load_scale 1.5: change %v, want %v", d, what.Entries[0].BeforeMs-base.Entries[0].BeforeMs)
	}
	// more workers lower the mean
	w, _ := Predict(Request{Fit: r, Changes: set(item(change.OpWorkers, "b", 4, 4))})
	if d := entryChange(t, w, "a").Lo; !(d < 0) || w.Items[0].Sign != change.SignDecreasing {
		t.Errorf("workers 2 -> 4: change %v", d)
	}
}

// sleeper is one entry service whose time per visit is a sleep holding one of 4 workers (cs2 0, one queue of 4: M/D/4),
// s ms per visit.
func sleeper(s float64) *model.Topology {
	return &model.Topology{Name: "sleeper", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "s", S: s, CS2: 0, Servers: 4},
	}}
}

// A what-if load scale (predict --fit F --load-scale S) solves the system before the change and after it at S times
// the fit's rate: change_ms is the change's effect at that load, and the load level, the load's effect alone, is
// apart. On a sleep-dominated M/G/c queue the effect of +1 ms grows with the load as Erlang C says.
//
// By hand, Allen-Cunneen with Poisson arrivals (ca2 1) and cs2 0: R = s + C(4, a) s/(4-a) (1+0)/2, a = lam s,
// C(m, a) = [a^m/m! /(1-a/m)] / [sum_{k<m} a^k/k! + a^m/m! /(1-a/m)].
//   - the fit's load, lam 0.2/ms, s 10: a 2, C = (16/24/0.5) / (1+2+2+8/6 + 16/24/0.5) = 4/23, R = 10 + 10/23
//     = 10.434782608696 ms.
//   - 1.4 times it, lam 0.28: s 10, a 2.8, C = 8.536889/19.915556 = 0.428654318233, R = 11.786059659302; s 11, a 3.08,
//     C = 16.302862/29.995747 = 0.543506..., R = 14.249219107893. The change at 1.4x is +2.463159448591 ms (+1.258214 at
//     the fit's load), the level +1.351277050607 ms.
func TestLoadScaleSleepDominated(t *testing.T) {
	r := handFit(t, sleeper(10), 0.2)
	plus1 := set(item(change.OpOwnMs, "s", 1, 1))
	at := func(scale float64) *report.V2 {
		v, err := Predict(Request{Fit: r, Changes: plus1, LoadScale: scale})
		if err != nil {
			t.Fatalf("load scale %v: %v", scale, err)
		}
		return v
	}
	one := at(1)
	e := one.Entries[0]
	if one.Load.EvaluatedAt != report.EvaluatedAtFitWindow || one.Load.LoadScale != 1 || e.BeforeAtFitLoadMs != nil ||
		e.LoadLevelMs != nil {
		t.Errorf("at the fit's load: %+v, %+v", one.Load, e)
	}
	if math.Abs(e.BeforeMs-(10+10.0/23)) > 1e-9 || math.Abs(e.ChangeMs.Lo-1.258213888692) > 1e-9 {
		t.Errorf("at the fit's load: before %v, change %+v", e.BeforeMs, *e.ChangeMs)
	}
	// the default and an explicit 1 are one report
	if def, err := Predict(Request{Fit: r, Changes: plus1}); err != nil || !sameJSON(t, def, one) {
		t.Errorf("LoadScale 0 and 1 differ: %v", err)
	}
	prev := e.ChangeMs.Lo
	for _, scale := range []float64{1.1, 1.2, 1.3, 1.4} {
		v := at(scale)
		e := v.Entries[0]
		if v.Load.EvaluatedAt != report.EvaluatedAtWhatIf || v.Load.LoadScale != scale || e.ChangeMs == nil ||
			e.BeforeAtFitLoadMs == nil || e.LoadLevelMs == nil {
			t.Fatalf("load scale %v: %+v, %+v", scale, v.Load, e)
		}
		// the level is the base at the load point less the base at the fit's load, and the change holds none of it
		if math.Abs(*e.BeforeAtFitLoadMs-one.Entries[0].BeforeMs) > 1e-12 || math.Abs(*e.LoadLevelMs-(e.BeforeMs-*e.BeforeAtFitLoadMs)) > 1e-12 ||
			!(*e.LoadLevelMs > 0) || math.Abs(e.ChangeMs.Lo-(e.AfterMs.Lo-e.BeforeMs)) > 1e-12 {
			t.Errorf("load scale %v: %+v", scale, e)
		}
		if !(e.ChangeMs.Lo > prev) {
			t.Errorf("load scale %v: change %v, not above %v at a lower load", scale, e.ChangeMs.Lo, prev)
		}
		prev = e.ChangeMs.Lo
		if scale == 1.4 {
			if math.Abs(e.BeforeMs-11.786059659302) > 1e-9 || math.Abs(e.AfterMs.Lo-14.249219107893) > 1e-9 ||
				math.Abs(e.ChangeMs.Lo-2.463159448591) > 1e-9 || math.Abs(*e.LoadLevelMs-1.351277050607) > 1e-9 {
				t.Errorf("1.4 times the fit's load: before %v, after %v, change %v, level %v; by hand 11.786059659302, "+
					"14.249219107893, +2.463159448591, +1.351277050607", e.BeforeMs, e.AfterMs.Lo, e.ChangeMs.Lo, *e.LoadLevelMs)
			}
			if u := v.Load.MaxUtilizationAfter; math.Abs(v.Load.MaxUtilizationBefore-0.7) > 1e-12 || u == nil ||
				math.Abs(u.Hi-0.77) > 1e-12 {
				t.Errorf("1.4 times the fit's load: utilization %+v", v.Load)
			}
		}
	}
	// the load point refuses as the fit's own load does: 1.8 times puts the queue at 90%. The refusal carries the load
	// point (load_scale), so that the gate reading it can say it was a what-if
	_, err := Predict(Request{Fit: r, Changes: plus1, LoadScale: 1.8})
	var re *report.RefusalError
	if !errors.As(err, &re) || re.Code != contract.ModelUtilizationObserved ||
		!strings.HasPrefix(err.Error(), "refusing to predict: at 1.8 times the observed load s would run at 90.0% utilization") {
		t.Errorf("1.8 times: %v", err)
	} else if re.Numbers[report.LoadScaleNumber] != 1.8 || len(re.Numbers) != 3 {
		t.Errorf("1.8 times: numbers %v", re.Numbers)
	}
	if c, ok := contract.Lookup(contract.ModelUtilizationObserved); !ok || !slices.Contains(c.Numbers, report.LoadScaleNumber) {
		t.Errorf("the registry does not list %s's %s number: %+v", contract.ModelUtilizationObserved, report.LoadScaleNumber, c)
	}
	// below the fit's load, a fit the model has no steady state for at its own load (112.5%) is refused: there is no
	// misfit check and no level. That refusal is of the fit's own load, as at load_scale 1: no load_scale number
	over := handFit(t, sleeper(10), 0.45)
	_, err = Predict(Request{Fit: over, LoadScale: 0.5})
	if !errors.As(err, &re) || re.Code != contract.ModelUtilizationObserved ||
		!strings.HasPrefix(err.Error(), "refusing to predict: at the observed load s would pass 100% utilization") {
		t.Errorf("0.5 times a fit over 100%%: %v", err)
	} else if _, has := re.Numbers[report.LoadScaleNumber]; has {
		t.Errorf("0.5 times a fit over 100%%: numbers %v", re.Numbers)
	}
	// at the fit's window the refusal is today's: no load_scale number
	if _, err = Predict(Request{Fit: over}); !errors.As(err, &re) || len(re.Numbers) != 2 {
		t.Errorf("a fit over 100%%: %v", err)
	}
	// one above the limit but steady is not refused below it: 0.9 at the fit's load, 0.45 at half of it
	busy, err := Predict(Request{Fit: handFit(t, sleeper(10), 0.36), LoadScale: 0.5})
	if err != nil || busy.Load.EvaluatedAt != report.EvaluatedAtWhatIf || !(*busy.Entries[0].LoadLevelMs < 0) {
		t.Errorf("0.5 times a fit at 90%%: %v", err)
	}
}

func sameJSON(t *testing.T, a, b *report.V2) bool {
	t.Helper()
	x, err1 := a.Canonical()
	y, err2 := b.Canonical()
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	return string(x) == string(y)
}

// The biased constructs on the path from an entry to a changed service (ARCHITECTURE.md 4.5, item 3), and the side
// each widens.
func TestBiasedConstructs(t *testing.T) {
	// a calls b then c in sequence; p calls q and r in parallel (r the slower); s calls stage:s:1, which calls t; u, a
	// smooth service, calls v
	topo := &model.Topology{Name: "shapes", Entry: map[int]float64{0: 0.25, 3: 0.25, 6: 0.25, 9: 0.25}, Services: []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}, {Child: 2, P: 1}}},
		{Name: "b", S: 1, CS2: 1},
		{Name: "c", S: 1, CS2: 1},
		{Name: "p", S: 1, CS2: 1, Parallel: true, Calls: []model.Call{{Child: 4, P: 1}, {Child: 5, P: 1}}},
		{Name: "q", S: 1, CS2: 1},
		{Name: "r", S: 5, CS2: 1},
		{Name: "s", S: 1, CS2: 1, Calls: []model.Call{{Child: 7, P: 1}}},
		{Name: "stage:s:1", Delay: true, CS2: 1, Calls: []model.Call{{Child: 8, P: 1}}},
		{Name: "t", S: 1, CS2: 1},
		{Name: "u", S: 1, CS2: 0.25, Calls: []model.Call{{Child: 10, P: 1}}},
		{Name: "v", S: 1, CS2: 1},
	}}
	r := handFit(t, topo, 0.05)
	v, err := Predict(Request{Fit: r, Changes: set(item(change.OpOwnMs, "c", 1, 1), item(change.OpOwnMs, "q", 1, 1),
		item(change.OpOwnMs, "t", 1, 1), item(change.OpOwnMs, "v", 1, 1), item(change.OpOwnMs, "b", 0, 0))})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]report.Biased{
		"a": {Construct: report.ConstructSecondSequentialCall, Service: "a", Direction: report.DirectionBoth},
		"p": {Construct: report.ConstructNonSlowestBranch, Service: "p", Direction: report.DirectionOver},
		"s": {Construct: report.ConstructStagedCalls, Service: "s", Direction: report.DirectionOver},
		"u": {Construct: report.ConstructRegularUpstream, Service: "u", Direction: report.DirectionUnder},
	}
	for _, e := range v.Entries {
		w := want[e.Name]
		if len(e.Biased) != 1 || e.Biased[0] != w {
			t.Errorf("%s: %+v, want %+v", e.Name, e.Biased, w)
			continue
		}
		m := report.DefaultModelBand()
		low := e.ChangeMs.Lo - m.Ms(e.ChangeMs.Lo, e.BeforeMs) - e.BandMs.Lo
		high := e.BandMs.Hi - e.ChangeMs.Hi - m.Ms(e.ChangeMs.Hi, e.BeforeMs)
		switch w.Direction {
		case report.DirectionOver: // the true change is nearer zero: the low side of an increase
			if !(low > 0) || math.Abs(high) > 1e-12 {
				t.Errorf("%s: widened %v below, %v above", e.Name, low, high)
			}
		case report.DirectionUnder:
			if !(high > 0) || math.Abs(low) > 1e-12 {
				t.Errorf("%s: widened %v below, %v above", e.Name, low, high)
			}
		case report.DirectionBoth:
			if !(high > 0) || !(low > 0) {
				t.Errorf("%s: widened %v below, %v above", e.Name, low, high)
			}
		}
	}
	// the basic model has no regular-upstream bias; a decrease widens the other way
	plain, _ := Predict(Request{Fit: r, Plain: true, Changes: set(item(change.OpOwnMs, "v", 1, 1))})
	for _, e := range plain.Entries {
		if len(e.Biased) != 0 {
			t.Errorf("plain: %s %+v", e.Name, e.Biased)
		}
	}
	if lo, hi := widening([]report.Biased{want["p"]}, change.Range{Lo: -2, Hi: -1}, 50); lo != 0 || math.Abs(hi-0.85) > 1e-12 {
		t.Errorf("a decrease over-predicted: widened %v below, %v above", lo, hi)
	}
}

// A service the traces name like a stage node (stage:S:1, S any text) is a service of its own, not a stage node: the
// biased constructs name it by its whole name, a row's, never by S, and a call to it is no staged call.
func TestBiasedServiceNamedLikeAStage(t *testing.T) {
	name := "stage:x 203.0.113.9:1"
	topo := &model.Topology{Name: "named", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "e", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: name, S: 1, CS2: 0.25, Calls: []model.Call{{Child: 2, P: 1}}},
		{Name: "f", S: 1, CS2: 1},
	}}
	v, err := Predict(Request{Fit: handFit(t, topo, 0.05), Changes: set(item(change.OpOwnMs, "f", 1, 1))})
	if err != nil {
		t.Fatal(err)
	}
	want := report.Biased{Construct: report.ConstructRegularUpstream, Service: name, Direction: report.DirectionUnder}
	if len(v.Entries) != 1 || len(v.Entries[0].Biased) != 1 || v.Entries[0].Biased[0] != want {
		t.Errorf("biased: %+v, want [%+v]", v.Entries, want)
	}
	for _, sv := range []struct {
		name  string
		stage bool
		want  string
	}{{name, false, name}, {"stage:s:1", true, "s"}, {"stage:s:2.1", true, "s"}, {"shape:s:1", true, "s"},
		{"shape:x y:1", false, "shape:x y:1"}} {
		if got := nodeService(sv.name, sv.stage); got != sv.want {
			t.Errorf("nodeService(%q, %v) = %q, want %q", sv.name, sv.stage, got, sv.want)
		}
	}
}

type records map[string]*abdiff.Record

func (r records) AB(d string) (*abdiff.Record, bool) { x, ok := r[d]; return x, ok }

// A measured item enters as its record's delta +-2 standard errors, cites the record, and carries its doubts.
func TestMeasuredItem(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	rec := &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: false, Findings: []contract.Finding{
		contract.NewFinding(contract.ABErrorsDiffer)},
		Services: []abdiff.Service{{Service: "b", OwnMs: abdiff.Delta{Baseline: 4, Candidate: 4.4, Delta: 0.4, SE: 0.05},
			VarianceChanged: true, CallsChanged: true}}}
	d := "sha256:" + strings.Repeat("ab", 32)
	recs := records{d: rec}
	it := change.Item{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.4), Label: change.LabelMeasuredAB, Evidence: []string{d}}
	s := set(it)
	if err := s.Refusal(r, recs); err != nil {
		t.Fatal(err)
	}
	fs := s.Validate(r, recs)
	var codes []string
	for _, f := range fs {
		codes = append(codes, f.Code)
	}
	if strings.Join(codes, " ") != strings.Join([]string{contract.ABErrorsDiffer, contract.ProfileSelfReportMismatch,
		contract.ProfileStructureChanged, contract.ProfileVarianceChanged}, " ") {
		t.Errorf("doubts %v", codes)
	}
	res, err := s.Resolve(r, recs)
	se := rec.Services[0].OwnMs.SE
	if err != nil || res.Items[0].Value != (change.Range{Lo: 0.4 - 2*se, Hi: 0.4 + 2*se}) || s.Items[0].Value != change.Point(0.4) {
		t.Fatalf("resolved %+v (%v)", res.Items[0].Value, err)
	}
	h, _ := s.Digest()
	v, err := Predict(Request{Fit: r, Changes: res, ChangeHash: h, Findings: fs})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.RecordDigests) != 1 || v.RecordDigests[0] != d || v.WeakestLabel != change.LabelMeasuredAB || v.Corners != 2 ||
		v.ChangeHash != h || len(v.Findings) != 4 {
		t.Errorf("record digests %v, weakest %q, %d corners, %d findings", v.RecordDigests, v.WeakestLabel, v.Corners, len(v.Findings))
	}
	if steady := v.Assumptions[7]; steady.ID != report.AssumeSteadyPoisson || steady.Status != report.NotChecked {
		t.Errorf("a fit with no check report: %+v", steady)
	}
	// a label the CLI did not mint, or a value that is not the record's
	for _, bad := range []change.Item{
		{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.3), Label: change.LabelMeasuredAB, Evidence: []string{d}},
		{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.4), Label: change.LabelMeasuredAB, Evidence: []string{"sha256:" + strings.Repeat("0", 64)}},
		{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.4), Label: change.LabelMeasuredAB},
		{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.4), Label: change.LabelDerived},
		{Op: change.OpOwnMs, Service: "b", Value: change.Point(0.4), Label: change.LabelMeasuredPaired, Evidence: []string{d}},
	} {
		var ce *change.Error
		if err := set(bad).Refusal(r, recs); !errors.As(err, &ce) || ce.Finding.Code != contract.ChangeLabelNotMinted {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	if err := set(it).Refusal(r, nil); err == nil {
		t.Error("a measured item with no record store")
	}
}

// A record's profile.variance_changed and profile.structure_changed are no doubts once the derived items apply them
// (own_var_ms2 from its variance difference; set_p for every callee whose calls per visit changed), as
// model.batch_arrivals is none once the batch term models it; a changed callee the engine cannot set keeps the doubt.
func TestModelledDoubtsAreDropped(t *testing.T) {
	r := handFit(t, chain(4, 2), 0.2)
	codes := func(calls []abdiff.Calls) []string {
		rec := &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: true, Findings: []contract.Finding{
			contract.NewFinding(contract.ProfileStructureChanged).WithSubject("a"),
			contract.NewFinding(contract.ProfileVarianceChanged).WithSubject("a")},
			Services: []abdiff.Service{{Service: "a", OwnMs: abdiff.Delta{Baseline: 1, Candidate: 1.4, Delta: 0.4, SE: 0.05},
				OwnVar: abdiff.Delta{Baseline: 1, Candidate: 2, Delta: 1, SE: 0.2}, VarianceChanged: true, CallsChanged: true,
				Calls: calls}}}
		d := "sha256:" + strings.Repeat("cd", 32)
		s := set(change.Item{Op: change.OpOwnMs, Service: "a", Value: change.Point(0.4), Label: change.LabelMeasuredAB,
			Evidence: []string{d}})
		var out []string
		for _, f := range s.Validate(r, records{d: rec}) {
			out = append(out, f.Code)
		}
		return out
	}
	if got := codes([]abdiff.Calls{{Callee: "b", Baseline: 1, Candidate: 0.5, BaselineSE: 0.01, CandidateSE: 0.01}}); len(got) != 0 {
		t.Errorf("a modelled variance and call change: doubts %v, want none", got)
	}
	got := codes([]abdiff.Calls{{Callee: "zzz", Baseline: 1, Candidate: 0.5, BaselineSE: 0.01, CandidateSE: 0.01}})
	if strings.Join(got, " ") != contract.ProfileStructureChanged {
		t.Errorf("a changed callee the fit does not call directly: doubts %v, want only %s, once", got,
			contract.ProfileStructureChanged)
	}
}

// A changed variance of a service the change does not touch is one of the record's tests of its untouched services:
// it is a doubt only past change.UntouchedVarianceZ for that many (Šidák on the 2-SE test), so a catalog A/B that also
// measures five index shards is not held by one shard at 2.27 SE (the Muse experiment's false holds), while 3 SE
// still holds, and a record with one untouched service keeps the plain 2-SE test.
func TestUntouchedVarianceDoubtCorrected(t *testing.T) {
	if z := change.UntouchedVarianceZ(1); z != 2 {
		t.Errorf("UntouchedVarianceZ(1) = %v, want 2", z)
	}
	if z := change.UntouchedVarianceZ(5); math.Abs(z-2.60) > 0.01 {
		t.Errorf("UntouchedVarianceZ(5) = %.3f, want about 2.60", z)
	}
	r := handFit(t, chain(4, 2), 0.2)
	doubts := func(untouched []string, zc float64) []string {
		svcs := []abdiff.Service{{Service: "a", OwnMs: abdiff.Delta{Baseline: 1, Candidate: 1.4, Delta: 0.4, SE: 0.05},
			OwnVar: abdiff.Delta{Baseline: 1, Candidate: 1, Delta: 0, SE: 0.2}}}
		for _, n := range untouched {
			d := 0.0
			if n == "c" {
				d = zc
			}
			svcs = append(svcs, abdiff.Service{Service: n, OwnVar: abdiff.Delta{Baseline: 10, Candidate: 10 + d, Delta: d, SE: 1}})
		}
		rec := &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: true, Services: svcs, Findings: []contract.Finding{
			contract.NewFinding(contract.ProfileVarianceChanged).WithSubject("c").WithNumber("var_delta", zc).WithNumber("var_se", 1)}}
		d := "sha256:" + strings.Repeat("ef", 32)
		s := set(change.Item{Op: change.OpOwnMs, Service: "a", Value: change.Point(0.4), Label: change.LabelMeasuredAB,
			Evidence: []string{d}})
		var out []string
		for _, f := range s.Validate(r, records{d: rec}) {
			out = append(out, f.Code)
		}
		return out
	}
	five := []string{"b", "c", "d", "e", "f"}
	if got := doubts(five, -2.27); slicesContains(got, contract.ProfileVarianceChanged) {
		t.Errorf("one of five untouched services at 2.27 SE: doubts %v, want no %s", got, contract.ProfileVarianceChanged)
	}
	if got := doubts(five, 3.0); !slicesContains(got, contract.ProfileVarianceChanged) {
		t.Errorf("one of five untouched services at 3 SE: doubts %v, want %s", got, contract.ProfileVarianceChanged)
	}
	if got := doubts([]string{"c"}, -2.27); !slicesContains(got, contract.ProfileVarianceChanged) {
		t.Errorf("the one untouched service at 2.27 SE: doubts %v, want %s (2 SE for one test)", got, contract.ProfileVarianceChanged)
	}
}
