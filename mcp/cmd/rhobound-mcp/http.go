// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server"
)

func init() { register("http", runHTTP) }

func runHTTP(ctx context.Context, args []string) error { return serveHTTP(ctx, args, nil) }

// serveHTTP runs the HTTP mode (container mode: the server checks bearer tokens itself) until ctx ends. ready, when
// set, receives the bound address (tests listen on port 0).
func serveHTTP(ctx context.Context, args []string, ready func(addr string)) error {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		configPath = fs.String("config", "", "the server's configuration file (rhobound-mcp.config/v1)")
		listen     = fs.String("listen", "127.0.0.1:8000", "address to listen on")
		resource   = fs.String("resource", "", "this server's public URL, for the protected resource metadata")
		issuer     = fs.String("jwt-issuer", "", "accept RS256 access tokens from this issuer")
		jwksURL    = fs.String("jwks-url", "", "the issuer's JSON Web Key Set")
		audience   = fs.String("jwt-audience", "", "the audience tokens must carry")
		clientIDs  = fs.String("jwt-client-ids", "", "comma-separated client ids accepted from tokens without an audience")
		tokenUse   = fs.String("jwt-token-use", "", `required token_use claim (Cognito: "access")`)
		resServer  = fs.String("jwt-resource-server", "", "resource-server identifier ID whose scopes ID/rhobound:* count")
		tokens     = fs.String("static-tokens", "", `file of hashed read-only tokens: {"tokens":[{"sha256","repo","expires"}]}`)
	)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: rhobound-mcp http --config FILE [--listen ADDR] [--jwt-issuer URL --jwks-url URL " +
			"(--jwt-audience A | --jwt-client-ids IDS) [--jwt-token-use U] [--jwt-resource-server ID]] " +
			"[--static-tokens FILE] [--resource URL]")
	}
	verify, err := verifier(jwtFlags{issuer: *issuer, jwksURL: *jwksURL, audience: *audience, clientIDs: *clientIDs,
		tokenUse: *tokenUse, resourceServer: *resServer}, *tokens)
	if err != nil {
		return err
	}
	d, err := setup(*configPath)
	if err != nil {
		return err
	}
	srv, err := server.New(server.Options{Config: d.cfg, Runner: d.engine, Store: d.store, WorkDir: d.work,
		Version: Version})
	if err != nil {
		return err
	}
	defer sweep(ctx, srv)()
	opt := server.HTTPOptions{}
	var prm string
	if *resource != "" {
		prm = strings.TrimSuffix(*resource, "/mcp") + "/.well-known/oauth-protected-resource"
		meta := &oauthex.ProtectedResourceMetadata{Resource: *resource, ScopesSupported: []string{auth.ScopeRead, auth.ScopeFit},
			BearerMethodsSupported: []string{"header"}, ResourceName: "rhobound-mcp"}
		if *issuer != "" {
			meta.AuthorizationServers = []string{*issuer}
		}
		opt.ResourceMetadata = sdkauth.ProtectedResourceMetadataHandler(meta)
	}
	opt.Auth = auth.Bearer(verify, auth.BearerOptions{ResourceMetadataURL: prm, ClockSkew: 30 * time.Second})
	h, err := srv.HTTPHandler(opt)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	if ready != nil {
		ready(ln.Addr().String())
	}
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 2 * time.Minute, IdleTimeout: time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := hs.Shutdown(shut); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// jwtFlags are the http mode's JWT flags.
type jwtFlags struct {
	issuer, jwksURL, audience, clientIDs, tokenUse, resourceServer string
}

// verifier builds the token check from the flags: JWT with JWKS, hashed static tokens, or both (a token is accepted
// when either accepts it). An HTTP server with neither refuses to start.
func verifier(j jwtFlags, tokensFile string) (sdkauth.TokenVerifier, error) {
	var vs []sdkauth.TokenVerifier
	if j.issuer != "" || j.jwksURL != "" {
		if j.issuer == "" || j.jwksURL == "" {
			return nil, errors.New("--jwt-issuer and --jwks-url go together")
		}
		var ids []string
		if j.clientIDs != "" {
			ids = strings.Split(j.clientIDs, ",")
		}
		v, err := auth.JWT(auth.JWTConfig{Issuer: j.issuer, Audience: j.audience, ClientIDs: ids, TokenUse: j.tokenUse,
			ResourceServer: j.resourceServer, Keys: &auth.JWKS{URL: j.jwksURL, HTTP: &http.Client{Timeout: 10 * time.Second}}})
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	if tokensFile != "" {
		b, err := os.ReadFile(tokensFile)
		if err != nil {
			return nil, err
		}
		var f struct {
			Tokens []struct {
				SHA256  string    `json:"sha256"`
				Repo    string    `json:"repo"`
				Expires time.Time `json:"expires"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("--static-tokens: %w", err)
		}
		var list []auth.StaticToken
		for _, t := range f.Tokens {
			list = append(list, auth.StaticToken{SHA256: t.SHA256, Repo: t.Repo, Expires: t.Expires})
		}
		v, err := auth.StaticTokens(list, nil)
		if err != nil {
			return nil, err
		}
		vs = append(vs, v)
	}
	switch len(vs) {
	case 0:
		return nil, errors.New("an HTTP server needs --jwt-issuer with --jwks-url, or --static-tokens")
	case 1:
		return vs[0], nil
	}
	return func(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
		var last error
		for _, v := range vs {
			ti, err := v(ctx, token, r)
			if err == nil {
				return ti, nil
			}
			last = err
		}
		return nil, last
	}, nil
}
