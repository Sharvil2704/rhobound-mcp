// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/apigw"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/lambdart"
)

// lambdaRuntime is a Lambda Runtime API serving one event, recording what the function posts.
type lambdaRuntime struct {
	srv   *httptest.Server
	mu    sync.Mutex
	event string
	given bool
	posts map[string]string // "response", "error", "init" -> body
	done  chan struct{}
}

func newLambdaRuntime(t *testing.T, event string) *lambdaRuntime {
	r := &lambdaRuntime{event: event, posts: map[string]string{}, done: make(chan struct{}, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /2018-06-01/runtime/invocation/next", func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		given := r.given
		r.given = true
		r.mu.Unlock()
		if given {
			<-req.Context().Done()
			return
		}
		w.Header().Set("Lambda-Runtime-Aws-Request-Id", "inv-1")
		w.Header().Set("Lambda-Runtime-Deadline-Ms", strconv.FormatInt(time.Now().Add(time.Minute).UnixMilli(), 10))
		io.WriteString(w, r.event)
	})
	record := func(kind string) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			b, _ := io.ReadAll(req.Body)
			r.mu.Lock()
			r.posts[kind] = string(b)
			r.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			r.done <- struct{}{}
		}
	}
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/inv-1/response", record("response"))
	mux.HandleFunc("POST /2018-06-01/runtime/invocation/inv-1/error", record("error"))
	mux.HandleFunc("POST /2018-06-01/runtime/init/error", record("init"))
	r.srv = httptest.NewServer(mux)
	t.Cleanup(r.srv.Close)
	t.Setenv("AWS_LAMBDA_RUNTIME_API", strings.TrimPrefix(r.srv.URL, "http://"))
	return r
}

// run starts a mode, waits for one post, and stops the mode.
func (r *lambdaRuntime) run(t *testing.T, mode string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- modes[mode](ctx, nil) }()
	select {
	case <-r.done:
	case err := <-errc:
		cancel()
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("nothing was posted")
	}
	cancel()
	return <-errc
}

func TestLambdaModesAreRegistered(t *testing.T) {
	for _, m := range []string{"lambda", "fit-worker"} {
		if modes[m] == nil {
			t.Fatalf("mode %s is not registered", m)
		}
		t.Setenv("AWS_LAMBDA_RUNTIME_API", "")
		if err := modes[m](context.Background(), nil); err == nil || !strings.Contains(err.Error(), "only inside AWS Lambda") {
			t.Fatalf("%s outside Lambda: %v", m, err)
		}
	}
}

func TestLambdaModesReportAnUnwiredBuild(t *testing.T) {
	saveH, saveJ := lambdaHandler, fitWorkerJob
	defer func() { lambdaHandler, fitWorkerJob = saveH, saveJ }()
	lambdaHandler, fitWorkerJob = nil, nil
	for _, m := range []string{"lambda", "fit-worker"} {
		r := newLambdaRuntime(t, "{}")
		if err := r.run(t, m); err == nil || !strings.Contains(err.Error(), "not wired") {
			t.Fatalf("%s: %v", m, err)
		}
		if !strings.Contains(r.posts["init"], "Runtime.InitError") {
			t.Fatalf("%s: init error post %q", m, r.posts["init"])
		}
	}
}

func TestLambdaModeServesAPIGatewayEvents(t *testing.T) {
	save := lambdaHandler
	defer func() { lambdaHandler = save }()
	lambdaHandler = func(ctx context.Context, args []string) (http.Handler, error) {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			c, _ := auth.ClaimsFromContext(req.Context())
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"path": req.URL.Path, "sub": c.Source + ":" + c.Subject,
				"scopes": strings.Join(c.Scopes, " ")})
		}), nil
	}
	r := newLambdaRuntime(t, `{"version":"2.0","rawPath":"/mcp","headers":{"content-type":"application/json"},
		"requestContext":{"http":{"method":"POST","path":"/mcp","sourceIp":"192.0.2.7"},"stage":"$default",
		"authorizer":{"jwt":{"claims":{"sub":"agent-1"},"scopes":["rhobound:read"]}}},"body":"{}","isBase64Encoded":false}`)
	if err := r.run(t, "lambda"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the loop ended with %v", err)
	}
	var ans struct {
		StatusCode int    `json:"statusCode"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal([]byte(r.posts["response"]), &ans); err != nil {
		t.Fatalf("response %q: %v", r.posts["response"], err)
	}
	if ans.StatusCode != 200 || !strings.Contains(ans.Body, `"path":"/mcp"`) || !strings.Contains(ans.Body, `"sub":"jwt:sub:agent-1"`) ||
		!strings.Contains(ans.Body, `"scopes":"rhobound:read"`) {
		t.Fatalf("answer %+v", ans)
	}
}

type tooLarge struct{}

func (tooLarge) Error() string { return "store.too_large" }
func (tooLarge) RefusalJSON() []byte {
	return []byte(`{"code":"store.too_large","reason":"over the cap","fix":"a shorter window"}`)
}

func TestFitWorkerRecordsAFailedJob(t *testing.T) {
	save := fitWorkerJob
	defer func() { fitWorkerJob = save }()
	var failed string
	root := t.TempDir()
	fitWorkerJob = func(ctx context.Context, args []string) (lambdart.Job, error) {
		return lambdart.Job{
			Root: root,
			Run:  func(ctx context.Context, event []byte, dir string) error { return tooLarge{} },
			Fail: func(ctx context.Context, event []byte, refusal []byte) error { failed = string(refusal); return nil },
		}, nil
	}
	r := newLambdaRuntime(t, `{"job_id":"job_01JBQX4Y9W2N7M6K5H3G2D1C0B","deadline_at":"`+
		time.Now().Add(time.Minute).UTC().Format(time.RFC3339)+`"}`)
	if err := r.run(t, "fit-worker"); !errors.Is(err, context.Canceled) {
		t.Fatalf("the loop ended with %v", err)
	}
	if !strings.Contains(failed, `"code":"store.too_large"`) || !strings.Contains(r.posts["error"], "store.too_large") {
		t.Fatalf("Fail got %q; posted error %q", failed, r.posts["error"])
	}
}

func TestFrontDoorClaimsFromEachAuthorizer(t *testing.T) {
	for _, c := range []struct {
		name string
		a    apigw.Authorizer
		want auth.Claims
		ok   bool
	}{
		{"JWT with scopes", apigw.Authorizer{Claims: map[string]string{"sub": "u-1"}, Scopes: []string{"rhobound:read", "rhobound:fit"}},
			auth.Claims{Subject: "sub:u-1", Scopes: []string{"rhobound:read", "rhobound:fit"}, Source: "jwt"}, true},
		{"Cognito resource-server scopes", apigw.Authorizer{Claims: map[string]string{"client_id": "client-a",
			"scope": "https://mcp.example.com/rhobound:read https://mcp.example.com/rhobound:fit"}},
			auth.Claims{Subject: "client:client-a", Scopes: []string{"rhobound:read", "rhobound:fit"}, Source: "jwt"}, true},
		// a user whose sub reads as a client's subject is not that client
		{"JWT whose sub names a client", apigw.Authorizer{Claims: map[string]string{"sub": "client:client-a", "client_id": "client-a"},
			Scopes: []string{"rhobound:read"}}, auth.Claims{Subject: "sub:client:client-a", Scopes: []string{"rhobound:read"}, Source: "jwt"}, true},
		// another resource server's scopes grant nothing here, nor do scopes nested under this one's
		{"another resource server's scopes", apigw.Authorizer{Claims: map[string]string{"sub": "u-2"},
			Scopes: []string{"https://mcp.example.com/rhobound:read", "https://other.example/rhobound:fit",
				"https://mcp.example.com/x/rhobound:fit", "other.example/rhobound:fit"}},
			auth.Claims{Subject: "sub:u-2", Scopes: []string{"rhobound:read"}, Source: "jwt"}, true},
		{"static token", apigw.Authorizer{Context: map[string]string{"repo": "shop", "scope": "rhobound:fit"}},
			auth.Claims{Subject: "shop", Scopes: []string{"rhobound:read"}, Source: "token"}, true},
		{"REST Lambda authorizer", apigw.Authorizer{PrincipalID: "repo-shop", Context: map[string]string{"integrationLatency": "0"}},
			auth.Claims{Subject: "repo-shop", Scopes: []string{"rhobound:read"}, Source: "token"}, true},
		{"AWS_IAM", apigw.Authorizer{IAM: &apigw.IAMCaller{UserARN: "arn:aws:iam::ACCOUNT:role/ci"}},
			auth.Claims{Subject: "arn:aws:iam::ACCOUNT:role/ci", Scopes: []string{"rhobound:read"}, Source: "iam"}, true},
		{"nothing", apigw.Authorizer{}, auth.Claims{}, false},
		{"JWT without a subject", apigw.Authorizer{Claims: map[string]string{"scope": "rhobound:read"}}, auth.Claims{}, false},
		// the container's verifier refuses the same tokens (auth TestJWTRefuses): a blank sub names no one, and a sub
		// or client_id that is not a string is refused, whatever else the token carries
		{"JWT with a blank sub", apigw.Authorizer{Claims: map[string]string{"sub": " ", "client_id": "client-a"}},
			auth.Claims{}, false},
		{"JWT whose sub is a number", apigw.Authorizer{Claims: map[string]string{"sub": "123"}, NotStrings: []string{"sub"}},
			auth.Claims{}, false},
		{"JWT whose client_id is a number", apigw.Authorizer{Claims: map[string]string{"client_id": "7"},
			NotStrings: []string{"client_id"}}, auth.Claims{}, false},
		{"JWT with an empty sub and a client", apigw.Authorizer{Claims: map[string]string{"sub": "", "client_id": "client-a"}},
			auth.Claims{Subject: "client:client-a", Source: "jwt"}, true},
	} {
		got, ok := claimsOf(c.a, "https://mcp.example.com")
		if ok != c.ok || (ok && (got.Subject != c.want.Subject || got.Source != c.want.Source ||
			strings.Join(got.Scopes, " ") != strings.Join(c.want.Scopes, " "))) {
			t.Errorf("%s: %+v %v, want %+v %v", c.name, got, ok, c.want, c.ok)
		}
	}
	// With no resource server configured, a prefixed scope is nobody's: the same rule as the container's verifier.
	got, _ := claimsOf(apigw.Authorizer{Claims: map[string]string{"sub": "u-3"},
		Scopes: []string{"rhobound:read", "https://mcp.example.com/rhobound:fit"}}, "")
	if strings.Join(got.Scopes, " ") != "rhobound:read" {
		t.Errorf("no resource server: %v", got.Scopes)
	}
}

// Through auth.FrontDoor: an event whose authorizer gave no claims is refused before the MCP handler, and so is one
// whose JWT claims have a sub that is not a string or is blank, as the container's verifier refuses such a token.
func TestLambdaModeRefusesAnEventWithoutClaims(t *testing.T) {
	save := lambdaHandler
	defer func() { lambdaHandler = save }()
	for _, authorizer := range []string{"", `,"authorizer":{"jwt":{"claims":{"sub":123,"scope":"rhobound:read"}}}`,
		`,"authorizer":{"jwt":{"claims":{"sub":" ","client_id":"client-a"},"scopes":["rhobound:read"]}}`} {
		reached := false
		lambdaHandler = func(ctx context.Context, args []string) (http.Handler, error) {
			return auth.FrontDoor(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { reached = true })), nil
		}
		r := newLambdaRuntime(t, `{"version":"2.0","rawPath":"/mcp","requestContext":{"http":{"method":"POST","path":"/mcp"},
			"stage":"$default"`+authorizer+`},"body":"{}","isBase64Encoded":false}`)
		if err := r.run(t, "lambda"); !errors.Is(err, context.Canceled) {
			t.Fatalf("the loop ended with %v", err)
		}
		if reached || !strings.Contains(r.posts["response"], `"statusCode":401`) {
			t.Fatalf("%s: reached %v, answer %s", authorizer, reached, r.posts["response"])
		}
	}
}
