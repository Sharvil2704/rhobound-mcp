// SPDX-License-Identifier: Apache-2.0

package infer_test

// An event-driven simulator for services whose visits make a varying number of calls to one callee, and take several
// call shapes. The batch simulator in sim_test.go gives every call of a node its own queue, so a callee called twice by
// one visit does not queue the second call behind other visits' first calls; here the callee has ONE first-come,
// first-served queue for all calls. It is the ground truth of the tests of variable call counts: the answers it gives do
// not come from the code under test.

import (
	"container/heap"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// desType is one kind of request of the service A.
type desType struct {
	p    float64 // share of the requests
	own  float64 // mean own time before the calls, ms (exponential; 0: none)
	glue float64 // constant time after the calls, ms
	mode string  // "seq" (calls one after another), "par" (all at once) or "none" (no calls)
	n    []int   // the number of calls is drawn uniformly from these
}

// desCfg is a run of A -> B: A has ca workers (held through its own time before the calls), B has cb workers and one
// FIFO queue for every call, with exponential service times of mean sb ms.
type desCfg struct {
	types   []desType
	rho     float64 // the utilization of B; the arrival rate follows from it
	rhoA    float64 // when set, the utilization of A instead (its arrival rate)
	ca, cb  int
	sb      float64
	seconds float64
	seed    int64
}

// meanCalls is the mean number of calls per request.
func (c desCfg) meanCalls() float64 {
	m := 0.0
	for _, t := range c.types {
		s := 0.0
		for _, n := range t.n {
			s += float64(n)
		}
		m += t.p * s / float64(len(t.n))
	}
	return m
}

type desEvent struct {
	t    float64
	seq  int
	kind int
	r    *desReq
	j    int
	job  *desJob
}

type desQueue []desEvent

func (q desQueue) Len() int { return len(q) }
func (q desQueue) Less(i, j int) bool {
	if q[i].t != q[j].t {
		return q[i].t < q[j].t
	}
	return q[i].seq < q[j].seq
}
func (q desQueue) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *desQueue) Push(x interface{}) { *q = append(*q, x.(desEvent)) }
func (q *desQueue) Pop() interface{} {
	o := *q
	v := o[len(o)-1]
	*q = o[:len(o)-1]
	return v
}

const (
	evArrive = iota
	evADone
	evSend
	evBArrive
	evBDone
	evResp
)

type desReq struct {
	trace      int
	id         string
	arr, start float64
	typ        int
	n, done    int
	own        float64
}

type desJob struct {
	r             *desReq
	cid, sid      string
	csend, bstart float64
}

// desRun simulates the configuration and writes the spans. totalMs are the requests' times from arrival to the end of
// the server span of A, a worker's wait included; bMs the callee's latencies as A's client spans measured them.
func desRun(c desCfg) (spans []span, totalMs, bMs []float64) {
	rng := rand.New(rand.NewSource(c.seed))
	lamB := c.rho * float64(c.cb) / c.sb // per ms
	lamA := lamB / c.meanCalls()
	if c.rhoA > 0 {
		own := 0.0
		for _, t := range c.types {
			own += t.p * t.own
		}
		lamA = c.rhoA * float64(c.ca) / own
	}
	var ev desQueue
	seq := 0
	push := func(t float64, kind int, r *desReq, j int, job *desJob) {
		seq++
		heap.Push(&ev, desEvent{t, seq, kind, r, j, job})
	}
	next := 0
	id := func() string { next++; return fmt.Sprintf("%016x", next) }
	for t, k := 0.0, 0; ; k++ {
		t += rng.ExpFloat64() / lamA
		if t > c.seconds*1000 {
			break
		}
		push(t, evArrive, &desReq{trace: k + 1}, 0, nil)
	}
	busyA, busyB := 0, 0
	var waitA []*desReq
	var waitB []*desJob
	startA := func(now float64, r *desReq) {
		busyA++
		r.start = now
		push(now+r.own, evADone, r, 0, nil)
	}
	startB := func(now float64, j *desJob) {
		busyB++
		j.bstart = now
		push(now+rng.ExpFloat64()*c.sb, evBDone, nil, 0, j)
	}
	finish := func(now float64, r *desReq) {
		end := now + c.types[r.typ].glue
		spans = append(spans, span{svc: "A", pod: "a-0", id: r.id, kind: 2, start: r.start, end: end, trace: r.trace})
		totalMs = append(totalMs, end-r.arr)
	}
	for ev.Len() > 0 {
		e := heap.Pop(&ev).(desEvent)
		now := e.t
		switch e.kind {
		case evArrive:
			r := e.r
			r.arr, r.id = now, id()
			u, acc := rng.Float64(), 0.0
			for i, t := range c.types {
				acc += t.p
				r.typ = i
				if u <= acc {
					break
				}
			}
			t := c.types[r.typ]
			r.n = t.n[rng.Intn(len(t.n))]
			if t.own > 0 {
				r.own = rng.ExpFloat64() * t.own
			}
			if busyA < c.ca {
				startA(now, r)
			} else {
				waitA = append(waitA, r)
			}
		case evADone:
			r := e.r
			busyA--
			if len(waitA) > 0 {
				startA(now, waitA[0])
				waitA = waitA[1:]
			}
			if c.types[r.typ].mode == "none" || r.n == 0 {
				finish(now, r)
				continue
			}
			push(now, evSend, r, 0, nil)
		case evSend:
			r := e.r
			last := e.j + 1
			if c.types[r.typ].mode == "par" {
				last = r.n // all at once
			}
			for j := e.j; j < last; j++ {
				push(now+hopMs, evBArrive, nil, 0, &desJob{r: r, cid: id(), sid: id(), csend: now})
			}
		case evBArrive:
			if busyB < c.cb {
				startB(now, e.job)
			} else {
				waitB = append(waitB, e.job)
			}
		case evBDone:
			busyB--
			push(now+hopMs, evResp, nil, 0, e.job)
			if len(waitB) > 0 {
				startB(now, waitB[0])
				waitB = waitB[1:]
			}
		case evResp:
			j := e.job
			r := j.r
			spans = append(spans,
				span{svc: "A", pod: "a-0", id: j.cid, parent: r.id, kind: 3, start: j.csend, end: now, trace: r.trace, peer: "B"},
				span{svc: "B", pod: "b-0", id: j.sid, parent: j.cid, kind: 2, start: j.bstart, end: now - hopMs, trace: r.trace})
			bMs = append(bMs, now-j.csend)
			r.done++
			switch {
			case c.types[r.typ].mode == "seq" && r.done < r.n:
				push(now+0.02, evSend, r, r.done, nil)
			case r.done == r.n:
				finish(now, r)
			}
		}
	}
	return spans, totalMs, bMs
}

// desPredict reads the spans and solves the model.
func desPredict(t *testing.T, spans []span, workers map[string]int) (*infer.Inferred, *model.Result) {
	t.Helper()
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{Workers: workers})
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	res, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Default)
	if err != nil || !res.Stable {
		t.Fatalf("model: %v", err)
	}
	return inf, res
}

// checkLoad: the arrival rate at the callee is the caller's rate times the mean calls per visit, the one exact claim of
// the mean count.
func checkLoad(t *testing.T, inf *infer.Inferred, caller, callee string, perVisit float64) {
	t.Helper()
	topo := inf.Topology
	v := topo.VisitRatios()
	if got := v[topo.Index(callee)] / v[topo.Index(caller)]; math.Abs(got-perVisit) > 1e-9*perVisit {
		t.Errorf("visits to %s per visit of %s: model %.12g, observed %.12g", callee, caller, got, perVisit)
	}
}

// observedMean is the mean calls per visit of A to B in the spans.
func observedMean(spans []span) float64 {
	calls, visits := 0, 0
	for _, sp := range spans {
		if sp.svc == "A" && sp.kind == 3 {
			calls++
		}
		if sp.svc == "A" && sp.kind == 2 {
			visits++
		}
	}
	return float64(calls) / float64(visits)
}

func desTypes(name string) []desType {
	switch name {
	case "seq": // 1, 3, or 2 or 4 calls, one after another
		return []desType{{0.5, 0.2, 0.05, "seq", []int{1}}, {0.3, 0.5, 0.05, "seq", []int{3}}, {0.2, 0.3, 0.05, "seq", []int{2, 4}}}
	case "heavy": // 1 call on 90% of the requests, 20 on 10%
		return []desType{{1, 0.3, 0.05, "seq", []int{1, 1, 1, 1, 1, 1, 1, 1, 1, 20}}}
	case "heavy2": // 1 call on 70%, 8 on 30%
		return []desType{{1, 0.3, 0.05, "seq", []int{1, 1, 1, 1, 1, 1, 1, 8, 8, 8}}}
	case "mix": // a call, three in a row, or four at once
		return []desType{{0.5, 0.2, 0.05, "seq", []int{1}}, {0.3, 0.5, 0.05, "seq", []int{3}}, {0.2, 0.3, 0.05, "par", []int{4}}}
	case "mixvar": // single, a run of 2-4, a fan-out of 2-5, no call
		return []desType{{0.4, 0.2, 0.05, "seq", []int{1}}, {0.2, 0.5, 0.05, "seq", []int{2, 3, 4}},
			{0.2, 0.3, 0.05, "par", []int{2, 3, 4, 5}}, {0.2, 0, 0, "none", []int{0}}}
	case "fan4": // four calls at once, always
		return []desType{{1, 0.3, 0.05, "par", []int{4}}}
	case "mixA": // a caller with one worker: a call, two calls, or a long stretch of own work and no call
		return []desType{{0.5, 3, 0.5, "seq", []int{1}}, {0.45, 6, 0.5, "seq", []int{2}}, {0.05, 60, 0, "none", []int{0}}}
	}
	panic(name)
}

// desRel is the relative error of the model against the simulation, in percent.
func desRel(model, sim float64) float64 { return 100 * (model/sim - 1) }

// TestVariableCountsSequentialAgainstTheSimulator: a caller that makes its calls one after another, a different number on
// different requests. The callee's load is exact, and over four runs of each setting the caller's and the callee's mean
// latencies are unbiased (the mean of the runs within 3%; 16 runs of the noisiest setting gave -0.6%) and each run within
// 10% of its simulation, for counts spread over 1-4 and for heavy tails (1 or 20; 1 or 8), with 1 and 4 workers. A single
// run's error is its estimation noise: a queue with bursts of 20 calls moves the mean by up to 7% between seeds.
func TestVariableCountsSequentialAgainstTheSimulator(t *testing.T) {
	for _, c := range []struct {
		sc  string
		cb  int
		rho float64
	}{{"seq", 1, 0.6}, {"seq", 4, 0.6}, {"heavy", 1, 0.3}, {"heavy", 4, 0.6}, {"heavy2", 1, 0.3}, {"heavy2", 4, 0.6}} {
		what := fmt.Sprintf("%s, %d workers, B %.0f%% busy", c.sc, c.cb, 100*c.rho)
		var errA, errB []float64
		runs, secs := 8, 200.0 // a single worker takes more runs: its queue is the noisiest
		if c.cb > 1 {
			runs, secs = 4, 100
		}
		for seed := int64(1); seed <= int64(runs); seed++ {
			cfg := desCfg{types: desTypes(c.sc), rho: c.rho, ca: 1_000_000, cb: c.cb, sb: 10, seconds: secs, seed: seed}
			spans, total, bMs := desRun(cfg)
			inf, res := desPredict(t, spans, map[string]int{"B": c.cb})
			topo := inf.Topology
			if !hasWarning(inf, "only their mean") {
				t.Errorf("%s: no warning that the mean count enters: %v", what, inf.Warnings)
			}
			vc := inf.Stats[topo.Index("A")].VariableCalls
			obs := observedMean(spans)
			if len(vc) != 1 || vc[0].Callee != "B" || vc[0].ModelledAs != infer.ModelledMean || math.Abs(vc[0].MeanPerVisit-obs) > 1e-12 {
				t.Errorf("%s: variable calls %+v, observed mean %.6f", what, vc, obs)
			}
			checkLoad(t, inf, "A", "B", obs)
			errA = append(errA, desRel(res.L[topo.Index("A")], mean(total)))
			errB = append(errB, desRel(res.L[topo.Index("B")]+topo.Services[topo.Index("B")].Hop, mean(bMs)))
		}
		for _, e := range []struct {
			who  string
			errs []float64
		}{{"A", errA}, {"B", errB}} {
			if m := mean(e.errs); math.Abs(m) > 3 {
				t.Errorf("%s: %s is biased by %+.1f%% (runs %+.1f)", what, e.who, m, e.errs)
			}
			for _, x := range e.errs {
				if math.Abs(x) > 10 {
					t.Errorf("%s: %s is off by %+.1f%% in a run (runs %+.1f)", what, e.who, x, e.errs)
					break
				}
			}
		}
	}
}

// TestCallShapesMixtureAgainstTheSimulator: requests of different kinds (one call, a run of three, four at once; or
// also a run of 2-4, a fan-out of 2-5 and none) are a mixture of call shapes. The callee's load is exact, the caller's
// and callee's latencies within 4% of the simulation when the callee has 8 workers.
func TestCallShapesMixtureAgainstTheSimulator(t *testing.T) {
	for _, c := range []struct {
		sc    string
		rho   float64
		shape int // call shapes kept
	}{{"mix", 0.2, 3}, {"mix", 0.4, 3}, {"mixvar", 0.2, 8}, {"mixvar", 0.4, 8}} {
		what := fmt.Sprintf("%s, 8 workers, B %.0f%% busy", c.sc, 100*c.rho)
		cfg := desCfg{types: desTypes(c.sc), rho: c.rho, ca: 1_000_000, cb: 8, sb: 10, seconds: 200, seed: 5}
		spans, total, bMs := desRun(cfg)
		inf, res := desPredict(t, spans, map[string]int{"B": 8})
		topo := inf.Topology
		a := inf.Stats[topo.Index("A")]
		if len(a.CallShapes) != c.shape {
			t.Errorf("%s: %d call shapes, want %d: %+v", what, len(a.CallShapes), c.shape, a.CallShapes)
		}
		if len(a.VariableCalls) != 1 || a.VariableCalls[0].ModelledAs != infer.ModelledMixture {
			t.Errorf("%s: variable calls %+v, want B as a mixture of call shapes", what, a.VariableCalls)
		}
		if !hasWarning(inf, "modelled as a mixture") {
			t.Errorf("%s: the mixture is not reported: %v", what, inf.Warnings)
		}
		if topo.Services[topo.Index("A")].Parallel {
			t.Errorf("%s: A is modelled as one parallel fan-out", what)
		}
		checkLoad(t, inf, "A", "B", observedMean(spans))
		within(t, what+": A", res.L[topo.Index("A")], mean(total), 0.04)
		within(t, what+": B as A saw it", res.L[topo.Index("B")]+topo.Services[topo.Index("B")].Hop, mean(bMs), 0.04)
	}
}

// desPredictPlain is desPredict with --plain: the basic model, which takes every call as an arrival of its own.
func desPredictPlain(t *testing.T, spans []span, workers map[string]int) (*infer.Inferred, *model.Result) {
	t.Helper()
	d, err := otlp.ReadDir(writeOTLP(t, spans))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{Workers: workers, Plain: true})
	if err != nil {
		t.Fatalf("infer: %v", err)
	}
	res, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, model.Plain)
	if err != nil || !res.Stable {
		t.Fatalf("model: %v", err)
	}
	return inf, res
}

// codedBatch reports whether the inference coded a model.batch_arrivals warning (a doubt of the fit).
func codedBatch(inf *infer.Inferred) bool {
	for _, c := range inf.WarningCodes {
		if c == contract.ModelBatchArrivals {
			return true
		}
	}
	return false
}

// noBatch is the refined model without the batch term: what the model was before it took calls made at once as batches.
var noBatch = model.Config{ArrivalWeight: model.Default.ArrivalWeight, MatchedMax: true}

// TestBatchArrivalsAreWarnedOf: calls made at once reach the callee together and queue behind each other. The refined
// model takes them as one batch (model.Config.Batch: an M^X/G/c queue at the callee, and the caller waits for the last
// of them), so with one worker A is within 8% of the simulation at 30% and 40% load, and the report notes how the batch
// is modelled, with no model.batch_arrivals doubt. Without the batch term (each call an arrival of its own) the callee
// is about 60% too fast whatever the load. --plain keeps the model.batch_arrivals doubt: the basic model has no batch
// term. With a quiet callee of 8 workers there is nothing to say.
func TestBatchArrivalsAreWarnedOf(t *testing.T) {
	for _, rho := range []float64{0.4, 0.3} {
		cfg := desCfg{types: desTypes("fan4"), rho: rho, ca: 1_000_000, cb: 1, sb: 10, seconds: 200, seed: 3}
		spans, total, _ := desRun(cfg)
		inf, res := desPredict(t, spans, map[string]int{"B": 1})
		topo := inf.Topology
		if !hasWarning(inf, "A calls B 4 times at once (on 100% of its visits); B has 1 worker and is ") ||
			!hasWarning(inf, "The model takes the 4 calls as one batch") || codedBatch(inf) {
			t.Errorf("B %.0f%% busy: want the batch noted as modelled, uncoded: %v %v", 100*rho, inf.Warnings, inf.WarningCodes)
		}
		if e := desRel(res.L[topo.Index("A")], mean(total)); math.Abs(e) > 8 {
			t.Errorf("B %.0f%% busy: A is %+.1f%% off its simulation with the batch term", 100*rho, e)
		}
		old, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, noBatch)
		if err != nil || !old.Stable {
			t.Fatalf("model: %v", err)
		}
		if e := desRel(old.L[topo.Index("A")], mean(total)); e > -40 {
			t.Errorf("B %.0f%% busy: without the batch term A is %+.1f%% off its simulation; it should be far below (about -60%%)", 100*rho, e)
		}
		if inf, _ = desPredictPlain(t, spans, map[string]int{"B": 1}); !codedBatch(inf) {
			t.Errorf("--plain, B %.0f%% busy: no model.batch_arrivals doubt: %v", 100*rho, inf.Warnings)
		}
	}
	// the mixture's groups of calls at once are checked too, and named by the service whose calls they are
	cfg := desCfg{types: desTypes("mix"), rho: 0.3, ca: 1_000_000, cb: 4, sb: 10, seconds: 200, seed: 3}
	spans, _, _ := desRun(cfg)
	inf, _ := desPredict(t, spans, map[string]int{"B": 4})
	if !hasWarning(inf, "A calls B 4 times at once (on 20% of its visits); B has 4 workers and is ") {
		t.Errorf("no batch warning in the mixture (four at once, 4 workers, 30%% busy): %v", inf.Warnings)
	}
	// 8 workers at 20%: batches of 4 and 5 do not matter yet
	cfg = desCfg{types: desTypes("mixvar"), rho: 0.2, ca: 1_000_000, cb: 8, sb: 10, seconds: 200, seed: 3}
	spans, _, _ = desRun(cfg)
	inf, _ = desPredict(t, spans, map[string]int{"B": 8})
	if hasWarning(inf, "times at once") {
		t.Errorf("a batch warning for 8 workers at 20%%: %v", inf.Warnings)
	}
}

// TestVariableCountsOfABusyCaller: a caller with one worker at 70%, three kinds of request (one call, two calls, a long
// stretch of own work and none): the calls' counts enter as their mean and the caller's own queue is the model's.
func TestVariableCountsOfABusyCaller(t *testing.T) {
	var errs []float64
	for seed := int64(1); seed <= 3; seed++ {
		cfg := desCfg{types: desTypes("mixA"), rhoA: 0.7, ca: 1, cb: 8, sb: 1, seconds: 300, seed: seed}
		spans, total, _ := desRun(cfg)
		inf, res := desPredict(t, spans, map[string]int{"A": 1, "B": 8})
		checkLoad(t, inf, "A", "B", observedMean(spans))
		errs = append(errs, desRel(res.L[inf.Topology.Index("A")], mean(total)))
	}
	t.Logf("a caller with one worker at 70%%: runs %+.1f%%", errs)
	if m := mean(errs); math.Abs(m) > 4 {
		t.Errorf("a caller with one worker at 70%%: biased by %+.1f%% (runs %+.1f)", m, errs)
	}
	for _, e := range errs {
		if math.Abs(e) > 8 {
			t.Errorf("a caller with one worker at 70%%: off by %+.1f%% in a run (runs %+.1f)", e, errs)
		}
	}
}

// TestBatchChangeAgainstTheSimulator: four calls at once to one worker, B's service time +20% at the same arrival
// rate: the change of A (the batch's makespan) against the simulator on the same seeds, mean of four runs, within 10%.
// Without the batch term, taking the calls as arrivals of their own, the model misses most of it.
func TestBatchChangeAgainstTheSimulator(t *testing.T) {
	var pred, plain, truth []float64
	for seed := int64(1); seed <= 4; seed++ {
		base := desCfg{types: desTypes("fan4"), rho: 0.3, ca: 1_000_000, cb: 1, sb: 10, seconds: 100, seed: seed}
		spans, total0, _ := desRun(base)
		inf, _ := desPredict(t, spans, map[string]int{"B": 1})
		for _, pl := range []bool{false, true} {
			cfg := model.Default
			if pl {
				cfg = noBatch
			}
			topo := inf.Topology
			n, bi, ai := len(topo.Services), topo.Index("B"), topo.Index("A")
			p := model.Patch{DS: make([]float64, n), Shift: make([]bool, n), Glue: make([]float64, n)}
			p.DS[bi] = 0.2 * topo.Services[bi].S
			d, err := model.ChangePatchWith(topo, inf.LamExtPerMs, p, cfg)
			if err != nil || !d.Stable {
				t.Fatalf("change: %v", err)
			}
			if pl {
				plain = append(plain, d.Patched.L[ai]-d.Base.L[ai])
			} else {
				pred = append(pred, d.Patched.L[ai]-d.Base.L[ai])
			}
		}
		slow := base
		slow.sb, slow.rho = 12, 0.3*1.2
		_, total1, _ := desRun(slow)
		truth = append(truth, mean(total1)-mean(total0))
	}
	t.Logf("four at once, one worker, 30%% busy, B +20%%: change of A %.2f ms (without the batch term %.2f), simulated %.2f",
		mean(pred), mean(plain), mean(truth))
	within(t, "batch: change of A", mean(pred), mean(truth), 0.10)
	if mean(plain) > 0.6*mean(truth) {
		t.Errorf("without the batch term the change %.2f ms should fall well short of the simulated %.2f", mean(plain), mean(truth))
	}
}

// TestVariableCountsChangeAgainstTheSimulator: the tool's purpose is the change when one service's time changes. B's
// service time +20%, the arrival rate kept: the change of A's mean latency, and of B's as A sees it, against the
// simulator run with the slower B on the same seeds, mean of four runs. A that calls B a different number of times (a
// mean count) or in several shapes (a mixture) passes the change on through the calls: within 12% of the change.
func TestVariableCountsChangeAgainstTheSimulator(t *testing.T) {
	for _, c := range []struct {
		sc  string
		cb  int
		rho float64
	}{{"seq", 4, 0.5}, {"mixvar", 8, 0.3}} {
		var predA, predB, truthA, truthB []float64
		for seed := int64(1); seed <= 4; seed++ {
			base := desCfg{types: desTypes(c.sc), rho: c.rho, ca: 1_000_000, cb: c.cb, sb: 10, seconds: 100, seed: seed}
			spans, total0, b0 := desRun(base)
			inf, _ := desPredict(t, spans, map[string]int{"B": c.cb})
			topo := inf.Topology
			n, bi, ai := len(topo.Services), topo.Index("B"), topo.Index("A")
			p := model.Patch{DS: make([]float64, n), Shift: make([]bool, n), Glue: make([]float64, n)}
			p.DS[bi] = 0.2 * topo.Services[bi].S
			d, err := model.ChangePatchWith(topo, inf.LamExtPerMs, p, model.Default)
			if err != nil || !d.Stable {
				t.Fatalf("change: %v", err)
			}
			predA = append(predA, d.Patched.L[ai]-d.Base.L[ai])
			predB = append(predB, d.Patched.L[bi]-d.Base.L[bi])
			slow := base
			slow.sb, slow.rho = 12, c.rho*1.2 // the same arrival rate at B: rho * workers / sb
			_, total1, b1 := desRun(slow)
			truthA = append(truthA, mean(total1)-mean(total0))
			truthB = append(truthB, mean(b1)-mean(b0))
		}
		what := fmt.Sprintf("%s, %d workers, B %.0f%% busy, B +20%%", c.sc, c.cb, 100*c.rho)
		within(t, what+": change of A", mean(predA), mean(truthA), 0.12)
		within(t, what+": change of B", mean(predB), mean(truthB), 0.12)
	}
}
