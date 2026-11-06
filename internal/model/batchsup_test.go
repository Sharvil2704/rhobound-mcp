// SPDX-License-Identifier: Apache-2.0

package model

import (
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/mathx"
)

// supMomentsRef is supMomentsOf as it was up to 2bd68056: one convolution per worker at every node, over every panel
// up to tmax. It is the reference the faster supMomentsOf is checked against (TestSupMomentsAgainstReference).
func supMomentsRef(neq, nfr, M int, cs2 float64) (et, et2 []float64) {
	et, et2 = make([]float64, M), make([]float64, M)
	c := neq + nfr
	if M < 1 || c < 1 {
		return et, et2
	}
	a, th := 1/cs2, cs2
	J := M // a worker's count is held at J (only N <= M-1 is read)
	mc := float64(M) / float64(c)
	tmax := mc + 4 + 12*math.Sqrt(cs2*(mc+1)) + 30*cs2
	// panels resolve a service's spread and, with many workers, the superposition's own spacing 1/c
	h := math.Min(0.5, math.Max(0.05, 0.5*math.Sqrt(cs2))) * math.Min(1, 4/float64(c))
	np := int(math.Ceil(tmax / h))
	lg := make([]float64, J+2) // lnGamma(j a + 1)
	for j := 1; j <= J+1; j++ {
		lg[j], _ = math.Lgamma(float64(j)*a + 1)
	}
	pj := make([]float64, J+2)  // P(j a, t/th) = F_j(t)
	pj1 := make([]float64, J+2) // P(j a + 1, t/th)
	ij := make([]float64, J+2)  // I_j(t)
	geq := make([]float64, J+2)
	gfr := make([]float64, J+2)
	peq := make([]float64, J+1)
	pfr := make([]float64, J+1)
	tot := make([]float64, J+1)
	tmp := make([]float64, J+1)
	pmf := func(ge, out []float64) {
		for j := 0; j < J; j++ {
			out[j] = math.Max(ge[j]-ge[j+1], 0)
		}
		out[J] = math.Max(ge[J], 0)
	}
	conv := func(p []float64) {
		for i := range tmp {
			tmp[i] = 0
		}
		for i := 0; i <= J; i++ {
			if tot[i] == 0 {
				continue
			}
			for j := 0; i+j <= J && j <= J; j++ {
				tmp[i+j] += tot[i] * p[j]
			}
			// what passes J is held at J
			rest := 0.0
			for j := J - i + 1; j <= J; j++ {
				rest += p[j]
			}
			tmp[J] += tot[i] * rest
		}
		copy(tot, tmp)
	}
	for pn := 0; pn < np; pn++ {
		lo := float64(pn) * h
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
			for j := 1; j <= J+1; j++ {
				sh := float64(j) * a
				pj[j] = mathx.GammaP(sh, x)
				// P(sh+1, x) = P(sh, x) - x^sh e^-x / Gamma(sh+1)
				pj1[j] = math.Max(pj[j]-math.Exp(sh*lx-x-lg[j]), 0)
				ij[j] = t*pj[j] - float64(j)*pj1[j] // a th = 1
			}
			geq[0], gfr[0] = 1, 1
			for j := 1; j <= J+1; j++ {
				geq[j] = math.Min(math.Max(ij[j-1]-ij[j], 0), 1)
				gfr[j] = pj[j]
			}
			pmf(geq, peq)
			pmf(gfr, pfr)
			for i := range tot {
				tot[i] = 0
			}
			tot[0] = 1
			for range neq {
				conv(peq)
			}
			for range nfr {
				conv(pfr)
			}
			cum := 0.0
			for mm := 1; mm <= M; mm++ {
				cum += tot[mm-1]
				f := math.Min(cum, 1)
				et[mm-1] += wt * f
				et2[mm-1] += wt * 2 * t * f
			}
		}
	}
	return et, et2
}

// The faster supMomentsOf against supMomentsRef, on the calls solveSup makes (busy: c equilibrium workers and
// M = 3c + kmax + 8; idle: nb equilibrium and c-nb fresh ones) and a few more: the same integrand and nodes, so they
// differ only by rounding and by what is dropped (count probabilities below supTiny, the panels past supStop).
// RB_SUP_REF_WORKERS (a list such as 32,48,64) checks those workers instead: the reference takes 1-30 s per call there.
func TestSupMomentsAgainstReference(t *testing.T) {
	cs := []int{1, 2, 3, 8, 16}
	if v := os.Getenv("RB_SUP_REF_WORKERS"); v != "" {
		cs = nil
		for _, f := range strings.Split(v, ",") {
			c, err := strconv.Atoi(f)
			if err != nil || c < 1 {
				t.Fatalf("RB_SUP_REF_WORKERS: %q", v)
			}
			cs = append(cs, c)
		}
	}
	worst := 0.0
	for _, c := range cs {
		for _, cs2 := range []float64{0.05, 0.25, 0.5, 1, 2, 4} {
			calls := [][3]int{{c, 0, 3*c + 14}, {0, c, 12}, {c / 2, c - c/2, 10}}
			if c > 1 {
				calls = append(calls, [3]int{c - 1, 1, 5})
			}
			for _, cl := range calls {
				et, et2 := supMomentsOf(cl[0], cl[1], cl[2], cs2, nil)
				rt, rt2 := supMomentsRef(cl[0], cl[1], cl[2], cs2)
				for m := range rt {
					for _, p := range [][2]float64{{et[m], rt[m]}, {et2[m], rt2[m]}} {
						d := math.Abs(p[0]-p[1]) / math.Abs(p[1])
						worst = math.Max(worst, d)
						if !(d <= 1e-9) {
							t.Errorf("neq %d nfr %d M %d cs2 %g, m %d: %.15g against the reference %.15g", cl[0], cl[1],
								cl[2], cs2, m+1, p[0], p[1])
						}
					}
				}
			}
		}
	}
	t.Logf("largest relative difference %.3g", worst)
}

// The work of the superposition's first solve at 64 workers, not its time, is bounded: the busy call and the idle
// ones solveSup makes for batches of up to 6. With one convolution per worker at every node up to tmax, the busy call
// alone took 1.6e4 nodes, 3.3e6 GammaP calls and up to 4.3e10 multiply-adds at cs2 1 (18 s); now 1.5e3, 7e4, 4.2e7.
func TestSupMomentsWork(t *testing.T) {
	const c, kmax = 64, 6
	for _, cs2 := range []float64{0.05, 0.25, 1, 2} {
		var w supWork
		supMomentsOf(c, 0, 3*c+kmax+8, cs2, &w)
		for nb := c - kmax + 1; nb < c; nb++ {
			supMomentsOf(nb, c-nb, kmax-(c-nb), cs2, &w)
		}
		if w.Nodes > 12000 || w.Gammas > 250000 || w.MulAdds > 120000000 {
			t.Errorf("cs2 %g: %d nodes, %d GammaP calls, %d multiply-adds; the bounds are 12000, 250000, 1.2e8", cs2,
				w.Nodes, w.Gammas, w.MulAdds)
		}
		t.Logf("cs2 %g: %d nodes, %d GammaP calls, %d multiply-adds", cs2, w.Nodes, w.Gammas, w.MulAdds)
	}
}
