// SPDX-License-Identifier: Apache-2.0

// Package schematest validates documents against the published JSON Schemas (docs/mcp/schemas), read from the
// repository: every file of the folder is loaded, and a reference from one file to another is resolved by the files'
// $id, without the network. Its tests validate the goldens and the real outputs of both modules against the schemas,
// so that a schema and the document it describes cannot drift apart.
package schematest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Base is the URI prefix of every published schema's $id.
const Base = "https://raw.githubusercontent.com/Sharvil2704/rhobound-mcp/main/docs/mcp/schemas/"

// Draft is the JSON Schema dialect every published schema declares.
const Draft = "https://json-schema.org/draft/2020-12/schema"

// RootModule is the module path of the repository's root module (the CLI).
const RootModule = "github.com/Sharvil2704/rhobound-mcp"

// Set is the folder of schemas, loaded.
type Set struct {
	Dir   string
	raw   map[string][]byte // file name -> bytes
	mu    sync.Mutex
	cache map[string]*jsonschema.Resolved // file + pointer -> resolved
}

// RepoRoot is the repository's root folder: the first folder up from the working directory whose go.mod declares the
// root module. Tests run in their package's folder, inside the nested mcp module.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && moduleLine(b) == RootModule {
			return dir, nil
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", errors.New("schematest: no go.mod of " + RootModule + " above the working directory")
		}
		dir = up
	}
}

func moduleLine(gomod []byte) string {
	for _, l := range strings.Split(string(gomod), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			return strings.TrimSpace(m)
		}
	}
	return ""
}

// Load reads every *.json file of dir. Each must declare the dialect and the $id Base + its file name.
func Load(dir string) (*Set, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &Set{Dir: dir, raw: map[string][]byte{}, cache: map[string]*jsonschema.Resolved{}}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var head struct {
			Schema string `json:"$schema"`
			ID     string `json:"$id"`
		}
		if err := json.Unmarshal(b, &head); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if head.Schema != Draft || head.ID != Base+e.Name() {
			return nil, fmt.Errorf("%s: $schema %q, $id %q (want %q, %q)", e.Name(), head.Schema, head.ID, Draft, Base+e.Name())
		}
		s.raw[e.Name()] = b
	}
	if len(s.raw) == 0 {
		return nil, fmt.Errorf("schematest: no schema in %s", dir)
	}
	return s, nil
}

// LoadRepo loads docs/mcp/schemas of the repository the working directory is in.
func LoadRepo() (*Set, error) {
	root, err := RepoRoot()
	if err != nil {
		return nil, err
	}
	return Load(filepath.Join(root, "docs", "mcp", "schemas"))
}

// Files lists the schema files, sorted.
func (s *Set) Files() []string {
	out := make([]string, 0, len(s.raw))
	for f := range s.raw {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Raw returns a schema file's bytes.
func (s *Set) Raw(file string) []byte { return s.raw[file] }

func (s *Set) schema(file string) (*jsonschema.Schema, error) {
	b, ok := s.raw[file]
	if !ok {
		return nil, fmt.Errorf("schematest: no schema %s", file)
	}
	var sc jsonschema.Schema
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return &sc, nil
}

// loader resolves a reference to another file of the set by its $id.
func (s *Set) loader(u *url.URL) (*jsonschema.Schema, error) {
	v := *u
	v.Fragment = ""
	name, ok := strings.CutPrefix(v.String(), Base)
	if !ok {
		return nil, fmt.Errorf("schematest: %s is not a published schema", u)
	}
	return s.schema(name)
}

// Resolve prepares the schema at ptr of file for validation: ptr is "" for the file's root, or a JSON pointer such as
// "/$defs/input".
func (s *Set) Resolve(file, ptr string) (*jsonschema.Resolved, error) {
	key := file + "#" + ptr
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.cache[key]; ok {
		return r, nil
	}
	var root *jsonschema.Schema
	base := Base + file
	if ptr == "" {
		var err error
		if root, err = s.schema(file); err != nil {
			return nil, err
		}
	} else {
		if _, ok := s.raw[file]; !ok {
			return nil, fmt.Errorf("schematest: no schema %s", file)
		}
		// a schema of its own that refers to the definition, at a URI of no file, so that the file is loaded
		root = &jsonschema.Schema{Ref: Base + file + "#" + ptr}
		base = Base + "schematest/pointer.json"
	}
	r, err := root.Resolve(&jsonschema.ResolveOptions{BaseURI: base, Loader: s.loader, ValidateDefaults: true})
	if err != nil {
		return nil, fmt.Errorf("%s#%s: %w", file, ptr, err)
	}
	s.cache[key] = r
	return r, nil
}

// Validate checks one JSON document against the schema at ptr of file.
func (s *Set) Validate(file, ptr string, doc []byte) error {
	r, err := s.Resolve(file, ptr)
	if err != nil {
		return err
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return r.Validate(v)
}

// ValidateEach checks every element of a JSON array against the schema at ptr of file.
func (s *Set) ValidateEach(file, ptr string, doc []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(doc, &items); err != nil {
		return fmt.Errorf("not a JSON array: %w", err)
	}
	if len(items) == 0 {
		return errors.New("an empty array: nothing to validate")
	}
	for i, it := range items {
		if err := s.Validate(file, ptr, it); err != nil {
			return fmt.Errorf("[%d]: %w", i, err)
		}
	}
	return nil
}
