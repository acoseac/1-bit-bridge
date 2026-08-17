#!/usr/bin/env bash
# deploy-bridge-vps.sh — update the public Linux VPS bridge (bridge.ars.md)
# to current main. Run from the operator's macOS/Linux workstation, from the
# repo root, ON main (the script checks).
#
# Mirrors docs/deployment-runbook.md "bridge.ars.md" exactly:
#   cross-compile linux/amd64 -> scp as .new -> SHA-256 verify BEFORE swap
#   -> two-step `sudo mv` swap (keeps a timestamped .old- backup for rollback)
#   -> setcap cap_net_bind_service=+ep -> systemctl restart -> verify health.
#
# Idempotent + safe: a truncated upload can't replace the working binary
# (SHA gate), and the prior binary is retained as /usr/local/bin/bridge.old-<ts>.
#
# Usage:  ./deploy/linux/deploy-bridge-vps.sh
# Env overrides: SSH_KEY, HOST, REMOTE_BIN, HEALTH_URL, KEEP_BACKUPS (below).
#
# If SSH to the host is filtered from this workstation while its :443 answers,
# that is the allowlist, not the key -- see the runbook's SSH-flap note for the
# `ssh -J` relay route (set HOST to a ProxyJump form to use it here).
set -euo pipefail

SSH_KEY="${SSH_KEY:-$HOME/.ssh/1bitbridge_key}"
HOST="${HOST:-arsenie@bridge.ars.md}"
REMOTE_BIN="${REMOTE_BIN:-/usr/local/bin/bridge}"
HEALTH_URL="${HEALTH_URL:-https://bridge.ars.md/v1/health}"
# Backups to retain after a VERIFIED-healthy deploy. Two keeps the immediate
# rollback plus one behind it; the runbook's ~24h retention guidance is about
# how long to wait before trusting a deploy, not about hoarding every build.
KEEP_BACKUPS="${KEEP_BACKUPS:-2}"
SVC="1-bit-bridge"
LOCAL_BIN="dist/bridge-linux-amd64"

ssh_vps() { ssh -i "$SSH_KEY" -o ConnectTimeout=15 "$HOST" "$@"; }

echo "==> 1/5 Verify on main + up to date"
branch=$(git rev-parse --abbrev-ref HEAD)
[ "$branch" = "main" ] || { echo "ERROR: on '$branch', not main. Aborting."; exit 1; }
git pull --ff-only
VER=$(git describe --tags --always)
echo "    version: $VER"

echo "==> 2/5 Cross-compile linux/amd64"
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$VER" \
  -o "$LOCAL_BIN" ./cmd/bridge
echo "    built $LOCAL_BIN ($(wc -c < "$LOCAL_BIN") bytes)"

echo "==> 3/5 Upload as .new + SHA-256 gate (no swap on mismatch)"
scp -i "$SSH_KEY" -o ConnectTimeout=15 "$LOCAL_BIN" "$HOST:/tmp/bridge.new"
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
  echo "ERROR: $HEALTH_URL never reported $VER."
  echo "  The swap runs detached, so it may still be mid-flight -- check /tmp/bridge-swap.log on the host."
  echo "  If $REMOTE_BIN is missing, restore the newest backup BEFORE any restart:"
  echo "    sudo mv $REMOTE_BIN.old-<ts> $REMOTE_BIN && sudo setcap cap_net_bind_service=+ep $REMOTE_BIN && sudo systemctl restart $SVC"
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
  echo "    WARNING: prune did not run (SSH unreachable?). Deploy itself is verified; prune later."
fi
echo "Done. Rollback (within ~24h): sudo mv $REMOTE_BIN $REMOTE_BIN.broken && sudo mv $REMOTE_BIN.old-<ts> $REMOTE_BIN && sudo setcap cap_net_bind_service=+ep $REMOTE_BIN && sudo systemctl restart $SVC"
