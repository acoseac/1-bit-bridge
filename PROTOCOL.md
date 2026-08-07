# 1-bit Bridge Wire Protocol — v1

This is the **source of truth** for the wire contract between the `1-bit-bridge` server and the `1-bit` iOS app. A verbatim copy is mirrored into the iOS repo at `com.acoseac.dsdplayer/docs/BridgeProtocol.md`; any change here must land in both places in the same PR cycle (see [CONTRIBUTING.md](CONTRIBUTING.md) — "Mirror-PR rule").

## Versioning

- **Protocol version**: `1`.
- Every response carries the header `X-Bridge-Protocol: 1`.
- `GET /v1/health` returns the current protocol version and the server's build version (see below).
- iOS checks `protocolVersion` at pairing and on each session's first request. A mismatch surfaces a clear error and refuses to connect rather than risk silent misbehavior.
- **Breaking wire changes bump `protocolVersion`.** Additive, backward-compatible changes (new optional fields, new endpoints) stay at the same version.

## Transport

- **HTTPS only**, HTTP/2 and HTTP/3 (QUIC) preferred. No plaintext HTTP endpoint.
- Server mints a self-signed certificate on first run (see `internal/tls`); iOS pins by the SHA-256 fingerprint captured during pairing. A public-CA cert is also supported if configured.
- **HTTP/3 Discovery**: Server advertises HTTP/3 capability via the `Alt-Svc` header on HTTP/1.1 and HTTP/2 responses. Clients supporting QUIC should upgrade future connections to the advertised UDP port.
- Path segment: all endpoints are prefixed `/v1/`.

## Discovery (mDNS / Bonjour)

The bridge advertises on the local network as `_onebit-bridge._tcp` (DNS-SD / Bonjour) so clients auto-discover it on a LAN. The advertisement carries A/AAAA + SRV records plus a TXT record with these keys:

| Key | Since | Meaning |
|---|---|---|
| `pv` | v1 | Advertised `ProtocolVersion` (decimal). Clients may refuse an incompatible value before attempting a TLS handshake. |
| `host` | v1 | The `<short-hostname>.local` SRV target; lets the client build `https://<host>:<port>` directly without a separate Bonjour hostport resolution. |
| `port` | v1 | TCP port the v1 API listens on (decimal, 1–65535). |
| `library` | v1 | Operator-set library name (omitted when blank). |
| `ips` | v1 (additive) | Comma-separated **bare** routable IP literals (IPv6 unbracketed) — the same interface-filtered set the A/AAAA records carry, restricted to **global-unicast** (so **link-local is excluded**: IPv6 `fe80::/10` needs a zone index the client can't map; IPv4 `169.254.0.0/16` is an APIPA fallback). Lets the client race direct connections at discovery time and skip slow/flaky `.local` resolution, falling back to `host` when the key is absent. The client builds `https://<ipv4>:<port>` for IPv4 and the bracketed `https://[<ipv6>]:<port>` for IPv6. Capped to fit the 255-byte TXT-string limit, so a client MUST tolerate a truncated or absent list. |

mDNS is a LAN convenience only — once paired, the client stores a durable endpoint (Tailscale IP or public hostname) for remote access. Discovery-time connections still pin the TLS fingerprint captured at pairing; the `ips=` literals present the same leaf certificate, so pinning is transport-agnostic.

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
    "isScanning": false,
    "pendingDeletions": 0
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
  "certNotAfter": "2027-05-26T10:15:04Z",
  "leCertNotAfter": "2026-08-22T12:00:00Z",
  "roots": [
    { "name": "Music", "reachable": true },
    { "name": "Audiobooks", "reachable": false, "reason": "offline" }
  ],
  "pushEventsSupported": true,
  "pairingEventsSupported": true
}
```

The four `latestServerVersion` / `updateAvailable` / `updateReleaseNotesURL` / `minClientVersion` fields are an additive extension landed in bridge 0.1.0; they are populated only when the bridge has an updater configured (it polls the GitHub Releases API in the background) and at least one successful poll has cached a result. All four are `omitempty` on the wire — older bridges ship the response without them, and iOS clients MUST tolerate their absence. `updateAvailable` is a JSON boolean carried as a `*bool` server-side, so once a poll has cached a result it is present as an explicit `true` **or** `false` (a "checked, up to date" result is `false`, not an omitted field); it is omitted only when no updater is configured. Clients should decode it as an optional boolean and treat absence as "unknown / not advertised".

`upscaleEnabled` (additive since v1.2, `*bool` with `omitempty`) reports whether the bridge has the offline PCM-upscaling feature enabled in `bridge.yaml` AND a working `sox` binary available on PATH (a misconfigured server with the flag on but no sox advertises `false` here — graceful degradation). iOS uses this single capability flag to gate every variant-related UI surface for that bridge: when `false` (or the field is absent on a pre-v1.2 bridge), the picker, glyph, and "Generate upscaled" context menu items are all hidden — bridge rows in the library look identical to SMB / local rows, so the user sees no functionality they can't use. Operator opt-in is in `bridge.yaml`'s `upscale.enabled: true`; default is off. See "Upscaling (offline PCM variants)" below for the wire shape on `/v1/manifest` and `/v1/download` when the flag is on.

`scanState.tracksIndexed` counts the bridge's **served** tracks — the same population `/v1/manifest` returns. Since the server-side duplicate filter (bridge v0.1.9, `duplicates.filter`, on by default), copies suppressed as duplicates are excluded here exactly as they are excluded from the manifest and its `total`, so a client comparing this number against its synced library is comparing like with like. This changed the VALUE for libraries containing duplicates, not the field's shape or presence.

`scanState.pendingDeletions` (additive since v1.2.x, `omitempty`) is the count of tracks + folders rows the scanner has marked missing across recent scans but hasn't yet reached the configured delete threshold for (default 3 consecutive missing scans, configurable via `scanner.deleteAfterMissingScans` in `bridge.yaml`). A non-zero value means the scanner is granting those rows a grace period before reaping — protecting against silent-empty-enumeration failure modes on flaky network mounts (SMB re-auth flap, NFS brownout, libsmb2 timeout returning an empty Readdir). A steadily climbing value indicates a real connectivity problem the operator should investigate. Pre-v1.2.x bridges (created before migration v5) omit the field; iOS and admin UIs treat absence as 0.

`roots` (additive since v1.2, `omitempty`) is the per-library-root reachability snapshot. Each entry's `name` is the root's basename (matches the entry rendered at the top level of `/v1/list`). `reachable` is true when the bridge most recently saw the root's filesystem path respond to a `stat` within the bridge-side probe budget (~2 s). When `reachable` is false, `reason` is a stable machine-readable code: `"offline"` (timeout / unclassified I/O error), `"not_mounted"` (ENOENT on the path), or `"permission_denied"` (EACCES). New reason values may appear over time; clients SHOULD treat unknown reason codes as a generic offline indicator. Probes are server-side TTL-cached (~5 s) so an aggressive `/v1/health` poll cadence doesn't re-stat every network mount. iOS uses this to surface a "Library X offline" hint without paginating `/v1/list`.

`certNotAfter` (additive since v1.2, `*time.Time` with `omitempty`) is the **on-disk self-signed** TLS certificate's `NotAfter` (UTC) — i.e. the cert iOS captures + pins at pairing time and uses for LAN, mDNS, IP-literal, and Tailscale-IP SNI handshakes. iOS uses it to surface a "Bridge cert expires in X days — re-pair to refresh" warning before the cert actually expires and TLS handshakes start failing at Apple's ATS layer (Apple's 397-day cap means operators must re-pair roughly annually). Pre-v1.2 bridges and bridges where cert parsing failed at startup omit the field; iOS treats absence as "no expiry info, never warn".

`leCertNotAfter` (additive in the v0.1.3 follow-up, `*time.Time` with `omitempty`) is the **public-domain Let's Encrypt** certificate's `NotAfter` (UTC) — i.e. the cert served on the operator's autocert domain when iOS dials `https://<autocert.domain>/`. Populated only in public mode (autocert enabled + cert minted on disk); loopback bridges and pre-autocert servers omit the field. Distinct from `certNotAfter` because the two certs follow different rotation schedules (self-signed: Apple's ~397-day cap; LE: ~90 days) and the operator needs both expiries to plan correctly. iOS / operator tooling reads this field live per probe (so background autocert renewals surface without restart) and treats absence as "no LE cert" (= LAN-only bridge).

`minClientVersion` advertises the iOS app version *this bridge* needs from its clients (build-time-injected via `-ldflags -X .../version.MinClientVersion=…`). iOS uses it to surface "your app may be too old for this bridge" hints. It is NOT the floor a *candidate update* would require — that's a Phase B install-side concern and is delivered through admin-console / CLI surfaces, not `/v1/health`.

`endpoints` (additive since v1, optional, may be absent or empty) is the full list of URLs the server is currently reachable at — LAN IPv4/IPv6 (global unicast only; link-local is filtered because it's not reachable across devices), the `<hostname>.local` mDNS form, and any Tailscale interface (CGNAT `100.64/10` for v4, `fd7a:115c:a1e0::/48` ULA for v6). Clients use this to learn new alternates at heartbeat time so they can roam between LAN and Tailscale without re-pairing.

`pushEventsSupported` and `pairingEventsSupported` (both additive since v1.3, `*bool` with `omitempty`) are capability flags advertising that this bridge implements the SSE streams documented at `/v1/events` (`upscale.*` + generic push events) and `/v1/pairing/{id}/events` (per-pairing-request push) respectively. iOS uses them to decide whether to subscribe via SSE or fall back to the polling endpoints — pre-v1.3 bridges omit both fields, and iOS treats absence as "polling only". Splitting the two flags means a bridge can support generic push without pairing push (or vice versa) as separate rollout vectors.

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

`reachable` / `reason` (additive since v1.2, both with `omitempty`) appear ONLY on the synthetic root-level entries returned in multi-root mode for an empty `path` query. Ordinary directories and files inside a root omit both fields. `reachable` is a `*bool` (the zero-value-omits-`false` rule means pointer is needed to send explicit `false`). When `reachable` is `false`, `reason` carries the same stable code set as the `roots` block of `/v1/health` (`"offline"`, `"not_mounted"`, `"permission_denied"`). Pre-v1.2 iOS ignores the field; iOS 1.2+ uses it to render a "library offline" hint instead of inferring from a silent zero-size row.

### `GET /v1/stat?path=<rel>`

Single-entry stat. Replaces `SMBConnectionPool.stat`. Used by the iOS scanner's folder-mtime skip logic.

**Response** (`200 OK`, JSON):
```json
{ "mtime": "2026-04-20T12:00:00Z", "isDir": true, "size": 0 }
```

`reachable` / `reason` (additive since v1.2, both with `omitempty`) populate when the requested path identifies a configured library root. On a healthy root, `reachable: true` is emitted (no `reason`). On an unreachable root, the server responds `200 OK` with `reachable: false` + the relevant `reason` instead of falling through to a 404 — this lets iOS distinguish "library is offline" from "user typed an unknown path", which the pre-v1.2 shape conflated. Descendants of a root (file or subdirectory inside a library) omit both fields.

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

The manifest describes the bridge's **served set**: when the server-side duplicate filter is active (`duplicates.filter`, on by default since v0.1.9), the non-winning copies of a duplicate group are excluded from `tracks`, from `total`, and from `enrichmentProgress.tracksTotal` — the counts always agree with the rows a client can actually fetch. Suppressed copies remain fully downloadable by path (`/v1/download`, `/v1/read`, `/v1/stat`) so clients holding older manifests keep working, and a copy that stops being suppressed re-enters delta syncs via an `indexed_at` advance. This is a serving decision, not a wire change: the shape is unchanged and `protocolVersion` stays 1.

Query parameters:
- `since` (optional): if set, only tracks whose server-side **`indexed_at` watermark** advanced past `since` are included (folders filter on their `mtime`). This is deliberately NOT file mtime — enrichment, metadata reconciliation, variant writes, and duplicate-suppression lifts all strict-advance `indexed_at` precisely so incremental syncs surface them without the file changing on disk. (An earlier revision of this document said "mtime > since" for tracks; that was a documentation error — the behaviour has always been the indexed watermark.) If unset, the full manifest is returned.
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
      "artworkVersion": "...",
      "artistMBID": "...",
      "enriched": true,
      "composer": "Ludwig van Beethoven",
      "conductor": "Herbert von Karajan",
      "work": "Symphony No. 5 in C minor, Op. 67",
      "originalYear": 1808,
      "bpm": 108
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

#### Deriving per-track enrichment state (no new field, since v1.1)

A client renders a per-track "pending / matched / no-cover" badge purely from the two fields the manifest already carries — there is **no dedicated state field on the wire**, and none is planned:

| `enriched` | `artworkMBID` | State | Meaning |
|---|---|---|---|
| `false` (or absent¹) | — | **pending** | still queued for the enricher; a cover may still appear |
| `true` | present | **matched** | a cover is cached and served by `/v1/artwork/{mbid}` |
| `true` | absent | **missing** | enriched, but no cover was found anywhere (MB / CAA / CAA-release-group / iTunes all missed, or there was no MB match) — a persistent coverage gap |

¹ Absent `enriched` (a pre-v1.1 server) is treated as "fully enriched" per the back-compat rule above, so those tracks resolve to **matched** / **missing** by `artworkMBID` presence — never **pending**. A `local-<sha256>` `artworkMBID` (a curated `cover.jpg` / embedded APIC) is a present value, so it reads as **matched**.

**A transient failure is deliberately indistinguishable from pending.** On an upstream outage the bridge leaves `enriched` at `false` (the `IsTransient` guard) precisely so the row is retried — so a client cannot, and should not try to, tell "still queued" apart from "failed transiently, will retry" from the manifest alone. Both present as **pending** and both self-heal. Operators who want the pending / matched / missing *counts* for the whole library read them from the loopback admin console (`GET /api/enrichment`), which derives the same split server-side.

#### Manifest-level `enrichmentProgress` (additive, since v1.1)

`enrichmentProgress` is a snapshot of library-wide enrichment status at manifest-build time:

- `tracksTotal` — total track count in the store. Same number as the top-level `total` in paginated mode; included redundantly here for convenience on non-paginated full-manifest responses (where `total` is absent).
- `tracksEnriched` — number of tracks past the enrich pass (`enriched_at != 0`).
- `lastEnrichedAt` — UTC wall-clock of the most recent successful `MarkEnriched` call across the whole library. Absent (omitempty) when no track has ever been enriched.

iOS uses this to render an "Enrichment in progress (X / Y)…" footer and gates the suppression-of-negative-cache behaviour on a 24h freshness check on `lastEnrichedAt` — a bridge that went idle a week ago shouldn't make the iOS UI claim enrichment is "still happening".

**Pagination behaviour.** `enrichmentProgress` is populated only on the **first page** of a paginated full-manifest response (request has no `cursor`) and on every non-paginated response. Same first-page-only convention as `folders` / `total` — the values are stable across a pagination run, so iOS snapshots them off the first page and ignores any later pages.

Both fields are additive — `ProtocolVersion` stays at `1`. Pre-v1.1 servers omit them; pre-v1.1 clients ignore them.

#### Classical metadata (additive, since v1.3)

`composer`, `conductor`, `work`, `originalYear`, and `bpm` are populated by the bridge's extractor when the source tags carry them. All five are `omitempty` — pre-v1.3 bridges emit nothing here and iOS treats absence as "no classical metadata". String fields (`composer` / `conductor` / `work`) ship the tag value verbatim after the standard display-name normalization; integer fields (`originalYear` / `bpm`) ship as `*int` so a zero-valued source can be distinguished from an unset one.

iOS surfaces these on a third subtitle line in track rows ("Composer: X" with conductor fallback) and as a "Conducted by X" header on albums where ≥80% of tracks share a conductor. Source-tag coverage: ID3v2 `TCOM` / `TPE3` / `TIT1` / `TORY` / `TDOR` / `TBPM`; Vorbis-comment `COMPOSER` / `CONDUCTOR` / `WORK` / `ORIGINALDATE` / `BPM`; MP4 `©wrt` / `cond` / `©wrk` / `BPM`. The bridge tag layer is the single source of truth — iOS no longer extracts classical fields from local sources on bridge shares.

#### Multi-value artist preservation (additive, since v1.3)

Source-tag multi-value `ARTIST` / `ALBUMARTIST` (FLAC Vorbis arrays, MP4 raw `[]string`) are preserved by the extractor and serialised as a `; `-joined scalar string on the existing `artist` / `albumArtist` fields. The wire shape stays scalar; the iOS app splits on `; ` and exposes a sub-menu picker on multi-artist rows. Pre-v1.3 extractors silently collapsed multi-value tags to whichever single value the underlying library returned first; the v1.3 behavior is backwards-compatible (single-artist sources are unchanged).

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

id: 43
event: upscale.complete
data: {"path":"Music/Diana Krall/Live in Paris/01.flac","variantId":"upscaled-v1-176400-24","sampleRate":176400,"bitsPerSample":24,"completedAt":"2026-05-15T11:40:12Z"}

event: pairing.abc123-def456
data: {"status":"approved","ttlSecondsRemaining":0,"bridgeStartedAt":1735689600123}

event: heartbeat
data: {}

event: dropped
data: {"missed":3}
```

**Topics**:
- `upscale.stats` — fires whenever the bridge's upscale state changes (job queued, completed, failed). Payload matches `/v1/upscale/stats`.
- `upscale.complete` (additive, since v1.3) — fires once per successful variant generation, immediately after the bridge has `UpsertVariant`-committed the row. Payload carries the library-relative source `path`, the resulting `variantId`, the variant's `sampleRate` + `bitsPerSample`, and a `completedAt` UTC timestamp. iOS uses this to reconcile in-flight wand state within ~1 s of the variant landing on disk instead of waiting for the next manifest poll. Failure paths still fire `upscale.stats` (the `failed` counter bumps); `upscale.complete` is success-only by design — `upscale.failed` is a deliberate follow-up.
- `pairing.<requestID>` — fires on `Approve` or `Decline` of the named request. Payload matches `/v1/pairing/<requestID>` for the relevant state **with the secret fields stripped**: `token`, `tokenId`, and `verificationCode` never appear on this shared bearer-authed bus — any paired device can subscribe to `?topics=pairing`, so the minted token is delivered exclusively via the pollSecret-gated `/v1/pairing/{requestId}` poll and its SSE sibling `/v1/pairing/{requestId}/events`. Clients that need the token (the pairing device itself) must use those; bus subscribers only learn that a transition happened.
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

**Response** (`202 Accepted`): the server has seen the MBID in a track AND enrichment for it is still pending — at least one track carrying the MBID has not been processed by the enricher yet (cold cache on first scan, background re-fetch in progress). Carries `Retry-After: <seconds>` (30 s today); clients SHOULD retry with jittered backoff up to a small cap (iOS uses 5 attempts). Body is the standard JSON error shape with `error: "pending"`.

**Response** (`404 Not Found`, `error: "no_image"`): the MBID is known but enrichment has COMPLETED for every track carrying it and no image was found upstream (Deezer has no portrait for the artist / CAA + iTunes have no cover for the release). Terminal — clients SHOULD render a placeholder and stop retrying; a later re-scan or forced re-enrichment (`enriched_at = 0`) flips the answer back to 202 if the upstream ever gains an image. Added 2026-08-06; before this, the known-but-imageless state answered `202` forever and retry-capped clients paid a full backoff ladder per item per sync.

**Never returned for a `local-<sha256>` artwork MBID.** Those bytes are extracted by the server's library scanner (embedded cover art, or a `cover.jpg` / `folder.jpg` beside the audio file), not fetched from any upstream, so "enrichment complete" carries no information about them — and the scanner restores a missing one on its next pass. A cache miss on a `local-` MBID is therefore always `202`.

**Response** (`404 Not Found`, `error: "not_found"`): the image is not cached under the requested key. Either the MBID is unknown — no track in the manifest references it — or the request asked for a `size` this server has not cached: covers are written at **500 px only**, so `size=250` / `size=1200` can miss for a release whose cover IS cached. Clients SHOULD treat it as terminal *for that key* and render a placeholder rather than re-requesting the same URL; a client that asked for an off-500 size MAY retry once at `size=500`.

**Response** (`400 Bad Request`): MBID is not a valid UUID, or `size` parameter is out of range.

Backwards compatibility: the 202 branch is a v1.1 addition; the `no_image` terminal split is additive on top of it. Servers that don't have an MBID probe wired (e.g. tests, legacy) fall back to 404 on any cache miss, matching the v1.0 behavior. No protocol version bump is required — iOS clients that ignore 202 still work (they just see a transient-looking failure), and clients have treated 404 on these endpoints as terminal since v1.0, so the pending→no_image reclassification only stops futile retries.

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

### Waveform (offline audio analysis, additive since v1.8)

An opt-in offline analysis feature that pre-computes a compact **peak-envelope waveform** per track (so the iOS player can render a scrubber waveform without decoding the file on-device) and, since v1.8, a **signal-derived loudness** value. Disabled by default; operators enable it via `analysis.enabled: true` in `bridge.yaml` and run `bridge analyze` on the server to populate the cache. Like upscaling, analysis decodes through `sox(1)` and is always offline — it only **reads** samples to derive metadata and never alters what `/v1/download` serves (the original streams byte-for-byte). DSD sources (`.dsf` / `.dff`) are skipped (sox can't decode 1-bit DSD).

Advertised via the `waveform` flag in `/v1/health.features` (plus `loudness` when loudness measurement is active — see below). Pre-feature bridges omit the flags, omit `Track.waveformTag`, and return `404` from `/v1/waveform` — iOS treats all of these identically (no bridge waveform → it falls through to on-device analysis, rendering the per-track synthetic dust shape until that lands).

#### `Track.waveformTag` (manifest)

When the feature is enabled AND the bridge has a cached waveform for a track, that track's `Track` entry in `/v1/manifest` carries an additional string field:

```json
{ "path": "Music/Album/01.flac", "size": 12345678, "mtime": "…", "waveformTag": "3b42801f" }
```

`waveformTag` is `omitempty` (absent when no waveform is cached, or the feature is off). Its value is a short **content tag** — the first 8 lowercase hex of the sidecar bytes' SHA‑256. iOS uses it as the cache key when fetching `/v1/waveform`: a regenerated waveform (the source file was edited) yields a new tag, so a client caching by tag re-fetches automatically. Writing a waveform bumps the parent track's `indexed_at`, so the new `waveformTag` surfaces on the next `/v1/manifest?since=` delta sync (only when the value actually changed — an identical recompute is a no-op).

#### `GET /v1/waveform?path=<rel>` (bearer-authenticated)

Returns the binary waveform sidecar for the track at `path` (the same library-relative form `/v1/download` accepts; iOS-shaped lowercase/leading-slash paths resolve case-insensitively).

- `200 OK`, `Content-Type: application/octet-stream`, `ETag: "<waveformTag>"`, `Cache-Control: private, no-cache`. The request carries **no content tag** — `path` is its only key — so re-analysis rewrites the body under a stable URL. `no-cache` means "store it, but revalidate", so a client that already holds this sidecar pays one conditional request (`If-None-Match: "<waveformTag>"`) and gets a `304 Not Modified`. Corrected 2026-08-06: this previously advertised `max-age=31536000, immutable`, which instructs a conforming client never to revalidate — pinning a stale waveform and making the `ETag` unusable.
- `400 bad_request` — missing/invalid `path`.
- `404 waveform_not_found` — analysis disabled, or no waveform cached for this track yet.
- `410 waveform_stale` — the source file drifted (mtime beyond a 2 s tolerance, or size changed) since the waveform was computed; the client falls through to on-device analysis until re-analysis catches up. `410 waveform_missing_on_disk` — the row exists but the sidecar file is gone (manual wipe / partial GC); same client handling.

##### Sidecar binary format (`1BWF`)

A 22‑byte little-endian header followed by `count` × `[int8 min, int8 max]` peak pairs:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | magic `"1BWF"` |
| 4 | 1 | format version (`1`) |
| 5 | 1 | flags (`0`) |
| 6 | 4 | `uint32` sample rate (Hz; `48000`) |
| 10 | 4 | `uint32` samples per bucket (`4800` = 0.1 s) |
| 14 | 4 | `uint32` bucket count `N` |
| 18 | 4 | `uint32` duration (ms) |
| 22 | 2·N | `N` pairs of `int8` (min, max), each in `[-127, 127]` |

Buckets are fixed-width in time (0.1 s); map bucket `i` to time `(i / N) × duration`. Peaks are decoded at 48 kHz mono and quantised symmetrically to `int8`. A typical 4-minute track is ~5 KB. The format version in the header bumps if the layout changes; a client that doesn't recognise the version should ignore the sidecar.

#### Signal-derived loudness — `replayGainTrackDB` (additive, since v1.8)

The same offline pass also measures **EBU R128 / ITU‑R BS.1770‑4 integrated loudness** — channel-aware (decoded at the source channel count, not a mono downmix, which would read several dB hot) — and converts it to a **ReplayGain 2.0 track gain** in dB (the gain that brings the program to the −18 LUFS reference). The bridge surfaces it in the **existing** `Track.replayGainTrackDB` manifest field (no new field, no `ProtocolVersion` bump) — populated from analysis **only when the source file carries no ReplayGain tag of its own**, so curated tags always win. A value present in the manifest is therefore either tag-curated or analysis-derived; the two are indistinguishable on the wire by design. The client applies it as playback gain exactly as it always has — never a bit change to the served file.

Advertised via the `loudness` flag in `/v1/health.features`, present alongside `waveform` when analysis is active. Loudness is computed in the same decode as the waveform, so a track gains its `replayGainTrackDB` when it is (re)analyzed; the write bumps the parent track's `indexed_at` so the value surfaces on the next `/v1/manifest?since=` delta (only when it actually changed). It is **skipped — and the field left tag-only —** for silent programs and for sources whose channel layout `sox` can't report. A bridge upgraded to v1.8 re-analyzes its existing waveform cache once to backfill loudness; because the peak envelope is byte-identical to the prior decode, `waveformTag` is unchanged and clients re-fetch no sidecars — only the new scalar syncs.

#### Estimated key + tempo — `keyRoot` / `keyMode` / `bpm` (additive, since v1.8)

The same offline decode also estimates the **musical key** (Krumhansl-Schmuckler correlation over a midrange chroma) and the **tempo** (spectral-flux onset envelope → autocorrelation, with a perceptual prior that suppresses octave errors). Both are **best-effort estimates** — the client should label them "estimated".

Two new `Track` fields carry the key:

```json
{ "path": "Music/Album/01.flac", "size": 12345678, "mtime": "…", "keyRoot": 7, "keyMode": "major", "bpm": 120 }
```

- `keyRoot` is the tonic as an integer **0–11 with C=0** (0=C, 1=C♯/D♭, … 9=A, 11=B); `keyMode` is `"major"` or `"minor"`. There is no curated key tag today, so these are **analysis-only** — present only when the bridge estimated a key, absent (`omitempty`) otherwise.
- `bpm` is the **existing** tempo field. It stays tag-first: a curated TBPM / BPM / `tmpo` tag always wins; the estimated tempo is spliced in **only when the source has no BPM tag**. A present value is therefore tag-curated *or* estimated, indistinguishable on the wire (same contract as `replayGainTrackDB`).

Advertised via the `keyTempo` flag in `/v1/health.features`, present alongside `waveform`/`loudness` when analysis is active. Key + tempo are computed in the same decode as the waveform + loudness; a track gains them when (re)analyzed, and the write bumps `indexed_at` so they surface on the next delta (only when changed). Both are **skipped** for inputs with too little signal (very short, atonal, or arrhythmic) rather than guessed. A bridge upgraded to v1.8 backfills key/tempo on the same one-time re-analysis as loudness — the waveform bytes and loudness are unchanged, so clients re-fetch no sidecars and only the new scalars sync. `ProtocolVersion` stays `1`.

#### Track quality — `truePeakDB` / `drScore` / `audioMD5State` (additive, since v1.9)

The same offline decode also measures two dynamics scalars, and FLAC sources get an integrity verification pass. Three new `Track` fields, all additive `omitempty` (`ProtocolVersion` stays `1`):

```json
{ "path": "Music/Album/01.flac", "truePeakDB": -0.4, "drScore": 12, "audioMD5State": "verified" }
```

- `truePeakDB` is a **BS.1770-style true peak** in dB relative to full scale: 4× polyphase-oversampled intersample peak detection, measured on the bridge's **48 kHz analysis rendering** (the analysis pipeline's one-decode invariant — a track-level statistic, honestly derived but not a native-rate measurement; the client's own live meter measures the native stream during playback). Values above `0.0` flag a master with intersample overs. Absent for silence.
- `drScore` is the **community DR value** (the Pleasurize Music Foundation / TT DR Offline Meter convention — "DR12"): per-channel 3-second blocks, the dB distance between the second-highest block peak and the energy-averaged RMS of the loudest 20 % of blocks, averaged across channels and rounded. Absent for programs under ~9 s or silence.
- `audioMD5State` is the **FLAC audio-checksum verification** result: `"verified"` when a native-depth decode of the audio hashes exactly to the MD5 the encoder stored in STREAMINFO; `"mismatch"` when a clean, complete decode hashed differently — the audio no longer matches what its encoder checksummed (file modified or corrupt; some tag editors rewrite FLAC without updating the checksum, so a mismatch is a flag, not proof of corruption). Absent when not verifiable: non-FLAC sources, a zeroed (unset) STREAMINFO checksum, uncommon bit depths (12/20-bit), or decode-tool failure — verification **never** reports `mismatch` from a failed or truncated decode.

Advertised via the `trackQuality` flag in `/v1/health.features`, present alongside `waveform`/`loudness`/`keyTempo` when analysis is active. All three ride the same analysis pass and surface via `indexed_at` deltas like the other scalars. A bridge upgraded to v1.9 re-analyzes once to backfill (the `wf4` schema stamp); waveform bytes, loudness, key and tempo are unchanged, so clients re-fetch no sidecars — only the new scalars sync.

#### File provenance — `Track.bandwidthHz` + `GET /v1/spectrum` (additive, v1.10)

A whole-track, time-averaged **frequency spectrum**, measured in the same decode as the waveform. It answers one question: *is this file genuinely hi-res, or a CD upsampled to look like one?*

```json
{ "path": "Music/Album/01.flac", "size": 12345678, "mtime": "…", "bandwidthHz": 22050 }
```

- `bandwidthHz` is the **highest frequency the file actually carries**, in Hz — the top of the highest FFT bin within 60 dB of the loudest bin, averaged across the whole track. `omitempty`.

**An absent `bandwidthHz` means NO ANSWER — never "no content up there".** Beyond the ordinary cases (analysis hasn't reached the track; it was too short to average), there is one routine and important reason for absence: the bridge decodes at **48 kHz**, so it can only see to **24 kHz**. When a file's content reaches that ceiling, the bridge reports nothing rather than `24000`. It must: 24 kHz is exactly 48 kHz's Nyquist, so a `24000` would let a client conclude "consistent with a 48 kHz source" and accuse a genuinely 96 kHz-native master on the strength of the *bridge's own analysis rate*. A track can therefore carry a spectrum and no `bandwidthHz`, and that pairing is complete, not partial.

This bounds what the field can be used for, and the bound is deliberate: it is sufficient to identify a **CD-sourced upsample** (which cliffs at 22.05 kHz, comfortably inside the ceiling) and insufficient to distinguish 96 kHz-native from 48 kHz-native. Clients must not present an absent value as evidence of anything.

#### `GET /v1/spectrum?path=<rel>` (bearer-authenticated)

Returns the binary spectrum curve for the track at `path` (the same library-relative form `/v1/download` accepts; iOS-shaped lowercase/leading-slash paths resolve case-insensitively).

- `200 OK`, `Content-Type: application/octet-stream`, `ETag: "<8 hex>"`, `Cache-Control: no-cache`. The ETag is over the curve's own bytes. As with `/v1/waveform`, `path` is the URL's only key, so re-analysis rewrites the body under a stable URL — `no-cache` means "store it, but revalidate", and a client holding the curve pays one conditional request for a `304`.
- `404 spectrum_not_found` — analysis disabled, or no spectrum for this track yet.
- `410 spectrum_stale` — the source drifted (mtime beyond a 2 s tolerance, or size changed) since the spectrum was computed.

**Body format — `1BSP`, 80 bytes**, little-endian:

| offset | size | field |
|---|---|---|
| 0 | 4 | magic `1BSP` |
| 4 | 1 | format version (`1`) |
| 5 | 1 | reserved (`0`) |
| 6 | 4 | analysis sample rate, Hz (`48000`) |
| 10 | 4 | STFT windows averaged |
| 14 | 4 | band count (`60`) |
| 18 | 4 | `bandwidthHz`, or `0` for absent |
| 22 | 2 | cliff depth in tenths of a dB, `0xFFFF` for absent |
| 24 | 60 | one band per byte: dB **below** full scale, floored at 90 |

The **60 bands are log-spaced from 20 Hz to the analysis Nyquist** by the same construction the iOS live meter uses for its own bars, so the two are drawn on one axis. **A client MUST position the curve using the sample rate in the header, not its own file rate** — the bridge's bands span 20 Hz → 24 kHz regardless of the file's rate, so a 96 kHz file's curve covers only the lower half of a 48 kHz axis.

Advertised via the `spectrum` flag in `/v1/health.features`, present alongside `waveform` when analysis is active. Pre-feature bridges omit the flag, omit `bandwidthHz`, and return `404` from `/v1/spectrum` — clients treat all three identically. A bridge upgraded to v1.10 re-analyzes its cache once to backfill (the `wf6` schema stamp); waveform bytes, loudness, key, tempo and the quality scalars are all unchanged, so clients re-fetch no waveform sidecars — only the spectrum and `bandwidthHz` are new. `ProtocolVersion` stays `1`.

#### `GET /v1/analysis/stats` (bearer-authenticated)

Authenticated read-only snapshot of the analysis feature, mirroring the admin tile. Cheap (one SQL `COUNT` + a TTL-cached sox precheck).

```json
{ "enabled": true, "soxAvailable": true, "cachedWaveforms": 24518, "cachedBytes": 124857600 }
```

`enabled` reflects live runtime state (config flag AND sox available); `soxAvailable` is omitted when no precheck is wired. There is no live `pool` field — the snapshot surfaces cached totals only. Generation runs via the `bridge analyze` CLI and an automatic serve-side background sweep that picks up newly-scanned tracks on a settle-delay-then-scan-interval cadence. Pre-feature bridges return `404` (the route is unregistered); iOS treats that identically to `{enabled: false}`.

#### Feature gate semantics

- `analysis.enabled: false` (default): `/v1/health.features` omits `waveform`, `loudness`, and `keyTempo`; manifest emits no `waveformTag`, no analysis-derived `replayGainTrackDB`, and no `keyRoot`/`keyMode`/estimated `bpm`; `/v1/waveform` returns `404`.
- `analysis.enabled: true` AND `sox` on PATH: full feature operates as documented (`waveform` + `loudness` + `keyTempo`).
- `analysis.enabled: true` AND `sox` MISSING from PATH: bridge logs `.error` at startup, in-memory disables the feature, omits the `waveform`, `loudness`, and `keyTempo` flags. The rest of the server keeps running.

### Atlas rich-tier metadata (additive — Phase 2)

Optional artist-bio / album-description / genre enrichment from a self-hosted **Atlas** service (`github.com/acoseac/1-bit-atlas`), enabled via `atlas.enabled: true` in `bridge.yaml`. Disabled by default. **The open-source bridge never holds an Atlas credential.** The closed-source 1-bit app holds the Atlas `read:bridge` key, fetches per-entity rich metadata from Atlas, and **ferries it into the bridge**, which caches it (in standalone `release_atlas` / `artist_atlas` SQLite tables, MBID-keyed, never spliced into the manifest) and serves it back to all the user's devices. `ProtocolVersion` stays `1`.

Advertised via the `atlasEnrichment` flag in `/v1/health.features`. Pre-feature bridges omit the flag and return `404` from the routes below.

#### `POST /v1/atlas-ingest` (bearer-authenticated)

The app pushes per-entity metadata. Body carries an optional `release` and/or `artist` object (at least one required):

```json
{
  "release": { "mbid": "<uuid>", "found": true, "description": "…", "recordLabel": "…", "genres": ["…"], "descriptionSource": "bandcamp", "descriptionSourceUrl": "https://…", "atlasEtag": "…" },
  "artist":  { "mbid": "<uuid>", "found": true, "bio": "…", "bioSummary": "…", "genres": ["…"], "bioSource": "wiki", "bioSourceUrl": "https://…", "atlasEtag": "…" }
}
```

`found: false` with empty fields writes a **tombstone** (Atlas was checked and had nothing) so the entity isn't re-queried on every view. The optional **`descriptionSource`/`descriptionSourceUrl`** (release) and **`bioSource`/`bioSourceUrl`** (artist) attribute the winning text to its origin (`wiki` / `bandcamp` / `lastfm` / `tadb` / `qobuz`) so the client can render "Read more on \<source\>" for CC-BY-SA / ToS compliance — additive, older clients omit them. Validation: `mbid` must be a UUID; `description`/`bio`/`bioSummary` ≤ 16 KiB, `recordLabel` ≤ 1 KiB, source name ≤ 64 B, source URL ≤ 2 KiB, ≤ 32 genres of ≤ 256 B each; whole body ≤ 256 KiB. **`ingested_at` is stamped bridge-side** (never taken from the body — the bridge owns its cache-freshness clock). Returns `200 {releaseIngested, artistIngested}`.

#### `GET /v1/atlas-meta/{release|artist}/{mbid}` (bearer-authenticated)

Serves the cached metadata. This is the iOS **Read-Before-Write gate**: the app asks the bridge first and only fetches from Atlas on a miss/stale entity.

```json
{ "found": true, "ingestedAt": "<RFC3339>", "ttlSeconds": 2592000, "description": "…", "recordLabel": "…", "genres": ["…"], "source": "bandcamp", "sourceUrl": "https://…" }
```

(artist responses carry `bio`/`bioSummary` instead of `description`/`recordLabel`.) **`source`/`sourceUrl`** attribute the primary text (the description for a release, the bio for an artist) to its origin — present only when a source contributed; both omitted on a tombstone. `ttlSeconds` is the operator's freshness window (`atlas.metaTtlHours`, default 720 h) — the client treats the row as stale once `now − ingestedAt > ttlSeconds` and re-fetches. **`404`** means the entity was **never checked** (the client then queries Atlas); a **tombstone** returns `200` with `found: false` (checked, nothing — don't re-query until the TTL elapses).

#### `POST /v1/atlas-harvest/credential` (bearer-authenticated — additive, Phase H)

Provisions the bridge-driven **bulk harvest**. The iOS app (which alone holds an App-Attest identity) mints a device-bound `bulk_harvest` token at Atlas and hands it to the bridge here; the bridge persists it locally (0600, never in the manifest DB) and its background harvest client submits the library's artist MBIDs to Atlas, delta-syncs the harvested Tier-1 bios, and caches them in the same `artist_atlas` overlay `GET /v1/atlas-meta/artist/{mbid}` serves. The open-source bridge thus never carries a long-lived Atlas secret of its own — the credential is the user's attested device's, revocable at Atlas.

```json
{ "token": "<atlas bulk_harvest bearer>", "atlasBaseUrl": "https://atlas.example", "expiresInSeconds": 2592000 }
```

`token` is required; `atlasBaseUrl` MUST be an `https` URL (the bridge dials it with the bearer — http is rejected to avoid cleartext token transport); `expiresInSeconds` ≤ 0 (or omitted) means "unknown expiry". **`200`** `{ "ok": true }` on success. Advertised by the bridge accepting the route; gated on `atlas.harvestEnabled` (a bridge with the feature off returns `404 harvest_not_supported`). The harvest client is dormant (cheap idle ticks) until a credential is provisioned.

### PDF album booklets (additive, since v1.8)

Digital booklets for releases, sourced through the operator's Atlas mirror and cached on the bridge. **No provenance is exposed anywhere on this surface — by design.** The wire never names where a booklet came from; clients present booklets as library content.

Advertised via the `booklets` flag in `/v1/health.features` — present only on bridges with the Atlas harvest credential wiring (`atlas.enabled` + `atlas.harvestEnabled`). Pre-feature bridges omit the flag and return `404` from the route below.

#### `Track.bookletTag` (manifest — additive field)

A non-empty `bookletTag` on a manifest track advertises that `GET /v1/booklet/{musicBrainzAlbumID}` serves a PDF booklet for that track's release. The value is an **opaque content tag** for cache-busting: it changes when the booklet's bytes change upstream, and the row's `indexed_at` bumps on any change, so delta-sync (`/v1/manifest?since=`) re-delivers the affected tracks. Keyed by **`musicBrainzAlbumID`** (not `artworkMBID` — a locally-curated cover doesn't preclude a booklet). Omitted (`omitempty`) when no booklet exists; pre-feature clients ignore the field. `ProtocolVersion` stays at `1`.

#### `GET /v1/booklet/{mbid}` (bearer-authenticated)

Serves the cached PDF for the release MBID (strict UUID validation).

- **`200`** — `application/pdf`, served with Range support (`http.ServeContent`), `Content-Disposition: inline`, `Cache-Control: private`.
- **`202` + `Retry-After: 30`** — the booklet is known + available but the bridge's background download hasn't landed it yet. The request also *prioritizes* that release in the download queue, so a client retrying after the window typically succeeds on the first retry. Same pending semantics as `/v1/artwork`.
- **`404`** — unknown release, or no booklet exists for it (also the shape pre-feature bridges return).

Booklets can be 10–64 MB; clients should download once and cache keyed by `(mbid, bookletTag)`.

### Playlist backup (additive, since v1.6; user-wide since v1.7)

Playlist backup. The bridge is a **safe, not a player**: a playlist may mix tracks from several bridges plus local/SMB sources. Items owned by this bridge are stored as resolvable `path`s; items owned by another bridge (or device-local / SMB) are stored as **opaque references** the bridge never resolves or serves — iOS re-resolves them locally on restore against its own shares.

All four routes require the `X-Device-Token` header (the durable recovery token). Advertised via the `playlistBackup` flag in `/v1/health.features`; pre-feature bridges return `404` from these routes.

**Scoping.** Backups are **user-wide**: every paired device belongs to the bridge operator, so any device can list, restore, update, or delete any playlist — recovery is initiable from any of the user's devices, not just the one that backed the playlist up. Bridges with this behaviour additionally advertise the `playlistsCrossDevice` flag in `/v1/health.features` and never emit the `playlist_conflict` 409 described below; pre-flag (v1.6) bridges scope each route to the calling `X-Device-Token` instead (one device never sees another's backups) and may emit it. The device token on a write records **last-writer provenance** (shown in the bridge admin console); a future multi-user mode would re-scope visibility by user.

**`PUT /v1/playlists/{id}`** — upsert. `{id}` is the client's stable lowercase UUID. Body:

```json
{
  "id": "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a",
  "name": "High-Res Favorites",
  "lastModifiedAt": 1730000000000000000,
  "items": [
    { "position": 0, "path": "Pink Floyd/Dark Side/Money.flac" },
    { "position": 1, "originFingerprint": "AB:CD:…:FF", "originPath": "Diana Krall/Live/01 Romance.flac", "title": "Isn't It Romantic", "artist": "Diana Krall" },
    { "position": 2, "originFingerprint": "local", "originPath": "<opaque iOS reference>", "title": "…", "artist": "…" }
  ]
}
```

`lastModifiedAt` is the client's wall-clock **UnixNano (UTC) integer** — the LWW guard key (never a float/string round-trip). Each item sets **either** `path` (local, resolvable) **or** `originFingerprint`+`originPath` (foreign/opaque) — never both, never neither (`400 bad_request` otherwise). `originFingerprint` is the owning bridge's colon-hex cert fingerprint, or the sentinel `"local"` / `"smb"`. `position` is the authoritative 0-based order. Body capped at 16 MiB.

- **`200`** `{ "id": "…", "stored": true }` — accepted (inbound `lastModifiedAt` ≥ stored, or new).
- **`409`** `stale` — inbound strictly older than the server copy; body carries the full server playlist so iOS can reconcile in one round-trip:
  ```json
  { "error": "stale", "message": "server copy is newer", "server": { "id": "…", "name": "…", "lastModifiedAt": 1730000000000000001, "items": [ … ] } }
  ```
- **`409`** `playlist_conflict` — *(v1.6 device-scoped bridges only; never emitted by bridges advertising `playlistsCrossDevice`)* the id exists under a different device (practically impossible with UUIDs; guards against a guessed-id overwrite).

**`GET /v1/playlists`** — summaries across all of the user's devices (caller-scoped on pre-`playlistsCrossDevice` bridges):

```json
{ "playlists": [ { "id": "5d9a…", "name": "High-Res Favorites", "trackCount": 42, "lastModifiedAt": 1730000000000000000 } ] }
```

**`GET /v1/playlists/{id}`** — the full playlist (same shape as the `PUT` body) for restore, regardless of which device backed it up. `404 not_found` if the id is unknown or was deleted (on pre-`playlistsCrossDevice` bridges, also when it's owned by another device).

**`DELETE /v1/playlists/{id}`** — tombstone; propagates the delete to the user's other devices on their next sweep. `200 { "id": "…", "deleted": true }`, or `404 not_found` if no live row matched.

### Playback history (additive, since v1.6; readable feed since v1.7)

Opt-in, owner-visible playback telemetry. Uploads require the `X-Device-Token` header; each event is attributed to the uploading device. History is **user-wide**: it aggregates listening across every paired device and is surfaced in the loopback admin console plus — on bridges advertising the `playbackHistoryRead` flag — the authenticated `GET /v1/history` feed below, so any of the user's devices can read the combined history. It never leaves the operator's host except to the operator's own paired devices. Upload is advertised via the `playbackHistory` flag in `/v1/health.features`; pre-feature bridges return `404`. iOS queues events offline-first and drains them in batches.

**`POST /v1/history/batch`** — bulk-insert events:

```json
{
  "events": [
    {
      "path": "Diana Krall/Live/01 Romance.flac",
      "startedAt": 1730000000000000000,
      "durationUsed": 184.5,
      "codec": "FLAC",
      "variantId": "upscaled-v2-176400-24",
      "outputTarget": {
        "interfaceType": "USB-DAC",
        "deviceName": "Chord Mojo 2",
        "outputRate": 176400,
        "isDoP": true
      }
    }
  ]
}
```

`path` is the file path on this bridge (the original-case `bridgeOriginalPath`); required. `startedAt` is **UnixNano (UTC) integer**. `durationUsed` is seconds actually listened (a JSON number / float — fractional skip seconds are preserved server-side). `codec` / `variantId` / `outputTarget` are optional; `interfaceType` ∈ `CarPlay` / `USB-DAC` / `Bluetooth` / `BuiltInSpeakers` / `Unknown`. Body capped at 4 MiB.

**Response** `202 Accepted`:

```json
{ "accepted": 11, "dropped": 1 }
```

The handler **drops, never faults,** events with an empty `path`, a non-positive `startedAt`, or a non-finite / negative `durationUsed` — one corrupt event never rolls back the rest of the device's stats. `dropped` counts them.

**`GET /v1/history?limit=&after=`** *(additive, since v1.7; advertised via the `playbackHistoryRead` flag)* — the cursor-paged, all-devices listening-history feed, newest first. Bearer-authenticated; the `X-Device-Token` header is **not** required (the feed is user-wide by design — every paired device belongs to the operator). `limit` defaults to 200, max 1000; `after` is the `nextCursor` from the prior page (omit for the first page).

**Response** `200`:

```json
{
  "events": [
    {
      "path": "Diana Krall/Live/01 Romance.flac",
      "startedAt": 1730000000000000000,
      "durationUsed": 184.5,
      "codec": "FLAC",
      "variantId": "upscaled-v2-176400-24",
      "outputTarget": { "interfaceType": "USB-DAC", "deviceName": "Chord Mojo 2", "outputRate": 176400, "isDoP": true },
      "deviceId": "9f86d081884c7d65",
      "deviceName": "Arseni's iPhone"
    }
  ],
  "nextCursor": 1841
}
```

`deviceId` / `deviceName` attribute the event to the **playing device** (`outputTarget.deviceName` remains the output hardware). `deviceId` is the first 16 lowercase-hex chars of `SHA-256(deviceToken)` — a stable, non-reversible display id; the raw recovery token never appears on the wire. A client can hash its own token the same way to mark "this device". `deviceName` is the registered display name, empty if the device never registered one. `nextCursor` is non-zero while more pages may follow (pass it back as `after`); a short or empty page returns `0`, meaning the feed is exhausted. `400 bad_request` on a non-positive `limit` or negative/non-integer `after`; `404 playback_history_not_supported` when the feature is off.

### Smart playlists (additive, since v1.9)

Server-generated dynamic "smart" playlists derived **entirely on the operator's own host** from the playback history + offline analysis already described above — no transcoding, no external service, nothing leaves the box. Opt-in (`smartPlaylists.enabled` in `bridge.yaml`); advertised via the `smartPlaylists` flag in `/v1/health.features`. Pre-feature (or feature-off) bridges return `404` from the route. `ProtocolVersion` stays `1` (a new endpoint + a new health flag, no change to any existing shape).

The bridge regenerates the families on a daily cadence into a server-side cache **distinct from the `/v1/playlists` backup store** (these are ephemeral/derived, never device-authored, no LWW). Only **populated** families appear — a family below its minimum-track threshold is omitted, so a brand-new library or one with analysis disabled simply returns fewer (or zero) playlists. Families:

| `kind` | Source | Notes |
|---|---|---|
| `heavyRotation` | history (last ~14d) | most-played, qualifying plays only (≥ 30s listened) |
| `driveMix` | history (CarPlay-only, last ~60d) | most-played over CarPlay (`iface_type = "CarPlay"`); a wider window than Heavy Rotation so occasional drives still populate it |
| `onRepeat` | history (last ~30d) | "sustained obsession" — paths played on ≥ 3 distinct days that each carried ≥ 2 qualifying plays (so ≥ 6 in total); **carries hysteresis** (surfaces at ≥ 12 tracks, stays visible while ≥ 8) |
| `forgottenFavorites` | history | loved long ago, untouched in the last ~30d |
| `recentlyPlayed` | history | distinct tracks, newest first |
| `autoMix` | analysis (key + tempo) | Camelot-wheel harmonic flow, level-matched via ReplayGain; **requires `analysis.enabled`** |
| `dailyMix` | history + analysis | ~70% familiar + ~30% discovery; discovery leg requires `analysis.enabled` |
| `timeOfDay` | history (by hour) | the device's current-local-hour habit (see `local_hour` below) |
| `artistDeepCuts` | history + library | tracks unplayed in the last ~90d, drawn from artists with ≥ 3 plays in the last ~30d (per-artist cap 3); rotates weekly by UTC ISO week |
| `liftOff` | analysis | bpm ≥ 120 AND replaygain ≤ -8 dB (loud, fast mood band); rotates weekly; **requires `analysis.enabled`** |
| `windDown` | analysis | bpm ≤ 90 AND replaygain > -6 dB (quiet, slow mood band); rotates weekly; **requires `analysis.enabled`** |
| `finishLine` | history (sessions) | tracks chained to the user's average listening-session length |

**`GET /v1/smart-playlists?local_hour=<0-23>`** — the populated families, served from the cache. Bearer-authenticated; the `X-Device-Token` header is **not** required (user-wide, like `GET /v1/history`). The optional `local_hour` is the device's current local hour, used **only to title** the `timeOfDay` family (the bucket itself is the server's current UTC hour — the same instant); omit it and the stored title is kept. A `timeOfDay` family with no listening habit around the current hour is omitted from that response.

**Response** `200`:

```json
{
  "refreshedAt": 1730000000000000000,
  "playlists": [
    {
      "slug": "heavy-rotation",
      "kind": "heavyRotation",
      "title": "Heavy Rotation",
      "subtitle": "Your most-played lately",
      "modalRateHz": 96000,
      "energy": [0.31, 0.62, 0.88, 0.54, 0.41],
      "refreshedAt": 1730000000000000000,
      "items": [
        { "position": 0, "path": "Diana Krall/Live/01 Romance.flac", "title": "Romance", "artist": "Diana Krall" }
      ]
    }
  ]
}
```

`slug` is the stable per-family id a client binds a homepage row to. `items` reuse the playlist-backup item shape (`position` + `path` + render-fallback `title`/`artist`); the client resolves `path` to its local library track exactly as it does for a restored backup. Top-level `refreshedAt` is the newest family's generating-run timestamp (UnixNano). Unknown future `kind` values should be tolerated (rendered generically) so older clients survive added families. `404 smart_playlists_not_supported` when the feature is off.

Each family MAY carry **`imageHash`** (additive, omitted when absent) — the SHA-256 hex of an operator-uploaded custom cover, served at **`GET /v1/smart-playlist-image/{slug}`** (bearer-authed, `image/jpeg`, `404` when none). The client prefers the custom cover and falls back to its auto-generated mosaic when `imageHash` is absent; treat the hash as an opaque cache key (a re-upload changes it). The same `imageHash` field + a parallel **`GET /v1/playlist-image/{id}`** endpoint are additive on the playlist-backup DTOs (`GET /v1/playlists` summaries + `GET /v1/playlists/{id}`). Operators set/replace/remove covers via the loopback admin API (`POST`/`DELETE /api/smart-playlists/{slug}/cover` and `/api/playlists/{id}/cover`, base64-JSON body). Covers are normalized server-side to ≤600 px JPEG; a deleted playlist's cover is pruned automatically. **Additive — no `ProtocolVersion` bump.**

Each family MAY also carry **`energy`** + **`modalRateHz`** (both additive, omitted when absent) — the server-derived inputs for a client's "waveform-signed cover" halo. `energy` is the mix's normalized **0…1** loudness contour: one element per member track (down-sampled to ≤48), mapped **linearly** from each track's ReplayGain across a clamped `[−24 dB, 0 dB]` window (`−12 dB → 0.5`); it is omitted when fewer than half the members are analyzed (the client renders its own seeded waveform instead). `modalRateHz` is the mix's modal sample rate (ties broken toward the **highest** rate, biasing the halo's glow color toward high-res); `0`/absent means "unknown — use a fixed family color". Both are decorative — a client that ignores them renders a plain cover. **Additive — no `ProtocolVersion` bump.**

### `POST /v1/pairing/requests` (additive, since v1.2)

Submit a join request that surfaces in the bridge admin web console as a pending entry. The admin reads the verification code off the iOS device's waiting screen, then approves or declines. iOS polls `/v1/pairing/{requestId}` for the verdict.

**Authentication**: none on this endpoint. iOS generates a 32-byte cryptographic random `pollSecret`, base64url-encodes it (no padding, 43 chars), and submits its SHA-256 hash here. See "pollSecret wire encoding" in the Authentication section above for the full canonical form.

**Request body**:
```json
{
  "deviceName": "Arseni's iPhone",
  "clientVersion": "1.4.0",
  "pollSecretHash": "<64-char lowercase hex SHA-256 of the base64url-encoded pollSecret>",
  "deviceToken": "<optional: client's durable recovery token, lowercase hex>"
}
```

`deviceToken` (additive, optional) is the iOS client's durable, device-local recovery token (stored in the Keychain, NOT iCloud-synced, surviving app reinstall). Supplying it at join time lets the bridge bind the device's registration — and reattach any prior playlist backups / playback history scoped to that token — the instant the operator approves, with the real device name attached. Pre-feature clients omit it; the binding then forms on the device's first authed request via the `X-Device-Token` header (below). The bridge never echoes it back on the wire.

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
|    404 | `no_image`               | Artwork / artist-image enrichment complete; no image exists upstream (terminal) |
|    400 | `bad_request`            | Malformed path, missing required query param     |
|    400 | `range_required`         | `/v1/read` called without a `Range` header        |
|    401 | `unauthorized`           | Missing / invalid bearer token (or pollSecret)    |
|    403 | `forbidden`              | Valid token, insufficient scope (reserved)        |
|    404 | `not_found`              | Path does not exist in any library root; or artwork is not cached under the requested MBID + `size` |
|    404 | `unknown_request`        | Pairing request ID unknown / cleaned up           |
|    404 | `pairing_not_supported`  | Bridge build doesn't expose tap-to-pair           |
|    404 | `events_not_supported`   | Bridge build doesn't expose `/v1/events` (pre-v1.2; iOS falls back to polling) |
|    404 | `smart_playlists_not_supported` | `smartPlaylists.enabled` is off (or pre-v1.9 build) — no `/v1/smart-playlists` |
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

### Device recovery token (additive, since v1.6)

The iOS app sends a second additive header on every authenticated request:

```http
X-Device-Token: <lowercase-hex recovery token>
```

This is the client's durable, device-local recovery token (generated once and held in the Keychain with `kSecAttrSynchronizable=false`, so it survives an app reinstall but does not sync across devices via iCloud). It is the stable identity that per-device state — **playlist backups** and **playback history** — is scoped to, distinct from the bearer token (which is re-minted on every re-pairing).

On each authed request the bridge binds the presented `X-Device-Token` to the current bearer token's ID in its `device_registrations` table (debounced — at most one write per device/token pair per 5 minutes, and immediately on a binding change). The binding self-heals across re-pairings: a device presenting the same recovery token with a freshly minted bearer reattaches to its prior backups automatically.

The header is optional and validated as bounded lowercase hex (≤128 chars); malformed or absent values are ignored and simply skip the binding. The token is never echoed on the wire.

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
