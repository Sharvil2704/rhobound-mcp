// SPDX-License-Identifier: Apache-2.0

package model

import (
	"errors"
	"math"
	"math/bits"
	"sync"

	"github.com/Sharvil2704/rhobound-mcp/internal/mathx"
)

// The M^X/G/c queue of a callee that receives batches (batch.go says where they come from). Batch epochs are Poisson
// at rate lam per ms; an epoch brings X calls at once, X ~ q (singles are batches of 1); c workers serve one FIFO
// queue; service times are gamma with mean s and scv cs2. docs: the batch lane's DERIVATION.md.
//
// Which solution (solve): c = 1, the closed M^X/G/1 forms (solveOne, exact for every cs2); c >= 2, the M^X/PH/c queue of
// batchph.go when it fits (up to 32 workers, 200 configurations of busy workers and 600 boundary states, a solve of
// about 0.15 s at most: any cs2 >= 1/2 at up to 32 workers, 1/3 at up to 13, 1/4 at up to 8; at cs2 <= 1 at most 2M
// cells near saturation), exact for its phase law, which is the gamma itself at cs2 = 1, 1/2, 1/4 and has the gamma's
// three moments above 1; else the superposition below. Against a FIFO simulation (maze-kit reports/maze-live/Q1.md)
// the phase-type queue's makespan is within 0.3% at cs2 <= 1 and 0.9% at cs2 2; the superposition's within about 4%
// at cs2 <= 1 and 16% at cs2 2 (c <= 8), 1.6% and 5.5% at 12 and 16 workers.
//
// The superposition ("SUP", desynchronized):
//   - The number in system N that an epoch finds (PASTA: its time-stationary law) is pi = (1-w) pD + w pM, w =
//     min(cs2, 1): pM is the exact M^X/M/c law (cut equations), pD the exact M^X/D/c law (the Crommelin chain
//     N(t+s) = (N(t)-c)^+ + A(s), solved by GTH state reduction).
//   - If N >= c, every worker is busy and the workers' service processes are taken as independent equilibrium
//     (stationary) renewal processes: their phases have drifted apart. Member j of the batch starts at the
//     (N-c+j)-th point T of their superposition. If N < c, c-N workers start the first members at once (they are
//     fresh renewal processes from 0) and the other N are in equilibrium.
//   - E[T_m] is an integral of P(N_sup(t) <= m-1), and each worker's count law has a closed form in the regularized
//     incomplete gamma function: P(N_eq(t) >= j) = (I_{j-1}(t) - I_j(t))/s with I_j = int_0^t F_j, F_j the CDF of a
//     sum of j services; P(N_fresh(t) >= j) = F_j(t).
//   - After the last member starts, the batch ends with the slowest of the members still in service: the last one
//     (a whole service) and the others still running. If N >= c, each other worker is taken to be running a member
//     of the batch, independently, with the chance that its member started within a fixed window; if N < c, every
//     fresh worker is taken to run one (tailQuad). There is no law of which jobs remain (no A of the M^X/M/c chain).
//
// It is exact for c = 1 at cs2 <= 1 (the closed M^X/G/1 forms below are used for every cs2). For exponential services
// at any c the number in system and the waits (pM, W0, Wbar) are exact, but the makespan's tail is an approximation
// at c >= 2: at c = 2, cs2 = 1, k = 2 it gives 2.0 instead of 1.75 in the state N = 1 and 1.1967 instead of 1.25 in the
// busy one, and MS[2] = 1.7550 instead of 1.7222 at rho 0.25, +1.9% (maze-kit lean/Rbproofs/Lanes/Queue.lean, finding
// Q1). solve uses it only past the phase-type queue's limits.

const (
	// batchMinCS2 floors the service scv in the batch term: the gamma shapes stay at or below 100 per service.
	batchMinCS2 = 0.01
	// batchMaxStates bounds the number-in-system laws; a queue that needs more has no usable steady state here.
	batchMaxStates = 200000
	// batchTol is where the laws are cut: the last kept probability is below it, relative to the total.
	batchTol = 1e-15
)

// errBatchStates is returned for a queue whose number in system does not settle within batchMaxStates.
var errBatchStates = errors.New("the batch queue's number in system does not settle within the state limit")

// mxq is one batch-fed queue.
type mxq struct {
	c   int
	s   float64   // mean service time, ms
	cs2 float64   // scv of the service time (the caller floors it at batchMinCS2)
	lam float64   // batch epochs per ms
	q   []float64 // q[k] = P(X = k), k = 0..kmax; q[0] = 0
}

// mxqSol is the solved queue. Times in ms.
type mxqSol struct {
	W0, W0sq float64   // the wait of a call that arrives alone (and of a batch's first member): mean, second moment
	Wbar     float64   // the mean wait over all calls
	MS, MSsq []float64 // by batch size k (index k, for the sizes asked): epoch to the last member's end of service
}

// solve returns the queue's waits and the makespans of batches of the given sizes.
func (m *mxq) solve(sizes []int) (*mxqSol, error) {
	kmax := len(m.q) - 1
	for _, k := range sizes {
		kmax = max(kmax, k)
	}
	if m.c == 1 {
		return m.solveOne(kmax), nil
	}
	// the phase-type queue (batchph.go) is exact for its phase law (the gamma itself at cs2 = 1, 1/2, 1/4, ...); past
	// its limits (c > 8, more than 500 configurations of busy workers, cs2 below 1/16) the superposition takes over
	if ps, err := m.phSolve(sizes); err == nil {
		return &mxqSol{W0: ps.W0, W0sq: ps.W0sq, Wbar: ps.Wbar, MS: ps.MS, MSsq: ps.MSsq}, nil
	}
	return m.solveSup(sizes, kmax)
}

// solveSup is solve by the superposition (any c; for c = 1 and cs2 <= 1 it is exact, and solve uses solveOne).
func (m *mxq) solveSup(sizes []int, kmax int) (*mxqSol, error) {
	pM, err := m.piM()
	if err != nil {
		return nil, err
	}
	pD := m.piD(len(pM) - 1)
	w := math.Min(m.cs2, 1)
	if pD == nil {
		// piD found no law (a state that cannot move down: e^(-lam s) is 0 in float64 at lam s >= 746): take the
		// exponential law alone rather than mixing in nothing, which would leave pi with mass w (Queue.lean, Q2)
		w = 1
	}
	n := max(len(pM), len(pD))
	pi := make([]float64, n)
	for i := range pi {
		var a, b float64
		if i < len(pD) {
			a = pD[i]
		}
		if i < len(pM) {
			b = pM[i]
		}
		pi[i] = (1-w)*a + w*b
	}
	c := m.c
	cf := float64(c)
	// the superposition in units of s; busy: c equilibrium workers, exact up to m0 points, then linear
	m0 := 3*c + kmax + 8
	etB, et2B := supMoments(c, 0, m0, m.cs2)
	busy := func(k int) (float64, float64) { // E[T_k], E[T_k^2], k >= 1, units of s
		if k <= m0 {
			return etB[k-1], et2B[k-1]
		}
		e := etB[m0-1] + float64(k-m0)/cf
		v := et2B[m0-1] - etB[m0-1]*etB[m0-1] + float64(k-m0)*m.cs2/(cf*cf)
		return e, v + e*e
	}
	// idle: n busy (equilibrium) workers and c-n fresh ones
	etI := make([][]float64, c)
	et2I := make([][]float64, c)
	for nb := 0; nb < c; nb++ {
		if mm := kmax - (c - nb); mm >= 1 {
			etI[nb], et2I[nb] = supMoments(nb, c-nb, mm, m.cs2)
		}
	}
	mk, mk2 := make([]float64, c+1), make([]float64, c+1)
	for k := 1; k <= min(c, kmax); k++ {
		mk[k], mk2[k] = maxOf(k, m.cs2)
	}
	// per size: the tail after the last start, busy and idle
	sol := &mxqSol{MS: make([]float64, kmax+1), MSsq: make([]float64, kmax+1)}
	want := make([]bool, kmax+1)
	for _, k := range sizes {
		want[k] = true
	}
	// busy: the other c-1 workers are in equilibrium and run a member if they started a job since the first member
	// did, (k-1)/c service times before the last on average
	tailB := make([][2]float64, kmax+1)
	for k := 1; k <= kmax; k++ {
		if want[k] {
			tailB[k][0], tailB[k][1] = tailQuad(0, c-1, 0, float64(k-1)/cf, m.cs2)
		}
	}
	// idle: of the other c-1 workers, the f-1 or f fresh ones run members; tbar has passed since they started
	tailI := make([][][2]float64, c)
	for nb := 0; nb < c; nb++ {
		f := c - nb
		tailI[nb] = make([][2]float64, kmax+1)
		for k := f + 1; k <= kmax; k++ {
			if !want[k] {
				continue
			}
			tbar := etI[nb][k-f-1]
			nf := min(f, c-1)
			tailI[nb][k][0], tailI[nb][k][1] = tailQuad(nf, c-1-nf, tbar, tbar, m.cs2)
		}
	}
	ev := make([]float64, kmax+1) // E[V_j]: the start of member j
	for nn, p := range pi {
		if !(p > 0) {
			continue
		}
		if nn >= c {
			for j := 1; j <= kmax; j++ {
				e, _ := busy(nn - c + j)
				ev[j] += p * e
			}
			e1, e21 := busy(nn - c + 1)
			sol.W0 += p * e1
			sol.W0sq += p * e21
			for k := 1; k <= kmax; k++ {
				if !want[k] {
					continue
				}
				e, e2 := busy(nn - c + k)
				t, t2 := tailB[k][0], tailB[k][1]
				sol.MS[k] += p * (e + t)
				sol.MSsq[k] += p * (e2 + 2*e*t + t2)
			}
			continue
		}
		f := c - nn
		for j := f + 1; j <= kmax; j++ {
			ev[j] += p * etI[nn][j-f-1]
		}
		for k := 1; k <= kmax; k++ {
			if !want[k] {
				continue
			}
			if k <= f {
				sol.MS[k] += p * mk[k]
				sol.MSsq[k] += p * mk2[k]
				continue
			}
			e, e2 := etI[nn][k-f-1], et2I[nn][k-f-1]
			t, t2 := tailI[nn][k][0], tailI[nn][k][1]
			sol.MS[k] += p * (e + t)
			sol.MSsq[k] += p * (e2 + 2*e*t + t2)
		}
	}
	// back to ms
	s := m.s
	sol.W0 *= s
	sol.W0sq *= s * s
	for k := range sol.MS {
		sol.MS[k] *= s
		sol.MSsq[k] *= s * s
	}
	ex, wsum := 0.0, 0.0
	for k := 1; k < len(m.q); k++ {
		if m.q[k] == 0 {
			continue
		}
		ex += float64(k) * m.q[k]
		acc := 0.0
		for j := 1; j <= k; j++ {
			acc += ev[j]
		}
		wsum += m.q[k] * acc
	}
	if ex > 0 {
		sol.Wbar = wsum / ex * s
	}
	return sol, nil
}

// solveOne is the exact M^X/G/1 queue: the workload an epoch finds is the M/G/1 workload of batches B (Takacs), and
// the members follow one another.
func (m *mxq) solveOne(kmax int) *mxqSol {
	var ex, ex2, ex3 float64
	for k := 1; k < len(m.q); k++ {
		x := float64(k)
		ex += x * m.q[k]
		ex2 += x * x * m.q[k]
		ex3 += x * x * x * m.q[k]
	}
	s, cs2 := m.s, m.cs2
	es2 := s * s * (1 + cs2)
	es3 := s * s * s * (1 + cs2) * (1 + 2*cs2)
	eb2 := ex*cs2*s*s + ex2*s*s
	// E[B^3] = E[X] E[S^3] + 3 E[X(X-1)] E[S^2] E[S] + E[X(X-1)(X-2)] E[S]^3
	eb3 := ex*es3 + 3*(ex2-ex)*es2*s + (ex3-3*ex2+2*ex)*s*s*s
	rho := m.lam * ex * s
	sol := &mxqSol{MS: make([]float64, kmax+1), MSsq: make([]float64, kmax+1)}
	if rho > 0 {
		sol.W0 = m.lam * eb2 / (2 * (1 - rho))
		sol.W0sq = 2*sol.W0*sol.W0 + m.lam*eb3/(3*(1-rho))
	}
	for k := 1; k <= kmax; k++ {
		x := float64(k)
		sol.MS[k] = sol.W0 + x*s
		sol.MSsq[k] = sol.W0sq + 2*sol.W0*x*s + x*cs2*s*s + x*x*s*s
	}
	if ex > 0 {
		sol.Wbar = sol.W0 + s*(ex2-ex)/(2*ex)
	}
	return sol
}

// piM is the law of the number in an M^X/M/c system, from the cut equations: the flow up across n-1|n,
// lam sum_{i<n} pi_i P(X > n-1-i), equals the flow down, min(n, c)/s pi_n.
func (m *mxq) piM() ([]float64, error) {
	kmax := len(m.q) - 1
	gbar := make([]float64, kmax+1) // P(X > j)
	for j := kmax - 1; j >= 0; j-- {
		gbar[j] = gbar[j+1] + m.q[j+1]
	}
	pi := []float64{1}
	tot := 1.0
	for n := 1; ; n++ {
		if n > batchMaxStates {
			return nil, errBatchStates
		}
		acc := 0.0
		for i := max(0, n-kmax); i < n; i++ {
			acc += pi[i] * gbar[n-1-i]
		}
		p := m.lam * m.s / float64(min(n, m.c)) * acc
		pi = append(pi, p)
		tot += p
		if tot > 1e250 {
			for i := range pi {
				pi[i] *= 1e-250
			}
			tot *= 1e-250
		}
		if n > m.c+kmax && p <= batchTol*tot {
			break
		}
	}
	for i := range pi {
		pi[i] /= tot
	}
	return pi, nil
}

// cpois is the law of the number of calls that arrive in one service time s: Poisson(lam s) epochs of X calls each
// (Panjer's recursion, started at 1 and normalized, so a long interval does not underflow).
func (m *mxq) cpois() []float64 {
	kmax := len(m.q) - 1
	L := m.lam * m.s
	var ex, ex2 float64
	for k := 1; k <= kmax; k++ {
		ex += float64(k) * m.q[k]
		ex2 += float64(k*k) * m.q[k]
	}
	end := L*ex + 12*math.Sqrt(L*ex2) + float64(kmax)
	a := []float64{1}
	peak := 1.0
	for n := 1; ; n++ {
		acc := 0.0
		for j := 1; j <= min(n, kmax); j++ {
			acc += float64(j) * m.q[j] * a[n-j]
		}
		v := L / float64(n) * acc
		a = append(a, v)
		peak = math.Max(peak, v)
		if peak > 1e250 {
			for i := range a {
				a[i] *= 1e-250
			}
			peak *= 1e-250
		}
		if float64(n) > end && v < 1e-18*peak {
			break
		}
		if n > batchMaxStates {
			break
		}
	}
	tot := 0.0
	for _, v := range a {
		tot += v
	}
	for i := range a {
		a[i] /= tot
	}
	return a
}

// piD is the law of the number in an M^X/D/c system at an arbitrary time: the stationary law of the Crommelin chain
// N(t+s) = (N(t)-c)^+ + A(s), cut at nmax+len(A) states (what passes the cut is held at it), solved by GTH state
// reduction, which only adds and multiplies. The chain moves down by at most c and up by at most len(A)-1, and the
// reduction keeps that band. It returns nil if a state cannot move down (a load far beyond any service's).
func (m *mxq) piD(nmax int) []float64 {
	a := m.cpois()
	la := len(a)
	c := m.c
	N := nmax + la
	lo := func(i int) int { return max(0, i-c) }
	// a row's band: from lo(i), wide enough for one step up (la columns) and for the reduction's reads of row k
	// (lo(k)..k, up to c+1 columns) and fill-in; la alone overruns the slice when c >= la (a low load on many workers)
	W := max(la, c+1)
	P := make([]float64, (N+1)*W)
	for i := 0; i <= N; i++ {
		l := lo(i)
		for x := 0; x < la; x++ {
			j := min(l+x, N)
			P[i*W+j-l] += a[x]
		}
	}
	at := func(i, j int) int { return i*W + j - lo(i) }
	first := func(k int) int { // the first row whose band reaches column k: a row below c starts at 0
		if k <= W-1 {
			return 0
		}
		return k + c - W + 1
	}
	for k := N; k >= 1; k-- {
		sum := 0.0
		for j := lo(k); j < k; j++ {
			sum += P[at(k, j)]
		}
		if !(sum > 0) {
			return nil
		}
		for i := first(k); i < k; i++ {
			if k > lo(i)+W-1 {
				continue
			}
			ik := at(i, k)
			if P[ik] == 0 {
				continue
			}
			f := P[ik] / sum
			P[ik] = f
			for j := lo(k); j < k; j++ {
				P[at(i, j)] += f * P[at(k, j)]
			}
		}
	}
	pi := make([]float64, N+1)
	pi[0] = 1
	tot := 1.0
	for k := 1; k <= N; k++ {
		v := 0.0
		for i := first(k); i < k; i++ {
			if k <= lo(i)+W-1 {
				v += pi[i] * P[at(i, k)]
			}
		}
		pi[k] = v
		tot += v
	}
	for i := range pi {
		pi[i] /= tot
	}
	// drop the negligible tail
	end := len(pi)
	for end > 1 && pi[end-1] < batchTol*1e-3 {
		end--
	}
	return pi[:end]
}

// glX8, glW8: the 8-point Gauss-Legendre rule on [-1, 1].
var glX8, glW8 = mathx.GaussLegendre(8)

// supCache keeps supMoments' results: they depend on the workers, the count and the service's cs2 only (times are in
// units of the mean service time), not on the load, so a prediction's solves at its range corners and corners of
// load share them. A busy call at 64 workers costs 25-55 ms at cs2 1/4-2 (supMomentsOf). Bounded: cleared when it
// holds supCacheMax entries.
var supCache = struct {
	sync.Mutex
	m map[supKey][2][]float64
}{m: map[supKey][2][]float64{}}

type supKey struct {
	neq, nfr, M int
	cs2         float64
}

const supCacheMax = 256

// supMoments is supMomentsOf through supCache. The slices it returns are shared: callers only read them.
func supMoments(neq, nfr, M int, cs2 float64) (et, et2 []float64) {
	k := supKey{neq, nfr, M, cs2}
	supCache.Lock()
	v, ok := supCache.m[k]
	supCache.Unlock()
	if ok {
		return v[0], v[1]
	}
	et, et2 = supMomentsOf(neq, nfr, M, cs2, nil)
	supCache.Lock()
	if len(supCache.m) >= supCacheMax {
		clear(supCache.m)
	}
	supCache.m[k] = [2][]float64{et, et2}
	supCache.Unlock()
	return et, et2
}

// supMomentsOf returns E[T_m] and E[T_m^2], m = 1..M, in units of the mean service time, where T_m is the m-th point
// of the superposition of neq equilibrium and nfr ordinary renewal processes with gamma(1, cs2) intervals:
// E[T_m^r] = int_0^inf r t^(r-1) P(N(t) <= m-1) dt, with the workers' counts independent and their laws closed.
// The integral runs over panels of a width that resolves the services' spread, each by the 8-point rule. w, if not
// nil, counts the work.
//
// Cost: a node's law of N(t) is the neq-th power of a worker's equilibrium law times the nfr-th power of its fresh
// law, as series cut at z^M, by squaring (supPow), each law kept as the window where it is at least supTiny. The
// integrand falls with t, and the panels stop after one where it is below supStop at every node: with c workers it is
// gone a few units past M/c (t = 5-9 at 64 workers), where tmax, set by one worker's spread, runs to 60 and more. A
// busy call (M = 3c + kmax + 8) takes 25-55 ms at 64 workers and 80-170 ms at 128 (cs2 1/4-2; 95 ms and 0.3 s at
// cs2 4), where one convolution per worker at every node up to tmax took 15-31 s at 64 workers and 205 s at 128
// (cs2 1). The moments agree with that way (batchsup_test.go) to about 1e-12 relative; TestSupMomentsWork bounds the
// work at 64 workers.
func supMomentsOf(neq, nfr, M int, cs2 float64, w *supWork) (et, et2 []float64) {
	et, et2 = make([]float64, M), make([]float64, M)
	c := neq + nfr
	if M < 1 || c < 1 {
		return et, et2
	}
	a, th := 1/cs2, cs2
	J := M // the counts' laws are cut at z^J (only N <= M-1 is read)
	mc := float64(M) / float64(c)
	tmax := mc + 4 + 12*math.Sqrt(cs2*(mc+1)) + 30*cs2
	// panels resolve a service's spread and, with many workers, the superposition's own spacing 1/c
	h := math.Min(0.5, math.Max(0.05, 0.5*math.Sqrt(cs2))) * math.Min(1, 4/float64(c))
	np := int(math.Ceil(tmax / h))
	lg := make([]float64, J+1) // lnGamma(j a + 1)
	for j := 1; j <= J; j++ {
		lg[j], _ = math.Lgamma(float64(j)*a + 1)
	}
	pj := make([]float64, J+1)  // P(j a, t/th) = F_j(t)
	pj1 := make([]float64, J+1) // P(j a + 1, t/th)
	ij := make([]float64, J+1)  // I_j(t)
	geq := make([]float64, J+1)
	buf := make([][]float64, 7)
	for i := range buf {
		buf[i] = make([]float64, J)
	}
	one := supPoly{p: []float64{1}}
	for pn := 0; pn < np; pn++ {
		lo := float64(pn) * h
		fM := 0.0 // the panel's largest P(N(t) <= M-1)
		for q := range glX8 {
			u := (glX8[q] + 1) / 2
			t, wt := lo+h*u, glW8[q]*h/2
			if pn == 0 { // t = h u^3: smooth at 0 for a service shape below 1
				t, wt = h*u*u*u, glW8[q]/2*3*h*u*u
			}
			x := t / th
			lx := math.Log(x)
			pj[0], pj1[0] = 1, 1
			ij[0] = t
			top := J // P(sh, x) falls with the shape: past the first one below supTiny, the rest are taken as 0
			for j := 1; j <= J; j++ {
				if j > top {
					pj[j], pj1[j], ij[j] = 0, 0, 0
					continue
				}
				sh := float64(j) * a
				pj[j] = mathx.GammaP(sh, x)
				// P(sh+1, x) = P(sh, x) - x^sh e^-x / Gamma(sh+1)
				pj1[j] = math.Max(pj[j]-math.Exp(sh*lx-x-lg[j]), 0)
				ij[j] = t*pj[j] - float64(j)*pj1[j] // a th = 1
				if pj[j] < supTiny {
					top = j
				}
			}
			if w != nil {
				w.Nodes++
				w.Gammas += top
			}
			geq[0] = 1
			for j := 1; j <= J; j++ {
				geq[j] = math.Min(math.Max(ij[j-1]-ij[j], 0), 1)
			}
			// a worker's count laws: P(N = j) = P(N >= j) - P(N >= j+1)
			peq, pfr := buf[0], buf[1]
			for j := 0; j < J; j++ {
				peq[j] = math.Max(geq[j]-geq[j+1], 0)
				pfr[j] = math.Max(pj[j]-pj[j+1], 0)
			}
			tot := one
			if neq > 0 {
				tot = supPow(supTrim(0, peq), neq, J, buf[2], buf[3], w)
			}
			if nfr > 0 {
				fr := supPow(supTrim(0, pfr), nfr, J, buf[4], buf[5], w)
				tot = supMul(buf[6], tot, fr, J, w)
			}
			cum := 0.0
			for mm := 1; mm <= M; mm++ {
				if k := mm - 1 - tot.lo; k >= 0 && k < len(tot.p) {
					cum += tot.p[k]
				}
				f := math.Min(cum, 1)
				et[mm-1] += wt * f
				et2[mm-1] += wt * 2 * t * f
			}
			fM = math.Max(fM, math.Min(cum, 1))
		}
		if fM < supStop {
			break
		}
	}
	return et, et2
}

const (
	// supTiny: a count probability below it is dropped (what a node's law loses is far below the moments' rounding)
	supTiny = 1e-40
	// supStop: the integration stops after a panel where P(N(t) <= M-1) is below it at every node; what is left adds
	// at most about supStop tmax^2 to a second moment
	supStop = 1e-20
)

// supWork counts supMomentsOf's work: integration nodes, GammaP calls and the multiply-adds of the count laws.
type supWork struct {
	Nodes, Gammas, MulAdds int
}

// supPoly is a count law cut at z^J, as a window: P(N = lo+i) = p[i], below supTiny outside it.
type supPoly struct {
	lo int
	p  []float64
}

// supTrim returns the window of p (offset lo) where it is at least supTiny.
func supTrim(lo int, p []float64) supPoly {
	i, j := 0, len(p)
	for i < j && p[i] < supTiny {
		i++
	}
	for j > i && p[j-1] < supTiny {
		j--
	}
	return supPoly{lo: lo + i, p: p[i:j]}
}

// supMul returns a b cut at z^J, in dst (len J; not a's or b's storage). The laws are nonnegative: no cancellation.
func supMul(dst []float64, a, b supPoly, J int, w *supWork) supPoly {
	lo := a.lo + b.lo
	if len(a.p) == 0 || len(b.p) == 0 || lo >= J {
		return supPoly{lo: J}
	}
	n := min(len(a.p)+len(b.p)-1, J-lo)
	d := dst[:n]
	clear(d)
	for i, ai := range a.p[:min(len(a.p), n)] {
		bp := b.p[:min(len(b.p), n-i)]
		di := d[i : i+len(bp)]
		for j, bj := range bp {
			di[j] += ai * bj
		}
		if w != nil {
			w.MulAdds += len(bp)
		}
	}
	return supTrim(lo, d)
}

// supSq returns a^2 cut at z^J, in dst (len J; not a's storage): each cross term once, doubled.
func supSq(dst []float64, a supPoly, J int, w *supWork) supPoly {
	lo := 2 * a.lo
	if len(a.p) == 0 || lo >= J {
		return supPoly{lo: J}
	}
	n := min(2*len(a.p)-1, J-lo)
	d := dst[:n]
	clear(d)
	for i, ai := range a.p {
		if 2*i >= n {
			break
		}
		d[2*i] += ai * ai
		a2 := 2 * ai
		ap := a.p[i+1 : min(len(a.p), n-i)]
		di := d[2*i+1 : 2*i+1+len(ap)]
		for j, aj := range ap {
			di[j] += a2 * aj
		}
		if w != nil {
			w.MulAdds += 1 + len(ap)
		}
	}
	return supTrim(lo, d)
}

// supPow returns v^n (n >= 1) cut at z^J, by squaring from the top bit down (a multiply by v itself, which is short,
// for each further bit set); the result is in b1 or b2 (len J each), and v is not overwritten.
func supPow(v supPoly, n, J int, b1, b2 []float64, w *supWork) supPoly {
	r := v
	cur, nxt := b1, b2
	for bit := bits.Len(uint(n)) - 2; bit >= 0; bit-- {
		r = supSq(cur, r, J, w)
		cur, nxt = nxt, cur
		if n>>bit&1 == 1 {
			r = supMul(cur, r, v, J, w)
			cur, nxt = nxt, cur
		}
	}
	return r
}

// tailQuad integrates the end of a batch after its last member starts, in units of the mean service time: the mean
// and second moment of max(S, the remaining times of the other workers' current members). The other workers are
// nf fresh ones (they started one of the batch's first members at 0, and tbar has passed since: the batch is theirs,
// and their remaining time is the forward recurrence of a renewal process at tbar, P(r > x) ~ 1 - F(tbar+x) +
// F_R(tbar+x) - F_R(x), exact for exponential services) and ne in equilibrium (each runs a member of the batch only
// if it started a job within the last win: P(ours and r > x) = F_R(x+win) - F_R(x)). F_S(x) = P(a, x/th);
// F_R(x) = int_0^x (1 - F_S) = x (1 - P(a, x/th)) + P(a+1, x/th), the equilibrium residual's CDF.
func tailQuad(nf, ne int, tbar, win, cs2 float64) (float64, float64) {
	a, th := 1/cs2, cs2
	lga, _ := math.Lgamma(a + 1)
	fs := func(x float64) (float64, float64) { // F_S, F_R
		if x <= 0 {
			return 0, 0
		}
		z := x / th
		p := mathx.GammaP(a, z)
		p1 := math.Max(p-math.Exp(a*math.Log(z)-z-lga), 0)
		return p, math.Min(x*(1-p)+p1, 1)
	}
	xmax := 1 + 15*math.Sqrt(cs2) + 30*cs2
	h := math.Min(0.25, math.Max(0.025, 0.25*math.Sqrt(cs2)))
	np := int(math.Ceil(xmax / h))
	var e, e2 float64
	for pn := 0; pn < np; pn++ {
		lo := float64(pn) * h
		for q := range glX8 {
			u := (glX8[q] + 1) / 2
			x, wt := lo+h*u, glW8[q]*h/2
			if pn == 0 { // x = h u^3: smooth at 0 for a shape below 1
				x, wt = h*u*u*u, glW8[q]/2*3*h*u*u
			}
			fx, rx := fs(x)
			g := fx
			if nf > 0 {
				ft, rt := fs(tbar + x)
				g *= math.Pow(math.Min(math.Max(ft-rt+rx, 0), 1), float64(nf))
			}
			if ne > 0 {
				_, rw := fs(x + win)
				g *= math.Pow(math.Min(math.Max(1-rw+rx, 0), 1), float64(ne))
			}
			e += wt * (1 - g)
			e2 += wt * 2 * x * (1 - g)
		}
	}
	return e, e2
}

// maxOf returns the mean and second moment of the largest of k services, in units of the mean service time (a batch
// that starts at once on free workers).
func maxOf(k int, cs2 float64) (float64, float64) {
	a, th := 1/cs2, cs2
	xmax := 1 + 15*math.Sqrt(cs2) + 30*cs2
	h := math.Min(0.25, math.Max(0.025, 0.25*math.Sqrt(cs2)))
	np := int(math.Ceil(xmax / h))
	var e, e2 float64
	for pn := 0; pn < np; pn++ {
		lo := float64(pn) * h
		for q := range glX8 {
			u := (glX8[q] + 1) / 2
			x, wt := lo+h*u, glW8[q]*h/2
			if pn == 0 {
				x, wt = h*u*u*u, glW8[q]/2*3*h*u*u
			}
			g := 1 - math.Pow(mathx.GammaP(a, x/th), float64(k))
			e += wt * g
			e2 += wt * 2 * x * g
		}
	}
	return e, e2
}
