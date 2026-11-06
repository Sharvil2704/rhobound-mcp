// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

func TestParseUnix(t *testing.T) {
	for in, want := range map[string]uint64{
		"1760000000":            1760000000_000000000,
		"1760000000.5":          1760000000_500000000,
		"1760000000.123456789":  1760000000_123456789,
		"1760000000.1234567891": 1760000000_123456789,
		" 7 ":                   7_000000000,
	} {
		got, err := parseUnix(in)
		if err != nil || got != want {
			t.Errorf("parseUnix(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "-1", "+1", "x", "1.x", ".5"} {
		if _, err := parseUnix(bad); err == nil {
			t.Errorf("parseUnix(%q): no error", bad)
		}
	}
}

// span writes one OTLP/JSON span; ids are hex, times in seconds from 1000 s.
func span(trace, id, parent string, kind int, start, end float64, attrs string) string {
	ns := func(s float64) string { return fmt.Sprintf("%d", uint64(1000e9+s*1e9)) }
	p := ""
	if parent != "" {
		p = fmt.Sprintf(`,"parentSpanId":"%016s"`, parent)
	}
	a := ""
	if attrs != "" {
		a = `,"attributes":[` + attrs + `]`
	}
	return fmt.Sprintf(`{"traceId":"%032s","spanId":"%016s"%s,"name":"op <a&b>","kind":%d,"startTimeUnixNano":"%s","endTimeUnixNano":"%s"%s,"status":{}}`,
		trace, id, p, kind, ns(start), ns(end), a)
}

func reqLine(svc, pod string, spans ...string) string {
	res := fmt.Sprintf(`{"key":"service.name","value":{"stringValue":"%s"}}`, svc)
	if pod != "" {
		res += fmt.Sprintf(`,{"key":"k8s.pod.name","value":{"stringValue":"%s"}},{"key":"k8s.node.name","value":{"stringValue":"node-1"}}`, pod)
	}
	return fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[%s]},"scopeSpans":[{"scope":{"name":"lib"},"spans":[%s]}],"schemaUrl":"https://x"}]}`,
		res, strings.Join(spans, ","))
}

const peerAttr = `{"key":"net.peer.name","value":{"stringValue":"%s"}}`

func TestCut(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "spans.jsonl")
	lines := []string{
		// trace 1 starts before the window (10 s) and ends inside it: left out whole; trace 2 is inside it
		reqLine("front", "front-0", span("1", "1", "", 2, 9.9, 10.2, ""), span("2", "21", "", 2, 10.5, 10.6, "")),
		reqLine("back", "back-0", span("1", "2", "1", 2, 10.0, 10.1, ""), span("2", "22", "21", 2, 10.51, 10.55, "")),
		"{not json",
		// trace 3 starts in the window and ends after it: kept whole; trace 4 starts after: left out
		reqLine("front", "", span("3", "31", "", 2, 19.99, 20.5, ""), span("4", "41", "", 2, 20.0, 20.1, "")),
		reqLine("back", "back-0", span("3", "32", "31", 2, 20.1, 20.4, "")),
	}
	partial := reqLine("back", "back-0", span("2", "23", "21", 2, 10.52, 10.53, ""))
	body := strings.Join(lines, "\n") + "\n" + partial[:len(partial)/2]
	if err := os.WriteFile(in, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cap")
	sum, err := cut(1010e9, 1020e9, out, []string{in}, cutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Lines != 6 || sum.BadLines != 1 || sum.PartialLines != 1 {
		t.Errorf("lines %d bad %d partial %d; want 6, 1, 1", sum.Lines, sum.BadLines, sum.PartialLines)
	}
	if sum.TracesSeen != 4 || sum.TracesKept != 2 || sum.TracesBefore != 1 || sum.TracesAfter != 1 {
		t.Errorf("traces seen %d kept %d before %d after %d; want 4, 2, 1, 1", sum.TracesSeen, sum.TracesKept, sum.TracesBefore, sum.TracesAfter)
	}
	if sum.SpansSeen != 7 || sum.SpansKept != 4 {
		t.Errorf("spans seen %d kept %d; want 7, 4", sum.SpansSeen, sum.SpansKept)
	}
	if sum.LinesWritten != 4 {
		t.Errorf("lines written %d; want 4", sum.LinesWritten)
	}
	var front *ResourceCount
	for _, r := range sum.Resources {
		if r.Service == "front" {
			front = r
		}
	}
	if front == nil || front.Spans != 2 || front.WithPodName != 1 || len(front.Pods) != 1 || front.Nodes[0] != "node-1" {
		t.Errorf("front's resource counts: %+v", front)
	}

	// the output is gzip, passes the other fields through, and the CLI's reader takes it
	raw, err := os.ReadFile(filepath.Join(out, "traces", "spans.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if _, err := plain.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	text := plain.String()
	for _, want := range []string{`"schemaUrl":"https://x"`, `"scope":{"name":"lib"}`, `"name":"op <a&b>"`, `"status":{}`} {
		if !strings.Contains(text, want) {
			t.Errorf("output lost %s:\n%s", want, text)
		}
	}
	for _, gone := range []string{`"spanId":"0000000000000001"`, `"spanId":"0000000000000002"`, `"spanId":"0000000000000041"`} {
		if strings.Contains(text, gone) {
			t.Errorf("output kept %s", gone)
		}
	}
	d, err := otlp.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spans) != 4 {
		t.Errorf("the CLI's reader read %d spans; want 4", len(d.Spans))
	}
}

func TestCutGzipInputCutShort(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for i := 0; i < 200; i++ {
		fmt.Fprintln(zw, reqLine("s", "s-0", span(fmt.Sprintf("%x", i+1), fmt.Sprintf("%x", i+1), "", 2, float64(i)/10, float64(i)/10+0.01, "")))
	}
	zw.Close()
	b := buf.Bytes()
	in := filepath.Join(dir, "live.jsonl.gz")
	// a copy of a gzip stream that was still being written: no trailer, the last block cut
	if err := os.WriteFile(in, b[:len(b)*3/4], 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := cut(1000e9, 1100e9, filepath.Join(dir, "cap"), []string{in}, cutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.TracesKept == 0 || sum.TracesKept >= 200 || sum.BadLines != 0 {
		t.Errorf("kept %d traces (bad lines %d) of a cut-short stream; want some, not all, and no bad lines", sum.TracesKept, sum.BadLines)
	}
}

func TestStats(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	// 20 requests from a load generator (CLIENT root) into proxy -> front, which calls back 1 or 3 times and a
	// database (no server spans) on half its visits; on every 4th, front produces a message, which a consumer
	// takes and then calls the database
	for i := 0; i < 20; i++ {
		tr := fmt.Sprintf("%x", i+1)
		id := func(n int) string { return fmt.Sprintf("%x%02d", i+1, n) }
		t0 := float64(i)
		lines = append(lines, reqLine("loadgen", "", span(tr, id(1), "", 3, t0, t0+0.9, `{"key":"server.address","value":{"stringValue":"proxy"}}`)))
		lines = append(lines, reqLine("proxy", "proxy-0", span(tr, id(2), id(1), 2, t0+0.01, t0+0.8, ""), span(tr, id(3), id(2), 3, t0+0.02, t0+0.7, "")))
		fs := []string{span(tr, id(4), id(3), 2, t0+0.03, t0+0.6, ""), span(tr, id(5), id(4), 1, t0+0.04, t0+0.5, "")}
		backs := 1
		if i%2 == 1 {
			backs = 3
		}
		var bs []string
		for j := 0; j < backs; j++ {
			fs = append(fs, span(tr, id(10+j), id(5), 3, t0+0.05+0.1*float64(j), t0+0.1+0.1*float64(j), fmt.Sprintf(peerAttr, "back")))
			bs = append(bs, span(tr, id(20+j), id(10+j), 2, t0+0.06+0.1*float64(j), t0+0.09+0.1*float64(j), ""))
		}
		if i%2 == 0 {
			fs = append(fs, span(tr, id(30), id(4), 3, t0+0.4, t0+0.45, fmt.Sprintf(peerAttr, "db")))
		}
		var consumer []string
		if i%4 == 0 {
			fs = append(fs, span(tr, id(40), id(4), 4, t0+0.46, t0+0.47, ""))
			consumer = append(consumer, span(tr, id(41), id(40), 5, t0+0.6, t0+0.7, ""), span(tr, id(42), id(41), 3, t0+0.61, t0+0.62, fmt.Sprintf(peerAttr, "db")))
		}
		lines = append(lines, reqLine("front", "front-0", fs...), reqLine("back", fmt.Sprintf("back-%d", i%2), bs...))
		if len(consumer) > 0 {
			lines = append(lines, reqLine("worker", "worker-0", consumer...))
		}
	}
	// a health probe is dropped by the reader
	lines = append(lines, reqLine("back", "back-0", span("ff", "ff01", "", 2, 5, 5.001, `{"key":"url.path","value":{"stringValue":"/healthz"}}`)))
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := otlp.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := describe(d, 5)
	if st.Probes != 1 || st.Traces != 20 {
		t.Errorf("probes %d traces %d; want 1, 20", st.Probes, st.Traces)
	}
	edge := map[string]*Edge{}
	for _, e := range st.Edges {
		edge[e.Caller+">"+e.Callee] = e
	}
	if e := edge["proxy>front"]; e == nil || e.Shape != "fixed" || e.Calls != 20 {
		t.Errorf("proxy>front: %+v", e)
	}
	if e := edge["front>back"]; e == nil || e.Shape != "varies" || e.Calls != 40 || e.PerVisit["1"] != "50.0%" || e.PerVisit["3"] != "50.0%" {
		t.Errorf("front>back: %+v", e)
	}
	if e := edge["front>peer:db"]; e == nil || e.Shape != "fixed+1" || e.Calls != 10 || e.VisitsCalling != 0.5 {
		t.Errorf("front>peer:db (the consumer's calls must not count): %+v", e)
	}
	if len(st.Entries) != 1 || st.Entries[0].Service != "proxy" || st.Entries[0].Visits != 20 || len(st.Entries[0].Buckets) != 3 {
		t.Errorf("entries: %+v", st.Entries)
	}
	if len(st.Roots) != 1 || st.Roots[0].Service != "loadgen" || st.Roots[0].Kind != "client" {
		t.Errorf("roots: %+v", st.Roots)
	}
	for _, s := range st.Services {
		switch s.Name {
		case "worker":
			if s.Consumers != 5 || s.ConsumerTops != 0 || s.Kinds["consumer"] != 5 {
				t.Errorf("worker: %+v", s)
			}
		case "back":
			if s.Pods != 2 || s.Kinds["server"] != 40 {
				t.Errorf("back: %+v", s)
			}
		case "front":
			if s.Kinds["producer"] != 5 || s.Kinds["internal"] != 20 {
				t.Errorf("front: %+v", s)
			}
		}
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatal(err)
	}
}

func TestStatsExampleCapture(t *testing.T) {
	d, err := otlp.ReadDir(filepath.Join("..", "..", "..", "..", "testdata", "capture-chain"))
	if err != nil {
		t.Fatal(err)
	}
	st := describe(d, 30)
	var got []string
	for _, e := range st.Edges {
		got = append(got, fmt.Sprintf("%s>%s %s %d", e.Caller, e.Callee, e.Shape, e.Calls))
	}
	if want := "a>b fixed 3099,b>c fixed 3099"; strings.Join(got, ",") != want {
		t.Errorf("edges %v; want %s", got, want)
	}
	if len(st.Entries) != 1 || st.Entries[0].Service != "a" || st.Entries[0].Visits != 3099 || len(st.Entries[0].Buckets) != 3 {
		t.Errorf("entries: %+v", st.Entries)
	}
}

func TestCutNestedAndDrop(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "spans.jsonl")
	// front's HTTP SERVER span holds the framework's own SERVER span (same pod), which calls back and the flag
	// service; back's SERVER span sits under front's CLIENT span; the flag service answers once with an error
	lines := []string{
		reqLine("front", "front-0",
			span("1", "1", "", 2, 0, 0.10, ""),
			span("1", "2", "1", 2, 0.001, 0.099, ""),
			span("1", "3", "2", 1, 0.002, 0.098, ""),
			span("1", "4", "3", 3, 0.003, 0.05, fmt.Sprintf(peerAttr, "back")),
			span("1", "5", "3", 3, 0.06, 0.07, fmt.Sprintf(peerAttr, "flags"))),
		reqLine("back", "back-0", span("1", "6", "4", 2, 0.004, 0.049, "")),
		reqLine("flags", "flags-0", strings.Replace(span("1", "7", "5", 2, 0.061, 0.069, ""), `"status":{}`, `"status":{"code":2}`, 1)),
		// back calls front's twin with a gRPC CLIENT span around the HTTP CLIENT span: the outer one goes
		reqLine("back", "back-0", span("1", "9", "6", 3, 0.03, 0.045, ""), span("1", "10", "9", 3, 0.031, 0.044, fmt.Sprintf(peerAttr, "db"))),
		// a SERVER span under a SERVER span of another pod of the same service is a call, not nested
		reqLine("back", "back-1", span("1", "8", "6", 2, 0.01, 0.02, "")),
	}
	if err := os.WriteFile(in, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "cap")
	sum, err := cut(999e9, 1001e9, out, []string{in}, cutOptions{nestedInternal: true, drop: map[string]bool{"flags": true}})
	if err != nil {
		t.Fatal(err)
	}
	if sum.NestedInternal["front"] != 1 || sum.NestedInternal["back"] != 1 || len(sum.NestedInternal) != 2 {
		t.Errorf("nested made internal: %v; want front 1, back 1", sum.NestedInternal)
	}
	if sum.DroppedServices["flags"] != 1 || sum.SpansKept != 9 || sum.DroppedLostChild != 0 {
		t.Errorf("dropped %v, kept %d, lost children %d; want flags 1, 9, 0", sum.DroppedServices, sum.SpansKept, sum.DroppedLostChild)
	}
	d, err := otlp.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[uint64]uint8{}
	for _, sp := range d.Spans {
		kinds[sp.ID] = sp.Kind
	}
	if kinds[1] != otlp.KindServer || kinds[2] != otlp.KindInternal || kinds[8] != otlp.KindServer ||
		kinds[9] != otlp.KindInternal || kinds[0x10] != otlp.KindClient {
		t.Errorf("kinds after the cut: %v; want span 1 server, 2 internal, 8 server, 9 internal, 0x10 client", kinds)
	}
	st := describe(d, 1)
	var got []string
	for _, e := range st.Edges {
		got = append(got, fmt.Sprintf("%s>%s %d", e.Caller, e.Callee, e.Calls))
	}
	if want := "back>back 1,back>peer:db 1,front>back 1,front>peer:flags 1"; strings.Join(got, ",") != want {
		t.Errorf("edges %v; want %s", got, want)
	}

	// without the changes, the error span is counted and the nested span is a self-call
	sum, err = cut(999e9, 1001e9, filepath.Join(dir, "cap2"), []string{in}, cutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.Errors) != 1 || sum.Errors[0].Service != "flags" || sum.Errors[0].Errors != 1 || sum.NestedInternal != nil {
		t.Errorf("errors %+v, nested %v", sum.Errors, sum.NestedInternal)
	}
	d, err = otlp.ReadDir(filepath.Join(dir, "cap2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range describe(d, 1).Edges {
		if e.Caller == "front" && e.Callee == "front" {
			return
		}
	}
	t.Errorf("as captured, front's nested SERVER span is not read as a call to front")
}
