// SPDX-License-Identifier: Apache-2.0

package infer

// The split of a service's visits over its pods. The model's random routing
// splits traffic evenly, which is what a Kubernetes Service does per
// connection; with clients that keep their connections alive (most HTTP and
// gRPC clients) the split is per connection, and a few busy connections can
// hold it uneven for minutes. The busier pod then queues more than an even
// split predicts. Each pod's share of the visits is measured over the window
// and tested against an even random split; when the test rules that out, the
// model is given the measured shares (model.Service.Shares).

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/mathx"
)

// PodShare is one pod's share of its service's visits.
type PodShare struct {
	Pod    string  `json:"pod"`
	Visits int64   `json:"visits"`
	Share  float64 `json:"share"`
}

// splitP is the p-value below which a split is called uneven: an even random
// split is flagged about once in a million services (for two pods, at 4.9
// standard deviations). There is no floor on how uneven: a split that is
// significant but slight is modelled as measured, which changes little.
const splitP = 1e-6

// splitMinExpected is the fewest visits per pod an even split must expect for
// the test to be read: the chi-square law approximates the counts' law, and
// in its far tail only for large counts.
const splitMinExpected = 20

// evenSplitP returns the p-value of visit counts per pod under an even random
// split (every visit to a pod picked uniformly at random): Pearson's
// chi-square with len(n)-1 degrees of freedom. ok is false when the test is
// not read: fewer than two pods, or fewer than splitMinExpected visits
// expected per pod.
func evenSplitP(n []int64) (p float64, ok bool) {
	if len(n) < 2 {
		return 0, false
	}
	var tot int64
	for _, x := range n {
		tot += x
	}
	e := float64(tot) / float64(len(n))
	if e < splitMinExpected {
		return 0, false
	}
	chi2 := 0.0
	for _, x := range n {
		d := float64(x) - e
		chi2 += d * d / e
	}
	return math.Max(0, 1-mathx.GammaP(float64(len(n)-1)/2, chi2/2)), true
}

// podSplit returns the pods' shares of a service's visits, by pod name, the
// p-value of the even-split test (nil when it is not read), and the shares for
// the model when the test rules an even split out (nil otherwise).
func podSplit(pods map[int32]bool, podName []string, podVisits []int64) (by []PodShare, p *float64, shares []float64) {
	ids := make([]int32, 0, len(pods))
	for id := range pods {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if podName[ids[a]] != podName[ids[b]] {
			return podName[ids[a]] < podName[ids[b]]
		}
		return ids[a] < ids[b]
	})
	n := make([]int64, len(ids))
	var tot int64
	for k, id := range ids {
		n[k] = podVisits[id]
		tot += n[k]
	}
	if tot == 0 {
		return nil, nil, nil
	}
	for k, id := range ids {
		by = append(by, PodShare{Pod: podName[id], Visits: n[k], Share: float64(n[k]) / float64(tot)})
	}
	pv, ok := evenSplitP(n)
	if !ok {
		return by, nil, nil
	}
	if pv < splitP {
		shares = make([]float64, len(by))
		for k := range by {
			shares[k] = by[k].Share
		}
	}
	return by, &pv, shares
}

// splitWarning describes an uneven split and how it is modelled.
func splitWarning(name string, by []PodShare, p float64) string {
	parts := make([]string, len(by))
	for k, s := range by {
		parts[k] = fmt.Sprintf("%s %.1f%%", s.Pod, 100*s.Share)
	}
	pv := fmt.Sprintf("p = %.1g", p)
	if p < 1e-12 {
		pv = "p < 1e-12"
	}
	return fmt.Sprintf("%s: its %d pods took uneven shares of its visits: %s (an even random split, which the model "+
		"would assume, gives this with %s). A Kubernetes Service picks a pod per connection, so clients that keep "+
		"their connections alive can hold such a split (so does a pod that was up for only part of the window, "+
		"which the model does not cover). Each pod is modelled as a queue fed its measured share, and the change "+
		"prediction keeps these shares", name, len(by), strings.Join(parts, ", "), pv)
}
