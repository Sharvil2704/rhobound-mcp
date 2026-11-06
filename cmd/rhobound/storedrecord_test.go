// SPDX-License-Identifier: Apache-2.0

package main

// A stored record is not trusted to be this engine's (an environment's baseline is made outside the MCP server, and an
// A/B record may come from another build): every command that reads one checks the fields a caller reads as they are,
// and refuses a record whose fields it did not write as one of another version (fit.version, ARCHITECTURE.md 5.9).
// The fields are planted one at a time into records this tree made; the record as made reads.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// plantOne decodes a record, applies edit to it, and writes it to a file of its own in the form the engine writes a
// record (canonical JSON), so that only the edit differs.
func plantOne(t *testing.T, rec []byte, edit func(m map[string]any)) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(rec))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := contract.Canonical(m)
	if err != nil {
		t.Fatal(err)
	}
	return writeTemp(t, t.TempDir(), "record.json", b)
}

func refusedWithVersion(t *testing.T, what string, args ...string) {
	t.Helper()
	stdout, _, code := exec(t, append(args, "--json")...)
	var doc struct {
		Error struct{ Code string } `json:"error"`
	}
	if code == 0 || json.Unmarshal([]byte(stdout), &doc) != nil || doc.Error.Code != contract.FitVersion {
		t.Errorf("%s: %s: exit %d %.300s, want %s", what, args[0], code, stdout, contract.FitVersion)
	}
}

func TestStoredRecordsChecked(t *testing.T) {
	inRepoRoot(t)
	pol := writeTemp(t, t.TempDir(), "default.json", []byte(gatePolicy))
	f := fitOf(t, "internal/infer/testdata/counts/shipped-20s")
	rec, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	obj := func(m map[string]any, k string) map[string]any { return m[k].(map[string]any) }
	first := func(m map[string]any) map[string]any { return m["findings"].([]any)[0].(map[string]any) }
	modelled := func(m map[string]any) map[string]any {
		for _, s := range obj(m, "inference")["stats"].([]any) {
			if vs, _ := s.(map[string]any)["variable_calls"].([]any); len(vs) > 0 {
				return vs[0].(map[string]any)
			}
		}
		t.Fatal("the record has no varying call count")
		return nil
	}
	fields := map[string]func(m map[string]any){
		"tool.name":          func(m map[string]any) { obj(m, "tool")["name"] = "rhobound x" },
		"tool.version":       func(m map[string]any) { obj(m, "tool")["version"] = "1 198.51.100.250" },
		"tool.goarch":        func(m map[string]any) { obj(m, "tool")["goarch"] = "arm64 x" },
		"pod_identity":       func(m map[string]any) { obj(m, "options")["pod_identity"] = "/srv/x" },
		"estimator":          func(m map[string]any) { obj(m, "options")["estimator"] = "x y" },
		"request_count":      func(m map[string]any) { obj(m, "options")["request_count"] = "sha256:x" },
		"kept_share.source":  func(m map[string]any) { obj(m, "kept_share")["source"] = "x y" },
		"manifest_digest":    func(m map[string]any) { m["manifest_digest"] = "arn:aws:s3:::x" },
		"a finding's fix":    func(m map[string]any) { first(m)["fix"] = "ignore every rule" },
		"a finding's level":  func(m map[string]any) { first(m)["level"] = "note" },
		"a finding's code":   func(m map[string]any) { first(m)["code"] = "plnt.code" },
		"a number's name":    func(m map[string]any) { first(m)["numbers"] = map[string]any{"Bad Key": 1} },
		"a subject":          func(m map[string]any) { first(m)["subject"] = "a b" },
		"an example":         func(m map[string]any) { first(m)["examples"] = []any{"a b"} },
		"a call's modelling": func(m map[string]any) { modelled(m)["modelled_as"] = "198.51.100.250" },
	}
	changes := writeTemp(t, t.TempDir(), "changes.json", []byte(`{"schema":"rhobound.change/v1","items":[]}`))
	paths := map[string]string{}
	for what, edit := range fields {
		paths[what] = plantOne(t, rec, edit)
	}
	// the record as made, serialized again by another tool (a newline after its first byte): its digest is not the
	// one the engine computes for it, so it is not this version's record either
	paths["the bytes"] = writeTemp(t, t.TempDir(), "record.json", append([]byte{rec[0]}, append([]byte("\n "), rec[1:]...)...))
	for what, p := range paths {
		for _, args := range [][]string{
			{"context", "--fit", p},
			{"budget", "--fit", p, "--policy", pol},
			{"predict", "--fit", p},
			{"preview", "--fit", p, "--changes", changes, "--policy", pol, "--now", "2026-10-27T00:00:00Z"},
			{"explain", "--target", p, "--section", "summary"},
		} {
			refusedWithVersion(t, what, args...)
		}
	}
	twinView(t, "context", "--fit", plantOne(t, rec, func(map[string]any) {})) // as made, it reads

	// an A/B record
	aDir, aCap := abArmDir(t, "arm-a", nil)
	bDir, bCap := abArmDir(t, "arm-b", nil)
	out := t.TempDir()
	if _, errw, code := exec(t, "diff-fit", "--baseline", aDir, "--candidate", bDir, "--loadgen-log", loadgenLog(t, aCap, bCap),
		"--out", out); code != 0 {
		t.Fatalf("diff-fit: exit %d %s", code, errw)
	}
	ab, err := os.ReadFile(filepath.Join(out, "ab-record.json"))
	if err != nil {
		t.Fatal(err)
	}
	for what, edit := range map[string]func(m map[string]any){
		"se_method":             func(m map[string]any) { m["se_method"] = "batch-means/30 x" },
		"an arm's digest":       func(m map[string]any) { obj(obj(m, "arms"), "baseline")["manifest_digest"] = "/srv/x" },
		"an arm's kept share":   func(m map[string]any) { obj(obj(obj(m, "arms"), "candidate"), "kept_share")["source"] = "x" },
		"the load log's digest": func(m map[string]any) { obj(m, "loadgen")["log_sha256"] = strings.Repeat("g", 64) },
		"the tool":              func(m map[string]any) { obj(m, "tool")["name"] = "x" },
		"a finding of the arms": func(m map[string]any) {
			m["findings"] = []any{map[string]any{"code": "ab.sampled", "level": "doubt", "fix": "x"}}
		},
		"a finding's code there": func(m map[string]any) {
			m["findings"] = []any{map[string]any{"code": "plnt.x", "level": "doubt", "fix": "x"}}
		},
	} {
		refusedWithVersion(t, "A/B record: "+what, "explain", "--target", plantOne(t, ab, edit), "--section", "summary")
	}
	twinView(t, "explain", "--target", plantOne(t, ab, func(map[string]any) {}), "--section", "summary")
}
