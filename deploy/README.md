# deploy/ — operator deploy scripts (source of truth)

The Windows script deploys the bridge to home-pc. The Linux script was written
for the VPS, which since 2026-09-22 runs only the public demo, deployed from
the release artifact by the runbook's procedure; the operator bridge, on a
home NUC since then, takes the runbook's manual form. Neither is a job for the
Linux script any more (the `linux/` section says why). **This directory
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

## linux/ — the Linux VPS, bridge.ars.md (public mode, systemd)

Since 2026-09-22 this host runs only the public demo (`bridge.1-bit.app`, a
unit and binary of its own); the operator bridge moved to a home NUC. Do not
point this script at the NUC: its health poll runs `curl` without `-k`, and
the NUC serves a self-signed certificate, so the poll never reads
`serverVersion` and the script exits 1 with its rollback advice after a swap
that may well have succeeded (it rolls nothing back). A deploy there follows
the runbook's manual form and checks health with `curl -k`.

The demo must be deployed from the release artifact. Do not run this script
for it: the script always builds current `main`, and a `git describe` version
makes the demo advertise an update forever. Follow the runbook's "Demo bridge"
procedure (download and verify the release artifact, upload `/tmp/rel/bridge`,
then the detached swap) with the demo's overrides, `SVC=1-bit-bridge-demo`,
`REMOTE_BIN=/usr/local/bin/bridge-demo` and
`HEALTH_URL=https://bridge.1-bit.app/v1/health`: the script's defaults
(`SVC=1-bit-bridge`, `REMOTE_BIN=/usr/local/bin/bridge`) name the operator
bridge's old unit and binary on this host.

So no production bridge takes this script as it stands; it is kept as the
reference for the manual form's steps. Its usage, for a Linux host that takes
a `main` build and serves a certificate `curl` verifies:

```sh
cp deploy/linux/.env.example deploy/linux/.env   # first run only; fill it in
./deploy/linux/deploy-bridge-vps.sh              # ENV_FILE=… picks another env file
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
the operator bridge (on the NUC since 2026-09-22; bridge.ars.md before). Skip
for docs-only / test-only merges (no shipped binary changes behavior).
