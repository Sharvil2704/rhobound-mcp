// SPDX-License-Identifier: Apache-2.0

// Package judge checks a judge model's answer to a judge packet (docs/mcp/ARCHITECTURE.md 7.2) and keeps only the
// typed fields that may return to the coding agent.
//
// A judge can say "this is it" (accept), "could be better" (revise, with typed operations that must be predicted
// again), or escalate, which the gate maps to inconclusive and never to a block. It never supplies a millisecond
// figure, never relaxes a verdict, never overrides a refusal, answers only the packet's own questions within their
// bounds, and cites only evidence the packet holds. Its free text is for people: it never reaches the agent, and
// nothing parses a number out of it.
package judge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
)

// The schema ids of the two documents.
const (
	AnswerSchema = "rhobound-mcp.judge-answer/v1"
	PacketSchema = "rhobound-mcp.judge-packet/v1"
)

// The decisions of an answer.
const (
	Accept   = "accept"
	Revise   = "revise"
	Escalate = "escalate"
)

// Answer is a judge answer (judge-answer.v1.json).
type Answer struct {
	Schema      string       `json:"schema"`
	Packet      string       `json:"packet"`
	Decision    string       `json:"decision"`
	Reasons     []Reason     `json:"reasons,omitempty"`
	Assumptions []Assumption `json:"assumptions,omitempty"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	Measure     []Measure    `json:"measure,omitempty"`
	Commentary  string       `json:"commentary,omitempty"`
}

// Reason is a reason code with the packet paths it rests on.
type Reason struct {
	Refs []string `json:"refs"`
	Code string   `json:"code"`
}

// Assumption is a structural range answering one of the packet's open questions.
type Assumption struct {
	Question   string        `json:"question"`
	Range      *change.Range `json:"range"`
	Evidence   []string      `json:"evidence"`
	Confidence string        `json:"confidence"`
}

// Suggestion is a revision, as change operations a new prediction must evaluate.
type Suggestion struct {
	Rank        int  `json:"rank"`
	ExpectedOps []Op `json:"expected_ops"`
}

// Op is a suggested change operation (defs.v1 ChangeItem). Its note is checked like every field, but it is free text
// and never returns to the agent.
type Op struct {
	Op      string        `json:"op"`
	Service string        `json:"service"`
	Callee  string        `json:"callee,omitempty"`
	Value   *change.Range `json:"value"`
	Note    string        `json:"note,omitempty"`
}

// Measure asks for a measurement of a service.
type Measure struct {
	Service string `json:"service"`
	Kind    string `json:"kind"`
	WhyCode string `json:"why_code"`
}

// Typed is what returns to the coding agent: the decision, the reason codes, the structural assumptions, the expected
// operations (without their notes) and the measurements asked for, with the digests of the packet and the answer.
type Typed struct {
	Packet      string       `json:"packet"`
	Answer      string       `json:"answer"`
	Provenance  string       `json:"provenance"`
	Decision    string       `json:"decision"`
	Reasons     []string     `json:"reasons"`
	Assumptions []Assumption `json:"assumptions"`
	ExpectedOps []Op         `json:"expected_ops"`
	Measure     []Measure    `json:"measure"`
}

// ShapeError is an answer or a packet that does not match its schema: not a coded refusal, since the document is not
// one the check can read (ARCHITECTURE.md 7.2 records such an answer as "judge unavailable").
type ShapeError struct{ Msg string }

func (e *ShapeError) Error() string { return e.Msg }

var (
	codeRE     = regexp.MustCompile(`^[a-z_]{1,40}$`)
	questionRE = regexp.MustCompile(`^Q[0-9]{1,3}$`)
	nameRE     = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)
	digestRE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// a millisecond figure: a number followed by a millisecond unit, in any typed string
	msRE = regexp.MustCompile(`(?i)[0-9]\s*(ms|msec|msecs|millis|millisecond|milliseconds)\b`)
	// reason codes that ask for a verdict to be relaxed
	loosenRE = regexp.MustCompile(`(^|_)(override|waive|relax|loosen|bypass|suppress|exempt|ignore)(_|$)|raise_threshold|widen_threshold|drop_band|accept_risk`)
	pathRE   = regexp.MustCompile(`^[a-z_]+(\[[0-9]+\])*(\.[a-z_]+(\[[0-9]+\])*)*$`)
)

// PacketDigest is the digest an answer cites: the sha256 of the packet's canonical JSON.
func PacketDigest(packet []byte) (string, error) {
	c, err := contract.CanonicalizeJSON(packet)
	if err != nil {
		return "", err
	}
	return contract.Digest(c), nil
}

// CheckAnswer checks an answer against the packet it answers. It returns the typed fields when the answer passes, and
// otherwise the refusals (judge.packet_mismatch, judge.ms_forbidden, judge.loosening, judge.answer_outside_question,
// judge.evidence_unresolved), at most one per check, in the order of the checks, with the first as an error, a
// *gate.Error. A document that does not match its
// schema is a *ShapeError. The provenance is client-asserted: only a CI step that obtained the answer from a pinned
// model may call it ci-judge, which it records in gate.json.
func CheckAnswer(packet, answer []byte) (*Typed, []contract.Finding, error) {
	var pk map[string]any
	dec := json.NewDecoder(bytes.NewReader(packet))
	dec.UseNumber()
	if err := dec.Decode(&pk); err != nil || pk == nil || pk["schema"] != PacketSchema {
		return nil, nil, &ShapeError{Msg: "judge-check: the packet is not a " + PacketSchema + " document"}
	}
	pd, err := PacketDigest(packet)
	if err != nil {
		return nil, nil, &ShapeError{Msg: "judge-check: the packet: " + err.Error()}
	}
	var a Answer
	if err := strictDecode(answer, &a); err != nil {
		return nil, nil, &ShapeError{Msg: "judge-check: the answer does not match " + AnswerSchema + ": " + err.Error()}
	}
	if err := shape(&a); err != nil {
		return nil, nil, err
	}
	ad, err := PacketDigest(answer)
	if err != nil {
		return nil, nil, &ShapeError{Msg: "judge-check: the answer: " + err.Error()}
	}

	var errs []*gate.Error
	refuse := func(code, subject, msg string) {
		f := contract.NewFinding(code)
		if subject != "" {
			f = f.WithSubject(subject)
		}
		errs = append(errs, &gate.Error{Finding: f, Msg: "judge-check: " + msg})
	}
	if a.Packet != pd {
		refuse(contract.JudgePacketMismatch, "", "the answer cites "+a.Packet+", not the packet sent ("+pd+")")
	} else {
		// the other checks are against the packet the answer cites
		checkMs(&a, refuse)
		checkLoosening(&a, pk, refuse)
		checkQuestions(&a, pk, refuse)
		checkEvidence(&a, pk, refuse)
	}
	if len(errs) > 0 {
		// in the order of the checks, the first being the refusal
		fs := make([]contract.Finding, len(errs))
		for i, e := range errs {
			fs[i] = e.Finding
		}
		return nil, fs, errs[0]
	}
	t := &Typed{Packet: pd, Answer: ad, Provenance: gate.ProvenanceClientAsserted, Decision: a.Decision,
		Reasons: []string{}, Assumptions: []Assumption{}, ExpectedOps: []Op{}, Measure: []Measure{}}
	for _, r := range a.Reasons {
		t.Reasons = append(t.Reasons, r.Code)
	}
	t.Assumptions = append(t.Assumptions, a.Assumptions...)
	for _, s := range a.Suggestions {
		for _, op := range s.ExpectedOps {
			op.Note = ""
			t.ExpectedOps = append(t.ExpectedOps, op)
		}
	}
	t.Measure = append(t.Measure, a.Measure...)
	return t, nil, nil
}

// Commentary returns an answer's free text, for summary.md only, and its decision; it reads nothing else.
func Commentary(answer []byte) string {
	var a struct {
		Commentary string `json:"commentary"`
	}
	if json.Unmarshal(answer, &a) != nil {
		return ""
	}
	return a.Commentary
}

func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("trailing data")
	}
	return nil
}

// shape checks what judge-answer.v1.json says beyond the keys: enums, patterns, lengths, counts.
func shape(a *Answer) error {
	bad := func(what string) error {
		return &ShapeError{Msg: "judge-check: the answer does not match " + AnswerSchema + ": " + what}
	}
	switch {
	case a.Schema != AnswerSchema:
		return bad("schema must be " + AnswerSchema)
	case !digestRE.MatchString(a.Packet):
		return bad("packet must be a sha256: digest")
	case a.Decision != Accept && a.Decision != Revise && a.Decision != Escalate:
		return bad("decision must be accept, revise or escalate")
	case len(a.Reasons) > 16 || len(a.Assumptions) > 16 || len(a.Suggestions) > 8 || len(a.Measure) > 8:
		return bad("too many reasons, assumptions, suggestions or measurements")
	case len(a.Commentary) > 2048:
		return bad("commentary is longer than 2048 characters")
	}
	for _, r := range a.Reasons {
		if !codeRE.MatchString(r.Code) || r.Refs == nil || len(r.Refs) > 8 || !shortAll(r.Refs) {
			return bad("a reason is {refs: at most 8 paths, code: [a-z_]}")
		}
	}
	for _, x := range a.Assumptions {
		if !questionRE.MatchString(x.Question) || x.Range == nil || x.Evidence == nil || len(x.Evidence) > 8 ||
			!shortAll(x.Evidence) || (x.Confidence != "low" && x.Confidence != "medium" && x.Confidence != "high") {
			return bad("an assumption is {question: Qn, range, evidence: at most 8, confidence: low, medium or high}")
		}
	}
	for _, s := range a.Suggestions {
		if s.Rank < 1 || s.ExpectedOps == nil || len(s.ExpectedOps) > 16 {
			return bad("a suggestion is {rank >= 1, expected_ops: at most 16}")
		}
		for _, op := range s.ExpectedOps {
			known := false
			for _, o := range change.Ops {
				known = known || o == op.Op
			}
			if !known || !nameRE.MatchString(op.Service) || (op.Callee != "" && !nameRE.MatchString(op.Callee)) ||
				op.Value == nil || len(op.Note) > 200 {
				return bad("an expected op is a change item {op, service, callee?, value, note?}")
			}
		}
	}
	for _, m := range a.Measure {
		if !nameRE.MatchString(m.Service) || (m.Kind != "ab" && m.Kind != "profile") || !codeRE.MatchString(m.WhyCode) {
			return bad("a measurement is {service, kind: ab or profile, why_code}")
		}
	}
	return nil
}

func shortAll(xs []string) bool {
	for _, x := range xs {
		if len(x) > 100 {
			return false
		}
	}
	return true
}

// checkMs refuses any millisecond figure in a typed field: an own_ms or glue_ms operation is one by its unit, and a
// number with a millisecond unit in any string the answer types is one too (judge.ms_forbidden).
func checkMs(a *Answer, refuse func(code, subject, msg string)) {
	for _, s := range a.Suggestions {
		for _, op := range s.ExpectedOps {
			if op.Op == change.OpOwnMs || op.Op == change.OpGlueMs {
				refuse(contract.JudgeMsForbidden, op.Op, "an expected "+op.Op+" operation supplies milliseconds")
				return
			}
			if msRE.MatchString(op.Note) {
				refuse(contract.JudgeMsForbidden, "", "an expected operation's note holds a millisecond figure")
				return
			}
		}
	}
	var typed []string
	for _, r := range a.Reasons {
		typed = append(typed, r.Refs...)
	}
	for _, x := range a.Assumptions {
		typed = append(typed, x.Evidence...)
	}
	for _, s := range typed {
		if msRE.MatchString(s) {
			refuse(contract.JudgeMsForbidden, "", "a typed field holds a millisecond figure")
			return
		}
	}
}

// checkLoosening refuses an answer that would relax a verdict (judge.loosening): accepting a change the preview
// blocked, accepting over a refusal in the packet's findings, or a reason code that asks for a rule to be waived,
// overridden or relaxed. A judge may turn a pass into an inconclusive (escalate), and nothing else.
func checkLoosening(a *Answer, pk map[string]any, refuse func(code, subject, msg string)) {
	pred, _ := pk["prediction"].(map[string]any)
	if a.Decision == Accept {
		if v, _ := pred["preview"].(string); v == gate.Block {
			refuse(contract.JudgeLoosening, "", "the answer accepts a change the preview blocked")
			return
		}
		fs, _ := pk["findings"].([]any)
		for _, f := range fs {
			code, _ := f.(string)
			if c, ok := contract.Lookup(code); ok && c.Level == contract.Refuse {
				refuse(contract.JudgeLoosening, code, "the answer accepts over a refusal")
				return
			}
		}
	}
	for _, r := range a.Reasons {
		if loosenRE.MatchString(r.Code) {
			refuse(contract.JudgeLoosening, r.Code, "the reason "+r.Code+" asks for a rule to be relaxed")
			return
		}
	}
}

// checkQuestions refuses a range for a question the packet did not ask, an inverted range, or one outside the
// question's bounds (judge.answer_outside_question).
func checkQuestions(a *Answer, pk map[string]any, refuse func(code, subject, msg string)) {
	asked := map[string]map[string]any{}
	qs, _ := pk["open_questions"].([]any)
	for _, q := range qs {
		if m, ok := q.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				asked[id] = m
			}
		}
	}
	for _, x := range a.Assumptions {
		q, ok := asked[x.Question]
		switch {
		case !ok:
			refuse(contract.JudgeAnswerOutsideQuestion, x.Question, "the packet asks no question "+x.Question)
			return
		case x.Range.Lo > x.Range.Hi:
			refuse(contract.JudgeAnswerOutsideQuestion, x.Question, "the range for "+x.Question+" is inverted")
			return
		}
		if b, ok := q["bounds"].(map[string]any); ok {
			lo, lok := num(b["lo"])
			hi, hok := num(b["hi"])
			if (lok && x.Range.Lo < lo) || (hok && x.Range.Hi > hi) {
				refuse(contract.JudgeAnswerOutsideQuestion, x.Question, "the range for "+x.Question+" is outside its bounds")
				return
			}
		}
	}
}

// checkEvidence refuses a reference that does not resolve to a value of the packet: every reason's refs and every
// assumption's evidence are paths into it, such as prediction.entries[0] or diff_excerpt (judge.evidence_unresolved).
func checkEvidence(a *Answer, pk map[string]any, refuse func(code, subject, msg string)) {
	var refs []string
	for _, r := range a.Reasons {
		refs = append(refs, r.Refs...)
	}
	for _, x := range a.Assumptions {
		refs = append(refs, x.Evidence...)
	}
	for _, ref := range refs {
		if !Resolves(pk, ref) {
			refuse(contract.JudgeEvidenceUnresolved, "", "the reference "+strconv.Quote(safeRef(ref))+" is not in the packet")
			return
		}
	}
}

// Resolves reports whether a path (key, key[i], key.key...) names a value of a decoded JSON document.
func Resolves(doc map[string]any, path string) bool {
	if !pathRE.MatchString(path) {
		return false
	}
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		key, rest, _ := strings.Cut(seg, "[")
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[key]; !ok {
			return false
		}
		for rest != "" {
			idx, after, _ := strings.Cut(rest, "]")
			rest = strings.TrimPrefix(after, "[")
			i, err := strconv.Atoi(idx)
			arr, ok := cur.([]any)
			if err != nil || !ok || i < 0 || i >= len(arr) {
				return false
			}
			cur = arr[i]
		}
	}
	return true
}

func num(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

// safeRef is a reference as an error message prints it: a path, or a hash of whatever else it was.
func safeRef(s string) string {
	if pathRE.MatchString(s) && len(s) <= 100 {
		return s
	}
	n, _ := contract.SafeName(s)
	return n
}
