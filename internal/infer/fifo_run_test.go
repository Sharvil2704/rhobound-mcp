// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"math"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The same simulated requests captured twice: spans that start at service and spans that include the wait
// for a worker. The FIFO reconstruction removes the wait exactly, so both give the same service times and
// worker counts; the earlier estimator reads them from a subsample and differs.
func TestFIFORemovesTheWaitInsideTheSpan(t *testing.T) {
	read := func(inSpan bool, est string) *infer.Inferred {
		out := run(chain(), 50, 200, 7, inSpan)
		d, err := otlp.ReadDir(writeOTLP(t, out.spans))
		if err != nil {
			t.Fatal(err)
		}
		inf, err := infer.Run(d, infer.Options{Estimator: est})
		if err != nil {
			t.Fatal(err)
		}
		return inf
	}
	at, in := read(false, infer.EstimatorFIFO), read(true, infer.EstimatorFIFO)
	for _, name := range []string{"a", "b", "c"} {
		x, y := at.Topology.Services[at.Topology.Index(name)], in.Topology.Services[in.Topology.Index(name)]
		// cs2 to 1e-3: arrivals closer than 0.05 ms are taken in order of release (the reference's tie rule), which
		// swaps part of the service time between two queued jobs; the mean is unchanged
		if math.Abs(x.S-y.S) > 1e-5*x.S || math.Abs(x.CS2-y.CS2) > 1e-3*x.CS2 || x.Servers*x.Workers != y.Servers*y.Workers {
			t.Errorf("%s: at service S %.6f cs2 %.5f cap %d; wait in span S %.6f cs2 %.5f cap %d", name,
				x.S, x.CS2, x.Servers*x.Workers, y.S, y.CS2, y.Servers*y.Workers)
		}
		st := in.Stats[in.Topology.Index(name)]
		if !st.WorkersFromQueueing || st.InSpanWaitMs <= 0 {
			t.Errorf("%s: the count should be identified from the waits (%+v)", name, st)
		}
	}
	if b := in.Topology.Services[in.Topology.Index("b")]; b.Servers*b.Workers != 2 {
		t.Errorf("b: %d workers, want 2", b.Servers*b.Workers)
	}
	if _, err := infer.Run(&otlp.Data{Spans: []otlp.Span{{}}}, infer.Options{Estimator: "nope"}); err == nil {
		t.Error("an unknown estimator should be refused")
	}
}

// Spans that start once a worker has taken the request show no wait, so no worker count can be pinned. That is
// said once, in one warning naming every service, not in one warning per service.
func TestFIFOLowerBoundsInOneWarning(t *testing.T) {
	out := run(chain(), 50, 200, 7, false)
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{Estimator: infer.EstimatorFIFO})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, w := range inf.Warnings {
		if strings.Contains(w, "lower bound") {
			lines = append(lines, w)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("%d lower-bound warnings, want 1: %q", len(lines), inf.Warnings)
	}
	for _, s := range []string{"a 1+", "b ", "c 1+", "no service shows"} {
		if !strings.Contains(lines[0], s) {
			t.Errorf("the warning should contain %q: %s", s, lines[0])
		}
	}
	for i, st := range inf.Stats {
		if !st.External && (st.WorkersFromQueueing || !st.WorkersInferred) {
			t.Errorf("%s: a lower bound expected (%+v)", inf.Topology.Services[i].Name, st)
		}
	}
}
