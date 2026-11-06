// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

func init() {
	register("explain", command{run: cmdExplain, usage: `  rhobound explain [--target FILE] --section S [--max-chars N] [--policy P [--now T] [--fit F]] [--json]
                                            one section of a fit, an A/B record or a prediction (JSON)
`})
}

const explainUsage = "usage: rhobound explain [--target FILE] --section S [--max-chars N] [--policy P [--policy-name N] [--now T] [--fit F]] [--json]"

func cmdExplain(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("explain")
	targetPath := fs.String("target", "", "a fit record, an A/B record or a report v2")
	section := fs.String("section", "", strings.Join(views.Sections, ", "))
	maxChars := fs.Int("max-chars", views.DefaultExplainChars, "the most characters of the section's content")
	policyPath := fs.String("policy", "", "for a report: the policy its rules and judge packet are evaluated under")
	policyName := fs.String("policy-name", "", "the policy's name in the judge packet (default: its file name)")
	nowFlag := fs.String("now", "", "the time the fit's age is measured at (RFC 3339; default: the clock)")
	fitPath := fs.String("fit", "", "for a report's judge packet: the fit it was predicted from")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	known := false
	for _, s := range views.Sections {
		known = known || s == *section
	}
	if err != nil || len(pos) != 0 || !known || *maxChars < views.MinExplainChars || *maxChars > views.MaxExplainChars {
		return t.usage(explainUsage)
	}
	var tgt views.Target
	if *targetPath != "" {
		b, err := os.ReadFile(*targetPath)
		if err != nil {
			return t.usage(err.Error())
		}
		var head struct {
			Schema  string `json:"schema"`
			Version string `json:"version"`
		}
		if json.Unmarshal(b, &head) != nil {
			return t.usage("the target is not a JSON object")
		}
		switch {
		case head.Schema == fit.Schema:
			// read as every twin reads a fit (fit.Unmarshal): a record this version did not write, or not in the form
			// it writes, is refused alike
			rec, err := fit.Unmarshal(b)
			if errors.Is(err, fit.ErrVersion) {
				return t.fail(&report.RefusalError{Code: contract.FitVersion, Err: err})
			}
			if err != nil {
				return t.usage("the target is not a readable fit record: " + err.Error())
			}
			tgt.Fit, tgt.FitDigest = rec, contract.Digest(b)
		case head.Schema == abdiff.Schema:
			rec, err := abdiff.Unmarshal(b)
			if errors.Is(err, abdiff.ErrVersion) {
				return t.fail(&report.RefusalError{Code: contract.FitVersion, Err: err})
			}
			if err != nil {
				return t.usage("the target is not a readable A/B record: " + err.Error())
			}
			tgt.AB = rec
		case head.Version == report.VersionV2:
			r, _, _, err := twinPrediction(b)
			if err != nil || r == nil {
				return t.usage("the target is not a readable report v2")
			}
			tgt.Report = r
			if *policyPath != "" {
				now, err := twinNow(*nowFlag)
				if err != nil {
					return t.usage(err.Error())
				}
				p, _, err := twinPolicy(*policyPath)
				if err != nil {
					return t.fail(err)
				}
				name := *policyName
				if name == "" {
					name = policyFileName(*policyPath)
				}
				pk, res, err := twinPacket(r, p, name, *fitPath, now)
				if err != nil {
					return t.fail(err)
				}
				tgt.Gate, tgt.Packet = res, pk
			}
		default:
			return t.usage("the target is not a fit record, an A/B record or a report v2")
		}
	}
	v, err := views.Explain(tgt, *section, *maxChars)
	if errors.Is(err, views.ErrSection) {
		need := "a target"
		if tgt.Report != nil {
			need = "--policy"
		} else if tgt.Fit != nil || tgt.AB != nil {
			need = "a prediction (a report v2) as the target"
		}
		return t.usage(fmt.Sprintf("explain: section %s needs %s", *section, need))
	}
	if err != nil {
		return t.fail(err)
	}
	return t.view(v)
}
