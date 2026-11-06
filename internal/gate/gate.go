// SPDX-License-Identifier: Apache-2.0

// Package gate is the deploy gate and the agent's preview of it (docs/mcp/ARCHITECTURE.md 6): a pure function of a
// report v2, a policy, an optional readiness record and the time, which gives pass, block or inconclusive by the
// thirteen rules of ARCHITECTURE.md 6.3, in their fixed order, with the band geometry of 6.4.
//
// The preview (`rhobound preview`, the MCP predict tool) and the authoritative gate (`rhobound gate`, in CI) apply the
// same rules, rule 11 included: a change made only of declared numbers is never a pass. Every verdict of this version
// is advisory: enforcement is earned per environment from ledger records (6.6), and there is no ledger yet.
//
// Rule tokens (fit_stale, unmeasured_delta, range_straddles, input_refused:<code>, ...) name why a rule fired. They
// are a namespace of their own, listed here, separate from the finding codes of the contract package; a token may
// cite the code that triggered it.
package gate

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/sanitize"
)

// The verdicts. Block outranks inconclusive, which outranks pass.
const (
	Pass         = "pass"
	Block        = "block"
	Inconclusive = "inconclusive"
)

// EffectNote is the effect of a rule that informs without deciding: a printed doubt, an advisory mark, or a rule that
// was not evaluated.
const EffectNote = "note"

// The exit statuses of `rhobound gate` (ARCHITECTURE.md 6.1): rhobound-cli already uses 2 for a usage error, so
// inconclusive is 3. A data refusal is inconclusive (3), never a tool error (2).
const (
	ExitPass         = 0
	ExitBlock        = 1
	ExitToolError    = 2
	ExitInconclusive = 3
)

// The rule tokens (ARCHITECTURE.md 5.4, 6.3). A rule's row carries its token when it fired, and its name (the first
// token listed for it) when it did not.
const (
	RuleInputRefused          = "input_refused" // rule 1, as input_refused:<code>
	RuleBaseOutOfScope        = "base_out_of_scope"
	RuleFitStale              = "fit_stale" // rule 3
	RuleBaselineMoved         = "baseline_moved"
	RuleModelMisfit           = "model_misfit"
	RuleDoubtHolds            = "doubt_holds" // rule 5, as doubt_holds:<code>
	RuleBeyondValidated       = "beyond_validated"
	RuleExtrapolatedLoad      = "extrapolated_load"
	RuleSaturates             = "saturates"
	RuleSaturationUpperBound  = "saturation_upper_bound"
	RuleUtilizationAfter      = "utilization_after" // rule 9
	RuleUtilizationUpperBound = "utilization_after_upper_bound"
	RuleMeanChange            = "mean_change" // rule 10
	RuleRangeStraddles        = "range_straddles"
	RuleRangeInverted         = "range_inverted" // rule 10: an entry's change range with lo above hi
	RuleChangeUpperBound      = "change_upper_bound"
	RuleUnmeasuredDelta       = "unmeasured_delta" // rule 11
	RuleTransferUnvalidated   = "transfer_unvalidated"
	RuleThresholdUnderFloor   = "threshold_under_floor"
	RuleAdvisory              = "advisory"
)

// VariantBlockBandModel names rule 10's variant under the policy's block_band "model" (Rule.Variant): the pass test
// reads the full band, the block test the change range widened by the model band alone (ARCHITECTURE.md 6.4). A
// variant is not a token: the row's token is mean_change, range_straddles or change_upper_bound as under the default.
const VariantBlockBandModel = "block_band_model"

// RuleTokens lists every rule token, in rule order. Exceptions (Policy.Exceptions) may name only RuleUnmeasuredDelta
// and RuleThresholdUnderFloor.
var RuleTokens = []string{RuleInputRefused, RuleBaseOutOfScope, RuleFitStale, RuleBaselineMoved, RuleModelMisfit,
	RuleDoubtHolds, RuleBeyondValidated, RuleExtrapolatedLoad, RuleSaturates, RuleSaturationUpperBound,
	RuleUtilizationAfter, RuleUtilizationUpperBound, RuleMeanChange, RuleRangeStraddles, RuleRangeInverted,
	RuleChangeUpperBound, RuleUnmeasuredDelta, RuleTransferUnvalidated, RuleThresholdUnderFloor, RuleAdvisory}

// ruleNumbers are the rows of every result, in order; ruleNames their names when they do not fire.
var (
	ruleNumbers = []string{"1", "2", "3", "4", "5", "6", "7", "8", "8b", "9", "10", "11", "12", "13"}
	ruleNames   = []string{RuleInputRefused, RuleBaseOutOfScope, RuleFitStale, RuleModelMisfit, RuleDoubtHolds,
		RuleBeyondValidated, RuleExtrapolatedLoad, RuleSaturates, RuleSaturationUpperBound, RuleUtilizationAfter,
		RuleMeanChange, RuleUnmeasuredDelta, RuleThresholdUnderFloor, RuleAdvisory}
)

// The sentence every preview carries (prediction.v1 preview.sentence).
const PreviewSentence = "advisory: computed from inputs the caller supplied; CI decides with measured inputs and the " +
	"repository's policy"

// PassMeans is printed with every gate verdict (ARCHITECTURE.md 1).
const PassMeans = "With the inputs listed here, the model's predicted change of mean latency at every entry, widened " +
	"by the model's documented error, stays under the policy's threshold, and every input passed its checks. It does " +
	"not mean the change is safe, and it says nothing about tail latency."

// MaxDecideWith is the number of decide_with lines a result holds at most, and MaxDecideChars their length.
const (
	MaxDecideWith  = 4
	MaxDecideChars = 200
)

// Input is what one evaluation reads. Exactly one of Report and Refusal is set: a prediction file holds a report v2,
// or the refusal the engine printed in its place, which rule 1 (or rule 2, for model.utilization_observed) turns into
// inconclusive.
type Input struct {
	Report  *report.V2
	Refusal *report.Refusal
	Policy  *Policy
	// Readiness is the --readiness record, or nil.
	Readiness *Readiness
	// Now is the time the fit's age is measured at: --now, or the clock the CLI read outside this function.
	Now time.Time
	// Preview marks an agent's preview: the same rules, always advisory.
	Preview bool
	// Running are the image digests running now, by service, for the baseline_moved half of rule 3; nil when not
	// known, and then only the fit's age is checked.
	Running map[string][]string
	// SafeBudgets are the safe budgets of own time per visit, by service (budget.Item.Safe), which decide_with quotes
	// when it names a measurement; nil when not computed.
	SafeBudgets map[string]float64
	// Judge is a judge answer that judge-check accepted, or nil (ARCHITECTURE.md 7.2).
	Judge *Judge
}

// Judge is a checked judge answer as the gate uses it. Escalation turns a pass into inconclusive, never into a block.
type Judge struct {
	// Provenance is ProvenanceClientAsserted, or ProvenanceCIJudge for an answer a CI step obtained from a pinned model.
	Provenance string `json:"provenance"`
	ModelID    string `json:"model_id,omitempty"`
	Decision   string `json:"decision"`
	// Answer is the digest of the answer's canonical JSON.
	Answer string `json:"answer"`
	// Reasons are the answer's reason codes ([a-z_]), and Commentary its free text, for people only: summary.md
	// prints it wrapped as untrusted, and nothing parses it.
	Reasons    []string `json:"-"`
	Commentary string   `json:"-"`
	// Request is the digest of the request a CI step sent to the model, or "".
	Request string `json:"-"`
}

// The provenances of a judge answer.
const (
	ProvenanceClientAsserted = "client-asserted"
	ProvenanceCIJudge        = "ci-judge"
)

// Result is one evaluation.
type Result struct {
	Verdict  string
	ExitCode int
	// Advisory is always true in this version.
	Advisory bool
	Preview  bool
	Now      time.Time
	// FitAgeH is the fit's age in hours at Now, nil when the report has no fit window.
	FitAgeH     *float64
	EvaluatedAt string
	// LoadScale is the load point of a what-if (EvaluatedAt what_if), a multiple of the fit's external rate: the
	// report's load.load_scale, or the load_scale number of a refusal at the load point; 0 otherwise.
	LoadScale float64
	// Extrapolated is true when the load point is beyond the fit's window (rule 7).
	Extrapolated bool
	// ValidatedCeiling is the policy's max_utilization_validated; BeyondValidated is rule 6's finding.
	ValidatedCeiling float64
	BeyondValidated  bool
	// Rules are the 14 rows, 1 to 13 with 8b, in order.
	Rules      []Rule
	DecideWith []string
	// ABScale is rule 10's measurement-limited straddle: how many times the A/B's visits would put the decisive entry's
	// band at or under its threshold if the measured change holds (abScale); 0 when more measurement cannot decide it
	// or the change is not measured by A/B alone. CI may run one longer A/B before it predicts (ab.sh preview).
	ABScale float64
	// Entries are rule 10's evaluation of each entry, in the report's order; nil for a refusal.
	Entries []Entry
	// Findings are the gate's own findings, sorted: fit.stale, load.beyond_validated, load.busiest_unknown,
	// gate.enforce_not_earned.
	Findings   []contract.Finding
	Exceptions []ExceptionApplied
	Judge      *Judge
	// shown is how the views of the report's fit show its node names (sanitize.Services).
	shown map[string]sanitize.Shown
}

// Name returns a node name as the result prints it: as the views of the report's fit show it (sanitize.Services, one
// to one over the report's rows, so that a row's entry, a decide_with line and a view name one service alike), and a
// name that is not a row by sanitize.Service's rule.
func (r *Result) Name(name string) string {
	if sh, ok := r.shown[name]; ok {
		return sh.Name
	}
	s, _ := sanitize.Service(name)
	return s
}

// Rule is one row of a result (gate.v1 rules[]).
type Rule struct {
	Number string `json:"number"`
	// Rule is the token when the rule fired, else the rule's name.
	Rule      string `json:"rule"`
	Evaluated bool   `json:"evaluated"`
	Effect    string `json:"effect"`
	// Entry is the entry or service the row is about, sanitized, or "".
	Entry     string   `json:"entry,omitempty"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	// Code is the finding code that triggered the rule, or "".
	Code string `json:"code,omitempty"`
	// Variant names the rule's variant when the policy chose one (VariantBlockBandModel on rule 10), or "".
	Variant string `json:"variant,omitempty"`
}

// Entry is rule 10 at one entry: the change range, the band (the change widened by the policy's model band, then by
// the biased constructs on the report's side), the threshold and the outcome.
type Entry struct {
	// Name is the entry as the report names it; views sanitize it.
	Name     string
	BeforeMs float64
	// ChangeMs is nil when a corner saturates a service; a report without change items has the point 0.
	ChangeMs    *change.Range
	ModelBandMs float64
	BandMs      *change.Range
	// BlockBandMs is the band the block test read under the policy's block_band "model": the change widened by the
	// model band alone, without the biased constructs. It is nil under the default, where BandMs is read on both
	// sides, and when ChangeMs is nil.
	BlockBandMs *change.Range
	ThresholdMs float64
	// ThresholdKey is the policy key that set ThresholdMs.
	ThresholdKey string
	Effect       string
}

// ExceptionApplied is a policy exception that relaxed a rule, with the change items it let through (gate.v1).
type ExceptionApplied struct {
	Rule   string `json:"rule"`
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason"`
	Items  []int  `json:"items,omitempty"`
}

var severity = map[string]int{Pass: 0, Inconclusive: 1, Block: 2}

// Evaluate applies the rules. It is pure: the time is in.Now, and nothing else is read.
func Evaluate(in Input) *Result {
	e := &evaluator{in: in, p: in.Policy, r: in.Report, res: &Result{Advisory: true, Preview: in.Preview,
		Now: in.Now.UTC(), EvaluatedAt: report.EvaluatedAtFitWindow, ValidatedCeiling: in.Policy.MaxUtilizationValidated,
		DecideWith: []string{}, Findings: []contract.Finding{}, Exceptions: []ExceptionApplied{}, Judge: in.Judge}}
	if e.r != nil {
		names := make([]string, len(e.r.Rows))
		for i, row := range e.r.Rows {
			names[i] = row.Name
		}
		e.res.shown = sanitize.Services(names)
	}
	e.rules = make([]Rule, len(ruleNumbers))
	for i := range e.rules {
		e.rules[i] = Rule{Number: ruleNumbers[i], Rule: ruleNames[i], Effect: EffectNote}
	}
	if e.r == nil {
		e.refused()
	} else {
		e.inputs()
		e.scope()
		e.fitAge()
		e.misfit()
		e.doubts()
		e.validated()
		e.load()
		e.saturation()
		e.utilization()
		e.meanChange()
		e.labels()
		e.floor()
	}
	e.advisory()
	return e.finish()
}

type evaluator struct {
	in    Input
	p     *Policy
	r     *report.V2
	res   *Result
	rules []Rule
}

// safe is a node name as the result prints it (Result.Name).
func (e *evaluator) safe(name string) string { return e.res.Name(name) }

func (e *evaluator) row(number string) *Rule {
	for i := range e.rules {
		if e.rules[i].Number == number {
			return &e.rules[i]
		}
	}
	panic("gate: no rule " + number)
}

// pass marks a rule evaluated and not fired.
func (e *evaluator) pass(number string) {
	r := e.row(number)
	r.Evaluated, r.Effect = true, Pass
}

// fire marks a rule fired with a token and an effect.
func (e *evaluator) fire(number, token, effect string) *Rule {
	r := e.row(number)
	r.Evaluated, r.Rule, r.Effect = true, token, effect
	return r
}

func (e *evaluator) decide(lines ...string) {
	for _, l := range lines {
		if len(l) > MaxDecideChars {
			l = l[:MaxDecideChars]
		}
		dup := false
		for _, x := range e.res.DecideWith {
			dup = dup || x == l
		}
		if !dup && len(e.res.DecideWith) < MaxDecideWith {
			e.res.DecideWith = append(e.res.DecideWith, l)
		}
	}
}

func (e *evaluator) finding(f contract.Finding) {
	for _, x := range e.res.Findings {
		if x.Code == f.Code && x.Subject == f.Subject {
			return
		}
	}
	e.res.Findings = append(e.res.Findings, f)
}

// refused handles a prediction that is a refusal: rule 2 for a base load above the limit, rule 1 for any other code;
// nothing else can be evaluated without a report. A base refused at a what-if load point carries it (the number
// load_scale, `predict --fit F --load-scale S`), and rule 7 is evaluated on it as on a report's.
func (e *evaluator) refused() {
	code := e.in.Refusal.Code
	if code == contract.ModelUtilizationObserved {
		e.pass("1")
		r := e.fire("2", RuleBaseOutOfScope, Inconclusive)
		r.Code = code
		if s, ok := e.in.Refusal.Numbers[report.LoadScaleNumber]; ok && s > 0 && !math.IsInf(s, 0) && s != 1 {
			e.loadPoint(report.EvaluatedAtWhatIf, s)
		}
		return
	}
	r := e.fire("1", RuleInputRefused+":"+code, Inconclusive)
	r.Code = code
	if !contract.Pattern.MatchString(code) {
		r.Rule, r.Code = RuleInputRefused, ""
	}
}

// inputs is rule 1: a refuse-level finding on an input.
func (e *evaluator) inputs() {
	for _, f := range e.r.Findings {
		if f.Level == contract.Refuse && f.Code != contract.ModelUtilizationObserved {
			r := e.fire("1", RuleInputRefused+":"+f.Code, Inconclusive)
			r.Code = f.Code
			return
		}
	}
	e.pass("1")
}

// scope is rule 2: the base load already above the utilization limit.
func (e *evaluator) scope() {
	limit := e.r.UtilizationLimit
	mr := e.r.Load.MaxUtilizationBefore
	refused := false
	for _, f := range e.r.Findings {
		refused = refused || f.Code == contract.ModelUtilizationObserved
	}
	if refused || (limit > 0 && !(mr <= limit)) {
		r := e.fire("2", RuleBaseOutOfScope, Inconclusive)
		r.Value, r.Threshold = num(mr), num(limit)
		if refused {
			r.Code = contract.ModelUtilizationObserved
		}
		return
	}
	e.pass("2")
}

// fitAge is rule 3: the fit older than max_fit_age_h at Now, or the running digests of a changed service no longer
// the fit's.
func (e *evaluator) fitAge() {
	end := e.r.FitWindow.End
	var age float64
	known := !end.IsZero()
	if known {
		age = e.in.Now.Sub(end).Hours()
		e.res.FitAgeH = &age
	}
	if !known || age > e.p.MaxFitAgeH {
		r := e.fire("3", RuleFitStale, Inconclusive)
		r.Code, r.Threshold = contract.FitStale, num(e.p.MaxFitAgeH)
		f := contract.NewFinding(contract.FitStale).WithNumber("max_fit_age_h", e.p.MaxFitAgeH)
		if known {
			r.Value = num(age)
			f = f.WithNumber("fit_age_h", age)
		}
		e.finding(f)
		e.decide("fit a fresh window, then predict again")
		return
	}
	if e.in.Running != nil {
		images := map[string][]string{}
		for _, row := range e.r.Rows {
			images[row.Name] = row.Images
		}
		for _, it := range e.r.Items {
			if run, ok := e.in.Running[it.Service]; ok && !sameSet(run, images[it.Service]) {
				r := e.fire("3", RuleBaselineMoved, Inconclusive)
				r.Entry = e.safe(it.Service)
				e.decide("fit a window after the running images of " + e.safe(it.Service) + " settled")
				return
			}
		}
	}
	e.pass("3")
}

// misfit is rule 4: an entry's or a service's error_pct above misfit_max_error_pct, rows whose hop was read from the
// mean excluded (their comparison is near zero by construction).
func (e *evaluator) misfit() {
	worst, at := 0.0, ""
	for _, row := range e.r.Rows {
		if row.ErrorPct == nil || row.HopFromMean || row.External || row.Stage || sanitize.IsModelNode(row.Name) {
			continue
		}
		if v := math.Abs(*row.ErrorPct); v > worst || math.IsNaN(v) {
			worst, at = v, row.Name
		}
	}
	if at != "" && !(worst <= e.p.MisfitMaxErrorPct) {
		r := e.fire("4", RuleModelMisfit, Inconclusive)
		r.Entry, r.Value, r.Threshold = e.safe(at), num(worst), num(e.p.MisfitMaxErrorPct)
		return
	}
	e.pass("4")
}

// doubts is rule 5: a doubt listed in doubts_that_hold.
func (e *evaluator) doubts() {
	hold := map[string]bool{}
	for _, c := range e.p.DoubtsThatHold {
		hold[c] = true
	}
	for _, f := range e.r.Findings {
		if f.Level == contract.Doubt && hold[f.Code] {
			r := e.fire("5", RuleDoubtHolds+":"+f.Code, Inconclusive)
			r.Code, r.Entry = f.Code, e.subject(f)
			return
		}
	}
	e.pass("5")
}

// subject is a finding's subject as rule 5's row names it. A subject the name rule made (subject_untrusted) that
// exactly one row is shown by alone is about that row, and is named as the other rows and decide_with name it (Name:
// svc_<hash8> of an ARN whose last element another service is named); one that two or more rows are shown by alone
// names no row (none: the views show each of them by svc_<hash8> of its ARN, and no row by the subject); any other as
// the views show a subject: none for a path from the root (sanitize.Subject).
func (e *evaluator) subject(f contract.Finding) string {
	s, _ := sanitize.Subject(f.Subject)
	if s == "" || !f.SubjectUntrusted {
		return s
	}
	var about []string
	for _, row := range e.r.Rows {
		if n, untrusted := sanitize.Service(row.Name); untrusted && n == s && !slices.Contains(about, row.Name) {
			about = append(about, row.Name)
		}
	}
	switch len(about) {
	case 0:
		return s
	case 1:
		return e.safe(about[0])
	}
	return ""
}

// IsService reports whether a report row is a service with a queue, one a change or a budget can name: not a peer
// outside the capture (ext:), not one of the model's zero-time nodes (stage:, shape:).
func IsService(row report.Row) bool {
	return !row.External && !row.Stage && !sanitize.IsModelNode(row.Name) &&
		!strings.HasPrefix(row.Name, sanitize.ExternalPrefix)
}

// services returns the report's rows that are services.
func (e *evaluator) services() []report.Row {
	var out []report.Row
	for _, row := range e.r.Rows {
		if IsService(row) {
			out = append(out, row)
		}
	}
	return out
}

// validated is rule 6: a service above max_utilization_validated before or after the change. It makes the verdict
// inconclusive when the policy enforces, and is a printed doubt otherwise. The report does not carry the entries'
// paths, so every service counts as on one.
func (e *evaluator) validated() {
	worst, at := 0.0, ""
	for _, row := range e.services() {
		u := row.Util
		if row.UtilizationAfter != nil && row.UtilizationAfter.Hi > u {
			u = row.UtilizationAfter.Hi
		}
		if u > worst {
			worst, at = u, row.Name
		}
	}
	if at == "" || worst <= e.p.MaxUtilizationValidated {
		e.pass("6")
		return
	}
	e.res.BeyondValidated = true
	effect := EffectNote
	if e.p.Enforce {
		effect = Inconclusive
	}
	r := e.fire("6", RuleBeyondValidated, effect)
	r.Code, r.Entry, r.Value, r.Threshold = contract.LoadBeyondValidated, e.safe(at), num(worst), num(e.p.MaxUtilizationValidated)
	f := contract.NewFinding(contract.LoadBeyondValidated).WithSubject(at).WithNumber("utilization", worst).
		WithNumber("max_utilization_validated", e.p.MaxUtilizationValidated)
	if sh, ok := e.res.shown[at]; ok {
		f.Subject, f.SubjectUntrusted = sh.Name, sh.Untrusted
	}
	e.finding(f)
}

// load is rule 7: a load point beyond the fit's window is a what-if, and the verdict is advisory whatever the policy
// says. Without a busiest-window pointer the gate evaluates at the fit's window, and says so (load.busiest_unknown).
func (e *evaluator) load() {
	if e.p.EvaluateAt == EvaluateBusiest {
		e.finding(contract.NewFinding(contract.LoadBusiestUnknown))
	}
	e.loadPoint(e.r.Load.EvaluatedAt, e.r.Load.LoadScale)
}

// loadPoint evaluates rule 7 at a load point: a what-if records its load scale (gate.json load_scale), at any value,
// so that the record says which load its verdict is of; above 1 the load is extrapolated.
func (e *evaluator) loadPoint(at string, scale float64) {
	e.res.EvaluatedAt = report.EvaluatedAtFitWindow
	if at == report.EvaluatedAtWhatIf {
		e.res.EvaluatedAt, e.res.LoadScale = report.EvaluatedAtWhatIf, scale
	}
	if at == report.EvaluatedAtWhatIf && scale > 1 {
		e.res.Extrapolated = true
		r := e.fire("7", RuleExtrapolatedLoad, EffectNote)
		r.Value = num(scale)
		return
	}
	e.pass("7")
}

// lowerBound reports whether the lower-bound direction may be used for a service: its worker count is a lower bound,
// and that bound can be trusted. With lower-bound workers the predicted wait and utilization are upper bounds, so an
// over-threshold result is inconclusive rather than a block, but only when CLIENT spans are present on the edges into
// the service and no clock-skew finding fired (ARCHITECTURE.md 6.3). A report carries no edges, so the edges are read
// from its findings: a trace.client_spans_missing finding whose subject (caller/callee) names the service, or whose
// subject was hashed and so may name it, means CLIENT spans are missing there.
func (e *evaluator) lowerBound(row report.Row) bool {
	if !row.WorkersLower {
		return false
	}
	for _, f := range e.r.Findings {
		switch f.Code {
		case contract.TraceClockSkew:
			return false
		case contract.TraceClientSpansMissing:
			if f.SubjectUntrusted || f.Subject == "" || strings.HasSuffix(f.Subject, "/"+row.Name) {
				return false
			}
		}
	}
	return true
}

// moved reports whether the change moves a service's utilization.
func moved(row report.Row) bool {
	u := row.UtilizationAfter
	if u == nil {
		return false
	}
	tol := 1e-9 * math.Max(1, math.Abs(row.Util))
	return math.Abs(u.Lo-row.Util) > tol || math.Abs(u.Hi-row.Util) > tol
}

// raised reports whether the change raises the service's utilization (its after range's high end above it): the
// engine takes such a service unbounded by the same test (report.Raises).
func raised(row report.Row) bool {
	u := row.UtilizationAfter
	return u != nil && report.Raises(row.Util, u.Hi)
}

// saturated returns the services the change pushes past the utilization limit or to saturation.
func (e *evaluator) saturated() []report.Row {
	var out []report.Row
	limit := e.r.UtilizationLimit
	for _, row := range e.services() {
		if u := row.UtilizationAfter; u != nil && moved(row) && (!(u.Hi <= limit) || u.Hi >= 1) {
			out = append(out, row)
		}
	}
	return out
}

// saturation is rules 8 and 8b: after the change a service passes the limit or saturates; block with identified
// workers, inconclusive with lower-bound ones, and decide_with names the worker count that would decide it.
func (e *evaluator) saturation() {
	if len(e.r.Items) == 0 {
		e.pass("8")
		e.pass("8b")
		return
	}
	var ident, lower []report.Row
	for _, row := range e.saturated() {
		if e.lowerBound(row) {
			lower = append(lower, row)
		} else {
			ident = append(ident, row)
		}
	}
	if len(ident) == 0 && len(lower) == 0 && e.anyNilChange() {
		// an entry has no finite mean after the change, but no row says which service: the busiest one after it
		if row, ok := e.busiestAfter(); ok {
			if e.lowerBound(row) {
				lower = append(lower, row)
			} else {
				ident = append(ident, row)
			}
		}
	}
	if len(ident) > 0 {
		r := e.fire("8", RuleSaturates, Block)
		r.Code, r.Entry, r.Value, r.Threshold = contract.ModelUtilizationAfter, e.safe(ident[0].Name),
			afterHi(ident[0]), num(e.r.UtilizationLimit)
	} else {
		e.pass("8")
	}
	if len(lower) > 0 {
		r := e.fire("8b", RuleSaturationUpperBound, Inconclusive)
		r.Entry, r.Value, r.Threshold = e.safe(lower[0].Name), afterHi(lower[0]), num(e.r.UtilizationLimit)
		for _, row := range lower {
			e.decide("workers:" + e.safe(row.Name))
		}
	} else {
		e.pass("8b")
	}
}

func (e *evaluator) anyNilChange() bool {
	for _, en := range e.r.Entries {
		if en.ChangeMs == nil {
			return true
		}
	}
	return false
}

func (e *evaluator) busiestAfter() (report.Row, bool) {
	best, ok := report.Row{}, false
	for _, row := range e.services() {
		if row.UtilizationAfter != nil && (!ok || !(row.UtilizationAfter.Hi <= best.UtilizationAfter.Hi)) {
			best, ok = row, true
		}
	}
	return best, ok
}

// utilization is rule 9: a service the change raises above max_utilization_after (below the limit): block, or
// inconclusive with lower-bound workers. A service the change does not move is not held against it.
func (e *evaluator) utilization() {
	sat := map[string]bool{}
	for _, row := range e.saturated() {
		sat[row.Name] = true
	}
	var hit *report.Row
	lower := true
	for _, row := range e.services() {
		u := row.UtilizationAfter
		if u == nil || sat[row.Name] || !moved(row) || !(u.Hi > e.p.MaxUtilizationAfter) || !(u.Hi > row.Util) {
			continue
		}
		row := row
		if hit == nil || (lower && !e.lowerBound(row)) {
			hit, lower = &row, e.lowerBound(row)
		}
	}
	if hit == nil {
		e.pass("9")
		return
	}
	if lower {
		r := e.fire("9", RuleUtilizationUpperBound, Inconclusive)
		r.Entry, r.Value, r.Threshold = e.safe(hit.Name), afterHi(*hit), num(e.p.MaxUtilizationAfter)
		e.decide("workers:" + e.safe(hit.Name))
		return
	}
	r := e.fire("9", RuleUtilizationAfter, Block)
	r.Entry, r.Value, r.Threshold = e.safe(hit.Name), afterHi(*hit), num(e.p.MaxUtilizationAfter)
}

// BandOf returns an entry's band for a change range under a model band (ARCHITECTURE.md 4.5, 6.4): each end widened
// by the model band at that end's change, mb.Ms(lo) below and mb.Ms(hi) above, as report v2 writes band_ms, and then
// by the biased constructs' widening on each side. It returns the model band in ms too: the larger of the two ends',
// mb.Ms(max(|lo|, |hi|)), as report v2 writes model_band_ms.
func BandOf(mb report.ModelBand, ch change.Range, beforeMs, biasLo, biasHi float64) (change.Range, float64) {
	mlo, mhi := mb.Ms(ch.Lo, beforeMs), mb.Ms(ch.Hi, beforeMs)
	return change.Range{Lo: ch.Lo - mlo - biasLo, Hi: ch.Hi + mhi + biasHi}, math.Max(mlo, mhi)
}

// bias returns the widening of an entry's band beyond its model band on each side, as the report computed it with its
// own model band (report v2's model_band, the defaults).
func bias(en report.Entry, rb report.ModelBand) (lo, hi float64) {
	if en.ChangeMs == nil || en.BandMs == nil {
		return 0, 0
	}
	return biasOf(*en.ChangeMs, *en.BandMs, en.BeforeMs, rb)
}

// biasOf is bias for a change range and the band the report widened it to: an entry's, or its workers_unbounded.
func biasOf(ch, band change.Range, beforeMs float64, rb report.ModelBand) (lo, hi float64) {
	lo = (ch.Lo - rb.Ms(ch.Lo, beforeMs)) - band.Lo
	hi = band.Hi - (ch.Hi + rb.Ms(ch.Hi, beforeMs))
	tol := 1e-12 * math.Max(1, beforeMs)
	if !(lo > tol) {
		lo = 0
	}
	if !(hi > tol) {
		hi = 0
	}
	return lo, hi
}

// meanChange is rule 10: the band of each entry's change against its threshold (ARCHITECTURE.md 6.4). The row names
// the decisive entry: the worst outcome, then the band's highest end above its threshold. Under the policy's
// block_band "model" the pass test still reads the full band, and the block test the band without the biased
// constructs' widening (Entry.BlockBandMs); the row names the variant. When the change raises a service whose workers
// are a trusted lower bound, a block is inconclusive (change_upper_bound) at each entry whose change with that
// queueing at its least would not block (unboundedBlocks); an entry whose change would still block keeps it.
func (e *evaluator) meanChange() {
	model := e.p.BlockBand == BlockBandModel
	if model {
		e.row("10").Variant = VariantBlockBandModel
	}
	mb := e.p.ModelBand.ModelBand()
	rb := e.r.ModelBand
	if rb.ChangeFrac == 0 && rb.LatencyFrac == 0 {
		rb = report.DefaultModelBand()
	}
	worst := -1
	var worstMargin float64
	for i, en := range e.r.Entries {
		t, key := e.p.Threshold(en.BeforeMs)
		ev := Entry{Name: en.Name, BeforeMs: en.BeforeMs, ThresholdMs: t, ThresholdKey: key}
		var ch *change.Range
		switch {
		case len(e.r.Items) == 0:
			ch = &change.Range{}
		case en.ChangeMs != nil:
			c := *en.ChangeMs
			ch = &c
		}
		if ch == nil {
			// a corner saturates a service on the path: rules 8 and 8b decide; the band is unbounded above
			ev.Effect = Block
			if e.row("8").Effect != Block && e.row("8b").Effect == Inconclusive {
				ev.Effect = Inconclusive
			}
		} else if !(ch.Lo <= ch.Hi) {
			// an inverted range: the pass test would read its high end alone and pass a change whose low end is over
			// the threshold (lean/Rbproofs/Gate.lean, inverted_range_passes). Nothing upstream refuses one.
			ev.ChangeMs, ev.Effect = ch, Inconclusive
		} else {
			bl, bh := bias(en, rb)
			band, m := BandOf(mb, *ch, en.BeforeMs, bl, bh)
			ev.ChangeMs, ev.BandMs, ev.ModelBandMs = ch, &band, m
			blockLo := band.Lo
			if model {
				// the model band alone: its low end is at or above the full band's, its high end at or under
				bb, _ := BandOf(mb, *ch, en.BeforeMs, 0, 0)
				ev.BlockBandMs, blockLo = &bb, bb.Lo
			}
			switch {
			case band.Hi <= t:
				ev.Effect = Pass
			case blockLo > t:
				ev.Effect = Block
			default:
				ev.Effect = Inconclusive
			}
		}
		e.res.Entries = append(e.res.Entries, ev)
		margin := math.Inf(1)
		if ev.BandMs != nil {
			margin = ev.BandMs.Hi - t
		}
		if worst < 0 || severity[ev.Effect] > severity[e.res.Entries[worst].Effect] ||
			(severity[ev.Effect] == severity[e.res.Entries[worst].Effect] && margin > worstMargin) {
			worst, worstMargin = i, margin
		}
	}
	if worst < 0 {
		e.pass("10")
		return
	}
	ev := e.res.Entries[worst]
	r := e.row("10")
	r.Evaluated, r.Effect, r.Entry, r.Threshold = true, ev.Effect, e.safe(ev.Name), num(ev.ThresholdMs)
	switch ev.Effect {
	case Pass:
		r.Value = num(ev.BandMs.Hi)
	case Block:
		if held := e.lowerBoundMoved(); len(held) > 0 && ev.BandMs != nil {
			// the blocks may rest on lower-bound workers: an entry keeps its block only when its change with their
			// queueing at its least blocks too, and the row names the decisive entry of those that keep it
			keep, keepMargin := -1, 0.0
			for i := range e.res.Entries {
				x := &e.res.Entries[i]
				if x.Effect != Block || x.BandMs == nil {
					continue
				}
				if !unboundedBlocks(e.r.Entries[i], held, mb, rb, model, x.ThresholdMs) {
					x.Effect = Inconclusive
				} else if m := x.BandMs.Hi - x.ThresholdMs; keep < 0 || m > keepMargin {
					keep, keepMargin = i, m
				}
			}
			if keep < 0 {
				r.Rule, r.Effect = RuleChangeUpperBound, Inconclusive
				e.decide("workers:" + e.safe(held[0]))
			} else {
				ev = e.res.Entries[keep]
				r.Entry, r.Threshold = e.safe(ev.Name), num(ev.ThresholdMs)
			}
		}
		if ev.BlockBandMs != nil {
			r.Value = num(ev.BlockBandMs.Lo)
		} else if ev.BandMs != nil {
			r.Value = num(ev.BandMs.Lo)
		}
	case Inconclusive:
		if ev.BandMs == nil && ev.ChangeMs != nil && !(ev.ChangeMs.Lo <= ev.ChangeMs.Hi) {
			// no band was formed: there is nothing to straddle, and no measurement line would decide it
			r.Rule, r.Value = RuleRangeInverted, num(ev.ChangeMs.Lo)
			return
		}
		r.Rule = RuleRangeStraddles
		if ev.BandMs != nil {
			r.Value = num(ev.BandMs.Hi)
		}
		k, ok := 0.0, false
		if ev.ChangeMs != nil && e.measuredOnly() {
			bl, bh := bias(e.r.Entries[worst], rb)
			k, ok = abScale(*ev.ChangeMs, mb, ev.BeforeMs, bl, bh, ev.ThresholdMs)
		}
		if ok {
			e.res.ABScale = math.Ceil(k*10) / 10
		}
		for _, svc := range e.changedServices() {
			if ok {
				// the change is measured, and its band straddles only because the A/B's standard errors are wide: the
				// measurement that decides it is a longer A/B, not another change (experiment 2's T7: four clean changes held)
				e.decide(fmt.Sprintf("a longer A/B of %s: about %s times this one's visits (a standard error falls with the "+
					"square root of the visits) puts the band at %s at or under %s ms if the measured change holds",
					e.safe(svc), strconv.FormatFloat(e.res.ABScale, 'f', 1, 64), e.safe(ev.Name), fmtMs(ev.ThresholdMs)))
				continue
			}
			e.decide(e.measureLine(svc, ev))
		}
	}
}

// measuredOnly reports whether every change item is measured by an A/B (measured-ab, or derived from its record):
// each item's range is then its delta +-2 standard errors, which a longer A/B narrows.
func (e *evaluator) measuredOnly() bool {
	if len(e.r.Items) == 0 {
		return false
	}
	for _, it := range e.r.Items {
		if it.Label != change.LabelMeasuredAB && it.Label != change.LabelDerived {
			return false
		}
	}
	return true
}

// abScale returns how many times the A/B's visits would put an entry's band at or under t if the measured change
// holds: the change range's half-width falls with the square root of the visits (each measured item is its delta
// +-2 standard errors), around the same centre, and the model band and bias widen it as BandOf does. ok is false for
// a range without width, and when even a zero-width range would not pass: the centre and the model band are then over
// t, and more measurement cannot decide it.
func abScale(ch change.Range, mb report.ModelBand, beforeMs, bl, bh, t float64) (float64, bool) {
	c, h := (ch.Lo+ch.Hi)/2, (ch.Hi-ch.Lo)/2
	pass := func(w float64) bool {
		b, _ := BandOf(mb, change.Range{Lo: c - w, Hi: c + w}, beforeMs, bl, bh)
		return b.Hi <= t
	}
	if !(h > 0) || !pass(0) {
		return 0, false
	}
	lo, hi := 0.0, h
	for range 64 {
		m := (lo + hi) / 2
		if pass(m) {
			lo = m
		} else {
			hi = m
		}
	}
	if !(lo > 0) {
		return 0, false
	}
	return (h / lo) * (h / lo), true
}

// lowerBoundMoved returns the services the change raises whose worker count is a trusted lower bound, in the rows'
// order: the predicted change is then an over-estimate, and a block at rule 10 that rests on their queueing is
// inconclusive instead (unboundedBlocks). A service the change only lowers does not count: at a lower bound its
// improvement is over-stated, so the entry's true change is at or above the predicted one and the block stands
// (ARCHITECTURE.md 6.3: never for a claimed improvement; lean/Rbproofs/Lanes/GateRules.lean, improvement_keeps_block).
func (e *evaluator) lowerBoundMoved() []string {
	var out []string
	for _, row := range e.services() {
		if raised(row) && e.lowerBound(row) {
			out = append(out, row.Name)
		}
	}
	return out
}

// unboundedBlocks reports whether an entry's block stands with the queueing of the lower-bound services the change
// raises at its least: the entry's workers_unbounded change range (report.Unbounded), widened as the block test widens
// its change range (by the model band at the low end, then, under the default block_band, by the biased constructs'
// widening on that side), still above the threshold. The block then does not rest on them: however many workers they
// have, the entry's change is over the threshold (GateRules.lean, GR-1: a negligible raise of a lower-bound service
// no longer turns a block that rests elsewhere into inconclusive). It is false for an entry without that range, and
// for one whose range leaves out a service of held: its block may rest on that service's queueing, and is
// inconclusive as before.
func unboundedBlocks(en report.Entry, held []string, mb, rb report.ModelBand, model bool, t float64) bool {
	u := en.Unbounded
	if u == nil || !(u.ChangeMs.Lo <= u.ChangeMs.Hi) {
		return false
	}
	for _, svc := range held {
		if !slices.Contains(u.Services, svc) {
			return false
		}
	}
	bl := 0.0
	if !model {
		bl, _ = biasOf(u.ChangeMs, u.BandMs, en.BeforeMs, rb)
	}
	band, _ := BandOf(mb, u.ChangeMs, en.BeforeMs, bl, 0)
	return band.Lo > t
}

// changedServices are the services the change items name, in item order, without repeats.
func (e *evaluator) changedServices() []string {
	var out []string
	seen := map[string]bool{}
	for _, it := range e.r.Items {
		if it.Op == change.OpLoadScale || seen[it.Service] {
			continue
		}
		seen[it.Service] = true
		out = append(out, it.Service)
	}
	return out
}

// measureLine is the decide_with line that names the measurement which would decide a service's change.
func (e *evaluator) measureLine(svc string, ev Entry) string {
	if v, ok := e.in.SafeBudgets[svc]; ok {
		return fmt.Sprintf("an A/B of %s (diff-fit): a measured delta passes if delta + 2 SE is at most %s ms per visit",
			e.safe(svc), fmtMs(v))
	}
	return fmt.Sprintf("an A/B of %s (diff-fit): a measured delta passes if the band at %s stays at or under %s ms",
		e.safe(svc), e.safe(ev.Name), fmtMs(ev.ThresholdMs))
}

// labels is rule 11: an item whose label is not trusted and not let through by an exception holds the verdict at
// inconclusive (unmeasured_delta); a block from rules 8 to 10 still blocks. In enforce mode a measured label whose
// transfer to production is not validated holds it too.
func (e *evaluator) labels() {
	byLabel := map[string]*ExceptionApplied{}
	for _, x := range e.p.Exceptions {
		if x.Rule == RuleUnmeasuredDelta {
			byLabel[x.Label] = &ExceptionApplied{Rule: x.Rule, Label: x.Label, Reason: x.Reason}
		}
	}
	var untrusted, untransferred []int
	validated := map[int]bool{}
	for _, l := range e.r.Labels {
		validated[l.Item] = l.TransferValidated
	}
	for i, it := range e.r.Items {
		switch {
		case e.p.Trusted(it.Label):
			if e.p.Enforce && !validated[i] && strings.HasPrefix(it.Label, "measured-") {
				untransferred = append(untransferred, i)
			}
		case byLabel[it.Label] != nil:
			byLabel[it.Label].Items = append(byLabel[it.Label].Items, i)
		default:
			untrusted = append(untrusted, i)
		}
	}
	for _, x := range e.p.Exceptions {
		if a := byLabel[x.Label]; x.Rule == RuleUnmeasuredDelta && a != nil && len(a.Items) > 0 {
			e.res.Exceptions = append(e.res.Exceptions, *a)
			byLabel[x.Label] = nil
		}
	}
	switch {
	case len(untrusted) > 0:
		r := e.fire("11", RuleUnmeasuredDelta, Inconclusive)
		r.Value = num(float64(len(untrusted)))
		// decide_with names the threshold of the entry whose band comes closest to it, or goes furthest over it
		var en *Entry
		margin := func(x *Entry) float64 {
			if x.BandMs == nil {
				return math.Inf(-1)
			}
			return x.BandMs.Hi - x.ThresholdMs
		}
		for i := range e.res.Entries {
			if en == nil || margin(&e.res.Entries[i]) > margin(en) {
				en = &e.res.Entries[i]
			}
		}
		seen := map[string]bool{}
		for _, i := range untrusted {
			svc := e.r.Items[i].Service
			if e.r.Items[i].Op == change.OpLoadScale || seen[svc] {
				continue
			}
			seen[svc] = true
			if en != nil {
				e.decide(e.measureLine(svc, *en))
			} else {
				e.decide("an A/B of " + e.safe(svc) + " (diff-fit) gives a measured delta")
			}
		}
	case len(untransferred) > 0:
		r := e.fire("11", RuleTransferUnvalidated, Inconclusive)
		r.Value = num(float64(len(untransferred)))
		e.decide("validate the transfer of measured deltas to production for this environment (the transfer study)")
	default:
		e.pass("11")
	}
}

// floor is rule 12: a threshold below the environment's A/A noise floor, once a floor exists (a readiness record with
// at least MinAAPairs pairs), unless an exception waives it.
func (e *evaluator) floor() {
	rd := e.in.Readiness
	if rd == nil || rd.AAFloor.Pairs < MinAAPairs {
		return // not evaluated: no floor exists
	}
	e.pass("12")
	for _, ev := range e.res.Entries {
		fl, ok := rd.AAFloor.FloorMs[ev.Name]
		if !ok || !(ev.ThresholdMs < fl) {
			continue
		}
		for _, x := range e.p.Exceptions {
			if x.Rule == RuleThresholdUnderFloor {
				e.res.Exceptions = append(e.res.Exceptions, ExceptionApplied{Rule: x.Rule, Reason: x.Reason})
				return
			}
		}
		r := e.fire("12", RuleThresholdUnderFloor, Inconclusive)
		r.Entry, r.Value, r.Threshold = e.safe(ev.Name), num(ev.ThresholdMs), num(fl)
		e.decide("raise the threshold for " + e.safe(ev.Name) + " above the A/A floor of " + fmtMs(fl) + " ms")
		return
	}
}

// advisory is rule 13: enforcement is earned from ledger records, which this version does not have, so every verdict
// is advisory and gate.enforce_not_earned is always listed.
func (e *evaluator) advisory() {
	r := e.fire("13", RuleAdvisory, EffectNote)
	if e.p.Enforce {
		r.Code = contract.GateEnforceNotEarned
	}
	e.finding(contract.NewFinding(contract.GateEnforceNotEarned))
	e.res.Advisory = true
}

func (e *evaluator) finish() *Result {
	e.res.Rules = e.rules
	v := Pass
	for _, r := range e.rules {
		if r.Evaluated && severity[r.Effect] > severity[v] {
			v = r.Effect
		}
	}
	if j := e.in.Judge; j != nil && j.Decision == "escalate" && v == Pass {
		// escalation turns a pass into inconclusive, never into a block (ARCHITECTURE.md 7.2)
		v = Inconclusive
		e.decide("a person reviews the change: the judge escalated")
	}
	e.res.Verdict = v
	e.res.ExitCode = map[string]int{Pass: ExitPass, Block: ExitBlock, Inconclusive: ExitInconclusive}[v]
	contract.SortFindings(e.res.Findings)
	return e.res
}

// Fired returns the rows that decide or inform: evaluated, with an effect other than pass.
func (r *Result) Fired() []Rule {
	var out []Rule
	for _, x := range r.Rules {
		if x.Evaluated && x.Effect != Pass {
			out = append(out, x)
		}
	}
	return out
}

// RuleRow returns the row of a rule number ("1" to "13", "8b").
func (r *Result) RuleRow(number string) Rule {
	for _, x := range r.Rules {
		if x.Number == number {
			return x
		}
	}
	return Rule{}
}

func num(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func afterHi(row report.Row) *float64 {
	if row.UtilizationAfter == nil {
		return nil
	}
	return num(row.UtilizationAfter.Hi)
}

// safe is a name that is not a row's as the gate prints it (sanitize.Service).
func safe(name string) string {
	s, _ := sanitize.Service(name)
	return s
}

// fmtMs prints milliseconds with at most 3 decimals.
func fmtMs(v float64) string {
	s := strconv.FormatFloat(v, 'f', 3, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "-0" {
		s = "0"
	}
	return s
}

func sameSet(a, b []string) bool {
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
