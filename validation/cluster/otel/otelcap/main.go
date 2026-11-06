// SPDX-License-Identifier: Apache-2.0

// Command otelcap prepares and describes the trace capture of Phase B of the real-cluster run (the OpenTelemetry
// Demo; validation/cluster/otel/run.sh drives it).
//
//	otelcap cut -from T0 -to T1 -o CAPTURE [-nested-internal] [-drop-service NAME]... FILE.jsonl[.gz] ...
//	otelcap stats [-bucket 30] CAPTURE
//
// cut keeps the whole traces whose first span starts in [T0, T1) (Unix seconds, decimals allowed) and writes them
// to CAPTURE/traces/spans.jsonl.gz, one ExportTraceServiceRequest per line, as the Collector's file exporter wrote
// them: every field of a kept span, scope and resource is passed through unchanged. The Collector writes its file
// from its start, so the warm-up and an earlier window are in it too; a window cut by span start instead would
// leave the requests that straddle T0 without their first spans (broken traces), and a cut by trace leaves them
// out whole. A last line that ends without a newline is a write in progress at the moment the file was copied: it
// is dropped and counted as partial. Any other line that does not decode is counted as bad and dropped (the CLI
// would refuse the whole capture for it). It prints a JSON summary: the resource attributes the CLI reads
// (service.name, k8s.pod.name, service.instance.id, host.name) counted per service over the kept spans, and the
// kept spans with an error status, by service, kind and name, with their mean time (timeouts show there).
//
// Two optional changes make a diagnostic capture, never the capture itself; the summary counts what each changed:
//   - -nested-internal: two nested spans of one kind in one service and pod become one, by making the one away from
//     the other service INTERNAL (kind 1): a SERVER span whose nearest SERVER or CLIENT ancestor (walking up
//     through INTERNAL and PRODUCER spans, as the CLI walks) is a SERVER span of the same service and pod (the inner
//     one goes), and a CLIENT span with a CLIENT span of the same service and pod as that ancestor (the outer one
//     goes). Some frameworks open a second SERVER span inside the HTTP instrumentation's (Next.js does): the CLI
//     reads it as the service calling itself, a cycle. Some gRPC clients wrap the HTTP client's CLIENT span in a
//     CLIENT span of their own (.NET's Grpc.Net.Client does): the CLI reads two calls where one was made.
//   - -drop-service NAME (repeatable): the spans of that service.name are left out, so the calls to it have no
//     server span under them and the CLI reads them as calls to an outside peer (a delay with no queue). Meant for
//     a leaf service: a dropped span's children would lose their parent.
//
// stats reads CAPTURE with the CLI's own reader (internal/otlp: the same service and pod names, span kinds and
// dropped health probes) and prints a JSON description of what is in it: per service its spans by kind, pods and
// SERVER rate and mean time; where traces start; spans whose parent is not in the capture; PRODUCER and CONSUMER
// spans; the calls of each caller's visits to each callee, as the distribution of the number of calls per visit,
// with the calls' mean time; and the arrivals of each entry service per bucket of the window. It is descriptive:
// what the CLI accepts or refuses is decided by `rhobound infer` and `rhobound predict`, whose rules are finer (it
// leaves broken traces out of the call structure, for one). The call walk is the CLI's: from a span up through
// INTERNAL and PRODUCER spans to the nearest SERVER or CLIENT span; a CONSUMER span ends it (a consumer's work is
// not the producer's visit).
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "cut":
		err = runCut(os.Args[2:], os.Stdout)
	case "stats":
		err = runStats(os.Args[2:], os.Stdout)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "otelcap:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: otelcap cut -from T0 -to T1 -o CAPTURE [-nested-internal] [-drop-service NAME]... FILE.jsonl[.gz] ...\n"+
		"       otelcap stats [-bucket S] CAPTURE")
	os.Exit(2)
}

// ------------------------------------------------------------------------------------------------------------ cut

// nanos reads an OTLP/JSON time: a string of digits (proto3 JSON) or a number.
type nanos uint64

func (n *nanos) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 {
		return nil
	}
	v, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(string(b), 64)
		if ferr != nil {
			return err
		}
		v = uint64(f)
	}
	*n = nanos(v)
	return nil
}

type kv struct {
	Key   string `json:"key"`
	Value struct {
		StringValue string `json:"stringValue"`
	} `json:"value"`
}

type resource struct {
	Attributes []kv `json:"attributes"`
}

func (r *resource) get(key string) (string, bool) {
	for _, a := range r.Attributes {
		if a.Key == key {
			return a.Value.StringValue, true
		}
	}
	return "", false
}

// spanFields is what cut reads of a span.
type spanFields struct {
	TraceID  string `json:"traceId"`
	SpanID   string `json:"spanId"`
	ParentID string `json:"parentSpanId"`
	Name     string `json:"name"`
	Kind     int    `json:"kind"`
	Start    nanos  `json:"startTimeUnixNano"`
	End      nanos  `json:"endTimeUnixNano"`
	Status   struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

// isError reads an OTLP status code: 2, or the enum's name.
func (s *spanFields) isError() bool {
	c := strings.Trim(string(s.Status.Code), `"`)
	return c == "2" || c == "STATUS_CODE_ERROR"
}

type request struct {
	ResourceSpans []struct {
		Resource   resource `json:"resource"`
		ScopeSpans []struct {
			Spans []spanFields `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// ResourceCount is the resource attributes the CLI reads, counted over one service's kept spans.
type ResourceCount struct {
	Service        string   `json:"service"`
	Spans          int64    `json:"spans"`
	WithPodName    int64    `json:"with_k8s_pod_name"`
	WithInstanceID int64    `json:"with_service_instance_id"`
	WithHostName   int64    `json:"with_host_name"`
	WithDeployment int64    `json:"with_k8s_deployment_name"`
	WithoutService int64    `json:"without_service_name"`
	Pods           []string `json:"pods"`
	Nodes          []string `json:"nodes"`
	pods, nodes    map[string]bool
}

// ErrorCount is the kept spans of one service, kind and name, and how many of them have an error status.
type ErrorCount struct {
	Service string  `json:"service"`
	Kind    int     `json:"kind"`
	Name    string  `json:"name"`
	Spans   int64   `json:"spans"`
	Errors  int64   `json:"error_spans"`
	MeanMs  float64 `json:"mean_ms"`
	sumMs   float64
}

// CutSummary is what cut prints.
type CutSummary struct {
	FromNs           uint64           `json:"from_unix_ns"`
	ToNs             uint64           `json:"to_unix_ns"`
	Inputs           []string         `json:"inputs"`
	Output           string           `json:"output"`
	Lines            int64            `json:"lines"`
	LinesWritten     int64            `json:"lines_written"`
	PartialLines     int64            `json:"partial_lines"`
	BadLines         int64            `json:"bad_lines"`
	TracesSeen       int64            `json:"traces_seen"`
	TracesKept       int64            `json:"traces_kept"`
	TracesBefore     int64            `json:"traces_before"`
	TracesAfter      int64            `json:"traces_after"`
	SpansSeen        int64            `json:"spans_seen"`
	SpansKept        int64            `json:"spans_kept"`
	FirstKeptStart   uint64           `json:"first_kept_start_unix_ns"`
	LastKeptStart    uint64           `json:"last_kept_start_unix_ns"`
	DataStart        uint64           `json:"data_first_start_unix_ns"`
	DataLastStart    uint64           `json:"data_last_start_unix_ns"`
	NestedInternal   map[string]int64 `json:"nested_spans_made_internal,omitempty"`
	DroppedServices  map[string]int64 `json:"dropped_service_spans,omitempty"`
	DroppedLostChild int64            `json:"spans_whose_parent_was_dropped,omitempty"`
	Resources        []*ResourceCount `json:"resources"`
	Errors           []*ErrorCount    `json:"spans_with_errors"`
}

// parseUnix reads Unix seconds with an optional decimal part as nanoseconds, exactly.
func parseUnix(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || strings.HasPrefix(whole, "-") || strings.HasPrefix(whole, "+") {
		return 0, fmt.Errorf("%q is not Unix seconds", s)
	}
	w, err := strconv.ParseUint(whole, 10, 64)
	if err != nil || w > math.MaxUint64/1_000_000_000-1 {
		return 0, fmt.Errorf("%q is not Unix seconds", s)
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	var f uint64
	if frac != "" {
		f, err = strconv.ParseUint(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not Unix seconds", s)
		}
	}
	return w*1e9 + f, nil
}

// eachLine calls fn for every line of a plain or gzip file. last is true for a final line with no newline.
func eachLine(path string, fn func(line []byte, last bool) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		defer gz.Close()
		r = gz
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			unterminated := !bytes.HasSuffix(line, []byte{'\n'})
			if ferr := fn(line, unterminated); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// a gzip stream cut short (the copy of a file still being written) ends like a partial line
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("%s: %w", path, err)
		}
	}
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// cutOptions are the optional changes of a diagnostic capture.
type cutOptions struct {
	nestedInternal bool
	drop           map[string]bool
}

func runCut(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("cut", flag.ContinueOnError)
	from := fs.String("from", "", "window start, Unix seconds (decimals allowed)")
	to := fs.String("to", "", "window end, Unix seconds (decimals allowed)")
	out := fs.String("o", "", "capture folder to write (CAPTURE/traces/spans.jsonl.gz)")
	nested := fs.Bool("nested-internal", false, "make one of two nested SERVER (the inner) or CLIENT (the outer) spans of one service and pod INTERNAL")
	var drop multiFlag
	fs.Var(&drop, "drop-service", "leave out the spans of this service.name (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" || *to == "" || *out == "" || fs.NArg() == 0 {
		return errors.New("cut needs -from, -to, -o and at least one input file")
	}
	t0, err := parseUnix(*from)
	if err != nil {
		return err
	}
	t1, err := parseUnix(*to)
	if err != nil {
		return err
	}
	if t1 <= t0 {
		return fmt.Errorf("-to %s is not after -from %s", *to, *from)
	}
	opt := cutOptions{nestedInternal: *nested, drop: map[string]bool{}}
	for _, d := range drop {
		opt.drop[d] = true
	}
	sum, err := cut(t0, t1, *out, fs.Args(), opt)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sum)
}

type spanKey struct{ trace, span string }

type spanInfo struct {
	parent   string
	kind     int
	svc, pod string
}

// cut makes up to three passes over the inputs: the first span of every trace; the parents, kinds and resources of
// the kept spans (only for -nested-internal or -drop-service); then the kept lines.
func cut(t0, t1 uint64, out string, inputs []string, opt cutOptions) (*CutSummary, error) {
	sum := &CutSummary{FromNs: t0, ToNs: t1, Inputs: inputs}
	first := map[string]uint64{}
	for _, in := range inputs {
		err := eachLine(in, func(line []byte, last bool) error {
			sum.Lines++
			var req request
			if err := json.Unmarshal(line, &req); err != nil {
				if last {
					sum.PartialLines++
				} else {
					sum.BadLines++
				}
				return nil
			}
			for _, rs := range req.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					for _, sp := range ss.Spans {
						sum.SpansSeen++
						st := uint64(sp.Start)
						if st == 0 {
							continue
						}
						if sum.DataStart == 0 || st < sum.DataStart {
							sum.DataStart = st
						}
						if st > sum.DataLastStart {
							sum.DataLastStart = st
						}
						if v, ok := first[sp.TraceID]; !ok || st < v {
							first[sp.TraceID] = st
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	keep := map[string]bool{}
	for id, st := range first {
		sum.TracesSeen++
		switch {
		case st < t0:
			sum.TracesBefore++
		case st >= t1:
			sum.TracesAfter++
		default:
			keep[id] = true
			sum.TracesKept++
			if sum.FirstKeptStart == 0 || st < sum.FirstKeptStart {
				sum.FirstKeptStart = st
			}
			if st > sum.LastKeptStart {
				sum.LastKeptStart = st
			}
		}
	}

	nested := map[spanKey]bool{}
	dropped := map[spanKey]bool{}
	if opt.nestedInternal || len(opt.drop) > 0 {
		info := map[spanKey]spanInfo{}
		for _, in := range inputs {
			err := eachLine(in, func(line []byte, last bool) error {
				var req request
				if json.Unmarshal(line, &req) != nil {
					return nil
				}
				for _, rs := range req.ResourceSpans {
					svc, _ := rs.Resource.get("service.name")
					pod, _ := rs.Resource.get("k8s.pod.name")
					for _, ss := range rs.ScopeSpans {
						for _, sp := range ss.Spans {
							if keep[sp.TraceID] {
								k := spanKey{sp.TraceID, sp.SpanID}
								info[k] = spanInfo{sp.ParentID, sp.Kind, svc, pod}
								if opt.drop[svc] {
									dropped[k] = true
								}
							}
						}
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		for k, si := range info {
			if dropped[k] {
				continue
			}
			if si.parent != "" && dropped[spanKey{k.trace, si.parent}] {
				sum.DroppedLostChild++
			}
			if !opt.nestedInternal || (si.kind != otlp.KindServer && si.kind != otlp.KindClient) {
				continue
			}
			// up through INTERNAL and PRODUCER spans to the nearest SERVER, CLIENT or CONSUMER span
			cur := si
			for hop := 0; hop < len(info) && cur.parent != ""; hop++ {
				p, ok := info[spanKey{k.trace, cur.parent}]
				if !ok {
					break
				}
				if p.kind == otlp.KindServer || p.kind == otlp.KindClient || p.kind == otlp.KindConsumer {
					if p.kind == si.kind && p.svc == si.svc && p.pod == si.pod {
						if si.kind == otlp.KindServer {
							nested[k] = true // the inner SERVER span
						} else {
							nested[spanKey{k.trace, cur.parent}] = true // the outer CLIENT span
						}
					}
					break
				}
				cur = p
			}
		}
	}

	dir := filepath.Join(out, "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sum.Output = filepath.Join(dir, "spans.jsonl.gz")
	f, err := os.Create(sum.Output)
	if err != nil {
		return nil, err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	w := bufio.NewWriterSize(gz, 1<<20)
	lw := &lineWriter{keep: keep, nested: nested, drop: opt.drop, res: map[string]*ResourceCount{},
		errs: map[string]*ErrorCount{}, sum: sum}
	lw.enc = json.NewEncoder(&lw.buf)
	lw.enc.SetEscapeHTML(false)
	for _, in := range inputs {
		err := eachLine(in, func(line []byte, last bool) error {
			kept, err := lw.filter(line)
			if err != nil || kept == nil { // undecodable lines were counted in the first pass
				return nil
			}
			sum.LinesWritten++
			_, werr := w.Write(kept)
			return werr
		})
		if err != nil {
			f.Close()
			return nil, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	for _, r := range lw.res {
		r.Pods = sortedKeys(r.pods, 50)
		r.Nodes = sortedKeys(r.nodes, 50)
		sum.Resources = append(sum.Resources, r)
	}
	sort.Slice(sum.Resources, func(i, j int) bool { return sum.Resources[i].Service < sum.Resources[j].Service })
	sum.Errors = []*ErrorCount{}
	for _, e := range lw.errs {
		if e.Errors > 0 {
			e.MeanMs = e.sumMs / float64(e.Spans)
			sum.Errors = append(sum.Errors, e)
		}
	}
	sort.Slice(sum.Errors, func(i, j int) bool {
		if sum.Errors[i].Errors != sum.Errors[j].Errors {
			return sum.Errors[i].Errors > sum.Errors[j].Errors
		}
		return sum.Errors[i].Service+sum.Errors[i].Name < sum.Errors[j].Service+sum.Errors[j].Name
	})
	return sum, nil
}

// lineWriter filters the lines of the last pass.
type lineWriter struct {
	keep   map[string]bool
	nested map[spanKey]bool
	drop   map[string]bool
	res    map[string]*ResourceCount
	errs   map[string]*ErrorCount
	sum    *CutSummary
	enc    *json.Encoder
	buf    bytes.Buffer
}

// filter keeps the spans of kept traces (less a dropped service's; nested SERVER spans made INTERNAL), and the
// scopes and resources that still hold one; nil when none is left. Every other field is passed through as it was.
func (lw *lineWriter) filter(line []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(line, &top); err != nil {
		return nil, err
	}
	var rss []map[string]json.RawMessage
	if raw, ok := top["resourceSpans"]; ok {
		if err := json.Unmarshal(raw, &rss); err != nil {
			return nil, err
		}
	}
	var outRS []map[string]json.RawMessage
	for _, rs := range rss {
		var res resource
		_ = json.Unmarshal(rs["resource"], &res)
		svc, hasSvc := res.get("service.name")
		var sss []map[string]json.RawMessage
		if raw, ok := rs["scopeSpans"]; ok {
			if err := json.Unmarshal(raw, &sss); err != nil {
				return nil, err
			}
		}
		var outSS []map[string]json.RawMessage
		var rsKept int64
		for _, ss := range sss {
			var spans []json.RawMessage
			if raw, ok := ss["spans"]; ok {
				if err := json.Unmarshal(raw, &spans); err != nil {
					return nil, err
				}
			}
			var kept []json.RawMessage
			for _, raw := range spans {
				var sp spanFields
				if err := json.Unmarshal(raw, &sp); err != nil {
					return nil, err
				}
				if !lw.keep[sp.TraceID] {
					continue
				}
				if lw.drop[svc] {
					if lw.sum.DroppedServices == nil {
						lw.sum.DroppedServices = map[string]int64{}
					}
					lw.sum.DroppedServices[svc]++
					continue
				}
				if lw.nested[spanKey{sp.TraceID, sp.SpanID}] {
					var m map[string]json.RawMessage
					if err := json.Unmarshal(raw, &m); err != nil {
						return nil, err
					}
					m["kind"] = json.RawMessage(strconv.Itoa(otlp.KindInternal))
					b, err := lw.marshal(m)
					if err != nil {
						return nil, err
					}
					raw = b
					sp.Kind = otlp.KindInternal
					if lw.sum.NestedInternal == nil {
						lw.sum.NestedInternal = map[string]int64{}
					}
					lw.sum.NestedInternal[svc]++
				}
				lw.countSpan(svc, &sp)
				kept = append(kept, raw)
			}
			if len(kept) == 0 {
				continue
			}
			raw, err := lw.marshal(kept)
			if err != nil {
				return nil, err
			}
			ss["spans"] = raw
			outSS = append(outSS, ss)
			rsKept += int64(len(kept))
		}
		if len(outSS) == 0 {
			continue
		}
		raw, err := lw.marshal(outSS)
		if err != nil {
			return nil, err
		}
		rs["scopeSpans"] = raw
		outRS = append(outRS, rs)
		lw.sum.SpansKept += rsKept
		lw.countResource(&res, svc, hasSvc, rsKept)
	}
	if len(outRS) == 0 {
		return nil, nil
	}
	raw, err := lw.marshal(outRS)
	if err != nil {
		return nil, err
	}
	top["resourceSpans"] = raw
	b, err := lw.marshal(top)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// marshal encodes v without HTML escaping (the Encoder's newline cut off), a copy the caller may keep.
func (lw *lineWriter) marshal(v any) (json.RawMessage, error) {
	lw.buf.Reset()
	if err := lw.enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.Clone(bytes.TrimRight(lw.buf.Bytes(), "\n"))), nil
}

func (lw *lineWriter) countSpan(svc string, sp *spanFields) {
	key := fmt.Sprintf("%s\x00%d\x00%s", svc, sp.Kind, sp.Name)
	e := lw.errs[key]
	if e == nil {
		e = &ErrorCount{Service: svc, Kind: sp.Kind, Name: sp.Name}
		lw.errs[key] = e
	}
	e.Spans++
	if sp.End >= sp.Start {
		e.sumMs += float64(sp.End-sp.Start) / 1e6
	}
	if sp.isError() {
		e.Errors++
	}
}

func (lw *lineWriter) countResource(r *resource, svc string, hasSvc bool, spans int64) {
	if !hasSvc || svc == "" {
		svc = "(no service.name)"
	}
	c := lw.res[svc]
	if c == nil {
		c = &ResourceCount{Service: svc, pods: map[string]bool{}, nodes: map[string]bool{}}
		lw.res[svc] = c
	}
	c.Spans += spans
	if !hasSvc {
		c.WithoutService += spans
	}
	if v, ok := r.get("k8s.pod.name"); ok && v != "" {
		c.WithPodName += spans
		c.pods[v] = true
	}
	if v, ok := r.get("service.instance.id"); ok && v != "" {
		c.WithInstanceID += spans
	}
	if v, ok := r.get("host.name"); ok && v != "" {
		c.WithHostName += spans
	}
	if v, ok := r.get("k8s.deployment.name"); ok && v != "" {
		c.WithDeployment += spans
	}
	if v, ok := r.get("k8s.node.name"); ok && v != "" {
		c.nodes[v] = true
	}
}

func sortedKeys(m map[string]bool, max int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > max {
		out = append(out[:max], fmt.Sprintf("... %d more", len(m)-max))
	}
	return out
}

// ---------------------------------------------------------------------------------------------------------- stats

// Stats is what stats prints.
type Stats struct {
	Capture       string          `json:"capture"`
	Files         int             `json:"files"`
	Spans         int             `json:"spans"`
	Traces        int             `json:"traces"`
	Skipped       int             `json:"skipped_spans"`
	Probes        int             `json:"probe_spans_dropped"`
	ProbeChildren int             `json:"probe_children"`
	WindowS       float64         `json:"window_s"`
	Services      []*ServiceStats `json:"services"`
	Roots         []Group         `json:"roots"`
	LostParents   []Group         `json:"spans_with_parent_not_in_capture"`
	Edges         []*Edge         `json:"edges"`
	Entries       []*Entry        `json:"entries"`
	BucketS       float64         `json:"bucket_s"`
}

// ServiceStats describes one service.name's spans.
type ServiceStats struct {
	Name         string           `json:"name"`
	Spans        int64            `json:"spans"`
	Kinds        map[string]int64 `json:"kinds"`
	Pods         int              `json:"pods"`
	PodNames     []string         `json:"pod_names"`
	Images       []string         `json:"images,omitempty"`
	ServerRPS    float64          `json:"server_rps"`
	ServerMeanMs float64          `json:"server_mean_ms"`
	ServerMaxMs  float64          `json:"server_max_ms"`
	Consumers    int64            `json:"consumer_spans_parent_in_capture"`
	ConsumerTops int64            `json:"consumer_spans_parent_absent"`
	EntryVisits  int64            `json:"entry_visits"`
	srvSum       float64
	pods         map[string]bool
	images       map[string]bool
}

// Group counts spans by service and kind.
type Group struct {
	Service string `json:"service"`
	Kind    string `json:"kind"`
	Spans   int64  `json:"spans"`
}

// Edge is the calls of a caller's visits to one callee: a service, or "peer:NAME" for a CLIENT span with no
// SERVER span under it ("peer:?" with no peer attribute).
type Edge struct {
	Caller        string            `json:"caller"`
	Callee        string            `json:"callee"`
	Calls         int64             `json:"calls"`
	CallerVisits  int64             `json:"caller_visits"`
	VisitsCalling float64           `json:"share_of_visits_calling"`
	PerVisit      map[string]string `json:"calls_per_visit"` // "n": share of the caller's visits, the 6 most common
	Shape         string            `json:"shape"`           // fixed | fixed+1 | varies (99% of visits)
	MeanMs        float64           `json:"call_mean_ms"`    // the client span's time (no client span: the server span's)
	hist          map[int64]int64
	sumMs         float64
}

// Entry is a service's visits with no caller's visit in the capture, per bucket of the window.
type Entry struct {
	Service string  `json:"service"`
	Visits  int64   `json:"visits"`
	Rate    float64 `json:"rate_per_s"`
	Buckets []int64 `json:"per_bucket"`
	Mean    float64 `json:"bucket_mean"`
	CV      float64 `json:"bucket_cv"`
	Poisson float64 `json:"poisson_cv"` // the CV a Poisson stream of that mean would show
}

var kindName = map[uint8]string{0: "unspecified", otlp.KindInternal: "internal", otlp.KindServer: "server",
	otlp.KindClient: "client", otlp.KindProducer: "producer", otlp.KindConsumer: "consumer"}

func runStats(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	bucket := fs.Float64("bucket", 30, "seconds per bucket of the entry arrivals")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *bucket <= 0 {
		return errors.New("stats needs one CAPTURE and a positive -bucket")
	}
	d, err := otlp.ReadDir(fs.Arg(0))
	if err != nil {
		return err
	}
	st := describe(d, *bucket)
	st.Capture = fs.Arg(0)
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(st)
}

// ancestor walks up from span k through INTERNAL and PRODUCER spans to the nearest SERVER or CLIENT span, as the
// CLI does: -1 at a root, a parent not in the capture, or a CONSUMER span (consumer true).
func ancestor(g []otlp.Span, pos map[uint64]int, k int) (p int, consumer bool) {
	cur := k
	for hop := 0; hop < len(g); hop++ {
		pid := g[cur].Parent
		if pid == 0 {
			return -1, false
		}
		q, ok := pos[pid]
		if !ok {
			return -1, false
		}
		switch g[q].Kind {
		case otlp.KindServer, otlp.KindClient:
			return q, false
		case otlp.KindConsumer:
			return -1, true
		}
		cur = q
	}
	return -1, false
}

// owner is the SERVER span whose visit span k belongs to, or -1.
func owner(g []otlp.Span, pos map[uint64]int, k int) int {
	cur := k
	for hop := 0; hop < len(g); hop++ {
		p, _ := ancestor(g, pos, cur)
		if p < 0 {
			return -1
		}
		if g[p].Kind == otlp.KindServer {
			return p
		}
		cur = p
	}
	return -1
}

func describe(d *otlp.Data, bucket float64) *Stats {
	st := &Stats{Files: len(d.Files), Spans: len(d.Spans), Skipped: d.Skipped, Probes: d.Probes,
		ProbeChildren: d.ProbeChildren, BucketS: bucket}
	if len(d.Spans) == 0 {
		return st
	}
	svcs := map[string]*ServiceStats{}
	svcOf := func(sp *otlp.Span) *ServiceStats {
		r := d.Resources[sp.Res]
		s := svcs[r.Service]
		if s == nil {
			s = &ServiceStats{Name: r.Service, Kinds: map[string]int64{}, pods: map[string]bool{}, images: map[string]bool{}}
			svcs[r.Service] = s
		}
		return s
	}
	var t0, t1 uint64
	for i := range d.Spans {
		sp := &d.Spans[i]
		if t0 == 0 || sp.Start < t0 {
			t0 = sp.Start
		}
		if sp.End > t1 {
			t1 = sp.End
		}
		s := svcOf(sp)
		s.Spans++
		s.Kinds[kindName[sp.Kind]]++
		r := d.Resources[sp.Res]
		if r.Pod != "" {
			s.pods[r.Pod] = true
		}
		if r.Image != "" {
			s.images[r.Image] = true
		}
		if sp.Kind == otlp.KindServer {
			s.srvSum += sp.Dur()
			if sp.Dur() > s.ServerMaxMs {
				s.ServerMaxMs = sp.Dur()
			}
		}
	}
	st.WindowS = float64(t1-t0) / 1e9

	// group the spans by trace (index order within a trace is capture order)
	byTrace := map[[2]uint64][]int{}
	for i := range d.Spans {
		byTrace[d.Spans[i].Trace] = append(byTrace[d.Spans[i].Trace], i)
	}
	st.Traces = len(byTrace)
	roots := map[[2]string]int64{}
	lost := map[[2]string]int64{}
	edges := map[[2]string]*Edge{}
	visits := map[string]int64{} // SERVER spans per service: the caller's visits
	entryAt := map[string][]uint64{}
	for _, idx := range byTrace {
		g := make([]otlp.Span, len(idx))
		for j, i := range idx {
			g[j] = d.Spans[i]
		}
		pos := make(map[uint64]int, len(g))
		for j := range g {
			pos[g[j].ID] = j
		}
		name := func(k int) string { return d.Resources[g[k].Res].Service }
		calls := map[int]map[string]int64{} // owner visit -> callee -> calls
		callMs := map[[2]string]float64{}   // caller, callee -> the calls' time in this trace
		add := func(own int, callee string, ms float64) {
			m := calls[own]
			if m == nil {
				m = map[string]int64{}
				calls[own] = m
			}
			m[callee]++
			callMs[[2]string{name(own), callee}] += ms
		}
		matched := map[int]bool{}
		for k := range g {
			sp := &g[k]
			key := [2]string{name(k), kindName[sp.Kind]}
			if sp.Parent == 0 {
				roots[key]++
			} else if _, ok := pos[sp.Parent]; !ok {
				lost[key]++
			}
			if sp.Kind == otlp.KindConsumer {
				s := svcs[name(k)]
				if _, ok := pos[sp.Parent]; ok && sp.Parent != 0 {
					s.Consumers++
				} else {
					s.ConsumerTops++
				}
			}
			if sp.Kind != otlp.KindServer {
				continue
			}
			visits[name(k)]++
			p, consumer := ancestor(g, pos, k)
			own, ms := -1, sp.Dur()
			if p >= 0 && g[p].Kind == otlp.KindClient {
				matched[p] = true
				own, ms = owner(g, pos, p), g[p].Dur()
			} else if p >= 0 {
				own = p
			}
			if own >= 0 {
				add(own, name(k), ms)
			} else if !consumer {
				svcs[name(k)].EntryVisits++
				entryAt[name(k)] = append(entryAt[name(k)], sp.Start)
			}
		}
		for k := range g {
			if g[k].Kind != otlp.KindClient || matched[k] {
				continue
			}
			own := owner(g, pos, k)
			if own < 0 {
				continue
			}
			peer := "peer:?"
			if g[k].Peer >= 0 {
				peer = "peer:" + d.Peers[g[k].Peer]
			}
			add(own, peer, g[k].Dur())
		}
		for own, m := range calls {
			caller := name(own)
			for callee, n := range m {
				e := edges[[2]string{caller, callee}]
				if e == nil {
					e = &Edge{Caller: caller, Callee: callee, hist: map[int64]int64{}}
					edges[[2]string{caller, callee}] = e
				}
				e.Calls += n
				e.hist[n]++
			}
		}
		for k, ms := range callMs {
			edges[k].sumMs += ms
		}
	}

	for _, s := range svcs {
		s.Pods = len(s.pods)
		s.PodNames = sortedKeys(s.pods, 20)
		s.Images = sortedKeys(s.images, 5)
		if n := s.Kinds["server"]; n > 0 {
			s.ServerMeanMs = s.srvSum / float64(n)
			if st.WindowS > 0 {
				s.ServerRPS = float64(n) / st.WindowS
			}
		}
		st.Services = append(st.Services, s)
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].Name < st.Services[j].Name })
	st.Roots = groups(roots)
	st.LostParents = groups(lost)
	for _, e := range edges {
		e.CallerVisits = visits[e.Caller]
		var with int64
		for _, c := range e.hist {
			with += c
		}
		if zero := e.CallerVisits - with; zero > 0 {
			e.hist[0] += zero
		}
		if e.CallerVisits > 0 {
			e.VisitsCalling = float64(with) / float64(e.CallerVisits)
		}
		e.PerVisit, e.Shape = shape(e.hist, e.CallerVisits)
		if e.Calls > 0 {
			e.MeanMs = e.sumMs / float64(e.Calls)
		}
		st.Edges = append(st.Edges, e)
	}
	sort.Slice(st.Edges, func(i, j int) bool {
		if st.Edges[i].Caller != st.Edges[j].Caller {
			return st.Edges[i].Caller < st.Edges[j].Caller
		}
		return st.Edges[i].Callee < st.Edges[j].Callee
	})
	for svc, ts := range entryAt {
		st.Entries = append(st.Entries, entry(svc, ts, t0, t1, bucket))
	}
	sort.Slice(st.Entries, func(i, j int) bool { return st.Entries[i].Visits > st.Entries[j].Visits })
	return st
}

func groups(m map[[2]string]int64) []Group {
	out := make([]Group, 0, len(m))
	for k, n := range m {
		out = append(out, Group{Service: k[0], Kind: k[1], Spans: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Spans != out[j].Spans {
			return out[i].Spans > out[j].Spans
		}
		return out[i].Service+out[i].Kind < out[j].Service+out[j].Kind
	})
	return out
}

// shape gives the 6 most common calls-per-visit counts with their share of the visits, and whether one count
// (fixed) or two neighbouring counts (fixed+1) cover 99% of them.
func shape(hist map[int64]int64, visits int64) (map[string]string, string) {
	type kc struct{ k, c int64 }
	var all []kc
	for k, c := range hist {
		all = append(all, kc{k, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].c != all[j].c {
			return all[i].c > all[j].c
		}
		return all[i].k < all[j].k
	})
	out := map[string]string{}
	for i, x := range all {
		if i == 6 {
			out["other"] = fmt.Sprintf("%d counts", len(all)-6)
			break
		}
		out[strconv.FormatInt(x.k, 10)] = fmt.Sprintf("%.1f%%", 100*float64(x.c)/float64(max(visits, 1)))
	}
	if visits == 0 || len(all) == 0 {
		return out, "varies"
	}
	need := 0.99 * float64(visits)
	if float64(all[0].c) >= need {
		return out, "fixed"
	}
	for _, x := range all {
		if x.k == all[0].k+1 || x.k == all[0].k-1 {
			if float64(all[0].c+x.c) >= need {
				return out, "fixed+1"
			}
		}
	}
	return out, "varies"
}

func entry(svc string, ts []uint64, t0, t1 uint64, bucket float64) *Entry {
	e := &Entry{Service: svc, Visits: int64(len(ts))}
	span := float64(t1-t0) / 1e9
	if span > 0 {
		e.Rate = float64(len(ts)) / span
	}
	nb := int(span / bucket) // whole buckets only: a part bucket at the end would read low
	if nb < 1 {
		return e
	}
	e.Buckets = make([]int64, nb)
	for _, t := range ts {
		b := int(float64(t-t0) / 1e9 / bucket)
		if b >= 0 && b < nb {
			e.Buckets[b]++
		}
	}
	var sum, sq float64
	for _, c := range e.Buckets {
		sum += float64(c)
	}
	e.Mean = sum / float64(nb)
	for _, c := range e.Buckets {
		sq += (float64(c) - e.Mean) * (float64(c) - e.Mean)
	}
	if nb > 1 && e.Mean > 0 {
		e.CV = math.Sqrt(sq/float64(nb-1)) / e.Mean
		e.Poisson = 1 / math.Sqrt(e.Mean)
	}
	return e
}
