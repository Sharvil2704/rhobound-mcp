// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"path/filepath"

	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

// CanaryTable is the file Canaries writes beside the captures: what each capture planted.
const CanaryTable = "canaries.json"

// canaryEntry is one capture of the table.
type canaryEntry struct {
	Name    string            `json:"name"`
	What    string            `json:"what"`
	Pair    bool              `json:"pair"`
	Refused bool              `json:"refused"`
	Planted []testgen.Planted `json:"planted"`
}

// Canaries writes the canary captures of internal/testgen (testgen.Canaries) into out, which must not exist or be
// empty, for the MCP server's leak sweep: a capture into out/NAME, the loop capture (image digests and its request
// count) with the canary's values planted; an A/B pair into out/NAME/baseline and out/NAME/candidate, the loop
// fixture's ab-shift pair with the values planted in each arm, written as the loop fixture's pairs are; and the table
// of what each one planted into out/canaries.json.
func Canaries(out string) error {
	if err := emptyDir(out); err != nil {
		return err
	}
	var table []canaryEntry
	for _, k := range testgen.Canaries() {
		base, err := testgen.Base()
		if err != nil {
			return err
		}
		c, _ := testgen.Loop(base)
		dir := filepath.Join(out, k.Name)
		if k.Pair {
			head := c.Clone()
			testgen.Window(head, 0, varianceWindow)
			a, b := testgen.ABShift(head)
			k.Build(a)
			k.Build(b)
			if err := writeAB(dir, a, b); err != nil {
				return err
			}
		} else {
			files := k.Build(c)
			if err := c.Write(dir, "spans.jsonl.gz"); err != nil {
				return err
			}
			if err := testgen.WriteFile(filepath.Join(dir, "request-count.json"), testgen.RequestCount(c, entries(c))); err != nil {
				return err
			}
			for p, b := range files {
				if err := testgen.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), b); err != nil {
					return err
				}
			}
		}
		table = append(table, canaryEntry{Name: k.Name, What: k.What, Pair: k.Pair, Refused: k.Refused, Planted: k.Planted})
	}
	b, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		return err
	}
	return testgen.WriteFile(filepath.Join(out, CanaryTable), append(b, '\n'))
}
