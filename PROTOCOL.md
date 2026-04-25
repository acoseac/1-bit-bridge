# 1-bit Bridge Wire Protocol — v1

This is the **source of truth** for the wire contract between the `1-bit-bridge` server and the `1-bit` iOS app. A verbatim copy is mirrored into the iOS repo at `com.acoseac.dsdplayer/docs/BridgeProtocol.md`; any change here must land in both places in the same PR cycle (see [CONTRIBUTING.md](CONTRIBUTING.md) — "Mirror-PR rule").

## Versioning

- **Protocol version**: `1`.
- Every response carries the header `X-Bridge-Protocol: 1`.
- `GET /v1/health` returns the current protocol version and the server's build version (see below).
- iOS checks `protocolVersion` at pairing and on each session's first request. A mismatch surfaces a clear error and refuses to connect rather than risk silent misbehavior.
- **Breaking wire changes bump `protocolVersion`.** Additive, backward-compatible changes (new optional fields, new endpoints) stay at the same version.

## Transport

- **HTTPS only**, HTTP/2 preferred. No plaintext HTTP endpoint.
- Server mints a self-signed certificate on first run (see `internal/tls`); iOS pins by the SHA-256 fingerprint captured during pairing. A public-CA cert is also supported if configured.
- Path segment: all endpoints are prefixed `/v1/`.

## Authentication

- Every request **except `GET /v1/health`** must carry `Authorization: Bearer <token>`. `/v1/health` is unauthenticated so the iOS "Add Bridge" sheet can surface a useful error before the user has pasted a token.
- Tokens are minted by `bridge pair` and stored server-side as a salted hash.
- An unauthenticated request is answered with `401 Unauthorized` and a JSON body:
  ```json
  { "error": "unauthorized" }
  ```
- A valid token with insufficient scope (reserved for future use) returns `403`.

## Path semantics

- All client-supplied paths are **relative to one of the server's configured `libraryRoots`**.
- Paths are `/`-delimited regardless of the server OS. The server maps to native separators internally.
- Absolute paths, `..` segments, and symlink-escapes out of a library root are rejected with `400 Bad Request`.
- An empty `path` parameter (or `/`) refers to the top-level of the (single) library root; multi-root support is TBD in a later minor revision.

## Endpoints

### `GET /v1/health`

Pairing probe and liveness check. No auth token required for this endpoint (so the iOS "Add Bridge" sheet can show a meaningful error before the user has pasted their token).

**Response** (`200 OK`, JSON):
```json
{
  "protocolVersion": 1,
  "serverVersion": "0.0.1",
  "libraryName": "My Music Library",
  "libraryRoots": ["Music"],
  "certFingerprint": "AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89",
  "startedAt": "2026-04-23T10:15:04Z",
  "scanState": {
    "lastFullScan": "2026-04-23T11:41:01Z",
    "tracksIndexed": 24518,
    "isScanning": false
  },
  "endpoints": [
    "https://192.168.1.10:7788",
    "https://homepc.local:7788",
    "https://100.64.5.9:7788",
    "https://[fd7a:115c:a1e0::f536:e41f]:7788"
  ],
  "latestServerVersion": "0.1.0",
  "updateAvailable": true,
  "updateReleaseNotesURL": "https://github.com/acoseac/1-bit-bridge/releases/tag/v0.1.0",
  "minClientVersion": "1.0.0"
}
```

The four `latestServerVersion` / `updateAvailable` / `updateReleaseNotesURL` / `minClientVersion` fields are an additive extension landed in bridge 0.1.0; they are populated only when the bridge has an updater configured (it polls the GitHub Releases API in the background) and at least one successful poll has cached a result. All four are `omitempty` on the wire — older bridges ship the response without them, and iOS clients MUST tolerate their absence.

`minClientVersion` advertises the iOS app version *this bridge* needs from its clients (build-time-injected via `-ldflags -X .../version.MinClientVersion=…`). iOS uses it to surface "your app may be too old for this bridge" hints. It is NOT the floor a *candidate update* would require — that's a Phase B install-side concern and is delivered through admin-console / CLI surfaces, not `/v1/health`.

`endpoints` (additive since v1, optional, may be absent or empty) is the full list of URLs the server is currently reachable at — LAN IPv4/IPv6 (global unicast only; link-local is filtered because it's not reachable across devices), the `<hostname>.local` mDNS form, and any Tailscale interface (CGNAT `100.64/10` for v4, `fd7a:115c:a1e0::/48` ULA for v6). Clients use this to learn new alternates at heartbeat time so they can roam between LAN and Tailscale without re-pairing.

**Ordering** (reflects the server's recommendation — clients pick the first URL they can reach):

1. LAN IPv4 (private-unicast on a non-loopback, non-Tailscale interface)
2. LAN IPv6 (global-unicast counterpart to LAN IPv4)
3. mDNS `<hostname>.local`
4. Tailscale IPv4 (CGNAT `100.64/10`)
5. Tailscale IPv6 (ULA `fd7a:115c:a1e0::/48`)
6. Public (any remaining globally-routable address)

The mDNS entry sits between LAN and Tailscale because it's a hostname alias for the same LAN path — on a LAN where resolution works it's a `.local` round-trip to the IPv4 we already listed, and on a LAN where mDNS is blocked (some captive portals, VLAN-segmented networks) the Tailscale CGNAT entry below it is the correct next step without waiting on a failing `.local` lookup.

### `GET /v1/list?path=<rel>`

Directory listing. Replaces `SMBConnectionPool.list`.

**Response** (`200 OK`, JSON array):
```json
[
  { "name": "Album A", "path": "Artist/Album A", "isDir": true,  "size": 0,        "mtime": "2026-04-20T12:00:00Z" },
  { "name": "01 Track.flac", "path": "Artist/Album A/01 Track.flac", "isDir": false, "size": 12345678, "mtime": "2026-04-20T12:00:00Z" }
]
```

### `GET /v1/stat?path=<rel>`

Single-entry stat. Replaces `SMBConnectionPool.stat`. Used by the iOS scanner's folder-mtime skip logic.

**Response** (`200 OK`, JSON):
```json
{ "mtime": "2026-04-20T12:00:00Z", "isDir": true, "size": 0 }
```

### `GET /v1/read?path=<rel>`

Ranged byte read. The request MUST carry a `Range: bytes=<start>-<end>` header. Unranged requests are rejected with `400 Bad Request` carrying `{"error":"range_required","message":"use /v1/download for unranged reads; /v1/read requires a Range header"}` — use `/v1/download` for whole-file reads. (RFC 7233 reserves `416 Range Not Satisfiable` for ranges outside the resource; a missing header is a request-shape error, not an out-of-range one.)

**Response** (`206 Partial Content`, binary) with `Content-Range` and `Content-Length` set.

Replaces `SMBConnectionPool.readRange`. Used for ID3v2 / Vorbis tag-header windows (typically 64–128 KB), though the iOS-side manifest fast path replaces most of these calls.

### `GET /v1/download?path=<rel>`

Whole-file stream. Supports `Range: bytes=<a>-<b>` for resumable downloads and for the iOS hybrid pre-cache waiter.

**Response**: `200 OK` for unranged, `206 Partial Content` for ranged. `Accept-Ranges: bytes` is always set. `Content-Length` is always set.

Replaces `SMBConnectionPool.download` and `SMBConnectionPool.downloadStreaming`.

### `GET /v1/manifest?since=<rfc3339-mtime>`

Pre-built library manifest. Replaces the iOS scanner's Phase 1 (folder walk) and Phase 2 (tag enrichment) entirely — the iOS side upserts the returned tracks straight into its SwiftData store.

Query parameters:
- `since` (optional): if set, only tracks with `mtime > since` and folders whose `mtime > since` are included. If unset, the full manifest is returned.
- `limit` (optional, **v1.1+**): positive integer. Server returns up to `limit` tracks in a single paginated page. Must NOT be combined with `since` (returns 400 — since-deltas are small by construction).
- `cursor` (optional, **v1.1+**): opaque token from the previous page's `nextCursor`. On the first page, omit. Iterate until `nextCursor` is null in the response.

Pagination semantics:
- Pages are ordered by track `path` ASC; the cursor is the last path of the previous page.
- **First-page-only fields.** `folders` and `total` are sent only on the first page (request has no `cursor`). Subsequent pages omit them to avoid ~250k rows of redundant JSON on a 50k-track library with 5k folders. `libraryRoots` and `generatedAt` are cheap scalars and ship on every page.
- A short read (fewer rows than `limit`) or empty page means the iteration is done; `nextCursor` is absent.
- Server enforces an upper bound (5000 today) on `limit` silently — a client requesting more gets capped but not rejected.
- Clients that don't send `limit` get the v1.0 single-shot response; no compatibility break.
- Clients MUST treat `folders` and `total` as "absent on mid-run pages" — binding scan-state UI off the first page and ignoring these keys on later pages is the safe pattern (v1.1 iOS does exactly this).

**Response** (`200 OK`, JSON, potentially large — server SHOULD set `Content-Encoding: gzip` when the client's `Accept-Encoding` allows):
```json
{
  "version": 1,
  "generatedAt": "2026-04-23T11:41:01Z",
  "libraryRoots": ["Music"],
  "folders": [
    { "path": "Music/Artist/Album", "mtime": "2026-04-20T12:00:00Z" }
  ],
  "tracks": [
    {
      "path":           "Music/Artist/Album/01 Track.flac",
      "size":           12345678,
      "mtime":          "2026-04-20T12:00:00Z",
      "title":          "Track Title",
      "artist":         "Artist",
      "albumArtist":    "Artist",
      "album":          "Album",
      "trackNumber":    1,
      "discNumber":     1,
      "year":           2024,
      "genre":          "Electronic",
      "duration":       240.5,
      "sampleRate":     96000,
      "bitsPerSample":  24,
      "isDSD":          false,
      "replayGainTrackDB":  -7.2,
      "replayGainAlbumDB":  -7.0,
      "musicBrainzTrackID": "...",
      "musicBrainzAlbumID": "..."
    }
  ],
  "nextCursor": "Music/Artist/Album/last-track.flac",
  "total": 4823
}
```

`nextCursor` and `total` are omitted on non-paginated responses (back-compat).

Field-for-field, this is a serialization of the iOS `Track` / folder rows in [`LibraryModels.swift`](https://github.com/acoseac/1-bit/blob/main/com.acoseac.dsdplayer/LibraryModels.swift); additive server fields not understood by the iOS decoder MUST be ignored rather than rejected.

#### DSD specifics

- `isDSD: true` tracks MUST set `sampleRate` to the DSD rate in Hz (e.g. `2822400` for DSD64, `5644800` for DSD128) and `bitsPerSample: 1`.
- `duration` is in seconds, regardless of format.

### `GET /v1/artwork/{mbid}?size=<int>` and `GET /v1/artist-image/{mbid}`

Serve cached album / artist artwork keyed by MusicBrainz release (or artist) MBID. `size` defaults to 500 px for album artwork.

**Response** (`200 OK`): JPEG body, `Content-Type: image/jpeg`, `Cache-Control: public, max-age=86400`.

**Response** (`202 Accepted`): the server has seen the MBID in a track but hasn't cached the image yet — enrichment is pending (cold cache on first scan, background re-fetch in progress, or cache file was trimmed). Carries `Retry-After: <seconds>` (30 s today); clients SHOULD retry with jittered backoff up to a small cap (iOS uses 5 attempts). Body is the standard JSON error shape with `error: "pending"`.

**Response** (`404 Not Found`): the MBID is unknown — no track in the manifest references it. Clients SHOULD treat as terminal and render a placeholder rather than retrying.

**Response** (`400 Bad Request`): MBID is not a valid UUID, or `size` parameter is out of range.

Backwards compatibility: the 202 branch is a v1.1 addition. Servers that don't have an MBID probe wired (e.g. tests, legacy) fall back to 404 on any cache miss, matching the v1.0 behavior. No protocol version bump is required — iOS clients that ignore 202 still work (they just see a transient-looking failure).

## Error responses

All errors are JSON:
```json
{ "error": "short-code", "message": "human-readable detail" }
```

| Status | `error` code         | When                                              |
|-------:|----------------------|---------------------------------------------------|
|    202 | `pending`            | Artwork / artist-image enrichment not yet cached  |
|    400 | `bad_request`        | Malformed path, missing required query param     |
|    400 | `range_required`     | `/v1/read` called without a `Range` header        |
|    401 | `unauthorized`       | Missing / invalid bearer token                    |
|    403 | `forbidden`          | Valid token, insufficient scope (reserved)        |
|    404 | `not_found`          | Path does not exist in any library root           |
|    500 | `internal`           | Server-side failure                               |
|    503 | `scan_in_progress`   | Manifest requested while an initial scan is busy  |

## Pairing URL scheme

Out-of-band setup path: the admin console emits a custom-scheme URL that carries everything the iOS app needs to add a bridge share in one tap (QR scan or deep link), so the operator never has to paste three separate fields by hand.

```text
bridge://pair?url=<https bridge URL>&token=<base64url bearer>&fingerprint=<AB:CD:…:EF>&name=<library display name>
```

**Query parameters** — all required (iOS MUST reject URLs missing any of these):

| Name          | Value                                                                 |
|---------------|-----------------------------------------------------------------------|
| `url`         | The HTTPS URL iOS should dial, including `https://` scheme and port.  |
| `token`       | Raw bearer token (the same 43-char base64url string `bridge pair` prints). |
| `fingerprint` | Server TLS cert SHA-256 in colon-delimited uppercase hex. Used for pinning. |
| `name`        | Human-readable library name (shown in the iOS UI). |

Unknown query parameters MUST be ignored — future additive fields (e.g. a display hint for the pairing modal) stay at the same protocol version.

**iOS behaviour**: after parsing, the client runs the same `/v1/health` probe + authed `/v1/manifest?since=<future>` verify steps it uses for manual pairing, then persists the share on success. A malformed URL (missing fields, token/fingerprint fail regex sanity) is rejected before any network call.

The scheme is **additive** — bridges that don't ship the admin console (pre-0.0.x builds, bespoke integrations) still work fine with the three-field manual paste path.

## Golden fixtures

See `testdata/fixtures/` in this repo for canonical manifest payloads used as decode-test goldens on the iOS side (`com.acoseac.dsdplayer/Tests/com_acoseac_dsdplayerTests/Fixtures/Bridge/`). A schema change must regenerate the fixtures in both places in the same PR pair.

## Updates

The bridge polls the GitHub Releases API on a background goroutine (`internal/updater`, 6 h cadence by default), caches the latest release tag, and exposes it to two surfaces:

1. **Admin console** — the dashboard's "Updates" tile shows current/available versions, last check time, and (when an update is available) an **Install & restart** button that downloads the archive, verifies SHA-256 from `checksums.txt` (and on macOS, codesign + notarization + Team-ID equality), atomically swaps the binary, arms a rollback marker, and triggers restart. Active downloads (`/v1/read` + `/v1/download` requests in flight) block install with a 409; an "Install anyway" affordance is available with explicit confirmation copy. **Windows install is not yet supported** — the operator-triggered flow is darwin/linux only in Phase B; Windows operators still upgrade by stopping the service, replacing `bridge.exe`, and starting it back up.
2. **`/v1/health`** — the four optional response fields documented above. iOS reads these and surfaces "Bridge update available" / "Bridge is older than recommended" hints in the Bridge Editor.

The CLI mirrors the admin-console flow: `bridge update --check` polls, `bridge update --yes` polls + installs (no admin console required for SSH-only operators).

Boot-time rollback: install records `<dataDir>/update-state.json` with the target version BEFORE swapping the binary. On the next boot, the new binary checks the marker and either confirms the install (transitions to `installed`, retains `bridge.bak` for one more boot) or restores `bridge.bak` over the live binary (when the running version doesn't match the target — the new binary either failed to start or behaves wrong). The previous binary is kept as `bridge.bak` for one extra boot after a successful install so a manual rollback via `POST /api/updates/rollback` is still possible without re-downloading.

**Auto-install (Phase C)** is opt-in via three new `bridge.yaml` keys under `update:`:

| Key                   | Default | Meaning                                                                                  |
|-----------------------|---------|------------------------------------------------------------------------------------------|
| `autoInstall`         | `false` | When `true`, every successful poll that surfaces an update triggers install + restart    |
| `quietHours`          | `""`    | Daily window in `HH:MM-HH:MM` form (server-local time). Empty = any time. Wraps midnight |
| `checkIntervalHours`  | `0`     | Override the default 6 h poll cadence. `0` = use the default. Clamped to a 1 h floor      |

Auto-install runs the same install path as the admin button (download → SHA-256 → codesign on macOS → atomic swap → arm rollback marker → restart). It defers to the next poll cycle when:

- `autoInstall = false`
- The wall-clock minute is outside `quietHours`
- The sessions tracker reports inflight `/v1/download` or `/v1/read` requests
- The platform is Windows (auto-install honours the same `CanInstall=false` as the manual path)

**Known follow-up**: a `MinClientVersion` compat gate that consults each paired token's last-seen `X-Client-Version` against the candidate release's floor and refuses auto-install when it would orphan an active client. Implementing this requires a `release-meta.json` asset alongside the archive (the candidate's `MinClientVersion` lives inside its own binary today, which we can't introspect without installing it first). Operator-triggered installs are not affected — the operator made the call explicitly.

**Trust boundary:** the install endpoint is admin-loopback only (same trust model as every other admin route — see `internal/admin/admin.go` doc comment). There is intentionally no remote-trigger install from iOS. Operators install at the bridge host. The iOS app's role is awareness.

### Client version reporting

The iOS app sends an additive request header on every authenticated request:

```http
X-Client-Version: <CFBundleShortVersionString>
```

Example: `X-Client-Version: 1.2`. The bridge persists the most recent value per token (`auth.Token.LastClientVersion` in `<dataDir>/tokens.json`) so the updater can later refuse an auto-install whose `MinClientVersion` would orphan a still-active iOS build that hasn't shipped through the App Store yet.

The header is optional — older iOS clients that don't send it continue to authenticate normally. Bridges record nothing for those tokens, and the updater treats them as "version unknown" → blocks `MinClientVersion`-gated auto-installs unless the operator overrides.

## Operator: Backup & restore

A snapshot bundles every file an operator would otherwise have to re-pair / re-scan to recover from corruption: the manifest SQLite database (`bridge.db`), the token store (`tokens.json`), the TLS material (`server.crt` + `server.key`), and the live config (`bridge.yaml`). Snapshots land in `<dataDir>/backups/<timestamp>/` (UTC, Windows-friendly format `2006-01-02T15-04-05Z`). The bundle is permissioned 0700/0600.

**Sensitivity:** snapshots contain the TLS private key and token hashes — anyone with read access to a snapshot can impersonate the bridge or forge an authenticated request. Treat them as secret-grade material. The CLI prints this warning on every backup.

**Periodic snapshots.** `bridge serve` runs an automatic ticker alongside the manifest scanner (default cadence 24 h, retention 7 most-recent). Tune via `bridge.yaml`:

```yaml
backup:
  intervalHours: 24    # 0 disables the periodic ticker; the CLI still works
  keep: 7              # rotate older snapshots after each periodic run
```

**On-demand snapshot.** `bridge backup [--config bridge.yaml] [--keep N]` is safe to run while `bridge serve` is up: SQLite's `VACUUM INTO` on a WAL-mode database produces an atomic clean copy without locking out the running process.

**Restore.** Stop the bridge first. Then `bridge restore [--yes] <snapshot-dir>`. The CLI validates the snapshot's `manifest.json` schema version and refuses incompatible bundles. Per-file copies are atomic (temp + rename); a partial failure leaves each restored file internally consistent. After restore, start the bridge service.

**Why no admin-UI download button.** The admin console exposes `GET /api/backups` (list) and `POST /api/backups` (snapshot) but deliberately does NOT offer a snapshot download — a one-click web download for a bundle containing the TLS private key would be a credential extraction surface. Operators move snapshots offsite via `scp`/`rsync` against `<dataDir>/backups/`.

## Compatibility matrix

The matrix below states what each side needs from the other. The hard `protocolVersion` integer (line 8 above) is the breaking-change boundary; this is the additive-feature recommendation.

| Bridge version | Min iOS app version | Notes                                                  |
|----------------|---------------------|--------------------------------------------------------|
| `0.0.x`        | `1.0.0`             | Pre-Phase-A. No version-awareness UI.                  |
| `0.1.x`        | `1.0.0`             | Phase A. iOS 1.2+ surfaces update hints; older clients still work. |

| iOS app version | Min bridge version  | Notes                                                  |
|-----------------|---------------------|--------------------------------------------------------|
| `1.0.x` – `1.1.x` | `0.0.1`           | Pre-Phase-A. No update-aware UI.                       |
| `1.2.0+`        | `0.0.1`             | Phase A. Will surface "bridge update recommended" when paired bridge is below `0.1.0`. |

Pairings whose `protocolVersion` integers don't match are refused at `/v1/health`-check time. The compat-matrix rows above are advisory: operators see hints in the iOS app + admin console but the connection still works.
