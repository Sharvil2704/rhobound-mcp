// SPDX-License-Identifier: Apache-2.0

package infer_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// Framework-style nested spans in the simulator (nestSpans): a request through the same services, with and without
// a SERVER span inside each SERVER span and a CLIENT span around each CLIENT span, must be read as the same
// requests. The simulator keeps the outer SERVER span's and the inner CLIENT span's times, so the whole inference
// (visits, the queue reconstruction by the FIFO release, own and glue times, the call structure) must come out
// identical, not just close; what differs is the span count and the notice.
func TestNestedSpansReadAsOneInTheSimulator(t *testing.T) {
	fan := func() *node {
		b := &node{name: "b", c: 1, pods: 2, mean: 30}
		c := &node{name: "c", c: 1, pods: 1, mean: 15}
		return &node{name: "a", c: 4, pods: 1, mean: 3, post: 2, parallel: true, children: []*node{b, c}}
	}
	for _, tc := range []struct {
		name string
		tree func() *node
		lam  float64
		in   bool // spans include the wait for a worker
		nest nestOpts
	}{
		{"chain, server and client, queue outside the span", chain, 45, false, nestOpts{server: true, client: true}},
		{"chain, server and client, queue inside the span", chain, 45, true, nestOpts{server: true, client: true}},
		{"chain, server only", chain, 45, false, nestOpts{server: true}},
		{"chain, client only", chain, 45, false, nestOpts{client: true}},
		{"chain, client without a peer on the inner span", chain, 45, false, nestOpts{client: true, bare: true}},
		{"fan-out with glue, server and client", fan, 40, false, nestOpts{server: true, client: true}},
		{"fan-out with glue, queue inside the span", fan, 40, true, nestOpts{server: true, client: true, bare: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := runWith(tc.tree(), tc.lam, 300, 17, simOpts{queueInSpan: tc.in})
			nested := runWith(tc.tree(), tc.lam, 300, 17, simOpts{queueInSpan: tc.in, nest: tc.nest})
			var nSrv, nCli int
			perSrv, perCli := map[string]int64{}, map[string]int64{}
			for _, sp := range plain.spans {
				switch sp.kind {
				case 2:
					nSrv++
					perSrv[sp.svc]++
				case 3:
					nCli++
					perCli[sp.svc]++
				}
			}
			wantExtra := 0
			if tc.nest.server {
				wantExtra += nSrv
			}
			if tc.nest.client {
				wantExtra += nCli
			}
			if len(nested.spans) != len(plain.spans)+wantExtra {
				t.Fatalf("the nested capture has %d spans, want %d + %d", len(nested.spans), len(plain.spans), wantExtra)
			}
			// the nesting is in the data: the inner SERVER spans are direct children of SERVER spans of the same pod
			direct := 0
			byID := map[string]span{}
			for _, sp := range nested.spans {
				byID[sp.id] = sp
			}
			for _, sp := range nested.spans {
				if p, ok := byID[sp.parent]; ok && p.kind == sp.kind && p.svc == sp.svc && p.pod == sp.pod {
					direct++
				}
			}
			if direct != wantExtra {
				t.Fatalf("the simulator wrote %d nested pairs, want %d", direct, wantExtra)
			}

			base := infer1(t, plain.spans)
			inf := infer1(t, nested.spans)

			// the same inference, to the byte: the topology the model reads and every service's statistics
			bt, _ := model.MarshalTopology(base.Topology)
			nt, _ := model.MarshalTopology(inf.Topology)
			if !bytes.Equal(bt, nt) {
				t.Errorf("the topology differs with nested spans:\nplain  %s\nnested %s", bt, nt)
			}
			bs, _ := json.Marshal(base.Stats)
			ns, _ := json.Marshal(inf.Stats)
			if !bytes.Equal(bs, ns) {
				t.Errorf("the statistics differ with nested spans:\nplain  %s\nnested %s", bs, ns)
			}
			if inf.Requests != base.Requests || inf.BrokenTraces != 0 || inf.LamExtPerMs != base.LamExtPerMs {
				t.Errorf("requests %d (plain %d), broken %d, rate %v (plain %v)", inf.Requests, base.Requests, inf.BrokenTraces,
					inf.LamExtPerMs, base.LamExtPerMs)
			}
			for _, sv := range inf.Topology.Services {
				if strings.HasPrefix(sv.Name, "ext:") {
					t.Errorf("an external service %s: a nested CLIENT span was read as a call that nobody answered", sv.Name)
				}
				for _, c := range sv.Calls {
					if inf.Topology.Services[c.Child].Name == sv.Name {
						t.Errorf("%s calls itself", sv.Name)
					}
				}
			}

			// the notice and the report field name the services and the numbers of spans that were merged
			want := map[string]infer.NestedMerge{}
			if tc.nest.server {
				for svc, n := range perSrv {
					m := want[svc]
					m.Service, m.Server = svc, n
					want[svc] = m
				}
			}
			if tc.nest.client {
				for svc, n := range perCli {
					m := want[svc]
					m.Service, m.Client = svc, n
					want[svc] = m
				}
			}
			if len(inf.NestedSpans) != len(want) {
				t.Errorf("nested spans %+v, want %+v", inf.NestedSpans, want)
			}
			for _, m := range inf.NestedSpans {
				if m.UnscopedServer != m.Server { // the simulator's spans name no instrumentation scope
					t.Errorf("nested spans of %s: %d SERVER, %d without a scope, want all", m.Service, m.Server, m.UnscopedServer)
				}
				m.UnscopedServer = 0
				if want[m.Service] != m {
					t.Errorf("nested spans of %s: %+v, want %+v", m.Service, m, want[m.Service])
				}
				if w := fmt.Sprintf("%s %d SERVER", m.Service, m.Server); m.Server > 0 && !warnedWith(inf, w) {
					t.Errorf("no warning with %q: %q", w, inf.Warnings)
				}
				if w := fmt.Sprintf("%s %d CLIENT", m.Service, m.Client); m.Client > 0 && !warnedWith(inf, w) {
					t.Errorf("no warning with %q: %q", w, inf.Warnings)
				}
			}
			if len(base.NestedSpans) != 0 {
				t.Errorf("the plain capture reports nested spans: %+v", base.NestedSpans)
			}
		})
	}
}

func warnedWith(inf *infer.Inferred, sub string) bool {
	for _, w := range inf.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// The same nesting, but a service calls itself over the network (a SERVER -> CLIENT -> SERVER chain in one service):
// the framework's nested SERVER spans are merged and the real recursion is still refused as a cycle.
func TestNestedSpansDoNotHideRecursionInTheSimulator(t *testing.T) {
	leaf := &node{name: "leaf", c: 2, pods: 1, mean: 5}
	inner := &node{name: "web", c: 4, pods: 1, mean: 3, children: []*node{leaf}}
	web := &node{name: "web", c: 4, pods: 1, mean: 3, children: []*node{inner}}
	gw := &node{name: "gw", c: 4, pods: 1, mean: 2, children: []*node{web}}
	out := runWith(gw, 30, 200, 3, simOpts{nest: nestOpts{server: true, client: true}})
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	_, err = infer.Run(d, infer.Options{})
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "a service calling itself: web;") {
		t.Fatalf("web calling itself over the network should be a cycle, naming web: %v", err)
	}
}

// A runtime with a whole-millisecond clock (Node.js) places the inner span up to a millisecond later than inside the
// outer one, so it can end after it: still one request. Further out than the merge allows (2 ms), the inner span is
// work that outlives the request and is not merged, and the capture is refused as a service calling itself.
func TestNestedSpansWithCoarseClocksInTheSimulator(t *testing.T) {
	plain := runWith(chain(), 45, 300, 17, simOpts{})
	base := infer1(t, plain.spans)
	for _, skew := range []float64{0.9, 1.9} {
		out := runWith(chain(), 45, 300, 17, simOpts{nest: nestOpts{server: true, client: true, skew: skew}})
		inf := infer1(t, out.spans)
		bt, _ := model.MarshalTopology(base.Topology)
		nt, _ := model.MarshalTopology(inf.Topology)
		if !bytes.Equal(bt, nt) {
			t.Errorf("skew %.1f ms: the topology differs with nested spans:\nplain  %s\nnested %s", skew, bt, nt)
		}
	}
	out := runWith(chain(), 45, 300, 17, simOpts{nest: nestOpts{server: true, skew: 2.5}})
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	_, err = infer.Run(d, infer.Options{})
	if err == nil || !strings.Contains(err.Error(), "cycle") || !strings.Contains(err.Error(), "more than 2 ms apart") {
		t.Fatalf("nested spans 2.5 ms apart should be refused as a service calling itself: %v", err)
	}
}

// Run makes the nested spans INTERNAL in the data it is given, so a second Run on the same data finds none left and
// reads the same topology.
func TestNestedSpansRunTwiceOnTheSameData(t *testing.T) {
	out := runWith(chain(), 45, 200, 5, simOpts{nest: nestOpts{server: true, client: true}})
	d, err := otlp.ReadDir(writeOTLP(t, out.spans))
	if err != nil {
		t.Fatal(err)
	}
	first, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := infer.Run(d, infer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := model.MarshalTopology(first.Topology)
	b, _ := model.MarshalTopology(second.Topology)
	if !bytes.Equal(a, b) {
		t.Errorf("the second run reads another topology:\n%s\n%s", a, b)
	}
	if len(first.NestedSpans) == 0 || len(second.NestedSpans) != 0 {
		t.Errorf("nested spans: first %+v, second %+v", first.NestedSpans, second.NestedSpans)
	}
}
