// SPDX-License-Identifier: Apache-2.0

// Package abdiff is the A/B record (rhobound.ab-record/v1, docs/mcp/schemas/ab-record.v1.json; docs/mcp/DATA.md 8):
// the per-service difference between the two arms of a concurrent A/B run, the image production runs now (the
// baseline) and the candidate, each a capture run through check and infer, with standard errors by batch means and the
// checks that decide whether the own-time difference may be applied alone.
//
// It is the first measured input: a change set item labelled measured-ab cites a record by its digest, and the CLI
// accepts the label only when it finds that record in its own record store and the item's value is the record's
// delta; the item then enters the prediction as the delta +-2 standard errors. Like the fit record, a record is its
// bytes: Marshal gives its canonical JSON, `diff-fit --out DIR` writes exactly those bytes to DIR/ab-record.json, and
// Digest is their sha256, the digest the MCP server stores the record under and cites as evidence.
package abdiff

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
)

// Schema is the schema id of an A/B record.
const Schema = "rhobound.ab-record/v1"

// File is the name of the record in the folder `rhobound diff-fit --out DIR` writes.
const File = "ab-record.json"

// DefaultBatches is the number of batches the standard errors are computed over [D]; SEMethod records it.
const DefaultBatches = 30

// Record is one A/B record. Services and Findings are never nil in a record Diff made.
type Record struct {
	Schema string   `json:"schema"`
	Tool   fit.Tool `json:"tool"`
	Arms   Arms     `json:"arms"`
	// OverlapShare is the share of the windows that overlap; below 0.9 is ab.not_concurrent.
	OverlapShare float64 `json:"overlap_share"`
	// SEMethod is how the standard errors were computed: "batch-means/<batches>".
	SEMethod string `json:"se_method"`
	// Loadgen is the load generator's own measurement of each arm, from outside the system under test.
	Loadgen Loadgen `json:"loadgen"`
	// SelfReportConsistent is true when the client-side latency difference agrees with the span-derived one
	// (profile.self_report_mismatch otherwise).
	SelfReportConsistent bool               `json:"self_report_consistent"`
	Services             []Service          `json:"services"`
	Findings             []contract.Finding `json:"findings"`
}

// Arms are the two arms of the run.
type Arms struct {
	Baseline  Arm `json:"baseline"`
	Candidate Arm `json:"candidate"`
}

// Arm is what the record says of one arm.
type Arm struct {
	// Capture is the arm's registered capture name.
	Capture        string `json:"capture"`
	ManifestDigest string `json:"manifest_digest"`
	// FitDigest is the digest of the arm's fit record, when one was made, or "".
	FitDigest string `json:"fit_digest,omitempty"`
	Window    Window `json:"window"`
	// Images are the image digests by service, from pod metadata in the spans (arm identity never comes from the
	// application). Never nil.
	Images    map[string][]string `json:"images"`
	KeptShare contract.KeptShare  `json:"kept_share"`
}

// Window is an arm's window, in UTC.
type Window struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Loadgen is the load generator's measurement: the sha256 of its per-request log, the requests it sent to each arm
// and the client-side latency of each.
type Loadgen struct {
	LogSHA256       string  `json:"log_sha256"`
	Requests        PairInt `json:"requests"`
	ClientLatencyMs Delta   `json:"client_latency_ms"`
	// ResponseBytes is the mean size of a response body in each arm, bytes, when the log has its bytes column; nil
	// otherwise. A change of more than PayloadGap is ab.payload_changed: a cost the callers pay (reading, decoding) that
	// no arm's spans contain.
	ResponseBytes *Pair `json:"response_bytes,omitempty"`
}

// Service is the difference measured for one service.
type Service struct {
	Service string  `json:"service"`
	Visits  PairInt `json:"visits"`
	// VisitsNeeded is the visits per arm the sizing rule needs for the gate's margin (ab.too_few_visits below it), or
	// 0 when it was not computed.
	VisitsNeeded int64 `json:"visits_needed,omitempty"`
	OwnMs        Delta `json:"own_ms"`
	GlueMs       Delta `json:"glue_ms"`
	OwnCS2       Delta `json:"own_cs2"`
	// OwnVar is the own time's variance in each arm, ms^2, and its difference with the standard error by batch means:
	// the evidence of a measured own_var_ms2 item, which the engine applies with the own-time difference.
	OwnVar Delta `json:"own_var"`
	// Calls are the calls per visit to each callee, in each arm. Never nil.
	Calls []Calls `json:"calls"`
	// CallsChanged is profile.structure_changed: the calls per visit differ, the change is structural, and the
	// own-time difference is never applied alone.
	CallsChanged bool `json:"calls_changed"`
	// VarianceChanged is profile.variance_changed: the own time's variance differs beyond 2 standard errors (by batch
	// means), which makes a result inconclusive until the engine has a variance patch. The variance, not the cs2: an
	// own-time difference applied as a shift keeps the variance and lowers the cs2, so a pure shift changes the cs2
	// and is no variance change.
	VarianceChanged bool `json:"variance_changed"`
	ErrorShare      Pair `json:"error_share"`
	// Routes are the own-time differences of the service's routes (infer.ServiceStats.Routes), for a service both arms
	// read two or more common routes of; omitted otherwise, so that a record of single-route services keeps its bytes.
	// With them, VarianceChanged tests the candidate's variance against the variance each route's own shift predicts
	// (the routes' variances kept, their means moved by their deltas, mixed by the candidate's shares), not against the
	// baseline's: shifting one route of a mixture moves the blended variance without changing any route's spread.
	Routes []Route `json:"routes,omitempty"`
}

// Route is the own-time difference of one route of a service.
type Route struct {
	Route  string  `json:"route"`
	Visits PairInt `json:"visits"`
	// Share is the route's share of the service's visits in each arm.
	Share Pair  `json:"share"`
	OwnMs Delta `json:"own_ms"`
}

// Calls are the calls per visit to one callee in each arm, and the standard error of each mean from the spread of the
// calls per visit over the visits (sd/sqrt(n), not batch means: the counts are kept as a histogram, not in order).
type Calls struct {
	Callee      string  `json:"callee"`
	Baseline    float64 `json:"baseline"`
	Candidate   float64 `json:"candidate"`
	BaselineSE  float64 `json:"baseline_se"`
	CandidateSE float64 `json:"candidate_se"`
}

// Ratio is the candidate's calls per visit over the baseline's and its standard error (by the delta method, the arms
// independent); ok is false when the baseline makes no call.
func (c Calls) Ratio() (ratio, se float64, ok bool) {
	if !(c.Baseline > 0) {
		return 0, 0, false
	}
	ratio = c.Candidate / c.Baseline
	se = math.Hypot(c.CandidateSE/c.Baseline, ratio*c.BaselineSE/c.Baseline)
	return ratio, se, true
}

// Pair is one value per arm.
type Pair struct {
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
}

// PairInt is one count per arm.
type PairInt struct {
	Baseline  int64 `json:"baseline"`
	Candidate int64 `json:"candidate"`
}

// Delta is a value in each arm, their difference (candidate minus baseline) and the difference's standard error, by
// the record's SEMethod. A measured item applies Delta +- 2 SE.
type Delta struct {
	Baseline  float64 `json:"baseline"`
	Candidate float64 `json:"candidate"`
	Delta     float64 `json:"delta"`
	SE        float64 `json:"se"`
}

// Marshal returns the record's canonical JSON, with no trailing newline: the bytes `diff-fit --out` writes.
func (r *Record) Marshal() ([]byte, error) {
	return contract.Canonical(r)
}

// Digest is the record's content id, contract.Digest of Marshal's bytes. A record that does not serialize (a
// non-finite number in it) is a programming error and panics.
func (r *Record) Digest() string {
	b, err := r.Marshal()
	if err != nil {
		panic(fmt.Sprintf("abdiff: digest of a record that does not serialize: %v", err))
	}
	return contract.Digest(b)
}

// ErrVersion is the error Unmarshal returns, wrapped, for a record of another schema id or one whose fixed fields are
// not what this version writes; the CLI refuses it with the code fit.version.
var ErrVersion = errors.New("not a " + Schema + " record")

// Unmarshal reads a record. A key the schema does not define is an error; another schema id, or fixed fields that are
// not what this version writes (Check), an error wrapping ErrVersion.
func Unmarshal(b []byte) (*Record, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var r Record
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("abdiff: %v", err)
	}
	if dec.More() {
		return nil, errors.New("abdiff: more than one JSON value")
	}
	if r.Schema != Schema {
		return nil, fmt.Errorf("abdiff: %w: the record's schema is %q", ErrVersion, r.Schema)
	}
	if err := r.Check(); err != nil {
		return nil, fmt.Errorf("abdiff: %w: %v", ErrVersion, err)
	}
	return &r, nil
}

var seMethodRE = regexp.MustCompile(`^batch-means/[0-9]+$`)

var sha256HexRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Check reports why a record's fixed fields are not what this version writes, or nil: the tool, the standard errors'
// method, each arm's digests and kept share, the load generator log's digest, and every finding
// (contract.CheckStored). A record may come from another build of the server or its worker, and these fields reach
// callers as they are, so they are checked, never taken as written; names (the arms' captures, services, callees)
// reach a caller only through the name rule.
func (r *Record) Check() error {
	if err := r.Tool.Check(); err != nil {
		return err
	}
	if !seMethodRE.MatchString(r.SEMethod) {
		return errors.New("the standard errors' method is not batch-means/<n>")
	}
	for _, a := range []Arm{r.Arms.Baseline, r.Arms.Candidate} {
		if !fit.CheckDigest(a.ManifestDigest) || a.FitDigest != "" && !fit.CheckDigest(a.FitDigest) {
			return errors.New("an arm's digest is not a digest")
		}
		if err := fit.CheckKeptShare(a.KeptShare); err != nil {
			return err
		}
	}
	if r.Loadgen.LogSHA256 != "" && !sha256HexRE.MatchString(r.Loadgen.LogSHA256) {
		return errors.New("the load generator log's digest is not a sha256")
	}
	for _, f := range r.Findings {
		if err := contract.CheckStored(f); err != nil {
			return err
		}
	}
	return nil
}

// Input is one arm as Diff reads it: a capture run through check, and its inference made with the per-visit samples
// kept (infer.Options.KeepSamples: own time, glue and calls per callee, in order of arrival), which the batch-means
// standard errors need.
type Input struct {
	// Capture is the arm's registered capture name (Arm.Capture).
	Capture  string
	Manifest *contract.Manifest
	Check    *contract.CheckReport
	Inferred *infer.Inferred
	// FitDigest is the digest of the arm's fit record, when one was made, or "".
	FitDigest string
	// Images are the image digests by service the arm's resources reported (fit.Images).
	Images map[string][]string
	// Hardware describes the machines the arm ran on (instance types, architectures and container resources, from the
	// cluster dump), or "" when unknown; two known descriptions that differ are ab.hardware_differs.
	Hardware string
}

// LoadgenLog is the load generator's per-request log (DATA.md 8): one row per request, with the arm it went to.
type LoadgenLog struct {
	// SHA256 is the bare hex sha256 of the log file (Loadgen.LogSHA256).
	SHA256 string
	Rows   []LoadgenRow
	// HasBytes is true for a log with the bytes column (LoadgenHeaderBytes).
	HasBytes bool
}

// LoadgenRow is one request of the log: arm,seq,sent_unix_ns,answered_unix_ns,status[,bytes].
type LoadgenRow struct {
	Arm            string // "A" (the baseline) or "B" (the candidate)
	Seq            int64
	SentUnixNs     int64
	AnsweredUnixNs int64
	Status         int
	// Bytes is the size of the response body the load generator read, or -1 when the log has no bytes column.
	Bytes int64
}

// LoadgenHeader is the header line of a load generator's log; LoadgenHeaderBytes that of a log that also gives each
// response body's size in bytes.
const (
	LoadgenHeader      = "arm,seq,sent_unix_ns,answered_unix_ns,status"
	LoadgenHeaderBytes = LoadgenHeader + ",bytes"
)

// ParseLoadgenLog reads a load generator's log in the CSV form of DATA.md 8, with its header line: LoadgenHeader, or
// LoadgenHeaderBytes for a log whose rows also give the response body's size.
func ParseLoadgenLog(b []byte) (*LoadgenLog, error) {
	r := csv.NewReader(bytes.NewReader(b))
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	head, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("abdiff: load generator log: %v", err)
	}
	withBytes := strings.Join(head, ",") == LoadgenHeaderBytes
	if strings.Join(head, ",") != LoadgenHeader && !withBytes {
		return nil, fmt.Errorf("abdiff: load generator log: the header is %q, want %q or %q", strings.Join(head, ","),
			LoadgenHeader, LoadgenHeaderBytes)
	}
	r.FieldsPerRecord = len(head)
	log := &LoadgenLog{SHA256: contract.SHA256Hex(b), HasBytes: withBytes}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("abdiff: load generator log: %v", err)
		}
		line, _ := r.FieldPos(0)
		var row LoadgenRow
		var errs [4]error
		row.Arm = rec[0]
		row.Seq, errs[0] = strconv.ParseInt(rec[1], 10, 64)
		row.SentUnixNs, errs[1] = strconv.ParseInt(rec[2], 10, 64)
		row.AnsweredUnixNs, errs[2] = strconv.ParseInt(rec[3], 10, 64)
		row.Status, errs[3] = strconv.Atoi(rec[4])
		row.Bytes = -1
		var errBytes error
		if withBytes {
			row.Bytes, errBytes = strconv.ParseInt(rec[5], 10, 64)
			if errBytes == nil && row.Bytes < 0 {
				errBytes = errors.New("negative bytes")
			}
		}
		if err := errors.Join(append(errs[:], errBytes)...); err != nil || (row.Arm != "A" && row.Arm != "B") ||
			row.AnsweredUnixNs < row.SentUnixNs {
			return nil, fmt.Errorf("abdiff: load generator log, line %d: want arm A or B, integers, answered at or after sent, "+
				"bytes not negative", line)
		}
		log.Rows = append(log.Rows, row)
	}
	return log, nil
}

// Options tune Diff.
type Options struct {
	// Batches is the number of batches for the standard errors; 0 means DefaultBatches.
	Batches int
	// MarginMs is the gate's margin on the difference, ms per visit: with it the record says how many visits per arm
	// the sizing rule needs (DATA.md 9.3) and raises ab.too_few_visits below that. 0: not computed.
	MarginMs float64
}

// The thresholds of Diff [D].
const (
	// MinOverlap is the share of the longer window the two arms' windows must share (ab.not_concurrent below it).
	MinOverlap = 0.9
	// MinCountedShare is the share of the requests the load generator sent to an arm that the arm's traces must hold
	// for its kept share to be proven 1 (ab.sampled below it).
	MinCountedShare = 0.99
	// ErrorShareGap is the difference of error shares above which the arms' errors differ (ab.errors_differ).
	ErrorShareGap = 0.01
	// CallsGap is the relative difference of calls per visit above which the structure changed
	// (profile.structure_changed).
	CallsGap = 0.05
	// PayloadGap is the relative difference of the mean response body above which the payload changed
	// (ab.payload_changed).
	PayloadGap = 0.10
	// SelfReportSlackMs and SelfReportFrac widen the band within which the client-side difference must agree with the
	// span-derived one, beyond 2 standard errors of the client-side difference.
	SelfReportSlackMs = 0.1
	SelfReportFrac    = 0.1
)

// Diff computes the record of an A/B run from its two arms and the load generator's log. It is pure. A refusal
// (ab.not_concurrent, ab.hardware_differs, ab.sampled) is a finding of level refuse in the record, not an error, and
// `diff-fit` turns the first one into its coded refusal (exit 1, {"error": ...} with --json) and mints nothing; the
// error is for inputs that cannot be compared at all.
func Diff(baseline, candidate *Input, log *LoadgenLog, opt Options, tool fit.Tool) (*Record, error) {
	if baseline == nil || candidate == nil || log == nil {
		return nil, errors.New("abdiff: two arms and the load generator's log are needed")
	}
	for _, in := range []*Input{baseline, candidate} {
		if in.Inferred == nil || in.Inferred.Samples == nil || in.Manifest == nil || in.Check == nil {
			return nil, fmt.Errorf("abdiff: arm %q: an inference with its samples kept, its manifest and its check report are needed", in.Capture)
		}
	}
	batches := opt.Batches
	if batches == 0 {
		batches = DefaultBatches
	}
	if batches < 2 {
		return nil, fmt.Errorf("abdiff: %d batches; at least 2", batches)
	}
	r := &Record{Schema: Schema, Tool: tool, SEMethod: fmt.Sprintf("batch-means/%d", batches), Services: []Service{},
		Findings: []contract.Finding{}}
	r.Arms.Baseline, r.Arms.Candidate = arm(baseline), arm(candidate)
	r.OverlapShare = overlap(r.Arms.Baseline.Window, r.Arms.Candidate.Window)
	if r.OverlapShare < MinOverlap {
		r.Findings = append(r.Findings, contract.NewFinding(contract.ABNotConcurrent).WithNumber("overlap_share", r.OverlapShare).
			WithNumber("needed", MinOverlap))
	}
	if baseline.Hardware != "" && candidate.Hardware != "" && baseline.Hardware != candidate.Hardware {
		r.Findings = append(r.Findings, contract.NewFinding(contract.ABHardwareDiffers))
	}

	// the load generator's side
	var lat [2][]float64
	var body [2]float64
	r.Loadgen.LogSHA256 = log.SHA256
	rows := append([]LoadgenRow(nil), log.Rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].SentUnixNs < rows[j].SentUnixNs })
	for _, row := range rows {
		k := 0
		if row.Arm == "B" {
			k = 1
		}
		lat[k] = append(lat[k], float64(row.AnsweredUnixNs-row.SentUnixNs)/1e6)
		body[k] += float64(row.Bytes)
	}
	r.Loadgen.Requests = PairInt{int64(len(lat[0])), int64(len(lat[1]))}
	r.Loadgen.ClientLatencyMs = delta(lat[0], lat[1], batches)
	if log.HasBytes && len(lat[0]) > 0 && len(lat[1]) > 0 {
		rb := &Pair{Baseline: body[0] / float64(len(lat[0])), Candidate: body[1] / float64(len(lat[1]))}
		r.Loadgen.ResponseBytes = rb
		if math.Abs(rb.Candidate-rb.Baseline) > PayloadGap*rb.Baseline {
			for _, name := range entryNames(baseline.Inferred) {
				r.Findings = append(r.Findings, contract.NewFinding(contract.ABPayloadChanged).WithSubject(name).
					WithNumber("baseline_bytes", rb.Baseline).WithNumber("candidate_bytes", rb.Candidate))
			}
		}
	}
	for k, in := range []*Input{baseline, candidate} {
		sent := r.Loadgen.Requests.Baseline
		if k == 1 {
			sent = r.Loadgen.Requests.Candidate
		}
		ks := in.Check.KeptShare
		counted := ks.Value == 1 && ks.Source == contract.KeptCounted
		logged, share := false, 0.0
		if sent > 0 {
			share = float64(in.Inferred.Requests) / float64(sent)
			logged = share >= MinCountedShare && share <= 2-MinCountedShare
		}
		switch {
		case !counted && !logged:
			r.Findings = append(r.Findings, contract.NewFinding(contract.ABSampled).WithSubject(in.Capture).
				WithNumber("traced_requests", float64(in.Inferred.Requests)).WithNumber("sent", float64(sent)))
		case logged && ks.Source == contract.KeptCounted && ks.Value != 1:
			// the log admits the arm, while its own request count says its traces hold another share: the record
			// says so, and its numbers stay those of the traces
			r.Findings = append(r.Findings, contract.NewFinding(contract.ABCountConflict).WithSubject(in.Capture).
				WithNumber("kept_share", ks.Value).WithNumber("log_share", math.Round(share*1e4)/1e4))
		}
	}

	// per service, in both arms
	a, b := baseline.Inferred, candidate.Inferred
	for _, name := range sortedKeys(a.Samples) {
		sa, sb := a.Samples[name], b.Samples[name]
		if sb == nil || len(sa.Own) == 0 || len(sb.Own) == 0 {
			continue
		}
		svc := Service{Service: name, Visits: PairInt{sa.Visits, sb.Visits}, OwnMs: delta(sa.Own, sb.Own, batches),
			GlueMs: delta(sa.Glue, sb.Glue, batches), OwnCS2: cs2Delta(sa.Own, sb.Own, batches),
			OwnVar: varDelta(sa.Own, sb.Own, batches), Calls: []Calls{},
			ErrorShare: Pair{share(sa.Errors, sa.Visits), share(sb.Errors, sb.Visits)}}
		for _, callee := range unionKeys(sa.Calls, sb.Calls) {
			c := Calls{Callee: callee, Baseline: sa.Calls[callee], Candidate: sb.Calls[callee],
				BaselineSE: callsSE(sa, callee), CandidateSE: callsSE(sb, callee)}
			svc.Calls = append(svc.Calls, c)
			if math.Abs(c.Candidate-c.Baseline) > CallsGap*math.Max(c.Baseline, c.Candidate) {
				svc.CallsChanged = true
			}
		}
		// The engine applies an own-time difference as a shift, which keeps the variance (its cs2 falls as the mean
		// grows), so the variance, not the cs2, is what must not have changed beyond noise for the difference to be
		// applied alone.
		svc.VarianceChanged = math.Abs(svc.OwnVar.Delta) > 2*svc.OwnVar.SE
		if svc.Routes = routeDeltas(sa, sb, batches); svc.Routes != nil {
			svc.VarianceChanged = math.Abs(svc.OwnVar.Candidate-mixtureVariance(sa, svc.Routes)) > 2*svc.OwnVar.SE
		}
		if opt.MarginMs > 0 {
			s, cs2 := svc.OwnMs.Baseline, svc.OwnCS2.Baseline
			svc.VisitsNeeded = int64(math.Ceil(8 * cs2 * s * s / (opt.MarginMs * opt.MarginMs)))
			if min(svc.Visits.Baseline, svc.Visits.Candidate) < svc.VisitsNeeded {
				r.Findings = append(r.Findings, contract.NewFinding(contract.ABTooFewVisits).WithSubject(name).
					WithNumber("visits", float64(min(svc.Visits.Baseline, svc.Visits.Candidate))).
					WithNumber("needed", float64(svc.VisitsNeeded)))
			}
		}
		if math.Abs(svc.ErrorShare.Candidate-svc.ErrorShare.Baseline) > ErrorShareGap {
			r.Findings = append(r.Findings, contract.NewFinding(contract.ABErrorsDiffer).WithSubject(name).
				WithNumber("baseline", svc.ErrorShare.Baseline).WithNumber("candidate", svc.ErrorShare.Candidate))
		}
		if svc.CallsChanged {
			r.Findings = append(r.Findings, contract.NewFinding(contract.ProfileStructureChanged).WithSubject(name))
		}
		if f, ok := routeFinding(name, svc.OwnMs, svc.Routes); ok {
			r.Findings = append(r.Findings, f)
		}
		if svc.VarianceChanged {
			r.Findings = append(r.Findings, contract.NewFinding(contract.ProfileVarianceChanged).WithSubject(name).
				WithNumber("var_delta", svc.OwnVar.Delta).WithNumber("var_se", svc.OwnVar.SE))
		}
		r.Services = append(r.Services, svc)
	}

	// the outside measurement against the spans: the client-side difference and the entries' server-span difference
	spanDelta := entryMean(b) - entryMean(a)
	cl := r.Loadgen.ClientLatencyMs
	r.SelfReportConsistent = len(lat[0]) > 0 && len(lat[1]) > 0 &&
		math.Abs(cl.Delta-spanDelta) <= 2*cl.SE+SelfReportFrac*math.Abs(spanDelta)+SelfReportSlackMs
	if !r.SelfReportConsistent {
		r.Findings = append(r.Findings, contract.NewFinding(contract.ProfileSelfReportMismatch).
			WithNumber("client_delta_ms", cl.Delta).WithNumber("span_delta_ms", spanDelta))
	}
	contract.SortFindings(r.Findings)
	return r, nil
}

func arm(in *Input) Arm {
	a := Arm{Capture: in.Capture, ManifestDigest: in.Manifest.Digest(), FitDigest: in.FitDigest,
		Window:    Window{Start: in.Manifest.Window.Start.UTC(), End: in.Manifest.Window.End.UTC()},
		Images:    map[string][]string{},
		KeptShare: in.Check.KeptShare}
	for k, v := range in.Images {
		a.Images[k] = append([]string(nil), v...)
	}
	return a
}

// overlap is the share of the longer window that the two windows share.
func overlap(a, b Window) float64 {
	start, end := a.Start, a.End
	if b.Start.After(start) {
		start = b.Start
	}
	if b.End.Before(end) {
		end = b.End
	}
	longer := math.Max(a.End.Sub(a.Start).Seconds(), b.End.Sub(b.Start).Seconds())
	if !end.After(start) || !(longer > 0) {
		return 0
	}
	return math.Min(1, end.Sub(start).Seconds()/longer)
}

// RouteZ is how many standard errors of a route's own-time difference it must lie from the service's blended
// difference for profile.route_heterogeneous [D].
const RouteZ = 3

// routeDeltas are the own-time differences of the routes both arms read (infer.Samples.RouteOwn), in name order;
// nil when fewer than two routes are common to both arms.
func routeDeltas(sa, sb *infer.Samples, batches int) []Route {
	var names []string
	for nm, xs := range sa.RouteOwn {
		if ys, ok := sb.RouteOwn[nm]; ok && len(xs) > 0 && len(ys) > 0 {
			names = append(names, nm)
		}
	}
	if len(names) < 2 {
		return nil
	}
	sort.Strings(names)
	na, nb := 0, 0
	for _, xs := range sa.RouteOwn {
		na += len(xs)
	}
	for _, ys := range sb.RouteOwn {
		nb += len(ys)
	}
	out := make([]Route, len(names))
	for k, nm := range names {
		xs, ys := sa.RouteOwn[nm], sb.RouteOwn[nm]
		out[k] = Route{Route: nm, Visits: PairInt{int64(len(xs)), int64(len(ys))},
			Share: Pair{float64(len(xs)) / float64(na), float64(len(ys)) / float64(nb)}, OwnMs: delta(xs, ys, batches)}
	}
	return out
}

// mixtureVariance is the variance the candidate's own time has if each route's own time only shifts by its delta: the
// baseline's route variances, the candidate's route means (the baseline's plus the delta) and the candidate's shares
// (renormalized over the routes common to both arms).
func mixtureVariance(sa *infer.Samples, routes []Route) float64 {
	var w, m1, m2 float64
	for _, r := range routes {
		mu := r.OwnMs.Candidate
		v := varOf(sa.RouteOwn[r.Route])
		w += r.Share.Candidate
		m1 += r.Share.Candidate * mu
		m2 += r.Share.Candidate * (v + mu*mu)
	}
	if !(w > 0) {
		return 0
	}
	m1, m2 = m1/w, m2/w
	return m2 - m1*m1
}

// routeFinding is profile.route_heterogeneous for a service whose routes moved differently: some route (of at least
// infer.MinRouteShare of the baseline's visits) lies more than RouteZ of its standard errors from the blended
// difference. The numbers are those of the such route farthest from the blend, in ms.
func routeFinding(name string, blend Delta, routes []Route) (contract.Finding, bool) {
	best, bestGap := -1, 0.0
	for k, r := range routes {
		if r.Share.Baseline < infer.MinRouteShare || !(r.OwnMs.SE > 0) {
			continue
		}
		gap := math.Abs(r.OwnMs.Delta - blend.Delta)
		if gap > RouteZ*r.OwnMs.SE && gap > bestGap {
			best, bestGap = k, gap
		}
	}
	if best < 0 {
		return contract.Finding{}, false
	}
	r := routes[best]
	return contract.NewFinding(contract.ProfileRouteHeterogeneous).WithSubject(name).
		WithNumber("route_delta_ms", r.OwnMs.Delta).WithNumber("blend_delta_ms", blend.Delta).
		WithNumber("route_se", r.OwnMs.SE).WithNumber("route_share", r.Share.Baseline), true
}

// entryNames are the entries of an arm (the services the load generator drives), in name order.
func entryNames(inf *infer.Inferred) []string {
	var out []string
	for _, st := range inf.Stats {
		if st.Entry && !st.External {
			out = append(out, st.Name)
		}
	}
	sort.Strings(out)
	return out
}

// callsSE is the standard error of a service's mean calls per visit to a callee: the spread of the calls per visit
// over the visits of complete traces, sd/sqrt(n); 0 without the counts.
func callsSE(s *infer.Samples, callee string) float64 {
	n := float64(s.CallVisits)
	if n < 2 || s.CallsSq == nil {
		return 0
	}
	m := s.Calls[callee]
	v := (s.CallsSq[callee] - m*m) * n / (n - 1)
	if !(v > 0) {
		return 0
	}
	return math.Sqrt(v / n)
}

// entryMean is the mean server-span time of the entries, weighted by their entry visits.
func entryMean(inf *infer.Inferred) float64 {
	var sum, n float64
	for _, st := range inf.Stats {
		if st.Entry && !st.External {
			w := float64(max(st.Entries, 1))
			sum += w * st.ObservedServerMs
			n += w
		}
	}
	if n == 0 {
		return 0
	}
	return sum / n
}

func share(k, n int64) float64 {
	if n == 0 {
		return 0
	}
	return float64(k) / float64(n)
}

// delta is the difference of two series' means and its standard error, by batch means in each arm.
func delta(a, b []float64, batches int) Delta {
	ma, sa := BatchMeans(a, batches)
	mb, sb := BatchMeans(b, batches)
	return Delta{Baseline: ma, Candidate: mb, Delta: mb - ma, SE: math.Hypot(sa, sb)}
}

// cs2Delta is the difference of two series' squared coefficients of variation, with its standard error from the
// spread of the batches' own cs2.
func cs2Delta(a, b []float64, batches int) Delta {
	ca, sa := cs2Batches(a, batches)
	cb, sb := cs2Batches(b, batches)
	return Delta{Baseline: ca, Candidate: cb, Delta: cb - ca, SE: math.Hypot(sa, sb)}
}

// BatchMeans returns the mean of a series in time order and the standard error of that mean by batch means: the series
// cut into batches consecutive batches of equal size (the remainder spread one more per leading batch), the standard
// error the standard deviation of the batches' means over the square root of their number. Batches long against the
// series' correlation time are nearly independent, so the error holds for autocorrelated visits, where the textbook
// sd/sqrt(n) is too small. Fewer values than batches use one batch per value; fewer than 2 give an error of 0.
func BatchMeans(xs []float64, batches int) (mean, se float64) {
	return batchStat(xs, batches, func(b []float64) float64 {
		s := 0.0
		for _, x := range b {
			s += x
		}
		return s / float64(len(b))
	})
}

// varDelta is the difference of two series' variances, with its standard error from the spread of the batches' own
// variances.
func varDelta(a, b []float64, batches int) Delta {
	va, sa := batchStat(a, batches, varOf)
	vb, sb := batchStat(b, batches, varOf)
	return Delta{Baseline: va, Candidate: vb, Delta: vb - va, SE: math.Hypot(sa, sb)}
}

// varOf is a series' sample variance, 0 for fewer than 2 values.
func varOf(xs []float64) float64 {
	n := float64(len(xs))
	if n < 2 {
		return 0
	}
	m := 0.0
	for _, x := range xs {
		m += x
	}
	m /= n
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return v / (n - 1)
}

func cs2Batches(xs []float64, batches int) (float64, float64) {
	return batchStat(xs, batches, cs2Of)
}

// cs2Of is a series' squared coefficient of variation (sample variance over the squared mean), 0 for fewer than 2
// values or a zero mean.
func cs2Of(xs []float64) float64 {
	n := float64(len(xs))
	if n < 2 {
		return 0
	}
	m := 0.0
	for _, x := range xs {
		m += x
	}
	m /= n
	if m == 0 {
		return 0
	}
	v := 0.0
	for _, x := range xs {
		v += (x - m) * (x - m)
	}
	return v / (n - 1) / (m * m)
}

// batchStat is a statistic of the whole series and its standard error from the statistic's spread over batches.
func batchStat(xs []float64, batches int, stat func([]float64) float64) (float64, float64) {
	n := len(xs)
	if n == 0 {
		return 0, 0
	}
	whole := stat(xs)
	k := min(batches, n)
	if k < 2 {
		return whole, 0
	}
	vals := make([]float64, k)
	at := 0
	for i := 0; i < k; i++ {
		size := n / k
		if i < n%k {
			size++
		}
		vals[i] = stat(xs[at : at+size])
		at += size
	}
	m := 0.0
	for _, v := range vals {
		m += v
	}
	m /= float64(k)
	ss := 0.0
	for _, v := range vals {
		ss += (v - m) * (v - m)
	}
	return whole, math.Sqrt(ss / float64(k-1) / float64(k))
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func unionKeys(a, b map[string]float64) []string {
	set := map[string]bool{}
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	return sortedKeys(set)
}
