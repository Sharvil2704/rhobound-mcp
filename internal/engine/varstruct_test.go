// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// pkWait is the Pollaczek-Khinchine mean wait of an M/G/1 queue, ms: rate per ms, mean s and variance v of the
// service time.
func pkWait(lam, s, v float64) float64 {
	return lam * (v + s*s) / (2 * (1 - lam*s))
}

// fraud is maze's fraud (one worker, s 18.48 ms, cs2 1.865, 14.86 requests/s) behind an entry of next to no work.
func fraud() *model.Topology {
	return &model.Topology{Name: "fraud", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "checkout", S: 0.001, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "fraud", S: 18.4788, CS2: 1.8653, Servers: 1},
	}}
}

// A variance change moves an M/G/1 queue by its second moment (Pollaczek-Khinchine): fraud's deep check, every 20th
// visit +61.6 ms, adds 3.08 ms to the mean and 188 ms^2 to the variance; the entry then moves by 7.17 ms, where the
// same mean as a shift (the variance kept) moves it by 5.10.
func TestVariancePatchIsPollaczekKhinchine(t *testing.T) {
	// the production fit's numbers (prediction.json services[fraud]) and the mixture's: D = 61.6 on 1 visit in 20
	const lam, s, cs2, ds, dvar = 0.014861, 18.4788, 1.8653, 3.081, 188.5
	r := handFit(t, fraud(), lam)
	v0 := cs2 * s * s
	want := ds + pkWait(lam, s+ds, v0+dvar) - pkWait(lam, s, v0)
	wantShift := ds + pkWait(lam, s+ds, v0) - pkWait(lam, s, v0)
	if math.Abs(want-7.17) > 0.01 || math.Abs(wantShift-5.10) > 0.01 {
		t.Fatalf("the closed form: %.3f and %.3f", want, wantShift)
	}
	for _, c := range []struct {
		items []change.Item
		want  float64
	}{
		{[]change.Item{item(change.OpOwnMs, "fraud", ds, ds)}, wantShift},
		{[]change.Item{item(change.OpOwnMs, "fraud", ds, ds), item(change.OpOwnVar, "fraud", dvar, dvar)}, want},
		// a variance change alone, and one that would take the variance below 0 (held at 0: a constant time)
		{[]change.Item{item(change.OpOwnVar, "fraud", dvar, dvar)}, pkWait(lam, s, v0+dvar) - pkWait(lam, s, v0)},
		{[]change.Item{item(change.OpOwnVar, "fraud", -1e4, -1e4)}, pkWait(lam, s, 0) - pkWait(lam, s, v0)},
	} {
		v, err := Predict(Request{Fit: r, Changes: set(c.items...), Plain: true})
		if err != nil {
			t.Fatal(err)
		}
		if got := entryChange(t, v, "checkout"); math.Abs(got.Lo-c.want) > 1e-9 || got.Lo != got.Hi {
			t.Errorf("%+v: %+v, want %.4f", c.items, got, c.want)
		}
	}
	// ranged: the corners take own_ms and own_var_ms2 together (both increasing)
	v, err := Predict(Request{Fit: r, Plain: true, Changes: set(item(change.OpOwnMs, "fraud", 2, 4),
		item(change.OpOwnVar, "fraud", 100, 300))})
	if err != nil {
		t.Fatal(err)
	}
	lo := 2 + pkWait(lam, s+2, v0+100) - pkWait(lam, s, v0)
	hi := 4 + pkWait(lam, s+4, v0+300) - pkWait(lam, s, v0)
	if got := entryChange(t, v, "checkout"); math.Abs(got.Lo-lo) > 1e-9 || math.Abs(got.Hi-hi) > 1e-9 {
		t.Errorf("ranged: %+v, want %.4f..%.4f", got, lo, hi)
	}
	// model.PatchedCS2 keeps the shift's arithmetic when the variance does not change
	if got := model.PatchedCS2(cs2, s, ds, true, 0); got != cs2*s*s/((s+ds)*(s+ds)) {
		t.Errorf("PatchedCS2 shift: %v", got)
	}
	if model.PatchedCS2(cs2, s, ds, false, 0) != cs2 || model.PatchedCS2(cs2, s, 0, true, 0) != cs2 {
		t.Error("PatchedCS2 changes a cs2 it should keep")
	}
}

// recsFit is maze's recs (calls profile on 61% of its visits, features always) under the entry web, and media reached
// by web through a stage node and by recs directly.
func recsFit() *model.Topology {
	return &model.Topology{Name: "recs", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "web", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}, {Child: 4, P: 1}}},
		{Name: "recs", S: 8.6, CS2: 0.87, Servers: 3, Calls: []model.Call{{Child: 2, P: 0.606}, {Child: 3, P: 1},
			{Child: 5, P: 1}}},
		{Name: "profile", S: 5.6, CS2: 0.8, Servers: 3},
		{Name: "features", S: 3.6, CS2: 0.7, Servers: 3},
		{Name: "stage:web:1.1", Delay: true, Parallel: true, Calls: []model.Call{{Child: 5, P: 1}, {Child: 5, P: 1}}},
		{Name: "media", S: 4, CS2: 0.5, Servers: 4},
	}}
}

// recsRecord is an A/B record of recs that caches profile: calls per visit 0.591 -> 0.207, the own time a little
// lower, its variance a little higher.
func recsRecord() *abdiff.Record {
	return &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: true, Findings: []contract.Finding{
		contract.NewFinding(contract.ProfileStructureChanged).WithSubject("recs")},
		Services: []abdiff.Service{{Service: "recs",
			OwnMs:  abdiff.Delta{Baseline: 8.64, Candidate: 8.59, Delta: -0.05, SE: 0.13},
			OwnVar: abdiff.Delta{Baseline: 66.5, Candidate: 62.5, Delta: -4, SE: 3},
			Calls: []abdiff.Calls{{Callee: "features", Baseline: 1, Candidate: 1},
				{Callee: "media", Baseline: 1, Candidate: 1.5, BaselineSE: 0, CandidateSE: 0.01},
				{Callee: "profile", Baseline: 0.591, Candidate: 0.207, BaselineSE: 0.0057, CandidateSE: 0.0047}},
			CallsChanged: true}}}
}

// A measured own_ms item brings what its record implies: the variance difference (own_var_ms2, +-2 standard errors)
// and, for a callee whose calls per visit changed and that the fit reaches by one direct call, the call's new
// probability (set_p: the fit's p times the ratio of calls per visit, +-2 standard errors). The prediction then moves
// the entry by the removed calls; the doubt profile.structure_changed stays.
func TestDerivedStructureAndVariance(t *testing.T) {
	r := handFit(t, recsFit(), 0.05)
	rec := recsRecord()
	d := "sha256:" + strings.Repeat("cd", 32)
	recs := records{d: rec}
	own := change.Item{Op: change.OpOwnMs, Service: "recs", Value: change.Point(-0.05), Label: change.LabelMeasuredAB,
		Evidence: []string{d}}
	s := set(own)
	if err := s.Refusal(r, recs); err != nil {
		t.Fatal(err)
	}
	res, err := s.Resolve(r, recs)
	if err != nil {
		t.Fatal(err)
	}
	ratio, se, _ := rec.Services[0].Calls[2].Ratio()
	p := 0.606 * ratio
	want := []change.Item{
		{Op: change.OpOwnMs, Service: "recs", Value: change.Range{Lo: -0.05 - 0.26, Hi: -0.05 + 0.26}},
		{Op: change.OpOwnVar, Service: "recs", Value: change.Range{Lo: -10, Hi: 2}},
		// media is not derived: recs makes more calls than visits (p above 1), and web reaches media through a stage
		{Op: change.OpSetP, Service: "recs", Callee: "profile", Value: change.Range{Lo: 0.606 * (ratio - 2*se),
			Hi: 0.606 * (ratio + 2*se)}},
	}
	if len(res.Items) != len(want) {
		t.Fatalf("resolved %+v", res.Items)
	}
	for k, w := range want {
		g := res.Items[k]
		if g.Op != w.Op || g.Service != w.Service || g.Callee != w.Callee || math.Abs(g.Value.Lo-w.Value.Lo) > 1e-12 ||
			math.Abs(g.Value.Hi-w.Value.Hi) > 1e-12 || g.Label != change.LabelMeasuredAB || g.Evidence[0] != d {
			t.Errorf("item %d: %+v, want %+v", k, g, w)
		}
	}
	if len(s.Items) != 1 {
		t.Error("Resolve changed the set as read")
	}
	fs := s.Validate(r, recs)
	h, _ := s.Digest()
	v, err := Predict(Request{Fit: r, Changes: res, ChangeHash: h, Findings: fs, Plain: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(v.Findings, func(f contract.Finding) bool { return f.Code == contract.ProfileStructureChanged }) {
		t.Error("the structure doubt is gone")
	}
	if len(v.Items) != 3 || v.Items[2].Op != change.OpSetP || v.Items[2].Callee != "profile" || v.Items[2].Sign != change.SignIncreasing {
		t.Errorf("applied %+v", v.Items)
	}
	// the point of the call change alone: the entry loses (0.606 - p) visits of profile's residence and hop
	pt := recsFit()
	if err := pt.Finish(); err != nil {
		t.Fatal(err)
	}
	pt2 := recsFit()
	pt2.Services[1].Calls[0].P = p
	if err := pt2.Finish(); err != nil {
		t.Fatal(err)
	}
	b0, _ := model.AnalyticWith(pt, 0.05, nil, model.Plain)
	b1, _ := model.AnalyticWith(pt2, 0.05, nil, model.Plain)
	v2, err := Predict(Request{Fit: r, Plain: true, Changes: set(change.Item{Op: change.OpSetP, Service: "recs", Callee: "profile",
		Value: change.Point(p), Label: change.LabelMeasuredAB, Evidence: []string{d}})})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryChange(t, v2, "web"); math.Abs(got.Lo-(b1.L[0]-b0.L[0])) > 1e-9 || !(got.Lo < -2) {
		t.Errorf("set_p alone: %+v, want %.4f", got, b1.L[0]-b0.L[0])
	}
	// the derived prediction lies around it, and below 0 throughout
	if got := entryChange(t, v, "web"); !(got.Hi < 0 && got.Lo < b1.L[0]-b0.L[0] && got.Hi > b1.L[0]-b0.L[0]) {
		t.Errorf("derived: %+v around %.4f", got, b1.L[0]-b0.L[0])
	}
	// a probability of 0 removes the call (the low corner), the fit's leaves the entry as it was (the high one)
	pt3 := recsFit()
	pt3.Services[1].Calls = pt3.Services[1].Calls[1:]
	if err := pt3.Finish(); err != nil {
		t.Fatal(err)
	}
	b3, _ := model.AnalyticWith(pt3, 0.05, nil, model.Plain)
	zero := item(change.OpSetP, "recs", 0, 0.606)
	zero.Callee = "profile"
	v3, err := Predict(Request{Fit: r, Plain: true, Changes: set(zero)})
	if err != nil {
		t.Fatal(err)
	}
	if got := entryChange(t, v3, "web"); math.Abs(got.Lo-(b3.L[0]-b0.L[0])) > 1e-9 || math.Abs(got.Hi) > 1e-9 {
		t.Errorf("set_p 0..0.606: %+v, want %.4f..0", got, b3.L[0]-b0.L[0])
	}
	if _, err := Predict(Request{Fit: r, Plain: true, Changes: set(item(change.OpSetP, "recs", 0, 1))}); err == nil {
		t.Error("a set_p without its callee predicted")
	}

	// what the set may say itself: a measured set_p or own_var_ms2 whose value is the record's, given once
	own2 := change.Item{Op: change.OpOwnVar, Service: "recs", Value: change.Point(-4), Label: change.LabelMeasuredAB, Evidence: []string{d}}
	setp := change.Item{Op: change.OpSetP, Service: "recs", Callee: "profile", Value: change.Point(p), Label: change.LabelMeasuredAB,
		Evidence: []string{d}}
	s2 := set(own, own2, setp)
	if err := s2.Refusal(r, recs); err != nil {
		t.Fatal(err)
	}
	if res2, err := s2.Resolve(r, recs); err != nil || len(res2.Items) != 3 {
		t.Errorf("explicit items derived again: %+v %v", res2, err)
	}
	// refused: declared (these ops are measured only), another value, a callee reached through a stage, an unknown
	// callee, no callee
	for _, c := range []struct {
		it   change.Item
		code string
	}{
		{item(change.OpOwnVar, "recs", 10, 10), contract.ChangeUnsupportedOp},
		{change.Item{Op: change.OpSetP, Service: "recs", Callee: "profile", Value: change.Point(0.2), Label: change.LabelDeclared},
			contract.ChangeUnsupportedOp},
		{change.Item{Op: change.OpOwnVar, Service: "recs", Value: change.Point(-3), Label: change.LabelMeasuredAB, Evidence: []string{d}},
			contract.ChangeLabelNotMinted},
		{change.Item{Op: change.OpSetP, Service: "recs", Callee: "profile", Value: change.Point(0.2), Label: change.LabelMeasuredAB,
			Evidence: []string{d}}, contract.ChangeLabelNotMinted},
		{change.Item{Op: change.OpSetP, Service: "web", Callee: "media", Value: change.Point(0.2), Label: change.LabelMeasuredAB,
			Evidence: []string{d}}, contract.ChangeUnsupportedOp},
		{change.Item{Op: change.OpSetP, Service: "recs", Callee: "nope", Value: change.Point(0.2), Label: change.LabelMeasuredAB,
			Evidence: []string{d}}, contract.ChangeUnsupportedOp},
		{change.Item{Op: change.OpSetP, Service: "recs", Value: change.Point(0.2), Label: change.LabelMeasuredAB,
			Evidence: []string{d}}, contract.ChangeUnsupportedOp},
		{change.Item{Op: change.OpSetP, Service: "recs", Callee: "media", Value: change.Point(1.5), Label: change.LabelMeasuredAB,
			Evidence: []string{d}}, contract.ChangeLabelNotMinted},
	} {
		var ce *change.Error
		if err := set(c.it).Refusal(r, recs); !errors.As(err, &ce) || ce.Finding.Code != c.code {
			t.Errorf("%+v: %v, want %s", c.it, err, c.code)
		}
	}

	// a record made before own_var (zero) and with no changed calls derives nothing
	old := recsRecord()
	old.Services[0].OwnVar = abdiff.Delta{}
	old.Services[0].Calls = old.Services[0].Calls[:1]
	old.Services[0].CallsChanged = false
	d2 := "sha256:" + strings.Repeat("ef", 32)
	own.Evidence = []string{d2}
	if res, err := set(own).Resolve(r, records{d2: old}); err != nil || len(res.Items) != 1 {
		t.Errorf("an old record: %+v %v", res, err)
	}
}

// ab.payload_changed in a cited record reaches the prediction naming the fit's callers of the driven service, a
// caller through the call structure's stage nodes by its service.
func TestPayloadDoubtNamesCallers(t *testing.T) {
	r := handFit(t, recsFit(), 0.05)
	rec := &abdiff.Record{Schema: abdiff.Schema, SelfReportConsistent: true, Findings: []contract.Finding{
		contract.NewFinding(contract.ABPayloadChanged).WithSubject("media").WithNumber("baseline_bytes", 16384).
			WithNumber("candidate_bytes", 49152)},
		Services: []abdiff.Service{{Service: "media", OwnMs: abdiff.Delta{Delta: 0.05, SE: 0.02}}}}
	d := "sha256:" + strings.Repeat("12", 32)
	s := set(change.Item{Op: change.OpOwnMs, Service: "media", Value: change.Point(0.05), Label: change.LabelMeasuredAB,
		Evidence: []string{d}})
	fs := s.Validate(r, records{d: rec})
	if len(fs) != 1 || fs[0].Code != contract.ABPayloadChanged || fs[0].Level != contract.Doubt || fs[0].Subject != "media" ||
		strings.Join(fs[0].Examples, " ") != "recs web" || fs[0].Numbers["candidate_bytes"] != 49152 {
		t.Errorf("findings %+v", fs)
	}
	topo := recsFit()
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := change.Callers(topo, topo.Index("profile")); strings.Join(got, " ") != "recs" {
		t.Errorf("callers of profile: %v", got)
	}
}
