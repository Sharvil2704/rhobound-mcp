// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
)

// initBed is a home folder with an engine, a server binary, a capture and a policy.
type initBed struct {
	home, engine, server, capture, policy, cfg, settings string
	env                                                  map[string]string
}

func newInitBed(t *testing.T) *initBed {
	t.Helper()
	h := t.TempDir()
	b := &initBed{home: h, engine: filepath.Join(h, "bin", "rhobound"), server: filepath.Join(h, "bin", "rhobound-mcp"),
		capture: filepath.Join(h, "captures", "loop"), policy: filepath.Join(h, "policy.json"),
		cfg: filepath.Join(h, ".config", "rhobound-mcp", "config.json"), settings: filepath.Join(h, ".config", "muse", "settings.json"),
		env: map[string]string{"HOME": h}}
	write(t, b.engine, engine, 0o755)
	write(t, b.server, "#!/bin/sh\n", 0o755)
	write(t, b.policy, "{}", 0o600)
	if err := os.MkdirAll(b.capture, 0o700); err != nil {
		t.Fatal(err)
	}
	return b
}

// run runs init with args and the bed's server, and returns its report and error.
func (b *initBed) run(t *testing.T, args ...string) (initReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := initMain(append([]string{"--server", b.server}, args...), &out, func(k string) string { return b.env[k] }, b.server)
	var r initReport
	if out.Len() > 0 {
		if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
			t.Fatalf("the report is not JSON: %v\n%s", jerr, out.String())
		}
	}
	return r, err
}

func (b *initBed) full() []string {
	return []string{"--engine", b.engine, "--capture", "loop=" + b.capture, "--policy", "default=" + b.policy}
}

// tree is every file and folder under dir with its mode and bytes, for before/after comparisons.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, _ := os.Lstat(p)
		v := fi.Mode().String()
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			v += " " + string(b)
		}
		m[p] = v
		return nil
	})
	return m
}

// TestInitDryRunWrite: the dry run writes nothing and says what --write would do; --write writes a 0600
// configuration with absolute paths that the server loads and a settings entry of mode optional; a second run keeps
// both, byte for byte.
func TestInitDryRunWrite(t *testing.T) {
	b := newInitBed(t)
	before := tree(t, b.home)
	r, err := b.run(t, b.full()...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, tree(t, b.home)) {
		t.Error("the dry run wrote files")
	}
	want := newMuseEntry(b.server, b.cfg, true)
	if r.Write || r.Config.Path != b.cfg || r.Config.Action != "create" || r.Config.Check != "ok" ||
		r.Muse.Path != b.settings || r.Muse.Action != "add" || r.Muse.Key != "mcp_servers" || !reflect.DeepEqual(r.Muse.Entry, want) {
		t.Fatalf("the dry run's report: %+v", r)
	}

	r, err = b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Config.Action != "create" || r.Muse.Action != "add" || r.Muse.Backup != "" {
		t.Fatalf("--write: %v %+v", err, r)
	}
	fi, err := os.Stat(b.cfg)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the configuration: %v", err)
	}
	if di, _ := os.Stat(filepath.Dir(b.cfg)); di.Mode().Perm() != 0o700 {
		t.Errorf("the configuration's folder: %v", di.Mode())
	}
	raw, _ := os.ReadFile(b.cfg)
	if strings.Contains(string(raw), "~") {
		t.Errorf("the configuration names ~: %s", raw)
	}
	c, err := config.Load(b.cfg)
	if err != nil || c.Engine.Path != b.engine || c.StateDir != filepath.Join(b.home, ".local", "state", "rhobound-mcp") ||
		c.Captures["loop"] != b.capture || c.Policies["default"] != b.policy {
		t.Fatalf("the written configuration: %v %+v", err, c)
	}
	sum := sha256.Sum256([]byte(engine))
	if c.Engine.SHA256 != hex.EncodeToString(sum[:]) {
		t.Error("the engine's sha256")
	}
	doc, _ := os.ReadFile(b.settings)
	if e, keys := entryIn(t, doc); !reflect.DeepEqual(e, want) || len(keys) != 1 {
		t.Fatalf("the settings: %s", doc)
	}
	if cfg, code := museRegistration(doc, func(k string) string { return b.env[k] }); cfg != b.cfg || code != "" {
		t.Errorf("the hooks do not find the written entry: %q %q", cfg, code)
	}

	mid := tree(t, b.home)
	r, err = b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Config.Action != "keep" || r.Muse.Action != "keep" || r.Muse.Backup != "" || !reflect.DeepEqual(mid, tree(t, b.home)) {
		t.Errorf("a second --write: %v %+v", err, r)
	}
	// no flags: the existing configuration is checked and kept
	if r, err = b.run(t); err != nil || r.Config.Action != "keep" || r.Config.Check != "ok" || r.Muse.Action != "keep" {
		t.Errorf("init alone: %v %+v", err, r)
	}
}

// TestInitSettingsKept: an existing settings file keeps every other byte, and its previous form is kept as a backup.
func TestInitSettingsKept(t *testing.T) {
	b := newInitBed(t)
	old := "{\n  \"schema_version\": 1,\n  \"model\": \"muse-spark-1.3\",\n  \"mcpServers\": {\n    \"docs\": {\"transport\": \"stdio\", " +
		"\"command\": \"docs-mcp\", \"args\": [], \"startup_timeout_sec\": 30}\n  },\n  \"permissions\": {\"schema_version\": 1}\n}\n"
	write(t, b.settings, old, 0o640)
	r, err := b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Muse.Action != "add" || r.Muse.Key != "mcpServers" || !strings.HasPrefix(r.Muse.Backup, b.settings+".rhobound-bak-") {
		t.Fatalf("%v %+v", err, r)
	}
	if fi, err := os.Stat(r.Muse.Backup); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the backup's mode: %v", err)
	}
	if bak, _ := os.ReadFile(r.Muse.Backup); string(bak) != old {
		t.Error("the backup is not the previous file")
	}
	doc, _ := os.ReadFile(b.settings)
	if p, s := common([]byte(old), doc); p+s != len(old) {
		t.Errorf("other bytes changed:\n%s", doc)
	}
	if fi, _ := os.Stat(b.settings); fi.Mode().Perm() != 0o640 {
		t.Errorf("the settings file's mode: %v", fi.Mode())
	}
}

// TestInitRefuses: what init will not do, and that it then changes nothing.
func TestInitRefuses(t *testing.T) {
	b := newInitBed(t)
	// a configuration whose engine sha256 is the placeholder: the server's start check fails, nothing is registered
	zero := `{"schema":"rhobound-mcp.config/v1","engine":{"path":"` + b.engine + `","sha256":"` + strings.Repeat("0", 64) +
		`"},"state_dir":"state","captures":{"loop":"` + b.capture + `"},"policies":{"default":"` + b.policy + `"}}`
	write(t, b.cfg, zero, 0o600)
	before := tree(t, b.home)
	r, err := b.run(t, "--write")
	if err == nil || !strings.Contains(err.Error(), "server.engine_mismatch") || r.Config.Check != "server.engine_mismatch" ||
		r.Muse.Action != "skipped" {
		t.Errorf("a placeholder sha256: %v %+v", err, r)
	}
	// a configuration that differs: refused without --force, replaced with it
	if _, err := b.run(t, append(b.full(), "--write")...); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("a differing configuration without --force: %v", err)
	}
	if !reflect.DeepEqual(before, tree(t, b.home)) {
		t.Error("a refused init wrote files")
	}
	if r, err := b.run(t, append(b.full(), "--force", "--write")...); err != nil || r.Config.Action != "replace" || r.Muse.Action != "add" {
		t.Errorf("--force: %v %+v", err, r)
	}

	// settings Muse would disable or misread
	for name, doc := range map[string]string{
		"both spellings": `{"schema_version": 1, "mcp_servers": {}, "mcpServers": {}}`,
		"jsonc":          "{\n  // mine\n  \"schema_version\": 1\n}\n",
		"schema 2":       `{"schema_version": 2}`,
	} {
		b := newInitBed(t)
		write(t, b.settings, doc, 0o600)
		before := tree(t, b.home)
		r, err := b.run(t, append(b.full(), "--write")...)
		if err == nil || r.Muse.Action != "refused" || r.Muse.Detail == "" {
			t.Errorf("%s: %v %+v", name, err, r)
		}
		if !reflect.DeepEqual(before, tree(t, b.home)) {
			t.Errorf("%s: files changed", name)
		}
	}

	// flags: each on a fresh bed (no configuration to differ from), with the error it must give
	for name, c := range map[string]struct {
		args func(b *initBed) []string
		want string
	}{
		"capture without engine": {func(b *initBed) []string { return []string{"--capture", "loop=" + b.capture} }, "--capture goes with --engine"},
		"profile without engine": {func(b *initBed) []string { return []string{"--profile", "bench"} }, "--profile goes with --engine"},
		"force without engine":   {func(b *initBed) []string { return []string{"--force"} }, "--force goes with --engine"},
		"repin with engine":      {func(b *initBed) []string { return []string{"--repin", "--engine", b.engine} }, "does not go with --engine"},
		"bad capture":            {func(b *initBed) []string { return []string{"--engine", b.engine, "--capture", "loop"} }, "want NAME=PATH"},
		"bad name":               {func(b *initBed) []string { return []string{"--engine", b.engine, "--capture", "a b=" + b.capture} }, "want NAME=PATH"},
		"twice": {func(b *initBed) []string {
			return []string{"--engine", b.engine, "--capture", "l=" + b.capture, "--capture", "l=" + b.capture}
		}, "named twice"},
		"capture is a file":  {func(b *initBed) []string { return []string{"--engine", b.engine, "--capture", "loop=" + b.policy} }, "is not a folder"},
		"policy is a folder": {func(b *initBed) []string { return []string{"--engine", b.engine, "--policy", "default=" + b.capture} }, "is not a file"},
		"missing engine":     {func(b *initBed) []string { return []string{"--engine", filepath.Join(b.home, "nope")} }, "--engine:"},
		"relative server":    {func(b *initBed) []string { return []string{"--server", "rhobound-mcp"} }, "--server must be"},
		"other binary":       {func(b *initBed) []string { return []string{"--server", b.engine} }, "--server must be"},
		"missing server":     {func(b *initBed) []string { return []string{"--server", filepath.Join(b.home, "x", "rhobound-mcp")} }, "not an executable file"},
		"argument":           {func(b *initBed) []string { return []string{"extra"} }, "usage:"},
		"no configuration":   {func(b *initBed) []string { return nil }, "there is no configuration"},
	} {
		b := newInitBed(t)
		before := tree(t, b.home)
		if _, err := b.run(t, c.args(b)...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
		if !reflect.DeepEqual(before, tree(t, b.home)) {
			t.Errorf("%s: files changed", name)
		}
	}
}

// TestInitIncomplete: a configuration with no policy is registered disabled, with a note saying why.
func TestInitIncomplete(t *testing.T) {
	b := newInitBed(t)
	r, err := b.run(t, "--engine", "~/bin/rhobound", "--capture", "loop="+b.capture)
	if err != nil || r.Muse.Entry.Enabled || r.Muse.Entry.Mode != "optional" || !strings.Contains(strings.Join(r.Notes, " "), "enabled false") {
		t.Errorf("%v %+v", err, r)
	}
}

// TestStartCheckIsSetup: init and the hooks judge a configuration as the server does at start: startCheck fails exactly
// when setup fails.
func TestStartCheckIsSetup(t *testing.T) {
	b := newInitBed(t)
	sum := sha256.Sum256([]byte(engine))
	good := hex.EncodeToString(sum[:])
	cfg := func(path, sha string) string {
		return `{"schema":"rhobound-mcp.config/v1","engine":{"path":"` + path + `","sha256":"` + sha + `"},"state_dir":"state"}`
	}
	for name, body := range map[string]string{
		"good":            cfg(b.engine, good),
		"zero sha":        cfg(b.engine, strings.Repeat("0", 64)),
		"missing engine":  cfg(filepath.Join(b.home, "nope"), good),
		"relative engine": cfg("bin/rhobound", good),
		"not json":        `{"schema":`,
		"unknown key":     strings.Replace(cfg(b.engine, good), `"state_dir"`, `"x":1,"state_dir"`, 1),
	} {
		dir := t.TempDir()
		p := filepath.Join(dir, "config.json")
		write(t, p, body, 0o600)
		_, code, cerr := startCheck(p)
		_, serr := setup(p)
		if (cerr == nil) != (serr == nil) || (cerr != nil) != (code != "") {
			t.Errorf("%s: startCheck %q %v, setup %v", name, code, cerr, serr)
		}
	}
}

// TestMuseSettingsExample: examples/mcp/muse-settings.json is init's entry, with placeholder paths.
func TestMuseSettingsExample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "examples", "mcp", "muse-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, keys := entryIn(t, raw)
	w := newMuseEntry("/ABSOLUTE/PATH/TO/rhobound-mcp", "/ABSOLUTE/PATH/TO/rhobound-mcp/config.json", true)
	if !reflect.DeepEqual(e, w) || !reflect.DeepEqual(keys, []string{"mcp_servers"}) {
		t.Errorf("the example: %+v", e)
	}
	var have map[string]map[string]map[string]any
	json.Unmarshal(raw, &have)
	var got []string
	for k := range have["mcp_servers"]["rhobound"] {
		got = append(got, k)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"args", "command", "enabled", "mode", "transport"}) {
		t.Errorf("the example's keys: %v", got)
	}
}

// TestInitSettingsLink: a settings file that is a symbolic link is written through; the link stays a link.
func TestInitSettingsLink(t *testing.T) {
	b := newInitBed(t)
	target := filepath.Join(b.home, "dotfiles", "muse", "settings.json")
	write(t, target, `{"schema_version": 1}`, 0o600)
	os.MkdirAll(filepath.Dir(b.settings), 0o700)
	if err := os.Symlink("../../dotfiles/muse/settings.json", b.settings); err != nil {
		t.Fatal(err)
	}
	r, err := b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Muse.Path != target || !strings.HasPrefix(r.Muse.Backup, target+".rhobound-bak-") {
		t.Fatalf("%v %+v", err, r)
	}
	if fi, err := os.Lstat(b.settings); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was replaced")
	}
	doc, _ := os.ReadFile(b.settings)
	if e, _ := entryIn(t, doc); e.Mode != "optional" {
		t.Errorf("not written through: %s", doc)
	}
}

// TestInitRepin: --repin pins the engine's sha256 again and changes no other byte of the configuration (environments,
// fit_ttl_h and all), keeping the old file as a backup; --force keeps one too.
func TestInitRepin(t *testing.T) {
	b := newInitBed(t)
	old := "{\n  \"schema\": \"rhobound-mcp.config/v1\",\n  \"engine\": {\"path\": \"" + b.engine + "\", \"sha256\": \"" +
		strings.Repeat("0", 64) + "\"},\n  \"state_dir\": \"state\",\n  \"envs\": {\"prod\": {\"store\": \"dir\", \"root\": \"" +
		b.capture + "\"}},\n  \"policies\": {\"default\": \"" + b.policy + "\"},\n  \"fit_ttl_h\": 48\n}\n"
	write(t, b.cfg, old, 0o600)
	r, err := b.run(t, "--repin")
	if err != nil || r.Config.Action != "repin" || r.Config.Check != "ok" || !r.Muse.Entry.Enabled {
		t.Fatalf("dry run: %v %+v", err, r)
	}
	if got, _ := os.ReadFile(b.cfg); string(got) != old {
		t.Fatal("the dry run changed the configuration")
	}
	r, err = b.run(t, "--repin", "--write")
	if err != nil || r.Config.Action != "repin" || !strings.HasPrefix(r.Config.Backup, b.cfg+".rhobound-bak-") {
		t.Fatalf("--write: %v %+v", err, r)
	}
	got, _ := os.ReadFile(b.cfg)
	sum := sha256.Sum256([]byte(engine))
	if want := strings.Replace(old, strings.Repeat("0", 64), hex.EncodeToString(sum[:]), 1); string(got) != want {
		t.Errorf("repinned:\n%s", got)
	}
	if bak, _ := os.ReadFile(r.Config.Backup); string(bak) != old {
		t.Error("the backup is not the old configuration")
	}
	if r, err := b.run(t, "--repin"); err != nil || r.Config.Action != "keep" {
		t.Errorf("a second --repin: %v %+v", err, r)
	}
	// --force: the replaced configuration is backed up
	r, err = b.run(t, append(b.full(), "--force", "--write")...)
	if err != nil || r.Config.Action != "replace" || r.Config.Backup == "" {
		t.Fatalf("--force: %v %+v", err, r)
	}
	if bak, _ := os.ReadFile(r.Config.Backup); string(bak) != string(got) {
		t.Error("--force kept no copy of the replaced configuration")
	}
}

// TestInitParked: a "enabled": false the user set is kept, and said so; one init wrote for an incomplete
// configuration is turned on when the configuration is complete.
func TestInitParked(t *testing.T) {
	b := newInitBed(t)
	if _, err := b.run(t, append(b.full(), "--write")...); err != nil {
		t.Fatal(err)
	}
	doc, _ := os.ReadFile(b.settings)
	write(t, b.settings, strings.Replace(string(doc), `"enabled": true`, `"enabled": false, "startup_timeout_sec": 60`, 1), 0o600)
	r, err := b.run(t, "--write")
	if err != nil || !r.Muse.Parked || r.Muse.Entry.Enabled || r.Muse.Action != "keep" {
		t.Errorf("a parked entry: %v %+v", err, r)
	}
	doc, _ = os.ReadFile(b.settings)
	if !strings.Contains(string(doc), `"enabled": false, "startup_timeout_sec": 60`) {
		t.Errorf("the parked entry changed:\n%s", doc)
	}

	c := newInitBed(t)
	if r, err := c.run(t, "--engine", c.engine, "--capture", "loop="+c.capture, "--write"); err != nil || r.Muse.Entry.Enabled {
		t.Fatalf("incomplete: %v %+v", err, r)
	}
	r, err = c.run(t, append(c.full(), "--force", "--write")...)
	if e, _ := entryIn(t, mustRead(t, c.settings)); err != nil || r.Muse.Parked || !e.Enabled {
		t.Errorf("init's own false was not turned on: %v %+v", err, r)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestInitXDG: with XDG_CONFIG_HOME set, both defaults move there, and the registration record lets a hook whose
// environment lacks it find the settings init wrote.
func TestInitXDG(t *testing.T) {
	b := newInitBed(t)
	xdg := filepath.Join(b.home, "xdg")
	b.env["XDG_CONFIG_HOME"] = xdg
	r, err := b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Config.Path != filepath.Join(xdg, "rhobound-mcp", "config.json") ||
		r.Muse.Path != filepath.Join(xdg, "muse", "settings.json") ||
		r.Muse.Record != filepath.Join(b.home, ".local", "state", "rhobound-mcp", "muse-registration.json") {
		t.Fatalf("%v %+v", err, r)
	}
	noXDG := func(k string) string { return map[string]string{"HOME": b.home}[k] }
	if cfg, code := hookRegistration(noXDG, "", ""); code != "" || cfg != r.Config.Path {
		t.Errorf("the hooks find %q %q, not %q", cfg, code, r.Config.Path)
	}
	if fi, err := os.Stat(r.Muse.Record); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the record: %v", err)
	}
	// a relative XDG_CONFIG_HOME is not one: the default is ~/.config again (where there is no configuration)
	b.env["XDG_CONFIG_HOME"] = "rel"
	if _, err := b.run(t); err == nil || !strings.Contains(err.Error(), filepath.Join(b.home, ".config", "rhobound-mcp", "config.json")) {
		t.Errorf("a relative XDG_CONFIG_HOME: %v", err)
	}
}

// TestInitRelative: relative --config and --muse-settings are taken from the working folder and written absolute.
func TestInitRelative(t *testing.T) {
	b := newInitBed(t)
	t.Chdir(b.home)
	r, err := b.run(t, append(b.full(), "--config", "c/config.json", "--muse-settings", "m/settings.json", "--write")...)
	if err != nil || r.Config.Path != filepath.Join(b.home, "c", "config.json") ||
		r.Muse.Path != filepath.Join(b.home, "m", "settings.json") || r.Muse.Entry.Args[2] != r.Config.Path {
		t.Fatalf("%v %+v", err, r)
	}
	if cfg, code := museRegistration(mustRead(t, r.Muse.Path), func(string) string { return "" }); cfg != r.Config.Path || code != "" {
		t.Errorf("%q %q", cfg, code)
	}
}

// TestInitSettingsLinkedFolder: a relative link that climbs out of a linked folder is followed as the kernel follows
// it.
func TestInitSettingsLinkedFolder(t *testing.T) {
	b := newInitBed(t)
	real := filepath.Join(b.home, "real", "cfg")
	os.MkdirAll(filepath.Join(real, "muse"), 0o700)
	write(t, filepath.Join(b.home, "real", "dotfiles", "settings.json"), `{"schema_version": 1}`, 0o600)
	if err := os.Symlink(real, filepath.Join(b.home, ".config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../dotfiles/settings.json", filepath.Join(real, "muse", "settings.json")); err != nil {
		t.Fatal(err)
	}
	r, err := b.run(t, append(b.full(), "--write")...)
	if err != nil || r.Muse.Path != filepath.Join(b.home, "real", "dotfiles", "settings.json") {
		t.Fatalf("%v %+v", err, r)
	}
	if e, _ := entryIn(t, mustRead(t, b.settings)); e.Mode != "optional" {
		t.Error("the file Muse reads was not written")
	}
	if _, err := os.Stat(filepath.Join(b.home, "dotfiles")); !os.IsNotExist(err) {
		t.Error("a stray file was written")
	}
}

// TestInitWriteFails: a settings folder init cannot write: the report is printed, says where it stopped, and a rerun
// finishes.
func TestInitWriteFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	b := newInitBed(t)
	write(t, b.settings, `{"schema_version": 1}`, 0o600)
	dir := filepath.Dir(b.settings)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	r, err := b.run(t, append(b.full(), "--write")...)
	if err == nil || r.Muse.Action != "failed" || r.Muse.Detail == "" || r.Config.Action != "create" {
		t.Errorf("%v %+v", err, r)
	}
	os.Chmod(dir, 0o700)
	if r, err := b.run(t, append(b.full(), "--write")...); err != nil || r.Config.Action != "keep" || r.Muse.Action != "add" {
		t.Errorf("the rerun: %v %+v", err, r)
	}
}

// TestInitBackups: each change keeps the file it replaced under a new name; the original survives later changes.
func TestInitBackups(t *testing.T) {
	b := newInitBed(t)
	orig := `{"schema_version": 1, "model": "x"}`
	write(t, b.settings, orig, 0o600)
	r1, err := b.run(t, append(b.full(), "--write")...)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(b.home, "bin2", "rhobound-mcp")
	write(t, other, "#!/bin/sh\n", 0o755)
	r2, err := b.run(t, append(b.full(), "--server", other, "--write")...)
	if err != nil || r2.Muse.Action != "replace" || r2.Muse.Backup == "" || r2.Muse.Backup == r1.Muse.Backup {
		t.Fatalf("%v %+v", err, r2)
	}
	if string(mustRead(t, r1.Muse.Backup)) != orig {
		t.Error("the original was overwritten")
	}
}

// TestInitParkSurvives: a park survives later runs of every kind: --write twice, --repin --write twice.
func TestInitParkSurvives(t *testing.T) {
	b := newInitBed(t)
	if _, err := b.run(t, append(b.full(), "--write")...); err != nil {
		t.Fatal(err)
	}
	doc := string(mustRead(t, b.settings))
	write(t, b.settings, strings.Replace(doc, `"enabled": true`, `"enabled": false`, 1), 0o600)
	runs := [][]string{{"--write"}, {"--write"}, {"--repin", "--write"}, {"--repin", "--write"}}
	for i, args := range runs {
		r, err := b.run(t, args...)
		if e, _ := entryIn(t, mustRead(t, b.settings)); err != nil || e.Enabled || !r.Muse.Parked {
			t.Errorf("run %d %v: the park was lost: %v %+v", i+1, args, err, r)
		}
	}
	// a record that names another settings file says nothing about this one: the false stays the user's
	rec := filepath.Join(b.home, ".local", "state", "rhobound-mcp", "muse-registration.json")
	other, _ := json.Marshal(museRecord{Schema: museRecordSchema, Settings: filepath.Join(b.home, "other.json"),
		Entry: newMuseEntry(b.server, b.cfg, false)})
	write(t, rec, string(other), 0o600)
	if r, err := b.run(t, "--write"); err != nil || !r.Muse.Parked {
		t.Errorf("a foreign record: %v %+v", err, r)
	}
	// a record of another schema is not one
	bad, _ := json.Marshal(museRecord{Schema: "x/v0", Settings: b.settings, Entry: newMuseEntry(b.server, b.cfg, false)})
	write(t, rec, string(bad), 0o600)
	if r, err := b.run(t, "--write"); err != nil || !r.Muse.Parked {
		t.Errorf("a record of another schema: %v %+v", err, r)
	}
}

// TestInitLinks: links resolved as the kernel resolves them: "..", after a linked folder, is that folder's parent.
func TestInitLinks(t *testing.T) {
	b := newInitBed(t)
	os.MkdirAll(filepath.Join(b.home, "real", "a", "b"), 0o700)
	os.MkdirAll(filepath.Dir(b.settings), 0o700)
	if err := os.Symlink(filepath.Join(b.home, "real", "a", "b"), filepath.Join(b.home, "dots")); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(b.home, "real", "a", "settings.json") // dots/.. is real/a, not the home folder
	for name, target := range map[string]string{
		"absolute, existing": filepath.Join(b.home, "dots") + "/../settings.json",
		"absolute, dangling": filepath.Join(b.home, "dots") + "/../settings.json",
		"relative, existing": "../../dots/../settings.json",
	} {
		t.Run(name, func(t *testing.T) {
			os.Remove(kernel)
			if !strings.Contains(name, "dangling") {
				write(t, kernel, `{"schema_version": 1}`, 0o600)
			}
			os.Remove(b.settings)
			if err := os.Symlink(target, b.settings); err != nil {
				t.Fatal(err)
			}
			r, err := b.run(t, append(b.full(), "--write")...)
			if err != nil || r.Muse.Path != kernel {
				t.Fatalf("%v %+v", err, r)
			}
			if e, _ := entryIn(t, mustRead(t, b.settings)); e.Mode != "optional" {
				t.Error("the file read through the link was not written")
			}
			if _, err := os.Stat(filepath.Join(b.home, "settings.json")); !os.IsNotExist(err) {
				t.Error("a stray file at the lexical target")
			}
		})
	}
	// a linked configuration is written through its link, its mode capped at 0600
	c := newInitBed(t)
	if _, err := c.run(t, append(c.full(), "--write")...); err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(c.home, "dotfiles", "config.json")
	write(t, dot, strings.Replace(string(mustRead(t, c.cfg)), c.engine, c.engine, 1), 0o640)
	raw := mustRead(t, dot)
	sum := sha256.Sum256([]byte(engine))
	write(t, dot, strings.Replace(string(raw), hex.EncodeToString(sum[:]), strings.Repeat("0", 64), 1), 0o640)
	os.Chmod(dot, 0o640)
	os.Remove(c.cfg)
	if err := os.Symlink(dot, c.cfg); err != nil {
		t.Fatal(err)
	}
	if r, err := c.run(t, "--repin", "--write"); err != nil || r.Config.Action != "repin" {
		t.Fatalf("%v %+v", err, r)
	}
	if fi, _ := os.Lstat(c.cfg); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("--repin replaced the link")
	}
	if fi, _ := os.Stat(dot); fi.Mode().Perm() != 0o600 || !strings.Contains(string(mustRead(t, dot)), hex.EncodeToString(sum[:])) {
		t.Errorf("the linked configuration: %v", fi.Mode())
	}
}

// TestInitReportsRefusals: a refusal after the paths are known prints the report, saying what was refused.
func TestInitReportsRefusals(t *testing.T) {
	b := newInitBed(t)
	if r, err := b.run(t); err == nil || r.Config.Action != "missing" || r.Schema == "" {
		t.Errorf("no configuration: %v %+v", err, r)
	}
	if _, err := b.run(t, append(b.full(), "--write")...); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(b.home, "bin", "rhobound2")
	write(t, other, engine+"#\n", 0o755)
	if r, err := b.run(t, "--engine", other); err == nil || r.Config.Action != "refused" || !strings.Contains(r.Config.Detail, "--force") {
		t.Errorf("differs: %v %+v", err, r)
	}
	write(t, b.cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"`+filepath.Join(b.home, "gone")+`","sha256":"`+
		strings.Repeat("0", 64)+`"},"state_dir":"/s"}`, 0o600)
	if r, err := b.run(t, "--repin"); err == nil || r.Config.Action != "failed" || strings.Contains(r.Config.Detail, "muse settings") {
		t.Errorf("repin, engine gone: %v %+v", err, r)
	}
	write(t, b.cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"/x","path":"/y","sha256":"`+strings.Repeat("0", 64)+`"},"state_dir":"/s"}`, 0o600)
	if r, err := b.run(t, "--repin"); err == nil || !strings.Contains(r.Config.Detail, "engine: the key \"path\" appears twice") {
		t.Errorf("repin, duplicate key: %v %+v", err, r)
	}
}

// TestInitRedacts: the old entry's env and headers (credentials) never reach init's output.
func TestInitRedacts(t *testing.T) {
	b := newInitBed(t)
	write(t, b.settings, `{"schema_version": 1, "mcp_servers": {"github": {"transport": "streamable_http", "url": "https://x", `+
		`"headers": {"Authorization": "Bearer GH-SECRET"}}, "rhobound": {"transport": "streamable_http", "url": "https://y", `+
		`"headers": {"Authorization": "Bearer RB-SECRET"}, "env": {"AWS_SECRET_ACCESS_KEY": "AWS-SECRET"}}}}`, 0o600)
	for _, args := range [][]string{b.full(), append(b.full(), "--write"), {}} {
		var out bytes.Buffer
		initMain(append([]string{"--server", b.server}, args...), &out, func(k string) string { return b.env[k] }, b.server)
		for _, secret := range []string{"GH-SECRET", "RB-SECRET", "AWS-SECRET"} {
			if strings.Contains(out.String(), secret) {
				t.Errorf("%v printed %s", args, secret)
			}
		}
	}
}

// TestInitWorkspace: paths inside the git work tree of the current folder are refused unless
// --allow-workspace-paths; the report shows the engine and its pin, and --repin the pin it replaces.
func TestInitWorkspace(t *testing.T) {
	b := newInitBed(t)
	repo := filepath.Join(b.home, "repo")
	gitIn(t, filepath.Dir(repo), "init", "-q", "repo")
	t.Chdir(repo)
	evil := filepath.Join(repo, "tools", "rhobound")
	write(t, evil, engine, 0o755)
	write(t, filepath.Join(repo, "policy.json"), "{}", 0o600)
	cases := map[string][]string{
		"engine":        {"--engine", evil, "--capture", "loop=" + b.capture, "--policy", "default=" + b.policy},
		"policy":        {"--engine", b.engine, "--capture", "loop=" + b.capture, "--policy", "default=" + filepath.Join(repo, "policy.json")},
		"capture":       {"--engine", b.engine, "--capture", "loop=" + repo, "--policy", "default=" + b.policy},
		"configuration": append(b.full(), "--config", filepath.Join(repo, ".rhobound", "config.json")),
		"state folder":  append(b.full(), "--state-dir", filepath.Join(repo, "state")),
	}
	for name, args := range cases {
		before := tree(t, b.home)
		r, err := b.run(t, append(args, "--write")...)
		if err == nil || r.Config.Action != "refused" || !strings.Contains(r.Config.Detail, name) {
			t.Errorf("%s in the repository: %v %+v", name, err, r.Config)
		}
		if !reflect.DeepEqual(before, tree(t, b.home)) {
			t.Errorf("%s: files changed", name)
		}
	}
	r, err := b.run(t, append(cases["engine"], "--allow-workspace-paths")...)
	sum := sha256.Sum256([]byte(engine))
	if err != nil || r.Config.Engine.Path != evil || r.Config.Engine.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("--allow-workspace-paths: %v %+v", err, r.Config)
	}
	// from outside the repository the same paths are the user's own choice
	t.Chdir(b.home)
	if _, err := b.run(t, cases["engine"]...); err != nil {
		t.Errorf("outside the work tree: %v", err)
	}
	// the repin report: the old pin and the new one
	write(t, b.cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"`+b.engine+`","sha256":"`+strings.Repeat("0", 64)+
		`"},"state_dir":"/s","captures":{"loop":"`+b.capture+`"},"policies":{"default":"`+b.policy+`"}}`, 0o600)
	if r, err := b.run(t, "--repin"); err != nil || r.Config.Engine.Previous != strings.Repeat("0", 64) ||
		r.Config.Engine.SHA256 != hex.EncodeToString(sum[:]) || r.Config.Engine.Path != b.engine {
		t.Errorf("repin report: %v %+v", err, r.Config.Engine)
	}
}

// TestInitDevices: an engine or configuration that is a FIFO or a device is refused at once, not read.
func TestInitDevices(t *testing.T) {
	b := newInitBed(t)
	fifo := filepath.Join(b.home, "eng.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skip(err)
	}
	for name, args := range map[string][]string{
		"fifo engine": {"--engine", fifo},
		"zero engine": {"--engine", "/dev/zero"},
		"fifo config": {"--config", fifo},
	} {
		t0 := time.Now()
		if _, err := b.run(t, args...); err == nil || time.Since(t0) > 2*time.Second {
			t.Errorf("%s: %v after %v", name, err, time.Since(t0))
		}
	}
	// --repin with a configuration whose engine is a FIFO
	write(t, b.cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"`+fifo+`","sha256":"`+strings.Repeat("0", 64)+`"},"state_dir":"/s"}`, 0o600)
	t0 := time.Now()
	if _, err := b.run(t, "--repin"); err == nil || time.Since(t0) > 2*time.Second {
		t.Errorf("repin of a FIFO engine: %v after %v", err, time.Since(t0))
	}
}

// TestInitWorkspaceMore: the outermost work tree counts (a nested checkout's enclosing repository), and so does a
// Muse settings file inside it; a plain init that changes nothing says so.
func TestInitWorkspaceMore(t *testing.T) {
	b := newInitBed(t)
	outer := filepath.Join(b.home, "outer")
	gitIn(t, b.home, "init", "-q", "outer")
	inner := filepath.Join(outer, "vendor", "sub")
	os.MkdirAll(inner, 0o700)
	gitIn(t, inner, "init", "-q")
	evil := filepath.Join(outer, "bin", "rhobound")
	write(t, evil, engine, 0o755)
	t.Chdir(inner)
	if r, err := b.run(t, "--engine", evil, "--capture", "loop="+b.capture, "--policy", "default="+b.policy); err == nil || r.Config.Action != "refused" {
		t.Errorf("an engine in the enclosing repository: %v %+v", err, r.Config)
	}
	if r, err := b.run(t, append(b.full(), "--muse-settings", filepath.Join(outer, ".muse", "settings.json"))...); err == nil || !strings.Contains(r.Config.Detail, "Muse settings") {
		t.Errorf("Muse settings in the repository: %v %+v", err, r.Config)
	}
	t.Chdir(b.home)
	if _, err := b.run(t, append(b.full(), "--write")...); err != nil {
		t.Fatal(err)
	}
	r, err := b.run(t)
	if notes := strings.Join(r.Notes, " "); err != nil || strings.Contains(notes, "dry run") || !strings.Contains(notes, "nothing to write") {
		t.Errorf("a plain init with nothing to change: %v %v", err, r.Notes)
	}
}
