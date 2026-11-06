// SPDX-License-Identifier: Apache-2.0

package infer_test

// Regression tests for the OTLP reader and the inference (each failed at 094b99a).

import (
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// A server span whose endTimeUnixNano is 0 (or before its start) was kept by
// the reader, and its duration wrapped as a uint64 (own time 1.1e10 ms).
func TestSpanEndBeforeStart(t *testing.T) {
	out := run(chain(), 25, 60, 1, false)
	spans := out.spans
	for i := range spans {
		if spans[i].kind == 2 && spans[i].svc == "b" {
			spans[i].end = -1_700_000_000_000 // endTimeUnixNano = 0 in writeOTLP
			break
		}
	}
	inf := infer1(t, spans)
	bi := inf.Topology.Index("b")
	st := inf.Stats[bi]
	if !(st.OwnMeanMs < 1000) || !(st.ObservedServerMs < 1000) {
		t.Errorf("one span with end 0 poisons b: own %.4g ms, server %.4g ms", st.OwnMeanMs, st.ObservedServerMs)
	}
}

// A call made below 8 or more nested internal spans lost its owner: the
// callee was counted as a second entry request and the edge disappeared.
func TestDeepInternalSpans(t *testing.T) {
	var spans []span
	id := 0
	nid := func() string { id++; return fmt.Sprintf("%016x", id) }
	for tr := 1; tr <= 300; tr++ {
		t0 := float64(tr) * 50
		root := nid()
		spans = append(spans, span{svc: "a", pod: "a-0", id: root, kind: 2, start: t0, end: t0 + 10, trace: tr})
		parent := root
		for d := 0; d < 9; d++ {
			x := nid()
			spans = append(spans, span{svc: "a", pod: "a-0", id: x, parent: parent, kind: 1, start: t0 + 0.1*float64(d+1), end: t0 + 9.9 - 0.1*float64(d), trace: tr})
			parent = x
		}
		cl := nid()
		spans = append(spans, span{svc: "a", pod: "a-0", id: cl, parent: parent, kind: 3, start: t0 + 2, end: t0 + 6, trace: tr})
		spans = append(spans, span{svc: "b", pod: "b-0", id: nid(), parent: cl, kind: 2, start: t0 + 2.2, end: t0 + 5.8, trace: tr})
	}
	inf := infer1(t, spans)
	if inf.Requests != 300 {
		t.Errorf("entry requests: got %d, want 300", inf.Requests)
	}
	ai := inf.Topology.Index("a")
	if len(inf.Topology.Services[ai].Calls) != 1 {
		t.Errorf("a should call b once per visit: calls %+v", inf.Topology.Services[ai].Calls)
	}
}

// The same capture must give the same numbers on every run (map iteration and
// worker finish order used to change the last digits).
func TestInferenceIsDeterministic(t *testing.T) {
	b := &node{name: "b", c: 1, pods: 2, mean: 20, dist: "gamma", cs2: 0.5}
	c := &node{name: "c", c: 2, pods: 3, mean: 15}
	d := &node{name: "d", c: 1, pods: 1, mean: 5}
	e := &node{name: "e", c: 1, pods: 1, mean: 7}
	a := &node{name: "a", c: 4, pods: 2, mean: 3, parallel: true, children: []*node{b, c, d, e}}
	out := run(a, 30, 200, 3, false)
	// d and e emit no server spans: they become external peers
	var spans []span
	for _, sp := range out.spans {
		if sp.kind == 2 && (sp.svc == "d" || sp.svc == "e") {
			continue
		}
		spans = append(spans, sp)
	}
	dir := writeOTLP(t, spans)
	prev := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(prev)
	var first string
	diffs := 0
	for i := 0; i < 12; i++ {
		data, err := otlp.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		inf, err := infer.Run(data, infer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		tj, _ := model.MarshalTopology(inf.Topology)
		sj, _ := json.Marshal(inf.Stats)
		wj, _ := json.Marshal(inf.Warnings)
		got := string(tj) + string(sj) + string(wj) + fmt.Sprint(inf.LamExtPerMs)
		if i == 0 {
			first = got
		} else if got != first {
			diffs++
			if diffs == 1 {
				t.Errorf("run %d differs from run 0:\n%s", i, firstDiff(first, got))
			}
		}
	}
	if diffs > 0 {
		t.Errorf("%d of 11 re-runs differ", diffs)
	}
}

// The cross-check against the reference: the inferred topology's last bits depended on the number of cores (glue 2.0121823585458793 at
// GOMAXPROCS 1 against 2.012182358545754 at 8 on a harness row; branch correlations too): the summation
// chunks were sized from GOMAXPROCS. The same capture must give the same bytes at any core count.
func TestInferenceDoesNotDependOnTheCoreCount(t *testing.T) {
	b := &node{name: "b", c: 1, pods: 2, mean: 20, dist: "gamma", cs2: 0.5}
	c := &node{name: "c", c: 2, pods: 3, mean: 15}
	d := &node{name: "d", c: 1, pods: 1, mean: 5, post: 1.5, children: []*node{{name: "e", c: 2, pods: 1, mean: 3}}}
	a := &node{name: "a", c: 4, pods: 2, mean: 3, post: 2, parallel: true, children: []*node{b, c, d}}
	out := run(a, 30, 400, 5, true)
	dir := writeOTLP(t, out.spans)
	prev := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(prev)
	var first string
	for _, procs := range []int{1, 2, 3, 8} {
		runtime.GOMAXPROCS(procs)
		data, err := otlp.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		inf, err := infer.Run(data, infer.Options{})
		if err != nil {
			t.Fatal(err)
		}
		tj, _ := model.MarshalTopology(inf.Topology)
		sj, _ := json.Marshal(inf.Stats)
		wj, _ := json.Marshal(inf.Warnings)
		got := string(tj) + string(sj) + string(wj) + fmt.Sprint(inf.LamExtPerMs)
		if procs == 1 {
			first = got
			if !strings.Contains(string(tj), `"glue"`) || !strings.Contains(string(tj), `"branch_corr"`) {
				t.Fatalf("the capture should give glue and a branch correlation: %s", tj)
			}
		} else if got != first {
			t.Errorf("GOMAXPROCS %d differs from 1:\n%s", procs, firstDiff(first, got))
		}
	}
}

func firstDiff(a, b string) string {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			lo := max(0, i-120)
			return fmt.Sprintf("run0: ...%s...\nrunN: ...%s...", a[lo:min(len(a), i+80)], b[lo:min(len(b), i+80)])
		}
	}
	return "length differs"
}

// A fast leaf service (own time under 0.2 ms per visit) left no busy segment,
// so its workers were read as 1 and it looked like a single busy queue.
func TestFastLeafServiceWorkers(t *testing.T) {
	b := &node{name: "b", c: 16, pods: 1, mean: 0.12, dist: "gamma", cs2: 0.02}
	a := &node{name: "a", c: 64, pods: 1, mean: 1, dist: "gamma", cs2: 0.2, children: []*node{b}}
	out := run(a, 6000, 6, 9, false)
	inf := infer1(t, out.spans)
	base, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
	if err != nil {
		t.Fatal(err)
	}
	bi := inf.Topology.Index("b")
	ai := inf.Topology.Index("a")
	st := inf.Stats[bi]
	truth := mean(out.totalMs)
	t.Logf("b: workers %d (inferred %v), S %.3f ms, rho %.2f; entry model %.3f ms vs simulated %.3f ms",
		st.WorkersPerPod, st.WorkersInferred, st.OwnMeanMs, base.Rho[bi], base.L[ai], truth)
	if base.Stable && base.Rho[bi] > 0.5 {
		t.Errorf("b (16 workers, 6000 rps x 0.12 ms = 0.72 erlangs) is modelled at %.0f%% utilization", 100*base.Rho[bi])
	}
	if !base.Stable || math.Abs(base.L[ai]-truth) > 0.08*truth {
		t.Errorf("entry latency: model %.3f ms, simulated %.3f ms", base.L[ai], truth)
	}
}

// Options: a NaN kept share and a single entry request gave NaN rates.
func TestOptionsGuards(t *testing.T) {
	out := run(chain(), 25, 30, 4, false)
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	if inf, err := infer.Run(d, infer.Options{KeptShare: math.NaN()}); err == nil && math.IsNaN(inf.LamExtPerMs) {
		t.Error("KeptShare NaN gives a NaN rate without an error")
	}
	one := []span{{svc: "x", pod: "x-0", id: "0000000000000001", kind: 2, start: 0, end: 1, trace: 1}}
	d1, _ := otlp.ReadDir(writeOTLP(t, one))
	if inf, err := infer.Run(d1, infer.Options{MinRequests: -1}); err == nil && (math.IsNaN(inf.WindowS) || math.IsNaN(inf.LamExtPerMs)) {
		t.Errorf("one entry request: window %v s, rate %v without an error", inf.WindowS, inf.LamExtPerMs)
	}
}
