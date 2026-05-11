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

- The default rule: every request must carry `Authorization: Bearer <token>` where `<token>` is a minted bearer.
- Three documented exceptions:
  - **`GET /v1/health`** — no auth, so the iOS "Add Bridge" sheet can surface a useful error before the user has pasted a token.
  - **`POST /v1/pairing/requests`** — no auth. The body's `pollSecretHash` (SHA-256 hex of the iOS-generated `pollSecret`) IS the binding: subsequent polls present the matching `pollSecret` raw, server hashes and constant-time-compares.
  - **`GET /v1/pairing/{requestId}` and `DELETE /v1/pairing/{requestId}`** — `Authorization: Bearer <pollSecret>` where `<pollSecret>` is the request-creator's textual encoded form (see "pollSecret wire encoding" below). Server applies `SHA-256` to the bytes of the bearer string and constant-time-compares against the stored hash.
- Tokens are minted by `bridge pair` (or by approving an admin-approval pairing request) and stored server-side as a salted hash.
- An unauthenticated request is answered with `401 Unauthorized` and a JSON body:
  ```json
  { "error": "unauthorized" }
  ```
- A valid token with insufficient scope (reserved for future use) returns `403`.

### pollSecret wire encoding (additive, since v1.2)

The `pollSecret` is a **32-byte cryptographic random value, encoded as `base64url` without padding** (RFC 4648 §5, `[A-Za-z0-9_-]`, no `=`). The 32-byte input produces a stable 43-character ASCII string — header-safe, copy-paste-safe, and identical to the encoding `bridge pair`-issued bearer tokens already use.

The textual form is what flows on the wire AND what gets hashed:

- Client generates 32 random bytes → encodes as `base64url` (no padding) → that 43-char string IS the `pollSecret`.
- `pollSecretHash` field in `POST /v1/pairing/requests` is `lower(hex(sha256(pollSecret)))` — i.e. SHA-256 over the UTF-8 bytes of the 43-char encoded string, hex-encoded lowercase (64 chars total).
- `Authorization: Bearer <pollSecret>` carries the same 43-char encoded string.
- Server side: extract bearer → `sha256(bytes(bearer))` → constant-time-compare against the stored hash.

Anchoring the hash to the encoded form (not the raw bytes) means client and server agree on a single canonical representation, eliminating any risk of a divergence where iOS hashes one value and the server hashes another.

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
  "minClientVersion": "1.0.0",
  "upscaleEnabled": false,
  "certNotAfter": "2027-05-26T10:15:04Z"
}
```

The four `latestServerVersion` / `updateAvailable` / `updateReleaseNotesURL` / `minClientVersion` fields are an additive extension landed in bridge 0.1.0; they are populated only when the bridge has an updater configured (it polls the GitHub Releases API in the background) and at least one successful poll has cached a result. All four are `omitempty` on the wire — older bridges ship the response without them, and iOS clients MUST tolerate their absence.

`upscaleEnabled` (additive since v1.2, `*bool` with `omitempty`) reports whether the bridge has the offline PCM-upscaling feature enabled in `bridge.yaml` AND a working `sox` binary available on PATH (a misconfigured server with the flag on but no sox advertises `false` here — graceful degradation). iOS uses this single capability flag to gate every variant-related UI surface for that bridge: when `false` (or the field is absent on a pre-v1.2 bridge), the picker, glyph, and "Generate upscaled" context menu items are all hidden — bridge rows in the library look identical to SMB / local rows, so the user sees no functionality they can't use. Operator opt-in is in `bridge.yaml`'s `upscale.enabled: true`; default is off. See "Upscaling (offline PCM variants)" below for the wire shape on `/v1/manifest` and `/v1/download` when the flag is on.

`certNotAfter` (additive since v1.2, `*time.Time` with `omitempty`) is the on-disk TLS certificate's `NotAfter` (UTC). iOS uses it to surface a "Bridge cert expires in X days — re-pair to refresh" warning before the cert actually expires and TLS handshakes start failing at Apple's ATS layer (Apple's 397-day cap means operators must re-pair roughly annually). Pre-v1.2 bridges and bridges where cert parsing failed at startup omit the field; iOS treats absence as "no expiry info, never warn".

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

### `GET /v1/download?path=<rel>[&variant=<id>]`

Whole-file stream. Supports `Range: bytes=<a>-<b>` for resumable downloads and for the iOS hybrid pre-cache waiter.

**Response**: `200 OK` for unranged, `206 Partial Content` for ranged. `Accept-Ranges: bytes` is always set. `Content-Length` is always set.

Replaces `SMBConnectionPool.download` and `SMBConnectionPool.downloadStreaming`.

`variant` (additive since v1.2, optional): selects an alternate rendering of the source instead of the original. Value is one of the IDs the bridge advertised on the source's `Track.variants` array in `/v1/manifest`. Today's only producer is `bridge upscale`, which mints `upscaled-<schemaVersion>-<targetRate>-<targetBits>` IDs (e.g. `upscaled-v1-176400-24`). Path validation runs on `path` first — a malformed `path` is rejected with the standard 400 family before any variant lookup.

**Variant-specific responses:**
- `404 variant_not_found`: no row exists for that `(path, variant)` pair, OR the upscale feature is disabled on this bridge. iOS treats this as "fall back to the original on the next playback".
- `410 Gone variant_stale`: a row exists but the source file's mtime/size has drifted since the sidecar was minted. iOS handles this the same way as a 404 (fall back to playing the original); the only difference is the error message surfaced in diagnostics ("variant expired" vs "variant not found"). The sidecar stays on disk; the operator's recovery is `bridge upscale --force <track>`.
- `410 Gone variant_missing_on_disk`: the row points at a sidecar file that's been removed under the bridge's feet (manual cleanup). iOS falls back to the original; `bridge upscale --gc` reconciles.

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
      "musicBrainzAlbumID": "...",
      "artworkMBID": "...",
      "artistMBID": "...",
      "enriched": true
    }
  ],
  "nextCursor": "Music/Artist/Album/last-track.flac",
  "total": 4823,
  "enrichmentProgress": {
    "tracksTotal":    4823,
    "tracksEnriched": 4810,
    "lastEnrichedAt": "2026-04-23T11:40:55Z"
  }
}
```

`nextCursor` and `total` are omitted on non-paginated responses (back-compat).

Field-for-field, this is a serialization of the iOS `Track` / folder rows in [`LibraryModels.swift`](https://github.com/acoseac/1-bit/blob/main/com.acoseac.dsdplayer/LibraryModels.swift); additive server fields not understood by the iOS decoder MUST be ignored rather than rejected.

#### Per-track `enriched` (additive, since v1.1)

`enriched` is set on every track to `true` once the bridge's enrichment loop has processed the row (regardless of whether MusicBrainz / Cover Art Archive / Deezer returned matches — empty lookups still flip the bit, see `markSkipped` in `internal/enrich/enricher.go`). `false` means the row is still queued for the enricher.

Pre-v1.1 servers omit the field. Clients MUST treat absence as "fully enriched" — the conservative back-compat assumption (matches the pre-flag behaviour where the iOS scanner unconditionally treated bridge tracks as parsed). Newer clients use `enriched: false` to suppress the permanent Deezer-miss stamp on artists whose MBID hasn't landed yet, so the eventual sync's bridge-cached image still wins over a premature negative cache.

#### Manifest-level `enrichmentProgress` (additive, since v1.1)

`enrichmentProgress` is a snapshot of library-wide enrichment status at manifest-build time:

- `tracksTotal` — total track count in the store. Same number as the top-level `total` in paginated mode; included redundantly here for convenience on non-paginated full-manifest responses (where `total` is absent).
- `tracksEnriched` — number of tracks past the enrich pass (`enriched_at != 0`).
- `lastEnrichedAt` — UTC wall-clock of the most recent successful `MarkEnriched` call across the whole library. Absent (omitempty) when no track has ever been enriched.

iOS uses this to render an "Enrichment in progress (X / Y)…" footer and gates the suppression-of-negative-cache behaviour on a 24h freshness check on `lastEnrichedAt` — a bridge that went idle a week ago shouldn't make the iOS UI claim enrichment is "still happening".

**Pagination behaviour.** `enrichmentProgress` is populated only on the **first page** of a paginated full-manifest response (request has no `cursor`) and on every non-paginated response. Same first-page-only convention as `folders` / `total` — the values are stable across a pagination run, so iOS snapshots them off the first page and ignores any later pages.

Both fields are additive — `ProtocolVersion` stays at `1`. Pre-v1.1 servers omit them; pre-v1.1 clients ignore them.

#### DSD specifics

- `isDSD: true` tracks MUST set `sampleRate` to the DSD rate in Hz (e.g. `2822400` for DSD64, `5644800` for DSD128) and `bitsPerSample: 1`.
- `duration` is in seconds, regardless of format.

### `POST /v1/upscale` (additive, since v1.2)

Hands a track or folder to the long-lived transcode worker pool inside `bridge serve` for offline PCM upscaling. Companion to the CLI `bridge upscale` command — same engine, different lifetime.

**Authentication**: standard `Authorization: Bearer <token>` (same rule as every other `/v1/*` endpoint except `/v1/health`).

**Request body** (`application/json`):
```json
{ "path": "Artist/Album/01.flac" }
```

`path` is a library-relative path. May reference a single track file or a folder; the handler stat()s to decide. Folder requests recursively enqueue every regular file under the folder; the per-track eligibility gate (PCM, source rate < target rate, no fresh sidecar already cached) runs inside the enqueuer and silently rejects ineligible candidates.

**Response** — happy path / partial-success (`202 Accepted`, JSON):
```json
{ "enqueued": 12, "rejected": 3, "eligible": 15, "queueFull": true }
```

| Field | Meaning |
|---|---|
| `enqueued` | Jobs the worker pool accepted onto its queue. |
| `rejected` | Candidates the handler considered but didn't queue (queue full + ineligible + missing on disk). Omitted from the wire when zero. |
| `eligible` | Total regular files the handler considered (folder walk surface, or 1 for a single-track request). Omitted when zero. |
| `queueFull` | `true` iff at least one rejection was specifically due to queue capacity. iOS surfaces this in a toast so the user knows to retry; ineligibility / missing-source rejections don't flip the bit. Omitted when false. |

**Response** — feature disabled (`503 Service Unavailable`, JSON):
```json
{ "error": "upscale_disabled", "message": "upscaling is not enabled on this bridge" }
```
Returned when:
- `cfg.Upscale.Enabled == false` in `bridge.yaml`, OR
- the sox-on-PATH probe failed at startup (graceful degradation — same wire code, operator privacy).

**Response** — every candidate bounced queue-full (`503 Service Unavailable`, JSON):
```json
{ "error": "queue_full", "message": "transcode worker queue is full; wait for current conversions to finish" }
```
Distinct from `upscale_disabled`. The pool's queue cap is operator-tunable via `cfg.Upscale.QueueCap` (default 5000).

**Other error codes**:
- `400 bad_request` — body isn't valid JSON, `path` is empty, or path traversal (`..`) attempted.
- `404 not_found` — path doesn't resolve under any library root.

**Idempotency**: the worker pool dedups jobs on `(source_path, variant_id)`. A duplicate request while a job is queued or running is a silent no-op (still counted in `enqueued`); the iOS app can mash "Generate" without server-side duplicate work.

**Asynchronous completion**: the response returns as soon as the jobs are queued. iOS discovers completed variants via the next `/v1/manifest` sync (which advertises the new `Track.variants` entries). For pool-level visibility while jobs are in flight (queue depth, lifetime totals, failure counts) iOS calls the companion `GET /v1/upscale/stats` endpoint described below; for per-track completion the manifest is still the authoritative signal.

### `GET /v1/events` (additive, since v1.2)

Server-sent events stream replacing per-resource polling for upscale stats and pairing approvals. Push delivery means iOS can react within tens of milliseconds instead of the 2–5s polling cadence the previous endpoints required, AND iOS doesn't burn radio cycles polling every few seconds while a management section is open.

**Authentication**: standard `Authorization: Bearer <token>` (same rule as every other `/v1/*` endpoint except `/v1/health` and the pairing routes).

**Query params**:
- `topics` (optional): comma-separated allowlist. Example: `?topics=upscale,pairing` subscribes to every event whose topic equals or has the prefix `upscale.` or `pairing.`. Empty / absent = subscribe to all topics. Whitespace around entries is trimmed.

**Response** (`200 OK`, `Content-Type: text/event-stream`):

```text
id: 42
event: upscale.stats
data: {"enabled":true,"pool":{"queueLen":12,"inflight":4,"done":126,"failed":0},"cachedVariants":138}

event: pairing.abc123-def456
data: {"state":"approved","verificationCode":"412 593"}

event: heartbeat
data: {}

event: dropped
data: {"missed":3}
```

**Topics**:
- `upscale.stats` — fires whenever the bridge's upscale state changes (job queued, completed, failed). Payload matches `/v1/upscale/stats`.
- `pairing.<requestID>` — fires on `Approve` or `Decline` of the named request. Payload matches `/v1/pairing/<requestID>` for the relevant state.
- `heartbeat` — every 15s; payload `{}`. iOS uses missing heartbeats as a "connection dead" signal that triggers reconnect with backoff. Bridge-internal — iOS parsers swallow this at the transport layer.
- `dropped` — synthetic notice fired when the server's per-subscriber buffer evicted events under back-pressure (slow client, network blip). Payload `{"missed":N}`. iOS treats this as "I missed state — refetch via the polling endpoint to reconcile."

**Reconnect / replay**:
- Each **publishable** event (`upscale.stats`, `pairing.<id>`) carries a monotonic `id:` field. Clients persist the last id observed and send `Last-Event-ID: <id>` on reconnect. The synthetic `heartbeat` and `dropped` events deliberately omit `id:` — they're transport-layer signals (keepalive, slow-consumer notice) that the iOS parser handles distinctly from publishable state changes, and including them in the Last-Event-ID stream would let a heartbeat-only window mask a missed publish on reconnect.
- Server holds a 100-event sliding buffer; reconnects within the buffer get the missed events as a replay burst.
- A `Last-Event-ID` the server doesn't recognise (older than the buffer) returns no replay — iOS interprets the empty replay + the next live event as "I missed too much; refetch state."

**Headers**:
- `Cache-Control: no-cache`
- `Connection: keep-alive`
- `Content-Encoding: identity` (defensive against future global gzip middleware that would buffer the stream)
- `X-Accel-Buffering: no` (defensive against fronting reverse-proxies)

**Backwards compatibility**:
- Pre-v1.2 bridges return `404 events_not_supported` from this endpoint. iOS clients fall back to the polling endpoints.
- The polling endpoints (`/v1/upscale/stats`, `/v1/pairing/{id}`) remain authoritative — operators or older clients can ignore SSE entirely.
- This is a fully-additive change. **No `protocolVersion` bump.**

### `GET /v1/upscale/stats` (additive, since v1.2)

Snapshot of the upscale feature's runtime + on-disk state. Designed for the iOS app's per-share "Upscaling" management section to show the operator how many jobs are queued, in flight, finished, or failed without surfacing the admin console externally. The wire shape is intentionally identical to the admin tile's `/api/upscale/stats` payload — same numbers in both places.

**Authentication**: standard `Authorization: Bearer <token>` (same rule as every other `/v1/*` endpoint except `/v1/health` and the pairing routes).

**Response** (`200 OK`, JSON):

```json
{
  "enabled": true,
  "soxAvailable": true,
  "pool": {
    "workers": 4,
    "queueCap": 5000,
    "queueLen": 12,
    "inflight": 4,
    "enqueued": 142,
    "done": 126,
    "failed": 0
  },
  "cachedVariants": 138,
  "cachedBytes": 4823917568
}
```


| Field | Meaning |
|---|---|
| `enabled` | Live runtime state. False when `cfg.Upscale.Enabled` is false OR the sox-precheck demoted the feature at startup OR the operator just PATCHed the feature off (the long-lived Pool may still be alive, but the contract is "feature is off live", matching `/v1/health.upscaleEnabled`). |
| `soxAvailable` | The current `sox(1)`-on-PATH probe result. Omitted when the test harness didn't wire a precheck closure. Operators can install sox without restarting the bridge — within ~30 s the field flips to `true`. |
| `pool` | Live worker-pool snapshot. **Omitted when `enabled` is false** (no pool to query). `queueCap` is operator-tunable via `cfg.Upscale.QueueCap` (default 5000). `enqueued`/`done`/`failed` are lifetime counters since the bridge process started — they reset on restart; `cachedVariants` survives. |
| `cachedVariants` | Row count from `track_variants`. Survives across restarts and reflects historical conversion work — non-zero even when `enabled == false` if the operator disabled the feature without `--gc`. |
| `cachedBytes` | Total size of all sidecar files, summed from `track_variants.size_bytes`. Helps the operator gauge disk usage before deciding to re-enable or `--gc`. |

**Empty / disabled bridge**: returns the zero-value response `{"enabled": false, "cachedVariants": 0, "cachedBytes": 0}`. iOS treats this identically to a 404 from a pre-v1.2 bridge — render "feature off" without distinguishing a missing endpoint from a disabled feature.

**Polling cadence**: iOS polls every 5 s **only while the management page is foregrounded** (never in background). The handler is cheap enough — single SQL `COUNT` + a mutex-protected pool snapshot + a `sox` precheck — to absorb that cadence on Pi-class hosts.

### `GET /v1/artwork/{mbid}?size=<int>` and `GET /v1/artist-image/{mbid}`

Serve cached album / artist artwork keyed by MusicBrainz release (or artist) MBID. `size` defaults to 500 px for album artwork.

**Response** (`200 OK`): JPEG body, `Content-Type: image/jpeg`, `Cache-Control: public, max-age=86400`.

**Response** (`202 Accepted`): the server has seen the MBID in a track but hasn't cached the image yet — enrichment is pending (cold cache on first scan, background re-fetch in progress, or cache file was trimmed). Carries `Retry-After: <seconds>` (30 s today); clients SHOULD retry with jittered backoff up to a small cap (iOS uses 5 attempts). Body is the standard JSON error shape with `error: "pending"`.

**Response** (`404 Not Found`): the MBID is unknown — no track in the manifest references it. Clients SHOULD treat as terminal and render a placeholder rather than retrying.

**Response** (`400 Bad Request`): MBID is not a valid UUID, or `size` parameter is out of range.

Backwards compatibility: the 202 branch is a v1.1 addition. Servers that don't have an MBID probe wired (e.g. tests, legacy) fall back to 404 on any cache miss, matching the v1.0 behavior. No protocol version bump is required — iOS clients that ignore 202 still work (they just see a transient-looking failure).

### Upscaling (offline PCM variants, additive since v1.2)

The bridge supports an opt-in offline PCM-upscaling feature that pre-renders high-rate FLAC sidecars from CD-rate PCM sources via `sox(1)`. The feature is **disabled by default**; operators enable it via `upscale.enabled: true` in `bridge.yaml` and run `bridge upscale` to populate the sidecar cache. Conversion is always offline — the bridge never modulates bytes in flight, preserving the bit-exact mission.

#### Wire shape

When the feature is enabled AND the bridge has cached at least one variant for a track, that track's `Track` entry in `/v1/manifest` carries an additional `variants` array:

```json
{
  "path": "Music/Album/01.flac",
  "size": 12345678,
  "mtime": "2026-04-20T12:00:00Z",
  "title": "...",
  "isDSD": false,
  "sampleRate": 44100,
  "bitsPerSample": 16,
  "variants": [
    {
      "id": "upscaled-v1-176400-24",
      "format": "flac",
      "sampleRate": 176400,
      "bitsPerSample": 24,
      "sizeBytes": 87654321,
      "label": "Upscaled FLAC 24/176.4"
    }
  ]
}
```

`variants` is `omitempty` — pre-v1.2 bridges, disabled bridges, and tracks with no cached variants emit no field. iOS clients unaware of the field decode cleanly via the lenient default JSONDecoder (no protocol version bump).

#### Variant identifier scheme

`id` is opaque to clients but follows a stable convention: `<kind>-<schemaVersion>-<key>`. Today's only producer mints `upscaled-v1-<targetRate>-<targetBits>`. iOS slot-resolves variants by the leading `upscaled-` prefix to honour the share-level "prefer upscaled" toggle. Future variant kinds (PCM→DSD synthesis, alternate bit depths, alternate dither) get their own prefixes without disturbing legacy resolution.

The schema version (`v1`) bumps only when the on-disk sidecar layout or the SoX command shape changes in a way that makes prior sidecars semantically different from a fresh run. Operators don't manage the schema version directly; `bridge upscale --force` re-converts to the current schema.

#### Feature gate semantics

- `upscale.enabled: false` (default): manifest emits no `variants` even if `track_variants` rows exist on disk (predictable round-trip — operator can re-enable to expose the cached sidecars without re-conversion). `/v1/health` reports `upscaleEnabled: false`. `/v1/download?variant=…` returns `404 variant_not_found`.
- `upscale.enabled: true` AND `sox` on PATH: full feature operates as documented.
- `upscale.enabled: true` AND `sox` MISSING from PATH: bridge logs `.error` at startup, in-memory disables the feature, advertises `upscaleEnabled: false`. The rest of the server keeps running.

### `POST /v1/pairing/requests` (additive, since v1.2)

Submit a join request that surfaces in the bridge admin web console as a pending entry. The admin reads the verification code off the iOS device's waiting screen, then approves or declines. iOS polls `/v1/pairing/{requestId}` for the verdict.

**Authentication**: none on this endpoint. iOS generates a 32-byte cryptographic random `pollSecret`, base64url-encodes it (no padding, 43 chars), and submits its SHA-256 hash here. See "pollSecret wire encoding" in the Authentication section above for the full canonical form.

**Request body**:
```json
{
  "deviceName": "Arseni's iPhone",
  "clientVersion": "1.4.0",
  "pollSecretHash": "<64-char lowercase hex SHA-256 of the base64url-encoded pollSecret>"
}
```

**Response** (`201 Created`):
```json
{
  "requestId": "<12-hex chars>",
  "verificationCode": "412593",
  "ttlSeconds": 300,
  "bridgeStartedAt": 1735689600123
}
```

`bridgeStartedAt` is Unix milliseconds of bridge process start. iOS observes it on this response and on every subsequent poll; a value change between calls means the bridge restarted mid-pairing and iOS surfaces a terminal "bridge restarted" state instead of blindly retrying.

**Response** (`400 Bad Request`): `bad_request` — `deviceName` empty or `pollSecretHash` is not exactly 64 hex chars.

**Response** (`503 Service Unavailable`): `queue_full` — bridge has hit the in-flight pending cap (16 by default). The admin sees the queue full of garbage and can mass-decline.

**Response** (`404 Not Found`): `pairing_not_supported` — bridge build doesn't support tap-to-pair (older release, or the route is unregistered). iOS treats this as "fall back to manual entry."

### `GET /v1/pairing/{requestId}` (additive, since v1.2)

Poll for the verdict on a pairing request.

**Authentication**: `Authorization: Bearer <pollSecret>` — the base64url-encoded textual form (43 chars, see "pollSecret wire encoding" above) submitted at request creation. The bridge applies SHA-256 to the bytes of the bearer string and constant-time-compares against the stored hash.

**Response** (`200 OK`):
```json
{
  "status": "pending" | "approved" | "declined" | "expired" | "cert_rotated",
  "ttlSecondsRemaining": 240,
  "bridgeStartedAt": 1735689600123,
  "verificationCode": "412593",
  "token": "<43-char base64url bearer>",
  "tokenId": "<12-hex>"
}
```

`token` and `tokenId` are populated only when `status == "approved"`. **The token is returned on every authorized poll while the request is approved** — NOT read-once. iOS may legitimately retry the same poll across a network blip, and the pollSecret bearer + cert pin gate re-reads. The token is discarded only when iOS sends `DELETE /v1/pairing/{requestId}` (acknowledgment after keychain persist) OR when TTL+grace elapses without acknowledgment, in which case the bridge revokes the minted token to prevent orphans.

`status == "cert_rotated"` indicates the bridge cert fingerprint changed between request creation and admin approve. iOS treats this as terminal and prompts the user to request again on the new cert.

**Response** (`401 Unauthorized`): missing or mismatched `pollSecret`.

**Response** (`404 Not Found`): `unknown_request` — request ID never existed, was deleted, or expired beyond its grace window. iOS treats as terminal.

### `DELETE /v1/pairing/{requestId}` (additive, since v1.2)

Cancel a pending request OR acknowledge receipt of an approved token. Same `Authorization: Bearer <pollSecret>` auth as the poll endpoint.

**Response** (`204 No Content`): success. The row is gone server-side.

**Response** (`401 Unauthorized`): missing or mismatched `pollSecret`.

The handler treats "already deleted" as success (returns 204), so a duplicate DELETE from a retrying iOS client is a no-op rather than a 404. This is the iOS-visible side of the read-many delivery contract.

## Error responses

All errors are JSON:
```json
{ "error": "short-code", "message": "human-readable detail" }
```

| Status | `error` code             | When                                              |
|-------:|--------------------------|---------------------------------------------------|
|    202 | `pending`                | Artwork / artist-image enrichment not yet cached  |
|    400 | `bad_request`            | Malformed path, missing required query param     |
|    400 | `range_required`         | `/v1/read` called without a `Range` header        |
|    401 | `unauthorized`           | Missing / invalid bearer token (or pollSecret)    |
|    403 | `forbidden`              | Valid token, insufficient scope (reserved)        |
|    404 | `not_found`              | Path does not exist in any library root           |
|    404 | `unknown_request`        | Pairing request ID unknown / cleaned up           |
|    404 | `pairing_not_supported`  | Bridge build doesn't expose tap-to-pair           |
|    404 | `events_not_supported`   | Bridge build doesn't expose `/v1/events` (pre-v1.2; iOS falls back to polling) |
|    429 | `rate_limited`           | Per-IP pairing-create rate-limit OR per-token `/v1/manifest` rate-limit tripped |
|    500 | `internal`               | Server-side failure                               |
|    503 | `scan_in_progress`       | Manifest requested while an initial scan is busy  |
|    503 | `queue_full`             | Pending pairing requests at the cap               |

### `/v1/manifest` rate limit (additive, since v1.2.x)

`/v1/manifest` is the bridge's most expensive endpoint — a 50k-track library produces a 100+ MB JSON stream. To protect against a misbehaving paired client (buggy build, future web admin) from exhausting bridge CPU + bandwidth, the bridge applies a per-token token-bucket limiter sized via `bridge.yaml`'s `limits.manifest.requestsPerMinute` (default `6`) and `limits.manifest.burst` (default `3`). Defaults allow the first 3 calls instant + one call every 10 s sustained — fits a typical iOS paginated scan flow.

On exceeded, the server responds:

- Status `429 Too Many Requests`
- Header `Retry-After: <seconds>` derived from the limiter's reservation delay (the honest cooldown)
- Body `{"error": "rate_limited", "message": "too many manifest requests; retry after the Retry-After window"}`

Clients SHOULD respect `Retry-After` rather than retrying immediately. iOS surfaces 429 as a generic transport error today; typed handling with Retry-After parsing is a Mirror-PR follow-up.

Operators can disable the limiter by setting `limits.manifest.requestsPerMinute: 0` in `bridge.yaml`.

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

1. **Admin console** — the dashboard's "Updates" tile shows current/available versions, last check time, and (when an update is available) an **Install & restart** button that downloads the archive, verifies SHA-256 from `checksums.txt` (and on macOS, codesign + notarization + Team-ID equality), atomically swaps the binary, arms a rollback marker, and triggers restart. Active downloads (`/v1/read` + `/v1/download` requests in flight) block install with a 409; an "Install anyway" affordance is available with explicit confirmation copy. Windows is supported alongside darwin/linux: when the bridge runs as an SCM service, the swap path stops the service before the rename and restarts it after; non-elevated Windows installs (Startup-folder layout) just rename without the SCM dance. The rename-trick works on Windows because `MoveFileEx(MOVEFILE_REPLACE_EXISTING)` is allowed against a running .exe — the kernel image-loader keeps the running process pointed at the old bytes while the directory entry is updated.
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

**MinClientVersion compat gate** (implemented): on each install attempt, the bridge fetches a `release-meta.json` sidecar published alongside the GitHub Release archive — `{"version": "...", "minClientVersion": "...", "protocolVersion": ...}`. If any paired token's last-seen `X-Client-Version` is strictly below the candidate's `minClientVersion`, the auto-installer refuses with `ErrCompatGateRefused` and surfaces the reason in `/api/updates.deferredReason` (rendered as a yellow "deferred" line on the dashboard). Tokens that never sent `X-Client-Version` (older iOS builds) are skipped — refusing every install on their behalf would mean the gate never opens until they update.

Releases that don't ship `release-meta.json` (any pre-Phase-C build) are treated as "no floor" — the gate stays permissive so legacy releases keep installing.

The manual `bridge update --override-client-floor` flag bypasses the gate; the auto-installer never sets it.

**Trust boundary:** the install endpoint is admin-loopback only (same trust model as every other admin route — see `internal/admin/admin.go` doc comment). There is intentionally no remote-trigger install from iOS. Operators install at the bridge host. The iOS app's role is awareness.

### Client version reporting

The iOS app sends an additive request header on every authenticated request:

```http
X-Client-Version: <CFBundleShortVersionString>
```

Example: `X-Client-Version: 1.2`. The bridge persists the most recent value per token (`auth.Token.LastClientVersion` in `<dataDir>/tokens.json`) so the updater can later refuse an auto-install whose `MinClientVersion` would orphan a still-active iOS build that hasn't shipped through the App Store yet.

The header is optional — older iOS clients that don't send it continue to authenticate normally. Bridges record nothing for those tokens, and the compat gate skips "version unknown" tokens rather than blocking on their behalf (see the **MinClientVersion compat gate** subsection above).

## Operator: TLS cert rotation

The bridge mints a self-signed ECDSA P-256 cert on first run with a **397-day validity window** (capped under Apple ATS's 398-day enforcement; an older cert is rejected at the iOS TLS handshake before fingerprint pinning can override it). Rotation is therefore an annual event — operators see a startup log warning at ≤30 days and the admin console / `bridge cert info` surface a yellow / red badge well before the cliff.

- **Inspect**: `bridge cert info` prints subject, fingerprint, not-before / not-after, and days-until-expiry. The admin dashboard shows the same fields under the "TLS fingerprint" panel and surfaces a yellow / red badge at ≤30 / ≤7 days respectively.
- **Rotate**: `bridge cert rotate [--yes]` regenerates both the cert and the key, prints the new fingerprint, and points the operator at the re-pair flow. Rotation is gated on a `--yes` confirmation prompt because **every paired device must re-pair**: iOS pins the cert fingerprint, and the new cert has a different fingerprint even if the key were preserved (the cert binary differs by serial number + NotBefore / NotAfter alone).

After `bridge cert rotate`, the operator must:

1. Restart the bridge service (`bridge serve` rereads cert + key from disk on startup; the running process keeps using its in-memory cert until it exits).
2. Re-pair every device. The admin console's existing "Pair new device" and per-token "Rotate" flows both emit fresh QR codes carrying the new fingerprint — no separate "fresh URL for everyone" button is needed because every URL is built from the live cert.

The CLI also surfaces `GET /api/cert` to the admin console (subject, fingerprint, NotBefore / NotAfter, daysUntilExpiry). Cert rotation itself stays CLI-only by design — exposing it via the admin console would mean a one-click action that strands every paired device, and the operator-restart step has no good web equivalent.

## Operator: Token lifecycle

`bridge pair --name <device>` mints a fresh 256-bit bearer token. Beyond that, the token surface supports rotation, expiry, and revocation:

- **List**: `bridge token list` — prints every paired token with ID, name, created/last-used timestamps, rotation marker, and expiry.
- **Rotate**: `bridge token rotate <id>` — replaces the raw bytes; ID, name, and CreatedAt are preserved; the previous raw token stops validating immediately. The device must scan a fresh pair URL (or paste the new raw token) to reconnect. The admin console's "Rotate" button does the same and emits a QR + pair URL inline.
- **Expire**: `bridge token expire <id> --in <duration>` (e.g. `24h`, `2160h` ≈ 90 days) sets a hard cutoff. `--clear` removes an existing expiry. The admin console's "Expiry…" button accepts the same shorthand. Validate rejects expired tokens; the iOS device gets a 401 and is expected to re-pair.
- **Revoke**: `bridge token revoke <id>` (or admin "Revoke" button) permanently deletes the row. Re-paring needs a fresh `bridge pair`.

The corresponding admin endpoints are:

- `POST /api/tokens/{id}/rotate` — emits the same `pairResult` shape as `POST /api/tokens` (raw, ID, fingerprint, pair URL, QR data URL).
- `PATCH /api/tokens/{id}` — body `{"expiresAt": "<RFC3339>"}` to set, `{"expiresAt": null}` to clear. Other fields ignored (reserved for future lifecycle work).

ProtocolVersion is unchanged for this surface — it's purely server-side + admin-UI work, no on-the-wire client contract to bump.

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
