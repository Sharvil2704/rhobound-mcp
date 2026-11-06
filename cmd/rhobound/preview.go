// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

func init() {
	register("preview", command{run: cmdPreview, usage: `  rhobound preview --fit F --changes C --policy P [--now T] [--detail summary|full] [--json]
                                            the agent's preview: prediction and advisory verdict (JSON)
`})
}

const previewUsage = "usage: rhobound preview --fit F --changes C --policy P [--now T] [--detail summary|full] [--json]"

// previewInputs are what preview, judge-packet and explain predict from: a fit, a validated and resolved change set.
type previewInputs struct {
	fit      *fit.Record
	digest   string
	report   *report.V2
	services []string // the services the change items name, in order, without repeats
	// loadScale is the load point the budgets are computed at (budget.Budgeter.LoadScale): 0, the fit's window, but
	// for a what-if report read by judge-packet and explain, its load.load_scale
	loadScale float64
}

// twinPredict reads a change set, checks it against the fit and the record store, and predicts it, as
// `predict --fit F --changes C --json` does: the same refusals, in the same order, and the same report v2.
func twinPredict(rec *fit.Record, digest string, changesPath string) (*previewInputs, error) {
	b, err := os.ReadFile(changesPath)
	if err != nil {
		return nil, err
	}
	set, err := change.Parse(b)
	if err != nil {
		return nil, err
	}
	if set.Fit == "" {
		set.Fit = digest
	}
	if set.Fit != digest {
		return nil, fmt.Errorf("%s was written against the fit %s, not %s", changesPath, set.Fit, digest)
	}
	recs := openRecords()
	if err := set.Refusal(rec, recs); err != nil {
		return nil, err
	}
	var doubts []contract.Finding
	for _, f := range set.Validate(rec, recs) {
		if f.Level != contract.Refuse {
			doubts = append(doubts, f)
		}
	}
	hash, err := set.Digest()
	if err != nil {
		return nil, err
	}
	resolved, err := set.Resolve(rec, recs)
	if err != nil {
		return nil, err
	}
	r, err := engine.Predict(engine.Request{Fit: rec, Changes: resolved, ChangeHash: hash, Findings: doubts,
		Plain: rec.Options.Plain})
	if err != nil {
		return nil, err
	}
	in := &previewInputs{fit: rec, digest: digest, report: r}
	seen := map[string]bool{}
	for _, it := range set.Items {
		if it.Op != change.OpLoadScale && !seen[it.Service] {
			seen[it.Service] = true
			in.services = append(in.services, it.Service)
		}
	}
	return in, nil
}

// maxDecideBudgets is the number of changed services whose safe budget a preview computes for decide_with.
const maxDecideBudgets = 4

// safeBudgets returns the safe own-time budget of each changed service the change set names (at most
// maxDecideBudgets), which decide_with quotes, and the budget items.
func (in *previewInputs) safeBudgets(p *gate.Policy) (map[string]float64, []budget.Item, error) {
	b := &budget.Budgeter{Fit: in.fit, FitDigest: in.digest, Policy: p, LoadScale: in.loadScale}
	out := map[string]float64{}
	var items []budget.Item
	for i, svc := range in.services {
		if i == maxDecideBudgets {
			break
		}
		its, _, err := b.Compute(svc, []string{budget.KindOwnMs})
		if err != nil {
			if _, coded := twinRefusal(err); coded {
				continue // a service the budget cannot take (a peer, say): no figure to quote
			}
			return nil, nil, err
		}
		for _, it := range its {
			items = append(items, it)
			if it.Safe != nil {
				out[it.Service] = *it.Safe
			}
		}
	}
	return out, items, nil
}

func cmdPreview(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("preview")
	fitPath := fs.String("fit", "", "the fit record (fit.json)")
	changes := fs.String("changes", "", "the change set (rhobound.change/v1)")
	policyPath := fs.String("policy", "", "the named policy")
	nowFlag := fs.String("now", "", "the time the fit's age is measured at (RFC 3339; default: the clock)")
	detail := fs.String("detail", views.DetailSummary, "summary or full")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	if err != nil || len(pos) != 0 || *fitPath == "" || *changes == "" || *policyPath == "" ||
		(*detail != views.DetailSummary && *detail != views.DetailFull) {
		return t.usage(previewUsage)
	}
	now, err := twinNow(*nowFlag)
	if err != nil {
		return t.usage(err.Error())
	}
	p, _, err := twinPolicy(*policyPath)
	if err != nil {
		return t.fail(err)
	}
	rec, digest, err := twinFit(*fitPath)
	if err != nil {
		return t.fail(err)
	}
	in, err := twinPredict(rec, digest, *changes)
	if err != nil {
		return t.fail(err)
	}
	safe, _, err := in.safeBudgets(p)
	if err != nil {
		return t.fail(err)
	}
	res := gate.Evaluate(gate.Input{Report: in.report, Policy: p, Now: now, Preview: true, SafeBudgets: safe})
	v, err := views.Prediction(in.report, res, *detail)
	if err != nil {
		return t.fail(err)
	}
	return t.view(v)
}
