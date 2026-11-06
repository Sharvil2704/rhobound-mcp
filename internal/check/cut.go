// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The canonical form, as the cut writes it: one OTLP/JSON request per line, reduced to the canonical keep-list. The
// values of kept attributes, the ids, the kinds and the integer times are copied byte for byte, so the reader reads
// the canonical form exactly as it read the capture (DATA.md 1, rule 8).
type (
	cutRequest struct {
		ResourceSpans []cutResourceSpans `json:"resourceSpans"`
	}
	cutResourceSpans struct {
		Resource struct {
			Attributes []cutKV `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []cutScopeSpans `json:"scopeSpans"`
	}
	cutScopeSpans struct {
		Scope struct {
			Name string `json:"name,omitempty"`
		} `json:"scope"`
		Spans []cutSpan `json:"spans"`
	}
	cutKV struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	cutSpan struct {
		TraceID                string          `json:"traceId"`
		SpanID                 string          `json:"spanId"`
		ParentSpanID           string          `json:"parentSpanId,omitempty"`
		Name                   string          `json:"name,omitempty"`
		Kind                   json.RawMessage `json:"kind,omitempty"`
		Start                  json.RawMessage `json:"startTimeUnixNano,omitempty"`
		End                    json.RawMessage `json:"endTimeUnixNano,omitempty"`
		TraceState             json.RawMessage `json:"traceState,omitempty"`
		Status                 json.RawMessage `json:"status,omitempty"`
		Events                 json.RawMessage `json:"events,omitempty"`
		Links                  json.RawMessage `json:"links,omitempty"`
		DroppedAttributesCount json.RawMessage `json:"droppedAttributesCount,omitempty"`
		Attributes             []cutKV         `json:"attributes,omitempty"`
	}
	cutEvent struct {
		Name       string  `json:"name"`
		Attributes []cutKV `json:"attributes,omitempty"`
	}
	cutLink struct {
		TraceID string `json:"traceId,omitempty"`
		SpanID  string `json:"spanId,omitempty"`
	}
)

// personalSpanKeys are the kept span attributes whose string values are masked when they look like personal data.
var personalSpanKeys = map[string]bool{"url.path": true, "http.target": true, "http.route": true, "http.path": true,
	"http.url": true, "user_agent.original": true, "rpc.method": true}

// copiedInputs are the inputs of other kinds the cut copies next to the canonical traces: they describe the same window.
var copiedInputs = []string{"peer-map.json", "request-count.json"}

// Cut writes the canonical form of a checked capture into outDir (which must not exist, or be empty): the traces whose
// entry SERVER span starts in [start, end), one request per line in outDir/traces/*.jsonl.gz, in the order read,
// reduced to the canonical keep-list, identical duplicate spans dropped, values that look like personal data masked,
// float times written as integers; the cluster dump, the peer map and the request count copied as they are; and the
// final manifest, outDir/manifest.json, which it returns. d is the capture as Run returned it.
func Cut(m *contract.Manifest, d *otlp.Data, start, end time.Time, outDir string) (*contract.Manifest, error) {
	ws, we, err := WindowNanos(start, end)
	if err != nil {
		return nil, err
	}
	if ents, err := os.ReadDir(outDir); err == nil && len(ents) > 0 {
		return nil, fmt.Errorf("--out %s is not empty", outDir)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(outDir, "traces"), 0o755); err != nil {
		return nil, err
	}
	entry := traceEntries(d)
	in := func(trace [2]uint64, ok bool, own uint64) bool {
		if ok {
			if t, known := entry[trace]; known {
				return t >= ws && t < we
			}
		}
		return own >= ws && own < we
	}
	keep := otlp.Keep()
	out := &contract.Manifest{Schema: contract.ManifestSchema, Source: m.Source,
		Window:  contract.Window{Start: start.UTC(), End: end.UTC(), Cut: contract.CutEntrySpanStart},
		Margins: m.Margins, PartitionTZ: m.PartitionTZ, Collector: m.Collector, KeepList: keep.SHA256(),
		Sampling: m.Sampling, ClusterDump: m.ClusterDump, Limits: m.Limits, Dropped: &contract.Dropped{}, Dir: outDir}
	if d.DroppedDuplicates != nil {
		for _, n := range d.DroppedDuplicates {
			out.Dropped.DuplicateSpans += int64(n)
		}
	}
	dupSeen := map[otlp.SpanKey]bool{}
	outside := map[[2]uint64]bool{}
	var traceObjects []contract.Object
	for _, o := range m.Objects {
		if otlp.IsTraceFile(o.Name) {
			traceObjects = append(traceObjects, o)
		}
	}
	for i, o := range traceObjects {
		name := fmt.Sprintf("traces/%06d.jsonl.gz", i)
		src := filepath.Join(m.Dir, filepath.FromSlash(o.Name))
		dst := filepath.Join(outDir, filepath.FromSlash(name))
		partial, err := cutFile(src, dst, i == len(traceObjects)-1, m.Limits, func(r *cutRequest) {
			filterRequest(r, keep, in, d.DroppedDuplicates, dupSeen, outside)
		})
		if err != nil {
			return nil, err
		}
		out.Dropped.PartialTailLines += int64(partial)
		sum, size, err := fileSHA256(dst)
		if err != nil {
			return nil, err
		}
		out.Objects = append(out.Objects, contract.Object{Name: name, Size: size, SHA256: sum})
	}
	out.Dropped.TracesOutsideWindow = int64(len(outside))
	if err := copyInputs(m.Dir, outDir); err != nil {
		return nil, err
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), append(out.Canonical(), '\n'), 0o644); err != nil {
		return nil, err
	}
	return out, nil
}

// WindowNanos returns the window [start, end) in Unix nanoseconds, as span times are. The error says why it is not a
// window: it needs a start and an end after it, both within the times a span can hold, from the Unix epoch to the
// last instant whose Unix nanoseconds fit an int64 (2262-04-11T23:47:16.854775807Z); a window outside them would wrap.
func WindowNanos(start, end time.Time) (uint64, uint64, error) {
	if start.IsZero() || !end.After(start) {
		return 0, 0, errors.New("--window: a window needs a start and an end after it")
	}
	if lo, hi := time.Unix(0, 0), time.Unix(0, math.MaxInt64); start.Before(lo) || end.After(hi) {
		return 0, 0, fmt.Errorf("--window: a window lies within %s and %s, the times a span can hold",
			lo.UTC().Format(time.RFC3339Nano), hi.UTC().Format(time.RFC3339Nano))
	}
	return uint64(start.UnixNano()), uint64(end.UnixNano()), nil
}

// traceEntries is the start of each trace's entry: its earliest entry SERVER span (one whose caller is not in the
// capture, is a message consumer, or is a CLIENT span of a service with no SERVER spans), else its earliest span.
func traceEntries(d *otlp.Data) map[[2]uint64]uint64 {
	sp := d.Spans
	modelled := map[string]bool{}
	for i := range sp {
		if sp[i].Kind == otlp.KindServer {
			modelled[d.Resources[sp[i].Res].Service] = true
		}
	}
	out := map[[2]uint64]uint64{}
	pos := map[uint64]int32{}
	ForEachTrace(d, func(g []int32) {
		clear(pos)
		for _, k := range g {
			if _, seen := pos[sp[k].ID]; !seen {
				pos[sp[k].ID] = k
			}
		}
		entry := sp[g[0]].Start // the spans are by start: the first entry found is the earliest
		for _, k := range g {
			if sp[k].Kind != otlp.KindServer {
				continue
			}
			p, ok := caller(sp, pos, k)
			if !ok || sp[p].Kind == otlp.KindConsumer || sp[p].Kind == otlp.KindClient && !modelled[d.Resources[sp[p].Res].Service] {
				entry = sp[k].Start
				break
			}
		}
		out[sp[g[0]].Trace] = entry
	})
	return out
}

// filterRequest reduces one request to the window and the keep-list.
func filterRequest(r *cutRequest, keep otlp.KeepList, in func([2]uint64, bool, uint64) bool,
	dups map[otlp.SpanKey]int, dupSeen map[otlp.SpanKey]bool, outside map[[2]uint64]bool) {
	var rss []cutResourceSpans
	for _, rs := range r.ResourceSpans {
		had := 0
		attrs := rs.Resource.Attributes[:0:0]
		for _, kv := range rs.Resource.Attributes {
			if keep.KeepsResource(kv.Key) {
				attrs = append(attrs, kv)
			}
		}
		rs.Resource.Attributes = attrs
		var sss []cutScopeSpans
		for _, ss := range rs.ScopeSpans {
			var spans []cutSpan
			for _, s := range ss.Spans {
				had++
				trace, ok := otlp.ParseTraceID(s.TraceID)
				if !in(trace, ok, timeOf(s.Start)) {
					if ok {
						outside[trace] = true
					}
					continue
				}
				if id, okID := otlp.ParseSpanID(s.SpanID); ok && okID && dups[otlp.SpanKey{Trace: trace, ID: id}] > 0 {
					k := otlp.SpanKey{Trace: trace, ID: id}
					if dupSeen[k] {
						continue
					}
					dupSeen[k] = true
				}
				spans = append(spans, canonicalSpan(s, keep))
			}
			if len(spans) > 0 || len(ss.Spans) == 0 {
				ss.Spans = spans
				sss = append(sss, ss)
			}
		}
		if had == 0 || len(sss) > 0 {
			rs.ScopeSpans = sss
			rss = append(rss, rs)
		}
	}
	r.ResourceSpans = rss
}

// canonicalSpan reduces one span to the keep-list. The fields the reader reads leniently (traceState, status, events,
// links) are reduced leniently too: a value that does not read is dropped, as the reader ignores it.
func canonicalSpan(s cutSpan, keep otlp.KeepList) cutSpan {
	s.Name = otlp.MaskPersonal(s.Name)
	s.Start, s.End = integerTime(s.Start), integerTime(s.End)
	s.TraceState = rawString(s.TraceState, otOnly)
	if len(s.Status) > 0 {
		var st struct {
			Code json.RawMessage `json:"code"`
		}
		raw := s.Status
		s.Status = nil
		if json.Unmarshal(raw, &st) == nil && len(st.Code) > 0 {
			s.Status, _ = json.Marshal(st)
		}
	}
	s.Events = keptEvents(s.Events, keep)
	s.Links = keptLinks(s.Links)
	var attrs []cutKV
	for _, kv := range s.Attributes {
		if !keep.KeepsSpan(kv.Key) {
			continue
		}
		if personalSpanKeys[kv.Key] {
			kv.Value = maskValue(kv.Value)
		}
		attrs = append(attrs, kv)
	}
	s.Attributes = attrs
	return s
}

// rawString applies f to a raw JSON string; anything else, or an empty result, is dropped.
func rawString(raw json.RawMessage, f func(string) string) json.RawMessage {
	var v string
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	if v = f(v); v == "" {
		return nil
	}
	b, _ := json.Marshal(v)
	return b
}

// keptEvents keeps the events of the keep-list (rpc.message) with their kept attributes.
func keptEvents(raw json.RawMessage, keep otlp.KeepList) json.RawMessage {
	var evs []struct {
		Name       string  `json:"name"`
		Attributes []cutKV `json:"attributes"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &evs) != nil {
		return nil
	}
	var out []cutEvent
	for _, e := range evs {
		if !slices.Contains(keep.Event, e.Name) {
			continue
		}
		var attrs []cutKV
		for _, kv := range e.Attributes {
			if keep.KeepsEventKey(kv.Key) {
				attrs = append(attrs, kv)
			}
		}
		out = append(out, cutEvent{Name: e.Name, Attributes: attrs})
	}
	if len(out) == 0 {
		return nil
	}
	b, _ := json.Marshal(out)
	return b
}

// keptLinks keeps each link's trace and span id: the reader counts links.
func keptLinks(raw json.RawMessage) json.RawMessage {
	var ls []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &ls) != nil || len(ls) == 0 {
		return nil
	}
	out := make([]cutLink, len(ls))
	for i, l := range ls {
		_ = json.Unmarshal(l, &out[i]) // a link that does not read still counts, with no ids
	}
	b, _ := json.Marshal(out)
	return b
}

// integerTime writes a time given as a float or with an exponent as the integer the reader reads it as; other forms
// are kept byte for byte.
func integerTime(raw json.RawMessage) json.RawMessage {
	t := bytes.Trim(raw, `"`)
	if len(t) == 0 || bytes.IndexAny(t, ".eE") < 0 {
		return raw
	}
	if _, err := strconv.ParseUint(string(t), 10, 64); err == nil {
		return raw
	}
	f, err := strconv.ParseFloat(string(t), 64)
	if err != nil {
		return raw
	}
	return json.RawMessage(`"` + strconv.FormatUint(uint64(f), 10) + `"`)
}

// timeOf reads a raw time as the reader does, 0 when it does not read.
func timeOf(raw json.RawMessage) uint64 {
	t := string(bytes.Trim(raw, `"`))
	if v, err := strconv.ParseUint(t, 10, 64); err == nil {
		return v
	}
	if f, err := strconv.ParseFloat(t, 64); err == nil {
		return uint64(f)
	}
	return 0
}

// otOnly keeps the OpenTelemetry entry of a W3C tracestate: the only one the reader reads.
func otOnly(ts string) string {
	for _, entry := range strings.Split(ts, ",") {
		if k, _, ok := strings.Cut(strings.TrimSpace(entry), "="); ok && k == "ot" {
			return strings.TrimSpace(entry)
		}
	}
	return ""
}

// maskValue masks what looks like personal data in a string value; other values are kept as they are.
func maskValue(raw json.RawMessage) json.RawMessage {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	sv, ok := v["stringValue"]
	if !ok {
		return raw
	}
	var s string
	if json.Unmarshal(sv, &s) != nil || otlp.PersonalKind(s) == "" {
		return raw
	}
	v["stringValue"], _ = json.Marshal(otlp.MaskPersonal(s))
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// cutFile streams one trace file into its canonical file. last says whether it is the capture's last file, whose
// unterminated, undecodable last line the reader drops (and so does the cut).
func cutFile(src, dst string, last bool, lim contract.Limits, each func(*cutRequest)) (int, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(src, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", src, err)
		}
		defer gz.Close()
		r = gz
	}
	w, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	zw := gzip.NewWriter(w) // no name and no time in the header: the same input gives the same bytes
	bw := bufio.NewWriterSize(zw, 1<<20)
	emit := func(raw []byte) error {
		var req cutRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		each(&req)
		if len(req.ResourceSpans) == 0 {
			return nil
		}
		b, err := marshalLine(&req)
		if err != nil {
			return err
		}
		_, err = bw.Write(b)
		return err
	}
	partial := 0
	var runErr error
	if strings.Contains(filepath.Base(src), ".jsonl") {
		br := bufio.NewReaderSize(r, 1<<20)
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				if err == io.EOF && last && !json.Valid(line) {
					partial++
				} else if e := emit(line); e != nil {
					runErr = e
					break
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				runErr = fmt.Errorf("%s: %w", src, err)
				break
			}
		}
	} else {
		dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err == io.EOF {
				break
			} else if err != nil {
				runErr = fmt.Errorf("%s: %w", src, err)
				break
			}
			if e := emit(raw); e != nil {
				runErr = e
				break
			}
		}
	}
	if err := bw.Flush(); err != nil && runErr == nil {
		runErr = err
	}
	if err := zw.Close(); err != nil && runErr == nil {
		runErr = err
	}
	if err := w.Close(); err != nil && runErr == nil {
		runErr = err
	}
	return partial, runErr
}

// marshalLine writes one request as one line, without HTML escaping (the values keep their bytes).
func marshalLine(r *cutRequest) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// copyInputs copies the capture's cluster dump, peer map and request count next to the canonical traces.
func copyInputs(from, to string) error {
	for _, n := range copiedInputs {
		b, err := os.ReadFile(filepath.Join(from, n))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, n), b, 0o644); err != nil {
			return err
		}
	}
	src := filepath.Join(from, "cluster")
	if !isDirPath(src) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if e.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		if !e.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}

func isDirPath(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
