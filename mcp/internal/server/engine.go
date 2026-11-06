// SPDX-License-Identifier: Apache-2.0

package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// run runs a CLI twin and classifies what it printed:
//   - an object with only the key "error", on a non-zero exit status: the CLI's refusal, returned as the tool's
//     refusal (a *refused error) with its reason replaced (engineRefusal);
//   - any other JSON object, on one of the exit statuses ok lists: the view, canonical;
//   - anything else: an internal error. Nothing the engine printed reaches the caller in that case, since it was not
//     the document the tool promises.
//
// A call is bounded by CallTimeout: run serves the tools that answer at once, never a fit job.
func (s *Server) run(ctx context.Context, sub string, args []string, files map[string][]byte, ok ...int) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opt.CallTimeout)
	defer cancel()
	out, exit, err := s.opt.Runner.Run(ctx, sub, args, files)
	if err != nil {
		return nil, engineError(err)
	}
	obj, derr := decodeObject(out)
	if derr == nil && exit != 0 && len(obj) == 1 {
		if _, isErr := obj["error"]; isErr {
			r, ok := s.engineRefusal(obj["error"], nil)
			if !ok {
				return nil, internalError("the engine's refusal is not a Refusal object")
			}
			return nil, refuse(r)
		}
	}
	if derr != nil || !slices.Contains(ok, exit) {
		return nil, internalError(fmt.Sprintf("the engine's %s failed (exit status %d)", sub, exit))
	}
	b, err := encode(obj)
	if err != nil {
		return nil, internalError("the engine's output does not encode")
	}
	return b, nil
}

// engineError maps a runner error: an engine whose sha256 changed is the refusal server.engine_mismatch; a timeout
// or any other failure to run is an internal error. A timeout stays recognizable (errors.Is runner.ErrTimeout), so a
// job reports it as job.timeout.
func engineError(err error) error {
	var em *runner.EngineMismatchError
	switch {
	case errors.As(err, &em):
		return refuse(state.Refusal{Code: state.CodeEngineMismatch,
			Reason: "the engine binary's sha256 differs from the configured one",
			Fix:    "install the engine the configuration names, or update engine.sha256 and restart the server"})
	case errors.Is(err, runner.ErrTimeout):
		return &engineTimeout{&jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "the engine ran past its timeout"}}
	}
	return internalError("the engine could not run")
}

// engineTimeout is an engine call past the runner's timeout: on the wire, the internal error it wraps.
type engineTimeout struct{ rpc *jsonrpc.Error }

func (e *engineTimeout) Error() string        { return e.rpc.Error() }
func (e *engineTimeout) Unwrap() error        { return e.rpc }
func (e *engineTimeout) Is(target error) bool { return target == runner.ErrTimeout }

// fitAge is the fit's age in whole seconds at now, measured from the end of its window; ok is false when the fit
// does not carry a window end.
func fitAge(fit []byte, now time.Time) (int64, bool) {
	var f struct {
		Window struct {
			End string `json:"end"`
		} `json:"window"`
	}
	if json.Unmarshal(fit, &f) != nil || f.Window.End == "" {
		return 0, false
	}
	end, err := time.Parse(time.RFC3339Nano, f.Window.End)
	if err != nil {
		return 0, false
	}
	age := int64(now.Sub(end) / time.Second)
	if age < 0 {
		age = 0
	}
	return age, true
}

// EngineRefusalReason is the reason of every refusal the engine printed, as the server returns it.
const EngineRefusalReason = "the engine refused this input; the code names the cause and the fix what to do " +
	"(explain section data_contract describes every code)"

// dataContractFix is the fix of an engine refusal that carries none the server can return.
const dataContractFix = "see the code in explain section data_contract"

// fixAsMessage is the registry's fix of the codes whose advice is in the engine's message (internal/contract's
// fixAsMessage; this module cannot import it, and a test pins it to docs/mcp/data-contract.json).
const fixAsMessage = "as the message says"

// messageFixes are the server's fixes for the codes whose registry fix is fixAsMessage: what the message the server
// withholds advises (internal/infer's refusals), in fixed text. The CLI twin's --json refusal keeps the registry's fix
// and the message (ARCHITECTURE.md 5.9).
var messageFixes = map[string]string{
	"trace.broken_traces": "more than 5% of the traces miss spans: check the exporters' queues and the sampling, and " +
		"that the capture holds whole requests (not the end of one window and the start of another); trace.edge_loss " +
		"findings, when listed, name the edges",
	"trace.cycle": "the call graph has a cycle: a service calling itself is a call over the network, SERVER spans of " +
		"one instrumentation scope nested in one another, or nested spans far apart; check the subject's instrumentation",
	"trace.too_many_shapes": "the subject's calls take more shapes (counts, orders, overlaps) than the model holds: " +
		"sequential, parallel or staged calls, or a mixture of a few shapes; the capture cannot be fitted as it is",
	"trace.negative_service_time": "the spans' times contradict a first-come-first-served queue: synchronize the " +
		"clocks (NTP) or export CLIENT spans for the calls, and check --workers",
}

// serverFix is a fix the server gives instead of the registry's, by where its caller is: in the default profile over
// stdio where a capture is configured, which can fit it (fit_start), and outside stdio mode (HTTP) or over stdio with
// no capture, which cannot; and in the bench profile, which has no jobs and asks no human, over stdio where a capture
// is configured (benchStdio, where context with the capture fits it; bench when empty) and elsewhere. An empty text
// keeps the registry's fix there.
type serverFix struct{ stdio, remote, bench, benchStdio string }

// serverFixes are the server's fixes for the codes whose registry fix names an action a caller of the server cannot
// take there: change.ambiguous_service's sends the reader to the service's name as the traces wrote it, which the
// server never serves and its input schemas refuse (and pinning workers, fit_start's, needs stdio and a capture);
// fit.version's fits again and stores the record's bytes, which is the operator's to do (a caller over stdio where a
// capture is configured fits again, with fit_start or, in the bench profile, context with the capture; one outside
// stdio mode or with no capture cannot fit); job.failed's starts a job again, and a bench server has no jobs.
var serverFixes = map[string]serverFix{
	"change.ambiguous_service": {
		stdio: "the views show more than one service by this name, and the server cannot tell them apart: change, " +
			"budget or pin another service, or ask the server's operator to run the CLI with the name the traces wrote",
		remote: "the views show more than one service by this name, and the server cannot tell them apart: change or " +
			"budget another service, or ask the server's operator to run the CLI with the name the traces wrote",
		bench: "the views show more than one service by this name, and the server cannot tell them apart: treat the " +
			"change as inconclusive, not as a pass",
	},
	"fit.version": {
		stdio: "the record this names is one this engine does not read (another version's, or stored in another " +
			"form): make it again with fit_start (a fit of the capture, or the A/B), or use the environment's " +
			"baseline (context with env) once it is refreshed",
		remote: "the record this names is one this engine does not read (another version's, or stored in another " +
			"form): use the environment's baseline (context with env) once it is refreshed, or an A/B handle made since",
		bench: "the record this names is one this engine does not read (another version's, or stored in another " +
			"form): use the environment's baseline once it is refreshed; its baseline fit and the A/B are made " +
			"outside this server",
		benchStdio: "the record this names is one this engine does not read (another version's, or stored in " +
			"another form): call context with the capture again, or use the environment's baseline once it is " +
			"refreshed; the A/B is made outside this server",
	},
	"job.failed": {
		bench: "call context with the capture again; if it fails again, treat the change as not passing",
	},
}

// fixOf is the fix the server returns for a code, given the engine's or the registry's: the server's own text for the
// codes whose fix sends the reader where the server does not (serverFixes, by the server's profile and whether its
// caller can fit a capture, fitsCaptures), or to the engine's message, which it withholds (messageFixes); else fix.
func (s *Server) fixOf(code, fix string) string {
	if f, ok := serverFixes[code]; ok {
		t := f.stdio
		switch {
		case s.bench() && s.fitsCaptures():
			t = cmp.Or(f.benchStdio, f.bench)
		case s.bench():
			t = f.bench
		case !s.fitsCaptures():
			t = f.remote
		}
		if t != "" {
			return t
		}
	}
	if fix == fixAsMessage {
		return cmp.Or(messageFixes[code], dataContractFix)
	}
	return fix
}

var codeRE = regexp.MustCompile(`^[a-z]+\.[a-z0-9_]+$`)

// engineRefusal rebuilds a Refusal object the engine printed from its parts that are fixed text or already sanitized:
// the code (a registry token), the fix (the registry's text), the subject (the CLI's sanitizing rule, checked again
// here, and kept back when it names a location of the server: s.subject) and the numbers the registry names for the
// code (registry.Code.Numbers; a record another build stored may carry others). rename, when not nil, maps
// the engine's subject first. The engine's reason is the message its text form prints, which can carry names as the
// traces wrote them, span names with their query strings, and paths of the server's own folders: it is replaced by
// EngineRefusalReason, and neither returned nor logged (ARCHITECTURE.md 5.9). A registry fix that sends the reader to
// that message (fixAsMessage), or to what the server cannot do, is replaced by the server's own text (fixOf). ok is
// false when v is not a Refusal object with a well-formed code.
func (s *Server) engineRefusal(v any, rename func(string) string) (state.Refusal, bool) {
	m, _ := v.(map[string]any)
	code, _ := m["code"].(string)
	if !codeRE.MatchString(code) {
		return state.Refusal{}, false
	}
	r := state.Refusal{Code: code, Reason: EngineRefusalReason, Fix: dataContractFix}
	if fix, ok := m["fix"].(string); ok && plainText(fix, 300) {
		r.Fix = fix
	}
	r.Fix = s.fixOf(code, r.Fix)
	if sub, ok := m["subject"].(string); ok {
		if rename != nil {
			sub = rename(sub)
		}
		r.Subject = s.subject(sub)
	}
	reg, _ := registry.Lookup(code)
	nums, _ := m["numbers"].(map[string]any)
	for k, x := range nums {
		n, ok := x.(json.Number)
		if !ok || !reg.HasNumber(k) {
			continue
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		if r.Numbers == nil {
			r.Numbers = map[string]float64{}
		}
		r.Numbers[k] = f
	}
	return r, true
}

// subject returns the subject of a refusal or a finding as the server may return it, or "" (no subject): a Name
// (ARCHITECTURE.md 5.4) that is not a path from the root, names no peer node (namesNode) and contains none of the
// server's own locations, with an AWS resource name in it shown as the name rule shows one (arnShown: an ARN as its
// last element). The Name rule alone admits a folder and an ARN, and the engine's subjects can name the folders it
// was given, and an engine other than the configured one may have stored the record (ARCHITECTURE.md 5.9).
func (s *Server) subject(sub string) string {
	if namesNode(sub) {
		return ""
	}
	sub, _ = arnShown(sub)
	if !nameRE.MatchString(sub) || strings.HasPrefix(sub, "/") {
		return ""
	}
	for _, p := range s.paths {
		if strings.Contains(sub, p) {
			return ""
		}
	}
	return sub
}

// serverPaths are the locations of the server that no result may name: the configured captures, policies and
// environment roots, the state folder, the engine, and the folder the jobs work in.
func serverPaths(c *config.Config, workDir string) []string {
	out := []string{c.StateDir, c.Engine.Path, workDir}
	for _, m := range []map[string]string{c.Captures, c.Policies} {
		for _, p := range m {
			out = append(out, p)
		}
	}
	for _, e := range c.Envs {
		out = append(out, e.Root)
	}
	// a location that is not absolute (an unset one, or a relative work folder) would match names, not paths
	out = slices.DeleteFunc(out, func(p string) bool { return !filepath.IsAbs(p) })
	slices.Sort(out)
	return slices.Compact(out)
}

// plainText reports whether s is at most max characters of printable text: no control or format characters (a
// bidirectional override is a format character), valid UTF-8.
func plainText(s string, max int) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, c := range s {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || !unicode.IsPrint(c) {
			return false
		}
	}
	return true
}
