// SPDX-License-Identifier: Apache-2.0

// Command rhobound-mcp serves the rhobound CLI to coding agents over the Model Context Protocol: "stdio" for a local
// agent, which starts it as a child process, and "http" for a team server. It runs the rhobound binary named in its
// configuration as a subprocess and never computes a number itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

// modes are the ways to run the server, by name. Each mode's file registers it from its init() with register.
var modes = map[string]func(ctx context.Context, args []string) error{}

// register adds a mode. A name registered twice is a programming error and panics.
func register(name string, run func(ctx context.Context, args []string) error) {
	if _, dup := modes[name]; dup {
		panic("rhobound-mcp: mode " + name + " registered twice")
	}
	modes[name] = run
}

func init() {
	register("version", func(_ context.Context, args []string) error { return version(args, os.Stdout) })
}

// version prints "rhobound-mcp VERSION", or with --json the build information as one canonical JSON line: the same
// keys as `rhobound version --json` (name, version, go, goarch, and vcs when the go command stamped the source).
func version(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "print the build information as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("usage: rhobound-mcp version [--json]")
	}
	if !*asJSON {
		_, err := fmt.Fprintln(w, "rhobound-mcp", Version)
		return err
	}
	bi, _ := debug.ReadBuildInfo()
	b, err := json.Marshal(versionOf(bi))
	if err != nil {
		return err
	}
	if b, err = server.Canonical(b); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// buildInfo is what `rhobound-mcp version --json` prints.
type buildInfo struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Go      string   `json:"go"`
	GOARCH  string   `json:"goarch"`
	VCS     *vcsInfo `json:"vcs,omitempty"`
}

type vcsInfo struct {
	Revision string `json:"revision"`
	Time     string `json:"time"`
	Modified bool   `json:"modified"`
}

// versionOf is the build information of bi (nil: none was embedded).
func versionOf(bi *debug.BuildInfo) buildInfo {
	v := buildInfo{Name: "rhobound-mcp", Version: Version, Go: runtime.Version(), GOARCH: runtime.GOARCH}
	if bi == nil {
		return v
	}
	if bi.GoVersion != "" {
		v.Go = bi.GoVersion
	}
	var vcs vcsInfo
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			vcs.Revision = s.Value
		case "vcs.time":
			vcs.Time = s.Value
		case "vcs.modified":
			vcs.Modified = s.Value == "true"
		}
	}
	if vcs.Revision != "" {
		v.VCS = &vcs
	}
	return v
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	args := os.Args[1:]
	if len(args) == 0 {
		args = lambdaArgs(os.Getenv)
	}
	code := run(ctx, args, os.Stderr)
	stop()
	os.Exit(code)
}

// lambdaArgs are the arguments of a process that AWS Lambda started: its bootstrap runs with none, and the function's
// Handler setting (_HANDLER) names the mode, "lambda" or "fit-worker". Outside Lambda, or with another handler, there
// are none, and run prints the usage.
func lambdaArgs(getenv func(string) string) []string {
	if getenv("AWS_LAMBDA_RUNTIME_API") == "" {
		return nil
	}
	switch h := getenv("_HANDLER"); h {
	case "lambda", "fit-worker":
		return []string{h}
	}
	return nil
}

// exitStatus is an error a mode returns to exit with that status and print nothing (muse-hook's 10).
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// run dispatches to a mode: exit 0, 1 when the mode fails, 2 for a usage error, or the mode's own exitStatus.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	m, ok := modes[args[0]]
	if !ok {
		usage(stderr)
		return 2
	}
	if err := m(ctx, args[1:]); err != nil {
		var st exitStatus
		if errors.As(err, &st) {
			return int(st)
		}
		fmt.Fprintf(stderr, "rhobound-mcp: %v\n", err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	names := make([]string, 0, len(modes))
	for n := range modes {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "usage: rhobound-mcp MODE [flags]\nmodes: %s\n"+
		"  rhobound-mcp stdio --config FILE                 serve one local agent on standard input and output\n"+
		"  rhobound-mcp http  --config FILE [--listen ADDR] serve over HTTP (127.0.0.1:8000 by default)\n"+
		"  rhobound-mcp version [--json]                    the version, and with --json the build information\n"+
		"  rhobound-mcp init [--engine FILE ...] [--write]  check, write and register a configuration with Muse Code\n"+
		"  rhobound-mcp refit --config FILE --env NAME --window START,END|--last D [--request-count F] [--json]\n"+
		"                                                   an environment's baseline fit from its trace store\n"+
		"  rhobound-mcp muse-hook api|session-start|stop    the Muse Code plugin's hooks (exit 0, or 10 to remind)\n",
		strings.Join(names, ", "))
}

// sweep runs the server's sweeper (server.RunSweeper) until the mode ends: the returned func stops it and waits for it.
func sweep(ctx context.Context, srv *server.Server) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.RunSweeper(ctx, server.DefaultSweepEvery)
	}()
	return func() {
		cancel()
		<-done
	}
}

// deps are what every mode builds from the configuration.
type deps struct {
	cfg    *config.Config
	engine *runner.Exec
	store  *state.Dir
	work   string // folder for per-job output folders
}

// engineTimeout bounds one engine call; a fit job is bounded by its own deadline as well.
const engineTimeout = 5 * time.Minute

// setup loads the configuration, checks the engine's sha256 (server.engine_mismatch refuses to start), and opens
// the state folder. Layout under state_dir: server/ (handles, jobs, artifacts, the preview log), engine/ (the CLI's
// own record store, RHOBOUND_STATE) and work/ (per-job folders).
func setup(configPath string) (*deps, error) {
	if configPath == "" {
		return nil, fmt.Errorf("--config is required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	dirs := map[string]string{}
	for _, sub := range []string{"server", "engine", "work"} {
		dirs[sub] = filepath.Join(cfg.StateDir, sub)
		if err := os.MkdirAll(dirs[sub], 0o700); err != nil {
			return nil, err
		}
	}
	engine, err := runner.NewExec(cfg.Engine.Path, cfg.Engine.SHA256, engineTimeout,
		[]string{"RHOBOUND_STATE=" + dirs["engine"]})
	var em *runner.EngineMismatchError
	if errors.As(err, &em) {
		return nil, fmt.Errorf("%s: %w", state.CodeEngineMismatch, err)
	}
	if err != nil {
		return nil, err
	}
	engine.TempRoot = dirs["work"]
	store, err := state.NewDir(dirs["server"], nil)
	if err != nil {
		return nil, err
	}
	return &deps{cfg: cfg, engine: engine, store: store, work: dirs["work"]}, nil
}
