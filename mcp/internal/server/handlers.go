// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// File names of the inputs a CLI call reads, in the call's private folder.
const (
	fitFile     = "fit.json"
	changesFile = "changes.json"
	policyFile  = "policy.json"
	targetFile  = "target.json"
)

// fitRef is a resolved fit: the handle to report, its digest and its bytes, and whether it is an environment's
// baseline.
type fitRef struct {
	handle, digest string
	bytes          []byte
	baseline       bool
}

// codeFitVersion is the engine's refusal of a record that its version did not write (fit.version).
const codeFitVersion = "fit.version"

// baselineRefusal is err as a call on a fit returns it. An environment's baseline is made outside the server, so one
// the engine refuses as a record its version did not write (fit.version) is not the caller's to fit again, as the
// registry's fix says: its fix is the server's own text, as for fit.expired. Any other error passes through.
func (s *Server) baselineRefusal(f *fitRef, err error) error {
	var rf *refused
	if !f.baseline || !errors.As(err, &rf) {
		return err
	}
	var env struct {
		Error state.Refusal `json:"error"`
	}
	if json.Unmarshal(rf.body, &env) != nil || env.Error.Code != codeFitVersion {
		return err
	}
	r := env.Error
	r.Fix = "the environment's baseline fit is a record this engine does not read (another version's, or stored in " +
		"another form): " + s.orFitID(s.text("wait for the environment's scheduled refresh",
		"wait for the environment's scheduled refresh; its baseline fit is made outside this server"))
	return refuse(r)
}

// resolveHandle returns the digest p's handle names, refusing an unknown, foreign or expired handle, or one of
// another kind.
func (s *Server) resolveHandle(ctx context.Context, p state.Principal, h, kind string) (string, error) {
	k, d, err := s.opt.Store.Resolve(ctx, p, h)
	if r, ok := s.handleRefusal(err); ok {
		return "", refuse(r)
	}
	if err != nil {
		return "", internalError("the state store failed")
	}
	if k != kind {
		r, _ := s.handleRefusal(state.ErrHandleUnknown)
		return "", refuse(r)
	}
	return d, nil
}

// handleRefusal is state.HandleRefusal with the fix of the server's profile and mode: the bench profile has no
// fit_start, and outside stdio mode, or over stdio with no capture configured, this build's fit_start makes no fit
// (its capture argument is stdio's and names a configured capture, and it does not fit an environment's window), so
// none of them sends the caller there.
func (s *Server) handleRefusal(err error) (state.Refusal, bool) {
	r, ok := state.HandleRefusal(err)
	if ok && (s.bench() || !s.fitsCaptures()) {
		r.Fix = map[string]string{
			state.CodeHandleUnknown: "use a handle this server returned to you, or an env's baseline",
			state.CodeHandleExpired: "use an env's baseline, or a handle returned since",
		}[r.Code]
	}
	return r, ok
}

// bench reports whether the server runs the bench profile (ARCHITECTURE.md 5.3): no fit_start or job_status, and an
// A/B is measured outside the server, by the CI.
func (s *Server) bench() bool { return s.opt.Config.Profile == config.ProfileBench }

// text is the text of the server's profile: full in the default profile, bench in the bench profile.
func (s *Server) text(full, bench string) string {
	if s.bench() {
		return bench
	}
	return full
}

// orFitID is a fix that waits for an environment's baseline, with "name a fit_id, or " before it where a caller can
// make a fit handle of a capture (fitsCaptures: fit_start or context with a capture); elsewhere only context with env
// makes one, the call these fixes answer, so the fix does not send the caller to one.
func (s *Server) orFitID(wait string) string {
	if s.fitsCaptures() {
		return "name a fit_id, or " + wait
	}
	return wait
}

// fitsCaptures reports whether a caller can make a fit handle of a capture: over stdio (the capture argument of
// fit_start and context), where at least one capture is configured. A/B arms are captures too, so without one no
// fit_start makes anything.
func (s *Server) fitsCaptures() bool { return s.opt.LocalCaptures && len(s.opt.Config.Captures) > 0 }

// artifact reads a stored artifact a handle named.
func (s *Server) artifact(ctx context.Context, digest string) ([]byte, error) {
	b, err := s.opt.Store.GetArtifact(ctx, digest)
	if err != nil {
		return nil, internalError("the state store has lost an artifact")
	}
	return b, nil
}

// resolveFit finds the fit a call names: a fit handle; an environment's baseline (a fit handle is minted for it);
// or, in stdio mode, a configured capture fitted now.
func (s *Server) resolveFit(ctx context.Context, c *call, fitID, env, capture string) (*fitRef, error) {
	switch {
	case fitID != "":
		d, err := s.resolveHandle(ctx, c.p, fitID, state.KindFit)
		if err != nil {
			return nil, err
		}
		b, err := s.artifact(ctx, d)
		if err != nil {
			return nil, err
		}
		return &fitRef{handle: fitID, digest: d, bytes: b}, nil
	case env != "":
		if _, err := s.opt.Config.Env(env); err != nil {
			return nil, unknownEnv(s.opt.Config)
		}
		// The fixes name no fit_start: this build does not fit an environment's window (jobs.go), and its baseline is
		// made outside the server.
		d, err := s.opt.Store.Baseline(ctx, env)
		if errors.Is(err, state.ErrNotFound) {
			return nil, refuse(state.Refusal{Code: state.CodeFitNoBaseline,
				Reason: "the environment has no baseline fit yet",
				Fix: s.orFitID(s.text("wait for the environment's first fit",
					"wait for the environment's baseline fit, which is made outside this server"))})
		}
		if err != nil {
			return nil, internalError("the state store failed")
		}
		b, err := s.artifact(ctx, d)
		if err != nil {
			return nil, err
		}
		// A baseline is refreshed by the scheduler; one whose window ended more than FitTTL ago is past its expiry
		// (ARCHITECTURE.md 4.2), and no handle is handed out for it.
		if age, ok := fitAge(b, s.opt.Now()); ok && time.Duration(age)*time.Second > s.opt.FitTTL {
			return nil, refuse(state.Refusal{Code: state.CodeFitExpired,
				Reason: "the environment's baseline fit is past its expiry",
				Fix: s.orFitID(s.text("wait for the environment's scheduled refresh",
					"wait for the environment's scheduled refresh; its baseline fit is made outside this server"))})
		}
		f, err := s.mintFit(ctx, c.p, d)
		if f != nil {
			f.baseline = true
		}
		return f, err
	case capture != "":
		folder, err := s.capture(capture)
		if err != nil {
			return nil, err
		}
		// a fit, run at once: it takes one of the caller's fit slots and the engine's job slots, as fit_start does
		done, ok := s.opt.Limits.StartFit(string(c.p))
		if !ok {
			return nil, refuse(rateLimited(s.opt.PollAfter))
		}
		defer done()
		ctx, cancel := context.WithTimeout(jobCall(ctx), s.opt.JobTimeout)
		defer cancel()
		dir, err := os.MkdirTemp(s.opt.WorkDir, "rhobound-fit-*")
		if err != nil {
			return nil, internalError("cannot make a work folder")
		}
		defer os.RemoveAll(dir)
		d, _, err := s.fitCapture(ctx, folder, capture, nil, dir, nil)
		var sr *stepRefusal
		if errors.As(err, &sr) {
			// the step's refusal, with the server's fix where it has one (job.failed's in the bench profile)
			var r state.Refusal
			if json.Unmarshal(sr.refusal, &r) == nil {
				r.Fix = s.fixOf(r.Code, r.Fix)
				return nil, refuse(r)
			}
			return nil, &refused{body: []byte(`{"error":` + string(sr.refusal) + `}`)}
		}
		if err != nil {
			return nil, err
		}
		return s.mintFit(ctx, c.p, d)
	}
	return nil, invalidParams("name a fit_id, an env or a capture")
}

// mintFit mints a fit handle for p on a stored fit.
func (s *Server) mintFit(ctx context.Context, p state.Principal, digest string) (*fitRef, error) {
	b, err := s.artifact(ctx, digest)
	if err != nil {
		return nil, err
	}
	h, err := s.opt.Store.Mint(ctx, p, state.KindFit, digest, s.opt.FitTTL)
	if err != nil {
		return nil, internalError("the state store failed")
	}
	return &fitRef{handle: h, digest: digest, bytes: b}, nil
}

// capture returns a configured capture's folder. The top-level capture argument exists in stdio mode only.
func (s *Server) capture(name string) (string, error) {
	if !s.opt.LocalCaptures {
		return "", invalidParams("the capture argument is available in stdio mode only")
	}
	return s.registered(name)
}

// registered returns the folder of a capture name in the configuration (also the A/B arms in any mode).
func (s *Server) registered(name string) (string, error) {
	folder, err := s.opt.Config.Capture(name)
	if err != nil {
		return "", invalidParams("unknown capture name")
	}
	return folder, nil
}

// defaultPolicy is the policy name used when a call names none.
const defaultPolicy = "default"

// policy returns a named policy's bytes and its configured name; name "" means "default". A call that names no policy
// on a server without a default is refused (policy.not_named), and so is a name the configuration does not list
// (policy.unknown): budget and predict never answer without a policy's thresholds (ARCHITECTURE.md 5.8).
func (s *Server) policy(name string) (b []byte, configured string, err error) {
	explicit := name != ""
	if !explicit {
		name = defaultPolicy
	}
	path, err := s.opt.Config.Policy(name)
	switch {
	case err != nil && !explicit:
		return nil, "", refuse(state.Refusal{Code: state.CodePolicyNotNamed,
			Reason: "the call names no policy, and the server's configuration has none named default", Fix: s.policyFix()})
	case err != nil:
		return nil, "", refuse(state.Refusal{Code: state.CodePolicyUnknown,
			Reason: "the server's configuration has no policy of that name", Fix: s.policyFix()})
	}
	b, err = os.ReadFile(path)
	if err != nil {
		return nil, "", internalError("cannot read the configured policy")
	}
	return b, name, nil
}

// policyFix is the fix of a policy refusal: the names of the configured policies, sorted, as many as fit in the fix's
// 300 characters. They are names from the server's configuration, which callers pass as the policy argument; nothing
// in them comes from traces.
func (s *Server) policyFix() string {
	names := make([]string, 0, len(s.opt.Config.Policies))
	for n := range s.opt.Config.Policies {
		names = append(names, n)
	}
	if len(names) == 0 {
		return "the server's configuration lists no policy; add one under policies, named default to apply it when a call names none"
	}
	sort.Strings(names)
	const max = 300
	fix := "name one of the configured policies in the policy argument: "
	for i, n := range names {
		sep := ", "
		if i == 0 {
			sep = ""
		}
		if len(fix)+len(sep)+len(n)+len(", ...") > max {
			return fix + sep + "..."
		}
		fix += sep + n
	}
	return fix
}

// cap is the structuredContent cap for a detail level.
func capFor(detail string) int {
	if detail == "full" {
		return FullMaxBytes
	}
	return SummaryMaxBytes
}

// fitKeys are the server keys every view about a fit carries: its handle, its age, and the link to its full summary.
func (s *Server) fitKeys(f *fitRef) map[string]any {
	keys := map[string]any{"fit_id": f.handle, "links": []any{fitURI + f.handle}}
	if age, ok := fitAge(f.bytes, s.opt.Now()); ok {
		keys["fit_age_s"] = age
	}
	return keys
}

// contextTool: rhobound context --fit F --json [--detail full].
func (s *Server) contextTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in struct {
		Env     string `json:"env"`
		FitID   string `json:"fit_id"`
		Capture string `json:"capture"`
		Detail  string `json:"detail"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	f, err := s.resolveFit(ctx, c, in.FitID, in.Env, in.Capture)
	if err != nil {
		return nil, err
	}
	args := []string{"--fit", runner.File(fitFile), "--json"}
	if in.Detail == "full" {
		args = append(args, "--detail=full")
	}
	view, err := s.run(ctx, "context", args, map[string][]byte{fitFile: f.bytes}, 0)
	if err != nil {
		return nil, s.baselineRefusal(f, err)
	}
	return s.viewResult(view, s.fitKeys(f), capFor(in.Detail))
}

// budgetTool: rhobound budget --fit F --policy P [--service=S] [--kinds=K,...] [--callee=C] --json. The policy is the
// named one, else the configured default; without either the call is refused (policy.not_named), so the server never
// returns a budget that no entry threshold binds.
func (s *Server) budgetTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in struct {
		FitID   string   `json:"fit_id"`
		Service string   `json:"service"`
		Kinds   []string `json:"kinds"`
		Callee  string   `json:"callee"`
		Policy  string   `json:"policy"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	pol, _, err := s.policy(in.Policy)
	if err != nil {
		return nil, err
	}
	f, err := s.resolveFit(ctx, c, in.FitID, "", "")
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{fitFile: f.bytes, policyFile: pol}
	args := []string{"--fit", runner.File(fitFile), "--policy", runner.File(policyFile)}
	// Names go in --flag=value form: a name may start with "-".
	if in.Service != "" {
		args = append(args, "--service="+in.Service)
	}
	if len(in.Kinds) > 0 {
		args = append(args, "--kinds="+strings.Join(in.Kinds, ","))
	}
	if in.Callee != "" {
		args = append(args, "--callee="+in.Callee)
	}
	args = append(args, "--json")
	view, err := s.run(ctx, "budget", args, files, 0)
	if err != nil {
		return nil, err
	}
	return s.viewResult(view, s.fitKeys(f), SummaryMaxBytes)
}

// predictIn is the predict tool's input.
type predictIn struct {
	FitID   string `json:"fit_id"`
	Env     string `json:"env"`
	Changes []struct {
		Op      string          `json:"op"`
		Service string          `json:"service"`
		Callee  string          `json:"callee"`
		Value   json.RawMessage `json:"value"`
		Note    string          `json:"note"`
	} `json:"changes"`
	Measured []struct {
		AB      string          `json:"ab"`
		Profile json.RawMessage `json:"profile"`
	} `json:"measured"`
	Assumptions []json.RawMessage `json:"assumptions"`
	Policy      string            `json:"policy"`
	Repo        string            `json:"repo"`
	Branch      string            `json:"branch"`
	Detail      string            `json:"detail"`
}

// predictTool: rhobound preview --fit F --changes C --policy P --now T --json [--detail full], then the preview log.
func (s *Server) predictTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in predictIn
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if len(in.Assumptions) > 0 {
		return nil, refuse(state.Refusal{Code: state.CodeUnsupportedOp,
			Reason: "structural assumptions from a judge answer need the structural patch, which this version lacks",
			Fix: s.text("predict without assumptions, or measure the change with fit_start ab",
				"predict without assumptions; a measured A/B is made outside this server, by the CI")})
	}
	pol, policyName, err := s.policy(in.Policy)
	if err != nil {
		return nil, err
	}
	f, err := s.resolveFit(ctx, c, in.FitID, in.Env, "")
	if err != nil {
		return nil, err
	}
	declared := []any{}
	for _, ch := range in.Changes {
		item := map[string]any{"op": ch.Op, "service": ch.Service, "value": ch.Value, "label": "declared"}
		if ch.Callee != "" {
			item["callee"] = ch.Callee
		}
		if ch.Note != "" {
			item["note"] = ch.Note
		}
		declared = append(declared, item)
	}
	items := append([]any{}, declared...)
	for _, m := range in.Measured {
		if m.AB == "" {
			return nil, refuse(state.Refusal{Code: state.CodeProfileNotFound,
				Reason: "no verified profile record exists in this version",
				Fix: s.text("measure the change with fit_start ab and pass measured: [{ab: <handle>}]",
					"predict with declared changes; a measured A/B is made outside this server, by the CI")})
		}
		more, err := s.measuredItems(ctx, c.p, m.AB)
		if err != nil {
			return nil, err
		}
		items = append(items, more...)
	}
	changes, err := encode(map[string]any{"schema": "rhobound.change/v1", "fit": f.digest, "items": items})
	if err != nil {
		return nil, internalError("cannot encode the change set")
	}
	now := s.opt.Now().UTC()
	args := []string{"--fit", runner.File(fitFile), "--changes", runner.File(changesFile),
		"--policy", runner.File(policyFile), "--now=" + now.Format(time.RFC3339), "--json"}
	if in.Detail == "full" {
		args = append(args, "--detail=full")
	}
	view, err := s.run(ctx, "preview", args,
		map[string][]byte{fitFile: f.bytes, changesFile: changes, policyFile: pol}, 0)
	if err != nil {
		return nil, s.baselineRefusal(f, err)
	}

	var v struct {
		ChangeHash string `json:"change_hash"`
		Preview    struct {
			Verdict string `json:"verdict"`
		} `json:"preview"`
	}
	if json.Unmarshal(view, &v) != nil {
		return nil, internalError("the engine's preview has no verdict")
	}
	decl, _ := encode(declared)
	attempt, err := s.opt.Store.AppendPreview(ctx, state.PreviewKey{Repo: in.Repo, Principal: c.p, Branch: in.Branch},
		state.PreviewRow{Time: now, ChangeHash: v.ChangeHash, Declared: decl, Verdict: v.Preview.Verdict})
	if err != nil {
		return nil, internalError("the preview log failed")
	}
	pred, err := s.storePrediction(ctx, c.p, predRecord{Fit: f.digest, PolicyName: policyName,
		Now: now.Format(time.RFC3339)}, changes, pol, view)
	if err != nil {
		return nil, err
	}
	keys := s.fitKeys(f)
	keys["prediction_id"] = pred
	// One link, to the full report and change set: the judge packet is a section of explain, and the fit's link is
	// in context. A summary preview leaves little room under its cap, and links are the first thing dropped.
	keys["links"] = []any{predictionURI + pred}
	keys["attempt"] = attempt
	max := maxAttempts(pol)
	keys["max_preview_attempts"] = max
	if attempt > max {
		if s.opt.Config.Profile == config.ProfileBench {
			keys["escalate"] = "bench_non_pass"
		} else {
			keys["escalate"] = "ask_a_human"
		}
	}
	return s.viewResult(view, keys, capFor(in.Detail))
}

// measuredItems turns an A/B handle into change items: per changed service of the record, its own-time delta as the
// CLI computed it, labelled measured-ab with the record's digest as evidence. The CLI checks the label against the
// record in its own store and applies the record's standard errors; the server copies, it does not compute.
//
// The changed services are those whose image differs between the arms (DATA.md 8: the record is per changed service;
// the CLI lists every service it measured). A service run from the same image in both arms did not change: its delta
// is noise around zero, and applying it would only widen the band. When the record names no images, or no image
// differs, every service it measured is taken.
func (s *Server) measuredItems(ctx context.Context, p state.Principal, h string) ([]any, error) {
	d, err := s.resolveHandle(ctx, p, h, state.KindAB)
	if err != nil {
		return nil, err
	}
	b, err := s.artifact(ctx, d)
	if err != nil {
		return nil, err
	}
	rec, err := decodeObject(b)
	if err != nil {
		return nil, internalError("the stored A/B record does not parse")
	}
	changed := changedServices(rec)
	svcs, _ := rec["services"].([]any)
	var items []any
	for _, x := range svcs {
		sv, _ := x.(map[string]any)
		name, _ := sv["service"].(string)
		own, _ := sv["own_ms"].(map[string]any)
		delta, ok := own["delta"].(json.Number)
		if name == "" || !ok || (changed != nil && !changed[name]) {
			continue
		}
		items = append(items, map[string]any{"op": "own_ms", "service": name, "value": delta,
			"label": "measured-ab", "evidence": []any{d}})
	}
	if len(items) == 0 {
		return nil, internalError("the A/B record carries no own-time difference")
	}
	return items, nil
}

// changedServices are the services whose image digests differ between the arms of an A/B record, or nil when the
// record names no images or no image differs (then every service counts).
func changedServices(rec map[string]any) map[string]bool {
	arms, _ := rec["arms"].(map[string]any)
	images := func(arm string) map[string]any {
		a, _ := arms[arm].(map[string]any)
		m, _ := a["images"].(map[string]any)
		return m
	}
	base, cand := images("baseline"), images("candidate")
	if len(base) == 0 && len(cand) == 0 {
		return nil
	}
	digests := func(v any) string {
		list, _ := v.([]any)
		var out []string
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return strings.Join(out, "\n")
	}
	changed := map[string]bool{}
	for _, m := range []map[string]any{base, cand} {
		for svc := range m {
			if digests(base[svc]) != digests(cand[svc]) {
				changed[svc] = true
			}
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return changed
}

// storePrediction keeps what a prediction was computed from, so its handle can be explained later, and mints the
// prediction handle. The record is the server's own; the published documents are the parts it names by digest.
func (s *Server) storePrediction(ctx context.Context, p state.Principal, r predRecord, changes, pol, view []byte) (string, error) {
	put := func(kind string, b []byte) string {
		d, err := s.opt.Store.PutArtifact(ctx, kind, b)
		if err != nil {
			return ""
		}
		return d
	}
	r.Changes, r.Policy, r.View = put("change", changes), put("policy", pol), put("view", view)
	if r.Changes == "" || r.Policy == "" || r.View == "" {
		return "", internalError("the state store failed")
	}
	rec, err := encode(map[string]any{"fit": r.Fit, "changes": r.Changes, "policy": r.Policy,
		"policy_name": r.PolicyName, "view": r.View, "now": r.Now})
	if err != nil {
		return "", internalError("cannot encode the prediction record")
	}
	d := put("prediction", rec)
	if d == "" {
		return "", internalError("the state store failed")
	}
	h, err := s.opt.Store.Mint(ctx, p, state.KindPrediction, d, s.opt.PredTTL)
	if err != nil {
		return "", internalError("the state store failed")
	}
	return h, nil
}

// maxAttempts reads max_preview_attempts from a policy (ARCHITECTURE.md 6.2), DefaultMaxAttempt when absent.
func maxAttempts(policy []byte) int {
	var p struct {
		Max json.Number `json:"max_preview_attempts"`
	}
	if json.Unmarshal(policy, &p) == nil && p.Max != "" {
		if n, err := strconv.Atoi(string(p.Max)); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxAttempt
}

// explainTool: rhobound explain [--target T] --section S --max-chars N --json. A prediction's target is its full report
// v2, computed from the stored fit and change set (rhobound predict --fit F --changes C --json), and explained under
// the policy, the clock and the fit it was previewed with, so its rules and judge packet are the preview's; both the
// report and the section are computed once (derive).
func (s *Server) explainTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in struct {
		Target   string `json:"target"`
		Section  string `json:"section"`
		MaxChars int    `json:"max_chars"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	if in.MaxChars == 0 {
		in.MaxChars = 2000
	}
	files := map[string][]byte{}
	args := []string{}
	tail := []string{"--section=" + in.Section, "--max-chars=" + strconv.Itoa(in.MaxChars), "--json"}
	switch {
	case in.Target == "" && in.Section != "data_contract" && in.Section != "scope":
		return nil, invalidParams("this section needs a target handle")
	case predictionOnly[in.Section] && state.HandleKind(in.Target) != state.KindPrediction:
		return nil, invalidParams("the section " + in.Section + " needs a prediction handle as its target")
	case state.HandleKind(in.Target) == state.KindPrediction:
		// A prediction's sections are evaluated with its judge packet (budgets and leverage): computed once each.
		pr, err := s.loadPrediction(ctx, c.p, in.Target)
		if err != nil {
			return nil, err
		}
		ctx, cancel := s.chain(ctx)
		defer cancel()
		view, err := s.derive(ctx, pr, "explain", strings.Join(tail, " "), func(ctx context.Context) ([]byte, error) {
			rep, err := s.report(ctx, pr)
			if err != nil {
				return nil, err
			}
			files := pr.files()
			files[targetFile] = rep
			args := append([]string{"--target", runner.File(targetFile)}, pr.policyArgs()...)
			return s.run(ctx, "explain", append(args, tail...), files, 0)
		})
		if err != nil {
			return nil, err
		}
		return s.viewResult(view, nil, FullMaxBytes)
	case in.Target != "":
		b, err := s.explainTarget(ctx, c.p, in.Target)
		if err != nil {
			return nil, err
		}
		files[targetFile] = b
		args = append(args, "--target", runner.File(targetFile))
	default:
		// scope or data_contract: the same text for every caller, remembered (staticView)
		view, err := s.staticView(ctx, tail)
		if err != nil {
			return nil, err
		}
		return s.viewResult(view, nil, FullMaxBytes)
	}
	view, err := s.run(ctx, "explain", append(args, tail...), files, 0)
	if err != nil {
		return nil, err
	}
	return s.viewResult(view, nil, FullMaxBytes)
}

// predictionOnly are the explain sections of a prediction only: the CLI explains them from a report v2 (assumptions)
// or from its gate evaluation (rules, judge_packet), never from a fit or an A/B record.
var predictionOnly = map[string]bool{"assumptions": true, "rules": true, "judge_packet": true}

// explainTarget returns the document a fit or A/B handle stands for.
func (s *Server) explainTarget(ctx context.Context, p state.Principal, h string) ([]byte, error) {
	switch kind := state.HandleKind(h); kind {
	case state.KindFit, state.KindAB:
		d, err := s.resolveHandle(ctx, p, h, kind)
		if err != nil {
			return nil, err
		}
		return s.artifact(ctx, d)
	}
	r, _ := s.handleRefusal(state.ErrHandleUnknown)
	return nil, refuse(r)
}

// predRecord is the server's record of a prediction: the digests of the fit, the change set, the policy and the view
// it previewed, the policy's configured name, and the time the fit's age was measured at (RFC 3339).
type predRecord struct {
	Fit        string `json:"fit"`
	Changes    string `json:"changes"`
	Policy     string `json:"policy"`
	PolicyName string `json:"policy_name"`
	View       string `json:"view"`
	Now        string `json:"now"`
}

// prediction is a stored prediction with the documents it was computed from, the digest of its record, and the
// caller it was resolved for.
type prediction struct {
	rec                  predRecord
	fit, changes, policy []byte
	digest               string
	p                    state.Principal
}

// loadPrediction resolves p's prediction handle and reads what the prediction was computed from.
func (s *Server) loadPrediction(ctx context.Context, p state.Principal, h string) (*prediction, error) {
	d, err := s.resolveHandle(ctx, p, h, state.KindPrediction)
	if err != nil {
		return nil, err
	}
	b, err := s.artifact(ctx, d)
	if err != nil {
		return nil, err
	}
	pr := &prediction{digest: d, p: p}
	var ok bool
	if pr.rec, ok = s.predictionRecord(b); !ok {
		r, _ := s.handleRefusal(state.ErrHandleUnknown)
		return nil, refuse(r)
	}
	for _, x := range []struct {
		digest string
		into   *[]byte
	}{{pr.rec.Fit, &pr.fit}, {pr.rec.Changes, &pr.changes}, {pr.rec.Policy, &pr.policy}} {
		if *x.into, err = s.artifact(ctx, x.digest); err != nil {
			return nil, err
		}
	}
	return pr, nil
}

// predictionRecord rebuilds a stored prediction record as the server writes one (storePrediction), never as stored: a
// record may have been written by another build of the server (ARCHITECTURE.md 5.9), and its policy name and clock
// reach the engine's flags and the judge packet. The digests must be digests and the clock RFC 3339, or ok is false
// (the handle reads as unknown); a policy name the configuration does not list (none, one removed since, or one an
// older build allowed) becomes "policy", the judge packet's name for a policy without one.
func (s *Server) predictionRecord(b []byte) (predRecord, bool) {
	var r predRecord
	if json.Unmarshal(b, &r) != nil {
		return predRecord{}, false
	}
	for _, d := range []string{r.Fit, r.Changes, r.Policy, r.View} {
		if !digestRE.MatchString(d) {
			return predRecord{}, false
		}
	}
	if _, err := time.Parse(time.RFC3339, r.Now); err != nil {
		return predRecord{}, false
	}
	if r.PolicyName == "" {
		r.PolicyName = defaultPolicy
	}
	if _, err := s.opt.Config.Policy(r.PolicyName); err != nil || !config.ValidName(r.PolicyName) {
		r.PolicyName = unnamedPolicy
	}
	return r, true
}

// unnamedPolicy is the name a prediction record's policy is shown by when the configuration does not list its own.
const unnamedPolicy = "policy"

// digestRE is a content id (defs.v1 Digest).
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// files are the input files of a CLI call about a prediction: its fit and its policy (the report goes in separately).
func (pr *prediction) files() map[string][]byte {
	return map[string][]byte{fitFile: pr.fit, policyFile: pr.policy}
}

// policyArgs are the flags that evaluate a prediction's report as its preview did: the policy and its configured
// name, the preview's clock, and the fit (for the judge packet's budgets and leverage).
func (pr *prediction) policyArgs() []string {
	return []string{"--policy", runner.File(policyFile), "--policy-name=" + pr.rec.PolicyName,
		"--now=" + pr.rec.Now, "--fit", runner.File(fitFile)}
}

// derive returns a document computed from a stored prediction (kind: its report, its judge packet, an explain
// section; variant tells apart documents of one kind). The engine computes each once per prediction record while the
// server remembers it (the MaxDerived most recently used): the computation runs the engine as a prediction does (a
// solve, and the judge packet's budgets), so it takes a token from the caller's predict bucket, and later reads come
// from the store and cost nothing. Without this, every explain, resource read and prompt on one prediction handle
// would be a free solve (ARCHITECTURE.md 10). The caller bounds the whole computation by one CallTimeout (chain).
//
// One request pays one token, however many documents it computes: compute gets a context marked as charged, and a
// document it derives in turn (the report under a packet or a section) takes none. So a caller refused here and told
// retry_after_ms needs one token when it retries, as the hint says.
func (s *Server) derive(ctx context.Context, pr *prediction, kind, variant string, compute func(context.Context) ([]byte, error)) ([]byte, error) {
	key := pr.digest + "\x00" + kind + "\x00" + variant
	if d, ok := s.derived.get(key); ok {
		if b, err := s.opt.Store.GetArtifact(ctx, d); err == nil {
			return b, nil
		}
	}
	if charged, _ := ctx.Value(chargedKey{}).(bool); !charged {
		if ok, wait := s.opt.Limits.Allow(string(pr.p), "predict"); !ok {
			return nil, refuse(rateLimited(wait))
		}
		ctx = context.WithValue(ctx, chargedKey{}, true)
	}
	b, err := compute(ctx)
	if err != nil {
		return nil, err
	}
	if d, err := s.opt.Store.PutArtifact(ctx, kind, b); err == nil {
		s.derived.put(key, d)
	}
	return b, nil
}

// chargedKey marks a context whose request has paid for the documents it derives (derive).
type chargedKey struct{}

// report is a prediction's full report v2 (rhobound predict --fit F --changes C --json), computed once.
func (s *Server) report(ctx context.Context, pr *prediction) ([]byte, error) {
	ctx, cancel := s.chain(ctx)
	defer cancel()
	return s.derive(ctx, pr, "report", "", func(ctx context.Context) ([]byte, error) {
		return s.run(ctx, "predict", []string{"--fit", runner.File(fitFile), "--changes", runner.File(changesFile), "--json"},
			map[string][]byte{fitFile: pr.fit, changesFile: pr.changes}, 0)
	})
}

// packet is a prediction's judge packet (rhobound judge-packet REPORT --policy P --policy-name N --now T --fit F),
// computed once.
func (s *Server) packet(ctx context.Context, pr *prediction) ([]byte, error) {
	ctx, cancel := s.chain(ctx)
	defer cancel()
	return s.derive(ctx, pr, "packet", "", func(ctx context.Context) ([]byte, error) {
		rep, err := s.report(ctx, pr)
		if err != nil {
			return nil, err
		}
		files := pr.files()
		files[targetFile] = rep
		args := append([]string{runner.File(targetFile)}, pr.policyArgs()...)
		return s.run(ctx, "judge-packet", append(args, "--json"), files, 0)
	})
}

// viewResult adds the server keys to a CLI view and makes the tool result.
func (s *Server) viewResult(view []byte, keys map[string]any, max int) (*mcp.CallToolResult, error) {
	out, err := withServerKeys(view, keys, max)
	if err != nil {
		return nil, internalError("the engine's view cannot take the server's keys within the result's size cap")
	}
	return result(out, false), nil
}
