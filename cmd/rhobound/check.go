// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// The exit statuses of check, its own (DATA.md 3.2): a data refusal is never 1, which the gate uses for block.
const (
	checkAdmitted  = 0
	checkToolError = 2
	checkRefused   = 4
)

func init() {
	register("check", command{run: cmdCheck, usage: `  rhobound check   CAPTURE|--manifest M [--request-count F] [--kept-share S] [--sampler svc=NAME]
                   [--probe-route R] [--pod-identity host] [--write-manifest] [--fleet] [--json]
                   [--window START,END --cut entry-start --out DIR]
                                            the data contract: exit 0 admitted, 4 refused, 2 error
                                            (--fleet: the findings per service and the modelable share of journeys)
`})
}

// listFlag is a flag that may be given several times.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

const checkUsage = "usage: rhobound check CAPTURE|--manifest M [--request-count F] [--kept-share S] [--sampler svc=NAME] " +
	"[--probe-route R] [--pod-identity host] [--write-manifest] [--fleet] [--window START,END --cut entry-start --out DIR] [--json]"

func cmdCheck(args []string, out, errw io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(errw, "rhobound:", err)
		return checkToolError
	}
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	manifest := fs.String("manifest", "", "the capture manifest to check (its objects are relative to its folder)")
	writeManifest := fs.Bool("write-manifest", false, "write CAPTURE/manifest.json")
	count := fs.String("request-count", "", "an independent request count (default: request-count.json of the capture, if any)")
	keptShare := fs.String("kept-share", "", "the declared share of requests the capture holds")
	podIdentity := fs.String("pod-identity", "", `"host": key the pods of resources with no pod or task identity by host.name`)
	window := fs.String("window", "", "START,END (RFC 3339): the window to cut")
	cut := fs.String("cut", "", "entry-start: cut whole traces by their entry span's start")
	outDir := fs.String("out", "", "the folder to write the canonical capture to")
	asJSON := fs.Bool("json", false, "print the check report as JSON")
	fleetFlag := fs.Bool("fleet", false, "print the fleet view instead of the report: the findings per service, the modelable share of journeys")
	var routes, samplers listFlag
	fs.Var(&routes, "probe-route", "a route that is a probe (repeatable)")
	fs.Var(&samplers, "sampler", "svc=NAME: the SDK sampler of a service (repeatable)")
	pos, err := parseInterleaved(fs, args)
	if err != nil || len(pos) > 1 || (len(pos) == 1) == (*manifest != "") {
		return fail(errors.New(checkUsage))
	}
	if (*window != "" || *cut != "" || *outDir != "") && (*window == "" || *cut != "entry-start" || *outDir == "") {
		return fail(errors.New("--window, --cut entry-start and --out go together"))
	}
	if *writeManifest && *manifest != "" {
		return fail(errors.New("--write-manifest writes the manifest of a capture folder; --manifest already names one"))
	}
	opt := check.Options{ExtraProbeRoutes: routes, PodIdentity: *podIdentity}
	if *keptShare != "" {
		k, err := strconv.ParseFloat(*keptShare, 64)
		if err != nil || !(k > 0 && k <= 1) {
			return fail(fmt.Errorf("--kept-share must be in (0, 1], got %q", *keptShare))
		}
		opt.DeclaredKeptShare = &k
	}
	for _, s := range samplers {
		svc, name, ok := strings.Cut(s, "=")
		if !ok || svc == "" || name == "" {
			return fail(fmt.Errorf("--sampler: %q is not svc=NAME", s))
		}
		if opt.DeclaredSamplers == nil {
			opt.DeclaredSamplers = map[string]string{}
		}
		opt.DeclaredSamplers[strings.TrimSpace(svc)] = strings.TrimSpace(name)
	}
	var start, end time.Time
	if *window != "" {
		s, e, ok := strings.Cut(*window, ",")
		var err1, err2 error
		start, err1 = time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
		end, err2 = time.Parse(time.RFC3339Nano, strings.TrimSpace(e))
		if !ok || err1 != nil || err2 != nil || !end.After(start) {
			return fail(fmt.Errorf("--window %q is not START,END in RFC 3339 with END after START", *window))
		}
		if _, _, err := check.WindowNanos(start, end); err != nil {
			return fail(err)
		}
	}

	var m *contract.Manifest
	var ignored map[string]int64
	if *manifest != "" {
		if m, err = check.ReadManifest(*manifest); err != nil {
			return fail(err)
		}
	} else if m, ignored, err = check.ManifestFromDir(pos[0], opt.Limits); err != nil {
		return fail(err)
	}
	countPath := *count
	if countPath == "" {
		if p := filepath.Join(m.Dir, "request-count.json"); fileExists(p) {
			countPath = p
		}
	}
	// A count given with --cut counts the window cut: the capture is checked without it, cut, and the cut checked with
	// it (DATA.md 4.3: cut, manifest, checks). Any other count, the folder's own included, counts the capture as read.
	countAfterCut := *count != "" && *outDir != ""
	var countBytes []byte
	var rc *check.RequestCount
	if countPath != "" {
		if countBytes, err = os.ReadFile(countPath); err != nil {
			return fail(err)
		}
		if rc, err = check.ParseRequestCount(countBytes); err != nil {
			return fail(fmt.Errorf("%s: %w", countPath, err))
		}
		if !countAfterCut {
			opt.RequestCount = rc
		}
	}
	// The capture as read may reach back past the window (a trace file holds what was written before it): its image
	// digests are counted in the window to cut only (trace.mixed_images, DATA.md 3.9). A check of the cut reads it whole,
	// as `fit --manifest` will, and as it reads the capture's manifest that --write-manifest writes: with --write-manifest
	// the capture is checked whole first, the manifest written only when that check admits it, and the window checked
	// alone only when two images refused it (any other refusal is the same in the window).
	inWindow := opt
	inWindow.WindowStart, inWindow.WindowEnd = start, end
	first := inWindow
	if *writeManifest {
		first = opt
	}
	rep, d, err := check.Run(m, first)
	if err != nil {
		return fail(err)
	}
	code := checkAdmitted
	if rep.Verdict == contract.Refused {
		code = checkRefused
	}
	if p := filepath.Join(m.Dir, "manifest.json"); code == checkAdmitted && *writeManifest {
		if err := os.WriteFile(p, append(m.Canonical(), '\n'), 0o644); err != nil {
			return fail(err)
		}
		fmt.Fprintf(errw, "wrote %s\n", p)
	} else if *writeManifest && *window != "" && rep.Has(contract.TraceMixedImages) {
		fmt.Fprintf(errw, "did not write %s: the capture as read is refused (%s); checking the window alone\n", p,
			contract.TraceMixedImages)
		if rep, d, err = check.Run(m, inWindow); err != nil {
			return fail(err)
		}
		if rep.Verdict != contract.Refused {
			code = checkAdmitted
		}
	}
	for k, v := range ignored {
		rep.Ignored[k] += v
	}
	if code == checkAdmitted && *outDir != "" {
		_, statErr := os.Stat(*outDir)
		final, err := check.Cut(m, d, start, end, *outDir)
		if err != nil {
			return fail(err)
		}
		rep.Manifest = final.Digest()
		if countAfterCut {
			opt.RequestCount = rc
			counted, cm, err := checkCounted(*outDir, countBytes, opt)
			if err != nil {
				return fail(err)
			}
			// ignored is what the capture as read carried and nothing reads: the cut keeps none of it
			counted.Ignored = rep.Ignored
			rep, final = counted, cm
			if rep.Verdict == contract.Refused {
				code = checkRefused
				if err := removeCut(*outDir, statErr == nil); err != nil {
					return fail(err)
				}
			}
		}
		if code == checkAdmitted {
			fmt.Fprintf(errw, "wrote the canonical capture to %s (%d objects, manifest %s)\n", *outDir, len(final.Objects), rep.Manifest)
		}
	}
	if *fleetFlag {
		v := fleet(rep, d)
		if *asJSON {
			b, err := contract.Canonical(v)
			if err != nil {
				return fail(err)
			}
			fmt.Fprintf(out, "%s\n", b)
		} else {
			writeFleetText(out, v)
		}
		return code
	}
	if *asJSON {
		b, err := contract.Canonical(rep)
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(out, "%s\n", b)
	} else {
		writeCheckText(out, rep)
	}
	return code
}

// checkCounted checks a cut with the request count of its window (opt.RequestCount, read from the bytes count): the
// bytes are copied into the cut's folder as request-count.json, the cut is checked with the count, and, unless that
// check refuses, its manifest is written again as the check completed it (the sampling evidence and the count's
// digest), so that `fit --manifest DIR/manifest.json` reads the same capture under the same digest, the report's
// manifest_sha256.
func checkCounted(dir string, count []byte, opt check.Options) (*contract.CheckReport, *contract.Manifest, error) {
	if err := os.WriteFile(filepath.Join(dir, "request-count.json"), count, 0o644); err != nil {
		return nil, nil, err
	}
	p := filepath.Join(dir, "manifest.json")
	m, err := check.ReadManifest(p)
	if err != nil {
		return nil, nil, err
	}
	rep, _, err := check.Run(m, opt)
	if err != nil {
		return nil, nil, err
	}
	if rep.Verdict != contract.Refused {
		if err := os.WriteFile(p, append(m.Canonical(), '\n'), 0o644); err != nil {
			return nil, nil, err
		}
	}
	return rep, m, nil
}

// removeCut removes what a cut wrote into dir, which was empty or did not exist (existed says which): a refused check
// leaves no canonical capture behind.
func removeCut(dir string, existed bool) error {
	if !existed {
		return os.RemoveAll(dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// fileExists reports whether p exists (or cannot be told not to: reading it then says why).
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

// writeCheckText prints a check report for people: the verdict, the kept share and the manifest, then one line per
// finding with its subject, numbers and fix.
func writeCheckText(w io.Writer, r *contract.CheckReport) {
	fmt.Fprintf(w, "check: %s (%s)\n", r.Verdict, r.Contract)
	fmt.Fprintf(w, "kept share %g (%s); capture manifest %s\n", r.KeptShare.Value, r.KeptShare.Source, r.Manifest)
	for _, f := range r.Findings {
		line := fmt.Sprintf("%-6s %s", f.Level, f.Code)
		if f.Subject != "" {
			line += " " + f.Subject
		}
		if len(f.Numbers) > 0 {
			keys := make([]string, 0, len(f.Numbers))
			for k := range f.Numbers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var nums []string
			for _, k := range keys {
				nums = append(nums, fmt.Sprintf("%s=%g", k, f.Numbers[k]))
			}
			line += " (" + strings.Join(nums, ", ") + ")"
		}
		if len(f.Examples) > 0 {
			line += " e.g. " + strings.Join(f.Examples, ", ")
		}
		fmt.Fprintf(w, "%s\n       fix: %s\n", line, f.Fix)
	}
	if len(r.Ignored) > 0 {
		keys := make([]string, 0, len(r.Ignored))
		for k := range r.Ignored {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, r.Ignored[k]))
		}
		fmt.Fprintf(w, "ignored (read, not used): %s\n", strings.Join(parts, "; "))
	}
}
