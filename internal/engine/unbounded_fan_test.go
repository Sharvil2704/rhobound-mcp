// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// fanAt is a lower-bound b (own time sb, scv cs2b, w pooled workers) whose departures feed k busy children (own time
// sc), called by the entry a; par makes b call them in parallel.
func fanAt(k int, sb, cs2b, sc float64, par bool, w int) *model.Topology {
	svcs := []model.Service{
		{Name: "a", S: 1, CS2: 1, Calls: []model.Call{{Child: 1, P: 1}}},
		{Name: "b", S: sb, CS2: cs2b, Servers: w, Parallel: par},
	}
	for j := 0; j < k; j++ {
		svcs[1].Calls = append(svcs[1].Calls, model.Call{Child: 2 + j, P: 1})
		svcs = append(svcs, model.Service{Name: fmt.Sprintf("c%d", j), S: sc, CS2: 1})
	}
	return &model.Topology{Name: "fan", Entry: map[int]float64{0: 1}, Services: svcs}
}

// workers_unbounded is a lower bound of the change at every admissible worker count. A raise of b smooths its
// departures and so shortens its busy children's queues: at a finite count the change can be below the change with b
// unbounded (review of lane D: k 4 in parallel, children at rho 0.84, b +10 ms: 10.0 unbounded, 6.8 at 3 workers),
// so unboundedChange also solves at workerLadder's counts.
func TestUnboundedBelowEveryWorkerCount(t *testing.T) {
	for _, par := range []bool{false, true} {
		for _, k := range []int{1, 4} {
			for _, cs2b := range []float64{1, 4} {
				name := fmt.Sprintf("k %d parallel %v cs2b %g", k, par, cs2b)
				items := set(item(change.OpOwnMs, "b", 10, 10))
				v, err := Predict(Request{Fit: lowerFit(t, fanAt(k, 10, cs2b, 0.84/0.075, par, 2), 0.075, "b"), Changes: items})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				en := wfEntry(t, v, "a")
				if en.Unbounded == nil {
					t.Fatalf("%s: no workers_unbounded", name)
				}
				for w := 2; w <= 64; w++ {
					vw, err := Predict(Request{Fit: handFit(t, fanAt(k, 10, cs2b, 0.84/0.075, par, w), 0.075), Changes: items})
					if err != nil {
						continue
					}
					if e := wfEntry(t, vw, "a"); e.ChangeMs != nil && e.ChangeMs.Lo < en.Unbounded.ChangeMs.Lo-1e-9 {
						t.Errorf("%s: change %v at %d workers below workers_unbounded's %v", name, e.ChangeMs.Lo, w,
							en.Unbounded.ChangeMs.Lo)
					}
				}
			}
		}
	}
}
