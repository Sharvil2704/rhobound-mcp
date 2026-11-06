// SPDX-License-Identifier: Apache-2.0

package infer_test

// A capture that lost spans: the refusal and the warnings name the edge (the caller, the address, the service that
// answers on it) and the share of its calls, the rows of the callee are marked, and an entry service that other
// services also call is warned about. What counts as lost is unchanged (hand-made spans below, and the real fixture).

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// traceCount is the number of traces the spans are in.
func traceCount(spans []span) int {
	m := map[int]bool{}
	for _, sp := range spans {
		m[sp.trace] = true
	}
	return len(m)
}

// warningWith returns the first warning that holds every part, or "".
func warningWith(inf *infer.Inferred, parts ...string) string {
	for _, w := range inf.Warnings {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(w, p)
		}
		if ok {
			return w
		}
	}
	return ""
}

func shareText(lost, total int) string {
	return fmt.Sprintf("%d of %d calls (%.1f%%)", lost, total, 100*float64(lost)/float64(total))
}

// A call that lost its callee's span, below 5% of the traces: the capture is used, the warning names the edge,
// the share of its calls and what the model did, and c (the callee) is marked: its rate misses those visits.
func TestLostCalleeBelowTheLimitIsNamed(t *testing.T) {
	out := run(chain(), 50, 200, 7, false)
	full := infer1(t, out.spans)
	spans, lost := dropSpans(out.spans, "c", 50, false) // 2% of the traces
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	total := traceCount(spans)
	w := warningWith(inf, "c: "+fmt.Sprint(lost)+" calls to it lost its span", "b -> c: "+shareText(lost, total),
		"the service time, the queue and the worker count of c are read from the visits whose spans were captured")
	if w == "" {
		t.Fatalf("no warning names the edge b -> c with %s: %q", shareText(lost, total), inf.Warnings)
	}
	ci, fi := inf.Topology.Index("c"), full.Topology.Index("c")
	if inf.Stats[ci].LostVisits != int64(lost) {
		t.Errorf("c: %d lost visits, want %d", inf.Stats[ci].LostVisits, lost)
	}
	within(t, "c: lost share", inf.Stats[ci].LostShare, float64(lost)/float64(full.Stats[fi].Visits), 1e-9)
	for _, s := range []string{"a", "b"} {
		if inf.Stats[inf.Topology.Index(s)].LostVisits != 0 {
			t.Errorf("%s is marked as having lost visits", s)
		}
	}
	if !strings.Contains(w, fmt.Sprintf("(%.1f%% of its visits)", 100*inf.Stats[ci].LostShare)) {
		t.Errorf("the warning's share differs from the row's %.4f: %s", inf.Stats[ci].LostShare, w)
	}
	// the capture without losses says nothing of the kind
	if warningWith(full, "lost its span") != "" || full.Stats[fi].LostVisits != 0 {
		t.Errorf("the complete capture reports lost spans: %q", full.Warnings)
	}
}

// Above 5% it is refused, and the refusal names the edge and its share of calls.
func TestLostCalleeRefusalNamesTheEdge(t *testing.T) {
	out := run(chain(), 50, 200, 7, false)
	spans, lost := dropSpans(out.spans, "c", 12, false) // 8%
	_, err := inferErr(t, spans)
	want := "have a call to a service with no span of that service under it (b -> c: " + shareText(lost, traceCount(spans)) + ")"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("want a refusal with %q, got %v", want, err)
	}
	// a call to an address that is a ClusterIP: the refusal says which service answers on it
	var ip []span
	for _, sp := range spans {
		if sp.kind == 3 && sp.peer == "c" {
			sp.peer, sp.port = "192.0.2.77", 8080
		}
		ip = append(ip, sp)
	}
	_, err = inferErr(t, ip)
	want = "b -> 192.0.2.77:8080 (answered by c): " + shareText(lost, traceCount(spans))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("want a refusal with %q, got %v", want, err)
	}
}

// Several edges: the most calls first, the same text every run.
func TestLostEdgesAreListedMostFirst(t *testing.T) {
	b := &node{name: "b", c: 2, pods: 1, mean: 10}
	c := &node{name: "c", c: 2, pods: 1, mean: 10}
	out := run(&node{name: "a", c: 4, pods: 1, mean: 5, children: []*node{b, c}}, 40, 200, 11, false)
	spans, nb := dropSpans(out.spans, "b", 12, false)
	spans, nc := dropSpans(spans, "c", 8, false)
	total := traceCount(spans)
	var first string
	for i := 0; i < 2; i++ {
		_, err := inferErr(t, spans)
		if err == nil {
			t.Fatal("two edges losing 8% and 12% of their calls were not refused")
		}
		msg := err.Error()
		ib, ic := strings.Index(msg, "a -> b: "+shareText(nb, total)), strings.Index(msg, "a -> c: "+shareText(nc, total))
		if ib < 0 || ic < 0 || ic > ib {
			t.Errorf("want a -> c (%d calls) before a -> b (%d calls) in %v", nc, nb, err)
		}
		if i == 0 {
			first = msg
		} else if msg != first {
			t.Errorf("the refusal differs between two runs:\n%s\n%s", first, msg)
		}
	}
}

// A real hole in the middle of a trace: b's server span is gone, its client span to c names it as its parent, and
// a's call to b has nobody under it. Both are named, below the limit as warnings and above it in the refusal.
func TestMidTraceHoleIsNamed(t *testing.T) {
	out := run(chain(), 50, 200, 7, false)
	spans, lost := dropSpans(out.spans, "b", 50, false)
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	if inf.BrokenTraces != int64(lost) {
		t.Errorf("%d broken traces, want %d", inf.BrokenTraces, lost)
	}
	if warningWith(inf, "b: "+fmt.Sprint(lost)+" calls to it lost its span", "a -> b: "+shareText(lost, traceCount(spans))) == "" {
		t.Errorf("no warning for the call a made to b: %q", inf.Warnings)
	}
	if warningWith(inf, fmt.Sprintf("%d traces hold spans whose parent is not in the capture below a top", lost), "b CLIENT "+fmt.Sprint(lost)) == "" {
		t.Errorf("no warning for b's orphan client spans: %q", inf.Warnings)
	}
	spans, lost = dropSpans(out.spans, "b", 12, false)
	_, err = inferErr(t, spans)
	if err == nil || !strings.Contains(err.Error(), "below a top that is in it") || !strings.Contains(err.Error(), "(the spans, by service and kind: b CLIENT "+fmt.Sprint(lost)+")") ||
		!strings.Contains(err.Error(), "a -> b: "+shareText(lost, traceCount(spans))) {
		t.Errorf("want the refusal for a hole below a top, naming b's client spans and the edge a -> b, got %v", err)
	}
}

// A service that is called by one in the capture and is entered from outside on the rest: orphan callee spans
// (the Demo's flagd: the cart calls it, and its other lookups come from a client that sends no context). They are
// entries, the load on the service is right, and the warning says that its senders are not in the model. Upstream
// shapes are not changed: nothing is broken or refused.
func TestEntryThatIsAlsoCalledIsWarnedAbout(t *testing.T) {
	for _, roots := range []bool{false, true} { // the outside requests name an absent parent, or are roots
		spans := upCalledSpans(func(i int) bool { return i%10 == 5 }) // fe called by admin on 10% of its visits
		if roots {
			for i := range spans {
				if spans[i].svc == "fe" && spans[i].parent != "" && !hasSpan(spans, spans[i].parent) {
					spans[i].parent = ""
				}
			}
		}
		inf, err := inferErr(t, spans)
		if err != nil {
			t.Fatal(err)
		}
		if inf.BrokenTraces != 0 || inf.Requests != 3000 {
			t.Errorf("roots %v: %d broken traces, %d requests; want 0 and 3000", roots, inf.BrokenTraces, inf.Requests)
		}
		fe := inf.Stats[inf.Topology.Index("fe")]
		if !fe.Entry || fe.Entries != 2700 {
			t.Errorf("roots %v: fe entry %v with %d entries, want an entry with 2700", roots, fe.Entry, fe.Entries)
		}
		if w := warningWith(inf, "fe is entered from outside the capture and is also called by admin", "90.0% of its 3000 visits",
			"10.0% are called by admin", "a change of fe's time reaches admin and not those senders"); w == "" {
			t.Errorf("roots %v: no warning for fe: %q", roots, inf.Warnings)
		}
	}
	// a few calls from an admin tool (2% of the visits) are not what the warning is about
	spans := upCalledSpans(func(i int) bool { return i%50 == 25 })
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	if warningWith(inf, "is entered from outside the capture and is also called by") != "" {
		t.Errorf("a warning for 2%% of the visits: %q", inf.Warnings)
	}
	// a chain, where only the entry is entered from outside, gets none
	if chainInf := infer1(t, run(chain(), 50, 200, 7, false).spans); warningWith(chainInf, "is entered from outside the capture and is also called by") != "" {
		t.Errorf("a warning on a plain chain: %q", chainInf.Warnings)
	}
}

func hasSpan(spans []span, id string) bool {
	for _, sp := range spans {
		if sp.id == id {
			return true
		}
	}
	return false
}

// dropServerSpans removes the SERVER spans of a service from every nth of the traces that hold one (in trace id
// order); it returns how many traces lost spans.
func dropServerSpans(d *otlp.Data, service string, every int) int {
	var ids [][2]uint64
	seen := map[[2]uint64]bool{}
	for _, sp := range d.Spans {
		if sp.Kind == otlp.KindServer && d.Resources[sp.Res].Service == service && !seen[sp.Trace] {
			seen[sp.Trace] = true
			ids = append(ids, sp.Trace)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i][0] < ids[j][0] || (ids[i][0] == ids[j][0] && ids[i][1] < ids[j][1])
	})
	gone := map[[2]uint64]bool{}
	for i, id := range ids {
		if i%every == every-1 {
			gone[id] = true
		}
	}
	out := d.Spans[:0]
	for _, sp := range d.Spans {
		if sp.Kind == otlp.KindServer && d.Resources[sp.Res].Service == service && gone[sp.Trace] {
			continue
		}
		out = append(out, sp)
	}
	d.Spans = out
	return len(gone)
}

// Real traces of the OpenTelemetry Demo (testdata/counts/shipped-20s): the capture as it is has no lost spans, and
// the flagd lookups of the cart under a gRPC client's wrapper span are not read as lost callees. With the
// product-catalog's SERVER spans taken out of some traces, the front end's calls (product-catalog:8080) and the
// checkout's (a ClusterIP) are named, the ClusterIP with the service that answers on it.
func TestLostCalleeOnRealTraces(t *testing.T) {
	read := func(every int) (*infer.Inferred, error, int) {
		d, err := otlp.ReadDir("testdata/counts/shipped-20s")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		if every > 0 {
			n = dropServerSpans(d, "product-catalog", every)
		}
		inf, err := infer.Run(d, infer.Options{})
		return inf, err, n
	}
	inf, err, _ := read(0)
	if err != nil {
		t.Fatal(err)
	}
	if inf.BrokenTraces != 0 || warningWith(inf, "lost its span") != "" {
		t.Fatalf("the capture as it is: %d broken traces, %q", inf.BrokenTraces, inf.Warnings)
	}
	for _, s := range inf.Stats {
		if s.LostVisits != 0 || s.BrokenVisitShare != 0 {
			t.Errorf("%s marked as having lost visits", s.Name)
		}
	}
	// flagd is the orphan callee of the Demo: the load generator's lookups (a CLIENT span with no visit of its own) and email's
	// (root spans, no context passed on) are entries, the cart's are calls
	if w := warningWith(inf, "flagd is entered from outside the capture and is also called by cart", "79.4% of its 97 visits",
		"20.6% are called by cart"); w == "" {
		t.Errorf("no warning for flagd: %q", inf.Warnings)
	}
	entryCalled := 0
	for _, w := range inf.Warnings {
		if strings.Contains(w, "is entered from outside the capture and is also called by") {
			entryCalled++
		}
	}
	if entryCalled != 1 {
		t.Errorf("%d warnings of an entry that is also called, want 1 (flagd)", entryCalled)
	}
	inf, err, n := read(25) // 11 of 279 traces: 2.3% of the 478
	if err != nil {
		t.Fatal(err)
	}
	if inf.BrokenTraces != int64(n) {
		t.Errorf("%d broken traces, want %d", inf.BrokenTraces, n)
	}
	if warningWith(inf, "frontend -> product-catalog:8080: ", "calls to it lost its span", "the service time, the queue and the worker count of product-catalog are read") == "" {
		t.Errorf("no warning for frontend -> product-catalog: %q", inf.Warnings)
	}
	pc := inf.Stats[inf.Topology.Index("product-catalog")]
	if pc.LostVisits < int64(n) || pc.LostShare <= 0 || pc.LostShare > 0.2 {
		t.Errorf("product-catalog: %d lost visits (share %.3f) for %d traces", pc.LostVisits, pc.LostShare, n)
	}
	_, err, n = read(6) // 46 traces: 9.6%
	if err == nil || !strings.Contains(err.Error(), "frontend -> product-catalog:8080: ") || !strings.Contains(err.Error(), "traces miss spans") {
		t.Errorf("%d traces without product-catalog spans: want a refusal naming frontend -> product-catalog:8080, got %v", n, err)
	}
	if err != nil && !strings.Contains(err.Error(), "-> 192.0.2.8:8080 (answered by product-catalog)") {
		t.Errorf("the checkout's calls (a ClusterIP) should name the service: %v", err)
	}
}

// Broken traces that are all of one kind of request: d is called on 3% of the requests and its callee e's span is
// lost on four of five of those. That is 2.4% of the traces, below the limit, and 80% of d's visits: the structure
// of d (its call to e) is read from the fifth only, and the row says so. Spread over the requests, the same number
// of broken traces marks nothing (TestLostCalleeBelowTheLimitIsNamed).
func TestVisitsOfOneKindOfRequestInBrokenTraces(t *testing.T) {
	e := &node{name: "e", c: 2, pods: 1, mean: 5}
	d := &node{name: "d", c: 2, pods: 1, mean: 5, children: []*node{e}}
	b := &node{name: "b", c: 4, pods: 1, mean: 5}
	a := &node{name: "a", c: 4, pods: 1, mean: 3, children: []*node{b, d}, p: []float64{1, 0.03}}
	out := run(a, 60, 300, 5, false)
	var spans []span
	for _, sp := range out.spans {
		if sp.svc == "e" && sp.kind == 2 && sp.trace%5 != 0 {
			continue
		}
		spans = append(spans, sp)
	}
	inf, err := inferErr(t, spans)
	if err != nil {
		t.Fatal(err)
	}
	di := inf.Topology.Index("d")
	st := inf.Stats[di]
	if st.BrokenVisitShare < 0.7 || st.BrokenVisitShare > 0.9 {
		t.Errorf("d: %.3f of its visits in broken traces, want about 0.8 (broken traces %d of %d)", st.BrokenVisitShare, inf.BrokenTraces, inf.Traces)
	}
	if share := float64(inf.BrokenTraces) / float64(inf.Traces); share > 0.05 {
		t.Fatalf("%.1f%% of the traces are broken: the test needs a capture below the limit", 100*share)
	}
	if warningWith(inf, "d: ", "of its", "visits are in traces that miss a span", "the row is marked !") == "" {
		t.Errorf("no warning names d: %q", inf.Warnings)
	}
	for _, s := range []string{"a", "b"} {
		if inf.Stats[inf.Topology.Index(s)].BrokenVisitShare != 0 {
			t.Errorf("%s is marked", s)
		}
	}
}
