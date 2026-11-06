// SPDX-License-Identifier: Apache-2.0

package otlp

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// The span keeps its size: the new fields live in a table it points into, not in every span.
func TestSpanSize(t *testing.T) {
	if s := unsafe.Sizeof(Span{}); s != 72 {
		t.Errorf("a Span is %d bytes, was 72 at 1a2df09: memory per span grew", s)
	}
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func refsOf(t *testing.T, dir string, names ...string) []FileRef {
	var out []FileRef
	for _, n := range names {
		p := filepath.Join(dir, n)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, FileRef{Path: p, SHA256: shaHex(b)})
	}
	return out
}

func TestReadFilesSHA256(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jsonl.gz", string(gz(t, []byte(req+"\n"))))
	write(t, dir, "b.jsonl", req+"\n")
	refs := refsOf(t, dir, "b.jsonl", "a.jsonl.gz")
	d, err := ReadFiles(refs, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spans) != 4 || d.Files[0] != refs[0].Path {
		t.Fatalf("spans %d, files %v: the listed files in the listed order", len(d.Spans), d.Files)
	}
	refs[1].SHA256 = strings.Repeat("0", 64)
	_, err = ReadFiles(refs, DefaultLimits)
	var e *Error
	if !errors.As(err, &e) || e.Code != contract.StoreManifestMismatch || e.Path != refs[1].Path {
		t.Fatalf("a changed file: %v", err)
	}
}

// The two decoder paths of a bad value both carry trace.bad_json, the file and the line or value index, and print what
// rhobound-cli printed.
func TestBadJSONBothPaths(t *testing.T) {
	dir := t.TempDir()
	// a .json file that does not split into values
	write(t, dir, "split.json", req+"\n{\"resourceSpans\": [\n")
	_, err := ReadFiles(refsOf(t, dir, "split.json"), DefaultLimits)
	var e *Error
	if !errors.As(err, &e) || e.Code != contract.TraceBadJSON || e.Value != 1 || e.Line != 0 ||
		err.Error() != filepath.Join(dir, "split.json")+": unexpected EOF" {
		t.Errorf("split error: %#v %q", e, err)
	}
	// a .jsonl line that is JSON but not the OTLP shape
	write(t, dir, "shape.jsonl", req+"\n"+`{"resourceSpans":"x"}`+"\n")
	_, err = ReadFiles(refsOf(t, dir, "shape.jsonl"), DefaultLimits)
	if !errors.As(err, &e) || e.Code != contract.TraceBadJSON || e.Line != 2 || e.Path != filepath.Join(dir, "shape.jsonl") ||
		!strings.HasPrefix(err.Error(), "bad OTLP/JSON: ") {
		t.Errorf("value error: %#v %q", e, err)
	}
	// a .jsonl line that is not JSON at all, on ReadDir's path: the message of 1a2df09
	only := t.TempDir()
	write(t, only, "x.jsonl", "{not json}\n")
	_, err = ReadDir(only)
	if err == nil || !strings.HasPrefix(err.Error(), "bad OTLP/JSON: invalid character") {
		t.Errorf("not JSON: %v", err)
	}
	// a string kind is a type error of the value
	write(t, dir, "kind.jsonl", strings.Replace(req, `"kind":2`, `"kind":"SPAN_KIND_SERVER"`, 1)+"\n")
	_, err = ReadFiles(refsOf(t, dir, "kind.jsonl"), DefaultLimits)
	if !errors.As(err, &e) || e.Code != contract.TraceBadJSON || e.Line != 1 {
		t.Errorf("string kind: %v", err)
	}
}

func TestCompressionMismatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jsonl.gz", req+"\n")
	_, err := ReadDir(dir)
	var e *Error
	if !errors.As(err, &e) || e.Code != contract.StoreCompressionMismatch || err.Error() != filepath.Join(dir, "a.jsonl.gz")+": gzip: invalid header" {
		t.Errorf("not gzip: %v", err)
	}
	// a gzip stream cut short
	full := gz(t, []byte(strings.Repeat(req+"\n", 50)))
	write(t, dir, "a.jsonl.gz", string(full[:len(full)/2]))
	if _, err := ReadDir(dir); !errors.As(err, &e) || e.Code != contract.StoreCompressionMismatch {
		t.Errorf("a cut gzip stream: %v", err)
	}
}

func TestPartialTail(t *testing.T) {
	dir := t.TempDir()
	cut := req[:len(req)/2]
	write(t, dir, "a.jsonl", req+"\n")
	write(t, dir, "b.jsonl", req+"\n"+cut)
	d, err := ReadFiles(refsOf(t, dir, "a.jsonl", "b.jsonl"), DefaultLimits)
	if err != nil || d.Stats.PartialTail != 1 || len(d.Spans) != 4 {
		t.Fatalf("ReadFiles: %v, partial tail %d", err, d.Stats.PartialTail)
	}
	// a complete last line without its newline is read
	write(t, dir, "c.jsonl", req+"\n"+req)
	if d, err := ReadFiles(refsOf(t, dir, "c.jsonl"), DefaultLimits); err != nil || d.Stats.PartialTail != 0 || len(d.Spans) != 4 {
		t.Errorf("complete last line: %v", err)
	}
	// only in the last file: elsewhere it is corruption
	if _, err := ReadFiles(refsOf(t, dir, "b.jsonl", "a.jsonl"), DefaultLimits); err == nil {
		t.Error("a partial line in a file that is not the last is accepted")
	}
	// ReadDir keeps refusing it, as rhobound-cli did
	if _, err := ReadDir(dir); err == nil || !strings.HasPrefix(err.Error(), "bad OTLP/JSON") {
		t.Errorf("ReadDir: %v", err)
	}
}

func TestLimits(t *testing.T) {
	isLimit := func(err error) bool {
		var le *LimitError
		return errors.As(err, &le) && le.Code == contract.InputTooLarge
	}
	dir := t.TempDir()
	// a 1 MB gzip that inflates far past the object cap
	big := gz(t, bytes.Repeat([]byte(" "), 200<<20))
	if len(big) > 1<<20 {
		t.Fatalf("the gzip is %d bytes", len(big))
	}
	write(t, dir, "bomb.jsonl.gz", string(big))
	lim := DefaultLimits
	lim.MaxObjectBytes = 64 << 20
	if _, err := ReadFiles(refsOf(t, dir, "bomb.jsonl.gz"), lim); !isLimit(err) {
		t.Errorf("gzip bomb: %v", err)
	}
	// a 100 MB line, under the default limits
	line := gz(t, append(append([]byte(`{"resourceSpans":[],"x":"`), bytes.Repeat([]byte("a"), 100<<20)...), []byte("\"}\n")...))
	write(t, dir, "line.jsonl.gz", string(line))
	if _, err := ReadFiles(refsOf(t, dir, "line.jsonl.gz"), DefaultLimits); !isLimit(err) || !strings.Contains(err.Error(), "line 1") {
		t.Errorf("a 100 MB line: %v", err)
	}
	// the capture's total, over two files each under the object cap
	write(t, dir, "a.jsonl", req+"\n")
	write(t, dir, "b.jsonl", req+"\n")
	lim = DefaultLimits
	lim.MaxCaptureBytes = int64(len(req)) + 10
	if _, err := ReadFiles(refsOf(t, dir, "a.jsonl", "b.jsonl"), lim); !isLimit(err) || !strings.Contains(err.Error(), "capture") {
		t.Errorf("capture total: %v", err)
	}
	// too many spans
	write(t, dir, "many.jsonl", strings.Repeat(req+"\n", 40))
	lim = DefaultLimits
	lim.MaxSpans = 50
	if _, err := ReadFiles(refsOf(t, dir, "many.jsonl"), lim); !isLimit(err) || !strings.Contains(err.Error(), "spans") {
		t.Errorf("too many spans: %v", err)
	}
	// a kept attribute value over its limit; an ignored one is not held against the capture
	long := strings.Repeat("x", 5000)
	write(t, dir, "attr.jsonl", strings.Replace(req, `"slow.example"`, `"`+long+`"`, 1)+"\n")
	if _, err := ReadFiles(refsOf(t, dir, "attr.jsonl"), DefaultLimits); !isLimit(err) || !strings.Contains(err.Error(), "http.host") {
		t.Errorf("long kept attribute: %v", err)
	}
	free := strings.Replace(req, `"key":"http.host","value":{"stringValue":"slow.example"}`, `"key":"db.statement","value":{"stringValue":"`+long+`"}`, 1)
	write(t, dir, "free.jsonl", free+"\n")
	if d, err := ReadFiles(refsOf(t, dir, "free.jsonl"), DefaultLimits); err != nil || d.Ignored["span attribute"] != 1 {
		t.Errorf("long ignored attribute: %v %v", err, d.Ignored)
	}
	// a long path is looked at, not kept: it is not held against the capture either
	write(t, dir, "path.jsonl", strings.Replace(req, `"key":"http.host","value":{"stringValue":"slow.example"}`,
		`"key":"http.url","value":{"stringValue":"https://shop.example/search?q=`+long+`"}`, 1)+"\n")
	if _, err := ReadFiles(refsOf(t, dir, "path.jsonl"), DefaultLimits); err != nil {
		t.Errorf("a long URL: %v", err)
	}
	// a .json value over the line limit
	write(t, dir, "v.json", req)
	lim = DefaultLimits
	lim.MaxLineBytes = 100
	if _, err := ReadFiles(refsOf(t, dir, "v.json"), lim); !isLimit(err) || !strings.Contains(err.Error(), "value 0") {
		t.Errorf("a long .json value: %v", err)
	}
}

// ReadDir is report v1's reader (predict CAPTURE, infer): it reads a capture whole, as rhobound-cli did, under none of
// the limits, whatever DefaultLimits holds; check and fit read through ReadFiles under them.
func TestReadDirHasNoLimits(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jsonl.gz", string(gz(t, []byte(req+"\n"+req+"\n"))))
	write(t, dir, "b.jsonl", req+"\n")
	want, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := DefaultLimits
	defer func() { DefaultLimits = saved }()
	DefaultLimits = Limits{MaxObjectBytes: 1, MaxCaptureBytes: 1, MaxLineBytes: 1, MaxSpans: 1, MaxAttrBytes: 1}
	if _, err := ReadFiles(refsOf(t, dir, "a.jsonl.gz", "b.jsonl"), DefaultLimits); err == nil {
		t.Fatal("ReadFiles reads past limits of one byte and one span")
	}
	got, err := ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir under limits of one byte and one span: %v", err)
	}
	if got.Stats.SpansRead != want.Stats.SpansRead || len(got.Spans) != len(want.Spans) || len(got.Resources) != len(want.Resources) {
		t.Errorf("ReadDir read %d spans (%d kept, %d resources), want %d (%d, %d)", got.Stats.SpansRead, len(got.Spans),
			len(got.Resources), want.Stats.SpansRead, len(want.Spans), len(want.Resources))
	}
}

const extReq = `{"resourceSpans":[{"resource":{"attributes":[` +
	`{"key":"service.name","value":{"stringValue":"api"}},{"key":"service.namespace","value":{"stringValue":"shop"}},` +
	`{"key":"k8s.pod.uid","value":{"stringValue":"0a1b"}},{"key":"host.name","value":{"stringValue":"node-1"}},` +
	`{"key":"service.instance.id","value":{"stringValue":"proc-1"}},{"key":"deployment.environment.name","value":{"stringValue":"prod"}},` +
	`{"key":"container.image.repo_digests","value":{"arrayValue":{"values":[{"stringValue":"reg/api@sha256:bb"},{"stringValue":"reg/api@sha256:aa"}]}}},` +
	`{"key":"faas.name","value":{"stringValue":"fn"}},{"key":"team","value":{"stringValue":"x"}}]},` +
	`"schemaUrl":"https://opentelemetry.io/schemas/1.21.0","scopeSpans":[{"scope":{"name":"s","version":"1"},"spans":[` +
	`{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1111111111111111","kind":2,"traceState":"ot=th:c;rv:01,vendor=x",` +
	`"startTimeUnixNano":"1.7e18","endTimeUnixNano":"1700000000010000000","status":{"code":"STATUS_CODE_ERROR"},` +
	`"events":[{"name":"rpc.message","attributes":[{"key":"rpc.message.type","value":{"stringValue":"SENT"}}]},` +
	`{"name":"rpc.message","attributes":[{"key":"message.type","value":{"stringValue":"SENT"}}]},{"name":"exception"}],` +
	`"attributes":[{"key":"http.route","value":{"stringValue":"/orders/{id}"}},{"key":"http.response.status_code","value":{"intValue":"503"}},` +
	`{"key":"user_agent.original","value":{"stringValue":"kube-probe/1.29"}},{"key":"url.path","value":{"stringValue":"/q/health/ready"}}]},` +
	`{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"2222222222222222","parentSpanId":"1111111111111111","kind":5,` +
	`"startTimeUnixNano":"1700000000001000000","endTimeUnixNano":"1700000000002000000","links":[{"traceId":"aa"},{"traceId":"bb"}],` +
	`"droppedAttributesCount":3,"flags":256,"status":"weird","attributes":[{"key":"http.request.resend_count","value":{"intValue":2}},` +
	`{"key":"url.path","value":{"stringValue":"/u/jane.doe@example.com"}}]},` +
	`{"traceId":"AQIDBAUGBwgJCgsMDQ4PEA==","spanId":"ERERERERERE=","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2"},` +
	`{"traceId":"0102030405060708","spanId":"3333333333333333","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2"},` +
	`{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"4444444444444444","parentSpanId":"zz","kind":1,` +
	`"startTimeUnixNano":"1700000000001000000","endTimeUnixNano":"1700000000002000000"}]}]}]}`

// TestSkippedExamples: an id that is not written as an id of any encoding (text a capture's writer put in an id field,
// or none) is shown in a skipped span's example by its class, "other", never as written; one that is, as written.
func TestSkippedExamples(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jsonl", `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"a"}}]},`+
		`"scopeSpans":[{"spans":[`+
		`{"traceId":"198.51.100.77","spanId":"canary-sid-00","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2"},`+
		`{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2"},`+
		`{"traceId":"acct 111122223333","spanId":"1111111111111111","kind":2,"startTimeUnixNano":"1","endTimeUnixNano":"2"}]}]}]}`+"\n")
	d, err := ReadFiles(refsOf(t, dir, "a.jsonl"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"other:other", "0102030405060708090a0b0c0d0e0f10:other", "other:1111111111111111"}
	if d.Skipped != 3 || !slices.Equal(d.Stats.SkippedExamples, want) {
		t.Errorf("skipped %d, examples %q, want %q", d.Skipped, d.Stats.SkippedExamples, want)
	}
}

func TestNewFields(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.jsonl", extReq+"\n")
	d, err := ReadFiles(refsOf(t, dir, "a.jsonl"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Spans) != 3 || d.Skipped != 2 || d.Stats.SkippedIDs != 2 || d.Stats.IDsBase64 != 1 || d.Stats.TraceIDs64 != 1 ||
		d.Stats.BadParents != 1 || d.Stats.FloatTimes != 1 || d.Stats.IDsHex != 3 || d.Stats.SpansRead != 5 {
		t.Errorf("spans %d, skipped %d, stats %+v", len(d.Spans), d.Skipped, d.Stats)
	}
	// a skipped span's ids are its examples: each as written when it is written as an id (base64, a 16-digit trace id)
	if want := []string{"AQIDBAUGBwgJCgsMDQ4PEA==:ERERERERERE=", "0102030405060708:3333333333333333"}; !slices.Equal(d.Stats.SkippedExamples, want) {
		t.Errorf("skipped examples %q, want %q", d.Stats.SkippedExamples, want)
	}
	if len(d.Resources) != 1 || d.Resources[0].Pod != "0a1b" {
		t.Fatalf("resources %+v: a pod uid is the pod when there is no pod name", d.Resources)
	}
	ri := d.Info[0]
	if ri.Namespace != "shop" || ri.PodUID != "0a1b" || ri.PodName != "" || !ri.PlatformIdentity() || ri.Hosts[0] != "node-1" ||
		ri.InstanceIDs[0] != "proc-1" || ri.Environments[0] != "prod" || !ri.FaaS ||
		fmt.Sprint(ri.RepoDigests) != "[reg/api@sha256:aa reg/api@sha256:bb]" {
		t.Errorf("info %+v", ri)
	}
	byID := map[uint64]SpanExt{}
	for i := range d.Spans {
		byID[d.Spans[i].ID] = d.ExtOf(&d.Spans[i])
	}
	srv := byID[0x1111111111111111]
	want := SpanExt{Route: "/orders/{id}", TH: "c", Status: 2, HTTPStatus: 503, UserAgent: UAKubeProbe, ProbePath: true, MsgSent: 2}
	if srv != want {
		t.Errorf("server ext %+v, want %+v", srv, want)
	}
	cons := byID[0x2222222222222222]
	if cons.Links != 2 || !cons.Dropped || cons.Resends != 2 || !cons.Personal || cons.Status != 0 {
		t.Errorf("consumer ext %+v", cons)
	}
	if byID[0x4444444444444444] != (SpanExt{}) {
		t.Errorf("a plain span carries %+v", byID[0x4444444444444444])
	}
	if len(d.Stats.PersonalExamples) != 1 || d.Stats.PersonalExamples[0] != "0102030405060708090a0b0c0d0e0f10" {
		t.Errorf("personal examples %v (trace ids, never the value)", d.Stats.PersonalExamples)
	}
	for _, k := range []string{"resource attribute team", "schemaUrl", "scope version", "span flags", "span droppedAttributesCount"} {
		if d.Ignored[k] != 1 {
			t.Errorf("ignored %q: %d (%v)", k, d.Ignored[k], d.Ignored)
		}
	}
}

// The pod is k8s.pod.name, else k8s.pod.uid, else aws.ecs.task.arn's task id (the ARN's last element, without the
// account, region and cluster it names), else service.instance.id, else host.name: the order of rhobound-cli for every
// resource without a platform identity.
func TestPodIdentityOrder(t *testing.T) {
	account := strings.Repeat("4", 12)
	res := func(kv ...string) string {
		var a []string
		a = append(a, `{"key":"service.name","value":{"stringValue":"s"}}`)
		for i := 0; i < len(kv); i += 2 {
			a = append(a, fmt.Sprintf(`{"key":%q,"value":{"stringValue":%q}}`, kv[i], kv[i+1]))
		}
		return `{"resourceSpans":[{"resource":{"attributes":[` + strings.Join(a, ",") + `]},"scopeSpans":[{"spans":[` +
			`{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1111111111111111","kind":2,` +
			`"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000010000000"}]}]}]}`
	}
	for _, c := range []struct {
		kv  []string
		pod string
	}{
		{[]string{"host.name", "h", "service.instance.id", "i", "k8s.pod.name", "p", "k8s.pod.uid", "u", "aws.ecs.task.arn", "t"}, "p"},
		{[]string{"host.name", "h", "service.instance.id", "i", "k8s.pod.uid", "u", "aws.ecs.task.arn", "t"}, "u"},
		{[]string{"host.name", "h", "service.instance.id", "i", "aws.ecs.task.arn", "t"}, "t"},
		{[]string{"host.name", "h", "aws.ecs.task.arn", "arn:aws:ecs:us-east-1:" + account + ":task/shop/0f1e2d3c4b5a6978"}, "0f1e2d3c4b5a6978"},
		{[]string{"aws.ecs.task.arn", "arn:aws:ecs:us-east-1:" + account + ":task/0f1e2d3c4b5a6978"}, "0f1e2d3c4b5a6978"},
		{[]string{"service.instance.id", "i", "aws.ecs.task.arn", "arn:aws:ecs:us-east-1:" + account + ":task/"}, "i"},
		{[]string{"host.name", "h", "service.instance.id", "i"}, "i"},
		{[]string{"host.name", "h"}, "h"},
		{nil, ""},
	} {
		dir := t.TempDir()
		write(t, dir, "a.jsonl", res(c.kv...)+"\n")
		d, err := ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if d.Resources[0].Pod != c.pod {
			t.Errorf("%v: pod %q, want %q", c.kv, d.Resources[0].Pod, c.pod)
		}
	}
}

// Resources that differ only in what the model does not read stay one Resource, as at 1a2df09, and their info is the
// union.
func TestResourceInfoUnion(t *testing.T) {
	line := func(inst, digest string) string {
		return `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"s"}},` +
			`{"key":"k8s.pod.name","value":{"stringValue":"s-0"}},{"key":"service.instance.id","value":{"stringValue":"` + inst + `"}},` +
			`{"key":"container.image.repo_digests","value":{"arrayValue":{"values":[{"stringValue":"` + digest + `"}]}}}]},` +
			`"scopeSpans":[{"spans":[{"traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1111111111111111","kind":2,` +
			`"startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000010000000"}]}]}]}` + "\n"
	}
	dir := t.TempDir()
	write(t, dir, "a.jsonl", line("p1", "r@sha256:1")+line("p2", "r@sha256:2")+line("p1", "r@sha256:1"))
	d, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Resources) != 1 || len(d.Info) != 1 || fmt.Sprint(d.Info[0].InstanceIDs) != "[p1 p2]" ||
		fmt.Sprint(d.Info[0].RepoDigests) != "[r@sha256:1 r@sha256:2]" {
		t.Errorf("resources %+v info %+v", d.Resources, d.Info)
	}
}

func TestIDEncoding(t *testing.T) {
	for _, c := range []struct {
		s     string
		bytes int
		want  string
	}{
		{"0102030405060708090a0b0c0d0e0f10", 16, "hex"}, {"0102030405060708090A0B0C0D0E0F10", 16, "hex"},
		{"AQIDBAUGBwgJCgsMDQ4PEA==", 16, "base64"}, {"0102030405060708", 16, "hex64"}, {"1111111111111111", 8, "hex"},
		{"ERERERERERE=", 8, "base64"}, {"zz", 16, "other"}, {"", 8, "other"}, {"AQIDBAUGBwgJCgsMDQ4P=A=", 16, "other"},
	} {
		if got := IDEncoding(c.s, c.bytes); got != c.want {
			t.Errorf("IDEncoding(%q, %d) = %s, want %s", c.s, c.bytes, got, c.want)
		}
	}
}

func TestPersonalKind(t *testing.T) {
	for s, want := range map[string]string{
		"/u/jane.doe@example.com/cart": "email", "Bearer eyJhbGciOi.eyJzdWIiOiIx.c2ln": "jwt", "/pay/4111111111111111": "card",
		"/pay/4111111111111112": "", "/orders/{id}": "", "kube-probe/1.29": "", "/a/1234567890123": "",
	} {
		if got := PersonalKind(s); got != want {
			t.Errorf("PersonalKind(%q) = %q, want %q", s, got, want)
		}
	}
	if got := MaskPersonal("/u/jane.doe@example.com/pay/4111111111111111/x"); got != "/u/[masked]/pay/[masked]/x" {
		t.Errorf("masked %q", got)
	}
}

func TestWideProbe(t *testing.T) {
	for r, want := range map[string]bool{"GET /actuator/health": true, "/actuator/health/liveness": true,
		"GET /q/health/ready": true, "/health/live": true, "GET /api/health": true, "GET /metrics": true,
		"GET /healthz": false, "GET /orders": false, "/metrics/x": false} {
		if got := WideProbe(r); got != want {
			t.Errorf("WideProbe(%q) = %v", r, got)
		}
	}
}

// The canonical keep-list holds every attribute key the reader reads: every dotted string in this package's code is
// either in it or one of the few keys that are not attribute keys.
func TestKeepListComplete(t *testing.T) {
	k := Keep()
	notAttributes := map[string]bool{
		"grpc.health.v1.Health": true, // in the probe rule's pattern, not a key
	}
	dotted := regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !dotted.MatchString(s) || notAttributes[s] || contract.Pattern.MatchString(s) && isCode(s) {
				return true
			}
			found++
			if s == eventName {
				return true
			}
			if !k.KeepsResource(s) && !k.KeepsSpan(s) && !k.KeepsEventKey(s) && s != strings.TrimSuffix(faasPrefix, ".") {
				t.Errorf("%s: the reader reads %q, which the keep-list lacks", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
	if found < 40 {
		t.Errorf("found %d keys in the reader; the scan is broken", found)
	}
	// the keep-list of DATA.md 15
	for _, key := range []string{"service.name", "service.namespace", "service.version", "service.instance.id",
		"deployment.environment.name", "deployment.environment", "host.name", "host.arch", "k8s.pod.name", "k8s.pod.uid",
		"k8s.deployment.name", "k8s.namespace.name", "k8s.node.name", "k8s.container.name", "aws.ecs.task.arn",
		"container.id", "container.image.name", "container.image.tag", "container.image.tags", "container.image.repo_digests",
		"telemetry.sdk.language", "telemetry.sdk.version", "faas.name"} {
		if !k.KeepsResource(key) {
			t.Errorf("resource key %s is in DATA.md 15 but not kept", key)
		}
	}
	for _, key := range []string{"http.route", "http.request.method", "http.method", "http.response.status_code",
		"http.request.resend_count", "rpc.method", "peer.service", "server.address", "net.peer.name", "http.host",
		"server.port", "net.peer.port", "url.path", "http.target", "http.path", "http.url", "user_agent.original",
		"code.function.name"} {
		if !k.KeepsSpan(key) {
			t.Errorf("span key %s is in DATA.md 15 but not kept", key)
		}
	}
	if k.SHA256() != Keep().SHA256() || len(k.SHA256()) != 64 {
		t.Error("the keep-list's digest is not stable")
	}
}

func isCode(s string) bool { _, ok := contract.Lookup(s); return ok }

// ReadDir and ReadFiles give the same spans for one capture.
func TestReadDirMatchesReadFiles(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "capture-chain")
	a, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := []FileRef{{Path: filepath.Join(dir, "traces", "spans.jsonl.gz")}}
	b, err := ReadFiles(refs, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Spans) != len(b.Spans) || len(a.Spans) != 15495 || fmt.Sprint(a.Resources) != fmt.Sprint(b.Resources) {
		t.Fatalf("ReadDir %d spans, ReadFiles %d", len(a.Spans), len(b.Spans))
	}
	for i := range a.Spans {
		if a.Spans[i] != b.Spans[i] {
			t.Fatalf("span %d differs", i)
		}
	}
}
