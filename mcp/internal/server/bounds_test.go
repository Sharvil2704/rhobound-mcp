// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// blockFits makes every fit wait until the test ends (or its job's deadline), and returns how many fits have started.
func blockFits(t *testing.T, h *harness) func() int {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) }) // runs before the harness waits for its jobs
	h.cli.mu.Lock()
	h.cli.block, h.cli.release = true, release
	h.cli.mu.Unlock()
	return func() int {
		n := 0
		for _, c := range h.fake.Calls() {
			if c.Sub == "fit" {
				n++
			}
		}
		return n
	}
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("waited five seconds for %s", what)
}

// TestJobsLeaveEngineSlots: fit jobs take at most MaxEngines-2 engine slots, whoever starts them, so two principals
// each running their most fits at once still leave context, budget and predict an engine at once.
func TestJobsLeaveEngineSlots(t *testing.T) {
	h := newHarness(t, harnessOpt{realTime: true, jobTimeout: time.Minute, callTimeout: 5 * time.Second})
	if e := h.srv.opt.Runner.(*engineSlots); cap(e.slots) != DefaultMaxEngines || cap(e.jobs) != DefaultMaxEngines-2 {
		t.Fatalf("slots %d, job slots %d", cap(e.slots), cap(e.jobs))
	}
	fit := h.startFit(`{"capture":"loop"}`)
	fits := blockFits(t, h)
	before := fits()
	var jobs []string
	for _, who := range []string{"local:alice", "local:bob"} {
		*h.who = who
		for w := 1; w <= 2; w++ { // ratelimit.Default: two fits at once per principal
			b, isErr := h.call("fit_start", fmt.Sprintf(`{"capture":"loop","workers":{"a":%d}}`, w))
			if isErr {
				t.Fatalf("%s fit_start %d: %s", who, w, b)
			}
			jobs = append(jobs, field(t, b, "job_id").(string))
		}
	}
	// two fits hold the two job slots; the other two jobs wait for one
	waitFor(t, "two fits to start", func() bool { return fits()-before == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := fits() - before; n != 2 {
		t.Fatalf("%d fits run at once, over the job slots", n)
	}
	*h.who = "local:alice"
	start := time.Now()
	for _, c := range []struct{ tool, args string }{
		{"context", `{"fit_id":"` + fit + `"}`},
		{"budget", `{"fit_id":"` + fit + `","service":"b"}`},
		{"predict", `{"fit_id":"` + fit + `","changes":[{"op":"own_ms","service":"b","value":0.4}]}`},
	} {
		if b, isErr, err := h.try(c.tool, c.args); err != nil || isErr {
			t.Errorf("%s while the jobs hold their slots: %s %v", c.tool, b, err)
		}
	}
	// a call that waited for an engine would wait for its CallTimeout and fail; the margin is for a slow runner
	if el := time.Since(start); el > 2500*time.Millisecond {
		t.Errorf("the calls waited %v for an engine", el)
	}
	for i, id := range jobs {
		*h.who = []string{"local:alice", "local:alice", "local:bob", "local:bob"}[i]
		if b, _ := h.call("job_status", `{"job_id":"`+id+`"}`); field(t, b, "state") != state.JobWorking {
			t.Errorf("job %d: %s", i, b)
		}
	}
}

// TestLongPollCap: a principal holds at most MaxPolls long-polls at once; over that, job_status answers at once, still
// working, with poll_after_ms, and the count goes back to zero when the polls end.
func TestLongPollCap(t *testing.T) {
	h := newHarness(t, harnessOpt{realTime: true, jobTimeout: time.Minute, maxPolls: 1})
	blockFits(t, h)
	b, _ := h.call("fit_start", `{"capture":"loop"}`)
	job := field(t, b, "job_id").(string)
	other := connect(t, h.srv, "") // a second session of the same principal
	poll := &mcp.CallToolParams{Name: "job_status", Arguments: map[string]any{"job_id": job, "wait_s": 4}}
	first := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_, _ = h.cs.CallTool(context.Background(), poll)
		first <- time.Since(start)
	}()
	open := func() int {
		h.srv.pollsMu.Lock()
		defer h.srv.pollsMu.Unlock()
		return h.srv.polls["local:alice"]
	}
	waitFor(t, "the first long-poll", func() bool { return open() == 1 })
	start := time.Now()
	res, err := other.CallTool(context.Background(), poll)
	if el := time.Since(start); err != nil || res.IsError || el > 2*time.Second {
		t.Fatalf("the second long-poll: %v %v after %v", res, err, el)
	}
	sc := res.StructuredContent.(map[string]any)
	if sc["state"] != state.JobWorking || sc["poll_after_ms"] == nil {
		t.Errorf("the second long-poll: %v", sc)
	}
	if el := <-first; el < 3*time.Second {
		t.Errorf("the first long-poll returned after %v, before its wait", el)
	}
	if n := open(); n != 0 {
		t.Errorf("%d long-polls counted after both ended", n)
	}
}

// TestDerivedChainShareOneDeadline: a document derived from a stored prediction takes all its engine calls within one
// CallTimeout. Here the report and the section each take 2 s against a 3 s timeout: the first explain runs out of
// time in the section, and the report it computed is kept, so the second fits. The margins are a second each way.
func TestDerivedChainShareOneDeadline(t *testing.T) {
	h := newHarness(t, harnessOpt{realTime: true, callTimeout: 3 * time.Second})
	fit := h.startFit(`{"capture":"loop"}`)
	b, isErr := h.call("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)
	if isErr {
		t.Fatal(string(b))
	}
	pred := field(t, b, "prediction_id").(string)
	h.cli.mu.Lock()
	h.cli.delay = map[string]time.Duration{"predict": 2 * time.Second, "explain": 2 * time.Second}
	h.cli.mu.Unlock()
	args := `{"target":"` + pred + `","section":"rules"}`
	start := time.Now()
	if _, _, err := h.try("explain", args); protocolCode(err) != jsonrpc.CodeInternalError {
		t.Errorf("the chain past its deadline: %v", err)
	}
	if el := time.Since(start); el > 4500*time.Millisecond {
		t.Errorf("the chain took %v, over one CallTimeout", el)
	}
	if b, isErr, err := h.try("explain", args); err != nil || isErr {
		t.Errorf("the section alone: %s %v", b, err)
	}
	reports := 0
	for _, c := range h.fake.Calls() {
		if c.Sub == "predict" {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("the report was computed %d times", reports)
	}
}

// TestDerivedIsBounded: the server remembers at most MaxDerived derived documents, the least recently used forgotten
// first; a forgotten one is computed again.
func TestDerivedIsBounded(t *testing.T) {
	c := newLRU(2)
	c.put("a", "1")
	c.put("b", "2")
	c.get("a")
	c.put("c", "3")
	if _, ok := c.get("b"); ok || c.len() != 2 {
		t.Errorf("the least recently used stays: %d", c.len())
	}
	if v, ok := c.get("a"); !ok || v != "1" {
		t.Error("a was forgotten")
	}
	if vals := c.values(); len(vals) != 2 || !vals["1"] || !vals["3"] {
		t.Errorf("values %v", vals)
	}

	h := newHarness(t, harnessOpt{maxDerived: 2})
	fit := h.startFit(`{"capture":"loop"}`)
	b, _ := h.call("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)
	pred := field(t, b, "prediction_id").(string)
	for _, sec := range []string{"rules", "summary", "services", "rules"} {
		if b, isErr := h.call("explain", `{"target":"`+pred+`","section":"`+sec+`"}`); isErr {
			t.Fatalf("%s: %s", sec, b)
		}
		if n := h.srv.derived.len(); n > 2 {
			t.Fatalf("%d derived documents remembered", n)
		}
	}
	explains := 0
	for _, c := range h.fake.Calls() {
		if c.Sub == "explain" {
			explains++
		}
	}
	if explains != 4 { // by the second rules, rules was forgotten: computed again
		t.Errorf("%d explain calls", explains)
	}
}

// TestServerSweep: Sweep runs the store's sweep with the longest handle lifetime as grace and the derived documents
// kept; expired handles and old jobs go, a live prediction's documents stay. A store that cannot sweep is left alone.
func TestServerSweep(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	fit := h.startFit(`{"capture":"loop"}`)
	b, _ := h.call("predict", `{"fit_id":"`+fit+`","changes":[{"op":"own_ms","service":"b","value":0.4}]}`)
	pred := field(t, b, "prediction_id").(string)
	if b, isErr := h.call("explain", `{"target":"`+pred+`","section":"rules"}`); isErr {
		t.Fatal(string(b))
	}
	if n, err := h.srv.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("nothing has expired: %d %v", n, err)
	}
	h.clock.add(DefaultFitTTL + state.ExpiredKeep)
	if n, err := h.srv.Sweep(context.Background()); err != nil || n != 2 {
		t.Errorf("%d removed, %v; want the fit handle and the job", n, err)
	}
	if b, isErr := h.call("explain", `{"target":"`+pred+`","section":"rules"}`); isErr {
		t.Errorf("a live prediction after the sweep: %s", b)
	}
	if b, isErr := h.call("context", `{"fit_id":"`+fit+`"}`); !isErr || refusalCode(t, b) != "handle.unknown" {
		t.Errorf("a swept fit handle: %s", b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.srv.RunSweeper(ctx, time.Millisecond) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done

	s, err := New(Options{Config: testConfig(t, "", ""), Runner: &runner.Fake{}, Store: noSweep{state.NewMem(nil)}})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.Sweep(context.Background()); n != 0 || err != nil {
		t.Errorf("a store without Sweep: %d %v", n, err)
	}
}

// noSweep is a store without Sweep.
type noSweep struct{ state.Store }
