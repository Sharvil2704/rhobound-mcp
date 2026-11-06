// SPDX-License-Identifier: Apache-2.0

package views

import (
	"strconv"

	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// The most rows of the context view (context.v1).
const (
	MaxContextEntries  = 20
	MaxContextServices = 40
)

// ContextInput is what the context view is built from: the fit's digest and kept share, its baseline report v2 (no
// change, at the fit's window), and the leverage of its services by entry (budget.Leverage).
type ContextInput struct {
	FitDigest string
	KeptShare contract.KeptShare
	Base      *report.V2
	Leverage  map[string]map[string]float64
	// ValidatedCeiling is max_utilization_validated (gate.DefaultMaxUtilizationValidated when 0).
	ValidatedCeiling float64
	Detail           string
}

type contextView struct {
	Schema           string              `json:"schema"`
	FitDigest        string              `json:"fit_digest"`
	WindowS          float64             `json:"window_s"`
	EntryRPS         float64             `json:"entry_rps"`
	KeptShare        contract.KeptShare  `json:"kept_share"`
	EvidenceClass    string              `json:"evidence_class"`
	MaxUtilization   float64             `json:"max_utilization"`
	ValidatedCeiling float64             `json:"validated_ceiling"`
	Entries          []contextEntry      `json:"entries"`
	Services         []contextService    `json:"services"`
	Findings         []contract.Finding  `json:"findings"`
	FindingsOmitted  int                 `json:"findings_omitted,omitempty"`
	Assumptions      []report.Assumption `json:"assumptions"`
	Truncated        bool                `json:"truncated"`
	Scope            string              `json:"scope"`
}

type contextEntry struct {
	Name          string   `json:"name"`
	NameUntrusted bool     `json:"name_untrusted,omitempty"`
	ModelMs       float64  `json:"model_ms"`
	ObservedMs    *float64 `json:"observed_ms,omitempty"`
}

type contextService struct {
	Name          string             `json:"name"`
	NameUntrusted bool               `json:"name_untrusted,omitempty"`
	Utilization   float64            `json:"utilization"`
	OwnMs         float64            `json:"own_ms"`
	Workers       string             `json:"workers"`
	Leverage      map[string]float64 `json:"leverage,omitempty"`
	MisfitPct     *float64           `json:"misfit_pct,omitempty"`
}

// Context returns the context view (rhobound-mcp.context/v1): where latency is tight, before any code is written. Its
// entries are listed by mean, its services by utilization, each with its leverage on each entry.
func Context(in ContextInput) ([]byte, error) {
	b := in.Base
	ceiling := in.ValidatedCeiling
	if ceiling == 0 {
		ceiling = gate.DefaultMaxUtilizationValidated
	}
	v := &contextView{Schema: ContextSchema, FitDigest: in.FitDigest, WindowS: f4(b.WindowS), EntryRPS: f4(b.RPS),
		KeptShare: in.KeptShare, EvidenceClass: b.EvidenceClass, MaxUtilization: f4(b.MaxRho),
		ValidatedCeiling: ceiling, Entries: []contextEntry{}, Services: []contextService{},
		Findings: findings(b.Findings), Assumptions: assumptions(b.Assumptions), Scope: Scope}
	nm := namerOf(rowNames(b.Rows))
	for _, row := range b.Rows {
		if !row.Entry || !budget.Budgetable(row) {
			continue
		}
		n, u := nm.name(row.Name)
		e := contextEntry{Name: n, NameUntrusted: u, ModelMs: f4(row.ModelMs)}
		if row.ObservedMs > 0 {
			e.ObservedMs = r4(row.ObservedMs)
		}
		v.Entries = append(v.Entries, e)
	}
	byKey(v.Entries, func(e contextEntry) float64 { return e.ModelMs }, func(e contextEntry) string { return e.Name })
	for _, row := range b.Rows {
		if !budget.Budgetable(row) {
			continue
		}
		n, u := nm.name(row.Name)
		s := contextService{Name: n, NameUntrusted: u, Utilization: f4(row.Util), OwnMs: f4(row.OwnMs),
			Workers: strconv.Itoa(row.WorkersPerPod)}
		if row.WorkersLower {
			s.Workers += "+"
		}
		if lev := in.Leverage[row.Name]; len(lev) > 0 {
			s.Leverage = map[string]float64{}
			for entry, l := range lev {
				if x := r4(l); x != nil {
					s.Leverage[nm.safe(entry)] = *x
				}
			}
		}
		if row.ErrorPct != nil && !row.HopFromMean {
			s.MisfitPct = r4(*row.ErrorPct)
		}
		v.Services = append(v.Services, s)
	}
	byKey(v.Services, func(s contextService) float64 { return s.Utilization }, func(s contextService) string { return s.Name })
	if len(v.Entries) > MaxContextEntries {
		v.Entries, v.Truncated = v.Entries[:MaxContextEntries], true
	}
	if len(v.Services) > MaxContextServices {
		v.Services, v.Truncated = v.Services[:MaxContextServices], true
	}
	t := trim{findings: &v.Findings, assumptions: &v.Assumptions, omitted: &v.FindingsOmitted, truncated: &v.Truncated,
		size: func() (int, error) { return size(v) },
		dropRow: func() bool {
			switch {
			case len(v.Services) > 1:
				v.Services = v.Services[:len(v.Services)-1]
			case len(v.Entries) > 1:
				v.Entries = v.Entries[:len(v.Entries)-1]
			default:
				return false
			}
			return true
		},
		// The last stage, when the doubts are gone too: the last service, then the last entry.
		last: []func() bool{func() bool {
			switch {
			case len(v.Services) > 0:
				v.Services = v.Services[:len(v.Services)-1]
			case len(v.Entries) > 0:
				v.Entries = v.Entries[:len(v.Entries)-1]
			default:
				return false
			}
			v.Truncated = true
			return true
		}}}
	if err := t.run(MaxBytes(in.Detail)); err != nil {
		return nil, err
	}
	return contract.Canonical(v)
}
