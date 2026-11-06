// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

func init() { register("init", runInit) }

// runInit checks a configuration the way the server checks it at start, writes one from flags, and registers the
// server with Muse Code: a dry run unless --write.
func runInit(_ context.Context, args []string) error {
	// a signal ends init at once (main asks to be told instead, for the servers)
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		self = ""
	}
	return initMain(args, os.Stdout, os.Getenv, self)
}

// multi is a repeatable NAME=VALUE flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// initReport is what init prints: one JSON object (rhobound-mcp.init/v1), also when it fails.
type initReport struct {
	Schema string `json:"schema"`
	Write  bool   `json:"write"`
	Config struct {
		Path   string `json:"path"`
		Action string `json:"action"`           // keep, create, replace, repin, or failed
		Check  string `json:"check"`            // ok, or the code of the start check that failed
		Detail string `json:"detail,omitempty"` // the failed check's message, or why the write failed
		Backup string `json:"backup,omitempty"` // the previous file, when it was replaced
		// What the configuration makes the server run and read, for the user to see before consenting to --write.
		Engine struct {
			Path     string `json:"path,omitempty"`
			SHA256   string `json:"sha256,omitempty"`
			Previous string `json:"previous_sha256,omitempty"` // the pin --repin replaces
		} `json:"engine"`
		StateDir string            `json:"state_dir,omitempty"`
		Captures map[string]string `json:"captures,omitempty"`
		Policies map[string]string `json:"policies,omitempty"`
		Envs     []string          `json:"envs,omitempty"`
	} `json:"config"`
	Muse struct {
		Path string `json:"path"`
		spliceResult
		Detail string    `json:"detail,omitempty"` // why it was refused, skipped or failed
		Backup string    `json:"backup,omitempty"` // the previous file, when it was changed
		Entry  museEntry `json:"entry"`            // the entry as written (enabled as kept, when parked)
		Record string    `json:"record,omitempty"` // the registration record the hooks read
	} `json:"muse_settings"`
	Notes []string `json:"notes"`
}

// initConfig is a configuration init writes: every path absolute.
type initConfig struct {
	Schema   string            `json:"schema"`
	Engine   config.Engine     `json:"engine"`
	StateDir string            `json:"state_dir"`
	Captures map[string]string `json:"captures,omitempty"`
	Policies map[string]string `json:"policies,omitempty"`
	Profile  string            `json:"profile"`
}

// startCheck runs the checks the server runs at start (setup), without creating the state folders: the configuration
// loads, and the engine's sha256 is the configured one. It returns the configuration, or a code and the error.
// The start check's limits: a configuration is a few KB; the engine binary a few MB (it is hashed whole).
const (
	configMax = 1 << 20
	engineMax = 256 << 20
)

func startCheck(path string) (*config.Config, string, error) {
	b, err := readBounded(path, configMax, true)
	if err != nil {
		return nil, "config.invalid", fmt.Errorf("config: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "config.invalid", fmt.Errorf("config: %w", err)
	}
	cfg, err := config.Parse(b, filepath.Dir(abs))
	if err != nil {
		return nil, "config.invalid", err
	}
	// a device (/dev/zero) or a FIFO would be hashed forever, a huge file for long: neither is an engine
	if fi, err := os.Stat(cfg.Engine.Path); err != nil || !fi.Mode().IsRegular() || fi.Size() > engineMax {
		if err == nil {
			err = fmt.Errorf("not a regular file of at most %d bytes", engineMax)
		}
		return cfg, "engine.unusable", fmt.Errorf("runner: engine %s: %w", cfg.Engine.Path, err)
	}
	if _, err := runner.NewExec(cfg.Engine.Path, cfg.Engine.SHA256, engineTimeout, nil); err != nil {
		var em *runner.EngineMismatchError
		if errors.As(err, &em) {
			return cfg, state.CodeEngineMismatch, err
		}
		return cfg, "engine.unusable", err
	}
	return cfg, "", nil
}

// complete reports whether a configuration names what a session needs: a policy, and a capture or an environment.
// init registers an incomplete one disabled, so that its tools do not answer every session with unknown names.
func complete(c *config.Config) bool {
	return len(c.Policies) > 0 && (len(c.Captures) > 0 || len(c.Envs) > 0)
}

func initMain(args []string, stdout io.Writer, getenv func(string) string, self string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "the configuration file (default $XDG_CONFIG_HOME/rhobound-mcp/config.json)")
	engine := fs.String("engine", "", "write a new configuration naming this rhobound binary")
	stateDir := fs.String("state-dir", "", "with --engine: the state folder (default ~/.local/state/rhobound-mcp)")
	profile := fs.String("profile", config.ProfileDefault, "with --engine: the tool profile, default or bench")
	var captures, policies multi
	fs.Var(&captures, "capture", "with --engine: a capture NAME=FOLDER (repeatable)")
	fs.Var(&policies, "policy", "with --engine: a policy NAME=FILE (repeatable)")
	settings := fs.String("muse-settings", "", "Muse's settings file (default $XDG_CONFIG_HOME/muse/settings.json)")
	server := fs.String("server", self, "the rhobound-mcp binary Muse runs (default: this one)")
	write := fs.Bool("write", false, "write the configuration and the settings (default: print what would be written)")
	force := fs.Bool("force", false, "with --engine: replace a configuration file that differs (it is backed up)")
	repin := fs.Bool("repin", false, "pin the existing configuration's engine sha256 again, after you rebuilt the engine")
	allowWS := fs.Bool("allow-workspace-paths", false, "allow an engine, server, configuration, state folder, capture "+
		"or policy inside the git work tree of the current folder")
	usage := fmt.Errorf("usage: rhobound-mcp init [--config FILE] [--engine FILE [--state-dir DIR] [--profile P] " +
		"[--capture NAME=FOLDER]... [--policy NAME=FILE]... [--force] | --repin] [--muse-settings FILE] [--server FILE] " +
		"[--allow-workspace-paths] [--write]")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *engine == "" {
		for _, f := range []string{"capture", "policy", "state-dir", "profile", "force"} {
			if set[f] {
				return fmt.Errorf("init: --%s goes with --engine", f)
			}
		}
	} else if *repin {
		return fmt.Errorf("init: --repin pins an existing configuration again; it does not go with --engine")
	}
	if !filepath.IsAbs(*server) || filepath.Base(*server) != "rhobound-mcp" {
		return fmt.Errorf("init: --server must be the absolute path of a binary named rhobound-mcp (have %q)", *server)
	}
	if fi, err := os.Stat(*server); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("init: --server %s is not an executable file", *server)
	}
	home, err := homeDir(getenv)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	ch, err := configHome(getenv)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}
	if *cfgPath == "" {
		*cfgPath = filepath.Join(ch, "rhobound-mcp", "config.json")
	}
	if *settings == "" {
		*settings = filepath.Join(ch, "muse", "settings.json")
	}
	if *cfgPath, err = filepath.Abs(*cfgPath); err != nil {
		return err
	}
	if *settings, err = filepath.Abs(*settings); err != nil {
		return err
	}
	r := initReport{Schema: "rhobound-mcp.init/v1", Write: *write, Notes: []string{}}
	r.Config.Path, r.Muse.Path, r.Config.Action = *cfgPath, *settings, "keep"
	// fail prints the report with the failure, so a refused or half-done run still says where it stopped.
	fail := func(err error) error {
		printReport(stdout, r)
		return err
	}
	cfgFail := func(action string, err error) error {
		r.Config.Action, r.Config.Detail = action, err.Error()
		return fail(fmt.Errorf("init: %w", err))
	}
	// A file that is a symbolic link (a dotfiles manager's) is written through, as the kernel resolves it: the rename
	// replaces its target, and the link stays.
	if *settings, err = throughLink(*settings); err != nil {
		r.Muse.Action, r.Muse.Detail = "failed", err.Error()
		return fail(fmt.Errorf("init: %w", err))
	}
	r.Muse.Path = *settings
	cfgFile, err := throughLink(*cfgPath) // where the configuration's bytes are (Muse and the server name *cfgPath)
	if err != nil {
		return cfgFail("failed", err)
	}
	recPath, err := museRecordPath(getenv)
	if err != nil {
		return fail(fmt.Errorf("init: %w", err))
	}
	have, err := readBounded(cfgFile, configMax, true)
	if errors.Is(err, os.ErrNotExist) {
		have, err = nil, nil
	}
	if err != nil {
		return cfgFail("failed", err)
	}
	var body []byte // the configuration to write, unless it is kept as it is
	switch {
	case *engine != "":
		if body, err = composeConfig(*engine, *stateDir, *profile, captures, policies, home); err != nil {
			return cfgFail("failed", err)
		}
		switch {
		case have == nil:
			r.Config.Action = "create"
		case bytes.Equal(have, body):
		case *force:
			r.Config.Action = "replace"
		default:
			return cfgFail("refused", fmt.Errorf("%s exists and differs from the configuration these flags make; run "+
				"without --engine to use it, --repin after an engine rebuild, or add --force to replace it (it is "+
				"backed up)", *cfgPath))
		}
	case have == nil:
		return cfgFail("missing", fmt.Errorf("there is no configuration at %s; name an engine with --engine to write one",
			*cfgPath))
	case *repin:
		if body, err = repinConfig(have); err != nil {
			return cfgFail("failed", fmt.Errorf("--repin: %w", err))
		}
		if !bytes.Equal(have, body) {
			r.Config.Action = "repin"
		}
	}

	// The check runs on what the file will hold: the new bytes in a scratch file (the configuration's own folder only
	// gives relative locations their base, and the start checks do not open them), or the file itself.
	checkPath := *cfgPath
	if r.Config.Action != "keep" {
		tmp, err := os.MkdirTemp("", "rhobound-mcp-init-")
		if err != nil {
			return cfgFail("failed", err)
		}
		defer os.RemoveAll(tmp)
		checkPath = filepath.Join(tmp, "config.json")
		if err := os.WriteFile(checkPath, body, 0o600); err != nil {
			return cfgFail("failed", err)
		}
	}
	cfg, code, cerr := startCheck(checkPath)
	r.Config.Check = "ok"
	if cerr != nil {
		r.Config.Check, r.Config.Detail = code, cerr.Error()
	}
	if cfg != nil {
		r.Config.Engine.Path, r.Config.Engine.SHA256 = cfg.Engine.Path, cfg.Engine.SHA256
		r.Config.StateDir, r.Config.Captures, r.Config.Policies = cfg.StateDir, cfg.Captures, cfg.Policies
		for name := range cfg.Envs {
			r.Config.Envs = append(r.Config.Envs, name)
		}
		sort.Strings(r.Config.Envs)
	}
	if r.Config.Action == "repin" {
		var old struct {
			Engine struct {
				SHA256 string `json:"sha256"`
			} `json:"engine"`
		}
		json.Unmarshal(have, &old)
		r.Config.Engine.Previous = old.Engine.SHA256
	}
	if code == state.CodeEngineMismatch {
		r.Notes = append(r.Notes, "the engine is not the binary whose sha256 was pinned. If you rebuilt it yourself "+
			"from a source you trust, run init --repin (a dry run), then init --repin --write; otherwise put the "+
			"configured engine back: an engine you did not rebuild may have been replaced")
	}
	// A repository's text may ask the agent to set rhobound up with its own files: an engine or server it ships would
	// then run as the user in every later session. Paths in the work tree of the current folder need a flag.
	if cerr == nil && !*allowWS {
		if ws := workTree(home); ws != "" {
			var in []string
			check := func(what, p string) {
				if p != "" && under(p, ws) {
					in = append(in, what+" "+p)
				}
			}
			check("server", *server)
			check("configuration", *cfgPath)
			check("Muse settings", *settings)
			check("engine", cfg.Engine.Path)
			check("state folder", cfg.StateDir)
			for _, k := range sortedNames(cfg.Captures) {
				check("capture "+k, cfg.Captures[k])
			}
			for _, k := range sortedNames(cfg.Policies) {
				check("policy "+k, cfg.Policies[k])
			}
			for _, k := range sortedNames(cfg.Envs) {
				check("environment "+k, cfg.Envs[k].Root)
			}
			if len(in) > 0 {
				return cfgFail("refused", fmt.Errorf("these are inside the git work tree of the current folder (%s): %s. "+
					"A repository must not choose what rhobound runs or reads for every session; if they are yours, "+
					"rerun with --allow-workspace-paths", ws, strings.Join(in, "; ")))
			}
		}
	}
	enabled := cerr == nil && complete(cfg)
	if cerr == nil && !enabled {
		r.Notes = append(r.Notes, "the configuration names no policy, or no capture or environment: the server is "+
			"registered with enabled false until it does (run init again then)")
	}
	if cerr == nil && kept(have, r.Config.Action) && hasTilde(have) {
		r.Notes = append(r.Notes, "the configuration names a location with ~, which the server expands from HOME or, "+
			"without HOME, the user database")
	}
	if x := getenv("XDG_CONFIG_HOME"); x != "" {
		r.Notes = append(r.Notes, "XDG_CONFIG_HOME is set: the plugin's hooks may not see it, so they find the settings "+
			"through the registration record ("+recPath+")")
	}
	r.Notes = append(r.Notes,
		"mode is optional: Muse's default, required, would abort every run whose server fails to start "+
			"(after an engine rebuild, say); keep it optional",
		"the server is registered for every repository: its six tool definitions and its instructions load in each "+
			"Muse session; set enabled to false in the entry to park it (init keeps a false you set)",
		"start a new Muse session to load the server; after muse plugins approve or update, run init again and "+
			"expect muse_settings.action keep")

	r.Muse.Entry = newMuseEntry(*server, *cfgPath, enabled)
	if cerr != nil {
		r.Muse.Action, r.Muse.Detail = "skipped", "the configuration does not pass the server's start checks"
		return fail(fmt.Errorf("init: %s: %v", code, cerr))
	}
	doc, err := readMuseSettings(*settings)
	if err != nil {
		r.Muse.Action, r.Muse.Detail = "failed", err.Error()
		return fail(fmt.Errorf("init: %w", err))
	}
	rec, hasRec := readMuseRecord(getenv)
	initOff := hasRec && rec.Settings == *settings && !rec.Entry.Enabled
	out, sr, serr := museSplice(doc, r.Muse.Entry, initOff)
	if serr != nil {
		r.Muse.Action, r.Muse.Detail = "refused", serr.Error()
		return fail(fmt.Errorf("init: %v", serr))
	}
	r.Muse.spliceResult = sr
	if sr.Parked {
		r.Muse.Entry.Enabled = false
		r.Notes = append(r.Notes, "the entry is parked (enabled false, set by hand): init keeps it; set it to true "+
			"in the settings to use the server")
	}
	if !*write {
		if r.Config.Action != "keep" || sr.Action != "keep" {
			r.Notes = append(r.Notes, "dry run: nothing was written; run again with --write to write it")
		} else {
			r.Notes = append(r.Notes, "nothing to write: the configuration and the settings entry are as they should be")
		}
		printReport(stdout, r)
		return nil
	}
	if r.Config.Action != "keep" {
		cmode := os.FileMode(0o600)
		if have != nil {
			if r.Config.Backup, err = backup(cfgFile, have); err != nil {
				r.Config.Action, r.Config.Detail = "failed", err.Error()
				return fail(fmt.Errorf("init: %w", err))
			}
			if fi, err := os.Stat(cfgFile); err == nil {
				cmode = fi.Mode().Perm() & 0o600 // the owner's bits it had, never more than 0600
			}
		}
		if err := writeAtomic(cfgFile, body, cmode, 0o700); err != nil {
			r.Config.Action, r.Config.Detail = "failed", err.Error()
			return fail(fmt.Errorf("init: %w", err))
		}
		if _, code, err := startCheck(*cfgPath); err != nil {
			r.Config.Action, r.Config.Detail = "failed", code+": "+err.Error()
			return fail(fmt.Errorf("init: the written configuration fails its check: %s: %v", code, err))
		}
	}
	if sr.Action != "keep" {
		if doc != nil {
			if r.Muse.Backup, err = backup(*settings, doc); err != nil {
				r.Muse.Action, r.Muse.Detail = "failed", err.Error()
				return fail(fmt.Errorf("init: %w", err))
			}
		}
		mode := os.FileMode(0o600)
		if fi, err := os.Stat(*settings); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := writeAtomic(*settings, out, mode, 0o700); err != nil {
			r.Muse.Action, r.Muse.Detail = "failed", err.Error()
			return fail(fmt.Errorf("init: %w", err))
		}
	}
	// The record holds init's own verdict (enabled as init chose it), not a false the user set and init kept: so a
	// park survives every later run, and only a false init chose is init's to turn on.
	own := newMuseEntry(*server, *cfgPath, enabled)
	rb, _ := json.MarshalIndent(museRecord{Schema: museRecordSchema, Settings: *settings, Entry: own}, "", "  ")
	if err := writeAtomic(recPath, append(rb, '\n'), 0o600, 0o700); err != nil {
		os.Remove(recPath) // a stale record would name an older settings file
		r.Notes = append(r.Notes, "the registration record was not written ("+err.Error()+"): the hooks look for the "+
			"settings at Muse's default place")
	} else {
		r.Muse.Record = recPath
	}
	printReport(stdout, r)
	return nil
}

// workTree is the outermost git work tree holding the current folder (a submodule's or a nested checkout's
// enclosing repository included), or "" when there is none; a work tree that holds the home folder (a dotfiles
// repository) is not a workspace, nor is anything above it.
func workTree(home string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	outer := ""
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			if under(home, dir) {
				break
			}
			outer = dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	return outer
}

// sortedNames is m's keys in order.
func sortedNames[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// kept reports whether init leaves the existing configuration's locations as they are.
func kept(have []byte, action string) bool {
	return have != nil && (action == "keep" || action == "repin")
}

// hasTilde reports whether a configuration names a location starting with ~.
func hasTilde(b []byte) bool {
	return bytes.Contains(b, []byte(`"~/`)) || bytes.Contains(b, []byte(`"~"`))
}

// repinConfig is the configuration b with engine.sha256 set to the sha256 of the engine it names, and every other byte
// unchanged.
func repinConfig(b []byte) ([]byte, error) {
	if !json.Valid(b) {
		return nil, fmt.Errorf("the configuration is not JSON")
	}
	trim := bytes.TrimLeft(b, " \t\r\n")
	off := len(b) - len(trim)
	top, err := members(b, off, bytes.TrimRight(trim, " \t\r\n"))
	if err != nil {
		return nil, err
	}
	for _, m := range top {
		if m.key != "engine" || b[m.start] != '{' {
			continue
		}
		em, err := members(b, m.start, b[m.start:m.end])
		if err != nil {
			return nil, fmt.Errorf("engine: %w", err)
		}
		var path string
		var sha *member
		for i := range em {
			switch em[i].key {
			case "path":
				json.Unmarshal(b[em[i].start:em[i].end], &path)
			case "sha256":
				sha = &em[i]
			}
		}
		if !filepath.IsAbs(path) || sha == nil {
			return nil, fmt.Errorf("the configuration's engine has no absolute path or no sha256")
		}
		sum, err := hashEngine(path)
		if err != nil {
			return nil, err
		}
		out := append(append(append([]byte{}, b[:sha.start]...), `"`+sum+`"`...), b[sha.end:]...)
		return out, nil
	}
	return nil, fmt.Errorf("the configuration has no engine object")
}

// hashEngine is the hex sha256 of an engine binary: a regular file of at most engineMax bytes, opened without
// blocking (a FIFO) and checked after the open (a device such as /dev/zero is refused).
func hashEngine(p string) (string, error) {
	f, err := openNonBlock(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Size() > engineMax {
		return "", fmt.Errorf("%s is not a regular file of at most %d bytes", p, engineMax)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, engineMax+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// backup writes the previous bytes of path beside it as path.rhobound-bak-<UTC time>, a new name each time, and
// returns that name.
func backup(path string, prev []byte) (string, error) {
	base := path + ".rhobound-bak-" + time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 100; i++ {
		p := base
		if i > 0 {
			p = fmt.Sprintf("%s-%d", base, i)
		}
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(prev)
		if cerr := f.Close(); werr != nil || cerr != nil {
			os.Remove(p)
			return "", errors.Join(werr, cerr)
		}
		return p, nil
	}
	return "", fmt.Errorf("%s: no free backup name", path)
}

// composeConfig is the configuration the flags name, as indented JSON: the engine's real sha256, and every path
// absolute (the server reads them literally; "~" would need HOME in its environment).
func composeConfig(engine, stateDir, profile string, captures, policies multi, home string) ([]byte, error) {
	abs := func(p string) (string, error) {
		if p == "~" || strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[1:])
		}
		return filepath.Abs(p)
	}
	c := initConfig{Schema: config.Schema, Profile: profile}
	var err error
	if c.Engine.Path, err = abs(engine); err != nil {
		return nil, err
	}
	if c.Engine.SHA256, err = hashEngine(c.Engine.Path); err != nil {
		return nil, fmt.Errorf("--engine: %w", err)
	}
	if stateDir == "" {
		stateDir = filepath.Join(home, ".local", "state", "rhobound-mcp")
	}
	if c.StateDir, err = abs(stateDir); err != nil {
		return nil, err
	}
	pairs := func(flag string, vs multi, dir bool) (map[string]string, error) {
		if len(vs) == 0 {
			return nil, nil
		}
		m := map[string]string{}
		for _, v := range vs {
			name, p, ok := strings.Cut(v, "=")
			if !ok || !config.ValidName(name) || p == "" {
				return nil, fmt.Errorf("--%s %q: want NAME=PATH with a valid name", flag, v)
			}
			if _, dup := m[name]; dup {
				return nil, fmt.Errorf("--%s: %q is named twice", flag, name)
			}
			if p, err = abs(p); err != nil {
				return nil, err
			}
			fi, err := os.Stat(p)
			if err != nil {
				return nil, fmt.Errorf("--%s %s: %w", flag, name, err)
			}
			if fi.IsDir() != dir {
				return nil, fmt.Errorf("--%s %s: %s is not a %s", flag, name, p, map[bool]string{true: "folder", false: "file"}[dir])
			}
			m[name] = p
		}
		return m, nil
	}
	if c.Captures, err = pairs("capture", captures, true); err != nil {
		return nil, err
	}
	if c.Policies, err = pairs("policy", policies, false); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// throughLink is path, or when path is a symbolic link, the file it names as the kernel resolves it ("..", after a
// linked folder, is that folder's parent): the existing target, or a dangling link's target, which init may create.
func throughLink(path string) (string, error) {
	for i := 0; i < 40; i++ {
		fi, err := os.Lstat(path)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		if real, err := filepath.EvalSymlinks(path); err == nil {
			return real, nil
		}
		// dangling: resolve the target's folder (it must exist) and keep the last name; no lexical cleaning first
		to, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(to) {
			d, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			to = d + string(filepath.Separator) + to
		}
		k := strings.LastIndexByte(to, filepath.Separator)
		base := to[k+1:]
		if base == "" || base == "." || base == ".." {
			return "", fmt.Errorf("%s: the link names a folder, not a file", path)
		}
		dir, err := filepath.EvalSymlinks(to[:k+1])
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		path = filepath.Join(dir, base)
	}
	return "", fmt.Errorf("%s: too many symbolic links", path)
}

// writeAtomic writes b to path through a temporary file in its folder (created with dirMode when missing) and a
// rename, so a reader sees the old file or the new one, never a part.
func writeAtomic(path string, b []byte, mode, dirMode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// printReport writes the report as indented JSON.
func printReport(w io.Writer, r initReport) {
	b, _ := json.MarshalIndent(r, "", "  ")
	fmt.Fprintf(w, "%s\n", b)
}
