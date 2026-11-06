// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// A hand-made capture: entries at a and d; a calls b and c; d calls c and ext (a lost-parent peer). b is refused
// by a finding, c doubted, a store finding has no subject.
func TestFleetReadsFindingsPerServiceAndJourneys(t *testing.T) {
	d := &otlp.Data{Resources: []otlp.Resource{
		{Service: "a", Pod: "a-0"}, {Service: "b", Pod: "b-0"}, {Service: "b", Pod: "b-1"}, {Service: "c", Pod: "c-0"}, {Service: "d", Pod: "d-0"},
	}}
	span := func(trace, id, parent uint64, res int32, kind uint8, start uint64) otlp.Span {
		return otlp.Span{Trace: [2]uint64{0, trace}, ID: id, Parent: parent, Res: res, Kind: kind, Start: start, End: start + 10, Peer: -1, Addr: -1}
	}
	for i := uint64(1); i <= 4; i++ { // four journeys entering at a: a -> b, a -> c
		d.Spans = append(d.Spans,
			span(i, 1, 0, 0, otlp.KindServer, 100*i), span(i, 2, 1, 0, otlp.KindClient, 100*i+1), span(i, 3, 2, 1+int32(i%2), otlp.KindServer, 100*i+2),
			span(i, 4, 1, 0, otlp.KindClient, 100*i+5), span(i, 5, 4, 3, otlp.KindServer, 100*i+6))
	}
	for i := uint64(5); i <= 6; i++ { // two journeys entering at d: d -> c
		d.Spans = append(d.Spans, span(i, 1, 0, 4, otlp.KindServer, 100*i), span(i, 2, 1, 4, otlp.KindClient, 100*i+1), span(i, 3, 2, 3, otlp.KindServer, 100*i+2))
	}
	d.Spans = append(d.Spans, span(7, 9, 77, 3, otlp.KindServer, 700)) // a lost-parent span: no journey
	findings := []contract.Finding{
		contract.NewFinding(contract.TraceMixedImages).WithSubject("b"),
		{Code: "trace.pod_churn", Level: contract.Doubt, Subject: "c", Fix: "fix c"},
		{Code: "store.small_batches", Level: contract.Note, Fix: "batch longer"},
	}
	rep := contract.NewCheckReport(findings, nil, "sha256:x", contract.KeptShare{})
	v := fleet(rep, d)
	if v.Verdict != contract.Refused || v.Schema != FleetSchema {
		t.Fatalf("verdict %s schema %s", v.Verdict, v.Schema)
	}
	byName := map[string]fleetService{}
	for _, s := range v.Services {
		byName[s.Name] = s
	}
	if len(byName) != 4 || byName["b"].Status != "refused" || byName["b"].Pods != 2 || byName["c"].Status != "doubt" ||
		byName["a"].Status != "admitted" || byName["d"].Status != "admitted" {
		t.Errorf("services: %+v", v.Services)
	}
	if byName["a"].Entries != 4 || byName["d"].Entries != 2 || byName["c"].Journeys != 6 || byName["b"].Journeys != 4 || byName["a"].Spans != 12 {
		t.Errorf("counts: %+v", v.Services)
	}
	if v.Journeys.Total != 6 || v.Journeys.TouchingRefused != 4 || v.Journeys.ModelableShare != 2.0/6 ||
		strings.Join(v.Journeys.RefusedServices, ",") != "b" {
		t.Errorf("journeys: %+v", v.Journeys)
	}
	if len(v.Journeys.ByEntry) != 2 || v.Journeys.ByEntry[0].Entry != "a" || v.Journeys.ByEntry[0].Modelable != 0 ||
		v.Journeys.ByEntry[1].Entry != "d" || v.Journeys.ByEntry[1].Modelable != 2 || v.Journeys.ByEntry[1].Share != 1 {
		t.Errorf("by entry: %+v", v.Journeys.ByEntry)
	}
	if len(v.Capture) != 1 || v.Capture[0].Code != "store.small_batches" {
		t.Errorf("capture findings: %+v", v.Capture)
	}
	var b strings.Builder
	writeFleetText(&b, v)
	if !strings.Contains(b.String(), "33.3% modelable") || !strings.Contains(b.String(), "b                            refused") {
		t.Errorf("text:\n%s", b.String())
	}
}

func TestCheckFleetOnFixtures(t *testing.T) {
	root := repoRoot(t)
	out, errw, code := exec(t, "check", filepath.Join(root, "testdata/mcp/loop"), "--fleet", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errw)
	}
	var v fleetView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatal(err)
	}
	if v.Verdict != contract.Admitted || len(v.Services) != 3 || v.Journeys.Total != 3099 || v.Journeys.ModelableShare != 1 ||
		v.Services[0].Name != "a" || v.Services[0].Entries != 3099 || v.Services[0].Status != "admitted" {
		t.Errorf("loop fleet: verdict %s services %+v journeys %+v", v.Verdict, v.Services, v.Journeys)
	}
	// a refused capture still prints the view, with exit 4
	out, _, code = exec(t, "check", filepath.Join(root, "internal/infer/testdata/nested/fraud-detection-consumer-in-consumer"), "--fleet")
	if code != 4 || !strings.HasPrefix(out, "fleet: refused;") {
		t.Errorf("refused fleet: exit %d\n%s", code, out)
	}
}
