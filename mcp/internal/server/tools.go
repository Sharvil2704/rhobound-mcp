// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server/schemas"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// tool is one registered tool: its definition (as tools/list shows it), its input validator, the scope it needs
// beyond rhobound:read, and its handler.
type tool struct {
	def   *mcp.Tool
	input *jsonschema.Resolved
	scope string
	run   func(ctx context.Context, c *call) (*mcp.CallToolResult, error)
}

// call is one tool call: the caller and the arguments, already checked against the input schema.
type call struct {
	id   auth.Identity
	p    state.Principal
	args json.RawMessage
}

// spec describes a tool before registration. benchDesc, when set, is its description in the bench profile, which has
// no fit_start (ARCHITECTURE.md 5.3).
type spec struct {
	name, file, desc, benchDesc string
	readOnly, bench             bool
	run                         func(s *Server) func(ctx context.Context, c *call) (*mcp.CallToolResult, error)
}

// specs are the six tools of ARCHITECTURE.md 5.3. Descriptions stay well under 300 tokens each, and the whole
// tools/list result under 2,400 (bytes/4); the input schemas are the published ones, made self-contained.
var specs = []spec{
	{name: "context", file: "context.v1.json", readOnly: true, bench: true,
		desc: "Where latency is tight, before you write code. Per entry: the model's mean latency. Per service: " +
			"utilization, own time per visit, workers, and leverage (entry ms per +1 ms of the service's own time " +
			"per visit). Plus findings and assumptions. Name the fit by fit_id, by env (its baseline fit) or, in " +
			"stdio mode, by a capture name from the config.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.contextTool }},
	{name: "budget", file: "budget.v1.json", readOnly: true, bench: true,
		desc: "How much own time per visit a service may add before an entry's mean crosses the policy's " +
			"threshold: max_value (the change reaches the threshold) and safe_value (the change plus the model " +
			"band reaches it). Design within safe_value. Per visit only; monotonicity is assumed, not tested.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.budgetTool }},
	{name: "predict", file: "prediction.v1.json", readOnly: true, bench: true,
		desc: "The mean-latency effect of a change on a fit (fit_id, or env for its baseline), with an advisory " +
			"preview verdict under a named policy. changes are numbers you declare (own_ms, own_pct, workers, " +
			"load_scale; a number or {lo,hi}): they preview as block or inconclusive, never pass. measured " +
			"[{ab: handle}] uses an A/B difference from fit_start. Each call is logged for the repo and branch.",
		benchDesc: "The mean-latency effect of a change on a fit (fit_id, or env for its baseline), with an advisory " +
			"preview verdict under a named policy. changes are numbers you declare (own_ms, own_pct, workers, " +
			"load_scale; a number or {lo,hi}): they preview as block or inconclusive, never pass. The A/B " +
			"measurement that can pass is made outside this server, by the CI. Each call is logged for the repo " +
			"and branch.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.predictTool }},
	{name: "explain", file: "explain.v1.json", readOnly: true, bench: true,
		desc: "Drill into a fit or prediction handle (target) by section: summary, services, findings, " +
			"assumptions, rules or judge_packet; data_contract and scope need no target. For clients without " +
			"resources.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.explainTool }},
	{name: "fit_start", file: "fit-start.v1.json",
		desc: "Start a fit as a job: an environment's window (env), a configured capture in stdio mode " +
			"(capture, with workers pinned per service if known), or the A/B difference of two registered " +
			"captures (ab: {baseline, candidate}; the CLI's diff-fit), which predict then uses as a measured change. " +
			"Starts compute; the same request returns the same job. Poll job_status.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.fitStartTool }},
	{name: "job_status", file: "job-status.v1.json", readOnly: true,
		desc: "The state of a fit_start job: working, completed (with fit_id or ab_id), failed (with a " +
			"refusal) or cancelled. wait_s long-polls up to 20 s in stdio and container modes.",
		run: func(s *Server) func(context.Context, *call) (*mcp.CallToolResult, error) { return s.jobStatusTool }},
}

// profileSpecs are the tools of a profile: all six by default, and context, budget, predict and explain in the bench
// profile (ARCHITECTURE.md 5.3), with their bench descriptions.
func profileSpecs(profile string) []spec {
	var out []spec
	for _, sp := range specs {
		if profile == config.ProfileBench {
			if !sp.bench {
				continue
			}
			if sp.benchDesc != "" {
				sp.desc = sp.benchDesc
			}
		}
		out = append(out, sp)
	}
	return out
}

// definition is a tool's definition as tools/list shows it, and its resolved input schema.
func definition(sp spec) (*mcp.Tool, *jsonschema.Resolved, error) {
	in, err := schemas.Bundle(sp.file, "/$defs/input")
	if err != nil {
		return nil, nil, err
	}
	rs, err := schemas.Resolve(in)
	if err != nil {
		return nil, nil, fmt.Errorf("server: %s input schema: %w", sp.name, err)
	}
	f := false
	return &mcp.Tool{
		Name:        sp.name,
		Description: sp.desc,
		InputSchema: in,
		// Every hint explicitly: the two pointer hints default to true when absent (ARCHITECTURE.md 5.3).
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: sp.readOnly, DestructiveHint: &f,
			IdempotentHint: true, OpenWorldHint: &f},
	}, rs, nil
}

// toolSet builds the tools of a profile.
func (s *Server) toolSet(profile string) ([]*tool, error) {
	var out []*tool
	for _, sp := range profileSpecs(profile) {
		def, rs, err := definition(sp)
		if err != nil {
			return nil, err
		}
		t := &tool{def: def, input: rs, run: sp.run(s)}
		if scope, ok := auth.ToolScopes[sp.name]; ok {
			t.scope = scope
		}
		out = append(out, t)
	}
	return out, nil
}

// handler wraps a tool's run with the checks every call gets: arguments against the input schema (a violation is a
// protocol error), the caller's identity, and the tool's scope.
func (s *Server) handler(t *tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := req.Params.Arguments
		if len(strings.TrimSpace(string(args))) == 0 || string(args) == "null" {
			args = json.RawMessage(`{}`)
		}
		var v any
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, invalidParams("arguments are not JSON")
		}
		if err := t.input.Validate(v); err != nil {
			return nil, invalidParams("arguments do not match the input schema of " + t.def.Name + ": " + clip(err.Error(), 300))
		}
		id, ok := s.opt.Identity(ctx)
		if !ok {
			return nil, internalError("no caller identity")
		}
		if t.scope != "" && !id.Has(t.scope) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidRequest, Message: "this caller lacks the scope " + t.scope}
		}
		if ok, wait := s.opt.Limits.Allow(id.Principal, t.def.Name); !ok {
			return result(rateLimited(wait).Envelope(), true), nil
		}
		res, err := t.run(ctx, &call{id: id, p: state.Principal(id.Principal), args: args})
		var rf *refused
		if errors.As(err, &rf) {
			return result(rf.body, true), nil
		}
		return res, err
	}
}

// rateLimited is the refusal of a call over a per-principal limit: rate.limited, allowed again after wait. The hint
// is rounded up to the millisecond, so that a retry sent when it says is never early (ARCHITECTURE.md 5.8).
func rateLimited(wait time.Duration) state.Refusal {
	ms := int64((wait + time.Millisecond - 1) / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	return state.Refusal{Code: state.CodeRateLimited, Reason: "this caller is over its limit for this tool",
		Fix: "retry after retry_after_ms", Numbers: map[string]float64{"retry_after_ms": float64(ms)}}
}

// decode reads a call's arguments into a tool's input struct.
func (c *call) decode(v any) error {
	if err := json.Unmarshal(c.args, v); err != nil {
		return invalidParams("arguments do not fit the tool's input")
	}
	return nil
}

// unknownEnv refuses an environment name the configuration does not hold, and names what it does hold: a model's
// first guess is often a capture's name or a made-up one (5 of the 11 refused calls in the Muse experiment, 2026-11-03).
func unknownEnv(c *config.Config) error {
	msg := "unknown environment"
	envs, caps := c.Names()
	if len(envs) > 0 {
		msg += "; environments: " + strings.Join(envs, ", ")
	} else {
		msg += "; this server has no environments"
	}
	if len(caps) > 0 {
		msg += "; captures (pass as capture, not env): " + strings.Join(caps, ", ")
	}
	return invalidParams(msg)
}

func invalidParams(msg string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: msg}
}

func internalError(msg string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: msg}
}

// refused carries a refusal through a handler's error return: body is the canonical {"error": Refusal}. A tool
// handler turns it back into a result with isError true; resources and prompts, which have no such result, return it
// as a protocol error (protocolRefusal).
type refused struct{ body []byte }

func (r *refused) Error() string { return "refused" }

// refuse returns the error that makes the call answer with r ({"error": r}, isError true).
func refuse(r state.Refusal) error {
	return &refused{body: r.Envelope()}
}

// protocolRefusal turns a refusal into a JSON-RPC error for a request that has no error result (resources/read,
// prompts/get): the code of the Refusal object leads the message, and the object is the error's data. A handle another
// principal holds reads as one that never existed, as in a tool. Other errors pass through.
func protocolRefusal(err error) error {
	var rf *refused
	if !errors.As(err, &rf) {
		return err
	}
	var env struct {
		Error struct {
			Code   string `json:"code"`
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if json.Unmarshal(rf.body, &env) != nil || env.Error.Code == "" {
		return internalError("a refusal does not parse")
	}
	code := int64(jsonrpc.CodeInvalidParams)
	if env.Error.Code == state.CodeHandleUnknown {
		code = mcp.CodeResourceNotFound
	}
	data, _ := decodeObject(rf.body)
	b, _ := encode(data["error"])
	return &jsonrpc.Error{Code: code, Message: env.Error.Code + ": " + env.Error.Reason, Data: b}
}

// result is a tool result: b as structuredContent, and as one text block ending with the scope sentence.
func result(b []byte, isError bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError:           isError,
		StructuredContent: json.RawMessage(b),
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b) + "\n" + ScopeSentence}},
	}
}

// clip shortens s to at most n bytes, on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
