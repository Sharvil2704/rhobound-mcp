#!/usr/bin/env bash
# live_mcp.sh: rhobound's MCP server live on AWS for a demo: one small arm64 machine behind HTTPS, read-only tokens.
# Run it in AWS CloudShell:
#   bash live_mcp.sh up       # launch (asks; YES=1 does not), then print the URL, your token and Muse's settings command
#   bash live_mcp.sh status   # the machine, its expiry, its boot log's last lines, and a check of the endpoint
#   bash live_mcp.sh token    # the URL, your token and Muse's settings command again
#   bash live_mcp.sh share    # the block to give judges: the URL, the judges' token, its expiry and how to try it
#   bash live_mcp.sh down     # terminate the machine and delete its security group (asks; YES=1 does not)
# What up makes, all tagged project=rhobound-live, in the region's default VPC:
#   - a security group with 80 and 443 open (Let's Encrypt and HTTPS) and nothing else: no ssh;
#   - one TYPE machine (Ubuntu 24.04, arm64) with no IAM role and IMDSv2 only, which shuts down, and so terminates,
#     after HOURS hours.
# The machine runs live_mcp_boot.sh, fetched from this repository at the commit REF names (resolved here to its sha): it
# builds rhobound and rhobound-mcp at that commit and serves the repository's sample traces, as the environment prod, on
# https://<public IP, dashed>.sslip.io/mcp. Two read-only tokens are made here and kept in STATE (0600): yours (demo)
# and one to hand out (judges). The machine stores only their sha256, and both expire with it. Never commit a token: a
# judge who wants an endpoint of their own runs this script in their own account. About 3-5 minutes after up, status
# says READY.
# Env: REGION [$AWS_REGION, else ap-south-1]  REF [main]  TYPE [t4g.small]  HOURS [8; at most 168, a week]
#      YES [0]  STATE [~/.rhobound-live]  DRYRUN [0; 1 = print every AWS change instead of making it]
#      BOOT [tests: a local live_mcp_boot.sh instead of the one at REF's commit]
set -euo pipefail
REGION=${REGION:-${AWS_REGION:-ap-south-1}}
REF=${REF:-main}
TYPE=${TYPE:-t4g.small}
HOURS=${HOURS:-8}
YES=${YES:-0}
DRYRUN=${DRYRUN:-0}
STATE=${STATE:-$HOME/.rhobound-live}
REPO=Sharvil2704/rhobound-mcp
SSM_AMI=/aws/service/canonical/ubuntu/server/24.04/stable/current/arm64/hvm/ebs-gp3/ami-id
TAG=rhobound-live
SG_DESC="rhobound live MCP demo: HTTPS and Lets Encrypt only"   # EC2 allows only a-zA-Z0-9. _-:/()#,@[]+=&;{}!$*

say() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }
die() { echo "error: $*" >&2; exit 1; }
awsr() { aws --region "$REGION" "$@"; }
awsm() {   # mutations; with DRYRUN=1 they are only printed
  if [[ $DRYRUN == 1 ]]; then echo "DRYRUN aws --region $REGION $*" >&2; echo "dryrun-id"; return 0; fi
  awsr "$@"
}
confirm() { [[ $YES == 1 || $DRYRUN == 1 ]] && return 0; read -r -p "$1 [y/N] " a; [[ $a == y || $a == Y ]]; }
if ! [[ $HOURS =~ ^[0-9]+$ ]] || ((HOURS < 1 || HOURS > 168)); then die "HOURS must be 1 to 168"; fi
for c in aws jq curl openssl; do command -v "$c" > /dev/null || die "$c is required (AWS CloudShell has them)"; done
install -d -m 0700 "$STATE"
st() { [[ -f $STATE/state.env ]] && sed -n "s/^$1=//p" "$STATE/state.env"; }

up() {
  [[ -z $(st instance) ]] || die "a live endpoint is already recorded ($(st instance)): bash live_mcp.sh down first"
  local sha ami vpc subnets sg boot ud tokens expires iid="" err ip host s n
  sha=$(curl -fsS "https://api.github.com/repos/$REPO/commits/$REF" | jq -r .sha)
  [[ $sha =~ ^[0-9a-f]{40}$ ]] || die "cannot resolve $REF on $REPO"
  ami=$(awsr ssm get-parameter --name "$SSM_AMI" --query Parameter.Value --output text)
  vpc=$(awsr ec2 describe-vpcs --filters Name=is-default,Values=true --query 'Vpcs[0].VpcId' --output text)
  [[ $vpc == vpc-* ]] || die "no default VPC in $REGION (aws ec2 create-default-vpc --region $REGION makes one)"
  subnets=$(awsr ec2 describe-subnets --filters "Name=vpc-id,Values=$vpc" Name=default-for-az,Values=true \
    --query 'Subnets[].SubnetId' --output text)
  [[ $subnets == subnet-* ]] || die "no default subnet in $vpc"
  say "region $REGION, $TYPE from $ami, rhobound at $sha ($REF), terminates itself after $HOURS h"
  confirm "Launch it (about 0.02 USD per hour)?" || die "nothing launched"
  boot=$STATE/live_mcp_boot.sh
  if [[ -n ${BOOT:-} ]]; then cp "$BOOT" "$boot"; else
    curl -fsS "https://raw.githubusercontent.com/$REPO/$sha/bed/aws/live_mcp_boot.sh" -o "$boot"
  fi
  tokens=""
  for n in demo judges; do
    ( umask 077; openssl rand -hex 32 | tr -d '\n' > "$STATE/token-$n" )
    tokens+="$n=$(openssl dgst -sha256 < "$STATE/token-$n" | sed 's/.*= *//') "
  done
  expires=$(date -u -d "+$((HOURS + 1)) hours" +%Y-%m-%dT%H:%M:%SZ)
  ud=$STATE/user-data.sh
  { echo '#!/bin/bash'
    printf 'export LIVE_SHA=%s LIVE_TOKENS="%s" LIVE_EXPIRES=%s LIVE_HOURS=%s\n' "$sha" "${tokens% }" "$expires" "$HOURS"
    tail -n +2 "$boot"; } > "$ud"
  sg=$(awsm ec2 create-security-group --group-name "$TAG-$(date -u +%Y%m%d%H%M%S)" --vpc-id "$vpc" --description "$SG_DESC" \
    --tag-specifications "ResourceType=security-group,Tags=[{Key=project,Value=$TAG}]" --query GroupId --output text)
  for p in 80 443; do
    awsm ec2 authorize-security-group-ingress --group-id "$sg" --protocol tcp --port "$p" --cidr 0.0.0.0/0 > /dev/null
  done
  for s in $subnets; do   # the first default subnet whose zone offers TYPE
    if iid=$(awsm ec2 run-instances --image-id "$ami" --instance-type "$TYPE" --subnet-id "$s" --security-group-ids "$sg" \
      --associate-public-ip-address --user-data "file://$ud" --instance-initiated-shutdown-behavior terminate \
      --metadata-options HttpTokens=required,HttpPutResponseHopLimit=1,HttpEndpoint=enabled \
      --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$TAG},{Key=project,Value=$TAG}]" \
      --query 'Instances[0].InstanceId' --output text 2> "$STATE/run.err"); then break; fi
    err=$(cat "$STATE/run.err"); iid=""
    grep -qE 'Unsupported|InsufficientInstanceCapacity' <<< "$err" || break
    say "$TYPE is not available in $s's zone: trying the next default subnet"
  done
  if [[ -z $iid ]]; then
    awsm ec2 delete-security-group --group-id "$sg" > /dev/null || true
    die "run-instances failed ($(tail -n 1 "$STATE/run.err")); $sg was deleted"
  fi
  if [[ $DRYRUN == 1 ]]; then ip=192.0.2.10; else
    awsr ec2 wait instance-running --instance-ids "$iid"
    ip=$(awsr ec2 describe-instances --instance-ids "$iid" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)
  fi
  host=${ip//./-}.sslip.io
  printf 'instance=%s\nsg=%s\nhost=%s\nsha=%s\nregion=%s\nexpires=%s\n' "$iid" "$sg" "$host" "$sha" "$REGION" \
    "$(date -u -d "+$HOURS hours" +%Y-%m-%dT%H:%MZ)" > "$STATE/state.env"
  say "launched $iid; it builds for about 3-5 minutes: run 'bash live_mcp.sh status' until it says READY"
  show
}

muse_cmd() {   # the command that makes Muse Code's rhobound entry this endpoint, under the key the file already uses
  printf "f=~/.config/muse/settings.json && jq --arg u '%s' '(if has(\"mcpServers\") then \"mcpServers\" else \"mcp_servers\" end) as \$k | .[\$k].rhobound = {transport: \"streamable_http\", url: \$u, headers: {Authorization: \"Bearer \${RHOBOUND_TOKEN}\"}, enabled: true, mode: \"optional\"}' \"\$f\" > \"\$f.new\" && mv \"\$f.new\" \"\$f\"\n" "https://$1/mcp"
}

curl_cmd() {   # one request: +4 ms on b, predicted on the hosted production fit; prints "block"
  printf '%s\n' "curl -s https://\$H/mcp -H \"Authorization: Bearer \$RHOBOUND_TOKEN\" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/call\",\"params\":{\"name\":\"predict\",\"arguments\":{\"env\":\"prod\",\"changes\":[{\"op\":\"own_ms\",\"service\":\"b\",\"value\":4}]}}}' | jq '.result.structuredContent.preview.verdict'"
}

show() {   # the URL, your token, and the commands for your laptop
  local host; host=$(st host); [[ -n $host ]] || die "nothing recorded: bash live_mcp.sh up"
  echo "URL      https://$host/mcp"
  echo "token    $(cat "$STATE/token-demo")   (yours; 'bash live_mcp.sh share' prints the judges' block)"
  echo "expires  $(st expires)"
  echo
  echo "On your laptop:"
  echo "  export RHOBOUND_TOKEN=$(cat "$STATE/token-demo") H=$host"
  echo "  $(curl_cmd)"
  echo "Point Muse Code's rhobound entry at this endpoint (Muse expands \${RHOBOUND_TOKEN} itself):"
  echo "  $(muse_cmd "$host")"
}

share() {   # the block to paste into the submission for the judges: never into the repository
  local host; host=$(st host); [[ -n $host ]] || die "nothing recorded: bash live_mcp.sh up"
  cat << EOF
rhobound, live on AWS (read-only; synthetic sample data; the machine terminates itself at $(st expires))

URL    https://$host/mcp
token  $(cat "$STATE/token-judges")

Try it from a terminal (it prints "block": +4 ms on service b would break entry a's latency limit):
  export RHOBOUND_TOKEN=$(cat "$STATE/token-judges") H=$host
  $(curl_cmd)

With Muse Code and the rhobound plugin, point its rhobound entry here (the steps: https://github.com/$REPO/blob/main/demo/README.md):
  $(muse_cmd "$host")

An endpoint of your own instead: bash live_mcp.sh up, in your own AWS CloudShell.
EOF
}

status() {
  local iid host code; iid=$(st instance); host=$(st host)
  [[ -n $iid ]] || die "nothing recorded: bash live_mcp.sh up"
  echo "machine  $iid: $(awsr ec2 describe-instances --instance-ids "$iid" --query 'Reservations[0].Instances[0].State.Name' --output text 2> /dev/null || echo unknown), terminates itself at $(st expires)"
  echo "boot     $(awsr ec2 get-console-output --instance-id "$iid" --latest --output text 2> /dev/null | grep -a 'rhobound-live:' | tail -n 3 | sed 's/.*rhobound-live: //' | paste -sd'|' - | sed 's/|/ | /g')"
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://$host/healthz" || true)
  echo "healthz  https://$host/healthz: HTTP $code"
  code=$(curl -s -o "$STATE/tools.json" -w '%{http_code}' --max-time 20 -X POST "https://$host/mcp" \
    -H "Authorization: Bearer $(cat "$STATE/token-demo")" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' || true)
  if [[ $code == 200 ]]; then echo "tools    HTTP 200: $(jq -r '[.result.tools[].name] | join(" ")' "$STATE/tools.json" 2> /dev/null)"
  else echo "tools    HTTP $code: $(head -c 160 "$STATE/tools.json" 2> /dev/null | tr '\n' ' ')"; fi
}

down() {
  local iid sg; iid=$(st instance); sg=$(st sg)
  [[ -n $iid ]] || die "nothing recorded"
  confirm "Terminate $iid and delete $sg?" || die "nothing changed"
  awsm ec2 terminate-instances --instance-ids "$iid" > /dev/null
  [[ $DRYRUN == 1 ]] || awsr ec2 wait instance-terminated --instance-ids "$iid"
  awsm ec2 delete-security-group --group-id "$sg" > /dev/null
  rm -f "$STATE/state.env" "$STATE"/token-* "$STATE/tools.json" "$STATE/run.err"
  say "terminated $iid, deleted $sg"
}

case ${1:-} in
  up) up ;;
  status) status ;;
  token) show ;;
  share) share ;;
  down) down ;;
  *) sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
