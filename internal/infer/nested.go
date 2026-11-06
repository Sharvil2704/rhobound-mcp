// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// Nested spans of one kind in one pod.
//
// Some frameworks open a second span of the same kind inside the span of the
// HTTP or RPC instrumentation, in the same process, for one and the same unit
// of work: Next.js opens a SERVER span for its request handler inside the HTTP
// instrumentation's SERVER span; flagd's flag-evaluation handler opens a
// SERVER span inside the Connect RPC interceptor's; .NET's gRPC client opens a
// CLIENT span around the HTTP client's CLIENT span. Read as they stand, the
// first two are a service calling itself (a cycle in the call graph, and every
// request counted as two visits, the first of which releases its worker at
// once), and the third is one call counted twice: the outer CLIENT span has no
// SERVER span under it, so it is an unmatched call to an address that does
// answer, and the trace is counted as having lost a span.
//
// A span is nested in another when
//   - it is a SERVER (or CLIENT) span and the nearest SERVER or CLIENT span
//     above it, found as ownership is (ancestor: through INTERNAL and PRODUCER
//     spans, ending at a CONSUMER span), is of the same kind;
//   - both were emitted by one pod of one service (same service name and pod
//     identity: k8s.pod.name, else service.instance.id, else host.name);
//   - the inner span starts within the outer's time and does not end later
//     than nestedSlackNs after it; and
//   - for SERVER spans, they are not from one named instrumentation scope.
//     A framework's span inside the HTTP or RPC instrumentation's comes from
//     another library (in the data seen: the HTTP instrumentation and next.js;
//     the Connect interceptor and flagd's handler), where a service that sends
//     a request to itself with a client that opens no span would show two
//     SERVER spans of one scope. That is a call, and it is left as one. A span
//     that names no scope is judged on the other conditions.
//
// Of two nested SERVER spans the inner is made INTERNAL: the outer is the one
// the caller's CLIENT span (or the caller's own span) is the parent of, it
// starts when the request reached the process and ends when the response left
// it. Of two nested CLIENT spans the one the callee's SERVER span is under is the
// call (in the data seen, the inner of 559 of 559), and the wrapper is made
// INTERNAL; a wrapper is never kept over a span with a SERVER span under it, and
// two CLIENT spans that each have one are two calls. When the wrapper wraps
// several (retries, a token fetch and then the call), each of those with a
// SERVER span under it is a call that reached the wire, and a span with none
// under it, below a wrapper that has one, is not another call. When no span
// of the nested CLIENT spans has a SERVER span under it (a call to a database
// or a service that sends no spans), the outermost is kept: it is the operation the
// caller waited for, and what is nested in it (a connect, an attempt) is part
// of it. A kept CLIENT span with no peer takes the peer and port of the nearest
// span of its nest that has one. What is under a demoted span is still under the
// kept one, as INTERNAL spans are walked through, so no work is lost: the kept
// span's time covers it. A parent cycle in malformed data (a span that is its
// own parent), or a chain of more than maxNestedDepth spans, is not nesting:
// such a chain is left alone.
//
// What is not merged: a SERVER span below a CLIENT span below a SERVER span of
// the same service, in this pod or another (a call to itself over the
// network: the CLIENT span stands between, so the nearest span above is not of
// the same kind), a SERVER span below a SERVER span of another pod of the
// same service (a call to a replica), and CONSUMER and PRODUCER spans (a
// consumer's "receive" and "process" spans are a hand-off, not a wrapper: the
// process span starts after the receive span has done most of its work). The
// pod is the identity the capture gives: a capture whose resources name no pod,
// instance or host has one pod per service.
//
// The time test cannot be strict containment. Node.js (and nginx) spans carry
// start times on whole milliseconds, so two spans of one process are placed up
// to a millisecond apart from where they were: in the 16,824 Next.js pairs of
// the OpenTelemetry Demo (two captures) the inner span starts 0 to 3 ms after the
// outer one, and 17 to 22% of them end after it, by up to 0.89 ms. The slack is
// twice that clock's resolution, so that a pair at the edge of it is still merged
// (one pair left unmerged would leave a service calling itself, and the whole
// capture refused); an inner span that starts or ends more than that after the
// outer span's end is work that outlives the request, which is not merged.
const nestedSlackNs = 2_000_000

// maxNestedDepth: a chain of more nested spans than this is not a framework's wrapper and is left alone, as a parent
// cycle is. The walk up a chain is then bounded, and so is the cost of a trace of thousands of nested spans.
const maxNestedDepth = 64

// nestedCount is the nested spans made INTERNAL for one service name; unscoped are the SERVER spans among them for
// which the instrumentation scope of the span or of the one it is nested in is not named, so that a wrapper could not be
// told from a request the service sent to itself.
type nestedCount struct{ server, client, unscoped int64 }

// nestedInTime reports whether inner is inside outer's time, to nestedSlackNs.
func nestedInTime(outer, inner *otlp.Span) bool {
	return inner.Start+nestedSlackNs >= outer.Start && inner.Start <= outer.End+nestedSlackNs &&
		inner.End <= outer.End+nestedSlackNs
}

// sameScope reports whether two spans were emitted by one named instrumentation scope.
func sameScope(a, b *otlp.Span) bool { return a.Scope != 0 && a.Scope == b.Scope }

// mergeNested makes the nested spans of one trace INTERNAL (see above) and adds
// them to count, by service name (resName).
func mergeNested(g []otlp.Span, pos map[uint64]int, resPod, resName []int32, count []nestedCount) {
	// np[k] is the span k is nested in: the nearest SERVER or CLIENT span above it when that is of its kind, from its
	// pod, around it in time and, for SERVER spans, from another instrumentation scope; else -1. It is read on the kinds as
	// read, before anything is changed.
	var np []int
	for k := range g {
		kind := g[k].Kind
		if kind != otlp.KindServer && kind != otlp.KindClient {
			continue
		}
		p, _ := ancestor(g, pos, k)
		if p < 0 || g[p].Kind != kind || resPod[g[p].Res] != resPod[g[k].Res] || !nestedInTime(&g[p], &g[k]) ||
			(kind == otlp.KindServer && sameScope(&g[p], &g[k])) {
			continue
		}
		if np == nil {
			np = make([]int, len(g))
			for i := range np {
				np[i] = -1
			}
		}
		np[k] = p
	}
	if np == nil {
		return
	}
	// chain returns the spans k is nested in, nearest first; ok is false for a chain longer than maxNestedDepth (or one
	// that loops: a span that is its own ancestor in malformed data), which is left alone.
	var buf []int
	chain := func(k int) (c []int, ok bool) {
		buf = buf[:0]
		for cur := k; np[cur] >= 0; cur = np[cur] {
			if len(buf) == maxNestedDepth {
				return nil, false
			}
			buf = append(buf, np[cur])
		}
		return buf, true
	}
	var demote []int
	var served []bool  // CLIENT spans with a SERVER span under them, found when the first CLIENT chain needs it
	var roots [][2]int // for CLIENT chains: the span (root) that each ends in, and its chain's length, per span
	var clients []int  // the CLIENT spans nested in another, with a valid chain
	for k := range g {
		if np[k] < 0 {
			continue
		}
		c, ok := chain(k)
		if !ok {
			continue
		}
		if g[k].Kind == otlp.KindServer {
			// the inner span of each pair goes (every span of a chain but the outermost has its own turn)
			demote = append(demote, k)
			continue
		}
		if served == nil {
			served = make([]bool, len(g))
			roots = make([][2]int, len(g))
			for i := range g {
				if g[i].Kind == otlp.KindServer {
					if p, _ := ancestor(g, pos, i); p >= 0 && g[p].Kind == otlp.KindClient {
						served[p] = true
					}
				}
			}
		}
		roots[k] = [2]int{c[len(c)-1], len(c)}
		clients = append(clients, k)
	}
	// a tree of nested CLIENT spans (an outermost span, and what is nested in it, however deep) is served when some span
	// of it has a SERVER span under it
	treeServed := map[int]bool{}
	for _, k := range clients {
		r := roots[k][0]
		treeServed[r] = treeServed[r] || served[k] || served[r]
	}
	nearest := map[int][2]int{} // for a tree nobody serves: the span that lends its peer to the outermost: its depth and index
	for _, k := range clients {
		r := roots[k][0]
		c, _ := chain(k)
		if !treeServed[r] {
			// nothing in the tree reached a callee that has spans: the outermost span is the operation the caller waited
			// for, and what is nested in it (a connect, an attempt) is part of it
			demote = append(demote, k)
			if g[k].Peer >= 0 {
				if n, ok := nearest[r]; !ok || roots[k][1] < n[0] || (roots[k][1] == n[0] && k < n[1]) {
					nearest[r] = [2]int{roots[k][1], k}
				}
			}
			continue
		}
		// the span the callee's SERVER span hangs under is the call: a wrapper with no SERVER span under it goes; and
		// a span with none under it, below a wrapper that has one, is not another call
		if !served[k] && slices.ContainsFunc(c, func(p int) bool { return served[p] }) {
			demote = append(demote, k)
			continue
		}
		for _, p := range c {
			if !served[p] || !served[k] {
				demote = append(demote, p)
			}
		}
		if g[k].Peer < 0 {
			for _, p := range c {
				if g[p].Peer >= 0 {
					g[k].Peer, g[k].Addr = g[p].Peer, g[p].Addr
					break
				}
			}
		}
	}
	for r, n := range nearest {
		if g[r].Peer < 0 {
			g[r].Peer, g[r].Addr = g[n[1]].Peer, g[n[1]].Addr
		}
	}
	if len(demote) == 0 {
		return
	}
	slices.Sort(demote)
	demote = slices.Compact(demote)
	for _, i := range demote {
		if g[i].Kind == otlp.KindServer {
			count[resName[g[i].Res]].server++
			if g[i].Scope == 0 || g[np[i]].Scope == 0 {
				count[resName[g[i].Res]].unscoped++
			}
		} else {
			count[resName[g[i].Res]].client++
		}
		g[i].Kind = otlp.KindInternal
	}
}

// NestedMerge is the nested spans of one service that were read as one: Server inner SERVER spans and Client
// outer CLIENT spans made INTERNAL (see above).
type NestedMerge struct {
	Service string `json:"service"`
	Server  int64  `json:"server_spans"`
	Client  int64  `json:"client_spans"`
	// UnscopedServer are the SERVER spans among Server whose own scope or that of the span they are nested in is not
	// named in the capture (see above).
	UnscopedServer int64 `json:"server_spans_without_scope,omitempty"`
}

// nestedSummary lists the merged spans by service name, in name order.
func nestedSummary(names []string, total []nestedCount) []NestedMerge {
	var out []NestedMerge
	for i, c := range total {
		if c.server > 0 || c.client > 0 {
			out = append(out, NestedMerge{Service: names[i], Server: c.server, Client: c.client, UnscopedServer: c.unscoped})
		}
	}
	slices.SortFunc(out, func(a, b NestedMerge) int { return strings.Compare(a.Service, b.Service) })
	return out
}

// nestedWarning describes the merged spans, one entry per service and kind ("" when there are none).
func nestedWarning(merged []NestedMerge) string {
	var parts, unscoped []string
	for _, m := range merged {
		if m.Server > 0 {
			parts = append(parts, fmt.Sprintf("%s %d SERVER (the outer span kept)", m.Service, m.Server))
		}
		if m.Client > 0 {
			parts = append(parts, fmt.Sprintf("%s %d CLIENT (the span the callee's SERVER span is under kept, else the outermost)", m.Service, m.Client))
		}
		if m.UnscopedServer > 0 {
			unscoped = append(unscoped, fmt.Sprintf("%s %d", m.Service, m.UnscopedServer))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	w := "nested spans of one kind in one pod (a framework's span inside the instrumentation's, a gRPC client's span around the " +
		"HTTP client's) were read as one: the other was made INTERNAL, so a request is one visit and a call is one call: " +
		strings.Join(parts, ", ")
	if len(unscoped) > 0 {
		w += ". Of the SERVER pairs, these name no instrumentation scope on one or both spans, so a framework's wrapper could not be " +
			"told from a request the service sent to itself with a client that opens no span (two SERVER spans of one scope are " +
			"kept apart as two visits): " + strings.Join(unscoped, ", ") + "; if the service does call itself that way, its visits " +
			"are counted once"
	}
	return w
}
