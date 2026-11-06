// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// The files the registry generates, relative to the repository's root.
const (
	DataDoc      = "docs/mcp/DATA.md"
	DataContract = "docs/mcp/data-contract.json"
)

// DataContractSchema is the schema id of the data-contract document (docs/mcp/schemas/data-contract.v1.json).
const DataContractSchema = "rhobound.data-contract/v1"

// A region of DATA.md that is generated: everything between its begin and end markers is replaced.
type region struct {
	name  string
	table func([]contract.Code) string
}

var regions = []region{
	{"capture-codes", captureTable},
	{"other-codes", otherTable},
}

func beginMarker(name string) string {
	return "<!-- gencontract:begin " + name +
		" (generated from internal/contract/codes.go by `go run ./tools/gencontract docs`; edit the registry, not this table) -->"
}

func endMarker(name string) string { return "<!-- gencontract:end " + name + " -->" }

// RewriteDoc returns DATA.md with its generated regions replaced by the tables of the registry codes.
func RewriteDoc(doc []byte, codes []contract.Code) ([]byte, error) {
	out := doc
	for _, r := range regions {
		begin, end := []byte(beginMarker(r.name)), []byte(endMarker(r.name))
		i := bytes.Index(out, begin)
		j := bytes.Index(out, end)
		if i < 0 || j < i || bytes.Count(out, begin) != 1 || bytes.Count(out, end) != 1 {
			return nil, fmt.Errorf("%s: the markers of the region %q are missing or repeated", DataDoc, r.name)
		}
		var b bytes.Buffer
		b.Write(out[:i+len(begin)])
		b.WriteString("\n")
		b.WriteString(r.table(codes))
		b.Write(out[j:])
		out = b.Bytes()
	}
	return out, nil
}

// captureTable is DATA.md 3.14: the codes a capture or its store triggers. Consecutive codes with the same level,
// condition, fix, behaviour at 1a2df09 and status share one row.
func captureTable(codes []contract.Code) string {
	var sb strings.Builder
	sb.WriteString("| code | level | condition | fix | today | status |\n|---|---|---|---|---|---|\n")
	for _, g := range group(codes, func(c contract.Code) bool { return c.Trigger == contract.Capture },
		func(c contract.Code) string {
			return strings.Join([]string{string(c.Level), c.When, c.Fix, c.Today, c.Status}, "\x00")
		}) {
		c := g[0]
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s |\n", ids(g), level(c.Level), cell(c.When), cell(c.Fix), cell(c.Today), status(c.Status))
	}
	return sb.String()
}

// otherTable is DATA.md 19: every other code.
func otherTable(codes []contract.Code) string {
	var sb strings.Builder
	sb.WriteString("| code | level | trigger | when | fix | where | status |\n|---|---|---|---|---|---|---|\n")
	for _, g := range group(codes, func(c contract.Code) bool { return c.Trigger != contract.Capture },
		func(c contract.Code) string {
			return strings.Join([]string{string(c.Level), string(c.Trigger), c.When, c.Fix, c.Doc, c.Status}, "\x00")
		}) {
		c := g[0]
		fmt.Fprintf(&sb, "| %s | %s | %s | %s | %s | %s | %s |\n", ids(g), level(c.Level), c.Trigger, cell(c.When), cell(c.Fix),
			where(c.Doc), status(c.Status))
	}
	return sb.String()
}

// group keeps the codes for which keep is true, in the registry's order, and puts consecutive codes with the same key in
// one group.
func group(codes []contract.Code, keep func(contract.Code) bool, key func(contract.Code) string) [][]contract.Code {
	var out [][]contract.Code
	last := ""
	for _, c := range codes {
		if !keep(c) {
			continue
		}
		k := key(c)
		if len(out) > 0 && k == last {
			out[len(out)-1] = append(out[len(out)-1], c)
			continue
		}
		out = append(out, []contract.Code{c})
		last = k
	}
	return out
}

func ids(g []contract.Code) string {
	s := make([]string, len(g))
	for i, c := range g {
		s[i] = "`" + c.ID + "`"
	}
	return strings.Join(s, ", ")
}

func level(l contract.Level) string {
	switch l {
	case contract.Refuse:
		return "R"
	case contract.Doubt:
		return "D"
	case contract.Note:
		return "N"
	}
	return string(l)
}

func status(s string) string { return "[" + s + "]" }

// where names the section that defines a code, relative to DATA.md: its own sections by number, the others by file.
func where(doc string) string {
	parts := strings.Split(doc, "; ")
	for i, p := range parts {
		switch {
		case strings.HasPrefix(p, "DATA.md "):
			parts[i] = strings.TrimPrefix(p, "DATA.md ")
		case strings.HasPrefix(p, "CODEPATHS.md "):
			parts[i] = "[CODEPATHS.md](CODEPATHS.md), " + strings.TrimPrefix(p, "CODEPATHS.md ")
		}
	}
	return cell(strings.Join(parts, "; "))
}

// cell escapes the characters that would end a table cell or start an HTML tag.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.NewReplacer("|", `\|`, "<", `\<`).Replace(s)
}

// contractDoc is the data-contract document: what the MCP server serves as rhobound://data-contract, one row per
// registered code with every field of the registry.
type contractDoc struct {
	Schema   string        `json:"schema"`
	Contract string        `json:"contract"`
	Codes    []contractRow `json:"codes"`
}

type contractRow struct {
	Code    string `json:"code"`
	Level   string `json:"level"`
	Trigger string `json:"trigger"`
	When    string `json:"when"`
	Fix     string `json:"fix"`
	Today   string `json:"today,omitempty"`
	Doc     string `json:"doc"`
	Since   string `json:"since"`
	Status  string `json:"status"`
	// Numbers are the names of the numbers a finding or a refusal of the code may carry.
	Numbers []string `json:"numbers,omitempty"`
}

// ContractJSON is the data-contract document, indented, with a final newline.
func ContractJSON(codes []contract.Code) ([]byte, error) {
	d := contractDoc{Schema: DataContractSchema, Contract: contract.Version, Codes: []contractRow{}}
	for _, c := range codes {
		d.Codes = append(d.Codes, contractRow{Code: c.ID, Level: string(c.Level), Trigger: string(c.Trigger), When: c.When,
			Fix: c.Fix, Today: c.Today, Doc: c.Doc, Since: c.Since, Status: c.Status, Numbers: c.Numbers})
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
