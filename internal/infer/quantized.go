// SPDX-License-Identifier: Apache-2.0

package infer

// Start times on whole milliseconds.
//
// Some runtimes stamp the start of a span on a whole millisecond (the OpenTelemetry
// JavaScript SDK on Node.js does, for the spans of the HTTP and gRPC instrumentations;
// the end is read from a finer clock). The start of such a span lies up to a
// millisecond before the work began. In the OpenTelemetry Demo, 100% of the starts
// of every span of the front end and the payment service (Node.js) are whole
// milliseconds, against 0.0 to 0.3% of those of the other services (a clock of
// microseconds would give 0.1%).
//
// For a caller that stamps its CLIENT spans so, calls that follow one another
// within a millisecond are read as made at once: the second call starts 1.6 ms after
// the first began (1.2 ms after the first ended), but both are stamped to the
// millisecond they fall in, and the second is stamped before the first has ended. In
// the Demo that made "cart, then cart" of the front end's POST /api/cart (967 of
// 1731 visits) and the recommendation lookup that precedes four catalogue lookups
// (256 of 794) read as calls at once, and so the front end's call shapes wrong
// (the callee's load is exact whatever the shape). The callee's SERVER span,
// stamped by a finer clock, tells the order: a call is placed where its callee's span
// is, and two calls are in one stage unless the callee spans leave a gap between them
// of at least quantGap of the round trip's overhead (the client span's time less the
// server span's, the time on the wire and in the client library, averaged over the
// two calls). One call's return and the next call's send fall in that gap when the
// second starts after the first has ended; calls made at once leave less.
//
// The test needs the callee's span, so a call without one is placed by its own start,
// and the pods that stamp on whole milliseconds are found from their own spans: a pod
// whose spans start on whole milliseconds in at least quantShare of them (and at
// least quantMinSpans of them) is one. The end of such a caller's span can come before the end of its last CLIENT
// span, so a call that ends after the span is allowed a millisecond (notWaited) before it is taken for one that
// was not waited for.

import (
	"math"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

const (
	quantMinSpans = 100       // spans a pod must have to be judged
	quantShare    = 0.99      // share of a pod's spans that start on a whole millisecond, from which its clock is taken to be one of milliseconds
	quantGap      = 0.25      // share of the calls' mean round-trip overhead that the callee spans of two successive calls must leave between them
	quantSlackNs  = 1_000_000 // how long after its caller's span a call's end may fall and the call still be one it waited for
)

// quantizedPods reports, for each pod (an index in resPod's values), whether the pod's spans start on whole milliseconds.
func quantizedPods(spans []otlp.Span, resPod []int32, nPods int) []bool {
	total := make([]int, nPods)
	whole := make([]int, nPods)
	for i := range spans {
		p := resPod[spans[i].Res]
		total[p]++
		if spans[i].Start%1_000_000 == 0 {
			whole[p]++
		}
	}
	out := make([]bool, nPods)
	for p := range out {
		out[p] = total[p] >= quantMinSpans && float64(whole[p]) >= quantShare*float64(total[p])
	}
	return out
}

// placed reports whether the call is placed by its callee's span rather than by its own start.
func (c call) placed() bool { return c.quant && c.hasServer && c.sstart > 0 }

// qa and qb are where the call is placed: its callee's SERVER span when it has one and its CLIENT span's start is
// stamped on a whole millisecond, else its own span.
func (c call) qa() uint64 {
	if c.placed() {
		return c.sstart
	}
	return c.start
}

func (c call) qb() uint64 {
	if c.placed() {
		return c.send
	}
	return c.end
}

// qo is the call's round-trip overhead in ns: its client span's time less its callee span's (0 for a call that is
// not placed by its callee's span).
func (c call) qo() float64 {
	if c.placed() {
		return math.Max(0, c.clientDur*1e6-float64(c.send-c.sstart))
	}
	return 0
}

// anyPlaced reports whether some call of the visit is placed by its callee's span.
func anyPlaced(cs []call) bool {
	for _, c := range cs {
		if c.placed() {
			return true
		}
	}
	return false
}

// stagesOfQ is stagesOf for calls sorted by qa: a call begins the next stage when it is placed at least quantGap of the two
// calls' round-trip overhead (the stage's largest so far and its own, averaged) after the stage's last end. For calls not
// placed by a callee's span the overhead is 0, which is stagesOf's rule (a call starting at or after the stage's latest end).
func stagesOfQ(cs []call) [][]int {
	var out [][]int
	var cur []int
	var curEnd uint64
	curO := 0.0
	for j, c := range cs {
		if len(cur) > 0 {
			gap := int64(c.qa()) - int64(curEnd) // whole ns: a float64 holds a Unix time in ns only to about 250 ns
			if float64(gap) >= quantGap*(curO+c.qo())/2 {
				out = append(out, cur)
				cur, curO = nil, 0
			}
		}
		cur = append(cur, j)
		if len(cur) > 1 {
			curEnd = max(curEnd, c.qb())
		} else {
			curEnd = c.qb()
		}
		curO = max(curO, c.qo())
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// stageSpans is the time the stages of a visit's calls cover, in ns: for each stage, from its earliest call's start to its
// latest call's end, clipped to the visit's span [start, end]. For calls that do not overlap in time it is the time they
// cover; calls stamped on whole milliseconds that follow one another can overlap by their stamps, and are covered twice.
func stageSpans(cs []call, stages [][]int, start, end uint64) uint64 {
	var total uint64
	for _, st := range stages {
		lo, hi := uint64(math.MaxUint64), uint64(0)
		for _, j := range st {
			lo = min(lo, max(cs[j].start, start))
			hi = max(hi, min(cs[j].end, end))
		}
		if hi > lo {
			total += hi - lo
		}
	}
	return total
}
