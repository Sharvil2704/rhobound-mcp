// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// SniffBytes is how much of each object the sniff stage reads (inflated).
const SniffBytes = 64 << 10

// unreadableSuffixes are the object names the reader does not parse and that are named, never skipped (DATA.md 3.2).
var unreadableSuffixes = []string{".zst", ".zstd", ".binpb", ".pb", ".parquet", ".proto"}

// IsUnreadableObject reports whether a file name is one of a trace store's objects that the reader cannot parse: a
// compression or encoding it does not read, or no suffix at all.
func IsUnreadableObject(name string) bool {
	base := filepath.Base(name)
	if !strings.Contains(base, ".") {
		return true
	}
	for _, s := range unreadableSuffixes {
		if strings.HasSuffix(base, s) {
			return true
		}
	}
	return false
}

var (
	traceIDRE = regexp.MustCompile(`"traceId"\s*:\s*"([^"]*)"`)
	spanIDRE  = regexp.MustCompile(`"spanId"\s*:\s*"([^"]*)"`)
	kindRE    = regexp.MustCompile(`"kind"\s*:\s*"([^"]*)"`)
	snakeRE   = regexp.MustCompile(`"(scope_spans|trace_id|span_id|parent_span_id)"\s*:`)
	preV1RE   = regexp.MustCompile(`"instrumentationLibrarySpans"\s*:`)
)

// Sniff reads the first SniffBytes of each file and refuses, by name, what the reader would misread or refuse without
// saying why: unreadable suffixes, a .gz object that is not gzip, a pretty-printed .jsonl file or a JSON array,
// snake_case or pre-1.0 OTLP keys, other trace formats (Jaeger, Zipkin, X-Ray), ids that are not OTLP/JSON hex, and
// span kinds written as strings. It never refuses what the reader reads: an object it cannot classify is left to the
// reader.
func Sniff(files []otlp.FileRef) []contract.Finding {
	var out []contract.Finding
	seen := map[string]bool{}
	add := func(f contract.Finding) {
		k := f.Code + "\x00" + f.Subject
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	for i, ref := range files {
		name := filepath.Base(ref.Path)
		obj := func(code string) contract.Finding {
			return contract.NewFinding(code).WithSubject(name).WithNumber("object", float64(i))
		}
		if !otlp.IsTraceFile(name) {
			if IsUnreadableObject(name) {
				add(obj(contract.StoreUnreadableObject))
			}
			continue
		}
		head, gzipOK, err := readHead(ref.Path)
		if err != nil {
			continue // the reader names an I/O error
		}
		if strings.HasSuffix(name, ".gz") != gzipOK {
			add(obj(contract.StoreCompressionMismatch))
			continue
		}
		for _, f := range classify(head, strings.Contains(name, ".jsonl")) {
			add(f.WithSubject(name).WithNumber("object", float64(i)))
		}
	}
	return out
}

// readHead returns the first SniffBytes of a file, inflated when it is gzip, and whether it is gzip (by its magic).
func readHead(path string) ([]byte, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	isGzip := n == 2 && magic[0] == 0x1f && magic[1] == 0x8b
	var r io.Reader = f
	if isGzip {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, false, nil
		}
		defer gz.Close()
		r = gz
	}
	head, err := io.ReadAll(io.LimitReader(r, SniffBytes))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && len(head) == 0 {
		return nil, isGzip, err
	}
	return head, isGzip, nil
}

// classify reads the head of one object.
func classify(head []byte, jsonl bool) []contract.Finding {
	var out []contract.Finding
	t := bytes.TrimLeft(head, " \t\r\n\ufeff")
	if len(t) == 0 {
		return nil
	}
	switch t[0] {
	case '[':
		if bytes.Contains(t, []byte(`"localEndpoint"`)) || bytes.Contains(t, []byte(`"remoteEndpoint"`)) {
			return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP).WithExamples("zipkin")}
		}
		if bytes.Contains(t, []byte(`"resourceSpans"`)) || bytes.Contains(t, []byte(`"resource_spans"`)) {
			return []contract.Finding{contract.NewFinding(contract.TraceJSONArray)}
		}
		return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP)}
	case '{':
	default:
		return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP)}
	}
	keys := topKeys(t)
	switch {
	case keys["resourceSpans"]:
	case keys["resource_spans"]:
		return []contract.Finding{contract.NewFinding(contract.TraceSnakeCaseKeys)}
	case keys["data"] && bytes.Contains(t, []byte(`"traceID"`)):
		return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP).WithExamples("jaeger")}
	case keys["Traces"] || keys["Segments"] || keys["trace_id"] && keys["start_time"]:
		return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP).WithExamples("x-ray")}
	case len(keys) > 0:
		return []contract.Finding{contract.NewFinding(contract.TraceNotOTLP)}
	}
	if preV1RE.Match(t) {
		out = append(out, contract.NewFinding(contract.TracePreV1OTLP))
	}
	if snakeRE.Match(t) {
		out = append(out, contract.NewFinding(contract.TraceSnakeCaseKeys))
	}
	if jsonl {
		// a .jsonl file's first line must be a whole value; pretty-printed JSON ends its first line inside one
		if line, _, complete := bytes.Cut(t, []byte("\n")); complete && !json.Valid(line) {
			var x any
			if err := json.NewDecoder(bytes.NewReader(line)).Decode(&x); errors.Is(err, io.ErrUnexpectedEOF) {
				out = append(out, contract.NewFinding(contract.TraceJSONLPretty))
			}
		}
	}
	// id and kind encodings, as far as the head shows them
	enc := map[string]bool{}
	for _, m := range traceIDRE.FindAllSubmatch(t, -1) {
		enc[otlp.IDEncoding(string(m[1]), 16)] = true
	}
	for _, m := range spanIDRE.FindAllSubmatch(t, -1) {
		if otlp.IDEncoding(string(m[1]), 8) == "base64" { // hex span ids go with hex or 64-bit trace ids alike
			enc["base64"] = true
		}
	}
	delete(enc, "other")
	switch {
	case len(enc) > 1:
		out = append(out, contract.NewFinding(contract.TraceIDsMixed))
	case enc["base64"]:
		out = append(out, contract.NewFinding(contract.TraceIDsBase64))
	case enc["hex64"]:
		out = append(out, contract.NewFinding(contract.TraceTraceID64Bit))
	}
	if m := kindRE.FindSubmatch(t); m != nil {
		if strings.HasPrefix(string(m[1]), "SPAN_KIND_") {
			out = append(out, contract.NewFinding(contract.TraceKindEnumName))
		} else {
			out = append(out, contract.NewFinding(contract.TraceKindUnknown))
		}
	}
	return out
}

// topKeys returns the keys of the first JSON object of the head, as far as the head holds them.
func topKeys(b []byte) map[string]bool {
	keys := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return keys
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return keys
		}
		k, ok := tok.(string)
		if !ok {
			return keys
		}
		keys[k] = true
		if err := skipValue(dec); err != nil {
			return keys
		}
	}
	return keys
}

// skipValue reads past one value of a token stream.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
		if depth == 0 {
			return nil
		}
	}
}
