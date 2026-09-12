# 1-bit-bridge

[![Latest release](https://img.shields.io/github/v/release/acoseac/1-bit-bridge?sort=semver&display_name=tag&label=release)](https://github.com/acoseac/1-bit-bridge/releases/latest)
[![License: FSL-1.1-MIT](https://img.shields.io/badge/license-FSL--1.1--MIT-blue)](LICENSE)
[![Go version](https://img.shields.io/github/go-mod/go-version/acoseac/1-bit-bridge)](go.mod)
[![iOS app on App Store](https://img.shields.io/badge/iOS%20app-1--bit%20on%20App%20Store-0A84FF?logo=apple&logoColor=white)](https://apps.apple.com/us/app/1-bit/id6762529497)

Companion server for the **[1-bit iOS music player](https://apps.apple.com/us/app/1-bit/id6762529497)** — bit-exact playback of your own music library, including DSD, from anywhere. The bridge runs on a home server (Windows / Linux / macOS / NAS) and exposes your library over HTTPS + bearer-token auth with a pre-built manifest endpoint, so the iOS app syncs its whole library in a single request instead of walking a slow SMB tree.

👉 **[1-bit.app/bridge](https://1-bit.app/bridge/)** for the landing page, setup walkthrough, feature reference, troubleshooting, and privacy policy.

## Why

1-bit's existing SMB transport is great on LAN but painful over Tailscale / similar overlays: SMB's chatty pread pattern multiplies every round-trip, and DERP-relayed connections can push latency past 3 seconds. 1-bit-bridge replaces SMB with an HTTP/2 protocol purpose-built for the app:

- **5 primitives** matching the iOS app's internal `SMBConnectionPool` API (`list`, `stat`, `readRange`, `download`, `downloadStreaming`), so the existing scanner + playback code paths are unchanged.
- **Pre-built library manifest** — the bridge keeps a SQLite-backed index; iOS fetches the full library (or a `?since=<mtime>` delta) in one call. Both scan phases (walk + enrich) are replaced by a single HTTP GET.
- **Bit-exact preserved** — the bridge delivers bytes; iOS still pre-caches the full DSF file before playback, protecting the Hugo 2 DoP lock. The bridge never transcodes.

## Status

**v0.2.0** — wire protocol stays at [`PROTOCOL.md`](PROTOCOL.md) v1 (additive only since v0.1.7); the first release under [FSL-1.1-MIT](LICENSE) — v0.1.9 and earlier remain MIT. Headline: **the console becomes a player, and covers reach every screen.** The admin console is now a full library player — albums, artists, genres, composers, playlists and smart mixes, an A–Z rail, a now-playing bar that survives navigation to the operator pages — with **web upload** (chunked, resumable, content-verified) and **delete-as-trash** behind their own switches. **DSD → PCM renditions** join the offline variant families: a clip-guarded two-stage ffmpeg + sox render into `pcm-` (faithful, 176.4/192 kHz 24-bit) and `optimized-dsd-` (compact) sidecars, measured rather than assumed — the decimation stopband reads **−116.8 dB**, the alias differential **+0.00 dB** faithful / **+0.38 dB** compact, the rendition lands at **−0.97 dBTP** against its −1.0 target — and the measurements now run in CI. **Lyrics**: the bridge extracts, stores and serves **one lyrics document per track** (`GET /v1/lyrics`) from ID3 SYLT/USLT, Vorbis, MP4 `©lyr` and `.lrc`/`.ttml`/`.txt` sidecars, with an opt-in **network tier** that fills the gaps from a self-hosted Atlas relaying LRCLIB; a transcript with one cue marker is plain text, not a synced stub, the same way the app reads it. **Covers for DLNA renderers**: `/dlna/artwork/{key}` serves `/v1/artwork`'s bytes on the LAN-only listener and the ContentDirectory finally sets `albumArtURI` (feature flag `dlnaArtwork`). **SACD ISO** images expand into virtual DST tracks; **ALAC** can be upscaled through an ffmpeg pipe; `GET /v1/search` puts the FTS5 index on the wire; artist portraits are content-keyed and cache immutable. Operationally: **one-time console login links** that survive a phone-to-laptop hand-off, a **hosted-tenant posture** (managed settings and controls the tenant does not own are hidden and refused), a data-portability **export** of everything a person made, database **compaction and retention** windows, per-route **rate-limit classes**, `/healthz` + `/readyz`, JSON logs off-terminal, HSTS in public mode, and settings that **hot-apply** — cadences, feature gates, enrichment endpoints, upscale and analysis take effect on save, and the PATCH response says per field what did (the five listener- and boot-bound ones say `restart`, honestly). Under the hood this is **161 PRs**: three review sweeps (a LOUPE on the CLI, a LOUPE on the 09-04..09 window, a full-codebase review), **39 fuzz targets across ten packages** fuzzed nightly in CI, the Windows and macOS test legs promoted into the merge gate, and the CLAUDE.md invariants split from their evidence into an engineering log. macOS binaries are Developer-ID-signed and Apple-notarized; Windows binaries are unsigned for now (Authenticode signing via SignPath Foundation pending). See the [releases page](https://github.com/acoseac/1-bit-bridge/releases) for per-release notes.

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

**Admin console** — [http://127.0.0.1:7789/](http://127.0.0.1:7789/). Add/remove library folders, pair iOS devices (QR + copy-buttons), revoke tokens, view scan state + stats, and run the preflight checks + read operational counters from the **Diagnostics** page. Loopback-only, no auth — anyone on the machine already has filesystem access to the token store. (For internet-exposed deployments, see `bridge init --public` in [the public-deployment docs](https://1-bit.app/bridge/features/#public-mode) — that profile binds the admin console to a routable interface and adds a single-user password login.)

**Pairing an iOS device**: on the Devices page, click _Pair new device_, give it a name, optionally edit the bridge URL (defaults to `https://<hostname>.local:7788`), then generate the token. The modal shows a QR encoding a `bridge://pair?...` URL — scan it in the 1-bit app, or copy the URL/token/fingerprint fields manually.

**Metrics** — the admin listener also serves Prometheus text at [http://127.0.0.1:7789/metrics](http://127.0.0.1:7789/metrics): SQLite lock waits, enrichment cache hit rates, conversion-job durations, log-event counts. Loopback-only, like the rest of the console, and always on. The Diagnostics page shows the current values; point a scraper at `/metrics` if you want history.

## Build from source

Requires Go 1.26+.

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

[FSL-1.1-MIT](LICENSE) — the Functional Source License, with an automatic MIT future grant. In plain language (the [`LICENSE`](LICENSE) text governs):

- **Self-hosting stays free, for everyone.** Running the bridge for your own library, personal and internal use, modification, and redistribution are all permitted — nothing changes for operators.
- The restricted use is a **Competing Use** as defined in the license: making the bridge available to others in a commercial product or service that substitutes for it (or for a product or service offered with it), or that offers substantially similar functionality — for example shipping it as another app's companion server, or selling it as a hosted service.
- **Each version automatically becomes available under MIT on the second anniversary of the date that version is made available** (the Future License grant).
- Releases up to and including **v0.1.9** were published under MIT and remain MIT.

The wire protocol documentation ([`PROTOCOL.md`](PROTOCOL.md)) remains public.
