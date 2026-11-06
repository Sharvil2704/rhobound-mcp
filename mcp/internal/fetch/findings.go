// SPDX-License-Identifier: Apache-2.0

package fetch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
)

// The codes the fetcher raises, from the one registry (DATA.md 3.14). The empty window has no store code of its
// own: it is refused as the capture it would become, one with no entry requests.
const (
	codeUnreadable    = "store.unreadable_object"
	codeCompression   = "store.compression_mismatch"
	codeMismatch      = "store.manifest_mismatch"
	codeTooLarge      = "store.too_large"
	codeTZOffset      = "store.partition_tz_offset"
	codeLateBatches   = "store.late_batches"
	codeSmallBatches  = "store.small_batches"
	codeTooFewEntries = "trace.too_few_entries"
)

// registryFix is the registry's fix for a code (store.too_large's: the per-object cap is a constant, so no fix of the
// fetcher's may tell the reader to raise it).
func registryFix(code string) string {
	c, _ := registry.Lookup(code)
	return c.Fix
}

// Finding is a finding of the data contract (defs.v1.json Finding).
type Finding struct {
	Code     string             `json:"code"`
	Level    string             `json:"level"`
	Subject  string             `json:"subject,omitempty"`
	Numbers  map[string]float64 `json:"numbers,omitempty"`
	Examples []string           `json:"examples,omitempty"`
	Fix      string             `json:"fix"`
}

// Refusal is a coded refusal (defs.v1.json Refusal).
type Refusal struct {
	Code    string             `json:"code"`
	Reason  string             `json:"reason"`
	Fix     string             `json:"fix"`
	Subject string             `json:"subject,omitempty"`
	Numbers map[string]float64 `json:"numbers,omitempty"`
}

// A RefusalError is a window the fetcher will not turn into a capture. Refusal is what a job reports; Finding is
// the same refusal as a finding, with up to three example objects. Except for store.manifest_mismatch, which is
// found while downloading, nothing was downloaded in full, and nothing is left on disk.
type RefusalError struct {
	Refusal Refusal
	Finding Finding
}

func (e *RefusalError) Error() string {
	return "fetch refused: " + e.Refusal.Code + ": " + e.Refusal.Reason
}

// RefusalJSON returns the refusal as a defs.v1.json Refusal object.
func (e *RefusalError) RefusalJSON() []byte {
	b, _ := json.Marshal(e.Refusal)
	return b
}

func refuse(code, subject, reason, fix string, numbers map[string]float64, examples []string) *RefusalError {
	return &RefusalError{
		Refusal: Refusal{Code: code, Reason: reason, Fix: fix, Subject: subject, Numbers: numbers},
		Finding: Finding{Code: code, Level: "refuse", Subject: subject, Numbers: numbers, Examples: examples, Fix: fix},
	}
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)

// exampleName names an object in a finding. A finding's examples must match the Name charset, which has no "=",
// so the partition path is written as its minute ("2026-10-26T12:00/traces_x.json.gz"); a key that still falls
// outside the charset is named by its hash, "obj_<first 8 hex of the key's sha256>".
func exampleName(rel string) string {
	if t, ok := partitionPrefix(rel); ok {
		rel = t.Format("2006-01-02T15:04") + "/" + rel[strings.Index(rel, "minute=")+len("minute=00/"):]
	}
	if nameRE.MatchString(rel) {
		return rel
	}
	sum := sha256.Sum256([]byte(rel))
	return "obj_" + hex.EncodeToString(sum[:4])
}

var leadingPartitionRE = regexp.MustCompile(`^year=\d{4}/month=\d{2}/day=\d{2}/hour=\d{2}/minute=\d{2}/`)

func partitionPrefix(rel string) (time.Time, bool) {
	if !leadingPartitionRE.MatchString(rel) {
		return time.Time{}, false
	}
	return parsePartition(rel)
}

func examples(objs []*listed) []string {
	var out []string
	for _, o := range objs[:min(len(objs), 3)] {
		out = append(out, exampleName(o.rel))
	}
	return out
}
