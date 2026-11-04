# rhobound

**Know what a code change will do to production latency before you deploy it.**

rhobound is a deploy gate for coding agents and CI. A change goes in: code written by a person or by an agent such as
Meta's Muse Code. rhobound measures the changed services side by side, off production. Then it predicts the mean
latency of every user-facing request in the live system. The verdict comes out signed: **pass**, **block** or
**inconclusive**. Production is deployed only on a signed pass.

> **An extension of [rhobound-cli](https://github.com/Sharvil2704/rhobound-cli),** the open-source latency engine. This
> repository adds:
> - **model corrections found on live AWS beds:** routed services as route nodes, calls made at once to one service as
>   an exact batch queue (M^X/PH/c), and a Šidák-corrected variance test;
> - **the measured A/B and the signed deploy gate,** the MCP server, the Muse Code plugin, the AWS bed and the live
>   endpoint;
> - **more rigorous testing:** 176 test files against the CLI's 58, the race detector, and byte-for-byte golden runs
>   against the CLI's last release, in CI.
>
> What it adds to the CLI, command by command: [docs/mcp/CHANGES.md](docs/mcp/CHANGES.md).

## What it is

| part | what it does | where |
|---|---|---|
| **Engine** | Rebuilds the call graph and every service's queue from OpenTelemetry traces, then predicts mean latency before and after a change. | [cmd/](cmd/), [internal/](internal/) |
| **Measurement** | An A/B of each changed image on its own node, off production, with the same traffic as the production image. | [bed/tools1/](bed/tools1/) |
| **Gate** | GitHub Actions: build by digest, A/B, prediction, verdict signed with AWS KMS. Only a signed pass deploys. | [bed/repo/](bed/repo/README.md) |
| **Agent side** | An MCP server with six tools, and a native Muse Code plugin. | [mcp/](mcp/), [integrations/muse-code/](integrations/muse-code/README.md) |
| **Test bed** | Ten AWS machines running *maze*, a 32-service Go app built to stress the model. | [bed/](bed/README.md) |

## Built before and during the hackathon

| | built |
|---|---|
| **Before Oct 26** | **The engine:** a Go CLI that predicts mean latency from traces. Its median error on entry latency was 2.0% over 288 simulated scenarios. On Kubernetes it predicted 46.15 ms against 46.10 ms measured. |
| Oct 26-27 | **The engine became a gate:** `fit` once and predict many times, `diff-fit` (an A/B as a measured change), `budget`, `gate`, and `judge-packet` for a second model. **The MCP server.** |
| Oct 30-31 | **The first live AWS bed:** KMS-signed verdicts, images by digest. A change written by Meta's Muse Spark 1.3 through MCP passed it. |
| Nov 1-2 | **maze** and cluster tests 2 and 3. In test 3 the gate made no wrong pass and no wrong block. **The Muse Code plugin.** |
| Nov 3-4 | **The Muse Code experiments,** the deploy-time measurement, **multi-image changes with parallel A/Bs,** and the public release |

## Results, measured on AWS

Every prediction was committed before production was measured
([docs/RESULTS.md](docs/RESULTS.md)).

| | result |
|---|---|
| **Every verdict came true** | 8 passes, all measured clean; 2 blocks, both real regressions (+35 to +38 ms); no wrong pass |
| **Stopped before users saw it** | A costly three-image change was blocked in 9.7 min, with the culprit named. A 5-min bake took 15.5 min to see it, while users ran +38 ms for 9 min. |
| **Faster than a bake** | One image: 7.5 min against 15.4. Two images: 9.2 min against 15.5. |
| **Parallel A/Bs** | Three A/Bs in 572 s instead of 1,400 |
| **Predictions inside their bands** | 25 of 25 entries in the multi-image runs, and 34 of 35 in the deploy-time runs |

## How Meta's models are used

- **Muse Code (1.4.14)** is the coding agent rhobound plugs into. The plugin has a native manifest and three skills: read the budget before coding and predict after, setup, and an optional self-review. It also has an opt-in Stop hook. Its MCP server runs in optional mode, so a failure never stops a Muse session.
- **Muse Spark 1.3** writes the code.
  - **The experiments:** on two identical AWS beds, Muse Code took tickets with hidden tests, with and without the plugin. Every change it shipped was measured in production, and none regressed ([reports](bed/reports/muse-live/)).
  - **The first bed:** an agent on the Meta Model API wrote the change and called rhobound's tools over MCP.
- **Muse Wonder 1.0** is a second, independent model that reviews the gate's evidence packet. `rhobound judge-check` accepts only a well-formed answer.
  - On the first bed it ran inside the gate step.
  - Here it is a pluggable step ([meta-judge.py](bed/tools1/judge/meta-judge.py)), not wired into the maze gate.

## Live demo: Meta's agent asks rhobound on AWS

Muse Code on a laptop, running Muse Spark 1.3 with the rhobound plugin, calls rhobound hosted on AWS: one machine, MCP
over HTTPS, a read-only token. Asked whether a feature that adds 4 ms to service b is safe to ship, it read the
plugin's skill, called `context`, `budget` and `predict` on the hosted endpoint, and answered (2026-11-04):

```text
◆ Not safe to ship. Preview verdict: block for entry a.
  • Entry a mean is ~45.22 ms at ~25.8 rps.
  • Budget for b: safe 1.07 ms, max 1.95 ms per visit. Your +4 ms is over both.
  • Prediction for +4 ms on b: entry a rises ~4.70 ms, over its 2.26 ms threshold.
```

At +1 ms it answered `inconclusive`: the change fits the budget, but a declared estimate cannot pass, and only a
measured A/B of b can. It then planned the feature inside the 1.07-ms budget. The endpoint answers live, on a recorded
sample capture: the full run, with a live app's OpenTelemetry traces, A/B tests and real pull requests, takes 2 to 3
hours ([JUDGES.md, tier 2](JUDGES.md#tier-2-the-gate-live-on-aws)).

**Try it yourself, against the same endpoint (live until 2026-11-11):** [demo/README.md](demo/README.md).

## Try it

### 1. Offline, about 10 minutes (Go 1.25+, python3, git, jq; no cloud)

```bash
git clone https://github.com/Sharvil2704/rhobound-mcp && cd rhobound-mcp
```

```bash
bash demo/judges.sh --quick
```

It builds everything and runs the agent's MCP loop on sample traces: a measured change passes, and one over the budget
is blocked. It then installs the Muse Code plugin into a scratch home, checks the bed against a fake AWS, and prints
the results. Expected output and the full test suites:
[JUDGES.md, tier 1](JUDGES.md#tier-1-offline-about-10-minutes).

The live demo, Muse Code against rhobound on AWS, is [demo/README.md](demo/README.md).

### 2. The plugin in your own Muse Code

1. Build both binaries and install the plugin:
   ```bash
   go build -o ~/.local/bin/rhobound ./cmd/rhobound && (cd mcp && go build -o ~/.local/bin/rhobound-mcp ./cmd/rhobound-mcp) && export PATH="$HOME/.local/bin:$PATH"
   ```
   ```bash
   muse plugins install ./integrations/muse-code && muse plugins approve rhobound
   ```
2. Register the server with `rhobound-mcp init`, giving it the engine, a capture and a policy:
   - run it as a dry run;
   - then with `--write`;
   - then once more, which must report `keep`.

   The commands are in [JUDGES.md, part 4](JUDGES.md#4-the-plugin-in-your-own-muse-code).

### 3. The gate live on AWS, about 1 h 45 min (about 4 USD)

Every command and variable is in [JUDGES.md, tier 2](JUDGES.md#tier-2-the-gate-live-on-aws).

1. **Workstation:** clone, `cd rhobound-mcp/bed` (run every workstation step from there), make an ssh key, and note
   your public IPv4.
2. **AWS CloudShell:** download `bed/aws/spawn_two.sh`, then run `spawn_two.sh plan` and `spawn_two.sh up`. Save the
   inventory to `state/inventory.txt`.
3. **Connect:**
   ```bash
   bash orch/rbd_connect.sh seed state/inventory.txt && bash orch/rbd_connect.sh test
   ```
4. **Create your private app repository:**
   ```bash
   export REPO=<you>/maze-app RBD_GH_REPO=<you>/maze-app && bash orch/publish_main.sh
   ```
5. **Bootstrap** (about 45 min):
   ```bash
   bash orch/rbd_bootstrap.sh build
   bash orch/rbd_bootstrap.sh core
   bash orch/rbd_connect.sh ssh rbd-tools1 'printf "AB_SECONDS=300\nRBD_WINDOW_S=300\n" | sudo tee /opt/rbd/etc/rbd.env.local'
   bash orch/rbd.sh refit prod-0
   bash orch/rbd_bootstrap.sh mcp runner verify
   ```
6. **Two real pull requests,** about 10 min each. `pass` changes pricing and media; `block` adds a costly media filter
   and is stopped before production:
   ```bash
   bash orch/demo_live.sh pass block
   ```
7. **Tear down** from CloudShell:
   ```bash
   BED=1 bash spawn_two.sh down
   ```

### 4. A challenge for your own agent

[rhobound-maze-challenge](https://github.com/Sharvil2704/rhobound-maze-challenge) is maze with a correct but slow
thumbnail filter in production. The ticket asks an agent to make it fast without changing a byte of output. The gate
then measures what the fix does to every user-facing request before it deploys. It runs on the same bed as step 3.

## Repository map

| path | what is in it |
|---|---|
| [cmd/](cmd/), [internal/](internal/) | the engine and its CLI |
| [mcp/](mcp/) | the MCP server |
| [integrations/muse-code/](integrations/muse-code/README.md) | the Muse Code plugin |
| [bed/](bed/README.md) | the AWS bed, the maze app and its gate, the experiment reports |
| [docs/](docs/) | [results](docs/RESULTS.md), [usage](docs/USAGE.md), [trace requirements](docs/REQUIREMENTS.md), [architecture](docs/mcp/ARCHITECTURE.md) |
| [demo/](demo/), [examples/](examples/), [testdata/](testdata/) | the judges' demo, MCP client configurations, sample traces |
| [e2e/](e2e/), [validation/](validation/), [tools/](tools/), [rust/](rust/README.md) | end-to-end and real-cluster tests, tools, an optional Rust build |

## License

Apache License 2.0 ([LICENSE](LICENSE), [NOTICE](NOTICE); third-party modules in [mcp/THIRD-PARTY.md](mcp/THIRD-PARTY.md)).
