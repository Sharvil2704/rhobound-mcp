# rhobound-mcp: the data contract

Garbage in, garbage out. This document says, for every input rhobound-mcp takes, which format and version it accepts,
which fields it needs, what it parses, what it refuses (with the code and the fix), what it reads but loses, and how
much of it is needed. It is the reference for `rhobound check`, for the server's `rhobound://data-contract` resource,
and for anyone preparing data.

Status markers: **[built]** is how rhobound-cli behaves today, at commit 1a2df09, with the code location given as
`file:line` at that commit. **[new]** is built in the first slice of rhobound-mcp (on the branch `rhobound-mcp`).
**[later]** comes after it. Every **[new]** code ships with a test: an injected defect for codes a capture triggers, a
unit test for the others (section 17). Tags [V] [K] [U] [E] [D] are as in
[ARCHITECTURE.md](ARCHITECTURE.md): verified, prior knowledge, unverified, estimate, placeholder threshold.

The short version is the checklist in section 16.

---

## 1. Rules of the contract

1. **Every input is a declared kind** with a name, a format and a version. A file is never "probably traces".
2. **Every finding has a stable code** `domain.name`, a level (`refuse`, `doubt`, `note`), a subject, the numbers
   read, up to three examples (trace ids, span ids, object keys or line numbers), and a **fix**. The code never
   contains data; the subject is sanitized.
3. **No silent path.** Everything in an input is parsed, normalized with a note, declared ignored with the reason,
   or refused by name. Today some inputs are skipped or misread without a word; section 3.14 lists each one and its
   new code.
4. **Derived over declared.** When the data can tell (the kept share from an independent count, the image digest from
   resource attributes, a worker count from the queue rebuild), it is derived. A declaration is a cross-check; a
   disagreement is a finding.
5. **Labels never move up.** Every number entering the model carries where it came from (ARCHITECTURE.md, 4.3). Only
   the server or the CLI mints labels other than `declared`.
6. **Admitted is not accurate.** Every output carries the assumptions list: what was checked, what raised a doubt,
   what cannot be checked from this data, and what was not checked.
7. **Mean only.** Inputs or requests about tails, burst correction, or load at or above the utilization limit are
   refused by name.
8. **One canonical form.** After admission a capture is one folder of `*.jsonl.gz` files, one OTLP/JSON request per
   line, hex ids, integer kinds, lowerCamelCase keys, attributes reduced to the canonical keep-list (every key the
   reader and `check` read: section 15), no duplicate spans, whole traces cut by their entry span's start, plus
   `manifest.json`. Every number is computed from that form, and the cut is tested to be neutral: on a window that
   covers the whole capture, the raw capture and its canonical form give the same prediction bytes (section 17).

---

## 2. The inputs at a glance

| # | input | format | required for | status |
|---|---|---|---|---|
| I1 | trace capture | OTLP/JSON, one request per line, plain or gzip | everything | [built] reader; [new] contract checks |
| I2 | capture manifest | `rhobound.capture-manifest/v1` JSON | every fit (written by the fetcher, or by hand for a local folder) | [new] |
| I3 | cluster / architecture dump | Kubernetes `List` JSON (EKS) in `cluster/` (flat, or `start/` and `end/`), or `rhobound.peer-map/v1` (ECS) | optional; recommended; required for profile environment matching | [new] |
| I4 | independent request count | `rhobound.request-count/v1` | enforce mode; otherwise optional | [new] |
| I5 | resource metrics | `rhobound.resource-metrics/v1` | optional cross-check | [later] |
| I6 | A/B captures | two I1 captures plus a load-generator log | the `measured-ab` label | [new] |
| I7 | image performance profile | in-toto Statement v1, predicate `perf-profile/v0.1`, signed, an OCI referrer | the `measured-paired` label | [later] |
| I8 | load levels | fields of I6 and I7, and the busiest observed window | measured labels; gating load | [new] / [later] |
| I9 | change description | `rhobound.change/v1`, or the existing `--patch` | every prediction | [built] `--patch`; [new] change sets |
| I10 | policy | `rhobound.policy/v1` | every gate | [new] |
| I11 | judge answer | `rhobound-mcp.judge-answer/v1` | optional | [later] |
| I12 | after-deploy capture | I1 plus binding rules | verification, enforcement readiness | [later] |
| I13 | code-path inputs | git trees, build identity | research only | see [CODEPATHS.md](CODEPATHS.md) |
| I14 | benchmark run record | `rhobound.run-record/v1` | studies | see [EVALUATION.md](EVALUATION.md) |

---

## 3. Traces (I1)

### 3.1 Format and version

- **OTLP/JSON** as defined by the OpenTelemetry Protocol specification [V: https://opentelemetry.io/docs/specs/otlp/],
  in the shape the Collector's `file` exporter and `awss3exporter` (`marshaler: otlp_json`) write
  [V: https://opentelemetry.io/docs/specs/otel/protocol/file-exporter/]. Each value is an
  `ExportTraceServiceRequest` (the same JSON as `TracesData`): `resourceSpans[].resource.attributes`,
  `resourceSpans[].scopeSpans[].scope`, `scopeSpans[].spans[]` [built: `jsonRequest`, `internal/otlp/otlp.go:144`].
- **OTLP/JSON is not generic protobuf-JSON.** The OTLP spec requires hex trace and span ids, integer enum values and
  lowerCamelCase keys. A generic protobuf-to-JSON converter writes base64 ids, enum names (`"SPAN_KIND_SERVER"`) and
  sometimes snake_case keys. Those are different formats and are refused by name (3.14).
- **Not read:** OTLP protobuf (`.pb`, `.binpb`), zstd, Jaeger, Zipkin, X-Ray segment documents, CloudWatch `aws/spans`
  records, Tempo or ClickHouse rows. Convert them with a Collector receiver into the `file` or `awss3` exporter
  (section 4.4). Converters inside rhobound-mcp are [later].

### 3.2 Files, framing, compression

| aspect | accepted | today [built] | contract |
|---|---|---|---|
| layout | a folder with `traces/`, a folder of trace files, or one file | `traceFiles`, `otlp.go:553-582`: not recursive; files in sorted name order | [new] the fit reads the **manifest's file list**, never "every matching file in the folder", and checks each file's sha256 |
| names | `*.jsonl`, `*.json`, `*.jsonl.gz`, `*.json.gz` | other names in the folder are **silently ignored** (`traceFiles`); with none left: `no trace files (*.jsonl, *.json, *.gz) under PATH` (`otlp.go:405-407`) | [new] `store.unreadable_object` names every `.zst`, `.binpb`, `.pb`, `.parquet` or unsuffixed object, even when other files are readable |
| compression | gzip, chosen by the `.gz` suffix | `feedFile`, `otlp.go:596-603` | [new] `store.compression_mismatch` when a `.gz` name is not gzip |
| framing | `.jsonl`: one request per line; `.json`: a stream of whitespace-separated values | `otlp.go:604-631`; one undecodable value fails the whole capture, by one of two paths: a `.json` file that is not valid JSON fails while it is split into values, as `<path>: <decoder error>` (`otlp.go:626-627`); a value or line that is valid JSON but not the OTLP shape (a wrong type, for example) fails as `bad OTLP/JSON: <decoder error>` (`otlp.go:537`), and so does a `.jsonl` line that is not valid JSON | [new] both paths map to `trace.bad_json`, with file and line (or value index); a pretty-printed `.jsonl` file and a top-level JSON array are refused by name (`trace.jsonl_pretty`, `trace.json_array`) |
| a partly written last line | only in the newest object of the window | today the whole capture is refused | [new] dropped and counted (`trace.partial_tail_dropped`, note); anywhere else it is corruption (`trace.bad_json`) |
| sizes | | **no limit**: gzip is inflated without a cap (`otlp.go:597`), lines are read with an unbounded `ReadBytes` (`otlp.go:607`), and the whole capture is held in memory | [new] caps per object and per capture on inflated bytes, a maximum line length, a maximum span count and attribute length; reading stops with `input.too_large` (defaults [D]: 1 GiB per object, 8 GiB per capture, 64 MiB per line, 20 million spans, 4 KiB per kept attribute value). A first estimate is made from the store listing before any download (`store.too_large`). The caps bound `check`, `fit` and the server; `predict CAPTURE` and `infer` (report v1) read with none, as today |

`rhobound check` [new] has its own exit codes, distinct from the gate's: **0** admitted (doubts are listed in the
report and do not change the exit code), **4** refused, **2** the tool itself failed (a usage error, an unreadable
file system). A data refusal is never exit 1, which the gate uses for `block`.

Throughput, for planning [E from a measurement on synthetic spans]: 619,800 small spans (139 MB of OTLP/JSON, 14 MB
gzipped) were read and inferred in about 1.05 s with `GOMAXPROCS=2` on an 8-core arm64 machine. Real spans carry more
attributes; budget a few seconds per 600,000 real spans on two cores. A 579 MB capture took 1.0 s on 8 cores with a
peak of about 200-230 MiB ([ACCURACY.md](../ACCURACY.md)).

### 3.3 Span fields

`jsonSpan`, `otlp.go:133-142`.

| field | required | read today [built] | if wrong today | contract |
|---|---|---|---|---|
| `traceId` | yes | exactly 32 hex characters, either case (`traceID`, `otlp.go:237-244`) | the span is skipped; if all are skipped: `the capture holds no usable spans` (`internal/infer/infer.go:315`) with no cause | [new] base64 ids (24 characters decoding to 16 bytes) refused as `trace.ids_base64`; 16-hex ids as `trace.trace_id_64bit`; mixed encodings as `trace.ids_mixed`; other skipped spans above 0.1% [D] a doubt `trace.spans_skipped`, whose examples show each id as written when it is written as an id of some encoding, else as `other` |
| `spanId` | yes | exactly 16 hex (`hex64`, `otlp.go:229-235`) | skipped | as above (12-character base64 for span ids) |
| `parentSpanId` | for every non-root span | 16 hex or empty | anything unparsable is read as "no parent", so the span becomes a root (`otlp.go:340-343`) | [new] counted; base64 refused as above |
| `kind` | yes | an integer 1-5 (`otlp.go:26-32`); 0 is walked through like INTERNAL | a string (`"SPAN_KIND_SERVER"`, `"Server"`) is a JSON type error that **refuses the whole capture** | [new] `trace.kind_enum_name` for the proto3 enum names, `trace.kind_unknown` for any other string; both refuse with the fix "export with the Collector's OTLP/JSON exporters" |
| `startTimeUnixNano`, `endTimeUnixNano` | yes | a decimal string or a JSON number, parsed from the digits exactly (`nanos`, `otlp.go:96-113`) | start 0 or end before start: skipped and counted in a warning (`otlp.go:336`; `infer.go:568-570`) | [new] float or exponent forms are normalized with a note (`trace.time_not_integer`); float64 at today's epoch resolves about 0.24 microseconds, irrelevant to millisecond means |
| `name` | no | kept only for SERVER spans of a second or more (stream naming), and as a route fallback for probe detection | | [new] never emitted raw to an agent (ARCHITECTURE.md, 5.9) |
| `traceState` | no | **not read** | the sampling threshold is lost | [new] `ot=th:` read on entry SERVER spans (3.8) |
| `status` | no | **not read** | errors count as normal visits | [new] `status.code` read (3.5) |
| `events` | no | **not read** | | [new] names and counts of `rpc.message` events per SERVER span (streams, 3.10) |
| `links` | no | not read | fan-in and batch consumers are invisible | [new] `trace.links_present` doubt on CONSUMER spans |
| `flags`, `droppedAttributesCount`, `droppedEventsCount`, `schemaUrl`, scope `version` | no | not read | | [new] counted in the check report's `ignored` block; `droppedAttributesCount > 0` on CLIENT spans that lack a peer is a note (lost-span detection is blind there) |

### 3.4 Resource attributes

Only `stringValue` is read today, and `intValue` for ports (`anyValue.text`, `otlp.go:115-126`). Array values are
ignored.

| purpose | keys, in order | today [built] (`decodeRequest`, `otlp.go:272-317`) | contract |
|---|---|---|---|
| service | `service.name`, else `k8s.deployment.name` | a span with neither is skipped | [new] `service.name` must be a string (`trace.service_name_not_string`, refuse); a name starting with `unknown_service` (the SDK default when nothing is configured [V: https://opentelemetry.io/docs/specs/semconv/registry/attributes/service/]) is refused (`trace.service_name_default`), because every unconfigured service would merge into one; services are keyed by (`service.namespace`, `service.name`) |
| pod identity | today `k8s.pod.name`, else `service.instance.id`, else `host.name` | without any, all spans of a service are one pod | [new] order `k8s.pod.name` (or `k8s.pod.uid`), then `aws.ecs.task.arn`, then today's `service.instance.id`, then `host.name`. Of `aws.ecs.task.arn` only the task's id, the ARN's last element, is kept: the rest names the account, the region and the cluster, which no output needs. Every view, and `rhobound://prediction`, shows any other ARN as its last element too, unless that holds an account, when it is hashed whole, in a record made by any build of the engine (ARCHITECTURE.md 5.9); a task's id the reader kept is shown as a name, as written. `host.name` is kept as written, and on EC2 it carries the host's private address, which findings then show to every caller with read scope (ARCHITECTURE.md 5.9). This is a deliberate change only for captures that carry a platform identity (a pod uid without a name, or an ECS task) and no pod name; captures without one keep today's order, so their numbers do not move. On a capture that does, the predictions can change: the pod count and the per-pod queues follow the platform identity, where 1a2df09 used `service.instance.id` or `host.name` or, with neither, read all of a service's spans as one pod; and report v1's split warning names the pods by that identity, an ECS task by its id (`cmd/rhobound/podidentity_test.go` pins such captures, section 17: one whose pods are named by task ARNs alone, and one whose two pods take uneven shares, where the warning names the task ids and the report is byte for byte the one of the same capture with those ids as `k8s.pod.name`). Ambiguity that remains: `service.instance.id` is per process, so behind a pre-fork server (gunicorn, uWSGI, Puma) one task of N workers sharing a socket reads as N pods of one worker each, while independent replicas on one VM really are separate queues. Traces cannot tell the two apart. When one `host.name` carries several instance ids of one service and no platform identity is present, `trace.instance_per_process` (doubt) is raised and today's reading stays; a team whose processes share one listening socket declares `--pod-identity host` (recorded in the fit), which keys pods by `host.name` for that capture. A fixture of each kind (pre-fork workers on one host; independent replicas on one host) tests both readings |
| pod identity, collapsed | one pod identity carrying SERVER spans of several services | not detected | [new] `trace.pod_shared` (doubt) when one pod carries SERVER spans of two services (a pod with a sidecar can be legitimate); `trace.pod_identity_collapsed` (refuse) at three or more [D]. The usual cause is a Collector that stamps its own pod on every span it forwards (4.1) |
| image | `container.image.name`, `container.image.tag` (singular) | reported per service, not modelled (`infer.go:765-769`) | the report's image names are not served to agents: `rhobound://prediction` leaves them out (ARCHITECTURE.md 5.9), since on a registry such as ECR they name the account. The singular `container.image.tag` is read for compatibility but is **not in the current semantic conventions** [V: https://opentelemetry.io/docs/specs/semconv/registry/attributes/container/]. [new] read `container.image.tags` and `container.image.repo_digests` (stable, `string[]`). Never bind on `container.image.id` (runtime-specific, "can vary in different environments" [V]). Two digests for one service in one window: `trace.mixed_images` (refuse for a baseline; the window spans a rollout). No digest: `trace.image_digest_missing` (doubt; a refusal of the binding when a measured label needs it) |
| environment | `deployment.environment.name` (or the older `deployment.environment`) | not read | [new] more than one value in a capture: `trace.mixed_environments` (refuse) |
| runtime | `faas.*` on a SERVER span's resource | not read | [new] `trace.faas_service` (refuse for that service): a function platform runs an execution environment per concurrent request, so there is no queue to rebuild; a delay-node treatment is not designed yet |

### 3.5 Span attributes

| purpose | keys | today [built] | contract |
|---|---|---|---|
| CLIENT peer | `peer.service`, `server.address`, `net.peer.name`, `http.host` (first set) (`peerKeys`, `otlp.go:160`); port `server.port` or `net.peer.port` (`otlp.go:163`) | names the far side of a call; `peer:port` keys lost-span detection (`otlp.go:353-387`); a CLIENT span with no peer attribute is an outside delay `ext:unknown` (`infer.go:380`) and can never be counted as lost | [new] a peer that is not a known service appears to agents only as `external_<hash8>` (the first 8 hex digits of the peer's sha256), `rhobound://prediction` included. That is an unkeyed pseudonym that hides a peer from casual view, not a secret: anyone who can guess candidate addresses can confirm one, so where peer addresses are sensitive, do not grant read scope to callers you do not trust. `ext:` is reserved for these nodes: any other string from a trace in which it starts a word (a route such as `/status/ext:x`) is shown hashed, `svc_<hash8>` (ARCHITECTURE.md 5.9). With a cluster dump, a peer given as a ClusterIP is renamed to its Service (`trace.peer_renamed`, note). All four keys are in the canonical keep-list (15): dropping `http.host` would turn peers named only by it into `ext:unknown` |
| SERVER route | `url.path`, `http.target`, `http.route`, `http.path`, `http.url`, else the span name (`pathKeys`, `otlp.go:169`; `route`, `otlp.go:212-227`), query and scheme stripped | used only to drop health probes; routes are not kept | [new] `http.route` and `rpc.method` are kept in the fit for later per-route work, and today show only among the examples of `trace.probe_like_traffic`, through the name rule (ARCHITECTURE.md 5.9); raw `url.path` is never emitted to an agent. All five path keys are in the canonical keep-list (15), so the cut cannot change a probe decision; a Collector that drops them earlier (4.1) changes probe detection for its own captures, before any of rhobound's steps |
| method | `http.request.method`, `http.method` | probe detection only | |
| probes | regex `grpc\.health\.v1\.Health/|(^|\s)/(_?healthz?|readyz|livez|ready|ping)(\?|$)` (`probeRoute`, `otlp.go:176`) | matching SERVER spans are dropped, their children re-rooted | the regex misses common framework probes and scrapes: `/actuator/health`, `/actuator/health/liveness`, `/q/health/ready`, `/health/live`, `/api/health`, `/metrics` (tested). [new] also classify by `user_agent.original` (`kube-probe/`, `ELB-HealthChecker/`, `Prometheus/` [K]); accept declared extra probe routes with a reason; a route with tiny spans at a fixed period per pod above 1% of entries [D] raises `trace.probe_like_traffic` (doubt) |
| errors | `status.code`, `http.response.status_code` | not read | [new] `trace.errors_present` when more than 1% [D] of a service's SERVER spans have `status.code = ERROR`; `trace.shedding` when 429 or 503 answers exceed 1% [D] (fast shedding lowers the mean, timeouts raise it) |
| retries | `http.request.resend_count` (stable [V: https://opentelemetry.io/docs/specs/semconv/registry/attributes/http/]) | not read | [new] `trace.retries` when present on more than 1% [D] of CLIENT spans: retries make calls per visit grow with load, which the model reads as a constant |
| code | `code.function.name`, `code.file.path` | not read | kept for research only ([CODEPATHS.md](CODEPATHS.md)) |

### 3.6 Span kinds, linking and context propagation

[built] (`internal/infer/infer.go`): a **SERVER** span is a visit that holds a worker, and a service is a
`service.name` with SERVER spans. A **CLIENT** span is a call of its nearest SERVER ancestor, matched to its callee
only through the parent link (a SERVER span whose nearest SERVER or CLIENT ancestor is that CLIENT span). A CLIENT
span with no SERVER span under it is an outside delay `ext:<peer>`, unless its address is answered by a service
elsewhere in the capture, in which case it is a lost span. INTERNAL, PRODUCER and unspecified spans are walked
through; a CONSUMER span ends the walk and its calls become entries. A SERVER span whose absent parent belongs to a
service with more than half of its spans uncalled is an entry (`entryLikeShare`, `infer.go:1876`).

Three defects give a confident wrong answer today, with no warning that names the cause:

| defect | what happens today | contract [new] |
|---|---|---|
| **no CLIENT spans** on an edge (the callee's SERVER span is parented directly to the caller's SERVER span) | the callee's wait before its span is read as the caller's own work: callers read 13-35% high in a test, worker counts are reported as identified when they are not, every hop is 0 | `trace.client_spans_missing` (doubt) per edge where more than 5% [D] of calls lack a CLIENT span; the services on that edge lose "identified" worker counts |
| a callee that **does not propagate the trace context** (its SERVER spans start new traces) | the callee becomes an entry **and** an outside delay at its caller; the entry rate doubles; a change at that callee does not reach the entry; one vague warning | `trace.context_not_propagated` (refuse) when a service's SERVER spans are over 95% roots while at least 20 CLIENT spans whose peer names it, or whose time window contains them far above a shuffled baseline, have no SERVER span under them [D]; the fix is to propagate W3C `traceparent`; a cluster dump allows re-joining by address [later] |
| **duplicate spans** (the same trace and span id twice) | the first copy is kept (`spanIndex`, `infer.go:1583-1591`) and the capture is refused with the misleading "N of N traces miss spans" | byte-identical copies are dropped and counted (`trace.duplicate_spans_dropped`, note: window margins and exporter retries produce them); the same ids with different content refuse (`trace.span_id_collision`) |

Partial loss is checked per edge: caller CLIENT calls against callee SERVER visits; more than 1% [D] lost on an edge
raises `trace.edge_loss` (doubt), even when broken traces are under the 5% refusal.

### 3.7 Clocks

[built] A call with a CLIENT span is timed on the caller's clock; one without, on the callee's. An edge is async when
at least 95% of its calls start after the caller's span ends (warned between 5% and 95%). A first call that starts
more than 0.25 ms before its own server span on more than 1% of visits is warned, with the direction of the error
(`infer.go:1351-1421`). Pods whose span starts fall on whole milliseconds (Node.js) are detected (`quantized.go:43-66`:
at least 100 spans, 99% on whole milliseconds) and read by their callees' span starts. A negative rebuilt service time
refuses (`infer.go:815-824`).

Contract [new]: the warnings become `trace.clock_skew` and `trace.whole_ms_starts` (doubts); a detector also flags
callee SERVER spans that fall outside their caller's CLIENT span, per pod pair. Required: NTP on every node, or CLIENT
spans on every call.

### 3.8 Sampling

The queue rebuild needs every request; a kept share repairs the rates, not the rebuild. Today sampling is **not
detected** and `--kept-share` divides only the per-service rates and the external rate (`infer.go:302-307`, `:810`,
`:1181`). A capture that kept half its traces reads half the load (in a test: 13% busiest utilization instead of 26%)
with no warning.

Why the obvious detectors do not work:
- Span exporters receive only sampled spans, so every exported span carries the sampled flag
  [V: https://opentelemetry.io/docs/specs/otel/trace/sdk/].
- A consistent head sampler drops whole traces, so a sampled capture has no gaps: it looks like a quieter, complete
  one.
- The `ot=th:` threshold in `tracestate` is written only by the newer probability samplers; the legacy
  `TraceIdRatioBased` sampler (behind `parentbased_traceidratio`) is deprecated and writes none
  [V: https://opentelemetry.io/docs/specs/otel/trace/tracestate-probability-sampling/]. That spec is in Development
  status.
- With Amazon CloudWatch Application Signals enabled, the SDK samples with X-Ray centralized rules by default
  (1 request per second plus 5%) [V:
  https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/Application-Signals-SampleRate.html]: neither a
  constant share nor marked with `th`.
- Forking the Collector before its samplers does not undo sampling done in the SDK.

The contract [new]:

| situation | result |
|---|---|
| an independent count of entry requests for the window (I4) agrees with the entry SERVER spans within 5% [D] | the capture is unsampled; kept share 1, source `counted` |
| the independent count is higher | kept share = entry spans / count, source `counted`; plus `fit.queue_rebuild_sampled` (doubt): worker counts are forced to lower bounds unless `--workers` is given |
| every entry SERVER span carries the same `ot=th:` value | kept share from the threshold (`th:c` is 25%), source `derived-th`; plus `fit.queue_rebuild_sampled` when below 1 |
| `th` values differ, or some entries lack one, and 1% [D] or more of the entries do not carry the most common value (a missing `th` counts as a value) | `trace.sampling_mixed` (refuse): one constant share is required |
| the same, but under 1% of the entries do not carry the most common value | `trace.sampling_minority` (doubt): their `th` is ignored, the most common value gives the kept share as in the rows above, and the finding counts them (`entries_ignored`) without listing their values. Under a ParentBased sampler an entry span's `tracestate` is the caller's inbound header, so one request from outside must not refuse a window. The 1% is also the cost a caller outside can impose: anyone who sends 1% or more of a window's requests with a forged `tracestate` (`ot=th:...`) makes that window refuse with `trace.sampling_mixed`, and so every fit of it (an environment's baseline, a capture, an A/B arm); an independent count does not lift the refusal. Where callers outside can reach an entry, drop or reset the inbound `tracestate` at the edge (a gateway, or the entry service's propagator configuration) |
| a declared SDK sampler configuration per service (`always_on` or `parentbased_always_on` on the capture path) and no count | kept share 1, source `declared`; `trace.sampling_unverified` stays as a doubt |
| none of the above | kept share 1, source `assumed`; `trace.sampling_unverified` (doubt; in the policy's default `doubts_that_hold` list, so inconclusive in every gate) |
| a declared tail, adaptive, reservoir or rate-limited sampler (including the X-Ray centralized sampler) | `trace.sampler_not_proportional` (refuse): its share varies with time and cannot be corrected |
| a declared share that disagrees with a derived one | `trace.kept_share_conflict` (refuse) |

The existing `--kept-share` flag remains as a declaration, recorded in the report.

### 3.9 Window, cut and margins

- **One steady window, cut by the entry span's start.** A trace belongs to the window when its entry SERVER span
  starts inside it; its later spans (an async consumer, a delayed job) are read past the window's end. Cutting by span
  end keeps streams that began earlier; cutting by "all spans start inside" drops async tails and makes async rates
  read low. [built]: the window length is read from the spread of entry starts times n/(n-1) (`infer.go:559-563`);
  the repository's `otelcap cut` tool cuts by trace start.
- **Margins** [new]: object partitions follow export time, not span time, and an export can be late by the
  Collector's retry time (the exporter helper retries for up to 300 s by default [V:
  https://github.com/open-telemetry/opentelemetry-collector/blob/main/exporter/exporterhelper/README.md]) plus its
  batch timeout plus the SDK's export delay. The fetcher reads 2 minutes before the window and, after it, that sum
  from the Collector configuration (default 7 minutes [D]); it then checks that the last margin minute holds no span
  starting inside the window, extending once if it does, else `store.late_batches` (doubt).
- **Async tails**: the share of traces still open at the end of the margin is reported; above 1% [D]
  `trace.async_tail_truncated` (doubt).
- **No rollout, restart, autoscaling step or node change inside the window.** Detected [new] from two digests per
  service (`trace.mixed_images`), pods seen for only part of the window (`trace.pod_churn`, doubt), and, with a
  cluster dump, start and end dumps that differ. [built] With `check --window S,E`, `trace.mixed_images` counts the
  digests of the resources that count in the window only (`internal/check/detect.go`, `inWindow`): a resource counts
  when it holds a span of a trace whose entry starts in [S, E), which the cut keeps whole (`Cut`), a span that
  overlaps [S, E) (it starts before E and ends at or after S, so a span that straddles S counts even when the cut
  drops its trace), or no span (the cut keeps such a resource). The check of the window so refuses every two images
  that the check of its cut would. An image whose spans all end before S, in traces that entered before S, read in
  the margin before the window, does not count. Two digests on one resource (the same service, pod and image name,
  which the reader merges) count whatever the window. `trace.image_digest_missing`, a check without `--window`, the
  check of a cut and `fit` read every resource, as before; `--write-manifest` writes the capture's manifest only after
  a check of the whole capture admits it.

### 3.10 Streams and long spans

[built] A SERVER span lasting at least 1 s, at least 10% of the capture's span and 1000 times its service's median is
"not a visit"; when such spans hold 5% or more of a service's busy time the capture is refused
(`internal/infer/longlived.go:43-45`, `:171-195`). Shorter streams pass silently: a 200 ms server-stream on a 5 ms
service would be read as a visit that holds a worker.

[new] A SERVER span with more than one `rpc.message` SENT or RECEIVED event [K], or an HTTP 101 answer, is a stream:
`trace.streaming_rpc` (refuse for that service, or exclude with a count when the team declares the route a stream).

### 3.11 How much data

| quantity | rule | source |
|---|---|---|
| entry requests | at least **100**, else refused | [built] `MinRequests`, `infer.go:308-313`, `:556-558` |
| broken traces | up to 5% with a warning; above 5% refused, the causes named | [built] `maxBrokenShare`, `infer.go:1277`, `:1281-1307` |
| visits per service | under 30: a warning that its estimates are rough | [built] `infer.go:770-772` |
| server spans per pod | under 20: no queue rebuild for that pod | [built] `minPodJobs`, `internal/infer/fifo.go:38` |
| identified worker count | at least 100 visits that waited, materially | [built] `identMinWaits`, `fifo.go:36` |
| calls on an edge for its own network time | at least 50 | [built] `hopMinCalls`, `internal/infer/hops.go:27` |
| call shapes per service | at most 12, the rarer ones under 5% | [built] `maxShapes`, `shapeTail`, `internal/infer/counts.go:51-52` |
| steadiness judgement | at least 30 sub-windows of at least 30 arrivals each [D], else `trace.arrivals_unjudged` (doubt: "cannot tell" is its own finding) | [new] |
| a gating window | recommended at least 10 minutes and 20 entry requests/s [D]; near 80% utilization a longer window helps more than anything else | [new] guidance |
| representativeness | the window's entry rate is recorded next to a reference (for example the 7-day p50 and p95 of the load balancer count); below the policy's minimum, `trace.window_unrepresentative` (doubt) | [later]: needs the reference counts |

### 3.12 Load conditions

- **Below the utilization limit** (`--max-rho`, default 0.85, at most 0.95), at the observed load and after the
  change [built]. Above the **validated** load, 0.58 by default, `load.beyond_validated` [new] (ARCHITECTURE.md, 4.6).
- **Steady and roughly Poisson.** [new] `trace.load_trend` (a trend in entry arrivals) and
  `trace.arrivals_overdispersed` (counts per sub-window far more variable than Poisson) are doubts, with thresholds set
  on the project's simulator [D]. They are **detected, never corrected**.
- **Calls that arrive together** (calls made at once in a loop) are batch arrivals: the callee's wait, and the latency
  of whoever waits for it, is understated, by 0.2% to 62% in the simulator ([ACCURACY.md](../ACCURACY.md))
  [built: warned]. [new] The warning becomes `model.batch_arrivals` (doubt).
- **Not modelled and not detectable from traces**: round-robin, least-request or power-of-two balancing (modelled as
  random, so the predicted wait is too high); a server that holds its worker after its first call out; CPU
  throttling; retries and timeouts that leave no span. These are listed as `not-checkable` in every assumptions list.
- **Gating load**: the busiest observed window of the last 7 days, not a multiplier on a quiet window. A multiplier
  beyond the busiest observed load is a what-if marked `extrapolated_load: true`, never gating.

### 3.13 What no check can see

Listed in every assumptions list as `not-checkable`:
- balancing policy (above);
- held workers after the first call out;
- uninstrumented outbound clients: a database or cache call with no CLIENT span folds its time and its queue into the
  caller's own time, so a slower database reads as the caller's own change. Ask for an instrumentation inventory
  (section 16);
- a capture with no captured top that lost more than half of the calls into one service (read as entries);
- CPU throttling, unless resource metrics (I5) are supplied;
- tail sampling that the team did not declare.

### 3.14 Finding codes for traces

Level: **R** refuse, **D** doubt, **N** note. "Today" is the behaviour at 1a2df09. Status: **[new]** is built in the
first slice; **[coded]** turns an existing English message into the code without changing the rule; **[later]** is
registered but not raised yet. The table is generated from the registry (section 19 says how); codes in one row
share every column. Some refusals are raised only when `fit` reads the traces (`trace.broken_traces` and the
inference's other refusals), so a capture `check` admits can still be refused by `fit`: judge a fit by its own exit
status.

<!-- gencontract:begin capture-codes (generated from internal/contract/codes.go by `go run ./tools/gencontract docs`; edit the registry, not this table) -->
| code | level | condition | fix | today | status |
|---|---|---|---|---|---|
| `store.unreadable_object` | R | an object with a suffix the reader does not parse | export with marshaler: otlp_json, compression: gzip | skipped silently | [new] |
| `store.compression_mismatch` | R | a .gz object that is not gzip | fix the exporter | read error | [new] |
| `store.manifest_mismatch` | R | a file's sha256, size or count differs from the capture manifest | re-fetch the window; never edit a capture in place | n/a (no manifest today) | [new] |
| `store.too_large` | R | the listing or the inflated stream passes a cap | a capture over its cap: a shorter window; an object over its cap: smaller objects (rotate the Collector's files sooner, or send smaller batches) | no limit | [new] |
| `input.too_large` | R | the inflated stream passes a reader cap: its bytes, a line, the span count or a kept attribute value | a value or a line too long: shorten it in the Collector (an OTTL truncate_all) or split the batch; too many bytes or spans: a shorter window or smaller files | no limit | [new] |
| `store.partition_tz_offset` | R | partition minutes differ from span starts by whole hours | set s3_partition_timezone: UTC | not detected | [new] |
| `store.late_batches` | D | spans of the window found in the last margin minute | raise the margin; check the Collector's export errors | not detected | [new] |
| `store.small_batches` | N | more than 30 [D] objects per Collector-minute; the note carries the monthly request cost | raise the batch timeout to 5-10 s | not detected | [new] |
| `store.query_api_incomplete` | R | the source is a trace backend's search API (Tempo, Jaeger, SaaS) | tee the Collector pipeline to S3 or files | n/a | [later] |
| `trace.bad_json` | R | an undecodable value, with file and line, from either decoder path (3.2) | re-export; do not edit by hand | bad OTLP/JSON: ... or \<path>: ... | [new] |
| `trace.jsonl_pretty`, `trace.json_array` | R | pretty-printed values in .jsonl; one array of requests | write one request per line | bad OTLP/JSON | [new] |
| `trace.partial_tail_dropped` | N | an unterminated last line of the newest object | none needed | whole capture refused | [new] |
| `trace.snake_case_keys`, `trace.pre_v1_otlp`, `trace.not_otlp` | R | resource_spans; instrumentationLibrarySpans; Jaeger UI, Zipkin or X-Ray JSON (named) | use a current Collector with the matching receiver | no usable spans | [new] |
| `trace.ids_base64`, `trace.trace_id_64bit`, `trace.ids_mixed` | R | id encodings other than OTLP/JSON hex | export with the Collector's OTLP/JSON exporters | every span skipped; no usable spans | [new] |
| `trace.kind_enum_name`, `trace.kind_unknown` | R | string span kinds | export with the Collector's OTLP/JSON exporters | whole capture refused | [new] |
| `trace.time_not_integer` | N | float or exponent times, normalized | none needed | read approximately | [new] |
| `trace.spans_skipped` | D | more than 0.1% [D] of spans skipped for missing ids or times | fix the exporter | a warning | [new] |
| `trace.service_name_not_string`, `trace.service_name_default` | R | non-string or unknown_service* names | set OTEL_SERVICE_NAME | skipped, or merged silently | [new] |
| `trace.mixed_environments` | R | more than one deployment environment | one environment per prefix | not detected | [new] |
| `trace.no_server_spans` | R | coded | instrument the servers | refused | [coded] |
| `trace.duplicate_spans_dropped` | N | byte-identical repeats dropped | none needed | misleading refusal | [new] |
| `trace.span_id_collision` | R | same ids, different content | find the second writer | misleading refusal | [new] |
| `trace.client_spans_missing` | D | per edge, more than 5% [D] of calls lack a CLIENT span | enable client instrumentation | silent, 13-35% high in a test | [new] |
| `trace.context_not_propagated` | R | a callee whose SERVER spans are almost all roots while calls address it | propagate W3C traceparent | silent double count | [new] |
| `trace.edge_loss` | D | more than 1% [D] of calls on an edge lost | check exporter queues and SDK drops | silent below 5% | [new] |
| `trace.broken_traces` | R | coded: more than 5% of traces miss spans, edges named | as the message says | refused in English | [coded] |
| `trace.sampling_unverified` | D | no independent count and no proportional-sampler evidence | supply a request count (I4) | silent | [new] |
| `trace.sampling_mixed`, `trace.sampler_not_proportional`, `trace.kept_share_conflict` | R | section 3.8 | one constant head-sampling share, or none | silent | [new] |
| `trace.sampling_minority` | D | under 1% [D] of the entries carry a th other than the dominant one, a missing th counting as one: their th is ignored (section 3.8) | none needed: a th on a few entries is usually a caller's tracestate, which a ParentBased sampler keeps | silent | [new] |
| `fit.queue_rebuild_sampled` | D | kept share below 1 | capture every request for gating windows | silent | [new] |
| `trace.rate_mismatch` | D | the trace entry rate differs from the independent count by more than 5% [D] | find the dropped spans or the sampler | n/a | [new] |
| `trace.image_digest_missing` | D | no container.image.repo_digests on a service's resource | run k8sattributes with container.image.repo_digests extracted, and keep container.id or k8s.container.name on resources so that it can pick the container in a pod with sidecars (4.1) | n/a | [new] |
| `trace.mixed_images` | R | two digests for one service in the window | a window after the rollout settled | not detected | [new] |
| `trace.instance_per_process` | D | several instance ids of one service on one host, and no pod or task identity | add k8s.pod.name or aws.ecs.task.arn; or declare --pod-identity host when the processes share one listening socket | silent misread | [new] |
| `trace.pod_shared` | D | one pod identity carries SERVER spans of two services | if it is not a sidecar pod, fix the pod association (4.1) | silent | [new] |
| `trace.pod_identity_collapsed` | R | one pod identity carries SERVER spans of three or more services [D] | associate pods by k8s.pod.uid set upstream, or export directly (4.1) | silent misread: all pods read as one | [new] |
| `trace.faas_service` | R | SERVER spans from a function platform | not modelled | silent misread | [new] |
| `trace.probe_like_traffic` | D | probe-like routes above 1% [D] of entries | declare probe routes, or filter them in the Collector | counted as user traffic | [new] |
| `trace.errors_present`, `trace.shedding`, `trace.retries` | D | section 3.5 | fix the errors, or capture a quiet window | silent | [new] |
| `trace.streams` | R | coded: long spans at 5% or more of busy time | cut the window by trace start | refused in English | [coded] |
| `trace.streaming_rpc` | R | message events show a stream | exclude the route, or cut the window | silent | [new] |
| `trace.links_present` | D | CONSUMER spans with links | none in this version | ignored | [new] |
| `trace.clock_skew`, `trace.whole_ms_starts` | D | coded from today's warnings, plus the per-pod-pair detector | NTP, or CLIENT spans | warning text | [coded] |
| `trace.load_trend`, `trace.arrivals_overdispersed`, `trace.arrivals_unjudged` | D | section 3.12 | a steady window; a longer window | not detected | [new] |
| `trace.pod_churn` | D | a pod present for part of the window | a quiet window | not modelled, silent | [new] |
| `trace.async_tail_truncated` | D | traces still open at the margin's end | raise the margin | silent | [new] |
| `trace.window_unrepresentative` | D | the window's load is below the policy's minimum relative to the reference | gate at the busiest window | n/a | [later] |
| `trace.peer_renamed` | N | a CLIENT peer given as an address was renamed to a service from the cluster dump or the peer map (section 5) | none needed | the address stays an outside delay | [new] |
| `trace.personal_data_suspect` | D | a kept string value looks like an e-mail address, a JWT or a card number; it is masked (section 15) | tighten the privacy list in the Collector | kept as is | [new] |
| `trace.too_few_entries` | R | coded: fewer than 100 entry requests | a longer window | refused in English | [coded] |
| `trace.cycle`, `trace.too_many_shapes`, `trace.negative_service_time` | R | coded from today's refusals | as the message says | refused in English | [coded] |
| `model.utilization_observed`, `model.utilization_after` | R | coded from limitError | --workers when the count is a lower bound | refused in English | [coded] |
| `model.batch_arrivals` | D | coded from today's warning | none in this version | warning text | [coded] |
| `load.beyond_validated` | D | a service on a gated path above max_utilization_validated (refuse-like in enforce mode) | none: the model is untested there | n/a | [new] |
<!-- gencontract:end capture-codes -->

Normalization policy: in the first build, only lossless, unambiguous cases are normalized (identical duplicates, the
newest object's partial tail, float times). Base64 ids, enum-name kinds, snake_case keys and arrays are **refused with
a fix**. Normalizers for those come later, each with a neutrality test: predictions on the normalized form must equal
the canonical form byte for byte.

---

## 4. Producing traces, and where they live

### 4.1 The capture Collector (recommended)

Run a dedicated capture Collector, fed **before any sampler**, in one of the two modes below. Do no disk I/O on the
team's own Collectors. These configurations are examples and were not run as part of this repository; check every
key against your Collector version [K]. `awss3exporter` is alpha and is in the contrib distribution
(`otelcol-contrib`); it is **not listed** in the AWS Distro for OpenTelemetry component table
[V*: https://github.com/aws-observability/aws-otel-collector, table read as flattened text; confirm with the binary's
`components` command]. Teams on ADOT need the contrib distribution, a custom build, or the file exporter plus an upload.

**Where pod identity comes from.** The queue rebuild needs each span's pod, and the pod must come from the platform,
not from the application. The two ways of feeding the capture Collector get it differently:

| mode | who sends to the capture Collector | how each span's pod is found | sender identity deleted where |
|---|---|---|---|
| **direct** | the SDKs in the workload pods | the capture Collector's `k8sattributes` with `pod_association` `from: connection`: the connection's source address is the pod's IP | at the capture Collector, before `k8sattributes` |
| **forwarded** | the team's agent Collectors (one per node, as a DaemonSet), through a second exporter placed before any sampler | each agent Collector runs the direct-mode steps for the pods on its own node and so sets `k8s.pod.uid`; the capture Collector authenticates each agent, keeps what it set, and (only if it needs metadata the agents did not extract) re-associates `from: resource_attribute` with `name: k8s.pod.uid` | at each agent Collector; the capture Collector deletes nothing |

`from: connection` is valid **only** where the workload's SDKs connect directly. Behind a gateway or any forwarding
Collector the connection's address is that Collector's pod: every span would get the Collector's pod name, all pods
would collapse into one, and the queue rebuild would be silently wrong. `trace.pod_identity_collapsed` (3.4) refuses
such captures, and `trace.pod_shared` flags the two-service case. [K: the `connection` and `resource_attribute` association sources; check them against your version.]

**Containers.** In a pod with sidecars the connection identifies the pod, not the container. `k8sattributes` needs
`container.id` (written by the SDK's container resource detector) or `k8s.container.name` (set through the pod spec)
on the resource to pick the container whose image it reports [K]. Both are therefore kept, not deleted. A sender can
at most name another container of its own pod, because the pod itself comes from the connection.

Direct mode:

```yaml
extensions:
  bearertokenauth/capture:            # authenticate every sender; mTLS on the receiver is the alternative
    filename: /etc/otel/capture-token

receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        auth: {authenticator: bearertokenauth/capture}

processors:
  # 1. Identity comes from the platform, not from the sender: delete what the SDK claimed about its pod and image...
  resource/drop_sender_identity:
    attributes:
      - {key: k8s.pod.name, action: delete}
      - {key: k8s.pod.uid, action: delete}
      - {key: k8s.deployment.name, action: delete}
      - {key: k8s.namespace.name, action: delete}
      - {key: container.image.name, action: delete}
      - {key: container.image.tag, action: delete}
      - {key: container.image.tags, action: delete}
      - {key: container.image.repo_digests, action: delete}
      # container.id and k8s.container.name are kept: they only choose a container inside the pod
      # that the connection identifies
  # ...then re-add it from the connection's source address. Direct mode only (see the table above).
  k8sattributes:
    pod_association:
      - sources: [{from: connection}]
    extract:
      metadata: [k8s.pod.name, k8s.pod.uid, k8s.deployment.name, k8s.namespace.name, k8s.node.name,
                 k8s.container.name, container.id, container.image.name, container.image.tag,
                 container.image.repo_digests]
  # 2. Privacy list (section 15): what may leave the cluster. It may be narrower than rhobound's canonical
  #    keep-list, never wider. Check the OTTL function names against your version [K].
  transform/privacy_list:
    trace_statements:
      - context: resource
        statements:
          - keep_keys(attributes, ["service.name", "service.namespace", "service.version", "service.instance.id",
              "deployment.environment.name", "host.name", "host.arch", "k8s.pod.name", "k8s.pod.uid",
              "k8s.deployment.name", "k8s.namespace.name", "k8s.node.name", "k8s.container.name",
              "aws.ecs.task.arn", "container.id", "container.image.name", "container.image.tag",
              "container.image.tags", "container.image.repo_digests", "telemetry.sdk.language",
              "telemetry.sdk.version"])
      - context: span
        statements:
          - keep_keys(attributes, ["http.route", "http.request.method", "http.method", "http.response.status_code",
              "http.request.resend_count", "rpc.method", "peer.service", "server.address", "server.port",
              "net.peer.name", "net.peer.port", "http.host", "user_agent.original", "code.function.name"])
  batch:
    timeout: 10s                      # one S3 PUT per batch: 5-10 s keeps requests to cents a day
    send_batch_size: 8192

exporters:
  awss3:
    s3uploader:
      region: us-east-1
      s3_bucket: example-traces
      s3_prefix: env=prod             # one prefix per environment; the server maps names to prefixes
      s3_partition_format: "year=%Y/month=%m/day=%d/hour=%H/minute=%M"
      s3_partition_timezone: UTC      # the default is Local
      compression: gzip               # the default is none
      unique_key_func_name: uuidv7    # the default is a random integer; several Collectors write the same minute
    marshaler: otlp_json

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [resource/drop_sender_identity, k8sattributes, transform/privacy_list, batch]
      exporters: [awss3]
```

Forwarded mode: each agent Collector runs `resource/drop_sender_identity` and `k8sattributes` exactly as above (its
own SDK connections are direct) and adds an exporter to the capture Collector before any sampler, authenticated with
a credential per agent. The capture Collector runs the same receiver, then `transform/privacy_list`, `batch` and the
exporter, with no deletion step, and adds a `k8sattributes` with
`pod_association: [{sources: [{from: resource_attribute, name: k8s.pod.uid}]}]` only if the agents extract less
metadata than the list above.

Notes on these configurations:
- The privacy list above removes `url.path`, `http.target`, `http.path` and `http.url`, which carry user-specific
  paths. Probe detection then relies on `http.route`, the span name and `user_agent.original`; a team that needs
  path-based probe detection keeps `url.path` with the query stripped, and accepts that it is personal-data-bearing.
  This is a choice made before rhobound reads anything: once spans are stored, rhobound's own steps never remove a
  key the reader uses (15), so the cut cannot move a number.
- Recording the Collector's configuration hash and its mode (`direct` or `forwarded`) in the capture manifest (I2)
  lets a fit show which pipeline produced it.
- On ECS there is no `k8sattributes`: use the ECS resource detector for `aws.ecs.task.arn` [K] (rhobound keeps only
  the task's id from it), and authenticate the receiver.
- The SDKs on the capture path use a non-sampling sampler (`OTEL_TRACES_SAMPLER=parentbased_always_on` [K]) or a
  probability sampler that writes `th`; with Application Signals' default sampler, supply an independent count (I4)
  and expect `fit.queue_rebuild_sampled`.

### 4.2 Files, for local use

The `file` exporter with `format: json` writes what the reader reads. Do not use its native `compression: zstd`;
gzip the files afterwards. Put them in `CAPTURE/traces/` and write a manifest with `rhobound check --write-manifest`
[new]. The existing example in [REQUIREMENTS.md](../REQUIREMENTS.md#an-example-opentelemetry-collector-configuration)
remains valid for local captures.

### 4.3 What the fetcher does with an S3 window [new]

1. Map the environment name to its bucket and prefix from server configuration (never from a tool argument); the
   prefix ends in `/`.
2. List the minute partitions for the window plus the margins (3.9). Estimate the size from the listing; refuse
   above the cap before any download.
3. Sniff the first kilobytes of every object: suffix, compression, framing, top-level shape, id and kind encoding.
   A sniff refusal stops before anything is downloaded in full.
4. Download in parallel into a **fresh, random, per-invocation folder**, with names derived from the key path, and
   check that the number of files equals the manifest's count.
5. Cut whole traces by their entry span's start; drop identical duplicates; apply the canonical keep-list (15), which
   keeps every key the reader uses, so this step cannot change a number. The canonical folder lives only for the
   invocation, like the downloaded objects.
6. Write the manifest (I2), then run the structural checks and the fit.

### 4.4 Other stores

| store | status | how to use it |
|---|---|---|
| S3 via `awss3exporter`, `otlp_json` + gzip | **canonical** | as above |
| Collector `file` exporter, JSON | accepted | local and test use |
| S3 `otlp_proto` or zstd objects | refused (`store.unreadable_object`) | the Collector's `awss3receiver` can re-read them into a `file` exporter [K]; a converter is [later] |
| S3 Express One Zone | possible "faster" option | lower request latency; `awss3exporter` support for directory buckets is [U] |
| ClickHouse (`otel_traces` table) | converter [later] | kinds are strings and attributes are string maps; end = start + `Duration` [V: the exporter's DDL] |
| Zipkin v2 JSON | refused (`trace.not_otlp`); converter [later] | prefer the Collector's `zipkin` receiver into the file exporter; 64-bit trace ids and shared client/server span ids are the traps |
| Tempo, Jaeger query APIs, SaaS trace APIs | refused (`store.query_api_incomplete`) | searches are not exhaustive exports; tee the Collector pipeline to S3 instead |
| CloudWatch `aws/spans`, X-Ray APIs | refused (`store.paged_api`); research converter | retrieval pages a few traces at a time; add an S3 exporter to the same Collector |
| CloudWatch Application Signals | not a capture (`store.metrics_only`) | a verification source only, after an equivalence test (section 12) |
| Athena or Parquet | not an input (`store.not_spans`) | useful for ad-hoc questions |

The four codes of these sources are registered, but the first slice has no reader for any of them and raises none:
a configured environment is a folder or an S3 prefix of the Collector's objects (4.3), so these sources cannot be
named. They are raised when a source kind is added to the configuration [later].

### 4.5 The capture manifest (I2) [new]

```json
{
  "schema": "rhobound.capture-manifest/v1",
  "source": {"kind": "s3", "env": "prod", "prefix_digest": "sha256:<of the configured prefix>"},
  "window": {"start": "2026-10-26T12:00:00Z", "end": "2026-10-26T12:10:00Z", "cut": "entry_span_start"},
  "margins_s": {"before": 120, "after": 420, "after_derived_from": "retry 300 s + batch 10 s + sdk 5 s + 105 s slack"},
  "partition_timezone": {"configured": "UTC", "derived_offset_h": 0},
  "objects": [{"name": "year=2026_month=10_day=26_hour=12_minute=00_traces_<uuid>.json.gz",
               "size": 812345, "etag": "<etag>", "sha256": "<hex>"}],
  "dropped": {"duplicate_spans": 12, "partial_tail_lines": 0, "traces_outside_window": 431},
  "collector": {"config_sha256": "<hex>", "distribution": "otelcol-contrib", "version": "<version>",
                "mode": "direct"},
  "keep_list_sha256": "<hex>",
  "sampling": {"evidence": "counted", "independent_count": "sha256:<I4 digest>", "declared_sdk_samplers": {}},
  "cluster_dump": {"layout": "start-end",
                   "start": {"digest": "sha256:<digest>", "taken_at": "2026-10-26T11:59:40Z"},
                   "end": {"digest": "sha256:<digest>", "taken_at": "2026-10-26T12:10:15Z"}},
  "limits": {"max_object_bytes": 1073741824, "max_capture_bytes": 8589934592}
}
```

The capture's identity is the sha256 of the canonical manifest. It goes into the fit, the report, the ledger and any
measured record that cites the capture. The manifest names no bucket: the prefix is held in server configuration and
only its digest is recorded, so reports and attestations do not disclose storage locations.

---

## 5. Cluster and architecture dump (I3) [new]

Optional, recommended. Scrubbed before it leaves the cluster. A Kubernetes `List` carries no time of its own, so the
time each dump was taken is recorded beside it (in the capture manifest, `cluster_dump.*.taken_at`, written by the
tool that takes the dump).

**EKS / Kubernetes.** Two layouts are read, both made of Kubernetes `List` objects as `kubectl get ... -o json`
writes them, each folder read in file-name order:

| layout | files | meaning |
|---|---|---|
| start and end | `CAPTURE/cluster/start/*.json` and `CAPTURE/cluster/end/*.json` | dumps taken at the window's start and end: replicas, pods and images can be compared across the window |
| one snapshot | `CAPTURE/cluster/*.json` (a flat folder, as simpler capture tools write it) | one dump; read as both start and end, with the note `cluster.single_snapshot`: changes inside the window cannot be seen from it |

When both a flat folder and `start/`/`end/` exist, the flat files are refused (`cluster.layout_ambiguous`). A file
that is not JSON is refused by name, never skipped.

| kind | fields read | used for |
|---|---|---|
| `Service` | `metadata.name`, `metadata.namespace`, `spec.clusterIP` (headless `None` skipped), `spec.ports[].port` | naming CLIENT peers reported by ClusterIP (a peer `<ClusterIP>:<port>` becomes the Service's workload, `trace.peer_renamed`, note); detecting, and later re-joining, a callee that drops the trace context |
| `Deployment` | `metadata.name`, `spec.replicas`, `spec.template.spec.containers[].{name, image, resources}` | replica counts against the pods seen in the spans; the container spec for environment matching |
| `Pod` | `metadata.name`, `metadata.creationTimestamp`, `spec.nodeName`, `status.containerStatuses[].{name, imageID, restartCount}` | the running image digest per service (cross-checked with `container.image.repo_digests` in the spans; a disagreement is `cluster.digest_disagrees`, doubt); pods created or restarted inside the window |
| `Node` | `metadata.labels["node.kubernetes.io/instance-type"]`, `["kubernetes.io/arch"]`, `status.nodeInfo.{kernelVersion, architecture}` | the environment record: instance type and architecture, for digest resolution and profile matching (9.5, check 9) |
| `HorizontalPodAutoscaler` | `status.currentReplicas`, `status.desiredReplicas` | autoscaling inside the window |

Codes: `cluster.not_list` (refuse: a file that is not a `List`, or not JSON); `cluster.unscrubbed` (refuse: `Secret`
or `ConfigMap` objects, container `env` values other than non-secret `OTEL_*` keys, `managedFields`, or the
last-applied annotation are present); `cluster.window_mismatch` (refuse: a recorded `taken_at` more than a minute [D]
outside the window); `cluster.time_unknown` (note: no `taken_at` recorded; the window check is skipped);
`cluster.single_snapshot` (note); `cluster.layout_ambiguous` (refuse); `cluster.digest_disagrees` (doubt);
`trace.pod_churn` (pods differ between start and end, or were created inside the window); `trace.peer_renamed`
(note). On captures without a `cluster/` folder nothing changes: no number moves.

**ECS:** a `rhobound.peer-map/v1` file, `CAPTURE/peer-map.json`: `{"schema": "rhobound.peer-map/v1", "taken_at":
"...", "addresses": {"<ip:port or DNS name>": "<service.name>"}, "desired_count": {"<service.name>": n},
"instance_type": {"<service.name>": "<type>"}}`, built from ECS service and task descriptions or Cloud Map / Service
Connect listings [U: no ECS source was checked]. ECS has no ClusterIPs, so peer naming relies on Service Connect or
Cloud Map names. Codes: `cluster.peer_map_invalid` (refuse), `trace.peer_renamed` (note).

---

## 6. Independent request count (I4) [new]

Required for enforce mode (it is the only check that sees SDK-side sampling); optional otherwise.

```json
{
  "schema": "rhobound.request-count/v1",
  "window": {"start": "2026-10-26T12:00:00Z", "end": "2026-10-26T12:10:00Z"},
  "source": "alb",
  "entries": [{"entry": "frontend", "count": 61234}]
}
```

Accepted sources, as independent of the trace pipeline: `alb` (load balancer `RequestCount`), `apigw` (API Gateway
`Count`), `ingress` (an ingress controller's request counter), `loadgen` (the load generator's own count in a test).
`appsignals` (`SampleCount` of Application Signals `Latency`) is accepted only as `unverified` until an equivalence
test shows whether it counts unsampled requests [U]. Counters inside the Collector (span metrics, receiver counts) are
**not** independent: they cannot see spans the SDK never exported. Health probes must be excluded from the count or
subtracted. Refused: `count.window_mismatch` (window edges more than 10 s [D] apart from the capture's),
`count.unknown_entry`. A count given to `check --cut` (`--request-count`) is the cut window's: it is checked against
the canonical capture, after the cut (4.3), and copied into it as `request-count.json`; the cut's manifest then records
its digest (`sampling.evidence: counted`, `independent_count`), so a fit of that manifest carries the digest the check
printed. A cut the count refuses is removed. A `request-count.json` in the capture's own folder counts the capture as
read, cut or not.

`rhobound count` writes this document from a source outside the trace pipeline (USAGE.md 11): an ALB access log
(`--from alb`, counted by the time the load balancer received each request, `request_creation_time`), JSON-lines
access logs with a time and a route key (`--from jsonl`: API Gateway, an ingress controller), or the JSON answer of a
Prometheus instant query over the window (`--from prometheus --label L`, `sum by (L) (increase(...[W]))` evaluated at
END). Give it the same `--window` as `check --window`, the path prefix of each entry service (`--entry frontend=/`) and
the probe routes (`--probe-route /healthz`). It writes every named entry (0 when nothing matched) and reports the
requests it did not count (probes, outside the window, no matching rule, unreadable lines): explain those before
trusting the count. The source is declared (`--source`), one of the four independent ones above; `count` never writes
`appsignals`.

## 7. Resource metrics (I5) [later]

```json
{"schema": "rhobound.resource-metrics/v1", "window": {"start": "...", "end": "..."},
 "pods": [{"pod": "checkout-7d9f-abcde", "cpu_usage_cores_mean": 0.61, "cpu_limit_cores": 1.0,
           "throttled_periods": 42, "total_periods": 6000}]}
```

From cAdvisor or Container Insights [K]. Cross-checks only, never corrections: `env.cpu_throttled` (doubt, threshold
[D]); `fit.utilization_disagrees` when the inferred utilization of a CPU-bound service and its CPU usage over its
limit disagree beyond a band to be calibrated.

---

## 8. A/B captures: the `measured-ab` label (I6) [new]

The first measured input needs no harness: the team's own A/B run of the old and the new image.

Required:
- two captures (arm A, the image production runs now; arm B, the candidate), each meeting section 3, **unsampled**
  (kept share 1 proven by the load generator's count), CLIENT spans on;
- both arms running **at the same time** (windows overlapping by at least 90% [D]), one pod per arm, the same instance
  type and container resources, at about the busiest production pod's rate;
- arm identity from pod metadata, not from the application;
- the load generator's per-request log, so each arm is also measured from outside the system under test:

```
arm,seq,sent_unix_ns,answered_unix_ns,status
A,1,1793016000123456789,1793016000131456789,200
B,1,1793016000124456789,1793016000133012345,200
```

What `rhobound diff-fit --baseline A --candidate B --out DIR [--loadgen-log L] [--baseline-name NAME]
[--candidate-name NAME]` writes to `DIR/ab-record.json` (`rhobound.ab-record/v1`; the log defaults to `loadgen.csv` in
the baseline's folder, then the candidate's; each arm is named by its name flag, else by its folder as given), per
service: own-time and glue difference with standard errors by batch means (30 batches [D]); the `cs2` difference with
its standard error; calls per visit per callee in each arm; error shares; the client-side latency per arm; the number
of visits; the capture manifest digests and each arm's image digests. The record's sha256 is its digest; the CLI keeps
it in its record store (`$RHOBOUND_STATE/ab/`), which is what lets it mint `measured-ab` later.

Findings: `ab.not_concurrent`, `ab.hardware_differs`, `ab.sampled` (refuse); `ab.errors_differ` (doubt: error shares
more than 1 point [D] apart); `profile.structure_changed` (calls per visit differ: the change is structural; the
own-time difference is never applied alone); `profile.variance_changed` (the own time's variance differs beyond 2
standard errors: inconclusive until the engine has a variance patch. `diff-fit` tests the variance, not the `cs2`: a
pure shift of own time keeps the variance and lowers the `cs2`, and is no variance change; for a service with routes it
tests against the variance the routes' own shifts predict, as shifting one route of a mixture moves the blended
variance); `profile.route_heterogeneous` (doubt: the record's `routes[]` hold each route's own-time difference, and some
route moved apart from the blend by more than 3 of its standard errors: apply the change by route, one `own_ms` item
per route, not the blend; a set that does so does not list it); `profile.self_report_mismatch` (the client-side latency
difference disagrees with the span-derived one beyond a band [D]); `ab.too_few_visits` (fewer visits than the sizing
rule of 10.2 needs for the gate's margin); `ab.count_conflict` (doubt: the load generator's log admits an arm whose own
request count gives a kept share other than 1, as one count shared by both arms does; the record is minted on the log,
its numbers unchanged, and says so).

The label is minted only by the CLI or server that computed the record. A change file that claims `measured-ab`
without a record the CLI can find and recompute is refused (`change.label_not_minted`).

---

<a id="perf-profile-v0.1"></a>

## 9. Image performance profile and its attestation (I7) [later]

This section turns the idea of "the parameters of the built image at different load levels, read from the container
registry, with attestations" into a contract. It is built only after the transfer study (EVALUATION.md) passes.

### 9.1 What it is, and what it is not

- **It is** the per-visit difference between two application images of one service, measured side by side on the
  production instance type, at several load levels: own time, glue, their variances, calls per visit, errors.
- **It is not** a property of a base image: a distroless or JDK base has no service time. A base-image update
  changes the application image's digest and is profiled like any other change.
- **It is not** a replacement for production traces: the call graph, the rates and the queues come from the fit.
- **It is not** about tails, bursts, retries, throttling, or load at or above 0.85.
- **Its transfer to production is an assumption** until the transfer study passes; every output says so
  (`transfer_validated: false`).

### 9.2 The statement

An in-toto Statement v1 [V: https://github.com/in-toto/attestation/blob/main/spec/v1/statement.md]. No vetted in-toto
predicate carries numeric performance data, so the predicate type is our own, versioned, decided once and never
renamed. Proposed: `https://github.com/Sharvil2704/rhobound-cli/blob/perf-profile-v0.1/docs/mcp/DATA.md#perf-profile-v0.1`,
where `perf-profile-v0.1` is a git tag cut when the predicate is frozen (a tag, not a branch, so the URI keeps
pointing at the text it was frozen with) and `#perf-profile-v0.1` is the explicit anchor at the start of this
section. Verifiers compare the URI as an exact string; it does not have to resolve. Illustrative values:

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [{"name": "<registry>/shop/checkout", "digest": {"sha256": "<candidate, linux/arm64 platform manifest>"}}],
  "predicateType": "https://github.com/Sharvil2704/rhobound-cli/blob/perf-profile-v0.1/docs/mcp/DATA.md#perf-profile-v0.1",
  "predicate": {
    "scope": {"statistic": "mean", "load": "steady, open-loop Poisson", "max_utilization": 0.85,
              "not_covered": ["p95/p99", "bursts", "retries", "timeouts", "cpu throttling", "utilization >= 0.85"]},
    "service": "checkout",
    "arms": [{"role": "baseline", "digest": {"sha256": "<running digest>"}},
             {"role": "candidate", "digest": {"sha256": "<subject digest>"}}],
    "pairing": {"mode": "concurrent", "run_id": "<uuidv7>"},
    "tool": {"name": "rhobound", "version": "<semver>", "digest": {"sha256": "<binary>"}, "goarch": "arm64"},
    "harness": {"image": {"sha256": "<harness>"}, "config": {"sha256": "<config>"},
                "loadgen": {"sha256": "<generator>"}, "collector": {"sha256": "<collector config>"},
                "arrivals": "poisson-open-loop", "warmup_s": 90, "dependencies": "stubbed"},
    "environment": {"platform": "linux/arm64", "instance_type": "m7g.xlarge", "cpu_model": "<from the node>",
                    "kernel": "<version>", "cpu_limit_millicores": 1000, "memory_limit_mib": 1024,
                    "container_spec": {"sha256": "<resources, limits, flags, probes only>"}},
    "sizing": {"margin_ms": 0.3, "rule": "n = 8 cs2 S^2 / m^2 per arm per level", "se_method": "batch-means/30"},
    "levels": [{
      "pod_rps": 40.0,
      "baseline":  {"visits": 9800, "utilization": 0.31, "utilization_is_upper_bound": true,
                    "own_ms": 7.42, "own_se_ms": 0.07, "own_cs2": 0.98, "own_cs2_se": 0.04,
                    "glue_ms": 0.81, "glue_se_ms": 0.02,
                    "calls": [{"callee": "inventory", "per_visit": 1.00, "parallel": false, "async": false}],
                    "workers": {"value": 4, "lower_bound": true}, "error_share": 0.0,
                    "client_latency_ms": 9.10, "client_latency_se_ms": 0.08, "cpu_ms_per_visit": 6.9},
      "candidate": {"...": "the same fields"},
      "delta": {"own_ms": 0.56, "own_se_ms": 0.10, "own_cs2": 0.03, "own_cs2_se": 0.06,
                "glue_ms": -0.01, "glue_se_ms": 0.03, "calls_changed": false, "self_report_consistent": true}
    }],
    "load_dependence": {"test": "TOST", "margin_pct_of_own": 3, "flat_shown": true},
    "harness_aa": {"false_delta_q95_ms": 0.12, "record": {"sha256": "<A/A run>"}},
    "captures": {"manifest": {"sha256": "<manifest digest>"}},
    "measured_at": "2026-10-26T12:00:00Z",
    "valid_for_days": 14
  }
}
```

Field rules:
- **Subject**: the candidate's **platform manifest** digest, because arm64 and amd64 service times differ.
- **Variance** (`own_cs2`, `own_cs2_se`) is required: a mean-only profile is refused (`profile.no_variance`).
- **Structure** (`calls`) is required per arm: a difference makes the result inconclusive.
- **Outside measurement** (`client_latency_ms` from the harness-owned load generator; `cpu_ms_per_visit` from cgroup
  accounting) is required: it is the check that the candidate, which is agent-written code, did not lower its own
  measured time without lowering its latency.
- **No storage locations, account ids or cluster ARNs** in the predicate: anyone who can pull the image can read it.
- Media types carry **no parameters**: Amazon ECR rejects manifests whose media types carry `;version=` parameters
  with 405 in an open issue [V: https://github.com/aws/containers-roadmap/issues/2306].

### 9.3 Sizing a level, and the standard errors

- Visits per arm per level for a ±2 SE band of half-width `m` on the difference: `n ≈ 8·cs2·S²/m²`. Time per level at
  utilization ρ with c workers: `t ≈ 8·cs2·S³/(m²·ρ·c)`. Examples [E]: S = 7.4 ms, cs2 = 1, c = 4, m = 0.5 ms: about 16
  s; c = 1, m = 0.2 ms: about 7 minutes; S = 40 ms, cs2 = 2, c = 1, m = 1 ms: about 85 minutes. The harness computes t
  from the fit and the gate's margin before booking machines, caps it, and above the cap answers `inconclusive` with
  the estimated minutes and dollars (`profile.window_over_cap`).
- 100 visits is the reader's refusal floor, not a design size. Visits are autocorrelated (garbage collection, JIT,
  caches), so standard errors are computed by **batch means** and the method is recorded.
- A **harness A/A** run (the same digest in both arms) per service class gives the harness's false-delta spread; the
  paired band is widened to it when it is larger.

### 9.4 Signing and storage

- cosign with an AWS KMS key (no public transparency-log entry for private images), or keyless from CI for public
  demo images (the public log then records the signer identity) [V: https://docs.sigstore.dev/cosign/key_management/overview/].
- Stored as an OCI 1.1 referrer of the candidate image in ECR (ECR serves the referrers API
  [V: https://aws.amazon.com/about-aws/whats-new/2024/06/amazon-ecr-oci-image-distribution-version-1-1]), with a copy
  in the artifact bucket, because ECR removes referrers within 24 hours of their image's deletion.
- ECR managed signing signs container images only; it cannot sign this predicate
  [V: https://docs.aws.amazon.com/AmazonECR/latest/userguide/managed-signing.html].

### 9.5 Verification, in order

Each failure is a coded refusal or doubt. The verifier runs once per pushed attestation and stores the result.

| # | check | code |
|---|---|---|
| 1 | resolve every digest to the platform manifest for the node architecture (a running pod's `imageID` or `repo_digests` entry may be the multi-platform index digest [K]); unresolvable is a refusal | `profile.digest_unresolvable` |
| 2 | a referrer exists (referrers API, else the `sha256-<hex>` tag fallback; filter by artifact type on the client side) | `profile.not_found` |
| 3 | the signature verifies, against a trust list of public keys with validity windows | `profile.bad_signature` |
| 4 | the signer is the harness identity, never the developer who pushed the image | `profile.untrusted_signer` |
| 5 | the predicate type matches exactly, version included | `profile.predicate_type` |
| 6 | the harness, load generator and Collector config digests are on the allow-list | `profile.harness_not_allowed` |
| 7 | the subject is the candidate about to run; the baseline arm is the digest the fit saw running | `profile.subject_mismatch`, `profile.baseline_not_running` |
| 8 | freshness: within `valid_for_days`, cross-checked with a witness row written at signing time in a separate ledger | `profile.stale`, `profile.time_disagrees` |
| 9 | instance type, CPU model family, CPU limit and container-spec digest equal production's (from I3) | `profile.hardware_mismatch` |
| 10 | the operating point after the change lies within the measured levels and below `max_rho`; levels are matched on per-pod requests per second at the same CPU allocation, not on inferred utilization (an upper bound when workers are a lower bound) | `profile.load_out_of_range` |
| 11 | `statistic == "mean"`; variance present | `profile.scope_not_mean`, `profile.no_variance` |
| 12 | enough visits for the gate's margin (9.3) | `profile.too_few_visits` |
| 13 | load dependence: flatness shown by an equivalence test, else nearest level only | `profile.level_drift` (doubt) |
| 14 | calls per visit equal between arms | `profile.structure_changed` (inconclusive) |
| 15 | `cs2` difference within 2 SE | `profile.variance_changed` (inconclusive) |
| 16 | the outside measurement agrees with the spans | `profile.self_report_mismatch` (inconclusive) |
| 17 | the harness baseline arm's own time against the production fit's for the same digest | `profile.transfer_gap` (doubt; the delta becomes a range over an absolute and a scaled transfer rule) |
| 18 | error shares within one point [D] | `profile.errors_differ` (doubt) |
| 19 | request mix against production's | not checkable until routes are kept in the fit; listed as `not-checkable` |

Only then does the verified delta become a change item labelled `measured-paired`, its range `delta ± 2 SE` (widened
to the harness A/A spread when larger).

---

## 10. Load levels (I8)

"Load level" means a per-pod request rate and the resulting utilization, at one CPU allocation.

- **Production's load** comes from the fit: the observed window, or for gating the busiest observed window of the
  last 7 days. Beyond that, a what-if only (3.12).
- **Measured levels** (I6, I7) are chosen from the fit: half of production's per-pod rate, production's rate, and the
  rate that brings an arm to about 0.7 utilization, capped below 0.85 [D]. Production's own rate is always on the
  ladder.
- **Levels test; they do not interpolate.** The model assumes service time does not depend on load. Flatness shown by
  TOST against a declared margin allows pooling (inverse-variance weights); otherwise the level nearest production's
  per-pod rate is used, with `profile.level_drift`.
- **Utilization read in a harness** under exclusive CPUs is not production's utilization; levels are matched on
  per-pod rate at equal CPU allocation (9.5, check 10).

---

## 11. The change description (I9)

### 11.1 Today [built]

`--patch svc=+3ms`, `svc=-2ms`, `svc=3` (bare means ms), `svc=+10%`, comma-separated (`parsePatch`,
`cmd/rhobound/main.go:157-197`). `+N ms` adds to the per-visit time that holds a worker, before the first call out: the
mean moves and the variance stays, so `cs2` becomes `cs2·S²/(S+N)²`. `+N%` scales own time and glue, keeping `cs2`.
Every node but the internal `stage:` and `shape:` nodes can be patched, including `ext:<peer>` delays. `--workers
svc=N` sets workers per pod for both the base and the change; **an unknown service name is silently ignored today**
(`main.go:118-129`). Refusals: an unknown service, an unreadable value, a negative resulting time.

### 11.2 Change sets [new]

```json
{
  "schema": "rhobound.change/v1",
  "fit": "sha256:<fit record digest>",
  "items": [
    {"op": "own_ms", "service": "checkout", "value": {"lo": 0.46, "hi": 0.66},
     "label": "measured-ab", "evidence": ["sha256:<ab-record digest>"]},
    {"op": "own_pct", "service": "cart", "value": -10, "label": "declared",
     "note": "removes a JSON re-encode per item"}
  ]
}
```

| op | status | meaning | engine mapping |
|---|---|---|---|
| `own_ms` | [new] | +ms per visit, holding a worker | today's `--patch svc=+Nms` |
| `own_pct` | [new] | ±% of own time and glue | `--patch svc=+N%` |
| `workers` | [new] | workers per pod | `--workers`, now refusing unknown names |
| `load_scale` | [new] | multiply the external rate | the model's rate input; a what-if beyond the busiest window. As an item it scales the entry's rate after the change only, so the entry's change holds the load's effect and the change's together; `predict --fit F --load-scale S` solves before and after at S and reports the level apart (`load_level_ms`, `evaluated_at: what_if`) |
| `glue_ms` | [later] | +ms after the first call out, holding no worker | a small engine change |
| `cs2` | [later] | the variance of own time | a variance patch; until it exists a measured variance change is inconclusive |
| `add_call`, `remove_call`, `set_p`, `scale_count`, `set_async`, `make_parallel` | [later] | structural changes in the topology's own vocabulary | a structural patch on the fit's topology. Each carries `callee_cost: assumed-mean-visit` (the callee's mean visit, which may differ for the endpoint the new call hits) and the label `assumed` for that cost, and raises `change.callee_cost_assumed` (doubt; it joins the policy's default `doubts_that_hold` in the release that ships these operations); in-loop or parallel added calls raise `model.batch_arrivals` |
| `own_ms` with `route` | [new] | +ms per visit of one route of the service | the fit keeps a service's routes (`routes[]`: up to 8 routes of at least 5% of its visits each, the rest as `other`; none for a service with one route) with each route's own time and who reaches it. The queue moves by the route's share of the change; each caller's calls move by the routes it reaches. A `measured-ab` item cites the record's `routes[].own_ms.delta`. |
| any other op restricted to a route | [later] | | refused: `change.route_scope_unsupported` (convert to a per-visit mean: ms times the route's share of visits) |

**Ranges** `{lo, hi}` are evaluated at their corners, oriented by the sign of each operation (ARCHITECTURE.md, 4.5:
`workers` lowers the mean as it grows, the other v1 operations raise it). A point is `lo == hi`.

**Labels** in a change file are checked, not trusted: `declared` is always accepted; any other label must name
evidence the CLI can resolve to a record it computed (an A/B record) or that the verifier stored (a profile), and the
item's value must match that record; otherwise `change.label_not_minted` (refuse).

**Services** are named as the traces wrote them or as the views show them: the views show a fit's services one to one
(ARCHITECTURE.md 5.9), so a name the name rule replaces (`svc_<hash8>`, an ARN's last element) names its service in a
change file, in `budget --service` and in `fit --workers` too. The one exception is a service whose name as the traces
wrote it is another's shown name (named `svc_<hash8>` like another's hash): the views show it as written, beside the
other, and that name means it (below). `--patch`, and `predict CAPTURE --workers`, take only the names the traces wrote,
as today.

Refused by name:
- `change.unknown_service` (also for `workers`, which is silently ignored today);
- `change.ambiguous_service`: a name the views show for more than one service, none of them named so as the traces
  wrote it (a collision of hashes); a service named like another's hash is meant by that name, and the other by its
  name as written;
- `change.negative_time`, `change.range_inverted`, `change.too_many_ranges`, `change.unsupported_op`;
- `change.free_text`: a number found in prose is never parsed;
- `change.cpu_profile_not_own_time`: a CPU profile gives CPU per call, not the time a visit holds a worker;
- `change.includes_queueing`: a canary's or a histogram's latency already contains the queue; as own time it would
  count the queue twice. It is accepted as a verification source (section 12);
- `change.microbench_unvalidated`: a microbenchmark delta is accepted only for research (label
  `estimated-microbench`) until it has been validated against measured differences for that service class.

---

## 12. After-deploy captures and verification (I12) [later]

As section 3, plus: unsampled (`verify.sampled`); every pod of the changed service runs the deployed digest
(`verify.digest_mismatch`); the window starts after warm-up and after the prediction was registered in the ledger
(`verify.not_preregistered`: stored, excluded from accuracy statistics); two windows pooled. Verification is
idempotent on (prediction digest, sorted after-capture digests), so a retried verification never counts a deploy
twice. A range built from verification history covers only changes the gate let through and were deployed, and it
states that limit.

Application Signals' `Latency` is "the delay before data transfer begins after the request is made"
[V: https://docs.aws.amazon.com/AmazonCloudWatch/latest/monitoring/AppSignals-MetricsCollected.html]. Whether it
equals the SERVER-span mean and whether it covers unsampled requests is [U]; it is used as a verification source only
after an equivalence test against an unsampled capture. The load balancer's or load generator's latency is the
independent measurement.

## 13. Policy (I10) and judge answers (I11)

Schemas are in [ARCHITECTURE.md](ARCHITECTURE.md), sections 6.2 and 7.2.

| input | refused |
|---|---|
| policy | `policy.no_threshold` (no threshold set); `policy.bad_value` (a known key with a bad value: exit 2); `policy.model_band_removed` (enforce mode without the model band); `policy.inline_refused` (an inline policy from an agent). Unknown keys are ignored and listed |
| judge answer | `judge.ms_forbidden` (a millisecond figure); `judge.loosening` (any attempt to relax a verdict); `judge.packet_mismatch`; `judge.evidence_unresolved`; `judge.answer_outside_question`. Free text is kept as commentary for humans and never parsed into a number |

## 14. Code-path inputs (I13) and run records (I14)

See [CODEPATHS.md](CODEPATHS.md) (source trees, build identity, route keys; research only) and
[EVALUATION.md](EVALUATION.md) (run records; a run with missing fields counts as a failure of its arm, never as
excluded).

---

## 15. Privacy: never send, and the two attribute lists

**Never send, never accept in a bundle:** credentials and secrets of any kind; configuration values (environment
variables, ConfigMap data); what a request or a response carries (bodies, headers, cookies); anything that identifies
an end user or a client (user ids, client addresses, query strings); database statements (`db.*`); error text
(exception messages); logs.

There are two lists, with different owners and different jobs:

- **The privacy list** is the team's: what its Collector lets leave the cluster (4.1). It may drop any key, path keys
  included; a dropped key is simply absent from the capture, and whatever that costs (probe detection by path, for
  example) is visible in `check`'s report.
- **The canonical keep-list** is rhobound's: what the cut keeps when it writes the canonical form (4.3, step 5). It is
  defined as **every key that the reader or `check` reads**, so applying it can never change a number. It is generated
  from the reader's own key lists, and a test fails when the reader reads a key the list lacks. The canonical form is
  written to the invocation's private folder and removed with it; only manifests, fits and reports are kept.

The canonical keep-list today:
- resource: `service.name`, `service.namespace`, `service.version`, `service.instance.id`,
  `deployment.environment.name`, `deployment.environment`, `host.name`, `host.arch`, `k8s.pod.name`, `k8s.pod.uid`,
  `k8s.deployment.name`, `k8s.namespace.name`, `k8s.node.name`, `k8s.container.name`, `aws.ecs.task.arn`,
  `container.id`, `container.image.name`, `container.image.tag` (singular, still read by the reader),
  `container.image.tags`, `container.image.repo_digests`, the presence of `faas.*`, `telemetry.sdk.language`,
  `telemetry.sdk.version`;
- span: ids, kind, times, name, `traceState`, status, event names and counts, link counts, `http.route`,
  `http.request.method`, `http.method`, `http.response.status_code`, `http.request.resend_count`, `rpc.method`, the
  four peer keys `peer.service`, `server.address`, `net.peer.name`, `http.host`, the ports `server.port` and
  `net.peer.port`, the five path keys `url.path`, `http.target`, `http.route`, `http.path`, `http.url` (with query
  and fragment removed), `user_agent.original`, `code.function.name`.

Delete identities rather than hash them: hashing a small id space does not anonymize. A scan of kept string values
for e-mail addresses, JWT-shaped strings and card-like numbers raises `trace.personal_data_suspect` (doubt) and masks
them [new]. Raw captures keep the bucket's 3-7 day lifecycle; only manifests, fits and reports are retained longer.

---

## 16. Garbage in, garbage out: the checklist

**Format**
- [ ] OTLP/JSON from the Collector's `file` or `awss3` exporter: hex ids, integer kinds, lowerCamelCase keys.
- [ ] `.jsonl.gz` or `.json.gz`, one request per line; no zstd, no protobuf.
- [ ] S3: `s3_partition_timezone: UTC`, `unique_key_func_name: uuidv7`, `compression: gzip`, batches of 5-10 s.

**Identity**
- [ ] `service.name` set explicitly on every service (never `unknown_service`); one deployment environment per
      prefix.
- [ ] Pod identity: `k8s.pod.name` on EKS, `aws.ecs.task.arn` on ECS (its task id is kept), not `host.name` alone,
      which on EC2 carries a private address; set by a Collector from the platform, not by the application:
      `from: connection` only where the SDKs connect directly, otherwise by `k8s.pod.uid` set on each node's agent
      Collector (4.1).
- [ ] `container.image.repo_digests` on every resource (`k8sattributes` extracting it, with `container.id` or
      `k8s.container.name` kept on the resource so that it can pick the container in a pod with sidecars).

**Structure**
- [ ] SERVER spans from every service; CLIENT spans for every outgoing call, databases and caches included.
- [ ] W3C `traceparent` propagated by every service.
- [ ] No service on a function platform among the modelled services.

**Sampling**
- [ ] Every request kept on the capture path (SDK sampler `parentbased_always_on`, no Collector sampler before the
      capture fork), **and** an independent request count per window (load balancer, API Gateway, ingress, or load
      generator). Without the count the result is inconclusive in every gate.
- [ ] No tail, adaptive, reservoir or rate-limited sampler anywhere on the capture path.

**Window and load**
- [ ] One steady window, at least 100 entry requests; for gating, at least 10 minutes at 20 or more requests/s.
- [ ] No rollout, restart, autoscaling step or node change during the window.
- [ ] The busiest service below 0.85; and know that above 0.58 the model's queueing is not validated.
- [ ] Clocks in step (NTP), or CLIENT spans on every call.
- [ ] Few errors, no load shedding, no retries during the window.

**Context data (recommended)**
- [ ] A cluster dump at the window's start and end, scrubbed.
- [ ] The SDK sampler configuration per service; the Collector distribution and version.
- [ ] An instrumentation inventory per service (which client libraries emit CLIENT spans).
- [ ] Extra probe routes, if any, with a reason.

**Change inputs**
- [ ] For CI: a measured difference (an A/B record, or a verified profile), not a typed number.
- [ ] Platform manifest digests of the old and new images; never tags alone.
- [ ] For A/B runs: both arms at once, one pod each, the same instance type, unsampled, with the load generator's
      per-request log.

**Never**
- [ ] Bodies, headers, tokens, query strings, user ids, secrets, environment values, logs.

## 17. How the contract is tested

- **One registry, three kinds of test.** Every code lives in one registry (section 19 says how the tables are
  generated from it). Each code has a trigger: `capture` codes (a defect in the traces or the store layout) each have
  a generated fixture that injects that defect into a clean capture (the repository's test data, scrubbed to
  192.0.2.0/24); `input` codes (a defect in another input file: a cluster dump, a request count, an A/B record, a
  profile, a change set, a policy, a judge answer) each have a unit test with a small defective file; `runtime` codes
  (handles, jobs, rate limits, the clock, the engine binary) each have a unit test of the component that raises them.
  A registered code without its test, or a fixture or test that names an unregistered code, fails CI.
- **The cut is neutral**: for every capture under the repository's test data, `check --cut` over a window that covers
  the whole capture, then `predict` on the canonical form, gives the same bytes as `predict` on the raw capture, apart
  from the `capture` field (the path argument).
- **Defect recall**: the right code for every deterministic class; a power curve against effect size and window
  length for statistical ones (trends, overdispersion).
- **False refusals**: clean captures, unchanged, give no refusal and no unexplained doubt.
- **Silent-wrong rate**, the number that matters most: captures admitted with no doubt whose prediction misses the
  truth by more than the documented band. Its exceedance rate is tested against the band's nominal rate per known
  class (a binomial test), not against zero, because a p90 band is exceeded about one time in ten by design.
- **Report v1 does not move**: report v1 is byte-identical to that of rhobound-cli at commit 1a2df09 on the
  repository's test captures and on every capture without a platform pod identity (`TestGoldenSweep1a2df09`; a capture
  past the reader's caps of 3.2 too, which report v1 reads whole: `TestReportV1ReadsWithoutCaps`). On a
  capture that names its pods only by `k8s.pod.uid` or an ECS task and has no `k8s.pod.name`, pods are keyed by that
  identity, so the predictions can change (3.4); `TestPodIdentityChange` pins one such capture and its numbers, and
  `TestPodIdentitySplitWarning` an uneven split on task ARNs, whose warning names the task ids. On an architecture
  without stored goldens, `tools/golden-live.sh` compares with a 1a2df09 binary built there (`TestGoldenLive1a2df09`).
- **Open-world intake**: the share of captures from systems we did not build that pass as-is, and which codes stop
  the rest (EVALUATION.md).

## 18. Not verified

- Whether container runtimes report the index or the platform digest in `repo_digests` and `imageID` for
  multi-platform tags [K].
- The `rpc.message` event conventions used for stream detection, the `user_agent.original` probe signatures, and the
  OTTL `keep_keys` function names [K].
- Whether Application Signals counts unsampled requests, and whether its `Latency` equals the SERVER-span mean [U].
- `awss3exporter` support for S3 directory buckets; `awss3receiver` handling of zstd [U].
- The ECS sources for a peer map [U].
- The `k8sattributes` association sources (`connection`, `resource_attribute`), its use of `container.id` or
  `k8s.container.name` to choose a container, and its extract key names (`container.image.tag` against the plural
  form) in the Collector version a team runs [K].
- The queue-rebuild error under head sampling at loads where worker counts are identified: untested, so
  `fit.queue_rebuild_sampled` stays a doubt until a simulator test bounds it.
- Every threshold marked [D].

## 19. Every other code

Section 3.14 lists the codes a capture or its store triggers (trigger `capture`). This table lists every other code.
Both tables are generated from the one registry in the code (`internal/contract/codes.go`) by `go run
./tools/gencontract docs`, which also writes the whole registry as JSON (`docs/mcp/data-contract.json`,
`rhobound.data-contract/v1`); `go test ./tools/gencontract` fails when a table or that file and the registry differ.
The server's `rhobound://data-contract` resource is that file, as the server's build embeds it: every code with its
level, trigger, condition, fix, status and the names of its numbers (`numbers`: a stored finding with another is not
one the registry makes). Trigger is as in
section 17: `capture`, `input` or `runtime`; "where" is the section that defines the code; status is as in 3.14.
Finding codes and the gate's rule tokens are separate namespaces (ARCHITECTURE.md, 5.4).

<!-- gencontract:begin other-codes (generated from internal/contract/codes.go by `go run ./tools/gencontract docs`; edit the registry, not this table) -->
| code | level | trigger | when | fix | where | status |
|---|---|---|---|---|---|---|
| `store.paged_api` | R | input | the source is CloudWatch aws/spans or an X-Ray retrieval API | add an S3 exporter to the same Collector | 4.4 | [later] |
| `store.metrics_only` | R | input | the source is Application Signals metrics | capture spans; metrics are a verification source only | 4.4 | [later] |
| `store.not_spans` | R | input | the source is Athena results or Parquet | export spans as OTLP/JSON | 4.4 | [later] |
| `cluster.not_list` | R | input | a cluster file that is not JSON or not a Kubernetes List | dump with kubectl get ... -o json | 5 | [new] |
| `cluster.unscrubbed` | R | input | secrets, ConfigMaps, env values, managedFields or the last-applied annotation in a dump | scrub the dump before it leaves the cluster | 5 | [new] |
| `cluster.window_mismatch` | R | input | a dump taken more than a minute [D] outside the window | dump at the window's start and end | 5 | [new] |
| `cluster.layout_ambiguous` | R | input | both a flat cluster/ folder and start/ or end/ | keep one layout | 5 | [new] |
| `cluster.peer_map_invalid` | R | input | an ECS peer map that does not match rhobound.peer-map/v1 | write the peer map as rhobound.peer-map/v1 | 5 | [new] |
| `cluster.digest_disagrees` | D | input | a pod's imageID and the spans' repo_digests name different images | a window after the rollout settled | 5 | [new] |
| `cluster.time_unknown` | N | input | no taken_at recorded for a dump | record taken_at in the capture manifest | 5 | [new] |
| `cluster.single_snapshot` | N | input | a flat cluster/ folder: one snapshot for the whole window | dump at the window's start and end | 5 | [new] |
| `count.window_mismatch` | R | input | the count's window edges more than 10 s [D] from the capture's | count the capture's own window | 6 | [new] |
| `count.unknown_entry` | R | input | the count names an entry the capture does not have | name entries by their service.name | 6 | [new] |
| `env.cpu_throttled` | D | input | throttled periods above a threshold [D] in resource metrics | raise the CPU limit, or capture a quieter window | 7 | [later] |
| `fit.utilization_disagrees` | D | input | inferred utilization and CPU use disagree beyond a band | check the worker counts | 7 | [later] |
| `fit.expired` | R | runtime | a fit past its expiry | fit a fresh window | ARCHITECTURE.md 4.2 | [new] |
| `fit.no_baseline` | R | runtime | a call names an environment that has no baseline fit yet | name a fit_id, or wait for the environment's first fit | ARCHITECTURE.md 4.2, 5.8 | [new] |
| `fit.version` | R | runtime | a fit or A/B record of another schema version, one whose fixed fields (the tool, digests, kept share, findings) this version does not write, or a fit record whose bytes are not those fit wrote (serialized again) | fit again with this version, and store the record's bytes as fit wrote them | ARCHITECTURE.md 4.2 | [new] |
| `fit.stale` | D | runtime | a fit older than max_fit_age_h at gate time (rule token fit_stale) | fit a fresh window | ARCHITECTURE.md 4.2, 6.3 | [new] |
| `ab.not_concurrent` | R | input | A/B arms overlapping less than 90% [D] | run both arms at the same time | 8 | [new] |
| `ab.hardware_differs` | R | input | A/B arms on different instance types or container resources | one instance type and one resource spec for both arms | 8 | [new] |
| `ab.sampled` | R | input | an A/B arm whose kept share is not proven to be 1 | capture every request, with the load generator's count | 8 | [new] |
| `ab.errors_differ` | D | input | error shares more than 1 point [D] apart | fix the errors in the failing arm | 8 | [new] |
| `ab.too_few_visits` | D | input | fewer visits than the sizing rule needs for the gate's margin | run each arm longer | 8, 9.3 | [new] |
| `ab.count_conflict` | D | input | an A/B arm whose own request count gives a kept share other than 1, while the load generator's log, which admits the arm, says its traces hold every request (the subject is the arm) | give each arm its own request count, or none: the log alone proves an arm's kept share | 8 | [new] |
| `ab.payload_changed` | D | input | the mean response body the load generator read differs between the arms by more than 10% [D] (the subject is the driven service; a prediction names the fit's callers of it as examples): the callers' cost of reading and decoding it is in no arm's spans | measure the callers with the candidate (an A/B of the callers, or a canary), or describe their cost as a change | 8 | [new] |
| `change.label_not_minted` | R | input | a label other than declared without a record the CLI can find and recompute | cite an A/B record the CLI computed, or label it declared | 11.2 | [new] |
| `change.unknown_service` | R | input | a change or --workers names a service the fit does not have | name a service of the fit | 11.2 | [new] |
| `change.ambiguous_service` | R | input | a change or --workers names a service by a name the views show for more than one service, none of them named so as the traces wrote it (a collision of hashes) | name the service as the traces wrote it, as the services of the fit record list it | 11.2 | [new] |
| `change.negative_time` | R | input | the change makes a time negative | a change no larger than the time it removes | 11.2 | [new] |
| `change.range_inverted` | R | input | lo > hi | give the range as lo \<= hi | 11.2 | [new] |
| `change.too_many_ranges` | R | input | more than 6 unsigned ranged items | at most 6 unsigned ranged items | 11.2; ARCHITECTURE.md 4.5 | [new] |
| `change.unsupported_op` | R | input | an operation this version cannot apply | use own_ms, own_pct, workers or load_scale | 11.2 | [new] |
| `change.free_text` | R | input | a number given in prose | give the number as a value | 11.2 | [new] |
| `change.cpu_profile_not_own_time` | R | input | a CPU profile offered as own time | measure own time per visit (an A/B run) | 11.2 | [new] |
| `change.includes_queueing` | R | input | a latency that already contains the queue offered as own time | measure own time per visit (an A/B run) | 11.2 | [new] |
| `change.microbench_unvalidated` | R | input | a microbenchmark delta outside research use | measure own time per visit (an A/B run) | 11.2 | [new] |
| `change.route_scope_unsupported` | R | input | an operation other than own_ms restricted to a route, or an own_ms item on a route the fit does not keep | give own_ms on a route the fit's routes[] names, or convert to a per-visit mean: ms times the route's share of visits | 11.2 | [new] |
| `change.callee_cost_assumed` | D | input | a structural operation that assumes the callee's mean visit | measure the callee's visit for that endpoint | 11.2 | [later] |
| `load.busiest_unknown` | N | runtime | no busiest-window pointer yet: evaluated at the fit's window | none needed | ARCHITECTURE.md 4.2 | [new] |
| `budget.no_policy` | D | input | a budget under no policy: no entry threshold applies, only the utilization limits bind | name a policy (budget --policy P, or the budget tool's policy argument), so that its entry threshold binds | ARCHITECTURE.md 4.7 | [new] |
| `policy.no_threshold` | R | input | neither threshold key is set | set max_entry_mean_change_pct or max_entry_mean_change_ms | 13 | [new] |
| `policy.bad_value` | R | input | a known key with a bad value, or an unregistered code in doubts_that_hold (exit 2) | correct the value | 13 | [new] |
| `policy.model_band_removed` | R | input | enforce without the model band | keep model_band when enforce is true | 13 | [new] |
| `policy.inline_refused` | R | input | an inline policy from an agent | name a policy from the server's configuration | 13 | [new] |
| `policy.not_named` | R | runtime | a budget or a prediction names no policy, and the configuration has none named default | name a configured policy | ARCHITECTURE.md 5.8 | [new] |
| `policy.unknown` | R | runtime | a policy name the server's configuration does not list | name a configured policy | ARCHITECTURE.md 5.8 | [new] |
| `gate.enforce_not_earned` | N | runtime | enforce: true before the readiness conditions hold (always, in the first build) | none: the verdict is advisory | ARCHITECTURE.md 6.6 | [new] |
| `judge.ms_forbidden` | R | input | a millisecond figure in a judge answer | answer with typed fields only | 13 | [new] |
| `judge.loosening` | R | input | an answer that would relax a verdict | answer with typed fields only | 13 | [new] |
| `judge.packet_mismatch` | R | input | the answer cites another packet | answer the packet that was sent | 13 | [new] |
| `judge.evidence_unresolved` | R | input | an evidence reference that is not in the packet | cite evidence from the packet | 13 | [new] |
| `judge.answer_outside_question` | R | input | a range for a question the packet did not ask, or outside its bounds | answer the packet's questions within their bounds | 13 | [new] |
| `handle.expired` | R | runtime | a handle past its expiry | start a new fit or prediction | ARCHITECTURE.md 5.8 | [new] |
| `handle.unknown` | R | runtime | an unknown handle, or another principal's | use a handle this principal received | ARCHITECTURE.md 5.8 | [new] |
| `rate.limited` | R | runtime | a per-principal limit was hit (retry_after_ms) | retry after retry_after_ms | ARCHITECTURE.md 5.8 | [new] |
| `job.timeout` | R | runtime | a job past its deadline_at | start the job again, with a shorter window | ARCHITECTURE.md 5.5 | [new] |
| `job.failed` | R | runtime | a job's step failed without a coded refusal: the engine exited without one, or wrote no record | start the job again; if it fails again, the server's operator can run rhobound check and fit on the capture to read the engine's message | ARCHITECTURE.md 5.5 | [new] |
| `server.engine_mismatch` | R | runtime | the engine binary's sha256 differs from the configured one | install the configured engine binary | DEPLOY_AWS.md 2.2 | [new] |
| `profile.not_found` | R | runtime | no verified record for (baseline, candidate) | profile the candidate, or run an A/B | 9.5 #2 | [new] |
| `profile.digest_unresolvable` | R | input | a digest that cannot be resolved to the node's platform manifest | push a platform manifest for the node's architecture | 9.5 #1 | [new] |
| `profile.media_type_parameter` | R | input | a manifest media type with parameters (;version=) | push media types without parameters | 9.2 | [new] |
| `profile.bad_signature` | R | input | the signature does not verify (the first build refuses every signature) | sign with a key on the trust list | 9.5 #3 | [new] |
| `profile.variance_changed` | D | input | the own time's variance (diff-fit: var_delta, var_se in ms^2) or cs2 (a profile) differs beyond 2 standard errors | none: a prediction applies the measured variance difference (own_var_ms2), and its uncertainty stays a doubt | 8, 9.5 #15 | [new] |
| `profile.structure_changed` | D | input | calls per visit differ between arms (also from diff-fit) | describe the change structurally | 8, 9.5 #14 | [new] |
| `profile.route_heterogeneous` | D | input | a route's own-time difference differs from the service's blended difference beyond 3 standard errors (diff-fit) | give the change as one own_ms item per route (route: the record's routes[].route, value: its own_ms delta), not the blend | 8 | [new] |
| `profile.self_report_mismatch` | D | input | the outside measurement disagrees with the spans (also from diff-fit) | check the candidate's instrumentation | 8, 9.5 #16 | [new] |
| `profile.untrusted_signer` | R | input | the signer is not the harness identity | sign with the harness identity | 9.5 #4 | [later] |
| `profile.predicate_type` | R | input | another predicate type or version | use the frozen predicate type | 9.5 #5 | [later] |
| `profile.harness_not_allowed` | R | input | harness, load generator or Collector config not on the allow-list | use an allowed harness | 9.5 #6 | [later] |
| `profile.subject_mismatch`, `profile.baseline_not_running` | R | input | the subject or the baseline arm is not the expected digest | profile the candidate about to run against the running digest | 9.5 #7 | [later] |
| `profile.stale`, `profile.time_disagrees` | R | input | too old, or its time disagrees with the witness row | profile again | 9.5 #8 | [later] |
| `profile.hardware_mismatch` | R | input | instance type, CPU family, limit or container spec differ from production | profile on production's instance type and spec | 9.5 #9 | [later] |
| `profile.load_out_of_range` | R | input | the operating point after the change lies outside the measured levels | measure a level at production's per-pod rate | 9.5 #10 | [later] |
| `profile.scope_not_mean`, `profile.no_variance` | R | input | not a mean profile, or no variance | profile the mean with its variance | 9.5 #11 | [later] |
| `profile.too_few_visits` | R | input | too few visits for the gate's margin | measure each level longer | 9.5 #12 | [later] |
| `profile.window_over_cap` | R | input | the sized measurement window is over the cost cap | a wider margin, or a higher cap | 9.3 | [later] |
| `profile.level_drift` | D | input | flatness across load levels not shown | measure at production's per-pod rate | 9.5 #13 | [later] |
| `profile.transfer_gap` | D | input | the harness baseline differs from production's own time | none: the delta is given as a range over two transfer rules | 9.5 #17 | [later] |
| `profile.errors_differ` | D | input | error shares more than one point [D] apart | fix the errors in the failing arm | 9.5 #18 | [later] |
| `profile.no_structure` | R | input | a profile without calls per visit per arm | measure the calls per visit to each callee in both arms | 9.2 | [new] |
| `profile.no_outside_measurement` | R | input | a profile without the client-side latency or the cgroup CPU time per visit | measure each arm from outside the system under test | 9.2 | [new] |
| `profile.locations_present` | R | input | a storage location, account id or cluster ARN in a profile's predicate | record digests only; never a location | 9.2 | [new] |
| `verify.sampled`, `verify.digest_mismatch` | R | input | an after-capture that is sampled, or runs another digest | capture every request on the deployed digest | 12 | [later] |
| `verify.not_preregistered` | N | input | the prediction was not registered before the window: stored, not counted | register the prediction before the deploy | 12 | [later] |
| `code.no_base_tree`, `code.build_unbound`, `code.language_unsupported` | R | input | code-path inputs that cannot be analysed | supply the base tree and the build identity | [CODEPATHS.md](CODEPATHS.md), 5 | [research] |
| `code.route_key_missing` | D | input | no route key on SERVER spans: falls back to services only | record http.route or rpc.method on SERVER spans | [CODEPATHS.md](CODEPATHS.md), 5 | [research] |
<!-- gencontract:end other-codes -->
