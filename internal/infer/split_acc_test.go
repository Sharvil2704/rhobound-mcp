// SPDX-License-Identifier: Apache-2.0

//go:build accuracy

package infer_test

import (
	"fmt"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
)

// TestNoUnevenSplitInScenarios: the simulator splits visits over pods at random, so the even-split test must
// flag none of the end-to-end harness's services (their numbers assume it does not; the structure family's
// uneven splits are left out). It runs each row's capture (its own seed, both span modes) for the development
// seeds 31337 and 1-7; no truth is computed. docs/ACCURACY.md (section 1, item 9) also reports it for seeds 301-308.
//
//	go test -tags accuracy -run TestNoUnevenSplitInScenarios -v -count=1 ./internal/infer
func TestNoUnevenSplitInScenarios(t *testing.T) {
	tested, flagged := 0, 0
	minP := 1.0
	for _, seed := range []int64{31337, 1, 2, 3, 4, 5, 6, 7} {
		for row, s := range e2eScenarios(seed, false) {
			for _, spanq := range []bool{false, true} {
				d, err := readCapture(t, run(s.build(0), s.lam, 400, rowSeed(seed, row, 0), spanq).spans)
				if err != nil {
					t.Fatal(err)
				}
				inf, err := infer.Run(d, infer.Options{})
				if err != nil {
					t.Fatal(err)
				}
				for i, st := range inf.Stats {
					if st.SplitP == nil {
						continue
					}
					tested++
					minP = min(minP, *st.SplitP)
					if st.UnevenSplit || inf.Topology.Services[i].Shares != nil {
						flagged++
						t.Errorf("seed %d %s [%s] spanq=%v: %s flagged: %+v", seed, s.name, s.group, spanq, st.Name, st.PodShares)
					}
				}
			}
		}
	}
	fmt.Printf("%d multi-pod services tested, %d flagged, smallest p %.3g\n", tested, flagged, minP)
	if tested == 0 {
		t.Error("no service was tested")
	}
}
