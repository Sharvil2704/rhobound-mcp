// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"math"
	"math/rand"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

func TestQuantizedPodsAreFoundFromTheirOwnSpans(t *testing.T) {
	const ms = 1_000_000
	spans := func(res int32, n, whole int) []otlp.Span {
		out := make([]otlp.Span, n)
		for i := range out {
			start := uint64(1_700_000_000*1_000_000_000 + i*7*ms)
			if i >= whole {
				start += 123_456 // a microsecond clock
			}
			out[i] = otlp.Span{Res: res, Start: start, End: start + ms}
		}
		return out
	}
	var all []otlp.Span
	all = append(all, spans(0, 300, 300)...) // every start on a whole ms
	all = append(all, spans(1, 300, 150)...) // half of them
	all = append(all, spans(2, 99, 99)...)   // all, but too few to judge
	all = append(all, spans(3, 300, 297)...) // 99% exactly
	all = append(all, spans(4, 300, 296)...) // just under
	got := quantizedPods(all, []int32{0, 1, 2, 3, 4}, 5)
	want := []bool{true, false, false, true, false}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pod %d: %v, want %v", i, got[i], want[i])
		}
	}
	// two resources of one pod count together
	got = quantizedPods(append(spans(0, 60, 60), spans(1, 60, 60)...), []int32{0, 0}, 1)
	if !got[0] {
		t.Error("120 spans of one pod on two resources, all on whole ms, are not found")
	}
}

// nsAt is a Unix time in ns for a time in ms after 1,700,000,000 s (whole ns: a float64 holds a Unix time in ns only to about 250 ns).
func nsAt(ms float64) uint64 { return 1_700_000_000*1_000_000_000 + uint64(math.Round(ms*1e6)) }

// pair is a placed call: a client span from `from` to `to` (ms) around a callee span `srvFrom`..`srvTo`.
func pair(from, to, srvFrom, srvTo float64) call {
	ns := nsAt
	return call{callee: 1, start: ns(from), end: ns(to), clientDur: to - from, hasServer: true, sstart: ns(srvFrom), send: ns(srvTo),
		ownClock: true, quant: true}
}

// Two calls whose client spans overlap by their stamps (the second is stamped before the first has ended) are in one stage if
// their callees' spans overlap or leave less than a quarter of the round trip's overhead between them, in two otherwise.
func TestStagesOfQSeparatesSequentialCallsStampedToTheMillisecond(t *testing.T) {
	// a round trip of 1.3 ms around a 0.6 ms callee span: an overhead of 0.7 ms for each call; a quarter of their mean is 0.175 ms
	first := pair(10.0, 11.3, 10.3, 10.9)
	for _, c := range []struct {
		name   string
		second call
		stages int
		note   string
	}{
		{"callee spans 0.30 ms apart (sequential)", pair(10.9, 12.2, 11.2, 11.8), 2, "stamped at 10.9: before the first call has ended (11.3)"},
		{"callee spans 0.18 ms apart (just sequential)", pair(10.9, 12.2, 11.08, 11.68), 2, ""},
		{"callee spans 0.17 ms apart (just at once)", pair(10.9, 12.2, 11.07, 11.67), 1, ""},
		{"callee spans 0.10 ms apart (at once)", pair(10.9, 12.2, 11.0, 11.6), 1, ""},
		{"callee spans overlapping (at once)", pair(10.0, 11.3, 10.5, 11.1), 1, ""},
		{"callee span before the first's (at once)", pair(10.0, 11.3, 10.1, 10.7), 1, ""},
	} {
		cs := []call{first, c.second}
		if got := len(stagesOfQ(cs)); got != c.stages {
			t.Errorf("%s: %d stages, want %d", c.name, got, c.stages)
		}
	}
	// read by their client spans alone, the overlapping stamps put all of them in one stage
	if got := len(stagesOf([]call{first, pair(10.9, 12.2, 11.2, 11.8)})); got != 1 {
		t.Errorf("by the client spans: %d stages, want 1 (the artefact)", got)
	}
	// an overhead of nothing is stagesOf's rule
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 500; trial++ {
		var cs []call
		at := uint64(1_700_000_000 * 1_000_000_000)
		for k := 0; k < 1+rng.Intn(6); k++ {
			at += uint64(rng.Intn(400_000))
			end := at + uint64(rng.Intn(900_000))
			cs = append(cs, call{callee: int32(rng.Intn(3)), start: at, end: end, clientDur: float64(end-at) / 1e6, hasServer: true, sstart: at + 1, send: end - 1})
		}
		a, b := stagesOf(cs), stagesOfQ(cs)
		if len(a) != len(b) {
			t.Fatalf("calls not stamped to the millisecond: %d stages by stagesOf, %d by stagesOfQ: %+v", len(a), len(b), cs)
		}
	}
}

func TestNotWaitedAllowsAMillisecondToACallerStampedToTheMillisecond(t *testing.T) {
	const end = 5_000_000_000
	for _, c := range []struct {
		name  string
		call  call
		want  bool
		quant bool
	}{
		{"ends 0.2 ms after the visit", call{start: end - 1_000_000, end: end + 200_000, ownClock: true}, false, false},
		{"ends 0.5 ms after the visit", call{start: end - 1_000_000, end: end + 500_000, ownClock: true}, true, false},
		{"ends 0.5 ms after the visit, caller stamps ms", call{start: end - 1_000_000, end: end + 500_000, ownClock: true, quant: true}, false, true},
		{"ends 0.9 ms after the visit, caller stamps ms", call{start: end - 1_000_000, end: end + 900_000, ownClock: true, quant: true}, false, true},
		{"ends 1.5 ms after the visit, caller stamps ms", call{start: end - 1_000_000, end: end + 1_500_000, ownClock: true, quant: true}, true, true},
		{"starts after the visit, caller stamps ms", call{start: end + 1, end: end + 2_000_000, ownClock: true, quant: true}, true, true},
		{"starts 0.5 ms before the visit's end, ends 0.5 ms before it, caller stamps ms", call{start: end - 500_000, end: end - 1}, false, true},
	} {
		if got := notWaited(c.call, end); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// Calls stamped to the millisecond that follow each other overlap in the spans, though not in time: the time they cover is
// counted by stage, so that the glue (own time after the first call) is the visit's less what the model adds for the stages.
func TestStageSpansCountsEachStageOnce(t *testing.T) {
	a, b := pair(10.0, 11.3, 10.3, 10.9), pair(10.9, 12.2, 11.2, 11.8)
	cs := []call{a, b}
	ns := nsAt
	// two stages: (11.3 - 10.0) + (12.2 - 10.9) = 2.6 ms (the union is 2.2 ms)
	if got := stageSpans(cs, [][]int{{0}, {1}}, ns(9), ns(13)); got != 2_600_000 {
		t.Errorf("two stages: %d ns, want 2600000", got)
	}
	// one stage: from the first start to the last end
	if got := stageSpans(cs, [][]int{{0, 1}}, ns(9), ns(13)); got != 2_200_000 {
		t.Errorf("one stage: %d ns, want 2200000", got)
	}
	// clipped to the visit
	if got := stageSpans(cs, [][]int{{0}, {1}}, ns(10.5), ns(12)); got != (11_300_000-10_500_000)+(12_000_000-10_900_000) {
		t.Errorf("clipped: %d ns", got)
	}
}
