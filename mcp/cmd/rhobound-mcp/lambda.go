// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/apigw"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/lambdart"
)

// Lambda mode (DEPLOY_AWS.md 2.2): the MCP server's HTTP handler behind API Gateway, served by the Lambda Runtime
// API loop through the in-process event adapter. No listener is opened.
func init() { register("lambda", runLambda) }

// lambdaHandler builds the MCP server's HTTP handler for Lambda mode: the stateless, JSON-response handler of the
// HTTP mode with auth.FrontDoor as its auth (server.HTTPOptions{Auth: auth.FrontDoor}), on the AWS state store.
// The AWS bindings set it; while it is nil, Lambda mode reports an init error. runLambda puts the authorizer's
// claims in each request's context (frontDoorClaims) before the handler sees it.
var lambdaHandler func(ctx context.Context, args []string) (http.Handler, error)

func runLambda(ctx context.Context, args []string) error {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		return errors.New("lambda mode runs only inside AWS Lambda (AWS_LAMBDA_RUNTIME_API is not set)")
	}
	if lambdaHandler == nil {
		return lambdaInitFailed(api, errors.New("lambda mode is not wired into this build"))
	}
	h, err := lambdaHandler(ctx, args)
	if err != nil {
		return lambdaInitFailed(api, err)
	}
	return lambdart.Serve(ctx, api, apigw.Handler(frontDoorClaims(h, os.Getenv(resourceServerEnv))))
}

// resourceServerEnv names the Lambda function's environment variable that holds this server's resource-server
// identifier, as --jwt-resource-server does in container mode.
const resourceServerEnv = "RHOBOUND_MCP_JWT_RESOURCE_SERVER"

// frontDoorClaims turns what API Gateway's authorizer established into the claims auth.FrontDoor reads
// (ARCHITECTURE.md 5.2, DEPLOY_AWS.md 2.1). A JWT authorizer's token gives its subject (auth.JWTSubject, which refuses
// a blank sub, and a sub or client_id that is not a string is refused before it) and its scopes as auth.ResourceScopes
// keeps them for resourceServer: "https://mcp.example.com/rhobound:read" counts as rhobound:read when resourceServer
// is "https://mcp.example.com", and a scope under another prefix is ignored. The static-token Lambda authorizer gives
// its repository (or principal id) and only rhobound:read, as header-only clients get; an AWS_IAM caller gives its ARN
// and rhobound:read. A request without any of them carries no claims, and FrontDoor refuses it.
func frontDoorClaims(next http.Handler, resourceServer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rc, ok := apigw.RequestContextFrom(r.Context()); ok {
			if c, ok := claimsOf(rc.Authorizer, resourceServer); ok {
				r = r.WithContext(auth.WithClaims(r.Context(), c))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func claimsOf(a apigw.Authorizer, resourceServer string) (auth.Claims, bool) {
	switch {
	case len(a.Claims) > 0:
		// as the container mode's JWT verifier names the caller, and refuses a token whose sub or client_id is not a
		// string, which it cannot decode
		if slices.Contains(a.NotStrings, "sub") || slices.Contains(a.NotStrings, "client_id") {
			return auth.Claims{}, false
		}
		sub := auth.JWTSubject(a.Claims["sub"], a.Claims["client_id"])
		scopes := a.Scopes
		if len(scopes) == 0 {
			scopes = strings.Fields(a.Claims["scope"])
		}
		return auth.Claims{Subject: sub, Scopes: auth.ResourceScopes(resourceServer, scopes), Source: "jwt"}, sub != ""
	case len(a.Context) > 0 || a.PrincipalID != "":
		sub := a.Context["repo"]
		if sub == "" {
			sub = a.PrincipalID
		}
		return auth.Claims{Subject: sub, Scopes: []string{auth.ScopeRead}, Source: "token"}, sub != ""
	case a.IAM != nil:
		sub := a.IAM.UserARN
		if sub == "" {
			sub = a.IAM.CallerID
		}
		return auth.Claims{Subject: sub, Scopes: []string{auth.ScopeRead}, Source: "iam"}, sub != ""
	}
	return auth.Claims{}, false
}

// lambdaInitFailed reports a failed start to Lambda, which then fails the waiting invocation, and returns the
// error for the process to exit with.
func lambdaInitFailed(api string, err error) error {
	if rerr := lambdart.InitError(api, err); rerr != nil {
		return errors.Join(err, rerr)
	}
	return err
}
