// SPDX-License-Identifier: Apache-2.0

package judge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
	"github.com/Sharvil2704/rhobound-mcp/internal/views"
)

var scripts = []string{"bedrock-converse.sh", "openai-compatible.sh"}

// The scripts parse, carry the license header, name no account, key or private path, read their secrets from the
// environment only, and send the packet's own role rules.
func TestScriptsAreSafe(t *testing.T) {
	accountID := regexp.MustCompile(`\b[0-9]{12}\b`)
	apiKey := regexp.MustCompile(`\bsk-[A-Za-z0-9]{8,}`)
	homePath := regexp.MustCompile(`/(home|Users)/[A-Za-z]`)
	for _, name := range scripts {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if !strings.HasPrefix(s, "#!/usr/bin/env bash\n# SPDX-License-Identifier: Apache-2.0\n") {
			t.Errorf("%s: no shebang and license header", name)
		}
		for _, bad := range []string{"AKIA", "arn:aws:"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s contains %q", name, bad)
			}
		}
		if m := accountID.FindString(s); m != "" {
			t.Errorf("%s contains what looks like an account id: %s", name, m)
		}
		if apiKey.MatchString(s) {
			t.Errorf("%s contains what looks like an API key", name)
		}
		if homePath.MatchString(s) {
			t.Errorf("%s names a path in a home folder", name)
		}
		if bash, err := exec.LookPath("bash"); err == nil {
			if out, err := exec.Command(bash, "-n", name).CombinedOutput(); err != nil {
				t.Errorf("bash -n %s: %v %s", name, err, out)
			}
		}
	}
	if b, _ := os.ReadFile("openai-compatible.sh"); !strings.Contains(string(b), `"Authorization: Bearer $JUDGE_API_KEY"`) {
		t.Error("openai-compatible.sh does not read its key from JUDGE_API_KEY")
	}
	prompt, err := os.ReadFile("review_latency_impact.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range views.RoleRules {
		if !strings.Contains(string(prompt), "- "+rule+"\n") {
			t.Errorf("the prompt does not state the role rule %q", rule)
		}
	}
}

// tools are what the scripts need to run.
func needTools(t *testing.T, names ...string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s is not installed", n)
		}
	}
}

// loopPrediction builds the CLI and makes the loop fixture's first draft: its fit, its report v2 and its policy.
func loopPrediction(t *testing.T) (bin, fit, prediction, policy string) {
	t.Helper()
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	dir := t.TempDir()
	bin = filepath.Join(dir, "rhobound")
	build := exec.Command(goTool, "build", "-o", bin, "./cmd/rhobound")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	loop := filepath.Join(root, "testdata", "mcp", "loop")
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "RHOBOUND_ESTIMATOR=", "RHOBOUND_STATE="+filepath.Join(dir, "state"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("rhobound %v: %v", args, err)
		}
		return out
	}
	run("fit", loop, "--workers", "a=1,b=2,c=1", "--out", filepath.Join(dir, "fit"), "--json")
	fit = filepath.Join(dir, "fit", "fit.json")
	changes := filepath.Join(dir, "changes.json")
	os.WriteFile(changes, []byte(`{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6},"label":"declared"}]}`), 0o644)
	prediction = filepath.Join(dir, "prediction.json")
	if err := os.WriteFile(prediction, run("predict", "--fit", fit, "--changes", changes, "--json"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, fit, prediction, filepath.Join(loop, "policy.json")
}

var digestRE = regexp.MustCompile(`packet digest: (sha256:[0-9a-f]{64})`)

// answerFor is the loop's canned judge answer, citing the given packet digest.
func answerFor(t *testing.T, digest string) string {
	t.Helper()
	root, _ := testgen.RepoRoot()
	b, err := os.ReadFile(filepath.Join(root, "testdata", "mcp", "loop", "judge-answer.json"))
	if err != nil {
		t.Fatal(err)
	}
	var a map[string]any
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	a["packet"] = digest
	out, _ := json.Marshal(a)
	return string(out)
}

// checkOutputs reads what a script left in its folder: the typed answer, and gate.json recording the judge as ci-judge.
func checkOutputs(t *testing.T, out string, code int, stderr []byte) {
	t.Helper()
	if code != 1 {
		t.Fatalf("exit %d, want 1 (the first draft blocks): %s", code, stderr)
	}
	var typed struct {
		Decision    string `json:"decision"`
		ExpectedOps []any  `json:"expected_ops"`
	}
	b, err := os.ReadFile(filepath.Join(out, "judge-check.json"))
	if err != nil || json.Unmarshal(b, &typed) != nil || typed.Decision != "revise" || len(typed.ExpectedOps) != 1 {
		t.Errorf("judge-check.json: %v %s", err, b)
	}
	g, err := os.ReadFile(filepath.Join(out, "gate", "gate.json"))
	if err != nil || !strings.Contains(string(g), `"ci-judge"`) || !strings.Contains(string(g), "example-model") {
		t.Errorf("gate.json does not record the judge: %v %s", err, g)
	}
}

func runScript(t *testing.T, script string, env []string, args ...string) (int, []byte) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, out
}

// openai-compatible.sh against a stand-in endpoint that answers with the canned answer, in a Markdown fence.
func TestOpenAICompatibleScript(t *testing.T) {
	needTools(t, "bash", "curl", "jq", "sha256sum")
	bin, fit, prediction, policy := loopPrediction(t)
	answer := answerFor(t, "PACKET")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role, Content string
			} `json:"messages"`
		}
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" ||
			json.NewDecoder(r.Body).Decode(&req) != nil || req.Model != "example-model" || len(req.Messages) != 2 {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		m := digestRE.FindStringSubmatch(req.Messages[1].Content)
		if m == nil || !strings.Contains(req.Messages[0].Content, "never supply milliseconds") {
			http.Error(w, "no packet digest", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
			"role": "assistant", "content": "```json\n" + strings.Replace(answer, "PACKET", m[1], 1) + "\n```"}}}})
	}))
	defer srv.Close()
	out := filepath.Join(t.TempDir(), "judge")
	code, stderr := runScript(t, "openai-compatible.sh", []string{"RHOBOUND=" + bin, "JUDGE_ENDPOINT=" + srv.URL + "/v1",
		"JUDGE_API_KEY=test-key", "JUDGE_MODEL_ID=example-model", "FIT=" + fit, "POLICY_NAME=default"}, prediction, policy, out)
	checkOutputs(t, out, code, stderr)
	if b, _ := os.ReadFile(filepath.Join(out, "request.json")); strings.Contains(string(b), "test-key") {
		t.Error("the key was written to the output folder")
	}
}

// bedrock-converse.sh against a stand-in for the AWS CLI that answers converse with the canned answer.
func TestBedrockConverseScript(t *testing.T) {
	needTools(t, "bash", "jq", "sha256sum", "grep", "sed")
	bin, fit, prediction, policy := loopPrediction(t)
	stub := t.TempDir()
	tmpl := filepath.Join(stub, "answer.json")
	if err := os.WriteFile(tmpl, []byte(answerFor(t, "PACKET")), 0o644); err != nil {
		t.Fatal(err)
	}
	aws := `#!/usr/bin/env bash
# a stand-in for the AWS CLI: answers bedrock-runtime converse with the canned answer for the packet in --messages
set -euo pipefail
[ "$1 $2" = "bedrock-runtime converse" ] || exit 64
model= msgs=
while [ $# -gt 0 ]; do
  case $1 in --model-id) model=$2; shift ;; --messages) msgs=$2; shift ;; esac
  shift
done
[ "$model" = example-model ] || exit 65
digest=$(printf '%s' "$msgs" | grep -o 'packet digest: sha256:[0-9a-f]\{64\}' | sed 's/packet digest: //')
jq -n --arg t "$(sed "s/PACKET/$digest/" "` + tmpl + `")" '{output: {message: {role: "assistant", content: [{text: $t}]}}}'
`
	if err := os.WriteFile(filepath.Join(stub, "aws"), []byte(aws), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "judge")
	code, stderr := runScript(t, "bedrock-converse.sh", []string{"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RHOBOUND=" + bin, "JUDGE_MODEL_ID=example-model", "FIT=" + fit, "POLICY_NAME=default"}, prediction, policy, out)
	checkOutputs(t, out, code, stderr)
}

// A judge that answers badly is "judge unavailable": a refused answer (a loosening reason) or one that is not an
// answer at all is recorded in judge-unavailable.json, the gate still runs without it, and the script's exit status
// is the gate's (1: the first draft blocks), with no judge in gate.json.
func TestJudgeUnavailable(t *testing.T) {
	needTools(t, "bash", "curl", "jq", "sha256sum")
	bin, fit, prediction, policy := loopPrediction(t)
	for name, c := range map[string]struct {
		content string
		status  float64
	}{
		"refused":    {`{"schema":"rhobound-mcp.judge-answer/v1","packet":"PACKET","decision":"accept","reasons":[{"refs":["prediction.entries[0]"],"code":"waive_threshold"}]}`, 1},
		"not answer": {"I think this change is fine.", 2},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Messages []struct{ Role, Content string } `json:"messages"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				content := c.content
				if m := digestRE.FindStringSubmatch(req.Messages[len(req.Messages)-1].Content); m != nil {
					content = strings.Replace(content, "PACKET", m[1], 1)
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{
					"role": "assistant", "content": content}}}})
			}))
			defer srv.Close()
			out := filepath.Join(t.TempDir(), "judge")
			code, stderr := runScript(t, "openai-compatible.sh", []string{"RHOBOUND=" + bin, "JUDGE_ENDPOINT=" + srv.URL + "/v1",
				"JUDGE_API_KEY=test-key", "JUDGE_MODEL_ID=example-model", "FIT=" + fit, "POLICY_NAME=default"}, prediction, policy, out)
			if code != 1 {
				t.Fatalf("exit %d, want the gate's 1 (block): %s", code, stderr)
			}
			var u struct {
				Judge  string  `json:"judge"`
				Status float64 `json:"judge_check_status"`
			}
			b, err := os.ReadFile(filepath.Join(out, "judge-unavailable.json"))
			if err != nil || json.Unmarshal(b, &u) != nil || u.Judge != "unavailable" || u.Status != c.status {
				t.Errorf("judge-unavailable.json: %v %s", err, b)
			}
			g, err := os.ReadFile(filepath.Join(out, "gate", "gate.json"))
			if err != nil || strings.Contains(string(g), "ci-judge") {
				t.Errorf("gate.json: %v %s", err, g)
			}
			if v, _ := os.ReadFile(filepath.Join(out, "gate", "verdict.txt")); strings.TrimSpace(string(v)) != "block" {
				t.Errorf("verdict %q", v)
			}
		})
	}
}
