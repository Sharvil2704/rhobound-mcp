// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// A Float survives JSON bit for bit, non-finite values included, and nothing but a number or the three strings reads
// as one.
func TestFloatRoundTrip(t *testing.T) {
	for _, v := range []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 1.0 / 3, 45.22, 2.261e-7, 1e21, 1e-300,
		math.SmallestNonzeroFloat64, math.MaxFloat64, -math.MaxFloat64, math.Inf(1), math.Inf(-1), math.NaN()} {
		b, err := json.Marshal(Float(v))
		if err != nil {
			t.Fatalf("%v: %v", v, err)
		}
		var got Float
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatalf("%v (%s): %v", v, b, err)
		}
		if math.IsNaN(v) {
			if !math.IsNaN(float64(got)) || string(b) != `"NaN"` {
				t.Errorf("NaN: wrote %s, read %v", b, got)
			}
			continue
		}
		if math.Float64bits(float64(got)) != math.Float64bits(v) {
			t.Errorf("%v: wrote %s, read %v", v, b, float64(got))
		}
	}
	for _, s := range []string{`+Inf`, `"+Inf"`, `"-Inf"`} {
		var f Float
		err := json.Unmarshal([]byte(s), &f)
		if (s == `+Inf`) != (err != nil) {
			t.Errorf("%s: err %v", s, err)
		}
	}
	for _, bad := range []string{`null`, `"nan"`, `"Inf"`, `""`, `true`, `{}`, `[1]`} {
		var f Float
		if err := json.Unmarshal([]byte(bad), &f); err == nil {
			t.Errorf("%s read as %v", bad, f)
		}
	}
	// inside a struct, as the record uses it
	var x struct {
		A, B Float
	}
	if err := json.Unmarshal([]byte(`{"A":"-Inf","B":2.5}`), &x); err != nil || !math.IsInf(float64(x.A), -1) || x.B != 2.5 {
		t.Errorf("struct: %v %v (%v)", x.A, x.B, err)
	}
}

func TestNewTool(t *testing.T) {
	if got := NewTool("1.2.3"); got != (Tool{Name: ToolName, Version: "1.2.3", GOARCH: runtime.GOARCH}) {
		t.Errorf("%+v", got)
	}
}

// The mirror carries every field of the inference: each exported field of infer.Inferred, its statistics, its
// topology and their parts has a field of the same name in the written form. A field added to infer or model fails
// here until the mirror carries it (or the record would silently drop it).
func TestMirrorCoversEveryField(t *testing.T) {
	for _, c := range []struct{ src, dst any }{
		{infer.Inferred{}, wireInferred{}},
		{infer.ServiceStats{}, wireStats{}},
		{infer.VariableCall{}, wireVariableCall{}},
		{infer.CallShape{}, wireCallShape{}},
		{infer.PodShare{}, wirePodShare{}},
		{infer.RouteStats{}, wireRoute{}},
		{infer.RouteCaller{}, wireRouteCaller{}},
		{model.Topology{}, wireTopology{}},
		{model.Service{}, wireService{}},
		{model.Call{}, wireCall{}},
	} {
		src, dst := reflect.TypeOf(c.src), reflect.TypeOf(c.dst)
		for i := 0; i < src.NumField(); i++ {
			f := src.Field(i)
			if !f.IsExported() || (src == reflect.TypeOf(infer.Inferred{}) && f.Name == "Samples") {
				continue // the per-visit samples of an A/B arm are no part of a fit
			}
			if _, ok := dst.FieldByName(f.Name); !ok {
				t.Errorf("%s.%s is not in the record's mirror (%s)", src, f.Name, dst)
			}
		}
	}
}

// A record that does not serialize, another schema version and keys the record does not define are refused.
func TestUnmarshalRefuses(t *testing.T) {
	var r Record
	if _, err := r.Marshal(); err == nil {
		t.Error("a record without its inference serializes")
	}
	if _, err := Unmarshal([]byte(`{"schema":"rhobound.fit/v2"}`)); !errors.Is(err, ErrVersion) {
		t.Errorf("another version: %v", err)
	}
	if _, err := Unmarshal([]byte(`{"schema":"rhobound.fit/v1","capture":"x","extra":1}`)); err == nil || errors.Is(err, ErrVersion) {
		t.Errorf("an unknown key: %v", err)
	}
	if _, err := Unmarshal([]byte(`{"schema":"rhobound.fit/v1","capture":"x"}`)); err == nil {
		t.Error("a record without its inference is read")
	}
}

// Every node a record's mirror names is one of its rows (checkNodes): the model's nodes, varying calls' callees, the
// words of call shapes (rows that may themselves hold a joiner, or "?"), and the services stage nodes are named after.
// Unmarshal refuses a record that names another as one of another version.
func TestCheckNodes(t *testing.T) {
	rows := []string{"a", "b", "c", "b + c", "product catalog", "recommendation svc", "ns:cart", "stage:a:1",
		"shape:ns:cart:2", "stage:ns:cart:3.1"}
	mirror := func(edit func(w *wireInferred)) *wireInferred {
		w := &wireInferred{Topology: &wireTopology{}}
		for _, n := range rows {
			w.Topology.Services = append(w.Topology.Services, wireService{Name: n})
			stage := strings.HasPrefix(n, "stage:") || strings.HasPrefix(n, "shape:")
			w.Stats = append(w.Stats, wireStats{Name: n, Stage: stage})
		}
		if edit != nil {
			edit(w)
		}
		return w
	}
	if err := mirror(nil).checkNodes(); err != nil {
		t.Fatalf("the rows alone: %v", err)
	}
	shape := func(s string) func(w *wireInferred) {
		return func(w *wireInferred) { w.Stats[0].CallShapes = []wireCallShape{{Shape: s, Share: 1}} }
	}
	for _, s := range []string{"b", "b + c", "b + c x2", "b + c then a", "product catalog x4 + recommendation svc",
		"recommendation svc then product catalog x4", "? then a", "ns:cart x2 then ns:cart", "a x123456789"} {
		if err := mirror(shape(s)).checkNodes(); err != nil {
			t.Errorf("the shape %q: %v", s, err)
		}
	}
	for _, s := range []string{"", "z", "a then z", "a then", "a  then b", "a x1", "a x02", "a x2x", "a x1234567890",
		"stage:a:1 then", "plnt 198.51.100.250 then a", "a then b + plnt", "product catalog x4 + recommendation"} {
		if err := mirror(shape(s)).checkNodes(); err == nil {
			t.Errorf("the shape %q is read", s)
		}
	}
	rename := func(i int, name string) func(w *wireInferred) {
		return func(w *wireInferred) { w.Stats[i].Name, w.Topology.Services[i].Name = name, name }
	}
	for what, edit := range map[string]func(w *wireInferred){
		"a varying call's callee": func(w *wireInferred) {
			w.Stats[0].VariableCalls = []wireVariableCall{{Callee: "plnt 198.51.100.250", ModelledAs: infer.ModelledMean}}
		},
		"a node of the model":        func(w *wireInferred) { w.Topology.Services[1].Name = "plnt 198.51.100.250" },
		"a stage after no row":       rename(7, "stage:z:1"),
		"a stage after a stage":      rename(7, "stage:stage:a:1:1"),
		"a shape with a stage's j.s": rename(8, "shape:a:1.2"),
		"a stage's j not a number":   rename(7, "stage:a:01"),
	} {
		if err := mirror(edit).checkNodes(); err == nil {
			t.Errorf("%s that is not a row is read", what)
		}
	}
	// a varying call to a row, and a service named like a stage node that is not one, are rows
	if err := mirror(func(w *wireInferred) {
		w.Stats[0].VariableCalls = []wireVariableCall{{Callee: "stage:a:1", ModelledAs: infer.ModelledMean}}
		rename(3, "stage:x y:1")(w)
	}).checkNodes(); err != nil {
		t.Errorf("rows: %v", err)
	}
}
