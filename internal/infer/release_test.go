// SPDX-License-Identifier: Apache-2.0

package infer_test

// The worker release fed to the queue rebuild lies in the span, a message
// consumer's calls are its own, and clock skew is named, not an internal
// error. The captures are the ones the code review built;
// each failed at 29a7ed9.

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// ids hands out span ids.
type ids struct{ n int }

func (x *ids) next() string { x.n++; return fmt.Sprintf("%016x", x.n+0x1000) }

// fifoOne is a single queue of c workers: the start of service of each arrival.
func fifoOne(arr, svc []float64, c int) []float64 {
	h := make(free, c)
	out := make([]float64, len(arr))
	for i := range arr {
		f := heap.Pop(&h).(float64)
		b := math.Max(arr[i], f)
		heap.Push(&h, b+svc[i])
		out[i] = b
	}
	return out
}

// entryQueue is the code review's cap-base: one service fe with one worker, exponential 10 ms service times, 60 requests/s,
// spans that include the wait (n requests, one trace each). It returns the arrivals, the starts of service and
// the ends, and the root span of each trace (index into the spans).
func entryQueue(n int, seed int64) (arr, start, end []float64) {
	rng := rand.New(rand.NewSource(seed))
	t := 0.0
	svc := make([]float64, n)
	for i := 0; i < n; i++ {
		t += rng.ExpFloat64() * 1000 / 60
		arr = append(arr, t)
		svc[i] = 10 * rng.ExpFloat64()
	}
	start = fifoOne(arr, svc, 1)
	for i := range arr {
		end = append(end, start[i]+svc[i])
	}
	return arr, start, end
}

func rootSpans(arr, end []float64, x *ids) []span {
	var spans []span
	for i := range arr {
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), kind: 2, start: arr[i], end: end[i], trace: i + 1})
	}
	return spans
}

func runOpts(t *testing.T, spans []span, opt infer.Options) (*infer.Inferred, error) {
	t.Helper()
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	return infer.Run(d, opt)
}

// sameQueue checks that fe reads as the clean capture does: one worker, identified, the same service time.
func sameQueue(t *testing.T, what string, got, clean *infer.Inferred) {
	t.Helper()
	g, c := got.Stats[got.Topology.Index("fe")], clean.Stats[clean.Topology.Index("fe")]
	if g.WorkersPerPod != 1 || !g.WorkersFromQueueing {
		t.Errorf("%s: fe has %d workers (identified %v), want 1, identified", what, g.WorkersPerPod, g.WorkersFromQueueing)
	}
	if math.Abs(g.OwnMeanMs-c.OwnMeanMs) > 1e-9 || g.GlueMs != c.GlueMs {
		t.Errorf("%s: fe service time %.4f ms, glue %.4g; the clean capture's %.4f, %.4g", what, g.OwnMeanMs, g.GlueMs, c.OwnMeanMs, c.GlueMs)
	}
}

// cap-async: 30% of the visits send a call 50 ms after their span ended (after the response). At 29a7ed9 the
// release was taken at that call, so the rebuild read 5 workers and a 36.7 ms service time against 1 x 10 ms.
func TestAsyncCallAfterTheSpanFreesNoWorker(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	clean := rootSpans(arr, end, x)
	rng := rand.New(rand.NewSource(2))
	spans := append([]span(nil), clean...)
	for i := range clean {
		if rng.Float64() < 0.3 {
			cs := end[i] + 50
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: clean[i].id, kind: 3, start: cs, end: cs + 5,
				trace: i + 1, peer: "analytics", port: 443})
		}
	}
	sameQueue(t, "async call after the span", infer1(t, spans), infer1(t, clean))
}

// cap-mq-consumer: fe publishes a message (a PRODUCER span) on every visit; a worker's CONSUMER span (its parent
// the PRODUCER span) handles it 20 ms after fe's span ended and calls a database. At 29a7ed9 the walk to the
// owning visit went through the CONSUMER and PRODUCER spans, so the database call became fe's: 4 workers, 38 ms
// of service time, the latency +95%. In the second capture the consumer calls a service with SERVER spans.
func TestConsumerCallsAreNotTheProducersCalls(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	clean := rootSpans(arr, end, x)
	for _, server := range []bool{false, true} {
		var spans []span
		for i := range clean {
			root := clean[i]
			prod, cons, cl := x.next(), x.next(), x.next()
			spans = append(spans, root,
				span{svc: "fe", pod: "fe-0", id: prod, parent: root.id, kind: 4, start: end[i] - 0.2, end: end[i] - 0.1, trace: i + 1},
				span{svc: "worker", pod: "worker-0", id: cons, parent: prod, kind: 5, start: end[i] + 20, end: end[i] + 30, trace: i + 1},
				span{svc: "worker", pod: "worker-0", id: cl, parent: cons, kind: 3, start: end[i] + 21, end: end[i] + 29, trace: i + 1,
					peer: "db", port: 5432})
			if server {
				spans = append(spans, span{svc: "db", pod: "db-0", id: x.next(), parent: cl, kind: 2, start: end[i] + 21.2,
					end: end[i] + 28.8, trace: i + 1})
			}
		}
		what := fmt.Sprintf("consumer (db with server spans %v)", server)
		inf := infer1(t, spans)
		sameQueue(t, what, inf, infer1(t, clean))
		fe := inf.Topology.Index("fe")
		if calls := inf.Topology.Services[fe].Calls; len(calls) != 0 {
			t.Errorf("%s: fe should make no calls (the consumer's are its own), got %+v", what, calls)
		}
		if inf.BrokenTraces != 0 {
			t.Errorf("%s: %d traces read as broken", what, inf.BrokenTraces)
		}
		if server {
			// the consumer is not a service (no SERVER spans): its calls arrive at db from outside the graph, as
			// entries counted apart from the user requests (the code review's consumer-db: at 5b71d67 the header read
			// 6000 entry requests for 3000 user requests)
			db := inf.Topology.Index("db")
			if db < 0 || !inf.Stats[db].Entry || inf.Requests != 6000 || inf.ConsumerRequests != 3000 ||
				inf.Stats[db].ConsumerEntries != 3000 || inf.Stats[fe].ConsumerEntries != 0 {
				t.Errorf("%s: db should be an entry fed by the consumer (requests %d, %d from consumers)", what, inf.Requests,
					inf.ConsumerRequests)
			}
		} else {
			if inf.Requests != 3000 || inf.ConsumerRequests != 0 || len(inf.Topology.Services) != 1 {
				t.Errorf("%s: want fe alone with 3000 entry requests, got %d services, %d requests", what, len(inf.Topology.Services), inf.Requests)
			}
			// The code review's cap-mq-consumer: the consumer's calls to db, which has no server spans, are on no request's path;
			// at 5b71d67 they were left out without a word
			if !warned(inf, "3000 client calls made under message consumers") {
				t.Errorf("%s: no warning for the consumer's calls: %q", what, inf.Warnings)
			}
		}
	}
}

// skewSpans is the code review's cap-skew: fe -> be through SERVER -> SERVER parent links (no CLIENT spans), be's clock
// skewMs behind fe's; fe works exp(preMs) before the call and 1 ms after it, be exp(4 ms); 40 requests/s.
func skewSpans(skewMs, preMs float64) []span {
	rng := rand.New(rand.NewSource(4))
	x := &ids{}
	var spans []span
	t := 0.0
	for i := 0; i < 3000; i++ {
		t += rng.ExpFloat64() * 1000 / 40
		pre, be := preMs*rng.ExpFloat64(), 4*rng.ExpFloat64()
		root := x.next()
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: root, kind: 2, start: t, end: t + pre + be + 1, trace: i + 1},
			span{svc: "be", pod: "be-0", id: x.next(), parent: root, kind: 2, start: t + pre - skewMs, end: t + pre + be - skewMs, trace: i + 1})
	}
	return spans
}

// cap-skew2_1: be's clock 2 ms behind, so most of fe's calls seem to start before fe's span. At 29a7ed9 the
// rebuild got a negative mean service time and inference failed with the topology's input check ("service time
// must be a finite non-negative number"). The release is now clipped to the span and the skew is named, with the
// direction of its effect: the service time reads low (0.14 ms against about 1 ms).
func TestClockSkewIsNamed(t *testing.T) {
	inf, err := runOpts(t, skewSpans(2, 1), infer.Options{})
	if err != nil {
		t.Fatalf("clock skew of 2 ms: %v", err)
	}
	fe := inf.Topology.Index("fe")
	if s := inf.Topology.Services[fe].S; !(s >= 0) {
		t.Errorf("fe service time %v", s)
	}
	if !warned(inf, "fe:", "clock skew between pods", "its service time reads low") || warned(inf, "fe:", "same pod's clock") {
		t.Errorf("no warning names clock skew at fe and a service time that reads low: %q", inf.Warnings)
	}
	// no skew: no such warning
	inf, err = runOpts(t, skewSpans(0, 1), infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range inf.Warnings {
		if strings.Contains(w, "clock skew") {
			t.Errorf("no skew, but: %s", w)
		}
	}
}

// A queue rebuilt with a worker count the spans contradict gives a negative mean service time. The release is
// clipped to the span, so the search itself stops before that; a --workers count below the real one does not.
// Every 50 ms visit A works 10 ms and visits B arrive 1, 2 and 3 ms later and call out at once (release at
// arrival): with one worker each B would start after A's 10 ms, 7-9 ms after its own release. That is refused
// with a message that names the services and the causes, not with the topology's input check.
func TestNegativeServiceTimeIsRefused(t *testing.T) {
	x := &ids{}
	var spans []span
	tr := 0
	for g := 0; g < 300; g++ {
		t0 := float64(g) * 50
		tr++
		spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), kind: 2, start: t0, end: t0 + 10, trace: tr})
		for k := 1; k <= 3; k++ {
			tr++
			s := t0 + float64(k)
			root := x.next()
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: root, kind: 2, start: s, end: s + 0.5, trace: tr},
				span{svc: "fe", pod: "fe-0", id: x.next(), parent: root, kind: 3, start: s, end: s + 0.5, trace: tr, peer: "cache"})
		}
	}
	if _, err := runOpts(t, spans, infer.Options{}); err != nil {
		t.Fatalf("inferred workers: %v", err)
	}
	_, err := runOpts(t, spans, infer.Options{Workers: map[string]int{"fe": 1}})
	if err == nil {
		t.Fatal("one worker: no error")
	}
	for _, want := range []string{"negative mean service time", "fe -", "clock skew", "--workers"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "must be a finite non-negative number") {
		t.Errorf("an internal input check, not a refusal: %v", err)
	}
}

// The code review's cap-early: on 5% of the visits a CLIENT span of fe's own pod starts 1 ms before fe's span (and ends before
// it). The release is clipped to the span's start there, and such a visit that had to wait fits no FIFO queue, so
// the rebuild reads 8+ workers and 23.66 ms against 1 x 10 ms. At 5b71d67 the warning named clock skew between pods
// and said the service time reads low; on one pod's clock it is not skew, and the reading is high.
func TestEarlyOwnClientSpansReadHigh(t *testing.T) {
	arr, _, end := entryQueue(3000, 1)
	x := &ids{}
	clean := rootSpans(arr, end, x)
	rng := rand.New(rand.NewSource(5))
	spans := append([]span(nil), clean...)
	for i := range clean {
		if rng.Float64() < 0.05 {
			spans = append(spans, span{svc: "fe", pod: "fe-0", id: x.next(), parent: clean[i].id, kind: 3, start: arr[i] - 1,
				end: arr[i] - 0.5, trace: i + 1, peer: "cache", port: 6379})
		}
	}
	inf := infer1(t, spans)
	fe := inf.Stats[inf.Topology.Index("fe")]
	if !warned(inf, "fe:", "a CLIENT span of its own", "same pod's clock", "with those calls skipped", "its queue has 1 worker and",
		"it reads high, and so does the worker count") || warned(inf, "fe:", "clock skew between pods") {
		t.Errorf("fe at %d workers, %.2f ms: the warning should name its own pod's clock and a high reading: %q",
			fe.WorkersPerPod, fe.OwnMeanMs, inf.Warnings)
	}
}
