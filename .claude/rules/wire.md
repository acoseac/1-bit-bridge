---
paths:
  - "internal/api/**"
  - "PROTOCOL.md"
  - "internal/version/**"
  - "internal/manifest/types.go"
  - "internal/manifest/store.go"
  - "internal/manifest/playlists*.go"
  - "internal/manifest/favorites*.go"
  - "internal/manifest/playlist_files*.go"
  - "internal/manifest/sync_*.go"
  - "internal/manifest/history*.go"
  - "internal/advertise/**"
  - "internal/mdns/**"
  - "internal/urlquery/**"
  - "internal/admin/query*.go"
  - "internal/admin/pairing*.go"
---
# The wire contract

What the `/v1` API and the other wire surfaces promise the app: the served set, optional fields, tombstones, events, query encoding, endpoints and mDNS, and `PROTOCOL.md`. The cross-repo half of the contract (the Mirror-PR pair, `ProtocolVersion`) is in CLAUDE.md under "iOS companion".

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Wire-type discipline

Types in [`internal/manifest/types.go`](../../internal/manifest/types.go) — `Track`, `Folder`, `Manifest`, `Variant`, `EnrichmentProgress` — ARE the wire contract. Their `json:` tags are versioned by `internal/version.ProtocolVersion`. Adding or renaming a tagged field requires a `ProtocolVersion` bump (or an `omitempty`-gated additive that pre-version-N iOS will ignore). This is intentional: the bridge serializes `manifest.Track` directly into the `/v1/manifest` stream and into the `Track.Variants` aggregation built by SQL `json_object` — those rows ARE the JSON payload iOS reads.

**The constraint that's load-bearing is "must not be encoded directly from an HTTP handler", not "must not have `json:` tags".** Some internal domain types (`auth.Token`, `pairing.Request`) carry tags for their own persistence reasons — the `tokens.json` flat-file store, future on-disk pairing journals. That's fine and intentional. The rule is one level higher: NEVER pass any of those types to `json.NewEncoder(w).Encode(x)` / `json.Marshal(x)` from an `internal/api/` handler. Always wrap in a DTO under `internal/api/` (e.g. `tokenRow` in `apiTokensList`, `UpscaleStats`, `HealthResponse`, `ScanState`, `Entry`, `StatResponse`, `ErrorResponse`) so a future schema change to the domain type — renaming a SQLite column, adding an internal-only field — can't silently leak onto the wire.

For the SQLite row structs in [`internal/manifest/store.go`](../../internal/manifest/store.go) (the row scan targets — distinct from the public `manifest.Track`), the stricter rule applies: those MUST NOT gain `json:` tags at all, because they have no persistence justification and the only reason to add tags would be handler convenience — exactly the leak vector this section guards against.

**Hidden-leak vectors to check during review:**

- **`json.RawMessage` in any handler return path** would pass bytes-shaped data through and bypass the type-tag discipline. None today. If a new use lands, name the wire field it serves and document the schema-stability contract — most realistic motivations (preserving a SQLite blob column verbatim) are better served by a wire DTO whose body field is `[]byte` or an explicit struct so the schema is committed to up front. The `marshalForStorage` shim in [`internal/manifest/store.go`](../../internal/manifest/store.go) is NOT this pattern — it's a producer of the persisted blob via `json.Marshal` of a `*Track` clone, not a `RawMessage` carrier.
- **`any` / `interface{}` in handler-side helper signatures** defeats compile-time wire-shape checking. `writeJSON(w http.ResponseWriter, status int, v any)` in `internal/api/api.go` is the canonical example — convenient, but the caller's responsibility to pass a wire DTO is enforced by code review, not the compiler. When writing a new handler that hands a fresh type to `writeJSON`, double-check that type's `json:` tags + field list intentionally form the wire contract.
- **SQL `json_object` / `json_group_array` aggregations** (used in `Track.Variants` via `variantsAggSQL` in [store.go](../../internal/manifest/store.go)) build wire JSON inside SQLite. The columns selected in the aggregation are wire fields and follow the same versioning rule as struct `json:` tags — adding a new column inside the `json_object(...)` call is an additive wire change.

Audited PR-by-PR; verified clean at the time of this section's introduction. Re-audit when introducing a new handler or a new SQLite column on a wire-aggregated table.

## The wire contract

- **`GET /v1/outputs` is additive and `ProtocolVersion` stays 1.** `localOutputs` is advertised only when `output.mpd` resolves on and the host is Linux; otherwise the route is `404` `local_outputs_not_supported`. The body is an `internal/api` DTO: a kind `mpd` element has every key, every list `[]`, `dac` null when no card, `mpd` a pointer present only for kind `mpd` so a client decodes it as optional, `isMPDsOutput` `true` or `null` and never `false`, `kind` an open string a client ignores when it does not know it. Each unresolved candidate card is its own element, sharing `mpd` and `pathRules`. `mpd.version` is empty when the daemon is down. `mpd.loopbackBaseURL` (additive) is the base URL of the loopback track route the MPD on that box fetches a bridge track from, present only while the route serves, so a client decodes it as optional; the route asks `output.mpd` on every request, so a URL handed to MPD earlier is refused (`not_serving`) once it stops resolving on; the route's refusal codes are its own, not `/v1` codes, and PROTOCOL.md's error table does not list them. Prefixes are filled for `mismatch` and `unchecked` when SameFile matched, and empty for `unproven`. **Don't encode an `alsa` or `mpd` type from the handler.** The contract is the `GET /v1/outputs` section of `PROTOCOL.md`.
- **`GET /v1/outputs/{id}/reading` is additive, on its own key `outputReading`, and `ProtocolVersion` stays 1** (B473 B3). The key is advertised exactly when `localOutputs` is; without it the route is `404` `local_outputs_not_supported`, and an id that names no output now is `404` `output_not_found` (a new row in PROTOCOL.md's table). Every key is present, a list `[]`, `attributes` `{}`, an unread value `null`; `state`, `verdict.value` and `verdict.bitExactness` are words and every reason and warning a code, never a sentence, and every code `signalpath.Codes()` and `WarningCodes()` give, every source `WarningSources()` gives, the three values and `not_verifiable` are named in PROTOCOL.md's section, and `bit_exact` is not (`TestEveryReadingCodeIsInTheProtocol`). Round 3 (the owner's two levels) made the values `format_preserved` and `format_preserved_dop`, added `verdict.bitExactness` (`not_verifiable` in every verdict) and `verdict.warnings` (`{code, source}`, `[]` for none) and the state code `alsa_config_unread`, and dropped round 2's `mpd_config_unread`, `mpd_config_incomplete`, `mpd_config_mismatch` and `mpd_config_changed` with `alsa_chain_unproven` and `alsa_plug_converts`: the route had not shipped, so no client decodes the old words. Round 5 added the reason `dop_unconfirmed` (a DoP claim for settings the stream was not watched opening with), additive for the same reason. **Don't encode an `alsa`, `mpd` or `signalpath` type from the handler, and don't give an MPD output's verdict a bit-exact value.** The contract is the `GET /v1/outputs/{id}/reading` section of `PROTOCOL.md`; the iOS mirror copies it when I3 lands.
- **`/v1` describes the SERVED set.** The manifest stream/page/total,
  `enrichmentProgress`, health's `tracksIndexed`, the DLNA adapter and the
  smart-playlist pools all go through the `ListServed*` / `CountServedTracks`
  family (`dupe_suppressed = 0`). The full-store readers stay unfiltered ON
  PURPOSE and must keep seeing suppressed rows: `TrackPaths`/`TrackPathsUnder`
  (deletion-pass snapshots — filtering them would REAP suppressed rows),
  `ListTracks`/`StreamTracks`, `UnenrichedTracks`, `GetTrack`, `GetTrackWithStat` (the output reading's row), and every admin
  rollup (operator truth). `/v1/list|stat|read|download` are path-keyed and
  unfiltered so stale clients keep working.
- **Optional numeric/bool `Track` fields are pointers.** Non-pointer +
  `omitempty` silently drops zero/false, so iOS decodes `nil` instead of
  `Some(false)`. Same trap with `omitempty time.Time`: Go does NOT drop a zero
  time, so it ships `"0001-01-01T00:00:00Z"` and the client parses a real,
  very-old date — use `*time.Time`.
- **`sampleFormat` is `"integer"`, `"float"` or absent, and absent is unknown,
  never integer** (2026-10-23, backlog B513; PROTOCOL.md "Sample kind").
  A string with `omitempty`, so an unknown row carries no key, never `null`
  or `""`; a client tolerates a value it does not know. `codec` and
  `bitsPerSample` do not change with it. A variant carries its own,
  derived from its `format` in `scanTrackVariants` (`variantSampleFormat`:
  every rendition the bridge writes is FLAC, so `"integer"`), never stored.
  **The health key `sampleFormat` means every row that can have a kind has
  it** (round 3): `SampleFormatReady` is the backfill's completion mark,
  which a header pass records when it has run to its end with every row the
  open-time fill decides decided (scanner.md), and nothing the backfill
  writes before the mark moves `indexed_at`, so a client's one-time
  paginated full sync on seeing the key delivers every kind at once; a kind
  recorded after it moves `indexed_at`, so a delta carries it. Until round 3
  the key waited for the fill alone and every WAV or AIFF header read moved
  its row, a second delivery the size of the library's WAV files. A file the
  finishing pass could not read does not hold the key back, which would be
  forever over one unreadable file. The mark survives a restart and is never
  removed. The answer is cached with `firstIndexedAt`'s through one helper
  (`healthCountsCache.stickyReady`): sticky once ready, a miss rechecked on
  the TTL, a timeout not stored. **Don't emit the key on an unknown row,
  don't advertise the key before the completion mark or without asking it,
  and don't let one unreadable file hold it back.**
  (`TestTheManifestCarriesSampleFormatOnlyWhereAHeaderStatesIt`, streamed and
  paged, `TestAVariantCarriesTheKindItsFormatIs`,
  `TestSampleFormatIsReadyOnlyOnceEveryRowTheFillDecidesIsDecided`,
  `TestTheBackfillMovesNoCursorAndTheKeyWaitsForAPassToItsEnd`,
  `TestHealthOmitsSampleFormatUntilTheBackfillIsDone`,
  `TestTheTwoReadinessKeysKeepTheirOwnAnswers`.)
- **An optional time on a wire DTO is a `*time.Time`, and a guard enforces it.**
  This rule was stated here, fixed correctly in three structs, and violated in
  TEN fields across five files — including the `/v1/health` DTO iOS decodes.
  Documenting it three times did not stop the eleventh, so
  `TestNoOmitemptyValueTimeOnTheWire` (an AST walk over `internal/api` +
  `internal/admin`) now fails the build on a value `time.Time` tagged
  `omitempty`. **Check the client's optionality before converting one** — omitting
  a field a shipped app declares non-optional breaks its decode, which is why
  `/v1/health`'s withholding scope was set by reading the iOS Codable rather than
  by principle. **And move the TEMPLATES in the same commit**: `timeAgo` takes a
  value, text/template auto-indirects a pointer, and a NIL pointer is an
  execution error that fails the whole page rather than rendering "never". That
  half no Go type check can see.
- **Delta manifests omit the `folders` block** (full-sync only), and `since`
  filters on `indexed_at`, never mtime.
- **A served→suppressed transition writes a `manifest_deletions` tombstone in
  the same transaction as the stamp**, and the since-leg emits it as
  `deleted: [paths]`. `indexed_at` is still never bumped on suppression; the
  tombstone is the delta signal.
- **A playlist tombstone is DATA, and `updated_at` on a `deleted = 1` row IS
  the delete time.** `DELETE /v1/playlists/{id}` sets a flag and keeps every
  `playlist_items` row, so the console can lift it (`RestorePlaylist`, behind
  `GET /api/playlists/deleted` + `POST /api/playlists/{id}/restore`) — which is
  what the 2026-09-20 incident needed and did not have: a freshly-paired phone
  replayed a queued delete, 17 DELETEs in 26 s, and recovery was an `UPDATE`
  by hand against a live database. A restore **must not move
  `last_modified_at`** — that is the client's wall clock and the LWW guard key,
  and an operator undoing a delete has not authored a new version; leaving it
  makes LWW resolve exactly as if the delete had never happened. There is
  deliberately **no `deleted_at` column**: `TombstonePlaylist` is the only
  writer of `deleted = 1` and stamps `updated_at` in the same statement, so a
  second column carrying the same instant could only disagree by being wrong
  (`TestUpdatedAtOnATombstonedRowIsTheDeleteTime`). What WAS missing is the
  DELETER — `device_token` is the last WRITER, a different device for all 17
  rows — hence `playlists.deleted_by` (v45), cleared on restore so a stale
  value cannot attribute the next delete. The custom cover does NOT come back:
  the DELETE unlinks the JPEG (`api.pruneCover`) and nothing keeps a copy.
- **Favorites revisions are compare-and-swap, and a legacy PUT is additions
  only** (2026-10-07). `PUT /v1/favorites` with `baseRevision` stores the
  body when that revision is current, or when the normalized body already
  equals the stored document; any other base whose body differs is `409`
  `stale` and writes nothing, including no device row. A PUT with no
  `baseRevision` — what 2.1 sends — inserts or keeps the earlier
  `favoritedAt`, leaves a tombstone tombstoned, leaves an omitted key
  alone, and answers `200`. A `409` that decodes as `stale` re-arms the
  2.1 flush, so that path never returns one. `revision` bumps only when
  the document changes, including tombstone collection. Collection runs
  in its own transaction before the compare-and-swap, under the store
  lock: a tombstone is eligible at `removed_at` ≤ now−90d, and one
  `legacy_put_at` later than that cutoff blocks every collection. A `GET`
  does not collect and does not take `Store.mu`; it reads the epoch and
  the revision in one read-only transaction and returns `304` before any
  row. After collection a legacy save can insert the key again, and a
  client whose base predates the collection reads that as a re-heart. A
  legacy merge that would pass 50,000 tracks or 10,000 albums stays `200`
  and keeps existing keys first, leaving room for the tombstones already
  stored so a revisioned echo of live rows plus those tombstones still
  fits. `bridge restore` replaces `backup_epoch` after the database
  copy, and the pre-copy WAL removal is what keeps a
  same-lineage WAL from replaying onto the restored file. Playlist
  `baseLastModifiedAt` is a different code, `base_mismatch`, and only
  when the base is not the stored stamp and the body differs; an identical
  body is decided first when a base is present or the row is deleted, so
  a stale base on the same body is `200` Unchanged, live or deleted. A
  live playlist with no base stays on the stamp guard even when the body
  already matches: an older stamp is `409` `stale` and a newer one stores
  the client's stamp. A matching base with a different body is
  stored, which revives a deleted row even when that base predates the
  deletion: `updated_at` is the bridge clock and `last_modified_at` is the
  client clock, so comparing them refuses every base the client holds. A
  behind clock stores one nanosecond above the stored stamp. A
  `base_mismatch` for a tombstoned row carries `server.deleted`. An
  unregistered `favorite_sync_devices` row is deleted by both registration
  reaps.
  (`TestTwoClientsCannotClobberFavorites`,
  `TestLegacyFavoritesPutNeverConflicts`,
  `TestPlaylistMatchingBaseAcceptsABehindClock`,
  `TestIdenticalPlaylistBodyDoesNotRevive`,
  `TestLegacyIdenticalPlaylistPutKeepsTheStampGuard`,
  `TestACappedLegacyDocumentEchoes`,
  `TestMatchingBaseRevivesADeletedPlaylist`,
  `TestReadFavoritesDoesNotTakeTheWriterLock`,
  `TestFavorites304DoesNotReadTheRows`,
  `TestRestoreRemovesStaleWALSHM`.)
- **A user-data change event is published after its commit, and
  `library.changed` is one event at the end of a full scan** (2026-10-07).
  `favorites.changed` (`{"revision":N}`), `playlists.changed`
  (`{"epoch":"…"}`) and `library.changed` (`{"indexedAt":"…"}`) go through
  `eventBroker.Publish`, so they carry `id:` and take part in
  `Last-Event-ID` replay. The store notifies from a hook installed by
  serve, and the hook runs only after `Commit` has returned nil and only
  when the write changed the document, the playlist list, or what a
  delta client sees (`indexed_at`, or a journaled deletion or
  suppression). A rolled-back write, a `409`, and a no-op publish
  nothing. Tombstone collection commits before the favorites
  compare-and-swap and publishes its own revision when it removed a row.
  That revision is read with `context.WithoutCancel`: the request may
  already be cancelled once the commit has returned, and a cancelled
  read drops the event while the revision has moved.
  `library.changed` is suppressed while health's
  `scanState.isScanning` is true (`Scanner.AdvertisedScanning`:
  `IsScanning` and not stalled, not `ScanInFlight`: a subtree scan stays
  on the debounce, and a stalled scan does not suppress). The scan-end
  callback is a defer registered ahead of `scanning.Store(false)`, so it
  runs once the flag is clear, once, on a successful scan whose context
  is still live, and that one event covers every `indexed_at` write the
  scan made, enrichment included. Every other library change shares one
  trailing 30s debounce, including a journaled deletion or suppression
  outside a full scan; a note during a scan cancels a timer already
  armed, and the scan-end event cancels one that is still pending. The
  payload watermark is the later of `MAX(indexed_at)`,
  `MAX(manifest_deletions.deleted_at)` and the deletion-journal
  coverage start, rendered as millisecond UTC RFC3339, which
  `GET /v1/manifest?since=` parses. A mass delete that resets that
  coverage publishes a start one past the watermark it replaced, including
  when it deletes every track. An empty library at scan end publishes the
  publisher clock when no watermark exists. A watermark query error at scan end is
  logged and that event carries the publisher clock; a note outside a
  scan still publishes nothing when the query fails. A sidecar lyrics bump on the
  version-stamp leg notes after its commit; a refusal skips lyrics.
  A smart-mix cover and a cover pruned from an already-tombstoned
  playlist publish no `playlists.changed`. The demo wires none of
  this and does not advertise `syncEvents`. A demo bridge answers
  `GET /v1/events` with 404 `events_not_supported` before it subscribes,
  so the public demo token occupies no broker slot; pairing streams
  keep the global cap, and a non-demo `/v1/events` still subscribes
  (`TestADemoTokenHolderCannotStarvePairingEvents`,
  `TestAMintedTokenOnADemoBridgeGetsNoEventStream`,
  `TestAPairedDeviceEventStreamStillSubscribes`). A shutdown ends an
  open stream because the handler also selects on the serve context the
  stop cancels (`Server.EndEventStreamsWhen`), after the 200 has flushed,
  so the client sees a finished stream and reconnects. An ordinary
  request keeps its own context and finishes inside the grace
  (`TestEventStreamsEndWhenServeShutsDown`,
  `TestAnOrdinaryRequestInFlightAtShutdownCompletes`, the serve-wiring
  bullet). `bridge restore` replaces the
  epoch in its own process, which has no broker and publishes nothing;
  the serving process publishes `playlists.changed` only from a store
  that holds the hooks.
  (`TestFavoritesRevisionPutPublishesTheNewRevisionAfterCommit`,
  `TestFavoriteTombstoneCollectionPublishesAfterItsCommit`,
  `TestFavoriteCollectionPublishesAfterItsRequestIsCancelled`,
  `TestPlaylistPutDeleteRestoreAndCoverPublishAfterCommit`,
  `TestAnIdenticalPlaylistBodyAndAMismatchPublishNothing`,
  `TestLibraryChangedIsSuppressedWhileAScanRunsAndEmittedOnceAtTheEnd`,
  `TestLibraryChangedIsOneEventAfterAQuietThirtySeconds`,
  `TestAFullScanPublishesOneLibraryEvent`,
  `TestAWatermarkQueryErrorPublishesTheScanEndClock`,
  `TestADemoBridgeDoesNotAdvertiseSyncEvents`,
  `TestADemoTokenHolderCannotStarvePairingEvents`,
  `TestAMintedTokenOnADemoBridgeGetsNoEventStream`,
  `TestAPairedDeviceEventStreamStillSubscribes`,
  `TestSyncTopicsReplayOnLastEventID`.)
- **The playlist mass-delete WARN counts from the TABLE, never an in-process
  ring**, and fires ONE LINE PER TOMBSTONE past the threshold
  (`manifest.PlaylistDeleteBurstThreshold` = 5 within
  `PlaylistDeleteBurstWindow` = 60 s). The console's panel gets the SAME
  predicate, not the same two numbers: `LargestPlaylistDeleteRun` groups the
  tombstones server-side over the whole `deleted_by` token, because the browser
  cannot reproduce it even in principle — `deletedByPrefix` is eight redacted
  characters, and the first draft's chain-of-pairwise-gaps ended a run at one
  interleaved delete from another device, so the panel said 3 where the journal
  said 6. **A predicate the client cannot evaluate exactly belongs on the
  server, whatever the numbers behind it.** Counting from the rows is
  what makes the warning and the restore panel unable to tell different
  stories, and what makes a burst spanning a restart still one burst. The
  repetition is deliberate and is **not** the M-SEARCH case: that ticker's
  failure is persistent and every line identical, while a delete run is bounded
  by how many playlists exist, each line carries a different count, and the last
  one is where the run stopped. Never a gate — the tombstone has committed by
  then, so a failed count logs and the delete stands.
- **Verify a wire field name against the Go tag before "fixing" code to match a
  doc.** `deletedIds` was written up as `deletedPlaylistIDs` here for months
  while the tag, both PROTOCOL.mds and the iOS DTO always agreed on `deletedIds`.
- **`last_modified_at` is the only precision-critical wire field** — client
  UnixNano `Int64`, never round-tripped through a string or `Date`, or
  truncation falsely trips the LWW 409.
- **Every `/v1` route declares a `rateClass`, and the zero value is INVALID.**
  Unlike `routeKind`, the permissive answer is the dangerous one here. **Size the
  BURST, not the rate**: iOS surfaces 429 as a transport error and does NOT
  retry, so a tight bucket breaks a sync rather than slowing it.
- **`safeQuery` on every path-bearing query consumer.** `url.Values` decodes `+`
  as a space, so a path containing `+` silently resolves to the wrong file — a
  `200 {deletedCount: 0}` no-op. The client half must use `encodeURIComponent`,
  never `URLSearchParams` (which form-encodes a space back to `+`). **The
  console's variant delete was the last admin handler reading a library path
  through `r.URL.Query()`** (2026-09-28): `curl -X DELETE
  '…/api/upscale/variants?prefix=AC+DC'` answered `deletedCount: 0` on main and
  left the rendition on disk. Its client, `deleteVariants` in
  static/player/api.js, was on `URLSearchParams` and worked only because the
  server form-decoded too, so the handler moved to `safeQuery` and the client to
  `encodeURIComponent` in one commit. **The two halves move together**: either
  alone breaks every path with a space.
  `TestDeleteVariantsClientRoundTripsThroughTheServer` runs the shipped client
  under node and sends the URL it builds to the handler.
- **…and a query the bridge WRITES for such a reader goes through
  `internal/urlquery`, never `url.QueryEscape` or `url.Values.Encode`**
  (#1046). Both write a space as `+`, and two readers keep `+` as a plus:
  Foundation's `URLComponents.queryItems`, which the app reads the pairing QR
  with, and `safeQuery`. So from April to September the QR named a library
  `My Library` as `My+Library` (the default as `1-bit+Bridge`), which the app
  pre-fills as the name it saves the bridge under, and `bridge enrichment
  misses --path "Meridian Glass"` asked for `Meridian+Glass` and reported 0
  misses of 5. `urlquery.Escape` / `Encode` write `%20`, which loses nothing
  (a literal `+` is always `%2B`) and decodes alike under both kinds of
  reader. **Read a pairing URL in a test as the app does, never with
  `u.Query()`**: that is a form decoder, it reads the `+` back as a space,
  and it is how every pairing test passed (`appQueryItems` in
  internal/admin; with the old encoder and a form-decoding reader, the
  mint/rotate test goes green). The bridge's fix is the whole fix: the app
  parses RFC 3986 correctly, and reading `+` as a space there would refuse
  codes that pair today (a pre-#1042 name with a trailing space is written
  `name=X+`, which the app takes as "X+" and would then refuse as "extra
  spaces").
- **`/v1/health` withholds `scanState` and the update triple from an
  unauthenticated caller**; the scope was set by reading which iOS Codable
  fields are optional, not by principle — `libraryName`, `libraryRoots`,
  `certFingerprint`, `serverVersion` and `startedAt` are non-optional there, so
  withholding them fails decoding on every shipped app. An invalid token is
  unauthenticated, never 401 — a client holding a revoked token still needs the
  endpoint list to reconnect.
- **Long-lived and streaming routes need their own deadlines**:
  `/v1/booklet/{mbid}` and `/v1/download` are `streamingRoute`; the two
  synchronous upscale operations carry a 15-minute `writeDeadline` because
  `boundedHandler` starts the clock BEFORE the handler runs.
- **`/v1/artwork` and `/v1/artist-image` cache-miss is a THREE-way split** — 202
  `pending` only while enrichment is genuinely pending, terminal 404 `no_image`
  once it has run. Fail OPEN to pending on a DB fault; no `Retry-After` on
  `no_image`; don't collapse it back to two states.
- **Don't assume byte-wise client consumption.** The iOS client once used
  `URLSession.bytes(for:)`, which yields one `UInt8` per async step — 20M yields
  for a 20 MB file stalled the pipeline and surfaced as "Network connection lost"
  even over localhost. Don't add a server-side chunked mode that assumes it.
- **Never advertise an endpoint synthesised from this process's own listen
  port.** In public mode the bridge emits `https://<autocert.domain>:<listen
  port>` beside `customEndpoints`. That is right only when nothing remaps the
  port — behind a proxy that does (each hosted tenant listens on a loopback high
  port and is published on one shared external port) it publishes an address no
  client can reach, and iOS puts every advertised URL into its failover
  rotation. The entry is skipped when `customEndpoints` already names that HOST;
  the operator declaring it there is them saying what the reachable address is.
  Host comparison, not URL comparison — the two differ in exactly the part that
  is wrong, so the existing string dedupe cannot see it. `autocert.domain` cannot
  simply be omitted: public mode refuses to start without it.
- **An endpoint the bridge advertises carries no user name, password, path,
  query or fragment** (backlog B54; the path since B66). A `customEndpoints` entry was kept as
  written, so `/v1/health`, which answers without a token, published
  `https://user:password@host:7788` (a token as the user name, a `?token=`,
  a `#…` alike) to any caller, and the pairing QR's `urls=` carried it
  (measured on main with the real binary: all four shapes, in both); in
  public mode a declared endpoint is also the QR's PRIMARY `url=`. The phone
  relies on none of those parts: it sets its own `Authorization` header (the
  bearer token) on every request, its request delegates cancel every
  challenge but the server's certificate, and `buildRequest` replaces the
  path, and the query of every request that carries one (the file routes),
  so no proxy that needed one of those parts could have served the app's
  file routes (read in `BridgeSourceClient`, `BridgePairingURL` and
  `SMBStore`, 2026-09-28). **A path is not the root's to keep either**
  (B66): `buildRequest` sets every data request's path over the endpoint's,
  and the requests that APPEND to it instead (`appendingPathComponent`: the
  pairing join and the redeem, the first-contact `/v1/health` probe, the
  `/v1/events` stream) reach a path the bridge does not serve, which answers
  404, and a redeem that meets one fails pairing outright ("doesn't accept
  pairing codes", no fallback). So no deployment used one, every request
  reaches the bridge without it, and a token in it reached every caller of
  health for nothing (read in `BridgePairingClient`, `BridgeEventStream`
  and `BridgePairingPersistence`, 2026-09-30). A bare `/` is the root and
  is kept; a path counts only after an authority, so `not a url` is still
  the prune's to drop, not a refusal naming a path.
  **Repair what is stored, refuse what is typed**
  (the library name's rule, under Config): `ValidateCustomEndpoints`, which
  `Normalize` runs for `Load` and every writer, keeps such an entry WITHOUT
  those parts (`config.HasCredentialParts`), dedupes on the published form,
  and warns once per entry under its own message, never "dropped"; the
  settings PATCH refuses a typed one whole (`config.CheckCustomEndpoints`,
  400 `validate`, nothing written), and `bridge init --public --domain`
  refuses any domain `config.AutocertHost` would change (exit 2, nothing
  written; the next bullet), since the domain is also the autocert host
  that health and the QR build a URL from, as a string. It asked
  `HasCredentialParts` until B66, which let a path, a scheme and a port
  through; and it wrote the untrimmed flag, so a padded domain's endpoint
  (`https://` joined to the spaces) did not parse and the install was saved
  with none.
  **Don't drop such an entry**: its host and port still reach the bridge,
  and a loaded config must not lose a route over a part nothing reads.
  **Don't strip at the publish sites**: every enumeration
  (`ReachableEndpoints`, `pairAlternates`' public branch,
  `defaultBridgeURL`, the console's panel) reads the normalized list, and a
  second strip is the copy that drifts. No wire change and no Mirror-PR: an
  endpoint is still a URL the client dials. The file keeps the value until
  the next save writes the normalized list, which the warning says.
  `TestServePublishesNoCustomEndpointCredential` boots serve and checks
  health, a minted link and every log line;
  `TestHealthPublishesNoCustomEndpointCredential` and
  `TestPublicPairingCarriesNoCustomEndpointCredential` cover public mode.
  **Nor does `/v1/health` publish a manual upstream's URL**:
  `upnpUpstreamServers[].descriptionURL` read it back for a server
  configured with a UDN AND a manual URL, whose poller caches its fetch
  under the UDN, while the DTO and PROTOCOL.md keep a manual URL off the
  wire. The public adapter leaves out a value equal to the configured
  manual URL, and never strips one: the device's own LOCATION is published
  as it gave it, query included (a Windows device host's is
  `…/udhisapi.dll?content=uuid:…`), so a rule on its parts would cost those
  devices the hint (`TestHealthDoesNotPublishTheOperatorsManualURL`).
- **…and `autocert.domain` is a host name alone, served as the host it
  names** (backlog B66). Public mode builds a URL from it as a string,
  `https://<domain>:<listen port>`, in `/v1/health` (`publicModeEndpoints`),
  in the QR's alternates (`pairAlternates`) and as the QR's PRIMARY `url=`
  (`defaultBridgeURL`), and the banner prints it ("Public mode — domain:",
  "Admin console:"). A hand edit `user:password@host` was published and
  printed whole (measured with the real binary: health, both QR fields and
  both banner lines). Every other consumer wants a host too and matched
  nothing: the ACME whitelist (`autocert.HostWhitelist` runs IDNA, which
  refuses the `:` and the `/` such values carry, measured), the SNI route,
  and the console's Origin allowlist, which compares a browser's Origin
  hostname with the domain. So `Normalize` serves it as
  `config.AutocertHost`'s reading (no scheme, user information, port, path,
  query or fragment), warning once under its own message and naming the
  field by scheme and host (`urlFieldForLog`), never the value. **Repaired,
  never refused**: the value loaded before. **Refuse what is typed**:
  `bridge init --public --domain` refuses any value `AutocertHost` would
  change (exit 2, nothing written), and trims once and writes what it
  checked.
  **A value that names no host that can be read is served as
  `config.InvalidAutocertDomain`** (`autocert-domain.invalid`, RFC 6761),
  never blanked: `Validate` refuses an empty public domain, and the phones of
  such a bridge may still reach it through `customEndpoints`. None of those
  values ever worked (no certificate, no Origin match, a URL the app cannot
  parse), so the placeholder breaks nothing. **Three rules keep the
  reading clean.** An IP address, once a scheme, user information, a path,
  a query and a fragment are removed, is returned as written:
  `url.Parse("//2001:db8::1")` reads the host `2001:db8:` and the port 1,
  after `user@` too. **Never bracketed**: the Origin allowlist compares the
  domain with an Origin hostname, which has no brackets, so bracketing
  would lock the console out of a bridge whose unbracketed IPv6 domain
  passes it today; the URL built from that domain is invalid, which is
  backlog B152 (both spellings measured, a review's proposal). **Nothing that
  precedes an `@` is ever returned**: an `@` after the first `/`, `?` or `#`
  could as well end user information a hand edit left unescaped
  (`user:12/34@host` parses with the host `user`), so it names no host.
  And a value `url.Parse` cannot read (a password with a space) names none.
  **What it returns is a fixed point** (`Normalize` is idempotent, and init
  takes back what it stores; a bracketed IPv6 keeps its brackets and a zone
  its `%25`). Read in public mode and wherever `autocert.enabled` (serve
  starts the ACME manager in either posture and prints the domain); a
  loopback config with autocert off keeps its value, unwarned.
  **Don't strip at the publish sites** (the bullet above: they read the
  normalized value). The Gemini consult on the placeholder was refused by
  the API's spending cap; decided here.
  `TestAutocertHostReadsTheHostAValueNames` (a table, every shape, the
  fixed point),
  `TestNormalizeServesAutocertDomainAsItsHost`,
  `TestHealthPublishesNoAutocertDomainCredential`,
  `TestPublicPairingCarriesNoAutocertDomainCredential` and
  `TestServePublishesNoAutocertDomainCredential` (the real public serve:
  health, a minted link, the banner and every log line).
- **mDNS TXT records carry `host` + `port`.** Without them iOS must
  NWConnection-resolve the Bonjour service to a hostport, which is unreliable;
  the bare-hostname-plus-`.local` form matches the SRV target the cert SANs
  already cover. Never emit `host=.local` — fall back to `localhost.local`.
- **A container id, the default docker0 address, and a point-to-point ULA
  stay out of the lists a phone dials** (2026-10-13, backlog B189, B199,
  B229). `/v1/health` endpoints and the certificate want-set share one
  walk (`eachUsableAddr`, `omitFromWalk`). Docker's generated hostname is
  exactly 12 lowercase hex digits after the cut at the first dot, and it
  is unpublished only while `/.dockerenv` exists: `advertise.Endpoints`
  drops `https://<id>.local:<port>`, and `CertHostname` leaves the
  want-set without the bare id and without `<id>.local`. A name the
  operator set stays, inside Docker included. A 12-hex name on a host
  with no marker stays. Setting the hostname to 12 hex digits inside
  Docker is the accepted miss. `172.17.0.0/16` leaves both walks only
  while that marker exists. A `192.168` address, a `10/8` address, and a
  Compose network on another `172.16/12` slice stay. After Tailscale
  classification, a point-to-point address in `fc00::/7` outside
  `fd7a:115c:a1e0::/48` leaves both walks. The classifier runs first, so
  a Tailscale ULA on a point-to-point interface stays on the certificate
  path; health still publishes Tailscale from the provider. Wintun
  carries no point-to-point flag and stays. A ULA on a physical
  interface stays, while the certificate names it (the next bullet). An
  address written in `customEndpoints` is still published, and in
  loopback mode that list stays additive.
  (`TestATwelveHexNameBesideDockerenvIsNotPublished`,
  `TestATwelveHexNameWithoutDockerenvIsPublished`,
  `TestAChosenHostnameInsideDockerIsPublished`,
  `TestCertHostnameOmitsAGeneratedContainerName`,
  `TestADefaultDockerBridgeAddressInsideDockerIsNotPublished`,
  `TestALanAddressInsideDockerIsPublished`,
  `TestAPointToPointULAIsNotPublished`,
  `TestATailscaleULAOnAPointToPointInterfaceStaysOnTheTailscalePath`,
  `TestAULAOnAPhysicalInterfaceIsPublished`,
  `TestAWintunShapedULAIsPublished`,
  `TestACustomEndpointKeepsAnAddressTheWalkSkipped`.)
- **…and an IPv6 address of the walk that the served certificate does not
  name is not advertised, and is said once; an IPv4 address is advertised
  either way** (2026-10-18, backlog B457; option B, the owner's
  decision). The operator bridge took an IPv6 ULA (`fd00::/8`) from a
  device on its LAN after its certificate was minted, and `/v1/health`
  advertised it, where a client that checks the
  certificate against the URL's host fails; `bridge doctor` called the
  SANs stale. Re-minting makes every phone re-pair, so the owner kept the
  certificate. Serve reads the IP addresses of the pair it loaded
  (`servertls.NamedIPs`) and, for a pair this bridge minted, hands them to
  `api.(*Server).WithCertifiedAddrs`: `advertise.Endpoints` then
  publishes an IPv6 address of the walk (a LAN or public class, not
  Tailscale's: `certGated`) only while the certificate names it, through
  `Params.NamedByCert`, and passes each other one to `HeldBack`, which
  Warns once per address (`heldBackMessage`, 64 addresses at most, since
  temporary IPv6 addresses change daily). **An IPv4 address of the walk
  is published whatever the certificate names**: the app pins the
  fingerprint rather than checking the host, and its LAN path, a VPN
  route (which cannot use the `.local` name) and the URLs it hands a DLNA
  renderer all need one, so a new DHCP lease or a data directory moved
  to another host must not take it away. The certificate is stale for
  such an address, and the startup warning and `tls-cert-sans` say so,
  as they did before B457. `CertGatedIPs` asks the same `certGated`, so
  the doctor holds back exactly what serve does. `/v1/health`, the
  pairing QR and the console's Reachable endpoints read that one list. A
  rotation (`bridge cert rotate`, a restart) brings an address back,
  because the want-set a mint covers (`GatherCertSANIPs`) keeps it; the
  doctor and the startup warning do not call the certificate stale for a
  held-back one (under Auth, pairing, TLS). **Not judged**: the Tailscale
  endpoints (the provider's), the `.local` name, and a `customEndpoints`
  entry. An address one declares is published as written, and the walk
  does not hold back its own copy of it either (`declaredIPs`, which
  `certGated` reads for `Endpoints` and `CertGatedIPs` alike), so where
  the certificate does not name it, it is stale on both surfaces, never
  "not advertised". **A certificate this bridge did not mint holds nothing back**:
  rotation is not advised for it. The first rule (option A) held back an
  IPv4 address too, which cost the app its LAN address after a lease
  change; the owner chose this one. **Don't hold back an IPv4 address,
  don't give `Endpoints` and `CertGatedIPs` two rules, don't take a walk
  address out of the want-set, don't judge the `.local` name, the
  Tailscale endpoints or a `customEndpoints` entry, and don't hold back
  for a certificate this bridge did not mint.** No wire change: the list
  only gets shorter.
  (`TestServeHoldsBackOnlyTheIPv6AddressesItsCertificateDoesNotName`,
  which boots the real serve over a fixed walk
  (`advertise.SetInterfacesForTest`) and a minted pair less one IPv4 and
  both IPv6 addresses of it, and then rotates,
  `TestAnIPv6WalkAddressTheCertificateDoesNotNameIsHeldBack`,
  `TestAnIPv4WalkAddressTheCertificateDoesNotNameIsPublished`,
  `TestAHeldBackAddressIsLoggedOnceAndTheLogIsBounded`.)
- **The Bonjour instance name is ONE DNS label, 63 bytes, and hashicorp/mdns
  does not check it** (#1046). The instance is the library name:
  `NewMDNSService` takes a longer one, `Advertise` succeeds, serve prints
  "mDNS: advertising as", and every answer to a browse then fails to pack
  (`dns: bad rdata`), so the bridge is never discovered; the one trace is an
  INFO line per query ("[ERR] mdns: Failed to handle query"). 64 ASCII
  characters, or 22 CJK ones, were enough (measured on a Linux LAN).
  `sanitizeInstance` cuts it to `maxInstanceLen` bytes on a rune boundary;
  TXT `library=` carries up to 240 bytes of the name. **The instance and
  every TXT string are DNS presentation format to miekg/dns**, where a
  backslash escapes what follows it (`\X` is X, `\DDD` a byte): each is
  written `\\` AFTER the cut or cap, which count the bytes on the wire.
  Unescaped, `AC\DC Live` was browsed as instance and `library=` "ACDC
  Live", and a trailing backslash (a name's own, or one a cut left) merged
  the instance into the service label or vanished from the TXT (CodeRabbit
  reported the cut case; measuring it found the rest). **A browse that finds
  nothing is a finding only once a short-named control is found**: on the
  dev Mac the responder bound a link-local `utun0` tunnel ahead of `en0`, so
  even "Probe Short Name" was invisible there, until the picker learned to
  prefer a real LAN (the selection bullet under DLNA, UPnP and discovery,
  2026-09-27).
- **A shape documented in PROTOCOL.md is a Mirror-PR obligation.** The two specs
  are byte-identical by rule, and both directions are guarded
  (`TestEveryDocumentedEndpointIsRouted` / `TestEveryRoutedEndpointIsDocumented`).
  A mention in running prose is NOT a contract — the guard accepts only a `### `
  heading or a bold `METHOD /path` lead-in, because prose-mention is the state
  six live endpoints were already in.
- **PROTOCOL.md's error table lists exactly the status and code pairs the
  `/v1` handlers send, and two guards hold it there** (2026-10-16, backlog
  B426). `TestEverySentErrorCodeIsInTheProtocolTable` fails on a pair with
  no row, and `TestEveryProtocolErrorRowIsSent` on a row nothing sends,
  except one marked `(reserved; no route sends it)`, which nothing may send
  (`forbidden`). A new code is a new row in the same PR, and the app's copy
  follows. The pairs are read by parsing `internal/api` with go/ast: the
  calls of the three envelope writers, and `writeJSON` / `writeJSONGzip` of
  a struct literal with a `json:"error"` field under a 4xx or 5xx status.
  **A status or code must be a constant, a parameter its callers pass one,
  or a local set from a call the guard can follow; anything else fails the
  guard rather than go unread.** `declaredFieldFlows` is the one exception:
  `proxyUPnP` writes a `*upnpproxy.PreStreamError`'s fields, whose values
  the guard reads from that package's literals. Not read: an error status
  with no envelope (`http.Error`, a bare `WriteHeader`), which no handler
  writes, and a writer called through a function value
  (`TestErrorPairScanReadsEveryShapeOnAFixture`,
  `TestParseErrorTableReadsRowsAndRefusesWhatItCannotPair`).
- **`DELETE /v1/atlas-harvest/credential` forgets the harvest credential,
  and a demo bridge refuses it** (#1049). Switching the app's library
  harvest off stopped only the app's renewals: the `bulk_harvest`
  credential the bridge held stayed usable until it expired (the audit's
  H3). The route calls the store's `Clear()`, which drops the token and its
  expiry and KEEPS the sync position (a re-provision resumes), answers 204
  whether or not one was held, and draws from the write bucket. **The demo
  answers 403 `demo_read_only`**: its one credential is shared by every
  demo user, so one of them switching off must not stop the harvest for
  all, and the POST's accepted residual (a public bearer can overwrite the
  token) is not widened into a public off switch, whatever the harvest
  setting. **A bridge with the harvest OFF clears the file too, and answers
  204**: no store is open there, so `serve` wires
  `atlasharvest.ClearStoredCredential` instead of the sink. This bullet said
  "a credential file it may still hold is left alone, since nothing there
  reads it" until CodeRabbit (on the app's #1981) caught the premise:
  re-enabling the harvest reads that file again, so a 404 told the app
  nothing was held while a credential waited to come back into use. **`204`
  is the only answer that means revoked**; the app reports anything else,
  405 from an older bridge included, as not revoked.
- **Don't label a spec section with a version you cannot verify.** The `since
  v1.x` labels are iOS app versions, which a bridge-side session cannot derive.
  Name the **feature flag** instead — it is checkable here and is what a client
  keys on, since no client can ask a bridge its protocol era.
- **`GET /v1/playlist-files` is read-only, paged at one version, each page's
  ETag is a hash of the page, and it names only served tracks** (2026-10-17,
  backlog B431, paged by B442; flags `playlistFiles` and
  `playlistFilesPaged`, wired on every bridge, the demo included; no
  `ProtocolVersion` bump). A page holds the files that fit
  `PlaylistFilePageBudget` (8 MiB by `PlaylistFilePageCost`: path and name
  in the bytes the JSON writes, escapes included, and 112 a file, JSON bytes
  plus 4 an entry, never less than the file's JSON), in path order; a file
  that does not fit starts the next page, and a page always holds one, so a
  file larger than a page has its own. `nextCursor` (absent on the last page)
  is the feed's `version` and the last path the page covers, HMAC-signed with
  a key the process draws (`playlistCursorSigner`): an edited or pre-restart
  cursor is a 400, one of another version a 410 `playlist_files_stale`, and
  the client restarts. The
  version hashes the rules (`playlistFeedRulesVersion`), the encoder's
  cost of a byte of invalid UTF-8 (`jsonInvalidByteBytes`, B452), the
  build, the backup epoch, `complete` and a generation the v57 triggers move on every
  track or playlist row change a page can show (a track added, removed,
  renamed or restamped; a playlist row added, removed, or changed in its
  lines, mtime, `too_large` or `folder_whole`); enrichment moves none. The
  ETag is the SHA-256 of the page's JSON, because members resolve per request;
  a placed line is its exact path, else the one track a case-and-composition
  fold answers; a suppressed duplicate is its group's one served copy or
  unresolved; a file of 2,000,000 bytes or more (the app's cap) or of more
  than 100,000 entry lines is listed `tooLarge` with empty lists, and no
  budget ever makes one so. **An album-folder playlist is
  not listed, by the app's four terms** (`AlbumFolderPlaylist.isAlbumPlaylist`,
  its B421; entries are the lines the path rule places, a dropped line is no
  entry): (1) at least one entry; (2) EVERY entry names a track at exactly
  that path, byte for byte, so one unresolved or fold-only entry keeps the
  file listed; (3) every entry's parent folder is the playlist's own, by
  bytes; (4) the tracks named are exactly the tracks directly in that folder,
  order and repeats ignored. A track there is any indexed row, a suppressed
  duplicate included, judged before the duplicate mapping. **And the file is
  judged at all only on whole evidence about its folder** (the app's final
  B421 gate, acoseac/1-bit#2237): the last scan that saw the file saw that
  folder whole, meaning no failed or cut-short read of it or of a folder
  above it, no entry in it the walk could not see, and a row for every audio
  file the walk found directly in it. Unsure, it is listed: an index that
  under-counts the folder (a new file it could not index) would let an
  ordinary playlist naming the indexed tracks meet term 4. The evidence is a
  finished scan's, taken after its walk's rows landed; a scan under way only
  adds rows to a folder before its pass, which can only take a file out of
  the rule. Change the rule only with the app's, in the same pair. **Don't
  serve a suppressed path as a member, don't let an unresolved entry count
  toward the album rule, don't judge a file whose folder evidence is not
  whole, don't put the store's error in the 500** (B425's fixed message),
  **don't serve a page at a version its cursor was not made at, don't take a
  cursor whose signature does not check, don't let a page's budget mark a
  file `tooLarge`, don't cost a string by anything but `jsonStringBytes`, and
  don't add a column a page reads without moving the v57 triggers' `WHEN`
  with it**; bump
  `playlistFeedRulesVersion` with any change to what the feed answers for
  the same rows. Tests:
  `TestPlaylistFilesAnswers304ToAMatchingETag`,
  `TestAlbumFolderPlaylistsAreNotServed`, `TestTheAlbumFolderRuleTermByTerm`,
  `TestASuppressedDuplicateMemberIsServedAsItsServedCopy`,
  `TestAPlaylistFilesStoreFailureAnswersAFixedMessage`,
  `TestAClientAbortDuringThePlaylistFilesReadIsInfoAnd499`,
  `TestAnAlbumFolderPlaylistIsServedWhileItsFolderHoldsAFileTheIndexLacks`,
  `TestAnAlbumFolderPlaylistBesideALinkTheWalkCannotFollowIsServed`,
  `TestPlaylistFilePagesConcatenateToTheWholeFeed`,
  `TestTheFeedVersionMovesWithEveryChangeAPageCanShow`,
  `TestAPageAskedForAtAnotherVersionIsRefused`,
  `TestTheFeedGenerationTriggersAreInPlace`,
  `TestAPlaylistFilesCursorFromAnotherVersionIs410`,
  `TestAPlaylistFilesCursorTheBridgeDidNotMakeIs400`,
  `TestAPlaylistFileCostsAtLeastItsJSON`,
  `TestJSONStringBytesIsWhatEncodingJSONWrites`.
