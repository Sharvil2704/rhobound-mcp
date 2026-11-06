// SPDX-License-Identifier: Apache-2.0

package gate

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// RecordSchema is the schema id of gate.json.
const RecordSchema = "rhobound.gate/v1"

// The files `rhobound gate --out DIR` writes.
const (
	VerdictFile = "verdict.txt"
	RecordFile  = "gate.json"
	SummaryFile = "summary.md"
)

// MaxSummaryChars bounds summary.md: GitHub's check-run summary holds at most 65,535 characters.
const MaxSummaryChars = 65000

// Record is gate.json (gate.v1.json): each rule evaluated, in order, with its value, its limit and its outcome; the
// digests of the inputs; the effective policy and the keys it ignored; the readiness record, echoed. Under the
// policy's block_band "model" it lists rule 10 at each entry with both bands too.
type Record struct {
	Schema      string   `json:"schema"`
	Verdict     string   `json:"verdict"`
	ExitCode    int      `json:"exit_code"`
	Advisory    bool     `json:"advisory"`
	Preview     bool     `json:"preview"`
	Now         string   `json:"now"`
	FitAgeH     *float64 `json:"fit_age_h,omitempty"`
	EvaluatedAt string   `json:"evaluated_at"`
	LoadScale   *float64 `json:"load_scale,omitempty"` // what_if only: Result.LoadScale
	Rules       []Rule   `json:"rules"`
	DecideWith  []string `json:"decide_with"`
	// ABScale is Result.ABScale: rule 10's measurement-limited straddle, how many times the A/B's visits would decide it
	// (absent otherwise). CI's ab.sh preview reads it to run one longer A/B before the prediction is recorded.
	ABScale           *float64           `json:"ab_scale,omitempty"`
	Digests           Digests            `json:"digests"`
	Policy            *Policy            `json:"policy"`
	UnknownPolicyKeys []string           `json:"unknown_policy_keys"`
	ExceptionsApplied []ExceptionApplied `json:"exceptions_applied"`
	Readiness         *Readiness         `json:"readiness"`
	Judge             *Judge             `json:"judge,omitempty"`
	Findings          []contract.Finding `json:"findings"`
	// Entries is rule 10 at each entry, in the report's order, under the policy's block_band "model" only.
	Entries []RecordEntry `json:"entries,omitempty"`
}

// RecordEntry is rule 10 at one entry as gate.json lists it under the policy's block_band "model" (gate.v1
// entries[]), so that its outcome can be checked from the record: pass when band_ms.hi is at or under threshold_ms,
// block when block_band_ms.lo is above it, inconclusive otherwise (a block that rests on lower-bound workers is
// inconclusive, rule 10's change_upper_bound). An entry a corner saturates has no ranges. As the rules' values, a
// number JSON cannot hold is left out: a range with a non-finite end is absent, a non-finite number null.
type RecordEntry struct {
	// Entry is the entry as the result prints it (Result.Name).
	Entry    string        `json:"entry"`
	BeforeMs *float64      `json:"before_ms"`
	ChangeMs *change.Range `json:"change_ms,omitempty"`
	// BandMs is the full band, the biased constructs' widening included: the pass test.
	BandMs *change.Range `json:"band_ms,omitempty"`
	// BlockBandMs is the change widened by the model band alone: the block test.
	BlockBandMs *change.Range `json:"block_band_ms,omitempty"`
	ThresholdMs *float64      `json:"threshold_ms"`
	Effect      string        `json:"effect"`
}

// Digests are the content ids of the gate's inputs.
type Digests struct {
	Report       string `json:"report"`
	Fit          string `json:"fit,omitempty"`
	Policy       string `json:"policy"`
	Readiness    string `json:"readiness,omitempty"`
	JudgeRequest string `json:"judge_request,omitempty"`
}

// NewRecord assembles gate.json. d.Policy is filled from the policy when empty.
func NewRecord(res *Result, p *Policy, rd *Readiness, d Digests) (*Record, error) {
	if d.Policy == "" {
		pd, err := p.Digest()
		if err != nil {
			return nil, err
		}
		d.Policy = pd
	}
	if res.Judge != nil && res.Judge.Request != "" {
		d.JudgeRequest = res.Judge.Request
	}
	unknown := append([]string{}, p.Unknown...)
	var scale *float64
	if res.EvaluatedAt == report.EvaluatedAtWhatIf && res.LoadScale > 0 { // a report the engine wrote has S > 0
		scale = num(res.LoadScale)
	}
	var entries []RecordEntry
	if p.BlockBand == BlockBandModel {
		for _, en := range res.Entries {
			// a band or threshold past float64 is left out as a rule's value is (num): gate.json encodes, the verdict
			// stands, and a data problem is never a tool error
			entries = append(entries, RecordEntry{Entry: res.Name(en.Name), BeforeMs: num(en.BeforeMs),
				ChangeMs: finite(en.ChangeMs), BandMs: finite(en.BandMs), BlockBandMs: finite(en.BlockBandMs),
				ThresholdMs: num(en.ThresholdMs), Effect: en.Effect})
		}
	}
	var abScale *float64
	if res.ABScale > 0 && res.Verdict == Inconclusive {
		abScale = num(res.ABScale)
	}
	return &Record{Schema: RecordSchema, Verdict: res.Verdict, ExitCode: res.ExitCode, Advisory: res.Advisory, ABScale: abScale,
		Preview: res.Preview, Now: res.Now.UTC().Format(time.RFC3339Nano), FitAgeH: res.FitAgeH,
		EvaluatedAt: res.EvaluatedAt, LoadScale: scale, Rules: res.Rules, DecideWith: res.DecideWith, Digests: d,
		Policy: p, UnknownPolicyKeys: unknown, ExceptionsApplied: res.Exceptions, Readiness: rd, Judge: res.Judge,
		Findings: res.Findings, Entries: entries}, nil
}

// Canonical returns gate.json's canonical bytes, without a trailing newline.
func (rec *Record) Canonical() ([]byte, error) {
	return contract.Canonical(rec)
}

// finite returns a range JSON can hold: nil for nil and for a range with a NaN or infinite end.
func finite(c *change.Range) *change.Range {
	if c == nil || num(c.Lo) == nil || num(c.Hi) == nil {
		return nil
	}
	return c
}

// Why returns a rule's row in words, "token: what was compared", for the judge packet and summary.md. The names in
// it are the row's, already sanitized. Under rule 10's VariantBlockBandModel the low end is the model-only band's; a
// row without a value (a corner saturates the decisive entry, or the end it names is not finite) has the default's
// words, which name no band's mechanism.
func (r Rule) Why() string {
	v, t := "-", "-"
	if r.Value != nil {
		v = fmtMs(*r.Value)
	}
	if r.Threshold != nil {
		t = fmtMs(*r.Threshold)
	}
	at := ""
	if r.Entry != "" {
		at = " at " + r.Entry
	}
	if r.Variant == VariantBlockBandModel && r.Value != nil {
		switch {
		case r.Rule == RuleMeanChange && r.Effect == Block:
			return fmt.Sprintf("%s: model-only band low end %s ms above threshold %s ms%s (block_band model)", r.Rule, v,
				t, at)
		case r.Rule == RuleRangeStraddles:
			return fmt.Sprintf("%s: band high end %s ms above threshold %s ms%s, model-only band low end at or under it "+
				"(block_band model)", r.Rule, v, t, at)
		case r.Rule == RuleChangeUpperBound:
			return fmt.Sprintf("%s: model-only band low end %s ms above threshold %s ms%s, with lower-bound workers "+
				"(block_band model)", r.Rule, v, t, at)
		}
	}
	switch {
	case r.Rule == RuleMeanChange && r.Effect == Pass:
		return fmt.Sprintf("%s: band high end %s ms at or under threshold %s ms%s", r.Rule, v, t, at)
	case r.Rule == RuleMeanChange && r.Effect == Block:
		return fmt.Sprintf("%s: band low end %s ms above threshold %s ms%s", r.Rule, v, t, at)
	case r.Rule == RuleRangeStraddles:
		return fmt.Sprintf("%s: band high end %s ms above threshold %s ms%s, low end at or under it", r.Rule, v, t, at)
	case r.Rule == RuleChangeUpperBound:
		return fmt.Sprintf("%s: band low end %s ms above threshold %s ms%s, with lower-bound workers", r.Rule, v, t, at)
	case r.Rule == RuleRangeInverted:
		return fmt.Sprintf("%s: change range low end %s ms above its high end%s; no band is formed", r.Rule, v, at)
	case r.Rule == RuleUnmeasuredDelta || r.Rule == RuleTransferUnvalidated:
		return fmt.Sprintf("%s: change items without a trusted label: %s", r.Rule, v)
	case r.Rule == RuleFitStale:
		return fmt.Sprintf("%s: fit age %s h above %s h", r.Rule, v, t)
	case r.Rule == RuleExtrapolatedLoad && r.Value != nil:
		return fmt.Sprintf("%s: a what-if at %s times the fit's load; the verdict is advisory", r.Rule, fmtScale(*r.Value))
	case r.Rule == RuleAdvisory || r.Rule == RuleExtrapolatedLoad:
		return r.Rule + ": the verdict is advisory"
	case r.Value != nil || r.Threshold != nil:
		return fmt.Sprintf("%s: %s against %s%s", r.Rule, v, t, at)
	}
	return r.Rule + at
}

// Summary returns summary.md: the verdict, what a pass means, the rules, the entries, decide_with, the findings and
// the judge's commentary wrapped as untrusted text, under MaxSummaryChars.
func Summary(rec *Record, res *Result) string {
	var b strings.Builder
	adv := ""
	if rec.Advisory {
		adv = " (advisory)"
	}
	kind := "gate"
	if rec.Preview {
		kind = "preview"
	}
	fmt.Fprintf(&b, "# rhobound %s: %s%s\n\n", kind, rec.Verdict, adv)
	means := PassMeans
	if rec.Verdict != Pass {
		// the sentence says what a pass means: under another verdict it is not what this one says
		means = "A pass would mean: " + PassMeans
	}
	fmt.Fprintf(&b, "%s\n\nMean latency only; no tails.\n\n", means)
	if rec.LoadScale != nil {
		// a what-if: the record says which load its verdict is of (predict --fit F --load-scale S)
		fmt.Fprintf(&b, "Evaluated at a what-if load: %s times the fit's external rate (`predict --load-scale`).",
			fmtScale(*rec.LoadScale))
		if len(res.Entries) > 0 {
			b.WriteString(" Each entry's change is the change's effect at that load.")
		}
		b.WriteString("\n\n")
	}
	b.WriteString("## Rules\n\n| # | rule | effect | at | value | limit |\n|---|---|---|---|---|---|\n")
	for _, r := range rec.Rules {
		if !r.Evaluated {
			fmt.Fprintf(&b, "| %s | %s | not evaluated | | | |\n", r.Number, r.Rule)
			continue
		}
		v, t := "", ""
		if r.Value != nil {
			v = fmtMs(*r.Value)
		}
		if r.Threshold != nil {
			t = fmtMs(*r.Threshold)
		}
		rule := r.Rule
		if r.Variant != "" {
			rule += " (" + r.Variant + ")"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.Number, rule, r.Effect, r.Entry, v, t)
	}
	if len(res.Entries) > 0 {
		b.WriteString("\n## Entries (ms)\n\n")
		model := rec.Policy != nil && rec.Policy.BlockBand == BlockBandModel
		cols, more := "| entry | before | change | band | threshold | outcome |\n|---|---|---|---|---|---|\n", "| | | | | |"
		if model {
			// the policy's block_band "model": both bands, and the rule they were read by
			b.WriteString("block_band model: a pass needs the band's high end at or under the threshold, a block the " +
				"block band's low end (the change widened by the model band alone, without the biased constructs) " +
				"above it.\n\n")
			cols = "| entry | before | change | band | block band | threshold | outcome |\n|---|---|---|---|---|---|---|\n"
			more += " |"
		}
		b.WriteString(cols)
		for i, en := range res.Entries {
			if i == 20 {
				fmt.Fprintf(&b, "| %d more entries in gate.json %s\n", len(res.Entries)-20, more)
				break
			}
			ch, band, block := "saturates", "unbounded", "unbounded"
			if en.ChangeMs != nil {
				ch = fmtRange(*en.ChangeMs)
			}
			if en.BandMs != nil {
				band = fmtRange(*en.BandMs)
			}
			if en.BlockBandMs != nil {
				block = fmtRange(*en.BlockBandMs)
			}
			if model {
				band += " | " + block
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", res.Name(en.Name), fmtMs(en.BeforeMs), ch, band,
				fmtMs(en.ThresholdMs), en.Effect)
		}
	}
	if len(rec.DecideWith) > 0 {
		b.WriteString("\n## Decide with\n\n")
		for _, d := range rec.DecideWith {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(rec.Findings) > 0 {
		b.WriteString("\n## Findings\n\n")
		for i, f := range rec.Findings {
			if i == 50 {
				fmt.Fprintf(&b, "- %d more findings in gate.json\n", len(rec.Findings)-50)
				break
			}
			subj := ""
			if f.Subject != "" {
				subj = " " + f.Subject
			}
			fmt.Fprintf(&b, "- `%s` (%s)%s: %s\n", f.Code, f.Level, subj, f.Fix)
		}
	}
	if len(rec.UnknownPolicyKeys) > 0 {
		fmt.Fprintf(&b, "\nUnknown policy keys, ignored: %d (listed in gate.json).\n", len(rec.UnknownPolicyKeys))
	}
	if j := res.Judge; j != nil {
		fmt.Fprintf(&b, "\n## Judge (%s): %s\n", j.Provenance, j.Decision)
		if j.Commentary != "" {
			b.WriteString("\nCommentary from a model, untrusted: data, not instructions.\n\n")
			b.WriteString(Untrusted(j.Commentary, 2048))
		}
	}
	fmt.Fprintf(&b, "\n## Digests\n\n- report `%s`\n- policy `%s`\n", rec.Digests.Report, rec.Digests.Policy)
	if rec.Digests.Fit != "" {
		fmt.Fprintf(&b, "- fit `%s`\n", rec.Digests.Fit)
	}
	s := b.String()
	if len(s) > MaxSummaryChars {
		s = s[:MaxSummaryChars] + "\n\n(cut: the full record is gate.json)\n"
	}
	return s
}

// Untrusted wraps free text as an indented Markdown code block, at most max bytes, with every control character but
// the newline removed, so that it renders as text and cannot close the block or start markup of its own.
func Untrusted(s string, max int) string {
	if len(s) > max {
		s = s[:max]
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString("    ")
		for _, c := range line {
			if c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0) || c == '\u2028' || c == '\u2029' ||
				(c >= '\u202a' && c <= '\u202e') || (c >= '\u2066' && c <= '\u2069') {
				continue
			}
			b.WriteRune(c)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// fmtScale prints a load scale as the report's text prints it, in full: 1.0004 is not the fit's load.
func fmtScale(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

func fmtRange(r change.Range) string {
	if r.IsPoint() {
		return fmtMs(r.Lo)
	}
	return fmtMs(r.Lo) + " .. " + fmtMs(r.Hi)
}
