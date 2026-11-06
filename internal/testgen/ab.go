// SPDX-License-Identifier: Apache-2.0

package testgen

import (
	"sort"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The changes the A/B pairs of the variance test inject into b's own time per visit (DATA.md 8; testdata/mcp/loop).
const (
	// ABShiftNs is the own time the candidate of ABShift adds to every visit of b.
	ABShiftNs = 2_000_000
	// ABVarianceNs is the own time the baseline of ABVariance adds to every visit of b; its candidate adds twice as much
	// to every other visit and nothing to the others.
	ABVarianceNs = 15_000_000
)

// AddOwnTime adds own time to the visits of svc (its SERVER spans) before their first call: the k-th visit, in the
// order the visits start, gets d(k) ns more. The calls it makes, and everything under them, start that much later,
// and its span and the spans above it end that much later; the visits themselves arrive when they did. Nothing is
// queued again: a d that varies between visits moves the callees' arrivals by different amounts, which a callee pinned
// to one worker reads as waits.
func AddOwnTime(c *Capture, svc string, d func(k int) int64) {
	byID := map[string]*Span{}
	kids := map[string][]*Span{}
	var visits []*Span
	c.Visit(func(rs *ResourceSpans, s *Span) {
		byID[s.TraceID+s.SpanID] = s
		kids[s.TraceID+s.ParentSpanID] = append(kids[s.TraceID+s.ParentSpanID], s)
		if rs.Service() == svc && s.KindOf() == otlp.KindServer {
			visits = append(visits, s)
		}
	})
	sort.SliceStable(visits, func(i, j int) bool {
		a, _ := visits[i].Times()
		b, _ := visits[j].Times()
		return a < b || a == b && visits[i].TraceID+visits[i].SpanID < visits[j].TraceID+visits[j].SpanID
	})
	var shift func(s *Span, by int64)
	shift = func(s *Span, by int64) {
		s.Shift(by)
		for _, k := range kids[s.TraceID+s.SpanID] {
			shift(k, by)
		}
	}
	for k, v := range visits {
		by := d(k)
		if by == 0 {
			continue
		}
		for _, kid := range kids[v.TraceID+v.SpanID] {
			shift(kid, by)
		}
		for p := v; p != nil; p = byID[p.TraceID+p.ParentSpanID] {
			a, b := p.Times()
			p.SetTimes(a, uint64(int64(b)+by))
		}
	}
}

// Window keeps the traces of the base capture whose entry span (a's SERVER span) starts in [from, to) after the
// data's start.
func Window(c *Capture, from, to time.Duration) {
	keepTraces(c, func(_ uint64, off time.Duration) bool { return off >= from && off < to })
}

// ABShift is an A/B pair of c for b: the baseline is c, and the candidate adds ABShiftNs of own time to every visit of
// b. The mean moves and the variance stays: a pure shift, which is no variance change.
func ABShift(c *Capture) (baseline, candidate *Capture) {
	baseline, candidate = c.Clone(), c.Clone()
	AddOwnTime(candidate, svcB, func(int) int64 { return ABShiftNs })
	return baseline, candidate
}

// ABVariance is an A/B pair of c for b with the same mean own time and a larger variance in the candidate: the
// baseline adds ABVarianceNs to every visit of b, and the candidate adds twice that to every other visit (in the order
// they start) and nothing to the others, which adds the square of ABVarianceNs to the variance. Own time cannot be
// taken from a visit that has little, so the means are matched by adding to both arms.
func ABVariance(c *Capture) (baseline, candidate *Capture) {
	baseline, candidate = c.Clone(), c.Clone()
	AddOwnTime(baseline, svcB, func(int) int64 { return ABVarianceNs })
	AddOwnTime(candidate, svcB, func(k int) int64 { return int64(k%2) * 2 * ABVarianceNs })
	return baseline, candidate
}
