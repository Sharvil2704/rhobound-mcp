# Live demo: Muse Code asks rhobound on AWS

rhobound's MCP server is running on AWS until **2026-11-11 13:38 UTC**. It runs the real engine, read-only, on a
recorded sample of production (120 s of a simulated three-service chain: `a` calls `b`, which calls `c`). Meta's Muse
Code, with the rhobound plugin, asks it before writing code.

Other ways to run rhobound:
- **Offline, about 10 minutes, no cloud:** `bash demo/judges.sh --quick` ([JUDGES.md, tier 1](../JUDGES.md#tier-1-offline-about-10-minutes)).
- **Your own endpoint on AWS, about 5 minutes:** `bash live_mcp.sh up` in AWS CloudShell ([bed/aws/live_mcp.sh](../bed/aws/live_mcp.sh)).
- **The full gate on AWS, 2 to 3 hours:** real OpenTelemetry traces, A/B tests and pull requests ([JUDGES.md, tier 2](../JUDGES.md#tier-2-the-gate-live-on-aws)).

## 0. Set up an empty machine

On macOS, install Homebrew if you do not have it, then the tools:

```bash
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
```

```bash
brew install git jq
```

On Ubuntu: `sudo apt-get update && sudo apt-get install -y git curl jq`.

Pull the repository the demo needs (its Muse Code plugin is in `integrations/muse-code`):

```bash
git clone https://github.com/Sharvil2704/rhobound-mcp ~/rhobound-mcp
```

Optionally, the challenge for your own agent: a 32-service app with a correct but slow filter to make fast
([its README](https://github.com/Sharvil2704/rhobound-maze-challenge)).

```bash
git clone https://github.com/Sharvil2704/rhobound-maze-challenge ~/maze-challenge
```

## 1. Try the endpoint from a terminal

```bash
export RHOBOUND_TOKEN=0c43abc013e07f6683f3b9b13a99d453224d3954c1b4b8ada114faf00c08856b H=3-111-197-50.sslip.io
```

```bash
curl -s https://$H/mcp -H "Authorization: Bearer $RHOBOUND_TOKEN" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"predict","arguments":{"env":"prod","changes":[{"op":"own_ms","service":"b","value":4}]}}}' | jq '.result.structuredContent.preview.verdict'
```

It prints `"block"`: adding 4 ms to service `b` would break entry `a`'s latency limit.

## 2. Ask Muse Code

You need the setup of part 0 and a Meta API key. These commands are for macOS on Apple silicon; on Linux, use
`A=aarch64_linux` or `A=x86_linux`.

1. Install Muse Code from Meta's release manifest, checked against its sha256:
   ```bash
   curl -fsS "$(curl -fsS https://api.meta.ai/muse-code/channels/muse-stable | jq -r .manifest_url)" -o ~/muse-manifest.json
   ```
   ```bash
   A=aarch64_macos && mkdir -p ~/.local/bin && curl -fsS "$(jq -r .artifacts.$A.url ~/muse-manifest.json)" -o ~/.local/bin/muse && echo "$(jq -r .artifacts.$A.checksum ~/muse-manifest.json)  $HOME/.local/bin/muse" | shasum -a 256 -c - && chmod 755 ~/.local/bin/muse && export PATH="$HOME/.local/bin:$PATH"
   ```
2. Install the rhobound plugin from the clone of part 0:
   ```bash
   muse plugins install ~/rhobound-mcp/integrations/muse-code && muse plugins approve rhobound
   ```
3. Point Muse at the live endpoint. Muse reads the token from `RHOBOUND_TOKEN`, so it is never written to the file:
   ```bash
   f=~/.config/muse/settings.json && mkdir -p ~/.config/muse && { [ -f "$f" ] || echo '{"schema_version": 1}' > "$f"; } && jq --arg u "https://$H/mcp" '(if has("mcpServers") then "mcpServers" else "mcp_servers" end) as $k | .[$k].rhobound = {transport: "streamable_http", url: $u, headers: {Authorization: "Bearer ${RHOBOUND_TOKEN}"}, enabled: true, mode: "optional"}' "$f" > "$f.new" && mv "$f.new" "$f"
   ```
4. Give Muse your Meta key. It is read without echo and stored by Muse (`muse login` with a Meta account also works):
   ```bash
   unset META_API_KEY; ( printf 'Meta API key: '; stty -echo; read -r K; stty echo; echo; printf '%s' "$K" | muse auth set --api-key-stdin )
   ```
5. Start Muse in an empty folder, in the terminal where `RHOBOUND_TOKEN` is set:
   ```bash
   mkdir -p ~/rhobound-try && cd ~/rhobound-try && muse --model muse-spark-1.3 --reasoning-effort high --trust-workspace
   ```
6. In Muse, type `/mcp` to see `rhobound` connected, then ask:
   ```text
   Service b is called by a, our user-facing entry, and calls c. A new feature will add about 4 ms of work to every call of b. Before you write any code, use the rhobound tools on the environment prod: read entry a's context and b's latency budget, then predict the +4 ms change. Is it safe to ship?
   ```
   ```text
   What if b only adds 1 ms? Predict that on prod.
   ```
   ```text
   So how would you build the feature without breaking that budget?
   ```

## 3. What Muse answered (2026-11-04)

```text
◆ Read skill rhobound-loop · rhobound · 3 KB
◆ 3 MCP calls · rhobound · ✓ ×3 · 0.6s
◆ Not safe to ship. Preview verdict: block for entry a.
  • Entry a mean is ~45.22 ms at ~25.8 rps.
  • Budget for b: safe 1.07 ms, max 1.95 ms per visit. Your +4 ms is over both.
  • Prediction for +4 ms on b: entry a rises ~4.70 ms, over its 2.26 ms threshold.
```

- **At +1 ms:** `inconclusive`. The change fits the budget, but an estimate cannot pass; only a measured A/B of `b`
  can.
- **The plan:** keep `b`'s added work under 1.07 ms (cache, async, or a faster check), then prove it with an A/B.

The wording is the model's own; the numbers are rhobound's, computed live on the AWS machine.

## Clean up

```bash
muse plugins remove rhobound --delete-data; muse logout
```

```bash
f=~/.config/muse/settings.json && jq 'del(.mcpServers.rhobound, .mcp_servers.rhobound)' "$f" > "$f.new" && mv "$f.new" "$f"; rm -rf ~/rhobound-mcp ~/maze-challenge ~/rhobound-try ~/muse-manifest.json
```

To remove Muse Code too: `rm -rf ~/.local/bin/muse ~/.config/muse ~/.local/share/muse "$HOME/Library/Application Support/Muse"`.
