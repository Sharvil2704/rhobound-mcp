// SPDX-License-Identifier: Apache-2.0

//go:build accuracy

package infer_test

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// A study of the model's error at 70-80% utilization, against the simulator
// with the model given the TRUE parameters (so inference error is out of it).
//
//	go test -tags accuracy -run TestAccuracy -v ./internal/infer

type scen struct {
	name  string
	group string
	build func(shift float64) *node
	lam   float64 // requests per second
	shift float64 // the change, ms, applied to node `at`
	at    string
	spanq *bool // the row's own span mode (spans include the wait for a worker); nil: the run's (ACC_SPANQ)
}

func walk(n *node, f func(*node)) {
	f(n)
	for _, c := range n.children {
		walk(c, f)
	}
}

// topo builds the model topology of a node tree, with the true parameters. corr is the measured branch
// correlation of each parallel group (truthOut.Corr; nil: independent branches), so that the true-parameter
// model reads the same kind of topology the inference gives.
func topo(root *node, corr map[string]float64) *model.Topology {
	t := &model.Topology{Name: "sim", Entry: map[int]float64{}}
	idx := map[*node]int{}
	walk(root, func(n *node) {
		if _, seen := idx[n]; seen { // a node called twice (a repeated call) is one service
			return
		}
		idx[n] = len(t.Services)
		cs2 := 1.0
		mean := n.mean + n.shift
		switch n.dist {
		case "gamma", "h2", "lognormal":
			cs2 = n.cs2 * n.mean * n.mean / (mean * mean)
		default:
			cs2 = n.mean * n.mean / (mean * mean)
		}
		sv := model.Service{Name: n.name, S: mean, CS2: cs2, Parallel: n.parallel, Servers: n.pods, Workers: n.c, Glue: n.post,
			BranchCorr: corr[n.name]}
		if n.pods > 1 {
			sv.Routing = model.Random
			sv.Shares = append([]float64(nil), n.shares...)
		} else {
			sv.Servers, sv.Workers, sv.Routing = n.c, 1, model.Pooled
		}
		if n != root {
			sv.Hop = 2 * (hopMs + envHopJitter())
		}
		t.Services = append(t.Services, sv)
	})
	done := map[*node]bool{}
	walk(root, func(n *node) {
		if done[n] {
			return
		}
		done[n] = true
		pOf := func(j int) float64 {
			if n.p != nil {
				return n.p[j]
			}
			return 1
		}
		if n.stage == nil {
			for j, c := range n.children {
				t.Services[idx[n]].Calls = append(t.Services[idx[n]].Calls, model.Call{Child: idx[c], P: pOf(j)})
			}
			return
		}
		// staged: a stage of one call is a call; a stage of several is a zero-time delay node calling them in
		// parallel (its members are always called: a conditional member has no form in the model)
		for j := 0; j < len(n.children); {
			k := j
			for k < len(n.children) && n.stage[k] == n.stage[j] {
				k++
			}
			if k-j == 1 {
				t.Services[idx[n]].Calls = append(t.Services[idx[n]].Calls, model.Call{Child: idx[n.children[j]], P: pOf(j)})
			} else {
				name := fmt.Sprintf("stage:%s:%d", n.name, n.stage[j]+1)
				st := model.Service{Name: name, Delay: true, Parallel: true, CS2: 1, BranchCorr: corr[name]}
				for _, c := range n.children[j:k] {
					st.Calls = append(st.Calls, model.Call{Child: idx[c], P: 1})
				}
				t.Services = append(t.Services, st)
				t.Services[idx[n]].Calls = append(t.Services[idx[n]].Calls, model.Call{Child: len(t.Services) - 1, P: pOf(j)})
			}
			j = k
		}
	})
	t.Entry[0] = 1
	if err := t.Finish(); err != nil {
		panic(err)
	}
	return t
}

func find(root *node, name string) *node {
	var out *node
	walk(root, func(n *node) {
		if n.name == name {
			out = n
		}
	})
	return out
}

func scenarios() []scen {
	var out []scen
	dists := []struct {
		d   string
		cs2 float64
	}{{"exp", 1}, {"gamma", 0.25}, {"gamma", 0.5}, {"h2", 2}, {"h2", 4}}
	// single queues: the base state at rho 0.70, the changed state (+10% time) at 0.77
	for _, c := range []int{1, 2, 4, 8} {
		for _, d := range dists {
			c, d := c, d
			mean := 20.0
			out = append(out, scen{
				name:  fmt.Sprintf("M/G/%d %s cs2=%.2f", c, d.d, d.cs2),
				group: fmt.Sprintf("single c=%d", c),
				build: func(shift float64) *node {
					return &node{name: "q", c: c, pods: 1, mean: mean, dist: d.d, cs2: d.cs2, shift: shift}
				},
				lam: 0.70 * float64(c) / mean * 1000, shift: 0.10 * mean, at: "q",
			})
		}
	}
	// a service with a fixed extra time (the usual kind of change): exp part 14 + shift
	for _, c := range []int{1, 2, 4} {
		c := c
		out = append(out, scen{
			name: fmt.Sprintf("M/G/%d shifted-exp (14+6)", c), group: "single shifted",
			build: func(shift float64) *node {
				return &node{name: "q", c: c, pods: 1, mean: 14, shift: 6 + shift}
			},
			lam: 0.70 * float64(c) / 20 * 1000, shift: 2, at: "q",
		})
	}
	// tandem chains: arrival variability from upstream matters
	chain := func(d1, d2 string, c1, c2 int, cs1, cs2 float64) func(float64) *node {
		return func(shift float64) *node {
			b := &node{name: "b", c: c2, pods: 1, mean: 20, dist: d2, cs2: cs2, shift: shift}
			return &node{name: "a", c: c1, pods: 1, mean: 15, dist: d1, cs2: cs1, children: []*node{b}}
		}
	}
	for _, cc := range []struct {
		n      string
		d1, d2 string
		c1, c2 int
		s1, s2 float64
		lam    float64
	}{
		{"a(c1,gamma.25)->b(c1,exp)", "gamma", "exp", 1, 1, 0.25, 1, 35},
		{"a(c1,h2:4)->b(c1,exp)", "h2", "exp", 1, 1, 4, 1, 35},
		{"a(c2,gamma.25)->b(c2,gamma.5)", "gamma", "gamma", 2, 2, 0.25, 0.5, 70},
		{"a(c1,exp)->b(c2,h2:2)", "exp", "h2", 1, 2, 1, 2, 40},
	} {
		cc := cc
		out = append(out, scen{name: cc.n, group: "chain", build: chain(cc.d1, cc.d2, cc.c1, cc.c2, cc.s1, cc.s2),
			lam: cc.lam, shift: 2, at: "b"})
	}
	// parallel fan-out: two and three branches; the changed branch is the slow one
	out = append(out, scen{name: "a->(b||c) b slow", group: "fan-out", lam: 30, shift: 3, at: "b",
		build: func(shift float64) *node {
			b := &node{name: "b", c: 1, pods: 1, mean: 22, shift: shift}
			c := &node{name: "c", c: 1, pods: 1, mean: 18}
			return &node{name: "a", c: 8, pods: 1, mean: 3, parallel: true, children: []*node{b, c}}
		}})
	out = append(out, scen{name: "a->(b||c||d) similar", group: "fan-out", lam: 30, shift: 3, at: "b",
		build: func(shift float64) *node {
			b := &node{name: "b", c: 1, pods: 1, mean: 22, shift: shift}
			c := &node{name: "c", c: 1, pods: 1, mean: 21}
			d := &node{name: "d", c: 1, pods: 1, mean: 20}
			return &node{name: "a", c: 8, pods: 1, mean: 3, parallel: true, children: []*node{b, c, d}}
		}})
	out = append(out, scen{name: "a->(b||c) gamma.25 branches", group: "fan-out", lam: 30, shift: 3, at: "b",
		build: func(shift float64) *node {
			b := &node{name: "b", c: 1, pods: 1, mean: 22, dist: "gamma", cs2: 0.25, shift: shift}
			c := &node{name: "c", c: 1, pods: 1, mean: 22, dist: "gamma", cs2: 0.25}
			return &node{name: "a", c: 8, pods: 1, mean: 3, parallel: true, children: []*node{b, c}}
		}})
	// replicas with a random split
	for _, r := range []int{2, 4} {
		r := r
		out = append(out, scen{name: fmt.Sprintf("a->b(%d replicas, c=1)", r), group: "replicas", lam: 0.7 * float64(r) / 22 * 1000, shift: 2.2, at: "b",
			build: func(shift float64) *node {
				b := &node{name: "b", c: 1, pods: r, mean: 22, dist: "gamma", cs2: 0.5, shift: shift}
				return &node{name: "a", c: 16, pods: 1, mean: 2, children: []*node{b}}
			}})
	}
	return out
}

var configs = []struct {
	name string
	cfg  model.Config
}{
	{"plain", model.Plain},
	{"+arrivals", model.Config{ArrivalWeight: 1}},
	{"+maxmatch", model.Config{MatchedMax: true}},
	{"both", model.Default},
}

type row struct {
	s               scen
	tBase, tSE      float64
	tAfter          float64
	tChange, tChgSE float64
	rhoBase, rhoAf  float64
	mBase, mAfter   [4]float64
}

func (r row) levelErr(i int) float64    { return 100 * (r.mBase[i] - r.tBase) / r.tBase }
func (r row) levelErrAft(i int) float64 { return 100 * (r.mAfter[i] - r.tAfter) / r.tAfter }
func (r row) changeErr(i int) float64 {
	return 100 * ((r.mAfter[i] - r.mBase[i]) - r.tChange) / r.tChange
}

func evaluate(s scen, seconds float64, seeds []int64) row {
	r := row{s: s}
	base, after := s.build(0), s.build(0)
	find(after, s.at).shift += s.shift
	tr := truthRow(base, after, s.lam, seconds, seeds)
	r.tBase, r.tSE, r.tAfter, r.tChange, r.tChgSE = tr.Base, tr.BaseSE, tr.After, tr.Change, tr.ChangeSE
	for ci, c := range configs {
		for k, n := range []*node{base, after} {
			res, err := model.AnalyticWith(topo(n, tr.Corr), s.lam/1000, nil, c.cfg)
			v := math.NaN()
			if err == nil && res.Stable {
				v = res.L[0]
				if ci == 0 {
					rho, _ := res.MaxRho()
					if k == 0 {
						r.rhoBase = rho
					} else {
						r.rhoAf = rho
					}
				}
			}
			if k == 0 {
				r.mBase[ci] = v
			} else {
				r.mAfter[ci] = v
			}
		}
	}
	return r
}

// TestAccuracy: the truth is ACC_TRUTH_REPS (64) x 500 s per scenario, the seeds of row i (its index in
// scenarios(), whatever ACC_ONLY selects) derived from ACC_SEED (31337) and i.
func TestAccuracy(t *testing.T) {
	reps, seconds := truthReps(), 500.0
	seed := envInt("ACC_SEED", 31337)
	only := os.Getenv("ACC_ONLY")
	var rows []row
	for i, s := range scenarios() {
		if only != "" && !strings.Contains(s.group, only) && !strings.Contains(s.name, only) {
			continue
		}
		rows = append(rows, evaluate(s, seconds, truthSeeds(seed, i, reps)))
	}
	fmt.Printf("seed %d, truth %d x %.0f s per scenario (common random numbers for the change)\n", seed, reps, seconds)
	tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "scenario\trho\ttruth ms (se)\tchange ms (se)\tlevel % plain / arr / max / both\tchange %: plain / arr / max / both\t")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f (%.2f)\t%.3f (%.3f)\t%+5.1f %+5.1f %+5.1f %+5.1f\t%+6.1f %+6.1f %+6.1f %+6.1f\t\n", r.s.name, r.rhoBase, r.tBase, r.tSE,
			r.tChange, r.tChgSE, r.levelErr(0), r.levelErr(1), r.levelErr(2), r.levelErr(3), r.changeErr(0), r.changeErr(1), r.changeErr(2), r.changeErr(3))
	}
	tw.Flush()
	groups := map[string][]row{}
	for _, r := range rows {
		groups[r.s.group] = append(groups[r.s.group], r)
	}
	var names []string
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	fmt.Println("\nmedian / max |error| %, by group and configuration (level at rho .70 and .77 pooled | change)")
	var allL, allC [4][]float64
	for _, g := range append(names, "ALL") {
		fmt.Printf("  %-16s", g)
		for ci := range configs {
			var l, c []float64
			rs := groups[g]
			if g == "ALL" {
				l, c = allL[ci], allC[ci]
			} else {
				for _, r := range rs {
					l = append(l, math.Abs(r.levelErr(ci)), math.Abs(r.levelErrAft(ci)))
					c = append(c, math.Abs(r.changeErr(ci)))
				}
				allL[ci], allC[ci] = append(allL[ci], l...), append(allC[ci], c...)
			}
			fmt.Printf("  %s %4.1f/%4.1f|%4.1f/%4.1f", configs[ci].name[:min(5, len(configs[ci].name))], med(l), mx(l), med(c), mx(c))
		}
		fmt.Println()
	}
}

func med(x []float64) float64 {
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	return y[len(y)/2]
}

func mx(x []float64) float64 {
	m := 0.0
	for _, v := range x {
		m = math.Max(m, v)
	}
	return m
}

// chainFamily is a family of 360 two-service chains a -> b.
func chainFamily() []scen {
	type d struct {
		d   string
		cs2 float64
	}
	ups := []d{{"gamma", 0.25}, {"gamma", 0.5}, {"exp", 1}, {"h2", 2}, {"h2", 4}}
	downs := []d{{"exp", 1}, {"gamma", 0.5}, {"h2", 2}}
	var out []scen
	for _, u := range ups {
		for _, rhoA := range []float64{0.3, 0.5, 0.7} {
			for _, c1 := range []int{1, 2} {
				for _, dn := range downs {
					for _, c2 := range []int{1, 2} {
						for _, at := range []string{"a", "b"} {
							u, dn, rhoA, c1, c2, at := u, dn, rhoA, c1, c2, at
							// downstream mean 20 ms at rho_b = 0.70; upstream mean from rho_a at the same rate
							lam := 0.70 * float64(c2) / 20 * 1000
							meanA := rhoA * float64(c1) / lam * 1000
							shift := 0.10 * 20.0
							if at == "a" {
								shift = 0.10 * meanA
							}
							out = append(out, scen{
								name:  fmt.Sprintf("a(c%d,%s%.2g,r%.1f)->b(c%d,%s%.2g) chg@%s", c1, u.d, u.cs2, rhoA, c2, dn.d, dn.cs2, at),
								group: "chain-" + at, lam: lam, shift: shift, at: at,
								build: func(sh float64) *node {
									b := &node{name: "b", c: c2, pods: 1, mean: 20, dist: dn.d, cs2: dn.cs2}
									a := &node{name: "a", c: c1, pods: 1, mean: meanA, dist: u.d, cs2: u.cs2, children: []*node{b}}
									_ = sh
									return a
								},
							})
						}
					}
				}
			}
		}
	}
	return out
}

func TestChainDamping(t *testing.T) {
	reps, seconds := truthReps(), 400.0
	seed := envInt("ACC_SEED", 31337)
	weights := []float64{0, 0.25, 0.5, 0.65, 0.8, 1}
	type acc struct{ lev, chg []float64 }
	type wr struct {
		name            string
		tc, e0, e65, e1 float64
	}
	var worst []wr
	res := map[string]map[float64]*acc{}
	n := 0
	for i, s := range chainFamily() {
		base, after := s.build(0), s.build(0)
		find(after, s.at).shift += s.shift
		tr := truthRow(base, after, s.lam, seconds, truthSeeds(seed, i, reps))
		tb, ta, tc := tr.Base, tr.After, tr.Change
		n++
		if s.group == "chain-b" {
			e := map[float64]float64{}
			for _, w := range []float64{0, 0.65, 1} {
				cfg := model.Config{ArrivalWeight: w}
				rb, _ := model.AnalyticWith(topo(base, nil), s.lam/1000, nil, cfg)
				ra, _ := model.AnalyticWith(topo(after, nil), s.lam/1000, nil, cfg)
				e[w] = 100 * ((ra.L[0] - rb.L[0]) - tc) / tc
			}
			worst = append(worst, wr{s.name, tc, e[0], e[0.65], e[1]})
		}
		for _, w := range weights {
			cfg := model.Config{ArrivalWeight: w}
			rb, e1 := model.AnalyticWith(topo(base, nil), s.lam/1000, nil, cfg)
			ra, e2 := model.AnalyticWith(topo(after, nil), s.lam/1000, nil, cfg)
			if e1 != nil || e2 != nil || !rb.Stable || !ra.Stable {
				continue
			}
			m := res[s.group]
			if m == nil {
				m = map[float64]*acc{}
				res[s.group] = m
			}
			if m[w] == nil {
				m[w] = &acc{}
			}
			m[w].lev = append(m[w].lev, math.Abs(100*(rb.L[0]-tb)/tb), math.Abs(100*(ra.L[0]-ta)/ta))
			m[w].chg = append(m[w].chg, math.Abs(100*((ra.L[0]-rb.L[0])-tc)/tc))
		}
	}
	if os.Getenv("ACC_WORST") != "" {
		sort.Slice(worst, func(i, j int) bool { return math.Abs(worst[i].e1) > math.Abs(worst[j].e1) })
		for i := 0; i < 14 && i < len(worst); i++ {
			fmt.Printf("  %-58s truth dL %6.2f  change err %%: w=0 %+6.1f  w=.65 %+6.1f  w=1 %+6.1f\n", worst[i].name, worst[i].tc, worst[i].e0, worst[i].e65, worst[i].e1)
		}
	}
	fmt.Printf("%d chain scenarios, seed %d, %d replications of %.0f s each\n", n, seed, reps, seconds)
	fmt.Println("weight on the propagated arrival variability (0 = plain model, 1 = full QNA): median / p90 / max |error| %")
	for _, g := range []string{"chain-a", "chain-b"} {
		fmt.Printf(" group %s (the change is in %s)\n", g, g[len(g)-1:])
		for _, w := range weights {
			a := res[g][w]
			fmt.Printf("   w=%-5.2f level %4.1f / %4.1f / %4.1f    change %4.1f / %4.1f / %4.1f\n", w, med(a.lev), pct(a.lev, 0.9), mx(a.lev), med(a.chg), pct(a.chg, 0.9), mx(a.chg))
		}
	}
}

func pct(x []float64, q float64) float64 {
	y := append([]float64(nil), x...)
	sort.Float64s(y)
	return y[int(q*float64(len(y)-1))]
}

func fanFamily() []scen {
	type d struct {
		d   string
		cs2 float64
	}
	dists := []d{{"exp", 1}, {"gamma", 0.5}, {"gamma", 0.25}, {"h2", 2}}
	var out []scen
	for _, dn := range dists {
		for _, nb := range []int{2, 3} {
			for _, rho := range []float64{0.70, 0.78} {
				for _, at := range []string{"b", "c"} {
					dn, nb, rho, at := dn, nb, rho, at
					means := []float64{22, 18, 20}
					lam := rho / 22 * 1000 / 1.1 // the slowest branch (b, 22 ms) runs at rho/1.1 now, rho after +10%
					shiftOf := map[string]float64{"b": 2.2, "c": 1.8}
					out = append(out, scen{
						name:  fmt.Sprintf("a->%d branches %s%.2g rho%.2f chg@%s", nb, dn.d, dn.cs2, rho, at),
						group: "fanout-" + at, lam: lam, shift: shiftOf[at], at: at,
						build: func(float64) *node {
							var kids []*node
							for i := 0; i < nb; i++ {
								kids = append(kids, &node{name: string(rune('b' + i)), c: 1, pods: 1, mean: means[i], dist: dn.d, cs2: dn.cs2})
							}
							return &node{name: "a", c: 8, pods: 1, mean: 3, parallel: true, children: kids}
						},
					})
				}
			}
		}
	}
	return out
}

// TestFanOut: "matched+r" is the matched maximum with the branch correlation measured in the truth's own
// runs; "matched" leaves it out (independent branches).
func TestFanOut(t *testing.T) {
	reps, seconds := truthReps(), 400.0
	seed := envInt("ACC_SEED", 31337)
	type variant struct {
		name string
		cfg  model.Config
		corr bool
	}
	vs := []variant{{"plain", model.Plain, false}, {"matched", model.Config{MatchedMax: true}, false},
		{"matched+r", model.Config{MatchedMax: true}, true}}
	lev := map[string]map[string][]float64{}
	chg := map[string]map[string][]float64{}
	n := 0
	for i, s := range fanFamily() {
		base, after := s.build(0), s.build(0)
		find(after, s.at).shift += s.shift
		tr := truthRow(base, after, s.lam, seconds, truthSeeds(seed, i, reps))
		tb, ta, tc := tr.Base, tr.After, tr.Change
		r := tr.Corr["a"]
		n++
		for _, v := range vs {
			mk := func(nd *node) *model.Topology {
				if v.corr {
					return topo(nd, tr.Corr)
				}
				return topo(nd, nil)
			}
			rb, e1 := model.AnalyticWith(mk(base), s.lam/1000, nil, v.cfg)
			ra, e2 := model.AnalyticWith(mk(after), s.lam/1000, nil, v.cfg)
			if e1 != nil || e2 != nil || !rb.Stable || !ra.Stable {
				continue
			}
			for _, m := range []*map[string]map[string][]float64{&lev, &chg} {
				if *m == nil {
					*m = map[string]map[string][]float64{}
				}
				if (*m)[v.name] == nil {
					(*m)[v.name] = map[string][]float64{}
				}
			}
			lev[v.name][s.group] = append(lev[v.name][s.group], math.Abs(100*(rb.L[0]-tb)/tb), math.Abs(100*(ra.L[0]-ta)/ta))
			chg[v.name][s.group] = append(chg[v.name][s.group], math.Abs(100*((ra.L[0]-rb.L[0])-tc)/tc))
			if os.Getenv("ACC_WORST") != "" && v.name == "matched+r" {
				fmt.Printf("  %-52s r=%.2f truth L %.1f dL %.2f  model dL %.2f  (error %.2f ms = %.2f%% of the latency)\n", s.name, r, tb, tc, ra.L[0]-rb.L[0], (ra.L[0]-rb.L[0])-tc, 100*math.Abs((ra.L[0]-rb.L[0])-tc)/tb)
			}
		}
	}
	fmt.Printf("%d fan-out scenarios, seed %d (%d replications of %.0f s)\nmedian / p90 / max |error| %%\n", n, seed, reps, seconds)
	for _, v := range vs {
		for _, g := range []string{"fanout-b", "fanout-c"} {
			l, c := lev[v.name][g], chg[v.name][g]
			fmt.Printf("  %-10s %-9s level %4.1f / %4.1f / %4.1f   change %4.1f / %4.1f / %4.1f\n", v.name, g, med(l), pct(l, .9), mx(l), med(c), pct(c, .9), mx(c))
		}
	}
}

// holdoutFamily is a seeded random family the weights were not chosen on: 3-node chains, trees whose
// children are called one after the other, and chains into replicas, with a new service law (lognormal).
func holdoutFamily(count int, seed int64) []scen {
	rng := rand.New(rand.NewSource(seed))
	type d struct {
		d   string
		cs2 float64
	}
	dists := []d{{"exp", 1}, {"gamma", 0.35}, {"gamma", 0.7}, {"h2", 3}, {"lognormal", 1.5}, {"lognormal", 0.6}}
	cs := []int{1, 2, 4}
	var out []scen
	for len(out) < count {
		kind := rng.Intn(3)
		lam := 25 + rng.Float64()*35
		rhoMax := 0.70 + rng.Float64()*0.08
		pick := func() d { return dists[rng.Intn(len(dists))] }
		mk := func(name string, rho float64, c, pods int, dd d) *node {
			return &node{name: name, c: c, pods: pods, mean: rho * float64(c*pods) / lam * 1000, dist: dd.d, cs2: dd.cs2}
		}
		lo := func() float64 { return 0.3 + rng.Float64()*0.4 }
		var root *node
		var nodes []*node
		switch kind {
		case 0: // a -> b -> c
			c3 := mk("c", rhoMax, cs[rng.Intn(3)], 1, pick())
			b := mk("b", lo(), cs[rng.Intn(3)], 1, pick())
			a := mk("a", lo(), cs[rng.Intn(3)], 1, pick())
			b.children, a.children = []*node{c3}, []*node{b}
			root, nodes = a, []*node{a, b, c3}
		case 1: // a calls b, then c
			b := mk("b", rhoMax, cs[rng.Intn(3)], 1, pick())
			c3 := mk("c", lo(), cs[rng.Intn(3)], 1, pick())
			a := mk("a", lo(), cs[rng.Intn(3)], 1, pick())
			a.children = []*node{b, c3}
			root, nodes = a, []*node{a, b, c3}
		default: // a -> b with two replicas
			b := mk("b", rhoMax, 1, 2, pick())
			a := mk("a", lo(), cs[rng.Intn(3)], 1, pick())
			a.children = []*node{b}
			root, nodes = a, []*node{a, b}
		}
		at := nodes[rng.Intn(len(nodes))]
		tree := root
		out = append(out, scen{name: fmt.Sprintf("kind%d chg@%s", kind, at.name), group: fmt.Sprintf("holdout-kind%d", kind),
			build: func(float64) *node { return cloneTree(tree) }, lam: lam, shift: 0.08 * at.mean, at: at.name})
	}
	return out
}

// structureFamily is a seeded family with the trace structures the other families lack, one kind per row in
// turn: glue (own time after the calls, worker free), staged calls (two calls together and one alone, the lone
// one sometimes conditional), conditional calls one after the other, and an uneven split over 2-3 replicas.
// Each kind appears with both span modes: rows 4-7, 12-15, ... have spans that include the wait for a worker,
// whatever ACC_SPANQ says. Callees are distinct (the simulator gives each call of a node its own queue) and
// only callers have glue (a leaf's glue reads as its service time).
func structureFamily(count int, seed int64) []scen {
	rng := rand.New(rand.NewSource(seed + 2))
	type d struct {
		d   string
		cs2 float64
	}
	dists := []d{{"exp", 1}, {"gamma", 0.35}, {"gamma", 0.7}, {"h2", 3}, {"lognormal", 1.5}, {"lognormal", 0.6}}
	cs := []int{1, 2, 4}
	var out []scen
	for j := 0; len(out) < count; j++ {
		lam := 25 + rng.Float64()*35
		rhoMax := 0.70 + rng.Float64()*0.08
		pick := func() d { return dists[rng.Intn(len(dists))] }
		lo := func() float64 { return 0.3 + rng.Float64()*0.4 }
		// mk makes a node at utilization rho when share (of the requests) of its visits reach it
		mk := func(name string, rho float64, c, pods int, share float64, dd d) *node {
			return &node{name: name, c: c, pods: pods, mean: rho * float64(c*pods) / (lam * share) * 1000, dist: dd.d, cs2: dd.cs2}
		}
		rhoOf := func(k, hot int) float64 {
			if k == hot {
				return rhoMax
			}
			return lo()
		}
		var root *node
		var nodes []*node
		kind := []string{"glue", "staged", "conditional", "uneven"}[j%4]
		switch kind {
		case "glue": // a -> b -> c, glue after the calls of a and b
			hot := rng.Intn(3)
			c3 := mk("c", rhoOf(2, hot), cs[rng.Intn(3)], 1, 1, pick())
			b := mk("b", rhoOf(1, hot), cs[rng.Intn(3)], 1, 1, pick())
			a := mk("a", rhoOf(0, hot), cs[rng.Intn(3)], 1, 1, pick())
			a.post, b.post = 1+4*rng.Float64(), 0.5+2.5*rng.Float64()
			b.children, a.children = []*node{c3}, []*node{b}
			root, nodes = a, []*node{a, b, c3}
		case "staged": // a calls {b c} then d, or b then {c d}; the lone call is conditional half the time
			stage := [][]int{{0, 0, 1}, {0, 1, 1}}[rng.Intn(2)]
			lone := 2
			if stage[1] == 1 {
				lone = 0
			}
			p := []float64{1, 1, 1}
			if rng.Intn(2) == 0 {
				p[lone] = 0.4 + 0.5*rng.Float64()
			}
			hot := 1 + rng.Intn(3)
			var kids []*node
			for k, name := range []string{"b", "c", "d"} {
				kids = append(kids, mk(name, rhoOf(k+1, hot), cs[rng.Intn(3)], 1, p[k], pick()))
			}
			a := mk("a", lo(), cs[rng.Intn(3)], 1, 1, pick())
			a.stage, a.children = stage, kids
			if p[lone] < 1 {
				a.p = p
			}
			if rng.Intn(2) == 0 {
				a.post = 0.5 + 1.5*rng.Float64()
			}
			root, nodes = a, append([]*node{a}, kids...)
		case "conditional": // a calls b, then c with probability .2-.8, then d with .05-.35
			p := []float64{1, 0.2 + 0.6*rng.Float64(), 0.05 + 0.3*rng.Float64()}
			hot := 1 + rng.Intn(3)
			var kids []*node
			for k, name := range []string{"b", "c", "d"} {
				kids = append(kids, mk(name, rhoOf(k+1, hot), cs[rng.Intn(3)], 1, p[k], pick()))
			}
			a := mk("a", lo(), cs[rng.Intn(3)], 1, 1, pick())
			a.p, a.children = p, kids
			root, nodes = a, append([]*node{a}, kids...)
		default: // a -> b over 2-3 replicas with uneven shares (the busiest at rhoMax), sometimes -> c
			shares := [][]float64{{0.6, 0.4}, {0.65, 0.35}, {0.7, 0.3}, {0.5, 0.3, 0.2}, {0.45, 0.35, 0.2}, {0.4, 0.4, 0.2}}[rng.Intn(6)]
			c := 1 + rng.Intn(2)
			b := mk("b", rhoMax/(float64(len(shares))*shares[0]), c, len(shares), 1, pick())
			b.shares = shares
			a := mk("a", lo(), cs[rng.Intn(3)], 1, 1, pick())
			a.children = []*node{b}
			root, nodes = a, []*node{a, b}
			if rng.Intn(2) == 0 {
				c3 := mk("c", lo(), cs[rng.Intn(3)], 1, 1, pick())
				b.children = []*node{c3}
				nodes = append(nodes, c3)
			}
		}
		at := nodes[rng.Intn(len(nodes))]
		spanq := (j/4)%2 == 1
		mode := "default"
		if spanq {
			mode = "spanq"
		}
		tree := root
		out = append(out, scen{name: fmt.Sprintf("%s chg@%s %s", kind, at.name, mode), group: "struct-" + kind,
			build: func(float64) *node { return cloneTree(tree) }, lam: lam, shift: 0.08 * at.mean, at: at.name, spanq: &spanq})
	}
	return out
}

// e2eScenarios is the end-to-end harness's rows in order: 30 random holdout scenarios, 6 of the 16 fan-outs
// changed in their slowest branch (picked by the seed), then, with structure set, the structure family's 12. A
// row's index fixes its seeds (rowSeed), so the structure rows come last and leave the other rows alone.
func e2eScenarios(seed int64, structure bool) []scen {
	scs := holdoutFamily(30, seed)
	var fan []scen
	for _, s := range fanFamily() {
		if s.group == "fanout-b" {
			fan = append(fan, s)
		}
	}
	pick := rand.New(rand.NewSource(seed + 1)).Perm(len(fan))[:6]
	sort.Ints(pick)
	for _, i := range pick {
		scs = append(scs, fan[i])
	}
	if structure {
		scs = append(scs, structureFamily(12, seed)...)
	}
	return scs
}

// family is the summary group of a row: holdout, fanout, or the structure kind.
func family(group string) string {
	if strings.HasPrefix(group, "struct-") {
		return group
	}
	return strings.Split(group, "-")[0]
}

func cloneTree(n *node) *node {
	c := *n
	c.children = nil
	for _, ch := range n.children {
		c.children = append(c.children, cloneTree(ch))
	}
	return &c
}

// errs holds a group's |errors| in %, and the z of each row's base level and change.
type errs struct{ lev, chg, chgLvl, zLev, zChg []float64 }

func (e *errs) zText() string {
	l, n := shareOver(e.zLev)
	c, _ := shareOver(e.zChg)
	return fmt.Sprintf("|z|>3: level %4.1f%% change %4.1f%% (n=%d)", l, c, n)
}

func TestHoldout(t *testing.T) {
	reps, seconds := truthReps(), 400.0
	seed := envInt("ACC_SEED", 4242)
	weights := []float64{0, 0.5, 0.65, 0.7, 0.8, 1}
	all := map[float64]*errs{}
	kinds := map[string]map[float64]*errs{}
	slot := func(m map[float64]*errs, w float64) *errs {
		if m[w] == nil {
			m[w] = &errs{}
		}
		return m[w]
	}
	n := 0
	for i, s := range holdoutFamily(60, seed) {
		base, after := s.build(0), s.build(0)
		find(after, s.at).shift += s.shift
		tr := truthRow(base, after, s.lam, seconds, truthSeeds(seed, i, reps))
		tb, ta, tc := tr.Base, tr.After, tr.Change
		n++
		if kinds[s.group] == nil {
			kinds[s.group] = map[float64]*errs{}
		}
		for _, w := range weights {
			cfg := model.Config{ArrivalWeight: w, MatchedMax: true}
			rb, e1 := model.AnalyticWith(topo(base, tr.Corr), s.lam/1000, nil, cfg)
			ra, e2 := model.AnalyticWith(topo(after, tr.Corr), s.lam/1000, nil, cfg)
			if e1 != nil || e2 != nil || !rb.Stable || !ra.Stable {
				continue
			}
			for _, a := range []*errs{slot(all, w), slot(kinds[s.group], w)} {
				a.lev = append(a.lev, math.Abs(100*(rb.L[0]-tb)/tb), math.Abs(100*(ra.L[0]-ta)/ta))
				a.chg = append(a.chg, math.Abs(100*((ra.L[0]-rb.L[0])-tc)/tc))
				a.chgLvl = append(a.chgLvl, math.Abs(100*((ra.L[0]-rb.L[0])-tc)/tb))
				a.zLev = append(a.zLev, zOf(rb.L[0]-tb, tr.BaseSE, 0))
				a.zChg = append(a.zChg, zOf(ra.L[0]-rb.L[0]-tc, tr.ChangeSE, 0))
			}
		}
	}
	fmt.Printf("%d holdout scenarios, seed %d (%d x %.0f s; z = base level or change error / the truth's standard error)\n"+
		"median / p90 / max |error| %%\n", n, seed, reps, seconds)
	for _, w := range weights {
		a := all[w]
		fmt.Printf("  w=%-4.2f level %4.1f / %4.1f / %4.1f   change %4.1f / %4.1f / %4.1f   change error as %% of the latency %4.2f / %4.2f / %4.2f  %s\n", w, med(a.lev), pct(a.lev, .9), mx(a.lev), med(a.chg), pct(a.chg, .9), mx(a.chg), med(a.chgLvl), pct(a.chgLvl, .9), mx(a.chgLvl), a.zText())
	}
	var ks []string
	for k := range kinds {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	for _, k := range ks {
		for _, w := range []float64{0, 0.8, 1} {
			a := kinds[k][w]
			fmt.Printf("    %-14s w=%-4.2f level %4.1f / %4.1f   change %4.1f / %4.1f  (n=%d)\n", k, w, med(a.lev), mx(a.lev), med(a.chg), mx(a.chg), len(a.chg))
		}
	}
}

// e2eRow is one row of the end-to-end harness: its scenario, both states, its truth and the true topology
// (with the branch correlation measured in the truth's runs, as the inference measures it from the capture).
type e2eRow struct {
	row         int
	s           scen
	base, after *node
	tr          truthOut
	tt          *model.Topology
	capSeed     int64
}

func newE2ERow(seed int64, row int, s scen, reps int, seconds float64) *e2eRow {
	r := &e2eRow{row: row, s: s, base: s.build(0), after: s.build(0), capSeed: rowSeed(seed, row, 0)}
	find(r.after, s.at).shift += s.shift
	r.tr = truthRow(r.base, r.after, s.lam, seconds, truthSeeds(seed, row, reps))
	r.tt = topo(r.base, r.tr.Corr)
	return r
}

// score is one model reading of a row. The errors are signed, in %; ZL and ZC are the base level's and the
// change's error over the truth's standard error and the capture's input noise (InputSEL, InputSEC) combined.
type score struct {
	L            float64   `json:"model_ms"`
	DL           float64   `json:"model_change_ms"`
	LevelPct     float64   `json:"level_err_pct"`
	ChangePct    float64   `json:"change_err_pct"`
	ChangeLatPct float64   `json:"change_err_pct_of_latency"`
	InputSEL     float64   `json:"input_se_ms"`
	InputSEC     float64   `json:"input_se_change_ms"`
	ZL           float64   `json:"z_level"`
	ZC           float64   `json:"z_change"`
	Rho          []float64 `json:"rho"`
}

// score runs the model on tp at lam (per ms) with the row's change. visits are the capture's server spans per
// service, for the input noise; nil (the true parameters) leaves it out.
func (r *e2eRow) score(tp *model.Topology, lam float64, visits map[string]float64, cfg model.Config, seconds float64) (score, error) {
	ai, root := tp.Index(r.s.at), tp.Index("a")
	if ai < 0 || root < 0 {
		return score{}, fmt.Errorf("the changed service %q or the root is not in the topology", r.s.at)
	}
	ds := make([]float64, len(tp.Services))
	sh := make([]bool, len(ds))
	ds[ai], sh[ai] = r.s.shift, true
	d, err := model.ChangeShiftWith(tp, lam, ds, sh, cfg)
	if err != nil {
		return score{}, err
	}
	if !d.Stable {
		mb, _ := d.Base.MaxRho()
		ma, _ := d.Patched.MaxRho()
		return score{}, fmt.Errorf("unstable: utilization %.3f before, %.3f after", mb, ma)
	}
	tb, tc := r.tr.Base, r.tr.Change
	sc := score{L: d.Base.L[root], DL: d.DL[root], Rho: d.Base.Rho}
	eL, eC := sc.L-tb, sc.DL-tc
	sc.LevelPct, sc.ChangePct, sc.ChangeLatPct = 100*eL/tb, 100*eC/tc, 100*eC/tb
	if visits != nil {
		sc.InputSEL, sc.InputSEC, err = inputSE(r.tt, r.base, r.s.lam/1000, seconds*1000, visits, r.s.at, r.s.shift, cfg)
		if err != nil {
			sc.InputSEL, sc.InputSEC = math.NaN(), math.NaN()
		}
	}
	sc.ZL, sc.ZC = zOf(eL, r.tr.BaseSE, sc.InputSEL), zOf(eC, r.tr.ChangeSE, sc.InputSEC)
	return sc, nil
}

func (e *errs) add(sc score) {
	e.lev, e.chg, e.chgLvl = append(e.lev, math.Abs(sc.LevelPct)), append(e.chg, math.Abs(sc.ChangePct)), append(e.chgLvl, math.Abs(sc.ChangeLatPct))
	e.zLev, e.zChg = append(e.zLev, sc.ZL), append(e.zChg, sc.ZC)
}

// TestEndToEndAccuracy runs the whole path (trace -> inference -> model) on one simulated 400 s trace per row
// and compares the basic and the refined model with the truth: ACC_TRUTH_REPS (64) x 400 s, the base and the
// changed state on the same seeds. Row i's capture and truth seeds come from ACC_SEED and i (rowSeed). z is a
// row's error over the truth's standard error and the capture's input noise (inputSE) combined.
//
//	ACC_SEED=7 ACC_WORST=1 go test -tags accuracy -run TestEndToEndAccuracy -v ./internal/infer
//
// ACC_WORST lists the large errors (and |z| > 3) with inferred and true parameters; ACC_SPANQ=1 makes the spans
// include the wait for a worker (the structure rows keep their own mode); RHOBOUND_ESTIMATOR=rise selects the
// earlier estimator; ACC_STRUCT=1 adds the structure family; ACC_ROWS=3,17 runs those rows only; ACC_DUMP=DIR
// writes each row run to DIR/seed<S>-row<NN> (dumpRow); ACC_NOISE=K also infers K more captures of each row and
// prints the spread of their predictions next to inputSE's, and their mean error with its z (the truth's
// standard error and that of the mean over the K captures).
func TestEndToEndAccuracy(t *testing.T) {
	start := time.Now()
	reps, seconds := truthReps(), 400.0
	seed := envInt("ACC_SEED", 31337)
	scs := e2eScenarios(seed, os.Getenv("ACC_STRUCT") != "")
	rows := rowsFromEnv()
	dump, noise, worst := os.Getenv("ACC_DUMP"), int(envInt("ACC_NOISE", 0)), os.Getenv("ACC_WORST") != ""
	names := []string{"basic", "refined"}
	cfgs := []model.Config{model.Plain, model.Default}
	res := map[string]map[string]*errs{}
	add := func(label, nm string, sc score) {
		if res[label] == nil {
			res[label] = map[string]*errs{"basic": {}, "refined": {}}
		}
		res[label][nm].add(sc)
	}
	var ratioL, ratioC []float64 // refined: the K captures' spread over inputSE
	n := 0
	for i, s := range scs {
		if !rows.has(i) {
			continue
		}
		r := newE2ERow(seed, i, s, reps, seconds)
		tr := r.tr
		spanq := os.Getenv("ACC_SPANQ") != ""
		if s.spanq != nil {
			spanq = *s.spanq
		}
		opts := simOpts{queueInSpan: spanq}
		rec := newCaptureRecorder()
		if dump != "" {
			opts.rec = rec.hook
		}
		out := runWith(r.base, s.lam, seconds, r.capSeed, opts)
		d, err := readCapture(t, out.spans)
		if err != nil {
			t.Fatal(err)
		}
		inf, err := infer.Run(d, infer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%02d %s", i, s.name)
		if inf.Topology.Index(s.at) < 0 {
			fmt.Printf("    skipped %s [%s]: the changed service %q is not in the inferred topology\n", name, s.group, s.at)
			continue
		}
		visits := serverVisits(out.spans)
		g := family(s.group)
		total := "ALL"
		if strings.HasPrefix(g, "struct-") {
			total = "ALL struct"
		}
		n++
		scored := map[string]any{}
		for ci, c := range cfgs {
			sc, err := r.score(inf.Topology, inf.LamExtPerMs, visits, c, seconds)
			if err != nil {
				fmt.Printf("    %s unscored %s [%s]: %v\n", names[ci], name, s.group, err)
				if ci == 1 {
					for _, sv := range inf.Topology.Services {
						fmt.Printf("      inferred %-2s S %.2f cs2 %.2f servers %d workers %d", sv.Name, sv.S, sv.CS2, sv.Servers, sv.Workers)
						if j := r.tt.Index(sv.Name); j >= 0 {
							tv := r.tt.Services[j]
							fmt.Printf("   true S %.2f cs2 %.2f servers %d workers %d", tv.S, tv.CS2, tv.Servers, tv.Workers)
						}
						fmt.Printf("   lam %.4f/ms true-lam %.4f\n", inf.LamExtPerMs, s.lam/1000)
					}
				}
				continue
			}
			scored[names[ci]] = sc
			if worst && ci == 1 && (math.Abs(sc.LevelPct) > 8 || math.Abs(sc.ChangeLatPct) > 3 || math.Abs(sc.ZL) > 3 || math.Abs(sc.ZC) > 3) {
				fmt.Printf("    LARGE %s [%s] level err %+.1f%% (truth %.1f se %.2f ms, model %.1f, input se %.2f, z %+.1f); "+
					"change truth %.2f se %.3f model %.2f (%+.2f%% of latency, input se %.3f, z %+.1f)\n",
					name, s.group, sc.LevelPct, tr.Base, tr.BaseSE, sc.L, sc.InputSEL, sc.ZL, tr.Change, tr.ChangeSE, sc.DL, sc.ChangeLatPct, sc.InputSEC, sc.ZC)
				for k, sv := range inf.Topology.Services {
					fmt.Printf("        %-2s inferred S %6.2f cs2 %.2f cap %d (%dx%d) rho %.2f", sv.Name, sv.S, sv.CS2, sv.Servers*sv.Workers, sv.Servers, sv.Workers, sc.Rho[k])
					if j := r.tt.Index(sv.Name); j >= 0 {
						tv := r.tt.Services[j]
						fmt.Printf(" | true S %6.2f cs2 %.2f cap %d", tv.S, tv.CS2, tv.Servers*tv.Workers)
					}
					fmt.Println()
				}
			}
			if worst && math.Abs(sc.ChangePct) > 25 {
				fmt.Printf("    %-8s %-26s truth dL %6.2f (%.1f%% of %.1f ms)  model dL %6.2f  err %.1f%% of dL, %.2f%% of latency\n",
					names[ci], name+" ["+s.group+"]", tr.Change, 100*tr.Change/tr.Base, tr.Base, sc.DL, math.Abs(sc.ChangePct), math.Abs(sc.ChangeLatPct))
			}
			add(total, names[ci], sc)
			add(g, names[ci], sc)
		}
		if noise > 0 {
			type pred struct {
				l, dl [2]float64
				ok    [2]bool
			}
			ps := make([]pred, noise)
			parallelFor(noise, func(j int) {
				o := runWith(r.base, s.lam, seconds, rowSeed(seed, i, seedNoise+j), simOpts{queueInSpan: spanq})
				d, err := readCapture(t, o.spans)
				if err != nil {
					return
				}
				inf2, err := infer.Run(d, infer.Options{})
				if err != nil {
					return
				}
				for ci, c := range cfgs {
					if sc, err := r.score(inf2.Topology, inf2.LamExtPerMs, nil, c, seconds); err == nil {
						ps[j].l[ci], ps[j].dl[ci], ps[j].ok[ci] = sc.L, sc.DL, true
					}
				}
			})
			for ci := range cfgs {
				var ls, dls []float64
				for _, p := range ps {
					if p.ok[ci] {
						ls, dls = append(ls, p.l[ci]), append(dls, p.dl[ci])
					}
				}
				sc, ok := scored[names[ci]].(score)
				if len(ls) < 2 || !ok {
					continue
				}
				mL, seL := meanSE(ls)
				mC, seC := meanSE(dls)
				sdL, sdC := seL*math.Sqrt(float64(len(ls))), seC*math.Sqrt(float64(len(dls)))
				// the mean over the captures has its input noise averaged out: its z tests the pipeline's bias
				fmt.Printf("    NOISE %s %-30s %d captures: level sd %.2f ms (input se %.2f), mean error %+.2f%% (z %+.1f); "+
					"change sd %.3f ms (input se %.3f), mean error %+.2f%% of the latency (z %+.1f)\n", names[ci], name, len(ls),
					sdL, sc.InputSEL, 100*(mL-tr.Base)/tr.Base, zOf(mL-tr.Base, tr.BaseSE, seL),
					sdC, sc.InputSEC, 100*(mC-tr.Change)/tr.Base, zOf(mC-tr.Change, tr.ChangeSE, seC))
				if ci == 1 && sc.InputSEL > 0 && sc.InputSEC > 0 {
					ratioL, ratioC = append(ratioL, sdL/sc.InputSEL), append(ratioC, sdC/sc.InputSEC)
				}
			}
		}
		if dump != "" {
			dumpRow(t, filepath.Join(dump, fmt.Sprintf("seed%d-row%02d", seed, i)), out.spans, r.base, r.after, tr.Corr, inf.Topology,
				map[string]any{
					"scenario": map[string]any{"acc_seed": seed, "row": i, "name": s.name, "group": s.group, "lam_rps": s.lam,
						"shift_ms": s.shift, "at": s.at, "spans_include_wait": spanq, "seconds": seconds, "capture_seed": r.capSeed,
						"hop_one_way_ms": hopMs},
					"truth":    tr,
					"capture":  map[string]any{"requests": len(out.totalMs), "spans": len(out.spans), "nodes": rec.records(r.base)},
					"inferred": map[string]any{"lam_ext_per_ms": inf.LamExtPerMs, "requests": inf.Requests, "window_s": inf.WindowS, "warnings": inf.Warnings, "stats": inf.Stats},
					"scored":   scored,
				})
		}
	}
	fmt.Printf("%d scenarios, seed %d, parameters inferred from one %.0f s trace each (truth: %d x %.0f s, common random numbers)\n"+
		"median / p90 / max |error| %%; |z|>3 is the share of rows whose error exceeds 3 combined standard errors (truth, capture)\n",
		n, seed, seconds, reps, seconds)
	var labels []string
	for g := range res {
		if !strings.HasPrefix(g, "ALL") {
			labels = append(labels, g)
		}
	}
	sort.Strings(labels)
	for _, label := range append([]string{"ALL", "ALL struct"}, labels...) {
		m := res[label]
		if m == nil {
			continue
		}
		for _, nm := range names {
			a := m[nm]
			if len(a.lev) == 0 {
				continue
			}
			fmt.Printf("  %-17s %-8s level %4.1f / %4.1f / %4.1f   change %4.1f / %4.1f / %4.1f   change error as %% of latency %4.2f / %4.2f / %4.2f  %s\n",
				label, nm, med(a.lev), pct(a.lev, .9), mx(a.lev), med(a.chg), pct(a.chg, .9), mx(a.chg), med(a.chgLvl), pct(a.chgLvl, .9), mx(a.chgLvl), a.zText())
		}
	}
	if len(ratioL) > 0 {
		fmt.Printf("ACC_NOISE: the captures' spread over inputSE (refined), median / min / max: level %.2f / %.2f / %.2f, change %.2f / %.2f / %.2f (%d rows)\n",
			med(ratioL), mn(ratioL), mx(ratioL), med(ratioC), mn(ratioC), mx(ratioC), len(ratioL))
	}
	fmt.Printf("took %.0f s at GOMAXPROCS %d\n", time.Since(start).Seconds(), runtime.GOMAXPROCS(0))
}

func mn(x []float64) float64 {
	m := math.Inf(1)
	for _, v := range x {
		m = math.Min(m, v)
	}
	return m
}

// TestEndToEndAB is TestEndToEndAccuracy for both span modes (spans start at service / include the wait)
// and both estimators (the FIFO reconstruction, "fifo", and the earlier rise detection, "rise"), with the
// truth computed once per row (the same rows, seeds and truth as TestEndToEndAccuracy; a structure row's own
// span mode is ignored, both are run). Each scored row is printed as a ROW line (tab-separated) and the
// inferred parameters as PAR lines, for offline tables; ROW and ROWTRUE end with the truth's standard errors
// (level, change), the input noise (level, change; ROW only) and z (level, change).
//
//	ACC_SEED=5 go test -tags accuracy -run TestEndToEndAB -v -count=1 ./internal/infer
func TestEndToEndAB(t *testing.T) {
	start := time.Now()
	reps, seconds := truthReps(), 400.0
	seed := envInt("ACC_SEED", 31337)
	scs := e2eScenarios(seed, os.Getenv("ACC_STRUCT") != "")
	rows := rowsFromEnv()
	names := []string{"basic", "refined"}
	cfgs := []model.Config{model.Plain, model.Default}
	modes := []string{"default", "spanq"}
	ests := []string{infer.EstimatorRise, infer.EstimatorFIFO}
	type key struct{ mode, est, mdl string }
	res := map[key]*errs{}
	for _, m := range modes {
		for _, e := range ests {
			for _, nm := range names {
				res[key{m, e, nm}] = &errs{}
			}
		}
	}
	n := 0
	for i, s := range scs {
		if !rows.has(i) {
			continue
		}
		n++
		r := newE2ERow(seed, i, s, reps, seconds)
		tr, tt := r.tr, r.tt
		name := fmt.Sprintf("%02d %s", i, s.name) // names repeat within a family
		// the model with the TRUE parameters and rate: what is left when inference is exact
		for ci, c := range cfgs {
			sc, err := r.score(tt, s.lam/1000, nil, c, seconds)
			if err != nil {
				fmt.Printf("UNSCORED\t%d\ttrue\t-\t%s\t%s\t%v\n", seed, names[ci], name, err)
				continue
			}
			fmt.Printf("ROWTRUE\t%d\t%s\t%s\t%s\t%+.3f\t%+.3f\t%+.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%+.2f\t%+.2f\n", seed, names[ci], name, s.group,
				sc.LevelPct, sc.ChangePct, sc.ChangeLatPct, tr.Base, tr.Change, sc.L, sc.DL, tr.BaseSE, tr.ChangeSE, sc.ZL, sc.ZC)
		}
		for _, mode := range modes {
			out := runWith(r.base, s.lam, seconds, r.capSeed, simOpts{queueInSpan: mode == "spanq"})
			d, err := readCapture(t, out.spans)
			if err != nil {
				t.Fatal(err)
			}
			visits := serverVisits(out.spans)
			for _, est := range ests {
				inf, err := infer.Run(d, infer.Options{Estimator: est})
				if err != nil {
					t.Fatal(err)
				}
				if inf.Topology.Index(s.at) < 0 {
					fmt.Printf("SKIP\t%d\t%s\t%s\t%s\n", seed, mode, est, name)
					continue
				}
				for k, sv := range inf.Topology.Services {
					j := tt.Index(sv.Name)
					if j < 0 {
						continue
					}
					st := inf.Stats[k]
					fmt.Printf("PAR\t%d\t%s\t%s\t%s\t%s\t%s\t%.4f\t%.4f\t%d\t%d\t%v\t%.4f\t%.4f\t%d\t%d\n", seed, mode, est, name, s.group, sv.Name,
						sv.S, sv.CS2, sv.Servers, sv.Workers, st.WorkersFromQueueing, tt.Services[j].S, tt.Services[j].CS2, tt.Services[j].Servers, tt.Services[j].Workers)
				}
				for ci, c := range cfgs {
					sc, err := r.score(inf.Topology, inf.LamExtPerMs, visits, c, seconds)
					if err != nil {
						fmt.Printf("UNSCORED\t%d\t%s\t%s\t%s\t%s\t%v\n", seed, mode, est, names[ci], name, err)
						continue
					}
					fmt.Printf("ROW\t%d\t%s\t%s\t%s\t%s\t%s\t%+.3f\t%+.3f\t%+.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%.3f\t%+.2f\t%+.2f\n",
						seed, mode, est, names[ci], name, s.group, sc.LevelPct, sc.ChangePct, sc.ChangeLatPct, tr.Base, tr.Change, sc.L, sc.DL,
						tr.BaseSE, tr.ChangeSE, sc.InputSEL, sc.InputSEC, sc.ZL, sc.ZC)
					res[key{mode, est, names[ci]}].add(sc)
				}
			}
		}
	}
	fmt.Printf("%d scenarios, seed %d, parameters inferred from one %.0f s trace each (truth: %d x %.0f s, common random numbers)\n"+
		"median / p90 / max |error| %%\n", n, seed, seconds, reps, seconds)
	for _, m := range modes {
		for _, e := range ests {
			for _, nm := range names {
				a := res[key{m, e, nm}]
				fmt.Printf("SUM\t%d\t%-7s\t%-4s\t%-7s\tlevel %4.1f / %4.1f / %4.1f   change %4.1f / %4.1f / %4.1f   change error as %% of latency %4.2f / %4.2f / %4.2f  (n=%d)  %s\n",
					seed, m, e, nm, med(a.lev), pct(a.lev, .9), mx(a.lev), med(a.chg), pct(a.chg, .9), mx(a.chg), med(a.chgLvl), pct(a.chgLvl, .9), mx(a.chgLvl), len(a.lev), a.zText())
			}
		}
	}
	fmt.Printf("took %.0f s at GOMAXPROCS %d\n", time.Since(start).Seconds(), runtime.GOMAXPROCS(0))
}

// TestGlueEndToEnd: a (12 ms exponential, then 3 ms after its call with the worker free) -> b (20 ms), 37.5
// requests/s, with a post = 0 control. Truth from ACC_TRUTH_REPS (64) x 400 s runs with common random numbers
// for the change (its standard error is that of the paired differences); the model with the true parameters
// and with the parameters inferred from one 400 s trace, in both span modes. Each case is a row for the seeds.
//
//	go test -tags accuracy -run TestGlueEndToEnd -v ./internal/infer
func TestGlueEndToEnd(t *testing.T) {
	const lam, seconds = 37.5, 400.0
	reps, seed := truthReps(), envInt("ACC_SEED", 31337)
	row := -1
	for _, post := range []float64{0, 3} {
		build := func() *node {
			b := &node{name: "b", c: 1, pods: 1, mean: 20}
			return &node{name: "a", c: 1, pods: 1, mean: 12, post: post, children: []*node{b}}
		}
		for _, at := range []string{"a", "b"} {
			row++
			base, after := build(), build()
			shift := 0.08 * find(base, at).mean
			find(after, at).shift += shift
			tr := truthRow(base, after, lam, seconds, truthSeeds(seed, row, reps))
			tb, tc, se := tr.Base, tr.Change, tr.ChangeSE
			predict := func(tp *model.Topology, lamPerMs float64) (float64, float64) {
				i := tp.Index(at)
				ds := make([]float64, len(tp.Services))
				sh := make([]bool, len(ds))
				ds[i], sh[i] = shift, true
				d, err := model.ChangeShiftWith(tp, lamPerMs, ds, sh, model.Default)
				if err != nil || !d.Stable {
					t.Fatalf("post %v at %s: %v", post, at, err)
				}
				return d.Base.L[0], d.DL[0]
			}
			tl, tdl := predict(topo(base, tr.Corr), lam/1000)
			fmt.Printf("GLUE post %.0f chg@%s truth L %.2f dL %.2f (se %.2f) | true parameters L %.2f (%+.1f%%) dL %.2f (%+.1f%%)\n",
				post, at, tb, tc, se, tl, 100*(tl-tb)/tb, tdl, 100*(tdl-tc)/tc)
			for _, inSpan := range []bool{false, true} {
				o := run(base, lam, seconds, rowSeed(seed, row, 0), inSpan)
				inf := infer1(t, o.spans)
				ai := inf.Topology.Index("a")
				sa := inf.Topology.Services[ai]
				l, dl := predict(inf.Topology, inf.LamExtPerMs)
				fmt.Printf("GLUE   spans include the wait %-5v a S %.2f glue %.2f cap %d | L %.2f (%+.1f%%) dL %.2f (%+.1f%% of dL, %+.2f%% of L)\n",
					inSpan, sa.S, sa.Glue, sa.Servers*sa.Workers, l, 100*(l-tb)/tb, dl, 100*(dl-tc)/tc, 100*(dl-tc)/tb)
			}
		}
	}
}

// TestStructureEndToEnd: staged and conditional calls at 60-75% utilization. Truth from ACC_TRUTH_REPS (64) x
// 400 s runs with common random numbers for the change; the model with the true parameters (with the measured
// branch correlation of the stages) and with the parameters inferred from one 400 s trace, in both span modes.
// For the staged shapes it also prints the reading the port made before the structure rules (every
// overlapping visit a parallel fan-out of all the calls). Each case is a row for the seeds.
//
//	go test -tags accuracy -run TestStructureEndToEnd -v ./internal/infer
func TestStructureEndToEnd(t *testing.T) {
	const seconds = 400.0
	reps, seed := truthReps(), envInt("ACC_SEED", 31337)
	leaf := func(n string, mean float64, dist string, cs2 float64) *node {
		return &node{name: n, c: 1, pods: 1, mean: mean, dist: dist, cs2: cs2}
	}
	cases := []struct {
		name  string
		lam   float64
		at    string
		build func() *node
	}{
		{"staged {b c} then d", 35, "d", func() *node {
			return &node{name: "a", c: 4, pods: 1, mean: 3, stage: []int{0, 0, 1},
				children: []*node{leaf("b", 20, "", 0), leaf("c", 18, "gamma", 0.5), leaf("d", 20, "", 0)}}
		}},
		{"staged {b c} then d p.6", 35, "b", func() *node {
			return &node{name: "a", c: 4, pods: 1, mean: 3, stage: []int{0, 0, 1}, p: []float64{1, 1, 0.6},
				children: []*node{leaf("b", 20, "", 0), leaf("c", 18, "gamma", 0.5), leaf("d", 20, "", 0)}}
		}},
		{"staged b then {c d e}", 30, "c", func() *node {
			return &node{name: "a", c: 4, pods: 1, mean: 3, stage: []int{0, 1, 1, 1},
				children: []*node{leaf("b", 15, "", 0), leaf("c", 22, "", 0), leaf("d", 20, "h2", 2), leaf("e", 18, "", 0)}}
		}},
		// distinct callees: the simulator gives each call of a node its own queue, so a repeated call to one
		// service (k + 1 calls with probability p) is checked for its counts only (TestConditionalCalls)
		{"conditional d, e p.3, f p.05", 30, "e", func() *node {
			return &node{name: "a", c: 2, pods: 1, mean: 12, p: []float64{1, 0.3, 0.05},
				children: []*node{{name: "d", c: 2, pods: 1, mean: 45}, leaf("e", 70, "gamma", 0.5), leaf("f", 30, "", 0)}}
		}},
	}
	for row, c := range cases {
		base, after := c.build(), c.build()
		shift := 0.08 * find(base, c.at).mean
		find(after, c.at).shift += shift
		tr := truthRow(base, after, c.lam, seconds, truthSeeds(seed, row, reps))
		tb, tc, se := tr.Base, tr.Change, tr.ChangeSE
		predict := func(tp *model.Topology, lamPerMs float64) (float64, float64, float64) {
			i := tp.Index(c.at)
			ds := make([]float64, len(tp.Services))
			sh := make([]bool, len(ds))
			ds[i], sh[i] = shift, true
			d, err := model.ChangeShiftWith(tp, lamPerMs, ds, sh, model.Default)
			if err != nil || !d.Stable {
				t.Fatalf("%s: %v", c.name, err)
			}
			mr, _ := d.Base.MaxRho()
			return d.Base.L[0], d.DL[0], mr
		}
		tl, tdl, rho := predict(topo(base, tr.Corr), c.lam/1000)
		fmt.Printf("STRUCT %-34s rho %.2f truth L %.2f dL %.2f (se %.2f) | true parameters L %.2f (%+.1f%%) dL %.2f (%+.1f%%)\n",
			c.name, rho, tb, tc, se, tl, 100*(tl-tb)/tb, tdl, 100*(tdl-tc)/tc)
		for _, inSpan := range []bool{false, true} {
			o := run(base, c.lam, seconds, rowSeed(seed, row, 0), inSpan)
			inf := infer1(t, o.spans)
			l, dl, _ := predict(inf.Topology, inf.LamExtPerMs)
			fmt.Printf("STRUCT   spans include the wait %-5v inferred     L %.2f (%+.1f%%) dL %.2f (%+.1f%% of dL, %+.2f%% of L)  %s\n",
				inSpan, l, 100*(l-tb)/tb, dl, 100*(dl-tc)/tc, 100*(dl-tc)/tb, callsText(inf.Topology, 0))
			if base.stage == nil {
				continue
			}
			// the reading before the structure rules: one parallel fan-out over every call
			flat := *inf.Topology
			flat.Services = append([]model.Service(nil), inf.Topology.Services...)
			sa := flat.Services[0]
			sa.Parallel, sa.Calls = true, nil
			for _, ch := range base.children {
				sa.Calls = append(sa.Calls, model.Call{Child: flat.Index(ch.name), P: 1})
			}
			flat.Services[0] = sa
			if err := flat.Finish(); err != nil {
				t.Fatal(err)
			}
			fl, fdl, _ := predict(&flat, inf.LamExtPerMs)
			fmt.Printf("STRUCT   spans include the wait %-5v all-parallel L %.2f (%+.1f%%) dL %.2f (%+.1f%% of dL)\n",
				inSpan, fl, 100*(fl-tb)/tb, fdl, 100*(fdl-tc)/tc)
		}
	}
}

// callsText lists service i's calls, by name and probability.
func callsText(tp *model.Topology, i int) string {
	var parts []string
	for _, c := range tp.Services[i].Calls {
		s := tp.Services[c.Child].Name
		if len(tp.Services[c.Child].Calls) > 0 && tp.Services[c.Child].Delay {
			s += "(" + callsText(tp, c.Child) + ")"
		}
		if c.P < 1 {
			s += fmt.Sprintf(" p%.3f", c.P)
		}
		parts = append(parts, s)
	}
	sep := ", "
	if tp.Services[i].Parallel {
		sep = " || "
	}
	return strings.Join(parts, sep)
}
