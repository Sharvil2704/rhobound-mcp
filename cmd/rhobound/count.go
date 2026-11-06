// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

func init() {
	register("count", command{run: cmdCount, usage: `  rhobound count   --from alb|jsonl|prometheus --window START,END --entry NAME=PREFIX... --out FILE [--source S]
                   [--probe-route R] [--time-key K] [--route-key K] [--label L] [--json] FILE...
                                            an independent request count (rhobound.request-count/v1) from a load
                                            balancer's logs or a metrics query, for check --request-count
`})
}

const countUsage = "usage: rhobound count --from alb|jsonl|prometheus --window START,END --entry NAME=PREFIX... --out FILE " +
	"[--source alb|apigw|ingress|loadgen] [--probe-route R] [--time-key K] [--route-key K] [--label L] [--json] FILE..."

// countExitError is the exit status of count when it cannot read its inputs or its flags: the same as check's tool
// error, since a count is an input of check.
const countExitError = 2

// countRule names an entry service for the requests whose path (or label value) starts with Prefix. The longest
// matching prefix wins; an empty prefix matches every request.
type countRule struct{ name, prefix string }

// countDoc is the request count as written: rhobound.request-count/v1 (DATA.md 6), the window as given, every named
// entry with its count (an entry nothing matched is written with 0, so that check sees the mismatch instead of a
// missing entry).
type countDoc struct {
	Schema string `json:"schema"`
	Window struct {
		Start string `json:"start"`
		End   string `json:"end"`
	} `json:"window"`
	Source  string           `json:"source"`
	Entries []countDocEntry  `json:"entries"`
	summary *countSummary    // never serialized
	digest  string           // of the written bytes
	counts  map[string]int64 // by entry
	rules   []countRule      // the --entry rules
	probes  []string         // --probe-route prefixes
	start   time.Time        // the window
	end     time.Time
	keys    map[string]string // --time-key, --route-key, --label
}

type countDocEntry struct {
	Entry string `json:"entry"`
	Count int64  `json:"count"`
}

// countSummary is what count prints: how the lines were used. It is not a registered document.
type countSummary struct {
	Schema        string           `json:"schema"`
	Source        string           `json:"source"`
	Window        string           `json:"window"`
	Files         int              `json:"files"`
	Lines         int64            `json:"lines"`
	Counted       int64            `json:"counted"`
	Probes        int64            `json:"probes"`
	Unmatched     int64            `json:"unmatched"`
	OutsideWindow int64            `json:"outside_window"`
	Skipped       int64            `json:"skipped"`
	Entries       map[string]int64 `json:"entries"`
	Out           string           `json:"out"`
	SHA256        string           `json:"sha256"`
}

// cmdCount is `rhobound count`: an independent request count for a window, from inputs outside the trace pipeline
// (DATA.md 6), written as rhobound.request-count/v1 for `check --request-count`. It reads a load balancer's access
// logs (--from alb: the ALB access log entry, one request per line, counted by the time the load balancer received
// the request), JSON lines with a time and a route key (--from jsonl: API Gateway or ingress access logs exported as
// JSON), or the JSON answer of a Prometheus instant query (--from prometheus: `sum by (LABEL) (increase(...[W]))`
// evaluated at the window's end, one entry per label value). A request is counted when its time is in [START, END),
// its path is not a probe route, and an --entry rule names its entry; the rest are reported, never silently dropped.
// The window is written as given, so give check the same --window. The count's source is declared (--source), one
// of the contract's independent sources; counters inside the Collector are not independent (DATA.md 6).
func cmdCount(args []string, out, errw io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(errw, "rhobound:", err)
		return countExitError
	}
	fs := flag.NewFlagSet("count", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "the input's form: alb, jsonl or prometheus")
	window := fs.String("window", "", "START,END (RFC 3339): the window the count is for")
	outPath := fs.String("out", "", "the request-count file to write")
	source := fs.String("source", "", "the count's source: alb, apigw, ingress or loadgen (default alb for --from alb)")
	timeKey := fs.String("time-key", "", "jsonl: the key of a request's time (RFC 3339, epoch seconds or milliseconds, or the CLF form)")
	routeKey := fs.String("route-key", "path", "jsonl: the key of a request's path or route")
	label := fs.String("label", "", "prometheus: the label whose value names the entry")
	asJSON := fs.Bool("json", false, "print the summary as JSON")
	var entries, probes listFlag
	fs.Var(&entries, "entry", "NAME=PREFIX: count requests whose path starts with PREFIX under the entry NAME (repeatable; the longest prefix wins)")
	fs.Var(&probes, "probe-route", "a path prefix that is a probe, left out of the count (repeatable)")
	files, err := parseInterleaved(fs, args)
	if err != nil || len(files) == 0 || *from == "" || *window == "" || *outPath == "" || len(entries) == 0 {
		return fail(errors.New(countUsage))
	}
	doc := &countDoc{Schema: check.RequestCountSchema, counts: map[string]int64{}, keys: map[string]string{}}
	s, e, ok := strings.Cut(*window, ",")
	var err1, err2 error
	doc.start, err1 = time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	doc.end, err2 = time.Parse(time.RFC3339Nano, strings.TrimSpace(e))
	if !ok || err1 != nil || err2 != nil || !doc.end.After(doc.start) {
		return fail(fmt.Errorf("--window %q is not START,END in RFC 3339 with END after START", *window))
	}
	doc.Window.Start, doc.Window.End = doc.start.UTC().Format(time.RFC3339Nano), doc.end.UTC().Format(time.RFC3339Nano)
	switch *from {
	case "alb":
		if *source == "" {
			*source = "alb"
		}
	case "jsonl":
		if *timeKey == "" || *routeKey == "" {
			return fail(errors.New("--from jsonl needs --time-key and --route-key"))
		}
	case "prometheus":
		if *label == "" {
			return fail(errors.New("--from prometheus needs --label, the label whose value names the entry"))
		}
	default:
		return fail(fmt.Errorf("--from %q is not alb, jsonl or prometheus", *from))
	}
	switch *source {
	case "alb", "apigw", "ingress", "loadgen":
		doc.Source = *source
	case "":
		return fail(errors.New("--source is required: alb, apigw, ingress or loadgen (the count must come from outside the trace pipeline, DATA.md 6)"))
	default:
		return fail(fmt.Errorf("--source %q is not alb, apigw, ingress or loadgen", *source))
	}
	for _, r := range entries {
		name, prefix, ok := strings.Cut(r, "=")
		if !ok || name == "" {
			return fail(fmt.Errorf("--entry %q is not NAME=PREFIX", r))
		}
		if err := checkName("entry", name); err != nil {
			return fail(err)
		}
		doc.rules = append(doc.rules, countRule{name: name, prefix: prefix})
		doc.counts[name] += 0
	}
	sort.SliceStable(doc.rules, func(i, j int) bool { return len(doc.rules[i].prefix) > len(doc.rules[j].prefix) })
	doc.probes = probes
	doc.keys["time"], doc.keys["route"], doc.keys["label"] = *timeKey, *routeKey, *label
	sum := &countSummary{Schema: "rhobound-cli.count-summary/v1", Source: doc.Source, Window: *window, Files: len(files), Entries: doc.counts, Out: *outPath}
	doc.summary = sum
	for _, f := range files {
		if err := doc.readFile(*from, f); err != nil {
			return fail(err)
		}
	}
	for name, n := range doc.counts {
		doc.Entries = append(doc.Entries, countDocEntry{Entry: name, Count: n})
	}
	sort.Slice(doc.Entries, func(i, j int) bool { return doc.Entries[i].Entry < doc.Entries[j].Entry })
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fail(err)
	}
	b = append(b, '\n')
	// the document must be one check accepts: the same parser, on the same bytes
	if _, err := check.ParseRequestCount(b); err != nil {
		return fail(fmt.Errorf("the count is not a valid request count: %w", err))
	}
	if err := os.WriteFile(*outPath, b, 0o644); err != nil {
		return fail(err)
	}
	sum.SHA256 = strings.TrimPrefix(contract.Digest(b), "sha256:")
	if sum.Unmatched > 0 {
		fmt.Fprintf(errw, "rhobound: %d requests in the window matched no --entry rule and were not counted\n", sum.Unmatched)
	}
	if sum.Skipped > 0 {
		fmt.Fprintf(errw, "rhobound: %d lines could not be read as %s entries\n", sum.Skipped, *from)
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(sum)
		return 0
	}
	fmt.Fprintf(out, "count: %d requests in %s (%s), written to %s\n", sum.Counted, *window, doc.Source, *outPath)
	for _, e := range doc.Entries {
		fmt.Fprintf(out, "  %s %d\n", e.Entry, e.Count)
	}
	fmt.Fprintf(out, "lines %d: counted %d, probes %d, unmatched %d, outside the window %d, skipped %d\n",
		sum.Lines, sum.Counted, sum.Probes, sum.Unmatched, sum.OutsideWindow, sum.Skipped)
	return 0
}

// readFile counts one input file ("-" is standard input; a .gz file, or one that starts with the gzip magic, is
// inflated).
func (d *countDoc) readFile(from, name string) error {
	var r io.Reader = os.Stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	br := bufio.NewReaderSize(r, 1<<20)
	if head, err := br.Peek(2); err == nil && len(head) == 2 && head[0] == 0x1f && head[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer gz.Close()
		br = bufio.NewReaderSize(gz, 1<<20)
	}
	if from == "prometheus" {
		b, err := io.ReadAll(br)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return d.readPrometheus(name, b)
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		d.summary.Lines++
		var t time.Time
		var path string
		var ok bool
		switch from {
		case "alb":
			t, path, ok = albEntry(line)
		case "jsonl":
			t, path, ok = jsonlEntry(line, d.keys["time"], d.keys["route"])
		}
		if !ok {
			d.summary.Skipped++
			continue
		}
		d.add(t, path, 1)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// add counts one request (or n of them) at time t with the path or label value key.
func (d *countDoc) add(t time.Time, key string, n int64) {
	if t.Before(d.start) || !t.Before(d.end) {
		d.summary.OutsideWindow += n
		return
	}
	for _, p := range d.probes {
		if p != "" && strings.HasPrefix(key, p) {
			d.summary.Probes += n
			return
		}
	}
	for _, r := range d.rules {
		if strings.HasPrefix(key, r.prefix) {
			d.counts[r.name] += n
			d.summary.Counted += n
			return
		}
	}
	d.summary.Unmatched += n
}

// albEntry reads one line of an Application Load Balancer access log: space-separated fields, some quoted. The
// request is the 13th field (`"GET https://host:443/path?query HTTP/1.1"`), the time the load balancer received
// the request the 22nd (request_creation_time); older entries without it are counted by the 2nd field, the time of
// the response. Both are ISO 8601.
func albEntry(line string) (time.Time, string, bool) {
	f := splitQuoted(line)
	if len(f) < 13 {
		return time.Time{}, "", false
	}
	var t time.Time
	var err error
	if len(f) > 21 {
		t, err = time.Parse(time.RFC3339Nano, f[21])
	}
	if len(f) <= 21 || err != nil {
		if t, err = time.Parse(time.RFC3339Nano, f[1]); err != nil {
			return time.Time{}, "", false
		}
	}
	return t, requestPath(f[12]), true
}

// requestPath is the path of an access log's request field, "METHOD URL PROTOCOL": the URL's path, "" when the field
// is a placeholder ("-" or "- - -").
func requestPath(req string) string {
	parts := strings.Fields(req)
	if len(parts) < 2 || parts[1] == "-" {
		return ""
	}
	if u, err := url.Parse(parts[1]); err == nil && u.Path != "" {
		return u.Path
	}
	return parts[1]
}

// splitQuoted splits a log line on spaces, keeping a double-quoted field as one field without its quotes.
func splitQuoted(line string) []string {
	var out []string
	var cur strings.Builder
	quoted, any := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
			quoted = !quoted
			any = true
		case c == ' ' && !quoted:
			if any {
				out = append(out, cur.String())
				cur.Reset()
				any = false
			}
		default:
			cur.WriteByte(c)
			any = true
		}
	}
	if any {
		out = append(out, cur.String())
	}
	return out
}

// jsonlEntry reads one JSON object with a time under timeKey and a path or route under routeKey. The time is RFC
// 3339, the CLF form API Gateway's $context.requestTime uses ("09/Apr/2015:12:34:56 +0000"), or a number: epoch
// seconds, milliseconds, microseconds or nanoseconds by its size. A missing route is "" (counted only by a rule with
// an empty prefix).
func jsonlEntry(line, timeKey, routeKey string) (time.Time, string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &obj); err != nil {
		return time.Time{}, "", false
	}
	raw, ok := obj[timeKey]
	if !ok {
		return time.Time{}, "", false
	}
	t, ok := parseLogTime(raw)
	if !ok {
		return time.Time{}, "", false
	}
	var route string
	if r, ok := obj[routeKey]; ok {
		if err := json.Unmarshal(r, &route); err != nil {
			route = strings.Trim(string(r), `"`)
		}
	}
	return t, route, true
}

// parseLogTime reads a JSON time value: a string in RFC 3339 or the CLF form, or a number of epoch seconds (with a
// fraction), milliseconds, microseconds or nanoseconds, told apart by their size.
func parseLogTime(raw json.RawMessage) (time.Time, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t, true
		}
		if t, err := time.Parse("02/Jan/2006:15:04:05 -0700", s); err == nil {
			return t, true
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return epochTime(n), true
		}
		return time.Time{}, false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return time.Time{}, false
	}
	return epochTime(n), true
}

// epochTime reads an epoch number as seconds, milliseconds, microseconds or nanoseconds by its magnitude (a value
// under 1e11 is seconds: that is the year 5138).
func epochTime(n float64) time.Time {
	switch {
	case n < 1e11:
		sec, frac := math.Modf(n)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC()
	case n < 1e14:
		return time.UnixMilli(int64(n)).UTC()
	case n < 1e17:
		return time.UnixMicro(int64(n)).UTC()
	default:
		return time.Unix(0, int64(n)).UTC()
	}
}

// readPrometheus reads the JSON answer of an instant query whose result is a vector: one count per label value,
// rounded to the nearest request (increase() is a float), at the window given (the query, evaluated at END over the
// window's length, is the caller's; the file carries no window of its own).
func (d *countDoc) readPrometheus(name string, b []byte) error {
	var q struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &q); err != nil {
		return fmt.Errorf("%s: not a Prometheus query answer: %w", name, err)
	}
	if q.Status != "success" || q.Data.ResultType != "vector" {
		return fmt.Errorf("%s: the query answer must have status success and resultType vector (an instant query), got %q and %q",
			name, q.Status, q.Data.ResultType)
	}
	label := d.keys["label"]
	for _, r := range q.Data.Result {
		d.summary.Lines++
		if len(r.Value) != 2 {
			d.summary.Skipped++
			continue
		}
		var vs string
		if err := json.Unmarshal(r.Value[1], &vs); err != nil {
			d.summary.Skipped++
			continue
		}
		v, err := strconv.ParseFloat(vs, 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			d.summary.Skipped++
			continue
		}
		key, ok := r.Metric[label]
		if !ok {
			d.summary.Skipped++
			continue
		}
		// the sample's own time is the evaluation time, inside the window by construction
		d.add(d.start, key, int64(math.Round(v)))
	}
	return nil
}
