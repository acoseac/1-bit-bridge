# Bridge deployment runbook (operator)

Extracted from CLAUDE.md to keep it out of always-loaded context. **Read this
before any production deploy** — covers the two live bridges (home-pc Windows,
bridge.ars.md Linux VPS) and the post-merge 3-step deploy flow.

> **Placeholders.** This repo is public, so live host coordinates are not committed.
> Substitute your own for `<WAN-IP>`, `<HOMEPC-SSH>`, `<HOMEPC-LAN-IP>`, `<VPS-SSH>`, and
> `<VPS-SSH-KEY>`. Keep the real values in `ops/coordinates.local.md` (gitignored) or your
> shell environment — see [`ops/README.md`](README.md). These values were committed in
> plaintext until 2026-07-20 and are still in git history, so treat the WAN endpoint as
> disclosed: **rotate the router port-forward** rather than relying on this scrub.

**Deploy scripts are git-tracked in [`deploy/`](../deploy/) — that is the source
of truth.** The copies on the hosts (home-pc Desktop) and in `/tmp` on the
workstation are synced FROM there, never edited in place (the 2026-06-01
cert-re-mint bug existed because the only copy lived on the host and drifted).
See [`deploy/README.md`](../deploy/README.md) for the script index + sync
contract. Routine update one-liners:
- **home-pc**: `ssh <HOMEPC-SSH> 'pwsh -NoProfile -Command -' < deploy/windows/update-bridge-windows.ps1` (cert-preserving; never re-mints)
- **bridge.ars.md**: `./deploy/linux/deploy-bridge-vps.sh` (needs `deploy/linux/.env` — copy `.env.example` once and fill in the coordinates; the script ships no host defaults because `deploy/` is public)

## Production deployments

### home-pc (Windows, SSH `<HOMEPC-SSH>`)

Operator's home Windows machine. Reachable from the operator's macOS workstation via SSH (OpenSSH server, **session is auto-elevated to admin** — no UAC popup needed for `New-NetFirewallRule` etc.). PowerShell 7 (`pwsh`), Git, and Go are pre-installed. Tailscale runs in CLI mode (`tailscale.exe` on PATH).

**Layout (committed by setup script):**

| Path | Purpose |
|---|---|
| `C:\1-bit-bridge\src\` | git clone of `acoseac/1-bit-bridge`, refreshed on each update |
| `C:\1-bit-bridge\bin\bridge.exe` | built binary (`go build` from `src\`, no CGO) |
| `C:\1-bit-bridge\data\bridge.yaml` | live config |
| `C:\1-bit-bridge\data\data\` | manifest DB + artwork cache + scan state |
| `C:\1-bit-bridge\data\serve.log` / `serve.err.log` | stdout / stderr from the scheduled-task launch |
| `F:\media\music` | library root |
| `E:\temp` | upscale + optimize variants storage (separate disk from library by design — variant generation is heavy I/O and shouldn't compete with the library drive) |

**Runtime ownership:** Scheduled Task `1-bit-bridge (home-pc)` running as `arsenie\Interactive`, triggered `AtLogOn`. Survives SSH disconnect / logout / reboot. **Old bridge service install (different exe path) is registered but stopped** — leave alone or remove manually if cleanup is wanted; the new task is the canonical runtime.

**⚠️ The admin-console Restart button likely strands the bridge on this host** (same incident class as the VPS `Restart=on-failure` outage of 2026-08-05 — see the bridge.ars.md section). `POST /api/restart` exits the process cleanly, and a Windows Scheduled Task does not relaunch a process that exited on its own — Task Scheduler restart settings (`RestartCount`/`RestartInterval`) only fire on task *failure*, and no repo script configures even those. Recovery after a console restart: `restart-bridge-windows.ps1` over SSH. Unverified on the live host as of 2026-08-05 (host unreachable from the workstation at the time) — when next on the LAN, check `(Get-ScheduledTask -TaskName '1-bit-bridge (home-pc)').Settings` and either confirm the strand or record the on-host task's restart config here.

**Windows Defender Firewall rules** added by `firewall-bridge-windows.ps1`:
- `1-bit-bridge (C:\1-bit-bridge)` — inbound TCP 7788 (HTTPS)
- `1-bit-bridge (C:\1-bit-bridge) admin` — inbound TCP 7789 (admin console, loopback-only by bind but the rule is belt-and-braces)
- `1-bit-bridge (C:\1-bit-bridge) UDP` — inbound UDP 7788 (HTTP/3 / QUIC, per the PR #271 LAN HTTP/3 path)

All scoped to the exe path, NOT port-wide. Old rules at the legacy exe paths (`C:\users\arsenie\desktop\1-bit-bridge_0.1.2_windows_amd64\bridge.exe`, `C:\users\arsenie\src\1-bit-bridge\bridge.exe`, etc.) are still in the firewall — harmless but stale; nuke at the next cleanup pass.

**Endpoints advertised in `/v1/health`** (six total, in registration order):
- `https://<HOMEPC-LAN-IP>:7788` — LAN (Ethernet)
- `https://home-pc.local:7788` — mDNS
- `https://<HOMEPC-MAGICDNS>:7788` — **Tailscale magic-DNS** (Let's Encrypt cert via the SNI cert switcher + `*.ts.net` pinning bypass in `PinningDelegate.shouldSkipPinning`; this is the working remote-access path from iOS over cellular)
- `https://<HOMEPC-TAILSCALE-V4>:7788` — Tailscale IPv4 (CGNAT `100.64/10`; iOS skips per `isTailscaleCGNATURL` filter — expected, magic-DNS replaces it)
- `https://[<HOMEPC-TAILSCALE-V6>]:7788` — Tailscale IPv6 (ULA `fd7a:115c:a1e0::/48`)
- `https://<WAN-IP>:7788` — **WAN custom endpoint** (router port-forward 7788 → <HOMEPC-LAN-IP>:7788). Configured via `customEndpoints:` top-level YAML field. Public IP is RIPE-allocated (NOT CGNAT), so iOS will probe it normally.

**Cert SAN gotcha (re-discovered 2026-05-19):** `bridge init` mints the TLS cert against the **config at that moment**. If you edit `customEndpoints:` in `bridge.yaml` AFTER `init`, the cert's SAN list is stale and the first `serve` logs `WARN cert SANs are stale — missing_ips=[...]`. Run `bridge cert rotate --config ...\bridge.yaml --yes` after any customEndpoints edit. The fresh-install script flow gets this right because it rotates after the YAML edit; manual edits in the field need the same follow-up.

**Helper scripts on `C:\Users\arsenie\Desktop\`** (idempotent — re-runnable for updates):

Canonical source: [`deploy/windows/`](../deploy/windows/) — `scp deploy/windows/*.ps1 <HOMEPC-SSH>:C:/Users/arsenie/Desktop/` to sync before running.

| Script | Purpose |
|---|---|
| `update-bridge-windows.ps1` | **routine code update (use this for every merge)**: fast-forward `src` → `go build` → restart the scheduled task. **No `bridge init`, so cert + config + pairings are preserved.** |
| `setup-bridge-windows.ps1` | clone/pull → `go build` → **`bridge init` ONLY on a fresh install (no existing config)** → inject `customEndpoints` + `upscale.variantsDir` into the YAML → print cert info. **On an existing install it preserves the cert + config (no re-init)** — so a routine binary update does NOT re-mint the TLS cert and does NOT invalidate paired iOS devices. (Fixed 2026-06-01 — the prior version re-ran `init -force` on every run, silently re-minting the cert and breaking every pairing on what looked like a plain update.) |
| `rotate-cert-windows.ps1` | `bridge cert rotate` then restart (use after any `customEndpoints` edit to refresh SANs; **invalidates every paired iOS device's pinned fingerprint** — every device must re-pair) |
| `firewall-bridge-windows.ps1` | install the 3 inbound rules above (idempotent — removes pre-existing rules with the same DisplayName first) |
| `task-bridge-windows.ps1` | register the scheduled task + start it now; survives SSH disconnect / logout / reboot |
| `restart-bridge-windows.ps1` | restart the running bridge via the scheduled task (`Stop-ScheduledTask` + kill orphan + `Start-ScheduledTask`). Cheapest path for "I edited the YAML, kick the process". |
| `start-bridge-windows.ps1` | legacy foreground-start script — **don't use over SSH**, dies on session close. Kept for in-person interactive debugging only. Prefer the scheduled task. |

**Canonical update procedure** (from operator's macOS workstation):

```sh
# 1. Push the script files if you've changed them locally (they live in /tmp/ on macOS).
scp /tmp/setup-bridge-windows.ps1 \
    /tmp/rotate-cert-windows.ps1 \
    /tmp/firewall-bridge-windows.ps1 \
    /tmp/task-bridge-windows.ps1 \
    <HOMEPC-SSH>:C:/Users/arsenie/Desktop/

# 2. Re-run setup (pulls latest origin/main, rebuilds, refreshes YAML — but
#    does NOT touch the cert unless you also pass `-force` to bridge init).
ssh <HOMEPC-SSH> 'pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass \
    -File C:\Users\arsenie\Desktop\setup-bridge-windows.ps1'

# 3. Restart via the scheduled task (already registered — this just kicks it).
ssh <HOMEPC-SSH> 'pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass \
    -File C:\Users\arsenie\Desktop\task-bridge-windows.ps1'

# 4. Verify from LAN.
curl -sS -k --max-time 10 https://<HOMEPC-LAN-IP>:7788/v1/health | jq .serverVersion
```

**Just restart (no rebuild)** — after a YAML edit or to recover from a wedged process:

```sh
ssh <HOMEPC-SSH> 'pwsh -NoLogo -NoProfile -ExecutionPolicy Bypass \
    -File C:\Users\arsenie\Desktop\restart-bridge-windows.ps1'
```

Or inline without the script:

```sh
ssh <HOMEPC-SSH> \
    'pwsh -Command "Stop-ScheduledTask -TaskName ''1-bit-bridge (home-pc)''; Start-Sleep 1; Start-ScheduledTask -TaskName ''1-bit-bridge (home-pc)''"'
```

**Don't reintroduce `Start-Process -RedirectStandardOutput` for the bridge serve** on Windows over SSH — `RedirectStandardOutput` flips `UseShellExecute=false` which makes the bridge a true subprocess of the SSH-launched pwsh, and the bridge dies when SSH disconnects. Use the scheduled task (`task-bridge-windows.ps1`); it spawns detached at the OS level. The bridge silently dying after a fresh install over SSH was the symptom — listener log lines wrote fine, then nothing, then `Get-Process bridge` returned empty (Burned 2026-05-19 during the post-PR-#276 deployment).

**Don't reintroduce `Start-Process -Verb RunAs` for the firewall script** assuming a UAC popup will appear over SSH — UAC popups pop on the **physical Windows desktop**, not in the SSH session. On `home-pc` the SSH session is already admin-elevated by default so the elevation branch is a no-op, but the script still has it as a fallback for any future host where SSH is non-elevated.

**Multiple desktop network categories on Windows**: `Get-NetConnectionProfile` may report multiple connections (Ethernet "ORBI16 2" → Public, Tailscale → Private). Firewall rules added via `firewall-bridge-windows.ps1` target ALL three profiles (`Domain,Private,Public`) by design — without `Public` the Ethernet LAN would still be blocked even though it's the active surface. Don't narrow the profile scope without checking `Get-NetConnectionProfile` first.

**zsh `echo` mangles `\b` in PowerShell paths sent via `-EncodedCommand`.** When piping a PowerShell script to PowerShell `-EncodedCommand` over SSH (the only sane way to avoid layered single-/double-quote escaping between zsh → CMD → pwsh), the natural pipeline is `echo -n "$PS" | iconv -f UTF-8 -t UTF-16LE | base64 | tr -d '\n'`. **`echo -n` on macOS zsh processes backslash escapes by default** (POSIX-noncompliant, zsh-historical behaviour), so any Windows path with `\b` — `C:\1-bit-bridge\bin`, `\bridge.exe`, `\backups` — gets `\b` collapsed to a literal 0x08 byte (backspace) BEFORE the base64 layer. PowerShell then receives a string with embedded BS control characters and fails with a `Test-Path` / `Rename-Item` error like `missing C:\1-bit-bridge_x0008_in\bridge.exe.new` (PowerShell's error formatter renders the BS byte as `_x0008_`). The failure mode is invisible at the source — single-quoted PowerShell strings don't help because the mangling happens in zsh, not PowerShell. **Always use `printf '%s' "$PS"` instead of `echo -n "$PS"`** as the first stage of the pipeline. `printf '%s'` is POSIX-defined to emit bytes verbatim. Bash's `echo` happens to default to non-interpret on macOS, but zsh is the default Mac shell now — code for zsh. Burned 2026-05-22 during the post-PR-#285 home-pc deploy: the rename step silently truncated three different paths until I switched the pipeline to `printf`.

### bridge.ars.md (Linux VPS, public mode, SSH `<VPS-SSH>`)

Public-internet-reachable bridge running in `deployment.mode: public` against a Backblaze B2 bucket mounted via rclone. Stood up 2026-05-24 (steps 1–11 of the post-PR-#296 deployment plan — iOS pairing deferred until the iOS Mirror PR for `PinningDelegate.shouldSkipPinning` extension lands; see `~/Desktop/to-do/2026-05-24-ios-mirror-public-vps-pinning-exemption.md`).

**Coordinates:**

| Item | Value |
|---|---|
| SSH | `ssh -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH>` (key was copied from `~/Downloads/<VPS-SSH-KEY>.pem`, perms 0600) |
| Default shell | bash (Linux) |
| Service manager | systemd |
| Binary path | `/usr/local/bin/bridge` (setcap `cap_net_bind_service=+ep` for non-root :443 bind) |
| Config path | `/home/arsenie/bridge-data/bridge.yaml` (parent 0700, file 0600) |
| Data dir | `/home/arsenie/bridge-data/data/` (adminauth.json, server.crt/.key, acme/, backups/) |
| Library mount | `/mnt/music` (rclone B2 FUSE, `--read-only --vfs-cache-mode full --vfs-cache-max-size 5G`) |
| Log | `journalctl -u 1-bit-bridge` (systemd journal — no separate file) |
| Public endpoint | `https://bridge.ars.md/` (autocert direct-TLS on :443) |
| Admin endpoint | `https://bridge.ars.md:7789/` (autocert + adminauth session cookie) |

**systemd units:**

| Unit | Notes |
|---|---|
| `rclone-music.service` | `Type=notify`, `User=arsenie`, mounts `b2-music:1bitbucket` → `/mnt/music`. Bridge `Requires=` this so a failed mount blocks the bridge from starting (avoids the FUSE-drop deletion-pass trap). |
| `1-bit-bridge.service` | `Type=simple`, `User=arsenie`, `Requires=rclone-music.service`, `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `Restart=always` (NOT `on-failure` — see the console-restart note below), `ProtectSystem=full` (via the `variants.conf` drop-in — see the sandbox note below) + `ReadWritePaths=/home/arsenie/bridge-data`. |
| `~/.config/rclone/rclone.conf` | `b2-music` backend defined as `s3` (NOT `b2` — bucket-scoped keys fail on the b2 backend); endpoint `s3.eu-central-003.backblazeb2.com`, `provider=Other`. Operator wrote it via `rclone config create` on the host — secrets never traverse this assistant or the local workstation. |

**`Restart=always` is load-bearing for the admin-console Restart button (changed from `on-failure` 2026-08-05 after an outage).** `POST /api/restart` routes through the same graceful-shutdown closure as SIGTERM and **exits 0** — the design relies on the service manager to bring the process back up. `Restart=on-failure` ignores clean exits, so every web-console restart ended as a permanent shutdown (bridge down until a manual `systemctl start`; that is exactly what happened at 02:50 UTC on 2026-08-05). `Restart=always` also covers the auto-updater's install path, which exits the same way. Explicit `systemctl stop` / `restart` are unaffected — systemd never auto-restarts after its own stop job. Don't revert to `on-failure`.

**systemd sandbox vs the FUSE mounts (changed 2026-07-16 — read before touching the unit).** The admin Library Inspector generates variants IN-DAEMON (PRs #435+/#504), so `/mnt/bridge-variants` must be WRITABLE for the service. The original unit used `ProtectSystem=strict` + a `ReadOnlyPaths=/mnt/bridge-variants` drop-in ("CLI-only generation, RO is sufficient") — with that config every Inspector Generate batch failed with `sox FAIL … Read-only file system` (field-reported 2026-07-16; the 167 GB of pre-existing optimized variants were written by CLI runs OUTSIDE the sandbox, which is why this never surfaced before). Two hard-won facts about the fix:

- **systemd CANNOT bind these user-FUSE rclone mounts into the sandbox at all.** `ReadWritePaths=/mnt/bridge-variants` crash-loops the service with `Failed to set up mount namespacing: … Permission denied` — pid-1 (root) can't traverse an rclone mount that lacks `allow_other`. The deceptive part: `ReadOnlyPaths=/mnt/music` *appeared* to work under `ProtectSystem=strict` only because it was a NO-OP there (everything was already read-only, so systemd skipped the bind); the moment `ProtectSystem=full` made it a real bind, it failed the same way.
- **Current shape** (in `/etc/systemd/system/1-bit-bridge.service.d/variants.conf`): `ProtectSystem=full` (only `/usr` `/boot` `/efi` `/etc` read-only — no `/mnt` binds needed) + `ReadOnlyPaths=` (clears the music bind). Library protection does NOT regress: `/mnt/music` is mounted read-only by `rclone-music.service` itself (`ro` mount option) — the mount is the enforcement, not the sandbox. `/mnt/bridge-variants` writes are governed by FUSE perms (`user_id=1000` = the service user). Verified post-change from inside the service's own namespace: variants writable, music still EROFS.
- Don't reintroduce `ProtectSystem=strict` or any `/mnt` path in `ReadOnlyPaths`/`ReadWritePaths` without re-testing a service restart — the failure mode is a crash-loop (status=226/NAMESPACE), i.e. a full outage, not a degraded feature. The alternative path (enabling `allow_other` on the rclone mounts + `user_allow_other` in `/etc/fuse.conf`) would let real binds work but touches the mount units; deliberately not taken.

**Firewall posture (ufw):**
- `22/tcp` (SSH) — **whitelisted to operator's public IP only**.
- `7789/tcp` (admin console) — **whitelisted to operator's public IP only**.
- `443/tcp` + `443/udp` (HTTPS + HTTP/3) — open to the internet (the public API + autocert TLS-ALPN-01 challenge land here).

**Connection-issues debugging hint**: if SSH or admin-console access starts failing intermittently, **check whether the operator's public IP has changed** (residential CGNAT rotation, switching networks, VPN flip). The 22/7789 whitelist is keyed on the IP at standup-time (2026-05-24). The fix is to update the ufw rules; the bridge itself doesn't care. Public-internet :443 access is unaffected by IP changes — if iOS clients can still reach `/v1/health` but you can't SSH, that's the whitelist class of issue.

**Host audio toolchain (upscale + analysis):** because this host runs the audio-analysis feature (and can run upscale/optimize), it needs the audio toolchain installed via apt: `sudo apt install sox libsox-fmt-all ffmpeg`. `sox` drives the offline upscale/optimize pipeline and is the primary analysis decoder; **`libsox-fmt-all` supplies FLAC** — Debian/Ubuntu split it into a separate plugin package and the bridge forces `-t flac`, so plain `sox` alone fails the `internal/doctor` FLAC check; `ffmpeg`/`ffprobe` are the analysis fallback decoder for AAC/m4a that sox can't open. Without these, `analysis.enabled` / `upscale.enabled` silently degrade to off at `bridge serve` startup (the LookPath probe logs a `disabling` line). This is a systemd/apt-host prerequisite only — the Docker image bundles the same toolchain by default (see [`docs/docker.md`](docker.md)).

**Host fingerprint toolchain (acoustic fingerprinting):** the fallback shells out to
`fpcalc` (Chromaprint), which is a *separate* dependency from sox — it links its own
FFmpeg and is unaffected by the `libsox-fmt-all` split above. Install per OS:

| OS | Command |
|---|---|
| Debian/Ubuntu | `sudo apt install libchromaprint-tools` |
| macOS | `brew install chromaprint` |
| Windows | `winget install AcoustID.Chromaprint` |
| Alpine / Docker | bundled in the image (`apk add chromaprint`) |

Two traps. **The Debian binary lives in `-tools`, not `libchromaprint1`** — the same
split-package shape as `sox` / `libsox-fmt-all`, and installing the library alone
leaves `fpcalc` absent. **On Windows, winget edits the machine PATH but an existing
shell keeps the old one**, so a fresh SSH session sees no `fpcalc` until it
reconnects; `$env:LOCALAPPDATA\Microsoft\WinGet\Links` is where it lands.

Fingerprinting also needs a free AcoustID application key
(https://acoustid.org/new-application), set as `ACOUSTID_API_KEY` or
`fingerprint.apiKey`. With the feature enabled but either prerequisite missing the
bridge boots normally and disables it with one stderr line — `bridge doctor`'s
`fingerprint-toolchain` check is the durable place to see why.

**Cost note for `bridge.ars.md` specifically.** Its library is an rclone/B2 FUSE
mount, and fingerprinting decodes the first 120s of each candidate. rclone's default
`--vfs-read-chunk-size` (128 MiB) exceeds every music file, so the first read pulls
the **whole object** — `fingerprint.lengthSeconds` is a CPU lever, not an egress one.
Egress itself is cheap against B2's 3x-stored free allowance; the real cost is cache
thrash, since streaming a large backlog through the bounded VFS cache evicts whatever
is actually being listened to. Hence `fingerprint.maxPerRun` (default 500) and
`workers: 1`. Measure a sample with `rclone rc vfs/stats` before enabling it there.

**Helper scripts**: none on the host today — operator runs setup commands directly during install. Update flow uses the cross-compile + `scp .new` + two-step rename + `systemctl restart` pattern from "Step 2 — Windows production bridge" below, adapted for Linux (see canonical deploy procedure below).

**Canonical update procedure** (from operator's macOS workstation, on every merged runtime-behavior PR). The scripted form is [`deploy/linux/deploy-bridge-vps.sh`](../deploy/linux/deploy-bridge-vps.sh) (cross-compile → SHA-gated upload → detached swap → `setcap` → restart → health-polled verify → prune); the manual steps below are what it runs. **Prefer the script** — it retries the health poll and prunes old backups; the manual form below is for when you need to drive a step by hand, and it must keep the same detached dispatch to be safe:

```sh
# 1. Cross-compile against current main.
git checkout main && git pull --ff-only
GOOS=linux GOARCH=amd64 go build \
  -ldflags "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$(git describe --tags --always)" \
  -o dist/bridge-linux-amd64 ./cmd/bridge

# 2. Upload as .new + verify SHA-256 BEFORE swap (so a truncated upload
#    can't replace a working binary).
scp -i ~/.ssh/<VPS-SSH-KEY> dist/bridge-linux-amd64 \
    <VPS-SSH>:/tmp/bridge.new
shasum -a 256 dist/bridge-linux-amd64                                                                 # local
ssh -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH> 'sha256sum /tmp/bridge.new'                        # remote

# 3. Two-step rename swap + restart, DISPATCHED DETACHED so a dropped SSH
#    channel cannot kill the swap between the two `mv`s (see the note below —
#    do NOT run these inline over the channel). Keeps the .old-<ts> backup.
ssh -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH> "cat > /tmp/bridge-swap.sh" <<'EOS'
set -e
TS=$(date +%Y%m%d-%H%M%S)
sudo mv /usr/local/bin/bridge /usr/local/bin/bridge.old-$TS
sudo mv /tmp/bridge.new /usr/local/bin/bridge
sudo chmod +x /usr/local/bin/bridge
sudo setcap cap_net_bind_service=+ep /usr/local/bin/bridge
sudo systemctl restart 1-bit-bridge
sleep 3
echo "active: $(systemctl is-active 1-bit-bridge)"
echo "SWAP_DONE backup=/usr/local/bin/bridge.old-$TS"
EOS
ssh -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH> \
  'setsid nohup bash /tmp/bridge-swap.sh > /tmp/bridge-swap.log 2>&1 < /dev/null &'

# 4. Verify over :443, which is open to everyone — so this works even if SSH
#    is filtered right now, and needs no second SSH round trip.
curl -s https://bridge.ars.md/v1/health | jq '.serverVersion, .leCertNotAfter'
```

**Leave the `bridge.old-<ts>` backup ~24h** so a regression caught later has one-step rollback (`sudo mv /usr/local/bin/bridge /usr/local/bin/bridge.broken && sudo mv /usr/local/bin/bridge.old-<ts> /usr/local/bin/bridge && sudo systemctl restart 1-bit-bridge`).

**The script prunes those backups itself, but only AFTER health confirms the new binary serves traffic** (`KEEP_BACKUPS`, default 2 — the immediate rollback plus one behind it). Pruning earlier could delete the rollback path while it is still the thing you need. Two details are load-bearing:

- **Sorted by the NAME's timestamp, never `ls -t`.** The name carries the *swap* time; mtime carries the local *build* time, and they disagree routinely (a binary built at 10:23 and deployed at 11:49). Verified against a fixture whose mtimes run opposite to its names: an `ls -t` prune deletes **exactly the two newest** backups and keeps the two oldest.
- **A prune failure is a warning, not a deploy failure.** The deploy is already verified at that point, and this host's SSH flaps (below). Re-running the script prunes on the next pass.

**Why this matters: the backups compete with the rclone VFS cache for the root disk.** They accumulate at ~44 MB per deploy and were never pruned before 2026-08-17, when 100 of them held **4.1 GB of the 29 GB root — 85% full, 4.3 GB free**. The mount runs `--vfs-cache-max-size 5G`, so the cache could not have reached its configured size, and the SQLite DB plus `data/backups/` share that same disk. Pruning to two took it to 71% / 8.2 GB free. If free space is under ~6 GB here, check for accumulated `bridge.old-*` before suspecting the library mount.

**Don't `journalctl --vacuum-time` aggressively** during a debug loop — bridge logs are the only forensic surface (no separate log file path). 7-day default retention is fine; cut tighter only when disk pressure is real.

**The swap is dispatched DETACHED (`setsid nohup`), and the verification polls `:443` rather than SSH.** Run inline over the SSH channel, the swap dies wherever the connection dies — and the window between the two `mv`s is the one state with **no binary at `/usr/local/bin/bridge`**. The running process survives on its held inode, so nothing looks wrong until the next restart, which then fails under `Restart=always` and takes the bridge down. On 2026-08-17 a deploy dropped exactly inside step 4; it happened to die *before* the first `mv`, which was luck. Diagnose an interrupted swap by comparing `/usr/local/bin/bridge version` against `/tmp/bridge.new version` and checking whether a `bridge.old-<today's ts>` exists — if the newest backup predates the attempt, the first `mv` never ran and nothing is broken. Progress lands in `/tmp/bridge-swap.log` on the host.

**Cli-mode-only host.** Tailscale is NOT installed; `cfg.Tailscale.Mode` is `disabled`. The bridge advertises a single `https://bridge.ars.md/` endpoint and that's the only path iOS clients can use post-pairing. Adding Tailscale later is straightforward but currently out of scope.

**Update reminder.** This host should be brought up to current `main` after every merged runtime-behavior PR (same as `home-pc`). Docs-only / test-only merges can skip. See [Post-merge deployment](#post-merge-deployment) for the umbrella rule — bridge.ars.md is **step 3** alongside the local fixture (step 1) and `home-pc` (step 2).

**UDP buffer cap for HTTP/3** (`/etc/sysctl.d/999-bridge-quic.conf`, applied 2026-05-24). quic-go asks the kernel for 7 MB receive+send buffers at UDP-bind time; Linux's default 2 MB cap clamps that and logs the well-known `failed to increase receive buffer size (wanted: 7168 kiB, got 2048 kiB)` INFO line at boot. Raising `net.core.rmem_max` + `net.core.wmem_max` to 7340032 silences the warning AND lets quic-go actually use the headroom (which materially improves HTTP/3 throughput for clients on lossy cellular links — cellular RTT is high, so a too-small kernel buffer becomes the throughput ceiling).

**`999-` prefix is load-bearing**: Ubuntu's Azure cloud-image ships `/etc/sysctl.d/99-cloudimg-udp.conf` which pins `net.core.rmem_max = 1048576`. sysctl drop-ins are processed in **alphabetical** order with later files winning — `99-cloudimg-udp.conf` (`c` at index 3) sorts AFTER `99-bridge-quic.conf` (`b` at index 3), so a naive `99-` prefix gets clobbered. First attempt with `99-` looked correct in `sysctl --system` output (line `net.core.rmem_max = 7340032` printed) but the subsequent `99-cloudimg-udp.conf` line silently overrode it. **Verify with `sysctl net.core.rmem_max` AFTER `sysctl --system`** — NOT by trusting the apply-time output. Fix is a numerically-higher prefix (`999-`) or alphabetically-later filename. Re-applies idempotently across reboots via the `systemd-sysctl.service` unit. Bridge needs `systemctl restart 1-bit-bridge` to pick up the new ceiling (the UDP socket reads the cap once at `setsockopt(SO_RCVBUF)` time, then is fixed for that socket's lifetime). Burned 2026-05-24 during the post-deploy tuning pass.

**HTTP/3 packet pacing (`net.core.default_qdisc=fq`)** — recommended alongside the buffer cap above. quic-go paces sends in userspace, but a fair-queue kernel qdisc (`fq`) cooperates with that pacing and avoids bufferbloat-style latency spikes on egress — the configuration Google runs its own QUIC servers with. Add `net.core.default_qdisc=fq` to the same `/etc/sysctl.d/999-bridge-quic.conf` drop-in, `sudo sysctl --system`, then confirm with `sysctl net.core.default_qdisc` (same alphabetical-precedence caveat as the buffer keys — verify the live value, don't trust the apply-time output). Low-risk and reversible; measure the effect with the throughput telemetry below rather than assuming a win.

**Verifying UDP GSO is actually engaging** — quic-go uses UDP Generic Segmentation Offload (kernel ≥ 4.18) to hand the kernel batches larger than one MTU, cutting per-packet syscall overhead. To confirm it's active on a real kernel UDP socket: with a download in flight, run `sudo tcpdump -ni any 'udp port 443' -c 20 -v` and look for UDP payloads **larger than ~1500 bytes** leaving the host (the NIC/kernel segments downstream), or inspect `ss -uem`. The absence of quic-go's `failed to increase receive buffer size` line (after the buffer cap above) is the complementary signal that the socket got its headroom. **Caveat — tsnet-mode bridges only:** if a bridge ever runs Tailscale via embedded `tsnet` (neither current production bridge does — home-pc uses the host `tailscale.exe`, bridge.ars.md has Tailscale disabled), its HTTP/3 listener is a gVisor *userspace* socket that quic-go can't type-assert to `*net.UDPConn`, so GSO / `sendmmsg` / kernel pacing are silently disabled for that listener and the buffer cap helps only the outer WireGuard socket. Don't chase an "unoptimized" log line there — it's inherent to the userspace overlay, not a misconfiguration.

**Measuring the effect (data-driven tuning gate).** Since the download-telemetry change (PR #363), every large transfer (≥ 2 MiB — the `downloadThroughputMinBytes` floor) on `/v1/download` and `/v1/read` emits a `download_complete` structured log line carrying `proto`, `bytes_sent`, `duration_ms`, and `throughput_mbps`, and feeds the `bridge_http_download_throughput_mbps{proto}` Prometheus histogram (loopback `/metrics`). To compare before/after a tuning change: `journalctl -u 1-bit-bridge | grep download_complete` and bucket by `proto=h2` vs `proto=h3`. Apply one tuning change at a time, let real traffic flow, compare the distribution, keep or revert. Note the value is *effective delivery speed* (network ⊕ the iOS client's read pacing / disk I/O), so the h2-vs-h3 comparison is the trustworthy signal — not the absolute number.


## Post-merge deployment

After **any** main-branch merge that ships a runtime fix, regenerate the local test fixture AND the Windows production host's binary. The PR workflow above ends at "merge"; this section is what runs next so the visible bridges actually pick up the change. Skip this for docs-only / CLAUDE.md-only / test-only merges where no shipped binary changes behavior.

### Step 1 — local test fixture (`/tmp/bridge-live/`)

```sh
git checkout main && git pull --ff-only
make build
kill -TERM $(pgrep -f "bin/bridge serve --config /tmp/bridge-live")   # graceful — wait for STOPPED
nohup ./bin/bridge serve --config /tmp/bridge-live/bridge.yaml >> /tmp/bridge-live/serve.log 2>&1 &
disown
curl -sk https://127.0.0.1:7788/v1/health   # confirm new serverVersion
```

Health-check tail: `tail -25 /tmp/bridge-live/serve.log` — look for the single-`v` banner and (on tsnet-mode bridges) the `tsnet HTTP/3 listeners bound count=N ipsReported=N port=...` INFO line.

### Step 2 — Windows production bridge (`home-pc`)

Full coordinates + canonical update procedure live in the `home-pc (Windows)` subsection under "Production deployments" above — don't duplicate the deploy commands here; consult that section directly. The host now runs the scheduled-task setup (binary at `C:\1-bit-bridge\bin\bridge.exe`, scheduled task `1-bit-bridge (home-pc)`); the older service-based flow with the desktop-resident exe is deprecated.

**One-line summary**: `scp` any changed helper scripts → `pwsh ... setup-bridge-windows.ps1` (pulls `main`, runs `go build` in-place on the Windows host, refreshes YAML) → `pwsh ... task-bridge-windows.ps1` (re-registers + starts the scheduled task) → `curl -sk https://<host>:7788/v1/health` to confirm `serverVersion`.

**What's safe to skip across upgrades:**
- TLS cert / fingerprint — unchanged on an update, so paired iOS clients don't need re-pairing. **This is only true because `setup-bridge-windows.ps1` skips `bridge init` when a config already exists** (fixed 2026-06-01). To deliberately re-mint the cert (then every device must re-pair), run `rotate-cert-windows.ps1` — never `init -force`. If you ever see "config exists -- re-running init with -force" in the setup output, you're on the OLD script and it's about to break every pairing; abort and re-pull the fixed script.
- Config file — `setup-bridge-windows.ps1` only injects missing `customEndpoints` + `upscale.variantsDir` keys; existing config preserved.
- Library DB — schema migrations are append-only and idempotent; the same DB carries forward across releases.

### Step 3 — Linux VPS public-mode bridge (`bridge.ars.md`)

Public-internet bridge running `deployment.mode: public` against rclone-mounted B2. Cross-compile + scp + two-step swap pattern for linux/amd64 + systemd. Full coordinates + canonical update procedure live in the `bridge.ars.md` subsection under "Production deployments" above — don't duplicate the deploy commands here; consult that section directly.

**One-line summary**: `GOOS=linux GOARCH=amd64 go build … -o dist/bridge-linux-amd64 ./cmd/bridge` → `scp -i ~/.ssh/<VPS-SSH-KEY> … :/tmp/bridge.new` → SHA-256 verify → two-step `sudo mv` swap + `setcap cap_net_bind_service=+ep` + `systemctl restart 1-bit-bridge`.

**Public-mode-specific verification** (extends the `/v1/health` check):

```sh
curl -s https://bridge.ars.md/v1/health | jq '.serverVersion, .certNotAfter, .leCertNotAfter'
# Expect: ServerVersion string matches the build, certNotAfter ~397d out
# (self-signed pinned cert), leCertNotAfter ~90d out (Let's Encrypt).
```

**Connection-issues hint** (re-stated for the post-merge loop): SSH and admin-port 7789 are whitelisted to the operator's public IP in ufw. A residential CGNAT rotation or VPN flip can make those fail while `/v1/health` over :443 still works — that's a whitelist class of issue, NOT a bridge bug. Update ufw rules from the new IP.

**SSH-flap note (2026-08-17): it is not the key, and it is not necessarily all-or-nothing.**

- **Tell the classes apart by WHERE it fails.** `Operation timed out` on `:22` is a TCP-connect failure — it happens before any key is offered, so *no key can fix it*. A wrong key gives `Permission denied (publickey)` over an established connection instead. Confirm by probing ports: `:443` answering while `:22` is filtered on the same resolved IP is the allowlist.
- **It can flap within seconds, and per-port.** Back-to-back probes gave timeout, then open, then twelve consecutive failures — while `:7789` stayed reachable throughout from the same workstation. So a single failed `ssh` proves nothing; probe twice before concluding anything, and re-run the deploy script (it is idempotent, and the SHA gate means a partial upload can never replace a working binary).
- **A LAN host with its own allowlisted egress works as a pure TCP relay:** `ssh -J <RELAY-SSH> -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH>`. `-J` tunnels only TCP, so the key never leaves the workstation and no binary transits the relay — strictly better than copying either. To make the deploy script take that route, set **`SSH_OPTS="-J <RELAY-SSH>"`** (in `deploy/linux/.env` or inline). **Not `HOST`** — `HOST` is passed as ssh's target argument, so a ProxyJump form there cannot work; an earlier revision of this note said otherwise. An option whose *value* contains spaces (a full `ProxyCommand`) needs `~/.ssh/config` instead, since `SSH_OPTS` is word-split.
- **Verification never needs SSH.** `/v1/health` on `:443` is open to everyone, so poll `serverVersion` there to confirm a deploy landed — which is what the script now does.


## Diagnosing client behavior from the journal

`sudo journalctl -u 1-bit-bridge` logs every API request
(`msg=http … method=GET path=/v1/… status=NNN duration_ms=N bytes=N`) and is
the **ground truth for what an iOS client actually did and when** — device
screenshots and user recollection routinely mislead. The 2026-08-06
"Caching artwork ran for six hours" report was resolved entirely from
status histograms here (the answer: hundreds of `202 pending` retries for
permanently-imageless artists, fixed by the `no_image` terminal split).

Grep mechanics that cost time to learn:

- **`path=` carries NO query string** (slog logs `r.URL.Path`), so
  `/v1/manifest?since=…` appears as plain `path=/v1/manifest`. Grep plain
  paths — don't hunt for quoted query forms. journald only quotes paths
  containing special characters (scanner probes).
- **Public-deployment noise**: WARN 404s for `/`, `/.git/HEAD`, `/owa/…`,
  `/robots.txt` etc. are internet scanners, not the app. Filter to
  `path=/v1/` for client traffic.
- **Timestamps are UTC and the phone's timezone is NOT safe to assume** —
  anchor a user-reported timeline on distinctive request bursts (a manifest
  page run, an artwork fan-out), never on an assumed offset.
- Handy shape:
  `journalctl -u 1-bit-bridge --since "<ts>" --no-pager | grep msg=http | grep -E 'path=/v1/(artist-image|artwork)' | grep -oE 'status=[0-9]+' | sort | uniq -c`
- `/dlna/file/*` requests never appear here (TelemetryMiddleware, not the
  access logger) — absence of those lines is not evidence a renderer
  didn't fetch.

## After a deploy: expect iOS syncs to quietly defer

Restarting the service kicks off a **startup library scan** — minutes on a
warm, fully-enriched library (~10 min on the VPS's rclone/B2 mount), hours
only when an extractor/analysis backfill re-processes every track (the
2026-08-05 wf3→wf4 pass ran ~18 h at ~85 % CPU). While
`scanState.isScanning` is true, iOS **incremental** rescans silently no-op
by design (the busy-defer): the user sees "rescan finished in a second and
nothing changed", the journal shows zero `/v1/manifest` fetches. That is
the deferral working, not a bug — tell users to retry once
`/v1/health` shows `isScanning: false`.

Related support pattern: server-side row REMOVALS (dedup suppression,
deleted files) reach phones only via a **Full rescan** on the device —
incremental delta syncs never delete rows, so a track-count change the
server made will not appear from a plain Rescan no matter how many times
it runs.
