// SPDX-License-Identifier: Apache-2.0

// Package budget says what can be said before any code is written (docs/mcp/ARCHITECTURE.md 4.7): how much an entry's
// mean moves per millisecond of a service's own time (leverage), and how much own time per visit a service may add
// before the policy's threshold or a utilization limit is crossed (the budget), with and without the model band.
//
//   - Leverage: entry ms per ms of a service's own time per visit, by a +1 ms finite difference.
//   - Budget, x_max: the largest x with change(x) <= T at every entry, the utilization limits held, by bisection.
//   - Safe budget: the largest x with change(x) + band(x) <= T at every entry, band(x) the model band at that change
//     (report.ModelBand.Ms with the policy's fractions), by the same bisection. Both sides are entry milliseconds and
//     the answer is in the service's own milliseconds per visit; it is never x_max minus a band, which would mix the
//     two units (too strict where leverage is above 1, not strict enough behind a fan-out where it is below 1).
//
// Both rest on the entry mean being monotone in the added quantity, which is assumed, not tested yet: every budget
// says so (Assumes). Each search makes at most MaxSolves solves.
package budget

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/sanitize"
)

// The kinds of budget (budget.v1 input kinds).
const (
	KindOwnMs      = "own_ms"
	KindLoadScale  = "load_scale"
	KindExtraCalls = "extra_calls" // needs the structural patch: refused with change.unsupported_op until it ships
)

// The binding constraints of an item (budget.v1 binding_constraint).
const (
	BindingPct         = "max_entry_mean_change_pct"
	BindingMs          = "max_entry_mean_change_ms"
	BindingUtilAfter   = "max_utilization_after"
	BindingLimit       = "utilization_limit"
	BindingNone        = "none_below_search_cap"
	BindingUtilCeiling = "max_utilization_validated" // load_scale only: the factor is capped at the validated load
)

// Per is the unit of every budget: per visit of the service. Per call of a function is not reported: calls per visit
// of a function are a data-dependent unknown.
const Per = "visit"

// MaxSolves is the most solves one search makes; MaxItems the most items a budget lists (the highest leverage first).
const (
	MaxSolves = 25
	MaxItems  = 40
)

// The assumptions a budget states (budget.v1 assumes).
const (
	AssumeMonotone = "monotone entry mean in the added quantity (untested)"
	AssumeNoPolicy = "no policy: no entry threshold applies, so only the utilization limits bind"
	// the load factor is utilization headroom, and carries its sentence (ARCHITECTURE.md 4.7)
	AssumeHeadroom = "load_scale is utilization headroom to the utilization limit (mean only; assumes service time " +
		"constant with load), capped at max_utilization_validated. This cannot justify cutting replicas against a p99 SLO."
)

// Item is the budget of one service and kind. Max and Safe are nil when no value satisfies the constraints, or when
// none bound below the search cap (Binding BindingNone).
type Item struct {
	Service string
	Kind    string
	Per     string
	Max     *float64
	Safe    *float64
	// Binding is the constraint that limits Max; BindingEntry the entry it binds at, for a threshold.
	Binding      string
	BindingEntry string
	// AtLimit are the values at Max: the entry change (at BindingEntry, else the largest), and the utilization after
	// (of the service, or the busiest for load_scale).
	AtLimit *AtLimit
	// Leverage is the service's largest leverage over the entries, which ranks the items.
	Leverage float64
}

// AtLimit are an item's values at its Max.
type AtLimit struct {
	EntryChangeMs    *float64
	UtilizationAfter *float64
}

// Solver predicts a request: engine.Predict, or a stand-in in tests.
type Solver func(engine.Request) (*report.V2, error)

// Budgeter computes leverage and budgets on one fit.
type Budgeter struct {
	Fit *fit.Record
	// FitDigest is the fit's digest, written into the change sets it solves.
	FitDigest string
	// Policy gives the thresholds, the band fractions and max_utilization_after; nil means no threshold, and only the
	// utilization limits bind.
	Policy *gate.Policy
	// MaxRho is the utilization limit (0: engine.DefaultMaxRho).
	MaxRho float64
	// LoadScale is the load point every solve is at, a multiple of the fit's external rate (engine.Request.LoadScale):
	// 0 and 1 are the fit's window. At another value the baseline, the leverage and the budgets are those of a what-if
	// at it (`predict --fit F --load-scale S`), and a load_scale budget's factor is on top of it.
	LoadScale float64
	// Solve is engine.Predict when nil.
	Solve Solver

	base *report.V2
}

// Leverage returns the leverage of each service on each entry of the fit (entry ms per +1 ms of the service's own
// time per visit), by service and then by entry, for every service the fit can change.
func Leverage(f *fit.Record, digest string) (map[string]map[string]float64, error) {
	b := &Budgeter{Fit: f, FitDigest: digest}
	svcs, err := b.Services()
	if err != nil {
		return nil, err
	}
	return b.Leverage(svcs)
}

// Compute returns the budgets of a fit under a policy: of one service, or of every service when service is "", for
// the kinds asked (own_ms when none), at most MaxItems by leverage; truncated is true when more existed.
func Compute(f *fit.Record, digest string, p *gate.Policy, service string, kinds []string) (items []Item, truncated bool, err error) {
	b := &Budgeter{Fit: f, FitDigest: digest, Policy: p}
	return b.Compute(service, kinds)
}

func (b *Budgeter) solve(req engine.Request) (*report.V2, error) {
	req.Fit = b.Fit
	req.MaxRho = b.maxRho()
	if b.LoadScale != 0 && b.LoadScale != 1 {
		// the request's load point is on top of the budgeter's; at the fit's window it is left as it is, so that those
		// requests are today's
		s := req.LoadScale
		if s == 0 {
			s = 1
		}
		req.LoadScale = s * b.LoadScale
	}
	if b.Fit != nil {
		req.Plain = b.Fit.Options.Plain
	}
	if b.Solve != nil {
		return b.Solve(req)
	}
	return engine.Predict(req)
}

func (b *Budgeter) maxRho() float64 {
	if b.MaxRho > 0 {
		return b.MaxRho
	}
	return engine.DefaultMaxRho
}

// Base returns the fit's baseline report (no change, at the load point: the fit's window unless LoadScale says
// otherwise), solved once.
func (b *Budgeter) Base() (*report.V2, error) {
	if b.base != nil {
		return b.base, nil
	}
	r, err := b.solve(engine.Request{})
	if err != nil {
		return nil, err
	}
	b.base = r
	return r, nil
}

// Services returns the services whose own time a budget can change: the baseline's rows that are services with a
// queue, not peers outside the capture (ext:) nor the model's zero-time nodes, in the report's order.
func (b *Budgeter) Services() ([]string, error) {
	base, err := b.Base()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, row := range base.Rows {
		if Budgetable(row) {
			out = append(out, row.Name)
		}
	}
	return out, nil
}

// Budgetable reports whether a report row is a service a budget or a change can name (gate.IsService).
func Budgetable(row report.Row) bool { return gate.IsService(row) }

// ownMs is the change set that adds x ms of own time per visit to a service.
func (b *Budgeter) ownMs(service string, x float64) *change.Set {
	return &change.Set{Schema: change.Schema, Fit: b.FitDigest, Items: []change.Item{{Op: change.OpOwnMs,
		Service: service, Value: change.Point(x), Label: change.LabelDeclared}}}
}

// Leverage returns the leverage of each of these services on each entry, by a +1 ms finite difference at the load
// point. An entry whose mean is not finite after the +1 ms is left out of the service's map.
func (b *Budgeter) Leverage(services []string) (map[string]map[string]float64, error) {
	out := map[string]map[string]float64{}
	for _, s := range services {
		r, err := b.solve(engine.Request{Changes: b.ownMs(s, 1)})
		if err != nil {
			if limited(err) {
				out[s] = map[string]float64{}
				continue
			}
			return nil, err
		}
		m := map[string]float64{}
		for _, en := range r.Entries {
			if en.ChangeMs != nil && isFinite(en.ChangeMs.Lo) {
				m[en.Name] = en.ChangeMs.Lo
			}
		}
		out[s] = m
	}
	return out, nil
}

// Compute is the package's Compute on this budgeter.
func (b *Budgeter) Compute(service string, kinds []string) ([]Item, bool, error) {
	if len(kinds) == 0 {
		kinds = []string{KindOwnMs}
	}
	want := map[string]bool{}
	for _, k := range kinds {
		switch k {
		case KindOwnMs, KindLoadScale:
			want[k] = true
		case KindExtraCalls:
			return nil, false, &gate.Error{Finding: contract.NewFinding(contract.ChangeUnsupportedOp).WithSubject(k),
				Msg: "budget: extra_calls needs the structural patch, which this version does not have"}
		default:
			return nil, false, &gate.Error{Finding: contract.NewFinding(contract.ChangeUnsupportedOp).WithSubject(k),
				Msg: fmt.Sprintf("budget: unknown kind %q (own_ms, load_scale)", sanitizeName(k))}
		}
	}
	base, err := b.Base()
	if err != nil {
		return nil, false, err
	}
	all, err := b.Services()
	if err != nil {
		return nil, false, err
	}
	svcs := all
	if service != "" {
		// the service as the views show it, else as the traces wrote it (sanitize.Resolve)
		names := make([]string, len(base.Rows))
		for i, r := range base.Rows {
			names[i] = r.Name
		}
		raw, ok, ambiguous := sanitize.Resolve(sanitize.Services(names), service)
		if ambiguous {
			return nil, false, &gate.Error{Finding: contract.NewFinding(contract.ChangeAmbiguousService).WithSubject(service),
				Msg: fmt.Sprintf("budget: the views show more than one service as %s", sanitizeName(service))}
		}
		found := false
		for _, s := range all {
			found = found || ok && s == raw
		}
		if !found {
			return nil, false, &gate.Error{Finding: contract.NewFinding(contract.ChangeUnknownService).WithSubject(service),
				Msg: fmt.Sprintf("budget: %s is not a service of the fit", sanitizeName(service))}
		}
		svcs = []string{raw}
	}
	var items []Item
	truncated := false
	if want[KindOwnMs] {
		lev, err := b.Leverage(svcs)
		if err != nil {
			return nil, false, err
		}
		for _, s := range svcs {
			l := math.Inf(-1)
			for _, v := range lev[s] {
				l = math.Max(l, v)
			}
			if math.IsInf(l, -1) {
				l = 0
			}
			items = append(items, Item{Service: s, Kind: KindOwnMs, Per: Per, Leverage: l})
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].Leverage > items[j].Leverage })
		if len(items) > MaxItems {
			items, truncated = items[:MaxItems], true
		}
		for i := range items {
			if err := b.search(&items[i], base); err != nil {
				return nil, false, err
			}
		}
	}
	if want[KindLoadScale] {
		it := Item{Service: base.Bottle, Kind: KindLoadScale, Per: Per}
		if service != "" {
			it.Service = service
		}
		if err := b.search(&it, base); err != nil {
			return nil, false, err
		}
		items = append(items, it)
	}
	return items, truncated, nil
}

// Findings returns the findings a budget carries: budget.no_policy when no policy applies, because then no entry
// threshold binds and an item is bounded by the utilization limits only, which can be many times what a policy allows.
func Findings(p *gate.Policy) []contract.Finding {
	if p != nil {
		return nil
	}
	return []contract.Finding{contract.NewFinding(contract.BudgetNoPolicy)}
}

// Assumes returns what a budget of these items assumes, in a fixed order.
func Assumes(items []Item, p *gate.Policy) []string {
	out := []string{AssumeMonotone}
	if p == nil {
		out = append(out, AssumeNoPolicy)
	}
	for _, it := range items {
		if it.Kind == KindLoadScale {
			out = append(out, AssumeHeadroom)
			break
		}
	}
	return out
}

// outcome is one solve judged against the constraints.
type outcome struct {
	ok, safeOK   bool
	binding      string
	bindingEntry string
	// changes are the entries' changes; util the service's utilization after (the busiest one for a load factor)
	changes map[string]float64
	util    float64
}

// change returns the change at an entry, or the largest change when entry is "" or unknown.
func (o outcome) change(entry string) float64 {
	if c, ok := o.changes[entry]; ok {
		return c
	}
	m := math.NaN()
	for _, c := range o.changes {
		if math.IsNaN(m) || c > m {
			m = c
		}
	}
	return m
}

// limited reports whether an engine error is the refusal of a load above the utilization limit: a constraint that
// binds, not a failure.
func limited(err error) bool {
	var re *report.RefusalError
	return errors.As(err, &re) && (re.Code == contract.ModelUtilizationObserved || re.Code == contract.ModelUtilizationAfter)
}

// at solves an item at x and judges the outcome.
func (b *Budgeter) at(it *Item, base *report.V2, x float64) (outcome, error) {
	req := engine.Request{}
	if it.Kind == KindLoadScale {
		req.LoadScale = x
	} else {
		req.Changes = b.ownMs(it.Service, x)
	}
	r, err := b.solve(req)
	if err != nil {
		if limited(err) {
			return outcome{binding: BindingLimit, util: math.NaN()}, nil
		}
		return outcome{}, err
	}
	o := outcome{ok: true, safeOK: true, util: math.Inf(-1), changes: map[string]float64{}}
	before := map[string]float64{}
	for _, en := range base.Entries {
		before[en.Name] = en.BeforeMs
	}
	var mb report.ModelBand
	if b.Policy != nil {
		mb = b.Policy.ModelBand.ModelBand()
	}
	fail := func(binding, entry string) {
		if o.ok {
			o.binding, o.bindingEntry = binding, entry
		}
		o.ok, o.safeOK = false, false
	}
	for _, en := range r.Entries {
		b0, known := before[en.Name]
		if !known {
			continue
		}
		var c float64
		switch {
		case it.Kind == KindLoadScale:
			c = en.BeforeMs - b0
		case en.ChangeMs != nil:
			c = en.ChangeMs.Lo
		default:
			c = math.NaN()
		}
		if !isFinite(c) {
			fail(BindingLimit, "")
			continue
		}
		o.changes[en.Name] = c
		if b.Policy == nil {
			continue
		}
		t, key := b.Policy.Threshold(b0)
		if c > t {
			fail(key, en.Name)
			continue
		}
		if c+mb.Ms(c, b0) > t {
			o.safeOK = false
		}
	}
	maxAfter := gate.DefaultMaxUtilizationAfter
	ceiling := gate.DefaultMaxUtilizationValidated
	if b.Policy != nil {
		maxAfter, ceiling = b.Policy.MaxUtilizationAfter, b.Policy.MaxUtilizationValidated
	}
	baseUtil := map[string]float64{}
	for _, row := range base.Rows {
		baseUtil[row.Name] = row.Util
	}
	for _, row := range r.Rows {
		if !Budgetable(row) {
			continue
		}
		u := row.Util
		if it.Kind == KindOwnMs {
			if row.UtilizationAfter == nil {
				continue
			}
			u = row.UtilizationAfter.Hi
		}
		u0 := baseUtil[row.Name]
		if row.Name == it.Service || (it.Kind == KindLoadScale && !(u <= o.util)) {
			o.util = u
		}
		switch {
		case !(u <= b.maxRho()) || u >= 1:
			fail(BindingLimit, "")
		case u > maxAfter && u > u0:
			fail(BindingUtilAfter, "")
		case it.Kind == KindLoadScale && u > ceiling && u > u0:
			fail(BindingUtilCeiling, "")
		}
	}
	return o, nil
}

// search finds an item's Max and Safe by bisection between 0 (or a load factor of 1) and a bracket past the
// utilization limit. Each search makes at most MaxSolves solves, its bracket included; the safe search starts from
// what the first one learned.
func (b *Budgeter) search(it *Item, base *report.V2) error {
	cache := map[float64]outcome{}
	eval := func(x float64, n *int) (outcome, error) {
		if o, ok := cache[x]; ok {
			return o, nil
		}
		*n++
		o, err := b.at(it, base, x)
		if err == nil {
			cache[x] = o
		}
		return o, err
	}
	tol := func(lo, hi float64) bool { return hi-lo <= math.Max(1e-4, 1e-6*math.Abs(hi)) }
	start := 0.0
	if it.Kind == KindLoadScale {
		start = 1
	}

	// x_max: the bracket is where the service (or, for a load factor, the busiest service) reaches the utilization
	// limit, as utilization grows in proportion to own time and to load; it is doubled while it is still feasible
	n := 0
	lo, hi := start, b.bracket(it, base)
	top, err := eval(hi, &n)
	for err == nil && top.ok {
		if n >= MaxSolves/2 {
			it.Binding = BindingNone // feasible as far as the search goes: no limit below the cap
			return nil
		}
		lo, hi = hi, hi+2*(hi-start)
		top, err = eval(hi, &n)
	}
	if err != nil {
		return err
	}
	bind := top
	for n < MaxSolves && !tol(lo, hi) {
		mid := (lo + hi) / 2
		o, err := eval(mid, &n)
		if err != nil {
			return err
		}
		if o.ok {
			lo = mid
		} else {
			hi, bind = mid, o
		}
	}
	it.Binding, it.BindingEntry = bind.binding, bind.bindingEntry
	it.Max = floorTo(lo, it.Kind)
	// the values at the limit are those of the largest feasible point solved, within the search's resolution of Max
	if o, ok := cache[lo]; ok {
		it.AtLimit = &AtLimit{EntryChangeMs: finite(o.change(it.BindingEntry)), UtilizationAfter: finite(o.util)}
	} else {
		it.AtLimit = &AtLimit{EntryChangeMs: finite(0), UtilizationAfter: finite(baseUtil(it, base))}
	}

	// the safe budget: none when there is no threshold, or when the band's floor alone is over a threshold
	if b.Policy == nil {
		return nil
	}
	mb := b.Policy.ModelBand.ModelBand()
	for _, en := range base.Entries {
		if t, _ := b.Policy.Threshold(en.BeforeMs); mb.Ms(0, en.BeforeMs) > t {
			return nil
		}
	}
	slo, shi := start, hi
	for x, o := range cache {
		if o.safeOK && x > slo && x < shi {
			slo = x
		}
		if !o.safeOK && x < shi && x > slo {
			shi = x
		}
	}
	for x, o := range cache { // a second pass: an unsafe point below a safe one cannot be (monotonicity)
		if !o.safeOK && x > slo && x < shi {
			shi = x
		}
	}
	n = 0
	for n < MaxSolves && !tol(slo, shi) {
		mid := (slo + shi) / 2
		o, err := eval(mid, &n)
		if err != nil {
			return err
		}
		if o.safeOK {
			slo = mid
		} else {
			shi = mid
		}
	}
	it.Safe = floorTo(slo, it.Kind)
	if *it.Safe > *it.Max {
		it.Safe = it.Max // the safe budget is never above x_max
	}
	return nil
}

// baseUtil is the utilization at no change: the service's, or the busiest one's for a load factor.
func baseUtil(it *Item, base *report.V2) float64 {
	if it.Kind == KindLoadScale {
		return base.Load.MaxUtilizationBefore
	}
	for _, row := range base.Rows {
		if row.Name == it.Service {
			return row.Util
		}
	}
	return math.NaN()
}

// bracket is the first upper end of a search: past the point where utilization reaches the limit, which no budget
// can pass.
func (b *Budgeter) bracket(it *Item, base *report.V2) float64 {
	limit := b.maxRho()
	if it.Kind == KindLoadScale {
		if mr := base.Load.MaxUtilizationBefore; mr > 0 {
			return limit / mr * 1.0001
		}
		return 4
	}
	for _, row := range base.Rows {
		if row.Name == it.Service && row.Util > 0 && row.OwnMs > 0 {
			return row.OwnMs*(limit/row.Util-1)*1.0001 + 1e-3
		}
	}
	x := 10.0
	if b.Policy != nil {
		for _, en := range base.Entries {
			if t, _ := b.Policy.Threshold(en.BeforeMs); 2*t > x && it.Leverage > 0 {
				x = 2 * t / it.Leverage
			}
		}
	}
	return x
}

// floorTo rounds a value found by bisection down to the search's resolution, so the reported value still satisfies
// the constraints: 1e-4 ms of own time, or 1e-4 of a load factor.
func floorTo(x float64, kind string) *float64 {
	v := math.Floor(x*1e4) / 1e4
	if kind == KindLoadScale && v < 1 {
		v = 1
	}
	return &v
}

func finite(v float64) *float64 {
	if !isFinite(v) {
		return nil
	}
	return &v
}

func isFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func sanitizeName(s string) string {
	n, _ := sanitize.Name(s)
	return n
}
