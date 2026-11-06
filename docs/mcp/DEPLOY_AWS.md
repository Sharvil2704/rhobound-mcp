# rhobound-mcp on AWS, and locally

How rhobound-mcp is deployed and served: the AWS services chosen and why, IAM, storage layout and lifecycle, the
trace store, image attestations, the MCP endpoint and its authentication, observability, cost, infrastructure as code,
and the local mode that needs no cloud at all. Status: **design; nothing here has been deployed.** The code that runs
on AWS is built in the first slice and tested against fakes: the Lambda runtime loop and the mode dispatch, the API
Gateway event adapter, the S3 window fetcher (against an object-store fake), the fit worker's job handler and the OCI
referrers client. The AWS bindings behind their interfaces (the S3 object store, the DynamoDB state store, the Lambda
invoke of the fit worker, the ECR token source) and the template are [later]. The first deploy is a maintainer-run
test with the acceptance checks of section 13. Tags [V] [K] [U] [E] [D] are as in
[ARCHITECTURE.md](ARCHITECTURE.md). Prices are us-east-1 list prices as read or remembered on 2026-10-26: re-check
them in the AWS Pricing Calculator before relying on any figure.

---

## 1. Shape of the deployment

- **Single tenant.** One deployment per team, in the team's own account, in the Region where its traces are. Traces
  never leave the account. A hosted multi-tenant service is not part of this design.
- **Serverless where the work is short, a container fleet where it is long.** Predictions, budgets and context are
  milliseconds on a cached fit: Lambda. Fitting a window is seconds: a separate Lambda worker. Measuring an image
  under load is minutes to an hour on the production instance type: ECS on EC2, later.
- **Split binaries and roles.** The function that faces agents holds no right to read traces, push to ECR, sign with
  KMS or call a model. Only the workers it invokes have those rights, each its own.
- **CI never calls the MCP server.** It runs the `rhobound` CLI in the account (CodeBuild) or on a runner that reads a
  fit by digest, with the repository's policy.

| need | service | why this one |
|---|---|---|
| MCP endpoint for agents | API Gateway **HTTP API** with its native JWT authorizer | unauthenticated calls stop before any compute runs; no authorizer code; $1.00 per million requests [V: https://aws.amazon.com/api-gateway/pricing/] |
| the same, when a WAF or usage plans are needed | API Gateway **REST API** with a Lambda authorizer; WAF attached to the stage | WAF attaches directly to REST APIs [V: https://docs.aws.amazon.com/cli/latest/reference/wafv2/]; $3.50 per million |
| compute, agent-facing | Lambda, `provided.al2023`, **arm64, zip** | no idle cost; Go is supported on the OS-only runtime [V: https://docs.aws.amazon.com/lambda/latest/dg/lambda-golang.html]; a zip with an in-process event adapter avoids the loopback hop of the Lambda Web Adapter |
| fits | Lambda, invoked asynchronously, 4-6 GB | seconds of work, parallel on the vCPUs that memory buys (1 vCPU per 1,769 MB [V: https://docs.aws.amazon.com/lambda/latest/dg/gettingstarted-limits.html]) |
| scheduled fits | EventBridge Scheduler | hourly, and after each rollout settles |
| state | DynamoDB, on demand, one table with TTL | handles, jobs, the baseline pointer, the preview log, verified-profile records |
| artifacts and ledger | S3 with SSE-KMS **and Bucket Keys**; the ledger with Object Lock in governance mode | content-addressed blobs; a registered prediction cannot be rewritten |
| traces | the team's S3 bucket, written by its Collector | the only store the reader parses unchanged |
| OAuth authorization server | Amazon Cognito with pre-registered app clients, or any OIDC provider | Cognito has no dynamic client registration [K], so each agent type is a pre-registered client; Okta, Auth0, Entra or Keycloak where dynamic registration or client metadata documents are wanted |
| image attestations [later] | ECR (OCI 1.1 referrers), KMS asymmetric signing key, a verifier Lambda | section 6 |
| profile harness [later] | Step Functions + ECS on EC2 (one instance type, one AZ) | no control-plane fee; placement pinned; section 7 |
| CI judge step | Amazon Bedrock Converse, or an OpenAI-compatible endpoint, called from CI by an example script (first build) | the server never calls a model; the answer is checked by `rhobound judge-check` |

Not chosen, and why:
- **A Lambda Function URL for OAuth clients.** A URL has one auth type [K]. With `NONE`, every unauthenticated request
  runs the function, and a WAF in front through CloudFront does not work for these clients: CloudFront origin access
  control to a Function URL needs `AWS_IAM`, re-signs the request (so a Bearer token either is replaced or reaches
  Lambda as a broken SigV4 header), and needs the client to send `x-amz-content-sha256` on POST, which MCP clients do
  not [V: https://docs.aws.amazon.com/AmazonCloudFront/latest/DeveloperGuide/private-content-restricting-access-to-lambda.html].
  A Function URL with `AWS_IAM` remains an option for AWS-internal callers.
- **App Runner.** Closed to new customers since 2026-04-30
  [V: https://docs.aws.amazon.com/apprunner/latest/dg/apprunner-availability-change.html].
- **AgentCore Runtime, for now.** It validates JWTs and serves the protected-resource metadata itself, which is
  attractive, but two behaviours must be tested first: whether it forwards the MCP standard headers
  (`MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name`) to the container [U], and whether a stateless client that drops
  the platform's session id causes a new session (and a cold start) per request, with a default limit of 25 new
  sessions per second per account [U]. Its 2 vCPU / 8 GB session cap does not matter for `predict` [V:
  https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/bedrock-agentcore-limits.html].
- **ECS Express Mode (Fargate)** is the fallback host if steady traffic or warm caches ever justify an always-on task
  [V: https://docs.aws.amazon.com/AmazonECS/latest/developerguide/express-service-getting-started.html].

---

## 2. The MCP endpoint and authentication

### 2.1 Routes

| route | authorization | for |
|---|---|---|
| `POST /mcp` | JWT authorizer: issuer, audience (or `client_id`), expiry, scope `rhobound:read` or `rhobound:fit` | agents with OAuth |
| `GET /.well-known/oauth-protected-resource` and `/.well-known/oauth-protected-resource/mcp` | none | Protected Resource Metadata (RFC 9728) |
| `POST /mcp-iam` | `AWS_IAM` | AWS-internal callers |
| `POST /mcp-token` | a Lambda authorizer checking a hashed static token | header-only clients (the Copilot cloud agent and code review cannot do OAuth for remote servers [V: https://docs.github.com/en/copilot/how-tos/use-copilot-agents/coding-agent/extend-coding-agent-with-mcp]) |
| anything else | none: 404 | |

The JWT authorizer validates before Lambda runs. It supports RSA keys only and checks `aud`, or `client_id` when a
token has no audience, as Cognito access tokens do without resource binding [V:
https://docs.aws.amazon.com/apigateway/latest/developerguide/http-api-jwt-authorizer.html]. Binding a Cognito token to
this server then means: an allow-listed `client_id`, `token_use == access`, and a scope prefixed with this server's
resource-server identifier. The function reads that identifier from `RHOBOUND_MCP_JWT_RESOURCE_SERVER`
(`--jwt-resource-server` in container mode): `<identifier>/rhobound:fit` counts as `rhobound:fit`, and a scope under
any other prefix is ignored. The authorizer's 401 headers cannot be customized [U]; MCP clients must then find the
metadata at the well-known URI, which the specification requires them to try [V:
https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization]. Testing that each target client does so
is part of the first deploy.

Static tokens: at least 128 random bits, one per repository, read-only scope, a rotation period, stored hashed in
DynamoDB, compared in constant time.

What read scope shows: the service names and the findings of the fits a caller can reach, pod identities included
(ARCHITECTURE.md 5.9). Of an ECS task only its id is shown, never the task ARN with the account, the region and the
cluster, and of any other ARN only its last element, or a hash of the whole ARN when that element holds an account,
whichever build of the engine made the record; but a pod named only by `host.name` is shown by that name, and on EC2 it
carries the host's private address. A peer that is not a service is shown as `external_<hash8>`, which anyone who can
guess its address can confirm. Give read scope only to callers trusted with these, or set `aws.ecs.task.arn` (or
`k8s.pod.name`) and trim `host.name` in the Collector.

HTTP APIs cap an integration at 30 seconds [K]; every synchronous tool targets a p99 under 5 s, and fits are jobs.

### 2.2 The function behind the route

- One zip: `bootstrap` (the `rhobound-mcp` binary) and `rhobound` (the engine), both arm64, both built with the latest
  patch release of a supported Go (the standard library's security fixes reach the network-facing server only through
  it), `-trimpath` and `CGO_ENABLED=0`. Two such builds from one tree on one host give the same bytes; docs/BUILD.md
  says what was checked. The zip is for the operator's own account. A binary given to others carries the notices of
  the modules it links (`mcp/THIRD-PARTY.md`): add `LICENSE`, `NOTICE` and each module's licence text, from the module
  cache at the versions `mcp/go.mod` pins, beside it.
  Lambda starts `bootstrap` without arguments; with `AWS_LAMBDA_RUNTIME_API` set it runs the mode its function's
  Handler setting names, `lambda` or `fit-worker` [slice]; `rhobound-mcp` checks the engine's sha256 at start and refuses to run on a mismatch
  (`server.engine_mismatch`).
- The in-process adapter turns the API Gateway event (HTTP API payload 2.0, or REST proxy 1.0) into an `http.Request`
  for the MCP SDK's handler and the recorded response back. There is no listener on loopback, so the SDK's
  localhost Host-header protection never fires; a unit test proves the adapter offline.
- The MCP handler runs in stateless mode with JSON responses, a 256 KiB body cap, and cross-origin protection.
- The engine runs as a subprocess with a **minimal environment**: no `AWS_*` variables reach it. Change sets are passed
  as JSON files in a per-call `0700` folder, never built into command-line strings from names.
- 1,024 MB [D], 25 s timeout (under the HTTP API's 30 s).
- On Lambda, `job_status` returns at once with `poll_after_ms`: a long poll would bill idle time and hold concurrency.
- Reserved concurrency is a template parameter, **unset by default**: Lambda requires 100 unreserved concurrency to
  remain in the account [V: https://docs.aws.amazon.com/lambda/latest/dg/configuration-concurrency.html], and new
  accounts can start below the documented default [K], so a hard-coded reservation fails on a fresh account.

### 2.3 The fit worker

- Invoked asynchronously by the MCP function (for `fit_start`) and by the scheduler. `MaximumRetryAttempts: 0` and
  `MaximumEventAgeInSeconds: 60`: by default Lambda retries a failed asynchronous event twice, timeouts included
  [V: https://docs.aws.amazon.com/lambda/latest/dg/invocation-async-error-handling.html], which would run a timed-out
  fit three times. An on-failure destination writes `failed` with `job.timeout` or the error code.
- Every job record carries `deadline_at`; `job_status` reports `failed` past it even if the worker died without
  writing anything.
- 4,096-6,144 MB [D] (about 2.3-3.5 vCPU), 300 s timeout, 2-4 GB of `/tmp`; each invocation works in a fresh random
  folder under `/tmp` and removes it at the end, and the reader reads only the files the manifest lists, checking each
  sha256, so a warm environment can never mix in a previous window's files.
- A window that cannot be fitted within the timeout is a job for a container (CodeBuild or ECS), not a longer Lambda.

---

## 3. Storage

### 3.1 Buckets

| bucket | contents | encryption | lifecycle |
|---|---|---|---|
| traces (the team's, per environment prefix) | Collector output: `env=<env>/year=/month=/day=/hour=/minute=/traces_<uuidv7>.json.gz`; A/B captures under `ab/<repository>/<run id>/arm=A/` and `arm=B/`, in the same layout, written by the A/B run's own Collector or uploaded by CI | SSE-KMS with a Bucket Key | expire after 3-7 days: spans hold URLs and sometimes user ids |
| artifacts | `fits/sha256/<hex>.json`, `manifests/sha256/<hex>.json`, `predictions/sha256/<hex>.json`, `ab/sha256/<hex>.json`, `gate/<id>/{verdict.txt,gate.json,summary.md}`, `judge/<hash>.json`, `profiles/sha256/<hex>.json` [later] | SSE-KMS with a Bucket Key | 90 days [D]; gate outputs 1 year |
| ledger | `ledger/<env>/<yyyy>/<mm>/<prediction digest>.json`: hashes and verdicts, no raw data | SSE-KMS with a Bucket Key | Object Lock, **governance mode**, retention a template parameter (365 days [D]) |

- **Bucket Keys on every bucket.** Without them S3 calls KMS on every object request; at $0.03 per 10,000 requests the
  Collectors' write stream alone adds a few dollars a month at 10 s batches and about a hundred at 200 ms batches [E],
  and it draws on the account's KMS request quota [V: https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucket-key.html].
  A cfn-guard rule fails any template without them.
- **Block Public Access** on every bucket; TLS-only bucket policies.
- **Governance mode, not compliance mode, for the ledger.** Compliance-mode retention cannot be shortened by anyone,
  the root user included, so a team that tries the stack could not delete it. Teams that need compliance mode opt in.
- **Only the CI role writes the ledger.** Agent previews never reach it; they go to the expiring preview log.
- **Raw captures are never copied** under Object Lock: only manifests, fits and reports are kept longer than the
  traces bucket's lifecycle.

### 3.2 DynamoDB (one table, on demand, TTL)

| item | key | contents | TTL |
|---|---|---|---|
| job | `JOB#<principal>#<ulid>` | state, step, deadline_at, result handle, refusal | 7 days |
| handle | `H#<principal>#<ulid>` | the artifact digest it names | fits and A/B records `fit_ttl_h` (7 days by default); predictions 30 days |
| fit index | `FIT#<env>` / `<window end>` | fit digest, manifest digest, running digests | 90 days |
| baseline pointer | `BASE#<env>` | the current fit digest, its window, running digests, `busiest_7d` fit digest | none |
| preview log | `PREV#<repo>#<principal>#<branch>` / `<time>` | change hash, declared values, verdict | 30 days |
| verified profile [later] | `VER#<baseline>#<candidate>#<harness>` | verified delta, label, checks, `transfer_validated` | 90 days |
| ledger index | `LED#<env>` / `<prediction digest>` | conditional put (`attribute_not_exists`) | none |
| in-flight release [later] | `REL#<env>` / `<release id>` | changed services, candidate digests, change-set digest, state `approved` or `settled`; written by the gate job when a release is approved, set to `settled` by the fit worker when a settled baseline shows those digests running; the gate job reads all `approved` items of the environment and adds them to the change set (ARCHITECTURE.md, 6.5) | 7 days; an alarm on items that expire unsettled |
| registered capture | `CAP#<repository>#<name>` | the prefix of an A/B arm (`ab/<repository>/<run id>/arm=A/`), the run id, the registering principal; written by CI when an A/B run ends; `fit_start` with `ab` resolves names only through these items | 30 days |
| static token | `TOK#<sha256 of token>` | repository, scope, expiry | expiry |
| rate limits | `RATE#<principal>` | token-bucket state; concurrent fit jobs | 1 day |

---

## 4. The trace store, and "a faster database"

**S3 written by `awss3exporter` with `marshaler: otlp_json` and `compression: gzip` is the store.** It is the only
format the reader parses unchanged, it is the cheapest per span, and its write cost is controlled by the Collector's
batch timeout (DATA.md, section 4.1 has the configuration).

- **ADOT caveat.** `awss3exporter` is in the contrib distribution and is not listed in the AWS Distro for
  OpenTelemetry component table [V*]. Teams on ADOT use the contrib distribution for the capture Collector, a custom
  build, or the file exporter plus a periodic upload.
- **Where the speed comes from.** The agent loop never reads traces: it reads a fit of a few kilobytes, cached in S3 and
  indexed in DynamoDB, so a `predict` is milliseconds whatever the trace store is. Traces are read once per window,
  by the fit worker, off the agent's critical path: a 10-minute window at 100 entry requests per second and 10 spans
  per request is about 30-120 MB of gzip [E], fetched in about 1-8 s with 5-10 s batches and parsed in seconds.
- **"Faster" options, when they help.** S3 Express One Zone for the most recent hour lowers per-request latency; whether
  `awss3exporter` writes to directory buckets is [U]. ClickHouse is worth it only if the team already runs it (for
  ad-hoc questions a judge might ask); it needs a converter, planned later. Athena over the same objects answers ad-hoc
  questions and is never the reader's input.
- **Not inputs:** Tempo, Jaeger and SaaS search APIs (not exhaustive exports), CloudWatch `aws/spans` and X-Ray
  retrieval APIs (paged a few traces at a time), Application Signals (metrics, used only for verification after an
  equivalence test). If a team already exports to X-Ray, it adds an S3 exporter to the same Collector.

---

## 5. Image identity and the registry

- Every comparison of image identity resolves both sides to the **platform manifest** for the node's architecture: the
  digest in the span's `container.image.repo_digests`, the deploy's reference, and an attestation's subject. A running
  pod may report the multi-platform index digest [K]. `container.image.id` is never used.
- An ECR lifecycle rule removes untagged candidate images that never deployed (their referrers go within 24 hours);
  each referrer counts against the repository's image quota (100,000 by default [V:
  https://docs.aws.amazon.com/AmazonECR/latest/userguide/service-quotas.html]); a CloudWatch alarm watches the count.

## 6. Image attestations [later]

Built only after the transfer study passes (section 9). The predicate is in [DATA.md](DATA.md), section 9.

- **Signing.** cosign with an asymmetric KMS key (`awskms:///alias/rhobound-profile`) in the harness role's account,
  through an alias. The key policy names the signer role as the only principal allowed `kms:Sign` and denies it to
  everyone else; administrators keep key management. A CloudTrail-based alarm fires on any `Sign` by another principal.
- **Rotation.** Asymmetric KMS keys support neither automatic nor on-demand rotation
  [V: https://docs.aws.amazon.com/kms/latest/developerguide/rotate-keys.html]. Verifiers therefore hold a trust list of
  public keys, each with `valid_from` and `valid_until`; a rotation adds a key with an overlap window. A rotation drill
  is part of the first measurement deploy.
- **Freshness.** With KMS signing there is no third-party timestamp, so the signer also writes a witness row (digest,
  time) to the ledger of a separate account at signing time; the verifier checks the attestation's declared time
  against it.
- **Storage.** An OCI 1.1 referrer of the candidate image, with media types **without parameters** (an open ECR issue
  rejects `;version=` parameters with 405 [V: https://github.com/aws/containers-roadmap/issues/2306]; another reports
  405 on copying referrers into ECR [V: https://github.com/aws/containers-roadmap/issues/2783]). A copy goes to the
  artifact bucket.
- **Verify once, at push time.** EventBridge matches `detail-type: "ECR Referrer Action"` for our artifact type on
  allow-listed repositories; the verifier Lambda checks the signature and the predicate (DATA.md, 9.5) and writes
  `VER#<baseline>#<candidate>#<harness>`. ECR events are best effort
  [V: https://docs.aws.amazon.com/AmazonECR/latest/userguide/ecr-eventbridge.html], so a scheduled reconciler lists
  recent candidates without a record. The hot path is one DynamoDB read; no signature is verified per prediction, and
  no sigstore code is in the agent-facing binary.
- **No admission enforcement.** The pipeline enforces. A Kyverno rule or ECS deployment hook that keys on a verdict
  digest would, in enforce mode, refuse new Pods after any policy change, during registry outages, and on rollbacks;
  it may run in audit mode only.
- **A real ECR round trip** (push with parameter-free media types; fetch through the referrers API and through the
  `sha256-<hex>` tag fallback; cosign verify) is the first test of this stage. It cannot be run from a machine without
  AWS credentials.

## 7. The profile harness [later]

- **Where.** ECS on EC2 with one capacity provider of the production instance type, in **one Availability Zone**
  (cheaper, and it pins placement, which lowers variance). Interface endpoints for `ecr.api`, `ecr.dkr` and `logs` in
  that AZ and an S3 gateway endpoint instead of a NAT gateway.
- **Isolation of agent-written code.** The system under test gets only resources, limits, runtime flags and probes
  copied from production. Secrets are harness-only stubs. Tasks have no task role (no AWS rights), instance metadata is
  blocked (`ECS_AWSVPC_BLOCK_IMDS` [K]), and egress is limited to the stubs and the harness Collector. The load
  generator and the Collector belong to the harness and are pinned by digest.
- **Signing is a separate step** (a separate Step Functions task and role, never on a node that hosts a system under
  test); it reads only the capture manifest and the computed predicate.
- **Trigger** once per pull request reaching "ready", a merge-queue entry or a label, idempotent on (baseline,
  candidate, harness) digests, only for services the change touches. Never on every push: agents push several times
  per pull request.
- **Sizing and capacity.** Each level's window comes from `t ≈ 8·cs2·S³/(m²·ρ·c)` (DATA.md, 9.3), capped; above the cap
  the request is answered `inconclusive` with the estimated minutes and dollars. The harness is itself a queue: the
  number of parallel sets is chosen so that their utilization stays at or below 0.5, concurrent runs are capped per
  environment, and the queue position and ETA are reported.

---

## 8. IAM, least privilege

Templates use `${AWS::AccountId}` and parameters; no account ids appear in this repository.

| role | trusted by | allows | does not allow |
|---|---|---|---|
| `McpFnRole` | the MCP function | `dynamodb:GetItem/PutItem/UpdateItem/Query` on the table; `s3:GetObject` on `artifacts/fits/*`, `artifacts/predictions/*`, `artifacts/ab/*`; `s3:PutObject` on `artifacts/predictions/*`; `lambda:InvokeFunction` on the fit worker; `kms:Decrypt` on the artifacts key via S3 | the traces bucket; ECR; `kms:Sign`; Bedrock; the ledger; any deploy API |
| `FitWorkerRole` | the fit worker | `s3:ListBucket` on the traces bucket with an `s3:prefix` condition per configured environment prefix and `ab/*`; `s3:GetObject` on those prefixes; `kms:Decrypt` on the traces key; `s3:PutObject` on `artifacts/fits/*`, `artifacts/manifests/*`, `artifacts/ab/*`; `dynamodb:UpdateItem/PutItem` on job, fit-index and baseline items; `dynamodb:GetItem` on `CAP#` items; `dynamodb:UpdateItem` on `REL#` items (to mark them settled), item scope by a `dynamodb:LeadingKeys` condition | writing traces; ECR; models; the ledger |
| `CiGateRole` | GitHub OIDC with `aud = sts.amazonaws.com` and `sub` pinned to `repo:<org>/<repo>:environment:latency-gate`, or a CodeBuild service role | `s3:GetObject` on `artifacts/*`; `s3:PutObject` on `artifacts/gate/*` and on the **ledger**; `dynamodb:PutItem` on the ledger index with `attribute_not_exists`; `dynamodb:GetItem` on baseline and verified-profile items; `dynamodb:PutItem/Query` on `REL#<env>` items and `PutItem` on `CAP#<repository>#*` items; for A/B runs that CI uploads, `s3:PutObject` on the traces bucket's `ab/<repository>/*` only, with `kms:GenerateDataKey` on the traces key; optionally `bedrock:InvokeModel` on one model ARN (the judge step) | deploy rights (the deploy job has its own role); reading traces |
| `CollectorRole` | EKS Pod Identity or IRSA, or the ECS task role of the capture Collector | `s3:PutObject` on `traces/env=<env>/*`; `kms:GenerateDataKey` on the traces key | any read |
| `VerifierRole` [later] | the verifier | `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`, `ecr:DescribeImages` on allow-listed repositories; `ecr:GetAuthorizationToken`; `kms:GetPublicKey` on the profile key; `dynamodb:PutItem` on `VER#` items | `kms:Sign`; traces |
| `HarnessRunnerRole` [later] | Step Functions | `ecs:RunTask` on the harness task definitions; scoped `iam:PassRole`; `s3:PutObject` on the harness capture prefix | signing |
| `SignerRole` [later] | the signing step | `kms:Sign` on the profile key (and named in its key policy); ECR push of referrer artifacts on allow-listed repositories; `s3:PutObject` on `artifacts/profiles/*` | developers cannot assume it: the author of an image never signs its profile |
| `AuthorizerRole` | the static-token authorizer | `dynamodb:GetItem` on `TOK#` items | everything else |
| `ArgoReaderRole` [later] | the Argo Rollouts analysis Job's service account, through EKS Pod Identity | `dynamodb:GetItem` on `LED#<env>` items; `s3:GetObject` on `artifacts/gate/*` | writing anything; traces; the MCP endpoint |
| `JudgeRole` (optional) | the CI judge step, when it runs in the account (CodeBuild) | `bedrock:InvokeModel` on one model ARN, through a VPC endpoint; the key of any third-party model API in Secrets Manager, reached through an egress allow-list | the traces bucket; the ledger |

Example: the fit worker's trace read for one environment.

```json
{"Version": "2012-10-17", "Statement": [
  {"Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::${TraceBucket}",
   "Condition": {"StringLike": {"s3:prefix": ["env=${EnvName}/*"]}}},
  {"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::${TraceBucket}/env=${EnvName}/*"}
]}
```

Example: the profile key's policy, signing restricted to one role.

```json
{"Version": "2012-10-17", "Statement": [
  {"Sid": "KeyAdmins", "Effect": "Allow", "Principal": {"AWS": "arn:aws:iam::${AWS::AccountId}:role/${KeyAdminRole}"},
   "Action": ["kms:Describe*", "kms:Get*", "kms:List*", "kms:Create*", "kms:Put*", "kms:Update*", "kms:Enable*",
              "kms:Disable*", "kms:TagResource", "kms:UntagResource", "kms:ScheduleKeyDeletion",
              "kms:CancelKeyDeletion"], "Resource": "*"},
  {"Sid": "OnlySignerSigns", "Effect": "Allow",
   "Principal": {"AWS": "arn:aws:iam::${AWS::AccountId}:role/${SignerRole}"},
   "Action": "kms:Sign", "Resource": "*"},
  {"Sid": "NobodyElseSigns", "Effect": "Deny", "Principal": "*", "Action": "kms:Sign", "Resource": "*",
   "Condition": {"ArnNotEquals": {"aws:PrincipalArn": "arn:aws:iam::${AWS::AccountId}:role/${SignerRole}"}}},
  {"Sid": "VerifiersReadPublicKey", "Effect": "Allow",
   "Principal": {"AWS": "arn:aws:iam::${AWS::AccountId}:role/${VerifierRole}"},
   "Action": "kms:GetPublicKey", "Resource": "*"}
]}
```

---

## 9. Order of building on AWS

1. **First deploy** (maintainer-run, section 13): HTTP API with the JWT authorizer, the MCP function, the fit worker,
   DynamoDB, the artifacts bucket and the governance-mode ledger, the scheduler. About $5-15 a month [E].
2. **Transfer study, before any signing machinery**: 10-20 real changes measured by a manual paired A/B (`diff-fit`),
   compared with production after-captures against a pre-registered criterion ([EVALUATION.md](EVALUATION.md)). If
   transfer fails, signing and verification would protect numbers that can never pass.
3. **Measurement**, only if it passes: the harness, KMS signing with a rotation runbook and drill, one real ECR round
   trip, the push-time verifier.
4. **Enforcement** per environment: verification, A/A floor, seeded regressions, the rate cross-check, the readiness
   record ([ARCHITECTURE.md](ARCHITECTURE.md), 6.6).
5. Later: judge reviews recorded in the ledger, with the escalation alarm; AgentCore after its tests; converters for
   other trace stores.

---

## 10. Observability

- **The server's own spans** go to a separate pipeline and bucket, never to the capture Collector, so the server is
  never fitted as a service. A client's `traceparent` in `_meta` becomes a span link, not a parent; `baggage` is
  dropped.
- **Logs** are structured JSON on stderr with codes, handles, durations and sizes, never trace-derived strings.
- **Metrics and alarms:**
  - Lambda `REPORT` lines: `Init Duration` (cold starts) and billed duration per tool;
  - S3 PUT requests on the traces bucket (a jump means the Collector's batches got small);
  - `kms:Sign` by any principal other than the signer (CloudTrail);
  - the judge escalation rate and the inconclusive rate per environment (a jump in either is investigated);
  - fit jobs failed or timed out; DynamoDB throttles; an AWS Budgets alarm on the account.
- **No MCP logging feature** (deprecated); per-request log levels in `_meta` are honoured.

## 11. Limits, and how the design stays inside them

| limit | value | design response |
|---|---|---|
| Lambda synchronous payload | 6 MB request and response [V] | references only; results of 1-8 KB; artifacts in S3 |
| HTTP API integration timeout | 30 s [K] | synchronous tools target p99 < 5 s; fits are jobs |
| Lambda duration | 900 s [V] | fits time out at 300 s; longer work goes to a container |
| Lambda memory and CPU | up to 10,240 MB; 1 vCPU per 1,769 MB [V] | fit worker at 4-6 GB |
| `/tmp` | up to 10 GB [V] | 2-4 GB for the fit worker; a fresh folder per invocation |
| asynchronous retries | 2 by default, timeouts included [V] | 0 retries, on-failure destination, `deadline_at` |
| account concurrency | 1,000 by default, shared [V]; 100 must stay unreserved [V] | reserved concurrency only as an opt-in parameter |
| cold start | INIT is billed [V: https://aws.amazon.com/lambda/pricing/]; no published figure for Go [U] | no network at init; lazy clients; measured at the first deploy |
| DynamoDB item | 400 KB [K] | handles and indexes only |
| ECR images per repository | 100,000, each referrer counts [V] | lifecycle rule and alarm |
| documents derived from predictions, remembered | 4,096 per server process, the least recently used forgotten first [D] | a known limit: the 4,096 are shared by every caller, so other callers' documents can push one out, and it is then computed again and charged to its caller's predict bucket again (ARCHITECTURE.md 5.10) [later: a share per principal] |
| documents derived from predictions, stored | no quota | a known limit: what one principal stores is bounded only by its predict rate times the time an unreferenced artifact is kept, 30 days in stdio and container modes (ARCHITECTURE.md 5.10) and the artifacts bucket's lifecycle on AWS (3.1) [later: a quota per principal] |
| architecture determinism | arm64 and amd64 differ in the last bits | content ids include `GOARCH`; CI compares the 1a2df09 goldens byte for byte on arm64 only, and the amd64 jobs skip them; a comparison with a 1e-9 relative tolerance on amd64 is [later] |
| GitHub arm64 hosted runners | public repositories only [V] | the public repository's CI runs the golden sweep on one; a private fork needs the tolerance above [later] or CodeBuild arm64 |

## 12. Cost model

### 12.1 Assumptions (the reference workload)

us-east-1; one production environment, 15 services; 100 entry requests per second, 10 spans per request, about 0.7 KB
per raw span and about 10x gzip (about 6 GB of gzip a day); 3 capture Collectors writing 10 s batches; traces kept 7
days; 20 developers whose agents make 17,600 MCP calls a month, each 0.5 s at 1 GB; 660 pull requests a month with 1.5
changed services each; about 1,000 fits a month (hourly plus one after each deploy) at 20 s x 4 GB. All figures are
[E] from these assumptions and the prices below.

Prices: Lambda arm64 $0.0000133334 per GB-s [secondary source] and $0.20 per million requests [V]; HTTP API $1.00 per
million [V]; S3 PUT $0.005 per 1,000 [K], GET $0.0004 per 1,000 [V], storage $0.023 per GB-month [K]; KMS $1 per
key-month, $0.03 per 10,000 symmetric and $0.15 per 10,000 asymmetric requests [V: https://aws.amazon.com/kms/pricing/];
EC2 m7g.xlarge about $0.163 and c7g.large about $0.073 per hour [K]; an interface endpoint about $7.30 per AZ-month
[K].

### 12.2 Monthly cost

| item | quantity | per month [E] |
|---|---|---|
| trace capture: PUTs | 3 x 8,640 a day x 30 = 777,600 | $3.9 |
| trace storage | about 42 GB on average | $1.0 |
| KMS for the trace stream | about 0 with Bucket Keys (about $2.3 without at 10 s batches; about $117 without at 200 ms batches) | $0 |
| fits | 1,000 x 80 GB-s = 80,000 GB-s, plus GETs | about $1.2 |
| serving: MCP function | 17,600 x 0.5 GB-s = 8,800 GB-s | $0.12 |
| serving: HTTP API, DynamoDB, artifact requests, logs, KMS keys | | about $3-5 |
| **first deploy, without harness or judge** | | **about $10-12** (most of it the team's own trace capture) |
| profile harness [later] | once per pull request ready, filtered to touched services and de-duplicated: about 500-990 runs at about $0.17 per 25-minute run on 2 x m7g.xlarge + 1 x c7g.large | about $80-165 |
| harness networking [later] | one AZ, three interface endpoints and an S3 gateway endpoint | about $25 |
| CI judge step (optional) | 660 pull requests x 2 reviews x (20k input + 2k output tokens) | about $40-120, by model price |
| WAF (only with the REST variant) | one web ACL, a few rules | about $9+ |
| **with measurement and a judge** | | **about $115-320** |

Two lines are not on the AWS bill:
- **Agent tokens for the tool definitions.** Six tools at up to about 300 tokens each are about 1,800 tokens per agent
  turn unless the client defers tool definitions. At about 88,000 turns a month (20 developers) and a $3 per million
  input price [K], that is about $475 uncached, about $90 with prompt caching, about $0 with deferred tool search
  [E]. This is why the agent-facing tool set is small.
- **Benchmark model endpoints**, priced in [EVALUATION.md](EVALUATION.md).

### 12.3 Unit costs [E]

| unit | cost |
|---|---|
| one MCP call | about $0.00001-0.00002 |
| one agent change (10-30 calls on a cached fit) | about $0.0002-0.0006 |
| one fresh fit | about $0.0012 |
| one profile run | about $0.07 at 10 minutes, $0.17-0.20 at 25 minutes, $0.35 at 50 minutes |
| one judge review (22k tokens) | about $0.03-0.09, by model |

The dominant costs are measurement and model calls, and the Collector's batch size; the serverless core is a rounding
error. The first deploy records billed cost per call and compares it with this table.

## 13. Infrastructure as code, and the first-deploy tests

- **AWS SAM** (`deploy/sam/template.yaml`) for the core stack: the HTTP API and its authorizers, the functions, the
  table, the buckets, the scheduler, the alarms. Parameters: environment names and their trace prefixes, Cognito pool
  or OIDC issuer, allowed client ids, ledger retention, reserved concurrency (unset), memory sizes.
- CI runs `cfn-lint` and `cfn-guard` with rules that fail a template when: a bucket lacks a Bucket Key or Block Public
  Access; a key policy allows `kms:Sign` to more than one principal; a role allows `s3:*` or `kms:*`; the ledger uses
  compliance mode without the opt-in parameter; reserved concurrency is hard-coded.
- Images and layers are pinned by digest, third-party CI actions by commit SHA, tools by version and hash. Releases
  ship signed binaries, an SBOM, build provenance, and the hash of the tool definitions, instructions and prompts.
- A Terraform module may follow; it is not planned.

**First-deploy acceptance tests** (maintainer-run; the machine this design was written on has no AWS access):
1. A `predict` through `POST /mcp` returns the same bytes as the local stdio server on the same fit.
2. Claude Code, Codex and Cursor complete OAuth against the protected-resource metadata, and find it at the
   well-known URI when the authorizer's 401 carries no `WWW-Authenticate` header.
3. 2026-07-28 requests keep their `Mcp-Method` and `Mcp-Name` headers through API Gateway; a 2025-11-25 client
   negotiates the older revision.
4. `Init Duration` and billed duration per tool are recorded, and billed dollars per call are compared with 12.3.
5. A fit that times out shows `failed` with `job.timeout` within its deadline, and runs once.
6. A header-only client with a static token reaches only read-only tools.
7. If AgentCore is considered: header forwarding, and 20 calls from a stateless client counting sessions and cold
   starts.

---

## 14. Local and development mode

No AWS, no network, no credentials: the stdio server and files.

```
$ go build -o bin/rhobound ./cmd/rhobound
$ (cd mcp && go build -o ../bin/rhobound-mcp ./cmd/rhobound-mcp)      # Go 1.25 or later
$ bin/rhobound check  testdata/mcp/loop --json             # the data contract: exit 0 admitted, 4 refused
$ bin/rhobound fit    testdata/mcp/loop --workers a=1,b=2,c=1 --out out/fit
$ bin/rhobound budget --fit out/fit/fit.json --policy testdata/mcp/loop/policy.json --json
```

`testdata/mcp/loop` is the scripted agent's fixture; its README walks the whole loop with the CLI twins.

The server reads a config file that maps names to locations; tools take names, never paths:

```json
{
  "schema": "rhobound-mcp.config/v1",
  "engine": {"path": "/usr/local/bin/rhobound", "sha256": "<hex of the binary>"},
  "state_dir": "~/.local/state/rhobound-mcp",
  "envs": {"staging": {"store": "dir", "root": "/data/captures/staging"},
           "prod": {"store": "s3", "bucket": "acme-traces", "region": "eu-west-1", "prefix": "env=prod/",
                    "retry_s": 300, "batch_s": 10, "sdk_delay_s": 5}},
  "captures": {"demo": "/data/captures/demo", "ab-old": "/data/ab/old", "ab-new": "/data/ab/new"},
  "policies": {"default": "/work/repo/.rhobound/policy.json"},
  "profile": "default",
  "fit_ttl_h": 168
}
```

Relative paths are resolved against the configuration file's folder, and `~/` against the home folder. A call that names
no policy uses the one named `default`; without it, `budget` and `predict` refuse such a call (`policy.not_named`).
`fit_ttl_h` (1 to 8,760; 168 when absent) is how many hours fit and A/B handles live, and how long after the end of its
window an environment's baseline fit is still used before `fit.expired`. The stdio and HTTP modes sweep
`state_dir/server` at start and every hour (ARCHITECTURE.md 5.10). `examples/mcp/config.example.json` maps the
repository's fixtures; its engine sha256 is a placeholder, so the server refuses to start (`server.engine_mismatch`)
until it holds the sha256 of the binary at `engine.path`. A `dir` store lays files out as the S3 exporter does
(`env=staging/year=.../traces_<id>.json.gz`), so the same fetcher code runs locally and in tests. An `s3` store names
its bucket and Region (`AWS_REGION` when absent) and the prefix the Collector writes under; the client is the
standard library's, signing with Signature Version 4 from the credentials in the environment (`AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`: what Lambda sets for its role; on ECS or EC2 export the role's
credentials before a refit, since the metadata endpoints are not read in this build). An `endpoint` (https, or
http on a loopback address) makes requests path-style for an S3-compatible store or the test fake
(`go run ./internal/objstore/s3fake/cmd/s3fake --root DIR --bucket NAME` from `mcp/`, which serves a folder as one
bucket and verifies every signature). The slice's server
does not run that fetcher yet (ARCHITECTURE.md 12): `fit_start` with `env` answers a protocol error (`-32602`), and
`context` and `predict` with `env` use a baseline fit that another step stored in `state_dir/server` (`fit.no_baseline`
until one is). That step makes the fit with `--name`, and stores it as the server's store lays out its files (folders
0700, files 0600): the fit's bytes in `artifacts/<hex>`, named by their sha256, and `baselines/<env>.json`,
`{"fit":"sha256:<hex>"}`, for an environment of the configuration:

```
$ bin/rhobound check RAW --window START,END --cut entry-start --out CUT --request-count COUNT --json
$ bin/rhobound fit --manifest CUT/manifest.json --name staging --out FIT --json
$ hex=$(sha256sum FIT/fit.json | cut -d' ' -f1)
$ install -d -m 0700 STATE/server/artifacts STATE/server/baselines
$ install -m 0600 FIT/fit.json STATE/server/artifacts/$hex
$ (umask 077; printf '{"fit":"sha256:%s"}' "$hex" > STATE/server/baselines/staging.json)
```

`rhobound-mcp refit` does those steps as one command, from the environment's trace store as the configuration names it
(a `dir` store laid out as the S3 exporter writes, `env=.../year=.../minute=.../traces_<id>.json.gz` under `root`): it
fetches the window (DATA.md 4.3, with the store's margins), runs `check --window --cut entry-start` (with
`--request-count` when one is given) and `fit --manifest --name ENV` through the configured engine, stores the fit's
bytes under `state_dir/server/artifacts` and moves `baselines/ENV.json` to it, and prints the result (`--json`:
`rhobound-mcp.refit/v1` with the fit's digest, the previous baseline, the cut's manifest digest and every finding).
A step that refuses the window (the fetcher's or the check's coded refusal, the engine's refusal of the fit) exits 4
with the refusal and the findings as JSON, and the baseline stays as it was; the window is `--window START,END` or
`--last D` (ending `--settle` before now, by default the store's export margin: its `retry_s + batch_s + sdk_delay_s`,
or 7 min), the shape a schedule asks for:

```
$ bin/rhobound-mcp refit --config config.json --env staging --last 10m --request-count COUNT --json
$ bin/rhobound-mcp refit --config config.json --env staging --window START,END --keep out/staging
```

The manual steps below are what it does; they remain for a fit made elsewhere (another machine, another store).

`STATE` is the configured `state_dir`, and `COUNT` the load generator's count of the window's requests
(`rhobound.request-count/v1`, DATA.md 4.3). The cut's manifest then records the count, and the fit's `manifest_digest`
is the `manifest_sha256` the cut printed. Without `--request-count` the cut and the fit carry the doubt
`trace.sampling_unverified`, which the default policy's `doubts_that_hold` lists, so every preview on that baseline is
`inconclusive` at best. Without `--name` the fit records the folder it was given (here `CUT/manifest.json`), and the
server shows only its last element, through the name rule: a capture recorded as a path that holds an ARN is hashed
whole, as `svc_<hash8>`. Make the fit with the configured engine binary: the numbers are its. The server does not trust
a stored record to be its engine's all the same, and shows every record through its own rules (ARCHITECTURE.md 5.9): a
record that an older build made, with an ECS task's whole ARN as a pod, shows the task's id, or `svc_<hash8>` of the
whole ARN when that id holds the account or 12 digits in a row (as written or with its `-` taken out), as the name rule
shows any ARN whose last element does. The engine checks the fields of a stored fit or A/B record that a caller would
read as they are: the tool, the options' enums and digests, the kept share and its source, the manifest digest, an A/B
record's standard errors' method and digests, and every finding, whose code must be registered and whose level and fix
must be the code's; and every node a fit's inference names (in the model, a varying call, a call shape or a stage node's
name) must be one of its rows. A record that fails is refused with `fit.version` on every surface, so a record edited by
hand or written by another build is never shown; its names (services, pods, the capture, a finding's subject and
examples) pass the name rule. A fit made by a build whose registry differs (an upgrade that rewords a fix) is refused
the same way, so fit the baseline again after upgrading the engine. Store the fit's bytes as `fit` wrote them, as
`install` does above: a record serialized again by another tool (a JSON pretty-printer, a language's JSON library) has
another sha256 than the one the engine computes for it, and is refused with `fit.version` too. On `context` and
`predict` with `env`, that refusal's fix is the server's own (wait for the environment's scheduled refresh; over stdio,
where a capture is configured, the fix also offers "name a `fit_id`"), since the caller cannot fit an environment again.
A fit or A/B record that `fit_start` made before an upgrade is made again by repeating the request, since its
idempotency key holds the engine's sha256. Stamp the engine with a version the records' rule admits (docs/BUILD.md,
"With version information"): a build stamped otherwise refuses to fit. `job_status` rebuilds a stored job record from
the registry's level and fix for each code, and the store returns a job only when its state, step, result and id are
ones the server writes (ARCHITECTURE.md 5.5). A baseline whose artifact is missing answers an internal error, and one
whose window ended more than `fit_ttl_h` ago answers `fit.expired`.

Registering the server with an agent (check the syntax against each client's documentation [K]):

```json
{"mcpServers": {"rhobound": {"command": "rhobound-mcp",
  "args": ["stdio", "--config", "/path/to/rhobound-mcp/config.json"]}}}
```

```toml
# ~/.codex/config.toml
[mcp_servers.rhobound]
command = "rhobound-mcp"
args = ["stdio", "--config", "/path/to/rhobound-mcp/config.json"]
```

A remote deployment is added as an HTTP server, for example `claude mcp add --transport http rhobound
https://mcp.example.com/mcp`, and the client runs the OAuth flow [V: https://code.claude.com/docs/en/mcp].

`rhobound-mcp http --config F --listen 127.0.0.1:8000 --static-tokens tokens.json` runs the HTTP transport locally
for testing, bound to loopback, with Origin checking on. The HTTP mode refuses to start without an authentication
flag: `--static-tokens FILE` (hashed read-only tokens, `{"tokens": [{"sha256", "repo", "expires"}]}`), or a JWT issuer
(`--jwt-issuer URL --jwks-url URL` with `--jwt-audience A` or `--jwt-client-ids IDS`, and `--jwt-token-use access` for
Cognito; `--jwt-resource-server ID` when the scopes carry a resource server's prefix, as Cognito's do). `--resource
URL` serves the Protected Resource Metadata. The modes are `stdio`, `http`, `lambda`, `fit-worker` and `version`.

The version is a variable set at link time, as for the CLI (docs/BUILD.md). `rhobound-mcp version --json` prints the
same keys as `rhobound version --json`: the name, the version, the Go release, the architecture and, when the binary was
built inside a git checkout, the source it was built from (`vcs`); `docs/mcp/schemas/version.v1.json` is its schema.

```
$ (cd mcp && go build -ldflags "-X main.Version=0.1.0" -o ../bin/rhobound-mcp ./cmd/rhobound-mcp)
$ bin/rhobound-mcp version --json
{"go":"go1.27.2","goarch":"arm64","name":"rhobound-mcp","vcs":{"modified":false,"revision":"<40 hex digits>","time":"<commit time, RFC 3339>"},"version":"0.1.0"}
```

## 15. Not verified

- The JWT authorizer's 401 behaviour and each client's fallback to the well-known metadata URI [U].
- API Gateway's handling of the MCP standard headers [K: proxies pass headers through].
- AgentCore header forwarding and session behaviour for stateless clients [U].
- Cognito support for client ID metadata documents [U]; its lack of dynamic client registration [K].
- Go cold-start times on `provided.al2023` arm64 [U]: measured at the first deploy.
- `awss3exporter` on directory buckets; the ADOT component list (read as flattened text) [V*].
- ECR end to end with cosign bundles as referrers [U].
- Every price marked [K] or from a secondary source.
