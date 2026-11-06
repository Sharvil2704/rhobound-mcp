// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"math"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// TestRoutePatchTakesTheMeasuredVariance: a routed service that an own_var_ms2 item also changes takes the measured
// variance for its queue (PatchedCS2 with DVar) instead of the routes' mixture, and its callers' EdgeVar is taken
// against that variance, so the queue's variance change is counted once. Without the item, the mixture's cs2 stays.
func TestRoutePatchTakesTheMeasuredVariance(t *testing.T) {
	topo := &model.Topology{Services: []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 2, P: 1}}},
		{Name: "b", S: 1, CS2: 1, Calls: []model.Call{{Child: 2, P: 1}}},
		{Name: "x", S: 0.77*2 + 0.23*9, CS2: 1.2},
	}}
	stats := make([]infer.ServiceStats, 3)
	stats[2].Routes = []infer.RouteStats{
		{Route: "GET /a", Share: 0.77, OwnMs: 2, OwnCS2: 1, Callers: []infer.RouteCaller{{Caller: "a", PerVisit: 1}}},
		{Route: "GET /b", Share: 0.23, OwnMs: 9, OwnCS2: 0.5, Callers: []infer.RouteCaller{{Caller: "b", PerVisit: 1}}},
	}
	routeDS := map[int][]float64{2: {0, 2.5}}
	patch := func(dvar float64, varied bool) model.Patch {
		p := model.Patch{DS: []float64{0, 0, 0.23 * 2.5}, Shift: []bool{false, false, true}, Glue: make([]float64, 3),
			DVar: []float64{0, 0, dvar}}
		routePatch(topo, stats, routeDS, map[int]bool{2: varied}, &p)
		return p
	}
	mix := patch(0, false)
	meas := patch(4.0, true)
	sv := topo.Services[2]
	s1 := sv.S + 0.23*2.5
	if want := model.PatchedCS2(sv.CS2, sv.S, 0.23*2.5, true, 4.0); meas.CS2[2] != want {
		t.Fatalf("measured cs2 %v, want PatchedCS2's %v", meas.CS2[2], want)
	}
	if mix.CS2[2] == meas.CS2[2] {
		t.Fatalf("the measured variance did not replace the mixture's cs2 %v", mix.CS2[2])
	}
	// the edge means are the routes' alone; the edge variances differ by exactly the two blends' variance changes
	for _, e := range []model.Edge{{Caller: 0, Child: 2}, {Caller: 1, Child: 2}} {
		if mix.Edge[e] != meas.Edge[e] {
			t.Fatalf("edge %v: mean %v vs %v", e, mix.Edge[e], meas.Edge[e])
		}
		blendMix := mix.CS2[2]*s1*s1 - sv.CS2*sv.S*sv.S
		blendMeas := meas.CS2[2]*s1*s1 - sv.CS2*sv.S*sv.S
		if got, want := meas.EdgeVar[e]-mix.EdgeVar[e], blendMix-blendMeas; math.Abs(got-want) > 1e-9 {
			t.Fatalf("edge %v: EdgeVar moved by %v, want %v", e, got, want)
		}
	}
	if math.Abs(meas.CS2[2]*s1*s1-sv.CS2*sv.S*sv.S-4.0) > 1e-9 {
		t.Fatalf("the queue's variance change is %v, want the measured 4", meas.CS2[2]*s1*s1-sv.CS2*sv.S*sv.S)
	}
}
