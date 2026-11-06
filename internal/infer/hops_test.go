// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// fastChain: a -> b -> c with service times of a millisecond or less, so that at 60 requests/s no service is 5% busy.
func fastChain() *node {
	c := &node{name: "c", c: 1, pods: 1, mean: 0.5}
	b := &node{name: "b", c: 2, pods: 1, mean: 1.0, children: []*node{c}}
	return &node{name: "a", c: 1, pods: 1, mean: 0.4, children: []*node{b}}
}

func solved(t *testing.T, inf *infer.Inferred) *model.Result {
	t.Helper()
	res, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
	if err != nil || !res.Stable {
		t.Fatalf("model: %v", err)
	}
	return res
}

// TestHopSpreadOfQuietEdges: the network time of a call varies (each leg takes hopMs plus 0.1 ms on average, a skewed
// spread), and a callee at 3% utilization has no wait to speak of. The quietest tenth of (client span time less
// server span time) lies 0.16 ms below the mean network time on each call, so a hop taken as that tenth leaves every
// call 0.16 ms short (-11% of the root's latency here); the hop is the mean, which the simulator knows.
func TestHopSpreadOfQuietEdges(t *testing.T) {
	const jitter = 0.1
	for _, spanq := range []bool{false, true} {
		out := runWith(fastChain(), 60, 300, 11, simOpts{hopJitter: jitter, queueInSpan: spanq})
		inf := infer1(t, out.spans)
		res := solved(t, inf)
		topo := inf.Topology
		a, b, c := topo.Index("a"), topo.Index("b"), topo.Index("c")
		want := 2 * (hopMs + jitter)
		for _, x := range []int{b, c} {
			if h := topo.Services[x].Hop; math.Abs(h-want) > 0.03 {
				t.Errorf("spans at arrival %v: %s: hop %.3f ms, want about %.3f (the mean of the two legs)", spanq, topo.Services[x].Name, h, want)
			}
			if !inf.Stats[x].HopFromMean {
				t.Errorf("spans at arrival %v: %s: the hop is not read from the mean", spanq, topo.Services[x].Name)
			}
			if w := inf.Stats[x].ObservedWaitMs; w > 0.05 {
				t.Errorf("spans at arrival %v: %s: seen wait %.3f ms at 3%% utilization", spanq, topo.Services[x].Name, w)
			}
		}
		within(t, fmt.Sprintf("root latency, spans at arrival %v", spanq), res.L[a], mean(out.totalMs), 0.015)
	}
}

// TestHopOfLoadedEdgesIsTheQuietestTenth: a callee at 60% utilization whose spans start at a worker: the gap holds its
// queue, which the model has, so the hop stays the quietest tenth of the gaps (the simulator's network time, 0.3 ms
// here without jitter) and the wait is what the traces show.
func TestHopOfLoadedEdgesIsTheQuietestTenth(t *testing.T) {
	out := runWith(chain(), 60, 400, 5, simOpts{})
	inf := infer1(t, out.spans)
	res := solved(t, inf)
	topo := inf.Topology
	for _, name := range []string{"b", "c"} {
		x := topo.Index(name)
		if res.Rho[x] < 0.4 {
			t.Fatalf("%s is %.0f%% busy; the test wants a loaded callee", name, 100*res.Rho[x])
		}
		if inf.Stats[x].HopFromMean {
			t.Errorf("%s (%.0f%% busy): the hop is read from the mean", name, 100*res.Rho[x])
		}
		if h := topo.Services[x].Hop; math.Abs(h-2*hopMs) > 0.02 {
			t.Errorf("%s: hop %.3f ms, want the simulator's %.3f", name, h, 2*hopMs)
		}
	}
}

// TestSharedCalleePerEdgeHops: d is called by b over a fast link and by c over a slow one. The hop of a call is its
// edge's, not the callee's pooled one: with one hop per callee the calls from the fast caller would be charged the slow
// link's time, or the reverse.
func TestSharedCalleePerEdgeHops(t *testing.T) {
	topo, inf := sharedCallee(t, false)
	a, b, c, d := topo.Index("a"), topo.Index("b"), topo.Index("c"), topo.Index("d")
	_ = a
	// the network time of a call is its two legs: 2 x (hop + jitter)
	wantBD, wantCD := 2*(sharedHopBD+sharedJitter), 2*(sharedHopCD+sharedJitter)
	if got := edgeHop(t, topo, b, d); math.Abs(got-wantBD) > 0.04 {
		t.Errorf("b -> d hop %.3f ms, want about %.3f", got, wantBD)
	}
	if got := edgeHop(t, topo, c, d); math.Abs(got-wantCD) > 0.04 {
		t.Errorf("c -> d hop %.3f ms, want about %.3f", got, wantCD)
	}
	// d's own hop (what the report adds to its latency) is the mean of its edges' by the calls each made: equal here
	if got, want := topo.Services[d].Hop, (wantBD+wantCD)/2; math.Abs(got-want) > 0.04 {
		t.Errorf("d hop %.3f ms, want the call-weighted mean about %.3f", got, want)
	}
	if !inf.Stats[d].HopFromMean {
		t.Error("d: the hop is not read from the mean")
	}
	// the model's latency of d as its callers saw it is the client spans' mean
	res := solved(t, inf)
	within(t, "d as its callers saw it", res.L[d]+topo.Services[d].Hop, inf.Stats[d].ObservedClientMs, 0.02)
	// a call that names its edge's hop is kept by the topology file
	data, err := model.MarshalTopology(topo)
	if err != nil {
		t.Fatal(err)
	}
	back, err := model.ParseTopology(data)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := model.AnalyticWith(back, inf.LamExtPerMs, nil, model.Default)
	if err != nil {
		t.Fatal(err)
	}
	for i := range res.L {
		if math.Abs(r2.L[i]-res.L[i]) > 1e-9*math.Max(1, res.L[i]) {
			t.Errorf("%s: latency %v after a round trip through the topology file, was %v", topo.Services[i].Name, r2.L[i], res.L[i])
		}
	}
	if h := edgeHop(t, back, c, d); math.Abs(h-edgeHop(t, topo, c, d)) > 1e-9 {
		t.Errorf("c -> d hop %v after a round trip, was %v", h, edgeHop(t, topo, c, d))
	}
}

// TestHopsOfCallsInStages: a calls b and d at once (a stage of a zero-time node), then c, which calls d over a slower
// link. The calls inside the stage carry the hop of their edge too.
func TestHopsOfCallsInStages(t *testing.T) {
	topo, _ := sharedCallee(t, true)
	a, c, d := topo.Index("a"), topo.Index("c"), topo.Index("d")
	var stage = -1
	for i := range topo.Services {
		if topo.Services[i].Delay && topo.Services[i].Parallel {
			stage = i
		}
	}
	if stage < 0 {
		t.Fatalf("no stage node in the topology: %+v", topo.Services)
	}
	found := false
	for _, call := range topo.Services[stage].Calls {
		if call.Child == d {
			found = true
			if want := 2 * (sharedHopBD + sharedJitter); !call.HasHop || math.Abs(call.Hop-want) > 0.04 {
				t.Errorf("a's call to d in its stage: hop %.3f (has %v), want about %.3f", call.Hop, call.HasHop, want)
			}
		}
	}
	if !found {
		t.Errorf("the stage does not call d: %+v", topo.Services[stage].Calls)
	}
	// the calls to the stage node itself have no hop (a zero-time node), and c -> d has its own
	for _, call := range topo.Services[a].Calls {
		if call.Child == stage && call.HasHop {
			t.Errorf("the call from a to its stage has a hop: %+v", call)
		}
	}
	if want := 2 * (sharedHopCD + sharedJitter); math.Abs(edgeHop(t, topo, c, d)-want) > 0.04 {
		t.Errorf("c -> d hop %.3f ms, want about %.3f", edgeHop(t, topo, c, d), want)
	}
}

const (
	sharedHopBD  = 0.05 // one way, ms
	sharedHopCD  = 0.50
	sharedHopA   = 0.10
	sharedJitter = 0.10 // mean extra time per leg, exponential
)

func edgeHop(t *testing.T, topo *model.Topology, from, to int) float64 {
	t.Helper()
	for _, call := range topo.Services[from].Calls {
		if call.Child == to {
			if !call.HasHop {
				t.Fatalf("the call %s -> %s has no hop of its own", topo.Services[from].Name, topo.Services[to].Name)
			}
			return call.Hop
		}
	}
	t.Fatalf("no call %s -> %s", topo.Services[from].Name, topo.Services[to].Name)
	return 0
}

// sharedCallee builds 3000 requests 20 ms apart (nothing queues) to a, which calls b then c (sequential), or, with
// staged, b and d at once and then c; b calls d (not when staged) and c calls d. It returns the inferred topology.
func sharedCallee(t *testing.T, staged bool) (*model.Topology, *infer.Inferred) {
	t.Helper()
	const n = 3000
	rng := rand.New(rand.NewSource(11))
	var spans []span
	next := 0
	id := func() string { next++; return fmt.Sprintf("%016x", next) }
	leg := func(h float64) float64 { return h + rng.ExpFloat64()*sharedJitter }
	for i := 0; i < n; i++ {
		tr := i + 1
		t0 := float64(i) * 20
		// call: caller's client span over the callee's server span; body runs the callee's own work and returns its end
		call := func(caller, callerSrv, callee string, hop, start float64, body func(srv string, start float64) float64) float64 {
			cID, sID := id(), id()
			in, out := leg(hop), leg(hop)
			sStart := start + in
			sEnd := body(sID, sStart)
			spans = append(spans, span{svc: callee, pod: callee + "-0", id: sID, parent: cID, kind: 2, start: sStart, end: sEnd, trace: tr})
			end := sEnd + out
			spans = append(spans, span{svc: caller, pod: caller + "-0", id: cID, parent: callerSrv, kind: 3, start: start, end: end, trace: tr, peer: callee})
			return end
		}
		leaf := func(own float64) func(string, float64) float64 {
			return func(_ string, start float64) float64 { return start + own }
		}
		aID := id()
		now := t0 + 0.2
		if staged {
			e1 := call("a", aID, "b", sharedHopA, now, leaf(0.3))
			e2 := call("a", aID, "d", sharedHopBD, now, leaf(0.3))
			now = math.Max(e1, e2)
		} else {
			now = call("a", aID, "b", sharedHopA, now, func(srv string, start float64) float64 {
				return call("b", srv, "d", sharedHopBD, start+0.2, leaf(0.3)) + 0.1
			})
		}
		now = call("a", aID, "c", sharedHopA, now, func(srv string, start float64) float64 {
			return call("c", srv, "d", sharedHopCD, start+0.2, leaf(0.3)) + 0.1
		})
		spans = append(spans, span{svc: "a", pod: "a-0", id: aID, kind: 2, start: t0, end: now + 0.1, trace: tr})
	}
	inf := infer1(t, spans)
	return inf.Topology, inf
}

// TestEdgeWithFewCallsTakesItsCalleesPooledGaps: d is called by c on 3000 requests over a fast link and by b on 40 of them over
// a slow one. The 40 calls are too few to read a hop from (a quarter of the minimum would put the quietest tenth on four
// calls), so the edge b -> d takes the gaps of all d's callers: the fast link's, which are almost all of them. Read from its own
// 40 calls the hop would be the slow link's whole time (about 1.0 ms against 0.3).
func TestEdgeWithFewCallsTakesItsCalleesPooledGaps(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	var spans []span
	next := 0
	id := func() string { next++; return fmt.Sprintf("%016x", next) }
	leg := func(h float64) float64 { return h + rng.ExpFloat64()*sharedJitter }
	for i := 0; i < 3000; i++ {
		tr, t0 := i+1, float64(i)*20
		aID := id()
		now := t0 + 0.2
		viaB := i%75 == 0 // 40 of the 3000
		hop, mid := sharedHopBD, "c"
		if viaB {
			hop, mid = 0.5, "b"
		}
		// a -> mid -> d, mid's span 0.3 ms around a call of 0.3 ms
		cID, sID, dc, ds := id(), id(), id(), id()
		in, out := leg(sharedHopA), leg(sharedHopA)
		sStart := now + in
		dcStart := sStart + 0.2
		dIn, dOut := leg(hop), leg(hop)
		dsStart := dcStart + dIn
		dsEnd := dsStart + 0.3
		dcEnd := dsEnd + dOut
		sEnd := dcEnd + 0.1
		spans = append(spans,
			span{svc: "d", pod: "d-0", id: ds, parent: dc, kind: 2, start: dsStart, end: dsEnd, trace: tr},
			span{svc: mid, pod: mid + "-0", id: dc, parent: sID, kind: 3, start: dcStart, end: dcEnd, trace: tr, peer: "d"},
			span{svc: mid, pod: mid + "-0", id: sID, parent: cID, kind: 2, start: sStart, end: sEnd, trace: tr},
			span{svc: "a", pod: "a-0", id: cID, parent: aID, kind: 3, start: now, end: sEnd + out, trace: tr, peer: mid},
			span{svc: "a", pod: "a-0", id: aID, kind: 2, start: t0, end: sEnd + out + 0.1, trace: tr})
	}
	inf := infer1(t, spans)
	topo := inf.Topology
	b, c, d := topo.Index("b"), topo.Index("c"), topo.Index("d")
	hb, hc := edgeHop(t, topo, b, d), edgeHop(t, topo, c, d)
	want := 2 * (sharedHopBD + sharedJitter)
	if math.Abs(hc-want) > 0.04 {
		t.Errorf("c -> d (3000 calls): hop %.3f ms, want about %.3f", hc, want)
	}
	if hb < want-0.04 || hb > want+0.15 {
		t.Errorf("b -> d (40 calls): hop %.3f ms, want the pooled one, about %.3f (its own would be about 1.2)", hb, want)
	}
}

// TestSingleCallerTopologiesNameNoHopPerCall: a callee whose callers' links are all one speed (here, one caller each) has its
// hop on the service; no call names its own, so the topology file reads as it did before there were hops per edge.
func TestSingleCallerTopologiesNameNoHopPerCall(t *testing.T) {
	out := runWith(fastChain(), 60, 300, 11, simOpts{hopJitter: 0.1})
	inf := infer1(t, out.spans)
	for i, sv := range inf.Topology.Services {
		for _, c := range sv.Calls {
			if c.HasHop {
				t.Errorf("%s: a call to %s names its own hop %v", sv.Name, inf.Topology.Services[c.Child].Name, c.Hop)
			}
		}
		_ = i
	}
	data, err := model.MarshalTopology(inf.Topology)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Services []struct {
			Name     string            `json:"name"`
			Children []json.RawMessage `json:"children"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	for _, sv := range file.Services {
		for _, ch := range sv.Children {
			if len(ch) == 0 || ch[0] != '"' {
				t.Errorf("%s: a child written as %s, want its name alone", sv.Name, ch)
			}
		}
	}
	if len(file.Services) != 3 {
		t.Errorf("%d services in the file, want 3", len(file.Services))
	}
}
