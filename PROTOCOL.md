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
  "certFingerprint": "SHA256:AbCdEf0123...",
  "startedAt": "2026-04-23T10:15:04Z",
  "scanState": {
    "lastFullScan": "2026-04-23T11:41:01Z",
    "tracksIndexed": 24518,
    "isScanning": false
  }
}
```

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
  ]
}
```

Field-for-field, this is a serialization of the iOS `Track` / folder rows in [`LibraryModels.swift`](https://github.com/acoseac/1-bit/blob/main/com.acoseac.dsdplayer/LibraryModels.swift); additive server fields not understood by the iOS decoder MUST be ignored rather than rejected.

#### DSD specifics

- `isDSD: true` tracks MUST set `sampleRate` to the DSD rate in Hz (e.g. `2822400` for DSD64, `5644800` for DSD128) and `bitsPerSample: 1`.
- `duration` is in seconds, regardless of format.

## Error responses

All errors are JSON:
```json
{ "error": "short-code", "message": "human-readable detail" }
```

| Status | `error` code         | When                                              |
|-------:|----------------------|---------------------------------------------------|
|    400 | `bad_request`        | Malformed path, missing required query param     |
|    400 | `range_required`     | `/v1/read` called without a `Range` header        |
|    401 | `unauthorized`       | Missing / invalid bearer token                    |
|    403 | `forbidden`          | Valid token, insufficient scope (reserved)        |
|    404 | `not_found`          | Path does not exist in any library root           |
|    500 | `internal`           | Server-side failure                               |
|    503 | `scan_in_progress`   | Manifest requested while an initial scan is busy  |

## Pairing URL scheme

Out-of-band setup path: the admin console emits a custom-scheme URL that carries everything the iOS app needs to add a bridge share in one tap (QR scan or deep link), so the operator never has to paste three separate fields by hand.

```
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

## Compatibility matrix

| Bridge protocol | iOS app version       |
|-----------------|-----------------------|
| `1`             | `>= 1.1.0` (unreleased) |

Pairings outside this matrix are refused at `/v1/health`-check time.
