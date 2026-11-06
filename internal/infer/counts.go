// SPDX-License-Identifier: Apache-2.0

package infer

// Calls per visit that vary, and visits that take several call shapes.
//
// The model has, for each callee of a service, a fixed number of calls per visit
// or one more call on some visits (callCount), and for how the calls run:
// one after another, all at once, or in stages (structure.go). Real services
// break both. A cart service makes one Redis call to read a cart, three to add
// to it and two to empty it; a web front end serves a dozen endpoints, one
// calling the cart twice in a row, another a recommendation service and then
// four catalogue lookups at once. The count of calls, and the shape, is a
// property of the request type, so it varies from visit to visit and the
// service-level histogram is only the mix of request types.
//
// Three rules take such a service, each only where the rules in structure.go
// would refuse it; what they accept before is read as before.
//
//  1. A callee whose calls per visit vary, called by a service that makes its
//     calls one after another, enters with the mean count: k calls on every
//     visit and one more with probability p (the form the async calls use).
//     The load on the callee is then exact, and so is the caller's mean time, which
//     is the sum of its calls' times. What is lost is the variability of the count.
//  2. When the stage template of the caller carries the visits, the stages
//     carry the count (a stage that runs on some visits only).
//  3. Otherwise the visits take several call shapes, and the service is a mixture:
//     it calls each shape with the share of its visits that took that shape, and
//     a shape is a call, a group of calls at once (a zero-time parallel node)
//     or several of these after one another (a zero-time node that calls its stages).
//     At most maxShapes shapes are kept, the rarer ones may hold shapeTail of the
//     calling visits at most and enter as mean counts, one after another. A callee
//     whose calls the shapes do not account for (calls the shapes leave out)
//     is topped up with mean-count calls, so that the load on every callee is
//     the observed one.
//
// What none of them models is that calls made together reach the callee together
// (batchWarnings says when that matters).

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

const (
	maxShapes     = 12   // call shapes of one service that a mixture keeps
	shapeTail     = 0.05 // share of the calling visits that the rarer shapes may hold
	residualWarn  = 0.01 // warn when the shapes leave out more than this share of a callee's calls
	residualFloor = 1e-9 // calls per visit below which a difference is rounding, not calls
	maxCountsText = 10   // distinct counts a warning lists
	batchMinShare = 0.01 // a group of calls at once that fewer visits than this make is not checked for batch arrivals
	batchWarnLoad = 0.15 // warn of batch arrivals when the callee's utilization times the batch, over its workers, reaches this
	batchLowerMin = 3    // a callee whose worker count is a lower bound is warned of at any load only for a batch of this many or more
)

// How a callee's varying count is modelled (VariableCall.ModelledAs).
const (
	ModelledMean    = "mean count"
	ModelledStages  = "stages"
	ModelledMixture = "mixture of call shapes"
)

// CallsVisits is a number of calls per visit and the visits that made that many.
type CallsVisits struct {
	Calls  int   `json:"calls"`
	Visits int64 `json:"visits"`
}

// VariableCall is a callee that a service calls a varying number of times per
// visit (neither a fixed count nor a fixed count and one more on some visits).
type VariableCall struct {
	Callee        string        `json:"callee"`
	MeanPerVisit  float64       `json:"mean_per_visit"`
	VisitsByCalls []CallsVisits `json:"visits_by_calls"` // by calls per visit, ascending; visits with none included
	ModelledAs    string        `json:"modelled_as"`     // ModelledMean, ModelledStages or ModelledMixture
}

// CallShape is one shape of the calls a service's visits take, in words (stages one after another joined
// with "then", calls at once with "+"), and the share of the service's visits that took it.
type CallShape struct {
	Shape string  `json:"shape"`
	Share float64 `json:"share"`
}

// visitsByCalls turns a histogram (calls -> visits, for the visits that made at
// least one) into calls -> visits over all nsp visits, ascending, zero included.
func visitsByCalls(hist map[int]int64, nsp int64) []CallsVisits {
	var out []CallsVisits
	var seen int64
	for n, v := range hist {
		if v > 0 {
			out = append(out, CallsVisits{n, v})
			seen += v
		}
	}
	if z := nsp - seen; z > 0 {
		out = append(out, CallsVisits{0, z})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Calls < out[j].Calls })
	return out
}

// countsText writes calls -> visits as "1: 3098, 2: 559, 3: 1731", listing at
// most maxCountsText counts.
func countsText(vc []CallsVisits) string {
	var parts []string
	for i, x := range vc {
		if i == maxCountsText {
			parts = append(parts, fmt.Sprintf("... %d more counts, up to %d", len(vc)-i, vc[len(vc)-1].Calls))
			break
		}
		parts = append(parts, fmt.Sprintf("%d: %d", x.Calls, x.Visits))
	}
	return strings.Join(parts, ", ")
}

// histMean is the mean number of calls per visit from hist (calls -> visits, for the visits that made at
// least one) over nsp visits.
func histMean(hist map[int]int64, nsp int64) float64 {
	sum := 0.0
	for n, v := range hist {
		sum += float64(n) * float64(v)
	}
	return sum / float64(nsp)
}

// meanOfCounts is the mean number of calls per visit over nsp visits.
func meanOfCounts(vc []CallsVisits, nsp int64) float64 {
	sum := 0.0
	for _, x := range vc {
		sum += float64(x.Calls) * float64(x.Visits)
	}
	return sum / float64(nsp)
}

// meanCount splits a mean number of calls into k that are made on every visit
// and one made with probability p, the form async calls already take.
func meanCount(mean float64) (k int, p float64) {
	k = int(math.Floor(mean + 1e-12))
	if p = mean - float64(k); p < 1e-12 {
		p = 0
	}
	return k, p
}

// meanCalls are the calls of a mean count: k with probability 1, one with p.
func meanCalls(child int, mean float64) []model.Call {
	k, p := meanCount(mean)
	var out []model.Call
	for j := 0; j < k; j++ {
		out = append(out, model.Call{Child: child, P: 1})
	}
	if p > 0 {
		out = append(out, model.Call{Child: child, P: p})
	}
	return out
}

// varyEdge is a callee whose calls per visit callCount refused.
type varyEdge struct {
	child   int // index in the topology
	name    string
	mean    float64
	maxN    int
	byCalls []CallsVisits
}

func newVaryEdge(child int, name string, hist map[int]int64, nsp int64) varyEdge {
	vc := visitsByCalls(hist, nsp)
	return varyEdge{child: child, name: name, mean: meanOfCounts(vc, nsp), maxN: vc[len(vc)-1].Calls, byCalls: vc}
}

// meanRuleWarning is the warning for a callee whose varying count enters as its mean. The explanation of what that
// does (and does not) model is long and the same for every such callee, so it is given in full once (full) and
// the later warnings point back to it.
func meanRuleWarning(caller string, v varyEdge, full bool) string {
	k, p := meanCount(v.mean)
	how := fmt.Sprintf("%d call(s) made on every visit", k)
	if p > 0 {
		how += fmt.Sprintf(" and one more with probability %.2f", p)
	}
	head := fmt.Sprintf("%s -> %s: calls per visit vary as {%s}; only their mean (%.3g per visit) enters the model, as %s.",
		caller, v.name, countsText(v.byCalls), v.mean, how)
	if !full {
		return head + " As for the callees above, that is exact for the load and for the mean time of a caller that makes its " +
			"calls one after another, and the spread of the counts is not modelled."
	}
	return head + fmt.Sprintf(" That is exact for the load on %s and for the mean time of a caller that makes its calls one after "+
		"another; the spread of the counts is not modelled (the calls of one visit reach %s close together, and the model takes "+
		"them as independent arrivals), which matters only when %s is busy (in simulations with up to 20 calls per visit and %s "+
		"30-60%% busy the mean latency was within 2%% on average). Calls lost from the capture, or sampled out, look the same "+
		"as a count that varies: if the count should be fixed, check the exporters", v.name, v.name, v.name, v.name)
}

// shapeBuilder adds the nodes of a mixture of call shapes to the topology.
type shapeBuilder struct {
	topo   *model.Topology
	stats  *[]ServiceStats
	si     int // the caller's index
	name   string
	caller int32
	nsp    int64
	idx    map[int32]int
	pairs  map[pairKey]*pairAcc
	owner  map[int]string // node index -> the service whose calls it holds
}

// sigCount is a call shape (a stageSig key) and the visits that took it.
type sigCount struct {
	key string
	n   int64
}

// rankedShapes lists the shapes by visits, most first, ties by key.
func rankedShapes(sigs map[string]int64) (all []sigCount, total int64) {
	for k, n := range sigs {
		if k != "" && n > 0 {
			all = append(all, sigCount{k, n})
			total += n
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].key < all[j].key
	})
	return all, total
}

// describeShape writes a shape with service names: stages one after another
// ("then"), calls at once joined with "+", a callee called m times at once as
// "name x m".
func describeShape(sig stageSig, stats []ServiceStats, idx map[int32]int) string {
	var parts []string
	for _, g := range sig {
		cnt := map[string]int{}
		var order []string
		for _, id := range g {
			nm := "?"
			if ci, ok := idx[id]; ok {
				nm = stats[ci].Name
			}
			if cnt[nm] == 0 {
				order = append(order, nm)
			}
			cnt[nm]++
		}
		var xs []string
		for _, nm := range order {
			if cnt[nm] > 1 {
				xs = append(xs, fmt.Sprintf("%s x%d", nm, cnt[nm]))
			} else {
				xs = append(xs, nm)
			}
		}
		parts = append(parts, strings.Join(xs, " + "))
	}
	return strings.Join(parts, " then ")
}

// node appends a zero-time node (a stage, or a shape of several stages) of the
// caller to the topology and returns its index.
func (b *shapeBuilder) node(sv model.Service, st ServiceStats) int {
	b.topo.Services = append(b.topo.Services, sv)
	*b.stats = append(*b.stats, st)
	ix := len(b.topo.Services) - 1
	b.owner[ix] = b.name
	return ix
}

// mixtureResult is what mixtureCalls returns.
type mixtureResult struct {
	calls    []model.Call
	shapes   []CallShape     // the kept shapes
	tail     float64         // share of the calling visits in the shapes left out
	expected map[int]float64 // callee (topology index) -> calls per visit the calls make
	nodes    []int           // the parallel nodes (groups of calls at once) and the share of visits calling each
	// shapeKeys are the kept shapes' signatures and shapeCall the index in calls of each one's call (-1: none):
	// how a route's visits split over the shapes becomes its chance of each call (routeCalls).
	shapeKeys []string
	shapeCall []int
}

// mixtureCalls represents a service whose visits take several call shapes as a
// mixture: it calls each shape with the share of its visits that took it. A shape
// of one call is that call, a shape of one stage of several calls is a
// zero-time parallel node, any other shape a zero-time node that calls its stages
// one after another. Every parallel node has all its members on every call, with
// probability 1: the probability sits on the call to the node, which the model
// accepts for a fan-out. The shapes beyond the commonest maxShapes enter as mean
// counts, one after another; ok is false when they hold more than shapeTail of the
// calling visits.
func (b *shapeBuilder) mixtureCalls(sigs map[string]int64) (res mixtureResult, ok bool) {
	all, total := rankedShapes(sigs)
	keep := all[:min(len(all), maxShapes)]
	var tailN int64
	for _, x := range all[len(keep):] {
		tailN += x.n
	}
	if total == 0 || float64(tailN) > shapeTail*float64(total) {
		return mixtureResult{tail: float64(tailN) / math.Max(float64(total), 1)}, false
	}
	res.expected = map[int]float64{}
	rate, vis := (*b.stats)[b.si].RateRPS, (*b.stats)[b.si].Visits
	// a group of calls at once: the call itself when it is one, else a zero-time parallel node
	group := func(g []int32, label string, q float64) (int, bool) {
		var members []model.Call
		for _, id := range g {
			if ci, ok := b.idx[id]; ok {
				members = append(members, model.Call{Child: ci, P: 1})
				res.expected[ci] += q
			}
		}
		if len(members) == 0 {
			return 0, false
		}
		if len(members) == 1 {
			return members[0].Child, true
		}
		r, _ := pairCorrelation(b.pairs[pairKey{b.caller, len(g), stageSig{g}.key()}], len(g))
		r = math.Max(0, math.Min(r, 0.95))
		ix := b.node(model.Service{Name: label, CS2: 1, Delay: true, Parallel: true, Calls: members, Servers: 1, Workers: 1,
			Routing: model.Pooled, BranchCorr: r},
			ServiceStats{Name: label, Stage: true, Visits: int64(math.Round(float64(vis) * q)), RateRPS: rate * q, Pods: 1,
				WorkersPerPod: 1, ParallelShare: 1, BranchCorr: r})
		res.nodes = append(res.nodes, ix)
		return ix, true
	}
	for j, x := range keep {
		q := float64(x.n) / float64(b.nsp)
		sig := parseSig(x.key)
		res.shapes = append(res.shapes, CallShape{describeShape(sig, *b.stats, b.idx), q})
		res.shapeKeys = append(res.shapeKeys, x.key)
		res.shapeCall = append(res.shapeCall, -1)
		var inner []model.Call
		for s, g := range sig {
			if ix, ok := group(g, fmt.Sprintf("stage:%s:%d.%d", b.name, j+1, s+1), q); ok {
				inner = append(inner, model.Call{Child: ix, P: 1})
			}
		}
		switch len(inner) {
		case 0:
		case 1:
			res.shapeCall[j] = len(res.calls)
			res.calls = append(res.calls, model.Call{Child: inner[0].Child, P: q})
		default:
			lbl := fmt.Sprintf("shape:%s:%d", b.name, j+1)
			ix := b.node(model.Service{Name: lbl, CS2: 1, Delay: true, Calls: inner, Servers: 1, Workers: 1, Routing: model.Pooled},
				ServiceStats{Name: lbl, Stage: true, Visits: int64(math.Round(float64(vis) * q)), RateRPS: rate * q, Pods: 1,
					WorkersPerPod: 1, Stages: len(inner)})
			res.shapeCall[j] = len(res.calls)
			res.calls = append(res.calls, model.Call{Child: ix, P: q})
		}
	}
	// the rarer shapes: their calls as mean counts, one after another
	if tailN > 0 {
		cnt := map[int32]int64{}
		for _, x := range all[len(keep):] {
			for _, g := range parseSig(x.key) {
				for _, id := range g {
					cnt[id] += x.n
				}
			}
		}
		ids := make([]int32, 0, len(cnt))
		for id := range cnt {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			if ci, ok := b.idx[id]; ok {
				m := float64(cnt[id]) / float64(b.nsp)
				res.calls = append(res.calls, meanCalls(ci, m)...)
				res.expected[ci] += m
			}
		}
	}
	res.tail = float64(tailN) / float64(total)
	return res, true
}

// callTotals are a callee's observed calls per visit, and the calls per visit the model makes to it.
type callTotals struct {
	child          int
	observed, have float64
}

// topUp returns the mean-count calls that bring each callee's modelled calls
// per visit (expected) up to the calls per visit the visits made (observed,
// by callee in the order given), and the callees that fall short by more than
// residualWarn of what they were called. The shapes read only the calls
// that start within the visit's span; the histograms count every call the visit
// waits for.
func topUp(observed []callTotals) (calls []model.Call, big []callTotals) {
	for _, s := range observed {
		r := s.observed - s.have
		if r <= residualFloor*math.Max(1, s.observed) {
			continue
		}
		calls = append(calls, meanCalls(s.child, r)...)
		if r > residualWarn*s.observed {
			big = append(big, callTotals{s.child, s.observed, s.have})
		}
	}
	return calls, big
}

// batchWarnings warns of a parallel node of a service that calls one callee m
// times at once, when the callee may be busy enough for that to matter. The m
// calls reach the callee together, and the model takes them as independent
// arrivals, so the wait it computes is shorter than the real one: a group's
// calls queue behind each other, and that wait does not shrink with load. In
// simulations (batches of 2 and 4 calls at once, callees of 1 to 8 workers at 10%
// to 60% utilization) the callee's mean latency was understated by 0.1% to 60%:
// the more the fewer its workers and the busier it is, and about 55% of a callee
// with one worker, whatever its load. The check is: the batch is larger than the
// callee's workers (when the count is a lower bound, only for a batch of
// batchLowerMin or more: a pair of calls at once to a callee seen with one
// worker is as likely two workers that were never both needed, and the wait is
// a single service time at the most), or utilization times the batch over the
// workers reaches batchWarnLoad. nodes are the parallel nodes to
// check, owner names the service whose calls a node holds, and share is the share
// of that service's visits that make the calls (several nodes of one service that
// call one callee the same number of times at once are one warning, with their
// shares added; groups under batchMinShare are left out).
//
// When modelled(callee) holds, the model takes the group as batch arrivals (model.Config.Batch): the warning is then a
// note of how it does, and coded is false; otherwise coded is true (model.batch_arrivals).
func batchWarnings(topo *model.Topology, stats []ServiceStats, nodes []int, owner map[int]string, modelled func(callee int) bool,
	warn func(coded bool, f string, a ...any)) {
	type group struct {
		owner string
		ci, n int
		share float64
	}
	var groups []group
	at := map[[3]int]int{} // (owner's index, callee, n) -> index in groups
	for _, ni := range nodes {
		sv := &topo.Services[ni]
		share := 1.0
		o := topo.Index(owner[ni])
		if o >= 0 && stats[o].RateRPS > 0 {
			share = stats[ni].RateRPS / stats[o].RateRPS
		}
		m := map[int]int{}
		var order []int
		for _, c := range sv.SyncCalls() {
			if m[c.Child] == 0 {
				order = append(order, c.Child)
			}
			m[c.Child]++
		}
		for _, ci := range order {
			if n := m[ci]; n >= 2 && !topo.Services[ci].Delay {
				key := [3]int{o, ci, n}
				gi, ok := at[key]
				if !ok {
					gi = len(groups)
					at[key] = gi
					groups = append(groups, group{owner: owner[ni], ci: ci, n: n})
				}
				groups[gi].share += share
			}
		}
	}
	for _, g := range groups {
		if g.share < batchMinShare {
			continue
		}
		ch := &topo.Services[g.ci]
		c := float64(ch.Servers * ch.Workers)
		lower := stats[g.ci].WorkersInferred && !stats[g.ci].WorkersFromQueueing && !stats[g.ci].External
		rho := stats[g.ci].RateRPS / 1000 * ch.S / c
		if rho*float64(g.n)/c < batchWarnLoad && (float64(g.n) <= c || (lower && g.n < batchLowerMin)) {
			continue
		}
		workers := fmt.Sprintf("%.0f workers", c)
		if c == 1 {
			workers = "1 worker"
		}
		if lower {
			workers = "at least " + workers
		}
		extra := ""
		if float64(g.n) > c {
			extra = "; a group larger than the workers waits for itself at any load"
		}
		if modelled(g.ci) {
			warn(false, "%s calls %s %d times at once (on %.0f%% of its visits); %s has %s and is %.0f%% busy. The model takes "+
				"the %d calls as one batch: %s's arrivals as batches at Poisson epochs (an M^X/G/c queue, calls that arrive "+
				"alone as batches of 1), and the wait for them as the time until the last of them is served", g.owner, ch.Name,
				g.n, 100*math.Min(g.share, 1), ch.Name, workers, 100*rho, g.n, ch.Name)
			continue
		}
		warn(true, "%s calls %s %d times at once (on %.0f%% of its visits); %s has %s and is %.0f%% busy. The model takes the %d calls "+
			"as independent arrivals, but they reach %s together and queue behind each other, so %s's wait, and the latency of "+
			"whoever waits for it, is understated (in simulations by up to 60%% of %s's latency: the more the fewer its workers "+
			"and the busier it is%s)", g.owner, ch.Name, g.n, 100*math.Min(g.share, 1), ch.Name, workers, 100*rho, g.n, ch.Name, ch.Name,
			ch.Name, extra)
	}
}
