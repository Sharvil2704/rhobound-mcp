// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// FleetSchema names the fleet view: rhobound-cli.fleet/v1.
const FleetSchema = "rhobound-cli.fleet/v1"

// fleetView is `check --fleet`: the check's findings read per service, and the capture's journeys read per entry,
// so that a team sees before any fit which of its services can be modelled and what one refusal costs. A refusal
// is capture-wide (one refused service refuses the whole capture): the view names the services the refuse-level
// findings are about, and the share of journeys that touch none of them, which is what a cut by journey would
// keep. A finding with no service as its subject (a store, count or window finding) is a capture finding.
type fleetView struct {
	Schema   string         `json:"schema"`
	Verdict  string         `json:"verdict"`
	Capture  []fleetFinding `json:"capture_findings"`
	Services []fleetService `json:"services"`
	Journeys fleetJourneys  `json:"journeys"`
	Note     string         `json:"note"`
}

type fleetFinding struct {
	Code  string `json:"code"`
	Level string `json:"level"`
	Fix   string `json:"fix"`
}

type fleetService struct {
	Name     string   `json:"name"`
	Status   string   `json:"status"` // admitted, doubt or refused, by the findings about this service
	Codes    []string `json:"codes,omitempty"`
	Fix      string   `json:"fix,omitempty"` // the first refuse-level finding's fix, else the first doubt's
	Pods     int      `json:"pods"`
	Spans    int      `json:"spans"`
	Entries  int      `json:"entries"`  // root SERVER spans: requests that enter the system here
	Journeys int      `json:"journeys"` // journeys that touch this service
}

type fleetJourneys struct {
	Total           int          `json:"total"`
	TouchingRefused int          `json:"touching_refused"`
	ModelableShare  float64      `json:"modelable_share"`
	ByEntry         []fleetEntry `json:"by_entry"`
	RefusedServices []string     `json:"refused_services"`
}

type fleetEntry struct {
	Entry     string  `json:"entry"`
	Journeys  int     `json:"journeys"`
	Modelable int     `json:"modelable"`
	Share     float64 `json:"modelable_share"`
}

// fleet builds the view from a check report and the data it read.
func fleet(rep *contract.CheckReport, d *otlp.Data) *fleetView {
	v := &fleetView{Schema: FleetSchema, Verdict: rep.Verdict, Capture: []fleetFinding{}, Services: []fleetService{},
		Note: "a refusal is capture-wide: one refused service refuses the whole capture; modelable_share is the share " +
			"of journeys that touch no refused service, what a cut by journey would keep"}
	// the services, keyed as the findings' subjects are written (SafeName)
	byShown := map[string]*fleetService{}
	order := []string{}
	shownOf := func(name string) string {
		s, _ := contract.SafeName(name)
		return s
	}
	if d != nil {
		pods := map[string]map[string]bool{}
		for i := range d.Resources {
			r := d.Resources[i]
			if r.Service == "" {
				continue
			}
			k := shownOf(r.Service)
			if _, ok := byShown[k]; !ok {
				byShown[k] = &fleetService{Name: k, Status: "admitted"}
				order = append(order, k)
				pods[k] = map[string]bool{}
			}
			if r.Pod != "" {
				pods[k][r.Pod] = true
			}
		}
		for k, p := range pods {
			byShown[k].Pods = len(p)
		}
	}
	for _, f := range rep.Findings {
		svc, ok := byShown[f.Subject]
		if f.Subject == "" || !ok {
			v.Capture = append(v.Capture, fleetFinding{Code: f.Code, Level: string(f.Level), Fix: f.Fix})
			continue
		}
		svc.Codes = append(svc.Codes, f.Code)
		switch {
		case f.Level == contract.Refuse:
			if svc.Status != "refused" {
				svc.Status, svc.Fix = "refused", f.Fix
			}
		case f.Level == contract.Doubt && svc.Status == "admitted":
			svc.Status, svc.Fix = "doubt", f.Fix
		}
	}
	refused := map[string]bool{}
	for _, k := range order {
		s := byShown[k]
		sort.Strings(s.Codes)
		s.Codes = uniq(s.Codes)
		if s.Status == "refused" {
			refused[k] = true
			v.Journeys.RefusedServices = append(v.Journeys.RefusedServices, k)
		}
	}
	if v.Journeys.RefusedServices == nil {
		v.Journeys.RefusedServices = []string{}
	}
	// the journeys: a trace's services, its entry (the earliest root SERVER span's service)
	if d != nil {
		type journey struct {
			entry      string
			entryStart uint64
			services   map[string]bool
		}
		js := map[[2]uint64]*journey{}
		for i := range d.Spans {
			sp := &d.Spans[i]
			if sp.Res < 0 || int(sp.Res) >= len(d.Resources) {
				continue
			}
			k := shownOf(d.Resources[sp.Res].Service)
			if svc, ok := byShown[k]; ok {
				svc.Spans++
			}
			j := js[sp.Trace]
			if j == nil {
				j = &journey{services: map[string]bool{}}
				js[sp.Trace] = j
			}
			j.services[k] = true
			if sp.Parent == 0 && sp.Kind == otlp.KindServer && (j.entry == "" || sp.Start < j.entryStart) {
				j.entry, j.entryStart = k, sp.Start
			}
		}
		byEntry := map[string]*fleetEntry{}
		for _, j := range js {
			if j.entry == "" {
				continue // no root SERVER span: a lost-parent trace, not a journey
			}
			v.Journeys.Total++
			if svc, ok := byShown[j.entry]; ok {
				svc.Entries++
			}
			for k := range j.services {
				if svc, ok := byShown[k]; ok {
					svc.Journeys++
				}
			}
			e := byEntry[j.entry]
			if e == nil {
				e = &fleetEntry{Entry: j.entry}
				byEntry[j.entry] = e
			}
			e.Journeys++
			touches := false
			for k := range j.services {
				if refused[k] {
					touches = true
					break
				}
			}
			if touches {
				v.Journeys.TouchingRefused++
			} else {
				e.Modelable++
			}
		}
		for _, e := range byEntry {
			if e.Journeys > 0 {
				e.Share = float64(e.Modelable) / float64(e.Journeys)
			}
			v.Journeys.ByEntry = append(v.Journeys.ByEntry, *e)
		}
		sort.Slice(v.Journeys.ByEntry, func(i, j int) bool { return v.Journeys.ByEntry[i].Entry < v.Journeys.ByEntry[j].Entry })
		if v.Journeys.Total > 0 {
			v.Journeys.ModelableShare = float64(v.Journeys.Total-v.Journeys.TouchingRefused) / float64(v.Journeys.Total)
		}
	}
	if v.Journeys.ByEntry == nil {
		v.Journeys.ByEntry = []fleetEntry{}
	}
	sort.Strings(order)
	for _, k := range order {
		v.Services = append(v.Services, *byShown[k])
	}
	return v
}

func uniq(s []string) []string {
	out := s[:0]
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// writeFleetText prints the view as a table.
func writeFleetText(w io.Writer, v *fleetView) {
	fmt.Fprintf(w, "fleet: %s; %d services; %d journeys, %.1f%% modelable (%d touch a refused service)\n",
		v.Verdict, len(v.Services), v.Journeys.Total, 100*v.Journeys.ModelableShare, v.Journeys.TouchingRefused)
	fmt.Fprintf(w, "  %-28s %-8s %5s %7s %7s %8s  %s\n", "service", "status", "pods", "spans", "entries", "journeys", "codes")
	for _, s := range v.Services {
		fmt.Fprintf(w, "  %-28s %-8s %5d %7d %7d %8d  %s\n", s.Name, s.Status, s.Pods, s.Spans, s.Entries, s.Journeys, strings.Join(s.Codes, ","))
		if s.Fix != "" {
			fmt.Fprintf(w, "  %-28s   fix: %s\n", "", s.Fix)
		}
	}
	for _, e := range v.Journeys.ByEntry {
		fmt.Fprintf(w, "  entry %-22s %d journeys, %d modelable (%.1f%%)\n", e.Entry, e.Journeys, e.Modelable, 100*e.Share)
	}
	for _, f := range v.Capture {
		fmt.Fprintf(w, "  capture %-6s %s\n          fix: %s\n", f.Level, f.Code, f.Fix)
	}
	fmt.Fprintln(w, "  note:", v.Note)
}
