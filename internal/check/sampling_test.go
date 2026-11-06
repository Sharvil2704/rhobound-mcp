// SPDX-License-Identifier: Apache-2.0

package check

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

func TestTHShare(t *testing.T) {
	for th, want := range map[string]float64{"0": 1, "8": 0.5, "c": 0.25, "f": 1.0 / 16, "fffffffffffffe": 2.0 / (1 << 56)} {
		got, ok := thShare(th)
		if !ok || math.Abs(got-want) > 1e-15 {
			t.Errorf("th:%s keeps %v (%v), want %v", th, got, ok, want)
		}
	}
	for _, bad := range []string{"", "zz", "123456789012345"} {
		if _, ok := thShare(bad); ok {
			t.Errorf("th:%q read as a share", bad)
		}
	}
}

var (
	w0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	w1 = w0.Add(10 * time.Minute)
)

func countDoc(edit func(m map[string]any)) []byte {
	m := map[string]any{"schema": RequestCountSchema, "source": "alb",
		"window":  map[string]string{"start": w0.Format(time.RFC3339), "end": w1.Format(time.RFC3339)},
		"entries": []map[string]any{{"entry": "frontend", "count": 1000}}}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func TestParseRequestCount(t *testing.T) {
	rc, err := ParseRequestCount(countDoc(nil))
	if err != nil || rc.Entries[0].Count != 1000 || !strings.HasPrefix(rc.Digest, "sha256:") {
		t.Fatalf("%+v %v", rc, err)
	}
	for name, edit := range map[string]func(map[string]any){
		"schema":    func(m map[string]any) { m["schema"] = "rhobound.request-count/v2" },
		"source":    func(m map[string]any) { m["source"] = "span-metrics" },
		"no window": func(m map[string]any) { delete(m, "window") },
		"inverted": func(m map[string]any) {
			m["window"] = map[string]string{"start": w1.Format(time.RFC3339), "end": w0.Format(time.RFC3339)}
		},
		"empty":    func(m map[string]any) { m["entries"] = []any{} },
		"negative": func(m map[string]any) { m["entries"] = []map[string]any{{"entry": "x", "count": -1}} },
		"twice": func(m map[string]any) {
			m["entries"] = []map[string]any{{"entry": "x", "count": 1}, {"entry": "x", "count": 2}}
		},
		"unknown key": func(m map[string]any) { m["bucket"] = "x" },
	} {
		if _, err := ParseRequestCount(countDoc(edit)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func codes(fs []contract.Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return strings.Join(out, " ")
}

// The rules of DATA.md 3.8, one row each.
func TestSamplingRules(t *testing.T) {
	in := func(entries int64, ths map[string]int64) samplingInput {
		if ths == nil {
			ths = map[string]int64{"": entries}
		}
		return samplingInput{entries: map[string]int64{"frontend": entries}, thEntries: ths, window: [2]time.Time{w0, w1}}
	}
	count := func(n int64, edit func(rc *RequestCount)) *RequestCount {
		rc, err := ParseRequestCount(countDoc(func(m map[string]any) {
			m["entries"] = []map[string]any{{"entry": "frontend", "count": n}}
		}))
		if err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(rc)
		}
		return rc
	}
	half := 0.5
	for _, c := range []struct {
		name  string
		in    samplingInput
		opt   Options
		kept  contract.KeptShare
		codes string
	}{
		{"nothing to go by", in(1000, nil), Options{}, contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingUnverified},
		{"a count that agrees", in(980, nil), Options{RequestCount: count(1000, nil)}, contract.KeptShare{Value: 1, Source: "counted"}, ""},
		{"a count twice the entries", in(500, nil), Options{RequestCount: count(1000, nil)}, contract.KeptShare{Value: 0.5, Source: "counted"},
			contract.TraceRateMismatch + " " + contract.FitQueueRebuildSampled},
		{"more entries than counted", in(1500, nil), Options{RequestCount: count(1000, nil)}, contract.KeptShare{Value: 1, Source: "counted"},
			contract.TraceRateMismatch},
		{"a count of another window", in(1000, nil), Options{RequestCount: count(1000, func(rc *RequestCount) {
			rc.Window.Start = w0.Add(30 * time.Second)
		})}, contract.KeptShare{Value: 1, Source: "assumed"}, contract.CountWindowMismatch + " " + contract.TraceSamplingUnverified},
		{"a count of an entry the capture lacks", in(1000, nil), Options{RequestCount: count(1000, func(rc *RequestCount) {
			rc.Entries = append(rc.Entries, EntryCount{Entry: "admin", Count: 5})
		})}, contract.KeptShare{Value: 1, Source: "assumed"}, contract.CountUnknownEntry + " " + contract.TraceSamplingUnverified},
		{"Application Signals' count is not independent", in(500, nil), Options{RequestCount: count(1000, func(rc *RequestCount) {
			rc.Source = "appsignals"
		})}, contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingUnverified},
		{"one threshold on every entry", in(1000, map[string]int64{"c": 1000}), Options{}, contract.KeptShare{Value: 0.25, Source: "derived-th"},
			contract.FitQueueRebuildSampled},
		{"thresholds that differ", in(1000, map[string]int64{"c": 500, "8": 500}), Options{},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingMixed + " " + contract.TraceSamplingUnverified},
		{"a threshold on some entries", in(1000, map[string]int64{"c": 500, "": 500}), Options{},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingMixed + " " + contract.TraceSamplingUnverified},
		// under 1% of the entries with a th that differs from the rest's is ignored and counted; 1% or more refuses
		{"a threshold on one entry in a thousand", in(1000, map[string]int64{"8": 1, "": 999}), Options{},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingMinority + " " + contract.TraceSamplingUnverified},
		{"no threshold on one entry in a thousand", in(1000, map[string]int64{"": 1, "c": 999}), Options{},
			contract.KeptShare{Value: 0.25, Source: "derived-th"}, contract.TraceSamplingMinority + " " + contract.FitQueueRebuildSampled},
		{"two thresholds on a few entries", in(1000, map[string]int64{"8": 4, "4": 5, "c": 991}), Options{},
			contract.KeptShare{Value: 0.25, Source: "derived-th"}, contract.TraceSamplingMinority + " " + contract.FitQueueRebuildSampled},
		{"a threshold on one entry in a hundred", in(1000, map[string]int64{"8": 10, "": 990}), Options{},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingMixed + " " + contract.TraceSamplingUnverified},
		{"a threshold on one entry of two", in(2, map[string]int64{"8": 1, "": 1}), Options{},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplingMixed + " " + contract.TraceSamplingUnverified},
		{"declared always-on samplers", in(1000, nil), Options{DeclaredSamplers: map[string]string{"frontend": "parentbased_always_on"}},
			contract.KeptShare{Value: 1, Source: "declared"}, contract.TraceSamplingUnverified},
		{"a declared X-Ray sampler", in(1000, nil), Options{DeclaredSamplers: map[string]string{"frontend": "xray"}},
			contract.KeptShare{Value: 1, Source: "assumed"}, contract.TraceSamplerNotProportional + " " + contract.TraceSamplingUnverified},
		{"a declared kept share", in(1000, nil), Options{DeclaredKeptShare: &half}, contract.KeptShare{Value: 0.5, Source: "declared"},
			contract.TraceSamplingUnverified + " " + contract.FitQueueRebuildSampled},
		{"a declared share against a counted one", in(1000, nil), Options{DeclaredKeptShare: &half, RequestCount: count(1000, nil)},
			contract.KeptShare{Value: 1, Source: "counted"}, contract.TraceKeptShareConflict},
		{"a counted share against the threshold's", in(1000, map[string]int64{"c": 1000}), Options{RequestCount: count(1000, nil)},
			contract.KeptShare{Value: 1, Source: "counted"}, contract.TraceKeptShareConflict},
	} {
		kept, fs := sampling(c.in, c.opt)
		if kept != c.kept || codes(fs) != c.codes {
			t.Errorf("%s: kept %+v, %q; want %+v, %q", c.name, kept, codes(fs), c.kept, c.codes)
		}
	}
}
