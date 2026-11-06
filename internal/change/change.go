// SPDX-License-Identifier: Apache-2.0

// Package change is the change set (rhobound.change/v1, docs/mcp/schemas/change.v1.json; docs/mcp/DATA.md 11.2): the
// operations a prediction applies to a fit, each a number or a range, each with a label that says where the number
// came from (docs/mcp/ARCHITECTURE.md 4.3).
//
// Labels are checked, not trusted. A caller can only ever supply declared numbers: any other label must cite evidence
// the CLI resolves to a record it computed itself (an A/B record in its record store), and the item's value must be
// that record's; otherwise the set is refused with change.label_not_minted. Ranges are evaluated at their corners,
// oriented by each operation's sign (ARCHITECTURE.md 4.5).
//
// The life of a set in the CLI: Parse (or FromPatch for today's --patch), then Validate against the fit and the record
// store, then Resolve, which widens each measured item to its record's delta +-2 standard errors, then the engine.
package change

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/sanitize"
)

// Schema is the schema id of a change set.
const Schema = "rhobound.change/v1"

// The limits of a set: items, unsigned ranged items (2 x 2^6 = 128 solves at most; change.too_many_ranges above it),
// and the length of a note.
const (
	MaxItems          = 64
	MaxUnsignedRanges = 6
	MaxNoteChars      = 200
)

// The operations (DATA.md 11.2). This version applies OpOwnMs, OpOwnPct, OpWorkers and OpLoadScale from any label it
// accepts, and OpOwnVar and OpSetP from an A/B record only (measured-ab: Resolve also derives them from the record of
// every measured own_ms item); the others are refused with change.unsupported_op until their patches ship.
const (
	OpOwnMs        = "own_ms"     // +ms per visit, holding a worker (--patch svc=+Nms)
	OpOwnPct       = "own_pct"    // +-% of own time and glue (--patch svc=+N%)
	OpWorkers      = "workers"    // workers per pod (--workers)
	OpLoadScale    = "load_scale" // a multiplier of the external rate: a what-if beyond the fit's window
	OpGlueMs       = "glue_ms"
	OpCS2          = "cs2"
	OpAddCall      = "add_call"
	OpRemoveCall   = "remove_call"
	OpSetP         = "set_p"
	OpScaleCount   = "scale_count"
	OpSetAsync     = "set_async"
	OpMakeParallel = "make_parallel"
	// OpOwnVar is the difference of the own time's variance, ms^2 (an A/B record's own_var): with an own_ms item of
	// the same service the engine sets the service time's cs2 to (cs2 s^2 + own_var_ms2) / (s + own_ms)^2, so that a
	// change that adds rare long visits moves the queue by its second moment, not by its mean alone.
	OpOwnVar = "own_var_ms2"
)

// Ops lists every operation, in the order of the schema's enum.
var Ops = []string{OpOwnMs, OpOwnPct, OpWorkers, OpLoadScale, OpGlueMs, OpCS2, OpAddCall, OpRemoveCall, OpSetP,
	OpScaleCount, OpSetAsync, OpMakeParallel, OpOwnVar}

// Applies reports whether this version applies an operation.
func Applies(op string) bool {
	switch op {
	case OpOwnMs, OpOwnPct, OpWorkers, OpLoadScale, OpOwnVar, OpSetP:
		return true
	}
	return false
}

// MeasuredOnly reports whether this version applies an operation only from an A/B record (label measured-ab).
func MeasuredOnly(op string) bool { return op == OpOwnVar || op == OpSetP }

// The labels (ARCHITECTURE.md 4.3, defs.v1 Label). Only LabelDeclared may come from a caller; the CLI or the server
// mints the others from records they made, and a label never moves up.
const (
	LabelDerived             = "derived"
	LabelMeasuredAB          = "measured-ab"
	LabelMeasuredPaired      = "measured-paired"
	LabelMeasuredUnpaired    = "measured-unpaired"
	LabelDeclared            = "declared"
	LabelEstimatedStatic     = "estimated-static"
	LabelEstimatedMicrobench = "estimated-microbench"
	LabelEstimatedJudge      = "estimated-judge"
	LabelAssumed             = "assumed"
)

// Labels lists every label, in the order of the schema's enum.
var Labels = []string{LabelDerived, LabelMeasuredAB, LabelMeasuredPaired, LabelMeasuredUnpaired, LabelDeclared,
	LabelEstimatedStatic, LabelEstimatedMicrobench, LabelEstimatedJudge, LabelAssumed}

// The signs of an operation: the direction in which a larger value moves an entry's mean (ARCHITECTURE.md 4.5).
const (
	SignIncreasing = "increasing"
	SignDecreasing = "decreasing"
	SignUnsigned   = "unsigned"
)

// SignOf returns an operation's sign as ARCHITECTURE.md 4.5 states it: increasing for own_ms, own_pct, glue_ms,
// load_scale, own_var_ms2 (a queue's wait grows with the second moment at a fixed mean) and the added calls, call
// probabilities and counts; decreasing for workers; unsigned for set_async and
// make_parallel (their effect depends on the other branches); and "" for an operation whose sign is not stated yet
// (cs2, remove_call) or is unknown. The low corner of a set takes every increasing item at its Lo and every decreasing
// item at its Hi, the high corner the opposite ends; unsigned items are enumerated at every corner of their own ranges.
// That the entry mean is monotone in each item is an assumption ("monotonicity assumed (untested)").
func SignOf(op string) string {
	switch op {
	case OpOwnMs, OpOwnPct, OpGlueMs, OpLoadScale, OpAddCall, OpSetP, OpScaleCount, OpOwnVar:
		return SignIncreasing
	case OpWorkers:
		return SignDecreasing
	case OpSetAsync, OpMakeParallel:
		return SignUnsigned
	}
	return ""
}

// Range is a value range, Lo <= Hi; a point is Lo == Hi. It reads a bare number as a point (change.v1's Value is a
// number or a range) and is always written as {"lo", "hi"} (report.v2 items and entries need the object).
type Range struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// Point returns the range of one value.
func Point(v float64) Range { return Range{Lo: v, Hi: v} }

// IsPoint reports whether the range is one value.
func (r Range) IsPoint() bool { return r.Lo == r.Hi }

// UnmarshalJSON reads {"lo": L, "hi": H}, both required and nothing else, or a number N as {N, N}. It does not check
// that Lo <= Hi: Validate refuses an inverted range by name (change.range_inverted).
func (r *Range) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] != '{' {
		if string(b) == "null" {
			return errors.New("change: a range or number is null")
		}
		var v float64
		if err := json.Unmarshal(b, &v); err != nil {
			return fmt.Errorf("change: a value is a number or {\"lo\", \"hi\"}: %v", err)
		}
		*r = Point(v)
		return nil
	}
	var o struct {
		Lo *float64 `json:"lo"`
		Hi *float64 `json:"hi"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return fmt.Errorf("change: a range is {\"lo\", \"hi\"}: %v", err)
	}
	if o.Lo == nil || o.Hi == nil {
		return errors.New("change: a range needs both lo and hi")
	}
	*r = Range{Lo: *o.Lo, Hi: *o.Hi}
	return nil
}

// Item is one operation of a set.
type Item struct {
	Op      string `json:"op"`
	Service string `json:"service"`
	// Callee is the callee of a structural operation, or "".
	Callee string `json:"callee,omitempty"`
	// Route restricts an own_ms item to one route of its service, as the fit names it (its routes[].route, or
	// "other"): value ms on every visit of that route. The engine moves the service's queue by the route's share of
	// it and the callers by the route they reach. "" for the whole service.
	Route string `json:"route,omitempty"`
	Value Range  `json:"value"`
	Label string `json:"label"`
	// Evidence are the digests of the records a label other than declared rests on (an A/B record's Digest).
	Evidence []string `json:"evidence,omitempty"`
	// Note is the caller's words, at most MaxNoteChars; never parsed (a number in prose is change.free_text).
	Note string `json:"note,omitempty"`
	// Spec is the --patch text of an item FromPatch made ("+3ms", "3", "-10%"), kept so that report v1's
	// changes[].spec reads as the patch was typed, as `predict CAPTURE --patch` prints it; "" for an item read from a
	// change file. It is never serialized, so it is no part of the set's digest.
	Spec string `json:"-"`

	// patch is the --patch text of the item ("b=+3ms") and patchErr why its value could not be read, for an item
	// FromPatch made.
	patch    string
	patchErr error
}

// Set is a change set. Items is never nil in a set Parse or FromPatch made.
type Set struct {
	Schema string `json:"schema"`
	// Fit is the digest of the fit record the items were written against (fit.Record.Digest). FromPatch leaves it
	// "", and the CLI sets it to the fit it predicts from.
	Fit   string `json:"fit"`
	Items []Item `json:"items"`

	// parsed are the refusals Parse found in items that are not change items (a value as text, a route scope).
	parsed []issue
}

// Parse reads a change file. It checks the shape (the schema id, known keys, at most MaxItems items, the value's
// form: a whole number of at least 1 for workers, a positive number for load_scale); what the items say is checked by
// Validate. Input that is not a change item of this version is kept for Validate to refuse by name (DATA.md 11.2): a
// value given as text (change.free_text), an item restricted to a route (change.route_scope_unsupported), and an item
// whose "source" says its number is not own time per visit: "cpu-profile" (change.cpu_profile_not_own_time),
// "canary" or "histogram" (change.includes_queueing), "microbenchmark" (change.microbench_unvalidated).
func Parse(b []byte) (*Set, error) {
	var raw struct {
		Schema string            `json:"schema"`
		Fit    string            `json:"fit"`
		Items  []json.RawMessage `json:"items"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("change: %v", err)
	}
	if dec.More() {
		return nil, errors.New("change: more than one JSON value")
	}
	if raw.Schema != Schema {
		return nil, fmt.Errorf("change: schema %q, want %q", raw.Schema, Schema)
	}
	if len(raw.Items) > MaxItems {
		return nil, fmt.Errorf("change: %d items, at most %d", len(raw.Items), MaxItems)
	}
	s := &Set{Schema: raw.Schema, Fit: raw.Fit, Items: make([]Item, 0, len(raw.Items))}
	for i, ri := range raw.Items {
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(ri, &keys); err != nil {
			return nil, fmt.Errorf("change: item %d: %v", i, err)
		}
		var pending []issue
		if _, ok := keys["route"]; ok {
			var op, route string
			_ = json.Unmarshal(keys["op"], &op)
			if err := json.Unmarshal(keys["route"], &route); op != OpOwnMs || err != nil || route == "" {
				delete(keys, "route")
				pending = append(pending, issue{code: contract.ChangeRouteScopeUnsupported,
					msg: "an item restricted to a route other than an own_ms item on a route the fit names; give the per-visit mean (ms times the route's share of visits)"})
			}
		}
		if src, ok := keys["source"]; ok {
			delete(keys, "source")
			var name string
			if err := json.Unmarshal(src, &name); err != nil {
				return nil, fmt.Errorf("change: item %d: source: %v", i, err)
			}
			is, ok := sources[name]
			if !ok {
				return nil, fmt.Errorf("change: item %d: source %q is not one this version reads (an A/B record is cited as "+
					"evidence of a measured-ab item)", i, name)
			}
			pending = append(pending, is)
		}
		if v, ok := keys["value"]; ok && len(bytes.TrimSpace(v)) > 0 && bytes.TrimSpace(v)[0] == '"' {
			keys["value"] = json.RawMessage("0")
			pending = append(pending, issue{code: contract.ChangeFreeText,
				msg: "a value given as text; a number in prose is never read: give it as a number or {\"lo\", \"hi\"}"})
		}
		clean, err := json.Marshal(keys)
		if err != nil {
			return nil, fmt.Errorf("change: item %d: %v", i, err)
		}
		var it Item
		dec := json.NewDecoder(bytes.NewReader(clean))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&it); err != nil {
			return nil, fmt.Errorf("change: item %d: %v", i, err)
		}
		if it.Service == "" || it.Op == "" || it.Label == "" || keys["value"] == nil {
			return nil, fmt.Errorf("change: item %d: op, service, value and label are required", i)
		}
		if !slices.Contains(Ops, it.Op) {
			return nil, fmt.Errorf("change: item %d: op %q is not one of %s", i, it.Op, strings.Join(Ops, ", "))
		}
		if !slices.Contains(Labels, it.Label) {
			return nil, fmt.Errorf("change: item %d: label %q is not one of %s", i, it.Label, strings.Join(Labels, ", "))
		}
		if utf8.RuneCountInString(it.Note) > MaxNoteChars {
			return nil, fmt.Errorf("change: item %d: a note of more than %d characters", i, MaxNoteChars)
		}
		if len(pending) == 0 {
			if err := checkValue(it); err != nil {
				return nil, fmt.Errorf("change: item %d: %v", i, err)
			}
		}
		for _, p := range pending {
			p.item = len(s.Items)
			s.parsed = append(s.parsed, p)
		}
		s.Items = append(s.Items, it)
	}
	return s, nil
}

// sources are the kinds of number a change file may say its value came from (an item's "source", which change.v1
// does not define) that are never own time per visit, each refused by name (DATA.md 11.2).
var sources = map[string]issue{
	"cpu-profile": {code: contract.ChangeCPUProfileNotOwnTime,
		msg: "a CPU profile gives CPU per call, not the time a visit holds a worker; measure own time per visit (an A/B run)"},
	"canary": {code: contract.ChangeIncludesQueueing,
		msg: "a canary's latency already contains the queue; as own time it would count the queue twice"},
	"histogram": {code: contract.ChangeIncludesQueueing,
		msg: "a latency histogram already contains the queue; as own time it would count the queue twice"},
	"microbenchmark": {code: contract.ChangeMicrobenchUnvalidated,
		msg: "a microbenchmark delta is accepted for research only, until microbenchmarks are validated against measured differences"},
}

// checkValue checks the form of an item's value for its operation.
func checkValue(it Item) error {
	for _, v := range []float64{it.Value.Lo, it.Value.Hi} {
		switch it.Op {
		case OpWorkers:
			if v != math.Trunc(v) || v < 1 || v > 1<<20 {
				return fmt.Errorf("workers is a whole number of workers per pod, at least 1, not %v", v)
			}
		case OpLoadScale:
			if !(v > 0) || math.IsInf(v, 0) {
				return fmt.Errorf("load_scale multiplies the rate: a positive number, not %v", v)
			}
		case OpSetP:
			if !(v >= 0 && v <= 1) {
				return fmt.Errorf("set_p is a call's probability per visit: a number in [0, 1], not %v", v)
			}
		}
	}
	return nil
}

// FromPatch turns today's --patch text ("svc=+3ms,other=-10%"; a bare number is ms) into a set of declared own_ms and
// own_pct items, in the order given, each keeping its text in Spec. Service names are not checked here (FromPatch has
// no topology): Validate does that. An item whose value cannot be read is kept and refused by Refusal, in the order
// `predict CAPTURE --patch` refuses (an unknown service first), with the message it has always printed.
func FromPatch(spec string) (*Set, error) {
	s := &Set{Schema: Schema, Items: []Item{}}
	for _, kv := range splitList(spec) {
		name, val, ok := strings.Cut(kv, "=")
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		it := Item{Service: name, Label: LabelDeclared, Spec: val, patch: kv, Op: OpOwnMs}
		if !ok {
			it.Service = "" // no service named: refused as an unknown service, as --patch has always done
		}
		var v float64
		var err error
		if strings.HasSuffix(val, "%") {
			it.Op = OpOwnPct
			v, err = strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64)
		} else {
			v, err = strconv.ParseFloat(strings.TrimSuffix(val, "ms"), 64)
		}
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			it.patchErr = fmt.Errorf("--patch: cannot read %q (use +3ms or -10%%)", kv)
		}
		it.Value = Point(v)
		s.Items = append(s.Items, it)
	}
	return s, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Records is the CLI's record store: the A/B records it computed, by digest.
type Records interface {
	AB(digest string) (*abdiff.Record, bool)
}

// issue is one problem of a set: a finding's code, the item it concerns (-1 for the set) and the message the text
// form prints.
type issue struct {
	code     string
	item     int
	subject  string
	numbers  map[string]float64
	examples []string
	msg      string
}

func (i issue) finding() contract.Finding {
	f := contract.NewFinding(i.code)
	if i.subject != "" {
		f = f.WithSubject(i.subject)
	}
	if len(i.examples) > 0 {
		f = f.WithExamples(i.examples...)
	}
	for _, k := range sortedKeys(i.numbers) {
		f = f.WithNumber(k, i.numbers[k])
	}
	return f
}

// Validate checks the set against the fit it is applied to and the record store, and returns its findings, sorted:
// refusals (change.unknown_service, workers included; change.negative_time; change.range_inverted;
// change.too_many_ranges; change.unsupported_op; change.label_not_minted for a label other than declared whose
// evidence is not a record in recs, or whose value is not that record's delta), then the doubts the cited records
// carry (profile.variance_changed, profile.structure_changed, profile.self_report_mismatch, and the records' own
// findings), which the report lists so the gate can hold on them. A set with a refusal is never predicted.
//
// An item FromPatch could not read has no code and no finding: a set made by FromPatch goes through Refusal first,
// which returns that error. A record that Build, FromInferred or Unmarshal did not make is a programming error and
// panics.
func (s *Set) Validate(r *fit.Record, recs Records) []contract.Finding {
	issues, _, err := s.check(r, recs)
	if err != nil {
		panic(fmt.Sprintf("change: validate against a record without its inference: %v", err))
	}
	out := make([]contract.Finding, 0, len(issues))
	for _, is := range issues {
		out = append(out, is.finding())
	}
	contract.SortFindings(out)
	return out
}

// Refusal returns the set's first refusal, in the order of its items (the order `predict CAPTURE --patch` refuses
// in, with the messages it prints for a patch's items), as an *Error; nil when Validate finds no refusal. An
// unreadable --patch value is an error with no code, as it has always been.
func (s *Set) Refusal(r *fit.Record, recs Records) error {
	issues, patchErr, err := s.check(r, recs)
	if err != nil {
		return err
	}
	if patchErr != nil {
		return patchErr
	}
	var first *issue
	for k := range issues {
		is := &issues[k]
		if is.finding().Level != contract.Refuse {
			continue
		}
		if first == nil || is.item < first.item {
			first = is
		}
	}
	if first == nil {
		return nil
	}
	return &Error{Finding: first.finding(), Err: errors.New(first.msg)}
}

// Error is a set's refusal: a refuse-level finding and the message the text form prints.
type Error struct {
	Finding contract.Finding
	Err     error
}

// Error returns the message.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the message's error.
func (e *Error) Unwrap() error { return e.Err }

// check returns every issue of the set, and the first unreadable --patch value when no refusal of an earlier item comes
// before it; the error is the record's.
func (s *Set) check(r *fit.Record, recs Records) ([]issue, error, error) {
	inf, err := r.Inferred()
	if err != nil {
		return nil, nil, err
	}
	topo := inf.Topology
	targets := patchTargets(topo)
	issues := append([]issue(nil), s.parsed...)
	add := func(code string, item int, subject, msg string, numbers map[string]float64) {
		issues = append(issues, issue{code: code, item: item, subject: subject, msg: msg, numbers: numbers})
	}
	var patchErr error
	patchErrItem := -1
	// the lowest own time and glue each service reaches, over the items so far (the v1 check accumulates)
	ds, glue := map[int]float64{}, map[int]float64{}
	// per service, the own_ms items on the whole service, and per route those on the route (route items, CB-3)
	wholeDS, routeDS := map[int]float64{}, map[int]map[int]float64{}
	unsigned := 0
	seenRec := map[string]bool{}
	var doubts []issue
	shown := sanitize.Services(topo.Names())
	for k, it := range s.Items {
		i, ambiguous := nodeOf(topo, shown, it)
		if ambiguous {
			add(contract.ChangeAmbiguousService, k, it.Service, fmt.Sprintf("%s: the views show more than one service as %q",
				it.Op, it.Service), nil)
			continue
		}
		if it.Service == "" || i < 0 {
			msg := fmt.Sprintf("%s: unknown service %q (services: %s)", it.Op, it.Service, strings.Join(targets, ", "))
			if it.patch != "" {
				msg = fmt.Sprintf("--patch: unknown service in %q (services: %s)", it.patch, strings.Join(targets, ", "))
			}
			add(contract.ChangeUnknownService, k, it.Service, msg, nil)
			continue
		}
		if it.patchErr != nil {
			if patchErrItem < 0 {
				patchErr, patchErrItem = it.patchErr, k
			}
			continue
		}
		if parsedFor(s.parsed, k) {
			continue
		}
		if SignOf(it.Op) == SignUnsigned && !it.Value.IsPoint() {
			unsigned++ // counted whether or not this version applies the operation
		}
		if !Applies(it.Op) {
			add(contract.ChangeUnsupportedOp, k, it.Service, fmt.Sprintf("%s: this version cannot apply %s (it applies %s, %s, %s and %s, "+
				"and %s and %s from an A/B record)", it.Service, it.Op, OpOwnMs, OpOwnPct, OpWorkers, OpLoadScale, OpOwnVar, OpSetP), nil)
			continue
		}
		if MeasuredOnly(it.Op) && it.Label != LabelMeasuredAB {
			add(contract.ChangeUnsupportedOp, k, it.Service, fmt.Sprintf("%s: this version applies %s from an A/B record only "+
				"(label measured-ab, citing the record)", it.Service, it.Op), nil)
			continue
		}
		if it.Value.Lo > it.Value.Hi {
			add(contract.ChangeRangeInverted, k, it.Service, fmt.Sprintf("%s %s: the range %v..%v has lo above hi",
				it.Service, it.Op, it.Value.Lo, it.Value.Hi), map[string]float64{"lo": it.Value.Lo, "hi": it.Value.Hi})
			continue
		}
		sv := topo.Services[i]
		switch it.Op {
		case OpWorkers:
			if sv.Delay || inf.Stats[i].Stage {
				add(contract.ChangeUnsupportedOp, k, it.Service, fmt.Sprintf("%s has no workers to change: it is %s",
					it.Service, map[bool]string{true: "an outside delay", false: "a node of the call structure"}[sv.Delay]), nil)
				continue
			}
		case OpLoadScale:
			if _, ok := topo.Entry[i]; !ok {
				add(contract.ChangeUnsupportedOp, k, it.Service, fmt.Sprintf("load_scale scales an entry's requests; %s is not an entry",
					it.Service), nil)
				continue
			}
		case OpSetP:
			if _, why := DirectCall(topo, i, it.Callee); why != "" {
				add(contract.ChangeUnsupportedOp, k, it.Service, fmt.Sprintf("%s set_p %s: %s", it.Service, it.Callee, why), nil)
				continue
			}
		case OpOwnMs:
			lo := it.Value.Lo
			if it.Label == LabelMeasuredAB {
				// Resolve applies a measured item at its record's delta - 2 SE: check that corner, not the point
				// (lean/Rbproofs/Lanes/ChangeBand.lean, CB-4)
				if _, rsv, why := measured(it, recs, topo); why == "" {
					d := rsv.OwnMs
					if j := slices.IndexFunc(rsv.Routes, func(x abdiff.Route) bool { return x.Route == it.Route }); it.Route != "" && j >= 0 {
						d = rsv.Routes[j].OwnMs
					}
					lo = math.Min(lo, d.Delta-2*d.SE)
				}
			}
			if it.Route != "" {
				rs := inf.Stats[i].Routes
				r := slices.IndexFunc(rs, func(x infer.RouteStats) bool { return x.Route == it.Route })
				if r < 0 {
					var names []string
					for _, x := range rs {
						names = append(names, x.Route)
					}
					add(contract.ChangeRouteScopeUnsupported, k, it.Service, fmt.Sprintf("%s: the fit names no route %q of it "+
						"(its routes: %s)", it.Service, it.Route, strings.Join(names, ", ")), nil)
					continue
				}
				ds[i] += rs[r].Share * lo
				if routeDS[i] == nil {
					routeDS[i] = map[int]float64{}
				}
				// a route moves by its own items and by every whole-service item, as routePatch moves it (CB-3)
				routeDS[i][r] += lo
				if d := wholeDS[i] + routeDS[i][r]; rs[r].OwnMs+d < 0 {
					add(contract.ChangeNegativeTime, k, it.Service, fmt.Sprintf("%s: own_ms %v on %s would make its service "+
						"time negative", it.Service, lo, it.Route), map[string]float64{"own_ms": rs[r].OwnMs, "change_ms": d})
				}
				break
			}
			ds[i] += lo
			wholeDS[i] += lo
			if sv.S+ds[i] < 0 {
				msg := fmt.Sprintf("%s: own_ms %v would make its service time negative", it.Service, lo)
				if it.patch != "" {
					msg = fmt.Sprintf("--patch: %s would get a negative service time", it.Service)
				}
				add(contract.ChangeNegativeTime, k, it.Service, msg, map[string]float64{"own_ms": sv.S, "change_ms": ds[i]})
			} else if rs := inf.Stats[i].Routes; len(rs) > 0 {
				for r, x := range rs {
					if d := wholeDS[i] + routeDS[i][r]; x.OwnMs+d < 0 {
						add(contract.ChangeNegativeTime, k, it.Service, fmt.Sprintf("%s: own_ms %v would make the own time of "+
							"its route %s negative", it.Service, lo, x.Route), map[string]float64{"own_ms": x.OwnMs, "change_ms": d})
						break
					}
				}
			}
		case OpOwnPct:
			ds[i] += sv.S * it.Value.Lo / 100
			glue[i] += sv.Glue * it.Value.Lo / 100
			switch {
			case sv.S+ds[i] < 0:
				msg := fmt.Sprintf("%s: own_pct %v would make its service time negative", it.Service, it.Value.Lo)
				if it.patch != "" {
					msg = fmt.Sprintf("--patch: %s would get a negative service time", it.Service)
				}
				add(contract.ChangeNegativeTime, k, it.Service, msg, map[string]float64{"own_ms": sv.S, "change_ms": ds[i]})
			case sv.Glue+glue[i] < 0:
				msg := fmt.Sprintf("%s: own_pct %v would make its glue time negative", it.Service, it.Value.Lo)
				if it.patch != "" {
					msg = fmt.Sprintf("--patch: %s would get a negative glue time", it.Service)
				}
				add(contract.ChangeNegativeTime, k, it.Service, msg, map[string]float64{"glue_ms": sv.Glue, "change_ms": glue[i]})
			}
		}
		// the label
		switch it.Label {
		case LabelDeclared:
		case LabelEstimatedMicrobench:
			add(contract.ChangeMicrobenchUnvalidated, k, it.Service, fmt.Sprintf("%s: a microbenchmark delta is accepted for "+
				"research only, until microbenchmarks are validated against measured differences", it.Service), nil)
		case LabelMeasuredAB:
			named := it
			named.Service = sv.Name // the record names the service as the traces wrote it
			rec, svc, why := measured(named, recs, topo)
			if why != "" {
				add(contract.ChangeLabelNotMinted, k, it.Service, fmt.Sprintf("%s: measured-ab %s", it.Service, why), nil)
				continue
			}
			key := it.Evidence[0] + "\x00" + sv.Name
			if seenRec[key] {
				continue
			}
			seenRec[key] = true
			structModelled, varModelled := structureModelled(topo, i, svc), varianceModelled(svc)
			if svc.CallsChanged && !structModelled {
				doubts = append(doubts, issue{code: contract.ProfileStructureChanged, item: k, subject: it.Service})
			}
			if svc.VarianceChanged && !varModelled {
				doubts = append(doubts, issue{code: contract.ProfileVarianceChanged, item: k, subject: it.Service,
					numbers: map[string]float64{"var_delta": svc.OwnVar.Delta, "var_se": svc.OwnVar.SE}})
			}
			if !rec.SelfReportConsistent && !seenRec[it.Evidence[0]] {
				doubts = append(doubts, issue{code: contract.ProfileSelfReportMismatch, item: k})
			}
			if !seenRec[it.Evidence[0]] {
				for _, f := range rec.Findings {
					// a record's routes that moved differently are no doubt for a set that applies them by route
					if f.Code == contract.ProfileRouteHeterogeneous && s.byRoute(it.Evidence[0], f.Subject, shown, topo) {
						continue
					}
					// nor a changed variance or call structure of this service: the record's flags above already raised it,
					// or found that the derived items apply it (the finding repeats the flag)
					if f.Subject == sv.Name && ((f.Code == contract.ProfileVarianceChanged && (varModelled || svc.VarianceChanged)) ||
						(f.Code == contract.ProfileStructureChanged && (structModelled || svc.CallsChanged))) {
						continue
					}
					// a changed variance of a service the item does not change is one of the record's tests of the
					// services it does not touch: a doubt only past the bound for that many tests (UntouchedVarianceZ)
					if f.Code == contract.ProfileVarianceChanged && f.Subject != sv.Name && !untouchedVarianceHolds(rec, f.Subject, sv.Name) {
						continue
					}
					if f.Level != contract.Refuse {
						d := issue{code: f.Code, item: k, subject: f.Subject, numbers: f.Numbers}
						if f.Code == contract.ABPayloadChanged {
							// the cost of a changed payload falls on the callers: name the fit's callers of the service
							if j := topo.Index(f.Subject); j >= 0 {
								d.examples = Callers(topo, j)
							}
						}
						doubts = append(doubts, d)
					}
				}
			}
			seenRec[it.Evidence[0]] = true
		default:
			add(contract.ChangeLabelNotMinted, k, it.Service, fmt.Sprintf("%s: the label %s needs a record this version "+
				"can find and recompute; only declared and measured-ab (with its A/B record) are accepted", it.Service, it.Label), nil)
		}
	}
	if unsigned > MaxUnsignedRanges {
		add(contract.ChangeTooManyRanges, -1, "", fmt.Sprintf("%d unsigned ranged items; at most %d", unsigned, MaxUnsignedRanges),
			map[string]float64{"unsigned_ranges": float64(unsigned), "limit": MaxUnsignedRanges})
	}
	issues = append(issues, doubts...)
	if patchErr != nil {
		// an unreadable value is refused before any refusal of a later item, after those of earlier items
		for _, is := range issues {
			if contract.NewFinding(is.code).Level == contract.Refuse && is.item < patchErrItem && is.item >= 0 {
				return issues, nil, nil
			}
		}
		return issues, patchErr, nil
	}
	return issues, nil, nil
}

// byRoute reports whether the set applies service svc (as the record names it) of the record digest by route: some
// own_ms item with a route cites that record for that service.
func (s *Set) byRoute(digest, svc string, shown map[string]sanitize.Shown, topo *model.Topology) bool {
	for _, it := range s.Items {
		if it.Route == "" || it.Op != OpOwnMs || len(it.Evidence) != 1 || it.Evidence[0] != digest {
			continue
		}
		if i, _ := nodeOf(topo, shown, it); i >= 0 && topo.Services[i].Name == svc {
			return true
		}
	}
	return false
}

// nodeOf is the node of the topology an item names: for an item of a change set, the node the views show by that name,
// else the one of that name as the traces wrote it (sanitize.Resolve), so that a caller can name a node whose name the
// views replace; for a --patch item, only the latter, as rhobound-cli read a patch. It returns -1 for none, and
// ambiguous true when the views show two nodes by the name.
func nodeOf(topo *model.Topology, shown map[string]sanitize.Shown, it Item) (i int, ambiguous bool) {
	if it.patch != "" {
		return topo.Index(it.Service), false
	}
	node, ok, ambiguous := sanitize.Resolve(shown, it.Service)
	if !ok {
		return -1, ambiguous
	}
	return topo.Index(node), false
}

func parsedFor(parsed []issue, item int) bool {
	for _, p := range parsed {
		if p.item == item {
			return true
		}
	}
	return false
}

// measured resolves a measured-ab item to its record and the record's row for the item's service, or says why it
// cannot: no evidence, a record the store does not hold, a service the record does not measure, an operation a record
// does not measure, or a value that is not the record's (own_ms: the own-time delta; own_var_ms2: the variance delta;
// set_p: the fit's probability of the call times the record's ratio of calls per visit, MeasuredP).
// UntouchedVarianceZ is the bound, in standard errors, that the variance difference of one of k services an A/B
// record measured but the change does not touch must pass to be a doubt (profile.variance_changed): the Šidák
// correction of the record's 2-SE test, so that the k tests together raise the doubt by chance as often as one 2-SE
// test does (4.55%). 2 for k <= 1; about 2.60 for 5. Each of k untouched services tested at 2 SE would raise it by
// chance in 1 - 0.9545^k of the A/Bs (21% for 5): CT3's maze bed, whose catalog A/B measures five untouched shards,
// held two clean changes in five that way (Muse experiment, 2026-11-03).
func UntouchedVarianceZ(k int) float64 {
	if k <= 1 {
		return 2
	}
	a0 := math.Erfc(2 / math.Sqrt2)
	a := 1 - math.Pow(1-a0, 1/float64(k))
	return math.Sqrt2 * math.Erfinv(1-a)
}

// untouchedVarianceHolds reports whether the record's profile.variance_changed of service subject, which the item for
// service own does not change, is past UntouchedVarianceZ of the record's untouched services. A service measured by
// route (its test is against the routes' mixture, not the plain difference) keeps the record's 2-SE flag.
func untouchedVarianceHolds(rec *abdiff.Record, subject, own string) bool {
	k := 0
	var sub *abdiff.Service
	for i := range rec.Services {
		if rec.Services[i].Service != own {
			k++
		}
		if rec.Services[i].Service == subject {
			sub = &rec.Services[i]
		}
	}
	if sub == nil || sub.Routes != nil || !(sub.OwnVar.SE > 0) {
		return true
	}
	return math.Abs(sub.OwnVar.Delta) > UntouchedVarianceZ(k)*sub.OwnVar.SE
}

func measured(it Item, recs Records, topo *model.Topology) (*abdiff.Record, *abdiff.Service, string) {
	if len(it.Evidence) != 1 {
		return nil, nil, "must cite exactly one A/B record (evidence)"
	}
	if recs == nil {
		return nil, nil, "cites a record, and no record store is open"
	}
	rec, ok := recs.AB(it.Evidence[0])
	if !ok {
		return nil, nil, "cites " + it.Evidence[0] + ", which is not an A/B record this tool computed"
	}
	if it.Op != OpOwnMs && it.Op != OpOwnVar && it.Op != OpSetP {
		return nil, nil, "applies to own_ms, own_var_ms2 and set_p only"
	}
	for k := range rec.Services {
		svc := &rec.Services[k]
		if svc.Service != it.Service {
			continue
		}
		switch it.Op {
		case OpOwnMs:
			if it.Route != "" {
				r := slices.IndexFunc(svc.Routes, func(x abdiff.Route) bool { return x.Route == it.Route })
				if r < 0 {
					return nil, nil, "cites a record that does not measure route " + it.Route + " of " + it.Service
				}
				if d := svc.Routes[r].OwnMs; it.Value != Point(d.Delta) {
					return nil, nil, fmt.Sprintf("value %v..%v is not the record's own-time delta %v of route %s", it.Value.Lo,
						it.Value.Hi, d.Delta, it.Route)
				}
			} else if it.Value != Point(svc.OwnMs.Delta) {
				return nil, nil, fmt.Sprintf("value %v..%v is not the record's own-time delta %v", it.Value.Lo, it.Value.Hi, svc.OwnMs.Delta)
			}
		case OpOwnVar:
			if it.Value != Point(svc.OwnVar.Delta) {
				return nil, nil, fmt.Sprintf("value %v..%v is not the record's own-time variance delta %v", it.Value.Lo, it.Value.Hi,
					svc.OwnVar.Delta)
			}
		case OpSetP:
			p, _, why := MeasuredP(topo, svc, it.Callee)
			if why != "" {
				return nil, nil, why
			}
			if it.Value != Point(p) {
				return nil, nil, fmt.Sprintf("value %v..%v is not the fit's probability of the call times the record's ratio of "+
					"calls per visit, %v", it.Value.Lo, it.Value.Hi, p)
			}
		}
		for _, f := range rec.Findings {
			if f.Level == contract.Refuse {
				return nil, nil, "cites a record that refused its run (" + f.Code + ")"
			}
		}
		return rec, svc, ""
	}
	return nil, nil, "cites a record that does not measure " + it.Service
}

// Resolve returns the set the engine applies to the fit r: a copy in which every item names its service as the traces
// wrote it (nodeOf: a caller may name it as the views show it), and every measured-ab item's value is its record's
// delta +-2 standard errors (the errors by batch means, as the record computed them). It expects a set Validate
// admitted.
func (s *Set) Resolve(r *fit.Record, recs Records) (*Set, error) {
	inf, err := r.Inferred()
	if err != nil {
		return nil, err
	}
	topo := inf.Topology
	shown := sanitize.Services(topo.Names())
	out := &Set{Schema: s.Schema, Fit: s.Fit, Items: make([]Item, len(s.Items))}
	type key struct{ op, service, callee, evidence string }
	given := map[key]bool{}
	var rows []measuredRow
	for k, it := range s.Items {
		if i, _ := nodeOf(topo, shown, it); i >= 0 {
			it.Service = topo.Services[i].Name
		}
		it.Evidence = append([]string(nil), it.Evidence...)
		if it.Label == LabelMeasuredAB {
			_, svc, why := measured(it, recs, topo)
			if why != "" {
				return nil, fmt.Errorf("change: item %d: measured-ab %s", k, why)
			}
			it.Value = widen(topo, svc, it.Op, it.Callee)
			if it.Op == OpOwnMs && it.Route != "" {
				d := svc.Routes[slices.IndexFunc(svc.Routes, func(x abdiff.Route) bool { return x.Route == it.Route })].OwnMs
				it.Value = Range{Lo: d.Delta - 2*d.SE, Hi: d.Delta + 2*d.SE}
			}
			given[key{it.Op, it.Service, it.Callee, it.Evidence[0]}] = true
			if it.Op == OpOwnMs {
				rows = append(rows, measuredRow{svc: svc, evidence: it.Evidence[0]})
			}
		}
		out.Items[k] = it
	}
	// The items the record of every measured own_ms item implies (derive): its variance difference, and the changed
	// calls per visit to callees the fit reaches by one direct call, unless the set gives them itself.
	for _, row := range rows {
		for _, it := range derive(topo, row.svc, row.evidence) {
			if !given[key{it.Op, it.Service, it.Callee, it.Evidence[0]}] && len(out.Items) < MaxItems {
				given[key{it.Op, it.Service, it.Callee, it.Evidence[0]}] = true
				out.Items = append(out.Items, it)
			}
		}
	}
	return out, nil
}

type measuredRow struct {
	svc      *abdiff.Service
	evidence string
}

// widen is a measured item's range: the record's difference +-2 standard errors (own_ms, own_var_ms2), or the fit's
// probability of the call times the record's ratio of calls per visit +-2 standard errors, within [0, 1] (set_p).
func widen(topo *model.Topology, svc *abdiff.Service, op, callee string) Range {
	switch op {
	case OpOwnVar:
		d := svc.OwnVar
		return Range{Lo: d.Delta - 2*d.SE, Hi: d.Delta + 2*d.SE}
	case OpSetP:
		_, r, _ := MeasuredP(topo, svc, callee)
		return r
	}
	d := svc.OwnMs
	return Range{Lo: d.Delta - 2*d.SE, Hi: d.Delta + 2*d.SE}
}

// derive returns the measured items an A/B record's row implies beside its own-time difference, already widened: the
// own time's variance difference (own_var_ms2; none for a record that does not carry it), and a set_p item for each
// callee whose calls per visit changed (abdiff.CallsGap) and that the fit reaches from the service by one direct call
// (DirectCall). A changed callee reached otherwise (through the call structure's stage or shape nodes, or by several
// calls) gets no item: the record's profile.structure_changed doubt then stands for it alone.
func derive(topo *model.Topology, svc *abdiff.Service, evidence string) []Item {
	var out []Item
	if svc.OwnVar.SE > 0 || svc.OwnVar.Delta != 0 {
		out = append(out, Item{Op: OpOwnVar, Service: svc.Service, Label: LabelMeasuredAB, Evidence: []string{evidence},
			Value: widen(topo, svc, OpOwnVar, "")})
	}
	for _, c := range svc.Calls {
		if !(math.Abs(c.Candidate-c.Baseline) > abdiff.CallsGap*math.Max(c.Baseline, c.Candidate)) {
			continue
		}
		if _, r, why := MeasuredP(topo, svc, c.Callee); why == "" {
			out = append(out, Item{Op: OpSetP, Service: svc.Service, Callee: c.Callee, Label: LabelMeasuredAB,
				Evidence: []string{evidence}, Value: r})
		}
	}
	return out
}

// varianceModelled reports whether the engine applies a record's variance difference for the service: the derived
// own_var_ms2 item (derive), which every measured record with a variance difference gets. profile.variance_changed is
// then no doubt, as model.batch_arrivals is none once the batch term models the batch.
func varianceModelled(svc *abdiff.Service) bool {
	return svc.OwnVar.SE > 0 || svc.OwnVar.Delta != 0
}

// structureModelled reports whether the engine applies a record's changed call structure for service i: every callee
// whose calls per visit changed (abdiff.CallsGap) gets a derived set_p item, a direct call the engine can change, and
// the service has no routes (set_p moves the blend's call chance, not each route's; a routed service keeps the doubt).
func structureModelled(topo *model.Topology, i int, svc *abdiff.Service) bool {
	if len(topo.Services[i].Routes) > 0 {
		return false
	}
	covered := 0
	for _, c := range svc.Calls {
		if !(math.Abs(c.Candidate-c.Baseline) > abdiff.CallsGap*math.Max(c.Baseline, c.Candidate)) {
			continue
		}
		if _, _, why := MeasuredP(topo, svc, c.Callee); why != "" {
			return false
		}
		covered++
	}
	return covered > 0 // a record that says the calls changed without naming a changed callee keeps the doubt
}

// DirectCall returns the index in the service's calls of its one direct call to callee, or why a set_p item cannot
// change it: no callee named, an unknown callee, no direct call or more than one (a callee the fit reaches through
// the call structure's stage or shape nodes), or a parallel fan-out (one with a call probability is not modelled).
func DirectCall(topo *model.Topology, i int, callee string) (int, string) {
	if callee == "" {
		return -1, "set_p names the callee whose call it changes"
	}
	j := topo.Index(callee)
	if j < 0 {
		return -1, fmt.Sprintf("unknown callee %q", callee)
	}
	sv := &topo.Services[i]
	at := -1
	for k, c := range sv.Calls {
		if c.Child != j {
			continue
		}
		if at >= 0 {
			return -1, "the fit has more than one call to the callee"
		}
		at = k
	}
	if at < 0 {
		return -1, "the fit has no direct call to the callee (it may reach it through the call structure)"
	}
	if sv.Parallel && !sv.Calls[at].Async && len(sv.SyncCalls()) > 1 {
		return -1, "the call is in a parallel fan-out, where a call probability is not modelled"
	}
	return at, ""
}

// MeasuredP is the probability per visit a measured set_p item gives the service's call to callee: the fit's
// probability times the record's ratio of calls per visit (candidate over baseline), as a point and as the range of
// the ratio +-2 standard errors, clipped to [0, 1]; or why it cannot (DirectCall, a callee the record does not count
// in the baseline, or a point above 1: more calls than visits, which a probability cannot carry).
func MeasuredP(topo *model.Topology, svc *abdiff.Service, callee string) (float64, Range, string) {
	i := topo.Index(svc.Service)
	if i < 0 {
		return 0, Range{}, "the record's service is not the fit's"
	}
	at, why := DirectCall(topo, i, callee)
	if why != "" {
		return 0, Range{}, why
	}
	for _, c := range svc.Calls {
		if c.Callee != callee {
			continue
		}
		ratio, se, ok := c.Ratio()
		if !ok {
			return 0, Range{}, "the record's baseline makes no call to the callee"
		}
		p := topo.Services[i].Calls[at].P
		point := p * ratio
		if point > 1 {
			return 0, Range{}, fmt.Sprintf("the measured probability %v is above 1", point)
		}
		r := Range{Lo: math.Max(0, p*(ratio-2*se)), Hi: math.Min(1, p*(ratio+2*se))}
		return point, r, ""
	}
	return 0, Range{}, "the record does not count calls to the callee"
}

// Callers are the services of the fit that call service j, in name order: a caller reached through the call
// structure's stage or shape nodes (stage:S:n, shape:S:n) is its service S. At most contract.MaxExamples.
func Callers(topo *model.Topology, j int) []string {
	seen := map[int]bool{}
	var walk func(int)
	var out []string
	walk = func(j int) {
		for i := range topo.Services {
			for _, c := range topo.Services[i].Calls {
				if c.Child != j || seen[i] {
					continue
				}
				seen[i] = true
				name := topo.Services[i].Name
				if strings.HasPrefix(name, "stage:") || strings.HasPrefix(name, "shape:") {
					walk(i)
				} else if !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	walk(j)
	sort.Strings(out)
	if len(out) > contract.MaxExamples {
		out = out[:contract.MaxExamples]
	}
	return out
}

// patchTargets are the services a patch can name, for the message that lists them: the topology's names without the
// zero-time nodes the inference adds for the call structure (stage:S:n and shape:S:n), which are not services.
func patchTargets(topo *model.Topology) []string {
	var out []string
	for _, n := range topo.Names() {
		if !strings.HasPrefix(n, "stage:") && !strings.HasPrefix(n, "shape:") {
			out = append(out, n)
		}
	}
	return out
}

func sortedKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Digest is the set's content id, the change_hash of report v2 and of the prediction view: contract.DigestOf of the set
// as read (before Resolve widens measured items), so that one request always has one hash.
func (s *Set) Digest() (string, error) {
	return contract.DigestOf(s)
}
