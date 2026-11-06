// SPDX-License-Identifier: Apache-2.0

// Command gentestdata writes the repository's generated test captures, from testdata/capture-chain: the data
// contract's defect fixtures (one folder per finding code) and the base of the scripted agent's capture. It is the one
// fixture generator; the generators are in internal/testgen. Run it from the repository.
//
//	go run ./tools/gentestdata fixtures OUT   every injected-defect fixture, in OUT/<code>/
//	go run ./tools/gentestdata loop OUT       testdata/capture-chain with image digests and its request count
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gentestdata:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: gentestdata fixtures|loop OUT")
	}
	out := args[1]
	if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
		return fmt.Errorf("%s is not empty", out)
	}
	base, err := testgen.Base()
	if err != nil {
		return err
	}
	switch args[0] {
	case "fixtures":
		for _, f := range testgen.Fixtures() {
			if f.Build == nil {
				continue
			}
			dir := filepath.Join(out, f.Code)
			if _, err := f.Build(base.Clone(), dir); err != nil {
				return fmt.Errorf("%s: %w", f.Code, err)
			}
		}
		return nil
	case "loop":
		c, count := testgen.Loop(base)
		if err := c.Write(out, "spans.jsonl.gz"); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(out, "request-count.json"), count, 0o644)
	}
	return fmt.Errorf("unknown target %q (fixtures or loop)", args[0])
}
