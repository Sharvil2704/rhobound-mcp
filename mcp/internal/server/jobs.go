// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// Output files the fit steps write into their --out folder.
const (
	fitOut = "fit.json"       // rhobound fit
	abOut  = "ab-record.json" // rhobound diff-fit
)

// Exit statuses of rhobound check (its own exit space).
const (
	checkAdmitted = 0
	checkRefused  = 4
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)

// jobSpec is what a fit job does: fit a capture folder (with pinned workers), or diff two A/B arms. Each folder comes
// with its registered name, which the engine records instead of the folder.
type jobSpec struct {
	kind                        string // "capture" or "ab"
	folder, name                string
	workers                     map[string]int
	baseline, candidate         string
	baselineName, candidateName string
}

// stepRefusal is a refusal met by a job step: the Refusal object, and the findings so far.
type stepRefusal struct {
	refusal  json.RawMessage
	findings json.RawMessage
}

func (e *stepRefusal) Error() string { return "the step was refused" }

// stepFailed is a step that failed without a coded refusal: the engine exited without one (or printed no report), or
// wrote no record, or not the one it reported. The job fails with job.failed, and context{capture} refuses with it, so
// that no failure reaches a caller without a code. findings are the check's, when it ran.
func stepFailed(findings json.RawMessage) error {
	return &stepRefusal{refusal: state.FailedRefusal().JSON(), findings: findings}
}

// fitStartTool starts a fit or an A/B difference as a job and returns its handle. The same request from the same
// caller, on folders whose files have not changed since, under the same engine, returns the job it already started,
// unless that job failed or was cancelled; a refreshed capture folder is fitted again, and so is any after the engine
// is upgraded, whose records the new engine may not read (fit.version).
func (s *Server) fitStartTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in struct {
		Env     string                                `json:"env"`
		Capture string                                `json:"capture"`
		AB      *struct{ Baseline, Candidate string } `json:"ab"`
		Workers map[string]int                        `json:"workers"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	for name := range in.Workers {
		// The CLI reads --workers as svc=N,svc=N: a name must not carry "," or "=" (Name excludes both).
		if !nameRE.MatchString(name) {
			return nil, invalidParams("a workers key is not a service name")
		}
	}
	var sp jobSpec
	switch {
	case in.Env != "":
		if _, err := s.opt.Config.Env(in.Env); err != nil {
			return nil, unknownEnv(s.opt.Config)
		}
		// not a server fault: no retry can succeed until the fetcher is wired (ARCHITECTURE.md 5.3)
		return nil, invalidParams("fitting an environment window needs the trace-store fetcher, which this build does not wire")
	case in.Capture != "":
		folder, err := s.capture(in.Capture)
		if err != nil {
			return nil, err
		}
		sp = jobSpec{kind: "capture", folder: folder, name: in.Capture, workers: in.Workers}
	case in.AB != nil:
		a, err := s.registered(in.AB.Baseline)
		if err != nil {
			return nil, err
		}
		b, err := s.registered(in.AB.Candidate)
		if err != nil {
			return nil, err
		}
		sp = jobSpec{kind: "ab", baseline: a, candidate: b, baselineName: in.AB.Baseline, candidateName: in.AB.Candidate,
			workers: in.Workers}
	default:
		return nil, invalidParams("name an env, a capture or an ab pair")
	}

	// The idempotency key is the canonical request, so key order and spacing do not matter, the listing of the
	// folders it reads, so a folder whose files changed is a new request, and the configured engine's sha256, so that
	// the request repeated after an upgrade makes the record again rather than return one the new engine refuses.
	key, err := encode(map[string]any{"tool": "fit_start", "request": json.RawMessage(c.args), "folders": sp.listing(),
		"engine": s.opt.Config.Engine.SHA256})
	if err != nil {
		return nil, internalError("cannot encode the request")
	}
	k := state.Digest(key)
	// One find-or-start at a time: the same request sent twice at once starts one job.
	s.starting.Lock()
	defer s.starting.Unlock()
	if j, ok, err := s.opt.Store.FindJob(ctx, c.p, k); err != nil {
		return nil, internalError("the state store failed")
	} else if ok {
		return s.started(j)
	}
	done, ok := s.opt.Limits.StartFit(string(c.p))
	if !ok {
		return nil, refuse(rateLimited(s.opt.PollAfter))
	}
	now := s.opt.Now()
	id, err := state.NewHandle(state.KindJob, now)
	if err != nil {
		done()
		return nil, internalError("cannot mint a job handle")
	}
	step := "check"
	if sp.kind == "ab" {
		step = "diff"
	}
	j := state.Job{ID: id, Principal: c.p, Key: k, State: state.JobWorking, Step: step,
		CreatedAt: now.UTC(), DeadlineAt: now.Add(s.opt.JobTimeout).UTC()}
	if err := s.opt.Store.PutJob(ctx, j); err != nil {
		done()
		return nil, internalError("the state store failed")
	}
	s.jobs.Add(1)
	go func() {
		// The fit slot is given back before WaitJobs can return: a caller that saw the job end may start the next.
		defer s.jobs.Done()
		defer done()
		s.runJob(j, sp)
	}()
	return s.started(j)
}

// listing identifies the content of the folders a job reads: the sha256 of every file's path in its folder, size and
// modification time, per folder, in walk order. A folder that cannot be read lists as "" (the job then fails at check).
func (sp jobSpec) listing() []string {
	var out []string
	for _, dir := range []string{sp.folder, sp.baseline, sp.candidate} {
		if dir == "" {
			continue
		}
		h := sha256.New()
		err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, p)
			fmt.Fprintf(h, "%q %d %d\n", rel, info.Size(), info.ModTime().UnixNano())
			return nil
		})
		if err != nil {
			out = append(out, "")
			continue
		}
		out = append(out, hex.EncodeToString(h.Sum(nil)))
	}
	return out
}

// started is fit_start's result for a job.
func (s *Server) started(j state.Job) (*mcp.CallToolResult, error) {
	out := map[string]any{"job_id": j.ID, "state": j.State, "deadline_at": j.DeadlineAt.UTC().Format(time.RFC3339),
		"poll_after_ms": s.opt.PollAfter.Milliseconds()}
	if j.State != state.JobWorking {
		out["state"] = state.JobWorking // fit_start's output is always "working"; job_status tells the rest
		out["poll_after_ms"] = 0
	}
	b, err := encode(out)
	if err != nil {
		return nil, internalError("cannot encode the job")
	}
	return result(b, false), nil
}

// WaitJobs waits until every job this server started has finished writing its result.
func (s *Server) WaitJobs() { s.jobs.Wait() }

// runJob runs a job to its end, within the job timeout, and records the outcome. A job that outlives its deadline is
// reported failed with job.timeout by the store whatever the worker does.
//
// The worker's own bound is the job timeout from now, on the process's clock. deadline_at is on the server's clock
// (Options.Now), which tests and replays pin to the capture's time; a context deadline taken from it would be long
// past and kill the engine at once.
func (s *Server) runJob(j state.Job, sp jobSpec) {
	ctx, cancel := context.WithTimeout(jobCall(context.Background()), s.opt.JobTimeout)
	defer cancel()
	setStep := func(step string) {
		j.Step = step
		_ = s.opt.Store.PutJob(ctx, j)
	}
	dir, err := os.MkdirTemp(s.opt.WorkDir, "rhobound-job-*")
	if err != nil {
		s.finish(ctx, j, "", nil, err)
		return
	}
	defer os.RemoveAll(dir)

	switch sp.kind {
	case "capture":
		d, findings, err := s.fitCapture(ctx, sp.folder, sp.name, sp.workers, dir, setStep)
		if err != nil {
			s.finish(ctx, j, "", findings, err)
			return
		}
		h, err := s.opt.Store.Mint(ctx, j.Principal, state.KindFit, d, s.opt.FitTTL)
		s.finish(ctx, j, h, findings, err)
	case "ab":
		setStep("diff")
		d, err := s.diffAB(ctx, sp, dir)
		if err != nil {
			s.finish(ctx, j, "", nil, err)
			return
		}
		h, err := s.opt.Store.Mint(ctx, j.Principal, state.KindAB, d, s.opt.FitTTL)
		s.finish(ctx, j, h, nil, err)
	}
}

// finish records a job's outcome: completed with its result handle, or failed with the step's refusal, or with
// job.timeout when the job ran out of time (its own context, an engine call past the runner's timeout, or deadline_at
// on the server's clock).
func (s *Server) finish(ctx context.Context, j state.Job, result string, findings json.RawMessage, err error) {
	j.Findings = findings
	var sr *stepRefusal
	switch {
	case err == nil:
		j.State, j.Result = state.JobCompleted, result
	case errors.As(err, &sr):
		j.State, j.Refusal = state.JobFailed, sr.refusal
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, runner.ErrTimeout) || !s.opt.Now().Before(j.DeadlineAt):
		j.State, j.Refusal = state.JobFailed, state.TimeoutRefusal().JSON()
	default:
		// a failure the server met itself (a work folder it could not make, a store that failed): coded all the same
		j.State, j.Refusal = state.JobFailed, state.FailedRefusal().JSON()
	}
	// Written with a fresh context: the job's own may be past its deadline, and the record must still land.
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.opt.Store.PutJob(wctx, j)
}

// fitCapture runs rhobound check, then rhobound fit, on a capture folder and stores the fit. The fit records the
// capture by its registered name (fit --name), never by the server's folder: any server that has the same capture under
// the same name makes the same fit, and the record names no folder of this machine (ARCHITECTURE.md 5.9). It returns
// the fit's digest and the check's findings. A refusal (check exit 4, or a coded refusal from fit) is a *stepRefusal.
func (s *Server) fitCapture(ctx context.Context, folder, name string, workers map[string]int, dir string, step func(string)) (string, json.RawMessage, error) {
	if step == nil {
		step = func(string) {}
	}
	step("check")
	out, exit, err := s.opt.Runner.Run(ctx, "check", []string{folder, "--json"}, nil)
	if err != nil {
		return "", nil, engineError(err)
	}
	var rep struct {
		Findings []json.RawMessage `json:"findings"`
	}
	if (exit != checkAdmitted && exit != checkRefused) || json.Unmarshal(out, &rep) != nil {
		return "", nil, stepFailed(nil)
	}
	findings, _ := encode(rep.Findings)
	if rep.Findings == nil {
		findings = json.RawMessage(`[]`)
	}
	if exit == checkRefused {
		r, ok := s.refusalFromFindings(rep.Findings)
		if !ok {
			return "", findings, stepFailed(findings)
		}
		return "", findings, &stepRefusal{refusal: r, findings: findings}
	}

	step("fit")
	// Names go in --flag=value form: a name may start with "-".
	args := []string{folder, "--name=" + name}
	if w := workersFlag(workers); w != "" {
		args = append(args, w)
	}
	// --json: a refusal is then the coded {"error": Refusal} on standard output, and success {"fit_digest": ...}.
	args = append(args, "--out", dir, "--json")
	out, exit, err = s.opt.Runner.Run(ctx, "fit", args, nil)
	if err != nil {
		return "", findings, engineError(err)
	}
	if exit != 0 {
		if r, ok := s.cliRefusal(out, nil); ok {
			return "", findings, &stepRefusal{refusal: r, findings: findings}
		}
		return "", findings, stepFailed(findings)
	}
	b, err := os.ReadFile(filepath.Join(dir, fitOut))
	if err != nil {
		return "", findings, stepFailed(findings)
	}
	if !printedDigest(out, "fit_digest", b) {
		return "", findings, stepFailed(findings)
	}
	d, err := s.opt.Store.PutArtifact(ctx, "fit", b)
	if err != nil {
		return "", findings, internalError("the state store failed")
	}
	return d, findings, nil
}

// diffAB runs rhobound diff-fit on two A/B arms and stores the A/B record. The load generator's log is the arms' own
// loadgen.csv (the baseline's, else the candidate's), which diff-fit finds itself. The record names each arm by its
// registered name (--baseline-name, --candidate-name), as fitCapture names a capture.
func (s *Server) diffAB(ctx context.Context, sp jobSpec, dir string) (string, error) {
	args := []string{"--baseline", sp.baseline, "--candidate", sp.candidate,
		"--baseline-name=" + sp.baselineName, "--candidate-name=" + sp.candidateName}
	if w := workersFlag(sp.workers); w != "" {
		args = append(args, w)
	}
	out, exit, err := s.opt.Runner.Run(ctx, "diff-fit", append(args, "--out", dir, "--json"), nil)
	if err != nil {
		return "", engineError(err)
	}
	if exit != 0 {
		// A refusal about one arm (ab.sampled) names it as the record does; one that names the arm's folder, or a file
		// in it, is about that arm, and the caller knows the arm by its registered name.
		arm := func(sub string) string {
			for _, a := range [][2]string{{sp.baseline, sp.baselineName}, {sp.candidate, sp.candidateName}} {
				if sub == a[0] || strings.HasPrefix(sub, a[0]+string(filepath.Separator)) {
					return a[1]
				}
			}
			return sub
		}
		if r, ok := s.cliRefusal(out, arm); ok {
			return "", &stepRefusal{refusal: r}
		}
		return "", stepFailed(nil)
	}
	b, err := os.ReadFile(filepath.Join(dir, abOut))
	if err != nil {
		return "", stepFailed(nil)
	}
	if !printedDigest(out, "ab_digest", b) {
		return "", stepFailed(nil)
	}
	d, err := s.opt.Store.PutArtifact(ctx, "ab", b)
	if err != nil {
		return "", internalError("the state store failed")
	}
	return d, nil
}

// printedDigest reports whether what a step printed with --json ({key: digest}) names the file it wrote. Output that
// is not such an object is not checked: the file is what the server stores.
func printedDigest(out []byte, key string, file []byte) bool {
	obj, err := decodeObject(out)
	if err != nil {
		return true
	}
	d, ok := obj[key].(string)
	return !ok || d == state.Digest(file)
}

// workersFlag is --workers=svc=N,... in service order, or "" for none.
func workersFlag(w map[string]int) string {
	if len(w) == 0 {
		return ""
	}
	names := make([]string, 0, len(w))
	for n := range w {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + "=" + strconv.Itoa(w[n])
	}
	return "--workers=" + strings.Join(parts, ",")
}

// cliRefusal returns the Refusal object of a CLI's {"error": Refusal} output, rebuilt by engineRefusal (rename as
// there): the CLI's reason, which can name the server's own folders and carry trace strings, is not passed on.
func (s *Server) cliRefusal(out []byte, rename func(string) string) (json.RawMessage, bool) {
	obj, err := decodeObject(out)
	if err != nil || len(obj) != 1 {
		return nil, false
	}
	r, ok := s.engineRefusal(obj["error"], rename)
	if !ok {
		return nil, false
	}
	return r.JSON(), true
}

// refusalFromFindings is the job refusal for a capture check refused: the first refuse-level finding's code, fix
// and subject (as s.subject returns it).
func (s *Server) refusalFromFindings(findings []json.RawMessage) (json.RawMessage, bool) {
	for _, raw := range findings {
		var f struct {
			Code             string `json:"code"`
			Level            string `json:"level"`
			Subject          string `json:"subject"`
			SubjectUntrusted bool   `json:"subject_untrusted"`
			Fix              string `json:"fix"`
		}
		if json.Unmarshal(raw, &f) != nil || f.Level != "refuse" || !codeRE.MatchString(f.Code) {
			continue
		}
		r := state.Refusal{Code: f.Code, Reason: checkRefusedReason, Fix: f.Fix}
		if !plainText(f.Fix, 300) {
			r.Fix = "see the code in explain section data_contract"
		}
		r.Subject = s.subject(f.Subject)
		return r.JSON(), true
	}
	return nil, false
}

// jobStatusTool reports a job, long-polling up to wait_s for it to leave "working" (not in Lambda mode). A principal
// holds at most MaxPolls long-polls at once; over that, job_status answers at once, with poll_after_ms.
func (s *Server) jobStatusTool(ctx context.Context, c *call) (*mcp.CallToolResult, error) {
	var in struct {
		JobID string `json:"job_id"`
		WaitS int    `json:"wait_s"`
	}
	if err := c.decode(&in); err != nil {
		return nil, err
	}
	j, err := s.opt.Store.GetJob(ctx, c.p, in.JobID)
	if r, ok := s.handleRefusal(err); ok {
		return nil, refuse(r)
	}
	if err != nil {
		return nil, internalError("the state store failed")
	}
	wait := min(time.Duration(in.WaitS)*time.Second, maxWait)
	if s.opt.NoLongPoll || j.State != state.JobWorking {
		wait = 0
	}
	if wait > 0 {
		if done, ok := s.startPoll(c.p); ok {
			defer done()
		} else {
			wait = 0 // the caller's long-polls are all open: answer at once
		}
	}
	if wait > 0 {
		deadline := time.NewTimer(wait)
		defer deadline.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
	poll:
		for j.State == state.JobWorking {
			select {
			case <-ctx.Done():
				break poll
			case <-deadline.C:
				break poll
			case <-tick.C:
				if j, err = s.opt.Store.GetJob(ctx, c.p, in.JobID); err != nil {
					return nil, internalError("the state store failed")
				}
			}
		}
	}
	// The store returns only a job whose state, step, result and id are ones the server writes (state.GetJob); its
	// refusal and findings are rebuilt here (jobRefusal, jobFindings).
	out := map[string]any{"state": j.State, "deadline_at": j.DeadlineAt.UTC().Format(time.RFC3339)}
	if j.Step != "" {
		out["step"] = j.Step
	}
	switch state.HandleKind(j.Result) {
	case state.KindFit:
		out["fit_id"] = j.Result
	case state.KindAB:
		out["ab_id"] = j.Result
	}
	if len(j.Refusal) > 0 {
		out["refusal"] = s.jobRefusal(j.Refusal)
	}
	if j.State == state.JobWorking {
		out["poll_after_ms"] = s.opt.PollAfter.Milliseconds()
	}
	b, err := s.capJobStatus(out, j.Findings)
	if err != nil {
		return nil, internalError("cannot encode the job")
	}
	return result(b, false), nil
}

// checkRefusedReason is the reason of a job refusal made from a check's refuse finding (refusalFromFindings).
const checkRefusedReason = "check refused the capture"

// jobRefusal is a job's stored refusal as job_status returns it, rebuilt as the server builds one, never as stored: a
// job record may have been written by another build of the server or its worker (ARCHITECTURE.md 5.9). The server's own
// refusals (job.timeout, job.failed) are its fixed text. Any other is an engine refusal (engineRefusal): its code (a
// registry token), the reason the server gives (EngineRefusalReason, or checkRefusedReason for one a check made), the
// registry's fix for the code (registry.Lookup; the server's own text for the codes whose fix is the engine's message
// or an action the server cannot take, fixOf; for a code the registry does not hold, a pointer to it), the subject as
// s.subject returns it, and the numbers whose names the registry gives the code; no other key. A stored refusal
// without a well-formed code is job.failed.
func (s *Server) jobRefusal(raw json.RawMessage) json.RawMessage {
	m, err := decodeObject(raw)
	if err != nil {
		return state.FailedRefusal().JSON()
	}
	switch m["code"] {
	case state.CodeJobTimeout:
		return state.TimeoutRefusal().JSON()
	case state.CodeJobFailed:
		return state.FailedRefusal().JSON()
	}
	r, ok := s.engineRefusal(m, nil)
	if !ok {
		return state.FailedRefusal().JSON()
	}
	r.Fix = dataContractFix
	if c, ok := registry.Lookup(r.Code); ok {
		r.Fix = s.fixOf(r.Code, c.Fix)
	}
	if m["reason"] == checkRefusedReason {
		r.Reason = checkRefusedReason
	}
	return r.JSON()
}

// jobFindings projects the check's findings for job_status, whatever their size. Each keeps its code, the registry's
// level and fix for it (registry.Lookup; the server's own text where fixOf gives one), its subject (as s.subject
// returns it), subject_untrusted and its finite numbers whose names the registry gives the code: a job record may have
// been written by another build of the server or its worker, so what it says is never shown as stored. Examples are
// never listed: trace.peer_renamed's are raw server.address values, and the others are trace ids and keys as the
// traces wrote them (ARCHITECTURE.md 5.9); examples counts them. A finding whose code the registry does not hold is not
// listed, and is counted in omitted. ok is false when raw is not a list of findings.
func (s *Server) jobFindings(raw json.RawMessage) (fs []any, examples, omitted int, ok bool) {
	var all []map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&all) != nil {
		return nil, 0, 0, false
	}
	fs = make([]any, 0, len(all))
	for _, f := range all {
		ex, _ := f["examples"].([]any)
		examples += len(ex)
		code, _ := f["code"].(string)
		reg, ok := registry.Lookup(code)
		if !ok {
			omitted++
			continue
		}
		g := map[string]any{"code": code, "level": reg.Level, "fix": s.fixOf(code, reg.Fix)}
		if u, ok := f["subject_untrusted"].(bool); ok {
			g["subject_untrusted"] = u
		}
		if sub, ok := f["subject"].(string); ok && s.subject(sub) != "" {
			g["subject"] = s.subject(sub)
			if g["subject"] != sub {
				g["subject_untrusted"] = true // shown as the name rule shows it, not as written
			}
		}
		nums, _ := f["numbers"].(map[string]any)
		kept := map[string]any{}
		for k, x := range nums {
			n, ok := x.(json.Number)
			if !ok || !reg.HasNumber(k) {
				continue
			}
			if v, err := n.Float64(); err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				continue
			}
			kept[k] = n
		}
		if len(kept) > 0 {
			g["numbers"] = kept
		}
		fs = append(fs, g)
	}
	return fs, examples, omitted, true
}

// capJobStatus encodes a job_status result with the check's findings (jobFindings) within the summary cap
// (ARCHITECTURE.md 5.9). When they make it larger, the findings' numbers go; then notes; then doubts and refusals, from
// the last. What is not listed is counted in findings_omitted, the examples in examples_omitted, and the refusal, if
// any, always stays. The full list is the check's (rhobound check CAPTURE --json), and a completed fit's is behind
// rhobound://fit/ and explain section findings, its examples sanitized.
func (s *Server) capJobStatus(out map[string]any, raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return encode(out)
	}
	fs, examples, omitted, ok := s.jobFindings(raw)
	if !ok {
		return encode(out)
	}
	if examples > 0 {
		out["examples_omitted"] = examples
	}
	var b []byte
	var err error
	fits := func() bool {
		out["findings"] = fs
		if omitted > 0 {
			out["findings_omitted"] = omitted
		}
		b, err = encode(out)
		return err != nil || len(b) <= SummaryMaxBytes
	}
	if fits() {
		return b, err
	}
	for _, f := range fs {
		delete(f.(map[string]any), "numbers")
	}
	if fits() {
		return b, err
	}
	for _, level := range []string{"note", "doubt", "refuse"} {
		for i := len(fs) - 1; i >= 0; i-- {
			if fs[i].(map[string]any)["level"] != level {
				continue
			}
			fs = append(fs[:i:i], fs[i+1:]...)
			omitted++
			if level != "note" && fits() {
				return b, err
			}
		}
		if fits() {
			return b, err
		}
	}
	return b, err
}
