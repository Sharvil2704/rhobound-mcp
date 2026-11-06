// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
)

// serverJobWords are the words that send an agent to a tool the bench profile does not have.
var serverJobWords = []string{"fit_start", "job_status"}

// TestBenchTextNamesNoServerJob: in the bench profile, which has no fit_start or job_status, nothing a model reads
// points at them: the server instructions, the tool descriptions, the prompts, and the fixes of the refusals the server
// raises (fit.no_baseline, fit.expired, handle.unknown, also for explain on a job or version handle, handle.expired,
// change.unsupported_op, profile.not_found); they say the A/B is measured outside the server, by the CI. The default
// profile's texts still point at fit_start, except the fixes of an environment's baseline, since this build does not
// fit an environment's window, and, outside stdio mode or over stdio with no capture configured, where fit_start makes
// no fit, the fixes of a handle; and there, in either profile, no fix of an environment's baseline names a fit_id.
func TestBenchTextNamesNoServerJob(t *testing.T) {
	texts := func(h *harness) map[string]string {
		t.Helper()
		out := map[string]string{"instructions": h.cs.InitializeResult().Instructions}
		tools, err := h.cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tl := range tools.Tools {
			out["tool "+tl.Name] = tl.Description
		}
		p, err := h.cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: promptPlanChange})
		if err != nil {
			t.Fatal(err)
		}
		out["prompt plan_change"] = p.Messages[0].Content.(*mcp.TextContent).Text
		out["prompt review"] = promptTexts(h.srv.opt.Config.Profile)[promptReview]

		fit := "fit_" + strings.Repeat("Z", 26)
		refusals := map[string]string{
			"fit.no_baseline":       `{"env":"staging","changes":[]}`,
			"handle.unknown":        `{"fit_id":"` + fit + `","changes":[]}`,
			"change.unsupported_op": `{"fit_id":"` + fit + `","changes":[],"assumptions":[{"question":"Q1","range":{"lo":1,"hi":3}}]}`,
		}
		for code, args := range refusals {
			b, isErr := h.call("predict", args)
			out["fix "+code] = refusalOK(t, code, b, isErr, nil, code)
		}
		for _, kind := range []string{"job", "ver"} {
			b, isErr := h.call("explain", `{"target":"`+kind+"_"+strings.Repeat("Z", 26)+`","section":"services"}`)
			out["fix handle.unknown of explain on "+kind+"_"] = refusalOK(t, "explain "+kind, b, isErr, nil, "handle.unknown")
		}
		ctx := context.Background()
		d, _ := h.store.PutArtifact(ctx, "fit", []byte(fitDoc))
		if err := h.store.SetBaseline(ctx, "staging", d); err != nil {
			t.Fatal(err)
		}
		b, isErr := h.call("context", `{"env":"staging"}`)
		if isErr {
			t.Fatalf("context env: %s", b)
		}
		minted := field(t, b, "fit_id").(string)
		b, isErr = h.call("predict", `{"fit_id":"`+minted+`","measured":[{"profile":{"service":"b","candidate":"sha256:`+
			strings.Repeat("a", 64)+`"}}]}`)
		out["fix profile.not_found"] = refusalOK(t, "profile.not_found", b, isErr, nil, "profile.not_found")
		h.clock.add(DefaultFitTTL)
		b, isErr = h.call("context", `{"env":"staging"}`)
		out["fix fit.expired"] = refusalOK(t, "fit.expired", b, isErr, nil, "fit.expired")
		b, isErr = h.call("budget", `{"fit_id":"`+minted+`"}`)
		out["fix handle.expired"] = refusalOK(t, "handle.expired", b, isErr, nil, "handle.expired")
		return out
	}

	bench := texts(newHarness(t, harnessOpt{profile: config.ProfileBench}))
	for what, text := range bench {
		for _, w := range serverJobWords {
			if strings.Contains(text, w) {
				t.Errorf("bench: %s names %s: %q", what, w, text)
			}
		}
	}
	for _, what := range []string{"instructions", "tool predict", "prompt plan_change", "fix fit.no_baseline", "fix fit.expired"} {
		if !strings.Contains(bench[what], "outside this server") {
			t.Errorf("bench: %s does not say where the A/B or the baseline is made: %q", what, bench[what])
		}
	}
	full := texts(newHarness(t, harnessOpt{}))
	for _, what := range []string{"instructions", "tool predict", "prompt plan_change", "fix handle.unknown",
		"fix change.unsupported_op", "fix profile.not_found"} {
		if !strings.Contains(full[what], "fit_start") {
			t.Errorf("default: %s does not point at fit_start: %q", what, full[what])
		}
	}
	// this build does not fit an environment's window (fit_start{env} is a protocol error), so the fixes of an
	// environment's missing or expired baseline do not send the caller there
	for _, what := range []string{"fix fit.no_baseline", "fix fit.expired"} {
		if strings.Contains(full[what], "fit_start") || !strings.Contains(full[what], "fit_id") {
			t.Errorf("default: %s: %q", what, full[what])
		}
	}
	// nor, outside stdio mode, where the capture argument is refused too, do the fixes of a handle
	remote := texts(newHarness(t, harnessOpt{remote: true}))
	for _, what := range []string{"fix handle.unknown", "fix handle.unknown of explain on job_",
		"fix handle.unknown of explain on ver_", "fix handle.expired"} {
		if strings.Contains(remote[what], "fit_start") || !strings.Contains(remote[what], "env's baseline") {
			t.Errorf("default, outside stdio mode: %s: %q", what, remote[what])
		}
	}
	// and outside stdio mode, in either profile, the fixes of an environment's missing or expired baseline name no
	// fit_id: only context with env mints one there, the call they answer
	benchRemote := texts(newHarness(t, harnessOpt{profile: config.ProfileBench, remote: true}))
	for _, what := range []string{"fix fit.no_baseline", "fix fit.expired"} {
		for profile, texts := range map[string]map[string]string{"default": remote, "bench": benchRemote} {
			if strings.Contains(texts[what], "fit_id") || strings.Contains(texts[what], "fit_start") {
				t.Errorf("%s, outside stdio mode: %s: %q", profile, what, texts[what])
			}
		}
		if !strings.Contains(benchRemote[what], "outside this server") {
			t.Errorf("bench, outside stdio mode: %s: %q", what, benchRemote[what])
		}
	}
	// over stdio with environments only, no capture (and so no A/B arm) can be fitted: in either profile no fix names a
	// fit_id or fit_start, as outside stdio mode
	for _, profile := range []string{config.ProfileDefault, config.ProfileBench} {
		envsOnly := texts(newHarness(t, harnessOpt{profile: profile, noCaptures: true}))
		for _, what := range []string{"fix fit.no_baseline", "fix fit.expired", "fix handle.unknown",
			"fix handle.unknown of explain on job_", "fix handle.unknown of explain on ver_", "fix handle.expired"} {
			if strings.Contains(envsOnly[what], "fit_id") || strings.Contains(envsOnly[what], "fit_start") ||
				envsOnly[what] != map[string]map[string]string{"default": remote, "bench": benchRemote}[profile][what] {
				t.Errorf("%s, stdio with environments only: %s: %q", profile, what, envsOnly[what])
			}
		}
	}
}

// TestBenchDefinitions: the published definitions hold each profile's own instructions, and the bench profile's
// definitions name neither fit_start nor job_status.
func TestBenchDefinitions(t *testing.T) {
	b, err := Definitions()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc) != 2 || doc[config.ProfileBench] == nil || doc[config.ProfileDefault] == nil {
		t.Fatalf("the definitions' keys: %s", b[:min(len(b), 200)])
	}
	for _, w := range serverJobWords {
		if strings.Contains(string(doc[config.ProfileBench]), w) {
			t.Errorf("the bench definitions name %s", w)
		}
		if !strings.Contains(string(doc[config.ProfileDefault]), w) {
			t.Errorf("the default definitions do not name %s", w)
		}
	}
	var bench struct {
		Instructions string `json:"instructions"`
	}
	if json.Unmarshal(doc[config.ProfileBench], &bench) != nil || bench.Instructions != instructions(config.ProfileBench) ||
		bench.Instructions == Instructions {
		t.Errorf("bench instructions: %q", bench.Instructions)
	}
	if strings.Contains(benchText(Instructions), abWay) || !strings.Contains(Instructions, abWay) ||
		!strings.Contains(planChangeText, abWay) {
		t.Error("benchText no longer finds the sentence it replaces")
	}
}
