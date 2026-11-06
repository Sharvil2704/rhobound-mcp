// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// museBed is a user with the server registered in Muse's settings and a repository that may opt in: a home folder
// holding a fake engine, its configuration, Muse's settings and the plugin's data folder, and a git repository.
type museBed struct {
	t                 *testing.T
	home, repo, data  string
	cfgPath, stateDir string
	settings          string
	env               map[string]string
	now               func() time.Time
	sessionID         string
}

const optInOn = `{"schema": "rhobound.muse/v1", "remind_predict": true}`

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"}
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
}

// newMuseBed makes the bed; optIn is the repository's .rhobound/muse.json ("" for none).
func newMuseBed(t *testing.T, optIn string) *museBed {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	home := t.TempDir()
	b := &museBed{t: t, home: home, repo: filepath.Join(home, "repo"), data: filepath.Join(home, "plugin-data"),
		cfgPath: filepath.Join(home, ".config", "rhobound-mcp", "config.json"), stateDir: filepath.Join(home, "state"),
		settings: filepath.Join(home, ".config", "muse", "settings.json"), now: time.Now, sessionID: "s-0001"}
	b.env = map[string]string{"HOME": home, "MUSE_PLUGIN_DATA_DIR": b.data, "PATH": "/usr/bin:/bin"}
	for _, d := range []string{filepath.Join(home, "bin"), filepath.Join(home, "captures", "loop"), filepath.Dir(b.cfgPath),
		filepath.Dir(b.settings), b.repo} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	eng := filepath.Join(home, "bin", "rhobound")
	write(t, eng, engine, 0o755)
	sum := sha256.Sum256([]byte(engine))
	write(t, filepath.Join(home, "policy.json"), "{}", 0o600)
	b.writeConfig(hex.EncodeToString(sum[:]), true)
	server := filepath.Join(home, "bin", "rhobound-mcp")
	write(t, server, "#!/bin/sh\nexit 0\n", 0o755)
	doc, _, err := museSplice(nil, newMuseEntry(server, b.cfgPath, true), false)
	if err != nil {
		t.Fatal(err)
	}
	write(t, b.settings, string(doc), 0o600)
	gitIn(t, b.repo, "init", "-q")
	write(t, filepath.Join(b.repo, "a.go"), "package a\n", 0o600)
	gitIn(t, b.repo, "add", "a.go")
	if optIn != "" {
		write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), optIn, 0o600)
	}
	return b
}

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

// writeConfig writes the server's configuration: complete, or with no policy.
func (b *museBed) writeConfig(sha string, withPolicy bool) {
	c := map[string]any{"schema": "rhobound-mcp.config/v1",
		"engine":    map[string]string{"path": filepath.Join(b.home, "bin", "rhobound"), "sha256": sha},
		"state_dir": b.stateDir, "captures": map[string]string{"loop": filepath.Join(b.home, "captures", "loop")}}
	if withPolicy {
		c["policies"] = map[string]string{"default": filepath.Join(b.home, "policy.json")}
	}
	j, _ := json.Marshal(c)
	write(b.t, b.cfgPath, string(j), 0o600)
}

// fixture is a hook fixture's stdin, with the bed's session id and workspace in place of the placeholders.
func (b *museBed) fixture(name, cwd string) []byte {
	b.t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "muse", name))
	if err != nil {
		b.t.Fatal(err)
	}
	var f struct {
		Event string         `json:"event"`
		Stdin map[string]any `json:"stdin"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Stdin["hook_event_name"] != f.Event {
		b.t.Fatalf("%s: %v", name, err)
	}
	f.Stdin["session_id"], f.Stdin["cwd"] = b.sessionID, cwd
	j, _ := json.Marshal(f.Stdin)
	return j
}

// hook runs one event with stdin and returns its exit status and what it wrote to stderr.
func (b *museBed) hook(event string, stdin []byte) (int, string) {
	var errb bytes.Buffer
	h := museHook{getenv: func(k string) string { return b.env[k] }, now: b.now, stdin: bytes.NewReader(stdin),
		stderr: &errb, cwd: "/"}
	return h.run(context.Background(), event), errb.String()
}

func (b *museBed) start() int {
	c, _ := b.hook("session-start", b.fixture("session-start.json", b.repo))
	return c
}

func (b *museBed) stop() (int, string) { return b.hook("stop", b.fixture("stop.json", b.repo)) }

func (b *museBed) marker() string {
	return filepath.Join(b.data, sessionsName, markerName(b.sessionID)+".json")
}

func (b *museBed) armed() bool {
	_, err := os.Stat(b.marker())
	return err == nil
}

// edit changes a tracked file (its size, so the fingerprint changes whatever the clock's grain).
func (b *museBed) edit() {
	b.t.Helper()
	f, err := os.OpenFile(filepath.Join(b.repo, "a.go"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		b.t.Fatal(err)
	}
	f.WriteString("// x\n")
	f.Close()
}

func (b *museBed) preview(at time.Time) {
	b.t.Helper()
	d, err := state.NewDir(filepath.Join(b.stateDir, "server"), nil)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := d.AppendPreview(context.Background(), state.PreviewKey{Repo: "r", Principal: state.Principal("p"), Branch: "b"},
		state.PreviewRow{Time: at, ChangeHash: "h", Verdict: "pass"}); err != nil {
		b.t.Fatal(err)
	}
}

// TestMuseHookArms: SessionStart arms a session only when the repository opts in with the closed form and the
// server is registered, enabled, passes its start checks and names a policy and a capture; it writes nothing else,
// creates no state folder, and never fails.
func TestMuseHookArms(t *testing.T) {
	b := newMuseBed(t, optInOn)
	if c := b.start(); c != 0 || !b.armed() {
		t.Fatalf("a healthy opted-in session: exit %d, armed %v", c, b.armed())
	}
	var m sessionMarker
	raw, _ := os.ReadFile(b.marker())
	if json.Unmarshal(raw, &m) != nil || m.Fingerprint == nil || m.Root != b.repo || m.StateDir != b.stateDir {
		t.Errorf("the marker: %s", raw)
	}
	if _, err := os.Stat(b.stateDir); !os.IsNotExist(err) {
		t.Errorf("SessionStart created the state folder: %v", err)
	}
	if fi, err := os.Stat(b.data); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("the data folder: %v", err)
	}
	// resumed: the marker of the start stays
	before, _ := os.ReadFile(b.marker())
	b.now = func() time.Time { return time.Now().Add(time.Hour) }
	b.start()
	if after, _ := os.ReadFile(b.marker()); !bytes.Equal(before, after) {
		t.Error("a second SessionStart rewrote the marker")
	}

	// from a subfolder, the opt-in file of the repository's root counts, and the root is fingerprinted
	sub := newMuseBed(t, optInOn)
	if err := os.MkdirAll(filepath.Join(sub.repo, "pkg", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if c, _ := sub.hook("session-start", sub.fixture("session-start.json", filepath.Join(sub.repo, "pkg", "deep"))); c != 0 || !sub.armed() {
		t.Error("a session in a subfolder was not armed")
	}

	// no data folder named: the fallback under the home folder
	fb := newMuseBed(t, optInOn)
	delete(fb.env, "MUSE_PLUGIN_DATA_DIR")
	fb.start()
	if _, err := os.Stat(filepath.Join(fb.home, ".cache", "rhobound-mcp", "muse", sessionsName, markerName("s-0001")+".json")); err != nil {
		t.Errorf("the fallback data folder: %v", err)
	}

	for name, setup := range map[string]func(b *museBed){
		"no opt-in": func(b *museBed) { os.RemoveAll(filepath.Join(b.repo, ".rhobound")) },
		"opt-in false": func(b *museBed) {
			write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), `{"schema":"rhobound.muse/v1","remind_predict":false}`, 0o600)
		},
		"opt-in extra key": func(b *museBed) {
			write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), `{"schema":"rhobound.muse/v1","remind_predict":true,"engine":"/tmp/x"}`, 0o600)
		},
		"opt-in old schema": func(b *museBed) {
			write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), `{"schema":"rhobound.muse/v0","remind_predict":true}`, 0o600)
		},
		"opt-in two values": func(b *museBed) { write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), optInOn+optInOn, 0o600) },
		"opt-in symlink": func(b *museBed) {
			p := filepath.Join(b.repo, ".rhobound", "muse.json")
			write(t, filepath.Join(b.home, "elsewhere.json"), optInOn, 0o600)
			os.Remove(p)
			if err := os.Symlink(filepath.Join(b.home, "elsewhere.json"), p); err != nil {
				t.Fatal(err)
			}
		},
		"opt-in huge": func(b *museBed) {
			write(t, filepath.Join(b.repo, ".rhobound", "muse.json"), optInOn+strings.Repeat(" ", 5000), 0o600)
		},
		"not a repository": func(b *museBed) { os.RemoveAll(filepath.Join(b.repo, ".git")) },
		"no settings":      func(b *museBed) { os.Remove(b.settings) },
		"disabled": func(b *museBed) {
			doc, _, _ := museSplice(nil, newMuseEntry(filepath.Join(b.home, "bin", "rhobound-mcp"), b.cfgPath, false), false)
			write(t, b.settings, string(doc), 0o600)
		},
		"both spellings": func(b *museBed) {
			doc, _ := os.ReadFile(b.settings)
			write(t, b.settings, strings.Replace(string(doc), `"schema_version": 1,`, `"schema_version": 1, "mcpServers": {},`, 1), 0o600)
		},
		"engine rebuilt": func(b *museBed) { b.writeConfig(strings.Repeat("0", 64), true) },
		"no policy": func(b *museBed) {
			b.writeConfig(func() string { s := sha256.Sum256([]byte(engine)); return hex.EncodeToString(s[:]) }(), false)
		},
		"no session id":   func(b *museBed) { b.sessionID = "" },
		"no home, no dir": func(b *museBed) { b.env = map[string]string{"PATH": "/usr/bin:/bin"} },
	} {
		t.Run(name, func(t *testing.T) {
			b := newMuseBed(t, optInOn)
			setup(b)
			if c := b.start(); c != 0 {
				t.Errorf("exit %d", c)
			}
			ents, _ := os.ReadDir(filepath.Join(b.data, sessionsName))
			if len(ents) != 0 {
				t.Errorf("armed: %v", ents)
			}
		})
	}
}

// TestMuseHookStop: the Stop matrix. Stop reminds (exit 10, the fixed reason on stderr) only when the session is
// armed, stop_hook_active is not the literal true, it has not reminded, the files changed since the start, and the
// preview log holds no row since the start; then never again in that session.
func TestMuseHookStop(t *testing.T) {
	b := newMuseBed(t, optInOn)
	b.start()
	if c, e := b.stop(); c != 0 || e != "" {
		t.Errorf("no change: %d %q", c, e)
	}
	b.edit()
	if c, _ := b.hook("stop", b.fixture("stop-active.json", b.repo)); c != 0 {
		t.Errorf("stop_hook_active: %d", c)
	}
	// the fixture's last message holds the text "stop_hook_active": true, inside a string: not the flag
	c, e := b.stop()
	if c != 10 || e != museStopReason+"\n" {
		t.Fatalf("changed, no preview: %d %q", c, e)
	}
	if c, e := b.stop(); c != 0 || e != "" {
		t.Errorf("a second Stop: %d %q", c, e)
	}
	if strings.ContainsAny(museStopReason, "\n\r") || len(museStopReason) > 600 ||
		!strings.Contains(museStopReason, "Previews are advisory; CI decides.") {
		t.Errorf("the reason: %q", museStopReason)
	}

	t.Run("previewed", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		b.start()
		b.edit()
		b.preview(time.Now())
		if c, _ := b.stop(); c != 0 {
			t.Errorf("a preview since the start: %d", c)
		}
	})
	t.Run("old row, new file", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		b.start()
		b.edit()
		b.preview(time.Now().Add(-time.Hour)) // the log file is new; its row is from before the session
		if c, _ := b.stop(); c != 10 {
			t.Errorf("an old row counted: %d", c)
		}
	})
	t.Run("not armed", func(t *testing.T) {
		b := newMuseBed(t, "")
		b.start()
		b.edit()
		if c, _ := b.stop(); c != 0 {
			t.Errorf("%d", c)
		}
	})
	t.Run("no fingerprint", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		b.start()
		raw, _ := os.ReadFile(b.marker())
		write(t, b.marker(), strings.Replace(string(raw), `"fingerprint":"`, `"fingerprint":null,"x":"`, 1), 0o600)
		b.edit()
		if c, _ := b.stop(); c != 0 {
			t.Errorf("%d", c)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		b.start()
		b.edit()
		var mu sync.Mutex
		var wg sync.WaitGroup
		n := 0
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if c, _ := b.stop(); c == 10 {
					mu.Lock()
					n++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if n != 1 {
			t.Errorf("20 Stops at once reminded %d times", n)
		}
	})
}

// TestMuseHookHostile: hostile ids, payloads and repositories. Session ids name files inside the data folder only;
// big, broken or odd payloads exit 0; a panic exits 0; the repository's git configuration (fsmonitor, filters,
// diff drivers, textconv, an external diff) never runs; a git that hangs leaves the session unreminded.
func TestMuseHookHostile(t *testing.T) {
	for _, id := range []string{"../x", "a/b", "..", ".hidden", strings.Repeat("a", 300), "nul\x00x", "sp ace"} {
		b := newMuseBed(t, optInOn)
		b.sessionID = id
		b.start()
		ents, _ := os.ReadDir(filepath.Join(b.data, sessionsName))
		if len(ents) != 1 || !markerRE.MatchString(ents[0].Name()) {
			t.Errorf("id %q: %v", id, ents)
		}
		up, _ := os.ReadDir(b.data)
		if len(up) != 1 {
			t.Errorf("id %q wrote beside sessions: %v", id, up)
		}
		b.edit()
		if c, _ := b.stop(); c != 10 {
			t.Errorf("id %q: the session was not found again: %d", id, c)
		}
	}

	b := newMuseBed(t, optInOn)
	b.start()
	b.edit()
	big := func(n int) []byte {
		j, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "session_id": "s-0001", "cwd": b.repo,
			"stop_hook_active": false, "last_assistant_message": strings.Repeat("x", n)})
		return j
	}
	if c, _ := b.hook("stop", big(9<<20)); c != 0 {
		t.Errorf("a 9 MiB payload: %d", c)
	}
	for name, in := range map[string]string{"broken": `{"session_id": "s-0001", `, "empty": "", "array": `[1]`,
		"string flag": `{"session_id": "s-0001", "stop_hook_active": "yes"}`} {
		if c, _ := b.hook("nope", []byte(in)); c != 0 {
			t.Errorf("unknown event, %s: %d", name, c)
		}
		if name != "string flag" {
			if c, _ := b.hook("stop", []byte(in)); c != 0 {
				t.Errorf("%s: %d", name, c)
			}
		}
	}
	if c, _ := b.hook("stop", big(5<<20)); c != 10 {
		t.Errorf("a 5 MiB payload is read: %d", c)
	}
	h := museHook{stdin: strings.NewReader(`{"session_id": "s-0001"}`), stderr: &bytes.Buffer{}, now: time.Now}
	if c := h.run(context.Background(), "stop"); c != 0 { // getenv is nil: the run panics
		t.Errorf("a panic: %d", c)
	}

	t.Run("git config", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		log := filepath.Join(b.home, "ran.log")
		evil := filepath.Join(b.home, "evil.sh")
		write(t, evil, "#!/bin/sh\necho \"$0 $*\" >> "+log+"\ncat\n", 0o755)
		cfg := "[core]\n\tfsmonitor = " + evil + "\n\thooksPath = " + b.home + "\n[filter \"evil\"]\n\tclean = " + evil +
			"\n\tsmudge = " + evil + "\n\tprocess = " + evil + "\n\trequired = true\n[diff \"evil\"]\n\ttextconv = " + evil +
			"\n\tcommand = " + evil + "\n[diff]\n\texternal = " + evil + "\n"
		f, _ := os.OpenFile(filepath.Join(b.repo, ".git", "config"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(cfg)
		f.Close()
		write(t, filepath.Join(b.repo, ".gitattributes"), "* filter=evil diff=evil\n", 0o600)
		if c := b.start(); c != 0 || !b.armed() {
			t.Fatal("not armed")
		}
		b.edit()
		if c, _ := b.stop(); c != 10 {
			t.Errorf("Stop: %d", c)
		}
		if out, err := os.ReadFile(log); err == nil {
			t.Errorf("the repository's git configuration ran: %s", out)
		}
		// the control: the planted configuration is live for an ordinary git command
		for _, args := range [][]string{{"diff", "HEAD"}, {"add", "a.go"}} {
			cmd := exec.Command("git", args...)
			cmd.Dir = b.repo
			cmd.Env = []string{"HOME=" + b.home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"}
			cmd.Run() // the planted process filter does not speak git's protocol: git add fails, after running it
		}
		if _, err := os.ReadFile(log); err != nil {
			t.Error("control: the planted git configuration did not run for git diff and git add, so this test shows nothing")
		}
	})
	t.Run("git hangs", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		fake := filepath.Join(b.home, "fakebin")
		write(t, filepath.Join(fake, "git"), "#!/bin/sh\nsleep 4\n", 0o755)
		b.env["PATH"] = fake + ":/usr/bin:/bin"
		t0 := time.Now()
		b.start()
		if d := time.Since(t0); d > 3*time.Second {
			t.Errorf("SessionStart took %v", d)
		}
		raw, _ := os.ReadFile(b.marker())
		if !strings.Contains(string(raw), `"fingerprint":null`) {
			t.Errorf("the marker: %s", raw)
		}
		b.edit()
		if c, _ := b.stop(); c != 0 {
			t.Errorf("no fingerprint, yet reminded: %d", c)
		}
	})
	t.Run("relative PATH", func(t *testing.T) {
		if g := findGit("fakebin:.:bin"); g != "/usr/bin/git" && g != "/bin/git" {
			t.Errorf("findGit took a relative entry: %q", g)
		}
	})
}

// TestMuseHookLazyFetch: a partial clone that lacks a tree it would list runs its remote's upload-pack command when an
// ordinary git fetches it lazily; the hooks' git never does (GIT_NO_LAZY_FETCH, no transport allowed).
func TestMuseHookLazyFetch(t *testing.T) {
	b := newMuseBed(t, "")
	l := filepath.Join(b.home, "lf")
	os.MkdirAll(l, 0o700)
	g := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com",
			"-c", "protocol.file.allow=always"}, args...)...)
		cmd.Dir = dir
		cmd.Env = []string{"HOME=" + l, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s (this git cannot make the partial clone)", args, err, out)
		}
	}
	g(l, "init", "-q", "origin")
	write(t, filepath.Join(l, "origin", "keep", "k"), "k\n", 0o600)
	write(t, filepath.Join(l, "origin", "far", "deep", "f"), "f\n", 0o600)
	g(filepath.Join(l, "origin"), "add", "-A")
	g(filepath.Join(l, "origin"), "commit", "-qm", "c1")
	g(filepath.Join(l, "origin"), "config", "uploadpack.allowFilter", "true")
	g(filepath.Join(l, "origin"), "config", "uploadpack.allowAnySHA1InWant", "true")
	g(l, "clone", "-q", "--filter=tree:0", "--sparse", "file://"+filepath.Join(l, "origin"), "victim")
	victim := filepath.Join(l, "victim")
	g(victim, "sparse-checkout", "set", "--cone", "--sparse-index", "keep")
	// drop the out-of-cone tree, so that listing it needs a lazy fetch: unpack the packs, then remove its object
	ls := exec.Command("git", "ls-files", "--sparse", "-s")
	ls.Dir, ls.Env = victim, []string{"HOME=" + l, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin", "GIT_NO_LAZY_FETCH=1"}
	lsOut, _ := ls.Output()
	far := ""
	for _, line := range strings.Split(string(lsOut), "\n") {
		if f := strings.Fields(line); len(f) == 4 && f[3] == "far/" {
			far = f[1]
		}
	}
	if len(far) != 40 {
		t.Skipf("no sparse directory entry for far/: %q", lsOut)
	}
	packs, _ := filepath.Glob(filepath.Join(victim, ".git", "objects", "pack", "*.pack"))
	for _, pk := range packs {
		moved := filepath.Join(l, filepath.Base(pk))
		os.Rename(pk, moved)
		os.Remove(strings.TrimSuffix(pk, ".pack") + ".idx")
		os.Remove(strings.TrimSuffix(pk, ".pack") + ".promisor")
		in, _ := os.Open(moved)
		up := exec.Command("git", "unpack-objects", "-q")
		up.Dir, up.Env, up.Stdin = victim, ls.Env, in
		up.Run()
		in.Close()
	}
	os.Remove(filepath.Join(victim, ".git", "objects", far[:2], far[2:]))
	g(victim, "config", "remote.origin.url", "/nonexistent/origin")
	pwned := filepath.Join(l, "PWNED")
	g(victim, "config", "remote.origin.uploadpack", "id >> "+pwned+"; false")
	write(t, filepath.Join(victim, ".rhobound", "muse.json"), optInOn, 0o600)

	// the control: an ordinary listing of this clone runs the planted command
	ctl := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	ctl.Dir = victim
	ctl.Env = []string{"HOME=" + l, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"}
	ctl.Run()
	if _, err := os.Stat(pwned); err != nil {
		t.Skip("control: this git does not fetch lazily here, so the attack cannot be shown")
	}
	os.Remove(pwned)

	b.repo = victim
	b.start()
	b.edit2(filepath.Join(victim, "keep", "k"))
	b.stop()
	if out, err := os.ReadFile(pwned); err == nil {
		t.Errorf("the hooks ran the repository's upload-pack command: %s", out)
	}

	// the same over ssh, whose command the repository names too (ssh is allowed to user and child processes alike)
	g(victim, "config", "remote.origin.url", "ssh://example.invalid/x")
	g(victim, "config", "core.sshCommand", "id >> "+pwned+"; false")
	ctl = exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	ctl.Dir, ctl.Env = victim, []string{"HOME=" + l, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "PATH=/usr/bin:/bin"}
	ctl.Run()
	if _, err := os.Stat(pwned); err != nil {
		t.Fatal("control: the ssh command did not run for an ordinary listing")
	}
	os.Remove(pwned)
	b.sessionID = "s-0002"
	b.start()
	b.edit2(filepath.Join(victim, "keep", "k"))
	b.stop()
	if out, err := os.ReadFile(pwned); err == nil {
		t.Errorf("the hooks ran the repository's ssh command: %s", out)
	}
}

// edit2 appends to a file.
func (b *museBed) edit2(p string) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		b.t.Fatal(err)
	}
	f.WriteString("x\n")
	f.Close()
}

// TestMuseHookIgnores: files the user's global git ignore file ignores are not changes.
func TestMuseHookIgnores(t *testing.T) {
	b := newMuseBed(t, optInOn)
	write(t, filepath.Join(b.home, ".config", "git", "ignore"), "*.swp\n__pycache__/\n", 0o600)
	b.start()
	write(t, filepath.Join(b.repo, ".a.go.swp"), "swap", 0o600)
	write(t, filepath.Join(b.repo, "__pycache__", "a.pyc"), "x", 0o600)
	if c, _ := b.stop(); c != 0 {
		t.Errorf("globally ignored files counted as changes: %d", c)
	}
	b.edit()
	if c, _ := b.stop(); c != 10 {
		t.Errorf("a tracked edit: %d", c)
	}
}

// TestMuseHookStdinOpen: a stdin that stays open without a whole JSON value is given up at the hook's deadline.
func TestMuseHookStdinOpen(t *testing.T) {
	b := newMuseBed(t, optInOn)
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte(`{"session_id": "s-0001", `))
	h := museHook{getenv: func(k string) string { return b.env[k] }, now: time.Now, stdin: r, stderr: &bytes.Buffer{}, cwd: "/"}
	t0 := time.Now()
	if c := h.run(context.Background(), "session-start"); c != 0 || time.Since(t0) > hookBudget+time.Second {
		t.Errorf("exit %d after %v", c, time.Since(t0))
	}
}

// TestMuseHookOptInFIFO: an opt-in "file" that is a FIFO is not read (reading it would block).
func TestMuseHookOptInFIFO(t *testing.T) {
	b := newMuseBed(t, "")
	p := filepath.Join(b.repo, ".rhobound", "muse.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip(err)
	}
	t0 := time.Now()
	if c := b.start(); c != 0 || b.armed() || time.Since(t0) > time.Second {
		t.Errorf("exit %d armed %v after %v", c, b.armed(), time.Since(t0))
	}
}

// TestMuseHookRecord: the hooks read the settings file the registration record names, wherever it is.
func TestMuseHookRecord(t *testing.T) {
	b := newMuseBed(t, optInOn)
	moved := filepath.Join(b.home, "xdg", "muse", "settings.json")
	write(t, moved, string(mustRead(t, b.settings)), 0o600)
	os.Remove(b.settings)
	b.start()
	if b.armed() {
		t.Fatal("armed with no settings at the default place and no record")
	}
	rec, _ := json.Marshal(museRecord{Schema: museRecordSchema, Settings: moved})
	write(t, filepath.Join(b.home, ".local", "state", "rhobound-mcp", "muse-registration.json"), string(rec), 0o600)
	b.start()
	if !b.armed() {
		t.Error("the record's settings were not read")
	}
}

// TestMuseHookRecordFallback: a record whose settings file is gone, or registers nothing, gives way to Muse's default
// settings file.
func TestMuseHookRecordFallback(t *testing.T) {
	b := newMuseBed(t, optInOn)
	rec := filepath.Join(b.home, ".local", "state", "rhobound-mcp", "muse-registration.json")
	for name, settings := range map[string]string{"gone": filepath.Join(b.home, "trial.json"), "empty": filepath.Join(b.home, "empty.json")} {
		write(t, filepath.Join(b.home, "empty.json"), `{"schema_version": 1}`, 0o600)
		j, _ := json.Marshal(museRecord{Schema: museRecordSchema, Settings: settings})
		write(t, rec, string(j), 0o600)
		b.sessionID = "s-" + name
		b.start()
		if !b.armed() {
			t.Errorf("%s: the default settings were not read after the record", name)
		}
	}
}

// TestMuseHookRootChecks: git must find the repository at the root the hook chose: a root reached through a link
// whose .git is not a repository does not let git climb into the outer repository.
func TestMuseHookRootChecks(t *testing.T) {
	b := newMuseBed(t, "")
	child := filepath.Join(b.repo, "child")
	os.MkdirAll(filepath.Join(child, ".git"), 0o700) // empty: not a repository
	write(t, filepath.Join(child, ".rhobound", "muse.json"), optInOn, 0o600)
	lnk := filepath.Join(b.home, "lnk")
	if err := os.Symlink(child, lnk); err != nil {
		t.Fatal(err)
	}
	if fp, ok := fingerprint(context.Background(), lnk, func(k string) string { return b.env[k] }); ok {
		t.Errorf("a fingerprint of the outer repository through the link: %s", fp)
	}
	if fp, ok := fingerprint(context.Background(), b.repo, func(k string) string { return b.env[k] }); !ok || fp == "" {
		t.Error("the outer repository itself has no fingerprint")
	}
	// a .git file naming another repository whose work tree is elsewhere: git's top level is not the root
	other := filepath.Join(b.home, "other")
	os.MkdirAll(other, 0o700)
	gitIn(t, other, "init", "-q")
	gitIn(t, other, "config", "core.worktree", other)
	pointer := filepath.Join(b.home, "pointer")
	write(t, filepath.Join(pointer, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n", 0o600)
	if fp, ok := fingerprint(context.Background(), pointer, func(k string) string { return b.env[k] }); ok {
		t.Errorf("a fingerprint of another work tree: %s", fp)
	}
	colon := filepath.Join(b.home, "co:lon")
	os.MkdirAll(colon, 0o700)
	gitIn(t, colon, "init", "-q")
	if _, ok := fingerprint(context.Background(), colon, func(k string) string { return b.env[k] }); ok {
		t.Error("a root with ':' in its path")
	}
}

// old makes a file older than the markers' retention.
func old(t *testing.T, p string) {
	t.Helper()
	write(t, p, "keep me", 0o600)
	past := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(p, past, past); err != nil {
		t.Fatal(err)
	}
}

// TestMuseHookPruneSafe: SessionStart deletes nothing but its own expired markers, in its own private folder, and
// only once the server is registered: not through a linked sessions folder, not in a data folder inside the
// repository, not a file of another name, not before registration.
func TestMuseHookPruneSafe(t *testing.T) {
	t.Run("own markers only", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		s := filepath.Join(b.data, sessionsName)
		old(t, filepath.Join(s, "notes.txt"))
		old(t, filepath.Join(s, "h-"+strings.Repeat("0", 64)+".json"))
		os.Chmod(b.data, 0o700)
		os.Chmod(s, 0o700)
		b.start()
		if _, err := os.Stat(filepath.Join(s, "notes.txt")); err != nil {
			t.Error("a file of another name was pruned")
		}
		if _, err := os.Stat(filepath.Join(s, "h-"+strings.Repeat("0", 64)+".json")); err == nil {
			t.Error("an expired marker was kept")
		}
		if !b.armed() {
			t.Error("not armed")
		}
	})
	t.Run("linked sessions folder", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		victim := filepath.Join(b.home, "victim")
		old(t, filepath.Join(victim, "id_ed25519"))
		old(t, filepath.Join(victim, "h-"+strings.Repeat("1", 64)+".json"))
		os.MkdirAll(b.data, 0o700)
		if err := os.Symlink(victim, filepath.Join(b.data, sessionsName)); err != nil {
			t.Fatal(err)
		}
		b.start()
		ents, _ := os.ReadDir(victim)
		if len(ents) != 2 {
			t.Errorf("the linked folder changed: %v", ents)
		}
	})
	t.Run("data folder in the repository", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		b.env["MUSE_PLUGIN_DATA_DIR"] = filepath.Join(b.repo, ".muse", "data")
		victim := filepath.Join(b.home, "victim")
		old(t, filepath.Join(victim, "h-"+strings.Repeat("2", 64)+".json"))
		os.MkdirAll(filepath.Join(b.repo, ".muse", "data"), 0o700)
		os.Symlink(victim, filepath.Join(b.repo, ".muse", "data", sessionsName))
		b.start()
		if ents, _ := os.ReadDir(victim); len(ents) != 1 {
			t.Errorf("a data folder in the repository was used: %v", ents)
		}
		if _, err := os.Stat(filepath.Join(b.home, ".cache", "rhobound-mcp", "muse", sessionsName, markerName(b.sessionID)+".json")); err != nil {
			t.Error("the hook did not fall back to the data folder under the home folder")
		}
	})
	t.Run("shared folder", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		os.MkdirAll(filepath.Join(b.data, sessionsName), 0o700)
		os.Chmod(filepath.Join(b.data, sessionsName), 0o777)
		b.start()
		if b.armed() {
			t.Error("armed in a folder others can write")
		}
	})
	t.Run("not registered", func(t *testing.T) {
		b := newMuseBed(t, optInOn)
		os.Remove(b.settings)
		b.start()
		if _, err := os.Stat(b.data); !os.IsNotExist(err) {
			t.Error("the data folder was made for a server that is not registered")
		}
	})
}

// TestMuseHookConfined: a HOME or XDG_CONFIG_HOME that points into the repository is not where the hooks look for
// the registration, the settings or the configuration (which here name an engine that would be read forever).
func TestMuseHookConfined(t *testing.T) {
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME"} {
		t.Run(k, func(t *testing.T) {
			b := newMuseBed(t, optInOn)
			planted := filepath.Join(b.repo, "planted")
			cfg := filepath.Join(planted, "config.json")
			write(t, cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"/dev/zero","sha256":"`+strings.Repeat("0", 64)+
				`"},"state_dir":"`+filepath.Join(planted, "state")+`","captures":{"x":"`+planted+`"},"policies":{"d":"`+cfg+`"}}`, 0o600)
			doc, _, _ := museSplice(nil, newMuseEntry("/usr/bin/rhobound-mcp", cfg, true), false)
			conf := planted
			if k == "HOME" {
				conf = filepath.Join(planted, ".config")
			}
			write(t, filepath.Join(conf, "muse", "settings.json"), string(doc), 0o600)
			os.Remove(b.settings)
			b.env[k] = planted
			t0 := time.Now()
			b.start()
			if d := time.Since(t0); d > time.Second || b.armed() {
				t.Errorf("%s in the repository was used: %v, armed %v", k, d, b.armed())
			}
		})
	}
}

// TestMuseHookBoundedReads: a marker, a settings file or an engine that is a device is refused at once.
func TestMuseHookBoundedReads(t *testing.T) {
	b := newMuseBed(t, optInOn)
	b.start()
	b.edit()
	os.Remove(b.marker())
	if err := os.Symlink("/dev/zero", b.marker()); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if c, _ := b.stop(); c != 0 || time.Since(t0) > time.Second {
		t.Errorf("a marker linked to /dev/zero: %d after %v", c, time.Since(t0))
	}
	c := newMuseBed(t, optInOn)
	os.Remove(c.settings)
	os.Symlink("/dev/zero", c.settings)
	t0 = time.Now()
	if c.start(); c.armed() || time.Since(t0) > time.Second {
		t.Errorf("settings linked to /dev/zero: armed %v after %v", c.armed(), time.Since(t0))
	}
	cfg := filepath.Join(t.TempDir(), "config.json")
	write(t, cfg, `{"schema":"rhobound-mcp.config/v1","engine":{"path":"/dev/zero","sha256":"`+strings.Repeat("0", 64)+`"},"state_dir":"/s"}`, 0o600)
	t0 = time.Now()
	if _, code, err := startCheck(cfg); err == nil || code != "engine.unusable" || time.Since(t0) > time.Second {
		t.Errorf("an engine that is /dev/zero: %q %v after %v", code, err, time.Since(t0))
	}
}

// TestMuseHookStaysInRoot: names reached through a linked folder are not followed out of the repository, so a file
// outside it does not count as a change; and a marker armed for another repository is not this session's.
func TestMuseHookStaysInRoot(t *testing.T) {
	b := newMuseBed(t, optInOn)
	out := filepath.Join(b.home, "outside")
	write(t, filepath.Join(out, "f"), "x", 0o600)
	write(t, filepath.Join(b.repo, "d", "f"), "x", 0o600)
	gitIn(t, b.repo, "add", "d/f")
	os.RemoveAll(filepath.Join(b.repo, "d"))
	if err := os.Symlink(out, filepath.Join(b.repo, "d")); err != nil {
		t.Fatal(err)
	}
	b.start()
	write(t, filepath.Join(out, "f"), "changed, and longer", 0o600)
	if c, _ := b.stop(); c != 0 {
		t.Errorf("a file outside the repository counted: %d", c)
	}
	// the same session id in another repository: its Stop does not use this marker
	other := newMuseBed(t, optInOn)
	other.home, other.data, other.env = b.home, b.data, b.env
	b.edit()
	if c, _ := other.hook("stop", other.fixture("stop.json", other.repo)); c != 0 {
		t.Errorf("another repository's Stop used this marker: %d", c)
	}
}

// TestMuseHookLinkedDataDir: a data folder that is a link to a private folder outside the repository is used; one
// whose target is shared, or inside the repository, is not.
func TestMuseHookLinkedDataDir(t *testing.T) {
	b := newMuseBed(t, optInOn)
	real := filepath.Join(b.home, "disk2", "data")
	os.MkdirAll(real, 0o700)
	link := filepath.Join(b.home, "data-link")
	os.Symlink(real, link)
	b.env["MUSE_PLUGIN_DATA_DIR"] = link
	b.start()
	if _, err := os.Stat(filepath.Join(real, sessionsName, markerName(b.sessionID)+".json")); err != nil {
		t.Error("a data folder linked to a private folder was not used")
	}
	c := newMuseBed(t, optInOn)
	inRepo := filepath.Join(c.repo, "data")
	os.MkdirAll(inRepo, 0o700)
	l2 := filepath.Join(c.home, "data-link")
	os.Symlink(inRepo, l2)
	c.env["MUSE_PLUGIN_DATA_DIR"] = l2
	c.start()
	if _, err := os.Stat(filepath.Join(inRepo, sessionsName)); err == nil {
		t.Error("a data folder linked into the repository was used")
	}
}

// TestMuseHookRepoState: a state folder or a settings file the repository holds is not used (the configuration's "~"
// resolved from a HOME the host pointed into the repository; a record naming a settings file there).
func TestMuseHookRepoState(t *testing.T) {
	b := newMuseBed(t, optInOn)
	sum := sha256.Sum256([]byte(engine))
	c := map[string]any{"schema": "rhobound-mcp.config/v1",
		"engine":    map[string]string{"path": filepath.Join(b.home, "bin", "rhobound"), "sha256": hex.EncodeToString(sum[:])},
		"state_dir": filepath.Join(b.repo, ".rb-state"), "captures": map[string]string{"loop": filepath.Join(b.home, "captures", "loop")},
		"policies": map[string]string{"default": filepath.Join(b.home, "policy.json")}}
	j, _ := json.Marshal(c)
	write(t, b.cfgPath, string(j), 0o600)
	b.start()
	if b.armed() {
		t.Error("armed with a state folder inside the repository")
	}
	d := newMuseBed(t, optInOn)
	inRepo := filepath.Join(d.repo, ".muse", "settings.json")
	write(t, inRepo, string(mustRead(t, d.settings)), 0o600)
	os.Remove(d.settings)
	rec, _ := json.Marshal(museRecord{Schema: museRecordSchema, Settings: inRepo})
	write(t, filepath.Join(d.home, ".local", "state", "rhobound-mcp", "muse-registration.json"), string(rec), 0o600)
	d.start()
	if d.armed() {
		t.Error("armed through a record naming a settings file inside the repository")
	}
}

// TestMuseHookGroupData: a data folder made under a umask of 002 (group-writable) is accepted; the sessions folder
// in it is private.
func TestMuseHookGroupData(t *testing.T) {
	b := newMuseBed(t, optInOn)
	os.MkdirAll(b.data, 0o700)
	os.Chmod(b.data, 0o775)
	b.start()
	if !b.armed() {
		t.Error("a group-writable data folder was refused")
	}
	if fi, _ := os.Stat(filepath.Join(b.data, sessionsName)); fi.Mode().Perm() != 0o700 {
		t.Errorf("the sessions folder: %v", fi.Mode())
	}
	c := newMuseBed(t, optInOn)
	os.MkdirAll(c.data, 0o700)
	os.Chmod(c.data, 0o777)
	c.start()
	if c.armed() {
		t.Error("a data folder anyone can write was used")
	}
	if _, err := os.Stat(filepath.Join(c.data, sessionsName)); !os.IsNotExist(err) {
		t.Error("a sessions folder was made in a data folder that was then refused")
	}
}

// TestPreviewedSinceStreams: the row that counts is found after many lines of no use, without keeping them.
func TestPreviewedSinceStreams(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 200000; i++ {
		b.WriteString("{}\n")
	}
	b.WriteString(`{"time":"2026-11-02T12:00:00Z","change_hash":"h","verdict":"pass"}` + "\n")
	write(t, filepath.Join(root, "previews", "x.jsonl"), b.String(), 0o600)
	t0, _ := time.Parse(time.RFC3339, "2026-11-02T11:00:00Z")
	if ok, err := state.PreviewedSince(root, t0); !ok || err != nil {
		t.Errorf("%v %v", ok, err)
	}
	if ok, _ := state.PreviewedSince(root, t0.Add(2*time.Hour)); ok {
		t.Error("a row before t counted")
	}
}
