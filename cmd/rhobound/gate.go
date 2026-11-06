// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/judge"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

func init() {
	register("gate", command{run: cmdGate, usage: `  rhobound gate    PREDICTION.json --policy P [--readiness R] [--now T] --out DIR [--json]
                   [--judge-answer A --judge-packet K [--judge-model ID --judge-request D]]
                                            the deploy gate: exit 0 pass, 1 block, 3 inconclusive, 2 error
`})
}

const gateUsage = "usage: rhobound gate PREDICTION.json --policy P [--readiness R] [--now T] --out DIR [--json] " +
	"[--judge-answer A --judge-packet K [--judge-model ID --judge-request D]]"

// twinPrediction reads a prediction file: a report v2, or the refusal the engine printed in its place
// ({"error": Refusal}, refusal.v1), and the digest of its canonical JSON.
func twinPrediction(b []byte) (*report.V2, *report.Refusal, string, error) {
	canon, err := contract.CanonicalizeJSON(b)
	if err != nil {
		return nil, nil, "", fmt.Errorf("the prediction is not JSON: %v", err)
	}
	var head struct {
		Version string          `json:"version"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(canon, &head); err != nil {
		return nil, nil, "", fmt.Errorf("the prediction is not a JSON object")
	}
	digest := contract.Digest(canon)
	switch {
	case head.Error != nil:
		var doc report.RefusalDoc
		dec := json.NewDecoder(bytes.NewReader(canon))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil || !contract.Pattern.MatchString(doc.Error.Code) {
			return nil, nil, "", fmt.Errorf("the prediction is a refusal, but not a coded one")
		}
		return nil, &doc.Error, digest, nil
	case head.Version == report.VersionV2:
		var r report.V2
		if err := json.Unmarshal(canon, &r); err != nil {
			return nil, nil, "", fmt.Errorf("the prediction is not a readable report v2: %v", err)
		}
		return &r, nil, digest, nil
	case head.Version == report.Version:
		return nil, nil, "", fmt.Errorf("the prediction is report v1: the gate reads report v2 (predict --fit F --json)")
	}
	return nil, nil, "", fmt.Errorf("the prediction is neither a report v2 nor a refusal")
}

var digestFlag = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func cmdGate(args []string, out, errw io.Writer) int {
	fs, asJSON := twinFlags("gate")
	policyPath := fs.String("policy", "", "the repository's policy")
	readinessPath := fs.String("readiness", "", "the environment's readiness record")
	nowFlag := fs.String("now", "", "the time the fit's age is measured at (RFC 3339; default: the clock)")
	outDir := fs.String("out", "", "the folder to write verdict.txt, gate.json and summary.md to")
	answerPath := fs.String("judge-answer", "", "a judge answer to the packet --judge-packet names")
	packetPath := fs.String("judge-packet", "", "the judge packet the answer answers")
	modelID := fs.String("judge-model", "", "the pinned model a CI step obtained the answer from")
	request := fs.String("judge-request", "", "the digest of the request the CI step sent to the model")
	pos, err := parseInterleaved(fs, args)
	tool := func(msg string) int {
		fmt.Fprintln(errw, "rhobound:", msg)
		return gate.ExitToolError
	}
	if err != nil || len(pos) != 1 || *policyPath == "" || *outDir == "" || (*answerPath == "") != (*packetPath == "") ||
		(*modelID != "") != (*request != "") || (*modelID != "" && *answerPath == "") {
		return tool(gateUsage)
	}
	if *request != "" && !digestFlag.MatchString(*request) {
		return tool("--judge-request must be a sha256: digest")
	}
	now, err := twinNow(*nowFlag)
	if err != nil {
		return tool(err.Error())
	}
	b, err := os.ReadFile(pos[0])
	if err != nil {
		return tool(err.Error())
	}
	r, refusal, reportDigest, err := twinPrediction(b)
	if err != nil {
		return tool(err.Error())
	}
	pb, err := os.ReadFile(*policyPath)
	if err != nil {
		return tool(err.Error())
	}
	p, _, err := gate.ParsePolicy(pb, false)
	if err != nil {
		return tool(err.Error())
	}
	in := gate.Input{Report: r, Refusal: refusal, Policy: p, Now: now}
	digests := gate.Digests{Report: reportDigest}
	if r != nil {
		digests.Fit = r.FitDigest
	}
	if *readinessPath != "" {
		rb, err := os.ReadFile(*readinessPath)
		if err != nil {
			return tool(err.Error())
		}
		if in.Readiness, err = gate.ParseReadiness(rb); err != nil {
			return tool(err.Error())
		}
		c, _ := contract.CanonicalizeJSON(rb)
		digests.Readiness = contract.Digest(c)
	}
	if *answerPath != "" {
		in.Judge = twinJudge(*answerPath, *packetPath, *modelID, *request, errw)
	}
	res := gate.Evaluate(in)
	rec, err := gate.NewRecord(res, p, in.Readiness, digests)
	if err != nil {
		return tool(err.Error())
	}
	js, err := rec.Canonical()
	if err != nil {
		return tool(err.Error())
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return tool(err.Error())
	}
	for name, content := range map[string][]byte{
		gate.VerdictFile: []byte(res.Verdict + "\n"),
		gate.RecordFile:  append(js, '\n'),
		gate.SummaryFile: []byte(gate.Summary(rec, res)),
	} {
		if err := os.WriteFile(filepath.Join(*outDir, name), content, 0o644); err != nil {
			return tool(err.Error())
		}
	}
	if *asJSON {
		if _, err := out.Write(append(js, '\n')); err != nil {
			return tool(err.Error())
		}
	} else {
		fmt.Fprintf(out, "%s (advisory)\n", res.Verdict)
		for _, rr := range res.Fired() {
			if rr.Effect != gate.EffectNote {
				fmt.Fprintf(out, "  rule %s: %s\n", rr.Number, rr.Why())
			}
		}
		for _, d := range res.DecideWith {
			fmt.Fprintf(out, "  decide with: %s\n", d)
		}
	}
	return res.ExitCode
}

// twinJudge checks a judge answer for the gate. An answer that fails the check is "judge unavailable": the gate goes
// on without it and never retries it (ARCHITECTURE.md 7.2), so a steered judge cannot stop a release by answering
// badly.
func twinJudge(answerPath, packetPath, modelID, request string, errw io.Writer) *gate.Judge {
	ab, err := os.ReadFile(answerPath)
	if err != nil {
		fmt.Fprintln(errw, "rhobound: judge unavailable:", err)
		return nil
	}
	pb, err := os.ReadFile(packetPath)
	if err != nil {
		fmt.Fprintln(errw, "rhobound: judge unavailable:", err)
		return nil
	}
	typed, _, err := judge.CheckAnswer(pb, ab)
	if err != nil {
		fmt.Fprintln(errw, "rhobound: judge unavailable:", err)
		return nil
	}
	j := &gate.Judge{Provenance: gate.ProvenanceClientAsserted, Decision: typed.Decision, Answer: typed.Answer,
		Reasons: typed.Reasons, Commentary: judge.Commentary(ab)}
	if modelID != "" {
		j.Provenance, j.Request = gate.ProvenanceCIJudge, request
		j.ModelID = judgeModel(modelID)
	}
	return j
}

// judgeModel is a judge's model as gate.json records it. A model or inference profile given by its AWS resource name
// (arn:aws:bedrock:<region>:<account>:inference-profile/us.anthropic.claude-x-v1:0) is recorded as the resource after
// its type (us.anthropic.claude-x-v1:0): the model, and not the account or the region. The name rule's last element
// would be the version alone (0), which two models share. Any other id passes the name rule.
func judgeModel(id string) string {
	if p := strings.SplitN(id, ":", 6); len(p) == 6 && strings.EqualFold(p[0], "arn") && p[1] != "" && p[2] != "" {
		if _, res, ok := strings.Cut(p[5], "/"); ok && res != "" {
			id = res
		}
	}
	s, _ := contract.SafeName(id)
	return s
}
