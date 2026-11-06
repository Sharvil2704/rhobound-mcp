// SPDX-License-Identifier: Apache-2.0

package main

// The Muse Code plugin's hooks (integrations/muse-code): its SessionStart and Stop wrappers run
// `rhobound-mcp muse-hook session-start|stop` with the hook's JSON on standard input. The verb never writes to standard
// output (on SessionStart Muse would read it as context) and exits 0, or 10 when Stop reminds; the Stop wrapper turns
// 10 into Muse's blocking exit 2 and every other status into 0, so an old binary, a crash or a signal never blocks.
//
// The reminder is opt-in per repository (.rhobound/muse.json), armed only when the server is registered, enabled and
// passes its start checks, and fires at most once per session: when the session changed files and the server's
// preview log has no row since the session started.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

func init() { register("muse-hook", runMuseHook) }

// museHookAPI is the hook protocol this binary speaks; the plugin's setup skill reads it (muse-hook api) to tell a
// binary that is older than the plugin.
const museHookAPI = 1

// museStopReason is the one text the hooks can put before the model, on Stop: fixed, so no trace, repository or
// transcript data ever reaches it. It is pinned with the plugin's skills in mcp/MUSE-TEXT.sha256.
const museStopReason = "rhobound: files in this repository changed since this session started, and no rhobound " +
	"preview was recorded since then. If your change can move a service's mean latency, call the rhobound predict " +
	"tool once and report its verdict; otherwise say in one line why not. This reminder appears once per session. " +
	"Previews are advisory; CI decides."

// Hook limits: the payload read, the verb's own deadline (the manifest gives each hook 5 s), git's share of it, the
// files listed for the fingerprint, and how long session markers are kept.
const (
	hookMaxPayload  = 8 << 20
	hookBudget      = 3 * time.Second
	gitBudget       = 1500 * time.Millisecond
	gitMaxListing   = 32 << 20
	fpMaxFiles      = 1 << 20
	markerRetention = 7 * 24 * time.Hour
	optInMaxBytes   = 4096
	markerMaxBytes  = 64 << 10
	// sessionsName is the markers' folder in the data folder: a name of this plugin's own, so that a data folder that
	// is not the plugin's (a host's mistake) holds nothing the hooks would take for theirs
	sessionsName = "rhobound-sessions-v1"
)

// museHook is one hook run, with what it reads from the process made replaceable for tests.
type museHook struct {
	getenv func(string) string
	now    func() time.Time
	stdin  io.Reader
	stderr io.Writer
	cwd    string // the process's folder, when the payload names none
}

func runMuseHook(ctx context.Context, args []string) error {
	// A hook is ended by Muse at its timeout: the default action of SIGTERM and SIGINT (main asks to be told instead).
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	if len(args) == 1 && args[0] == "api" {
		fmt.Println(museHookAPI)
		return nil
	}
	// A backstop: whatever a read or a hash does, the process ends soon after its budget (Muse may signal only the
	// wrapper it started, which would leave this process behind).
	time.AfterFunc(hookBudget+500*time.Millisecond, func() { os.Exit(0) })
	cwd, _ := os.Getwd()
	h := museHook{getenv: os.Getenv, now: time.Now, stdin: os.Stdin, stderr: os.Stderr, cwd: cwd}
	event := ""
	if len(args) == 1 {
		event = args[0]
	}
	if code := h.run(ctx, event); code != 0 {
		return exitStatus(code)
	}
	return nil
}

// run runs one event and returns the exit status: 10 when Stop reminds, else 0. A panic is 0 too.
func (h *museHook) run(ctx context.Context, event string) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()
	switch event {
	case "session-start":
		h.sessionStart(ctx)
	case "stop":
		return h.stop(ctx)
	}
	return 0
}

// hookPayload is what the hooks read from Muse's JSON. stop_hook_active counts only as the JSON literal true.
type hookPayload struct {
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	StopHookActive json.RawMessage `json:"stop_hook_active"`
}

// payload decodes the first JSON value on stdin (at most hookMaxPayload bytes) and leaves the rest unread. A stdin
// that stays open without a whole value is given up at the deadline.
func (h *museHook) payload(ctx context.Context) (hookPayload, bool) {
	type res struct {
		p  hookPayload
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		var p hookPayload
		err := json.NewDecoder(io.LimitReader(h.stdin, hookMaxPayload)).Decode(&p)
		ch <- res{p, err == nil}
	}()
	select {
	case r := <-ch:
		return r.p, r.ok
	case <-ctx.Done():
		return hookPayload{}, false
	}
}

// sessionMarker is what SessionStart records for a session it arms.
type sessionMarker struct {
	StartedAt   time.Time `json:"started_at"`
	Root        string    `json:"root"`
	Fingerprint *string   `json:"fingerprint"`
	StateDir    string    `json:"state_dir"`
}

// markerRE matches the files the hooks make in the sessions folder, and the only ones they ever remove.
var markerRE = regexp.MustCompile(`^h-[0-9a-f]{64}\.(json|reminded)$`)

// markerName is a session's file name: the sha256 of its id, so that no id can name a path, collide with another's
// name or differ from it only in case.
func markerName(id string) string {
	s := sha256.Sum256([]byte(id))
	return "h-" + hex.EncodeToString(s[:])
}

// dataDir is the plugin's data folder: MUSE_PLUGIN_DATA_DIR when it is absolute (Muse names it but does not create
// it), else ~/.cache/rhobound-mcp/muse.
func dataDir(getenv func(string) string) (string, error) {
	if d := getenv("MUSE_PLUGIN_DATA_DIR"); filepath.IsAbs(d) {
		return filepath.Clean(d), nil
	}
	home, err := homeDir(getenv)
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "rhobound-mcp", "muse"), nil
}

// confined is the hook's environment less every location inside the repository: a host that points HOME,
// XDG_CONFIG_HOME, MUSE_PLUGIN_DATA_DIR or a PATH entry into the workspace must not let the repository supply the
// settings, the registration, the configuration, git, or the folder the hooks write and prune. Without HOME, homeDir
// asks the user database.
func (h *museHook) confined(root, ws string) func(string) string {
	return func(k string) string {
		v := h.getenv(k)
		switch k {
		case "HOME", "XDG_CONFIG_HOME", "MUSE_PLUGIN_DATA_DIR":
			if filepath.IsAbs(v) && (under(v, root) || under(v, ws)) {
				return ""
			}
		case "PATH":
			var keep []string
			for _, d := range filepath.SplitList(v) {
				if filepath.IsAbs(d) && !under(d, root) && !under(d, ws) {
					keep = append(keep, d)
				}
			}
			return strings.Join(keep, string(filepath.ListSeparator))
		}
		return v
	}
}

// privateDir reports whether p is a real folder (not a link) of the user's own that group and others cannot write.
func privateDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir() && ownedByMe(fi) && fi.Mode().Perm()&0o022 == 0
}

// sessionsDir is the markers' folder, <data>/rhobound-sessions-v1, when it is safe to use: data lies outside the
// repository and the workspace (as written and with its links resolved), data is the user's own and not writable by
// others, and the sessions folder is private (privateDir). create makes the missing levels (data as
// its host named it, the sessions folder one level, never through a link).
func sessionsDir(data, root, ws string, create bool) (string, bool) {
	if !filepath.IsAbs(data) || under(data, root) || under(data, ws) {
		return "", false
	}
	if create {
		if err := os.MkdirAll(data, 0o700); err != nil {
			return "", false
		}
	}
	// The data folder (its place is the host's; it may be a link) must be the user's own and not writable by others;
	// a group-writable one (a umask of 002) is accepted. The sessions folder in it must be private and not a link.
	if fi, err := os.Stat(data); err != nil || !fi.IsDir() || !ownedByMe(fi) || fi.Mode().Perm()&0o002 != 0 {
		return "", false
	}
	s := filepath.Join(data, sessionsName)
	if create {
		if err := os.Mkdir(s, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", false
		}
	}
	if !privateDir(s) {
		return "", false
	}
	return s, true
}

// workspace is the session's folder: the payload's cwd when it is an absolute folder, else the process's.
func (h *museHook) workspace(p hookPayload) string {
	if filepath.IsAbs(p.Cwd) {
		if fi, err := os.Stat(p.Cwd); err == nil && fi.IsDir() {
			return filepath.Clean(p.Cwd)
		}
	}
	return h.cwd
}

// optIn finds the repository's opt-in file, .rhobound/muse.json, from dir up to the first folder holding .git, and
// returns that folder (the repository's root) when the file turns the reminder on. The file has one closed form,
// {"schema": "rhobound.muse/v1", "remind_predict": true|false}: a repository can switch the reminder and nothing else
// (no path, binary or setting is ever read from the workspace). .rhobound must be a real folder, not a link. Outside a
// repository nothing is armed.
func optIn(dir string) (string, bool) {
	if !filepath.IsAbs(dir) {
		return "", false
	}
	on := false
	for {
		if !on {
			if di, err := os.Lstat(filepath.Join(dir, ".rhobound")); err == nil && di.IsDir() {
				p := filepath.Join(dir, ".rhobound", "muse.json")
				if fi, err := os.Lstat(p); err == nil {
					if on = readOptIn(p, fi); !on {
						return "", false
					}
				}
			}
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir, on
		}
		up := filepath.Dir(dir)
		if up == dir {
			return "", false
		}
		dir = up
	}
}

// readOptIn reads an opt-in file: true only for the closed form with remind_predict true.
func readOptIn(p string, fi os.FileInfo) bool {
	if !fi.Mode().IsRegular() || fi.Size() > optInMaxBytes {
		return false
	}
	// Opened without following a link and without blocking (a FIFO swapped in after the Lstat), then checked again.
	b, err := readBounded(p, optInMaxBytes, false)
	if err != nil {
		return false
	}
	var o struct {
		Schema        string `json:"schema"`
		RemindPredict *bool  `json:"remind_predict"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&o) != nil || dec.More() || o.Schema != "rhobound.muse/v1" || o.RemindPredict == nil {
		return false
	}
	return *o.RemindPredict
}

func (h *museHook) sessionStart(ctx context.Context) {
	p, ok := h.payload(ctx)
	if !ok || p.SessionID == "" {
		return
	}
	ws := h.workspace(p)
	root, on := optIn(ws)
	if !on {
		return
	}
	env := h.confined(root, ws)
	// nothing is written, and nothing removed, until the server is registered, enabled and passes its start checks
	cfgPath, code := hookRegistration(env, root, ws)
	if code != "" {
		return
	}
	cfg, _, err := startCheck(cfgPath)
	if err != nil || !complete(cfg) || under(cfg.StateDir, root) || under(cfg.StateDir, ws) {
		return // a state folder in the repository ("~/" from a HOME the host pointed there) would be its to fill
	}
	data, err := dataDir(env)
	if err != nil {
		return
	}
	sessions, ok := sessionsDir(data, root, ws, true)
	if !ok {
		return
	}
	pruneMarkers(sessions, h.now().Add(-markerRetention))
	m := sessionMarker{StartedAt: h.now().UTC(), Root: root, StateDir: cfg.StateDir}
	if fp, ok := fingerprint(ctx, root, env); ok {
		m.Fingerprint = &fp
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	// O_EXCL: a resumed or compacted session keeps the marker of its start.
	f, err := os.OpenFile(filepath.Join(sessions, markerName(p.SessionID)+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr != nil || cerr != nil {
		os.Remove(f.Name())
	}
}

func (h *museHook) stop(ctx context.Context) int {
	p, ok := h.payload(ctx)
	if !ok || p.SessionID == "" || string(p.StopHookActive) == "true" {
		return 0
	}
	ws := h.workspace(p)
	root, on := optIn(ws)
	if !on {
		return 0
	}
	env := h.confined(root, ws)
	data, err := dataDir(env)
	if err != nil {
		return 0
	}
	sessions, ok := sessionsDir(data, root, ws, false)
	if !ok {
		return 0
	}
	base := filepath.Join(sessions, markerName(p.SessionID))
	b, err := readBounded(base+".json", markerMaxBytes, false)
	if err != nil {
		return 0
	}
	var m sessionMarker
	if json.Unmarshal(b, &m) != nil || m.Fingerprint == nil || m.Root != root || !filepath.IsAbs(m.StateDir) {
		return 0 // a marker of another repository (a reused id, a session that moved) is not this one's
	}
	if _, err := os.Lstat(base + ".reminded"); err == nil {
		return 0
	}
	fp, ok := fingerprint(ctx, m.Root, env)
	if !ok || fp == *m.Fingerprint {
		return 0
	}
	if seen, err := state.PreviewedSince(filepath.Join(m.StateDir, "server"), m.StartedAt); err != nil || seen {
		return 0
	}
	// O_EXCL: of two Stops at once, one reminds.
	f, err := os.OpenFile(base+".reminded", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0
	}
	f.Close()
	fmt.Fprintln(h.stderr, museStopReason)
	return 10
}

// pruneMarkers removes the hooks' own session files (markerRE) last changed before t; nothing else in the folder.
func pruneMarkers(dir string, t time.Time) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !markerRE.MatchString(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.Mode().IsRegular() && fi.ModTime().Before(t) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// gitDirs are where git is looked for after the absolute folders of PATH (a hook's environment may have none).
var gitDirs = []string{"/usr/bin", "/bin", "/usr/local/bin", "/opt/homebrew/bin"}

// findGit is the first git in the absolute folders of path, then gitDirs: never one a relative entry would name.
func findGit(path string) string {
	for _, d := range append(filepath.SplitList(path), gitDirs...) {
		if !filepath.IsAbs(d) {
			continue
		}
		p := filepath.Join(d, "git")
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// fingerprint is a digest of the repository at root's tracked and untracked (not ignored) files: their names from
// git, their type, size and modification time from lstat. Nothing in the repository is read as content, and git is
// kept from running anything the repository's configuration names: ls-files runs no filter, textconv, diff driver or
// hook; fsmonitor is off; no transport is allowed and a partial clone never fetches what it lacks (its fetch would run
// the configured upload-pack or ssh command); git finds the repository itself, so its ownership check (safe.directory)
// applies; no system or global configuration is read, but the default global ignore file is (git ignores what the user
// ignores; a core.excludesFile set in ~/.gitconfig is not seen). ok is false when there is no git, no repository, git
// refuses it, or the listing does not finish within gitBudget.
func fingerprint(ctx context.Context, root string, getenv func(string) string) (string, bool) {
	git := findGit(getenv("PATH"))
	if git == "" {
		return "", false
	}
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		return "", false
	}
	// git compares its ceiling with the physical folder, and splits it on ':'
	root, err := filepath.EvalSymlinks(root)
	if err != nil || strings.ContainsRune(root, filepath.ListSeparator) {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	env := []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(root), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=none", "GIT_PROTOCOL_FROM_USER=0",
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	if home, err := homeDir(getenv); err == nil {
		env = append(env, "HOME="+home) // for the default global ignore file, ~/.config/git/ignore
	}
	if x := getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
		env = append(env, "XDG_CONFIG_HOME="+x)
	}
	run := func(out io.Writer, args ...string) bool {
		cmd := exec.CommandContext(ctx, git, append([]string{"-c", "core.fsmonitor=false", "-c",
			"core.untrackedCache=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = root, env, nil, out, nil
		cmd.WaitDelay = 200 * time.Millisecond // a child git left behind cannot hold the pipe past the deadline
		return cmd.Run() == nil && ctx.Err() == nil
	}
	// the repository git finds must be the one at root (an empty or broken .git would let it climb)
	var top capped
	if !run(&top, "rev-parse", "--show-toplevel") || strings.TrimSpace(top.b.String()) != root {
		return "", false
	}
	var listing capped
	if !run(&listing, "ls-files", "-z", "--cached", "--others", "--exclude-standard") {
		return "", false
	}
	// every name is looked at inside root only: a name with '..' or a leading '/' (from a crafted index), or a path
	// through a linked folder, is not followed out of it
	r, err := os.OpenRoot(root)
	if err != nil {
		return "", false
	}
	defer r.Close()
	hs := sha256.New()
	w := bufio.NewWriter(hs)
	n := 0
	seen := map[string]bool{}
	for _, name := range strings.Split(listing.b.String(), "\x00") {
		if name == "" || seen[name] {
			continue // ls-files repeats a name once per unmerged stage
		}
		seen[name] = true
		if n++; n > fpMaxFiles || ctx.Err() != nil {
			return "", false
		}
		w.WriteString(name)
		w.WriteByte(0)
		if !filepath.IsLocal(name) {
			w.WriteString("!")
		} else if fi, err := r.Lstat(name); err != nil {
			w.WriteString("-")
		} else {
			w.WriteString(fi.Mode().String())
			w.WriteByte(0)
			w.WriteString(strconv.FormatInt(fi.Size(), 10))
			w.WriteByte(0)
			w.WriteString(strconv.FormatInt(fi.ModTime().UnixNano(), 10))
		}
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", false
	}
	return hex.EncodeToString(hs.Sum(nil)), true
}

// capped is a buffer that fails a write past gitMaxListing bytes, which ends the listing.
type capped struct{ b bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if c.b.Len()+len(p) > gitMaxListing {
		return 0, errors.New("listing too long")
	}
	return c.b.Write(p)
}
