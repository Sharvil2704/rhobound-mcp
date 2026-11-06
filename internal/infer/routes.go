// SPDX-License-Identifier: Apache-2.0

package infer

import (
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// Per-route own time. One service can serve routes of very different cost (a cheap /item and a dear /search), each
// reached from its own callers. The model keeps one own time per service, the visit-weighted blend; a change that
// moves one route moves the blend by the route's share only, and a caller that reaches only that route sees all of it.
// The routes kept here let an A/B record measure each route's own-time difference (internal/abdiff) and let the
// engine apply each route's difference to the calls that reach that route, while the queue still moves by the blend.

// The route buckets of a service: at most MaxRoutesPerService routes, each with at least MinRouteShare of its visits;
// the rest count under OtherRoute. A service gets routes only when two or more routes pass: a service with one route
// (the usual case) has none, and every output stays as it was without routes.
const (
	MaxRoutesPerService = 8
	MinRouteShare       = 0.05
	OtherRoute          = "other"
)

// RouteStats are the observations of one route of a service: its share of the visits the service time is read from,
// its own time (the service time of those visits, as ServiceStats.OwnMeanMs is for all of them) and who reaches it.
type RouteStats struct {
	Route  string  `json:"route"`
	Visits int64   `json:"visits"`
	Share  float64 `json:"share"`
	OwnMs  float64 `json:"own_ms"`
	OwnCS2 float64 `json:"own_cs2"` // as measured, never snapped to 1
	// Callers are the visits of this route per visit of each caller, over every trace that kept the caller's visit
	// (each caller's own mix, however often its traces break); the caller "" counts the route's entry visits per entry
	// visit of the service, over complete traces.
	Callers []RouteCaller `json:"callers"`
}

// RouteCaller is how often one caller reaches a route: route visits per caller visit.
type RouteCaller struct {
	Caller   string  `json:"caller"`
	PerVisit float64 `json:"per_visit"`
}

// routeEdgeKey counts the visits of callee's route (a Data.Routes index + 1, 0 none) made by caller's visits, in every
// trace that has the caller's visit (-1: entry visits, in complete traces).
type routeEdgeKey struct {
	caller, callee int32
	route          uint16
}

// routeKey counts the visits of a service's route (routeKnown: those of broken traces whose caller is known).
type routeKey struct {
	svc   int32
	route uint16
}

// routeBuckets picks each service's routes from its visits: bucket[svc] maps a span's route (Span.Route) to its
// bucket's index in names[svc] (OtherRoute last when some visits fall outside the kept routes). Both are nil for a
// service with fewer than two kept routes.
func routeBuckets(vrec []visitRec, routes []string, nSvc int) (names [][]string, bucket []map[uint16]int) {
	names, bucket = make([][]string, nSvc), make([]map[uint16]int, nSvc)
	counts := make([]map[uint16]int64, nSvc)
	total := make([]int64, nSvc)
	for i := range vrec {
		s := vrec[i].svc
		if int(s) >= nSvc || s < 0 {
			continue
		}
		if counts[s] == nil {
			counts[s] = map[uint16]int64{}
		}
		counts[s][vrec[i].route]++
		total[s]++
	}
	name := func(r uint16) string {
		if r == 0 || int(r) > len(routes) {
			return ""
		}
		return routes[r-1]
	}
	for s := 0; s < nSvc; s++ {
		type rc struct {
			r uint16
			n int64
		}
		var all []rc
		for r, n := range counts[s] {
			if r != 0 && name(r) != "" && name(r) != OtherRoute {
				all = append(all, rc{r, n})
			}
		}
		sort.Slice(all, func(a, b int) bool {
			if all[a].n != all[b].n {
				return all[a].n > all[b].n
			}
			return name(all[a].r) < name(all[b].r)
		})
		var kept []rc
		var keptN int64
		for _, x := range all {
			if len(kept) == MaxRoutesPerService || float64(x.n) < MinRouteShare*float64(total[s]) {
				break
			}
			kept = append(kept, x)
			keptN += x.n
		}
		if len(kept) < 2 {
			continue
		}
		b := map[uint16]int{}
		for k, x := range kept {
			names[s] = append(names[s], name(x.r))
			b[x.r] = k
		}
		if keptN < total[s] {
			names[s] = append(names[s], OtherRoute)
		}
		bucket[s] = b
	}
	return names, bucket
}

// bucketOf is the bucket of a visit's route: its kept route, else OtherRoute (the last name).
func bucketOf(names []string, bucket map[uint16]int, route uint16) int {
	if k, ok := bucket[route]; ok {
		return k
	}
	return len(names) - 1
}

// routeStatsOf returns the routes of service svc: each bucket's service times, read from the same visits as the
// service's (the FIFO rebuild's jobs; with the rise estimator, each visit's own time up to its release), and its
// callers from the traces that keep them (routeEdge). names and bucket are routeBuckets'.
func routeStatsOf(t *acc, q *svcQueue, svc int32, names []string, bucket map[uint16]int, svcNames []string) []RouteStats {
	if len(names) == 0 {
		return nil
	}
	xs := make([][]float64, len(names))
	if q != nil {
		for _, pq := range q.pods {
			for k := range pq.B {
				v := &t.vrec[pq.vidx[pq.Order[k]]]
				b := bucketOf(names, bucket, v.route)
				xs[b] = append(xs[b], pq.D[k]-pq.B[k])
			}
		}
	} else {
		for i := range t.vrec {
			if v := &t.vrec[i]; v.svc == svc {
				b := bucketOf(names, bucket, v.route)
				xs[b] = append(xs[b], float64(v.own))
			}
		}
	}
	n := 0
	for _, x := range xs {
		n += len(x)
	}
	if n == 0 {
		return nil
	}
	out := make([]RouteStats, len(names))
	for b, nm := range names {
		rs := RouteStats{Route: nm, Visits: int64(len(xs[b])), Share: float64(len(xs[b])) / float64(n),
			Callers: []RouteCaller{}}
		if len(xs[b]) > 0 {
			m, v := meanVar(xs[b])
			rs.OwnMs = m
			if m > 0 && len(xs[b]) > 1 {
				rs.OwnCS2 = math.Max(0, v) / (m * m)
			}
		}
		out[b] = rs
	}
	// callers: route visits per caller visit, over every trace for a caller (its visits, broken traces included) and
	// over complete traces for the entries
	perCaller := map[int32][]int64{}
	for k, c := range t.routeEdge {
		if k.callee != svc {
			continue
		}
		if perCaller[k.caller] == nil {
			perCaller[k.caller] = make([]int64, len(names))
		}
		perCaller[k.caller][bucketOf(names, bucket, k.route)] += c
	}
	callers := make([]int32, 0, len(perCaller))
	for c := range perCaller {
		callers = append(callers, c)
	}
	callerName := func(c int32) string {
		if c < 0 {
			return ""
		}
		return svcNames[c]
	}
	sort.Slice(callers, func(a, b int) bool { return callerName(callers[a]) < callerName(callers[b]) })
	for _, c := range callers {
		den := float64(t.entries[svc])
		if c >= 0 {
			den = float64(t.visits[c])
		}
		if !(den > 0) {
			continue
		}
		for b := range names {
			if n := perCaller[c][b]; n > 0 {
				out[b].Callers = append(out[b].Callers, RouteCaller{Caller: callerName(c), PerVisit: float64(n) / den})
			}
		}
	}
	return out
}

// routeSamples are a service's own times by route, each in order of arrival (Samples.RouteOwn), from the FIFO
// rebuild's jobs; nil without routes or without the rebuild.
func routeSamples(t *acc, q *svcQueue, names []string, bucket map[uint16]int) map[string][]float64 {
	if len(names) == 0 || q == nil {
		return nil
	}
	type timed struct {
		start uint64
		v     float64
	}
	xs := make([][]timed, len(names))
	for _, pq := range q.pods {
		for k := range pq.B {
			v := &t.vrec[pq.vidx[pq.Order[k]]]
			b := bucketOf(names, bucket, v.route)
			xs[b] = append(xs[b], timed{v.start, pq.D[k] - pq.B[k]})
		}
	}
	out := make(map[string][]float64, len(names))
	for b, nm := range names {
		x := xs[b]
		sort.Slice(x, func(i, j int) bool {
			if x[i].start != x[j].start {
				return x[i].start < x[j].start
			}
			return x[i].v < x[j].v
		})
		vs := make([]float64, len(x))
		for i := range x {
			vs[i] = x[i].v
		}
		out[nm] = vs
	}
	return out
}

// NodeOwner is the service a node of an inferred topology belongs to: the service itself, or, for a zero-time node
// of the call structure (stage:S:n, stage:S:n.m, shape:S:n), the service S whose calls it holds.
func NodeOwner(name string) string {
	for _, p := range []string{"stage:", "shape:"} {
		if strings.HasPrefix(name, p) {
			rest := name[len(p):]
			if k := strings.LastIndexByte(rest, ':'); k > 0 {
				return rest[:k]
			}
		}
	}
	return name
}

// routeNodes gives each routed service of the topology its routes (model.Route: share, own time, the chance of each
// of its calls on a visit of the route) and each call into it the share of the caller's visits each route takes
// (Call.RouteQ, from RouteStats.Callers; a stage or shape node takes its owner's). A route's call chances come from
// its visits' call shapes when the service is a mixture of shapes (mixOf); its other calls keep the blend's chance.
// A parallel service, a service with fewer than two routes and a caller none of whose visits reach a route keep the
// blend.
func routeNodes(topo *model.Topology, stats []ServiceStats, idx map[int32]int, routeSigs map[routeSigKey]int64,
	routeKnown map[routeKey]int64, names [][]string, bucket []map[uint16]int, mixOf map[int]mixtureResult) {
	routed := map[string]int{}        // service name -> topology index
	completeUp := map[int][]float64{} // topology index -> per route, its visits over its visits whose caller is known
	for id := range names {
		si, ok := idx[int32(id)]
		if !ok || len(names[id]) < 2 || len(stats[si].Routes) != len(names[id]) || topo.Services[si].Parallel {
			continue
		}
		sv := &topo.Services[si]
		rs := stats[si].Routes
		if slices.ContainsFunc(rs, func(r RouteStats) bool { return !(r.OwnMs >= 0) || math.IsInf(r.OwnMs, 0) }) {
			// a route whose own time comes out below zero (rebuilt FIFO jobs that land behind a busy request, within
			// the jitter allowance) while the service's does not: the service keeps the blend, as it had before route
			// nodes, instead of failing the fit (lean/Rbproofs/Lanes/Infer.lean, INFER-3)
			continue
		}
		nk := make([]int64, len(rs))
		bySig := make([]map[string]int64, len(rs))
		for k := range bySig {
			bySig[k] = map[string]int64{}
		}
		for key, n := range routeSigs {
			if key.svc != int32(id) {
				continue
			}
			k := bucketOf(names[id], bucket[id], key.route)
			nk[k] += n
			bySig[k][key.sig] += n
		}
		var shareSum float64
		for _, r := range rs {
			shareSum += r.Share
		}
		if !(shareSum > 0) {
			continue
		}
		// a route's visits whose caller is known against all its visits: each caller's counts are of the traces that
		// kept its visit (routeEdge), the shares of every visit, so the visits whose caller was lost (its span, or the
		// call's, not in the capture) are shared out over the route's callers by their counts, and a caller that reaches
		// every visit gets weights equal to the shares (lean/Rbproofs/Lanes/Infer.lean, INFER-1). The known visits are
		// those of complete traces (nk) and those of broken traces that kept their caller (routeKnown). A caller that loses
		// its own spans more on one route than on another still moves the other callers' weights (the lost visits are
		// shared out by the counts).
		known := slices.Clone(nk)
		for key, n := range routeKnown {
			if key.svc == int32(id) {
				known[bucketOf(names[id], bucket[id], key.route)] += n
			}
		}
		up := make([]float64, len(rs))
		for k, r := range rs {
			if known[k] > 0 {
				up[k] = float64(r.Visits) / float64(known[k])
			}
		}
		completeUp[si] = up
		mix, isMix := mixOf[si]
		routes := make([]model.Route, len(rs))
		for k, r := range rs {
			p := make([]float64, len(sv.Calls))
			for j, c := range sv.Calls {
				p[j] = c.P
			}
			if isMix && nk[k] > 0 {
				for m, key := range mix.shapeKeys {
					if j := mix.shapeCall[m]; j >= 0 {
						p[j] = float64(bySig[k][key]) / float64(nk[k])
					}
				}
			}
			routes[k] = model.Route{Name: r.Route, Share: r.Share / shareSum, S: r.OwnMs, CS2: r.OwnCS2, P: p}
		}
		sv.Routes = routes
		routed[sv.Name] = si
	}
	if len(routed) == 0 {
		return
	}
	for i := range topo.Services {
		owner := NodeOwner(topo.Services[i].Name)
		for j := range topo.Services[i].Calls {
			c := &topo.Services[i].Calls[j]
			if _, ok := routed[topo.Services[c.Child].Name]; !ok || c.Async {
				continue
			}
			rs := stats[c.Child].Routes
			q := make([]float64, len(rs))
			sum := 0.0
			up := completeUp[c.Child]
			for k, r := range rs {
				for _, rc := range r.Callers {
					if rc.Caller == owner {
						q[k] = rc.PerVisit * up[k]
					}
				}
				sum += q[k]
			}
			if !(sum > 0) {
				continue
			}
			for k := range q {
				q[k] /= sum
			}
			c.RouteQ = q
		}
	}
}
