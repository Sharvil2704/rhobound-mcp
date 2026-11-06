// SPDX-License-Identifier: Apache-2.0

// Package engine is the predictor as a pure function (docs/mcp/ARCHITECTURE.md 4.1): a fit record, a change set and a
// load point in, report v2 out. Pure means no file, clock or environment variable is read; the same request, tool
// version and GOARCH give the same bytes.
//
// For each corner of the change set's ranges (ARCHITECTURE.md 4.5) it solves the fit's topology before and after the
// change, and reports per entry the change range, the model band at the default fractions and the band widened on the
// side of each biased construct on the entry's path, per service the utilization after the change, the evidence
// class, the labels, the assumptions list and the findings.
//
// The v1 part of the report is made exactly as `rhobound predict CAPTURE` makes it, by the same calls (model.AnalyticWith,
// model.ChangePatchWith, report.Build) on the same values, so that the v1 projection of a prediction from a fit is
// byte-identical to the v1 report of the capture the fit was made from, with the same options and a point change.
package engine

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// DefaultMaxRho is the default utilization limit, as `predict --max-rho`.
const DefaultMaxRho = 0.85

// Request is one prediction.
type Request struct {
	// Fit is the fit record to predict from.
	Fit *fit.Record
	// Changes is the change set, validated (change.Set.Validate found no refusal) and resolved (change.Set.Resolve:
	// measured items widened to their delta +-2 standard errors). Nil, or a set without items, gives the fit's
	// baseline: every change range null.
	Changes *change.Set
	// ChangeHash is the digest of the set as it was read, before Resolve (change.Set.Digest); the report's
	// change_hash.
	ChangeHash string
	// Findings are the change set's own findings below refuse level (Validate's doubts and notes), which the report
	// lists beside the fit's and the engine's.
	Findings []contract.Finding
	// LoadScale is the load point: a multiplier of the fit's external rate. 0 and 1 are the fit's window
	// (evaluated_at fit_window); any other value is a what-if, where the system before the change and the system
	// after it are both solved at LoadScale times the fit's rate, so that an entry's change_ms is the change's effect
	// at that load and its load_level_ms the load's effect alone. A load_scale item of the set multiplies on top of it.
	LoadScale float64
	// Plain solves with the basic model (Poisson arrivals everywhere, exponential branches), as --plain.
	Plain bool
	// MaxRho is the utilization limit, in (0, 0.95]; 0 means DefaultMaxRho.
	MaxRho float64
}

// ModelName is the model line of a report, as rhobound-cli prints it.
func ModelName(plain bool) string {
	if plain {
		return "basic: Poisson arrivals at every queue, exponential parallel branches"
	}
	return "refined: arrival variability propagated through the graph (weight 0.7), parallel branches as correlated gammas"
}

// ModelNameFor is ModelName for a topology: the refined model of one with calls made at once to one service says
// that it takes them as batch arrivals.
func ModelNameFor(plain bool, t *model.Topology) string {
	if !plain && t != nil && model.HasBatches(t) {
		return ModelName(plain) + ", calls made at once to one service as batch arrivals (M^X/G/c)"
	}
	return ModelName(plain)
}

// Config is the model configuration of --plain or the default.
func Config(plain bool) model.Config {
	if plain {
		return model.Plain
	}
	return model.Default
}

// EvidenceClass is the evidence class every report of this version carries: a fit is read from a real capture whose
// software is not the repository's own queue-like demo, and the engine cannot tell third-party software from it, so it
// says the least generous class (ARCHITECTURE.md 4.6). The gate may lower nothing from it.
const EvidenceClass = report.EvidenceRealUntested

// Predict solves the request and writes report v2.
//
// It refuses, with a *report.RefusalError, a base load already above MaxRho (model.utilization_observed, with
// rhobound-cli's message; in a what-if, the base at LoadScale times the fit's rate, with the number load_scale, and
// also the fit's own load when the model has no steady state there, without it), an operation this version cannot
// apply (change.unsupported_op) and more than change.MaxUnsignedRanges unsigned ranged items (change.too_many_ranges).
// A change that pushes a service past MaxRho or to saturation is not refused: the report shows it (the rows'
// utilization_after, null ranges where a corner leaves no finite mean), because the gate must be able to block on it
// (ARCHITECTURE.md 6.3, rule 8). `predict --fit F --report v1` refuses it from the report, with the message `predict
// CAPTURE` prints (AfterLimit).
func Predict(req Request) (*report.V2, error) {
	if req.Fit == nil {
		return nil, errors.New("engine: no fit")
	}
	maxRho := req.MaxRho
	if maxRho == 0 {
		maxRho = DefaultMaxRho
	}
	if !(maxRho > 0 && maxRho <= 0.95) {
		return nil, fmt.Errorf("engine: the utilization limit must be in (0, 0.95], got %v", maxRho)
	}
	scale := req.LoadScale
	if scale == 0 {
		scale = 1
	}
	if !(scale > 0) || math.IsInf(scale, 0) {
		return nil, fmt.Errorf("engine: the load scale must be a positive number, got %v", scale)
	}
	inf, err := req.Fit.Inferred()
	if err != nil {
		return nil, err
	}
	topo := inf.Topology
	cfg := Config(req.Plain)
	lam := inf.LamExtPerMs * scale
	base, err := model.AnalyticWith(topo, lam, nil, cfg)
	if err != nil {
		return nil, err
	}
	if mr, _ := base.MaxRho(); !base.Stable || !(mr <= maxRho) { // a NaN utilization is refused too
		when := "refusing to predict: at the observed load"
		if scale != 1 {
			when = fmt.Sprintf("refusing to predict: at %g times the observed load", scale)
		}
		le := LimitError(contract.ModelUtilizationObserved, when, inf, base, maxRho)
		var re *report.RefusalError
		if scale != 1 && errors.As(le, &re) {
			// refused at the load point: the refusal says which, so that the gate reading it records the what-if
			re.Numbers[report.LoadScaleNumber] = scale
		}
		return nil, le
	}
	// a what-if: the fit's own load is solved too, for the misfit check and the load level (fitLoad). Above the fit's
	// load it is stable whenever the scaled base is; below it a fit the model has no steady state for at its own load
	// is refused, as there is then neither a misfit check nor a level.
	var fitBase *model.Result
	if scale != 1 {
		if fitBase, err = model.AnalyticWith(topo, inf.LamExtPerMs, nil, cfg); err != nil {
			return nil, err
		}
		if !fitBase.Stable {
			// refused at the fit's own load, as at load_scale 1: no load_scale number
			return nil, LimitError(contract.ModelUtilizationObserved, "refusing to predict: at the observed load", inf,
				fitBase, maxRho)
		}
	}
	var items []change.Item
	if req.Changes != nil {
		items = req.Changes.Items
	}
	plan, err := planCorners(topo, inf.Stats, items)
	if err != nil {
		return nil, err
	}
	deltas := make([]*model.Delta, len(plan.corners))
	for k, c := range plan.corners {
		if deltas[k], err = solve(topo, lam, base, c, cfg); err != nil {
			return nil, err
		}
	}

	// the v1 part, as predict CAPTURE makes it
	kept := req.Fit.KeptShare.Value
	var point *model.Delta
	var changes []report.Change
	if plan.points && len(deltas) == 1 {
		point, changes = deltas[0], plan.changes
	}
	rep := report.Build(req.Fit.Capture, inf, kept, base, point, changes)
	rep.Model = ModelNameFor(req.Plain, topo)
	var fitLoad []report.Row // the rows at the fit's load, in a what-if only
	if fitBase != nil {
		// the rows' model is at the load point, the traces at the fit's load: error_pct stays the fit's misfit check
		// (gate rule 4), the model at the fit's load against the traces, which the load point does not change
		fitLoad = report.Build(req.Fit.Capture, inf, kept, fitBase, nil, nil).Rows
		for i := range rep.Rows {
			rep.Rows[i].ErrorPct = fitLoad[i].ErrorPct
		}
	}
	v := &report.V2{Report: *rep}
	v.Version = report.VersionV2
	v.FitDigest = req.Fit.Digest()
	v.ManifestDigest = req.Fit.ManifestDigest
	v.FitWindow = req.Fit.Window
	v.ChangeHash = req.ChangeHash
	v.Tool = req.Fit.Tool
	v.EvidenceClass = EvidenceClass
	v.UtilizationLimit = maxRho
	v.Corners = len(deltas)
	v.Monotonicity = report.Monotonicity
	v.ModelBand = report.DefaultModelBand()
	v.Items, v.Labels, v.RecordDigests, v.WeakestLabel = applied(items)
	v.Load = report.Load{EvaluatedAt: report.EvaluatedAtFitWindow, LoadScale: scale, MaxUtilizationBefore: v.MaxRho}
	if scale != 1 {
		v.Load.EvaluatedAt = report.EvaluatedAtWhatIf
	}
	if len(deltas) > 0 {
		r := rangeOf(len(deltas), func(k int) float64 { mr, _ := deltas[k].Patched.MaxRho(); return mr })
		v.Load.MaxUtilizationAfter = &r
	}
	hops := make([]float64, len(topo.Services))
	for i, st := range inf.Stats {
		if !st.Entry {
			hops[i] = topo.Services[i].Hop
		}
	}
	stable := true
	for _, d := range deltas {
		stable = stable && d.Stable
	}
	rows := make([]report.Row, len(v.Rows))
	for i, row := range v.Rows {
		if len(deltas) > 0 {
			u := rangeOf(len(deltas), func(k int) float64 { return deltas[k].Patched.Rho[i] })
			row.UtilizationAfter = &u
			if stable {
				c := rangeOf(len(deltas), func(k int) float64 { return deltas[k].Patched.L[i] + hops[i] - row.ModelMs })
				row.ChangeRangeMs = &c
			}
		}
		rows[i] = row
	}
	v.Rows = rows
	var free *unbounded
	if stable {
		free = unboundedChange(topo, lam, cfg, base, plan.corners, deltas, v.Rows)
	}
	v.Entries = entries(topo, inf, base, v.Rows, fitLoad, deltas, stable, plan.changed, req.Plain, v.ModelBand, free)
	v.Findings = findings(req.Fit.Findings, req.Findings)
	v.Assumptions = assumptions(v.Findings)
	return v, nil
}

// AfterLimit is the refusal of `predict --fit F --report v1` for a change that a v1 report cannot show: one that leaves
// some service, at some corner, above the utilization limit or without a steady state (model.utilization_after, with
// the message `predict CAPTURE` prints); nil when there is none. It expects a request Predict answered.
func AfterLimit(req Request) error {
	if req.Changes == nil || len(req.Changes.Items) == 0 {
		return nil
	}
	maxRho, scale := req.MaxRho, req.LoadScale
	if maxRho == 0 {
		maxRho = DefaultMaxRho
	}
	if scale == 0 {
		scale = 1
	}
	inf, err := req.Fit.Inferred()
	if err != nil {
		return err
	}
	cfg := Config(req.Plain)
	lam := inf.LamExtPerMs * scale
	base, err := model.AnalyticWith(inf.Topology, lam, nil, cfg)
	if err != nil {
		return err
	}
	plan, err := planCorners(inf.Topology, inf.Stats, req.Changes.Items)
	if err != nil {
		return err
	}
	for _, c := range plan.corners {
		d, err := solve(inf.Topology, lam, base, c, cfg)
		if err != nil {
			return err
		}
		if mr, _ := d.Patched.MaxRho(); !d.Stable || !(mr <= maxRho) {
			return LimitError(contract.ModelUtilizationAfter, "refusing the change: after it", inf, d.Patched, maxRho)
		}
	}
	return nil
}

// corner is the change at one corner of the ranges: the model patch, the workers per pod by node, the entry
// weights' factors by node and the probabilities of calls by node and call index (set_p).
type corner struct {
	patch   model.Patch
	workers map[int]int
	entry   map[int]float64
	calls   map[[2]int]float64
}

type cornerPlan struct {
	corners []corner
	// points: every item is a point (one corner), as --patch gives; changes are then v1's changes list.
	points  bool
	changes []report.Change
	// changed are the nodes the items change (own time or workers), for the biased constructs on their paths.
	changed []int
}

// planCorners lists the corners of a set's ranges (ARCHITECTURE.md 4.5): the low corner takes every increasing item at
// its lo and every decreasing one at its hi, the high corner the opposite ends, and each unsigned ranged item is
// enumerated at both ends, combined with both signed corners. A set of points has one corner; no items, none.
//
// An own_ms item on a route of a service (the fit's stats[i].Routes) moves the service's time by the route's share of
// it, so its queue moves by the blend, and the calls that reach the route by the rest (routePatch).
func planCorners(topo *model.Topology, stats []infer.ServiceStats, items []change.Item) (cornerPlan, error) {
	var p cornerPlan
	if len(items) == 0 {
		return p, nil
	}
	p.points = true
	var unsigned []int
	routed := map[int]bool{} // the services some item changes by route
	varied := map[int]bool{} // the services an own_var_ms2 item changes
	for k, it := range items {
		if !change.Applies(it.Op) {
			return p, &report.RefusalError{Code: contract.ChangeUnsupportedOp, Subject: it.Service,
				Err: fmt.Errorf("%s: this version cannot apply %s", it.Service, it.Op)}
		}
		i := topo.Index(it.Service)
		if i < 0 {
			return p, &report.RefusalError{Code: contract.ChangeUnknownService, Subject: it.Service,
				Err: fmt.Errorf("%s: unknown service %q", it.Op, it.Service)}
		}
		if !slices.Contains(p.changed, i) && it.Op != change.OpLoadScale {
			p.changed = append(p.changed, i)
		}
		if it.Route != "" {
			if it.Op != change.OpOwnMs || routeIndex(stats, i, it.Route) < 0 {
				return p, &report.RefusalError{Code: contract.ChangeRouteScopeUnsupported, Subject: it.Service,
					Err: fmt.Errorf("%s: the fit has no route %q of %s for an own_ms item", it.Op, it.Route, it.Service)}
			}
			routed[i] = true
		}
		if it.Op == change.OpOwnVar {
			varied[i] = true
		}
		if !it.Value.IsPoint() {
			p.points = false
			if change.SignOf(it.Op) == change.SignUnsigned {
				unsigned = append(unsigned, k)
			}
		}
	}
	if len(unsigned) > change.MaxUnsignedRanges {
		return p, &report.RefusalError{Code: contract.ChangeTooManyRanges,
			Numbers: map[string]float64{"unsigned_ranges": float64(len(unsigned)), "limit": change.MaxUnsignedRanges},
			Err:     fmt.Errorf("%d unsigned ranged items; at most %d", len(unsigned), change.MaxUnsignedRanges)}
	}
	signed := []bool{false}
	if !p.points {
		signed = []bool{false, true}
	}
	for _, high := range signed {
		for mask := 0; mask < 1<<len(unsigned); mask++ {
			c := corner{workers: map[int]int{}, entry: map[int]float64{}, calls: map[[2]int]float64{}}
			n := len(topo.Services)
			c.patch = model.Patch{DS: make([]float64, n), Shift: make([]bool, n), Glue: make([]float64, n),
				DVar: make([]float64, n)}
			routeDS := map[int][]float64{} // per routed service, each route's own-time change
			for k, it := range items {
				v := it.Value.Lo
				switch change.SignOf(it.Op) {
				case change.SignIncreasing:
					if high {
						v = it.Value.Hi
					}
				case change.SignDecreasing:
					if !high {
						v = it.Value.Hi
					}
				case change.SignUnsigned:
					if j := slices.Index(unsigned, k); j >= 0 && mask&(1<<j) != 0 {
						v = it.Value.Hi
					}
				}
				i := topo.Index(it.Service)
				sv := topo.Services[i]
				switch it.Op {
				case change.OpOwnMs: // as --patch svc=+Nms: extra work holding the worker; the mean moves, the variance stays
					dv := v // the service's own-time change: a route's change counts by its share of the visits
					if rs := stats[i].Routes; routed[i] {
						if routeDS[i] == nil {
							routeDS[i] = make([]float64, len(rs))
						}
						if it.Route == "" {
							for r := range rs {
								routeDS[i][r] += v
							}
						} else {
							r := routeIndex(stats, i, it.Route)
							routeDS[i][r] += v
							dv = rs[r].Share * v
						}
					}
					c.patch.DS[i] += dv
					c.patch.Shift[i] = true
					if p.points && mask == 0 && !high {
						p.changes = append(p.changes, report.Change{Service: it.Service, DeltaMs: dv + 0, GlueMs: 0, Spec: spec(it)})
					}
				case change.OpOwnPct: // as --patch svc=+N%: the service time and the glue scaled alike
					delta := sv.S * v / 100
					glue := sv.Glue * v / 100
					c.patch.DS[i] += delta
					c.patch.Glue[i] += glue
					if p.points && mask == 0 && !high {
						p.changes = append(p.changes, report.Change{Service: it.Service, DeltaMs: delta + glue, GlueMs: glue, Spec: spec(it)})
					}
				case change.OpOwnVar: // a change of the service time's spread, ms^2, on top of the own-time change
					c.patch.DVar[i] += v
				case change.OpSetP: // the probability per visit of the service's one direct call to the callee
					at, why := change.DirectCall(topo, i, it.Callee)
					if why != "" {
						return p, &report.RefusalError{Code: contract.ChangeUnsupportedOp, Subject: it.Service,
							Err: fmt.Errorf("%s set_p %s: %s", it.Service, it.Callee, why)}
					}
					c.calls[[2]int{i, at}] = v
				case change.OpWorkers:
					c.workers[i] = int(v)
				case change.OpLoadScale:
					if _, ok := c.entry[i]; !ok {
						c.entry[i] = 1
					}
					c.entry[i] *= v
				}
			}
			routePatch(topo, stats, routeDS, varied, &c.patch)
			p.corners = append(p.corners, c)
		}
	}
	return p, nil
}

// routeIndex is the index of a route among service i's routes, or -1.
func routeIndex(stats []infer.ServiceStats, i int, route string) int {
	if i < 0 || i >= len(stats) {
		return -1
	}
	for r, rs := range stats[i].Routes {
		if rs.Route == route {
			return r
		}
	}
	return -1
}

// routePatch completes a corner's patch for its routed services (routeDS: each route's own-time change, by node). The
// queue already moves by the blend (DS). What a route's change adds beyond the blend reaches the calls that reach that
// route: a call from a node of caller C (C itself, or its stage: and shape: nodes) gets, on top of the blend, the mean
// of the routes' departures from the blend weighted by C's visits to each route (infer.RouteStats.Callers). The cs2
// after the change is the routes' mixture's: each route keeps its variance and moves its mean, and the model's cs2 is
// scaled by the mixture's change of cs2. That moves the service time's variance for the queue; a call sees instead
// the change of variance of the routes it reaches (EdgeVar), so that moving one route does not widen another's.
// A routed service that an own_var_ms2 item also changes (varied: a measured change of the blend's variance) takes the
// measured variance for its queue instead of the mixture's, and its callers' EdgeVar is taken against that.
func routePatch(topo *model.Topology, stats []infer.ServiceStats, routeDS map[int][]float64, varied map[int]bool, p *model.Patch) {
	nodes := make([]int, 0, len(routeDS))
	for i := range routeDS {
		nodes = append(nodes, i)
	}
	sort.Ints(nodes)
	for _, i := range nodes {
		rs, d := stats[i].Routes, routeDS[i]
		blend := 0.0
		for r := range rs {
			blend += rs[r].Share * d[r]
		}
		// the mixture's cs2 before and after
		var m0, q0, m1, q1 float64
		for r, x := range rs {
			v := x.OwnCS2 * x.OwnMs * x.OwnMs
			m0 += x.Share * x.OwnMs
			q0 += x.Share * (v + x.OwnMs*x.OwnMs)
			mu := x.OwnMs + d[r]
			m1 += x.Share * mu
			q1 += x.Share * (v + mu*mu)
		}
		// the model's service-time variance before and after (S and the cs2 the queue is solved with)
		sv := topo.Services[i]
		s0, s1 := sv.S, sv.S+p.DS[i]
		var blendVar float64 // its change
		if m0 > 0 && m1 > 0 && q0-m0*m0 > 0 {
			cs0, cs1 := (q0-m0*m0)/(m0*m0), math.Max(0, q1-m1*m1)/(m1*m1)
			if p.CS2 == nil {
				p.CS2 = map[int]float64{}
			}
			p.CS2[i] = sv.CS2 * cs1 / cs0
			if varied[i] && p.DVar != nil {
				p.CS2[i] = model.PatchedCS2(sv.CS2, s0, p.DS[i], true, p.DVar[i])
			}
			blendVar = p.CS2[i]*s1*s1 - sv.CS2*s0*s0
		}
		// the callers' share of each route
		for j := range topo.Services {
			calls := false
			for _, c := range topo.Services[j].Calls {
				calls = calls || c.Child == i
			}
			if !calls {
				continue
			}
			owner := infer.NodeOwner(topo.Services[j].Name)
			q := make([]float64, len(rs)) // the caller's visits to each route
			var w, adj float64
			for r, x := range rs {
				for _, c := range x.Callers {
					if c.Caller == owner && c.Caller != "" {
						q[r] += c.PerVisit
						w += c.PerVisit
						adj += c.PerVisit * (d[r] - blend)
					}
				}
			}
			if !(w > 0) {
				continue
			}
			if adj != 0 {
				if p.Edge == nil {
					p.Edge = map[model.Edge]float64{}
				}
				p.Edge[model.Edge{Caller: j, Child: i}] = adj / w
			}
			// the change of variance of the routes this caller reaches, against the blend's
			var a0, b0, a1, b1 float64
			for r, x := range rs {
				v := x.OwnCS2 * x.OwnMs * x.OwnMs
				a0 += q[r] / w * x.OwnMs
				b0 += q[r] / w * (v + x.OwnMs*x.OwnMs)
				mu := x.OwnMs + d[r]
				a1 += q[r] / w * mu
				b1 += q[r] / w * (v + mu*mu)
			}
			if dv := (b1 - a1*a1) - (b0 - a0*a0) - blendVar; dv != 0 {
				if p.EdgeVar == nil {
					p.EdgeVar = map[model.Edge]float64{}
				}
				p.EdgeVar[model.Edge{Caller: j, Child: i}] = dv
			}
		}
	}
}

// spec is an item's text in report v1's changes list: the --patch text as typed, or the value written so.
func spec(it change.Item) string {
	if it.Spec != "" {
		return it.Spec
	}
	s := strconv.FormatFloat(it.Value.Lo, 'g', -1, 64)
	if it.Value.Lo >= 0 {
		s = "+" + s
	}
	if it.Op == change.OpOwnPct {
		return s + "%"
	}
	return s + "ms"
}

// solve solves the topology after one corner's change, at the load lam. A corner that changes own times only is solved
// by model.ChangePatchWith, as predict CAPTURE solves a patch.
func solve(topo *model.Topology, lam float64, base *model.Result, c corner, cfg model.Config) (*model.Delta, error) {
	if len(c.workers) == 0 && len(c.entry) == 0 && len(c.calls) == 0 {
		return model.ChangePatchWith(topo, lam, c.patch, cfg)
	}
	pt := *topo
	pt.Services = append([]model.Service(nil), topo.Services...)
	if len(c.calls) > 0 {
		// a call's new probability; 0 removes it (the order of the nodes stays a topological one)
		for _, i := range sortedCallNodes(c.calls) {
			calls := make([]model.Call, 0, len(topo.Services[i].Calls))
			for k, call := range topo.Services[i].Calls {
				if v, ok := c.calls[[2]int{i, k}]; ok {
					if !(v >= 0 && v <= 1) {
						return nil, fmt.Errorf("%s: a call probability of %v", topo.Services[i].Name, v)
					}
					if v == 0 {
						continue
					}
					call.P = v
				}
				calls = append(calls, call)
			}
			pt.Services[i].Calls = calls
		}
	}
	for i, w := range c.workers {
		if pt.Services[i].Routing == model.Pooled {
			pt.Services[i].Servers = w // one pod: a pool of w workers
		} else {
			pt.Services[i].Workers = w
		}
	}
	if len(c.entry) > 0 {
		pt.Entry = make(map[int]float64, len(topo.Entry))
		for i, w := range topo.Entry {
			if f, ok := c.entry[i]; ok {
				w *= f
			}
			pt.Entry[i] = w
		}
	}
	ds := c.patch.DS
	for i := range pt.Services {
		pt.Services[i].CS2 = model.PatchedCS2(topo.Services[i].CS2, topo.Services[i].S, ds[i], c.patch.Shift[i], c.patch.DVar[i])
		if cs2, ok := c.patch.CS2[i]; ok {
			pt.Services[i].CS2 = cs2
		}
		if c.patch.Glue[i] != 0 {
			g := pt.Services[i].Glue + c.patch.Glue[i]
			if !(g >= 0) || math.IsInf(g, 0) {
				return nil, fmt.Errorf("%s: the changed glue %v + %v is not a non-negative number", topo.Services[i].Name,
					topo.Services[i].Glue, c.patch.Glue[i])
			}
			pt.Services[i].Glue = g
		}
	}
	pr, err := model.PatchedWith(&pt, lam, ds, c.patch, cfg)
	if err != nil {
		return nil, err
	}
	return &model.Delta{Stable: base.Stable && pr.Stable, Base: base, Patched: pr}, nil
}

// sortedCallNodes are the nodes whose calls a corner changes, in order.
func sortedCallNodes(m map[[2]int]float64) []int {
	var out []int
	for k := range m {
		if !slices.Contains(out, k[0]) {
			out = append(out, k[0])
		}
	}
	sort.Ints(out)
	return out
}

// rangeOf is the range of f over k = 0..n-1.
func rangeOf(n int, f func(k int) float64) change.Range {
	r := change.Range{Lo: math.Inf(1), Hi: math.Inf(-1)}
	for k := 0; k < n; k++ {
		x := f(k)
		r.Lo, r.Hi = math.Min(r.Lo, x), math.Max(r.Hi, x)
	}
	return r
}

// labelRank orders the labels from the strongest to the weakest: the order of ARCHITECTURE.md 4.3 and of the schema's
// enum.
func labelRank(l string) int { return slices.Index(change.Labels, l) }

// applied lists the items as applied, their labels, the records they cite, and the weakest label.
func applied(items []change.Item) ([]report.Applied, []report.LabelRef, []string, string) {
	out := make([]report.Applied, 0, len(items))
	labels := make([]report.LabelRef, 0, len(items))
	var recs []string
	weakest := ""
	for k, it := range items {
		out = append(out, report.Applied{Op: it.Op, Service: it.Service, Callee: it.Callee, Value: it.Value, Label: it.Label,
			Evidence: append([]string(nil), it.Evidence...), Sign: change.SignOf(it.Op)})
		labels = append(labels, report.LabelRef{Item: k, Label: it.Label})
		if weakest == "" || labelRank(it.Label) > labelRank(weakest) {
			weakest = it.Label
		}
		if it.Label != change.LabelDeclared {
			for _, e := range it.Evidence {
				if !slices.Contains(recs, e) {
					recs = append(recs, e)
				}
			}
		}
	}
	sort.Strings(recs)
	return out, labels, recs, weakest
}

// unbounded is each corner's change of every node with the services whose worker count is a lower bound and which
// the corner raises taken with unbounded workers (report.Unbounded).
type unbounded struct {
	// services are the rows' names of the nodes some corner takes unbounded, in node order (node i is row i).
	services []string
	// change is, by corner and by node, the node's mean after the change less its mean before, both so solved.
	change [][]float64
}

// unboundedChange solves each corner again with the services whose worker count is a lower bound (Row.WorkersLower)
// and whose utilization that corner raises (report.Raises, the gate's test) given more workers: as pure delays
// (unbounded workers, no queue, the service time kept) and at each count of workerLadder above their bound, and keeps
// per node the least change over them. More workers do not always mean a smaller change: under the default model a
// raise also smooths the service's departures (departureSCV), which shortens its busy children's queues, so the least
// can lie at a finite count (a lower-bound service at rho 0.375 feeding four children at 0.84, own time +10 ms: 10.0
// unbounded, 6.8 at 3 workers). The ladder is a finite search: the least between its counts is assumed not far below.
// The system before the change is solved at the same counts, once for each set of services and count. It returns nil
// when no corner raises such a service, and when a solve fails or has no finite mean: the report then has no
// workers_unbounded, and the gate holds a block that may rest on them at inconclusive.
func unboundedChange(topo *model.Topology, lam float64, cfg model.Config, base *model.Result, corners []corner,
	deltas []*model.Delta, rows []report.Row) *unbounded {
	if len(corners) > maxLadderCorners {
		// eight solves per corner: past this many corners the report goes without workers_unbounded, and the gate
		// holds a block that may rest on lower-bound workers at inconclusive, as before (this version applies only
		// signed items, so a set has at most two corners)
		return nil
	}
	n := len(topo.Services)
	sets := make([][]bool, len(deltas))
	union := make([]bool, n)
	for k, d := range deltas {
		sets[k] = make([]bool, n)
		for i := 0; i < n && i < len(rows); i++ {
			if rows[i].WorkersLower && report.Raises(base.Rho[i], d.Patched.Rho[i]) {
				sets[k][i], union[i] = true, true
			}
		}
	}
	u := &unbounded{change: make([][]float64, len(deltas))}
	for i, free := range union {
		if free {
			u.services = append(u.services, rows[i].Name)
		}
	}
	if len(u.services) == 0 {
		return nil
	}
	type solved struct {
		set  []bool
		at   int // -1: pure delays; else the index into workerLadder
		topo *model.Topology
		base *model.Result
	}
	var done []solved // in the order of the corners that first need them: no map is iterated
	variant := func(set []bool, at int) (solved, bool) {
		if j := slices.IndexFunc(done, func(s solved) bool { return s.at == at && slices.Equal(s.set, set) }); j >= 0 {
			return done[j], true
		}
		pt := *topo
		pt.Services = append([]model.Service(nil), topo.Services...)
		for i, free := range set {
			if !free {
				continue
			}
			sv := &pt.Services[i]
			switch {
			case at < 0:
				sv.Delay, sv.Shares = true, nil
			case sv.Routing == model.Pooled:
				sv.Servers = workerLadder(sv.Servers)[at]
			default:
				sv.Workers = workerLadder(sv.Workers)[at]
			}
		}
		b, err := model.AnalyticWith(&pt, lam, nil, cfg)
		if err != nil || !b.Stable {
			return solved{}, false
		}
		done = append(done, solved{set: set, at: at, topo: &pt, base: b})
		return done[len(done)-1], true
	}
	for k, c := range corners {
		u.change[k] = make([]float64, n)
		for at := -1; at < ladderSteps; at++ {
			s, ok := variant(sets[k], at)
			if !ok {
				return nil
			}
			d, err := solve(s.topo, lam, s.base, c, cfg)
			if err != nil || !d.Stable {
				return nil
			}
			for i := range u.change[k] {
				if x := d.Patched.L[i] - s.base.L[i]; at < 0 || x < u.change[k][i] {
					u.change[k][i] = x
				}
			}
		}
	}
	return u
}

// ladderSteps is the length of workerLadder; maxLadderCorners the corners unboundedChange solves at most.
const (
	ladderSteps      = 7
	maxLadderCorners = 16
)

// workerLadder are the worker counts above a lower bound w at which unboundedChange solves: the next four, then 2, 4
// and 8 times (a count may repeat for small w; it is solved again, at no harm).
func workerLadder(w int) [ladderSteps]int {
	return [ladderSteps]int{w + 1, w + 2, w + 3, w + 4, 2 * w, 4 * w, 8 * w}
}

// entries are the report's entry rows: the change range over the corners, the model band and the band widened by the
// biased constructs on the path from the entry to a changed service, and, when the change raises a lower-bound service
// (free), the change with its queueing at its least. In a what-if, fitLoad are the rows at the fit's load, and each
// entry carries its mean there and the load level, its mean at the load point less that.
func entries(topo *model.Topology, inf *infer.Inferred, base *model.Result, rows, fitLoad []report.Row,
	deltas []*model.Delta, stable bool, changed []int, plain bool, band report.ModelBand, free *unbounded) []report.Entry {
	out := []report.Entry{}
	stage := make([]bool, len(topo.Services)) // node i is row i (fit.Record.Check)
	for i, st := range inf.Stats {
		if i < len(stage) {
			stage[i] = st.Stage
		}
	}
	for i, st := range inf.Stats {
		if !st.Entry || st.External {
			continue
		}
		e := report.Entry{Name: st.Name, BeforeMs: rows[i].ModelMs, Biased: []report.Biased{}}
		if fitLoad != nil {
			at, level := fitLoad[i].ModelMs, rows[i].ModelMs-fitLoad[i].ModelMs
			e.BeforeAtFitLoadMs, e.LoadLevelMs = &at, &level
		}
		if len(deltas) > 0 && stable {
			after := rangeOf(len(deltas), func(k int) float64 { return deltas[k].Patched.L[i] })
			ch := rangeOf(len(deltas), func(k int) float64 { return deltas[k].Patched.L[i] - e.BeforeMs })
			e.AfterMs, e.ChangeMs = &after, &ch
			if e.BeforeMs > 0 {
				pct := change.Range{Lo: 100 * ch.Lo / e.BeforeMs, Hi: 100 * ch.Hi / e.BeforeMs}
				e.ChangePct = &pct
			}
		}
		mag := 0.0
		if e.ChangeMs != nil {
			mag = math.Max(math.Abs(e.ChangeMs.Lo), math.Abs(e.ChangeMs.Hi))
		}
		e.ModelBandMs = band.Ms(mag, e.BeforeMs)
		if len(deltas) > 0 {
			e.Biased = biased(topo, stage, base, i, changed, plain)
		}
		if e.ChangeMs != nil {
			low, high := widening(e.Biased, *e.ChangeMs, e.BeforeMs)
			// the model band at each end's own change (ARCHITECTURE.md 4.7: band(x) at the change x)
			e.BandMs = &change.Range{Lo: e.ChangeMs.Lo - band.Ms(e.ChangeMs.Lo, e.BeforeMs) - low,
				Hi: e.ChangeMs.Hi + band.Ms(e.ChangeMs.Hi, e.BeforeMs) + high}
		}
		if e.ChangeMs != nil && free != nil {
			// at each corner the lesser of the change and the change unbounded: the true change is at least that
			ch := rangeOf(len(deltas), func(k int) float64 {
				return math.Min(deltas[k].Patched.L[i]-e.BeforeMs, free.change[k][i])
			})
			low, high := widening(e.Biased, ch, e.BeforeMs)
			e.Unbounded = &report.Unbounded{Services: slices.Clone(free.services), ChangeMs: ch,
				BandMs: change.Range{Lo: ch.Lo - band.Ms(ch.Lo, e.BeforeMs) - low, Hi: ch.Hi + band.Ms(ch.Hi, e.BeforeMs) + high}}
		}
		out = append(out, e)
	}
	return out
}

// findings are the fit's findings and the request's, each once by its code, subject, subject_untrusted and level, as
// the views list them (two services the name rule shows by one subject, one of them untrusted, keep a finding each),
// sorted.
func findings(lists ...[]contract.Finding) []contract.Finding {
	out := []contract.Finding{}
	seen := map[string]bool{}
	for _, l := range lists {
		for _, f := range l {
			key := f.Code + "\x00" + f.Subject + "\x00" + strconv.FormatBool(f.SubjectUntrusted) + "\x00" + string(f.Level)
			if !seen[key] {
				seen[key] = true
				out = append(out, f)
			}
		}
	}
	contract.SortFindings(out)
	return out
}

// assumptions is the assumptions list (ARCHITECTURE.md 4.8; DATA.md 3.12, 3.13), in a fixed order: what no check of
// the traces can see is not-checkable, and steady roughly Poisson load is checked by check's arrival detectors.
func assumptions(fs []contract.Finding) []report.Assumption {
	has := func(codes ...string) bool {
		for _, f := range fs {
			if slices.Contains(codes, f.Code) {
				return true
			}
		}
		return false
	}
	const traces = "not visible in traces"
	out := []report.Assumption{
		{ID: report.AssumeBalancingRandom, Status: report.NotCheckable, How: "the balancing policy is " + traces + "; modelled as random (wait too high under round-robin)"},
		{ID: report.AssumeWorkerFreedAtCall, Status: report.NotCheckable, How: "a worker held after the first call out is " + traces},
		{ID: report.AssumeClientsInstrumented, Status: report.NotCheckable, How: "an uninstrumented client folds its time into the caller's own time"},
		{ID: report.AssumeNoUntracedRetries, Status: report.NotCheckable, How: "retries and timeouts that leave no span are " + traces},
		{ID: report.AssumeNoCPUThrottling, Status: report.NotCheckable, How: "no resource metrics were supplied"},
		{ID: report.AssumeEntriesNotLostCalls, Status: report.NotCheckable, How: "a capture with no captured top that lost most calls into a service reads them as entries"},
		{ID: report.AssumeTailSamplingDeclared, Status: report.NotCheckable, How: "tail sampling that was not declared is " + traces},
	}
	steady := report.Assumption{ID: report.AssumeSteadyPoisson, Status: report.CheckedOK, How: "check's arrival detectors (DATA.md 3.12)"}
	switch {
	case has(contract.TraceLoadTrend, contract.TraceArrivalsOverdispersed, contract.ModelBatchArrivals):
		steady.Status = report.CheckedDoubt
	case has(contract.TraceArrivalsUnjudged):
		steady.Status, steady.How = report.NotChecked, "too few entry requests to judge the arrivals (trace.arrivals_unjudged)"
	case len(fs) == 0 || !hasCheck(fs):
		steady.Status, steady.How = report.NotChecked, "the fit carries no check report"
	}
	out = append(out, steady,
		report.Assumption{ID: report.AssumeServiceTimeFlat, Status: report.NotCheckable, How: "one capture is one load level"})
	return out
}

// hasCheck reports whether the findings come from a check report: every checked capture carries the permanent
// sampling doubt or a kept-share source finding, so a check report is never silent.
func hasCheck(fs []contract.Finding) bool {
	for _, f := range fs {
		if strings.HasPrefix(f.Code, "trace.") || strings.HasPrefix(f.Code, "store.") || strings.HasPrefix(f.Code, "cluster.") {
			return true
		}
	}
	return false
}

// LimitError is the refusal of a load (the observed one, or the one after the change) at which a service is above the
// --max-rho limit (code model.utilization_observed or model.utilization_after): it names each such service and its
// utilization, says which would pass 100% (no steady state; the model is unstable there), and, for a service whose
// worker count is only a lower bound, that its utilization is then an upper bound and --workers sets the count (a pod
// with no worker limit reads as such a bound). The message is rhobound-cli's.
func LimitError(code, when string, inf *infer.Inferred, r *model.Result, limit float64) error {
	var busy, lower []string
	example, subject := "", ""
	for i, rho := range r.Rho {
		if rho <= limit {
			continue
		}
		name := inf.Topology.Services[i].Name
		if subject == "" {
			subject = name
		}
		if math.IsNaN(rho) {
			busy = append(busy, fmt.Sprintf("%s has no utilization (NaN: an input is out of range)", name))
			continue
		}
		if rho < 1 {
			busy = append(busy, fmt.Sprintf("%s would run at %.1f%% utilization", name, 100*rho))
		} else {
			busy = append(busy, fmt.Sprintf("%s would pass 100%% utilization (%.1f%%: no steady state, the model is unstable)", name, 100*rho))
		}
		if i < len(inf.Stats) {
			if st := inf.Stats[i]; st.WorkersInferred && !st.WorkersFromQueueing && !st.External && !st.Stage {
				sv := inf.Topology.Services[i]
				pods, w := sv.Servers, sv.Workers
				if sv.Routing == model.Pooled {
					pods, w = 1, sv.Servers*sv.Workers
				}
				lower = append(lower, fmt.Sprintf("%s (%d x %d+)", name, pods, w))
				if example == "" {
					example = name
				}
			}
		}
	}
	if len(busy) == 0 { // every utilization is within the limit, but a residence is not finite (rounding at saturation)
		busy = append(busy, "the model has no finite latency (a queue at the edge of saturation)")
	}
	msg := fmt.Sprintf("%s %s, above the limit of %.1f%% (--max-rho; the model covers steady load below it only)", when,
		strings.Join(busy, ", "), 100*limit)
	if len(lower) > 0 {
		msg += fmt.Sprintf(". The worker count of %s is only a lower bound: too few of its requests waited for a worker, or "+
			"too briefly, to tell its pool from a larger one or from one with no limit, so that utilization is an upper bound. "+
			"If the pool is larger, or has no limit, set its size with --workers (for example --workers %s=N with N its "+
			"workers per pod)", strings.Join(lower, ", "), example)
	}
	mr, _ := r.MaxRho()
	return &report.RefusalError{Code: code, Subject: subject, Numbers: map[string]float64{"utilization": mr, "limit": limit},
		Err: errors.New(msg)}
}
