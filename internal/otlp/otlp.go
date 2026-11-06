// SPDX-License-Identifier: Apache-2.0

// Package otlp reads OpenTelemetry traces in OTLP/JSON (one
// ExportTraceServiceRequest per line, as the Collector's file exporter writes
// them), plain or gzip-compressed, and keeps only what the model needs.
package otlp

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// Span kinds (OTLP SpanKind values).
const (
	KindInternal = 1
	KindServer   = 2
	KindClient   = 3
	KindProducer = 4
	KindConsumer = 5
)

// Span is one span reduced to the fields the model uses. Times are Unix
// nanoseconds.
type Span struct {
	Trace  [2]uint64
	ID     uint64
	Parent uint64 // 0 when the span has no parent
	Start  uint64
	End    uint64
	Res    int32 // index into Data.Resources
	Peer   int32 // index into Data.Peers for client spans, else -1
	Addr   int32 // index into Data.Addrs (the peer and its port) for client spans with a peer, else -1
	Scope  int32 // 1 + the index into Data.Scopes of the instrumentation scope that emitted the span; 0 when it names none
	Kind   uint8
	// Route is 1 + the index into Data.Routes of a SERVER span's route (RouteOf), 0 when it has none. It sits in the
	// padding after Kind, so a Span stays 72 bytes.
	Route uint16
	Ext   int32 // 1 + the index into Data.Exts of the span's other fields; 0 when it carries none of them
}

// Dur returns the span's duration in milliseconds.
func (s *Span) Dur() float64 {
	if s.End < s.Start {
		return 0
	}
	return float64(s.End-s.Start) / 1e6
}

// Resource identifies who emitted a span.
type Resource struct {
	Service string // service.name, else k8s.deployment.name
	Pod     string // k8s.pod.name, else k8s.pod.uid, else an ECS task's id (taskID), else service.instance.id, else host.name
	Image   string // container.image.name[:tag]
}

// ResourceInfo is what a resource says besides its Resource: everything the reader reads from it that the model does
// not use. Several resources of the capture can map to one Resource (the same service, pod and image); their
// ResourceInfo is then the union: multi-valued fields list every value seen, in the order read, and single-valued
// fields keep the first value seen.
type ResourceInfo struct {
	Namespace      string   // service.namespace
	ServiceVersion string   // service.version
	Environments   []string // deployment.environment.name, else deployment.environment
	PodName        string   // k8s.pod.name
	PodUID         string   // k8s.pod.uid
	TaskARN        string   // aws.ecs.task.arn
	InstanceIDs    []string // service.instance.id
	Hosts          []string // host.name
	HostArch       string   // host.arch
	K8sNamespace   string   // k8s.namespace.name
	Node           string   // k8s.node.name
	Deployment     string   // k8s.deployment.name
	ContainerName  string   // k8s.container.name
	ContainerID    string   // container.id
	ImageName      string   // container.image.name
	ImageTag       string   // container.image.tag (singular; not in the current semantic conventions, read for compatibility)
	ImageTags      []string // container.image.tags
	RepoDigests    []string // container.image.repo_digests
	SDKLanguage    string   // telemetry.sdk.language
	SDKVersion     string   // telemetry.sdk.version
	FaaS           bool     // a faas.* key: the resource is a function platform's
	// ServiceNameNotString is true when service.name is present with a value that is not a string.
	ServiceNameNotString bool
}

// PlatformIdentity reports whether the resource names its pod or task from the platform (a pod name or uid, or an ECS
// task), not only a process (service.instance.id) or a host.
func (ri *ResourceInfo) PlatformIdentity() bool {
	return ri.PodName != "" || ri.PodUID != "" || ri.TaskARN != ""
}

// SpanExt holds a span's fields that the model does not use: the ones the data contract checks. Spans that carry none
// of them have Ext 0. Equal values are stored once (Data.Exts), so memory stays flat.
type SpanExt struct {
	Route      string // http.route
	RPCMethod  string // rpc.method
	TH         string // the sampling threshold ot=th: in the traceState of a SERVER span ("" when there is none)
	Status     uint8  // status.code: 0 unset, 1 ok, 2 error
	HTTPStatus uint16 // http.response.status_code
	Resends    uint16 // http.request.resend_count (at most 65535)
	UserAgent  uint8  // the user_agent.original of a probe (UAKubeProbe, UAELBHealthChecker, UAPrometheus), else 0
	ProbePath  bool   // a SERVER span whose route matches the wider probe rule (WideProbe), beyond IsProbe's
	MsgSent    uint16 // rpc.message events of type SENT on a SERVER span (at most 65535)
	MsgRecv    uint16 // ... of type RECEIVED
	Links      uint16 // links of a CONSUMER span (at most 65535)
	Dropped    bool   // droppedAttributesCount above 0
	Personal   bool   // a kept value looks like personal data (PersonalKind)
}

// The user agents of probes (DATA.md 3.5).
const (
	UAKubeProbe        = 1
	UAELBHealthChecker = 2
	UAPrometheus       = 3
)

// LongSpan is the duration, in nanoseconds, from which a SERVER span's name is kept (Data.LongNames).
const LongSpan = 1_000_000_000

// maxLongNames bounds Data.LongNames: a capture of spans that all last seconds keeps this many names.
const maxLongNames = 4096

// SpanKey names a span across the capture: its trace and its id.
type SpanKey struct {
	Trace [2]uint64
	ID    uint64
}

// Data is everything read from one capture.
type Data struct {
	Spans     []Span
	Resources []Resource
	Peers     []string
	Addrs     []string // "peer:port" as client spans name it (the port when given): where a call went
	Scopes    []string // instrumentation scope names (Span.Scope - 1 indexes it)
	// LongNames holds the names of the SERVER spans that lasted LongSpan or more (at most maxLongNames of them, the
	// first read), to name them in a report. Span names are not kept for the others.
	LongNames map[SpanKey]string
	Skipped   int // spans dropped for a missing id, service, start or end
	Probes    int // health and readiness probe spans dropped (see IsProbe)
	// ProbeChildren are spans whose parent was a dropped probe; they are
	// roots now (Parent 0), not spans with a lost parent.
	ProbeChildren int
	Files         []string

	// Info is what each resource says besides its Resource (Info[i] belongs to Resources[i]). Data built by hand may
	// leave it short; InfoOf then returns an empty one.
	Info []ResourceInfo
	// Exts are the spans' other fields (Span.Ext - 1 indexes it).
	Exts []SpanExt
	// Routes are the SERVER spans' routes (Span.Route - 1 indexes it), at most MaxRoutes of them: the spans of a route
	// read after that many have none.
	Routes []string
	// Stats count what the reader saw and normalized.
	Stats ReadStats
	// Ignored counts what the input carried that nothing reads, by what it was.
	Ignored map[string]int64
	// DroppedDuplicates is filled by the data contract's check, not by the reader: the spans it removed as identical
	// copies of a span already read, with the number of copies.
	DroppedDuplicates map[SpanKey]int
}

// InfoOf returns the ResourceInfo of resource i, or an empty one.
func (d *Data) InfoOf(i int32) *ResourceInfo {
	if int(i) < len(d.Info) && i >= 0 {
		return &d.Info[i]
	}
	return &ResourceInfo{}
}

// RouteName returns a span's route, or "" when it has none.
func (d *Data) RouteName(sp *Span) string {
	if sp.Route > 0 && int(sp.Route) <= len(d.Routes) {
		return d.Routes[sp.Route-1]
	}
	return ""
}

// ExtOf returns a span's other fields, or the zero SpanExt when it carries none.
func (d *Data) ExtOf(sp *Span) SpanExt {
	if sp.Ext > 0 && int(sp.Ext) <= len(d.Exts) {
		return d.Exts[sp.Ext-1]
	}
	return SpanExt{}
}

// ReadStats count what the reader saw, skipped and normalized.
type ReadStats struct {
	Requests  int64 // request values read
	SpansRead int64 // every span of the input: kept, skipped and probes
	IDsHex    int64 // spans whose ids are OTLP/JSON hex
	// SkippedIDs are spans skipped for an id that is not hex of the right length; IDsBase64 of them had base64 ids
	// (24 characters for a trace, 12 for a span), TraceIDs64 a 16-digit (64-bit) trace id.
	SkippedIDs, IDsBase64, TraceIDs64 int
	SkippedService                    int   // spans skipped for a resource with no service name
	SkippedTime                       int   // spans skipped for a missing start, a missing end, or an end before the start
	BadParents                        int   // non-empty parent ids that are not 16 hex digits, read as no parent
	Base64Parents                     int   // ... of which base64
	FloatTimes                        int64 // span times written as a float or with an exponent, read as integers
	PartialTail                       int   // the unterminated, undecodable last line of the last file, dropped
	// SkippedExamples are the ids of the first skipped spans (at most contract.MaxExamples), trace id and span id
	// joined by ":", each as written when it is written as an id, else "other" (skippedID).
	SkippedExamples []string
	// PersonalExamples are the trace ids of the first spans with a value that looks like personal data.
	PersonalExamples []string
}

// ---------------------------------------------------------------- limits and errors

// FileRef is one file to read and its expected sha256 (lower-case hex; "" when it is not checked).
type FileRef struct{ Path, SHA256 string }

// Limits bound what one capture may take to read: inflated bytes per object (file) and per capture, the length of a
// line (or of one value of a .json file), the number of spans, and the length of an attribute value the reader keeps
// (a resource attribute, a peer and its port, a route). A field of 0 is no limit.
type Limits struct {
	MaxObjectBytes, MaxCaptureBytes, MaxLineBytes int64
	MaxSpans, MaxAttrBytes                        int
}

// DefaultLimits are the limits of DATA.md 3.2 [D]: 1 GiB per object, 8 GiB per capture, 64 MiB per line, 20 million
// spans, 4 KiB per kept attribute value.
var DefaultLimits = Limits{1 << 30, 8 << 30, 64 << 20, 20_000_000, 4096}

// LimitError stops a read that passes one of the Limits. Code is contract.InputTooLarge; What names what passed which
// limit.
type LimitError struct {
	Code, What string
	Limit      int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("the input is too large: %s passes the limit of %d", e.What, e.Limit)
}

// Error is a refusal of the input by the reader, with the contract code that names it and where it happened. Its text
// is the reader's message as rhobound-cli printed it.
type Error struct {
	Code  string // contract.TraceBadJSON, contract.StoreCompressionMismatch or contract.StoreManifestMismatch
	Path  string // the file
	Line  int64  // the 1-based line of a .jsonl file, 0 when the error is not on a line
	Value int64  // the 0-based index of the value in a .json file, -1 when the error is not on a value
	Err   error
	// inValue: an undecodable request ("bad OTLP/JSON: ..."); otherwise an error of the file ("<path>: ...").
	inValue bool
}

func (e *Error) Error() string {
	if e.inValue {
		return "bad OTLP/JSON: " + e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// ---------------------------------------------------------------- JSON shapes

type nanos struct {
	v     uint64
	float bool // written as a float or with an exponent
}

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
		n.float = true
	}
	n.v = v
	return nil
}

type anyValue struct {
	StringValue string          `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"` // a number, or (proto3 JSON) a string of one
	// The other kinds are read only to tell that a value is not a string, and for arrays of strings.
	BoolValue   json.RawMessage `json:"boolValue"`
	DoubleValue json.RawMessage `json:"doubleValue"`
	ArrayValue  json.RawMessage `json:"arrayValue"`
	KvlistValue json.RawMessage `json:"kvlistValue"`
	BytesValue  json.RawMessage `json:"bytesValue"`
}

// text is the value as text: a string, or an integer's digits.
func (v anyValue) text() string {
	if v.StringValue != "" || len(v.IntValue) == 0 {
		return v.StringValue
	}
	return strings.Trim(string(v.IntValue), `"`)
}

// notString reports whether the value is present as another kind than a string.
func (v anyValue) notString() bool {
	return v.StringValue == "" && (len(v.IntValue) > 0 || len(v.BoolValue) > 0 || len(v.DoubleValue) > 0 ||
		len(v.ArrayValue) > 0 || len(v.KvlistValue) > 0 || len(v.BytesValue) > 0)
}

// strings returns the strings of an array value, or the string value as one; values of other kinds are left out.
func (v anyValue) strings() []string {
	if len(v.ArrayValue) == 0 {
		if v.StringValue != "" {
			return []string{v.StringValue}
		}
		return nil
	}
	var arr struct {
		Values []json.RawMessage `json:"values"`
	}
	if json.Unmarshal(v.ArrayValue, &arr) != nil {
		return nil
	}
	var out []string
	for _, raw := range arr.Values {
		var e struct {
			StringValue string `json:"stringValue"`
		}
		if json.Unmarshal(raw, &e) == nil && e.StringValue != "" {
			out = append(out, e.StringValue)
		}
	}
	return out
}

type keyValue struct {
	Key   string   `json:"key"`
	Value anyValue `json:"value"`
}

type jsonSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	ParentSpanID string     `json:"parentSpanId"`
	Name         string     `json:"name"`
	Kind         int        `json:"kind"`
	Start        nanos      `json:"startTimeUnixNano"`
	End          nanos      `json:"endTimeUnixNano"`
	Attributes   []keyValue `json:"attributes"`
	// The fields below were not read by rhobound-cli. They are kept raw and read leniently, so that a value of an
	// unexpected type is counted as ignored instead of refusing a capture that was read before.
	TraceState             json.RawMessage `json:"traceState"`
	Status                 json.RawMessage `json:"status"`
	Events                 json.RawMessage `json:"events"`
	Links                  json.RawMessage `json:"links"`
	Flags                  json.RawMessage `json:"flags"`
	DroppedAttributesCount json.RawMessage `json:"droppedAttributesCount"`
	DroppedEventsCount     json.RawMessage `json:"droppedEventsCount"`
	DroppedLinksCount      json.RawMessage `json:"droppedLinksCount"`
}

type jsonRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []keyValue `json:"attributes"`
		} `json:"resource"`
		SchemaURL  json.RawMessage `json:"schemaUrl"`
		ScopeSpans []struct {
			Scope struct {
				Name    string          `json:"name"`
				Version json.RawMessage `json:"version"`
			} `json:"scope"`
			SchemaURL json.RawMessage `json:"schemaUrl"`
			Spans     []jsonSpan      `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// peerKeys are the attributes that name the far side of a client call, in
// order of preference.
var peerKeys = []string{"peer.service", "server.address", "net.peer.name", "http.host"}

// portKeys give the port of a client call's far side.
var portKeys = []string{"server.port", "net.peer.port"}

// The attributes that give a server span's path and method, in the
// reference's order (current and previous HTTP semantic conventions, then
// Zipkin and OpenTracing); the span name stands in for a missing path.
var (
	pathKeys   = []string{"url.path", "http.target", "http.route", "http.path", "http.url"}
	methodKeys = []string{"http.request.method", "http.method"}
)

// spanKeys are the other span attributes the reader reads (the data contract's checks), and codeKeys those kept for
// research only (CODEPATHS.md).
var (
	spanKeys = []string{"http.route", "rpc.method", "http.response.status_code", "http.request.resend_count",
		"user_agent.original"}
	codeKeys = []string{"code.function.name"}
)

// resourceKeys are the resource attributes the reader reads, and faasPrefix the prefix of the function-platform keys
// whose presence it reads.
var resourceKeys = []string{"service.name", "service.namespace", "service.version", "service.instance.id",
	"deployment.environment.name", "deployment.environment", "host.name", "host.arch", "k8s.pod.name", "k8s.pod.uid",
	"k8s.deployment.name", "k8s.namespace.name", "k8s.node.name", "k8s.container.name", "aws.ecs.task.arn",
	"container.id", "container.image.name", "container.image.tag", "container.image.tags",
	"container.image.repo_digests", "telemetry.sdk.language", "telemetry.sdk.version"}

const faasPrefix = "faas."

// eventName is the name of the span events the reader counts, and eventTypeKeys the attributes that give their type
// (current and previous semantic conventions).
var (
	eventName     = "rpc.message"
	eventTypeKeys = []string{"rpc.message.type", "message.type"}
)

// probeRoute is the reference's health-probe rule:
// kubelet liveness and readiness probes and gRPC health checks are not
// traffic. Left in, each probe is a near-zero visit and an entry request.
var probeRoute = regexp.MustCompile(`grpc\.health\.v1\.Health/|(^|\s)/(_?healthz?|readyz|livez|ready|ping)(\?|$)`)

// IsProbe reports whether a server span's route ("METHOD /path") is a
// health or readiness probe.
func IsProbe(route string) bool { return probeRoute.MatchString(route) }

// wideProbeRoute matches the framework probes and scrapes that IsProbe misses (DATA.md 3.5). The reader only marks
// them (SpanExt.ProbePath); it does not drop them.
var wideProbeRoute = regexp.MustCompile(`(^|\s)/(actuator/health(/[A-Za-z]+)?|q/health(/(ready|live|started))?|` +
	`health/(live|ready|liveness|readiness)|api/health|metrics)/?(\?|$)`)

// WideProbe reports whether a server span's route matches a framework probe or scrape that IsProbe does not drop.
func WideProbe(route string) bool { return wideProbeRoute.MatchString(route) && !IsProbe(route) }

// userAgentClass returns the probe class of a user agent.
func userAgentClass(ua string) uint8 {
	switch {
	case strings.HasPrefix(ua, "kube-probe/"):
		return UAKubeProbe
	case strings.HasPrefix(ua, "ELB-HealthChecker/"):
		return UAELBHealthChecker
	case strings.HasPrefix(ua, "Prometheus/"):
		return UAPrometheus
	}
	return 0
}

// attr returns the first of keys that the span carries (even with an
// empty value, as the reference reads it) and whether there was one.
func attr(kvs []keyValue, keys []string) (string, bool) {
	for _, key := range keys {
		for _, kv := range kvs {
			if kv.Key == key {
				return kv.Value.StringValue, true
			}
		}
	}
	return "", false
}

// noQuery cuts a path's query string and fragment, only after a '/' (a
// span name such as "UsersController#show" is no URL).
func noQuery(p string) string {
	cut := -1
	for _, i := range []int{strings.IndexByte(p, '?'), strings.IndexByte(p, '#')} {
		if i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut > 0 && strings.Contains(p[:cut], "/") {
		return p[:cut]
	}
	return p
}

// route is a server span's "METHOD /path" as the reference builds it: the
// path without its query, and an http.url without its scheme and host.
func route(js *jsonSpan) string {
	path, _ := attr(js.Attributes, pathKeys)
	if path == "" {
		path = js.Name
	}
	path = noQuery(path)
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if strings.Count(path, "/") >= 3 {
			path = "/" + strings.SplitN(path, "/", 4)[3]
		} else {
			path = "/"
		}
	}
	meth, _ := attr(js.Attributes, methodKeys)
	return strings.TrimSpace(meth + " " + path)
}

// MaxRoutes is the most distinct routes the reader keeps (Data.Routes); MaxRouteBytes the longest route it keeps.
const (
	MaxRoutes     = 4096
	MaxRouteBytes = 256
)

// methodPath matches a span name of the form "METHOD /path", as HTTP server instrumentations name their spans.
var methodPath = regexp.MustCompile(`^[A-Z]+ /`)

// serverRoute is the route a SERVER span's visit is counted under, for the per-route own time: "METHOD template"
// from http.route when the span carries it; else the span's name when it reads "METHOD /path"; else, for an RPC
// span (rpc.method set), its name ("pkg.Service/Method"); else "METHOD path" from url.path (then http.target) without
// its query. "" when none applies, when the route is longer than MaxRouteBytes, or when it looks like personal data
// (PersonalKind): such a visit counts under no route.
func serverRoute(js *jsonSpan) string {
	meth, _ := attr(js.Attributes, methodKeys)
	join := func(p string) string { return strings.TrimSpace(meth + " " + noQuery(p)) }
	var rt string
	if v, ok := attr(js.Attributes, []string{"http.route"}); ok && v != "" {
		rt = join(v)
	} else if methodPath.MatchString(js.Name) {
		rt = noQuery(js.Name)
	} else if v, ok := attr(js.Attributes, []string{"rpc.method"}); ok && v != "" && js.Name != "" {
		rt = js.Name
	} else if v, ok := attr(js.Attributes, []string{"url.path", "http.target"}); ok && v != "" {
		rt = join(v)
	}
	if len(rt) > MaxRouteBytes || PersonalKind(rt) != "" {
		return ""
	}
	return rt
}

func hex64(s string) (uint64, bool) {
	if len(s) != 16 {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 64)
	return v, err == nil
}

func traceID(s string) ([2]uint64, bool) {
	if len(s) != 32 {
		return [2]uint64{}, false
	}
	hi, err1 := strconv.ParseUint(s[:16], 16, 64)
	lo, err2 := strconv.ParseUint(s[16:], 16, 64)
	return [2]uint64{hi, lo}, err1 == nil && err2 == nil
}

// ParseTraceID reads an OTLP/JSON trace id: 32 hex digits, either case.
func ParseTraceID(s string) ([2]uint64, bool) { return traceID(s) }

// ParseSpanID reads an OTLP/JSON span id: 16 hex digits, either case.
func ParseSpanID(s string) (uint64, bool) { return hex64(s) }

// TraceIDHex writes a trace id as OTLP/JSON does.
func TraceIDHex(t [2]uint64) string { return fmt.Sprintf("%016x%016x", t[0], t[1]) }

// skippedID is an id of a skipped span as its example shows it: as written when it is written as an id of some
// encoding (IDEncoding: hex, a 16-digit trace id, base64), else by its class, "other": an id field that holds no id
// holds whatever the capture's writer put there, and an example reaches every view of the finding.
func skippedID(s string, bytes int) string {
	if e := IDEncoding(s, bytes); e == "other" {
		return e
	}
	return s
}

// IDEncoding names how an id is written: "hex" (OTLP/JSON), "base64" (a generic protobuf-JSON converter: 24
// characters for 16 bytes, 12 for 8), "hex64" (a 16-digit trace id) or "other". bytes is the id's length: 16 for a
// trace id, 8 for a span id.
func IDEncoding(s string, bytes int) string {
	isHex := func(s string) bool {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
		return true
	}
	switch {
	case len(s) == 2*bytes && isHex(s):
		return "hex"
	case bytes == 16 && len(s) == 16 && isHex(s):
		return "hex64"
	case len(s) == base64Len(bytes) && isBase64(s, bytes):
		return "base64"
	}
	return "other"
}

func base64Len(n int) int { return (n + 2) / 3 * 4 }

func isBase64(s string, n int) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/' || c == '=' ||
			c == '-' || c == '_') {
			return false
		}
	}
	pad := strings.Count(s, "=")
	return pad == (3-n%3)%3 && strings.TrimRight(s, "=") == s[:len(s)-pad]
}

// thOf returns the sampling threshold of a W3C tracestate: the th sub-key of its ot entry ("" when there is none).
func thOf(ts string) string {
	for _, entry := range strings.Split(ts, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok || k != "ot" {
			continue
		}
		for _, sub := range strings.Split(v, ";") {
			if th, ok := strings.CutPrefix(sub, "th:"); ok {
				return th
			}
		}
	}
	return ""
}

// rawPresent reports whether a raw field is present with a value other than null, 0, "", false or an empty array.
func rawPresent(r json.RawMessage) bool {
	switch s := string(bytes.TrimSpace(r)); s {
	case "", "null", "0", `"0"`, `""`, "false", "[]", "{}":
		return false
	}
	return true
}

// rawCount reads a raw count (a number, or a string of one), 0 when it is none.
func rawCount(r json.RawMessage) uint64 {
	n, err := strconv.ParseUint(strings.Trim(string(bytes.TrimSpace(r)), `"`), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func cap16(n uint64) uint16 {
	if n > 65535 {
		return 65535
	}
	return uint16(n)
}

// statusCode reads a span status's code leniently: 0 unset, 1 ok, 2 error; the proto3 enum names are read too.
func statusCode(r json.RawMessage) uint8 {
	var st struct {
		Code json.RawMessage `json:"code"`
	}
	if json.Unmarshal(r, &st) != nil {
		return 0
	}
	switch strings.Trim(string(bytes.TrimSpace(st.Code)), `"`) {
	case "1", "STATUS_CODE_OK":
		return 1
	case "2", "STATUS_CODE_ERROR":
		return 2
	}
	return 0
}

// messageEvents counts the rpc.message events of a span by type, leniently; ok is false when the events do not read.
func messageEvents(r json.RawMessage) (sent, recv uint64, ok bool) {
	var evs []struct {
		Name       string     `json:"name"`
		Attributes []keyValue `json:"attributes"`
	}
	if json.Unmarshal(r, &evs) != nil {
		return 0, 0, false
	}
	for _, ev := range evs {
		if ev.Name != eventName {
			continue
		}
		t, _ := attr(ev.Attributes, eventTypeKeys)
		switch t {
		case "SENT":
			sent++
		case "RECEIVED":
			recv++
		}
	}
	return sent, recv, true
}

// The kinds of personal data the reader recognizes in kept values (DATA.md 15).
var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	jwtRE   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]+`)
	cardRE  = regexp.MustCompile(`\b[0-9]{13,19}\b`)
)

// PersonalKind returns what kind of personal data a value looks like: "email", "jwt", "card" (a run of 13 to 19
// digits that passes the Luhn check), or "" for none.
func PersonalKind(s string) string {
	if strings.IndexByte(s, '@') >= 0 && emailRE.MatchString(s) {
		return "email"
	}
	if strings.Contains(s, "eyJ") && jwtRE.MatchString(s) {
		return "jwt"
	}
	if hasDigitRun(s, 13) {
		for _, m := range cardRE.FindAllString(s, -1) {
			if luhn(m) {
				return "card"
			}
		}
	}
	return ""
}

// MaskPersonal replaces what PersonalKind recognizes with "[masked]".
func MaskPersonal(s string) string {
	s = emailRE.ReplaceAllString(s, "[masked]")
	s = jwtRE.ReplaceAllString(s, "[masked]")
	return cardRE.ReplaceAllStringFunc(s, func(m string) string {
		if luhn(m) {
			return "[masked]"
		}
		return m
	})
}

func hasDigitRun(s string, n int) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		if '0' <= s[i] && s[i] <= '9' {
			if run++; run >= n {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

func luhn(d string) bool {
	sum := 0
	for i := 0; i < len(d); i++ {
		n := int(d[len(d)-1-i] - '0')
		if i%2 == 1 {
			if n *= 2; n > 9 {
				n -= 9
			}
		}
		sum += n
	}
	return sum%10 == 0
}

// job is one raw request and its position in the capture.
type job struct {
	seq  int
	raw  []byte
	file int   // index of the file in the list read
	line int64 // 1-based line of a .jsonl file, else 0
	val  int64 // 0-based index of the value in a .json file, else -1
}

// part is what one worker decodes from one request.
type part struct {
	seq     int
	res     []Resource
	info    []ResourceInfo
	peers   []string
	addrs   []string
	scopes  []string
	exts    []SpanExt
	routes  []string
	spans   []Span // Res, Peer, Addr, Scope, Route and Ext index into this part's own tables
	probes  []SpanKey
	long    []longName
	skipped int
	stats   ReadStats
	ignored map[string]int64
	nspans  int
	err     error
}

// longName is the name of a SERVER span that lasted LongSpan or more.
type longName struct {
	key  SpanKey
	name string
}

// keptSpanKey reports whether the reader reads a span attribute.
var keptSpanKey = func() map[string]bool {
	m := map[string]bool{}
	for _, l := range [][]string{peerKeys, portKeys, pathKeys, methodKeys, spanKeys, codeKeys} {
		for _, k := range l {
			m[k] = true
		}
	}
	return m
}()

// storedSpanKey are the span attributes whose values the reader holds in memory (peers and their ports, routes): the
// ones Limits.MaxAttrBytes bounds. Paths, methods and the user agent are only looked at while a span is read.
var storedSpanKey = func() map[string]bool {
	m := map[string]bool{"http.route": true, "rpc.method": true}
	for _, l := range [][]string{peerKeys, portKeys} {
		for _, k := range l {
			m[k] = true
		}
	}
	return m
}()

// personalKeys are the span attributes whose values are scanned for personal data: paths, routes and the user agent.
var personalKeys = func() map[string]bool {
	m := map[string]bool{"user_agent.original": true, "rpc.method": true}
	for _, k := range pathKeys {
		m[k] = true
	}
	return m
}()

func decodeRequest(raw []byte, lim Limits) (p part) {
	var req jsonRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		p.err = err
		return p
	}
	p.stats.Requests = 1
	ignore := func(what string, n int64) {
		if p.ignored == nil {
			p.ignored = map[string]int64{}
		}
		p.ignored[what] += n
	}
	tooLong := func(key, v string) bool {
		if lim.MaxAttrBytes > 0 && len(v) > lim.MaxAttrBytes {
			p.err = &LimitError{Code: contract.InputTooLarge, What: "a value of the attribute " + key, Limit: int64(lim.MaxAttrBytes)}
			return true
		}
		return false
	}
	peerIdx := map[string]int32{}
	addrIdx := map[string]int32{}
	scopeIdx := map[string]int32{}
	extIdx := map[SpanExt]int32{}
	routeIdx := map[string]uint16{}
	var ignoredAttrs int64 // span attributes nothing reads
	defer func() {
		if ignoredAttrs > 0 {
			ignore("span attribute", ignoredAttrs)
		}
	}()
	for _, rs := range req.ResourceSpans {
		var r Resource
		var ri ResourceInfo
		var svc, dep string
		var inst, host, imgName, imgTag string
		var env, envOld string
		for _, kv := range rs.Resource.Attributes {
			v := kv.Value.StringValue
			switch kv.Key {
			case "service.name":
				svc = v
				ri.ServiceNameNotString = kv.Value.notString()
			case "k8s.deployment.name":
				dep = v
			case "k8s.pod.name":
				r.Pod = v
			case "service.instance.id":
				inst = v
			case "host.name":
				host = v
			case "container.image.name":
				imgName = v
			case "container.image.tag":
				imgTag = v
			case "service.namespace":
				ri.Namespace = v
			case "service.version":
				ri.ServiceVersion = v
			case "deployment.environment.name":
				env = v
			case "deployment.environment":
				envOld = v
			case "k8s.pod.uid":
				ri.PodUID = v
			case "aws.ecs.task.arn":
				ri.TaskARN = v
			case "host.arch":
				ri.HostArch = v
			case "k8s.namespace.name":
				ri.K8sNamespace = v
			case "k8s.node.name":
				ri.Node = v
			case "k8s.container.name":
				ri.ContainerName = v
			case "container.id":
				ri.ContainerID = v
			case "container.image.tags", "container.image.repo_digests":
				vs := kv.Value.strings()
				for _, s := range vs {
					if tooLong(kv.Key, s) {
						return p
					}
				}
				if kv.Key == "container.image.tags" {
					ri.ImageTags = vs
				} else {
					ri.RepoDigests = vs
				}
			case "telemetry.sdk.language":
				ri.SDKLanguage = v
			case "telemetry.sdk.version":
				ri.SDKVersion = v
			default:
				if strings.HasPrefix(kv.Key, faasPrefix) {
					ri.FaaS = true
				} else {
					ignore("resource attribute "+ignoredKey(kv.Key), 1)
				}
				continue
			}
			if tooLong(kv.Key, v) {
				return p
			}
		}
		if rawPresent(rs.SchemaURL) {
			ignore("schemaUrl", 1)
		}
		r.Service = svc
		if r.Service == "" {
			r.Service = dep
		}
		ri.PodName = r.Pod
		if r.Pod == "" {
			r.Pod = ri.PodUID
		}
		if r.Pod == "" {
			r.Pod = taskID(ri.TaskARN)
		}
		if r.Pod == "" {
			r.Pod = inst
		}
		if r.Pod == "" {
			r.Pod = host
		}
		r.Image = imgName
		if imgTag != "" && imgName != "" {
			r.Image = imgName + ":" + imgTag
		}
		ri.Deployment, ri.ImageName, ri.ImageTag = dep, imgName, imgTag
		if inst != "" {
			ri.InstanceIDs = []string{inst}
		}
		if host != "" {
			ri.Hosts = []string{host}
		}
		if env == "" {
			env = envOld
		}
		if env != "" {
			ri.Environments = []string{env}
		}
		ri.RepoDigests = sortedUnique(ri.RepoDigests)
		ri.ImageTags = sortedUnique(ri.ImageTags)
		resIdx := int32(len(p.res))
		p.res = append(p.res, r)
		p.info = append(p.info, ri)
		for _, ss := range rs.ScopeSpans {
			var scope int32 // 0: no scope name
			if n := ss.Scope.Name; n != "" {
				si, seen := scopeIdx[n]
				if !seen {
					si = int32(len(p.scopes))
					p.scopes = append(p.scopes, n)
					scopeIdx[n] = si
				}
				scope = si + 1
			}
			if rawPresent(ss.Scope.Version) {
				ignore("scope version", 1)
			}
			if rawPresent(ss.SchemaURL) {
				ignore("schemaUrl", 1)
			}
			for i := range ss.Spans {
				js := &ss.Spans[i]
				p.nspans++
				tid, ok1 := traceID(js.TraceID)
				sid, ok2 := hex64(js.SpanID)
				if js.Start.float || js.End.float {
					p.stats.FloatTimes++
				}
				// a span without an end (0) or ending before it starts has no usable duration
				if !ok1 || !ok2 || r.Service == "" || js.Start.v == 0 || js.End.v < js.Start.v {
					p.skipped++
					switch {
					case !ok1 || !ok2:
						p.stats.SkippedIDs++
						te, se := IDEncoding(js.TraceID, 16), IDEncoding(js.SpanID, 8)
						if te == "base64" || se == "base64" {
							p.stats.IDsBase64++
						} else if te == "hex64" {
							p.stats.TraceIDs64++
						}
					case r.Service == "":
						p.stats.SkippedService++
					default:
						p.stats.SkippedTime++
					}
					if len(p.stats.SkippedExamples) < contract.MaxExamples {
						p.stats.SkippedExamples = append(p.stats.SkippedExamples,
							skippedID(js.TraceID, 16)+":"+skippedID(js.SpanID, 8))
					}
					continue
				}
				p.stats.IDsHex++
				var pid uint64
				if js.ParentSpanID != "" {
					var okp bool
					if pid, okp = hex64(js.ParentSpanID); !okp {
						p.stats.BadParents++
						if IDEncoding(js.ParentSpanID, 8) == "base64" {
							p.stats.Base64Parents++
						}
					}
				}
				// the span's other fields, read in one pass over its attributes
				var ext SpanExt
				for _, kv := range js.Attributes {
					v := kv.Value.StringValue
					if !keptSpanKey[kv.Key] {
						ignoredAttrs++
						continue
					}
					if storedSpanKey[kv.Key] && tooLong(kv.Key, v) {
						return p
					}
					switch kv.Key {
					case "http.route":
						if ext.Route == "" {
							ext.Route = v
						}
					case "rpc.method":
						if ext.RPCMethod == "" {
							ext.RPCMethod = v
						}
					case "http.response.status_code":
						if n, err := strconv.ParseUint(kv.Value.text(), 10, 16); err == nil && ext.HTTPStatus == 0 {
							ext.HTTPStatus = uint16(n)
						}
					case "http.request.resend_count":
						if n, err := strconv.ParseUint(kv.Value.text(), 10, 64); err == nil && ext.Resends == 0 {
							ext.Resends = cap16(n)
						}
					case "user_agent.original":
						if ext.UserAgent == 0 {
							ext.UserAgent = userAgentClass(v)
						}
					}
					if personalKeys[kv.Key] && !ext.Personal && PersonalKind(v) != "" {
						ext.Personal = true
					}
				}
				if js.Kind == KindServer {
					if ts := thOfRaw(js.TraceState); ts != "" {
						ext.TH = ts
					}
				}
				if rawPresent(js.Status) {
					ext.Status = statusCode(js.Status)
				}
				if rawPresent(js.Events) {
					if js.Kind == KindServer {
						s, rv, ok := messageEvents(js.Events)
						if !ok {
							ignore("span events (unreadable)", 1)
						}
						ext.MsgSent, ext.MsgRecv = cap16(s), cap16(rv)
					} else {
						ignore("span events", 1)
					}
				}
				if rawPresent(js.Links) {
					var links []json.RawMessage
					if js.Kind == KindConsumer && json.Unmarshal(js.Links, &links) == nil {
						ext.Links = cap16(uint64(len(links)))
					} else {
						ignore("span links", 1)
					}
				}
				if rawCount(js.DroppedAttributesCount) > 0 {
					ext.Dropped = true
					ignore("span droppedAttributesCount", 1)
				}
				if rawPresent(js.Flags) {
					ignore("span flags", 1)
				}
				if rawCount(js.DroppedEventsCount) > 0 {
					ignore("span droppedEventsCount", 1)
				}
				if rawCount(js.DroppedLinksCount) > 0 {
					ignore("span droppedLinksCount", 1)
				}
				var rt string
				if js.Kind == KindServer {
					rt = route(js)
					if IsProbe(rt) {
						p.probes = append(p.probes, SpanKey{tid, sid})
						continue
					}
					ext.ProbePath = WideProbe(rt)
				}
				if ext.Personal && len(p.stats.PersonalExamples) < contract.MaxExamples {
					p.stats.PersonalExamples = append(p.stats.PersonalExamples, js.TraceID)
				}
				sp := Span{Trace: tid, ID: sid, Parent: pid, Start: js.Start.v, End: js.End.v,
					Res: resIdx, Peer: -1, Addr: -1, Scope: scope, Kind: uint8(js.Kind)}
				if js.Kind == KindServer {
					if sr := serverRoute(js); sr != "" {
						ri, seen := routeIdx[sr]
						if !seen && len(p.routes) < MaxRoutes {
							ri = uint16(len(p.routes)) + 1
							p.routes = append(p.routes, sr)
							routeIdx[sr] = ri
						}
						sp.Route = ri // 0 past MaxRoutes
					}
				}
				if ext != (SpanExt{}) {
					ei, seen := extIdx[ext]
					if !seen {
						ei = int32(len(p.exts))
						p.exts = append(p.exts, ext)
						extIdx[ext] = ei
					}
					sp.Ext = ei + 1
				}
				if js.Kind == KindServer && js.End.v-js.Start.v >= LongSpan {
					p.long = append(p.long, longName{SpanKey{tid, sid}, js.Name})
				}
				if js.Kind == KindClient {
					for _, key := range peerKeys {
						found := ""
						for _, kv := range js.Attributes {
							if kv.Key == key && kv.Value.StringValue != "" {
								found = kv.Value.StringValue
								break
							}
						}
						if found != "" {
							pi, seen := peerIdx[found]
							if !seen {
								pi = int32(len(p.peers))
								p.peers = append(p.peers, found)
								peerIdx[found] = pi
							}
							sp.Peer = pi
							addr := found
							for _, kv := range js.Attributes {
								if (kv.Key == portKeys[0] || kv.Key == portKeys[1]) && kv.Value.text() != "" {
									addr = found + ":" + kv.Value.text()
									break
								}
							}
							ai, seen := addrIdx[addr]
							if !seen {
								ai = int32(len(p.addrs))
								p.addrs = append(p.addrs, addr)
								addrIdx[addr] = ai
							}
							sp.Addr = ai
							break
						}
					}
				}
				p.spans = append(p.spans, sp)
			}
		}
	}
	return p
}

func thOfRaw(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(r, &s) != nil {
		return ""
	}
	return thOf(s)
}

// taskID is an ECS task's identity as a pod identity: the last element of its aws.ecs.task.arn
// (arn:aws:ecs:REGION:ACCOUNT:task/CLUSTER/ID, or .../task/ID), after its last "/" or ":", which is unique to the task.
// The rest of the ARN names the account, the region and the cluster, which no output needs (ARCHITECTURE.md 5.9). An
// ARN that ends in a separator has no id, and the next identity in the order is taken.
func taskID(arn string) string {
	return arn[strings.LastIndexAny(arn, "/:")+1:]
}

// ignoredKey is an attribute key as the Ignored counts name it: the key when it is a plain name, else "(other)".
func ignoredKey(k string) string {
	if s, untrusted := contract.SafeName(k); !untrusted && len(s) <= 64 {
		return s
	}
	return "(other)"
}

func sortedUnique(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	j := 0
	for i := range out {
		if i == 0 || out[i] != out[j-1] {
			out[j] = out[i]
			j++
		}
	}
	return out[:j]
}

// appendNew appends the values of add that s does not hold yet, in order.
func appendNew(s, add []string) []string {
	for _, v := range add {
		found := false
		for _, w := range s {
			if w == v {
				found = true
				break
			}
		}
		if !found {
			s = append(s, v)
		}
	}
	return s
}

// mergeInfo adds what another resource of the same Resource says to ri.
func mergeInfo(ri *ResourceInfo, o *ResourceInfo) {
	first := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	first(&ri.Namespace, o.Namespace)
	first(&ri.ServiceVersion, o.ServiceVersion)
	first(&ri.PodName, o.PodName)
	first(&ri.PodUID, o.PodUID)
	first(&ri.TaskARN, o.TaskARN)
	first(&ri.HostArch, o.HostArch)
	first(&ri.K8sNamespace, o.K8sNamespace)
	first(&ri.Node, o.Node)
	first(&ri.Deployment, o.Deployment)
	first(&ri.ContainerName, o.ContainerName)
	first(&ri.ContainerID, o.ContainerID)
	first(&ri.ImageName, o.ImageName)
	first(&ri.ImageTag, o.ImageTag)
	first(&ri.SDKLanguage, o.SDKLanguage)
	first(&ri.SDKVersion, o.SDKVersion)
	ri.Environments = appendNew(ri.Environments, o.Environments)
	ri.InstanceIDs = appendNew(ri.InstanceIDs, o.InstanceIDs)
	ri.Hosts = appendNew(ri.Hosts, o.Hosts)
	ri.ImageTags = appendNew(ri.ImageTags, o.ImageTags)
	ri.RepoDigests = appendNew(ri.RepoDigests, o.RepoDigests)
	ri.FaaS = ri.FaaS || o.FaaS
	ri.ServiceNameNotString = ri.ServiceNameNotString || o.ServiceNameNotString
}

// ------------------------------------------------------------------- reading

// ReadDir reads every trace file in a capture: CAPTURE/traces/*.jsonl,
// *.json, and their .gz forms. A path to a single file or to a folder of
// trace files is also accepted. It uses all cores. It is report v1's reader
// (predict CAPTURE, infer), and reads a capture whole under none of the
// Limits, as rhobound-cli did; check and fit read through ReadFiles.
func ReadDir(path string) (*Data, error) {
	files, err := traceFiles(path)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no trace files (*.jsonl, *.json, *.gz) under %s", path)
	}
	refs := make([]FileRef, len(files))
	for i, f := range files {
		refs[i] = FileRef{Path: f}
	}
	return read(refs, Limits{}, false)
}

// ReadFiles reads exactly the listed files, in the order given, under the limits given; a file with an SHA256 is
// checked against it while it is read. It differs from ReadDir in one more way: the last line of the last file, when
// it has no newline and does not decode (an object still being written), is dropped and counted in
// Stats.PartialTail instead of refusing the capture.
//
// Errors: a *LimitError when a limit is passed; an *Error with the code contract.StoreManifestMismatch for a sha256
// that differs, contract.StoreCompressionMismatch for a .gz file that is not gzip, and contract.TraceBadJSON for a
// value that does not decode; other errors are I/O errors.
func ReadFiles(files []FileRef, lim Limits) (*Data, error) {
	return read(files, lim, true)
}

// errStop is how the merge loop tells the file reader to stop.
var errStop = errors.New("stopped")

func read(files []FileRef, lim Limits, partialTail bool) (*Data, error) {
	workers := runtime.GOMAXPROCS(0)
	jobs := make(chan job, workers*2)
	parts := make(chan part, workers*2)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				p := decodeRequest(j.raw, lim)
				p.seq = j.seq
				if p.err != nil {
					if _, isLimit := p.err.(*LimitError); !isLimit {
						p.err = &Error{Code: contract.TraceBadJSON, Path: files[j.file].Path, Line: j.line, Value: j.val,
							Err: p.err, inValue: true}
					}
				}
				parts <- p
			}
		}()
	}
	go func() { wg.Wait(); close(parts) }()

	var readErr error
	var partialTails int
	go func() {
		defer close(jobs)
		seq := 0
		var total int64
		for i, f := range files {
			n, e := feedFile(f, i, partialTail && i == len(files)-1, lim, &total, jobs, stop, &seq)
			partialTails += n
			if e != nil {
				if e != errStop {
					readErr = e
				}
				return
			}
		}
	}()

	d := &Data{Files: make([]string, len(files))}
	for i, f := range files {
		d.Files[i] = f.Path
	}
	resIdx := map[Resource]int32{}
	peerIdx := map[string]int32{}
	addrIdx := map[string]int32{}
	scopeIdx := map[string]int32{}
	extIdx := map[SpanExt]int32{}
	routeIdx := map[string]uint16{}
	var firstErr error
	stopped := false
	halt := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
		if !stopped {
			stopped = true
			close(stop)
		}
	}
	// Parts arrive in the order the workers finish; they are merged in capture
	// order, so the spans, resources and peers (and every sum over them) come
	// out the same on every run.
	pending := map[int]part{}
	next := 0
	probes := map[SpanKey]bool{}
	merge := func(p part) {
		if p.err != nil {
			if _, isLimit := p.err.(*LimitError); isLimit {
				halt(p.err)
			} else if firstErr == nil {
				firstErr = p.err
			}
			return
		}
		if firstErr != nil {
			return // the capture is refused anyway; only the order of the first error matters
		}
		d.Stats.SpansRead += int64(p.nspans)
		if lim.MaxSpans > 0 && d.Stats.SpansRead > int64(lim.MaxSpans) {
			halt(&LimitError{Code: contract.InputTooLarge, What: "the number of spans", Limit: int64(lim.MaxSpans)})
			return
		}
		addStats(&d.Stats, &p.stats)
		for k, v := range p.ignored {
			if d.Ignored == nil {
				d.Ignored = map[string]int64{}
			}
			d.Ignored[k] += v
		}
		d.Skipped += p.skipped
		d.Probes += len(p.probes)
		for _, k := range p.probes {
			probes[k] = true
		}
		rmap := make([]int32, len(p.res))
		for i, r := range p.res {
			gi, ok := resIdx[r]
			if !ok {
				gi = int32(len(d.Resources))
				d.Resources = append(d.Resources, r)
				d.Info = append(d.Info, p.info[i])
				resIdx[r] = gi
			} else {
				mergeInfo(&d.Info[gi], &p.info[i])
			}
			rmap[i] = gi
		}
		pmap := make([]int32, len(p.peers))
		for i, s := range p.peers {
			gi, ok := peerIdx[s]
			if !ok {
				gi = int32(len(d.Peers))
				d.Peers = append(d.Peers, s)
				peerIdx[s] = gi
			}
			pmap[i] = gi
		}
		smap := make([]int32, len(p.scopes)+1) // 0 stays 0: no scope
		for i, s := range p.scopes {
			gi, ok := scopeIdx[s]
			if !ok {
				gi = int32(len(d.Scopes)) + 1
				d.Scopes = append(d.Scopes, s)
				scopeIdx[s] = gi
			}
			smap[i+1] = gi
		}
		emap := make([]int32, len(p.exts)+1) // 0 stays 0: no other fields
		for i, e := range p.exts {
			gi, ok := extIdx[e]
			if !ok {
				gi = int32(len(d.Exts)) + 1
				d.Exts = append(d.Exts, e)
				extIdx[e] = gi
			}
			emap[i+1] = gi
		}
		rtmap := make([]uint16, len(p.routes)+1) // 0 stays 0: no route
		for i, s := range p.routes {
			gi, ok := routeIdx[s]
			if !ok && len(d.Routes) < MaxRoutes {
				gi = uint16(len(d.Routes)) + 1
				d.Routes = append(d.Routes, s)
				routeIdx[s] = gi
			}
			rtmap[i+1] = gi
		}
		for _, ln := range p.long {
			if d.LongNames == nil {
				d.LongNames = map[SpanKey]string{}
			}
			if len(d.LongNames) < maxLongNames {
				d.LongNames[ln.key] = ln.name
			}
		}
		amap := make([]int32, len(p.addrs))
		for i, s := range p.addrs {
			gi, ok := addrIdx[s]
			if !ok {
				gi = int32(len(d.Addrs))
				d.Addrs = append(d.Addrs, s)
				addrIdx[s] = gi
			}
			amap[i] = gi
		}
		for _, sp := range p.spans {
			sp.Res = rmap[sp.Res]
			if sp.Peer >= 0 {
				sp.Peer = pmap[sp.Peer]
			}
			if sp.Addr >= 0 {
				sp.Addr = amap[sp.Addr]
			}
			sp.Scope = smap[sp.Scope]
			sp.Ext = emap[sp.Ext]
			sp.Route = rtmap[sp.Route]
			d.Spans = append(d.Spans, sp)
		}
	}
	for p := range parts {
		pending[p.seq] = p
		for {
			q, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			merge(q)
		}
	}
	if readErr != nil {
		return nil, readErr
	}
	if firstErr != nil {
		return nil, firstErr
	}
	d.Stats.PartialTail = partialTails
	if len(probes) > 0 {
		// a probe's own child spans (zipkin-go records the health check under its GET /health) are probe
		// traffic with no parent left, not spans whose parent was lost: they would mark their trace broken
		for i := range d.Spans {
			sp := &d.Spans[i]
			if sp.Parent != 0 && probes[SpanKey{sp.Trace, sp.Parent}] {
				sp.Parent = 0
				d.ProbeChildren++
			}
		}
	}
	return d, nil
}

func addStats(t, s *ReadStats) {
	t.Requests += s.Requests
	t.IDsHex += s.IDsHex
	t.SkippedIDs += s.SkippedIDs
	t.IDsBase64 += s.IDsBase64
	t.TraceIDs64 += s.TraceIDs64
	t.SkippedService += s.SkippedService
	t.SkippedTime += s.SkippedTime
	t.BadParents += s.BadParents
	t.Base64Parents += s.Base64Parents
	t.FloatTimes += s.FloatTimes
	for _, e := range s.SkippedExamples {
		if len(t.SkippedExamples) < contract.MaxExamples {
			t.SkippedExamples = append(t.SkippedExamples, e)
		}
	}
	for _, e := range s.PersonalExamples {
		if len(t.PersonalExamples) < contract.MaxExamples {
			t.PersonalExamples = append(t.PersonalExamples, e)
		}
	}
}

// TraceFileSuffixes are the file names a capture's trace files carry.
var TraceFileSuffixes = []string{".jsonl", ".json", ".jsonl.gz", ".json.gz"}

// IsTraceFile reports whether a file name is one the reader reads.
func IsTraceFile(name string) bool {
	for _, s := range TraceFileSuffixes {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// TraceDir returns the folder that holds a capture's trace files: CAPTURE/traces when it exists, else CAPTURE.
func TraceDir(path string) string {
	if sub := filepath.Join(path, "traces"); isDir(sub) {
		return sub
	}
	return path
}

func traceFiles(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	dir := TraceDir(path)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); IsTraceFile(n) {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Strings(out)
	return out, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// errTooLarge is what a limitedReader returns when its limit is passed.
var errTooLarge = errors.New("too large")

// limitedReader counts the inflated bytes of one object against the object's limit and, through total, the capture's.
type limitedReader struct {
	r        io.Reader
	n, max   int64
	total    *int64
	maxTotal int64
	what     string
	limit    *LimitError
}

func (l *limitedReader) Read(b []byte) (int, error) {
	n, err := l.r.Read(b)
	l.n += int64(n)
	*l.total += int64(n)
	if l.max > 0 && l.n > l.max {
		l.limit = &LimitError{Code: contract.InputTooLarge, What: "the inflated size of " + l.what, Limit: l.max}
		return n, errTooLarge
	}
	if l.maxTotal > 0 && *l.total > l.maxTotal {
		l.limit = &LimitError{Code: contract.InputTooLarge, What: "the inflated size of the capture", Limit: l.maxTotal}
		return n, errTooLarge
	}
	return n, err
}

// send hands one request to the workers, unless the merge loop asked the reader to stop.
func send(jobs chan<- job, stop <-chan struct{}, j job) bool {
	select {
	case jobs <- j:
		return true
	case <-stop:
		return false
	}
}

// feedFile reads one file's requests into jobs. It returns how many partial last lines it dropped (0 or 1; only when
// tail is true) and errStop when the merge loop asked it to stop.
func feedFile(ref FileRef, fileIdx int, tail bool, lim Limits, total *int64, jobs chan<- job, stop <-chan struct{}, seq *int) (int, error) {
	path := ref.Path
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	h := sha256.New()
	var raw io.Reader = f
	if ref.SHA256 != "" {
		raw = io.TeeReader(f, h)
	}
	var r io.Reader = raw
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(raw)
		if err != nil {
			return 0, &Error{Code: contract.StoreCompressionMismatch, Path: path, Value: -1, Err: err}
		}
		defer gz.Close()
		r = gz
	}
	lr := &limitedReader{r: r, max: lim.MaxObjectBytes, total: total, maxTotal: lim.MaxCaptureBytes, what: filepath.Base(path)}
	fileErr := func(err error, line, val int64) error {
		if lr.limit != nil {
			return lr.limit
		}
		code := contract.TraceBadJSON
		if strings.HasSuffix(path, ".gz") && !isJSONError(err) {
			code = contract.StoreCompressionMismatch // the gzip stream itself is damaged
		}
		return &Error{Code: code, Path: path, Line: line, Value: val, Err: err}
	}
	dropped := 0
	if strings.Contains(filepath.Base(path), ".jsonl") {
		br := bufio.NewReaderSize(lr, 1<<20)
		var lineNo int64
		for {
			line, err := readLine(br, lim.MaxLineBytes)
			lineNo++
			if err == errLineTooLong {
				return 0, &LimitError{Code: contract.InputTooLarge, What: fmt.Sprintf("line %d of %s", lineNo, filepath.Base(path)), Limit: lim.MaxLineBytes}
			}
			if len(bytes.TrimSpace(line)) > 0 {
				if err == io.EOF && tail && !json.Valid(line) {
					dropped++ // an object still being written: its last line is cut short
				} else {
					if !send(jobs, stop, job{seq: *seq, raw: line, file: fileIdx, line: lineNo, val: -1}) {
						return 0, errStop
					}
					*seq++
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return 0, fileErr(err, lineNo, -1)
			}
		}
	} else {
		// *.json: any number of whitespace-separated top-level values
		dec := json.NewDecoder(bufio.NewReaderSize(lr, 1<<20))
		var val int64
		for ; ; val++ {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				return 0, fileErr(err, 0, val)
			}
			if lim.MaxLineBytes > 0 && int64(len(raw)) > lim.MaxLineBytes {
				return 0, &LimitError{Code: contract.InputTooLarge, What: fmt.Sprintf("value %d of %s", val, filepath.Base(path)), Limit: lim.MaxLineBytes}
			}
			if !send(jobs, stop, job{seq: *seq, raw: []byte(raw), file: fileIdx, val: val}) {
				return 0, errStop
			}
			*seq++
		}
	}
	if ref.SHA256 != "" {
		if _, err := io.Copy(io.Discard, raw); err != nil { // the rest of the file, past the end of the gzip stream
			return 0, err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != ref.SHA256 {
			return 0, &Error{Code: contract.StoreManifestMismatch, Path: path, Value: -1,
				Err: fmt.Errorf("sha256 %s, the capture manifest says %s", got, ref.SHA256)}
		}
	}
	return dropped, nil
}

func isJSONError(err error) bool {
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	return errors.As(err, &se) || errors.As(err, &te)
}

var errLineTooLong = errors.New("line too long")

// readLine reads one line, its newline included, and refuses one longer than max bytes (0: no limit). At the end of
// the input it returns the last line, possibly without a newline, and io.EOF.
func readLine(br *bufio.Reader, max int64) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if max > 0 && int64(len(buf)+len(chunk)) > max {
			return nil, errLineTooLong
		}
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, err
	}
}
