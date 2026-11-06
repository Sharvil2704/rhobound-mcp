// SPDX-License-Identifier: Apache-2.0

// Package infer builds a model.Topology from traces.
//
// What it reads from the spans: which service calls which (parent and child
// spans), how often (a fixed count, one more call on a share of visits, or the mean count),
// whether the calls of a visit overlap (parallel), follow one another
// (sequential) or run in stages (a group at once, then the next; see
// structure.go), the network time per call, the arrival rate, the
// replicas (distinct pods) and how evenly they share the visits (split.go),
// and the workers per pod and every visit's wait for
// a worker, from each pod's first-come-first-served queue rebuilt from the
// spans (fifo.go). A visit arrives at its span's start and holds a worker from
// its start of service until its first call out, or its end, clipped to the
// span: the service time is that part. Calls a caller sends after its response
// (at least 95% of them start once its span has ended) are async: they load
// the callee, but are not on the caller's latency path and free no worker.
// What a message consumer calls is not a call of the visit that produced the
// message: a service it calls gets those visits as entries, counted apart. The
// rest of the span that is not in a call (work between or
// after the calls: the client library, writing the response) holds no worker;
// it is the service's glue, a pure delay in the model, as in the reference.
// The earlier estimator (service time from the visits that found a free
// worker, workers from where the own time rises with the number already busy)
// stays selectable for comparison: Options.Estimator or RHOBOUND_ESTIMATOR=rise.
//
// As in the reference, health and readiness probes are dropped by the reader,
// a trace that lost a span (a parent that is not in the capture, or a call to
// a service with no span of it under the call) is kept out of the entries and
// the call structure but counts in the queues and the rates, and more than 5%
// of such traces is refused. Unlike the reference, a SERVER span whose caller
// is outside the capture (an ingress, a gateway, a browser), of a service most
// of whose server spans have no caller in the capture, is an entry, as is a
// CONSUMER span whose producer is outside it, however many a trace holds, when
// the trace's top is not in the capture (topLoss). Calls per visit
// that are not k or k + 1 enter as their mean count, and visits that take several call shapes (calls that overlap on
// some visits only, a parallel branch called on some visits, a varying number of calls at once) as a mixture of the
// shapes (counts.go); a mixture of more than maxShapes shapes is refused by name.
//
// What it does not do: read sampling rates, see a wait that happens before the
// span starts, rebuild a queue that is not first come, first served or that
// keeps its worker across calls, or match calls into a service that does not
// propagate the trace context. See docs/REQUIREMENTS.md.
package infer

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// Options tune the inference.
type Options struct {
	// KeptShare is the share of requests the capture holds (1 = all). Rates
	// are divided by it. Sampling is not detected from the traces.
	KeptShare float64
	// Workers overrides the inferred workers per pod, by service name.
	Workers map[string]int
	// MinRequests is the fewest entry requests the capture must hold.
	MinRequests int
	// Plain reads the wait the model predicts outside a callee's spans (what the network time of a call is read against, see
	// hops.go) with the basic model (Poisson arrivals) instead of the refined one.
	Plain bool
	// Estimator picks how workers and service times are read: EstimatorFIFO
	// (the FIFO queue reconstruction per pod, the default) or EstimatorRise (own
	// time by visits busy at arrival). Empty: the RHOBOUND_ESTIMATOR variable,
	// else EstimatorFIFO.
	Estimator string
	// KeepSamples keeps each service's per-visit samples in Inferred.Samples, for the standard errors of an A/B
	// difference (internal/abdiff). It changes no other result.
	KeepSamples bool
}

// Samples are one service's per-visit observations, kept with Options.KeepSamples: what the batch-means standard
// errors of an A/B difference are computed from.
type Samples struct {
	// Own are the service times of the visits (release minus start of service, ms), in order of arrival: the FIFO
	// queue rebuild's, so empty with the rise estimator.
	Own []float64
	// Glue are the glue times of the visits of complete traces (ms), in order of arrival.
	Glue []float64
	// Calls are the mean calls per visit the visits wait for, by callee, over the visits of complete traces.
	Calls map[string]float64
	// CallsSq are the mean squared calls per visit, by callee, over the same visits (the spread of the calls per visit,
	// for the standard error of Calls), and CallVisits the number of those visits.
	CallsSq    map[string]float64
	CallVisits int64
	// Visits are the visits counted; Errors those whose SERVER span has an error status or an HTTP status of 500 or
	// more.
	Visits, Errors int64
	// RouteOwn are Own's values by route (ServiceStats.Routes), each in order of arrival; nil for a service without
	// routes.
	RouteOwn map[string][]float64
}

// ServiceStats are the observations behind one service of the topology.
type ServiceStats struct {
	Name                string   `json:"name"`
	External            bool     `json:"external,omitempty"`
	Entry               bool     `json:"entry,omitempty"`
	Entries             int64    `json:"entries,omitempty"`          // entry visits (no caller's visit in the capture)
	ConsumerEntries     int64    `json:"consumer_entries,omitempty"` // ... of which reached through a message consumer (a CONSUMER span above)
	Visits              int64    `json:"visits"`
	RateRPS             float64  `json:"rate_rps"`
	Pods                int      `json:"pods"`
	WorkersPerPod       int      `json:"workers_per_pod"`
	WorkersInferred     bool     `json:"workers_inferred"`
	OwnMeanMs           float64  `json:"own_mean_ms"`               // service time: the worker-holding part, up to the first call out
	OwnCS2              float64  `json:"own_cs2"`                   // the model's: 1 when the measured one is within 3 standard errors of 1
	OwnCS2Measured      float64  `json:"own_cs2_measured"`          // as measured
	CS2Snapped          bool     `json:"own_cs2_snapped,omitempty"` // the measured cs2 was taken as 1
	GlueMs              float64  `json:"glue_ms"`                   // own time after the first call out, holding no worker
	GlueCS2             float64  `json:"glue_cs2,omitempty"`
	HopMs               float64  `json:"hop_ms"`
	ObservedWaitMs      float64  `json:"observed_wait_ms"`      // mean(client - server) - hop: queueing outside the span
	InSpanWaitMs        float64  `json:"in_span_wait_ms"`       // mean own time minus the service time: the wait for a worker inside the span
	WorkersFromQueueing bool     `json:"workers_from_queueing"` // workers identified from the requests that waited, not a lower bound
	ObservedServerMs    float64  `json:"observed_server_ms"`    // mean server span
	ObservedClientMs    float64  `json:"observed_client_ms"`    // mean as the callers' client spans saw it
	ParallelShare       float64  `json:"parallel_share"`        // share of multi-call visits whose calls overlap
	Stages              int      `json:"stages,omitempty"`      // calls in this many stages, each after the last ends
	Stage               bool     `json:"stage,omitempty"`       // a stage: calls a service makes at once, a zero-time delay node
	BranchCorr          float64  `json:"branch_corr,omitempty"` // correlation between parallel branches' latencies
	Images              []string `json:"images,omitempty"`
	// HopFromMean: on some edge that calls this service the hop (HopMs: the network time of a call into it, the mean
	// over its edges, hops.go) is the mean time between a client span and the server span under it less the model's
	// wait, not the quietest tenth of that time. The report's seen wait for this service, and the hop's part of its
	// seen ms, are then set by the hop and do not check it.
	HopFromMean bool `json:"hop_from_mean,omitempty"`
	// LostVisits are calls to an address this service answers on that had no span of it under them (lost.go): its
	// visits missing from the capture, LostShare as a share of its visits. BrokenVisitShare is the share of its
	// visits in traces that miss a span (left out of the call structure), when above brokenVisitShareWarn.
	LostVisits       int64   `json:"lost_visits,omitempty"`
	LostShare        float64 `json:"lost_share,omitempty"`
	BrokenVisitShare float64 `json:"broken_visit_share,omitempty"`
	// VariableCalls are the callees this service calls a varying number of times per visit, and how the model
	// takes them (counts.go).
	VariableCalls []VariableCall `json:"variable_calls,omitempty"`
	// CallShapes are the shapes of the calls a service's visits take, when it is modelled as a mixture of them.
	CallShapes []CallShape `json:"call_shapes,omitempty"`
	// PodShares is each pod's share of the visits (two or more pods), SplitP the
	// p-value of those visits under an even random split (when enough visits),
	// UnevenSplit that it is below 1e-6: the model then takes the shares.
	PodShares   []PodShare `json:"pod_shares,omitempty"`
	SplitP      *float64   `json:"split_p,omitempty"`
	UnevenSplit bool       `json:"uneven_split,omitempty"`
	// Routes are the service's routes (routes.go), when two or more of them each hold MinRouteShare of its visits;
	// nil otherwise.
	Routes []RouteStats `json:"routes,omitempty"`
}

// Inferred is the result.
type Inferred struct {
	Topology    *model.Topology
	Stats       []ServiceStats // parallel to Topology.Services
	LamExtPerMs float64        // external request rate, per ms
	WindowS     float64
	Requests    int64 // entry requests, in complete traces
	// ConsumerRequests are the entry requests (in Requests) that a message
	// consumer made: a service's visits under a CONSUMER span, such as the
	// work a request's message causes later. They are not more user requests.
	ConsumerRequests int64
	Spans            int
	Traces           int64
	// BrokenTraces lost a span: they count in the queues and the rates, not
	// in the entries and the call structure.
	BrokenTraces int64
	ProbeSpans   int // health and readiness probe spans the reader dropped
	// NestedSpans lists, by service, the nested spans of one kind in one pod that were read as one (nested.go).
	NestedSpans []NestedMerge
	Warnings    []string
	// WarningCodes and WarningSubjects are parallel to Warnings: the registered code of each warning that has one
	// (internal/contract), and the service it concerns, "" for a warning with no code or no subject. The text of a
	// warning does not depend on them.
	WarningCodes    []string
	WarningSubjects []string
	// Samples are the services' per-visit samples by name, with Options.KeepSamples; nil otherwise. They are no part
	// of a fit record.
	Samples map[string]*Samples
}

// glueSample is one visit's glue, kept with Options.KeepSamples.
type glueSample struct {
	svc   int32
	start uint64
	glue  float64
}

type edgeKey struct{ caller, callee int32 }

type edgeAcc struct {
	n         int64
	offsetSum float64 // ms from the caller's start to the call's start
}

type seg struct {
	pod        int32
	leaf       bool // the visit's whole span is own work (it made no calls)
	start, end uint64
}

// visitRec is one server visit: when it arrived, when it freed its worker (its
// first call out, or its end, clipped to the span) and its own work before
// that (any wait for a worker inside the span included). early marks a visit
// whose first call starts before its span (the release is clipped there); alt
// is its release if that call is skipped (the next one in the span, or the
// end), which earlyWarning compares with.
type visitRec struct {
	pod   int32
	svc   int32
	start uint64
	rel   uint64
	alt   uint64
	own   float32
	early bool
	route uint16 // the SERVER span's route (otlp.Span.Route)
}

// maxK is the largest "visits already busy at arrival" tracked; more is lumped into it.
const maxK = 64

// byK accumulates the own time of visits by the number of other visits that were
// doing their own work on the same pod when they arrived.
type byK struct {
	n    [maxK + 1]int64
	sum  [maxK + 1]float64
	sq   [maxK + 1]float64
	peak int
}

// pairAcc sums the call durations of the parallel visits that made exactly k
// calls, to get the correlation between the branches' latencies.
type pairAcc struct {
	n     int64
	sum   [maxSlots]float64
	sq    [maxSlots]float64
	cross [maxSlots][maxSlots]float64
}

const maxSlots = 8

// pairKey names one set of correlation sums: a service's parallel visits
// that made exactly k calls (group ""), or the visits in which one stage of
// its calls, the callees in group (a stageSig stage key), ran together.
type pairKey struct {
	svc   int32
	k     int
	group string
}

// countKey counts the visits of caller that made exactly n calls to callee.
type countKey struct {
	caller, callee int32
	n              int32
}

// sigKey counts the visits of svc whose calls ran in the stages sig
// (a stageSig key; visits that made no call are not counted).
type sigKey struct {
	svc int32
	sig string
}

// routeSigKey counts a service's visits by the SERVER span's route (Span.Route, raw) and call signature ("" for a
// visit that makes no calls), complete traces only: the call shapes of each route (routeCalls).
type routeSigKey struct {
	svc   int32
	route uint16
	sig   string
}

// acc holds the sums over a set of traces. A broken trace (one that lost a
// span) adds its visits to the queues and the rates only: visits, own times,
// server times, segs and vrec, and the route counts of the calls whose caller
// it kept (routeEdge, routeKnown). Everything else is the call structure and
// the entries, read from complete traces only; sVisits counts their visits.
type acc struct {
	pairs      map[pairKey]*pairAcc
	visits     []int64
	sVisits    []int64   // visits in complete traces: what the call structure is per
	ownSum     []float64 // own time up to the release (worker-holding)
	ownSq      []float64
	glueSum    []float64 // own time after the release (holding no worker)
	glueSq     []float64
	serverSum  []float64
	clientSum  []float64
	clientN    []int64
	multi      []int64 // visits with two or more calls
	overlap    []int64 // ... whose calls overlap
	edges      map[edgeKey]*edgeAcc
	counts     map[countKey]int64 // visits by the number of calls to a callee they wait for
	asyncN     map[edgeKey]int64  // calls on an async edge that the visits did not wait for
	sigs       map[sigKey]int64
	routeSigs  map[routeSigKey]int64
	hopSamples [][]float64           // client span time minus server span time of each call, by callee
	edgeGaps   map[edgeKey][]float64 // the same gaps by edge (caller, callee)
	segs       []seg
	vrec       []visitRec
	entries    []int64
	entryMin   uint64
	entryMax   uint64
	entryN     int64
	unmatched  int64
	traces     int64
	broken     int64
	brokenBy   [nLoss]int64 // broken traces by what they lost (a trace can count under several)
	calledTop  []int64      // broken traces with an orphan SERVER span of this service, which the capture's services call
	consumerN  int64        // entries reached through a message consumer (in entryN too)
	consumerEn []int64      // ... per service
	consumerX  int64        // client calls under a message consumer, to peers without server spans: on no request's path
	early      []int64      // visits whose first call starts before their span, by more than earlyCallNs ...
	earlyOwn   []int64      // ... of which the call's start is on the visit's own pod clock (a CLIENT span of its own)
	earlyMax   []float64    // ... and by how much at most, ms

	routeEdge  map[routeEdgeKey]int64 // visits by route and caller: entries in complete traces, callers in every trace (routes.go)
	routeKnown map[routeKey]int64     // visits by route in broken traces whose caller is known (routes.go)

	lostEdges map[lostKey]int64   // calls with no span of their callee under them, by caller and address (lost.go)
	lostBelow map[orphanKey]int64 // spans whose parent is not in the capture, below a top in it or not SERVER/CONSUMER, by service and kind

	keep    bool         // Options.KeepSamples: keep glueRec and errs
	glueRec []glueSample // each complete trace's visits' glue
	errs    []int64      // visits whose SERVER span is an error, by service
}

func newAcc(n int) *acc {
	return &acc{
		visits: make([]int64, n), sVisits: make([]int64, n), ownSum: make([]float64, n), ownSq: make([]float64, n),
		glueSum: make([]float64, n), glueSq: make([]float64, n),
		serverSum: make([]float64, n), clientSum: make([]float64, n), clientN: make([]int64, n),
		multi: make([]int64, n), overlap: make([]int64, n), edges: map[edgeKey]*edgeAcc{},
		counts: map[countKey]int64{}, asyncN: map[edgeKey]int64{}, sigs: map[sigKey]int64{}, routeSigs: map[routeSigKey]int64{},
		hopSamples: make([][]float64, n), edgeGaps: map[edgeKey][]float64{}, entries: make([]int64, n), entryMin: math.MaxUint64,
		pairs: map[pairKey]*pairAcc{}, early: make([]int64, n), earlyOwn: make([]int64, n), earlyMax: make([]float64, n),
		calledTop: make([]int64, n), consumerEn: make([]int64, n),
		lostEdges: map[lostKey]int64{}, lostBelow: map[orphanKey]int64{}, routeEdge: map[routeEdgeKey]int64{},
		routeKnown: map[routeKey]int64{},
	}
}

type call struct {
	callee       int32
	start, end   uint64
	clientDur    float64
	hasServer    bool
	sstart, send uint64 // the callee's SERVER span, when the call has one
	ownClock     bool   // start is read on the caller's pod clock (a CLIENT span of its own), not the callee's or another's
	quant        bool   // the CLIENT span is stamped on whole milliseconds (quantized.go): its start is up to a millisecond early
	async        bool   // not waited for (notWaited), on an edge whose calls mostly are not (prepass.async)
}

// Run infers a topology from the capture's spans. It reorders d.Spans in place (by trace and start) and makes
// nested spans of one kind in one pod INTERNAL (nested.go), so d is not the capture as read afterwards: a second
// Run on the same d gives the same topology but reports no nested spans, as they are INTERNAL by then.
func Run(d *otlp.Data, opt Options) (*Inferred, error) {
	if opt.KeptShare == 0 {
		opt.KeptShare = 1
	}
	if !(opt.KeptShare > 0 && opt.KeptShare <= 1) {
		return nil, fmt.Errorf("the kept share must be in (0, 1], got %v", opt.KeptShare)
	}
	if opt.MinRequests == 0 {
		opt.MinRequests = 100
	}
	if opt.MinRequests < 2 {
		opt.MinRequests = 2 // the window is read from the spread of the entry arrivals
	}
	if len(d.Spans) == 0 {
		return nil, &Refusal{Code: contract.TraceSpansSkipped, Numbers: map[string]float64{"skipped": float64(d.Skipped)},
			Err: fmt.Errorf("the capture holds no usable spans")}
	}
	est := opt.Estimator
	if est == "" {
		est = os.Getenv("RHOBOUND_ESTIMATOR")
	}
	switch est {
	case "", EstimatorFIFO:
		est = EstimatorFIFO
	case EstimatorRise:
	default:
		return nil, fmt.Errorf("unknown estimator %q (want %q or %q)", est, EstimatorFIFO, EstimatorRise)
	}
	fifo := est == EstimatorFIFO

	// service ids: services that emit server spans, then external peers
	hasServer := map[string]bool{}
	for i := range d.Spans {
		if d.Spans[i].Kind == otlp.KindServer {
			hasServer[d.Resources[d.Spans[i].Res].Service] = true
		}
	}
	if len(hasServer) == 0 {
		return nil, &Refusal{Code: contract.TraceNoServerSpans, Err: fmt.Errorf("the capture holds no SERVER spans: nothing to infer")}
	}
	var names []string
	for s := range hasServer {
		names = append(names, s)
	}
	sort.Strings(names)
	nSvc := len(names)
	svcOf := map[string]int32{}
	for i, s := range names {
		svcOf[s] = int32(i)
	}
	for _, name := range sortedNames(opt.Workers) {
		if _, ok := svcOf[name]; !ok {
			return nil, &Refusal{Code: contract.ChangeUnknownService, Subject: name,
				Err: fmt.Errorf("--workers: unknown service %q (services with SERVER spans: %s)", name, strings.Join(names, ", "))}
		}
	}
	resSvc := make([]int32, len(d.Resources)) // -1: emits no server spans
	podOf := map[string]int32{}
	resPod := make([]int32, len(d.Resources))
	nameOf := map[string]int32{} // every service name, with server spans or not (a message consumer may have none)
	resName := make([]int32, len(d.Resources))
	for i, r := range d.Resources {
		if id, ok := svcOf[r.Service]; ok {
			resSvc[i] = id
		} else {
			resSvc[i] = -1
		}
		nid, ok := nameOf[r.Service]
		if !ok {
			nid = int32(len(nameOf))
			nameOf[r.Service] = nid
		}
		resName[i] = nid
		key := r.Service + "\x00" + r.Pod
		pid, ok := podOf[key]
		if !ok {
			pid = int32(len(podOf))
			podOf[key] = pid
		}
		resPod[i] = pid
	}
	nPeer := len(d.Peers)
	nAll := nSvc + nPeer + 1 // the last slot is an unnamed external peer
	unknownPeer := int32(nSvc + nPeer)
	extName := func(id int32) string {
		if int(id) == nSvc+nPeer {
			return "ext:unknown"
		}
		return "ext:" + d.Peers[int(id)-nSvc]
	}

	slices.SortFunc(d.Spans, func(a, b otlp.Span) int {
		if a.Trace[0] != b.Trace[0] {
			if a.Trace[0] < b.Trace[0] {
				return -1
			}
			return 1
		}
		if a.Trace[1] != b.Trace[1] {
			if a.Trace[1] < b.Trace[1] {
				return -1
			}
			return 1
		}
		if a.Start != b.Start {
			if a.Start < b.Start {
				return -1
			}
			return 1
		}
		return 0
	})
	var bounds []int // start index of each trace group, plus the end
	for i := range d.Spans {
		if i == 0 || d.Spans[i].Trace != d.Spans[i-1].Trace {
			bounds = append(bounds, i)
		}
	}
	bounds = append(bounds, len(d.Spans))
	nGroups := len(bounds) - 1

	// one accumulator per chunk, merged in chunk order below: floating-point sums
	// then do not depend on which goroutine finished first
	chunk := chunkSize(nGroups)
	nChunks := (nGroups + chunk - 1) / chunk
	// nested spans of one kind in one pod become one (see nested.go), before anything reads the kinds
	nameList := make([]string, len(nameOf))
	for n, id := range nameOf {
		nameList[id] = n
	}
	// streams and other server spans that stay open for much of the capture are no visits (see longlived.go)
	long := longVisits(d, resName, nameList)
	if err := longVisitsError(d, long); err != nil {
		r := &Refusal{Code: contract.TraceStreams, Err: err}
		for _, ls := range long {
			if ls.share >= longBusyRefuse {
				r.Subject = ls.name
				r.Numbers = map[string]float64{"busy_share": ls.share}
				break
			}
		}
		return nil, r
	}
	nestedBy := make([][]nestedCount, nChunks)
	eachChunk(nGroups, chunk, func(ci, lo, hi int) {
		cnt := make([]nestedCount, len(nameList))
		for g := lo; g < hi; g++ {
			sp := d.Spans[bounds[g]:bounds[g+1]]
			mergeNested(sp, spanIndex(sp), resPod, resName, cnt)
		}
		nestedBy[ci] = cnt
	})
	nestedTotal := make([]nestedCount, len(nameList))
	for _, cnt := range nestedBy {
		for i, c := range cnt {
			nestedTotal[i].server += c.server
			nestedTotal[i].client += c.client
			nestedTotal[i].unscoped += c.unscoped
		}
	}
	quantPod := quantizedPods(d.Spans, resPod, len(podOf))
	pps := make([]*prepass, nChunks)
	eachChunk(nGroups, chunk, func(ci, lo, hi int) {
		pp := newPrepass(len(d.Addrs), nSvc, len(nameOf))
		pp.quantPod = quantPod
		for g := lo; g < hi; g++ {
			pp.addTrace(d.Spans[bounds[g]:bounds[g+1]], resSvc, resPod, resName, nSvc, unknownPeer)
		}
		pps[ci] = pp
	})
	pp := newPrepass(len(d.Addrs), nSvc, len(nameOf))
	pp.quantPod = quantPod
	for _, o := range pps {
		pp.merge(o)
	}
	pp.decide()
	accs := make([]*acc, nChunks)
	eachChunk(nGroups, chunk, func(ci, lo, hi int) {
		a := newAcc(nAll)
		if opt.KeepSamples {
			a.keep, a.errs = true, make([]int64, nAll)
		}
		for g := lo; g < hi; g++ {
			processTrace(d, d.Spans[bounds[g]:bounds[g+1]], a, resSvc, resPod, resName, nSvc, unknownPeer, !fifo, pp)
		}
		accs[ci] = a
	})

	// ---- merge
	t := newAcc(nAll)
	if opt.KeepSamples {
		t.keep, t.errs = true, make([]int64, nAll)
	}
	for _, a := range accs {
		if a.keep {
			t.glueRec = append(t.glueRec, a.glueRec...)
			for i, n := range a.errs {
				t.errs[i] += n
			}
		}
		for i := 0; i < nAll; i++ {
			t.visits[i] += a.visits[i]
			t.sVisits[i] += a.sVisits[i]
			t.ownSum[i] += a.ownSum[i]
			t.ownSq[i] += a.ownSq[i]
			t.glueSum[i] += a.glueSum[i]
			t.glueSq[i] += a.glueSq[i]
			t.serverSum[i] += a.serverSum[i]
			t.clientSum[i] += a.clientSum[i]
			t.clientN[i] += a.clientN[i]
			t.multi[i] += a.multi[i]
			t.overlap[i] += a.overlap[i]
			t.entries[i] += a.entries[i]
			t.consumerEn[i] += a.consumerEn[i]
			t.calledTop[i] += a.calledTop[i]
			t.early[i] += a.early[i]
			t.earlyOwn[i] += a.earlyOwn[i]
			t.earlyMax[i] = max(t.earlyMax[i], a.earlyMax[i])
			t.hopSamples[i] = append(t.hopSamples[i], a.hopSamples[i]...)
		}
		for k, g := range a.edgeGaps {
			t.edgeGaps[k] = append(t.edgeGaps[k], g...)
		}
		for k, e := range a.edges {
			te := t.edges[k]
			if te == nil {
				te = &edgeAcc{}
				t.edges[k] = te
			}
			te.n += e.n
			te.offsetSum += e.offsetSum
		}
		for k, n := range a.counts {
			t.counts[k] += n
		}
		for k, n := range a.asyncN {
			t.asyncN[k] += n
		}
		for k, n := range a.sigs {
			t.sigs[k] += n
		}
		for k, n := range a.routeSigs {
			t.routeSigs[k] += n
		}
		for k, p := range a.pairs {
			tp := t.pairs[k]
			if tp == nil {
				tp = &pairAcc{}
				t.pairs[k] = tp
			}
			tp.n += p.n
			for i := 0; i < maxSlots; i++ {
				tp.sum[i] += p.sum[i]
				tp.sq[i] += p.sq[i]
				for j := 0; j < maxSlots; j++ {
					tp.cross[i][j] += p.cross[i][j]
				}
			}
		}
		t.segs = append(t.segs, a.segs...)
		t.vrec = append(t.vrec, a.vrec...)
		if a.entryN > 0 {
			t.entryMin = min(t.entryMin, a.entryMin)
			t.entryMax = max(t.entryMax, a.entryMax)
			t.entryN += a.entryN
		}
		t.unmatched += a.unmatched
		t.traces += a.traces
		t.broken += a.broken
		for c := range a.brokenBy {
			t.brokenBy[c] += a.brokenBy[c]
		}
		for k, n := range a.lostEdges {
			t.lostEdges[k] += n
		}
		for k, n := range a.routeEdge {
			t.routeEdge[k] += n
		}
		for k, n := range a.routeKnown {
			t.routeKnown[k] += n
		}
		for k, n := range a.lostBelow {
			t.lostBelow[k] += n
		}
		t.consumerN += a.consumerN
		t.consumerX += a.consumerX
	}
	lostEdges := lostEdgesOf(t, pp, d, nameList, names)
	if t.broken > 0 && float64(t.broken) > maxBrokenShare*float64(t.traces) {
		return nil, &Refusal{Code: contract.TraceBrokenTraces,
			Numbers: map[string]float64{"broken": float64(t.broken), "traces": float64(t.traces)},
			Err:     brokenError(t, names, pp, nameList, lostEdges)}
	}
	if t.entryN < int64(opt.MinRequests) {
		return nil, &Refusal{Code: contract.TraceTooFewEntries,
			Numbers: map[string]float64{"entries": float64(t.entryN), "needed": float64(opt.MinRequests)},
			Err:     fmt.Errorf("only %d entry requests in the capture; need at least %d", t.entryN, opt.MinRequests)}
	}
	windowNs := float64(t.entryMax-t.entryMin) * float64(t.entryN) / float64(t.entryN-1)
	windowS := windowNs / 1e9
	if windowS <= 0 {
		return nil, &Refusal{Code: contract.TraceTooFewEntries, Numbers: map[string]float64{"entries": float64(t.entryN)},
			Err: fmt.Errorf("the entry requests span no time")}
	}

	out := &Inferred{WindowS: windowS, Requests: t.entryN, Spans: len(d.Spans), Traces: t.traces, BrokenTraces: t.broken,
		ProbeSpans: d.Probes, NestedSpans: nestedSummary(nameList, nestedTotal)}
	warn := func(f string, a ...any) {
		out.Warnings = append(out.Warnings, fmt.Sprintf(f, a...))
		out.WarningCodes = append(out.WarningCodes, "")
		out.WarningSubjects = append(out.WarningSubjects, "")
	}
	// coded tags the warning just added with its registered code and subject
	coded := func(code, subject string) {
		out.WarningCodes[len(out.WarningCodes)-1] = code
		out.WarningSubjects[len(out.WarningSubjects)-1] = subject
	}
	if d.Skipped > 0 {
		warn("%d spans were skipped (a missing id, service name, start or end time)", d.Skipped)
	}
	for _, w := range longVisitsWarnings(d, long) {
		warn("%s", w)
	}
	if w := nestedWarning(out.NestedSpans); w != "" {
		warn("%s", w)
	}
	// The entries are read from complete traces only, so the external rate is scaled up by the broken share to
	// count every request (the reference does not scale it; the per-service rates there count every visit).
	entryScale := 1.0
	if t.broken > 0 {
		entryScale = float64(t.traces) / float64(t.traces-t.broken)
		warn("%d of %d traces miss a span (%.2f%%): left out of the entries and the call structure, kept in the queues "+
			"and the rates (the external rate is scaled up by %.4f to count them)", t.broken, t.traces,
			100*float64(t.broken)/float64(t.traces), entryScale)
		for _, w := range lostWarnings(t, pp, lostEdges, nameList, names) {
			warn("%s", w)
		}
	}
	for _, w := range entryCalledWarnings(pp, names) {
		warn("%s", w)
	}

	// pods and peak concurrency per service
	podsOf := make([]map[int32]bool, nAll)
	for i := range podsOf {
		podsOf[i] = map[int32]bool{}
	}
	spanKinds := map[int32]uint8{} // the kinds (a bit each) of the spans of services that emit no SERVER span
	for i := range d.Spans {
		sp := &d.Spans[i]
		if sp.Kind == otlp.KindServer && resSvc[sp.Res] >= 0 {
			podsOf[resSvc[sp.Res]][resPod[sp.Res]] = true
		} else if resSvc[sp.Res] < 0 {
			spanKinds[resName[sp.Res]] |= 1 << sp.Kind
		}
	}
	if w := notModelledWarning(spanKinds, nameList); w != "" {
		warn("%s", w)
	}
	podName := make([]string, len(podOf))
	for i, r := range d.Resources {
		podName[resPod[i]] = r.Pod
	}
	podVisits := make([]int64, len(podOf)) // every visit, broken traces included, as the queues count them
	for i := range t.vrec {
		podVisits[t.vrec[i].pod]++
	}
	var hist []*byK
	var queues []*svcQueue
	fixed := map[int32]int{}
	if fifo {
		for name, c := range opt.Workers {
			if id, ok := svcOf[name]; ok {
				fixed[id] = c
			}
		}
		queues = reconstructQueues(t.vrec, nSvc, fixed)
	} else {
		hist = sweepByK(t.segs, t.vrec, nAll)
	}

	// each service's routes (routes.go): nil for the services with one
	routeNames, routeBucket := routeBuckets(t.vrec, d.Routes, nSvc)
	mixOf := map[int]mixtureResult{} // topology index -> its mixture of call shapes (routeCalls)

	// which services and externals are in the topology
	var used []int32
	for i := 0; i < nAll; i++ {
		if t.visits[i] > 0 || (i >= nSvc && t.clientN[i] > 0) {
			used = append(used, int32(i))
		}
	}
	idx := map[int32]int{}
	for k, id := range used {
		idx[id] = k
	}
	topo := &model.Topology{Name: "inferred", Entry: map[int]float64{}}
	stats := make([]ServiceStats, len(used))
	var lower []string    // services whose rebuilt queues saw too few waits to pin the worker count
	var negative []string // services whose rebuilt queues give a negative mean service time
	var negName string    // the first of them
	identified := false
	briefly := false // some lower bound saw enough waits, too short to pin the count
	for k, id := range used {
		i := int(id)
		var sv model.Service
		var st ServiceStats
		if i < nSvc {
			st.Name = names[i]
			n := float64(t.visits[i])
			allMean := t.ownSum[i] / n
			mean, cs2 := allMean, 1.0
			nEst := n // the visits the estimate is read from
			if allMean > 0 && t.visits[i] > 1 {
				v := (t.ownSq[i] - n*allMean*allMean) / (n - 1)
				cs2 = math.Max(0, v) / (allMean * allMean)
			}
			var c int
			var saturated bool
			st.WorkersInferred = true
			if fifo {
				// The rebuilt queue gives every visit's start of service, so the service time is the
				// release minus that start, read from every visit (the reference's quantity).
				q := queues[i]
				_, set := opt.Workers[st.Name]
				st.WorkersInferred = !set
				if q == nil || q.jobs < 2 {
					c = 1
					if set {
						c = opt.Workers[st.Name]
						warn("%s: too few visits to rebuild its queue, so --workers %d is taken as given; its service time "+
							"includes any queueing in the span", st.Name, c)
					} else {
						warn("%s: too few visits to rebuild its queue; its service time includes any queueing in the span", st.Name)
					}
				} else {
					c, saturated = q.workers, q.identified
					mean, cs2, nEst = q.relMean, 0, float64(q.jobs)
					if mean > 0 {
						cs2 = math.Max(0, q.relVar) / (mean * mean)
					}
					identified = identified || q.identified
					if st.WorkersInferred && !q.identified {
						why := fmt.Sprintf("%d waited", q.minWaited)
						if q.minWaited >= identMinWaits {
							why = fmt.Sprintf("%d waited, %.3g ms on average against %.3g ms of service: too short to rule out more workers",
								q.minWaited, q.waitedMean, q.relMean)
							briefly = true
						}
						lower = append(lower, fmt.Sprintf("%s %d+ (%s; up to %d in a pod at once)", st.Name, q.workers, why, q.maxSys))
					}
					if q.negShare > 0.001 {
						warn("%s: %.2f%% of reconstructed service times are negative (clock or ordering jitter)", st.Name, 100*q.negShare)
						coded(contract.TraceClockSkew, st.Name)
					}
					if !(mean >= 0) {
						if negName == "" {
							negName = st.Name
						}
						negative = append(negative, fmt.Sprintf("%s %.3f ms", st.Name, mean))
					}
				}
			} else {
				// Service time and its variability come from visits that found a free worker: they cannot
				// have queued, so a span that includes queueing time does not inflate them. Those are the
				// visits that arrived with fewer than c others busy, not only the ones that found the pod
				// empty: at four workers and 75% utilization that is half of the visits, not 4%.
				h := hist[i]
				var free moments
				var haveFree bool
				c, saturated, free, haveFree = workersFromK(h)
				if ov, ok := opt.Workers[st.Name]; ok {
					c, st.WorkersInferred = ov, false
					free, haveFree = poolBelow(h, c)
				}
				if haveFree {
					mean, cs2, nEst = free.mean(), 0, free.n
					if mean > 0 {
						cs2 = math.Max(0, free.variance()) / (mean * mean)
					}
				} else {
					warn("%s: only %.0f visits found a free worker; its service time includes any queueing in the span",
						st.Name, free.n)
				}
			}
			// As in the reference, a cs2 within cs2SnapSigma standard errors of 1 is taken as 1 (exponential):
			// sqrt(8/n) is the standard error of the cs2 of n exponential times.
			st.OwnCS2Measured = cs2
			if math.Abs(cs2-1) < cs2SnapSigma*math.Sqrt(8/math.Max(nEst, 1)) {
				cs2, st.CS2Snapped = 1, true
			}
			st.Visits, st.OwnMeanMs, st.OwnCS2 = t.visits[i], mean, cs2
			st.InSpanWaitMs = math.Max(0, allMean-mean)
			var rq *svcQueue // the jobs the service time was read from, when the rebuild gave it
			if fifo && queues[i] != nil && queues[i].jobs >= 2 {
				rq = queues[i]
			}
			st.Routes = routeStatsOf(t, rq, id, routeNames[i], routeBucket[i], names)
			if ns := float64(t.sVisits[i]); ns > 0 { // glue is call structure: complete traces only
				if g := t.glueSum[i] / ns; g > 0 {
					st.GlueMs = g
					if ns > 1 {
						st.GlueCS2 = math.Max(0, (t.glueSq[i]-ns*g*g)/(ns-1)) / (g * g)
					}
				}
			}
			st.ObservedServerMs = t.serverSum[i] / n
			st.Pods = len(podsOf[i])
			if c < 1 {
				c = 1
			}
			st.WorkersFromQueueing = saturated && st.WorkersInferred
			st.WorkersPerPod = c
			sv = model.Service{Name: st.Name, S: mean, CS2: cs2, Glue: st.GlueMs, GlueCS2: st.GlueCS2}
			if st.Pods <= 1 {
				sv.Servers, sv.Workers, sv.Routing = c, 1, model.Pooled
			} else {
				sv.Servers, sv.Workers, sv.Routing = st.Pods, c, model.Random
				var p *float64
				st.PodShares, p, sv.Shares = podSplit(podsOf[i], podName, podVisits)
				st.SplitP, st.UnevenSplit = p, sv.Shares != nil
				if st.UnevenSplit {
					warn("%s", splitWarning(st.Name, st.PodShares, *p))
				}
			}
			if t.multi[i] > 0 {
				st.ParallelShare = float64(t.overlap[i]) / float64(t.multi[i])
			}
			for _, r := range d.Resources {
				if r.Service == st.Name && r.Image != "" && !slices.Contains(st.Images, r.Image) {
					st.Images = append(st.Images, r.Image)
				}
			}
			if t.visits[i] < 30 {
				warn("%s: only %d visits; its estimates are rough", st.Name, t.visits[i])
			}
			if sh := float64(t.early[i]) / n; sh > earlyCallShare {
				var skip *svcQueue // the queue rebuilt with those calls skipped, for the direction of the effect
				if fifo && queues[i] != nil {
					var vs []visitRec
					for _, v := range t.vrec {
						if v.svc == id {
							v.rel = v.alt
							vs = append(vs, v)
						}
					}
					skip = reconstructQueues(vs, nSvc, fixed)[i]
				}
				warn("%s", earlyWarning(st.Name, sh, t.early[i], t.earlyOwn[i], t.earlyMax[i], c, mean, skip))
				if t.earlyOwn[i] < t.early[i] { // some on another pod's clock: clock skew between pods
					coded(contract.TraceClockSkew, st.Name)
				}
			}
		} else {
			st.Name, st.External = extName(id), true
			n := float64(t.clientN[i])
			mean := t.clientSum[i] / n
			st.Visits, st.OwnMeanMs, st.OwnCS2 = t.clientN[i], mean, 1
			st.ObservedClientMs = mean
			sv = model.Service{Name: st.Name, S: mean, CS2: 1, Delay: true, Servers: 1, Workers: 1, Routing: model.Pooled}
		}
		// hop: the quietest tenth of (client - server) is the network; the rest is queueing outside the span
		if hs := t.hopSamples[i]; len(hs) > 0 && !st.External {
			slices.Sort(hs)
			hop := hs[len(hs)/10]
			sum := 0.0
			for _, v := range hs {
				sum += v
			}
			st.HopMs = hop
			st.ObservedWaitMs = math.Max(0, sum/float64(len(hs))-hop)
			sv.Hop = hop
		}
		if t.clientN[i] > 0 && !st.External {
			st.ObservedClientMs = t.clientSum[i] / float64(t.clientN[i])
		}
		st.RateRPS = float64(st.Visits) / windowS / opt.KeptShare
		topo.Services = append(topo.Services, sv)
		stats[k] = st
	}

	if len(negative) > 0 {
		// The release is clipped to the span, so at the most workers the pod ever had busy every service time is
		// at least 0; the search stops below that only with a few contradictions. A negative mean takes a worker
		// count that the spans contradict: a user-set one, or times that do not fit any FIFO queue.
		return nil, &Refusal{Code: contract.TraceNegativeServiceTime, Subject: negName, Err: fmt.Errorf("refusing to infer: "+
			"the rebuilt queues give a negative mean service time (%s): the spans' "+
			"times contradict a first-come-first-served queue with that many workers. Clock skew between pods does "+
			"this (a call with no CLIENT span is timed by the callee's clock, so a callee whose clock runs behind seems "+
			"to answer before it was called), and so does a --workers count below the pod's real one -- synchronize the "+
			"clocks (NTP) or export CLIENT spans for the calls, and check --workers", strings.Join(negative, ", "))}
	}
	if len(lower) > 0 {
		// one line, not one per service: on a capture whose spans start once a worker has taken the request no
		// visit can wait inside its span, so every count is a lower bound; a pod with no worker limit has waits,
		// but too short to pin a count
		if identified || briefly {
			warn("too few requests waited for a worker, or too briefly, to pin these worker counts, so they are lower bounds "+
				"(a pod with no worker limit looks like this too): %s", strings.Join(lower, ", "))
		} else {
			warn("no service shows enough requests waiting for a worker inside its spans to pin its worker count (none can "+
				"when spans start once a worker has taken the request), so the counts are lower bounds: %s", strings.Join(lower, ", "))
		}
	}

	// calls: per caller, how many calls each visit makes to each callee, and how they run
	type ce struct {
		callee int32
		offset float64
		hist   map[int]int64 // calls per visit -> visits (visits that made at least one)
	}
	callsOf := map[int32]map[int32]*ce{}
	var asyncDesc []string
	for ek, e := range t.edges {
		if t.sVisits[ek.caller] == 0 {
			continue
		}
		if callsOf[ek.caller] == nil {
			callsOf[ek.caller] = map[int32]*ce{}
		}
		callsOf[ek.caller][ek.callee] = &ce{ek.callee, e.offsetSum / float64(e.n), map[int]int64{}}
	}
	for ck, n := range t.counts {
		if c := callsOf[ck.caller][ck.callee]; c != nil {
			c.hist[int(ck.n)] += n
		}
	}
	callers := make([]int32, 0, len(callsOf))
	for caller := range callsOf {
		callers = append(callers, caller)
	}
	slices.Sort(callers)
	meanExplained := false // the long explanation of the mean count rule is given once (meanRuleWarning)
	meanWarn := func(caller string, v varyEdge) string {
		w := meanRuleWarning(caller, v, !meanExplained)
		meanExplained = true
		return w
	}
	var batchNodes []int          // parallel nodes of calls made at once that batchWarnings checks (counts.go)
	nodeOwner := map[int]string{} // ... and the service whose calls each holds
	for _, caller := range callers {
		var list []*ce
		for _, c := range callsOf[caller] {
			list = append(list, c)
		}
		sort.Slice(list, func(a, b int) bool {
			if list[a].offset != list[b].offset {
				return list[a].offset < list[b].offset
			}
			return list[a].callee < list[b].callee
		})
		si := idx[caller]
		name := stats[si].Name
		nsp := t.sVisits[caller]
		type counted struct {
			child, k int
			p        float64
		}
		var cnts []counted
		var cond []string
		var varying []varyEdge      // callees whose calls per visit are neither k nor k + 1 (counts.go)
		var asyncCalls []model.Call // calls sent after the response: k + p per visit, the mean count (only the load matters)
		nkeys := 0
		for _, c := range list {
			ci, ok := idx[c.callee]
			if !ok {
				continue
			}
			if pp.async[edgeKey{caller, c.callee}] {
				mean := float64(t.asyncN[edgeKey{caller, c.callee}]) / float64(nsp)
				k := int(math.Floor(mean + 1e-12))
				for j := 0; j < k; j++ {
					asyncCalls = append(asyncCalls, model.Call{Child: ci, P: 1, Async: true})
				}
				if p := mean - float64(k); p > 1e-12 {
					asyncCalls = append(asyncCalls, model.Call{Child: ci, P: p, Async: true})
				}
				d := fmt.Sprintf("%s -> %s (%.3g per visit", name, stats[ci].Name, mean)
				if o := pp.afterOther[edgeKey{caller, c.callee}]; o > 0 {
					// no CLIENT span: the start is the callee's, so a callee clock that runs ahead looks the same
					d += fmt.Sprintf("; %.0f%% of them timed by the callee's clock, where a clock that runs ahead of %s's "+
						"looks the same: export CLIENT spans for the calls, or synchronize the clocks (NTP), to tell",
						100*float64(o)/float64(pp.callAfter[edgeKey{caller, c.callee}]), name)
				}
				asyncDesc = append(asyncDesc, d+")")
				if len(c.hist) == 0 {
					continue
				}
				// the edge's few calls that a visit did wait for (they start and end within its span) stay on its path
			}
			k, p, other, err := callCount(c.hist, nsp)
			if err != nil {
				// calls per visit that are neither k nor k + 1: the mean enters (counts.go)
				ve := newVaryEdge(ci, stats[ci].Name, c.hist, nsp)
				varying = append(varying, ve)
				k, p = meanCount(ve.mean)
				stats[si].VariableCalls = append(stats[si].VariableCalls, VariableCall{Callee: ve.name, MeanPerVisit: ve.mean,
					VisitsByCalls: ve.byCalls, ModelledAs: ModelledMean})
				cnts = append(cnts, counted{ci, k, p})
				nkeys += ve.maxN // it may be made at once, up to that many
				continue
			}
			if other > 0 {
				warn("%s -> %s: %.2f%% of visits show another call count; ignored", name, stats[ci].Name, 100*other)
			}
			if p > 0 {
				cond = append(cond, stats[ci].Name)
				nkeys++
			}
			nkeys += k
			cnts = append(cnts, counted{ci, k, p})
		}
		// topUpCalls are the mean-count calls that bring the calls the model makes to each callee (expected, per
		// visit) up to the calls the visits made, and a warning for a callee left far short
		topUpCalls := func(expected map[int]float64, what string) []model.Call {
			var obs []callTotals
			for _, c := range list {
				if ci, ok := idx[c.callee]; ok {
					obs = append(obs, callTotals{ci, histMean(c.hist, nsp), expected[ci]})
				}
			}
			calls, big := topUp(obs)
			for _, x := range big {
				warn("%s -> %s: %s hold %.3g of the %.3g calls per visit that the visits made (calls that start after a visit's "+
					"span ends, or shapes left out); the rest enter as mean counts, one after another", name,
					stats[x.child].Name, what, x.have, x.observed)
			}
			return calls
		}
		multi := t.multi[caller]
		share := stats[si].ParallelShare
		sigs := map[string]int64{}
		if multi > 0 {
			for sk, n := range t.sigs {
				if sk.svc == caller {
					sigs[sk.sig] = n
				}
			}
		}
		// Calls in stages (as in the reference): each stage one call or a group at once, the next once
		// the last has ended, a whole stage sometimes left out. A group is a zero-time delay node that calls its
		// members in parallel; the service calls its stages one after another (its glue stays on it). One case
		// is added to the reference's: a single group that runs on some visits only, all of it or none (the
		// reference refuses it as a conditional parallel call), is one stage called with that probability.
		if tm, ran, fit, ok := stageTemplate(sigs, nsp); ok && hasGroup(tm) && fit >= parallelShare &&
			(len(tm) >= 2 || ran[0] < 1-1e-12) {
			var desc []string
			var stCalls []model.Call
			expected := map[int]float64{} // calls per visit the stages make to each callee
			for j, g := range tm {
				q := ran[j]
				if q >= 1-1e-12 {
					q = 1
				}
				var members []model.Call
				var mnames []string
				for _, id := range g {
					if ci, ok := idx[id]; ok {
						members = append(members, model.Call{Child: ci, P: 1})
						mnames = append(mnames, stats[ci].Name)
						expected[ci] += q
					}
				}
				d := strings.Join(mnames, " + ")
				if len(members) > 1 {
					d += " at once"
				}
				if q < 1 {
					d += fmt.Sprintf(" (p %.2f)", q)
				}
				desc = append(desc, d)
				if len(members) == 1 {
					stCalls = append(stCalls, model.Call{Child: members[0].Child, P: q})
					continue
				}
				r, _ := pairCorrelation(t.pairs[pairKey{caller, len(g), stageSig{g}.key()}], len(g))
				r = math.Max(0, math.Min(r, 0.95))
				stName := fmt.Sprintf("stage:%s:%d", name, j+1)
				topo.Services = append(topo.Services, model.Service{Name: stName, CS2: 1, Delay: true, Parallel: true,
					Calls: members, Servers: 1, Workers: 1, Routing: model.Pooled, BranchCorr: r})
				stats = append(stats, ServiceStats{Name: stName, Stage: true, Visits: int64(math.Round(float64(t.visits[caller]) * q)),
					RateRPS: stats[si].RateRPS * q, Pods: 1, WorkersPerPod: 1, ParallelShare: 1, BranchCorr: r})
				stCalls = append(stCalls, model.Call{Child: len(topo.Services) - 1, P: q})
				batchNodes = append(batchNodes, len(topo.Services)-1)
				nodeOwner[len(topo.Services)-1] = name
			}
			if len(varying) > 0 {
				// calls per visit that vary, carried by the stages: the stages are the count's shape. A callee that no
				// stage calls (all its calls start after the visits' spans) enters as a mean count.
				for j, v := range varying {
					if expected[v.child] > 0 {
						stats[si].VariableCalls[j].ModelledAs = ModelledStages
						warn("%s -> %s: calls per visit vary as {%s}; the stages below carry them (%.3g per visit on average)",
							name, v.name, countsText(v.byCalls), v.mean)
					} else {
						warn("%s", meanWarn(name, v))
					}
				}
				stCalls = append(stCalls, topUpCalls(expected, "the stages")...)
			}
			topo.Services[si].Calls = append(stCalls, asyncCalls...)
			stats[si].Stages = len(tm)
			if len(tm) == 1 {
				warn("%s: calls %s on some visits only, all of them or none", name, desc[0])
			} else {
				warn("%s: calls in %d stages, each after the last ends: %s", name, len(tm), strings.Join(desc, "; then "))
			}
			if fit < 1 {
				warn("%s: %.2f%% of its visits call in another shape than its stages; modelled as the stages", name, 100*(1-fit))
			}
			continue
		}
		parallel, mixture := false, false
		switch {
		case nkeys <= 1 || multi == 0 || share <= 1-parallelShare:
		case share >= parallelShare:
			parallel = true
		default:
			mixture = true // overlap on some visits, in a shape the stage template does not carry
		}
		if parallel && (len(cond) > 0 || len(varying) > 0) {
			// the expected maximum needs every branch, and how many there are, on every visit: each shape of
			// the visits has them
			parallel, mixture = false, true
		}
		if mixture {
			b := &shapeBuilder{topo: topo, stats: &stats, si: si, name: name, caller: caller, nsp: nsp, idx: idx, pairs: t.pairs,
				owner: nodeOwner}
			res, ok := b.mixtureCalls(sigs)
			if !ok {
				shape, seen := commonShape(sigs, stats, idx)
				return nil, &Refusal{Code: contract.TraceTooManyShapes, Subject: name,
					Numbers: map[string]float64{"left_out_share": res.tail, "limit": shapeTail}, Err: fmt.Errorf(
						"%s: its calls overlap in %.0f%% of its visits with two or more calls, and its visits take "+
							"more than %d call shapes, so that the commonest %d leave %.0f%% of its visits that make calls out (the limit is %.0f%%); the model has "+
							"sequential, parallel and staged calls, or a mixture of up to %d shapes (most common shape: %s, %.0f%% of visits)",
						name, 100*share, maxShapes, maxShapes, 100*res.tail, 100*shapeTail, maxShapes, shape, 100*float64(seen)/float64(nsp))}
			}
			mixOf[si] = res
			calls := append(res.calls, topUpCalls(res.expected, "the call shapes")...)
			topo.Services[si].Calls = append(calls, asyncCalls...)
			batchNodes = append(batchNodes, res.nodes...)
			stats[si].CallShapes = res.shapes
			var desc []string
			for _, sh := range res.shapes {
				desc = append(desc, fmt.Sprintf("%s (%.1f%% of visits)", sh.Shape, 100*sh.Share))
			}
			warn("%s: its visits take %d call shapes, modelled as a mixture (it calls each shape with the share of visits that "+
				"took it): %s", name, len(res.shapes), strings.Join(desc, "; "))
			if res.tail > 0 {
				warn("%s: %.2f%% of its visits that make calls take rarer shapes; their calls enter as mean counts, one after another", name, 100*res.tail)
			}
			for j, v := range varying {
				if res.expected[v.child] > 0 {
					stats[si].VariableCalls[j].ModelledAs = ModelledMixture
					warn("%s -> %s: calls per visit vary as {%s}; the call shapes carry them (%.3g per visit on average)", name,
						v.name, countsText(v.byCalls), v.mean)
				} else {
					warn("%s", meanWarn(name, v))
				}
			}
			continue
		}
		for _, v := range varying {
			warn("%s", meanWarn(name, v))
		}
		sv := &topo.Services[si]
		sv.Parallel = parallel
		if parallel {
			batchNodes = append(batchNodes, si)
			nodeOwner[si] = name
			if r, n := branchCorrelation(t.pairs, caller); n > 0 {
				stats[si].BranchCorr = math.Max(0, math.Min(r, 0.95))
				sv.BranchCorr = stats[si].BranchCorr
			}
		}
		for _, c := range cnts {
			for j := 0; j < c.k; j++ {
				sv.Calls = append(sv.Calls, model.Call{Child: c.child, P: 1})
			}
			if c.p > 0 {
				sv.Calls = append(sv.Calls, model.Call{Child: c.child, P: c.p})
			}
		}
		sv.Calls = append(sv.Calls, asyncCalls...)
	}
	routeNodes(topo, stats, idx, t.routeSigs, t.routeKnown, routeNames, routeBucket, mixOf)
	if len(asyncDesc) > 0 {
		warn("calls the caller does not wait for (at least %.0f%% of them start once the caller's span has ended, or outlive "+
			"it on the caller's clock: fire and forget) are "+
			"modelled as async: they load the callee, but the caller does not wait for them, so neither the callee's latency "+
			"nor a change of it reaches the caller: %s", 100*parallelShare, strings.Join(asyncDesc, ", "))
	}
	mixed := pp.asyncMixed()
	var mixedKeys []edgeKey
	for k := range mixed {
		if _, ok := idx[k.callee]; ok && t.sVisits[k.caller] > 0 {
			mixedKeys = append(mixedKeys, k)
		}
	}
	slices.SortFunc(mixedKeys, func(a, b edgeKey) int {
		if a.caller != b.caller {
			return int(a.caller - b.caller)
		}
		return int(a.callee - b.callee)
	})
	for _, k := range mixedKeys {
		caller := stats[idx[k.caller]].Name
		clock := ""
		if o := pp.afterOther[k]; o > 0 {
			// no CLIENT span: the start is the callee's, so a callee clock that runs ahead looks the same
			clock = fmt.Sprintf("; %.0f%% of those are timed by the callee's clock, where a clock that runs ahead of %s's "+
				"looks the same: export CLIENT spans for the calls, or synchronize the clocks (NTP), to tell",
				100*float64(o)/float64(pp.callAfter[k]), caller)
		}
		warn("%s -> %s: %.1f%% of the calls start after %s's span has ended or outlive it (sent with the response, not "+
			"waited for) and the rest within it; all are modelled as on its latency path (calls are taken as async when at "+
			"least %.0f%% of them are not waited for)%s",
			caller, stats[idx[k.callee]].Name, 100*mixed[k], caller, 100*parallelShare, clock)
	}
	// entries
	for k, id := range used {
		if int(id) < nSvc && t.entries[id] > 0 {
			topo.Entry[k] = float64(t.entries[id]) / float64(t.entryN)
			stats[k].Entry = true
			stats[k].Entries, stats[k].ConsumerEntries = t.entries[id], t.consumerEn[id]
		}
	}
	out.ConsumerRequests = t.consumerN
	if err := topo.Finish(); err != nil {
		// name the services that call themselves: what is left of a self-call once nested spans are read as one is a
		// call over the network (SERVER -> CLIENT -> SERVER of one service), or nested spans too far apart in time
		var selfCalls []string
		for i, sv := range topo.Services {
			if slices.ContainsFunc(sv.Calls, func(c model.Call) bool { return c.Child == i }) {
				selfCalls = append(selfCalls, stats[i].Name)
			}
		}
		var msg error
		if len(selfCalls) > 0 {
			msg = fmt.Errorf("the inferred call graph is not usable: %w (a service calling itself: %s; nested spans of one pod "+
				"are read as one, so this is a call over the network, SERVER spans of one instrumentation scope inside one another (read as a "+
				"call), or nested spans more than %d ms apart)", err,
				strings.Join(selfCalls, ", "), nestedSlackNs/1_000_000)
		} else {
			msg = fmt.Errorf("the inferred call graph is not usable: %w", err)
		}
		if !errors.Is(err, model.ErrCycle) {
			return nil, msg
		}
		r := &Refusal{Code: contract.TraceCycle, Err: msg}
		if len(selfCalls) > 0 {
			r.Subject = selfCalls[0]
		}
		return nil, r
	}
	// the refined model takes calls made at once to a pooled callee as batch arrivals (model.Config.Batch); --plain does not
	modelled := func(callee int) bool { return !opt.Plain && model.BatchModelled(topo, callee) }
	batchWarnings(topo, stats, batchNodes, nodeOwner, modelled, func(isCoded bool, f string, a ...any) {
		warn(f, a...)
		if isCoded {
			subject, _ := a[0].(string) // the caller that sends the calls at once
			coded(contract.ModelBatchArrivals, subject)
		}
	})
	markLost(lostEdges, stats, idx)
	for _, w := range markStructure(t, stats, idx) {
		warn("%s", w)
	}
	out.Topology, out.Stats = topo, stats
	out.LamExtPerMs = float64(t.entryN) * entryScale / windowS / opt.KeptShare / 1000
	for _, w := range setHops(out, t, used, nodeOwner, opt.Plain) {
		warn("%s", w)
	}
	if t.unmatched > 0 {
		warn("%d client calls to services without server spans were treated as external delays", t.unmatched)
	}
	if t.consumerX > 0 {
		warn("%d client calls made under message consumers (CONSUMER spans), to services without server spans, are on no "+
			"request's path and were left out (a consumer's work is its own, not the producing request's; a call from it "+
			"to a service with server spans is an entry of that service)", t.consumerX)
	}
	if opt.KeepSamples {
		out.Samples = keptSamples(t, queues, names, nSvc, extName)
		for i := 0; i < nSvc; i++ {
			if smp := out.Samples[names[i]]; smp != nil && i < len(queues) && queues[i] != nil {
				smp.RouteOwn = routeSamples(t, queues[i], routeNames[i], routeBucket[i])
			}
		}
	}
	return out, nil
}

// keptSamples gathers each service's per-visit samples (Options.KeepSamples), each list in order of arrival, ties
// broken by value, so that they do not depend on how the work was split.
func keptSamples(t *acc, queues []*svcQueue, names []string, nSvc int, extName func(int32) string) map[string]*Samples {
	out := map[string]*Samples{}
	type timed struct {
		start uint64
		v     float64
	}
	byStart := func(xs []timed) []float64 {
		sort.Slice(xs, func(i, j int) bool {
			if xs[i].start != xs[j].start {
				return xs[i].start < xs[j].start
			}
			return xs[i].v < xs[j].v
		})
		out := make([]float64, len(xs))
		for i, x := range xs {
			out[i] = x.v
		}
		return out
	}
	glue := make([][]timed, nSvc)
	for _, g := range t.glueRec {
		if int(g.svc) < nSvc {
			glue[g.svc] = append(glue[g.svc], timed{g.start, g.glue})
		}
	}
	for i := 0; i < nSvc; i++ {
		smp := &Samples{Calls: map[string]float64{}, CallsSq: map[string]float64{}, CallVisits: t.sVisits[i],
			Visits: t.visits[i], Errors: t.errs[i], Glue: byStart(glue[i]), Own: []float64{}}
		if i < len(queues) && queues[i] != nil {
			var own []timed
			for _, q := range queues[i].pods {
				for k := range q.B {
					own = append(own, timed{t.vrec[q.vidx[q.Order[k]]].start, q.D[k] - q.B[k]})
				}
			}
			smp.Own = byStart(own)
		}
		if n := float64(t.sVisits[i]); n > 0 {
			var keys []countKey // in a fixed order, so that the sums do not depend on the map's
			for k := range t.counts {
				if int(k.caller) == i {
					keys = append(keys, k)
				}
			}
			sort.Slice(keys, func(a, b int) bool {
				if keys[a].callee != keys[b].callee {
					return keys[a].callee < keys[b].callee
				}
				return keys[a].n < keys[b].n
			})
			for _, k := range keys {
				var callee string
				if int(k.callee) < nSvc {
					callee = names[k.callee]
				} else {
					callee = extName(k.callee)
				}
				smp.Calls[callee] += float64(k.n) * float64(t.counts[k]) / n
				smp.CallsSq[callee] += float64(k.n) * float64(k.n) * float64(t.counts[k]) / n
			}
		}
		out[names[i]] = smp
	}
	return out
}

// sortedNames returns a map's keys in order.
func sortedNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// hasGroup reports whether some stage holds two or more calls.
func hasGroup(s stageSig) bool {
	for _, g := range s {
		if len(g) > 1 {
			return true
		}
	}
	return false
}

// commonShape describes the commonest call shape in sigs, by callee name, and
// the visits that show it.
func commonShape(sigs map[string]int64, stats []ServiceStats, idx map[int32]int) (string, int64) {
	best, n := "", int64(0)
	for k, c := range sigs {
		if c > n || (c == n && k < best) {
			best, n = k, c
		}
	}
	var parts []string
	for _, g := range parseSig(best) {
		var names []string
		for _, id := range g {
			if ci, ok := idx[id]; ok {
				names = append(names, stats[ci].Name)
			}
		}
		parts = append(parts, "("+strings.Join(names, " + ")+")")
	}
	return strings.Join(parts, " then "), n
}

// cs2SnapSigma is the reference's cs2 snap: a measured cs2 within this
// many standard errors of 1 is taken as 1.
const cs2SnapSigma = 3.0

// asyncLink reports whether link l, a call of visit l.owner (of service owner) to callee, is one the visit did not wait
// for: on an async edge (prepass.async) and not waited for (notWaited), timed as collectCalls times it.
func asyncLink(g []otlp.Span, l link, owner, callee int32, resPod []int32, pp *prepass) bool {
	if !pp.async[edgeKey{owner, callee}] {
		return false
	}
	k := l.client
	if k < 0 {
		k = l.server // no client span: the call is timed by the callee's span
	}
	c := call{start: g[k].Start, end: g[k].End, ownClock: resPod[g[k].Res] == resPod[g[l.owner].Res]}
	if l.client >= 0 {
		c.quant = pp.quantPod[resPod[g[k].Res]]
	}
	return notWaited(c, g[l.owner].End)
}

// notWaited reports whether a call cannot have been waited for by a visit that ends at end: it starts after the
// visit's span has ended, or its CLIENT span, on the visit's own pod clock, ends after the span (a fire-and-forget
// call sent just before the response). A call timed on another clock is judged by its start only. The end of a CLIENT span
// may follow the visit's end by a quarter of a millisecond (clock jitter); a caller that stamps its spans on whole
// milliseconds (quantized.go) closes the visit's span before the last client span's end is read, by up to a millisecond
// (in the Demo, 8.4% of the front end's calls to the cart and 9.3% of those to the ad service end more than a quarter of a
// millisecond after the front end's span, by up to 0.8 ms, though it waited for them), and is allowed that.
func notWaited(c call, end uint64) bool {
	tol := uint64(earlyCallNs)
	if c.quant {
		tol = quantSlackNs
	}
	return c.start >= end || (c.ownClock && c.end > end+tol)
}

// notModelledWarning names the services that have spans in the capture and no SERVER span: the model has a service for a
// SERVER span (a visit that holds a worker), so a message consumer, a load generator or a batch job that only calls out is not
// in it, and a patch cannot name it. kinds is each such service's span kinds, a bit for each kind.
func notModelledWarning(kinds map[int32]uint8, nameList []string) string {
	var parts []string
	for id, k := range kinds {
		var ks []string
		for _, kind := range []struct {
			bit  uint8
			name string
		}{{otlp.KindClient, "CLIENT"}, {otlp.KindConsumer, "CONSUMER"}, {otlp.KindProducer, "PRODUCER"}, {otlp.KindInternal, "INTERNAL"}} {
			if k>>kind.bit&1 == 1 && (kind.bit != otlp.KindInternal || len(ks) == 0) { // INTERNAL only when it is all there is
				ks = append(ks, kind.name)
			}
		}
		parts = append(parts, fmt.Sprintf("%s (%s spans)", nameList[id], strings.Join(ks, ", ")))
	}
	if len(parts) == 0 {
		return ""
	}
	slices.Sort(parts)
	if len(parts) == 1 {
		return "1 service emits no SERVER span, so it is not in the model (the model has a service for a SERVER span, a visit that " +
			"holds a worker; --patch cannot name it): " + parts[0]
	}
	return fmt.Sprintf("%d services emit no SERVER span, so they are not in the model (the model has a service for a SERVER span, a "+
		"visit that holds a worker; --patch cannot name them): %s", len(parts), strings.Join(parts, ", "))
}

// maxBrokenShare is the reference's limit on traces that miss a span.
const maxBrokenShare = 0.05

// brokenError is the refusal of a capture with too many broken traces: what
// they lost, by cause, and what to check.
func brokenError(t *acc, names []string, pp *prepass, nameList []string, edges []lostEdge) error {
	var parts []string
	if n := t.brokenBy[lossCalledTop]; n > 0 {
		var svcs []string
		for i, c := range t.calledTop {
			if c > 0 && i < len(names) {
				svcs = append(svcs, fmt.Sprintf("%s, called on %.1f%% of its %d visits", names[i],
					100*float64(pp.srvCalled[i])/float64(pp.srvSpans[i]), pp.srvSpans[i]))
			}
		}
		parts = append(parts, fmt.Sprintf("%d have a SERVER span whose parent is not in the capture, of a service that "+
			"services in the capture call (%s): a caller of it was lost, since a service counts as entered from outside "+
			"only when most of its server spans have no caller in the capture", n, strings.Join(svcs, "; ")))
	}
	if n := t.brokenBy[lossBelowTop]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d have a span whose parent is not in the capture below a top that is in it "+
			"(a SERVER span with no parent), or on a span that is not a SERVER or CONSUMER span (the spans, by service and kind: %s)",
			n, orphanList(t.lostBelow, nameList)))
	}
	if n := t.brokenBy[lossCallee]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d have a call to a service with no span of that service under it (%s)", n, edgeList(edges)))
	}
	return fmt.Errorf("%d of %d traces miss spans (%.1f%%): %s. The queue reconstruction needs every request and the "+
		"call structure whole requests, so above %.0f%% the result is not trustworthy -- check the exporters' queues "+
		"and the sampling, and that the capture holds whole requests (not the end of one window and the start of another)",
		t.broken, t.traces, 100*float64(t.broken)/float64(t.traces), strings.Join(parts, "; "), 100*maxBrokenShare)
}

// chunkSize is how many trace groups one task of eachChunk takes: at least
// minChunkGroups, and at most maxChunks chunks. It depends on the capture only,
// not on the number of cores: the chunks' sums are merged in chunk order, so
// the same capture gives the same bits on any machine (with chunks sized by
// GOMAXPROCS the glue, its cs2 and the branch correlations differed in the
// last bits between core counts).
func chunkSize(nGroups int) int {
	return max(minChunkGroups, (nGroups+maxChunks-1)/maxChunks)
}

const (
	minChunkGroups = 64
	maxChunks      = 256
)

// eachChunk calls f on every chunk of the trace groups, on all cores. f gets
// the chunk's index and its groups [lo, hi); each index is called once.
func eachChunk(nGroups, chunk int, f func(ci, lo, hi int)) {
	workers := runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	next := make(chan [2]int, workers*4)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range next {
				f(r[0]/chunk, r[0], r[1])
			}
		}()
	}
	for g := 0; g < nGroups; g += chunk {
		next <- [2]int{g, min(g+chunk, nGroups)}
	}
	close(next)
	wg.Wait()
}

const idleMin = 100

// earlyCallNs is jitterMs in ns: a visit's first call that starts this much
// before its own span is not clock jitter. More than earlyCallShare of a
// service's visits doing so is reported (earlyWarning).
const (
	earlyCallNs    = 250_000
	earlyCallShare = 0.01
)

// plural is "1 worker" or "n workers".
func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// earlyWarning describes a service whose first call starts before the visit's
// span on a share sh of its visits (n of them, own of those timed on the
// visit's own pod clock, by up to maxMs): what the clocks say about the cause,
// and which way it moves the reading (workers, mean). The worker is taken as
// freed at the span's start there. A visit freed at its arrival that had to
// wait fits no first-come-first-served queue, so many of them make the rebuild
// add workers and fold the waits into the service time (it reads high);
// otherwise the work before the call is lost (it reads low). Which one
// happened is read from skip, the queue rebuilt with every visit and those
// calls skipped (the worker held to the visit's next call in its span, or its
// end); nil for the rise estimator. On one pod's clock that is the reading;
// with clock skew it counts the call itself as work, so it is high, and the
// truth lies between the two.
func earlyWarning(name string, sh float64, n, own int64, maxMs float64, workers int, mean float64, skip *svcQueue) string {
	var cause string
	switch {
	case own == n:
		cause = fmt.Sprintf("a CLIENT span of its own starts before the visit's span (by up to %.2f ms) on the same pod's "+
			"clock, so not clock skew: the server span starts late, after work that already called out, or the call names "+
			"the wrong parent", maxMs)
	case own == 0:
		cause = fmt.Sprintf("the first call starts before the visit's own span (by up to %.2f ms): clock skew between pods "+
			"(a call with no CLIENT span is timed by the callee's clock, a CLIENT span by its own pod's)", maxMs)
	default:
		cause = fmt.Sprintf("the first call starts before the visit's own span (by up to %.2f ms): on %d of them on another "+
			"pod's clock (clock skew between pods), on %d a CLIENT span of its own on the same clock (the server span starts "+
			"late, or the call names the wrong parent)", maxMs, n-own, own)
	}
	effect := "its service time can read low (the work before the call is lost) or, if such visits had to wait, its " +
		"worker count and service time high"
	if skip != nil && skip.jobs >= 2 {
		w2, m2 := skip.workers, skip.relMean
		read := fmt.Sprintf("with those calls skipped (the worker held to the visit's next call in its span, or its end) "+
			"its queue has %s and %.3g ms of service time, against %d and %.3g ms", plural(w2, "worker"), m2, workers, mean)
		if own < n {
			read += " (with clock skew that reading counts the call itself as work, so the truth lies between the two)"
		}
		switch {
		case mean > 1.1*m2:
			effect = read + ": visits freed at their arrival that still had to wait fit no first-come-first-served queue, " +
				"so the rebuild adds workers and folds waits into the service time: it reads high"
			if workers > w2 {
				effect += ", and so does the worker count"
			}
		case mean < 0.9*m2:
			effect = read + ": its service time reads low (the work before the call is lost)"
			if workers > w2 {
				effect += ", and its worker count high"
			}
		case workers > w2:
			effect = read + ": its worker count reads high"
		default:
			effect = read + ": little difference"
		}
	}
	return fmt.Sprintf("%s: on %.1f%% of its visits %s; the worker is taken as freed at the span's start there: %s",
		name, 100*sh, cause, effect)
}

// overlapEpsilonNs is how far a busy segment's end is pulled in, and the
// length (twice it) below which a segment is ignored: a worker is freed a few
// microseconds before the span records the next call, and clocks on different
// pods differ by more than that, so shorter overlaps and gaps are noise.
const overlapEpsilonNs = 100_000

// moments are running sums of own times.
type moments struct{ n, sum, sq float64 }

func (m *moments) add(n int64, sum, sq float64) {
	m.n, m.sum, m.sq = m.n+float64(n), m.sum+sum, m.sq+sq
}

func (m moments) mean() float64 {
	if m.n == 0 {
		return 0
	}
	return m.sum / m.n
}

func (m moments) variance() float64 {
	if m.n < 2 {
		return 0
	}
	mu := m.sum / m.n
	return (m.sq - m.n*mu*mu) / (m.n - 1)
}

// workersFromK reads the workers per pod from how the own time depends on the
// number of visits already busy at arrival: it is flat while a worker is free
// and rises once the arrival has to wait. It also returns the sums over the
// visits that found a free worker (the bins below the rise), which give the
// service time. saturated reports whether a rise was found; if not, the peak
// concurrency is returned (a lower bound) and every bin counts as free.
//
// Each bin is compared with the pool of the bins below it, not with the empty
// pod alone: with few visits at k = 0 a low mean there would make every later
// bin look like a rise. A rise at k busy must also be large enough to be
// queueing: an arrival that finds all k workers busy waits for the first of
// them to finish, about S/(k+1) even for a regular service, and the floor
// (30% of S/k, at least 10% of S) stays below that.
func workersFromK(h *byK) (workers int, saturated bool, free moments, ok bool) {
	// Seed the baseline with the lowest bins until it holds idleMin visits. With many workers
	// the empty pod alone is rare (8 workers at 78%: about 1 visit in 500).
	k := 0
	for ; k <= maxK && free.n < idleMin; k++ {
		free.add(h.n[k], h.sum[k], h.sq[k])
	}
	if free.n < idleMin {
		return h.peak, false, free, false
	}
	for ; k <= maxK; k++ {
		if h.n[k] < 30 {
			continue
		}
		base := free.mean()
		nk := float64(h.n[k])
		mk := h.sum[k] / nk
		se := math.Sqrt(math.Max(free.variance(), 0)/free.n + math.Max(0, (h.sq[k]-nk*mk*mk)/(nk-1))/nk)
		if base > 0 && mk-base > 3*se && mk-base > math.Max(0.1, 0.3/float64(k))*base {
			return k, true, free, true
		}
		free.add(h.n[k], h.sum[k], h.sq[k])
	}
	return h.peak, false, free, true
}

// poolBelow pools the visits that arrived with fewer than c others busy (the
// ones that found a free worker if the pod has c workers). It reports whether
// there are enough of them to use.
func poolBelow(h *byK, c int) (m moments, ok bool) {
	for k := 0; k < c && k <= maxK; k++ {
		m.add(h.n[k], h.sum[k], h.sq[k])
	}
	return m, m.n >= idleMin
}

// sweepByK walks each pod's busy segments in time order. For every visit it
// counts the other visits doing their own work at its arrival, and it tracks
// the peak number of visits busy at once. It returns one byK per service.
func sweepByK(segs []seg, vrec []visitRec, nAll int) []*byK {
	out := make([]*byK, nAll)
	for i := range out {
		out[i] = &byK{}
	}
	type ev struct {
		t    uint64
		kind uint8 // 0 end, 1 query, 2 start: at one instant ends, then queries, then starts
		i    int32 // visit index for queries
	}
	segsBy := map[int32][]seg{}
	for _, s := range segs {
		segsBy[s.pod] = append(segsBy[s.pod], s)
	}
	visBy := map[int32][]int32{}
	for i := range vrec {
		visBy[vrec[i].pod] = append(visBy[vrec[i].pod], int32(i))
	}
	pods := make([]int32, 0, len(visBy))
	for pod := range visBy {
		pods = append(pods, pod)
	}
	slices.Sort(pods) // the per-service sums below then run in a fixed order
	for _, pod := range pods {
		vs := visBy[pod]
		ss := segsBy[pod]
		evs := make([]ev, 0, 2*len(ss)+len(vs))
		for _, s := range ss {
			pull := uint64(overlapEpsilonNs)
			if s.end-s.start <= 2*overlapEpsilonNs {
				if !s.leaf || s.end-s.start < 2 {
					continue // a few microseconds before the first call out are not work
				}
				// a visit that made no calls is all work, however short (a cache, a
				// lookup); its own clock bounds it, so it keeps its first half (the
				// end is pulled in by half its length instead of by 0.1 ms)
				pull = (s.end - s.start) / 2
			}
			evs = append(evs, ev{s.start, 2, 0}, ev{s.end - pull, 0, 0})
		}
		for _, i := range vs {
			evs = append(evs, ev{vrec[i].start, 1, i})
		}
		slices.SortFunc(evs, func(a, b ev) int {
			if a.t != b.t {
				if a.t < b.t {
					return -1
				}
				return 1
			}
			return int(a.kind) - int(b.kind)
		})
		active, best := 0, 0
		for _, e := range evs {
			switch e.kind {
			case 0:
				active--
			case 2:
				active++
				best = max(best, active)
			case 1:
				v := &vrec[e.i]
				k := min(active, maxK)
				h := out[v.svc]
				x := float64(v.own)
				h.n[k]++
				h.sum[k] += x
				h.sq[k] += x * x
			}
		}
		for _, i := range vs {
			h := out[vrec[i].svc]
			h.peak = max(h.peak, best)
			break // all visits of a pod belong to one service
		}
	}
	return out
}

// spanIndex maps span ids to their first position in a trace group.
func spanIndex(g []otlp.Span) map[uint64]int {
	pos := make(map[uint64]int, len(g))
	for i := range g {
		if _, dup := pos[g[i].ID]; !dup {
			pos[g[i].ID] = i
		}
	}
	return pos
}

// Why a walk up the parents stopped without a span of the kind it looks for.
const (
	stopFound    = iota // it found one
	stopRoot            // a span with no parent (or a parent cycle)
	stopMissing         // a parent that is not in the capture
	stopConsumer        // a CONSUMER span: a message consumer's own unit of work
)

// ancestor walks up from span k through internal (and producer) spans to the
// nearest server or client span; -1 if there is none, with why the walk
// stopped. A consumer span ends the walk with -1 (stopConsumer): a message
// consumer's work is its own unit of work, not the producing request's, so
// what it calls is not a call of the visit that sent the message. The walk is
// bounded by the group size (a parent cycle ends it), not by a fixed depth:
// instrumented frameworks nest many internal spans.
func ancestor(g []otlp.Span, pos map[uint64]int, k int) (idx int, stop uint8) {
	cur := k
	for hop := 0; hop < len(g); hop++ {
		pid := g[cur].Parent
		if pid == 0 {
			return -1, stopRoot
		}
		p, ok := pos[pid]
		if !ok {
			return -1, stopMissing
		}
		switch g[p].Kind {
		case otlp.KindServer, otlp.KindClient:
			return p, stopFound
		case otlp.KindConsumer:
			return -1, stopConsumer
		}
		cur = p
	}
	return -1, stopRoot
}

// ownerVisit returns the server span that owns span k (a client span), or -1
// with why there is none.
func ownerVisit(g []otlp.Span, pos map[uint64]int, k int) (int, uint8) {
	cur := k
	for hop := 0; hop < len(g); hop++ {
		p, stop := ancestor(g, pos, cur)
		if p < 0 {
			return -1, stop
		}
		if g[p].Kind == otlp.KindServer {
			return p, stopFound
		}
		cur = p
	}
	return -1, stopRoot
}

// link is a server span of a service, the client span over it (-1: none) and
// its caller's visit (-1: none, an entry). consumer: there is no caller's
// visit because a message consumer's span is above it. lost: there is none
// because the walk up to it met a span whose parent is not in the capture.
type link struct {
	server, client, owner int
	consumer, lost        bool
}

// loose is a client span with no server span under it, the peer it calls
// (callee), and the visit it belongs to (-1: none; consumer as for link).
type loose struct {
	client, owner int
	callee        int32
	consumer      bool
}

// traceCalls is one trace's calls: per owner visit (a server span) its calls,
// to services and to external peers, sorted by start; every server span's link;
// and the client spans with no server span under them.
type traceCalls struct {
	calls map[int][]call
	links []link
	loose []loose
}

// collectCalls reads one trace's calls (the pre-pass and processTrace share it).
func collectCalls(g []otlp.Span, pos map[uint64]int, resSvc, resPod []int32, quantPod []bool, nSvc int, unknownPeer int32) traceCalls {
	svcOf := func(k int) int32 { return resSvc[g[k].Res] }
	tc := traceCalls{calls: map[int][]call{}}
	matched := map[int]bool{}
	for k := range g {
		if g[k].Kind != otlp.KindServer || svcOf(k) < 0 {
			continue
		}
		p, stop := ancestor(g, pos, k)
		callee := svcOf(k)
		l := link{server: k, client: -1, owner: -1, consumer: stop == stopConsumer, lost: stop == stopMissing}
		if p >= 0 {
			if g[p].Kind == otlp.KindClient {
				matched[p] = true
				l.client = p
				owner, ostop := ownerVisit(g, pos, p)
				if owner >= 0 && svcOf(owner) >= 0 {
					cs := &g[p]
					tc.calls[owner] = append(tc.calls[owner], call{callee: callee, start: cs.Start, end: cs.End,
						clientDur: cs.Dur(), hasServer: true, sstart: g[k].Start, send: g[k].End,
						ownClock: resPod[cs.Res] == resPod[g[owner].Res], quant: quantPod[resPod[cs.Res]]})
					l.owner = owner
				} else {
					l.consumer, l.lost = ostop == stopConsumer, ostop == stopMissing
				}
			} else if g[p].Kind == otlp.KindServer && svcOf(p) >= 0 {
				// no client span: the call is timed by the callee's span, on the callee's clock
				tc.calls[p] = append(tc.calls[p], call{callee: callee, start: g[k].Start, end: g[k].End,
					clientDur: g[k].Dur(), hasServer: true, ownClock: resPod[g[k].Res] == resPod[g[p].Res]})
				l.owner = p
			}
		}
		tc.links = append(tc.links, l)
	}
	for k := range g {
		if g[k].Kind != otlp.KindClient || matched[k] {
			continue
		}
		peer := unknownPeer
		if g[k].Peer >= 0 {
			peer = int32(nSvc) + g[k].Peer
		}
		owner, stop := ownerVisit(g, pos, k)
		lc := loose{client: k, owner: -1, callee: peer, consumer: stop == stopConsumer}
		if owner >= 0 && svcOf(owner) >= 0 {
			lc.owner = owner
			tc.calls[owner] = append(tc.calls[owner], call{callee: peer, start: g[k].Start, end: g[k].End,
				clientDur: g[k].Dur(), ownClock: resPod[g[k].Res] == resPod[g[owner].Res], quant: quantPod[resPod[g[k].Res]]})
		}
		tc.loose = append(tc.loose, lc)
	}
	for _, cs := range tc.calls {
		slices.SortFunc(cs, func(x, y call) int {
			if x.start != y.start {
				if x.start < y.start {
					return -1
				}
				return 1
			}
			return int(x.callee - y.callee)
		})
	}
	return tc
}

// prepass is what processTrace needs to know about the whole capture, read in
// a first pass over every trace (broken ones included; it only counts):
//   - answeredBy: the addresses (peer and port) some service answers on: somewhere
//     a client span to the address has a server span under it, and that service.
//     A call to such an address with no server span under it lost its callee's
//     span. The port matters: services on one host (localhost) answer on theirs, a
//     database on that host on its own. addrCalls counts, per caller and address,
//     the calls that did find their callee's span (the share a lost edge lost).
//   - per service, its server spans and those with a caller's visit in the
//     capture; per service name (resName), its CONSUMER spans and those whose
//     parent is not in the capture or that have none. They decide which services
//     and consumers are entered from outside (entryLike, consumerLike).
//   - per edge (caller, callee or peer), the calls, those that start at or
//     after the caller's span has ended (sent after the response: async), and
//     those of them timed on another pod's clock (the callee's, with no CLIENT
//     span), where a clock that runs ahead looks the same.
type prepass struct {
	answeredBy          []int32           // per address: the service whose server span sits under a call to it (-1: none, not answered; -2: several)
	addrCalls           map[lostKey]int64 // per caller (service name) and address: the calls that have a server span under them
	srvSpans, srvCalled []int64
	conSpans, conTops   []int64
	callN, callAfter    map[edgeKey]int64
	afterOther          map[edgeKey]int64

	entryLike, consumerLike []bool
	async                   map[edgeKey]bool
	quantPod                []bool // per pod: its spans start on whole milliseconds (quantized.go)
}

func newPrepass(nAddr, nSvc, nNames int) *prepass {
	by := make([]int32, nAddr)
	for i := range by {
		by[i] = -1
	}
	return &prepass{answeredBy: by, addrCalls: map[lostKey]int64{}, srvSpans: make([]int64, nSvc),
		srvCalled: make([]int64, nSvc), conSpans: make([]int64, nNames), conTops: make([]int64, nNames), callN: map[edgeKey]int64{},
		callAfter: map[edgeKey]int64{}, afterOther: map[edgeKey]int64{}}
}

// answered reports whether some service answers on the address: somewhere a client span to it has a server span under it.
func (pp *prepass) answered(addr int32) bool { return pp.answeredBy[addr] != -1 }

// answeredByMerge combines two readings of which service answers on an address (answeredBy).
func answeredByMerge(x, y int32) int32 {
	switch {
	case x == -1:
		return y
	case y == -1 || x == y:
		return x
	}
	return -2
}

func (pp *prepass) addTrace(g []otlp.Span, resSvc, resPod, resName []int32, nSvc int, unknownPeer int32) {
	pos := spanIndex(g)
	tc := collectCalls(g, pos, resSvc, resPod, pp.quantPod, nSvc, unknownPeer)
	for _, l := range tc.links {
		svc := resSvc[g[l.server].Res]
		pp.srvSpans[svc]++
		if l.owner >= 0 {
			pp.srvCalled[svc]++
		}
		if l.client >= 0 && g[l.client].Addr >= 0 {
			addr := g[l.client].Addr
			pp.answeredBy[addr] = answeredByMerge(pp.answeredBy[addr], svc)
			pp.addrCalls[lostKey{resName[g[l.client].Res], addr}]++
		}
	}
	for owner, cs := range tc.calls {
		caller := resSvc[g[owner].Res]
		for _, c := range cs {
			k := edgeKey{caller, c.callee}
			pp.callN[k]++
			if notWaited(c, g[owner].End) {
				pp.callAfter[k]++
				if !c.ownClock {
					pp.afterOther[k]++
				}
			}
		}
	}
	for k := range g {
		if g[k].Kind != otlp.KindConsumer {
			continue
		}
		n := resName[g[k].Res]
		pp.conSpans[n]++
		if _, ok := pos[g[k].Parent]; g[k].Parent == 0 || !ok {
			pp.conTops[n]++
		}
	}
}

// merge adds another chunk's counts (integers: the order does not matter).
func (pp *prepass) merge(o *prepass) {
	for i, v := range o.answeredBy {
		pp.answeredBy[i] = answeredByMerge(pp.answeredBy[i], v)
	}
	for k, n := range o.addrCalls {
		pp.addrCalls[k] += n
	}
	for i := range o.srvSpans {
		pp.srvSpans[i] += o.srvSpans[i]
		pp.srvCalled[i] += o.srvCalled[i]
	}
	for i := range o.conSpans {
		pp.conSpans[i] += o.conSpans[i]
		pp.conTops[i] += o.conTops[i]
	}
	for k, n := range o.callN {
		pp.callN[k] += n
	}
	for k, n := range o.callAfter {
		pp.callAfter[k] += n
	}
	for k, n := range o.afterOther {
		pp.afterOther[k] += n
	}
}

// entryLikeShare: a service is entered from outside the capture when more than
// this share of its server spans have no caller's visit in the capture (they
// are roots, name a parent outside the capture, or are fed by a message
// consumer); likewise a consumer, for its CONSUMER spans. Then a span of it
// whose parent is not in the capture is an entry, a request from an ingress, a
// gateway or a browser whose spans are not exported here, or a message from a
// producer outside the capture. Otherwise it is a lost caller. Why half: of a
// service that services in the capture call, the spans with no caller's visit
// are its lost callers, so their share is the share of the calls into it that
// the capture lost; below half its absent parents are counted as lost (and
// above 5% of the traces the capture is refused, as before), and only a capture
// that loses more than half of the calls into one service, with no captured
// top above them, passes it off as entries. A service that some service in the
// capture calls on a few of its visits (an admin tool, a batch job) stays an
// entry, as do the requests of a gateway that calls several captured services,
// or retries one, in one trace. A trace whose top is in the capture (a SERVER
// span with no parent) has no caller outside it: an absent parent there is a
// lost span whatever its service.
const entryLikeShare = 0.5

// decide sets entryLike, consumerLike and async from the merged counts. A
// caller's calls to a callee are sent after its response (async: they load the
// callee, but the caller does not wait for them) when at least parallelShare of
// them start at or after the caller's span has ended, the share the overlap
// rule uses; a call that starts after the span cannot be on the span's latency
// path (on one clock). Below that they are on the path: a few that start after
// the end are clock jitter or skew (a call with no CLIENT span is timed by the
// callee's clock), and a mix is warned about (asyncMixed).
func (pp *prepass) decide() {
	pp.entryLike = make([]bool, len(pp.srvSpans))
	for i, n := range pp.srvSpans {
		pp.entryLike[i] = n > 0 && float64(n-pp.srvCalled[i]) > entryLikeShare*float64(n)
	}
	pp.consumerLike = make([]bool, len(pp.conSpans))
	for i, n := range pp.conSpans {
		pp.consumerLike[i] = n > 0 && float64(pp.conTops[i]) > entryLikeShare*float64(n)
	}
	pp.async = map[edgeKey]bool{}
	for k, n := range pp.callN {
		if a := pp.callAfter[k]; a > 0 && float64(a) >= parallelShare*float64(n) {
			pp.async[k] = true
		}
	}
}

// asyncMixed returns the edges on which more than 1 - parallelShare of the
// calls, but less than parallelShare, are not waited for (notWaited), with
// that share.
func (pp *prepass) asyncMixed() map[edgeKey]float64 {
	out := map[edgeKey]float64{}
	for k, n := range pp.callN {
		if sh := float64(pp.callAfter[k]) / float64(n); sh > 1-parallelShare && !pp.async[k] {
			out[k] = sh
		}
	}
	return out
}

// What a broken trace lost (acc.brokenBy).
const (
	lossCalledTop = iota // a SERVER span whose parent is not in the capture, of a service the capture's services call (not entryLike)
	lossBelowTop         // another span whose parent is not in the capture: below a top in the capture, not a SERVER or CONSUMER span, or a consumer fed in the capture
	lossCallee           // a call to an address a service answers on, with no server span of it under the call
	nLoss
)

// topLoss reports what a trace lost above its spans, as a bit set of the loss
// causes (0: nothing): every span whose parent is not in the capture must be a
// SERVER span of an entryLike service or a CONSUMER span of a consumerLike
// one, in a trace whose top is not in the capture (no SERVER span of a service
// without a parent; topIn reports one). called gets the services of SERVER
// spans that fail only because their service is not entryLike; orphan gets
// each span lost below a top.
func topLoss(g []otlp.Span, pos map[uint64]int, resSvc, resName []int32, pp *prepass, called func(svc int32), orphan func(k int)) (loss uint8, topIn bool) {
	for k := range g {
		if g[k].Parent == 0 && g[k].Kind == otlp.KindServer && resSvc[g[k].Res] >= 0 {
			topIn = true
			break
		}
	}
	for k := range g {
		pid := g[k].Parent
		if pid == 0 {
			continue
		}
		if _, ok := pos[pid]; ok {
			continue
		}
		svc := resSvc[g[k].Res]
		switch {
		case topIn:
			loss |= 1 << lossBelowTop
			orphan(k)
		case g[k].Kind == otlp.KindServer && svc >= 0:
			if !pp.entryLike[svc] {
				loss |= 1 << lossCalledTop
				called(svc)
			}
		case g[k].Kind == otlp.KindConsumer && pp.consumerLike[resName[g[k].Res]]:
		default:
			loss |= 1 << lossBelowTop
			orphan(k)
		}
	}
	return loss, topIn
}

// processTrace adds one trace's visits and calls to a. keepSegs keeps the
// busy segments, which only the rise estimator reads. pp holds the pre-pass's
// decisions over the whole capture.
//
// A visit's own time is its span minus the union of the calls it waits for
// (not the async ones). The part up to its release (the first call out it
// waits for, or the end) holds the worker: it is the busy segment and the
// service time. The rest is glue: between and after the calls the worker is
// free, so that time queues nobody and is a delay.
//
// The trace is broken when a span names a parent that is not in the capture,
// unless it is an entry from outside (topLoss), or when a call to a peer that a
// service answers on has no server span under it: a span was lost (an
// exporter's full queue, a pod that went away, a capture cut mid-request). Its
// visits still count in the queues and the rates (and the route each of its
// calls reached, in the caller's route weights, when the caller's visit is in
// the capture); its calls and entries do not, since a lost caller would turn
// its callees into entries and a lost callee its calls into external delays.
// The reference counts every absent parent as a loss.
func processTrace(d *otlp.Data, g []otlp.Span, a *acc, resSvc, resPod, resName []int32, nSvc int, unknownPeer int32, keepSegs bool,
	pp *prepass) {
	svcOf := func(k int) int32 { return resSvc[g[k].Res] }
	pos := spanIndex(g)
	a.traces++
	var calledTop []int32
	loss, topIn := topLoss(g, pos, resSvc, resName, pp, func(s int32) {
		if !slices.Contains(calledTop, s) {
			calledTop = append(calledTop, s)
		}
	}, func(k int) { a.lostBelow[orphanKey{resName[g[k].Res], g[k].Kind}]++ })
	tc := collectCalls(g, pos, resSvc, resPod, pp.quantPod, nSvc, unknownPeer)
	for _, lc := range tc.loose {
		if addr := g[lc.client].Addr; addr >= 0 && pp.answered(addr) {
			loss |= 1 << lossCallee
			a.lostEdges[lostKey{resName[g[lc.client].Res], addr}]++
		}
	}
	broken := loss != 0
	if broken {
		a.broken++
		for c := 0; c < nLoss; c++ {
			if loss>>c&1 == 1 {
				a.brokenBy[c]++
			}
		}
		for _, s := range calledTop {
			a.calledTop[s]++
		}
	} else {
		for _, l := range tc.links {
			callee, sp := svcOf(l.server), &g[l.server]
			if l.owner < 0 {
				a.routeEdge[routeEdgeKey{-1, callee, sp.Route}]++
			}
			switch {
			case l.owner < 0:
				a.entries[callee]++
				a.entryN++
				if l.consumer {
					a.consumerEn[callee]++
					a.consumerN++
				}
				a.entryMin = min(a.entryMin, sp.Start)
				a.entryMax = max(a.entryMax, sp.Start)
			case l.client >= 0:
				cs := &g[l.client]
				a.hop(svcOf(l.owner), callee, cs.Dur()-sp.Dur())
				a.clientSum[callee] += cs.Dur()
				a.clientN[callee]++
			default:
				a.clientSum[callee] += sp.Dur()
				a.clientN[callee]++
			}
		}
		// client calls with no server child: external delays (or, under a message consumer, on no request's path)
		for _, lc := range tc.loose {
			switch {
			case lc.owner >= 0:
				a.clientSum[lc.callee] += g[lc.client].Dur()
				a.clientN[lc.callee]++
				a.unmatched++
			case lc.consumer:
				a.consumerX++
			}
		}
	}
	// A call counts toward its caller's route weights in every trace (routes.go): a broken trace may have lost a span
	// anywhere, and a link that keeps its caller's visit still says which route that caller reached, so a caller keeps
	// its own mix however often its traces break against another's (lean/Rbproofs/Lanes/Infer.lean, INFER-1). A
	// broken trace's visits whose caller is known (its visit, or an entry as a complete trace would have it: nothing
	// lost above, or an entry-like service's span entered from outside a trace whose top is not in the capture) count
	// apart, for each route's completeness.
	for _, l := range tc.links {
		callee, sp := svcOf(l.server), &g[l.server]
		if l.owner >= 0 && !asyncLink(g, l, svcOf(l.owner), callee, resPod, pp) {
			// a caller's route weights are those of the calls it waits for: a fire-and-forget visit to another
			// route of the callee is not one of them (lean/Rbproofs/Lanes/Infer.lean, INFER-2)
			a.routeEdge[routeEdgeKey{svcOf(l.owner), callee, sp.Route}]++
		}
		if !broken || sp.End < sp.Start {
			continue
		}
		known := l.owner >= 0 || !l.lost
		if !known && !topIn && pp.entryLike[callee] {
			_, in := pos[sp.Parent]
			known = !in // the span itself names the parent not in the capture (topLoss)
		}
		if known {
			a.routeKnown[routeKey{callee, sp.Route}]++
		}
	}
	var structCs []call
	for k := range g {
		if g[k].Kind != otlp.KindServer || svcOf(k) < 0 || g[k].End < g[k].Start {
			continue
		}
		svc := svcOf(k)
		v := &g[k]
		cs := tc.calls[k] // sorted by start
		// The calls the visit waits for: all but those on an edge whose calls are sent after the response. Its
		// call structure (stages, overlap, branch correlation) is read from those that also start before its end.
		structCs = structCs[:0]
		first := -1 // the first call it waits for
		for j := range cs {
			// on an async edge, a call the visit could have waited for (it starts and, on the visit's own clock, ends
			// within the span) is still one it waited for
			cs[j].async = pp.async[edgeKey{svc, cs[j].callee}] && notWaited(cs[j], v.End)
			if cs[j].async {
				continue
			}
			if first < 0 {
				first = j
			}
			if cs[j].start < v.End {
				structCs = append(structCs, cs[j])
			}
		}
		// calls stamped on whole milliseconds are ordered, and told apart from calls at once, by where their callees' spans are
		placed := anyPlaced(structCs)
		var placedStages [][]int
		if placed {
			slices.SortStableFunc(structCs, func(x, y call) int {
				if xa, ya := x.qa(), y.qa(); xa != ya {
					if xa < ya {
						return -1
					}
					return 1
				}
				return int(x.callee - y.callee)
			})
			placedStages = stagesOfQ(structCs)
		}
		var covered, maxEnd uint64
		for _, c := range cs {
			if c.async {
				continue
			}
			s, e := max(c.start, v.Start), min(c.end, v.End)
			if e <= s {
				continue
			}
			if e > maxEnd {
				if s > maxEnd {
					covered += e - s
				} else {
					covered += e - maxEnd
				}
				maxEnd = e
			}
		}
		if placed {
			// the model adds the calls' times stage by stage, so the time they cover is read stage by stage too: calls
			// stamped on whole milliseconds that follow one another can overlap in the spans though they do not in time
			covered = min(stageSpans(structCs, placedStages, v.Start, v.End), v.End-v.Start)
		}
		// The release: the first call out it waits for, or the end, clipped to the span. A call that starts after
		// the span has ended does not free the worker before the end; one that starts before the span (a callee
		// or a client span on a clock that runs behind, or a span that starts late) frees it at the start.
		rel, early := v.End, false
		if first >= 0 {
			rel = cs[first].start
			if rel+earlyCallNs < v.Start {
				early = true
				a.early[svc]++
				if cs[first].ownClock {
					a.earlyOwn[svc]++
				}
				a.earlyMax[svc] = max(a.earlyMax[svc], float64(v.Start-rel)/1e6)
			}
		}
		held := min(max(rel, v.Start), v.End)
		alt := held
		if early {
			alt = v.End
			for _, c := range cs[first:] {
				if !c.async && c.start >= v.Start {
					alt = min(c.start, v.End)
					break
				}
			}
		}
		if held > v.Start && keepSegs {
			a.segs = append(a.segs, seg{resPod[v.Res], held == v.End, v.Start, held})
		}
		dur := float64(v.End-v.Start) / 1e6
		// Every call it waits for starts at or after the release, so those calls cover none of [start, held] and
		// the glue (own time after it) is not negative. It is taken in whole ns: exactly 0 when the calls fill the
		// rest.
		preNs := held - v.Start
		pre, glue := float64(preNs)/1e6, 0.0
		if ownNs := v.End - v.Start - covered; ownNs > preNs {
			glue = float64(ownNs-preNs) / 1e6
		}
		a.vrec = append(a.vrec, visitRec{resPod[v.Res], svc, v.Start, held, alt, float32(pre), early, v.Route})
		a.visits[svc]++
		a.ownSum[svc] += pre
		a.ownSq[svc] += pre * pre
		a.serverSum[svc] += dur
		if a.keep {
			if ext := d.ExtOf(v); ext.Status == 2 || ext.HTTPStatus >= 500 {
				a.errs[svc]++
			}
		}
		if broken {
			continue
		}
		a.sVisits[svc]++
		a.glueSum[svc] += glue
		a.glueSq[svc] += glue * glue
		if a.keep {
			a.glueRec = append(a.glueRec, glueSample{svc, v.Start, glue})
		}
		for j, c := range cs {
			key := edgeKey{svc, c.callee}
			ea := a.edges[key]
			if ea == nil {
				ea = &edgeAcc{}
				a.edges[key] = ea
			}
			ea.n++ // calls clipped to nothing still count as calls
			if s, e := max(c.start, v.Start), min(c.end, v.End); e > s {
				ea.offsetSum += float64(s-v.Start) / 1e6
			}
			if !slices.ContainsFunc(cs[:j], func(x call) bool { return x.callee == c.callee }) { // once per callee
				n := int32(0) // the calls to it the visit waits for; on an async edge the others are counted apart
				for _, x := range cs {
					if x.callee != c.callee {
						continue
					}
					if x.async {
						a.asyncN[key]++
					} else {
						n++
					}
				}
				if n > 0 {
					a.counts[countKey{svc, c.callee, n}]++
				}
			}
		}
		if len(structCs) == 0 {
			a.routeSigs[routeSigKey{svc, v.Route, ""}]++
			continue
		}
		stages := stagesOf(structCs)
		if placed {
			stages = placedStages
		}
		a.sigs[sigKey{svc, sigOf(structCs, stages).key()}]++
		a.routeSigs[routeSigKey{svc, v.Route, sigOf(structCs, stages).key()}]++
		if len(structCs) >= 2 {
			a.multi[svc]++
			if len(stages) < len(structCs) { // some stage holds two or more calls: they overlap
				a.overlap[svc]++
				a.recordBranches(pairKey{svc, len(structCs), ""}, structCs)
			}
		}
		for _, st := range stages {
			if len(st) < 2 {
				continue
			}
			group := make([]call, len(st))
			for x, ci := range st {
				group[x] = structCs[ci]
			}
			slices.SortStableFunc(group, func(x, y call) int { return int(x.callee - y.callee) })
			ids := make([]int32, len(group))
			for x := range group {
				ids[x] = group[x].callee
			}
			a.recordBranches(pairKey{svc, len(group), stageSig{ids}.key()}, group)
		}
	}
}

// hop records the gap of one call (its CLIENT span's time less its callee's SERVER span's) under its callee and its edge.
func (a *acc) hop(caller, callee int32, d float64) {
	if d < 0 {
		d = 0
	}
	a.hopSamples[callee] = append(a.hopSamples[callee], d)
	k := edgeKey{caller, callee}
	a.edgeGaps[k] = append(a.edgeGaps[k], d)
}

// recordBranches adds one parallel visit's call durations (or one stage's),
// in a fixed slot order (by callee, then start), to the correlation sums.
func (a *acc) recordBranches(key pairKey, cs []call) {
	k := len(cs)
	if k < 2 || k > maxSlots {
		return
	}
	ord := make([]call, k)
	copy(ord, cs)
	slices.SortStableFunc(ord, func(x, y call) int { return int(x.callee - y.callee) })
	p := a.pairs[key]
	if p == nil {
		p = &pairAcc{}
		a.pairs[key] = p
	}
	p.n++
	for i := 0; i < k; i++ {
		x := ord[i].clientDur
		p.sum[i] += x
		p.sq[i] += x * x
		for j := i + 1; j < k; j++ {
			p.cross[i][j] += x * ord[j].clientDur
		}
	}
}

// branchCorrelation is the mean pairwise correlation between the latencies of
// a service's parallel branches, from its most common fan-out width.
func branchCorrelation(pairs map[pairKey]*pairAcc, svc int32) (float64, int64) {
	var best *pairAcc
	bestK := 0
	for k, p := range pairs {
		// ties go to the narrower fan-out, not to map order
		if k.svc == svc && k.group == "" && (best == nil || p.n > best.n || (p.n == best.n && k.k < bestK)) {
			best, bestK = p, k.k
		}
	}
	return pairCorrelation(best, bestK)
}

// pairCorrelation is the mean pairwise correlation of k slots' sums (0, 0
// with fewer than 50 visits).
func pairCorrelation(best *pairAcc, bestK int) (float64, int64) {
	if best == nil || best.n < 50 {
		return 0, 0
	}
	n := float64(best.n)
	sum, cnt := 0.0, 0
	for i := 0; i < bestK; i++ {
		for j := i + 1; j < bestK; j++ {
			vi := best.sq[i]/n - (best.sum[i]/n)*(best.sum[i]/n)
			vj := best.sq[j]/n - (best.sum[j]/n)*(best.sum[j]/n)
			if vi <= 0 || vj <= 0 {
				continue
			}
			cov := best.cross[i][j]/n - best.sum[i]/n*best.sum[j]/n
			sum += cov / math.Sqrt(vi*vj)
			cnt++
		}
	}
	if cnt == 0 {
		return 0, 0
	}
	return sum / float64(cnt), best.n
}
