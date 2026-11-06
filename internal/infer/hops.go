// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"fmt"
	"math"
	"slices"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

const (
	// hopQueueShare: the wait outside the span that the model predicts may be at most this share of the gap's excess
	// over its quietest tenth for the rest of the excess to be read as the network's. On the real captures measured
	// the quiet edges (the demo's cache, the OpenTelemetry demo's browse edges) have a share of 0.00-0.53 and every
	// loaded edge (api and db at 35-80% busy) 0.95-1.41. It is a hard switch: a smooth one (the excess less twice the
	// wait) did worse on the real cache edge.
	hopQueueShare = 0.75
	// hopMaxRho: the mean-based hop is used only on a callee the model puts at or below this utilization. Above it
	// the wait the model predicts is the weak point of the rule (a pod of several workers whose waits the model
	// understates, or bursts), and a hop that holds queueing is worse than a hop that misses the network's spread: in
	// the accuracy harness without this limit, 8 of 288 inferences took a hop of up to 17.7 ms (the network's is 0.3 ms)
	// at a callee 32-77% busy, and the change error rose on 4 of 6 runs.
	hopMaxRho = 0.3
	// hopMinCalls: an edge with fewer calls than this takes the pooled gaps of its callee.
	hopMinCalls = 50
	// A mean-based hop that lies this far above the quietest tenth of the gaps (ms), and this many times above it, is
	// reported: it is more than the spread of a network time, and may hold waiting that the model does not show.
	hopWarnMs    = 1.0
	hopWarnRatio = 3.0
	// hopSameMs: a call's hop within this of its callee's is its callee's (rounding of the mean of equal hops).
	hopSameMs = 1e-9
)

// hopFromGaps is the network time of a set of calls from their gaps (client span time minus server span time,
// sorted), the wait outside the callee's spans that the model predicts at this load (waitOut, ms: 0 when the spans
// start at arrival, because the wait is then inside them) and the callee's utilization rho. fromMean reports that the
// mean-based rule was used.
//
// The gap of a call is its network time plus any wait outside the callee's span. The quietest tenth of the gaps holds
// no wait when at least a tenth of the calls find a free worker; that is the shipped rule (hop = p10), and it is right
// where the queue explains the rest of the gap. But the quietest tenth also holds the quiet end of the network time's
// own spread, so it lies below the mean network time by the spread's mean, 0.2-0.3 ms per call on the real clusters
// measured; at 1% utilization, where the model's wait is 0.01 ms, that is all that is missing from every call.
// When the callee is at most hopMaxRho busy and the model's wait explains at most hopQueueShare of the excess
// mean - p10, the rest is not queueing: the hop is the mean gap less the model's wait. Otherwise it stays p10.
// So p10 <= hop <= mean, always.
func hopFromGaps(sorted []float64, waitOut, rho float64) (hop float64, fromMean bool) {
	n := len(sorted)
	if n == 0 {
		return 0, false
	}
	p10 := sorted[n/10]
	if n < hopMinCalls || !(rho <= hopMaxRho) {
		return p10, false
	}
	mean := meanSorted(sorted)
	if waitOut <= hopQueueShare*(mean-p10) {
		return mean - waitOut, true
	}
	return p10, false
}

func meanSorted(sorted []float64) float64 {
	sum := 0.0
	for _, v := range sorted {
		sum += v
	}
	return sum / float64(len(sorted))
}

// setHops gives every call into a service the network time of its own edge (caller to callee) and every callee the
// mean of the hops into it, by the calls observed on each edge (what the report adds to the callee's latency to
// compare it with what its callers saw, and what its observed wait is read against). It runs once the topology is
// final: the wait the model predicts outside a callee's spans is its wait there less the wait the visits' spans
// hold, and neither depends on the hops, so one pass is enough. When the model cannot be solved the callees keep the
// shipped hop, the quietest tenth of the gaps into them (set where the services are built).
//
// An edge is a caller's service to a callee, however the model reaches it: the calls of a stage or of a shape node
// are their service's (nodeOwner), and a call to such a node has no hop of its own. A callee whose callers' edges
// are not all in the topology as calls the callers wait for (async edges) has the mean over the edges that are.
// The wait is the one the refined model predicts, or the basic model's with plain (--plain). It returns the warnings for
// edges whose mean-based hop is larger than a network's spread.
func setHops(out *Inferred, t *acc, used []int32, nodeOwner map[int]string, plain bool) (warnings []string) {
	topo := out.Topology
	cfg := model.Default
	if plain {
		cfg = model.Plain
	}
	res, err := model.AnalyticWith(topo, out.LamExtPerMs, nil, cfg)
	if err != nil || !res.Stable {
		return nil
	}
	waitOut := make([]float64, len(used))
	for k := range used {
		if !out.Stats[k].External {
			waitOut[k] = math.Max(0, res.R[k]-topo.Services[k].S-out.Stats[k].InSpanWaitMs)
		}
	}
	// callees that some parallel node calls twice or more at once: their calls reach them together and queue behind
	// each other, a wait the model does not show (batchWarnings), and the hop then holds it
	batched := map[int]bool{}
	for i := range topo.Services {
		if !topo.Services[i].Parallel {
			continue
		}
		cnt := map[int]int{}
		for _, c := range topo.Services[i].SyncCalls() {
			if cnt[c.Child]++; cnt[c.Child] == 2 {
				batched[c.Child] = true
			}
		}
	}
	pooled := map[int32][]float64{}
	pooledOf := func(callee int32) []float64 {
		g, ok := pooled[callee]
		if !ok {
			g = t.hopSamples[callee]
			if !slices.IsSorted(g) {
				slices.Sort(g)
			}
			pooled[callee] = g
		}
		return g
	}
	type edgeHop struct {
		hop      float64
		has      bool
		fromMean bool
	}
	edges := map[edgeKey]edgeHop{}
	type into struct{ n, hopSum float64 }
	in := make([]into, len(used))
	fromMean := make([]bool, len(used))
	for si := range topo.Services {
		ck := si // the service whose visits make these calls
		if si >= len(used) {
			if ck = topo.Index(nodeOwner[si]); ck < 0 || ck >= len(used) {
				continue
			}
		}
		caller := used[ck]
		for ci := range topo.Services[si].Calls {
			c := &topo.Services[si].Calls[ci]
			if c.Async || c.Child >= len(used) || out.Stats[c.Child].External {
				continue
			}
			ek := edgeKey{caller, used[c.Child]}
			eh, seen := edges[ek]
			if !seen {
				gaps := t.edgeGaps[ek]
				n := float64(len(gaps))
				if len(gaps) < hopMinCalls { // too few calls on this edge: its callee's, from all its callers
					gaps = pooledOf(ek.callee)
				} else if !slices.IsSorted(gaps) {
					slices.Sort(gaps)
				}
				if len(gaps) > 0 {
					eh.hop, eh.fromMean = hopFromGaps(gaps, waitOut[c.Child], res.Rho[c.Child])
					eh.has = true
					if p10 := gaps[len(gaps)/10]; eh.fromMean && eh.hop-p10 > hopWarnMs && eh.hop > hopWarnRatio*p10 {
						callee := out.Stats[c.Child].Name
						group := ""
						if batched[c.Child] {
							group = fmt.Sprintf(" (and part of it may be the calls of one group, made to %s at once, queueing behind each other)", callee)
						}
						warnings = append(warnings, fmt.Sprintf("%s -> %s: a call takes %.2f ms on average between its client span and the server span "+
							"under it, and %.2f ms for the quietest tenth of the calls; the model's wait for %s outside its spans (%.2f ms at %.0f%% "+
							"utilization) explains little of the difference. The hop is taken as that mean less the wait, %.2f ms, so what the model does "+
							"not show (calls that reach %s together, a stall) is in the hop%s, and a change of %s's time will not move it",
							out.Stats[ck].Name, callee, meanSorted(gaps), p10, callee, waitOut[c.Child], 100*res.Rho[c.Child], eh.hop, callee, group, callee))
					}
					// each edge counts once in its callee's mean, by the calls it was observed to make
					in[c.Child].n += n
					in[c.Child].hopSum += n * eh.hop
					fromMean[c.Child] = fromMean[c.Child] || eh.fromMean
				}
				edges[ek] = eh
			}
			if eh.has {
				c.Hop, c.HasHop = eh.hop, true
			}
		}
	}
	for k, id := range used {
		st := &out.Stats[k]
		if st.External || in[k].n == 0 {
			continue
		}
		h := in[k].hopSum / in[k].n
		topo.Services[k].Hop, st.HopMs, st.HopFromMean = h, h, fromMean[k]
		if g := pooledOf(id); len(g) > 0 {
			st.ObservedWaitMs = math.Max(0, meanSorted(g)-h)
		}
	}
	// A call whose hop is its callee's own (a callee with one caller, or callers over links of one speed) names none:
	// the topology then reads as it always did, and the callee's hop is the one to edit.
	for si := range topo.Services {
		for ci := range topo.Services[si].Calls {
			if c := &topo.Services[si].Calls[ci]; c.HasHop && math.Abs(c.Hop-topo.Services[c.Child].Hop) <= hopSameMs {
				c.Hop, c.HasHop = 0, false
			}
		}
	}
	return warnings
}
