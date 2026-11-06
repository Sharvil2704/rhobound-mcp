// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// help1a2df09 is the sha256 of the help that rhobound printed at 1a2df09, before the command registry.
const help1a2df09 = "045af4cace8acecfb017272eba86402b4192701249166550ef2e6f0cd5fa09f6"

// The help lists every registered command that has a usage, the commands of 1a2df09 first and in their order and
// version last; with the other commands' lines taken out it is the help of 1a2df09, byte for byte. Every registered
// command runs.
func TestCommandRegistryAndHelp(t *testing.T) {
	out, _, code := exec(t, "help")
	if code != 0 || !strings.HasPrefix(out, usageHead) || !strings.HasSuffix(out, usageNotes) {
		t.Fatalf("help: exit %d:\n%s", code, out)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(out, usageHead), usageNotes)
	last := -1
	for _, name := range append(append([]string{}, firstCommands...), "version") {
		i := strings.Index(body, commands[name].usage)
		if commands[name].usage == "" || i <= last {
			t.Errorf("%s: usage %q at %d, after %d", name, commands[name].usage, i, last)
		}
		last = i
	}
	base := usageHead
	for name, c := range commands {
		if c.run == nil {
			t.Errorf("%s has no run function", name)
		}
		if c.usage != "" && (!strings.HasPrefix(c.usage, "  rhobound "+name) || !strings.HasSuffix(c.usage, "\n")) {
			t.Errorf("%s: usage %q should be its lines, each ending in a newline, starting with \"  rhobound %s\"", name, c.usage, name)
		}
		if c.usage != "" && !strings.Contains(body, c.usage) {
			t.Errorf("%s is not in the help", name)
		}
	}
	for _, name := range append(append([]string{}, firstCommands...), "version") {
		base += commands[name].usage
	}
	if s := sha256.Sum256([]byte(base + usageNotes)); hex.EncodeToString(s[:]) != help1a2df09 {
		t.Errorf("the help of 1a2df09's commands changed:\n%s", base+usageNotes)
	}
	for _, alias := range []string{"--help", "-h"} {
		if o, _, code := exec(t, alias); code != 0 || o != out {
			t.Errorf("%s: exit %d, a different text", alias, code)
		}
	}
	for _, alias := range []string{"--version", "-v"} {
		if o, _, code := exec(t, alias); code != 0 || o != "rhobound "+Version+"\n" {
			t.Errorf("%s: exit %d, %q", alias, code, o)
		}
	}
	if _, errw, code := exec(t); code != 2 || errw != out {
		t.Errorf("no command: exit %d, standard error is not the help", code)
	}
	if _, errw, code := exec(t, "frobnicate"); code != 2 || errw != "unknown command \"frobnicate\"\n\n"+out {
		t.Errorf("unknown command: exit %d, %q", code, errw)
	}
}

// version --json prints the build as canonical JSON and a newline: the name, the version, the Go release, GOARCH and,
// when the go command stamped it, the version control state. Plain version, and --json=false, print what they did.
func TestVersionJSON(t *testing.T) {
	keys := []string{"name", "version", "go", "goarch", "vcs"}
	for _, args := range [][]string{{"version", "--json"}, {"--version", "--json"}, {"version", "-json=true"}} {
		out, _, code := exec(t, args...)
		b := []byte(strings.TrimSuffix(out, "\n"))
		c, err := contract.CanonicalizeJSON(b)
		var v map[string]any
		if code != 0 || err != nil || !bytes.Equal(c, b) || !strings.HasSuffix(out, "\n") || json.Unmarshal(b, &v) != nil {
			t.Fatalf("%v: exit %d, %q", args, code, out)
		}
		if v["name"] != "rhobound" || v["version"] != Version || !strings.HasPrefix(v["go"].(string), "go") ||
			v["goarch"] != runtime.GOARCH {
			t.Errorf("%v: %v", args, v)
		}
		for k := range v {
			if !slices.Contains(keys, k) {
				t.Errorf("%v: key %q", args, k)
			}
		}
	}
	if out, _, code := exec(t, "version", "--json=false"); code != 0 || out != "rhobound "+Version+"\n" {
		t.Errorf("--json=false: exit %d, %q", code, out)
	}
	stamped := &debug.BuildInfo{GoVersion: "go1.25.0", Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"},
		{Key: "vcs.revision", Value: "0123456789abcdef0123456789abcdef01234567"}, {Key: "vcs.time", Value: "2026-10-27T00:00:00Z"},
		{Key: "vcs.modified", Value: "true"}}}
	b, err := contract.Canonical(versionOf(stamped))
	want := `{"go":"go1.25.0","goarch":"` + runtime.GOARCH + `","name":"rhobound","vcs":{"modified":true,` +
		`"revision":"0123456789abcdef0123456789abcdef01234567","time":"2026-10-27T00:00:00Z"},"version":"` + Version + `"}`
	if err != nil || string(b) != want {
		t.Errorf("stamped: %s %v", b, err)
	}
	for _, bi := range []*debug.BuildInfo{nil, {GoVersion: "go1.25.0"}} {
		if v := versionOf(bi); v.VCS != nil || v.Name != "rhobound" || v.Go == "" {
			t.Errorf("unstamped %v: %+v", bi, v)
		}
	}
}
