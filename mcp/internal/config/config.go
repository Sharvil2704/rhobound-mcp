// SPDX-License-Identifier: Apache-2.0

// Package config reads the server's configuration, rhobound-mcp.config/v1 (DEPLOY_AWS.md 14): environment, capture
// and policy names mapped to locations, the engine's path and sha256, and the state folder.
//
// Tools take names, never paths. A name is looked up as a whole key; it is never joined onto a folder, and a name
// with an empty, "." or ".." path segment is refused before the lookup, so no tool argument can reach a folder the
// configuration does not list.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/objstore"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server/schemas"
)

// Schema is the configuration's schema string.
const Schema = "rhobound-mcp.config/v1"

// Profiles: the default tool set, or the bench profile (ARCHITECTURE.md 5.3).
const (
	ProfileDefault = "default"
	ProfileBench   = "bench"
)

// DefaultFitTTLH is fit_ttl_h when the configuration does not set it: a week.
const DefaultFitTTLH = 168

var (
	// ErrBadName: the name is not a Name (ARCHITECTURE.md 5.4), or has an empty, "." or ".." path segment.
	ErrBadName = errors.New("config: not a valid name")
	// ErrUnknownName: the name is valid but the configuration does not list it.
	ErrUnknownName = errors.New("config: unknown name")
)

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)
	envRE  = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	hexRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Config is a loaded configuration. Every path in it is absolute and clean.
type Config struct {
	Schema   string            `json:"schema"`
	Engine   Engine            `json:"engine"`
	StateDir string            `json:"state_dir"`
	Envs     map[string]Env    `json:"envs,omitempty"`
	Captures map[string]string `json:"captures,omitempty"`
	Policies map[string]string `json:"policies,omitempty"`
	Profile  string            `json:"profile,omitempty"`
	// FitTTLH is how many hours fit and A/B handles live, and how old an environment's baseline fit may be, counted
	// from the end of its window, before it is past its expiry (fit.expired); 1 to 8760, 0 when not set
	// (DefaultFitTTLH then applies).
	FitTTLH int `json:"fit_ttl_h,omitempty"`
}

// Engine is the rhobound binary the server runs.
type Engine struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Env is one environment's trace store.
type Env struct {
	Store  string `json:"store"` // "dir" or "s3"
	Root   string `json:"root,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	// Bucket, Region and Endpoint are the s3 store's bucket, its Region (AWS_REGION when empty) and, for an
	// S3-compatible store or a test fake, a path-style endpoint URL (https, or http on a loopback address).
	Bucket         string  `json:"bucket,omitempty"`
	Region         string  `json:"region,omitempty"`
	Endpoint       string  `json:"endpoint,omitempty"`
	RetryS         float64 `json:"retry_s,omitempty"`
	BatchS         float64 `json:"batch_s,omitempty"`
	SDKDelayS      float64 `json:"sdk_delay_s,omitempty"`
	MaxListedBytes int64   `json:"max_listed_bytes,omitempty"`
}

// bucketRE is an S3 bucket name's shape; regionRE an AWS Region name's.
var (
	bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)
)

// Load reads and checks the configuration file at path. Relative locations in it are taken relative to the file's
// folder; "~/" at the start of one is the user's home folder.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return Parse(b, filepath.Dir(abs))
}

// Parse checks a configuration against config.v1.json and the rules the schema cannot state, and resolves its
// locations against base.
func Parse(b []byte, base string) (*Config, error) {
	if err := validate(b); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	err := dec.Decode(&c)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if c.Profile == "" {
		c.Profile = ProfileDefault
	}
	if !filepath.IsAbs(c.Engine.Path) {
		return nil, fmt.Errorf("config: engine.path must be absolute")
	}
	c.Engine.Path = filepath.Clean(c.Engine.Path)
	if !hexRE.MatchString(c.Engine.SHA256) {
		return nil, fmt.Errorf("config: engine.sha256 must be 64 lower-case hex digits")
	}
	if c.StateDir, err = resolve(c.StateDir, base); err != nil {
		return nil, fmt.Errorf("config: state_dir: %w", err)
	}
	for _, name := range sortedKeys(c.Envs) {
		e := c.Envs[name]
		if !envRE.MatchString(name) {
			return nil, fmt.Errorf("config: envs: %q is not an environment name", name)
		}
		switch e.Store {
		case "dir":
			if e.Root == "" || e.Prefix != "" || e.Bucket != "" || e.Region != "" || e.Endpoint != "" {
				return nil, fmt.Errorf("config: envs.%s: a dir store has a root and no prefix, bucket, region or endpoint", name)
			}
			if e.Root, err = resolve(e.Root, base); err != nil {
				return nil, fmt.Errorf("config: envs.%s.root: %w", name, err)
			}
		case "s3":
			if e.Prefix == "" || !strings.HasSuffix(e.Prefix, "/") || e.Root != "" {
				return nil, fmt.Errorf("config: envs.%s: an s3 store has a prefix ending in / and no root", name)
			}
			if !bucketRE.MatchString(e.Bucket) {
				return nil, fmt.Errorf("config: envs.%s: an s3 store names its bucket (3 to 63 characters of [a-z0-9.-])", name)
			}
			if e.Region != "" && !regionRE.MatchString(e.Region) {
				return nil, fmt.Errorf("config: envs.%s: region %q is not an AWS Region name", name, e.Region)
			}
			if e.Endpoint != "" && !objstore.LoopbackOrHTTPS(e.Endpoint) {
				return nil, fmt.Errorf("config: envs.%s: endpoint must be an https URL, or http on a loopback address, with no path or query", name)
			}
		}
		c.Envs[name] = e
	}
	if err := resolveAll("captures", c.Captures, base); err != nil {
		return nil, err
	}
	if err := resolveAll("policies", c.Policies, base); err != nil {
		return nil, err
	}
	return &c, nil
}

// Capture returns the folder configured for a capture name.
func (c *Config) Capture(name string) (string, error) { return lookup(c.Captures, name) }

// Policy returns the policy file configured for a policy name.
func (c *Config) Policy(name string) (string, error) { return lookup(c.Policies, name) }

// Names returns the configuration's environment and capture names, sorted.
func (c *Config) Names() (envs, captures []string) { return sortedKeys(c.Envs), sortedKeys(c.Captures) }

// Env returns an environment's store.
func (c *Config) Env(name string) (Env, error) {
	if !envRE.MatchString(name) {
		return Env{}, ErrBadName
	}
	e, ok := c.Envs[name]
	if !ok {
		return Env{}, ErrUnknownName
	}
	return e, nil
}

// ValidName reports whether s is a capture or policy name: a name the engine takes as a fit's --name (cmd/rhobound's
// checkName; this module cannot import it, and the tests of both pin the same answers). That is a Name (ARCHITECTURE.md
// 5.4) with no empty, "." or ".." path segment (so not a path from the root), in which ext: (the model's prefix of a
// peer) starts no word but the first, and that holds no arn: (an AWS resource name: arn: at the start or after a
// character that is not a letter or a digit, or anywhere before an AWS partition). A configuration whose captures or
// policies hold another name does not load.
func ValidName(s string) bool {
	if !nameRE.MatchString(s) {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for i := 0; i < len(s); i++ {
		switch {
		case i > 0 && strings.HasPrefix(s[i:], "ext:") && !alnum(s[i-1]) && s[i-1] != '_':
			return false
		case i+4 <= len(s) && strings.EqualFold(s[i:i+4], "arn:"):
			rest := s[i+4:]
			partition := len(rest) > 3 && strings.EqualFold(rest[:3], "aws") && (rest[3] == ':' || rest[3] == '-')
			if i == 0 || !alnum(s[i-1]) || partition {
				return false
			}
		}
	}
	return true
}

// alnum reports whether c is in [A-Za-z0-9].
func alnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func lookup(m map[string]string, name string) (string, error) {
	if !ValidName(name) {
		return "", ErrBadName
	}
	p, ok := m[name]
	if !ok {
		return "", ErrUnknownName
	}
	return p, nil
}

func resolveAll(what string, m map[string]string, base string) error {
	for _, name := range sortedKeys(m) {
		if !ValidName(name) {
			return fmt.Errorf("config: %s: %q is not a valid name", what, name)
		}
		p, err := resolve(m[name], base)
		if err != nil {
			return fmt.Errorf("config: %s.%s: %w", what, name, err)
		}
		m[name] = p
	}
	return nil
}

// resolve makes a configured location absolute and clean.
func resolve(p, base string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			// no HOME, or a relative one (an agent may start the server with a cleared environment): the user database
			u, uerr := user.Current()
			if uerr != nil || !filepath.IsAbs(u.HomeDir) {
				return "", fmt.Errorf("no home folder for ~")
			}
			home = u.HomeDir
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p), nil
}

func validate(b []byte) error {
	sch, err := schemas.Bundle("config.v1.json", "")
	if err != nil {
		return err
	}
	rs, err := schemas.Resolve(sch)
	if err != nil {
		return err
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := rs.Validate(v); err != nil {
		return fmt.Errorf("config: does not match %s: %w", Schema, err)
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
