// SPDX-License-Identifier: Apache-2.0

package abdiff

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
)

// gammaHalf is n own times from a gamma of shape 1/2 (cs2 2) with the mean m: m Z^2 for a standard normal Z.
func gammaHalf(n int, m float64, rng *rand.Rand) []float64 {
	out := make([]float64, n)
	for i := range out {
		z := rng.NormFloat64()
		out[i] = m * z * z
	}
	return out
}

// The record carries the own time's variance in each arm and its difference (own_var), the evidence of a measured
// own_var_ms2 item. A rare long visit (every 20th visit +60 ms, as maze's c5 fraud deep check) raises the variance by
// about p(1-p)D^2 = 171 ms^2, which the record measures on average; a constant shift of the same mean leaves the
// variance, and the detector (profile.variance_changed, at 2 standard errors) rarely fires on it.
func TestOwnVarMixture(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	tool := fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"}
	const n, seeds = 4444, 100
	var sumMix, sumShift float64
	var fireMix, fireShift int
	for seed := uint64(1); seed <= seeds; seed++ {
		rng := rand.New(rand.NewPCG(seed, 11))
		ownA := gammaHalf(n, 18, rng)
		mix, shift := gammaHalf(n, 18, rng), gammaHalf(n, 18, rng)
		for i := range mix {
			if i%20 == 12 {
				mix[i] += 60
			}
			shift[i] += 3
		}
		for k, b := range [][]float64{mix, shift} {
			r, err := Diff(synthArm("a", ownA, 1, 0, t0, n), synthArm("b", b, 1, 0, t0, n), loadgen(ownA, b), Options{}, tool)
			if err != nil {
				t.Fatal(err)
			}
			s := r.Services[1]
			if s.Service != "b" || !(s.OwnVar.SE > 0) || math.Abs(s.OwnVar.Delta-(varOf(b)-varOf(ownA))) > 1e-9 {
				t.Fatalf("own_var %+v", s.OwnVar)
			}
			if k == 0 {
				sumMix += s.OwnVar.Delta
				if s.VarianceChanged {
					fireMix++
				}
			} else {
				sumShift += s.OwnVar.Delta
				if s.VarianceChanged {
					fireShift++
				}
			}
			if s.VarianceChanged {
				f := r.Findings[0]
				if f.Code != contract.ProfileVarianceChanged || f.Numbers["var_delta"] != s.OwnVar.Delta ||
					f.Numbers["var_se"] != s.OwnVar.SE {
					t.Fatalf("the finding %+v", f)
				}
			}
		}
	}
	want := 0.05 * 0.95 * 60 * 60
	if got := sumMix / seeds; math.Abs(got/want-1) > 0.1 {
		t.Errorf("mixture: mean own_var delta %.1f, want %.1f", got, want)
	}
	if got := sumShift / seeds; math.Abs(got) > 15 {
		t.Errorf("shift: mean own_var delta %.1f, want about 0", got)
	}
	if fireMix < seeds*6/10 || fireShift > seeds/10 {
		t.Errorf("variance_changed fired on %d of %d mixtures and %d of %d shifts", fireMix, seeds, fireShift, seeds)
	}
}

// The calls per visit carry their standard errors from the counts' spread, and Ratio their ratio with its error.
func TestCallsSE(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	tool := fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"}
	own := series(7500, 4, 1, 1)
	a, b := synthArm("a", own, 0.6, 0, t0, 7500), synthArm("b", own, 0.2, 0, t0, 7500)
	// Bernoulli calls: the mean square is the mean
	a.Inferred.Samples["b"].CallsSq, a.Inferred.Samples["b"].CallVisits = map[string]float64{"ext:db": 0.6}, 7500
	b.Inferred.Samples["b"].CallsSq, b.Inferred.Samples["b"].CallVisits = map[string]float64{"ext:db": 0.2}, 7500
	r, err := Diff(a, b, loadgen(own, own), Options{}, tool)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Services[1].Calls[0]
	seA, seB := math.Sqrt(0.6*0.4/7499), math.Sqrt(0.2*0.8/7499)
	if !r.Services[1].CallsChanged || math.Abs(c.BaselineSE-seA) > 1e-12 || math.Abs(c.CandidateSE-seB) > 1e-12 {
		t.Fatalf("calls %+v, want se %v %v", c, seA, seB)
	}
	ratio, se, ok := c.Ratio()
	if !ok || math.Abs(ratio-1.0/3) > 1e-12 || math.Abs(se-math.Hypot(seB/0.6, ratio*seA/0.6)) > 1e-15 {
		t.Errorf("ratio %v +- %v", ratio, se)
	}
	if _, _, ok := (Calls{Callee: "x", Candidate: 1}).Ratio(); ok {
		t.Error("a ratio over no baseline call")
	}
	// without the counts' spread (an inference made before CallsSq), the errors are 0
	if c := r.Services[0].Calls[0]; c.BaselineSE != 0 || c.CandidateSE != 0 {
		t.Errorf("a's calls %+v", c)
	}
}

// bytesLog is a load generator log with the bytes column: every response of arm A has bytesA bytes, of arm B bytesB.
func bytesLog(n int, bytesA, bytesB int64) []byte {
	var sb strings.Builder
	sb.WriteString(LoadgenHeaderBytes + "\n")
	t0 := int64(1793016000000000000)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "A,%d,%d,%d,200,%d\n", i+1, t0+int64(i)*1e6, t0+int64(i)*1e6+4e6, bytesA)
		fmt.Fprintf(&sb, "B,%d,%d,%d,200,%d\n", i+1, t0+int64(i)*1e6+5e5, t0+int64(i)*1e6+45e5, bytesB)
	}
	return []byte(sb.String())
}

// A log with the bytes column gives the record each arm's mean response body; a change of more than 10% is
// ab.payload_changed, a doubt about the driven service (the arms' entry), whose callers pay to read and decode it in
// spans no arm holds (maze's c4: thumbnails of 16 KiB become 48 KiB).
func TestPayloadChanged(t *testing.T) {
	t0 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	tool := fit.Tool{Name: fit.ToolName, Version: "test", GOARCH: "arm64"}
	const n = 3000
	own := series(n, 4, 1, 1)
	for _, c := range []struct {
		name   string
		a, b   int64
		finds  bool
		nobyte bool
	}{
		{"16 KiB to 48 KiB", 16 << 10, 48 << 10, true, false},
		{"48 KiB to 16 KiB", 48 << 10, 16 << 10, true, false},
		{"5% larger", 20000, 21000, false, false},
		{"no bytes column", 0, 0, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var log *LoadgenLog
			var err error
			if c.nobyte {
				log = loadgen(own, own)
			} else if log, err = ParseLoadgenLog(bytesLog(n, c.a, c.b)); err != nil {
				t.Fatal(err)
			}
			r, err := Diff(synthArm("a", own, 1, 0, t0, n), synthArm("b", own, 1, 0, t0, n), log, Options{}, tool)
			if err != nil {
				t.Fatal(err)
			}
			rb := r.Loadgen.ResponseBytes
			switch {
			case c.nobyte && rb != nil:
				t.Fatalf("response bytes %+v from a log without them", rb)
			case !c.nobyte && (rb == nil || rb.Baseline != float64(c.a) || rb.Candidate != float64(c.b)):
				t.Fatalf("response bytes %+v", rb)
			}
			got := codes(r)
			if c.finds {
				f := r.Findings[0]
				if len(got) != 1 || f.Code != contract.ABPayloadChanged || f.Level != contract.Doubt || f.Subject != "a" ||
					f.Numbers["baseline_bytes"] != float64(c.a) || f.Numbers["candidate_bytes"] != float64(c.b) {
					t.Errorf("findings %+v", r.Findings)
				}
			} else if len(got) != 0 {
				t.Errorf("findings %v", got)
			}
			b, err := r.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if back, err := Unmarshal(b); err != nil || (back.Loadgen.ResponseBytes == nil) != c.nobyte {
				t.Errorf("read back: %v", err)
			}
			if c.nobyte && strings.Contains(string(b), "response_bytes") {
				t.Error("a record without bytes writes response_bytes")
			}
		})
	}
}

// The log's bytes column: read when the header names it, a 5-column log still read as before.
func TestParseLoadgenLogBytes(t *testing.T) {
	l, err := ParseLoadgenLog(bytesLog(2, 100, 300))
	if err != nil || !l.HasBytes || len(l.Rows) != 4 || l.Rows[0].Bytes != 100 || l.Rows[1].Bytes != 300 {
		t.Fatalf("%+v %v", l, err)
	}
	l, err = ParseLoadgenLog([]byte(LoadgenHeader + "\nA,1,1,2,200\n"))
	if err != nil || l.HasBytes || l.Rows[0].Bytes != -1 {
		t.Fatalf("%+v %v", l, err)
	}
	for _, bad := range []string{
		LoadgenHeaderBytes + "\nA,1,1,2,200\n",
		LoadgenHeaderBytes + "\nA,1,1,2,200,-5\n",
		LoadgenHeaderBytes + "\nA,1,1,2,200,x\n",
		LoadgenHeader + "\nA,1,1,2,200,7\n",
		LoadgenHeader + ",size\nA,1,1,2,200,7\n",
	} {
		if _, err := ParseLoadgenLog([]byte(bad)); err == nil {
			t.Errorf("read %q", bad)
		}
	}
}
