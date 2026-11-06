// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"math"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// seenError is a service's model latency as its callers saw it (the entry's: its server span), less what they measured, in %.
func seenError(t *testing.T, inf *infer.Inferred, name string) float64 {
	t.Helper()
	res := solved(t, inf)
	k := inf.Topology.Index(name)
	st := inf.Stats[k]
	hop, seen := inf.Topology.Services[k].Hop, st.ObservedClientMs
	if st.Entry {
		hop, seen = 0, st.ObservedServerMs
	}
	return 100 * (res.L[k] + hop - seen) / seen
}

func readHops(t *testing.T, dir string) *infer.Inferred {
	t.Helper()
	d, err := otlp.ReadDir("testdata/hops/" + dir)
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return inf
}

// testdata/hops/browse-45s is 824 whole traces (7,373 spans) of the OpenTelemetry Demo 2.2.0 on a real five-machine k3s
// cluster (AWS, 2026-10-21; 100 Locust users, the browsing mix), the first 45 s of a 420 s window, cut down to what the reader uses: frontend-proxy (Envoy)
// -> frontend (Next.js) -> product-catalog (Go), at 1-2% utilization, with the nested spans of the frameworks as they were
// captured. Over the whole 420 s window the quietest tenth of (client span time less server span time) was 0.18 ms on the
// first edge and 0.61 ms on the second, the means 0.44 and 0.93: with the hop taken as the tenth, the model read frontend
// 19% and product-catalog 31% too fast (the same in this slice). The model's wait is 0.01 ms there, so the difference is the network time's spread, not queueing.
func TestRealQuietEdgesTakeTheMeanGap(t *testing.T) {
	inf := readHops(t, "browse-45s")
	for _, name := range []string{"frontend", "frontend-proxy", "product-catalog"} {
		if e := seenError(t, inf, name); math.Abs(e) > 3 {
			t.Errorf("%s: %+.1f%% off what its callers saw (the hop as the quietest tenth gave -19%% and -31%%)", name, e)
		}
	}
	for _, name := range []string{"frontend", "product-catalog"} {
		st := inf.Stats[inf.Topology.Index(name)]
		if !st.HopFromMean || st.ObservedWaitMs > 0.05 {
			t.Errorf("%s: hop from the mean %v, seen wait %.3f ms", name, st.HopFromMean, st.ObservedWaitMs)
		}
	}
	if h := inf.Topology.Services[inf.Topology.Index("product-catalog")].Hop; math.Abs(h-0.93) > 0.1 {
		t.Errorf("product-catalog hop %.3f ms, want about 0.93 (its calls' mean gap)", h)
	}
}

// testdata/hops/cache-30s is 1,121 whole traces (7,847 spans) of the repository's demo services on the same cluster (level 1
// of the real validation, before the change), the first 30 s of the window: api (2 pods) -> cache and db. The cache is 10%
// busy and its calls' gap has a spread, with a mean 0.2 ms above the quietest tenth (the model read it 3.2% too fast); api
// and db are 32-39% busy, where the gap holds their queues and the hop is the quietest tenth.
func TestRealLoadedEdgesKeepTheQuietestTenth(t *testing.T) {
	inf := readHops(t, "cache-30s")
	if e := seenError(t, inf, "cache"); math.Abs(e) > 1 {
		t.Errorf("cache: %+.1f%% off what its callers saw (the hop as the quietest tenth gave -3.2%%)", e)
	}
	for name, want := range map[string]bool{"cache": true, "api": false, "db": false} {
		if got := inf.Stats[inf.Topology.Index(name)].HopFromMean; got != want {
			t.Errorf("%s: hop from the mean %v, want %v", name, got, want)
		}
	}
}
