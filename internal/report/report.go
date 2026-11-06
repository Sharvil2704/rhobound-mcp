// SPDX-License-Identifier: Apache-2.0

// Package report builds the prediction report and renders it as text or JSON.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// Version of the report format.
const Version = "rhobound-cli.report/v1"

// Row is one service of the report.
type Row struct {
	Name          string   `json:"name"`
	External      bool     `json:"external,omitempty"`
	Entry         bool     `json:"entry,omitempty"`
	ConsumerEntry bool     `json:"entry_from_consumer,omitempty"` // some of its entries come from a message consumer
	UserEntry     bool     `json:"-"`                             // some of its entries do not
	RateRPS       float64  `json:"rate_rps"`
	Replicas      int      `json:"replicas"`
	WorkersPerPod int      `json:"workers_per_pod"`
	WorkersLower  bool     `json:"workers_are_a_lower_bound,omitempty"`
	OwnMs         float64  `json:"own_ms"`
	CS2           float64  `json:"own_cs2"`
	GlueMs        float64  `json:"glue_ms,omitempty"` // time per visit holding no worker, outside the calls
	Util          float64  `json:"utilization"`
	WaitMs        float64  `json:"model_wait_ms"`
	ObservedWait  float64  `json:"observed_wait_ms"`
	ModelMs       float64  `json:"model_ms"`    // mean latency as the caller sees it (hop included)
	ObservedMs    float64  `json:"observed_ms"` // the callers' client-span mean (entries: the server span)
	ErrorPct      *float64 `json:"error_pct,omitempty"`
	AfterMs       *float64 `json:"after_ms,omitempty"`
	ChangeMs      *float64 `json:"change_ms,omitempty"`
	ChangePct     *float64 `json:"change_pct,omitempty"`
	Parallel      bool     `json:"parallel,omitempty"`
	Stage         bool     `json:"stage,omitempty"` // calls a service makes at once, one stage of its calls: a zero-time delay
	Images        []string `json:"images,omitempty"`
	// LostVisits are calls to the address this service answers on that had no span of it under them (a span lost, or
	// the trace context not passed on): visits missing from its rate and utilization, LostShare as a share of its visits.
	LostVisits int64   `json:"lost_visits,omitempty"`
	LostShare  float64 `json:"lost_share,omitempty"`
	// HopFromMean: the hop added to this service's latency (model_ms) is the mean client-minus-server time less the
	// model's wait, not its quietest tenth. The observed_wait_ms of this row, and the hop's part of observed_ms and
	// error_pct, are then set by the hop: they do not check it.
	HopFromMean bool `json:"hop_from_mean,omitempty"`
	// BrokenVisitShare is the share of its visits that are in traces that miss a span (left out of the call
	// structure), when it is a quarter or more.
	BrokenVisitShare float64 `json:"broken_visit_share,omitempty"`
	// VariableCalls are the callees this service calls a varying number of times per visit, with the visits by
	// calls and how the model takes them: the mean count, the stages, or a mixture of the call shapes.
	VariableCalls []infer.VariableCall `json:"variable_calls,omitempty"`
	// CallShapes are the shapes of the calls its visits take, with the share of visits that took each, when
	// it is modelled as a mixture of them.
	CallShapes []infer.CallShape `json:"call_shapes,omitempty"`
	// ReplicaShares is each replica's share of the visits when they are
	// uneven (the inference's pod_shares, in pod-name order); the model and the
	// change keep them.
	ReplicaShares []float64 `json:"replica_shares,omitempty"`

	// The keys report v2 adds to a row (V2). Build never sets them, so report v1 never carries them, and V1 clears them.
	//
	// UtilizationAfter is the utilization after the change, over the corners of the change set's ranges; at or above
	// 1 the service saturates. ChangeRangeMs is the change of ModelMs over the corners, nil when a corner leaves the
	// service no finite mean. Both are nil without a change. AfterMs, ChangeMs and ChangePct keep their v1 meaning in
	// report v2: set when every item of the change is a point, as with --patch.
	UtilizationAfter *change.Range `json:"utilization_after,omitempty"`
	ChangeRangeMs    *change.Range `json:"change_range_ms,omitempty"`
}

// Change describes a requested change of a service's own time per visit.
// DeltaMs is the whole change, GlueMs the part of it in the glue (time after
// the first call out, which holds no worker; a +N% change scales it too).
type Change struct {
	Service string  `json:"service"`
	DeltaMs float64 `json:"delta_ms"`
	GlueMs  float64 `json:"glue_delta_ms,omitempty"`
	Spec    string  `json:"spec"`
}

// AsyncCall is a call sent after the caller's response: it loads the callee
// but is not on the caller's latency path.
type AsyncCall struct {
	Caller   string  `json:"caller"`
	Callee   string  `json:"callee"`
	PerVisit float64 `json:"per_visit"`
}

// Report is the full result.
type Report struct {
	Version  string  `json:"version"`
	Capture  string  `json:"capture"`
	WindowS  float64 `json:"window_s"`
	Requests int64   `json:"requests"`
	// ConsumerRequests are the entry requests (in Requests) made by message
	// consumers: work a request's message causes later, not more user requests.
	ConsumerRequests int64       `json:"consumer_requests,omitempty"`
	AsyncCalls       []AsyncCall `json:"async_calls,omitempty"`
	Spans            int         `json:"spans"`
	Traces           int64       `json:"traces"`
	Broken           int64       `json:"broken_traces"`                 // traces that lost a span: in the queues, not in the call structure
	Probes           int         `json:"probe_spans_dropped,omitempty"` // health and readiness probes, not traffic
	RPS              float64     `json:"external_rps"`
	KeptShare        float64     `json:"kept_share"`
	MaxRho           float64     `json:"max_utilization"`
	Bottle           string      `json:"busiest_service"`
	Rows             []Row       `json:"services"`
	Changes          []Change    `json:"changes,omitempty"`
	Warnings         []string    `json:"warnings,omitempty"`
	Scope            string      `json:"scope"`
	Model            string      `json:"model"`

	// NestedSpans are the nested spans of one kind in one pod that were read as one, by service: SERVER spans
	// made INTERNAL inside another SERVER span, CLIENT spans made INTERNAL around another CLIENT span.
	NestedSpans []infer.NestedMerge `json:"nested_spans_merged,omitempty"`

	// FromTopology marks a report of a topology file (rhobound model): no
	// capture, so no window and nothing seen to compare with.
	FromTopology bool `json:"-"`
}

// Scope is printed with every report.
const Scope = "Mean latency only, for steady load below the utilization limit, assuming Poisson arrivals. " +
	"No tail (p95/p99), burst, retry or timeout modelling. Worker counts marked + are lower bounds."

// Build assembles a report from an inference and the solved model(s).
func Build(capture string, inf *infer.Inferred, kept float64, base *model.Result, d *model.Delta, changes []Change) *Report {
	topo := inf.Topology
	rep := &Report{Version: Version, Capture: capture, WindowS: inf.WindowS, Requests: inf.Requests,
		ConsumerRequests: inf.ConsumerRequests, Spans: inf.Spans, Traces: inf.Traces, Broken: inf.BrokenTraces,
		Probes: inf.ProbeSpans, NestedSpans: inf.NestedSpans, RPS: inf.LamExtPerMs * 1000, KeptShare: kept, Changes: changes, Warnings: inf.Warnings,
		Scope: Scope}
	for _, sv := range topo.Services {
		for _, c := range sv.Calls {
			if !c.Async {
				continue
			}
			callee := topo.Services[c.Child].Name
			if n := len(rep.AsyncCalls); n > 0 && rep.AsyncCalls[n-1].Caller == sv.Name && rep.AsyncCalls[n-1].Callee == callee {
				rep.AsyncCalls[n-1].PerVisit += c.P
				continue
			}
			rep.AsyncCalls = append(rep.AsyncCalls, AsyncCall{sv.Name, callee, c.P})
		}
	}
	mr, at := base.MaxRho()
	rep.MaxRho = mr
	if at >= 0 {
		rep.Bottle = topo.Services[at].Name
	}
	for i, st := range inf.Stats {
		sv := topo.Services[i]
		row := Row{Name: st.Name, External: st.External, Entry: st.Entry, ConsumerEntry: st.ConsumerEntries > 0,
			UserEntry: st.Entry && (st.Entries > st.ConsumerEntries || st.Entries == 0), RateRPS: base.Lam[i] * 1000,
			Replicas: sv.Servers, WorkersPerPod: sv.Workers, WorkersLower: st.WorkersInferred && !st.WorkersFromQueueing && !st.External,
			OwnMs: sv.S, CS2: sv.CS2, GlueMs: sv.Glue, Util: base.Rho[i], WaitMs: base.R[i] - sv.S, ObservedWait: st.ObservedWaitMs + st.InSpanWaitMs,
			Parallel: sv.Parallel, Stage: st.Stage, Images: st.Images, ReplicaShares: sv.Shares, VariableCalls: st.VariableCalls,
			CallShapes: st.CallShapes, LostVisits: st.LostVisits, LostShare: st.LostShare,
			BrokenVisitShare: st.BrokenVisitShare, HopFromMean: st.HopFromMean}
		if sv.Routing == model.Pooled {
			row.Replicas, row.WorkersPerPod = 1, sv.Servers*sv.Workers
		}
		hop := sv.Hop
		if st.Entry {
			hop = 0
			row.ObservedMs = st.ObservedServerMs
		} else {
			row.ObservedMs = st.ObservedClientMs
		}
		row.ModelMs = base.L[i] + hop
		if row.ObservedMs > 0 && !st.External {
			e := 100 * (row.ModelMs - row.ObservedMs) / row.ObservedMs
			row.ErrorPct = &e
		}
		if d != nil && d.Stable {
			after := d.Patched.L[i] + hop
			ch := after - row.ModelMs
			row.AfterMs, row.ChangeMs = &after, &ch
			if row.ModelMs > 0 {
				p := 100 * ch / row.ModelMs
				row.ChangePct = &p
			}
		}
		rep.Rows = append(rep.Rows, row)
	}
	return rep
}

// WriteJSON writes the report as indented JSON.
func (r *Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func f(v float64, d int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "-"
	}
	return fmt.Sprintf("%.*f", d, v)
}

// signedMs prints a change in ms with a sign and up to 3 decimals, never in
// exponent form (%+.3g printed +1.23e+03 for 1234.5 ms).
func signedMs(v float64) string {
	if v != 0 && math.Abs(v) < 0.001 {
		return fmt.Sprintf("%+.3g", v)
	}
	s := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%+.3f", v), "0"), ".")
	if s == "-0" || s == "+0" {
		return "+0"
	}
	return s
}

func pf(p *float64, d int, sign bool) string {
	if p == nil {
		return "-"
	}
	if sign {
		return fmt.Sprintf("%+.*f", d, *p)
	}
	return f(*p, d)
}

// errWriter keeps the first write error; once one happens it writes nothing more.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// writeHeader writes the capture's lines of the text report: its path and its
// window, requests and spans.
func (r *Report) writeHeader(w io.Writer) {
	fmt.Fprintf(w, "rhobound-cli: mean latency from traces\n")
	fmt.Fprintf(w, "capture   %s\n", r.Capture)
	if r.ConsumerRequests > 0 && r.Requests > 0 {
		// a message consumer's calls are entries of the services it calls, but not more user requests
		user := r.Requests - r.ConsumerRequests
		rate := r.RPS / float64(r.Requests)
		if user > 0 {
			fmt.Fprintf(w, "window    %.0f s, %d entry requests (%.1f /s) + %d from message consumers (%.1f /s), %d spans",
				r.WindowS, user, rate*float64(user), r.ConsumerRequests, rate*float64(r.ConsumerRequests), r.Spans)
		} else {
			fmt.Fprintf(w, "window    %.0f s, %d entry requests, all from message consumers (%.1f /s), %d spans",
				r.WindowS, r.ConsumerRequests, r.RPS, r.Spans)
		}
	} else if complete := r.Traces - r.Broken; r.Traces > 0 && r.Requests != complete {
		// a gateway outside the capture that calls several services, or retries one, in one trace
		fmt.Fprintf(w, "window    %.0f s, %d entry requests in %d traces (%.1f /s), %d spans", r.WindowS, r.Requests,
			complete, r.RPS, r.Spans)
	} else {
		fmt.Fprintf(w, "window    %.0f s, %d entry requests (%.1f /s), %d spans", r.WindowS, r.Requests, r.RPS, r.Spans)
	}
	if r.Broken > 0 {
		fmt.Fprintf(w, ", %d of %d traces broken", r.Broken, r.Traces)
	}
	if r.Probes > 0 {
		fmt.Fprintf(w, ", %d probe spans dropped", r.Probes)
	}
	fmt.Fprintln(w)
}

// inMixture reports whether a node's name is that of a node of a mixture of call shapes ("shape:S:j", "stage:S:j.n"), as
// opposed to a stage of the stage template ("stage:S:n").
func inMixture(name string) bool {
	if strings.HasPrefix(name, "shape:") {
		return true
	}
	if !strings.HasPrefix(name, "stage:") {
		return false
	}
	return strings.Contains(name[strings.LastIndex(name, ":")+1:], ".")
}

// WriteText writes the human report and returns the first write error.
func (r *Report) WriteText(out io.Writer) error {
	w := &errWriter{w: out}
	if r.FromTopology {
		fmt.Fprintf(w, "rhobound-cli: mean latency of a topology\n")
		fmt.Fprintf(w, "topology  %s\n", r.Capture)
		fmt.Fprintf(w, "load      %.1f requests/s\n", r.RPS)
	} else {
		r.writeHeader(w)
	}
	fmt.Fprintf(w, "busiest   %s at %.0f%% utilization\n", r.Bottle, 100*r.MaxRho)
	fmt.Fprintf(w, "model     %s\n", r.Model)
	fmt.Fprintf(w, "scope     %s\n\n", r.Scope)

	rows := append([]Row(nil), r.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return !rows[i].External && rows[j].External })
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', tabwriter.AlignRight)
	hasChange := len(r.Changes) > 0
	hasGlue := false // the column is shown only when some service has glue that rounds to more than 0.00
	hasStage, hasShape, hasShares, hasConsumer := false, false, false, false
	var lost, thin, hopMean []string // services with calls that lost their span, with many visits in broken traces, and a hop read from the mean
	for _, x := range rows {
		if x.HopFromMean {
			hopMean = append(hopMean, x.Name)
		}
		if x.LostVisits > 0 {
			lost = append(lost, fmt.Sprintf("%s %d (%.1f%%)", x.Name, x.LostVisits, 100*x.LostShare))
		}
		if x.BrokenVisitShare > 0 {
			thin = append(thin, fmt.Sprintf("%s %.0f%%", x.Name, 100*x.BrokenVisitShare))
		}
		hasGlue = hasGlue || x.GlueMs >= 0.005
		hasStage = hasStage || (x.Stage && !inMixture(x.Name))
		hasShape = hasShape || (x.Stage && inMixture(x.Name))
		hasShares = hasShares || x.ReplicaShares != nil
		hasConsumer = hasConsumer || x.ConsumerEntry
	}
	head := "service\trps\tpods x w\town ms\tcs2\t"
	if hasGlue {
		head += "glue ms\t"
	}
	head += "util\twait ms\tseen wait\tmodel ms\tseen ms\terr %\t"
	if hasChange {
		head += "after ms\tchange ms\tchange %\t"
	}
	fmt.Fprintln(tw, head)
	for _, x := range rows {
		name := x.Name
		switch {
		case x.Entry && x.ConsumerEntry && x.UserEntry:
			name += " *>"
		case x.Entry && x.ConsumerEntry:
			name += " >"
		case x.Entry:
			name += " *"
		}
		if x.Parallel {
			name += " ||"
		}
		if x.LostVisits > 0 || x.BrokenVisitShare > 0 {
			name += " !"
		}
		w2 := fmt.Sprint(x.WorkersPerPod)
		if x.WorkersLower {
			w2 += "+"
		}
		pods := fmt.Sprint(x.Replicas)
		if x.ReplicaShares != nil {
			pods += "~"
		}
		line := fmt.Sprintf("%s\t%s\t%s x %s\t%s\t%s\t", name, f(x.RateRPS, 1), pods, w2, f(x.OwnMs, 2), f(x.CS2, 2))
		if hasGlue {
			line += f(x.GlueMs, 2) + "\t"
		}
		seenWait, seen := f(x.ObservedWait, 2), f(x.ObservedMs, 2)
		if x.Stage || r.FromTopology { // a node of the model, not a service, or no traces at all: nothing was seen
			seenWait, seen = "-", "-"
		}
		line += fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t", f(100*x.Util, 0)+"%", f(x.WaitMs, 2), seenWait, f(x.ModelMs, 2),
			seen, pf(x.ErrorPct, 1, true))
		if hasChange {
			line += fmt.Sprintf("%s\t%s\t%s\t", pf(x.AfterMs, 2, false), pf(x.ChangeMs, 2, true), pf(x.ChangePct, 1, true))
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
	fmt.Fprintf(w, "\n* entry service   || parallel fan-out   w+ = workers per pod, a lower bound (too few requests waited, or too briefly, to read it from)\n")
	if r.FromTopology {
		fmt.Fprintf(w, "own ms: service time per visit (up to its first call out). seen wait, seen ms: a topology has no traces to compare with.\n")
	} else {
		fmt.Fprintf(w, "own ms: service time per visit (up to its first call out), without the wait for a worker inside the span. seen wait: queueing the traces show (in-span plus between client and server span).\n")
	}
	if hasGlue {
		fmt.Fprintf(w, "glue ms: time per visit after its first call that is not in a call (the worker is free by then); a pure delay.\n")
	}
	if !r.FromTopology {
		fmt.Fprintf(w, "seen ms: the mean the callers' client spans measured (entries: the server span).\n")
	}
	if hasStage {
		fmt.Fprintf(w, "stage:S:n: the calls S makes at once in stage n of its calls (each stage starts once the last has answered); a zero-time delay.\n")
	}
	if hasShape {
		fmt.Fprintf(w, "shape:S:j, stage:S:j.n: S's visits take several call shapes; S calls shape j with the share of its visits that took it "+
			"(rps: visits per second that do); a shape holds its stages one after another, and stage j.n is the calls made at once in "+
			"stage n of shape j; zero-time delays.\n")
	}
	if len(lost) > 0 {
		fmt.Fprintf(w, "! lost spans, calls to this service had no span of it under them (visits lost, share of its visits: %s): its service time, "+
			"queue and worker count are read from the visits captured (see the warnings).\n", strings.Join(lost, ", "))
	}
	if len(thin) > 0 {
		fmt.Fprintf(w, "! many visits in traces that miss a span (share of its visits: %s): they are left out of its call structure (see the warnings).\n",
			strings.Join(thin, ", "))
	}
	if len(hopMean) > 0 {
		fmt.Fprintf(w, "hop from the mean: the network time of the calls into %s is the mean client-minus-server span time less the model's wait "+
			"(not its quietest tenth, which lies below the mean by the spread of the network time). Their seen wait, and the hop's part of "+
			"their seen ms, are set by it, so they do not check it.\n", strings.Join(hopMean, ", "))
	}
	if hasShares {
		fmt.Fprintf(w, "pods~: the pods take uneven shares of the visits (from the traces, see the warnings), each a queue fed its share; the shares are kept at any load and after the change; util is the busiest pod's.\n")
	}
	if hasConsumer {
		fmt.Fprintf(w, "> entry from a message consumer: the visits a CONSUMER span's calls make, counted apart from the entry requests in the header.\n")
	}
	if len(r.AsyncCalls) > 0 {
		fmt.Fprintf(w, "async calls (sent after the response): they load the callee, but are not on the caller's latency path, so the callee's latency and its change do not reach the caller:")
		for i, a := range r.AsyncCalls {
			sep := ","
			if i == 0 {
				sep = ""
			}
			fmt.Fprintf(w, "%s %s -> %s (%.3g per visit)", sep, a.Caller, a.Callee, a.PerVisit)
		}
		fmt.Fprintln(w)
	}
	var vary []string
	for _, x := range rows {
		for _, v := range x.VariableCalls {
			vary = append(vary, fmt.Sprintf("%s -> %s %.3g per visit (%s)", x.Name, v.Callee, v.MeanPerVisit, v.ModelledAs))
		}
	}
	if len(vary) > 0 {
		fmt.Fprintf(w, "calls per visit that vary (the visits' counts are in the JSON report and the warnings): %s\n", strings.Join(vary, ", "))
	}
	if hasChange {
		fmt.Fprintf(w, "\nchange applied:")
		for _, c := range r.Changes {
			if c.GlueMs != 0 {
				fmt.Fprintf(w, " %s %s ms (%s: %s ms service time, %s ms glue);", c.Service, signedMs(c.DeltaMs), c.Spec,
					signedMs(c.DeltaMs-c.GlueMs), signedMs(c.GlueMs))
				continue
			}
			fmt.Fprintf(w, " %s %s ms (%s);", c.Service, signedMs(c.DeltaMs), c.Spec)
		}
		fmt.Fprintln(w)
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(w, "\nwarnings:\n")
		for _, s := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", strings.TrimSpace(s))
		}
	}
	return w.err
}
