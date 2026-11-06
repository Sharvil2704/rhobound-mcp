// SPDX-License-Identifier: Apache-2.0

package testgen

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The stages a fixture's defect is raised at: by `rhobound check` (StageCheck), by the inference when a capture is
// fitted (StageFit: a refusal or a warning of today's CLI, which the fit codes), by the model at the observed load or
// after a change (StagePredict), or by the S3 window fetcher (StageFetch, in the mcp module: its fixtures are the
// fetcher's injected store defects, and the fixture here only names the code).
const (
	StageCheck   = "check"
	StageFit     = "fit"
	StagePredict = "predict"
	StageFetch   = "fetch"
)

// Fixture injects one defect into a clean capture.
type Fixture struct {
	Code  string // the code the defect raises
	Stage string
	What  string // the defect, in a few words
	// Build writes the defective capture into the empty folder dir from base (a copy it may change) and says how to
	// run it. Nil for StageFetch.
	Build func(base *Capture, dir string) (*Case, error)
}

// Case is how to run a fixture's capture.
type Case struct {
	Dir         string            // the capture folder
	Manifest    string            // a capture manifest to check instead of the folder ("" for none)
	KeptShare   *float64          // --kept-share
	Samplers    map[string]string // --sampler
	ProbeRoutes []string          // --probe-route
	PodIdentity string            // --pod-identity
	Limits      *otlp.Limits      // reading limits other than the defaults
	Workers     map[string]int    // --workers (StageFit)
	Patch       string            // the change (StagePredict)
	Plain       bool              // --plain: the basic model (a code only it raises)
	// Want is, for StageFit and StagePredict, a part of today's refusal or warning text that names the defect; for
	// load.beyond_validated it is empty, and the model's utilization says it.
	Want string
}

// Fixtures returns every fixture, by code.
func Fixtures() []Fixture {
	out := append([]Fixture(nil), fixtures...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// The services of the base capture (testdata/capture-chain): a calls b, b calls c, each call with a CLIENT span; one
// pod each (a-0, b-0, c-0); 3099 traces over 120 s; trace ids 1 to 3099.
const (
	svcA = "a"
	svcB = "b"
	svcC = "c"
)

func plain(c *Capture, dir string) (*Case, error) {
	return &Case{Dir: dir}, c.Write(dir, "spans.jsonl.gz")
}

// spansOf calls f for the spans of one service and kind.
func spansOf(c *Capture, svc string, kind int, f func(rs *ResourceSpans, s *Span)) {
	c.Visit(func(rs *ResourceSpans, s *Span) {
		if rs.Service() == svc && s.KindOf() == kind {
			f(rs, s)
		}
	})
}

// every keeps the spans whose trace number is a multiple of n.
func every(s *Span, n uint64) bool { return TraceNumber(s.TraceID)%n == 0 }

// dataWindow is the span of a capture's data: the earliest start and the latest end.
func dataWindow(c *Capture) (uint64, uint64) {
	var lo, hi uint64
	first := true
	c.Visit(func(_ *ResourceSpans, s *Span) {
		a, b := s.Times()
		if first || a < lo {
			lo = a
		}
		if first || b > hi {
			hi = b
		}
		first = false
	})
	return lo, hi
}

// entryStarts maps each trace to the start of its entry span (a's SERVER span).
func entryStarts(c *Capture) map[string]uint64 {
	out := map[string]uint64{}
	spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
		a, _ := s.Times()
		out[s.TraceID] = a
	})
	return out
}

// keepTraces keeps the traces for which keep, given the trace's entry offset from the data's start, is true.
func keepTraces(c *Capture, keep func(n uint64, offset time.Duration) bool) {
	t0, _ := dataWindow(c)
	entry := entryStarts(c)
	c.Filter(func(_ *ResourceSpans, s *Span) bool {
		return keep(TraceNumber(s.TraceID), time.Duration(entry[s.TraceID]-t0))
	})
}

// RequestCount writes a request count (rhobound.request-count/v1) of the base capture's entry service a, for the
// capture's own window (its data window).
func RequestCount(c *Capture, count int64) []byte {
	lo, hi := dataWindow(c)
	doc := map[string]any{"schema": "rhobound.request-count/v1",
		"window": map[string]string{"start": time.Unix(0, int64(lo)).UTC().Format(time.RFC3339Nano),
			"end": time.Unix(0, int64(hi)).UTC().Format(time.RFC3339Nano)},
		"source": "loadgen", "entries": []map[string]any{{"entry": svcA, "count": count}}}
	b, _ := json.MarshalIndent(doc, "", "  ")
	return append(b, '\n')
}

// Manifest builds the capture manifest of the trace files under dir/traces (cut "none", window the data's span), for
// fixtures that need one; edit may change it before it is written to dir/manifest.json. It returns the manifest's path.
func Manifest(dir string, c *Capture, edit func(m *contract.Manifest)) (string, error) {
	tdir := filepath.Join(dir, "traces")
	ents, err := os.ReadDir(tdir)
	if err != nil {
		return "", err
	}
	lo, hi := dataWindow(c)
	m := &contract.Manifest{Schema: contract.ManifestSchema, Source: contract.Source{Kind: "dir"},
		Window:   contract.Window{Start: time.Unix(0, int64(lo)).UTC(), End: time.Unix(0, int64(hi)).UTC(), Cut: contract.CutNone},
		Sampling: contract.Sampling{Evidence: contract.KeptAssumed},
		Limits:   contract.Limits{MaxObjectBytes: otlp.DefaultLimits.MaxObjectBytes, MaxCaptureBytes: otlp.DefaultLimits.MaxCaptureBytes}}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(tdir, e.Name()))
		if err != nil {
			return "", err
		}
		m.Objects = append(m.Objects, contract.Object{Name: "traces/" + e.Name(), Size: int64(len(b)), SHA256: contract.SHA256Hex(b)})
	}
	if edit != nil {
		edit(m)
	}
	p := filepath.Join(dir, "manifest.json")
	return p, os.WriteFile(p, append(m.Canonical(), '\n'), 0o644)
}

// hexToBase64 rewrites a hex id as base64, as a generic protobuf-to-JSON converter writes ids.
func hexToBase64(id string) string {
	b, err := hex.DecodeString(id)
	if err != nil || id == "" {
		return id
	}
	return base64.StdEncoding.EncodeToString(b)
}

func lines(c *Capture) [][]byte { return c.Lines() }

func writeLines(dir, name string, ls [][]byte) error {
	return WriteFile(filepath.Join(dir, "traces", name), bytes.Join(ls, nil))
}

var fixtures = []Fixture{
	// ---- the store and the files
	{contract.StoreUnreadableObject, StageCheck, "a zstd object beside the trace files", func(c *Capture, dir string) (*Case, error) {
		if _, err := plain(c, dir); err != nil {
			return nil, err
		}
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "traces", "late.jsonl.zst"), []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0})
	}},
	{contract.StoreCompressionMismatch, StageCheck, "a .gz object that is plain text", func(c *Capture, dir string) (*Case, error) {
		p := filepath.Join(dir, "traces", "spans.jsonl.gz")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		return &Case{Dir: dir}, os.WriteFile(p, bytes.Join(lines(c), nil), 0o644)
	}},
	{contract.StoreManifestMismatch, StageCheck, "a manifest whose sha256 is not the file's", func(c *Capture, dir string) (*Case, error) {
		if _, err := plain(c, dir); err != nil {
			return nil, err
		}
		p, err := Manifest(dir, c, func(m *contract.Manifest) { m.Objects[0].SHA256 = strings.Repeat("0", 64) })
		return &Case{Dir: dir, Manifest: p}, err
	}},
	{contract.StoreTooLarge, StageCheck, "a listing over the capture's byte limit", func(c *Capture, dir string) (*Case, error) {
		if _, err := plain(c, dir); err != nil {
			return nil, err
		}
		p, err := Manifest(dir, c, nil)
		lim := otlp.DefaultLimits
		lim.MaxCaptureBytes = 1 << 10
		return &Case{Dir: dir, Manifest: p, Limits: &lim}, err
	}},
	{contract.InputTooLarge, StageCheck, "more spans than the span limit", func(c *Capture, dir string) (*Case, error) {
		lim := otlp.DefaultLimits
		lim.MaxSpans = 1000
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Limits = &lim
		}
		return cs, err
	}},
	{contract.StorePartitionTZOffset, StageFetch, "partitions in local time", nil},
	{contract.StoreLateBatches, StageFetch, "spans of the window in the last margin minute", nil},
	{contract.StoreSmallBatches, StageFetch, "many small objects per minute", nil},
	{contract.StoreQueryAPIIncomplete, StageFetch, "a trace backend's search API as the source", nil},

	// ---- framing and encodings
	{contract.TraceBadJSON, StageCheck, "a request of the wrong shape", func(c *Capture, dir string) (*Case, error) {
		ls := lines(c)
		ls = append(ls[:5], append([][]byte{[]byte(`{"resourceSpans":"x"}` + "\n")}, ls[5:]...)...)
		return &Case{Dir: dir}, writeLines(dir, "spans.jsonl.gz", ls)
	}},
	{contract.TraceJSONLPretty, StageCheck, "pretty-printed requests in a .jsonl file", func(c *Capture, dir string) (*Case, error) {
		b, err := json.MarshalIndent(c.Requests[0], "", "  ")
		if err != nil {
			return nil, err
		}
		ls := append([][]byte{append(b, '\n')}, lines(c)[1:]...)
		return &Case{Dir: dir}, writeLines(dir, "spans.jsonl.gz", ls)
	}},
	{contract.TraceJSONArray, StageCheck, "one JSON array of requests", func(c *Capture, dir string) (*Case, error) {
		b, err := json.Marshal(c.Requests)
		if err != nil {
			return nil, err
		}
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "traces", "spans.json"), b)
	}},
	{contract.TracePartialTailDropped, StageCheck, "a last line cut short", func(c *Capture, dir string) (*Case, error) {
		ls := lines(c)
		ls = append(ls, ls[0][:len(ls[0])/2])
		return &Case{Dir: dir}, writeLines(dir, "spans.jsonl.gz", ls)
	}},
	{contract.TraceSnakeCaseKeys, StageCheck, "snake_case keys", func(c *Capture, dir string) (*Case, error) {
		b := bytes.Join(lines(c), nil)
		for _, r := range [][2]string{{`"resourceSpans"`, `"resource_spans"`}, {`"scopeSpans"`, `"scope_spans"`},
			{`"traceId"`, `"trace_id"`}, {`"spanId"`, `"span_id"`}, {`"parentSpanId"`, `"parent_span_id"`}} {
			b = bytes.ReplaceAll(b, []byte(r[0]), []byte(r[1]))
		}
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), b)
	}},
	{contract.TracePreV1OTLP, StageCheck, "instrumentationLibrarySpans", func(c *Capture, dir string) (*Case, error) {
		b := bytes.ReplaceAll(bytes.Join(lines(c), nil), []byte(`"scopeSpans"`), []byte(`"instrumentationLibrarySpans"`))
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), b)
	}},
	{contract.TraceNotOTLP, StageCheck, "Zipkin v2 JSON", func(c *Capture, dir string) (*Case, error) {
		var spans []map[string]any
		c.Visit(func(rs *ResourceSpans, s *Span) {
			a, b := s.Times()
			spans = append(spans, map[string]any{"traceId": s.TraceID, "id": s.SpanID, "parentId": s.ParentSpanID,
				"timestamp": a / 1000, "duration": (b - a) / 1000, "localEndpoint": map[string]string{"serviceName": rs.Service()}})
		})
		b, err := json.Marshal(spans)
		if err != nil {
			return nil, err
		}
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "traces", "zipkin.json"), b)
	}},
	{contract.TraceIDsBase64, StageCheck, "base64 ids", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(_ *ResourceSpans, s *Span) {
			s.TraceID, s.SpanID, s.ParentSpanID = hexToBase64(s.TraceID), hexToBase64(s.SpanID), hexToBase64(s.ParentSpanID)
		})
		return plain(c, dir)
	}},
	{contract.TraceTraceID64Bit, StageCheck, "64-bit trace ids", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(_ *ResourceSpans, s *Span) { s.TraceID = s.TraceID[16:] })
		return plain(c, dir)
	}},
	{contract.TraceIDsMixed, StageCheck, "hex and base64 ids in one capture", func(c *Capture, dir string) (*Case, error) {
		for i, r := range c.Requests {
			if i%2 == 1 {
				continue
			}
			(&Capture{Requests: []*Request{r}}).Visit(func(_ *ResourceSpans, s *Span) {
				s.TraceID, s.SpanID, s.ParentSpanID = hexToBase64(s.TraceID), hexToBase64(s.SpanID), hexToBase64(s.ParentSpanID)
			})
		}
		return plain(c, dir)
	}},
	{contract.TraceKindEnumName, StageCheck, "span kinds as proto3 enum names", func(c *Capture, dir string) (*Case, error) {
		names := []string{"SPAN_KIND_UNSPECIFIED", "SPAN_KIND_INTERNAL", "SPAN_KIND_SERVER", "SPAN_KIND_CLIENT", "SPAN_KIND_PRODUCER",
			"SPAN_KIND_CONSUMER"}
		c.Visit(func(_ *ResourceSpans, s *Span) { s.Kind, _ = json.Marshal(names[s.KindOf()]) })
		return plain(c, dir)
	}},
	{contract.TraceKindUnknown, StageCheck, "span kinds as other strings", func(c *Capture, dir string) (*Case, error) {
		names := []string{"Unspecified", "Internal", "Server", "Client", "Producer", "Consumer"}
		c.Visit(func(_ *ResourceSpans, s *Span) { s.Kind, _ = json.Marshal(names[s.KindOf()]) })
		return plain(c, dir)
	}},
	{contract.TraceTimeNotInteger, StageCheck, "times written as floats", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(_ *ResourceSpans, s *Span) {
			a, b := s.Times()
			s.Start = json.RawMessage(fmt.Sprintf("%e", float64(a)))
			s.End = json.RawMessage(fmt.Sprintf("%e", float64(b)))
		})
		return plain(c, dir)
	}},
	{contract.TraceSpansSkipped, StageCheck, "spans with no span id", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcC, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 50) {
				s.SpanID = ""
			}
		})
		return plain(c, dir)
	}},

	// ---- identity
	{contract.TraceServiceNameNotString, StageCheck, "a service.name that is a number", func(c *Capture, dir string) (*Case, error) {
		done := false
		c.Visit(func(rs *ResourceSpans, _ *Span) {
			if !done && rs.Service() == svcC {
				rs.SetAttr("service.name", Int(7))
				done = true
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceServiceNameDefault, StageCheck, "the SDK's default service name", func(c *Capture, dir string) (*Case, error) {
		forResources(c, svcC, func(rs *ResourceSpans, _ int) { rs.SetAttr("service.name", Str("unknown_service:java")) })
		return plain(c, dir)
	}},
	{contract.TraceMixedEnvironments, StageCheck, "two deployment environments", func(c *Capture, dir string) (*Case, error) {
		forAllResources(c, func(rs *ResourceSpans) {
			env := "prod"
			if rs.Service() == svcC {
				env = "staging"
			}
			rs.SetAttr("deployment.environment.name", Str(env))
		})
		return plain(c, dir)
	}},
	{contract.TraceImageDigestMissing, StageCheck, "no container.image.repo_digests (the base capture)", plain},
	{contract.TraceMixedImages, StageCheck, "two image digests for one service", func(c *Capture, dir string) (*Case, error) {
		forAllResources(c, func(rs *ResourceSpans) {
			rs.SetAttr("container.image.repo_digests", Strs("registry.example/"+rs.Service()+"@"+Digest(rs.Service())))
		})
		forResources(c, svcB, func(rs *ResourceSpans, i int) {
			if i >= 5 {
				rs.SetAttr("container.image.repo_digests", Strs("registry.example/b@"+Digest("b-next")))
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceInstancePerProcess, StageCheck, "worker processes of one host as instances, no pod", func(c *Capture, dir string) (*Case, error) {
		forResources(c, svcA, func(rs *ResourceSpans, i int) {
			rs.DelAttr("k8s.pod.name")
			rs.SetAttr("host.name", Str("vm-1"))
			rs.SetAttr("service.instance.id", Str(fmt.Sprintf("worker-%d", i%4)))
		})
		return plain(c, dir)
	}},
	{contract.TracePodShared, StageCheck, "two services on one pod identity", func(c *Capture, dir string) (*Case, error) {
		forResources(c, svcC, func(rs *ResourceSpans, _ int) { rs.SetAttr("k8s.pod.name", Str("b-0")) })
		return plain(c, dir)
	}},
	{contract.TracePodIdentityCollapsed, StageCheck, "every span on the forwarding Collector's pod", func(c *Capture, dir string) (*Case, error) {
		forAllResources(c, func(rs *ResourceSpans) { rs.SetAttr("k8s.pod.name", Str("collector-0")) })
		return plain(c, dir)
	}},
	{contract.TraceFaaSService, StageCheck, "a service on a function platform", func(c *Capture, dir string) (*Case, error) {
		forResources(c, svcC, func(rs *ResourceSpans, _ int) { rs.SetAttr("faas.name", Str("fn-c")) })
		return plain(c, dir)
	}},
	{contract.TracePodChurn, StageCheck, "a pod replaced half way through", func(c *Capture, dir string) (*Case, error) {
		t0, t1 := dataWindow(c)
		mid := t0 + (t1-t0)/2
		splitResources(c, svcB, func(s *Span) bool { a, _ := s.Times(); return a >= mid }, func(rs *ResourceSpans) {
			rs.SetAttr("k8s.pod.name", Str("b-1"))
		})
		return plain(c, dir)
	}},

	// ---- structure
	{contract.TraceNoServerSpans, StageCheck, "SERVER spans exported as INTERNAL", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(_ *ResourceSpans, s *Span) {
			if s.KindOf() == otlp.KindServer {
				s.SetKind(otlp.KindInternal)
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceDuplicateSpansDropped, StageCheck, "identical copies of requests (an exporter retry)", func(c *Capture, dir string) (*Case, error) {
		c.Requests = append(c.Requests, c.Clone().Requests[:3]...)
		return plain(c, dir)
	}},
	{contract.TraceSpanIDCollision, StageCheck, "a second writer of the same span ids", func(c *Capture, dir string) (*Case, error) {
		r := (&Capture{Requests: c.Requests[:1]}).Clone()
		r.Visit(func(_ *ResourceSpans, s *Span) {
			a, b := s.Times()
			s.SetTimes(a, b+1_000_000)
		})
		c.Requests = append(c.Requests, r.Requests...)
		return plain(c, dir)
	}},
	{contract.TraceClientSpansMissing, StageCheck, "no CLIENT spans on the edge a -> b", func(c *Capture, dir string) (*Case, error) {
		parent := map[string]string{}
		spansOf(c, svcA, otlp.KindClient, func(_ *ResourceSpans, s *Span) { parent[s.SpanID] = s.ParentSpanID })
		spansOf(c, svcB, otlp.KindServer, func(_ *ResourceSpans, s *Span) { s.ParentSpanID = parent[s.ParentSpanID] })
		c.Filter(func(rs *ResourceSpans, s *Span) bool { return !(rs.Service() == svcA && s.KindOf() == otlp.KindClient) })
		return plain(c, dir)
	}},
	{contract.TraceContextNotPropagated, StageCheck, "b starts new traces instead of joining a's", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(rs *ResourceSpans, s *Span) {
			if svc := rs.Service(); svc == svcB || svc == svcC {
				s.TraceID = "ff" + s.TraceID[2:]
				if svc == svcB && s.KindOf() == otlp.KindServer {
					s.ParentSpanID = ""
				}
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceEdgeLoss, StageCheck, "3% of b's SERVER spans lost", func(c *Capture, dir string) (*Case, error) {
		c.Filter(func(rs *ResourceSpans, s *Span) bool {
			return !(rs.Service() == svcB && s.KindOf() == otlp.KindServer && every(s, 30))
		})
		return plain(c, dir)
	}},
	{contract.TraceBrokenTraces, StageFit, "12% of b's SERVER spans lost", func(c *Capture, dir string) (*Case, error) {
		c.Filter(func(rs *ResourceSpans, s *Span) bool {
			return !(rs.Service() == svcB && s.KindOf() == otlp.KindServer && every(s, 8))
		})
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "traces miss spans"
		}
		return cs, err
	}},
	{contract.TraceLinksPresent, StageCheck, "CONSUMER spans with links", func(c *Capture, dir string) (*Case, error) {
		addChildren(c, svcC, otlp.KindServer, 50, func(parent *Span, n int) *Span {
			s := childOf(parent, n)
			s.SetKind(otlp.KindConsumer)
			s.Links = []Link{{TraceID: parent.TraceID, SpanID: parent.SpanID}}
			return s
		})
		return plain(c, dir)
	}},
	{contract.TraceAsyncTailTruncated, StageCheck, "traces of the window still open at the margin's end", func(c *Capture, dir string) (*Case, error) {
		t0, t1 := dataWindow(c)
		mid := t0 + (t1-t0)/2
		entry := entryStarts(c)
		addChildren(c, svcC, otlp.KindServer, 20, func(parent *Span, n int) *Span {
			if entry[parent.TraceID] >= mid {
				return nil
			}
			s := childOf(parent, n)
			s.SetKind(otlp.KindInternal)
			a, _ := s.Times()
			s.SetTimes(a, t1-100_000_000)
			return s
		})
		if _, err := plain(c, dir); err != nil {
			return nil, err
		}
		p, err := Manifest(dir, c, func(m *contract.Manifest) {
			m.Window = contract.Window{Start: time.Unix(0, int64(t0)).UTC(), End: time.Unix(0, int64(mid)).UTC(), Cut: contract.CutEntrySpanStart}
			m.Margins = &contract.Margins{Before: 0, After: float64(t1-mid) / 1e9}
		})
		return &Case{Dir: dir, Manifest: p}, err
	}},
	{contract.TracePeerRenamed, StageCheck, "a peer given as a Service's ClusterIP, with a cluster dump", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcA, otlp.KindClient, func(_ *ResourceSpans, s *Span) {
			s.SetAttr("server.address", Str("192.0.2.10"))
			s.SetAttr("server.port", Int(8080))
		})
		if _, err := plain(c, dir); err != nil {
			return nil, err
		}
		list := `{"apiVersion":"v1","kind":"List","items":[{"kind":"Service","metadata":{"name":"b","namespace":"shop"},` +
			`"spec":{"clusterIP":"192.0.2.10","ports":[{"port":8080}]}}]}` + "\n"
		return &Case{Dir: dir}, WriteFile(filepath.Join(dir, "cluster", "services.json"), []byte(list))
	}},
	{contract.TraceClockSkew, StageCheck, "c's clock 5 ms ahead of b's", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(rs *ResourceSpans, s *Span) {
			if rs.Service() == svcC {
				s.Shift(5_000_000)
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceWholeMsStarts, StageCheck, "a's clock stamping whole milliseconds", func(c *Capture, dir string) (*Case, error) {
		c.Visit(func(rs *ResourceSpans, s *Span) {
			if rs.Service() == svcA {
				a, b := s.Times()
				a, b = a-a%1_000_000, b-b%1_000_000
				s.SetTimes(a, max(a, b))
			}
		})
		return plain(c, dir)
	}},

	// ---- sampling (DATA.md 3.8)
	{contract.TraceSamplingUnverified, StageCheck, "no count and no sampler evidence (the base capture)", plain},
	{contract.TraceSamplingMixed, StageCheck, "a sampling threshold on half of the entries", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 2) {
				s.TraceState = "ot=th:8"
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceSamplingMinority, StageCheck, "a sampling threshold on one entry", func(c *Capture, dir string) (*Case, error) {
		first := true
		spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if first && s.ParentSpanID == "" {
				s.TraceState, first = "ot=th:8", false
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceSamplerNotProportional, StageCheck, "a declared X-Ray sampler", func(c *Capture, dir string) (*Case, error) {
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Samplers = map[string]string{svcA: "xray"}
		}
		return cs, err
	}},
	{contract.TraceKeptShareConflict, StageCheck, "a declared kept share against a counted one", func(c *Capture, dir string) (*Case, error) {
		cs, err := plain(c, dir)
		if err != nil {
			return nil, err
		}
		half := 0.5
		cs.KeptShare = &half
		return cs, os.WriteFile(filepath.Join(dir, "request-count.json"), RequestCount(c, 3099), 0o644)
	}},
	{contract.FitQueueRebuildSampled, StageCheck, "half the traces kept, against a request count", func(c *Capture, dir string) (*Case, error) {
		full := RequestCount(c, 3099)
		keepTraces(c, func(n uint64, _ time.Duration) bool { return n%2 == 0 })
		cs, err := plain(c, dir)
		if err != nil {
			return nil, err
		}
		return cs, os.WriteFile(filepath.Join(dir, "request-count.json"), full, 0o644)
	}},
	{contract.TraceRateMismatch, StageCheck, "a request count far from the traced entries", func(c *Capture, dir string) (*Case, error) {
		cs, err := plain(c, dir)
		if err != nil {
			return nil, err
		}
		return cs, os.WriteFile(filepath.Join(dir, "request-count.json"), RequestCount(c, 2000), 0o644)
	}},

	// ---- traffic
	{contract.TraceProbeLikeTraffic, StageCheck, "kube-probe entries", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 40) {
				s.SetAttr("user_agent.original", Str("kube-probe/1.29"))
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceErrorsPresent, StageCheck, "errors on 2.5% of b's visits", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcB, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 40) {
				s.Status = json.RawMessage(`{"code":2}`)
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceShedding, StageCheck, "503 answers on 2.5% of b's visits", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcB, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 40) {
				s.SetAttr("http.response.status_code", Int(503))
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceRetries, StageCheck, "resent requests on 2.5% of a's calls", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcA, otlp.KindClient, func(_ *ResourceSpans, s *Span) {
			if every(s, 40) {
				s.SetAttr("http.request.resend_count", Int(1))
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceStreamingRPC, StageCheck, "server streams on c", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcC, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 50) {
				for i := 0; i < 3; i++ {
					s.Events = append(s.Events, Event{Name: "rpc.message", Attributes: []KV{{"rpc.message.type", Str("SENT")}}})
				}
			}
		})
		return plain(c, dir)
	}},
	{contract.TraceStreams, StageFit, "SERVER spans of c that stay open 40 s", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcC, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 50) {
				a, _ := s.Times()
				s.SetTimes(a, a+40_000_000_000)
			}
		})
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "streams or other calls that stay open"
		}
		return cs, err
	}},
	{contract.TracePersonalDataSuspect, StageCheck, "an e-mail address in a path", func(c *Capture, dir string) (*Case, error) {
		spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
			if every(s, 100) {
				s.SetAttr("url.path", Str("/users/jane.doe@example.com/cart"))
			}
		})
		return plain(c, dir)
	}},

	// ---- arrivals (DATA.md 3.12)
	{contract.TraceLoadTrend, StageCheck, "a fifth of the load in the first half", func(c *Capture, dir string) (*Case, error) {
		keepTraces(c, func(n uint64, off time.Duration) bool { return off >= 60*time.Second || n%5 == 0 })
		return plain(c, dir)
	}},
	{contract.TraceArrivalsOverdispersed, StageCheck, "arrivals in 5 s bursts", func(c *Capture, dir string) (*Case, error) {
		keepTraces(c, func(_ uint64, off time.Duration) bool { return int(off/(5*time.Second))%2 == 0 })
		return plain(c, dir)
	}},
	{contract.TraceArrivalsUnjudged, StageCheck, "500 traces: too few sub-windows", func(c *Capture, dir string) (*Case, error) {
		keepTraces(c, func(n uint64, _ time.Duration) bool { return n <= 500 })
		return plain(c, dir)
	}},

	// ---- the inference and the model (the codes of today's refusals and warnings)
	{contract.TraceTooFewEntries, StageFit, "50 entry requests", func(c *Capture, dir string) (*Case, error) {
		keepTraces(c, func(n uint64, _ time.Duration) bool { return n <= 50 })
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "entry requests in the capture; need at least"
		}
		return cs, err
	}},
	{contract.TraceCycle, StageFit, "c calling a on some visits", func(c *Capture, dir string) (*Case, error) {
		var extra []*Span
		addChildren(c, svcC, otlp.KindServer, 10, func(parent *Span, n int) *Span {
			cl := childOf(parent, n)
			cl.SetKind(otlp.KindClient)
			cl.SetAttr("server.address", Str(svcA))
			srv := childOf(cl, n+1)
			srv.SetKind(otlp.KindServer)
			extra = append(extra, srv)
			return cl
		})
		c.Requests = append(c.Requests, &Request{ResourceSpans: []*ResourceSpans{{
			Resource:   Resource{Attributes: []KV{{"service.name", Str(svcA)}, {"k8s.pod.name", Str("a-0")}}},
			ScopeSpans: []*ScopeSpans{{Spans: extra}}}}})
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "call graph is not usable"
		}
		return cs, err
	}},
	{contract.TraceTooManyShapes, StageFit, "a's calls to b at once in 20 different counts", func(c *Capture, dir string) (*Case, error) {
		bServer := map[string]*Span{}
		spansOf(c, svcB, otlp.KindServer, func(_ *ResourceSpans, s *Span) { bServer[s.ParentSpanID] = s })
		var extra []*Span
		var clients []*Span
		spansOf(c, svcA, otlp.KindClient, func(_ *ResourceSpans, s *Span) { clients = append(clients, s) })
		var added []*Span
		for _, cl := range clients {
			k := int(TraceNumber(cl.TraceID) % 20)
			for j := 0; j < k; j++ {
				dup := cl.Copy()
				dup.SpanID = fmt.Sprintf("%016x", 0xa000000000000000|TraceNumber(cl.TraceID)<<8|uint64(j))
				added = append(added, dup)
				if srv := bServer[cl.SpanID]; srv != nil {
					s := srv.Copy()
					s.SpanID = fmt.Sprintf("%016x", 0xb000000000000000|TraceNumber(cl.TraceID)<<8|uint64(j))
					s.ParentSpanID = dup.SpanID
					extra = append(extra, s)
				}
			}
		}
		appendSpans(c, svcA, otlp.KindClient, added)
		appendSpans(c, svcB, otlp.KindServer, extra)
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "call shapes"
		}
		return cs, err
	}},
	{contract.TraceNegativeServiceTime, StageFit, "one visit holding b's worker 10 s, and --workers b=1", func(c *Capture, dir string) (*Case, error) {
		holdWorker(c, svcB, 1000, 10*time.Second)
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Workers = map[string]int{svcB: 1}
			cs.Want = "negative mean service time"
		}
		return cs, err
	}},
	// the refined model takes calls made at once to a pooled callee as a batch (model.Config.Batch) and notes it without
	// the code; the basic model (--plain) has no batch term and raises it
	{contract.ModelBatchArrivals, StageFit, "b calling c four times at once on a quarter of its visits (--plain)", func(c *Capture, dir string) (*Case, error) {
		BatchCalls(c, 4, 4)
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want, cs.Plain = "times at once", true
		}
		return cs, err
	}},
	{contract.ModelUtilizationObserved, StagePredict, "c 3.6 times slower: 93% busy", func(c *Capture, dir string) (*Case, error) {
		SlowDown(c, svcC, 3.6)
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Want = "refusing to predict: at the observed load"
		}
		return cs, err
	}},
	{contract.ModelUtilizationAfter, StagePredict, "a change that saturates c", func(c *Capture, dir string) (*Case, error) {
		cs, err := plain(c, dir)
		if cs != nil {
			cs.Patch, cs.Want = svcC+"=+30ms", "refusing the change: after it"
		}
		return cs, err
	}},
	{contract.LoadBeyondValidated, StagePredict, "c 2.7 times slower: 70% busy", func(c *Capture, dir string) (*Case, error) {
		SlowDown(c, svcC, 2.7)
		return plain(c, dir)
	}},
}

// forAllResources calls f for every resource.
func forAllResources(c *Capture, f func(rs *ResourceSpans)) {
	for _, r := range c.Requests {
		for _, rs := range r.ResourceSpans {
			f(rs)
		}
	}
}

// forResources calls f for the resources of one service, with their index among them in the order read.
func forResources(c *Capture, svc string, f func(rs *ResourceSpans, i int)) {
	i := 0
	forAllResources(c, func(rs *ResourceSpans) {
		if rs.Service() == svc {
			f(rs, i)
			i++
		}
	})
}

// Digest is a stable, made-up image digest for a name.
func Digest(name string) string { return contract.Digest([]byte("image " + name)) }

// childOf returns a new span of the parent's trace, under it, inside its time; n makes its id unique.
func childOf(parent *Span, n int) *Span {
	a, b := parent.Times()
	s := &Span{TraceID: parent.TraceID, SpanID: fmt.Sprintf("%016x", 0xc000000000000000|TraceNumber(parent.TraceID)<<8|uint64(n&0xff)),
		ParentSpanID: parent.SpanID}
	s.SetTimes(a+(b-a)/4, b-(b-a)/4)
	return s
}

// addChildren adds, under every n-th span of one service and kind (by trace number), the span make returns (none when
// it returns nil), in the parent's own scope.
func addChildren(c *Capture, svc string, kind int, n uint64, make func(parent *Span, k int) *Span) {
	for _, r := range c.Requests {
		for _, rs := range r.ResourceSpans {
			if rs.Service() != svc {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				var add []*Span
				for _, s := range ss.Spans {
					if s.KindOf() == kind && every(s, n) {
						if ch := make(s, 1); ch != nil {
							add = append(add, ch)
						}
					}
				}
				ss.Spans = append(ss.Spans, add...)
			}
		}
	}
}

// appendSpans adds spans to the first resource of a service that holds spans of that kind.
func appendSpans(c *Capture, svc string, kind int, spans []*Span) {
	for _, r := range c.Requests {
		for _, rs := range r.ResourceSpans {
			if rs.Service() != svc {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					if s.KindOf() == kind {
						ss.Spans = append(ss.Spans, spans...)
						return
					}
				}
			}
		}
	}
}

// splitResources moves the spans of a service for which move is true into a copy of their resource that edit changes,
// in the same request.
func splitResources(c *Capture, svc string, move func(s *Span) bool, edit func(rs *ResourceSpans)) {
	for _, r := range c.Requests {
		var add []*ResourceSpans
		for _, rs := range r.ResourceSpans {
			if rs.Service() != svc {
				continue
			}
			moved := &ResourceSpans{Resource: Resource{Attributes: append([]KV(nil), rs.Resource.Attributes...)}}
			for _, ss := range rs.ScopeSpans {
				var keep, out []*Span
				for _, sp := range ss.Spans {
					if move(sp) {
						out = append(out, sp)
					} else {
						keep = append(keep, sp)
					}
				}
				ss.Spans = keep
				if len(out) > 0 {
					moved.ScopeSpans = append(moved.ScopeSpans, &ScopeSpans{Scope: ss.Scope, Spans: out})
				}
			}
			if len(moved.ScopeSpans) > 0 {
				edit(moved)
				add = append(add, moved)
			}
		}
		r.ResourceSpans = append(r.ResourceSpans, add...)
	}
	c.Filter(func(*ResourceSpans, *Span) bool { return true }) // drops the resources left empty
}

// SlowDown makes the callee svc of the base capture's chain k times slower per visit, as a single first-come-first-
// served worker serves it: each of its SERVER spans starts when the worker is free and lasts k times as long, and the
// spans above it in its trace (its caller's CLIENT and SERVER spans, and theirs) end that much later. Its load rises
// k-fold with the same worker count.
func SlowDown(c *Capture, svc string, k float64) {
	byID := map[string]*Span{}
	var calls []*Span
	c.Visit(func(rs *ResourceSpans, s *Span) {
		byID[s.TraceID+s.SpanID] = s
		if rs.Service() == svc && s.KindOf() == otlp.KindServer {
			calls = append(calls, s)
		}
	})
	sort.Slice(calls, func(i, j int) bool { a, _ := calls[i].Times(); b, _ := calls[j].Times(); return a < b })
	var free uint64
	for _, s := range calls {
		a, b := s.Times()
		start := max(a, free)
		end := start + uint64(k*float64(b-a))
		free = end
		delta := int64(end) - int64(b)
		s.SetTimes(start, end)
		for p := byID[s.TraceID+s.ParentSpanID]; p != nil; p = byID[p.TraceID+p.ParentSpanID] {
			pa, pb := p.Times()
			p.SetTimes(pa, uint64(int64(pb)+delta))
		}
	}
}

// BatchCalls makes every m-th visit of b call c n times at once: b's CLIENT span to c becomes n CLIENT spans that
// start together, and c serves them one after another (each SERVER span of c starts when the last ended); b's span, and
// the spans above it, end that much later.
func BatchCalls(c *Capture, n int, m uint64) {
	byID := map[string]*Span{}
	c.Visit(func(_ *ResourceSpans, s *Span) { byID[s.TraceID+s.SpanID] = s })
	var clients, servers []*Span
	c.Visit(func(rs *ResourceSpans, s *Span) {
		if rs.Service() != svcC || s.KindOf() != otlp.KindServer || !every(s, m) {
			return
		}
		cl := byID[s.TraceID+s.ParentSpanID]
		if cl == nil {
			return
		}
		ca, cb := cl.Times()
		sa, sb := s.Times()
		dur := sb - sa
		for j := 1; j < n; j++ {
			dc := cl.Copy()
			dc.SpanID = fmt.Sprintf("%016x", 0xa000000000000000|TraceNumber(cl.TraceID)<<8|uint64(j))
			ds := s.Copy()
			ds.SpanID = fmt.Sprintf("%016x", 0xb000000000000000|TraceNumber(cl.TraceID)<<8|uint64(j))
			ds.ParentSpanID = dc.SpanID
			off := uint64(j) * dur
			ds.SetTimes(sa+off, sb+off)
			dc.SetTimes(ca+uint64(j)*1000, cb+off)
			clients = append(clients, dc)
			servers = append(servers, ds)
		}
		delta := int64(uint64(n-1) * dur)
		for p := byID[cl.TraceID+cl.ParentSpanID]; p != nil; p = byID[p.TraceID+p.ParentSpanID] {
			pa, pb := p.Times()
			p.SetTimes(pa, uint64(int64(pb)+delta))
		}
	})
	appendSpans(c, svcB, otlp.KindClient, clients)
	appendSpans(c, svcC, otlp.KindServer, servers)
}

// holdWorker makes one visit of svc (in trace n) hold its worker for d before its call out: its CLIENT span and
// everything under it start d later, and the spans above it end d later.
func holdWorker(c *Capture, svc string, n uint64, d time.Duration) {
	byID := map[string]*Span{}
	c.Visit(func(_ *ResourceSpans, s *Span) { byID[s.TraceID+s.SpanID] = s })
	var visit *Span
	c.Visit(func(rs *ResourceSpans, s *Span) {
		if rs.Service() == svc && s.KindOf() == otlp.KindServer && TraceNumber(s.TraceID) == n {
			visit = s
		}
	})
	if visit == nil {
		return
	}
	// everything under the visit's span moves; the visit and its ancestors end later
	under := func(s *Span) bool {
		for p := byID[s.TraceID+s.ParentSpanID]; p != nil; p = byID[p.TraceID+p.ParentSpanID] {
			if p == visit {
				return true
			}
		}
		return false
	}
	c.Visit(func(_ *ResourceSpans, s *Span) {
		if s.TraceID == visit.TraceID && under(s) {
			s.Shift(int64(d))
		}
	})
	for p := visit; p != nil; p = byID[p.TraceID+p.ParentSpanID] {
		a, b := p.Times()
		p.SetTimes(a, b+uint64(d))
	}
}

// Loop is the capture of the scripted agent's loop (testdata/mcp/loop): the base capture with an image digest on each
// service's resources (made-up, stable: Digest of the service's name), and its request count, the load generator's
// count of the entry service a for the capture's own window. Neither moves a number: digests are read but not modelled,
// and the count makes the kept share counted, 1.
func Loop(c *Capture) (*Capture, []byte) {
	forAllResources(c, func(rs *ResourceSpans) {
		svc := rs.Service()
		rs.SetAttr("container.image.repo_digests", Strs("registry.example/shop/"+svc+"@"+Digest(svc)))
	})
	var entries int64
	spansOf(c, svcA, otlp.KindServer, func(*ResourceSpans, *Span) { entries++ })
	return c, RequestCount(c, entries)
}
