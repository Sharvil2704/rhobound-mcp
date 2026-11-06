// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/model"
)

// TestHopFromMeanIsReported: a service whose hop is read from the mean gap has the field in the JSON row and one line
// in the text report that names it; the rows of the others have neither.
func TestHopFromMeanIsReported(t *testing.T) {
	topo, err := model.ParseTopology([]byte(`{"entry":{"a":1},"services":[
	 {"name":"a","s":1,"children":[{"name":"b","hop":0.4},{"name":"c","hop":0.3}]},
	 {"name":"b","s":1,"hop":0.4},{"name":"c","s":1,"hop":0.3}]}`))
	if err != nil {
		t.Fatal(err)
	}
	lam := 0.01
	base, err := model.AnalyticWith(topo, lam, nil, model.Default)
	if err != nil || !base.Stable {
		t.Fatal(err)
	}
	stats := []infer.ServiceStats{{Name: "a", Entry: true}, {Name: "b", HopFromMean: true, ObservedClientMs: 2}, {Name: "c", ObservedClientMs: 2}}
	rep := Build("cap", &infer.Inferred{Topology: topo, Stats: stats, LamExtPerMs: lam, WindowS: 60}, 1, base, nil, nil)

	var js bytes.Buffer
	if err := rep.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	var back struct {
		Services []struct {
			Name string `json:"name"`
			Hop  *bool  `json:"hop_from_mean"`
		} `json:"services"`
	}
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatal(err)
	}
	for _, s := range back.Services {
		if got, want := s.Hop != nil && *s.Hop, s.Name == "b"; got != want {
			t.Errorf("%s: hop_from_mean %v, want %v", s.Name, got, want)
		}
	}
	var txt bytes.Buffer
	if err := rep.WriteText(&txt); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(txt.String(), "hop from the mean:"); n != 1 || !strings.Contains(txt.String(), "calls into b is") {
		t.Errorf("want one footnote naming b only:\n%s", txt.String())
	}

	stats[1].HopFromMean = false
	rep = Build("cap", &infer.Inferred{Topology: topo, Stats: stats, LamExtPerMs: lam, WindowS: 60}, 1, base, nil, nil)
	txt.Reset()
	if err := rep.WriteText(&txt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(txt.String(), "hop from the mean") {
		t.Errorf("a footnote with no service to name:\n%s", txt.String())
	}
}
