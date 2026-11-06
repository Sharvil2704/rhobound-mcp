// SPDX-License-Identifier: Apache-2.0

// Package fit is the fit record (rhobound.fit/v1, docs/mcp/schemas/fit.v1.json): what `rhobound fit` makes of a
// capture once, so that every prediction after it is a pure function of the record, a change set and a load point
// (docs/mcp/ARCHITECTURE.md 4.1).
//
// The record has two parts. Its summary fields (the capture name, the tool, the options, the kept share, the window,
// the manifest digest, the image digests, a per-service summary and the coded findings) are a contract: the gate, the
// views and the MCP server read them. The rest is a lossless mirror of the inference (the topology, the statistics of
// every service with their lower-bound flags, the observed values and the warnings), which only the CLI that wrote it
// reads back, through Inferred; it is unexported here so that nothing else depends on its layout.
//
// A record is its bytes. Marshal gives its canonical JSON (contract.Canonical: sorted keys, no insignificant
// whitespace, no trailing newline), `rhobound fit --out DIR` writes exactly those bytes to DIR/fit.json, and Digest is
// the sha256 of them, so the digest the MCP server computes over the file it stores is the fit_digest every view
// carries. Floats round-trip exactly; the ones that may be non-finite are written as Float.
package fit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// Schema is the schema id of a fit record.
const Schema = "rhobound.fit/v1"

// File is the name of the record in the folder `rhobound fit --out DIR` writes.
const File = "fit.json"

// ErrVersion is the error Unmarshal returns, wrapped, for a record of another schema version; the CLI refuses it with
// the code fit.version.
var ErrVersion = errors.New("not a " + Schema + " record")

// Record is one fit. The exported fields are the summary of fit.v1.json; a record made by Build or Unmarshal also holds
// the mirror of its inference. Workers, Images, Services and Findings are never nil in such a record, so that they
// serialize as {} and [] rather than null.
type Record struct {
	Schema string `json:"schema"`
	// Capture is the capture name or path exactly as it was given to `fit`. Report v1's capture field is filled from
	// it, so that `predict --fit F --report v1` prints what `predict CAPTURE` prints.
	Capture   string             `json:"capture"`
	Tool      Tool               `json:"tool"`
	Options   Options            `json:"options"`
	KeptShare contract.KeptShare `json:"kept_share"`
	Window    Window             `json:"window"`
	// ManifestDigest is the digest of the capture manifest as check completed it (contract.Manifest.Digest).
	ManifestDigest string `json:"manifest_digest"`
	// Images are the running image digests by service, as the resources reported them; two for one service is
	// trace.mixed_images.
	Images   map[string][]string `json:"images"`
	Services []Service           `json:"services"`
	// Findings are the check report's findings and the inference's coded warnings, sorted (contract.SortFindings).
	Findings []contract.Finding `json:"findings"`

	// mirror is the inference the record mirrors, in its written form; Marshal writes it, Unmarshal reads it back and
	// Inferred rebuilds a fresh inference from it.
	mirror *wireInferred
}

// Options are the options the fit was made with (fit.v1 options). Workers is never nil.
type Options struct {
	// Workers are the pinned workers per pod by service (--workers); every name is a service of the capture
	// (change.unknown_service otherwise).
	Workers map[string]int `json:"workers"`
	// PodIdentity is PodIdentityDefault, or PodIdentityHost for pre-fork servers (--pod-identity host).
	PodIdentity string `json:"pod_identity"`
	// KeptShare is a declared --kept-share, or nil.
	KeptShare *float64 `json:"kept_share,omitempty"`
	// Plain is --plain: the inference reads the wait outside a callee's spans with the basic model.
	Plain bool `json:"plain"`
	// Estimator is the estimator the inference used, resolved: infer.EstimatorFIFO or infer.EstimatorRise, never ""
	// (the RHOBOUND_ESTIMATOR variable is read by the CLI before the fit, never by Build).
	Estimator string `json:"estimator"`
	// ProbeRoutes are the declared --probe-route values.
	ProbeRoutes []string `json:"probe_routes,omitempty"`
	// RequestCount is the digest of the rhobound.request-count/v1 file check used, or "".
	RequestCount string `json:"request_count,omitempty"`
}

// The pod identities of Options.PodIdentity.
const (
	PodIdentityDefault = "default"
	PodIdentityHost    = "host"
)

// Window is the capture's window, in UTC; the fit's age is measured from its end.
type Window struct {
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	WindowS float64   `json:"window_s"`
}

// Service is the summary of one service: what the server and the gate read without solving the model. The zero-time
// stage: and shape: nodes the inference adds for the call structure are not services and are not listed.
type Service struct {
	Name     string `json:"name"`
	Entry    bool   `json:"entry"`
	External bool   `json:"external"`
	// Workers is the workers per pod, as the report prints them (workers_per_pod).
	Workers int `json:"workers"`
	// WorkersLowerBound is true when Workers is only a lower bound (too few requests waited, or too briefly, to read
	// it from), as the report's workers_are_a_lower_bound.
	WorkersLowerBound bool `json:"workers_lower_bound"`
	// ClientSpans is true when the calls into the service carry CLIENT spans; the gate's lower-bound worker rule needs
	// them (ARCHITECTURE.md 6.3). False for a service no caller in the capture calls.
	ClientSpans bool `json:"client_spans"`
}

// Tool is the tool that computed a record (defs.v1 Tool). Content ids include GOARCH, because the last bits of
// floating point differ between architectures (ARCHITECTURE.md 4.1).
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	GOARCH  string `json:"goarch"`
}

// ToolName is Tool.Name.
const ToolName = "rhobound"

// NewTool returns this binary's tool record for a version string (the CLI's main.Version).
func NewTool(version string) Tool {
	return Tool{Name: ToolName, Version: version, GOARCH: runtime.GOARCH}
}

var (
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+~-]{0,63}$`)
	goarchRE  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	digestRE  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// VersionRule is the rule of a version a record holds (defs.v1 Tool.version), in words.
const VersionRule = "1 to 64 characters from letters, digits and . _ + ~ -, the first a letter or a digit"

// CheckVersion reports whether a version may be recorded (VersionRule). A build stamped with another (-X
// main.Version) would write records that every build, itself included, refuses to read (Tool.Check), so the commands
// that write a record refuse to run under it.
func CheckVersion(v string) bool { return versionRE.MatchString(v) }

// Check reports why a tool record read from a stored record is not one a build of this tool writes, or nil: the
// name rhobound, a version of VersionRule, and a GOARCH of lower-case letters and digits.
func (t Tool) Check() error {
	switch {
	case t.Name != ToolName:
		return errors.New("the tool's name is not " + ToolName)
	case !CheckVersion(t.Version):
		return errors.New("the tool's version is not a version")
	case !goarchRE.MatchString(t.GOARCH):
		return errors.New("the tool's GOARCH is not one")
	}
	return nil
}

// CheckDigest reports whether s is a content id, sha256: and 64 lower-case hex digits (defs.v1 Digest).
func CheckDigest(s string) bool { return digestRE.MatchString(s) }

// CheckKeptShare reports why a kept share read from a stored record is not one this tool writes, or nil: a value in
// (0, 1] and one of the four sources (DATA.md 3.8).
func CheckKeptShare(k contract.KeptShare) error {
	if !(k.Value > 0 && k.Value <= 1) {
		return errors.New("the kept share is not in (0, 1]")
	}
	switch k.Source {
	case contract.KeptCounted, contract.KeptDerivedTH, contract.KeptDeclared, contract.KeptAssumed:
		return nil
	}
	return errors.New("the kept share's source is not one of the four")
}

// Check reports why a record's fixed fields are not what this version writes, or nil: the tool, the options' enums and
// digest, the kept share, the manifest digest, every finding (contract.CheckStored) and, in a record with its
// inference, how each varying call count is modelled and that every node the mirror names is one of its rows
// (checkNodes: the model's nodes, varying calls' callees, call shapes and the services stage nodes are named after).
// A record may come from outside the tool (an environment's baseline is made outside the server), and these fields
// reach callers as they are, or, for the nodes, as the name rule shows the rows, so they are checked, never taken as
// written; Unmarshal refuses a record that fails as one of another version (ErrVersion), and the strings it does not
// check (names, image digests, the services of merged nested spans, the mirror's other strings) reach a caller only
// through the name rule, or not at all.
func (r *Record) Check() error {
	if err := r.Tool.Check(); err != nil {
		return err
	}
	o := r.Options
	switch {
	case o.PodIdentity != PodIdentityDefault && o.PodIdentity != PodIdentityHost:
		return errors.New("the pod identity option is not one this tool sets")
	case o.Estimator != infer.EstimatorFIFO && o.Estimator != infer.EstimatorRise:
		return errors.New("the estimator is not one this tool has")
	case o.RequestCount != "" && !CheckDigest(o.RequestCount):
		return errors.New("the request count's digest is not a digest")
	case !CheckDigest(r.ManifestDigest):
		return errors.New("the manifest digest is not a digest")
	}
	if err := CheckKeptShare(r.KeptShare); err != nil {
		return err
	}
	for _, f := range r.Findings {
		if err := contract.CheckStored(f); err != nil {
			return err
		}
	}
	if r.mirror != nil {
		for _, st := range r.mirror.Stats {
			for _, v := range st.VariableCalls {
				switch v.ModelledAs {
				case infer.ModelledMean, infer.ModelledStages, infer.ModelledMixture:
				default:
					return errors.New("a varying call count is modelled in a way this tool does not")
				}
			}
		}
		if r.mirror.Topology != nil {
			return r.mirror.checkNodes()
		}
	}
	return nil
}

// Input is what a fit is built from: one capture as check admitted it.
type Input struct {
	// Capture is the capture argument exactly as given to `fit` (Record.Capture).
	Capture string
	// Data is the data check.Run returned: identical duplicates dropped, pods re-keyed by host under
	// --pod-identity host. It must not be data that infer has already read (infer reorders the spans).
	Data *otlp.Data
	// Manifest is the manifest check.Run completed; Check is its report.
	Manifest *contract.Manifest
	Check    *contract.CheckReport
	// PeerNames maps peer addresses to services (check.ClusterInputs; trace.peer_renamed). It is not recorded: the
	// manifest digest covers the cluster dump and the peer map it came from.
	PeerNames map[string]string
	Options   Options
}

// Build runs the inference on in.Data with in.Options and returns its record. It is pure: no file, clock or
// environment variable is read. The inference reorders in.Data's spans.
func Build(in Input, tool Tool) (*Record, error) {
	if in.Data == nil {
		return nil, errors.New("fit: no data to fit")
	}
	opt := in.Options
	if opt.Estimator == "" {
		opt.Estimator = infer.EstimatorFIFO
	}
	// --workers may name a service as the views show it; the record names it as the traces wrote it
	w, err := infer.ShownWorkers(in.Data, opt.Workers)
	if err != nil {
		return nil, err
	}
	if opt.Workers != nil {
		opt.Workers = w
	}
	inf, err := infer.Run(in.Data, infer.Options{KeptShare: keptShareOf(in).Value, Workers: opt.Workers, Plain: opt.Plain,
		Estimator: opt.Estimator})
	if err != nil {
		return nil, err
	}
	in.Options = opt
	return FromInferred(in, inf, tool)
}

// keptShareOf is the kept share a fit uses: check's (counted, derived, declared or assumed), else a declared
// --kept-share, else 1, assumed.
func keptShareOf(in Input) contract.KeptShare {
	switch {
	case in.Check != nil && in.Check.KeptShare.Value > 0:
		return in.Check.KeptShare
	case in.Options.KeptShare != nil:
		return contract.KeptShare{Value: *in.Options.KeptShare, Source: contract.KeptDeclared}
	}
	return contract.KeptShare{Value: 1, Source: contract.KeptAssumed}
}

// FromInferred returns the record of an inference already made, with in's capture name, manifest, check report and
// options; in.Data, when given, is read for the image digests only. Tests use it for hand-written topologies.
func FromInferred(in Input, inf *infer.Inferred, tool Tool) (*Record, error) {
	if inf == nil || inf.Topology == nil || len(inf.Stats) != len(inf.Topology.Services) {
		return nil, errors.New("fit: an inference without a topology, or with statistics not one per node")
	}
	opt := in.Options
	if opt.Workers == nil {
		opt.Workers = map[string]int{}
	}
	if opt.PodIdentity == "" {
		opt.PodIdentity = PodIdentityDefault
	}
	if opt.Estimator == "" {
		opt.Estimator = infer.EstimatorFIFO
	}
	r := &Record{Schema: Schema, Capture: in.Capture, Tool: tool, Options: opt, KeptShare: keptShareOf(in),
		Window: Window{WindowS: inf.WindowS}, Images: map[string][]string{}, Services: []Service{},
		Findings: []contract.Finding{}}
	if in.Manifest != nil {
		r.ManifestDigest = in.Manifest.Digest()
		r.Window.Start, r.Window.End = in.Manifest.Window.Start.UTC(), in.Manifest.Window.End.UTC()
	}
	if in.Data != nil {
		r.Images = Images(in.Data)
	}
	for i, st := range inf.Stats {
		if st.Stage {
			continue // a stage: or shape: node, not a service
		}
		sv := inf.Topology.Services[i]
		w := sv.Workers
		if sv.Routing == model.Pooled {
			w = sv.Servers * sv.Workers
		}
		r.Services = append(r.Services, Service{Name: st.Name, Entry: st.Entry, External: st.External, Workers: w,
			WorkersLowerBound: st.WorkersInferred && !st.WorkersFromQueueing && !st.External,
			ClientSpans:       st.External || st.ObservedClientMs > 0})
	}
	if in.Check != nil {
		r.Findings = append(r.Findings, in.Check.Findings...)
	}
	r.Findings = append(r.Findings, inferFindings(inf, r.Findings)...)
	contract.SortFindings(r.Findings)
	r.mirror = toWire(inf)
	return r, nil
}

// Images are the image digests by service that a capture's resources report (container.image.repo_digests), each
// list sorted; never nil. Two digests for one service in a window is trace.mixed_images.
func Images(d *otlp.Data) map[string][]string {
	out := map[string][]string{}
	for i, res := range d.Resources {
		for _, dg := range d.InfoOf(int32(i)).RepoDigests {
			if !slices.Contains(out[res.Service], dg) {
				out[res.Service] = append(out[res.Service], dg)
			}
		}
	}
	for _, l := range out {
		sort.Strings(l)
	}
	return out
}

// inferFindings are the inference's coded warnings as findings, without the ones a check finding already names (the
// same code and subject).
func inferFindings(inf *infer.Inferred, have []contract.Finding) []contract.Finding {
	var out []contract.Finding
	for i, code := range inf.WarningCodes {
		if code == "" || i >= len(inf.Warnings) {
			continue
		}
		f := contract.NewFinding(code)
		if i < len(inf.WarningSubjects) && inf.WarningSubjects[i] != "" {
			f = f.WithSubject(inf.WarningSubjects[i])
		}
		dup := false
		for _, h := range append(have, out...) {
			if h.Code == f.Code && h.Subject == f.Subject {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, f)
		}
	}
	return out
}

// wire is a record as it is written: the summary, then the mirror of the inference under "inference".
type wire struct {
	recordFields
	Inference *wireInferred `json:"inference"`
}

// recordFields is Record without its methods, so that encoding it does not call Record.MarshalJSON.
type recordFields Record

// Marshal returns the record's canonical JSON, the summary and the mirror of its inference: the bytes `fit --out`
// writes, with no trailing newline.
func (r *Record) Marshal() ([]byte, error) {
	// It encodes a separate wire type, never *Record itself: MarshalJSON calls Marshal.
	if r == nil || r.mirror == nil {
		return nil, errors.New("fit: a record without its inference (made neither by Build, FromInferred nor Unmarshal)")
	}
	return contract.Canonical(wire{recordFields: recordFields(*r), Inference: r.mirror})
}

// MarshalJSON is Marshal, so that encoding a record by any route, as a value or a pointer, keeps its inference (or
// fails).
func (r Record) MarshalJSON() ([]byte, error) { return r.Marshal() }

// Unmarshal reads a record that Marshal wrote. A record of another schema version, one whose fixed fields are not
// what this version writes (Check), or one whose bytes are not those Marshal writes for it (a record serialized again
// by another tool) is an error wrapping ErrVersion; a key the record does not define is an error.
func Unmarshal(b []byte) (*Record, error) {
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, fmt.Errorf("fit: %v", err)
	}
	if head.Schema != Schema {
		return nil, fmt.Errorf("fit: %w: the record's schema is %q", ErrVersion, head.Schema)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var w wire
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("fit: %v", err)
	}
	if dec.More() {
		return nil, errors.New("fit: more than one JSON value")
	}
	if w.Inference == nil || w.Inference.Topology == nil {
		return nil, errors.New("fit: the record holds no inference")
	}
	r := Record(w.recordFields)
	if r.Options.Workers == nil {
		r.Options.Workers = map[string]int{}
	}
	if r.Images == nil {
		r.Images = map[string][]string{}
	}
	if r.Services == nil {
		r.Services = []Service{}
	}
	if r.Findings == nil {
		r.Findings = []contract.Finding{}
	}
	r.mirror = w.Inference
	if _, err := r.Inferred(); err != nil {
		return nil, err
	}
	if err := r.Check(); err != nil {
		return nil, fmt.Errorf("fit: %w: %v", ErrVersion, err)
	}
	// A record's content id is the digest of its bytes, and every reader agrees on it (the views' fit_digest, a change
	// set's fit, the report's fit_digest) only for the bytes Marshal writes, as `fit` wrote them; a record serialized
	// again by another tool (other whitespace, key order or number text) is one this version did not write.
	if m, err := r.Marshal(); err != nil || !bytes.Equal(m, b) {
		return nil, fmt.Errorf("fit: %w: the record is not in the form rhobound fit writes", ErrVersion)
	}
	return &r, nil
}

// Digest is the record's content id, contract.Digest of Marshal's bytes; for a record read by Unmarshal, the sha256
// of the bytes it read. A record that cannot be serialized (one not made by Build, FromInferred or Unmarshal) is a
// programming error and panics.
func (r *Record) Digest() string {
	b, err := r.Marshal()
	if err != nil {
		panic(fmt.Sprintf("fit: digest of a record that does not serialize: %v", err))
	}
	return contract.Digest(b)
}

// Inferred returns a copy of the inference the record mirrors, for the engine to solve: the topology finished
// (model.Topology.Finish) and every statistic the report reads. Each call returns a fresh copy, so a caller may change
// it.
func (r *Record) Inferred() (*infer.Inferred, error) {
	if r == nil || r.mirror == nil {
		return nil, errors.New("fit: a record without its inference")
	}
	return fromWire(r.mirror)
}

// Float is a float64 that may be non-finite (defs.v1 Float). JSON has no NaN or infinity, so they are written as the
// strings "NaN", "+Inf" and "-Inf"; a finite value is written as encoding/json writes a float64, the shortest text
// that reads back as the same bits.
type Float float64

// MarshalJSON writes f as a number, or as "NaN", "+Inf" or "-Inf".
func (f Float) MarshalJSON() ([]byte, error) {
	switch v := float64(f); {
	case math.IsNaN(v):
		return []byte(`"NaN"`), nil
	case math.IsInf(v, 1):
		return []byte(`"+Inf"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Inf"`), nil
	default:
		return json.Marshal(v)
	}
}

// UnmarshalJSON reads a number, or one of the strings "NaN", "+Inf" and "-Inf". Anything else, null included, is an
// error: a record never writes null for a float, and reading it as 0 would change a number silently.
func (f *Float) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("fit: float: %v", err)
		}
		switch s {
		case "NaN":
			*f = Float(math.NaN())
		case "+Inf":
			*f = Float(math.Inf(1))
		case "-Inf":
			*f = Float(math.Inf(-1))
		default:
			return fmt.Errorf("fit: float %s is not a number, \"NaN\", \"+Inf\" or \"-Inf\"", strconv.Quote(s))
		}
		return nil
	}
	if string(b) == "null" {
		return errors.New("fit: float is null")
	}
	var v float64
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("fit: float: %v", err)
	}
	*f = Float(v)
	return nil
}
