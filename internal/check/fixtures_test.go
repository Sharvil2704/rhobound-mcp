// SPDX-License-Identifier: Apache-2.0

package check_test

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/check"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

var (
	baseOnce sync.Once
	baseCap  *testgen.Capture
	baseErr  error
)

func base(t *testing.T) *testgen.Capture {
	baseOnce.Do(func() { baseCap, baseErr = testgen.Base() })
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	return baseCap.Clone()
}

// runCase checks a fixture's capture as `rhobound check` would.
func runCase(t *testing.T, cs *testgen.Case) *contract.CheckReport {
	t.Helper()
	opt := check.Options{DeclaredKeptShare: cs.KeptShare, DeclaredSamplers: cs.Samplers,
		ExtraProbeRoutes: cs.ProbeRoutes, PodIdentity: cs.PodIdentity}
	if cs.Limits != nil {
		opt.Limits = *cs.Limits
	}
	var m *contract.Manifest
	var err error
	if cs.Manifest != "" {
		m, err = check.ReadManifest(cs.Manifest)
	} else {
		m, _, err = check.ManifestFromDir(cs.Dir, opt.Limits)
	}
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(m.Dir, "request-count.json")); err == nil {
		if opt.RequestCount, err = check.ParseRequestCount(b); err != nil {
			t.Fatal(err)
		}
	}
	rep, _, err := check.Run(m, opt)
	if err != nil {
		t.Fatal(err)
	}
	// every number of a finding is one its code lists, as a stored record's must be (contract.CheckStored)
	for _, f := range rep.Findings {
		c, _ := contract.Lookup(f.Code)
		for k := range f.Numbers {
			if !slices.Contains(c.Numbers, k) {
				t.Errorf("a finding of %s carries the number %s, which the code does not list", f.Code, k)
			}
		}
	}
	return rep
}

func codesOf(r *contract.CheckReport) string {
	var out []string
	for _, f := range r.Findings {
		s := f.Code
		if f.Subject != "" {
			s += "(" + f.Subject + ")"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

// Defect recall: every fixture that check is to catch raises its code (DATA.md 17), including the four silent-wrong
// classes: head sampling undeclared (trace.sampling_unverified, fit.queue_rebuild_sampled), no CLIENT spans
// (trace.client_spans_missing), context not propagated (trace.context_not_propagated) and duplicate spans
// (trace.duplicate_spans_dropped, trace.span_id_collision).
func TestFixtureRecall(t *testing.T) {
	for _, f := range testgen.Fixtures() {
		if f.Stage != testgen.StageCheck {
			continue
		}
		t.Run(f.Code, func(t *testing.T) {
			cs, err := f.Build(base(t), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rep := runCase(t, cs)
			if !rep.Has(f.Code) {
				t.Errorf("%s (%s): not raised; the report has %s", f.Code, f.What, codesOf(rep))
			}
			c, _ := contract.Lookup(f.Code)
			if c.Level == contract.Refuse && rep.Verdict != contract.Refused || c.Level != contract.Refuse && rep.Verdict == contract.Refused &&
				!expectedRefusal[f.Code] {
				t.Errorf("%s: verdict %s with %s", f.Code, rep.Verdict, codesOf(rep))
			}
		})
	}
}

// expectedRefusal are the doubt and note fixtures whose defect also refuses the capture by another code.
var expectedRefusal = map[string]bool{}

// The clean captures of the repository: no refusal but the one the CLI makes too (a capture with no SERVER spans),
// and exactly these doubts. trace.image_digest_missing is a doubt on every capture whose resources carry no
// container.image.repo_digests (none of the repository's do); trace.arrivals_unjudged on captures under 900 entries;
// trace.whole_ms_starts on the OpenTelemetry Demo's Node.js frontend, whose clock stamps whole milliseconds.
var cleanCaptures = map[string]string{
	"testdata/capture-chain": "trace.image_digest_missing(a) trace.image_digest_missing(b) trace.image_digest_missing(c) " +
		"trace.sampling_unverified",
	"internal/infer/testdata/counts/shipped-20s": "trace.arrivals_unjudged " + digestsMissing("ad", "cart", "checkout", "currency",
		"email", "flagd", "frontend", "frontend-proxy", "image-provider", "payment", "product-catalog", "quote", "recommendation",
		"shipping") + " trace.sampling_unverified trace.whole_ms_starts(frontend-554669cf7c-z98zc)",
	"internal/infer/testdata/hops/browse-45s": "trace.arrivals_unjudged " + digestsMissing("frontend", "frontend-proxy",
		"image-provider", "product-catalog") + " trace.sampling_unverified trace.whole_ms_starts(frontend-554669cf7c-z98zc)",
	"internal/infer/testdata/hops/cache-30s": digestsMissing("api", "cache", "db", "frontend") + " trace.sampling_unverified",
	"internal/infer/testdata/nested/cart-client-in-client": "trace.arrivals_unjudged " + digestsMissing("cart", "checkout",
		"currency", "email", "flagd", "frontend", "frontend-proxy", "payment", "product-catalog", "quote", "shipping") +
		" trace.sampling_unverified",
	"internal/infer/testdata/nested/flagd-server-in-server": "trace.arrivals_unjudged " + digestsMissing("cart", "checkout",
		"currency", "email", "flagd", "frontend", "frontend-proxy", "payment", "product-catalog", "quote", "shipping") +
		" trace.sampling_unverified",
	"internal/infer/testdata/nested/frontend-server-in-server": "trace.arrivals_unjudged " + digestsMissing("cart", "frontend",
		"frontend-proxy", "product-catalog", "recommendation") + " trace.sampling_unverified",
	"internal/infer/testdata/nested/fraud-detection-consumer-in-consumer": "trace.no_server_spans",
}

func digestsMissing(svcs ...string) string {
	sort.Strings(svcs)
	var out []string
	for _, s := range svcs {
		out = append(out, "trace.image_digest_missing("+s+")")
	}
	return strings.Join(out, " ")
}

func TestCleanCaptures(t *testing.T) {
	root, err := testgen.RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range cleanCaptures {
		t.Run(rel, func(t *testing.T) {
			rep := runCase(t, &testgen.Case{Dir: filepath.Join(root, filepath.FromSlash(rel))})
			if got := codesOf(rep); got != want {
				t.Errorf("findings\n  got:  %s\n  want: %s", got, want)
			}
			wantVerdict := contract.AdmittedWithDoubts
			if strings.HasPrefix(want, "trace.no_server_spans") {
				wantVerdict = contract.Refused
			}
			if rep.Verdict != wantVerdict || rep.KeptShare != (contract.KeptShare{Value: 1, Source: contract.KeptAssumed}) {
				t.Errorf("verdict %s, kept share %+v", rep.Verdict, rep.KeptShare)
			}
		})
	}
}
