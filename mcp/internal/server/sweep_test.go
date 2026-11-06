// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/ratelimit"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// TestServerFoldersNeverLeak: no location of the server reaches a caller. Two servers' captures, policies,
// environment root, state and work folders and engine are each laid out under a sentinel folder; every tool, resource
// and prompt is called in stdio and HTTP modes and in both profiles, refusal paths included (a capture check refuses,
// a refusal the engine prints, ab.sampled, unknown names, handles and sections), and no byte the client reads carries
// either sentinel, or the raw peer address that a baseline's trace.peer_renamed finding holds. The records name the
// captures as registered, so the same arms under two folders give the same fit and the same A/B record.
//
// What each mode reaches: over HTTP the top-level capture argument is refused, so the fit handle comes from the
// environment's baseline (set to the stdio fit, and last, for a read-only caller, to fits made outside the server
// without --name, whose records name the folders they were made from, and to one whose records hold a peer's address);
// the bench profile has no fit_start or job_status, so its stdio server fits through context{capture} and makes no A/B
// record. In the bench profile, no message the server sends names fit_start or job_status, apart from the protocol
// error for a call of one (ARCHITECTURE.md 5.3): not the instructions, the lists, the prompts, the refusals' fixes, nor
// the engine's decide_with.
func TestServerFoldersNeverLeak(t *testing.T) {
	fx := loop(t)
	now := fx.windowEnd.Add(time.Hour)
	store := state.NewMem(func() time.Time { return now })
	tmp := t.TempDir()
	rootA, rootB := filepath.Join(tmp, "rb-sentinel-a"), filepath.Join(tmp, "rb-sentinel-b")
	layOut(t, fx, rootA)
	layOut(t, fx, rootB)
	forbidden := []string{"rb-sentinel", tmp, fx.capture, real.root, real.bin, real.dir, peerAddress}

	// stdio, default profile, under A: fits, A/B runs, and every refusal a local caller can meet
	w := sweepServer(t, rootA, config.ProfileDefault, store, now, "")
	fitA := w.job(`{"capture":"loop"}`, "completed")["fit_id"].(string)
	if st := w.job(`{"capture":"bad"}`, "failed"); refusalOf(st) != "trace.bad_json" {
		t.Errorf("the capture check refuses: %v", st)
	}
	w.tool("context", `{"capture":"bad"}`)
	w.tool("context", `{"capture":"nope"}`)
	w.tool("fit_start", `{"env":"staging"}`)
	w.tool("fit_start", `{"ab":{"baseline":"shift-a","candidate":"nope"}}`)
	abA := w.job(`{"ab":{"baseline":"shift-a","candidate":"shift-b"}}`, "completed")["ab_id"].(string)
	st := w.job(`{"ab":{"baseline":"sampled-a","candidate":"sampled-b"}}`, "failed")
	if r, _ := st["refusal"].(map[string]any); r["code"] != "ab.sampled" || r["subject"] != "sampled-b" {
		t.Errorf("ab.sampled: %v", st)
	}
	w.views(fitA, abA)
	fitDigest := w.digest(fitA)
	abDigest := w.digest(abA)
	if err := store.SetBaseline(context.Background(), "staging", fitDigest); err != nil {
		t.Fatal(err)
	}
	// the records name the captures as registered
	rec := record(t, store, fitDigest)
	if rec["capture"] != "loop" {
		t.Errorf("the fit records the capture %v", rec["capture"])
	}
	arms, _ := record(t, store, abDigest)["arms"].(map[string]any)
	for arm, want := range map[string]string{"baseline": "shift-a", "candidate": "shift-b"} {
		if a, _ := arms[arm].(map[string]any); a["capture"] != want {
			t.Errorf("the A/B record's %s arm is %v, want %s", arm, a["capture"], want)
		}
	}
	logs := []*sweeper{w}

	// HTTP, default profile, under B: the same arms give the same A/B record
	w = sweepServer(t, rootB, config.ProfileDefault, store, now, "fit-token")
	w.tool("fit_start", `{"capture":"loop"}`)
	w.tool("context", `{"capture":"loop"}`)
	fitB := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
	abB := w.job(`{"ab":{"baseline":"shift-a","candidate":"shift-b"}}`, "completed")["ab_id"].(string)
	if d := w.digest(abB); d != abDigest {
		t.Errorf("the same arms under another folder give another A/B record: %s, %s", d, abDigest)
	}
	w.job(`{"ab":{"baseline":"sampled-a","candidate":"sampled-b"}}`, "failed")
	w.views(fitB, abB)
	logs = append(logs, w)

	// stdio, bench profile, under B: the same capture gives the same fit
	w = sweepServer(t, rootB, config.ProfileBench, store, now, "")
	w.tool("fit_start", `{"capture":"loop"}`)
	w.tool("context", `{"capture":"bad"}`)
	fitC := w.tool("context", `{"capture":"loop"}`)["fit_id"].(string)
	if d := w.digest(fitC); d != fitDigest {
		t.Errorf("the same capture under another folder gives another fit: %s, %s", d, fitDigest)
	}
	w.views(fitC, "")
	logs = append(logs, w)

	// HTTP, bench profile, under A
	w = sweepServer(t, rootA, config.ProfileBench, store, now, "fit-token")
	w.tool("context", `{"capture":"loop"}`)
	fitD := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
	w.views(fitD, "")
	logs = append(logs, w)

	// HTTP, default profile, a read-only caller, under A: baselines made outside the server without --name, from a
	// folder and from a cut's manifest, and one with --name whose findings give a peer's address. Their records name
	// those folders and that address; no reply does, rhobound://prediction included.
	for _, seeded := range seededFits(t, fx, rootA, store) {
		if err := store.SetBaseline(context.Background(), "staging", seeded.digest); err != nil {
			t.Fatal(err)
		}
		w = sweepServer(t, rootA, config.ProfileDefault, store, now, "read-token")
		w.name += ", a baseline fitted from " + seeded.form
		fit, _ := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
		if fit == "" {
			t.Fatalf("%s: no fit handle", w.name)
		}
		w.views(fit, "")
		pred, _ := w.tool("predict", `{"env":"staging","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)["prediction_id"].(string)
		res, err := w.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: predictionURI + pred})
		if err != nil || len(res.Contents) != 1 {
			t.Fatalf("%s: the prediction resource: %v", w.name, err)
		}
		var doc struct {
			Report struct {
				Capture string `json:"capture"`
			} `json:"report"`
		}
		if err := json.Unmarshal([]byte(res.Contents[0].Text), &doc); err != nil || doc.Report.Capture != seeded.shown {
			t.Errorf("%s: the prediction resource's capture is %q, want %q (%v)", w.name, doc.Report.Capture, seeded.shown, err)
		}
		logs = append(logs, w)
	}

	for _, w := range logs {
		b := w.log.bytes()
		if len(b) == 0 {
			t.Fatalf("%s: nothing recorded", w.name)
		}
		if strings.HasPrefix(w.name, config.ProfileBench) {
			for _, line := range strings.Split(string(w.reads.bytes()), "\n") {
				for _, word := range serverJobWords {
					if strings.Contains(line, word) && !strings.Contains(line, `unknown tool \"`+word+`\"`) {
						t.Errorf("%s: a reply names %s: %s", w.name, word, line[:min(len(line), 400)])
					}
				}
			}
			if !strings.Contains(string(w.reads.bytes()), "outside this server") {
				t.Errorf("%s: no reply says where the A/B is measured", w.name)
			}
		}
		for _, f := range forbidden {
			if bytes.Contains(b, []byte(f)) {
				i := bytes.Index(b, []byte(f))
				t.Errorf("%s: a reply carries %q: ...%s...", w.name, f, b[max(0, i-300):min(len(b), i+200)])
			}
		}
		t.Logf("%s: %d calls, %d bytes read", w.name, w.calls, len(b))
	}
}

// seededFit is a baseline fit made outside the server: how it was made, its digest, and the capture the prediction
// resource shows for it.
type seededFit struct{ form, digest, shown string }

// seededFits makes two fits of the loop capture under root with the real CLI, as a step outside the server may make an
// environment's baseline, without --name: `fit FOLDER`, and `check --window --cut` then `fit --manifest`. Each record
// names the folder it was made from (checked, so that the sweep looks for something that is there), and is stored. A
// third is the trace.peer_renamed fixture, with its cluster folder, fitted with --name as DEPLOY_AWS.md 14 says: its
// record holds the raw server.address:port of the finding's examples (checked too), which no reply may carry.
func seededFits(t *testing.T, fx loopFixture, root string, store state.Store) []seededFit {
	t.Helper()
	ext := filepath.Join(root, "ext")
	engine := func(args ...string) {
		t.Helper()
		cmd := exec.Command(real.bin, args...)
		cmd.Env = append(os.Environ(), "RHOBOUND_ESTIMATOR=", "RHOBOUND_STATE="+filepath.Join(ext, "state"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("rhobound %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	fit := func(out string, args ...string) string {
		t.Helper()
		engine(append([]string{"fit"}, append(args, "--out", out, "--json")...)...)
		b, err := os.ReadFile(filepath.Join(out, "fit.json"))
		if err != nil {
			t.Fatal(err)
		}
		d, err := store.PutArtifact(context.Background(), "fit", b)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := record(t, store, d)["capture"].(string)
		if c != "staging" && !strings.HasPrefix(c, root) || c == "staging" && !bytes.Contains(b, []byte(peerAddress)) {
			t.Fatalf("the seeded fit records the capture %q and no folder under the sentinel or peer address", c)
		}
		return d
	}
	peer := filepath.Join(root, "captures", "peer")
	copyTree(t, testgenCapture(t, "trace.peer_renamed"), peer)
	// the window form: a copy of the capture without its request count, so that the cut is not counted
	raw := filepath.Join(ext, "raw")
	copyTree(t, filepath.Join(fx.capture, "traces"), filepath.Join(raw, "traces"))
	cut := filepath.Join(ext, "cut")
	engine("check", raw, "--window", fx.windowEnd.Add(-3*time.Minute).Format(time.RFC3339)+","+
		fx.windowEnd.Add(time.Minute).Format(time.RFC3339), "--cut", "entry-start", "--out", cut, "--json")
	return []seededFit{
		{"a folder", fit(filepath.Join(ext, "fit-folder"), filepath.Join(root, "captures", "loop")), "loop"},
		{"a cut's manifest", fit(filepath.Join(ext, "fit-manifest"), "--manifest", filepath.Join(cut, "manifest.json")),
			"manifest.json"},
		{"a capture with a renamed peer", fit(filepath.Join(ext, "fit-peer"), peer, "--name=staging"), "staging"},
	}
}

// peerAddress is the peer of the trace.peer_renamed fixture, a Service's ClusterIP, which its traces give as
// server.address.
const peerAddress = "192.0.2.10"

// layOut puts a server's locations under root: the loop capture and a copy that check refuses (an undecodable file),
// the shift pair and a copy of it that diff-fit refuses as sampled (the candidate's request count removed, the load
// generator's log short of every 20th of its requests), two policies, an environment root, the engine, and the state,
// work and engine-state folders.
func layOut(t *testing.T, fx loopFixture, root string) {
	t.Helper()
	for _, c := range []string{"loop", "bad"} {
		copyTree(t, filepath.Join(fx.capture, "traces"), filepath.Join(root, "captures", c, "traces"))
		copyFile(t, filepath.Join(fx.capture, "request-count.json"), filepath.Join(root, "captures", c, "request-count.json"))
	}
	if err := os.WriteFile(filepath.Join(root, "captures", "bad", "traces", "extra.json"), []byte(`{"resourceSpans": [ not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	pair := filepath.Join(fx.capture, "ab-shift")
	for _, p := range []string{"shift", "sampled"} {
		copyTree(t, filepath.Join(pair, "baseline"), filepath.Join(root, "captures", p+"-a"))
		copyTree(t, filepath.Join(pair, "candidate"), filepath.Join(root, "captures", p+"-b"))
	}
	if err := os.Remove(filepath.Join(root, "captures", "sampled-b", "request-count.json")); err != nil {
		t.Fatal(err)
	}
	dropEveryNth(t, filepath.Join(root, "captures", "sampled-a", "loadgen.csv"), "B", 20)
	for _, sub := range []string{"policies", "env", "state", "work", "engine-state", "engine"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "policies", "default.json"), fx.policy, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "policies", "strict.json"),
		[]byte(`{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFile(t, real.bin, filepath.Join(root, "engine", "rhobound"))
	if err := os.Chmod(filepath.Join(root, "engine", "rhobound"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// sweeper is a client of one server whose every reply is recorded: each message read, and over HTTP each response
// body as it came.
type sweeper struct {
	t     *testing.T
	name  string
	cs    *mcp.ClientSession
	store state.Store
	who   state.Principal
	log   *syncBuffer
	reads *syncBuffer // the messages the client read, one per line
	calls int
	marks []mark // where each request's part of log starts
	about string // what the requests from here on are about, put before each in marks
	// again connects another recording client to the same server, asking for a protocol revision.
	again func(protocol string) *sweeper
}

// mark is where in a sweeper's log the replies to one request start, and the request.
type mark struct {
	at   int
	what string
}

// mark records that what the client reads from here on answers the request what (calls are made one at a time).
func (w *sweeper) mark(what string) {
	w.marks = append(w.marks, mark{w.log.len(), w.about + what})
}

// reply is one request of a sweeper, and every byte its client recorded while it ran: the replies, error messages
// and error data, and over HTTP the bodies as they came.
type reply struct {
	what string
	body []byte
}

// replies splits the log by request.
func (w *sweeper) replies() []reply {
	b := w.log.bytes()
	out := make([]reply, len(w.marks))
	for i, m := range w.marks {
		end := len(b)
		if i+1 < len(w.marks) {
			end = w.marks[i+1].at
		}
		out[i] = reply{m.what, b[m.at:end]}
	}
	return out
}

// readsOnly keeps the lines of an mcp.LoggingTransport that record a message read.
type readsOnly struct{ w io.Writer }

func (r readsOnly) Write(p []byte) (int, error) {
	if bytes.HasPrefix(p, []byte("read: ")) {
		r.w.Write(p)
	}
	return len(p), nil
}

// sweepServer starts a server on the real CLI with the configuration of layOut's root, in stdio mode (token "": an
// in-memory transport, local captures) or behind the HTTP handler and its bearer auth, and connects a recording client
// that sends token (testVerifier's "fit-token" or "read-token").
func sweepServer(t *testing.T, root, profile string, store state.Store, now time.Time, token string) *sweeper {
	t.Helper()
	return sweepServerOf(t, root, map[string]string{"loop": "captures/loop", "bad": "captures/bad",
		"shift-a": "captures/shift-a", "shift-b": "captures/shift-b", "sampled-a": "captures/sampled-a",
		"sampled-b": "captures/sampled-b"}, profile, store, now, token, "")
}

// sweepServerOf is sweepServer for a root laid out with the given captures (by name, relative to root), whose client
// asks for a protocol revision ("" for the SDK client's default). The sweeper's again connects another recording
// client to the same server.
func sweepServerOf(t *testing.T, root string, captures map[string]string, profile string, store state.Store, now time.Time,
	token, protocol string) *sweeper {
	t.Helper()
	cfg, err := json.Marshal(map[string]any{"schema": config.Schema,
		"engine":    map[string]string{"path": filepath.Join(root, "engine", "rhobound"), "sha256": real.sha},
		"state_dir": "state", "envs": map[string]any{"staging": map[string]string{"store": "dir", "root": "env"}},
		"captures": captures,
		"policies": map[string]string{"default": "policies/default.json", "strict": "policies/strict.json"},
		"profile":  profile})
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.Parse(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := runner.NewExec(c.Engine.Path, c.Engine.SHA256, 2*time.Minute,
		[]string{"RHOBOUND_STATE=" + filepath.Join(root, "engine-state")})
	if err != nil {
		t.Fatal(err)
	}
	engine.TempRoot = filepath.Join(root, "work")
	opt := Options{Config: c, Runner: engine, Store: store, Now: func() time.Time { return now },
		WorkDir: filepath.Join(root, "work"), Limits: ratelimit.New(ratelimit.Rules{})}
	if token == "" {
		opt.LocalCaptures = true
		opt.Identity = func(context.Context) (auth.Identity, bool) {
			return auth.Identity{Principal: "local:alice", Scopes: []string{auth.ScopeRead, auth.ScopeFit}}, true
		}
	}
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.WaitJobs)
	var ts *httptest.Server
	if token != "" {
		h, err := srv.HTTPHandler(HTTPOptions{Auth: auth.Bearer(testVerifier, auth.BearerOptions{
			ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource"})})
		if err != nil {
			t.Fatal(err)
		}
		ts = httptest.NewServer(h)
		t.Cleanup(ts.Close)
	}
	var dial func(protocol string) *sweeper
	dial = func(protocol string) *sweeper {
		t.Helper()
		w := &sweeper{t: t, store: store, log: &syncBuffer{}, reads: &syncBuffer{}, name: profile + " stdio",
			who: "local:alice", again: dial}
		ctx := context.Background()
		var tr mcp.Transport
		if token != "" {
			w.name, w.who = profile+" HTTP", map[string]state.Principal{"fit-token": "fitter", "read-token": "reader"}[token]
			tr = &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", MaxRetries: -1, DisableStandaloneSSE: true,
				HTTPClient: &http.Client{Transport: recordBodies{w.log, withToken{token, http.DefaultTransport}}}}
		} else {
			ct, st := mcp.NewInMemoryTransports()
			ss, err := srv.MCP().Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ss.Wait() })
			tr = ct
		}
		w.mark("initialize")
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx,
			&mcp.LoggingTransport{Transport: tr, Writer: io.MultiWriter(w.log, readsOnly{w.reads})},
			&mcp.ClientSessionOptions{ProtocolVersion: protocol})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cs.Close() })
		w.cs = cs
		// the lists
		w.mark("tools/list, prompts/list, resources/list, resources/templates/list")
		if _, err := cs.ListTools(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.ListPrompts(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.ListResources(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.ListResourceTemplates(ctx, nil); err != nil {
			t.Fatal(err)
		}
		return w
	}
	return dial(protocol)
}

// tool calls a tool and returns its structuredContent, or nil for a protocol error (a refusal path).
func (w *sweeper) tool(name, args string) map[string]any {
	w.t.Helper()
	w.calls++
	w.mark("tools/call " + name + " " + args)
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		w.t.Fatal(err)
	}
	res, err := w.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		return nil
	}
	m, _ := res.StructuredContent.(map[string]any)
	return m
}

// job runs fit_start and job_status until the job leaves working, and checks the state it ends in.
func (w *sweeper) job(args, want string) map[string]any {
	w.t.Helper()
	id, _ := w.tool("fit_start", args)["job_id"].(string)
	if id == "" {
		w.t.Fatalf("%s: fit_start %s started no job", w.name, args)
	}
	for i := 0; i < 10; i++ {
		st := w.tool("job_status", `{"job_id":"`+id+`","wait_s":20}`)
		if s := st["state"]; s != "working" {
			if s != want {
				w.t.Fatalf("%s: fit_start %s: %v", w.name, args, st)
			}
			return st
		}
	}
	w.t.Fatalf("%s: fit_start %s is still working", w.name, args)
	return nil
}

// read reads a resource, and returns the error a refusal path answers with.
func (w *sweeper) read(uri string) error {
	w.calls++
	w.mark("resources/read " + uri)
	_, err := w.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
	return err
}

// prompt gets a prompt, and returns the error a refusal path answers with.
func (w *sweeper) prompt(name string, args map[string]string) error {
	w.calls++
	w.mark(fmt.Sprintf("prompts/get %s %v", name, args))
	_, err := w.cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: name, Arguments: args})
	return err
}

// digest is the record a handle of this caller names.
func (w *sweeper) digest(h string) string {
	w.t.Helper()
	_, d, err := w.store.Resolve(context.Background(), w.who, h)
	if err != nil {
		w.t.Fatal(err)
	}
	return d
}

// views calls every view, resource and prompt on a fit handle and, when there is one, an A/B handle: results and the
// refusals of unknown services, policies, sections and handles.
func (w *sweeper) views(fit, ab string) {
	w.t.Helper()
	f := `"fit_id":"` + fit + `"`
	w.tool("context", `{`+f+`}`)
	w.tool("context", `{`+f+`,"detail":"full"}`)
	w.tool("budget", `{`+f+`,"service":"b"}`)
	w.tool("budget", `{`+f+`,"policy":"strict"}`)
	w.tool("budget", `{`+f+`,"policy":"nope"}`)
	w.tool("budget", `{`+f+`,"service":"zz"}`)
	pred, _ := w.tool("predict", `{`+f+`,"changes":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6.0}}],"repo":"shop","branch":"x"}`)["prediction_id"].(string)
	if pred == "" {
		w.t.Fatalf("%s: predict minted no prediction", w.name)
	}
	w.tool("predict", `{`+f+`,"changes":[{"op":"own_ms","service":"b","value":0.4}],"detail":"full"}`)
	w.tool("predict", `{`+f+`,"changes":[{"op":"own_ms","service":"zz","value":0.4}]}`)
	w.tool("predict", `{`+f+`,"changes":[],"policy":"nope"}`)
	w.tool("predict", `{`+f+`,"changes":[],"assumptions":[{"question":"Q1","range":{"lo":1,"hi":3}}]}`)
	w.tool("predict", `{`+f+`,"measured":[{"profile":{"service":"b","candidate":"sha256:`+strings.Repeat("a", 64)+`"}}]}`)
	w.tool("predict", `{`+f+`,"measured":[{"ab":"ab_`+strings.Repeat("1", 26)+`"}]}`)
	sections := []string{"summary", "services", "findings", "assumptions", "rules", "judge_packet"}
	targets := []string{fit, pred, "fit_" + strings.Repeat("2", 26)}
	preds := []string{pred}
	if ab != "" {
		// the predictions an A/B record backs, alone and beside a declared change: the documents in which its items,
		// record digests and labels enter a report
		for _, args := range []string{`{` + f + `,"measured":[{"ab":"` + ab + `"}]}`,
			`{` + f + `,"changes":[{"op":"own_ms","service":"c","value":0.4}],"measured":[{"ab":"` + ab + `"}],"detail":"full"}`} {
			p, _ := w.tool("predict", args)["prediction_id"].(string)
			if p == "" {
				w.t.Fatalf("%s: predict %s minted no prediction", w.name, args)
			}
			preds = append(preds, p)
			targets = append(targets, p)
		}
		targets = append(targets, ab)
	}
	for _, tg := range targets {
		for _, sec := range sections {
			w.tool("explain", `{"target":"`+tg+`","section":"`+sec+`","max_chars":8000}`)
		}
	}
	w.tool("explain", `{"section":"data_contract"}`)
	w.tool("explain", `{"section":"scope"}`)
	w.tool("job_status", `{"job_id":"job_`+strings.Repeat("3", 26)+`"}`)
	for _, uri := range []string{scopeURI, dataContractURI, policyURI + "default", policyURI + "strict", policyURI + "nope",
		fitURI + fit, fitURI + "fit_" + strings.Repeat("2", 26), predictionURI + "x"} {
		w.read(uri)
	}
	// the documents of the predictions are served, so that the sweep reads them, not a refusal
	for _, p := range preds {
		for _, uri := range []string{predictionURI + p, packetURI + p} {
			if err := w.read(uri); err != nil {
				w.t.Errorf("%s: %s: %v", w.name, uri, err)
			}
		}
	}
	w.prompt(promptPlanChange, nil)
	for _, p := range preds {
		if err := w.prompt(promptReview, map[string]string{"prediction_id": p}); err != nil {
			w.t.Errorf("%s: %s on %s: %v", w.name, promptReview, p, err)
		}
	}
	w.prompt(promptReview, map[string]string{"prediction_id": fit})
}

// refusalOf is the code of a job's refusal.
func refusalOf(st map[string]any) string {
	r, _ := st["refusal"].(map[string]any)
	c, _ := r["code"].(string)
	return c
}

// record reads a stored record.
func record(t *testing.T, store state.Store, digest string) map[string]any {
	t.Helper()
	b, err := store.GetArtifact(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeObject(b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// syncBuffer is a buffer safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

func (s *syncBuffer) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// recordBodies records every HTTP response body, as it came, before the client reads it.
type recordBodies struct {
	log  io.Writer
	base http.RoundTripper
}

func (r recordBodies) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	r.log.Write(b)
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return resp, nil
}
