// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// PolicySchema is the schema id of a policy.
const PolicySchema = "rhobound.policy/v1"

// Policy is a gate policy (rhobound.policy/v1; docs/mcp/ARCHITECTURE.md 6.2) with its defaults applied: the effective
// policy, which gate.json records with its digest. There is no default entry threshold: at least one of
// MaxEntryMeanChangePct and MaxEntryMeanChangeMs is set (policy.no_threshold otherwise).
type Policy struct {
	Schema string `json:"schema"`
	// MaxEntryMeanChangePct is a threshold in percent of an entry's model mean before the change, nil when not set.
	MaxEntryMeanChangePct *float64 `json:"max_entry_mean_change_pct"`
	// MaxEntryMeanChangeMs is a threshold in entry milliseconds, nil when not set.
	MaxEntryMeanChangeMs *float64 `json:"max_entry_mean_change_ms"`
	// MaxUtilizationAfter: a service raised above it by the change blocks (rule 9).
	MaxUtilizationAfter float64 `json:"max_utilization_after"`
	// MaxUtilizationValidated is the highest load the public record supports (ARCHITECTURE.md 4.6): above it,
	// load.beyond_validated (rule 6).
	MaxUtilizationValidated float64 `json:"max_utilization_validated"`
	// ModelBand is the model's own error added to every change range; nil when the policy removed it, which only an
	// advisory policy may do (policy.model_band_removed).
	ModelBand          *Band       `json:"model_band"`
	MisfitMaxErrorPct  float64     `json:"misfit_max_error_pct"`
	MaxFitAgeH         float64     `json:"max_fit_age_h"`
	EvaluateAt         string      `json:"evaluate_at"`
	TrustedLabels      []string    `json:"trusted_labels"`
	DoubtsThatHold     []string    `json:"doubts_that_hold"`
	MaxPreviewAttempts int         `json:"max_preview_attempts"`
	Enforce            bool        `json:"enforce"`
	Exceptions         []Exception `json:"exceptions"`
	// BlockBand is the band rule 10's block test reads (ARCHITECTURE.md 6.4): "" for the full band, the default
	// ("full" in the file reads as ""), or BlockBandModel, which needs a model band (ModelBand not nil). It is the one
	// key of the effective policy omitted at its default, so a policy without it keeps the effective policy and the
	// digest it had before the key existed.
	BlockBand string `json:"block_band,omitempty"`

	// Unknown are the keys of the file this version does not know, sorted: ignored, and listed in gate.json.
	Unknown []string `json:"-"`
}

// Band is a policy's model band: max(ChangeFrac x |change|, LatencyFrac x the entry's mean before the change).
type Band struct {
	ChangeFrac  float64 `json:"change_frac"`
	LatencyFrac float64 `json:"latency_frac"`
	Source      string  `json:"source"`
}

// ModelBand returns the band as report.ModelBand, whose Ms is the one band formula (the worst case and the label are
// the defaults': a policy sets the fractions only). It returns the zero band, which adds nothing, for nil.
func (b *Band) ModelBand() report.ModelBand {
	if b == nil {
		return report.ModelBand{}
	}
	mb := report.DefaultModelBand()
	mb.ChangeFrac, mb.LatencyFrac, mb.Source = b.ChangeFrac, b.LatencyFrac, b.Source
	return mb
}

// Exception relaxes one rule, with a recorded reason (ARCHITECTURE.md 6.2): RuleUnmeasuredDelta for the items of one
// label, or RuleThresholdUnderFloor. No other rule can be relaxed.
type Exception struct {
	Rule   string `json:"rule"`
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason"`
}

// The policy defaults (ARCHITECTURE.md 6.2).
const (
	DefaultMaxUtilizationAfter     = 0.80
	DefaultMaxUtilizationValidated = 0.58
	DefaultMisfitMaxErrorPct       = 10
	DefaultMaxFitAgeH              = 24
	DefaultMaxPreviewAttempts      = 5
)

// The values of the policy key block_band: the full band (the default) or the model band alone on the block side.
const (
	BlockBandFull  = "full"
	BlockBandModel = "model"
)

// The values of Policy.EvaluateAt.
const (
	EvaluateBusiest   = "busiest_window_7d"
	EvaluateFitWindow = "fit_window"
)

// DefaultDoubtsThatHold are the doubt codes that hold a verdict at inconclusive by default (rule 5).
var DefaultDoubtsThatHold = []string{contract.TraceSamplingUnverified, contract.FitQueueRebuildSampled,
	contract.TraceClientSpansMissing, contract.TraceContextNotPropagated, contract.TraceErrorsPresent,
	contract.TraceRetries, contract.TraceShedding, contract.TraceArrivalsUnjudged, contract.TraceArrivalsOverdispersed,
	contract.TraceLoadTrend, contract.ModelBatchArrivals, contract.ProfileVarianceChanged,
	contract.ProfileStructureChanged, contract.ProfileSelfReportMismatch, contract.ProfileRouteHeterogeneous,
	contract.ABPayloadChanged}

// DefaultTrustedLabels are the labels that pass rule 11 by default.
var DefaultTrustedLabels = []string{change.LabelMeasuredAB, change.LabelMeasuredPaired}

// defaults returns a policy holding every default and no threshold.
func defaults() *Policy {
	return &Policy{Schema: PolicySchema, MaxUtilizationAfter: DefaultMaxUtilizationAfter,
		MaxUtilizationValidated: DefaultMaxUtilizationValidated,
		ModelBand: &Band{ChangeFrac: report.DefaultChangeFrac, LatencyFrac: report.DefaultLatencyFrac,
			Source: report.ModelBandSource},
		MisfitMaxErrorPct: DefaultMisfitMaxErrorPct, MaxFitAgeH: DefaultMaxFitAgeH, EvaluateAt: EvaluateBusiest,
		TrustedLabels:  append([]string{}, DefaultTrustedLabels...),
		DoubtsThatHold: append([]string{}, DefaultDoubtsThatHold...), MaxPreviewAttempts: DefaultMaxPreviewAttempts,
		Exceptions: []Exception{}}
}

// Error is a refused policy (or, from the readiness reader, a refused readiness record): the finding with its code,
// and a message for the text form. A command that evaluates the gate exits 2 on it; a view command refuses with it.
type Error struct {
	Finding contract.Finding
	Msg     string
}

func (e *Error) Error() string { return e.Msg }

// Refusal returns the coded refusal the error is (defs.v1 Refusal), for a command's --json output.
func (e *Error) Refusal() report.Refusal {
	r := report.Refusal{Code: e.Finding.Code, Reason: e.Msg, Fix: e.Finding.Fix, Subject: e.Finding.Subject,
		Numbers: e.Finding.Numbers}
	if len(r.Reason) > report.MaxReasonChars {
		r.Reason = r.Reason[:report.MaxReasonChars]
	}
	return r
}

// ParsePolicy reads a policy and applies the defaults. fromAgent is true for a policy an agent supplied inline, which
// is always refused (policy.inline_refused): policies are named in configuration, never written by the caller.
//
// The findings are the refusals: policy.bad_value for a known key with a bad value (an unregistered code in
// doubts_that_hold included, and block_band "model" without a model band), policy.no_threshold when neither threshold
// is set, policy.model_band_removed for a policy that enforces without the model band. The error is the first of
// them, an *Error. Unknown keys are not an error: they are listed in Policy.Unknown.
func ParsePolicy(b []byte, fromAgent bool) (*Policy, []contract.Finding, error) {
	if fromAgent {
		f := contract.NewFinding(contract.PolicyInlineRefused)
		return nil, []contract.Finding{f}, &Error{Finding: f,
			Msg: "policy: an inline policy is refused; name a policy from the configuration"}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw == nil {
		f := contract.NewFinding(contract.PolicyBadValue)
		return nil, []contract.Finding{f}, &Error{Finding: f, Msg: "policy: not a JSON object"}
	}
	p := defaults()
	var errs []*Error
	bad := func(key, why string) {
		f := contract.NewFinding(contract.PolicyBadValue).WithSubject(key)
		errs = append(errs, &Error{Finding: f, Msg: fmt.Sprintf("policy: %s: %s", key, why)})
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		v := raw[key]
		switch key {
		case "schema":
			var s string
			if json.Unmarshal(v, &s) != nil || s != PolicySchema {
				bad(key, "must be "+PolicySchema)
			}
		case "max_entry_mean_change_pct", "max_entry_mean_change_ms":
			x, null, ok := number(v)
			switch {
			case null:
			case !ok || !(x > 0):
				bad(key, "must be a number above 0, or null")
			case key == "max_entry_mean_change_pct":
				p.MaxEntryMeanChangePct = &x
			default:
				p.MaxEntryMeanChangeMs = &x
			}
		case "max_utilization_after", "max_utilization_validated":
			x, _, ok := number(v)
			if !ok || !(x > 0 && x < 1) {
				bad(key, "must be a number in (0, 1)")
			} else if key == "max_utilization_after" {
				p.MaxUtilizationAfter = x
			} else {
				p.MaxUtilizationValidated = x
			}
		case "misfit_max_error_pct", "max_fit_age_h":
			x, _, ok := number(v)
			if !ok || !(x > 0) {
				bad(key, "must be a number above 0")
			} else if key == "misfit_max_error_pct" {
				p.MisfitMaxErrorPct = x
			} else {
				p.MaxFitAgeH = x
			}
		case "model_band":
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				p.ModelBand = nil
				continue
			}
			var mb struct {
				ChangeFrac  *float64 `json:"change_frac"`
				LatencyFrac *float64 `json:"latency_frac"`
				Source      *string  `json:"source"`
			}
			if err := strict(v, &mb); err != nil || mb.ChangeFrac == nil || mb.LatencyFrac == nil || mb.Source == nil ||
				!(*mb.ChangeFrac >= 0) || !(*mb.LatencyFrac >= 0) || *mb.Source == "" {
				bad(key, "must be {change_frac >= 0, latency_frac >= 0, source}, or null")
			} else {
				p.ModelBand = &Band{ChangeFrac: *mb.ChangeFrac, LatencyFrac: *mb.LatencyFrac, Source: *mb.Source}
			}
		case "block_band":
			var s string
			if json.Unmarshal(v, &s) != nil || (s != BlockBandFull && s != BlockBandModel) {
				bad(key, "must be "+BlockBandFull+" or "+BlockBandModel)
			} else if s == BlockBandModel {
				p.BlockBand = s
			}
		case "evaluate_at":
			var s string
			if json.Unmarshal(v, &s) != nil || (s != EvaluateBusiest && s != EvaluateFitWindow) {
				bad(key, "must be "+EvaluateBusiest+" or "+EvaluateFitWindow)
			} else {
				p.EvaluateAt = s
			}
		case "trusted_labels":
			ls, ok := stringList(v)
			if !ok || !unique(ls) || !allIn(ls, change.Labels) {
				bad(key, "must be a list of distinct labels ("+strings.Join(change.Labels, ", ")+")")
			} else {
				p.TrustedLabels = ls
			}
		case "doubts_that_hold":
			cs, ok := stringList(v)
			if !ok || !unique(cs) {
				bad(key, "must be a list of distinct registered codes")
				continue
			}
			okAll := true
			for _, c := range cs {
				if _, known := contract.Lookup(c); !known {
					bad(key, fmt.Sprintf("%q is not a registered code", c))
					okAll = false
				}
			}
			if okAll {
				p.DoubtsThatHold = cs
			}
		case "max_preview_attempts":
			x, _, ok := number(v)
			if !ok || !(x >= 1) || x != float64(int(x)) || x > 1e6 {
				bad(key, "must be an integer of at least 1")
			} else {
				p.MaxPreviewAttempts = int(x)
			}
		case "enforce":
			var e bool
			if json.Unmarshal(v, &e) != nil {
				bad(key, "must be true or false")
			} else {
				p.Enforce = e
			}
		case "exceptions":
			ex, why := exceptions(v)
			if why != "" {
				bad(key, why)
			} else {
				p.Exceptions = ex
			}
		default:
			p.Unknown = append(p.Unknown, key)
		}
	}
	if _, ok := raw["schema"]; !ok {
		bad("schema", "is missing; it must be "+PolicySchema)
	}
	if len(errs) == 0 && p.MaxEntryMeanChangePct == nil && p.MaxEntryMeanChangeMs == nil {
		f := contract.NewFinding(contract.PolicyNoThreshold)
		errs = append(errs, &Error{Finding: f,
			Msg: "policy: no threshold: set max_entry_mean_change_pct or max_entry_mean_change_ms"})
	}
	if len(errs) == 0 && p.Enforce && p.ModelBand == nil {
		f := contract.NewFinding(contract.PolicyModelBandRemoved)
		errs = append(errs, &Error{Finding: f, Msg: "policy: model_band cannot be removed when enforce is true"})
	}
	if len(errs) == 0 && p.BlockBand == BlockBandModel && p.ModelBand == nil {
		// the block test would read the bare change range, with no model error at all
		bad("block_band", "model reads the model band; model_band cannot be null")
	}
	if len(errs) > 0 {
		fs := make([]contract.Finding, len(errs))
		for i, e := range errs {
			fs[i] = e.Finding
		}
		return nil, fs, errs[0]
	}
	if p.Unknown == nil {
		p.Unknown = []string{}
	}
	return p, nil, nil
}

// Digest is the effective policy's content id, recorded in gate.json.
func (p *Policy) Digest() (string, error) {
	return contract.DigestOf(p)
}

// Threshold returns an entry's threshold in ms: the smaller of max_entry_mean_change_ms and max_entry_mean_change_pct
// of the entry's mean before the change, and which of the two keys set it.
func (p *Policy) Threshold(beforeMs float64) (ms float64, key string) {
	ms, key = 0, ""
	if p.MaxEntryMeanChangeMs != nil {
		ms, key = *p.MaxEntryMeanChangeMs, "max_entry_mean_change_ms"
	}
	if p.MaxEntryMeanChangePct != nil {
		if t := *p.MaxEntryMeanChangePct / 100 * beforeMs; key == "" || t < ms {
			ms, key = t, "max_entry_mean_change_pct"
		}
	}
	return ms, key
}

// Trusted reports whether a label passes rule 11.
func (p *Policy) Trusted(label string) bool {
	for _, l := range p.TrustedLabels {
		if l == label {
			return true
		}
	}
	return false
}

// number reads a JSON number; null reports whether the value is null.
func number(v json.RawMessage) (x float64, null, ok bool) {
	v = bytes.TrimSpace(v)
	if bytes.Equal(v, []byte("null")) {
		return 0, true, false
	}
	if len(v) == 0 || (v[0] != '-' && (v[0] < '0' || v[0] > '9')) {
		return 0, false, false
	}
	if err := json.Unmarshal(v, &x); err != nil {
		return 0, false, false
	}
	return x, false, true
}

// stringList reads a JSON array of strings (never null).
func stringList(v json.RawMessage) ([]string, bool) {
	var out []string
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &out) != nil {
		return nil, false
	}
	if out == nil {
		out = []string{}
	}
	return out, true
}

func unique(xs []string) bool {
	seen := map[string]bool{}
	for _, x := range xs {
		if seen[x] {
			return false
		}
		seen[x] = true
	}
	return true
}

func allIn(xs, set []string) bool {
	for _, x := range xs {
		found := false
		for _, s := range set {
			found = found || s == x
		}
		if !found {
			return false
		}
	}
	return true
}

// strict decodes one JSON value into v, refusing unknown keys and trailing data.
func strict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func exceptions(v json.RawMessage) ([]Exception, string) {
	var raws []json.RawMessage
	if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, &raws) != nil {
		return nil, "must be a list of {rule, label?, reason}"
	}
	out := []Exception{}
	for i, r := range raws {
		var e struct {
			Rule   *string `json:"rule"`
			Label  *string `json:"label"`
			Reason *string `json:"reason"`
		}
		if err := strict(r, &e); err != nil || e.Rule == nil || e.Reason == nil {
			return nil, fmt.Sprintf("item %d must be {rule, label?, reason}", i)
		}
		if n := len(*e.Reason); n == 0 || n > 400 {
			return nil, fmt.Sprintf("item %d: reason must hold 1 to 400 characters", i)
		}
		switch *e.Rule {
		case RuleUnmeasuredDelta:
			if e.Label == nil || !allIn([]string{*e.Label}, change.Labels) {
				return nil, fmt.Sprintf("item %d: %s needs the label it lets through", i, RuleUnmeasuredDelta)
			}
			out = append(out, Exception{Rule: *e.Rule, Label: *e.Label, Reason: *e.Reason})
		case RuleThresholdUnderFloor:
			if e.Label != nil {
				return nil, fmt.Sprintf("item %d: %s takes no label", i, RuleThresholdUnderFloor)
			}
			out = append(out, Exception{Rule: *e.Rule, Reason: *e.Reason})
		default:
			return nil, fmt.Sprintf("item %d: only %s and %s can be relaxed", i, RuleUnmeasuredDelta, RuleThresholdUnderFloor)
		}
	}
	return out, ""
}
