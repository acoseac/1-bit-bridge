# 1-bit-bridge

Companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Runs on your home server (Windows / Linux / macOS / NAS) and exposes your music library over HTTPS + bearer-token auth, with a pre-built manifest endpoint so the iOS app can sync its whole library in a single request instead of walking a slow SMB tree.

## Why

1-bit's existing SMB transport is great on LAN but painful over Tailscale / similar overlays: SMB's chatty pread pattern multiplies every round-trip, and DERP-relayed connections can push latency past 3 seconds. 1-bit-bridge replaces SMB with an HTTP/2 protocol purpose-built for the app:

- **5 primitives** matching the iOS app's internal `SMBConnectionPool` API (`list`, `stat`, `readRange`, `download`, `downloadStreaming`), so the existing scanner + playback code paths are unchanged.
- **Pre-built library manifest** — the bridge keeps a SQLite-backed index; iOS fetches the full library (or a `?since=<mtime>` delta) in one call. Both scan phases (walk + enrich) are replaced by a single HTTP GET.
- **Bit-exact preserved** — the bridge delivers bytes; iOS still pre-caches the full DSF file before playback, protecting the Hugo 2 DoP lock. The bridge never transcodes.

## Status

v0 — scaffolding only. See [issues](https://github.com/acoseac/1-bit-bridge/issues) for tracking. The wire protocol is frozen at [`PROTOCOL.md`](PROTOCOL.md) v1.

## Build

Requires Go 1.23+.

```sh
make build          # builds ./bin/bridge for the host OS
make build-all      # builds dist/bridge-<os>-<arch>{.exe} for win/linux/darwin × amd64/arm64
make test           # unit tests
```

## Run

```sh
cp config/bridge.yaml.example bridge.yaml
# edit bridge.yaml to point libraryRoots at your music folder
./bin/bridge serve --config bridge.yaml
```

Then pair an iOS client:

```sh
./bin/bridge pair --name "iPhone 15 Pro"
# prints a bearer token + QR code; paste or scan into the 1-bit app.
```

## Protocol

See [`PROTOCOL.md`](PROTOCOL.md). Every endpoint is prefixed `/v1/`.

## License

MIT. See [`LICENSE`](LICENSE).
