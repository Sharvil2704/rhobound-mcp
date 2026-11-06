// SPDX-License-Identifier: Apache-2.0

package infer

// What a capture lost, named. A trace that lost a span is kept out of the entries and the call structure and
// counts in the queues and the rates (processTrace); here the loss is told by service and by address, so that
// whoever reads the refusal or the warning knows which exporter or which edge to look at, how much of the
// edge's traffic is concerned, and what the model did with it.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// lostKey names calls: the service (an index into every service name, nameOf) that made them and the address
// (an index into Data.Addrs) they went to.
type lostKey struct{ caller, addr int32 }

// orphanKey names spans lost below a top: a service name (nameOf) and a span kind.
type orphanKey struct {
	name int32
	kind uint8
}

// lostEdge is the calls from one service to one address that some service answers on, and the calls that did
// have a span of the callee under them.
type lostEdge struct {
	caller, addr string
	callee       int32  // the service that answers on addr (-1: none or several)
	calleeName   string // ... its name
	lost, found  int64
}

// share is the share of the edge's calls that lost their callee's span.
func (e lostEdge) share() float64 { return float64(e.lost) / float64(e.lost+e.found) }

// String is "a -> b:8080 (answered by b): 62 of 600 calls (10.3%)"; the service is named when the address does not
// (a ClusterIP, a pod IP).
func (e lostEdge) String() string {
	via := ""
	if e.calleeName != "" && e.addr != e.calleeName && !strings.HasPrefix(e.addr, e.calleeName+":") {
		via = " (answered by " + e.calleeName + ")"
	}
	return fmt.Sprintf("%s -> %s%s: %d of %d calls (%.1f%%)", e.caller, e.addr, via, e.lost, e.lost+e.found, 100*e.share())
}

// lostEdgesOf lists the edges that lost their callee's span, most calls first (ties by name), from the traces'
// counts (acc.lostEdges) and the pre-pass's calls that found theirs.
func lostEdgesOf(t *acc, pp *prepass, d *otlp.Data, nameList, names []string) []lostEdge {
	var out []lostEdge
	for k, n := range t.lostEdges {
		e := lostEdge{caller: nameList[k.caller], addr: d.Addrs[k.addr], callee: -1, lost: n, found: pp.addrCalls[k]}
		if by := pp.answeredBy[k.addr]; by >= 0 && int(by) < len(names) {
			e.callee, e.calleeName = by, names[by]
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.lost != y.lost {
			return x.lost > y.lost
		}
		if x.caller != y.caller {
			return x.caller < y.caller
		}
		return x.addr < y.addr
	})
	return out
}

// maxNamed is how many edges (or services) a message names; the rest are counted.
const maxNamed = 3

// edgeList names the first maxNamed edges, "a -> b:8080: 62 of 600 calls (10.3%); ...", and counts the others.
func edgeList(es []lostEdge) string {
	var parts []string
	for i, e := range es {
		if i == maxNamed {
			parts = append(parts, fmt.Sprintf("and %d more edges", len(es)-maxNamed))
			break
		}
		parts = append(parts, e.String())
	}
	return strings.Join(parts, "; ")
}

var kindName = map[uint8]string{otlp.KindInternal: "INTERNAL", otlp.KindServer: "SERVER", otlp.KindClient: "CLIENT",
	otlp.KindProducer: "PRODUCER", otlp.KindConsumer: "CONSUMER"}

// orphanList names the services and kinds of the spans lost below a top, most first: "b CLIENT 12, c SERVER 3".
func orphanList(m map[orphanKey]int64, nameList []string) string {
	type item struct {
		text string
		n    int64
	}
	var items []item
	for k, n := range m {
		kn := kindName[k.kind]
		if kn == "" {
			kn = fmt.Sprint("kind ", k.kind)
		}
		items = append(items, item{fmt.Sprintf("%s %s", nameList[k.name], kn), n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].text < items[j].text
	})
	var parts []string
	for i, it := range items {
		if i == maxNamed {
			parts = append(parts, fmt.Sprintf("and %d more", len(items)-maxNamed))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", it.text, it.n))
	}
	return strings.Join(parts, ", ")
}

// lostByCallee sums the lost calls of the edges by the service that answers on their address.
func lostByCallee(edges []lostEdge) map[int32]int64 {
	m := map[int32]int64{}
	for _, e := range edges {
		if e.callee >= 0 {
			m[e.callee] += e.lost
		}
	}
	return m
}

// lostWarnings are the warnings for a capture with broken traces that is not refused (at most maxBrokenShare of
// them): per cause what was lost, where, and what the model did with it. The caller has put the general one first
// (how many traces, that they are out of the entries and the structure, kept in the queues and the rates).
func lostWarnings(t *acc, pp *prepass, edges []lostEdge, nameList, names []string) []string {
	var out []string
	if t.brokenBy[lossCallee] > 0 {
		// per callee, the service that answers on the addresses (the most lost calls first; edges that no single service
		// answers on, last)
		type group struct {
			callee int32
			edges  []lostEdge
			lost   int64
		}
		var groups []*group
		byCallee := map[int32]*group{}
		for _, e := range edges {
			g := byCallee[e.callee]
			if g == nil {
				g = &group{callee: e.callee}
				byCallee[e.callee] = g
				groups = append(groups, g)
			}
			g.edges = append(g.edges, e)
			g.lost += e.lost
		}
		sort.SliceStable(groups, func(i, j int) bool {
			if (groups[i].callee < 0) != (groups[j].callee < 0) {
				return groups[j].callee < 0
			}
			return groups[i].lost > groups[j].lost
		})
		for i, g := range groups {
			if i == maxNamed {
				out = append(out, fmt.Sprintf("and %d more services whose calls lost their span", len(groups)-maxNamed))
				break
			}
			if g.callee < 0 {
				out = append(out, fmt.Sprintf("calls to an address that no single service answers on lost the callee's span: %s. Their traces "+
					"are out of the entries and the call structure and stay in the queues and the rates", edgeList(g.edges)))
				continue
			}
			name := names[g.callee]
			share := float64(g.lost) / float64(g.lost+t.visits[g.callee])
			marked := "the row is marked !"
			if share < lostMarkShare {
				marked = "a share too small to mark the row"
			}
			out = append(out, fmt.Sprintf("%s: %d calls to it lost its span (%.1f%% of its visits): %s. Their traces are out of the entries and the "+
				"call structure and stay in the queues and the rates. The model counts their requests (the external rate is scaled up) "+
				"and gives them the calls of the others, but the service time, the queue and the worker count of %s are read from the "+
				"visits whose spans were captured, so they are off if the lost spans are not typical (the slow ones, or those of the busiest "+
				"moments, as when an exporter's queue is full); %s. The span was dropped (an exporter's full queue, "+
				"sampling, a pod that went away) or the trace context did not reach it",
				name, g.lost, 100*share, edgeList(g.edges), name, marked))
		}
	}
	if t.brokenBy[lossCalledTop] > 0 {
		for i, c := range t.calledTop {
			if c == 0 || i >= len(names) {
				continue
			}
			out = append(out, fmt.Sprintf("%s: %d traces hold a SERVER span of it whose parent is not in the capture, and services in the "+
				"capture call %s on %.1f%% of its %d visits, so a caller's span was lost (it is not read as an entry from outside, "+
				"which needs most of its server spans to have no caller in the capture)", names[i], c,
				names[i], 100*float64(pp.srvCalled[i])/float64(pp.srvSpans[i]), pp.srvSpans[i]))
		}
	}
	if t.brokenBy[lossBelowTop] > 0 {
		out = append(out, fmt.Sprintf("%d traces hold spans whose parent is not in the capture below a top that is (a SERVER span with no "+
			"parent), or on a span that is not a SERVER or CONSUMER span; the spans, by service and kind: %s. The parent's span was lost",
			t.brokenBy[lossBelowTop], orphanList(t.lostBelow, nameList)))
	}
	return out
}

// brokenVisitShareWarn is the share of a service's visits in broken traces from which it is warned about: their
// calls are left out of its call structure, and when the broken traces are all of one kind of request (an endpoint
// whose callee's span is always lost, 5% of the traces and every visit of the service behind it) the structure
// of that kind of request is not in the model at all. Broken traces spread over the requests give about the overall share.
const brokenVisitShareWarn = 0.25

// minBrokenVisits is how many visits a service needs for that share to be told.
const minBrokenVisits = 30

// markStructure sets BrokenVisitShare on the services with many visits in broken traces and returns the warnings.
func markStructure(t *acc, stats []ServiceStats, idx map[int32]int) []string {
	var out []string
	for i := range t.visits {
		k, ok := idx[int32(i)]
		if !ok || stats[k].External || t.visits[i] < minBrokenVisits {
			continue
		}
		share := 1 - float64(t.sVisits[i])/float64(t.visits[i])
		if share <= brokenVisitShareWarn {
			continue
		}
		stats[k].BrokenVisitShare = share
		what := fmt.Sprintf("so its calls per visit, their order and its glue are read from the other %d visits only (broken traces can be all of "+
			"one kind of request, which then has no structure in the model)", t.sVisits[i])
		if t.sVisits[i] == 0 {
			what = "so none of its calls are in the call structure: it is modelled without calls (broken traces can be all of one kind of request)"
		}
		out = append(out, fmt.Sprintf("%s: %.0f%% of its %d visits are in traces that miss a span, %s; the row is marked !", stats[k].Name, 100*share,
			t.visits[i], what))
	}
	return out
}

// markLost sets LostVisits and LostShare on the stats of the services that answer on the edges' addresses and lost
// at least lostMarkShare of their visits (their visits miss from the capture). idx maps a service id to its place in stats.
func markLost(edges []lostEdge, stats []ServiceStats, idx map[int32]int) {
	for id, n := range lostByCallee(edges) {
		if k, ok := idx[id]; ok {
			if share := float64(n) / float64(n+stats[k].Visits); share >= lostMarkShare {
				stats[k].LostVisits, stats[k].LostShare = n, share
			}
		}
	}
}

// lostMarkShare is the share of a service's visits that calls without its span must make for the service to be marked
// in the report (LostVisits, LostShare); a few lost spans are told in the warning only.
const lostMarkShare = 0.01

// minCalledShare is the share of an entry service's visits that services in the capture must call for
// entryCalledWarnings to name it: a few calls from an admin tool or a batch job are not what it is about.
const minCalledShare = 0.05

// entryCalledWarnings name the services that are entered from outside the capture (most of their server spans have
// no caller's visit in it, entryLike) and that services in the capture also call on at least minCalledShare of
// their visits. What is outside is not known: an ingress or a gateway whose spans are not exported (an entry
// indeed), or services that call it without a span of their own or without passing the trace context on, whose
// requests are then entries with no sender. Their load on the service is right, but the sender is not in the
// model: a change of the service's time reaches the callers in the capture and not that sender.
func entryCalledWarnings(pp *prepass, names []string) []string {
	var out []string
	for i, n := range pp.srvSpans {
		if n == 0 || !pp.entryLike[i] || pp.srvCalled[i] == 0 {
			continue
		}
		called := float64(pp.srvCalled[i]) / float64(n)
		if called < minCalledShare {
			continue
		}
		type caller struct {
			name string
			n    int64
		}
		var cs []caller
		for k, c := range pp.callN {
			if int(k.callee) == i && int(k.caller) != i {
				cs = append(cs, caller{names[k.caller], c})
			}
		}
		sort.Slice(cs, func(x, y int) bool {
			if cs[x].n != cs[y].n {
				return cs[x].n > cs[y].n
			}
			return cs[x].name < cs[y].name
		})
		if len(cs) == 0 {
			continue // called only by itself
		}
		var who []string
		for _, c := range cs {
			who = append(who, c.name)
		}
		if len(who) > maxNamed {
			who = append(who[:maxNamed:maxNamed], fmt.Sprintf("%d more", len(cs)-maxNamed))
		}
		callers := strings.Join(who, ", ")
		out = append(out, fmt.Sprintf("%s is entered from outside the capture and is also called by %s: %.1f%% of its %d visits have no caller's "+
			"visit in the capture (requests from outside it, or from a service whose spans are not here or that does not pass the trace "+
			"context on), %.1f%% are called by %s. The load on %s is right; a change of %s's time reaches %s and not those senders",
			names[i], callers, 100*(1-called), n, 100*called, callers, names[i], names[i], callers))
	}
	return out
}
