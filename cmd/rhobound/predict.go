// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// cmdPredict is `rhobound predict`. With a capture it reads the capture and prints report v1, exactly as rhobound-cli
// has always done. With --fit it predicts from a fit record (`rhobound fit`) and a change (--patch, or a change set
// with --changes) and prints report v2, or its v1 projection with --report v1; --load-scale S solves it at S times the
// fit's external rate, a what-if.
func cmdPredict(args []string, out, errw io.Writer) error {
	fs, c := newFlags("predict")
	fitPath := fs.String("fit", "", "predict from this fit record (rhobound fit) instead of a capture: report v2")
	changes := fs.String("changes", "", "with --fit: the change set (rhobound.change/v1) to apply")
	form := fs.String("report", "", "with --fit: v1 prints the v1 projection of report v2")
	loadScale := fs.Float64("load-scale", 1, "with --fit: solve before and after the change at this multiple of the fit's external rate (a what-if)")
	pos, err := parseInterleaved(fs, args)
	// a flag given with an empty value (--fit=, --changes '') is given all the same: 1a2df09 had none of these flags
	// and refused them, so no form of them reads a capture as if they were absent
	if err == nil && given(fs, "fit") {
		if len(pos) != 0 || *fitPath == "" || given(fs, "changes") && *changes == "" {
			return errors.New("usage: rhobound predict --fit F [--patch svc=+3ms | --changes C] [--report v1] [--load-scale S] [--max-rho 0.85] [--out DIR] [--json]")
		}
		if given(fs, "report") && *form == "" {
			return fmt.Errorf("--report %q: the only other form is v1", *form)
		}
		return predictFromFit(fs, c, *fitPath, *changes, *form, *loadScale, out, errw)
	}
	if err != nil || len(pos) != 1 {
		return fmt.Errorf("usage: rhobound predict CAPTURE [--patch svc=+3ms] [--workers svc=N] [--out DIR] [--json]")
	}
	if given(fs, "changes") || given(fs, "report") {
		return errors.New("--changes and --report go with --fit (rhobound predict --fit F); a capture takes --patch and prints report v1")
	}
	if given(fs, "load-scale") {
		return errors.New("--load-scale goes with --fit (rhobound predict --fit F --load-scale S); a capture is predicted at its observed load")
	}
	if !(*c.maxRho > 0 && *c.maxRho <= 0.95) { // also refuses NaN
		return fmt.Errorf("--max-rho must be in (0, 0.95]")
	}
	inf, err := load(pos[0], c)
	if err != nil {
		return err
	}
	patch, changeList, err := parsePatch(*c.patch, inf.Topology)
	if err != nil {
		return err
	}
	cfg := modelConfig(c)
	base, err := model.AnalyticWith(inf.Topology, inf.LamExtPerMs, nil, cfg)
	if err != nil {
		return err
	}
	if mr, _ := base.MaxRho(); !base.Stable || !(mr <= *c.maxRho) { // a NaN utilization is refused too
		return engine.LimitError(contract.ModelUtilizationObserved, "refusing to predict: at the observed load", inf, base, *c.maxRho)
	}
	var delta *model.Delta
	if patch.DS != nil {
		delta, err = model.ChangePatchWith(inf.Topology, inf.LamExtPerMs, patch, cfg)
		if err != nil {
			return err
		}
		if mr, _ := delta.Patched.MaxRho(); !delta.Stable || !(mr <= *c.maxRho) {
			return engine.LimitError(contract.ModelUtilizationAfter, "refusing the change: after it", inf, delta.Patched, *c.maxRho)
		}
	}
	rep := report.Build(pos[0], inf, *c.keptShare, base, delta, changeList)
	rep.Model = modelName(c, inf.Topology)
	return writeReport(rep, c, out, errw)
}

// predictFromFit is `rhobound predict --fit F`: the fit record, the change (--patch or --changes; none gives the
// fit's baseline), checked against the fit and the record store, then the engine. The options the inference was
// made with (--workers, --kept-share, --plain) are the fit's; giving them here is an error.
//
// loadScale is the engine's load point (engine.Request.LoadScale): 1, the default, is the fit's window and today's
// report byte for byte; any other positive number is a what-if (load.evaluated_at what_if), where the system before
// the change and after it are both solved at loadScale times the fit's rate, and is refused with --report v1, whose
// keys cannot say it.
func predictFromFit(fs *flag.FlagSet, c *common, fitPath, changesPath, form string, loadScale float64, out, errw io.Writer) error {
	var fitOnly []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "workers" || f.Name == "kept-share" || f.Name == "plain" {
			fitOnly = append(fitOnly, "--"+f.Name)
		}
	})
	if len(fitOnly) > 0 {
		return fmt.Errorf("predict --fit does not take %s: they are options of the fit (rhobound fit), which it uses; a "+
			"workers change is an item of --changes", strings.Join(fitOnly, " or "))
	}
	if *c.patch != "" && changesPath != "" {
		return errors.New("--patch and --changes: give one change")
	}
	if form != "" && form != "v1" {
		return fmt.Errorf("--report %q: the only other form is v1", form)
	}
	if !(*c.maxRho > 0 && *c.maxRho <= 0.95) { // also refuses NaN
		return fmt.Errorf("--max-rho must be in (0, 0.95]")
	}
	if !(loadScale > 0) || math.IsInf(loadScale, 0) { // also refuses NaN
		return fmt.Errorf("--load-scale must be a positive number, got %v", loadScale)
	}
	if loadScale != 1 && form == "v1" {
		return errors.New("--load-scale and --report v1: report v1 has no load point, so it cannot say that it is a what-if; use report v2")
	}
	rec, err := readFit(fitPath)
	if err != nil {
		return err
	}
	digest := rec.Digest()
	set := &change.Set{Schema: change.Schema, Fit: digest, Items: []change.Item{}}
	switch {
	case *c.patch != "":
		if set, err = change.FromPatch(*c.patch); err != nil {
			return err
		}
		set.Fit = digest
	case changesPath != "":
		b, err := os.ReadFile(changesPath)
		if err != nil {
			return err
		}
		if set, err = change.Parse(b); err != nil {
			return err
		}
		if set.Fit == "" {
			set.Fit = digest
		}
		if set.Fit != digest {
			return fmt.Errorf("%s was written against the fit %s, not %s (%s)", changesPath, set.Fit, digest, fitPath)
		}
	}
	recs := openRecords()
	if err := set.Refusal(rec, recs); err != nil {
		return err
	}
	var doubts []contract.Finding
	for _, f := range set.Validate(rec, recs) {
		if f.Level != contract.Refuse {
			doubts = append(doubts, f)
		}
	}
	hash, err := set.Digest()
	if err != nil {
		return err
	}
	resolved, err := set.Resolve(rec, recs)
	if err != nil {
		return err
	}
	req := engine.Request{Fit: rec, Changes: resolved, ChangeHash: hash, Findings: doubts, Plain: rec.Options.Plain,
		MaxRho: *c.maxRho, LoadScale: loadScale}
	v, err := engine.Predict(req)
	if err != nil {
		return err
	}
	if form == "v1" {
		if err := engine.AfterLimit(req); err != nil {
			return err
		}
		return writeReport(v.V1(), c, out, errw)
	}
	return writeReportV2(v, c, out, errw)
}

// readFit reads a fit record; a record of another schema version is refused with fit.version.
func readFit(path string) (*fit.Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rec, err := fit.Unmarshal(b)
	if errors.Is(err, fit.ErrVersion) {
		return nil, &report.RefusalError{Code: contract.FitVersion, Err: fmt.Errorf("%s: %w", path, err)}
	}
	return rec, err
}

// writeReport writes the report into the --out folder (report.txt and
// report.json) when one is given, then prints it, as JSON with --json.
func writeReport(rep *report.Report, c *common, out, errw io.Writer) error {
	return writeBoth(c, out, errw, rep.WriteText, rep.WriteJSON)
}

// writeReportV2 is writeReport for report v2: its JSON is canonical, its text is the v1 text of its v1 keys.
func writeReportV2(v *report.V2, c *common, out, errw io.Writer) error {
	return writeBoth(c, out, errw, v.WriteText, v.WriteJSON)
}

func writeBoth(c *common, out, errw io.Writer, text, js func(io.Writer) error) error {
	if *c.out != "" {
		if err := os.MkdirAll(*c.out, 0o755); err != nil {
			return err
		}
		for _, f := range []struct {
			name  string
			write func(io.Writer) error
		}{{"report.txt", text}, {"report.json", js}} {
			fh, err := os.Create(filepath.Join(*c.out, f.name))
			if err != nil {
				return err
			}
			err = f.write(fh)
			if cerr := fh.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
		fmt.Fprintf(errw, "wrote %s/report.txt and report.json\n", *c.out)
	}
	if *c.asJSON {
		return js(out)
	}
	return text(out)
}

// patchTargets are the services a patch can name: the topology's names without the zero-time nodes the inference adds
// for the call structure (stage:S:n and shape:S:n), which are not services.
func patchTargets(topo *model.Topology) []string {
	var out []string
	for _, n := range topo.Names() {
		if !strings.HasPrefix(n, "stage:") && !strings.HasPrefix(n, "shape:") {
			out = append(out, n)
		}
	}
	return out
}

// parsePatch turns "svc=+3ms,other=-10%" into a model patch. +N ms is extra
// work on every visit, added to the service time (it holds the worker); +N%
// scales the service's whole own time per visit, the service time and the
// glue (the time after its first call out, which holds no worker) alike.
// An unknown service and a negative time are refused with their codes
// (change.unknown_service, change.negative_time), the messages unchanged.
func parsePatch(s string, topo *model.Topology) (model.Patch, []report.Change, error) {
	if s == "" {
		return model.Patch{}, nil, nil
	}
	n := len(topo.Services)
	p := model.Patch{DS: make([]float64, n), Shift: make([]bool, n), Glue: make([]float64, n)}
	var changes []report.Change
	for _, kv := range splitList(s) {
		name, val, ok := strings.Cut(kv, "=")
		name, val = strings.TrimSpace(name), strings.TrimSpace(val)
		i := topo.Index(name)
		if !ok || i < 0 {
			return model.Patch{}, nil, &report.RefusalError{Code: contract.ChangeUnknownService, Subject: name,
				Err: fmt.Errorf("--patch: unknown service in %q (services: %s)", kv, strings.Join(patchTargets(topo), ", "))}
		}
		var delta, glue float64
		var err error
		switch {
		case strings.HasSuffix(val, "%"):
			var pct float64
			pct, err = strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64)
			delta = topo.Services[i].S * pct / 100
			glue = topo.Services[i].Glue * pct / 100
		default:
			delta, err = strconv.ParseFloat(strings.TrimSuffix(val, "ms"), 64)
			p.Shift[i] = true // a fixed amount of extra time per visit: the mean moves, the variance stays
		}
		if err != nil || math.IsNaN(delta) || math.IsInf(delta, 0) || math.IsNaN(glue) || math.IsInf(glue, 0) {
			return model.Patch{}, nil, fmt.Errorf("--patch: cannot read %q (use +3ms or -10%%)", kv)
		}
		p.DS[i] += delta
		p.Glue[i] += glue
		changes = append(changes, report.Change{Service: name, DeltaMs: delta + glue, GlueMs: glue, Spec: val})
		if topo.Services[i].S+p.DS[i] < 0 {
			return model.Patch{}, nil, &report.RefusalError{Code: contract.ChangeNegativeTime, Subject: name,
				Err: fmt.Errorf("--patch: %s would get a negative service time", name)}
		}
		if topo.Services[i].Glue+p.Glue[i] < 0 {
			return model.Patch{}, nil, &report.RefusalError{Code: contract.ChangeNegativeTime, Subject: name,
				Err: fmt.Errorf("--patch: %s would get a negative glue time", name)}
		}
	}
	return p, changes, nil
}
