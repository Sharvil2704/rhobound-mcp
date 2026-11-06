// SPDX-License-Identifier: Apache-2.0

package gate

// Regression test for the Lean lane GateRules finding GR-1 (maze-kit lean/Rbproofs/Lanes/GateRules.lean), written by the
// workflow's verifier as a reproduction against the real code and kept once fixed.

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// GR-1: chain fixture with b raised (identified workers) and a second measured-ab item that IMPROVES c, whose workers
// are a trusted lower bound. The block rests on b alone; c's predicted improvement is an over-estimate of the
// improvement, so the true entry change is at or above the predicted one.
func gr1Report(cWorkersLower bool, cAfter float64, cOwn float64) *report.V2 {
	r := chain(3.5, 6.0, change.LabelMeasuredAB)
	sign := change.SignDecreasing
	if cOwn > 0 {
		sign = change.SignIncreasing
	}
	r.Items = append(r.Items, report.Applied{Op: change.OpOwnMs, Service: "c", Value: change.Range{Lo: cOwn, Hi: cOwn},
		Label: change.LabelMeasuredAB, Sign: sign})
	r.Labels = append(r.Labels, report.LabelRef{Item: 1, Label: change.LabelMeasuredAB})
	r.Entries = []report.Entry{entry("a", 45.22, 3.5*1.19+cOwn, 6.0*1.19+cOwn)}
	uc := change.Range{Lo: cAfter, Hi: cAfter}
	r.Rows[2].UtilizationAfter = &uc
	r.Rows[2].WorkersLower = cWorkersLower
	return r
}

func TestWFGateRulesGR1(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lower bool
		after float64
		own   float64
	}{
		{"c improved, identified workers", false, 0.195, -0.2},
		{"c improved, lower-bound workers", true, 0.195, -0.2},
		{"c raised negligibly, lower-bound workers (workers_unbounded b's change alone)", true, 0.20001, 0.0001},
	} {
		r := gr1Report(tc.lower, tc.after, tc.own)
		if tc.own > 0 {
			withUnbounded(&r.Entries[0], 3.5*1.19, 6.0*1.19, "c")
		}
		res := Evaluate(Input{Report: r, Policy: policy(t, basePolicy), Now: now})
		r10 := res.RuleRow("10")
		var band string
		if len(res.Entries) > 0 && res.Entries[0].BandMs != nil {
			band = strings.TrimSpace(strings.Join([]string{fmtMs(res.Entries[0].BandMs.Lo), fmtMs(res.Entries[0].BandMs.Hi)}, ".."))
		}
		t.Logf("GR-1 %s: verdict=%s exit=%d rule10=%s/%s value=%v threshold=%v band=%s decide_with=%q",
			tc.name, res.Verdict, res.ExitCode, r10.Rule, r10.Effect, deref(r10.Value), deref(r10.Threshold), band,
			res.DecideWith)
		for _, x := range res.Fired() {
			t.Logf("   fired %s %s %s", x.Number, x.Rule, x.Effect)
		}
	}
	// the documented contract: a block that does not rest on lower-bound workers (c's change is an improvement) stays
	// a block, exit 1
	res := Evaluate(Input{Report: gr1Report(true, 0.195, -0.2), Policy: policy(t, basePolicy), Now: now})
	if res.Verdict != Block || res.ExitCode != ExitBlock {
		t.Errorf("GR-1 CONTRACT BROKEN: c improved with lower-bound workers: verdict %s exit %d rule 10 %s, want block exit 1",
			res.Verdict, res.ExitCode, res.RuleRow("10").Rule)
	}
	// the second facet: c raised negligibly. The engine writes the entry's change with c unbounded, at most the change
	// less c's queueing; with c's queueing all gone (its own +0.0001 ms too) it is b's change alone, still a block
	for _, pol := range []string{basePolicy, `{"schema": "rhobound.policy/v1", "max_entry_mean_change_pct": 5, "block_band": "model"}`} {
		r := gr1Report(true, 0.20001, 0.0001)
		withUnbounded(&r.Entries[0], 3.5*1.19, 6.0*1.19, "c")
		res = Evaluate(Input{Report: r, Policy: policy(t, pol), Now: now})
		if r10 := res.RuleRow("10"); res.Verdict != Block || res.ExitCode != ExitBlock || r10.Rule != RuleMeanChange ||
			r10.Effect != Block || res.Entries[0].Effect != Block || len(res.DecideWith) != 0 {
			t.Errorf("GR-1 c raised negligibly, policy %s: verdict %s exit %d rule 10 %s/%s decide_with %q, want block exit 1",
				pol, res.Verdict, res.ExitCode, r10.Rule, r10.Effect, res.DecideWith)
		}
		// a report without the range (an engine before it) holds the block at inconclusive, as before
		r.Entries[0].Unbounded = nil
		res = Evaluate(Input{Report: r, Policy: policy(t, pol), Now: now})
		if r10 := res.RuleRow("10"); res.Verdict != Inconclusive || r10.Rule != RuleChangeUpperBound ||
			!slices.Equal(res.DecideWith, []string{"workers:c"}) {
			t.Errorf("GR-1 c raised negligibly, no workers_unbounded, policy %s: verdict %s rule 10 %s decide_with %q", pol,
				res.Verdict, r10.Rule, res.DecideWith)
		}
	}
}

// withUnbounded sets an entry's workers_unbounded as the engine writes it, for a change of [lo, hi] ms with the
// services unbounded: widened as entry widens its change range.
func withUnbounded(en *report.Entry, lo, hi float64, services ...string) {
	w := entry(en.Name, en.BeforeMs, lo, hi)
	en.Unbounded = &report.Unbounded{Services: services, ChangeMs: *w.ChangeMs, BandMs: *w.BandMs}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}
