// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server/schemas"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// refusalOK checks that b is a refusal result with code, valid against the published Refusal definition, and returns
// its fix.
func refusalOK(t *testing.T, what string, b []byte, isErr bool, err error, code string) string {
	t.Helper()
	if err != nil || !isErr || refusalCode(t, b) != code {
		t.Fatalf("%s: %s %v %v, want the refusal %s", what, b, isErr, err, code)
	}
	sch, serr := schemas.Bundle("defs.v1.json", "/$defs/Refusal")
	rs, rerr := schemas.Resolve(sch)
	if serr != nil || rerr != nil {
		t.Fatal(serr, rerr)
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if json.Unmarshal(b, &env) != nil {
		t.Fatalf("%s: %s", what, b)
	}
	if verr := rs.Validate(env.Error); verr != nil {
		t.Errorf("%s: the refusal does not match defs Refusal: %v", what, verr)
	}
	fix, _ := env.Error["fix"].(string)
	return fix
}

// TestPolicyRefusalsAreCoded: budget and predict never answer without a policy's thresholds. On a server without a
// default policy, a call that names none is refused with policy.not_named, and a name the configuration does not list
// with policy.unknown, both as tool results with isError and a fix that names the configured policies; neither runs
// the engine nor writes a preview-log row. A named policy still answers.
func TestPolicyRefusalsAreCoded(t *testing.T) {
	h := newHarness(t, harnessOpt{noDefaultPolicy: true})
	fit := h.startFit(`{"capture":"loop"}`)
	calls := len(h.fake.Calls())
	for _, c := range []struct{ tool, args, code string }{
		{"budget", `{"fit_id":"` + fit + `","service":"b"}`, "policy.not_named"},
		{"predict", `{"fit_id":"` + fit + `","changes":[{"op":"own_ms","service":"b","value":0.4}],"repo":"shop"}`, "policy.not_named"},
		{"budget", `{"fit_id":"` + fit + `","policy":"default"}`, "policy.unknown"},
		{"predict", `{"fit_id":"` + fit + `","changes":[],"policy":"nope"}`, "policy.unknown"},
		{"predict", `{"fit_id":"` + fit + `","changes":[],"policy":"a//b"}`, "policy.unknown"},
		// the policy is checked first: an env without a baseline mints nothing on the way
		{"predict", `{"env":"staging","changes":[]}`, "policy.not_named"},
	} {
		b, isErr, err := h.try(c.tool, c.args)
		fix := refusalOK(t, c.tool+" "+c.args, b, isErr, err, c.code)
		if !strings.HasSuffix(fix, ": loose, strict") {
			t.Errorf("%s %s: the fix does not name the configured policies: %q", c.tool, c.args, fix)
		}
	}
	if n := len(h.fake.Calls()); n != calls {
		t.Errorf("%d engine calls for refused policies", n-calls)
	}
	// a named policy answers, and the budget runs under it
	if b, isErr := h.call("budget", `{"fit_id":"`+fit+`","policy":"strict"}`); isErr {
		t.Fatalf("budget under strict: %s", b)
	}
	last := h.fake.Calls()[len(h.fake.Calls())-1]
	if _, ok := last.FileFor("--policy"); last.Sub != "budget" || !ok {
		t.Errorf("budget ran without --policy: %v", last.Args)
	}
	// the first preview logged for the repository: the refused one wrote no row
	b, isErr := h.call("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}],"policy":"loose","repo":"shop"}`)
	if isErr || field(t, b, "attempt") != json.Number("1") {
		t.Errorf("predict under loose: %s", b)
	}

	// With a default policy, a call that names none uses it: budget always runs with --policy.
	d := newHarness(t, harnessOpt{})
	dfit := d.startFit(`{"capture":"loop"}`)
	if b, isErr := d.call("budget", `{"fit_id":"`+dfit+`"}`); isErr {
		t.Fatalf("budget under the default: %s", b)
	}
	if c := d.fake.Calls()[len(d.fake.Calls())-1]; c.Sub != "budget" {
		t.Errorf("last call %s", c.Sub)
	} else if _, ok := c.FileFor("--policy"); !ok {
		t.Errorf("budget under the default ran without --policy: %v", c.Args)
	}
}

// TestPolicyFixFits: the fix names as many configured policies as fit in 300 characters, sorted, and says so when it
// stops; a server with no policy at all says how to add one.
func TestPolicyFixFits(t *testing.T) {
	c := testConfig(t, "", "")
	c.Policies = map[string]string{}
	for i := 0; i < 40; i++ {
		c.Policies["team-"+strings.Repeat(string(rune('a'+i%26)), 20)+"-"+string(rune('a'+i/26))] = "/x"
	}
	s := &Server{opt: Options{Config: c}}
	fix := s.policyFix()
	if len(fix) > 300 || !strings.HasSuffix(fix, ", ...") || !strings.Contains(fix, "team-"+strings.Repeat("a", 20)+"-a, ") {
		t.Errorf("%d characters: %q", len(fix), fix)
	}
	c.Policies = nil
	if fix := s.policyFix(); !strings.Contains(fix, "add one under policies") {
		t.Errorf("no policies: %q", fix)
	}
}

// TestEnvWithoutBaselineIsCoded: an environment with no baseline fit is the refusal fit.no_baseline, in context,
// budget's fit resolution aside, and predict; an unknown environment, a schema violation, an unknown tool and the
// stdio-only capture argument over HTTP stay protocol errors (-32602).
func TestEnvWithoutBaselineIsCoded(t *testing.T) {
	h := newHarness(t, harnessOpt{remote: true})
	for _, args := range []struct{ tool, args string }{
		{"context", `{"env":"staging"}`},
		{"predict", `{"env":"staging","changes":[{"op":"own_ms","service":"b","value":0.4}]}`},
	} {
		b, isErr, err := h.try(args.tool, args.args)
		// this build does not fit an environment's window (fit_start{env} is a protocol error): the fix does not
		// send the caller there; nor, outside stdio mode, where only context with env mints a fit handle, to a fit_id
		fix := refusalOK(t, args.tool, b, isErr, err, "fit.no_baseline")
		if strings.Contains(fix, "fit_start") || strings.Contains(fix, "fit_id") {
			t.Errorf("%s: the default profile's fix: %q", args.tool, fix)
		}
	}
	if len(h.fake.Calls()) != 0 {
		t.Errorf("%d engine calls", len(h.fake.Calls()))
	}
	for _, c := range []struct{ tool, args string }{
		{"context", `{"env":"nowhere"}`},
		{"context", `{"capture":"loop"}`},
		{"budget", `{}`},
		{"predict", `{"env":"staging","changes":[],"policy":"BAD NAME"}`},
		{"no_such_tool", `{}`},
	} {
		if _, _, err := h.try(c.tool, c.args); protocolCode(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("%s %s: %v, want -32602", c.tool, c.args, err)
		}
	}
	// the refusal of an unknown environment names the configured ones, so the model's next call can be right
	if _, _, err := h.try("context", `{"env":"nowhere"}`); err == nil || !strings.Contains(err.Error(), "environments: staging") {
		t.Errorf("unknown environment: %v, want the configured environments named", err)
	}
}

// TestFitTTLFromConfig: fit_ttl_h sets how long fit handles live and how old a baseline may be (fit.expired); without
// it, a week.
func TestFitTTLFromConfig(t *testing.T) {
	h := newHarness(t, harnessOpt{fitTTLH: 2})
	if h.srv.opt.FitTTL != 2*time.Hour {
		t.Fatalf("FitTTL %v", h.srv.opt.FitTTL)
	}
	ctx := context.Background()
	d, _ := h.store.PutArtifact(ctx, "fit", []byte(fitDoc)) // its window ended 20 s before the harness clock
	if err := h.store.SetBaseline(ctx, "staging", d); err != nil {
		t.Fatal(err)
	}
	b, isErr := h.call("context", `{"env":"staging"}`)
	if isErr {
		t.Fatalf("a fresh baseline: %s", b)
	}
	fit := field(t, b, "fit_id").(string)
	h.clock.add(2 * time.Hour)
	b, isErr, err := h.try("context", `{"env":"staging"}`)
	refusalOK(t, "a baseline past fit_ttl_h", b, isErr, err, "fit.expired")
	b, isErr, err = h.try("context", `{"fit_id":"`+fit+`"}`)
	refusalOK(t, "a fit handle past fit_ttl_h", b, isErr, err, "handle.expired")

	if d := newHarness(t, harnessOpt{}); d.srv.opt.FitTTL != DefaultFitTTL || DefaultFitTTL != 168*time.Hour {
		t.Errorf("no fit_ttl_h: %v", d.srv.opt.FitTTL)
	}
	if c := testConfig(t, config.ProfileDefault, ""); c.FitTTLH != 0 {
		t.Errorf("fit_ttl_h absent reads as %d", c.FitTTLH)
	}
}

// TestEngineFailuresAreCoded: a step whose engine exits without a coded refusal, or writes no record, fails its job
// with job.failed (a fixed reason and the registry's fix), and context{capture} in the bench profile refuses with it,
// with the bench profile's fix: no failure reaches a caller without a code.
func TestEngineFailuresAreCoded(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply runner.Reply
		sub   string
	}{
		{"check exits 2 with no report", runner.Reply{Exit: 2}, "check"},
		{"fit exits 1 with no refusal", runner.Reply{Exit: 1, Stdout: []byte("rhobound: --name: ...")}, "fit"},
		{"fit writes no record", runner.Reply{}, "fit"},
	} {
		h := newHarness(t, harnessOpt{})
		h.cli.replies = map[string]runner.Reply{c.sub: c.reply}
		b, _ := h.call("fit_start", `{"capture":"loop"}`)
		st, _ := h.call("job_status", `{"job_id":"`+field(t, b, "job_id").(string)+`","wait_s":10}`)
		r, _ := field(t, st, "refusal").(map[string]any)
		if field(t, st, "state") != "failed" || r["code"] != state.CodeJobFailed || r["fix"] != state.FailedRefusal().Fix ||
			r["reason"] != state.FailedRefusal().Reason {
			t.Errorf("%s: job_status %s", c.name, st)
		}
		bench := newHarness(t, harnessOpt{profile: config.ProfileBench})
		bench.cli.replies = map[string]runner.Reply{c.sub: c.reply}
		b, isErr, err := bench.try("context", `{"capture":"loop"}`)
		// a bench server has no jobs: its fix sends the caller to context again, never to a job
		if fix := refusalOK(t, c.name+": bench context{capture}", b, isErr, err, "job.failed"); fix !=
			serverFixes["job.failed"].bench || strings.Contains(fix, "job") {
			t.Errorf("%s: bench context{capture}: the fix %q", c.name, fix)
		}
	}
}

// TestStoredJobsRebuilt: job_status shows a stored job record as the server builds one, whoever wrote it: a step of
// the job-status enum only, a refusal with a registered code, a fixed reason, the registry's fix, a subject through
// s.subject and only the numbers the registry names for its code (a snake_case name of another code's, or of none, is
// left out), and findings with the registry's level and fix and their code's numbers (an unregistered code is counted,
// not listed).
func TestStoredJobsRebuilt(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	now := h.srv.opt.Now()
	put := func(j state.Job) map[string]any {
		t.Helper()
		id, err := state.NewHandle(state.KindJob, now)
		if err != nil {
			t.Fatal(err)
		}
		j.ID, j.Principal, j.CreatedAt, j.DeadlineAt, j.State = id, "local:alice", now, now.Add(time.Hour), state.JobFailed
		if err := h.srv.opt.Store.PutJob(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		b, _ := h.call("job_status", `{"job_id":"`+id+`"}`)
		var out map[string]any
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	planted := "/srv/x 198.51.100.250:5432"
	out := put(state.Job{Step: "check",
		Refusal: json.RawMessage(`{"code":"trace.broken_traces","reason":"` + planted + `","fix":"` + planted +
			`","subject":"/srv/x","numbers":{"Bad Key":1,"broken":2,"broken_share":0.2},"extra":"` + planted + `"}`),
		Findings: json.RawMessage(`[{"code":"trace.pod_churn","level":"refuse","fix":"` + planted + `","subject":"b-1",` +
			`"numbers":{"present_share":0.5,"Bad Key":1,"lost_share":0.1}},{"code":"plnt.unknown","level":"doubt","fix":"x"}]`)})
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "198.51.100.250") || strings.Contains(string(b), "/srv/x") || out["step"] != "check" {
		t.Errorf("a planted job record is shown as stored: %s", b)
	}
	r, _ := out["refusal"].(map[string]any)
	want := map[string]any{"code": "trace.broken_traces", "reason": EngineRefusalReason,
		"fix": messageFixes["trace.broken_traces"], "numbers": map[string]any{"broken": 2.0}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("refusal %v, want %v", r, want)
	}
	fs, _ := out["findings"].([]any)
	if len(fs) != 1 || out["findings_omitted"] != 1.0 {
		t.Fatalf("findings %v, omitted %v", fs, out["findings_omitted"])
	}
	f := fs[0].(map[string]any)
	if f["level"] != "doubt" || f["fix"] != "a quiet window" || f["subject"] != "b-1" ||
		!reflect.DeepEqual(f["numbers"], map[string]any{"present_share": 0.5}) {
		t.Errorf("finding %v", f)
	}
	// a refusal with no well-formed code is job.failed; the server's own refusals keep its text
	if r, _ := put(state.Job{Refusal: json.RawMessage(`{"code":"` + planted + `"}`)})["refusal"].(map[string]any); r["code"] != state.CodeJobFailed {
		t.Errorf("a refusal without a code: %v", r)
	}
	if r, _ := put(state.Job{Step: "fit", Refusal: json.RawMessage(`{"code":"job.timeout","reason":"` + planted + `"}`)})["refusal"].(map[string]any); r["reason"] != state.TimeoutRefusal().Reason {
		t.Errorf("job.timeout: %v", r)
	}
	// a step, a result or a state the server does not write is refused where a job is stored; the store's read of a
	// file written otherwise is state's TestJobRecordsChecked
	id, _ := state.NewHandle(state.KindJob, now)
	for _, j := range []state.Job{{Step: planted}, {Result: "fit_" + planted}, {Result: "pred_01HF81WAV64APK0M95Y61P1FVY"},
		{State: planted}} {
		j.ID, j.Principal, j.CreatedAt, j.DeadlineAt = id, "local:alice", now, now.Add(time.Hour)
		j.State = cmp.Or(j.State, state.JobCompleted)
		if err := h.srv.opt.Store.PutJob(context.Background(), j); err == nil || strings.Contains(err.Error(), planted) {
			t.Errorf("PutJob %+v: %v", j, err)
		}
	}
}
