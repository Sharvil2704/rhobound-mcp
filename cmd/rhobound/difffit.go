// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

func init() {
	register("diff-fit", command{run: withRefusal(cmdDiffFit), usage: `  rhobound diff-fit --baseline A --candidate B --out DIR [--loadgen-log L] [--workers svc=N] [--margin-ms M]
                    [--batches N] [--baseline-name NAME] [--candidate-name NAME] [--json]
                                            an A/B run's per-service difference: DIR/ab-record.json
`})
}

const diffFitUsage = "usage: rhobound diff-fit --baseline A --candidate B --out DIR [--loadgen-log L] [--workers svc=N] " +
	"[--margin-ms M] [--batches N] [--baseline-name NAME] [--candidate-name NAME] [--json]"

// LoadgenFile is the load generator's log diff-fit reads from an arm's folder when --loadgen-log is not given: the
// baseline's first, then the candidate's.
const LoadgenFile = "loadgen.csv"

// cmdDiffFit is `rhobound diff-fit`: the two arms of a concurrent A/B run, each a capture checked and inferred with its
// per-visit samples kept, and the load generator's per-request log, give the A/B record (rhobound.ab-record/v1,
// docs/mcp/DATA.md 8). A refusal in the record (ab.not_concurrent, ab.hardware_differs, ab.sampled) refuses the run and
// mints nothing. Otherwise the record is written to DIR/ab-record.json, its canonical bytes, and into the CLI's record
// store, where a change set's measured-ab item may cite it by digest; the digest is printed on standard output. The
// record names each arm's capture as --baseline-name and --candidate-name give it, else as its folder was given.
func cmdDiffFit(args []string, out, errw io.Writer) error {
	fs := flag.NewFlagSet("diff-fit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	baseline := fs.String("baseline", "", "the baseline arm's capture (the image production runs now)")
	candidate := fs.String("candidate", "", "the candidate arm's capture")
	outDir := fs.String("out", "", "the folder to write ab-record.json to")
	logPath := fs.String("loadgen-log", "", "the load generator's per-request log (default: "+LoadgenFile+" of an arm)")
	workers := fs.String("workers", "", "workers per pod in both arms, as svc=N,svc=N (default: inferred)")
	margin := fs.Float64("margin-ms", 0, "the gate's margin on the difference, ms per visit: the visits each arm needs")
	batches := fs.Int("batches", abdiff.DefaultBatches, "batches for the standard errors")
	asJSON := fs.Bool("json", false, `print {"ab_digest": ...}, or the coded refusal`)
	baselineName := fs.String("baseline-name", "", "the baseline's capture name in the record (default: the folder as given)")
	candidateName := fs.String("candidate-name", "", "the candidate's capture name in the record (default: the folder as given)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) != 0 || *baseline == "" || *candidate == "" || *outDir == "" || !(*margin >= 0) {
		return errors.New(diffFitUsage)
	}
	if err := recordVersion(); err != nil {
		return err
	}
	names := [2]string{*baseline, *candidate}
	for k, n := range []struct{ flag, value string }{{"baseline-name", *baselineName}, {"candidate-name", *candidateName}} {
		if !given(fs, n.flag) {
			continue
		}
		if err := checkName(n.flag, n.value); err != nil {
			return err
		}
		names[k] = n.value
	}
	w, err := parseWorkers(*workers)
	if err != nil {
		return err
	}
	est, err := estimator()
	if err != nil {
		return err
	}
	if *logPath == "" {
		for _, dir := range []string{*baseline, *candidate} {
			if p := filepath.Join(dir, LoadgenFile); fileExists(p) {
				*logPath = p
				break
			}
		}
		if *logPath == "" {
			return fmt.Errorf("no load generator log: give --loadgen-log, or put %s in an arm's folder", LoadgenFile)
		}
	}
	b, err := os.ReadFile(*logPath)
	if err != nil {
		return err
	}
	log, err := abdiff.ParseLoadgenLog(b)
	if err != nil {
		return err
	}
	var arms [2]*abdiff.Input
	for k, capture := range []string{*baseline, *candidate} {
		if arms[k], err = abArm(capture, names[k], w, est); err != nil {
			return err
		}
	}
	rec, err := abdiff.Diff(arms[0], arms[1], log, abdiff.Options{Batches: *batches, MarginMs: *margin}, fit.NewTool(Version))
	if err != nil {
		return err
	}
	for _, f := range rec.Findings {
		if f.Level == contract.Refuse {
			return &report.RefusalError{Code: f.Code, Subject: f.Subject, Numbers: f.Numbers,
				Err: fmt.Errorf("the A/B run is refused (%s), and no record is minted: %s", f.Code, whenOf(f.Code))}
		}
	}
	rb, err := rec.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	// minted first: a record the store did not take is not left in DIR, where it would look like one a change set can
	// cite
	digest, err := openRecords().mint(rb)
	if err != nil {
		return err
	}
	p := filepath.Join(*outDir, abdiff.File)
	if err := os.WriteFile(p, rb, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(errw, "wrote %s (%d services, %d findings) and minted it in the record store\n", p, len(rec.Services), len(rec.Findings))
	if *asJSON {
		fmt.Fprintf(out, "{\"ab_digest\":%q}\n", digest)
		return nil
	}
	fmt.Fprintln(out, digest)
	return nil
}

func whenOf(code string) string {
	if c, ok := contract.Lookup(code); ok {
		return c.When
	}
	return code
}

// estimator is the estimator RHOBOUND_ESTIMATOR names, resolved.
func estimator() (string, error) {
	switch est := os.Getenv("RHOBOUND_ESTIMATOR"); est {
	case "", infer.EstimatorFIFO:
		return infer.EstimatorFIFO, nil
	case infer.EstimatorRise:
		return infer.EstimatorRise, nil
	default:
		return "", fmt.Errorf("unknown estimator %q (want %q or %q)", est, infer.EstimatorFIFO, infer.EstimatorRise)
	}
}

// abArm checks one arm's capture (a refusal there refuses the run) and infers it with its samples kept; name is what
// the record calls the arm.
func abArm(capture, name string, workers map[string]int, est string) (*abdiff.Input, error) {
	m, _, err := check.ManifestFromDir(capture, otlp.Limits{})
	if err != nil {
		return nil, err
	}
	opt := check.Options{}
	if opt.RequestCount, err = requestCount("", m.Dir); err != nil {
		return nil, err
	}
	rep, d, err := check.Run(m, opt)
	if err != nil {
		return nil, err
	}
	if rep.Verdict == contract.Refused {
		return nil, fmt.Errorf("%s: %w", capture, checkRefusal(rep))
	}
	in := &abdiff.Input{Capture: name, Manifest: m, Check: rep, Images: fit.Images(d)}
	if dump, _, _, _, err := check.ClusterInputs(m); err == nil && dump != nil {
		var hw []string
		for _, n := range dump.Environment().Nodes {
			if s := n.InstanceType + "/" + n.Arch; !contains(hw, s) {
				hw = append(hw, s)
			}
		}
		sort.Strings(hw)
		in.Hardware = strings.Join(hw, ",")
	}
	// --workers may name a service as the views show it (infer.ShownWorkers)
	if workers, err = infer.ShownWorkers(d, workers); err != nil {
		return nil, fmt.Errorf("%s: %w", capture, err)
	}
	if in.Inferred, err = infer.Run(d, infer.Options{KeptShare: rep.KeptShare.Value, Workers: workers, Estimator: est,
		KeepSamples: true}); err != nil {
		return nil, fmt.Errorf("%s: %w", capture, err)
	}
	return in, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
