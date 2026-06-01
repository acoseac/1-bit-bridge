# deploy/ — operator deploy scripts (source of truth)

These scripts deploy the bridge to the two production hosts. **This directory
is the canonical source of truth** — the copies on the hosts (`home-pc`
Desktop, `/tmp` on the workstation) must be synced FROM here, never edited in
place. The cert-re-mint bug fixed 2026-06-01 existed precisely because the
only copy lived on the host and silently drifted.

Full host coordinates, layout, firewall posture, and gotchas live in
[`../docs/deployment-runbook.md`](../docs/deployment-runbook.md). This README
is just the script index + sync contract.

## windows/ — home-pc (Windows, scheduled-task runtime)

Run from the operator's workstation. Sync first, then invoke over SSH via
`pwsh ... -Command -` (stdin) or `-File` after scp'ing:

```sh
scp deploy/windows/*.ps1 arsenie@192.168.0.208:C:/Users/arsenie/Desktop/
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
ssh arsenie@192.168.0.208 'pwsh -NoProfile -Command -' < deploy/windows/update-bridge-windows.ps1
curl -sk https://192.168.0.208:7788/v1/health | jq .serverVersion
```

**Cert policy:** only `rotate-cert-windows.ps1` re-mints. `setup` skips `init`
when a config exists, and `update` never runs `init`. Never run `bridge init
-force` against a live install — it changes the fingerprint and breaks every
pairing.

## linux/ — bridge.ars.md (Linux VPS, public mode, systemd)

```sh
./deploy/linux/deploy-bridge-vps.sh
```

Cross-compiles linux/amd64, uploads as `.new`, **SHA-256-gates before swap**,
two-step `sudo mv` (keeps a `bridge.old-<ts>` backup ~24h for one-step
rollback), `setcap cap_net_bind_service=+ep`, `systemctl restart`, verifies
public `/v1/health`. Override `SSH_KEY` / `HOST` / `REMOTE_BIN` / `HEALTH_URL`
via env if coordinates change.

## When to deploy

Per [`../docs/deployment-runbook.md`](../docs/deployment-runbook.md): after any
merged **runtime-behavior** PR, update the local fixture, then home-pc, then
bridge.ars.md. Skip for docs-only / test-only merges (no shipped binary
changes behavior).
