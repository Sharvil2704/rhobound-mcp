// SPDX-License-Identifier: Apache-2.0

package model

import (
	"math"

	"github.com/Sharvil2704/rhobound-mcp/internal/mathx"
)

// flow is a stream of calls into a service: its rate and arrival variability.
type flow struct{ rate, scv float64 }

// departureSCV is the squared coefficient of variation of the departures of a
// queue (Whitt's QNA, multi-server form): the arrival variability is kept in
// proportion to 1-rho^2, the service variability to rho^2 (damped by sqrt(c)).
func departureSCV(ca2, cs2, rho float64, c int) float64 {
	return 1 + (1-rho*rho)*(ca2-1) + rho*rho*(cs2-1)/math.Sqrt(float64(c))
}

// arrivalVariability fills res.Ca2: for every service, the squared coefficient
// of variation of the arrivals it sees. External arrivals are Poisson. A call
// made with probability p thins its caller's departure stream (Bernoulli
// splitting: p*cd2 + 1-p), an async call as any other (it loads its child);
// streams into one service merge by Whitt's QNA hybrid of the asymptotic and
// the stationary-interval methods.
func arrivalVariability(t *Topology, lamExt float64, res *Result, s []float64, weight float64) {
	n := len(t.Services)
	in := make([][]flow, n)
	for i, w := range t.Entry {
		in[i] = append(in[i], flow{lamExt * w, 1})
	}
	for _, i := range t.order {
		sv := &t.Services[i]
		// merge what comes in
		fl := in[i]
		tot, sumSq := 0.0, 0.0
		for _, f := range fl {
			tot += f.rate
		}
		ca2 := 1.0
		if tot > 0 {
			asym := 0.0
			for _, f := range fl {
				share := f.rate / tot
				asym += share * f.scv
				sumSq += share * share
			}
			v := 1 / sumSq
			// (with replica shares, Rho is the busiest replica's: its queue dominates the mean)
			w := 1 / (1 + 4*(1-res.Rho[i])*(1-res.Rho[i])*(v-1))
			ca2 = w*asym + (1 - w)
		}
		// what goes out
		cd2 := ca2
		switch {
		case sv.Delay:
		case sv.Shares != nil: // each replica's departures, merged in proportion to their rates
			cd2 = 0
			for _, p := range sv.Shares {
				rho := res.Lam[i] * p * s[i] / float64(sv.Workers)
				cd2 += p * departureSCV(p*ca2+1-p, sv.CS2, rho, sv.Workers)
			}
		default:
			capacity := sv.Servers * sv.Workers
			c, caQ := sv.Workers, ca2
			if sv.Servers > 1 && sv.Routing == Random { // each replica sees a thinned stream
				r := float64(sv.Servers)
				caQ = ca2/r + 1 - 1/r
			} else {
				c = capacity
			}
			cd2 = departureSCV(caQ, sv.CS2, res.Rho[i], c)
		}
		for _, c := range sv.Calls {
			in[c.Child] = append(in[c.Child], flow{res.Lam[i] * c.P, c.P*cd2 + (1 - c.P)})
		}
		res.Ca2[i] = 1 + weight*(ca2-1)
	}
}

// residenceVariance returns the variance of each service's residence: the wait
// is a mixture of zero (with probability 1-pw) and an exponential, whose mean
// makes the mean wait match; the service adds cs2*s^2. With replica shares the
// wait is that mixture per replica, mixed over the replicas by their shares
// (for M/M/1 replicas this is the exact variance).
func residenceVariance(t *Topology, res *Result, s []float64) []float64 {
	v := make([]float64, len(t.Services))
	for i := range t.Services {
		sv := &t.Services[i]
		if sv.Delay {
			v[i] = sv.CS2 * s[i] * s[i]
			continue
		}
		if sv.Shares != nil {
			ew2 := 0.0 // E[wait^2]
			for _, p := range sv.Shares {
				wq := math.Max(replicaResidence(s[i], res.Lam[i], p, sv.Workers, res.Ca2[i], sv.CS2)-s[i], 0)
				if pw := waitChance(sv.Workers, res.Lam[i]*p*s[i]); pw > 0 {
					ew2 += p * 2 * wq * wq / pw
				}
			}
			wq := math.Max(res.R[i]-s[i], 0)
			v[i] = math.Max(ew2-wq*wq, 0) + sv.CS2*s[i]*s[i]
			continue
		}
		wq := math.Max(res.R[i]-s[i], 0)
		pw := waitProbability(sv, s[i], res.Lam[i])
		varW := 0.0
		if pw > 0 {
			varW = math.Max(2*wq*wq/pw-wq*wq, 0)
		}
		v[i] = varW + sv.CS2*s[i]*s[i]
	}
	return v
}

// waitChance is the chance an arrival waits at a queue of c workers offered a
// erlangs.
func waitChance(c int, a float64) float64 {
	if c == 1 {
		return math.Min(a, 1)
	}
	return ErlangC(c, a)
}

// waitProbability is the chance an arrival waits, by the service's routing.
func waitProbability(sv *Service, s, lam float64) float64 {
	if sv.Routing == Random && sv.Servers > 1 {
		lam /= float64(sv.Servers)
		if sv.Workers == 1 {
			return math.Min(lam*s, 1)
		}
		return ErlangC(sv.Workers, lam*s)
	}
	c := sv.Servers * sv.Workers
	if c == 1 {
		return math.Min(lam*s, 1)
	}
	return ErlangC(c, lam*s)
}

// edgeVariance is a child's latency variance as a call on the edge caller -> child sees it: v, changed by edgeVar's
// value for the edge and floored at 0 (v itself with no change).
func edgeVariance(v float64, edgeVar map[Edge]float64, caller, child int) float64 {
	if edgeVar == nil {
		return v
	}
	x, ok := edgeVar[Edge{caller, child}]
	if !ok {
		return v
	}
	return math.Max(0, v+x)
}

// composeMatched is compose with variances: sequential calls add means and
// variances; a parallel fan-out takes the expected maximum over independent
// gamma branches matched to each branch's mean and variance; async calls add
// nothing. The glue adds its mean and its variance, independent of the rest.
// With batches (b, batch.go), a node's batch group is one branch whose latency
// is the batch's (batchState.groupMoments), a call to a batch-fed child that
// arrives alone waits what a single call waits (b.r0), and a batch-fed
// service's returned latency is the mean over all its visits. Edge changes
// (edge, edgeVar: per-route own time, routes.go in infer) add to each call on
// their edge; on a batch group the mean change is added to the group's latency.
func composeMatched(t *Topology, r, v []float64, b *batchState, edge, edgeVar map[Edge]float64) []float64 {
	l := make([]float64, len(t.Services))
	vl := make([]float64, len(t.Services))
	for k := len(t.order) - 1; k >= 0; k-- {
		i := t.order[k]
		sv := &t.Services[i]
		calls := sv.SyncCalls()
		ri, vi := r[i], v[i]
		var groups []batchGroup
		if b != nil {
			if b.fed[i] {
				ri, vi = b.r0[i], b.v0[i]
			}
			groups = b.groups[i]
		}
		switch {
		case len(calls) == 0:
			l[i], vl[i] = ri, vi
		case groups != nil:
			var mu, va []float64
			for _, g := range groups {
				m, mv := b.groupMoments(t, g, l, vl)
				mu, va = append(mu, m+edgeMs(edge, i, g.child)), append(va, mv)
			}
			for _, c := range calls {
				inGroup := false
				for _, g := range groups {
					inGroup = inGroup || g.child == c.Child
				}
				if !inGroup {
					dm, dv := t.RouteAdj(c.Child, c.RouteQ, l, vl, edge, edgeVar)
					mu = append(mu, t.CallHop(c)+l[c.Child]+edgeMs(edge, i, c.Child)+dm)
					va = append(va, math.Max(0, edgeVariance(vl[c.Child], edgeVar, i, c.Child)+dv))
				}
			}
			if len(mu) == 1 {
				l[i], vl[i] = ri+mu[0], vi+va[0]
			} else {
				m, mv := mathx.MaxMomentsGammaCorr(mu, va, sv.BranchCorr)
				l[i], vl[i] = ri+m, vi+mv
			}
		case sv.Parallel && len(calls) > 1:
			mu := make([]float64, len(calls))
			va := make([]float64, len(calls))
			for j, c := range calls {
				dm, dv := t.RouteAdj(c.Child, c.RouteQ, l, vl, edge, edgeVar)
				mu[j] = t.CallHop(c) + l[c.Child] + edgeMs(edge, i, c.Child) + dm
				va[j] = math.Max(0, edgeVariance(vl[c.Child], edgeVar, i, c.Child)+dv)
			}
			m, mv := mathx.MaxMomentsGammaCorr(mu, va, sv.BranchCorr)
			l[i], vl[i] = ri+m, vi+mv
		default:
			sum, vs := 0.0, 0.0
			for _, c := range calls {
				dm, dv := t.RouteAdj(c.Child, c.RouteQ, l, vl, edge, edgeVar)
				m := t.CallHop(c) + l[c.Child] + edgeMs(edge, i, c.Child) + dm
				sum += c.P * m
				vs += c.P*(math.Max(0, edgeVariance(vl[c.Child], edgeVar, i, c.Child)+dv)+m*m) - (c.P*m)*(c.P*m)
			}
			l[i], vl[i] = ri+sum, vi+vs
		}
		l[i] += sv.Glue
		vl[i] += sv.GlueCS2 * sv.Glue * sv.Glue
	}
	if b != nil {
		out := append([]float64(nil), l...)
		for i, fed := range b.fed {
			if fed {
				out[i] = l[i] - b.r0[i] + r[i]
			}
		}
		return out
	}
	return l
}
