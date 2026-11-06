// SPDX-License-Identifier: Apache-2.0

package testgen

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// The canary captures of the MCP server's leak sweep (mcp/internal/server, TestServerCanariesNeverLeak): copies of the
// base capture in which every trace value that no remote caller may see (docs/mcp/ARCHITECTURE.md 5.9: raw peer
// addresses and host names, a peer that is not a service, raw request paths, image names, a route that holds the
// model's ext: prefix, the account and cluster of an AWS ARN) is planted as a unique string, so that a test can
// look for each one in every reply. Addresses are from the documentation ranges, host names end in .example, and the
// account is AWS's documentation example; no canary is a part of a registered name or of another canary. Only
// canary-named's, canary-stage's and canary-overlap's are a part of a service name: a trace producer chooses its
// services' names, and the name rule hashes those.

// Planted is one value a canary capture plants in its traces.
type Planted struct {
	Key   string `json:"key"`            // the attribute it is planted as, a span's or a resource's ("" for none)
	Value string `json:"value"`          // the attribute's value, or a part of it, which no reply may carry
	Port  string `json:"port,omitempty"` // the port planted beside it, as "key=value" ("" for none)
	// Class is "peer" (no service answers it), "address" (a known service's), "path", "image", "route" (in an
	// http.route) or "pod" (in a pod identity).
	Class string `json:"class"`
	// Kept is true when the engine's output for the capture holds the value (the fit record, the A/B record, or the
	// refusal's reason); false when only the trace files do, because the reader drops it or the name rule hashes it.
	Kept bool `json:"kept"`
	// Shown is what the engine's output holds in the value's place when it is not kept ("" for nothing): the sign
	// that the capture still reaches the output where the value would be.
	Shown string `json:"shown,omitempty"`
	// Was is what an engine without the rule that keeps the value out wrote in Shown's place ("" for none), so that a
	// test can make a record as such an engine did (a stored record is not trusted to be the current engine's).
	Was string `json:"was,omitempty"`
}

// Canary is one canary capture: a folder, or an A/B pair of the loop capture's ab-shift arms.
type Canary struct {
	Name    string // its folder, and the name a server registers it under
	What    string
	Pair    bool // an A/B pair: build changes each arm
	Refused bool // rhobound fit refuses it
	Planted []Planted
	// build plants the values into c and returns the other files of the capture folder, by path in it.
	build func(c *Capture) map[string][]byte
}

// Build plants the canary's values into c (a copy of the base capture, or one arm of the pair) and returns the other
// files its folder holds, by path in it.
func (k Canary) Build(c *Capture) map[string][]byte { return k.build(c) }

// Canaries returns every canary capture.
func Canaries() []Canary { return append([]Canary(nil), canaries...) }

// The planted values.
const (
	canaryDB     = "canary-db-7f3a.example"       // server.address, with server.port: c's database, 1 to 3 calls a visit
	canaryAudit  = "203.0.113.77"                 // server.address: a's fire-and-forget call
	canaryPay    = "canary-pay-2b9d"              // peer.service
	canaryCache  = "198.51.100.42"                // net.peer.name, with net.peer.port
	canaryAPI    = "canary-api-8e4c.example:8443" // http.host, which carries its port
	canaryPath   = "/orders/canary-path-5b1e"
	canaryTarget = "/orders/canary-target-0d6a?page=2"
	canaryURL    = "https://canary-url-c3f0.example/orders/7"
	canaryLostB  = "canary-b-41c9.example"  // server.address of a's calls to b
	canaryLostC  = "198.51.100.23"          // server.address of b's calls to c
	canaryRefB   = "203.0.113.140"          // server.address of a's calls to b, in the refused capture
	canaryPeer   = "198.51.100.61"          // server.address of a's calls to b: b's ClusterIP in the cluster dump
	canaryQueue  = "canary-mq-5c3b.example" // server.address, with server.port: one of c's two calls in a mixture
	canarySearch = "203.0.113.201"          // net.peer.name: the other
	canaryABPeer = "canary-cache-6a2e.example"
	canaryImage  = "canary-reg-5f2e.example/shop/canary-img-7a1f" // container.image.name of b: a registry's URI
	canaryTag    = "canary-tag-3b6d"                              // container.image.tag of b
	canaryRoute  = "canary-dos-1e9b"                              // in an http.route of probe-like entries, after /ext:
	canaryECS    = "canary-cl-7d2e"                               // the ECS cluster in the task ARNs
	canaryTask   = "1a2b3c4d5e6f70819293a4b5c6d7e8f9"             // the task b runs on in the second half
	canaryCtr    = "canary-ctr-8c1f"                              // in aws.ecs.container.arn, which the reader ignores
	canaryRes    = "canary-rid-2e7b"                              // in cloud.resource_id, which the reader ignores
	canaryInst   = "canary-inst-4b8e"                             // the cluster in the ARNs given as service.instance.id
	canaryProc   = "4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a"             // the container b runs in in the second half
	canaryUnder  = "canary-und-6c3a"                              // the cluster in an ARN after "task_": b's first pod
	canarySvc    = "canary-svc-4e2a"                              // in c's service.name, after a space
	canaryTID    = "203.0.113.188"                                // a trace id that is text, not an id
	canaryStage  = "canary-stg-6d1f"                              // in a service's name that a stage node's would be
	canaryStgIP  = "203.0.113.57"                                 // after it, in the same name
	canaryOvPeer = "203.0.113.163"                                // net.peer.name: c's call after its call to w
	canaryOvl    = "canary-ovl-3a8c"                              // in the name of a service c calls after w
	canaryOvlIP  = "198.51.100.149"                               // after it, in the same name
)

// canaryAccount is the account in the task ARNs, AWS's documentation example account id.
var canaryAccount = strings.Repeat("1", 4) + strings.Repeat("2", 4) + strings.Repeat("3", 4)

// taskARN is the ARN of an ECS task of the canary cluster.
func taskARN(task string) string {
	return "arn:aws:ecs:us-east-1:" + canaryAccount + ":task/" + canaryECS + "/" + task
}

// underARN is b's first pod in canary-arn: an ARN after "task_", where arn: starts no word (_ is a word character).
// The name rule still finds the ARN in it and hashes the whole id: an ARN inside a name is not shown as its last
// element.
var underARN = "task_arn:aws:ecs:us-east-1:" + canaryAccount + ":task/" + canaryUnder + "/9c0d1e2f3a4b5c6d"

// hashed is a string as the name rule shows one it does not keep: svc_ and the first 8 hex digits of its sha256.
func hashed(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "svc_" + hex.EncodeToString(sum[:4])
}

// containerARN is the ARN of a container of an ECS task of the canary cluster, in the instance ids' cluster when
// inst is true.
func containerARN(inst bool, task, container string) string {
	cluster := canaryECS
	if inst {
		cluster = canaryInst
	}
	return "arn:aws:ecs:eu-west-1:" + canaryAccount + ":container/" + cluster + "/" + task + "/" + container
}

var canaries = []Canary{
	{Name: "canary-peers", What: "peers no service answers, under every peer key, raw request paths, and an image name",
		Planted: []Planted{
			{Key: "server.address", Value: canaryDB, Port: "server.port=5432", Class: "peer", Kept: true},
			{Key: "server.address", Value: canaryAudit, Class: "peer", Kept: true},
			{Key: "peer.service", Value: canaryPay, Class: "peer", Kept: true},
			{Key: "net.peer.name", Value: canaryCache, Port: "net.peer.port=6379", Class: "peer", Kept: true},
			{Key: "http.host", Value: canaryAPI, Class: "peer", Kept: true},
			{Key: "", Value: "ext:unknown", Class: "peer", Kept: true},
			{Key: "url.path", Value: canaryPath, Class: "path"},
			{Key: "http.target", Value: canaryTarget, Class: "path"},
			{Key: "http.url", Value: canaryURL, Class: "path"},
			{Key: "container.image.name", Value: canaryImage, Class: "image", Kept: true},
			{Key: "container.image.tag", Value: canaryTag, Class: "image", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			// c, the chain's leaf, calls the database 1 to 3 times a visit, then each other peer once, one after another
			addLeafCalls(c, svcC, func(n uint64) ([][]KV, bool) {
				var calls [][]KV
				for k := uint64(0); k <= n%3; k++ {
					calls = append(calls, []KV{{"server.address", Str(canaryDB)}, {"server.port", Int(5432)}})
				}
				return append(calls, []KV{{"peer.service", Str(canaryPay)}},
					[]KV{{"net.peer.name", Str(canaryCache)}, {"net.peer.port", Int(6379)}},
					[]KV{{"http.host", Str(canaryAPI)}}, nil), false
			})
			// a sends one call that ends after its response
			addChildren(c, svcA, otlp.KindServer, 1, func(parent *Span, k int) *Span {
				s := childOf(parent, k+0x40)
				a, b := parent.Times()
				s.SetTimes(b-(b-a)/10, b+20_000_000)
				s.SetKind(otlp.KindClient)
				s.Name = "POST"
				s.SetAttr("server.address", Str(canaryAudit))
				return s
			})
			// a's requests carry their raw path in one of the three keys, beside the template
			spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
				s.SetAttr("http.request.method", Str("GET"))
				s.SetAttr("http.route", Str("/orders/{id}"))
				key, v := [3]string{"url.path", "http.target", "http.url"}, [3]string{canaryPath, canaryTarget, canaryURL}
				i := TraceNumber(s.TraceID) % 3
				s.SetAttr(key[i], Str(v[i]))
			})
			// b's resources name the image it runs, as a registry's URI and a tag
			forAllResources(c, func(rs *ResourceSpans) {
				if rs.Service() == svcB {
					rs.SetAttr("container.image.name", Str(canaryImage))
					rs.SetAttr("container.image.tag", Str(canaryTag))
				}
			})
			return nil
		}},
	{Name: "canary-lost", What: "2.5% of the traces lose a known callee's spans, the callers' CLIENT spans giving its address",
		Planted: []Planted{
			{Key: "server.address", Value: canaryLostB, Port: "server.port=8080", Class: "address", Kept: true},
			{Key: "server.address", Value: canaryLostC, Port: "net.peer.port=9090", Class: "address", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			address(c, svcA, canaryLostB, "server.port", 8080)
			address(c, svcB, canaryLostC, "net.peer.port", 9090)
			// every 80th trace loses the spans of b and c, every 80th from the 40th those of c
			c.Filter(func(rs *ResourceSpans, s *Span) bool {
				n, svc := TraceNumber(s.TraceID)%80, rs.Service()
				return !(n == 0 && (svc == svcB || svc == svcC) || n == 40 && svc == svcC)
			})
			return nil
		}},
	{Name: "canary-refused", What: "12.5% of b's SERVER spans lost, a's CLIENT spans giving b's address: fit refuses",
		Refused: true,
		Planted: []Planted{
			{Key: "server.address", Value: canaryRefB, Port: "server.port=8080", Class: "address", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			address(c, svcA, canaryRefB, "server.port", 8080)
			c.Filter(func(rs *ResourceSpans, s *Span) bool {
				return !(rs.Service() == svcB && s.KindOf() == otlp.KindServer && every(s, 8))
			})
			return nil
		}},
	{Name: "canary-renamed", What: "a peer given as a Service's ClusterIP, with a cluster dump (trace.peer_renamed)",
		Planted: []Planted{
			{Key: "server.address", Value: canaryPeer, Port: "server.port=8080", Class: "address", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			address(c, svcA, canaryPeer, "server.port", 8080)
			list := `{"apiVersion":"v1","kind":"List","items":[{"kind":"Service","metadata":{"name":"b","namespace":"shop"},` +
				`"spec":{"clusterIP":"` + canaryPeer + `","ports":[{"port":8080}]}}]}` + "\n"
			return map[string][]byte{"cluster/services.json": []byte(list)}
		}},
	{Name: "canary-shapes", What: "c calls two peers no service answers, at once on every other visit: a mixture of call shapes",
		Planted: []Planted{
			{Key: "server.address", Value: canaryQueue, Port: "server.port=5672", Class: "peer", Kept: true},
			{Key: "net.peer.name", Value: canarySearch, Class: "peer", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			addLeafCalls(c, svcC, func(n uint64) ([][]KV, bool) {
				return [][]KV{{{"server.address", Str(canaryQueue)}, {"server.port", Int(5672)}},
					{{"net.peer.name", Str(canarySearch)}}}, n%2 == 0
			})
			return nil
		}},
	{Name: "canary-ab", What: "the ab-shift pair, c calling a cache no service answers on every visit, in both arms",
		Pair: true,
		Planted: []Planted{
			{Key: "server.address", Value: canaryABPeer, Port: "server.port=11211", Class: "peer", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			addLeafCalls(c, svcC, func(uint64) ([][]KV, bool) {
				return [][]KV{{{"server.address", Str(canaryABPeer)}, {"server.port", Int(11211)}}}, false
			})
			return nil
		}},
	{Name: "canary-probe", What: "probe-like entries (a kube-probe user agent) whose http.route holds /ext: and a canary",
		Planted: []Planted{
			// the name rule hashes the route whole, since ext: starts a word in it: sha256 starts e9ec6358
			{Key: "http.route", Value: canaryRoute, Class: "route", Shown: "svc_e9ec6358"},
		},
		build: func(c *Capture) map[string][]byte {
			// every 10th entry: trace.probe_like_traffic lists the route among its examples
			spansOf(c, svcA, otlp.KindServer, func(_ *ResourceSpans, s *Span) {
				if every(s, 10) {
					s.SetAttr("http.route", Str("/status/ext:"+canaryRoute))
					s.SetAttr("user_agent.original", Str("kube-probe/1.30"))
				}
			})
			return nil
		}},
	{Name: "canary-ecs", What: "ECS task ARNs as the pod identity, b's task replaced half way (trace.pod_churn)",
		Planted: []Planted{
			// the reader keeps a task's id, the ARN's last element: b's second task is subject of trace.pod_churn; an
			// engine without that rule wrote the whole ARN there, and the name rule of the views shows its last element
			{Key: "aws.ecs.task.arn", Value: canaryAccount, Class: "pod", Shown: canaryTask, Was: taskARN(canaryTask)},
			{Key: "aws.ecs.task.arn", Value: canaryECS, Class: "pod", Shown: canaryTask, Was: taskARN(canaryTask)},
			// the other ARNs of an ECS resource, which the reader does not read
			{Key: "aws.ecs.container.arn", Value: canaryCtr, Class: "pod"},
			{Key: "cloud.resource_id", Value: canaryRes, Class: "pod"},
		},
		build: func(c *Capture) map[string][]byte {
			forAllResources(c, func(rs *ResourceSpans) { rs.DelAttr("k8s.pod.name") })
			for svc, task := range map[string]string{svcA: "2b3c4d5e6f708192a3b4c5d6e7f80910",
				svcB: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", svcC: "3c4d5e6f70819203b4c5d6e7f8091021"} {
				forResources(c, svc, func(rs *ResourceSpans, _ int) {
					rs.SetAttr("aws.ecs.task.arn", Str(taskARN(task)))
					rs.SetAttr("aws.ecs.container.arn", Str(containerARN(false, task, canaryCtr)))
					rs.SetAttr("cloud.resource_id", Str(containerARN(false, task, canaryRes)))
				})
			}
			// b's traces whose entry starts in the second half of the window run on a new task
			lo, hi := dataWindow(c)
			starts := entryStarts(c)
			splitResources(c, svcB, func(s *Span) bool { return starts[s.TraceID] >= lo+(hi-lo)/2 }, func(rs *ResourceSpans) {
				rs.SetAttr("aws.ecs.task.arn", Str(taskARN(canaryTask)))
			})
			return nil
		}},
	{Name: "canary-arn", What: "container ARNs as service.instance.id, the pod identity without a pod name or a task, " +
		"b's replaced half way (trace.pod_churn), b's first an ARN after task_",
		Planted: []Planted{
			// the fit record's pod shares hold the pods as read; the name rule shows an ARN as its last element, in the
			// subject of trace.pod_churn too
			{Key: "service.instance.id", Value: canaryInst, Class: "pod", Kept: true, Shown: canaryProc},
			// and hashes a name that holds an ARN after "_"
			{Key: "service.instance.id", Value: canaryUnder, Class: "pod", Kept: true, Shown: hashed(underARN)},
		},
		build: func(c *Capture) map[string][]byte {
			forAllResources(c, func(rs *ResourceSpans) { rs.DelAttr("k8s.pod.name") })
			for svc, task := range map[string]string{svcA: "5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b",
				svcB: "6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c", svcC: "7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d"} {
				id := containerARN(true, task, "8b9c0d1e2f3a4b5c6d7e8f9a0b1c2d3e")
				if svc == svcB {
					id = underARN
				}
				forResources(c, svc, func(rs *ResourceSpans, _ int) { rs.SetAttr("service.instance.id", Str(id)) })
			}
			lo, hi := dataWindow(c)
			starts := entryStarts(c)
			splitResources(c, svcB, func(s *Span) bool { return starts[s.TraceID] >= lo+(hi-lo)/2 }, func(rs *ResourceSpans) {
				rs.SetAttr("service.instance.id", Str(containerARN(true, "6f7a8b9c0d1e2f3a4b5c6d7e8f9a0b1c", canaryProc)))
			})
			return nil
		}},
	{Name: "canary-named", What: "a service whose name the name rule hashes (c, with a space and a canary), calling two " +
		"peers at once on every other visit: its rows, and the model's nodes of its call shapes, are named after it",
		Planted: []Planted{
			// the fit record and the report keep a service's name as the traces wrote it; every view shows this one as
			// svc_<hash8> of it, and so must every document the server serves
			{Key: "service.name", Value: canarySvc, Class: "name", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			addLeafCalls(c, svcC, func(n uint64) ([][]KV, bool) {
				return [][]KV{{{"server.address", Str("mq.example")}, {"server.port", Int(5672)}},
					{{"net.peer.name", Str("search.example")}}}, n%2 == 0
			})
			forResources(c, svcC, func(rs *ResourceSpans, _ int) { rs.SetAttr("service.name", Str(svcC+" "+canarySvc)) })
			return nil
		}},
	{Name: "canary-ids", What: "copies of every 20th of a's SERVER spans, as INTERNAL spans whose trace id is text, " +
		"not an id: the reader skips them (trace.spans_skipped)",
		Planted: []Planted{
			// the findings' examples of skipped spans name an id that does not parse by its encoding class, never as
			// written; no attribute holds it
			{Key: "", Value: canaryTID, Class: "id"},
		},
		build: func(c *Capture) map[string][]byte {
			addChildren(c, svcA, otlp.KindServer, 20, func(parent *Span, _ int) *Span {
				s := parent.Copy()
				s.TraceID, s.SpanID = canaryTID, fmt.Sprintf("%016x", 0xd000000000000000|TraceNumber(parent.TraceID))
				s.SetKind(otlp.KindInternal) // not an entry: the capture's request count stays the entries'
				return s
			})
			return nil
		}},
	{Name: "canary-stage", What: "a service named as the model names a stage node (stage:S:1), S a name the name rule " +
		"hashes, between a and b: the biased constructs on the path to b pass through it",
		Planted: []Planted{
			// the fit record keeps the service's name as the traces wrote it; nothing of S may be shown as written
			{Key: "service.name", Value: canaryStage, Class: "name", Kept: true},
			{Key: "service.name", Value: canaryStgIP, Class: "name", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			between(c, svcA, svcB, "stage:"+canaryStage+" "+canaryStgIP+":1", "stg-0")
			return nil
		}},
	{Name: "canary-overlap", What: "services c calls, one after another on every other visit and at once on the others, " +
		"named so that a name runs across the words of c's call shapes: w then ext: before a peer no service answers, " +
		"w then q before a service whose name the name rule hashes",
		Planted: []Planted{
			{Key: "net.peer.name", Value: canaryOvPeer, Class: "peer", Kept: true},
			{Key: "service.name", Value: canaryOvl, Class: "name", Kept: true},
			{Key: "service.name", Value: canaryOvlIP, Class: "name", Kept: true},
		},
		build: func(c *Capture) map[string][]byte {
			// c's calls, in order: a service's name, or "" for the peer; in the shape of the visits that call one after
			// another, "w then ext:" runs from w into the peer's node, and "w then q" from w into q's name
			callees := []string{"w", "", "w", "q " + canaryOvl + " " + canaryOvlIP, "w then ext:", "w then q"}
			pods := []string{"w-0", "", "w-0", "q-0", "wx-0", "wq-0"}
			addLeafCalls(c, svcC, func(n uint64) ([][]KV, bool) {
				calls := make([][]KV, len(callees))
				for k, svc := range callees {
					if svc == "" {
						calls[k] = []KV{{"net.peer.name", Str(canaryOvPeer)}}
					}
				}
				return calls, n%2 == 0
			})
			answerCalls(c, svcC, callees, pods)
			return nil
		}},
}

// between puts a service named name, on the pod pod, between caller and callee: each SERVER span of callee under a
// CLIENT span of caller is moved under a CLIENT span of the new service, whose SERVER span is under the caller's, each
// inside the one above it.
func between(c *Capture, caller, callee, name, pod string) {
	clients := map[string]*Span{}
	spansOf(c, caller, otlp.KindClient, func(_ *ResourceSpans, s *Span) { clients[s.TraceID+"/"+s.SpanID] = s })
	for _, r := range c.Requests {
		var add []*Span
		for _, rs := range r.ResourceSpans {
			if rs.Service() != callee {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					parent := clients[s.TraceID+"/"+s.ParentSpanID]
					if s.KindOf() != otlp.KindServer || parent == nil {
						continue
					}
					n := TraceNumber(s.TraceID)
					a0, b0 := parent.Times()
					a1, b1 := s.Times()
					srv := &Span{TraceID: s.TraceID, SpanID: fmt.Sprintf("%016x", 0xe000000000000000|n<<8|1),
						ParentSpanID: parent.SpanID, Name: "GET"}
					srv.SetTimes(a0+(a1-a0)/3, b0-(b0-b1)/3)
					srv.SetKind(otlp.KindServer)
					cl := &Span{TraceID: s.TraceID, SpanID: fmt.Sprintf("%016x", 0xe000000000000000|n<<8|2),
						ParentSpanID: srv.SpanID, Name: "GET", Attributes: []KV{{"server.address", Str(callee)}}}
					cl.SetTimes(a0+2*(a1-a0)/3, b0-2*(b0-b1)/3)
					cl.SetKind(otlp.KindClient)
					s.ParentSpanID = cl.SpanID
					add = append(add, srv, cl)
				}
			}
		}
		if len(add) > 0 {
			r.ResourceSpans = append(r.ResourceSpans, &ResourceSpans{
				Resource:   Resource{Attributes: []KV{{"service.name", Str(name)}, {"k8s.pod.name", Str(pod)}}},
				ScopeSpans: []*ScopeSpans{{Spans: add}}})
		}
	}
}

// answerCalls gives each CLIENT span of svc that addLeafCalls added ("call k") a SERVER span under it, of the service
// callees[k] on the pod pods[k], inside it; a call whose callee is "" is left to no service.
func answerCalls(c *Capture, svc string, callees, pods []string) {
	for _, r := range c.Requests {
		bySvc := map[string][]*Span{}
		var order []string
		for _, rs := range r.ResourceSpans {
			if rs.Service() != svc {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					k, err := strconv.Atoi(strings.TrimPrefix(s.Name, "call "))
					if s.KindOf() != otlp.KindClient || !strings.HasPrefix(s.Name, "call ") || err != nil ||
						k >= len(callees) || callees[k] == "" {
						continue
					}
					a, b := s.Times()
					srv := &Span{TraceID: s.TraceID, SpanID: fmt.Sprintf("%016x", 0x8000000000000000|
						TraceNumber(s.TraceID)<<8|uint64(k)), ParentSpanID: s.SpanID, Name: "GET"}
					srv.SetTimes(a+(b-a)/10, b-(b-a)/10)
					srv.SetKind(otlp.KindServer)
					if bySvc[callees[k]] == nil {
						order = append(order, callees[k])
					}
					bySvc[callees[k]] = append(bySvc[callees[k]], srv)
				}
			}
		}
		for _, name := range order {
			pod := pods[slices.Index(callees, name)]
			r.ResourceSpans = append(r.ResourceSpans, &ResourceSpans{
				Resource:   Resource{Attributes: []KV{{"service.name", Str(name)}, {"k8s.pod.name", Str(pod)}}},
				ScopeSpans: []*ScopeSpans{{Spans: bySvc[name]}}})
		}
	}
}

// address gives every CLIENT span of svc the peer address host and the port under portKey.
func address(c *Capture, svc, host, portKey string, port int64) {
	spansOf(c, svc, otlp.KindClient, func(_ *ResourceSpans, s *Span) {
		s.SetAttr("server.address", Str(host))
		s.SetAttr(portKey, Int(port))
	})
}

// addLeafCalls adds, under each SERVER span of svc (a leaf of the chain: its span's time is all its own), the CLIENT
// spans of the calls that calls returns for its trace number, each with the attributes given (none: a call that names
// no peer), one after another inside the span, or all at once when calls says so.
func addLeafCalls(c *Capture, svc string, calls func(n uint64) (attrs [][]KV, atOnce bool)) {
	for _, r := range c.Requests {
		for _, rs := range r.ResourceSpans {
			if rs.Service() != svc {
				continue
			}
			for _, ss := range rs.ScopeSpans {
				var add []*Span
				for _, s := range ss.Spans {
					if s.KindOf() != otlp.KindServer {
						continue
					}
					n := TraceNumber(s.TraceID)
					cs, atOnce := calls(n)
					a, b := s.Times()
					step := (b - a) / uint64(2*len(cs)+1)
					for k, attrs := range cs {
						cl := &Span{TraceID: s.TraceID, SpanID: fmt.Sprintf("%016x", 0x9000000000000000|n<<8|uint64(k)),
							ParentSpanID: s.SpanID, Name: "call " + strconv.Itoa(k), Attributes: append([]KV(nil), attrs...)}
						st := a + step*uint64(2*k+1)
						if atOnce {
							st = a + step
						}
						cl.SetTimes(st, st+step)
						cl.SetKind(otlp.KindClient)
						add = append(add, cl)
					}
				}
				ss.Spans = append(ss.Spans, add...)
			}
		}
	}
}
