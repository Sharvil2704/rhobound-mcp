# JSON Schemas of rhobound-mcp

The documents that cross a boundary: between the `rhobound` CLI (the root module, standard library only) and the
`rhobound-mcp` server (the `mcp/` module), which runs the CLI as a subprocess and passes JSON both ways; between CI and
the CLI; and between people and either. Both sides test against these files. They are drafted from
[ARCHITECTURE.md](../ARCHITECTURE.md) and [DATA.md](../DATA.md) and frozen per release; until the first release they
may change, and where a file and those documents disagree the documents say what is meant.

How they are tested: `mcp/internal/schematest` loads this whole folder (references resolved by `$id`, without the
network) and validates the goldens of both modules, the examples and fixtures, and the real output of every CLI
command the server or CI reads, made by a CLI built from this repository on `testdata/mcp/loop`. The server embeds
byte copies of the schemas it serves (`mcp/internal/server/schemas`), and a test there fails when a copy differs from
this folder: after editing one of those files here, copy it there.

## Conventions

- JSON Schema 2020-12. Every file has an `$id` under
  `https://raw.githubusercontent.com/Sharvil2704/rhobound-mcp/main/docs/mcp/schemas/` and refers to the others by
  relative path, so a validator that loads the whole folder resolves every `$ref` without the network.
- `defs.v1.json` holds the shared definitions of ARCHITECTURE.md 5.4 (`Handle`, `Name`, `Route`, `Code`, `Env`,
  `Range`, `Value`, `ChangeItem`, `MeasuredRef`, `Finding`, `Assumption`, `Refusal`) and the ones the other files add
  (`Digest`, `Label`, `KeptShare`, `Tool`, `Float`, `RuleToken`, ...). Every code matches
  `Code`, `^[a-z]+\.[a-z0-9_]+$`; gate rule tokens (`RuleToken`) are a separate namespace.
- Every object is closed (`additionalProperties: false`, or `unevaluatedProperties: false` where a schema extends
  another) unless its description says otherwise. Three are open on purpose: the policy (unknown keys are ignored and
  listed in `gate.json`), the fit record (only its summary fields are a contract; the rest is the engine's lossless
  mirror of the inference, read only by the CLI that wrote it) and `explain`'s `content`.
- The root of each file validates the document named in its `title`. For an MCP tool, the root is the tool's
  output (the view) and `$defs/input` is its input schema.
- **Server keys.** A view is canonical JSON printed by a CLI twin. The server adds only `prediction_id`, `fit_id`,
  `fit_age_s`, `attempt`, `max_preview_attempts`, `escalate` and `links` (ARCHITECTURE.md 5.5). They are optional
  in every view schema, so one schema validates both the server's `structuredContent` and the twin's output.
- Non-finite floats, where a record may hold them (the fit record), are written as the strings `NaN`, `+Inf` and
  `-Inf` (`Float`).

## Files

| file | document | written by | read by |
|---|---|---|---|
| `defs.v1.json` | shared definitions | | every file here |
| `report.v1.json` | `rhobound-cli.report/v1` | `predict CAPTURE --json`, `predict --fit F --report v1 --json`, `model --json` | people, CI scripts |
| `report.v2.json` | `rhobound-cli.report/v2` | `predict --fit F --json` | `gate`, `preview`, `judge-packet`, the server (stored, served as a resource) |
| `fit.v1.json` | `rhobound.fit/v1` (summary fields) | `fit` | the CLI; the server (window, images, digest) |
| `change.v1.json` | `rhobound.change/v1` (`fit` may be `""`: the fit on the command line) | the server, people | `predict --changes`, `preview` |
| `check.v1.json` | `rhobound.check/v1` | `check --json` | the server (job results), CI |
| `capture-manifest.v1.json` | `rhobound.capture-manifest/v1` | the fetcher, `check --write-manifest`, `check --cut` | `check`, `fit` |
| `request-count.v1.json` | `rhobound.request-count/v1` | people, load generators | `check` |
| `peer-map.v1.json` | `rhobound.peer-map/v1` | people (ECS) | `check`, `fit` |
| `ab-record.v1.json` | `rhobound.ab-record/v1` | `diff-fit` | the CLI (labels), the server (measured items) |
| `policy.v1.json` | `rhobound.policy/v1` | people | `gate`, `preview`, `budget` |
| `readiness.v1.json` | `rhobound.readiness/v1` | CI | `gate` |
| `gate.v1.json` | `rhobound.gate/v1` (`gate.json`) | `gate` | CI, people |
| `refusal.v1.json` | `{"error": Refusal}` | every CLI command with `--json` that refuses | the server, CI |
| `context.v1.json` | `rhobound-mcp.context/v1` and the `context` tool's input | `context --json` | the server |
| `budget.v1.json` | `rhobound-mcp.budget/v1` and the `budget` tool's input | `budget --json` | the server |
| `prediction.v1.json` | `rhobound-mcp.prediction/v1` and the `predict` tool's input | `preview --json` | the server |
| `explain.v1.json` | `rhobound-mcp.explain/v1` and the `explain` tool's input | `explain --json` | the server |
| `fit-start.v1.json` | the `fit_start` tool | the server | agents |
| `job-status.v1.json` | the `job_status` tool | the server | agents |
| `judge-packet.v1.json` | `rhobound-mcp.judge-packet/v1` | `judge-packet --json` | judge models |
| `judge-answer.v1.json` | `rhobound-mcp.judge-answer/v1` | judge models | `judge-check` |
| `judge-check.v1.json` | what `judge-check --json` prints: the typed fields only | `judge-check` | agents, CI |
| `perf-profile.v0.1.json` | the `perf-profile/v0.1` statement (DATA.md 9.2) | the profile harness [later] | the verifier, the OCI client |
| `run-record.v1.json` | `rhobound.run-record/v1` | the evaluation studies | the analysis |
| `config.v1.json` | `rhobound-mcp.config/v1` | people | the server |
| `version.v1.json` | what `version --json` prints: the name, the version, the Go release, the architecture and the source (`vcs`) | `rhobound version --json`, `rhobound-mcp version --json` | build and deploy scripts |
| `data-contract.v1.json` | `rhobound.data-contract/v1`, the registry as JSON (`docs/mcp/data-contract.json`) | `go run ./tools/gencontract docs` | people, tools |

## Report v2 and its v1 projection

Report v2 is additive over v1: `report.v1.json` keeps the keys of the v1 report in `$defs/base` and `$defs/rowBase`,
and `report.v2.json` extends both. The v1 projection of a v2 report removes every key those two definitions do not
name, at the top level and in each service row, and sets `version` to `rhobound-cli.report/v1`; `capture` is already
the capture name stored in the fit. On one architecture, `predict CAPTURE --patch P --json` and
`predict --fit F --patch P --report v1 --json` print the same bytes (ARCHITECTURE.md 4.1).
