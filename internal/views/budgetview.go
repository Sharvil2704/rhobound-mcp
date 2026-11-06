// SPDX-License-Identifier: Apache-2.0

package views

import (
	"github.com/Sharvil2704/rhobound-mcp/internal/budget"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/gate"
)

// BudgetInput is what the budget view is built from: the items budget.Compute returned, ranked by leverage, whether
// it cut them, and the policy they were computed under (nil: none, so no threshold).
type BudgetInput struct {
	FitDigest string
	// Names are the names of the fit's nodes (its services), which the views show one to one (sanitize.Services).
	Names     []string
	Items     []budget.Item
	Truncated bool
	Policy    *gate.Policy
}

type budgetView struct {
	Schema    string       `json:"schema"`
	FitDigest string       `json:"fit_digest"`
	Items     []budgetItem `json:"items"`
	Assumes   []string     `json:"assumes"`
	// Findings are budget.Findings: budget.no_policy without a policy, else none (and no key, so that a budget under
	// a policy prints what it did before the key existed).
	Findings  []contract.Finding `json:"findings,omitempty"`
	Truncated bool               `json:"truncated"`
	Scope     string             `json:"scope"`
}

type budgetItem struct {
	Service           string   `json:"service"`
	ServiceUntrusted  bool     `json:"service_untrusted,omitempty"`
	Kind              string   `json:"kind"`
	Per               string   `json:"per"`
	MaxValue          *float64 `json:"max_value"`
	SafeValue         *float64 `json:"safe_value"`
	BindingConstraint string   `json:"binding_constraint,omitempty"`
	BindingEntry      string   `json:"binding_entry,omitempty"`
	AtLimit           *atLimit `json:"at_limit,omitempty"`
}

type atLimit struct {
	EntryChangeMs    *float64 `json:"entry_change_ms,omitempty"`
	UtilizationAfter *float64 `json:"utilization_after,omitempty"`
}

// Budget returns the budget view (rhobound-mcp.budget/v1): how much each service may add per visit, max_value and
// safe_value, with the constraint that binds, and budget.no_policy when no policy applies. It is always a summary: the
// items it drops to fit are those with the lowest leverage, and truncated says so; a finding is never dropped.
func Budget(in BudgetInput) ([]byte, error) {
	v := &budgetView{Schema: BudgetSchema, FitDigest: in.FitDigest, Items: []budgetItem{},
		Assumes: budget.Assumes(in.Items, in.Policy), Truncated: in.Truncated, Scope: Scope}
	if fs := budget.Findings(in.Policy); len(fs) > 0 {
		v.Findings = findings(fs)
	}
	nm := namerOf(in.Names)
	for _, it := range in.Items {
		v.Items = append(v.Items, nm.item(it))
	}
	if len(v.Items) > budget.MaxItems {
		v.Items, v.Truncated = v.Items[:budget.MaxItems], true
	}
	for {
		n, err := size(v)
		if err != nil {
			return nil, err
		}
		if n <= SummaryMaxBytes {
			break
		}
		if len(v.Items) == 0 {
			return nil, &TooLargeError{Size: n, Max: SummaryMaxBytes}
		}
		v.Items, v.Truncated = v.Items[:len(v.Items)-1], true
	}
	return contract.Canonical(v)
}

func (nm namer) item(it budget.Item) budgetItem {
	n, u := nm.name(it.Service)
	b := budgetItem{Service: n, ServiceUntrusted: u, Kind: it.Kind, Per: it.Per, BindingConstraint: it.Binding}
	if it.Max != nil {
		b.MaxValue = r4(*it.Max)
	}
	if it.Safe != nil {
		b.SafeValue = r4(*it.Safe)
	}
	if it.BindingEntry != "" {
		b.BindingEntry = nm.safe(it.BindingEntry)
	}
	if a := it.AtLimit; a != nil && it.Max != nil {
		al := &atLimit{}
		if a.EntryChangeMs != nil {
			al.EntryChangeMs = r4(*a.EntryChangeMs)
		}
		if a.UtilizationAfter != nil {
			al.UtilizationAfter = r4(*a.UtilizationAfter)
		}
		if al.EntryChangeMs != nil || al.UtilizationAfter != nil {
			b.AtLimit = al
		}
	}
	return b
}
