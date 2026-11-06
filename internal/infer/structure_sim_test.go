// SPDX-License-Identifier: Apache-2.0

package infer_test

// Trace structure as the reference reads it: broken traces, probes, staged
// calls, conditional calls, and the shapes the model has no form for.

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

func inferErr(t *testing.T, spans []span) (*infer.Inferred, error) {
	t.Helper()
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	return infer.Run(d, infer.Options{})
}

func hasWarning(inf *infer.Inferred, part string) bool {
	for _, w := range inf.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

// dropSpans removes the server spans of svc in every nth trace (and, with alsoChildren, the client spans
// they made: their callees' server spans then have a client parent whose own parent is lost).
func dropSpans(spans []span, svc string, every int, alsoChildren bool) ([]span, int) {
	gone := map[string]bool{}
	traces := map[int]bool{}
	for _, sp := range spans {
		if sp.kind == 2 && sp.svc == svc && sp.trace%every == 0 {
			gone[sp.id] = true
			traces[sp.trace] = true
		}
	}
	var out []span
	for _, sp := range spans {
		if gone[sp.id] || (alsoChildren && gone[sp.parent]) {
			continue
		}
		out = append(out, sp)
	}
	return out, len(traces)
}

// TestBrokenTraces: a trace that lost a span is kept out of the entries and the call structure, and still
// counts in the queues and the rates. At 094b99a-6171ceb a lost a's span made b an entry (b's client parent
// had no owner), and a lost c's span turned b's call into an external delay "ext:c".
func TestBrokenTraces(t *testing.T) {
	const lam = 50.0
	out := run(chain(), lam, 200, 7, false)
	full := infer1(t, out.spans)
	for _, c := range []struct {
		what, svc string
		kids      bool
	}{{"a's server span lost", "a", false}, {"a's server and client spans lost", "a", true}, {"c's server span lost", "c", false}} {
		spans, lost := dropSpans(out.spans, c.svc, 50, c.kids) // 2% of the traces
		inf, err := inferErr(t, spans)
		if err != nil {
			t.Fatalf("%s: %v", c.what, err)
		}
		topo := inf.Topology
		if len(topo.Entry) != 1 || topo.Entry[topo.Index("a")] != 1 {
			t.Errorf("%s: entries %v, want a only", c.what, topo.Entry)
		}
		if topo.Index("ext:c") >= 0 || topo.Index("ext:b") >= 0 {
			t.Errorf("%s: a lost callee became an external delay: %v", c.what, topo.Names())
		}
		if inf.BrokenTraces != int64(lost) || inf.Requests != full.Requests-int64(lost) {
			t.Errorf("%s: broken %d (want %d), entry requests %d (complete traces %d)", c.what, inf.BrokenTraces, lost,
				inf.Requests, full.Requests-int64(lost))
		}
		if !hasWarning(inf, "traces miss a span") {
			t.Errorf("%s: no warning: %v", c.what, inf.Warnings)
		}
		within(t, c.what+": external rate", inf.LamExtPerMs, full.LamExtPerMs, 0.005)
		// the queues keep every visit: b's visits and service time are those of the full capture
		bi, fb := topo.Index("b"), full.Topology.Index("b")
		if inf.Stats[bi].Visits != full.Stats[fb].Visits {
			t.Errorf("%s: b visits %d, want all %d", c.what, inf.Stats[bi].Visits, full.Stats[fb].Visits)
		}
		within(t, c.what+": b service time", topo.Services[bi].S, full.Topology.Services[fb].S, 1e-9)
		v := topo.VisitRatios()
		within(t, c.what+": calls to c per request", v[topo.Index("c")], 1, 1e-12)
	}
	// above 5% the capture is refused, with the reason
	spans, _ := dropSpans(out.spans, "c", 12, false)
	if _, err := inferErr(t, spans); err == nil || !strings.Contains(err.Error(), "traces miss spans") ||
		!strings.Contains(err.Error(), "needs every request") {
		t.Errorf("8%% broken traces: want a refusal that says why, got %v", err)
	}
}

// TestLostCalleeByPort: a call is a lost callee only if its address, port included, is one a service answers
// on. Here b answers on localhost:8081 and a also calls a database on localhost:5432 that emits no spans: the
// database is an external delay, not a lost span (keyed by host alone every trace would read broken).
func TestLostCalleeByPort(t *testing.T) {
	b := &node{name: "b", c: 1, pods: 1, mean: 10}
	db := &node{name: "db", c: 4, pods: 1, mean: 2}
	out := run(&node{name: "a", c: 2, pods: 1, mean: 5, children: []*node{b, db}}, 40, 100, 7, false)
	var spans []span
	for _, sp := range out.spans {
		switch {
		case sp.kind == 2 && sp.svc == "db":
			continue // the database emits no spans
		case sp.kind == 3 && sp.peer == "b":
			sp.peer, sp.port = "localhost", 8081
		case sp.kind == 3 && sp.peer == "db":
			sp.peer, sp.port = "localhost", 5432
		}
		spans = append(spans, sp)
	}
	inf := infer1(t, spans)
	if inf.BrokenTraces != 0 || inf.Topology.Index("ext:localhost") < 0 || inf.Topology.Index("b") < 0 {
		t.Errorf("broken traces %d (want 0), services %v (want b and ext:localhost)", inf.BrokenTraces, inf.Topology.Names())
	}
	// the same calls to b's own address, with no span of b under them, lost their callee's span
	for i := range spans {
		if spans[i].port == 5432 {
			spans[i].port = 8081
		}
	}
	if _, err := inferErr(t, spans); err == nil || !strings.Contains(err.Error(), "traces miss spans") {
		t.Errorf("calls to b's address with no span of b: want the broken-trace refusal, got %v", err)
	}
}

// TestProbesInCapture: kubelet probes on b's pod (GET /healthz, each with a child span, as zipkin-go records
// them) are dropped: b gains no entry weight and no visits, and the probes' children do not break traces.
func TestProbesInCapture(t *testing.T) {
	out := run(chain(), 50, 200, 7, false)
	full := infer1(t, out.spans)
	spans := append([]span(nil), out.spans...)
	n := 0
	for ms := 0.0; ms < 200_000; ms += 1000 {
		n++
		id, kid := fmt.Sprintf("%016x", 1<<40+n), fmt.Sprintf("%016x", 1<<41+n)
		spans = append(spans, span{svc: "b", pod: "b-0", id: id, kind: 2, start: ms, end: ms + 0.05, trace: 1<<30 + n, route: "/healthz"},
			span{svc: "b", pod: "b-0", id: kid, parent: id, kind: 1, start: ms + 0.01, end: ms + 0.04, trace: 1<<30 + n})
	}
	inf := infer1(t, spans)
	if inf.ProbeSpans != n || inf.BrokenTraces != 0 {
		t.Errorf("probe spans %d (want %d), broken traces %d (want 0)", inf.ProbeSpans, n, inf.BrokenTraces)
	}
	bi := inf.Topology.Index("b")
	if _, ok := inf.Topology.Entry[bi]; ok || inf.Stats[bi].Visits != full.Stats[full.Topology.Index("b")].Visits {
		t.Errorf("b: entry weight %v, visits %d (want none and %d)", inf.Topology.Entry, inf.Stats[bi].Visits,
			full.Stats[full.Topology.Index("b")].Visits)
	}
}

// staged is a -> {b, c at once}, then d (after both have answered); pD < 1 makes the d stage conditional.
func staged(pD float64) *node {
	b := &node{name: "b", c: 1, pods: 1, mean: 10}
	c := &node{name: "c", c: 1, pods: 1, mean: 10}
	d := &node{name: "d", c: 1, pods: 1, mean: 10}
	a := &node{name: "a", c: 4, pods: 1, mean: 3, children: []*node{b, c, d}, stage: []int{0, 0, 1}}
	if pD < 1 {
		a.p = []float64{1, 1, pD}
	}
	return a
}

// TestStagedCalls: a calls b and c together, then d once both have answered. The reference reads this as two
// stages; until it was ported here, every overlapping visit made the service a parallel fan-out of b, c and d,
// whose expected maximum drops most of d's time (about -27% of the call part for three equal exponentials).
func TestStagedCalls(t *testing.T) {
	for _, pD := range []float64{1, 0.6} {
		const lam = 30.0
		out := run(staged(pD), lam, 400, 9, false)
		inf := infer1(t, out.spans)
		topo := inf.Topology
		a := topo.Index("a")
		st := topo.Index("stage:a:1")
		if st < 0 || topo.Services[a].Parallel || len(topo.Services[a].Calls) != 2 {
			t.Fatalf("p(d)=%v: a should call a stage (b + c) and then d: %v %+v", pD, topo.Names(), topo.Services[a])
		}
		sg := topo.Services[st]
		if !sg.Parallel || !sg.Delay || sg.S != 0 || len(sg.Calls) != 2 || !inf.Stats[st].Stage {
			t.Errorf("p(d)=%v: the stage should be a zero-time parallel delay calling b and c: %+v", pD, sg)
		}
		if c0, c1 := topo.Services[a].Calls[0], topo.Services[a].Calls[1]; c0.Child != st || c0.P != 1 ||
			c1.Child != topo.Index("d") || math.Abs(c1.P-pD) > 0.03 {
			t.Errorf("p(d)=%v: a's calls %+v", pD, topo.Services[a].Calls)
		}
		if !hasWarning(inf, "calls in 2 stages") {
			t.Errorf("p(d)=%v: the stages are not reported: %v", pD, inf.Warnings)
		}
		base, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
		if err != nil || !base.Stable {
			t.Fatalf("model: %v", err)
		}
		within(t, fmt.Sprintf("p(d)=%v: entry latency, staged model vs simulation", pD), base.L[a], mean(out.totalMs), 0.05)
		if pD == 1 {
			// the same capture read as one parallel fan-out of b, c and d
			flat := *topo
			flat.Services = append([]model.Service(nil), topo.Services...)
			sa := flat.Services[a]
			sa.Parallel, sa.Calls = true, []model.Call{{Child: topo.Index("b"), P: 1}, {Child: topo.Index("c"), P: 1}, {Child: topo.Index("d"), P: 1}}
			flat.Services[a] = sa
			if err := flat.Finish(); err != nil {
				t.Fatal(err)
			}
			fl, _ := model.AnalyticWith(&flat, inf.LamExtPerMs, nil, model.Default)
			if e := (fl.L[a] - mean(out.totalMs)) / mean(out.totalMs); e > -0.15 {
				t.Errorf("all-parallel reading should be far too short; it is %+.1f%%", 100*e)
			}
		}
	}
}

// TestConditionalCalls: a calls d once on every visit and a second time on 30%, and e on 1.5% of visits.
// Both are kept as exact probabilities (until the rules were ported a call on fewer than 2% of visits was
// dropped and one on more than 98% made always). The simulator gives each call of a node its own queue,
// so d's second call does not queue behind its first: the load is kept low enough for that not to matter.
func TestConditionalCalls(t *testing.T) {
	d := &node{name: "d", c: 2, pods: 1, mean: 8}
	e := &node{name: "e", c: 1, pods: 1, mean: 20}
	a := &node{name: "a", c: 4, pods: 1, mean: 3, children: []*node{d, d, e}, p: []float64{1, 0.3, 0.015}}
	out := run(a, 40, 400, 13, false)
	inf := infer1(t, out.spans)
	topo := inf.Topology
	v := topo.VisitRatios()
	within(t, "visits to d per request", v[topo.Index("d")], 1.3, 0.03)
	within(t, "visits to e per request", v[topo.Index("e")], 0.015, 0.15)
	var ps []float64
	for _, c := range topo.Services[topo.Index("a")].Calls {
		ps = append(ps, c.P)
	}
	if len(ps) != 3 || ps[0] != 1 || math.Abs(ps[1]-0.3) > 0.02 || math.Abs(ps[2]-0.015) > 0.004 {
		t.Errorf("a's call probabilities %v, want 1, ~0.3, ~0.015", ps)
	}
	base, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
	if err != nil || !base.Stable {
		t.Fatalf("model: %v", err)
	}
	within(t, "entry latency with conditional calls", base.L[topo.Index("a")], mean(out.totalMs), 0.05)
}

// TestShapesFormerlyRefused: shapes that the model had no form for until calls per visit that vary and visits that take
// several call shapes were ported (counts.go): a conditional parallel branch (c on 60% of visits), a call count spread
// over 1, 2 and 3 calls, and calls that overlap on half the visits. They are read as the mean count and as a mixture of
// shapes, and the entry's latency is the simulation's within 6%; what is still refused is in counts_test.go
// (TestTooManyShapesAreStillRefused).
func TestShapesFormerlyRefused(t *testing.T) {
	leaf := func(n string) *node { return &node{name: n, c: 2, pods: 1, mean: 5} }
	// a conditional parallel branch (c on 60% of visits): it used to be called on every visit
	par := &node{name: "a", c: 4, pods: 1, mean: 3, parallel: true, children: []*node{leaf("b"), leaf("c")}, p: []float64{1, 0.6}}
	// 1, 2 or 3 calls to d: neither a fixed count nor one conditional call
	cnt := &node{name: "a", c: 4, pods: 1, mean: 3, children: []*node{leaf("d"), leaf("d"), leaf("d")}, p: []float64{1, 0.5, 0.5}}
	for _, c := range []struct {
		what string
		root *node
		want string
	}{{"conditional parallel branch", par, "modelled as a mixture"}, {"call count spread", cnt, "only their mean"}} {
		out := run(c.root, 30, 100, 3, false)
		inf, err := inferErr(t, out.spans)
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		if !hasWarning(inf, c.want) {
			t.Errorf("%s: no warning with %q: %v", c.what, c.want, inf.Warnings)
		}
		base, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
		if err != nil || !base.Stable {
			t.Fatalf("%s: model: %v", c.what, err)
		}
		within(t, c.what+": entry latency", base.L[inf.Topology.Index("a")], mean(out.totalMs), 0.06)
		parallelNodesCertain(t, inf.Topology)
	}
}

// TestCS2Snap: as in the reference, a measured cs2 within 3 standard errors (sqrt(8/n)) of 1 is taken as 1;
// a clearly regular or variable service keeps its own.
func TestCS2Snap(t *testing.T) {
	for _, c := range []struct {
		dist string
		cs2  float64
		snap bool
	}{{"exp", 1, true}, {"gamma", 0.5, false}, {"h2", 2, false}} {
		b := &node{name: "b", c: 1, pods: 1, mean: 10, dist: c.dist, cs2: c.cs2}
		a := &node{name: "a", c: 2, pods: 1, mean: 5, dist: "gamma", cs2: 0.25, children: []*node{b}}
		inf := infer1(t, run(a, 50, 200, 17, false).spans)
		bi := inf.Topology.Index("b")
		st, sv := inf.Stats[bi], inf.Topology.Services[bi]
		if st.CS2Snapped != c.snap || (c.snap && sv.CS2 != 1) || (!c.snap && sv.CS2 != st.OwnCS2Measured) {
			t.Errorf("%s cs2 %v: measured %.4f, model %.4f, snapped %v (want %v)", c.dist, c.cs2, st.OwnCS2Measured, sv.CS2,
				st.CS2Snapped, c.snap)
		}
		if math.Abs(st.OwnCS2Measured-c.cs2) > 0.15*c.cs2 {
			t.Errorf("%s: measured cs2 %.4f, true %v", c.dist, st.OwnCS2Measured, c.cs2)
		}
		if ai := inf.Topology.Index("a"); inf.Stats[ai].CS2Snapped {
			t.Errorf("a (gamma, cs2 0.25) was snapped to 1")
		}
	}
}
