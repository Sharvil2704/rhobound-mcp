// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// nodeLike makes n visits of A (60 ms apart, nothing queues) that call B twice, one after the other (seq) or together, with a
// round trip of 1.3 ms around a callee span of 0.6 ms and 0.35 ms between one call's end and the next call's send. With
// whole set, A's spans are stamped as Node.js stamps them: the start of every one on a whole millisecond (the floor), the
// end as it was.
func nodeLike(n int, seq, whole bool, seed int64) []span {
	rng := rand.New(rand.NewSource(seed))
	var spans []span
	id := 0
	nid := func() string { id++; return fmt.Sprintf("%016x", id) }
	stamp := func(ms float64) float64 {
		if whole {
			return math.Floor(ms)
		}
		return ms
	}
	for i := 0; i < n; i++ {
		t0 := 100 + float64(i)*60 + rng.Float64() // a fraction of a ms: the floors differ from visit to visit
		root := nid()
		now := t0 + 0.2
		calls := [][2]float64{}
		if seq {
			for k := 0; k < 2; k++ {
				calls = append(calls, [2]float64{now, now + 1.3})
				now += 1.3 + 0.35
			}
		} else {
			calls = append(calls, [2]float64{now, now + 1.3}, [2]float64{now + 0.02, now + 1.32})
			now += 1.32
		}
		for _, c := range calls {
			cid := nid()
			spans = append(spans,
				span{svc: "A", pod: "a-0", id: cid, parent: root, kind: 3, start: stamp(c[0]), end: c[1], trace: i + 1, peer: "B", port: 80, wholeMs: whole},
				span{svc: "B", pod: "b-0", id: nid(), parent: cid, kind: 2, start: c[0] + 0.35, end: c[0] + 0.95, trace: i + 1})
		}
		spans = append(spans, span{svc: "A", pod: "a-0", id: root, kind: 2, start: stamp(t0), end: now + 0.1, trace: i + 1, wholeMs: whole})
	}
	return spans
}

// A caller that stamps its spans on whole milliseconds, and calls B twice one after the other with 0.35 ms between the calls: in
// 65% of the visits the second call is stamped before the first has ended. Read by the stamps, they are calls at once and A
// a parallel service; read by where B's spans are, two calls in a row. And calls that are made together stay together.
func TestCallsStampedToTheMillisecondKeepTheirOrder(t *testing.T) {
	for _, c := range []struct {
		name     string
		seq      bool
		whole    bool
		parallel bool
	}{
		{"in a row, clock of microseconds", true, false, false},
		{"in a row, stamped to the millisecond", true, true, false},
		{"together, clock of microseconds", false, false, true},
		{"together, stamped to the millisecond", false, true, true},
	} {
		inf, err := inferErr(t, nodeLike(600, c.seq, c.whole, 7))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		a := inf.Topology.Services[inf.Topology.Index("A")]
		if a.Parallel != c.parallel {
			t.Errorf("%s: A parallel %v, want %v (calls %+v)", c.name, a.Parallel, c.parallel, a.Calls)
		}
		// two services and two calls from A to B: no stage or shape nodes, no mixture of a pair in a row and a pair together
		b := inf.Topology.Index("B")
		if len(inf.Topology.Services) != 2 || len(a.Calls) != 2 || a.Calls[0].Child != b || a.Calls[1].Child != b || a.Calls[0].P != 1 || a.Calls[1].P != 1 {
			t.Errorf("%s: %d services, A's calls %+v, want A and B with two certain calls", c.name, len(inf.Topology.Services), a.Calls)
		}
	}
	// the pods that stamp so are found from their spans: A's spans here are 1800 of whole ms, B's are not
	in, err := inferErr(t, nodeLike(600, true, true, 7))
	if err != nil {
		t.Fatal(err)
	}
	res, err := model.AnalyticWith(in.Topology, in.LamExtPerMs, nil, model.Default)
	if err != nil {
		t.Fatal(err)
	}
	// A's latency: the visit's span, which is what the callers saw. Own, glue and two calls in a row add up to it.
	want := 0.2 + 2*1.3 + 0.35 + 0.1 // the visit's span (the stamped start is up to a millisecond early: inside "own")
	if got := res.L[in.Topology.Index("A")]; math.Abs(got-want) > 1.0 {
		t.Errorf("A's latency %.2f ms, want about %.2f", got, want)
	}
}
