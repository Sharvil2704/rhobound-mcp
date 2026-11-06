// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/judge"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

func init() {
	register("judge-packet", command{run: cmdJudgePacket, usage: `  rhobound judge-packet PREDICTION.json --policy P [--policy-name N] [--fit F] [--now T] [--json]
                                            the bounded, sanitized packet for a judge model (JSON)
`})
	register("judge-check", command{run: cmdJudgeCheck, usage: `  rhobound judge-check ANSWER.json --packet PACKET.json [--json]
                                            check a judge's answer; print only its typed fields (JSON)
`})
}

const (
	judgePacketUsage = "usage: rhobound judge-packet PREDICTION.json --policy P [--policy-name N] [--fit F] [--now T] [--json]"
	judgeCheckUsage  = "usage: rhobound judge-check ANSWER.json --packet PACKET.json [--json]"
)

// twinPacket builds the judge packet of a report v2 under a policy: the preview's evaluation of the report at now,
// and, with the fit it was predicted from, the budgets and the leverage of the services it changes. Those are at the
// report's load point: for a what-if (`predict --fit F --load-scale S`) at S times the fit's rate, as its before_ms,
// change_ms and thresholds are, so that the packet does not quote budgets of the fit's load beside a change at S.
func twinPacket(r *report.V2, p *gate.Policy, policyName string, fitPath string, now time.Time) ([]byte, *gate.Result, error) {
	var safe map[string]float64
	var items []budget.Item
	var lev map[string]map[string]float64
	if fitPath != "" {
		rec, digest, err := twinFit(fitPath)
		if err != nil {
			return nil, nil, err
		}
		if digest != r.FitDigest {
			return nil, nil, fmt.Errorf("the prediction was made from fit %s, not %s", r.FitDigest, digest)
		}
		in := &previewInputs{fit: rec, digest: digest, report: r}
		if r.Load.EvaluatedAt == report.EvaluatedAtWhatIf {
			in.loadScale = r.Load.LoadScale
		}
		seen := map[string]bool{}
		for _, it := range r.Items {
			if it.Op != change.OpLoadScale && !seen[it.Service] {
				seen[it.Service] = true
				in.services = append(in.services, it.Service)
			}
		}
		if safe, items, err = in.safeBudgets(p); err != nil {
			return nil, nil, err
		}
		b := &budget.Budgeter{Fit: rec, FitDigest: digest, Policy: p, LoadScale: in.loadScale}
		if lev, err = b.Leverage(in.services); err != nil {
			return nil, nil, err
		}
	}
	res := gate.Evaluate(gate.Input{Report: r, Policy: p, Now: now, Preview: true, SafeBudgets: safe})
	pd, err := p.Digest()
	if err != nil {
		return nil, nil, err
	}
	pk, err := views.JudgePacket(views.PacketInput{Report: r, Gate: res, Items: items, Leverage: lev,
		PolicyName: policyName, PolicyDigest: pd})
	return pk, res, err
}

// policyFileName is a policy's name taken from its file: the base name without its extension.
func policyFileName(path string) string {
	n := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	s, _ := contract.SafeName(n)
	return s
}

func cmdJudgePacket(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("judge-packet")
	policyPath := fs.String("policy", "", "the policy the preview is evaluated under")
	policyName := fs.String("policy-name", "", "the policy's name (default: its file name)")
	fitPath := fs.String("fit", "", "the fit the prediction was made from: adds budgets and leverage")
	nowFlag := fs.String("now", "", "the time the fit's age is measured at (RFC 3339; default: the clock)")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	if err != nil || len(pos) != 1 || *policyPath == "" {
		return t.usage(judgePacketUsage)
	}
	now, err := twinNow(*nowFlag)
	if err != nil {
		return t.usage(err.Error())
	}
	b, err := os.ReadFile(pos[0])
	if err != nil {
		return t.fail(err)
	}
	r, refusal, _, err := twinPrediction(b)
	if err != nil {
		return t.fail(err)
	}
	if refusal != nil {
		return t.fail(fmt.Errorf("the prediction is a refusal (%s): there is nothing for a judge to review", refusal.Code))
	}
	p, _, err := twinPolicy(*policyPath)
	if err != nil {
		return t.fail(err)
	}
	name := *policyName
	if name == "" {
		name = policyFileName(*policyPath)
	}
	pk, _, err := twinPacket(r, p, name, *fitPath, now)
	if err != nil {
		return t.fail(err)
	}
	return t.view(pk)
}

func cmdJudgeCheck(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("judge-check")
	packetPath := fs.String("packet", "", "the judge packet the answer answers")
	pos, err := parseInterleaved(fs, args)
	t := twinIO{out: out, errw: errw, asJSON: *asJSON}
	if err != nil || len(pos) != 1 || *packetPath == "" {
		return t.usage(judgeCheckUsage)
	}
	ab, err := os.ReadFile(pos[0])
	if err != nil {
		return t.usage(err.Error())
	}
	pb, err := os.ReadFile(*packetPath)
	if err != nil {
		return t.usage(err.Error())
	}
	typed, _, err := judge.CheckAnswer(pb, ab)
	var se *judge.ShapeError
	if errors.As(err, &se) {
		return t.usage(se.Error())
	}
	if err != nil {
		return t.fail(err)
	}
	v, err := contract.Canonical(typed)
	if err != nil {
		return t.fail(err)
	}
	return t.view(v)
}
