// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// nestedTrace is one request through gw -> web -> api where web's framework opens a SERVER span inside its HTTP
// instrumentation's, and web's gRPC client opens a CLIENT span around its HTTP client's, as Next.js and .NET do.
func nestedTrace(i int, t0 float64) []span {
	id := func(k int) string { return fmt.Sprintf("%016x", i*100+k) }
	return []span{
		{svc: "gw", pod: "gw-0", id: id(1), kind: 2, start: t0, end: t0 + 10, trace: i + 1},
		{svc: "gw", pod: "gw-0", id: id(2), parent: id(1), kind: 3, start: t0 + 1, end: t0 + 9, trace: i + 1, peer: "web", port: 80},
		{svc: "web", pod: "web-0", id: id(3), parent: id(2), kind: 2, start: t0 + 1.5, end: t0 + 8.5, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(4), parent: id(3), kind: 2, start: t0 + 1.6, end: t0 + 8.4, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(5), parent: id(4), kind: 1, start: t0 + 1.7, end: t0 + 8.3, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(6), parent: id(5), kind: 3, start: t0 + 2, end: t0 + 7, trace: i + 1, peer: "api", port: 9000},
		{svc: "web", pod: "web-0", id: id(7), parent: id(6), kind: 3, start: t0 + 2.1, end: t0 + 6.9, trace: i + 1, peer: "api", port: 9000},
		{svc: "api", pod: "api-0", id: id(8), parent: id(7), kind: 2, start: t0 + 2.5, end: t0 + 6.5, trace: i + 1},
	}
}

// recursionTrace is gw -> web -> web -> api over the network: web's second SERVER span is below a CLIENT span of
// web, so it is a call, not a wrapper.
func recursionTrace(i int, t0 float64) []span {
	id := func(k int) string { return fmt.Sprintf("%016x", i*100+k) }
	return []span{
		{svc: "gw", pod: "gw-0", id: id(1), kind: 2, start: t0, end: t0 + 12, trace: i + 1},
		{svc: "gw", pod: "gw-0", id: id(2), parent: id(1), kind: 3, start: t0 + 1, end: t0 + 11, trace: i + 1, peer: "web", port: 80},
		{svc: "web", pod: "web-0", id: id(3), parent: id(2), kind: 2, start: t0 + 1.5, end: t0 + 10.5, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(4), parent: id(3), kind: 3, start: t0 + 2, end: t0 + 9, trace: i + 1, peer: "web", port: 80},
		{svc: "web", pod: "web-0", id: id(5), parent: id(4), kind: 2, start: t0 + 2.5, end: t0 + 8.5, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(6), parent: id(5), kind: 3, start: t0 + 3, end: t0 + 8, trace: i + 1, peer: "api", port: 9000},
		{svc: "api", pod: "api-0", id: id(7), parent: id(6), kind: 2, start: t0 + 3.5, end: t0 + 7.5, trace: i + 1},
	}
}

func capture(t *testing.T, gen func(i int, t0 float64) []span) *otlp.Data {
	var spans []span
	for i := 0; i < 300; i++ {
		spans = append(spans, gen(i, float64(i)*20)...)
	}
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNestedSpansAreOneVisitAndOneCall(t *testing.T) {
	inf, err := infer.Run(capture(t, nestedTrace), infer.Options{})
	if err != nil {
		t.Fatalf("a request through nested spans should be modelled: %v", err)
	}
	if inf.BrokenTraces != 0 {
		t.Errorf("broken traces: %d, want 0 (the outer CLIENT span is no unmatched call)", inf.BrokenTraces)
	}
	byName := map[string]infer.ServiceStats{}
	for _, s := range inf.Stats {
		byName[s.Name] = s
	}
	for _, name := range []string{"gw", "web", "api"} {
		if got := byName[name].Visits; got != 300 {
			t.Errorf("%s visits: %d, want 300 (one per request)", name, got)
		}
	}
	if _, ok := byName["ext:api"]; ok {
		t.Errorf("the outer CLIENT span of web -> api was read as a second call, to an external api")
	}
	web := inf.Topology.Index("web")
	for _, c := range inf.Topology.Services[web].Calls {
		if c.Child == web {
			t.Errorf("web calls itself")
		}
	}
	if n := len(inf.Topology.Services[web].Calls); n != 1 {
		t.Errorf("web makes %d calls per visit, want 1", n)
	}
	if len(inf.NestedSpans) != 1 || inf.NestedSpans[0] != (infer.NestedMerge{Service: "web", Server: 300, Client: 300, UnscopedServer: 300}) {
		t.Errorf("nested spans: %+v, want web 300 server (none of them with a scope), 300 client", inf.NestedSpans)
	}
	found := false
	for _, w := range inf.Warnings {
		if strings.Contains(w, "web 300 SERVER (the outer span kept)") && strings.Contains(w, "web 300 CLIENT (the span the callee's SERVER span is under kept, else the outermost)") {
			found = true
		}
	}
	if !found {
		t.Errorf("no warning naming the merged spans: %q", inf.Warnings)
	}
	if !warnedWith(inf, "name no instrumentation scope on one or both spans") || !warnedWith(inf, ": web 300; if the service does call itself") {
		t.Errorf("no warning that the SERVER pairs could not be judged by scope: %q", inf.Warnings)
	}
}

// With the scopes named (the HTTP instrumentation's and the framework's), the same capture merges without that notice.
func TestNestedSpansWithScopesNeedNoNotice(t *testing.T) {
	scoped := func(i int, t0 float64) []span {
		sp := nestedTrace(i, t0)
		for k := range sp {
			if sp[k].svc == "web" && sp[k].kind == 2 {
				sp[k].scope = "http"
				if sp[k].id == fmt.Sprintf("%016x", i*100+4) {
					sp[k].scope = "next.js"
				}
			}
		}
		return sp
	}
	inf, err := infer.Run(capture(t, scoped), infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(inf.NestedSpans) != 1 || inf.NestedSpans[0] != (infer.NestedMerge{Service: "web", Server: 300, Client: 300}) {
		t.Errorf("nested spans: %+v, want web 300 server and 300 client, all judged by scope", inf.NestedSpans)
	}
	if warnedWith(inf, "name no instrumentation scope") {
		t.Errorf("a notice that scopes are missing: %q", inf.Warnings)
	}
}

func TestRecursionOverTheNetworkIsNotMerged(t *testing.T) {
	_, err := infer.Run(capture(t, recursionTrace), infer.Options{})
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a service calling itself: web;") {
		t.Fatalf("web calling itself over the network should still be a cycle, naming web: %v", err)
	}
}

// hairpinTrace is gw -> web -> web -> api where web's second SERVER span sits straight below its first (the service sent
// the request to itself with a client that opens no span and passes the trace context on). With the spans from one
// instrumentation scope it is two visits and a cycle; from two scopes it would be a framework's wrapper.
func hairpinTrace(outerScope, innerScope string) func(i int, t0 float64) []span {
	return func(i int, t0 float64) []span {
		id := func(k int) string { return fmt.Sprintf("%016x", i*100+k) }
		return []span{
			{svc: "gw", pod: "gw-0", id: id(1), kind: 2, start: t0, end: t0 + 12, trace: i + 1},
			{svc: "gw", pod: "gw-0", id: id(2), parent: id(1), kind: 3, start: t0 + 1, end: t0 + 11, trace: i + 1, peer: "web", port: 80},
			{svc: "web", pod: "web-0", id: id(3), parent: id(2), kind: 2, start: t0 + 1.5, end: t0 + 10.5, trace: i + 1, scope: outerScope},
			{svc: "web", pod: "web-0", id: id(4), parent: id(3), kind: 2, start: t0 + 2.5, end: t0 + 9.5, trace: i + 1, scope: innerScope},
			{svc: "web", pod: "web-0", id: id(5), parent: id(4), kind: 3, start: t0 + 3, end: t0 + 8, trace: i + 1, peer: "api", port: 9000},
			{svc: "api", pod: "api-0", id: id(6), parent: id(5), kind: 2, start: t0 + 3.5, end: t0 + 7.5, trace: i + 1},
		}
	}
}

func TestServerSpansOfOneScopeAreACallToItself(t *testing.T) {
	_, err := infer.Run(capture(t, hairpinTrace("http", "http")), infer.Options{})
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a service calling itself: web;") ||
		!strings.Contains(err.Error(), "instrumentation scope") {
		t.Fatalf("two SERVER spans of one scope inside one another should be a cycle through web, with the scope named: %v", err)
	}
	// ... and from two scopes they are one visit
	inf, err := infer.Run(capture(t, hairpinTrace("http", "next.js")), infer.Options{})
	if err != nil {
		t.Fatalf("a framework's SERVER span inside the instrumentation's should be merged: %v", err)
	}
	if got := inf.Stats[inf.Topology.Index("web")].Visits; got != 300 {
		t.Errorf("web visits %d, want 300", got)
	}
	// ... and with no scope in the capture, the spans are judged on their times alone, as before
	if _, err := infer.Run(capture(t, hairpinTrace("", "")), infer.Options{}); err != nil {
		t.Fatalf("spans that name no scope should be merged: %v", err)
	}
}

// externalNest is gw -> web -> redis, where the cache client opens a span for the operation (6 ms, "GET") and, inside it,
// one for the connection it opens (0.5 ms): neither has a SERVER span under it, as redis sends none.
func externalNest(i int, t0 float64) []span {
	id := func(k int) string { return fmt.Sprintf("%016x", i*100+k) }
	return []span{
		{svc: "gw", pod: "gw-0", id: id(1), kind: 2, start: t0, end: t0 + 14, trace: i + 1},
		{svc: "gw", pod: "gw-0", id: id(2), parent: id(1), kind: 3, start: t0 + 0.5, end: t0 + 13.5, trace: i + 1, peer: "web", port: 80},
		{svc: "web", pod: "web-0", id: id(3), parent: id(2), kind: 2, start: t0 + 1, end: t0 + 13, trace: i + 1},
		{svc: "web", pod: "web-0", id: id(4), parent: id(3), kind: 3, start: t0 + 2, end: t0 + 8, trace: i + 1, peer: "redis", port: 6379, name: "GET"},
		{svc: "web", pod: "web-0", id: id(5), parent: id(4), kind: 3, start: t0 + 2.1, end: t0 + 2.6, trace: i + 1, peer: "redis", port: 6379, name: "connect"},
	}
}

func TestNestedClientSpansNobodyServesAreOneOperation(t *testing.T) {
	inf, err := infer.Run(capture(t, externalNest), infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	i := inf.Topology.Index("ext:redis")
	if i < 0 {
		t.Fatalf("no external redis in %v", inf.Topology.Services)
	}
	if got := inf.Topology.Services[i].S; got < 5.9 || got > 6.1 {
		t.Errorf("the cache call takes %.2f ms in the model, want 6 (the operation, not the connection)", got)
	}
	web := inf.Topology.Index("web")
	if n := len(inf.Topology.Services[web].Calls); n != 1 {
		t.Errorf("web makes %d calls per visit, want 1", n)
	}
	if len(inf.NestedSpans) != 1 || inf.NestedSpans[0] != (infer.NestedMerge{Service: "web", Client: 300}) {
		t.Errorf("nested spans: %+v, want web 300 client", inf.NestedSpans)
	}
}
