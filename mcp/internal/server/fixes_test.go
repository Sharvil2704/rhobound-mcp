// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
)

// TestBaselineVersionFix: an environment's baseline is made outside the server, so one the engine refuses as a record
// its version did not write (fit.version: another version's, or serialized again) is refused on context{env} and
// predict{env} with the server's own fix, which sends the caller neither to fit_start nor to fit again, in both
// profiles, outside stdio mode and over stdio with environments only. A fit or prediction handle refused so (after an
// engine upgrade) gets the server's text too, never the registry's operator advice (store the record's bytes): where
// its caller cannot fit (outside stdio mode, or over stdio with no capture configured) the environment's baseline, on
// the tools and rhobound://fit; over stdio where a capture is configured, fit_start or the baseline in the default
// profile, and context with the capture again or the baseline in the bench profile, which the caller can follow.
func TestBaselineVersionFix(t *testing.T) {
	refusal := runner.Reply{Exit: 1, Stdout: []byte(`{"error":{"code":"fit.version","reason":"x",` +
		`"fix":"fit again with this version, and store the record's bytes as fit wrote them"}}`)}
	for _, o := range []harnessOpt{{}, {remote: true}, {noCaptures: true}, {profile: config.ProfileBench},
		{profile: config.ProfileBench, remote: true}, {profile: config.ProfileBench, noCaptures: true}} {
		fits := !o.remote && !o.noCaptures // a caller who can fit a capture (stdio, a capture configured)
		h := newHarness(t, o)
		ctx := context.Background()
		d, _ := h.store.PutArtifact(ctx, "fit", []byte(fitDoc))
		if err := h.store.SetBaseline(ctx, "staging", d); err != nil {
			t.Fatal(err)
		}
		h.cli.replies = map[string]runner.Reply{"context": refusal, "preview": refusal}
		for tool, args := range map[string]string{"context": `{"env":"staging"}`, "predict": `{"env":"staging","changes":[]}`} {
			b, isErr := h.call(tool, args)
			fix := refusalOK(t, tool+" env", b, isErr, nil, "fit.version")
			if strings.Contains(fix, "fit_start") || strings.Contains(fix, "fit again") ||
				!strings.Contains(fix, "scheduled refresh") || (o.profile == config.ProfileBench) != strings.Contains(fix, "outside this server") {
				t.Errorf("%+v: %s env: the fix %q", o, tool, fix)
			}
			// where no capture can be fitted only context with env mints a fit handle, and the baseline it names is the
			// one refused
			if !fits && strings.Contains(fix, "fit_id") {
				t.Errorf("%+v: %s env: the fix names a fit_id the caller cannot make here: %q", o, tool, fix)
			}
		}
		if o.profile == config.ProfileBench || !fits {
			// a fit handle (one context{env} minted before an engine upgrade), and a prediction on it, are no caller's
			// to fit again with fit_start here: the server's text, on every call and resource that reads the fit
			h.cli.replies = nil
			b, isErr := h.call("context", `{"env":"staging"}`)
			if isErr {
				t.Fatalf("%+v: context env: %s", o, b)
			}
			fit := field(t, b, "fit_id").(string)
			b, isErr = h.call("predict", `{"fit_id":"`+fit+`","changes":[]}`)
			if isErr {
				t.Fatalf("%+v: predict fit_id: %s", o, b)
			}
			pred := field(t, b, "prediction_id").(string)
			h.cli.replies = map[string]runner.Reply{"context": refusal, "preview": refusal, "budget": refusal,
				"explain": refusal, "predict": refusal}
			for tool, args := range map[string]string{"context": `{"fit_id":"` + fit + `"}`,
				"predict": `{"fit_id":"` + fit + `","changes":[]}`, "budget": `{"fit_id":"` + fit + `"}`,
				"explain":            `{"target":"` + fit + `","section":"summary"}`,
				"explain prediction": `{"target":"` + pred + `","section":"summary"}`} {
				b, isErr := h.call(strings.Fields(tool)[0], args)
				fix := refusalOK(t, tool+" handle", b, isErr, nil, "fit.version")
				if strings.Contains(fix, "fit again") || strings.Contains(fix, "store the record") ||
					strings.Contains(fix, "fit_start") || !strings.Contains(fix, "environment's baseline") {
					t.Errorf("%+v: %s handle: the fix %q", o, tool, fix)
				}
				// in the bench profile over stdio, where a capture is configured, context with it makes a fit again
				if strings.Contains(fix, "context with the capture") != (o.profile == config.ProfileBench && fits) {
					t.Errorf("%+v: %s handle: the fix %q", o, tool, fix)
				}
			}
			_, err := h.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: fitURI + fit})
			if err == nil || strings.Contains(err.Error(), "fit again") || strings.Contains(err.Error(), "store the record") {
				t.Errorf("%+v: rhobound://fit: %v", o, err)
			}
			if o.profile == config.ProfileBench && fits {
				// the fix can be followed: context with a capture makes a fit handle the engine reads
				h.cli.replies = nil
				b, isErr := h.call("context", `{"capture":"loop"}`)
				if fit, _ := field(t, b, "fit_id").(string); isErr || fit == "" {
					t.Errorf("%+v: context capture after fit.version: %s", o, b)
				}
			}
			continue
		}
		h.cli.replies = nil
		fit := h.startFit(`{"capture":"loop"}`)
		h.cli.replies = map[string]runner.Reply{"context": refusal}
		b, isErr := h.call("context", `{"fit_id":"`+fit+`"}`)
		if fix := refusalOK(t, "context fit_id", b, isErr, nil, "fit.version"); !strings.Contains(fix, "fit_start") ||
			strings.Contains(fix, "store the record") || !strings.Contains(fix, "environment's baseline") {
			t.Errorf("context fit_id over stdio: the fix %q", fix)
		}
	}
}

// TestAmbiguousServiceFix: change.ambiguous_service's registry fix names the service as the traces wrote it, which the
// server never serves and its input schemas refuse, so predict, budget and a fit job's refusal give the server's own
// text instead, in the server's profile.
func TestAmbiguousServiceFix(t *testing.T) {
	const registryFix = "name the service as the traces wrote it, as the services of the fit record list it"
	refusal := runner.Reply{Exit: 1, Stdout: []byte(`{"error":{"code":"change.ambiguous_service","reason":"x",` +
		`"fix":"` + registryFix + `","subject":"svc_72abd8ce"}}`)}
	check := func(what, fix, profile string) {
		t.Helper()
		want := serverFixes["change.ambiguous_service"].stdio
		if profile == config.ProfileBench {
			want = serverFixes["change.ambiguous_service"].bench
		}
		if fix != want || strings.Contains(fix, "as the services of the fit record list it") ||
			profile == config.ProfileBench && strings.Contains(fix, "operator") {
			t.Errorf("%s (%s): the fix %q", what, profile, fix)
		}
	}
	// outside stdio mode no caller pins workers (fit_start's capture argument is stdio's): the fix names no pin
	remote := newHarness(t, harnessOpt{remote: true})
	d, _ := remote.store.PutArtifact(context.Background(), "fit", []byte(fitDoc))
	if err := remote.store.SetBaseline(context.Background(), "staging", d); err != nil {
		t.Fatal(err)
	}
	b, isErr := remote.call("context", `{"env":"staging"}`)
	if isErr {
		t.Fatalf("context env: %s", b)
	}
	remote.cli.replies = map[string]runner.Reply{"preview": refusal, "budget": refusal}
	b, isErr = remote.call("budget", `{"fit_id":"`+field(t, b, "fit_id").(string)+`","service":"svc_72abd8ce"}`)
	if fix := refusalOK(t, "budget, remote", b, isErr, nil, "change.ambiguous_service"); fix !=
		serverFixes["change.ambiguous_service"].remote || strings.Contains(fix, "pin") {
		t.Errorf("budget, remote: the fix %q", fix)
	}
	for _, profile := range []string{config.ProfileDefault, config.ProfileBench} {
		h := newHarness(t, harnessOpt{profile: profile})
		var fit string
		if profile == config.ProfileDefault {
			fit = h.startFit(`{"capture":"loop"}`)
		} else {
			b, isErr := h.call("context", `{"capture":"loop"}`)
			if isErr {
				t.Fatalf("bench context capture: %s", b)
			}
			fit = field(t, b, "fit_id").(string)
		}
		h.cli.replies = map[string]runner.Reply{"preview": refusal, "budget": refusal}
		b, isErr := h.call("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"svc_72abd8ce","value":1}]}`)
		check("predict", refusalOK(t, "predict", b, isErr, nil, "change.ambiguous_service"), profile)
		b, isErr = h.call("budget", `{"fit_id":"`+fit+`","service":"svc_72abd8ce"}`)
		check("budget", refusalOK(t, "budget", b, isErr, nil, "change.ambiguous_service"), profile)
		if profile == config.ProfileBench {
			continue
		}
		h.cli.replies = map[string]runner.Reply{"fit": refusal}
		b, isErr = h.call("fit_start", `{"capture":"loop","workers":{"svc_72abd8ce":2}}`)
		if isErr {
			t.Fatalf("fit_start: %s", b)
		}
		st, _ := h.call("job_status", `{"job_id":"`+field(t, b, "job_id").(string)+`","wait_s":10}`)
		var out struct {
			State   string `json:"state"`
			Refusal struct {
				Code, Fix string
			} `json:"refusal"`
		}
		if json.Unmarshal(st, &out) != nil || out.State != "failed" || out.Refusal.Code != "change.ambiguous_service" {
			t.Fatalf("fit_start workers on an ambiguous name: %s", st)
		}
		check("fit_start", out.Refusal.Fix, profile)
	}
}

// TestDataContractResource: rhobound://data-contract is the whole registry (docs/mcp/data-contract.json, canonical),
// every code with its level, trigger, condition and fix, not the explain section cut to a size.
func TestDataContractResource(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	res, err := h.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: dataContractURI})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Schema string                       `json:"schema"`
		Codes  []map[string]json.RawMessage `json:"codes"`
	}
	text := res.Contents[0].Text
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Schema != "rhobound.data-contract/v1" ||
		strings.Contains(text, `"truncated"`) {
		t.Fatalf("rhobound://data-contract: %v %.300s", err, text)
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "docs", "mcp", "data-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		Codes []struct {
			Code string `json:"code"`
		} `json:"codes"`
	}
	if err := json.Unmarshal(b, &want); err != nil || len(want.Codes) < 100 {
		t.Fatalf("docs/mcp/data-contract.json: %v, %d codes", err, len(want.Codes))
	}
	if len(got.Codes) != len(want.Codes) {
		t.Fatalf("the resource lists %d codes, the registry %d", len(got.Codes), len(want.Codes))
	}
	for i, c := range want.Codes {
		var code string
		if json.Unmarshal(got.Codes[i]["code"], &code) != nil || code != c.Code {
			t.Errorf("code %d: %s, want %s", i, got.Codes[i]["code"], c.Code)
		}
		for _, k := range []string{"level", "trigger", "when", "fix", "status"} {
			if len(got.Codes[i][k]) == 0 {
				t.Errorf("%s: no %s", c.Code, k)
			}
		}
	}
	if n := len(h.fake.Calls()); n != 0 {
		t.Errorf("reading the registry ran the engine %d times", n)
	}
}

// arnCollision writes testdata/capture-chain with its service b renamed to a Lambda function whose ARN's last element
// is c, the name of another service of the capture, and returns its folder and the account the ARN names.
func arnCollision(t *testing.T) (dir, account string) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(root, "testdata", "capture-chain", "traces", "spans.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	account = strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	const b = `"key":"service.name","value":{"stringValue":"b"}`
	if !bytes.Contains(raw, []byte(b)) {
		t.Fatal("capture-chain has no service b")
	}
	raw = bytes.ReplaceAll(raw, []byte(b),
		[]byte(`"key":"service.name","value":{"stringValue":"arn:aws:lambda:us-east-1:`+account+`:function:c"}`))
	dir = filepath.Join(t.TempDir(), "arn-collision")
	if err := os.MkdirAll(filepath.Join(dir, "traces"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil || zw.Close() != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "traces", "spans.jsonl.gz"), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, account
}

// TestFindingsOfEachServiceOverMCP is the server's mirror of the CLI's TestFindingsOfEachServiceReachEveryView: on a
// fit with a service c and a Lambda function whose ARN's last element is c, check records
// trace.image_digest_missing about a, c and the function (c, untrusted). context full, predict full, explain's
// findings on the fit and on the prediction, rhobound://fit and rhobound://prediction each list all three, and none
// shows the account.
func TestFindingsOfEachServiceOverMCP(t *testing.T) {
	dir, account := arnCollision(t)
	h := realServer(t, map[string]string{"coll": dir}, []byte(`{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":5}`),
		time.Unix(1700000000, 0).Add(time.Hour), "")
	fit := field(t, h.wait(`{"capture":"coll"}`), "fit_id").(string)
	type shown struct {
		Subject   string `json:"subject"`
		Untrusted bool   `json:"subject_untrusted"`
		Code      string `json:"code"`
	}
	want := []shown{{"a", false, ""}, {"c", false, ""}, {"c", true, ""}}
	check := func(what string, body []byte, path ...string) {
		t.Helper()
		if bytes.Contains(body, []byte(account)) {
			t.Errorf("%s shows the account", what)
		}
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		for _, k := range path {
			v = v.(map[string]any)[k]
		}
		b, _ := json.Marshal(v)
		var fs []shown
		if err := json.Unmarshal(b, &fs); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		var got []shown
		for _, f := range fs {
			if f.Code == "trace.image_digest_missing" {
				got = append(got, shown{f.Subject, f.Untrusted, ""})
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s lists trace.image_digest_missing about %v, want %v", what, got, want)
		}
	}
	body, _ := h.view("context", `{"fit_id":"`+fit+`","detail":"full"}`, FullMaxBytes)
	check("context full", body, "findings")
	body, _ = h.view("explain", `{"target":"`+fit+`","section":"findings","max_chars":8000}`, FullMaxBytes)
	check("explain findings on the fit", body, "content", "findings")
	body, _ = h.view("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"a","value":0.1}],"detail":"full"}`,
		FullMaxBytes)
	check("predict full", body, "findings")
	pred := field(t, body, "prediction_id").(string)
	body, _ = h.view("explain", `{"target":"`+pred+`","section":"findings","max_chars":8000}`, FullMaxBytes)
	check("explain findings on the prediction", body, "content", "findings")
	check("rhobound://fit", []byte(h.readResource(fitURI+fit)), "findings")
	check("rhobound://prediction", []byte(h.readResource(predictionURI+pred)), "report", "findings")
}
