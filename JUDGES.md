# For judges: what rhobound is, and how to check it yourself

**rhobound predicts what a code change will do to production's mean latency before you deploy it.**
- **How:** it measures the changed services side by side (an A/B on a separate node, off production) and runs a
  queueing model of the live system, fitted from its OpenTelemetry traces.
- **Who uses it:**
  - a coding agent, through an MCP server and a Muse Code plugin;
  - CI, through a signed deploy gate.

**The claim, measured on AWS** ([docs/RESULTS.md](docs/RESULTS.md)):
- every verdict the gate gave came true: 8 passes clean, 2 blocks real regressions, 0 wrong passes;
- it stopped a +38 ms regression before any user saw it, and named the culprit;
- on one- and two-image changes it gives its verdict faster than a 5-minute bake: 7.5 and 9.2 min against 15.4.

**Its one cost:** when it cannot decide, it holds the change (7 holds, all on clean changes).

| tier | what you check | time | cost | needs |
|---|---|---|---|---|
| [1](#tier-1-offline-about-10-minutes) | the engine, the MCP loop, the plugin's install, every test suite | 1-20 min | none | a laptop: Go, python3, git, jq |
| [2](#tier-2-the-gate-live-on-aws) | the gate live on a 10-machine AWS bed: one pass, one block | about 1 h 45 min | about 4 USD | AWS, GitHub, a workstation with ssh |
| [3](#tier-3-the-agent-experiments) | Muse Code with and without the plugin | a day | about 2 USD/h per bed | tier 2, Muse Code and a Meta API key |

## Tier 1: offline, about 10 minutes

No cloud, no credentials. Network only for Go's module download.

**What you need:**
- Go 1.25 or later;
- python3, git and jq;
- shellcheck, optionally (one CI test uses it).

On Debian or Ubuntu: `sudo apt-get update && sudo apt-get install -y python3 git jq shellcheck curl`, and Go from
https://go.dev/dl/ (download the archive for your machine, `sudo tar -C /usr/local -xzf` it, and add `/usr/local/go/bin`
to your `PATH`). On macOS:
`brew install go python git jq shellcheck`.

### 1. Clone and run

```bash
git clone https://github.com/Sharvil2704/rhobound-mcp && cd rhobound-mcp
```

```bash
bash demo/judges.sh --quick
```

`--quick` takes under a minute: 23 s on a fresh machine, with empty Go caches. Without `--quick`, the CI pipeline's and
the measurement scripts' test suites also run (about 2 minutes). Scratch files go to `$RBD_SCRATCH`, by default
`$TMPDIR/rbd`.

### 2. What you should see

These are the real lines of a run on this commit. The ids and paths differ on yours.

**Parts 1 and 2: the build, then the agent's loop over MCP** on the repository's sample traces
(`examples/mcp/demo.sh`):

```
1. fit_start  traces of a -> b -> c, 3,099 requests: fit_01M6HEV649F4CRZTTCYC941Z7S
2. context    entry a: model 45.22 ms, observed 43.74 ms
3. budget     service b may add 1.07 ms per visit (limit reached at 1.95)
4. fit_start  A/B of the candidate image of b: ab_01M6HEV6FRGR0V2R8KHGD4PAZV
5. predict    the measured A/B: entry a -1.43 .. +2.26 ms (limit 2.26 ms) -> pass
6. predict    b +3 ms, over the budget: entry a +2.48 .. +4.53 ms (limit 2.26 ms) -> block (mean_change, unmeasured_delta)
   0.64 s for the whole loop
```

An agent fits production, reads the model's view of an entry and the latency budget of the service it is changing,
measures its change, and predicts. The measured change passes; a change over the budget is blocked.

**Part 3: the Muse Code plugin**, installed for real into a scratch home (`rhobound-mcp init`):

```
   dry run: {"config_action":"create","muse_settings":"add"}
   --write: the server's configuration and Muse's settings entry:
     { "transport": "stdio", "command": ".../bin/rhobound-mcp", "args": ["stdio", "--config", ".../config.json"],
       "enabled": true, "mode": "optional" }
   the check after it: muse_settings keep (keep = installed as written)
```

**Part 4: the AWS bed's own checks, offline:**

```
  -- 25 passed, 0 failed                                (spawn_two.sh against a fake AWS)
[demo ...] pass: --dry: branch demo-pass-... at d07974df71bf, every service tree matches the gated one; nothing pushed
[demo ...] block: --dry: ... every service tree matches the gated one; nothing pushed
```

The live demo's changes are rebuilt from `bed/changes/demo/*.patch` and checked, byte for byte (git tree ids),
against the code the experiments gated.

**Part 5: the results tables** from [docs/RESULTS.md](docs/RESULTS.md).

The script ends with `judges.sh: done`.

### 3. The full test suites (about 20 minutes in all)

| command | what it covers | time |
|---|---|---|
| `go test ./...` | the engine (CLI): model, fit, gate, golden outputs | about 1 min |
| `(cd mcp && go test ./...)` | the MCP server, against the real CLI | about 10 min |
| `bash bed/tools1/tests/ci/run_tests.sh` | the CI steps end to end with the real CLI: pr-check, multi-image builds, parallel A/Bs, predict, sign, deploy | 1-2 min |
| `bash bed/verify/tests/run.sh && bash bed/verify/tests/truthx/run.sh` | the measurement scripts: truth brackets, staged rollouts | about 2 min |
| `bash bed/orch/tests/run_all.sh --quick` | the bootstrap as a dry run, the load tools, the Muse harness's driver | about 6 min |
| `bash bed/aws/tests/run_tests.sh` | `spawn_two.sh` against a fake AWS | under 1 min |

Each prints `ALL PASSED`, or `N passed, 0 failed`. The bed's suites also use `file`, `rsync`, `gpg` and `visudo`, which a
minimal Ubuntu lacks: `sudo apt-get install -y file rsync gnupg sudo`. The CI suite downloads the maze's Go modules once (network), then
compiles offline. GitHub Actions runs the Go and Rust suites on every push (`.github/workflows/ci.yml`).

### 4. The plugin in your own Muse Code

Build both binaries and put them on your `PATH`:

```bash
go build -o ~/.local/bin/rhobound ./cmd/rhobound
```

```bash
(cd mcp && go build -o ~/.local/bin/rhobound-mcp ./cmd/rhobound-mcp)
```

Ubuntu adds `~/.local/bin` to your `PATH` only at login, and only if it existed then. In this terminal:

```bash
export PATH="$HOME/.local/bin:$PATH"
```

Install and approve the plugin:

```bash
muse plugins install ./integrations/muse-code && muse plugins approve rhobound
```

Write the server's configuration and Muse's settings entry. `init` refuses a capture or policy inside the git checkout
of the folder you run it from (a repository must not choose what rhobound reads), so keep them outside any repository.
To try it with the repository's samples, copy them out first, from the clone's root:

```bash
mkdir -p ~/rhobound-try && cp -r testdata/mcp/loop ~/rhobound-try/capture && cp examples/mcp/policy.example.json ~/rhobound-try/policy.json
```

Then, from outside the clone, run the dry run, the same command with `--write`, and `rhobound-mcp init` once more:

```bash
cd ~ && rhobound-mcp init --engine ~/.local/bin/rhobound --capture prod=$HOME/rhobound-try/capture --policy default=$HOME/rhobound-try/policy.json
```

```bash
rhobound-mcp init --engine ~/.local/bin/rhobound --capture prod=$HOME/rhobound-try/capture --policy default=$HOME/rhobound-try/policy.json --write
```

```bash
rhobound-mcp init
```

- The dry run reports `"action": "create"` for the server's configuration and `"add"` for Muse's settings entry.
- `--write` writes `~/.config/rhobound-mcp/config.json` and the entry in `~/.config/muse/settings.json`.
- The last check must report `"keep"` for both.
- For your own system, the capture is a folder of its OpenTelemetry traces, and the policy your gate policy.

The guides are [integrations/muse-code/README.md](integrations/muse-code/README.md) and
[GUIDE.md](integrations/muse-code/GUIDE.md). For Claude Code, Codex or any other MCP client,
[examples/mcp/](examples/mcp/README.md) has the configuration.

## Tier 2: the gate live on AWS

**What you build:** one bed of 10 machines in one AWS region:
- a k3s cluster running the maze, a 32-service app built to break rhobound's model (fan-out batches, routes of
  different cost, payloads, shared images, asynchronous calls);
- an OpenTelemetry Collector;
- a load generator, at 120 req/s;
- three A/B nodes;
- a tools machine that hosts GitHub Actions' self-hosted runner, the rhobound CLI and the MCP server.

Then you open real pull requests on a private app repository, and the gate decides each one live.

**Time and cost:** about 1 h 15 min of setup, then about 10 minutes per demo. About 2 USD per hour of machines, so
about 4 USD in all. The machines terminate themselves 8 hours after launch, unless you extend that.

**You need:**
- **an AWS account,** with 42 on-demand vCPUs free in the region (standard families) and AWS CloudShell;
- **a GitHub account,** with `gh` logged in on your workstation, with the `repo` and `workflow` scopes
  (`gh auth login -s repo,workflow`);
- **a workstation:** Linux or macOS, with ssh, Go 1.25 or later, python3, git, jq and gh. It must be reachable for ssh
  replies, and its public IPv4 must not change during the run. The scripts call it the orchestrator, and `ORCH_IP` is
  its address. It never runs `aws`: it reaches the machines by ssh only, and only CloudShell holds AWS credentials.

### Step 1: the workstation: clone, make a key, note your IP (2 min)

```bash
git clone https://github.com/Sharvil2704/rhobound-mcp && cd rhobound-mcp/bed
```

```bash
mkdir -p ~/.ssh && chmod 700 ~/.ssh && ssh-keygen -t ed25519 -N '' -f ~/.ssh/id_ed25519_rhobound
```

`orch/rbd_connect.sh` uses this key path; set `RBD_KEY=` to use another. If the file exists, keep it: answer `n`.

Note your workstation's public IPv4: your router, your cloud console, or a service such as
`curl -s https://checkip.amazonaws.com`.

### Step 2: CloudShell: spawn the bed (5 min, then 3-5 min of cloud-init)

Open CloudShell in your region. Upload `~/.ssh/id_ed25519_rhobound.pub` (Actions > Upload file) and fetch the script:

```bash
curl -fsSLO https://raw.githubusercontent.com/Sharvil2704/rhobound-mcp/main/bed/aws/spawn_two.sh
```

Check the plan first. It creates nothing, and prints the machines, the price against the budget and the vCPU quota:

```bash
REGION=<your region> ORCH_IP=<workstation IPv4> BED=1 AB_NODE=3 APP_TYPE=m7i.xlarge AB_TYPE=m7i.xlarge LOAD_TYPE=m7i.xlarge SPOT_ROLES="" bash spawn_two.sh plan
```

Then create it. The same line, with `up`, your key and the bucket:

```bash
REGION=<your region> ORCH_IP=<workstation IPv4> ORCH_PUBKEY_FILE=id_ed25519_rhobound.pub CREATE_BUCKET=1 BED=1 AB_NODE=3 APP_TYPE=m7i.xlarge AB_TYPE=m7i.xlarge LOAD_TYPE=m7i.xlarge SPOT_ROLES="" YES=1 bash spawn_two.sh up
```

- **`REGION`:** the default is `ap-south-1`, where the experiments ran.
- **`ORCH_PUBKEY_FILE`** is required for you. Without it, the machines trust only the authors' workstation key.
- **`CREATE_BUCKET=1`** creates the private S3 bucket the machines upload their boot logs to
  (`rhobound-research-ACCOUNT-REGION`; `BUCKET=` names one you have). `YES=1` alone never creates a bucket.
- **The instance types** are the ones the results were measured on: m7i app, A/B and load nodes, the rest m6a.
- **`SPOT_ROLES=""`** puts every machine on demand. Spot is cheaper, but spot nodes were reclaimed twice during the
  experiments.

`up` creates the IAM roles, a KMS signing key, an ECR repository, a VPC of its own, and the 10 machines. On the way it
prints `imported key pair ... (your workstation's key, SHA256:...)`: check that fingerprint against
`ssh-keygen -lf ~/.ssh/id_ed25519_rhobound.pub` on your workstation. It ends with
`done. cloud-init needs about 3-5 minutes per machine`. Then:

```bash
BED=1 bash spawn_two.sh inventory > inventory.txt
```

Download `inventory.txt` (Actions > Download file). It holds the machines' IPs and your account id: keep it private.

### Step 3: the workstation: connect (2 min)

From `rhobound-mcp/bed`:

```bash
mkdir -p state && mv ~/Downloads/inventory.txt state/
```

`state/` is git-ignored, and every file with an IP or the account id goes there.

```bash
bash orch/rbd_connect.sh seed state/inventory.txt && bash orch/rbd_connect.sh test
```

`test` prints one line per machine: `base=` and the time its base setup finished, then its type, CPUs, memory, clock
and time cap.
- `base=NOT-YET`: cloud-init is still running. Wait 3-5 minutes and run `test` again.
- `Connection timed out`: the machines do not accept ssh from your current IP. See the troubleshooting table.

### Step 4: the workstation: the app repository (1 min)

Pick a name for a new private repository, for example `<you>/maze-app`:

```bash
export REPO=<you>/maze-app RBD_GH_REPO=<you>/maze-app
```

```bash
bash orch/publish_main.sh
```

It creates the repository: private, merge commits only, and a read-only workflow token. It pushes `bed/repo` as `main`: the maze's source, the gate
policy `policy/gate.json` (block above +4 ms at any entry) and the workflows `.github/workflows/{gate,deploy}.yml`. A
leak scan runs first. The last line printed is `main`'s sha.

### Step 5: the workstation: bootstrap the bed (about 45 min)

```bash
bash orch/rbd_bootstrap.sh build
```

That builds the engine, the MCP server and the A/B driver from your checkout (about 5 min). It ends with
`built from <sha>` and `done: build`.

```bash
bash orch/rbd_bootstrap.sh core
```

That installs, in order (15-20 min):
- the tools machine;
- k3s on every node, every agent ending `joined`;
- the images, built on the tools machine and pushed to your ECR repository;
- the Collector;
- the maze at baseline;
- the background load.

It ends with `done: core`.

Next, set the two settings the experiments ran with: a 300-s A/B and 300-s baseline windows.

```bash
bash orch/rbd_connect.sh ssh rbd-tools1 'printf "AB_SECONDS=300\nRBD_WINDOW_S=300\n" | sudo tee /opt/rbd/etc/rbd.env.local'
```

```bash
bash orch/rbd.sh refit prod-0
```

That makes the production fit the gate predicts from (about 11 min). Keep the terminal open until it returns.

```bash
bash orch/rbd_bootstrap.sh mcp runner verify
```

That installs the MCP server (on the tools machine, localhost only), the GitHub runner registered to your app repository,
and the measurement scripts. It ends with `done: mcp runner verify`. Your repository's Settings > Actions > Runners
now lists the runner as Idle.

### Step 6: the demo: real PRs, decided live (about 10 min each)

```bash
bash orch/demo_live.sh pass block
```

Each demo:
1. branches your app repository's `main`, applies its patches (real code a coding agent wrote in the experiments,
   `bed/changes/demo/`) and checks each service against the code the experiments gated;
2. opens a PR (never merge it);
3. waits for the gate;
4. prints what the gate decided.

| demo | change | expected (RESULTS-4) |
|---|---|---|
| `pass` | pricing + media, two images | **pass** in about 9 min; production measured clean |
| `block` | rank + pricing + media's sharpen written the direct way, three images | **block** in about 10 min, naming media (utilization 0.80); production measured +38 ms |
| `single` | pricing alone | pass in about 7.5 min |
| `edge` | rank + pricing + media | at the 4-ms bar: **inconclusive or pass**, depending on that run's A/B noise. Do not use it to show a pass |

What it prints, in this format. Your numbers come from your run; the measured ones are in
[RESULTS-4](bed/reports/muse-live/RESULTS-4.md).

```
[demo 14:02:11] pass: pricing media (two images, only media on mobile's path: expect pass)
[demo 14:02:15] pass: PR https://github.com/<you>/maze-app/pull/1; waiting for the gate
  verdict: PASS
    web         predicted change   ... ms   band   ...   <effect>
    mobile      predicted change   ... ms   band   ...   <effect>
    ...         (every user-facing entry)
  rhobound's own CI time (first A/B to signature): 9.x min
  A/Bs at once: 2, 300 s, on rbd-d-ab1, rbd-d-ab2
```

For `block`, the verdict line names the rules that decided it, for example
`verdict: BLOCK (utilization_after, mean_change)`.

**On GitHub,** open the PR > Checks > "rhobound gate" > Summary. It shows:
- the verdict and the rule that decided it;
- each entry's predicted change and band;
- each A/B;
- the image digests;
- how to verify the KMS-signed verdict by hand.

The commit status `rhobound/verdict` carries the verdict. `deploy.yml` deploys only an image whose signed verdict is
pass, so leave the PRs unmerged.

**To also measure production:** `bash orch/demo_live.sh --truth pass` deploys the change once between two baseline
windows, rolls it back, and prints whether each entry landed inside its predicted band. It adds about 18 minutes.

### Step 7: tear down

In CloudShell:

```bash
BED=1 bash spawn_two.sh down
```

That terminates every machine. The VPC, roles, key pair, ECR repository, KMS key and secrets stay; delete them in the
console when you are done (the KMS key can only be scheduled for deletion). Close the demo PRs with
`gh pr close N --repo "$REPO"`, and delete the app repository if you wish.

### Troubleshooting

| what you see | cause | what to do |
|---|---|---|
| `up` refuses: vCPU quota | fewer than 42 on-demand vCPUs free | ask AWS for a quota increase, or free some |
| `up` refuses: budget | the price check is 2.5 USD/h for everything | `BUDGET=3` |
| `up`: bucket not found | no bucket and no `CREATE_BUCKET=1` | add `CREATE_BUCKET=1`, or `BUCKET=<yours>` |
| `up`: no capacity for a type | the zone is out of that type | `up` tries the same size of the next family by itself; run it again to fill gaps |
| `test`: `base=NOT-YET` | cloud-init still running | wait 3-5 min, run `test` again |
| `test`: `Connection timed out` | your public IP changed, or `ORCH_IP` was wrong | `bash orch/rbd_connect.sh myip`, then in CloudShell `BED=1 bash spawn_two.sh allow-ip <your IPv4>` |
| `publish_main.sh` or `runner`: a `gh` error | `REPO` / `RBD_GH_REPO` not exported, or a missing scope | export both; `gh auth refresh -s repo,workflow` |
| a spot machine vanished | spot reclaim (only if you kept spot roles) | run `up` again; then `bash orch/rbd_bootstrap.sh core` (every step is idempotent) |
| the gate stops before a verdict | `demo_live.sh` prints the run id | `gh run view <run> --repo "$REPO" --log-failed` |
| a demo's verdict differs from the table | A/B noise: each verdict is one real measurement | run it again; `edge` is expected to vary |

## Tier 3: the agent experiments

These compared Muse Code (Meta's coding agent) with and without the rhobound plugin, on tickets with hidden tests:
- **tickets:** `bed/tools1/muse/tickets/` (T6 to T9);
- **protocols:** [DECLARE-2](bed/reports/muse-live/DECLARE-2.md), [DECLARE-3](bed/reports/muse-live/DECLARE-3.md) and
  [DECLARE-4](bed/reports/muse-live/DECLARE-4.md), each written before its run;
- **results:** [RESULTS-2](bed/reports/muse-live/RESULTS-2.md) to [RESULTS-4](bed/reports/muse-live/RESULTS-4.md).

They are documented as a protocol, not as a one-command run, because they need software and keys that are not public:
- **Muse Code** 1.4.14 and its `muse-spark-1.3` model;
- **a Meta model API key,** stored from CloudShell with `BED=1 bash spawn_two.sh secret meta-key`. Only the tools
  machine's role can read it.

**Two beds, one per arm.**
- **Bed 1, Muse with the plugin:** the bed of tier 2.
- **Bed 2, Muse alone:** `BED=2 ... bash spawn_two.sh up`. It gets its own VPC (10.83.0.0/16) and no A/B node by
  default. Both beds share the roles, key pair, ECR repository, KMS key and secrets, so `destroy` is refused; take each
  bed `down`.
- **Quota:** both beds need about 72 on-demand vCPUs with spot off.

**Two clones, one per bed.** Each clone has its own `state/`, with a `state/kit.env`. Set `REPO`, `RBD_GH_REPO`, a
separate ssh config (`RBD_SSHC`, `RBD_KH`) and bed 2's subnet (`RBD_SUBNET_RE`) there. Run every command of a bed through
`orch/kitenv.sh`, for example:

```bash
bash orch/kitenv.sh bash orch/rbd_bootstrap.sh core
```

**The harness:**
- **`bed/tools1/muse/install.sh ARM CAPTURE_DIR`,** as root on the tools machine, sets up the sandboxed users, Go, the
  tickets and, for arm R, the plugin. Its header lists the files it expects under `/var/lib/rbd/muse`. Those files (the
  Muse binary, `bed/tools1/muse/`, the tickets, `bed/verify/`, and for arm R `rhobound-mcp` and
  `integrations/muse-code/`) were put there by hand with `orch/rbd.sh put`.
- **`bed/orch/muse_arm.sh TICKET ARM`** runs one ticket for one arm: the agent, the untrusted-output checks, the hidden
  tests, a PR, the gate or the plain deploy, and a truth bracket.
- **`bed/orch/muse2_chain.sh ARM T0`** runs the time-boxed sequence.
- **`bed/orch/muse_score.py`, `p3_score.py` and `p4_score.py`** make the scoreboards.

## Where to look

| what | where |
|---|---|
| the results, with the verdict and its limits | [docs/RESULTS.md](docs/RESULTS.md) |
| the bed: what runs where, the CI pipeline, the measurement scripts | [bed/README.md](bed/README.md) |
| the engine: model, data contract, refusals | [docs/USAGE.md](docs/USAGE.md), [docs/REQUIREMENTS.md](docs/REQUIREMENTS.md), [docs/ACCURACY.md](docs/ACCURACY.md) |
| the MCP server and the gate's design | [docs/mcp/ARCHITECTURE.md](docs/mcp/ARCHITECTURE.md) |
| the Muse Code plugin | [integrations/muse-code/](integrations/muse-code/README.md) |
