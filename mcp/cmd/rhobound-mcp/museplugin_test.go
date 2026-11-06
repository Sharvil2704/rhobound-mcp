// SPDX-License-Identifier: Apache-2.0

package main

// The Muse Code plugin (integrations/muse-code) offline: its manifest against the plugin contract as the community
// measured it with `muse plugins validate` (no official page exists; github.com/hypery11/oh-my-musecode,
// research/experiments/plugin-contract.md, builds 1.0.x to 1.3, and DietrichGebert/ponytail#1299 on 1.4.13), its hook
// wrappers under a cleared environment, the pin of its model-visible text, the server text its skills quote, the
// commands they cite, and one scripted session run the way Muse runs the plugin and the settings entry.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/server"
)

func pluginRoot(t *testing.T) string { return filepath.Join(repoRoot(t), "integrations", "muse-code") }

var (
	museIDRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
	museEvents  = strings.Fields("SessionStart UserPromptSubmit PreToolUse PermissionRequest PostToolUse PreLLMCall PostLLMCall PreCompact PostCompact SubagentStart SubagentStop Stop SessionEnd Notification PostToolUseFailure StopFailure PostToolBatch")
	hookFields  = strings.Fields("id event command timeoutMs statusMessage async compatibilityName outputCapabilities")
	skillFields = strings.Fields("id path enabledDefault")
	topFields   = strings.Fields("schemaVersion name displayName version description compat capabilities")
	capFamilies = strings.Fields("skills commands hooks mcpServers reminders")
)

// manifest is the plugin's manifest, decoded loosely so the contract checks can see every key.
type manifest map[string]json.RawMessage

func readManifest(t *testing.T) (manifest, []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), ".muse-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m, raw
}

func keysOf(raw json.RawMessage) ([]string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks, nil
}

// packagePath checks a capability path as the validator does: relative, non-empty, no backslash and no ".." segment
// (the check is lexical), naming a regular file (no symlink) in the plugin root.
func packagePath(root, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("empty path")
	case filepath.IsAbs(p) || strings.HasPrefix(p, "/"):
		return fmt.Errorf("%q is absolute", p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("%q has a backslash", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%q has a .. segment", p)
		}
	}
	fi, err := os.Lstat(filepath.Join(root, p))
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file (%v)", p, fi.Mode())
	}
	return nil
}

// frontmatter is a SKILL.md's frontmatter keys and values; a duplicate key is an error.
func frontmatter(b []byte) (map[string]string, error) {
	s := string(b)
	if !strings.HasPrefix(s, "---\n") {
		return nil, fmt.Errorf("no frontmatter")
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return nil, fmt.Errorf("frontmatter not closed")
	}
	m := map[string]string{}
	for _, line := range strings.Split(s[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("line %q", line)
		}
		if _, dup := m[k]; dup {
			return nil, fmt.Errorf("duplicate key %q", k)
		}
		m[k] = strings.TrimSpace(v)
	}
	return m, nil
}

// TestMusePluginContract checks the bundle against the measured native plugin contract, plus this plugin's own rules
// (no MCP servers in the plugin, every hook with a timeout of at least a second, no matcher, no status message).
func TestMusePluginContract(t *testing.T) {
	root := pluginRoot(t)
	m, raw := readManifest(t)
	if len(raw) > 131072 {
		t.Errorf("the manifest is %d bytes", len(raw))
	}
	// no key twice in any object (the validator's reading of a duplicate is not measured)
	var noDup func(b []byte, off int, where string)
	noDup = func(b []byte, off int, where string) {
		ms, err := members(raw, off, b)
		if err != nil {
			t.Errorf("%s: %v", where, err)
			return
		}
		for _, mm := range ms {
			v := raw[mm.start:mm.end]
			switch v[0] {
			case '{':
				noDup(v, mm.start, where+"."+mm.key)
			case '[':
				var items []json.RawMessage
				json.Unmarshal(v, &items)
				at := mm.start
				for i, it := range items {
					if len(it) > 0 && it[0] == '{' {
						at += bytes.Index(raw[at:], it)
						noDup(it, at, fmt.Sprintf("%s.%s[%d]", where, mm.key, i))
						at += len(it)
					}
				}
			}
		}
	}
	trim := bytes.TrimSpace(raw)
	noDup(trim, bytes.Index(raw, trim), "manifest")
	// one host manifest per plugin root
	for _, d := range []string{".claude-plugin", ".codex-plugin", ".cursor-plugin"} {
		if _, err := os.Lstat(filepath.Join(root, d)); err == nil {
			t.Errorf("a rival manifest %s: Muse reports multiple-manifests", d)
		}
	}
	for k := range m {
		if !contains(topFields, k) {
			t.Errorf("top-level key %q: unsupported-field", k)
		}
	}
	if string(m["schemaVersion"]) != "1" {
		t.Errorf("schemaVersion %s", m["schemaVersion"])
	}
	var name, version, desc string
	for k, p := range map[string]*string{"name": &name, "version": &version, "description": &desc} {
		if err := json.Unmarshal(m[k], p); err != nil || *p == "" {
			t.Errorf("%s: a non-empty string is required (%v)", k, err)
		}
	}
	if !museIDRE.MatchString(name) {
		t.Errorf("the plugin id %q", name)
	}
	var display string
	if d, ok := m["displayName"]; ok && (json.Unmarshal(d, &display) != nil || display == "") {
		t.Errorf("displayName %s", d)
	}
	var compat struct{ Source, ManifestDir string }
	if json.Unmarshal(m["compat"], &compat) != nil || compat.ManifestDir != ".muse-plugin" || compat.Source != "native" {
		t.Errorf("compat %s", m["compat"])
	}
	var caps map[string][]json.RawMessage
	if err := json.Unmarshal(m["capabilities"], &caps); err != nil {
		t.Fatalf("capabilities: each family must be an array: %v", err)
	}
	for k := range caps {
		if !contains(capFamilies, k) {
			t.Errorf("capability family %q: unsupported-field", k)
		}
	}
	if len(caps["mcpServers"]) != 0 || len(caps["reminders"]) != 0 || len(caps["commands"]) != 0 {
		t.Error("this plugin carries no MCP server, reminder or command: the server is registered in Muse's settings")
	}

	ids := map[string]bool{}
	for _, s := range caps["skills"] {
		ks, _ := keysOf(s)
		for _, k := range ks {
			if !contains(skillFields, k) {
				t.Errorf("skill field %q", k)
			}
		}
		var sk struct {
			ID, Path       string
			EnabledDefault *bool
		}
		if err := json.Unmarshal(s, &sk); err != nil {
			t.Errorf("skill %s: %v", s, err)
			continue
		}
		if !museIDRE.MatchString(sk.ID) || ids[sk.ID] {
			t.Errorf("skill id %q (invalid or duplicate)", sk.ID)
		}
		ids[sk.ID] = true
		if err := packagePath(root, sk.Path); err != nil {
			t.Errorf("skill %s: %v", sk.ID, err)
			continue
		}
		if sk.Path != "skills/"+sk.ID+"/SKILL.md" {
			t.Errorf("skill %s: path %s", sk.ID, sk.Path)
		}
		b, _ := os.ReadFile(filepath.Join(root, sk.Path))
		fm, err := frontmatter(b)
		if err != nil || fm["name"] != sk.ID || fm["description"] == "" || len(fm) != 2 || len(fm["description"]) > 1024 {
			t.Errorf("skill %s: frontmatter %v %v", sk.ID, fm, err)
		}
		// plain YAML scalars: nothing a YAML reader would take for a mapping, a comment, a quote or another type
		for k, v := range fm {
			if strings.Contains(v, ": ") || strings.Contains(v, " #") || strings.ContainsAny(v[:1], "'\"`[{>|*&!%@,?-") {
				t.Errorf("skill %s: %s is not a plain YAML scalar: %q", sk.ID, k, v)
			}
		}
	}
	if len(ids) != 3 {
		t.Errorf("%d skills", len(ids))
	}

	sources := map[string]string{}
	events := map[string]bool{}
	for _, h := range caps["hooks"] {
		ks, _ := keysOf(h)
		for _, k := range ks {
			if !contains(hookFields, k) {
				t.Errorf("hook field %q: unsupported-field (matcher is rejected)", k)
			}
		}
		var hk struct {
			ID, Event, StatusMessage string
			Command                  []string
			TimeoutMs                json.RawMessage
			Async                    *bool
		}
		if err := json.Unmarshal(h, &hk); err != nil {
			t.Errorf("hook: %v", err)
			continue
		}
		if !museIDRE.MatchString(hk.ID) || ids[hk.ID] {
			t.Errorf("hook id %q", hk.ID)
		}
		ids[hk.ID] = true
		if !contains(museEvents, hk.Event) {
			t.Errorf("hook %s: event %q", hk.ID, hk.Event)
		}
		events[hk.Event] = true
		if ms, err := strconv.Atoi(string(hk.TimeoutMs)); err != nil || ms < 1000 || ms > 10000 {
			t.Errorf("hook %s: timeoutMs %s (Muse has no default: a hook without one can block forever)", hk.ID, hk.TimeoutMs)
		}
		if hk.Async != nil {
			t.Errorf("hook %s: async (an async Stop could not block, and its records are not measured)", hk.ID)
		}
		if hk.StatusMessage != "" {
			t.Errorf("hook %s: a status message would flash at every %s for every user", hk.ID, hk.Event)
		}
		if len(hk.Command) != 2 || hk.Command[0] != "/bin/sh" {
			t.Errorf("hook %s: command %v (want /bin/sh and a script, so the exec bit does not matter)", hk.ID, hk.Command)
			continue
		}
		for _, a := range hk.Command[1:] {
			if err := packagePath(root, a); err != nil {
				t.Errorf("hook %s: %v", hk.ID, err)
			}
			if other, dup := sources[a]; dup {
				t.Errorf("hook %s reuses %s of %s: duplicate-hook-source", hk.ID, a, other)
			}
			sources[a] = hk.ID
		}
	}
	if !events["SessionStart"] || !events["Stop"] || len(events) != 2 {
		t.Errorf("events %v", events)
	}

	// no symlink anywhere in the package (fatal to Muse), only regular files and folders
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Error(err)
			return nil
		}
		if fi, err := os.Lstat(p); err != nil || !(fi.Mode().IsRegular() || fi.IsDir()) {
			t.Errorf("%s: %v", p, fi.Mode())
		}
		return nil
	})
	// nothing in the bundle names this machine (its home folder, the repository's place, a scratch folder): it is
	// published as it is
	locals := []string{"/tmp/claude-", repoRoot(t) + "/"}
	if h, err := os.UserHomeDir(); err == nil && len(h) > 1 {
		locals = append(locals, h+"/")
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			for _, local := range locals {
				if bytes.Contains(b, []byte(local)) {
					t.Errorf("%s names a local path (%s)", p, local)
				}
			}
		}
		return nil
	})
	lic, _ := os.ReadFile(filepath.Join(root, "LICENSE"))
	want, _ := os.ReadFile(filepath.Join(repoRoot(t), "LICENSE"))
	if len(want) == 0 || !bytes.Equal(lic, want) {
		t.Error("LICENSE is not a byte copy of the repository's")
	}

	// the marketplace names this plugin, at its version, by a local path inside the repository
	mraw, err := os.ReadFile(filepath.Join(repoRoot(t), ".agents", "plugins", "marketplace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mk struct {
		SchemaVersion int `json:"schemaVersion"`
		Plugins       []struct {
			Name, Version string
			Source        struct{ Source, Path string }
		}
	}
	if err := json.Unmarshal(mraw, &mk); err != nil || mk.SchemaVersion != 1 || len(mk.Plugins) != 1 {
		t.Fatalf("marketplace: %v %+v", err, mk)
	}
	p := mk.Plugins[0]
	if p.Name != name || p.Version != version || p.Source.Source != "local" || strings.Contains(p.Source.Path, "..") ||
		filepath.Join(repoRoot(t), p.Source.Path) != root {
		t.Errorf("marketplace entry %+v", p)
	}
}

// wrapperCase is one behaviour of a stub rhobound-mcp, and what each wrapper must make of it.
type wrapperCase struct {
	name, stub     string
	startCode      int
	stopCode       int
	stopStderr     string
	ranWithPayload bool
}

// TestMuseHookWrappers runs both wrappers under `env -i` (dash as /bin/sh, and bash --posix) against stub binaries:
// only the stub's 10 becomes an exit 2 (on Stop, with its stderr), everything else exits 0 with no output, a
// SessionStart never prints, a binary named only by a relative PATH entry or in the workspace never runs, and the
// wrappers never exec.
func TestMuseHookWrappers(t *testing.T) {
	root := pluginRoot(t)
	scripts := map[string]string{"session-start": filepath.Join(root, "hooks", "session-start.sh"),
		"stop": filepath.Join(root, "hooks", "stop.sh")}
	block := regexp.MustCompile(`(?s)# --- find rhobound-mcp.*?# --- end ---`)
	var blocks []string
	for name, p := range scripts {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(b, []byte("#!/bin/sh\n")) {
			t.Errorf("%s: not a /bin/sh script", name)
		}
		blocks = append(blocks, block.FindString(string(b)))
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "#") {
				continue
			}
			if regexp.MustCompile(`(^|[;&|(]\s*)exec(\s|$)`).MatchString(line) {
				t.Errorf("%s execs: %q (an older binary's exit 2 would block)", name, line)
			}
		}
		for _, sh := range [][]string{{"/bin/sh", "-n"}, {"bash", "--posix", "-n"}} {
			if out, err := exec.Command(sh[0], append(sh[1:], p)...).CombinedOutput(); err != nil {
				t.Errorf("%s: %v: %v %s", name, sh, err, out)
			}
		}
	}
	if len(blocks) != 2 || blocks[0] == "" || blocks[0] != blocks[1] {
		t.Error("the find-rhobound-mcp block differs between the wrappers")
	}

	shells := [][]string{{"/bin/sh"}}
	if _, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, []string{"/usr/bin/env", "bash", "--posix"})
	}
	cases := []wrapperCase{
		{name: "no binary", stub: ""},
		{name: "old binary", stub: "echo 'usage: rhobound-mcp MODE' >&2\nexit 2", ranWithPayload: true},
		{name: "remind", stub: "cat > \"$LOG.in\"\necho \"$@\" > \"$LOG\"\necho REASON >&2\nexit 10", stopCode: 2, stopStderr: "REASON\n", ranWithPayload: true},
		{name: "10, no reason", stub: "cat > \"$LOG.in\"\necho \"$@\" > \"$LOG\"\nexit 10", ranWithPayload: true},
		{name: "prints", stub: "cat > \"$LOG.in\"\necho \"$@\" > \"$LOG\"\necho CONTEXT\nexit 0", ranWithPayload: true},
		{name: "fails", stub: "cat > \"$LOG.in\"\necho \"$@\" > \"$LOG\"\necho boom >&2\nexit 1", ranWithPayload: true},
		{name: "killed", stub: "cat > \"$LOG.in\"\necho \"$@\" > \"$LOG\"\nkill -9 $$", ranWithPayload: true},
	}
	payload := bytes.Repeat([]byte(`{"session_id":"s-1","stop_hook_active":false}`+"\n"), 1<<14) // about 750 KB
	for _, sh := range shells {
		for _, c := range cases {
			for event, script := range scripts {
				t.Run(strings.Join(sh, " ")+"/"+c.name+"/"+event, func(t *testing.T) {
					dir := t.TempDir()
					log := filepath.Join(dir, "ran")
					bin := filepath.Join(dir, "bin")
					os.MkdirAll(bin, 0o700)
					if c.stub != "" {
						write(t, filepath.Join(bin, "rhobound-mcp"), "#!/bin/sh\nLOG="+log+"\n"+c.stub+"\n", 0o755)
					}
					cmd := exec.Command(sh[0], append(sh[1:], script)...)
					cmd.Dir = t.TempDir()                                // the workspace: the binary is outside it (one under it is never run)
					cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"} // env -i, then a PATH
					cmd.Stdin = bytes.NewReader(payload)
					var out, errb bytes.Buffer
					cmd.Stdout, cmd.Stderr = &out, &errb
					code := 0
					if err := cmd.Run(); err != nil {
						ee, ok := err.(*exec.ExitError)
						if !ok {
							t.Fatal(err)
						}
						code = ee.ExitCode()
					}
					wantCode, wantErr := 0, ""
					if event == "stop" {
						wantCode, wantErr = c.stopCode, c.stopStderr
					}
					if code != wantCode || out.Len() != 0 || errb.String() != wantErr {
						t.Errorf("exit %d stdout %q stderr %q; want exit %d, no stdout, stderr %q", code, out.String(),
							errb.String(), wantCode, wantErr)
					}
					if c.ranWithPayload && c.stub != "" && strings.Contains(c.stub, "$LOG") {
						args, _ := os.ReadFile(log)
						in, _ := os.ReadFile(log + ".in")
						if strings.TrimSpace(string(args)) != "muse-hook "+event || !bytes.Equal(in, payload) {
							t.Errorf("the binary ran with %q and %d bytes of stdin", args, len(in))
						}
					}
				})
			}
		}
	}

	t.Run("relative and workspace binaries", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "ran")
		stub := "#!/bin/sh\necho ran >> " + log + "\nexit 10\n"
		write(t, filepath.Join(dir, "rhobound-mcp"), stub, 0o755)
		write(t, filepath.Join(dir, "rel", "rhobound-mcp"), stub, 0o755)
		for _, tool := range []string{"id", "getent", "cut"} { // the no-HOME lookup must not run the workspace's
			write(t, filepath.Join(dir, tool), stub, 0o755)
			write(t, filepath.Join(dir, "rel", tool), stub, 0o755)
		}
		for event, script := range scripts {
			cmd := exec.Command("/bin/sh", script)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=.:rel::/nonexistent"} // and no HOME
			cmd.Stdin = strings.NewReader("{}")
			if err := cmd.Run(); err != nil {
				t.Errorf("%s: %v", event, err)
			}
		}
		if b, err := os.ReadFile(log); err == nil {
			t.Errorf("a relative or workspace rhobound-mcp ran: %s", b)
		}
	})
	t.Run("spellings", func(t *testing.T) {
		// a workspace folder named in PATH another way (through a link, '//', '..') is still the workspace
		d := t.TempDir()
		real := filepath.Join(d, "real", "ws")
		log := filepath.Join(d, "ran")
		write(t, filepath.Join(real, "bin", "rhobound-mcp"), "#!/bin/sh\necho ran >> "+log+"\n", 0o755)
		alias := filepath.Join(d, "alias")
		if err := os.Symlink(filepath.Join(d, "real"), alias); err != nil {
			t.Fatal(err)
		}
		for _, sh := range shells {
			for _, c := range [][2]string{
				{filepath.Join(alias, "ws"), filepath.Join(real, "bin")},
				{real, filepath.Join(alias, "ws", "bin")},
				{real, "/" + real + "/bin"},
				{real, filepath.Join(d, "real") + "/../real/ws/bin"},
				{real, filepath.Join(real, "bin")},
			} {
				cmd := exec.Command(sh[0], append(sh[1:], scripts["stop"])...)
				cmd.Dir, cmd.Env, cmd.Stdin = c[0], []string{"PATH=" + c[1], "PWD=" + c[0]}, strings.NewReader("{}")
				cmd.Run()
			}
		}
		if b, err := os.ReadFile(log); err == nil {
			t.Errorf("a workspace binary ran through another spelling of its folder: %s", b)
		}
	})
	t.Run("home", func(t *testing.T) {
		// HOME is not trusted over the user database, and never when it is the current folder (a host that runs the
		// hook with HOME set to the workspace)
		stub := func(dir, log string) {
			write(t, filepath.Join(dir, ".local", "bin", "rhobound-mcp"), "#!/bin/sh\necho ran > "+log+"\n", 0o755)
		}
		run := func(dir string, env []string) {
			cmd := exec.Command("/bin/sh", scripts["stop"])
			cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader("{}")
			if err := cmd.Run(); err != nil {
				t.Fatal(err)
			}
		}
		ws := t.TempDir()
		log := filepath.Join(ws, "ran")
		stub(ws, log)
		run(ws, []string{"HOME=" + ws, "PATH=/nonexistent"})
		if _, err := os.ReadFile(log); err == nil {
			t.Error("the workspace's .local/bin/rhobound-mcp ran with HOME set to the workspace")
		}
		if out, err := exec.Command("/bin/sh", "-c", `getent passwd "$(id -u)" | cut -d: -f6`).Output(); err == nil && strings.HasPrefix(string(out), "/") {
			other := t.TempDir()
			olog := filepath.Join(other, "ran")
			stub(other, olog)
			run(t.TempDir(), []string{"HOME=" + other, "PATH=/nonexistent"})
			if _, err := os.ReadFile(olog); err == nil {
				t.Error("HOME was taken over the user database's home folder")
			}
		}
	})
}

// museText is the plugin's model-visible text, in a fixed order: each skill (by path) and the Stop reason.
func museText(t *testing.T) []byte {
	t.Helper()
	root := pluginRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no skills: %v", err)
	}
	sort.Strings(paths)
	var b bytes.Buffer
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(&b, "file %s\n%d\n%s", filepath.ToSlash(rel), len(body), body)
	}
	fmt.Fprintf(&b, "stop-reason\n%s\n", museStopReason)
	return b.Bytes()
}

// TestMuseTextPinned: the skills and the Stop reason hash to mcp/MUSE-TEXT.sha256. A change to text a model reads
// needs a reviewed re-pin, as the server's own text needs DEFINITIONS.sha256.
func TestMuseTextPinned(t *testing.T) {
	s := sha256.Sum256(museText(t))
	got := hex.EncodeToString(s[:])
	b, err := os.ReadFile(filepath.Join("..", "..", "MUSE-TEXT.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	want, _, _ := strings.Cut(strings.TrimSpace(string(b)), " ")
	if got != want {
		t.Errorf("the Muse plugin's text hashes to %s; mcp/MUSE-TEXT.sha256 holds %s. Review the changed text, then "+
			"write the new hash to that file.", got, want)
	}
}

// TestMuseSkillQuotes: every block quote in a skill is, word for word, text the server itself serves (its
// instructions or a prompt, in Definitions), so a skill cannot drift from the pinned server text it quotes.
func TestMuseSkillQuotes(t *testing.T) {
	d, err := server.Definitions()
	if err != nil {
		t.Fatal(err)
	}
	var v any
	json.Unmarshal(d, &v)
	var texts []string
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case string:
			texts = append(texts, strings.Join(strings.Fields(y), " "))
		case []any:
			for _, e := range y {
				walk(e)
			}
		case map[string]any:
			for _, e := range y {
				walk(e)
			}
		}
	}
	walk(v)
	paths, _ := filepath.Glob(filepath.Join(pluginRoot(t), "skills", "*", "SKILL.md"))
	quotes := map[string]int{}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		var cur []string
		flush := func() {
			if len(cur) == 0 {
				return
			}
			q := strings.Join(strings.Fields(strings.Join(cur, " ")), " ")
			cur = nil
			quotes[filepath.Base(filepath.Dir(p))]++
			found := false
			for _, s := range texts {
				found = found || strings.Contains(s, q)
			}
			if !found {
				t.Errorf("%s quotes text the server does not serve:\n%s", p, q)
			}
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "> ") {
				cur = append(cur, strings.TrimPrefix(line, "> "))
			} else {
				flush()
			}
		}
		flush()
	}
	if quotes["rhobound-loop"] != 1 || quotes["rhobound-judge-review"] != 1 {
		t.Errorf("quotes %v: the loop and judge skills each quote the server once", quotes)
	}
}

// citedCommands are the rhobound and rhobound-mcp command lines the plugin's skills, README and GUIDE cite, in
// backticks or on a line of a fenced block, in document order (the GUIDE's chains run as written); the GUIDE's
// walkthrough block is left to TestMuseGuideWalkthrough.
func citedCommands(t *testing.T) []string {
	t.Helper()
	root := pluginRoot(t)
	paths, _ := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md"))
	sort.Strings(paths)
	paths = append(paths, filepath.Join(root, "README.md"), filepath.Join(root, "GUIDE.md"))
	seen := map[string]bool{}
	var cmds []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		f := strings.Fields(s)
		if len(f) > 1 && (f[0] == "rhobound" || f[0] == "rhobound-mcp") && !seen[s] { // a bare name is a mention
			seen[s] = true
			cmds = append(cmds, s)
		}
	}
	tick := regexp.MustCompile("`([^`\n]+)`")
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		fenced, walkthrough := false, false
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "<!-- walkthrough") {
				walkthrough = true
				continue
			}
			if strings.HasPrefix(line, "```") {
				if fenced && walkthrough {
					walkthrough = false
				}
				fenced = !fenced
				continue
			}
			if fenced {
				if !walkthrough {
					add(line)
				}
				continue
			}
			for _, m := range tick.FindAllStringSubmatch(line, -1) {
				add(m[1])
			}
		}
	}
	return cmds
}

// TestMuseCitedCommands runs every command the plugin cites, in document order, placeholders filled from the loop
// fixture, against the real binaries in a scratch home: none may be a usage error (an invented verb or flag).
func TestMuseCitedCommands(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	root := repoRoot(t)
	loop := filepath.Join(root, "testdata", "mcp", "loop")
	bin := t.TempDir()
	cli := build(t, goBin, root, "./cmd/rhobound", filepath.Join(bin, "rhobound"))
	build(t, goBin, ".", ".", filepath.Join(bin, "rhobound-mcp"))
	home := t.TempDir()
	write(t, filepath.Join(home, "changes.json"), `{"schema":"rhobound.change/v1","fit":"","items":[{"op":"own_ms",`+
		`"service":"b","value":{"lo":3.5,"hi":6},"label":"declared"}]}`, 0o600)
	h := func(p string) string { return filepath.Join(home, p) }
	fill := strings.NewReplacer("~/.local/bin/rhobound", cli, "POLICY.json", filepath.Join(loop, "policy.json"),
		"PREDICTION.json", h("preddir/report.json"), "ENGINE", cli, "NAME=FOLDER", "loop="+loop, "FOLDER", loop,
		"NAME", "loop", "POLICY", filepath.Join(loop, "policy.json"),
		`"$D/ANSWER.json"`, filepath.Join(loop, "judge-answer.json"), `"$D/PACKET.json"`, h("packet.json"),
		"ANSWER.json", filepath.Join(loop, "judge-answer.json"), "PACKET.json", h("packet.json"),
		"CHANGES.json", h("changes.json"), "FITDIR", h("fitdir"), "PREDDIR", h("preddir"), "GATEDIR", h("gatedir"),
		"OUTDIR", h("outdir"))
	cmds := citedCommands(t)
	if len(cmds) < 15 {
		t.Fatalf("only %d cited commands: %v", len(cmds), cmds)
	}
	for _, c := range cmds {
		f := strings.Fields(fill.Replace(c))
		var out bytes.Buffer
		var to string
		for i, a := range f {
			if a == ">" && i+1 < len(f) { // the command's standard output goes to a file
				to, f = f[i+1], f[:i]
				break
			}
		}
		cmd := exec.Command(filepath.Join(bin, f[0]), f[1:]...)
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader("")
		var errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		cmd.Run()
		if to != "" {
			write(t, to, out.String(), 0o600)
		}
		if strings.Contains(out.String()+errb.String(), "usage:") {
			t.Errorf("%s: a usage error:\n%s%s", c, out.String(), errb.String())
		}
		if c == "rhobound-mcp muse-hook api" && out.String() != "1\n" {
			t.Errorf("muse-hook api: %q", out.String())
		}
	}
	// the GUIDE's command-line judging chain ran for real, to the gate's verdict (unpinned workers: a block held at
	// inconclusive)
	if b, err := os.ReadFile(h("gatedir/verdict.txt")); err != nil || !contains([]string{"pass", "block", "inconclusive"}, strings.TrimSpace(string(b))) {
		t.Errorf("the GUIDE's chain did not reach the gate's verdict: %q %v", b, err)
	}
	t.Logf("%d commands: %v", len(cmds), cmds)
}

// TestMuseGuideWalkthrough runs GUIDE.md's walkthrough block as written, from the repository's root, line by line in
// one shell: every "# exit N" line must exit N, the echoed digest must be the one the canned answer cites, the gate's
// verdict must be block, and the repository must be left as it was.
func TestMuseGuideWalkthrough(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(pluginRoot(t), "GUIDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "<!-- walkthrough")
	if i < 0 {
		t.Fatal("GUIDE.md has no walkthrough block")
	}
	start := strings.Index(doc[i:], "```sh\n") + i + len("```sh\n")
	end := strings.Index(doc[start:], "```") + start
	exitRE := regexp.MustCompile(`\s+# exit (\d+)(:.*)?$`)
	var script strings.Builder
	script.WriteString("RCLOG=$1\n")
	var want []int
	for _, line := range strings.Split(strings.TrimRight(doc[start:end], "\n"), "\n") {
		if m := exitRE.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			want = append(want, n)
			fmt.Fprintf(&script, "%s\nprintf '%%s\\n' \"$?\" >> \"$RCLOG\"\n", line[:len(line)-len(m[0])])
			continue
		}
		script.WriteString(line + "\n")
	}
	if len(want) < 8 {
		t.Fatalf("only %d checked lines in the walkthrough", len(want))
	}
	bin := t.TempDir()
	build(t, goBin, root, "./cmd/rhobound", filepath.Join(bin, "rhobound"))
	build(t, goBin, ".", ".", filepath.Join(bin, "rhobound-mcp"))
	status := func() string {
		out, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
		return string(out)
	}
	before := status()
	scratch := t.TempDir()
	rclog := filepath.Join(scratch, "rc")
	cmd := exec.Command("/bin/sh", "-c", script.String(), "walkthrough", rclog)
	cmd.Dir = root
	cmd.Env = []string{"HOME=" + scratch, "TMPDIR=" + scratch, "PATH=" + bin + ":/usr/bin:/bin"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("the walkthrough: %v\n%s", err, errb.String())
	}
	var got []int
	for _, l := range strings.Fields(string(mustRead(t, rclog))) {
		n, _ := strconv.Atoi(l)
		got = append(got, n)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("exit statuses %v, the guide says %v\nstdout:\n%s\nstderr:\n%s", got, want, out.String(), errb.String())
	}
	var canned struct{ Packet string }
	json.Unmarshal(mustRead(t, filepath.Join(root, "testdata", "mcp", "loop", "judge-answer.json")), &canned)
	lines := strings.Split(out.String(), "\n")
	if !contains(lines, canned.Packet) || !contains(lines, "block") {
		t.Errorf("the echoed digest (%s) or the verdict block is missing:\n%s", canned.Packet, out.String())
	}
	if status() != before {
		t.Errorf("the walkthrough changed the repository:\n%s", status())
	}
}

// TestMuseSession is one scripted session, run the way Muse runs the plugin and the settings entry: init writes the
// entry; the server starts from that entry's command and args alone with an environment of MUSE_SESSION_ID only; the
// hooks run from the manifest's argv, relative to the plugin root, in the workspace, under a cleared environment with
// the plugin variables, fed the fixtures. A session that changes a file and never previews is reminded once; a
// session that previews is not.
func TestMuseSession(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("the go command is not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := repoRoot(t)
	loop := filepath.Join(root, "testdata", "mcp", "loop")
	bin := t.TempDir()
	cli := build(t, goBin, root, "./cmd/rhobound", filepath.Join(bin, "rhobound"))
	srv := build(t, goBin, ".", ".", filepath.Join(bin, "rhobound-mcp"))
	home := t.TempDir()
	path := bin + ":/usr/bin:/bin"

	// 1. init --write, as the setup skill runs it
	initCmd := exec.Command(srv, "init", "--engine", cli, "--capture", "loop="+loop, "--policy", "default="+filepath.Join(loop, "policy.json"), "--write")
	initCmd.Dir = home // not in the repository's work tree, whose paths init would refuse
	initCmd.Env = []string{"HOME=" + home, "PATH=" + path}
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	doc, err := os.ReadFile(filepath.Join(home, ".config", "muse", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := entryIn(t, doc)
	if entry.Mode != "optional" || !entry.Enabled || entry.Transport != "stdio" {
		t.Fatalf("the entry: %+v", entry)
	}

	// 2. the server, from the entry alone
	ctx := context.Background()
	sc := exec.Command(entry.Command, entry.Args...)
	sc.Env = []string{"MUSE_SESSION_ID=muse-s-1"}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "muse-sim", Version: "0"}, nil).Connect(ctx, &mcp.CommandTransport{Command: sc}, nil)
	if err != nil {
		t.Fatalf("the server did not start from the settings entry: %v", err)
	}
	defer cs.Close()
	tl, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tl.Tools {
		names = append(names, "mcp__rhobound__"+tool.Name)
	}
	sort.Strings(names)
	want := []string{"mcp__rhobound__budget", "mcp__rhobound__context", "mcp__rhobound__explain", "mcp__rhobound__fit_start",
		"mcp__rhobound__job_status", "mcp__rhobound__predict"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("tools %v", names)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %+v", name, err, res)
		}
		return res.StructuredContent.(map[string]any)
	}

	// 3. the hooks, as Muse runs them
	m, _ := readManifest(t)
	var caps struct {
		Hooks []struct {
			Event     string
			Command   []string
			TimeoutMs int
		}
	}
	json.Unmarshal(m["capabilities"], &caps)
	argv := map[string][]string{}
	timeout := map[string]time.Duration{}
	for _, h := range caps.Hooks {
		a := append([]string{}, h.Command...)
		for i := 1; i < len(a); i++ {
			if !filepath.IsAbs(a[i]) {
				a[i] = filepath.Join(pluginRoot(t), a[i])
			}
		}
		argv[h.Event], timeout[h.Event] = a, time.Duration(h.TimeoutMs)*time.Millisecond
	}
	ws := filepath.Join(home, "work", "shop")
	os.MkdirAll(ws, 0o700)
	gitIn(t, ws, "init", "-q")
	write(t, filepath.Join(ws, "handler.go"), "package shop\n", 0o600)
	write(t, filepath.Join(ws, ".rhobound", "muse.json"), optInOn+"\n", 0o600)
	gitIn(t, ws, "add", ".")
	data := filepath.Join(home, ".local", "share", "muse", "plugins", "rhobound", "data")
	b := &museBed{t: t, sessionID: "s-0001"}
	runHook := func(event, fixture, session string) (int, string, string, time.Duration) {
		t.Helper()
		b.sessionID = session
		hctx, cancel := context.WithTimeout(ctx, timeout[event])
		defer cancel()
		cmd := exec.CommandContext(hctx, argv[event][0], argv[event][1:]...)
		cmd.Dir = ws
		cmd.Env = []string{"HOME=" + home, "PATH=" + path, "MUSE_PLUGIN_ID=rhobound", "MUSE_PLUGIN_ROOT=" + pluginRoot(t),
			"MUSE_PLUGIN_DATA_DIR=" + data, "CLAUDE_PLUGIN_ROOT=" + pluginRoot(t), "PLUGIN_ROOT=" + pluginRoot(t)}
		cmd.Stdin = bytes.NewReader(b.fixture(fixture, ws))
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		t0 := time.Now()
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("%s: %v", event, err)
			}
			code = ee.ExitCode()
		}
		return code, out.String(), errb.String(), time.Since(t0)
	}
	edit := func(s string) {
		f, _ := os.OpenFile(filepath.Join(ws, "handler.go"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(s)
		f.Close()
	}

	// session A: changes a file, never previews: reminded once
	if c, out, e, d := runHook("SessionStart", "session-start.json", "muse-a"); c != 0 || out != "" || e != "" || d > 3*time.Second {
		t.Fatalf("SessionStart: exit %d stdout %q stderr %q in %v", c, out, e, d)
	}
	if c, _, _, _ := runHook("Stop", "stop.json", "muse-a"); c != 0 {
		t.Errorf("Stop before any change: %d", c)
	}
	edit("func Slow() {}\n")
	if c, out, e, _ := runHook("Stop", "stop.json", "muse-a"); c != 2 || out != "" || e != museStopReason+"\n" {
		t.Errorf("Stop after a change with no preview: exit %d stdout %q stderr %q", c, out, e)
	}
	if c, _, e, _ := runHook("Stop", "stop-active.json", "muse-a"); c != 0 || e != "" {
		t.Errorf("the continuation's Stop: %d %q", c, e)
	}
	if c, _, e, _ := runHook("Stop", "stop.json", "muse-a"); c != 0 || e != "" {
		t.Errorf("a later Stop: %d %q", c, e)
	}

	// session B: changes a file, previews it through the server: not reminded
	job := call("fit_start", map[string]any{"capture": "loop", "workers": map[string]any{"a": 1, "b": 2, "c": 1}})
	st := call("job_status", map[string]any{"job_id": job["job_id"], "wait_s": 20})
	for i := 0; i < 5 && st["state"] != "completed" && st["state"] != "failed"; i++ {
		st = call("job_status", map[string]any{"job_id": job["job_id"], "wait_s": 20})
	}
	if st["state"] != "completed" {
		t.Fatalf("the fit: %v", st)
	}
	if c, _, _, _ := runHook("SessionStart", "session-start.json", "muse-b"); c != 0 {
		t.Fatal(c)
	}
	edit("func Slower() {}\n")
	pv := call("predict", map[string]any{"fit_id": st["fit_id"],
		"changes": []any{map[string]any{"op": "own_ms", "service": "b", "value": 0.4}}})
	if pv["preview"].(map[string]any)["verdict"] == "" {
		t.Fatalf("predict: %v", pv)
	}
	if c, _, e, _ := runHook("Stop", "stop.json", "muse-b"); c != 0 || e != "" {
		t.Errorf("Stop after a preview: %d %q", c, e)
	}
}

// TestNoTrackedBinaries: the repository tracks no compiled program (a binary built inside a package folder and swept
// into a commit would ship an unreviewable server).
func TestNoTrackedBinaries(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Skip("not a git checkout")
	}
	magics := [][]byte{{0x7f, 'E', 'L', 'F'}, {0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}, {'M', 'Z'}}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" {
			continue
		}
		f, err := os.Open(filepath.Join(root, name))
		if err != nil {
			continue
		}
		head := make([]byte, 4)
		n, _ := f.Read(head)
		f.Close()
		for _, m := range magics {
			if n >= len(m) && bytes.Equal(head[:len(m)], m) && !strings.HasSuffix(name, ".txt") {
				t.Errorf("%s is a compiled program", name)
			}
		}
	}
}
