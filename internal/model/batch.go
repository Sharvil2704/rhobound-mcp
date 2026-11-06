// SPDX-License-Identifier: Apache-2.0

package model

import (
	"math"

	"github.com/Sharvil2704/rhobound-mcp/internal/mathx"
)

// Batch arrivals. A parallel node that calls one child k >= 2 times sends that child a batch: k calls that arrive
// together and queue behind each other. The node (a fit's stage node, or any parallel service) waits for the last of
// them. Such a child is "batch-fed" (when its queue is one pool: pooled, or a single replica, without replica shares).
// The model then takes its arrivals as compound Poisson (Poisson epochs, one batch per epoch; calls from anywhere
// else are batches of 1) into its c workers, an M^X/G/c queue (batchq.go), instead of Poisson calls with a ca2 near 1.
// Three numbers come out of it, one per kind of caller:
//   - R, the residence over all its visits (s + the mean wait over all calls): the service's row;
//   - R0, the residence of a call that arrives alone (s + the wait the epoch finds): what a caller of one call waits;
//   - per batch size k, the makespan: from the epoch to the last member's end of service. The node's latency is the
//     call's hop plus that plus the members' work after their worker is freed (their own calls and glue), of which the
//     batch waits for the latest (postExtra).
//
// A service whose callers send it no batch keeps every number it had.

// batchGroup is k calls of a parallel node to one batch-fed child.
type batchGroup struct {
	child, k int
	hop      float64
}

// BatchModelled reports whether the model takes calls sent to service i at once as batch arrivals (with cfg's
// Batch refinement): its queue is one pool.
func BatchModelled(t *Topology, i int) bool {
	sv := &t.Services[i]
	return !sv.Delay && sv.Shares == nil && (sv.Routing == Pooled || sv.Servers == 1)
}

// batchGroupsOf returns per service its batch groups (nil for most): the children its parallel calls reach two or
// more times, in the order of their first call, when the model takes them as batches.
func batchGroupsOf(t *Topology) [][]batchGroup {
	var out [][]batchGroup
	for i := range t.Services {
		sv := &t.Services[i]
		calls := sv.SyncCalls()
		if !sv.Parallel || len(calls) < 2 {
			continue
		}
		var gs []batchGroup
		for x, c := range calls {
			seen := false
			for _, e := range calls[:x] {
				if e.Child == c.Child {
					seen = true
					break
				}
			}
			if seen {
				continue
			}
			n, hop := 0, 0.0
			for _, e := range calls[x:] {
				if e.Child == c.Child {
					n++
					hop += t.CallHop(e)
				}
			}
			if n >= 2 && BatchModelled(t, c.Child) {
				gs = append(gs, batchGroup{child: c.Child, k: n, hop: hop / float64(n)})
			}
		}
		if gs != nil {
			if out == nil {
				out = make([][]batchGroup, len(t.Services))
			}
			out[i] = gs
		}
	}
	return out
}

// HasBatches reports whether the model takes some calls of the topology as batch arrivals.
func HasBatches(t *Topology) bool { return batchGroupsOf(t) != nil }

// IsBatchNode reports whether node i is a parallel node whose calls are all one batch to one child: the model's
// latency for it is that batch's (with the Batch refinement).
func IsBatchNode(t *Topology, i int) bool {
	gs := batchGroupsOf(t)
	if gs == nil || len(gs[i]) != 1 {
		return false
	}
	for _, c := range t.Services[i].SyncCalls() {
		if c.Child != gs[i][0].child {
			return false
		}
	}
	return true
}

// batchState is the solved batch term of one solution of the network.
type batchState struct {
	groups [][]batchGroup // per node
	fed    []bool
	r0, v0 []float64   // fed: a single call's residence, mean and variance
	ms     [][]float64 // fed: makespan by batch size, mean
	ms2    [][]float64 // fed: second moment
	delta  []float64   // fed: s/c, the mean spacing of the members' ends of service
}

// solveBatches solves every batch-fed service's queue at the solution's rates and service times s, and sets its
// residence res.R to the mean over all its visits. It returns nil when the topology has no batch.
func solveBatches(t *Topology, res *Result, s []float64) (*batchState, error) {
	groups := batchGroupsOf(t)
	if groups == nil {
		return nil, nil
	}
	n := len(t.Services)
	b := &batchState{groups: groups, fed: make([]bool, n), r0: make([]float64, n), v0: make([]float64, n),
		ms: make([][]float64, n), ms2: make([][]float64, n), delta: make([]float64, n)}
	for _, gs := range groups {
		for _, g := range gs {
			b.fed[g.child] = true
		}
	}
	for i := range t.Services {
		if !b.fed[i] {
			continue
		}
		sv := &t.Services[i]
		kmax := 1
		for _, gs := range groups {
			for _, g := range gs {
				if g.child == i {
					kmax = max(kmax, g.k)
				}
			}
		}
		rate := make([]float64, kmax+1) // batch epochs per ms by size
		inBatches := 0.0
		for node, gs := range groups {
			for _, g := range gs {
				if g.child == i {
					rate[g.k] += res.Lam[node]
					inBatches += float64(g.k) * res.Lam[node]
				}
			}
		}
		if single := res.Lam[i] - inBatches; single > 1e-12*res.Lam[i] {
			rate[1] += single
		}
		total := 0.0
		for _, r := range rate {
			total += r
		}
		q := make([]float64, kmax+1)
		var sizes []int
		for k, r := range rate {
			if r > 0 {
				q[k] = r / total
				sizes = append(sizes, k)
			}
		}
		if total == 0 { // no traffic: every size at zero load
			q[kmax] = 1
		}
		for _, gs := range groups {
			for _, g := range gs {
				if g.child == i {
					sizes = append(sizes, g.k)
				}
			}
		}
		sizes = append(sizes, 1)
		c := sv.Servers * sv.Workers
		qq := &mxq{c: c, s: s[i], cs2: math.Max(sv.CS2, batchMinCS2), lam: total, q: q}
		var sol *mxqSol
		if s[i] > 0 {
			var err error
			if sol, err = qq.solve(sizes); err != nil {
				return nil, err
			}
		} else {
			sol = &mxqSol{MS: make([]float64, kmax+1), MSsq: make([]float64, kmax+1)}
		}
		res.R[i] = s[i] + sol.Wbar
		b.r0[i] = s[i] + sol.W0
		b.v0[i] = math.Max(sol.W0sq-sol.W0*sol.W0, 0) + sv.CS2*s[i]*s[i]
		b.ms[i], b.ms2[i] = sol.MS, sol.MSsq
		b.delta[i] = s[i] / float64(c)
	}
	return b, nil
}

// postLaw is the time a call spends after its worker is freed (its own calls and its glue): P = g + Q, where Q is 0
// with probability p0 (no call made) and otherwise a gamma of mean m and variance v.
type postLaw struct{ g, p0, m, v float64 }

// postLawOf matches a postLaw to the mean and variance of service i's work after its worker is freed.
func postLawOf(t *Topology, i int, mean, variance float64) postLaw {
	sv := &t.Services[i]
	g := sv.Glue
	calls := sv.SyncCalls()
	if len(calls) == 0 || !(mean-g > 1e-12) {
		return postLaw{g: math.Max(mean, 0), p0: 1}
	}
	p0 := 0.0
	if !sv.Parallel {
		p0 = 1
		for _, c := range calls {
			p0 *= 1 - c.P
		}
	}
	mq := (mean - g) / (1 - p0) // Q given a call
	varQ := math.Max(variance-sv.GlueCS2*g*g, 0)
	// Var(Q) = (1-p0)(v + mq^2) - ((1-p0) mq)^2
	v := (varQ+(1-p0)*(1-p0)*mq*mq)/(1-p0) - mq*mq
	return postLaw{g: g, p0: p0, m: mq, v: math.Max(v, 1e-9*mq*mq)}
}

// cdf is P(P <= y).
func (p postLaw) cdf(y float64) float64 {
	if y < p.g {
		return 0
	}
	if p.p0 >= 1 {
		return 1
	}
	k := p.m * p.m / p.v
	return p.p0 + (1-p.p0)*mathx.GammaP(k, (y-p.g)*p.m/p.v)
}

// postExtra is how much later than its last end of service a batch of k ends once each member's post time is added:
// E[max_j (C_j + P_j)] - C_max, with the members' ends of service delta apart (the last at C_max) and the P_j
// independent. For k = 1 it is E[P].
func postExtra(k int, delta float64, p postLaw) float64 {
	if p.p0 >= 1 {
		return p.g
	}
	sd := math.Sqrt(p.v)
	ymax := p.g + p.m + 15*sd + 30*p.v/p.m
	// breakpoints: where a member's atom at g enters, g - i delta
	edges := []float64{0}
	for i := k - 1; i >= 0; i-- {
		if e := p.g - float64(i)*delta; e > 0 && e < ymax {
			edges = append(edges, e)
		}
	}
	h := math.Max(math.Min(sd, p.m)/4, ymax/2000)
	for x := edges[len(edges)-1] + h; x < ymax; x += h {
		edges = append(edges, x)
	}
	edges = append(edges, ymax)
	out := 0.0
	for e := 0; e+1 < len(edges); e++ {
		lo, hi := edges[e], edges[e+1]
		if !(hi > lo) {
			continue
		}
		for q := range glX8 {
			y := lo + (hi-lo)*(glX8[q]+1)/2
			f := 1.0
			for i := 0; i < k; i++ {
				f *= p.cdf(y + float64(i)*delta)
			}
			out += glW8[q] * (hi - lo) / 2 * (1 - f)
		}
	}
	return out
}

// groupMoments is the latency of a batch group of node's calls: the call's hop, the child's makespan for k calls
// and the latest of the members' post times; mean and variance (the makespan's, plus one post time's).
func (b *batchState) groupMoments(t *Topology, g batchGroup, l, vl []float64) (float64, float64) {
	pm := l[g.child] - b.r0[g.child]
	pv := math.Max(vl[g.child]-b.v0[g.child], 0)
	law := postLawOf(t, g.child, pm, pv)
	m := b.ms[g.child][g.k]
	v := math.Max(b.ms2[g.child][g.k]-m*m, 0)
	return g.hop + m + postExtra(g.k, b.delta[g.child], law), v + pv
}

// BatchPath reports per service whether the batch term can change its numbers (with the Batch refinement): a
// batch-fed service, a node with a batch group, and every service that waits, through calls, for one of them. Every
// other service's residence and latency are what the model without the term gives, bit for bit.
func BatchPath(t *Topology) []bool {
	on := make([]bool, len(t.Services))
	groups := batchGroupsOf(t)
	if groups == nil {
		return on
	}
	for i, gs := range groups {
		if gs != nil {
			on[i] = true
		}
		for _, g := range gs {
			on[g.child] = true
		}
	}
	for k := len(t.order) - 1; k >= 0; k-- {
		i := t.order[k]
		for _, c := range t.Services[i].SyncCalls() {
			if on[c.Child] {
				on[i] = true
			}
		}
	}
	return on
}

// BatchFed reports per service whether the model (with the Batch refinement) takes its arrivals as batches: some
// parallel node calls it two or more times at once.
func BatchFed(t *Topology) []bool {
	fed := make([]bool, len(t.Services))
	for _, gs := range batchGroupsOf(t) {
		for _, g := range gs {
			fed[g.child] = true
		}
	}
	return fed
}
