// SPDX-License-Identifier: Apache-2.0

// Package views builds what an agent reads (docs/mcp/ARCHITECTURE.md 5.5): the context, budget, prediction and explain
// views and the judge packet, each the canonical JSON a CLI twin prints (`rhobound context --json`, ...) and the MCP
// server returns as structuredContent after adding only its own keys (prediction_id, fit_id, fit_age_s, attempt,
// max_preview_attempts, escalate, links), which no view here ever holds.
//
// Every view carries fit_digest (the explain view excepted, which holds a section of a document), every string that
// came from a trace passes the sanitizing rule (package sanitize), and every view fits its size: a summary at most
// SummaryMaxBytes, a full view at most FullMaxBytes, leaving the server room for its keys. What a summary drops to fit,
// in this order (ARCHITECTURE.md 5.9): assumptions that are checked-ok; the examples and numbers of findings, and the
// texts (how) of the assumptions, each still listed by id and status; notes, counted in findings_omitted; repeats of
// one code, which keeps one finding with its code, level and fix and no subject, the others counted; then the
// lowest-ranked rows; then doubts, counted, with truncated true; then a view's own last stages (the prediction view's
// derived ranges, its per-item labels, its utilization after, then its last rows). Never dropped: the verdict and its
// rules, decide_with, every refusal, every assumption that is not checked-ok, and scope. A view still over its cap
// after every stage is an error (*TooLargeError), never an oversize view.
package views

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/sanitize"
)

// The schema ids of the views.
const (
	ContextSchema    = "rhobound-mcp.context/v1"
	BudgetSchema     = "rhobound-mcp.budget/v1"
	PredictionSchema = "rhobound-mcp.prediction/v1"
	ExplainSchema    = "rhobound-mcp.explain/v1"
	PacketSchema     = "rhobound-mcp.judge-packet/v1"
	AnswerSchema     = "rhobound-mcp.judge-answer/v1"
)

// Scope is the short scope code every view carries (ARCHITECTURE.md 5.9).
const Scope = "mean-only/v1"

// The details of a view.
const (
	DetailSummary = "summary"
	DetailFull    = "full"
)

// The sizes of a view's canonical JSON: the server's caps (2 KB for a summary, 8 KB for a full result) less the room
// it keeps for its own keys.
const (
	ServerKeyReserve = 256
	SummaryMaxBytes  = 2048 - ServerKeyReserve
	FullMaxBytes     = 8192 - ServerKeyReserve
	// PacketMaxBytes bounds the judge packet: about 4,000 tokens.
	PacketMaxBytes = 16000
)

// MaxBytes is the size cap of a detail.
func MaxBytes(detail string) int {
	if detail == DetailFull {
		return FullMaxBytes
	}
	return SummaryMaxBytes
}

// name is a node name as a view prints it, and whether it was replaced.
func name(s string) (string, bool) {
	return sanitize.Service(s)
}

func safe(s string) string {
	n, _ := name(s)
	return n
}

// namer shows the node names of one fit as all of its views do (sanitize.Services): one to one, so that a caller can
// name each node by what a view shows. A name it was not made with goes through the name rule alone.
type namer map[string]sanitize.Shown

// namerOf is the namer of the nodes with these names.
func namerOf(names []string) namer { return namer(sanitize.Services(names)) }

// rowNames are the names of a report's rows: every node of its fit.
func rowNames(rows []report.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Name
	}
	return out
}

func (nm namer) name(s string) (string, bool) {
	if sh, ok := nm[s]; ok {
		return sh.Name, sh.Untrusted
	}
	return name(s)
}

func (nm namer) safe(s string) string {
	n, _ := nm.name(s)
	return n
}

// r4 rounds to 4 decimals, the precision views print ms, utilizations and leverage at; it returns nil for a value
// JSON cannot hold.
func r4(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	x := math.Round(v*1e4) / 1e4
	if x == 0 {
		x = 0 // no -0
	}
	return &x
}

// f4 is r4 for a value known to be finite (0 otherwise).
func f4(v float64) float64 {
	if p := r4(v); p != nil {
		return *p
	}
	return 0
}

type rng struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// span rounds a range for a view; nil stays nil, and so does a range with a non-finite end.
func span(lo, hi float64) *rng {
	a, b := r4(lo), r4(hi)
	if a == nil || b == nil {
		return nil
	}
	return &rng{Lo: *a, Hi: *b}
}

// findings returns findings as a view prints them: subjects through sanitize.Subject (none for a path from the root)
// and examples through the sanitizing rule (a peer address in trace.peer_renamed's examples becomes external_<hash8>),
// non-finite numbers dropped, fixes cut to 300 characters, sorted, without repeats (a repeat has the code, the subject
// and subject_untrusted of one before it). An example that starts with / is left out, but trace.probe_like_traffic's,
// which are routes (rooted): a path from the root names a folder of the machine that made the record, which the views
// never show (ARCHITECTURE.md 5.9), and the subjects of this tool's findings are names. A subject is the name rule's
// alone, as the finding was made: an ARN service shown as svc_<hash8> because its last element is another service's
// name (sanitize.Services) is the subject of its findings by that element, untrusted.
func findings(lists ...[]contract.Finding) []contract.Finding {
	out := []contract.Finding{}
	seen := map[string]bool{}
	for _, fs := range lists {
		for _, f := range fs {
			g := f
			if g.Subject != "" {
				s, u := sanitize.Subject(g.Subject)
				g.Subject, g.SubjectUntrusted = s, s != "" && (g.SubjectUntrusted || u)
			}
			if len(g.Examples) > 0 {
				ex := make([]string, 0, len(g.Examples))
				for i, e := range g.Examples {
					if i == contract.MaxExamples {
						break
					}
					if strings.HasPrefix(e, "/") && g.Code != contract.TraceProbeLikeTraffic {
						continue
					}
					if g.Code == contract.TracePeerRenamed {
						ex = append(ex, sanitize.External(e))
					} else {
						ex = append(ex, safe(e))
					}
				}
				g.Examples = ex
				if len(ex) == 0 {
					g.Examples = nil
				}
			}
			if len(g.Numbers) > 0 {
				m := map[string]float64{}
				for k, v := range g.Numbers {
					if !math.IsNaN(v) && !math.IsInf(v, 0) {
						m[k] = v
					}
				}
				g.Numbers = m
				if len(m) == 0 {
					g.Numbers = nil
				}
			}
			if len(g.Fix) > 300 {
				g.Fix = g.Fix[:300]
			}
			key := fmt.Sprintf("%s\x00%s\x00%t", g.Code, g.Subject, g.SubjectUntrusted)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, g)
		}
	}
	contract.SortFindings(out)
	return out
}

// assumptions returns a copy of the list, never nil.
func assumptions(as []report.Assumption) []report.Assumption {
	return append([]report.Assumption{}, as...)
}

// TooLargeError is a view whose part that is never dropped is, alone, over its cap: the view is not returned. The CLI
// reports it as a tool error, and the MCP server, which never returns an oversize result, as an internal error.
type TooLargeError struct{ Size, Max int }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("the view is %d bytes after every drop of the drop order, over its cap of %d", e.Size, e.Max)
}

// trim is the shared part of the drop order. A view gives it pointers to its findings, its assumptions, its
// findings_omitted and truncated, a function that drops its lowest-ranked row (false when none is left), its own last
// stages (run after the doubts; each drops one thing and reports whether to run it again), and size, which returns
// the view's canonical size; trim drops until the size is at most max, and fails with *TooLargeError when nothing is
// left to drop.
type trim struct {
	findings    *[]contract.Finding
	assumptions *[]report.Assumption
	omitted     *int
	truncated   *bool
	dropRow     func() bool
	last        []func() bool
	size        func() (int, error)
}

func (t trim) run(max int) error {
	fits := func() (bool, error) {
		n, err := t.size()
		return n <= max, err
	}
	stages := []func() bool{
		func() bool { // checked-ok assumptions
			if t.assumptions == nil {
				return false
			}
			out := (*t.assumptions)[:0:0]
			for _, a := range *t.assumptions {
				if a.Status != report.CheckedOK {
					out = append(out, a)
				}
			}
			*t.assumptions = out
			return false
		},
		func() bool { // the examples and numbers of findings
			for i := range *t.findings {
				(*t.findings)[i].Examples, (*t.findings)[i].Numbers = nil, nil
			}
			return false
		},
		func() bool { // the texts of the assumptions: each is still listed, by id and status
			if t.assumptions != nil {
				for i := range *t.assumptions {
					(*t.assumptions)[i].How = ""
				}
			}
			return false
		},
		func() bool { // notes, counted
			out := (*t.findings)[:0:0]
			for _, f := range *t.findings {
				if f.Level == contract.Note {
					*t.omitted++
					continue
				}
				out = append(out, f)
			}
			*t.findings = out
			return false
		},
		func() bool { // repeats: one finding per code keeps its code, level and fix, without a subject; the others counted
			out := (*t.findings)[:0:0]
			first := map[string]int{}
			for _, f := range *t.findings {
				if i, seen := first[f.Code]; seen {
					out[i].Subject, out[i].SubjectUntrusted = "", false
					*t.omitted++
					continue
				}
				first[f.Code] = len(out)
				out = append(out, f)
			}
			*t.findings = out
			return false
		},
		func() bool { // the lowest-ranked rows, one at a time
			if t.dropRow != nil && t.dropRow() {
				*t.truncated = true
				return true
			}
			return false
		},
		func() bool { // doubts, from the last, counted
			fs := *t.findings
			for i := len(fs) - 1; i >= 0; i-- {
				if fs[i].Level == contract.Doubt {
					*t.findings = append(fs[:i:i], fs[i+1:]...)
					*t.omitted++
					*t.truncated = true
					return true
				}
			}
			return false
		},
	}
	for _, stage := range append(stages, t.last...) {
		for {
			ok, err := fits()
			if err != nil || ok {
				return err
			}
			if !stage() {
				break
			}
		}
	}
	n, err := t.size()
	if err != nil {
		return err
	}
	if n > max {
		return &TooLargeError{Size: n, Max: max}
	}
	return nil
}

// byKey sorts by a float key, highest first, then by name.
func byKey[T any](xs []T, key func(T) float64, nameOf func(T) string) {
	sort.SliceStable(xs, func(i, j int) bool {
		a, b := key(xs[i]), key(xs[j])
		if a != b {
			return a > b
		}
		return nameOf(xs[i]) < nameOf(xs[j])
	})
}

func size(v any) (int, error) {
	b, err := contract.Canonical(v)
	return len(b), err
}
