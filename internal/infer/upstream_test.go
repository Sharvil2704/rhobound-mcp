// SPDX-License-Identifier: Apache-2.0

package infer_test

// An entry whose caller is outside the capture (an ingress, a gateway, a
// browser that sends a traceparent but whose spans are not exported here) is
// an entry, not a lost span. The code review's cap-upstream: every root
// server span names a parent that is not in the capture; at 29a7ed9 all 3000
// traces read as broken and the capture was refused.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// sameInference checks that two captures give the same call graph and the same per-service reading.
func sameInference(t *testing.T, what string, got, want *infer.Inferred) {
	t.Helper()
	gt, _ := model.MarshalTopology(got.Topology)
	wt, _ := model.MarshalTopology(want.Topology)
	gs, _ := json.Marshal(got.Stats)
	ws, _ := json.Marshal(want.Stats)
	if string(gt) != string(wt) || string(gs) != string(ws) || got.LamExtPerMs != want.LamExtPerMs || got.Requests != want.Requests {
		t.Errorf("%s: the inference differs from the capture without the upstream parent:\n%s\n%s", what, gt, wt)
	}
}

func TestEntryWithAnUpstreamCaller(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	clean := rootSpans(arr, end, x)
	up := append([]span(nil), clean...)
	for i := range up {
		up[i].parent = x.next() // the ingress's span: never exported
	}
	inf, err := runOpts(t, up, infer.Options{})
	if err != nil {
		t.Fatalf("every entry has an upstream caller: %v", err)
	}
	if inf.BrokenTraces != 0 {
		t.Errorf("%d of %d traces read as broken", inf.BrokenTraces, inf.Traces)
	}
	sameInference(t, "cap-upstream", inf, infer1(t, clean))
}

// chainSpans is fe -> be with CLIENT spans, n traces at 20/s. lose(i) says what trace i loses or gains:
// "upstream" (fe's span names an absent parent), "callee-link" (be's span names an absent parent: the client span
// was lost), "both", "client-top" (fe's server span was lost: its client span is the only span with an absent
// parent), "top-lost" (fe's server and client spans were lost: be's server span is the only span, with an absent
// parent, and be is called by fe elsewhere in the capture), "callee-lost" (an upstream caller, and be's server span
// was lost: the code review's up-lost) or "" (nothing).
func chainSpans(n int, lose func(i int) string) []span {
	rng := rand.New(rand.NewSource(3))
	x := &ids{}
	var spans []span
	t := 0.0
	for i := 0; i < n; i++ {
		t += rng.ExpFloat64() * 1000 / 20
		tr := i + 1
		root, cl := x.next(), x.next()
		how := lose(i)
		fe := span{svc: "fe", pod: "fe-0", id: root, kind: 2, start: t, end: t + 12, trace: tr}
		c := span{svc: "fe", pod: "fe-0", id: cl, parent: root, kind: 3, start: t + 2, end: t + 9, trace: tr, peer: "be", port: 80}
		be := span{svc: "be", pod: "be-0", id: x.next(), parent: cl, kind: 2, start: t + 3, end: t + 8, trace: tr}
		switch how {
		case "upstream":
			fe.parent = x.next()
		case "callee-link":
			be.parent = x.next()
		case "both":
			fe.parent, be.parent = x.next(), x.next()
		case "client-top":
			c.parent = x.next()
		case "callee-lost":
			fe.parent = x.next()
		}
		if how != "client-top" && how != "top-lost" {
			spans = append(spans, fe)
		}
		if how != "top-lost" {
			spans = append(spans, c)
		}
		if how != "callee-lost" {
			spans = append(spans, be)
		}
	}
	return spans
}

// The rule: every span that names an absent parent is a SERVER span of a service most of whose server spans have
// no caller in the capture (or a CONSUMER span of such a consumer), and the trace's top is not in the capture. A
// missing parent below a root, one on a span that is not a SERVER span, one of a service that services in the
// capture call (its caller was lost), or a call with no span of its callee under it is a lost span, as before, and
// above 5% of the traces it is refused.
func TestBrokenTracesWithUpstreamCallers(t *testing.T) {
	clean := infer1(t, chainSpans(2000, func(int) string { return "" }))
	up := infer1(t, chainSpans(2000, func(int) string { return "upstream" }))
	if up.BrokenTraces != 0 {
		t.Errorf("upstream callers: %d broken traces", up.BrokenTraces)
	}
	sameInference(t, "fe -> be with an upstream caller", up, clean)
	for _, how := range []string{"callee-link", "both", "client-top", "top-lost", "callee-lost"} {
		for _, share := range []float64{0.03, 0.06} {
			every := int(1 / share)
			spans := chainSpans(2000, func(i int) string {
				if i%every == 0 {
					return how
				}
				return "upstream"
			})
			want := int64((2000 + every - 1) / every)
			what := fmt.Sprintf("%s on %.0f%% of the traces (the rest with an upstream caller)", how, 100*share)
			inf, err := runOpts(t, spans, infer.Options{})
			switch {
			case share > 0.05:
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d of 2000 traces miss spans", want)) {
					t.Errorf("%s: want the refusal for %d broken traces, got %v", what, want, err)
				}
			case err != nil:
				t.Errorf("%s: %v", what, err)
			case inf.BrokenTraces != want:
				t.Errorf("%s: %d broken traces, want %d", what, inf.BrokenTraces, want)
			}
		}
	}
}

// gatewaySpans: an un-exported gateway sends each request on with a traceparent (the code review's
// gateway and gw_retry captures). fe is cap-base. "two": the gateway calls fe, then be (one worker, exponential 5 ms), in
// the same trace. "retry": it calls fe again 0.5 ms after the first answer on 8% of the requests, in the same trace,
// through the same queue (each attempt's SERVER span names its own gateway span). fe is a root (no parent) instead
// on the traces rooted(i) picks. It returns the spans and the number of fe and be spans.
func gatewaySpans(mode string, rooted func(i int) bool) (spans []span, nfe, nbe int) {
	rng := rand.New(rand.NewSource(12))
	x := &ids{}
	parentOf := func(i int) string {
		if rooted != nil && rooted(i) {
			return ""
		}
		return x.next()
	}
	if mode == "retry" {
		type ev struct {
			a      float64
			i, att int
		}
		var evs []ev
		t := 0.0
		for i := 0; i < 3000; i++ {
			t += rng.ExpFloat64() * 1000 / 55
			evs = append(evs, ev{t, i, 0})
		}
		free := 0.0
		for len(evs) > 0 {
			k := 0
			for j := range evs {
				if evs[j].a < evs[k].a {
					k = j
				}
			}
			e := evs[k]
			evs = append(evs[:k], evs[k+1:]...)
			b := math.Max(e.a, free)
			sv := 10 * rng.ExpFloat64()
			free = b + sv
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: parentOf(e.i), kind: 2, start: e.a, end: b + sv, trace: e.i + 1})
			nfe++
			if e.att == 0 && rng.Float64() < 0.08 {
				evs = append(evs, ev{b + sv + 0.5, e.i, 1})
			}
		}
		return spans, nfe, 0
	}
	arr, _, end := entryQueue(3000, 1)
	beFree := 0.0
	for i := range arr {
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: parentOf(i), kind: 2, start: arr[i], end: end[i], trace: i + 1})
		nfe++
		if mode == "two" {
			a := end[i] + 0.5
			b := math.Max(a, beFree)
			beFree = b + 5*rng.ExpFloat64()
			spans = append(spans, span{svc: "be", pod: "be-0", id: x.next(), parent: x.next(), kind: 2, start: a, end: beFree, trace: i + 1})
			nbe++
		}
	}
	return spans, nfe, nbe
}

// upCalledSpans is the code review's up-called: cap-upstream (fe's SERVER spans name a parent outside the
// capture), and on the visits called(i) picks an in-capture service admin (a root SERVER span) calls fe through a
// CLIENT span instead.
func upCalledSpans(called func(i int) bool) []span {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	var spans []span
	for i := range arr {
		if called(i) {
			root, c := x.next(), x.next()
			spans = append(spans, span{svc: "admin", pod: "admin-0", id: root, kind: 2, start: arr[i] - 1, end: end[i] + 1, trace: i + 1},
				span{svc: "admin", pod: "admin-0", id: c, parent: root, kind: 3, start: arr[i] - 0.5, end: end[i] + 0.5, trace: i + 1,
					peer: "fe", port: 80},
				span{svc: "fe", pod: "fe-0", id: x.next(), parent: c, kind: 2, start: arr[i], end: end[i], trace: i + 1})
			continue
		}
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: x.next(), kind: 2, start: arr[i], end: end[i], trace: i + 1})
	}
	return spans
}

// The code review's upstream shapes, each refused at 5b71d67 as lost spans ("... check
// the exporters' queues"): a gateway that calls fe, then be, in one trace (3000 of 3000 traces broken); gateway
// retries of fe on 8% of the requests (255 of 3000, 8.5%); fe called by an in-capture service admin on 2 traces in
// 3000 (2998 broken) or on 2% (97.9%); a CONSUMER span whose PRODUCER parent is outside the capture, calling fe
// (3000 of 3000, with a message about a span below the trace's top, where the orphan is the top).
func TestUpstreamShapesAreEntries(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	clean := infer1(t, rootSpans(arr, end, &ids{}))

	spans, nfe, nbe := gatewaySpans("two", nil)
	inf, err := runOpts(t, spans, infer.Options{})
	if err != nil {
		t.Fatalf("gw-two: %v", err)
	}
	if inf.BrokenTraces != 0 || inf.Requests != int64(nfe+nbe) {
		t.Errorf("gw-two: %d broken traces, %d entry requests; want 0 and %d", inf.BrokenTraces, inf.Requests, nfe+nbe)
	}
	for _, s := range []string{"fe", "be"} {
		if i := inf.Topology.Index(s); i < 0 || !inf.Stats[i].Entry {
			t.Errorf("gw-two: %s should be an entry", s)
		}
	}
	sameQueue(t, "gw-two", inf, clean)

	spans, nfe, _ = gatewaySpans("retry", nil)
	if inf, err = runOpts(t, spans, infer.Options{}); err != nil {
		t.Fatalf("gw-retry8: %v", err)
	}
	if inf.BrokenTraces != 0 || inf.Requests != int64(nfe) || nfe <= 3100 {
		t.Errorf("gw-retry8: %d broken traces, %d entry requests; want 0 and %d (with %d retries)", inf.BrokenTraces,
			inf.Requests, nfe, nfe-3000)
	}
	if fe := inf.Stats[inf.Topology.Index("fe")]; fe.WorkersPerPod != 1 || !fe.WorkersFromQueueing {
		t.Errorf("gw-retry8: fe %d workers (identified %v), want 1", fe.WorkersPerPod, fe.WorkersFromQueueing)
	}

	for _, k := range []int{2, 60} { // 2 of 3000 (up-called-1) and 2% (up-called)
		every := 3000 / k
		spans := upCalledSpans(func(i int) bool { return i%every == every/2 })
		inf, err := runOpts(t, spans, infer.Options{})
		if err != nil {
			t.Fatalf("fe called by admin on %d of 3000 visits: %v", k, err)
		}
		fe, admin := inf.Topology.Index("fe"), inf.Topology.Index("admin")
		if inf.BrokenTraces != 0 || inf.Requests != 3000 || admin < 0 || inf.Stats[fe].Entries != int64(3000-k) ||
			inf.Stats[admin].Entries != int64(k) {
			t.Errorf("fe called by admin on %d of 3000: %d broken traces, %d entry requests", k, inf.BrokenTraces, inf.Requests)
		}
		sameQueue(t, "up-called", inf, clean)
	}

	// consumer-entry: a CONSUMER span whose PRODUCER parent is outside the capture calls fe
	x := &ids{}
	spans = nil
	for i := range arr {
		co, cl := x.next(), x.next()
		spans = append(spans, span{svc: "worker", pod: "worker-0", id: co, parent: x.next(), kind: 5, start: arr[i] - 0.3, end: end[i] + 0.3, trace: i + 1},
			span{svc: "worker", pod: "worker-0", id: cl, parent: co, kind: 3, start: arr[i] - 0.2, end: end[i] + 0.2, trace: i + 1, peer: "fe", port: 80},
			span{svc: "fe", pod: "fe-0", id: x.next(), parent: cl, kind: 2, start: arr[i], end: end[i], trace: i + 1})
	}
	if inf, err = runOpts(t, spans, infer.Options{}); err != nil {
		t.Fatalf("consumer-entry: %v", err)
	}
	fe := inf.Topology.Index("fe")
	if inf.BrokenTraces != 0 || inf.Requests != 3000 || inf.ConsumerRequests != 3000 || inf.Stats[fe].ConsumerEntries != 3000 {
		t.Errorf("consumer-entry: %d broken traces, %d entry requests (%d from message consumers)", inf.BrokenTraces,
			inf.Requests, inf.ConsumerRequests)
	}
	sameQueue(t, "consumer-entry", inf, clean)
}

// Real loss beside the upstream shapes is still a loss. (1) gw-two, but on some traces fe is a root (no parent):
// the trace's top is in the capture, so be's absent parent there is a lost span, though be is mostly entered from
// outside. (2) fe called by admin on 60% of its visits and from outside on 40%: fe is a service the capture calls,
// so its absent parents are lost callers, and the refusal says so, naming fe and its share.
func TestLostSpansBesideUpstreamEntries(t *testing.T) {
	for _, share := range []float64{0.03, 0.06} {
		every := int(1 / share)
		spans, _, _ := gatewaySpans("two", func(i int) bool { return i%every == 0 })
		want := int64((3000 + every - 1) / every)
		inf, err := runOpts(t, spans, infer.Options{})
		switch {
		case share > 0.05:
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d of 3000 traces miss spans", want)) ||
				!strings.Contains(err.Error(), "below a top that is in it") {
				t.Errorf("fe a root on %.0f%%: want the refusal for %d broken traces below a top, got %v", 100*share, want, err)
			}
		case err != nil:
			t.Errorf("fe a root on %.0f%%: %v", 100*share, err)
		case inf.BrokenTraces != want:
			t.Errorf("fe a root on %.0f%%: %d broken traces, want %d", 100*share, inf.BrokenTraces, want)
		}
	}
	spans := upCalledSpans(func(i int) bool { return i%5 >= 2 })
	_, err := runOpts(t, spans, infer.Options{})
	if err == nil || !strings.Contains(err.Error(), "1200 of 3000 traces miss spans") ||
		!strings.Contains(err.Error(), "fe, called on 60.0% of its 3000 visits") {
		t.Errorf("fe called by admin on 60%%: want the refusal naming fe as a called service, got %v", err)
	}
}
