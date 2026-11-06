// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// TestServerCanariesNeverLeak: no trace value that ARCHITECTURE.md 5.9 keeps from a caller, and no folder, reaches any
// reply. The canary captures (gencontract canaries, from internal/testgen) plant each such value as a unique string:
// peers no service answers, under every peer key, with and without a port, called a varying number of times,
// fire-and-forget, and at once on some visits (call shapes); a known callee's address on the CLIENT spans of the 2.5%
// of traces that lost its spans (admitted with doubts) and of 12.5% (refused); a peer renamed by a cluster dump; an A/B
// pair whose calls name a peer; raw request paths; an image name that is a registry's URI, with its tag; an http.route
// of probe-like entries in which ext: starts a word; ECS task ARNs as the pod identity, with their account and cluster,
// and the other ARNs of an ECS resource; container ARNs as service.instance.id, and an ARN after "task_" as one; a
// service whose name the name rule hashes, which every reply must name as the views do; a service named as the model
// names a stage node (stage:S:1), on the path to the changed service, which the biased constructs pass through;
// services named so that a name runs across the words of a call shape (w then ext: before a peer, w then q before a
// service whose name the rule hashes); and a trace id that is text, not an id, on spans the reader skips. Their table, with the folders laid out here (the server's own, and those of
// baselines fitted outside the server without --name, from a folder and from a cut's manifest) and the canaries of the
// records shaped by hand below, is the forbidden list: a canary added to the table is looked for everywhere. Before the
// sweep, each canary is shown to be where the engine's output (or, for what the reader drops or the name rule hashes,
// the traces) holds it, and what the engine shows in its place to be there, so that the sweep looks for something that
// is there.
//
// Each baseline (every capture fitted outside the server from its folder with --name; the capture with peers also from
// a cut's manifest with --name, as DEPLOY_AWS.md 14 says, and from a folder and that manifest without it; and the ECS
// capture's record as an engine without the reader's task-id rule wrote it, with the whole ARN) is swept through every
// read-scope surface (views: context, budget, predict, explain on the fit and the prediction, the fit, prediction and
// judge-packet resources, the prompts, job_status and the refusals), in both profiles, both protocol revisions and both
// transports (in memory as in stdio mode, and HTTP with a read-only token). Fits made by the server are swept in stdio
// mode (fit_start, and context{capture} in the bench profile), with the refusals of a fit job; the A/B surfaces
// (predict with a measured A/B, explain on its handle, a refused A/B job) in stdio mode and over HTTP with a fit token,
// since only the caller who made an A/B record can use it.
//
// Records shaped by hand (canary_records_test.go), since a stored record is not trusted to be the configured engine's:
// a fit record with every string field holding a canary of its own, as tokens and as ARNs, stored as the baseline,
// which the engine refuses as a record its version did not write (fit.version), and so the engine's record
// re-serialized; one for each node-name field of its inference mirror (a varying call's callee, a word of a call shape,
// a node's name in the model, the caller a stage node names) holding a name that is not one of its rows, which it
// refuses so too; one with only the names its checks cannot judge planted (among them the subject of a doubt the
// default policy holds, which the gate's rule 5 names, ARNs whose last element holds their account, also in its dashed
// form, and a merged nested span's service, which may name a service that is not a row), and two whose capture is an
// ARN that holds its account (alone, and after a folder), which it reads; all swept in both profiles and both
// transports, on every read surface whatever context with env answered (through handles minted on the record as before
// an engine upgrade), each surface refusing a record the engine refuses with fit.version. In stdio mode: A/B records
// with every field planted, stored and minted for the caller, and one with only its names planted; job records with a
// planted refusal and findings; prediction records with a planted policy name and clock; and job records in a Dir store
// whose files were rewritten after they were stored (a planted state, step, result and id, and a file that is not
// JSON), read by job_status and, for the id, by fit_start repeating a request.
//
// No reply, error message or error data may carry a canary, the model's ext: prefix, an AWS ARN, or a folder; and none
// may be an internal error, such as a document the server would not serve, so that no planted string can stop a
// surface answering.
func TestServerCanariesNeverLeak(t *testing.T) {
	realCLI(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, serverCanary)
	table, captures := canaryCaptures(t, filepath.Join(root, "captures"))
	layOutCanaries(t, root)
	forbidden := []canary{{serverCanary, "the server's folder"}, {tmp, "the test's folder"},
		{real.root, "the repository"}, {real.dir, "the engine's build folder"}, {arnCanary, "an AWS ARN"}}
	for _, c := range table {
		for _, p := range c.Planted {
			key := p.Key
			if key == "" {
				key = "no attribute"
			}
			forbidden = append(forbidden, canary{p.Value, c.Name + ": " + p.Class + ", " + key})
		}
	}
	// the captures the sweep needs one of: a capture with peers no service answers (its folder forms are fitted too),
	// one with none (the A/B surfaces' baseline), a refused one, and an A/B pair
	var peers, plain, refused, pair string
	for _, c := range table {
		switch {
		case c.Pair:
			pair = cmp.Or(pair, c.Name)
		case c.Refused:
			refused = cmp.Or(refused, c.Name)
		case slices.ContainsFunc(c.Planted, func(p canaryPlanted) bool { return p.Class == "peer" }):
			peers = cmp.Or(peers, c.Name)
		default:
			plain = cmp.Or(plain, c.Name)
		}
	}
	if peers == "" || plain == "" || refused == "" || pair == "" {
		t.Fatalf("the canary table lacks a kind of capture the sweep needs: %+v", table)
	}
	peersDir := filepath.Join(root, "captures", peers)
	now := windowEnd(t, peersDir).Add(time.Hour)

	// baselines made outside the server: every capture fitted with --name, and the one with peers from a folder and
	// from a cut's manifest without it (their records name those folders, which no reply may)
	type baseline struct{ capture, form string }
	var baselines []baseline
	fits := map[baseline][]byte{}
	out := filepath.Join(tmp, "outside")
	held := map[string][]byte{} // what the engine printed or wrote for each capture
	for _, c := range table {
		dir := filepath.Join(root, "captures", c.Name)
		switch {
		case c.Pair:
			continue
		case c.Refused:
			b, exit := outsideCLI(t, out, "fit", dir, "--name=staging", "--out", filepath.Join(out, c.Name), "--json")
			if exit == 0 {
				t.Fatalf("%s: the engine fits it", c.Name)
			}
			held[c.Name] = b
		default:
			base := baseline{c.Name, c.Name + " fitted outside with --name"}
			fits[base] = outsideFit(t, out, c.Name, dir, "--name=staging")
			held[c.Name] = fits[base]
			baselines = append(baselines, base)
		}
	}
	folder := filepath.Join(tmp, folderCanary, "wide")
	copyTree(t, peersDir, folder)
	base := baseline{peers, peers + " fitted outside from a folder"}
	if fits[base] = outsideFit(t, out, "folder", folder); !bytes.Contains(fits[base], []byte(folder)) {
		t.Fatalf("the fit made from a folder does not record it")
	}
	baselines = append(baselines, base)
	raw := filepath.Join(out, "raw")
	copyTree(t, filepath.Join(peersDir, "traces"), filepath.Join(raw, "traces"))
	cut := filepath.Join(tmp, manifestCanary, "cut")
	end := windowEnd(t, peersDir)
	if _, exit := outsideCLI(t, out, "check", raw, "--window", end.Add(-3*time.Minute).Format(time.RFC3339)+","+
		end.Add(time.Minute).Format(time.RFC3339), "--cut", "entry-start", "--out", cut, "--json"); exit != 0 {
		t.Fatalf("check --cut: exit %d", exit)
	}
	manifest := filepath.Join(cut, "manifest.json")
	base = baseline{peers, peers + " fitted outside from a cut's manifest"}
	if fits[base] = outsideFit(t, out, "manifest", "--manifest", manifest); !bytes.Contains(fits[base], []byte(manifest)) {
		t.Fatalf("the fit made from a manifest does not record it")
	}
	baselines = append(baselines, base)
	base = baseline{peers, peers + " fitted outside from a cut's manifest with --name"} // DEPLOY_AWS.md 14's form
	fits[base] = outsideFit(t, out, "manifest-named", "--manifest", manifest, "--name=staging")
	baselines = append(baselines, base)
	// a record stored by an engine without a rule of the current one (the task id of an ECS task ARN): the current
	// engine's record with what that engine wrote in place of what this one shows, as the step outside the server
	// stores it (DEPLOY_AWS.md 14); the server shows a stored record through the same rules, whoever made it
	for _, c := range table {
		var was []string
		for _, p := range c.Planted {
			if p.Was != "" && !slices.Contains(was, p.Shown) {
				was = append(was, p.Shown, p.Was)
			}
		}
		if len(was) == 0 {
			continue
		}
		named := fits[baseline{c.Name, c.Name + " fitted outside with --name"}]
		foreign := []byte(strings.NewReplacer(was...).Replace(string(named)))
		for i := 1; i < len(was); i += 2 {
			if !bytes.Contains(foreign, []byte(was[i])) {
				t.Fatalf("%s: the record does not hold %q, where an older engine wrote %q", c.Name, was[i-1], was[i])
			}
		}
		base := baseline{c.Name, c.Name + " as an engine without its rules wrote it, stored as the baseline"}
		fits[base] = foreign
		baselines = append(baselines, base)
	}
	forbidden = append(forbidden, canary{folderCanary, "the folder of a fit made outside"},
		canary{manifestCanary, "the folder of a manifest fitted outside"})
	// fit records shaped by hand, stored as the baseline (canary_records_test.go): every string field planted, as
	// tokens and as ARNs, which the engine refuses as a record its version did not write; and only the names its
	// checks cannot judge planted, which it reads and shows through the name rule. A planter is used by one goroutine
	// at a time, and its canaries join the forbidden list once every sweep is done.
	planters := []*planter{{}, {arn: true}, {}, {}, {}}
	plantedFolder := filepath.Join(root, "captures")
	peersFit := fits[baseline{peers, peers + " fitted outside with --name"}]
	var staged []byte // a baseline whose model has a stage node, the calls a service makes at once
	for _, c := range table {
		if b := fits[baseline{c.Name, c.Name + " fitted outside with --name"}]; staged == nil &&
			bytes.Contains(b, []byte(`"stage":true`)) {
			staged = b
		}
	}
	if staged == nil {
		t.Fatalf("no canary capture's fit has a stage node")
	}
	type plantedFit struct {
		base baseline
		read bool
	}
	var plantedFits []plantedFit
	for _, v := range []struct {
		form string
		rec  []byte
		read bool
	}{
		{"a fit record with every field planted as tokens", plantFit(t, peersFit, planters[0], plantedFolder), false},
		{"a fit record with every field planted as ARNs", plantFit(t, peersFit, planters[1], plantedFolder), false},
		// a rooted path that names the server's folder (the real one may pass the 128 characters of a name)
		{"a fit record with its names planted", plantNames(t, peersFit, planters[2], "/srv/"+serverCanary+"/captures"), true},
		// a capture recorded as an AWS resource name whose last element holds its account, alone and after a folder
		// of the server: shown through the name rule of the whole string, never as its last path element
		{"a fit record whose capture is an ARN that holds its account", plantCapture(t, peersFit, accountARNs()[0]), true},
		{"a fit record whose capture is a folder's ARN", plantCapture(t, peersFit,
			"/srv/"+serverCanary+"/captures/"+accountARNs()[2]), true},
		// a number whose name no code has (one that holds an account): not a record this engine wrote, so not read
		{"a fit record with a number's name planted", plantNumber(t, peersFit, planters[3]), false},
		// the engine's record re-serialized by another JSON tool (a newline after its first byte): not the bytes the
		// engine wrote, so not read, as every reader of the record agrees on its digest only for those bytes
		{"a fit record re-serialized", append([]byte{peersFit[0]}, append([]byte("\n "), peersFit[1:]...)...), false},
		// a node its mirror names that is not one of its rows, one field each: not a record this engine wrote
		{"a fit record whose varying call names a callee that is not a row", plantNode(t, peersFit, planters[4],
			nodeCallee), false},
		{"a fit record whose call shape names a node that is not a row", plantNode(t, peersFit, planters[4], nodeShape),
			false},
		{"a fit record whose model names a node apart from its row", plantNode(t, peersFit, planters[4], nodeModel), false},
		{"a fit record whose stage node names a caller that is not a row", plantNode(t, staged, planters[4], nodeStage),
			false},
	} {
		base := baseline{peers, v.form + ", stored as the baseline"}
		fits[base] = v.rec
		plantedFits = append(plantedFits, plantedFit{base, v.read})
	}

	// The sweeps run a few at a time, each in a subtest with a state store of its own whose environment staging has
	// the baseline given (stored as the step outside the server stores it), and its own servers.
	var (
		mu    sync.Mutex
		ws    []*sweeper
		wg    sync.WaitGroup
		slots = make(chan struct{}, 4)
	)
	sweep := func(name string, b baseline, run func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			t.Run(name, func(t *testing.T) {
				store := state.NewMem(func() time.Time { return now })
				d, err := store.PutArtifact(context.Background(), "fit", fits[b])
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetBaseline(context.Background(), "staging", d); err != nil {
					t.Fatal(err)
				}
				got := run(t, func(profile, token string) *sweeper {
					t.Helper()
					return sweepServerOf(t, root, captures, profile, store, now, token, revisions[0])
				})
				mu.Lock()
				ws = append(ws, got...)
				mu.Unlock()
			})
		}()
	}
	abArgs := `{"ab":{"baseline":"` + pair + `-a","candidate":"` + pair + `-b"}}`

	// each baseline in both profiles, both transports and both revisions (two clients of one server)
	for _, base := range baselines {
		sweep(base.form, base, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
			var ws []*sweeper
			for _, profile := range []string{config.ProfileDefault, config.ProfileBench} {
				for _, token := range []string{"", "read-token"} {
					for _, w := range bothRevisions(t, server(profile, token), "baseline "+base.form) {
						fit, _ := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
						if fit == "" {
							t.Fatalf("%s: no fit handle", w.name)
						}
						w.views(fit, "")
						w.tool("context", `{"env":"staging","detail":"full"}`)
						w.tool("predict", `{"env":"staging","changes":[{"op":"own_ms","service":"c","value":0.4}]}`)
						if token != "" {
							w.tool("fit_start", abArgs) // HTTP 403: no fit scope
						}
						ws = append(ws, w)
					}
				}
			}
			return ws
		})
	}

	// each fit record shaped by hand, in both profiles and both transports: refused with fit.version, or read and swept
	// (checked after the sweep, so that a record read where it should be refused shows what it leaks). Every read
	// surface is called on it whatever context with env answered: predict with env, whose prediction's documents are
	// read when it makes one, and the surfaces on handles the store mints on the record (plantedSurfaces), each of which
	// must refuse a record the engine refuses with fit.version
	var unexpected []string
	for _, pf := range plantedFits {
		sweep(pf.base.form, pf.base, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
			var ws []*sweeper
			for _, profile := range []string{config.ProfileDefault, config.ProfileBench} {
				for _, token := range []string{"", "read-token"} {
					w := server(profile, token)
					w.name += " " + revisions[0] + ", baseline " + pf.base.form
					var bad []string
					got := w.tool("context", `{"env":"staging"}`)
					fit, _ := got["fit_id"].(string)
					if pf.read && fit == "" || !pf.read && errorCode(got) != codeFitVersion {
						bad = append(bad, fmt.Sprintf("context env: read %v, want %v: %.300v", fit != "", pf.read, got))
					}
					if fit != "" {
						w.lenientViews(fit, "")
					}
					w.tool("context", `{"env":"staging","detail":"full"}`)
					got = w.tool("predict", `{"env":"staging","changes":[{"op":"own_ms","service":"c","value":0.4}]}`)
					pred, _ := got["prediction_id"].(string)
					if pf.read && pred == "" || !pf.read && errorCode(got) != codeFitVersion {
						bad = append(bad, fmt.Sprintf("predict env: read %v, want %v: %.300v", pred != "", pf.read, got))
					}
					if pred != "" {
						w.read(predictionURI + pred)
						w.read(packetURI + pred)
						w.prompt(promptReview, map[string]string{"prediction_id": pred})
						w.tool("explain", `{"target":"`+pred+`","section":"summary","max_chars":8000}`)
					}
					bad = append(bad, w.plantedSurfaces(peersFit, !pf.read)...)
					mu.Lock()
					for _, b := range bad {
						unexpected = append(unexpected, w.name+": "+b)
					}
					mu.Unlock()
					ws = append(ws, w)
				}
			}
			return ws
		})
	}

	// fits made by the server, in stdio mode: fit_start and job_status (default profile), context{capture} (bench),
	// the refused capture, and the A/B pair with an A/B job that refuses; and over HTTP with a fit token, the A/B
	// surfaces and the refused A/B job. The A/B surfaces work on the environment's baseline, the fit of a capture
	// that plants no peer, so that an ext: node they carry comes from the A/B record.
	plainBase := baseline{plain, plain + " fitted outside with --name"}
	refusedAB := `{"ab":{"baseline":"` + refused + `","candidate":"` + plain + `"}}`
	onAB := pair + " (an A/B record) on the baseline " + plainBase.form + ": "
	sweep("fits made by the server, stdio", plainBase, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
		ws := bothRevisions(t, server(config.ProfileDefault, ""), "fits made by the server")
		for _, w := range ws {
			for _, c := range table {
				w.about = c.Name + ": "
				switch {
				case c.Pair:
					w.about = onAB
					ab := w.job(abArgs, "completed")["ab_id"].(string)
					rec, err := w.store.GetArtifact(context.Background(), w.digest(ab))
					if err != nil {
						t.Fatal(err)
					}
					mu.Lock()
					held[c.Name] = rec
					mu.Unlock()
					fit, _ := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
					w.views(fit, ab)
				case c.Refused:
					// a fix that points at the engine's message, which the server withholds, is replaced (messageFixes)
					if st := w.job(`{"capture":"`+c.Name+`"}`, "failed"); refusalOf(st) == "" ||
						st["refusal"].(map[string]any)["fix"] == fixAsMessage {
						t.Errorf("%s: %s: %v", w.name, c.Name, st)
					}
					w.tool("context", `{"capture":"`+c.Name+`"}`)
					w.job(refusedAB, "failed")
				default:
					st := w.job(`{"capture":"`+c.Name+`"}`, "completed")
					w.views(st["fit_id"].(string), "")
				}
			}
		}
		return ws
	})
	sweep("fits made by the server, bench", plainBase, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
		ws := bothRevisions(t, server(config.ProfileBench, ""), "fits made by the server")
		for _, w := range ws {
			for _, c := range table {
				if c.Pair {
					continue
				}
				w.about = c.Name + ": "
				fit, _ := w.tool("context", `{"capture":"`+c.Name+`"}`)["fit_id"].(string)
				if fit != "" {
					w.views(fit, "")
				} else if !c.Refused {
					t.Fatalf("%s: context{capture:%s} made no fit", w.name, c.Name)
				}
			}
		}
		return ws
	})
	sweep("AB over HTTP with a fit token", plainBase, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
		ws := bothRevisions(t, server(config.ProfileDefault, "fit-token"), "A/B made over HTTP with a fit token")
		for _, w := range ws {
			w.about = onAB
			ab := w.job(abArgs, "completed")["ab_id"].(string)
			fit, _ := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
			w.views(fit, ab)
			w.job(refusedAB, "failed")
			w.tool("fit_start", `{"capture":"`+plain+`"}`) // the capture argument outside stdio mode: a protocol error
		}
		return ws
	})
	// A/B records shaped by hand, every field planted as tokens and as ARNs, stored as the server and the engine store
	// one and minted for the caller, and one with only the names its checks cannot judge planted, which it reads; job
	// records with a planted refusal and findings; and prediction records as another build of the server may have
	// stored them: a policy name that is a path naming the server's folder, and a clock that is not RFC 3339
	abPlanters := []*planter{{}, {arn: true}, {}, {}}
	sweep("A/B and job records shaped by hand, stdio", plainBase, func(t *testing.T, server func(profile, token string) *sweeper) []*sweeper {
		w := server(config.ProfileDefault, "")
		w.name += " " + revisions[0] + ", A/B and job records shaped by hand"
		w.about = onAB
		ab := w.job(abArgs, "completed")["ab_id"].(string)
		rec, err := w.store.GetArtifact(context.Background(), w.digest(ab))
		if err != nil {
			t.Fatal(err)
		}
		fit, _ := w.tool("context", `{"env":"staging"}`)["fit_id"].(string)
		if fit == "" {
			t.Fatalf("%s: no fit handle", w.name)
		}
		for _, p := range abPlanters[:2] {
			w.lenientViews(fit, storeAB(t, w, root, plantAB(t, rec, p, plantedFolder)))
		}
		w.about = onAB + "names planted: "
		w.lenientViews(fit, storeAB(t, w, root, plantABNames(t, rec, abPlanters[3], "/srv/"+serverCanary+"/captures")))
		w.about = "job records shaped by hand: "
		for _, id := range plantJobs(t, w.store, w.who, now, abPlanters[2], plantedFolder) {
			w.tool("job_status", `{"job_id":"`+id+`"}`)
		}
		w.about = "prediction records shaped by hand: "
		pred, _ := w.tool("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)["prediction_id"].(string)
		if pred == "" {
			t.Fatalf("%s: predict minted no prediction", w.name)
		}
		made, err := w.store.GetArtifact(context.Background(), w.digest(pred))
		if err != nil {
			t.Fatal(err)
		}
		for _, edit := range []func(m map[string]any){
			func(m map[string]any) { m["policy_name"] = "/srv/" + serverCanary + "/policies/plnt-policyname" },
			func(m map[string]any) { m["now"] = abPlanters[2].at("prednow", "a prediction record's clock") },
		} {
			m := decodeRecord(t, made)
			edit(m)
			d, err := w.store.PutArtifact(context.Background(), "prediction", encodeRecord(t, m))
			if err != nil {
				t.Fatal(err)
			}
			h, err := w.store.Mint(context.Background(), w.who, state.KindPrediction, d, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for _, sec := range []string{"summary", "rules", "judge_packet"} {
				w.tool("explain", `{"target":"`+h+`","section":"`+sec+`","max_chars":8000}`)
			}
			w.read(predictionURI + h)
			w.read(packetURI + h)
			w.prompt(promptReview, map[string]string{"prediction_id": h})
		}
		return []*sweeper{w}
	})
	// job records in a Dir store, rewritten after the server or PutJob wrote them, since a file may have been left by
	// another build of the server or its worker: job_status on each, and fit_start repeating the request of a job
	// whose file holds another id (the store finds the job by the request's key)
	jobPlanter := &planter{}
	sweep("job records rewritten in a Dir store, stdio", plainBase, func(t *testing.T, _ func(profile, token string) *sweeper) []*sweeper {
		ctx := context.Background()
		dir := filepath.Join(root, "state", "jobs-dir")
		store, err := state.NewDir(dir, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		d, err := store.PutArtifact(ctx, "fit", fits[plainBase])
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetBaseline(ctx, "staging", d); err != nil {
			t.Fatal(err)
		}
		w := sweepServerOf(t, root, captures, config.ProfileDefault, store, now, "", revisions[0])
		w.name += " " + revisions[0] + ", job records rewritten in a Dir store"
		w.about = "job records rewritten in a Dir store: "
		args := `{"capture":"` + plain + `"}`
		w.job(args, "completed")
		keys, err := filepath.Glob(filepath.Join(dir, "principals", "*", "jobkeys", "*"))
		if err != nil || len(keys) != 1 {
			t.Fatalf("%d job keys in the store (%v)", len(keys), err)
		}
		id, err := os.ReadFile(keys[0])
		if err != nil {
			t.Fatal(err)
		}
		editJobFile(t, dir, string(id), func(m map[string]any) { m["id"] = "job_" + jobPlanter.at("jobkeyid", "a job's id") })
		w.tool("fit_start", args)
		for _, id := range plantJobFiles(t, store, dir, w.who, now, jobPlanter) {
			w.tool("job_status", `{"job_id":"`+id+`"}`)
		}
		return []*sweeper{w}
	})
	wg.Wait()
	for _, p := range append(append(planters, abPlanters...), jobPlanter) {
		forbidden = append(forbidden, p.forbidden()...)
	}
	if t.Failed() {
		t.FailNow()
	}

	// each canary is where the engine's output, or the traces, hold it
	for _, c := range table {
		for _, p := range c.Planted {
			if p.Kept && !bytes.Contains(held[c.Name], []byte(p.Value)) {
				t.Errorf("%s: the engine's output does not hold %q (%s): the sweep would not see it leak", c.Name, p.Value, p.Key)
			}
			if !p.Kept && !bytes.Contains(traceBytes(t, filepath.Join(root, "captures", c.Name)), []byte(p.Value)) {
				t.Errorf("%s: the traces do not hold %q (%s)", c.Name, p.Value, p.Key)
			}
			if p.Shown != "" && !bytes.Contains(held[c.Name], []byte(p.Shown)) {
				t.Errorf("%s: the engine's output does not hold %q where %q would be (%s)", c.Name, p.Shown, p.Value, p.Key)
			}
		}
	}

	// the server's copy of the name rule (served) gives the CLI's values: each peer, and each service the name rule
	// hashes, that explain section services shows on a fit is in the report that rhobound://prediction serves for a
	// prediction on it, by the same name
	pinned := 0
	for _, w := range ws {
		var shown []string
		for _, r := range w.replies() {
			switch {
			case strings.Contains(r.what, `tools/call explain {"target":"fit_`) && strings.Contains(r.what, `"section":"services"`):
				if a := aliasRE.FindAllString(string(r.body), -1); len(a) > 0 { // not an unknown handle's refusal
					shown = a
				}
			case strings.Contains(r.what, "resources/read "+predictionURI+"pred_") && len(shown) > 0:
				var missing []string
				for _, a := range shown {
					if !bytes.Contains(r.body, []byte(a)) && !slices.Contains(missing, a) {
						missing = append(missing, a)
					}
				}
				if len(missing) > 0 {
					t.Errorf("%s: %s: explain shows the nodes %s, which the prediction resource does not name", w.name, r.what,
						strings.Join(missing, ", "))
				}
				shown, pinned = nil, pinned+1
			}
		}
	}
	if pinned == 0 {
		t.Errorf("no fit with a peer was explained and its prediction read")
	}

	// the sweep: each surface, by what it was asked about, with the canaries its replies carry, where in them, and the
	// servers that sent them
	type finding struct {
		canaries, paths, servers map[string]bool
		example                  string
	}
	found := map[string]*finding{}
	requests, distinct, fewest, most := 0, map[string]bool{}, 0, 0
	for _, w := range ws {
		one := map[string]bool{}
		for _, r := range w.replies() {
			requests++
			what := handleRE.ReplaceAllString(r.what, "${1}_*")
			one[strings.TrimPrefix(what, w.about)] = true
			distinct[what] = true
			if bytes.Contains(r.body, []byte(`"code":-32603`)) {
				t.Errorf("%s: %s: an internal error: %s", w.name, r.what, r.body)
			}
			hits := leaks(r.body, forbidden)
			if len(hits) == 0 {
				continue
			}
			base := w.name[strings.Index(w.name, ", ")+2:]
			k := base + ": " + what
			f := found[k]
			if f == nil {
				f = &finding{canaries: map[string]bool{}, paths: map[string]bool{}, servers: map[string]bool{}}
				found[k] = f
				i := bytes.Index(r.body, []byte(hits[0].value))
				if hits[0].value == extPrefix {
					i = extNode.FindIndex(r.body)[0]
				}
				f.example = string(r.body[max(0, i-160):min(len(r.body), i+120)])
			}
			for _, h := range hits {
				f.canaries[h.value+" ("+h.what+")"] = true
			}
			for _, p := range leakPaths(r.body, forbidden) {
				f.paths[p] = true
			}
			f.servers[strings.TrimSuffix(w.name, ", "+base)] = true
		}
		if fewest == 0 || len(one) < fewest {
			fewest = len(one)
		}
		most = max(most, len(one))
	}
	for _, k := range sortedKeys(found) {
		f := found[k]
		t.Errorf("%s\n  carries %s\n  at %s\n  from %s\n  e.g. ...%s...", k, strings.Join(sortedKeys(f.canaries), ", "),
			strings.Join(sortedKeys(f.paths), ", "), strings.Join(sortedKeys(f.servers), "; "), f.example)
	}
	for _, u := range unexpected {
		t.Errorf("a fit record shaped by hand is not read, or not refused with fit.version, as it should be on a surface: %s",
			u)
	}
	t.Logf("%d clients, %d requests (%d distinct, %d to %d on one client); %d strings and the ext: prefix looked for in each reply; "+
		"%d prediction resources checked against explain's nodes", len(ws), requests, len(distinct), fewest, most, len(forbidden), pinned)
}

// The folders this test lays out, whose names no reply may carry: the server's root (its captures, policies,
// environment root, state, work folder and engine), and the parents of a baseline fitted outside the server from a
// folder and from a cut's manifest, without --name.
const (
	serverCanary   = "rb-canary-srv-0e5d"
	folderCanary   = "rb-canary-dir-91f4"
	manifestCanary = "rb-canary-cut-3c7a"
)

// canary is a string no reply may carry, and where it comes from.
type canary struct{ value, what string }

// arnCanary starts an AWS ARN (arn:partition:service:region:account:resource), which names an account, and no reply
// may carry one: the name rule shows an ARN as its last element.
const arnCanary = "arn:aws"

// extPrefix stands, in a list of leaks, for the model's prefix of a peer node, ext:<peer> (extNode).
const extPrefix = "ext:"

// extNode matches the model's prefix of a peer node, not the end of a word such as "context:"; a JSON escape (\n)
// before it counts as a boundary.
var extNode = regexp.MustCompile(`(^|[^A-Za-z0-9_]|\\[nrt])ext:`)

// aliasRE matches a peer as the views show it (external_<hash8>).
var aliasRE = regexp.MustCompile(`(external|svc)_[0-9a-f]{8}`)

// handleRE matches a handle, so that the same surface on two servers reads the same.
var handleRE = regexp.MustCompile(`\b(fit|pred|ab|job)_[0-9A-Z]{26}\b`)

// leaks returns the canaries a reply carries.
func leaks(b []byte, forbidden []canary) []canary {
	var out []canary
	for _, c := range forbidden {
		if bytes.Contains(b, []byte(c.value)) {
			out = append(out, c)
		}
	}
	if extNode.Match(b) {
		out = append(out, canary{extPrefix, "the model's peer node prefix"})
	}
	return out
}

// leakPaths lists where, in the JSON messages of a reply, the strings that carry a canary are: the path to each,
// array indices dropped; a string that holds a JSON document (a resource's text, or a tool's text block before its
// scope sentence) is read as one, and the path goes on after "->".
func leakPaths(body []byte, forbidden []canary) []string {
	seen := map[string]bool{}
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				if len(leaks([]byte(k), forbidden)) > 0 {
					// a hit in an object's key: its path, marked as a key's
					seen[strings.ReplaceAll(strings.TrimPrefix(path+"."+k, "."), "->.", "->")+" (the key)"] = true
				}
				walk(path+"."+k, e)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		case string:
			var doc any
			if strings.HasPrefix(x, "{") && json.NewDecoder(strings.NewReader(x)).Decode(&doc) == nil {
				walk(path+"->", doc)
			} else if len(leaks([]byte(x), forbidden)) > 0 {
				seen[strings.ReplaceAll(strings.TrimPrefix(path, "."), "->.", "->")] = true
			}
		}
	}
	for _, line := range bytes.Split(body, []byte("\n")) {
		for _, prefix := range []string{"read: ", "data: "} {
			if i := bytes.Index(line, []byte(prefix)); i >= 0 {
				var v any
				if json.Unmarshal(line[i+len(prefix):], &v) == nil {
					walk("", v)
				}
			}
		}
	}
	return sortedKeys(seen)
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// canaryCapture is one capture of the canary table (gencontract canaries) and what it planted.
type canaryCapture struct {
	Name    string          `json:"name"`
	Pair    bool            `json:"pair"`
	Refused bool            `json:"refused"`
	Planted []canaryPlanted `json:"planted"`
}

// canaryPlanted is one value a canary capture planted (testgen.Planted).
type canaryPlanted struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Class string `json:"class"`
	Kept  bool   `json:"kept"`
	Shown string `json:"shown"`
	Was   string `json:"was"`
}

// revisions are the protocol revisions the SDK client speaks, the newest first.
var revisions = []string{"2026-07-28", "2025-11-25"}

// bothRevisions returns w, a client of a server at revisions[0], and a second client of the same server at
// revisions[1], each named with the revision it negotiated (which must be the one it asked for) and with what it is
// used for.
func bothRevisions(t *testing.T, w *sweeper, what string) []*sweeper {
	t.Helper()
	ws := []*sweeper{w, w.again(revisions[1])}
	for i, w := range ws {
		if got := w.cs.InitializeResult().ProtocolVersion; got != revisions[i] {
			t.Fatalf("%s: negotiated %q, want %q", w.name, got, revisions[i])
		}
		w.name += " " + revisions[i] + ", " + what
	}
	return ws
}

// canaryCaptures writes the canary captures into dir (go run ./tools/gencontract canaries DIR: this module cannot
// import testgen) and returns their table and their registered names, relative to dir's parent: a capture by its
// name, an A/B pair's arms by its name and -a, -b.
func canaryCaptures(t *testing.T, dir string) ([]canaryCapture, map[string]string) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	cmd := exec.Command(goBin, "run", "./tools/gencontract", "canaries", dir)
	cmd.Dir = real.root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gencontract canaries: %v\n%s", err, b)
	}
	b, err := os.ReadFile(filepath.Join(dir, "canaries.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table []canaryCapture
	if err := json.Unmarshal(b, &table); err != nil || len(table) == 0 {
		t.Fatalf("the canary table: %v", err)
	}
	rel := filepath.Base(dir)
	captures := map[string]string{}
	for _, c := range table {
		if c.Pair {
			captures[c.Name+"-a"] = rel + "/" + c.Name + "/baseline"
			captures[c.Name+"-b"] = rel + "/" + c.Name + "/candidate"
		} else {
			captures[c.Name] = rel + "/" + c.Name
		}
	}
	return table, captures
}

// layOutCanaries puts the rest of a server's locations under root, beside its captures: two policies, an environment
// root, the engine, and the state, work and engine-state folders.
func layOutCanaries(t *testing.T, root string) {
	t.Helper()
	for _, sub := range []string{"policies", "env", "state", "work", "engine-state", "engine"} {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, doc := range map[string]string{"default": `{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":5}`,
		"strict": `{"schema":"rhobound.policy/v1","max_entry_mean_change_pct":2}`} {
		if err := os.WriteFile(filepath.Join(root, "policies", name+".json"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(t, real.bin, filepath.Join(root, "engine", "rhobound"))
	if err := os.Chmod(filepath.Join(root, "engine", "rhobound"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// outsideCLI runs the real CLI as a step outside the server does, with its own record store under out, and returns
// what it printed and its exit status.
func outsideCLI(t *testing.T, out string, args ...string) ([]byte, int) {
	t.Helper()
	cmd := exec.Command(real.bin, args...)
	cmd.Env = append(os.Environ(), "RHOBOUND_ESTIMATOR=", "RHOBOUND_STATE="+filepath.Join(out, "state"))
	b, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Fatalf("rhobound %s: %v", strings.Join(args, " "), err)
	}
	return b, cmd.ProcessState.ExitCode()
}

// outsideFit fits outside the server (fit ARGS --out OUT/NAME --json) and returns the record.
func outsideFit(t *testing.T, out, name string, args ...string) []byte {
	t.Helper()
	dir := filepath.Join(out, name)
	if b, exit := outsideCLI(t, out, append(append([]string{"fit"}, args...), "--out", dir, "--json")...); exit != 0 {
		t.Fatalf("rhobound fit %s: exit %d: %s", strings.Join(args, " "), exit, b)
	}
	b, err := os.ReadFile(filepath.Join(dir, "fit.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// traceBytes is every trace file under a capture folder, decompressed.
func traceBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var all []byte
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.Contains(p, string(filepath.Separator)+"traces"+string(filepath.Separator)) {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".gz") {
			zr, err := gzip.NewReader(bytes.NewReader(b))
			if err != nil {
				return err
			}
			if b, err = io.ReadAll(zr); err != nil {
				return err
			}
		}
		all = append(all, b...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return all
}
