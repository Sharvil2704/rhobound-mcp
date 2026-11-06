// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

func init() {
	register("fit", command{run: withRefusal(cmdFit), usage: `  rhobound fit     CAPTURE|--manifest M --out DIR [--workers svc=N] [--kept-share F] [--pod-identity host]
                   [--request-count F] [--sampler svc=NAME] [--probe-route R] [--name NAME] [--plain] [--json]
                                            check, then fit once: writes DIR/fit.json (rhobound.fit/v1)
  rhobound predict --fit F [--patch svc=+3ms | --changes C] [--report v1] [--load-scale S] [--max-rho 0.85]
                   [--out DIR] [--json]
                                            predict from a fit: report v2 (--report v1: its v1 projection;
                                            --load-scale S: a what-if, before and after at S x the fit's rate)
`})
}

const fitUsage = "usage: rhobound fit CAPTURE|--manifest M --out DIR [--workers svc=N] [--kept-share F] [--pod-identity host] " +
	"[--request-count F] [--sampler svc=NAME] [--probe-route R] [--name NAME] [--plain] [--json]"

// cmdFit is `rhobound fit`: the capture through check (a refusal there refuses the fit, with its code), then the
// inference on check's data, written as DIR/fit.json: the record's canonical bytes, with no trailing newline, so that
// the file's sha256 is the record's digest. The digest is printed on standard output. The record names the capture as
// --name gives it, else as the capture argument (or the manifest's path) was given.
func cmdFit(args []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("fit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	manifest := fs.String("manifest", "", "the capture manifest to fit (its objects are relative to its folder)")
	outDir := fs.String("out", "", "the folder to write fit.json to")
	workers := fs.String("workers", "", "workers per pod, as svc=N,svc=N (default: inferred)")
	keptShare := fs.String("kept-share", "", "the declared share of requests the capture holds")
	podIdentity := fs.String("pod-identity", "", `"host": key the pods of resources with no pod or task identity by host.name`)
	count := fs.String("request-count", "", "an independent request count (default: request-count.json of the capture, if any)")
	plain := fs.Bool("plain", false, "read the hops with the basic model, and predict with it")
	asJSON := fs.Bool("json", false, `print {"fit_digest": ...}, or the coded refusal`)
	name := fs.String("name", "", "the capture's name in the record (default: the capture as given)")
	var routes, samplers listFlag
	fs.Var(&routes, "probe-route", "a route that is a probe (repeatable)")
	fs.Var(&samplers, "sampler", "svc=NAME: the SDK sampler of a service (repeatable)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 1 || (len(pos) == 1) == (*manifest != "") || *outDir == "" {
		return errors.New(fitUsage)
	}
	if err := recordVersion(); err != nil {
		return err
	}
	named := given(fs, "name")
	if named {
		if err := checkName("name", *name); err != nil {
			return err
		}
	}
	w, err := parseWorkers(*workers)
	if err != nil {
		return err
	}
	opt := check.Options{ExtraProbeRoutes: routes, PodIdentity: *podIdentity}
	fopt := fit.Options{Workers: w, PodIdentity: fit.PodIdentityDefault, Plain: *plain, ProbeRoutes: routes}
	if *podIdentity != "" {
		fopt.PodIdentity = *podIdentity
	}
	if *keptShare != "" {
		k, err := strconv.ParseFloat(*keptShare, 64)
		if err != nil || !(k > 0 && k <= 1) {
			return fmt.Errorf("--kept-share must be in (0, 1], got %q", *keptShare)
		}
		opt.DeclaredKeptShare, fopt.KeptShare = &k, &k
	}
	for _, s := range samplers {
		svc, name, ok := strings.Cut(s, "=")
		if !ok || svc == "" || name == "" {
			return fmt.Errorf("--sampler: %q is not svc=NAME", s)
		}
		if opt.DeclaredSamplers == nil {
			opt.DeclaredSamplers = map[string]string{}
		}
		opt.DeclaredSamplers[strings.TrimSpace(svc)] = strings.TrimSpace(name)
	}
	// the estimator is read here, once: the fit itself reads no environment variable, and records it
	if fopt.Estimator, err = estimator(); err != nil {
		return err
	}

	capture := *manifest
	var m *contract.Manifest
	if capture != "" {
		m, err = check.ReadManifest(capture)
	} else {
		capture = pos[0]
		m, _, err = check.ManifestFromDir(capture, otlp.Limits{})
	}
	if err != nil {
		return err
	}
	if opt.RequestCount, err = requestCount(*count, m.Dir); err != nil {
		return err
	}
	if opt.RequestCount != nil {
		fopt.RequestCount = opt.RequestCount.Digest
	}
	rep, d, err := check.Run(m, opt)
	if err != nil {
		return err
	}
	if rep.Verdict == contract.Refused {
		return checkRefusal(rep)
	}
	_, _, names, _, err := check.ClusterInputs(m)
	if err != nil {
		return err
	}
	if named {
		capture = *name
	}
	rec, err := fit.Build(fit.Input{Capture: capture, Data: d, Manifest: m, Check: rep, PeerNames: names, Options: fopt},
		fit.NewTool(Version))
	if err != nil {
		return err
	}
	b, err := rec.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(*outDir, fit.File)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(errw, "wrote %s (%s, %d findings)\n", p, rep.Verdict, len(rec.Findings))
	if *asJSON {
		fmt.Fprintf(out, "{\"fit_digest\":%q}\n", contract.Digest(b))
		return nil
	}
	fmt.Fprintln(out, contract.Digest(b))
	return nil
}

// given reports whether a flag was set on the command line.
func given(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// recordVersion refuses to write a record under this build's version when the records' rule does not admit it
// (fit.VersionRule): every build, this one included, would refuse to read the record (fit.version), so fit and
// diff-fit refuse up front, before they read the capture, and say the rule.
func recordVersion() error {
	if fit.CheckVersion(Version) {
		return nil
	}
	return fmt.Errorf("this build's version %q (-X main.Version) cannot be recorded: a version is %s; build again with "+
		"one", Version, fit.VersionRule)
}

// checkName checks the value of a name flag (--name, --baseline-name, --candidate-name), the capture name a record
// holds instead of the folder: the Name rule of the views (at most 128 characters from [A-Za-z0-9._:/@-], with ext:
// starting no word but the first), no arn: (contract.NamesARN: SafeName leaves a name that starts with ext: to the
// node rule, so it is checked apart), not a path from the root, and no empty, "." or ".." path segment. The MCP
// server's configuration takes the same names (config.ValidName; the tests of both pin the same answers).
func checkName(flagName, name string) error {
	s, untrusted := contract.SafeName(name)
	bad := untrusted || s != name || contract.NamesARN(name)
	for _, seg := range strings.Split(name, "/") {
		bad = bad || seg == "" || seg == "." || seg == ".."
	}
	if bad {
		return fmt.Errorf("--%s: a capture name is 1 to 128 characters from [A-Za-z0-9._:/@-], does not start with /, "+
			"has no empty, . or .. path segment, has ext: (a peer's prefix) at the start of no word but the first, and "+
			"holds no arn: (an AWS resource name) at its start, after a character that is not a letter or a digit, or "+
			"before an AWS partition", flagName)
	}
	return nil
}

// requestCount reads the independent request count: the file given, else request-count.json in the capture's folder
// when there is one, else none.
func requestCount(path, dir string) (*check.RequestCount, error) {
	if path == "" {
		if p := filepath.Join(dir, "request-count.json"); fileExists(p) {
			path = p
		}
	}
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rc, err := check.ParseRequestCount(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rc, nil
}

// checkRefusal is the refusal of a capture check refused: its first refusal, with the code, the subject and the
// numbers, and a message naming each refusal and its fix.
func checkRefusal(rep *contract.CheckReport) error {
	var first *contract.Finding
	var parts []string
	for k := range rep.Findings {
		f := &rep.Findings[k]
		if f.Level != contract.Refuse {
			continue
		}
		if first == nil {
			first = f
		}
		what := f.Code
		if f.Subject != "" {
			what += " " + f.Subject
		}
		if c, ok := contract.Lookup(f.Code); ok && c.When != "" {
			what += ": " + c.When
		}
		parts = append(parts, what+" (fix: "+f.Fix+")")
	}
	if first == nil {
		return errors.New("check refused the capture")
	}
	return &report.RefusalError{Code: first.Code, Subject: first.Subject, Numbers: first.Numbers,
		Err: fmt.Errorf("check refused the capture: %s", strings.Join(parts, "; "))}
}
