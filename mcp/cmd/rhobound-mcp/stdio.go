// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/auth"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server"
)

func init() { register("stdio", runStdio) }

// runStdio serves one local agent on standard input and output: no network, no auth; the caller is the local user,
// and capture names from the configuration may be used directly (ARCHITECTURE.md 5.1).
func runStdio(ctx context.Context, args []string) error {
	return serveStdio(ctx, args, &mcp.StdioTransport{})
}

// serveStdio runs the stdio mode on a given transport (tests pass an in-memory one).
func serveStdio(ctx context.Context, args []string, t mcp.Transport) error {
	fs := flag.NewFlagSet("stdio", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "", "the server's configuration file (rhobound-mcp.config/v1)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: rhobound-mcp stdio --config FILE")
	}
	d, err := setup(*configPath)
	if err != nil {
		return err
	}
	local := auth.Local()
	srv, err := server.New(server.Options{
		Config: d.cfg, Runner: d.engine, Store: d.store, WorkDir: d.work, Version: Version,
		LocalCaptures: true,
		Identity:      func(context.Context) (auth.Identity, bool) { return local, true },
	})
	if err != nil {
		return err
	}
	defer sweep(ctx, srv)()
	return srv.MCP().Run(ctx, t)
}
