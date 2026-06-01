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
# Env overrides: SSH_KEY, HOST, REMOTE_BIN, HEALTH_URL (defaults below).
set -euo pipefail

SSH_KEY="${SSH_KEY:-$HOME/.ssh/1bitbridge_key}"
HOST="${HOST:-arsenie@bridge.ars.md}"
REMOTE_BIN="${REMOTE_BIN:-/usr/local/bin/bridge}"
HEALTH_URL="${HEALTH_URL:-https://bridge.ars.md/v1/health}"
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

echo "==> 4/5 Two-step swap + setcap + restart (keeps .old- backup)"
ssh_vps "
  set -e
  TS=\$(date +%Y%m%d-%H%M%S)
  sudo mv $REMOTE_BIN $REMOTE_BIN.old-\$TS
  sudo mv /tmp/bridge.new $REMOTE_BIN
  sudo chmod +x $REMOTE_BIN
  sudo setcap cap_net_bind_service=+ep $REMOTE_BIN
  sudo systemctl restart $SVC
  sleep 3
  echo \"    backup: $REMOTE_BIN.old-\$TS\"
  echo \"    active: \$(systemctl is-active $SVC)\"
"

echo "==> 5/5 Verify public health"
sleep 2
curl -s --max-time 15 "$HEALTH_URL" | python3 -c "
import sys, json
d = json.load(sys.stdin)
f = d.get('features', [])
print('    serverVersion :', d.get('serverVersion'))
print('    playlistBackup:', 'playlistBackup' in f, '| playbackHistory:', 'playbackHistory' in f)
print('    certNotAfter  :', d.get('certNotAfter'))
print('    leCertNotAfter:', d.get('leCertNotAfter'))
"
echo "Done. Rollback (within ~24h): sudo mv $REMOTE_BIN $REMOTE_BIN.broken && sudo mv $REMOTE_BIN.old-<ts> $REMOTE_BIN && sudo setcap cap_net_bind_service=+ep $REMOTE_BIN && sudo systemctl restart $SVC"
