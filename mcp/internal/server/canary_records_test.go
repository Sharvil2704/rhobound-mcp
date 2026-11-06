// SPDX-License-Identifier: Apache-2.0

package server

// The leak gate's records shaped by hand. A baseline is made outside the server (DEPLOY_AWS.md 14), and A/B and job
// records may have been written by another build of the server or its worker, so the gate does not trust a stored
// record to be the configured engine's: it stores records whose every string field holds a canary of its own, and
// sweeps them like any other.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// plantAddress is the address in every token a planter writes: a string with a space and an address, which the name
// rule would hash, so that one shown raw is seen.
const plantAddress = "198.51.100.250"

// plantAccount is the account of every ARN a planter writes (built, as the other test accounts are, so that no
// account id is written out).
var plantAccount = strings.Repeat("4", 4) + strings.Repeat("5", 4) + strings.Repeat("6", 4)

// planter writes a canary of its own into each field it is given, as a token (with a space and an address) or as an
// AWS ARN whose cluster is the canary (the name rule shows only its last element), and keeps what it planted.
type planter struct {
	arn      bool
	canaries []canary
}

// at returns the value planted in a field, named by a short word of [a-z0-9].
func (p *planter) at(field, what string) string {
	c := "plnt-" + field + "-5e1f"
	p.canaries = append(p.canaries, canary{c, what + " of a record shaped by hand"})
	if p.arn {
		return "arn:aws:ecs:us-east-1:" + plantAccount + ":task/" + c + "/tail"
	}
	return c + " " + plantAddress + ":5432"
}

// forbidden are the strings no reply may carry once the planter has run: its canaries, the account of the ARNs every
// planter may write (plantNames, plantCapture, plantJobs), in the form an ARN writes it and in the dashed form the AWS
// console prints, and the address of its tokens.
func (p *planter) forbidden() []canary {
	out := append([]canary(nil), p.canaries...)
	out = append(out, canary{plantAccount, "the account of the ARNs planted in records shaped by hand"},
		canary{dashedAccount, "the account of the ARNs planted in records shaped by hand, dashed"})
	if p.arn {
		return out
	}
	return append(out, canary{plantAddress, "the address planted in records shaped by hand"})
}

// dashedAccount is plantAccount as the AWS console prints an account id: three groups of four digits.
var dashedAccount = plantAccount[:4] + "-" + plantAccount[4:8] + "-" + plantAccount[8:]

// accountARNs are AWS resource names whose last element holds their own account, which the name rule would otherwise
// show as that element: an assumed role's session named after the account, a function named after it, an ECS task
// whose id holds it, and a session named after the account in its dashed form.
func accountARNs() []string {
	return []string{
		"arn:aws:sts::" + plantAccount + ":assumed-role/plnt-role-5e1f/" + plantAccount + "-sess",
		"arn:aws:lambda:us-east-1:" + plantAccount + ":function:fn-" + plantAccount,
		"arn:aws:ecs:us-east-1:" + plantAccount + ":task/plnt-cl-5e1f/" + plantAccount + "0a1b",
		"arn:aws:sts::" + plantAccount + ":assumed-role/plnt-role-5e1f/" + dashedAccount + "-sess",
	}
}

// accountNumber is a number's name that holds an account: snake_case, as a number's name of a finding is, but no
// number of any code.
var accountNumber = "acct_" + plantAccount

func decodeRecord(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func encodeRecord(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func jsonList(v any) []any { l, _ := v.([]any); return l }

func jsonObject(v any) map[string]any { m, _ := v.(map[string]any); return m }

// plantFindings plants into each finding of a list: its fix, a key of its numbers, its examples and, in a finding it
// adds, its code, its level and its subject (a folder of the server).
func plantFindings(fs []any, p *planter, prefix, folder string) []any {
	fs = append(fs, map[string]any{"code": "plnt.added", "level": "doubt", "fix": "x"})
	for i, x := range fs {
		f := jsonObject(x)
		f["fix"] = p.at(fmt.Sprintf("%sfix%d", prefix, i), "a finding's fix")
		f["numbers"] = map[string]any{p.at(fmt.Sprintf("%snum%d", prefix, i), "a key of a finding's numbers"): json.Number("1")}
		f["examples"] = []any{p.at(fmt.Sprintf("%sex%d", prefix, i), "a finding's example"), folder + "/plnt-example"}
		if i == len(fs)-1 {
			f["code"] = "plnt.x_" + strings.ReplaceAll(p.at(prefix+"code", "a finding's code"), " ", "_")
			f["level"] = p.at(prefix+"level", "a finding's level")
			f["subject"] = folder + "/plnt-subject"
		}
	}
	return fs
}

// plantFit plants into every string field of a fit record: the summary (capture, tool, options, kept share, manifest
// digest, images, findings) and the mirror of its inference (warnings and their subjects, image names, pods, how a
// varying call count is modelled, the topology's name, a merged nested span's service).
func plantFit(t *testing.T, rec []byte, p *planter, folder string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	m["capture"] = p.at("capture", "the capture")
	tool := jsonObject(m["tool"])
	tool["name"], tool["version"], tool["goarch"] = p.at("toolname", "the tool's name"),
		p.at("toolversion", "the tool's version"), p.at("goarch", "the tool's GOARCH")
	opt := jsonObject(m["options"])
	opt["pod_identity"], opt["estimator"] = p.at("podidentity", "the pod identity option"), p.at("estimator", "the estimator")
	opt["probe_routes"] = []any{p.at("proberoute", "a probe route option")}
	opt["request_count"] = p.at("requestcount", "the request count's digest")
	w := jsonObject(opt["workers"])
	if w == nil {
		w = map[string]any{}
		opt["workers"] = w
	}
	w[p.at("workers", "a workers option's service")] = json.Number("2")
	jsonObject(m["kept_share"])["source"] = p.at("keptsource", "the kept share's source")
	m["manifest_digest"] = p.at("manifest", "the manifest digest")
	m["images"] = map[string]any{p.at("imageskey", "a key of images"): []any{p.at("imagesvalue", "an image digest")}}
	m["findings"] = plantFindings(jsonList(m["findings"]), p, "", folder)
	plantMirror(m, p)
	for _, x := range jsonList(jsonObject(m["inference"])["stats"]) {
		for _, v := range jsonList(jsonObject(x)["variable_calls"]) {
			jsonObject(v)["modelled_as"] = p.at("modelledas", "a call's model")
		}
	}
	return encodeRecord(t, m)
}

// plantMirror plants into the names and free text of a fit record's inference mirror that its checks cannot judge:
// warnings and their subjects, image names, pods, the topology's name, and the service of a merged nested span, which
// names a service of the capture whether or not it is a row (one that emits no SERVER span has none), and which
// rhobound://prediction serves (report.nested_spans_merged).
func plantMirror(m map[string]any, p *planter) {
	inf := jsonObject(m["inference"])
	inf["warnings"] = append(jsonList(inf["warnings"]), p.at("warning", "a warning"))
	inf["warning_codes"] = append(jsonList(inf["warning_codes"]), "")
	inf["warning_subjects"] = append(jsonList(inf["warning_subjects"]), p.at("warnsubject", "a warning's subject"))
	for _, x := range jsonList(inf["stats"]) {
		st := jsonObject(x)
		st["images"] = []any{p.at("statimage", "a service's image")}
		for _, s := range jsonList(st["pod_shares"]) {
			jsonObject(s)["pod"] = p.at("pod", "a pod")
		}
	}
	inf["nested_spans"] = append(jsonList(inf["nested_spans"]), map[string]any{
		"service": p.at("nested", "a merged nested span's service"), "server_spans": json.Number("1"),
		"client_spans": json.Number("1")})
	jsonObject(inf["topology"])["name"] = p.at("topology", "the topology's name")
}

// The node-name fields of a fit record's inference mirror that plantNode plants into, one per record: a varying
// call's callee, a word of a call shape, a node's name in the model apart from its row, and the caller that a stage
// node's name holds (stage:S:n, which the report's biased constructs read as S).
const (
	nodeCallee = "vccallee"
	nodeShape  = "callshape"
	nodeModel  = "toponode"
	nodeStage  = "stagecaller"
)

// plantNode plants into one node-name field of a fit record's inference mirror (nodeCallee, nodeShape, nodeModel or
// nodeStage) a name that is not one of its rows (stats[].name). Every node a record names must be one of its rows,
// which rhobound://prediction names as the views do (served), so the engine refuses such a record as one its version
// did not write (fit.version).
func plantNode(t *testing.T, rec []byte, p *planter, field string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	inf := jsonObject(m["inference"])
	stats := jsonList(inf["stats"])
	model := jsonList(jsonObject(inf["topology"])["services"])
	rowNamed := func(name string) int {
		for i, x := range stats {
			if jsonObject(x)["name"] == name {
				return i
			}
		}
		t.Fatalf("plantNode %s: the record has no row %q", field, name)
		return -1
	}
	planted := false
	switch field {
	case nodeCallee:
		for _, x := range stats {
			for _, v := range jsonList(jsonObject(x)["variable_calls"]) {
				jsonObject(v)["callee"], planted = p.at(field, "a varying call's callee"), true
			}
		}
	case nodeShape:
		// a row's name, then the planted one: a shape is refused for any word that is not a row
		jsonObject(stats[rowNamed("a")])["call_shapes"] = []any{map[string]any{
			"shape": "b then " + p.at(field, "a word of a call shape"), "share": json.Number("1")}}
		planted = true
	case nodeModel:
		// a, which calls a peer asynchronously (the report's async_calls), and b
		jsonObject(model[rowNamed("a")])["name"] = p.at(field+"a", "a calling node's name in the model")
		jsonObject(model[rowNamed("b")])["name"], planted = p.at(field, "a node's name in the model"), true
	case nodeStage:
		for i, x := range stats {
			if st := jsonObject(x); st["stage"] == true {
				name := "stage:" + p.at(field, "the caller a stage node names") + ":1"
				st["name"], jsonObject(model[i])["name"], planted = name, name, true
				break
			}
		}
	}
	if !planted {
		t.Fatalf("plantNode %s: the record has no such field", field)
	}
	return encodeRecord(t, m)
}

// plantNames plants into the strings a record's own checks cannot judge: the capture, a workers option's service,
// image digests, the subjects and examples of findings (a path that names the server's folder, in the findings the
// record has and in two it adds with their codes' levels and fixes; trace.probe_like_traffic's examples are routes,
// which may start with /, and are left alone), and the inference mirror. The rest of the record stays as the engine
// wrote it, so the record is read, and each such string must reach no reply.
func plantNames(t *testing.T, rec []byte, p *planter, folder string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	m["capture"] = folder + "/" + p.at("namecapture", "the capture")
	opt := jsonObject(m["options"])
	w := jsonObject(opt["workers"])
	if w == nil {
		w = map[string]any{}
		opt["workers"] = w
	}
	w[p.at("nameworkers", "a workers option's service")] = json.Number("2")
	m["images"] = map[string]any{p.at("nameimageskey", "a key of images"): []any{p.at("nameimages", "an image digest")}}
	fs := jsonList(m["findings"])
	// trace.errors_present is a doubt the default policy holds (rule 5), whose row names the finding's subject
	fs = append(fs, registered(t, "trace.pod_churn", "trace.edge_loss", "trace.errors_present")...)
	for _, x := range fs {
		f := jsonObject(x)
		f["subject"] = folder + "/plnt-subject"
		if f["code"] != "trace.probe_like_traffic" {
			f["examples"] = []any{folder + "/plnt-example"}
		}
	}
	// ARNs whose last element holds their own account, as a subject and as examples: shown through the name rule, which
	// must hash them whole
	arns := accountARNs()
	for _, a := range arns {
		tail := registered(t, "trace.pod_shared")
		jsonObject(tail[0])["subject"] = a
		jsonObject(tail[0])["examples"] = []any{arns[1], arns[2]}
		fs = append(fs, tail...)
	}
	m["findings"] = fs
	plantMirror(m, p)
	return encodeRecord(t, m)
}

// plantCapture sets a record's capture to capture, the rest as the engine wrote it, so that the record is read: a fit
// made outside the server records its capture as given to fit, and nothing checks what that was.
func plantCapture(t *testing.T, rec []byte, capture string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	m["capture"] = capture
	return encodeRecord(t, m)
}

// plantNumber plants a number whose name holds an account (accountNumber) into a finding of a registered code with a
// subject the views show: a record that carries it is not one the engine wrote, as no code has that number.
func plantNumber(t *testing.T, rec []byte, p *planter) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	num := registered(t, "trace.pod_churn")
	jsonObject(num[0])["subject"] = "a"
	jsonObject(num[0])["numbers"] = map[string]any{accountNumber: json.Number("1")}
	p.canaries = append(p.canaries, canary{accountNumber, "a number's name of a record shaped by hand"})
	m["findings"] = append(jsonList(m["findings"]), num...)
	return encodeRecord(t, m)
}

// registered returns a finding of each code with its registry level and fix, as CheckStored admits one.
func registered(t *testing.T, codes ...string) []any {
	t.Helper()
	var out []any
	for _, code := range codes {
		reg, ok := registry.Lookup(code)
		if !ok {
			t.Fatalf("%s is not registered", code)
		}
		out = append(out, map[string]any{"code": code, "level": reg.Level, "fix": reg.Fix})
	}
	return out
}

// plantABNames plants into the strings an A/B record's own checks cannot judge: each arm's capture, a callee of each
// service, and a finding it adds with its code's level and fix (a doubt the default policy holds, which a measured
// change carries into its report) whose subject is a path that names the server's folder. The rest of the record stays
// as the engine wrote it, so the record is read.
func plantABNames(t *testing.T, rec []byte, p *planter, folder string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	for _, a := range []string{"baseline", "candidate"} {
		jsonObject(jsonObject(m["arms"])[a])["capture"] = p.at("abnamecapture"+a[:1], "an A/B arm's capture")
	}
	for i, x := range jsonList(m["services"]) {
		s := jsonObject(x)
		s["calls"] = append(jsonList(s["calls"]), map[string]any{"callee": p.at(fmt.Sprintf("abnamecallee%d", i), "an A/B callee"),
			"baseline": json.Number("1"), "candidate": json.Number("1")})
	}
	added := registered(t, "trace.errors_present")
	jsonObject(added[0])["subject"] = folder + "/plnt-absubject"
	m["findings"] = append(jsonList(m["findings"]), added...)
	return encodeRecord(t, m)
}

// plantAB plants into every string field of an A/B record: the tool's version, the standard errors' method, each arm's
// capture, manifest digest and kept share's source, the load generator's log digest, a callee of each service, and
// findings.
func plantAB(t *testing.T, rec []byte, p *planter, folder string) []byte {
	t.Helper()
	m := decodeRecord(t, rec)
	jsonObject(m["tool"])["version"] = p.at("abtoolversion", "an A/B record's tool version")
	m["se_method"] = p.at("semethod", "an A/B record's standard errors' method")
	for _, a := range []string{"baseline", "candidate"} {
		arm := jsonObject(jsonObject(m["arms"])[a])
		arm["capture"] = p.at("abcapture"+a[:1], "an A/B arm's capture")
		arm["manifest_digest"] = p.at("abmanifest"+a[:1], "an A/B arm's manifest digest")
		jsonObject(arm["kept_share"])["source"] = p.at("abkept"+a[:1], "an A/B arm's kept share's source")
	}
	if lg := jsonObject(m["loadgen"]); lg != nil {
		lg["log_sha256"] = p.at("loadgen", "the load generator log's digest")
	}
	for i, x := range jsonList(m["services"]) {
		s := jsonObject(x)
		s["calls"] = append(jsonList(s["calls"]), map[string]any{"callee": p.at(fmt.Sprintf("abcallee%d", i), "an A/B callee"),
			"baseline": json.Number("1"), "candidate": json.Number("1")})
	}
	m["findings"] = plantFindings(jsonList(m["findings"]), p, "ab", folder)
	return encodeRecord(t, m)
}

// plantJobs stores, for principal, job records with a planted refusal (its code's reason, fix, subject, a key of its
// numbers and a key no refusal has) and findings, and returns their handles. Their other fields are ones the server
// writes, which the store checks when a job is stored (plantJobFiles plants those).
func plantJobs(t *testing.T, store state.Store, principal state.Principal, now time.Time, p *planter, folder string) []string {
	t.Helper()
	ctx := context.Background()
	refusal := func(sub string) json.RawMessage {
		return encodeRecord(t, map[string]any{"code": "fit.version", "reason": p.at("jobreason", "a job refusal's reason"),
			"fix": p.at("jobfix", "a job refusal's fix"), "subject": sub,
			"numbers": map[string]any{p.at("jobnum", "a key of a job refusal's numbers"): json.Number("1")},
			"extra":   p.at("jobextra", "a key a job refusal does not have")})
	}
	findings := encodeRecord(t, plantFindings([]any{map[string]any{"code": "trace.pod_churn", "level": "doubt",
		"fix": "x", "subject": "a"}}, p, "job", folder))
	// findings and refusals with an ARN whose last element holds its account as their subject, and a number whose name
	// holds an account
	arns := accountARNs()
	var held []any
	for _, a := range arns {
		held = append(held, map[string]any{"code": "trace.pod_shared", "level": "doubt", "fix": "x", "subject": a,
			"numbers": map[string]any{accountNumber: json.Number("1"), "services": json.Number("2")}})
	}
	p.canaries = append(p.canaries, canary{accountNumber, "a number's name of a job record shaped by hand"})
	arnRefusal := func(sub string) json.RawMessage {
		return encodeRecord(t, map[string]any{"code": "fit.version", "reason": "x", "fix": "x", "subject": sub,
			"numbers": map[string]any{accountNumber: json.Number("1")}})
	}
	var ids []string
	for i, j := range []state.Job{
		{State: state.JobFailed, Step: "check", Refusal: refusal("a"), Findings: findings},
		{State: state.JobFailed, Step: "fit", Refusal: refusal(folder + "/plnt-subject")},
		{State: state.JobFailed, Step: "check", Refusal: refusal(p.at("jobsubject", "a job refusal's subject"))},
		{State: state.JobFailed, Step: "check", Refusal: arnRefusal(arns[0]), Findings: encodeRecord(t, held)},
		{State: state.JobFailed, Step: "fit", Refusal: arnRefusal(arns[1])},
		{State: state.JobFailed, Step: "fit", Refusal: arnRefusal(arns[2])},
		{State: state.JobFailed, Step: "fit", Refusal: arnRefusal(arns[3])},
	} {
		id, err := state.NewHandle(state.KindJob, now.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		j.ID, j.Principal, j.CreatedAt, j.DeadlineAt = id, principal, now, now.Add(time.Hour)
		if err := store.PutJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// plantJobFiles stores, for principal, job records in a Dir store as the server writes them, then rewrites each file
// as another build of the server or its worker may have left it: a planted state, step, result (a fit handle's and an
// A/B handle's kind, then a planted rest) and id, and a file that is not JSON. It returns their handles: the store
// reads each file back.
func plantJobFiles(t *testing.T, store *state.Dir, dir string, principal state.Principal, now time.Time, p *planter) []string {
	t.Helper()
	var ids []string
	for i, edit := range []func(m map[string]any){
		func(m map[string]any) { m["state"] = p.at("jobstate", "a job's state") },
		func(m map[string]any) { m["step"] = p.at("jobstep", "a job's step") },
		func(m map[string]any) { m["result"] = "fit_" + p.at("jobresult", "a job's result handle") },
		func(m map[string]any) { m["result"] = "ab_" + p.at("jobresultab", "a job's A/B result handle") },
		func(m map[string]any) { m["id"] = "job_" + p.at("jobid", "a job's id") },
		nil, // not JSON
	} {
		id, err := state.NewHandle(state.KindJob, now.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutJob(context.Background(), state.Job{ID: id, Principal: principal, State: state.JobCompleted,
			Step: "fit", CreatedAt: now, DeadlineAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		editJobFile(t, dir, id, edit)
		ids = append(ids, id)
	}
	return ids
}

// editJobFile rewrites the file in which a Dir store under dir keeps a job (principals/<key>/jobs/<id>.json): edit
// changes its fields, and nil leaves a file that is not JSON.
func editJobFile(t *testing.T, dir, id string, edit func(m map[string]any)) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "principals", "*", "jobs", id+".json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("the job %s is in %d files of the store (%v)", id, len(files), err)
	}
	b := []byte("{")
	if edit != nil {
		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		m := decodeRecord(t, raw)
		edit(m)
		b = encodeRecord(t, m)
	}
	if err := os.WriteFile(files[0], b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeAB stores an A/B record as the server and the engine store one, and mints a handle on it for the sweeper's
// caller.
func storeAB(t *testing.T, w *sweeper, root string, rec []byte) string {
	t.Helper()
	ctx := context.Background()
	d, err := w.store.PutArtifact(ctx, "ab", rec)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "engine-state", "ab")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, strings.TrimPrefix(d, "sha256:")+".json"), rec, 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := w.store.Mint(ctx, w.who, state.KindAB, d, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// lenientViews is views for a record shaped by hand, which may be refused anywhere: every view, resource and prompt on
// a fit handle and, when there is one, an A/B handle, with no check of what answers.
func (w *sweeper) lenientViews(fit, ab string) {
	w.t.Helper()
	f := `"fit_id":"` + fit + `"`
	w.tool("context", `{`+f+`}`)
	w.tool("context", `{`+f+`,"detail":"full"}`)
	w.tool("budget", `{`+f+`,"service":"b"}`)
	w.tool("budget", `{`+f+`,"policy":"strict"}`)
	var preds []string
	args := []string{`{` + f + `,"changes":[{"op":"own_ms","service":"b","value":{"lo":3.5,"hi":6.0}}],"repo":"shop","branch":"x"}`,
		`{` + f + `,"changes":[{"op":"own_ms","service":"c","value":0.4}],"detail":"full"}`}
	if ab != "" {
		args = append(args, `{`+f+`,"measured":[{"ab":"`+ab+`"}]}`,
			`{`+f+`,"changes":[{"op":"own_ms","service":"c","value":0.4}],"measured":[{"ab":"`+ab+`"}],"detail":"full"}`)
	}
	for _, a := range args {
		if p, _ := w.tool("predict", a)["prediction_id"].(string); p != "" {
			preds = append(preds, p)
		}
	}
	targets := append([]string{fit}, preds...)
	if ab != "" {
		targets = append(targets, ab)
	}
	for _, tg := range targets {
		for _, sec := range []string{"summary", "services", "findings", "assumptions", "rules", "judge_packet"} {
			w.tool("explain", `{"target":"`+tg+`","section":"`+sec+`","max_chars":8000}`)
		}
	}
	w.read(fitURI + fit)
	for _, p := range preds {
		w.read(predictionURI + p)
		w.read(packetURI + p)
		w.prompt(promptReview, map[string]string{"prediction_id": p})
	}
}

// plantedSurfaces calls the read surfaces on the environment's baseline, a fit record shaped by hand, whatever context
// with env answered, through handles on it that the store mints for the caller, as a handle minted before an engine
// upgrade names a record the engine reads no more: context, predict, budget and explain on a fit handle, and
// rhobound://fit; explain, rhobound://prediction, rhobound://judge-packet and the review prompt on a prediction handle
// whose record and change set name it (a prediction the caller made on the engine's own record, clean, then made to
// name it). It returns each surface that does not refuse the record with fit.version when refused is true, and each
// surface that does not answer when it is false.
func (w *sweeper) plantedSurfaces(clean []byte, refused bool) []string {
	w.t.Helper()
	ctx := context.Background()
	digest, err := w.store.Baseline(ctx, "staging")
	if err != nil {
		w.t.Fatal(err)
	}
	mint := func(kind, d string) string {
		h, err := w.store.Mint(ctx, w.who, kind, d, time.Hour)
		if err != nil {
			w.t.Fatal(err)
		}
		return h
	}
	fit := mint(state.KindFit, digest)
	cd, err := w.store.PutArtifact(ctx, "fit", clean)
	if err != nil {
		w.t.Fatal(err)
	}
	made, _ := w.tool("predict", `{"fit_id":"`+mint(state.KindFit, cd)+
		`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)["prediction_id"].(string)
	if made == "" {
		w.t.Fatalf("%s: predict minted no prediction on the engine's record", w.name)
	}
	// the prediction's record and its change set name the record (a change set names the fit it was written against)
	m := record(w.t, w.store, w.digest(made))
	changes := record(w.t, w.store, m["changes"].(string))
	changes["fit"] = digest
	if m["changes"], err = w.store.PutArtifact(ctx, "change", encodeRecord(w.t, changes)); err != nil {
		w.t.Fatal(err)
	}
	m["fit"] = digest
	pd, err := w.store.PutArtifact(ctx, "prediction", encodeRecord(w.t, m))
	if err != nil {
		w.t.Fatal(err)
	}
	pred := mint(state.KindPrediction, pd)

	var bad []string
	tool := func(name, args string) {
		got := w.tool(name, args)
		code := errorCode(got)
		if refused && code != codeFitVersion || !refused && (got == nil || code != "") {
			bad = append(bad, fmt.Sprintf("%s %s: %.200v", name, args, got))
		}
	}
	other := func(what string, err error) {
		if refused && (err == nil || !strings.Contains(err.Error(), codeFitVersion+":")) || !refused && err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", what, err))
		}
	}
	f := `"fit_id":"` + fit + `"`
	tool("context", `{`+f+`}`)
	tool("predict", `{`+f+`,"changes":[{"op":"own_ms","service":"c","value":0.4}]}`)
	tool("budget", `{`+f+`,"policy":"strict"}`)
	tool("explain", `{"target":"`+fit+`","section":"summary","max_chars":8000}`)
	other("rhobound://fit", w.read(fitURI+fit))
	tool("explain", `{"target":"`+pred+`","section":"summary","max_chars":8000}`)
	other("rhobound://prediction", w.read(predictionURI+pred))
	other("rhobound://judge-packet", w.read(packetURI+pred))
	other(promptReview, w.prompt(promptReview, map[string]string{"prediction_id": pred}))
	if refused {
		w.lenientViews(fit, "") // the rest of the surfaces, which context with env gave no handle for
	}
	return bad
}

// errorCode is the code of a refusal's structuredContent ({"error": Refusal}), or "".
func errorCode(m map[string]any) string {
	c, _ := jsonObject(m["error"])["code"].(string)
	return c
}
