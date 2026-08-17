# deploy/ — operator deploy scripts (source of truth)

These scripts deploy the bridge to the two production hosts. **This directory
is the canonical source of truth** — the copies on the hosts (`home-pc`
Desktop, `/tmp` on the workstation) must be synced FROM here, never edited in
place. The cert-re-mint bug fixed 2026-06-01 existed precisely because the
only copy lived on the host and silently drifted.

Full host coordinates, layout, firewall posture, and gotchas live in
[`../ops/deployment-runbook.md`](../ops/deployment-runbook.md). This README
is just the script index + sync contract.

## windows/ — home-pc (Windows, scheduled-task runtime)

Run from the operator's workstation. Sync first, then invoke over SSH via
`pwsh ... -Command -` (stdin) or `-File` after scp'ing. Set `HOMEPC` to the
target's `user@host` once — this repo is public, so no real host coordinates are
committed here (the operator's live values live in `ops/deployment-runbook.md`,
which is not published):

```sh
HOMEPC=user@192.0.2.10        # your Windows host
scp deploy/windows/*.ps1 "$HOMEPC:C:/Users/<user>/Desktop/"
```

| Script | When | Cert? |
|---|---|---|
| `update-bridge-windows.ps1` | **every routine code update** — pull main, rebuild, restart task | **preserved** |
| `setup-bridge-windows.ps1` | fresh install only (mints cert) / re-inject YAML keys | minted on fresh; **preserved on existing** |
| `restart-bridge-windows.ps1` | kick the process after a YAML edit / wedge | untouched |
| `rotate-cert-windows.ps1` | **deliberately** re-mint the cert (after a customEndpoints edit) | **re-minted → every iOS device must re-pair** |
| `firewall-bridge-windows.ps1` | install the 3 inbound rules (7788 TCP/UDP, 7789 TCP) | n/a |

Routine update (cert-safe), from the workstation:

```sh
ssh "$HOMEPC" 'pwsh -NoProfile -Command -' < deploy/windows/update-bridge-windows.ps1
curl -sk "https://${HOMEPC#*@}:7788/v1/health" | jq .serverVersion
```

**Cert policy:** only `rotate-cert-windows.ps1` re-mints. `setup` skips `init`
when a config exists, and `update` never runs `init`. Never run `bridge init
-force` against a live install — it changes the fingerprint and breaks every
pairing.

## linux/ — bridge.ars.md (Linux VPS, public mode, systemd)

```sh
cp deploy/linux/.env.example deploy/linux/.env   # first run only; fill it in
./deploy/linux/deploy-bridge-vps.sh
```

Cross-compiles linux/amd64, uploads as `.new`, **SHA-256-gates before swap**,
two-step `sudo mv` (keeps a `bridge.old-<ts>` backup for one-step rollback),
`setcap cap_net_bind_service=+ep`, `systemctl restart`, then verifies by polling
public `/v1/health` until it reports the built version, and finally prunes old
backups down to `KEEP_BACKUPS` (default 2).

**The swap is dispatched detached** (`setsid nohup`) because the window between
the two `mv`s is the one state with no binary at the destination — run inline,
a dropped SSH channel can leave the host unable to restart. Verification polls
`:443`, which needs no SSH at all.

**Coordinates live in `deploy/linux/.env`, never in the script** — this file is
public. `HOST` and `SSH_KEY` are required with no defaults; `.env` is gitignored
and `.env.example` documents every knob. Explicit env still wins, so a one-off
`HOST=… SSH_KEY=… ./deploy/linux/deploy-bridge-vps.sh` works.

If the host's `:22` is filtered from your workstation while its `:443` answers,
that is the ufw allowlist rather than your key (a timeout fails at TCP connect,
before any key is offered; a wrong key says `Permission denied`). Route through
a host whose egress *is* allowlisted with `SSH_OPTS="-J relay-user@relay-host"` —
`-J` tunnels only TCP, so the key never leaves your workstation and no binary
transits the relay.

## When to deploy

Per [`../ops/deployment-runbook.md`](../ops/deployment-runbook.md): after any
merged **runtime-behavior** PR, update the local fixture, then home-pc, then
bridge.ars.md. Skip for docs-only / test-only merges (no shipped binary
changes behavior).
