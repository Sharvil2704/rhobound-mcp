// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/abdiff"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/report"
)

// withRefusal adapts a command whose refusals carry codes: the error is printed as "rhobound: <error>" and the exit
// status is 1, as withError does, and with --json a coded refusal is also printed on standard output as
// {"error": {"code", "reason", "fix", ...}} (refusal.v1; docs/mcp/ARCHITECTURE.md 5.8). An error with no code (a usage
// or tool error) prints nothing on standard output.
func withRefusal(f func(args []string, stdout, stderr io.Writer) error) func(args []string, stdout, stderr io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		err := f(args, stdout, stderr)
		if err == nil {
			return 0
		}
		fmt.Fprintln(stderr, "rhobound:", err)
		if jsonRequested(args) {
			if _, werr := report.WriteRefusal(stdout, err); werr != nil {
				fmt.Fprintln(stderr, "rhobound:", werr)
			}
		}
		return 1
	}
}

// jsonRequested reports whether a command line asks for JSON: --json (or -json) anywhere, as the flag parser of
// parseInterleaved reads it, before a "--".
func jsonRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "json" {
			continue
		}
		if !hasVal {
			return true
		}
		b, err := strconv.ParseBool(val)
		return err == nil && b
	}
	return false
}

// stateDir is the CLI's own state folder: $RHOBOUND_STATE, else $XDG_STATE_HOME/rhobound, else
// ~/.local/state/rhobound. The record store (A/B records the CLI computed) is its ab/ folder.
func stateDir() (string, error) {
	if d := os.Getenv("RHOBOUND_STATE"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "rhobound"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no state folder: set RHOBOUND_STATE")
	}
	return filepath.Join(home, ".local", "state", "rhobound"), nil
}

// recordStore is the record store: the A/B records `diff-fit` computed, each in ab/<hex>.json named by the sha256 of
// its bytes. A record is found only when its file's bytes have the digest cited, so a file put there by hand under
// another record's name is not found.
type recordStore struct{ dir string }

var digestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)

// openRecords opens the record store; a store that does not exist holds no record.
func openRecords() recordStore {
	d, err := stateDir()
	if err != nil {
		return recordStore{}
	}
	return recordStore{dir: filepath.Join(d, "ab")}
}

// AB returns the A/B record with this digest.
func (s recordStore) AB(digest string) (*abdiff.Record, bool) {
	m := digestRE.FindStringSubmatch(digest)
	if s.dir == "" || m == nil {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(s.dir, m[1]+".json"))
	if err != nil || contract.Digest(b) != digest {
		return nil, false
	}
	r, err := abdiff.Unmarshal(b)
	if err != nil {
		return nil, false
	}
	return r, true
}

// mint writes a record into the store: the label measured-ab may cite it from now on. It returns the record's digest.
func (s recordStore) mint(b []byte) (string, error) {
	if s.dir == "" {
		return "", errors.New("no record store: set RHOBOUND_STATE")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return "", err
	}
	digest := contract.Digest(b)
	p := filepath.Join(s.dir, strings.TrimPrefix(digest, "sha256:")+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return digest, os.Rename(tmp, p)
}
