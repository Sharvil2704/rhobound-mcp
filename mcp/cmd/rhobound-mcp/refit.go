// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/refit"
)

func init() { register("refit", runRefit) }

// refitUsage is the mode's usage line.
const refitUsage = "usage: rhobound-mcp refit --config FILE --env NAME (--window START,END | --last D [--settle D]) " +
	"[--request-count FILE] [--workers svc=N,...] [--keep DIR] [--json]"

// runRefit is `rhobound-mcp refit`: an environment's baseline fit from its trace store, for a schedule (hourly, and
// after each rollout settles: ARCHITECTURE.md 4.1) or by hand. It fetches the window, runs the engine's check (with
// the cut) and fit, stores the fit under state_dir/server and moves the environment's baseline to it, so that
// context, budget and predict with env answer from it. The window is --window START,END, or --last D: the last D
// ending --settle before now (by default the environment's export margin: its retry, batch and SDK delays, or
// 7 min), so that every span of the window has been exported. Exit 0 with the result as JSON (--json) or a line;
// 4 when a step refused the window (the coded refusal and the check's findings are printed as JSON); 1 on any other
// failure; 2 for a usage error. The baseline is moved only on exit 0.
func runRefit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("refit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "the server configuration")
	env := fs.String("env", "", "the environment's configured name")
	window := fs.String("window", "", "START,END (RFC 3339)")
	last := fs.Duration("last", 0, "the window is the last D, ending --settle before now")
	settle := fs.Duration("settle", 0, "how long before now the --last window ends (default: the environment's export margin)")
	countPath := fs.String("request-count", "", "an independent request count of the window (rhobound.request-count/v1)")
	workers := fs.String("workers", "", "workers per pod, as svc=N,svc=N")
	keep := fs.String("keep", "", "keep the cut and the fit in this folder")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	if err := fs.Parse(args); err != nil {
		return exitStatus(2)
	}
	if fs.NArg() != 0 || *configPath == "" || *env == "" || (*window == "") == (*last == 0) {
		fmt.Fprintln(os.Stderr, refitUsage)
		return exitStatus(2)
	}
	d, err := setup(*configPath)
	if err != nil {
		return err
	}
	e, err := d.cfg.Env(*env)
	if err != nil {
		return fmt.Errorf("--env %q is not a configured environment (%s)", *env, strings.Join(envNames(d), ", "))
	}
	opt := refit.Options{Env: *env, Config: e, Runner: d.engine, State: d.store, Work: d.work}
	if *window != "" {
		s, en, ok := strings.Cut(*window, ",")
		var err1, err2 error
		opt.Start, err1 = time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
		opt.End, err2 = time.Parse(time.RFC3339Nano, strings.TrimSpace(en))
		if !ok || err1 != nil || err2 != nil || !opt.End.After(opt.Start) {
			fmt.Fprintf(os.Stderr, "rhobound-mcp: --window %q is not START,END in RFC 3339 with END after START\n", *window)
			return exitStatus(2)
		}
	} else {
		if *last <= 0 {
			fmt.Fprintln(os.Stderr, refitUsage)
			return exitStatus(2)
		}
		opt.Start, opt.End = refit.LastWindow(e, time.Now(), *last, *settle)
	}
	if *countPath != "" {
		b, err := readBounded(*countPath, configMax, true)
		if err != nil {
			return fmt.Errorf("--request-count: %w", err)
		}
		opt.RequestCount = b
	}
	if *workers != "" {
		opt.Workers = map[string]int{}
		for _, kv := range strings.Split(*workers, ",") {
			name, v, ok := strings.Cut(kv, "=")
			n, perr := strconv.Atoi(v)
			if !ok || name == "" || perr != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "rhobound-mcp: --workers %q is not svc=N,svc=N\n", *workers)
				return exitStatus(2)
			}
			opt.Workers[name] = n
		}
	}
	if *keep != "" {
		if opt.Keep, err = filepath.Abs(*keep); err != nil {
			return err
		}
	}
	res, err := refit.Run(ctx, opt)
	var refusal *refit.Refusal
	if errors.As(err, &refusal) {
		b, _ := json.MarshalIndent(refusal, "", "  ")
		fmt.Fprintln(os.Stdout, string(b))
		fmt.Fprintf(os.Stderr, "rhobound-mcp: refit %s: %v (the baseline of %s is unchanged)\n", *env, refusal, *env)
		return exitStatus(4)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Fprintln(os.Stdout, string(b))
		return nil
	}
	fmt.Fprintf(os.Stdout, "refit %s: baseline %s (window %s .. %s, check %s, %d objects", res.Env, res.Fit,
		res.Window.Start, res.Window.End, res.Verdict, res.Files)
	if res.Previous != "" {
		fmt.Fprintf(os.Stdout, "; was %s", res.Previous)
	}
	fmt.Fprintln(os.Stdout, ")")
	for _, raw := range append(append([]json.RawMessage{}, res.Findings.Fetch...), res.Findings.Check...) {
		var f struct {
			Code, Level, Fix string
		}
		if json.Unmarshal(raw, &f) == nil && f.Code != "" {
			fmt.Fprintf(os.Stdout, "  %-6s %s\n         fix: %s\n", f.Level, f.Code, f.Fix)
		}
	}
	return nil
}

func envNames(d *deps) []string {
	envs, _ := d.cfg.Names()
	return envs
}
