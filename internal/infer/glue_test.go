// SPDX-License-Identifier: Apache-2.0

package infer_test

// Glue: own time after a visit's first call out, when its worker is already
// free (work between or after the calls, writing the response). It is a delay
// in the model; only the part up to the first call out is service time.

import (
	"fmt"
	"math"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// glued is a -> b where a writes its response for 3 ms after b answers, with its worker already free (as a
// server that frees its worker at its first call out does). That time is glue: it adds to the latency but
// queues nobody, so it must not enter a's service time or its busy time.
func glued(shiftA float64) *node {
	b := &node{name: "b", c: 1, pods: 1, mean: 20}
	return &node{name: "a", c: 1, pods: 1, mean: 12, shift: shiftA, post: 3, children: []*node{b}}
}

// TestGlueQueueOutsideSpan and TestGlueQueueInsideSpan: at 77e6b8d a read 15 ms of service time (12 + 3), so
// the model put it at 56% utilization instead of 45%: entry latency +4%, the change of a +3 ms change at a
// +72% (11.3 ms against 6.6), and the earlier estimator read 5 workers at a in the first mode.
func TestGlueQueueOutsideSpan(t *testing.T) { checkGlue(t, false) }

func TestGlueQueueInsideSpan(t *testing.T) { checkGlue(t, true) }

func checkGlue(t *testing.T, inSpan bool) {
	const lam, shift = 37.5, 3.0
	out := run(glued(0), lam, 400, 7, inSpan)
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	topo := inf.Topology
	a, b := topo.Index("a"), topo.Index("b")
	sa, sb := topo.Services[a], topo.Services[b]
	within(t, "a service time", sa.S, 12, 0.07)
	within(t, "a glue", sa.Glue, 3, 0.01)
	within(t, "b service time", sb.S, 20, 0.07)
	if sb.Glue != 0 {
		t.Errorf("b makes no calls, so it has no glue: got %.4g ms", sb.Glue)
	}
	if got := sa.Servers * sa.Workers; got != 1 {
		t.Errorf("a workers: got %d, want 1", got)
	}
	if inSpan != inf.Stats[a].WorkersFromQueueing {
		t.Errorf("a's count read from queueing: %v, want %v", inf.Stats[a].WorkersFromQueueing, inSpan)
	}
	base, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
	if err != nil || !base.Stable {
		t.Fatalf("model: %v", err)
	}
	within(t, "a utilization", base.Rho[a], lam/1000*12, 0.07)
	within(t, "entry latency (model vs simulation)", base.L[a], mean(out.totalMs), 0.05)

	var obs float64
	for seed := int64(1); seed <= 6; seed++ {
		o1 := run(glued(0), lam, 400, seed, inSpan)
		o2 := run(glued(shift), lam, 400, seed, inSpan)
		obs += (mean(o2.totalMs) - mean(o1.totalMs)) / 6
	}
	ds := make([]float64, len(topo.Services))
	sh := make([]bool, len(ds))
	ds[a], sh[a] = shift, true
	dl, err := model.ChangeShiftWith(topo, inf.LamExtPerMs, ds, sh, model.Default)
	if err != nil || !dl.Stable {
		t.Fatalf("change: %v", err)
	}
	t.Logf("a S %.3f glue %.3f cap %d; entry %.2f ms vs %.2f simulated; change at a %+.2f ms vs %+.2f simulated",
		sa.S, sa.Glue, sa.Servers*sa.Workers, base.L[a], mean(out.totalMs), dl.DL[a], obs)
	if math.Abs(dl.DL[a]-obs) > 0.10*obs+1.0 {
		t.Errorf("change of the entry latency: model %+.2f ms, simulation %+.2f ms", dl.DL[a], obs)
	}

	// the earlier estimator: only [start, first call out] is busy time, so a's count is not inflated
	rise, err := infer.Run(d, infer.Options{Estimator: infer.EstimatorRise})
	if err != nil {
		t.Fatal(err)
	}
	r := rise.Topology.Services[rise.Topology.Index("a")]
	if got := r.Servers * r.Workers; got != 1 {
		t.Errorf("a workers (rise estimator): got %d, want 1", got)
	}
	within(t, "a service time (rise estimator)", r.S, 12, 0.07)
	within(t, "a glue (rise estimator)", r.Glue, 3, 0.01)
}

// glueSpans builds 300 requests, 50 ms apart (nobody waits), to service a. On odd requests a works 4 ms,
// calls its callees and returns at +10 ms; on even ones it calls nothing and works 6 ms. Sequential: b over
// [4, 6] and c over [7, 9], so 1 ms between the calls and 1 ms after them is glue (2 ms). Parallel: b over
// [4, 8] and c over [4.5, 7], so 2 ms after the slower call is glue.
func glueSpans(parallel bool) []span {
	var spans []span
	id := 0
	nid := func() string { id++; return fmt.Sprintf("%016x", id) }
	for tr := 1; tr <= 300; tr++ {
		t0 := float64(tr) * 50
		root := nid()
		if tr%2 == 0 {
			spans = append(spans, span{svc: "a", pod: "a-0", id: root, kind: 2, start: t0, end: t0 + 6, trace: tr})
			continue
		}
		spans = append(spans, span{svc: "a", pod: "a-0", id: root, kind: 2, start: t0, end: t0 + 10, trace: tr})
		calls := [][2]float64{{4, 6}, {7, 9}}
		if parallel {
			calls = [][2]float64{{4, 8}, {4.5, 7}}
		}
		for k, c := range calls {
			callee := []string{"b", "c"}[k]
			cl := nid()
			spans = append(spans, span{svc: "a", pod: "a-0", id: cl, parent: root, kind: 3, start: t0 + c[0], end: t0 + c[1], trace: tr, peer: callee})
			spans = append(spans, span{svc: callee, pod: callee + "-0", id: nid(), parent: cl, kind: 2, start: t0 + c[0] + 0.1, end: t0 + c[1] - 0.1, trace: tr})
		}
	}
	return spans
}

// Own time after the first call out is glue, averaged over every visit (the visits that call nothing have
// none); the service time is the part before it. Services that make no calls keep all of it as service time.
// In the parallel case a calls b and c together on half of its visits: one stage, called with probability
// 0.5 (until the structure rules were ported it was read as an always-parallel fan-out, which doubled the
// calls to b and c).
func TestGlueIsReadPerVisit(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		d, err := otlp.ReadDir(writeOTLP(t, glueSpans(parallel)))
		if err != nil {
			t.Fatal(err)
		}
		for _, est := range []string{infer.EstimatorFIFO, infer.EstimatorRise} {
			inf, err := infer.Run(d, infer.Options{Estimator: est, MinRequests: 100})
			if err != nil {
				t.Fatal(err)
			}
			what := fmt.Sprintf("parallel=%v %s", parallel, est)
			topo := inf.Topology
			ai := topo.Index("a")
			sa, st := topo.Services[ai], inf.Stats[ai]
			if sa.Parallel {
				t.Errorf("%s: a's calls are sequential or one stage, not an always-parallel fan-out", what)
			}
			if v := topo.VisitRatios(); math.Abs(v[topo.Index("b")]-0.5) > 1e-12 || math.Abs(v[topo.Index("c")]-0.5) > 1e-12 {
				t.Errorf("%s: visits to b and c per request %.4g and %.4g, want 0.5", what, v[topo.Index("b")], v[topo.Index("c")])
			}
			if parallel {
				st := topo.Index("stage:a:1")
				if st < 0 || len(sa.Calls) != 1 || sa.Calls[0].Child != st || sa.Calls[0].P != 0.5 ||
					!topo.Services[st].Parallel || !topo.Services[st].Delay || topo.Services[st].S != 0 {
					t.Errorf("%s: a should call one parallel stage (b + c) with p 0.5: %+v", what, sa.Calls)
				}
			}
			near := func(name string, got, want float64) {
				t.Helper()
				if math.Abs(got-want) > 1e-3 {
					t.Errorf("%s: %s %.6f, want %.6f", what, name, got, want)
				}
			}
			near("a service time", sa.S, 5) // (4 + 6) / 2
			near("a stats own time", st.OwnMeanMs, 5)
			near("a glue", sa.Glue, 1) // (2 + 0) / 2
			near("a stats glue", st.GlueMs, sa.Glue)
			near("a glue cs2", sa.GlueCS2, 300.0/299) // 0 or 2 ms, half and half: sample variance 300/299, mean 1
			near("a in-span wait", st.InSpanWaitMs, 0)
			for _, leaf := range []string{"b", "c"} {
				i := topo.Index(leaf)
				if topo.Services[i].Glue != 0 || inf.Stats[i].GlueMs != 0 {
					t.Errorf("%s: %s makes no calls but has glue %.4g", what, leaf, topo.Services[i].Glue)
				}
				want := map[bool]map[string]float64{false: {"b": 1.8, "c": 1.8}, true: {"b": 3.8, "c": 2.3}}[parallel][leaf]
				near(leaf+" service time", topo.Services[i].S, want)
			}
			if got := sa.Servers * sa.Workers; got != 1 {
				t.Errorf("%s: a workers %d, want 1", what, got)
			}
		}
	}
}
