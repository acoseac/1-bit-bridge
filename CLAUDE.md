# 1-bit-bridge

Cross-platform Go companion server for the [1-bit](https://apps.apple.com/us/app/1-bit/id6762529497) iOS music player. Replaces SMB as the transport over high-latency links (Tailscale / DERP relay) — HTTP/2 + bearer-token auth, TLS fingerprint pinning, pre-built library manifest that skips the iOS scanner's two-phase walk. Target platforms: macOS / Linux / Windows × amd64 / arm64. Tested primarily on macOS.

## Build

- `make build` — builds `./bin/bridge` for the host OS.
- `make build-all` — cross-compiles to `dist/bridge-<os>-<arch>{.exe}`.
- `make test` — pure-Go race-enabled suite; ~150 tests across 10 packages.
- `make fmt vet test build-all` is the pre-push gate — there's no PR-check workflow today, so local-green is load-bearing. See `CONTRIBUTING.md`.
- Pure-Go stack: `modernc.org/sqlite` (no cgo), `github.com/mewkiz/flac`, `github.com/dhowden/tag`, `github.com/hashicorp/mdns`. One static binary, no runtime deps.

## Architecture at a glance

| Package | Role |
|---|---|
| `cmd/bridge` | CLI: `init` / `serve` / `pair` / `scan` / `version` |
| `internal/config` | YAML loader with defaults + path-relative resolution + `Save()` for admin edits |
| `internal/tls` | Self-signed ECDSA P-256 cert minter, SHA-256 fingerprint for iOS pinning |
| `internal/auth` | Bearer-token store (hashed, atomic persist, cross-process pickup) |
| `internal/fs` | Path-safe resolver (traversal-rejection, multi-root routing, hot-reload via SetRoots) |
| `internal/manifest` | SQLite library index + tag extractors (FLAC / DSF / MP3 / M4A) + JSON serializer; Scanner roots are hot-reloadable |
| `internal/enrich` | MusicBrainz + Cover Art Archive + Deezer clients, rate-limited (1.1s / 500ms / 120ms) |
| `internal/api` | HTTP/2 handlers: `/v1/{health,list,stat,read,download,manifest,artwork/{mbid},artist-image/{mbid}}` |
| `internal/admin` | Local web console on 127.0.0.1:7789 — library/devices/stats/settings pages + JSON API + bridge:// QR pair URL. Loopback-only, no auth. |
| `internal/packaging` | launchd plist + systemd unit templates + install/uninstall helpers, used by `bridge init` |
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
- **Admin console is loopback-only, no auth.** `config.validateLoopbackAddress` + `admin.loopbackOnly` middleware both enforce this. Don't add an auth layer that bypasses the loopback constraint; don't expose admin behind Tailscale / reverse-proxy. Anyone on the host already owns the token store and the SQLite DB — auth on top would be theatre. For remote admin, SSH-tunnel the port.
- **Single ↔ multi-root storage form flips.** When the admin adds a second root or removes back down to one, track paths change from `Artist/Album/…` to `<basename>/Artist/Album/…`. The admin handler calls `store.WipeAllTracks()` before the new scan so no stale rows survive. Don't try to migrate in place — the rescan is cheap, enrichment is cached by MBID.

**Working the bridge**: `feat/<topic>` branches, PR to `main`, pre-push `make fmt vet test build-all`. **Working the iOS side**: same convention at `~/Desktop/com.acoseac.dsdplayer/`. Never push direct to `main` on either repo.

## Local test fixture

Point `--library` at any folder with a handful of tagged audio files and you've got a working test setup — FLAC / DSF / MP3 / M4A all work. A few dozen tracks across 4–5 artists covers the tag-extraction, enrichment, and playback paths without needing a NAS.

Short path (no service install, doesn't touch launchd):

```sh
make build >/dev/null
./bin/bridge init --yes --no-service \
  --dir /tmp/bridge-live \
  --library ~/Music/test-library \
  --name "Test Library"
./bin/bridge serve --config /tmp/bridge-live/bridge.yaml &
# Admin console: http://127.0.0.1:7789/ — pair from there, or keep using `bridge pair` for scripts.
```

The old hand-authored `bridge.yaml` recipe still works; keeping the init form because it also mints the TLS cert up-front and catches config typos before `serve` is up.

Force re-enrichment if the DB is already populated from a prior run:

```sh
sqlite3 /tmp/bridge-live/data/bridge.db "UPDATE tracks SET enriched_at = 0;"
```

## Things that have bitten before

- **Byte-by-byte async iteration kills throughput.** Early `BridgeSourceClient` on the iOS side used `URLSession.bytes(for:)` which yields one `UInt8` per async step — 20M yields for a 20 MB file stalled the pipeline and surfaced as "Network connection lost" even over localhost. Fixed by switching to `URLSession.download(for:)`. Don't regress the iOS side back to byte-wise async reads; and don't add a server-side chunked-encoding mode that assumes byte-wise client consumption.
- **MusicBrainz `release-group` is an object, not a string.** Decoded as `*releaseGroup` struct with `{id, title, primary-type}`. Public MB's live response has this shape; mock fixtures must too (`TestMusicBrainzDecodeRealResponseShape` locks it in).
- **Negative-cache MB errors.** On any MB search error, store an empty MBID under the `(artist, album)` cache key — otherwise sibling tracks on the same album re-query with the same inputs and hit the same error, turning a 1-track failure into an N-track spin loop. See `enricher.enrichOne`.
- **`enriched_at` on upsert resets to 0.** Any edit to the upsert SQL must preserve this reset — otherwise re-scans after a tag change don't re-enrich.
- **Optional `Track` numeric/bool fields are pointers** (PR #51): `IsDSD`, `TrackNumber`, `DiscNumber`, `Year`. Non-pointer + `omitempty` silently drops zero/false from the wire — iOS's `Bool?` / `Int?` decoders end up with `nil` instead of `Some(false)` / `Some(0)`. `extractFLACFormat` sets `*IsDSD = false` explicitly so iOS can trust `isDSD: false` to mean "definitely PCM"; `extractDSF` sets `*IsDSD = true`. The dhowden-tag fallback path leaves all three integer pointers populated (always non-nil) — the underlying tag library returns 0 for both "absent" and "explicit zero" with no way to distinguish.
- **`Retry-After` is honored on MB and iTunes 429/503 responses** (PR #50, PR #52). `parseRetryAfter` lives in `internal/enrich/musicbrainz.go` and is reused by `ITunesClient.get`, `ITunesClient.FetchArtwork`, and `MusicBrainzClient.get`. Caps at `maxRetryAfter = 1h` to prevent a hostile/misconfigured upstream from parking the enricher indefinitely; the cap applies in the *seconds* domain BEFORE multiplying by `time.Second` (overflow guard) AND on `strconv.ErrRange` overflow (which `ParseInt` returns for >int64-max values). `time.Sleep` for pacing is replaced with `sleepCtx` in the iTunes path so shutdown isn't blocked by up to 2× ITunesMinInterval per in-flight call.
- **iTunes is a fallback, not a primary source** (PR #52). The artwork chain is MB → CAA-release → CAA-release-group → iTunes. iTunes hits cache under the MB-derived release MBID (`<MBID>-<size>.jpg`), keeping the wire shape unchanged — `/v1/artwork/{mbid}` serves whatever path matches. Skipping MB entirely when iTunes hits would require a synthetic-MBID cache key OR relaxing the strict-UUID `mbidPattern` regex on the artwork handler; both are out of scope until the wire shape gets a proper rev. The `itunesFallbackHits` atomic counter tracks how often iTunes salvaged a CAA miss — useful telemetry for whether the fallback is pulling weight.
- **mDNS TXT records carry `host` + `port`** (PR #53). Without these, iOS would have to NWConnection-resolve the Bonjour service to its hostport form, which is unreliable on iOS 26.4 (`currentPath?.remoteEndpoint` stays in `.service(...)` form even at state `.ready` time). The bare-hostname-plus-`.local` form `cfg.advertisedHost()` produces matches the SRV target the bridge has always advertised, so the cert SANs cover it without any extra change. The function falls back to `localhost.local` if both `cfg.Hostname` AND `os.Hostname()` come back blank, so we never emit `host=.local` (which would build invalid client URLs). `Port` is validated as `1..=65535` at `Advertise` time — out-of-range would land on the wire and have iOS construct unusable URLs.

## Repo clean-up

Pre-push:

```sh
make fmt vet test build-all
```

No PR-check CI workflow today — local `make fmt vet test build-all` is the gate. Paste the clean output into the PR body. If a CI workflow is ever re-added, the expectation is that it matches the local gate (same four commands) rather than drift.

Releases *are* wired up: `.github/workflows/release.yml` runs goreleaser on tag push (`git tag v0.1.0 && git push --tags`), producing signed+notarized darwin archives and unsigned linux / windows archives as a draft GitHub Release. Windows Authenticode signing is pending — tracked against the next release once SignPath Foundation approval lands. Edit the auto-generated release notes and publish; the `README.md` install recipe works for end users from that point.
