// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
)

// Definitions is all the text of this server that a model reads, as one canonical JSON document: for each profile,
// the server instructions, the tool definitions (as tools/list shows them, by name) and the prompts (their
// definitions and fixed texts). Each release publishes its sha256 (ARCHITECTURE.md 5.11); mcp/DEFINITIONS.sha256
// holds it, and a test fails when the text changes without it.
func Definitions() ([]byte, error) {
	doc := map[string]any{}
	for _, profile := range []string{config.ProfileDefault, config.ProfileBench} {
		var tools []any
		sps := profileSpecs(profile)
		sort.Slice(sps, func(i, j int) bool { return sps[i].name < sps[j].name })
		for _, sp := range sps {
			def, _, err := definition(sp)
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(def)
			if err != nil {
				return nil, err
			}
			tools = append(tools, json.RawMessage(b))
		}
		texts := promptTexts(profile)
		var prompts []any
		for _, p := range promptDefs() {
			b, err := json.Marshal(p)
			if err != nil {
				return nil, err
			}
			prompts = append(prompts, map[string]any{"definition": json.RawMessage(b), "text": texts[p.Name]})
		}
		doc[profile] = map[string]any{"instructions": instructions(profile), "tools": tools, "prompts": prompts}
	}
	return encode(doc)
}

// DefinitionsSHA256 is the hex sha256 of Definitions.
func DefinitionsSHA256() (string, error) {
	b, err := Definitions()
	if err != nil {
		return "", err
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}
