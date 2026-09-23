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

> **⚠️ The operator bridge moved (2026-09-22).** `bridge.ars.md` was replaced by
> a home NUC, reached over Tailscale as `<OPERATOR-SSH>`. The `bridge.ars.md`
> subsection below still describes a live host — it keeps running the **public
> demo** — but it is **no longer where the operator bridge lives**, so the
> "Step 3" post-merge target is now the NUC. Coordinates, unit name, paths and
> auth resolve in `ops/coordinates.local.md` as usual.
>
> Two things differ from the Azure host and both bite the tooling:
>
> - **Self-signed cert, so every health probe needs `curl -k`.**
>   `deploy/linux/deploy-bridge-vps.sh` polls `curl -s "$HEALTH_URL"` with no
>   `-k`, so on this host the poll can never match and the script exits 1 with
>   its rollback guidance AFTER a swap that actually succeeded. It does not roll
>   back, so nothing is broken — but the exit code lies. **Drive the manual form**
>   (cross-compile → scp `.new` → SHA gate → DETACHED swap → `setcap` → restart),
>   which is what the `bridge.ars.md` subsection documents, and verify with
>   `curl -sk` from the host over the ControlMaster.
> - **The library is a local USB SSD, not an rclone/B2 mount.** A full
>   re-extraction of 21,460 tracks took **under two minutes** on 2026-09-22
>   and **~70 s** on 2026-09-23, against the hours the B2-mounted VPS needed.
>   Don't carry the VPS's timing intuition over: size a re-extraction window
>   from the host's storage.
> - **`/tmp` here IS a tmpfs** (31 GB, RAM-backed), which is the opposite of
>   `bridge.ars.md` — and the `upscale.tempDir` note in that subsection says in
>   as many words to check this per host rather than assume it. `tempDir` is
>   set explicitly to `/home/arsenie/bridge-data/tmp`, on the disk-backed root,
>   so a faithful render staging ~5 GB cannot pressure RAM. **Don't clear that
>   setting on this host**: the empty default is "the OS temp dir", which here
>   means RAM. Verified 2026-09-23 with `findmnt /tmp`.

### home-pc (Windows, SSH `<HOMEPC-SSH>`)

Operator's home Windows machine. Reachable from the operator's macOS workstation via SSH (OpenSSH server, **session is auto-elevated to admin** — no UAC popup needed for `New-NetFirewallRule` etc.). PowerShell 7 (`pwsh`), Git, and Go are pre-installed. Tailscale runs in CLI mode (`tailscale.exe` on PATH). **Audio toolchain for the DSD → PCM renditions (`upscale.dsdRender.enabled`, PR #863):** `choco install ffmpeg` — the stock Chocolatey build carries the `dsd_*` and `dst` decoders; verify with `ffmpeg -hide_banner -decoders | findstr dsd_` (all FOUR `dsd_*` names — the runtime probe requires every one) and, separately, `ffmpeg -hide_banner -decoders | findstr " dst "`, since `dst` is its own capability and only DST-compressed DSDIFF depends on it, before flipping the flag, and `bridge doctor` (`dsd-render-toolchain`) names the install line when it is missing. Enabling the flag starts a sweep that reads every DSD track once, so do it when the library disk is not otherwise busy.

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
| Admin endpoint | **`https://bridge.ars.md:7790/`** (autocert + adminauth session cookie). Since 2026-09-16 **`:7789` on this host is HAProxy's tenant-console frontend** (`<id>.cloud.1-bit.app`, by Host, under the `*.cloud.1-bit.app` wildcard) — opening `bridge.ars.md:7789` gets that certificate and a `421`, which reads as "wrong certificate" in a browser. This row said `:7789` until 2026-09-18. |

**systemd units:**

| Unit | Notes |
|---|---|
| `rclone-music.service` | `Type=notify`, `User=arsenie`, mounts `b2-music:1bitbucket` → `/mnt/music`. Bridge `Requires=` this so a failed mount blocks the bridge from starting (avoids the FUSE-drop deletion-pass trap). |
| `1-bit-bridge.service` | `Type=simple`, `User=arsenie`, `Requires=rclone-music.service`, `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `Restart=always` (NOT `on-failure` — see the console-restart note below), `ProtectSystem=full` (via the `variants.conf` drop-in — see the sandbox note below) + `ReadWritePaths=/home/arsenie/bridge-data`. |
| `~/.config/rclone/rclone.conf` | `b2-music` backend defined as `s3` (NOT `b2` — bucket-scoped keys fail on the b2 backend); endpoint `s3.eu-central-003.backblazeb2.com`, `provider=Other`. Operator wrote it via `rclone config create` on the host — secrets never traverse this assistant or the local workstation. |

**`Restart=always` is load-bearing for the admin-console Restart button (changed from `on-failure` 2026-08-05 after an outage).** `POST /api/restart` routes through the same graceful-shutdown closure as SIGTERM and **exits 0** — the design relies on the service manager to bring the process back up. `Restart=on-failure` ignores clean exits, so every web-console restart ended as a permanent shutdown (bridge down until a manual `systemctl start`; that is exactly what happened at 02:50 UTC on 2026-08-05). `Restart=always` also covers the auto-updater's install path, which exits the same way. Explicit `systemctl stop` / `restart` are unaffected — systemd never auto-restarts after its own stop job. Don't revert to `on-failure`.

**systemd sandbox vs the FUSE mounts (changed 2026-07-16 — read before touching the unit).** The admin console generates variants IN-DAEMON (PRs #435+/#504; the surface was the Library Inspector until 2026-08-24 and is now Browse — album, artist and folder panels — plus the Roots page), so `/mnt/bridge-variants` must be WRITABLE for the service. The original unit used `ProtectSystem=strict` + a `ReadOnlyPaths=/mnt/bridge-variants` drop-in ("CLI-only generation, RO is sufficient") — with that config every in-daemon Generate batch failed with `sox FAIL … Read-only file system` (field-reported 2026-07-16; the 167 GB of pre-existing optimized variants were written by CLI runs OUTSIDE the sandbox, which is why this never surfaced before). Two hard-won facts about the fix:

- **systemd CANNOT bind these user-FUSE rclone mounts into the sandbox at all.** `ReadWritePaths=/mnt/bridge-variants` crash-loops the service with `Failed to set up mount namespacing: … Permission denied` — pid-1 (root) can't traverse an rclone mount that lacks `allow_other`. The deceptive part: `ReadOnlyPaths=/mnt/music` *appeared* to work under `ProtectSystem=strict` only because it was a NO-OP there (everything was already read-only, so systemd skipped the bind); the moment `ProtectSystem=full` made it a real bind, it failed the same way.
- **Current shape** (in `/etc/systemd/system/1-bit-bridge.service.d/variants.conf`): `ProtectSystem=full` (only `/usr` `/boot` `/efi` `/etc` read-only — no `/mnt` binds needed) + `ReadOnlyPaths=` (clears the music bind). Library protection does NOT regress: `/mnt/music` is mounted read-only by `rclone-music.service` itself (`ro` mount option) — the mount is the enforcement, not the sandbox. `/mnt/bridge-variants` writes are governed by FUSE perms (`user_id=1000` = the service user). Verified post-change from inside the service's own namespace: variants writable, music still EROFS.
- Don't reintroduce `ProtectSystem=strict` or any `/mnt` path in `ReadOnlyPaths`/`ReadWritePaths` without re-testing a service restart — the failure mode is a crash-loop (status=226/NAMESPACE), i.e. a full outage, not a degraded feature. The alternative path (enabling `allow_other` on the rclone mounts + `user_allow_other` in `/etc/fuse.conf`) would let real binds work but touches the mount units; deliberately not taken.

**Firewall posture — the Azure NSG (`1bitbridge-nsg`) is the allowlist; ufw is not a gate.** `sudo ufw status numbered` on 2026-09-18: `ALLOW IN Anywhere` for OpenSSH, 80/tcp, 443/tcp+udp, 7789/tcp, 7790/tcp and 8443/tcp, v4 and v6 — nothing is source-restricted there any more (it was, until the NSG took that over on 2026-09-16). Editing ufw from a new IP therefore changes nothing.
- `22/tcp` (SSH) — **NSG source-allowlisted to the operator's ranges**.
- `7790/tcp` (the operator's admin console) — **NSG source-allowlisted to the operator's ranges**. `7789/tcp` (HAProxy's tenant-console frontend) and `8443/tcp` (the tenant API passthrough) are open to Any.
- `443/tcp` + `443/udp` (HTTPS + HTTP/3) — open to the internet (the public API + autocert TLS-ALPN-01 challenge land here).

**Connection-issues debugging hint**: if SSH or admin-console access starts failing intermittently, **check whether the operator's public IP has changed** (residential CGNAT rotation, switching networks, VPN flip). The `:22` and `:7790` allowlists live in the Azure NSG and are keyed on the operator's ranges; ufw allows both ports from anywhere (verified 2026-09-18), so editing ufw changes nothing. The fix is to add the new source range to the NSG rules; the bridge itself doesn't care. Public-internet :443 access is unaffected by IP changes — if iOS clients can still reach `/v1/health` but you can't SSH, that's the whitelist class of issue.

**Host audio toolchain (upscale + analysis):** because this host runs the audio-analysis feature (and can run upscale/optimize), it needs the audio toolchain installed via apt: `sudo apt install sox libsox-fmt-all ffmpeg`. `sox` drives the offline upscale/optimize pipeline and is the primary analysis decoder; **`libsox-fmt-all` supplies FLAC** — Debian/Ubuntu split it into a separate plugin package and the bridge forces `-t flac`, so plain `sox` alone fails the `internal/doctor` FLAC check; `ffmpeg`/`ffprobe` are the analysis fallback decoder for AAC/m4a that sox can't open, and — since PR #863 — the decoder for the DSD → PCM renditions (`upscale.dsdRender.enabled`): the apt build carries the `dsd_*` + `dst` decoders (verify with `ffmpeg -hide_banner -decoders | grep -E ' dsd_| dst '`; `bridge doctor`'s `dsd-render-toolchain` check says the same). Rendering scratch goes to `upscale.tempDir` (default under the system temp dir) — keep it on LOCAL disk, never on the B2 FUSE mount, and mind that a faithful 176.4 kHz render of an hour-long track stages ~5 GB there. The three prerequisites degrade INDEPENDENTLY, so read `bridge doctor` rather than assuming one verdict covers them: no `sox` (or no FLAC handler) turns `upscale.enabled` off at `bridge serve` startup and takes `analysis.enabled`'s primary decoder with it (the LookPath probe logs a `disabling` line); an `ffmpeg` missing any of the four `dsd_*` decoders leaves `upscale.dsdRender.enabled` written but INERT — `/v1/health` stops advertising `dsdRender` and every DSD job is refused with a typed error — while PCM upscaling carries on unaffected; and an `ffmpeg` that has the `dsd_*` decoders but not `dst` renders plain DSF/DFF normally and skips only DST-compressed DSDIFF. This is a systemd/apt-host prerequisite only — the Docker image bundles the same toolchain by default (see [`docs/docker.md`](docker.md)).

**Set `upscale.tempDir` EXPLICITLY on this host — do not leave it empty.** The unit runs `PrivateTmp=yes`, so an empty `tempDir` (the "OS temp dir" default) puts the daemon's Stage A scratch inside a per-service mount namespace that **no CLI process can see**. `bridge --gc`'s `PurgeStaleRenderScratch` then sweeps the *real* `/tmp` and never the daemon's, so the one cleanup path for scratch orphaned by a SIGKILL or power loss silently covers nothing. (systemd wipes the private tmp on every restart, which is a second net, but it only fires on a restart.) `bridge.ars.md` is set to `/home/arsenie/bridge-data/tmp` — inside the unit's `ReadWritePaths` carve-out, on the same local ext4 root as `/tmp` (checked: `/tmp` here is **not** a separate tmpfs, so it is disk-backed either way and a multi-GB scratch cannot pressure RAM — verify with `findmnt /tmp` before assuming this on another host), and unambiguously not the B2 mount. The render creates the `1-bit-bridge-render/` subdirectory itself via `MkdirAll`, so only the parent needs to exist and be writable.

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

**⚠️ A BINARY ROLLBACK MUST REVERT THE CONFIG IN THE SAME STEP WHENEVER THE DEPLOY ADDED A CONFIG KEY — ROLLING BACK PAST A CONFIG KEY CRASH-LOOPS THE SERVICE.** The loader is strict (`dec.KnownFields(true)` — a typo-catcher, so an unknown YAML key *fails the load*, it does not warn), and the unit is `Restart=always`. So an older binary meeting a newer config does not start, retries every 5 s, and the bridge is down — while `/usr/local/bin/bridge` looks perfectly fine and the rollback command above reports success. The one-step rollback in the previous paragraph is correct only for a deploy that changed no config. Concretely, `v0.1.9-154` (2026-09-08) introduced `upscale.dsdRender` and `upscale.tempDir`, so rolling back to `v0.1.9-149` or earlier requires the config backup too — and the [release deploy ledger](#release-deploy-ledger) below records every key each later deploy added:

```bash
# Rollback for a deploy that ALSO added config keys. Config FIRST — while the
# new binary is still running and can still read the new config, so there is no
# window where the running binary and the on-disk config disagree.
ls -1t /home/arsenie/bridge-data/bridge.yaml.bak-*   # the deploy leaves one per config change
cp -p /home/arsenie/bridge-data/bridge.yaml.bak-<ts> /home/arsenie/bridge-data/bridge.yaml
sudo mv /usr/local/bin/bridge /usr/local/bin/bridge.broken
sudo mv /usr/local/bin/bridge.old-<ts> /usr/local/bin/bridge
sudo setcap cap_net_bind_service=+ep /usr/local/bin/bridge   # the swap drops the file capability
sudo systemctl restart 1-bit-bridge
systemctl is-active 1-bit-bridge && curl -s https://bridge.ars.md/v1/health | jq '.serverVersion'
```

**Validate a config edit BEFORE restarting, never after** — `bridge doctor --config <path>` loads and validates it in a throwaway process, so a bad key is a non-zero exit against a service that is still happily serving. Restarting first turns the same typo into a crash-loop you then have to diagnose from the journal. For the same reason, **deploy the binary first and edit the config second** when a release adds keys: the running (old) binary never sees a key it does not know, and the deploy stays independently verifiable before any behaviour changes.

**The script prunes those backups itself, but only AFTER health confirms the new binary serves traffic** (`KEEP_BACKUPS`, default 2 — the immediate rollback plus one behind it). Pruning earlier could delete the rollback path while it is still the thing you need. Two details are load-bearing:

- **Sorted by the NAME's timestamp, never `ls -t`.** The name carries the *swap* time; mtime carries the local *build* time, and they disagree routinely (a binary built at 10:23 and deployed at 11:49). Verified against a fixture whose mtimes run opposite to its names: an `ls -t` prune deletes **exactly the two newest** backups and keeps the two oldest.
- **A prune failure is a warning, not a deploy failure.** The deploy is already verified at that point, and this host's SSH flaps (below). Re-running the script prunes on the next pass.

**Why this matters: the backups compete with the rclone VFS cache for the root disk.** They accumulate at ~44 MB per deploy and were never pruned before 2026-08-17, when 100 of them held **4.1 GB of the 29 GB root — 85% full, 4.3 GB free**. The mount runs `--vfs-cache-max-size 5G`, so the cache could not have reached its configured size, and the SQLite DB plus `data/backups/` share that same disk. Pruning to two took it to 71% / 8.2 GB free. If free space is under ~6 GB here, check for accumulated `bridge.old-*` before suspecting the library mount.

**`journalctl -p warning` FINDS NOTHING for either bridge — grep the text instead.** The
bridge writes every slog level to stderr, and systemd stamps a journal priority from the
*stream*, not from the `level=` field inside the line. So `-p warning` filters on a
priority the bridge never sets, and returns zero however bad things are. Measured on
bridge.1-bit.app, same 24 h window:

| Query | Hits |
|---|---|
| `journalctl -u 1-bit-bridge -p warning` | **0** |
| `journalctl -u 1-bit-bridge \| grep -c 'level=WARN'` | **1325** |
| `journalctl -u 1-bit-bridge \| grep -c 'level=ERROR'` | 0 |

This is a health check that looks like it passed. Use:

```sh
journalctl -u 1-bit-bridge --since '-24h' --no-pager | grep -c 'level=ERROR'
journalctl -u 1-bit-bridge --since '-24h' --no-pager | grep 'level=WARN' | grep -v 'component=http'
```

The `component=http` exclusion matters on a public host: those 1325 WARNs are all 404s
from internet scanners probing `:443` (`GET /`, `/favicon.ico`, `/api/v2/static/not.found`)
— expected noise, and they drown any real warning in an unfiltered grep. A bridge behind
a LAN or a whitelist sees none of it, so don't port the exclusion there by reflex.
(Found 2026-08-26 while health-checking the demo bridge, after using `-p warning` on
bridge.ars.md and reporting a clean result the query could not have established. That
one happened to be clean; the check hadn't shown it.)

**Don't `journalctl --vacuum-time` aggressively** during a debug loop — bridge logs are the only forensic surface (no separate log file path). 7-day default retention is fine; cut tighter only when disk pressure is real.

**The swap is dispatched DETACHED (`setsid nohup`), and the verification polls `:443` rather than SSH.** Run inline over the SSH channel, the swap dies wherever the connection dies — and the window between the two `mv`s is the one state with **no binary at `/usr/local/bin/bridge`**. The running process survives on its held inode, so nothing looks wrong until the next restart, which then fails under `Restart=always` and takes the bridge down. On 2026-08-17 a deploy dropped exactly inside step 4; it happened to die *before* the first `mv`, which was luck. Diagnose an interrupted swap by comparing `/usr/local/bin/bridge version` against `/tmp/bridge.new version` and checking whether a `bridge.old-<today's ts>` exists — if the newest backup predates the attempt, the first `mv` never ran and nothing is broken. Progress lands in `/tmp/bridge-swap.log` on the host.

**Cli-mode-only host.** Tailscale is NOT installed; `cfg.Tailscale.Mode` is `disabled`. The bridge advertises a single `https://bridge.ars.md/` endpoint and that's the only path iOS clients can use post-pairing. Adding Tailscale later is straightforward but currently out of scope.

**Update reminder.** This host should be brought up to current `main` after every merged runtime-behavior PR (same as `home-pc`). Docs-only / test-only merges can skip. See [Post-merge deployment](#post-merge-deployment) for the umbrella rule — bridge.ars.md is **step 3** alongside the local fixture (step 1) and `home-pc` (step 2).

**UDP buffer cap for HTTP/3** (`/etc/sysctl.d/999-bridge-quic.conf`, applied 2026-05-24). quic-go asks the kernel for 7 MB receive+send buffers at UDP-bind time; Linux's default 2 MB cap clamps that and logs the well-known `failed to increase receive buffer size (wanted: 7168 kiB, got 2048 kiB)` INFO line at boot. Raising `net.core.rmem_max` + `net.core.wmem_max` to 7340032 silences the warning AND lets quic-go actually use the headroom (which materially improves HTTP/3 throughput for clients on lossy cellular links — cellular RTT is high, so a too-small kernel buffer becomes the throughput ceiling).

**`999-` prefix is load-bearing**: Ubuntu's Azure cloud-image ships `/etc/sysctl.d/99-cloudimg-udp.conf` which pins `net.core.rmem_max = 1048576`. sysctl drop-ins are processed in **alphabetical** order with later files winning — `99-cloudimg-udp.conf` (`c` at index 3) sorts AFTER `99-bridge-quic.conf` (`b` at index 3), so a naive `99-` prefix gets clobbered. First attempt with `99-` looked correct in `sysctl --system` output (line `net.core.rmem_max = 7340032` printed) but the subsequent `99-cloudimg-udp.conf` line silently overrode it. **Verify with `sysctl net.core.rmem_max` AFTER `sysctl --system`** — NOT by trusting the apply-time output. Fix is a numerically-higher prefix (`999-`) or alphabetically-later filename. Re-applies idempotently across reboots via the `systemd-sysctl.service` unit. Bridge needs `systemctl restart 1-bit-bridge` to pick up the new ceiling (the UDP socket reads the cap once at `setsockopt(SO_RCVBUF)` time, then is fixed for that socket's lifetime). Burned 2026-05-24 during the post-deploy tuning pass.

**HTTP/3 packet pacing (`net.core.default_qdisc=fq`)** — recommended alongside the buffer cap above. quic-go paces sends in userspace, but a fair-queue kernel qdisc (`fq`) cooperates with that pacing and avoids bufferbloat-style latency spikes on egress — the configuration Google runs its own QUIC servers with. Add `net.core.default_qdisc=fq` to the same `/etc/sysctl.d/999-bridge-quic.conf` drop-in, `sudo sysctl --system`, then confirm with `sysctl net.core.default_qdisc` (same alphabetical-precedence caveat as the buffer keys — verify the live value, don't trust the apply-time output). Low-risk and reversible; measure the effect with the throughput telemetry below rather than assuming a win.

**Verifying UDP GSO is actually engaging** — quic-go uses UDP Generic Segmentation Offload (kernel ≥ 4.18) to hand the kernel batches larger than one MTU, cutting per-packet syscall overhead. To confirm it's active on a real kernel UDP socket: with a download in flight, run `sudo tcpdump -ni any 'udp port 443' -c 20 -v` and look for UDP payloads **larger than ~1500 bytes** leaving the host (the NIC/kernel segments downstream), or inspect `ss -uem`. The absence of quic-go's `failed to increase receive buffer size` line (after the buffer cap above) is the complementary signal that the socket got its headroom. **Caveat — tsnet-mode bridges only:** if a bridge ever runs Tailscale via embedded `tsnet` (neither current production bridge does — home-pc uses the host `tailscale.exe`, bridge.ars.md has Tailscale disabled), its HTTP/3 listener is a gVisor *userspace* socket that quic-go can't type-assert to `*net.UDPConn`, so GSO / `sendmmsg` / kernel pacing are silently disabled for that listener and the buffer cap helps only the outer WireGuard socket. Don't chase an "unoptimized" log line there — it's inherent to the userspace overlay, not a misconfiguration.

**Measuring the effect (data-driven tuning gate).** Since the download-telemetry change (PR #363), every large transfer (≥ 2 MiB — the `downloadThroughputMinBytes` floor) on `/v1/download` and `/v1/read` emits a `download_complete` structured log line carrying `proto`, `bytes_sent`, `duration_ms`, and `throughput_mbps`, and feeds the `bridge_http_download_throughput_mbps{proto}` Prometheus histogram (loopback `/metrics`). To compare before/after a tuning change: `journalctl -u 1-bit-bridge | grep download_complete` and bucket by `proto=h2` vs `proto=h3`. Apply one tuning change at a time, let real traffic flow, compare the distribution, keep or revert. Note the value is *effective delivery speed* (network ⊕ the iOS client's read pacing / disk I/O), so the h2-vs-h3 comparison is the trustworthy signal — not the absolute number.


### Demo bridge (`bridge.1-bit.app` — public read-only demo; ON bridge.ars.md since 2026-09-16)

Public read-only demo bridge behind the iOS app's **"Add demo bridge"** one-tap source (Sources → add menu). Exists for App Review and for users who want to see bridge features without running their own server. Stood up 2026-08-18 on its own Azure VM (UK South); **moved onto the bridge.ars.md host on 2026-09-16** as its own systemd unit + binary + user behind that host's HAProxy front door (the move is logged in the conductor repo's `ops/azure-migration.md`; the old VM is retired). Two properties define it and both are load-bearing:

- **`demo.enabled: true`** — playlist backup, favorites and playback-history uploads are structurally disabled (typed 404s; `/v1/health` drops their five feature flags and advertises `demoMode`), and demo clients leave no device rows. The bridge collects NOTHING from demo users; the iOS app locks its sync toggles off when it sees `demoMode`. **Never disable this in prod** — it is what makes the app's "collects nothing" claim true.
- **`demo.tokenSHA256`** — the static bearer token every installed iOS app carries (`DemoBridgeConfiguration` in the iOS repo). The hash lives in bridge.yaml; the raw token lives only in the app + the iOS repo. **Never change or remove it** — a changed hash 401s every shipped app until its next App Store release. It survives a `tokens.json` / dataDir wipe by construction (config-seeded, in-memory).

**Coordinates** (everything else about the host — NSG, ufw, HAProxy, ZFS — is the bridge.ars.md section above and the conductor repo's `ops/hosts.md`):

| Item | Value |
|---|---|
| Host / SSH | the bridge.ars.md VM: `ssh -i <VPS-SSH-KEY> arsenie@bridge.ars.md` (Azure NSG source-allowlists :22 — see the SSH-flap note above; multiplex over one connection) |
| Service manager | systemd `1-bit-bridge-demo.service` — `User=onebit-demo` (nologin system user), the tenant template's hardening (`ProtectSystem=strict`, `ProtectHome=yes`, empty capability set, `MemoryMax=2G`) |
| Binary | `/usr/local/bin/bridge-demo` — the RELEASE ARTIFACT (see below), **separate from** `/usr/local/bin/bridge`, which is the operator's own bridge on the same host; a deploy of one never restarts the other |
| Config | `/srv/onebit-demo/bridge.yaml` (owner `onebit-demo`, 0600 — `sudo cat` to read) |
| Data dir | `/srv/onebit-demo/data/` (adminauth.json, server.crt/.key, acme/, backups/, transcoded/, waveforms/, bridge.db, atlas-harvest.json) |
| Library | `/srv/onebit-demo/library/` — GENERATED demo content only (see `tools/demo-library/`); never put real/licensed music here |
| Storage | ZFS dataset `tank/public-demo` (quota 24 G, lz4) mounted at `/srv/onebit-demo` — the SAME path the old VM used, so nothing below this table changed |
| Log | `journalctl -u 1-bit-bridge-demo` |
| Public endpoint | `https://bridge.1-bit.app/` → HAProxy `:443` SNI passthrough (`use_backend demo_bridge if { req.ssl_sni -i bridge.1-bit.app }` in `1-bit-conductor/host/haproxy/haproxy.cfg`, pinned by `TestTenantsStayPassthroughOn443`) → the bridge on loopback `127.0.0.1:8446`. The bridge terminates its OWN TLS (autocert, TLS-ALPN-01 rides the passthrough), so the certificate the shipped apps see is the bridge's, never the proxy's |
| Admin console | `https://127.0.0.1:7791/` — **loopback-bound, reach via SSH tunnel only** (`ssh -L 7791:127.0.0.1:7791 -i <VPS-SSH-KEY> arsenie@bridge.ars.md`, then open `https://127.0.0.1:7791/`); credentials in `/srv/onebit-demo/ADMIN_CREDENTIALS.txt` (`sudo cat`) |
| Bridge CLIs | **always as the service user**: `sudo -u onebit-demo /usr/local/bin/bridge-demo <cmd> -config /srv/onebit-demo/bridge.yaml …` — a root- or `arsenie`-run CLI leaves root-owned files under `data/` and the service's sweepers then fail every job (observed 2026-08-18 on the old VM); heal with `sudo chown -R onebit-demo:onebit-demo /srv/onebit-demo && sudo systemctl restart 1-bit-bridge-demo` |

**Network posture** is the host's, not the demo's: the VM's NSG (`1bitbridge-nsg`) opens 443/tcp to the internet and source-allowlists 22 and 7790; ufw allows everything it lists from anywhere and is not the gate (verified 2026-09-18). Nothing listens off-loopback for the demo itself. HTTP/3 does NOT reach it (HAProxy is a TCP passthrough; the old VM's 443/udp is gone) — same as bridge.ars.md, and the iOS client falls back to h2 by itself.

**systemd unit** (`/etc/systemd/system/1-bit-bridge-demo.service`; `Restart=always` because the console's Restart button exits 0):

```ini
[Unit]
Description=1-bit-bridge PUBLIC DEMO (bridge.1-bit.app)
After=network-online.target zfs-mount.service zfs.target haproxy.service
Wants=network-online.target
ConditionPathIsMountPoint=/srv/onebit-demo
ConditionPathExists=/srv/onebit-demo/bridge.yaml

[Service]
Type=simple
User=onebit-demo
Group=onebit-demo
WorkingDirectory=/srv/onebit-demo
ExecStart=/usr/local/bin/bridge-demo serve --config /srv/onebit-demo/bridge.yaml
Restart=always
RestartSec=3
KillSignal=SIGTERM
KillMode=mixed
TimeoutStopSec=45
CPUWeight=100
MemoryHigh=1024M
MemoryMax=2G
TasksMax=1024
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/srv/onebit-demo
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
UMask=0027
ProtectProc=invisible
ProtectHostname=yes
ProtectClock=yes
RemoveIPC=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
```

No `CAP_NET_BIND_SERVICE`: both listeners are loopback high ports; HAProxy owns :443.

**Config shape** (`/srv/onebit-demo/bridge.yaml`): public mode + autocert for `bridge.1-bit.app` (email `ars@ars.md`) with **`autocert.external443Mapping: true`** (the listener is `127.0.0.1:8446`, the world reaches it on :443 through the passthrough — without the flag public-mode validation refuses autocert off :443), `adminAddress: 127.0.0.1:7791`, `libraryName: "1-bit Demo Library"`, `analysis.enabled: true` (waveforms / loudness / DR / spectrum / key+tempo are half the demo), and the `demo:` block with `enabled: true` + the pinned `tokenSHA256` (`798007f038402931d0d34cd15284d864fb6fdb728a23740af61674d369b66506`). Host audio toolchain (`sox libsox-fmt-all ffmpeg`) is the host's. The pre-move config is kept beside it as `bridge.yaml.uksouth-20260916`.

**Known interim on the v0.1.9 artifact — one unreachable alternate endpoint.** `/v1/health.endpoints` reads `["https://bridge.1-bit.app", "https://bridge.1-bit.app:8446"]`: the tag's `publicModeEndpoints` appends `https://<autocert.domain>:<listen port>` whenever the listen port is not 443, and `:8446` is loopback-only here. PR #871 (`fix(api): don't advertise a listen port a proxy has remapped`, 2026-09-08) fixed exactly this on `main`, after the tag. Cost until the next release artifact: iOS puts the alternate into its failover rotation, which it consults only after the primary FAILS — so normal operation is unaffected and a demo outage merely reads as offline more slowly. A loopback `:443` listener would collapse the pair on the tag, and it is not available: HAProxy's wildcard `:::443` conflicts with any loopback `:443` bind on Linux (measured `EADDRINUSE`, 2026-09-16). **After the first release-artifact deploy that includes #871, verify `endpoints` reads exactly `["https://bridge.1-bit.app"]`** and delete this paragraph.

**Update per release** (this host is on the SAME cadence as the other production bridges — the iOS repo's release checklist points here). `.env.demo` (untracked) carries `HOST=arsenie@bridge.ars.md`, the VPS key, `REMOTE_BIN=/usr/local/bin/bridge-demo`, `SVC=1-bit-bridge-demo` and `HEALTH_URL=https://bridge.1-bit.app/v1/health`; the deploy script honours all of them:

```sh
ENV_FILE=deploy/linux/.env.demo ./deploy/linux/deploy-bridge-vps.sh
```

**Deploy this host from the RELEASE ARTIFACT, not from `main`** — it is the only
bridge where the version *string* is user-visible, and the deploy script cannot produce
the right one. The script builds whatever `git describe --tags` yields on `main`; one
commit past a tag that is `v0.1.9-1-g8f3a873`, which is valid semver where `-1-g8f3a873`
is a **prerelease** — and semver ranks a prerelease BELOW the plain release:

```
semverGreater("0.1.9", "0.1.9-1-g8f3a873") == true      # reports an update forever
semverGreater("0.1.9", "0.1.9")            == false
```

So the bridge sets `updateAvailable: true` permanently, and iOS is not passive about it:
`BridgeEditorView.probeResultSection` renders `bridgeUpdateAdvisoryRow` with **no demo
gate**, so a user opening the demo bridge in Settings is told *"Bridge update available ·
0.1.9"* about a server they do not own. (Observed 2026-08-26; the host had been shipping
that since 08-20, on a build whose only delta from the tag was a docs commit.)

Use the published artifact — it is what `latestServerVersion` names, it carries
goreleaser's clean `{{.Version}}` stamp (`0.1.9`, no `v`), and it is the binary users get:

```sh
# 1. Fetch + verify against the release's OWN manifest, then extract.
gh release download v0.1.9 -R acoseac/1-bit-bridge \
  -p '1-bit-bridge_0.1.9_linux_amd64.tar.gz' -p 'checksums.txt' -D /tmp/rel
(cd /tmp/rel && shasum -a 256 -c checksums.txt --ignore-missing && tar xzf *_linux_amd64.tar.gz)

# 2. Upload THAT binary -- /tmp/rel/bridge, NOT dist/bridge-linux-amd64. The
#    bridge.ars.md upload step above hardcodes the locally-built path, so
#    "then follow the flow above" would ship a main build and reintroduce the
#    exact version string this whole note exists to avoid.
scp -i <VPS-SSH-KEY> /tmp/rel/bridge arsenie@bridge.ars.md:/tmp/bridge.new
shasum -a 256 /tmp/rel/bridge                                          # local
ssh -i <VPS-SSH-KEY> arsenie@bridge.ars.md 'chmod +x /tmp/bridge.new
                                  sha256sum /tmp/bridge.new
                                  /tmp/bridge.new version'             # remote

# Gate on BOTH: the digests match, AND `version` prints a bare `0.1.9` with no
# -N-g<sha> suffix. The second is the one that catches having uploaded the
# wrong file, which a digest comparison against that same wrong file cannot.

# 3. Then the DETACHED swap -> setcap -> restart from the bridge.ars.md section,
#    with REMOTE_BIN=/usr/local/bin/bridge-demo and SVC=1-bit-bridge-demo -- it
#    operates on /tmp/bridge.new and never names the local path. `.env.demo`
#    carries both; NEVER let a demo swap touch /usr/local/bin/bridge, which is
#    the operator's own bridge on the same host.
```

Keep the detached dispatch: the window between the two `mv`s is the one state with no
binary at `/usr/local/bin/bridge-demo`. Confirm with `updateAvailable` reading **false**
afterwards — that is the signal the version string is clean, and no other check shows it.

The other two bridges track `main` deliberately and will keep reporting
`updateAvailable: true` between releases. That is correct there ("you are ahead of the
last tag") and needs no fix.

The script path (a `main` build) stays the right one for a hotfix that has no release tag. Verify afterwards:

```sh
curl -s https://bridge.1-bit.app/v1/health | jq '.serverVersion, .updateAvailable, .leCertNotAfter, .endpoints, (.features | index("demoMode") != null)'
# Expect: the new version, `false` (see the release-artifact note above), an LE expiry
# ~90d out, the single endpoint once #871 has shipped, and `true` (demoMode advertised).
```

**Demo content** is generated (Lyria 3 music + Gemini cover art, invented artists/albums — no licensing exposure) by `tools/demo-library/`; the catalog lives in `tools/demo-library/catalog.json`. To regenerate or extend: run the generator on the workstation, then

```sh
rsync -av --delete --rsync-path="sudo -u onebit-demo rsync" <out>/library/ arsenie@bridge.ars.md:/srv/onebit-demo/library/
```

(the tree is owned by the service user, so a plain rsync as `arsenie` cannot write it — the remote side runs AS `onebit-demo`, which lands every file with the right owner and needs no chown pass) and trigger a **Full rescan** (admin console via tunnel, or `sudo systemctl restart 1-bit-bridge-demo` — startup scans). Remember the standing doctrine: delta scans never delete, so removals need the full rescan.

**Upscaling + CarPlay-optimized variants on the demo bridge** are deliberately allowed (they showcase the features against the hosted lossless content): `upscale.enabled: true` + `autoOptimize` in the demo config. This is safe ONLY because demo mode 403s the four upscale MUTATION endpoints (`demo_read_only` — `POST /v1/upscale`, `POST /v1/upscale/batch`, `DELETE /v1/upscale/batches/{id}`, `DELETE /v1/upscale/variants`): every bearer on this host is effectively public, and an open batch endpoint would let anyone burn its CPU. The operator generates variants via the loopback admin console's Browse views (SSH tunnel), which don't route through those handlers. The `bridge upscale` / `bridge optimize` CLIs work too — **as the service user** (the coordinates table's CLI row); a root-run CLI creates `data/transcoded/` subdirs and DB/WAL siblings owned by root, and the service's auto-optimize sweeper then fails every job with `mkdir … permission denied` (observed 2026-08-18 — the sweep burned a whole pass silently).

**Atlas enrichment on the demo bridge:** `enrich.musicbrainzBaseURL` / `coverArtBaseURL` point at `https://atlas.ars.md` (keyless server-to-server, mirrors bridge.ars.md — kills public-MusicBrainz 503s); since the move Atlas is on the same host, so that name resolves to loopback via `/etc/hosts` and the requests never leave the box. Rich-tier bios (`atlas.enabled: true` + `harvestEnabled: true`) are safe here ONLY because demo mode 403s `POST /v1/atlas-ingest` (`demo_read_only`) — the client-content push a public bearer could poison; the bridge instead pulls bios itself via the harvest client, bootstrapped ONCE by the operator's attested 1-bit app (on the demo share in the app: set Atlas base URL `https://atlas.ars.md` + enable the harvest toggle; App Attest needs a real device, not the Simulator). Harvest state persists in `data/atlas-harvest.json` (carried over in the move); re-provision only when new content needs harvesting after the token TTL.

**Do-nots:** never wipe `/srv/onebit-demo/data/acme/` (LE duplicate-cert rate limit — the cache moved with the data, so the move cost no issuance); never rotate `demo.tokenSHA256` outside an iOS release cycle; never flip `demo.enabled` off; never bind the admin console off-loopback here (nobody but the operator ever needs it); never point a demo deploy at `/usr/local/bin/bridge` or `1-bit-bridge` (those are the operator's bridge on the same host).

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

**Connection-issues hint** (re-stated for the post-merge loop): SSH and the admin port `7790` are allowlisted to the operator's ranges (the Azure NSG since 2026-09-16; ufw before). A residential CGNAT rotation or VPN flip can make those fail while `/v1/health` over :443 still works — that's a whitelist class of issue, NOT a bridge bug. Add the new source range to the NSG rules — ufw allows those ports from anywhere and is not the gate.

### Step 4 — demo bridge (`bridge.1-bit.app`)

Public read-only demo bridge (the iOS "Add demo bridge" target). Full coordinates + rationale live in the `Demo bridge` subsection under "Production deployments" above.

**One-line summary**: deploy the RELEASE ARTIFACT rather than a `main` build (see the `Demo bridge` subsection for why — a `git describe` version string makes this host advertise `updateAvailable: true` forever, which iOS shows to demo users), then `curl -s https://bridge.1-bit.app/v1/health | jq '.serverVersion, .updateAvailable, (.features | index("demoMode") != null)'` — expect the new version, `false`, and `true`. If `demoMode` ever reads `false` on this host, STOP and fix the config before anything else: the read-only posture is what the iOS app's "collects nothing" demo claim rests on.

**SSH-flap note (2026-08-17): it is not the key, and it is not necessarily all-or-nothing.**

- **Tell the classes apart by WHERE it fails.** `Operation timed out` on `:22` is a TCP-connect failure — it happens before any key is offered, so *no key can fix it*. A wrong key gives `Permission denied (publickey)` over an established connection instead. Confirm by probing ports: `:443` answering while `:22` is filtered on the same resolved IP is the allowlist.
- **It can flap within seconds, and per-port.** Back-to-back probes gave timeout, then open, then twelve consecutive failures — while `:7789` stayed reachable throughout from the same workstation. So a single failed `ssh` proves nothing; probe twice before concluding anything, and re-run the deploy script (it is idempotent, and the SHA gate means a partial upload can never replace a working binary).
- **A LAN host with its own allowlisted egress works as a pure TCP relay:** `ssh -J <RELAY-SSH> -i ~/.ssh/<VPS-SSH-KEY> <VPS-SSH>`. `-J` tunnels only TCP, so the key never leaves the workstation and no binary transits the relay — strictly better than copying either. To make the deploy script take that route, set **`SSH_OPTS="-J <RELAY-SSH>"`** (in `deploy/linux/.env` or inline). **Not `HOST`** — `HOST` is passed as ssh's target argument, so a ProxyJump form there cannot work; an earlier revision of this note said otherwise. An option whose *value* contains spaces (a full `ProxyCommand`) needs `~/.ssh/config` instead, since `SSH_OPTS` is word-split.
- **Verification never needs SSH.** `/v1/health` on `:443` is open to everyone, so poll `serverVersion` there to confirm a deploy landed — which is what the script now does.


## Release deploy ledger

One row per deploy that changed the config surface or carries checks that must
be run afterwards. **Read the row before deploying**: the "config keys" column
is the rollback hazard (the loader is strict — an older binary meeting a newer
key crash-loops; see the rollback note under bridge.ars.md), and the "after"
column is the list of things a deploy is not finished until someone has looked
at. A check that only lives in a to-do note on one machine is a check nobody
runs; that is how the #849–#851 verification sat unrun for four days on a
bridge that already had the fix.

| Version | Date | Hosts | Config keys added since the previous row | After the restart |
|---|---|---|---|---|
| `v0.1.9-154` | 2026-09-08 | bridge.ars.md | `upscale.dsdRender.*`, `upscale.tempDir` (#863) | `bridge doctor` → `dsd-render-toolchain` ok; `/v1/health` advertises `dsdRender`; `upscale.tempDir` set explicitly (PrivateTmp) |
| `v0.2.0` | 2026-09-18 | bridge.ars.md (operator) → the three hosted tenants → demo, all from the RELEASE ARTIFACT (`1-bit-bridge_0.2.0_linux_amd64.tar.gz`, checksum-verified); **home-pc not deployed** | `atlas.lyricsEnabled` (#887), `deployment.managedControls` (#876). Both `omitempty`, both default off; neither was written on any host, so the one-step binary rollback applies: `bridge.old-20260918-114618`, `bridge-demo.old-20260918-114647`, tenants `releases/v0.1.9-216-g393a47e` (flip `current`, restart `bridge@*`). | Items 1 and 7 done on the day: all five endpoints `0.2.0` with `lyrics`, `dsdRender` on bridge.ars.md only, `dlnaArtwork` absent, `demoMode` true on the demo, `bridge doctor` exit 0 on the live config. Item 2: NO re-extraction — every deployed host was on `-216`, which already carried `ExtractorVersion` 10 (8 → 10 landed with `-207`), and every row on all five live DBs is stamped 10 (measured 12:58Z), so no version-stale re-extraction was required — which is all the histogram proves; see item 2. Items 3–5 OPEN (the steady-state checks, per unit; 4's wall-clock is the #850 detector, 3's grep only counts extraction errors). Item 6 is the next nightly fuzz run; item 8 DONE (the note is marked). |
| `v0.2.0-39-gdc2e171` | 2026-09-22 | **`<OPERATOR-SSH>` (the NUC) ONLY** — first deploy to the host that replaced `bridge.ars.md`. Demo, tenants and home-pc NOT deployed; they remain on `v0.2.0`. | **NONE.** The window added one config field (`integrity.variantSweepMaxDeletePercent`, #940) but it is `omitempty` with a default and was not written here, so the one-step binary rollback applies: `sudo mv /usr/local/bin/bridge.old-20260922-144244 /usr/local/bin/bridge && sudo setcap cap_net_bind_service=+ep /usr/local/bin/bridge && sudo systemctl restart 1-bit-bridge` — the `setcap` is not optional, the swap drops the file capability and this unit binds `:443` as an unprivileged user. | Carries #934–#966: the post-v0.2.0 window plus its review. **`ExtractorVersion` 10 → 13**, so the startup scan re-extracted all 21,460 tracks — **completed in under 2 minutes** on the local USB SSD, against the hours a B2-mounted host needs. Verified on the day: health `v0.2.0-39-gdc2e171`, **cert fingerprint UNCHANGED** (`AB:1B:79:…:76:3E` — iOS pins survive, no re-pairing), `bridge doctor` 14 ok / 2 warn / 0 fail on the live config (both warns benign: the `:443` bind probe cannot elevate, and no browser opener on a headless box), `variants-index` 10248 rows / 10248 files all referenced, `sidecar-paths` clean, 0 restarts. **#947's debounce fired on its first run and found 29 genuinely truncated FLACs** (`tracksUnreadable: 29`, threshold 3, suppressed 0 — they are on strike one); they are listed by `GET /api/analysis/unreadable`, by the Jobs page and now by `bridge status`, and they are files to REPLACE, not a bridge fault. Re-check that they reach `suppressed: 29` rather than being retried forever. |
| `v0.2.0-58-gaca8be3` | 2026-09-23 | **`<OPERATOR-SSH>` (the NUC) ONLY.** Demo, tenants and home-pc NOT deployed; they remain on `v0.2.0`. | **NONE.** No config key and no migration in the window — verified by diffing `internal/config/` and `internal/manifest/` across `dc2e171..main`, not assumed. One-step binary rollback: `sudo mv /usr/local/bin/bridge.old-20260923-065952 /usr/local/bin/bridge && sudo setcap cap_net_bind_service=+ep /usr/local/bin/bridge && sudo systemctl restart 1-bit-bridge` — the `setcap` is not optional. | Carries #967–#975 plus ten dependency bumps (`modernc.org/sqlite` 1.56.0 → 1.59.0 is the notable one; it moves the embedded engine 3.53.3 → 3.53.4, and the v46 partial-index plan assertion still passes on it). **`ExtractorVersion` 13 → 14**, so the startup scan re-extracted all 21,460 tracks — **completed in ~70 s**, consistent with the previous row. Verified on the day: health `v0.2.0-58-gaca8be3`, **cert fingerprint UNCHANGED** (`AB:1B:79:…:76:3E`), track count unchanged at 21,460, `bridge doctor` **15 ok / 1 warn / 0 fail** exit 0 (one better than the previous row — `port-api` / `port-admin` now grade the capability-bound binary as ok rather than warning, #970), `variants-index` 10248 rows / 10248 files all referenced, `sidecar-paths` clean, 0 restarts. Only ERROR lines were three transient MusicBrainz 503s on one album, which the enricher is required NOT to cache. **The previous row's open item is CLOSED: the truncated FLACs reached `suppressed: 30` of 30** (threshold 3), so #947's debounce is doing what it exists to do — they are still files to REPLACE. ⚠️ **`/tmp` on this host is a tmpfs** (31 GB, RAM-backed) — the opposite of `bridge.ars.md`, whose note says to verify this per host. `upscale.tempDir` is explicitly `/home/arsenie/bridge-data/tmp` (on disk), so the multi-GB DSD render scratch cannot pressure RAM; do not clear that setting here. |

### `v0.2.0` post-deploy checklist

Run on **each** host, in order. The lyrics items are the #849 / #850 / #851
verification that was written on 2026-09-06 and never run — those PRs have
been live on bridge.ars.md since the `-154` deploy, so the numbers are
measurable today, and the v0.2.0 restart is the first scan cycle anyone will
be watching.

**Scope: the five DEPLOYED endpoints** — bridge.ars.md (`1-bit-bridge`), the
three hosted tenants (`bridge@demo`, `bridge@t26f0939137`, `bridge@t0ae4e78fa1`)
and the public demo (`1-bit-bridge-demo`). **home-pc is NOT on v0.2.0** — the
standing default is never to deploy it without an explicit ask — and is outside
this checklist; when it is brought forward, items 1–2 apply to it afresh and
item 2's re-extraction expectation is live there.

1. **Version and flags.** `curl -s https://<host>/v1/health | jq '.serverVersion, .features'` —
   expect `0.2.0` (bare, on the release artifact) and `lyrics` present;
   `dsdRender` present on a host with the ffmpeg `dsd_*` decoders; `dlnaArtwork`
   ABSENT on bridge.ars.md and the demo (public mode never starts the DLNA
   listener) and present on home-pc if `dlna.enabled` is on there.
2. **Version-stale re-extraction: NONE on this deploy — measured, not inferred.** Every
   deployed host was on `-216` before 0.2.0, and `-216` already carried
   `ExtractorVersion` 10 (7 → 8 went live with `-191` on 2026-09-10, 8 → 10
   with `-207` on 2026-09-15; `-216` and `v0.2.0` are both 10). The persisted
   stamps agree — `SELECT extractor_version, COUNT(*) FROM tracks GROUP BY 1`,
   read-only on each live DB at 12:58Z: bridge.ars.md `10 × 21,460`, tenants
   `10 × 1` / `10 × 4` / `10 × 1`, demo `10 × 183` — no row below 10, so the
   skip gate (`existing.ExtractorVersion >= ExtractorVersion`) had no
   version-stale leg to take. That is all the histogram proves: the same
   `reExtractUnchanged` path also serves a current-version row whose sidecar
   lyrics drifted or whose local artwork needs recovering, so "re-read
   nothing" is not a claim it can carry. The scan metric beside it: the
   operator bridge's startup scan reached its duplicate-stamping tail eleven
   minutes after the restart (11:46:18 → 11:57:57). This item expected 7 → 10 here because it was written against
   `-154`; it applies to a host brought forward from before `-207` (home-pc —
   see the scope note), where the startup scan re-reads every audio file
   once: expect `scanState.isScanning: true` for a long time on a mounted
   library, and `tracksIndexed` back at the pre-deploy value when it ends. A
   count that dropped means suppression or reaping changed something and
   wants the Duplicates page before anything else.
3. **#850, steady state — OPEN, and the journal grep is NOT the detector.**
   The scanner logs nothing per re-read: `re-extract (version-stale)` is an
   ERROR line (an extraction that FAILED on the version-stale leg), so
   `journalctl -u <unit> --since '<restart>' --no-pager | grep -c 're-extract'`
   — `<unit>` is `1-bit-bridge` on bridge.ars.md, `bridge@<tenant>` on a
   hosted tenant, `1-bit-bridge-demo` on the demo; the wrong unit reads as a
   clean zero — counts failures, and its zero on all five units (2026-09-18)
   says only that nothing failed. The #850 class (`reExtractUnchanged` →
   `versionStampOnly`) leaves no log line and no `indexed_at` churn; what
   shows it is item 4's wall-clock, and after a version bump the stamp
   histogram in item 2 (every row at the current version = the version-stale
   leg is spent). Run the grep anyway — a non-zero is a real extraction error
   on a file the phone will now miss.
4. **Scan wall-clock, cycle over cycle — OPEN.** From the journal's scan
   start/finish lines on two consecutive periodic scans, same unit rule as
   3. This is the #850 detector; the number to record is the steady state.
5. **#849, silent by nature — OPEN.** Add a `.lrc` beside a track on the
   library root, trigger a scan (`POST /api/scan` on a loopback console; a
   restart on a public one), and confirm the phone receives the lyrics on a
   DELTA sync — `/v1/manifest?since=` carries the track with a new
   `lyricsTag` — not only on a full sync. Remove the file afterwards and
   confirm the row goes.
6. **Nightly fuzz.** The next `fuzz.yml` run is green and the matrix shows
   **39** targets — the list is discovered from the tree, so the count is the
   check that the new `internal/lyrics` target was picked up.
7. **Config keys.** `bridge doctor --config <path>` exits 0 BEFORE any edit
   that adds `atlas.lyricsEnabled` or `deployment.managedControls`; the
   rollback note applies from the moment either is written.
8. **Mark the verify note folded — DONE 2026-09-18.** `~/Desktop/to-do/2026-09-06-loupe-lyrics-verify.md`
   is superseded by items 3–6 here and says so at the top.

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

**The v0.1.8 → v0.1.9 upgrade runs TWO one-time backfills, and they compound.**
`manifest.ExtractorVersion` did **not exist at v0.1.8** — it was introduced
within this release cycle (#586) and is now `4` — so the stamp reads 0 on every
pre-existing row and the first scan **re-extracts tags for the entire library**.
Independently, `analyze.WaveformSchemaVersion` moved **wf3 → wf7**, so every
analysed track is **re-decoded** to backfill the spectrum and the track-quality
scalars. Budget accordingly: the wf3→wf4 pass alone ran ~18 h at ~85 % CPU on
bridge.ars.md, and this one is wider. Two things make it survivable rather than
alarming: #606's version-stale diff-guard means re-extracted rows whose metadata
did not actually change are stamped WITHOUT an `indexed_at` bump, so the client
delta stays bounded; and no waveform sidecar is re-fetched (only the spectrum
and `bandwidthHz` are new on the wire). Expect the busy-defer below for the
duration, and don't read a long first scan on 0.1.9 as a regression.

**Also expect the served track count to DROP on 0.1.9** — `duplicates.filter`
defaults to `highest-quality`, and `/v1/health`'s `tracksIndexed` plus the
manifest's `total` now describe the served set. Nothing was deleted; the admin
Library → Duplicates page shows every group and its winner.


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
