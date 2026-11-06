// SPDX-License-Identifier: Apache-2.0

package infer

// Server spans that are not request visits.
//
// A streaming call (a gRPC server stream, a watch, a long poll, a feature-flag
// client that holds a stream open) is one SERVER span that lasts minutes: as long
// as the client stays connected. The model reads a SERVER span as a visit, one
// request that holds a worker, so it would read such a stream as a visit that
// long. In the OpenTelemetry Demo three flagd streams of 600 s each were all
// of flagd's busy time (the other 6,824 visits lasted 0.07 ms at the median),
// which gave flagd a service time of 380 ms and 53% utilization, and the
// callers' mean latencies ten to a thousand times too high, with nothing said.
//
// Such spans reach a capture when it is cut by the time its spans ended or
// covers the whole file a Collector wrote: a stream that began before the
// window is still open in it. A capture cut by the time its traces started
// does not hold them.
//
// A SERVER span is taken to be no visit when it lasts
//   - at least otlp.LongSpan (a second),
//   - at least longVisitShare of the time over which the capture's SERVER spans
//     start (the capture's span; a request that long in a capture that short is
//     not a request of the same service as the rest), and
//   - at least longVisitRatio times the median SERVER span of its service.
//
// When such spans hold longBusyRefuse or more of a service's busy time (the
// sum of its SERVER spans), the capture is refused: its mean time and its
// utilization are theirs. Below that they are in the means as the visits they
// are, and a warning names them.

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

const (
	longVisitShare = 0.10 // of the capture's span, from which a SERVER span is no visit (with the two conditions below)
	longVisitRatio = 1000 // times its service's median SERVER span
	longBusyRefuse = 0.05 // share of a service's busy time held by such spans, from which the capture is refused
	longNamesShown = 3    // services named in the message
)

// longService is what a service's long SERVER spans amount to.
type longService struct {
	name       string
	n          int
	longestNs  uint64
	longestKey otlp.SpanKey
	longNs     uint64  // the long spans' time
	busyNs     uint64  // all the service's SERVER spans' time
	medianNs   uint64  // the service's median SERVER span
	share      float64 // longNs / busyNs
}

// longVisits finds the SERVER spans that are no visits (see above), by service name. It reads the spans once, and again
// for the services that have one. resName gives a resource's service name (an index into nameList).
func longVisits(d *otlp.Data, resName []int32, nameList []string) []longService {
	var first, last uint64
	seen := false
	var cands []int // SERVER spans of a second or more
	for i := range d.Spans {
		sp := &d.Spans[i]
		if sp.Kind != otlp.KindServer {
			continue
		}
		if !seen || sp.Start < first {
			first = sp.Start
		}
		if !seen || sp.Start > last {
			last = sp.Start
		}
		seen = true
		if sp.End-sp.Start >= otlp.LongSpan {
			cands = append(cands, i)
		}
	}
	if len(cands) == 0 || last <= first {
		return nil
	}
	window := float64(last - first)
	byName := map[int32]bool{} // services with a span of a second or more that is a tenth of the capture's span
	for _, i := range cands {
		if sp := &d.Spans[i]; float64(sp.End-sp.Start) >= longVisitShare*window {
			byName[resName[sp.Res]] = true
		}
	}
	if len(byName) == 0 {
		return nil
	}
	durs := map[int32][]uint64{} // those services' SERVER spans' times
	for i := range d.Spans {
		if sp := &d.Spans[i]; sp.Kind == otlp.KindServer && byName[resName[sp.Res]] {
			durs[resName[sp.Res]] = append(durs[resName[sp.Res]], sp.End-sp.Start)
		}
	}
	median := map[int32]uint64{}
	busy := map[int32]uint64{}
	for id, ds := range durs {
		var sum uint64
		for _, v := range ds {
			sum += v
		}
		busy[id] = sum
		slices.Sort(ds)
		median[id] = ds[len(ds)/2]
	}
	acc := map[int32]*longService{}
	for _, i := range cands {
		sp := &d.Spans[i]
		id := resName[sp.Res]
		dur := sp.End - sp.Start
		if !byName[id] || float64(dur) < longVisitShare*window || float64(dur) < longVisitRatio*float64(median[id]) {
			continue
		}
		ls := acc[id]
		if ls == nil {
			ls = &longService{name: nameList[id], busyNs: busy[id], medianNs: median[id]}
			acc[id] = ls
		}
		ls.n++
		ls.longNs += dur
		key := otlp.SpanKey{Trace: sp.Trace, ID: sp.ID}
		if dur > ls.longestNs || (dur == ls.longestNs && lessKey(key, ls.longestKey)) {
			ls.longestNs, ls.longestKey = dur, key
		}
	}
	out := make([]longService, 0, len(acc))
	for _, ls := range acc {
		ls.share = float64(ls.longNs) / float64(ls.busyNs)
		out = append(out, *ls)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].share != out[b].share {
			return out[a].share > out[b].share
		}
		return out[a].name < out[b].name
	})
	return out
}

func lessKey(a, b otlp.SpanKey) bool {
	if a.Trace != b.Trace {
		return a.Trace[0] < b.Trace[0] || (a.Trace[0] == b.Trace[0] && a.Trace[1] < b.Trace[1])
	}
	return a.ID < b.ID
}

// describe writes what a service's long spans are: how many, how long the longest, its name when the capture kept it
// and the share of the service's busy time.
func (ls longService) describe(d *otlp.Data) string {
	which := fmt.Sprintf("trace %016x%016x", ls.longestKey.Trace[0], ls.longestKey.Trace[1])
	if n := d.LongNames[ls.longestKey]; n != "" {
		which = n + ", " + which
	}
	longest := fmt.Sprintf("%.0f ms", float64(ls.longestNs)/1e6)
	if ls.longestNs >= 1e9 {
		longest = fmt.Sprintf("%.0f s", float64(ls.longestNs)/1e9)
	}
	return fmt.Sprintf("%s: %d of its server spans last up to %s (%s) against a median of %.3g ms, and hold %.0f%% of its busy time",
		ls.name, ls.n, longest, which, float64(ls.medianNs)/1e6, 100*ls.share)
}

// longVisitsError is the refusal when long spans hold longBusyRefuse or more of a service's busy time, nil when no service
// is that far.
func longVisitsError(d *otlp.Data, found []longService) error {
	var parts []string
	n := 0
	for _, ls := range found {
		if ls.share >= longBusyRefuse {
			n++
			if len(parts) < longNamesShown {
				parts = append(parts, ls.describe(d))
			}
		}
	}
	if n == 0 {
		return nil
	}
	more := ""
	if n > len(parts) {
		more = fmt.Sprintf("; and %d more services", n-len(parts))
	}
	return fmt.Errorf("the capture holds server spans that are streams or other calls that stay open, not request visits (a "+
		"span of at least a second, a tenth of the capture's duration and %d times its service's median is taken for one): %s%s. "+
		"A visit is one request that holds a worker; read as visits, these would be most of the service's busy time, and its "+
		"mean time and utilization would be theirs. A stream that began before the window is still open in it, so cut the "+
		"capture by the time its traces started, not by the time its spans ended, or drop these spans (for example with a "+
		"filter in the Collector), and run again", longVisitRatio, strings.Join(parts, "; "), more)
}

// longVisitsWarnings are the warnings for long spans that hold less than longBusyRefuse of a service's busy time.
func longVisitsWarnings(d *otlp.Data, found []longService) []string {
	var out []string
	for _, ls := range found {
		if ls.share < longBusyRefuse {
			out = append(out, fmt.Sprintf("%s. They are read as visits, so they are in its mean time as such; if they are streams or other "+
				"calls that stay open (not requests), drop them and run again", ls.describe(d)))
		}
	}
	return out
}
