// SPDX-License-Identifier: Apache-2.0

package abdiff

import (
	"bytes"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
)

func testRecord() *Record {
	d := "sha256:" + strings.Repeat("b", 64)
	w := Window{Start: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 26, 12, 10, 0, 0, time.UTC)}
	arm := Arm{Capture: "arm", ManifestDigest: d, Window: w, Images: map[string][]string{"b": {d}},
		KeptShare: contract.KeptShare{Value: 1, Source: contract.KeptCounted}}
	return &Record{Schema: Schema, Tool: fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"},
		Arms: Arms{Baseline: arm, Candidate: arm}, OverlapShare: 1, SEMethod: "batch-means/30",
		Loadgen: Loadgen{LogSHA256: strings.Repeat("c", 64), Requests: PairInt{3099, 3101},
			ClientLatencyMs: Delta{Baseline: 45.2, Candidate: 45.9, Delta: 0.7, SE: 0.21}},
		SelfReportConsistent: true,
		Services: []Service{{Service: "b", Visits: PairInt{6198, 6202}, OwnMs: Delta{1.1, 1.5, 0.4, 0.05},
			Calls: []Calls{{Callee: "c", Baseline: 1, Candidate: 1}}, ErrorShare: Pair{0, 0.001}}},
		Findings: []contract.Finding{}}
}

// A record is its canonical bytes: written, read back and written again, it is the same bytes, and its digest is
// theirs.
func TestRecordBytes(t *testing.T) {
	r := testRecord()
	b, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := back.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, b2) || bytes.HasSuffix(b, []byte("\n")) {
		t.Errorf("not stable:\n%s\n%s", b, b2)
	}
	if r.Digest() != contract.Digest(b) {
		t.Error("the digest is not that of the bytes")
	}
	for _, bad := range []string{
		strings.Replace(string(b), `"overlap_share"`, `"overlap"`, 1), // a key the schema does not define
		strings.Replace(string(b), Schema, "rhobound.ab-record/v2", 1),
		string(b) + string(b),
	} {
		if _, err := Unmarshal([]byte(bad)); err == nil {
			t.Errorf("read %s", bad)
		}
	}
}

// Batch means hold for autocorrelated visits: on an AR(1) series with phi 0.5 and unit innovations the standard error
// of the mean is 1/((1-phi) sqrt(n)), twice the textbook sd/sqrt(n) of the series' own spread would say less; batch
// means recovers it (averaged over seeds, within 10%), where the textbook figure is about 42% low.
func TestBatchMeansOnAR1(t *testing.T) {
	const n, phi, seeds = 30000, 0.5, 40
	want := 1 / ((1 - phi) * math.Sqrt(n))
	var sum, naive float64
	for seed := uint64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 7))
		xs := make([]float64, n)
		x := 0.0
		for i := range xs {
			x = phi*x + rng.NormFloat64()
			xs[i] = 10 + x
		}
		_, se := BatchMeans(xs, DefaultBatches)
		sum += se
		var m, v float64
		for _, x := range xs {
			m += x
		}
		m /= n
		for _, x := range xs {
			v += (x - m) * (x - m)
		}
		naive += math.Sqrt(v / (n - 1) / n)
	}
	if got := sum / seeds; math.Abs(got/want-1) > 0.1 {
		t.Errorf("batch-means standard error %.5f, want %.5f", got, want)
	}
	if r := naive / seeds / want; r > 0.7 {
		t.Errorf("the textbook error is %.2f of the true one: the series is not autocorrelated enough to test", r)
	}
	// the edges: one value, no value, fewer values than batches
	if m, se := BatchMeans([]float64{3}, 30); m != 3 || se != 0 {
		t.Errorf("one value: %v %v", m, se)
	}
	if m, se := BatchMeans(nil, 30); m != 0 || se != 0 {
		t.Errorf("none: %v %v", m, se)
	}
	if _, se := BatchMeans([]float64{1, 2, 3, 4}, 30); math.Abs(se-math.Sqrt(5.0/3/4)) > 1e-12 {
		t.Errorf("four values: %v", se)
	}
}

// arm is a synthetic arm: one service b called once per visit by the entry a, with per-visit own times own.
func synthArm(name string, own []float64, calls float64, errs int64, start time.Time, requests int64) *Input {
	inf := &infer.Inferred{Requests: requests, Stats: []infer.ServiceStats{
		{Name: "a", Entry: true, Entries: requests, ObservedServerMs: 10 + mean(own)},
		{Name: "b", ObservedServerMs: mean(own)}},
		Samples: map[string]*infer.Samples{
			"a": {Own: []float64{1, 1, 1, 1}, Glue: []float64{0, 0, 0, 0}, Calls: map[string]float64{"b": 1}, Visits: requests},
			"b": {Own: own, Glue: make([]float64, len(own)), Calls: map[string]float64{"ext:db": calls}, Visits: int64(len(own)),
				Errors: errs}}}
	m := &contract.Manifest{Schema: contract.ManifestSchema, Window: contract.Window{Start: start, End: start.Add(10 * time.Minute),
		Cut: contract.CutNone}}
	return &Input{Capture: name, Manifest: m, Inferred: inf,
		Check: contract.NewCheckReport(nil, nil, m.Digest(), contract.KeptShare{Value: 1, Source: contract.KeptCounted})}
}

func mean(xs []float64) float64 {
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// series is n exponential-like own times with the mean m (a fixed pseudo-random sequence), times k on alternate
// visits and 2-k on the others (k = 1: as drawn).
func series(n int, m, k float64, seed uint64) []float64 {
	rng := rand.New(rand.NewPCG(seed, 3))
	out := make([]float64, n)
	for i := range out {
		f := k
		if i%2 == 1 {
			f = 2 - k
		}
		out[i] = m * rng.ExpFloat64() * f
	}
	return out
}

func loadgen(a, b []float64) *LoadgenLog {
	l := &LoadgenLog{SHA256: strings.Repeat("c", 64)}
	t0 := int64(1793016000000000000)
	for i := range a {
		l.Rows = append(l.Rows, LoadgenRow{Arm: "A", Seq: int64(i), SentUnixNs: t0 + int64(i)*1e6, AnsweredUnixNs: t0 + int64(i)*1e6 + int64(a[i]*1e6), Status: 200})
	}
	for i := range b {
		l.Rows = append(l.Rows, LoadgenRow{Arm: "B", Seq: int64(i), SentUnixNs: t0 + int64(i)*1e6 + 5e5, AnsweredUnixNs: t0 + int64(i)*1e6 + 5e5 + int64(b[i]*1e6), Status: 200})
	}
	return l
}

func codes(r *Record) []string {
	var out []string
	for _, f := range r.Findings {
		out = append(out, f.Code)
	}
	return out
}

// Diff on synthetic arms: a known own-time difference, and each finding the record can raise.
func TestDiff(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	tool := fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"}
	const n = 6000
	ownA, ownB := series(n, 4, 1, 1), series(n, 4, 1, 2)
	for i := range ownB {
		ownB[i] += 0.5
	}
	client := func(own []float64) []float64 {
		out := make([]float64, len(own))
		for i, o := range own {
			out[i] = 10 + o
		}
		return out
	}
	a, b := synthArm("arm-a", ownA, 1, 0, t0, n), synthArm("arm-b", ownB, 1, 0, t0, n)
	r, err := Diff(a, b, loadgen(client(ownA), client(ownB)), Options{}, tool)
	if err != nil {
		t.Fatal(err)
	}
	var sb *Service
	for k := range r.Services {
		if r.Services[k].Service == "b" {
			sb = &r.Services[k]
		}
	}
	if sb == nil || math.Abs(sb.OwnMs.Delta-(mean(ownB)-mean(ownA))) > 1e-12 || math.Abs(sb.OwnMs.Delta-0.5) > 3*sb.OwnMs.SE ||
		!(sb.OwnMs.SE > 0) || sb.CallsChanged || sb.VarianceChanged || !r.SelfReportConsistent || len(r.Findings) != 0 ||
		r.OverlapShare != 1 || r.SEMethod != "batch-means/30" {
		t.Fatalf("+0.5 ms: %+v, findings %v, consistent %v", sb, codes(r), r.SelfReportConsistent)
	}
	if _, err := r.Marshal(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name string
		b    *Input
		log  *LoadgenLog
		opt  Options
		want []string
	}{
		{"variance only", synthArm("arm-b", series(n, 4, 1.8, 2), 1, 0, t0, n), nil, Options{},
			[]string{contract.ProfileVarianceChanged}},
		{"structure", synthArm("arm-b", ownA, 2, 0, t0, n), nil, Options{}, []string{contract.ProfileStructureChanged}},
		{"self-report", synthArm("arm-b", ownB, 1, 0, t0, n), loadgen(client(ownA), client(ownA)), Options{},
			[]string{contract.ProfileSelfReportMismatch}},
		{"errors", synthArm("arm-b", ownA, 1, n/20, t0, n), nil, Options{}, []string{contract.ABErrorsDiffer}},
		{"too few visits", synthArm("arm-b", ownA, 1, 0, t0, n), nil, Options{MarginMs: 0.05},
			[]string{contract.ABTooFewVisits}},
		{"not concurrent", synthArm("arm-b", ownA, 1, 0, t0.Add(5*time.Minute), n), nil, Options{},
			[]string{contract.ABNotConcurrent}},
		{"sampled", func() *Input {
			x := synthArm("arm-b", ownA, 1, 0, t0, n/2)
			x.Check.KeptShare = contract.KeptShare{Value: 1, Source: contract.KeptAssumed}
			return x
		}(), nil, Options{}, []string{contract.ABSampled}},
		{"hardware", func() *Input { x := synthArm("arm-b", ownA, 1, 0, t0, n); x.Hardware = "m7g.large/arm64"; return x }(), nil,
			Options{}, []string{contract.ABHardwareDiffers}},
		// the arm's own count gives a share of 0.5 (one count for both arms), while the log admits it: a doubt, and the
		// record is minted
		{"count conflict", func() *Input {
			x := synthArm("arm-b", ownA, 1, 0, t0, n)
			x.Check.KeptShare = contract.KeptShare{Value: 0.5, Source: contract.KeptCounted}
			return x
		}(), nil, Options{}, []string{contract.ABCountConflict}},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := synthArm("arm-a", ownA, 1, 0, t0, n)
			if c.name == "hardware" {
				a.Hardware = "m7i.large/amd64"
			}
			log := c.log
			if log == nil {
				log = loadgen(client(a.Inferred.Samples["b"].Own), client(c.b.Inferred.Samples["b"].Own))
			}
			r, err := Diff(a, c.b, log, c.opt, tool)
			if err != nil {
				t.Fatal(err)
			}
			if got := codes(r); strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("findings %v, want %v", got, c.want)
			}
			if c.name == "count conflict" {
				f := r.Findings[0]
				if f.Level != contract.Doubt || f.Subject != "arm-b" || f.Numbers["kept_share"] != 0.5 || f.Numbers["log_share"] != 1 {
					t.Errorf("the conflict: %+v", f)
				}
			}
		})
	}
	if _, err := Diff(a, &Input{Capture: "x"}, loadgen(nil, nil), Options{}, tool); err == nil {
		t.Error("an arm without samples is compared")
	}
}

func TestParseLoadgenLog(t *testing.T) {
	good := LoadgenHeader + "\nA,1,1793016000123456789,1793016000131456789,200\nB,1,1793016000124456789,1793016000133012345,200\n"
	l, err := ParseLoadgenLog([]byte(good))
	if err != nil || len(l.Rows) != 2 || l.Rows[1].Arm != "B" || l.Rows[1].AnsweredUnixNs != 1793016000133012345 ||
		l.SHA256 != contract.SHA256Hex([]byte(good)) {
		t.Fatalf("%+v %v", l, err)
	}
	for _, bad := range []string{
		"arm,seq,sent,answered,status\n",
		LoadgenHeader + "\nC,1,1,2,200\n",
		LoadgenHeader + "\nA,1,5,2,200\n",
		LoadgenHeader + "\nA,x,1,2,200\n",
		LoadgenHeader + "\nA,1,1,2\n",
	} {
		if _, err := ParseLoadgenLog([]byte(bad)); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}

// A service with two routes where the candidate shifts one route only: the routes' differences are measured apart,
// profile.route_heterogeneous names the moved route, and the variance test reads the shift of one route of the mixture
// as no variance change; the same blended series spread out (no route moved alone) is one.
func TestDiffRoutes(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	tool := fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"}
	const n = 8000
	mk := func(name string, addB float64, seed uint64) *Input {
		ra, rb := series(n*77/100, 2.5, 1, seed), series(n*23/100, 11, 1, seed+100)
		var own []float64
		for i := range rb {
			rb[i] += addB
		}
		ia, ib := 0, 0
		for k := 0; k < len(ra)+len(rb); k++ { // interleaved in arrival order
			if ib < len(rb) && (ia >= len(ra) || k%100 >= 77) {
				own = append(own, rb[ib])
				ib++
			} else {
				own = append(own, ra[ia])
				ia++
			}
		}
		x := synthArm(name, own, 1, 0, t0, int64(len(own)))
		x.Inferred.Samples["b"].RouteOwn = map[string][]float64{"GET /a": ra, "GET /b": rb}
		return x
	}
	a, b := mk("arm-a", 0, 1), mk("arm-b", 2.5, 1)
	r, err := Diff(a, b, loadgen(a.Inferred.Samples["b"].Own, b.Inferred.Samples["b"].Own), Options{}, tool)
	if err != nil {
		t.Fatal(err)
	}
	sb := r.Services[1]
	if sb.Service != "b" || len(sb.Routes) != 2 || sb.Routes[0].Route != "GET /a" || sb.Routes[0].OwnMs.Delta != 0 ||
		math.Abs(sb.Routes[1].OwnMs.Delta-2.5) > 1e-9 || sb.VarianceChanged {
		t.Fatalf("b: %+v", sb)
	}
	got := codes(r)
	if len(got) != 1 || got[0] != contract.ProfileRouteHeterogeneous || r.Findings[0].Numbers["route_delta_ms"] < 2.4 {
		t.Errorf("findings %v %+v", got, r.Findings)
	}
	// without routes the same arms read as a variance change (the blended test), and nothing about routes
	a.Inferred.Samples["b"].RouteOwn, b.Inferred.Samples["b"].RouteOwn = nil, nil
	r, err = Diff(a, b, loadgen(a.Inferred.Samples["b"].Own, b.Inferred.Samples["b"].Own), Options{}, tool)
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(r); len(got) != 1 || got[0] != contract.ProfileVarianceChanged || r.Services[1].Routes != nil {
		t.Errorf("blended: %v", got)
	}
}
