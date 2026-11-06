// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"math"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// The documented sizes of the biased constructs (docs/ACCURACY.md section 1; docs/VALIDATION.md), the largest seen in
// the simulator, as fractions of the change's size or of the entry's mean before the change. A band is widened by
// each construct on its path, once per construct, on the side its error lies on (ARCHITECTURE.md 4.5, item 3).
const (
	// The second call of a sequential caller: over-predicted behind a smooth first callee by up to 34% of the change,
	// under-predicted behind a bursty one by up to 38% (item 2); the traces do not say which, so both sides widen.
	seqOverFrac  = 0.34
	seqUnderFrac = 0.38
	// A change in a fan-out branch that is not the slowest: over-predicted by up to 1.7% of the latency (item 4).
	branchLatencyFrac = 0.017
	// Staged calls pass too much of a small change through: over-predicted by up to 85.5% of the change (item 5).
	stagedFrac = 0.855
	// A regular upstream makes the change too small: under-predicted by up to 8.5% of the change (item 8).
	upstreamFrac = 0.085
	// regularCS2 is the service-time cs2 below which an upstream service's departures count as regular [D]: the
	// documented case is a gamma upstream of cs2 0.25.
	regularCS2 = 0.5
)

// nodeService is the service a node of the call structure belongs to: S for a stage node (stage:S:n, stage:S:j.s or
// shape:S:j, a node the inference adds for the calls of S, which it marks as a stage), the node's own name otherwise.
// A service the traces name like a stage node is a service of its own, named by its whole name, which is a row's; the
// fit's checks hold a stage node's S to be a row too.
func nodeService(name string, stage bool) string {
	if !stage {
		return name
	}
	for _, p := range []string{"stage:", "shape:"} {
		if rest, ok := strings.CutPrefix(name, p); ok {
			if k := strings.LastIndexByte(rest, ':'); k >= 0 {
				return rest[:k]
			}
		}
	}
	return name
}

// biased lists the constructs with a documented directional error on the paths from an entry to the changed nodes:
// every call the entry waits for, through any chain of calls, that leads to a changed node. It reads the call
// structure, which of its nodes are stage nodes (stage, by node) and the base solution only.
func biased(topo *model.Topology, stage []bool, base *model.Result, entry int, changed []int,
	plain bool) []report.Biased {
	n := len(topo.Services)
	// leads: the node is changed, or a call it waits for leads to one
	leads := make([]bool, n)
	for _, i := range changed {
		leads[i] = true
	}
	order := topo.Order()
	for k := len(order) - 1; k >= 0; k-- {
		i := order[k]
		for _, c := range topo.Services[i].SyncCalls() {
			if leads[c.Child] {
				leads[i] = true
			}
		}
	}
	// reached: the entry waits for the node, through calls
	reached := make([]bool, n)
	reached[entry] = true
	for _, i := range order {
		if !reached[i] {
			continue
		}
		for _, c := range topo.Services[i].SyncCalls() {
			reached[c.Child] = true
		}
	}
	out := []report.Biased{}
	add := func(construct, service, direction string) {
		for _, b := range out {
			if b.Construct == construct && b.Service == service {
				return
			}
		}
		out = append(out, report.Biased{Construct: construct, Service: service, Direction: direction})
	}
	isChanged := func(i int) bool {
		for _, c := range changed {
			if c == i {
				return true
			}
		}
		return false
	}
	for _, i := range order {
		if !reached[i] {
			continue
		}
		sv := &topo.Services[i]
		calls := sv.SyncCalls()
		slowest := math.Inf(-1)
		if sv.Parallel {
			for _, c := range calls {
				slowest = math.Max(slowest, c.P*(topo.CallHop(c)+base.L[c.Child]))
			}
		}
		for j, c := range calls {
			if !leads[c.Child] {
				continue
			}
			child := topo.Services[c.Child].Name
			switch {
			case sv.Parallel && len(calls) > 1 && c.P*(topo.CallHop(c)+base.L[c.Child]) < slowest:
				add(report.ConstructNonSlowestBranch, nodeService(sv.Name, stage[i]), report.DirectionOver)
			case !sv.Parallel && j > 0 && hasOtherChild(calls[:j], c.Child):
				add(report.ConstructSecondSequentialCall, nodeService(sv.Name, stage[i]), report.DirectionBoth)
			}
			// a stage node whose calls are one batch is composed by the batch term (model.Config.Batch), not as the
			// maximum over its branches that item 5 measured
			if stage[c.Child] && strings.HasPrefix(child, "stage:") && (plain || !model.IsBatchNode(topo, c.Child)) {
				add(report.ConstructStagedCalls, nodeService(child, true), report.DirectionOver)
			}
			if !plain && isChanged(c.Child) && !sv.Delay && sv.CS2 < regularCS2 {
				add(report.ConstructRegularUpstream, nodeService(sv.Name, stage[i]), report.DirectionUnder)
			}
		}
	}
	return out
}

// hasOtherChild reports whether an earlier call goes to another node: the call is then a later call of a sequential
// caller, whose arrivals are an earlier callee's departures.
func hasOtherChild(earlier []model.Call, child int) bool {
	for _, c := range earlier {
		if c.Child != child {
			return true
		}
	}
	return false
}

// widening is how far the biased constructs widen a change range beyond the model band, on its low and its high side.
// Over-prediction means the true change is nearer zero than predicted, with the same sign, so it widens the side
// toward zero, but never past zero; under-prediction widens the side away from it. A range that straddles zero has a
// corner on each side: erring toward zero from either stays inside the range, and erring away widens each side by its
// own corner's share (lean/Rbproofs/Lanes/ChangeBand.lean). The widening is then continuous in the range: it no longer
// jumps by a construct's whole size, from one side to the other, as the range's midpoint crosses zero.
func widening(bs []report.Biased, ch change.Range, beforeMs float64) (low, high float64) {
	mag := math.Max(math.Abs(ch.Lo), math.Abs(ch.Hi))
	var toward, awayFrac float64
	seen := map[string]bool{}
	for _, b := range bs {
		if seen[b.Construct] {
			continue
		}
		seen[b.Construct] = true
		switch b.Construct {
		case report.ConstructSecondSequentialCall:
			toward += seqOverFrac * mag
			awayFrac += seqUnderFrac
		case report.ConstructNonSlowestBranch:
			toward += branchLatencyFrac * beforeMs
		case report.ConstructStagedCalls:
			toward += stagedFrac * mag
		case report.ConstructRegularUpstream:
			awayFrac += upstreamFrac
		}
	}
	switch {
	case ch.Lo >= 0: // an increase (or none): toward zero is the low side
		return math.Min(toward, ch.Lo), awayFrac * mag
	case ch.Hi <= 0: // a decrease
		return awayFrac * mag, math.Min(toward, -ch.Hi)
	}
	return awayFrac * -ch.Lo, awayFrac * ch.Hi
}
