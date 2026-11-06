// SPDX-License-Identifier: Apache-2.0

// Command rhobound predicts mean latency of a service call graph from
// OpenTelemetry traces, and the effect of a change in one service's time.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// Version is set at build time with -ldflags "-X main.Version=...".
var Version = "0.1.0-dev"

// command is one subcommand: run gets the arguments after its name and returns the exit status; usage is its lines
// in the help ("" for an alias, which the help does not list).
type command struct {
	run   func(args []string, stdout, stderr io.Writer) int
	usage string
}

// commands are the subcommands by name. main.go registers its own in init(); a subcommand in another file registers
// itself from that file's init() with register.
var commands = map[string]command{}

// register adds a subcommand. A name registered twice is a programming error and panics.
func register(name string, c command) {
	if _, dup := commands[name]; dup {
		panic("rhobound: command " + name + " registered twice")
	}
	commands[name] = c
}

func init() {
	register("predict", command{run: withRefusal(cmdPredict), usage: `  rhobound predict CAPTURE [--patch svc=+3ms,other=+10%] [--workers svc=N] [--kept-share F]
                           [--max-rho 0.85] [--out DIR] [--json] [--plain]
`})
	register("infer", command{run: withRefusal(cmdInfer),
		usage: "  rhobound infer   CAPTURE [--workers svc=N]        print the inferred call graph (JSON)\n"})
	register("model", command{run: withRefusal(cmdModel),
		usage: "  rhobound model   TOPOLOGY.json --rps N [--patch ...] [--max-rho 0.85] [--out DIR] [--json] [--plain]\n"})
	version := func(args []string, out, errw io.Writer) int {
		if jsonRequested(args) {
			b, err := contract.Canonical(versionInfo())
			if err != nil {
				fmt.Fprintln(errw, "rhobound:", err)
				return 1
			}
			fmt.Fprintf(out, "%s\n", b)
			return 0
		}
		fmt.Fprintln(out, "rhobound", Version)
		return 0
	}
	register("version", command{run: version, usage: "  rhobound version\n"})
	register("--version", command{run: version})
	register("-v", command{run: version})
	help := func(_ []string, out, _ io.Writer) int {
		fmt.Fprint(out, usageText())
		return 0
	}
	register("help", command{run: help})
	register("--help", command{run: help})
	register("-h", command{run: help})
}

// buildInfo is what `rhobound version --json` prints: the name and version, the Go release and GOARCH the binary was
// built with, and the version control state the go command stamped into it (vcs, absent when it stamped none).
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

// versionInfo is the binary's own build information.
func versionInfo() buildInfo {
	bi, _ := debug.ReadBuildInfo()
	return versionOf(bi)
}

// versionOf is the build information of bi (nil: none was embedded).
func versionOf(bi *debug.BuildInfo) buildInfo {
	v := buildInfo{Name: "rhobound", Version: Version, Go: runtime.Version(), GOARCH: runtime.GOARCH}
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

// withError adapts a subcommand that returns an error: the error is printed as "rhobound: <error>" and the exit
// status is 1.
func withError(f func(args []string, stdout, stderr io.Writer) error) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		if err := f(args, stdout, stderr); err != nil {
			fmt.Fprintln(stderr, "rhobound:", err)
			return 1
		}
		return 0
	}
}

// firstCommands are listed first in the help, in this order; version is listed last, and every other registered
// command with a usage between them, by name.
var firstCommands = []string{"predict", "infer", "model"}

// usageText is the help: usageHead, the usage lines of the registered commands, usageNotes.
func usageText() string {
	var rest []string
	for name, c := range commands {
		if c.usage != "" && name != "version" && !slices.Contains(firstCommands, name) {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	var b strings.Builder
	b.WriteString(usageHead)
	for _, name := range append(append(append([]string{}, firstCommands...), rest...), "version") {
		b.WriteString(commands[name].usage)
	}
	b.WriteString(usageNotes)
	return b.String()
}

const usageHead = `rhobound - mean latency from OpenTelemetry traces

Usage:
`

const usageNotes = `
CAPTURE is a folder with traces/ (OTLP/JSON lines, plain or .gz), or a trace file.
--patch svc=+3ms adds work that holds the worker to every visit; svc=+10% scales
the service's whole own time per visit, glue (time after its first call) included.
--plain turns off the refinements for 70-80% utilization (arrival variability
carried through the graph, correlated parallel branches); see docs/ACCURACY.md.
It models mean latency under steady, roughly Poisson load. It refuses a load
(the observed one, or the one after the change) at which a service is above
--max-rho utilization, and does not model tails, bursts, retries or timeouts.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes one command line and returns the exit status.
func run(args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprint(errw, usageText())
		return 2
	}
	c, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(errw, "unknown command %q\n\n%s", args[0], usageText())
		return 2
	}
	return c.run(args[1:], out, errw)
}

// parseInterleaved parses flags that may follow positional arguments.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type common struct {
	workers, patch *string
	keptShare      *float64
	maxRho         *float64
	out            *string
	asJSON         *bool
	plain          *bool
}

func newFlags(name string) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c := &common{
		workers:   fs.String("workers", "", "workers per pod, as svc=N,svc=N (default: inferred)"),
		patch:     fs.String("patch", "", "change, as svc=+3ms (extra work holding the worker),svc=-10% (the whole own time, glue included)"),
		keptShare: fs.Float64("kept-share", 1, "share of requests the capture holds (1 = all)"),
		maxRho:    fs.Float64("max-rho", 0.85, "refuse when any service is busier than this"),
		out:       fs.String("out", "", "write report.txt and report.json into this folder"),
		asJSON:    fs.Bool("json", false, "print the JSON report instead of the text one"),
		plain:     fs.Bool("plain", false, "use the basic model (Poisson arrivals everywhere, exponential branches)"),
	}
	return fs, c
}

func parseWorkers(s string) (map[string]int, error) {
	out := map[string]int{}
	for _, kv := range splitList(s) {
		k, v, ok := strings.Cut(kv, "=")
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if !ok || err != nil || n < 1 {
			return nil, fmt.Errorf("--workers: %q is not svc=N with N >= 1", kv)
		}
		out[strings.TrimSpace(k)] = n
	}
	return out, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func load(path string, c *common) (*infer.Inferred, error) {
	if k := *c.keptShare; !(k > 0 && k <= 1) {
		return nil, fmt.Errorf("--kept-share must be in (0, 1], got %v", k)
	}
	w, err := parseWorkers(*c.workers)
	if err != nil {
		return nil, err
	}
	d, err := otlp.ReadDir(path)
	if err != nil {
		return nil, err
	}
	return infer.Run(d, infer.Options{KeptShare: *c.keptShare, Workers: w, Plain: *c.plain})
}

func cmdInfer(args []string, out, errw io.Writer) error {
	fs, c := newFlags("infer")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("usage: rhobound infer CAPTURE [--workers svc=N]")
	}
	inf, err := load(pos[0], c)
	if err != nil {
		return err
	}
	b, err := model.MarshalTopology(inf.Topology)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, string(b))
	for _, w := range inf.Warnings {
		fmt.Fprintln(errw, "warning:", w)
	}
	fmt.Fprintf(errw, "external load %.2f requests/s over %.0f s\n", inf.LamExtPerMs*1000, inf.WindowS)
	return nil
}

func cmdModel(args []string, out, errw io.Writer) error {
	fs, c := newFlags("model")
	rps := fs.Float64("rps", 0, "external request rate, requests per second")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 1 || !(*rps > 0) || math.IsInf(*rps, 1) {
		return fmt.Errorf("usage: rhobound model TOPOLOGY.json --rps N [--patch svc=+3ms] [--max-rho 0.85] [--out DIR] [--json]")
	}
	// the topology fixes the worker counts and holds no capture to sample
	var unused []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "workers" || f.Name == "kept-share" {
			unused = append(unused, "--"+f.Name)
		}
	})
	if len(unused) > 0 {
		return fmt.Errorf("model does not take %s (they apply to a capture: predict and infer)", strings.Join(unused, " or "))
	}
	if !(*c.maxRho > 0 && *c.maxRho <= 0.95) { // also refuses NaN
		return fmt.Errorf("--max-rho must be in (0, 0.95]")
	}
	topo, err := model.LoadTopology(pos[0])
	if err != nil {
		return err
	}
	lam := *rps / 1000
	inf := &infer.Inferred{Topology: topo, LamExtPerMs: lam}
	for _, s := range topo.Services {
		inf.Stats = append(inf.Stats, infer.ServiceStats{Name: s.Name, Entry: false})
	}
	for i := range topo.Entry {
		inf.Stats[i].Entry = true
	}
	cfg := modelConfig(c)
	base, err := model.AnalyticWith(topo, lam, nil, cfg)
	if err != nil {
		return err
	}
	if mr, _ := base.MaxRho(); !base.Stable || !(mr <= *c.maxRho) {
		return engine.LimitError(contract.ModelUtilizationObserved, fmt.Sprintf("refusing to evaluate: at %g requests/s", *rps), inf, base, *c.maxRho)
	}
	patch, changes, err := parsePatch(*c.patch, topo)
	if err != nil {
		return err
	}
	var delta *model.Delta
	if patch.DS != nil {
		if delta, err = model.ChangePatchWith(topo, lam, patch, cfg); err != nil {
			return err
		}
		if mr, _ := delta.Patched.MaxRho(); !delta.Stable || !(mr <= *c.maxRho) {
			return engine.LimitError(contract.ModelUtilizationAfter, "refusing the change: after it", inf, delta.Patched, *c.maxRho)
		}
	}
	rep := report.Build(pos[0], inf, 1, base, delta, changes)
	rep.Model = modelName(c, topo)
	rep.FromTopology = true
	return writeReport(rep, c, out, errw)
}

func modelConfig(c *common) model.Config { return engine.Config(*c.plain) }

func modelName(c *common, t *model.Topology) string { return engine.ModelNameFor(*c.plain, t) }
