// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"math"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// TestUnevenReplicaSplit: b's two pods take 75% and 25% of its visits, as keep-alive connections behind a
// Kubernetes Service can hold them; c's two pods split evenly. The inference flags b's split and not c's,
// the model fed the measured shares matches the simulation, and the even split it would otherwise assume
// is far off. A change in b keeps the shares, in the simulation as in the model.
func TestUnevenReplicaSplit(t *testing.T) {
	mk := func(shift float64) *node {
		b := &node{name: "b", c: 1, pods: 2, mean: 10, shares: []float64{0.75, 0.25}, shift: shift}
		c := &node{name: "c", c: 1, pods: 2, mean: 6}
		return &node{name: "a", c: 4, pods: 1, mean: 2, children: []*node{b, c}}
	}
	const lam = 90.0 // b's pods at 68% and 23%, 45% each if even
	out := run(mk(0), lam, 300, 17, false)
	inf := infer1(t, out.spans)
	topo := inf.Topology
	a, b, c := topo.Index("a"), topo.Index("b"), topo.Index("c")
	if a < 0 || b < 0 || c < 0 {
		t.Fatalf("services: %v", topo.Names())
	}
	sb, sc := inf.Stats[b], inf.Stats[c]
	if !sb.UnevenSplit || len(topo.Services[b].Shares) != 2 || len(sb.PodShares) != 2 || sb.PodShares[0].Pod != "b-0" {
		t.Fatalf("b's split should be flagged and modelled: %+v, shares %v", sb, topo.Services[b].Shares)
	}
	within(t, "b-0's share", topo.Services[b].Shares[0], 0.75, 0.015)
	if sc.UnevenSplit || topo.Services[c].Shares != nil || sc.SplitP == nil || !(*sc.SplitP > 1e-6) {
		t.Errorf("c's even split should be tested and pass: %+v", sc)
	}
	warned := false
	for _, w := range inf.Warnings {
		warned = warned || strings.HasPrefix(w, "b: its 2 pods took uneven shares")
		if strings.HasPrefix(w, "c:") && strings.Contains(w, "uneven") {
			t.Errorf("c flagged: %s", w)
		}
	}
	if !warned {
		t.Errorf("no warning for b's split: %q", inf.Warnings)
	}

	base, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
	if err != nil || !base.Stable {
		t.Fatalf("model: %v", err)
	}
	seen := mean(out.totalMs)
	within(t, "entry latency (model with the shares vs simulation)", base.L[a], seen, 0.05)
	within(t, "b's utilization is its busy pod's", base.Rho[b], 0.675, 0.04)
	even := *topo
	even.Services = append([]model.Service(nil), topo.Services...)
	even.Services[b].Shares = nil
	if err := even.Finish(); err != nil {
		t.Fatal(err)
	}
	re, _ := model.AnalyticWith(&even, inf.LamExtPerMs, nil, model.Default)
	if e := (re.L[a] - seen) / seen; e > -0.15 {
		t.Errorf("the even split should be far low here: %.1f%% (model %.2f, simulation %.2f ms)", 100*e, re.L[a], seen)
	}

	// +1.5 ms of fixed time in b (its busy pod to 78%); the truth is the mean over 6 re-simulations with
	// common random numbers
	const shift = 1.5
	var obs float64
	for seed := int64(1); seed <= 6; seed++ {
		o1 := run(mk(0), lam, 300, seed, false)
		o2 := run(mk(shift), lam, 300, seed, false)
		obs += (mean(o2.totalMs) - mean(o1.totalMs)) / 6
	}
	ds := make([]float64, len(topo.Services))
	sh := make([]bool, len(topo.Services))
	ds[b], sh[b] = shift, true
	d, err := model.ChangeShiftWith(topo, inf.LamExtPerMs, ds, sh, model.Default)
	if err != nil || !d.Stable {
		t.Fatalf("change: %v", err)
	}
	de, err := model.ChangeShiftWith(&even, inf.LamExtPerMs, ds, sh, model.Default)
	if err != nil || !de.Stable {
		t.Fatalf("change, even split: %v", err)
	}
	t.Logf("entry %.2f ms simulated, %.2f with the shares, %.2f even; change %+.2f simulated, %+.2f with the shares, %+.2f even",
		seen, base.L[a], re.L[a], obs, d.DL[a], de.DL[a])
	if math.Abs(d.DL[a]-obs) > 0.10*obs+0.3 {
		t.Errorf("change of the entry latency: model %+.2f ms, simulation %+.2f ms", d.DL[a], obs)
	}
}
