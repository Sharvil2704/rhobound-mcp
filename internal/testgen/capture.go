// SPDX-License-Identifier: Apache-2.0

// Package testgen builds test captures: it reads a capture into memory as OTLP/JSON, changes it, and writes it back,
// and it holds the defect injectors of the data contract: for every finding code a capture triggers, a fixture that
// injects that defect into a clean capture (docs/mcp/DATA.md 17). tools/gentestdata is its command.
package testgen

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Capture is a capture in memory: its requests, in the order read.
type Capture struct {
	Requests []*Request
}

// Request is one ExportTraceServiceRequest.
type Request struct {
	ResourceSpans []*ResourceSpans `json:"resourceSpans"`
}

// ResourceSpans are the spans of one resource.
type ResourceSpans struct {
	Resource   Resource      `json:"resource"`
	ScopeSpans []*ScopeSpans `json:"scopeSpans"`
}

// Resource is a resource's attributes.
type Resource struct {
	Attributes []KV `json:"attributes"`
}

// ScopeSpans are the spans of one instrumentation scope.
type ScopeSpans struct {
	Scope *Scope  `json:"scope,omitempty"`
	Spans []*Span `json:"spans"`
}

// Scope is an instrumentation scope.
type Scope struct {
	Name string `json:"name,omitempty"`
}

// Span is one span. Kind, times and status are kept as JSON values, so that a fixture can write them in any form.
type Span struct {
	TraceID      string          `json:"traceId"`
	SpanID       string          `json:"spanId"`
	ParentSpanID string          `json:"parentSpanId,omitempty"`
	Name         string          `json:"name,omitempty"`
	Kind         json.RawMessage `json:"kind"`
	Start        json.RawMessage `json:"startTimeUnixNano"`
	End          json.RawMessage `json:"endTimeUnixNano"`
	TraceState   string          `json:"traceState,omitempty"`
	Status       json.RawMessage `json:"status,omitempty"`
	Events       []Event         `json:"events,omitempty"`
	Links        []Link          `json:"links,omitempty"`
	Attributes   []KV            `json:"attributes,omitempty"`
}

// Event is a span event.
type Event struct {
	Name       string `json:"name"`
	Attributes []KV   `json:"attributes,omitempty"`
}

// Link is a span link.
type Link struct {
	TraceID string `json:"traceId"`
	SpanID  string `json:"spanId"`
}

// KV is one attribute.
type KV struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

// Str, Int and Strs are attribute values.
func Str(s string) map[string]any { return map[string]any{"stringValue": s} }

// Int is an integer attribute value, written as proto3 JSON does (a string of digits).
func Int(n int64) map[string]any { return map[string]any{"intValue": strconv.FormatInt(n, 10)} }

// Strs is an array of strings.
func Strs(ss ...string) map[string]any {
	vals := make([]any, len(ss))
	for i, s := range ss {
		vals[i] = map[string]any{"stringValue": s}
	}
	return map[string]any{"arrayValue": map[string]any{"values": vals}}
}

// Load reads every trace file of a capture folder (CAPTURE/traces/*, or the folder's own trace files), in name order.
func Load(dir string) (*Capture, error) {
	tdir := dir
	if st, err := os.Stat(filepath.Join(dir, "traces")); err == nil && st.IsDir() {
		tdir = filepath.Join(dir, "traces")
	}
	ents, err := os.ReadDir(tdir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && (strings.HasSuffix(n, ".jsonl") || strings.HasSuffix(n, ".jsonl.gz") || strings.HasSuffix(n, ".json") ||
			strings.HasSuffix(n, ".json.gz")) {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	c := &Capture{}
	for _, n := range names {
		b, err := readMaybeGzip(filepath.Join(tdir, n))
		if err != nil {
			return nil, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		for {
			var r Request
			if err := dec.Decode(&r); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("%s: %w", n, err)
			}
			c.Requests = append(c.Requests, &r)
		}
	}
	if len(c.Requests) == 0 {
		return nil, fmt.Errorf("no requests under %s", dir)
	}
	return c, nil
}

func readMaybeGzip(p string) ([]byte, error) {
	b, err := os.ReadFile(p)
	if err != nil || !strings.HasSuffix(p, ".gz") {
		return b, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(zr)
}

// Clone returns a deep copy.
func (c *Capture) Clone() *Capture {
	b, err := json.Marshal(c.Requests)
	if err != nil {
		panic(err)
	}
	var out Capture
	if err := json.Unmarshal(b, &out.Requests); err != nil {
		panic(err)
	}
	return &out
}

// Lines returns each request as one line of JSON, newline included.
func (c *Capture) Lines() [][]byte {
	out := make([][]byte, len(c.Requests))
	for i, r := range c.Requests {
		b, err := json.Marshal(r)
		if err != nil {
			panic(err)
		}
		out[i] = append(b, '\n')
	}
	return out
}

// Write writes the capture as one file of a capture folder: dir/traces/name, gzip-compressed when name ends in .gz.
func (c *Capture) Write(dir, name string) error {
	return WriteFile(filepath.Join(dir, "traces", name), bytes.Join(c.Lines(), nil))
}

// WriteFile writes a file, making its folder, and gzip-compresses it when its name ends in .gz.
func WriteFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if strings.HasSuffix(p, ".gz") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(b); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		b = buf.Bytes()
	}
	return os.WriteFile(p, b, 0o644)
}

// Visit calls f for every span with its resource, in the order read.
func (c *Capture) Visit(f func(rs *ResourceSpans, s *Span)) {
	for _, r := range c.Requests {
		for _, rs := range r.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					f(rs, s)
				}
			}
		}
	}
}

// Filter keeps the spans for which keep is true; resources and requests left with no span are dropped.
func (c *Capture) Filter(keep func(rs *ResourceSpans, s *Span) bool) {
	var reqs []*Request
	for _, r := range c.Requests {
		var rss []*ResourceSpans
		for _, rs := range r.ResourceSpans {
			var sss []*ScopeSpans
			for _, ss := range rs.ScopeSpans {
				var spans []*Span
				for _, s := range ss.Spans {
					if keep(rs, s) {
						spans = append(spans, s)
					}
				}
				if len(spans) > 0 {
					ss.Spans = spans
					sss = append(sss, ss)
				}
			}
			if len(sss) > 0 {
				rs.ScopeSpans = sss
				rss = append(rss, rs)
			}
		}
		if len(rss) > 0 {
			r.ResourceSpans = rss
			reqs = append(reqs, r)
		}
	}
	c.Requests = reqs
}

// Attr returns a resource attribute's string value.
func (rs *ResourceSpans) Attr(key string) string {
	for _, kv := range rs.Resource.Attributes {
		if kv.Key == key {
			s, _ := kv.Value["stringValue"].(string)
			return s
		}
	}
	return ""
}

// Service is the resource's service.name.
func (rs *ResourceSpans) Service() string { return rs.Attr("service.name") }

// SetAttr sets (or adds) a resource attribute.
func (rs *ResourceSpans) SetAttr(key string, v map[string]any) {
	for i := range rs.Resource.Attributes {
		if rs.Resource.Attributes[i].Key == key {
			rs.Resource.Attributes[i].Value = v
			return
		}
	}
	rs.Resource.Attributes = append(rs.Resource.Attributes, KV{key, v})
}

// DelAttr removes a resource attribute.
func (rs *ResourceSpans) DelAttr(key string) {
	var out []KV
	for _, kv := range rs.Resource.Attributes {
		if kv.Key != key {
			out = append(out, kv)
		}
	}
	rs.Resource.Attributes = out
}

// SetAttr sets (or adds) a span attribute.
func (s *Span) SetAttr(key string, v map[string]any) {
	for i := range s.Attributes {
		if s.Attributes[i].Key == key {
			s.Attributes[i].Value = v
			return
		}
	}
	s.Attributes = append(s.Attributes, KV{key, v})
}

// KindOf is the span's kind as an integer (0 when it is not one).
func (s *Span) KindOf() int {
	n, _ := strconv.Atoi(string(s.Kind))
	return n
}

// SetKind writes the kind as an integer.
func (s *Span) SetKind(k int) { s.Kind = json.RawMessage(strconv.Itoa(k)) }

// Times returns the span's start and end in Unix nanoseconds.
func (s *Span) Times() (uint64, uint64) { return nanos(s.Start), nanos(s.End) }

// SetTimes writes the span's times as decimal strings.
func (s *Span) SetTimes(start, end uint64) {
	s.Start = json.RawMessage(strconv.Quote(strconv.FormatUint(start, 10)))
	s.End = json.RawMessage(strconv.Quote(strconv.FormatUint(end, 10)))
}

// Shift moves the span by delta nanoseconds.
func (s *Span) Shift(delta int64) {
	a, b := s.Times()
	s.SetTimes(uint64(int64(a)+delta), uint64(int64(b)+delta))
}

func nanos(raw json.RawMessage) uint64 {
	t := strings.Trim(string(raw), `"`)
	if v, err := strconv.ParseUint(t, 10, 64); err == nil {
		return v
	}
	f, _ := strconv.ParseFloat(t, 64)
	return uint64(f)
}

// TraceNumber reads a trace id as a number (its low 64 bits).
func TraceNumber(id string) uint64 {
	if len(id) < 16 {
		return 0
	}
	v, _ := strconv.ParseUint(id[len(id)-16:], 16, 64)
	return v
}

// Copy returns a deep copy of a span.
func (s *Span) Copy() *Span {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	var out Span
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

// Lines reads a file of JSON lines, or a gzip of one, as lines (newlines included).
func Lines(p string) ([][]byte, error) {
	b, err := readMaybeGzip(p)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<30)
	for sc.Scan() {
		out = append(out, append(append([]byte(nil), sc.Bytes()...), '\n'))
	}
	return out, sc.Err()
}

// RepoRoot is the repository's root folder: the first folder up from the working directory whose go.mod names this
// module (tests run in their package's folder).
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(b), "module github.com/Sharvil2704/rhobound-mcp\n") {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("no repository root above the working directory")
		}
		dir = up
	}
}

// BaseCapture is the clean capture the fixtures inject their defects into, relative to the repository's root.
const BaseCapture = "testdata/capture-chain"

// Base loads the base capture.
func Base() (*Capture, error) {
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	return Load(filepath.Join(root, filepath.FromSlash(BaseCapture)))
}
