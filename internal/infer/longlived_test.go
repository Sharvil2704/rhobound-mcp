// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

const (
	msNs    = 1_000_000             // ns
	secNs   = 1_000_000_000         // ns
	epochNs = 1_700_000_000 * secNs // a capture's start
)

// serverSpans is n SERVER spans of one service (resource 0) that start every gap ns and last dur ns, in trace order.
func serverSpans(n int, gap, dur uint64, first uint64) []otlp.Span {
	out := make([]otlp.Span, n)
	for i := range out {
		st := epochNs + uint64(i)*gap
		out[i] = otlp.Span{Trace: [2]uint64{1, first + uint64(i)}, ID: first + uint64(i), Start: st, End: st + dur, Peer: -1, Addr: -1, Kind: otlp.KindServer}
	}
	return out
}

func dataOf(spans ...otlp.Span) *otlp.Data {
	return &otlp.Data{Spans: spans, Resources: []otlp.Resource{{Service: "flagd", Pod: "flagd-0"}}}
}

func longOf(d *otlp.Data) []longService {
	return longVisits(d, []int32{0}, []string{"flagd"})
}

// Three streams of 600 s in a capture of 7,000 visits of a tenth of a millisecond over 420 s: they are all of flagd's busy
// time, the capture is refused, and the message names the service, the longest stream and what to do.
func TestLongVisitsAreRefused(t *testing.T) {
	spans := serverSpans(7000, 60*msNs, 100_000, 100)
	stream := serverSpans(3, 10*secNs, 600*secNs, 1)
	d := dataOf(append(spans, stream...)...)
	d.LongNames = map[otlp.SpanKey]string{{Trace: [2]uint64{1, 1}, ID: 1}: "flagd.evaluation.v1.Service/EventStream"}
	found := longOf(d)
	if len(found) != 1 || found[0].n != 3 || found[0].longestNs != 600*secNs || found[0].share < 0.99 {
		t.Fatalf("found %+v, want 3 streams of 600 s holding all of flagd's busy time", found)
	}
	err := longVisitsError(d, found)
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{"flagd: 3 of its server spans last up to 600 s (flagd.evaluation.v1.Service/EventStream, trace ",
		"against a median of 0.1 ms, and hold 100% of its busy time", "cut the capture by the time its traces started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal lacks %q: %v", want, err)
		}
	}
	if w := longVisitsWarnings(d, found); len(w) != 0 {
		t.Errorf("warnings %q for a capture that is refused", w)
	}
	// without the name the capture kept, the trace is named
	d.LongNames = nil
	if err := longVisitsError(d, longOf(d)); err == nil || !strings.Contains(err.Error(), "(trace 0000000000000001") {
		t.Errorf("no trace named: %v", err)
	}
}

// What is not a stream: a request that is slow but a small part of the capture's duration, one that is not a thousand times
// the median, one under a second, and a capture with no long span at all. And the edges of the three conditions.
func TestSlowRequestsAreNotStreams(t *testing.T) {
	base := func() []otlp.Span { return serverSpans(5000, 20*msNs, 5*msNs, 100) } // 100 s of 5 ms visits
	for _, c := range []struct {
		name string
		dur  uint64 // of one more span, which starts first
		long bool
	}{
		{"none", 0, false},
		{"2 s (2% of the capture, 400 medians)", 2 * secNs, false},
		{"9 s (9% of the capture)", 9 * secNs, false},
		{"12 s of a capture of 100 s, 2400 medians", 12 * secNs, true},
		{"600 s", 600 * secNs, true},
	} {
		spans := base()
		if c.dur > 0 {
			spans = append(spans, serverSpans(1, 1, c.dur, 1)...)
		}
		found := longOf(dataOf(spans...))
		if (len(found) == 1) != c.long {
			t.Errorf("%s: found %+v, want long: %v", c.name, found, c.long)
		}
	}
	// the median: a service whose requests last 20 ms takes 20 s to be a stream (1000 medians), however short the capture
	for _, c := range []struct {
		dur  uint64
		long bool
	}{{19 * secNs, false}, {21 * secNs, true}} {
		spans := serverSpans(3000, 20*msNs, 20*msNs, 100) // 60 s
		spans = append(spans, serverSpans(1, 1, c.dur, 1)...)
		if found := longOf(dataOf(spans...)); (len(found) == 1) != c.long {
			t.Errorf("%d s among visits of 20 ms: found %+v, want long: %v", c.dur/secNs, found, c.long)
		}
	}
	// under a second is never one, in a capture of ten seconds
	spans := serverSpans(10000, msNs, 20_000, 100)
	spans = append(spans, serverSpans(1, 1, 900*msNs, 1)...)
	if found := longOf(dataOf(spans...)); len(found) != 0 {
		t.Errorf("a request of 0.9 s among visits of 0.02 ms: %+v", found)
	}
	// ... and no spans that start over any time: nothing to compare with
	if found := longOf(dataOf(serverSpans(1, 1, 10*secNs, 1)...)); len(found) != 0 {
		t.Errorf("a single span: %+v", found)
	}
	// CLIENT and INTERNAL spans are not visits
	spans = base()
	long := serverSpans(1, 1, 600*secNs, 1)
	long[0].Kind = otlp.KindClient
	if found := longOf(dataOf(append(spans, long...)...)); len(found) != 0 {
		t.Errorf("a long CLIENT span: %+v", found)
	}
}

// Long spans that hold under longBusyRefuse of a busy service's time are in its means as visits, and a warning names them.
func TestLongVisitsOfABusyServiceAreWarnedOf(t *testing.T) {
	// 40,000 visits of 1 ms, 0.25 ms apart: ten seconds, four workers busy; one request of 1.5 s starts among them
	spans := serverSpans(40000, 250_000, msNs, 100)
	spans = append(spans, serverSpans(1, 3*secNs, 1500*msNs, 1)...)
	// a 1.5 s span is 15% of the capture's 10 s and 1500 medians: a stream; it holds 1.5 of the 41.5 s
	d := dataOf(spans...)
	found := longOf(d)
	if len(found) != 1 || found[0].n != 1 || found[0].share >= longBusyRefuse || found[0].share < 0.03 {
		t.Fatalf("found %+v, want one long span holding 3-5%% of the busy time", found)
	}
	if err := longVisitsError(d, found); err != nil {
		t.Fatalf("refused: %v", err)
	}
	w := longVisitsWarnings(d, found)
	if len(w) != 1 || !strings.Contains(w[0], "flagd: 1 of its server spans last up to 2 s") || !strings.Contains(w[0], "are read as visits") {
		t.Errorf("warnings %q", w)
	}
}

// Run refuses a real capture (20 s of the Demo's shipped mix) with a stream of flagd added, before anything else is said
// about it, and reads it as before without.
func TestRunRefusesAStreamInARealCapture(t *testing.T) {
	read := func() *otlp.Data {
		d, err := otlp.ReadDir("testdata/counts/shipped-20s")
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	flagd := -1
	d := read()
	for i, r := range d.Resources {
		if r.Service == "flagd" {
			flagd = i
		}
	}
	if flagd < 0 {
		t.Fatal("no flagd in the capture")
	}
	first := d.Spans[0].Start
	for _, sp := range d.Spans {
		first = min(first, sp.Start)
	}
	for k := 0; k < 3; k++ {
		d.Spans = append(d.Spans, otlp.Span{Trace: [2]uint64{0xabc, uint64(k + 1)}, ID: uint64(k + 1), Start: first + uint64(k)*secNs, End: first + uint64(k)*secNs + 600*secNs,
			Res: int32(flagd), Peer: -1, Addr: -1, Kind: otlp.KindServer})
	}
	_, err := Run(d, Options{})
	if err == nil || !strings.Contains(err.Error(), "flagd: 3 of its server spans last up to 600 s") {
		t.Fatalf("a capture with three streams of 600 s: %v", err)
	}
	if _, err := Run(read(), Options{}); err != nil {
		t.Fatalf("the capture without the streams: %v", err)
	}
}

func TestLongVisitsKeyOrder(t *testing.T) {
	a, b := otlp.SpanKey{Trace: [2]uint64{1, 2}, ID: 5}, otlp.SpanKey{Trace: [2]uint64{1, 3}, ID: 1}
	if !lessKey(a, b) || lessKey(b, a) || lessKey(a, a) {
		t.Error("lessKey is not a strict order")
	}
}
