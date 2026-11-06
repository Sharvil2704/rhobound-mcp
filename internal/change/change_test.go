// SPDX-License-Identifier: Apache-2.0

package change

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// A value is a number or {"lo", "hi"} when read, and always {"lo", "hi"} when written.
func TestRangeJSON(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Range
	}{
		{`0.4`, Range{0.4, 0.4}},
		{`-10`, Range{-10, -10}},
		{` 3 `, Range{3, 3}},
		{`{"lo": 3.5, "hi": 6}`, Range{3.5, 6}},
		{`{"hi": 2, "lo": 1}`, Range{1, 2}},
		{`{"lo": 6, "hi": 3.5}`, Range{6, 3.5}}, // inverted: Validate refuses it by name
	} {
		var r Range
		if err := json.Unmarshal([]byte(c.in), &r); err != nil || r != c.want {
			t.Errorf("%s: %+v, %v", c.in, r, err)
		}
	}
	for _, bad := range []string{`null`, `"3ms"`, `{"lo": 1}`, `{"hi": 1}`, `{"lo": 1, "hi": 2, "mid": 1.5}`,
		`{"lo": "1", "hi": 2}`, `[1, 2]`, `true`} {
		var r Range
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s read as %+v", bad, r)
		}
	}
	b, err := json.Marshal(Point(0.46))
	if err != nil || string(b) != `{"lo":0.46,"hi":0.46}` {
		t.Errorf("a point is written %s (%v)", b, err)
	}
	if !Point(1).IsPoint() || (Range{1, 2}).IsPoint() {
		t.Error("IsPoint")
	}
}

// The set's digest is over its canonical JSON: the same set, whatever the key order or the value's form in the file,
// has one digest, and Spec (the --patch text) is no part of it.
func TestSetDigest(t *testing.T) {
	read := func(s string) *Set {
		var set Set
		if err := json.Unmarshal([]byte(s), &set); err != nil {
			t.Fatal(err)
		}
		return &set
	}
	fitDigest := "sha256:" + strings.Repeat("0", 64)
	a := read(`{"schema":"` + Schema + `","fit":"` + fitDigest + `","items":[{"op":"own_ms","service":"b","value":0.4,"label":"declared"}]}`)
	b := read(`{"items":[{"label":"declared","value":{"hi":0.4,"lo":0.4},"service":"b","op":"own_ms"}],"fit":"` + fitDigest + `","schema":"` + Schema + `"}`)
	b.Items[0].Spec = "+0.4ms"
	da, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if da != db || !strings.HasPrefix(da, "sha256:") || len(da) != len("sha256:")+64 {
		t.Errorf("digests %s and %s", da, db)
	}
	b.Items[0].Value.Hi = 0.5
	if d, _ := b.Digest(); d == da {
		t.Error("a different value has the same digest")
	}
}

// The signs are those ARCHITECTURE.md 4.5 states.
func TestSignOf(t *testing.T) {
	want := map[string]string{
		OpOwnMs: SignIncreasing, OpOwnPct: SignIncreasing, OpGlueMs: SignIncreasing, OpLoadScale: SignIncreasing,
		OpAddCall: SignIncreasing, OpSetP: SignIncreasing, OpScaleCount: SignIncreasing, OpOwnVar: SignIncreasing,
		OpWorkers:  SignDecreasing,
		OpSetAsync: SignUnsigned, OpMakeParallel: SignUnsigned,
		OpCS2: "", OpRemoveCall: "", "own_seconds": "",
	}
	for op, sign := range want {
		if got := SignOf(op); got != sign {
			t.Errorf("SignOf(%s) = %q, want %q", op, got, sign)
		}
	}
	for _, op := range Ops {
		if _, ok := want[op]; !ok {
			t.Errorf("%s: no expected sign", op)
		}
	}
}

// testFit is the fit of a hand-written chain: a (entry) calls b (one pod, two workers, 4 ms, 1 ms of glue), which
// calls the outside delay ext:db; stage:b:1 is a node of the call structure.
func testFit(t *testing.T) *fit.Record {
	t.Helper()
	topo := &model.Topology{Name: "t", Entry: map[int]float64{0: 1}, Services: []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "b", S: 4, CS2: 1, Servers: 2, Glue: 1, Calls: []model.Call{{Child: 2, P: 1}}},
		{Name: "stage:b:1", Delay: true, CS2: 1, Calls: []model.Call{{Child: 3, P: 1}}},
		{Name: "ext:db", S: 2, CS2: 1, Delay: true},
	}}
	if err := topo.Finish(); err != nil {
		t.Fatal(err)
	}
	inf := &infer.Inferred{Topology: topo, LamExtPerMs: 0.1, WindowS: 60}
	for i, s := range topo.Services {
		_, entry := topo.Entry[i]
		inf.Stats = append(inf.Stats, infer.ServiceStats{Name: s.Name, Entry: entry, External: s.Name == "ext:db",
			Stage: strings.HasPrefix(s.Name, "stage:")})
	}
	r, err := fit.FromInferred(fit.Input{Capture: "t"}, inf, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func parse(t *testing.T, items string) *Set {
	t.Helper()
	s, err := Parse([]byte(`{"schema":"` + Schema + `","fit":"","items":[` + items + `]}`))
	if err != nil {
		t.Fatalf("%s: %v", items, err)
	}
	return s
}

// Every refusal a change set can raise, each named by its code (DATA.md 11.2).
func TestValidateRefusesByName(t *testing.T) {
	r := testFit(t)
	ranged := strings.TrimSuffix(strings.Repeat(`{"op":"set_async","service":"a","value":{"lo":0,"hi":1},"label":"declared"},`,
		MaxUnsignedRanges+1), ",")
	for _, c := range []struct {
		items string
		code  string
	}{
		{`{"op":"own_ms","service":"x","value":1,"label":"declared"}`, contract.ChangeUnknownService},
		{`{"op":"workers","service":"x","value":3,"label":"declared"}`, contract.ChangeUnknownService},
		{`{"op":"workers","service":"ext:db","value":3,"label":"declared"}`, contract.ChangeUnsupportedOp},
		{`{"op":"load_scale","service":"b","value":2,"label":"declared"}`, contract.ChangeUnsupportedOp},
		{`{"op":"own_ms","service":"b","value":-5,"label":"declared"}`, contract.ChangeNegativeTime},
		{`{"op":"own_pct","service":"b","value":{"lo":-120,"hi":10},"label":"declared"}`, contract.ChangeNegativeTime},
		{`{"op":"own_ms","service":"b","value":{"lo":2,"hi":1},"label":"declared"}`, contract.ChangeRangeInverted},
		{`{"op":"cs2","service":"b","value":2,"label":"declared"}`, contract.ChangeUnsupportedOp},
		{ranged, contract.ChangeTooManyRanges},
		{`{"op":"own_ms","service":"b","value":"about 3 ms","label":"declared"}`, contract.ChangeFreeText},
		{`{"op":"own_ms","service":"b","value":1,"label":"declared","route":"GET /orders/{id}"}`, contract.ChangeRouteScopeUnsupported},
		{`{"op":"own_ms","service":"b","value":1,"label":"declared","source":"cpu-profile"}`, contract.ChangeCPUProfileNotOwnTime},
		{`{"op":"own_ms","service":"b","value":1,"label":"declared","source":"canary"}`, contract.ChangeIncludesQueueing},
		{`{"op":"own_ms","service":"b","value":1,"label":"declared","source":"microbenchmark"}`, contract.ChangeMicrobenchUnvalidated},
		{`{"op":"own_ms","service":"b","value":1,"label":"estimated-microbench"}`, contract.ChangeMicrobenchUnvalidated},
		{`{"op":"own_ms","service":"b","value":1,"label":"measured-ab"}`, contract.ChangeLabelNotMinted},
		{`{"op":"own_ms","service":"b","value":1,"label":"estimated-judge"}`, contract.ChangeLabelNotMinted},
	} {
		s := parse(t, c.items)
		fs := s.Validate(r, nil)
		found := false
		for _, f := range fs {
			found = found || f.Code == c.code
			if f.Code == c.code && f.Level != contract.Refuse {
				t.Errorf("%s: %s is not a refusal", c.items, c.code)
			}
		}
		var ce *Error
		if err := s.Refusal(r, nil); !found || !errors.As(err, &ce) {
			t.Errorf("%s: want %s, findings %+v, refusal %v", c.items, c.code, fs, err)
		}
	}
	// stage nodes take an own-time change, as --patch has always allowed; a set that is fine has no finding
	ok := parse(t, `{"op":"own_ms","service":"stage:b:1","value":1,"label":"declared"},
		{"op":"workers","service":"b","value":{"lo":2,"hi":4},"label":"declared"},
		{"op":"load_scale","service":"a","value":1.5,"label":"declared"},
		{"op":"own_pct","service":"b","value":-50,"label":"declared","note":"halves the encoding work"}`)
	if fs := ok.Validate(r, nil); len(fs) != 0 || ok.Refusal(r, nil) != nil {
		t.Errorf("a valid set: %+v", fs)
	}
}

// The shape is checked when a file is read: the schema id, the keys, the operation and label names, the forms of
// workers and load_scale values, the number of items and the note's length.
func TestParseShape(t *testing.T) {
	for _, bad := range []string{
		`{"schema":"rhobound.change/v2","fit":"","items":[]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[],"extra":1}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":1,"label":"declared","why":"x"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_seconds","service":"b","value":1,"label":"declared"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":1,"label":"trusted"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":1}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"workers","service":"b","value":2.5,"label":"declared"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"workers","service":"b","value":0,"label":"declared"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"load_scale","service":"a","value":0,"label":"declared"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":1,"label":"declared","source":"vibes"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":1,"label":"declared","note":"` +
			strings.Repeat("n", MaxNoteChars+1) + `"}]}`,
		`{"schema":"rhobound.change/v1","fit":"","items":[` + strings.TrimSuffix(strings.Repeat(
			`{"op":"own_ms","service":"b","value":1,"label":"declared"},`, MaxItems+1), ",") + `]}`,
	} {
		if s, err := Parse([]byte(bad)); err == nil {
			t.Errorf("read %.120s as %+v", bad, s)
		}
	}
}

// --patch reads as it always has: a bare number is ms, N% scales the own time and the glue, and the first problem in
// the items' order is the refusal, with the message predict CAPTURE prints.
func TestFromPatch(t *testing.T) {
	r := testFit(t)
	s, err := FromPatch(" b=+3ms, a=-10% ,b=2")
	if err != nil {
		t.Fatal(err)
	}
	want := []Item{{Op: OpOwnMs, Service: "b", Value: Point(3), Label: LabelDeclared, Spec: "+3ms"},
		{Op: OpOwnPct, Service: "a", Value: Point(-10), Label: LabelDeclared, Spec: "-10%"},
		{Op: OpOwnMs, Service: "b", Value: Point(2), Label: LabelDeclared, Spec: "2"}}
	for k, w := range want {
		g := s.Items[k]
		if g.Op != w.Op || g.Service != w.Service || g.Value != w.Value || g.Label != w.Label || g.Spec != w.Spec {
			t.Errorf("item %d: %+v, want %+v", k, g, w)
		}
	}
	if fs := s.Validate(r, nil); len(fs) != 0 {
		t.Errorf("findings %+v", fs)
	}
	for _, c := range []struct{ patch, msg, code string }{
		{"x=+1ms", `--patch: unknown service in "x=+1ms" (services: a, b, ext:db)`, contract.ChangeUnknownService},
		{"b", `--patch: unknown service in "b" (services: a, b, ext:db)`, contract.ChangeUnknownService},
		{"b=fast,x=1", `--patch: cannot read "b=fast" (use +3ms or -10%)`, ""},
		{"x=1,b=fast", `--patch: unknown service in "x=1" (services: a, b, ext:db)`, contract.ChangeUnknownService},
		{"b=-3,b=-2", "--patch: b would get a negative service time", contract.ChangeNegativeTime},
		{"b=-120%", "--patch: b would get a negative service time", contract.ChangeNegativeTime},
	} {
		s, err := FromPatch(c.patch)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Refusal(r, nil)
		var ce *Error
		if err == nil || err.Error() != c.msg || errors.As(err, &ce) != (c.code != "") || (ce != nil && ce.Finding.Code != c.code) {
			t.Errorf("%s: %v", c.patch, err)
		}
	}
}
