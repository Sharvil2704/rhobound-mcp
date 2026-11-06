// SPDX-License-Identifier: Apache-2.0

package check

import (
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/cluster"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The detectors' thresholds (DATA.md 3) [D]: placeholders until the simulator and the open-world intake set them.
const (
	skippedShare       = 0.001 // trace.spans_skipped above this share of the spans read
	clientMissingShare = 0.05  // trace.client_spans_missing per edge
	edgeLossShare      = 0.01  // trace.edge_loss per edge
	rootShare          = 0.95  // trace.context_not_propagated: a callee's SERVER spans that are roots
	addressedCalls     = 20    // ... and the calls without a SERVER span that address it
	containFactor      = 5.0   // ... or contain its root spans this many times the shuffled baseline
	errorShare         = 0.01  // trace.errors_present, trace.shedding, trace.retries, trace.probe_like_traffic
	collapsedServices  = 3     // trace.pod_identity_collapsed from this many services on one pod; two is trace.pod_shared
	skewSlackNs        = 1_000_000
	skewShare          = 0.05 // trace.clock_skew: callee spans outside their caller's CLIENT span, per pod pair
	skewMinPairs       = 20
	wholeMsMin         = 100  // trace.whole_ms_starts: spans a pod must have
	wholeMsShare       = 0.99 // ... that start on a whole millisecond
	arrivalsPerBin     = 30   // trace.arrivals_*: sub-windows of this many arrivals on average ...
	arrivalBins        = 30   // ... and at least this many of them, else trace.arrivals_unjudged
	maxArrivalBins     = 200
	dispersionZ        = 4.0 // trace.arrivals_overdispersed: index of dispersion this many standard errors above 1 ...
	dispersionMin      = 1.5 // ... and at least this
	trendZ             = 4.0 // trace.load_trend: halves this many Poisson standard errors apart ...
	trendMin           = 0.10
	churnShare         = 0.2  // trace.pod_churn: a pod absent for this share of the window at one end ...
	churnGaps          = 20.0 // ... and for this many of its mean gaps between visits
	churnMinVisits     = 20
	periodicMin        = 10   // probe-like traffic: at least this many entries of one route on one pod ...
	periodicCV         = 0.1  // ... at a fixed period (coefficient of variation of the gaps under this) ...
	periodicMaxMs      = 5.0  // ... and tiny (mean duration under this)
	tailZone           = 0.05 // trace.async_tail_truncated: the last share of the margin after the window ...
	tailShare          = 0.01 // ... reached by more than this share of the window's traces
)

type edgeKey struct{ caller, callee string }

type edgeStat struct{ withClient, withoutClient, lost int64 }

type podKey struct{ svc, pod string }

// unmatched is a CLIENT span with no SERVER span under it.
type unmatched struct {
	caller     string
	peer, addr int32
	start, end uint64
	trace      [2]uint64
}

type svcStat struct {
	server, roots, errors, shed, streams int64
	client, resends                      int64
	links                                int64
	entries                              int64
	probes                               int64
	probeEx                              []string
}

type pairStat struct {
	n, outside int64
	maxOffNs   uint64
}

type analysis struct {
	d   *otlp.Data
	m   *contract.Manifest
	opt Options
	fs  []contract.Finding

	kept        contract.KeptShare
	clusterDump *contract.ClusterDump

	// the cluster inputs, read by ClusterInputs before the analysis
	dump      *cluster.Dump
	peerNames map[string]string
	clusterFs []contract.Finding

	svc      map[string]*svcStat
	edges    map[edgeKey]*edgeStat
	answers  map[int32]map[string]int64 // address -> the services whose SERVER spans answered calls to it
	unmatch  []unmatched
	pairs    map[[2]podKey]*pairStat
	roots    map[string][][2]uint64 // root SERVER spans by service: start, end
	podOf    map[string]map[string]bool
	podSpan  map[podKey][2]int64 // all spans, whole-ms starts
	podSrv   map[podKey]*podSeen
	entries  []uint64 // entry start times
	ths      map[string]int64
	byEntry  map[string]int64
	traceN   int64
	tailOpen int64
	tailIn   int64
	periodic map[periodicKey]*periodicGroup
}

type podSeen struct {
	first, last uint64
	n           int64
}

type periodicKey struct {
	pod   podKey
	route string
}

type periodicGroup struct {
	starts []uint64
	durNs  uint64
	probe  int64 // entries already counted as probe-like
	svc    string
}

func newAnalysis(d *otlp.Data, m *contract.Manifest, opt Options) *analysis {
	return &analysis{d: d, m: m, opt: opt, kept: contract.KeptShare{Value: 1, Source: contract.KeptAssumed}, svc: map[string]*svcStat{}, edges: map[edgeKey]*edgeStat{},
		answers: map[int32]map[string]int64{}, pairs: map[[2]podKey]*pairStat{}, roots: map[string][][2]uint64{},
		podOf: map[string]map[string]bool{}, podSpan: map[podKey][2]int64{}, podSrv: map[podKey]*podSeen{},
		ths: map[string]int64{}, byEntry: map[string]int64{}, periodic: map[periodicKey]*periodicGroup{}}
}

func (a *analysis) add(f contract.Finding) { a.fs = append(a.fs, f) }

func (a *analysis) stat(s string) *svcStat {
	st := a.svc[s]
	if st == nil {
		st = &svcStat{}
		a.svc[s] = st
	}
	return st
}

func (a *analysis) run() []contract.Finding {
	a.readStats()
	a.resources()
	if a.opt.PodIdentity == PodIdentityHost {
		a.podsByHost()
	}
	a.duplicates()
	if refused(a.fs) {
		return a.fs
	}
	a.traces()
	if a.noServerSpans() {
		return a.fs
	}
	names := a.clusterFindings(a.dump, a.peerNames, a.clusterFs)
	a.edgeFindings(names)
	a.contextNotPropagated(names)
	a.serviceFindings()
	a.podFindings()
	a.arrivals()
	a.asyncTail()
	a.samplingFindings()
	return a.fs
}

// readStats: what the reader counted.
func (a *analysis) readStats() {
	st := a.d.Stats
	classes := map[string]bool{}
	if st.IDsHex > 0 {
		classes["hex"] = true
	}
	if st.IDsBase64+st.Base64Parents > 0 {
		classes["base64"] = true
	}
	if st.TraceIDs64 > 0 {
		classes["hex64"] = true
	}
	ex := st.SkippedExamples
	switch {
	case len(classes) > 1:
		a.add(contract.NewFinding(contract.TraceIDsMixed).WithNumber("base64", float64(st.IDsBase64+st.Base64Parents)).
			WithNumber("hex64", float64(st.TraceIDs64)).WithNumber("hex", float64(st.IDsHex)).WithExamples(ex...))
	case classes["base64"]:
		a.add(contract.NewFinding(contract.TraceIDsBase64).WithNumber("spans", float64(st.IDsBase64)).WithExamples(ex...))
	case classes["hex64"]:
		a.add(contract.NewFinding(contract.TraceTraceID64Bit).WithNumber("spans", float64(st.TraceIDs64)).WithExamples(ex...))
	}
	if read := st.SpansRead; read > 0 && float64(a.d.Skipped) > skippedShare*float64(read) {
		a.add(contract.NewFinding(contract.TraceSpansSkipped).WithNumber("skipped", float64(a.d.Skipped)).
			WithNumber("share", round6(float64(a.d.Skipped)/float64(read))).WithNumber("ids", float64(st.SkippedIDs)).
			WithNumber("no_service", float64(st.SkippedService)).WithNumber("times", float64(st.SkippedTime)).WithExamples(ex...))
	}
	if st.FloatTimes > 0 {
		a.add(contract.NewFinding(contract.TraceTimeNotInteger).WithNumber("spans", float64(st.FloatTimes)))
	}
	if st.PartialTail > 0 {
		f := contract.NewFinding(contract.TracePartialTailDropped).WithNumber("lines", float64(st.PartialTail))
		if n := len(a.d.Files); n > 0 {
			f = f.WithSubject(filepath.Base(a.d.Files[n-1]))
		}
		a.add(f)
	}
	var personal int64
	for i := range a.d.Spans {
		if a.d.ExtOf(&a.d.Spans[i]).Personal {
			personal++
		}
	}
	if personal > 0 {
		a.add(contract.NewFinding(contract.TracePersonalDataSuspect).WithNumber("spans", float64(personal)).
			WithExamples(st.PersonalExamples...))
	}
}

// resources: service names, environments, images, function platforms and pod identities. trace.image_digest_missing
// reads the digests of every resource; trace.mixed_images, with a window (Options.WindowStart), those of the resources
// that count in it only (inWindow).
func (a *analysis) resources() {
	d := a.d
	envs := map[string]bool{}
	digests := map[string]map[string]bool{}
	windowed, mixed := a.inWindow(), digests
	if windowed != nil {
		mixed = map[string]map[string]bool{}
	}
	hasServer := map[string]bool{}
	for i := range d.Spans {
		if d.Spans[i].Kind == otlp.KindServer {
			hasServer[d.Resources[d.Spans[i].Res].Service] = true
		}
	}
	notString, defaults := map[string]bool{}, map[string]bool{}
	faas := map[string]bool{}
	type hostSvc struct{ host, svc string }
	inst := map[hostSvc]map[string]bool{}
	for i, r := range d.Resources {
		ri := d.InfoOf(int32(i))
		if ri.ServiceNameNotString {
			notString[r.Pod] = true
		}
		if strings.HasPrefix(r.Service, "unknown_service") {
			defaults[r.Service] = true
		}
		for _, e := range ri.Environments {
			envs[e] = true
		}
		if digests[r.Service] == nil {
			digests[r.Service] = map[string]bool{}
		}
		if mixed[r.Service] == nil {
			mixed[r.Service] = map[string]bool{}
		}
		for _, ref := range ri.RepoDigests {
			dg := cluster.ImageDigest(ref)
			if dg == "" {
				dg = ref
			}
			digests[r.Service][dg] = true
			if windowed == nil || windowed[i] {
				mixed[r.Service][dg] = true
			}
		}
		if ri.FaaS && hasServer[r.Service] {
			faas[r.Service] = true
		}
		if !ri.PlatformIdentity() && a.opt.PodIdentity != PodIdentityHost {
			for _, h := range ri.Hosts {
				k := hostSvc{h, r.Service}
				if inst[k] == nil {
					inst[k] = map[string]bool{}
				}
				for _, id := range ri.InstanceIDs {
					inst[k][id] = true
				}
			}
		}
	}
	for _, p := range sortedKeys(notString) {
		a.add(contract.NewFinding(contract.TraceServiceNameNotString).WithSubject(p))
	}
	for _, s := range sortedKeys(defaults) {
		a.add(contract.NewFinding(contract.TraceServiceNameDefault).WithSubject(s))
	}
	if len(envs) > 1 {
		a.add(contract.NewFinding(contract.TraceMixedEnvironments).WithNumber("environments", float64(len(envs))).
			WithExamples(sortedKeys(envs)...))
	}
	for _, s := range sortedKeys(hasServer) {
		switch n := len(mixed[s]); {
		case len(digests[s]) == 0:
			a.add(contract.NewFinding(contract.TraceImageDigestMissing).WithSubject(s))
		case n > 1:
			a.add(contract.NewFinding(contract.TraceMixedImages).WithSubject(s).WithNumber("digests", float64(n)).
				WithExamples(sortedKeys(mixed[s])...))
		}
	}
	for _, s := range sortedKeys(faas) {
		a.add(contract.NewFinding(contract.TraceFaaSService).WithSubject(s))
	}
	perSvc := map[string][2]int{} // hosts with several instance ids, the most ids on one
	for k, ids := range inst {
		if len(ids) > 1 && hasServer[k.svc] {
			v := perSvc[k.svc]
			v[0]++
			v[1] = max(v[1], len(ids))
			perSvc[k.svc] = v
		}
	}
	for _, s := range sortedKeys(perSvc) {
		a.add(contract.NewFinding(contract.TraceInstancePerProcess).WithSubject(s).WithNumber("hosts", float64(perSvc[s][0])).
			WithNumber("instances_on_one_host", float64(perSvc[s][1])))
	}
}

// inWindow marks, by index, the resources that count in the window of Options; nil without a window. A resource counts
// when it holds a span of a trace whose entry starts in [WindowStart, WindowEnd), which the cut keeps whole (Cut,
// traceEntries), or a span that overlaps the window (it starts before the window ends and ends at or after it starts),
// or no span at all (the cut keeps a resource with no span, and nothing places it in time). The check of the window so
// counts every digest that the check of its cut will, and more: a span that straddles the window's start counts
// although the cut drops its trace when that trace entered before the window; the check errs toward refusing. Spans
// the reader skips are not seen (trace.spans_skipped). A resource is a service, pod and image name as the reader
// merges them (otlp.Resource), so two digests reported on one such resource both count whatever the window: a rollout
// gives its new pods new names.
func (a *analysis) inWindow() []bool {
	if a.opt.WindowStart.IsZero() && a.opt.WindowEnd.IsZero() {
		return nil
	}
	ws, we, _ := WindowNanos(a.opt.WindowStart, a.opt.WindowEnd) // Run refuses any other window
	entry := traceEntries(a.d)
	in := make([]bool, len(a.d.Resources))
	held := make([]bool, len(a.d.Resources))
	for i := range a.d.Spans {
		s := &a.d.Spans[i]
		held[s.Res] = true
		e, known := entry[s.Trace]
		if !known {
			e = s.Start
		}
		if e >= ws && e < we || s.Start < we && s.End >= ws {
			in[s.Res] = true
		}
	}
	for i := range in {
		in[i] = in[i] || !held[i]
	}
	return in
}

// podsByHost keys the pods of resources with no platform identity by their host (--pod-identity host).
func (a *analysis) podsByHost() {
	for i := range a.d.Resources {
		ri := a.d.InfoOf(int32(i))
		if !ri.PlatformIdentity() && len(ri.Hosts) > 0 {
			a.d.Resources[i].Pod = ri.Hosts[0]
		}
	}
}

// duplicates: the same trace and span id twice. Identical copies (in every field the reader keeps) are dropped, the
// first kept, the order of the others unchanged; copies that differ refuse the capture.
func (a *analysis) duplicates() {
	sp := a.d.Spans
	idx := make([]int32, len(sp))
	for i := range idx {
		idx[i] = int32(i)
	}
	slices.SortFunc(idx, func(x, y int32) int {
		p, q := &sp[x], &sp[y]
		if c := cmpTrace(p.Trace, q.Trace); c != 0 {
			return c
		}
		if p.ID != q.ID {
			if p.ID < q.ID {
				return -1
			}
			return 1
		}
		return int(x - y)
	})
	drop := map[int32]bool{}
	var copies, collisions int64
	var collEx []string
	dropped := map[otlp.SpanKey]int{}
	for i := 1; i < len(idx); i++ {
		first, cur := &sp[idx[i-1]], &sp[idx[i]]
		if first.Trace != cur.Trace || first.ID != cur.ID {
			continue
		}
		// walk back to the first copy of this key
		j := i - 1
		for j > 0 && sp[idx[j-1]].Trace == cur.Trace && sp[idx[j-1]].ID == cur.ID {
			j--
		}
		if *cur == sp[idx[j]] {
			drop[idx[i]] = true
			copies++
			dropped[otlp.SpanKey{Trace: cur.Trace, ID: cur.ID}]++
		} else {
			collisions++
			if len(collEx) < contract.MaxExamples {
				collEx = append(collEx, otlp.TraceIDHex(cur.Trace))
			}
		}
	}
	if collisions > 0 {
		a.add(contract.NewFinding(contract.TraceSpanIDCollision).WithNumber("spans", float64(collisions)).WithExamples(collEx...))
	}
	if copies > 0 {
		out := make([]otlp.Span, 0, len(sp)-len(drop))
		for i := range sp {
			if !drop[int32(i)] {
				out = append(out, sp[i])
			}
		}
		a.d.Spans = out
		a.d.DroppedDuplicates = dropped
		var ex []string
		for k := range dropped {
			ex = append(ex, otlp.TraceIDHex(k.Trace))
		}
		sort.Strings(ex)
		a.add(contract.NewFinding(contract.TraceDuplicateSpansDropped).WithNumber("spans", float64(copies)).WithExamples(ex...))
	}
}

func cmpTrace(a, b [2]uint64) int {
	switch {
	case a[0] != b[0]:
		if a[0] < b[0] {
			return -1
		}
		return 1
	case a[1] != b[1]:
		if a[1] < b[1] {
			return -1
		}
		return 1
	}
	return 0
}

// ForEachTrace calls f with the spans of each trace (indexes into d.Spans), each trace's spans by start time, ties in
// the order read. It does not reorder d.Spans.
func ForEachTrace(d *otlp.Data, f func(g []int32)) {
	sp := d.Spans
	idx := make([]int32, len(sp))
	for i := range idx {
		idx[i] = int32(i)
	}
	slices.SortFunc(idx, func(x, y int32) int {
		p, q := &sp[x], &sp[y]
		if c := cmpTrace(p.Trace, q.Trace); c != 0 {
			return c
		}
		if p.Start != q.Start {
			if p.Start < q.Start {
				return -1
			}
			return 1
		}
		return int(x - y)
	})
	for lo := 0; lo < len(idx); {
		hi := lo + 1
		for hi < len(idx) && sp[idx[hi]].Trace == sp[idx[lo]].Trace {
			hi++
		}
		f(idx[lo:hi])
		lo = hi
	}
}

// caller finds the span a SERVER or CLIENT span k was sent from: its nearest SERVER, CLIENT or CONSUMER ancestor,
// walking through INTERNAL, PRODUCER and unspecified spans (as the inference does). ok is false when the span has no
// such ancestor in the capture.
func caller(sp []otlp.Span, pos map[uint64]int32, k int32) (int32, bool) {
	p, ok := pos[sp[k].Parent]
	if sp[k].Parent == 0 {
		return 0, false
	}
	for hops := 0; ok; hops++ {
		if kd := sp[p].Kind; kd == otlp.KindServer || kd == otlp.KindClient || kd == otlp.KindConsumer {
			return p, true
		}
		if sp[p].Parent == 0 || hops > len(pos) {
			return 0, false // a root, or a cycle of parents
		}
		p, ok = pos[sp[p].Parent]
	}
	return 0, false
}

// traces is the one pass over the capture, trace by trace.
func (a *analysis) traces() {
	d := a.d
	sp := d.Spans
	modelled := map[string]bool{} // services that emit SERVER spans: the others (a load generator) are outside callers
	for i := range sp {
		if sp[i].Kind == otlp.KindServer {
			modelled[d.Resources[sp[i].Res].Service] = true
		}
	}
	podOf := func(k int32) podKey { r := d.Resources[sp[k].Res]; return podKey{r.Service, r.Pod} }
	pos := map[uint64]int32{}
	matched := map[int32]bool{}
	wrapper := map[int32]bool{}
	ForEachTrace(d, func(g []int32) {
		a.traceN++
		clear(pos)
		clear(matched)
		clear(wrapper)
		for _, k := range g {
			if _, seen := pos[sp[k].ID]; !seen {
				pos[sp[k].ID] = k
			}
		}
		for _, k := range g {
			s := &sp[k]
			ext := d.ExtOf(s)
			pk := podOf(k)
			ps := a.podSpan[pk]
			ps[0]++
			if s.Start%1_000_000 == 0 {
				ps[1]++
			}
			a.podSpan[pk] = ps
			switch s.Kind {
			case otlp.KindServer:
				a.server(k, s, ext, pk, pos, matched, modelled)
			case otlp.KindClient:
				st := a.stat(pk.svc)
				st.client++
				if ext.Resends > 0 {
					st.resends++
				}
				// a CLIENT span inside another of its own service (a gRPC client's span around the HTTP client's)
				// is one call: the outer one is a wrapper, not a call that lost its callee
				if p, ok := caller(sp, pos, k); ok && sp[p].Kind == otlp.KindClient && d.Resources[sp[p].Res].Service == pk.svc {
					wrapper[p] = true
				}
			case otlp.KindConsumer:
				if ext.Links > 0 {
					a.stat(pk.svc).links++
				}
			}
		}
		for _, k := range g {
			if s := &sp[k]; s.Kind == otlp.KindClient && !matched[k] && !wrapper[k] {
				a.unmatch = append(a.unmatch, unmatched{caller: d.Resources[s.Res].Service, peer: s.Peer, addr: s.Addr,
					start: s.Start, end: s.End, trace: s.Trace})
			}
		}
	})
}

// server reads one SERVER span: its service's counts, whether it is an entry, and how its caller reached it. An entry
// is a SERVER span whose caller is not in the capture, is a message consumer, or is a CLIENT span of a service that
// emits no SERVER span (a load generator).
func (a *analysis) server(k int32, s *otlp.Span, ext otlp.SpanExt, pk podKey, pos map[uint64]int32, matched map[int32]bool,
	modelled map[string]bool) {
	d := a.d
	sp := d.Spans
	st := a.stat(pk.svc)
	st.server++
	if ext.Status == 2 {
		st.errors++
	}
	if ext.HTTPStatus == 429 || ext.HTTPStatus == 503 {
		st.shed++
	}
	if ext.MsgSent > 1 || ext.MsgRecv > 1 || ext.HTTPStatus == 101 {
		st.streams++
	}
	if pk.pod != "" {
		if a.podOf[pk.pod] == nil {
			a.podOf[pk.pod] = map[string]bool{}
		}
		a.podOf[pk.pod][pk.svc] = true
	}
	seen := a.podSrv[pk]
	if seen == nil {
		seen = &podSeen{first: s.Start, last: s.Start}
		a.podSrv[pk] = seen
	}
	seen.first, seen.last = min(seen.first, s.Start), max(seen.last, s.Start)
	seen.n++
	p, ok := caller(sp, pos, k)
	var from otlp.Resource
	if ok {
		from = d.Resources[sp[p].Res]
		if sp[p].Kind == otlp.KindClient {
			matched[p] = true
		}
	}
	if !ok || sp[p].Kind == otlp.KindConsumer || sp[p].Kind == otlp.KindClient && !modelled[from.Service] {
		a.entry(s, ext, pk, st)
		return
	}
	switch sp[p].Kind {
	case otlp.KindClient:
		e := a.edge(from.Service, pk.svc)
		e.withClient++
		c := &sp[p]
		if c.Addr >= 0 {
			if a.answers[c.Addr] == nil {
				a.answers[c.Addr] = map[string]int64{}
			}
			a.answers[c.Addr][pk.svc]++
		}
		// the callee's span inside its caller's CLIENT span, by the two pods' clocks
		pair := [2]podKey{{from.Service, from.Pod}, pk}
		ps := a.pairs[pair]
		if ps == nil {
			ps = &pairStat{}
			a.pairs[pair] = ps
		}
		ps.n++
		var off uint64
		if s.Start+skewSlackNs < c.Start {
			off = c.Start - s.Start
		}
		if s.End > c.End+skewSlackNs {
			off = max(off, s.End-c.End)
		}
		if off > 0 {
			ps.outside++
			ps.maxOffNs = max(ps.maxOffNs, off)
		}
	case otlp.KindServer:
		if from.Service != pk.svc {
			a.edge(from.Service, pk.svc).withoutClient++
		}
	}
}

// entry counts one entry SERVER span.
func (a *analysis) entry(s *otlp.Span, ext otlp.SpanExt, pk podKey, st *svcStat) {
	st.roots++
	st.entries++
	a.byEntry[pk.svc]++
	a.entries = append(a.entries, s.Start)
	a.ths[ext.TH]++
	a.roots[pk.svc] = append(a.roots[pk.svc], [2]uint64{s.Start, s.End})
	probe := ext.UserAgent != 0 || ext.ProbePath || a.declaredProbe(ext)
	if probe {
		st.probes++
		if len(st.probeEx) < contract.MaxExamples {
			st.probeEx = append(st.probeEx, probeExample(ext))
		}
	}
	key := periodicKey{pk, ext.Route + "\x00" + ext.RPCMethod}
	g := a.periodic[key]
	if g == nil {
		g = &periodicGroup{svc: pk.svc}
		a.periodic[key] = g
	}
	g.starts = append(g.starts, s.Start)
	g.durNs += s.End - s.Start
	if probe {
		g.probe++
	}
}

func (a *analysis) edge(caller, callee string) *edgeStat {
	k := edgeKey{caller, callee}
	e := a.edges[k]
	if e == nil {
		e = &edgeStat{}
		a.edges[k] = e
	}
	return e
}

// declaredProbe reports whether an entry's route is one declared to be a probe (Options.ExtraProbeRoutes): its
// http.route or rpc.method, with or without the method in front.
func (a *analysis) declaredProbe(ext otlp.SpanExt) bool {
	for _, r := range a.opt.ExtraProbeRoutes {
		if r == "" {
			continue
		}
		if ext.Route != "" && (r == ext.Route || strings.HasSuffix(r, " "+ext.Route)) || ext.RPCMethod != "" && r == ext.RPCMethod {
			return true
		}
	}
	return false
}

func probeExample(ext otlp.SpanExt) string {
	switch {
	case ext.Route != "":
		return ext.Route
	case ext.RPCMethod != "":
		return ext.RPCMethod
	case ext.UserAgent == otlp.UAKubeProbe:
		return "user-agent:kube-probe"
	case ext.UserAgent == otlp.UAELBHealthChecker:
		return "user-agent:ELB-HealthChecker"
	case ext.UserAgent == otlp.UAPrometheus:
		return "user-agent:Prometheus"
	}
	return "probe-path"
}

func (a *analysis) noServerSpans() bool {
	for _, st := range a.svc {
		if st.server > 0 {
			return false
		}
	}
	a.add(contract.NewFinding(contract.TraceNoServerSpans))
	return true
}

// ClusterInputs reads the cluster inputs of a capture (DATA.md 5): the cluster dump in the manifest's folder (its
// times from the manifest's cluster_dump record, checked against the manifest's window) and the ECS peer map. It
// returns them (nil when absent), the peer names they give together (address -> service; the dump's names win over
// the peer map's), and their own findings. Run uses it; a fit uses it for the same names (trace.peer_renamed) and for
// the environment record (Dump.Environment). The error is an I/O error.
func ClusterInputs(m *contract.Manifest) (*cluster.Dump, *cluster.PeerMap, map[string]string, []contract.Finding, error) {
	if m.Dir == "" {
		return nil, nil, nil, nil, nil
	}
	win := &cluster.Window{Start: m.Window.Start, End: m.Window.End}
	var taken cluster.TakenAt
	if cd := m.ClusterDump; cd != nil {
		if cd.Start != nil {
			taken.Start = cd.Start.TakenAt
		}
		if cd.End != nil {
			taken.End = cd.End.TakenAt
		}
	}
	dump, fs, err := cluster.Read(m.Dir, taken, win)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	pm, pfs, err := cluster.ReadPeerMap(m.Dir, win)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	fs = append(fs, pfs...)
	names := map[string]string{}
	if dump != nil {
		for k, v := range dump.PeerNames() {
			names[k] = v
		}
	}
	if pm != nil {
		for k, v := range pm.PeerNames() {
			if _, ok := names[k]; !ok {
				names[k] = v
			}
		}
	}
	return dump, pm, names, fs, nil
}

// clusterFindings adds the cluster inputs' findings and those that compare the dump with the spans (running image
// digests, pods that changed in the window), records the dump in the manifest, and returns the peer names.
func (a *analysis) clusterFindings(dump *cluster.Dump, names map[string]string, fs []contract.Finding) map[string]string {
	a.fs = append(a.fs, fs...)
	if dump == nil {
		return names
	}
	cd := &contract.ClusterDump{Layout: dump.Layout}
	if dump.Start != nil {
		cd.Start = &contract.Dump{Digest: dump.Start.Digest, TakenAt: dump.Start.TakenAt}
	}
	if dump.End != nil {
		cd.End = &contract.Dump{Digest: dump.End.Digest, TakenAt: dump.End.TakenAt}
	}
	a.clusterDump = cd
	span := map[string]map[string][]string{}
	for i, r := range a.d.Resources {
		ri := a.d.InfoOf(int32(i))
		if len(ri.RepoDigests) == 0 || ri.PodName == "" {
			continue
		}
		if span[r.Service] == nil {
			span[r.Service] = map[string][]string{}
		}
		span[r.Service][ri.PodName] = append(span[r.Service][ri.PodName], ri.RepoDigests...)
	}
	a.fs = append(a.fs, dump.CheckDigests(span)...)
	for _, p := range dump.Churn(cluster.Window{Start: a.m.Window.Start, End: a.m.Window.End}) {
		a.add(contract.NewFinding(contract.TracePodChurn).WithSubject(p).WithExamples("cluster-dump"))
	}
	return names
}

// renamed returns the service a CLIENT span's peer or address names through the cluster dump or the peer map.
func (a *analysis) renamed(names map[string]string, peer, addr int32) string {
	if len(names) == 0 {
		return ""
	}
	if addr >= 0 {
		if s, ok := names[a.d.Addrs[addr]]; ok {
			return s
		}
	}
	if peer >= 0 {
		if s, ok := names[a.d.Peers[peer]]; ok {
			return s
		}
	}
	return ""
}

// edgeFindings: CLIENT spans missing on an edge, calls lost on an edge, and peers renamed by the cluster inputs.
func (a *analysis) edgeFindings(names map[string]string) {
	// an address answered by a service: calls to it without a SERVER span under them are lost calls of that edge
	answeredBy := map[int32]string{}
	for addr, m := range a.answers {
		answeredBy[addr] = majority(m)
	}
	for _, u := range a.unmatch {
		if u.addr < 0 {
			continue
		}
		if callee := answeredBy[u.addr]; callee != "" && callee != u.caller {
			a.edge(u.caller, callee).lost++
		}
	}
	keys := make([]edgeKey, 0, len(a.edges))
	for k := range a.edges {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].caller != keys[j].caller {
			return keys[i].caller < keys[j].caller
		}
		return keys[i].callee < keys[j].callee
	})
	for _, k := range keys {
		e := a.edges[k]
		subject := k.caller + "/" + k.callee
		if calls := e.withClient + e.withoutClient; calls > 0 && float64(e.withoutClient) > clientMissingShare*float64(calls) {
			a.add(contract.NewFinding(contract.TraceClientSpansMissing).WithSubject(subject).WithNumber("calls", float64(calls)).
				WithNumber("without_client_span", float64(e.withoutClient)))
		}
		if calls := e.withClient + e.lost; e.lost > 0 && float64(e.lost) > edgeLossShare*float64(calls) {
			a.add(contract.NewFinding(contract.TraceEdgeLoss).WithSubject(subject).WithNumber("calls", float64(calls)).
				WithNumber("lost", float64(e.lost)).WithNumber("share", round4(float64(e.lost)/float64(calls))))
		}
	}
	// peers renamed through the cluster dump or the peer map
	if len(names) > 0 {
		calls := map[int32]int64{}
		for i := range a.d.Spans {
			if s := &a.d.Spans[i]; s.Kind == otlp.KindClient && s.Addr >= 0 {
				calls[s.Addr]++
			}
		}
		type ren struct {
			addr string
			n    int64
		}
		byService := map[string][]ren{}
		for ai, addr := range a.d.Addrs {
			if calls[int32(ai)] == 0 {
				continue
			}
			s, ok := names[addr]
			if !ok {
				host := addr
				if i := strings.LastIndexByte(addr, ':'); i > 0 {
					host = addr[:i]
				}
				s, ok = names[host]
			}
			if ok && s != addr {
				byService[s] = append(byService[s], ren{addr, calls[int32(ai)]})
			}
		}
		for _, s := range sortedKeys(byService) {
			var n int64
			var ex []string
			for _, r := range byService[s] {
				n += r.n
				ex = append(ex, r.addr)
			}
			sort.Strings(ex)
			a.add(contract.NewFinding(contract.TracePeerRenamed).WithSubject(s).WithNumber("calls", float64(n)).WithExamples(ex...))
		}
	}
}

func majority(m map[string]int64) string {
	best, bn := "", int64(0)
	for s, n := range m {
		if n > bn || n == bn && s < best {
			best, bn = s, n
		}
	}
	return best
}

// contextNotPropagated: a service whose SERVER spans are almost all roots while calls address it, by name or by the
// cluster inputs, or while calls without a SERVER span under them contain its root spans far more often than chance.
func (a *analysis) contextNotPropagated(names map[string]string) {
	lo, hi := uint64(math.MaxUint64), uint64(0)
	for i := range a.d.Spans {
		lo, hi = min(lo, a.d.Spans[i].Start), max(hi, a.d.Spans[i].End)
	}
	for _, s := range sortedKeys(a.svc) {
		st := a.svc[s]
		if st.server == 0 || float64(st.roots) <= rootShare*float64(st.server) {
			continue
		}
		var named int64
		var ex []string
		var outside []unmatched // calls to addresses no service answers
		for _, u := range a.unmatch {
			if u.caller == s {
				continue
			}
			if a.names(u, s, names) {
				named++
				if len(ex) < contract.MaxExamples {
					ex = append(ex, otlp.TraceIDHex(u.trace))
				}
			} else if u.addr < 0 || len(a.answers[u.addr]) == 0 {
				outside = append(outside, u)
			}
		}
		share := round4(float64(st.roots) / float64(st.server))
		if named >= addressedCalls {
			a.add(contract.NewFinding(contract.TraceContextNotPropagated).WithSubject(s).WithNumber("root_share", share).
				WithNumber("calls_addressing_it", float64(named)).WithExamples(ex...))
			continue
		}
		roots := append([][2]uint64(nil), a.roots[s]...)
		sort.Slice(roots, func(i, j int) bool { return roots[i][0] < roots[j][0] })
		obs := contained(outside, roots, 0, lo, hi)
		if obs < addressedCalls {
			continue
		}
		rng := splitmix(uint64(len(outside))*7919 + uint64(len(roots)))
		var base float64
		const shuffles = 3
		for i := 0; i < shuffles; i++ {
			base += float64(contained(outside, roots, rng.next(), lo, hi))
		}
		base /= shuffles
		if float64(obs) > containFactor*(base+1) {
			a.add(contract.NewFinding(contract.TraceContextNotPropagated).WithSubject(s).WithNumber("root_share", share).
				WithNumber("calls_containing_it", float64(obs)).WithNumber("shuffled_baseline", math.Round(base*100)/100))
		}
	}
}

// names reports whether an unmatched call addresses service s: its peer or address is s's name, or names s through
// the cluster inputs.
func (a *analysis) names(u unmatched, s string, names map[string]string) bool {
	if u.peer >= 0 && a.d.Peers[u.peer] == s {
		return true
	}
	if u.addr >= 0 {
		addr := a.d.Addrs[u.addr]
		if host, _, ok := strings.Cut(addr, ":"); ok && host == s {
			return true
		}
	}
	return a.renamed(names, u.peer, u.addr) == s
}

// contained counts the calls whose span contains at least one of the root spans (sorted by start). A seed other than
// 0 shifts every call to a random place in [lo, hi): the shuffled baseline.
func contained(calls []unmatched, roots [][2]uint64, seed uint64, lo, hi uint64) int {
	var rng *splitmixState
	if seed != 0 {
		rng = splitmix(seed)
	}
	n := 0
	for _, c := range calls {
		st, en := c.start, c.end
		if rng != nil && hi > lo {
			dur := en - st
			st = lo + rng.next()%(hi-lo)
			en = st + dur
		}
		i := sort.Search(len(roots), func(i int) bool { return roots[i][0] >= st })
		for ; i < len(roots) && roots[i][0] <= en; i++ {
			if roots[i][1] <= en {
				n++
				break
			}
		}
	}
	return n
}

// splitmixState is a small deterministic generator (splitmix64): the shuffled baseline is the same on every run.
type splitmixState struct{ s uint64 }

func splitmix(seed uint64) *splitmixState { return &splitmixState{seed} }

func (r *splitmixState) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	return (z ^ z>>31) | 1
}

// serviceFindings: errors, shedding, retries, streams, links and probe-like traffic, by service.
func (a *analysis) serviceFindings() {
	// periodic tiny entries of one route on one pod are probe-like too
	for _, k := range sortedPeriodic(a.periodic) {
		g := a.periodic[k]
		n := int64(len(g.starts))
		if n < periodicMin || float64(g.durNs)/float64(n)/1e6 >= periodicMaxMs {
			continue
		}
		s := append([]uint64(nil), g.starts...)
		slices.Sort(s)
		var sum, sq float64
		for i := 1; i < len(s); i++ {
			gap := float64(s[i] - s[i-1])
			sum += gap
			sq += gap * gap
		}
		m := sum / float64(len(s)-1)
		cv := math.Sqrt(math.Max(0, sq/float64(len(s)-1)-m*m)) / m
		if m > 0 && cv < periodicCV {
			st := a.stat(g.svc)
			st.probes += n - g.probe
			if len(st.probeEx) < contract.MaxExamples {
				st.probeEx = append(st.probeEx, "periodic:"+k.pod.pod)
			}
		}
	}
	for _, s := range sortedKeys(a.svc) {
		st := a.svc[s]
		share := func(n, of int64) float64 { return round4(float64(n) / float64(of)) }
		if st.server > 0 && float64(st.errors) > errorShare*float64(st.server) {
			a.add(contract.NewFinding(contract.TraceErrorsPresent).WithSubject(s).WithNumber("share", share(st.errors, st.server)))
		}
		if st.server > 0 && float64(st.shed) > errorShare*float64(st.server) {
			a.add(contract.NewFinding(contract.TraceShedding).WithSubject(s).WithNumber("share", share(st.shed, st.server)))
		}
		if st.client > 0 && float64(st.resends) > errorShare*float64(st.client) {
			a.add(contract.NewFinding(contract.TraceRetries).WithSubject(s).WithNumber("share", share(st.resends, st.client)))
		}
		if st.streams > 0 {
			a.add(contract.NewFinding(contract.TraceStreamingRPC).WithSubject(s).WithNumber("spans", float64(st.streams)))
		}
		if st.links > 0 {
			a.add(contract.NewFinding(contract.TraceLinksPresent).WithSubject(s).WithNumber("spans", float64(st.links)))
		}
		if st.entries > 0 && float64(st.probes) > errorShare*float64(st.entries) {
			a.add(contract.NewFinding(contract.TraceProbeLikeTraffic).WithSubject(s).WithNumber("share", share(st.probes, st.entries)).
				WithExamples(st.probeEx...))
		}
	}
}

func sortedPeriodic(m map[periodicKey]*periodicGroup) []periodicKey {
	out := make([]periodicKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.pod != b.pod {
			if a.pod.svc != b.pod.svc {
				return a.pod.svc < b.pod.svc
			}
			return a.pod.pod < b.pod.pod
		}
		return a.route < b.route
	})
	return out
}

// podFindings: shared and collapsed pod identities, clock skew between pod pairs, whole-millisecond clocks, and pods
// present for part of the window.
func (a *analysis) podFindings() {
	for _, pod := range sortedKeys(a.podOf) {
		svcs := sortedKeys(a.podOf[pod])
		switch n := len(svcs); {
		case n >= collapsedServices:
			a.add(contract.NewFinding(contract.TracePodIdentityCollapsed).WithSubject(pod).WithNumber("services", float64(n)).
				WithExamples(svcs...))
		case n == 2:
			a.add(contract.NewFinding(contract.TracePodShared).WithSubject(pod).WithNumber("services", 2).WithExamples(svcs...))
		}
	}
	pairs := make([][2]podKey, 0, len(a.pairs))
	for k := range a.pairs {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool { return fmt.Sprint(pairs[i]) < fmt.Sprint(pairs[j]) })
	for _, k := range pairs {
		ps := a.pairs[k]
		if ps.n >= skewMinPairs && float64(ps.outside) > skewShare*float64(ps.n) {
			a.add(contract.NewFinding(contract.TraceClockSkew).WithSubject(k[0].pod+"/"+k[1].pod).WithNumber("calls", float64(ps.n)).
				WithNumber("outside_share", round4(float64(ps.outside)/float64(ps.n))).
				WithNumber("max_offset_ms", math.Round(float64(ps.maxOffNs)/1e3)/1e3))
		}
	}
	pods := make([]podKey, 0, len(a.podSpan))
	for k := range a.podSpan {
		pods = append(pods, k)
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].svc != pods[j].svc {
			return pods[i].svc < pods[j].svc
		}
		return pods[i].pod < pods[j].pod
	})
	for _, k := range pods {
		c := a.podSpan[k]
		if c[0] >= wholeMsMin && float64(c[1]) >= wholeMsShare*float64(c[0]) {
			a.add(contract.NewFinding(contract.TraceWholeMsStarts).WithSubject(k.pod).WithNumber("spans", float64(c[0])))
		}
	}
	// pods present for part of the window, by their SERVER spans
	if len(a.entries) < 2 {
		return
	}
	var w0, w1 uint64 = math.MaxUint64, 0
	for _, s := range a.podSrv {
		w0, w1 = min(w0, s.first), max(w1, s.last)
	}
	win := float64(w1 - w0)
	for _, k := range pods {
		s := a.podSrv[k]
		if s == nil || s.n < churnMinVisits || win <= 0 {
			continue
		}
		gap := float64(s.last-s.first) / float64(s.n-1)
		absent := math.Max(float64(s.first-w0), float64(w1-s.last))
		if absent > churnShare*win && absent > churnGaps*gap {
			a.add(contract.NewFinding(contract.TracePodChurn).WithSubject(k.pod).WithNumber("present_share",
				round4(float64(s.last-s.first)/win)))
		}
	}
}

// arrivals: is the entry load steady and roughly Poisson? Sub-windows of the entries' span, about arrivalsPerBin
// arrivals each, at least arrivalBins of them.
func (a *analysis) arrivals() {
	n := len(a.entries)
	k := n / arrivalsPerBin
	if k < arrivalBins {
		a.add(contract.NewFinding(contract.TraceArrivalsUnjudged).WithNumber("entries", float64(n)).
			WithNumber("needed", arrivalsPerBin*arrivalBins))
		return
	}
	k = min(k, maxArrivalBins)
	e := append([]uint64(nil), a.entries...)
	slices.Sort(e)
	lo, span := e[0], float64(e[n-1]-e[0])+1
	counts := make([]float64, k)
	for _, t := range e {
		b := int(float64(t-lo) / span * float64(k))
		counts[min(b, k-1)]++
	}
	var mean, v float64
	for _, c := range counts {
		mean += c
	}
	mean /= float64(k)
	for _, c := range counts {
		v += (c - mean) * (c - mean)
	}
	v /= float64(k - 1)
	disp := v / mean
	if disp > math.Max(dispersionMin, 1+dispersionZ*math.Sqrt(2/float64(k-1))) {
		a.add(contract.NewFinding(contract.TraceArrivalsOverdispersed).WithNumber("dispersion_index", round4(disp)).
			WithNumber("sub_windows", float64(k)))
	}
	var n1, n2 float64
	for i, c := range counts {
		if i < k/2 {
			n1 += c
		} else if i >= k-k/2 {
			n2 += c
		}
	}
	if z := (n2 - n1) / math.Sqrt(n1+n2); math.Abs(z) > trendZ && math.Abs(n2-n1)/((n1+n2)/2) > trendMin {
		a.add(contract.NewFinding(contract.TraceLoadTrend).WithNumber("second_half_over_first", round4(n2/n1)))
	}
}

// asyncTail: for a cut capture with margins, the share of the window's traces whose spans reach the last part of the
// margin after the window: those may still have been open when the margin ended.
func (a *analysis) asyncTail() {
	m := a.m
	if m.Window.Cut != contract.CutEntrySpanStart || m.Margins == nil || m.Margins.After <= 0 {
		return
	}
	end := m.Window.End.Add(time.Duration(m.Margins.After * float64(time.Second)))
	zone := time.Duration(math.Max(1, tailZone*m.Margins.After) * float64(time.Second))
	from := uint64(end.Add(-zone).UnixNano())
	ws, we := uint64(m.Window.Start.UnixNano()), uint64(m.Window.End.UnixNano())
	entry := traceEntries(a.d)
	last := map[[2]uint64]uint64{}
	for i := range a.d.Spans {
		s := &a.d.Spans[i]
		last[s.Trace] = max(last[s.Trace], s.End)
	}
	var in, open int64
	for t, e := range entry {
		if e < ws || e >= we {
			continue
		}
		in++
		if last[t] >= from {
			open++
		}
	}
	if in > 0 && float64(open) > tailShare*float64(in) {
		a.add(contract.NewFinding(contract.TraceAsyncTailTruncated).WithNumber("share", round4(float64(open)/float64(in))))
	}
}

func (a *analysis) samplingFindings() {
	in := samplingInput{entries: a.byEntry, thEntries: a.ths, window: [2]time.Time{a.m.Window.Start, a.m.Window.End}}
	kept, fs := sampling(in, a.opt)
	a.kept = kept
	a.fs = append(a.fs, fs...)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
