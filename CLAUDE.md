# 1-bit-bridge

Cross-platform Go companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Replaces SMB as the transport over high-latency links (Tailscale / DERP relay) — HTTP/2 + bearer-token auth, TLS fingerprint pinning, pre-built library manifest that skips the iOS scanner's two-phase walk. Target platforms: macOS / Linux / Windows × amd64 / arm64. Tested primarily on macOS.

## Build

- `make build` — builds `./bin/bridge` for the host OS.
- `make build-all` — cross-compiles to `dist/bridge-<os>-<arch>{.exe}`.
- `make test` — pure-Go race-enabled suite; ~150 tests across 10 packages.
- `make fmt vet test build-all` is the pre-push gate (CI is disabled while the private-repo Actions budget is blocked; see `CONTRIBUTING.md`).
- Pure-Go stack: `modernc.org/sqlite` (no cgo), `github.com/mewkiz/flac`, `github.com/dhowden/tag`, `github.com/hashicorp/mdns`. One static binary, no runtime deps.

## Architecture at a glance

| Package | Role |
|---|---|
| `cmd/bridge` | CLI: `serve` / `pair` / `scan` / `version` |
| `internal/config` | YAML loader with defaults + path-relative resolution |
| `internal/tls` | Self-signed ECDSA P-256 cert minter, SHA-256 fingerprint for iOS pinning |
| `internal/auth` | Bearer-token store (hashed, atomic persist, cross-process pickup) |
| `internal/fs` | Path-safe resolver (traversal-rejection, multi-root routing) |
| `internal/manifest` | SQLite library index + tag extractors (FLAC / DSF / MP3 / M4A) + JSON serializer |
| `internal/enrich` | MusicBrainz + Cover Art Archive + Deezer clients, rate-limited (1.1s / 500ms / 120ms) |
| `internal/api` | HTTP/2 handlers: `/v1/{health,list,stat,read,download,manifest,artwork/{mbid},artist-image/{mbid}}` |
| `internal/mdns` | Bonjour `_onebit-bridge._tcp` advertisement |
| `internal/version` | `ServerVersion` + `ProtocolVersion` constants (source of truth for `PROTOCOL.md`) |

## iOS companion (coupled repo)

The iOS app **1-bit** lives at `github.com/acoseac/1-bit` with a local clone at `~/Desktop/com.acoseac.dsdplayer/`. It consumes this server via `BridgeSourceClient.swift` + `LibraryScanner.runBridgeSync` + the artwork prefetch. The wire protocol spec is co-committed as `PROTOCOL.md` here and `docs/BridgeProtocol.md` in the iOS repo — they must stay byte-identical (see `CONTRIBUTING.md` → **Mirror-PR rule**).

**When editing this repo, also check the iOS app:**

- **Wire-protocol change** (`/v1/*` response shape, `BridgeTrack` / `BridgeManifest` JSON, `X-Bridge-Protocol` header semantics, error-envelope codes): the iOS `BridgeSourceClient.swift` DTOs + `Tests/…/Fixtures/Bridge/manifest_basic.json` golden fixture MUST be updated in the same PR pair. Bump `internal/version.ProtocolVersion` + iOS's `BridgeSourceClient.supportedProtocolVersion` together for breaking changes; leave both unchanged for purely additive fields (add optional properties, don't rename existing ones).
- **Behavioural coupling**: auth flow, rate-limit assumptions, buffer-size tradeoffs, error semantics. A bug on one side often has a sibling on the other — pause before committing and ask whether the iOS decoder/scanner could hit the same class of issue.
- **Bug-fix mirroring examples**: the MB `release-group` decode bug (this repo) had no iOS twin — the iOS side just consumes the server's output. The iOS `resolveLibraryTrackID` path-normalization bug had no server twin — paths are normalized once, on the iOS side, from the server's raw output. Each case was 30 seconds of "does the other side need the same fix?" — worth asking every time.

**Don't regress these cross-cutting invariants:**

- **No server-side transcoding, ever.** 1-bit is bit-exact by mission. `/v1/download` serves the file as-is via `http.ServeContent`; never introduce a transcoding path.
- **Rate limits respect the services.** MB anon is 1 req/s (we pace at 1.1s); CAA is IA-infrastructure and polite at 500ms; Deezer is ~50 req/5s (we pace at 120ms). User-Agent identifies the app + GitHub URL per MB's TOS.
- **TLS fingerprint is captured once.** The iOS pin is set during pairing via first-contact; rotating the server cert requires re-pairing. Don't mint a new cert on every `serve` run — `LoadOrGenerate` is sticky by design.
- **`enriched_at` monotonicity.** Upsert resets to 0 on track change so the enricher re-runs; the enricher marks it to `time.Now().UnixNano()` on completion (success or skipped). Never touch it outside those two paths — the query `WHERE enriched_at = 0` drives the worker.

**Working the bridge**: `feat/<topic>` branches, PR to `main`, pre-push `make fmt vet test build-all`. **Working the iOS side**: same convention at `~/Desktop/com.acoseac.dsdplayer/`. Never push direct to `main` on either repo.

## Local test fixture

A small audio library lives at **`/Users/arsenie/medialibtest/`** — 5 artists (Abdullah Ibrahim, Amestoy Trio, Angie Stone, Anthony Hamilton, Dire Straits), ~48 tracks across FLAC / M4A / MP3. Good coverage for tag extraction, enrichment, and playback paths without a NAS.

Restartable recipe (`/tmp/` is wiped on reboot — just re-run):

```sh
mkdir -p /tmp/bridge-live
cat > /tmp/bridge-live/bridge.yaml <<'EOF'
libraryRoots:
  - /Users/arsenie/medialibtest
libraryName: "Media Test Library"
listenAddress: ":7788"
dataDir: /tmp/bridge-live/data
scanIntervalSec: 3600
EOF
make build >/dev/null
./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &
./bin/bridge pair --config /tmp/bridge-live/bridge.yaml --name simulator
```

Force re-enrichment if the DB is already populated from a prior run:

```sh
sqlite3 /tmp/bridge-live/data/bridge.db "UPDATE tracks SET enriched_at = 0;"
```

## Things that have bitten before

- **Byte-by-byte async iteration kills throughput.** Early `BridgeSourceClient` on the iOS side used `URLSession.bytes(for:)` which yields one `UInt8` per async step — 20M yields for a 20 MB file stalled the pipeline and surfaced as "Network connection lost" even over localhost. Fixed by switching to `URLSession.download(for:)`. Don't regress the iOS side back to byte-wise async reads; and don't add a server-side chunked-encoding mode that assumes byte-wise client consumption.
- **MusicBrainz `release-group` is an object, not a string.** Decoded as `*releaseGroup` struct with `{id, title, primary-type}`. Public MB's live response has this shape; mock fixtures must too (`TestMusicBrainzDecodeRealResponseShape` locks it in).
- **Negative-cache MB errors.** On any MB search error, store an empty MBID under the `(artist, album)` cache key — otherwise sibling tracks on the same album re-query with the same inputs and hit the same error, turning a 1-track failure into an N-track spin loop. See `enricher.enrichOne`.
- **`enriched_at` on upsert resets to 0.** Any edit to the upsert SQL must preserve this reset — otherwise re-scans after a tag change don't re-enrich.

## Repo clean-up

Pre-push:

```sh
make fmt vet test build-all
```

No CI (private repo + Actions-budget block); local checklist is the gate. Paste the clean output into the PR body. When public / budget lifted, re-add `.github/workflows/ci.yml`.
