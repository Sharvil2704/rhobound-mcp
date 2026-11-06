// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// ns builds a span for the merge tests. res 0 and 1 are two pods of one service, 2 another service.
type nsp struct {
	id, parent uint64
	kind       uint8
	res        int32
	start, end uint64 // microseconds
	peer       int32
	scope      int32 // 0: names none
}

func build(spans ...nsp) []otlp.Span {
	g := make([]otlp.Span, len(spans))
	for i, s := range spans {
		g[i] = otlp.Span{Trace: [2]uint64{1, 1}, ID: s.id, Parent: s.parent, Start: s.start * 1000, End: s.end * 1000,
			Res: s.res, Peer: s.peer, Addr: s.peer, Scope: s.scope, Kind: s.kind}
	}
	return g
}

const (
	srv, cli, int_, prod, cons = otlp.KindServer, otlp.KindClient, otlp.KindInternal, otlp.KindProducer, otlp.KindConsumer
)

// merged runs mergeNested on spans with res 0 and 1 in the same service but different pods, res 2 another service.
func merged(spans ...nsp) ([]otlp.Span, nestedCount) {
	g := build(spans...)
	resPod := []int32{0, 1, 2, 3}  // pod identity per resource
	resName := []int32{0, 0, 1, 0} // res 3: the same service and pod as res 0 (another Resource value)
	resPod[3] = 0
	cnt := make([]nestedCount, 2)
	mergeNested(g, spanIndex(g), resPod, resName, cnt)
	return g, cnt[0]
}

func kinds(g []otlp.Span) []uint8 {
	out := make([]uint8, len(g))
	for i := range g {
		out[i] = g[i].Kind
	}
	return out
}

func wantKinds(t *testing.T, what string, g []otlp.Span, want ...uint8) {
	t.Helper()
	got := kinds(g)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%s: kinds %v, want %v", what, got, want)
			return
		}
	}
}

func TestNestedServerInServer(t *testing.T) {
	// a gateway's CLIENT (res 2), then web's http SERVER, an INTERNAL, web's framework SERVER (same pod), its INTERNAL
	g, c := merged(
		nsp{id: 1, kind: cli, res: 2, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: srv, res: 0, start: 10, end: 90, peer: -1},
		nsp{id: 3, parent: 2, kind: int_, res: 0, start: 11, end: 89, peer: -1},
		nsp{id: 4, parent: 3, kind: srv, res: 0, start: 12, end: 88, peer: -1},
		nsp{id: 5, parent: 4, kind: int_, res: 0, start: 13, end: 87, peer: -1},
	)
	wantKinds(t, "server in server", g, cli, srv, int_, int_, int_)
	if c.server != 1 || c.client != 0 {
		t.Errorf("counts %+v, want 1 server", c)
	}
}

func TestNestedChainOfThree(t *testing.T) {
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: srv, res: 0, start: 1, end: 99, peer: -1},
		nsp{id: 3, parent: 2, kind: srv, res: 3, start: 2, end: 98, peer: -1}, // another Resource value, same service and pod
	)
	wantKinds(t, "chain of three servers", g, srv, int_, int_)
	if c.server != 2 {
		t.Errorf("counts %+v, want 2 servers", c)
	}
}

func TestNestedNotRecursion(t *testing.T) {
	// SERVER -> CLIENT -> SERVER of one service and pod: a call to itself over the network. Nothing is merged.
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 0},
		nsp{id: 3, parent: 2, kind: srv, res: 0, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "recursion in one pod", g, srv, cli, srv)
	if c != (nestedCount{}) {
		t.Errorf("counts %+v, want none", c)
	}
	// ... the same through INTERNAL spans, and with the second SERVER on another pod
	g, c = merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: int_, res: 0, start: 5, end: 95, peer: -1},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 10, end: 90, peer: 0},
		nsp{id: 4, parent: 3, kind: srv, res: 1, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "recursion to another pod", g, srv, int_, cli, srv)
	if c != (nestedCount{}) {
		t.Errorf("counts %+v, want none", c)
	}
}

func TestNestedOtherPodOrService(t *testing.T) {
	// a SERVER span of another pod of the same service directly below one (no CLIENT span: the caller is not
	// instrumented) is a call to a replica, not a wrapper; so is a SERVER span of another service.
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: srv, res: 1, start: 10, end: 90, peer: -1},
		nsp{id: 3, parent: 1, kind: srv, res: 2, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "other pod, other service", g, srv, srv, srv)
	if c != (nestedCount{}) {
		t.Errorf("counts %+v, want none", c)
	}
}

func TestNestedClientInClient(t *testing.T) {
	// a gRPC client's CLIENT span (peer 5) around the HTTP client's (no peer): the outer goes, the inner takes the peer
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 88, peer: -1},
		nsp{id: 4, parent: 3, kind: srv, res: 2, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "client in client", g, srv, int_, cli, srv)
	if c.client != 1 || c.server != 0 {
		t.Errorf("counts %+v, want 1 client", c)
	}
	if g[2].Peer != 5 || g[2].Addr != 5 {
		t.Errorf("the inner CLIENT span should take the outer's peer: got peer %d addr %d", g[2].Peer, g[2].Addr)
	}
	// nothing in the nest has a SERVER span under it: the outermost is the operation the caller waited for and keeps its
	// own peer, the inner (another peer: a different call, as far as the data shows) is part of it
	g, c = merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 88, peer: 7},
	)
	wantKinds(t, "unserved client in client", g, srv, cli, int_)
	if g[1].Peer != 5 || c.client != 1 {
		t.Errorf("the outermost keeps its own peer: got %d, want 5 (counts %+v)", g[1].Peer, c)
	}
}

func TestNestedClientRetries(t *testing.T) {
	// one wrapper around two attempts, the second of which reached a callee that sent spans: that attempt is the call and
	// the wrapper is not. The first attempt (it failed on the wire, or its callee sent none) is a call of its own: it takes
	// the wrapper's peer
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 40, peer: -1},
		nsp{id: 4, parent: 2, kind: cli, res: 0, start: 50, end: 88, peer: -1},
		nsp{id: 5, parent: 4, kind: srv, res: 2, start: 55, end: 85, peer: -1},
	)
	wantKinds(t, "retries, the second served", g, srv, int_, cli, cli, srv)
	if c.client != 1 {
		t.Errorf("counts %+v, want 1 client", c)
	}
	if g[2].Peer != 5 || g[3].Peer != 5 {
		t.Errorf("both attempts should take the wrapper's peer: got %d, %d", g[2].Peer, g[3].Peer)
	}
	// the same with no attempt served (a database, a service that sends no spans): the wrapper is the one call
	g, c = merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 40, peer: -1},
		nsp{id: 4, parent: 2, kind: cli, res: 0, start: 50, end: 88, peer: -1},
	)
	wantKinds(t, "retries, none served", g, srv, cli, int_, int_)
	if c.client != 2 {
		t.Errorf("counts %+v, want 2 clients", c)
	}
}

func TestNestedClientChainTakesNearestPeer(t *testing.T) {
	// a chain nobody serves: the outermost is kept, with its own peer
	g, _ := merged(
		nsp{id: 1, kind: cli, res: 0, start: 0, end: 100, peer: 5},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 1, end: 99, peer: -1},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 2, end: 98, peer: -1},
	)
	wantKinds(t, "chain of three clients, none served", g, cli, int_, int_)
	if g[0].Peer != 5 {
		t.Errorf("the outermost keeps its peer: got %d, want 5", g[0].Peer)
	}
	// ... which takes the nearest peer of the nest when it has none
	g, _ = merged(
		nsp{id: 1, kind: cli, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 1, end: 99, peer: -1},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 2, end: 98, peer: 6},
		nsp{id: 4, parent: 2, kind: cli, res: 0, start: 3, end: 97, peer: 7},
	)
	wantKinds(t, "peer of the nest", g, cli, int_, int_, int_)
	if g[0].Peer != 6 || g[0].Addr != 6 {
		t.Errorf("the outermost takes the peer of the nearest span that has one (the first of two): got %d, want 6", g[0].Peer)
	}
	// a chain served at the innermost: the served span is the call, and takes the nearest wrapper's peer
	g, _ = merged(
		nsp{id: 1, kind: cli, res: 0, start: 0, end: 100, peer: 5},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 1, end: 99, peer: -1},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 2, end: 98, peer: -1},
		nsp{id: 4, parent: 3, kind: srv, res: 2, start: 10, end: 90, peer: -1},
	)
	wantKinds(t, "chain of three clients, the innermost served", g, int_, int_, cli, srv)
	if g[2].Peer != 5 {
		t.Errorf("the innermost takes the peer of the nearest wrapper that has one: got %d, want 5", g[2].Peer)
	}
}

// Two SERVER spans of one named instrumentation scope, one inside the other in one pod, with no CLIENT span between, are a
// request the service sent to itself (an uninstrumented client that passes the trace context on), not a framework's
// wrapper: they are two visits. A framework's span comes from another scope; a span that names none is judged on the rest.
func TestNestedServerScope(t *testing.T) {
	two := func(outer, inner int32) []uint8 {
		g, _ := merged(
			nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1, scope: outer},
			nsp{id: 2, parent: 1, kind: srv, res: 0, start: 10, end: 90, peer: -1, scope: inner},
		)
		return kinds(g)
	}
	for _, c := range []struct {
		name         string
		outer, inner int32
		merged       bool
	}{
		{"scopes differ (http, next.js)", 1, 2, true},
		{"one scope", 1, 1, false},
		{"the outer names none", 0, 2, true},
		{"the inner names none", 1, 0, true},
		{"neither names one", 0, 0, true},
	} {
		got := two(c.outer, c.inner)
		if (got[1] == int_) != c.merged {
			t.Errorf("%s: kinds %v, merged want %v", c.name, got, c.merged)
		}
	}
	// a chain of three: the pair of one scope stays apart, the other pair is merged
	g, _ := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1, scope: 1},
		nsp{id: 2, parent: 1, kind: srv, res: 0, start: 5, end: 95, peer: -1, scope: 2},
		nsp{id: 3, parent: 2, kind: srv, res: 0, start: 10, end: 90, peer: -1, scope: 2},
	)
	wantKinds(t, "http, next.js, next.js", g, srv, int_, srv)
}

// Nested CLIENT spans are merged by scope-blind rules: the .NET gRPC client's span (GrpcNetClient) wraps the HTTP client's
// (System.Net.Http), and two spans of one scope (an SDK's wrapper and its attempts) are handled the same.
func TestNestedClientIgnoresScope(t *testing.T) {
	for _, sc := range [][2]int32{{1, 2}, {1, 1}, {0, 0}} {
		g, _ := merged(
			nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
			nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5, scope: sc[0]},
			nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 88, peer: -1, scope: sc[1]},
			nsp{id: 4, parent: 3, kind: srv, res: 2, start: 20, end: 80, peer: -1},
		)
		wantKinds(t, fmt.Sprintf("scopes %v", sc), g, srv, int_, cli, srv)
	}
}

// A chain of more nested spans than maxNestedDepth is left alone, whole, and costs time in proportion to its length
// (the walk up each span's chain stops at the limit). One at the limit is merged.
func TestNestedDeepChains(t *testing.T) {
	chainOf := func(n int, kind uint8) []nsp {
		spans := []nsp{{id: 1, kind: srv, res: 2, start: 0, end: 1_000_000, peer: -1}} // another service's: the walk ends there
		parent := uint64(1)
		for i := 0; i < n; i++ {
			id := uint64(i + 2)
			spans = append(spans, nsp{id: id, parent: parent, kind: kind, res: 0, start: uint64(1 + i), end: uint64(900_000 - i), peer: 3})
			parent = id
		}
		return spans
	}
	for _, kind := range []uint8{srv, cli} {
		// maxNestedDepth + 1 spans: the innermost has maxNestedDepth above it
		g, c := merged(chainOf(maxNestedDepth+1, kind)...)
		n := int64(len(g) - 2) // all but the other service's span and the outermost
		if got := c.server + c.client; got != n {
			t.Errorf("kind %d: a chain of %d merged %d spans, want %d", kind, maxNestedDepth+1, got, n)
		}
		g, c = merged(chainOf(maxNestedDepth+2, kind)...)
		if got := c.server + c.client; got == 0 || got >= int64(len(g)-2) {
			t.Errorf("kind %d: a chain of %d merged %d spans, want the shorter chains inside it only", kind, maxNestedDepth+2, got)
		}
	}
	// thousands deep: it finishes at once
	done := make(chan struct{})
	go func() {
		merged(chainOf(20000, srv)...)
		merged(chainOf(20000, cli)...)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("mergeNested on a chain of 20000 nested spans took over 20 s")
	}
}

func TestNestedTimeTest(t *testing.T) {
	const ms = 1000
	mk := func(innerStart, innerEnd uint64) []uint8 {
		g, _ := merged(
			nsp{id: 1, kind: srv, res: 0, start: 10 * ms, end: 20 * ms, peer: -1},
			nsp{id: 2, parent: 1, kind: srv, res: 0, start: innerStart, end: innerEnd, peer: -1},
		)
		return kinds(g)
	}
	for _, c := range []struct {
		name         string
		start, end   uint64
		merged       bool
		quantization string
	}{
		{"contained", 11 * ms, 19 * ms, true, ""},
		{"inner ends 0.9 ms after the outer (whole-millisecond start times)", 11 * ms, 20*ms + 900, true, ""},
		{"inner starts 0.9 ms before the outer", 10*ms - 900, 15 * ms, true, ""},
		{"inner ends 1.5 ms after the outer", 11 * ms, 20*ms + 1500, true, ""},
		{"inner starts 1.5 ms before the outer", 10*ms - 1500, 15 * ms, true, ""},
		{"inner ends 2.5 ms after the outer", 11 * ms, 20*ms + 2500, false, ""},
		{"inner starts 2.5 ms before the outer", 10*ms - 2500, 15 * ms, false, ""},
		{"inner starts after the outer has ended", 25 * ms, 26 * ms, false, ""},
	} {
		got := mk(c.start, c.end)
		if (got[1] == int_) != c.merged {
			t.Errorf("%s: kinds %v, merged want %v", c.name, got, c.merged)
		}
	}
}

func TestNestedOnlyServerAndClient(t *testing.T) {
	// a consumer's "receive" and "process", and two PRODUCER spans, are not wrappers
	g, c := merged(
		nsp{id: 1, kind: cons, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cons, res: 0, start: 90, end: 100, peer: -1},
		nsp{id: 3, kind: srv, res: 0, start: 200, end: 300, peer: -1},
		nsp{id: 4, parent: 3, kind: prod, res: 0, start: 210, end: 290, peer: -1},
		nsp{id: 5, parent: 4, kind: prod, res: 0, start: 220, end: 280, peer: -1},
	)
	wantKinds(t, "consumer and producer pairs", g, cons, cons, srv, prod, prod)
	if c != (nestedCount{}) {
		t.Errorf("counts %+v, want none", c)
	}
	// a SERVER below a CONSUMER below a SERVER of the same service and pod: the consumer ends the walk
	g, c = merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cons, res: 0, start: 10, end: 90, peer: -1},
		nsp{id: 3, parent: 2, kind: srv, res: 0, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "server under a consumer", g, srv, cons, srv)
}

func TestNestedWarning(t *testing.T) {
	if got := nestedSummary([]string{"a", "b"}, []nestedCount{{}, {}}); got != nil || nestedWarning(got) != "" {
		t.Errorf("no merges, no warning: %v %q", got, nestedWarning(got))
	}
	sum := nestedSummary([]string{"b", "a"}, []nestedCount{{client: 2}, {server: 3}})
	if len(sum) != 2 || sum[0] != (NestedMerge{Service: "a", Server: 3}) || sum[1] != (NestedMerge{Service: "b", Client: 2}) {
		t.Errorf("summary (by service name): %+v", sum)
	}
	w := nestedWarning(sum)
	if !strings.Contains(w, "a 3 SERVER") || !strings.Contains(w, "b 2 CLIENT") || strings.Contains(w, "scope") {
		t.Errorf("warning: %q", w)
	}
	sum = nestedSummary([]string{"a"}, []nestedCount{{server: 3, unscoped: 2}})
	if w = nestedWarning(sum); !strings.Contains(w, "no instrumentation scope") || !strings.Contains(w, ": a 2; if the service does call itself") {
		t.Errorf("warning with unscoped pairs: %q", w)
	}
}

func TestNestedParentCycleIsLeftAlone(t *testing.T) {
	// a span that is its own parent, and two that are each other's: malformed ids, not nesting. No merge, no hang.
	g, c := merged(nsp{id: 1, parent: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1})
	wantKinds(t, "self parent", g, srv)
	if c != (nestedCount{}) {
		t.Errorf("counts %+v, want none", c)
	}
	g, c = merged(
		nsp{id: 1, parent: 2, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: srv, res: 0, start: 1, end: 99, peer: -1},
	)
	wantKinds(t, "two-cycle", g, srv, srv)
	g, c = merged(
		nsp{id: 1, parent: 2, kind: cli, res: 0, start: 0, end: 100, peer: 5},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 1, end: 99, peer: -1},
	)
	wantKinds(t, "two-cycle clients", g, cli, cli)
	if g[1].Peer != -1 {
		t.Errorf("a client in a parent cycle took a peer: %d", g[1].Peer)
	}
}

func TestNestedClientServedWrapperKept(t *testing.T) {
	// the callee's SERVER span hangs under the wrapper and the inner CLIENT span has none: the wrapper is the call
	g, c := merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 88, peer: 5},
		nsp{id: 4, parent: 2, kind: srv, res: 2, start: 20, end: 80, peer: -1},
	)
	wantKinds(t, "served wrapper", g, srv, cli, int_, srv)
	if c.client != 1 {
		t.Errorf("counts %+v, want 1 client", c)
	}
	// a SERVER under each of two nested CLIENT spans: two calls, both kept
	g, _ = merged(
		nsp{id: 1, kind: srv, res: 0, start: 0, end: 100, peer: -1},
		nsp{id: 2, parent: 1, kind: cli, res: 0, start: 10, end: 90, peer: 5},
		nsp{id: 3, parent: 2, kind: cli, res: 0, start: 12, end: 88, peer: 5},
		nsp{id: 4, parent: 2, kind: srv, res: 2, start: 20, end: 40, peer: -1},
		nsp{id: 5, parent: 3, kind: srv, res: 2, start: 50, end: 80, peer: -1},
	)
	wantKinds(t, "both served", g, srv, cli, cli, srv, srv)
}
