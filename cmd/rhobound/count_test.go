// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
)

// albLine is one ALB access log entry: the 13th field is the quoted request, the 22nd the request creation time.
func albLine(created time.Time, method, url string) string {
	resp := created.Add(12 * time.Millisecond)
	return fmt.Sprintf(`http %s app/web/50dc6c495c0c9188 203.0.113.9:2817 10.0.0.1:80 0.000 0.001 0.000 200 200 34 366 "%s %s HTTP/1.1" "curl/7.46.0" - - arn:aws:elasticloadbalancing:us-east-1:000000000000:targetgroup/web/73e2d6bc24d8a067 "Root=1-58337262-36d228ad5d99923122bbe354" "www.example.com" "-" 0 %s "forward" "-" "-" "10.0.0.1:80" "200" "-" "-" TID_0`,
		resp.Format(time.RFC3339Nano), method, url, created.Format(time.RFC3339Nano))
}

func TestCountALB(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	var lines []string
	for i := 0; i < 100; i++ { // 100 product requests in the window
		lines = append(lines, albLine(start.Add(time.Duration(i)*5*time.Second), "GET", "https://www.example.com:443/product/42?ref=x"))
	}
	for i := 0; i < 20; i++ { // 20 cart requests
		lines = append(lines, albLine(start.Add(time.Duration(i)*20*time.Second), "POST", "https://www.example.com:443/cart"))
	}
	lines = append(lines,
		albLine(start.Add(-time.Second), "GET", "https://www.example.com:443/product/1"), // before the window
		albLine(end, "GET", "https://www.example.com:443/product/1"),                     // at END: outside
		albLine(start.Add(time.Minute), "GET", "https://www.example.com:443/healthz"),    // a probe
		albLine(start.Add(time.Minute), "GET", "https://www.example.com:443/admin/x"),    // no rule
		"not an access log line",
	)
	// an old-format entry (no request_creation_time): counted by its response time
	old := start.Add(2 * time.Minute)
	lines = append(lines, fmt.Sprintf(`http %s app/web/x 203.0.113.9:2817 10.0.0.1:80 0.000 0.001 0.000 200 200 34 366 "GET https://www.example.com:443/cart HTTP/1.1" "curl" - - arn "Root=1" "www.example.com" "-" 0`, old.Format(time.RFC3339Nano)))
	log := filepath.Join(dir, "alb.log.gz")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	gz.Write([]byte(strings.Join(lines, "\n") + "\n"))
	gz.Close()
	f.Close()
	outPath := filepath.Join(dir, "request-count.json")
	window := start.Format(time.RFC3339Nano) + "," + end.Format(time.RFC3339Nano)
	out, errw, code := exec(t, "count", "--from", "alb", "--window", window, "--entry", "frontend=/product", "--entry", "cart=/cart",
		"--probe-route", "/healthz", "--out", outPath, "--json", log)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errw)
	}
	var sum countSummary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("summary: %v\n%s", err, out)
	}
	if sum.Counted != 121 || sum.Probes != 1 || sum.Unmatched != 1 || sum.OutsideWindow != 2 || sum.Skipped != 1 || sum.Lines != 126 {
		t.Errorf("summary: %+v", sum)
	}
	if sum.Entries["frontend"] != 100 || sum.Entries["cart"] != 21 {
		t.Errorf("entries: %v", sum.Entries)
	}
	if !strings.Contains(errw, "matched no --entry rule") || !strings.Contains(errw, "could not be read") {
		t.Errorf("stderr should warn about the unmatched request and the bad line:\n%s", errw)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := check.ParseRequestCount(b)
	if err != nil {
		t.Fatalf("the written count does not parse: %v\n%s", err, b)
	}
	if rc.Source != "alb" || !rc.Window.Start.Equal(start) || !rc.Window.End.Equal(end) || len(rc.Entries) != 2 ||
		rc.Entries[0].Entry != "cart" || rc.Entries[0].Count != 21 || rc.Entries[1].Entry != "frontend" || rc.Entries[1].Count != 100 {
		t.Errorf("written count: %+v", rc)
	}
	if sum.SHA256 == "" || "sha256:"+sum.SHA256 != rc.Digest {
		t.Errorf("summary sha256 %q, parsed digest %q", sum.SHA256, rc.Digest)
	}
	// the text summary
	out, _, code = exec(t, "count", "--from", "alb", "--window", window, "--entry", "frontend=/product", "--entry", "cart=/cart",
		"--probe-route", "/healthz", "--out", outPath, log)
	if code != 0 || !strings.Contains(out, "count: 121 requests") || !strings.Contains(out, "  cart 21\n") {
		t.Errorf("text summary (exit %d):\n%s", code, out)
	}
}

func TestCountJSONLAndPrometheus(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Minute)
	window := start.Format(time.RFC3339Nano) + "," + end.Format(time.RFC3339Nano)
	// API Gateway access logs as JSON lines: a CLF time, an epoch-millisecond time, an RFC 3339 time
	lines := []string{
		fmt.Sprintf(`{"requestTime":"%s","path":"/orders","status":"200"}`, start.Add(time.Minute).Format("02/Jan/2006:15:04:05 -0700")),
		fmt.Sprintf(`{"requestTime":%d,"path":"/orders/7"}`, start.Add(2*time.Minute).UnixMilli()),
		fmt.Sprintf(`{"requestTime":"%s","path":"/ping"}`, start.Add(3*time.Minute).Format(time.RFC3339)),
		fmt.Sprintf(`{"requestTime":"%s"}`, start.Add(4*time.Minute).Format(time.RFC3339)), // no route: counted only by an empty prefix
		fmt.Sprintf(`{"requestTime":"%s","path":"/orders"}`, end.Add(time.Second).Format(time.RFC3339)),
		`{"path":"/orders"}`,
	}
	log := filepath.Join(dir, "apigw.jsonl")
	if err := os.WriteFile(log, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "count.json")
	out, errw, code := exec(t, "count", "--from", "jsonl", "--source", "apigw", "--time-key", "requestTime", "--route-key", "path",
		"--window", window, "--entry", "orders=/orders", "--entry", "other=", "--probe-route", "/ping", "--out", outPath, "--json", log)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errw)
	}
	var sum countSummary
	json.Unmarshal([]byte(out), &sum)
	if sum.Entries["orders"] != 2 || sum.Entries["other"] != 1 || sum.Probes != 1 || sum.OutsideWindow != 1 || sum.Skipped != 1 || sum.Unmatched != 0 {
		t.Errorf("jsonl summary: %+v", sum)
	}
	// --source is required for jsonl, and must be an independent source
	if _, _, code := exec(t, "count", "--from", "jsonl", "--time-key", "requestTime", "--window", window, "--entry", "orders=/orders", "--out", outPath, log); code != countExitError {
		t.Errorf("jsonl without --source: exit %d, want %d", code, countExitError)
	}
	if _, _, code := exec(t, "count", "--from", "jsonl", "--source", "collector", "--time-key", "requestTime", "--window", window, "--entry", "orders=/orders", "--out", outPath, log); code != countExitError {
		t.Errorf("--source collector: exit %d, want %d", code, countExitError)
	}

	// a Prometheus instant query: sum by (route) (increase(nginx_ingress_controller_requests[10m]))
	prom := `{"status":"success","data":{"resultType":"vector","result":[
	  {"metric":{"route":"/orders"},"value":[1762430400,"1234.4"]},
	  {"metric":{"route":"/ping"},"value":[1762430400,"600"]},
	  {"metric":{"route":"/search"},"value":[1762430400,"10.6"]},
	  {"metric":{"other":"x"},"value":[1762430400,"5"]}]}}`
	pf := filepath.Join(dir, "prom.json")
	os.WriteFile(pf, []byte(prom), 0o644)
	out, errw, code = exec(t, "count", "--from", "prometheus", "--source", "ingress", "--label", "route", "--window", window,
		"--entry", "orders=/orders", "--entry", "search=/search", "--probe-route", "/ping", "--out", outPath, "--json", pf)
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, out, errw)
	}
	json.Unmarshal([]byte(out), &sum)
	if sum.Entries["orders"] != 1234 || sum.Entries["search"] != 11 || sum.Probes != 600 || sum.Skipped != 1 || sum.Counted != 1245 {
		t.Errorf("prometheus summary: %+v", sum)
	}
	b, _ := os.ReadFile(outPath)
	rc, err := check.ParseRequestCount(b)
	if err != nil || rc.Source != "ingress" || len(rc.Entries) != 2 {
		t.Errorf("written count: %v %+v", err, rc)
	}
	// a range query is not a count
	os.WriteFile(pf, []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), 0o644)
	if _, _, code := exec(t, "count", "--from", "prometheus", "--source", "ingress", "--label", "route", "--window", window, "--entry", "orders=/orders", "--out", outPath, pf); code != countExitError {
		t.Errorf("matrix answer: exit %d, want %d", code, countExitError)
	}
}

func TestCountUsage(t *testing.T) {
	for _, args := range [][]string{
		{"count"},
		{"count", "--from", "alb", "--window", "x,y", "--entry", "a=/", "--out", "o", "f"},
		{"count", "--from", "alb", "--window", "2026-11-06T12:00:00Z,2026-11-06T11:00:00Z", "--entry", "a=/", "--out", "o", "f"},
		{"count", "--from", "csv", "--window", "2026-11-06T12:00:00Z,2026-11-06T13:00:00Z", "--entry", "a=/", "--out", "o", "f"},
		{"count", "--from", "alb", "--window", "2026-11-06T12:00:00Z,2026-11-06T13:00:00Z", "--entry", "a=/", "--out", "o"},
		{"count", "--from", "alb", "--window", "2026-11-06T12:00:00Z,2026-11-06T13:00:00Z", "--entry", "bad name=/", "--out", "o", "f"},
		{"count", "--from", "alb", "--window", "2026-11-06T12:00:00Z,2026-11-06T13:00:00Z", "--entry", "a=/", "--out", "o", "/nonexistent/file"},
	} {
		if _, errw, code := exec(t, args...); code != countExitError || errw == "" {
			t.Errorf("%v: exit %d, stderr %q", args, code, errw)
		}
	}
}
