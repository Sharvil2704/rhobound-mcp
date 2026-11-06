# rhobound-mcp: what it adds to rhobound-cli

> **History.** This page records the October 2026 demo on the kshop bed. That bed has been replaced in `bed/` by the maze
> bed, and its files are in this branch's history (commit 04ea0e9). The current results are in
> [docs/RESULTS.md](../RESULTS.md).

Base: [rhobound-cli `main`](https://github.com/Sharvil2704/rhobound-cli/tree/main) at `1a2df09` (2026-10-21), a CLI that
predicts mean latency from traces for a change you state by hand. This branch (2026-10-27 to 2026-10-31) makes the
prediction **measured, gated, agent-facing and deployable**: the change comes from an A/B of the real image, the verdict
gates CI, a coding agent asks it over MCP, and the whole loop runs on AWS.

`git diff --stat 1a2df09 rhobound-mcp`: 678 files, +134,830 / -326.

| Area | Files | Lines | What |
|---|---|---|---|
| `cmd/`, `internal/` (CLI) | 102 | +29,739 | data contract `check`, `fit`, `predict --fit --changes`, `--load-scale`, `budget`, `context`, `preview`, `gate` (policy, `block_band`), `diff-fit` (A/B record), `explain`, `judge-packet` / `judge-check`, coded refusals |
| `mcp/` | 99 | +25,828 | `rhobound-mcp`: MCP server (stdio, HTTP with token), tools `context`, `budget`, `predict`, `explain`, `fit_start`, `job_status` |
| `bed/` | 372 | +35,036 | the AWS deploy gate: CloudShell launcher, cluster bootstrap, CI steps, A/B driver, Meta agent and judge, signed verdicts, truth measurement |
| `docs/` | 39 | +8,463 | architecture, data contract, schemas, AWS deployment, usage of every new command, [RESULTS.md](RESULTS.md) |
| `testdata/`, `examples/`, `tools/` | 57 | +35,501 | golden outputs, MCP walkthrough, judge examples, contract generator |
| `rust/` | 4 | +125 | rbfast: refuses topologies it cannot read exactly; `rhobound-fast.sh` front door (rbfast where it matches Go, Go elsewhere) |

## The model

rhobound models every service as a queue (its own time, worker count, replicas, call graph and visit counts, all read
from OpenTelemetry traces) and composes them through sequential calls and parallel fan-outs, so a change's extra wait at
a busy service and its effect on every caller are predicted, not only the added time. New here:

- **Fit once, predict many:** `rhobound fit CAPTURE --out F` stores the system; `rhobound predict --fit F --changes C`
  answers in ~27 ms.
- **Measured changes:** `rhobound diff-fit` turns an A/B of the old and new image into a change item (own-time delta
  with its standard error); several items (several services, several images) predict together.
- **Load what-if:** `--load-scale S` predicts the change at S x today's traffic.
- **Budgets:** `rhobound budget` / `context` give how much time a service may add per call before an entry crosses the
  policy limit; an agent reads them while it edits.
- **Gate:** `rhobound gate` applies a policy (limit per entry, `block_band: model` blocks on the model band, doubts hold
  for review) and writes `gate.json`, `summary.md` and an exit code (0 pass, 1 block, 3 hold).

## Use

```bash
go build -o bin/rhobound ./cmd/rhobound && (cd mcp && go build -o ../bin/rhobound-mcp ./cmd/rhobound-mcp)
bin/rhobound check CAPTURE                                   # is the capture usable, and why not
bin/rhobound fit CAPTURE --out fit/                          # the production fit
bin/rhobound diff-fit --baseline A --candidate B --out ab/   # an A/B of old vs new image -> a measured change
bin/rhobound predict --fit fit/fit.json --changes change.json --load-scale 1.2 --json > prediction.json
bin/rhobound gate prediction.json --policy policy.json --out gate/   # exit 0 / 1 / 3
bin/rhobound budget --fit fit/fit.json --service=pricing --json   # what a service may still add
bin/rhobound-mcp stdio --config examples/mcp/config.example.json   # for a coding agent
```

Every flag, exit code and refusal: [USAGE.md](../USAGE.md) section 11; the loop an agent follows:
[testdata/mcp/](../../testdata/mcp/README.md); the full CI and AWS setup: [bed/](../../bed/README.md).

## What it is for, and not for

For: the mean latency of every service and user-facing request, before a deploy, for a change that adds or removes time
in one or more services: code, CPU, waits, locks, a shared library or a base image used by many services, several images
at once; at today's load or a scaled one.

Not for: tails (p95 / p99), bursty or non-steady load, retries, timeouts and autoscaling, utilization above 85% of the
busiest service, or changes that alter a request's call graph without an A/B.

## Results

On AWS with kshop (10 services) at 100 req/s, every prediction sealed before production was measured: a shared library in
5 services, storefront +4.54..+6.55 predicted against +5.32 measured, checkout +12.80..+18.09 against +14.28; two images at
once, storefront +10.55..+13.20 against +11.87; a fan-out change +7.21..+8.18 against +8.17; peak load from a today's A/B,
+16.35 against +16.27. Six regressions blocked, every no-op and the Meta agent's change passed. Details, the Meta
orchestration and the time saved: [RESULTS.md](RESULTS.md).

## Since the public release (2026-11-06)

- `rhobound count`: an independent request count (`rhobound.request-count/v1`) from an ALB access log, JSON-lines
  access logs or a Prometheus instant query, for `check --request-count` ([USAGE.md](../USAGE.md) 11, [DATA.md](DATA.md) 6).
  Before it, the count was a file written by hand.
- `rhobound-mcp refit --config FILE --env NAME --window START,END|--last D [--request-count F] [--keep DIR] [--json]`:
  an environment's baseline fit from its trace store in one command (fetch, `check --cut`, `fit --name ENV`, store the
  artifact, move the baseline), for a schedule or by hand; a refused window leaves the baseline as it was
  ([DEPLOY_AWS.md](DEPLOY_AWS.md) 14). `fit_start` with `env` still answers that the fetcher is not wired into the server.
- The `s3` store (`mcp/internal/objstore`): a read-only S3 client in the standard library (Signature Version 4,
  ListObjectsV2 paging, ranged reads, bounded retries), so `refit` and the fetcher read an environment's window from
  the bucket the Collector writes to; configured by `bucket`, `region` and an optional `endpoint`
  ([DEPLOY_AWS.md](DEPLOY_AWS.md) 14). Credentials come from the environment only. A fake S3 endpoint for tests and
  dry runs (`s3fake`) verifies signatures with the credentials it is given.
