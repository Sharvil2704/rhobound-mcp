// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server/schemas"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// tool calls a tool and returns the result's text body (the exact bytes), its structuredContent as the client decoded
// it, and whether it is an error result. A protocol error fails the test.
func (h *realHarness) tool(name, args string) ([]byte, any, bool) {
	h.t.Helper()
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		h.t.Fatal(err)
	}
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		h.t.Fatalf("%s %s: %v", name, args, err)
	}
	if len(res.Content) != 1 {
		h.t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text := res.Content[0].(*mcp.TextContent).Text
	body, ok := strings.CutSuffix(text, "\n"+ScopeSentence)
	if !ok {
		h.t.Fatalf("%s: the text block does not end with the scope sentence", name)
	}
	return []byte(body), res.StructuredContent, res.IsError
}

// outputSchema is the published file of each tool's output.
var outputSchema = map[string]string{"context": "context.v1.json", "budget": "budget.v1.json",
	"predict": "prediction.v1.json", "explain": "explain.v1.json", "fit_start": "fit-start.v1.json",
	"job_status": "job-status.v1.json"}

// validOutput checks a tool's result against its published output schema.
func validOutput(t *testing.T, tool string, body []byte) {
	t.Helper()
	sch, err := schemas.Bundle(outputSchema[tool], "/$defs/output")
	if err != nil {
		t.Fatal(err)
	}
	rs, err := schemas.Resolve(sch)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if err := rs.Validate(v); err != nil {
		t.Errorf("%s: the result does not match %s: %v\n%s", tool, outputSchema[tool], err, body)
	}
}

// view calls a view tool, which must answer with a result, not a refusal, within the size of its detail level and
// valid against its published output schema.
func (h *realHarness) view(name, args string, max int) ([]byte, any) {
	h.t.Helper()
	body, sc, isErr := h.tool(name, args)
	if isErr {
		h.t.Fatalf("%s %s: %s", name, args, body)
	}
	if len(body) > max {
		h.t.Errorf("%s: structuredContent is %d bytes, over %d", name, len(body), max)
	}
	h.t.Logf("%s: %d bytes (cap %d)", name, len(body), max)
	validOutput(h.t, name, body)
	return body, sc
}

// wait runs fit_start and job_status until the job leaves working, and returns job_status's result.
func (h *realHarness) wait(args string) []byte {
	h.t.Helper()
	b, _, isErr := h.tool("fit_start", args)
	if isErr || field(h.t, b, "state") != "working" {
		h.t.Fatalf("fit_start %s: %s", args, b)
	}
	validOutput(h.t, "fit_start", b)
	job := field(h.t, b, "job_id").(string)
	for i := 0; i < 10; i++ {
		st, _, isErr := h.tool("job_status", `{"job_id":"`+job+`","wait_s":20}`)
		if isErr {
			h.t.Fatalf("job_status: %s", st)
		}
		validOutput(h.t, "job_status", st)
		if field(h.t, st, "state") != "working" {
			return st
		}
	}
	h.t.Fatal("the job is still working after 200 s")
	return nil
}

// obj and arr read decoded JSON.
func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object", v)
	}
	return m
}

func arr(t *testing.T, v any) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%v is not an array", v)
	}
	return a
}

// number reads a number of a document decoded by decodeObject (a json.Number).
func number(t *testing.T, v any) float64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("%v is not a number", v)
	}
	f, err := n.Float64()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func near(t *testing.T, what string, got any, want, tol float64) {
	t.Helper()
	if g := number(t, got); math.Abs(g-want) > tol {
		t.Errorf("%s = %v, want %v within %v", what, g, want, tol)
	}
}

// ruleEffects are a preview's rules, by rule token.
func ruleEffects(t *testing.T, preview map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, r := range arr(t, preview["rules"]) {
		m := obj(t, r)
		out[m["rule"].(string)] = m["effect"].(string)
	}
	return out
}

// TestScriptedAgent is the scripted agent (testdata/mcp/README.md): no model, the real CLI, the loop fixture, the
// workers pinned a=1, b=2, c=1 and a 5% policy, in both protocol eras through the SDK client. It follows exactly
// fit_start, job_status, context, budget for b, a declared 3.5..6 ms on b (block), the judge packet and judge-check on
// the canned answer, a declared +0.4 ms (inconclusive, unmeasured_delta), fit_start of the A/B pair, and the measured
// preview (pass). The verdicts are exact; the numbers are those measured on the CLI before this server existed, at the
// tolerance of the budget's bisection. Every view tool's result is checked against its CLI twin (parity) and against
// its size cap.
func TestScriptedAgent(t *testing.T) {
	fx := loop(t)
	for _, era := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(era, func(t *testing.T) { scriptedAgent(t, fx, era) })
	}
}

func scriptedAgent(t *testing.T, fx loopFixture, era string) {
	now := fx.windowEnd.Add(time.Hour)
	h := realServer(t, map[string]string{"loop": fx.capture, "loop-a": fx.baseline, "loop-b": fx.candidate},
		fx.policy, now, era)
	if got := h.cs.InitializeResult().ProtocolVersion; got != era {
		t.Fatalf("negotiated %q, want %q", got, era)
	}

	// fit_start with the workers pinned, then job_status
	st := h.wait(`{"capture":"loop","workers":{"a":1,"b":2,"c":1}}`)
	if field(t, st, "state") != "completed" || field(t, st, "step") != "fit" {
		t.Fatalf("fit job: %s", st)
	}
	fit := field(t, st, "fit_id").(string)
	if f, _ := field(t, st, "findings").([]any); len(f) != 0 {
		t.Errorf("the loop capture is admitted with findings: %v", f)
	}
	if fw := strings.Join(h.rec.last(t, "fit").args, " "); !strings.Contains(fw, "--workers=a=1,b=2,c=1") || !strings.Contains(fw, "--name=loop") {
		t.Errorf("fit args %q", fw)
	}

	// context: b's leverage on a, its pinned workers, the fit's age at the pinned clock
	body, sc := h.view("context", `{"fit_id":"`+fit+`"}`, SummaryMaxBytes)
	h.parity("context", "context", body, sc)
	ctxView, _ := decodeObject(body)
	if ctxView["fit_id"] != fit || number(t, ctxView["fit_age_s"]) != 3600 ||
		!reflect.DeepEqual(ctxView["links"], []any{fitURI + fit}) {
		t.Errorf("context server keys: %s", body)
	}
	var b map[string]any
	for _, s := range arr(t, ctxView["services"]) {
		if m := obj(t, s); m["name"] == "b" {
			b = m
		}
	}
	if b == nil || b["workers"] != "2" {
		t.Fatalf("context: service b %v", b)
	}
	near(t, "leverage of b on a", obj(t, b["leverage"])["a"], 1.1531, 1e-4)

	// budget for b: about 1.95 ms per visit, safe about 1.07 ms
	body, sc = h.view("budget", `{"fit_id":"`+fit+`","service":"b"}`, SummaryMaxBytes)
	h.parity("budget", "budget", body, sc)
	bud, _ := decodeObject(body)
	item := obj(t, arr(t, bud["items"])[0])
	if item["service"] != "b" || item["binding_constraint"] != "max_entry_mean_change_pct" || item["binding_entry"] != "a" {
		t.Errorf("budget item %v", item)
	}
	near(t, "budget max_value", item["max_value"], 1.95, 0.01)
	near(t, "budget safe_value", item["safe_value"], 1.07, 0.01)

	predict := func(args string, attempt int) (map[string]any, map[string]any, map[string]any) {
		t.Helper()
		body, sc := h.view("predict", args, SummaryMaxBytes)
		h.parity("predict", "preview", body, sc)
		v, _ := decodeObject(body)
		if v["fit_id"] != fit || number(t, v["attempt"]) != float64(attempt) || number(t, v["max_preview_attempts"]) != 5 ||
			v["escalate"] != nil || !state.ValidHandle(v["prediction_id"].(string)) {
			t.Errorf("predict server keys: %s", body)
		}
		// links go first when a summary would pass its cap (ARCHITECTURE.md 5.9)
		if l, ok := v["links"]; ok && !reflect.DeepEqual(l, []any{predictionURI + v["prediction_id"].(string)}) {
			t.Errorf("predict links: %v", l)
		}
		p := obj(t, v["preview"])
		if p["advisory"] != true {
			t.Errorf("a preview that is not advisory: %v", p)
		}
		return v, p, obj(t, arr(t, v["entries"])[0])
	}

	// draft 1, declared 3.5..6 ms per visit of b: block
	v, p, e := predict(`{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6.0}}],`+
		`"repo":"shop","branch":"reserve-stock"}`, 1)
	if p["verdict"] != "block" || ruleEffects(t, p)["mean_change"] != "block" || ruleEffects(t, p)["unmeasured_delta"] != "inconclusive" {
		t.Fatalf("draft 1: %v", p)
	}
	near(t, "draft 1 change lo", obj(t, e["change_ms"])["lo"], 4.10, 0.05)
	near(t, "draft 1 change hi", obj(t, e["change_ms"])["hi"], 7.15, 0.05)
	near(t, "draft 1 band lo", obj(t, e["band_ms"])["lo"], 3.07, 0.05)
	near(t, "draft 1 band hi", obj(t, e["band_ms"])["hi"], 8.32, 0.05)
	near(t, "threshold", e["threshold_ms"], 2.261, 0.001)
	pred1 := v["prediction_id"].(string)

	// the judge packet of draft 1, and judge-check on the canned answer: only typed fields come back
	ex, sc := h.view("explain", `{"target":"`+pred1+`","section":"judge_packet","max_chars":8000}`, FullMaxBytes)
	h.parity("explain", "explain", ex, sc)
	exv, _ := decodeObject(ex)
	if exv["truncated"] != false {
		t.Fatalf("the judge packet is truncated: %s", ex)
	}
	packet, err := encode(exv["content"])
	if err != nil {
		t.Fatal(err)
	}
	pk, _ := decodeObject(packet)
	if obj(t, pk["prediction"])["preview"] != "block" || obj(t, pk["policy"])["name"] != "default" {
		t.Errorf("judge packet: %s", packet)
	}
	if res := h.readResource(packetURI + pred1); res != string(packet) {
		t.Errorf("the judge-packet resource differs from explain's:\n%s\n%s", res, packet)
	}
	typed := judgeCheck(t, fx.answer, packet)
	if typed["decision"] != "revise" || len(arr(t, typed["expected_ops"])) != 1 || typed["provenance"] != "client-asserted" ||
		bytes.Contains(mustEncode(t, typed), []byte("commentary")) {
		t.Fatalf("judge-check: %v", typed)
	}

	// draft 2, declared +0.4 ms: inconclusive with unmeasured_delta; rule 10's own outcome is a pass; decide_with names
	// an A/B of b
	_, p, e = predict(`{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}],`+
		`"repo":"shop","branch":"reserve-stock"}`, 2)
	r := ruleEffects(t, p)
	if p["verdict"] != "inconclusive" || r["unmeasured_delta"] != "inconclusive" || r["mean_change"] != "pass" || len(r) != 2 ||
		!strings.HasPrefix(arr(t, p["decide_with"])[0].(string), "an A/B of b") {
		t.Fatalf("draft 2: %v", p)
	}
	near(t, "draft 2 change", obj(t, e["change_ms"])["hi"], 0.46, 0.05)
	near(t, "draft 2 band lo", obj(t, e["band_ms"])["lo"], -0.57, 0.05)
	near(t, "draft 2 band hi", obj(t, e["band_ms"])["hi"], 1.49, 0.05)

	// the A/B run of the two images, then the measured preview: pass, advisory
	st = h.wait(`{"ab":{"baseline":"loop-a","candidate":"loop-b"}}`)
	if field(t, st, "state") != "completed" || field(t, st, "step") != "diff" {
		t.Fatalf("A/B job: %s", st)
	}
	ab := field(t, st, "ab_id").(string)
	if dw := strings.Join(h.rec.last(t, "diff-fit").args, " "); !strings.Contains(dw, "--baseline-name=loop-a --candidate-name=loop-b") {
		t.Errorf("diff-fit args %q", dw)
	}
	for _, sec := range []string{"assumptions", "rules", "judge_packet"} {
		args := `{"target":"` + ab + `","section":"` + sec + `"}`
		if _, _, err := h.harness.try("explain", args); protocolCode(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("explain %s of an A/B handle: %v", sec, err)
		}
	}
	v, p, e = predict(`{"fit_id":"`+fit+`","measured":[{"ab":"`+ab+`"}],"repo":"shop","branch":"reserve-stock"}`, 3)
	r = ruleEffects(t, p)
	if p["verdict"] != "pass" || r["mean_change"] != "pass" || r["unmeasured_delta"] != "" || v["weakest_label"] != "measured-ab" {
		t.Fatalf("measured: %v", v)
	}
	// the pass has a margin of about 0.002 ms under the 2.261 ms threshold (the A/B record measures b at 0.355 +- 0.32
	// ms, not the 0.4 ms injected; testdata/mcp/README.md; and since the variance patch the prediction also applies
	// b's measured own-time variance difference +- 2 standard errors, which widened the band from -1.36..2.18):
	// pinned here, so drift shows before the verdict flips
	near(t, "measured band lo", obj(t, e["band_ms"])["lo"], -1.4332, 0.01)
	near(t, "measured band hi", obj(t, e["band_ms"])["hi"], 2.2603, 0.01)
	// the measured change set is b's delta alone: a and c ran the same image in both arms
	var set struct {
		Items []struct {
			Service, Label string
			Evidence       []string
		} `json:"items"`
	}
	if err := json.Unmarshal(h.rec.last(t, "preview").files[changesFile], &set); err != nil || len(set.Items) != 1 ||
		set.Items[0].Service != "b" || set.Items[0].Label != "measured-ab" || len(set.Items[0].Evidence) != 1 {
		t.Errorf("measured change set: %+v %v", set, err)
	}
}

// TestScriptedAgentStaleFit is the loop on a fit two days past its window, as a user meets it when the clock is not
// pinned: fit_stale holds every preview at block or inconclusive, adding a rule, a decide_with line and a doubt, and
// every summary still fits its cap with the server's keys.
func TestScriptedAgentStaleFit(t *testing.T) {
	fx := loop(t)
	h := realServer(t, map[string]string{"loop": fx.capture, "loop-a": fx.baseline, "loop-b": fx.candidate},
		fx.policy, fx.windowEnd.Add(48*time.Hour), "")
	fit := field(t, h.wait(`{"capture":"loop","workers":{"a":1,"b":2,"c":1}}`), "fit_id").(string)
	ab := field(t, h.wait(`{"ab":{"baseline":"loop-a","candidate":"loop-b"}}`), "ab_id").(string)
	body, sc := h.view("context", `{"fit_id":"`+fit+`"}`, SummaryMaxBytes)
	h.parity("context", "context", body, sc)
	body, sc = h.view("budget", `{"fit_id":"`+fit+`","service":"b"}`, SummaryMaxBytes)
	h.parity("budget", "budget", body, sc)
	for _, c := range []struct{ args, verdict string }{
		{`"changes":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6.0}}]`, "block"},
		{`"changes":[{"op":"own_ms","service":"b","value":0.4}]`, "inconclusive"},
		{`"measured":[{"ab":"` + ab + `"}]`, "inconclusive"},
	} {
		body, sc := h.view("predict", `{"fit_id":"`+fit+`",`+c.args+`}`, SummaryMaxBytes)
		h.parity("predict", "preview", body, sc)
		v, _ := decodeObject(body)
		p := obj(t, v["preview"])
		if p["verdict"] != c.verdict || ruleEffects(t, p)["fit_stale"] != "inconclusive" || v["fit_id"] != fit ||
			!state.ValidHandle(v["prediction_id"].(string)) || v["attempt"] == nil {
			t.Errorf("stale fit, %s: %s", c.args, body)
		}
	}
}

// judgeCheck runs `rhobound judge-check` on an answer to the packet, as the agent's host would on a judge's reply. The
// canned answer cites the packet by digest (sha256 of its canonical JSON), so the packet the preview emits must be the
// one it was written for.
func judgeCheck(t *testing.T, answer, packet []byte) map[string]any {
	t.Helper()
	ans, err := decodeObject(answer)
	if err != nil {
		t.Fatal(err)
	}
	if live := state.Digest(packet); ans["packet"] != live {
		// the committed answer cites the packet of linux/arm64, where it was computed: the packet's numbers are rounded
		// to 4 decimals, and the last bits of floating point may differ on another architecture
		if runtime.GOARCH == "arm64" {
			t.Fatalf("the canned answer cites the packet %v; the preview emitted %s:\n%s", ans["packet"], live, packet)
		}
		t.Logf("judge-answer.json is pinned on arm64; on %s the answer is checked against the live packet %s", runtime.GOARCH, live)
		ans["packet"] = live
		if answer, err = encode(ans); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	ap, pp := filepath.Join(dir, "answer.json"), filepath.Join(dir, "packet.json")
	if os.WriteFile(ap, answer, 0o600) != nil || os.WriteFile(pp, packet, 0o600) != nil {
		t.Fatal("cannot write the judge files")
	}
	cmd := exec.Command(real.bin, "judge-check", ap, "--packet", pp, "--json")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("judge-check: %v: %s", err, out)
	}
	typed, err := decodeObject(out)
	if err != nil {
		t.Fatal(err)
	}
	return typed
}

func mustEncode(t *testing.T, v any) []byte {
	t.Helper()
	b, err := encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// readResource reads a resource and returns its one text content.
func (h *realHarness) readResource(uri string) string {
	h.t.Helper()
	res, err := h.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		h.t.Fatalf("read %s: %v", uri, err)
	}
	if len(res.Contents) != 1 || res.Contents[0].MIMEType != jsonMIME {
		h.t.Fatalf("read %s: %+v", uri, res.Contents)
	}
	return res.Contents[0].Text
}

// TestRealResourcesAndPrompts reads every resource and gets both prompts on the real CLI: the cache hints of
// ARCHITECTURE.md 5.6, handles bound to their principal, and the judge prompt carrying the same packet as explain.
func TestRealResourcesAndPrompts(t *testing.T) {
	fx := loop(t)
	h := realServer(t, map[string]string{"loop": fx.capture}, fx.policy, fx.windowEnd.Add(time.Hour), "")
	st := h.wait(`{"capture":"loop","workers":{"a":1,"b":2,"c":1}}`)
	fit := field(t, st, "fit_id").(string)
	body, _ := h.view("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`, SummaryMaxBytes)
	pred := field(t, body, "prediction_id").(string)
	ctx := context.Background()

	list, err := h.cs.ListResources(ctx, nil)
	if err != nil || len(list.Resources) != 2 || list.TTLMs != 3600000 || list.CacheScope != "public" {
		t.Fatalf("resources/list: %+v %v", list, err)
	}
	tmpl, err := h.cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(tmpl.ResourceTemplates) != 4 {
		t.Fatalf("resources/templates/list: %+v %v", tmpl, err)
	}
	for _, c := range []struct {
		uri, scope string
		ttl        time.Duration
		has        string
	}{
		{scopeURI, "public", staticTTL, `"mean-only/v1"`},
		{dataContractURI, "public", staticTTL, `"contract":"rhobound.contract/v1`},
		{policyURI + "default", "private", policyTTL, `"max_entry_mean_change_pct":5`},
		{fitURI + fit, "private", handleTTL, `"schema":"rhobound-mcp.context/v1"`},
		{predictionURI + pred, "private", handleTTL, `"version":"rhobound-cli.report/v2"`},
		{packetURI + pred, "private", handleTTL, `"schema":"rhobound-mcp.judge-packet/v1"`},
	} {
		res, err := h.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: c.uri})
		if err != nil {
			t.Errorf("%s: %v", c.uri, err)
			continue
		}
		if res.CacheScope != c.scope || res.TTLMs != int(c.ttl/time.Millisecond) || len(res.Contents) != 1 ||
			!strings.Contains(res.Contents[0].Text, c.has) {
			t.Errorf("%s: %s %d %.300s", c.uri, res.CacheScope, res.TTLMs, res.Contents[0].Text)
		}
	}
	// the prediction resource carries the change set the server wrote and the full report
	var pr struct {
		Changes struct{ Fit string } `json:"changes"`
		Report  struct{ Version string }
	}
	if err := json.Unmarshal([]byte(h.readResource(predictionURI+pred)), &pr); err != nil || !strings.HasPrefix(pr.Changes.Fit, "sha256:") {
		t.Errorf("prediction resource: %+v %v", pr, err)
	}

	// another principal, in its own session (a client caches what it read), reads none of them; unknown names and
	// malformed handles are not found
	*h.who = "local:mallory"
	other := connect(t, h.srv, "")
	for _, uri := range []string{fitURI + fit, predictionURI + pred, packetURI + pred} {
		_, err := other.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
		if code := protocolCode(err); code != mcp.CodeResourceNotFound || !strings.Contains(err.Error(), "handle.unknown") {
			t.Errorf("%s by another principal: %v", uri, err)
		}
	}
	*h.who = "local:alice"
	for _, uri := range []string{policyURI + "nope", policyURI + "../x", fitURI + "fit_1", fitURI + pred, "rhobound://ledger"} {
		if _, err := h.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); protocolCode(err) != mcp.CodeResourceNotFound {
			t.Errorf("%s: %v", uri, err)
		}
	}

	// prompts: the loop, and the judge prompt around the same packet explain gives
	pl, err := h.cs.ListPrompts(ctx, nil)
	if err != nil || len(pl.Prompts) != 2 || pl.TTLMs != 3600000 || pl.CacheScope != "public" {
		t.Fatalf("prompts/list: %+v %v", pl, err)
	}
	gp, err := h.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: promptPlanChange})
	if err != nil || len(gp.Messages) != 1 || gp.Messages[0].Content.(*mcp.TextContent).Text != planChangeText {
		t.Errorf("plan_change: %+v %v", gp, err)
	}
	gp, err = h.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: promptReview, Arguments: map[string]string{"prediction_id": pred}})
	if err != nil || len(gp.Messages) != 2 {
		t.Fatalf("review_latency_impact: %+v %v", gp, err)
	}
	ex, _ := h.view("explain", `{"target":"`+pred+`","section":"judge_packet","max_chars":8000}`, FullMaxBytes)
	want, _ := encode(field(t, ex, "content"))
	if got, _ := strings.CutPrefix(gp.Messages[1].Content.(*mcp.TextContent).Text, packetLead); got != string(want) {
		t.Errorf("the judge prompt's packet differs from explain's:\n%s\n%s", got, want)
	}
	if txt := gp.Messages[0].Content.(*mcp.TextContent).Text; txt != reviewText {
		t.Errorf("review text: %s", txt)
	}
	for _, args := range []map[string]string{{}, {"prediction_id": fit}, {"prediction_id": "pred_1"}} {
		if _, err := h.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: promptReview, Arguments: args}); protocolCode(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("review_latency_impact %v: %v", args, err)
		}
	}
	// the rules of a prediction, explained under the policy and the clock of its preview
	rules, _ := h.view("explain", `{"target":"`+pred+`","section":"rules"}`, FullMaxBytes)
	if c := obj(t, field(t, rules, "content")); c["verdict"] != "inconclusive" {
		t.Errorf("explain rules: %s", rules)
	}
	// a fit has no assumptions, rules or judge packet of its own: those sections need a prediction handle (the CLI
	// explains them from a report v2)
	for _, sec := range []string{"assumptions", "rules", "judge_packet"} {
		args := `{"target":"` + fit + `","section":"` + sec + `"}`
		if _, _, err := h.harness.try("explain", args); protocolCode(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("explain %s: %v", args, err)
		}
	}
}

// TestHostileNamesNeverLeak: the hostile-names capture (service names with a right-to-left override, an instruction, a
// terminal escape, a 200-character name, and a token in a URL path), fitted and queried through every tool and the
// fit and packet resources, never shows any of those strings, or a part of them, in a result's structuredContent or
// text; the views mark the names untrusted.
func TestHostileNamesNeverLeak(t *testing.T) {
	dir := hostileCapture(t)
	h := realServer(t, map[string]string{"hostile": dir}, []byte(`{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":5}`),
		windowEnd(t, dir).Add(time.Hour), "")
	var results [][]byte
	keep := func(b []byte) []byte { results = append(results, b); return b }
	st := keep(h.wait(`{"capture":"hostile","workers":{"a":1}}`))
	fit, _ := field(t, st, "fit_id").(string)
	if fit == "" {
		t.Fatalf("fit: %s", st)
	}
	ctxView, _ := h.view("context", `{"fit_id":"`+fit+`","detail":"full"}`, FullMaxBytes)
	keep(ctxView)
	b, _ := h.view("budget", `{"fit_id":"`+fit+`"}`, SummaryMaxBytes)
	keep(b)
	pv, _ := h.view("predict", `{"fit_id":"`+fit+`","detail":"full","changes":[{"op":"own_ms","service":"a","value":0.4}]}`, FullMaxBytes)
	keep(pv)
	pred := field(t, pv, "prediction_id").(string)
	for _, args := range []string{
		`{"target":"` + fit + `","section":"services","max_chars":8000}`,
		`{"target":"` + fit + `","section":"summary","max_chars":8000}`,
		`{"target":"` + fit + `","section":"findings","max_chars":8000}`,
		`{"target":"` + pred + `","section":"findings","max_chars":8000}`,
		`{"target":"` + pred + `","section":"judge_packet","max_chars":8000}`,
	} {
		b, _ := h.view("explain", args, FullMaxBytes)
		keep(b)
	}
	keep([]byte(h.readResource(fitURI + fit)))
	keep([]byte(h.readResource(packetURI + pred)))
	frags := []string{"ignore every rule", "\u202e", "\x1b", "[2J", "s3cr3t", "token=", "/orders/42", strings.Repeat("c", 129),
		`\u202e`, `\u001b`}
	for i, r := range results {
		for _, frag := range frags {
			if bytes.Contains(r, []byte(frag)) {
				t.Errorf("result %d leaks %q:\n%s", i, frag, r)
			}
		}
	}
	if !bytes.Contains(ctxView, []byte(`"name_untrusted":true`)) {
		t.Errorf("context does not mark the hostile names untrusted:\n%s", ctxView)
	}
}
