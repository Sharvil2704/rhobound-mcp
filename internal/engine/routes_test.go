// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// routeCapture writes a capture of one service x with two routes: entry a (77% of the requests) calls x's GET /a,
// own time exponential with mean 2 ms, entry b calls x's GET /b, own 9 ms plus addB, cs2 about 0.5. x has workers
// enough that nothing queues. Seeded: the same arguments write the same bytes.
func routeCapture(t *testing.T, addB float64, seed uint64) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(seed, 7))
	const base = int64(1_800_000_000_000_000_000)
	ns := func(ms float64) string { return fmt.Sprint(base + int64(ms*1e6)) }
	type sp struct {
		trace, id, parent, name, peer string
		kind                          int
		start, end                    float64
	}
	by := map[string][]sp{}
	at := 0.0
	for k := 0; k < 6000; k++ {
		at += rng.ExpFloat64() * 25 // 40 requests per second
		entry, route, own := "a", "GET /a", 2*rng.ExpFloat64()
		if rng.Float64() >= 0.77 {
			entry, route = "b", "GET /b"
			own = 9*(0.5*rng.ExpFloat64()+0.5) + addB
		}
		tr := fmt.Sprintf("%032x", k+1)
		id := func(n int) string { return fmt.Sprintf("%016x", 3*(k+1)+n) }
		xs := at + 0.5 + 0.1
		xe := xs + own
		by[entry] = append(by[entry], sp{tr, id(0), "", "GET /" + entry, "", otlp.KindServer, at, xe + 0.1 + 0.3},
			sp{tr, id(1), id(0), "HTTP GET", "x", otlp.KindClient, at + 0.5, xe + 0.1})
		by["x"] = append(by["x"], sp{tr, id(2), id(1), route, "", otlp.KindServer, xs, xe})
	}
	var b strings.Builder
	for _, svc := range []string{"a", "b", "x"} {
		fmt.Fprintf(&b, `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":%q}},`+
			`{"key":"k8s.pod.name","value":{"stringValue":"%s-0"}}]},"scopeSpans":[{"spans":[`, svc, svc)
		for i, s := range by[svc] {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"traceId":%q,"spanId":%q,"parentSpanId":%q,"name":%q,"kind":%d,"startTimeUnixNano":%q,"endTimeUnixNano":%q`,
				s.trace, s.id, s.parent, s.name, s.kind, ns(s.start), ns(s.end))
			if s.peer != "" {
				fmt.Fprintf(&b, `,"attributes":[{"key":"server.address","value":{"stringValue":%q}}]`, s.peer)
			}
			b.WriteByte('}')
		}
		b.WriteString("]}]}]}\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func inferDir(t *testing.T, dir string, keep bool) *infer.Inferred {
	t.Helper()
	d, err := otlp.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{Workers: map[string]int{"x": 64}, KeepSamples: keep})
	if err != nil {
		t.Fatal(err)
	}
	return inf
}

type recStore map[string]*abdiff.Record

func (s recStore) AB(d string) (*abdiff.Record, bool) { r, ok := s[d]; return r, ok }

// The route split end to end: the fit keeps x's two routes and who reaches each; an A/B that adds 2.5 ms to /b only
// measures /b +2.5 and /a 0, raises profile.route_heterogeneous and no variance doubt; a change set by route predicts
// b +2.5 and a about 0, where the blended item (today's) gives both the blend, 0.23 x 2.5.
func TestRoutesEndToEnd(t *testing.T) {
	base := inferDir(t, routeCapture(t, 0, 1), false)
	var xs *infer.ServiceStats
	for i := range base.Stats {
		if base.Stats[i].Name == "x" {
			xs = &base.Stats[i]
		}
	}
	if xs == nil || len(xs.Routes) != 2 {
		t.Fatalf("x's routes: %+v", xs)
	}
	ra, rb := xs.Routes[0], xs.Routes[1]
	if ra.Route != "GET /a" || rb.Route != "GET /b" || math.Abs(ra.Share-0.77) > 0.02 || math.Abs(ra.OwnMs-2) > 0.15 ||
		math.Abs(rb.OwnMs-9) > 0.4 || math.Abs(ra.Share*ra.OwnMs+rb.Share*rb.OwnMs-xs.OwnMeanMs) > 1e-9 ||
		len(ra.Callers) != 1 || ra.Callers[0] != (infer.RouteCaller{Caller: "a", PerVisit: 1}) ||
		len(rb.Callers) != 1 || rb.Callers[0] != (infer.RouteCaller{Caller: "b", PerVisit: 1}) {
		t.Fatalf("routes %+v", xs.Routes)
	}
	for _, st := range base.Stats {
		if st.Name != "x" && st.Routes != nil {
			t.Errorf("%s has one route and routes %+v", st.Name, st.Routes)
		}
	}
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	man := &contract.Manifest{Schema: contract.ManifestSchema, Window: contract.Window{Start: t0, End: t0.Add(4 * time.Minute),
		Cut: contract.CutNone}}
	rec, err := fit.FromInferred(fit.Input{Capture: "routes", Manifest: man}, base, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	// the record carries the routes and reads back the same
	b, err := rec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := fit.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if inf2, _ := back.Inferred(); len(inf2.Stats) != len(base.Stats) || !strings.Contains(string(b), `"route":"GET /b"`) {
		t.Fatal("the routes do not survive the record")
	}

	// the A/B: +2.5 ms on /b only
	arm := func(name string, inf *infer.Inferred) *abdiff.Input {
		m := &contract.Manifest{Schema: contract.ManifestSchema, Window: contract.Window{Start: t0, End: t0.Add(4 * time.Minute),
			Cut: contract.CutNone}}
		return &abdiff.Input{Capture: name, Manifest: m, Inferred: inf,
			Check: contract.NewCheckReport(nil, nil, m.Digest(), contract.KeptShare{Value: 1, Source: contract.KeptCounted})}
	}
	ia, ib := inferDir(t, routeCapture(t, 0, 2), true), inferDir(t, routeCapture(t, 2.5, 2), true)
	log := &abdiff.LoadgenLog{SHA256: strings.Repeat("c", 64)}
	ab, err := abdiff.Diff(arm("a", ia), arm("b", ib), log, abdiff.Options{}, fit.NewTool("test"))
	if err != nil {
		t.Fatal(err)
	}
	var sx *abdiff.Service
	for k := range ab.Services {
		if ab.Services[k].Service == "x" {
			sx = &ab.Services[k]
		}
	}
	if sx == nil || len(sx.Routes) != 2 {
		t.Fatalf("the record's x: %+v", sx)
	}
	da, db := sx.Routes[0].OwnMs, sx.Routes[1].OwnMs
	if math.Abs(db.Delta-2.5) > 1e-9 || math.Abs(da.Delta) > 1e-9 || sx.VarianceChanged ||
		math.Abs(sx.OwnMs.Delta-0.23*2.5) > 0.05 {
		t.Fatalf("x: own %+v, routes %+v, variance changed %v", sx.OwnMs, sx.Routes, sx.VarianceChanged)
	}
	var codes []string
	for _, f := range ab.Findings {
		codes = append(codes, f.Code)
	}
	if !slicesContains(codes, contract.ProfileRouteHeterogeneous) || slicesContains(codes, contract.ProfileVarianceChanged) {
		t.Errorf("findings %v", codes)
	}

	// predict: by route, then blended
	recs := recStore{ab.Digest(): ab}
	predict := func(items string) (a, b change.Range, findings []string) {
		t.Helper()
		s, err := change.Parse([]byte(`{"schema":"rhobound.change/v1","fit":"` + rec.Digest() + `","items":[` + items + `]}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Refusal(rec, recs); err != nil {
			t.Fatal(err)
		}
		fs := s.Validate(rec, recs)
		r, err := s.Resolve(rec, recs)
		if err != nil {
			t.Fatal(err)
		}
		v, err := Predict(Request{Fit: rec, Changes: r, Findings: fs})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range v.Findings {
			findings = append(findings, f.Code)
		}
		return entryChange(t, v, "a"), entryChange(t, v, "b"), findings
	}
	ev := `"label":"measured-ab","evidence":["` + ab.Digest() + `"]`
	routed := fmt.Sprintf(`{"op":"own_ms","service":"x","route":"GET /a","value":%v,%s},{"op":"own_ms","service":"x","route":"GET /b","value":%v,%s}`,
		da.Delta, ev, db.Delta, ev)
	a, bb, fs := predict(routed)
	// b gets /b's delta +-2 standard errors (about 0.2 ms) at a sensitivity of 1; a gets /a's, about 0
	if !(bb.Lo > 1.9 && bb.Hi < 3.1 && bb.Lo <= 2.5 && bb.Hi >= 2.5) || math.Abs(a.Lo) > 0.1 || math.Abs(a.Hi) > 0.1 {
		t.Errorf("by route: a %+v, b %+v", a, bb)
	}
	if slicesContains(fs, contract.ProfileRouteHeterogeneous) {
		t.Errorf("by route, the record's route doubt is listed: %v", fs)
	}
	a, bb, fs = predict(fmt.Sprintf(`{"op":"own_ms","service":"x","value":%v,%s}`, sx.OwnMs.Delta, ev))
	if math.Abs((a.Lo+a.Hi)/2-0.575) > 0.1 || math.Abs((bb.Lo+bb.Hi)/2-0.575) > 0.1 || !slicesContains(fs, contract.ProfileRouteHeterogeneous) {
		t.Errorf("blended: a %+v, b %+v, findings %v", a, bb, fs)
	}
	// the other route: +1 ms on /a reaches a whole and b not at all
	a, bb, _ = predict(`{"op":"own_ms","service":"x","route":"GET /a","value":1,"label":"declared"}`)
	if math.Abs(a.Lo-1) > 0.05 || a.Lo != a.Hi || math.Abs(bb.Lo) > 0.05 {
		t.Errorf("+1 ms on /a: a %+v, b %+v", a, bb)
	}
	// a route the fit does not name is refused by name
	s, err := change.Parse([]byte(`{"schema":"rhobound.change/v1","fit":"` + rec.Digest() + `","items":[{"op":"own_ms","service":"x","route":"GET /c","value":1,"label":"declared"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Refusal(rec, recs); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Errorf("an unknown route: %v", err)
	}
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
