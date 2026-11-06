// SPDX-License-Identifier: Apache-2.0

package views

import (
	"math"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// The most rows of the prediction view (prediction.v1).
const (
	MaxPredictionEntries = 20
	MaxMoved             = 10
	MaxPreviewRules      = 8
)

// CallerAsserted is how a preview shows the label declared: a number the caller typed (ARCHITECTURE.md 4.3).
const CallerAsserted = "caller-asserted"

type predictionView struct {
	Schema          string              `json:"schema"`
	FitDigest       string              `json:"fit_digest"`
	ChangeHash      string              `json:"change_hash"`
	EvidenceClass   string              `json:"evidence_class"`
	Load            predLoad            `json:"load"`
	Entries         []predEntry         `json:"entries"`
	Moved           []predMoved         `json:"moved"`
	Labels          []predLabel         `json:"labels"`
	WeakestLabel    string              `json:"weakest_label,omitempty"`
	Preview         predPreview         `json:"preview"`
	Findings        []contract.Finding  `json:"findings"`
	FindingsOmitted int                 `json:"findings_omitted,omitempty"`
	Assumptions     []report.Assumption `json:"assumptions"`
	Truncated       bool                `json:"truncated"`
	Scope           string              `json:"scope"`
}

type predLoad struct {
	EvaluatedAt          string  `json:"evaluated_at"`
	ExtrapolatedLoad     bool    `json:"extrapolated_load"`
	MaxUtilizationBefore float64 `json:"max_utilization_before"`
	MaxUtilizationAfter  *rng    `json:"max_utilization_after,omitempty"`
	ValidatedCeiling     float64 `json:"validated_ceiling"`
	BeyondValidated      bool    `json:"beyond_validated"`
}

type predEntry struct {
	Name          string   `json:"name"`
	NameUntrusted bool     `json:"name_untrusted,omitempty"`
	BeforeMs      float64  `json:"before_ms"`
	AfterMs       *rng     `json:"after_ms,omitempty"`
	ChangeMs      *rng     `json:"change_ms,omitempty"`
	ChangePct     *rng     `json:"change_pct,omitempty"`
	ModelBandMs   float64  `json:"model_band_ms"`
	BandMs        *rng     `json:"band_ms,omitempty"`
	BlockBandMs   *rng     `json:"block_band_ms,omitempty"`
	ThresholdMs   *float64 `json:"threshold_ms"`
	size          float64
}

type predMoved struct {
	Service           string  `json:"service"`
	ServiceUntrusted  bool    `json:"service_untrusted,omitempty"`
	UtilizationBefore float64 `json:"utilization_before"`
	UtilizationAfter  *rng    `json:"utilization_after,omitempty"`
	ChangeMs          *rng    `json:"change_ms,omitempty"`
	rank              float64
}

type predLabel struct {
	Item              int    `json:"item"`
	Label             string `json:"label"`
	TransferValidated bool   `json:"transfer_validated,omitempty"`
}

type predPreview struct {
	Verdict    string     `json:"verdict"`
	Advisory   bool       `json:"advisory"`
	Rules      []predRule `json:"rules"`
	DecideWith []string   `json:"decide_with"`
	Sentence   string     `json:"sentence"`
}

type predRule struct {
	Rule      string   `json:"rule"`
	Entry     string   `json:"entry,omitempty"`
	Effect    string   `json:"effect"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Variant   string   `json:"variant,omitempty"`
}

// PreviewLabel is a label as a preview shows it: declared is caller-asserted.
func PreviewLabel(l string) string {
	if l == change.LabelDeclared {
		return CallerAsserted
	}
	return l
}

// PreviewRules are the rows a preview lists, at most MaxPreviewRules, in rule order: every evaluated row that blocks,
// holds or informs (the advisory mark of rule 13 excepted: preview.advisory says it), and rule 10's own outcome even
// when it passed, so the agent knows whether its estimate fits.
func PreviewRules(g *gate.Result) []gate.Rule {
	var out []gate.Rule
	for _, r := range g.Rules {
		if !r.Evaluated || r.Number == "13" {
			continue
		}
		if r.Effect != gate.Pass || r.Number == "10" {
			out = append(out, r)
		}
	}
	if len(out) > MaxPreviewRules {
		out = out[:MaxPreviewRules]
	}
	return out
}

// Prediction returns the prediction view (rhobound-mcp.prediction/v1) of a report v2 and the preview's evaluation of
// it: the entries with their change, band and threshold (and the block band under the policy's block_band "model"),
// the services the change moves, the labels, and the preview's verdict, always advisory, with its rules and
// decide_with. It carries no server key.
func Prediction(r *report.V2, g *gate.Result, detail string) ([]byte, error) {
	v := &predictionView{Schema: PredictionSchema, FitDigest: r.FitDigest, ChangeHash: r.ChangeHash,
		EvidenceClass: r.EvidenceClass, Entries: []predEntry{}, Moved: []predMoved{}, Labels: []predLabel{},
		WeakestLabel: PreviewLabel(r.WeakestLabel),
		Preview: predPreview{Verdict: g.Verdict, Advisory: true, Rules: []predRule{},
			DecideWith: append([]string{}, g.DecideWith...), Sentence: gate.PreviewSentence},
		Findings: findings(r.Findings, g.Findings), Assumptions: assumptions(r.Assumptions), Scope: Scope}
	v.Load = predLoad{EvaluatedAt: g.EvaluatedAt, ExtrapolatedLoad: g.Extrapolated,
		MaxUtilizationBefore: f4(r.Load.MaxUtilizationBefore), ValidatedCeiling: g.ValidatedCeiling,
		BeyondValidated: g.BeyondValidated}
	if u := r.Load.MaxUtilizationAfter; u != nil {
		v.Load.MaxUtilizationAfter = span(u.Lo, u.Hi)
	}
	reported := map[string]report.Entry{}
	for _, en := range r.Entries {
		reported[en.Name] = en
	}
	nm := namerOf(rowNames(r.Rows))
	for _, ev := range g.Entries {
		n, u := nm.name(ev.Name)
		en := reported[ev.Name]
		e := predEntry{Name: n, NameUntrusted: u, BeforeMs: f4(ev.BeforeMs), ModelBandMs: f4(ev.ModelBandMs),
			ThresholdMs: r4(ev.ThresholdMs), size: math.Inf(1)}
		if ev.ChangeMs != nil {
			e.ChangeMs = span(ev.ChangeMs.Lo, ev.ChangeMs.Hi)
			e.size = math.Max(math.Abs(ev.ChangeMs.Lo), math.Abs(ev.ChangeMs.Hi))
		}
		if ev.BandMs != nil {
			e.BandMs = span(ev.BandMs.Lo, ev.BandMs.Hi)
		}
		if ev.BlockBandMs != nil {
			e.BlockBandMs = span(ev.BlockBandMs.Lo, ev.BlockBandMs.Hi)
		}
		if en.AfterMs != nil {
			e.AfterMs = span(en.AfterMs.Lo, en.AfterMs.Hi)
		} else if ev.ChangeMs != nil {
			e.AfterMs = span(ev.BeforeMs+ev.ChangeMs.Lo, ev.BeforeMs+ev.ChangeMs.Hi)
		}
		if en.ChangePct != nil {
			e.ChangePct = span(en.ChangePct.Lo, en.ChangePct.Hi)
		} else if ev.ChangeMs != nil && ev.BeforeMs > 0 {
			e.ChangePct = span(100*ev.ChangeMs.Lo/ev.BeforeMs, 100*ev.ChangeMs.Hi/ev.BeforeMs)
		}
		v.Entries = append(v.Entries, e)
	}
	byKey(v.Entries, func(e predEntry) float64 { return e.size }, func(e predEntry) string { return e.Name })
	for _, row := range r.Rows {
		u := row.UtilizationAfter
		if !budget.Budgetable(row) || u == nil || (u.Lo == row.Util && u.Hi == row.Util) {
			continue
		}
		n, un := nm.name(row.Name)
		m := predMoved{Service: n, ServiceUntrusted: un, UtilizationBefore: f4(row.Util), UtilizationAfter: span(u.Lo, u.Hi),
			rank: u.Hi}
		if c := row.ChangeRangeMs; c != nil {
			m.ChangeMs = span(c.Lo, c.Hi)
		}
		if math.IsNaN(m.rank) {
			m.rank = math.Inf(1)
		}
		v.Moved = append(v.Moved, m)
	}
	byKey(v.Moved, func(m predMoved) float64 { return m.rank }, func(m predMoved) string { return m.Service })
	if len(v.Moved) > MaxMoved {
		v.Moved = v.Moved[:MaxMoved]
	}
	if len(v.Entries) > MaxPredictionEntries {
		v.Entries, v.Truncated = v.Entries[:MaxPredictionEntries], true
	}
	for _, l := range r.Labels {
		v.Labels = append(v.Labels, predLabel{Item: l.Item, Label: PreviewLabel(l.Label), TransferValidated: l.TransferValidated})
	}
	for _, rr := range PreviewRules(g) {
		v.Preview.Rules = append(v.Preview.Rules, predRule{Rule: rr.Rule, Entry: rr.Entry, Effect: rr.Effect,
			Value: round(rr.Value), Threshold: round(rr.Threshold), Variant: rr.Variant})
	}
	t := trim{findings: &v.Findings, assumptions: &v.Assumptions, omitted: &v.FindingsOmitted, truncated: &v.Truncated,
		size: func() (int, error) { return size(v) },
		dropRow: func() bool {
			switch {
			case len(v.Moved) > 0:
				v.Moved = v.Moved[:len(v.Moved)-1]
			case len(v.Entries) > 1:
				v.Entries = v.Entries[:len(v.Entries)-1]
			default:
				return false
			}
			return true
		},
		// The last stages, when the doubts are gone too: the entries' ranges a reader can recompute (after_ms is
		// before_ms plus change_ms, change_pct is change_ms over before_ms), the per-item labels (weakest_label
		// stays), the utilization after, then the remaining entries, last first. The verdict's rules still name the
		// entry, its value and its threshold.
		last: []func() bool{
			func() bool {
				for i := range v.Entries {
					v.Entries[i].AfterMs, v.Entries[i].ChangePct = nil, nil
				}
				v.Truncated = true
				return false
			},
			func() bool {
				v.Labels, v.Truncated = []predLabel{}, true
				return false
			},
			func() bool {
				v.Load.MaxUtilizationAfter, v.Truncated = nil, true
				return false
			},
			func() bool {
				if len(v.Entries) == 0 {
					return false
				}
				v.Entries, v.Truncated = v.Entries[:len(v.Entries)-1], true
				return true
			},
		}}
	if err := t.run(MaxBytes(detail)); err != nil {
		return nil, err
	}
	return contract.Canonical(v)
}

func round(p *float64) *float64 {
	if p == nil {
		return nil
	}
	return r4(*p)
}
