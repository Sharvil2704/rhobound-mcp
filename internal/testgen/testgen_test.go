// SPDX-License-Identifier: Apache-2.0

package testgen

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

var (
	baseOnce sync.Once
	baseCap  *Capture
	baseErr  error
)

func base(t *testing.T) *Capture {
	baseOnce.Do(func() { baseCap, baseErr = Base() })
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	return baseCap.Clone()
}

// Every code a capture triggers has an injected fixture, and every fixture names a registered capture code (DATA.md
// 17): a fixture class without a code, or a capture code without a fixture, fails.
func TestEveryCaptureCodeHasAFixture(t *testing.T) {
	have := map[string]int{}
	for _, f := range Fixtures() {
		c, ok := contract.Lookup(f.Code)
		switch {
		case !ok:
			t.Errorf("fixture %q names an unregistered code", f.Code)
		case c.Trigger != contract.Capture:
			t.Errorf("fixture %q: the code's trigger is %s, not capture: it needs a unit test, not a fixture", f.Code, c.Trigger)
		}
		switch f.Stage {
		case StageCheck, StageFit, StagePredict:
			if f.Build == nil {
				t.Errorf("fixture %q has no builder", f.Code)
			}
		case StageFetch:
			if f.Build != nil {
				t.Errorf("fixture %q: a fetch-stage defect is injected by the fetcher's own store fake", f.Code)
			}
		default:
			t.Errorf("fixture %q: stage %q", f.Code, f.Stage)
		}
		if f.What == "" {
			t.Errorf("fixture %q says nothing of its defect", f.Code)
		}
		have[f.Code]++
	}
	for _, c := range contract.All() {
		if c.Trigger != contract.Capture || c.Status == contract.StatusLater || c.Status == contract.StatusResearch {
			continue
		}
		if have[c.ID] == 0 {
			t.Errorf("%s: a capture code with no injected fixture", c.ID)
		}
	}
}

// Every fixture builds from the base capture, into its folder only, the same bytes each time.
func TestFixturesBuild(t *testing.T) {
	for _, f := range Fixtures() {
		if f.Build == nil {
			continue
		}
		t.Run(f.Code, func(t *testing.T) {
			var sums [2]string
			for i := range sums {
				dir := t.TempDir()
				cs, err := f.Build(base(t), dir)
				if err != nil {
					t.Fatal(err)
				}
				if cs.Dir != dir || cs.Manifest != "" && filepath.Dir(cs.Manifest) != dir {
					t.Fatalf("case %+v outside its folder %s", cs, dir)
				}
				sums[i] = treeSum(t, dir)
			}
			if sums[0] != sums[1] {
				t.Error("two builds wrote different bytes")
			}
		})
	}
}

func treeSum(t *testing.T, dir string) string {
	var b bytes.Buffer
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		b.WriteString(rel + " " + contract.SHA256Hex(data) + "\n")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The base capture round-trips: written back, it reads as the original does.
func TestRoundTrip(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	want, err := otlp.ReadDir(filepath.Join(root, "testdata", "capture-chain"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := base(t).Write(dir, "spans.jsonl.gz"); err != nil {
		t.Fatal(err)
	}
	got, err := otlp.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Spans) != len(want.Spans) || len(got.Resources) != len(want.Resources) {
		t.Fatalf("%d spans, %d resources; want %d, %d", len(got.Spans), len(got.Resources), len(want.Spans), len(want.Resources))
	}
	for i := range got.Spans {
		if got.Spans[i] != want.Spans[i] {
			t.Fatalf("span %d: %+v, want %+v", i, got.Spans[i], want.Spans[i])
		}
	}
}
