// SPDX-License-Identifier: Apache-2.0

// Package sanitize is the one rule every string from a trace passes before it reaches an agent
// (docs/mcp/ARCHITECTURE.md 5.9). Service names, peer names and routes come from traces, and some of them are set by
// anonymous end users (a request path, a peer address), so a view never prints one as it was read:
//
//   - a name of at most 128 characters from [A-Za-z0-9._:/@-] is kept; anything else becomes svc_ and the first 8 hex
//     digits of its sha256, marked untrusted (contract.SafeName, the rule check's findings use too), and so does a
//     name in which ext: starts a word anywhere but at its start (a route such as /status/ext:x): ext: stands only for
//     the model's node of a peer;
//   - an AWS resource name (arn:partition:service:region:account:resource) is shown as its last element, untrusted
//     (for an ECS task, its id), unless that holds an account (the ARN's own account field anywhere, or 12 digits in
//     a row, as written or with its "-" taken out), and any other name that holds arn: (after a character that is
//     not a letter or a digit, or before a partition: contract.NamesARN) is hashed: an ARN names an account. The rule
//     sees what it is given: an ECS task's id that the reader kept is a name like any other;
//   - a peer the capture does not know as a service (the model's ext:<peer> nodes) becomes external_ and the first 8
//     hex digits of the peer's sha256, so that an address never appears, even one the name rule would keep;
//   - a route, only from http.route or rpc.method, may be an upper-case method, one space and a template from
//     [A-Za-z0-9._:/@{}*-] (POST /orders/{id}), in which ext: starts no word and that holds no arn:; anything else is
//     hashed as a name is.
//
// Raw url.path, http.target, http.url and server.address values are never read by the views at all.
package sanitize

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
)

// ExternalPrefix is the prefix of the model's nodes for peers that are not services of the capture (ext:<peer>).
const ExternalPrefix = "ext:"

// Name returns the name as a view prints it, and whether it was replaced because it broke the rule. It is
// contract.SafeName: one rule for findings and views.
func Name(s string) (out string, untrusted bool) {
	return contract.SafeName(s)
}

// Service returns a node name of the model as a view prints it. A peer node (ext:<peer>, a call to something the
// capture holds no spans of) becomes external_<hash8> of the peer, so a peer address never reaches a view; untrusted
// is false for it, since the replacement is the rule, not a defect. Any other name goes through Name.
func Service(name string) (out string, untrusted bool) {
	if peer, ok := strings.CutPrefix(name, ExternalPrefix); ok {
		return External(peer), false
	}
	return Name(name)
}

// Subject returns a finding's subject as a view, and a row of the gate, prints it: none ("") for no subject and for a
// subject that starts with /, since a path from the root names a folder of the machine that made the record, which
// the views never show (ARCHITECTURE.md 5.9), and the subjects of this tool's findings are names; any other through
// Service.
func Subject(s string) (out string, untrusted bool) {
	if s == "" || strings.HasPrefix(s, "/") {
		return "", false
	}
	return Service(s)
}

// Shown is how a view shows one node name: the name, and whether it was replaced.
type Shown struct {
	Name      string
	Untrusted bool
}

// Services returns how the views of one fit show its node names, by name: Service's rule, made one to one. A name the
// rule shows as an AWS resource name's last element (a Lambda function's ARN, ...:function:c) that another node is
// shown by too (a service c, or a second ARN with that last element) is shown as svc_<hash8> of the whole name
// instead, still untrusted. So no two nodes of a fit share a shown name, but by a collision of hashes or a node the
// traces named as another is shown (a service named svc_<hash8> like another's hash, which is shown as written beside
// it), and a caller can name a node by what a view shows (Resolve).
func Services(names []string) map[string]Shown {
	out := make(map[string]Shown, len(names))
	count := map[string]int{}
	for _, n := range names {
		if _, seen := out[n]; seen {
			continue
		}
		s, u := Service(n)
		out[n] = Shown{s, u}
		count[s]++
	}
	for n, sh := range out {
		if last, ok := contract.ARNLast(n); ok && sh.Name == last && count[last] > 1 {
			out[n] = Shown{"svc_" + hash8(n), true}
		}
	}
	return out
}

// Resolve returns the node a caller means by name, among nodes the views show as shown says (Services): the one node
// shown by that name, else the node of that name as the traces wrote it (one the views do not show, which the CLI is
// given). When the views show two or more nodes by that name, the one whose name as the traces wrote it is that name
// is meant, if one is (a service named like another's hash: the others are named by their names as written);
// ambiguous is true when none is (a collision of hashes). ok is false when no node is meant.
func Resolve(shown map[string]Shown, name string) (node string, ok, ambiguous bool) {
	var found []string
	for n, sh := range shown {
		if sh.Name == name {
			found = append(found, n)
		}
	}
	switch len(found) {
	case 1:
		return found[0], true, false
	case 0:
		_, ok := shown[name]
		return name, ok, false
	}
	if slices.Contains(found, name) {
		return name, true, false
	}
	return "", false, true
}

// External returns the printed form of a peer that is not a known service: external_ and the first 8 hex digits of
// the peer's sha256.
func External(peer string) string {
	return "external_" + hash8(peer)
}

// IsModelNode reports whether a node name is one of the zero-time nodes the inference adds for the call structure
// (stage:S:n, shape:S:j), which are not services and are never listed in a view.
func IsModelNode(name string) bool {
	return strings.HasPrefix(name, "stage:") || strings.HasPrefix(name, "shape:")
}

var routeRE = regexp.MustCompile(`^([A-Z]{3,10} )?[A-Za-z0-9._:/@{}*-]{1,128}$`)

// Route returns a route template as a view prints it (defs.v1 Route): an optional upper-case method and one space, then
// at most 128 characters from [A-Za-z0-9._:/@{}*-], in which ext: starts no word and that holds no arn:
// (contract.NamesARN). Anything else becomes svc_<hash8>, untrusted, as a name does. Callers pass only http.route or
// rpc.method values, never a raw path.
func Route(s string) (out string, untrusted bool) {
	if routeRE.MatchString(s) && !strings.HasPrefix(s, ExternalPrefix) && !contract.NodeInside(s) &&
		!contract.NamesARN(s) {
		return s, false
	}
	return "svc_" + hash8(s), true
}

func hash8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}
