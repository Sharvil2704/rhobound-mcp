// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

// The CLI twins of the MCP tools (context, budget, preview, explain) and the judge hand-off print canonical JSON
// views. Their exit statuses: 0 with the view (a preview's block or inconclusive is an answer, not an error), 1 for a
// refusal, which --json prints on standard output as {"error": Refusal} (refusal.v1), and 2 for a usage error.
const (
	twinOK      = 0
	twinRefused = 1
	twinUsage   = 2
)

func init() {
	register("context", command{run: cmdContext, usage: `  rhobound context --fit F [--detail summary|full] [--policy P] [--json]
                                            where latency is tight: the context view (JSON)
`})
}

// twinIO is a twin command's output: the view on standard output, a refusal on standard output with --json or on
// standard error without it.
type twinIO struct {
	out, errw io.Writer
	asJSON    bool
}

// view writes a view and a newline.
func (t twinIO) view(b []byte) int {
	if _, err := t.out.Write(append(b, '\n')); err != nil {
		fmt.Fprintln(t.errw, "rhobound:", err)
		return twinRefused
	}
	return twinOK
}

// usage reports a usage error.
func (t twinIO) usage(msg string) int {
	fmt.Fprintln(t.errw, "rhobound:", msg)
	return twinUsage
}

// fail reports an error: a coded refusal as refusal.v1 with --json, anything else as a message.
func (t twinIO) fail(err error) int {
	if r, ok := twinRefusal(err); ok {
		if t.asJSON {
			if werr := r.WriteJSON(t.out); werr != nil {
				fmt.Fprintln(t.errw, "rhobound:", werr)
			}
		}
		fmt.Fprintln(t.errw, "rhobound:", err)
		return twinRefused
	}
	fmt.Fprintln(t.errw, "rhobound:", err)
	return twinRefused
}

// twinRefusal returns the coded refusal an error carries: a refused input of the gate, the budget or the judge check
// (*gate.Error), or any refusal report.RefusalOf knows (the engine's, a change set's, the inference's).
func twinRefusal(err error) (report.Refusal, bool) {
	var ge *gate.Error
	if errors.As(err, &ge) {
		return ge.Refusal(), true
	}
	return report.RefusalOf(err)
}

// twinFit reads a fit record file: the record, and its digest, the sha256 of the file's bytes (what `fit --out`
// wrote, and what the MCP server stored and names in fit_digest). A record of another schema version is refused
// (fit.version).
func twinFit(path string) (*fit.Record, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	rec, err := fit.Unmarshal(b)
	if err != nil {
		if errors.Is(err, fit.ErrVersion) {
			return nil, "", &report.RefusalError{Code: contract.FitVersion, Err: err}
		}
		return nil, "", err
	}
	return rec, contract.Digest(b), nil
}

// twinPolicy reads a policy file; "" gives nil (no policy). A refused policy is a coded *gate.Error.
func twinPolicy(path string) (*gate.Policy, []byte, error) {
	if path == "" {
		return nil, nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := gate.ParsePolicy(b, false)
	return p, b, err
}

// twinNow reads --now (RFC 3339); without it, the clock: the one place the time is read, outside the pure gate.
func twinNow(s string) (time.Time, error) {
	if s == "" {
		return time.Now().UTC(), nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--now must be an RFC 3339 time, as 2026-10-27T12:00:00Z")
	}
	return t.UTC(), nil
}

// twinFlags is a flag set for a twin command, with --json.
func twinFlags(name string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs, fs.Bool("json", false, "print a refusal as JSON on standard output")
}

const contextUsage = "usage: rhobound context --fit F [--detail summary|full] [--policy P] [--json]"

func cmdContext(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("context")
	fitPath := fs.String("fit", "", "the fit record (fit.json)")
	detail := fs.String("detail", views.DetailSummary, "summary or full")
	policyPath := fs.String("policy", "", "a policy, for its max_utilization_validated")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	if err != nil || len(pos) != 0 || *fitPath == "" || (*detail != views.DetailSummary && *detail != views.DetailFull) {
		return t.usage(contextUsage)
	}
	p, _, err := twinPolicy(*policyPath)
	if err != nil {
		return t.fail(err)
	}
	rec, digest, err := twinFit(*fitPath)
	if err != nil {
		return t.fail(err)
	}
	b := &budget.Budgeter{Fit: rec, FitDigest: digest}
	base, err := b.Base()
	if err != nil {
		return t.fail(err)
	}
	// leverage for the services the view can list: the busiest MaxContextServices
	var rows []report.Row
	for _, row := range base.Rows {
		if budget.Budgetable(row) {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Util > rows[j].Util })
	if len(rows) > views.MaxContextServices {
		rows = rows[:views.MaxContextServices]
	}
	svcs := make([]string, len(rows))
	for i, row := range rows {
		svcs[i] = row.Name
	}
	lev, err := b.Leverage(svcs)
	if err != nil {
		return t.fail(err)
	}
	in := views.ContextInput{FitDigest: digest, KeptShare: rec.KeptShare, Base: base, Leverage: lev, Detail: *detail}
	if p != nil {
		in.ValidatedCeiling = p.MaxUtilizationValidated
	}
	v, err := views.Context(in)
	if err != nil {
		return t.fail(err)
	}
	return t.view(v)
}
