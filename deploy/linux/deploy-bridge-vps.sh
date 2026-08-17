#!/usr/bin/env bash
# deploy-bridge-vps.sh — update the public Linux VPS bridge (bridge.ars.md)
# to current main. Run from the operator's macOS/Linux workstation, from the
# repo root, ON main (the script checks).
#
# Mirrors ops/deployment-runbook.md "bridge.ars.md" exactly (the runbook moved
# out of the Pages-served docs/ tree; see CLAUDE.md):
#   cross-compile linux/amd64 -> scp as .new -> SHA-256 verify BEFORE swap
#   -> two-step `sudo mv` swap (keeps a timestamped .old- backup for rollback)
#   -> setcap cap_net_bind_service=+ep -> systemctl restart -> verify health.
#
# Idempotent + safe: a truncated upload can't replace the working binary
# (SHA gate), and the prior binary is retained as /usr/local/bin/bridge.old-<ts>.
#
# Usage:  ./deploy/linux/deploy-bridge-vps.sh
#   First run: cp deploy/linux/.env.example deploy/linux/.env  (then fill it in)
# Env vars: HOST, SSH_KEY (required); SSH_OPTS, HEALTH_URL, REMOTE_BIN,
#           KEEP_BACKUPS, ENV_FILE (optional; see .env.example).
#
# If SSH to the host is filtered from this workstation while its :443 answers,
# that is the allowlist, not the key -- see the runbook's SSH-flap note. Route
# around it through a host whose egress IS allowlisted by setting
# SSH_OPTS="-J relay-user@relay-host": `-J` tunnels only TCP, so the key never
# leaves this workstation and no binary transits the relay.
set -euo pipefail

# HOST COORDINATES ARE NOT HARDCODED HERE. deploy/ is public (CLAUDE.md: "keep
# host coordinates in env vars with placeholder defaults, never hardcoded"), and
# the runbook redacts the very two values this file used to carry -- so the
# script contradicted its own runbook's posture. Its Windows sibling already
# reads BRIDGE_WAN_URL from the environment for the same reason.
#
# Precedence: explicit env > $ENV_FILE > required-and-unset (hard error).
# .env is already covered by .gitignore's `.env` pattern and .env.example is
# un-ignored by `!.env.example`, so the template ships and real values cannot.
#
# NOTE this does not un-publish anything: the previous hardcoded values remain
# in git history, so this is hygiene going forward, not remediation.
ENV_FILE="${ENV_FILE:-$(cd "$(dirname "$0")" && pwd)/.env}"
_cli_host="${HOST:-}"; _cli_key="${SSH_KEY:-}"
_cli_health="${HEALTH_URL:-}"; _cli_opts="${SSH_OPTS:-}"
# shellcheck source=/dev/null
[ -f "$ENV_FILE" ] && . "$ENV_FILE"
HOST="${_cli_host:-${HOST:-}}"
SSH_KEY="${_cli_key:-${SSH_KEY:-}}"
HEALTH_URL="${_cli_health:-${HEALTH_URL:-}}"
SSH_OPTS="${_cli_opts:-${SSH_OPTS:-}}"

if [ -z "$HOST" ] || [ -z "$SSH_KEY" ]; then
  {
    echo "ERROR: HOST and SSH_KEY must be set (this script ships no host defaults)."
    echo "  cp $(dirname "$0")/.env.example $ENV_FILE   # then fill in the coordinates"
    echo "  or one-off: HOST=user@host SSH_KEY=~/.ssh/key $0"
  } >&2
  exit 1
fi

REMOTE_BIN="${REMOTE_BIN:-/usr/local/bin/bridge}"
# Defaults to the host's public :443, stripping any user@ -- the same shape
# deploy/README.md uses for the Windows host's health URL.
HEALTH_URL="${HEALTH_URL:-https://${HOST#*@}/v1/health}"

# SSH_OPTS is word-split into an array so a relay route works. The +-guarded
# expansion at the call sites is REQUIRED, not defensive: macOS ships bash 3.2,
# where `set -u` plus a bare "${arr[@]}" on an EMPTY array aborts with "unbound
# variable" (verified on 3.2.57) -- i.e. it would break every run that needs no
# relay, which is all of them normally. Verified to pass 5 args when empty and 7
# with `-J relay@host`, with no stray empty argument (an empty arg would be read
# by ssh as a hostname). Simple word-splitting, so an option whose VALUE
# contains spaces (a full ProxyCommand) needs ~/.ssh/config instead.
read -ra SSH_OPT_ARR <<< "$SSH_OPTS"
# Backups to retain after a VERIFIED-healthy deploy. Two keeps the immediate
# rollback plus one behind it; the runbook's ~24h retention guidance is about
# how long to wait before trusting a deploy, not about hoarding every build.
KEEP_BACKUPS="${KEEP_BACKUPS:-2}"
SVC="1-bit-bridge"
LOCAL_BIN="dist/bridge-linux-amd64"

ssh_vps() { ssh -i "$SSH_KEY" -o ConnectTimeout=15 "${SSH_OPT_ARR[@]+"${SSH_OPT_ARR[@]}"}" "$HOST" "$@"; }

echo "==> 1/6 Verify on main + up to date"
branch=$(git rev-parse --abbrev-ref HEAD)
[ "$branch" = "main" ] || { echo "ERROR: on '$branch', not main. Aborting."; exit 1; }
git pull --ff-only
VER=$(git describe --tags --always)
echo "    version: $VER"

echo "==> 2/6 Cross-compile linux/amd64"
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$VER" \
  -o "$LOCAL_BIN" ./cmd/bridge
echo "    built $LOCAL_BIN ($(wc -c < "$LOCAL_BIN") bytes)"

echo "==> 3/6 Upload as .new + SHA-256 gate (no swap on mismatch)"
scp -i "$SSH_KEY" -o ConnectTimeout=15 "${SSH_OPT_ARR[@]+"${SSH_OPT_ARR[@]}"}" "$LOCAL_BIN" "$HOST:/tmp/bridge.new"
LOCAL_SHA=$(shasum -a 256 "$LOCAL_BIN" | awk '{print $1}')
REMOTE_SHA=$(ssh_vps 'sha256sum /tmp/bridge.new | cut -d" " -f1')
echo "    local : $LOCAL_SHA"
echo "    remote: $REMOTE_SHA"
if [ "$LOCAL_SHA" != "$REMOTE_SHA" ]; then
  echo "ERROR: SHA-256 mismatch — upload corrupt. Leaving the running binary untouched."
  ssh_vps 'rm -f /tmp/bridge.new'
  exit 1
fi

echo "==> 4/6 Two-step swap + setcap + restart (keeps .old- backup)"
# DISPATCHED DETACHED, deliberately. Run inline over the SSH channel, the swap
# dies wherever the connection dies -- and the window between the two `mv`s is
# the one state with NO binary at $REMOTE_BIN. The running process survives on
# its held inode, so nothing looks wrong until the next restart, which then
# fails with Restart=always and takes the bridge down. That is not theoretical:
# the operator's workstation egress flaps (see the runbook's SSH-flap note) and
# a 2026-08-17 deploy dropped exactly here -- it happened to die BEFORE the
# first `mv`, which was luck, not design.
#
# setsid detaches from the SSH session's process group so a dropped channel
# cannot signal it. Progress goes to a log on the host, and verification below
# polls the PUBLIC health endpoint -- which needs no SSH at all, so a link that
# drops after dispatch still yields a trustworthy verdict.
ssh_vps "cat > /tmp/bridge-swap.sh" <<EOS
set -e
TS=\$(date +%Y%m%d-%H%M%S)
echo "swap start ts=\$TS"
sudo mv $REMOTE_BIN $REMOTE_BIN.old-\$TS
sudo mv /tmp/bridge.new $REMOTE_BIN
sudo chmod +x $REMOTE_BIN
sudo setcap cap_net_bind_service=+ep $REMOTE_BIN
sudo systemctl restart $SVC
sleep 3
echo "active: \$(systemctl is-active $SVC)"
echo "SWAP_DONE backup=$REMOTE_BIN.old-\$TS"
EOS
ssh_vps "setsid nohup bash /tmp/bridge-swap.sh > /tmp/bridge-swap.log 2>&1 < /dev/null & sleep 1; echo '    dispatched detached (log: /tmp/bridge-swap.log)'"

echo "==> 5/6 Verify public health (polls :443 -- survives an SSH drop)"
deployed=""
for _ in $(seq 1 20); do
  sleep 6
  live=$(curl -s --max-time 15 "$HEALTH_URL" \
    | python3 -c "import sys,json;print(json.load(sys.stdin).get('serverVersion',''))" 2>/dev/null || true)
  echo "    serving: ${live:-<unreachable>}"
  if [ "$live" = "$VER" ]; then deployed=yes; break; fi
done
if [ -z "$deployed" ]; then
  # Failure guidance goes to stderr so a caller piping stdout still sees it,
  # and so success output stays separable -- same reason `bridge doctor
  # --json --fix` keeps its progress lines off stdout.
  {
    echo "ERROR: $HEALTH_URL never reported $VER."
    echo "  The swap runs detached, so it may still be mid-flight -- check /tmp/bridge-swap.log on the host."
    echo "  If $REMOTE_BIN is missing, restore the newest backup BEFORE any restart:"
    echo "    sudo mv $REMOTE_BIN.old-<ts> $REMOTE_BIN && sudo setcap cap_net_bind_service=+ep $REMOTE_BIN && sudo systemctl restart $SVC"
  } >&2
  exit 1
fi
curl -s --max-time 15 "$HEALTH_URL" | python3 -c "
import sys, json
d = json.load(sys.stdin)
f = d.get('features', [])
print('    serverVersion :', d.get('serverVersion'))
print('    playlistBackup:', 'playlistBackup' in f, '| playbackHistory:', 'playbackHistory' in f)
print('    certNotAfter  :', d.get('certNotAfter'))
print('    leCertNotAfter:', d.get('leCertNotAfter'))
"

echo "==> 6/6 Prune old backups (keeping $KEEP_BACKUPS)"
# Runs ONLY after health confirmed the new binary serves traffic -- pruning
# before that could delete the rollback path while the rollback is still needed.
#
# Sorted by the NAME's timestamp, never mtime: the name carries the SWAP time
# while mtime carries the local BUILD time, and the two disagree (a binary
# built at 10:23 and deployed at 11:49 sorts differently under each), so an
# `ls -t` prune can delete the newest backup.
#
# A failure here is a WARNING, not a deploy failure: the deploy itself is done
# and verified, and this host's SSH flaps. Left unpruned, these accumulate at
# ~44 MB per deploy -- 100 of them reached 4.1 GB of a 29 GB root disk on
# 2026-08-17, crowding the rclone VFS cache (see the runbook).
if ssh_vps "
  set -e
  cd \$(dirname $REMOTE_BIN)
  base=\$(basename $REMOTE_BIN)
  ls -1 \$base.old-* 2>/dev/null | sort -r | tail -n +\$(($KEEP_BACKUPS + 1)) > /tmp/bridge-prune.txt
  n=\$(wc -l < /tmp/bridge-prune.txt | tr -d ' ')
  xargs -a /tmp/bridge-prune.txt -r sudo rm -f
  echo \"    pruned \$n; kept: \$(ls -1 \$base.old-* | sort -r | tr '\n' ' ')\"
  df -h / | awk 'NR==2{print \"    root disk: \"\$3\" / \"\$2\" (\"\$5\" full), \"\$4\" free\"}'
"; then :; else
  echo "    WARNING: prune did not run (SSH unreachable?). Deploy itself is verified; prune later." >&2
fi
echo "Done. Rollback (within ~24h): sudo mv $REMOTE_BIN $REMOTE_BIN.broken && sudo mv $REMOTE_BIN.old-<ts> $REMOTE_BIN && sudo setcap cap_net_bind_service=+ep $REMOTE_BIN && sudo systemctl restart $SVC"
