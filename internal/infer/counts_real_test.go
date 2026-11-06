// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// testdata/counts/shipped-20s is 478 whole traces (539 entry requests, 6,049 spans) of the OpenTelemetry Demo 2.2.0 on a
// real five-machine k3s cluster (AWS, 2026-10-21; 100 Locust users, the shipped load mix), the first 20 s of a 420 s
// window, cut down to what the reader uses. The capture as it stands was refused before the rules for calls that vary: cart calls its Redis 1, 2 or 3
// times per visit (GetCart, EmptyCart, AddItem), checkout calls product-catalog and currency once per cart line, and the
// frontend serves a dozen endpoints with different call shapes (one cart call, two in a row, a recommendation and then four
// catalogue lookups at once, a checkout and then one lookup per line, and so on).
func readShipped(t *testing.T) *infer.Inferred {
	t.Helper()
	d, err := otlp.ReadDir("testdata/counts/shipped-20s")
	if err != nil {
		t.Fatal(err)
	}
	inf, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatalf("the shipped mix is refused: %v", err)
	}
	return inf
}

func TestRealShippedMixIsAccepted(t *testing.T) {
	inf := readShipped(t)
	topo := inf.Topology
	stat := func(name string) infer.ServiceStats { return inf.Stats[topo.Index(name)] }
	calls := func(name, callee string) infer.VariableCall {
		t.Helper()
		for _, v := range stat(name).VariableCalls {
			if v.Callee == callee {
				return v
			}
		}
		t.Fatalf("%s has no variable calls to %s: %+v", name, callee, stat(name).VariableCalls)
		return infer.VariableCall{}
	}
	if inf.BrokenTraces != 0 || inf.Requests != 539 {
		t.Errorf("%d broken traces, %d requests; want 0 and 539", inf.BrokenTraces, inf.Requests)
	}
	// the cart's Redis calls: one (GetCart), three (AddItem) or two (EmptyCart), by endpoint
	v := calls("cart", "ext:valkey-cart")
	want := []infer.CallsVisits{{Calls: 1, Visits: 122}, {Calls: 2, Visits: 20}, {Calls: 3, Visits: 65}}
	if v.ModelledAs != infer.ModelledMean || len(v.VisitsByCalls) != 3 || v.VisitsByCalls[0] != want[0] || v.VisitsByCalls[1] != want[1] || v.VisitsByCalls[2] != want[2] {
		t.Errorf("cart -> valkey: %+v, want the mean count over %v", v, want)
	}
	if math.Abs(v.MeanPerVisit-(122+40+195)/207.0) > 1e-12 {
		t.Errorf("cart -> valkey: mean %v, want %v", v.MeanPerVisit, (122+40+195)/207.0)
	}
	// checkout: one call per cart line
	if v := calls("checkout", "product-catalog"); v.ModelledAs != infer.ModelledMean || len(v.VisitsByCalls) != 4 {
		t.Errorf("checkout -> product-catalog: %+v", v)
	}
	if v := calls("checkout", "currency"); v.ModelledAs != infer.ModelledMean || v.VisitsByCalls[0].Calls != 2 {
		t.Errorf("checkout -> currency: %+v", v)
	}
	// the frontend: a mixture of at most 12 shapes covering all but a few visits
	fe := stat("frontend")
	if len(fe.CallShapes) == 0 || len(fe.CallShapes) > 12 {
		t.Fatalf("frontend: %d call shapes", len(fe.CallShapes))
	}
	share := 0.0
	for _, s := range fe.CallShapes {
		share += s.Share
	}
	if share < 0.85 || share > 1 { // the visits that make no call (a page, a static file) have no shape
		t.Errorf("frontend: the shapes hold %.3f of the visits, want 0.85 to 1", share)
	}
	if fe.CallShapes[0].Shape != "product-catalog" || math.Abs(fe.CallShapes[0].Share-0.461) > 0.001 {
		t.Errorf("frontend: commonest shape %+v, want product-catalog on 46.1%% of visits", fe.CallShapes[0])
	}
	if v := calls("frontend", "cart"); v.ModelledAs != infer.ModelledMixture {
		t.Errorf("frontend -> cart: %+v", v)
	}
	if v := calls("frontend", "product-catalog"); v.ModelledAs != infer.ModelledMixture {
		t.Errorf("frontend -> product-catalog: %+v", v)
	}
	// the load on each callee is its callers' load times the mean calls per visit, exactly: Redis has only the cart as a caller
	vr := topo.VisitRatios()
	at := func(n string) float64 { return vr[topo.Index(n)] }
	if got, want := at("ext:valkey-cart")/at("cart"), calls("cart", "ext:valkey-cart").MeanPerVisit; math.Abs(got-want) > 1e-9 {
		t.Errorf("valkey per cart visit: %v, want %v", got, want)
	}
	// ... and the arrival rates the model has are the observed ones, at every service
	res, err := model.AnalyticWith(topo, inf.LamExtPerMs, nil, model.Default)
	if err != nil {
		t.Fatal(err)
	}
	for i, st := range inf.Stats {
		if st.Stage {
			continue
		}
		if got := res.Lam[i] * 1000; math.Abs(got-st.RateRPS) > 0.001*st.RateRPS {
			t.Errorf("%s: the model's rate %.3f/s, observed %.3f/s", st.Name, got, st.RateRPS)
		}
	}
	// the busiest callee is at a few percent: nothing to warn of about calls that arrive together
	if hasWarning(inf, "times at once") {
		t.Errorf("a batch warning on a capture at 2%% utilization")
	}
	parallelNodesCertain(t, topo)
}

// The capture gives the same topology, statistics and warnings on every read.
func TestRealShippedMixIsDeterministic(t *testing.T) {
	a, b := readShipped(t), readShipped(t)
	if len(a.Warnings) != len(b.Warnings) || len(a.Topology.Services) != len(b.Topology.Services) {
		t.Fatalf("%d and %d warnings, %d and %d nodes", len(a.Warnings), len(b.Warnings), len(a.Topology.Services), len(b.Topology.Services))
	}
	for i := range a.Warnings {
		if a.Warnings[i] != b.Warnings[i] {
			t.Errorf("warning %d differs: %q and %q", i, a.Warnings[i], b.Warnings[i])
		}
	}
	for i := range a.Topology.Services {
		if a.Topology.Services[i].Name != b.Topology.Services[i].Name || len(a.Topology.Services[i].Calls) != len(b.Topology.Services[i].Calls) {
			t.Errorf("node %d differs", i)
		}
	}
}

// The real captures give the same topology, statistics and warnings, to the last digit, with one core and with eight: the
// per-trace work is spread over chunks and merged in a fixed order. (A test of the warnings and the number of nodes alone
// would not see a sum that changes its last bit.)
func TestRealCapturesAreIdenticalAtAnyParallelism(t *testing.T) {
	prev := runtime.GOMAXPROCS(0)
	defer runtime.GOMAXPROCS(prev)
	for _, dir := range []string{"counts/shipped-20s", "hops/browse-45s", "hops/cache-30s"} {
		var ref string
		for _, procs := range []int{1, 3, 8} {
			runtime.GOMAXPROCS(procs)
			d, err := otlp.ReadDir("testdata/" + dir)
			if err != nil {
				t.Fatal(err)
			}
			inf, err := infer.Run(d, infer.Options{})
			if err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			topo, err := model.MarshalTopology(inf.Topology)
			if err != nil {
				t.Fatal(err)
			}
			stats, err := json.Marshal(inf.Stats)
			if err != nil {
				t.Fatal(err)
			}
			got := fmt.Sprintf("%s\n%s\n%v %v %v\n%s", topo, stats, inf.LamExtPerMs, inf.WindowS, inf.Requests, strings.Join(inf.Warnings, "\n"))
			if ref == "" {
				ref = got
			} else if got != ref {
				t.Errorf("%s: the result with %d cores differs from the one with 1", dir, procs)
			}
		}
	}
}
