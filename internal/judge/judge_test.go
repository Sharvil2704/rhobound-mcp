// SPDX-License-Identifier: Apache-2.0

package judge

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
)

// packet is a judge packet of a blocked preview, with one open question.
const packet = `{"schema": "rhobound-mcp.judge-packet/v1", "scope": "mean-only/v1",
 "role_rules": ["never supply milliseconds"],
 "prediction": {"entries": [{"name": "a", "before_ms": 45.22, "change_ms": {"lo": 4.1, "hi": 7.15},
   "band_ms": {"lo": 2.93, "hi": 8.32}, "threshold_ms": 2.261}],
   "moved": [{"service": "b", "utilization_before": 0.26, "utilization_after": {"lo": 0.3, "hi": 0.33}}],
   "preview": "block", "why": ["mean_change: band low end 2.93 ms above threshold 2.261 ms at a"],
   "evidence_class": "simulated-70-80"},
 "budgets": [{"service": "b", "kind": "own_ms", "per": "visit", "max_value": 1.9494, "safe_value": 1.0701}],
 "leverage": [{"service": "b", "entry": "a", "ms_per_ms": 1.1531}],
 "labels": ["declared"], "findings": ["load.busiest_unknown"], "assumptions": [],
 "codepath_cards": [], "open_questions": [{"id": "Q1", "kind": "count", "ask": "items per order", "bounds": {"lo": 1, "hi": 50}}],
 "policy": {"name": "default", "digest": "sha256:` + "0000000000000000000000000000000000000000000000000000000000000000" + `"},
 "answer_schema": "rhobound-mcp.judge-answer/v1"}`

func answer(t *testing.T, body string) []byte {
	t.Helper()
	d, err := PacketDigest([]byte(packet))
	if err != nil {
		t.Fatal(err)
	}
	return []byte(strings.ReplaceAll(body, "PACKET", d))
}

// the canned answer of the loop fixture: revise, one expected operation
const revise = `{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "revise",
 "reasons": [{"refs": ["prediction.entries[0]", "budgets[0]"], "code": "over_budget"}],
 "assumptions": [{"question": "Q1", "range": {"lo": 1, "hi": 12}, "evidence": ["open_questions[0]"], "confidence": "low"}],
 "suggestions": [{"rank": 1, "expected_ops": [{"op": "scale_count", "service": "b", "callee": "c", "value": 1, "note": "batch the lookups"}]}],
 "measure": [{"service": "b", "kind": "ab", "why_code": "own_time_unknown"}],
 "commentary": "Batch the lookups: one call per order, not per item. 3 ms is my guess."}`

func TestCheckAnswerValid(t *testing.T) {
	typed, fs, err := CheckAnswer([]byte(packet), answer(t, revise))
	if err != nil || fs != nil {
		t.Fatal(err, fs)
	}
	if typed.Decision != Revise || len(typed.Reasons) != 1 || typed.Reasons[0] != "over_budget" ||
		len(typed.ExpectedOps) != 1 || typed.ExpectedOps[0].Note != "" || typed.ExpectedOps[0].Value.Lo != 1 ||
		len(typed.Measure) != 1 || len(typed.Assumptions) != 1 || typed.Provenance != gate.ProvenanceClientAsserted {
		t.Fatalf("%+v", typed)
	}
	// only typed fields return: the commentary and the notes never do
	b, err := contract.Canonical(typed)
	if err != nil || strings.Contains(string(b), "Batch") || strings.Contains(string(b), "commentary") {
		t.Fatalf("%s %v", b, err)
	}
	if !strings.Contains(Commentary(answer(t, revise)), "one call per order") {
		t.Fatal("commentary")
	}
	// the digest is over the canonical bytes: whitespace and key order do not matter
	d1, _ := PacketDigest([]byte(packet))
	d2, _ := PacketDigest([]byte(strings.ReplaceAll(packet, "\n", "")))
	if d1 != d2 {
		t.Fatal("digest depends on whitespace")
	}
}

// One defective answer per judge code.
func TestCheckAnswerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
	}{
		{"another packet", strings.Replace(revise, "PACKET", "sha256:"+strings.Repeat("f", 64), 1), contract.JudgePacketMismatch},
		{"an own_ms operation", strings.Replace(revise, `"op": "scale_count"`, `"op": "own_ms"`, 1), contract.JudgeMsForbidden},
		{"a glue_ms operation", strings.Replace(revise, `"op": "scale_count"`, `"op": "glue_ms"`, 1), contract.JudgeMsForbidden},
		{"milliseconds in a reference", strings.Replace(revise, `"budgets[0]"`, `"budgets[0] is 2 ms"`, 1), contract.JudgeMsForbidden},
		{"milliseconds in a note", strings.Replace(revise, `"batch the lookups"`, `"saves 1.5ms"`, 1), contract.JudgeMsForbidden},
		{"accepting a blocked change", strings.Replace(revise, `"decision": "revise"`, `"decision": "accept"`, 1), contract.JudgeLoosening},
		{"a reason that waives a rule", strings.Replace(revise, `"over_budget"`, `"waive_threshold"`, 1), contract.JudgeLoosening},
		{"an unasked question", strings.Replace(revise, `"question": "Q1"`, `"question": "Q7"`, 1), contract.JudgeAnswerOutsideQuestion},
		{"a range outside the bounds", strings.Replace(revise, `"hi": 12}`, `"hi": 80}`, 1), contract.JudgeAnswerOutsideQuestion},
		{"an inverted range", strings.Replace(revise, `{"lo": 1, "hi": 12}`, `{"lo": 12, "hi": 1}`, 1), contract.JudgeAnswerOutsideQuestion},
		{"evidence the packet does not hold", strings.Replace(revise, `"open_questions[0]"`, `"diff_excerpt"`, 1), contract.JudgeEvidenceUnresolved},
		{"a reference out of range", strings.Replace(revise, `"budgets[0]"`, `"budgets[3]"`, 1), contract.JudgeEvidenceUnresolved},
		{"a reference that is not a path", strings.Replace(revise, `"budgets[0]"`, `"see the diff"`, 1), contract.JudgeEvidenceUnresolved},
	} {
		_, fs, err := CheckAnswer([]byte(packet), answer(t, tc.body))
		var ge *gate.Error
		if !errors.As(err, &ge) || ge.Finding.Code != tc.code || len(fs) == 0 || fs[0].Code != tc.code ||
			ge.Refusal().Fix == "" {
			t.Errorf("%s: %v %v, want %s", tc.name, err, fs, tc.code)
		}
	}
}

func TestCheckAnswerShape(t *testing.T) {
	for _, body := range []string{
		`{"schema": "rhobound-mcp.judge-answer/v0", "packet": "PACKET", "decision": "revise"}`,
		`{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "ship it"}`,
		`{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "revise", "verdict": "pass"}`,
		`{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "revise", "reasons": [{"refs": [], "code": "Over Budget"}]}`,
		`{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "revise", "measure": [{"service": "b", "kind": "guess", "why_code": "x"}]}`,
		`{"schema": "rhobound-mcp.judge-answer/v1", "packet": "PACKET", "decision": "revise", "suggestions": [{"rank": 1, "expected_ops": [{"op": "rewrite", "service": "b", "value": 1}]}]}`,
		`not json`,
	} {
		_, _, err := CheckAnswer([]byte(packet), answer(t, body))
		var se *ShapeError
		if !errors.As(err, &se) {
			t.Errorf("%s: %v", body, err)
		}
	}
	if _, _, err := CheckAnswer([]byte(`{"schema": "x"}`), answer(t, revise)); err == nil {
		t.Error("a packet of another schema was read")
	}
	// escalate and accept on a pass are allowed
	pass := strings.Replace(packet, `"preview": "block"`, `"preview": "pass"`, 1)
	d, _ := PacketDigest([]byte(pass))
	for _, dec := range []string{Accept, Escalate} {
		body := `{"schema": "rhobound-mcp.judge-answer/v1", "packet": "` + d + `", "decision": "` + dec + `"}`
		if typed, _, err := CheckAnswer([]byte(pass), []byte(body)); err != nil || typed.Decision != dec {
			t.Errorf("%s: %v", dec, err)
		}
	}
}

func TestResolves(t *testing.T) {
	doc := map[string]any{"a": []any{map[string]any{"b": 1}}, "c": map[string]any{"d": []any{[]any{2}}}}
	for path, want := range map[string]bool{"a": true, "a[0]": true, "a[0].b": true, "a[1]": false, "c.d[0][0]": true,
		"c.d[0][1]": false, "c.e": false, "a.b": false, "": false, "a[-1]": false, "A": false} {
		if got := Resolves(doc, path); got != want {
			t.Errorf("%q: %v", path, got)
		}
	}
}
