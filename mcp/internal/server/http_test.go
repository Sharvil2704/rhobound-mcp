// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// testVerifier accepts two tokens: "read-token" (rhobound:read) and "fit-token" (both scopes).
func testVerifier(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	exp := time.Now().Add(time.Hour)
	switch token {
	case "read-token":
		return &sdkauth.TokenInfo{UserID: "reader", Scopes: []string{auth.ScopeRead}, Expiration: exp}, nil
	case "fit-token":
		return &sdkauth.TokenInfo{UserID: "fitter", Scopes: []string{auth.ScopeRead, auth.ScopeFit}, Expiration: exp}, nil
	}
	return nil, sdkauth.ErrInvalidToken
}

// withToken adds a bearer token to every request.
type withToken struct {
	token string
	base  http.RoundTripper
}

func (w withToken) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+w.token)
	return w.base.RoundTrip(r)
}

func newHTTPServer(t *testing.T) (*httptest.Server, *runner.Fake) {
	return newHTTPServerOf(t, "")
}

func newHTTPServerOf(t *testing.T, profile string) (*httptest.Server, *runner.Fake) {
	t.Helper()
	cli := &fakeCLI{}
	fake := &runner.Fake{Handle: cli.handle}
	srv, err := New(Options{Config: testConfig(t, profile, ""), Runner: fake, Store: state.NewMem(nil), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	h, err := srv.HTTPHandler(HTTPOptions{Auth: auth.Bearer(testVerifier, auth.BearerOptions{
		ResourceMetadataURL: "https://mcp.example.com/.well-known/oauth-protected-resource"})})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(func() { ts.Close(); srv.WaitJobs() })
	return ts, fake
}

func httpClient(t *testing.T, ts *httptest.Server, token, protocol string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: withToken{token, http.DefaultTransport}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(context.Background(), tr,
		&mcp.ClientSessionOptions{ProtocolVersion: protocol})
	if err == nil {
		t.Cleanup(func() { cs.Close() })
	}
	return cs, err
}

// TestHTTPBothEras: over stateless Streamable HTTP, the SDK client negotiates each revision exactly and runs a job.
func TestHTTPBothEras(t *testing.T) {
	ts, _ := newHTTPServer(t)
	for _, v := range []string{"2026-07-28", "2025-11-25"} {
		cs, err := httpClient(t, ts, "fit-token", v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if got := cs.InitializeResult().ProtocolVersion; got != v {
			t.Errorf("negotiated %q, want %q", got, v)
		}
		if _, err := cs.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("%s: list: %v", v, err)
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fit_start",
			Arguments: map[string]any{"ab": map[string]any{"baseline": "old", "candidate": "new"}}})
		if err != nil || res.IsError {
			t.Fatalf("%s: fit_start: %v %+v", v, err, res)
		}
		job := res.StructuredContent.(map[string]any)["job_id"].(string)
		res, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "job_status",
			Arguments: map[string]any{"job_id": job, "wait_s": 10}})
		if err != nil || res.StructuredContent.(map[string]any)["state"] != "completed" {
			t.Errorf("%s: job_status: %v %+v", v, err, res)
		}
		// The top-level capture argument is local only.
		if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fit_start",
			Arguments: map[string]any{"capture": "loop"}}); err == nil {
			t.Errorf("%s: capture argument accepted over HTTP", v)
		}
	}
}

// TestHTTPScopes: a read-only token can list and read, but not start a fit; the engine never runs for it.
func TestHTTPScopes(t *testing.T) {
	ts, fake := newHTTPServer(t)
	cs, err := httpClient(t, ts, "read-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	_, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "fit_start",
		Arguments: map[string]any{"ab": map[string]any{"baseline": "old", "candidate": "new"}}})
	if err == nil || len(fake.Calls()) != 0 {
		t.Errorf("fit_start with a read-only token: %v, %d engine calls", err, len(fake.Calls()))
	}
	if _, err := httpClient(t, ts, "stolen", ""); err == nil {
		t.Error("an unknown token connected")
	}
}

// TestHTTPScopesOfTheProfile: the per-tool scopes are checked for the tools of the server's profile only. The bench
// profile has no fit_start, so a call of it answers as an unknown tool (-32602) whatever the token's scopes, never with
// a step-up challenge for rhobound:fit; the default profile still challenges a read-only token.
func TestHTTPScopesOfTheProfile(t *testing.T) {
	call := func(ts *httptest.Server, token string) *http.Response {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{` + newMeta + `,"name":"fit_start",` +
			`"arguments":{"ab":{"baseline":"old","candidate":"new"}}}}`
		r, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		r.Header.Set("Mcp-Method", "tools/call")
		r.Header.Set("Mcp-Name", "fit_start")
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { res.Body.Close() })
		return res
	}
	bench, fake := newHTTPServerOf(t, config.ProfileBench)
	for _, token := range []string{"read-token", "fit-token"} {
		res := call(bench, token)
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode == http.StatusForbidden || res.Header.Get("WWW-Authenticate") != "" ||
			!strings.Contains(string(b), `"code":-32602`) || !strings.Contains(string(b), "unknown tool") {
			t.Errorf("bench, fit_start with %s: %d %s %q", token, res.StatusCode, b, res.Header.Get("WWW-Authenticate"))
		}
	}
	if n := len(fake.Calls()); n != 0 {
		t.Errorf("the engine ran %d times", n)
	}
	full, _ := newHTTPServerOf(t, "")
	if res := call(full, "read-token"); res.StatusCode != http.StatusForbidden ||
		!strings.Contains(res.Header.Get("WWW-Authenticate"), `scope="rhobound:fit"`) {
		t.Errorf("default, fit_start with a read-only token: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
}

const newMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// TestHTTPTransportRules: header mismatch, cross-origin requests, methods other than POST, the body cap, missing
// tokens and the health check, each with the status the design names.
func TestHTTPTransportRules(t *testing.T) {
	ts, _ := newHTTPServer(t)
	list := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + newMeta + `}}`
	type req struct {
		name, method, path, body string
		hdr                      map[string]string
		status                   int
		bodyHas                  string
	}
	newEra := map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list"}
	with := func(m map[string]string, kv ...string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			out[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	cases := []req{
		{"new era ok", "POST", "/mcp", list, newEra, 200, `"tools"`},
		{"missing Mcp-Method", "POST", "/mcp", list, map[string]string{"Mcp-Protocol-Version": "2026-07-28"}, 400, "-32020"},
		{"Mcp-Method mismatch", "POST", "/mcp", list, with(newEra, "Mcp-Method", "prompts/list"), 400, "-32020"},
		{"tools/call without Mcp-Name", "POST", "/mcp",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"explain","arguments":{"section":"scope"},` + newMeta + `}}`,
			with(newEra, "Mcp-Method", "tools/call"), 400, "-32020"},
		{"cross-origin", "POST", "/mcp", list, with(newEra, "Origin", "https://attacker.example"), 403, ""},
		{"cross-site fetch", "POST", "/mcp", list, with(newEra, "Sec-Fetch-Site", "cross-site"), 403, ""},
		{"GET", "GET", "/mcp", "", nil, 405, ""},
		{"DELETE", "DELETE", "/mcp", "", map[string]string{"Mcp-Session-Id": "abc"}, 405, ""},
		{"body over 256 KiB", "POST", "/mcp",
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + strings.Repeat("a", 300<<10) + `",` + newMeta + `}}`,
			newEra, 413, ""},
		{"no token", "POST", "/mcp", list, with(newEra, "Authorization", ""), 401, ""},
		{"health", "GET", "/healthz", "", map[string]string{"Authorization": ""}, 200, "ok"},
	}
	for _, c := range cases {
		r, _ := http.NewRequest(c.method, ts.URL+c.path, strings.NewReader(c.body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Authorization", "Bearer read-token")
		for k, v := range c.hdr {
			if v == "" {
				r.Header.Del(k)
			} else {
				r.Header.Set(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status || !strings.Contains(string(b), c.bodyHas) {
			t.Errorf("%s: %d %q, want %d with %q", c.name, resp.StatusCode, b, c.status, c.bodyHas)
		}
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			t.Errorf("%s: stateless server set a session id", c.name)
		}
		if c.name == "no token" && !strings.Contains(resp.Header.Get("WWW-Authenticate"), "resource_metadata=") {
			t.Errorf("no token: WWW-Authenticate %q", resp.Header.Get("WWW-Authenticate"))
		}
	}
}

func TestHTTPHandlerNeedsAuth(t *testing.T) {
	srv, err := New(Options{Config: testConfig(t, "", ""), Runner: &runner.Fake{}, Store: state.NewMem(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.HTTPHandler(HTTPOptions{}); err == nil {
		t.Error("an HTTP endpoint without auth")
	}
	if _, err := New(Options{}); err == nil {
		t.Error("a server without config, runner and store")
	}
}
