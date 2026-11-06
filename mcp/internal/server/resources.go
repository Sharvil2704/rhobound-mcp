// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/config"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/registry"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/runner"
	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/state"
)

// Resource URIs (ARCHITECTURE.md 5.6). Those ending in "/" take a policy name or a handle; a handle resolves for the
// caller only, as in a tool.
const (
	scopeURI        = "rhobound://scope"
	dataContractURI = "rhobound://data-contract"
	policyURI       = "rhobound://policy/"
	fitURI          = "rhobound://fit/"
	predictionURI   = "rhobound://prediction/"
	packetURI       = "rhobound://judge-packet/"
)

// Cache hints of the resources (ARCHITECTURE.md 5.6). A handle's resource never changes while the handle lives (its
// documents are content-addressed), so any time-to-live up to its expiry is right; the store does not report the
// expiry, and handleTTL is a short lower bound of it.
const (
	staticTTL  = 24 * time.Hour
	policyTTL  = 5 * time.Minute
	handleTTL  = 5 * time.Minute
	listTTL    = time.Hour
	jsonMIME   = "application/json"
	explainMax = "--max-chars=8000"
)

// addResources registers the resources and the resource templates.
func (s *Server) addResources() {
	s.mcp.AddResource(&mcp.Resource{URI: scopeURI, Name: "scope", MIMEType: jsonMIME,
		Description: "What rhobound predicts and what it does not: the full scope text and the known failures."},
		s.readStatic("scope"))
	s.mcp.AddResource(&mcp.Resource{URI: dataContractURI, Name: "data-contract", MIMEType: jsonMIME,
		Description: "Every finding and refusal code of the one registry (rhobound.data-contract/v1), each with its " +
			"level, trigger, condition, fix, status and the names of its numbers. The fixes are the CLI's: a refusal " +
			"from this server carries the server's own fix for some codes."},
		s.readDataContract)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: policyURI + "{name}", Name: "policy", MIMEType: jsonMIME,
		Description: "A named policy from the server's configuration."}, s.readPolicy)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: fitURI + "{fit_id}", Name: "fit", MIMEType: jsonMIME,
		Description: "A fit's full summary: every service, finding and assumption (the context view at detail full)."},
		s.readFit)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: predictionURI + "{prediction_id}", Name: "prediction",
		MIMEType: jsonMIME, Description: "A prediction's report v2 and its change set, which name every node as the views " +
			"do: a peer that is not a service as external_<hash8>, a service whose name the name rule does not keep as " +
			"svc_<hash8>. The report's image names are left out, and so are its warnings, counted in warnings_omitted."},
		s.readPrediction)
	s.mcp.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: packetURI + "{prediction_id}", Name: "judge-packet",
		MIMEType: jsonMIME, Description: "A prediction's judge packet (rhobound-mcp.judge-packet/v1)."}, s.readPacket)
}

// resourceCall is the caller of a resource read, or a protocol error when the request carries no identity.
func (s *Server) resourceCall(ctx context.Context) (state.Principal, error) {
	id, ok := s.opt.Identity(ctx)
	if !ok {
		return "", internalError("no caller identity")
	}
	return state.Principal(id.Principal), nil
}

// resourceResult is one JSON document as a resource's contents, with its cache hint.
func resourceResult(uri string, b []byte, ttl time.Duration, scope string) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{
		Cacheable: mcp.Cacheable{TTLMs: int(ttl / time.Millisecond), CacheScope: scope},
		Contents:  []*mcp.ResourceContents{{URI: uri, MIMEType: jsonMIME, Text: string(b)}},
	}
}

// handleOf returns the handle at the end of a resource URI, when it is a well-formed handle of kind.
func handleOf(uri, prefix, kind string) (string, bool) {
	h, ok := strings.CutPrefix(uri, prefix)
	return h, ok && state.ValidHandle(h) && state.HandleKind(h) == kind
}

// readDataContract serves the whole registry of codes, canonical: the document docs/mcp/data-contract.json is, as this
// build embeds it (package registry). Explain's section data_contract lists a subset of each code's fields, cut to its
// max_chars, so the resource is not that section.
func (s *Server) readDataContract(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if _, err := s.resourceCall(ctx); err != nil {
		return nil, err
	}
	b, err := Canonical(registry.Published())
	if err != nil {
		return nil, internalError("the embedded registry is not JSON")
	}
	return resourceResult(req.Params.URI, b, staticTTL, "public"), nil
}

// readStatic serves an explain section that needs no target: rhobound explain --section S --max-chars 8000 --json,
// computed once per process (staticView).
func (s *Server) readStatic(section string) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if _, err := s.resourceCall(ctx); err != nil {
			return nil, err
		}
		view, err := s.staticView(ctx, []string{"--section=" + section, explainMax, "--json"})
		if err != nil {
			return nil, protocolRefusal(err)
		}
		return resourceResult(req.Params.URI, view, staticTTL, "public"), nil
	}
}

// staticView is an explain section that needs no target (scope, data_contract) with the given arguments. Its text is
// fixed for the engine binary, which the configuration pins by sha256. At the resources' size (explainMax, at which
// rhobound://scope serves its section) the server keeps it for the life of the process, so each is computed once per
// process, however many callers read it at once and whatever they ask before; at another max_chars it remembers the
// last maxStatic texts, which explain's own bucket bounds (staticCache). The computation is not cut short when the
// request that started it ends: others may be waiting for it, and the engine call has its own timeout.
func (s *Server) staticView(ctx context.Context, args []string) ([]byte, error) {
	return s.static.get(ctx, strings.Join(args, " "), slices.Contains(args, explainMax), func() ([]byte, error) {
		return s.run(context.WithoutCancel(ctx), "explain", args, nil, 0)
	})
}

// readPolicy serves a configured policy, canonical.
func (s *Server) readPolicy(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	if _, err := s.resourceCall(ctx); err != nil {
		return nil, err
	}
	name, _ := strings.CutPrefix(req.Params.URI, policyURI)
	if !config.ValidName(name) {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	if _, err := s.opt.Config.Policy(name); err != nil {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	b, _, err := s.policy(name)
	if err != nil {
		return nil, err
	}
	c, err := Canonical(b)
	if err != nil {
		return nil, internalError("the configured policy is not JSON")
	}
	return resourceResult(req.Params.URI, c, policyTTL, "private"), nil
}

// readFit serves a fit's full summary: rhobound context --fit F --detail=full --json.
func (s *Server) readFit(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	p, err := s.resourceCall(ctx)
	if err != nil {
		return nil, err
	}
	h, ok := handleOf(req.Params.URI, fitURI, state.KindFit)
	if !ok {
		return nil, mcp.ResourceNotFoundError(req.Params.URI)
	}
	d, err := s.resolveHandle(ctx, p, h, state.KindFit)
	if err != nil {
		return nil, protocolRefusal(err)
	}
	fit, err := s.artifact(ctx, d)
	if err != nil {
		return nil, err
	}
	// the full context view computes the leverage of every service, as the context tool does: the same bucket
	if ok, wait := s.opt.Limits.Allow(string(p), "context"); !ok {
		return nil, protocolRefusal(refuse(rateLimited(wait)))
	}
	view, err := s.run(ctx, "context", []string{"--fit", runner.File(fitFile), "--detail=full", "--json"},
		map[string][]byte{fitFile: fit}, 0)
	if err != nil {
		return nil, protocolRefusal(err)
	}
	return resourceResult(req.Params.URI, view, handleTTL, "private"), nil
}

// readPrediction serves a prediction's report v2, computed once from its stored fit and change set, and the change
// set: {"changes": ..., "report": ..., "warnings_omitted": N}. The report is the CLI's full artifact, which names
// services as the traces wrote them; it leaves through served, which names every node (services, peers, the model's
// nodes) and the capture as the views show them (ARCHITECTURE.md 5.9) and leaves out its image names and its
// warnings, counted in warnings_omitted; the report stored, and the engine's bytes, are not changed. The notes of the
// change set's items are the caller's own words, echoed as written: served does not read them.
func (s *Server) readPrediction(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	pr, err := s.resourcePrediction(ctx, req.Params.URI, predictionURI)
	if err != nil {
		return nil, err
	}
	rep, err := s.report(ctx, pr)
	if err != nil {
		return nil, protocolRefusal(err)
	}
	ch, err := decode(pr.changes)
	if err != nil {
		return nil, internalError("the stored change set does not parse")
	}
	r, err := decodeObject(rep)
	if err != nil {
		return nil, internalError("the engine's report does not parse")
	}
	doc := map[string]any{"changes": ch, "report": r}
	restore := takeNotes(ch)
	omitted, _ := served(doc)
	restore()
	doc["warnings_omitted"] = omitted
	b, err := encode(doc)
	if err != nil {
		return nil, internalError("cannot encode the prediction")
	}
	return resourceResult(req.Params.URI, b, handleTTL, "private"), nil
}

// served is the one function every document the server serves whole passes on its way out (ARCHITECTURE.md 5.9):
// the report and change set of rhobound://prediction, and the judge packet of rhobound://judge-packet and of
// review_latency_impact. The views (the tools' results, and rhobound://fit, which is the context view) are not served
// whole: they are the CLI's projections, which apply these rules themselves, and the server passes them on as the CLI
// printed them, byte for byte their twins'. served rewrites doc in place, wherever in it these are:
//
//   - a model node of a peer that no service of the capture answers, ext:<peer> (the name of a services[] row: every
//     row marked external has one), and ext:unknown, becomes external_<first 8 hex of the peer's sha256>, as the CLI's
//     views print it (sanitize.Service). It is replaced in every string, object keys included, whole or in part where
//     ext: starts a word and the name ends (nodeEnds), the longest name first, so that a peer's name is never
//     rewritten inside a longer one's, and a longer name is never half hidden;
//   - any other string or key in which ext: starts a word, a peer node no row names or trace text such as a route, is
//     replaced whole by svc_<first 8 hex of its sha256>, as the name rule hashes a name it does not keep
//     (contract.SafeName): nothing of it is served, and no string can stop a document being served;
//   - every other row's name that the views show by another name (a name the name rule does not keep, such as one
//     with a space, text outside ASCII or over 128 characters, as svc_<hash8> of it; an ARN as the rule shows one; a
//     row named by an ARN whose last element another row is shown by as svc_<hash8> of the whole name, so that no two
//     rows share a name: viewName and oneToOne, the views' rule) becomes that name wherever a string or key is that
//     name, so that the document names each node as the views do, and by a name a caller can give a tool;
//   - a call shape (call_shapes[].shape) is read word by word as the inference writes one, each word a row's name,
//     and each word is shown as the views show its row; a shape that does not read so in exactly one way is hashed
//     whole (shape), so that a name that runs across the words of a shape never takes another word's name with it;
//   - the service of a merged nested span (nested_spans_merged[].service) that is not a row, a service of the capture
//     that emits no SERVER span and so is no node of the model, becomes the name the views would show it by alone
//     (viewName): the record's checks hold every node it names to be a row, but not these (fit.Record.Check); and so
//     does any other node name outside the rows (busiest_service, a biased construct's service, an asynchronous
//     call's caller and callee, a varying call's callee) that names no row, as the engine never writes one
//     (notNodes);
//   - then an AWS resource name (arn:partition:service:region:account:resource), which names an account, becomes its
//     last element unless that holds an account, and any other string or key that holds arn: is hashed whole, as the
//     name rule shows them (arnShown): a stored record is not trusted to be the current engine's, whose reader keeps
//     an ECS task's id;
//   - a key rewritten to one the object already has keeps its value under the name rule's hash of it (freeKey);
//   - each example of trace.peer_renamed, a raw server.address:port, becomes external_<hash8> of it (hidePeers);
//   - a finding's subject that starts with /, and an example that does (but trace.probe_like_traffic's, which are
//     routes), are left out, as the views leave them out (unrooted);
//   - a capture is shown as explain's fit summary shows it (captureName);
//   - image names (container.image.name and its tag) are left out, with every images key (today a row's): on a
//     registry such as ECR they name the account, the region and the repositories;
//   - warnings are left out: they are free text, and a lost-span warning prints a callee's server.address:port, which
//     no row of the report names, so the server cannot rewrite them safely.
//
// It returns how many warnings it left out, and whether it changed anything: a document it leaves alone is served as
// the engine printed it (a judge answer cites the sha256 of the packet's bytes).
func served(doc any) (omitted int, changed bool) {
	peers := peerNodes(doc)
	var walk func(v any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			slices.Sort(keys) // which of two keys that collide keeps its name does not depend on the map's order
			for _, k := range keys {
				e := x[k]
				switch k {
				case "warnings":
					if w, ok := e.([]any); ok {
						omitted += len(w)
					} else {
						omitted++
					}
					delete(x, k)
					changed = true
					continue
				case "images":
					delete(x, k)
					changed = true
					continue
				case "capture":
					if c, ok := e.(string); ok && captureName(c) != c {
						e, changed = captureName(c), true
					}
				case "findings":
					changed = hidePeers(e) || changed
					changed = unrooted(e) || changed
				case "shape":
					if sh, ok := e.(string); ok {
						if v, ch := peers.shape(sh); ch {
							e, changed = v, true
						}
					}
				case "nested_spans_merged":
					changed = peers.notRows(e) || changed
				case "busiest_service", "biased", "async_calls", "variable_calls":
					var ch bool
					e, ch = peers.notNodes(e, nodeKeys[k]...)
					changed = ch || changed
				}
				e = walk(e)
				if nk, ok := peers.show(k); ok {
					delete(x, k)
					k, changed = freeKey(x, k, nk), true
				}
				x[k] = e
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		case string:
			if s, ok := peers.show(x); ok {
				changed = true
				return s
			}
		}
		return v
	}
	walk(doc)
	return omitted, changed
}

// freeKey is the key served gives the key k of the object x, which it rewrote to nk: nk, unless x already has that key
// (another of its keys, or one rewritten to it), and then the name rule's hash of k, as residue is hashed (and of k
// with a count, should that be taken too). No value is dropped, and nothing of k is served.
func freeKey(x map[string]any, k, nk string) string {
	for n := 0; ; n++ {
		if _, taken := x[nk]; !taken {
			return nk
		}
		nk = untrusted(k + strings.Repeat("\x00", n))
	}
}

// takeNotes takes the note out of each item of a change set and returns the function that puts them back. A note is
// the caller's own words, never parsed, and the change set echoes it as written.
func takeNotes(changes any) (restore func()) {
	type held struct {
		item map[string]any
		note any
	}
	var notes []held
	set, _ := changes.(map[string]any)
	items, _ := set["items"].([]any)
	for _, it := range items {
		if item, ok := it.(map[string]any); ok {
			if n, ok := item["note"]; ok {
				notes = append(notes, held{item, n})
				delete(item, "note")
			}
		}
	}
	return func() {
		for _, h := range notes {
			h.item["note"] = h.note
		}
	}
}

// servedPacket is a judge packet as a resource or a prompt serves it: through served, and as the engine printed it
// when served changes nothing, which the CLI's packet (sanitized names, no capture, no warnings) never needs. A packet
// has no services rows, so a peer node in it other than ext:unknown is hashed whole with the string that names it.
func servedPacket(pk []byte) ([]byte, error) {
	v, err := decode(pk)
	if err != nil {
		return nil, internalError("the engine's judge packet does not parse")
	}
	if _, changed := served(v); !changed {
		return pk, nil
	}
	b, err := encode(v)
	if err != nil {
		return nil, internalError("cannot encode the judge packet")
	}
	return b, nil
}

// nodePrefix starts the name of a model node that stands for a peer no service of the capture answers (ext:<peer>,
// ext:unknown for a CLIENT span that names no peer); sanitize.ExternalPrefix in the CLI.
const nodePrefix = "ext:"

// peerAliases are the peer nodes of a document, the longest name first, each with the name the views show it by; and
// the other rows that the views show by a name other than the one the traces wrote, each with the name the views show
// it by (rows).
type peerAliases struct {
	names []string
	alias map[string]string
	rows  map[string]string
	all   map[string]bool // the name of every row
	// words are the words a call shape is made of, by their first byte: the name of every row, and "?"
	words map[byte][]string
}

// peerNodes collects the peer nodes a document names: the name of every services[] row that starts with ext: (every
// row marked external, and only those, in the CLI's report), and ext:unknown, which may have no row. It also collects
// every other row that the views show by another name than the traces wrote, with that name: the views' name rule
// (viewName: svc_<hash8> of a name it does not keep, an ARN's last element), made one to one as the views make it
// (oneToOne: svc_<hash8> of an ARN whose last element another row is shown by; sanitize.Services in the CLI).
func peerNodes(doc any) peerAliases {
	p := peerAliases{alias: map[string]string{nodePrefix + "unknown": external("unknown")}}
	var all []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			rows, _ := x["services"].([]any)
			for _, r := range rows {
				row, _ := r.(map[string]any)
				n, ok := row["name"].(string)
				if ok && strings.HasPrefix(n, nodePrefix) {
					p.alias[n] = external(strings.TrimPrefix(n, nodePrefix))
				}
				if ok {
					all = append(all, n)
				}
			}
			for _, e := range x {
				walk(e)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	ones := oneToOne(all)
	p.rows, p.all = map[string]string{}, map[string]bool{}
	for _, n := range all {
		p.all[n] = true
		if n == "" || strings.HasPrefix(n, nodePrefix) {
			continue // a peer node is hidden as one (hide); an empty name names nothing
		}
		v, ok := ones[n]
		if !ok {
			v = viewName(n)
		}
		if v != n {
			p.rows[n] = v
		}
	}
	for n := range p.alias {
		p.names = append(p.names, n)
	}
	slices.SortFunc(p.names, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	p.words = map[byte][]string{}
	for n := range p.all {
		if n != "" && n != "?" {
			p.words[n[0]] = append(p.words[n[0]], n)
		}
	}
	p.words['?'] = append(p.words['?'], "?")
	for _, l := range p.words {
		slices.Sort(l)
	}
	return p
}

// show is a string as served shows it: a row's name as the views show that row (rows), else its peer nodes hidden
// (hide), then an AWS resource name in what is left shown as the name rule shows one (arnShown). It reports whether it
// changed s.
func (p peerAliases) show(s string) (string, bool) {
	if a, ok := p.rows[s]; ok {
		return a, true
	}
	h, hid := p.hide(s)
	a, arn := arnShown(h)
	return a, hid || arn
}

// notRows shows the service of each merged nested span in list (nested_spans_merged) that names no row as the views
// would show it alone (viewName); a row's name is left for show. It reports whether it changed one.
func (p peerAliases) notRows(list any) bool {
	changed := false
	l, _ := list.([]any)
	for _, x := range l {
		m, _ := x.(map[string]any)
		if s, ok := m["service"].(string); ok && !p.all[s] && viewName(s) != s {
			m["service"], changed = viewName(s), true
		}
	}
	return changed
}

// notNodes shows each node name of v that names no row as the views would show it alone (viewName): v itself, a
// string (busiest_service), or, with keys, the value of each key in each item of the list v (a biased construct's
// service; an asynchronous call's caller and callee; a varying call's callee). A record's checks hold each of these to
// be a row (fit.Record.Check), as the engine names them; one that is not is not left to show, which passes on a name
// that is no row as written. A row's name is left for show, and so is a name in which ext: starts a word, which hide
// hides or hashes whole. It returns the new value and whether it changed one.
func (p peerAliases) notNodes(v any, keys ...string) (any, bool) {
	one := func(s string) (string, bool) {
		if p.all[s] || namesNode(s) || viewName(s) == s {
			return s, false
		}
		return viewName(s), true
	}
	if s, ok := v.(string); ok && len(keys) == 0 {
		n, ch := one(s)
		return n, ch
	}
	changed := false
	l, _ := v.([]any)
	for _, x := range l {
		m, _ := x.(map[string]any)
		for _, k := range keys {
			if s, ok := m[k].(string); ok {
				if n, ch := one(s); ch {
					m[k], changed = n, true
				}
			}
		}
	}
	return v, changed
}

// nodeKeys are the keys of a report that name a model node outside its rows, each with the keys of its items that do
// (none: the value is the name): notNodes shows each that names no row as the views would show it alone.
var nodeKeys = map[string][]string{"busiest_service": nil, "biased": {"service"}, "async_calls": {"caller", "callee"},
	"variable_calls": {"callee"}}

// shapeJoiners join the items of a call shape: stages, and calls at once.
var shapeJoiners = []string{" then ", " + "}

// shape is a call shape (call_shapes[].shape) with each node named as the views show it. The shape is read as the
// inference writes one (describeShape) and as the record's checks read it (fit's isShape): items joined by " then "
// (between stages) or " + " (between calls at once), each a word, a row's name or "?" (a callee the inference could
// not name), then " xN" (N calls at once, a whole number of at least 2) or nothing. Each word is shown as the views
// show its row (a peer node by its alias, another row by the name of rows, a name the rule keeps as written) and the
// joiners and counts as written. A shape that does not read so in exactly one way (a name that runs across the words
// of another reading, as a service named "w then ext:" does in "w then ext:<peer>") is hashed whole (untrusted):
// nothing of it is served. served then shows the result as any other string (show). It reports whether it changed s.
func (p peerAliases) shape(s string) (string, bool) {
	// The readings are counted from the start, never more than two: ways[i] is the number of ways s[:i] reads as items
	// each followed by a joiner (0, 1, or 2 for two or more), and from[i] the last item of one of them.
	type item struct{ at, word, end int } // the item's start, its word's end, and its end (after its count)
	ways := make([]uint8, len(s)+1)
	from := map[int]item{}
	ways[0] = 1
	total, last := 0, item{}
	for i := 0; i < len(s); i++ {
		if ways[i] == 0 {
			continue
		}
		for _, w := range p.words[s[i]] {
			if !strings.HasPrefix(s[i:], w) {
				continue
			}
			ends := []int{i + len(w)}
			if n := shapeCount(s, i+len(w)); n > 0 {
				ends = append(ends, i+len(w)+n)
			}
			for _, e := range ends {
				it := item{i, i + len(w), e}
				if e == len(s) {
					if total == 0 {
						last = it
					}
					total = min(total+int(ways[i]), 2)
					continue
				}
				for _, j := range shapeJoiners {
					if k := e + len(j); k < len(s) && strings.HasPrefix(s[e:], j) {
						if ways[k] == 0 {
							from[k] = it
						}
						ways[k] = min(ways[k]+ways[i], 2)
					}
				}
			}
		}
	}
	if total != 1 {
		return untrusted(s), true
	}
	// the one reading, from its last item back: each word as shown, then its count and the joiner after it as written
	var parts []string
	next := len(s)
	for it := last; ; it = from[it.at] {
		w := s[it.at:it.word]
		shown := w
		if a, ok := p.alias[w]; ok {
			shown = a
		} else if v, ok := p.rows[w]; ok {
			shown = v
		}
		parts = append(parts, shown+s[it.word:next])
		if it.at == 0 {
			break
		}
		next = it.at
	}
	slices.Reverse(parts)
	r := strings.Join(parts, "")
	return r, r != s
}

// shapeCount is the length of " xN" at i of s, N a whole number of at least 2 as %d writes one (no leading zero, at
// most 9 digits), or 0 (fit's countAt).
func shapeCount(s string, i int) int {
	rest, ok := strings.CutPrefix(s[i:], " x")
	if !ok {
		return 0
	}
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 || n > 9 || rest[0] == '0' || rest[:n] == "1" {
		return 0
	}
	return 2 + n
}

// hide replaces each peer node in s, where ext: starts a word (nodeAt) and the node's name ends (nodeEnds), by its
// alias, trying the longest name first. When ext: starts a word where no node's name does, s is hashed whole instead
// (untrusted). It reports whether it replaced anything.
func (p peerAliases) hide(s string) (string, bool) {
	if !strings.Contains(s, nodePrefix) {
		return s, false
	}
	var b strings.Builder
	hit := false
	for i := 0; i < len(s); {
		if nodeAt(s, i) {
			j := slices.IndexFunc(p.names, func(n string) bool { return strings.HasPrefix(s[i:], n) && nodeEnds(s, i+len(n)) })
			if j < 0 {
				return untrusted(s), true
			}
			b.WriteString(p.alias[p.names[j]])
			i += len(p.names[j])
			hit = true
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	if !hit {
		return s, false
	}
	return b.String(), true
}

// viewName is a node name as the CLI's views show it alone (sanitize.Service; this module cannot import it, and the
// tests pin the two to the same values): a peer node as external_<hash8>, an ARN as the name rule shows it (arnShown),
// a name of the name rule kept, anything else svc_<hash8>.
func viewName(n string) string {
	if peer, ok := strings.CutPrefix(n, nodePrefix); ok {
		return external(peer)
	}
	if a, ok := arnShown(n); ok {
		return a
	}
	if nameRE.MatchString(n) && !namesNode(n) {
		return n
	}
	return untrusted(n)
}

// oneToOne returns, of a fit's node names, those the views show by svc_<hash8> of the whole name although the name
// rule alone shows them by their ARN's last element: another node is shown by that element too (sanitize.Services).
func oneToOne(names []string) map[string]string {
	count := map[string]int{}
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			count[viewName(n)]++
		}
	}
	out := map[string]string{}
	for n := range seen {
		if last, isARN := arnShown(n); isARN && last != untrusted(n) && count[last] > 1 {
			out[n] = untrusted(n)
		}
	}
	return out
}

// nodeAt reports whether a peer node's prefix starts a word of s at i: ext: at the start of s, or after a character
// outside [A-Za-z0-9_] (so that "next:" is not one).
func nodeAt(s string, i int) bool {
	return strings.HasPrefix(s[i:], nodePrefix) && (i == 0 || !wordByte(s[i-1]))
}

// nodeEnds reports whether a node's name that ends at i of s ends there: at the end of s, or before an ASCII
// character the name rule does not allow (outside [A-Za-z0-9._:/@-]), so that the name is not the start of a longer
// one. A name that runs into a byte of a multi-byte character (é, U+202E) does not end there: the string is then
// hashed whole, as no row names that longer name.
func nodeEnds(s string, i int) bool {
	return i == len(s) || s[i] < utf8.RuneSelf && !nameRE.MatchString(s[i:i+1])
}

// namesNode reports whether s names a peer node: ext: starts a word of it (nodeAt).
func namesNode(s string) bool {
	for i := strings.Index(s, nodePrefix); i >= 0; {
		if nodeAt(s, i) {
			return true
		}
		j := strings.Index(s[i+1:], nodePrefix)
		if j < 0 {
			return false
		}
		i += 1 + j
	}
	return false
}

// arnPrefix starts an AWS resource name: arn:partition:service:region:account:resource.
const arnPrefix = "arn:"

// arnShown is s as the name rule shows a string that holds an AWS resource name, which names an account, a region
// and more (contract.SafeName; this module cannot import it, and the tests pin the two to the same values): an ARN as
// its last element, after the last "/" or ":" of its resource (an ECS task's id), when that is a name and holds no
// account (holdsAccount); any other string that holds arn: (namesARN) hashed whole, untrusted. It reports whether s
// holds one; a stored record is shown through this rule whichever engine made it.
func arnShown(s string) (string, bool) {
	if !namesARN(s) {
		return s, false
	}
	if p := strings.SplitN(s, ":", 6); len(p) == 6 && p[0]+":" == arnPrefix && p[1] != "" && p[2] != "" && p[5] != "" {
		last := p[5][strings.LastIndexAny(p[5], "/:")+1:]
		if nameRE.MatchString(last) && !holdsAccount(p[4], last) {
			return last, true
		}
	}
	return untrusted(s), true
}

// holdsAccount reports whether last, an ARN's last element, may name an account (contract's holdsAccount, which
// contract.SafeName applies; this module cannot import it, and the tests pin the two to the same values): it holds
// account, the ARN's own account field, anywhere, or 12 digits in a row, as written or with its "-" taken out (the
// dashed form the AWS console prints).
func holdsAccount(account, last string) bool {
	undashed := strings.ReplaceAll(last, "-", "")
	if account != "" && (strings.Contains(last, account) || strings.Contains(undashed, account)) {
		return true
	}
	run := 0
	for i := 0; i < len(undashed); i++ {
		if undashed[i] < '0' || undashed[i] > '9' {
			run = 0
		} else if run++; run == 12 {
			return true
		}
	}
	return false
}

// namesARN reports whether s holds an AWS resource name, or text that looks like one (contract.NamesARN): arn:, in any
// case, at the start of s or after a character that is not a letter or a digit (so that "warn:" is not one, and
// "task_arn:" is), or anywhere before an AWS partition (arn:aws: or arn:aws-<...>:).
func namesARN(s string) bool {
	for i := 0; i+len(arnPrefix) <= len(s); i++ {
		if !strings.EqualFold(s[i:i+len(arnPrefix)], arnPrefix) {
			continue
		}
		rest := s[i+len(arnPrefix):]
		partition := len(rest) > 3 && strings.EqualFold(rest[:3], "aws") && (rest[3] == ':' || rest[3] == '-')
		if i == 0 || !alnumByte(s[i-1]) || partition {
			return true
		}
	}
	return false
}

// wordByte reports whether c is in [A-Za-z0-9_].
func wordByte(c byte) bool {
	return c == '_' || alnumByte(c)
}

// alnumByte reports whether c is in [A-Za-z0-9].
func alnumByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// untrusted is a string as the name rule shows one it does not keep: svc_ and the first 8 hex digits of its sha256
// (contract.SafeName; this module cannot import it, and the tests pin the two to the same values).
func untrusted(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "svc_" + hex.EncodeToString(sum[:4])
}

// external is a peer as the CLI's views show it, a name that is not a service's: external_ and the first 8 hex digits
// of its sha256 (sanitize.External; this module cannot import it, and the tests pin the two to the same values).
func external(peer string) string {
	sum := sha256.Sum256([]byte(peer))
	return "external_" + hex.EncodeToString(sum[:4])
}

// captureName is a fit's capture as a resource shows it, as explain's fit summary shows it: its last element, through
// the views' name rule (viewName); one that holds an AWS resource name through that rule whole, since its last path
// element may hold the account. A fit records its capture as given to `rhobound fit` (--name, else the folder), and a
// baseline fit made outside the server without --name names a folder of the machine that made it, which no reply may
// carry (ARCHITECTURE.md 5.9). A registered name with no "/" is shown as it is.
func captureName(c string) string {
	if namesARN(c) {
		return viewName(c)
	}
	return viewName(filepath.Base(c))
}

// peerRenamed is the finding whose examples are peer addresses (server.address:port) as the traces wrote them.
const peerRenamed = "trace.peer_renamed"

// hidePeers replaces, in a report's findings, each example of trace.peer_renamed with external_<first 8 hex of its
// sha256>, as the CLI's views and explain show it: a raw server.address value is never emitted (ARCHITECTURE.md 5.9).
// The other findings' examples are ids (a skipped span's trace and span id, each as written when it is written as an
// id, else its class), keys and routes, which the CLI already put through the name rule. It reports whether it
// replaced any.
func hidePeers(findings any) bool {
	fs, _ := findings.([]any)
	hid := false
	for _, f := range fs {
		m, _ := f.(map[string]any)
		if m == nil || m["code"] != peerRenamed {
			continue
		}
		ex, _ := m["examples"].([]any)
		for i, e := range ex {
			s, _ := e.(string)
			ex[i], hid = external(s), true
		}
	}
	return hid
}

// probeLike is the finding whose examples are routes, which start with /.
const probeLike = "trace.probe_like_traffic"

// unrooted removes, from a report's findings, a subject that starts with / and an example that does, but
// trace.probe_like_traffic's (routes), as the CLI's views do: a path from the root names a folder of the machine that
// made the record (a stored record is not trusted to be the configured engine's), and this tool's subjects are names.
// It reports whether it removed any.
func unrooted(findings any) bool {
	fs, _ := findings.([]any)
	cut := false
	for _, f := range fs {
		m, _ := f.(map[string]any)
		if m == nil {
			continue
		}
		if sub, _ := m["subject"].(string); strings.HasPrefix(sub, "/") {
			delete(m, "subject")
			delete(m, "subject_untrusted")
			cut = true
		}
		ex, _ := m["examples"].([]any)
		if len(ex) == 0 || m["code"] == probeLike {
			continue
		}
		kept := ex[:0:0]
		for _, e := range ex {
			if s, _ := e.(string); strings.HasPrefix(s, "/") {
				cut = true
				continue
			}
			kept = append(kept, e)
		}
		if len(kept) == 0 {
			delete(m, "examples")
		} else {
			m["examples"] = kept
		}
	}
	return cut
}

// readPacket serves a prediction's judge packet (servedPacket).
func (s *Server) readPacket(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	pr, err := s.resourcePrediction(ctx, req.Params.URI, packetURI)
	if err != nil {
		return nil, err
	}
	pk, err := s.packet(ctx, pr)
	if err != nil {
		return nil, protocolRefusal(err)
	}
	if pk, err = servedPacket(pk); err != nil {
		return nil, err
	}
	return resourceResult(req.Params.URI, pk, handleTTL, "private"), nil
}

// resourcePrediction resolves the prediction a resource URI names, for its caller.
func (s *Server) resourcePrediction(ctx context.Context, uri, prefix string) (*prediction, error) {
	p, err := s.resourceCall(ctx)
	if err != nil {
		return nil, err
	}
	h, ok := handleOf(uri, prefix, state.KindPrediction)
	if !ok {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	pr, err := s.loadPrediction(ctx, p, h)
	if err != nil {
		return nil, protocolRefusal(err)
	}
	return pr, nil
}
