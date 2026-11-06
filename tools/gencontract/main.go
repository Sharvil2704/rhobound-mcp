// SPDX-License-Identifier: Apache-2.0

// Command gencontract writes the files derived from the data contract's one registry (internal/contract), and the
// fixtures of the MCP server's acceptance tests. Run it from the repository's root:
//
//	go run ./tools/gencontract docs                 the code tables of docs/mcp/DATA.md (3.14, 19) and
//	                                                docs/mcp/data-contract.json
//	go run ./tools/gencontract loop OUT             the scripted agent's fixture (testdata/mcp/loop) but its judge answer
//	go run ./tools/gencontract answer RHOBOUND DIR  DIR/judge-answer.json, computed with the rhobound binary RHOBOUND
//	go run ./tools/gencontract hostile OUT          the hostile-names fixture (testdata/mcp/hostile)
//	go run ./tools/gencontract fixture CODE OUT     the capture of internal/testgen's fixture for CODE (for the server's
//	                                                tests, which cannot import testgen)
//	go run ./tools/gencontract canaries OUT         the canary captures of internal/testgen and the table of what each
//	                                                one planted (for the server's leak sweep)
//
// The tests in this folder fail when a generated file is not current.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/testgen"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gencontract:", err)
		os.Exit(2)
	}
}

const usage = "usage: gencontract docs | loop OUT | answer RHOBOUND DIR | hostile OUT | fixture CODE OUT | canaries OUT"

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch {
	case args[0] == "docs" && len(args) == 1:
		root, err := testgen.RepoRoot()
		if err != nil {
			return err
		}
		return WriteDocs(root)
	case args[0] == "loop" && len(args) == 2:
		return Loop(args[1])
	case args[0] == "answer" && len(args) == 3:
		b, err := CannedAnswer(args[1], args[2])
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(args[2], "judge-answer.json"), b, 0o644)
	case args[0] == "hostile" && len(args) == 2:
		return Hostile(args[1])
	case args[0] == "fixture" && len(args) == 3:
		return Fixture(args[1], args[2])
	case args[0] == "canaries" && len(args) == 2:
		return Canaries(args[1])
	}
	return errors.New(usage)
}

// WriteDocs rewrites the generated parts of the docs under the repository's root.
func WriteDocs(root string) error {
	p := filepath.Join(root, filepath.FromSlash(DataDoc))
	doc, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	out, err := RewriteDoc(doc, contract.All())
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, out, 0o644); err != nil {
		return err
	}
	js, err := ContractJSON(contract.All())
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, filepath.FromSlash(DataContract)), js, 0o644)
}
