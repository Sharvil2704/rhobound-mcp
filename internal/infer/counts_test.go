// SPDX-License-Identifier: Apache-2.0

package infer_test

// Calls per visit that vary and visits that take several call shapes, on hand-made spans: every number in them is
// known, so the inference's reading of them is checked exactly.

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// handCall is a call of A: its CLIENT span starts at (ms after the visit's start) and lasts dur; the callee's SERVER
// span is inside it, 0.1 ms in from each end.
type handCall struct {
	callee   string
	at, dur  float64
	afterEnd bool // the call is sent after A's span has ended: at is then counted from A's end
}

// handTraces makes n traces of A (one visit each, gap ms apart: no queueing) with the calls plan(i) gives; aDur(i) is
// A's span in ms.
func handTraces(n int, gap float64, aDur func(i int) float64, plan func(i int) []handCall) []span {
	var spans []span
	id := 0
	nid := func() string { id++; return fmt.Sprintf("%016x", id) }
	for i := 0; i < n; i++ {
		t0 := 10 + float64(i)*gap
		root := nid()
		d := aDur(i)
		spans = append(spans, span{svc: "A", pod: "a-0", id: root, kind: 2, start: t0, end: t0 + d, trace: i + 1})
		for _, c := range plan(i) {
			start := t0 + c.at
			if c.afterEnd {
				start = t0 + d + c.at
			}
			cid := nid()
			spans = append(spans,
				span{svc: "A", pod: "a-0", id: cid, parent: root, kind: 3, start: start, end: start + c.dur, trace: i + 1, peer: c.callee},
				span{svc: c.callee, pod: c.callee + "-0", id: nid(), parent: cid, kind: 2, start: start + 0.1, end: start + c.dur - 0.1, trace: i + 1})
		}
	}
	return spans
}

// seqCalls are n calls of dur ms to callee one after another from `at`, 0.5 ms apart.
func seqCalls(callee string, n int, at, dur float64) []handCall {
	var out []handCall
	for k := 0; k < n; k++ {
		out = append(out, handCall{callee: callee, at: at + float64(k)*(dur+0.5), dur: dur})
	}
	return out
}

// fanCalls are n calls of dur ms to callee, all starting at `at`.
func fanCalls(callee string, n int, at, dur float64) []handCall {
	var out []handCall
	for k := 0; k < n; k++ {
		out = append(out, handCall{callee: callee, at: at, dur: dur})
	}
	return out
}

func callsOf(topo *model.Topology, name string) []model.Call {
	return topo.Services[topo.Index(name)].Calls
}

// perVisit is the calls per visit of caller to callee that the model makes: the visit ratios' quotient.
func perVisit(topo *model.Topology, caller, callee string) float64 {
	v := topo.VisitRatios()
	return v[topo.Index(callee)] / v[topo.Index(caller)]
}

// parallelNodesCertain: no parallel node has a call with a probability below 1, which the model refuses.
func parallelNodesCertain(t *testing.T, topo *model.Topology) {
	t.Helper()
	for _, s := range topo.Services {
		if sc := s.SyncCalls(); s.Parallel && len(sc) > 1 {
			for _, c := range sc {
				if c.P != 1 {
					t.Errorf("%s is a parallel node with a call of probability %v", s.Name, c.P)
				}
			}
		}
	}
}

// TestVariableCountsEnterAsTheirMean: A calls B one, two or three times one after another, equally often. The mean
// (2 per visit) enters as two calls made every visit, B gets exactly twice A's visits, the report says how the counts
// were taken, and A's latency is the visit's.
func TestVariableCountsEnterAsTheirMean(t *testing.T) {
	spans := handTraces(600, 200, func(i int) float64 { return 2 + float64(1+i%3)*4.5 }, func(i int) []handCall {
		return seqCalls("B", 1+i%3, 1, 4)
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	cs := callsOf(topo, "A")
	if len(cs) != 2 || cs[0].P != 1 || cs[1].P != 1 || cs[0].Child != topo.Index("B") || cs[1].Child != cs[0].Child {
		t.Errorf("A's calls %+v, want two calls to B, both certain", cs)
	}
	if topo.Services[topo.Index("A")].Parallel {
		t.Errorf("A is parallel")
	}
	vc := inf.Stats[topo.Index("A")].VariableCalls
	want := []infer.CallsVisits{{Calls: 1, Visits: 200}, {Calls: 2, Visits: 200}, {Calls: 3, Visits: 200}}
	if len(vc) != 1 || vc[0].Callee != "B" || vc[0].MeanPerVisit != 2 || vc[0].ModelledAs != infer.ModelledMean || fmt.Sprint(vc[0].VisitsByCalls) != fmt.Sprint(want) {
		t.Errorf("variable calls %+v, want B, mean 2, %v, %q", vc, want, infer.ModelledMean)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-2) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 2", got)
	}
	if !hasWarning(inf, "A -> B: calls per visit vary as {1: 200, 2: 200, 3: 200}; only their mean (2 per visit) enters the model, as 2 call(s) "+
		"made on every visit.") || !hasWarning(inf, "lost from the capture, or sampled out") {
		t.Errorf("no warning with the counts and what the model does: %v", inf.Warnings)
	}
	res, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
	if err != nil {
		t.Fatal(err)
	}
	var obs float64
	for i := 0; i < 600; i++ {
		obs += 2 + float64(1+i%3)*4.5
	}
	within(t, "A's latency", res.L[topo.Index("A")], obs/600, 0.02)
}

// TestManyCallsPerVisitTerminate: A calls B 1 to 40 times in a row per visit (a query per row). The stage template's search
// for the stages a visit ran was exponential in the calls of a visit, and taking varying counts made it reachable: at 36
// calls it did not finish in two minutes. The mean enters, as for any varying count, in well under a second.
func TestManyCallsPerVisitTerminate(t *testing.T) {
	spans := handTraces(400, 300, func(i int) float64 { return 2 + float64(1+i%40)*4.5 }, func(i int) []handCall {
		return seqCalls("B", 1+i%40, 1, 4)
	})
	type result struct {
		inf *infer.Inferred
		err error
	}
	done := make(chan result, 1)
	go func() {
		inf, err := inferErr(t, spans)
		done <- result{inf, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("refused: %v", r.err)
		}
		vc := r.inf.Stats[r.inf.Topology.Index("A")].VariableCalls
		if len(vc) != 1 || vc[0].ModelledAs != infer.ModelledMean || math.Abs(vc[0].MeanPerVisit-20.5) > 0.1 {
			t.Errorf("variable calls %+v, want B at a mean of about 20.5 per visit, taken as a mean count", vc)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no answer in 30 s for a service that calls one callee 1 to 40 times per visit")
	}
}

// TestFractionalMeanCount: counts 1, 2, 2, 3, 3 per five visits have the mean 2.2: two calls and one with probability 0.2.
func TestFractionalMeanCount(t *testing.T) {
	counts := []int{1, 2, 2, 3, 3}
	spans := handTraces(500, 200, func(i int) float64 { return 2 + float64(counts[i%5])*4.5 }, func(i int) []handCall {
		return seqCalls("B", counts[i%5], 1, 4)
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	cs := callsOf(topo, "A")
	if len(cs) != 3 || cs[0].P != 1 || cs[1].P != 1 || math.Abs(cs[2].P-0.2) > 1e-12 {
		t.Errorf("A's calls %+v, want two certain calls to B and one with probability 0.2", cs)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-2.2) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 2.2", got)
	}
}

// TestVariableWidthFanOutIsAMixture: A calls B at once, two, three or four times. As one parallel fan-out the expected
// maximum would need the number of branches; as a mixture each width is a shape that A calls with its share of the visits.
func TestVariableWidthFanOutIsAMixture(t *testing.T) {
	spans := handTraces(600, 200, func(i int) float64 { return 2 + 4.2 }, func(i int) []handCall {
		return fanCalls("B", 2+i%3, 1, 4)
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	a := topo.Services[topo.Index("A")]
	if a.Parallel {
		t.Errorf("A is one parallel fan-out")
	}
	st := inf.Stats[topo.Index("A")]
	if len(st.CallShapes) != 3 {
		t.Fatalf("call shapes %+v, want three widths", st.CallShapes)
	}
	for _, s := range st.CallShapes {
		if math.Abs(s.Share-1.0/3) > 1e-12 {
			t.Errorf("shape %q has share %v, want a third", s.Shape, s.Share)
		}
	}
	got := []string{st.CallShapes[0].Shape, st.CallShapes[1].Shape, st.CallShapes[2].Shape}
	if fmt.Sprint(got) != "[B x2 B x3 B x4]" {
		t.Errorf("shapes %v, want B x2, B x3, B x4", got)
	}
	if len(st.VariableCalls) != 1 || st.VariableCalls[0].ModelledAs != infer.ModelledMixture || st.VariableCalls[0].MeanPerVisit != 3 {
		t.Errorf("variable calls %+v", st.VariableCalls)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-3) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 3", got)
	}
	parallelNodesCertain(t, topo)
	for _, name := range []string{"stage:A:1.1", "stage:A:2.1", "stage:A:3.1"} {
		if i := topo.Index(name); i < 0 || !topo.Services[i].Parallel || !topo.Services[i].Delay || !inf.Stats[i].Stage {
			t.Errorf("%s is not a zero-time parallel node", name)
		}
	}
	if !hasWarning(inf, "A: its visits take 3 call shapes, modelled as a mixture") {
		t.Errorf("the mixture is not reported: %v", inf.Warnings)
	}
	if hasWarning(inf, "times at once") {
		t.Errorf("a batch warning for a callee that is idle: %v", inf.Warnings)
	}
}

// TestStagesCarryAVaryingCount: A calls B and C at once and then D, once on 96% of the visits and three times in a row
// on 4%. The stage template of the reference carries the visits (D, then D with probability 0.04, then D with probability
// 0.04), so D's count is the stages' and the report says so: it used to be refused for its call count.
func TestStagesCarryAVaryingCount(t *testing.T) {
	spans := handTraces(1000, 200, func(i int) float64 {
		if i%25 == 0 {
			return 2 + 5 + 3*4.5
		}
		return 2 + 5 + 4.5
	}, func(i int) []handCall {
		cs := fanCalls("B", 1, 1, 5)
		cs = append(cs, fanCalls("C", 1, 1, 5)...)
		n := 1
		if i%25 == 0 {
			n = 3
		}
		return append(cs, seqCalls("D", n, 6.5, 4)...)
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	vc := inf.Stats[topo.Index("A")].VariableCalls
	if len(vc) != 1 || vc[0].Callee != "D" || vc[0].ModelledAs != infer.ModelledStages || math.Abs(vc[0].MeanPerVisit-1.08) > 1e-12 {
		t.Errorf("variable calls %+v, want D, 1.08 per visit, carried by the stages", vc)
	}
	if got := perVisit(topo, "A", "D"); math.Abs(got-1.08) > 1e-12 {
		t.Errorf("visits to D per visit of A: %v, want 1.08", got)
	}
	if !hasWarning(inf, "calls in 4 stages") || !hasWarning(inf, "A -> D: calls per visit vary as {1: 960, 3: 40}; the stages below carry them") {
		t.Errorf("the stages are not reported: %v", inf.Warnings)
	}
	parallelNodesCertain(t, topo)
}

// TestVaryingCountWithAnAsyncEdge: A calls B one to three times one after another and sends a message to L after its
// response on every visit. L's load is A's, the counts of B enter as their mean, and the async call stays async.
func TestVaryingCountWithAnAsyncEdge(t *testing.T) {
	spans := handTraces(600, 200, func(i int) float64 { return 2 + float64(1+i%3)*4.5 }, func(i int) []handCall {
		cs := seqCalls("B", 1+i%3, 1, 4)
		return append(cs, handCall{callee: "L", at: 3, dur: 2, afterEnd: true})
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	if got := perVisit(topo, "A", "B"); math.Abs(got-2) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 2", got)
	}
	if got := perVisit(topo, "A", "L"); math.Abs(got-1) > 1e-12 {
		t.Errorf("visits to L per visit of A: %v, want 1", got)
	}
	nAsync := 0
	for _, c := range callsOf(topo, "A") {
		if c.Async {
			nAsync++
			if c.Child != topo.Index("L") {
				t.Errorf("async call to %s", topo.Services[c.Child].Name)
			}
		} else if c.Child != topo.Index("B") {
			t.Errorf("a call to %s", topo.Services[c.Child].Name)
		}
	}
	if nAsync != 1 {
		t.Errorf("%d async calls, want 1", nAsync)
	}
}

// TestMixtureTopsUpCallsItsShapesLeaveOut: A calls B and C at once on half its visits and B, C and B again one after
// another on the other half (no stage template carries both), and on 30% of the visits also calls D: on a third of those within its span, on the others after its span
// has ended (too few of them waited for for the edge to be async, so it is on the path and the calls count). The shapes
// read only the calls within a visit's span, so D's other calls are added as mean-count calls: D's load is the observed one.
func TestMixtureTopsUpCallsItsShapesLeaveOut(t *testing.T) {
	spans := handTraces(600, 200, func(i int) float64 { return 18 }, func(i int) []handCall {
		var cs []handCall
		if i%2 == 0 {
			cs = []handCall{{callee: "B", at: 1, dur: 4}, {callee: "C", at: 2, dur: 4}}
		} else {
			cs = []handCall{{callee: "B", at: 1, dur: 4}, {callee: "C", at: 6, dur: 4}, {callee: "B", at: 11, dur: 4}}
		}
		switch i % 10 {
		case 0:
			cs = append(cs, handCall{callee: "D", at: 15, dur: 2})
		case 1, 2:
			cs = append(cs, handCall{callee: "D", at: 3, dur: 2, afterEnd: true})
		}
		return cs
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	if got := perVisit(topo, "A", "D"); math.Abs(got-0.3) > 1e-12 {
		t.Errorf("visits to D per visit of A: %v, want 0.3", got)
	}
	if !hasWarning(inf, "A -> D: the call shapes hold 0.1 of the 0.3 calls per visit that the visits made") {
		t.Errorf("no warning for the calls the shapes leave out: %v", inf.Warnings)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-1.5) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 1.5", got)
	}
	if got := perVisit(topo, "A", "C"); math.Abs(got-1) > 1e-12 {
		t.Errorf("visits to C per visit of A: %v, want 1", got)
	}
	parallelNodesCertain(t, topo)
}

// widthShapes makes traces of A calling B at once, w times, with w drawn by the shares: a share of 1/50 per visit of 50.
func widthShapes(n int, widths []int) []span {
	return handTraces(n, 200, func(i int) float64 { return 2 + 4.2 }, func(i int) []handCall {
		return fanCalls("B", widths[i%len(widths)], 1, 4)
	})
}

// TestTooManyShapesAreStillRefused: a mixture keeps the 12 commonest shapes, and the rarer ones may hold 5% of the
// visits at most. A fan-out of 14 widths, each on 1/14 of the visits, leaves 14% out: refused, by name. With 12 widths on
// 8% of the visits each and two on 2%, the rare ones (4%) enter as mean counts.
func TestTooManyShapesAreStillRefused(t *testing.T) {
	var even []int
	for w := 1; w <= 14; w++ {
		even = append(even, w)
	}
	_, err := inferErr(t, widthShapes(700, even))
	if err == nil || !strings.Contains(err.Error(), "A: its calls overlap in 100% of its visits with two or more calls, and its visits take more "+
		"than 12 call shapes, so that the commonest 12 leave 14% of its visits that make calls out (the limit is 5%)") {
		t.Errorf("14 even widths: want a refusal naming the shapes, got %v", err)
	}
	var skew []int
	for w := 1; w <= 12; w++ {
		for k := 0; k < 4; k++ {
			skew = append(skew, w)
		}
	}
	skew = append(skew, 13, 14) // 50 visits: 4 of each of 12 widths (8% each), one of each of 13 and 14 (2% each)
	inf, err := inferErr(t, widthShapes(700, skew))
	if err != nil {
		t.Fatalf("12 common widths and 2 rare: %v", err)
	}
	topo := inf.Topology
	if st := inf.Stats[topo.Index("A")]; len(st.CallShapes) != 12 {
		t.Errorf("%d call shapes, want 12", len(st.CallShapes))
	}
	if !hasWarning(inf, "A: 4.00% of its visits that make calls take rarer shapes; their calls enter as mean counts, one after another") {
		t.Errorf("no warning for the rare shapes: %v", inf.Warnings)
	}
	want := 0.0
	for _, w := range skew {
		want += float64(w)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-want/float64(len(skew))) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want %v", got, want/float64(len(skew)))
	}
	parallelNodesCertain(t, topo)
}

// TestConditionalParallelBranchIsAMixture: B on every visit and C on 60%, both at once. The expected maximum of a fan-out
// needs every branch on every visit, so A is a mixture of two shapes (B and C at once, B alone).
func TestConditionalParallelBranchIsAMixture(t *testing.T) {
	spans := handTraces(500, 200, func(i int) float64 { return 2 + 4.2 }, func(i int) []handCall {
		cs := fanCalls("B", 1, 1, 4)
		if i%5 < 3 {
			cs = append(cs, fanCalls("C", 1, 1, 4)...)
		}
		return cs
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	topo := inf.Topology
	st := inf.Stats[topo.Index("A")]
	if len(st.CallShapes) != 2 || st.CallShapes[0].Shape != "B + C" || math.Abs(st.CallShapes[0].Share-0.6) > 1e-12 ||
		st.CallShapes[1].Shape != "B" || math.Abs(st.CallShapes[1].Share-0.4) > 1e-12 {
		t.Errorf("call shapes %+v, want B + C (60%%) and B (40%%)", st.CallShapes)
	}
	if got := perVisit(topo, "A", "B"); math.Abs(got-1) > 1e-12 {
		t.Errorf("visits to B per visit of A: %v, want 1", got)
	}
	if got := perVisit(topo, "A", "C"); math.Abs(got-0.6) > 1e-12 {
		t.Errorf("visits to C per visit of A: %v, want 0.6", got)
	}
	parallelNodesCertain(t, topo)
}

// TestHalfOverlappingCallsAreAMixture: half the visits call B and C together, the other half one after the other: two
// shapes, each with its share. They were refused as "neither all at once, one after another nor in stages".
func TestHalfOverlappingCallsAreAMixture(t *testing.T) {
	spans := handTraces(400, 50, func(i int) float64 { return 12 }, func(i int) []handCall {
		if i%2 == 0 {
			return []handCall{{callee: "B", at: 1, dur: 4}, {callee: "C", at: 2, dur: 4}}
		}
		return []handCall{{callee: "B", at: 1, dur: 4}, {callee: "C", at: 6, dur: 4}}
	})
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	st := inf.Stats[inf.Topology.Index("A")]
	if len(st.CallShapes) != 2 || st.CallShapes[0].Shape != "B + C" || st.CallShapes[1].Shape != "B then C" ||
		st.CallShapes[0].Share != 0.5 || st.CallShapes[1].Share != 0.5 {
		t.Errorf("call shapes %+v, want B + C and B then C, half each", st.CallShapes)
	}
	if len(st.VariableCalls) != 0 {
		t.Errorf("variable calls %+v on fixed counts", st.VariableCalls)
	}
	parallelNodesCertain(t, inf.Topology)
}

// TestFixedCountsAreReadAsBefore: calls that a visit makes k or k + 1 times, a fixed fan-out and a staged service are not
// touched: no variable calls, no call shapes, the same nodes.
func TestFixedCountsAreReadAsBefore(t *testing.T) {
	d := &node{name: "d", c: 2, pods: 1, mean: 8}
	e := &node{name: "e", c: 1, pods: 1, mean: 20}
	a := &node{name: "a", c: 4, pods: 1, mean: 3, children: []*node{d, d, e}, p: []float64{1, 0.3, 0.015}}
	inf := infer1(t, run(a, 40, 200, 13, false).spans)
	for _, st := range inf.Stats {
		if len(st.VariableCalls) != 0 || len(st.CallShapes) != 0 {
			t.Errorf("%s: variable calls %+v, call shapes %+v", st.Name, st.VariableCalls, st.CallShapes)
		}
	}
	if hasWarning(inf, "calls per visit vary") || hasWarning(inf, "call shapes") {
		t.Errorf("warnings for a fixed count: %v", inf.Warnings)
	}
}

// TestVariableCountsAreDeterministic: the same capture gives the same topology, statistics and warnings, run after run.
func TestVariableCountsAreDeterministic(t *testing.T) {
	spans := handTraces(600, 200, func(i int) float64 { return 2 + 4.2 }, func(i int) []handCall {
		switch i % 4 {
		case 0:
			return fanCalls("B", 2, 1, 4)
		case 1:
			return seqCalls("B", 3, 1, 4)
		case 2:
			return append(fanCalls("B", 3, 1, 4), seqCalls("C", 2, 6, 3)...)
		}
		return nil
	})
	var first string
	for k := 0; k < 3; k++ {
		inf, err := inferErr(t, spans)
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		got := fmt.Sprintf("%+v\n%+v\n%q", inf.Topology.Services, inf.Stats, inf.Warnings)
		if k == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d differs from the first:\n%s\n%s", k, got, first)
		}
	}
}

// TestRandomShapesKeepTheLoad: callers of random request kinds (one to six kinds, each a sequence of up to four steps, a
// step one call or up to four calls at once to one of three callees; a third of the callers only one call at a time), whatever the rules make of them, are either refused
// by name or read so that every service gets the arrival rate that was observed (0.1%), no parallel node has a call
// with a probability below 1, and a second read gives the same topology.
func TestRandomShapesKeepTheLoad(t *testing.T) {
	callees := []string{"B", "C", "D"}
	accepted, refused := 0, 0
	paths := map[string]int{}
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		type step struct {
			callee string
			n      int
		}
		kinds := make([][]step, 1+rng.Intn(6))
		weights := make([]int, len(kinds))
		width := 4
		if seed%3 == 0 {
			width = 1 // a caller that only makes its calls one after another
		}
		for k := range kinds {
			weights[k] = 1 + rng.Intn(5)
			for s := rng.Intn(5); s > 0; s-- {
				kinds[k] = append(kinds[k], step{callees[rng.Intn(3)], 1 + rng.Intn(width)})
			}
		}
		var order []int
		for k, w := range weights {
			for j := 0; j < w; j++ {
				order = append(order, k)
			}
		}
		spans := handTraces(700, 300, func(i int) float64 {
			d := 2.0
			for _, st := range kinds[order[i%len(order)]] {
				d += 5
				_ = st
			}
			return d + 1
		}, func(i int) []handCall {
			var cs []handCall
			at := 1.0
			for _, st := range kinds[order[i%len(order)]] {
				cs = append(cs, fanCalls(st.callee, st.n, at, 4)...)
				at += 5
			}
			return cs
		})
		inf, err := inferErr(t, spans)
		if err != nil {
			refused++
			if !strings.Contains(err.Error(), "call shapes") && !strings.Contains(err.Error(), "calls in parallel") {
				t.Errorf("seed %d: refused for another reason: %v", seed, err)
			}
			continue
		}
		accepted++
		how := "fixed"
		for _, v := range inf.Stats[inf.Topology.Index("A")].VariableCalls {
			how = v.ModelledAs
		}
		if len(inf.Stats[inf.Topology.Index("A")].CallShapes) > 0 && how == "fixed" {
			how = "mixture, no varying count"
		}
		paths[how]++
		parallelNodesCertain(t, inf.Topology)
		res, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		for i, st := range inf.Stats {
			if st.Stage {
				continue
			}
			if got := res.Lam[i] * 1000; math.Abs(got-st.RateRPS) > 0.001*st.RateRPS {
				t.Errorf("seed %d: %s: the model's rate %.4f/s, observed %.4f/s (kinds %v, weights %v)", seed, st.Name, got, st.RateRPS, kinds, weights)
			}
		}
		again, err := inferErr(t, spans)
		if err != nil || fmt.Sprintf("%+v", again.Topology.Services) != fmt.Sprintf("%+v", inf.Topology.Services) {
			t.Errorf("seed %d: a second read differs (%v)", seed, err)
		}
	}
	t.Logf("%d random callers read, %d refused; by how the counts were taken: %v", accepted, refused, paths)
	if accepted < 40 {
		t.Errorf("only %d of 60 random callers were read", accepted)
	}
}
