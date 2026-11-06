// SPDX-License-Identifier: Apache-2.0

// Package refit makes an environment's baseline fit from its trace store: it fetches one window of the environment's
// objects (fetch), runs the engine's `check --window --cut entry-start` on them and `fit --manifest --name ENV` on the
// cut, stores the fit's bytes as an artifact and moves the environment's baseline pointer to it (DEPLOY_AWS.md 14,
// which this replaces as a procedure). It never computes a number itself: the engine does, as a subprocess, and the
// fit's digest is the sha256 of the bytes the engine wrote. A refusal at any step is returned as a coded refusal with
// the check's findings, and the baseline pointer is then left as it was.
package refit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/fetch"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// Schema names the result document the refit mode prints.
const Schema = "rhobound-mcp.refit/v1"

// Store is what refit needs of the server's state store.
type Store interface {
	PutArtifact(ctx context.Context, kind string, b []byte) (string, error)
	Baseline(ctx context.Context, env string) (string, error)
	SetBaseline(ctx context.Context, env, fitDigest string) error
}

// Options are one refit.
type Options struct {
	Env    string     // the environment's configured name
	Config config.Env // its trace store, from server configuration
	// Start and End are the window; End is at most Now minus the store's export margin for the fit to read every
	// span of the window (Window computes one).
	Start, End time.Time
	// RequestCount is an independent count of the window's requests (rhobound.request-count/v1), or nil; without one
	// the fit carries trace.sampling_unverified, which the default policy holds (DATA.md 6).
	RequestCount []byte
	Workers      map[string]int // --workers svc=N, when the pool sizes are known
	Runner       runner.Runner
	State        Store
	// Work is the folder the fetched window, the cut and the fit are made under; it is removed at the end unless
	// Keep names a folder to move the cut and the fit to.
	Work string
	Keep string
	// Fetcher is the window fetcher; nil means fetch.Default.
	Fetcher fetch.Fetcher
	// Store is the object store; nil means the one Config names (StoreFor).
	Store objstore.Store
}

// Result is a refit that reached a fit.
type Result struct {
	Schema   string `json:"schema"`
	Env      string `json:"env"`
	Window   Window `json:"window"`
	Fit      string `json:"fit_digest"`
	Previous string `json:"previous_baseline,omitempty"`
	// Manifest is the cut's capture manifest digest, which the fit records as its manifest_digest.
	Manifest string `json:"cut_manifest_sha256,omitempty"`
	Files    int    `json:"files"`
	// Findings are the fetcher's doubts and notes and the cut's check findings (defs.v1.json Finding objects).
	Findings struct {
		Fetch []json.RawMessage `json:"fetch"`
		Check []json.RawMessage `json:"check"`
	} `json:"findings"`
	Verdict string `json:"check_verdict"`
	Kept    string `json:"kept,omitempty"` // the folder the cut and the fit were kept in
}

// Window is a window as RFC 3339 strings, as given to the engine.
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// Refusal is a refit that a step refused: Refusal is the coded refusal (defs.v1.json Refusal: the first refuse-level
// finding of the check, or the engine's own), Findings the check's findings when it ran.
type Refusal struct {
	Step     string            `json:"step"`
	Refusal  json.RawMessage   `json:"refusal"`
	Findings []json.RawMessage `json:"findings,omitempty"`
}

func (r *Refusal) Error() string {
	var c struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(r.Refusal, &c)
	if c.Code == "" {
		return r.Step + ": refused"
	}
	return fmt.Sprintf("%s: %s: %s", r.Step, c.Code, c.Reason)
}

// RefusalJSON is the refusal object, for callers that record coded refusals (lambdart.Job).
func (r *Refusal) RefusalJSON() []byte { return r.Refusal }

// StoreFor opens the object store an environment's configuration names.
func StoreFor(e config.Env) (objstore.Store, error) {
	switch e.Store {
	case "dir":
		return objstore.Dir{Root: e.Root}, nil
	case "s3":
		return nil, errors.New("refit: the s3 store is not in this build; lay the window out as the S3 exporter writes it and use a dir store")
	}
	return nil, fmt.Errorf("refit: store %q is not dir or s3", e.Store)
}

// MarginAfter is how long after a window's end its last spans may still be exported: the environment's configured
// retry, batch and SDK delays, or the fetcher's default (DATA.md 3.9).
func MarginAfter(e config.Env) time.Duration {
	sum := time.Duration((e.RetryS + e.BatchS + e.SDKDelayS) * float64(time.Second))
	if sum <= 0 {
		return fetch.DefaultMarginAfter
	}
	return sum
}

// LastWindow is the window of the last d, ending settle before now, both truncated to the second: the shape a
// scheduled refit asks for. A settle of 0 means the environment's MarginAfter.
func LastWindow(e config.Env, now time.Time, d, settle time.Duration) (time.Time, time.Time) {
	if settle <= 0 {
		settle = MarginAfter(e)
	}
	end := now.Add(-settle).UTC().Truncate(time.Second)
	return end.Add(-d), end
}

const (
	checkAdmitted = 0
	checkRefused  = 4
)

// Run makes the fit and sets the baseline.
func Run(ctx context.Context, opt Options) (*Result, error) {
	if opt.Env == "" || opt.Runner == nil || opt.State == nil || opt.Work == "" {
		return nil, errors.New("refit: an env, a runner, a state store and a work folder are required")
	}
	if !opt.End.After(opt.Start) {
		return nil, errors.New("refit: the window must end after it starts")
	}
	st := opt.Store
	if st == nil {
		var err error
		if st, err = StoreFor(opt.Config); err != nil {
			return nil, err
		}
	}
	f := opt.Fetcher
	if f == nil {
		f = fetch.Default
	}
	if err := os.MkdirAll(opt.Work, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(opt.Work, "refit-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	cfg := fetch.EnvConfig{Store: st, Prefix: opt.Config.Prefix, MaxListedBytes: opt.Config.MaxListedBytes,
		Retry: time.Duration(opt.Config.RetryS * float64(time.Second)), Batch: time.Duration(opt.Config.BatchS * float64(time.Second)),
		SDKDelay: time.Duration(opt.Config.SDKDelayS * float64(time.Second))}
	res := &Result{Schema: Schema, Env: opt.Env}
	res.Window.Start, res.Window.End = opt.Start.UTC().Format(time.RFC3339Nano), opt.End.UTC().Format(time.RFC3339Nano)
	res.Findings.Fetch, res.Findings.Check = []json.RawMessage{}, []json.RawMessage{}

	fetched, err := f.Fetch(ctx, cfg, fetch.Request{Env: opt.Env, Start: opt.Start, End: opt.End}, work, Scanner{})
	if err != nil {
		var rerr *fetch.RefusalError
		if errors.As(err, &rerr) {
			return nil, &Refusal{Step: "fetch", Refusal: rerr.RefusalJSON()}
		}
		return nil, fmt.Errorf("refit: fetch: %w", err)
	}
	res.Findings.Fetch = append(res.Findings.Fetch, fetched.Findings...)

	// check --window --cut: the canonical capture of the window, checked with the count when there is one
	cut := filepath.Join(work, "cut")
	args := []string{"--manifest", fetched.ManifestPath, "--window", res.Window.Start + "," + res.Window.End,
		"--cut", "entry-start", "--out", cut, "--json"}
	var files map[string][]byte
	if opt.RequestCount != nil {
		args = append(args, "--request-count", runner.File("request-count.json"))
		files = map[string][]byte{"request-count.json": opt.RequestCount}
	}
	out, exit, err := opt.Runner.Run(ctx, "check", args, files)
	if err != nil {
		return nil, fmt.Errorf("refit: check: %w", err)
	}
	var rep struct {
		Verdict  string            `json:"verdict"`
		Findings []json.RawMessage `json:"findings"`
		Manifest string            `json:"manifest_sha256"`
	}
	if (exit != checkAdmitted && exit != checkRefused) || json.Unmarshal(out, &rep) != nil {
		return nil, fmt.Errorf("refit: check exited %d without a report: %s", exit, head(out))
	}
	res.Verdict, res.Manifest = rep.Verdict, rep.Manifest
	if rep.Findings != nil {
		res.Findings.Check = rep.Findings
	}
	if exit == checkRefused {
		r, ok := firstRefusal(rep.Findings)
		if !ok {
			return nil, fmt.Errorf("refit: check refused the window without a refuse-level finding: %s", head(out))
		}
		return nil, &Refusal{Step: "check", Refusal: r, Findings: rep.Findings}
	}

	// fit --manifest CUT/manifest.json --name ENV
	fitDir := filepath.Join(work, "fit")
	args = []string{"--manifest", filepath.Join(cut, "manifest.json"), "--name=" + opt.Env, "--out", fitDir, "--json"}
	if w := workersFlag(opt.Workers); w != "" {
		args = append(args, w)
	}
	out, exit, err = opt.Runner.Run(ctx, "fit", args, nil)
	if err != nil {
		return nil, fmt.Errorf("refit: fit: %w", err)
	}
	if exit != 0 {
		var e struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && len(e.Error) > 0 {
			return nil, &Refusal{Step: "fit", Refusal: e.Error, Findings: rep.Findings}
		}
		return nil, fmt.Errorf("refit: fit exited %d: %s", exit, head(out))
	}
	b, err := os.ReadFile(filepath.Join(fitDir, "fit.json"))
	if err != nil {
		return nil, fmt.Errorf("refit: fit wrote no fit.json: %w", err)
	}
	var printed struct {
		Digest string `json:"fit_digest"`
	}
	if json.Unmarshal(out, &printed) != nil || printed.Digest != state.Digest(b) {
		return nil, fmt.Errorf("refit: fit printed %q, fit.json hashes to %s", printed.Digest, state.Digest(b))
	}
	res.Fit = printed.Digest
	ents, _ := os.ReadDir(fetched.Dir)
	res.Files = len(ents) - 1 // the manifest

	if d, err := opt.State.PutArtifact(ctx, "fit", b); err != nil {
		return nil, fmt.Errorf("refit: storing the fit: %w", err)
	} else if d != res.Fit {
		return nil, fmt.Errorf("refit: the store returned %s for a fit whose digest is %s", d, res.Fit)
	}
	if prev, err := opt.State.Baseline(ctx, opt.Env); err == nil {
		res.Previous = prev
	} else if !errors.Is(err, state.ErrNotFound) {
		return nil, fmt.Errorf("refit: reading the baseline: %w", err)
	}
	if err := opt.State.SetBaseline(ctx, opt.Env, res.Fit); err != nil {
		return nil, fmt.Errorf("refit: setting the baseline: %w", err)
	}
	if opt.Keep != "" {
		if err := keep(work, opt.Keep); err != nil {
			return nil, err
		}
		res.Kept = opt.Keep
	}
	return res, nil
}

// keep moves the cut and the fit (not the fetched objects) to dst.
func keep(work, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	for _, sub := range []string{"cut", "fit"} {
		to := filepath.Join(dst, sub)
		if err := os.RemoveAll(to); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(work, sub), to); err != nil {
			return fmt.Errorf("refit: keeping %s: %w", sub, err)
		}
	}
	return nil
}

// firstRefusal is the first refuse-level finding as a Refusal object (code, reason from the finding's fix, fix).
func firstRefusal(findings []json.RawMessage) (json.RawMessage, bool) {
	for _, raw := range findings {
		var f struct {
			Code    string         `json:"code"`
			Level   string         `json:"level"`
			Subject string         `json:"subject"`
			Fix     string         `json:"fix"`
			Numbers map[string]any `json:"numbers"`
		}
		if json.Unmarshal(raw, &f) != nil || f.Level != "refuse" {
			continue
		}
		r := map[string]any{"code": f.Code, "reason": "the window's capture was refused", "fix": f.Fix}
		if f.Subject != "" {
			r["subject"] = f.Subject
		}
		if len(f.Numbers) > 0 {
			r["numbers"] = f.Numbers
		}
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		return b, true
	}
	return nil, false
}

func workersFlag(w map[string]int) string {
	if len(w) == 0 {
		return ""
	}
	var parts []string
	for name, n := range w {
		parts = append(parts, fmt.Sprintf("%s=%d", name, n))
	}
	// a stable order for the record
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return "--workers=" + strings.Join(parts, ",")
}

func head(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}
