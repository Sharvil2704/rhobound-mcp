// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/ratelimit"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// The fake engine below answers the CLI twins with documents shaped like the published views. Its numbers are
// placeholders (the real CLI is wired in later); what the tests check is what the server does with the bytes.

const fitDoc = `{"schema":"rhobound.fit/v1","capture":"loop","window":{"start":"2026-10-26T11:50:00Z","end":"2026-10-26T11:59:40Z","window_s":580}}`

// contextView carries numbers the server must pass through exactly as written (1.50, 1e-7, a 20-digit integer) and
// characters JSON encoders like to escape.
const contextView = `{"schema":"rhobound-mcp.context/v1","fit_digest":"FIT","window_s":580,"entry_rps":5.3431,
 "evidence_class":"simulated-70-80","max_utilization":0.4512,"validated_ceiling":0.58,
 "entries":[{"name":"a","model_ms":45.22,"observed_ms":45.21}],
 "services":[{"name":"b","utilization":0.4512,"own_ms":1.50,"workers":"2","leverage":{"a":1.1531},"misfit_pct":1e-7}],
 "findings":[{"code":"trace.sampling_unverified","level":"doubt","fix":"give a request count <or> declare the kept share & sampler"}],
 "assumptions":[{"id":"balancing_random","status":"not-checkable"}],"truncated":false,"scope":"mean-only/v1"}`

const budgetView = `{"schema":"rhobound-mcp.budget/v1","fit_digest":"FIT","items":[{"service":"b","kind":"own_ms","per":"visit",
 "max_value":1.95,"safe_value":1.07,"binding_constraint":"max_entry_mean_change_pct","binding_entry":"a"}],
 "assumes":["monotone entry mean in the added quantity (untested)"],"truncated":false,"scope":"mean-only/v1"}`

const predictView = `{"schema":"rhobound-mcp.prediction/v1","fit_digest":"FIT","change_hash":"CHANGE","evidence_class":"simulated-70-80",
 "entries":[{"name":"a","before_ms":45.22,"after_ms":{"lo":49.32,"hi":52.37},"change_ms":{"lo":4.1,"hi":7.15},
  "change_pct":{"lo":9.07,"hi":15.81},"model_band_ms":1.027,"band_ms":{"lo":3.07,"hi":8.32},"threshold_ms":2.261}],
 "preview":{"verdict":"VERDICT","advisory":true,"rules":[],"decide_with":[],
  "sentence":"advisory: computed from inputs the caller supplied; CI decides with measured inputs and the repository's policy"},
 "findings":[],"assumptions":[],"truncated":false,"scope":"mean-only/v1"}`

const explainView = `{"schema":"rhobound-mcp.explain/v1","section":"SECTION","content":{"text":"x"},"truncated":false}`

const abRecord = `{"schema":"rhobound.ab-record/v1","services":[{"service":"b","own_ms":{"baseline":1.20,"candidate":1.60,"delta":0.40,"se":0.05}}]}`

// fakeCLI answers the CLI twins. block makes fit wait for its context (a job that outlives its deadline) or, when set,
// for release to close; delay makes a subcommand take that long, within its context; ab, when set, is the A/B record
// diff-fit writes (abRecord otherwise).
type fakeCLI struct {
	mu      sync.Mutex
	block   bool
	release chan struct{}
	delay   map[string]time.Duration
	ab      string
	replies map[string]runner.Reply // overrides by subcommand
}

func (f *fakeCLI) handle(ctx context.Context, c runner.Call) runner.Reply {
	f.mu.Lock()
	r, override := f.replies[c.Sub]
	block, release, delay, ab := f.block, f.release, f.delay[c.Sub], f.ab
	f.mu.Unlock()
	if ab == "" {
		ab = abRecord
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return runner.Reply{Err: runner.ErrTimeout}
		}
	}
	if override {
		return r
	}
	ok := func(s string) runner.Reply { return runner.Reply{Stdout: []byte(s)} }
	digest := func(flag string) string {
		b, _ := c.FileFor(flag)
		return state.Digest(b)
	}
	switch c.Sub {
	case "check":
		if strings.Contains(c.Args[0], "refused") {
			return runner.Reply{Exit: 4, Stdout: []byte(`{"schema":"rhobound.check/v1","verdict":"refused","findings":[` +
				`{"code":"trace.ids_base64","level":"refuse","subject":"a b","fix":"write ids as hex"},` +
				`{"code":"trace.sampling_unverified","level":"doubt","fix":"give a request count"}]}`)}
		}
		return ok(`{"schema":"rhobound.check/v1","verdict":"admitted-with-doubts","findings":[{"code":"trace.sampling_unverified","level":"doubt","fix":"give a request count"}]}`)
	case "fit":
		if block {
			select {
			case <-ctx.Done():
				return runner.Reply{Err: runner.ErrTimeout}
			case <-release:
			}
		}
		out, _ := c.Flag("--out")
		if err := os.WriteFile(filepath.Join(out, fitOut), []byte(fitDoc), 0o600); err != nil {
			return runner.Reply{Exit: 2}
		}
		return ok("")
	case "diff-fit":
		out, _ := c.Flag("--out")
		if err := os.WriteFile(filepath.Join(out, abOut), []byte(ab), 0o600); err != nil {
			return runner.Reply{Exit: 2}
		}
		return ok("")
	case "context":
		return ok(strings.Replace(contextView, "FIT", digest("--fit"), 1))
	case "budget":
		return ok(strings.Replace(budgetView, "FIT", digest("--fit"), 1))
	case "preview":
		verdict := "inconclusive"
		ch, _ := c.FileFor("--changes")
		var set struct {
			Items []struct {
				Label string          `json:"label"`
				Value json.RawMessage `json:"value"`
			} `json:"items"`
		}
		json.Unmarshal(ch, &set)
		for _, it := range set.Items {
			if it.Label == "declared" && strings.HasPrefix(string(it.Value), "{") {
				verdict = "block"
			}
		}
		if len(set.Items) > 0 && set.Items[0].Label == "measured-ab" {
			verdict = "pass"
		}
		v := strings.Replace(predictView, "FIT", digest("--fit"), 1)
		v = strings.Replace(v, "CHANGE", digest("--changes"), 1)
		return ok(strings.Replace(v, "VERDICT", verdict, 1))
	case "predict":
		return ok(`{"version":"rhobound-cli.report/v2","capture":"loop"}`)
	case "explain":
		sec, _ := c.Flag("--section")
		return ok(strings.Replace(explainView, "SECTION", sec, 1))
	case "judge-packet":
		return ok(`{"schema":"rhobound-mcp.judge-packet/v1"}`)
	}
	return runner.Reply{Exit: 2}
}

// clock is a settable time source shared by the server and its store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// harness is a server on a fake engine, a memory store and a fixed clock, with a client session.
type harness struct {
	t     *testing.T
	srv   *Server
	fake  *runner.Fake
	cli   *fakeCLI
	store *state.Mem
	clock *clock
	who   *string // the principal of the next call
	cs    *mcp.ClientSession
}

type harnessOpt struct {
	profile         string
	remote          bool // LocalCaptures off, as over HTTP
	realTime        bool
	jobTimeout      time.Duration
	callTimeout     time.Duration
	protocol        string
	policyContent   string
	noDefaultPolicy bool // policies "loose" and "strict", none named default
	fitTTLH         int  // the configuration's fit_ttl_h, when not 0
	maxPolls        int
	maxDerived      int
	limits          *ratelimit.Limiter
	noCaptures      bool // environments only: no capture, and so no A/B arm, is configured
}

func testConfig(t *testing.T, profile, policy string) *config.Config {
	t.Helper()
	return harnessConfig(t, harnessOpt{profile: profile, policyContent: policy})
}

// harnessConfig is the configuration of a harness: the captures loop, refused, old and new (none with noCaptures), the
// environment staging, and the policies default and strict (loose and strict with noDefaultPolicy), all one policy
// file.
func harnessConfig(t *testing.T, o harnessOpt) *config.Config {
	t.Helper()
	profile, policy := o.profile, o.policyContent
	dir := t.TempDir()
	for _, sub := range []string{"captures/loop", "captures/refused", "ab/old", "ab/new"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if policy == "" {
		policy = `{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":5}`
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	if profile == "" {
		profile = "default"
	}
	policies := `{"default":"policy.json","strict":"policy.json"}`
	if o.noDefaultPolicy {
		policies = `{"loose":"policy.json","strict":"policy.json"}`
	}
	ttl := ""
	if o.fitTTLH != 0 {
		ttl = `,"fit_ttl_h":` + strconv.Itoa(o.fitTTLH)
	}
	captures := `
  "captures":{"loop":"captures/loop","refused":"captures/refused","old":"ab/old","new":"ab/new"},`
	if o.noCaptures {
		captures = ""
	}
	doc := `{"schema":"rhobound-mcp.config/v1",
  "engine":{"path":"/usr/local/bin/rhobound","sha256":"` + strings.Repeat("0", 64) + `"},
  "state_dir":"state","envs":{"staging":{"store":"dir","root":"traces"}},` + captures + `
  "policies":` + policies + `,"profile":"` + profile + `"` + ttl + `}`
	c, err := config.Parse([]byte(doc), dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newHarness(t *testing.T, o harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, cli: &fakeCLI{}, clock: &clock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}}
	now := h.clock.now
	if o.realTime {
		now = time.Now
	}
	h.fake = &runner.Fake{Handle: h.cli.handle}
	h.store = state.NewMem(now)
	who := "local:alice"
	h.who = &who
	srv, err := New(Options{
		Config: harnessConfig(t, o), Runner: h.fake, Store: h.store, Now: now,
		LocalCaptures: !o.remote, WorkDir: t.TempDir(), JobTimeout: o.jobTimeout, CallTimeout: o.callTimeout,
		MaxPolls: o.maxPolls, MaxDerived: o.maxDerived, Limits: o.limits,
		Identity: func(context.Context) (auth.Identity, bool) {
			return auth.Identity{Principal: *h.who, Scopes: []string{auth.ScopeRead, auth.ScopeFit}}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	t.Cleanup(srv.WaitJobs)
	h.cs = connect(t, srv, o.protocol)
	return h
}

// connect opens an in-memory client session at a protocol revision ("" for the SDK client's default, 2026-07-28).
func connect(t *testing.T, srv *Server, protocol string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.MCP().Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct,
		&mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close(); ss.Wait() })
	return cs
}

// call calls a tool and returns the result's exact structuredContent bytes (from the text block, which carries the
// same JSON) and whether it is an error result. A protocol error fails the test.
func (h *harness) call(name string, args string) ([]byte, bool) {
	h.t.Helper()
	b, isErr, err := h.try(name, args)
	if err != nil {
		h.t.Fatalf("%s %s: %v", name, args, err)
	}
	return b, isErr
}

func (h *harness) try(name string, args string) ([]byte, bool, error) {
	h.t.Helper()
	var a map[string]any
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		h.t.Fatal(err)
	}
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: a})
	if err != nil {
		return nil, false, err
	}
	if len(res.Content) != 1 {
		h.t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text := res.Content[0].(*mcp.TextContent).Text
	body, ok := strings.CutSuffix(text, "\n"+ScopeSentence)
	if !ok {
		h.t.Fatalf("%s: text does not end with the scope sentence: %q", name, text)
	}
	// structuredContent and the text block carry the same document (the client reads numbers as float64, so they
	// are compared as values; the text keeps the exact bytes).
	var fromText, fromSC any
	sc, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal([]byte(body), &fromText) != nil || json.Unmarshal(sc, &fromSC) != nil || !reflect.DeepEqual(fromText, fromSC) {
		h.t.Fatalf("%s: structuredContent %s differs from the text %s", name, sc, body)
	}
	return []byte(body), res.IsError, nil
}

// protocolCode returns the JSON-RPC code of a protocol error.
func protocolCode(err error) int64 {
	var we *jsonrpc.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return 0
}

// refusalCode is the code of a refusal result, {"error": Refusal}, or "" for any other result.
func refusalCode(t *testing.T, b []byte) string {
	t.Helper()
	var env struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) != nil || env.Error == nil {
		return ""
	}
	return env.Error.Code
}

// field reads one top-level field of a JSON object.
func field(t *testing.T, b []byte, key string) any {
	t.Helper()
	m, err := decodeObject(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m[key]
}

// startFit runs fit_start(capture=loop, workers) to completion and returns the fit handle.
func (h *harness) startFit(args string) string {
	h.t.Helper()
	b, isErr := h.call("fit_start", args)
	if isErr {
		h.t.Fatalf("fit_start: %s", b)
	}
	job := field(h.t, b, "job_id").(string)
	st, _ := h.call("job_status", `{"job_id":"`+job+`","wait_s":10}`)
	if s := field(h.t, st, "state"); s != "completed" {
		h.t.Fatalf("job: %s", st)
	}
	if id, ok := field(h.t, st, "fit_id").(string); ok {
		return id
	}
	return field(h.t, st, "ab_id").(string)
}

// strip removes the server keys and re-serializes canonically: what the parity test compares with the CLI twin.
func strip(t *testing.T, b []byte) string {
	t.Helper()
	m, err := decodeObject(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ServerKeys {
		delete(m, k)
	}
	out, err := encode(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
