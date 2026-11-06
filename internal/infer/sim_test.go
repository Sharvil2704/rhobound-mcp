// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"bytes"
	"compress/gzip"
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// A small discrete-event simulator of a tree of FIFO multi-server services
// that writes OTLP/JSON traces. It is the ground truth of these tests: the
// answers it gives do not come from the code under test.

const hopMs = 0.15 // one-way network time

// hopJitterK is the shape of the extra time a network leg takes when a run has a hop jitter (simOpts.hopJitter).
const hopJitterK = 0.7

// envHopJitter is ACC_HOPJIT, the hop jitter the accuracy runs use when their options set none (default 0: the
// constant hop, the logs of docs/accuracy-logs).
func envHopJitter() float64 {
	v := os.Getenv("ACC_HOPJIT")
	if v == "" {
		return 0
	}
	var x float64
	if _, err := fmt.Sscanf(v, "%g", &x); err != nil || x < 0 {
		panic(fmt.Sprintf("ACC_HOPJIT=%q is not a non-negative number of ms", v))
	}
	return x
}

type node struct {
	name     string
	c        int // workers per pod
	pods     int
	mean     float64 // mean service time, ms (of the random part)
	shift    float64 // constant extra time per visit (a "change"), ms
	post     float64 // constant time after the calls (writing the response), worker not held, ms; a leaf's reads as service
	dist     string  // "" or "exp": exponential; "gamma"; "h2": balanced-means hyperexponential
	cs2      float64 // squared coefficient of variation of the random part, for gamma and h2
	parallel bool
	children []*node
	// stage, when set, makes the calls in stages: child j is in stage stage[j] (equal values next to each
	// other), the calls of one stage go out together, and the next stage starts once they have all answered.
	stage []int
	// p, when set, is the probability that child j is called on a visit (a conditional call), drawn per call.
	p []float64
	// shares, when set, is each pod's probability of taking a visit (an uneven split); else pods are even.
	shares []float64
}

// gammaSample draws Gamma(shape k, scale theta) (Marsaglia and Tsang).
func gammaSample(r *rand.Rand, k, theta float64) float64 {
	if k < 1 {
		return gammaSample(r, k+1, theta) * math.Pow(r.Float64(), 1/k)
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := r.Float64()
		if math.Log(u) < 0.5*x*x+d-d*v+d*math.Log(v) {
			return d * v * theta
		}
	}
}

// draw returns one service time in ms.
func (n *node) draw(r *rand.Rand) float64 {
	switch n.dist {
	case "gamma":
		return gammaSample(r, 1/n.cs2, n.mean*n.cs2) + n.shift
	case "lognormal":
		sg2 := math.Log(1 + n.cs2)
		return math.Exp(math.Log(n.mean)-sg2/2+math.Sqrt(sg2)*r.NormFloat64()) + n.shift
	case "h2":
		p1 := 0.5 * (1 + math.Sqrt((n.cs2-1)/(n.cs2+1)))
		if r.Float64() < p1 {
			return r.ExpFloat64()*n.mean/(2*p1) + n.shift
		}
		return r.ExpFloat64()*n.mean/(2*(1-p1)) + n.shift
	}
	return r.ExpFloat64()*n.mean + n.shift
}

type span struct {
	svc, pod   string
	id, parent string
	kind       int
	start, end float64 // ms
	trace      int
	peer       string
	port       int    // a client span's server.port, if set
	route      string // a server span's url.path, if set
	scope      string // the instrumentation scope that emitted it, if set
	wholeMs    bool   // start is a whole number of ms, written exactly (a float64 holds a Unix time in ns only to about 250 ns)
	name       string // the span's name, if set
}

type simOut struct {
	spans   []span
	entryMs []float64 // entry server-span durations
	totalMs []float64 // entry arrival to end, wait included
}

type free []float64 // min-heap of worker free times

func (h free) Len() int            { return len(h) }
func (h free) Less(i, j int) bool  { return h[i] < h[j] }
func (h free) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *free) Push(x interface{}) { *h = append(*h, x.(float64)) }
func (h *free) Pop() interface{} {
	o := *h
	v := o[len(o)-1]
	*h = o[:len(o)-1]
	return v
}

type sim struct {
	rng *rand.Rand
	simOpts
	spans  []span
	nextID int
	hr     *rand.Rand // the random stream of the hop jitter
}

// hop is the time of one network leg: hopMs, and with a hop jitter that much extra on average (a gamma draw: network
// times have a right-skewed spread, the mean above the quietest tenth).
func (s *sim) hop() float64 {
	if s.hr == nil {
		return hopMs
	}
	return hopMs + gammaSample(s.hr, hopJitterK, s.hopJitter/hopJitterK)
}

// simOpts are a run's options. None of them changes the random numbers of the requests and the service times, so a
// seed gives the same ones whatever they are (the hop jitter draws from a stream of its own).
type simOpts struct {
	queueInSpan bool // the server span starts at arrival, so it includes the wait for a worker
	noSpans     bool // write no spans (a truth run needs only the latencies)
	// hopJitter, when positive, is the mean extra time of each network leg, ms, on top of hopMs: the legs of one call
	// then differ, and the hop of a callee, taken as the mean, lies above the hop as its quietest tenth.
	hopJitter float64
	// nest, when set, adds framework-style nested spans to the written spans (nestSpans). It draws no random
	// numbers and adds spans only at the end, so the requests, the times and the ids of the other spans are the
	// same with and without it.
	nest nestOpts
	// rec, when set, sees every visit batch of a node once its server spans have ended: arrival, start of
	// service, end of own work and end of the server span (ms), and the pod and trace of each visit.
	rec func(n *node, arr, start, ownEnd, end []float64, podOf, traces []int)
}

func (s *sim) id() string { s.nextID++; return fmt.Sprintf("%016x", s.nextID) }

// visit runs one node for a batch of arrivals (ms, one per request) and returns
// each request's server-span end time. parents[i] is the client span id (or "").
func (s *sim) visit(n *node, arr []float64, parents []string, traces []int, entry bool) []float64 {
	N := len(arr)
	order := make([]int, N)
	for i := range order {
		order[i] = i
	}
	podOf := make([]int, N)
	for i := range podOf {
		if n.shares == nil {
			podOf[i] = s.rng.Intn(n.pods)
			continue
		}
		u := s.rng.Float64()
		for podOf[i] < n.pods-1 && u >= n.shares[podOf[i]] {
			u -= n.shares[podOf[i]]
			podOf[i]++
		}
	}
	sort.Slice(order, func(a, b int) bool { return arr[order[a]] < arr[order[b]] })
	servers := make([]*free, n.pods)
	for p := range servers {
		h := make(free, n.c)
		servers[p] = &h
	}
	start := make([]float64, N)
	ownEnd := make([]float64, N)
	for _, i := range order {
		h := servers[podOf[i]]
		t := heap.Pop(h).(float64)
		st := math.Max(arr[i], t)
		x := n.draw(s.rng)
		start[i], ownEnd[i] = st, st+x
		heap.Push(h, st+x)
	}
	ids := make([]string, N)
	if !s.noSpans {
		for i := range ids {
			ids[i] = s.id()
		}
	}
	end := make([]float64, N)
	copy(end, ownEnd)
	// calls
	prev := make([]float64, N) // sequential: when the next call goes out
	copy(prev, ownEnd)
	var stStart, stEnd []float64 // staged: when the current stage started, and its latest answer so far
	if n.stage != nil {
		stStart, stEnd = make([]float64, N), make([]float64, N)
		copy(stStart, ownEnd)
		copy(stEnd, ownEnd)
	}
	all := make([]int, N)
	for i := range all {
		all[i] = i
	}
	for j, ch := range n.children {
		if n.stage != nil && (j == 0 || n.stage[j] != n.stage[j-1]) {
			copy(stStart, stEnd)
		}
		sel, tr := all, traces // the visits that make this call
		if n.p != nil && n.p[j] < 1 {
			sel, tr = nil, nil
			for i := 0; i < N; i++ {
				if s.rng.Float64() < n.p[j] {
					sel, tr = append(sel, i), append(tr, traces[i])
				}
			}
		}
		callAt := make([]float64, len(sel))
		for x, i := range sel {
			switch {
			case n.stage != nil:
				callAt[x] = stStart[i]
			case n.parallel:
				callAt[x] = ownEnd[i]
			default:
				callAt[x] = prev[i]
			}
		}
		clientIDs := make([]string, len(sel))
		arrCh := make([]float64, len(sel))
		for x := range clientIDs {
			if !s.noSpans {
				clientIDs[x] = s.id()
			}
			arrCh[x] = callAt[x] + s.hop()
		}
		chEnd := s.visit(ch, arrCh, clientIDs, tr, false)
		for x, i := range sel {
			ce := chEnd[x] + s.hop()
			if !s.noSpans {
				s.spans = append(s.spans, span{svc: n.name, pod: fmt.Sprintf("%s-%d", n.name, podOf[i]), id: clientIDs[x], parent: ids[i],
					kind: 3, start: callAt[x], end: ce, trace: traces[i], peer: ch.name})
			}
			switch {
			case n.stage != nil:
				stEnd[i] = math.Max(stEnd[i], ce)
				end[i] = math.Max(end[i], ce)
			case n.parallel:
				end[i] = math.Max(end[i], ce)
			default:
				prev[i], end[i] = ce, ce
			}
		}
	}
	for i := 0; i < N; i++ {
		end[i] += n.post
		if s.noSpans {
			continue
		}
		st := start[i]
		if s.queueInSpan {
			st = arr[i]
		}
		s.spans = append(s.spans, span{svc: n.name, pod: fmt.Sprintf("%s-%d", n.name, podOf[i]), id: ids[i], parent: parents[i],
			kind: 2, start: st, end: end[i], trace: traces[i]})
	}
	if s.rec != nil {
		s.rec(n, arr, start, ownEnd, end, podOf, traces)
	}
	return end
}

// run simulates requests arriving as a Poisson process at lam per second.
func run(root *node, lam float64, seconds float64, seed int64, queueInSpan bool) *simOut {
	return runWith(root, lam, seconds, seed, simOpts{queueInSpan: queueInSpan})
}

// runWith is run with options.
func runWith(root *node, lam float64, seconds float64, seed int64, o simOpts) *simOut {
	if o.hopJitter == 0 {
		o.hopJitter = envHopJitter()
	}
	s := &sim{rng: rand.New(rand.NewSource(seed)), simOpts: o}
	if o.hopJitter > 0 {
		s.hr = rand.New(rand.NewSource(seed*7919 + 13))
	}
	var arr []float64
	t := 0.0
	for {
		t += s.rng.ExpFloat64() * 1000 / lam
		if t > seconds*1000 {
			break
		}
		arr = append(arr, t)
	}
	n := len(arr)
	parents := make([]string, n)
	traces := make([]int, n)
	for i := range traces {
		traces[i] = i + 1
	}
	end := s.visit(root, arr, parents, traces, true)
	if o.nest.server || o.nest.client {
		s.nestSpans(o.nest)
	}
	out := &simOut{spans: s.spans}
	for i := range arr {
		out.totalMs = append(out.totalMs, end[i]-arr[i])
	}
	for _, sp := range s.spans {
		if sp.kind == 2 && sp.parent == "" {
			out.entryMs = append(out.entryMs, sp.end-sp.start)
		}
	}
	return out
}

// nestOpts say which nested spans nestSpans adds.
type nestOpts struct {
	server bool // every SERVER span gets a SERVER span inside it: a framework's handler span inside the HTTP instrumentation's
	client bool // every CLIENT span gets a CLIENT span around it: a gRPC client's span around the HTTP client's
	// bare: the inner CLIENT span carries no peer, only the outer one does
	bare bool
	// skew, ms: the inner SERVER span is placed this much later than inside the outer one (a runtime whose clock ticks
	// in whole milliseconds places the spans of one process up to a millisecond away from where they were)
	skew float64
}

// nestEps is how much inside the outer span the inner span starts and ends, ms.
const nestEps = 0.01

// nestSpans adds nested spans to the spans written so far, the way frameworks do. The outer SERVER span and the
// inner CLIENT span are the simulator's own (their ids, parents and times are unchanged: the callers' CLIENT spans
// are the parents of the outer SERVER span, the callees' SERVER spans the children of the inner CLIENT span), so a
// capture read with the nested spans merged is the same as the one without them. A new inner SERVER span takes over
// the CLIENT spans of its pod's visit; a new outer CLIENT span takes over as the parent of the inner one.
func (s *sim) nestSpans(o nestOpts) {
	orig := s.spans
	s.spans = make([]span, 0, 2*len(orig))
	innerOf := map[string]string{} // outer SERVER span id -> its inner span's id
	if o.server {
		for _, sp := range orig {
			if sp.kind == 2 {
				innerOf[sp.id] = s.id()
			}
		}
	}
	for _, sp := range orig {
		switch {
		case sp.kind == 2 && o.server:
			s.spans = append(s.spans, sp)
			in := sp
			in.id, in.parent = innerOf[sp.id], sp.id
			if sp.end-sp.start > 2*nestEps { // a shorter span has its inner span the same
				in.start, in.end = sp.start+nestEps, sp.end-nestEps
			}
			in.start, in.end = in.start+o.skew, in.end+o.skew
			s.spans = append(s.spans, in)
		case sp.kind == 3 && o.client:
			in := sp
			if in.parent != "" {
				if inner, ok := innerOf[in.parent]; ok {
					in.parent = inner
				}
			}
			wrap := in
			wrap.id, wrap.parent = s.id(), in.parent
			wrap.start, wrap.end = sp.start-nestEps, sp.end+nestEps
			in.parent = wrap.id
			if o.bare {
				in.peer, in.port = "", 0
			}
			s.spans = append(s.spans, wrap, in)
		case sp.kind == 3 && o.server:
			if inner, ok := innerOf[sp.parent]; ok {
				sp.parent = inner
			}
			s.spans = append(s.spans, sp)
		default:
			s.spans = append(s.spans, sp)
		}
	}
}

func writeOTLP(t *testing.T, spans []span) string {
	t.Helper()
	dir := t.TempDir()
	if err := writeOTLPTo(dir, spans); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeOTLPTo writes spans as a capture directory (dir/traces/spans.jsonl).
func writeOTLPTo(dir string, spans []span) error {
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "traces", "spans.jsonl"))
	if err != nil {
		return err
	}
	const base = 1_700_000_000_000_000_000
	ns := func(ms float64) uint64 { return uint64(base + ms*1e6) }
	startNs := func(sp span) uint64 {
		if sp.wholeMs {
			return base + uint64(sp.start)*1_000_000
		}
		return ns(sp.start)
	}
	// group by (svc, pod, scope)
	type key struct{ svc, pod, scope string }
	groups := map[key][]span{}
	for _, sp := range spans {
		k := key{sp.svc, sp.pod, sp.scope}
		groups[k] = append(groups[k], sp)
	}
	var keys []key
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool {
		return keys[a].svc+keys[a].pod+keys[a].scope < keys[b].svc+keys[b].pod+keys[b].scope
	})
	for _, k := range keys {
		ss := groups[k]
		for off := 0; off < len(ss); off += 500 {
			end := min(off+500, len(ss))
			var b strings.Builder
			scope := ""
			if k.scope != "" {
				scope = fmt.Sprintf(`"scope":{"name":%q},`, k.scope)
			}
			fmt.Fprintf(&b, `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}},{"key":"k8s.pod.name","value":{"stringValue":%q}}]},"scopeSpans":[{%s"spans":[`, k.svc, k.pod, scope)
			for i, sp := range ss[off:end] {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, `{"traceId":"%032x","spanId":%q,"parentSpanId":%q,"kind":%d,"startTimeUnixNano":"%d","endTimeUnixNano":"%d"`,
					sp.trace, sp.id, sp.parent, sp.kind, startNs(sp), ns(sp.end))
				if sp.name != "" {
					fmt.Fprintf(&b, `,"name":%q`, sp.name)
				}
				if sp.peer != "" && sp.port != 0 {
					fmt.Fprintf(&b, `,"attributes":[{"key":"server.address","value":{"stringValue":%q}},{"key":"server.port","value":{"intValue":"%d"}}]`,
						sp.peer, sp.port)
				} else if sp.peer != "" {
					fmt.Fprintf(&b, `,"attributes":[{"key":"server.address","value":{"stringValue":%q}}]`, sp.peer)
				} else if sp.route != "" {
					fmt.Fprintf(&b, `,"attributes":[{"key":"url.path","value":{"stringValue":%q}}]`, sp.route)
				}
				b.WriteByte('}')
			}
			b.WriteString("]}]}]}\n")
			if _, err := f.WriteString(b.String()); err != nil {
				f.Close()
				return err
			}
		}
	}
	return f.Close()
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func infer1(t *testing.T, spans []span) *infer.Inferred {
	t.Helper()
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return inf
}

func within(t *testing.T, what string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol*math.Abs(want) {
		t.Errorf("%s: got %.4g, want %.4g (tolerance %.0f%%)", what, got, want, 100*tol)
	}
}

func chain() *node {
	c := &node{name: "c", c: 1, pods: 1, mean: 10}
	b := &node{name: "b", c: 2, pods: 1, mean: 20, children: []*node{c}}
	return &node{name: "a", c: 1, pods: 1, mean: 8, children: []*node{b}}
}

// TestChainQueueOutsideSpan: spans start when a worker picks the request up.
func TestChainQueueOutsideSpan(t *testing.T) { checkChain(t, false) }

// TestChainQueueInsideSpan: spans start at arrival, so they include the wait.
func TestChainQueueInsideSpan(t *testing.T) { checkChain(t, true) }

func checkChain(t *testing.T, inSpan bool) {
	const lam = 50.0
	out := run(chain(), lam, 400, 7, inSpan)
	inf := infer1(t, out.spans)
	topo := inf.Topology
	a, b, c := topo.Index("a"), topo.Index("b"), topo.Index("c")
	if a < 0 || b < 0 || c < 0 {
		t.Fatalf("services: %v", topo.Names())
	}
	if len(topo.Services[a].Calls) != 1 || topo.Services[a].Calls[0].Child != b ||
		len(topo.Services[b].Calls) != 1 || topo.Services[b].Calls[0].Child != c {
		t.Fatalf("wrong call graph: %+v", topo.Services)
	}
	if _, ok := topo.Entry[a]; !ok || len(topo.Entry) != 1 {
		t.Fatalf("entry: %v", topo.Entry)
	}
	within(t, "external rate", inf.LamExtPerMs*1000, lam, 0.04)
	within(t, "a service time", topo.Services[a].S, 8, 0.07)
	within(t, "b service time", topo.Services[b].S, 20, 0.07)
	within(t, "c service time", topo.Services[c].S, 10, 0.07)
	within(t, "a cs2", topo.Services[a].CS2, 1, 0.15)
	within(t, "hop", topo.Services[b].Hop, 2*hopMs, 0.3)
	if got := topo.Services[b].Servers * topo.Services[b].Workers; got != 2 {
		if inSpan {
			t.Errorf("b workers: got %d, want 2 (read from queueing)", got)
		} else if got < 1 || got > 2 {
			t.Errorf("b workers: got %d, a lower bound of 2 expected", got)
		}
	}
	// the model's mean vs the simulation's (arrival to end, so the entry's own wait counts)
	base, err := model.Analytic(topo, inf.LamExtPerMs, nil)
	if err != nil || !base.Stable {
		t.Fatalf("model: %v stable=%v", err, base != nil && base.Stable)
	}
	within(t, "entry latency (model vs simulation)", base.L[a], mean(out.totalMs), 0.05)

	// a change of +8 ms of fixed extra time in b. The truth is the mean over 6 re-simulations with
	// common random numbers; one run at this load has a standard deviation of about 2 ms.
	const shift = 8.0
	var obs float64
	for seed := int64(1); seed <= 6; seed++ {
		changed := chain()
		changed.children[0].shift = shift
		o1 := run(chain(), lam, 400, seed, inSpan)
		o2 := run(changed, lam, 400, seed, inSpan)
		obs += (mean(o2.totalMs) - mean(o1.totalMs)) / 6
	}
	ds := make([]float64, len(topo.Services))
	sh := make([]bool, len(topo.Services))
	ds[b], sh[b] = shift, true
	d, err := model.ChangeShift(topo, inf.LamExtPerMs, ds, sh)
	if err != nil || !d.Stable {
		t.Fatalf("change: %v", err)
	}
	if math.Abs(d.DL[a]-obs) > 0.10*obs+1.0 {
		t.Errorf("change of the entry latency: model %+.2f ms, simulation %+.2f ms", d.DL[a], obs)
	}
}

func TestParallelFanOutAndReplicas(t *testing.T) {
	b := &node{name: "b", c: 1, pods: 2, mean: 30}
	c := &node{name: "c", c: 1, pods: 1, mean: 15}
	a := &node{name: "a", c: 4, pods: 1, mean: 3, parallel: true, children: []*node{b, c}}
	out := run(a, 40, 400, 11, false)
	inf := infer1(t, out.spans)
	topo := inf.Topology
	ai := topo.Index("a")
	if !topo.Services[ai].Parallel || len(topo.Services[ai].Calls) != 2 {
		t.Fatalf("a should be a parallel fan-out of 2: %+v", topo.Services[ai])
	}
	bi := topo.Index("b")
	if topo.Services[bi].Servers != 2 || topo.Services[bi].Routing != model.Random {
		t.Errorf("b should have 2 replicas with random routing: %+v", topo.Services[bi])
	}
	if st := inf.Stats[bi]; st.UnevenSplit || topo.Services[bi].Shares != nil || st.SplitP == nil {
		t.Errorf("b's random split should be tested and read as even: %+v", st)
	}
	base, _ := model.Analytic(topo, inf.LamExtPerMs, nil)
	if !base.Stable {
		t.Fatal("unstable")
	}
	// the exponential E[max] approximation is within a few percent for this fan-out
	within(t, "entry latency with a parallel fan-out", base.L[ai], mean(out.totalMs), 0.08)
}

func TestConditionalAndRepeatedCalls(t *testing.T) {
	leaf := &node{name: "d", c: 4, pods: 1, mean: 5}
	a := &node{name: "a", c: 4, pods: 1, mean: 2, children: []*node{leaf, leaf}}
	out := run(a, 30, 300, 5, false)
	inf := infer1(t, out.spans)
	topo := inf.Topology
	ai := topo.Index("a")
	if got := len(topo.Services[ai].Calls); got != 2 {
		t.Fatalf("a calls d twice per visit: got %d calls", got)
	}
	v := topo.VisitRatios()
	within(t, "visits to d per request", v[topo.Index("d")], 2, 0.01)
}

func TestTooFewRequestsAndNoServerSpans(t *testing.T) {
	out := run(chain(), 5, 4, 3, false) // ~20 requests
	d, _ := otlp.ReadDir(writeOTLP(t, out.spans))
	if _, err := infer.Run(d, infer.Options{}); err == nil || !strings.Contains(err.Error(), "entry requests") {
		t.Errorf("expected a too-few-requests error, got %v", err)
	}
	only := []span{{svc: "x", pod: "x-0", id: "0000000000000001", kind: 3, start: 0, end: 1, trace: 1}}
	d2, err := otlp.ReadDir(writeOTLP(t, only))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := infer.Run(d2, infer.Options{}); err == nil || !strings.Contains(err.Error(), "SERVER") {
		t.Errorf("expected a no-server-spans error, got %v", err)
	}
}

// TestWriteFixture writes the small capture the CLI tests read:
//
//	WRITE_FIXTURE=../../testdata/capture-chain go test -count=1 ./internal/infer -run WriteFixture
func TestWriteFixture(t *testing.T) {
	dir := os.Getenv("WRITE_FIXTURE")
	if dir == "" {
		t.Skip("WRITE_FIXTURE not set")
	}
	out := run(chain(), 25, 120, 21, false)
	src := writeOTLP(t, out.spans)
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(src, "traces", "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(raw)
	zw.Close()
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
