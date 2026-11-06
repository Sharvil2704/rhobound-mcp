// SPDX-License-Identifier: Apache-2.0

package main

// predict --fit F --load-scale S (SPEC3 F4): a what-if load point. The system before the change and after it are both
// solved at S times the fit's external rate, so an entry's change_ms is the change's effect at that load and
// load_level_ms the load's effect alone; load.evaluated_at is what_if, and the gate reading the prediction applies
// rule 7. S = 1, the default, is today's report byte for byte.

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// predictV2 runs predict --fit --json and reads report v2 strictly.
func predictV2(t *testing.T, args ...string) *report.V2 {
	t.Helper()
	out, errw, code := exec(t, append(append([]string{"predict"}, args...), "--json")...)
	if code != 0 {
		t.Fatalf("predict %v: exit %d: %s", args, code, errw)
	}
	var v report.V2
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("predict %v: %v", args, err)
	}
	return &v
}

func entryOf(t *testing.T, v *report.V2, name string) report.Entry {
	t.Helper()
	for _, e := range v.Entries {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no entry %s", name)
	return report.Entry{}
}

func TestPredictLoadScale(t *testing.T) {
	inRepoRoot(t)
	f := fitOf(t, "testdata/capture-chain", "--workers", "a=1,b=2,c=1")

	// --load-scale 1 is today's report, byte for byte, in every form
	for _, form := range [][]string{{"--json"}, nil, {"--report", "v1", "--json"}, {"--report", "v1"}} {
		for _, patch := range [][]string{nil, {"--patch", "b=+1ms"}} {
			args := append(append([]string{"predict", "--fit", f}, patch...), form...)
			want, werr, wcode := exec(t, args...)
			got, gerr, gcode := exec(t, append(args, "--load-scale", "1")...)
			if wcode != 0 || gcode != 0 || got != want {
				t.Errorf("%v --load-scale 1: exit %d (%s) and %d (%s), differs at %s", args[1:], wcode, werr, gcode, gerr,
					firstDiff([]byte(got), []byte(want)))
			}
		}
	}

	one := predictV2(t, "--fit", f, "--patch", "b=+1ms")
	what := predictV2(t, "--fit", f, "--patch", "b=+1ms", "--load-scale", "1.3")
	if one.Load.EvaluatedAt != report.EvaluatedAtFitWindow || what.Load.EvaluatedAt != report.EvaluatedAtWhatIf ||
		what.Load.LoadScale != 1.3 || !(what.Load.MaxUtilizationBefore > one.Load.MaxUtilizationBefore) {
		t.Fatalf("load: %+v at 1, %+v at 1.3", one.Load, what.Load)
	}
	e1, e := entryOf(t, one, "a"), entryOf(t, what, "a")
	if e1.BeforeAtFitLoadMs != nil || e1.LoadLevelMs != nil || e.BeforeAtFitLoadMs == nil || e.LoadLevelMs == nil {
		t.Fatalf("the level is set at 1 or missing at 1.3: %+v, %+v", e1, e)
	}
	// the level is the base at 1.3 less the base at the fit's load; the change is after minus before, both at 1.3, and
	// larger than at the fit's load (the queues are longer)
	if math.Abs(*e.BeforeAtFitLoadMs-e1.BeforeMs) > 1e-9 || math.Abs(*e.LoadLevelMs-(e.BeforeMs-e1.BeforeMs)) > 1e-9 ||
		!(*e.LoadLevelMs > 0) || math.Abs(e.ChangeMs.Lo-(e.AfterMs.Lo-e.BeforeMs)) > 1e-9 || !(e.ChangeMs.Lo > e1.ChangeMs.Lo) {
		t.Errorf("a at 1.3: %+v (at 1: %+v)", e, e1)
	}
	// a load_scale item on the entry at the fit's load says level and effect at once (the item scales the system after
	// the change only); --load-scale says them apart, and they add up to it
	mixed := predictV2(t, "--fit", f, "--changes", changeFile(t, f,
		change.Item{Op: change.OpLoadScale, Service: "a", Value: change.Range{Lo: 1.3, Hi: 1.3}, Label: change.LabelDeclared},
		change.Item{Op: change.OpOwnMs, Service: "b", Value: change.Range{Lo: 1, Hi: 1}, Label: change.LabelDeclared}))
	if m := entryOf(t, mixed, "a"); mixed.Load.EvaluatedAt != report.EvaluatedAtFitWindow ||
		math.Abs(m.ChangeMs.Lo-(*e.LoadLevelMs+e.ChangeMs.Lo)) > 1e-9 {
		t.Errorf("a load_scale item: change %+v; level %v + change %v at --load-scale 1.3", m.ChangeMs, *e.LoadLevelMs, e.ChangeMs.Lo)
	}
	// the rows are at the load point; their error_pct stays the fit's misfit check, the model at the fit's load
	for i, row := range what.Rows {
		r1 := one.Rows[i]
		if row.Name != r1.Name || (row.ErrorPct == nil) != (r1.ErrorPct == nil) ||
			(row.ErrorPct != nil && math.Abs(*row.ErrorPct-*r1.ErrorPct) > 1e-9) ||
			math.Abs(row.RateRPS-1.3*r1.RateRPS) > 1e-9*math.Max(1, r1.RateRPS) || row.ObservedMs != r1.ObservedMs {
			t.Errorf("row %s at 1.3: %+v, at 1: %+v", row.Name, row, r1)
		}
	}
	// the text report says it is a what-if, and the level apart from the change
	text, _, code := exec(t, "predict", "--fit", f, "--patch", "b=+1ms", "--load-scale", "1.3")
	if code != 0 || !strings.Contains(text, "\nwhat-if: 1.3 times the fit's external rate.") ||
		!strings.Contains(text, "ms at fit load  load level ms") || !strings.Contains(text, "\nload level: ") {
		t.Errorf("text at 1.3:\n%s", text)
	}

	// the utilization limit applies at the load point: c is at 25.8% at the fit's load, 90% at 3.5 times it
	out, errw, code := exec(t, "predict", "--fit", f, "--patch", "b=+1ms", "--load-scale", "3.5", "--json")
	var doc report.RefusalDoc
	if code != 1 || json.Unmarshal([]byte(out), &doc) != nil || doc.Error.Code != contract.ModelUtilizationObserved ||
		!strings.HasPrefix(errw, "rhobound: refusing to predict: at 3.5 times the observed load") ||
		doc.Error.Numbers[report.LoadScaleNumber] != 3.5 {
		t.Errorf("--load-scale 3.5: exit %d, %s %s", code, out, errw)
	}

	// usage: a positive finite number, with --fit only, and not with report v1, which has no load point
	for _, bad := range [][]string{
		{"predict", "--fit", f, "--load-scale", "0"},
		{"predict", "--fit", f, "--load-scale", "-1.3"},
		{"predict", "--fit", f, "--load-scale", "NaN"},
		{"predict", "--fit", f, "--load-scale", "+Inf"},
		{"predict", "--fit", f, "--load-scale", "x"},
		{"predict", "--fit", f, "--load-scale="},
		{"predict", "--fit", f, "--load-scale", "1.3", "--report", "v1"},
		{"predict", "testdata/capture-chain", "--load-scale", "1.3"},
		{"predict", "testdata/capture-chain", "--load-scale", "1"},
	} {
		if out, errw, code := exec(t, append(bad, "--json")...); code != 1 || out != "" || errw == "" {
			t.Errorf("%v: exit %d, %q %q", bad[1:], code, out, errw)
		}
	}
}

// The gate on a what-if prediction applies rule 7 as designed (ARCHITECTURE.md 6.3): above the fit's load the load is
// extrapolated (extrapolated_load, a note: the verdict stays the other rules'), gate.json says evaluated_at what_if
// and the load point (load_scale, at every scale, below 1 too), and rule 4 does not take the load point for a misfit.
// A refusal at the load point is rule 2, of a what-if at that load point, and rule 7 is evaluated on it.
func TestGateOnWhatIf(t *testing.T) {
	inRepoRoot(t)
	pol := writeTemp(t, t.TempDir(), "default.json", []byte(gatePolicy))
	dir, _ := abArmDir(t, "loop", nil)
	f := fitOf(t, dir, "--workers", "a=1,b=2,c=1")
	now := nowOf(t, f)
	type rec struct {
		Verdict     string      `json:"verdict"`
		EvaluatedAt string      `json:"evaluated_at"`
		LoadScale   *float64    `json:"load_scale"`
		Rules       []gate.Rule `json:"rules"`
		summary     string
	}
	gateAt := func(scale string) (rec, int) {
		t.Helper()
		pdir := t.TempDir()
		out, errw, code := exec(t, "predict", "--fit", f, "--patch", "b=+1ms", "--load-scale", scale, "--json")
		if code != 0 && code != 1 {
			t.Fatalf("predict at %s: exit %d %s", scale, code, errw)
		}
		pred := writeTemp(t, pdir, "prediction.json", []byte(out))
		js, errw, code := exec(t, "gate", pred, "--policy", pol, "--now", now, "--out", filepath.Join(pdir, "gate"), "--json")
		var r rec
		if err := json.Unmarshal([]byte(js), &r); err != nil {
			t.Fatalf("gate at %s: exit %d %v %s", scale, code, err, errw)
		}
		b, _ := os.ReadFile(filepath.Join(pdir, "gate", gate.RecordFile))
		if string(b) != js {
			t.Errorf("gate at %s: gate.json is not what --json printed", scale)
		}
		sum, _ := os.ReadFile(filepath.Join(pdir, "gate", gate.SummaryFile))
		r.summary = string(sum)
		return r, code
	}
	// the record of a what-if says its load point, in gate.json and in summary.md
	saysLoad := func(r rec, scale string) bool {
		want, _ := strconv.ParseFloat(scale, 64)
		return r.EvaluatedAt == report.EvaluatedAtWhatIf && r.LoadScale != nil && *r.LoadScale == want &&
			strings.Contains(r.summary, "Evaluated at a what-if load: "+scale+" times the fit's external rate")
	}
	row := func(r rec, number string) gate.Rule {
		for _, x := range r.Rules {
			if x.Number == number {
				return x
			}
		}
		t.Fatalf("no rule %s", number)
		return gate.Rule{}
	}
	at1, code1 := gateAt("1")
	if r7 := row(at1, "7"); at1.EvaluatedAt != report.EvaluatedAtFitWindow || r7.Rule != gate.RuleExtrapolatedLoad ||
		r7.Effect != gate.Pass || at1.LoadScale != nil || strings.Contains(at1.summary, "what-if") {
		t.Errorf("at 1: evaluated at %s, load scale %v, rule 7 %+v", at1.EvaluatedAt, at1.LoadScale, r7)
	}
	for _, c := range []struct {
		scale  string
		effect string
	}{{"1.3", gate.EffectNote}, {"2", gate.EffectNote}, {"0.8", gate.Pass}} {
		r, code := gateAt(c.scale)
		r7, r4 := row(r, "7"), row(r, "4")
		if !saysLoad(r, c.scale) || r7.Rule != gate.RuleExtrapolatedLoad || r7.Effect != c.effect ||
			r4.Rule != gate.RuleModelMisfit || r4.Effect != gate.Pass {
			t.Errorf("at %s: evaluated at %s, load scale %v, rule 7 %+v, rule 4 %+v", c.scale, r.EvaluatedAt, r.LoadScale, r7, r4)
		}
		if want, _ := strconv.ParseFloat(c.scale, 64); c.effect == gate.EffectNote && (r7.Value == nil || *r7.Value != want) {
			t.Errorf("at %s: rule 7's value %v", c.scale, r7.Value)
		}
		// the declared change holds the verdict at the fit's load and at the load point alike (rule 11; rule 10 may
		// block at a higher load); rule 7 informs and never decides
		if r.Verdict == gate.Pass || (code == gate.ExitPass) || (code1 == gate.ExitPass) {
			t.Errorf("at %s: verdict %s (exit %d)", c.scale, r.Verdict, code)
		}
	}
	// a load point above the limit is refused by predict, and the gate reads the refusal as rule 2, at that load point:
	// a what-if, extrapolated (rule 7), which the record says
	r, code := gateAt("3.5")
	if r2 := row(r, "2"); r.Verdict != gate.Inconclusive || code != gate.ExitInconclusive || r2.Rule != gate.RuleBaseOutOfScope ||
		r2.Code != contract.ModelUtilizationObserved {
		t.Errorf("at 3.5: %s (exit %d), rule 2 %+v", r.Verdict, code, r2)
	}
	if r7 := row(r, "7"); !saysLoad(r, "3.5") || !r7.Evaluated || r7.Rule != gate.RuleExtrapolatedLoad ||
		r7.Effect != gate.EffectNote || r7.Value == nil || *r7.Value != 3.5 {
		t.Errorf("at 3.5: evaluated at %s, load scale %v, rule 7 %+v", r.EvaluatedAt, r.LoadScale, r7)
	}
}

// judge-packet --fit F and explain --fit F on a what-if (SPEC3 F4 review): the budgets and the leverage are at the
// report's load point, as its before_ms, change_ms and thresholds are, not at the fit's load. A +1 ms change of b
// moves a by b's leverage on a at that load point, and the packet says so; its why says the load point above 1.
func TestJudgePacketWhatIf(t *testing.T) {
	inRepoRoot(t)
	pol := writeTemp(t, t.TempDir(), "default.json", []byte(gatePolicy))
	dir, _ := abArmDir(t, "loop", nil)
	f := fitOf(t, dir, "--workers", "a=1,b=2,c=1")
	now := nowOf(t, f)
	type packet struct {
		Prediction struct {
			Entries []struct {
				Name     string       `json:"name"`
				ChangeMs change.Range `json:"change_ms"`
			} `json:"entries"`
			Why []string `json:"why"`
		} `json:"prediction"`
		Budgets []struct {
			Service   string   `json:"service"`
			Kind      string   `json:"kind"`
			MaxValue  *float64 `json:"max_value"`
			SafeValue *float64 `json:"safe_value"`
		} `json:"budgets"`
		Leverage []struct {
			Service string  `json:"service"`
			Entry   string  `json:"entry"`
			MsPerMs float64 `json:"ms_per_ms"`
		} `json:"leverage"`
	}
	type at struct {
		change, leverage, max, safe float64
		why                         []string
	}
	packetAt := func(scale string) at {
		t.Helper()
		pdir := t.TempDir()
		out, errw, code := exec(t, "predict", "--fit", f, "--patch", "b=+1ms", "--load-scale", scale, "--json")
		if code != 0 {
			t.Fatalf("predict at %s: exit %d %s", scale, code, errw)
		}
		pred := writeTemp(t, pdir, "prediction.json", []byte(out))
		pk, errw, code := exec(t, "judge-packet", pred, "--policy", pol, "--policy-name", "default", "--fit", f, "--now", now,
			"--json")
		var p packet
		if err := json.Unmarshal([]byte(pk), &p); code != 0 || err != nil || len(p.Prediction.Entries) != 1 ||
			len(p.Budgets) != 1 || len(p.Leverage) != 1 {
			t.Fatalf("judge-packet at %s: exit %d %v %s %s", scale, code, err, errw, pk)
		}
		// explain --fit builds the same packet
		ex, errw, code := exec(t, "explain", "--target", pred, "--section=judge_packet", "--max-chars=8000", "--policy", pol,
			"--policy-name", "default", "--fit", f, "--now", now, "--json")
		if code != 0 || !strings.Contains(ex, strings.TrimSpace(pk)) {
			t.Errorf("explain at %s: exit %d %s: not the judge packet", scale, code, errw)
		}
		b, l := p.Budgets[0], p.Leverage[0]
		if p.Prediction.Entries[0].Name != "a" || b.Service != "b" || b.Kind != "own_ms" || b.MaxValue == nil ||
			b.SafeValue == nil || l.Service != "b" || l.Entry != "a" {
			t.Fatalf("judge-packet at %s: %s", scale, pk)
		}
		return at{change: p.Prediction.Entries[0].ChangeMs.Lo, leverage: l.MsPerMs, max: *b.MaxValue, safe: *b.SafeValue,
			why: p.Prediction.Why}
	}
	one, low, high := packetAt("1"), packetAt("0.7"), packetAt("1.3")
	for _, c := range []struct {
		scale string
		at
	}{{"1", one}, {"0.7", low}, {"1.3", high}} {
		// the +1 ms change is the leverage's own finite difference, so at the same load point they are the same number
		if c.leverage != c.change {
			t.Errorf("at %s: b's leverage on a is %v ms/ms, while +1 ms of b moves a by %v ms", c.scale, c.leverage, c.change)
		}
	}
	// the queues are longer at 1.3 and shorter at 0.7: more leverage and less budget above, the reverse below
	if !(low.leverage < one.leverage && one.leverage < high.leverage) || !(high.max < one.max && one.max < low.max) ||
		!(high.safe < one.safe && one.safe < low.safe) {
		t.Errorf("budgets and leverage do not follow the load point: 0.7 %+v, 1 %+v, 1.3 %+v", low, one, high)
	}
	if !slices.Contains(high.why, "extrapolated_load: a what-if at 1.3 times the fit's load; the verdict is advisory") ||
		slices.ContainsFunc(append(one.why, low.why...), func(w string) bool { return strings.HasPrefix(w, gate.RuleExtrapolatedLoad) }) {
		t.Errorf("why: 0.7 %q, 1 %q, 1.3 %q", low.why, one.why, high.why)
	}
}
