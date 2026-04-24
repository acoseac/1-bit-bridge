# 1-bit-bridge

Companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Runs on your home server (Windows / Linux / macOS / NAS) and exposes your music library over HTTPS + bearer-token auth, with a pre-built manifest endpoint so the iOS app can sync its whole library in a single request instead of walking a slow SMB tree.

## Why

1-bit's existing SMB transport is great on LAN but painful over Tailscale / similar overlays: SMB's chatty pread pattern multiplies every round-trip, and DERP-relayed connections can push latency past 3 seconds. 1-bit-bridge replaces SMB with an HTTP/2 protocol purpose-built for the app:

- **5 primitives** matching the iOS app's internal `SMBConnectionPool` API (`list`, `stat`, `readRange`, `download`, `downloadStreaming`), so the existing scanner + playback code paths are unchanged.
- **Pre-built library manifest** — the bridge keeps a SQLite-backed index; iOS fetches the full library (or a `?since=<mtime>` delta) in one call. Both scan phases (walk + enrich) are replaced by a single HTTP GET.
- **Bit-exact preserved** — the bridge delivers bytes; iOS still pre-caches the full DSF file before playback, protecting the Hugo 2 DoP lock. The bridge never transcodes.

## Status

v0.0.x — wire protocol frozen at [`PROTOCOL.md`](PROTOCOL.md) v1. Admin console + one-command install landed; see [CHANGELOG](https://github.com/acoseac/1-bit-bridge/releases) for per-release notes.

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

On Windows the bridge installs as a **Startup-folder launcher** — it starts at logon and keeps running while you're logged in. Close-on-logout; a full Windows Service install (survives logout, runs at boot) is tracked as a follow-up.

Config lands under `%LOCALAPPDATA%\1-bit-bridge\`; the launcher lives at `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\com.acoseac.1-bit-bridge.cmd`.

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
make build-all      # cross-compiles darwin/linux × amd64/arm64 into dist/
make test           # unit tests
```

For a local release dry-run: `goreleaser release --snapshot --clean`.

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
