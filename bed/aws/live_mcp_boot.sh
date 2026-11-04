#!/usr/bin/env bash
# live_mcp_boot.sh: the boot of live_mcp.sh's machine (its user data, as root, on Ubuntu 24.04 arm64). It builds rhobound
# and rhobound-mcp from this repository at LIVE_SHA, fits the sample traces (testdata/mcp/loop) and registers that fit as
# environment prod's baseline (bed/tools1/mcp/register-fit.sh, as the bed's tools machine does), serves MCP on
# 127.0.0.1:8765, and puts Caddy in front of it on https://<public IP, dashed>.sslip.io with a Let's Encrypt certificate.
# A token is read-only (rhobound:read): callers pass env "prod" to context, budget, predict and explain.
# The bearer tokens are stored only as their sha256 (LIVE_TOKENS). The machine shuts down, and so terminates, after
# LIVE_HOURS hours. Progress goes to the serial console, every line prefixed "rhobound-live:"; the last is "READY URL".
#   live_mcp.sh writes these first: LIVE_SHA (40 hex), LIVE_TOKENS ("NAME=<64 hex> ...", one per token), LIVE_EXPIRES
#   (RFC 3339), LIVE_HOURS
#   LIVE_TEST=1 (tests, in a container): no shutdown, no IMDS (host live.test, not a loopback name, so the Host check
#   below is exercised), no systemd, Caddy with its internal CA
set -euo pipefail
: "${LIVE_SHA:?}" "${LIVE_TOKENS:?}" "${LIVE_EXPIRES:?}" "${LIVE_HOURS:?}"
TEST=${LIVE_TEST:-0}
[[ $LIVE_SHA =~ ^[0-9a-f]{40}$ && $LIVE_HOURS =~ ^[0-9]+$ ]] || { echo "bad parameters" >&2; exit 2; }
for t in $LIVE_TOKENS; do [[ $t =~ ^[a-z0-9-]{1,40}=[0-9a-f]{64}$ ]] || { echo "bad token entry" >&2; exit 2; }; done
if [[ $TEST != 1 ]]; then
  exec > >(tee -a /var/log/rhobound-live.log | sed -u 's/^/rhobound-live: /' > /dev/console) 2>&1
  shutdown -h "+$((LIVE_HOURS * 60))"
fi
say() { echo "[$(date -u +%H:%M:%S)] $*"; }
GO_VERSION=go1.27.2
CADDY_VERSION=2.10.0
CADDY_SHA512=6d100bfd609e8cfe6a51afe1b86066ac68f53ac56670c74a7d7537d93c643aa7f0e82b9f3083218eee1d369a427bfcdafcb445c7a730f9fc0ddf546401d95484
O=/opt/rhobound
export DEBIAN_FRONTEND=noninteractive HOME=/root GOTOOLCHAIN=local PATH=/usr/local/go/bin:$PATH

say "packages"
apt-get update -qq > /dev/null && apt-get install -y -qq git jq curl ca-certificates python3 > /dev/null

say "Go $GO_VERSION from go.dev, checked against its published sha256"
f=$GO_VERSION.linux-arm64.tar.gz
want=$(curl -fsSL "https://go.dev/dl/?mode=json&include=all" | jq -r --arg f "$f" '.[].files[] | select(.filename == $f) | .sha256')
curl -fsSLo "/root/$f" "https://go.dev/dl/$f" && echo "$want  /root/$f" | sha256sum -c - > /dev/null
rm -rf /usr/local/go && tar -C /usr/local -xzf "/root/$f" && rm "/root/$f"

say "rhobound at $LIVE_SHA"
rm -rf "$O" && install -d -m 0755 "$O" "$O/bin" "$O/data"
git clone -q https://github.com/Sharvil2704/rhobound-mcp "$O/src" && git -C "$O/src" checkout -q "$LIVE_SHA"
(cd "$O/src" && go build -trimpath -o "$O/bin/rhobound" ./cmd/rhobound && cd mcp && go build -trimpath -o "$O/bin/rhobound-mcp" ./cmd/rhobound-mcp)

say "the policy (the samples date from 2023: the fit's age limit is raised), the configuration, the token"
cp -r "$O/src/testdata/mcp/loop" "$O/data/loop"
say "the sample traces (2023) re-timed to end a minute ago: every timestamp moves by one constant, so every duration and gap,
  and so the fit, stay as they are (a baseline older than fit_ttl_h, at most a year, is refused as fit.expired)"
python3 -I - "$O/data/loop" << 'PY'
import gzip, json, os, re, sys, time
from datetime import datetime, timezone
d = sys.argv[1]
p = os.path.join(d, "traces", "spans.jsonl.gz")
lines = gzip.open(p, "rt").read().splitlines()
off = time.time_ns() - 60 * 10**9 - max(int(v) for l in lines for v in re.findall(r'"endTimeUnixNano":"(\d+)"', l))
out = [re.sub(r'"(startTimeUnixNano|endTimeUnixNano)":"(\d+)"', lambda m: '"%s":"%d"' % (m.group(1), int(m.group(2)) + off), l)
       for l in lines]
with gzip.open(p, "wt") as f:
    f.write("\n".join(out) + "\n")
def shift(s):   # RFC 3339 with nanoseconds, Z
    head, frac = s.rstrip("Z").split(".")
    ns = int(datetime.strptime(head, "%Y-%m-%dT%H:%M:%S").replace(tzinfo=timezone.utc).timestamp()) * 10**9
    ns += int(frac.ljust(9, "0")) + off
    return datetime.fromtimestamp(ns // 10**9, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ".%09dZ" % (ns % 10**9)
rc = os.path.join(d, "request-count.json")
r = json.load(open(rc))
r["window"] = {k: shift(v) for k, v in r["window"].items()}
json.dump(r, open(rc, "w"), indent=2, sort_keys=True)
print("re-timed by %.0f s; window %s .. %s" % (off / 1e9, r["window"]["start"], r["window"]["end"]))
PY
jq '. + {max_fit_age_h: 1000000}' "$O/src/testdata/mcp/loop/policy.json" > "$O/data/policy.json"
jq -n --arg e "$O/bin/rhobound" --arg s "$(sha256sum "$O/bin/rhobound" | cut -d' ' -f1)" --arg d "$O/data" '{
  schema: "rhobound-mcp.config/v1", engine: {path: $e, sha256: $s}, state_dir: "/var/lib/rhobound-mcp",
  envs: {prod: {store: "dir", root: "\($d)/env-prod"}}, policies: {default: "\($d)/policy.json"}, profile: "default"}' \
  > "$O/config.json"
install -d -m 0755 "$O/data/env-prod"
# shellcheck disable=SC2086  # LIVE_TOKENS is a list of words, checked above
printf '%s\n' $LIVE_TOKENS | jq -R --arg e "$LIVE_EXPIRES" 'split("=") | {sha256: .[1], repo: .[0], expires: $e}' | jq -s '{tokens: .}' > "$O/tokens.json"
id rhobound > /dev/null 2>&1 || useradd --system --home-dir /var/lib/rhobound-mcp --shell /usr/sbin/nologin rhobound
install -d -o rhobound -g rhobound -m 0700 /var/lib/rhobound-mcp
chown root:rhobound "$O/config.json" "$O/tokens.json" && chmod 0640 "$O/config.json" "$O/tokens.json"

say "production's fit: the sample traces, registered as environment prod's baseline"
su -s /bin/bash rhobound -c "set -e; $O/bin/rhobound fit $O/data/loop --name=loop --workers=a=1,b=2,c=1 --out /var/lib/rhobound-mcp/fit-prod > /dev/null
  RBD_MCP_STATE=/var/lib/rhobound-mcp bash $O/src/bed/tools1/mcp/register-fit.sh /var/lib/rhobound-mcp/fit-prod/fit.json prod"

if [[ $TEST == 1 ]]; then
  HOST=live.test
else
  t=$(curl -fsS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
  ip=$(curl -fsS -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/public-ipv4)
  [[ $ip =~ ^[0-9]+(\.[0-9]+){3}$ ]] || { echo "no public IPv4" >&2; exit 1; }
  HOST=${ip//./-}.sslip.io
fi

say "Caddy $CADDY_VERSION, checked against its published sha512"
f=caddy_${CADDY_VERSION}_linux_arm64.tar.gz
curl -fsSLo "/root/$f" "https://github.com/caddyserver/caddy/releases/download/v$CADDY_VERSION/$f"
echo "$CADDY_SHA512  /root/$f" | sha512sum -c - > /dev/null
tar -C /usr/local/bin -xzf "/root/$f" caddy && rm "/root/$f"
id caddy > /dev/null 2>&1 || useradd --system --home-dir /var/lib/caddy --create-home --shell /usr/sbin/nologin caddy
install -d -m 0755 /etc/caddy
{
  echo "$HOST {"
  [[ $TEST == 1 ]] && echo "  tls internal"
  # the upstream's own address as Host: rhobound-mcp listens on loopback, and the MCP SDK's DNS-rebinding protection
  # refuses a request to a loopback listener whose Host is not loopback (Forbidden: invalid Host header)
  echo "  reverse_proxy 127.0.0.1:8765 {"
  echo "    header_up Host {upstream_hostport}"
  echo "  }"
  echo "}"
} > /etc/caddy/Caddyfile
/usr/local/bin/caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile > /root/caddy-validate.log 2>&1 || { cat /root/caddy-validate.log; exit 1; }

MCP=("$O/bin/rhobound-mcp" http --config "$O/config.json" --listen 127.0.0.1:8765 --static-tokens "$O/tokens.json" \
  --resource "https://$HOST/mcp")
if [[ $TEST == 1 ]]; then
  say "starting both in the background (no systemd here)"
  (su -s /bin/sh rhobound -c "cd /var/lib/rhobound-mcp && exec $(printf '%q ' "${MCP[@]}")" > /var/log/rhobound-mcp.log 2>&1 &)
  (HOME=/var/lib/caddy /usr/local/bin/caddy run --config /etc/caddy/Caddyfile --adapter caddyfile > /var/log/caddy.log 2>&1 &)
else
  cat > /etc/systemd/system/rhobound-mcp.service << EOF
[Unit]
Description=rhobound-mcp (live demo)
After=network-online.target
[Service]
User=rhobound
WorkingDirectory=/var/lib/rhobound-mcp
ExecStart=$(printf '%q ' "${MCP[@]}")
Restart=on-failure
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/rhobound-mcp
[Install]
WantedBy=multi-user.target
EOF
  cat > /etc/systemd/system/caddy.service << 'EOF'
[Unit]
Description=Caddy (HTTPS in front of rhobound-mcp)
After=network-online.target rhobound-mcp.service
[Service]
User=caddy
Environment=HOME=/var/lib/caddy
ExecStart=/usr/local/bin/caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
Restart=on-failure
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=full
[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload && systemctl enable --now rhobound-mcp caddy
fi

say "waiting for https://$HOST/healthz (the certificate comes first)"
k=(); [[ $TEST == 1 ]] && k=(-k --resolve "$HOST:443:127.0.0.1")
for _ in $(seq 120); do curl -fsS "${k[@]}" "https://$HOST/healthz" > /dev/null 2>&1 && break; sleep 5; done
curl -fsS "${k[@]}" "https://$HOST/healthz" > /dev/null
say "READY https://$HOST/mcp"
