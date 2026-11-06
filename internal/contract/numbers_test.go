// SPDX-License-Identifier: Apache-2.0

package contract_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// numberSites are the places, in either Go module, that write a number's name the scan below cannot tie to one code
// by the code alone: a finding built on a variable or a code given as an argument, a map of numbers handed to a
// refusal, or a name computed at run time. Each is keyed "file function name" (name is the literal, or the Go
// expression of a computed one) and lists the codes it may write that name for, and, for a computed name, every name
// it may take. A copy of another finding's or refusal's numbers is keyed with the expression and lists nothing.
var numberSites = map[string]struct{ codes, names []string }{
	// check: a reader's refusal as a finding (readFinding), and the sniffed formats of an object (sniff)
	"internal/check/check.go readFinding object": {codes: []string{contract.TraceBadJSON, contract.TraceKindUnknown,
		contract.StoreCompressionMismatch, contract.StoreManifestMismatch}},
	"internal/check/check.go readFinding line": {codes: []string{contract.TraceBadJSON, contract.TraceKindUnknown,
		contract.StoreCompressionMismatch, contract.StoreManifestMismatch}},
	"internal/check/check.go readFinding value": {codes: []string{contract.TraceBadJSON, contract.TraceKindUnknown}},
	"internal/check/check.go objectRefs what": {codes: []string{contract.StoreManifestMismatch},
		names: []string{"missing", "size"}},
	"internal/check/sniff.go Sniff object": {codes: []string{contract.StoreUnreadableObject,
		contract.StoreCompressionMismatch, contract.TraceJSONLPretty, contract.TraceJSONArray, contract.TraceNotOTLP,
		contract.TraceSnakeCaseKeys, contract.TracePreV1OTLP, contract.TraceIDsMixed, contract.TraceIDsBase64,
		contract.TraceTraceID64Bit, contract.TraceKindEnumName, contract.TraceKindUnknown}},
	"internal/check/sampling.go sampling entries_without_th": {codes: []string{contract.TraceSamplingMixed}},
	"internal/check/sampling.go sampling strings.ReplaceAll(x.Source, \"-\", \"_\")": {
		codes: []string{contract.TraceKeptShareConflict}, names: []string{"counted", "derived_th", "declared", "assumed"}},
	"internal/gate/gate.go fitAge fit_age_h": {codes: []string{contract.FitStale}},
	// change: a change set's refusals, and the doubts of the records it cites (copied)
	"internal/change/change.go finding k":             {},
	"internal/change/change.go check lo":              {codes: []string{contract.ChangeRangeInverted}},
	"internal/change/change.go check hi":              {codes: []string{contract.ChangeRangeInverted}},
	"internal/change/change.go check own_ms":          {codes: []string{contract.ChangeNegativeTime}},
	"internal/change/change.go check glue_ms":         {codes: []string{contract.ChangeNegativeTime}},
	"internal/change/change.go check change_ms":       {codes: []string{contract.ChangeNegativeTime}},
	"internal/change/change.go check var_delta":       {codes: []string{contract.ProfileVarianceChanged}},
	"internal/change/change.go check var_se":          {codes: []string{contract.ProfileVarianceChanged}},
	"internal/change/change.go check unsigned_ranges": {codes: []string{contract.ChangeTooManyRanges}},
	"internal/change/change.go check limit":           {codes: []string{contract.ChangeTooManyRanges}},
	// the engine's and the model's refusals
	"internal/engine/engine.go LimitError utilization": {codes: []string{contract.ModelUtilizationObserved,
		contract.ModelUtilizationAfter}},
	"internal/engine/engine.go LimitError limit": {codes: []string{contract.ModelUtilizationObserved,
		contract.ModelUtilizationAfter}},
	"internal/infer/infer.go Run busy_share":     {codes: []string{contract.TraceStreams}},
	"internal/report/refusal.go RefusalOf limit": {codes: []string{contract.InputTooLarge}},
	// the S3 window fetcher (mcp/internal/fetch) and the profile checks (mcp/internal/oci) of the MCP module
	"mcp/internal/fetch/fetch.go fetchInto late_spans":                             {codes: []string{contract.StoreLateBatches}},
	"mcp/internal/fetch/fetch.go fetchInto margin_after_s":                         {codes: []string{contract.StoreLateBatches}},
	"mcp/internal/fetch/fetch.go admit listed_bytes":                               {codes: []string{contract.StoreTooLarge}},
	"mcp/internal/fetch/fetch.go admit cap_bytes":                                  {codes: []string{contract.StoreTooLarge}},
	"mcp/internal/fetch/fetch.go admit objects":                                    {codes: []string{contract.StoreTooLarge, contract.StoreUnreadableObject, contract.StoreCompressionMismatch}},
	"mcp/internal/fetch/fetch.go tzRefusal derived_offset_h":                       {codes: []string{contract.StorePartitionTZOffset}},
	"mcp/internal/fetch/fetch.go tzRefusal objects_sampled":                        {codes: []string{contract.StorePartitionTZOffset}},
	"mcp/internal/fetch/fetch.go emptyWindow objects":                              {codes: []string{contract.TraceTooFewEntries}},
	"mcp/internal/fetch/fetch.go download objects":                                 {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/fetch/fetch.go download listed_bytes":                            {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/fetch/fetch.go download read_bytes":                              {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/fetch/fetch.go smallBatchesFinding objects_per_minute":           {codes: []string{contract.StoreSmallBatches}},
	"mcp/internal/fetch/fetch.go smallBatchesFinding put_requests_per_month":       {codes: []string{contract.StoreSmallBatches}},
	"mcp/internal/fetch/fetch.go smallBatchesFinding put_cost_usd_per_month":       {codes: []string{contract.StoreSmallBatches}},
	"mcp/internal/fetch/fetch.go smallBatchesFinding threshold_objects_per_minute": {codes: []string{contract.StoreSmallBatches}},
	"mcp/internal/fetch/fetch.go countFiles files":                                 {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/fetch/fetch.go countFiles objects":                               {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/fetch/fetch.go countFiles other_entries":                         {codes: []string{contract.StoreManifestMismatch}},
	"mcp/internal/oci/check.go Check age_days":                                     {codes: []string{contract.ProfileStale}},
	"mcp/internal/oci/check.go Check valid_for_days":                               {codes: []string{contract.ProfileStale}},
	"mcp/internal/oci/check.go Check seconds_apart":                                {codes: []string{contract.ProfileTimeDisagrees}},
	"mcp/internal/oci/check.go Check fields":                                       {codes: []string{contract.ProfileHardwareMismatch}},
	"mcp/internal/oci/check.go Check pod_rps":                                      {codes: []string{contract.ProfileLoadOutOfRange}},
	"mcp/internal/oci/check.go Check measured_lo":                                  {codes: []string{contract.ProfileLoadOutOfRange}},
	"mcp/internal/oci/check.go Check measured_hi":                                  {codes: []string{contract.ProfileLoadOutOfRange}},
	"mcp/internal/oci/check.go Check candidate_utilization":                        {codes: []string{contract.ProfileLoadOutOfRange}},
	"mcp/internal/oci/check.go Check arm_levels_short":                             {codes: []string{contract.ProfileTooFewVisits}},
	"mcp/internal/oci/check.go Check visits_needed":                                {codes: []string{contract.ProfileTooFewVisits}},
	"mcp/internal/oci/check.go Check margin_ms":                                    {codes: []string{contract.ProfileTooFewVisits}},
	"mcp/internal/oci/check.go Check levels": {codes: []string{contract.ProfileLevelDrift, contract.ProfileStructureChanged,
		contract.ProfileVarianceChanged, contract.ProfileSelfReportMismatch, contract.ProfileErrorsDiffer}},
	"mcp/internal/oci/profile.go ParseProfile fields": {codes: []string{contract.ProfileLocationsPresent, contract.ProfileNoVariance,
		contract.ProfileNoStructure, contract.ProfileNoOutsideMeasure, contract.ProfileScopeNotMean,
		contract.ProfilePredicateType}},
	"mcp/internal/server/tools.go rateLimited retry_after_ms": {codes: []string{contract.RateLimited}},
}

// TestNumbersAreRegistered: every name of a number that the engine, the server or its fetcher writes into a finding
// or a refusal is one of its code's (Code.Numbers), since CheckStored refuses a stored finding with another, and the
// server shows a stored record's numbers only by these names. The scan reads every Go file outside the tests: a
// WithNumber on a finding of a code constant is checked against that code; a map of numbers given with a code
// constant (Code: contract.X, Numbers: ...) too; any other WithNumber or map of numbers with string keys, and every
// computed name, is a numberSites entry. TestFixtureRecall checks the findings check makes on every fixture too.
func TestNumbersAreRegistered(t *testing.T) {
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	consts := codeConstants(t, filepath.Join(root, "internal", "contract", "codes.go"))
	has := func(code, name string) bool {
		c, ok := contract.Lookup(code)
		return ok && slices.Contains(c.Numbers, name)
	}
	seen := map[string]bool{}
	site := func(pos token.Position, rel, fn, name string, computed bool) {
		key := rel + " " + fn + " " + name
		seen[key] = true
		s, ok := numberSites[key]
		if !ok {
			t.Errorf("%s: a number %s is written for no code the scan can tell: add %q to numberSites", pos, name, key)
			return
		}
		names := []string{name}
		if computed {
			names = s.names
		}
		for _, code := range s.codes {
			for _, n := range names {
				if !has(code, n) {
					t.Errorf("%s: %s writes the number %s, which %s does not list", pos, key, n, code)
				}
			}
		}
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			switch e.Name() {
			case ".git", "node_modules", "target", "vendor", "testdata", "rust":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Errorf("%s does not parse: %v", rel, err)
			return nil
		}
		local := contractImportName(f)
		if filepath.Dir(p) == filepath.Join(root, "internal", "contract") {
			local = "" // the package itself, whose own findings name codes by their bare constants
		}
		codeOf := func(x ast.Expr) string {
			if sel, ok := x.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && local != "" && id.Name == local {
					return consts[sel.Sel.Name]
				}
			}
			return ""
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			attributed := map[*ast.CompositeLit]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					sel, ok := x.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "WithNumber" || len(x.Args) != 2 {
						return true
					}
					pos := fset.Position(x.Pos())
					lit, isLit := x.Args[0].(*ast.BasicLit)
					if !isLit || lit.Kind != token.STRING {
						site(pos, rel, fn, exprString(fset, x.Args[0]), true)
						return true
					}
					name, _ := strconv.Unquote(lit.Value)
					if code := codeOf(findingCode(sel.X)); code != "" {
						if !has(code, name) {
							t.Errorf("%s: a finding of %s carries the number %s, which the code does not list", pos, code, name)
						}
						return true
					}
					site(pos, rel, fn, name, false)
				case *ast.CompositeLit:
					// Code: contract.X with Numbers: map[string]float64{...}
					var code string
					var nums *ast.CompositeLit
					for _, el := range x.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch k, _ := kv.Key.(*ast.Ident); {
						case k != nil && k.Name == "Code":
							code = codeOf(kv.Value)
						case k != nil && k.Name == "Numbers":
							nums, _ = kv.Value.(*ast.CompositeLit)
						}
					}
					if code != "" && nums != nil {
						attributed[nums] = true
						for _, name := range mapKeys(t, fset, nums) {
							if !has(code, name) {
								t.Errorf("%s: a refusal of %s carries the number %s, which the code does not list",
									fset.Position(nums.Pos()), code, name)
							}
						}
						return true
					}
					if mt, ok := x.Type.(*ast.MapType); ok && exprString(fset, mt.Value) == "float64" && !attributed[x] {
						for _, name := range mapKeys(t, fset, x) {
							site(fset.Position(x.Pos()), rel, fn, name, false)
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var stale []string
	for k := range numberSites {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		t.Errorf("numberSites lists %q, which the scan no longer finds", k)
	}
}

// findingCode returns the argument of NewFinding at the start of a chain of calls on a finding
// (contract.NewFinding(X).WithSubject(s).WithNumber(...)), or nil when the chain starts elsewhere (a variable).
func findingCode(e ast.Expr) ast.Expr {
	for {
		call, ok := e.(*ast.CallExpr)
		if !ok {
			return nil
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name == "NewFinding" && len(call.Args) == 1 {
				return call.Args[0]
			}
			e = fun.X
		case *ast.Ident:
			if fun.Name == "NewFinding" && len(call.Args) == 1 {
				return call.Args[0]
			}
			return nil
		default:
			return nil
		}
	}
}

// mapKeys returns the keys of a map literal; a key that is not a string literal is a computed name, which no map of
// numbers has.
func mapKeys(t *testing.T, fset *token.FileSet, m *ast.CompositeLit) []string {
	t.Helper()
	var out []string
	for _, el := range m.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		lit, ok := kv.Key.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s: a map of numbers with a computed key", fset.Position(kv.Pos()))
			continue
		}
		s, _ := strconv.Unquote(lit.Value)
		out = append(out, s)
	}
	return out
}

// exprString is an expression as Go source, for the keys of numberSites.
func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return fmt.Sprintf("%T", e)
	}
	return b.String()
}
