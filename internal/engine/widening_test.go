// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"math"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// The biased constructs' widening near a change of zero (lean/Rbproofs/Lanes/ChangeBand.lean, CB-1 and CB-2).
func TestWideningAroundZero(t *testing.T) {
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	// AWS run 75bba15f's web entry: two changes of about 0.01 ms either side of zero no longer move 0.98 ms of
	// widening (non_slowest_branch, 1.7% of the latency) from one side to the other
	bs := []report.Biased{{Construct: report.ConstructSecondSequentialCall}, {Construct: report.ConstructNonSlowestBranch},
		{Construct: report.ConstructStagedCalls}}
	l1, h1 := widening(bs, change.Range{Lo: -0.0101, Hi: 0.0104}, 57.48)
	l2, h2 := widening(bs, change.Range{Lo: -0.0098, Hi: 0.0096}, 57.48)
	if math.Abs(l1-l2) > 0.001 || math.Abs(h1-h2) > 0.001 || l1 > 0.005 || h1 > 0.005 {
		t.Errorf("straddling ranges: widening (%v, %v) and (%v, %v); want small and alike", l1, h1, l2, h2)
	}
	// over-prediction never widens past zero: an increase of [0.1, 0.5] keeps its low side at or above 0
	if l, h := widening(bs, change.Range{Lo: 0.1, Hi: 0.5}, 57.48); !near(l, 0.1) || !near(h, 0.38*0.5) {
		t.Errorf("increase: widening (%v, %v), want (0.1, 0.19)", l, h)
	}
	if l, h := widening(bs, change.Range{Lo: -3, Hi: -2}, 57.48); !near(l, 0.38*3) || !near(h, 2) {
		t.Errorf("decrease: widening (%v, %v), want (1.14, 2)", l, h)
	}
	// continuous where a corner reaches zero
	lA, hA := widening(bs, change.Range{Lo: 1e-12, Hi: 0.5}, 57.48)
	lB, hB := widening(bs, change.Range{Lo: -1e-12, Hi: 0.5}, 57.48)
	if math.Abs(lA-lB) > 1e-9 || math.Abs(hA-hB) > 1e-9 {
		t.Errorf("at lo = 0: (%v, %v) vs (%v, %v)", lA, hA, lB, hB)
	}
	// CB-2: a regular upstream under-predicts both corners of a straddle, so the high side widens too, and a thin
	// policy band no longer passes a true change the construct allows (1 x 1.085 against a 1.06 ms threshold)
	up := []report.Biased{{Construct: report.ConstructRegularUpstream}}
	l, h := widening(up, change.Range{Lo: -1.01, Hi: 1}, 10)
	if !near(l, 0.085*1.01) || !near(h, 0.085) {
		t.Errorf("regular_upstream on a straddle: widening (%v, %v), want (%v, 0.085)", l, h, 0.085*1.01)
	}
	thin := report.DefaultModelBand()
	thin.ChangeFrac, thin.LatencyFrac = 0.05, 0.005
	if b, _ := gate.BandOf(thin, change.Range{Lo: -1.01, Hi: 1}, 10, l, h); !(b.Hi >= 1.085) {
		t.Errorf("thin band high end %v under the construct's reach 1.085", b.Hi)
	}
}
