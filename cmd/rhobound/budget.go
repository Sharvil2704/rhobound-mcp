// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

func init() {
	register("budget", command{run: cmdBudget, usage: `  rhobound budget  --fit F [--policy P] [--service S] [--kinds own_ms,load_scale] [--json]
                                            how much a service may add per visit: max and safe (JSON)
`})
}

const budgetUsage = "usage: rhobound budget --fit F [--policy P] [--service S] [--kinds own_ms,load_scale] [--json]"

func cmdBudget(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("budget")
	fitPath := fs.String("fit", "", "the fit record (fit.json)")
	policyPath := fs.String("policy", "", "the policy: its thresholds and model band (without one, only the utilization limits bind)")
	service := fs.String("service", "", "one service (default: every service, the highest leverage first)")
	kinds := fs.String("kinds", "", "own_ms and/or load_scale (default own_ms); extra_calls is refused until structural operations ship")
	callee := fs.String("callee", "", "the callee of extra_calls")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	if err != nil || len(pos) != 0 || *fitPath == "" {
		return t.usage(budgetUsage)
	}
	ks := splitList(*kinds)
	if *callee != "" {
		// a callee only means something to extra_calls, which this version cannot apply
		return t.fail(&gate.Error{Finding: contract.NewFinding(contract.ChangeUnsupportedOp).WithSubject(budget.KindExtraCalls),
			Msg: "budget: --callee is for extra_calls, which needs the structural patch this version does not have"})
	}
	p, _, err := twinPolicy(*policyPath)
	if err != nil {
		return t.fail(err)
	}
	rec, digest, err := twinFit(*fitPath)
	if err != nil {
		return t.fail(err)
	}
	b := &budget.Budgeter{Fit: rec, FitDigest: digest, Policy: p}
	items, truncated, err := b.Compute(strings.TrimSpace(*service), ks)
	if err != nil {
		return t.fail(err)
	}
	names := make([]string, len(rec.Services))
	for i, s := range rec.Services {
		names[i] = s.Name
	}
	v, err := views.Budget(views.BudgetInput{FitDigest: digest, Names: names, Items: items, Truncated: truncated, Policy: p})
	if err != nil {
		return t.fail(err)
	}
	return t.view(v)
}
