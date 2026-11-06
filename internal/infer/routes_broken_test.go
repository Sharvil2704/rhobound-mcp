// SPDX-License-Identifier: Apache-2.0

package infer

// Route weights when traces break (INFER-1's remaining case, maze-kit lean/Rbproofs/Lanes/Infer.lean). A caller's
// route weights (Call.RouteQ) are read from its links into the routed service; a broken trace whose link still has its
// caller's visit counts too, so that each caller's weights are its own mix whatever the other callers lose, and only
// the visits whose caller is not in the capture are shared out by the route's completeness.

import (
	"math"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// routeQOf is caller's route weights into callee by route name (nil without weights).
func routeQOf(t *testing.T, inf *Inferred, caller, callee string) map[string]float64 {
	t.Helper()
	topo := inf.Topology
	i, x := topo.Index(caller), topo.Index(callee)
	if i < 0 || x < 0 {
		t.Fatalf("services %v", topo.Names())
	}
	for _, c := range topo.Services[i].Calls {
		if c.Child != x || c.RouteQ == nil {
			continue
		}
		q := map[string]float64{}
		for k, r := range topo.Services[x].Routes {
			q[r.Name] = c.RouteQ[k]
		}
		return q
	}
	return nil
}

// rbCaller adds n traces of an entry service svc (its own pod) that calls catalog once per visit: the first nA of
// every 10 visits reach GET /a (20 ms), the rest GET /b (1 ms). Visit i starts at 100 i + off ms. lose(i, route)
// says what the trace loses: "" nothing, "orphan" an INTERNAL span of svc whose parent is not in the capture (the
// link keeps its caller's visit), "client" svc's client span (catalog's span names a parent not in the capture).
func rbCaller(x *wfIDs, sp []wfSpan, svc string, n, nA int, off float64, tr *int, lose func(i int, route string) string) []wfSpan {
	for i := 0; i < n; i++ {
		t0 := 100*float64(i) + off
		route, own := "GET /a", 20.0
		if i%10 >= nA {
			route, own = "GET /b", 1.0
		}
		*tr++
		w, wc := x.next(), x.next()
		what := lose(i, route)
		sp = append(sp, wfSpan{svc: svc, pod: svc + "-0", id: w, kind: 2, start: t0, end: t0 + own + 3, trace: *tr, name: "GET /"})
		parent := wc
		if what == "client" {
			parent = "00000000deadbeef"
		} else {
			sp = append(sp, wfSpan{svc: svc, pod: svc + "-0", id: wc, parent: w, kind: 3, start: t0 + 1, end: t0 + 1 + own + 0.4,
				trace: *tr, peer: "catalog"})
		}
		sp = append(sp, wfSpan{svc: "catalog", pod: "catalog-0", id: x.next(), parent: parent, kind: 2, start: t0 + 1.2,
			end: t0 + 1.2 + own, trace: *tr, name: route})
		if what == "orphan" {
			sp = append(sp, wfSpan{svc: svc, pod: svc + "-0", id: x.next(), parent: "00000000deadbeef", kind: 1,
				start: t0 + 0.5, end: t0 + 0.6, trace: *tr})
		}
	}
	return sp
}

// rbEntries adds n entry visits of catalog's GET /a (20 ms) at 100 i + 50 ms, each in its own trace: a top span
// (outside == false) or one whose parent, the load generator's call, is not in the capture. broken of them lose an
// INTERNAL span of catalog below the visit.
func rbEntries(x *wfIDs, sp []wfSpan, n, broken int, outside bool, tr *int) []wfSpan {
	for i := 0; i < n; i++ {
		t0 := 100*float64(i) + 50
		*tr++
		parent := ""
		if outside {
			parent = "00000000feedface"
		}
		c := x.next()
		sp = append(sp, wfSpan{svc: "catalog", pod: "catalog-0", id: c, parent: parent, kind: 2, start: t0, end: t0 + 20,
			trace: *tr, name: "GET /a"})
		if i < broken {
			sp = append(sp, wfSpan{svc: "catalog", pod: "catalog-0", id: x.next(), parent: "00000000deadbeef", kind: 1,
				start: t0 + 1, end: t0 + 2, trace: *tr})
		}
	}
	return sp
}

func checkRouteQ(t *testing.T, what string, inf *Inferred, caller string, want map[string]float64) {
	t.Helper()
	q := routeQOf(t, inf, caller, "catalog")
	if q == nil {
		t.Fatalf("%s: %s has no route weights into catalog", what, caller)
	}
	for r, w := range want {
		if math.Abs(q[r]-w) > 1e-9 {
			t.Errorf("%s: %s's route weights %v, want its own mix %v", what, caller, q, want)
			return
		}
	}
}

// Two callers of catalog with their own mixes (web 70% /a, mobile 30% /a); 28 of web's 280 /a traces lose an
// INTERNAL span of web (3.5% of the traces). Before the fix, the callers' counts were of complete traces scaled by
// each route's completeness: web got /a 0.6930 (want 0.7) and mobile 0.3155 (want 0.3), as the 28 lost /a visits
// were shared over both callers. Each link keeps its caller's visit, so each caller's weights are its own mix.
func TestRouteWeightsTwoCallersBrokenAtDifferentRates(t *testing.T) {
	x, tr := &wfIDs{}, 0
	var sp []wfSpan
	lost := 0
	sp = rbCaller(x, sp, "web", 400, 7, 0, &tr, func(i int, route string) string {
		if route == "GET /a" && lost < 28 {
			lost++
			return "orphan"
		}
		return ""
	})
	sp = rbCaller(x, sp, "mobile", 400, 3, 50, &tr, func(int, string) string { return "" })
	inf, err := wfRun(t, sp)
	if err != nil {
		t.Fatal(err)
	}
	if inf.BrokenTraces != 28 {
		t.Fatalf("broken traces %d, want 28", inf.BrokenTraces)
	}
	for _, r := range wfRoutesOf(inf, "catalog") {
		t.Logf("catalog route %s: visits %d share %.4f callers %+v", r.Route, r.Visits, r.Share, r.Callers)
	}
	t.Logf("web %v, mobile %v", routeQOf(t, inf, "web", "catalog"), routeQOf(t, inf, "mobile", "catalog"))
	checkRouteQ(t, "two callers", inf, "web", map[string]float64{"GET /a": 0.7, "GET /b": 0.3})
	checkRouteQ(t, "two callers", inf, "mobile", map[string]float64{"GET /a": 0.3, "GET /b": 0.7})
}

// A sole caller whose own client span is lost on 18 of its 200 /b visits: those visits have no caller in the capture,
// and the route's completeness shares them out, so the sole caller still gets the shares (dm 0).
func TestRouteWeightsSoleCallerLostClientSpans(t *testing.T) {
	x, tr := &wfIDs{}, 0
	lost := 0
	sp := rbCaller(x, nil, "web", 400, 5, 0, &tr, func(i int, route string) string {
		if route == "GET /b" && lost < 18 {
			lost++
			return "client"
		}
		return ""
	})
	inf, err := wfRun(t, sp)
	if err != nil {
		t.Fatal(err)
	}
	if inf.BrokenTraces != 18 {
		t.Fatalf("broken traces %d, want 18", inf.BrokenTraces)
	}
	cat := inf.Topology.Index("catalog")
	if inf.Stats[cat].Entries != 0 {
		t.Fatalf("catalog has entries: %d", inf.Stats[cat].Entries)
	}
	checkRouteQ(t, "sole caller", inf, "web", map[string]float64{"GET /a": 0.5, "GET /b": 0.5})
	q := routeQOf(t, inf, "web", "catalog")
	res, err := model.Analytic(inf.Topology, inf.LamExtPerMs, nil)
	if err != nil {
		t.Fatal(err)
	}
	qs := make([]float64, len(inf.Topology.Services[cat].Routes))
	for k, r := range inf.Topology.Services[cat].Routes {
		qs[k] = q[r.Name]
	}
	if dm, _ := inf.Topology.RouteAdj(cat, qs, res.L, nil, nil, nil); math.Abs(dm) > 1e-9 {
		t.Errorf("sole caller moved off the blend by %v ms", dm)
	}
}

// A caller (web, 50% /a) and entries of catalog's /a, some of whose traces break below the entry: an entry is still an
// entry in a broken trace (a top span, or one entered from outside the capture as an entry-like service's is), so the
// lost /a traces are not shared over web. Before the fix web got /a 0.5128 (top entries, 20 of 200 broken) and
// 0.5128 (entries from outside, 40 of 600 broken).
func TestRouteWeightsCallerAndBrokenEntries(t *testing.T) {
	for _, outside := range []bool{false, true} {
		what := map[bool]string{false: "top entries", true: "entries from outside"}[outside]
		x, tr := &wfIDs{}, 0
		sp := rbCaller(x, nil, "web", 400, 5, 0, &tr, func(int, string) string { return "" })
		n, broken := 200, 20
		if outside {
			n, broken = 600, 40 // catalog entry-like: more of its visits come from outside than from web
		}
		sp = rbEntries(x, sp, n, broken, outside, &tr)
		inf, err := wfRun(t, sp)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if inf.BrokenTraces != int64(broken) {
			t.Fatalf("%s: broken traces %d, want %d", what, inf.BrokenTraces, broken)
		}
		t.Logf("%s: web %v", what, routeQOf(t, inf, "web", "catalog"))
		checkRouteQ(t, what, inf, "web", map[string]float64{"GET /a": 0.5, "GET /b": 0.5})
	}
}

// The same on the internal functions: web is catalog's sole caller; route /b has 10 visits, 5 in complete traces, 3
// in broken traces that kept web's visit (routeKnown) and 2 whose caller was lost. web's counts take the 3 (8 in all),
// so /b's completeness is 10/8, not 10/5, and web's weights are the shares; scaling its 8 by 10/5 would count the 3
// twice.
func TestRouteWeightsKnownCallerInBrokenTraces(t *testing.T) {
	a := &acc{routeEdge: map[routeEdgeKey]int64{{0, 1, 1}: 10, {0, 1, 2}: 8}, visits: []int64{20, 20}, sVisits: []int64{17, 17},
		entries: []int64{17, 0}}
	for k := 0; k < 20; k++ {
		r, own := uint16(1), float32(31)
		if k >= 10 {
			r, own = 2, 1
		}
		a.vrec = append(a.vrec, visitRec{svc: 1, route: r, own: own})
	}
	names := []string{"GET /a", "GET /b"}
	bucket := map[uint16]int{1: 0, 2: 1}
	rs := routeStatsOf(a, nil, 1, names, bucket, []string{"web", "catalog"})
	topo := &model.Topology{Name: "x", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "web", S: 1, CS2: 1, Servers: 1, Workers: 1, Routing: model.Pooled, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "catalog", S: 16, CS2: 1, Servers: 1, Workers: 1, Routing: model.Pooled},
	}}
	stats := []ServiceStats{{Name: "web"}, {Name: "catalog", Routes: rs}}
	sigs := map[routeSigKey]int64{{svc: 1, route: 1, sig: ""}: 10, {svc: 1, route: 2, sig: ""}: 5}
	routeNodes(topo, stats, map[int32]int{0: 0, 1: 1}, sigs, map[routeKey]int64{{svc: 1, route: 2}: 3},
		[][]string{nil, names}, []map[uint16]int{nil, bucket}, map[int]mixtureResult{})
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	q := topo.Services[0].Calls[0].RouteQ
	if len(q) != 2 || math.Abs(q[0]-0.5) > 1e-12 || math.Abs(q[1]-0.5) > 1e-12 {
		t.Fatalf("sole caller's route weights %v, want the shares [0.5 0.5]", q)
	}
	if pv := rs[1].Callers[0].PerVisit; pv != 8.0/20 {
		t.Errorf("web's /b visits per visit %v, want 8/20 (its visits in every trace)", pv)
	}
}
