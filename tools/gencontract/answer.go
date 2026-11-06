// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// The loop's clock and inputs, as the scripted agent uses them (testdata/mcp/README.md).
const (
	// LoopNow is the clock the loop runs at: one hour after the capture's window ends, inside max_fit_age_h. Any
	// instant within 24 hours of the window's end gives the same packet.
	LoopNow = "2023-11-14T23:15:20Z"
	// LoopWorkers are the worker counts the loop's fit pins.
	LoopWorkers = "a=1,b=2,c=1"
	// LoopName is the name the server's configuration gives the loop's capture, which its fit records (--name).
	LoopName = "loop"
	// LoopPolicyName is the name the server's configuration gives the loop's policy.
	LoopPolicyName = "default"
	// blockChanges is the agent's first draft: 3.5 to 6.0 ms more own time per visit of b, declared.
	blockChanges = `{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6},"label":"declared"}]}`
)

// answerTemplate is the canned judge answer (rhobound-mcp.judge-answer/v1) to the packet of the loop's first draft:
// revise, with one expected operation and no millisecond figure in a typed field. %s is the packet's digest.
const answerTemplate = `{
  "schema": "rhobound-mcp.judge-answer/v1",
  "packet": "%s",
  "decision": "revise",
  "reasons": [{"refs": ["prediction.entries[0]", "budgets[0]"], "code": "over_budget"}],
  "suggestions": [{"rank": 1, "expected_ops": [{"op": "scale_count", "service": "b", "callee": "c", "value": 0.5,
    "note": "batch the lookups to c: one call per two visits"}]}],
  "measure": [{"service": "b", "kind": "ab", "why_code": "own_time_unknown"}],
  "commentary": "The added work in b is above its per-visit budget. Batch the lookups to c, or move the work off the request path, then measure b with an A/B run."
}
`

// CannedAnswer computes the loop's judge answer with a rhobound binary: it fits the loop capture in dir as the server
// does (named LoopName, the workers pinned), predicts the first draft, builds the judge packet under dir/policy.json
// named LoopPolicyName at LoopNow, and returns the answer citing that packet's digest, after `rhobound judge-check`
// accepted it.
func CannedAnswer(engine, dir string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "gencontract-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(engine, args...)
		cmd.Env = append(os.Environ(), "RHOBOUND_ESTIMATOR=", "RHOBOUND_STATE="+filepath.Join(tmp, "state"))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("rhobound %s: %v: %s", strings.Join(args, " "), err, stderr.Bytes())
		}
		return out, nil
	}
	fitDir, fitFile := filepath.Join(tmp, "fit"), filepath.Join(tmp, "fit", "fit.json")
	if _, err := run("fit", dir, "--name="+LoopName, "--workers", LoopWorkers, "--out", fitDir, "--json"); err != nil {
		return nil, err
	}
	changes := filepath.Join(tmp, "changes.json")
	if err := os.WriteFile(changes, []byte(blockChanges), 0o644); err != nil {
		return nil, err
	}
	pred, err := run("predict", "--fit", fitFile, "--changes", changes, "--json")
	if err != nil {
		return nil, err
	}
	predFile := filepath.Join(tmp, "prediction.json")
	if err := os.WriteFile(predFile, pred, 0o644); err != nil {
		return nil, err
	}
	packet, err := run("judge-packet", predFile, "--policy", filepath.Join(dir, "policy.json"), "--policy-name", LoopPolicyName,
		"--fit", fitFile, "--now", LoopNow, "--json")
	if err != nil {
		return nil, err
	}
	canon, err := contract.CanonicalizeJSON(packet)
	if err != nil {
		return nil, err
	}
	answer := []byte(fmt.Sprintf(answerTemplate, contract.Digest(canon)))
	packetFile, answerFile := filepath.Join(tmp, "packet.json"), filepath.Join(tmp, "judge-answer.json")
	if err := os.WriteFile(packetFile, packet, 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(answerFile, answer, 0o644); err != nil {
		return nil, err
	}
	if _, err := run("judge-check", answerFile, "--packet", packetFile, "--json"); err != nil {
		return nil, err
	}
	return answer, nil
}
