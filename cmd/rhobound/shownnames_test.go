// SPDX-License-Identifier: Apache-2.0

package main

// The views show a fit's services one to one (sanitize.Services), and a caller names a service by what a view shows.
// A service named by a Lambda function's ARN is shown by the ARN's last element (c-fn), or by svc_<hash8> of the whole
// name when another service is shown by that element (a function c beside a service c). budget, preview and predict
// take each shown name for its service, and fit --workers too, exactly as they take the name the traces wrote; a name
// the views show for two services (one named like another's hash) means the one named so as the traces wrote it.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

func hashedName(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "svc_" + hex.EncodeToString(sum[:4])
}

// renamed writes the base capture with services renamed (old name -> new) and returns its folder.
func renamed(t *testing.T, names map[string]string) string {
	t.Helper()
	c := baseCapture(t)
	c.Visit(func(rs *testgen.ResourceSpans, s *testgen.Span) {
		if n, ok := names[rs.Service()]; ok {
			rs.SetAttr("service.name", testgen.Str(n))
		}
	})
	dir := filepath.Join(t.TempDir(), "renamed")
	if err := c.Write(dir, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestShownNamesResolve(t *testing.T) {
	inRepoRoot(t)
	account := strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	fn := func(name string) string { return "arn:aws:lambda:us-east-1:" + account + ":function:" + name }
	pol := writeTemp(t, t.TempDir(), "default.json", []byte(gatePolicy))
	for _, c := range []struct {
		name    string
		renames map[string]string
		raw     string // the renamed service, as the traces wrote it
		shown   string // and as every view shows it
	}{
		{"an ARN-named service", map[string]string{"c": fn("c-fn")}, fn("c-fn"), "c-fn"},
		{"an ARN whose last element another service is named", map[string]string{"b": fn("c")}, fn("c"), hashedName(fn("c"))},
	} {
		dir := renamed(t, c.renames)
		f := fitOf(t, dir)
		// context shows each service once, the renamed one by its shown name, untrusted
		ctx := twinView(t, "context", "--fit", f, "--detail=full")
		seen := map[string]bool{}
		found := false
		for _, x := range ctx["services"].([]any) {
			s := x.(map[string]any)
			n := s["name"].(string)
			if seen[n] {
				t.Errorf("%s: context shows two services as %q", c.name, n)
			}
			seen[n] = true
			found = found || n == c.shown && s["name_untrusted"] == true
		}
		if !found || strings.Contains(fmtJSON(t, ctx), account) {
			t.Errorf("%s: context does not show %q, untrusted, or shows the account: %v", c.name, c.shown, ctx["services"])
		}
		// budget, preview and predict on the shown name act on that service, as on its name in the traces
		for _, svc := range []string{c.shown, c.raw} {
			b := twinView(t, "budget", "--fit", f, "--policy", pol, "--service="+svc)
			items := b["items"].([]any)
			if len(items) == 0 || items[0].(map[string]any)["service"] != c.shown {
				t.Errorf("%s: budget --service=%s: %v", c.name, svc, b["items"])
			}
		}
		entries := func(args ...string) any {
			t.Helper()
			out := map[string][]any{}
			for _, svc := range []string{c.shown, c.raw} {
				ch := changeFile(t, f, change.Item{Op: change.OpOwnMs, Service: svc, Value: change.Point(0.4),
					Label: change.LabelDeclared})
				v := twinView(t, append(args, "--changes", ch)...)
				out[svc] = v["entries"].([]any)
			}
			if !reflect.DeepEqual(out[c.shown], out[c.raw]) {
				t.Errorf("%s: %s on %q and on its name in the traces differ:\n%v\n%v", c.name, args[0], c.shown,
					out[c.shown], out[c.raw])
			}
			return out[c.shown]
		}
		entries("preview", "--fit", f, "--policy", pol, "--now", nowOf(t, f))
		entries("predict", "--fit", f)
		// the preview's verdict names services as its rows do: each service its rules, decide_with and moved name is a
		// shown name of context, and its measurement line of a change of the renamed service names that service
		ch := changeFile(t, f, change.Item{Op: change.OpOwnMs, Service: c.shown, Value: change.Point(0.4),
			Label: change.LabelDeclared})
		pv := twinView(t, "preview", "--fit", f, "--policy", pol, "--now", nowOf(t, f), "--changes", ch)
		verdict := pv["preview"].(map[string]any)
		measured := false
		for _, d := range verdict["decide_with"].([]any) {
			if l, ok := strings.CutPrefix(d.(string), "an A/B of "); ok {
				measured = true
				if !strings.HasPrefix(l, c.shown+" (diff-fit)") {
					t.Errorf("%s: a change of %s: decide_with %q", c.name, c.shown, d)
				}
			}
			if w, ok := strings.CutPrefix(d.(string), "workers:"); ok && !seen[w] {
				t.Errorf("%s: decide_with %q names no service of context", c.name, d)
			}
		}
		if !measured {
			t.Errorf("%s: no measurement in decide_with %v", c.name, verdict["decide_with"])
		}
		for _, x := range verdict["rules"].([]any) {
			if e, _ := x.(map[string]any)["entry"].(string); e != "" && !seen[e] {
				t.Errorf("%s: the rule %v names no service of context", c.name, x)
			}
		}
		for _, x := range pv["moved"].([]any) {
			if s := x.(map[string]any)["service"].(string); !seen[s] {
				t.Errorf("%s: moved names %q, no service of context", c.name, s)
			}
		}
		// fit --workers on the shown name pins that service, and the record names it as the traces wrote it
		w := fitOf(t, dir, "--workers", c.shown+"=3")
		rec := readJSON(t, w)
		if ws := rec["options"].(map[string]any)["workers"].(map[string]any); !reflect.DeepEqual(ws, map[string]any{c.raw: 3.0}) {
			t.Errorf("%s: fit --workers %s=3 records %v", c.name, c.shown, ws)
		}
		for _, x := range rec["services"].([]any) {
			if s := x.(map[string]any); s["name"] == c.raw && s["workers"] != 3.0 {
				t.Errorf("%s: fit --workers %s=3 gives %v workers", c.name, c.shown, s["workers"])
			}
		}
	}

	// a service named like another's hash: the views show both alike, and that name means the service named so as the
	// traces wrote it (c), as each command takes it; the other (b c) is named by its name as written
	twin := hashedName("b c")
	dir := renamed(t, map[string]string{"b": "b c", "c": twin})
	f := fitOf(t, dir)
	for _, c := range []struct{ name, raw string }{{twin, twin}, {"b c", "b c"}} {
		b := twinView(t, "budget", "--fit", f, "--policy", pol, "--service="+c.name)
		if items := b["items"].([]any); len(items) == 0 || items[0].(map[string]any)["service"] != twin {
			t.Errorf("budget --service=%q: %v", c.name, b["items"])
		}
		ch := changeFile(t, f, change.Item{Op: change.OpOwnMs, Service: c.name, Value: change.Point(0.4), Label: change.LabelDeclared})
		twinView(t, "preview", "--fit", f, "--policy", pol, "--now", nowOf(t, f), "--changes", ch)
		report, _, code := exec(t, "predict", "--fit", f, "--changes", ch, "--json")
		var rep struct {
			Items []struct{ Service string } `json:"items"`
		}
		if code != 0 || json.Unmarshal([]byte(report), &rep) != nil || len(rep.Items) != 1 || rep.Items[0].Service != c.raw {
			t.Errorf("predict on %q: exit %d, items %+v, want the service %q", c.name, code, rep.Items, c.raw)
		}
		w := readJSON(t, fitOf(t, dir, "--workers", c.name+"=2"))
		if ws := w["options"].(map[string]any)["workers"].(map[string]any); !reflect.DeepEqual(ws, map[string]any{c.raw: 2.0}) {
			t.Errorf("fit --workers %s=2 records %v", c.name, ws)
		}
	}
}

func fmtJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func readJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// TestFindingsOfEachServiceReachEveryView: on a fit with a service c and a Lambda function whose ARN's last element is
// c (b renamed), check records trace.image_digest_missing about a, about c and about the function (subject c,
// untrusted). Every view of the fit and of a prediction on it lists all three, as explain on the fit does: the
// engine's report keeps a finding per (code, subject, subject_untrusted), as the views do.
func TestFindingsOfEachServiceReachEveryView(t *testing.T) {
	inRepoRoot(t)
	account := strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	dir := renamed(t, map[string]string{"b": "arn:aws:lambda:us-east-1:" + account + ":function:c"})
	f := fitOf(t, dir)
	pol := writeTemp(t, t.TempDir(), "default.json", []byte(gatePolicy))
	ch := changeFile(t, f, change.Item{Op: change.OpOwnMs, Service: "a", Value: change.Point(0.1), Label: change.LabelDeclared})
	report, _, code := exec(t, "predict", "--fit", f, "--changes", ch, "--json")
	if code != 0 {
		t.Fatalf("predict: exit %d", code)
	}
	pred := writeTemp(t, t.TempDir(), "report.json", []byte(report))
	var rep map[string]any
	if err := json.Unmarshal([]byte(report), &rep); err != nil {
		t.Fatal(err)
	}
	type shown struct {
		subject   string
		untrusted bool
	}
	want := []shown{{"a", false}, {"c", false}, {"c", true}}
	digestsMissing := func(where string, fs any) {
		t.Helper()
		if where != "the report of predict --fit" && strings.Contains(fmtJSON(t, fs), account) {
			t.Errorf("%s shows the account", where) // report v2 names services as the traces wrote them; views do not
		}
		var got []shown
		for _, x := range fs.([]any) {
			if m := x.(map[string]any); m["code"] == contract.TraceImageDigestMissing {
				u, _ := m["subject_untrusted"].(bool)
				got = append(got, shown{m["subject"].(string), u})
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s lists trace.image_digest_missing about %v, want %v", where, got, want)
		}
	}
	explain := func(target string, args ...string) any {
		v := twinView(t, append([]string{"explain", "--target", target, "--section", "findings", "--max-chars", "8000"}, args...)...)
		return v["content"].(map[string]any)["findings"]
	}
	digestsMissing("explain findings on the fit", explain(f))
	digestsMissing("context --detail=full", twinView(t, "context", "--fit", f, "--detail=full")["findings"])
	digestsMissing("the report of predict --fit", rep["findings"])
	digestsMissing("predict --fit, full", twinView(t, "predict", "--fit", f, "--changes", ch)["findings"])
	digestsMissing("preview --detail=full", twinView(t, "preview", "--fit", f, "--policy", pol, "--now", nowOf(t, f),
		"--changes", ch, "--detail=full")["findings"])
	digestsMissing("explain findings on the prediction", explain(pred, "--policy", pol, "--now", nowOf(t, f)))
}
