# 1-bit-bridge

[![Latest release](https://img.shields.io/github/v/release/acoseac/1-bit-bridge?sort=semver&display_name=tag&label=release)](https://github.com/acoseac/1-bit-bridge/releases/latest)
[![License: MIT](https://img.shields.io/github/license/acoseac/1-bit-bridge)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/acoseac/1-bit-bridge)](go.mod)
[![iOS app on App Store](https://img.shields.io/badge/iOS%20app-1--bit%20on%20App%20Store-0A84FF?logo=apple&logoColor=white)](https://apps.apple.com/us/app/1-bit/id6762529497)

Companion server for the **[1-bit iOS music player](https://apps.apple.com/us/app/1-bit/id6762529497)** — bit-exact playback of your own music library, including DSD, from anywhere. The bridge runs on a home server (Windows / Linux / macOS / NAS) and exposes your library over HTTPS + bearer-token auth with a pre-built manifest endpoint, so the iOS app syncs its whole library in a single request instead of walking a slow SMB tree.

👉 **[acoseac.github.io/1-bit-bridge](https://acoseac.github.io/1-bit-bridge/)** for the landing page with install walk-through.

## Why

1-bit's existing SMB transport is great on LAN but painful over Tailscale / similar overlays: SMB's chatty pread pattern multiplies every round-trip, and DERP-relayed connections can push latency past 3 seconds. 1-bit-bridge replaces SMB with an HTTP/2 protocol purpose-built for the app:

- **5 primitives** matching the iOS app's internal `SMBConnectionPool` API (`list`, `stat`, `readRange`, `download`, `downloadStreaming`), so the existing scanner + playback code paths are unchanged.
- **Pre-built library manifest** — the bridge keeps a SQLite-backed index; iOS fetches the full library (or a `?since=<mtime>` delta) in one call. Both scan phases (walk + enrich) are replaced by a single HTTP GET.
- **Bit-exact preserved** — the bridge delivers bytes; iOS still pre-caches the full DSF file before playback, protecting the Hugo 2 DoP lock. The bridge never transcodes.

## Status

**v0.1.3** — wire protocol stays at [`PROTOCOL.md`](PROTOCOL.md) v1 (additive only since v0.1.2: classical-metadata fields on `TrackEntry`, `pushEventsSupported` / `pairingEventsSupported` on `/v1/health`, per-root structured reachability, `upscale.complete` SSE). macOS binaries are Developer-ID-signed and Apple-notarized; Windows binaries are unsigned for now (Authenticode signing via SignPath Foundation pending). See the [releases page](https://github.com/acoseac/1-bit-bridge/releases) for per-release notes.

## Install

Grab a pre-built binary from the [releases page](https://github.com/acoseac/1-bit-bridge/releases) — no Go toolchain required. Pick your OS + arch, then run `bridge init`.

**macOS / Linux:**

```sh
tar -xzf 1-bit-bridge_*_macos_arm64.tar.gz   # or linux_amd64 / linux_arm64 / macos_amd64
./bridge init
```

Writes config + TLS cert under `~/Library/Application Support/1-bit-bridge/` (macOS) or `$XDG_CONFIG_HOME/1-bit-bridge/` (Linux), registers a launchd user agent / systemd user unit, opens the admin console.

**Windows** (PowerShell or File Explorer):

```powershell
# Unzip 1-bit-bridge_*_windows_amd64.zip (or _arm64)
bridge.exe init
```

Two install paths on Windows:

- **Startup-folder launcher (default)** — `bridge init`. Per-user, runs at logon, stops on logout. No admin required. Launcher lives at `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\com.acoseac.1-bit-bridge.cmd`; config under `%LOCALAPPDATA%\1-bit-bridge\`.
- **Windows Service (survives logout)** — `bridge init --service`, run from an **elevated PowerShell**. Installs an SCM service that starts at boot, runs as LocalSystem, logs to `%PROGRAMDATA%\1-bit-bridge\bridge.log`. Because the service runs as LocalSystem, point `--library` at a machine-wide path (e.g. `C:\Music`) rather than `%USERPROFILE%\Music`.

Uninstall: run `bridge init` again and answer "no" to overwrite (Startup launcher), or `sc.exe delete 1-bit-bridge` from an admin shell (Windows Service).

### Preflight: `bridge doctor`

Before (or any time after) running `bridge init`, `bridge doctor` prints a punch list of what's wrong in your environment:

```
[ok]   platform            darwin/arm64
[ok]   config-dir          /Users/me/Library/Application Support/1-bit-bridge
[ok]   tls-cert            present
[FAIL] port-api            :7788 in use
  ↳ another process owns this port; stop it or pick a different address in bridge.yaml
[ok]   port-admin          free (:7789)
[ok]   library-roots       1 root(s) reachable
[ok]   service-manager     launchctl available
[ok]   browser-opener      open
```

`bridge init` runs doctor automatically and bails on `fail` (use `--skip-doctor` to override — not recommended).

**Admin console** — [http://127.0.0.1:7789/](http://127.0.0.1:7789/). Add/remove library folders, pair iOS devices (QR + copy-buttons), revoke tokens, view scan state + stats. Loopback-only, no auth — anyone on the machine already has filesystem access to the token store.

**Pairing an iOS device**: on the Devices page, click _Pair new device_, give it a name, optionally edit the bridge URL (defaults to `https://<hostname>.local:7788`), then generate the token. The modal shows a QR encoding a `bridge://pair?...` URL — scan it in the 1-bit app, or copy the URL/token/fingerprint fields manually.

## Build from source

Requires Go 1.25+.

```sh
make build          # builds ./bin/bridge for the host OS
make build-all      # cross-compiles darwin/linux/windows × amd64/arm64 into dist/
make test           # unit tests
```

For a local release dry-run: `goreleaser release --snapshot --clean`.

## Contributing

PRs welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the branch / PR conventions, the pre-push checklist, and the **mirror-PR rule** that applies whenever a change touches the wire protocol shared with the [1-bit iOS app](https://github.com/acoseac/1-bit).

## Security

Found a vulnerability? Please report privately via GitHub's [Security Advisories](https://github.com/acoseac/1-bit-bridge/security/advisories/new) — see [`SECURITY.md`](SECURITY.md) for scope, response SLA, and what to include in your report.

## Tailscale modes

The bridge offers three Tailscale integration modes via the `tailscale.mode` key in `bridge.yaml`:

- **`cli`** *(default)* — shells out to the host's installed `tailscale` binary for status detection and Let's Encrypt cert provisioning on `*.ts.net` connections. Requires Tailscale to be installed and running on the host.
- **`tsnet`** — embeds a tailnet node directly in the bridge process via `tailscale.com/tsnet`. No Tailscale binary required; LE cert is renewed in-process. Authenticate with `bridge tsnet auth` on first run, or set `tailscale.authKey` for unattended deployments.
- **`disabled`** — turns off both the CLI auto-pilot and the embedded tsnet node. Use this for LAN-only deployments. The admin tile renders a one-line explanation so operators who flipped to `disabled` accidentally can recover.

Mode changes require a bridge restart. The admin tile shows the current Tailscale state and any errors from the active path.

## Manual run (without `bridge init`)

For anyone who wants to skip the service install or run the bridge out of an arbitrary directory:

```sh
./bridge init --no-service --yes --library /path/to/music --dir ./bridge-data
./bridge serve --config ./bridge-data/bridge.yaml
```

## Protocol

See [`PROTOCOL.md`](PROTOCOL.md). Every client endpoint is prefixed `/v1/`. The admin console (`/`, `/api/*`, `/static/*`) lives on a separate loopback listener and is **not** part of the wire protocol — iOS never talks to it.

## License

MIT. See [`LICENSE`](LICENSE).
