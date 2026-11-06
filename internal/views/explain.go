// SPDX-License-Identifier: Apache-2.0

package views

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// The sections of explain (explain.v1 input).
const (
	SectionSummary      = "summary"
	SectionServices     = "services"
	SectionFindings     = "findings"
	SectionAssumptions  = "assumptions"
	SectionRules        = "rules"
	SectionJudgePacket  = "judge_packet"
	SectionDataContract = "data_contract"
	SectionScope        = "scope"
)

// Sections lists the sections, in the order of the schema's enum.
var Sections = []string{SectionSummary, SectionServices, SectionFindings, SectionAssumptions, SectionRules,
	SectionJudgePacket, SectionDataContract, SectionScope}

// The bounds of explain's max_chars (explain.v1 input).
const (
	MinExplainChars     = 200
	MaxExplainChars     = 8000
	DefaultExplainChars = 2000
)

// NotModelled is what the model leaves out, in the scope section (ARCHITECTURE.md 1).
var NotModelled = []string{"tail latency (p95, p99)", "bursts", "retries", "timeouts", "circuit breakers",
	"autoscaling", "CPU throttling", "cold starts", "load at or above the utilization limit"}

// Target is the document an explain is about. At most one of Fit, Report and AB is set; none for the data_contract
// and scope sections. Gate and Packet are the preview's evaluation of Report and its judge packet, when the command
// was given a policy.
type Target struct {
	Fit       *fit.Record
	FitDigest string
	Report    *report.V2
	Gate      *gate.Result
	Packet    []byte
	AB        *abdiff.Record
}

// ErrSection is a section the target cannot give: rules and judge_packet need a prediction and a policy, assumptions a
// prediction, and every section but data_contract and scope a target.
var ErrSection = errors.New("explain: this section is not available for this target")

type explainView struct {
	Schema    string          `json:"schema"`
	Section   string          `json:"section"`
	Content   json.RawMessage `json:"content"`
	Truncated bool            `json:"truncated"`
}

// Explain returns the explain view (rhobound-mcp.explain/v1) of one section of a target: its content, sanitized like
// every other view, cut to maxChars characters of canonical JSON (the longest lists lose their last items first), with
// truncated saying so.
func Explain(t Target, section string, maxChars int) ([]byte, error) {
	if maxChars < MinExplainChars || maxChars > MaxExplainChars {
		return nil, fmt.Errorf("explain: max-chars must be in [%d, %d]", MinExplainChars, MaxExplainChars)
	}
	content, err := Content(t, section)
	if err != nil {
		return nil, err
	}
	raw, err := contract.Canonical(content)
	if err != nil {
		return nil, err
	}
	limit := maxChars
	if room := FullMaxBytes - 120; limit > room {
		limit = room
	}
	raw, truncated, err := cut(raw, limit)
	if err != nil {
		return nil, err
	}
	return contract.Canonical(explainView{Schema: ExplainSchema, Section: section, Content: raw, Truncated: truncated})
}

// Content returns a section's content.
func Content(t Target, section string) (any, error) {
	switch section {
	case SectionDataContract:
		return dataContract(), nil
	case SectionScope:
		return map[string]any{"scope": Scope, "text": report.Scope, "pass_means": gate.PassMeans,
			"not_modelled": NotModelled}, nil
	}
	switch {
	case t.Report != nil:
		return reportSection(t, section)
	case t.Fit != nil:
		return fitSection(t, section)
	case t.AB != nil:
		return abSection(t.AB, section)
	}
	return nil, ErrSection
}

func dataContract() any {
	type row struct {
		Code    string `json:"code"`
		Level   string `json:"level"`
		Trigger string `json:"trigger"`
		Fix     string `json:"fix"`
		Status  string `json:"status,omitempty"`
	}
	rows := []row{}
	for _, c := range contract.All() {
		rows = append(rows, row{Code: c.ID, Level: string(c.Level), Trigger: string(c.Trigger), Fix: c.Fix, Status: c.Status})
	}
	return map[string]any{"contract": contract.Version, "codes": rows}
}

func reportSection(t Target, section string) (any, error) {
	r := t.Report
	nm := namerOf(rowNames(r.Rows))
	switch section {
	case SectionSummary:
		entries := []map[string]any{}
		for _, en := range r.Entries {
			n, u := nm.name(en.Name)
			e := map[string]any{"name": n, "before_ms": f4(en.BeforeMs)}
			if u {
				e["name_untrusted"] = true
			}
			if en.ChangeMs != nil {
				e["change_ms"] = span(en.ChangeMs.Lo, en.ChangeMs.Hi)
			}
			entries = append(entries, e)
		}
		s := map[string]any{"version": r.Version, "fit_digest": r.FitDigest, "change_hash": r.ChangeHash,
			"evidence_class": r.EvidenceClass, "entries": entries, "monotonicity": r.Monotonicity,
			"load": map[string]any{"evaluated_at": r.Load.EvaluatedAt, "load_scale": f4(r.Load.LoadScale),
				"max_utilization_before": f4(r.Load.MaxUtilizationBefore)}}
		if r.WeakestLabel != "" {
			s["weakest_label"] = r.WeakestLabel
		}
		if t.Gate != nil {
			s["verdict"], s["advisory"] = t.Gate.Verdict, t.Gate.Advisory
		}
		return s, nil
	case SectionServices:
		out := []map[string]any{}
		for _, row := range r.Rows {
			if row.Stage {
				continue
			}
			n, u := nm.name(row.Name)
			workers := fmt.Sprint(row.WorkersPerPod)
			if row.WorkersLower {
				workers += "+"
			}
			s := map[string]any{"name": n, "utilization": f4(row.Util), "own_ms": f4(row.OwnMs), "workers": workers,
				"model_ms": f4(row.ModelMs)}
			if u {
				s["name_untrusted"] = true
			}
			if row.External {
				s["external"] = true
			}
			if a := row.UtilizationAfter; a != nil {
				s["utilization_after"] = span(a.Lo, a.Hi)
			}
			if c := row.ChangeRangeMs; c != nil {
				s["change_ms"] = span(c.Lo, c.Hi)
			}
			out = append(out, s)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i]["utilization"].(float64) > out[j]["utilization"].(float64) })
		return map[string]any{"services": out}, nil
	case SectionFindings:
		var gf []contract.Finding
		if t.Gate != nil {
			gf = t.Gate.Findings
		}
		return map[string]any{"findings": findings(r.Findings, gf)}, nil
	case SectionAssumptions:
		return map[string]any{"assumptions": assumptions(r.Assumptions), "monotonicity": r.Monotonicity}, nil
	case SectionRules:
		if t.Gate == nil {
			return nil, ErrSection
		}
		return map[string]any{"verdict": t.Gate.Verdict, "advisory": t.Gate.Advisory, "rules": t.Gate.Rules,
			"decide_with": t.Gate.DecideWith}, nil
	case SectionJudgePacket:
		if t.Packet == nil {
			return nil, ErrSection
		}
		return json.RawMessage(t.Packet), nil
	}
	return nil, ErrSection
}

func fitSection(t Target, section string) (any, error) {
	f := t.Fit
	names := make([]string, len(f.Services))
	for i, s := range f.Services {
		names[i] = s.Name
	}
	nm := namerOf(names)
	switch section {
	case SectionSummary:
		workers := map[string]int{}
		for k, v := range f.Options.Workers {
			workers[nm.safe(k)] = v
		}
		levels := map[string]int{}
		for _, x := range f.Findings {
			levels[string(x.Level)]++
		}
		// the capture's last element only: a fit made from a folder records the folder as given, and a view never
		// shows a path of the machine that made it; a capture that holds an AWS resource name is shown by the name
		// rule of the whole string, since its last path element may hold the account (ARCHITECTURE.md 5.9)
		capture, _ := name(filepath.Base(f.Capture))
		if contract.NamesARN(f.Capture) {
			capture, _ = name(f.Capture)
		}
		return map[string]any{"schema": f.Schema, "fit_digest": t.FitDigest, "capture": capture, "window": f.Window,
			"kept_share": f.KeptShare, "manifest_digest": f.ManifestDigest, "tool": f.Tool,
			"options": map[string]any{"workers": workers, "pod_identity": f.Options.PodIdentity,
				"estimator": f.Options.Estimator, "plain": f.Options.Plain},
			"services": len(f.Services), "findings": levels}, nil
	case SectionServices:
		out := []map[string]any{}
		for _, s := range f.Services {
			n, u := nm.name(s.Name)
			m := map[string]any{"name": n, "entry": s.Entry, "external": s.External, "workers": s.Workers,
				"workers_lower_bound": s.WorkersLowerBound, "client_spans": s.ClientSpans}
			if u {
				m["name_untrusted"] = true
			}
			out = append(out, m)
		}
		return map[string]any{"services": out}, nil
	case SectionFindings:
		return map[string]any{"findings": findings(f.Findings)}, nil
	}
	return nil, ErrSection
}

func abSection(r *abdiff.Record, section string) (any, error) {
	names := make([]string, len(r.Services))
	for i, s := range r.Services {
		names[i] = s.Service
	}
	nm := namerOf(names)
	switch section {
	case SectionSummary, SectionServices:
		out := []map[string]any{}
		for _, s := range r.Services {
			n, u := nm.name(s.Service)
			m := map[string]any{"service": n, "own_ms": map[string]any{"delta": f4(s.OwnMs.Delta), "se": f4(s.OwnMs.SE)},
				"own_cs2":       map[string]any{"delta": f4(s.OwnCS2.Delta), "se": f4(s.OwnCS2.SE)},
				"calls_changed": s.CallsChanged, "variance_changed": s.VarianceChanged}
			if u {
				m["service_untrusted"] = true
			}
			if section == SectionServices {
				m["visits"] = s.Visits
				m["glue_ms"] = map[string]any{"delta": f4(s.GlueMs.Delta), "se": f4(s.GlueMs.SE)}
				m["error_share"] = s.ErrorShare
			}
			out = append(out, m)
		}
		if section == SectionServices {
			return map[string]any{"services": out}, nil
		}
		return map[string]any{"schema": r.Schema, "overlap_share": f4(r.OverlapShare), "se_method": r.SEMethod,
			"self_report_consistent": r.SelfReportConsistent, "services": out}, nil
	case SectionFindings:
		return map[string]any{"findings": findings(r.Findings)}, nil
	}
	return nil, ErrSection
}

// cut shortens a JSON document to at most max bytes by dropping the last item of its longest list, again and again;
// a document with no list left that is still too long becomes {}.
func cut(raw []byte, max int) ([]byte, bool, error) {
	if len(raw) <= max {
		return raw, false, nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, false, err
	}
	for {
		b, err := contract.Canonical(doc)
		if err != nil {
			return nil, false, err
		}
		if len(b) <= max {
			return b, true, nil
		}
		if !dropLongest(doc) {
			return []byte("{}"), true, nil
		}
	}
}

// dropLongest removes the last item of the longest non-empty list in a decoded JSON document.
func dropLongest(doc any) bool {
	var best *[]any
	var walk func(v any, set func([]any))
	var bestSet func([]any)
	walk = func(v any, set func([]any)) {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				k := k
				walk(x[k], func(a []any) { x[k] = a })
			}
		case []any:
			if len(x) > 0 && (best == nil || len(x) > len(*best)) && set != nil {
				xs := x
				best, bestSet = &xs, set
			}
			for i := range x {
				i := i
				walk(x[i], func(a []any) { x[i] = a })
			}
		}
	}
	walk(doc, nil)
	if best == nil {
		return false
	}
	bestSet((*best)[:len(*best)-1])
	return true
}
