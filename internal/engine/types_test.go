// SPDX-License-Identifier: Apache-2.0

package engine_test

// The types other packages code against (report v2, the fit record, the change set, the A/B record, the coded
// refusal) and the published JSON Schemas under docs/mcp/schemas say the same thing: every JSON key of a Go type is a
// property of its schema, every required property is a key the type always writes (no omitempty), and every list of
// enum constants equals the schema's enum. The signatures below are compile-time pins: changing one breaks this file
// before it breaks a caller.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/engine"
	"github.com/Sharvil2704/rhobound-mcp/internal/fit"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

type recordsFunc func(string) (*abdiff.Record, bool)

func (f recordsFunc) AB(d string) (*abdiff.Record, bool) { return f(d) }

var (
	_ func(fit.Input, fit.Tool) (*fit.Record, error)                  = fit.Build
	_ func(fit.Input, *infer.Inferred, fit.Tool) (*fit.Record, error) = fit.FromInferred
	_ func([]byte) (*fit.Record, error)                               = fit.Unmarshal
	_ func(*fit.Record) ([]byte, error)                               = (*fit.Record).Marshal
	_ func(*fit.Record) string                                        = (*fit.Record).Digest
	_ func(*fit.Record) (*infer.Inferred, error)                      = (*fit.Record).Inferred
	_ func(string) fit.Tool                                           = fit.NewTool
	_ json.Marshaler                                                  = fit.Record{}
	_ json.Marshaler                                                  = fit.Float(0)
	_ json.Unmarshaler                                                = (*fit.Float)(nil)

	_ func([]byte) (*change.Set, error)                                   = change.Parse
	_ func(string) (*change.Set, error)                                   = change.FromPatch
	_ func(*change.Set, *fit.Record, change.Records) []contract.Finding   = (*change.Set).Validate
	_ func(*change.Set, *fit.Record, change.Records) (*change.Set, error) = (*change.Set).Resolve
	_ func(*change.Set) (string, error)                                   = (*change.Set).Digest
	_ func(string) string                                                 = change.SignOf
	_ func(string) bool                                                   = change.Applies
	_ func(float64) change.Range                                          = change.Point
	_ change.Records                                                      = recordsFunc(nil)
	_ json.Unmarshaler                                                    = (*change.Range)(nil)

	_ func(*abdiff.Input, *abdiff.Input, *abdiff.LoadgenLog, abdiff.Options, fit.Tool) (*abdiff.Record, error) = abdiff.Diff
	_ func([]byte) (*abdiff.LoadgenLog, error)                                                                 = abdiff.ParseLoadgenLog
	_ func([]byte) (*abdiff.Record, error)                                                                     = abdiff.Unmarshal
	_ func(*abdiff.Record) ([]byte, error)                                                                     = (*abdiff.Record).Marshal
	_ func(*abdiff.Record) string                                                                              = (*abdiff.Record).Digest

	_ func(engine.Request) (*report.V2, error)         = engine.Predict
	_ func(*report.V2) *report.Report                  = (*report.V2).V1
	_ func(*report.V2) ([]byte, error)                 = (*report.V2).Canonical
	_ func(*report.V2) (string, error)                 = (*report.V2).Digest
	_ func() report.ModelBand                          = report.DefaultModelBand
	_ func(report.ModelBand, float64, float64) float64 = report.ModelBand.Ms
	_ func(error) (report.Refusal, bool)               = report.RefusalOf
	_ error                                            = (*report.RefusalError)(nil)
	_ error                                            = (*infer.Refusal)(nil)
)

// schemas are the published JSON Schemas, by file name.
type schemas map[string]map[string]any

func loadSchemas(t *testing.T) schemas {
	t.Helper()
	dir := filepath.Join("..", "..", "docs", "mcp", "schemas")
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no schemas in %s: %v", dir, err)
	}
	s := schemas{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		s[filepath.Base(p)] = doc
	}
	return s
}

// node is a schema node and the file its relative $refs resolve against.
type node struct {
	file string
	m    map[string]any
}

// at returns the node a $ref names, relative to file: "other.json#/a/b", "#/a/b" or "other.json".
func (s schemas) at(t *testing.T, file, ref string) node {
	t.Helper()
	f, ptr, _ := strings.Cut(ref, "#")
	if f != "" {
		file = f
	}
	doc, ok := s[file]
	if !ok {
		t.Fatalf("$ref %q: no schema file %s", ref, file)
	}
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		if part == "" {
			continue
		}
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("$ref %q: %s is not an object", ref, part)
		}
		if cur = m[part]; cur == nil {
			t.Fatalf("$ref %q: no %s", ref, part)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("$ref %q is not a schema object", ref)
	}
	return node{file, m}
}

// deref follows $refs until a node has none.
func (s schemas) deref(t *testing.T, n node) node {
	for i := 0; i < 16; i++ {
		ref, ok := n.m["$ref"].(string)
		if !ok {
			return n
		}
		n = s.at(t, n.file, ref)
	}
	t.Fatal("a $ref chain longer than 16")
	return n
}

// object collects an object schema's properties (through $ref and allOf; a node's own properties win over those it
// extends) and its required names.
func (s schemas) object(t *testing.T, n node) (map[string]node, map[string]bool) {
	n = s.deref(t, n)
	props, req := map[string]node{}, map[string]bool{}
	if all, ok := n.m["allOf"].([]any); ok {
		for _, x := range all {
			p, r := s.object(t, node{n.file, x.(map[string]any)})
			for k, v := range p {
				props[k] = v
			}
			for k := range r {
				req[k] = true
			}
		}
	}
	if p, ok := n.m["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = node{n.file, v.(map[string]any)}
		}
	}
	if r, ok := n.m["required"].([]any); ok {
		for _, k := range r {
			req[k.(string)] = true
		}
	}
	return props, req
}

// pick returns the branch of a oneOf or anyOf that describes an object (wantObject) or an array, else the node itself.
func (s schemas) pick(t *testing.T, n node, wantObject bool) node {
	n = s.deref(t, n)
	for _, key := range []string{"oneOf", "anyOf"} {
		alts, ok := n.m[key].([]any)
		if !ok {
			continue
		}
		for _, a := range alts {
			b := s.deref(t, node{n.file, a.(map[string]any)})
			_, hasProps := b.m["properties"]
			_, hasAll := b.m["allOf"]
			_, hasItems := b.m["items"]
			if (wantObject && (hasProps || hasAll)) || (!wantObject && hasItems) {
				return b
			}
		}
	}
	return n
}

// jsonField is one key a Go struct writes.
type jsonField struct {
	typ       reflect.Type
	omitempty bool
}

// jsonFields lists the keys a struct type writes, embedded structs promoted, as encoding/json does for these types.
func jsonFields(typ reflect.Type) map[string]jsonField {
	out := map[string]jsonField{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" || (!f.IsExported() && !f.Anonymous) {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			for k, v := range jsonFields(f.Type) {
				if _, shadowed := out[k]; !shadowed {
					out[k] = v
				}
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = jsonField{f.Type, strings.Contains(","+opts+",", ",omitempty,")}
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

// conform checks a Go type against a schema node and returns what disagrees. pending maps the paths of keys the Go type
// may write that the schema does not define to the reason; they are returned as notes, not problems.
func (s schemas) conform(t *testing.T, path string, typ reflect.Type, n node, pending map[string]string) (problems, notes []string) {
	t.Helper()
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch {
	case typ == timeType:
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() != reflect.Uint8:
		a := s.pick(t, n, false)
		if items, ok := a.m["items"].(map[string]any); ok {
			return s.conform(t, path+"[]", typ.Elem(), node{a.file, items}, pending)
		}
		problems = append(problems, fmt.Sprintf("%s: the Go type %s is a slice, the schema is not an array", path, typ))
	case typ.Kind() == reflect.Struct:
		props, req := s.object(t, s.pick(t, n, true))
		if len(props) == 0 {
			return []string{fmt.Sprintf("%s: the Go type %s is a struct, the schema defines no properties", path, typ)}, nil
		}
		fields := jsonFields(typ)
		for _, k := range sortedKeys(fields) {
			p, ok := props[k]
			why, isPending := pending[path+"/"+k]
			switch {
			case !ok && isPending:
				notes = append(notes, fmt.Sprintf("%s/%s: a key of %s that the schema does not define: %s", path, k, typ, why))
			case !ok:
				problems = append(problems, fmt.Sprintf("%s/%s: written by %s, not a property of the schema", path, k, typ))
			default:
				pr, no := s.conform(t, path+"/"+k, fields[k].typ, p, pending)
				problems, notes = append(problems, pr...), append(notes, no...)
			}
		}
		for _, k := range sortedKeys(req) {
			f, ok := fields[k]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s/%s: required by the schema, not written by %s", path, k, typ))
			case f.omitempty:
				problems = append(problems, fmt.Sprintf("%s/%s: required by the schema, but %s may omit it (omitempty)", path, k, typ))
			}
		}
	}
	return problems, notes
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestTypesMatchTheSchemas(t *testing.T) {
	s := loadSchemas(t)
	root := func(file string) node { return node{file, s[file]} }
	const proposed = "proposed as an addition to the published schema"
	v2Only := "a report v2 row key, which report v1 never writes (omitempty, and V1 clears it)"
	for _, c := range []struct {
		name    string
		v       any
		file    string
		pending map[string]string
	}{
		// fit_window: the gate measures the fit's age from it (ARCHITECTURE.md 6.3, rule 3) and reads the report only.
		// change_range_ms: the prediction view's moved[].change_ms is a range per service.
		{"report", report.V2{}, "report.v2.json", map[string]string{"report/fit_window": proposed,
			"report/services[]/change_range_ms": proposed}},
		{"report_v1", report.Report{}, "report.v1.json", map[string]string{"report_v1/services[]/utilization_after": v2Only,
			"report_v1/services[]/change_range_ms": v2Only}},
		{"fit", fit.Record{}, "fit.v1.json", nil},
		{"change", change.Set{}, "change.v1.json", nil},
		{"ab_record", abdiff.Record{}, "ab-record.v1.json", nil},
		{"refusal", report.RefusalDoc{}, "refusal.v1.json", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			problems, notes := s.conform(t, c.name, reflect.TypeOf(c.v), root(c.file), c.pending)
			for _, p := range problems {
				t.Error(p)
			}
			for _, n := range notes {
				t.Log(n)
			}
		})
	}
	// The check itself finds a disagreement: a type against another document's schema, a missing required key and a
	// required key the type may omit.
	if p, _ := s.conform(t, "x", reflect.TypeOf(abdiff.Record{}), root("change.v1.json"), nil); len(p) == 0 {
		t.Error("an A/B record conforms to the change set's schema")
	}
	type missing struct {
		Code string `json:"code"`
		Fix  string `json:"fix,omitempty"`
	}
	p, _ := s.conform(t, "x", reflect.TypeOf(missing{}), s.at(t, "defs.v1.json", "#/$defs/Refusal"), nil)
	if len(p) != 2 {
		t.Errorf("a refusal without reason and with an omitempty fix: %q", p)
	}
}

// enum returns the enum (or const) of the schema node at file#ptr, as strings.
func (s schemas) enum(t *testing.T, file, ptr string) []string {
	t.Helper()
	n := s.deref(t, s.at(t, file, "#"+ptr))
	if c, ok := n.m["const"]; ok {
		return []string{c.(string)}
	}
	raw, ok := n.m["enum"].([]any)
	if !ok {
		t.Fatalf("%s#%s has no enum", file, ptr)
	}
	out := make([]string, len(raw))
	for i, x := range raw {
		out[i] = x.(string)
	}
	return out
}

func TestConstantsMatchTheSchemaEnums(t *testing.T) {
	s := loadSchemas(t)
	for _, c := range []struct {
		what, file, ptr string
		got             []string
		ordered         bool
	}{
		{"operations", "change.v1.json", "/properties/items/items/properties/op", change.Ops, true},
		{"operations (defs)", "defs.v1.json", "/$defs/ChangeItem/properties/op", change.Ops, true},
		{"labels", "defs.v1.json", "/$defs/Label", change.Labels, true},
		{"evidence classes", "defs.v1.json", "/$defs/EvidenceClass", report.EvidenceClasses, true},
		{"assumption statuses", "defs.v1.json", "/$defs/Assumption/properties/status", report.AssumptionStatuses, true},
		{"applied operations", "report.v2.json", "/properties/items/items/properties/op",
			[]string{change.OpOwnMs, change.OpOwnPct, change.OpWorkers, change.OpLoadScale}, true},
		{"signs", "report.v2.json", "/properties/items/items/properties/sign",
			[]string{change.SignIncreasing, change.SignDecreasing, change.SignUnsigned}, true},
		{"evaluated at", "report.v2.json", "/properties/load/properties/evaluated_at",
			[]string{report.EvaluatedAtFitWindow, report.EvaluatedAtWhatIf}, true},
		{"constructs", "report.v2.json", "/properties/entries/items/properties/biased/items/properties/construct",
			[]string{report.ConstructSecondSequentialCall, report.ConstructNonSlowestBranch, report.ConstructStagedCalls,
				report.ConstructRegularUpstream}, true},
		{"directions", "report.v2.json", "/properties/entries/items/properties/biased/items/properties/direction",
			[]string{report.DirectionBoth, report.DirectionOver, report.DirectionUnder}, true},
		{"monotonicity", "report.v2.json", "/properties/monotonicity", []string{report.Monotonicity}, true},
		{"report v2 version", "report.v2.json", "/properties/version", []string{report.VersionV2}, true},
		{"report v1 version", "report.v1.json", "/properties/version", []string{report.Version}, true},
		{"band source", "report.v2.json", "/properties/model_band/properties/source", []string{report.ModelBandSource}, true},
		{"band label", "report.v2.json", "/properties/model_band/properties/label", []string{report.ModelBandLabel}, true},
		{"fit schema", "fit.v1.json", "/properties/schema", []string{fit.Schema}, true},
		{"pod identities", "fit.v1.json", "/properties/options/properties/pod_identity",
			[]string{fit.PodIdentityDefault, fit.PodIdentityHost}, false},
		{"estimators", "fit.v1.json", "/properties/options/properties/estimator",
			[]string{infer.EstimatorFIFO, infer.EstimatorRise}, false},
		{"tool name", "defs.v1.json", "/$defs/Tool/properties/name", []string{fit.ToolName}, true},
		{"change schema", "change.v1.json", "/properties/schema", []string{change.Schema}, true},
		{"A/B schema", "ab-record.v1.json", "/properties/schema", []string{abdiff.Schema}, true},
		{"kept-share sources", "defs.v1.json", "/$defs/KeptShare/properties/source",
			[]string{contract.KeptCounted, contract.KeptDerivedTH, contract.KeptDeclared, contract.KeptAssumed}, true},
	} {
		want := s.enum(t, c.file, c.ptr)
		got := append([]string(nil), c.got...)
		if !c.ordered {
			sort.Strings(want)
			sort.Strings(got)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: the Go constants are %q, %s says %q", c.what, got, c.file, want)
		}
	}
}

// The coded errors print the message they wrap, unchanged, and are found anywhere in an error's chain.
func TestCodedErrorsKeepTheMessage(t *testing.T) {
	msg := errors.New("refusing to predict: at the observed load ...")
	for _, coded := range []error{&report.RefusalError{Code: "x", Err: msg}, &infer.Refusal{Code: "x", Err: msg}} {
		err := fmt.Errorf("context: %w", coded)
		if coded.Error() != msg.Error() || !errors.Is(err, msg) {
			t.Errorf("%T: %q", coded, coded.Error())
		}
		var re *report.RefusalError
		var ir *infer.Refusal
		if !errors.As(err, &re) && !errors.As(err, &ir) {
			t.Errorf("%T is not found in the chain", coded)
		}
	}
}

// Every applied operation has a sign, and every operation's sign is one the report may write.
func TestEveryAppliedOperationHasASign(t *testing.T) {
	for _, op := range change.Ops {
		sign := change.SignOf(op)
		if change.Applies(op) && sign == "" {
			t.Errorf("%s is applied but has no sign", op)
		}
		if sign != "" && sign != change.SignIncreasing && sign != change.SignDecreasing && sign != change.SignUnsigned {
			t.Errorf("%s: sign %q", op, sign)
		}
	}
}
