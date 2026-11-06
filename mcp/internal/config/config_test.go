// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const sha = "fdbca5174cd1e1bb73e5064d776e9240f1877b8753f0ff87e06224ae36948889"

func good() string {
	return `{
  "schema": "rhobound-mcp.config/v1",
  "engine": {"path": "/usr/local/bin/rhobound", "sha256": "` + sha + `"},
  "state_dir": "state",
  "envs": {"staging": {"store": "dir", "root": "traces/staging"},
           "prod": {"store": "s3", "prefix": "env=prod/", "bucket": "acme-traces", "region": "eu-west-1"}},
  "captures": {"demo": "captures/demo", "loop": "/data/loop", "ab/old": "ab/old"},
  "policies": {"default": "policy.json"}
}`
}

func TestParseResolvesLocations(t *testing.T) {
	c, err := Parse([]byte(good()), "/work/cfg")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		c.StateDir:             "/work/cfg/state",
		c.Envs["staging"].Root: "/work/cfg/traces/staging",
		c.Captures["demo"]:     "/work/cfg/captures/demo",
		c.Captures["loop"]:     "/data/loop",
		c.Captures["ab/old"]:   "/work/cfg/ab/old",
		c.Policies["default"]:  "/work/cfg/policy.json",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if c.Profile != ProfileDefault {
		t.Errorf("profile %q, want the default", c.Profile)
	}
	if p, err := c.Capture("ab/old"); err != nil || p != "/work/cfg/ab/old" {
		t.Errorf("Capture(ab/old) = %q, %v", p, err)
	}
}

func TestHomeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c, err := Parse([]byte(strings.Replace(good(), `"state_dir": "state"`, `"state_dir": "~/.local/state/rhobound-mcp"`, 1)), "/work/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local/state/rhobound-mcp"); c.StateDir != want {
		t.Errorf("state_dir %q, want %q", c.StateDir, want)
	}
}

// TestNamesNeverTraverse: a tool argument is looked up as a whole key; traversal is refused even when its first
// segment is a configured name.
func TestNamesNeverTraverse(t *testing.T) {
	c, err := Parse([]byte(good()), "/work/cfg")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../../etc/passwd", "demo/../loop", "demo/..", "./demo", "/demo", "demo/",
		"demo//x", "..", ".", "", "demo name", "demo\x00", strings.Repeat("a", 129)} {
		if p, err := c.Capture(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Capture(%q) = %q, %v; want ErrBadName", name, p, err)
		}
		if _, err := c.Policy(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Policy(%q): %v; want ErrBadName", name, err)
		}
	}
	for _, name := range []string{"demo/x", "other", "ab", "a.b"} {
		if _, err := c.Capture(name); !errors.Is(err, ErrUnknownName) {
			t.Errorf("Capture(%q): %v; want ErrUnknownName", name, err)
		}
	}
	if _, err := c.Env("Prod"); !errors.Is(err, ErrBadName) {
		t.Errorf("Env(Prod): %v; want ErrBadName", err)
	}
	if _, err := c.Env("dev"); !errors.Is(err, ErrUnknownName) {
		t.Errorf("Env(dev): %v; want ErrUnknownName", err)
	}
	if e, err := c.Env("prod"); err != nil || e.Prefix != "env=prod/" {
		t.Errorf("Env(prod) = %+v, %v", e, err)
	}
}

func TestParseRefuses(t *testing.T) {
	cases := map[string][2]string{
		"wrong schema":       {`"rhobound-mcp.config/v1"`, `"rhobound-mcp.config/v2"`},
		"unknown key":        {`"state_dir": "state",`, `"state_dir": "state", "bucket": "x",`},
		"relative engine":    {`"/usr/local/bin/rhobound"`, `"bin/rhobound"`},
		"short sha256":       {sha, sha[:63]},
		"upper-case sha256":  {sha, strings.ToUpper(sha)},
		"traversing capture": {`"demo": "captures/demo"`, `"../demo": "captures/demo"`},
		"dot capture":        {`"demo": "captures/demo"`, `"a/./b": "captures/demo"`},
		"traversing policy":  {`"default": "policy.json"`, `"x/../y": "policy.json"`},
		"bad env name":       {`"staging": {`, `"Staging": {`},
		"prefix without /":   {`"env=prod/"`, `"env=prod"`},
		"dir store, prefix":  {`"root": "traces/staging"}`, `"root": "traces/staging", "prefix": "x/"}`},
		"bad profile":        {`"policies"`, `"profile": "admin", "policies"`},
		"no state_dir":       {`"state_dir": "state",`, ``},
	}
	for name, r := range cases {
		doc := strings.Replace(good(), r[0], r[1], 1)
		if doc == good() {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Parse([]byte(doc), "/work/cfg"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(good()), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "captures/demo"); c.Captures["demo"] != want {
		t.Errorf("demo = %q, want %q", c.Captures["demo"], want)
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
}

// fit_ttl_h is optional: absent reads as 0 (the server then takes DefaultFitTTLH, a week); 1 to 8760 hours are taken as
// given; anything else is refused by the schema or the decoder.
func TestFitTTLH(t *testing.T) {
	with := func(v string) string {
		return strings.Replace(good(), `"state_dir": "state",`, `"state_dir": "state", "fit_ttl_h": `+v+`,`, 1)
	}
	if c, err := Parse([]byte(good()), "/work/cfg"); err != nil || c.FitTTLH != 0 || DefaultFitTTLH != 168 {
		t.Errorf("absent: %+v %v", c, err)
	}
	for v, want := range map[string]int{"1": 1, "24": 24, "8760": 8760} {
		if c, err := Parse([]byte(with(v)), "/work/cfg"); err != nil || c.FitTTLH != want {
			t.Errorf("fit_ttl_h %s: %v", v, err)
		}
	}
	for _, v := range []string{"0", "-1", "8761", "1.5", `"24"`, "null"} {
		if _, err := Parse([]byte(with(v)), "/work/cfg"); err == nil {
			t.Errorf("fit_ttl_h %s was accepted", v)
		}
	}
}

// TestValidNameIsCheckName: the configuration takes as capture and policy names exactly the names the engine takes as
// a fit's --name (cmd/rhobound's TestFitName pins the same lists), and refuses a configuration that holds another,
// naming the key.
func TestValidNameIsCheckName(t *testing.T) {
	account := strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	for _, good := range []string{"chain-prod", "team/chain-1", "ext:x", "a.b@c:d", "warn:x", "x1arn:x"} {
		if !ValidName(good) {
			t.Errorf("%q is refused", good)
		}
	}
	for _, bad := range []string{"", "/srv/captures/prod", "a b", strings.Repeat("x", 129), "prod\x1b[2J", "team/ext:db",
		"arn:aws:s3:::traces/prod", "team/arn:aws:x", "arn:probe", "team/arn:x", "team/ext:x",
		"ext:arn:aws:lambda:us-east-1:" + account + ":function:pay", "ext:x/arn:aws:y", "a//b", "a/./b", "a/../b",
		"x_arn:aws:y", "xarn:aws:y"} {
		if ValidName(bad) {
			t.Errorf("%q is taken", bad)
		}
	}
	for _, name := range []string{"arn:x", "team/arn:x", "team/ext:x"} {
		cfg := fmt.Sprintf(`{"schema":%q,"engine":{"path":"/opt/rhobound","sha256":%q},"state_dir":"state",`+
			`"captures":{%q:"captures/x"}}`, Schema, strings.Repeat("a", 64), name)
		if _, err := Parse([]byte(cfg), t.TempDir()); err == nil || !strings.Contains(err.Error(), strconv.Quote(name)) {
			t.Errorf("captures %q: %v", name, err)
		}
	}
}

// TestTildeWithoutHome: "~/" resolves from the user database when HOME is unset (an agent may start the server with
// a cleared environment).
func TestTildeWithoutHome(t *testing.T) {
	u, err := user.Current()
	if err != nil || !filepath.IsAbs(u.HomeDir) {
		t.Skip("no user database entry")
	}
	for _, home := range []string{"", "rel/home"} {
		t.Setenv("HOME", home)
		c, err := Parse([]byte(`{"schema":"rhobound-mcp.config/v1","engine":{"path":"/bin/true","sha256":"`+
			strings.Repeat("0", 64)+`"},"state_dir":"~/.local/state/rhobound-mcp"}`), "/")
		if err != nil || c.StateDir != filepath.Join(u.HomeDir, ".local", "state", "rhobound-mcp") {
			t.Errorf("HOME=%q: %v %+v", home, err, c)
		}
	}
}

// An s3 store names its bucket; its Region and endpoint have shapes; a dir store has none of them.
func TestEnvS3Fields(t *testing.T) {
	base := `{"schema":"rhobound-mcp.config/v1","engine":{"path":"/bin/true","sha256":"` + strings.Repeat("ab", 32) + `"},"state_dir":"/tmp/s","envs":{"e":%s}}`
	for env, wantErr := range map[string]string{
		`{"store":"s3","prefix":"p/","bucket":"traces","region":"us-east-1"}`:                "",
		`{"store":"s3","prefix":"p/","bucket":"traces","endpoint":"http://127.0.0.1:9000"}`:  "",
		`{"store":"s3","prefix":"p/","bucket":"traces","endpoint":"https://s3.example.com"}`: "",
		`{"store":"s3","prefix":"p/"}`:                                                              "bucket",
		`{"store":"s3","prefix":"p/","bucket":"Traces"}`:                                            "bucket",
		`{"store":"s3","prefix":"p/","bucket":"traces","region":"US East"}`:                         "region",
		`{"store":"s3","prefix":"p/","bucket":"traces","endpoint":"http://10.0.0.5:9000"}`:          "endpoint",
		`{"store":"s3","prefix":"p/","bucket":"traces","endpoint":"https://s3.example.com/bucket"}`: "endpoint",
		`{"store":"dir","root":"/data","bucket":"traces"}`:                                          "dir store",
	} {
		_, err := Parse([]byte(fmt.Sprintf(base, env)), "/tmp")
		if wantErr == "" && err != nil {
			t.Errorf("%s: %v", env, err)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Errorf("%s: %v, want an error mentioning %q", env, err, wantErr)
		}
	}
}
