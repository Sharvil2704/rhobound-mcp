// SPDX-License-Identifier: Apache-2.0

package infer

// Regression tests for the Lean lane Infer findings (maze-kit lean/Rbproofs/Lanes/Infer.lean, INFER-1..3), written by
// the workflow's verifier as reproductions against the real code and kept once fixed: end to end (OTLP JSON written to
// a capture directory, otlp.ReadDir, Run, then the model on the inferred topology) and, for INFER-1 and INFER-3, on the
// internal functions (routeStatsOf, routeNodes). Each fails on the code before the fix ("CONFIRMED") and passes after.

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

type wfSpan struct {
	svc, pod   string
	id, parent string
	kind       int
	start, end float64 // ms
	trace      int
	peer       string // a client span's server.address (port 80)
	name       string // a server span's name ("GET /a" is its route)
}

type wfIDs struct{ n int }

func (x *wfIDs) next() string { x.n++; return fmt.Sprintf("%016x", x.n+0x5000) }

// wfRun writes spans as a capture (one resourceSpans object per line), reads it and infers.
func wfRun(t *testing.T, spans []wfSpan) (*Inferred, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	const base = uint64(1_700_000_000_000_000_000)
	ns := func(ms float64) uint64 { return base + uint64(math.Round(ms*1e6)) }
	var b strings.Builder
	for _, sp := range spans {
		fmt.Fprintf(&b, `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}},`+
			`{"key":"k8s.pod.name","value":{"stringValue":%q}}]},"scopeSpans":[{"spans":[`, sp.svc, sp.pod)
		fmt.Fprintf(&b, `{"traceId":"%032x","spanId":%q,"parentSpanId":%q,"kind":%d,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"`,
			sp.trace, sp.id, sp.parent, sp.kind, ns(sp.start), ns(sp.end))
		if sp.name != "" {
			fmt.Fprintf(&b, `,"name":%q`, sp.name)
		}
		if sp.peer != "" {
			fmt.Fprintf(&b, `,"attributes":[{"key":"server.address","value":{"stringValue":%q}},{"key":"server.port","value":{"intValue":"80"}}]`, sp.peer)
		}
		b.WriteString("}]}]}]}\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := otlp.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Run(d, Options{})
}

func wfRoutesOf(inf *Inferred, svc string) []RouteStats {
	return inf.Stats[inf.Topology.Index(svc)].Routes
}

// ---------------------------------------------------------------------------------------------------------------
// INFER-1: web is catalog's only caller. catalog has two routes, /a (20 ms own time) and /b (1 ms), half the visits
// each. `broken` of the /b traces lose a span (an orphan INTERNAL span of web: lossBelowTop), at most 5% of traces
// so the capture is not refused. Shares count every visit (the FIFO jobs), RouteQ only the complete traces.

func wf1Capture(broken int) []wfSpan {
	x := &wfIDs{}
	var sp []wfSpan
	nb := 0
	for i := 0; i < 400; i++ {
		t0 := 100 * float64(i)
		route, own := "GET /a", 20.0
		if i%2 == 1 {
			route, own = "GET /b", 1.0
		}
		w, wc, cs := x.next(), x.next(), x.next()
		tr := i + 1
		sp = append(sp,
			wfSpan{svc: "web", pod: "web-0", id: w, kind: 2, start: t0, end: t0 + own + 3, trace: tr, name: "GET /"},
			wfSpan{svc: "web", pod: "web-0", id: wc, parent: w, kind: 3, start: t0 + 1, end: t0 + 1 + own + 0.4, trace: tr, peer: "catalog"},
			wfSpan{svc: "catalog", pod: "catalog-0", id: cs, parent: wc, kind: 2, start: t0 + 1.2, end: t0 + 1.2 + own, trace: tr, name: route})
		if i%2 == 1 && nb < broken {
			nb++
			sp = append(sp, wfSpan{svc: "web", pod: "web-0", id: x.next(), parent: "00000000deadbeef", kind: 1,
				start: t0 + 0.5, end: t0 + 0.6, trace: tr})
		}
	}
	return sp
}

// wfSoleCallerShift infers the capture and returns catalog's shares, web's RouteQ into catalog, the RouteAdj dm of
// that call and the move of web's modelled latency against the same topology with the call on the blend.
func wfSoleCallerShift(t *testing.T, broken int) (shares, q []float64, dm, dL float64, inf *Inferred) {
	t.Helper()
	inf, err := wfRun(t, wf1Capture(broken))
	if err != nil {
		t.Fatal(err)
	}
	topo := inf.Topology
	web, cat := topo.Index("web"), topo.Index("catalog")
	if web < 0 || cat < 0 {
		t.Fatalf("services %v", topo.Names())
	}
	if inf.Stats[cat].Entries != 0 {
		t.Fatalf("catalog has entries: %d", inf.Stats[cat].Entries)
	}
	callers := 0
	for i, s := range topo.Services {
		for _, c := range s.Calls {
			if c.Child == cat {
				callers++
				if i != web {
					t.Fatalf("catalog has another caller: %s", s.Name)
				}
			}
		}
	}
	if callers != 1 {
		t.Fatalf("web makes %d calls to catalog, want 1", callers)
	}
	for _, r := range topo.Services[cat].Routes {
		shares = append(shares, r.Share)
	}
	var call model.Call
	for _, c := range topo.Services[web].Calls {
		if c.Child == cat {
			call = c
		}
	}
	q = call.RouteQ
	res, err := model.Analytic(topo, inf.LamExtPerMs, nil)
	if err != nil {
		t.Fatal(err)
	}
	dm, _ = topo.RouteAdj(cat, q, res.L, nil, nil, nil)
	blend := *topo
	blend.Services = append([]model.Service(nil), topo.Services...)
	blend.Services[web].Calls = append([]model.Call(nil), topo.Services[web].Calls...)
	for j := range blend.Services[web].Calls {
		blend.Services[web].Calls[j].RouteQ = nil
	}
	if err := blend.Finish(); err != nil {
		t.Fatal(err)
	}
	rb, err := model.Analytic(&blend, inf.LamExtPerMs, nil)
	if err != nil {
		t.Fatal(err)
	}
	return shares, q, dm, res.L[web] - rb.L[web], inf
}

func TestWFInfer1SoleCallerMovedOffBlendByBrokenTraces(t *testing.T) {
	// control: no broken traces, the sole caller's weights are the shares and dm is 0
	shares0, q0, dm0, dL0, inf0 := wfSoleCallerShift(t, 0)
	t.Logf("control (0 broken of %d traces): shares %.4f, web RouteQ %.4f, dm %.6f ms, web L moved %.6f ms",
		inf0.Traces, shares0, q0, dm0, dL0)
	if math.Abs(dm0) > 1e-9 {
		t.Errorf("control: dm %v, want 0", dm0)
	}
	shares, q, dm, dL, inf := wfSoleCallerShift(t, 18)
	cat := inf.Topology.Index("catalog")
	rs := wfRoutesOf(inf, "catalog")
	for _, r := range rs {
		t.Logf("catalog route %s: visits %d share %.4f own %.4f ms callers %+v", r.Route, r.Visits, r.Share, r.OwnMs, r.Callers)
	}
	t.Logf("broken %d of %d traces: shares %.4f, web RouteQ %.4f, dm %.4f ms, web L moved %.4f ms (catalog blend S %.4f ms)",
		inf.BrokenTraces, inf.Traces, shares, q, dm, dL, inf.Topology.Services[cat].S)
	// web reaches every visit of catalog: the call should see the blend (dm = 0); any dm moves the prediction
	if math.Abs(dm) > 0.01 {
		t.Errorf("INFER-1 CONFIRMED: the sole caller web is moved off catalog's blend by dm = %+.4f ms (web's latency %+.4f ms) "+
			"because Share counts all %d visits and RouteQ only those of complete traces", dm, dL, rs[0].Visits+rs[1].Visits)
	}
}

// INFER-1 on the internal functions, as the go_repro: 20 visits, 10 per route, own 31 and 1 ms; web's complete
// traces reach /a 10 times and /b 5 times.
func TestWFInfer1InternalSoleCaller(t *testing.T) {
	a := &acc{routeEdge: map[routeEdgeKey]int64{{0, 1, 1}: 10, {0, 1, 2}: 5}, visits: []int64{15, 15}, sVisits: []int64{15, 15},
		entries: []int64{15, 0}}
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
	routeNodes(topo, stats, map[int32]int{0: 0, 1: 1}, map[routeSigKey]int64{}, nil, [][]string{nil, names},
		[]map[uint16]int{nil, bucket}, map[int]mixtureResult{})
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	q := topo.Services[0].Calls[0].RouteQ
	dm, _ := topo.RouteAdj(1, q, make([]float64, 2), nil, nil, nil)
	t.Logf("shares %.4f/%.4f own %.1f/%.1f, web RouteQ %.4f, dm %.4f ms", rs[0].Share, rs[1].Share, rs[0].OwnMs, rs[1].OwnMs, q, dm)
	if math.Abs(dm) > 1e-9 {
		t.Errorf("INFER-1 (internal) CONFIRMED: sole caller dm = %.4f ms, want 0", dm)
	}
}

// ---------------------------------------------------------------------------------------------------------------
// INFER-2: web's calls to catalog. async: on every visit a POST /events (10 ms) sent 1 ms after web's span ended,
// and on 4% of visits a GET /item (1 ms) web waits for; the edge is async (96% not waited) and the waited calls stay
// on the path as a sync call. mixed50: every visit waits for /item and sends /events after the response (50%, not
// async). catalog also takes 400 entry visits of /item, so its shares differ from web's mix.

func wf2Capture(async bool) []wfSpan {
	x := &wfIDs{}
	var sp []wfSpan
	tr := 0
	call := func(w string, t0, s, d float64, route string) {
		c := x.next()
		sp = append(sp,
			wfSpan{svc: "web", pod: "web-0", id: c, parent: w, kind: 3, start: t0 + s, end: t0 + s + d + 0.4, trace: tr, peer: "catalog"},
			wfSpan{svc: "catalog", pod: "catalog-0", id: x.next(), parent: c, kind: 2, start: t0 + s + 0.2, end: t0 + s + 0.2 + d, trace: tr, name: route})
	}
	for i := 0; i < 400; i++ {
		t0 := 100 * float64(i)
		tr++
		w := x.next()
		sp = append(sp, wfSpan{svc: "web", pod: "web-0", id: w, kind: 2, start: t0, end: t0 + 5, trace: tr, name: "GET /"})
		if !async || i%25 == 0 {
			call(w, t0, 1, 1, "GET /item") // waited for: within web's span
		}
		call(w, t0, 6, 10, "POST /events") // after web's span ended
		tr++
		sp = append(sp, wfSpan{svc: "catalog", pod: "catalog-0", id: x.next(), kind: 2, start: t0 + 50, end: t0 + 51, trace: tr, name: "GET /item"})
	}
	return sp
}

func TestWFInfer2AsyncVisitsInSyncRouteWeights(t *testing.T) {
	for _, async := range []bool{true, false} {
		what := map[bool]string{true: "async edge, 4% waited", false: "50% after the response"}[async]
		inf, err := wfRun(t, wf2Capture(async))
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		topo := inf.Topology
		web, cat := topo.Index("web"), topo.Index("catalog")
		var item, events int = -1, -1
		for k, r := range topo.Services[cat].Routes {
			t.Logf("%s: catalog route %s share %.4f own %.4f ms", what, r.Name, r.Share, r.S)
			switch r.Name {
			case "GET /item":
				item = k
			case "POST /events":
				events = k
			}
		}
		if item < 0 || events < 0 {
			t.Fatalf("%s: catalog routes %+v", what, topo.Services[cat].Routes)
		}
		res, err := model.Analytic(topo, inf.LamExtPerMs, nil)
		if err != nil {
			t.Fatal(err)
		}
		var syncCalls int
		for j, c := range topo.Services[web].Calls {
			if c.Child != cat {
				continue
			}
			dm, _ := topo.RouteAdj(cat, c.RouteQ, res.L, nil, nil, nil)
			t.Logf("%s: web call %d -> catalog: P %.4f async %v RouteQ %.4f dm %.4f ms", what, j, c.P, c.Async, c.RouteQ, dm)
			if c.Async {
				continue
			}
			syncCalls++
			if async && c.RouteQ != nil {
				// every visit of this sync call is a GET /item: RouteQ should be 1 on /item
				want := make([]float64, len(c.RouteQ))
				want[item] = 1
				dmWant, _ := topo.RouteAdj(cat, want, res.L, nil, nil, nil)
				t.Logf("%s: with RouteQ %v the call's dm would be %.4f ms; web's latency off by P*(dm-dmWant) = %+.4f ms (web L %.4f ms)",
					what, want, dmWant, c.P*(dm-dmWant), res.L[web])
				if c.RouteQ[item] < 0.9 {
					t.Errorf("INFER-2 CONFIRMED (%s): the sync call's RouteQ puts %.4f on POST /events, which only the "+
						"fire-and-forget calls reach (want 1 on GET /item)", what, c.RouteQ[events])
				}
			}
		}
		if !async {
			t.Logf("%s: %d sync calls to catalog (the finding expected one with P = 1)", what, syncCalls)
		}
	}
}

// ---------------------------------------------------------------------------------------------------------------
// INFER-3: one catalog pod with one worker; blocks of an /a pair (the second waits for the first) and a health probe
// answered off the worker (a cached GET; a real /health would be dropped as a probe). jitter: every probe ends 0.2 ms before the busy /a is released (within jitterMs, no
// contradiction). deep: probes take 0.02 ms off-load, three of the forty land during a busy /a (three
// contradictions, the jitterFloor allowance). oneRoute: the probes carry /a's route (the control: no routes).

func wf3Capture(deep, oneRoute bool) []wfSpan {
	x := &wfIDs{}
	var sp []wfSpan
	tr := 0
	add := func(name string, s, e float64) {
		tr++
		sp = append(sp, wfSpan{svc: "catalog", pod: "catalog-0", id: x.next(), kind: 2, start: s, end: e, trace: tr, name: name})
	}
	h := "GET /cached" // not "/health": the reader drops probe routes (otlp.IsProbe)
	if oneRoute {
		h = "GET /a"
	}
	for j := 0; j < 40; j++ {
		T := 30 * float64(j)
		s1 := 4.5 + 0.02*float64(j%7)
		s2 := 5 + 0.03*float64(j%5)
		add("GET /a", T, T+s1)
		add("GET /a", T+1, T+s1+s2)
		switch {
		case !deep:
			add(h, T+s1+s2-0.2, T+s1+s2-0.19)
		case j == 5 || j == 18 || j == 31:
			add(h, T+s1+2, T+s1+2.02)
		default:
			add(h, T+20, T+20.02)
		}
	}
	return sp
}

func TestWFInfer3NegativeRouteOwnTimeFailsFit(t *testing.T) {
	for _, deep := range []bool{false, true} {
		what := map[bool]string{false: "probes within jitter", true: "three probes during a busy request"}[deep]
		ctl, err := wfRun(t, wf3Capture(deep, true))
		if err != nil {
			t.Fatalf("%s, control (one route): %v", what, err)
		}
		cs := ctl.Stats[ctl.Topology.Index("catalog")]
		t.Logf("%s, control (one route): fit OK, catalog own %.4f ms, %d x %d workers", what, cs.OwnMeanMs, cs.Pods, cs.WorkersPerPod)
		_, err = wfRun(t, wf3Capture(deep, false))
		var ref *Refusal
		t.Logf("%s, two routes: err = %v (a Refusal: %v)", what, err, errors.As(err, &ref))
		if err != nil && strings.Contains(err.Error(), "own time and cs2 finite and non-negative") && !errors.As(err, &ref) {
			t.Errorf("INFER-3 CONFIRMED (%s): the service mean is %.4f ms >= 0 (no TraceNegativeServiceTime), yet the fit fails "+
				"with an uncoded error: %v", what, cs.OwnMeanMs, err)
		}
	}
}

// INFER-3 on the internal functions, as the go_repro.
func TestWFInfer3InternalNegativeRoute(t *testing.T) {
	a := &acc{}
	for _, r := range []uint16{1, 1, 1, 2, 2} {
		a.vrec = append(a.vrec, visitRec{svc: 0, route: r})
	}
	q := &svcQueue{jobs: 5, relMean: 2.6, pods: []*podQueue{{B: []float64{0, 0, 0, 0, 0}, D: []float64{5, 5, 5, -1, -1},
		Order: []int{0, 1, 2, 3, 4}, vidx: []int32{0, 1, 2, 3, 4}}}}
	names := []string{"GET /a", "GET /b"}
	bucket := map[uint16]int{1: 0, 2: 1}
	rs := routeStatsOf(a, q, 0, names, bucket, []string{"catalog"})
	topo := &model.Topology{Name: "x", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "catalog", S: q.relMean, CS2: 1, Servers: 1, Workers: 1, Routing: model.Pooled}}}
	routeNodes(topo, []ServiceStats{{Name: "catalog", Routes: rs}}, map[int32]int{0: 0}, map[routeSigKey]int64{}, nil,
		[][]string{names}, []map[uint16]int{bucket}, map[int]mixtureResult{})
	err := topo.Finish()
	t.Logf("routes %+v; service S %.2f; Finish: %v", topo.Services[0].Routes, q.relMean, err)
	if rs[1].OwnMs < 0 && err != nil {
		t.Errorf("INFER-3 (internal) CONFIRMED: route OwnMs %.2f passes routeStatsOf/routeNodes unclamped and Finish fails: %v", rs[1].OwnMs, err)
	}
}

// INFER-1's scaling itself (routeSigs not empty, so routeNodes sets RouteQ rather than leaving it nil): web is
// catalog's sole caller; route /a has 10 visits, all in complete traces; route /b has 10, of which 5 are in complete
// traces and the other 5 lost their caller (no routeKnown). The shares are 1/2, 1/2; web's counts are 10 and 5, and
// scaled by each route's completeness (10/10, 10/5) its weights are the shares. Several callers whose traces break at
// different rates: routes_broken_test.go.
func TestRouteWeightsScaledByCompleteness(t *testing.T) {
	a := &acc{routeEdge: map[routeEdgeKey]int64{{0, 1, 1}: 10, {0, 1, 2}: 5}, visits: []int64{20, 20}, sVisits: []int64{20, 20},
		entries: []int64{20, 0}}
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
	routeNodes(topo, stats, map[int32]int{0: 0, 1: 1}, sigs, nil, [][]string{nil, names}, []map[uint16]int{nil, bucket},
		map[int]mixtureResult{})
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	q := topo.Services[0].Calls[0].RouteQ
	if len(q) != 2 || math.Abs(q[0]-0.5) > 1e-12 || math.Abs(q[1]-0.5) > 1e-12 {
		t.Fatalf("sole caller's route weights %v, want the shares [0.5 0.5]", q)
	}
	if dm, _ := topo.RouteAdj(1, q, make([]float64, 2), nil, nil, nil); math.Abs(dm) > 1e-9 {
		t.Errorf("sole caller moved off the blend by %v ms", dm)
	}
}
