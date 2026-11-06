// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// batchTopo is a caller A with parallel nodes that call B n[i] times at once; B has c workers (known unless lower), serves
// at rate rps and takes s ms. shares are the shares of A's visits that call each node.
func batchTopo(n []int, shares []float64, c int, lower bool, rps, s float64) (*model.Topology, []ServiceStats, []int, map[int]string) {
	topo := &model.Topology{Services: []model.Service{
		{Name: "A", Servers: 1, Workers: 1},
		{Name: "B", S: s, Servers: 1, Workers: c, Routing: model.Pooled},
	}}
	stats := []ServiceStats{{Name: "A", RateRPS: 100}, {Name: "B", RateRPS: rps, WorkersInferred: lower}}
	var nodes []int
	owner := map[int]string{}
	for i, k := range n {
		var calls []model.Call
		for j := 0; j < k; j++ {
			calls = append(calls, model.Call{Child: 1, P: 1})
		}
		topo.Services = append(topo.Services, model.Service{Name: fmt.Sprintf("stage:A:%d", i+1), Delay: true, Parallel: true, Calls: calls, Servers: 1, Workers: 1})
		stats = append(stats, ServiceStats{Name: fmt.Sprintf("stage:A:%d", i+1), RateRPS: 100 * shares[i], Stage: true})
		nodes = append(nodes, len(topo.Services)-1)
		owner[len(topo.Services)-1] = "A"
	}
	return topo, stats, nodes, owner
}

func batchWarned(topo *model.Topology, stats []ServiceStats, nodes []int, owner map[int]string) []string {
	var out []string
	batchWarnings(topo, stats, nodes, owner, func(int) bool { return false }, func(coded bool, f string, a ...any) {
		if !coded {
			panic("an unmodelled batch warning without its code")
		}
		out = append(out, fmt.Sprintf(f, a...))
	})
	return out
}

// A group the model takes as a batch (model.Config.Batch) is noted without the model.batch_arrivals code, and says how
// it is modelled; one it does not take stays coded.
func TestBatchWarningModelledIsANote(t *testing.T) {
	topo, stats, nodes, owner := batchTopo([]int{4}, []float64{0.5}, 1, false, 50, 10)
	type got struct {
		coded bool
		text  string
	}
	var out []got
	batchWarnings(topo, stats, nodes, owner, func(callee int) bool { return model.BatchModelled(topo, callee) },
		func(coded bool, f string, a ...any) { out = append(out, got{coded, fmt.Sprintf(f, a...)}) })
	if len(out) != 1 || out[0].coded || !strings.Contains(out[0].text, "an M^X/G/c queue") {
		t.Fatalf("warnings %+v, want one uncoded note of the batch model", out)
	}
	out = nil
	batchWarnings(topo, stats, nodes, owner, func(int) bool { return false },
		func(coded bool, f string, a ...any) { out = append(out, got{coded, fmt.Sprintf(f, a...)}) })
	if len(out) != 1 || !out[0].coded || !strings.Contains(out[0].text, "queue behind each other") {
		t.Fatalf("warnings %+v, want one coded warning", out)
	}
}

// Two shapes of a service that each call one callee four times at once are one warning, with their shares added.
func TestBatchWarningsOfOneCalleeAreOne(t *testing.T) {
	topo, stats, nodes, owner := batchTopo([]int{4, 4}, []float64{0.05, 0.03}, 1, false, 5, 10)
	w := batchWarned(topo, stats, nodes, owner)
	if len(w) != 1 || !strings.Contains(w[0], "A calls B 4 times at once (on 8% of its visits); B has 1 worker and is 5% busy") {
		t.Fatalf("warnings %q, want one for B, on 8%% of A's visits", w)
	}
	// a different size of group is a warning of its own
	topo, stats, nodes, owner = batchTopo([]int{4, 3}, []float64{0.05, 0.03}, 1, false, 5, 10)
	if w = batchWarned(topo, stats, nodes, owner); len(w) != 2 {
		t.Fatalf("warnings %q, want one per size of group", w)
	}
	// ... and shares that are small one by one but count together are not left out
	topo, stats, nodes, owner = batchTopo([]int{4, 4}, []float64{0.006, 0.006}, 1, false, 5, 10)
	if w = batchWarned(topo, stats, nodes, owner); len(w) != 1 {
		t.Fatalf("warnings %q, want one (1.2%% of the visits together)", w)
	}
	topo, stats, nodes, owner = batchTopo([]int{4}, []float64{0.006}, 1, false, 5, 10)
	if w = batchWarned(topo, stats, nodes, owner); len(w) != 0 {
		t.Fatalf("warnings %q, want none for 0.6%% of the visits", w)
	}
}

// A batch larger than the workers is warned of at any load. With the workers a lower bound (the spans start once a worker
// has the request, so a queue shows nothing), calls at once that were served one after another found no more workers than
// that: a batch of three or more is warned of at 3% load, a pair is not (it waits one service time at the most, and may have a
// second worker that was never needed).
func TestBatchWarnedOfAtLowLoadOnALowerBound(t *testing.T) {
	for _, c := range []struct {
		name  string
		n, w  int
		lower bool
		warns bool
	}{
		{"4 at once, 1 worker (known)", 4, 1, false, true},
		{"4 at once, at least 1 worker", 4, 1, true, true},
		{"3 at once, at least 1 worker", 3, 1, true, true},
		{"2 at once, at least 1 worker", 2, 1, true, false},
		{"2 at once, 1 worker (known)", 2, 1, false, true},
		{"4 at once, at least 4 workers", 4, 4, true, false},
		{"4 at once, 4 workers (known)", 4, 4, false, false},
		{"4 at once, 8 workers (known)", 4, 8, false, false},
	} {
		topo, stats, nodes, owner := batchTopo([]int{c.n}, []float64{1}, c.w, c.lower, 6, 5) // 3% busy per worker at one
		stats[1].WorkersInferred = c.lower
		w := batchWarned(topo, stats, nodes, owner)
		if (len(w) == 1) != c.warns {
			t.Errorf("%s: warnings %q, want a warning: %v", c.name, w, c.warns)
		}
		if c.warns && c.lower && !strings.Contains(w[0], "has at least 1 worker") {
			t.Errorf("%s: %q should say the count is a lower bound", c.name, w[0])
		}
		if c.warns && c.n > c.w && !strings.Contains(w[0], "waits for itself at any load") {
			t.Errorf("%s: %q should say a group larger than the workers waits at any load", c.name, w[0])
		}
	}
	// the load on a callee of several workers still warns once the batch over the workers is busy enough
	topo, stats, nodes, owner := batchTopo([]int{4}, []float64{1}, 4, false, 160, 10) // 40% busy
	if w := batchWarned(topo, stats, nodes, owner); len(w) != 1 || strings.Contains(w[0], "any load") {
		t.Errorf("a batch of the workers at 40%%: %q, want a warning without the any-load clause", w)
	}
}

// The mean-count explanation is long and the same for every callee: given in full once, then a line that points back.
func TestMeanRuleWarningIsExplainedOnce(t *testing.T) {
	v := newVaryEdge(1, "B", map[int]int64{1: 5, 2: 5}, 10)
	full := meanRuleWarning("A", v, true)
	short := meanRuleWarning("A", v, false)
	if !strings.Contains(full, "Calls lost from the capture, or sampled out") || strings.Contains(short, "Calls lost from the capture") {
		t.Errorf("full:\n%s\nshort:\n%s", full, short)
	}
	for _, w := range []string{full, short} {
		if !strings.HasPrefix(w, "A -> B: calls per visit vary as {1: 5, 2: 5}; only their mean (1.5 per visit) enters the model, as 1 call(s) made on every visit and one more with probability 0.50.") {
			t.Errorf("both start with the counts and the mean: %q", w)
		}
	}
	if len(short) > len(full)/2 {
		t.Errorf("the later warning is %d characters, the first %d", len(short), len(full))
	}
}

// Services that emit spans but no SERVER span are not in the model; the report names them and what they emit (INTERNAL spans
// only when that is all there is).
func TestNotModelledWarning(t *testing.T) {
	names := []string{"accounting", "load-generator", "batch"}
	bit := func(kinds ...uint8) uint8 {
		var b uint8
		for _, k := range kinds {
			b |= 1 << k
		}
		return b
	}
	if w := notModelledWarning(nil, names); w != "" {
		t.Errorf("none: %q", w)
	}
	w := notModelledWarning(map[int32]uint8{0: bit(otlp.KindConsumer, otlp.KindClient, otlp.KindInternal), 1: bit(otlp.KindClient, otlp.KindInternal)}, names)
	for _, want := range []string{"2 services emit no SERVER span, so they are not in the model", "--patch cannot name them",
		"accounting (CLIENT, CONSUMER spans), load-generator (CLIENT spans)"} {
		if !strings.Contains(w, want) {
			t.Errorf("warning lacks %q: %s", want, w)
		}
	}
	w = notModelledWarning(map[int32]uint8{2: bit(otlp.KindInternal)}, names)
	if !strings.Contains(w, "1 service emits no SERVER span, so it is not in the model") || !strings.Contains(w, "batch (INTERNAL spans)") {
		t.Errorf("one service with INTERNAL spans only: %s", w)
	}
}
