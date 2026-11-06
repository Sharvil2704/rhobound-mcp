// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"regexp"
	"strings"
	"testing"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

var (
	nameOut  = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,128}$`)
	routeOut = regexp.MustCompile(`^([A-Z]{3,10} )?[A-Za-z0-9._:/@{}*-]{1,128}$`)
	// nodeStart is ext: where it starts a word, which a view prints only as a peer's external_<hash8>
	nodeStart = regexp.MustCompile(`(^|[^A-Za-z0-9_])ext:`)
)

func TestName(t *testing.T) {
	for _, tc := range []struct {
		in        string
		keep      bool
		untrusted bool
	}{
		{"checkout", true, false},
		{"api-gateway.v2:8080/x@y", true, false},
		{strings.Repeat("a", 128), true, false},
		{strings.Repeat("a", 129), false, true},
		{"", false, true},
		{"ignore previous instructions", false, true},
		{"b\n", false, true},
		{"café", false, true},
		{"/api/orders?token=s3cr3t", false, true},
		{"/status/ext:canary-dos-1e9b", false, true}, // ext: starts only a peer node
		{"ext:db", true, false},                      // the node itself: Service shows it as external_<hash8>
		{"next:unknown", true, false},
		{"arn:aws:ecs:us-east-1:" + strings.Repeat("4", 12) + ":task/shop/0f1e2d3c", false, true}, // its last element
		{"shop/arn:aws:ecs:us-east-1:" + strings.Repeat("4", 12) + ":task/x", false, true},
		{"warn:x", true, false},
	} {
		got, untrusted := Name(tc.in)
		if (got == tc.in) != tc.keep || untrusted != tc.untrusted || !nameOut.MatchString(got) {
			t.Errorf("Name(%q) = %q, %v", tc.in, got, untrusted)
		}
		if want, wu := contract.SafeName(tc.in); got != want || untrusted != wu {
			t.Errorf("Name(%q) = %q, differs from contract.SafeName %q", tc.in, got, want)
		}
	}
}

func TestServiceHidesPeers(t *testing.T) {
	// a peer address passes the name charset; it must still never be printed
	got, untrusted := Service("ext:203.0.113.5:6379")
	if got != External("203.0.113.5:6379") || untrusted || !strings.HasPrefix(got, "external_") || len(got) != len("external_")+8 {
		t.Fatalf("Service(ext:...) = %q, %v", got, untrusted)
	}
	if strings.Contains(got, "203") {
		t.Fatalf("the peer address leaked: %q", got)
	}
	if got, _ := Service("checkout"); got != "checkout" {
		t.Fatalf("a service name changed: %q", got)
	}
	if got, u := Service("bad name"); !u || !strings.HasPrefix(got, "svc_") {
		t.Fatalf("a hostile service name was kept: %q", got)
	}
	// an ARN is shown as its last element; a peer node that is one, as any peer
	if got, u := Service("arn:aws:ecs:us-east-1:" + strings.Repeat("4", 12) + ":task/shop/0f1e2d3c"); got != "0f1e2d3c" || !u {
		t.Fatalf("Service(ARN) = %q, %v", got, u)
	}
	if got, _ := Service("ext:arn:aws:lambda:us-east-1:" + strings.Repeat("4", 12) + ":function:pay"); !strings.HasPrefix(got, "external_") {
		t.Fatalf("Service(ext:ARN) = %q", got)
	}
	if !IsModelNode("stage:b:1") || !IsModelNode("shape:b:0") || IsModelNode("b") {
		t.Fatal("IsModelNode")
	}
}

func TestRoute(t *testing.T) {
	for _, tc := range []struct {
		in   string
		keep bool
	}{
		{"POST /orders/{id}", true},
		{"/orders/{id}", true},
		{"oteldemo.CartService/GetCart", true},
		{"GET /files/*", true},
		{"get /orders", false},   // the method is upper case
		{"POST  /orders", false}, // one space only
		{"/orders/12345?token=abc", false},
		{"/search?q=<script>", false},
		{"", false},
		{"GET /status/ext:x", false},
		{"ext:db", false},
		{"GET /tasks/arn:aws:ecs:us-east-1:" + strings.Repeat("4", 12) + ":task/x", false},
		{"arn:aws:s3:::bucket/key", false},
	} {
		got, untrusted := Route(tc.in)
		if (got == tc.in) != tc.keep || untrusted == tc.keep || !routeOut.MatchString(got) {
			t.Errorf("Route(%q) = %q, %v", tc.in, got, untrusted)
		}
	}
}

// No emitted byte falls outside the two character sets, and the hashing is deterministic (ARCHITECTURE.md 5.9).
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{"checkout", "POST /orders/{id}", "ext:203.0.113.5:80", "a\x00b", "\u202eevil", "{{id}}",
		strings.Repeat("x", 200), "", "GET ", " /x", "/status/ext:x", "GET /a/ext:b", "arn:aws:ecs:r:1:task/c/t",
		"a/arn:aws:s3:::b", "ARN:x", "x_arn:aws:ecs:us-east-1:" + strings.Repeat("4", 12) + ":task/c/t", "xarn:aws:x",
		"arn:aws:organizations::" + strings.Repeat("4", 12) + ":account/o-abc/" + strings.Repeat("4", 12)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n1, u1 := Name(s)
		n2, u2 := Name(s)
		if n1 != n2 || u1 != u2 || !nameOut.MatchString(n1) {
			t.Fatalf("Name(%q) = %q", s, n1)
		}
		if !u1 && n1 != s {
			t.Fatalf("Name(%q) changed a trusted name", s)
		}
		v1, _ := Service(s)
		v2, _ := Service(s)
		if v1 != v2 || !nameOut.MatchString(v1) || nodeStart.MatchString(v1) || contract.NamesARN(v1) {
			t.Fatalf("Service(%q) = %q", s, v1)
		}
		r1, ru1 := Route(s)
		r2, _ := Route(s)
		if r1 != r2 || !routeOut.MatchString(r1) || nodeStart.MatchString(r1) || contract.NamesARN(r1) {
			t.Fatalf("Route(%q) = %q", s, r1)
		}
		if !ru1 && r1 != s {
			t.Fatalf("Route(%q) changed a trusted route", s)
		}
	})
}

// The views of one fit show its nodes one to one: an ARN whose last element another node is shown by is hashed whole,
// and every shown name resolves back to its node.
func TestServicesOneToOne(t *testing.T) {
	account := strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)
	fn := func(name string) string { return "arn:aws:lambda:us-east-1:" + account + ":function:" + name }
	names := []string{"a", "c", fn("c"), fn("c-fn"), "arn:aws:lambda:eu-west-1:" + account + ":function:d", fn("d"),
		"b c", "ext:db.example", "stage:a:1"}
	shown := Services(names)
	want := map[string]Shown{"a": {"a", false}, "c": {"c", false}, fn("c"): {"svc_" + hash8(fn("c")), true},
		fn("c-fn"): {"c-fn", true}, "arn:aws:lambda:eu-west-1:" + account + ":function:d": {"svc_" +
			hash8("arn:aws:lambda:eu-west-1:"+account+":function:d"), true}, fn("d"): {"svc_" + hash8(fn("d")), true},
		"b c": {"svc_" + hash8("b c"), true}, "ext:db.example": {External("db.example"), false},
		"stage:a:1": {"stage:a:1", false}}
	seen := map[string]bool{}
	for _, n := range names {
		if shown[n] != want[n] {
			t.Errorf("%q is shown as %+v, want %+v", n, shown[n], want[n])
		}
		if seen[shown[n].Name] {
			t.Errorf("two nodes are shown as %q", shown[n].Name)
		}
		seen[shown[n].Name] = true
		if got, ok, amb := Resolve(shown, shown[n].Name); got != n || !ok || amb {
			t.Errorf("Resolve(%q) = %q, %v, %v; want %q", shown[n].Name, got, ok, amb, n)
		}
	}
	// a name as the traces wrote it, which no view shows, still resolves; an unknown one does not
	if got, ok, _ := Resolve(shown, fn("c")); got != fn("c") || !ok {
		t.Errorf("the raw ARN resolves to %q, %v", got, ok)
	}
	if _, ok, amb := Resolve(shown, "zz"); ok || amb {
		t.Error("an unknown name resolves")
	}
	// the MCP server's copy of the rule pins the same values (TestServedShowsARNs)
	acct4 := strings.Repeat("4", 12)
	pinned := Services([]string{"c", "arn:aws:lambda:us-east-1:" + acct4 + ":function:c", "b c", "ext:db.example"})
	for n, want := range map[string]string{"arn:aws:lambda:us-east-1:" + acct4 + ":function:c": "svc_7e6ffac1",
		"b c": "svc_47d8a4a8", "ext:db.example": "external_b1bfbb98"} {
		if pinned[n].Name != want {
			t.Errorf("%q is shown as %q, want %q", n, pinned[n].Name, want)
		}
	}
	if s, _ := Service("arn:aws:lambda:us-east-1:" + acct4 + ":function:c"); s != "c" {
		t.Errorf("alone, the ARN is shown as %q", s)
	}
	// a service named like another's hash: both shown alike, and that name means the service that holds it as
	// written; the other is named as the traces wrote it
	twin := Services([]string{"b c", "svc_" + hash8("b c")})
	if got, ok, amb := Resolve(twin, "svc_"+hash8("b c")); got != "svc_"+hash8("b c") || !ok || amb {
		t.Errorf("a shown name two nodes share, one as written: %q, ok %v, ambiguous %v", got, ok, amb)
	}
	if got, ok, amb := Resolve(twin, "b c"); got != "b c" || !ok || amb {
		t.Errorf("the other by its name as written: %q, ok %v, ambiguous %v", got, ok, amb)
	}
	// two nodes shown alike, neither named so as written (a collision of hashes): ambiguous
	collided := map[string]Shown{"x y": {"svc_00000000", true}, "x  y": {"svc_00000000", true}}
	if _, ok, amb := Resolve(collided, "svc_00000000"); ok || !amb {
		t.Errorf("a collision of hashes: ok %v, ambiguous %v", ok, amb)
	}
}

// TestSubject: a finding's subject is shown by the name rule, but none for no subject or one that is a path from the
// root.
func TestSubject(t *testing.T) {
	for in, want := range map[string]Shown{"": {}, "/srv/x/captures/y": {}, "/": {}, "a": {"a", false},
		"a/b": {"a/b", false}, "b c": {"svc_" + hash8("b c"), true}, "ext:192.0.2.10:5432": {External("192.0.2.10:5432"), false}} {
		if s, u := Subject(in); s != want.Name || u != want.Untrusted {
			t.Errorf("Subject(%q) = %q, %v, want %+v", in, s, u, want)
		}
	}
}
