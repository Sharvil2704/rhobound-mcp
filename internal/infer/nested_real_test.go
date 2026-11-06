// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The fixtures in testdata/nested are whole traces of the OpenTelemetry Demo 2.2.0 (about 25 services in many
// languages), captured on a real cluster, cut down to what the reader uses (service, pod, ids, kind, times, the
// peer address and port). Each holds one pattern of nested spans:
//
//	frontend-server-in-server   Next.js opens a SERVER span for its request handler inside the HTTP
//	                            instrumentation's SERVER span (frontend, Node.js)
//	flagd-server-in-server      the same in a Go service: flagd's handler span inside the Connect RPC
//	                            interceptor's; and the cart's gRPC client span around its HTTP client's
//	cart-client-in-client       .NET's gRPC client opens a CLIENT span around the HTTP client's CLIENT span
//	                            (cart -> flagd), in whole checkout requests
//	fraud-detection-consumer-in-consumer
//	                            a message consumer's "receive" and "process" spans: a hand-off, not nesting
//
// The start times of Next.js spans are whole milliseconds, so the pairs are not exactly nested in time.

// fixtureNames maps the span ids of a fixture to "scope name", for messages.
func fixtureNames(t *testing.T, dir string) map[uint64]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "traces", "spans.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type jspan struct {
		SpanID string `json:"spanId"`
		Name   string `json:"name"`
	}
	var req struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Scope struct {
					Name string `json:"name"`
				} `json:"scope"`
				Spans []jspan `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	names := map[uint64]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		req.ResourceSpans = nil
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			t.Fatal(err)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					id, _ := hexID(sp.SpanID)
					names[id] = ss.Scope.Name + " " + sp.Name
				}
			}
		}
	}
	return names
}

func hexID(s string) (uint64, bool) {
	var v uint64
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint64(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint64(c-'a'+10)
		default:
			return 0, false
		}
	}
	return v, len(s) == 16
}

// loadFixture reads a fixture and returns its traces (spans in the order Run sorts them), with the service and pod
// keys Run builds from the resources.
func loadFixture(t *testing.T, name string) (traces [][]otlp.Span, d *otlp.Data, resPod, resName []int32) {
	t.Helper()
	d, err := otlp.ReadDir(filepath.Join("testdata", "nested", name))
	if err != nil {
		t.Fatal(err)
	}
	podOf, nameOf := map[string]int32{}, map[string]int32{}
	for _, r := range d.Resources {
		key := r.Service + "\x00" + r.Pod
		if _, ok := podOf[key]; !ok {
			podOf[key] = int32(len(podOf))
		}
		if _, ok := nameOf[r.Service]; !ok {
			nameOf[r.Service] = int32(len(nameOf))
		}
		resPod = append(resPod, podOf[key])
		resName = append(resName, nameOf[r.Service])
	}
	byTrace := map[[2]uint64][]otlp.Span{}
	for _, sp := range d.Spans {
		byTrace[sp.Trace] = append(byTrace[sp.Trace], sp)
	}
	for _, g := range byTrace {
		slices.SortFunc(g, func(a, b otlp.Span) int {
			switch {
			case a.Start < b.Start:
				return -1
			case a.Start > b.Start:
				return 1
			}
			return 0
		})
		traces = append(traces, g)
	}
	return traces, d, resPod, resName
}

// directPairs counts the SERVER spans whose direct parent is a SERVER span of the same service and pod, and the
// CLIENT spans whose direct parent is a CLIENT span of the same service and pod, by service name: the nesting
// as the data shows it, found without ancestor.
func directPairs(d *otlp.Data, g []otlp.Span, resPod []int32) (server, client map[string]int64) {
	server, client = map[string]int64{}, map[string]int64{}
	byID := map[uint64]int{}
	for i := range g {
		byID[g[i].ID] = i
	}
	for i := range g {
		p, ok := byID[g[i].Parent]
		if !ok || g[p].Kind != g[i].Kind || resPod[g[p].Res] != resPod[g[i].Res] {
			continue
		}
		switch g[i].Kind {
		case otlp.KindServer:
			server[d.Resources[g[i].Res].Service]++
		case otlp.KindClient:
			client[d.Resources[g[i].Res].Service]++
		}
	}
	return
}

func TestNestedRealFixtures(t *testing.T) {
	for _, c := range []struct {
		name           string
		server, client map[string]int64 // the merged spans expected, by service, over the whole fixture
	}{
		{"frontend-server-in-server", map[string]int64{"frontend": 4}, nil},
		{"flagd-server-in-server", map[string]int64{"flagd": 2, "frontend": 1}, map[string]int64{"cart": 1}},
		{"cart-client-in-client", map[string]int64{"flagd": 2, "frontend": 2}, map[string]int64{"cart": 2}},
		{"fraud-detection-consumer-in-consumer", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			traces, d, resPod, resName := loadFixture(t, c.name)
			names := fixtureNames(t, filepath.Join("testdata", "nested", c.name))
			gotS, gotC := map[string]int64{}, map[string]int64{}
			for _, g := range traces {
				// what the data holds, before the merge
				wantS, wantC := directPairs(d, g, resPod)
				before := slices.Clone(g)
				pos := spanIndex(g)
				cnt := make([]nestedCount, len(d.Resources))
				mergeNested(g, pos, resPod, resName, cnt)
				seenS, seenC := map[string]int64{}, map[string]int64{}
				for i := range g {
					if g[i].Kind != before[i].Kind {
						svc := d.Resources[g[i].Res].Service
						if g[i].Kind != otlp.KindInternal || before[i].Kind == otlp.KindInternal {
							t.Errorf("%s: span %s changed from kind %d to %d, only to INTERNAL is allowed", svc, names[g[i].ID], before[i].Kind, g[i].Kind)
						}
						if before[i].Kind == otlp.KindServer {
							seenS[svc]++
						} else {
							seenC[svc]++
						}
					}
				}
				// every directly nested SERVER pair loses exactly one span; for CLIENT pairs too (these traces hold no
				// chain of three CLIENT spans, and no CLIENT wrapper around several calls)
				for svc, n := range wantS {
					if seenS[svc] != n {
						t.Errorf("%s: %d SERVER spans made INTERNAL, the trace holds %d nested pairs", svc, seenS[svc], n)
					}
				}
				for svc, n := range wantC {
					if seenC[svc] != n {
						t.Errorf("%s: %d CLIENT spans made INTERNAL, the trace holds %d nested pairs", svc, seenC[svc], n)
					}
				}
				for svc, n := range seenS {
					gotS[svc] += n
				}
				for svc, n := range seenC {
					gotC[svc] += n
				}
				// nothing nested is left, and no counts disagree with the kinds
				for i := range g {
					k := g[i].Kind
					if k != otlp.KindServer && k != otlp.KindClient {
						continue
					}
					if p, _ := ancestor(g, pos, i); p >= 0 && g[p].Kind == k && resPod[g[p].Res] == resPod[g[i].Res] {
						t.Errorf("%s still nested in %s", names[g[i].ID], names[g[p].ID])
					}
				}
				var n, m int64
				for _, x := range cnt {
					n, m = n+x.server, m+x.client
				}
				var ws, wc int64
				for _, v := range seenS {
					ws += v
				}
				for _, v := range seenC {
					wc += v
				}
				if n != ws || m != wc {
					t.Errorf("counted %d SERVER and %d CLIENT, the kinds changed on %d and %d", n, m, ws, wc)
				}
				// work under a demoted span is still under the kept one: every CLIENT span has its owner SERVER span
				// as before, and a CLIENT span with a SERVER span of the callee under it still has it
				for i := range before {
					if before[i].Kind != otlp.KindServer {
						continue
					}
					if g[i].Kind == otlp.KindServer {
						continue
					}
					// a demoted SERVER span: its children's nearest SERVER or CLIENT ancestor is the outer one
					for j := range g {
						if g[j].Parent != g[i].ID || g[j].Kind != otlp.KindClient {
							continue
						}
						o, _ := ownerVisit(g, pos, j)
						if o < 0 || g[o].Kind != otlp.KindServer || resPod[g[o].Res] != resPod[g[i].Res] {
							t.Errorf("%s lost its owner after %s was made INTERNAL", names[g[j].ID], names[g[i].ID])
						}
					}
				}
			}
			if !sameCounts(gotS, c.server) || !sameCounts(gotC, c.client) {
				t.Errorf("merged SERVER spans %v (want %v), CLIENT spans %v (want %v)", gotS, c.server, gotC, c.client)
			}
		})
	}
}

func sameCounts(a, b map[string]int64) bool {
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// The cart's gRPC client opens a CLIENT span around the HTTP client's, and flagd's SERVER span hangs under the inner
// one. Read as they stand, the outer span is a call with no SERVER span under it, to an address that does answer: the
// trace counts as having lost a span. Merged, the span the SERVER span is under is the call.
func TestNestedRealCartKeepsTheCallTheCalleeAnswers(t *testing.T) {
	traces, d, resPod, resName := loadFixture(t, "cart-client-in-client")
	names := fixtureNames(t, filepath.Join("testdata", "nested", "cart-client-in-client"))
	flagd := 0
	for _, g := range traces {
		pos := spanIndex(g)
		mergeNested(g, pos, resPod, resName, make([]nestedCount, len(d.Resources)))
		for i := range g {
			if d.Resources[g[i].Res].Service != "cart" || g[i].Kind != otlp.KindClient || g[i].Peer < 0 || d.Peers[g[i].Peer] != "flagd" {
				continue
			}
			flagd++
			served := false
			for j := range g {
				if g[j].Kind == otlp.KindServer && d.Resources[g[j].Res].Service == "flagd" {
					if p, _ := ancestor(g, pos, j); p == i {
						served = true
					}
				}
			}
			if !served {
				t.Errorf("the cart's CLIENT span to flagd that is left, %s, has no flagd SERVER span under it", names[g[i].ID])
			}
		}
	}
	if flagd != 2 {
		t.Errorf("%d cart -> flagd CLIENT spans left, want 2 (one per checkout request)", flagd)
	}
}

// flagd-server-in-server holds a ResolveBoolean under the cart (SERVER in SERVER, and CLIENT in CLIENT around it), a
// ResolveInt that starts a trace of its own, and a whole checkout: the capture is read to its end, no trace counts
// as having lost a span, and no service is a caller of itself.
func TestNestedRealFlagdThroughRun(t *testing.T) {
	d, err := otlp.ReadDir(filepath.Join("testdata", "nested", "flagd-server-in-server"))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := Run(d, Options{MinRequests: 2})
	if err != nil {
		t.Fatalf("a capture of nested spans should be read: %v", err)
	}
	if inf.BrokenTraces != 0 {
		t.Errorf("broken traces: %d, want 0", inf.BrokenTraces)
	}
	want := []NestedMerge{{Service: "cart", Client: 1}, {Service: "flagd", Server: 2}, {Service: "frontend", Server: 1}}
	if !slices.Equal(inf.NestedSpans, want) {
		t.Errorf("nested spans %+v, want %+v", inf.NestedSpans, want)
	}
	for i, sv := range inf.Topology.Services {
		for _, c := range sv.Calls {
			if c.Child == i {
				t.Errorf("%s calls itself", sv.Name)
			}
		}
	}
}
