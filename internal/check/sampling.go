// SPDX-License-Identifier: Apache-2.0

package check

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// RequestCountSchema is the schema id of an independent request count.
const RequestCountSchema = "rhobound.request-count/v1"

// RequestCount is an independent count of entry requests for the capture's window (rhobound.request-count/v1,
// DATA.md 6), from a source outside the trace pipeline.
type RequestCount struct {
	Schema string `json:"schema"`
	Window struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	} `json:"window"`
	Source  string       `json:"source"`
	Entries []EntryCount `json:"entries"`

	// Digest is the content id of the file (sha256 of its bytes), recorded in the capture manifest.
	Digest string `json:"-"`
}

// EntryCount is the count of one entry service.
type EntryCount struct {
	Entry string `json:"entry"`
	Count int64  `json:"count"`
}

// countSources are the accepted sources; appsignals is accepted but not taken as independent evidence (DATA.md 6).
var countSources = map[string]bool{"alb": true, "apigw": true, "ingress": true, "loadgen": true, "appsignals": true}

// ParseRequestCount reads a request count and checks it against rhobound.request-count/v1. An invalid file is an
// error (a usage error, not a finding about the capture).
func ParseRequestCount(b []byte) (*RequestCount, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rc RequestCount
	if err := dec.Decode(&rc); err != nil {
		return nil, fmt.Errorf("request count: %w", err)
	}
	if dec.More() {
		return nil, errors.New("request count: more than one JSON value")
	}
	switch {
	case rc.Schema != RequestCountSchema:
		return nil, fmt.Errorf("request count: schema %q, want %q", rc.Schema, RequestCountSchema)
	case rc.Window.Start.IsZero() || rc.Window.End.IsZero() || rc.Window.End.Before(rc.Window.Start):
		return nil, errors.New("request count: window start and end are required, start not after end")
	case !countSources[rc.Source]:
		return nil, fmt.Errorf("request count: source %q is not alb, apigw, ingress, loadgen or appsignals", rc.Source)
	case len(rc.Entries) == 0:
		return nil, errors.New("request count: no entries")
	}
	seen := map[string]bool{}
	for _, e := range rc.Entries {
		if e.Entry == "" || e.Count < 0 || seen[e.Entry] {
			return nil, errors.New("request count: every entry is named once, with a count that is not negative")
		}
		seen[e.Entry] = true
	}
	rc.Digest = contract.Digest(b)
	return &rc, nil
}

// Thresholds of the sampling rules [D].
const (
	// countTolerance is how far the trace entries may be from an independent count and still be taken as all of them.
	countTolerance = 0.05
	// countWindowSkew is how far a count's window edges may be from the capture's (count.window_mismatch).
	countWindowSkew = 10 * time.Second
	// keptConflict is how far a declared kept share may be from a derived one (trace.kept_share_conflict).
	keptConflict = 0.05
	// thMinority is the share of the entries under which those whose th differs from the dominant one are ignored and
	// counted (trace.sampling_minority) rather than refusing the capture (trace.sampling_mixed).
	thMinority = 0.01
)

// Declared SDK samplers (OTEL_TRACES_SAMPLER values and the names of the samplers that are not): an always-on sampler
// keeps every request; the others keep a share that varies with time, which no constant can correct.
var (
	alwaysOnSamplers = map[string]bool{"always_on": true, "parentbased_always_on": true}
	varyingSamplers  = []string{"xray", "aws_xray", "tail", "adaptive", "reservoir", "rate_limit", "ratelimit", "rate-limit",
		"jaeger_remote", "parentbased_jaeger_remote"}
)

// notProportional reports whether a declared sampler keeps a share that varies with time.
func notProportional(s string) bool {
	s = strings.ToLower(s)
	for _, v := range varyingSamplers {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}

// thShare is the share a sampling threshold keeps (OpenTelemetry tracestate probability sampling): the threshold is up
// to 14 hex digits of a 56-bit rejection threshold, so th:0 keeps all, th:8 a half, th:c a quarter.
func thShare(th string) (float64, bool) {
	if th == "" || len(th) > 14 {
		return 0, false
	}
	v, err := strconv.ParseUint(th+strings.Repeat("0", 14-len(th)), 16, 64)
	if err != nil {
		return 0, false
	}
	return 1 - float64(v)/float64(uint64(1)<<56), true
}

// samplingInput is what the sampling rules read from the capture.
type samplingInput struct {
	entries   map[string]int64 // entry SERVER spans by service
	thEntries map[string]int64 // ... by their ot=th: value ("" for none)
	window    [2]time.Time     // the capture's window
}

// sampling applies the rules of DATA.md 3.8 and returns the kept share and its findings.
func sampling(in samplingInput, opt Options) (contract.KeptShare, []contract.Finding) {
	var fs []contract.Finding
	kept := contract.KeptShare{Value: 1, Source: contract.KeptAssumed}
	evidence := false // proportional-sampler evidence or an independent count

	// declared SDK samplers
	declaredAllOn := len(opt.DeclaredSamplers) > 0
	svcs := make([]string, 0, len(opt.DeclaredSamplers))
	for s := range opt.DeclaredSamplers {
		svcs = append(svcs, s)
	}
	sort.Strings(svcs)
	for _, s := range svcs {
		name := opt.DeclaredSamplers[s]
		switch {
		case notProportional(name):
			fs = append(fs, contract.NewFinding(contract.TraceSamplerNotProportional).WithSubject(s).WithExamples(name))
			declaredAllOn = false
		case !alwaysOnSamplers[strings.ToLower(name)]:
			declaredAllOn = false
		}
	}

	// the th values of the entry spans. One constant share is required, but the entries whose th differs from the
	// dominant one (a missing th counts as a value), when they are under thMinority of all, are ignored and counted:
	// under a ParentBased sampler an entry's tracestate is the caller's, so one request should not refuse a window.
	// Their th values are not listed: callers set them.
	var ths []string
	var all int64
	for th, n := range in.thEntries {
		ths = append(ths, th)
		all += n
	}
	sort.Strings(ths)
	if len(ths) > 1 {
		dominant := ths[0]
		for _, th := range ths {
			if in.thEntries[th] > in.thEntries[dominant] {
				dominant = th
			}
		}
		if rest := all - in.thEntries[dominant]; float64(rest) < thMinority*float64(all) {
			fs = append(fs, contract.NewFinding(contract.TraceSamplingMinority).WithNumber("entries_ignored", float64(rest)).
				WithNumber("thresholds", float64(len(ths))))
			ths = []string{dominant}
		}
	}
	var derived *contract.KeptShare
	switch {
	case len(ths) == 1 && ths[0] != "":
		if v, ok := thShare(ths[0]); ok {
			derived = &contract.KeptShare{Value: v, Source: contract.KeptDerivedTH}
		}
	case len(ths) > 1:
		f := contract.NewFinding(contract.TraceSamplingMixed).WithNumber("thresholds", float64(len(ths)))
		if n := in.thEntries[""]; n > 0 {
			f = f.WithNumber("entries_without_th", float64(n))
		}
		var ex []string
		for _, th := range ths {
			if th != "" {
				ex = append(ex, "th:"+th)
			}
		}
		fs = append(fs, f.WithExamples(ex...))
	}

	// the independent count
	var counted *contract.KeptShare
	if rc := opt.RequestCount; rc != nil {
		if d := math.Max(math.Abs(rc.Window.Start.Sub(in.window[0]).Seconds()), math.Abs(rc.Window.End.Sub(in.window[1]).Seconds())); d >
			countWindowSkew.Seconds() {
			fs = append(fs, contract.NewFinding(contract.CountWindowMismatch).WithNumber("seconds_apart", math.Round(d*1000)/1000))
		} else {
			var traced, count int64
			unknown := false
			for _, e := range rc.Entries {
				n, ok := in.entries[e.Entry]
				if !ok {
					fs = append(fs, contract.NewFinding(contract.CountUnknownEntry).WithSubject(e.Entry))
					unknown = true
					continue
				}
				traced += n
				count += e.Count
			}
			if !unknown && count > 0 && rc.Source != "appsignals" {
				ratio := float64(traced) / float64(count)
				if math.Abs(ratio-1) > countTolerance {
					fs = append(fs, contract.NewFinding(contract.TraceRateMismatch).WithNumber("traced_entries", float64(traced)).
						WithNumber("counted_entries", float64(count)).WithNumber("ratio", round4(ratio)))
				}
				share := 1.0
				if ratio < 1-countTolerance {
					share = ratio
				}
				counted = &contract.KeptShare{Value: round6(share), Source: contract.KeptCounted}
			}
		}
	}

	switch {
	case counted != nil:
		kept, evidence = *counted, true
	case derived != nil:
		kept, evidence = *derived, true
	case opt.DeclaredKeptShare != nil:
		kept = contract.KeptShare{Value: *opt.DeclaredKeptShare, Source: contract.KeptDeclared}
	case declaredAllOn:
		kept = contract.KeptShare{Value: 1, Source: contract.KeptDeclared}
	}
	// a declared share against a derived one
	if dk := opt.DeclaredKeptShare; dk != nil {
		for _, x := range []*contract.KeptShare{counted, derived} {
			if x != nil && math.Abs(*dk-x.Value) > keptConflict {
				fs = append(fs, contract.NewFinding(contract.TraceKeptShareConflict).WithNumber("declared", *dk).
					WithNumber(strings.ReplaceAll(x.Source, "-", "_"), x.Value))
				break
			}
		}
	}
	if counted != nil && derived != nil && math.Abs(counted.Value-derived.Value) > keptConflict {
		fs = append(fs, contract.NewFinding(contract.TraceKeptShareConflict).WithNumber("counted", counted.Value).
			WithNumber("derived_th", derived.Value))
	}
	if !evidence {
		fs = append(fs, contract.NewFinding(contract.TraceSamplingUnverified))
	}
	if kept.Value < 1 {
		fs = append(fs, contract.NewFinding(contract.FitQueueRebuildSampled).WithNumber("kept_share", kept.Value))
	}
	return kept, fs
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }
func round6(x float64) float64 { return math.Round(x*1e6) / 1e6 }
