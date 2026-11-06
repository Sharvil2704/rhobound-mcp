// SPDX-License-Identifier: Apache-2.0

package infer_test

// Calls sent after the caller's response (fire and forget) load the callee but are not on the caller's latency
// path. The captures are the code review's (async_slow): fe is its cap-base (one
// worker, exponential 10 ms, 60 requests/s, spans that include the wait), and 30% of its visits call analytics
// (exponential 200 ms) 50 ms after fe's span ended, a service with SERVER spans (async-int) or an external peer
// (async-ext). At 5b71d67 the queue read right (1 x 1, 9.89 ms), but the calls counted on fe's path: its level
// was +260.8% (88.73 ms against 24.60 seen, with no warning) and --patch analytics=+100ms moved fe by +34.92 ms,
// where the truth is 0.

import (
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// asyncSpans is async-int (internal) or async-ext: cap-base, and on a share of its visits a call to analytics
// 50 ms after fe's span ended. inside is the share of those calls that start halfway through fe's service instead:
// fire and forget (the call outlives fe's span) or, with waited, a call fe waits for (it ends just before fe's span).
// It returns the spans, the clean capture (the same fe spans without the calls) and the number of calls.
func asyncSpans(internal bool, share, inside float64, waited bool) (spans, clean []span, n int) {
	arr, start, end := entryQueue(3000, 1)
	x := &ids{}
	clean = rootSpans(arr, end, x)
	rng := rand.New(rand.NewSource(11))
	spans = append(spans, clean...)
	for i := range clean {
		if rng.Float64() >= share {
			continue
		}
		cs, d := end[i]+50, 200*rng.ExpFloat64()
		if rng.Float64() < inside {
			cs = (start[i] + end[i]) / 2
			if waited {
				d = end[i] - 0.01 - cs - 0.2
			} else {
				d += end[i] - cs
			}
		}
		c := x.next()
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: c, parent: clean[i].id, kind: 3, start: cs, end: cs + d + 0.2,
			trace: i + 1, peer: "analytics", port: 80})
		if internal {
			spans = append(spans, span{svc: "analytics", pod: "analytics-0", id: x.next(), parent: c, kind: 2, start: cs + 0.1,
				end: cs + 0.1 + d, trace: i + 1})
		}
		n++
	}
	return spans, clean, n
}

// asyncCallee returns fe's calls to the callee named name: the async ones' summed probability, and whether
// there is any call to it that is not async.
func asyncCallee(topo *model.Topology, caller, name string) (p float64, sync bool) {
	for _, c := range topo.Services[topo.Index(caller)].Calls {
		if topo.Services[c.Child].Name != name {
			continue
		}
		if c.Async {
			p += c.P
		} else {
			sync = true
		}
	}
	return p, sync
}

func warned(inf *infer.Inferred, parts ...string) bool {
	for _, w := range inf.Warnings {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func TestCallsAfterTheResponseAreAsync(t *testing.T) {
	for _, internal := range []bool{true, false} {
		// inside 3%: fire and forget from within the span (the calls outlive it); the edge is async and those calls
		// free no worker and cover none of its own time
		for _, inside := range []float64{0, 0.03} {
			spans, clean, n := asyncSpans(internal, 0.3, inside, false)
			callee := "analytics"
			if !internal {
				callee = "ext:analytics"
			}
			what := callee
			if inside > 0 {
				what += ", 3% of the calls within the span"
			}
			inf, base := infer1(t, spans), infer1(t, clean)
			sameQueue(t, what, inf, base)
			p, sync := asyncCallee(inf.Topology, "fe", callee)
			if sync || math.Abs(p-float64(n)/3000) > 1e-12 {
				t.Errorf("%s: fe's calls to %s: async p %.4f (want %.4f), a call on the path %v", what, callee, p, float64(n)/3000, sync)
			}
			if !warned(inf, "async", "fe -> "+callee) {
				t.Errorf("%s: no warning names the async call: %q", what, inf.Warnings)
			}
			fe, an := inf.Topology.Index("fe"), inf.Topology.Index(callee)
			for _, cfg := range []model.Config{model.Plain, model.Default} {
				r, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, cfg)
				if err != nil || !r.Stable {
					t.Fatalf("%s: %v", what, err)
				}
				rb, err := model.AnalyticWith(base.Topology, base.LamExtPerMs, nil, cfg)
				if err != nil || !rb.Stable {
					t.Fatal(err)
				}
				// fe's latency is the clean capture's: the call is not on its path (at 5b71d67 +260%)
				if math.Abs(r.L[fe]-rb.L[base.Topology.Index("fe")]) > 1e-9 {
					t.Errorf("%s: fe's model latency %.4f ms, the capture without the calls %.4f", what, r.L[fe],
						rb.L[base.Topology.Index("fe")])
				}
				// the callee keeps its load
				if got, want := r.Lam[an]*1000, float64(n)/inf.WindowS; math.Abs(got-want) > 1e-9*want {
					t.Errorf("%s: %s at %.3f /s, want %.3f", what, callee, got, want)
				}
				// a change at the callee does not reach fe (at 5b71d67 +34.92 ms for +100 ms)
				ds, sh := make([]float64, len(inf.Topology.Services)), make([]bool, len(inf.Topology.Services))
				ds[an], sh[an] = 100, true
				d, err := model.ChangePatchWith(inf.Topology, inf.LamExtPerMs, model.Patch{DS: ds, Shift: sh}, cfg)
				if err != nil || !d.Stable {
					t.Fatalf("%s: %v", what, err)
				}
				if d.DL[fe] != 0 {
					t.Errorf("%s: %s +100 ms moves fe by %+.4f ms, want 0", what, callee, d.DL[fe])
				}
			}
		}
	}
}

// A mix: 30% of the calls start after the span, the rest halfway through it and end within it (calls fe waits for).
// The edge is on the latency path (fewer than 95% are not waited for), and the mix is named.
func TestMixedAsyncCallsAreOnThePathAndNamed(t *testing.T) {
	spans, _, _ := asyncSpans(true, 1, 0.7, true)
	inf := infer1(t, spans)
	if p, sync := asyncCallee(inf.Topology, "fe", "analytics"); p != 0 || !sync {
		t.Errorf("fe's calls to analytics: async p %v, on the path %v; want them on the path", p, sync)
	}
	if !warned(inf, "fe -> analytics", "start after fe's span has ended", "on its latency path") {
		t.Errorf("no warning for the mix: %q", inf.Warnings)
	}
}

// A call sent after the response is not a stage of the visit's calls. fe calls b and c at once on every visit and
// analytics 50 ms after its span on 30% of them. At 5b71d67 the late call made a second stage, so fe read as two
// stages (a zero-time node for b + c, then analytics with p 0.3), with analytics on its path.
func TestAsyncCallIsNotAStage(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	rng := rand.New(rand.NewSource(12))
	var spans []span
	for i := range arr {
		root := x.next()
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: root, kind: 2, start: arr[i], end: end[i] + 6, trace: i + 1})
		for j, callee := range []string{"b", "c"} { // at the end of fe's own work, together
			cl := x.next()
			s, e := end[i], end[i]+5-float64(j)
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: cl, parent: root, kind: 3, start: s, end: e, trace: i + 1,
				peer: callee, port: 80},
				span{svc: callee, pod: callee + "-0", id: x.next(), parent: cl, kind: 2, start: s + 0.1, end: e - 0.1, trace: i + 1})
		}
		if rng.Float64() < 0.3 {
			cs := end[i] + 56
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: root, kind: 3, start: cs, end: cs + 20,
				trace: i + 1, peer: "analytics", port: 80})
		}
	}
	inf := infer1(t, spans)
	fe := inf.Topology.Index("fe")
	sv, st := inf.Topology.Services[fe], inf.Stats[fe]
	if !sv.Parallel || st.Stages != 0 {
		t.Errorf("fe: parallel %v, %d stages; want a parallel fan-out to b and c", sv.Parallel, st.Stages)
	}
	var onPath []string
	for _, c := range sv.Calls {
		if !c.Async {
			onPath = append(onPath, inf.Topology.Services[c.Child].Name)
		}
	}
	if strings.Join(onPath, ",") != "b,c" {
		t.Errorf("fe's calls on its path: %v, want b and c", onPath)
	}
	if p, sync := asyncCallee(inf.Topology, "fe", "ext:analytics"); sync || math.Abs(p-0.3) > 0.03 {
		t.Errorf("fe -> ext:analytics: async p %v, on the path %v", p, sync)
	}
	if warned(inf, "stages") {
		t.Errorf("a stage warning: %q", inf.Warnings)
	}
}

// Calls timed by the callee's clock (a SERVER -> SERVER link, no CLIENT span) that start after the caller's span:
// a callee clock that runs ahead looks the same as calls sent after the response. skewSpans with be's clock 30 ms
// ahead puts nearly every call past fe's end; the async warning says the calls are timed by the callee's clock.
func TestAsyncOnTheCalleesClockIsNamed(t *testing.T) {
	inf := infer1(t, skewSpans(-30, 1))
	if p, _ := asyncCallee(inf.Topology, "fe", "be"); p == 0 {
		t.Fatalf("fe -> be did not read as async (%q)", inf.Warnings)
	}
	if !warned(inf, "async", "fe -> be", "timed by the callee's clock", "runs ahead") {
		t.Errorf("the async warning should name the callee's clock: %q", inf.Warnings)
	}
	spans, _, _ := asyncSpans(true, 0.3, 0, false)
	if inf := infer1(t, spans); warned(inf, "timed by the callee's clock") {
		t.Errorf("calls with CLIENT spans of the caller's own pod: %q", inf.Warnings)
	}
}

// A fire-and-forget call sent just before the response: its CLIENT span, on fe's own clock, starts lead ms before
// fe's span ends and outlives it, so fe cannot have waited for it. The code review's ff-pre captures (round 3): at
// 22e1886 only a start after the span counted, so these stayed on fe's path (+214.7% at a 0.05 ms lead, with no
// warning) and a +3 ms change at fe came out +5.80 ms against about +26.
func TestFireAndForgetBeforeTheResponseIsAsync(t *testing.T) {
	for _, lead := range []float64{0.05, 1} {
		arr, _, end := entryQueue(3000, 1)
		x := &ids{}
		clean := rootSpans(arr, end, x)
		rng := rand.New(rand.NewSource(13))
		spans := append([]span(nil), clean...)
		n := 0
		for i := range clean {
			if rng.Float64() >= 0.3 {
				continue
			}
			cs, d := end[i]-lead, 200*rng.ExpFloat64()+2*lead
			c := x.next()
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: c, parent: clean[i].id, kind: 3, start: cs, end: cs + d + 0.2,
				trace: i + 1, peer: "analytics", port: 80},
				span{svc: "analytics", pod: "analytics-0", id: x.next(), parent: c, kind: 2, start: cs + 0.1, end: cs + 0.1 + d, trace: i + 1})
			n++
		}
		inf, base := infer1(t, spans), infer1(t, clean)
		what := fmt.Sprintf("lead %g ms", lead)
		sameQueue(t, what, inf, base)
		if p, sync := asyncCallee(inf.Topology, "fe", "analytics"); sync || math.Abs(p-float64(n)/3000) > 1e-12 {
			t.Errorf("%s: fe -> analytics async p %.4f (want %.4f), on the path %v", what, p, float64(n)/3000, sync)
		}
		r, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
		rb, errb := model.AnalyticWith(base.Topology, base.LamExtPerMs, nil, model.Default)
		if err != nil || errb != nil || !r.Stable || !rb.Stable {
			t.Fatalf("%s: %v %v", what, err, errb)
		}
		if fe := inf.Topology.Index("fe"); math.Abs(r.L[fe]-rb.L[base.Topology.Index("fe")]) > 1e-9 {
			t.Errorf("%s: fe's model latency %.4f ms, the capture without the calls %.4f", what, r.L[fe], rb.L[base.Topology.Index("fe")])
		}
	}
}

// An async edge with a few calls the visit did wait for: every visit calls be, 96% of them after the response and
// 4% inside the span, ending before it. The edge is async (at least 95% start after the span), but the in-span calls
// are waited for: they stay on fe's path with their own probability and release fe's worker at their start. At
// 22e1886 they were taken as async too, so they freed no worker: the rebuild read fe as 1 x 2 at about 7.8 ms
// against a true 1 x 6.06, and a +3 ms change came out -31 to -35% (the review's mixed-0.96 captures, no warning).
func TestWaitedCallsOnAnAsyncEdgeStayOnThePath(t *testing.T) {
	arr, start, end := entryQueue(3000, 1)
	x := &ids{}
	rng := rand.New(rand.NewSource(14))
	var spans []span
	inside := 0
	for i := range arr {
		root := x.next()
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: root, kind: 2, start: arr[i], end: end[i], trace: i + 1})
		cs, ce := end[i]+2, end[i]+7
		if rng.Float64() < 0.04 {
			cs = start[i] + 0.5*(end[i]-start[i])
			ce = end[i] - 0.01
			inside++
		}
		c := x.next()
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: c, parent: root, kind: 3, start: cs, end: ce, trace: i + 1,
			peer: "be", port: 80},
			span{svc: "be", pod: "be-0", id: x.next(), parent: c, kind: 2, start: cs + 0.05, end: ce - 0.05, trace: i + 1})
	}
	inf := infer1(t, spans)
	fe := inf.Topology.Index("fe")
	p, sync := asyncCallee(inf.Topology, "fe", "be")
	if !sync || math.Abs(p-float64(3000-inside)/3000) > 1e-12 {
		t.Errorf("fe -> be: async p %.4f (want %.4f), a waited call on the path %v (want true)", p, float64(3000-inside)/3000, sync)
	}
	if st := inf.Stats[fe]; st.WorkersPerPod != 1 {
		t.Errorf("fe reads %d workers; the in-span calls release the one worker at their start", st.WorkersPerPod)
	}
}
