// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

const otlpLine = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]},"scopeSpans":` +
	`[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1111111111111111","kind":2,` +
	`"startTimeUnixNano":"1","endTimeUnixNano":"2","attributes":[{"key":"trace_id","value":{"stringValue":"x"}}]}]}]}]}`

func TestClassify(t *testing.T) {
	for name, c := range map[string]struct {
		head  string
		jsonl bool
		want  string
	}{
		"OTLP/JSON":            {otlpLine + "\n" + otlpLine, true, ""},
		"OTLP/JSON, cut short": {otlpLine[:100], true, ""},
		"an array":             {"[" + otlpLine + "]", false, contract.TraceJSONArray},
		"Zipkin":               {`[{"traceId":"a","id":"b","localEndpoint":{"serviceName":"x"}}]`, false, contract.TraceNotOTLP},
		"Jaeger":               {`{"data":[{"traceID":"a","spans":[]}]}`, false, contract.TraceNotOTLP},
		"X-Ray":                {`{"Traces":[{"Id":"1-x","Segments":[]}]}`, false, contract.TraceNotOTLP},
		"an X-Ray segment":     {`{"trace_id":"1-x","id":"y","start_time":1.5}`, true, contract.TraceNotOTLP},
		"other JSON":           {`{"hello":"world"}`, true, contract.TraceNotOTLP},
		"text":                 {"time=1 msg=hi", true, contract.TraceNotOTLP},
		"snake_case":           {`{"resource_spans":[]}`, true, contract.TraceSnakeCaseKeys},
		"snake_case inside":    {`{"resourceSpans":[{"scope_spans":[]}]}`, true, contract.TraceSnakeCaseKeys},
		"pre-1.0":              {`{"resourceSpans":[{"instrumentationLibrarySpans":[]}]}`, true, contract.TracePreV1OTLP},
		"pretty-printed":       {"{\n  \"resourceSpans\": []\n}\n", true, contract.TraceJSONLPretty},
		"pretty .json is fine": {"{\n  \"resourceSpans\": []\n}\n", false, ""},
		"base64 ids":           {strings.Replace(otlpLine, "0102030405060708090a0b0c0d0e0f10", "AQIDBAUGBwgJCgsMDQ4PEA==", 1), true, contract.TraceIDsBase64},
		"64-bit trace ids":     {strings.Replace(otlpLine, "0102030405060708090a0b0c0d0e0f10", "0102030405060708", 1), true, contract.TraceTraceID64Bit},
		"mixed ids": {otlpLine + "\n" + strings.Replace(otlpLine, "0102030405060708090a0b0c0d0e0f10", "AQIDBAUGBwgJCgsMDQ4PEA==", 1), true,
			contract.TraceIDsMixed},
		"enum kinds":  {strings.Replace(otlpLine, `"kind":2`, `"kind":"SPAN_KIND_SERVER"`, 1), true, contract.TraceKindEnumName},
		"other kinds": {strings.Replace(otlpLine, `"kind":2`, `"kind":"server"`, 1), true, contract.TraceKindUnknown},
	} {
		if got := codes(classify([]byte(c.head), c.jsonl)); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestSniffFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) otlp.FileRef {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return otlp.FileRef{Path: p}
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(otlpLine + "\n"))
	zw.Close()
	refs := []otlp.FileRef{
		write("a.jsonl.gz", gz.Bytes()),
		write("b.jsonl", []byte(otlpLine+"\n")),
		write("c.jsonl.gz", []byte(otlpLine+"\n")), // not gzip
		write("d.jsonl", gz.Bytes()),               // gzip without the suffix
		write("e.jsonl.zst", []byte{0x28, 0xb5, 0x2f, 0xfd}),
		write("f.binpb", []byte{1, 2}),
		write("g", []byte("x")),
		write("h.parquet", []byte("PAR1")),
		write("README.md", []byte("# notes")),
	}
	got := Sniff(refs)
	var lines []string
	for _, f := range got {
		lines = append(lines, f.Code+" "+f.Subject)
	}
	want := []string{"store.compression_mismatch c.jsonl.gz", "store.compression_mismatch d.jsonl", "store.unreadable_object e.jsonl.zst",
		"store.unreadable_object f.binpb", "store.unreadable_object g", "store.unreadable_object h.parquet"}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("sniff:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	for _, f := range got {
		if f.Numbers["object"] < 2 {
			t.Errorf("%s names object %v", f.Code, f.Numbers["object"])
		}
	}
}
