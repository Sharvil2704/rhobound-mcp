// SPDX-License-Identifier: Apache-2.0

//go:build accuracy

package infer_test

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The accuracy harness's plumbing: seeds, the truth, the noise a capture carries, and the recorder.

func envInt(name string, def int64) int64 {
	if v := os.Getenv(name); v != "" {
		x, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			panic(fmt.Sprintf("%s=%q is not an integer", name, v))
		}
		return x
	}
	return def
}

// truthReps is the number of replications of the truth (ACC_TRUTH_REPS, default 64).
func truthReps() int {
	r := int(envInt("ACC_TRUTH_REPS", 64))
	if r < 2 || r >= seedNoise {
		panic(fmt.Sprintf("ACC_TRUTH_REPS=%d: give 2 to %d", r, seedNoise-1))
	}
	return r
}

// Seeds. math/rand reduces a seed modulo 2^31-1, so the seeds are laid out in that range: each row of a test
// owns a block of 2^16 consecutive seeds, offset 0 for its capture, 1..reps for its truth replications (the
// same seed for the base and the changed state: common random numbers) and seedNoise+j for its j-th extra
// capture (ACC_NOISE). Within one run no two (row, offset) pairs share a seed; where the blocks start depends
// on ACC_SEED, so another ACC_SEED draws other captures and truths.
const (
	seedBlock = 1 << 16
	seedNoise = 1 << 15
)

func rowSeed(accSeed int64, row, k int) int64 {
	const m = 1<<31 - 2
	if row < 0 || row >= m/seedBlock || k < 0 || k >= seedBlock {
		panic(fmt.Sprintf("row %d offset %d is outside the seed layout", row, k))
	}
	// splitmix64 of ACC_SEED: where this run's blocks start
	z := uint64(accSeed) + 0x9e3779b97f4a7c15
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	z ^= z >> 31
	return 1 + int64((z%m+uint64(row)*seedBlock+uint64(k))%m)
}

func truthSeeds(accSeed int64, row, reps int) []int64 {
	out := make([]int64, reps)
	for r := range out {
		out[r] = rowSeed(accSeed, row, 1+r)
	}
	return out
}

// rowFilter is ACC_ROWS, a comma-separated list of row indices to run (empty: all).
type rowFilter map[int]bool

func rowsFromEnv() rowFilter {
	f := rowFilter{}
	for _, p := range strings.Split(os.Getenv("ACC_ROWS"), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		i, err := strconv.Atoi(p)
		if err != nil {
			panic(fmt.Sprintf("ACC_ROWS: %q is not a row index", p))
		}
		f[i] = true
	}
	return f
}

func (f rowFilter) has(i int) bool { return len(f) == 0 || f[i] }

// parallelFor runs f(0..n-1) on GOMAXPROCS goroutines. Callers store results by index, so the output does not
// depend on the scheduling.
func parallelFor(n int, f func(i int)) {
	var next atomic.Int64
	next.Store(-1)
	var wg sync.WaitGroup
	for g := 0; g < min(n, runtime.GOMAXPROCS(0)); g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int(next.Add(1)); i < n; i = int(next.Add(1)) {
				f(i)
			}
		}()
	}
	wg.Wait()
}

// truthOut is the simulator's answer for one row: the base and the changed state run on the same seeds, each
// replication's mean latency (arrival to end) taken after its first tenth of requests.
type truthOut struct {
	Reps     int       `json:"reps"`
	Seconds  float64   `json:"seconds"`
	Seeds    []int64   `json:"seeds"`
	Base     float64   `json:"base_ms"`
	BaseSE   float64   `json:"base_se"`
	After    float64   `json:"after_ms"`
	AfterSE  float64   `json:"after_se"`
	Change   float64   `json:"change_ms"`
	ChangeSE float64   `json:"change_se"` // of the paired differences
	RepBase  []float64 `json:"rep_base_ms"`
	RepAfter []float64 `json:"rep_after_ms"`
	// Corr is each parallel group's branch correlation in the base state, measured as the inference measures
	// it (client span durations, clipped to [0, 0.95]), pooled over the replications; keyed by the group's
	// service name in topo.
	Corr map[string]float64 `json:"branch_corr,omitempty"`
}

func meanSE(xs []float64) (float64, float64) {
	m := mean(xs)
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return m, math.Sqrt(v / float64(len(xs)-1) / float64(len(xs)))
}

// truthRow runs base and after on each seed, in parallel. Only the base runs of a tree with parallel calls
// write spans (for the branch correlation); each run is reduced to its numbers at once.
func truthRow(base, after *node, lam, seconds float64, seeds []int64) truthOut {
	gs := parGroups(base)
	n := len(seeds)
	rb, ra := make([]float64, n), make([]float64, n)
	accs := make([][]corrAcc, n)
	parallelFor(n, func(r int) {
		ob := runWith(base, lam, seconds, seeds[r], simOpts{noSpans: len(gs) == 0})
		skip := len(ob.totalMs) / 10
		rb[r] = mean(ob.totalMs[skip:])
		if len(gs) > 0 {
			accs[r] = make([]corrAcc, len(gs))
			addCorr(gs, accs[r], ob.spans, skip)
		}
		oa := runWith(after, lam, seconds, seeds[r], simOpts{noSpans: true})
		ra[r] = mean(oa.totalMs[len(oa.totalMs)/10:])
	})
	tr := truthOut{Reps: n, Seconds: seconds, Seeds: seeds, RepBase: rb, RepAfter: ra}
	tr.Base, tr.BaseSE = meanSE(rb)
	tr.After, tr.AfterSE = meanSE(ra)
	d := make([]float64, n)
	for r := range d {
		d[r] = ra[r] - rb[r]
	}
	tr.Change, tr.ChangeSE = meanSE(d)
	if len(gs) > 0 {
		tr.Corr = map[string]float64{}
		for gi, g := range gs {
			var tot corrAcc
			for r := range accs {
				tot.add(&accs[r][gi])
			}
			tr.Corr[g.name] = tot.corr()
		}
	}
	return tr
}

// parGroup is one set of calls made together: a parallel node's calls, or one stage of two or more calls of a
// staged node. name is its service in topo's topology (the node, or its stage node).
type parGroup struct {
	name, caller string
	members      []string
}

func parGroups(root *node) []parGroup {
	var out []parGroup
	seen := map[*node]bool{}
	walk(root, func(n *node) {
		if seen[n] || len(n.children) < 2 {
			return
		}
		seen[n] = true
		switch {
		case n.stage != nil:
			for j := 0; j < len(n.children); {
				k := j
				for k < len(n.children) && n.stage[k] == n.stage[j] {
					k++
				}
				if k-j > 1 {
					g := parGroup{name: fmt.Sprintf("stage:%s:%d", n.name, n.stage[j]+1), caller: n.name}
					for _, c := range n.children[j:k] {
						g.members = append(g.members, c.name)
					}
					out = append(out, g)
				}
				j = k
			}
		case n.parallel:
			g := parGroup{name: n.name, caller: n.name}
			for _, c := range n.children {
				g.members = append(g.members, c.name)
			}
			out = append(out, g)
		}
	})
	return out
}

// corrAcc holds the sums of a group's branch latencies over the visits that made all of its calls.
type corrAcc struct {
	n       float64
	sum, sq []float64
	cross   [][]float64
}

func (a *corrAcc) init(k int) {
	if a.sum == nil {
		a.sum, a.sq, a.cross = make([]float64, k), make([]float64, k), make([][]float64, k)
		for i := range a.cross {
			a.cross[i] = make([]float64, k)
		}
	}
}

func (a *corrAcc) add(b *corrAcc) {
	if b.sum == nil {
		return
	}
	a.init(len(b.sum))
	a.n += b.n
	for i := range b.sum {
		a.sum[i] += b.sum[i]
		a.sq[i] += b.sq[i]
		for j := range b.cross[i] {
			a.cross[i][j] += b.cross[i][j]
		}
	}
}

// corr is the mean pairwise correlation, clipped as the inference clips it.
func (a *corrAcc) corr() float64 {
	if a.n < 50 {
		return 0
	}
	s, c := 0.0, 0
	for i := range a.sum {
		for j := i + 1; j < len(a.sum); j++ {
			vi := a.sq[i]/a.n - a.sum[i]*a.sum[i]/a.n/a.n
			vj := a.sq[j]/a.n - a.sum[j]*a.sum[j]/a.n/a.n
			if vi <= 0 || vj <= 0 {
				continue
			}
			s += (a.cross[i][j]/a.n - a.sum[i]/a.n*a.sum[j]/a.n) / math.Sqrt(vi*vj)
			c++
		}
	}
	if c == 0 {
		return 0
	}
	return math.Max(0, math.Min(s/float64(c), 0.95))
}

// addCorr adds one run's branch latencies (client span durations by callee) for the visits of requests after
// the first skip.
func addCorr(gs []parGroup, accs []corrAcc, spans []span, skip int) {
	callers := map[string]bool{}
	for _, g := range gs {
		callers[g.caller] = true
	}
	calls := map[string]map[string]float64{} // caller visit's server span id -> callee -> client duration
	for _, sp := range spans {
		if sp.kind != 3 || sp.trace <= skip || !callers[sp.svc] {
			continue
		}
		m := calls[sp.parent]
		if m == nil {
			m = map[string]float64{}
			calls[sp.parent] = m
		}
		m[sp.peer] = sp.end - sp.start
	}
	x := make([]float64, 0, 8)
	for _, sp := range spans {
		if sp.kind != 2 || sp.trace <= skip || !callers[sp.svc] {
			continue
		}
		m := calls[sp.id]
		for gi, g := range gs {
			if g.caller != sp.svc {
				continue
			}
			x = x[:0]
			for _, mb := range g.members {
				if d, ok := m[mb]; ok {
					x = append(x, d)
				}
			}
			if len(x) != len(g.members) {
				continue
			}
			a := &accs[gi]
			a.init(len(x))
			a.n++
			for i := range x {
				a.sum[i] += x[i]
				a.sq[i] += x[i] * x[i]
				for j := i + 1; j < len(x); j++ {
					a.cross[i][j] += x[i] * x[j]
				}
			}
		}
	}
}

// nodeMoments returns the mean and the second to fourth central moments of a node's service time (its random
// part plus its shift).
func nodeMoments(n *node) (mu, m2, m3, m4 float64) {
	var e [5]float64 // raw moments of the random part
	m, c := n.mean, n.cs2
	fact := [5]float64{1, 1, 2, 6, 24}
	switch n.dist {
	case "gamma":
		k, th := 1/c, m*c
		e[0] = 1
		for j := 1; j <= 4; j++ {
			e[j] = e[j-1] * th * (k + float64(j-1))
		}
	case "lognormal":
		s2 := math.Log(1 + c)
		lm := math.Log(m) - s2/2
		for j := range e {
			fj := float64(j)
			e[j] = math.Exp(fj*lm + fj*fj*s2/2)
		}
	case "h2":
		p1 := 0.5 * (1 + math.Sqrt((c-1)/(c+1)))
		a, b := m/(2*p1), m/(2*(1-p1)) // the two phases' means
		for j := range e {
			e[j] = fact[j] * (p1*math.Pow(a, float64(j)) + (1-p1)*math.Pow(b, float64(j)))
		}
	default:
		for j := range e {
			e[j] = fact[j] * math.Pow(m, float64(j))
		}
	}
	mu = e[1]
	m2 = e[2] - mu*mu
	m3 = e[3] - 3*mu*e[2] + 2*mu*mu*mu
	m4 = e[4] - 4*mu*e[3] + 6*mu*mu*e[2] - 3*mu*mu*mu*mu
	return mu + n.shift, m2, m3, m4
}

func cloneTopo(t *model.Topology) *model.Topology {
	c := &model.Topology{Name: t.Name, Entry: map[int]float64{}}
	for k, v := range t.Entry {
		c.Entry[k] = v
	}
	for _, s := range t.Services {
		s.Calls = append([]model.Call(nil), s.Calls...)
		s.Shares = append([]float64(nil), s.Shares...)
		c.Services = append(c.Services, s)
	}
	return c
}

// serverVisits counts each service's server spans.
func serverVisits(spans []span) map[string]float64 {
	v := map[string]float64{}
	for _, sp := range spans {
		if sp.kind == 2 {
			v[sp.svc]++
		}
	}
	return v
}

// inputSE is the standard error one capture's sampling noise gives the model's base latency and change at the
// root (service 0): the delta method around the true parameters tt at lam (per ms). The noisy inputs are each
// service's sample mean and cs2 over its visits in the capture (their covariance from the service law's
// moments), each call probability below 1 (binomial), each uneven split's shares (multinomial) and the request
// rate (Poisson over the window). In the simulator the estimators read these sample values exactly (section 2.2
// of docs/ACCURACY.md), so this is the noise an inferred row carries besides the truth's own. Left out: the
// worker counts and branch correlations, and the cs2 of an exponential service, which the inference snaps to 1.
func inputSE(tt *model.Topology, root *node, lam, windowMs float64, visits map[string]float64, at string, shift float64,
	cfg model.Config) (seL, seC float64, err error) {
	ai := tt.Index(at)
	eval := func(tp *model.Topology, l float64) (float64, float64, error) {
		if err := tp.Finish(); err != nil {
			return 0, 0, err
		}
		ds := make([]float64, len(tp.Services))
		sh := make([]bool, len(ds))
		ds[ai], sh[ai] = shift, true
		d, err := model.ChangeShiftWith(tp, l, ds, sh, cfg)
		if err != nil {
			return 0, 0, err
		}
		if !d.Stable {
			return 0, 0, fmt.Errorf("unstable")
		}
		return d.Base.L[0], d.DL[0], nil
	}
	type coord struct {
		h   float64
		set func(tp *model.Topology, l *float64, d float64)
	}
	type block struct {
		cs  []coord
		cov [][]float64
	}
	blocks := []block{{[]coord{{1e-3 * lam, func(_ *model.Topology, l *float64, d float64) { *l += d }}}, [][]float64{{lam / windowMs}}}}
	seen := map[*node]bool{}
	walk(root, func(n *node) {
		i := tt.Index(n.name)
		nv := visits[n.name]
		if seen[n] || i < 0 || nv < 2 {
			return
		}
		seen[n] = true
		sv := tt.Services[i]
		M, m2, m3, m4 := nodeMoments(n)
		vm, vs2, cms2 := m2/nv, (m4-m2*m2)/nv, m3/nv // the sample mean's and variance's variances, their covariance
		setS := coord{1e-3 * M, func(tp *model.Topology, _ *float64, d float64) { tp.Services[i].S += d }}
		if (n.dist == "" || n.dist == "exp") && n.shift == 0 {
			blocks = append(blocks, block{[]coord{setS}, [][]float64{{vm}}})
		} else {
			// cs2 = variance / mean^2, to first order
			vc := vs2/math.Pow(M, 4) + 4*m2*m2*vm/math.Pow(M, 6) - 4*m2*cms2/math.Pow(M, 5)
			cmc := cms2/(M*M) - 2*m2*vm/(M*M*M)
			setC := coord{1e-3 * sv.CS2, func(tp *model.Topology, _ *float64, d float64) { tp.Services[i].CS2 += d }}
			blocks = append(blocks, block{[]coord{setS, setC}, [][]float64{{vm, cmc}, {cmc, vc}}})
		}
		if k := len(sv.Shares); k >= 2 {
			var b block
			for a := 0; a < k-1; a++ {
				a := a
				b.cs = append(b.cs, coord{1e-4, func(tp *model.Topology, _ *float64, d float64) {
					tp.Services[i].Shares[a] += d
					tp.Services[i].Shares[k-1] -= d
				}})
				row := make([]float64, k-1)
				for c := range row {
					row[c] = -sv.Shares[a] * sv.Shares[c] / nv
					if c == a {
						row[c] += sv.Shares[a] / nv
					}
				}
				b.cov = append(b.cov, row)
			}
			blocks = append(blocks, b)
		}
		for ci, c := range sv.Calls {
			if c.P < 1 {
				ci := ci
				blocks = append(blocks, block{[]coord{{1e-3 * math.Min(c.P, 1-c.P), func(tp *model.Topology, _ *float64, d float64) {
					tp.Services[i].Calls[ci].P += d
				}}}, [][]float64{{c.P * (1 - c.P) / nv}}})
			}
		}
	})
	vL, vC := 0.0, 0.0
	for _, b := range blocks {
		gL, gC := make([]float64, len(b.cs)), make([]float64, len(b.cs))
		for a, c := range b.cs {
			var f [2][2]float64
			for si, sg := range []float64{1, -1} {
				tp, l := cloneTopo(tt), lam
				c.set(tp, &l, sg*c.h)
				if f[si][0], f[si][1], err = eval(tp, l); err != nil {
					return math.NaN(), math.NaN(), err
				}
			}
			gL[a], gC[a] = (f[0][0]-f[1][0])/(2*c.h), (f[0][1]-f[1][1])/(2*c.h)
		}
		for a := range b.cs {
			for c := range b.cs {
				vL += gL[a] * gL[c] * b.cov[a][c]
				vC += gC[a] * gC[c] * b.cov[a][c]
			}
		}
	}
	return math.Sqrt(math.Max(vL, 0)), math.Sqrt(math.Max(vC, 0)), nil
}

// zOf is an error over its standard error (NaN when that is 0 or unknown).
func zOf(err, se1, se2 float64) float64 {
	s := math.Hypot(se1, se2)
	if !(s > 0) {
		return math.NaN()
	}
	return err / s
}

// shareOver is the share (in %) of the finite values with |z| > 3, and how many are finite.
func shareOver(zs []float64) (float64, int) {
	n, k := 0, 0
	for _, z := range zs {
		if math.IsNaN(z) {
			continue
		}
		n++
		if math.Abs(z) > 3 {
			k++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return 100 * float64(k) / float64(n), n
}

// nodeRecord is one node's realized sample in a capture, over every visit (the set the inference reads).
type nodeRecord struct {
	Visits   int     `json:"visits"`
	SMean    float64 `json:"s_mean"`
	SCS2     float64 `json:"s_cs2"`
	WaitMean float64 `json:"wait_mean"`
	PWait    float64 `json:"p_wait"`
	// the node's true values, for comparison
	TrueS   float64 `json:"true_s"`
	TrueCS2 float64 `json:"true_cs2"`
}

// captureRecorder collects nodeRecords through the simulator's recorder hook.
type captureRecorder struct {
	by map[string]*recSums
}

type recSums struct {
	s, s2, w float64 // sums of the service times, their squares and the waits
	n, nw    int     // visits, and those that waited
}

func newCaptureRecorder() *captureRecorder { return &captureRecorder{by: map[string]*recSums{}} }

func (r *captureRecorder) hook(n *node, arr, start, ownEnd, end []float64, podOf, traces []int) {
	a := r.by[n.name]
	if a == nil {
		a = &recSums{}
		r.by[n.name] = a
	}
	for i := range arr {
		x, w := ownEnd[i]-start[i], start[i]-arr[i]
		a.s, a.s2, a.w, a.n = a.s+x, a.s2+x*x, a.w+w, a.n+1
		if w > 1e-9 {
			a.nw++
		}
	}
}

func (r *captureRecorder) records(root *node) map[string]nodeRecord {
	out := map[string]nodeRecord{}
	walk(root, func(n *node) {
		a := r.by[n.name]
		if a == nil || a.n < 2 {
			return
		}
		k := float64(a.n)
		m := a.s / k
		mu, m2, _, _ := nodeMoments(n)
		out[n.name] = nodeRecord{Visits: a.n, SMean: m, SCS2: (a.s2 - k*m*m) / (k - 1) / (m * m), WaitMean: a.w / k,
			PWait: float64(a.nw) / k, TrueS: mu, TrueCS2: m2 / (mu * mu)}
	})
	return out
}

// toAny turns v into maps, slices and scalars with NaN and Inf as null (encoding/json refuses them).
func toAny(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return toAny(v.Elem())
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
		return nil
	case reflect.Struct:
		m := map[string]any{}
		tp := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := tp.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag == "-" {
				continue
			} else if tag != "" {
				name = tag
			}
			m[name] = toAny(v.Field(i))
		}
		return m
	case reflect.Map:
		m := map[string]any{}
		for _, k := range v.MapKeys() {
			m[fmt.Sprint(k.Interface())] = toAny(v.MapIndex(k))
		}
		return m
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil
		}
		out := make([]any, v.Len())
		for i := range out {
			out[i] = toAny(v.Index(i))
		}
		return out
	}
	return v.Interface()
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(toAny(reflect.ValueOf(v)), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTopo(t *testing.T, path string, tp *model.Topology) {
	t.Helper()
	b, err := model.MarshalTopology(tp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dumpRow is the recorder (ACC_DUMP=DIR): it writes one row's capture as the CLI reads it (DIR/<row>/capture),
// the true topologies of both states with the measured branch correlation, the inferred topology, and the
// truth, the capture's realized per-node sample and the scored numbers (truth.json).
func dumpRow(t *testing.T, dir string, spans []span, base, after *node, corr map[string]float64, inf *model.Topology,
	info map[string]any) {
	t.Helper()
	if err := writeOTLPTo(filepath.Join(dir, "capture"), spans); err != nil {
		t.Fatal(err)
	}
	writeTopo(t, filepath.Join(dir, "true_base.json"), topo(base, corr))
	writeTopo(t, filepath.Join(dir, "true_after.json"), topo(after, corr))
	writeTopo(t, filepath.Join(dir, "inferred.json"), inf)
	var nodes []map[string]any
	walk(base, func(n *node) {
		var kids []string
		for _, c := range n.children {
			kids = append(kids, c.name)
		}
		nodes = append(nodes, map[string]any{"name": n.name, "c": n.c, "pods": n.pods, "mean": n.mean, "dist": n.dist,
			"cs2_param": n.cs2, "post": n.post, "parallel": n.parallel, "stage": n.stage, "p": n.p, "shares": n.shares,
			"children": kids})
	})
	info["nodes"] = nodes
	writeJSON(t, filepath.Join(dir, "truth.json"), info)
}

// readCapture writes spans as a capture and reads it back, then removes the files (a 400 s capture is
// about 16 MB). Safe from any goroutine.
func readCapture(t *testing.T, spans []span) (*otlp.Data, error) {
	dir := t.TempDir()
	defer os.RemoveAll(dir)
	if err := writeOTLPTo(dir, spans); err != nil {
		return nil, err
	}
	return otlp.ReadDir(dir)
}

// TestHarnessPlumbing checks the harness's own arithmetic: the seed layout gives every (row, offset) of a run
// its own effective seed, the service laws' moments match a large sample, and the change truth of a zero
// change is exactly zero (common random numbers).
//
//	go test -tags accuracy -run TestHarnessPlumbing -v ./internal/infer
func TestHarnessPlumbing(t *testing.T) {
	for _, acc := range []int64{31337, 1, 2, -5, 1 << 40} {
		seen := map[int64]bool{}
		for row := 0; row < 400; row++ {
			for _, k := range []int{0, 1, 2, 64, 1000, seedNoise, seedNoise + 15, seedBlock - 1} {
				s := rowSeed(acc, row, k) % (1<<31 - 1) // math/rand's own reduction
				if s <= 0 || seen[s] {
					t.Fatalf("seed %d row %d offset %d: effective seed %d repeats or is not positive", acc, row, k, s)
				}
				seen[s] = true
			}
		}
	}
	rng := rand.New(rand.NewSource(99))
	for _, n := range []*node{{mean: 20}, {mean: 14, shift: 6}, {mean: 20, dist: "gamma", cs2: 0.35}, {mean: 20, dist: "h2", cs2: 3},
		{mean: 20, dist: "lognormal", cs2: 0.6}} {
		mu, m2, m3, _ := nodeMoments(n)
		var s1, s2, s3 float64
		const N = 2_000_000
		for i := 0; i < N; i++ {
			x := n.draw(rng)
			s1 += x
		}
		rng = rand.New(rand.NewSource(99))
		m := s1 / N
		for i := 0; i < N; i++ {
			d := n.draw(rng) - m
			s2, s3 = s2+d*d, s3+d*d*d
		}
		rng = rand.New(rand.NewSource(99))
		within(t, fmt.Sprintf("%s %v mean", n.dist, n.cs2), m, mu, 0.005)
		within(t, fmt.Sprintf("%s %v variance", n.dist, n.cs2), s2/N, m2, 0.02)
		within(t, fmt.Sprintf("%s %v third moment", n.dist, n.cs2), s3/N, m3, 0.15)
	}
	b := &node{name: "b", c: 1, pods: 1, mean: 20}
	a := &node{name: "a", c: 2, pods: 1, mean: 3, parallel: true, children: []*node{b, {name: "c", c: 1, pods: 1, mean: 18}}}
	tr := truthRow(a, cloneTree(a), 30, 100, truthSeeds(7, 0, 4))
	if tr.Change != 0 || tr.ChangeSE != 0 || tr.Base != tr.After {
		t.Errorf("a zero change: truth change %v (se %v), base %v after %v", tr.Change, tr.ChangeSE, tr.Base, tr.After)
	}
	if r := tr.Corr["a"]; !(r > 0.05 && r < 0.6) {
		t.Errorf("branch correlation of a fan-out of two queues fed the same requests: %v", r)
	}
}
