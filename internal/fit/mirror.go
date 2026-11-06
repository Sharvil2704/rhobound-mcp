// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// The mirror of an inference: every field of infer.Inferred, its topology and its statistics, written so that reading
// it back gives the same values bit for bit. Every float is a Float (a non-finite one is written as a string), no key
// is omitted, and a nil list is written as null and an empty one as [], so that reading keeps the difference (the text
// report keys on a nil list in places). TestMirrorCoversEveryField fails when infer or model gains a field this file
// does not carry.

type wireInferred struct {
	Topology         *wireTopology       `json:"topology"`
	Stats            []wireStats         `json:"stats"`
	LamExtPerMs      Float               `json:"lam_ext_per_ms"`
	WindowS          Float               `json:"window_s"`
	Requests         int64               `json:"requests"`
	ConsumerRequests int64               `json:"consumer_requests"`
	Spans            int                 `json:"spans"`
	Traces           int64               `json:"traces"`
	BrokenTraces     int64               `json:"broken_traces"`
	ProbeSpans       int                 `json:"probe_spans"`
	NestedSpans      []infer.NestedMerge `json:"nested_spans"`
	Warnings         []string            `json:"warnings"`
	WarningCodes     []string            `json:"warning_codes"`
	WarningSubjects  []string            `json:"warning_subjects"`
}

type wireTopology struct {
	Name     string        `json:"name"`
	Services []wireService `json:"services"`
	// Entry is the topology's entry weights by node index, in index order.
	Entry []wireEntry `json:"entry"`
}

type wireEntry struct {
	Node   int   `json:"node"`
	Weight Float `json:"weight"`
}

type wireService struct {
	Name       string     `json:"name"`
	S          Float      `json:"s"`
	Calls      []wireCall `json:"calls"`
	Parallel   bool       `json:"parallel"`
	Servers    int        `json:"servers"`
	Workers    int        `json:"workers"`
	CS2        Float      `json:"cs2"`
	Routing    string     `json:"routing"`
	Delay      bool       `json:"delay"`
	Hop        Float      `json:"hop"`
	BranchCorr Float      `json:"branch_corr"`
	Glue       Float      `json:"glue"`
	GlueCS2    Float      `json:"glue_cs2"`
	Shares     []Float    `json:"shares"`
	// Routes and RouteQ are omitted when empty, as wireStats.Routes: a topology without routed services keeps its
	// bytes.
	Routes []wireTopoRoute `json:"routes,omitempty"`
}

type wireTopoRoute struct {
	Name  string  `json:"name"`
	Share Float   `json:"share"`
	S     Float   `json:"s"`
	CS2   Float   `json:"cs2"`
	P     []Float `json:"p"`
}

type wireCall struct {
	Child  int     `json:"child"`
	P      Float   `json:"p"`
	Async  bool    `json:"async"`
	Hop    Float   `json:"hop"`
	HasHop bool    `json:"has_hop"`
	RouteQ []Float `json:"route_q,omitempty"`
}

type wireStats struct {
	Name                string             `json:"name"`
	External            bool               `json:"external"`
	Entry               bool               `json:"entry"`
	Entries             int64              `json:"entries"`
	ConsumerEntries     int64              `json:"consumer_entries"`
	Visits              int64              `json:"visits"`
	RateRPS             Float              `json:"rate_rps"`
	Pods                int                `json:"pods"`
	WorkersPerPod       int                `json:"workers_per_pod"`
	WorkersInferred     bool               `json:"workers_inferred"`
	OwnMeanMs           Float              `json:"own_mean_ms"`
	OwnCS2              Float              `json:"own_cs2"`
	OwnCS2Measured      Float              `json:"own_cs2_measured"`
	CS2Snapped          bool               `json:"own_cs2_snapped"`
	GlueMs              Float              `json:"glue_ms"`
	GlueCS2             Float              `json:"glue_cs2"`
	HopMs               Float              `json:"hop_ms"`
	ObservedWaitMs      Float              `json:"observed_wait_ms"`
	InSpanWaitMs        Float              `json:"in_span_wait_ms"`
	WorkersFromQueueing bool               `json:"workers_from_queueing"`
	ObservedServerMs    Float              `json:"observed_server_ms"`
	ObservedClientMs    Float              `json:"observed_client_ms"`
	ParallelShare       Float              `json:"parallel_share"`
	Stages              int                `json:"stages"`
	Stage               bool               `json:"stage"`
	BranchCorr          Float              `json:"branch_corr"`
	Images              []string           `json:"images"`
	HopFromMean         bool               `json:"hop_from_mean"`
	LostVisits          int64              `json:"lost_visits"`
	LostShare           Float              `json:"lost_share"`
	BrokenVisitShare    Float              `json:"broken_visit_share"`
	VariableCalls       []wireVariableCall `json:"variable_calls"`
	CallShapes          []wireCallShape    `json:"call_shapes"`
	PodShares           []wirePodShare     `json:"pod_shares"`
	SplitP              *Float             `json:"split_p"`
	UnevenSplit         bool               `json:"uneven_split"`
	// Routes is the one key the mirror omits when it is empty: a service with one route has none, and a record of a
	// capture whose services each have one route keeps the bytes (and the digest) it had before routes were read.
	Routes []wireRoute `json:"routes,omitempty"`
}

type wireRoute struct {
	Route   string            `json:"route"`
	Visits  int64             `json:"visits"`
	Share   Float             `json:"share"`
	OwnMs   Float             `json:"own_ms"`
	OwnCS2  Float             `json:"own_cs2"`
	Callers []wireRouteCaller `json:"callers"`
}

type wireRouteCaller struct {
	Caller   string `json:"caller"`
	PerVisit Float  `json:"per_visit"`
}

type wireVariableCall struct {
	Callee        string              `json:"callee"`
	MeanPerVisit  Float               `json:"mean_per_visit"`
	VisitsByCalls []infer.CallsVisits `json:"visits_by_calls"`
	ModelledAs    string              `json:"modelled_as"`
}

type wireCallShape struct {
	Shape string `json:"shape"`
	Share Float  `json:"share"`
}

type wirePodShare struct {
	Pod    string `json:"pod"`
	Visits int64  `json:"visits"`
	Share  Float  `json:"share"`
}

func floats(xs []float64) []Float {
	if xs == nil {
		return nil
	}
	out := make([]Float, len(xs))
	for i, x := range xs {
		out[i] = Float(x)
	}
	return out
}

func unfloats(xs []Float) []float64 {
	if xs == nil {
		return nil
	}
	out := make([]float64, len(xs))
	for i, x := range xs {
		out[i] = float64(x)
	}
	return out
}

// clone returns a copy of a list, nil for nil and empty for empty.
func clone[T any](xs []T) []T {
	if xs == nil {
		return nil
	}
	return append(make([]T, 0, len(xs)), xs...)
}

func cloneCounts(xs []infer.CallsVisits) []infer.CallsVisits { return clone(xs) }

// toWire copies an inference into its written form.
func toWire(inf *infer.Inferred) *wireInferred {
	t := inf.Topology
	wt := &wireTopology{Name: t.Name, Services: make([]wireService, len(t.Services)), Entry: []wireEntry{}}
	for i, s := range t.Services {
		ws := wireService{Name: s.Name, S: Float(s.S), Parallel: s.Parallel, Servers: s.Servers, Workers: s.Workers,
			CS2: Float(s.CS2), Routing: s.Routing, Delay: s.Delay, Hop: Float(s.Hop), BranchCorr: Float(s.BranchCorr),
			Glue: Float(s.Glue), GlueCS2: Float(s.GlueCS2), Shares: floats(s.Shares)}
		if s.Calls != nil {
			ws.Calls = make([]wireCall, len(s.Calls))
			for j, c := range s.Calls {
				ws.Calls[j] = wireCall{Child: c.Child, P: Float(c.P), Async: c.Async, Hop: Float(c.Hop), HasHop: c.HasHop,
					RouteQ: floats(c.RouteQ)}
			}
		}
		for _, r := range s.Routes {
			ws.Routes = append(ws.Routes, wireTopoRoute{Name: r.Name, Share: Float(r.Share), S: Float(r.S), CS2: Float(r.CS2),
				P: floats(r.P)})
		}
		wt.Services[i] = ws
	}
	for n, w := range t.Entry {
		wt.Entry = append(wt.Entry, wireEntry{Node: n, Weight: Float(w)})
	}
	sort.Slice(wt.Entry, func(i, j int) bool { return wt.Entry[i].Node < wt.Entry[j].Node })

	w := &wireInferred{Topology: wt, LamExtPerMs: Float(inf.LamExtPerMs), WindowS: Float(inf.WindowS),
		Requests: inf.Requests, ConsumerRequests: inf.ConsumerRequests, Spans: inf.Spans, Traces: inf.Traces,
		BrokenTraces: inf.BrokenTraces, ProbeSpans: inf.ProbeSpans, NestedSpans: clone(inf.NestedSpans),
		Warnings: clone(inf.Warnings), WarningCodes: clone(inf.WarningCodes), WarningSubjects: clone(inf.WarningSubjects)}
	if inf.Stats != nil {
		w.Stats = make([]wireStats, len(inf.Stats))
	}
	for i, st := range inf.Stats {
		ws := wireStats{Name: st.Name, External: st.External, Entry: st.Entry, Entries: st.Entries,
			ConsumerEntries: st.ConsumerEntries, Visits: st.Visits, RateRPS: Float(st.RateRPS), Pods: st.Pods,
			WorkersPerPod: st.WorkersPerPod, WorkersInferred: st.WorkersInferred, OwnMeanMs: Float(st.OwnMeanMs),
			OwnCS2: Float(st.OwnCS2), OwnCS2Measured: Float(st.OwnCS2Measured), CS2Snapped: st.CS2Snapped,
			GlueMs: Float(st.GlueMs), GlueCS2: Float(st.GlueCS2), HopMs: Float(st.HopMs),
			ObservedWaitMs: Float(st.ObservedWaitMs), InSpanWaitMs: Float(st.InSpanWaitMs),
			WorkersFromQueueing: st.WorkersFromQueueing, ObservedServerMs: Float(st.ObservedServerMs),
			ObservedClientMs: Float(st.ObservedClientMs), ParallelShare: Float(st.ParallelShare), Stages: st.Stages,
			Stage: st.Stage, BranchCorr: Float(st.BranchCorr), Images: clone(st.Images), HopFromMean: st.HopFromMean,
			LostVisits: st.LostVisits, LostShare: Float(st.LostShare), BrokenVisitShare: Float(st.BrokenVisitShare),
			UnevenSplit: st.UnevenSplit}
		if st.VariableCalls != nil {
			ws.VariableCalls = make([]wireVariableCall, len(st.VariableCalls))
			for j, v := range st.VariableCalls {
				ws.VariableCalls[j] = wireVariableCall{Callee: v.Callee, MeanPerVisit: Float(v.MeanPerVisit),
					VisitsByCalls: cloneCounts(v.VisitsByCalls), ModelledAs: v.ModelledAs}
			}
		}
		if st.CallShapes != nil {
			ws.CallShapes = make([]wireCallShape, len(st.CallShapes))
			for j, c := range st.CallShapes {
				ws.CallShapes[j] = wireCallShape{Shape: c.Shape, Share: Float(c.Share)}
			}
		}
		if st.PodShares != nil {
			ws.PodShares = make([]wirePodShare, len(st.PodShares))
			for j, p := range st.PodShares {
				ws.PodShares[j] = wirePodShare{Pod: p.Pod, Visits: p.Visits, Share: Float(p.Share)}
			}
		}
		if st.SplitP != nil {
			p := Float(*st.SplitP)
			ws.SplitP = &p
		}
		for _, r := range st.Routes {
			wr := wireRoute{Route: r.Route, Visits: r.Visits, Share: Float(r.Share), OwnMs: Float(r.OwnMs),
				OwnCS2: Float(r.OwnCS2), Callers: make([]wireRouteCaller, len(r.Callers))}
			for j, c := range r.Callers {
				wr.Callers[j] = wireRouteCaller{Caller: c.Caller, PerVisit: Float(c.PerVisit)}
			}
			ws.Routes = append(ws.Routes, wr)
		}
		w.Stats[i] = ws
	}
	return w
}

// checkNodes reports why the mirror names a node that is not one of its rows (the names of its statistics, which the
// report's services rows carry), or nil. Every node a record names is a row, as the inference writes them: node i of
// the topology is row i, whose name the report's busiest_service, async_calls and biased constructs carry; a varying
// call's callee is a row; a call shape is rows joined as describeShape joins them (" then " between stages, " + "
// between calls at once, " xN" after a callee called N times at once, "?" for a callee the inference could not name);
// and a stage node, a zero-time node the inference adds for the calls of a service, is named stage:S:j, stage:S:j.s or
// shape:S:j after S, a row that is not a stage node, which the engine reads back from the name of a row marked as a
// stage node only (a service the traces name so is a row of its own, named by its whole name). So whoever names a
// node as the views name a row (the MCP server's rhobound://prediction) names every node. nested_spans is not checked:
// it names the services of the capture whose nested spans were merged, among them one that emits no SERVER span and
// so has no row.
func (w *wireInferred) checkNodes() error {
	rows := make(map[string]bool, len(w.Stats))
	services := map[string]bool{}
	for _, st := range w.Stats {
		rows[st.Name] = true
		if !st.Stage {
			services[st.Name] = true
		}
	}
	for i, sv := range w.Topology.Services {
		if i >= len(w.Stats) || sv.Name != w.Stats[i].Name {
			return errors.New("a node of the model is named apart from its row")
		}
	}
	// the words of a call shape, the rows and "?", by their first byte
	words := map[byte][]string{'?': {"?"}}
	for n := range rows {
		if n != "" {
			words[n[0]] = append(words[n[0]], n)
		}
	}
	for _, l := range words {
		sort.Strings(l)
	}
	for _, st := range w.Stats {
		if st.Stage && !services[stageCaller(st.Name)] {
			return errors.New("a stage node of the model is not named after a service of it")
		}
		for _, v := range st.VariableCalls {
			if !rows[v.Callee] {
				return errors.New("a varying call's callee is not a node of the model")
			}
		}
		for _, c := range st.CallShapes {
			if !isShape(c.Shape, words) {
				return errors.New("a call shape names what is not a node of the model")
			}
		}
	}
	return nil
}

// stageCaller is the service a stage node's name holds, S of stage:S:j, stage:S:j.s or shape:S:j (j and s whole
// numbers written as the inference writes them), or "" for any other name.
func stageCaller(name string) string {
	rest, stage := strings.CutPrefix(name, "stage:")
	if !stage {
		var ok bool
		if rest, ok = strings.CutPrefix(name, "shape:"); !ok {
			return ""
		}
	}
	k := strings.LastIndexByte(rest, ':')
	if k < 0 {
		return ""
	}
	j, s, two := strings.Cut(rest[k+1:], ".")
	if !wholeNumber(j) || two && (!stage || !wholeNumber(s)) {
		return ""
	}
	return rest[:k]
}

// wholeNumber reports whether s is a positive whole number as %d writes one: digits, no leading zero, at most 9.
func wholeNumber(s string) bool {
	if s == "" || len(s) > 9 || s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isShape reports whether s is a call shape made of words, given by their first byte (describeShape): items joined by
// " then " or " + ", each a word, then " xN" (N calls at once, N at least 2) or nothing. A word may itself hold a
// joiner, so every way of reading s is tried, each position once.
func isShape(s string, words map[byte][]string) bool {
	failed := map[int]bool{} // positions at which no item starts a reading to the end
	var item func(i int) bool
	item = func(i int) bool {
		if failed[i] || i >= len(s) {
			return false
		}
		for _, w := range words[s[i]] {
			if !strings.HasPrefix(s[i:], w) {
				continue
			}
			ends := []int{i + len(w)}
			if n := countAt(s, i+len(w)); n > 0 {
				ends = append(ends, i+len(w)+n)
			}
			for _, e := range ends {
				if e == len(s) {
					return true
				}
				for _, joiner := range []string{" then ", " + "} {
					if strings.HasPrefix(s[e:], joiner) && item(e+len(joiner)) {
						return true
					}
				}
			}
		}
		failed[i] = true
		return false
	}
	return s != "" && item(0)
}

// countAt is the length of " xN" at i of s, N a whole number of at least 2 (wholeNumber), or 0.
func countAt(s string, i int) int {
	rest, ok := strings.CutPrefix(s[i:], " x")
	if !ok {
		return 0
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if !wholeNumber(rest[:n]) || rest[:n] == "1" {
		return 0
	}
	return 2 + n
}

// fromWire rebuilds a fresh inference from its written form, its topology finished.
func fromWire(w *wireInferred) (*infer.Inferred, error) {
	wt := w.Topology
	if wt == nil {
		return nil, errors.New("fit: the inference has no topology")
	}
	t := &model.Topology{Name: wt.Name, Services: make([]model.Service, len(wt.Services)), Entry: map[int]float64{}}
	for i, ws := range wt.Services {
		s := model.Service{Name: ws.Name, S: float64(ws.S), Parallel: ws.Parallel, Servers: ws.Servers, Workers: ws.Workers,
			CS2: float64(ws.CS2), Routing: ws.Routing, Delay: ws.Delay, Hop: float64(ws.Hop),
			BranchCorr: float64(ws.BranchCorr), Glue: float64(ws.Glue), GlueCS2: float64(ws.GlueCS2),
			Shares: unfloats(ws.Shares)}
		if ws.Calls != nil {
			s.Calls = make([]model.Call, len(ws.Calls))
			for j, c := range ws.Calls {
				s.Calls[j] = model.Call{Child: c.Child, P: float64(c.P), Async: c.Async, Hop: float64(c.Hop), HasHop: c.HasHop,
					RouteQ: unfloats(c.RouteQ)}
			}
		}
		for _, r := range ws.Routes {
			s.Routes = append(s.Routes, model.Route{Name: r.Name, Share: float64(r.Share), S: float64(r.S), CS2: float64(r.CS2),
				P: unfloats(r.P)})
		}
		t.Services[i] = s
	}
	for _, e := range wt.Entry {
		t.Entry[e.Node] = float64(e.Weight)
	}
	if err := t.Finish(); err != nil {
		return nil, fmt.Errorf("fit: the record's topology: %v", err)
	}
	if len(w.Stats) != len(t.Services) {
		return nil, fmt.Errorf("fit: %d statistics for %d nodes", len(w.Stats), len(t.Services))
	}
	inf := &infer.Inferred{Topology: t, LamExtPerMs: float64(w.LamExtPerMs), WindowS: float64(w.WindowS),
		Requests: w.Requests, ConsumerRequests: w.ConsumerRequests, Spans: w.Spans, Traces: w.Traces,
		BrokenTraces: w.BrokenTraces, ProbeSpans: w.ProbeSpans, NestedSpans: clone(w.NestedSpans),
		Warnings: clone(w.Warnings), WarningCodes: clone(w.WarningCodes), WarningSubjects: clone(w.WarningSubjects)}
	if w.Stats != nil {
		inf.Stats = make([]infer.ServiceStats, len(w.Stats))
	}
	for i, ws := range w.Stats {
		st := infer.ServiceStats{Name: ws.Name, External: ws.External, Entry: ws.Entry, Entries: ws.Entries,
			ConsumerEntries: ws.ConsumerEntries, Visits: ws.Visits, RateRPS: float64(ws.RateRPS), Pods: ws.Pods,
			WorkersPerPod: ws.WorkersPerPod, WorkersInferred: ws.WorkersInferred, OwnMeanMs: float64(ws.OwnMeanMs),
			OwnCS2: float64(ws.OwnCS2), OwnCS2Measured: float64(ws.OwnCS2Measured), CS2Snapped: ws.CS2Snapped,
			GlueMs: float64(ws.GlueMs), GlueCS2: float64(ws.GlueCS2), HopMs: float64(ws.HopMs),
			ObservedWaitMs: float64(ws.ObservedWaitMs), InSpanWaitMs: float64(ws.InSpanWaitMs),
			WorkersFromQueueing: ws.WorkersFromQueueing, ObservedServerMs: float64(ws.ObservedServerMs),
			ObservedClientMs: float64(ws.ObservedClientMs), ParallelShare: float64(ws.ParallelShare), Stages: ws.Stages,
			Stage: ws.Stage, BranchCorr: float64(ws.BranchCorr), Images: clone(ws.Images), HopFromMean: ws.HopFromMean,
			LostVisits: ws.LostVisits, LostShare: float64(ws.LostShare), BrokenVisitShare: float64(ws.BrokenVisitShare),
			UnevenSplit: ws.UnevenSplit}
		if ws.VariableCalls != nil {
			st.VariableCalls = make([]infer.VariableCall, len(ws.VariableCalls))
			for j, v := range ws.VariableCalls {
				st.VariableCalls[j] = infer.VariableCall{Callee: v.Callee, MeanPerVisit: float64(v.MeanPerVisit),
					VisitsByCalls: cloneCounts(v.VisitsByCalls), ModelledAs: v.ModelledAs}
			}
		}
		if ws.CallShapes != nil {
			st.CallShapes = make([]infer.CallShape, len(ws.CallShapes))
			for j, c := range ws.CallShapes {
				st.CallShapes[j] = infer.CallShape{Shape: c.Shape, Share: float64(c.Share)}
			}
		}
		if ws.PodShares != nil {
			st.PodShares = make([]infer.PodShare, len(ws.PodShares))
			for j, p := range ws.PodShares {
				st.PodShares[j] = infer.PodShare{Pod: p.Pod, Visits: p.Visits, Share: float64(p.Share)}
			}
		}
		if ws.SplitP != nil {
			p := float64(*ws.SplitP)
			st.SplitP = &p
		}
		for _, wr := range ws.Routes {
			r := infer.RouteStats{Route: wr.Route, Visits: wr.Visits, Share: float64(wr.Share), OwnMs: float64(wr.OwnMs),
				OwnCS2: float64(wr.OwnCS2), Callers: make([]infer.RouteCaller, len(wr.Callers))}
			for j, c := range wr.Callers {
				r.Callers[j] = infer.RouteCaller{Caller: c.Caller, PerVisit: float64(c.PerVisit)}
			}
			st.Routes = append(st.Routes, r)
		}
		inf.Stats[i] = st
	}
	return inf, nil
}
