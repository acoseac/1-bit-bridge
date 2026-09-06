# Engineering log — the full record behind CLAUDE.md's invariants

`CLAUDE.md` carries the **rules**: what must not be undone, and enough of the
why to make each rule act on. This file carries the **record** each rule was
distilled from — the measurement that settled it, the alternative that was
tried and rejected, the test that pins it, the review round that caught it, and
which PR shipped it.

**When to read this.** Before changing anything a CLAUDE.md invariant names.
Grep for the symbol, the file, or the PR number; the entry will tell you what
was already tried and why it did not work. A rule in CLAUDE.md that looks
arbitrary almost always has a measurement behind it here.

**When NOT to read this.** It is not auto-loaded and does not need to be. If
CLAUDE.md's rule is enough to act on, act on it.

**Adding to it.** New batches append here in date order. Put the *rule* in
CLAUDE.md and the *record* here — never only here, because nothing in this file
reaches a session that has not gone looking for it.

> Entries are preserved verbatim from CLAUDE.md as of the 2026-09-02 split.
> Some carry corrections applied later in the same entry; where an entry says a
> claim was "STALE" or "corrected", the correction is the current one.

---

## Index

- [LOUPE on the lyrics surface (PRs #849 / #850 / #851, 2026-09-06)](#loupe-on-the-lyrics-surface-prs-849--850--851-2026-09-06)
- [ALAC upscaling — the ffmpeg fallback, and three things only measurement settled (issue #127, 2026-09-02)](#alac-upscaling--the-ffmpeg-fallback-and-three-things-only-measurement-settled-issue-127-2026-09-02)
- [File-provenance spectrum — floors, the decode filter's transition band, and the v34/v35 pair (PRs #684–#688 + iOS #1338–#1341, 2026-08-14)](#file-provenance-spectrum-floors-the-decode-filters-transition-band-and-the-v34v35-pair-prs-684688-ios-13381341-2026-08-14)
- [2026-08-15 — the 2026-08-14 feature-review fix batch, bridge half (PRs #698 / #699)](#2026-08-15-the-2026-08-14-feature-review-fix-batch-bridge-half-prs-698-699)
- [Favorites — singleton LWW document + the `favorites` smart-mix family (PRs #695 / #697, iOS #1347/#1351–#1353, 2026-08-14)](#favorites-singleton-lww-document-the-favorites-smart-mix-family-prs-695-697-ios-134713511353-2026-08-14)
- [Launcher menu + shell-aware handoff (PRs #63 / #64 / #65 / #66)](#launcher-menu-shell-aware-handoff-prs-63-64-65-66)
- [v0.1.2 batch — updater + advertise + TLS-SAN broadening (PRs #89 / #94 / #95 / #96 / #97)](#v012-batch-updater-advertise-tls-san-broadening-prs-89-94-95-96-97)
- [Multi-disc folder art — parent-dir fallback + version-stale diff-guard + ExtractorVersion 2 (PR #606, 2026-07-30)](#multi-disc-folder-art-parent-dir-fallback-version-stale-diff-guard-extractorversion-2-pr-606-2026-07-30)
- [v0.1.3 — local artwork extraction (PR #98)](#v013-local-artwork-extraction-pr-98)
- [v0.1.4 — perf + hardening (PR #99)](#v014-perf-hardening-pr-99)
- [v0.1.5 — Windows artwork robustness (PR #100)](#v015-windows-artwork-robustness-pr-100)
- [v0.1.7 — Tailscale HTTPS auto-pilot (PR #102)](#v017-tailscale-https-auto-pilot-pr-102)
- [v0.1.8 — admin-approval pairing flow (PRs #103 + #105)](#v018-admin-approval-pairing-flow-prs-103-105)
- [v0.1.6 — operator-UX batch (PR #101)](#v016-operator-ux-batch-pr-101)
- [v0.1.9 — admin SSE replaces dashboard polling (PR #107)](#v019-admin-sse-replaces-dashboard-polling-pr-107)
- [v1.2 — offline PCM upscaling foundation + on-demand API (PRs #108 + #109)](#v12-offline-pcm-upscaling-foundation-on-demand-api-prs-108-109)
- [v1.2 — admin console upscale toggle + stats (PR #110)](#v12-admin-console-upscale-toggle-stats-pr-110)
- [v1.2 — public `GET /v1/upscale/stats` endpoint (PR #111)](#v12-public-get-v1upscalestats-endpoint-pr-111)
- [v1.2.x — variant write/delete bumps `tracks.indexed_at` (PR #156, mirror with iOS #255)](#v12x-variant-writedelete-bumps-tracksindexedat-pr-156-mirror-with-ios-255)
- [v1.2 — mDNS rebind on interface set change (PR #113)](#v12-mdns-rebind-on-interface-set-change-pr-113)
- [Cross-page pairing badge (PR #161, mirror with iOS PR #267 Discover sheet)](#cross-page-pairing-badge-pr-161-mirror-with-ios-pr-267-discover-sheet)
- [Bug-fix batch — review-identified regressions](#bug-fix-batch-review-identified-regressions)
- [post-v0.1.4 — config validation split for public-mode VPS deploys](#post-v014-config-validation-split-for-public-mode-vps-deploys)
- [v1.6 — cross-bridge playlist backup + playback telemetry (bridge PRs #334 / #337 / #336, iOS PRs #620 / #621 / #624 / #623)](#v16-cross-bridge-playlist-backup-playback-telemetry-bridge-prs-334-337-336-ios-prs-620-621-624-623)
- [v1.6.x — public-mode pairing QR advertises the served (LE) cert (PR #338)](#v16x-public-mode-pairing-qr-advertises-the-served-le-cert-pr-338)
- [v1.7 — user-wide device state: cross-device playlists + readable history (PR #371)](#v17-user-wide-device-state-cross-device-playlists-readable-history-pr-371)
- [Review-batch invariants (PRs #372 / #373 / #374 / #375, 2026-06-11)](#review-batch-invariants-prs-372-373-374-375-2026-06-11)
- [v1.7.x — library-root flip spares UPnP rows; inspector root derives from tracks (PRs #404 / #405, 2026-06-15)](#v17x-library-root-flip-spares-upnp-rows-inspector-root-derives-from-tracks-prs-404-405-2026-06-15)
- [CLI-hardening batch — doctor Windows port attribution + sox FLAC verify + init re-prompt (PRs #432 / #433 / #434, 2026-06-22)](#cli-hardening-batch-doctor-windows-port-attribution-sox-flac-verify-init-re-prompt-prs-432-433-434-2026-06-22)
- [admin-console UX batch — composition bars + SSE telemetry + worker grid + Camelot wheel + skip badges (PRs #435 / #436 / #437 / #438 / #439, 2026-06-23)](#admin-console-ux-batch-composition-bars-sse-telemetry-worker-grid-camelot-wheel-skip-badges-prs-435-436-437-438-439-2026-06-23)
- [DLNA/UPnP discovery review batch (PRs #469 / #470 / #471, 2026-07-03)](#dlnaupnp-discovery-review-batch-prs-469-470-471-2026-07-03)
- [bridge0901 review batch (PRs #486 / #487 / #488, 2026-07-10)](#bridge0901-review-batch-prs-486-487-488-2026-07-10)
- [v1.8 — PDF album booklets (bridge PR #496 + Atlas #108 + iOS #1041, merged 2026-07-13)](#v18-pdf-album-booklets-bridge-pr-496-atlas-108-ios-1041-merged-2026-07-13)
- [v1.8 — admin enrichment UX (PR #495, merged 2026-07-13)](#v18-admin-enrichment-ux-pr-495-merged-2026-07-13)
- [Inspector coverage + Atlas layer batch (PRs #504 / #505 / #506, merged 2026-07-16)](#inspector-coverage-atlas-layer-batch-prs-504-505-506-merged-2026-07-16)
- [Comprehensive-audit fix batch (PRs #511–#535, merged + deployed 2026-07-19)](#comprehensive-audit-fix-batch-prs-511535-merged-deployed-2026-07-19)
- [Atlas-pointed enrichment — pacing, recall, reclaim (PRs #593 / #595 + atlas#132, 2026-07-29)](#atlas-pointed-enrichment-pacing-recall-reclaim-prs-593-595-atlas132-2026-07-29)
- [Acoustic fingerprinting fallback (PRs #604 / #605 / #607 / #608, 2026-07-30)](#acoustic-fingerprinting-fallback-prs-604-605-607-608-2026-07-30)
- [Enrichment matching — fold before comparing; relax the query, not the acceptance (PRs #600 / #601 / #602, 2026-07-30)](#enrichment-matching-fold-before-comparing-relax-the-query-not-the-acceptance-prs-600-601-602-2026-07-30)
- [Serve-time duplicate suppression (PRs #651–#654, 2026-08-05)](#serve-time-duplicate-suppression-prs-651654-2026-08-05)
- [Smart Mixes per-family actions (PR #657, 2026-08-05)](#smart-mixes-per-family-actions-pr-657-2026-08-05)
- [Diagnostics log export + bug-report bundle (2026-08-16)](#diagnostics-log-export-bug-report-bundle-2026-08-16)
- [Log volume: M-SEARCH send suppression + a log-size check (2026-08-16)](#log-volume-m-search-send-suppression-a-log-size-check-2026-08-16)
- [Auto-optimize: background pre-generation of CarPlay variants (2026-08-17)](#auto-optimize-background-pre-generation-of-carplay-variants-2026-08-17)
- [CodeQL triage — the whole open queue was FP; log-injection is FP BY CONSTRUCTION (2026-08-18)](#codeql-triage-the-whole-open-queue-was-fp-log-injection-is-fp-by-construction-2026-08-18)
- [2026-08-19 bug review — reap-time sidecar reclamation, harvest base-URL pin (PRs #723 / #724 / #725)](#2026-08-19-bug-review-reap-time-sidecar-reclamation-harvest-base-url-pin-prs-723-724-725)
- [Admin web player (2026-08-23) — `/` is a library player, Settings is one screen](#admin-web-player-2026-08-23-is-a-library-player-settings-is-one-screen)
- [Player partial-boost — audio survives navigation to operator pages (PR #742, 2026-08-24)](#player-partial-boost-audio-survives-navigation-to-operator-pages-pr-742-2026-08-24)
- [Console shell: one sidebar replaces two nav levels (2026-08-24)](#console-shell-one-sidebar-replaces-two-nav-levels-2026-08-24)
- [Web player: right-sized artwork, catalog freshness, A–Z, collections (PRs #743–#747, 2026-08-24)](#web-player-right-sized-artwork-catalog-freshness-az-collections-prs-743747-2026-08-24)
- [Variant management moves into Browse; the Inspector is retired (2026-08-24)](#variant-management-moves-into-browse-the-inspector-is-retired-2026-08-24)
- [Detail tabs, smart mixes on the player, and three parity tests (PRs #754 / #755 / #756, 2026-08-25)](#detail-tabs-smart-mixes-on-the-player-and-three-parity-tests-prs-754-755-756-2026-08-25)
- [Favorites on the player, and the router races review found (PRs #757 / #758 / #759, 2026-08-25)](#favorites-on-the-player-and-the-router-races-review-found-prs-757-758-759-2026-08-25)
- [Playlists consolidate into Browse; per-page feature trays (2026-08-26)](#playlists-consolidate-into-browse-per-page-feature-trays-2026-08-26)
- [Settings apply live where it is structurally sane; the rest is reported PER FIELD (2026-08-28)](#settings-apply-live-where-it-is-structurally-sane-the-rest-is-reported-per-field-2026-08-28)
- [Cloud-readiness batch (PRs #797–#803 + iOS #1480, 2026-08-30)](#cloud-readiness-batch-prs-797803-ios-1480-2026-08-30)
- [2026-09-01 improvement batch (PRs #816–#827)](#2026-09-01-improvement-batch-prs-816827)
- [SACD ISO expansion — .iso images mint virtual DST track rows (PR #779, 2026-08-28)](#sacd-iso-expansion-iso-images-mint-virtual-dst-track-rows-pr-779-2026-08-28)
- [Web upload + delete-as-trash (PRs #788–#792, 2026-08-30)](#web-upload-delete-as-trash-prs-788792-2026-08-30)
- [Library sources in the sidebar (PRs #807 / #808 / #809, 2026-08-31)](#library-sources-in-the-sidebar-prs-807-808-809-2026-08-31)
- [Cross-source duplicates and UPnP import — PARKED, and why (2026-08-31)](#cross-source-duplicates-and-upnp-import-parked-and-why-2026-08-31)
- [DIDL is not tags — routed rows fill artist/album from the container path (PR #813, 2026-08-31)](#didl-is-not-tags-routed-rows-fill-artistalbum-from-the-container-path-pr-813-2026-08-31)
- [The original "Things that have bitten before" list (2026-04 → 2026-08)](#the-original-things-that-have-bitten-before-list-2026-04-2026-08)

---

## File-provenance spectrum — floors, the decode filter's transition band, and the v34/v35 pair (PRs #684–#688 + iOS #1338–#1341, 2026-08-14)

The whole-track spectrum (`1BSP`, 84 bytes = 24-byte header + 60 band bytes; `Track.bandwidthHz`; `GET /v1/spectrum`; `spectrum` health flag) answers "is this file an upsampled CD rip?". Five bridge PRs, each finding a defect only REAL data exposed — every synthetic fixture agreed with every other synthetic fixture and they were all wrong together. Invariants:

- **The measurement floor belongs to the AVERAGE, never to a threshold comparison** (#685/#686, iOS #1340). The cliff is `below − above`; clamping `above` at the −90 dB display floor capped the whole metric at `below − floor` — real music at 20 kHz sits at −57…−85 dBFS, so 317 live hi-res tracks in the 44.1 kHz window measured 10–32 dB and NO upsample could ever reach a threshold. The fix (−160 measurement floor) then must NOT leak into the two threshold scans: clamping a comparison can only inflate (quiet file, peak −110 ⇒ content floor −170 ⇒ every bin ≥ −160 passes ⇒ full Nyquist reported). `binDB` (unclamped, −Inf on silence) is for comparisons; `powerToDBMeasured` is for `meanBinDB` alone. Both repos, negative-controlled both ways.
- **`bandwidthCeilingGuardHz` = the decode resampler's TRANSITION BAND (1200 Hz), not a Nyquist margin** (#687). `sox … -r 48000` auto-inserts `rate` with a 95%-of-Nyquist passband, so any file carrying content past 24 kHz gets its measured "stop" placed at 23.0–23.5 kHz BY OUR OWN FILTER with a "cliff" that is the filter's slope — on the full 18k-row wf7 library, 469 of 3,220 hi-res files (14.6%), 12 staged as false "48 kHz source" flags, all-analog Blue Note tape transfers among them; the old 500 Hz guard caught 2. Consequence, and it is PROTOCOL.md's documented contract: **the bridge can never support a "48 kHz source" claim** (the whole 48 k window [22.8k, 24.4k] is guarded); only the 44.1 kHz question is answerable bridge-side. iOS decodes at the file's NATIVE rate with no resampler, so its 48 kHz candidate stays valid for local curves — don't "fix" the iOS candidate list to match.
- **The 60 dB cliff threshold is CONFIRMED from the full distribution — don't retune it on taste.** 44.1 k window: 14 flags at 62.2–86.4 (all corroborated upsamples: Aerosmith GH Deluxe 2023, Cranberries Super Deluxe bonus discs, Einaudi Starkey remixes; "Get A Grip" measures identically — 86.4 dB / 21891 Hz — from two releases eight years apart), then an EMPTY gap down to 56.9. Recorded caveat, deliberately not acted on: the cliff's `above` strip partially overlaps the transition band for 44.1 k-window files (inflates cliffs of files with real ultrasonic content — the false-positive direction); the calibration was derived end-to-end through the same pipeline, so it cancels. Capping the strip at the passband edge = full re-calibration + schema bump; only with evidence of an actual false positive.
- **Migrations v34+v35 are a UNIT; 22800 in them is FROZEN** (the wf7-era passband edge, never a reference to the live analyze constant). v34 heals the 567 stored artifact rows as a deterministic DATA FIX — column NULL, blob patched to the encoder's absent form (bandwidth 0 / cliff 0xFFFF), strict-advance `indexed_at` — legitimate ONLY because the corrected output was derivable from the stored one; ~38 h of re-analysis avoided for a byte-identical outcome. **SQLite gotcha that produced v35: a substr/concat expression over a BLOB stores its result TEXT-typed** (bytes intact, type wrong), and SQLite's string functions NUL-truncate on text (a 1BSP header has a NUL at byte 6) — so `length()` reported 5 for a healthy 84-byte value and the first post-deploy verification misread the migration as having corrupted the blobs. Go's `Scan` returns full bytes either way (wire never affected). v35 is the lossless `CAST(… AS BLOB)` retype; any future blob-surgery migration should CAST its result up front.
- **Pin cross-cycle contracts with CAPTURED bytes, never a second copy of the offsets.** Three review rounds each caught the same class: a hand-built fixture asserts its author's beliefs, and two hand-written copies can be wrong TOGETHER (the "80-byte" 1BSP fixture + doc were). The shapes that survived: `internal/api`'s fixture goes through the real `EncodeSpectrum` (test-only import of analyze — production keeps them apart); the heal test's `wf7MeasuredBlob`/`wf7AbsentBlob` are captured encoder output (manifest can't import analyze — cycle); and `TestMigrationLadderHealsAndRetypes` drives the REGISTERED migrations by rewinding `PRAGMA user_version` to 33 and reopening (sanctioned: idempotency is the ladder's own crash-recovery contract). Negative-control every such pin — two of these passed under the bug until the fixture was made honest.
- **The analysis sweeper's post-restart collection pass is ~7 minutes of SILENCE** (stat + `GetAnalysis` for ~19,900 tracks on the B2 FUSE mount) before anything is enqueued or logged — progress sampled inside that window looks exactly like a stall (frozen row count, no decoders, no log lines). The recovery line is `auto-analysis sweep enqueued tracks (queue now full) count=5001`; wait for it before concluding a deploy broke the backfill.
- **`Track.BPMEstimated` is set ONLY at the tag-absent splice and zeroed unconditionally in `marshalForStorage`** (PR #689, the full-tier batch's wire half; iOS mirror in the key/tempo-chips PR). It is the wire form of `bpmFromAnalysis`, and its contract is asymmetric on purpose: **only a positively-marked estimate may be labelled "estimated" by a client — absence makes NO claim** (a curated tag, a pre-#689 bridge, and no bpm at all are indistinguishable without it, and all three must render unlabelled; mislabelling a user's curated tag is the failure the field exists to prevent). Don't set it anywhere but the `t.BPM == nil && kt.BPM != nil` branch of `spliceAnalysisScalars`, and don't drop the `marshalForStorage` zero — frozen into `tags_json` it would label a LATER curated tag as an estimate. Both directions negative-control-verified (`TestManifestSplicesKeyTempo` red without the splice marker; `TestSplicedKeyTempoNotPersistedOnRoundTrip` red without the zero). Additive omitempty; ProtocolVersion stays 1.
- **DSD stays OUT of the spectrum pipeline — measured, not assumed (2026-08-14; keep the `.dsf`/`.dff` skip in `collectAnalysisCandidates`).** 240 real DSFs from bridge.ars.md (232 DSD64) were decoded through the pipeline's own ffmpeg shape at 48 kHz AND at 176.4 kHz (one-off probe; ground-truth-validated first — a synthetic CD upsample read cliff 105 dB). Result: the 48 kHz view puts **49/240 inside the 44.1 kHz candidate window** with a cliff CONTINUUM to 58.9 dB and **no gap anywhere** (the PCM library has a clean 56.9→62.2 gap); the wide view proves 16 of those are genuinely PCM-heritage (wall-drop ≥20 dB at exactly the CD band, troughs pinned 22.7–24 kHz vs native p50 27 kHz) — but their cliffs (26.1–57.7) OVERLAP files with no wall evidence (up to 37.0). **The DSD64 noise shelf back-fills a ~100 dB PCM wall into the same 26–58 dB range native rolloffs occupy: the populations do not separate, so no threshold is defensible — one that catches known PCM-heritage SACDs accuses native DSD.** Routing DSD through the ffmpeg fallback works mechanically and was deliberately NOT shipped (the curve would carry a verdict-shaped, verdict-meaningless bandwidth/cliff). iOS carries the structural guard (`SignalPathTrackQuality.sourceIsDSD` gates both accusation surfaces) so even a future DSD-curve producer can't re-open the path. The remaining distribution facts: guard fires on 22/240 (9%); bw48 p50 = 20.1 kHz (the music's own ceiling, below the shelf at DSD64); cliff p50/p90/p99 = 13.4/36.8/51.6. Revisit only with a measurement that separates the populations (shelf-model subtraction at a wide analysis rate — research, not a retune); re-run a fresh probe rather than trusting stale CSVs.
---

## 2026-08-15 — the 2026-08-14 feature-review fix batch, bridge half (PRs #698 / #699)

The bridge side of the batch iOS shipped as PRs #1355–#1370 (its CLAUDE.md carries the
full record). Two PRs here, both additive — `ProtocolVersion` stays 1:

- **Playlist tombstone ids ride the list response** (#699, the B2 delete-propagation
  half): `GET /v1/playlists` gains `deletedIds` (omitempty), listing tombstoned
  playlist ids so a second device's pull sweep can delete locally instead of resurrecting.
  **The wire field is `deletedIds`** — this entry said `deletedPlaylistIDs` until
  2026-08-16, which matched nothing: the Go tag, PROTOCOL.md in both repos, and iOS's
  `BridgePlaylistsListResponse.deletedIds` have always agreed on `deletedIds`. Verify a
  field name against the tag before "fixing" code to match a doc.
  **Tombstones are read AFTER the live rows as a SECOND plain query — deliberately NOT a
  shared read-only transaction** (corrected 2026-08-16; this entry claimed the opposite
  since #699, and `internal/api/playlists.go`'s own comment has always said otherwise —
  don't "restore" a transaction that was never there). A playlist tombstoned between the
  two reads therefore appears in BOTH lists, and that is the accepted interleaving: a
  client processing tombstones first self-heals, because the follow-up
  `GET /v1/playlists/{id}` for a both-listed id 404s and is skipped. The reverse order
  (revived between the reads → in neither list) costs one sweep. Contrast `GetFavorites` /
  `ListFavoritesForAdmin`, which DO wrap their multi-query reads in one
  `BeginTx(ReadOnly: true)` — favorites are a single LWW document whose meta and entries
  would genuinely tear; playlist summaries and tombstones are independent lists.
  A revived playlist (same id re-PUT) drops off the
  tombstone list — the revive query is error-checked, not `_`-swallowed. The iOS consumer
  (#1370) additionally records ITS OWN deletes durably (`PendingPlaylistDeleteStore`) and
  retries the wire DELETE per bridge (404 = landed), so a failed best-effort DELETE
  converges instead of resurrecting.
- **B9 batch** (#698): UPnP-upstream tracks route genre through the same normalization
  as local scans (genre lands on the NEXT upstream walk's re-upsert — existing rows
  don't heal in place); delta manifests omit the `folders` block (full-sync-only, the
  documented shape); admin Data page surfaces upstream-vs-local provenance.
- **Test-hardening lesson repeated** (#699 round 2): omitempty absence is asserted by
  `json.Unmarshal` into `map[string]any` + key-absence — never a substring probe on the
  raw body (a substring match can't distinguish `"deletedIds":[]` from absent).
- Deploy reminders (operator-driven, NONE made from the fix sessions): bridge.ars.md
  wants a build with #698+#699; home-pc's favorites deploy is ungated once an app build
  with iOS #1355 (favorites first-sync adopt) is on the user's devices.
---

## Favorites — singleton LWW document + the `favorites` smart-mix family (PRs #695 / #697, iOS #1347/#1351–#1353, 2026-08-14)

Per-device favorite tracks + albums, backed up per-bridge (iOS opt-in, default OFF).
Design source of truth: iOS repo `docs/FavoritesDesign.md`; wire section in
PROTOCOL.md is byte-mirrored to the iOS repo's `docs/BridgeProtocol.md`. Additive —
`ProtocolVersion` stays 1; `favorites` health feature flag gates the iOS toggle.

- **ONE document, LWW by `last_modified_at`.** `favorites_meta` (CHECK(id=1)) +
  `favorite_tracks`/`favorite_albums`; PUT is a wholesale replace inside one
  transaction; a strictly-OLDER stamp gets **409 carrying the FULL server doc**
  (the iOS union-merge depends on the whole doc riding the 409 — if the post-409
  re-read fails, return 500, never a doc-less 409); an EQUAL stamp is accepted
  (idempotent re-push). No DELETE route — an empty PUT is the delete.
- **Local-XOR-foreign identity is enforced at THREE layers** and all must stay:
  API validation (`favorites.go` rejects mixed/partial rows), store validation
  (`UpsertFavorites` re-checks per row — the API is not the only conceivable
  caller), and the `favorite_tracks` table CHECK in **migration v36** (edited
  INTO v36 rather than a v37 because v36 never shipped in a release — don't
  repeat that move once a tag exists). Local rows = `path` set; foreign rows =
  `origin_fingerprint` + `origin_path` set, stored VERBATIM (opaque — another
  bridge's identity, never normalized here).
- **Local paths normalize with `strings.TrimPrefix(p, "/")` — exactly ONE
  leading slash, deliberately.** This is the `DLNATrackIDHasher` single-slash
  convention (iOS stores bridge paths slash-prefixed; the manifest stores them
  slashless). A bot-suggested `TrimLeft` would collapse structurally distinct
  paths (`//x` vs `/x`) — declined, don't re-apply.
- **Reads are snapshot reads.** `GetFavorites` AND `ListFavoritesForAdmin` each
  wrap meta + tracks + albums queries in one `BeginTx(ReadOnly: true)` so a
  concurrent PUT can't produce a torn meta-vs-entries view. The admin listing
  LEFT JOINs `tracks` for display metadata (falls back to the raw path when the
  row is gone); surfaced on the admin data page's Favorites panel.
- **Body decode is strict**: caps enforced during streaming decode
  (`decodeCappedArray`), trailing top-level JSON rejected via `dec.Token()` ==
  `io.EOF` (`dec.More()` cannot see a stray `]`), dedup keys are comparable
  STRUCTS (`trackKey{path, originFingerprint, originPath}`) — never
  NUL-joined strings, since JSON strings may contain escaped NULs.
- **The `favorites` smart-mix family pools ONLY bridge-local favorites.**
  `FavoritedTrackFeatures` rides `trackFeatureSelect` with an IN-subselect on
  `favorite_tracks WHERE path IS NOT NULL` — dupe-suppressed + deleted rows are
  excluded for free, and foreign refs can never satisfy the floor. The query is
  a true Go `const` (`const q = trackFeatureSelect + …`) — the shape that keeps
  SonarCloud `go:S2077` quiet; don't rebuild it with fmt.Sprintf. `MinFavorites`
  (5) floor-defaults on a zero-value Options (the `buildOnRepeat` convention —
  an un-configured engine must not emit an empty family; this exact bug shipped
  in the first draft and broke `TestGenerate_AllFamiliesInOrder`). Weekly
  `WeekSeed^offset` shuffle; Camelot-harmonic arm when `AnalysisEnabled` and ≥5
  hearts carry a key, with un-keyed hearts APPENDED (never dropped). Registered
  right after Heavy Rotation; hydration in `smartplaylistgen` is NOT
  analysis-gated (hearts without analysis still mix, un-harmonically).
- **The three device→bridge upload stores (playlists / favorites / history)
  are deliberately UNGATED bridge-side** — no config kill switch, unlike every
  other data feature (decided 2026-08-14 feature review, P2-38): the
  single-operator trust model holds (the operator controls pairing, and each
  store is opt-in per-device on iOS), so a bridge-side gate would only shadow
  the iOS toggles. A future multi-tenant posture would need config gates here.
---

## Launcher menu + shell-aware handoff (PRs #63 / #64 / #65 / #66)

A captured PowerShell transcript showed a fresh user running `bridge init`, getting a `bridge serve --config <path>` command in the post-init handoff, typing it back unchanged, and hitting `CommandNotFound` three times before guessing `.\bridge serve --config ...` (PowerShell doesn't search CWD by default). The four-PR sequence below fixed the underlying class of failure and added a context-aware launcher menu for first-time operators.

- **`menuLoop`'s outer ctx MUST be `context.Background()` — NOT signal-wired.** Each "Start now" action creates its own `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)` scope inside `actStartNow`, runs `runServeForMenu`, and returns to the menu when the user hits Ctrl+C. Sharing a signal-wired parent across calls would lock out every invocation after the first Ctrl+C — Go contexts can't be un-canceled. `TestServeContextNotShared` pins the contract via a stub-runServe seam; don't replace it without an equivalent regression gate. SIGTERM at the menu's input prompt falls through to Go's default signal handler (terminate process), which is the right UX at a synchronous prompt — `bufio.ReadBytes` doesn't observe ctx anyway. (PR #65)

- **`shellHandoff` renders FLUSH-LEFT, never inside the frame.** Production install paths like `~/Library/Application Support/1-bit-bridge/bridge.yaml` quoted-and-prefixed produce >70-char commands; the 55-col frame would mid-truncate through `serve --config` and leave the user with an uncopyable garbled string. The frame stays as a visual header ("to start the bridge later, run:") with no body lines that need truncating; `writeShell{PS,Cmd,Posix}` emit the labeled command to a `strings.Builder` with shell-specific line continuations (`` ` `` for PS, `^` for cmd, `\` for bash). Width discipline applies only to the cinematic boxed elements (logo, status, options, fingerprint). `TestShellHandoffPreservesLongPaths` is the regression gate. (PR #66)

- **TLS fingerprint MUST be split, never truncated.** A SHA-256 colon-separated hex is 95 chars and operators copy it byte-for-byte to the iOS pin field. `splitFingerprint(fp) (first, second)` splits on a colon boundary so `first + second == fp` verbatim; `TestSplitFingerprintIsLossless` covers 6 input shapes including the no-colons pathological case. Don't reintroduce `truncateMid(fp, ...)` in the rendering path — Gemini flagged this as critical on PR #64's first push because a truncated fingerprint silently breaks every paired client. (PR #64)

- **Path escaping is shell-specific.** `quotePS` backtick-escapes `` ` ``, `"`, `$`. `quoteCmd` doubles internal `"` (cmd's only escape inside `"..."` — backslash is fine, paths round-trip). `quotePosix` uses the standard `'\''` trick. On Windows we ALWAYS print BOTH PowerShell AND cmd.exe variants because `$PSModulePath` is set in BOTH shell environments — guessing wrong with single-shell detection brings the original transcript bug back. `TestQuoteHelpersHandleHostilePaths` covers spaces, embedded quotes, single quotes, and `$HOME` expansion across all three shells. (PR #64)

- **`colorEnabled` requires BOTH stdout AND stderr to be TTYs.** `shellHandoff` is written to stderr in two error branches (service-install fail, spawn fail). A stdout-only TTY check would leak raw `\x1b[95m` bytes into a redirected stderr log. Windows additionally requires `windows.SetConsoleMode(handle, ENABLE_VIRTUAL_TERMINAL_PROCESSING)` to succeed before we commit to color — `initTerminal()` returns bool on both platforms (true on POSIX always; on Windows iff the SCM-mode flip succeeded), and `colorState.on = initTerminal()` gates on that. Stdlib `syscall` exports `GetConsoleMode` but NOT `SetConsoleMode` on Windows; the Set side lives in `golang.org/x/sys/windows`, which the bridge already imports directly via `internal/packaging`'s SCM code (no new dep). (PR #64)

- **`runeWidth` and frame truncation MUST strip ANSI before measuring.** `stripANSI(s)` removes CSI / SGR escape sequences so `\e[95mhello\e[0m` measures as 5 columns visible, not 13 bytes. Without this, colored body lines would shift the right border left and long colored strings could truncate mid-escape (corrupting the rest of the terminal). `box` and `frame` always strip ANSI in their truncation path. (PR #64)

- **`readMenuChoice` discards any line containing `\x1b`.** Users press ↑/↓ instinctively at a prompt; cooked-mode bufio reads deliver the raw `\x1b[A` bytes when the user finally presses Enter. The line-discard policy keeps the cinematic frame intact and silently re-prompts on bare-arrow input. **Documented limitation: bufio.ReadBytes blocks until newline, so a bare ↑ shows nothing until Enter — DO NOT file as "menu hangs on arrow key".** Trade-off for not importing `golang.org/x/term` raw mode. (PR #65)

- **Windows `IsAdmin()` uses a stdlib-only PHYSICALDRIVE0 probe** (with `PHYSICALDRIVE1` and SCM-Connect fallbacks for hypervisor / Sandbox / minimal CI environments). `os.Open(\\.\PHYSICALDRIVE0)` succeeds only with admin rights. We could use `windows.GetCurrentProcessToken().IsElevated()` from x/sys, but the PHYSICALDRIVE trick is stdlib-only and the worst-case mis-detection is cosmetic (the menu shows the "(Requires Administrator)" hint to a real admin who can still pick the option). `IsRoot()` on POSIX is `os.Geteuid() == 0` — used to warn against `sudo bridge` install (would resolve `$HOME` to `/root` and silently break the config dir). Cached via `sync.Once` on Windows; elevation doesn't change inside a process lifetime. (PR #63)

- **`packaging.Stop` / `Restart` reject system-level installs.** `KindLaunchdSystem` and `KindSystemdSystem` (sudo / non-supported install paths) return `ErrSystemInstallNeedsRoot` rather than calling user-context managers (`launchctl gui/<uid>`, `systemctl --user`) which would silently no-op against the wrong namespace. macOS Stop also captures `bootout` output and only swallows the canonical "Could not find" / "not currently loaded" sentinels — any other failure surfaces. Windows OpenService errors are classified via `errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)` (idempotent) vs `ERROR_ACCESS_DENIED` (admin-needed wrap) vs anything else (real fault). qodo flagged the original "swallow-everything" pattern on PR #63's first push. (PR #63)

- **`actSetup` MUST pass the menu's shared `*bufio.Reader` to `initCmd`, not bare `os.Stdin`.** Two `bufio.Reader`s wrapping the same fd each maintain their own buffer; if the menu pre-buffered the next chunk, the inner `initCmd` reader would skip those bytes and prompts would desync at unpredictable moments. The wrapping pattern (`bufio.NewReader` over an existing `*bufio.Reader`) works correctly because the inner reader satisfies its reads from the outer's buffered stream. qodo catch on PR #65. (PR #65)

- **`actInstallService` MUST mirror init.go's install pattern exactly.** That means: resolve binary path with `os.Executable + EvalSymlinks + argv[0]` fallback, error-check `packaging.DefaultLogPath()`, `os.MkdirAll(filepath.Dir(logPath), 0o755)`, set `params.WorkingDir = filepath.Join(filepath.Dir(s.cfgPath), "data")`. The launchd plist and systemd unit templates embed `.WorkingDir` as `WorkingDirectory`; an empty string installs a service that "succeeds" but whose process can't resolve relative paths. qodo catch on PR #65. (PR #65)

- **`actOpenAdmin` MUST load `cfg.AdminAddress` from the resolved config**, NOT hardcode `http://127.0.0.1:7789/`. `runServe` binds and advertises the admin console from the same field, so a hardcoded URL 404s whenever the operator customised the admin port. Both Gemini and CodeRabbit flagged this on PR #65. (PR #65)

- **Init-write permissions: `0o700` for dirs, `0o600` for files on POSIX.** Includes the config dir, data dir, cert/key dir. `os.MkdirAll` preserves existing-dir mode, so `os.Chmod(dir, 0o700)` follow-up is what hardens upgrades from a previous `0o755` install. Chmod errors are non-fatal (some filesystems ignore POSIX modes); print a warning and continue. **On Windows the Go file mode is advisory only** — protection there relies on per-user-profile NTFS ACLs at `%LOCALAPPDATA%`, which already block other standard users without an `icacls` shell-out. Windows install path resolves under `%LOCALAPPDATA%`, never `%PROGRAMDATA%`. (PR #63)

- **`frame width = 55` for cinematic elements.** `frameWidth` const in `styles.go`. Half-width tmux pane (typical 40 / 66 cols) survives without the right border wrapping. Long paths inside frames go through `truncateMid`. `TestFrameWidthBudget` asserts every line of every frame is exactly `frameWidth` runes wide — this is the regression gate against accidental over-wide lines.

- **`bridge` no-args TTY gate.** `main.go:264` checks `isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())` — both must be terminals before entering `menuLoop`. Pipes (`bridge | cat`), redirects (`bridge > out.txt`), CI scripts all hit the existing `usage + exit 2`. Output redirected to a file MUST contain zero `\x1b` bytes. The `--help` / `-h` explicit flag goes through usage, NOT the menu — only the bare no-args TTY case enters the menu. (PR #65)

- **Hardening batch (PRs #70 / #71 / #72 / #73).** Eight load-bearing invariants from the operator-feedback batch:
  - **`fs.Resolver.Resolve`'s prefix check uses `TrimSuffix`** before appending the separator. Without it, a filesystem-root library configuration (Docker mount-to-`/` on Unix, `C:\` on Windows) builds `"//"` and rejects every path. `TestResolveAcceptsFilesystemRoot{Unix,Windows}` is the regression gate. (PR #70)
  - **Legacy `/v1/manifest` MUST stream from `manifest.WriteManifest`** — never reintroduce in-memory `[]Track` materialisation in the handler. Per-row JSON unmarshal × 50k tracks alone allocates >200 MB on Pi-class hosts. `Store.StreamTracks(sp, fn)` reuses **one `Track` allocation across iterations** (zeroed via `t = Track{}` per row); callbacks MUST NOT retain. The handler defers `WriteHeader(200)` via `deferredStatusWriter` so a pre-stream DB error surfaces as 5xx instead of `200`-with-truncated-body. `WriteManifest` uses a deferred `bw.Flush()` via named return so an early-return on stream error still ships the buffered prefix. The `ManifestProvider` interface uses `WriteManifest(io.Writer, time.Time) error`; v1.1 paginated path is untouched. (PR #70)
  - **TLS cert validity capped at 397 days** (Apple ATS enforces 398-day max for certs issued after 2020-09-01; iOS 26.4's lower-layer applies it independently of `NSAllowsLocalNetworking`). Yearly re-pair is by design — `logIfExpiringSoon` warns at ≤30 days during `LoadOrGenerate`. **Don't raise `certDuration` past 398 days** — iOS rejects at TLS handshake before pinning runs. (PR #70)
  - **`auth.Store.persist` calls `tmp.Chmod(0o600)` after `os.CreateTemp`** as belt-and-braces against unusual filesystems whose ACLs widen perms on close. (PR #70)
  - **Scanner runs as `walker → NumCPU workers → 1 writer` pipeline**. Walker drives `filepath.WalkDir` and inlines folder upserts (single writer, no contention). Workers do `GetTrack` early-skip + `Extract`. Writer batches into `scanBatchSize`-row (500) flushes via `Store.UpsertTrackBatch` — one `BEGIN`/`COMMIT` + reused prepared statement per batch. The previous shape paid one fsync per row (50k transactions on a 50k-track library) AND left multi-core extraction unused. **`s.progress` reflects rows COMMITTED**, not extracted-but-pending. `UpsertTrackBatch` pre-marshals rows OUTSIDE `s.mu` so JSON serialisation × N doesn't extend the critical section. Cancelled scan never leaves `CountTracks != Scan-return-count` (no leaked partial batch). (PR #71)
  - **Listing sort uses `sortEntriesByName` (decorate-sort-undecorate)** with a parallel `[]string` of precomputed lower-cased keys. Previous `sort.Slice + lessCaseFold` allocated 2 strings × O(N log N) comparisons (~22k allocs per 1000-entry directory). **`Less` tie-breaks on original `Name` when folded keys match** — without it, fold-equal entries permute arbitrarily under `sort.Sort` and cause iOS list flicker. (PR #71)
  - **`internal/advertise` filters Windows host-only virtual-switch interfaces** via `isVirtualSwitchInterface` (case-insensitive prefix match on the **parenthesised type label**: `vEthernet (Default Switch)` / `(WSL)` / `(Internal)` / `(Private)` / `(nat)`, plus `WSL`, `VirtualBox*`, `VMware*`, `VBoxNet*`, `Docker*`, `Npcap Loopback*`, `Bluetooth*`). **Hyper-V `vEthernet (External*)` is deliberately NOT filtered** — on hosts that bridge their LAN through an external switch, that adapter is the only physical-LAN-carrying interface and a blanket `vethernet` matcher would silently drop the real LAN endpoint. `cmd/bridge/menu.go actRestart` health-probes the admin port via `waitForListen` (200ms cadence, 5s deadline, `ctx`-aware `DialContext`) so "service restarted." stops lying when the new process hasn't bound its admin socket yet. `adminAddrFromCfg` falls back to `config.DefaultAdminAddress` on any `config.Load` error — a typo elsewhere in YAML MUST NOT silently disable the probe. (PR #72)
  - **Wizard's "Startup-folder launcher" choice routes through `packaging.InstallStartup` (strict)**, never the two-tier `packaging.Install`. `Install` tries SCM first via `tryInstallWindowsService` and falls through only when SCM access is denied — under Administrator the SCM path always succeeds, silently overriding the operator's choice. The `else`-branch in `cmd/bridge/init.go finishInit` routes ALL non-`useService` Windows installs through `InstallStartup` regardless of `choice.spawnNow` — so non-interactive `bridge init --yes` (no `--service` flag) also gets the strict path. `installedKindForOS` itself was correct — it always reported reality; the bug was the install-path choice. `InstallStartup` returns `("", nil)` on non-Windows so callers without a `runtime.GOOS` branch fall back cleanly. (PR #73)

- **Manifest enrichment-progress hint** (additive, `ProtocolVersion` stays at `1`). Two new fields in `internal/manifest/types.go`: `Track.Enriched *bool` (per-row) and `Manifest.EnrichmentProgress *EnrichmentProgress` (top-level). Three load-bearing invariants: (a) **`Track.Enriched` is column-only** — spliced from the `enriched_at` column at READ time in `ListTracks` / `ListTracksPage` and MUST NOT be persisted into `tags_json`. `UpsertTrack` and `MarkEnriched` route through the `marshalForStorage(*Track)` helper that clones the struct and zeros `Enriched` before marshalling. Without this, a caller that takes a Track from `ListTracks` (Enriched-spliced) and feeds it back into a write path would leak the spliced value into the JSON blob, and `GetTrack` / `UnenrichedTracks` (JSON-only readers) would return a stale flag that contradicts the column. CodeRabbit caught the latent risk on PR #68; pinned by `TestUpsertTrackDoesNotPersistEnrichedField` / `TestMarkEnrichedDoesNotPersistEnrichedField`. (b) **`EnrichmentProgress.LastEnrichedAt` is `*time.Time`, NOT `time.Time`.** Go's `omitempty` does NOT drop a zero `time.Time` — the encoder doesn't treat the time-struct's `IsZero` as "empty", so a non-pointer would emit `"0001-01-01T00:00:00Z"` on the wire and the iOS decoder would parse that as a real, very-old date — breaking both the "never enriched" sentinel AND the iOS-side 24 h freshness gate. Gemini caught it on PR #68 review. Pinned by `TestEnrichmentProgressOmitsLastEnrichedAtWhenNeverEnriched`. Same shape applies to any future `omitempty time.Time` field. (c) **`Store.EnrichmentCounts()` deliberately returns `(enriched, lastEnrichedAt, err)` and NOT total.** Adding total back here would re-introduce a divergence window with `BuildManifestPage`'s `CountTracks()` call (concurrent `UpsertTrack` / `DeleteTrack` between the two queries could let `manifest.total` and `enrichmentProgress.tracksTotal` disagree in the same response). Qodo caught it on PR #68; pinned by `TestEnrichmentProgressTotalMatchesManifestTotal`. The field is populated only on the first page of a paginated response (matching the `Folders` / `Total` first-page-only convention). (PR #68)
---

## v0.1.2 batch — updater + advertise + TLS-SAN broadening (PRs #89 / #94 / #95 / #96 / #97)

- **Makefile MUST inject `ServerVersion` for `make build` artefacts** (PR #89). Goreleaser is not the only build path — CI / dev loops / locally-built snapshots an operator might run all skip goreleaser entirely. Without `-X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$(VERSION)` on the Makefile's LDFLAGS, the binary reports the source default `0.0.1` forever, and the dashboard's "Updates" card can't compare against GitHub's latest tag. `VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)` auto-derives a meaningful string (e.g. `v0.1.1-5-gabcdef-dirty`); explicit override still wins. `.goreleaser.yaml` carries an aligned comment block reminding future maintainers to mirror the injection.
- **`updater.ErrNoReleasesPublished` is the canonical "private repo OR no releases" sentinel** (PR #89). 404 from `/releases/latest` AND prerelease/draft branches both return it; `LatestRelease` MUST NOT regress to the silent `(&Release{}, nil)` short-circuit (left the dashboard's Updates card stuck on "checking…" forever — `LastError == "" && LatestVersion == ""` is the "checking" template branch). `Updater.checkOnce` clears `LatestVersion` / `UpdateAvailable` / `ReleaseNotesURL` when `errors.Is(err, ErrNoReleasesPublished)` — without this, a previously-cached "update available" state from before a repo went private masks the new "check failed" badge (Qodo on PR #89 round 1; the dashboard template branches on `UpdateAvailable` first, then `LatestVersion`, then `LastError`). Transient errors (rate limit, network blip) leave cached state alone — operators see the last good answer while the bridge retries. `TestUpdaterClearsCachedAvailabilityOn404` is the regression gate.
- **`semverGreater` treats invalid `current` as `v0.0.0`** (PR #89). `make build` on a tagless / shallow clone stamps `ServerVersion` with a bare git short-SHA via `git describe --always`; the previous "any invalid → false" rule meant those builds couldn't see updates AT ALL. Now an invalid current is treated as the floor so any valid release surfaces as an upgrade candidate. Invalid `latest` is still rejected — a malformed remote tag isn't comparable. `TestSemverGreater_TreatsInvalidCurrentAsZero` covers dev / SHA / dirty-describe forms.
- **Virtual-iface filter uses an inverse heuristic for the Hyper-V `vEthernet (...)` family** (PR #94, supersedes the prefix-allowlist from PR #72). Drop every `vEthernet (` interface UNLESS the parenthesised label contains a token from `vEthernetPhysicalCarveOuts` (`external`, plus physical-NIC vendor tokens: `realtek`, `intel`, `broadcom`, `marvell`, `killer`, `qualcomm`, `atheros`, `mediatek`, `aquantia`, `mellanox`). The carve-out check uses `strings.Contains` so multi-NIC trailing-index variants (`vEthernet (Default Switch) 2`, `vEthernet (External Switch) 3`) are handled correctly. **Catches every future Hyper-V rename without per-name allowlist updates**; the carve-out list grows additively when new physical-NIC vendor names show up. Standalone non-Hyper-V vendor prefixes grew with VPN-style vNICs (`teamviewer`, `zerotier`, `hamachi`, `radmin`, `tap-`, `tunnelblick`) AND Linux virtualisation prefixes (`vmnet`, `vboxnet`, `virbr`, `br-`). Bot suggested a fallback "keep one vEthernet IP if filtering would drop ALL endpoints" — **rejected**: re-introduces exactly the host-only IP leak we're fixing. Add to the carve-out list instead. `TestIsVirtualSwitchInterfaceMatchesKnownNames` (32 entries) + `TestIsVirtualSwitchInterfaceLeavesPhysicalAlone` (16 entries) pin the contract.
- **Tailscale CLI is cached on the `/v1/health` hot-path** (PR #95). `Endpoints()` is called per-request; uncached `GetTailscaleDNSName()` spawns a `tailscale status --json` subprocess up to 1.5 s long per probe — under concurrent load this fans out into N parallel processes and stalls the health endpoint. `cachedTailscaleStatus` wraps the CLI with a 30 s TTL cache + singleflight gate so one CLI call covers all concurrent + TTL-window callers. **Errors are NOT cached** — a transient failure (Tailscale not yet up) retries on the very next call rather than locking out for 30 s. Tests bypass the cache by reassigning `tailscaleStatusJSONFunc` directly + `resetTailscaleStatusCache()` in cleanup. `TestCachedTailscaleStatus_DedupesConcurrentCallers` pins the singleflight contract; `TestCachedTailscaleStatus_TTLHitReturnsCached` and `TestCachedTailscaleStatus_ErrorsAreNotCached` pin the TTL + error-pass-through. Don't introduce a different invocation path that bypasses `cachedTailscaleStatus` without re-establishing the singleflight contract for that path.
- **Tailscale CLI invocation always uses `--peers=false`** (PR #95). The default `tailscale status --json` payload includes every peer node + its routes; we only read `Self.DNSName` and `Self.TailscaleIPs`, both still present without peers. Available since Tailscale CLI 1.32 (2022); earlier installs are unsupported upstream.
- **`exec.LookPath("tailscale")` result MUST pass `filepath.IsAbs(p)`** (PR #95). Defense-in-depth — production service-manager configs never put `.` or user-writable dirs on PATH, but a misconfigured deployment could otherwise run an attacker-staged binary with bridge privileges. PATH-relative results fall through to the platform-specific absolute fallback paths (`/Applications/Tailscale.app/...` on macOS, `<ProgramFiles>\Tailscale\tailscale.exe` on Windows).
- **`config.CustomEndpoints` is prune-and-warn validated** (PR #96). `ValidateCustomEndpoints(in)` returns `(kept, warnings)` — non-HTTPS, malformed, or empty entries are silently dropped from `kept` rather than failing the whole config load. `Validate()` rewrites the slice in-place AND logs each warning at `.warn` via the `config` component logger so the operator sees the breadcrumb in the bridge logs (Qodo on PR #92 round 1 — without observability, a typo just disappeared). The single-page settings textarea (one URL per line) and the JSON-array form (`customEndpoints`) and the textarea form (`customEndpointsText`) are ALL accepted by the PATCH handler; the array form wins when both are sent. `apiSettingsGet` exposes both fields so programmatic clients can round-trip the config.
- **Runtime config now uses copy-on-write + `atomic.Pointer[*Config]` holder** (PR #96 follow-up). `internal/config.RuntimeConfig` is the single live source of truth: readers call `Load()` per-request; writers (`/api/settings`, roots add/remove) clone→mutate→validate→save→run hot-apply hooks→`Store()` atomically. **Never mutate `Load()` result in place.** This removed the cross-server config race between admin writes and API `/v1/health` reads.
- **Things that have bitten before: in-place config mutation leaks across goroutines.** Even under `admin.Server.mu`, mutating a shared `*config.Config` lets concurrent readers race. Pattern to keep: clone snapshot (`config.Clone(holder.Load())`), apply changes on clone, validate + persist, perform side-effect hooks, then atomically publish via holder.
- **TLS cert SAN list covers the full advertised endpoint set** (PR #97). `tls.GenerateOptions{Hostname, ExtraDNSNames, ExtraIPs}` is the SAN-aware path; `tls.GenerateWithOptions` and `tls.LoadOrGenerateWithOptions` are the new entry points. Legacy 3-arg `Generate` / `LoadOrGenerate` shims preserved for callers that don't need the broader SAN set. New helpers `advertise.GatherCertSANIPs(cfg)` / `advertise.GatherCertSANDNS(cfg)` walk the same iface filter as `Endpoints()`, plus the Tailscale CLI (Self.TailscaleIPs + Self.DNSName), plus the parsed hosts of `cfg.CustomEndpoints`. **`<shortHostname>.local` is auto-appended** to the SAN DNSNames so the advertised mDNS URL `https://<short>.local:port` passes TLS hostname verification — without this, iOS clients dialing the mDNS URL fail at the platform check before pinning runs (Qodo on PR #93 round 1). `dnsNames(hostname)` skips the duplicate when the hostname is already `.local`-suffixed (macOS hostnames typically are).
- **Cert upgrade is warn-only, never auto-rotate** (PR #97). `LoadOrGenerateWithOptions` detects when the on-disk cert's SANs don't cover the currently-advertised endpoints and logs at `.notice` suggesting `bridge cert rotate`. **Do NOT add an auto-rotate path** — iOS pins the cert SHA-256 fingerprint at pairing time, and a rotated cert has a new fingerprint (different NotBefore / NotAfter / serial). Auto-rotation would silently break every paired iOS device on upgrade. Existing pairings keep working over the URLs they were originally pinned against (LAN, loopback, os.Hostname); rotation becomes a deliberate operator action when they have an iOS device in hand to re-pair. New installs (no cert on disk) get the broader SAN set automatically because `LoadOrGenerateWithOptions` mints fresh.
- **Custom-endpoints settings save fires a JS confirm dialog** (PR #97) when the textarea diff is non-empty after normalisation. Diff normalises whitespace + delimiter (commas → newlines, trim, drop blanks) but **PRESERVES order** — `advertise.Endpoints()` keeps the operator's input order within `ClassCustom`, so reordering DOES change which custom URL iOS tries first. Sorting in the diff would suppress the dialog for reorder-only edits while still persisting the order change (Qodo on PR #93 round 1). The dialog explains the cert-SAN-coverage prerequisite + re-pair-every-device consequence before the PATCH actually fires; cancel aborts cleanly. The bare `restartRequired: true` toast that fires on every settings save isn't loud enough for a contract-breaking action — operators learn to dismiss it.
- **`tls.ParseHostFromURL(raw)` strips scheme + port + IPv6 brackets** before the value reaches `crypto/x509.CreateCertificate` (PR #97). `x509` rejects malformed SAN entries with an opaque `x509: cannot parse DNSName "https://foo:7788"`. The helper returns `(host, isIP)` so callers route IP literals into `ExtraIPs` and DNS names into `ExtraDNSNames`. The `advertise` package keeps a sibling private `parseHostFromURL` (same signature, same logic) so `internal/advertise` doesn't need to import `internal/tls` — Gemini suggested DRY-ing across packages but the cross-package import would invert the natural layering (advertise is more "lower-level" than tls). When extending either, keep the two implementations in sync via the test pin (`TestParseHostFromURL_StripsSchemeAndPort` in `internal/tls`).
---

## Multi-disc folder art — parent-dir fallback + version-stale diff-guard + ExtractorVersion 2 (PR #606, 2026-07-30)

Root cause of the "Puccini: Turandot" grey tile (production, bridge.ars.md): the standard multi-disc layout keeps ONE `cover.jpg` at the album root with tracks in `Disc 1/`+`Disc 2/`, and the own-directory folder-art lookup could never see it — with no embedded APIC and a `no_mb_match` enrichment miss (comma-joined artist), `artworkMBID` stayed empty forever (the size+mtime fast-skip never re-looks; `needsLocalArtworkRecovery` deliberately returns false for empty). Invariants:

- **Disc-subfolder parent fallback** (`extractLocalArtwork`): when the track's own dir has no candidate AND its basename matches `discFolderRe` (`disc|disk|cd|lp|bd|dvd` + 1–3 digits, optional separator suffix — the anchored digit rejects `Disco 2`/`Discovery`/`CD 1234`; `Vol N`/`Side A` deliberately excluded), climb EXACTLY ONE level via the SAME per-directory single-flight cache keyed on the parent path. Own-dir always wins. `ExtractContext.LibraryRootDirs` (nil-safe, per-scan **absolute** cleaned roots — a relative root's Clean form never matches the walk's absolute dirs, silently disarming the guard) stops a root named like a disc folder from reading outside the library. **Don't widen the climb past one level or drop the disc-name gate** (an unconditional walk-up attributes `Artist/cover.jpg` to every album). Shallow `Artist/Disc 1/track` inheriting an artist-level cover is accepted + documented.
- **Version-stale diff-guard** (`reExtractUnchanged` + `Store.StampExtractorVersionBatch`): the upserts bump `indexed_at`, zero `enriched_at`, and replace `tags_json` wholesale on EVERY conflicting row — so a naive `ExtractorVersion` bump meant every paired iOS client re-pulls the ENTIRE library (twice: re-extract + re-enrichment waves), enricher fields transiently vanish (grey tiles), and premium `artwork_version` churns. Size+mtime-unchanged version-stale rows now re-extract → merge the post-scan-owned fields (**derived from the actual `tags_json` writers** — MarkEnriched + the four reconciliation passes: `ArtworkMBID`/`ArtistMBID`/`MusicBrainzAlbumID`/`MusicBrainzTrackID`/`Album`/`AlbumArtist`/`Year`/`TrackNumber`; fresh-non-zero wins so a new `local-` cover overrides a stored CAA UUID) → compare via `marshalForStorage`. **`MusicBrainzTrackID` was omitted until 2026-08-06 on the belief that it is extractor-owned — it is not.** The enricher's acoustic fallback writes it (`internal/enrich/acoustic.go` `applyAcousticFallback`: `t.MusicBrainzTrackID = m.RecordingMBID`, committed through the same `MarkEnriched`), so every fingerprint-recovered row differed from its stored twin, took the full-upsert leg, and lost the recording MBID + its `enriched_at` stamp + a spurious `indexed_at` bump — staged to fire library-wide on the `ExtractorVersion` 2→3 bump. Locked by `TestReExtractUnchanged_PreservesFingerprintRecordingMBID`. **The lesson generalises: derive the merge set from `grep` over the actual writers, not from what a field "looks like".** Equal → the non-wire `versionStampOnly` marker routes through `StampExtractorVersionBatch` (`extractor_version` + `missing_count` ONLY — deliberately NOT an `enriched_at` writer). Different → normal upsert (the `indexed_at` bump is what lets iOS pull the improvement). Extract errors skip write+stamp (retry next scan — never clobber a good row with a partial extract); lookup failures fail open to the full upsert. **Don't pad the merge set with fields no post-scan writer touches** (masks legitimate extractor changes); a MISSED future post-scan field is a bounded self-healing loss window (the full-upsert leg zeroes `enriched_at` → re-fill), not silence — still add it. **Don't route the stamp leg through anything that bumps `indexed_at`.**
- **`ExtractorVersion = 2`** is the designed one-shot heal: the first scan after deploy re-extracts every stale row once and multi-disc albums gain their album-root cover; the guard keeps the client delta bounded to rows that actually changed. Negative-control-verified: disabling the stamp decision turns exactly the two no-delta tests red (`scanner_disc_art_heal_test.go`). Also fixed the stale `BuildManifest` docstring — `since` filters by `indexed_at`, NOT mtime.
---

## v0.1.3 — local artwork extraction (PR #98)

The bridge now prefers **user-curated artwork** (embedded ID3 APIC + folder.jpg / cover.jpg) over the post-scan MusicBrainz / Cover Art Archive / iTunes path. Targets the obscure-album case: MB has no record, CAA 404s, iTunes rate-limits — but the user already curated `cover.jpg` next to the audio file. Wire-protocol-compatible (additive sentinel hijack, `ProtocolVersion` stays at 1, no iOS rebuild needed).

- **`Track.ArtworkMBID` carries two value shapes**: a MusicBrainz UUID (set by the enricher post-CAA fetch, original meaning) OR a `local-<sha256>` sentinel (set by the scanner during Extract from embedded APIC bytes or a folder-level `cover.jpg` / `folder.jpg`). The /v1/artwork/{mbid} regex is relaxed to `^([0-9a-fA-F]{8}-...|local-[0-9a-f]{64})$` — the local- branch is **lowercase-only** (matches `hex.EncodeToString` deterministically). Path traversal stays impossible (regex alphabet `[a-z0-9-]`). Artist-image endpoint keeps strict UUID validation (no local- shape there per V1 scope).
- **JPEG-only by design.** The cache file path is `<artDir>/local-<hash>-500.jpg` and the API serves `Content-Type: image/jpeg`. PR #98's first draft accepted PNG candidates (`cover.png` / `folder.png`) and `image/png` APIC frames — that would write PNG bytes into a `.jpg` path served as image/jpeg, breaking spec-strict clients. Restricted to JPEG-only with **two-layer defense**: MIME check (`image/jpeg` or `image/jpg`) AND magic-byte sniff (`FF D8 FF` SOI marker). Catches both misdeclared MIME on tag forgery and misnamed PNGs inside `cover.jpg`. PNG support would need path-scheme + Content-Type changes done together; that's a follow-up, not V1 scope.
- **Folder-art lookup is single-flighted per directory** via `*folderArtPromise` (`sync.Once` + `sync.Map.LoadOrStore`). A 15-track album processed by `runtime.NumCPU()` parallel workers does ReadDir + read + hash + write **exactly once** instead of 15 times. **Don't reintroduce a "compute then `LoadOrStore`" pattern** — that produces N concurrent ReadDir + hash calls per album under contention. Single-flight cache is reset per `Scan()` / `ScanSubtree()` call (cross-scan persistence would create stale "no folder.jpg" hits when a user adds cover.jpg between scans).
- **Cache-wipe recovery in `Scanner.runScanWorker`** via `needsLocalArtworkRecovery(t *Track) bool`. Pre-fix the unchanged-track fast-path skipped Extract entirely based on audio-file size + mtime; wiping `<dataDir>/artwork/` after a scan left tracks with `local-<hash>` references to files that never came back, and the API served 202 + Retry-After indefinitely (the enricher won't refetch a local- value). The recovery helper does one `os.Stat` per `local-`-prefixed track per scan and forces re-extraction when the cache file is missing. **Gates on `os.IsNotExist`, NOT any `os.Stat` error** — a NAS hiccup, permission flap, or antivirus lock would otherwise force unnecessary re-extraction (audio reopen + tag reparse) on every flaky scan. UUID-bearing rows AND empty-MBID rows skip the check entirely (zero overhead for non-local libraries).
- **Enricher bypass has TWO load-bearing changes** in `enrichOne`. (a) The `albumMBID == ""` early-return is relaxed: `if albumMBID == "" && !strings.HasPrefix(t.ArtworkMBID, "local-")`. Without this, the obscure-album case (no MB match, but `local-` ArtworkMBID stamped by the scanner) would `markSkipped` and never call `resolveArtist` — silently breaking the very case the feature targets. (b) `ensureArtworkCached` is wrapped in a `local-` prefix guard so neither CAA nor iTunes is called when the scanner already curated the bytes. **Don't combine the two checks into one** — the bailout fix protects `resolveArtist`; the wrap protects the artwork fetch. They serve different code paths.
- **Embedded APIC is NOT extracted in `populateFromTagMetadata`** — that function stays pure (no I/O, no cacheDir). The hook lives in `extractLocalArtwork(absPath, t, m, ec)`, called from BOTH `extractViaDhowdenWithContext` (after `populateFromTagMetadata`) AND `extractDSFWithContext` (after its own `populateFromTagMetadata`). DSF's ID3v2 chunk gets embedded-APIC support via the same code path — don't fork the logic.
- **25 MiB cap (`maxArtworkBytes`) on per-image bytes** (raised from 10 MiB on PR #100). Modern audiophile rips routinely embed 10–20 MiB digital-booklet scans; the original 10 MiB cap silently rejected near-boundary cases (e.g., a 10.04 MiB cover lost the entire album's `ArtworkMBID` to MusicBrainz fallback). Worker-RAM math under the parallel-worker model: `runtime.NumCPU()` workers (8–16 cores) × 25 MiB ≈ 200–400 MiB peak — comfortable on any machine running the bridge (PC/Mac, not iOS) while still rejecting genuine misuse (lossless TIFFs in tags, misnamed 4K wallpapers as cover.jpg). Overrun is logged + skipped; the track stays indexed without an ArtworkMBID and the enricher's MusicBrainz path remains as fallback.
- **Stat-before-write makes `stampLocalArtwork` idempotent across re-scans.** First call writes `<artDir>/local-<hash>-500.jpg`; subsequent calls with the same bytes hit `os.Stat` and return success without an overwrite. Two tracks with byte-identical embedded artwork share one cache file (desired SHA-256 dedup; not a bug — comment in `extractLocalArtwork`).
- **Cache directory perms hardened to `0o700`** (owner-only) in both writers (`writeArtworkAtomic` in enrich + `writeArtworkAtomicScan` in manifest). Application-owned caches shouldn't be world-readable on POSIX. Whichever writer touches the dir first creates it at the new mode; existing 0o755 dirs from prior deployments stay at their previous mode until a clean install (mode not retro-applied).
- **`writeArtworkAtomicScan` uses `.scan-*.jpg.tmp`** as the temp-file prefix (vs the enricher's `.caa-*.jpg.tmp`). A stale temp tells you scanner-side (not enricher-side) was the writer. Duplication of the helper across the two packages is deliberate — extracting to a shared package would add a third internal/* import path for a 30-line helper called from only two sites. Keep the duplication; mirror future changes across both copies.
- **Test-only test counters use `atomic.Int32`** (not bare `int`) for `caaCalls` / `itunesCalls` in the new enricher tests. HTTP handler goroutines write; main test goroutine reads. Without atomic, `go test -race` flags the read of int with goroutine-write handler still in scope. Pre-existing `TestEnricherSkipsNetworkCallIfCoverAlreadyCached` has the same int-counter pattern and is grandfathered — convert if a future bot run flags it.
- **18 `NewScanner` call sites** across `cmd/bridge/main.go` (2) + manifest_test.go (12) + watcher_test.go (2) + scanner_walk_error_test.go (2) + admin_test.go (1) + extractors_test.go (1, new). The signature is `NewScanner(roots []string, store *Store, artworkCacheDir string)`; tests pass `t.TempDir()` or `""`. Production main.go passes the same `<cfg.DataDir>/artwork` string to BOTH `NewScanner` AND `enrich.NewEnricher` — single source of truth.
---

## v0.1.4 — perf + hardening (PR #99)

Four independent quick wins from a code-review pass; cohesive ~180-line PR. None of these touched local-artwork code from PR #98.

- **`dynamicHandler.cache` keys on `*slog.Logger`, NOT `slog.Handler`** ([logging.go](internal/logging/logging.go)). Go `==` on interface values panics if the concrete type is non-comparable (struct containing slice / map / func). Standard handlers (`*slog.TextHandler`, `*slog.JSONHandler`) are pointers so they're safe today, but `dynamicHandler` is a general-purpose shim and must accept any handler an operator installs via `slog.SetDefault`. `*slog.Logger` is always a pointer and always comparable; `slog.Default()` swaps the pointer on `SetDefault`, so cache invalidation still fires correctly. Caught by Gemini + CodeRabbit on PR #99 round 1. **Don't reintroduce a `slog.Handler` cache key** — even though the standard handlers happen to be comparable, the panic risk is real for future user-supplied handlers.
- **`dynamicHandler.Handle` cache contract** ([logging.go:142](internal/logging/logging.go)). Pre-fix every log line replayed `WithGroup` / `WithAttrs` and deep-cloned the handler tree (1-2 throwaway clones per line for a `Component(name)` logger). Cache hits on the steady-state path; `slog.SetDefault` implicitly invalidates by swapping the `*slog.Logger` pointer. Concurrent rebuilds race-store `atomic.Pointer[cachedResolution]`; both produce identical chains (pure function of `h.groups + h.attrs + logger`), so the loser's wasted work is one extra clone. **`Enabled()` stays uncached** — `slog.Handler.Enabled` is short-circuit cheap on the level filter and doesn't deep-clone; caching it would burn complexity for no gain. `TestDynamicHandlerCachesResolvedChain` + `TestDynamicHandlerCacheInvalidatesOnSetDefault` pin the contract under `-race`.
- **Atomic-write helpers carry `defer func() { _ = tmp.Close() }()`** registered AFTER the existing `Remove` defer ([auth.go:236](internal/auth/auth.go), [config.go:556](internal/config/config.go), [state.go:129](internal/updater/state.go), [enricher.go:631](internal/enrich/enricher.go), [extractors.go:565](internal/manifest/extractors.go), [backup.go:336](internal/backup/backup.go) — 6 sites). Pre-fix a panic between `CreateTemp` and the explicit Close (e.g. inside an `http.Handler` that net/http will recover from) leaked the file descriptor. **LIFO ordering is load-bearing**: defer registered LAST runs FIRST — Close runs before Remove because Windows holds an open file from being unlinked. Don't reorder the defer registration: Remove first (registered first → runs last), then Close (registered second → runs first). The explicit `tmp.Close()` calls in error paths still run normally; the deferred Close on the success path returns `fs.ErrClosed` and is ignored via `_ =`.
- **`WriteManifest` streams via `json.NewEncoder(bw).Encode(t)`**, not `json.Marshal(t) + bw.Write(b)` ([scanner.go:880](internal/manifest/scanner.go)). For a 50k-track library that's 50k `[]byte` allocations eliminated. The encoder appends `\n` after each value; **JSON spec treats `\n` as ignorable whitespace inside an array**, so `{...}\n,{...}\n,{...}\n` stays valid for any spec-compliant parser (api tests round-trip clean). The trailing `]` lands after the last track's `\n` — also valid whitespace. **Don't switch back to `Marshal + Write`** — the allocation profile under load was the GC bottleneck for `/v1/manifest` on large libraries.
- **Admin JSON decoders wrap `r.Body` in `http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)`** (1 MiB cap, [admin.go](internal/admin/admin.go)). 5 sites in handlers_api.go + handlers_backups.go + handlers_token_lifecycle.go. **`http.MaxBytesReader` over `io.LimitReader`** because the former sets `Connection: close` on overflow and returns a proper error type that the JSON decoder treats correctly. Admin binds loopback by default so the practical threat surface is thin, but defense-in-depth against a misbehaving local tool sending a multi-GB payload is cheap. The shared `decodeOptionalJSONBody` helper in `handlers_token_lifecycle.go` already routes through the same wrap; **prefer the helper over inline `r.ContentLength > 0` gates** — chunked uploads arrive with `ContentLength == -1` and the gate would silently drop the body (CodeRabbit caught `handlers_backups.go` regressing this on PR #99 round 1).
- **Pre-existing CodeQL `go/path-injection` alerts dismissed as false-positive** (alerts #1-4, dismissed 2026-04-29). All four route through `internal/fs.Resolver.Resolve`, which: rejects NUL bytes, walks raw segments and rejects any `..` BEFORE canonicalization, applies `path.Clean`, joins via `filepath.Join` against the trusted root, and final-prefix-checks the absolute path against the absolute root. CodeQL's flow analysis tracks `clientPath → abs → os.Stat` as raw taint and can't model the validation barrier. Alert #1 (`handlers_api.go:250`) is the admin "add library root" endpoint — loopback-only, the principal IS the operator, admin access is equivalent to shell access. Don't re-open these alerts without first checking the dismiss comments on each — the rationale is stable and the validation barriers haven't moved.
---

## v0.1.5 — Windows artwork robustness (PR #100)

Two scan-time failures from a fresh Windows scan, fixed in one bridge-only PR.

- **Artwork-write rename uses `renameWithRetry` (5 attempts, 0/50/100/200/400 ms backoff, total ≤ 750 ms)** ([extractors.go](internal/manifest/extractors.go), [enricher.go](internal/enrich/enricher.go) — duplicated, deliberate). Bare `os.Rename` on Windows trips intermittent "Access is denied" because (a) Defender / Search Indexer scan-on-close briefly hold a file handle on freshly-written tmp + destination files, and (b) concurrent scanner workers writing the same content-hashed destination race even though Go's `os.Rename` passes `MOVEFILE_REPLACE_EXISTING`. Backoff schedule absorbs the AV-window-class flake; for non-transient errors (parent-dir permission), the loop burns ~750 ms before failing — acceptable for a per-album-once code path. **On Unix the first attempt always succeeds, so the loop is a no-op** — single code path, no `_windows.go` build tag.
- **Post-rename-failure stat-and-accept via `bytes.Equal`, NOT size match**. When `renameWithRetry` exhausts its budget but a concurrent writer / prior pass has already published a byte-equivalent destination, return success. Read the existing destination + `bytes.Equal` against the buffer we tried to write — size match alone isn't proof; the enricher's filename is `<mbid>-<size>.jpg` (no content hash), and a future MusicBrainz cover-swap with same mbid + same size + different bytes would silently match (CodeRabbit on PR #100). Cost: one read of ≤ `maxArtworkBytes` on a rare-fallback path. Symmetric in both packages so future readers don't have to reason about per-caller filename invariants.
- **`tmpName = ""` ONLY on successful rename — never in the stat-and-accept fallback**. The defer at the top of both writers (`if tmpName != "" { _ = os.Remove(tmpName) }`) cleans up the tmp file on any non-success path. Pre-fix the fallback branch had `tmpName = ""` too, suppressing the deferred Remove even though the rename failed and the tmp was still on disk. Unbounded leak of `.scan-NNN.jpg.tmp` / `.caa-NNN.jpg.tmp` per race/AV-window hit (Gemini on PR #100). `TestWriteArtworkAtomicScan_RaceWinnerCleansTmp` + `TestWriteArtworkAtomic_RaceWinnerCleansTmp` pin the cleanup contract; `TestWriteArtworkAtomic*_RaceLoserDoesNotAcceptOnSizeCollision` (negative case: same size, different bytes) pins the byte-equivalence contract.
- **`renameFunc` test seam** ([extractors.go](internal/manifest/extractors.go), [enricher.go](internal/enrich/enricher.go)). Package-private `var renameFunc = os.Rename`; `renameWithRetry` calls `renameFunc(src, dst)`. Tests inject a deterministic failure to exercise the fallback path without burning the full 750 ms backoff budget. **Production code MUST NOT mutate this**; only test packages override + restore via `t.Cleanup`. Same convention as the `friendlyErrorMessage` / `_seedQueueForTest` test affordances on the iOS side.
- **`maxArtworkBytes` raised 10 MiB → 25 MiB** (already updated inline at line 177 in the same PR, recapped here for the v0.1.5 rollup). Modern audiophile rips routinely embed 10–20 MiB digital-booklet scans; the original 10 MiB cap silently rejected near-boundary cases (e.g., a 10.04 MiB cover lost the entire album's `ArtworkMBID` to MusicBrainz fallback). Worker-RAM math: `runtime.NumCPU()` × 25 MiB ≈ 200–400 MiB peak — comfortable on any machine running the bridge while still rejecting genuine misuse (lossless TIFFs, misnamed 4K wallpapers).
---

## v0.1.7 — Tailscale HTTPS auto-pilot (PR #102)

Bridge now mints + serves a real Let's Encrypt cert on Tailscale magic-DNS connections, while keeping the existing self-signed cert for LAN/mDNS/IP-literal connections (and the existing iOS pin contract on those paths). Eliminates the magic-DNS pairing dead-end that surfaced from a real user report — `.ts.net` is publicly-routable DNS, so `NSAllowsLocalNetworking` doesn't exempt it from ATS, so iOS rejected the self-signed cert before fingerprint pinning could override.

- **SNI cert switcher in [internal/tls/manager.go](internal/tls/manager.go).** `Manager.Get` is the `tls.Config.GetCertificate` callback: SNI under the local node's MagicDNS suffix → LE cert (loaded async); everything else → self-signed. **`atomic.Pointer[tls.Certificate]` for the LE cert** so the renewer can swap a fresh cert mid-flight without locking the handshake hot path. `magicDNSSuffix` is `atomic.Value` (string-typed). Empty suffix or nil LE cert → fall through to self-signed for that SNI (= pre-PR behaviour, ATS rejects, endpoint silently dropped from iOS selector pool).
- **`tls.LoadX509KeyPair` does NOT populate `cert.Leaf`** (the `tls.Certificate.Leaf` field is `nil` after Load) — reading `cert.Leaf.NotAfter` directly is a nil-pointer panic. **`CertNotAfter(cert)` and `LoadTailscaleCertFromDisk(certPath, keyPath)` in [manager.go](internal/tls/manager.go) are the only approved expiry-read paths**; both `x509.ParseCertificate(cert.Certificate[0])` explicitly. Three call sites (renewer freshness check, on-disk-cert age inspection at startup, admin-tile rendering) all route through these helpers. Don't reintroduce a bare `cert.Leaf.NotAfter` read.
- **Auto-mint goroutine + 24h renewer in [cmd/bridge/tailscale.go](cmd/bridge/tailscale.go).** At startup the auto-pilot runs `tailscale.Detect()` (parses `tailscale status --json` for `Self.DNSName` + `MagicDNSSuffix`) then `tailscale cert --cert-file=<path>.crt --key-file=<path>.key <magicdns>` if the on-disk cert is missing or within `tailscale.FreshnessThreshold = 14 * 24 * time.Hour` of expiry. Renewer ticks every 24h. Both run on `scanCtx` so SIGINT clears them with the rest of the periodic workers. **All errors are non-fatal**: missing CLI, missing MagicDNS, mint failure → admin tile shows the failure reason, bridge runs identically to today.
- **Fixed-filename LE cert path: `<dataDir>/tls/tailscale.crt` and `tailscale.key`** ([tailscale.go:LECertPaths](internal/tailscale/tailscale.go)). NOT magic-DNS-keyed — a tailnet/host rename or reinstallation would otherwise leave orphan files in dataDir. Matches the `server.crt` / `server.key` convention. Don't switch to a `<magicdns>.crt` filename without a migration step that cleans up the old name.
- **Tailscale CLI binary resolution: `exec.LookPath("tailscale")` first, then `/Applications/Tailscale.app/Contents/MacOS/Tailscale` fallback for the Mac App Store install** ([tailscale.go:resolveBinary](internal/tailscale/tailscale.go)). The MAS sandbox doesn't put the CLI on `$PATH`, so a `LookPath` miss isn't the same as "not installed" on macOS. Resolved path is stored on `NodeInfo.BinaryPath` and reused by `MintCert` so subsequent calls don't re-resolve.
- **`MintCert` typed errors** ([tailscale.go:classifyMintError](internal/tailscale/tailscale.go)): `ErrHTTPSCertsDisabled` (tailnet hasn't toggled HTTPS in admin/dns), `ErrPermission` (Linux: user not in `tailscale` group). Mapped from stderr substrings; admin tile renders distinct guidance per typed error. Unmatched failures fall through to a wrapped generic error so the verbatim stderr fragment still surfaces in `LastError`. **Pattern-matches are case-insensitive substrings** so a Tailscale CLI rewording doesn't silently demote a known failure to "generic" — locked by `TestClassifyMintError_*` in [tailscale_test.go](internal/tailscale/tailscale_test.go).
- **`commandContext = exec.CommandContext` test seam** ([tailscale.go](internal/tailscale/tailscale.go)). Tests inject a fake to drive specific stdout/stderr/exit-code shapes without spawning real `tailscale` processes. Production code MUST NOT mutate. Same convention as `renameFunc` in [internal/manifest/extractors.go](internal/manifest/extractors.go).
- **Admin Tailscale tile** ([handlers_tailscale.go](internal/admin/handlers_tailscale.go), [templates/dashboard.html](internal/admin/templates/dashboard.html), [static/app.js:refreshTailscale](internal/admin/static/app.js)). `GET /api/tailscale/status` returns the latest auto-pilot snapshot; `POST /api/tailscale/refresh-cert` triggers a re-mint. **Tile is hidden when `CLIAvailable=false`** — operators on hosts without Tailscale don't see clutter. JS poll is 30s (lower than dashboard's 3s) because Tailscale state is slow-moving. **Re-mint button uses disabled+`"Minting…"` UX, no confirm dialog** — combined with the auto-pilot's server-side 30s rate limit (`minMintInterval` in [cmd/bridge/tailscale.go](cmd/bridge/tailscale.go)) this absorbs panic-clicks before they reach Let's Encrypt's per-domain quotas. **Cert path surfaced as `title=` tooltip on the expiry line**, not as a visible UI element — power-user debugging without UI clutter.
- **Admin uses an interface-based adapter (`TailscaleProvider`)** so [internal/admin](internal/admin) doesn't import [internal/tailscale](internal/tailscale) or `cmd/bridge`. Adapter `tailscaleAdminAdapter` lives in [cmd/bridge/tailscale.go](cmd/bridge/tailscale.go) and translates between the local `tailscaleStatus` and admin's `TailscaleStatus`. Same pattern as `UpdateProvider`.
- **iOS 1.1 compatibility**: existing paired-via-LAN devices see ZERO behaviour change — they connect via LAN, get the self-signed cert, fingerprint matches captured pin, works as before. Magic-DNS endpoint stays unreachable for them (different failure reason than pre-PR — pinning rejects the LE fingerprint instead of ATS rejecting the self-signed chain — but same observable outcome: endpoint silently drops from selector pool). **Edge case**: a fresh iOS 1.1 device that explicitly types the magic-DNS URL into the pair flow would now succeed (capturing the LE pin) but subsequent LAN connections would fail (LAN serves self-signed, fingerprint mismatch). Recovery is one re-pair via LAN URL. iOS 1.2 (per-host pinning policy) removes the foot-gun entirely.
---

## v0.1.8 — admin-approval pairing flow (PRs #103 + #105)

`internal/pairing` is the in-memory state machine that backs the new tap-to-pair UX. iOS now POSTs to `/v1/pairing/requests`, the request shows up as a card on the admin Devices page, and the operator clicks Approve/Decline. ProtocolVersion stays at 1 — this is purely additive on top of the existing v1 wire shape.

- **Read-many delivery contract**: `Poll` returns `RawToken` on EVERY authorized poll while state is Approved, NOT read-once. iOS retries on a network blip (Wi-Fi flap dropping the 200 OK with the token) must be recoverable; the row is consumed only when iOS sends `DELETE` (acknowledgment) OR when TTL+grace elapses without ack. Pre-fix the qodo-flagged race could revoke the freshly-minted token before iOS got it. **Don't reintroduce a "clear RawToken on first read" or a `StateDelivered` terminal — the pollSecret bearer + cert pin are what gate re-reads, and that's the safety surface.** ([store.go](internal/pairing/store.go) Poll path; locked by `TestApproveProducesTokenOnEveryAuthorizedPoll` + `TestPairingApprovedTokenDeliveryReadMany` end-to-end via the wire.)
- **Per-request timer-generation guard**. `time.Timer.Stop()` returns false when the callback is already queued past the recall window, so a stale Pending-phase callback can fire AFTER `Approve` has rescheduled the timer. Without the `timerGen uint64` counter on each Request (incremented by every `scheduleTimer`, captured by value in the AfterFunc closure, compared in `onTimer`), the stale callback would see `State == StateApproved` and treat the post-grace deadline as reached → revoke + delete the freshly-minted token. **Don't rely on `Stop()`'s return value — check the captured generation against `req.timerGen` at the top of `onTimer` and no-op on mismatch.** Locked by `TestStaleTimerCallbackIsNoOp`.
- **Wall-clock TTL guard inside Approve / Decline**. The Pending sweeper is best-effort — `Stop()` returning false on a fired-but-unprocessed timer means `onTimer` hasn't taken `s.mu` yet, OR the runtime hasn't dispatched it under load. Without the wall-clock check, a late admin tap (after `CreatedAt+TTL` but before `onTimer` takes the lock) would mint a token for a request the user has likely abandoned. **Both Approve and Decline transition the row to Expired themselves and surface `ErrAlreadyDecided` if elapsed >= TTL** — the timer is cleanup, not the authoritative deadline. Locked by `TestApproveRefusesPastWallClockTTL` / `TestDeclineRefusesPastWallClockTTL` (use injected clock).
- **Cert-rotation guard fails closed**. The bridge fingerprint captured at `CreateRequest` time is compared at admin Approve time; ANY divergence — different value AND empty value — flips the state to `CertRotated` and refuses the mint. The original carve-out `currentFingerprint != ""` (skip-on-empty) silently bypassed the pin check on a caller regression / fingerprint-lookup failure. **Once `req.CertFingerprint` was captured, require an exact match: `if req.CertFingerprint != "" && req.CertFingerprint != currentFingerprint`** (PR #103 third-pass review). Test-mode escape hatch: leave `req.CertFingerprint == ""` to opt out of the guard explicitly. Locked by `TestApproveCertRotationGuardFailsClosedOnEmptyCurrent`.
- **Revoke-then-delete with bounded retry**. `onTimer`'s Approved-undelivered branch must NOT delete the row before `auth.Store.Revoke` succeeds — a revoke failure (disk full, permission, transient I/O) on a deleted row orphans a still-valid token in `tokens.json` with no Store-side handle to retry. Pattern: capture intent under the lock, drop the lock for the revoke call, re-acquire to finalize. Success deletes the row; failure increments `req.revokeAttempts` and reschedules the expiry timer with exponential backoff (`revokeRetryBackoff`: 1s → 10s → 60s). After `maxRevokeAttempts = 3` the row is dropped + logged so the operator can recover via `bridge token revoke <id>`. **Bounded** so a permanently-failing `auth.Store` can't pin the row for the process lifetime. Locked by `TestRevokeRetryOnFailure` + `TestRevokeRetryGivesUpAfterMaxAttempts`.
- **`snapshot()` redacts secret material**. `Request.RawToken` (live bearer) and `Request.PollHash` (auth-binding hash) MUST be zeroed in the copy path used by `CreateRequest` / `Approve` / `Decline` / `List` — otherwise they flow through admin-side templates / JSON / debug logs during the grace window. The live `Poll` path reads from `*Request` directly (not via `snapshot`), so the redaction doesn't affect the read-many delivery contract. **Don't return a bare `*req` copy from any new admin-facing path; route through `snapshot`.** Locked by `TestSnapshotRedactsSecretMaterial`.
- **No per-IP rate cap on `/v1/pairing/requests`**. Under double-NAT / mesh routers every LAN device presents the same router IP, so per-IP throttling produces false positives that block legitimate join attempts. The 16-pending-bridge-wide cap (`DefaultMaxPending`) is the single bound; the admin's visible queue is the remediation surface. **Don't reintroduce a per-source-IP token bucket** — bot reviews flagged it as "defense in depth"; CLAUDE.md call: false-positive surface > marginal spam protection.
- **6-digit verification code from `crypto/rand`**, formatted with `fmt.Sprintf("%06d", n)` so leading zeros survive (`004123`, not `4123`). Stored as the formatted string on the request; never re-derived from `n`. Shown to the admin AND iOS so the operator can verify the device matches before approving — fingerprint suffix alone is unsuitable (admin can't compare 12 hex chars under social pressure but can compare 6 digits trivially). Drawn via `rand.Int(rand.Reader, big.NewInt(1_000_000))` — a uniform draw with no modulo bias (PR #465 replaced the earlier `binary.BigEndian.Uint32(buf) % 1_000_000` form; the bias was inconsequential against the threat model but the uniform draw is cleaner and silences the recurring static-analysis flag). Don't switch to `math/rand` even for tests — a non-CSPRNG would be a real downgrade.
- **`pollSecretHash` storage and comparison**. POST body field is hex-encoded SHA-256 (64 chars). Server decodes once at creation into `[32]byte` (`PollHash`); rejects malformed hex with 400. Poll's bearer header is the raw `pollSecret` (base64url-no-padding, 43 chars). Server `sha256` it, then compares the two `[32]byte` slices via `subtle.ConstantTimeCompare` — no string compare, no hex case sensitivity in the compare path. **The auth contract is anchored to the canonical encoded form** (the 43-char ASCII string), per `PROTOCOL.md` "pollSecret wire encoding". Don't hash the raw bytes pre-encoding — client and server would diverge.
- **`bridgeStartedAt`** is `time.Now().UnixMilli()` captured at process start, embedded in every pairing response. iOS observes a value change between calls and treats it as terminal "bridge restarted, please request again" — distinct from a network error which would keep retrying against a server that no longer knows the request ID.
- **Admin handler doesn't take `Server.mu`** in `apiPairingApprove` / `apiPairingDecline`. `pairing.Store` and `auth.Store` each have their own mutex; `Server.mu` is for serializing config-rewrite paths. Holding it through `Mint`'s disk persist would block unrelated admin operations under spam (gemini on PR #105). **Card lifecycle on the admin UI** (no manual refresh): existing 3 s Devices poller is the only mechanism. Approve/Decline optimistically flip the card to a transient "approved · awaiting device acknowledgment" / "declined" state via a JS-side latch (`pendingActionLatch`) so the next poll tick can't briefly flash "pending" while the server is still processing. Server-driven removal: card disappears when `/api/pairing` no longer lists it (iOS DELETEd, OR sweeper expired the row at TTL+grace).
---

## v0.1.6 — operator-UX batch (PR #101)

Three independent operator-UX fixes from the same review pass — common theme: small papercuts on startup + dashboard + pair surfaces that compound on a debug-restart loop. Cohesive ~470-line PR, two rounds of bot-review fixups folded in.

- **Backup-on-startup throttle** ([cmd/bridge/backup.go](cmd/bridge/backup.go)). `runBackupTicker` skips the startup snapshot if a snapshot exists within `startupBackupSkipThreshold = 24h`. The scheduled-tick + manual `bridge backup` paths still cover the "haven't booted in a week" case. Helper `startupSnapshotShouldSkip` is unit-tested across no-snapshots / recent / old / multiple-pick-newest / missing-dir-not-an-error / **exact-threshold (boundary case — strict `<` comparison, equality must NOT skip)**. Pre-fix an operator restarting 10×/day for config tweaks paid 10 snapshots/day in disk + log noise. Don't lower the threshold below the default scheduled interval — a startup landing inside a freshly-completed scheduled snapshot's window must still be a no-op.
- **Admin Pair page surfaces all alternates** ([handlers_api.go](internal/admin/handlers_api.go), [handlers_token_lifecycle.go](internal/admin/handlers_token_lifecycle.go), [pairing.go](internal/admin/pairing.go), [templates/devices.html](internal/admin/templates/devices.html), [static/app.js](internal/admin/static/app.js), [static/app.css](internal/admin/static/app.css)). `pairResult` JSON gains `alternates: []string` rendered as a "Other URLs the device will try" block in the modal, hidden when only the primary URL is present. Pre-fix the modal showed only the operator-supplied primary URL even though the QR's `bridge://pair?...&urls=...` payload bakes every alternate (LAN IPv4/IPv6, .local mDNS, Tailscale magicdns) — operators thought only one URL had been shared with the device. **`ensurePrimaryFirst(primary, alternates)` helper in [pairing.go](internal/admin/pairing.go)** is the single source of truth for the response-boundary contract: rebuilds the slice with primary at head AND dedups primary occurrences (CodeRabbit round 2 — the original early-return path let `[primary, other, primary]` slip through with the duplicate). Called by both mint and rotate sites before writing JSON. Test invariant: `pairResult.Alternates[0] == pairResult.URL` and primary appears exactly once. **When extending the pair flow, populate `Alternates` at every `pairResult` emission site AND route through `ensurePrimaryFirst`** — the helper duplication risk is real (mint + rotate already exist) and the contract matters for older iOS clients that read just `alternates[0]`.
- **`LastFullScan` persistence + dashboard live-refresh** ([scanner.go:122](internal/manifest/scanner.go), [templates/dashboard.html](internal/admin/templates/dashboard.html), [static/app.js](internal/admin/static/app.js)). Two converging bugs: (a) `s.lastFull` was an in-memory `atomic.Int64` only; the matching `SetScanState("last_full_scan", ...)` write at scan-completion was a dead-code orphan with no reader, so a process restart reset the dashboard to "never" until the next scan completed (which on a 50k-track library is a long minute or two). (b) The dashboard's 3 s auto-tick didn't repaint the "Last full scan" tile, so a scan finishing mid-session left a stale "never" until manual page refresh. Fix: `NewScanner` reads the SQLite `last_full_scan` key into the atomic at construction (defensive: read failures + parse failures fall through silently to the existing zero-time semantics, same UX as a fresh install; `OpenStore` already created the `scan_state` table before any caller can construct a Scanner against the store, so the read is safe at init time). The JS tick gets a third update path that pulls `/api/stats.lastFullScan` and renders via the existing `formatTimeAgo` helper. **DOM lookups (`last-full-scan`, `scan-status`) are cached outside the tick** (Gemini on PR #101) — first-paint-stable elements; re-querying every 3 s was wasted work. Tests pin the seed-on-restart and zero-on-fresh-DB invariants.
---

## v0.1.9 — admin SSE replaces dashboard polling (PR #107)

`GET /api/events` is a Server-Sent Events stream that multiplexes the five live dashboard surfaces (stats, endpoints, pairing, updates, tailscale) at three cadences (500 ms / 5 s / 30 s) with per-event JSON-diff suppression and a 15 s `: heartbeat` SSE comment. Replaces four `setInterval` polls in [internal/admin/static/app.js](internal/admin/static/app.js); each `render*` polling function split into an `applyX(parsedData)` helper that the SSE listener calls. Connection-status badge `#conn-status` in the layout reflects EventSource readyState. REST endpoints stay intact as thin wrappers over the same `getXxxSnapshot()` helpers — useful for curl, integration tests, debugging. No protocol bump; admin-only, iOS clients don't see this.

- **`BaseContext` on the admin `http.Server`** ([admin.go Serve](internal/admin/admin.go)). Wired to the parent ctx passed to `Serve`, so per-request contexts derive from it and auto-cancel on graceful shutdown. Without `BaseContext`, `http.Server.Shutdown` would block the full 5 s grace window waiting for the SSE connection to "idle" (it never does — SSE is one long request). Pre-existing per-request handlers (e.g. `apiUpdatesCheck`'s GitHub poll) inherit the same cancellation chain — net win, no regression. **`WriteTimeout` MUST stay unset (PR #75 invariant)** — a finite WriteTimeout would tear the SSE connection mid-stream every minute. `ReadTimeout` (30 s) and `IdleTimeout` (120 s) don't apply to SSE: GET has no body to time out reading, and SSE is one long request, not multiple between idle gaps.
- **`UptimeSec` zeroed in the SSE payload** via `getStatsSSESnapshot()` ([handlers_api.go](internal/admin/handlers_api.go)). The REST `getStatsSnapshot()` keeps `UptimeSec` for back-compat; the SSE wrapper zeroes it so the byte-wise JSON-diff cache is stable across ticks where nothing meaningful changed. Without this, `UptimeSec` ticks every second and would force a `stats` frame on every fast tick, defeating diff suppression. The dashboard never renders `UptimeSec` in its live tick (it's server-rendered from `StartedAt` at first paint via the Go template); zeroing on the wire is invisible. **Don't add other monotonically-changing fields to `statsResponse` without applying the same zero-in-SSE treatment** — anything that ticks every tick poisons the diff.
- **Fast-tick stats gated on `IsScanning() || wasScanning`** ([handlers_events.go](internal/admin/handlers_events.go)). Pre-fix the 500 ms ticker called `publishStats()` unconditionally, which ran `Manifest.CountTracks()` (`SELECT COUNT(*)`) every 500 ms per open client even when diff suppression skipped emitting a frame — N tabs × 2 Hz × COUNT(*) scaled badly on large libraries (Qodo on PR #107). The `wasScanning bool` latch fires one final stats publish on the tick after a scan completes (so the post-scan `TracksIndexed` + "idle" badge land sub-second), then idle dashboards skip the fast-tick stats work entirely. The medium ticker (5 s) keeps unconditional cadence so off-scan changes (DeviceCount after a token mint, DBBytes growth) still surface within 5 s. **Pairing stays unconditional on the fast tick** because `pairing.List()` is a cheap map iteration AND the `SecondsUntilExpiry` countdown depends on it during a join flow.
- **`SecondsUntilExpiry` intentionally streams pairing every ~1 s during a join**. `pendingPairingRow.SecondsUntilExpiry` is computed from `time.Now()` ([handlers_pairing.go](internal/admin/handlers_pairing.go)) and decrements every second while a request is pending — JSON-diff therefore won't suppress pairing frames during an in-flight join. **This is the desired behaviour, not a bug**: the server naturally streams the countdown to the browser, removing the need for a client-side ticker. Wire-cost is negligible (small JSON, bounded by the rare in-flight-request window). Once all pairing requests resolve, the empty-array snapshot stabilises and pairing frames stop. Don't rework `getPairingSnapshot()` to round / drop the field for diff stability — it's load-bearing for the UX.
- **Origin gate on `apiEvents`** (Qodo on PR #107). `csrfGuard` lets all GETs through unconditionally (correct for the body-bearing-mutation threat model it was designed for), but `/api/events` is long-lived and would otherwise be openable from any cross-origin tab on the same loopback. The SSE handler runs `originMatchesAdmin(Origin)` before writing headers — Origin-when-present must match `AdminAddress`; absent Origin allowed (curl, the admin UI itself in some browsers). `TestEventsRejectsCrossOriginGET` pins the contract. **Apply the same Origin gate to any future long-lived GET endpoint** — the csrfGuard exemption for GETs is sized for one-shot reads, not held connections.
- **Empty-list teardown branch in `applyPairing` is load-bearing** ([static/app.js](internal/admin/static/app.js)). Server restart wipes the in-memory pairing store; the initial SSE snapshot then sends `[]`; `applyPairing([])` MUST clear `pendingActionLatch`, hide `#pending-pairing-panel`, and empty `#pending-pairing-list`. Without this, stale optimistic-action latch entries from before the restart would inherit any new request that happened to reuse the same `id` slot. `getPairingSnapshot()` also returns `[]pendingPairingRow{}` (not `nil`) so the JSON marshals as `[]` not `null` — the JS-side `Array.isArray(entries) && entries.length === 0` teardown gate depends on the array shape.
- **`marshalAndPublish()` centralises diff-then-emit** with `logger.Error("sse marshal", ...)` on any `json.Marshal` failure (Gemini on PR #107). Pre-fix the silent `return nil` would mask a regression in any response struct shape. The `publish` helper splits payload bytes on `\n` and re-prefixes each line with `data: ` — `json.Marshal` (without indentation) never emits literal newlines today, but it's cheap defense in depth against any future caller using `MarshalIndent` or embedding raw bytes. **`publish`'s line-splitting + flush-after-write is the SSE protocol contract; don't bypass with raw `fmt.Fprintf` on `data:` payloads at any new SSE site.**
---

## v1.2 — offline PCM upscaling foundation + on-demand API (PRs #108 + #109)

Optional sox(1)-driven upscaling produces FLAC sidecars cached under `<dataDir>/transcoded/`. Disabled by default (`upscale.enabled: false`); operators opt in via bridge.yaml. Two driver paths: `bridge upscale` CLI for whole-library batches; `POST /v1/upscale` for iOS-driven per-track / per-folder requests. Both feed the same `transcode.Pool` primitive.

- **Mission preserved**: no on-the-fly transcoding. Conversion is offline; the bridge serves cached sidecars bit-exact via the same `serveFile` path as originals. The "no server-side transcoding" invariant in PROTOCOL.md now reads "no **on-the-fly** server-side transcoding" — offline pre-conversion is allowed because what gets shipped is still bit-exact to a real file on disk.
- **Feature gate is layered**: `cfg.Upscale.Enabled` (config) AND `transcode.PrecheckSox()` succeeds at startup (sox on PATH, --version returns within 2 s via `exec.CommandContext`). A `true` config with missing sox logs a `.error` and degrades to feature-off in-memory — bridge keeps running, advertises `upscaleEnabled: false` on `/v1/health`. Hard-restart required to flip the flag because the `transcode.Pool` is wired at constructor time.
- **VariantID-suffixed sidecar filenames** (`<sha256(source_relpath)>-<variantID>.flac`): hashing only the source path would let two runs at different target rates silently overwrite each other (Gemini bot caught at plan review). The DB row is authoritative for sidecar paths; never reconstruct from the filename pattern outside the producer.
- **`json_group_array` correlated subquery in store reads** ([store.go ListTracks/StreamTracks/ListTracksPage](internal/manifest/store.go)): one SQL per page regardless of variant cardinality. Per-track SELECTs would N+1 on a 50k-track library. The empty-table fast-path (`len == 2 && raw == "[]"`) skips the unmarshal in the common case.
- **Proactive sidecar cleanup in `Store.DeleteTrack` / `DeleteTracksByPrefix` / `WipeAllTracks`** (`removeSidecarFiles` shared helper): CASCADE drops the `track_variants` row but leaves the `.flac` orphaned. Without explicit cleanup, an admin "remove library root" or full-wipe would leak every sidecar belonging to that root with no DB row left for `--gc` to find by source-path lookup. CodeRabbit second-pass on PR #108. `bridge upscale --gc` is the recovery path that walks `<dataDir>/transcoded/` and removes files with no matching DB row (mark-and-sweep) for the cases where proactive cleanup escapes (interrupted DeleteTrack, manual SQL tampering).
- **Variant freshness check belongs to the api, not the manifest** ([api/files.go serveVariant](internal/api/files.go)). Pre-fix the manifest provider re-resolved the source path via hand-rolled basename-stripping which (a) failed in single-root mode (scanner emits paths WITHOUT root-basename prefix in that layout) AND (b) tripped CodeQL "uncontrolled data used in path expression". Both go away when the api owns the source-side stat call (it already validated the path via `bridgefs.Resolver.ResolveChecked` for the standard download path) and asks the variant store ONLY for "do you have this row, what's its recorded mtime/size". The interface is `VariantStore.LookupVariant` returning `*VariantRecord` (nil → 404); freshness verdict happens at the api site against the validated `os.FileInfo`. **Don't reintroduce path resolution inside the manifest package's variant-resolve path.**
- **Differentiate missing-sidecar (410 `variant_missing_on_disk`) from other open failures (500 `internal`)** in serveVariant. CodeRabbit second-pass on PR #108: pre-fix every `os.Open` error mapped to 410, hiding permission errors and transient I/O faults as if the variant were permanently missing.
- **`transcode.Pool` Stop()/Enqueue race fix**: pre-fix `Stop()` did `close(p.jobs)` while `Enqueue()` could be mid-send to the same channel — sending on closed channel panics. Fix: `Stop()` acquires `p.mu` before close; `Enqueue()`'s channel-send branch runs INSIDE the same mutex with a re-checked `closed` flag. The mutex serialises the two operations cheaply (sub-microsecond — the send is non-blocking via select + default). Gemini high + Qodo bug + CodeRabbit echo on PR #109. **Don't reintroduce a channel-send outside the mutex** at any new pool path.
- **Pool dedup keyed on (source_path, variant_id)**: a duplicate enqueue while a job is queued or running is a silent no-op — iOS can mash "Generate" without server-side duplicate work. The `inflight` map and the channel send claim/rollback the slot atomically inside `p.mu` (optimistic-insert pattern; rollback on `select default`).
- **Pool's bounded queue + non-blocking Enqueue** ([transcode/pool.go](internal/transcode/pool.go)). Default cap `cfg.Upscale.QueueCap` = 5000. `select { case p.jobs <- job: ... default: return ErrQueueFull }` — `POST /v1/upscale` then maps to `503 queue_full` when the entire candidate batch bounces, OR `202 Accepted` with `queueFull: true` and a partial-rejected count when some fit. **Don't widen Enqueue to a blocking send** — the user-spam-the-button case (50k-track folder long-press) MUST hit a clean rejection, not exhaust memory or wedge the HTTP path.
- **CLI cancellation returns exit 130 (POSIX 128+SIGINT)** so scripts can tell "interrupted mid-batch" apart from "completed cleanly with no failures". CodeRabbit on PR #108. The producer loop (`for _, c := range candidates { select { case jobsCh <- c: case <-ctx.Done(): break producerLoop } }`) MUST honor cancellation — without it, a SIGINT during dispatch (full jobsCh + slow workers) blocks until a worker drains.
- **`bridge serve`'s upscale enqueuer adapter borrows `apiSrv.Resolver()`** (NOT a snapshot from `cfg.LibraryRoots`). The api Resolver hot-reloads via SetRoots when admin removes/adds a library root at runtime; a snapshot resolver would silently keep routing against the old set and the upscale endpoint would 404 freshly-added paths. Qodo bug 2 on PR #109. The adapter is constructed AFTER apiSrv via a fresh `apiSrv.WithUpscaleEnqueuer(...)` call so they share the same hot-reloading instance.
- **`PrecheckSox` is bounded by 2 s `context.WithTimeout`**. Prevents a wedge from a broken PATH wrapper or hung sox process from deadlocking `bridge serve` startup. CodeRabbit second-pass on PR #108.
- **Per-job `context.WithTimeout(p.stopCtx, p.jobTimeout)` in `processJob`** (PR #162, default 10 min). Pre-fix workers passed the pool-wide `p.stopCtx` directly into RunSox, so a corrupt FLAC header / hung decoder / network FUSE backing the source path stalling in `read()` consumed a worker slot until the entire bridge was restarted. With the typical `Upscale.Workers` of 2–4, a small handful of pathological tracks queued back-to-back deadlocks the upscale feature with no operator signal beyond "queue stops draining". The per-job context kills sox via `exec.CommandContext` after the deadline, surfaces as a normal failed job in `/v1/upscale/stats`, and frees the worker slot. **Extracted into a separate `processJob` method** so `defer cancel()` releases per-job rather than accumulating until the worker exits — leaving the timeout cancel as a lifetime-scoped defer in `workerLoop` would leak a pending timer per processed job. **Shutdown gating uses `p.closed.Load()`, NOT `p.stopCtx.Err()`**: `Stop()` flips `p.closed` BEFORE it cancels `p.stopCtx`, so during the gap a worker draining buffered jobs sees `stopCtx.Err() == nil` even though Stop has been called — the suppression check would falsely classify a graceful-shutdown error as a real failure. The atomic flag is monotonic (false→true), so a single read at each branch suffices. CodeRabbit on PR #162.
- **`Pool.runner func(ctx, spec) (int64, error)` test seam** (PR #162, defaults to `RunSox` in `NewPool`). Same DI shape `manifest.Store.now` uses for clock injection — tests inject a hang-until-ctx-cancelled stub to drive the per-job timeout branch without a real sox process. **`Pool.jobTimeout time.Duration` is per-instance, NOT a package-level `var`** (Gemini × 5 on PR #162). Field-level override removes the global mutable that would race a future `t.Parallel()` and matches the existing per-instance DI convention. **Slot-reclaim assertion in TestPoolJobTimesOutAndCountsAsFailure**: enqueuing a second quick job after the first times out and requiring it to run within 2 s proves the recovery path; asserting `failedCnt` ticks alone would let a regression that times out the job but leaks the slot pass silently (CodeRabbit on PR #162).
- **Bounded coalescing publisher for transcode pool SSE events** ([transcode/pool.go](internal/transcode/pool.go)). Replaces the prior pattern of spawning one ephemeral `go fire()` goroutine per state transition (unbounded under burst — a 500-clip enqueue storm fanned ≥3000 ephemeral goroutines if the broker briefly stalled). One long-lived publisher goroutine (`runPublisher`) consumes two channels and invokes the wired callbacks synchronously, one at a time. **Channel sizing**: `stateChangeChan chan struct{}` cap=1 (state changes coalesce — wired callback always reads a fresh `UpscaleStatsSnapshot`); `jobCompleteChan chan jobCompleteEvent` cap=`2×workers` (`upscale.complete` events do NOT coalesce, each carries a unique path/variantID iOS keys on for its reverse index). **Send semantics**: workers `fireStateChange()` non-blocking send (drops are correct — next signal carries the latest state); workers `fireJobComplete()` blocking send for fidelity (a full buffer briefly stalls the next send, which is the correct backpressure). **Stop() ordering is load-bearing** — (1) close `p.jobs` under `p.mu`; (2) `stopCancel()` to kill in-flight sox; (3) `p.wg.Wait()` for workers to exit so no further sends are possible; (4) close `stateChangeChan` + `jobCompleteChan`; (5) `p.publisherWG.Wait()` for the publisher to drain remaining buffered events. The publisher does NOT exit on `stopCtx.Done()` — workers blocking-send to jobCompleteChan and an early publisher exit would deadlock the worker on a buffer-full send. The publisher uses a per-iteration channel-nil pattern (nil out a closed channel in the local select reference) so a closed channel doesn't hot-spin the loop until both are closed. **Deadlock avoidance preserved**: the prior `go fire()` shape existed because the wired broker callback re-enters `p.mu` via `Stats() → UpscaleStatsSnapshot`. The publisher is a SEPARATE goroutine; workers never hold p.mu while sending; broker callbacks run on the publisher's stack with no `p.mu` held. **Don't reintroduce `go fire()` at any new pool path** — funnel through `fireStateChange` / `fireJobComplete`. **And `Enqueue` MUST call `fireStateChange()` UNDER `p.mu`, before the unlock** (both `internal/transcode` AND `internal/analyze` pools; PR #461): workers fire from `processJob` so they're bounded by `Stop`'s `wg.Wait()` before `close(stateChangeChan)`, but `Enqueue` is NOT wg-tracked — firing AFTER the unlock lets a preempted enqueuer resume after a concurrent `Stop()` has run to completion + closed the channel, then panic on send-to-closed (which fires even inside a `select`/`default` — that guard only covers a FULL channel, not a CLOSED one; the `p.closed` re-check inside the lock guards only the JOBS send, not this). Under the lock the send strictly happens-before `Stop`'s close (Stop must take `p.mu` to close the jobs channels first, and closes `stateChangeChan` only afterward). Safe from the PR #136 deadlock because `fireStateChange` is a non-blocking send to this async publisher, NOT a synchronous callback into `p.mu`. **Don't move it back after the unlock to "shrink the lock window".** Pinned by `TestPoolEnqueueRacingStopNoPanic` in both packages (verified to panic on the pre-fix ordering under `-race`). Pinned by `TestPoolPublisherBurstStaysGoroutineBounded` (1000-job burst, peak goroutines ≤ baseline + workers + 16), `TestPoolPublisherCoalescesStateChanges` (1000 burst signals collapse to ≤ 2 callback invocations), `TestPoolPublisherJobCompleteFidelity` (100 jobs, 100 unique callback invocations, no drops), `TestPoolPublisherStopDrainsBufferedEvents` (50 jobs with slow callback, all reach callback after Stop returns).
- **`POST /v1/upscale` returns 202 Accepted, NOT 200** — the iOS `BridgeSourceClient.requestUpscale` MUST pass `allowAccepted: true` to `ensureOK`. Pre-fix every successful enqueue was classified as an error and the response body never decoded. CodeRabbit critical + Qodo bug 2 on PR #188. The `ensureOK` signature gained `allowAccepted bool = false` — defaults to false so existing callers that expect strict 200 are unaffected.
- **`POST /v1/upscale` body-path normalisation**: handler runs `strings.ReplaceAll(req.Path, "\\", "/")` before any `path.Join` math. Defensive against a Windows-deployed admin curl call OR a future Windows iOS client; the wire protocol explicitly requires forward slashes. Gemini medium on PR #109.
- **Three typed enqueuer error sentinels**: `ErrUpscaleQueueFull` (→ 503/partial 202), `ErrUpscaleSourceMissing` (→ silent reject in handler counts), `ErrUpscaleIneligible` (DSD source / already at target / fresh sidecar exists — silent reject). Defined in `internal/api` so the handler can `errors.Is` without importing `internal/transcode` (preserves the abstraction-down dep direction). cmd/bridge adapter translates `transcode.ErrQueueFull` → `api.ErrUpscaleQueueFull` and `transcode.ErrPoolClosed` → `api.ErrUpscaleSourceMissing` (Stop() race during shutdown surfaces as feature-unavailable rather than confusing-transient).
- **Mirror-PR pair contract**: PROTOCOL.md changes ship in lockstep with the iOS client's `docs/BridgeProtocol.md` mirror. v1.2 additions (`upscaleEnabled` on /v1/health, `variants` array on Track, `?variant=<id>` query on /v1/download, `POST /v1/upscale` endpoint) all stay at `ProtocolVersion: 1` — additive only. iOS clients aware of the new fields decode them; pre-v1.2 clients ignore them via the lenient default `JSONDecoder`.
---

## v1.2 — admin console upscale toggle + stats (PR #110)

Admin Settings page gets an "Audio quality" section with a toggle for `upscale.enabled`, an OS-aware sox install hint when missing, and a live stats card. Replaces "edit bridge.yaml + restart" with a real operator surface.

- **Decoupled wiring via closures** ([admin.go](internal/admin/admin.go) `Deps.UpscalePrecheck` + `Deps.UpscaleStats`). Same MBIDProbe / UpdateProvider / TailscaleProvider pattern — admin package compiles without importing `internal/transcode`. cmd/bridge wires `transcode.PrecheckSox` directly + a `func() *admin.UpscalePoolStats` that snapshots the live `upscalePool.Stats()` AND gates on **both** `upscalePool != nil` AND `cfg.Upscale.Enabled` (CodeRabbit on PR #110 — a live PATCH-off must immediately stop returning live counters even though the underlying Pool isn't torn down until restart, otherwise the off-with-history JS card-visibility logic breaks).
- **`/api/upscale/stats.enabled` reports LIVE runtime state, not the persisted config**. The two diverge in two real cases: (a) startup demoted the feature when sox-precheck failed even though `cfg.Upscale.Enabled == true`, (b) operator just PATCHed `upscaleEnabled = false` but the long-lived Pool is alive until restart. Both surface as `Pool == nil` from the closure; handler reports `Enabled = (Pool != nil)` so admin tile + iOS-facing `/v1/health.upscaleEnabled` agree about what "active" means. **The Settings PATCH form continues to read the persisted flag via `/api/settings.upscaleEnabled` separately** for the toggle's initial state — distinct semantic from the runtime field.
- **Sox-precheck result cached for 30 s** ([handlers_api.go](internal/admin/handlers_api.go) `cachedSoxAvailability` + `Server.soxAvailability{,Mu,At}` fields). The Settings page polls `/api/upscale/stats` at 5 s and the precheck shells out to `sox --version` with up to 2 s timeout; without the cache, an open Settings tab burns 12 × 2 s = 24 s/min on the probe (CodeRabbit major on PR #110). 30 s TTL feels right — operator installing sox waits at most 30 s for the UI to update.
- **OS-aware sox install hint computed server-side** ([handlers_pages.go](internal/admin/handlers_pages.go) `soxInstallHintForCurrentOS`). `runtime.GOOS` resolves on the bridge host, NOT the operator's browser host — sox needs to be installed where `bridge serve` runs. Coverage: Linux (apt/dnf/pacman), macOS (brew), Windows (choco / sourceforge). Mirrors the CLI's `printSoxInstallHint` one-to-one. Pre-computed boolean (`UpscaleSoxMissing`) + multi-line string (`UpscaleSoxInstallHint`) on `settingsResponse` so the html/template doesn't need a custom `deref` helper for the `*bool` precheck result.
- **Settings page PATCH `upscaleEnabled` flips marked `RestartRequired: true`** but only on actual change — idempotent same-value submissions skip the banner (no spurious "restart required" when the operator clicks Save with the displayed value unchanged). Test in `TestSettingsPatchUpscaleEnabled` pins the contract.
- **Stats card visibility** ([static/app.js](internal/admin/static/app.js) `refreshUpscaleStats`): hidden when `enabled == false && cachedVariants == 0` (fresh install never sees clutter); shown with em-dashed live fields when `enabled == false && cachedVariants > 0` (historical state visible during a feature-off window); shown with full live snapshot when `enabled == true`. **Don't reintroduce the always-visible card** — operators who never enabled the feature should see zero upscale chrome.
---

## v1.2 — public `GET /v1/upscale/stats` endpoint (PR #111)

Authenticated read-only mirror of the admin tile, exposed on the public protocol so paired iOS clients can render an "Upscaling" management section without surfacing the admin console.

- **Wire shape is field-for-field identical to admin's `upscaleStatsResponse`** — same `enabled` / `soxAvailable` / `pool` / `cachedVariants` / `cachedBytes` semantics. Documented in PROTOCOL.md. `internal/version.ProtocolVersion` stays at 1 (additive endpoint, not a breaking change). Pre-v1.2 bridges return 404 from the unregistered route — iOS treats identically to `{enabled: false}`, so the two paths are intentionally indistinguishable to clients.
- **`api.UpscaleStatsProvider` is the abstraction** ([upscale_stats.go](internal/api/upscale_stats.go)) — `cmd/bridge.upscaleStatsAdapter` wires it with the same closures the admin tile already consumes, so the operator's Settings page and a paired iOS client always see the same numbers (no drift between the two surfaces).
- **Sox precheck cached on the adapter at 30 s TTL** (mirrors `admin.Server.cachedSoxAvailability`). iOS polls `/v1/upscale/stats` at 5 s while the management page is foregrounded; without the cache, the per-poll fork-exec to `sox --version` would burn ~12 fork-execs/min for no good reason (gemini-code-assist on PR #111). Adapter holds its own cache rather than reaching into admin internals — same TTL means both surfaces stay aligned on what the host reports.
- **Authenticated, NOT a pairing probe** — same bearer-token gate as `POST /v1/upscale` and every other `/v1/*` except `/v1/health` and the pairing routes. Don't expose it unauthed; queue depth + failure counts are operator-visibility-only.
- **Known limitation — `cfg.Upscale.Enabled` is read without synchronization**. Same data race that existed in the admin tile's closure ([cmd/bridge/main.go:909](cmd/bridge/main.go)) when admin's PATCH writer mutates the same field under `admin.Server.mu`. Adapter doc-comment documents the worst case (one 5 s poll snapshot may report `enabled` inconsistently with a freshly-PATCHed state; the next poll converges). The proper fix is an `atomic.Bool` on `*config.Config` touching admin's writer too — out-of-scope for this endpoint addition.
---

## v1.2.x — variant write/delete bumps `tracks.indexed_at` (PR #156, mirror with iOS #255)

User-reported regression: variant for "Isn't It Romantic" was generated on disk + in `track_variants` at 11:40 today (Diana Krall, 84 MB FLAC 192/24), but the iOS-side wand sat at `.inFlight` for 1.5 h then surfaced as a stalled yellow triangle. Re-tapping looped `enqueued=0` → empty delta → no promotion.

- **Root cause**: `UpsertVariant` / `DeleteVariant` only wrote `track_variants`. The parent `tracks.indexed_at` stayed at its original index time. iOS delta-sync filter is [`ListTracks` `WHERE indexed_at > ?`](internal/manifest/store.go) — so a track whose only change since the last sync was a new variant fell outside the time window. Every iOS-side ladder rung's `scanShare` returned an empty delta; `Track.variantsJSON` stayed empty; the wand never promoted.
- **Fix**: `UpsertVariant` and `DeleteVariant` now wrap the variant INSERT/DELETE plus a parent `tracks.indexed_at` UPDATE in a single transaction. `defer tx.Rollback()` is the structural unwind guarantee. Both writes use a strictly-advancing SQL form. Three guarantees in one expression: (1) monotonic — never regresses under past-clock injection / NTP rewind; (2) STRICTLY advancing — same-clock-equality writes still advance so clients that synced at the equal timestamp can't miss the change (CodeRabbit round 2 on PR #156); (3) single statement, atomic under `s.mu`. **CORRECTED 2026-08-17 — guarantee (2) was only ever true relative to the row's OWN prior value.** This entry used to quote the expression as `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END`, whose `ELSE` arm assigns the raw clock and therefore lets a bump land exactly ON a cursor equal to a SIBLING row's value. All nine bump writers now share `indexedAtAdvanceSQL`, which clears the library-wide max — see the `indexedAtAdvanceSQL` bullet in "## Things that have bitten before" for the mechanism, the three deliberate exclusions, and the negative control.
- **`DeleteVariant` skips the bump on no-op delete** (RowsAffected==0). Bumping when the variant set is unchanged would create false manifest churn (CodeRabbit + Gemini round 1).
- **Injectable `Store.now func() time.Time`** (defaults to `time.Now`) lets tests inject a stepping clock so assertions are deterministic without `time.Sleep` flakiness on slow CI. Same DI shape we'd reach for the next time a write path needs a controllable clock; `UpsertTrack` stays on direct `time.Now()` (out of this PR's scope).
- **`HealthResponse.Features []string`** additive field advertises stable-key capability flags. iOS uses presence of `"variantBumpsIndex"` to skip its `+600s` silent fullRescan recovery rung — modern bridges no-op cheaply, only pre-fix bridges pay the defense-in-depth cost. Wire shape is purely additive (`omitempty`); `ProtocolVersion` stays put. Pre-fix iOS clients ignore the unknown JSON key.
- **`DeleteVariant` has zero production callers today** (the `bridge upscale --gc` path in [cmd/bridge/upscale.go:runGC](cmd/bridge/upscale.go) walks the FILESYSTEM and removes orphan sidecar files; it does NOT touch DB rows). Defensive plumbing — bump symmetry matches `UpsertVariant` so a future caller that does delete a variant doesn't lose iOS visibility.
- **Considered + rejected: SQLite `AFTER INSERT` trigger** to enforce the bump invariant at the DB layer. Bridge architecture has no external SQLite writers (UpsertVariant / DeleteVariant / GC are the only paths, all in Go), and a trigger fires on every test-fixture insert (complicating fixture setup) + adds invisible behaviour that bites future readers. Re-evaluate if a future feature ever needs to write `track_variants` from outside the Go layer.
- **Tests** (regression-locking is the load-bearing one): `TestUpsertVariantBumpsParentIndexedAt`, `TestUpsertVariantDeltaManifestSurfacesNewVariant` (mimics the user's exact iOS flow end-to-end), `TestDeleteVariantBumpsParentIndexedAt`, `TestDeleteVariantNoOpSkipsBump`, `TestUpsertVariantMonotonicGuard`, `TestUpsertVariantEqualClockStillAdvances`, `TestHealthAdvertisesVariantBumpsIndexFeature`. All under [internal/manifest/store_variants_test.go](internal/manifest/store_variants_test.go).
- **Pairs with iOS PR #255's defense-in-depth recovery** — bridge fix is load-bearing; iOS side adds a `+600s` silent fullRescan rung + foreground-resume hook gated on absence of the `variantBumpsIndex` flag for operators on pre-fix bridges. Either side's correctness is enough on its own; both together cover the full "wand stuck" matrix.
---

## v1.2 — mDNS rebind on interface set change (PR #113)

User-reported regression: long-running bridge (5 days uptime) logs "advertising" but `dns-sd -B _onebit-bridge._tcp` returns nothing. Verified via fresh restart — discovery immediately resurfaces.

- **Root cause**: `hashicorp/mdns` snapshots IPs at `NewServer()` and never re-binds. After any network transition (Wi-Fi roam, Ethernet plug, dock, sleep/wake) the cached UDP sockets are tied to interfaces that no longer carry the right IP.
- **Fix**: `Advertise()` spawns a 60 s rebind goroutine that diffs `ipsForAdvertise()` against the cached set and rebuilds the underlying `hcmdns` server when they drift. Cheap when nothing changes (single `net.Interfaces()` + sorted-string compare); the rebuild path runs only on the rare transition tick.
- **`Advertiser.cachedIPs` + `closed` flag are guarded by `rebindMu`** so the background goroutine and `Close()` don't race the server pointer. The `closed` flag is the belt-and-braces backstop for a stray tick that lands after `Close` has already fired (the `done` channel is the primary stop signal). `Close` drains the goroutine first, then shuts down the current server.
- **Test seam: `advertiseInternal(cfg, ipSource, interval, spawnLoop)`**. Tests inject a controllable `ipSource` and pass `spawnLoop=false` so they drive `maybeRebind()` directly. The race detector caught the alternative pattern (writing `Advertiser.ipSource` after `Advertise`); a lock-on-every-read fix would have added cost to the production 60 s hot path. Mirror of the seam pattern several iOS-side `_*ForTest` helpers use.
- **`ipSetEqual` is order-invariant** — `net.Interfaces()` may return addresses in different orders across calls (interface renames, hot-plug, kernel scheduler). Sorted-string compare since both sides are typically <10 entries on a dev machine; no need for a per-entry hash map.
- **Failure log uses the same `ips` key + `ipsForLog(...)` shaping as the success log** (Qodo on PR #112). Stable attribute keys per the logging convention — log-greppers correlate rebind sequences across success/failure ticks without learning a per-callsite vocabulary.
- **Pairs with iOS PR #193's Bonjour Refresh button** — either side's recovery is enough on its own; both together cover the full "discovery stopped working" matrix.
---

## Cross-page pairing badge (PR #161, mirror with iOS PR #267 Discover sheet)

Surfaces incoming pairing requests on every admin page (Dashboard, Library, Settings) instead of only on Devices — operators on other pages were missing new requests entirely. Companion to the iOS "Discover on network" sheet ([acoseac/1-bit#267](https://github.com/acoseac/1-bit/pull/267)) but ships independently; either side works on its own.

- **`#pairing-badge` lives in `layout.html`** (every page) — amber pill with count, links to `/devices`. `role="status" aria-live="polite" aria-atomic="true"` on the inner `<span class="pairing-badge-count">`, NOT on the outer `<a>` element. The anchor stays a link; the child span announces count changes politely. Putting `role="status"` on the anchor would override the native link semantics for assistive tech (CodeRabbit Major).
- **Pulse animation fires on count INCREASE only** (operator just got a NEW request, deserves attention). Decrease (operator approved one) doesn't re-fire. **`prefers-reduced-motion` strips the pulse** to a static brighter background — the heightened color conveys "new request arrived" without movement.
- **`lastPendingCount = null` baseline** — first SSE snapshot is treated as "establish state, don't pulse". Without this, opening a fresh admin tab while a pending request was already in flight would falsely pulse for an event the operator never missed (CodeRabbit). After the first snapshot lands, the increase-only comparison takes over.
- **`lastEventSourceSeenAt` tracking** drives the visibility-restore handler (refreshed on `onopen`, on every successful SSE event, AND on `visibilitychange`). The prior `lastEventSourceConnectAt` (stream-creation time) was wrong for this purpose — a long-running stream + brief tab switch would falsely classify the connection as idle and force-cycle it (CodeRabbit). The 60 s threshold is `SSE_RECONNECT_IDLE_THRESHOLD_MS` (extracted constant — Gemini).
- **Belt-and-braces `/api/pairing` backfill on visibility-restore** — covers the rare case where the SSE re-arm post-sleep takes a moment to deliver the first frame. Adds zero overhead on short tab switches (gated by the 60 s threshold).
- **Plain-color `--accent-warm` fallback before each `color-mix(...)` declaration** on `.pairing-badge` (light + dark mode). `color-mix(...)` containing `var(--warn)` is valid at parse but invalid at computed-value time (IACVT) in browsers without color-mix support (Chrome <111, Firefox <113, Safari <16.2). Without the fallback, IACVT resolves `background` to `transparent` and the dark-mode badge becomes unreadable black-on-clear (CodeRabbit Major).
- **Pulse animation peak `box-shadow` mix is 35%, NOT 0%** — the prior `0%` mix at the 50 % keyframe made the expanding ring fully transparent at peak expansion (the ring was there but invisible). 35 % keeps it visible mid-animation; 0 % at 100 % is correct (the ring fades out at the end).
- **Pending-pairing card itself bumped from green to amber**: `--ok 35%` border was easy to miss against the panel background. New: `--warn 60%` border + 4 px left accent stripe + amber-tinted background + bigger halo. Same visual language as `.warn-banner` elsewhere in the admin UI.
- **`--accent-warm-tint` precomputed CSS variable** for the card-background fallback path. Modern browsers progressive-enhance to `color-mix`; legacy enterprise webviews see the precomputed pastel without breaking.
---

## Bug-fix batch — review-identified regressions

- **`apiScan` MUST route through `spawnBackgroundScan`, NOT a raw `go func()`** ([handlers_api.go](internal/admin/handlers_api.go)). Pre-fix `POST /api/scan` launched a background goroutine without `s.bgScans.Add(1)` / `defer s.bgScans.Done()`, so admin shutdown (which waits on the `bgScans` WaitGroup capped at 5 s grace) could exit while the scan was mid-write to SQLite. The existing `spawnBackgroundScan` helper already carried the correct WG tracking + `scanCtx` usage + error logging; `apiScan` was the only caller that duplicated the logic without the WG. Comment on `spawnBackgroundScan` updated to reflect that `apiScan` calls it directly — the previous "mirrors the pattern in `apiScan`" note was stale. **Don't reintroduce a raw `go func()` at any new scan-trigger site — funnel through `spawnBackgroundScan`.** This is the same invariant documented under PR #76 ("Don't reintroduce a fire-and-forget `go func()` in `spawnBackgroundScan` without re-wiring the WG"); the `apiScan` handler pre-dated that entry and was missed.
- **`bridge tsnet logout` probes the admin port before wiping state** ([tsnet.go](cmd/bridge/tsnet.go)). Pre-fix the command was documented as a v1 limitation: it did NOT detect a running `bridge serve`, so wiping the tsnet state dir while the live server held open files inside it could leave runtime/disk state inconsistent. Fix: `isAdminAlive(cfg)` probes `http://<adminAddress>/api/stats` with a 500 ms timeout (same pattern as `tryLibraryViaAdmin` in `library.go`). If the admin responds, logout refuses with a clear error message directing the operator to stop the bridge first. `--force` flag overrides the guard for scripted/automated use. The function now uses `flag.FlagSet` to parse `--config` and `--force` instead of the shared `loadConfigAndRequireTsnetMode` helper (which doesn't support extra flags); the mode-check logic is inlined. Existing tests pass `--force` to exercise the confirmation-prompt logic without being affected by a local running bridge. New `TestTsnetLogoutRefusesWhileRunning` spins up a test HTTP server and asserts both the refuse path and the `--force` override. **Don't remove the admin-port probe without re-introducing an equivalent guard** (lock file, PID check, etc.) — the v1-era "operators are expected to stop first" honour system was the original design acknowledgement that this was needed.
- **`TestPoolPublisherBurstStaysGoroutineBounded` passes (not a regression).** The reported bug claimed the pinning test was FAILING with peak=38 (want≤28). Investigation: test passes consistently (5/5 runs, `go test -run TestPoolPublisherBurstStaysGoroutineBounded -v -count=1 ./internal/transcode/`). The production `onStateChange` callback ([cmd/bridge/main.go:1568-1597](cmd/bridge/main.go)) does call `UpscaleStatsSnapshot` with a 2 s context timeout, which is a blocking DB call — but the test's own callback uses a trivial `time.Sleep(50µs)`, so it doesn't reproduce the reported production stall. The test is sound as a goroutine-bound contract; the production callback's timeout guard (2 s `context.WithTimeout`) prevents the publisher from wedging indefinitely. No code change warranted.
- **`Year` recovers from ISO-8601 DATE/YEAR tags + `stringOf` is deterministic (PR #447, mirror with iOS #906).** `populateFromTagMetadata` trusted dhowden's `m.Year()` for the regular `Year` field, which returns **0** for a full-date value like `2023-06-09` (a valid DATE / TDRC tag — Melody Gardot's *Entre eux deux (The Paris Sessions)* is tagged that way), so the release indexed as year 0 and collided downstream with the 2022 standard edition. Fix: on the `Year()==0` case only, fall back to `parseYearPrefix` over the raw `tdrc`/`tdrl`/`tyer`/`date`/`year`/`©day`/`©yyy` tag — the same helper `OriginalYear` already used. A clean `Year()` (plain "2022") is untouched; an unparseable-but-present tag still surfaces as present-zero. **Also hardened `stringOf`**: it iterated `range raw` (Go map order is RANDOMIZED) and returned the first value whose key matched ANY requested alias, so a file carrying more than one matching tag (e.g. both DATE and YEAR) resolved a non-deterministic value across scans — flapping the parsed year. It now iterates the REQUESTED keys in PRIORITY ORDER (outer) and scans the map per key (inner); the earliest-listed alias wins deterministically. Identical for the single-match + empty-value cases; hardens every multi-alias caller (Year, OriginalYear, composer/conductor/work, genre, MB IDs). **Don't revert `stringOf` to `range raw`**, and **don't drop the `Year()==0` ISO-date fallback**. Value-only — `ProtocolVersion` unchanged (not a Mirror-PR; the iOS side independently shows the year on same-name album tiles to disambiguate). Locked by `extractors_year_isodate_test.go` (ISO/plain/timestamp/unparseable) + `TestStringOf_DeterministicKeyPriority` (200-iteration two-date-tag map).
---

## post-v0.1.4 — config validation split for public-mode VPS deploys

The bridge.ars.md production deploy of v0.1.4 surfaced a structural mismatch between the existing `config.Validate()` shape and the public-mode VPS layout. The bridge daemon runs as user `arsenie`; the binary lives at root-owned `/usr/local/bin/bridge`; the library is a `rclone --vfs-cache-mode full` FUSE mount at `/mnt/music` with the default `user_allow_other=false` (only `arsenie` can stat into it). Two mutually-exclusive failure modes resulted:

- **`bridge update` as `arsenie`** → atomic-rename inside `/usr/local/bin/` fails (dir not writable). Pre-flight check surfaces this cleanly: `binary path not writable by this user (try sudo bridge update)`.
- **`sudo bridge update`** → config validation tries `os.Stat("/mnt/music")` (called via `Validate()` → libraryRoots loop) and fails with `permission denied`. The update subcommand can't even load its config.

Neither path lets `bridge update` work on this layout. The CLAUDE.md "Step 3 — bridge.ars.md" canonical procedure (cross-compile + scp + sudo mv + sudo setcap + sudo systemctl restart) sidesteps both blockers but bypasses `bridge update` entirely — every release requires SSH from a whitelisted IP, brittle under residential CGNAT rotation.

**Fix**: `Config.Validate()` is now a pure shape check. The `os.Stat(r)` + `IsDir()` block moved out into a new `Config.CheckLibraryRootsAccessible() []*LibraryRootError` method ([config.go](internal/config/config.go)). Strictness is the caller's decision:

- **`bridge serve` startup** ([main.go:runServe](cmd/bridge/main.go)) — calls `CheckLibraryRootsAccessible` after the existing `bridgefs.ValidateRoots` shape check. Loopback installs fail-fast on any inaccessible root (preserves the historical typo'd-YAML protection). Public installs log a warning per failing root and continue — the scanner's PR #74 error-subtree machinery prevents the deletion pass from wiping the manifest of a momentarily-unreadable root, so the bridge can come up serving cached state while a slow FUSE mount catches up.
- **`bridge update` / `bridge status` / `bridge admin reset-password` / etc.** — never call the accessibility check at all. These subcommands don't touch the library, so a temporarily-inaccessible root MUST NOT block them.
- **Mutation paths** (`bridge library add` at [library.go:69](cmd/bridge/library.go), admin's `apiRootsAdd` at [handlers_api.go:391](internal/admin/handlers_api.go)) — already do their own `os.Stat` + `IsDir` before persisting. Unchanged; the duplicate stat in `Validate` was redundant defensive coverage.

**Test contracts pinned**:
- `TestValidatePassesNonexistentLibraryRoot` + `TestValidatePassesLibraryRootIsAFile` — `Validate()` returns nil even when the root doesn't exist or isn't a directory.
- `TestCheckLibraryRootsAccessible{Nonexistent,IsAFile,AllPresent,PermissionDenied}` — the new method's full contract.
- `TestLoadSucceedsWithInaccessibleLibraryRoot` — the bridge.ars.md regression target at the `Load()` level. Builds a tmpdir whose PARENT is chmod 0o000 so `os.Stat` returns EACCES; the test confirms `config.Load()` succeeds (the surface `bridge update` invokes) AND `CheckLibraryRootsAccessible` flags the same path.

**Don't reintroduce `os.Stat(r)` into `Validate()`** without rethinking the public-mode update path. The split is the load-bearing contract. If a future caller needs the accessibility check, invoke `CheckLibraryRootsAccessible` explicitly — it preserves the call-site visibility the inline stat hid.

**Gotcha for the permission-denied test**: making a directory mode `0o000` doesn't reliably produce EACCES on `os.Stat(thatDir)` itself — the kernel lets you stat a 0o000 directory, it just refuses to enumerate it. To produce a real EACCES, chmod the PARENT to `0o000` and stat the leaf inside. Register the chmod-back-to-0o700 cleanup IMMEDIATELY after the chmod-down (and before any further test work) so it survives an early `t.Fatalf` — `t.TempDir`'s own cleanup is registered BEFORE the test body runs, so it fires AFTER your `t.Cleanup` and walks the tree successfully.
---

## v1.6 — cross-bridge playlist backup + playback telemetry (bridge PRs #334 / #337 / #336, iOS PRs #620 / #621 / #624 / #623)

The bridge gains **per-device state** for the first time — playlist backups and playback-history telemetry — anchored to a durable device identity, NOT the ephemeral auth token. Additive throughout: `ProtocolVersion` stays `1`; iOS gates on `/v1/health.features` flags (`playlistBackup`, `playbackHistory`, alpha-sorted in the `feats` builder). Mirror-PR pair: `PROTOCOL.md` ⇄ iOS `docs/BridgeProtocol.md` stay byte-identical (Appendix-A wire-contract sections cover every new payload).

**Device identity is the durable recovery token, never the auth token.** iOS generates a 64-char lowercase-hex token once (`Keychain.loadOrCreateRecoveryToken`, `kSecAttrSynchronizable=false`, `AfterFirstUnlockThisDeviceOnly`) — survives reinstall, NOT iCloud-synced, so each physical device is its own backup shard. Sent as `X-Device-Token` on every authed request (+ in the pairing body). The bridge's `device_registrations` table (migration **v10**) binds it to the *current* auth token; the binding self-heals across re-pairings. **`BridgeSourceClient.deviceRecoveryToken` caches in memory ONLY when the Keychain confirms persistence** — a background launch while the device is locked returns a transient (`persisted==false`) token that must NOT be cached process-wide, else it sends a one-off `X-Device-Token` that vanishes next launch and orphans the device's backups. `loadOrCreateRecoveryToken` mints only on `errSecItemNotFound` (transient errors return uncached); a malformed/corrupted persisted value is validated (64-hex), deleted, and reminted (the add-only save can't replace a present item).

**`api.touchDevice` debounce — three load-bearing rules** ([internal/api/api.go](internal/api/api.go)): (1) record the in-memory `deviceSeen` entry **only after a successful upsert** — stamping before the write lets a transient SQLite/ctx-cancel failure suppress the retry for the full 5-min TTL (the binding would self-heal only after TTL, not on the next request). (2) Reserve an **in-flight key while the upsert runs** so a cold-start burst of concurrent same-device requests fires the upsert once, not N times. (3) **Key the in-flight reservation on `(deviceToken, tokenID)`, NOT `deviceToken` alone** — a concurrent rebind (`dev,tokB` while `dev,tokA` is in flight) must still write immediately, preserving the "immediate on bind change" contract. Pairing-path `deviceToken` is validated with the same `validDeviceToken` (bounded lowercase hex) the header path uses; a malformed value is dropped to `""` (don't fail pairing over an optional field).

**Playlist backup = safe, not federation** (migration **v11**, `playlists` + `playlist_items`). A playlist may mix tracks from several bridges + local/SMB; items owned by *another* bridge are stored as **opaque references** (`origin_fingerprint` = the owning bridge's cert fp or a `local`/`smb` sentinel, + `origin_path`) the bridge NEVER resolves or serves — iOS re-resolves them locally on restore. Each item is strictly **local-XOR-foreign**: local has only `path`; foreign has BOTH `origin_fingerprint` AND `origin_path` (partial items → 400). Item `position` must be non-negative AND unique — a dup hits the `(playlist_id, position)` PK and would surface as 500 not 400, so the handler rejects dups up front. `UpsertPlaylist` does the ownership + LWW-guard checks **inside one transaction** (no TOCTOU): a strictly-older `last_modified_at` → `ErrPlaylistStale` (handler 409s with the full server copy for one-round-trip reconcile); an id under a different device → `ErrPlaylistOwnedByOther`. **`last_modified_at` is the only precision-critical wire field** — client UnixNano `Int64`, never round-tripped through a string/`Date` (truncation would falsely trip the 409). All scoped to the caller's device token (other devices' rows are invisible / 404). iOS: `Playlist.markModified()` bumps the LWW key on every mutation (add/remove/reorder/cross-share-cleanup); `PlaylistSyncCoordinator` fetches **only the dirty playlists' referenced track IDs** (predicate + `propertiesToFetch`), never the full 50k manifest, and does all `@Model` work on the main context (carrying `Sendable` value types across the network awaits — never a live `@Model` across actors).

**Playback telemetry = opt-in, owner-visible** (migration **v12**, `playback_history`). `duration_used` is `REAL` → scan into Go `float64` (fractional skip seconds survive). `started_at` is UnixNano. **`POST /v1/history/batch` validates-then-transacts**: malformed events (empty path / non-positive `startedAt` / non-finite|negative `durationUsed`) are *dropped and counted* (`202 {accepted, dropped}`), never faulted — one bad event must not roll back the device's other stats. Event `path` has its **leading slash stripped** to match the scanner's slash-free track paths (iOS normalizes bridge paths with a leading `/`). The index is `(device_token, id DESC)` to match `ListHistory`'s `ORDER BY id DESC` cursor paging (a `started_at` index would filesort). Owner-visible only — surfaced in the loopback admin console (`/api/history` histograms), never off-host; no new auth layer. iOS: `PlaybackHistoryEntry` IS the durable offline queue (`uploadedAt==nil` = pending; survives flights/relaunch). **Eligibility (opted-in bridge advertising `playbackHistory`) is decided at capture time in `PlaybackTelemetryCoordinator.end()`** — PlayerService reports for every track, but a non-eligible source (SMB/local/opted-out) is dropped rather than persisted, so the queue never fills with rows no bridge will ever accept. `flushPending` has an `isDraining` reentrancy guard (track-end + reachability change can both fire) and is gated on reachability AND `thermalState < .serious`. **Telemetry capture is macro-hook only, never the render loop**: the session is armed at *play-start* (`status = .playing`, so the captured AVAudioSession route + sample rate are settled — not the previous track's) AND in the gapless-promotion branch (which bypasses `startCurrent`); `startCurrent`'s top clears the session so a non-arming path can't leave a stale one for the next `end()` to mis-finalize.

**Per-bridge UI**: Edit Bridge "Sync & History" section (two toggles gated on the feature flags + a restore button); pairing seeds the flags from the join-time `/v1/health` and defaults **playlist backup ON when advertised** (user's own server, recoverable) but **telemetry OFF** (opt-in by design). Both coordinators use the app's no-arg-init + `attach(modelContext:reachability:)` lifecycle (mirrors `DownloadCoordinator`), injected via `.environment`.

**Stacked-squash-merge gotcha (process)**: when landing a stacked PR set bottom-up with squash merges, **retarget each child's base to `main` BEFORE deleting the parent's branch** — GitHub auto-CLOSES (and won't reopen) a PR whose base branch is deleted, and a PR whose head momentarily equals its base also auto-closes. Two PRs (#335, #622) were lost this way and recreated as **#337 / #624** (identical content). When re-propagating a stack after rebasing a parent, use `git rebase --onto <new-parent-tip> <OLD-parent-tip>` — passing the child's *own* tip as the base replays zero commits and silently collapses the child onto its parent.
---

## v1.6.x — public-mode pairing QR advertises the served (LE) cert (PR #338)

Diagnosed from a live `bridge.ars.md` pairing failure: a phone scanning the admin "Pair a new device" QR got iOS's "TLS fingerprint doesn't match the pairing link" error. **Root cause**: a public-mode bridge (autocert) serves its **LE cert** on the public-domain SNI via the [SNI cert switcher](internal/tls/manager.go), but `buildPairURL` baked `deps.Fingerprint` (the **self-signed** LAN cert fingerprint) and `defaultBridgeURL` defaulted the dial URL to `<hostname>.local` — both leftovers from before public mode existed. The phone captured the LE cert (`7E:E2:40…`), compared it to the baked self-signed value (`34:7E…`), mismatch → reject.

- **`Manager.FingerprintForServerName(host)`** ([manager.go](internal/tls/manager.go)) returns the fingerprint of the cert `Get()` would serve for that SNI (autocert LE for the public domain / Tailscale magic-DNS; self-signed otherwise). **It MIRRORS `Get`'s SNI routing rather than delegating to it** — PR #339 changed this; the autocert branch must read the cached leaf directly, because `Get(syntheticHello)` returns a different leaf for a hello lacking real cipher-suite / sig-alg negotiation. (An earlier version of this entry still claimed it "delegates to `Get` so it can never drift"; that was true only as written in PR #338. Corrected 2026-07-28.) **Because the routing is duplicated, every freshness/validity gate in `Get` must be restated here** — the Tailscale branch lost its `CertNotAfter` check in that refactor, so an expired LE leaf made the pairing QR advertise a fingerprint the listener never presents (`Get` had already fallen back to self-signed) and every join failed the pin check. Pinned by `TestFingerprintForServerName_AgreesWithGet`, which asserts the two agree across fresh and expired certs. The pairing-QR baker (mint + rotate) calls it via `pairFingerprint(dialURL, selfSigned, deps.FingerprintForHost)`; **self-signed fallback** when the resolver is nil (loopback/tests) or returns "" (host unresolvable). `deps.FingerprintForHost` wired in `cmd/bridge/main.go` to `certManager.FingerprintForServerName`.
- **`defaultBridgeURL(cfg)`** now prefers the public endpoint (`autocert.Domain`, then first `customEndpoint`) in public mode instead of `<hostname>.local`. Loopback mode unchanged.
- **Bridge-only fix, no wire change, `ProtocolVersion` unchanged** — iOS already handles LE-cert pins + the public-host pinning carve-out (iOS PR #532). Post-pairing iOS switches to CA validation for the public host, so this **survives the ~60-day LE renewals**; the baked LE fingerprint is load-bearing only for the one-time first-contact check.
- **`deps.Fingerprint` stays self-signed** — it's the dashboard cert-tile (LAN pin) display; only the *pairing QR* needed the served-cert value. **Don't revert the QR baker to `deps.Fingerprint`** or a public-mode device will fail the pin check again. **`pairURLHost` prepends `https://` for scheme-less operator input** (`bridge.ars.md:8443`) so the fingerprint resolver doesn't silently fall back to self-signed (Gemini MEDIUM on PR #338). Locked by `FingerprintForServerName` tests (5) + admin pairing tests (`defaultBridgeURL` public/loopback, `pairFingerprint`, `pairURLHost`).
---

## v1.7 — user-wide device state: cross-device playlists + readable history (PR #371)

Playlists and listening history stopped being per-device silos (product call 2026-06-10: every paired device belongs to the operator; multi-user is a future re-scope, not the present model). Additive throughout — `ProtocolVersion` stays 1. **The iOS `docs/BridgeProtocol.md` mirror was NOT updated in this pair** (iOS was under separate review) and the drift compounded through the v1.9 smart-playlists additions before being **reconciled byte-identical in iOS PR #910 (2026-06-27)** — the Mirror-PR rule is satisfied again. (Future per-PR additions still ship as Mirror-PR pairs; this entry records the one-time catch-up.)

- **Playlists are id-scoped, not device-scoped.** `GetPlaylist(ctx, id)` / `ListPlaylists(ctx)` / `TombstonePlaylist(ctx, id)` carry NO device token; `UpsertPlaylist` keeps the deviceToken param but ONLY as last-writer provenance (`ON CONFLICT ... SET device_token = excluded.device_token`). The LWW stale guard (strictly-older `last_modified_at` → `ErrPlaylistStale` → 409 with full server copy) is the ONLY write gate. `ErrPlaylistOwnedByOther` + the `playlist_conflict` 409 are GONE — don't reintroduce an ownership guard without the multi-user user-id re-scope; PROTOCOL.md documents the old behaviour for pre-flag bridges. A tombstoned row revives on a newer-clock upsert (LWW decides, not the tombstone — `TestUpsertPlaylistRevivesTombstone`). `X-Device-Token` stays REQUIRED on all four routes (provenance + registration freshness), even though reads don't use it.
- **`GET /v1/history` is the all-devices read feed** (bearer-auth, NO device-token requirement — deliberately). `ListHistory` LEFT JOINs `device_registrations` for source-device attribution; `HistoryEventOut.SourceDeviceToken` is a SECRET (recovery token) — wire DTOs MUST NOT expose it; the handler derives `deviceId` = first 16 hex of SHA-256(token) (`historyDeviceID`), documented in PROTOCOL.md so iOS can hash its own token to mark "this device". `SourceDeviceName` ≠ `DeviceName` (the latter is the OUTPUT hardware / DAC). `nextCursor` is 0 on a short/empty page — the handler mirrors the store's limit clamp (default 200 / max 1000) precisely so the short-page check is meaningful; don't remove the handler-side clamp "because the store already clamps".
- **Feature flags `playbackHistoryRead` + `playlistsCrossDevice`** gate the behaviour for iOS (alpha-sorted; gated on the same store wirings as their sibling base flags; feats capacity now 14). Playlist write-path hardening rode along: path-id capped at 128 (`maxPlaylistIDLen`), item inserts hoisted onto one prepared statement (the per-item `ExecContext` re-prepared at every row — measurable at the 50k cap).
- **Deferred, deliberately**: `playback_history` has NO retention policy; `deviceSeen`/`device_registrations` grow per distinct (valid-bearer-presented) device token with no reaper; both documented as future work, not bugs. *(Closed 2026-09-02 by PRs #819/#822/#829, and the closing work is itself reviewed in the 2026-09-06 LOUPE entry at the end of this file — the reaps shipped with an unbounded window that could delete the whole table.)*
---

## Review-batch invariants (PRs #372 / #373 / #374 / #375, 2026-06-11)

Four fix PRs from a verified comprehensive review (4 parallel reviewers, every finding re-traced against the code before fixing — accuracy was ~100%, unlike the DeepSeek baseline's 70% FP rate).

- **`Ingester.Run` is serialised by `runMu`** ([upnpingest/ingest.go](internal/upnpingest/ingest.go)). The periodic lifecycle tick and the admin ForceRescan goroutine share one Ingester (ForceRescan's `inFlight` latch only guards against concurrent ForceRescans) — two overlapping walks race their reconcile sweeps: the later-walkStart walk can reap rows the earlier walk wrote after the later one passed that path. Don't remove the mutex; a queued second run is cheap (SystemUpdateID gate short-circuits).
- **`reapOrphanServers` is the ONLY lifecycle for a removed UPnP server's rows** (PR #372). Runs at the top of every `Ingester.Run`; reaps every `server_udn` in `upnp_track_routing` absent from the configured `StableServerKey` set. Pre-fix NO such path existed (the RemoveServer docblock claimed one did) — a removed 15k-track upstream stayed in the manifest forever because PR #370 deliberately blinds the fs scanner to routed rows. **Feature-disabled runs deliberately do NOT sweep** (temporary toggle must not wipe state; re-ingest would lose cached enrichment in `tags_json`). Per-orphan failures accumulate via `errors.Join` and continue. Locked by `TestIngester_Run_ReapsOrphanServerRows` + `TestIngester_Run_DisabledLeavesOrphanRows`.
- **`effectiveWalkErr` folds `WalkStats.Truncated` into the walk error** (PR #372). Per-container `ErrBrowseLimit` is non-fatal to the walker (keeps walking siblings, returns nil + `stats.Truncated`) but the result is a PARTIAL library view — it MUST route through the same no-reap + no-`idStore.Set` guard as `ErrWalkTruncated`. Pre-fix the ingest discarded the stats and this truncation flavour fell through to the reconcile sweep. Don't call `BrowseFoldersWalk` anywhere without consuming the stats.
- **SSDP server-discovery refreshes a known UDN's controlURL on host change** (PR #372, [upnp/discovery.go](internal/upnp/discovery.go) `handlePacket` + `sameURLHost`). Pre-fix the exists-branch only bumped LastSeenAt, so after DHCP renew the cached controlURL pointed at the dead address forever (TTL eviction never fires while the server answers M-SEARCH). `sameURLHost` compares "same" on unparseable input (no refetch storms). **The renderer-side cache (`internal/dlna/discovery`) still has this bug** — its transient-failure STUB semantics interact (a failed refetch would stub-clobber a good entry), so the twin needs its own pass; don't blind-copy the server-side fix there.
- **Root `BrowseMetadata` childCount MUST equal the `BrowseDirectChildren("0")` container count** (2: All Tracks + Folders) — strict controllers (mconnect/Cling class) validate the two against each other. Asserted in the root-metadata test; a third root container must bump both sites.
- **Per-route write-deadline overrides** (PR #373, [api/route_classification.go](internal/api/route_classification.go)). `route.writeDeadline` (zero = 60 s default) exists because `boundedHandler` starts the clock BEFORE the handler runs; `POST /v1/upscale` (synchronous whole-folder WalkDir) and `DELETE /v1/upscale/variants` (per-variant fsync'd delete loop) get 15 min (`upscaleLongOpWriteDeadline`) — pre-fix the server-side work completed, the response write failed, and the client retried the entire operation. `TestRouteRegistry_writeDeadlineOverrides` pins the exact override set; a new override is a deliberate decision that must land there.
- **`safeQuery` on EVERY path-bearing query consumer** — `DELETE /v1/upscale/variants` was the one miss (PR #373): `r.URL.Query()` form-decodes literal `+` to space, so deletes for paths containing `+` silently no-op'd with `200 {deletedCount: 0}`. When adding a handler that reads a library path from the query string, use `safeQuery(r)`.
- **Reachability probe runs under `context.WithoutCancel`** (PR #373). The singleflight result is shared by every joined caller — with the first caller's ctx, a client hang-up mid-probe synthesized an "offline" verdict for healthy concurrent callers. Only the 2 s probe timeout can fire now; a timeout-fired offline IS the root's real state and is cached. Don't reintroduce caller-ctx coupling inside the singleflight closure.
- **Updater downloads ride a timeout-free client** (PR #374, [updater/github.go](internal/updater/github.go)). `http.Client.Timeout` caps the ENTIRE exchange including body read — the 10 s poll client killed multi-MiB archive downloads on links slower than ~1.5 MB/s, permanently (auto-installer retried into the same wall). `Client.download` has `Timeout: 0`; the download phase is bounded by `downloadTimeout` (15 min ctx) at the Install site. `release-meta.json` deliberately stays on the poll client. Structural pin: `TestNewClient_DownloadClientHasNoOverallTimeout`.
- **codesign fallback is GATED on flag-unsupported** (PR #374, [updater/verify_darwin.go](internal/updater/verify_darwin.go)). Two stacked bugs: `--strict --no-strict` meant strict never ran (last flag wins); and the unconditional `--strict`-only fallback silently bypassed notarization on modern macOS (signed-but-not-notarized → first fails, fallback passes). `notarizationFlagUnsupported(err)` (case-insensitive substrings anchored on "check-notarization", classifyMintError posture) is the only route to the fallback; real verification failures surface directly, and a failing fallback surfaces err2 (the actual verdict), not the flag complaint. Table-test-pinned.
- **`IsTransient` includes ECONNREFUSED / ENETUNREACH / EHOSTUNREACH** (PR #374). An MB nginx restart or boot-before-network window surfaces as these (NOT `net.Error.Timeout()`) — classifying them persistent negative-cached + markSkipped-stamped every in-flight track (the PR #74 poisoning class via different errnos). Pinned in the classification table test with production `net.OpError → os.SyscallError` wrapping; DNS-NXDOMAIN stays persistent deliberately.
- **`auth.Store.FlushLastUsed` reloads before persisting** (PR #374). The shutdown flush rewrote tokens.json from the in-memory slice — a sibling `bridge pair` mint with no subsequent authed request was silently deleted at shutdown. `reloadIfStale()` then `persist()` (RecordClientVersion's shape; both hold `s.mu`); reload failure ABORTS the flush (losing a debounced timestamp is recoverable, deleting a sibling's token is not). `TestFlushLastUsedPreservesExternalMint`.
- **Admin config mutations: re-`Load()` + `config.Clone` INSIDE `Server.mu`** (PR #375). `apiVariantsDirPatch` cloned a pre-mutex snapshot (and shallow `next := *cfg` — wrong, `Upscale.OptimizeEnabled` is `*bool`), so a settings PATCH committing between Load and Lock was silently reverted. Cheap validation may read an early snapshot; the WRITE must clone fresh under the mutex. Same shape as `apiSettingsPatch`.
- **`assertNotUnderLibraryRoots` resolves symlinks via an `evalSymlinksOrClean` twin of config's** (PR #375). Lexical-only admin validation accepted symlinked variants dirs that `config.Load`'s validation rejects at next boot (bridge refuses to start over a value the UI said was fine). The two copies carry lockstep notes; keep them in sync.
- **UTF-8 truncation is `trimPartialTrailingRune` (O(1)), NOT validate-the-whole-string** (PR #375, lockstep twins in [transcode/pool.go](internal/transcode/pool.go) + [tailscale/tailscale.go](internal/tailscale/tailscale.go)). The PR #75 `for !utf8.ValidString` loop shape is ONLY safe for inputs guaranteed valid except at the cut (HTTP headers); sox stderr / localized CLI output can carry INTERIOR invalid bytes, and the validate-loop discarded everything after the first bad byte (plus O(N²) rescans). The new helper drops at most `utf8.UTFMax-1` trailing bytes (the split-rune case); interior garbage stays (JSON encodes U+FFFD). `TestRedactSoxErr_InteriorGarbagePreservesMessage` pins the difference.
- **`fsBasenameCap = 255 - len(sidecarTmpSuffix)`** (PR #375). sox writes `<sidecar>.tmp`; a 255-byte basename made the tmp name 259 bytes → ENAMETOOLONG, failing exactly the long-classical-filename inputs `safeVariantFilename` exists for. The suffix const is shared with the writer sites — don't reintroduce a bare 255.
- **`backup.Snapshot` removes its partial dir on any error/cancel** (PR #375). Partials carry a near-full DB copy, have no `manifest.json`, `List` skips them, so `Prune` could NEVER reclaim them — and admin-triggered snapshots run on `r.Context()` (browser disconnect mid-VACUUM cancels). Named-return + deferred `os.RemoveAll(dst)` on non-nil error.
- **M3U8 export flattens CR/LF in every device-supplied field** via the package-level `m3uReplacer` ("\r\n" first so CRLF collapses to ONE space) (PR #375). A `\n` in a playlist name/title injected arbitrary playlist lines (a bare URL line = media location → beacon when the operator opens the export). CSV/JSON exports are safe by construction.
---

## v1.7.x — library-root flip spares UPnP rows; inspector root derives from tracks (PRs #404 / #405, 2026-06-15)

Diagnosed live on a **hybrid library** (2 filesystem roots + a Chord 2Go UPnP upstream: 122 FS tracks + 15,283 routed). Two correctness fixes; bridge-only, no `ProtocolVersion` bump, no iOS mirror (admin/internal surfaces only). The CSS fix rode along in #405.

- **A single↔multi FS-root transition MUST call `Store.WipeFilesystemTracks`, NEVER `WipeAllTracks`** (PR #404). The transition rewrites every filesystem track's stored path form (bare `Artist/…` ↔ `<basename>/Artist/…`) so FS rows must be wiped + re-scanned — but `WipeAllTracks` does a bare `DELETE FROM tracks` which CASCADE-deletes `upnp_track_routing` + `track_variants`, **destroying the ENTIRE upstream library + its cached enrichment (`tags_json` MBIDs) on a mere FS-root-count toggle** (the wipe-loop class, cf. PR #369/#370 — a full re-ingest + re-enrich of every routed track). `WipeFilesystemTracks` deletes only rows `WHERE NOT EXISTS (SELECT 1 FROM upnp_track_routing r WHERE r.source_path = tracks.path)` (index-backed anti-join on the routing PK) **plus all folder rows** (folders are FS-only AND flip form with the root count → all rebuilt by the rescan). Routed rows carry the `<server>/…` form independent of FS root count; their lifecycle belongs SOLELY to the ingest reconcile (the PR #370 invariant). Spared UPnP rows never carry sidecars (remote — no local file for the sox pipelines), so listing every sidecar for removal can't orphan a spared row's cache. **4 call sites**: `apiRootsAdd`/`apiRootsRemove` (transition/collapse branch) + CLI `wipeManifest` (`library add` 1→N) / `libraryRemoveCmd` (collapse). `WipeAllTracks` survives as the true-wipe primitive but has NO production caller in the flip path anymore. **Public-mode is a no-op** (a public VPS is UPnP-`Validate`-rejected → no routing rows → behaves like the old wipe). **Don't reintroduce `WipeAllTracks` on the flip; don't make `WipeFilesystemTracks` skip the folder wipe** (the flip changes folder path form too). Locked by `TestWipeFilesystemTracks_SparesUPnPRoutedRows`.
- **The admin Library Inspector's ROOT (empty-parent) browse derives top-level folders from `tracks`, NOT the `folders` table** (PR #404). The `folders` table has TWO blind spots at the root: (1) in MULTI-ROOT mode the scanner records each root's contents under `<basename>/…` but NEVER inserts a bare `<basename>` folder row (shallowest entries are `<root>/.`), so the old `WHERE instr(path,'/')=0` match found ZERO rows → inspector root rendered empty (user-reported); (2) UPnP ingest never writes the folders table at all. The shared `topLevelFSFolderSource` const (`SELECT DISTINCT substr(t.path,1,instr(t.path,'/')-1) … WHERE instr(t.path,'/')>0 AND NOT EXISTS (… upnp_track_routing …)`) backs the empty-parent branch of `ListChildFoldersPage` / `ListChildFolders` / `CountChildFolders`. The UPnP anti-join is **deliberate**: routed tracks are remote + non-upscalable, and the inspector is a variant-generation surface, so it intentionally lists only filesystem roots. `instr(path,'/')>0` skips loose root-level files (they surface via the empty-parent `ListChildTracks*` path). Single-root mode is preserved (top level = artist folders, same set the folders-table query produced). The non-empty-parent folder branches additionally exclude the `<root>/.` sentinel (`AND … NOT LIKE '%/.'`) so drilling into a multi-root basename doesn't surface a junk `.` folder. **Don't revert the empty-parent branches to the `folders` table; don't drop the `NOT LIKE '%/.'` sentinel filter; don't drop the `NOT EXISTS upnp_track_routing` exclusion** (would surface non-upscalable remote tracks in the variant UI). **Known cosmetic gap, deliberately deferred**: at the whole-library scope the inspector subtree counter still totals ALL tracks (incl. UPnP) via `RollupByPrefix("")` — a header number; the folders themselves are correctly hidden. Locked by `TestListChildFoldersRoot_MultiRootDerivesFSRootsHidesUPnP` + `_SingleRootDerivesAlbumFolders` + the updated `TestCountChildFolders` (now seeds tracks, not bare folders).
- **Both UPnP anti-joins use `NOT EXISTS`, not `NOT IN`** (Gemini on PR #404). Semantically identical today (`source_path` is the routing PK, non-null) but idiomatic + NULL-safe against a future nullable schema. The pre-existing `IncrementMissingTracksAndDeleteAtThreshold` threshold-DELETE (PR #370) keeps its own `NOT IN` form — out of #404's scope, not flagged.
- **The Settings `.section-head` hide rule is scoped to `:first-of-type`** (PR #405). The "Audio" tab now houses THREE section-heads (Audio quality / Audio analysis / Smart mixes); the global `.settings.tabs-enabled .tab-nav ~ .tab-pane > .section-head` visually-hidden rule was hiding ALL of them, running the sub-sections together. `:first-of-type` hides only the first (redundant with the tab label) and reveals the genuine sub-section headings. Every pane opens with its section-head as the first `<h2>`, so single-section panes still hide their one heading exactly as before. **Don't drop `:first-of-type` in a future "minimize the settings CSS" pass** — it re-hides the multi-section pane's sub-headings.
---

## CLI-hardening batch — doctor Windows port attribution + sox FLAC verify + init re-prompt (PRs #432 / #433 / #434, 2026-06-22)

Three CLI/console-hardening PRs from an external-suggestion review. Bridge-only; no `ProtocolVersion` bump, no iOS mirror (all CLI / admin / internal surfaces). Each took two bot-review rounds; the round-2 fixes are folded into the invariants below.

- **Native Windows port→PID attribution via `iphlpapi.dll` `GetExtendedTcpTable`** (PR #432). Pre-fix Windows had no owner probe (`lsofPath==""` → `portProbeAvailable()==false`), so `checkPort` degraded EVERY bound port to **Warn** — couldn't tell our own running bridge from a real conflict on the one OS a live bridge runs (home-pc). The probe is pure-Go (no shell-out), split per-platform behind the existing build-tag convention (`internal/doctor/doctor_windows.go` / `doctor_notwindows.go`, mirroring `startup_windows.go`/`startup_notwindows.go`). **Load-bearing constants/contracts:** (a) **`TCP_TABLE_OWNER_PID_LISTENER = 3`, NOT 4** — 4 is `_CONNECTIONS` (established sockets), which misses every listener and makes the probe useless; this was the round-1 critical bug bots caught, named as a const so it can't silently drift. (b) **The probe is a predicate `isPIDListeningOnPort(port, targetPID) (bool, error)`, NOT `pidListening() int`** — membership across ALL matching rows (query BOTH `windows.AF_INET` + `AF_INET6`) avoids a dual-stack / multi-interface first-match false-fail, AND the **error return is load-bearing**: a probe-MECHANISM failure (AV blocking the DLL load, `ERROR_NOT_SUPPORTED`, or lsof can't start) makes `checkPort` degrade to **Warn**, never a hard Fail. `portProbeAvailable()` is hardcoded `true` on Windows, so without the error channel a broken syscall would falsely Fail a healthy install. **Don't revert to a bare bool/int return.** (c) v4/v6 share ONE generic scan — `tcpTableEntries[T]` + `pidOwnsPortFamily[T]`; a local `{ NumEntries uint32; rows [1]T }` carries the per-family header shape, **bounds-checked (`unsafe.Offsetof`/`Sizeof`, compile-time, BEFORE `unsafe.Slice`)** so a short/corrupt buffer can't read OOB. Sizing is the canonical two-call pattern in a **bounded retry loop** (the table can grow between size-query and fetch — TOCTOU). `ntohsPort` uses `windows.Ntohs`. The generic dedup was the round-2 fix (Sonar `new_duplicated_lines_density` failed at 6.5% when the two near-identical funcs tripped the 3% gate). (d) **Unix lsof predicate: only exit code 1 = "ran, no match → not us"** (`errors.As` + `ExitCode()==1`); any other non-zero exit or can't-start error → `(false, err)` → Warn. Don't treat all `*exec.ExitError` as "not found". **Windows behaviour can't be CI-tested** (runners are linux/darwin) — `doctor_windows_test.go` (`//go:build windows`) is the home-pc gate; `make build-all` cross-compiles Windows and catches compile errors.
- **`bridge doctor --json` carries self-describing metadata** (PR #432). `--json` already existed (PR #82); added `schemaVersion` (const `doctorJSONSchemaVersion`, currently 1) / `generatedAt` (RFC3339 UTC) / `platform` / `arch` to the `jsonDoctorReport` DTO for container health-probes + CI orchestrators. **CLI-only report, NOT the v1 wire** — bump `doctorJSONSchemaVersion` on a shape change, never `ProtocolVersion`. `--json --fix` still keeps fix-progress lines on stderr so stdout stays valid JSON (PR #82 invariant).
- **`transcode.ProbeSox(ctx)` verifies sox FLAC support before enabling features** (PR #433). The bridge forces `-t flac` for EVERY conversion (`SoxArgs`), so a FLAC-less sox (common on minimal `apt`: FLAC ships in `libsox-fmt-all`) passes the bare runnable check, the feature enables, then EVERY upscale/analysis job fails at runtime. `ProbeSox` is the SINGLE sox-inspection primitive (one `sox --help` spawn → version + formats); **`PrecheckSox` is now a thin wrapper** over it (`{ _, err := ProbeSox(context.Background()); return err }`) — don't re-duplicate. **Conservative contract — callers act ONLY on `FormatsKnown && !HasFLAC`**; an unparseable `sox --help` (`!FormatsKnown`) is treated as "FLAC present" so a help-output reword can NEVER disable a working install (don't change to fail-on-unknown). ProbeSox wraps the INCOMING ctx with a 2 s cap (propagates parent cancellation AND keeps the startup deadline), pins `LC_ALL`/`LANG`/`LANGUAGE=C`, uses `CombinedOutput` (`--help` often → stderr), and parses the block STRUCTURALLY — `soxAudioFileFormatsRE` (`(?i)AUDIO FILE FORMATS:`, `FindStringIndex` — byte-accurate vs `strings.ToUpper(text)`) up to the next ALL-CAPS section header (`soxSectionHeaderRE`) or EOF, surviving wrapped/reordered layouts. `parseSoxVersion` returns "" for a digit-less token (Homebrew HEAD prints a bare `SoX v`). Test seams: `soxLookPath` + `soxProbeCommand`. **Wired at every site a FLAC-less sox would fail silently**: serve gates (`soxFeatureReady(ctx,…)` → degrade to feature-off), CLI preflight (`soxCLIReady(ctx,…)` shared across upscale/optimize/analyze → exit 1; extracting it also kept `analyzeCmd` under the Sonar cognitive-complexity budget), the config-gated **`audio-toolchain` doctor check** (ALWAYS in `Run()`; a no-op "not enabled" when neither `Upscale`/`Analysis` is on, so minimal installs / `init` preflight aren't nagged), and the admin Settings tile (`admin.Deps.UpscaleSoxFLAC` closure; both admin sox closures share one 30 s-TTL `soxToolchainCache` so the Settings page does ≤1 fork-exec/window). **FLAC stays admin-only — deliberately NOT on the public `/v1/upscale/stats` wire** (which keeps the boolean `soxAvailable`); adding it there would force a `ProtocolVersion` review + iOS mirror.
- **`bridge init` re-prompts on an invalid library path instead of aborting** (PR #434). A paste typo used to abort the whole command (single `os.Stat` → exit); the interactive prompt now re-prompts (bounded `maxLibraryPrompts=5`, colored `✗ <reason>` via `paint(ansiRed,…)`). Declined the original "full bubbletea/tcell TUI" suggestion — it contradicts the documented "one static binary, no runtime deps" posture (the menu deliberately avoids even `golang.org/x/term` raw mode); this is the lightweight fix within the cooked-mode `ask()`/`confirm()` vocabulary. **Empty Enter is a DELIBERATE hard abort (exit 2), NOT a re-prompt** — the intentional escape hatch, matching the non-interactive `--yes requires --library` failure so a piped-stdin run fails identically whether or not `--yes` is set. CodeRabbit flagged this as a bug across both review rounds; **rejected both times** (it then reviewed the rationale and withdrew). **Don't change empty-Enter to re-prompt** — the loop re-prompts only on an *invalid* path. `askLine` is EOF-aware: ANY non-nil read error (io.EOF / closed pipe) is terminal so a broken reader can't spin the loop to its cap; a valid path typed then Ctrl+D (data + EOF together) is still accepted. The `--library` flag + `--yes` / public-mode paths are unchanged (single-shot hard error, exit 1 on a bad value). `resolveLibraryDir` is the shared validate helper. Locked by `TestPromptLibraryDir{Reprompts,EmptyAndEOF,EOFWithData,Exhaustion}` + `TestResolveLibraryDir`.
---

## admin-console UX batch — composition bars + SSE telemetry + worker grid + Camelot wheel + skip badges (PRs #435 / #436 / #437 / #438 / #439, 2026-06-23)

Five-PR stacked batch, all **admin-console-only** (no `/v1/*`, no `ProtocolVersion` bump, no iOS mirror, no schema migration). 4/5 verified with screenshots; PR E wire+unit only (the preview sandbox hung). **Deploy pending** (a human glance at PR E's inspector badge + the live worker grid after deploy closes the visual gap).

- **#435 composition bars.** `manifest.FormatDistribution` is a full-table `json_extract` GROUP BY (format fields live in `tags_json`, NOT columns) — **NEVER call it on the 500ms/5s SSE tick**; `admin.getCompositionSnapshot` wraps it in a 60s TTL cache + `singleflight.Group` (a **`Server` field**, not request-local) so N tabs hitting the 30s tick after expiry collapse to ONE scan. `buildComposition` is pure + deterministic (fixed PCM/DSD tier order; codecs count-desc then label-asc) so the SSE byte-diff stays stable. New `composition` SSE event (initial-emit + 30s slow tick). Bars include ALL rows so they reconcile to `TracksIndexed`; the **"Unknown" bucket** collects rows with no `sampleRate`/`bitsPerSample`. **CORRECTED 2026-07-22 — the original note here claimed the bridge extracts those "for FLAC/DSD only (the dhowden / WAV / AIFF / M4A paths don't)" and pointed at a spawned follow-up. That was already being fixed as it was written: [PR #440](https://github.com/acoseac/1-bit-bridge/pull/440) (2026-06-23, "extract PCM geometry for WAV/AIFF/M4A/MP3") landed the same week.** WAV reads `fmt ` and AIFF reads `COMM` in [extractors_aiff_wav.go](internal/manifest/extractors_aiff_wav.go); M4A reads the sample-description atom. What legitimately remains in "Unknown" is narrower: lossy sources whose bit depth `canSetBitsPerSample` deliberately refuses (the PR #225 structural gate), containers with no PCM geometry to report, and unparseable files. **Don't re-derive an "extractor gap" from this entry — check the extractor before believing a doc about it** (a stale claim here plus a too-narrow grep is exactly how it got re-reported as pending work months later). `isDSD` CAST mirrors the proven `ListTrackProjectionsUnderPrefix` form (SQLite `json_extract` returns int 1/0 for JSON booleans). Locked by `TestFormatDistribution` + `TestBuildComposition`.
- **#436 SSE telemetry.** Retired the Settings page's two 5s pollers; `upscale` + `analysis` SSE events on the **medium (5s) tick + the synchronous initial-emit slice** (direct-nav hydrates on connect, no blank window). `getUpscaleStatsSnapshot`/`getAnalysisStatsSnapshot` are the single source of truth shared by the REST handlers (kept as thin wrappers + first-paint fallback) and the SSE publisher; both take the **connection `ctx`** (Gemini #436 — cancels the snapshot's DB query on disconnect), NOT `context.Background()`. Diff-suppressed via handler-local `last*` (auto-reaped on `ctx.Done()`). `apiEvents` initial-snapshot is now 8 events — keep the SSE tests' frame count in lockstep.
- **#437 worker grid.** Per-worker `Pool.activeJobs []atomic.Pointer[ActiveJob]` (sized at `NewPool`, stable `workerID` threaded through `workerLoop(workerID)`/`processJob(workerID, job)`). **`ActiveJob` is IMMUTABLE after `Store()`** — lock-free `ActiveWorkers()` reads are torn-free, and it carries `StartedAtUnixMs` (NOT a ticking elapsed) so the SSE frame stays diff-stable while a job runs (browser ticks elapsed via a 1s `setInterval`). **`finishJob(workerID, dedup)` clears the slot + releases dedup BEFORE each terminal `fireStateChange`** — a bare top-level `defer Store(nil)` would LIFO-run AFTER the body's explicit fire and publish a stale "active" frame; slot `Store(&aj)` AFTER the cooperative-stop early-return (no flash on an abandoned job). The grid streams on the **fast (500ms) tick WHILE the pool is busy** (cheap `Deps.UpscaleBusy` gate — atomic `Stats()`, NO DB; `wasUpscaleBusy` latch fires one final idle frame), so **sub-5s jobs are visible at per-second resolution** (the 5s structural refresh alone missed them — caught live during verification); idle bridges fall back to the 5s tick. `JobSpec.SourceBits` (display-only) at all 5 enqueue sites. **Don't reintroduce a top-level `defer Store(nil)`; don't add a server-computed `elapsedSec` to the DTO** (would churn the diff every tick). Locked by `TestPoolActiveWorkersReflectsInflight` (`-race`).
- **#438 Camelot wheel.** `manifest.KeyDistribution` is a cheap GROUP BY on the REAL `track_analysis.key_root/key_mode` columns (no `json_extract`); naturally local-only (UPnP tracks aren't analysed). `smartplaylist.toCamelot` → exported **`ToCamelot`** is the single source of truth for the wheel's codes (the sequencer's own mapping — don't duplicate). New `json` template helper (`template.JS`; `json.Marshal` HTML-escapes so no `</script>` break) embeds `KeyCoverage` for one-shot first-paint (key data changes only after analysis — no SSE). SVG built via `createElement` (`viewBox`, responsive, `role=img` + per-segment `<title>`); hover adjacency uses **safe cyclic modulo** `((n-2+12)%12)+1` / `(n%12)+1` (JS `%` is remainder — a naive `(n-1)%12` underflows at the 12↔1 seam). Locked by `TestKeyDistribution`.
- **#439 skip badges.** Per-track skip reason lives on **`browseTrackRow.SkipReason`** (the BROWSE rows ARE the displayed tiles), NOT the projection response — the projection is folder-scoped, so its tracks are NOT the displayed tiles (an earlier draft put it on the projection; pivoted). `fundamentalSkipReason(isDSD, codec, *rate, *bits)` is kind-agnostic + target-independent (`dsd_bitstream` / `lossy_source` / `unknown_format`), computed from each row's own data — always visible while browsing, no projection round-trip. **The recursive projection loop + its aggregates are UNCHANGED.** `lossyCodecLabels` (MP3/AAC/OGG/OPUS/WMA) is a denylist for the LABEL only — eligibility elsewhere stays a PCM allowlist; codec `""` is `unknown_format`, not lossy. DSD > lossy > unknown precedence. **Declined Gemini's `isLossyCodecLabel` fast-path** — `strings.ToUpper`/`TrimSpace` don't allocate for already-clean ASCII (Go stdlib returns the original `s`), so the fast-path adds a redundant lookup for the common non-lossy-uppercase codec; false-positive optimisation. Locked by `TestFundamentalSkipReason` (15 cases) + `TestIsLossyCodecLabel`.
- **Process.** Stacked-squash-merge done per the documented gotcha: merge bottom-up, `git rebase --onto origin/main <child's-fork-point-SHA> <child-branch>` (fork point = the parent's ORIGINAL tip, captured BEFORE any review-fix commits), `--force-with-lease`, `gh pr edit <child> --base main` BEFORE deleting the parent branch, then merge. All 5 rebases were clean (a linear stack replays cleanly onto main). Only Gemini reviewed (3 MEDIUM findings → 2 applied, 1 declined); CodeRabbit/Qodo not active on this repo.
---

## DLNA/UPnP discovery review batch (PRs #469 / #470 / #471, 2026-07-03)

Three parallel-off-main PRs triaging an external review (`bridge-review-03-01`): 12 fixes, 3 declines. Bridge-only (internal DLNA/UPnP discovery internals — no `/v1/*`, no `ProtocolVersion` bump, no iOS mirror). DLNA/SSDP is a LAN feature (public VPS runs public-mode which rejects UPnP), so the deploy surface is the LAN bridges only.

- **#469 — UPnP `MediaServerDiscoveryClient` gets the renderer's shutdown-race `WaitGroup`.** The upstream MediaServer client ([internal/upnp/discovery.go](internal/upnp/discovery.go)) was missing the `wg sync.WaitGroup` the renderer `SSDPDiscoveryClient` ([internal/dlna/discovery/client.go](internal/dlna/discovery/client.go)) already got in external-review-r3 — so an in-flight `fetchAndCacheDetails` that already passed its `runCtx.Err()` guard could `Upsert` into the cache AFTER `Stop()` cleared it (leaks a ghost server that never ages out). Mirrors the renderer template EXACTLY: `Start()` `wg.Add(2)` under `runMu` before spawning the loops; loops + every tracked fetch `defer wg.Done()`; `handlePacket`'s two fetch spawns route through `spawnDetailFetch` (`Add(1)` before `go` — safe vs a concurrent `Wait` because `handlePacket` runs ON the runLoop goroutine which already holds a slot, so the counter goes ≥1→≥2 never 0→1); `Stop()` `wg.Wait()`s OUTSIDE `runMu` before `cache.Clear()`, keeping `runCancel` non-nil across the Wait so a concurrent `Start()` refuses instead of racing `Add` against `Wait`. **`cache.Clear()` MUST run UNDER `runMu`** (the final critical section) in BOTH clients' `Stop()` — after the unlock, a concurrent `Start()` could pass its `runCancel != nil` guard in the gap, spin fresh loops that `Upsert`, and have the stale `Clear()` wipe them (Gemini HIGH; applied to both twins). **Don't drop the `wg`, don't move `Clear()` back after the unlock, don't skip the `spawnDetailFetch` Add(1) at either spawn site** (the `replace_all` that converts them is indentation-sensitive — a missed site panics with a negative WaitGroup counter, which the mirrored `TestStopWaitsForInFlightFetch` catches).
- **#469 — both SSDP read loops survive transient UDP errors instead of dying.** Pre-fix a non-timeout, non-shutdown `ReadFromUDP` error `return`ed permanently, killing discovery for the process lifetime. On Windows an *unconnected* UDP socket surfaces `WSAECONNRESET` on the read following a send whose datagram drew an ICMP port-unreachable (`SIO_UDP_CONNRESET`) — a transient one-shot (Gemini-confirmed). The shared policy lives in ONE exported `discovery.HandleReadErr(ctx, err, *streak, *slog.Logger) (stop bool)` both loops call: timeout → reset streak + retry; `net.ErrClosed`/ctx-done → stop; else → log + **ctx-aware backoff** (`ssdpReadErrBackoff` 250ms, bounds a persistently-broken socket at ~4 reads/sec instead of hot-spinning a core) + retry, escalating Warn→**Error once at `ssdpReadErrEscalateAt` (20 ≈ 5s)** for a distinct "sustained-degraded" operator signal. Chose the **whitelist-free** form over a `syscall.WSAECONNRESET` fast-path (portable — no `_windows.go` split; the socket only sends multicast M-SEARCH which doesn't elicit ICMP unreachable, so the reset is rare here; the OS recv buffer holds packets during the backoff). **The shared helper is load-bearing for the SonarCloud duplication gate** — keep the policy there, don't re-inline it per loop. Locked by `TestHandleReadErr`.
- **#470 — tolerate real-world SOAP responses.** (a) `FetchGetProtocolInfo` accepts HTTP **500** through to the body parse (SOAP 1.1 §4.4 returns Faults with 500) so `ErrSOAPFault` surfaces instead of an opaque `status 500`; a non-fault 500 body re-attaches the status via `%w` so `errors.Is(ErrSOAPFault)` still works. (b) **Version-tolerant renderer service lookup** — `ParseDeviceDescription`'s `canonicalServiceType` folds `AVTransport:2/:3` / `RenderingControl:N` / `ConnectionManager:N` to their `:1` map keys (numeric-version-only rewrite; unknown types pass through) so a modern renderer isn't wrongly "no AVTransport". Mirrors the version-tolerant prefix match `internal/upnp.lookupContentDirectoryControlURL` already uses. Same-service-multi-version collapses last-wins (correct — minor UPnP revisions are backward-compatible). (c) `ParseBrowseResponse` + `parseSystemUpdateID` ([internal/upnp/didl.go](internal/upnp/didl.go)) match faults case-insensitively (`strings.EqualFold`) — parity with the sibling dlna parser. (d) The two fixed GetProtocolInfo / GetSystemUpdateID SOAP envelopes are hoisted to package-level `var`s (mirror `silenceWAVCache`), shared-immutable (read-only `bytes.NewReader` callers).
- **#471 — dual-stack-safe LAN eligibility (`IsLANEligibleInterface`, [internal/dlna/interfaces.go](internal/dlna/interfaces.go)).** Pre-fix returned `true` on the FIRST private/link-local address, so a pure-WAN NIC (public IP + an `fe80` link-local, no private) was wrongly LAN-eligible via the `fe80` short-circuit. Now a scan-all-then-decide predicate: **`return hasPrivate || (hasLinkLocal && !hasPublic)`** — eligible iff it has a private (RFC1918/ULA) address, OR it ACTUALLY carries a link-local with no public unicast. The `hasLinkLocal` flag is load-bearing: the literal Gemini-consult predicate `hasPrivate || !hasPublic` REGRESSED the existing `no_addrs` / `loopback_addr_only` → `false` table cases (a no-usable-address interface has no public → would falsely qualify). **DECLINED the PR-review Gemini security-HIGH "disqualify on any public IPv4"** — it contradicts the DELIBERATE existing tests (`multi_addr_first_private` = `[192.168, 8.8.8.8]` → `want: true`), reverses the Q2 consult (dual-stack home LANs carry a public IPv6 via SLAAC alongside private v4 → "disqualify on any public IP" breaks DLNA on IPv6 home nets), and isn't a target deployment (home = NAT'd no public v4; VPS = DLNA off). SSDP is multicast-local; the correct place for public-attach defense-in-depth is the DLNA HTTP bind, not interface selection. **Don't reintroduce the public-IPv4 disqualification; don't drop `hasLinkLocal`.**
- **#471 — `relParentDir` ([internal/dlna/folder_index.go](internal/dlna/folder_index.go)) normalizes separators BEFORE the `libRoot` prefix check.** Pre-fix `strings.HasPrefix(absPath, libRoot)` ran on the raw backslash path while `libRoot` (from `longestCommonPathPrefix`) is forward-slashed, so a Windows path leaked the raw `C:/lib/...` into the DLNA folder hierarchy (LCP-fallback path only — production uses `RelativePath`). The `libRoot` prefix match needs no boundary guard (declined CodeRabbit): `longestCommonPathPrefix` ALWAYS returns a trailing `/`, and its suggested `libRoot+"/"` would double the slash and break the common case. `WrapDIDLLite` pre-sizes its builder (`sb.Grow(220 + Σlen)`, mirror `DIDLForTrack`).
- **Declined (don't relitigate):** F14 "M-SEARCH missing `\r\n` after HOST" was a **false positive** (the `\r\n` is present; the reviewer misread that the following `` `MAN: …` `` backtick literal is a separate concat token). F3 "hard-fail on `SetMulticastInterface`" declined — the soft-fail is the documented fix for the 2026-05-27 Windows+Tailscale incident; added a Warn-log on the upnp side (which had swallowed the error) for parity instead. F10 telemetry `Snapshot` append = cosmetic churn.
- **SonarCloud `new_duplicated_lines_density` on #469 (11.7% vs 3%, accepted non-blocking).** The genuinely-shareable read-loop policy WAS extracted (`HandleReadErr`, 13.4%→11.7%); the residual is the UPnP `Stop()` + its `TestStopWaitsForInFlightFetch` DELIBERATELY mirroring the renderer client — distinct types in distinct packages with unexported lifecycle fields (`runMu`/`runCtx`/`handlePacket`), so a cross-package test helper can't reach them and a shared generic base would contradict the codebase's "keep the two clients parallel-but-separate" design (cf. `ServerCache` docblock). The required `gate.yml` (fmt+vet+`-race`+build-all) is green; Sonar is a non-blocking status here (accepted per operator, not gamed).
---

## bridge0901 review batch (PRs #486 / #487 / #488, 2026-07-10)

Three parallel-off-main PRs triaging an external LLM review (`bridge0901.md`): all 3 findings real, but 2 of the finding's *suggested fixes* were wrong-as-written. Bridge-only — no `ProtocolVersion` bump, no iOS mirror. Each took a 2nd Gemini round (both HIGH, both valid + applied). All CI green (SonarCloud cleared after a test-scaffolding dedup via the existing `quickStore` helper).

- **#486 — pairing `onTimer` closes the revoke token-leak window; `Delete` won't abort an in-flight revoke.** Pre-fix the `StateApproved` timeout branch captured `revokeID`, released `s.mu`, and revoked out-of-lock while the row stayed `StateApproved` with `RawToken` populated — across the revoke call AND every backoff-retry window. `Poll` gates token delivery solely on `State==StateApproved` ([store.go](internal/pairing/store.go) `Poll`), so a concurrent poll handed iOS a token the revoke then destroyed → silent 401s on every subsequent request. Fix: transition the row to `StateExpired` UNDER `s.mu` BEFORE the out-of-lock revoke. The revoke bookkeeping moves to `req.TokenID`: the terminal-state branch (`StateDeclined`/`StateExpired`/`StateCertRotated`) re-dispatches the revoke when `TokenID != ""` — only a previously-minted, now-expiring row reaches a terminal state carrying a token (Declined / CertRotated / the Pending→Expired sweep never mint), so the discriminator is exact and the bounded retry loop is preserved. **Round-2 (Gemini HIGH):** that `Expired`-with-token retry window is now `Delete`-reachable — a concurrent `Delete` (the client holds the pollSecret) would stop the retry timer + drop the row, orphaning a still-live token in `auth.Store` that the revoke never killed. `Delete` now rejects `State==StateExpired && TokenID != ""` (returns `ErrNotFound`, placed AFTER the pollSecret auth check) so the revoke lifecycle stays owned by `onTimer`; the "expired → token dies" semantics become consistent (a late ack can't resurrect a doomed token). **Don't leave the row `StateApproved` during the out-of-lock revoke, don't "simplify" the terminal branch back to a bare `delete`, don't drop the `Delete` guard.** Locked by `TestApprovedTimeoutRevokeDoesNotLeakTokenViaPoll` (polls from INSIDE the injected revoke, where `s.mu` is released) + `TestDeleteDuringRevokeRetryIsRejected`; `TestRevokeRetryOnFailure` / `...GivesUpAfterMaxAttempts` / `TestDeleteAfterApprovePreventsRevoke` still pin the retry loop + normal-ack path. The Explore agent that first read only the *Approved* branch wrongly concluded the finding "breaks the retry loop" — the finding also modified the terminal branch; tracing BOTH branches through the two retry tests confirms they pass.
- **#487 — `GenerateWithOptions` is a two-phase commit so a failed `bridge cert rotate` can't destroy the cert/key.** Pre-fix `writePEM` was truncate-in-place (`O_TRUNC`) AND `cmd/bridge/cert.go` `os.Remove`d both files before regenerating — a failed / interrupted rotation (disk full, kill) left the bridge unbootable. Round-1 made `writePEM` atomic + dropped cert.go's pre-delete, but that alone was insufficient (**Gemini HIGH round-2**): the cert was atomically renamed over the old cert, then a key-write failure ran the orphan-cleanup `os.Remove(certPath)` — deleting the freshly-committed cert, leaving NO cert at all. Fix: `writePEM` → `stagePEM` (stage to temp, NO rename); `GenerateWithOptions` stages BOTH cert+key temps, THEN `atomicwrite.RenameWithRetry`s both into place. A failure writing EITHER file leaves the pre-existing pair fully intact (bridge boots on the old cert); the orphan-cleanup `os.Remove` calls are GONE. `tmp.Chmod(mode)` runs before the rename so 0o644 cert / 0o600 key land with no window; `RenameWithRetry` absorbs the Windows AV-scan-on-close window (%LOCALAPPDATA%). **Residual (accepted):** a crash BETWEEN the two renames leaves a new-cert / old-key mismatch — fails CLOSED (`LoadX509KeyPair` rejects), recovered by re-running rotate; irreducible without a dir-swap, and rotate is a rare operator-in-the-loop action. **Don't restore the per-file-write + orphan-`os.Remove` shape, don't re-add cert.go's pre-remove** (the rotate CLI relies on the atomic overwrite). Locked by `TestGenerateKeyFailureDoesNotDeleteCert` (cert rename succeeds, key rename fails → a cert still exists) + `TestGeneratePreservesExistingOnWriteFailure`. Closing check: `internal/auth/auth.go` `persist()` already uses the canonical atomic temp+rename (its only `os.Remove`s are scratch-tmp cleanup) — no pre-delete anti-pattern there. (Latent nit, deferred: `auth.persist` / `config.Save` / `updater.SaveState` use bare `os.Rename`, not `RenameWithRetry` — no Windows AV-window retry; low severity, infrequent/debounced writes.)
- **#488 — DLNA `TelemetryMiddleware` records 500 (not stale 200) on a pre-header panic** ([internal/dlna/telemetry.go](internal/dlna/telemetry.go)). The deferred `Record` read `recw.statusCode`, which stays at the `http.StatusOK` default when a handler panics before `WriteHeader` — logging a dramatically-failed request as a clean 200. Fix: capture `recover()` at the TOP of the defer, record 500 when `rec != nil && !recw.wroteHeader`, keep the SINGLE existing `Record`, then re-panic (preserving net/http's per-request recovery). The `!wroteHeader` guard preserves a genuinely-committed status (a handler that streamed a 206 + body then panicked still records 206 — that's what the client received; `telemetryWriter` sets `wroteHeader` in `WriteHeader` / `Write` / `ReadFrom`). The finding's literal code added a SECOND `Record` inside the recover block → would DOUBLE-RECORD every panic; rejected. (net/http doesn't literally emit 500 on an unrecovered panic — it aborts the connection — but 500 is the right convention for "handler panicked".) Locked by `Test_TelemetryMiddleware_PanicBeforeHeaderRecords500` (500 + exactly one entry) + `_PanicAfterHeaderKeepsStatus`.
- **Follow-up #489 (2026-07-10) — atomic temp-file commits use `atomicwrite.RenameWithRetry`, not bare `os.Rename`.** The deferred nit from #487: seven "write temp → rename over final" sites (`auth.persist`, `config.Save`, `updater.SaveState`, `adminauth` store, `transcode`/`analyze` sidecars, `backup` snapshot) used bare `os.Rename`, so — unlike the enricher / scanner / tls paths — they lacked the Windows Defender / Search-Indexer scan-on-close rename-retry window. Swapped all seven to `atomicwrite.RenameWithRetry` (POSIX first attempt always succeeds → the loop is a no-op off Windows; each site keeps its OWN `Chmod` / `Sync` / parent-dir fsync / tmp-cleanup — the swap is one line each, NOT a move to `WriteBytes`, which would drop e.g. `auth.persist`'s belt-and-braces `Chmod(0o600)` from PR #70). **`atomicwrite.RenameWithRetryCtx(ctx, …)`** is the cancellation-aware variant (the inter-attempt sleeps become a `select` on `ctx.Done()` so a cancelled worker returns `ctx.Err()` instead of sleeping out the ~750ms budget); used ONLY at the two ctx-cancellable job-worker sites (`transcode.RunSox`, `analyze.RunAnalysis`) so a shutting-down pool frees its slot promptly. `RenameWithRetry` stays the context-free form (delegates with `context.Background()`) for the one-shot writers — no shared-helper signature break (CodeRabbit suggested threading ctx everywhere; scoped to the two job workers instead). **Deliberately NOT swept:** the updater binary-swap renames (`swap_{windows,unix}.go` — a transactional 3-way rename + rollback with Windows SCM coordination, distinct semantics) and `cmd/bridge/variants.go` (opportunistic migration move). **Don't swap those two; don't thread ctx into the one-shot writers; don't collapse the per-site temp handling into `WriteBytes`.** Locked by `TestPersistRetriesRenameOnTransientFailure` (auth) + `TestRenameWithRetryCtx_CancelledReturnsPromptly` / `_RetriesThenSucceedsUnderLiveCtx`. **SonarCloud note:** the swaps touch pre-existing intentionally-duplicated persist boilerplate; round-1's dup flag (3.8%) cleared once the round-2 ctx variant + tests raised the non-duplicated new-line count (non-blocking either way, per the #469 precedent).
---

## v1.8 — PDF album booklets (bridge PR #496 + Atlas #108 + iOS #1041, merged 2026-07-13)

Booklets flow Atlas → bridge → iOS. Additive wire (`ProtocolVersion` stays 1; `Track.bookletTag` + `GET /v1/booklet/{mbid}` + the `booklets` health flag — PROTOCOL.md section "PDF album booklets", mirrored byte-identically in iOS `docs/BridgeProtocol.md`). Migration **v24** (`booklets` table + `idx_tracks_release_mbid` functional index + `tracks.booklet_tag`).

- **No provenance anywhere, by design.** The upstream source (Qobuz goodies via the operator's Atlas) is never named on the Atlas check/serve responses, the bridge wire, or any iOS surface — booklets present as library content. Don't add a `source` field to any of these.
- **`booklet_tag` is column-only** — spliced at the three track-read sites like `artwork_version`, zeroed in `marshalForStorage`. `SetBookletTagAndBumpIndex` is the `SetArtworkVersionAndBumpIndex` clone: whole-album UPDATE keyed on `$.musicBrainzAlbumID` (NOT artworkMBID — a locally-curated cover doesn't preclude a booklet), strict-advance `indexed_at`, **no `enriched_at` touch**.
- **The booklet loops ride the harvest client** ([internal/atlasharvest/booklets.go](internal/atlasharvest/booklets.go)): check cycle at `SubmitInterval` cadence (attempt cap 8; a pre-booklet Atlas 404 is logged + STAMPS `LastBookletCheckAt` so it's probed once per interval — and MUST NOT trip the 401/403 credential-wipe path; transient errors don't stamp → retry next tick), GC in the same cycle (`DeleteBookletsNotIn` removes rows + cached PDFs together; **an empty universe is a deliberate NO-OP** so a transient enumeration failure can't wipe the cache), fetch sweep 3/tick with a 64 MiB `LimitedReader` overrun REFUSAL (never truncate — a truncated PDF is corrupt) + the `NudgeBookletFetch` priority channel fed by the API's 202 path.
- **`GET /v1/booklet/{mbid}` is a `streamingRoute`** — 10–64 MB over a slow DERP relay would blow the bounded 60s write deadline (same rationale as `/v1/download`); it's in the route-registry pin tests' allow-list. 200 `http.ServeContent` (Range helps PDFKit) / 202+Retry-After (known+available, download pending — also nudges the fetch queue) / 404.
- **iOS**: `BridgeSyncActor` records a per-album `BookletMarker` for EVERY row carrying a release MBID (nil-tag verdict = explicit clear — the bridge's album-wide bump puts cleared rows in the delta; nil→tagged upgrade so a merged album spanning two release MBIDs keeps whichever HAS a booklet). `BookletCache` keys `<sha256(mbid)>-<sha256(tag)>.pdf` (tag change re-keys; prefix scan drops stale siblings). The viewer parses `PDFDocument(url:)` OFF-main and rotates via an in-viewer button (all pages + one relayout) — the app stays portrait-locked on iPhone; don't add Info.plist/AppDelegate orientation changes for this.
- **Atlas side** (repo `1-bit-atlas`): `qobuzCoverProvider.EnqueueBarcode` now name-match-enqueues (the historical no-op left barcoded releases — most of a library — with no qobuz link; booklets have no Tidal equivalent). `POST /v1/atlas/harvest/booklets/check` self-heals by enqueuing name-match for UNLINKED releases only (linked-but-bookletless are never re-searched); the ingest Loop's rate buckets are the throttle.
---

## v1.8 — admin enrichment UX (PR #495, merged 2026-07-13)

Dedicated Settings → Enrichment tab + extended dashboard coverage + operator retry. Admin-local (no `/v1/*` change, no `ProtocolVersion` bump).

- **The enrichment-source picker is DERIVED, not stored.** `deriveEnrichSource(mbBase, caBase)` in [handlers_api.go](internal/admin/handlers_api.go) classifies the existing `enrich.musicbrainzBaseURL`/`coverArtBaseURL` pair into `musicbrainz` (both empty) / `atlas` (MB == `<host>/ws/2` AND CoverArt == `<host>`) / `custom` (anything else). There is deliberately NO `enrich.source` config field — the URLs stay the single source of truth (env overrides can't disagree with a stored enum), existing Atlas-pointed deploys auto-detect with zero migration, and the PATCH surface is unchanged (the picker maps to the two base URLs in `app.js mapEnrichSourceToBases`; raw fields live in the collapsed Advanced block, and hand-editing them flips the picker to Custom). Both derivation inputs are trailing-slash-trimmed defensively. **Don't add a stored source field** without solving the URL/enum divergence it reintroduces.
- **`AtlasMetaBreakdownCounts` ([atlas_meta.go](internal/manifest/atlas_meta.go)) is a full-table `json_extract` CTE — same TTL discipline as `FormatDistribution`/`EnrichmentBreakdown`:** only ever called behind `getEnrichmentMetaSnapshot`'s 60s TTL + singleflight, never on an SSE tick. Found=1 rows with EMPTY text count as MISSING (`TRIM(bio/bio_summary/description) != ''`) — the counters describe what the UI can show, not mere resolution. Empty MBID universes omit their facet (nil, `omitempty`) so a fresh library renders no "0 have · 0 missing" noise rows.
- **Artist-image coverage comes from the filesystem, not the DB**: `enrich.CachedArtistImageMBIDs(artworkDir)` (one `os.ReadDir`; strict-UUID middle segment excludes the `artist-name-<sha>` canonicals + `<mbid>-<size>` covers; lowercase keys) wired via nil-safe `admin.Deps.ArtistImageMBIDs`. Guard: `cacheDir == ""` returns empty rather than enumerating the CWD.
- **"Retry missing" (POST /api/enrichment/retry, 60s rate guard → 429)** re-queues per facet via the CORRECT mechanism: covers/artist-match via `ResetEnrichedMisses` (COALESCE-to-'' predicates so explicit-empty MBIDs count as missing), artist images via `ResetEnrichedByArtistMBIDs` (missing set computed in Go from the file scan; the MBID set travels as ONE bound JSON array consumed by `json_each(?)` — **no placeholder-concat SQL**, which both dodges go:S2077 and drops the bind-ceiling chunking), and bios/descriptions via `Deps.HarvestForceSubmit` (zeroes `harvestState.LastSubmitAt`; the next 60s tick re-submits the full library — bios are NOT in `tags_json` and NOT enricher-owned, so an `enriched_at` reset cannot retry them; an external review suggested a `$.artistBio` predicate — false premise, don't re-add). `indexed_at` is NOT bumped by the resets (no iOS delta churn); `MarkEnriched` bumps when the retry lands data.
---

## Inspector coverage + Atlas layer batch (PRs #504 / #505 / #506, merged 2026-07-16)

Stacked triple: disk-check volume fix + eligible-denominator coverage (migration **v25**) + the Inspector's Atlas metadata layer. Admin-console-only + one migration — no `/v1` wire change, no `ProtocolVersion` bump, no iOS mirror. The Library section's first tab is now labeled **"Roots"** (was "Browse"; the page manages roots + the transcoded cache — browsing lived in Inspector, and since 2026-08-24 lives in the player's Browse); the `ActiveTab` key stays `"library"`.

- **Transcode disk pre-flight grades the VARIANTS volume, resolved per-call** (PR #504). The projection endpoint and `Coordinator.Submit`/`SubmitOptimize` statted `cfg.DataDir` while sidecars are written to `cfg.Upscale.EffectiveVariantsDir(...)` — on bridge.ars.md (variants on a ~1 PB B2 mount, dataDir on a 29 GB root disk) every sizable batch was refused with "not enough free space". All check sites now grade the per-call `outputDir` (`c.dataDir` is a documented `""`-fallback only); the three cmd/bridge adapters resolve `outputDir func() string` from the LIVE config holder (nil-guarded) so hot variants-dir changes (POST /api/upscale/variants-dir) apply without restart — **don't snapshot the variants dir at adapter construction again**. `transcode.AvailableDiskSpaceNearest(dir)` walks to the closest EXISTING ancestor before statfs (the variants dir is lazily created; a bare statfs would ENOENT the pre-flight) — the walk advances **only on `os.IsNotExist`**; any other stat failure re-probes the configured dir so the genuine error (EACCES etc.) surfaces instead of silently grading an ancestor volume. `DiskHasHeadroom` routes through it, and Submit/SubmitOptimize surface its typed `*InsufficientDiskSpaceError` DIRECTLY (the "disk probe" wrap covers only genuine probe failures; the old unreachable `!ok` reconstruction is gone). Locked by `TestApiLibraryBrowseProjection_ProbesVariantsDir`, `TestSubmit_RefusesOnInsufficientDiskSpace`, `TestSubmitOptimize_DiskCheckTargetsOutputDir`, `TestSubmit_DiskCheckFallsBackToDataDir`, `TestAvailableDiskSpaceNearest_*` (incl. the POSIX EACCES pin).
- **Migration v25: format-fact columns are query accelerators ONLY** (PR #505). `tracks` gained `sample_rate` / `bits_per_sample` / `is_dsd` / `codec`, stamped by BOTH upserts (`formatColumnBinds`, nil-safe pointer handling) and backfilled once from `tags_json` (`backfillFormatColumns` — touches ONLY the four columns, **never `enriched_at`/`indexed_at`**; pinned by `TestBackfillFormatColumns`). **tags_json stays the Go readers' truth** — the columns are never spliced onto wire output and MUST NOT gain `json:` tags (same class as the `mtime_ns` gotcha). `MarkEnriched`/`applyReconciledTracks` deliberately do NOT re-stamp (they never change format fields — docblocked). The columns exist so the coverage rollups evaluate eligibility as plain-column SQL — **no `json_extract` on the browse hot path**.
- **SQLite alias-vs-column GROUP BY trap** (PR #505, caught by `TestFormatDistribution`). Adding real `codec`/`is_dsd` columns made `FormatDistribution`'s bare `GROUP BY codec, …, is_dsd` silently REBIND from the SELECT aliases to the (NULL-on-unstamped-rows) table columns — SQLite resolves bare GROUP BY names to table columns BEFORE aliases — misgrouping the composition bars. Fixed with ordinal `GROUP BY 1,2,3,4`. **When adding a tracks column, grep for `AS <name>` + GROUP/ORDER BY collisions.**
- **Eligible-denominator coverage bars** (PR #505). Tile bars + panel headers count `covered + currently-eligible` ([internal/manifest/eligibility.go](internal/manifest/eligibility.go): `EligibleCountsForFolders` — json_each single-bind path array, bind order (rate,bits,rate,bits,blob) pinned by a transposition-detecting test — and `EligibleRollupByPrefix`) with a muted "N need nothing" note and a dimmed "—" row when a kind has nothing to do; the field case (ABBA "62/136 optimized" where the other 74 are 16/44.1, natively at the CarPlay floor) now reads 62/62. The SQL predicates are LOCKSTEP MIRRORS: `optimizeEligibleSQL` ⇄ `transcode.OptimizeEligible`, `upscaleEligibleSQL` ⇄ `Coordinator.Submit`'s gate — **change the Go gate and the SQL together**; `TestEligibilitySQLAgreesWithOptimizeEligible` / `...WithUpscaleSubmitGate` (internal/admin — the only package importing both; the real transcode import in that test file is load-bearing) fail on divergence. ~~Deliberately semantics-neutral: lossy sources remain upscale-eligible~~ **SUPERSEDED by PR #507 (merged 2026-07-16): lossy sources are now gated out of upscale on EVERY surface** — `manifest.IsLossyCodec` (fail-open on empty codec, deliberately unlike the extractors' `canSetBitsPerSample` allowlist; docblocked) is the single source of truth called by `Coordinator.Submit`'s walk, `EnqueueOne`, the CLI `classifyUpscaleTrack` (which also fixed lossy-under-optimize being counted "alreadyAtTarget" → now `notPCM`), the projection walk, and the admin badge (delegation); `upscaleEligibleSQL` carries the `NOT IN` mirror and the lockstep test's Submit re-statement calls the real predicate. PROTOCOL.md already documented the gate as "PCM", so the code now matches the wire contract — no doc mirror needed. Practical exposure was the bogus-bits-tag case only (real lossy files carry no bit depth and already fell out on geometry). Wire nuance: per-folder eligible counts are `*int` — nil/omitted = degraded (JS falls back to trackCount), present 0 = genuinely "all set"; **don't flatten to plain ints**. The projection gained `alreadyAtTargetFiles` (split out of `unknownFormatFiles`) so "already done" renders as a neutral hint, not "skipped"; pre-split, upscale's at/above-target rows vanished from every bucket via the ProjectedSize<=0 fall-through.
- **Inspector Atlas layer** (PR #506, [internal/admin/handlers_library_meta.go](internal/admin/handlers_library_meta.go) + [internal/manifest/library_meta.go](internal/manifest/library_meta.go)). Tiles get artwork thumbs (album cover / artist portrait; data-driven regardless of Atlas — covers exist via CAA/local extraction) + a booklet chip; the panel gets an "About" card (bio/description with the **mandatory "Read more on <source>" attribution**, booklet view/fetch, folder-scoped retry), gated on `atlas.enabled` and fetched LAZILY on first expand per folder (the root's detail is a full-table walk — never paid for operators who wanted the upscale numbers). Load-bearing:
  - `StreamTrackMetaRefsUnderPrefix` + the scoped `Distinct*MBIDsUnderPrefix` are **json_extract subtree walks (AtlasMetaBreakdownCounts cost class)** — click-driven admin endpoints only, behind the 60s TTL + per-path singleflight (`libMetaCache`; recompute under `context.WithoutCancel`, PR #373 shared-result precedent), NEVER on an SSE tick. Child grouping + representative-cover voting happen in Go over the streamed scan (no SUBSTR/INSTR GROUP BY). Both JSON endpoints send `Cache-Control: no-store` (a browser disk-cache hit after a retry would resurrect a stale "missing" state).
  - **`ResetEnrichedMissesUnderPrefix` joins the sanctioned `enriched_at` writers** (enricher + the PR #495 retry pair): prefix-scoped, enriched-but-incomplete rows only, never bumps `indexed_at`, reachable only from the rate-guarded `POST /api/library/enrichment/retry`. That retry guard is **60s PER normalized folder path** (distinct folders queue back-to-back; upstream protection is the enrich/harvest clients' own pacing) — the harvest facet keeps its own GLOBAL 60s gate (library-wide by construction). `ResetBookletChecks` re-arms only `available = 0` rows (the attempts<8 check gate).
  - **Loopback byte routes** `/api/library/{artwork,artist-image,booklet}/{mbid}` serve the same cache dirs as the bearer-authed `/v1` twins (admin can't call `/v1`; `boundaryMiddleware` is the gate — no added auth, per the admin posture). Ids MUST pass the twins' bounded-alphabet regexes (`adminMBIDPattern`/`adminArtworkMBIDPattern`, lockstep with api/artwork.go) before any path join. Caching: `immutable` only for content-keyed responses (`v=` from `artwork_version`, or `local-<sha>` — the hash IS the content key); bare-UUID covers 1 day (premium refetch can change them); artist images deliberately 1 day + mtime revalidation (enricher-fetched from Deezer, no version column — **don't add one for this**).
  - About-card DOM renders via **createElement/textContent ONLY** (bios/descriptions/labels/genres are third-party strings; attribution `href` admitted only for parseable http(s) URLs). Retry UX is an **optimistic in-card status with NO auto-refetch** (enrichment runs at MB/CAA/Deezer pacing; an immediate refetch loses the race and reads as failure — the card refreshes on next open).
  - Admin Deps gained nil-safe `ArtworkPath`/`ArtistImagePath`/`BookletPath`/`BookletNudge` closures (admin still imports neither internal/enrich nor internal/api — ArtistImageMBIDs precedent); the booklet closures gate on the harvest client, mirroring the `/v1/health` `booklets` flag.
  - **Phone tile header WRAPS** (name on a full-width second row): at 375px the fixed 44px controls + 44px thumb crushed the flex-shrunk name to one character per line — **don't "simplify" the wrap away**.
---

## Comprehensive-audit fix batch (PRs #511–#535, merged + deployed 2026-07-19)

A full-codebase audit (`ops/audit-2026-07-18.md` — 53 bugs · 53 quick wins · 3 refactors, every finding re-traced against source) landed as 25 PRs. Bridge-only: **no `ProtocolVersion` bump, no `/v1/*` shape change, no iOS mirror**. Deployed to bridge.ars.md (`v0.1.7-96`); home-pc pending. Invariants worth not re-breaking:

- **All FIVE post-scan reconciliation passes exclude UPnP-routed rows, from ONE routed set** ([scanner.go](internal/manifest/scanner.go), #511). Four of the five didn't (only `runTrackNumberReconciliation` had the guard) — and since `walkFieldsEqual` diffs exactly AlbumArtist/Album/Year, a disagreeing routed album flip-flopped every fs-scan ↔ UPnP-walk cycle: perpetual re-enrich treadmill + iOS delta churn (the PR #369/#370 wipe-loop class, re-opened via the reconciliation vector). The set is computed ONCE at the reconciliation head and threaded into all five — fail-closed (a fetch error skips ALL passes; an already-done ctx skips them at Info, not Error). **Don't reintroduce a per-pass `routedExclusionSet` call, don't drop the exclusion from any pass**, and **don't add a `len(routedSet)>0` guard before the map lookup** — Go's map access fast-paths `count==0` before hashing the key, so it saves nothing and costs a branch on hybrid libraries (declined on review).
- **`go upd.Run(scanCtx)` must stay started, and `AutoInstallRestart` must NOT `os.Exit`** ([main.go](cmd/bridge/main.go), #512). The updater was fully wired but never launched — background polling AND the whole auto-install path were silently dead, with only the manual "Check now" button working. The restart callback now routes through the same cancellation closure as SIGINT / admin-restart (the documented "graceful shutdown triggers full cleanup" invariant, on the updater path).
- **`upnpproxy` sets `CheckRedirect: ErrUseLastResponse`** ([proxy.go](internal/upnpproxy/proxy.go), #513). Without it the stdlib followed up to 10 redirects, so a rogue/spoofed LAN upstream answering a `<res>` fetch with `302 → 127.0.0.1:7789` made the bridge fetch its own no-auth loopback admin API and relay the bytes back — an SSRF + info-disclosure primitive reachable **unauthenticated** via `/dlna/file/{id}`. Relaying 3xx verbatim is also the package's bit-exact contract. Mirrors `enrich/deezer.go`'s `installRedirectGuard`.
- **`fsutil.IsUnderAny` folds case via an EMPIRICAL filesystem probe, not `runtime.GOOS`** ([symlink.go](internal/fsutil/symlink.go), #513). `EvalSymlinks` doesn't canonicalise case, so a variants dir `…/music/variants` against root `…/Music` reported not-under → sidecars written INSIDE the (possibly read-only) library root, the PR #475 phantom-rows class. `caseInsensitiveFS` walks to the nearest existing ancestor and compares `os.SameFile` against a case-swapped sibling — which a GOOS check gets wrong in BOTH directions (Linux libraries on case-insensitive FAT/exFAT/NTFS mounts, common on Pi/SBC; and case-sensitive macOS/Windows volumes).
- **`AllowAndReserve` callers MUST NOT also call `RecordFailure`** ([ratelimit.go](internal/adminauth/ratelimit.go) + [handlers_login.go](internal/admin/handlers_login.go), #531/#533). The login gate was check-then-act across two lock acquisitions, so N concurrent `POST /login` all observed `attempts < max` before any recorded — the ceiling was exceeded by the in-flight concurrency, with bcrypt (~250 ms) the only real backstop. `AllowAndReserve` folds check + reservation under one lock; **the reservation IS the failure count**, so the fail path only logs and `RecordSuccess` clears the bucket on a good login. Re-adding `RecordFailure` double-counts.
- **Shutdown joins: the watcher's fired `AfterFunc` scans and every background manifest writer are waited on before `Store.Close`** ([watcher.go](internal/manifest/watcher.go) + [main.go](cmd/bridge/main.go), #534). The watcher's debounced `ScanSubtree` dispatches were tracked by nothing — a TOCTOU past the callback's ctx check could run mid-`UpsertTrackBatch` while `Store.Close` executed (the SQLite-corruption class the `bgScans` WaitGroup guards elsewhere, which never covered this layer). They're now counted in a `scanWG` with a `closing` flag, **both mutated under `wt.mu`** so a callback firing during shutdown is deterministically either counted-and-waited or skipped. Separately a `bgWriters` WaitGroup joins scanner / enricher / harvester / admin / analysis-sweeper / smart-playlist-regenerator, its wait registered as a defer immediately AFTER the `Store.Close` defer (LIFO → the wait runs first) and **grace-bounded** — a wedged writer degrades to the pre-fix behaviour plus a log line, never a hung exit. **Don't make either wait unbounded.** **The wait MUST be written INLINE in that defer, never routed through a function variable assigned later in `runServe`** (#536): the first tracked goroutine starts ~1200 lines before the end, so any early return in between (a Tailscale/config error, a failed listen) would leave the variable nil, make the defer a no-op, and let a live writer race `Store.Close()` — reintroducing the very corruption this guards. `WaitGroup.Wait` on a zero counter returns immediately, so an exit before any writer starts costs nothing. Use `time.NewTimer` + `defer Stop` for the grace, not `time.After` (PR #290 convention — `runServe` is re-entered from the launcher menu, so abandoned timers accumulate).
- **Playlist-cover filenames are injective `sha256(scope + "\x00" + key)`** ([playlist_covers.go](internal/manifest/playlist_covers.go), #532). The prior lossy sanitizer mapped ids `"a b"` and `"a_b"` onto one file, so uploading a cover for one silently overwrote the other's while the DB advertised the correct `imageHash` (cache-busting couldn't recover). **Deploy note: the scheme change orphans pre-existing cover files — they need re-upload** (rows keep the hash; covers are re-uploadable).
- **Prefix rollups / counts use a byte-range, never `LIKE`** ([store.go](internal/manifest/store.go), #532). `path LIKE 'p/%'` can't use the BINARY-collated index under SQLite's default `case_sensitive_like=OFF` → full table scans, on an admin Inspector path that is NOT TTL-cached. Use `path >= p||'/' AND path < p||'0'` (the `childFolderRollupSelect` / `EligibleRollupByPrefix` idiom) and collapse the two `track_variants` scans into one CASE-WHEN. **Always `strings.TrimSuffix(prefix, "/")` before binding** (#536): the range appends its own `/`, so a caller passing `"Album/"` builds `path >= 'Album//'` and matches NOTHING — a silently-empty rollup (the Inspector's numbers read zero) rather than an error.
- **The binary swap keeps `dst` present throughout, and falls back to copy on EXDEV** ([swap_unix.go](internal/updater/swap_unix.go), #522). The old "atomic" swap was two renames with NO file at the running-binary path between them — a power loss there is permanently unbootable and `maybeRollbackOnBoot` can't help (the missing file IS the bridge). Now `Remove(bak)` → `Link(dst,bak)` → `Rename(new,dst)`, with the old two-rename path kept as the link-less (EXDEV/FUSE) fallback, plus `placeNewBinary`/`copyAndRename` for the genuinely cross-filesystem case (`<DataDir>/updates` and `/usr/local/bin` are commonly different mounts). Windows can't use the hardlink trick at all (a running `.exe` can't be replaced in place) — documented, not papered over. **On Windows a stop-timeout must best-effort `s.Start()`**: the old path sent Stop, timed out, returned a nil handle so the deferred restart never ran, and left the service stopped — bridge offline until a manual `sc start`.
- **Config cadence ceilings are UNIT-APPROPRIATE, and the port range admits 0** ([config.go](internal/config/config.go), #529). `time.Duration(n)*time.Second` overflows to a NEGATIVE duration past ~9.2e9 and `time.NewTicker` PANICS on a non-positive interval → `bridge serve` crashes at startup; `Validate` now caps `*Sec` fields at 31536000 and `*Hours` at 8760 (a single seconds-cap applied to an hours field still overflows). Port validation requires `0 <= p <= 65535` — **not `1..`**: port `0` is the documented OS-picks-an-ephemeral-port mode, used by the `:0` test fixtures across api/admin/dlna and matching admin's own `port < 0 || port > 65535` check.
- **`/v1/health`'s UPnP upstream counts are TTL-cached and serve-stale-on-error** ([public_servers_cache.go](internal/api/public_servers_cache.go), #525). `PublicServers` was the one DB-backed field on the UNAUTHENTICATED handler without a cache, driving a per-server `COUNT(*)` over ~15k routed rows on every request — the exact flood cost `healthCountsCache` / `reachabilityCache` exist to bound. Same shape (TTL + singleflight + detached fetch); a failed refresh keeps the last-good snapshot rather than publishing an empty one.
- **The SSDP M-SEARCH listener reuses `discovery.HandleReadErr`** ([ssdp.go](internal/dlna/ssdp.go), #521). A read deadline alone makes ctx-cancel responsive but hot-spins at 100% CPU on a persistent non-timeout error (interface down); the shared PR #469 policy — timeout → reset streak + continue, `net.ErrClosed`/ctx → exit, else log + ctx-aware backoff with one escalation — is the intended third caller. **Don't re-inline the policy per loop.**
- **`resolveReleaseGroupMBID` negative-caches on `!IsTransient(err) && ctx.Err() == nil`** ([enricher.go](internal/enrich/enricher.go), #520). Caching only 404s left persistent decode / 4xx errors re-queried once per sibling track at MB's 1.1 s pacing; the `ctx.Err()` half is load-bearing because `IsTransient` classifies `context.Canceled` as NON-transient, so a clean shutdown would otherwise poison the cache with an empty resolution.
- **`doctor_windows.go` calls `syscall.Syscall6(proc.Addr(), …)`, not `proc.Call(…)`** ([doctor_windows.go](internal/doctor/doctor_windows.go), #526). `LazyProc.Call` is NOT `//go:uintptrescapes`, so Go's unsafe.Pointer rule-4 liveness special-case applies to none of its arguments — and a `runtime.KeepAlive` cannot cure a `uintptr` that was stored in a local first. `Syscall6` IS annotated, so inline `uintptr(unsafe.Pointer(…))` arguments are pinned across the call. Validate with `GOOS=windows go vet` — the `unsafeptr` analyzer is the real check, and the file doesn't compile on a non-Windows host.
- **Booklet GC is skipped while a library scan is in flight** (`ScanInProgress: scanner.IsScanning`, #527/#533). Mid-rescan the album-release-MBID universe is transiently partial (an admin root add/remove runs `WipeFilesystemTracks` + rescan), so GCing against it deletes the booklet rows + cached PDFs for every filesystem album and re-fetches them next cycle. The nil hook = GC runs (legacy behaviour), so the wiring is what activates it.
- **Pairing `Delete` logs an orphan token ONLY when it was never delivered** ([store.go](internal/pairing/store.go), #523/#533). A `delivered` flag set inside `Poll` where it returns the RawToken (additive — it does not change what Poll returns) gates the log, so the NORMAL ack flow (poll → persist → DELETE) stays silent and only a delete-without-ever-polling leaves a breadcrumb. **It must NOT revoke** — `onTimer`'s TTL+grace sweep stays the only sanctioned revoke path (pinned by `TestDeleteAfterApprovePreventsRevoke`).
- **`backup.ReapOrphans` refuses an empty root** (#536): it DELETES subdirectories that lack a `manifest.json`, and `os.ReadDir("")` reads the process working directory — a misconfigured/empty `backupsRoot` would reap unrelated directories next to wherever the bridge runs. Any future directory-reaping helper needs the same fail-closed guard.
- Smaller pins: `buildDailyMix` must not emit a visible-but-empty family (#523); `SoxArgs` returns the tmp-path it computes so `RunSox` can't independently rebuild a drifting one (#535); the "All Tracks" childCount uses the raw `lib.TrackCount()` the flat list actually enumerates (#535); systemd `ExecStart` needs `$`→`$$` while path settings must NOT get it (env expansion vs specifier expansion), and the Windows batch template needs `%`→`%%` while `SpawnDetached`'s argv must not double (#526).

**Deliberately NOT done** (tracked in the audit doc): **B25** — renderer controlURL refresh; the server-side fix can't be copied verbatim because a failed re-fetch upserts a stub whose merge advances `LastSeenAt` while keeping the dead URL, pinning it forever, so it needs `Remove(udn)`-first or a stub-merge gate. **Q6** — combining the two `ffprobe` spawns would touch decode.go's load-bearing length-complete-decode gate for a one-fork saving. Refactors **L1** (five full-library reconciliation streams per scan → one), **L2** (`Validate()` mutates its receiver — split out `Normalize()`), **L3** (`atomicwrite` parent-dir fsync for crash-durability).
---

## Atlas-pointed enrichment — pacing, recall, reclaim (PRs #593 / #595 + atlas#132, 2026-07-29)

Diagnosed live against `bridge.ars.md` (`enrich.musicbrainzBaseURL: https://atlas.ars.md/ws/2`). The bridge was resolving **50.0 %** of albums where public MusicBrainz resolved **68.3 %** on the identical `release:"<album>" AND artist:"<artist>"` queries, and **8,945 of 19,482 tracks (45.9 %) carried no release MBID** — no Atlas description, label, genres, booklet or premium cover, all of which key on it. Post-fix the same 150-album probe measures **69.3 %**.

- **The enricher's MB/CAA pacing DERIVES from the client's base URL, and must stay that way** ([pacing.go](internal/enrich/pacing.go)). `MBMinInterval` / `CAAMinInterval` used to be hardcoded in `NewEnricher` and never conditioned on where the client pointed — `MusicBrainzClient.base` is unexported with no accessor, so the pacing code physically could not see its own target. An Atlas-pointed bridge therefore slept 1.1 s before every search **against its own server**: ~60 % of the enricher's wall clock. `MinInterval()` now travels with the base URL, so the CLAUDE.md politeness invariant ("MB anon is 1 req/s") is enforced **by construction** — it is a contract with two specific hosts, not with a code path. `minIntervalForBase` **FAILS SAFE** (unparseable/host-less → the public interval) and its suffix match is **dot-anchored** so `notmusicbrainz.org` and `musicbrainz.org.example.com` stay third-party. **`SelfHostedMinInterval` is 150 ms, NOT zero** — Atlas's own `PublicTierGate` throttles `/ws/2/`, `/release/`, `/release-group/`, `/artist/` at 600 req/min/IP and the enricher SHARES that per-IP bucket with the harvest client's cover sweep. **Don't reintroduce a fixed interval in `NewEnricher`, and don't drop the floor to zero.**
- **`Enricher.Run` needs its own inter-batch sleep — the pacer is not a brake.** The MB pacer only fires *when a network call is made*, so a batch whose rows all hit the LRU caches (or bail on an early check) completes in milliseconds and immediately re-polls the store; a batch failing WITHOUT stamping `enriched_at` (the transient path) would spin the DB as fast as the loop turns. At the old 1.1 s that was invisible; at 150 ms it is not. The 50 ms `interBatchPause` is unconditional **on purpose** — don't gate it on whether an API call happened.
- **A release-search miss must NOT cost the track its artist resolution** ([enricher.go](internal/enrich/enricher.go)). `enrichOne` returned straight to `markSkipped` when `albumMBID == ""`, so `resolveArtist` never ran — at a 50 % album hit rate that was roughly half the library losing its artist MBID and portrait for a reason unrelated to the artist. The two halves are independent and the artist search is the cheap, reliable one (30–190 ms). **`enriched_at` is still stamped on a no-match** — that is what keeps the `WHERE enriched_at = 0` worker from re-querying unresolvable albums every poll cycle forever; recovery is `ResetEnrichedMisses`, not an unstamped row.
- **`ResetEnrichedMisses` tests THREE arms — artwork, artist, AND release MBID** ([store.go](internal/manifest/store.go) `enrichmentMissPredicateSQL`). `artworkMBID` is not a proxy for the release MBID: it also carries the scanner's `local-<sha256>` sentinel for embedded APIC / `folder.jpg` art, so a track whose album never resolved but which HAS local art read as "not missing". Measured: of the 8,945 rows with no release MBID, **6,801 — every one via a `local-` sentinel — were invisible to the old two-arm predicate**. The operator pressed "Retry missing", the handler reported success, and 76 % of affected rows silently stayed put — worst on exactly the well-tagged libraries the feature is for. The predicate is written **verbatim** into `resetEnrichedMissesSQL` and `resetEnrichedMissesUnderPrefixSQL` rather than concatenated (a `const stmt = "…" + predicate` is compile-time-folded and equally safe, but trips SonarCloud `go:S2077` and reads as an assembled query); `TestEnrichmentMissPredicateIsShared` is what stops the two copies drifting. **Don't drop the release-MBID arm, and don't let the two statements diverge.**
- **Atlas side (`1-bit-atlas` PR #132) — `releaseTrigramSQL`'s fence MUST stay artist-aware.** The trigram plan is what every 3+ char album title takes, so it is what this enricher actually hits, and it had none of the protections its short-term siblings got in atlas#131: an artist-blind `ORDER BY rg_sim DESC LIMIT 200` fence (hundreds of groups tie at 1.0 for a generic title, so the wanted one often never entered the CTE), a `LEAST(…, 100)` clamp that annihilated the +10 artist bonus for exact-title matches (Jesse Cook's *Vertigo* ranked **91st**; the bridge asks for `limit=10`), and no deterministic tiebreak (*Greatest Hits* / Fleetwood Mac was absent at `limit=10` but rank 1 at `limit=25`). `artist_exact` leads `rg_exact` there **deliberately** — `rg_exact` is a raw `lower()` equality while pg_trgm strips punctuation, so a U+2019-vs-ASCII apostrophe scores similarity 1.0 but `rg_exact` false; 77,887 release groups carry a curly apostrophe. **This is the cross-repo half of the same bug — if album recall regresses, check that SQL before suspecting the bridge.**
- ~~**Residual, deliberately deferred**: Atlas matches only `release_group.name` while MusicBrainz matches `release.name`… `release_name_trgm` is valid but never queried.~~ **BOTH HALVES OF THIS ARE NOW STALE (corrected 2026-07-30) — don't act on it.** Atlas side: atlas#133 added `releaseNameTrigramSQL`, which DOES query `release_name_trgm` as a gated fallback (fires only when `artistFilter != ""` and no candidate is both artist-exact and ≥80), and atlas#134 changed the scoring to `GREATEST(rg_name_sim, similarity(r.name, $1))`. Bridge side: `pickBestRelease` now accepts on the release-group title as a second arm (PR #601), which is the other half — Atlas returns `release.name` as `title` while having matched on `rg.name`, and the bridge was rejecting candidates on a title its upstream never claimed to match. The Halcyon-class case is addressed from both ends; re-measure before assuming it isn't.
---

## Acoustic fingerprinting fallback (PRs #604 / #605 / #607 / #608, 2026-07-30)

The floor the folding work left behind — tracks whose tags no text match can fix
(album "CD 01", albumArtist "An Unknown Artist") — is reached by identifying the
recording from the AUDIO. `fpcalc` (Chromaprint) fingerprints, AcoustID
identifies, and the EXISTING text acceptance decides. Off by default; needs both
fpcalc and a free AcoustID application key, and degrades to off with a stderr
line if either is missing (`bridge doctor` → `fingerprint-toolchain`).

**WRITE-TARGET DISCIPLINE — the load-bearing rule.** A fingerprint identifies
AUDIO. AcoustID maps audio to a RECORDING. It does NOT identify a release, and
cannot: one recording sits under many release groups *precisely because those
releases contain the same audio*. So `acoustid.Decision` has **nowhere to put a
release or artwork MBID**, and `TestDecisionCannotCarryAReleaseMBID` asserts its
exact field set — adding a field fails that test deliberately. An album MBID is
still reached, but only by running the existing ladder with the recovered artist
NAME and letting `pickBestRelease` decide unchanged. **Never write
`MusicBrainzAlbumID` or `ArtworkMBID` straight from a fingerprint.** This is what
bounds a wrong answer to a wrong portrait and bio rather than a wrong album
identity, cover, booklet and folder grouping.

**`sources` is PER RECORDING, not per result** — verified against a live
response, whose result objects carry only `{id, score, recordings}`. Reading it
off the result yields 0 for every track and the reliability clause then refuses
the ENTIRE library. No fixture can catch this (a hand-written one reproduces
whatever shape its author believed); only a live call settles it. It is also
strictly more useful per-recording: it discriminates *within* a cluster, so a
lone mis-tagged submission is dropped while its well-attested sibling survives.

**Ambiguity means the tied clusters would yield a DIFFERENT ANSWER, not that two
clusters exist.** AcoustID carries unmerged duplicate clusters, so most near-ties
are one answer stored twice (ABBA 0.978 vs ABBA 0.974). The original margin
clause rejected 16 of 60 sampled tracks while preventing nothing — of those 16,
14 agreed on artist and the other 2 were already refused by the duration and
consensus clauses; cross-cluster disagreement occurred **zero** times. Every
competing cluster is still put through the SAME duration and sources filtering
before its opinion counts, disagreement still refuses, and **a tie still costs
the recording MBID and the album cue** (which cluster to draw those from is
undetermined). That suppression is what makes accepting a tie conservative.
Don't drop it.

**The entropy floor is measured, not assumed.** Distinct base64 characters in the
compressed fingerprint separate degenerate audio from real by ~5x with nothing in
between — 45s silence 13, pure tone 13, a stationary 4-note chord 14, 35s melody
64, pink noise 63 — stable across durations, so the threshold is 32. It uses ONE
fpcalc spawn: `-raw` and the compressed form are mutually exclusive output modes
and AcoustID needs the compressed one, so the obvious sub-fingerprint metric
would mean decoding every file twice. **A rich but STATIONARY chord is as
degenerate as silence** (Chromaprint keys on spectral change over time), so a
hand-made "music-like" fixture is not evidence here.

**Truncation is caught by the EXIT STATUS, not the duration comparison.** fpcalc
exits non-zero on a truncated stream while still emitting a usable fingerprint,
and for FLAC it reports the STREAMINFO duration — so a truncated FLAC still
claims its full length and a duration comparison sees nothing wrong. The
decode-agreement clause is a backstop, documented as such.

**The local-artist veto lives in `internal/enrich`, not `internal/acoustid`.**
It needs the match-folding vocabulary, and `internal/enrich` consumes the
acoustid package, so importing it back would be a cycle. It is the ONLY check in
the whole path using information the fingerprint pipeline did not produce —
everything inside the gate is AcoustID grading its own homework — and it only
ever SUBTRACTS. The junk-tag list that disables it is **closed, tiny and
fold-exact**: an over-eager classifier removes the last witness on exactly the
tracks where a wrong answer is hardest to notice. Two subtleties worth keeping:
`!!!` is a real band that folds to nothing and therefore cannot be a witness in
either direction (correct, not a misclassification — the consequence is a higher
sources bar); and **an all-digits ALBUM title is NOT junk though an all-digits
artist is** ("1", "4", "21", "1989", "90125"), because misclassifying an artist
only removes a witness while misclassifying an album SUBSTITUTES the
fingerprint's title for the operator's own.

**The sweeper is a separate goroutine, never a step inside `enrichOne`.** The
enricher is rate-limited, not CPU-bound, and has no filesystem dependency —
`os.Stat` takes no context, so a hung (not dropped) network mount would block the
single goroutine driving all enrichment. Its worker join is **bounded**
(`sweeperDrainGrace`): `exec.CommandContext` sends SIGKILL on ctx expiry, but a
process in a FUSE syscall sits in uninterruptible sleep and will not take it, so
an unbounded `wg.Wait()` would turn a wedged mount into a hung shutdown. **Every
`ResolveChecked` error is treated identically** — ENOENT and ENOTCONN alike;
distinguishing them is the seed of a "mark permanently unfingerprintable" bug
during a mount outage (the PR #74 class). Nothing is persisted on failure, so an
unreadable file costs exactly today's behaviour.

**`Store.ResetEnrichedByPaths` is a NEW sanctioned `enriched_at` writer** —
narrowest of them, taking an explicit path set rather than a predicate. **Do NOT
have the sweeper call `ResetEnrichedMisses` instead**: that selects ~46% of the
library and `MarkEnriched` strictly advances `indexed_at`, so every sweep would
push a ~9,000-track delta to every paired device — the PR #369 wipe-loop class on
a timer. **Cache writes must complete BEFORE the re-queue**, or a row can be
picked up on `enriched_at=0` before its verdict exists and be re-skipped.

**`tracks.acoustid_match` (migration v28) is column-only** and must never gain a
`json:` tag or be spliced onto wire output — same rule as the v25 format-fact
columns, and what keeps this whole feature off the protocol (no `ProtocolVersion`
bump, no PROTOCOL.md change, no iOS mirror). It exists because a fingerprint
match carries a residual error rate text matching does not: without provenance an
MBID written from audio is indistinguishable from one written from tags forever,
so there is no way to audit or selectively undo the feature.

**The candidate pool excludes rows the fingerprint has ALREADY answered — keyed on
matched AND holding an artist MBID, never on `acoustid_match` alone** (PR #700). The
dedup cache is in-memory and `collectCandidates` excluded only rows holding BOTH
MBIDs, so a row whose verdict landed but whose release the text ladder still cannot
find stayed a candidate forever: on bridge.ars.md **1,284 of the 1,456 matched rows**,
re-fingerprinted at EVERY restart (~500 whole-object B2 reads through the rclone VFS
mount, ~400 lookups) for an answer that cannot change — the write-target discipline
above means a fingerprint can never supply the missing release MBID — then re-stamped
via `ResetEnrichedByPaths` + `MarkEnriched` into a ~400-track no-op delta for every
paired device. **The second half of the key is load-bearing**: 136 matched rows carry
no artist MBID, because provenance records ACCEPTANCE, not application (see
`SetAcoustIDMatch`) — the apply-time local-artist veto can refuse a verdict, and a
restart between the re-queue and the enrichment loses it. Those are exactly the rows a
later sweep can still advance, so a skip keyed on membership alone would strand them
permanently, while one keyed on `ArtistMBID` alone would drop text-resolved artists
that never had a match at all. `Store.AcoustIDMatchedPaths` fetches the set once per
sweep (the `routedPathSet` shape) because the column never reaches the streamed
`Track`; **it FAILS the sweep on error rather than degrading to an empty set** —
unlike `routedPathSet`, whose empty degradation is merely permissive, an empty set
here re-fingerprints the whole matched head, which is the cost it exists to avoid, and
a skipped background pass costs nothing because the next tick retries. **Residual,
accepted:** the converse is approximate in the SAFE direction — an artist MBID may be
TEXT-derived on a row whose fingerprint verdict was then vetoed, which this reads as
settled. Re-sweeping such a row is deterministic (same file → same fingerprint → same
decision → same veto against the same tags), so the skip costs nothing until the FILE
itself changes; the column deliberately does not record which MBID came from audio, so
no exact test exists, and clearing `acoustid_match` (the undo path the column exists
for) is what re-opens it. Locked by `TestCollectCandidatesSkipsMatchedAndConsumedRows`
(three-row discriminator: consumed / vetoed / text-resolved),
negative-control-verified against the pre-fix collector.

**A no-match is PERSISTED, version-gated, with a 30-day TTL — and this
deliberately overrides `acoustid.Cache`'s recorded rejection of the idea**
(PR #701, migration v37). That docblock still says a persistent marker "fights
the operator's Retry missing button" and that "AcoustID's database grows, so a
six-month-old no-match is worth re-checking" — both true, and both now
ANSWERED rather than ignored; read this entry before treating the docblock as
current. What reopened it was production: the cache is per-process, so on
bridge.ars.md every restart re-decoded ~500 candidates, and because a no-match
wrote nothing the same unanswerable files were bought again forever. **The
saving is not the AcoustID call, it is the whole-object read** through the
rclone/B2 VFS mount. `tracks.acoustid_nomatch_{at,size,mtime_ns}` are
column-only (v28/v25 rule — no `json:` tags, never wire-spliced, so no
`ProtocolVersion` bump and no iOS mirror). **The size+mtime pair is what keeps
suppression from becoming permanent**: it pins the file version the verdict
describes, so a re-encode or tag edit makes the scanner rewrite `tags_json`,
the pair stops matching, and the row re-enters the pool — self-healing with no
upsert-path change and no backfill (all-zero = "never asked", so existing rows
need none). `fingerprintNoMatchTTL` (30 days) is the answer to the
growing-database objection; don't remove it to "save more reads". **ONLY
`ErrNoMatch` persists** — a lookup ERROR is a fact about the upstream, and the
gate rejections additionally depend on the row's own artist tag
(`HasLocalArtistWitness`), so a tag fix, exactly the operator's natural remedy,
must re-open them; persisting either would sideline files for reasons unrelated
to their audio. **Retry clears BOTH layers or it clears nothing that matters**:
the sweeper reads the in-process cache FIRST, so clearing only the SQLite rows
leaves everything answered this session suppressed until a restart — the button
would look like it worked for exactly the files the operator just watched fail.
`Server.clearFingerprintNoMatch` is the single place that does both;
`Cache.Forget(prefix)` is boundary-anchored (so "Album" can't reach
"AlbumOther") and sweeps BOTH generations, since a survivor demoted into `prev`
keeps answering `Get`. The folder-scoped retry forgets only its own prefix —
dropping the whole cache would force unrelated folders to re-decode, defeating
the point. The prefix-scoped SQL clear uses the BYTE RANGE, not `LIKE`
(path-keyed write; the `LIKE` form measurably clears the case twin), and an
unscoped prefix MUST keep delegating library-wide — without that the global
retry silently affects 0 rows while reporting success. Locked by
`TestCollectCandidatesSkipsPersistedNoMatchUnlessFileChanged` (settled /
re-encoded / retagged / never-asked),
`TestAcoustIDNoMatchRecordsVersionAndRespectsTTL`,
`TestClearAcoustIDNoMatchesUnderPrefixIsByteRanged` and
`TestCacheForgetScopesToPrefixAndSweepsBothGenerations` — every one
negative-control-verified, including the both-generations case, which at
capacity 2 drops ZERO under a current-generation-only sweep.

**The apply-time TAG VETO is persisted too — narrower than the no-match, and
pinned to BOTH of its inputs** (PR #703, migration v38). The candidate pool
deliberately keeps matched-but-artistless rows (see PR #700 above) because that
shape covers two populations: verdicts VETOED at apply time, and verdicts merely
LOST to a restart between the re-queue and the enrichment. Only the second is
advanceable, and without a marker they are indistinguishable — so on
bridge.ars.md all 136 were re-decoded, re-looked-up, re-accepted, re-queued and
re-stamped on every restart (11 of them in one folder, every one logging
`tagged="JJ Cale" fingerprinted="J.J. Cale"`), for a refusal that is a pure
function of inputs neither of which changed. `tracks.acoustid_veto_*` records it;
a lost verdict has provenance and NO marker, which is the exact discriminator.
**Only the tag-contradiction branch persists** — `applyAcousticFallback`'s other
`acousticRefused` exit, a non-UUID artist MBID, is a fact about the UPSTREAM's
data quality (nothing about the row can fix it, and suppressing would silence
the Warn that is the only signal), the same distinction that keeps lookup errors
out of the v37 marker. **The stored ARTIST is what makes the gate exact rather
than probable**: the veto is a pure function of (fingerprint answer, artist tag),
and while `Track.Artist` is written only by the extractors and `fillFromPath` —
both scan-time, so an edit or a move already moves size+mtime or lands a fresh
row — `reExtractUnchanged` rewrites `tags_json` for version-stale rows whose
BYTES never moved, so an `ExtractorVersion` bump that alters artist parsing would
otherwise leave the marker outliving the tag it judged. Comparing both inputs
needs no maintenance contract. The **CLEAR is one statement covering both marker
kinds** (`ClearAcoustIDSuppression{,UnderPrefix}`, renamed from the v37
no-match-only pair) because a retry that cleared one and not the other makes
"Retry missing" silently do nothing for half the population — that is the failure
worth making structural; the two READERS stay separate methods, since forgetting
to read one merely costs a decode. Same 30-day TTL as the no-match, as a
SEPARATE const (`fingerprintTagVetoTTL`) — the number agrees, the reason does not
(AcoustID's database grows vs. the resolved cluster gaining recordings). **Spelled
as its own literal, NOT `= fingerprintNoMatchTTL`** — it was written that way until
2026-08-16, which made the separation nominal: retuning the no-match would have
silently dragged the veto with it, the exact coupling the separate name exists to
prevent. Don't "simplify" it back to an alias;
`TestFingerprintSuppressionTTLsAreIndependent` fails BOTH cases (rather than one)
when they are aliased, which is how the coupling announces itself. Locked by
`TestCollectCandidatesSkipsPersistedTagVetoUnlessInputsChanged` (vetoed / lost /
re-encoded / re-tagged), `TestApplyAcousticFallbackPersistsOnlyTheTagVeto`,
`TestAcoustIDTagVetoRecordsBothInputsAndRespectsTTL` and
`TestClearAcoustIDSuppressionUnderPrefixIsByteRanged` (each seeded row carries a
different marker kind, so a one-sided statement fails) — all nine assertions
negative-control-verified against the un-fixed code.

**Measured on home-pc** (18,429 tracks, 60 sampled from the 7,375
release-missing FLACs): 50 accepted (83.3%), ~155ms decode and ~75ms lookup per
track on local disk. FLAC-only in practice, because the gate needs a
container-derived duration and `Track.Duration` is set only by `extractFLAC` and
`extractDSF` (DSD excluded separately) — extending the other extractors widens it
automatically. `bridge fingerprint <file>` is the diagnostic; it writes nothing.
---

## Enrichment matching — fold before comparing; relax the query, not the acceptance (PRs #600 / #601 / #602, 2026-07-30)

Follow-on to the pacing/recall work above. After #593/#595/#599 the dashboard still read **5,435 of 19,482 tracks** short of a cover, artist MBID or release MBID, and the working assumption was that the library had simply reached the limits of text matching. It had not: MusicBrainz was returning the right answer and the bridge was discarding it in two comparison functions.

**The measured shape of the gap** (live DB): 3,557 tracks lack a release MBID (281 of 1,446 albums), 3,061 lack an artist MBID (820 of 1,753 artists), only 471 lack artwork entirely (18,435 carry a `local-` sentinel). Three routes were RULED OUT BY MEASUREMENT, so don't re-chase them: the files carry no exact identifiers (40 sampled unmatched albums → 0 MusicBrainz IDs, 1 ISRC, 1 UPC — they are Tidal/Qobuz downloads with sparse Vorbis comments); the `[27597634]` Tidal album IDs in the paths are only **5 of 130** present in Atlas's `atlas_tidal_album`; and **363 tracks have genuinely junk tags** ("CD 01", "An Unknown Artist") that no text match can ever fix — that is the floor, and only acoustic fingerprinting can move it.

- **GOVERNING PRINCIPLE: relaxations belong in the QUERY, strictness belongs in the ACCEPTANCE.** Every dangerous idea in this area is dangerous because it sits in the wrong bucket. Stripping a leading "The" as a *fold rule* silently accepts any `The X`/`X` pair anywhere in a candidate list; as a *query rung* it issues a fresh request and still demands the result fold-equal. Both ladders' rungs must survive `pickBestRelease`/`pickBestArtist` unchanged — a rung cannot lower the bar, only ask a different question.

- **The album containment was ALWAYS symmetric; the defect was byte-literalness.** `caseInsensitiveContains` did `Contains(a,b) || Contains(b,a)` since it was written (renamed in #458, semantics unchanged). Every real failure was Unicode/punctuation: `What's Up?` vs `What’s Up?` (U+2019), `Songs 2003-2013` vs `Songs 2003–2013` (en dash), `II - Yo…` vs `II: Yo…`, `Abba Gold Anniversary Edition` vs `Gold (anniversary edition)` (parentheses) — all scoring 96–100 and all rejected. **Don't re-diagnose this as an asymmetry problem**; the first plan for the fix did, and it was wrong.

- **`internal/enrich/matchfold.go` is COMPARISON-TIME ONLY.** Its output must never be persisted, hashed into a filename, used as a cache key, or embedded in SQL. **DO NOT UNIFY it with the three sibling normalisers** — they look like duplication and are not: `unicodeLowerScalar` ([sqlfunc.go](internal/manifest/sqlfunc.go)) is a deterministic SQL scalar backing three functional indexes (changing it needs a migration; v26 already rebuilt them once); `ArtistImagePathByName` ([enricher.go](internal/enrich/enricher.go)) hashes into a FILENAME, so unifying orphans every cached artist portrait — a silent mass re-fetch invisible in CI, which `TestFoldForMatchIsNotTheArtistImageCacheKey` exists to trip; `normTitle` ([reconcile.go](internal/manifest/reconcile.go)) is deliberately weak because it groups albums for a pass that REWRITES TAGS.

- **Fold pipeline order is load-bearing** (pinned by `TestFoldForMatchPinsTheOrderedPipeline`): NFKD (**decomposition**, not NFKC — separating a base letter from its accent is what recovers `Zdob și Zdub` and `Yael Naïm`) → drop nonspacing marks → `cases.Fold()` via a **package-level** caser → `&` → `" and "` **space-padded and BEFORE punctuation** (a bare `&`→`and` turns `R&B` into `randb`, failing against `R and B`) → punctuation: **apostrophes DELETED** (taggers drop them, so `Ain't` must equal `Aint`; mapping to space gives `ain t` and fails), **dashes/colons/slashes to SPACE** (deleting dashes collapses `Re-Load` into `Reload`, two different Metallica albums) → collapse whitespace. **`foldTitle` never strips articles; `foldName` has a separate `foldNameNoArticle` variant.** `foldNameNoArticle(x) == stripLeadingArticle(foldName(x))` holds only while the article strip is the LAST stage — `pickBestArtist` derives one from the other to fold each candidate once, and `TestFoldNameNoArticleIsStripOverFoldName` fails loudly if that stops being true. **Documented gap, don't "fix" without evidence:** `ø ł đ æ` are atomic code points with no decomposition, so `Bjornstad` ≠ `Bjørnstad` while `si` == `și`. Measured: 8 of 300 unresolved artists carry one, and in every case it is on BOTH sides — they fail on the role suffix instead.

- **The short-title guard is LENGTH-ONLY (≤3 runes), never "or a single token".** An earlier draft had that clause and it is a real bug: canonical release-group titles are very often one token (Thriller, Gold, Nevermind, Rumours, Unplugged) and local tags routinely hang an edition suffix off them, so it rejects `Thriller 25th Anniversary Edition` → `Thriller` — the exact superset class the fold exists to recover. Measured in RUNES so a 3-character Cyrillic title doesn't take the strict path by byte-length accident. Against Atlas it almost never fires (Atlas's score IS `int(pg_trgm_similarity*100)+bonus`, and trigram similarity dilutes with length mismatch, so a short query against a long title dies at the floor first) — it is there for the **public MusicBrainz** configuration, whose Lucene relevance is not length-aware the same way. Locked by `TestShortTitleRuleIsLengthOnly`.

- **`pickBestArtist` drops the score floor from its equality passes, and that is the fix.** The `>=80` floor ran BEFORE the name was compared, so the right answer was discarded unread: `Peter, Paul and Mary` at **78** (186 tracks), `Carpenters` at **73** (81), `The Oscar Peterson Trio` at 86 (68), `Zdob și Zdub` at **57** (66), `Yael Naïm` at **53** (25). This is not a relaxation — equality after folding is a STRICTLY STRONGER predicate than a fuzzy score; `Zdob și Zdub` scores 57 only because `ș` and `s` share no trigrams. Passes are ordered **A1 raw-exact → A2 folded → A3 article-stripped → A4 the unchanged `>=90` fuzzy fallback**, and that ORDER IS THE SAFETY MECHANISM: the loosest rule can only ever apply to candidates every stricter one rejected. **Don't collapse the passes into one loop with "best so far" pointers** — behaviour-preserving, property-hiding. Zero measured resolutions came from A4, which is why it is untouched. **The `>=80` RELEASE floor is deliberately unchanged** (`TestPickBestReleaseStillRejectsBelowScoreFloor`) — it is the bound every relaxation leans on.

- **`releaseCandidate.ReleaseGroup.Title` is a second match arm, and it was free.** Atlas's trigram plan MATCHES on `release_group.name` but RETURNS `release.name` as `title` (Atlas's own analysis: they differ for **4.4% of releases**), so the bridge was rejecting candidates on a title its upstream never claimed to match. The field was already decoded and never read. Artist search `limit` went **5 → 25** in the same change: once the floor is gone the candidate WINDOW becomes binding.

- **Head-credit splitting: a separator qualifies ONLY if it is an unambiguous credit delimiter.** `;`, whitespace-delimited ` / ` (so `AC/DC` survives), `feat.`/`ft.`/`featuring`. **BANNED: `&`, bare `,`, ` with `, ` vs `** — all appear INSIDE real artist names (Simon & Garfunkel; Crosby, Stills & Nash; Sleeping with Sirens; Us vs Them). Measured: splitting `Peter, Paul & Mary` on `&` yields `Peter, Paul`, which matches an unrelated MusicBrainz artist `Peter Paul` at score 100 — **186 tracks with a wrong MBID**. Two things make this unrecoverable rather than merely risky: `pickBestArtist` validates against the QUERY THAT WAS SENT, not the original tag, so an exact match for the truncated name is accepted as correct; and `foldName` erases commas, so `foldName("Peter, Paul") == foldName("Peter Paul")` BY CONSTRUCTION — the acceptance layer cannot catch that one even in principle. **Never generating the query is the only defence.** Role truncation stays safe for the mirror-image reason: it cuts at the first comma-delimited segment that IS a bare role word from a CLOSED vocabulary, so `Earth, Wind & Fire` is untouched. Locked by `TestSplitHeadCreditNeverSplitsOnAmpersandOrBareComma`.

- **`stripArtistPrefix` returns WHICH artist it stripped, and the caller must query with that one.** On a split-credit album (track artist `John Lennon`, albumArtist `The Beatles`, album `The Beatles 1962 – 1966`) the prefix is found via the albumArtist, so querying the stripped title with the track artist fails `pickBestRelease`'s credit gate and wastes the rung. **`stripUnbracketedEditionSuffix` is KEYWORD-ANCHORED**, deliberately the opposite call from `albumEditionSuffixRE` next to it: a bracket is a *syntactic marker* announcing "this is a qualifier", so that regex can be content-blind; unbracketed text has no marker and a generic drop-trailing-words rule mangles `Dark Side of the Moon`.

- **`resolveArtist` still does NOT cache a clean no-match** (the PR #13 invariant, test-locked by `TestResolveArtist_NoMatchIsNotCached`). The artist ladder was nearly the reason to flip it, on the assumption a miss now costs up to `maxArtistAttempts` requests instead of one. **Measured instead: on the 300 unresolved artists the ladder generates 1 rung for 79 and 2 for 214 — a 1.47× multiplier that shrinks as artists resolve and get positively cached.** Not worth trading away a documented invariant; the number is in `buildArtistLadder`'s docblock. Re-measure before concluding otherwise. Ladder budget is per DISTINCT album/artist, not per track — both are LRU-backed and hard-capped.

- **`markSkipped`'s reason keys a BOUNDED map.** Keys are the `skipReason*` constants only (`no_search_terms` / `no_mb_match` / `mb_error`); a formatted error string must NEVER become a key or a flaky upstream mints one per distinct message (`TestSkipReasonsKeysStayBounded`). Surfaced via `Deps.EnrichSkipReasons` → `GET /api/enrichment/misses`, which also enumerates which tracks lack which field. **`facet` and `limit` deliberately do NOT join that endpoint's cache key** — they narrow a cached snapshot, so clicking through facets can't re-walk the library once per facet (the walk is a `json_extract` subtree scan, the `AtlasMetaBreakdownCounts` cost class). **`TrackMetaRef.MissFacets`/`IsMiss` are a LOCKSTEP MIRROR of `enrichmentMissPredicateSQL`**, and `TestMissFacetsMirrorsTheMissPredicate` seeds real rows and requires both to select the same set rather than comparing strings — a listing that disagrees with the button is how #596 happened.

- **The live control is a TEST, not a script** ([atlas_control_test.go](internal/enrich/atlas_control_test.go), env-gated so CI skips it). Run it before changing `pickBestRelease`. The reason it is in the repo: the recall numbers behind this work were first produced by a **Python reimplementation** of the fold that spaced apostrophes instead of deleting them and left `&` intact — numbers measured with a lookalike are evidence about the lookalike. It drives the REAL `buildReleaseLadder` and the real pickers. **Gate: 0 albums that resolve today may move to a DIFFERENT RELEASE GROUP** (a sibling pressing of the same group is benign — artwork and descriptions resolve through the group).
---

## Serve-time duplicate suppression (PRs #651–#654, 2026-08-05)

The bridge groups tracks by the iOS client's own duplicate-collapse identity and stops SERVING the non-winning copies (`duplicates.filter: highest-quality`, **default on**). Store keeps every row; nothing on disk is ever touched. Admin surface: Library → Duplicates (+ Jobs card). CLI: `bridge duplicates` (report-only, no mutating flag ever — FlagSet-walk test). No wire-shape change, `ProtocolVersion` stays 1; PROTOCOL.md documents the served-set semantics + fixed its old `since`-filters-on-mtime doc bug (it always was `indexed_at`); iOS `docs/BridgeProtocol.md` re-mirrored byte-identical (acoseac/1-bit#1293) after a two-way drift was found and reconciled.

- **`internal/dupes` is a VERBATIM MIRROR of iOS `MetadataNormalizer` + `CrossSourceTrackDedup.ContentKey` — the fourth DO-NOT-UNIFY entry in matchfold.go.** Its output must equal the client's partition; a fix that makes it "better" than the client makes it WRONG. Test literals are lifted verbatim from `MetadataNormalizerTests.swift` (an iOS rule change trips a test here). Honoured divergences: whitespace is `unicode.IsSpace` hand-rolled (Swift regex `\s` is Unicode, Go's is ASCII); the filename track rule accepts a BARE SPACE (`"07 Song.flac"` → 7) because Swift's does — `parseLeadingTrackNumber` is deliberately NOT reused; the KeyFor filename split is `/`-only (mirrors `NSString.lastPathComponent`; store paths are ToSlash'd anyway — Gemini's LastIndexAny suggestion declined). **Disc+track stay in the key**: dropping them inflates version-token false-positives 2.2%→19.3% (measured, pinned).
- **Tiers are evidence claims**: `different-format` = different masters (NEVER framed as redundant; renders per-member geometry inline), `same-format` = inference, `identical-audio`/`different-audio` = STREAMINFO-MD5-proven (v32; the ONLY tier allowed to say "reclaimable" is identical-audio), `inconclusive` absorbs every uncertainty and is NEVER suppressed, `self-nested` = upload accidents detected by **collapsed-path equality** (an eponymous album's run-of-2 can never read as one). `classify()` has a len<2 inconclusive floor.
- **Winner election** (`dupes.PlanSuppression`): lossless > bits > rate > size > shallower/shorter/lexicographic path — a strict total order so winners never flap (flapping = indexed_at churn). Self-nested keeps the shallowest per twin class. **DSD and PCM are NEVER cross-suppressed** (user decision 2026-08-05: both editions serve; ranking applies within each domain). `different-audio` (proven remasters) never suppresses under any mode, and a previously-suppressed copy AUTO-UN-SUPPRESSES when MD5 evidence lands (restamp diff + bump). Every suppression unit serves exactly one winner. The dupes-side lossy set mirrors `manifest.IsLossyCodec` (import cycle forbids calling it); `TestDupesLossyCodecsMirrorIsLossyCodec` is the lockstep pin.
- **Stamping pass = pass #6 of Scan's success tail** (`Scanner.RestampDuplicates`), AFTER all metadata reconciliation so keys see reconciled tags. v31 columns `dupe_group_id`/`dupe_tier`/`dupe_suppressed` are COLUMN-ONLY (v25/v28 rule — no json tags, never spliced to wire); upserts deliberately DON'T touch them (fresh INSERT = served; the same scan's tail re-stamps). Diff-vs-current → zero writes on a stable library. **Corrected 2026-08-06 — "the tail" is THREE successful exits, and ScanSubtree is a writer too.** Scan now dispatches the pass from a `defer` (scanOK-gated, registered after the post-scan-hook defer so LIFO runs stamps first) because the ctx-done skip and the routed-exclusion-set failure both `return count, nil` AFTER the deletion pass has committed — reached inline at the bottom, a reaped winner left its twin `dupe_suppressed = 1` with no served copy in its group, i.e. an album invisible to every client. `ScanSubtree` (the watcher's path) gained the same tail: its upserts retag rows out of / into groups and its bounded deletion pass reaps winners, and the upsert comment's premise ("the full-scan tail runs in the SAME scan as these upserts") was never true there — a de-duplicated row stayed hidden until the next full scan, up to `ScanIntervalSec` (6h) away. Its pass is whole-library like Scan's: duplicates group ACROSS directories, so a subtree-scoped election cannot see the twin in another folder. **The ctx-done leg is the one exit that still can't restamp** (a cancelled context cannot write) — documented, heals on the next scan. **Best-effort here is NOT symmetric: an unstamped row is served (fail-open), a STALE SUPPRESSION hides one (fail-closed) and only another stamping pass clears it.** `ScanSubtree`'s pass is gated on that scan having committed or reaped a row (the stamps derive only from `tracks`, and unlike Scan this runs per debounced watcher event — an unconditional pass would put two full-table `json_extract` streams on every noise event). Locked by `TestScanSubtree_RestampsAfterReapingTheServedWinner` + `TestScan_RestampsWhenTheReconciliationTailBailsEarly`. `ApplyDupeStamps` never touches `enriched_at`/`tags_json`; **`indexed_at` strict-advances ONLY on suppressed→served transitions** — reappearing rows MUST bump or deltas never see them. Don't add a bump on suppression; don't move the pass before reconciliation. **The suppression→delta story CHANGED with the deletion journal** (2026-08-19 sync-redesign batch): a served→suppressed transition now writes a `manifest_deletions` tombstone (`DupeStamp.JournalDelete`, same transaction as the stamp) and the since-leg emits it as `deleted: [paths]`, so suppressed copies no longer linger on synced clients until a full sync — the pre-journal sentence that stood here ("iOS deltas never delete; suppressed rows linger invisibly…") described the v0.1.9–v1.9 behaviour. `indexed_at` is STILL never bumped on suppression; the tombstone is the delta signal, and any non-suppressed stamp clears the path's tombstone (see `deletion_journal.go`'s header for the coverage/retention/mass-op contract).
- **The stamping pass re-checks for an in-flight scan immediately BEFORE `ApplyDupeStamps`, not just at the top.** `runDuplicatesSweeper` gates on `IsScanning()` and then calls `RestampDuplicates` WITHOUT `s.mu`, so a scan starting in that gap runs its own tail from fresher state while the sweeper is still mid-snapshot (two full-library streams + the election) — and the stale commit lands after it, un-suppressing rows the scan just suppressed. `Scanner.activeScans` (an `atomic.Int64` incremented by BOTH `Scan` and `ScanSubtree`) is the predicate; the exported `RestampDuplicates` abandons at the commit point when it is non-zero, while the scan tails call the unexported `restampDuplicates(ctx, insideScan=true)` and are exempt — they ARE what makes it non-zero. **Don't take `s.mu` instead** (deadlocks the in-scan callers; for external ones it blocks the scan for two library walks) and **don't widen the public `IsScanning`** to cover subtree scans — it drives the admin badge, the SSE fast tick and the booklet-GC skip. Residual, accepted: a scan that starts AND finishes inside one pass is undetectable. Locked by `TestRestampDuplicates_AbandonsWhenAScanStartsMidPass` (via the `beforeApplyDupeStampsHookForTests` seam, which is what pins the guard's POSITION) + `TestScanSubtree_MarksItselfScanInFlight`.
- **Served-population rule**: every number leaving `/v1` describes the served set. `ListServedTracks`/`StreamServedTracks`/`ListServedTracksPage`/`CountServedTracks` power the manifest (stream+page+total), `enrichmentProgress` (EnrichmentCounts' numerator gained `AND dupe_suppressed = 0`; `MAX(enriched_at)` deliberately unscoped), health `tracksIndexed`, the DLNA adapter (one `ListServedTracks` call filters the whole CDS surface), and the smart-playlist pools (predicate baked into `trackFeatureSelect`'s WHERE — callers append with AND — because mixes PERSIST paths and serve them verbatim). **The full-store readers stay unfiltered on purpose and MUST keep seeing suppressed rows**: `TrackPaths`/`TrackPathsUnder` (deletion-pass snapshots — filtering them would REAP suppressed rows), `ListTracks`/`StreamTracks` (reconciliation passes, fingerprint sweeper, upscale CLI), `UnenrichedTracks`, `GetTrack`, admin rollups/browse/composition (operator truth — the admin dashboard's TracksIndexed deliberately re-diverges from the wire count). `/v1/list`/`stat`/`read`/`download` are fs/path-keyed and unfiltered (stale clients keep working; suppressed files stay downloadable).
- **`duplicates.filter` is the one HOT-APPLYING feature setting** (highest-quality default | same-format | off; TailscaleMode-style resolver, validated at load). The settings PATCH fires `Deps.TriggerDuplicatesPass` → buffered-1 nudge → `runDuplicatesSweeper` (bgWriters-joined, nudge-only — no tick, no startup run; DEFERS when a scan is in flight because the scan's own tail reads the policy closure at runtime). `RestartRequired` stays false. **`SetDupePolicy` MUST be wired BEFORE `RunPeriodic` starts** — an unwired scanner stamps with FilterOff, which on boot would CLEAR every suppression (mass indexed_at bumps → iOS delta churn) before the next pass re-suppressed; main.go late-binds the live config holder through an atomic pointer with the boot snapshot as fallback. Unwired (tests/CLI) = groups still stamped, NOTHING suppressed (fail-open; stats work with filter off).
- **`tracks.audio_md5` (v32) rides the version-stale stamp leg** — the PR-D trap: `marshalForStorage` can't see the unexported `Track.audioMD5`, so byte-identical rows take `versionStampOnly` → `StampExtractorVersionBatch` now takes `[]*Track` and carries the hash via `COALESCE(NULLIF(?,''), audio_md5)` (fresh EMPTY must not erase a known value — this leg only runs on proved-identical rows), while both upserts write `excluded.audio_md5` UNCONDITIONALLY (bytes changed → stale hash must die, empty included). Reversing either direction fails silently. All-zero STREAMINFO MD5 = "not computed" sentinel → "" (else every checksum-less encode reads bit-identical). `ExtractorVersion` is 3; the bump's one-shot re-extract IS the column backfill (no migration backfill possible — needs the file).
- **Admin page reads the persisted `dupe_summary` scan_state document** (written at stamp time) — zero-cost tiles exactly consistent with the stamps, NO TTL/singleflight machinery, refreshed on the SSE `stats` isScanning true→false edge (Diagnostics-page precedent; deliberately NO new SSE event). Group listing = live paged query over the stamped columns (v31 partial index). The E2E browser pass caught `formatTimeAgo(sum.stampedAt)` needing `new Date(...)` — render tests pin ids, not JS runtime types; keep the live-browser step for new admin pages.
---

## Smart Mixes per-family actions (PR #657, 2026-08-05)

Admin-only — no `/v1` wire change, no `ProtocolVersion` bump, no migration. `/smartmixes` renders families as a card grid (track lists behind a collapsed `<details>`); each card gains **Regenerate** (`POST /api/smart-playlists/{slug}/regenerate`) and **Save as playlist** (`POST /api/smart-playlists/{slug}/save-as-playlist`, inline name form).

- **Per-family regenerate commits ONE row.** `smartplaylistgen.RegenerateFamily` runs the FULL engine (families share their assembled inputs — a "cheap single-family generate" would save nothing; don't attempt one) and commits only the slug via `Store.ReplaceSmartPlaylistFamily`: an existing row KEEPS its cached position (stable iOS homepage order — don't write the fresh engine index over it), a newly-visible family appends after max, and a family the fresh run no longer populates is REMOVED (matching the wholesale "empty families aren't written" semantics; surfaced as `removed: true` + explained in the UI, not treated as failure).
- **Saved-mix playlist ids MUST be canonical lowercase UUID v4** (`newPlaylistUUID`): the iOS restore path parses ids with `UUID(uuidString:)` and SILENTLY SKIPS anything else (`PlaylistSyncCoordinator`), so any other scheme makes saved mixes invisible to every device. Pinned by `uuidV4Re` in `handlers_smartplaylists_test.go`. The mint is hand-rolled (`crypto/rand`, correct version + variant bits) but **that is not because the dependency is forbidden — it is not** (corrected 2026-08-31): `github.com/google/uuid` has been a **direct** dependency in `go.mod` since PR #7, linked into every binary and imported by production code in `cmd/bridge/main.go`, `internal/manifest/store.go` and `internal/transcode/{batch,transcode}.go`. The PR #657 note declining to "add" it was already describing a dep the bridge carried, so the single-static-binary argument bought nothing there (unlike the genuinely declined bubbletea / `x/term` raw-mode cases). Either form is fine; `uuid.NewString()` would be equivalent. **The load-bearing half is the ID SHAPE, not who mints it.**
- **Save re-indexes item positions 0..N-1 from display order** — the flattened time-of-day pools repeat their per-hour positions and `(playlist_id, position)` is the PK; original blob positions must never be persisted. `device_token` carries the `admin-console` provenance sentinel (column has no FK; renders as `admin-co…` on the Data page and resolves through `resolvePlaylistDeviceToken`; never exposed on `/v1`). A mix's operator-uploaded cover clones to the playlist scope best-effort (same bytes + imageHash; failure logs, never fails the save). Saves are frozen snapshots by design — the mix keeps regenerating, the saved playlist doesn't follow.
---

## Diagnostics log export + bug-report bundle (2026-08-16)

Admin-only — no `/v1` change, no `ProtocolVersion` bump, no migration. Diagnostics
gains `GET /api/logs/{status,export,bundle}` plus an Export-logs panel; the Log-events
counts become buttons that arm the export at that level.

- **The log is parsed back from TEXT, because there is no structured copy.**
  `logging.Init` installs slog's TEXT handler on stderr and the service units send
  stdout AND stderr to one file, so the log interleaves structured records with the
  CLI's unstructured startup banner. Parsing is a STRICT prefix (`time=` then
  `level=`, the order TextHandler guarantees) — a whole-line search for `level=`
  matches the text inside a quoted `msg`, and `TestParseLogLine` pins that case.
- **Filtering is MINIMUM level, never exact match.** "warn only" as an exact match
  omits every ERROR — the worst events missing from the file the operator is about to
  attach to a bug report. `TestLogFilterMinLevelIncludesMoreSevere` fails on the
  exact-match form (negative-control-verified). Banner lines carry no severity, so
  they ride only the unfiltered "Everything" mode (`keepUnstructured`).
- **The scan starts at the END minus `maxLogScanBytes` (64 MiB), not at byte zero.**
  Nothing rotates this log — the author's own was **301 MB spanning 72 days** — and a
  windowed export wants the tail. `TestStreamFilteredLogTruncatesFromTheEnd` pins it,
  and needs SEVERAL early markers: with one, the partial-first-line skip ate it and
  the test passed against a from-the-start scan (i.e. pinned nothing until
  negative-controlled).
- **Redaction covers EVERY section of the bundle, not just log lines.** The header
  promises "absolute paths replaced"; the preflight section is free text carrying the
  config dir, roots and cert path, and shipped that in full under `redacted: true`
  until `bundleText` routed it through the redactor. **A bundle that claims redaction
  and leaks is worse than one that claims nothing** — the operator trusts the label
  and posts it. The handler tests missed this because their server had no `DoctorRun`
  wired, so the section was empty; the fixture now wires one that returns paths.
  Found by running the endpoint, not by review.
- **Quoted values must redact to the CLOSING QUOTE.** TextHandler quotes any value
  containing a space and music paths contain them constantly ("Album (Deluxe)"), so a
  whitespace-terminated rule redacts `/Music/Album` and leaves ` (Deluxe)/track.flac`.
  The unquoted rule's prefix class also needs `[`/`(`/`,` — the banner prints
  `(roots: [/srv/Music])` and a whitespace-only class matched none of them. Both were
  live-test findings. Documented residual: an unquoted path WITH a space keeps its
  tail (banner only); widening past whitespace would swallow the log message itself.
- **Defaults are asymmetric on purpose**: the plain export is raw (the operator
  reading their own bridge on a loopback console, where absolute paths are the point);
  the bundle redacts by default (it exists to be sent somewhere). Unknown `level` /
  `since` / `redact` values are REJECTED, never defaulted — a typo that silently fell
  back to "everything" hands back a larger, less-redacted file than was asked for.
- **`Deps.LogPath` empty is a legitimate state, not a misconfiguration.** Only a
  SERVICE install redirects stderr to a file; a foreground `bridge serve` has none, so
  `/api/logs/status` explains rather than offering a button that 404s.

**The two log-volume defects this work surfaced are fixed in the follow-up (below).**
---

## Log volume: M-SEARCH send suppression + a log-size check (2026-08-16)

The follow-up to the export work, which is what made the volume visible.

- **`sendMSearch`'s failure log is STREAK-SUPPRESSED: first occurrence at Warn, one
  Error at `ssdpSendErrEscalateAt` (20 ticks ≈ 10 min), silence until recovery, then
  one Info carrying the suppressed count.** It runs on a ticker and its failure mode
  is persistent by nature — "can't assign requested address" means the multicast
  route is gone, not that one packet was unlucky — so it failed on every tick and
  logged every one: 12 lines/minute unbroken, **199,078 of the author's last 200,000
  log lines, ~99.5% of a 301 MB log spanning 72 days**. The cost is not disk, it is
  that every other line becomes unfindable. `noteSendResult` holds the policy,
  separate from the I/O, so the rule is testable without arranging a real multicast
  failure (the `HandleReadErr` separation, for the same reason). Negative-controlled:
  logging every failure turns one day of ticks into 2,881 lines vs 2.
- **The READ side was deliberately left alone.** Its Warn-per-error is bounded by a
  250 ms backoff, and it logged **zero** lines across those same 72 days — it is not
  the bug, and rewriting shared policy used by two clients pre-release would be risk
  without evidence. If it ever does spam, `noteSendResult` is the shape to copy.
- **Rotation is a doctor WARNING, not a bridge feature — a deliberate call.** The
  bridge does not own the log file: `logging.Init` writes to stderr and the service
  units redirect it, so launchd/systemd/the Windows wrapper opens it and the bridge
  holds no descriptor to roll. Rotating from inside would mean copy-truncate (races
  the writer, drops lines) or taking ownership of the file, which changes the unit
  templates on three platforms — neither belongs in a point release, and neither is
  the real fix, since an unbounded log is a symptom of something logging on a timer.
  `checkLogSize` (warn at 256 MiB) names the condition and points at the platform's
  own tool (newsyslog / logrotate / a scheduled task). **`doctor.Deps.LogPath` empty
  is a no-op, not a complaint** — a foreground `bridge serve` has no log file.
  `TestRun_FullReportShape` enumerates check names in order, so adding a check is a
  deliberate edit rather than a silent one.
---

## Auto-optimize: background pre-generation of CarPlay variants (2026-08-17)

`upscale.autoOptimize.enabled` (default **off**) turns on a serve-side sweeper that
builds `optimized-*` variants ahead of a request. **No wire change** — variants already
ride `Track.variants`, so iOS discovers them on the next sync with no app-side edit, no
`ProtocolVersion` bump, no PROTOCOL.md change and no Mirror-PR (`bridge optimize` has
always been able to mint variants nobody asked for; only the trigger is new).

- **The motivation is a structural miss, not latency.** iOS mints these lazily:
  `PlayerService.resolveVariantID` tier 0 asks for an optimized variant when CarPlay is
  the active output and, finding none, returns nil → plays the **hi-res source** while
  firing a fire-and-forget `requestOptimize`, so only the NEXT play is cheap. On shuffle
  nearly every play is a first play, and the bandwidth saving almost never lands. The
  download path parks the job in `.generatingVariant` for up to **90 s** and falls back
  to the source on timeout. Don't re-frame this as "saves a few seconds".
- **`JobSpec.Background` is the load-bearing field, and `Kind` cannot express it.**
  `optimizeJobs` is the Pool's FOREGROUND lane, added specifically so an on-demand
  CarPlay request doesn't head-of-line block behind a batch. A swept job is the same
  KIND of work with the opposite urgency, so `routesToOptimizeChannel(kind, background)`
  demotes it to the low lane while `Kind` still drives `VariantID()`. Enqueuing a
  library-wide sweep on the foreground lane re-opens the exact HOL blocking the
  two-channel queue exists to prevent — with no symptom visible bridge-side. Pinned by
  `TestRoutesToOptimizeChannel` + `TestPoolBackgroundOptimizeUsesUpscaleLane` (asserted
  through the channels, not through timing) and by the sweeper's own
  `TestAutoOptimizeSweepEnqueuesBackgroundJobs`.
- **The candidate query is `ListAutoOptimizeCandidates`, deliberately NOT
  `ListTrackProjectionsUnderPrefix`** ([manifest/auto_optimize.go](internal/manifest/auto_optimize.go)).
  That one backs the admin Inspector, where operator-truth means showing everything;
  a path that spends disk and CPU needs three things it lacks: **UPnP-routed rows
  excluded** (measured on the author's hybrid fixture: 1,559 eligible rows, **1,525
  routed from a Chord 2Go** — unfiltered, every tick resolve-fails them, the shape
  `TrackPathsLocal` fixed for the analysis sweeper); **`dupe_suppressed = 0`** (a
  suppressed row is never served, so its variant could never be requested);
  and **selection keyed on "NO FRESH variant exists"** — `HasVariant` is existence-only,
  so a re-encoded source keeps its old sidecar forever, `serveVariant` answers
  `410 variant_stale`, iOS silently falls back to the source and nothing ever
  regenerates it. All four arms negative-control-verified.

  **The coverage test must be `NOT EXISTS (fresh variant)`, never "some row is stale",
  and never a JOIN** (caught in review; the first draft was both). `track_variants` is
  keyed on `(source_path, variant_id)` and an optimize id encodes the schema version AND
  the target rate, so ONE track can hold several `optimized-%` rows — a
  `VariantSchemaVersion` bump leaves the old id behind (which is why
  `ListTrackProjectionsUnderPrefix`'s LIKE is documented as "version-agnostic to cover
  both v1 and v2"), and so does a re-rip that moves the source between the 44.1k and 48k
  families. The sweeper only ever writes the CURRENT target's id, so a superseded row's
  recorded source facts never advance: it is stale forever. "Some row is stale" therefore
  re-selects the track on EVERY sweep and regenerates an already-fresh variant, and since
  `UpsertVariant` strict-advances `indexed_at`, every sweep pushes a delta row to every
  paired device — the exact regenerate-every-sweep loop this design exists to avoid. The
  JOIN additionally multiplied the row (one candidate per stale variant), double-spending
  `maxPerSweep` and over-reporting the backlog, so `StaleVariantID` comes from a
  single-row correlated subquery. Pinned by
  `TestListAutoOptimizeCandidatesIgnoresSupersededVariantRows`.
- **Staleness compares the variant against the TRACK ROW, and the sweeper stamps
  `SourceMTimeNS`/`SourceSize` from that same row.** Self-consistency, not laziness: a
  freshly written variant necessarily satisfies the predicate and cannot be re-selected.
  Stamping a LIVE stat instead (which looks more correct) makes every variant read as
  stale whenever the scanner hasn't caught up — a regenerate-every-sweep loop on exactly
  the files that changed most recently. The scanner stays the authority on what a file
  is; a drifted file heals on its next scan. Matches `Coordinator.buildOptimizeCandidates`,
  so a swept variant is indistinguishable from an on-demand one.
- **`maxPerSweep` is not just a queue guard.** `UpsertVariant` strict-advances the parent
  track's `indexed_at`, so an uncapped first sweep pushes one delta row per variant to
  every paired device at once. The cap turns that into a drip. `minFreeBytes` is a
  RUNNING budget (accumulating `ProjectedSize` per candidate), because the on-demand
  path's per-batch `diskPreflight` is a point check and cannot bound a loop that runs
  forever. The disk probe **fails CLOSED** — no free-space reading means no way to honour
  the floor, and a skipped sweep costs nothing while a wrong guess fills the operator's
  volume. Both defaults resolve from zero AND negative input (a `-1` typed hoping to
  disable a cap must not yield an unbounded sweep).
- **Ordering is `indexed_at DESC`.** Newest-indexed first is both the literal ask and the
  right spend order under a cap or a floor — the head of the queue is what actually gets
  built. Considered and rejected: filtering to a *curated* scope (favorites / playlists /
  CarPlay history). Those tables are opt-in per device on iOS and empty on both live
  bridges, so a curated scope silently does nothing; ordering achieves the same
  prioritisation without a config that can be wrong.
- **`Scanner.SetPostScanHook` REPLACES — never call it twice.** `cmd/bridge` now collects
  nudge channels in `postScanNudges` and registers ONE fan-out hook after every sweeper
  has appended. Registering a second hook for this feature would have silently unhooked
  the analysis sweeper, leaving freshly scanned tracks unanalysed with nothing failing
  anywhere. Landing the registration after `RunPeriodic` has started is the pre-existing
  intended pattern (`postScanHook` is an `atomic.Pointer` for exactly that reason).
- **The settings toggle HOT-APPLIES — deliberately unlike every sibling audio flag.** The
  sweeper reads `enabled` live per sweep, so the PATCH nudges it instead of setting
  `RestartRequired` (the `duplicates.filter` precedent). The nudge fires in BOTH
  directions: on→off is what makes the Jobs card stop showing numbers from the last real
  run instead of waiting out a tick hours away. The outer `upscale.enabled` gate stays
  restart-required (the Pool is wired at boot).
- **A disabled sweep returns a `Disabled`-marked counts struct, not nil.** `nil` means
  "failed, keep the previous numbers" in `sweepStatus.sweepFinished`, which would freeze
  the card on a stale successful run right after the operator turned the feature off.
- **A negative control MUST be gated on a successful build.** Twice during this PR the
  disk-floor control was mutated to `if false {`, which makes `floor` unused — so `go test`
  reported failure from the COMPILE ERROR and the control read as "good" while pinning
  nothing. Mutate in a way that keeps the symbol used (here `< -floor`), and run
  `go vet ./<pkg>/` first, treating a build break as *control invalid* rather than as
  evidence. The same trap applies to any mutation that deletes the only use of a variable
  — which is most "just disable this branch" edits.
---

## CodeQL triage — the whole open queue was FP; log-injection is FP BY CONSTRUCTION (2026-08-18)

Full sweep of Security → Code scanning: **58 open alerts dismissed, 0 left open** (62 dismissed
total including the pre-existing #1–4; 7 already auto-fixed). No code change was warranted
anywhere. The workflow runs `security-extended` deliberately (`.github/workflows/codeql.yml`),
accepting "the occasional false positive" as the price of taint-flow coverage — this is what
that trade looks like in practice. **Don't re-open any of these without reading the dismiss
comment on the alert first**; each carries its own rationale.

- **`go/log-injection` (51 alerts) is a false positive BY CONSTRUCTION, and it WILL regenerate.**
  Every new slog call carrying request-derived data mints a fresh alert, so this triage recurs —
  dismiss on the same grounds rather than re-deriving them from scratch. The grounds: Go's
  `slog` **`TextHandler` AND `JSONHandler` quote the value and escape `\n` / `\r` inside it**, so
  injected content can never start a new physical line and a forged entry is impossible.
  **Verify empirically, not from memory** — a 10-line program logging
  `"ok\ntime=… level=ERROR msg=…"` settles it in seconds. Two structural facts keep the class
  closed: every flagged site passes its value as a structured ATTRIBUTE (never concatenated into
  a raw writer), and `internal/` contains no `log.Printf` / `fmt.Fprintf(os.Stderr, …)` at all —
  which is exactly what the `## Things that have bitten before` structured-logging entry
  enforces, so that invariant is load-bearing for this dismissal too. The three sites that
  concatenate into the MESSAGE (`logger.Info(scope+": …")`, `handlers_api.go`) are safe twice
  over: `scope` is a caller-supplied literal at both call sites, and slog quotes the msg as well.
  Corroboration from the other direction — `/api/logs/export`'s parser is anchored on a strict
  `time=` + `level=` PREFIX precisely because a whole-line search would match text inside a
  quoted `msg`; the codebase already assumes and depends on this escaping.
- **`go/path-injection` (3: #7 `files.go:117`, #8 `files.go:314`, #69 `files.go:503`)** — same
  class as #1–4 with the rationale unchanged: the path flows through `fs.Resolver.Resolve`.
  #69's site already carries an in-code comment saying so. Don't contort any of them into a
  lexical re-check to appease the scanner.
- **`go/uncontrolled-allocation-size` (#9, `search.go:127`)** — `limit` is clamped to
  `searchHardCap = 500` THREE LINES ABOVE the flagged `make()`, and that clamp was added for
  this exact alert (PR #243). CodeQL doesn't model the reassignment as a sanitizer, so "fix it
  again" isn't available — dismissal is the only disposition left.
- **`go/cookie-secure-not-set` (#10 / #11)** — `Secure: s.cookieSecure()` = `cfg.IsPublic()`.
  Correct by design, and **must NOT be "fixed" to a literal `true`**: loopback admin is plain
  `http://127.0.0.1:7789`, a browser never returns a `Secure` cookie over http, so hardcoding it
  breaks admin login outright.
- **`go/request-forgery` (#12, `server.go:557`) — the only "critical", the only judgment call,
  and its residual is real if bounded.** `callbackHostAllowed` rejects hostnames outright and
  admits only loopback / RFC1918 / link-local IPs or the SUBSCRIBE source IP. **Loopback IS
  admitted**, so an on-LAN control point can aim the GENA initial NOTIFY at the admin port —
  contained three ways: the response body is never read (blind, no exfiltration), the body is a
  fixed XML propertyset, and `csrfGuard` 415s it before any handler (NOTIFY misses the
  GET/HEAD/OPTIONS pass-through, so it takes the must-be-`application/json` arm with a
  `text/xml` body). Residual capability: a blind LAN-only trigger against a LAN/loopback
  host:port. Tightening is a legitimate option (drop loopback, or pin to the source IP alone) —
  reopen deliberately rather than by re-discovering the flow.
---

## 2026-08-19 bug review — reap-time sidecar reclamation, harvest base-URL pin (PRs #723 / #724 / #725)

Scope-selected review of the four highest-blast-radius areas (silent data loss >
auth/token lifecycle > unauthenticated LAN parsers > path resolution +
concurrency). **The three areas ranked most dangerous came back clean** — every
past regression there already carries a pin and a comment. Both real defects were
resource-reclamation gaps, and the one live-exploitable finding was in the
NEWEST code, not the most dangerous-sounding.

- **The threshold reap now unlinks its sidecars, reversing PR #193's documented
  trade-off** (#723). `IncrementMissingTracksAndDeleteAtThreshold` deleted `tracks`
  rows while CASCADE dropped the `track_variants` / `track_analysis` rows, leaving
  the `.flac` variant and waveform FILES unreachable by path. It is the ONLY
  track-deletion path that fires in normal operation — `DeleteTrack` has no
  production caller at all, and `DeleteTracksBatch` serves only case-only renames
  and the UPnP reap — while all five siblings already enumerated + unlinked.
  PR #193 skipped it deliberately ("an N-row SELECT + Stat-cluster per scan …
  the dominant scanner cost on a 50k-track library"); **every premise fails for
  the SET-SCOPED form the siblings use** — the `len(missingPaths) == 0` early
  return means a stable library never reaches it (so it is per-REAP-PASS, not per
  scan), it is cheaper than the per-path `UPDATE … WHERE path = ?` loop
  immediately above it, and nothing stats (`removeSidecarFiles` calls `os.Remove`
  directly). The reasoning fit `DeleteTrack`-in-a-loop. Magnitude also changed:
  auto-optimize (2026-08-17) pre-generates variants library-wide, and the forward
  orphan sweeper that was the stated mitigation is still opt-in and off by
  default. **Both DELETE arms and both sidecar enumerations are DERIVED at
  compile time from `thresholdReapBatchWhereSQL` / `thresholdReapOneWhereSQL`**
  (const + const is a const) so the unlink set and the row set cannot diverge —
  the failure `DeleteTracksByPrefix` states as "these two MUST agree" — and that
  const-derivation is also what keeps SonarCloud `go:S2077` quiet (the
  `trackFeatureSelect` convention; a runtime `+where+` reads as an assembled
  query). **The ill-formed-UTF-8 arm needs its OWN enumeration**: those paths
  cannot ride the JSON array, so they could not ride its sidecar query either,
  and would have been the one reaped population still leaking. Variant
  enumeration is STRICT (abort + rollback before the CASCADE runs); waveform
  stays best-effort. **Don't drop the explicit `rows.Close()` in favour of the
  deferred one** — a `*sql.Tx` pins ONE connection, so the variant Rows must be
  closed before the waveform read runs on the same tx; the defer is only a
  panic-safety net (the `PrunePlaylistCoversExcept` idiom, `Close` being
  idempotent). `--gc` remains the recovery path for orphans stranded BEFORE this
  shipped; it is no longer the routine mechanism.
- **`atlas.harvestBaseUrl` pins the Atlas host `POST /v1/atlas-harvest/credential`
  accepts — a demo bridge that enables harvest MUST set it** (#724). The
  credential body carries `atlasBaseUrl` and the harvest client dials it for BOTH
  submit and fetch, so whoever sets it chooses where the bridge pulls bios from;
  they land in `artist_atlas` and are served to every client by `/v1/atlas-meta`
  with an attacker-chosen `SourceURL` the app renders as a "Read more on …" link.
  That is the same content injection `refuseAtlasIngestInDemoMode` blocks on
  `/v1/atlas-ingest` — **its comment deliberately left this sibling open, reasoning
  about the TOKEN ("a bogus one just fails the harvest") and not about the base URL
  travelling in the same request.** It was LIVE on `bridge.1-bit.app`:
  `WithAtlasHarvest` and `WithBooklets` are wired in the SAME `if harvestState != nil`
  block, `harvestState` opens only under `Atlas.HarvestEnabled && Atlas.Enabled`,
  and the bridge advertises `booklets` — and a demo bearer is public by
  construction (the static `demo.tokenSHA256` ships inside every installed app,
  the stated premise of `refuseUpscaleMutationInDemoMode`). Rules: a pin binds in
  EVERY mode; unpinned is refused in demo and still allowed off-demo (back-compat
  — bridge.ars.md keeps working). **The check runs BEFORE persistence** because
  `SetCredential` resets the sync cursor and clobbers the token when the base URL
  differs. **Both sides of the comparison MUST go through
  `config.CanonicalHTTPSBase`** — it is an equality test, so a reduction applied
  to one side only (the `:443` case) turns a correct pin into a mismatch that
  fails CLOSED and reads as a broken feature; `WithAtlasHarvest` canonicalizes its
  argument rather than trusting the caller. Residual, accepted: a public demo
  bearer can still overwrite the TOKEN for the pinned host (denial of function,
  not injection). **`bridge.1-bit.app` must set `atlas.harvestBaseUrl` when it
  next deploys**, or its credential submissions are refused.
- **`PrunePlaylistCoversExcept` is deliberately unwired — don't "finish the job"**
  (#725). Filed as a reclamation gap (written, documented, tested, zero callers),
  and the obvious fix is wrong: smart-mix retirement is REVERSIBLE
  (`buildFavorites` returns `(GeneratedPlaylist{}, false)` below `MinFavorites`,
  and the time-of-day families behave the same), and playlist deletion is a
  revivable tombstone. Covers are operator-UPLOADED, so either trigger silently
  destroys authored content to reclaim a JPEG; the operator already has
  `DELETE /api/playlists/{id}/cover`. An AST-based guard test fails if a
  production caller appears — **substring scanning was wrong here** because the
  natural way to record this decision is a comment mentioning the name, and a
  guard that cries wolf gets deleted.
- **`fs.Resolve`'s final containment check is the PRIMARY defense on Windows, not
  belt-and-braces** (#724, comment only). Both guards above it are slash-based —
  the raw `..`-segment scan splits on `/` and `path.Clean` treats `\` as ordinary
  — so `..\..\..\Windows\System32\config\SAM` passes both untouched and it is
  `filepath.Join`, Cleaning with `\`, that collapses it into a real escape. The
  prefix check refuses it. `FuzzResolveContainment` seeds that exact shape. Don't
  delete it as redundant.

**Process (both worth repeating):** a review doc enumerating UNFIXED weaknesses is
an exploit index, and this repo is PUBLIC — `.gitignore` covered `ops/*audit*.md`
but not the new filename, so the findings doc reached a public branch before being
force-pushed out (the blob stays fetchable by SHA regardless). The rule now covers
`ops/*bug-review*.md` / `*review*.md` / `*findings*.md`. And **two of four findings
changed materially after first write-up, both from acting on a first reading before
finding the code that already reasoned about it** — F1's rationale was 28 lines
above the function header, and F4's fix was nearly a blanket demo refusal until the
sibling guard's comment showed demo bridges are MEANT to be credentialed there. In
this codebase, starting to read at `func` is a reliable way to mis-file a
deliberate decision as a bug.
---

## Admin web player (2026-08-23) — `/` is a library player, Settings is one screen

The admin console's root is a **music player** (album grid → artist/album
detail → in-browser playback), Stats is the old dashboard, and every operator
page moved verbatim behind one **Server** nav entry. Bridge-only: no `/v1`
change, no `ProtocolVersion` bump, no `PROTOCOL.md` change, no iOS mirror, no
migration.

- **The catalog is computed, not stored** (`internal/librarycat`, cached on
  `admin.Server` behind an `atomic.Pointer` + singleflight + epoch fence). Album
  identity is `dupes.AlbumIDOf(dupes.Resolve(row))` — the SAME value the iOS
  client computes — so the browser's album partition equals the phone's **by
  construction**. `TestAlbumPartitionMatchesDupes` is the contract test.
  **Don't add album/artist columns to `tracks` "for speed"**: genres and
  composers are multi-value axes with fold rules SQL can't express, so the Go
  pass is required either way, and the measured cost is a fraction of a second.
- **`internal/dupes` gained `Resolve` / `AlbumIDOf` / `SortName` / `ArtistID`.**
  The extraction is behaviour-preserving and `clientkey_test.go`'s Swift-lifted
  literals passing UNMODIFIED is the proof. The catalog needs the RESOLVED
  display values and the INFERRED disc/track, not just the keys — `discNumber`
  is tagged on 38 of 15,370 rows on the reference library, so the folder rule
  is what actually orders every box set.
- **Invalidation is LAZY and that IS the debounce.** The post-scan nudge bumps
  an epoch; the next reader rebuilds. `postScanNudges` fires after every scan
  including watcher-driven `ScanSubtree`, so an eager rebuild would re-fold the
  library dozens of times during a bulk import — and none of it matters if
  nobody has the player open. **Append to `postScanNudges`; never call
  `SetPostScanHook` a second time** (it replaces).
- **The browser MIME table is NOT `dlna.defaultMIMEForExtension`**
  (`internal/admin/player_audio.go`). That table maps `.flac → audio/x-flac`,
  which is right for hardware renderers and wrong for browsers: measured in
  Chromium, `canPlayType("audio/flac")` is `"probably"` and
  `canPlayType("audio/x-flac")` is `""`. Reusing it would have made FLAC — 88%
  of the reference library — look unplayable. Two tables, two contracts.
- **Playability reports FACTS, not a verdict.** `canPlayType` answers `""` for
  codec strings an engine doesn't RECOGNISE even when it can decode the file,
  and ALAC/AIFF support diverges across engines — so the server says
  `universal` / `engine-dependent` / `none`, offers a fresh FLAC variant when
  the source isn't universal, and lets a decode attempt plus
  `MEDIA_ERR_SRC_NOT_SUPPORTED` be the authority. **`none` is DSD only.**
  Verified with bytes, not a capability string: a 192/24 FLAC decodes and seeks
  in the browser.
- **`safeQuery` now exists in `internal/admin` too, and the client must pair
  with it.** `url.Values` decodes `+` as a space, so a track at
  `Plus Test/A+B Song.flac` resolved to `A B Song.flac` and 404'd — the
  documented `/v1` variant-delete trap, present in the PRE-EXISTING admin
  browse / projection / enrichment / keyset-cursor handlers. **The client must
  use `encodeURIComponent`, never `URLSearchParams`**: the latter form-encodes
  a space as `+`, which broke every spaced path the moment `safeQuery` landed.
  Pinned by `TestSafeQueryRoundTripsEncodeURIComponent`.
- **Routed (UPnP) tracks are in scope** — 15,283 of 15,370 on the reference
  library, 13,519 of them FLAC. Playback reuses `internal/upnpproxy`, but the
  response goes through a writer that overrides `Content-Type` at
  `WriteHeader` time, because that package relays with `Header().Add` and a
  pre-set value emits TWO. **Don't change `upnpproxy`** — its verbatim relay is
  the bit-exact contract for iOS and DLNA. The same writer reports whether the
  upstream honoured `Range`, so the client can disable the scrubber instead of
  binding it to a duration the upstream won't serve.
- **`//go:embed static/*` recurses, but skips `.`/`_`-prefixed names inside
  matched SUBDIRECTORIES.** `static/player/_util.js` would compile, embed
  nothing, and 404 in a release build while working from a dev checkout.
  `TestEmbeddedStaticTreeMatchesDisk` catches it. **Don't "fix" this with
  `all:static`** — that pulls `.DS_Store` into every release binary.
- **`/static/` forces `Content-Type` + `nosniff` + `Cache-Control: no-cache`.**
  Module scripts are MIME-checked unconditionally and hard-fail, and on Windows
  `mime.TypeByExtension` consults the registry where `.js` is routinely
  `text/plain`. `no-cache` covers the other half: `?v=` busts only the ENTRY
  module, since relative import specifiers don't inherit a parent's query.
- **The admin artwork route had DRIFTED from its documented `/v1` mirror** and
  is now fixed: three-arm pattern (incl. the 16-hex `artworkVersion` alias) and
  the `{1200,500,250}` ladder. Before: `?size=1200` → 404, `?size=250` → 404,
  alias → 400. `TestAdminArtworkPatternMatchesV1` reads the regex out of
  `internal/api`'s SOURCE rather than importing it — `internal/api` imports
  `internal/admin`, so any import is a cycle, and a hand-copied literal is two
  copies that can be wrong together.
- **Settings is one screen; the PATCH payload is an explicit ALLOWLIST.** A
  field that renders but isn't mapped in `app.js` saves nothing while the page
  still says "Saved." — worse than not offering it. Caught in a browser when
  the backup fields were made editable.
  `TestEverySettingsFieldIsMappedIntoThePatchPayload` walks the template and
  requires a mapping, with an exemption list that must state a reason.
- **The prerequisite chip beside each gated toggle is the highest-value part.**
  It exists because of a live state: `analysis.enabled: true`, toggle reads on,
  `/api/jobs` says `degradedReason: sox_missing` (boot precheck failed, pool
  never wired), `/api/analysis/stats` says `enabled:false, soxAvailable:true`,
  `/api/doctor` says `audio-toolchain ok`. Four endpoints, four true
  statements, nine days of doing nothing, and nothing next to the switch.
- **`smartPlaylists.enabled` now defaults ON** via the nil-means-on pointer
  shape (`EffectiveEnabled()` — **never read the pointer directly**, a bare nil
  check reads as off and silently restores the old default). It is pure SQL
  over the existing manifest: no toolchain, no endpoint, no disk.
  **`upscale`, `autoOptimize`, `analysis`, `fingerprint`, `atlas` and
  `libraryWatch` stay OFF** — each commits the operator to gigabytes, CPU, a
  third-party key, or an open endpoint. `TestOtherFeatureDefaultsUnchanged`
  pins that; making them one glance away is the fix for "looks broken", not
  changing the value under every install on upgrade.
- **The web player is NOT a bit-exact path** and says so. Browsers resample to
  the device rate; there is no exclusive mode and no DoP. iOS remains the
  reference player.
---

## Player partial-boost — audio survives navigation to operator pages (PR #742, 2026-08-24)

Leaving the player for Stats / Settings / Server was a full page load, which
destroyed the DOM and the `<audio>` element, so playback stopped. Partial-boost
swaps `<main>` in place instead. Admin-console only — **no wire change, no
`ProtocolVersion` bump, no migration, no iOS mirror.**

- **What makes it possible at all: the `<audio>` element AND the now-playing
  bar are parented to `<body>`, outside `<main>`** (`audio.init()` /
  `nowplaying.mount()`, both idempotent). A swap that only replaces `<main>`'s
  content leaves them — and playback — untouched. Do not move either inside
  `<main>` / `#player-root`.
- **Server**: `renderPage` gained `r`; on `X-Bridge-Partial: 1` it executes the
  `"content"` block instead of `"layout"` and returns `X-Bridge-Active` /
  `X-Bridge-Section` headers (the client updates `body[data-active]` + the
  top-nav highlight from them, so `sectionForTab` stays the single source of
  truth). `Vary: X-Bridge-Partial` on both shapes. Pinned by
  `TestPartialBoostRendersContentOnly` (content-only, headers, both shapes'
  Vary; a UPnP-specific marker, not the shared `class="subnav"`).
- **Teardown is the load-bearing hazard.** Operator `initX` register
  document/window listeners + intervals nothing removed; on a full load that
  never mattered, but under boost they stack a copy per visit — and the
  inspector's `popstate` handler actively fights the router. Every such
  registration is scoped to an `AbortController` (`pageSignal()`) that
  `dispatchPageInit(tab)` aborts before the next page's init.
  `dispatchPageInit` is the SINGLE entry for operator init (first paint AND
  every swap), so the two can't drift. Element-level listeners need no scope —
  they die with the swapped DOM. The diagnostics poll and the inspector panel's
  a11y listeners get explicit `pageSignal().addEventListener("abort", …)`
  teardowns; the timers that self-terminate on DOM absence
  (`makeVisibilityChain`, `workerElapsedTimer`) are left alone. Verified: the
  5 s diagnostics poll fires 0 times in 6.5 s after boosting away.
- **`PLAYER_HEADS` (boot.js) MUST match the server's `playerRoutes`.**
  `isPlayerPath` decides player-vs-operator; a head the server serves as a
  player route but that the set omits gets fetched as an operator partial and
  `dispatchPageInit("player")` (a no-op) runs instead of `mountShell()`,
  leaving the shell un-booted. This drift was present as written
  (genre/composer/playlist/mix missing) — now pinned by
  `TestPlayerHeadsMatchServerRoutes`, negative-controlled both ways.
- **Concurrent-navigation generation guard.** `boostSwap` claims `++boostGen`
  up front and discards its response (returns true — no hard fallback) if a
  newer navigation started while it awaited the network. Without it, two fast
  clicks let the slower fetch resolve last and land you on the wrong page.
  Because `pushState` happens INSIDE `boostSwap` after the guard, a superseded
  nav pushes no phantom history. Verified: three rapid nav clicks land on the
  last, URL + content paired, history grows by exactly the ones that committed.
- **Player boot split.** `boot()` runs `wireGlobal()` ONCE (delegated
  `a[data-route]` click, the `/` shortcut re-querying the input by id, popstate
  guarded to `isPlayerPath`) and `mountShell()` per shell injection
  (renderSections + element-level search wiring + route). `window.__player`
  exposes `isPlayerPath` / `mountShell` / `route` for the boost router to
  re-mount the shell after swapping it back into `<main>`. Re-running
  `wireGlobal` would stack the global listeners — don't. The old "open operator
  links in a new tab while playing" handler is gone (boost replaces it).
- **SSE recycle IS the resnapshot.** The server byte-diff-suppresses frames per
  CONNECTION, so a freshly-injected tile gets nothing until a value changes
  (≤30 s). `recycleEventStream()` (close + reopen) after each swap forces a full
  initial snapshot — the mechanism `handleVisibilityRestore` already uses.
  Verified: `tracks-indexed` on the injected Stats page hydrates immediately.
- **`?boost=0` LATCHES for the tab session via `sessionStorage`** (else it
  disables boost for exactly one load then re-enables on the next, useless when
  boost itself is what's broken). Any single boost that can't complete falls
  back to `location.assign` — a hard load of the same target — so boost is a
  pure enhancement with no new failure mode. `runInlineScripts` block-wraps
  inline (no-src) classic scripts so a re-execution can't throw a const/let
  redeclaration; `application/json` islands + module scripts are left in place.
---

## Console shell: one sidebar replaces two nav levels (2026-08-24)

Admin-console only — no `/v1` shape change, no `ProtocolVersion` bump, no
migration, no iOS mirror. A visual redesign, but the load-bearing part is
structural: the console carried FOUR nav idioms at once (top tabs, a
nine-entry `.subnav` rendered inside every Server page, the Settings jump
rail, the player's section rail), and two of them were competing for the same
job. One flat sidebar absorbs the first two.

- **The rail is still the `<header>` ELEMENT, and that is what made the change
  cheap.** `initMobileNav()` resolves it with `querySelector("header")` and
  toggles `data-nav-open` on it; `#nav-toggle`, `#primary-nav`,
  `#theme-toggle`, `#conn-status`, `#pairing-badge` and `#logout-btn` all kept
  their ids. So the entire JS side of the shell needed exactly one change (see
  below) — everything else is CSS and template. **Don't rename the element or
  those ids** to something more literal; the tag IS the contract.
- **`boostUpdateTopNav` matches the TAB first and the SECTION only as a
  fallback, in two passes.** With the sub-nav absorbed, every operator page
  has its own rail entry keyed on its own tab, so a section-only match (what
  it did before) would light one entry for nine different pages. The fallback
  exists for exactly one case: the player's client-side sub-routes (`/albums`,
  `/artists`, …) all render the `player` tab, and Browse is keyed on the
  section so every sub-route keeps it lit. **Two passes, not one pass with an
  OR** — `data` and `smartmixes` carry their own tab while still belonging to
  the `server` section, so a single-pass OR lights their entry AND any
  section-keyed one. Both values arrive on `X-Bridge-Active` /
  `X-Bridge-Section`, so `sectionForTab` stays the single source of truth and
  the client never re-derives it. Pinned by
  `TestPrimaryNavHighlightsEveryEntry`, which now asserts exactly one entry is
  current — the assertion that earns its keep once there are twelve entries
  and two match keys.
- **Rail groups are semantic, not a restatement of `sectionForTab`.**
  Playlists, history and smart mixes sit under LIBRARY even though the server
  still files them under the Server section for routing. No URL moved, so no
  bookmark broke — the grouping is presentation.
- **`body` is a two-column grid and the rail is a real grid COLUMN, not a
  fixed overlay.** `<main>` needs no margin compensation, nothing can slide
  under the rail, and the collapse below 1024px is one `grid-template` change
  instead of unwinding a set of offsets. `body.login-page` resets the grid —
  that page is full-bleed with no shell.
- **Icon presentation lives on the `<use>` host (`.nav-ico`), never on the
  sprite's source `<g>`, and the `viewBox` is an HTML attribute.** A CSS rule
  matching the original element does NOT reach the shadow tree `<use>` builds
  from it; what crosses that boundary is ordinary INHERITANCE from the `<use>`
  element. Both halves were wrong in the first cut and the symptom was the
  same either way — twelve filled black blobs. `viewbox` is not a CSS
  property.
- **The type ramp is named by ROLE and the page title is 2.07x the body.** At
  the old 22px against a 14px body it was 1.57x, too shallow to anchor a page,
  which is why every screen read as one undifferentiated field of text. The
  uppercase micro-label (`--text-micro` + `--track-wide`) is now ONE idiom used
  by the rail's group headings, the stat-tile labels, table headers and
  `.section-head` — that last one used to be 13px accent-coloured body text
  competing with the panel's own `h2` for the same rank.
- **Stat tiles cap at 320px, not `1fr`.** `auto-fit` collapses empty tracks, so
  a two-card page stretched each tile to ~590px and left the number floating in
  an empty field.
- **The pairing badge moved onto the Devices rail entry** — a count belongs on
  its destination. It is a `<span>` inside an `<a>` now, so it must not paint
  its own hover/focus affordance; the row owns both. The dot is dropped
  visually and the word "pending" is visually-hidden but kept in the accessible
  name, because 248px of rail cannot hold "• 2 pending" beside "Devices".
  **It must carry NO `aria-label`.** Its name comes from the enclosing link's
  content, and accname step 2C says a descendant with `aria-label` contributes
  that string IN PLACE OF its subtree — so the label it shipped with replaced
  the live count with static prose and the row announced "Devices Pending
  pairing requests", dropping the one thing the badge exists to say. Verified
  after the fix: the link reads "Devices 2 pending". `.pairing-badge-label` is
  clipped rather than `display:none` precisely so it survives into that name.
- **The now-playing bar starts where the rail ends**, via `--npbar-left` bound
  to `--sidebar-w` — one token, so the two can never disagree about that edge.
- **A back link is navigation and does not belong in an action row.** The
  collection detail view appended `← Smart Mixes` after Play / Shuffle / Add to
  queue; on a mix, which has a SECOND action row below (Regenerate / Save as
  playlist), it wrapped onto its own line and read as a third kind of button
  sandwiched between the two. It is now a breadcrumb above the title, in a
  shell-owned `#player-crumb` mount filled through `ctx.setCrumb` — it could
  not live in the view, because `#player-title` sits outside `#player-view`.
  **The crumb is cleared in `route()`, up front, not by each view the way the
  toolbar is**: only three views set one, and the failure mode of forgetting is
  a stale `← Smart Mixes` above an unrelated page.
- **`renderCollectionDetail` no longer renders `.detail-title`** —
  `setAxisTitle` had already put the same string in the page's `<h1>`, so every
  playlist and mix printed its name twice. Albums keep theirs, because their
  `<h1>` stays the generic section name.
- **The player's section rail dropped its filled active pill.** It is the
  SECOND rail on screen now, and that idiom belongs to the shell rail alone;
  active is marked with weight and ink instead.
- **Test markers that were really layout proxies had to move.**
  `TestPartialBoostRendersContentOnly` asserted every Server page's fragment
  contained `class="subnav"` — a marker that lived in the LAYOUT, so it failed
  for exactly the reason it exists to catch. Each page now names its own
  content root, which is the stronger assertion anyway: it proves the right
  template rendered, not merely that something did. `primaryNavMarkup` pinned
  the literal `<nav id="primary-nav">` and broke on an added `aria-label`; it
  now matches the opening tag without its bracket.
- **Deliberately NOT changed: the accent palette.** `--accent` (`#856428`) is
  measured against three light surfaces for 4.5:1 as normal-size TEXT, and the
  comment above it records the two lighter candidates that fail. A filled
  button only needs its LABEL to contrast, so a richer fill is available as a
  separate `--accent-fill` token — but that is a colour-direction decision, not
  a layout one, and it was left alone.
---

## Web player: right-sized artwork, catalog freshness, A–Z, collections (PRs #743–#747, 2026-08-24)

Admin-console only — no `/v1` shape change, no `ProtocolVersion` bump, no
migration, no iOS mirror. Five PRs closing the first round of field reports
against the player that shipped in #739–#742.

- **`?size=` is now honoured, and the ladder must verify DIMENSIONS, not the
  filename.** `stampLocalArtwork` writes every local cover under a `-500`
  suffix whatever its real size is — a misnomer since the scaling module — so
  a serve path that matched on NAME answered `?size=250` and `?size=500` with
  the same 600–1200 px file (measured: 191 KB for both; a 12-tile grid pulled
  ~2.3 MB to fill ~0.5 MP). `resolveArtworkTier` reads the header and derives
  when the stored tier overshoots by ≥20%. **Derived tiers live in
  `<artworkDir>/thumbs/`, and that subdirectory is load-bearing three ways:**
  `/v1/artwork` shares the artwork dir and stats the requested size FIRST (a
  sibling file would silently change what iOS receives — verified
  byte-identical before/after with a real bearer token);
  `enrich.CachedArtistImageMBIDs` enumerates that dir to build the coverage
  set; and `RunArtworkRescaleOnce` walks it flat. **Don't extend derivation to
  `/v1` casually** — it reverses the deliberate 2026-08-19 one-tier decision in
  `artwork_scale.go`'s header and wants its own measurement.
  **`EnsureThumb`'s mtime freshness check is REQUIRED, not belt-and-braces:**
  covers are content-keyed (`local-<sha256>`) so a changed cover is a changed
  filename, but portraits live under a fixed `artist-<mbid>.jpg` the enricher
  OVERWRITES IN PLACE. **Decline dimensionally, never by byte length** — a
  downscale is not guaranteed to shrink (measured: a q5 400 px source is 10,897
  bytes and its correct 250 px thumb is 24,648), and the byte form discarded
  valid thumbnails and served the oversized original.
  **Deploy note:** responses carry `immutable` with a one-year max-age, so a
  browser that cached the pre-fix bytes keeps them under the unchanged URL. One
  hard reload; not worth a permanent version token.
- **Catalog staleness has two causes and they get two answers.** An EPOCH
  change means a scan happened, so it still rebuilds SYNCHRONOUSLY ("I scanned,
  I refreshed, my album isn't there" is the complaint catalog.go:74-82
  protects). TTL expiry is only a GUESS that an unnudged writer moved
  something, so the request is answered from the existing snapshot and the
  rebuild happens behind it. Plus a one-shot warm at boot (bgWriters-joined) —
  without it an operator who visits every few hours paid a full fold nearly
  every visit. **All three paths enter through `rebuildCatalog`'s
  singleflight**; the refresher must NOT re-enter `libraryCatalog`, which would
  take its own serve-the-stale shortcut and rebuild nothing (a test caught
  exactly that). `catalogRefreshing` bounds GOROUTINE SPAWNS, not folds — the
  singleflight already coalesces the work, and a test asserting otherwise
  passed with the flag removed.
- **The A–Z rail files through `librarycat.bucket()`, never a fresh copy.**
  `sortkey.go`'s header states it mirrors the iOS `AlphabetScrubber` so "the
  browser's A–Z index and the phone's scrubber" agree — this rail is the client
  that comment was written for. Diacritics already fold (`Édith` → E) including
  the deliberate `ø ł đ æ` gap shared with the phone; **don't close that gap
  here alone.** Buckets ride `playerPageMeta` first-page-only and are OMITTED
  when the ordering isn't alphabetical, which is how the client knows not to
  draw a rail. **The letter depends on the SORT**: `Album.Bucket` derives from
  `SortArtist`, right under an artist sort and wrong under a title sort.
  Genres are ordered by track count, so `axisIsAlphabetical` CHECKS rather than
  assumes — a rail over a count-ordered list misdirects while looking
  authoritative. **A jump is a RESET, not an append** (verified by building the
  broken version: 17 tiles became 22, spliced).
- **`route()`'s dispatch table needs a parity guard of its own.**
  `routes[section] || routes.albums` is a silent fallback, so a head
  `PLAYER_HEADS` claims and the table forgets renders the ALBUM GRID under the
  wrong title with no error — that was true for `genre`, `composer`, `playlist`
  and `mix` from the day the router was written, while
  `TestPlayerHeadsMatchServerRoutes` pinned only the head SET despite its
  failure message promising otherwise. `knownRouteGaps` is now EMPTY and the
  emptiness is the assertion; keep the map so a future gap is a visible line.
- **A collection is not an album.** `trackList`'s `collection` mode drops disc
  headings and numbers rows by position: a playlist spanning six albums was
  otherwise punctuated with "Disc 1" and numbered 3, 1, 8, 2. **Foreign
  playlist members** (another bridge's `origin_fingerprint`) are COUNTED and
  REPORTED, never silently dropped — hiding them makes the page disagree with
  its own tile and the operator's Data page. **Mosaic covers filter on artwork
  presence BEFORE deduping to four**, or an artworkless album consumes a
  quadrant and renders as a hole.
- **`csrfGuard` does NOT 415 a bodyless POST** — it gates the Content-Type
  check on `ContentLength != 0` and explicitly lets empty bodies through
  (admin.go). The house convention of always sending the header still applies;
  it becomes a REQUIREMENT only once a body is sent. Recorded because a plausible
  misreading sends the next reader chasing a 415 that cannot occur.
- **Palette: `--accent` is `#856428` (light) / `#d9bc7a` (dark), and the value
  is contrast-driven.** `--accent` colours normal-size text, so it must clear
  4.5:1 on all THREE light surfaces: `#9a7b3f` measures 3.98/3.80/3.63 (fails),
  `#8e6f32` 4.70/4.49/**4.29** (fails on `--bg-elev`), `#856428`
  5.45/5.21/4.98. New **`--accent-on`** token is the foreground for text on an
  accent FILL, because the readable choice flips with the theme; the
  `color: #fff` it replaced was ALREADY failing at 2.98:1 on indigo-400.
- **Windows CI catches wall-clock test assumptions.** Two `time.Now()` stamps
  milliseconds apart are not reliably ordered under Windows' ~15.6 ms
  granularity — the same trap as the `indexed_at` bump. Assert on counted
  events, and detect "was this file rewritten?" by CONTENT (a planted
  sentinel), never by comparing mtimes: two writes in one tick leave them
  equal, so an mtime check silently PASSES on the platform most likely to break.
---

## Variant management moves into Browse; the Inspector is retired (2026-08-24)

Admin-console only — no `/v1` shape change, no `ProtocolVersion` bump, no
migration, no iOS mirror. Five PRs. The Library Inspector page is gone; its
URLs 301 to the views that replaced it.

**Reading the older entries above:** several name the Inspector as the consumer
of a query or an invariant — the `topLevelFSFolderSource` root-browse
derivation, the byte-range prefix rollups, the eligible-denominator coverage,
`ListTrackProjectionsUnderPrefix`'s bind order. Every one of those is STILL
LIVE and unchanged; only the caller moved, to the player's Folders view and the
album/artist panels. Don't read "the Inspector does X" as dead text.

- **An album is a SET of tracks, never a path prefix — this is the load-bearing
  fact.** `librarycat.Album.FolderPath` is `commonDir()` of its tracks, and
  measured on the reference library **69 of 880 albums (7.8%) share that
  directory with another album**; `2go/Music/Peter Gabriel/Hi-Res Masters/`
  alone holds **18 albums flat**. So a prefix submit for such an album enqueues
  every neighbour and a prefix delete reclaims their sidecars. A single track is
  the mirror image: `subtreeLikePattern` builds `<base>/%`, which matches strict
  DESCENDANTS, so a file path projects zero rows — which is why the Inspector's
  per-track "Generate variants" menu item never had anything to enqueue, for its
  entire life. **Don't re-express an album or artist action as a path prefix.**
  Pinned end-to-end (store, coordinator, admin, api) by fixtures that stage two
  albums in one directory and assert the neighbour is untouched.
- **Identity scopes travel as IDS and are expanded SERVER-SIDE against the
  catalog.** `POST /api/upscale/batch` takes `albumIds` / `artistId` /
  `trackPaths`; `DELETE /api/upscale/variants` takes `?albumId=` / `?artistId=`.
  There is deliberately **no `album_id` column to expand against in SQL** —
  album identity is `dupes.AlbumIDOf(dupes.Resolve(row))`, folded on read (see
  `librarycat/doc.go`) — so the expansion goes through `Album.TrackPaths`. That
  also keeps a 3,000-track artist to one 16-hex id on the wire. The folder form
  (`path`, `""` = whole library) is untouched and remains the default.
- **A present-but-EMPTY scope must never read as an ABSENT one.** Absence means
  the folder form, and an empty folder path means EVERYTHING. `{"albumIds": []}`
  would have upscaled the whole library; `?artistId=&confirm=true` would have
  cleared the entire variant cache (`RunVariantDelete`'s own shape guard refused
  it, so it was a 500 rather than a wipe — but the handler must not build that
  request). `encoding/json` distinguishes an absent field from `[]`, and the
  query parser keys off parameter PRESENCE, not value. **The test for this is
  worthless without `confirm=true` in the table** — without it a blank identity
  that slipped the presence check still 400s, on the unscoped form's own confirm
  gate, for an unrelated reason.
- **Coverage bars read against an ELIGIBLE denominator, and a STALE copy stays
  in COVERED.** The batch walks skip any track that has a variant of the kind,
  freshness unread, so reporting a stale sidecar as missing would show an
  enabled Generate that enqueues nothing. It is counted separately and named
  with its remedy (delete, then generate). Freshness compares the variant's
  stamped facts against the SCANNER's record (`tracks.mtime_ns` + `size`), not a
  live stat — the `autoOptimizeCandidateSQL` definition, which costs no
  filesystem access and is the only one available for a routed row. The playback
  path keeps its own live-stat check (`variantFresh`); don't unify them.
- **The album grid's coverage snapshot is NOT in the catalog, and both halves
  have their own reason.** ELIGIBILITY could be (it changes only on a rescan)
  except that it depends on the runtime upscale TARGET, which the catalog knows
  nothing about — so the snapshot keys on `(epoch, rate, bits)` and the target
  write drops it. COVERAGE cannot be cached there at all: the auto-optimize
  sweeper writes variants continuously and does **not** bump the catalog epoch,
  so a baked mask would tell the operator an album still needs work it finished
  minutes ago. The `needs` filter is why a whole-library snapshot exists rather
  than a per-page query — filtering a page draws page 1 of the filtered list
  from page 1 of the unfiltered one and reports a total for the wrong set.
- **The live refresh keys on the pool's DONE + FAILED counters, NOT a busy→idle
  edge.** The edge is the obvious choice and it is wrong: a two-track batch on
  two workers starts and finishes between two SSE frames, so the client never
  observes `busy` and the edge never fires (reproduced every time — server 2/2,
  panel 0/2 until a manual reload). Frames are diff-suppressed, so a completion
  always produces one. The queue-empty frame bypasses the client's throttle;
  everything else is throttled, or a projection query lands on a 2 Hz timer.
  Generate deliberately does NOT refresh on its own response — the work has only
  been queued, and re-rendering destroys the status line just read.
- **The whole-library variant clear lives on Roots behind a typed `CLEAR`, and
  the Browse panels refuse to offer it.** Exact match, not prefix or case-fold —
  that looseness is what made the old bare `[y/N]` uninstall prompt a fat-finger
  hazard. The click handler re-checks rather than trusting `disabled`, which a
  programmatic click walks past. **Don't add a whole-library delete to a
  coverage-bar panel.**
- **`/library/inspector` 301s, and `?camelot=` goes to `/tracks`, not
  `/folders`.** A harmonic key is not a place and a folder tree cannot filter by
  one; the Smart Mixes wheel deep-links a single Camelot code, so it gets a
  key-filtered track list backed by the existing `GET /api/library/browse?camelot=`.
  A new player route must be registered in **all three** of `playerRoutes`,
  `PLAYER_HEADS` and boot.js's `routes` table — the parity tests pin each pair.
- **Deleting scattered CSS needs a SELECTOR-SET diff, not a brace scan.** Two
  near-misses in one pass: a comment containing a literal `{` (`.hint { display:`
  quoted in prose) made a naive scan swallow the following rule, which was the
  global `[hidden] { display: none !important }`; and `status-*` / `pairing-*`
  classes are composed dynamically (`class="status-${r.status}"`), so absence of
  a string literal does not mean dead. The safe shape is: collect every selector
  before and after, and refuse to write unless every disappearing selector
  mentions a proven-dead class and nothing new appeared.
- **A server-rendered panel driven by runtime ids fails SILENTLY when the two
  drift** — the JS resolves null, every guard returns early, and the controls do
  nothing. `roots_variants_panel_test.go` pins both directions and immediately
  found a span rendered as a count that nothing filled. Its first version matched
  only `getElementById`, which covered a handful of the ids while appearing to
  cover all of them, because most wiring goes through the shared `setText`
  helper — **match any `"prefix-…"` literal, not one call shape.**
---

## Detail tabs, smart mixes on the player, and three parity tests (PRs #754 / #755 / #756, 2026-08-25)

Admin-console only — no `/v1` change, no `ProtocolVersion` bump, no migration,
no iOS mirror. The user-visible half is small; the durable half is that **three
separate classes of silent breakage were live at once**, and each now has a test.

- **Album and artist detail are TABS** (Tracks / About / Variants; Albums /
  About / Variants), default to the track list or discography. Stacked, the
  Atlas About card and the variant panel pushed the first track most of a
  screen down — on a one-track album with nothing to generate, entirely below
  the fold. **Nothing is lost by hiding the coverage**: the album tile carries
  its variant badge and every track row carries its own marks, so "does this
  have CarPlay copies" is still answerable from the default tab. A tab whose
  panel would be empty is DROPPED (no Atlas entry → no About tab), and an empty
  track list or discography renders an **empty state rather than a blank
  panel** — omitting the primary tab would leave the page's content absent with
  no explanation.
  **The tab choice survives a re-render and is keyed on the SUBJECT.** The
  album page re-renders itself whenever a variant job lands or a delete
  completes — which is exactly when the reader is on the Variants tab — so
  without the memory every one of those bounced them back to Tracks
  mid-operation. One slot, not a Map: only the current view can be re-rendered.
  The tab buttons reuse app.css's `.tab-btn` (the Settings idiom) rather than
  restyling; the variant panel is compacted independently (title · ratio ·
  both buttons on one line above the bar, and no permanently-blank status row).

- **`.small` had a rule in NEITHER stylesheet, and had ridden ~25 nodes since
  the player was written** — every stat line, every variant note, the About
  label and attribution. Everything asking to recede rendered at the body's
  13.5px, which is a real part of why those panels felt heavy. Now bound to
  `--text-sm`. `TestPlayerEmittedClassesAreStyled` found it and now guards it:
  the player builds every node in JS, so nothing connects an emitted class to a
  rule. It also **records which classes are BORROWED from app.css** (`btn`,
  `muted`, `section-head`, `tab-btn`) — those make an app.css-only rule
  load-bearing for a file that never mentions it. **Strip CSS comments before
  scanning**: this repo's commentary names the classes it discusses, and a
  comment mentioning `.tab-btn` read as a definition — a false pass in the one
  direction that matters.

- **`dispatchPageInit` dispatched on a tab name no page rendered, and five
  controls were dead for two days.** PR #739 renamed the operator dashboard's
  page key `"dashboard"` → `"stats"` and left `case "dashboard": initDashboard()`
  behind. Every lookup inside is nil-guarded, so nothing threw — the function
  simply never ran. Dead on Stats: **Scan now**, **Which tracks?**, **Retry
  missing**; dead on Settings: **Check now**, **Roll back** (the same function
  wired them, from when the two pages were one). Confirmed in a browser before
  fixing: clicking *Scan now* fired no request and did not even disable the
  button. The update-panel wiring moved to `initSettings`, where the panel has
  lived since #129. `TestEveryPageTabHasAnInitCase` pins BOTH directions — a
  page with no case, and a case naming a page that no longer exists — reading
  the tab list off `Server.pageTmpls` rather than a second hand-written copy.
  **When you rename a page key, grep the dispatch switch.**

- **`humanBytes` was deleted out from under nine callers.** #753 retired the
  Library Inspector, deleted 3,362 lines of app.js including
  `function humanBytes(n)`, and left every caller. `applyStats` threw on EVERY
  SSE stats frame on every page; the Roots page's `variants-free` sat on its
  em-dash placeholder; and because the throw happens eight statements earlier,
  `clear.disabled = !s.usedBytes` never ran — leaving "Clear all variants…"
  live on an empty cache, on the one button in that panel guarded by a typed
  phrase. All nine now call `formatBytes`.
  `TestAppJSHasNoCallsToDeletedHelpers` is the guard. **It is a heuristic, not
  a parser, deliberately**: Go cannot parse JavaScript and this project will
  not grow a Node build step for one file. It strips comments and string
  literals, collects bare `name(` calls, and collects declarations across the
  forms this codebase uses (function, const/let/var, object property, method
  shorthand, arrow params, array AND object destructuring, ES imports), with
  two small documented allowlists — browser globals, and locals the scan cannot
  see. **A name in neither IS the bug**, and the failure message says so.

- **The sidebar's Smart mixes entry points INTO the player, which needs a
  discriminator in THREE places.** `/smartmixes` is retired (301 → `/mixes`,
  matching the Inspector); the harmonic-coverage wheel moved to Stats (a fact
  about the library, not a control; hidden when nothing is analyzed). Because
  every player route renders the `player` tab AND the `player` section,
  tab-or-section matching cannot tell `/albums` from `/mixes` — both entries
  would light, and `TestPrimaryNavHighlightsEveryEntry` requires exactly one.
  `pageData.PlayerNav` (from `playerNavEntry`) is the answer, applied at first
  paint in layout.html, on a boosted swap via the **`X-Bridge-Player-Nav`**
  header, and in boot.js's `updateSidebarNav` for the navigations that never
  reach the server. `boostUpdateTopNav` skips `data-player-section` entries in
  its tab pass, so it depends on neither markup order nor `route()` running
  afterwards — verified by stubbing `window.__player` and boosting into
  `/mixes`. `TestPlayerNavEntriesMatchTheLayout` pins the Go ↔ template ↔
  boot.js triple. **`updateSidebarNav` compares `dataset.playerSection` as a
  VALUE**: `section` is a path segment off `location.pathname`, and
  interpolating it into a selector lets a URL with a quote throw a
  DOMException that takes `route()` — and the whole render — down with it.

- **Operator affordances ride the mix, not a separate page**: the grid gains
  "Regenerate all", mix detail gains Set / Replace / Remove cover, and
  **playlist detail gains the same control** — `POST /api/playlists/{id}/cover`
  had existed since the covers work landed with NO caller, because the only UI
  ever built was the smart-mix half on the page that is now gone. The file
  input is `sr-only`, never `[hidden]`: `[hidden]` is `display:none !important`
  (app.css:955), which drops it from the tab order, and a `<label>` is not
  natively focusable — the label paints the ring via `:focus-within`.

- **PROCESS — stacked PRs get NO CI in this repo.** `gate.yml` and `gofmt.yml`
  (and codeql) are `pull_request: branches: [main]`, so a PR targeting a
  feature branch runs **only SonarCloud**. The documented stack-and-batch
  workflow therefore ships its children un-gated until they are retargeted to
  `main`. Retargeting alone does not fire the workflows either — the base
  change is not a `synchronize` event — so **amend for a fresh SHA and
  force-push after retargeting**, then wait for gate/gofmt/codeql before
  merging. Two other traps hit in the same session: capture each child's fork
  point BEFORE amending its parent (an amend changes the SHA `--onto` needs),
  and `[hidden] { display: none !important }` already exists at app.css:955 —
  a review finding claiming otherwise is a false positive, verifiable in
  seconds by reading a computed style.
---

## Favorites on the player, and the router races review found (PRs #757 / #758 / #759, 2026-08-25)

Admin-console only — no `/v1` change, no `ProtocolVersion` bump, no
`PROTOCOL.md` change, no iOS mirror, no migration. The user-visible half is
small; the durable half is two router invariants and a test that had never
run on one platform.

- **`/api/favorites` and `/api/player/favorites` are deliberately two
  endpoints.** The first serves the stored backup document verbatim — raw
  display strings, no album ids, no artwork, no playability — which is the
  right answer to the operator's question and is what the Data page reads.
  The player's joins it against the catalog. **Don't widen the operator one
  into something that serves both**; they answer different questions and the
  operator's is a faithful dump by design.
- **A hearted album resolves by the catalog's OWN identity, and there is no
  looser fallback — on purpose.** The wire stores album favorites as the
  display triple `(albumArtist, album, year)`, which is exactly the input to
  the client's album identity, and `internal/dupes` mirrors that identity
  verbatim; so `favoriteAlbumID` is `librarycat.HashID(dupes.AlbumIDOf(...))`
  — the same key the builder stamped onto `Album.ID`, not a resemblance test.
  A miss means the album is genuinely not in this library and is reported as
  unresolved. **A second, fuzzier match would attribute a heart to the wrong
  record while looking like it worked**, and the whole point of the mirror is
  that album identity has ONE definition on both sides.
  `TestFavoriteAlbumIDMatchesTheCatalogsOwnIdentity` compares against the
  catalog's own ids rather than a hard-coded digest, so the two derivations
  cannot drift unnoticed.
  **Test-fixture lesson worth generalising:** the no-fallback test initially
  PASSED against a build with the fallback added, because its fixture (a heart
  with a year, against a library album with a DIFFERENT year) misses under the
  fallback too. Only the other direction catches it — a heart carrying a year
  the library lacks, where retry-without-the-year FINDS the album. When
  pinning the ABSENCE of a rule, build the fixture the rule would actually
  fire on, and negative-control it.
- **`route()` MUST call `abortReads()`; `getJSON`'s per-key abort is not a
  substitute.** The per-key abort cancels a second request under the SAME key
  — paging inside a view, a search keystroke — and does nothing across views.
  Every render awaits its fetch and then `clear(view)`s, so a slow read from
  the view the reader LEFT paints over the page that replaced it. Measured,
  not theorised: delay `/api/player/favorites` by 3s, open `/favorites`, go to
  `/albums` 150 ms later — URL and `<h1>` said Albums while the body was the
  Favorites tab strip with six favorite tiles. Fixed once at the router rather
  than eleven times in the views (every render already returns on
  `isAborted`). **Scoped to READS deliberately** — `postJSON`/`deleteJSON`
  carry no signal and never enter that map, so a generate or a delete survives
  a navigation, which is what an operator who pressed the button and walked
  away expects. **Don't move the call into the views, and don't extend it to
  the mutations.**
- **The post-render scroll restore is generation-guarded, and the guard became
  REQUIRED the moment `abortReads` landed.** An abandoned render now reliably
  reaches route()'s `.then` (its fetch rejects, the view returns on
  `isAborted`, the promise resolves), so it would apply ITS route's scroll
  offset to the page that replaced it. The comfortable assumption — "the stale
  scroll lands first, so it is a flicker" — is WRONG: instrumenting
  `window.scrollTo` through the same repro gave `[{top: 0}, {top: 640}]`, the
  stale offset LAST, leaving the reader 640px down a page they had just
  opened. One comparison against the generation counter, the `chunkAppend`
  idiom. Verified in both directions, including that an un-superseded route
  still restores.
- **The folder variant panel's open state is in memory, and must NOT become
  `sessionStorage`** (suggested on review, declined). The player's own
  analogue — `detailTabs`' `tabMemory` — is in memory for the same reason: a
  fresh load opens on the default, and here the default (collapsed) IS the
  change. `app.js` persisting the SETTINGS tab is not a precedent; that is a
  navigation position, so losing it lands you somewhere you were not, whereas
  losing this only re-collapses a tool one click away. Remembering it at all
  is load-bearing: the panel re-renders when generated variants land, so
  without it Generate would collapse the panel reporting its own result.
- **A `\n`-literal scan of a static file in a test MUST normalize CRLF
  first.** `TestEveryPageTabHasAnInitCase` located the end of
  `dispatchPageInit` with `strings.Index(body, "\n}\n")`; there is no
  `.gitattributes` pinning `eol`, so a Windows checkout carries CRLF and that
  terminator is not in the bytes at all (LF: offset 782; CRLF: -1). It failed
  on `windows-latest` **from the day it was added**. Fixed at the read
  (`strings.ReplaceAll(..., "\r\n", "\n")`) rather than by adding
  `.gitattributes` — pinning `eol` repo-wide rewrites every contributor's
  working tree and is a separate, deliberate policy call. Regex-based scans
  were already fine (`\s` matches `\r`); this was the only such site, proven
  by converting all 22 `static/`+`templates/` files to CRLF and running the
  package.
- **PROCESS — `test (windows-latest)` is NON-BLOCKING in `gate.yml`, so a red
  Windows leg hides behind a green `gate`.** That is how the above went
  unnoticed for a day on `main`. **When a Windows-only failure is suspected,
  read the per-JOB conclusions** (`gh api .../runs/<id>/jobs`), not the run's
  conclusion — and treat a permanently-red leg as urgent for a second reason:
  it masks any genuine Windows regression that lands behind it, so no PR's
  Windows signal is readable until it is fixed. Simulating it locally is
  cheap and conclusive — convert the static files to CRLF, run the package,
  convert back.
- **`TestWatcherShutdownDrainsInflightScan` (`internal/manifest`) is a KNOWN
  Windows timing flake — re-run before investigating.** It is built on sleeps
  (100 ms for the per-dir watches to register, 60 ms for a 20 ms debounce to
  fire, then cancel), and on Windows fsnotify registration is slower and the
  clock granularity is ~15.6 ms, so the debounce can miss its window and no
  scan is in flight to drain. Observed 2026-08-25 on a PR touching ONLY
  `internal/admin` (job 97791984802); a re-run of the identical commit passed,
  and the same manifest code passed on two other PRs the same day.
  **This compounds with the non-blocking leg above**: a leg that is both
  non-blocking AND intermittently red is one a reader learns to skip, which is
  precisely how a genuine Windows regression would land unnoticed. **Rewritten
  2026-08-25 (PR #761) — it was also VACUOUS**: with `wt.scanWG.Wait()`
  removed, the very regression it names, the sleep-based shape passed 10 runs
  out of 10, because a sleep long enough to be reliable also let the one-file
  scan FINISH before the cancel. It now parks a dispatch at its tail via
  `Watcher.afterDispatchHookForTests` — after `ScanSubtree` returns but before
  the deferred `scanWG.Done()`, the only window in which a dispatch is provably
  in flight — cancels, and asserts `Run` has not returned. The seam is a FIELD
  and not a package var for the reason `Pool.jobTimeout` is: as a package var
  it raced under `-race`, a dispatch goroutine from an EARLIER watcher test
  reading it against the next test's write. **And in a test whose subject is a
  shutdown deadlock, every channel receive in the FAILURE and CLEANUP paths
  needs a bound** — two review rounds each caught one unbounded wait that would
  hang the binary instead of reporting the failure it had just detected.
- **A `.track`-style CSS grid must not hardcode a column count when its
  children are conditional.** `trackRow` appends an extra unplayable-reason
  chip only for a track the browser cannot decode, so the row has 7 or 8
  children; a seven-column template put the eighth on a second row and doubled
  every DSD row's height (shipped, field-reported). The fix is
  `grid-auto-flow: column` — implicit trailing columns, so the count cannot
  disagree — NOT widening the template, which works until the next conditional
  child. `TestTrackRowGridHoldsEveryConditionalChild` pins it and accepts
  either shape.
- **A track-list row is a SUBGRID of the list, and the reason cell is always
  emitted** (PR #762). Per-row grids cannot align: a grid sizes its tracks from
  its own content and knows nothing about its siblings, so format/size/duration
  sat at a different x on every line (measured spread up to 202px). `.tracks` is
  the grid; each `.track` spans it with `grid-template-columns: subgrid`.
  **NOT `display: contents` on the row** — that shares the parent grid too, but
  a contents-display `<li>` has no box, so the row loses its hover background,
  its bottom border and its padding, and it has a history of dropping list
  semantics for assistive tech. **Keep the `@supports` guard**: an engine
  without `subgrid` treats the declaration as invalid, which would leave
  `.track` with no template at all and stack every cell vertically — guarded, it
  simply keeps the per-row base rule. The always-emitted `.track-why` is what
  makes the cell count constant (a shared grid cannot align a 7-cell row with an
  8-cell row); it is `display: none` when `:empty` at the top level and on
  mobile, where the columns are NOT shared and it would only add a gap, and
  reinstated inside the `@supports` block where the constant count is the whole
  mechanism.
---

## Playlists consolidate into Browse; per-page feature trays (2026-08-26)

Admin-console only — no `/v1` shape change, no `ProtocolVersion` bump, no
`PROTOCOL.md` change, no iOS mirror, no migration. Two halves: the console
stopped showing playlists (and favorites) in two places, and every togglable
feature grew a switch on the page that reports on it.

- **`/data` is retired; `/history` is the page, and it carries telemetry
  ONLY.** Playlists and favorites duplicated the player's own views, so they
  moved to Browse and the page kept its name honest. 301 from `/data`
  (bookmarked for the console's whole life; the Inspector / Smart Mixes
  precedent). The tab key, the template and the `.page` class all moved to
  `history` together — `handlers_partial_test.go`'s per-page content marker
  and `TestEveryPageTabHasAnInitCase` both key on that name, so a half-rename
  fails loudly.
- **A consolidation must carry the facts the retired surface had, or it is a
  loss dressed as a tidy-up.** Three things moved with the playlists rather
  than being dropped: **provenance** (`deviceName` / `deviceTokenPrefix` /
  `updatedAt` on `playerCollectionDTO`, resolved server-side by
  `deviceNamesByToken` — a backup listing whose whole point is "is this device
  still syncing" needs the date); **`unresolvedItems`**, which NAMES the
  members that could not be hydrated and says which are another bridge's and
  which are simply gone (the count alone was all the player had); and
  **export** (JSON / CSV / M3U8), which reuses `/api/playlists/export`
  unchanged rather than growing a second set of writers — the M3U8 one
  newline-flattens every device-supplied field against playlist-line
  injection, and a twin would be a second place to get that wrong. Favorites
  got the provenance line; its unresolved entries stay a count, deliberately
  (a heart that lives on another bridge is not a document you repair).
  A smart mix carries none of it — it is generated here, so the fields are
  absent and `omitempty` drops them; pinned by
  `TestPlayerMixesCarryNoProvenance`.
- **`device` on `/api/playlists/{detail,export}` is OPTIONAL, not unchecked.**
  It was only ever a consistency check on a fact the caller already had — the
  read has been id-scoped since playlists stopped being device-scoped in v1.7
  — and the player's playlist page has no prefix to send. Blank skips the
  check; a supplied-but-wrong prefix still 404s, which is what keeps the guard
  meaningful for the callers that do pass it.
- **`unresolvedPlaylistItems` SUBTRACTS the hydrated paths rather than
  re-deriving the drop rule.** `hydrateTracks` skips a path for reasons this
  function has no business knowing (deleted since the snapshot, newly
  duplicate-suppressed), and a second copy of that judgement would disagree
  with the count beside it the first time either changed. The COUNT is exact;
  only the list is capped (`maxUnresolvedListed`).

**Per-page feature trays** — a gear beside a heading that opens that feature's
switches in place. The Duplicates page has had its serving policy inline since
it shipped; everything else had its status on one page (Jobs, Smart mixes,
History, an album's Variants tab) and its switch on another.

- **No new endpoint, deliberately.** `PATCH /api/settings` is already a partial
  update with pointer fields, so a tray sends only the field it owns and the
  server's own hot-apply / restart rules answer for it. A tray must never be
  able to mean something different from the same control on the Settings page.
- **`TestEveryFeatureTrayFieldExistsOnBothSettingsStructs` is the load-bearing
  test.** A row names its settings field as a STRING, and `encoding/json`
  DROPS an unknown field — so a typo saves nothing, the handler answers 200,
  and the tray reports "Saved." while the operator watches a switch move. It
  walks every `.js` under `static/` rather than a hand-listed set, because a
  tray added to a fourth file would otherwise be silently unchecked — the same
  forgot-the-list failure the test is about.
  `TestFeatureTrayRestartBadgesAgreeWithSettings` pins the badges against the
  Settings page field-by-field (the badge is a PREDICTION; the authoritative
  answer is `restartRequired` on the response, which the tray reports either
  way — so two surfaces predicting differently is the failure worth catching).
  `autoOptimizeEnabled` is the field it catches: it hot-applies and carries no
  badge on either surface.
- **Trays are INLINE disclosures, not anchored popovers.** A popover needs
  viewport clamping, a z-index in the ledger, outside-click dismissal and its
  own phone layout; a panel that expands under its heading needs none of those
  and cannot be clipped by the card it lives in.
- **The gear goes into the head's `.panel-actions` when there is one.**
  `.panel-head` wraps and a job card in the two-up grid is ~320px: a gear
  added as a third child of the head dropped to a line of its own at the LEFT
  edge, below the heading and nowhere near the controls it belongs with.
- **The heading takes the slack with an auto margin, NOT a `justify-content`
  flip.** `.jobs-page .job-card .panel-head` is (0,3,0) and beats a two-class
  `:has()` selector silently. An auto margin competes only with
  `.panel-head h2` (0,1,1).
- **`@container`, not `@media`.** A job card is ~320px wide on a 1400px
  screen, so a viewport query never fires for the one case that needs it.
- **The snapshot is warmed at MOUNT, not at first open.** Controls start
  disabled and unchecked, so an open that waits on the fetch shows every
  switch briefly OFF — telling the reader the wrong thing about their own
  configuration. One request per page; all trays share the promise (the Jobs
  page mounts nine).
- **`window.BridgeFeatureTray`** is how the player module reaches it: app.js is
  a deferred classic script and the player is an ES module, the same one-way
  window handshake `window.__player` uses in the other direction. Guarded at
  every call site — a missing app.js must cost the gear, not the view.
- **History gets a tray with NO server-side switch, and that is the honest
  answer.** Playlists / favorites / history are deliberately ungated
  bridge-side (2026-08-14 feature review, P2-38); the tray says so and points
  at the per-device toggle in the app, rather than inventing a gate that would
  shadow the iOS one. The Duplicates page keeps its inline fieldset — a gear
  there would be two controls for one field on one page.
- **Smart Mixes now stays in the player's section rail when the feature is
  off.** Skipping it was coherent while the off-state said "enable this in
  Settings"; the page is where the switch IS now, so hiding it left no way to
  discover the feature from inside the player while the shell sidebar listed
  it two inches away.

**Two pre-existing bugs surfaced by loading the page, not by review:**

- **`player.css`'s bare `.rows { display: flex; content-visibility: auto }`
  had been hijacking every operator TABLE since the player shipped.** app.css
  styles them as `table.rows` and sets no `display` at desktop width, relying
  on the browser default; player.css loads second and won. `content-visibility`
  on a table with no `contain-intrinsic-size` then collapsed it — 50 rows of
  listening history rendered as blank space under the heading. Now
  `.rows:not(table)` (excluded by ELEMENT, so a future player list built as a
  `ul` still gets the rule), pinned by
  `TestPlayerCSSDoesNotHijackOperatorTableClasses`, which flags any bare class
  rule in player.css that app.css qualifies with an element.
- **`historyEventDTO.DeviceName` is the OUTPUT hardware (the DAC), not the
  phone.** The two differ by one word and mean completely different things,
  and the new "Device" column was wired to it on the reasonable-looking
  assumption that it named the source device — every row read "—" until a real
  DAC name appeared, at which point it would have named the wrong thing
  confidently. `SourceDevice` (from the roster LEFT JOIN that has always been
  there) is the phone; both are on the wire now, and the output hardware rides
  the Route cell's title.

**Verification note.** Both of the above, and four layout defects, were found
by seeding a throwaway copy of a real bridge DB with playlists / favorites /
history and driving the console in a browser. `go test` was green through all
of them. For anything with no data on a dev box, a `_`-prefixed seeder
(invisible to `./...`) writing through `internal/manifest` is the cheapest way
to get a page that renders something.
---

## Settings apply live where it is structurally sane; the rest is reported PER FIELD (2026-08-28)

`PATCH /api/settings` used to answer with one blanket `restartRequired` boolean.
Sixteen of twenty-six fields now apply live, and every supplied field reports its
own outcome. **The field → apply-semantics matrix lives in
`ops/settings-apply-semantics.md`** and is the contract the planned cloud control
plane reads; `TestMatrixDocMatchesWhatTheHandlerReports` drives the real handler
for every row in it, so the doc cannot silently drift from the code the way the
WAV/AIFF extractor claim above did.

Admin-only throughout: no `/v1` wire-shape change, `ProtocolVersion` untouched.
The `/v1/health.features` CONTENT does change mid-process now (that is the point
of a live feature gate), which is allowed and observable to iOS — it is not a
shape change and needs no mirror.

**Four rules a future change must not undo:**

1. **Never split a field's halves.** Either EVERY consumer of a config field
   reads it live, or every consumer takes it at boot. Hot-applying a cheap struct
   field while reporting `restart` makes `/v1/health` advertise a capability in
   the same breath the settings response calls the change pending. This is why
   there is no `partial` status — the rule removes the case instead of naming it,
   and it is why `atlasEnabled` stays wholly boot-bound (an API field it could
   convert in a line, plus a file-backed harvest state store it cannot).
   `TestSmartPlaylistsHealthFlagAndEndpointMoveTogether` is the pin.
2. **When a change cannot take effect, say so.** The rule `autoOptimizeEnabled`
   established, generalised: `reason` is populated exactly when the OUTCOME
   depended on THIS bridge's runtime state — no sweeper wired (`restart` +
   reason), or applied-but-inert like `fingerprintEnabled` on a host without
   fpcalc (`live` + reason, because a restart would change nothing). NOT for
   "listeners bind once", which is true everywhere; twenty near-identical strings
   is how the two that carry information get skipped. The verdict is computed
   INSIDE the `CfgHolder.Update` closure and never derived afterwards from a
   static table — a table cannot see this bridge's wiring.
3. **A cadence provider needs a rearm, or `live` is a lie.** Every loop reads a
   `func() time.Duration` before each wait (a timer per iteration, not a ticker —
   a ticker cannot change period), and `Deps.TriggerCadenceRearm` wakes them on
   change. Without it, a shortened 6 h interval is not read until the old one
   elapses, which is indistinguishable from being ignored. **The rearm is NOT a
   work-nudge** — it re-reads the schedule and never runs the work — and it fires
   only on an actual change, because it restarts the wait. `interval() <= 0`
   PARKS a loop rather than ending it, and the backup ticker is started
   unconditionally; the old early-return made "disabled" terminal for the process
   so `0 → N` had no loop alive to notice.
4. **The enrich pacing travels with the base URL.** `MinInterval()` re-derives
   from the same live value. A live base with a frozen interval is the one
   mistake here that reaches a third party: clearing the mirror URL starts
   calling public MusicBrainz at the self-hosted 150 ms, ~6.7 rps against a
   service that asks anonymous clients for one. The base/interval straddle is
   safe by construction — the gap is measured since the last request to the OLD
   host, so the new one sees its first request with no prior traffic.

**The split that made the feature gates convertible:** WIRED (a boot fact — the
pool exists, the toolchain is present) vs ACTIVE (the operator's toggle). Wire
the subsystem unconditionally, gate it on ONE shared live predicate read by every
consumer. Three copies of the same gates is how a card claims "active" while
every sweep short-circuits. A disabled pass records NO status — a "last run"
timestamp for work that never happened is worse than a stale card.

**Deliberately left restart-bound, don't "finish the job":** the transcode and
analyze pools (enqueue-under-lock / Stop ordering / publisher drain — the
invariants that produced live panics), the fsnotify watcher (its `scanWG` /
`closing` drain guards a SQLite-corruption vector, and that drain's test was once
VACUOUS), DLNA (a cloud tenant has no LAN), and the two listener binds. Reopen
the watcher only if tenants mount storage after boot.

**`updateAutoInstall` is symmetric on purpose.** An asymmetry hot-applying only
the OFF direction was considered and rejected as a hidden state machine. The ON
direction cannot surprise anyone: `maybeAutoInstall` runs ONLY from the poll loop
(never the admin "Check now" path), the cadence floor is 1 h with a 6 h default,
and the install still clears quiet-hours and the in-flight sessions gate —
re-checked AFTER the download. The install opts + restart callback are now wired
unconditionally; they are inert unless the gate is on, and boot-gating them was
what would have made the toggle a lie.

**Process note worth keeping:** the badge-parity test added in the first PR
(`TestTrayBadgesAgreeWithWhatTheServerReports` — UI badge vs what the SERVER
reports, not the older two-predictions-agree test) caught all eight stale
`restart` badges as the fields converted, across three PRs, with no manual sweep.
When converting a field, expect it to fail and drop the badge; that is the
mechanism working.

**…and the hole it left, which shipped.** Both tray badge tests iterate TRAY
rows and consult the Settings page only for fields a tray happens to contain, so
a Settings-page-ONLY field is invisible to both. `updateQuietHours`,
`fingerprintApiKey` and the derived `enrichSource` picker kept stale `restart`
badges through all four conversion PRs and reached production telling the
operator a bounce was needed that was not — found by looking at the live console,
not by any test. `TestSettingsPageBadgesAgreeWithWhatTheServerReports` walks the
PAGE's own controls and closes it. **A UI-only control inherits the semantics of
what it writes** (`uiOnlyControls` maps `enrichSource` to the two enrich base
URLs); there is deliberately no `enrich.source` config field, so nothing else
connects that control to an apply semantic.

**Then a THIRD surface: the description prose.** Apply semantics are stated in
the badge, the hint text, AND the server's report. `optimizeEnabled` lost its
badge when the field went live but kept a sentence reading "wired at startup, so
a change takes effect after a restart" — the same wrong answer, moved somewhere
nobody was checking, and a reader who trusts prose over a chip gets it.
`TestSettingsProseDoesNotContradictTheBadge` flags any field whose hint claims a
restart while its badge says otherwise. **Collapse whitespace before matching
prose in this template** — hints wrap across lines, and the first version of that
test passed against the very sentence it was written to catch because the phrase
straddled a newline.

Generalisable lesson: when several surfaces state the same fact, a test that
walks ONE of them proves nothing about the others — walk each from its own side,
and count the surfaces before assuming there are two.
---

## Cloud-readiness batch (PRs #797–#803 + iOS #1480, 2026-08-30)

Came out of a page-by-page walk of the live public console. Seven bridge PRs,
one iOS doc mirror. `ProtocolVersion` stays 1. Deployed to bridge.ars.md as
`v0.1.9-73-g166daf8`.

**Three of the walk's own findings were WRONG and were withdrawn.** Recorded
because each was withdrawn for a reason a future reader would otherwise
re-derive:

- **Playlist / smart-mix tiles DO have mosaic covers** (17 of 19 on the live
  bridge). They looked blank because `document.visibilityState` in an
  automated browser tab is `"hidden"`, and `loading="lazy"` images never load
  in a hidden tab — the first image request did not start until **14.3 s**
  after navigation. **Any perceived-performance claim measured through browser
  automation is suspect for this reason**; check `visibilityState` before
  believing a timing. The one measurement that survived was over the wire
  (49 portrait requests with a non-zero `transferSize`, so real 304s).
- **The genre/composer axes must NOT gain a comma split.**
  `internal/librarycat/genre.go` already implements the multi-value fold and
  explicitly rejects comma ("Folk, World, & Country" is ONE genre); it is the
  fourth entry in the do-not-unify family and mirrors iOS
  `GenreNormalizer.swift`. The live library's "Pop, Rock" / "Classique" vs
  "Clásica" untidiness is a LOCALE and source-tag problem, not a fold gap.
- **The artist axis must NOT be folded bridge-side.** It keys on
  `dupes.Normalize` over `"; "` segments — the client-mirrored partition, whose
  stated design property is that the browser's list equals the phone's *by
  construction*. Stripping `feat.`/role suffixes here (as `internal/enrich`'s
  `matchfold` does for COMPARISON) would make the two disagree. It is a
  Mirror-PR project needing an iOS `MetadataNormalizer` decision, not a bridge
  fix.

**Invariants worth not re-breaking:**

- **A `git describe` build is a DESCENDANT of its tag, not a pre-release of
  it** (#797). `make build` stamps `0.1.9-65-g8b092ad`; semver reads a
  hyphenated suffix as a pre-release sorting BELOW `0.1.9`, so the updater
  offered the tag as an "upgrade" and Install would have rolled the binary
  back 65 commits — silently, on every poll, with auto-install on.
  `normalizeDescribe` rewrites the suffix as BUILD METADATA
  (`0.1.9+65.g8b092ad`), which semver ignores when ordering: equal to the tag
  (no update) while `0.1.10` still compares greater (real updates still land).
  **Both halves matter** — suppressing the downgrade alone is achievable by
  suppressing every update, which is worse and quieter, so the "still
  upgrades" rows are the real assertion. `appendBuildMeta` joins with `.` when
  the tag ALREADY carries metadata: `v0.1.9+ci.1+65.g…` fails `semver.IsValid`,
  `current` falls to the v0.0.0 floor, and the downgrade returns for that
  input shape.
- **`/api/jobs` has no `upscale` node, and never did** (#798). The Settings
  prerequisite chip read `jobs.upscale.enabled`, so it was `undefined` on every
  bridge and the chip could never say "active" — beside a checked toggle, on a
  bridge with a live two-worker pool and 8,163 cached variants. It now reads
  `/api/upscale/stats`, the endpoint the stats block on the same page already
  polls. `TestSettingsPrereqsOnlyReadRealJobsFields` reflects over
  `jobsSnapshotResponse` and fails on any `jobs.<field>` read app.js makes that
  the endpoint does not return — **an undefined property read is not an error
  in JS, so the control renders and silently reports the wrong state**.
- **One byte formatter, binary, PB-deep** (#798). `formatBytes` stopped at GB
  ("1048576 GB free" on a petabyte mount) and `player/format.js` was DECIMAL
  while its docblock claimed it matched the operator pages. Binary is the
  survivor: the numbers are compared against `df -h` on a Linux host.
  `TestByteFormattersAgree` is the lockstep pin (the two files cannot share
  code — classic script vs ES module).
- **The `/` catch-all 404 absorbs Go's 405** (#798), verified with a probe, not
  assumed: `POST /stats` renders 404. Nothing in the product issues one, and
  the alternative is enumerating every pattern to tell them apart.
  `/api/*` still answers the JSON envelope.
- **Artist portraits are content-keyed and the token is VERIFIED** (#799). The
  enricher overwrites `artist-<mbid>.jpg` in place, so there is no content key
  in the id the way `local-<sha256>` covers have one;
  `manifest.ArtworkFileVersion(mtime,size)` supplies it. **The handler
  recomputes the token from the file rather than trusting `v=`** — the client's
  token comes from a TTL-cached directory listing, so a portrait replaced
  inside that window is requested under the OLD token, and answering that
  `immutable` freezes the previous image in a viewer's cache for a year. A
  mismatch degrades to the short max-age. The token lives in
  `internal/manifest` because `internal/admin` must not import
  `internal/enrich`.
- **Admin sessions persist in the credentials file** (#800), reversing the
  package doc's original call. Rejected alternatives, both from review: SQLite
  puts session writes behind the scanner/enricher writer mutex for data
  unrelated to the library; stateless signed cookies buy a key whose storage,
  rotation and blast radius are all new problems, and a key in the same
  directory gains nothing over the sessions. **Writes are graded by what losing
  one costs**: login and LOGOUT synchronous (a revocation left in a debounce
  window is UNDONE by a restart, so the logout silently is not one), LastUsedAt
  on a 30 s debounce with `FlushSessions` at shutdown. `load()` seeds
  `lastSessionFlush` or the first request of every boot fsyncs. Expired
  sessions are DROPPED at load — restoring one lets a restart EXTEND a login.
  Legacy files are detected by a top-level `passwordHash`, checked explicitly
  because `encoding/json` ignores unknown fields and would decode the old shape
  into an all-nil envelope.
- **`/healthz` and `/readyz` bypass the session gate BEFORE the
  auth-configured guard** (#800). Both previously 302'd to `/login`, and a 302
  reads as healthy to most health checkers. They are two endpoints because they
  drive different actions: a liveness failure restarts the process, a readiness
  failure only drains it — and a bridge doing its first scan is alive and not
  ready, so restarting it makes it start that scan again. The ordering matters
  for exactly one case: public mode with no credential store is a running
  process a restart cannot fix, so liveness answers 200 while readiness answers
  503. Both disclose a status code and nothing else.
- **`/v1/health` withholds `scanState` and the update triple from an
  UNAUTHENTICATED caller** (#801, mirrored in iOS #1480). Unauthenticated
  across the internet, `serverVersion` + `updateAvailable` enumerate every
  reachable bridge and sort them by how far behind they are. **The scope was
  set by reading `BridgeSourceClient.HealthResponse` field by field**, not by
  principle: everything withheld is already `ScanState?` / `String?` / `Bool?`
  there, while `libraryName`, `libraryRoots`, `certFingerprint`,
  `serverVersion` and `startedAt` are NON-optional — withholding them fails
  Codable decoding outright on every shipped app. Narrowing those needs an iOS
  release making them optional first. `minClientVersion` stays unauthenticated
  (client-compat floor, not disclosure) and was dropped by accident in the
  first cut. An invalid token is unauthenticated, never 401 — a client holding
  a revoked token would otherwise lose the endpoint list it needs to reconnect.
- **Env overrides are DERIVED from the Config struct** (#802), 11 → 84
  bindings. **Only `libraryRoots` uses the OS PATH separator**; everything else
  is comma-separated, because `customEndpoints` holds URLs and
  `metrics.allowCidrs` holds CIDRs — splitting `https://host:7788` on `:` gives
  three fragments that then fail validation and vanish. Two legacy names
  (`BRIDGE_MUSICBRAINZ_BASE_URL`, `BRIDGE_COVERART_BASE_URL`) are kept as
  aliases; losing them sends an Atlas-configured bridge back to public
  MusicBrainz at the self-hosted 150 ms pace. **Lesson worth more than the
  feature**: the completeness test's first predicate accepted only `*bool`
  among pointers — exactly what the implementation handled — so it could never
  flag a forgotten kind, and missed all five `*int` fields. A completeness test
  must be written from what the mechanism CAN express, not from what the code
  currently does.
- **`SeedFromEnv` seeds an EMPTY store only** (#802). Otherwise the env becomes
  the credential rather than the seed and a rotated password is undone by the
  next restart. It deliberately does NOT force a change at first login — the
  secret is issued by the platform, there is no human there, and forcing a
  change breaks the automation it exists for. `_PASSWORD_FILE` wins over the
  inline form (a mounted secret is not in `ps`/`docker inspect`) and an
  unreadable file FAILS startup rather than quietly installing a different
  credential. The 12-rune floor applies to seeding only, never to
  `ResetPassword`, which has a human at an interactive prompt.
- **Logs default to JSON when stderr is not a terminal; HSTS is public-mode +
  TLS only; `/metrics` gains `metrics.allowCidrs`** (#803). The HSTS loopback
  exclusion is the load-bearing half — pinning HSTS for `localhost` poisons
  that host name in the operator's browser for every other local service they
  run. `/metrics` keeps loopback-always-allowed so the default posture is
  unchanged; the CIDR list exists because loopback is UNREACHABLE from a
  Prometheus outside the container's network namespace. An unparseable CIDR is
  skipped, never fatal — it can only narrow the gate.

**Process notes.** A negative control that fails to BUILD reads as "control
invalid", never as a pass — and one that mutates the wrong line is worse,
because it passes: a `perl -0pi` substitution for `s.lastSessionFlush = now`
matched the FIRST occurrence (in `persistSessionsLocked`) rather than the
intended one in `load()`, and the control passed while proving nothing. Check
which line the mutation actually changed. Two bot findings were declined with
evidence on the thread: `encoding/json`'s `omitempty` DOES drop a non-nil empty
map (verified with a 10-line program — the test written for the "fix" was
vacuous, which is how it was caught), and the nil checks in `Server.ready()`
stay because a nil dereference in a liveness probe becomes an orchestrator
restart loop rather than a stack trace.
---

## 2026-09-01 improvement batch (PRs #816–#827)

Eleven items from a forward-looking survey, planned in
`ops/plan-2026-09-01-improvements.md`. Two premises were reversed by
MEASUREMENT before any code was written, and both are worth keeping:

- **Squashing the migration ladder is dropped.** A fresh install replays all
  41 migrations in **15–24 ms**. There is no performance problem to solve, and a
  baseline schema would add a second, independently-maintained definition — the
  "two copies wrong together" class this file records repeatedly. If a baseline
  is ever wanted the motivation must be something other than speed, and it must
  be GENERATED from the ladder with a test diffing `sqlite_master`.
- **The Windows/macOS gate leg now BLOCKS** (#817). Promotion was gated on
  evidence and the evidence was 60 consecutive workflow runs audited across all
  branches with **zero** Windows failures, 21 of them consecutive on `main`. If
  a new flake appears the response is to fix the test — that is what all five
  previous ones needed. It earned its keep within hours: it caught #825's
  POSIX-only `file://` URL construction.

**Don't-undo list from the batch itself:**

- **`wal_checkpoint(TRUNCATE)` runs AFTER `VACUUM`, never only before**
  (`internal/manifest/compact.go`). In WAL mode the vacuum's own output lands in
  the WAL, so without the post-checkpoint the file does not shrink by a single
  byte and peak disk RISES. Measured: 5,623,808 → 5,623,808 with the WAL grown
  to 2.8 MB, then 2,572,288 once checkpointed. A review proposed the
  pre-vacuum checkpoint alone, which would have shipped a button reporting
  success and reclaiming nothing. The test asserts the FILE SHRINKING, not just
  `freelist_count` — freelist still reads 0 under the broken form.
  `auto_vacuum = INCREMENTAL` was considered and rejected (it needs a full
  VACUUM to switch anyway, so enabling it for new DBs alone splits the
  population).
- **The retention reap FAILS CLOSED on an empty live-token set, and the two
  empty forms are NOT interchangeable** (`internal/manifest/retention.go`).
  Measured without the guard: `nil` deletes ZERO rows (`json_each('null')`
  yields a NULL row, so `NOT IN (NULL)` is never true) while `[]string{}`
  deletes EVERY row — and `cmd/bridge` builds `make([]string, 0, n)`, the
  dangerous spelling. Never "simplify" the guard away on the reasoning that an
  empty IN-list is harmless; it is harmless in one spelling only.
- **`retention.playbackHistoryDays` REFUSES 1–89 rather than clamping.** The
  bounded smart-mix windows run to 90 days, so a shorter retention would
  silently gut the time-of-day and session families. And **any** non-zero value
  degrades Forgotten Favourites, which has no lower time bound at all — stated
  in the config docblock rather than hidden. Default is 0 (keep everything).
- **Every `/v1` route declares a `rateClass`, and the zero value is INVALID.**
  Unlike `routeKind`, whose zero (`boundedRoute`) is the safe default, the
  permissive answer here is the dangerous one — a route that forgets to choose
  would ship unlimited. The second guard classifies by HTTP METHOD, so a new
  mutating route cannot be added as `rateNone` by omission; it has to be argued
  for in an exemption list in the test. **Size the BURST, not the rate**: iOS
  surfaces 429 as a transport error and does NOT retry, so a tight bucket
  breaks a sync rather than slowing it.
- **`GET /v1/search` serves the SERVED set** (`SearchServedTracks`).
  `tracks_fts` is trigger-populated from `tracks`, so it contains
  duplicate-suppressed rows; the join with `dupe_suppressed = 0` is what keeps
  them off the wire. The admin console deliberately keeps calling the
  UNRESTRICTED `SearchTracks` — operator truth. The query plan was raised as a
  full-scan hazard and measured not to be one (`tracks.path` is the PK and the
  MATCH drives), but the `EXPLAIN QUERY PLAN` test is kept anyway.
- **A manual-URL UPnP server is cached under the ingest's `StableServerKey`,
  not the device's own UDN** (`internal/upnp/manual.go`) — routing rows,
  telemetry, `LiveHost` and the online chip all key on that string. One
  insertion point into the shared `ServerCache` makes all three surfaces work.
  The duplicate guard admits a device whose UDN is **this entry's own key**
  (a server configured with BOTH wants the manual URL as its SSDP fallback)
  and refuses one matching a DIFFERENT configured server.
- **`sendErrStreak` in `internal/dlna/discovery` is deliberately
  unsynchronised, and that binds TESTS too.** A test calling `noteSendResult`
  directly must do so while no run loop is live — before `Start`, or after
  `Stop`, which joins it. One that did neither raced under `-race` on CI and
  was not reproducible locally in 26 runs. Adding a mutex would pay production
  for a test's convenience.

**Test-shape lessons, all learned the hard way in this batch:**

- **A test on a HELPER proves nothing about whether anything CALLS it.** The
  `search` feature flag shipped in a commit whose message said it was
  advertised, and it was not — an edit silently failed. The only assertion was
  on `searchAvailable()`. Drive the real endpoint.
- **A behavioural assertion can pass because a DIFFERENT layer saved you.**
  "Registrations survived a token-read failure" went green against a sweeper
  that skipped nothing, because the store's own guard caught it. Observe the
  call, not the outcome, when the test names a layer.
- **The node module-load stub must TERMINATE.** A fully-permissive `Proxy`
  stub hangs (a module loops while a truthy value keeps coming back), so the
  harness also needs a per-module watchdog. Negative controls on module code
  must copy the whole directory — relative imports mean mutating one file in
  isolation fails on a missing sibling, which is a green control for the wrong
  reason.
- **`filepath.ToSlash` is a NO-OP on POSIX**, so a Windows-shaped path handed
  to it on a Mac keeps its backslashes. Any test asserting Windows path
  behaviour from a Mac needs the explicit `strings.ReplaceAll` that
  `internal/dsn.File` documents.
- **`git add -A` with another branch's untracked files on disk sweeps them into
  your commit.** Use explicit paths.

**The wiring-test lesson, which cost three separate bugs in one batch and is
the most transferable thing here.** `/v1/search` shipped with a handler, a
store method, a rate limiter and its own rate class, config, a health flag and
a PROTOCOL.md section — and answered **404 in production**, twice over: no
entry in `routeRegistry` (an edit to that file failed its assertion and never
wrote), and `manifest.Provider` not forwarding the search surface the api
type-asserts for. Both were found by curling the live bridge after deploying;
every unit test was green throughout.

The three instances, all the same shape — *a test that exercises everything
except the wiring*:

1. a **helper nothing called** — the health flag's `searchAvailable()` was
   tested directly while nothing appended the flag (Gemini caught this one);
2. a **type nothing constructs** — the handler tests' stub implemented
   `SearchServedTracks` directly, so they proved nothing about
   `*manifest.Provider`, which is what `cmd/bridge` actually passes;
3. a **handler nothing dispatches to** — every search test called `s.search`
   directly, so the mux was never exercised.

So: **when a feature's test never touches the thing that connects it, the
feature can be entirely dead and the suite entirely green.** Drive the real
entry point — `Handler()`, the real `Provider`, the real endpoint — at least
once per feature. `TestEveryDocumentedEndpointIsRouted` now closes the routing
half by requiring every endpoint PROTOCOL.md documents to appear in the
registry; it compares against the REGISTRY rather than probing responses,
because several routed endpoints answer 404 by design
(`pairing_not_supported`) and a response probe cannot tell "no route" from
"routed and refusing" — the first version probed, produced four false
positives, and taught exactly that.

**The iOS mirror is DONE** (`acoseac/1-bit` #1515) and the two files are
byte-identical again. **No Swift change was needed or made**:
`HealthResponse.features` is `[String]?`, so the new flag decodes and is
ignored, and a `BridgeFeatures` constant with no consumer would be speculative.
Wiring `BridgeSourceClient` and deciding where server-side search belongs in the
UI is a product decision left open on purpose — nothing on iOS calls
`/v1/search` yet.

**The guard is now BIDIRECTIONAL, and the converse direction was the one with
real findings** (#835). `TestEveryDocumentedEndpointIsRouted` was written
because an endpoint was documented and not routed;
`TestEveryRoutedEndpointIsDocumented` was added because **six were routed and
not documented** — the four `/v1/upscale/batch*` + `DELETE
/v1/upscale/variants` routes appeared only in a rate-limit list and a demo-mode
paragraph, and **`GET /v1/renderers` and `GET /v1/diagnostics` appeared nowhere
at all**, despite both being live and both already having a `BridgeFeatures`
constant on iOS. So the app depended on two endpoints the shared wire contract
never described, and a third-party client author could not have written either.
Now 41 routed / 38 with their own section / 3 exempted-with-reason, and zero
documented-but-unrouted.

Three things worth carrying forward from writing those contracts. **Read the
status code out of the running handler, not out of the source** — that is what
corrected `GET /v1/diagnostics`, which I had written as flag-gated from the
flag's NAME when it is unconditionally wired. **Don't label a section with a
version you cannot verify**: the spec's `since v1.x` labels are iOS app
versions, so a bridge-side session cannot derive them — the first draft guessed
two and one was wrong by a release. The headings name the **feature flag**
instead (`operatorDrivenUpscale`, `deleteVariants`, `rendererDiscovery`,
`diagnosticsSummary`), which is both checkable here and the thing a client
actually keys on, since no client can ask a bridge its protocol era. And the
guard deliberately accepts only a `### ` heading or a bold `METHOD /path`
lead-in as a contract — **a mention in running prose does not count**, because
that is precisely the state all six were already in.

**Still open from the plan:** the CLAUDE.md structural split (invariants stay,
per-PR narrative moves to an archive) — deliberately NOT done here. Moving
content out of this file means future sessions stop auto-loading it, so
deciding what is load-bearing is a judgement call worth making deliberately
rather than at the end of a long batch.
---

## SACD ISO expansion — .iso images mint virtual DST track rows (PR #779, 2026-08-28)

The bridge half of the SACD ISO batch (iOS acoseac/1-bit#1451/#1454/#1455). A scanned `.iso` SACD image expands into one VIRTUAL track row per stereo track at `<iso>/st/NN.dff` — fully typed (`codec "DFF"`, `compression "DST"`, DSD64/1-bit/2ch, TRL2 durations, TOC titles/performers/album text) — and the container gets NO row. `ProtocolVersion` stays 1 (virtual rows are ordinary manifest rows; PROTOCOL.md § "SACD ISO expansion" carries the pinned grammar, byte-mirrored in the iOS repo's docs/BridgeProtocol.md). Don't-undo list:

- **CLEAN-ROOM LAW (licensing, P0)**: `internal/manifest/sacd.go` descends from the project's OWN byte-level derivation against two real ISOs (its header carries the provenance; the design record is the iOS repo's docs/SACDISOFeasibility.md §3) — **NEVER read, excerpt, or borrow from GPL SACD lineage (sacd-ripper / sacd_extract / foo_input_sacd — code OR prose), and never open the leaked "System Description" scan.** Format corners are resolved empirically against real ISOs (`TestSACDExpand_RealISOs`, env-gated `BRIDGE_SACD_ISO_PLAIN`/`_RAW` — both operator discs match the iOS spike's ground truth exactly: Genesis 9 tracks / 208,672 area frames, Division Bell 11 / 299,924; the invariant is `lastStart + lastDur == areaFrames`, NOT the duration sum, which legitimately undershoots on discs with lead-in/gap frames — Genesis carries 225).
- **The path grammar is a PINNED cross-repo wire contract** — `SACDVirtualTrackPath` / `SACDVirtualContainer` / `IsSACDVirtualPath` mirror the iOS `SACDVirtualPath.swift` EXACTLY (index `%02d` 1–99 / unpadded 100–255, `st` minted, `mc` recognized-never-minted, `.iso` case-insensitive). A parse-rule change on either side without the other drifts deletion membership and client classification.
- **THE deletion-pass membership fix** (both `Scan` and `ScanSubtree`, kept in step per the `routedPathSet` rule): a virtual row is SEEN whenever its CONTAINER is in the walk's seen set — virtual paths never appear in any disk walk, so a bare seen-diff counts them missing every scan and the threshold reaps them (the routed-rows hazard, one row-species over; the iOS `reapRowIsSeen` mirror). **Negative-control verified red-first**: removing the guard turns exactly 3 of 12 tests red (`TestScanner_SACDVirtualRows_SurviveRescansOfAnUnchangedLibrary` first). A genuinely-gone container falls through and its rows reap at the threshold, journaled.
- **`processSACDISO` owns the ISO leg in the worker**: its own skip-gate stats the REPRESENTATIVE first virtual row (`<iso>/st/01.dff` carries the container's size+mtime — the generic gate keys `GetTrackStat` on `pi.rel`, and a container has no row), then `ExpandSACDISO`, then an **immediate journaled retire** of virtual rows the fresh expansion no longer mints (`IncrementMissingTracksAndDeleteAtThreshold(…, 1)` — the deletion pass's own writer, so delta clients see tombstones). This is what closes the shrink/stopped-being-SACD hole the container-seen sparing would otherwise hold open forever. Non-SACD / plain-DSD-area / multichannel-only images contribute NOTHING (v1 is stereo DST, the iOS envelope).
- **The worker's hand-off is `tracksToWrite []*Track`** (one input file → N rows; the writer batches indiscriminately, no consumer change). The `extractByFormat` `.iso` case is a LOCKSTEP STUB (expansion cannot ride the 1:1 dispatcher) — keep it, `TestExtCoversDispatcher` pins it.
- **ExtractorVersion 5→6** per the standing every-extraction-change rule. ISO pickup itself comes from the DISCOVERY gate (never-walked files have no stale rows); the bump makes any FUTURE sacd.go fix re-expand. Unchanged non-ISO rows ride `reExtractUnchanged`'s diff-guard onto the light stamp — no re-enrich wave (the v5 lesson's machinery, unchanged).
- **Nothing else changed, verified not assumed**: `/v1/read`/`/v1/download`/`/v1/stat` 404 virtual paths cleanly through `ResolveChecked`; the DLNA file handler 404s the same way (the intended baseline — clients play by fetching the CONTAINER bytes and demuxing the window client-side); analysis keeps its `.dff` DSD exclusion; fingerprint + upscale eligibility already gate DSD out. **Don't add a bridge-side demux** — serving stays bit-exact verbatim by mission.

Locked by `sacd_test.go` (grammar truth table + plain/raw geometry identity + damaged-first-copy fallback + nothing-from-non-SACD + stem fallback; the Go fixture builder writes raw bytes INDEPENDENTLY of the parser so a shared misunderstanding cannot self-validate) + `scanner_sacd_test.go` (expansion E2E, THE survival control, container-removal reap, shrink immediate-retire, sibling-audio isolation, the unchanged-rescan skip-gate pinned via a `MarkEnriched` title surviving).
---

## Web upload + delete-as-trash (PRs #788–#792, 2026-08-30)

The console can put files INTO a library and take them out again. Five stacked
PRs; admin-surface only — **no `/v1` change, no `ProtocolVersion` bump, no
`PROTOCOL.md` change, no iOS mirror, no migration.** Design record:
`ops/plan-web-upload.md`. Two independent gates, both default OFF:
`upload.enabled` and `library.allowDelete`.

- **Staging and trash live INSIDE the target root, as dot-directories**
  (`.bridge-upload/`, `.bridge-trash/<unixNano>/`). `shouldSkipDir` returns
  `SkipDir` for any `.`-prefixed name BEFORE the walker upserts a folder row, so
  neither tree is ever walked — no folder rows, no track rows, no interaction
  with the deletion pass. That is what makes commit and delete **same-filesystem
  renames**. The tidier-looking alternative — staging under `<dataDir>` — is a
  cross-device `EXDEV` copy wherever the library is a separate mount, which is
  the normal case (bridge.ars.md has its data dir on the 29 GB root disk and its
  library on a B2 FUSE mount): every byte written twice. **Don't move staging or
  trash out of the root, and don't drop the leading dot.**
- **`csrfGuard`'s relaxation is `application/octet-stream` on PUT only, and
  `multipart/form-data` stays refused EVERYWHERE.** The Content-Type check is a
  CORS simple-request defense: the simple types are exactly
  `x-www-form-urlencoded`, `multipart/form-data` and `text/plain`, so multipart
  is forgeable cross-origin while octet-stream (and a PUT method) forces a
  preflight the bridge never answers. Building the upload as a multipart form
  would hand back precisely the property the guard provides. The allowlist
  refuses any path that changes under `path.Clean`, mirroring
  `sessionMiddleware`'s bypass rule.
- **The admin server's `ReadTimeout: 30s` caps reading the ENTIRE body**, so a
  200 MB file dies at 30 s under ~55 Mbps. Fixed by rolling the deadline forward
  as bytes arrive (`uploadBodyReader`, `http.ResponseController`) rather than
  raising it globally and losing the Slowloris protection PR #75 added. The
  window is a per-instance field, not a package var (`Pool.jobTimeout`
  precedent).
- **The durable offset is the per-file meta record, NEVER the staged file's
  size.** A dropped PUT leaves bytes past the last acknowledged offset, so every
  open truncates back to the recorded one. Ordering is bytes → fsync → offset →
  fsync: the reverse lets a crash advertise an offset the file cannot honour,
  and then truncate-on-open discards REAL data. Staged files are opened WITHOUT
  `O_APPEND` — POSIX would ignore the seek, which the truncate makes harmless
  *by accident*, and an accident that happens to be correct is one edit from not
  being. The running SHA-256 is marshalled into the meta record (108 bytes,
  verified), so a resume finishes with the right whole-file hash without
  re-reading what it staged.
- **Locks are refcounted and per `(session, file)`, never per session.** The
  naive map leaks an entry per file; the naive FIX is worse, since deleting a
  key while another goroutine is between lookup and lock hands them different
  mutexes for the same file. A session-wide lock passes every other locking test
  while quietly serialising a folder upload, which is where browser throughput
  comes from — there is a control test that goes red if it is widened.
- **`planScanDirs` does not scan the common ancestor, and the depth-1 floor and
  the in-loop re-prune are BOTH load-bearing.** `A/Album` and `Z/Album` have the
  root as their LCA, so an ancestor trigger degrades to a full scan on exactly
  the sessions where a targeted one matters. But one-scan-per-directory has a
  worse cliff: **`ScanSubtree`'s tail runs `restampDuplicates`, and that pass is
  WHOLE-LIBRARY**, so N subtree scans cost N whole-library restamps. Without the
  floor, nine top-level artist folders collapse to the root in one iteration and
  escalate — after destroying the information needed to do the nine targeted
  scans. Without the re-prune the result keeps redundant pairs and over-collapses
  to whole top-level folders: **brute-forced over 400,000 random inputs, 12,718
  differ.** My first test for the re-prune did NOT distinguish it and I nearly
  wrote the reviewer's finding off on hand-reasoning — the brute force settled
  it. `maxSubtreeScans = 8` is still a guess; the restamp duration is now logged
  on every subtree scan so it can be tuned from data.
- **The duplicate pre-flight WARNS, it never decides.** `(basename, size)`
  against `tracks`, once per session. It answers the overlapping-upload case:
  uploading an album folder and then the artist folder containing it lands two
  copies at DIFFERENT paths, so nothing collides, both survive on disk, and the
  duplicate election tie-breaks on the SHALLOWER path — the flat copy wins and
  the properly-nested one is suppressed. A control test pins that a flagged file
  still uploads: a track legitimately on both an album and a compilation is a
  real library, and that is serve-time suppression's job.
- **Committed files are 0644, not 0600.** The staged file's mode SURVIVES the
  rename, so it is the library file's mode. At 0600 an uploaded track is
  readable only by the bridge's own user, unlike everything else in the library
  — a Samba share or a backup job running as someone else silently cannot read
  it. Umask still applies.
- **Trash age comes from the `<stamp>` DIRECTORY NAME, never a file's `stat`.**
  `os.Rename` preserves mtime — measured, not assumed: a file stamped 2019 and
  trashed now reads as **2797 days old the instant it lands**, so an mtime-driven
  sweeper purges it on the very next tick, oldest-content-first, destroying the
  recovery window for precisely the material most likely to be irreplaceable.
  Restore `MkdirAll`s the destination parent FIRST — the directory may have been
  removed after the last track in it was trashed, which is exactly the case
  restore exists for. A directory whose name is not a stamp is LEFT ALONE: its
  age is unknowable and guessing deletes user content.
- **Trashing does not free space; purging does.** That tension is why
  `/api/library/space` reports reclaimable bytes beside free ones, why the `507`
  body carries `reclaimableBytes` (so a full disk becomes "empty trash and
  resume" rather than a dead end), and why the trash panel leads with what
  emptying would return. Rows retire IMMEDIATELY via
  `IncrementMissingTracksAndDeleteAtThreshold(paths, 1)` — the existing path that
  already unlinks sidecars and writes tombstones — not the three-scan
  missing-count debounce, because an explicit operator delete should not linger.
- **Deleting takes an EXPLICIT PATH LIST, never a prefix.** That sidesteps the
  `LIKE`-vs-byte-range case-fold class entirely rather than getting it right:
  there is no prefix to scope. The plan called for a byte-ranged pin; the
  implementation made it unnecessary, which is strictly better.
- **`upload.enabled` and `library.allowDelete` are two gates and must stay two.**
  Enabling an additive feature must never silently enable a destructive one; a
  control test asserts that turning uploads on leaves deleting refused. A nil
  delete gate fails CLOSED.

- **Chunks are verified by CONTENT, not just length** (2026-08-30). The server has
  parsed and verified an RFC 9530 `Content-Digest` since the path shipped and skips
  the check when absent; nothing sent it, so uploads were size-verified only. The
  client now hashes each chunk with `crypto.subtle`. **Per chunk is the right
  granularity, not a compromise** — SubtleCrypto has no incremental digest API, so a
  whole-file hash would mean holding a 900 MB DSF in memory, and every byte travels
  inside some chunk. **The retry is scoped to `digest_mismatch` and MUST stay
  scoped**: it is safe only because the server refuses the bytes without advancing
  its offset and `openStagedFile` truncates back to it, so the re-send writes the
  same range over nothing — widen it to all failures and you are blindly retrying
  errors that are not idempotent. Without a retry, adding verification could only
  ever have converted silent corruption into a dead multi-gigabyte upload.
  `crypto.subtle` needs a secure context; **both supported deployments are one**
  (loopback is potentially-trustworthy, and public mode's `:7789` serves a real LE
  cert — it is an HTTPS listener, which Chrome's omnibox hides), so an absent
  SubtleCrypto degrades to no header AND says so in the result line rather than
  dropping the guarantee silently. `errorFromResponse` carries `code`/`status` so
  none of this depends on matching prose.
  **The test runs the SHIPPED client function under node against the real Go
  parser** — a hand-written Go replica asserts its author's beliefs, and a source
  scan cannot catch a format disagreement that would 400 every chunk. Two fixture
  traps worth not repeating: the payload must travel as explicit BYTES (a string
  round-trip put U+FFFD in place of an invalid UTF-8 byte, so the two sides hashed
  different inputs), and a corruption test must send DIFFERENT bytes than the retry
  (sending the same bytes twice cannot distinguish a working truncate from a broken
  one). **`extractJSFunction` anchors on the opening paren** because
  `putUploadChunkVerified` prefix-matches `putUploadChunk` — a bare-name scan
  silently examines the wrong function, which is exactly how this change broke an
  existing test.

**Process lesson worth more than any single fix: `go test` was green through
FIVE defects that a live browser found** — committed files at 0600; `totalBytes`
declared and never populated (the sidebar bar pinned at 0% forever); the
duplicate note hidden by the post-commit reset; the note nested inside the block
that collapses when the upload starts (**a JS assertion read `hidden === false`
and LIED — that says nothing about an ancestor**, only a screenshot caught it);
and `.hint.warn` being an inert class combination whose first guard could not
fail, because `.hint.warn` is a substring of `.hint.warn-DISABLED`. Seed a
throwaway bridge and drive the console before believing a green suite about UI.

**Round-two review findings worth keeping** (the stack drew 14; 12 taken, 1
corrected, 1 declined): the read deadline must advance on ELAPSED TIME, not
bytes — a 256 KiB threshold starves the slow client it exists to protect (4
KiB/s delivers 240 KiB in a 60s window and is torn down mid-transfer);
collision handling must be atomic ACROSS SESSIONS, since the per-file lock is
keyed on `(session, file)` and `os.Rename` REPLACES, so two sessions targeting
one destination let the second silently destroy the first (a destination-keyed
lock, because `RENAME_NOREPLACE` is Linux-only and `os.Link` needs hardlinks
the rclone/B2 mount lacks); and `SetReadDeadline` must only log the
wrapped-ResponseWriter diagnosis for `ErrNotSupported`, because everything else
is a client hanging up and `warnOnce` meant one spurious disconnect SUPPRESSED
the real diagnosis forever. **`upload.stagingDir` was removed rather than
shipped**: it was declared in config and never read, and an inert knob an
operator can set to no effect is worse than none.

**Two guards in this repo caught things nothing else would have**, and both
deserve their keep: `TestEverySettingsFieldIsMappedIntoThePatchPayload` flagged
a settings control that would have rendered, accepted an edit, reported
"Saved." and changed nothing; and `TestAppJSHasNoCallsToDeletedHelpers` flagged
four functions that plainly exist — the cause was a comment of mine containing
`/api/upload/` followed by `*`, whose `/*` opens a fake block comment and
swallowed **46 KB** of app.js from the scanner. **Don't write `/*` inside a
`//` comment in this codebase's JS.**

**Process notes from the batch.** Stacked PRs in this repo get **no CI at all**
until they target `main` (`gate.yml` / `gofmt.yml` / `codeql` are
`pull_request: branches: [main]`, and `gh pr checks` reports "no checks
reported" on a feature-branch base) — so the retarget-then-amend dance is not
optional if you want the Windows leg to have run before you merge. And when
waiting on a local `make check`, do NOT poll with `pgrep -f "go test -p"`:
that pattern matches the waiting shell's OWN command line, so the loop waits on
itself forever and reports the gate as busy long after it finished. Use the
harness's background-task completion notification, or a self-match-proof
pattern like `[/]go-build.*[.]test`.

**Follow-ups from the first real folder upload (#793, #794).** Enabling uploads
on the VPS turned up four things a green suite did not:

- **A read-only library root fails every session at the first `mkdir`**, and
  the generic wrap reached the operator as a 500 whose cause was only in the
  log. `classifyStagingError` now names `EROFS` and permission failures
  separately as `ErrLibraryNotWritable` → **503 `library_read_only`** with the
  remedy in the message. The reference deployment mounted its B2 library
  `--read-only` until this feature needed writes.
- **`--vfs-cache-mode minimal` cannot serve this upload path.** rclone's docs:
  under `minimal`, *"files opened for write only can't be seeked"* and
  *"existing files opened for write must have O_TRUNC set"* — the chunked path
  does both. Dropping `--read-only` alone leaves uploads broken confusingly;
  the mount needs `writes` (NOT `full`, which caches every read and thrashes
  the cache on a streaming library). **Measured and not a problem:** staging
  inside a B2-backed root does not amplify uploads — rclone coalesces repeated
  close-triggered write-backs into ONE upload — though that depends on
  `--vfs-write-back` (5s), so chunks arriving slower than that would each
  upload the file-so-far.
- **One `.DS_Store` blocked a fourteen-file album.** `Create` refused the whole
  session on the first invalid path. It now skips-and-reports; nothing
  acceptable is still an error. The client also filters OS junk
  (`.DS_Store`, `Thumbs.db`, `desktop.ini`, `@eaDir`, AppleDouble `._*`)
  BEFORE declaring, so the operator is never told about a rejection for a file
  the OS created without asking.
- **`.error` had no CSS rule anywhere** — four elements across the templates
  used it and all rendered as ordinary body text, which is why the rejection
  was missed in the field. Same inert-class family as `.hint.warn`.

**Two latent layout bugs the new page exposed.** `#primary-nav` carried
`min-height: 0` with no scroll: that only PERMITS shrinking below content, and
the shrunk box then spills so the last links render UNDERNEATH what follows —
Settings overlapped the space meter by 35px once the rail gained a third flex
child. Giving the nav its own scroll fixed the overlap and replaced it with a
worse bug (Settings hidden behind an invisible internal scroll); the answer is
a natural-height nav so `header.sidebar`'s own overflow scrolls the WHOLE rail.
And the drop zone was `role="button"` around two label-wrapped file inputs — a
button must not contain interactive descendants, so the card announced as one
button while holding two controls. SonarCloud flagged the missing keyboard
handler, which was the right flag for the wrong reason: the fix is dropping the
role, not adding an `onkeydown` to satisfy the checker.
---

## Library sources in the sidebar (PRs #807 / #808 / #809, 2026-08-31)

A hybrid bridge serves its own filesystem AND one or more upstream UPnP
MediaServers, and the player blended both into one set of grids with nothing to
tell them apart. Upstreams now have their own first-level sidebar group between
LIBRARY and SERVER — one row each, with whether it is reachable, linking to the
library scoped to it — plus a `source=` filter on the album / artist / genre /
composer endpoints. Admin-console only: no `/v1` change, no `ProtocolVersion`
bump, no `PROTOCOL.md` change, no iOS mirror, no migration.

- **`upnp_track_routing.server_udn` is the ingest's `StableServerKey`, NOT the
  device's UDN** — lowercased UDN for a UDN-configured upstream,
  `"manual:<sha256(url)>"` for one configured by description URL alone. The SSDP
  cache is keyed on the RAW UDN as the device reported it. The two are equal
  ONLY for a device whose UDN is already lowercase, and NEVER for a manual
  entry. `routedOnline` handed the routing key straight to `UPnPHostOnline` (an
  exact raw-UDN map lookup) and therefore answered "offline" about upstreams
  that were up; `admin.UPnPSource` now carries BOTH spellings and liveness is
  resolved on the config side, which is the only place that knows both.
  **Anything wanting both membership and liveness must carry the pair** — one
  lookup cannot serve both.
- **A Go template's `if` on a POINTER tests non-nil.** `sidebarSourceRow.Status`
  is a `string`, not the API's `*bool`: a pointer to `false` reads as TRUE, so
  every offline upstream would have rendered online. Resolve three-way state to
  a string before it reaches a template.
- **Browse stands DOWN while a source row is current** (`pageData.SourceCurrent`
  server-side; the same rule restated in `updateSidebarNav` for the navigations
  the server never sees). Every player route renders the player section, so
  without it both light and `TestPrimaryNavHighlightsEveryEntry` fails —
  correctly, since two "you are here" marks tell the reader nothing.
- **`boot();` MUST be the last statement in `boot.js`** (`TestPlayerBootCallIsLast`).
  It reaches most of the module, so a top-level call near the TOP puts every
  later `const`/`let` in the temporal dead zone. This emptied the entire player
  TWICE in one sitting — the sources-rail TTL, then `SOURCE_SCOPED_SECTIONS` —
  and both times there was **no failing test and no symptom but a
  `ReferenceError` in the console and a page that rendered nothing**. Function
  declarations hoist, so the call's position changes nothing else.
- **A scoped count describes the albums ON SCREEN, not the tracks from that
  source.** An album is shown when it holds ANY track from the source, and its
  own page is not source-filtered — so the narrower per-source number would
  contradict what the next click reveals. `librarycat.Artist`/`AxisEntry` gained
  `AlbumTracks` (the per-album share, emitted alongside `AlbumIDs` by
  `rankAlbums` so the two cannot drift) precisely so a filtered group can state
  a TRUE total: without it a scoped artist grid reports whole-library numbers,
  and the genre list stays SORTED by a count the reader can no longer see.
  `narrowGroup` narrows the album IDS too, not just the counts — `AlbumIDs[0]`
  is the group's cover tile, and a filtered list showing artwork from the
  filtered-OUT source undoes the filter visually.
- **Two different rules for two different questions, both deliberate.** The
  SIDEBAR lists what the operator CONFIGURED (an upstream not yet walked is
  exactly when its status is most worth seeing; its grid is empty until the
  first walk, which is the honest answer). `/api/player/sources` lists only
  sources that HAVE tracks (it is a facet over the library, and a row that
  filters to nothing is a dead end). Don't unify them.
- **`librarycat.SourceID` prefixes `"source:"` before hashing** so the source id
  space is provably disjoint from the album/artist/axis one — a routing key can
  never hash onto an album id and be admitted by the wrong filter.
  `LocalSourceID` is the magic token `"local"`, outside the 16-hex alphabet by
  construction. It has no nav entry since LIBRARY *is* this bridge; `?source=local`
  still works on the wire.
- **The section rail must carry `?source=` on its links, and narrow to the
  sections a source can actually filter** (Albums / Artists / Composers /
  Genres). The links are static hrefs, so before #809 clicking Artists inside an
  upstream dropped the scope and landed on the whole library — the scope
  survived a SORT change (the toolbar mutates the live URL) but not a SECTION
  change. `applySectionScope` rewrites each href from an unscoped `data-base`
  (deriving from the live href compounds the query string) and hides the rest —
  Smart Mixes is whole-library-generated, Folders is filesystem-only by
  construction, and Favorites/Playlists are documents that span every source at
  once, so a playlist is not "on" an upstream. **The section the reader is ON is
  never hidden.** Scope is applied BEFORE the strip reveal: hiding entries
  changes the strip's scroll geometry.
- **The mobile section strip reveals its active entry by assigning the
  container's own `scrollLeft`, never `scrollIntoView`** (#808). That method
  walks ANCESTORS, so on a page whose vertical position the boost router is
  separately restoring — under its own generation guard, because a stale offset
  landing last is a real defect there — it becomes a second writer to the same
  scroll state. It also must NOT move a strip whose active entry is already
  visible: `route()` runs on every filter change, so an unconditional re-centre
  yanks the strip back from wherever the reader scrolled it. It queries
  `[aria-current="page"]` rather than a class, so any future rail row works
  without it knowing about them.
- **`.sr-only` / `.visually-hidden` is `position: absolute`** — with no
  positioned ancestor its containing block is the PAGE, so inside a
  horizontally-scrolling container it escapes the overflow clip, sits at the far
  end of the scrolled content, and extends the document's scroll width to match.
  The whole page scrolled sideways because of a 1px span nobody can see.
  `#primary-nav a` is already `position: relative`; `.player-source` had to be
  given it.
- **The sidebar is operator chrome, so its styles live in `app.css`** — player.css
  must not reach into it, the same one-way rule its own header states in the
  other direction (and `TestPlayerCSSDoesNotHijackOperatorTableClasses` enforces).

**Process, three items worth keeping:**

- **Deleting scattered JS needs cuts that end at the NEXT declaration.** Slicing
  boot.js from a docblock down to `function route() {` swallowed five unrelated
  functions in between. `TestAppJSHasNoCallsToDeletedHelpers` and
  `TestEveryPushStateCarriesTheRouteTrail` both failed immediately and named
  exactly what had gone — the same class the CSS selector-set rule covers.
- **A parity test can pin the wrong half of a two-sided bug.**
  `TestScopedSectionsAreRealSectionsTheServerActuallyFilters` drives the JS list
  against the real endpoints and passes UNCHANGED against a build with the href
  rewrite removed — i.e. against the exact reported failure. Negative-controlling
  it is what exposed that; `TestSectionLinksCarryTheSourceScope` is the other
  half. When a symptom has two possible causes, control the test against BOTH.
- **CodeRabbit posts a "Review limit reached" comment when its plan's quota is
  exhausted, and then simply does not review.** Two pushes on #807 drew no
  review at all and it looked like a clean pass. **"No comments" is not
  "approved"** — check for the rate-limit notice before reading silence as
  agreement. (Gemini has its own daily quota; same trap, different shape.)
---

## Cross-source duplicates and UPnP import — PARKED, and why (2026-08-31)

Both were scoped, costed and deliberately NOT built. Recorded because the
dangerous half is how easy the first one looks: `StreamTrackDupeRefsUnderPrefix`
already takes an `includeRouted` flag, so "dedup across sources" reads as a
one-line change to anyone who finds it.

- **`outranks` has NO availability term.** The election is `lossless → bit depth
  → sample rate → size → shallower path → shorter path → lexicographic`
  ([internal/dupes/policy.go](internal/dupes/policy.go)), and nothing in it knows
  whether a copy is on this bridge's own disk or on an upstream that is powered
  off half the time. So flipping `includeRouted` to true would suppress a local
  44.1/16 CD rip in favour of a 96/24 copy on a 2Go — and the copy the bridge can
  ALWAYS serve is the one that disappears, precisely when the upstream is
  unreachable. **Don't enable cross-source suppression without first deciding
  where availability sits in that ranking**, because that placement IS the
  product: availability first = always playable, sometimes lower quality;
  availability last = best quality, sometimes unplayable.
- **The status quo is the only option that cannot hide music the operator owns**,
  which is why it was kept. Routed rows stay out of the dupe pass (their
  lifecycle belongs to the ingest reconcile — the PR #370 invariant), so the same
  recording in two places yields two rows, both counted, both served. The cost is
  a visibly doubled album; the mixed-album source filter (PR #810) already lets
  the reader see each side on its own, which is what made the display problem
  tolerable enough to park the suppression one.
- **Import ("copy the upstream's files into the bridge") is downstream of that
  decision, not independent of it.** With suppression off every import doubles
  the album until the upstream is removed; with it on, the availability question
  above has already been answered by implication. The bytes are the easy part —
  `internal/upnpproxy` already fetches them; the open questions are disk headroom
  for a multi-terabyte upstream, resumability, what the next walk does with an
  imported track, and whether the routed row is retired or left behind.
- **Measure before designing either.** Routed tags come from DIDL, not from file
  tags, so a local copy and an upstream copy only land in the same duplicate
  group if those agree. That measurement has since been taken against a live 2Go
  — see the next section — and the answer is that 12.7% of items arrive with no
  artist and no album at all, which is enough on its own to make cross-source
  grouping unreliable for that slice. Re-measure per upstream before any policy
  work here; MiniDLNA is one server's behaviour, not the protocol's.
---

## DIDL is not tags — routed rows fill artist/album from the container path (PR #813, 2026-08-31)

Measured against a real Chord 2Go (MiniDLNA) on the LAN, 15,283 walked items:
title 100%; artist / albumArtist / album / trackNumber 13,341 (87.3%); year
13,176; duration + sampleRate 13,552; **discNumber 0%; bitsPerSample 0%**. The
1,942 items carrying a title and nothing else are 1,730 DSF + 210 FLAC + 1 WAV
— **every DSF on the server**, because MiniDLNA does not read DSF tags.

- **A routed row with no artist or no album is unreachable to the enricher, and
  nothing else will ever fix it.** `enrichOne` skips at `t.Artist == "" ||
  t.Album == ""` (`skipReasonNoSearchTerms`) and stamps `enriched_at` so the row
  is never polled again. The acoustic fallback that normally catches this
  population cannot help here — it needs a local file to fingerprint. On that
  library the split was total: **7,657 FLAC rows carried an `artworkMBID` and 0
  of 1,730 DSF rows did.** The fix is `fillFromContainerPath` in
  [ingest.go](internal/upnpingest/ingest.go), which supplies the two fields the
  enricher searches by.
- **The derivation MUST be `dupes.Resolve`, NOT manifest's `fillFromPath`.** The
  scanner's rule is a plain two-directories-up, which for `Artist/Album/CD1/track`
  puts "CD1" in the album field. `dupes` strips disc folders
  (`effectiveAlbumPath`), and `dupes` is what the catalog and the iOS client
  already resolve these rows through — so filling from it writes the values
  ALREADY ON SCREEN and cannot regroup anything, while borrowing the scanner's
  rule would REGRESS the 46 rows on that library that group correctly today,
  splitting each multi-disc set into one album per disc. This is the fourth
  member of the do-not-unify family: the two derivations look interchangeable
  and are not.
- **`dupes.UnknownArtist` / `UnknownAlbum` are display text and must never be
  persisted.** They are now exported consts precisely so this caller can drop
  them. Written through, "Unknown Artist" becomes a MusicBrainz search term, and
  any release it matched would be attributed to an arbitrary track — strictly
  worse than the skip it replaced, because a wrong cover looks right.
- **Fill only; never rewrite a field the upstream supplied.** `Resolve` CLEANS
  the names it returns (`cleanDisplayName` strips `[65616303]`-style numeric
  brackets, `stripArtistDisambiguation` strips Discogs `*` suffixes), and the
  reference library is full of both. Writing a resolved value over a present tag
  would therefore silently normalise metadata the upstream owns — and because
  `walkFieldsEqual` compares Artist and Album, every one of the 13,341 tagged
  rows would differ from its stored twin on the next walk, re-upserting, resetting
  `enriched_at` to 0 and pushing an `indexed_at` bump: a full re-enrichment pass
  and a whole-library delta to every paired device, to change nothing anyone asked
  for.
- **Album identity is unchanged BY CONSTRUCTION, not by luck.** `Resolve`'s ladder
  is `albumArtist → artist → path default`, so writing `Artist = d.Artist` and
  `AlbumArtist = Artist` produces an identical `Resolved`, hence an identical
  `AlbumIDOf`. Pinned by `TestFillingDoesNotMoveAlbumIdentity` — without it, every
  affected album would change id on the next walk and each paired device would
  watch one vanish and another appear.
- **Deploy consequence, intended and bounded:** those 1,942 rows now differ from
  their stored twins, so the first walk after this ships re-upserts them once —
  `enriched_at` back to 0 (which is the point: they can finally be enriched) and
  one `indexed_at` bump each. Tagged rows are untouched.
- **Test lesson worth more than the fix.** `TestUpstreamMetadataAlwaysWins`
  originally used the fixture `"Tagged Artist"` — which cleans to itself, so it
  passed against code that overwrote unconditionally and pinned NOTHING. Only a
  negative control exposed it. When a test asserts "we do not transform X", the
  fixture must be a value the transformation would actually change. The same test
  is also LAYERED: a both-present row is protected first by the early return, so
  removing only the per-field guards leaves it green (removing both turns it red)
  — the partial-row case is what covers the guards directly.

- **A routed track's variant-skip reason must say it is ROUTED, and that case
  comes FIRST** (PR #814). With 0% bit depth from DIDL, every routed FLAC fell
  into `fundamentalSkipReason`'s geometry test and the reader was told "Format
  unreadable — variants not possible" about a file whose format is perfectly
  well known — 13,519 tracks on the reference library. Routed DOMINATES every
  other reason (the bridge has no local file to hand sox, so complete geometry
  changes nothing), so reporting geometry would be a second wrong answer rather
  than the first one fixed. It is a PARAMETER, not a check at the single call
  site that needs it today: the two browse sites pass `false` because their
  queries anti-join `upnp_track_routing` — verified empirically
  (`/api/library/browse?path=2go` returns zero rows), not assumed — and this
  session has been making UPnP first-class, so if routed rows are ever admitted
  to Browse the reason is already there to pass. Pinned by
  `TestRoutedTracksReportBeingOnAnUpstream` (which also re-states the whole
  pre-existing truth table, so the fix cannot trade one wrong badge for another)
  + `TestEverySkipReasonHasALabel` for the Go→JS half, since an unlabelled
  reason renders as the raw identifier.
> **SUPERSEDED by PR #824 (2026-09-01), which implemented this path.**
> `ingestOne` no longer refuses it — it now reports "has not answered yet",
> a network/typo diagnostic. The paragraph below is why it needed three
> surfaces rather than one, which is what #824 built. Kept for that reasoning.

- **`manualDescriptionURL` WAS CONFIGURED, VALIDATED, and UNIMPLEMENTED.**
  `config.Validate` accepts it and `StableServerKey` has a whole
  `manual:<sha256(url)>` branch, but `ingestOne` refuses it at runtime ("not yet
  supported"), so that branch is dead in production and reachable only from
  tests — which means the `manual:`-keyed half of `routedOnline` (PR #807) is
  defensive-only and cannot be exercised against real hardware. Implementing it
  needs THREE surfaces, not one: the walk's `ResolveControlURL`, `LiveHost` for
  playback (a miss is a hard 503 `upnp_server_offline` with NO fallback, so the
  library would ingest and then fail to play), and the online/offline status.
  Until then a network that blocks SSDP multicast cannot add an upstream at all,
  despite the config appearing to offer exactly that escape hatch. Known
  limitation with an honest error, feature-review P2-29.
- **The exempt bucket's "N of these need nothing" also absorbs routed tracks.**
  `exempt = total - eligible`, and its docblock says "tracks this kind can never
  apply to", so the BUCKET is right — but the phrase conflates "already fine"
  with "impossible", for DSD and lossy as much as for routed. The per-track badge
  is where the specific reason is told; left alone deliberately.
- **The sox probe is hoisted out of the per-track loop, and only an invocation
  COUNT can pin that.** `deps.SoxCanDecode` is the 30s-TTL toolchain cache behind
  a mutex and its answer is fixed for the life of a request, so asking it per
  track was a lock per track on a list that can be a 50k-path playlist. A source
  scan cannot tell the two apart — the call reads identically wherever it sits —
  so `TestToolchainProbeIsAskedOncePerRequest` counts real calls through a real
  request. Its first negative control BROKE THE BUILD (the revert left
  `canDecode` unused), which reads as "control invalid" rather than as evidence;
  re-run keeping the symbol referenced, it goes red. Same trap as every other
  "just disable this branch" mutation that deletes a variable's only use.
- **Process: a rate-limited bot's silence is not a pass, and it recurs.**
  CodeRabbit reviewed #813 genuinely (it lists the files it processed) but was
  rate-limited through BOTH rounds of #814 — "Review limit reached … 96 included
  PR review attempts over the past 7 days set your current allowance at 1 review
  per hour." A day of heavy PR traffic exhausts it, which is exactly when review
  matters most. Check for the notice before reading "no comments" as agreement;
  the distinguishing marker is the "Files selected for processing" block, which a
  real review has and a limit notice also carries — so match on
  "Review limit reached", not on file counts.

---

## The original "Things that have bitten before" list (2026-04 → 2026-08)

These 45 bullets were the first form of the invariant list, accumulated
chronologically as each bug was found. Their rules now live in CLAUDE.md's
by-subsystem digest; the full text is kept here for the reasoning, the test
names, and the PR provenance.

- **Byte-by-byte async iteration kills throughput.** Early `BridgeSourceClient` on the iOS side used `URLSession.bytes(for:)` which yields one `UInt8` per async step — 20M yields for a 20 MB file stalled the pipeline and surfaced as "Network connection lost" even over localhost. Fixed by switching to `URLSession.download(for:)`. Don't regress the iOS side back to byte-wise async reads; and don't add a server-side chunked-encoding mode that assumes byte-wise client consumption.
- **Anything walking FLAC metadata blocks SEEKS past a validated PICTURE payload — never drains it** (PR #568, regression of the #563 preflight). The single-open FLAC path ([extractors.go](internal/manifest/extractors.go) `.flac` branch) exists precisely because the 5–25 MiB embedded cover crossing the wire twice per track halved scanner throughput on NAS-mounted libraries. #563's `flacPictureBodySane` then deferred an `io.Copy(io.Discard, lr)` to re-align the walk, which re-read that payload for a verdict that needs ~30 bytes of header — and the caller `Seek(0)`s straight afterwards and hands the file to dhowden, which reads it again. `flacPictureBlocksSane` now records the body offset and `Seek`s to `pos+block.Length`. **This is only safe because `meta.New` reads the 4-byte header DIRECTLY from the reader and wraps it in a plain `io.LimitReader` — there is no internal buffering to desync** (`mewkiz/flac/meta/meta.go`); check that still holds before adding a seek anywhere else in that walk. Pinned by `TestFLACPictureBlocksSaneDoesNotReadThePayload` (byte-counting `io.ReadSeeker`, 4 MiB payload, ≤1 KiB budget) + `...LeavesWalkAlignedAfterPicture` (a misaligned walk bails fail-open and silently disables the allocation guard, so alignment needs its own pin).
- **MusicBrainz `release-group` is an object, not a string.** Decoded as `*releaseGroup` struct with `{id, title, primary-type}`. Public MB's live response has this shape; mock fixtures must too (`TestMusicBrainzDecodeRealResponseShape` locks it in).
- **Negative-cache MB errors.** On any MB search error, store an empty MBID under the `(artist, album)` cache key — otherwise sibling tracks on the same album re-query with the same inputs and hit the same error, turning a 1-track failure into an N-track spin loop. See `enricher.enrichOne`.
- **`enriched_at` on upsert resets to 0.** Any edit to the upsert SQL must preserve this reset — otherwise re-scans after a tag change don't re-enrich.
- **Every delta-visibility `indexed_at` bump goes through `indexedAtAdvanceSQL` — a bump must clear the LIBRARY-WIDE max, not just the row's own prior value** (2026-08-17, found on the `windows-latest` CI leg via `TestRestampDuplicates_PolicyFlipUnsuppressesViaDelta`). The long-standing `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END` form advances strictly relative to the row ITSELF; its `ELSE` arm assigns the raw clock, so when the clock equals a value **another row already holds**, the bumped row lands EXACTLY ON a cursor equal to that value and `indexed_at > since` filters it out. Windows' ~15.6 ms clock granularity makes that collision routine (reproducible ~1–5 times per 60–100 runs with a quantised clock; nanosecond clocks hide it entirely, which is why macOS/Linux stayed green). The replacement is `MAX(?, COALESCE((SELECT MAX(indexed_at) FROM tracks), 0) + 1)` — **BOTH terms are load-bearing**: the clock term anchors the value to wall-clock time because the cursor IS wall-clock (iOS sends its OWN `Date.now` captured at sync start — `LibraryScanner.syncStartedAt` → `share.lastScanFinishedAt`; `indexed_at` is never on the wire, so no client can derive a cursor from stored values, and a purely counter-based value would drift behind the cursor and break delta sync outright), while the `MAX+1` term clears same-tick siblings. It is a strict generalisation of the old form (`globalMax >= own`, so it subsumes the monotonic + strict-advance-own guarantees). Cost is one index seek — `idx_tracks_indexed` makes the subquery a `SEARCH … USING COVERING INDEX`, not a scan; `COALESCE` is required because SQLite's `MAX(x, NULL)` is NULL and the column is NOT NULL. Applied at all NINE bump writers (`MarkEnriched`, `applyReconciledTracks`, `UpsertVariant`, `DeleteVariant`, `UpsertAnalysis`, `DeleteAnalysis`, `SetArtworkVersionAndBumpIndex`, `SetBookletTagAndBumpIndex`, `ApplyDupeStamps`). **Three deliberate exclusions — don't "finish the job" by converting them:** the `UpsertTrack`/`UpsertTrackBatch` conflict arms (they compare `excluded.indexed_at` and write NEW content at wall-clock time on the hottest path — 500-row batches share one `now`, so a per-row subquery would make each row's value depend on evaluation order for a gain no client can observe); migration v34's `post()` `healTransitionBandBandwidths` (**shipped migrations are append-only and MUST NOT be rewritten** — both live bridges ran it, so editing it would change only fresh installs while diverging from what deployed DBs did); and `StampExtractorVersionBatch`, which is **not an `indexed_at` writer at all** (it stamps `extractor_version` / `missing_count` / `audio_md5` only — verify before adding it to any bump sweep). **Honest severity: this is a contract violation, not an active production data loss.** `ApplyDupeStamps`'s docblock claimed "strict-advances so `indexed_at > since` can't miss it" and delivered less; but because iOS's cursor is its own wall clock rather than a library-derived max, exact equality with a server nanosecond value essentially never happens in the field, and `indexed_at > ?` is the ONLY comparison consumer in the codebase (no internal sweeper derives a cursor from stored values). The fix makes the code match its documented promise and hardens the moment any consumer derives a cursor from server state. Locked by `TestIndexedAtBumpsClearTheLibraryWideMax` (all nine writers, table-driven), **negative-control-verified**: `MAX(?, indexed_at + 1)` is EXACTLY the old CASE WHEN with one bind, so it is the control that keeps compiling — all nine subtests go red under it, whereas a mutation that drops a bind breaks the build and a build break reads as "control invalid", never as a passing control.
- **Post-scan AlbumArtist reconciliation unifies split albums** (PR-pending; bridge-only, no `ProtocolVersion` bump, no iOS code change). The scanner sets `Track.AlbumArtist` from raw per-file tags, so an album whose tracks were tagged inconsistently (band vs leader — `Aspiration` vs `Peter Asplund; Aspiration`; a single comma-separated string vs the scanner's own `"; "` multi-value join) splits into 2+ album rows on iOS (album identity there is `normalize(albumArtist)|normalize(album)|year`), and cover art lands on only one row ("split-album syndrome"). `Scan` runs `runAlbumArtistReconciliation` at its success tail: `StreamTracks` → lightweight `[]ReconcileTarget` (Path/Album/AlbumArtist only — **never materialize every full `Track`**; OOM discipline for low-memory hosts like a Pi, matching the rest of the codebase's streaming, Gemini HIGH on PR #399) → pure `reconcileAlbumArtists` (`reconcile.go`) → `GetTrack` only the changed rows → `Store.ApplyAlbumArtistReconciliation`. A row deleted between the stream and the get is SKIPPED, not fatal. **Design invariants (consult 2026-06-14, don't relitigate):** (a) **directory-scoped** — group by `(trackDir(path), normalized album)`; **NEVER reconcile across directories** (protects classical box-sets with deliberate per-disc performers + distinct same-named albums like "Greatest Hits" / "Live" in separate folders). `trackDir` is separator-agnostic (`/` and `\`) for Windows hosts. (b) **dominant existing value, NOT MusicBrainz** — pick the most-frequent AlbumArtist (tie → longest, then lexicographic); preserves the user's curation (the enricher's `artist-credit` is deliberately NOT used to override tags, because MB's classical credits favour performers over composers and would shatter composer-sorted libraries). (c) **disagreement-driven** — a group whose values already agree is untouched; blank values are filled from the dominant (the compilation-flag hole) but an all-blank group is skipped. (d) **`ApplyAlbumArtistReconciliation` bumps `indexed_at` (strict-advance CASE-WHEN) but LEAVES `enriched_at` untouched** — reconciliation is a metadata-consistency pass, not (re-)enrichment; touching `enriched_at` would re-trigger the MB/CAA/Deezer treadmill (the wipe-loop class PR #369 fixed). (e) **`marshalForStorage` now also zeros `clone.Variants`** — reconciliation round-trips a `ListTracks` Track (variants spliced from the column) back through the write path, so without the strip it would freeze stale variants into `tags_json` where the JSON-only readers (`GetTrack` / `UnenrichedTracks`) would surface them. **iOS heals with no change**: on the next delta-sync the reconciled track's `albumID` changes, `upsertBridgeTrack` touches BOTH the old + new albumIDs (`BridgeSyncActor.swift:688/703`), `recomputeCounts` prunes the orphaned old split-album row, and the unified album re-fetches its cover. Runs every `Scan` (cheap DB-only pass; a stable library produces zero writes) — that's ALSO the migration path for existing libraries (first scan after deploy unifies them; no re-enrichment). **Don't touch `enriched_at` in the reconciliation write**, **don't reconcile across directories**, **don't override with MB credit**. Locked by `reconcile_test.go` (dominant vote / separator unification / compilation-blank fill / cross-dir + box-set safety / loose-track skip / Windows paths). **Year reconciliation rides the same shape (PR #419, merged + deployed bridge.ars.md 2026-06-20):** `Scan` runs `runYearReconciliation` right after the AlbumArtist pass — `reconcileYears` (`reconcile.go`) fills a MISSING album year (nil OR the bridge's `year=0` "tag absent" sentinel) from the group's DOMINANT present year, reusing the SAME `albumArtistGroupKey` (directory + normalized album) so the directory-scoping / box-set protection is byte-identical. **FILL-MISSING ONLY** — a present-but-DIFFERENT year is LEFT ALONE (overwriting a deliberate per-track year is the sharper, riskier call; deferred). Fixes the field-reported Alphaville "[A] Eternally Yours Bonus-EP" split (one untagged bonus track with no year split off as its own 1-track album). `Store.ApplyYearReconciliation` delegates to a shared `applyReconciledTracks` helper extracted from `ApplyAlbumArtistReconciliation` (same indexed_at-strict-advance / enriched_at-untouched contract). The year stream DEEP-COPIES `t.Year` (the pointer analog of the AlbumArtist value-copy — honours StreamTracks' "callbacks MUST NOT retain" contract literally, robust to a future pointer-reusing unmarshal; Gemini suggested dropping it as redundant, declined for contract-independence at negligible cost). Locked by 10 `TestReconcileYears` cases (fill / year=0 sentinel / present-but-different no-overwrite / fill+leave-different / cross-dir safety / all-missing / tie-break-to-larger / lone-track / empty-album). **Don't extend to overwrite present years, don't cross directories** (same protections). Note both passes are directory-scoped, so a cross-disc-subfolder albumArtist split (e.g. `…/3/CD 01/` tagged `Abdullah Ibrahim` vs `…/3/CD 02/` tagged `ABDULLAH IBRAHIM, Composer`) is NOT auto-healed — that's a source-tag fix (Picard / a direct `mutagen` edit on the file), not a reconciliation case.
- **Metadata-correctness batch (PRs #222 + #223 + #224 + #225 + #226 + #227 + #228)** — paired with iOS PRs #373 + #374. Six bridge PRs, one Mirror-PR pair. (1) **PR #222** ports the `CASE WHEN tracks.indexed_at >= excluded.indexed_at THEN tracks.indexed_at + 1 ELSE excluded.indexed_at END` form from `UpsertVariant` / `MarkEnriched` into `UpsertTrack` / `UpsertTrackBatch` — closes the same-nanosecond delta-sync miss the variant path already guarded against. Uses SQLite's `excluded` reference so bind count stays at 5. Batch semantics: `now` computed once per flush; CASE WHEN holds per-row. (2) **PR #225** adds `isLossyCodec(codec)` helper + structural defense-in-depth gate at every `t.BitsPerSample` write site (AAC/MP3/OGG/OPUS/WMA refuse the assignment). Codec stamp hoisted BEFORE bits-write in FLAC + DSF paths. `populateFromTagMetadata` gains explicit "MUST NOT set t.Codec from m.FileType()" docblock. DSF IsDSD flipped from strict `bitsPerSample==1` to default-true (since `.dsf` is structurally DSD) with `isValidDSDSampleRate` sanity floor refusing the flip on PCM-like rates (44100, 48000, …). **Bridge isn't currently emitting wrong bits for AAC** — extractors leave nil + omitempty suppresses — so this is forward-defense against a future enricher addition that would re-introduce the iOS PR #371 "M4A 32-bit" chip regression. (3) **PR #226** wires presence-gated assignment for Year / TrackNumber / DiscNumber via the existing `stringOf` helper. dhowden returns 0 for BOTH "tag absent" and "value is 0"; the raw-map presence check distinguishes them (explicit zero still surfaces as Some(0); absence drops to nil). Aliases passed lowercase to match `normaliseRawTagKey`. (4) **PR #227** adds `applyMultiValueArtistsFromRaw` — MP4 multiple-data-atom multi-value via `[]string` detection in `m.Raw()`. ID3v2 multi-value is documented as a limitation (dhowden's `readTFrame` strips NULL bytes via `strings.Join(strings.Split(txt, string([]byte{0})), "")`); the NULL-separated-string branch is forward-compat insurance against a future dhowden release that preserves NULLs. (5) **PR #228** adds five wire-additive fields to `Track`: `Composer string`, `Conductor string`, `Work string`, `OriginalYear *int`, `BPM *int`. Populated via `stringOf` (lowercase aliases). **TIT1 → Work, TIT2 → Title** — classical convention; iOS PR #374's work-grouping helper depends on this exact split. New `parseYearPrefix` helper handles both bare YYYY (TORY) and ISO-8601 (TDOR). ProtocolVersion stays at 1 (additive). (6) **PR #223** extends `extractDFFWithContext` to walk past PROP + DSD audio chunk and find the optional DIIN container — DITI / DIAR / DIAL / DIGN sub-chunks decoded as DSDIFF pstrings (1-byte length + N bytes + pad-if-(1+N)-odd). COMT recognised and skipped. Per-pstring bounds check refuses overruns; sub-chunk loop bounded by declared chunk size to defend against malformed files. (7) **PR #224** adds dedicated AIFF / WAV extractors. dhowden/tag explicitly does NOT support these containers; the new walkers find embedded `ID3 ` (or lowercase `id3 ` per RIFF spec) chunks and feed them to `tag.ReadID3v2Tags`. WAV also walks LIST/INFO for INAM / IART / IPRD / IGNR. ID3 wins over LIST/INFO via empty-field guards.
- **MP4 © atoms need `normaliseRawTagKey` to canonicalize the `0xA9` lead byte — and every extraction-logic fix bumps `manifest.ExtractorVersion`** (PR #586, paired with a user report on app 1.7). dhowden surfaces MP4 ilst atoms under a raw map key with a SINGLE `0xA9` lead byte (`\xa9day`, `\xa9ART`, `\xa9wrt`), which is invalid UTF-8. `hasAnyRawKey` / `stringOf` / `extractMultiValueTagFromRaw` normalize only the MAP KEY via `normaliseRawTagKey` (`strings.ToLower`), and `ToLower` rewrites the lone `0xA9` to `utf8.RuneError` (`0xEF 0xBF 0xBD`) — so the source-literal aliases (`"©day"` etc., UTF-8 `0xC2 0xA9`) NEVER matched. Result: **year / composer / multi-value artist silently dropped for M4A/ALAC** (regressed the `stringOf` presence-gate + `applyMultiValueArtistsFromRaw` work in PRs #222–#228; present in 0.1.7 AND 0.1.8). Fix: `normaliseRawTagKey` canonicalizes a LEADING `0xA9` to UTF-8 `©` BEFORE `ToLower` (`if strings.HasPrefix(k, "\xa9") { k = "©" + k[1:] }`) — prefix-anchored (MP4 © atoms are always byte 0, so an interior `0xA9` continuation byte in a multibyte key is untouched); behavior-preserving for every ASCII key (ID3/Vorbis frames + MP4 non-© atoms don't start with `0xA9`). **Don't reintroduce a raw-byte alias `"\xa9day"`** — it would be compared against the mangled `"�day"` and still miss; the canonicalization is the ONLY fix that lets the readable UTF-8 aliases match. **The masking was invisible because the tests used UTF-8 `"©ART"` map keys** (which match the UTF-8 alias but don't reproduce dhowden's single-byte key) — the `©`-atom tests now use the real `"\xa9…"` byte keys (red→green negative-control-verified). **Self-healing re-extraction**: a per-track `extractor_version` column (migration v27, stamped by BOTH `UpsertTrack` / `UpsertTrackBatch`) records the `manifest.ExtractorVersion` that produced the row's tags; `runScanWorker`'s size+mtime skip-gate re-extracts any row whose stamp `< ExtractorVersion` — the check is on the OUTER `&&`, NOT the inner artwork-recovery block (the inner would take the light `ResetTrackMissingCount`+return and never re-extract). So the first scan after this ships re-extracts the whole library once and the missing M4A years populate with **no operator wipe**. **BUMP `ExtractorVersion` (const in `extractors.go`, currently 1, MUST stay ≥ 1 — at 0 the `stored >= current` gate is `0 >= 0` and nothing re-extracts) on EVERY future extraction-logic change** so it auto-reapplies; the `= excluded.extractor_version` line is MANDATORY in both upserts (else a re-extracted row keeps its stale stamp and re-extracts every scan). Mirrors the `analyze.WaveformSchemaVersion` stamp idiom. Bridge-only, **no `ProtocolVersion` bump** (populates the existing `year` field; iOS shows it after a normal `runBridgeSync`). Locked by `extractors_mp4_copyright_atom_test.go` (year / composer / multi-artist via real `\xa9…` keys + direct `normaliseRawTagKey`) + the converted `©`-atom cases in `extractors_multi_value_test.go` / `extractors_presence_gate_test.go`.
- **Optional `Track` numeric/bool fields are pointers** (PR #51): `IsDSD`, `TrackNumber`, `DiscNumber`, `Year`. Non-pointer + `omitempty` silently drops zero/false from the wire — iOS's `Bool?` / `Int?` decoders end up with `nil` instead of `Some(false)` / `Some(0)`. `extractFLACFormat` sets `*IsDSD = false` explicitly so iOS can trust `isDSD: false` to mean "definitely PCM"; `extractDSF` sets `*IsDSD = true`. `extractDFFWithContext` (PR #186, v1.2) walks just the FRM8 / PROP / FS / CMPR chunks: stamps `Codec="DFF"` always, plus `IsDSD=true` + `SampleRate` + `BitsPerSample=1` for uncompressed DSDIFF; DST-compressed payloads (CMPR FOURCC = `DST `) leave the DSD stamps nil so iOS classifies them as unknown audio rather than DSD that fails to load. iOS-side companion in `LibraryScannerActor.enrichDFF` walks the same chunk shape via SMB `readRange`. The dhowden-tag fallback path leaves all three integer pointers populated (always non-nil) — the underlying tag library returns 0 for both "absent" and "explicit zero" with no way to distinguish.
- **`fillFromPath` is multiRoot-aware** (PR #186, v1.2). When `multiRoot==true`, `relPath` prefixes every track's library-relative form with the root's basename (e.g. `Music/Pink Floyd/Dark Side/Money.flac`). The album/artist heuristics MUST strip `parts[0]` before evaluating the trailing segments — pre-fix, an untagged file directly under a root named like an artist (the field-reported "Alphaville bug") inherited the root basename as its Artist for every album in the root. The `multiRoot bool` flows from `Scan` / `ScanSubtree` through `runScanWorker` into `fillFromPath`. Single-root scans don't carry the prefix and pass through unchanged. **Don't drop the `multiRoot` parameter** without rethinking the prefix-stripping contract.
- **`Retry-After` is honored on MB and iTunes 429/503 responses** (PR #50, PR #52). `parseRetryAfter` lives in `internal/enrich/musicbrainz.go` and is reused by `ITunesClient.get`, `ITunesClient.FetchArtwork`, and `MusicBrainzClient.get`. Caps at `maxRetryAfter = 1h` to prevent a hostile/misconfigured upstream from parking the enricher indefinitely; the cap applies in the *seconds* domain BEFORE multiplying by `time.Second` (overflow guard) AND on `strconv.ErrRange` overflow (which `ParseInt` returns for >int64-max values). `time.Sleep` for pacing is replaced with `sleepCtx` in the iTunes path so shutdown isn't blocked by up to 2× ITunesMinInterval per in-flight call.
- **iTunes is a fallback, not a primary source** (PR #52). The artwork chain is MB → CAA-release → CAA-release-group → iTunes. iTunes hits cache under the MB-derived release MBID (`<MBID>-<size>.jpg`), keeping the wire shape unchanged — `/v1/artwork/{mbid}` serves whatever path matches. Skipping MB entirely when iTunes hits would require a synthetic-MBID cache key OR relaxing the strict-UUID `mbidPattern` regex on the artwork handler; both are out of scope until the wire shape gets a proper rev. The `itunesFallbackHits` atomic counter tracks how often iTunes salvaged a CAA miss — useful telemetry for whether the fallback is pulling weight.
- **mDNS TXT records carry `host` + `port`** (PR #53). Without these, iOS would have to NWConnection-resolve the Bonjour service to its hostport form, which is unreliable on iOS 26.4 (`currentPath?.remoteEndpoint` stays in `.service(...)` form even at state `.ready` time). The bare-hostname-plus-`.local` form `cfg.advertisedHost()` produces matches the SRV target the bridge has always advertised, so the cert SANs cover it without any extra change. The function falls back to `localhost.local` if both `cfg.Hostname` AND `os.Hostname()` come back blank, so we never emit `host=.local` (which would build invalid client URLs). `Port` is validated as `1..=65535` at `Advertise` time — out-of-range would land on the wire and have iOS construct unusable URLs.
- **Scanner deletion pass MUST honor `errorSubtrees` from `walkRoot`** (PR #74). A transient `WalkDir` err callback (NAS drop, antivirus lock, permission flap) leaves affected paths outside `seen`; the deletion pass would otherwise `DeleteTrack` every one of them. **Files on disk are unaffected, but the manifest goes empty until the next clean scan repopulates it — this is the most likely cause of any user report of "bridge lost my library."** Recovery is `bridge serve` after the transient condition clears. Implementation: walker writes affected dir paths into `errorSubtrees`; deletion pass spares any candidate matched by `isUnderErroredSubtree`. Sentinel normalization handles BOTH single-root (`relPath` returns `"."`) AND multi-root (`relPath` returns `"<rootBase>/."`) — without both forms a whole-root outage spares zero tracks. `TestScannerSparesTracksUnderWalkErrorSubtree` (uses `chmod 0`, `//go:build !windows`) + `TestScannerStillDeletesTracksUnderHealthySubtree` + `TestIsUnderErroredSubtree` (table test covering single-root `"."`, multi-root `"<root>/."`, sibling-name false-match guard) pin the contract end-to-end. **`isUnderErroredSubtree` MUST be evaluated BEFORE every other branch in the deletion loop that can delete** (PR #568, after #549 re-broke this). #549 added a case-fold `renames` immediate-reap branch and placed it AHEAD of the sparing check, so a path absent from `seen` because its subtree errored, which case-fold-matched a path that WAS seen, was deleted outright — bypassing both this guard and the `missing_count >= threshold` debounce. On a case-sensitive FS `Album/` and `album/` are distinct real directories, so a permission flap on one reaped live rows of the other (a whole-root outage stayed safe: `seen` is empty, so nothing fold-matches). The rule generalises past that one branch — **"we could not see this path" must dominate every "…but it looks like X" classification**, because each such branch is a fresh chance to delete rows the walk never observed. Pinned by `TestScannerSparesCaseTwinUnderWalkErrorSubtree` (skips on case-insensitive FS + as root, where the bug is unreachable / the fixture can't be staged). New scanner tests should use the shared fixture helpers `seedTrackDirs` / `newScanFixture` / `scanOnce` / `mustIndexed` / `caseSensitiveFS`, which live in the **untagged** `scanner_fixture_test.go`; `breakWalk` stays in `scanner_walk_error_test.go` because `chmod 0000` doesn't block traversal on Windows. **Put new shared scanner fixtures in the untagged file, never in a build-tagged one.** They were all in the `//go:build !windows` file until 2026-08-04, and untagged siblings (`scanner_extractor_version_test.go`, then PR #606's `scanner_disc_art_heal_test.go`) referencing them made the whole `manifest` test binary fail to COMPILE for windows/amd64 — invisible because no workflow had ever built this package for Windows.
- **SQLite `LIKE` folds ASCII case — any path predicate that writes, deletes, or bounds a scope MUST be a byte range.** Nothing sets `case_sensitive_like` (the DSN in `OpenStore` sets only `journal_mode`/`busy_timeout`/`foreign_keys`), so `path LIKE 'prefix/%'` matches a case-twin sibling directory — which on a case-sensitive filesystem is a DIFFERENT directory. Use `path COLLATE BINARY >= ?||'/' AND path COLLATE BINARY < ?||'0'` ('0' is the ASCII successor of '/'), binding the `TrimRight(prefix,"/")` base twice; it also rides the BINARY-collated `path` PRIMARY KEY where LIKE forces a full scan. Non-ASCII prefixes need nothing extra — UTF-8 continuation bytes are all ≥ 0x80 and sort after both '/' (0x2F) and '0' (0x30). PR #532 converted the rollup/count helpers for PERFORMANCE and recorded the assumption it relied on ("numerically identical … for folder-derived prefixes, **which never differ only by case**"); the sites that don't take a folder-derived prefix were left behind, and that is where it bit: **`DeleteTracksByPrefix` takes a library-root BASENAME** (removing `/srv/Music` deleted `/srv/music`'s rows AND unlinked its variant + waveform sidecars, while the pre-confirm count from the already-case-exact `CountTracksByPrefix` understated the damage), and **`TrackPathsUnder`/`FolderPathsUnder` take a watcher-supplied directory** — a case-twin's rows entered `ScanSubtree`'s scope snapshot, were absent from `seen` because the walk never visits that directory, and `caseOnlyRenames` then reaped them OUTRIGHT, bypassing the `missing_count` debounce. That immediate reap is sound only under caseOnlyRenames' stated premise ("a fold-match refers to a file the walker DID enumerate this pass") — true for a full `Scan`, false the moment the SNAPSHOT is broader than the WALK, and `isUnderErroredSubtree` can't help because there is no walk error. Three rules: **`DeleteTracksByPrefix` must ERROR on an empty base** (the range's natural reading is "everything"; whole-library deletion is `WipeAllTracks`/`WipeFilesystemTracks`); **`FolderPathsUnder`'s `path = ?` term is load-bearing** (it is the only thing matching the directory's own row and the multi-root `<base>/.` sentinel — the range covers strictly descendants); and **the sidecar enumerations must use the SAME bounds as the DELETE**, since they unlink files. `subtreeRangeBase` is the shared derivation; `trackScopeBase` handles the `/.` sentinel via the two-char suffix so a real directory ending in a dot isn't mis-trimmed. `subtreeLikePattern` survives ONLY for the deliberately case-folding callers (`ListVariantsUnderPrefix`'s `unicode_lower` GC lookup, PR #477) and the display-only `ListTrackProjectionsUnderPrefix`; **if you can't say why folding is wanted, you want `subtreeRangeBase`.** `ValidateRoots` + both remove-root guards (admin and CLI) now key off the shared `bridgefs.FoldRootBasename` so a case-twin root pair is refused at config time and a destructive removal that reaches one anyway is refused rather than guessed. The browse-side readers (`ListChildFolders`/`ListChildTracks` + Count/Page twins) are still on LIKE — display-only, tracked as follow-up. Locked by `store_prefix_case_test.go` + `scanner_subtree_case_test.go` (both negative-control-verified against the LIKE form; the case-twin fixtures skip on a case-insensitive FS, so verify on Linux or a case-sensitive macOS volume).
- **Scanner per-iteration panic recovery in `runScanWorker`** ([scanner.go](internal/manifest/scanner.go)). dhowden/tag and the project's own DSF/DFF parsers can panic on malformed files (truncated ID3v2 headers, bad MP4 atom trees, FLAC blocks lying about their length). Pre-fix a panicked extraction took down the entire worker goroutine and reduced pool capacity for the rest of the scan — on a single-CPU host that stalled the whole scan. The recover lives **inside** the per-iteration func wrapping the work (`func() { defer func() { if r := recover(); r != nil { ... } }(); ... }()`), NOT around the whole `for pi := range paths` loop — so a panicked file logs + skips and the worker proceeds to the next path on the channel. Mirrors the `processJob` recovery pattern in `internal/transcode/pool.go`. Logging uses `scanLogger` (component=scanner) at `Error` level with `path: pi.abs` + `panic: r` + `stack: string(debug.Stack())` so the operator has the absolute path AND a crash address to file an upstream bug. `Scanner.panickedCnt atomic.Int64` exposes the cumulative count via `PanickedCount()` — surfaced in the admin "Library" dashboard. `afterExtractHookForTests` (file-scope, nil in production) is the test seam used by `TestScannerWorkerRecoversFromExtractPanic` — production cost is one nil-check per file, negligible against the actual extract work. **Don't move the recover OUTSIDE the per-iteration func** — that's the whole point: the worker stays alive across panicked files.
- **Scanner excludes its own optimize/upscale variant sidecars from the walk** (PR #475). The per-file inclusion gate in BOTH `WalkDir` callbacks (full `Scan` + incremental subtree) is the shared `enqueueableAudioFile(abs, name)` = supported audio ext AND `!isVariantSidecarName(name)`. Variant sidecars are `<srcBase>.<variantID>.flac` (variantID `<kind>-v<schema>-<rate>-<bits>`, kind ∈ upscaled/optimized) written under `upscale.variantsDir`, served on-demand + aggregated onto their PARENT track's manifest entry — NEVER standalone library content. Pre-fix the extension-only gate indexed them as tracks whenever they landed inside a scanned `libraryRoot` (field case: a `variants/` folder inside a read-only B2 library bucket → **6,516 phantom rows, ~26% of a 24k-track library, across 457 albums**, each doubling with a no-embedded-art downscaled copy that ALSO reached iOS since `ListTracks` / `/v1/manifest` has no variant filter — this is what surfaced as the "2L — The MQA Experience" duplicate-tracks report). `isVariantSidecarName` is **ANCHORED, not a loose `.optimized-`/`.upscaled-` substring** (which false-positives on a real `Song.optimized-Mix.flac` — both Gemini + CodeRabbit flagged this on PR #475): strip the trailing `.flac`, the final dot-segment must fully match `^(upscaled|optimized)-v[0-9]+-[0-9]+-[0-9]+$` (**version-agnostic** — a future schema is still excluded) AND the part before it must itself be a supported audio ext (the source file). Reuses the `VariantKindPrefix*` constants (mirrored in `manifest` to dodge the `transcode`→`manifest` import cycle). A re-scan after deploy reaps the phantom rows via the existing bounded deletion pass (they stop appearing in `seen`); iOS heals on the next delta-sync (`recomputeCounts` prunes the orphaned duplicate tracks). Bridge-only, **no `ProtocolVersion` bump**. **Don't revert to the extension-only gate**, **don't loosen the anchored match back to a substring**, **don't skip a whole `variants/` DIR by name** (a legit album folder could be named that — the filename anchor is the robust catch). **Operationally** variants shouldn't be inside a `libraryRoot` at all (`variantsDir` is meant to be a separate path); the scanner exclusion is defense-in-depth, and cleaning the `variants/` folder out of the source bucket is the complementary operator fix. Locked by `TestIsVariantSidecarName` (anchoring truth table) + `TestScannerSkipsVariantSidecars` (end-to-end: real track indexed, sidecars under `variants/` AND next-to-source excluded).
- **Enricher MUST classify `SearchRelease` errors via `IsTransient`** (PR #74). Pre-fix, every error path went straight to `markSkipped(t, "MB error: %v")` which stamps `enriched_at` — a 30-second MusicBrainz outage permanently poisoned every track currently being enriched (the `WHERE enriched_at = 0` query never picked them up again). Now: transient errors (HTTP 5xx, 429, `net.Error.Timeout`, ECONNRESET, deadline-exceeded) return without stamping; persistent errors (HTTP 4xx other than 429, 404, JSON decode) still markSkipped. CRITICAL: do NOT cache an empty resolution on transient errors — sibling tracks on the same album would hit the cache, see empty resolution, and call `markSkipped(t, "no MB match")` anyway, defeating the retry. The HTTP-code parser MUST be structured (`strings.HasPrefix(msg, "musicbrainz: HTTP ")` then `strconv.Atoi`), NOT substring-match, otherwise persistent 4xx whose body contains "HTTP 503" / "HTTP 429" misclassifies as transient and the worker retries forever. `TestIsTransient_PinsClassification` covers the full table including the body-mention false-positive guard.
- **Default `ScanIntervalSec` is 21600 (6h), not 3600** (PR #75). A 50k-track library on mechanical NAS with the prior 1h cadence spun the disks every hour preventing idle spindown — operator-facing wear hazard. Operators with quiet libraries should set this higher; admin console exposes on-demand triggers. Don't drop to fsnotify without a design pass — Windows path semantics + recursive watch fan-out on large libraries are real cross-platform pitfalls.
- **Admin `http.Server` sets `ReadTimeout` + `IdleTimeout` but NOT `WriteTimeout`** (PR #75). Slowloris-class clients trickling bytes 1/sec held FDs open indefinitely under the prior `ReadHeaderTimeout`-only config. WriteTimeout MUST stay unset because admin handlers expose long-running synchronous endpoints (`/api/updates/install` binary swap, `/api/backups` snapshot) that legitimately take minutes — a 60s WriteTimeout would tear the response mid-flight and leave the operator's UI stuck on "loading" while the server-side op continues in the background (qodo bot review). The Slowloris-class write-trickle is not a meaningful threat on a loopback admin listener anyway; IdleTimeout reaps the kept-alive socket pool which is the realistic FD-exhaustion vector. The public-facing `cmd/bridge/main.go` server's timeout config is unchanged (different threat model, different invariants).
- **`auth.RecordClientVersion` truncates at a UTF-8 rune boundary** (PR #75). Pre-fix, `ver = ver[:maxClientVersionLen]` byte-sliced at index 64; landing mid-rune persisted malformed UTF-8 to `tokens.json`. The fix loops `for !utf8.ValidString(ver) && len(ver) > 0 { ver = ver[:len(ver)-1] }` to trim back to the last valid boundary. Cheaper than a `[]rune` round-trip; same correctness. `TestRecordClientVersion_TruncatesAtUTF8Boundary` covers a 65-byte string with a 3-byte rune at byte 62; `TestRecordClientVersion_ASCIIBoundaryUnchanged` is the regression guard ensuring the UTF-8-safe path doesn't over-truncate pure-ASCII inputs.
- **Destructive CLI commands require typed-phrase confirmation** (PR #75). `actUninstall`'s wipe prompt requires the literal string `WIPE` (exact match, no prefix-match) before `os.RemoveAll(cfgDir)` — mirrors `actInstallService`'s `INSTALL-AS-ROOT` pattern. The bare `[y/N]` prefix-match on `"y"` was a fat-finger hazard. Preamble explicitly states what WILL be deleted (config, data, certs, tokens) AND what will NOT — the user-supplied `--library` path is read-only by design; bridge has NO code path that can delete library files (verified during PR #74 exploration). Always derive `cfgDir` from `filepath.Dir(s.cfgPath)` not `packaging.DefaultConfigDir()` — the two diverge when the bridge was initialized via a non-default `--config` path (gemini bot review).
- **Admin API `csrfGuard` middleware** (PR #76). Layered between `loopbackOnly` and the mux. **Body-bearing** mutating requests (POST/PATCH/PUT/DELETE) MUST carry `Content-Type: application/json` (case-insensitive, charset suffix tolerated); `text/plain` / `application/x-www-form-urlencoded` / `multipart/form-data` rejected with 415. Body detection uses `r.ContentLength != 0 || len(r.TransferEncoding) > 0` because net/http strips `Transfer-Encoding` from the header during request parsing — `r.Header.Get("Transfer-Encoding")` would be dead code (CodeRabbit on PR #76's first push). **Origin allowlist** is reject-if-mismatched, NOT reject-if-missing — Firefox/Safari sometimes omit `Origin` for loopback navigations and failing closed locks legitimate operators out. Origin parsing uses `url.URL.Hostname()` / `Port()` to handle bracketed IPv6 (`http://[::1]:7789`). `loopbackHostname` strips `[]` before `net.ParseIP`. CSRF defends against drive-by cross-origin browser POSTs that the loopback bind alone can't catch (the attacker page runs in the same UA that has loopback access). When extending the admin JS to send mutating requests, ALWAYS include `content-type: application/json` even on bodyless POSTs — `app.js` does this through `API.post` plus an explicit set on the bare `/api/restart` fetch.
- **SQLite writer contract: every writer holds `Store.mu`** (PR #76). Methods that issue `INSERT`/`UPDATE`/`DELETE` SQL — `UpsertTrack`, `UpsertTrackBatch`, `DeleteTrack`, `DeleteTracksByPrefix`, `MarkEnriched`, `WipeAllTracks`, `UpsertFolder`, `SetScanState` — all hold `s.mu` so SQLite never sees two concurrent writers. **Reads stay un-mutexed** (WAL handles concurrent readers). Pre-fix, `DeleteTrack` (called from the scanner's deletion pass) and `MarkEnriched` (called from the enricher) raced under load — surfaced as `database is locked` after the 5s busy_timeout. **Do NOT** set `db.SetMaxOpenConns(1)` — that would defeat WAL's concurrent-reader benefit. The Go-side mutex serialises writers BEFORE they reach SQLite, so busy_timeout retries are never triggered. The mutex ALSO protects multi-statement transactions (`UpsertVariant`'s INSERT + UPDATE parent) and `SELECT sidecars → DELETE rows → os.Remove files` ordering in `DeleteTrack` / `DeleteTracksByPrefix` / `WipeAllTracks` — `busy_timeout` is a retry, not a serializer, so removing `s.mu` and "letting SQLite handle it" would tear those sequences and leave readers seeing rows pointing at deleted files. Reviewer-suggested removal of `s.mu` is therefore declined.
- **`Track.Enriched` uses per-row `boolPtr(b bool) *bool` allocation in read paths** ([store.go](internal/manifest/store.go) `ListTracks` / `StreamTracks` / `ListTracksPage`). Prior shape used two package-level singleton vars (`enrichedTrue` / `enrichedFalse`) shared across every read to avoid 50k heap allocs per scan. The singletons created an external-mutation footgun because `Track` is exported: a downstream consumer writing `*track.Enriched = true` would have clobbered every subsequent read for the process lifetime (no internal mutation site exists today, but that's not a guarantee the next caller honours). Per-row allocation is structurally safe and the cost is in the noise: 50k tracks × (8-byte pointer + 1-byte bool) ≈ 450 KB total, negligible against the SQLite query + JSON marshalling that already dominate the read path. **Don't reintroduce package-level singleton bool pointers** for the same exported-field-mutation reason.
- **Enricher caches are bounded LRUs (`internal/lrucache`), not `sync.Map`** (PR #76). `albumCache` (50k), `releaseGroupCache` (50k), `artistCache` (50k), `deezerNegCache` (100k). Pre-fix, all four grew for the lifetime of the process — a long-running bridge with a multi-decade library slowly leaked memory. Map underlying storage is **pre-allocated at full capacity** at construction (`make(map[K]*list.Element, capacity)`) so the bulk-ingestion phase of a 50k-track scan doesn't trigger Go map bucket resizing mid-flight. `deezerNegCache` lookup is presence-only via `Has()` (does NOT promote to MRU) so a sibling-track read doesn't keep a stale negative entry alive past a positive Deezer re-fetch. Cache size constants (`albumCacheCap`, etc.) are tunable in code but not config-exposed; raise only if telemetry shows hot working sets approaching the cap.
- **Schema migrations use `PRAGMA user_version` ladder** (PR #76). `internal/manifest/store.go` defines `migrations []migration{...}`; `migrate()` walks the slice and applies any whose `version` exceeds current `user_version`, bumping the stamp after each step. Migration 1 is the v1.0/v1.1 baseline (idempotent `IF NOT EXISTS` + swallowed-`ALTER` post-DDL fallback for in-place upgrades from a pre-`enriched_at` install). Pre-ladder DBs (created before PR #76) carry `user_version = 0` plus the v1.1 schema — re-opening runs migration 1 idempotently and bumps the version. **Append-only**: never reorder or rewrite existing migrations; new migrations append to the slice. Migration `sql` MUST be idempotent (`IF NOT EXISTS`, `OR REPLACE`) — a crash mid-migration and restart should re-run cleanly. Tests pin head-version assertions against `migrations[len(migrations)-1].version` so adding a migration doesn't break them.
- **Admin shutdown waits for `bgScans` WaitGroup** (PR #76). `Server.bgScans sync.WaitGroup` tracks every `spawnBackgroundScan` goroutine. `Serve()`'s shutdown branch waits on the WG (capped at the same 5s grace as the HTTP listener) before returning. Concurrent-drain via `select` so a stuck scan can't wedge process exit indefinitely. Don't reintroduce a fire-and-forget `go func()` in `spawnBackgroundScan` without re-wiring the WG — a process exit during a mid-write scan can corrupt SQLite.
- **Structured logging via `internal/logging`** (PR #81). All `internal/` packages declare `var logger = logging.Component("name")` at package scope (one per package — `admin`, `api`, `auth`, `enricher`, `manifest`, `scanner`, `tls`, `updater`, `watcher`). CLI commands in `cmd/bridge/` keep `fmt.Fprintf(stdout/stderr)` — those are user output, not telemetry. **Critical: `logging.Component` returns a logger backed by a `dynamicHandler` that resolves `slog.Default().Handler()` at log time**, NOT at construction. This is load-bearing because Go evaluates `var logger = logging.Component(...)` during package init, BEFORE `main()`'s `logging.Init()` runs — a captured-handler shape would lock loggers to the pre-Init handler forever, and Windows-service log redirects would never reach them. `dynamicHandler.WithAttrs` / `WithGroup` accumulate the caller's intent locally and re-apply at Handle time so chained `.With(...)` survives the indirection. Convention: `.Error` for caught failures, `.Warn` for degraded-but-functional, `.Info` for state transitions worth a line. Attribute keys are short and stable: `path`, `mbid`, `addr`, `rows`, `err`. **No `log.Printf` anywhere in `internal/`** — if you re-introduce one, the migration is incomplete.
- **`bridge library add/remove` auto-detects running bridge** (PR #82). Probes `AdminAddress` with a 200ms client; on success forwards the mutation to `/api/roots`; on connection-refused / non-200 falls through to direct YAML + `manifest.Store` mutation (offline path). **Use SEPARATE `http.Client` instances** for the probe (200ms) and the mutation (30s) — `http.Client.Timeout` caps the entire request duration regardless of the request context, so reusing the probe's client for the mutation would fail at 200ms even with a 30s `context.WithTimeout` (Qodo on PR #82). The offline `library remove` path defensively refuses when another configured root shares the basename of the target — `bridgefs.ValidateRoots` (which the admin API runs at add-time) prevents this, but a hand-edited `bridge.yaml` that bypassed the CLI/admin would otherwise have `DeleteTracksByPrefix(basename+"/")` wipe tracks from the surviving root with the same basename.
- **`bridge logs -f` follow loop owns the `*os.File`** (PR #82). `followLog` has its own `defer f.Close()` because it reassigns `f = nf` after a rotation (the file shrinks below the last known offset). The outer `logsCmd` `defer f.Close()` is harmless (idempotent close on already-closed fd) but `followLog`'s defer is the load-bearing one — without it, every rotation leaks a file descriptor (Gemini High on PR #82). `followLog` uses a single `time.Ticker` instead of `time.After`-in-a-loop. `tailFile` accumulates chunks into `[][]byte` and joins once — pre-fix `buf = append(piece, buf...)` was O(N²).
- **`bridge status` decodes endpoints as `[]any`, not `map[string]any`** (PR #82). `/api/endpoints` returns a JSON ARRAY (`[]adminEndpointEntry`) — decoding into `map[string]any` silently produces nil and the human-readable output never paints the endpoints section. `fetchAdminJSON(ctx, addr, path, dst any)` takes a destination so each caller decodes into the correct shape. `isConnRefused` uses `errors.Is(err, syscall.ECONNREFUSED)` with a substring fallback for stripped-errno cases.
- **`bridge update` post-install restart prompt** (PR #82). After a successful install, if a service unit is installed AND stdin is a TTY, prompts `"Restart the service now to apply? [Y/n]"` — Enter or `'y'` triggers `packaging.Restart()`. `--yes` / `-y` skips the prompt and restarts immediately. Non-interactive without `--yes` (CI) keeps pre-PR behaviour: prints manual hint, exits. Manual hint extracted to `printManualRestartHint` for reuse on auto-restart failure. `-y` short alias for `--yes` is wired on `init`, `update`, `cert rotate`, `restore` — uses `flag.BoolVar` pointing at the same destination so both forms write to the same target.
- **`bridge doctor --json --fix` must NOT interleave human + JSON on stdout** (PR #82). When both flags are set, `runFixes` writes its `✓`/`✗` progress lines to stderr; the JSON envelope still lands on stdout cleanly. Pre-fix, scripts piping `bridge doctor --json --fix | jq` got invalid JSON. Auto-fix `mkdir` mode is `0o700` to match `init.go`'s hardening. `--fix` ONLY does mkdir-class remediations; destructive or security-relevant fixes (port reassignment, service re-install, cert rotation) stay operator-in-the-loop.
- **fsnotify watcher is OFF by default** (PR #83). Config knob `LibraryWatch.Enabled` (with `EffectiveDebounceSeconds` defaulting to 10s). The periodic full scan (default 6h via `ScanIntervalSec`) remains the safety net regardless — the watcher only shortens time-to-visibility for newly-dropped files; missed events are reconciled on the next periodic tick. **`Scanner.ScanSubtree` runs a BOUNDED deletion pass** — only rows that were under `relScope` to begin with are candidates, so a cross-root move cleans the source-side row from one root's scan while the destination-side row arrives via the other's. (This entry previously claimed ScanSubtree "deliberately skips the deletion pass"; that was true when the watcher shipped and stopped being true when the bounded pass landed. Corrected 2026-07-28.) It shares the full scan's missing_count threshold model **and its UPnP-routed exclusion** — both passes call `Scanner.routedPathSet`; keep them in step, since a single-root watcher event at the library root yields `relScope "."` which short-circuits `TrackPathsUnder` to the whole library. **Path containment uses `filepath.Rel`**, NOT `strings.HasPrefix` — pre-fix the byte-exact prefix check was case-sensitive on macOS / Windows where the underlying filesystem isn't, so an event for `/Music/Album/track.flac` against a configured root that round-tripped through `filepath.Abs` as `/music` would false-negative. **Linux watch-limit handling is two-layer**: runtime — `isWatchLimitError` matches the canonical fsnotify failure messages and falls back to periodic-only with a single `Error`-level log on hit; pre-flight — `bridge doctor`'s `inotify-watch-limit` check reads `/proc/sys/fs/inotify/max_user_watches` on Linux and warns when configured roots' directory count exceeds 80% of the budget (no-op on macOS/Windows; only fires when `LibraryWatch.Enabled=true`).
- **CDS root exposes TWO sibling containers: All Tracks (id=1, flat list) + Folders (id=2, disk hierarchy)** (PR #317). Folders surfaces the on-disk Artist → Album → Track structure derived from `manifest.Track.Path` — lets users navigate by release rather than scrolling a 24k-row flat list. Matches what every reference MediaServer (MiniDLNA, MinimServer, Asset UPnP) presents. **`FolderObjectID(relPath)`** is the hash → numeric uint64 → decimal-string ObjectID for each folder (per the PR #315 mconnect/Cling int-parse invariant — non-numeric IDs cause silent rejection at any drill-down level). Reserved-range bump (`< 1000` → `+ 1000`) guarantees no collision with static IDs `0` / `1` / `2`. **`TrackInfo.RelativePath`** is the LOAD-BEARING source for folder hierarchy derivation — populated by the production adapter from `manifest.Track.Path` (which IS the relative path). LCP-based fallback derivation from `AbsolutePath` is preserved for test fixtures only; the LCP method silently strips a top-level folder when every track shares a single parent directory, which is why `RelativePath` is mandatory in production. **`BuildFolderIndex` is built LAZILY** per Browse call — All Tracks browse (hot path under flat library access) skips the O(N) walk entirely; only `Browse("0")` / `Browse("2")` / `Browse(hashedFolderID)` / `BrowseMetadata` for folder ObjectIDs pay the build cost (Gemini HIGH on PR #317). **`browseFolderChildren` emits folders FIRST, then tracks** per reference MediaServer convention; pagination cuts at the requested boundary regardless of which side it lands on, with uint64-clamp 32-bit-overflow defence (same shape as the All Tracks case). **`!ok` continue paths inside browseFolderChildren are defensive-only** — `childFolderIDs` / `childTrackIDs` come from the same `FolderIndex` instance, so a missing entry indicates a contract violation in `BuildFolderIndex`; silent skip keeps the wire response well-formed under any such bug (CodeRabbit on PR #317). **Don't drop the Folders container or replace All Tracks** — both serve distinct workflows (known-song-name access vs by-release browsing); removing either harms one of them. **Don't reintroduce LCP-derived relative paths in production** — the `RelativePath` plumb-through is the structurally correct shape. **Don't add a non-numeric prefix to `FolderObjectID`** (e.g. `"f-" + hex`) — would re-open the mconnect/Cling int-parse regression PR #315 closes. **Don't pre-build the folder index unconditionally at the top of `handleBrowse`** — would re-open the O(N) cost on every All Tracks browse. Locked by 10 cases in `folder_index_test.go` (ID stability + numeric-only output + nested/loose-file shapes + ordering + TrackCount roundtrip) + 5 integration cases in `content_directory_test.go` (root dual-container shape with `strings.Count == 2` hardening per CodeRabbit / Folders drill-down / sub-folder drill-down / track-item emission / unknown-ID NoSuchObject / BrowseMetadata for folders).
- **The fs scanner's missing-tracks pass SPARES UPnP-routed rows — two layers, both load-bearing** (PR #370, caught live minutes after deploying #369). Routed rows live in `tracks` but never appear in a disk walk, so the scanner counted all 15,283 of them "missing" on EVERY scan (hourly + startup); only #369's removal of the accidental per-walk `missing_count = 0` reset surfaced it, but the wipe already fired pre-#369 whenever the upstream was offline for `threshold` (3) consecutive scans — the fs-scanner's global `DELETE FROM tracks WHERE missing_count >= ?` bypassed the ingest's own don't-reap-on-failed-walk guard. Layer 1: the scanner's missing loop excludes paths in `upnp_track_routing` (`Store.UPnPRoutedSourcePaths`, read-only; fetch failure degrades to empty set). Layer 2: `IncrementMissingTracksAndDeleteAtThreshold`'s threshold DELETE carries `AND path NOT IN (SELECT source_path FROM upnp_track_routing)` so NO caller can threshold-delete a routed row regardless of pre-fix accumulated counters (heals stale state without a migration). Routed-row lifecycle belongs EXCLUSIVELY to the ingest's `last_seen_at` reconcile. **Don't drop either layer**, **don't "simplify" the DELETE back to the bare threshold form**, **don't add routed paths back into the scanner's missing accounting** (e.g. via a future beforeSet refactor that re-derives from `TrackPaths`). Locked by `TestScanner_MissingPass_SparesUPnPRoutedRows` (real temp-dir scans, threshold+1 passes) + `TestIncrementMissingTracks_ThresholdDeleteSparesRoutedRows` (at-threshold routed row survives while a filesystem row is reaped).
- **`/v1/artwork` + `/v1/artist-image` cache-miss is a THREE-way split — 202 `pending` ONLY while enrichment is genuinely pending; known-but-imageless answers terminal 404 `no_image`** (PR-pending, 2026-08-06; Mirror-PR pair with iOS's sweep single-shot change). Pre-fix the miss branch answered 202 for ANY known MBID with no cache file, with no way to say "enrichment ran; no image exists upstream" — so the 78 artists on bridge.ars.md whose portraits Deezer simply doesn't have answered `202 pending` forever, and iOS's resumable coverage sweep paid a ~4–5 min retry ladder per artist on EVERY sync (field-diagnosed from journald: 224 futile 202s across one evening). `MBIDProbe` gained `Artwork/ArtistMBIDEnrichmentPending` (store: `EXISTS(… MBID = ? AND enriched_at = 0)`, riding the same functional MBID indexes; **fail-OPEN to pending on a DB fault** — a wrong "pending" costs one bounded retry, a wrong "complete" would terminal-404 an image about to land). The discriminator is sound because `MarkEnriched` stamps `enriched_at` on success AND on skip, while the enricher's `IsTransient` guard deliberately leaves it 0 on transient upstream failures (PROTOCOL.md line ~282) — so "no track pending + no file" = every turn the enricher will ever take was taken. Forced re-enrichment (`enriched_at = 0`) flips the answer back to 202 by construction. **No `ProtocolVersion` bump** — iOS has treated 404 on these endpoints as terminal-nil since v1.0. **Don't collapse the split back to two states**, **don't put `Retry-After` on the `no_image` response** (clients must not retry it), **don't flip the DB-fault direction to fail-closed**. Locked by `TestArtwork/ArtistImageReturns404NoImageWhenEnrichmentComplete` (+ updated 202 fixtures now requiring pending=true) and `TestStoreMBIDEnrichmentPending` (upsert→pending, one-of-two-enriched→still-pending, all-enriched→complete).
- **UPnP upstream ingest is skip-if-unchanged — unchanged walked tracks keep `indexed_at` AND `enriched_at`** (PR #369, 2026-06-10). Pre-fix every walk re-upserted all routed tracks unconditionally; `UpsertTrack` always advances `indexed_at` and resets `enriched_at = 0`, so on a 15k-track upstream (Chord 2Go) every walk (a) wiped + re-queued the WHOLE upstream for enrichment (perpetual MB/CAA/Deezer treadmill that also discarded previously-fetched MBIDs from `tags_json`), and (b) made every iOS delta sync re-receive all 15k tracks (`/v1/manifest?since=` gates on `indexed_at`) — the client-side @Query churn behind the iOS freeze report (iOS PR #789 is the diff-before-write twin). `ingestOne` now loads `Store.ListUPnPTracksByServer` (routing-join → `tags_json` decode, read-only no `s.mu`) once per walk and skips the Track upsert when `walkFieldsEqual(existing, fresh)`. **Two `walkFieldsEqual` exclusions are load-bearing**: `ModTime` (buildTrackAndRouting stamps walkStart — including it defeats the skip entirely) and the enricher-owned fields (`MusicBrainz*` / `ArtworkMBID` / `ArtistMBID` — a fresh walk row never carries them; including them marks every ENRICHED row as changed forever → the exact wipe loop this fix stops). Genre / DiscNumber / ReplayGain* excluded in the other direction (walker never sets them). **The ROUTING row is still upserted every walk** — `last_seen_at` drives the reconcile sweep, and `res_url`/`object_id` float across upstream restarts (DHCP) without a content change; `flush()` handles routing-only batches (parent track rows already exist → FK satisfied) and triggers on the routing slice length (grows on every item, so it alone bounds the batch). **Baseline load failure degrades to nil** (= legacy rewrite-everything; upserts are ON CONFLICT keyed so correctness is unaffected) rather than failing the server. `ServerIngestResult.Unchanged` counts skips (surfaced in the wiring's walked/unchanged/reaped log line). **Don't add enricher fields or ModTime to `walkFieldsEqual`**, **don't skip the routing upsert for unchanged tracks**, **don't make the baseline load failure fatal**. Locked by the `TestWalkFieldsEqual_*` truth table + `TestIngester_Run_SecondWalkSkipsUnchangedTracks` (empty since-delta, enrichment survives, routing refreshed, no reap) + `TestIngester_Run_ChangedTrackStillUpserts`.
- **ContentDirectory:1 SCPD MUST advertise the 3 spec-mandatory introspection actions — `GetSearchCapabilities`, `GetSortCapabilities`, `GetSystemUpdateID`** (PR #316). Pre-fix the SCPD declared only `Browse` on the (genuinely correct) reasoning "renderers tolerate optional-action absence gracefully" — that's true for `Search` / `CreateObject` / `DestroyObject` but FALSE for the 3 introspection actions which are MANDATORY per UPnP CDS:1 §2.3 regardless of whether Search is implemented. User-visible symptom: mconnect Player (and likely other strict UPnP control points) renders the root container (`Browse(0)` → "All Tracks [121]") but tap-to-drill silently does nothing. **Mechanism**: mconnect polls `GetSystemUpdateID` between every navigation step to verify directory freshness; with the action missing it returns SOAPFault 401 (InvalidAction); mconnect interprets that as "directory in inconsistent state, abandon drill" — never dispatches the downstream `Browse(child)`. Empirically validated 2026-05-28 against mconnect via a minimal Go-based UPnP server at `/tmp/upnp-test` (200 LOC, mirrors our SCPD shape-for-shape): with only `Browse` declared, mconnect repeated `Browse(0)` on every "All Tracks" tap; after adding the 3 actions and restarting, the very next refresh showed `GetSystemUpdateID → Browse("1") DirectChildren → render`. **Fix shape**: 3 new no-input action handlers in `ContentDirectoryHandler`'s actionName switch returning canonical stable values (`<SearchCaps></SearchCaps>` / `<SortCaps></SortCaps>` / `<Id>1</Id>`). Empty SortCaps declares "no sortable fields" (matches our actual capability — Browse honours `SortCriteria=""` only). **The SearchCaps half of this entry is STALE and was corrected 2026-09-01: `Search` IS implemented.** It returned empty SearchCaps ("Search not supported") when #316 shipped, but `handleSearch` exists and `searchCapsFields` now advertises `dc:title,upnp:artist,upnp:album` — the fields mirrored into the FTS5 `tracks_fts` table, which is the signal that flips the Search action on in BubbleUPnP and Linn Kazoo. Don't re-derive "the bridge has no DLNA Search" from this entry; check `internal/dlna/content_directory.go` before believing any doc about it. (This was the third stale claim of the kind, after the WAV/AIFF extractor gap and the `deletedIds` field name — see the stale-claims note at the top of CLAUDE.md's "Things that have bitten before".) **Stable `SystemUpdateID=1`** is spec-allowed for best-effort CDS impls without eventing infrastructure — the downside is controllers can't detect manifest changes via CDS poll alone, but the iOS client uses SSE for that signal so there's no functional regression. **Don't drop any of the 3 introspection actions** at any future refactor — would re-open the mconnect-silent-drill-abort regression. The pre-PR-#316 comment in `device_description.go` claimed "renderers tolerate optional-action absence gracefully" — corrected to spell out the introspection-actions-are-mandatory distinction so a future "minimize the SCPD" refactor doesn't repeat the bug. Locked by 4 cases in `internal/dlna/content_directory_test.go`: per-handler wire shape (`Test_CDS_GetSearchCapabilities_ReturnsEmptySearchCaps` / `_GetSortCapabilities_ReturnsEmptySortCaps` / `_GetSystemUpdateID_ReturnsStableID`) + SCPD declaration invariant (`Test_CDS_SCPD_AdvertisesSpecMandatoryActions`).
- **Chord 2Go file-fetch identifies as generic MPD — `chordFamily` matchers don't fire on file-stream requests** (latent invariant, no code change shipped). The 2Go's playback worker sends `User-Agent: "Music Player Daemon 0.21.26"` for the actual `/dlna/file/{id}` GET (the SSDP description / GetProtocolInfo / SetAVTransportURI dispatches come over different transports — DLNA SOAP control vs raw HTTP fetch). `internal/dlna/renderer_profile.go::MatchProfile` walks `Profiles` in declared order; `profileChordFamily()`'s matchers (`["Chord", "2go", "Poly"]`) don't fire on the MPD UA, so file-stream requests from real 2Go hardware fall through to `profileMPDGeneric()`. The chordFamily docblock acknowledges this explicitly. **Today this is moot** because (a) both profiles' `PreferredMIME` maps are identical (`.dsf → audio/x-dsf`, `.dff → audio/x-dff`) and (b) neither `KnownBugs` nor `MaxSafeFileSize` is enforced anywhere in the bridge's file-fetch path. **It becomes a real silent-failure if any future PR adds enforcement** — `MaxSafeFileSize` blocks for the 2Go's `BugID3OffsetOverflowOver2GB` 2 GiB cap would NOT fire because real 2Go traffic lands on `mpdGeneric` (no cap). **Don't reintroduce a `"Music Player Daemon 0.21"` UA matcher to `profileChordFamily` as a prophylactic fix** — version-anchored substring matchers rot the moment Chord ships a firmware update with a different MPD version (the 0.21 → silent regression hazard). When adding enforcement, the structurally correct shape is either (a) explicit fall-through dispatch (e.g. `applyChordCapsTo(profile)` when `(host + UA) signal points at Chord hardware`) keyed off SSDP-derived metadata cached against the source IP, OR (b) per-call enforcement parameterized on `Profile` IDs the file handler resolves explicitly. Per Gemini cross-codebase audit 2026-05-28.
- **Docker image runs as non-root `bridge` user** (PR #84). `mkdir -p /data && chown bridge:bridge /data` happens in the same `RUN` layer that creates the user, BEFORE `USER bridge` switches contexts. Without the explicit chown, `WORKDIR` / `VOLUME` create `/data` with `root:root` ownership and the runtime fails on first-run TLS-cert mint or manifest-DB create. Operators bind-mounting their own pre-owned volume override the in-image baseline naturally. **`VERSION` build-arg feeds `-ldflags`**: `-X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=${VERSION}` so `bridge version` reports the build identity; default arg is `"docker"` for unpinned builds. **Env-var overrides** (`BRIDGE_LISTEN_ADDRESS`, `BRIDGE_ADMIN_ADDRESS`, `BRIDGE_DATA_DIR`, `BRIDGE_LIBRARY_NAME`, `BRIDGE_LIBRARY_ROOTS`) are applied in `config.applyEnvOverrides` BETWEEN `applyDefaults` and `resolvePaths`, so relative paths from env inherit the same "relative-to-config-dir" semantics as YAML fields. `BRIDGE_LIBRARY_ROOTS` is colon-separated regardless of host OS — accepted universally because the only realistic container deployments are linux/amd64 + linux/arm64. Empty / unset env = no change. Documented precedence: env > yaml > defaults.
- **Audio-analysis decode commits ONLY on a length-complete decode — gated by `decodedShortOfDuration` (probed duration), NOT exit code / `-xerror` / stderr matching** ([internal/analyze/decode.go](internal/analyze/decode.go), PRs #448 ffmpeg + #449 sox, found by the v0.1.7 pre-release review). BOTH decoders exit 0 on a truncated-but-openable source: ffmpeg conceals a mid-stream error and exits 0; sox opens a truncated FLAC via its intact front STREAMINFO, decodes ~half, prints `sox FAIL ... LOST_SYNC`, and exits 0. Pre-fix `decodeFrames` treated a clean exit as a clean decode and committed a PARTIAL waveform (wrong duration + biased ReplayGain/key/tempo) keyed to the file's mtime+size, so the scan-skip gate never re-analyzed it — a permanently-wrong sidecar (field case: a partially-uploaded rclone-to-B2 faststart m4a, non-zero-byte so it passes the #446 zero-byte skip). Fix: probe the expected duration once at `probeChannels` time (`sox --i -D` on the sox path, ffprobe `format=duration` on the ffmpeg path) and reject when the decoded length is < 90% of it (`minDecodedFraction`); nothing is committed so the candidate re-flows until fully re-uploaded. Unknown duration (probe miss → 0) skips the check (commit as before). **Two alternatives REJECTED, both empirically disproven — DON'T reintroduce:** (a) ffmpeg `-xerror` aborts on ANY decode error → also fails a glitchy-but-COMPLETE file (one concealed bad frame) → no sidecar + a permanent treadmill (a failed analysis writes NO row, so the candidate re-flows every sweep). (b) sox stderr-marker matching (`sox FAIL`/`LOST_SYNC`) CANNOT distinguish truncation from a glitchy-but-complete file — a 200-byte mid-stream flip on a 10s FLAC decodes to FULL length (sox resyncs to EOF) yet prints the SAME `sox FAIL ... LOST_SYNC`; the decoded-LENGTH check distinguishes them (truncated = short → reject; resynced-complete = full → commit). The `sox WARN ... MD5 checksum mismatch` line is ALSO a trap — it's a WARN (not FAIL) that fires on a valid tag-edited FLAC, so matching it would treadmill valid files. **`sox --i -D` is safe for VBR MP3** — verified it matches the decoded length within <0.1% both WITH and WITHOUT the Xing/Info tag, so it doesn't false-reject complete VBR MP3s. **All exec sites resolve via `resolveBin(soxLookPath,"sox")` / `resolveBin(ffprobeLookPath,"ffprobe")`** — absolute-path / `filepath.IsAbs` defense-in-depth, which ALSO keeps new code clear of SonarCloud `go:S4036` (it fires on bare-name execs in new code; the pre-existing bare-name execs across the repo are grandfathered, but new ones must use `resolveBin`). Locked by `TestDecodedShortOfDuration` + `TestRunAnalysisTruncatedFLACWritesNoSidecar` + `TestRunAnalysisGlitchyCompleteFLACCommits`.
- **The orphan-sidecar sweeper's chunk-resume cursor compares in `filepath.WalkDir` traversal order, NOT raw string order** ([internal/integrity/sidecars.go](internal/integrity/sidecars.go), PR #466, Gemini-consulted ×2). `WalkDir` orders each directory's entries by BASE name and visits a directory before its children, so it walks all of `A/` before sibling `A-Bonus/` (base names `"A"` < `"A-Bonus"`) — but the separator (`/`=0x2F, `\`=0x5C) sorts AFTER `-`(0x2D), ` `(0x20), `.`(0x2E), `&`, `'`, so `A-Bonus/…` **<** `A/…` as a RAW string. Pre-fix `dirEntirelyBehindCursor` (`withSep < cursor`) + the file-skip (`path <= cursor`) therefore `SkipDir`'d the still-unwalked `A-Bonus` subtree on a resume tick, permanently missing its orphan `.flac` sidecars (opt-in sweeper, >`gcChunkSize`=5000-sidecar libraries only, disk-space-only, recoverable via the non-chunked `bridge upscale --gc`). The variant tree is nested + source-path-mirrored (`SidecarPath()`: `<OutputDir>/Artist/Album/Track.flac.<variantID>.flac`) with UNSANITIZED directory segments, so real music dir names (spaces, `-`, `&`, `'`) hit this constantly. **The PRIOR (PR #282-era, also Gemini-blessed) predicate encoded the SAME misconception** — its test asserted the `A/B` dir must be descended when the cursor is the sibling FILE `A/B.flac`, on the false belief that `A/B/02.flac` sorts AFTER `A/B.flac` (WalkDir visits it BEFORE; the dir subtree is fully swept before the sibling file → safe to prune). Fix: `pathWalkCompare(a,b)` compares segment-by-segment (a shorter ancestor path orders before a longer one that extends it), **zero-alloc via an index scan (NOT `strings.Split`** — it runs per-entry on the walk hot path, so a 50k–100k-sidecar library would pay 2 heap allocs/entry). `dirEntirelyBehindCursor` = ancestor-guard (`cursor==trimmed || HasPrefix(cursor, trimmed+sep)`, which also covers the trailing-slash / volume root) + `pathWalkCompare(trimmed, cursor) < 0`. **Don't reintroduce a raw-string `<=`/`<` path compare in any WalkDir-resume cursor; don't reintroduce `strings.Split` in `pathWalkCompare`.** Locked by `TestPathWalkCompare_MatchesActualWalkDirOrder` (records the REAL WalkDir order + asserts the helper reproduces it), `TestPathWalkCompare_ZeroAlloc`, `TestOrphanSidecarSweeper_SiblingDashDir_NotPruned` (fails on the raw-string predicate), + the corrected `TestDirEntirelyBehindCursor` table.

---

## ALAC upscaling — the ffmpeg fallback, and three things only measurement settled (issue #127, 2026-09-02)

Closes the bridge half of issue #127. `POST /v1/upscale` has always routed
everything through sox, and no stock sox build carries an MP4 demuxer — so
ALAC, the one LOSSLESS format that clears `manifest.IsLossyCodec`,
`canSetBitsPerSample` and `OptimizeEligible`, reached the decoder and could
not be read. PR #440 made that reachable by populating MP4 geometry; #572
restored an honest refusal. This makes it work.

**Shape.** `ffmpeg -map 0:a:0 -f f32le -` piped into `sox -t raw -e float
-b 32 -L -r <rate> -c <ch> -`, with the rest of the chain untouched.
`SoxArgs` delegates to `soxArgsFrom(input, decoder)` so the two routes share
ONE definition of gain-guard / target bits / FLAC output / rate / dither —
a piped sidecar is the same transform on the same samples.
`CanDecodeVia` consults the fallback, so all four eligibility call sites
gained coverage at the seam that already centralises their fail-open policy.

**Three measurements, each of which changed the design:**

1. *ffmpeg exits 0 on a truncated source.* A half-truncated faststart `.m4a`
   still reports `duration=8.000000` (the moov is intact) and the pipe exits 0
   while producing **3.901s**. The sidecar is keyed on source mtime+size, so
   nothing would ever regenerate it. Hence the duration guard.
2. *A complete decode is exactly 1.000000.* Five shapes — 44.1/48/96/192 kHz,
   mono and stereo, non-round durations — every one exactly 1.000000. This path
   resamples, it does not re-time. So a 2% tolerance is generous, much tighter
   than `internal/analyze`'s 0.90 (correct there, where the decode targets a
   fixed 48 kHz).
3. *A too-low input rate makes the output LONGER.* 44.1 kHz described as 22050
   produced exactly 2.0x the duration, which a lower-bound-only guard **accepts**
   while committing a half-speed variant. The guard is two-sided; the negative
   control (lower bound only) turns exactly the two upper-bound cases red.

**Raw, not `-f wav`.** WAV is self-describing and would remove the dependence
on probed rate/channels — but ffmpeg cannot seek back to patch a header on a
pipe, so it writes RIFF size `0xFFFFFFFF` and sox prints
`WARN wav: Premature EOF on .wav input file` on EVERY successful job. Noise,
and worse, a warning indistinguishable from the real truncation. Raw also has
no 4 GiB ceiling. (The rate-mismatch risk raw reintroduces is what the
two-sided guard covers.)

**The allowlist is closed.** `ffmpegRoutableExt` is the MP4 family only, not
"anything sox refused". Lossy and DSD are excluded upstream, so anything else
reaching a refusal is a shape neither decoder was chosen for; routing it would
convert an honest "sox can't read this" into a mysterious mid-job failure.
Note `CanDecode`'s OTHER documented fail-open still applies: an extension
absent from `soxFormatsForExt` (e.g. `.wma`) reaches sox and gets sox's own
diagnostic — a test asserted `routeNone` for that and was wrong, not the code.

**`RunSox` now returns the settings it used** (`(int64, string, error)`).
The persist sites rebuilt them with `SoxArgs()`, which hardcodes the decoder
and cannot know the route — so `"decoder"` would have named sox for every
piped sidecar. Same lesson as `SoxArgs` handing back its temp path instead of
letting `RunSox` re-derive a drifting one, one level up. ~29 test stubs
followed, compiler-enforced.

**Doctor.** `checkAudioToolchain` warns when upscale is on, the library holds
ALAC (`Store.HasTracksWithCodec`, existence-only, counts suppressed and routed
rows because the question is what the operator HAS), and ffmpeg is missing.
It stays silent when there is no probe wired or the probe errors — a fresh
install with no scan must not be told to install ffmpeg for a library it has
not read. `probeSox` / `ffmpegAvailable` became seams because CI runners have
neither binary, so the sox probe would fail first and the new branch would
never be reached.

**Two things review and self-review changed.** (a) `FFmpegAvailable` is false
when EITHER binary is absent, but the doctor warning blamed ffmpeg — a host
with ffmpeg and no ffprobe (some distros package them apart) was told to
install what it already had. `MissingFFmpegBinaries` names which.
(b) Self-caught by re-reading `RunSox`'s own docblock, which says it does not
probe "so a worker-pool body doesn't pay the LookPath cost per iteration": the
route decision probed unconditionally. Measured 7.9 ms per `ProbeSox` fork+exec
against 17 µs for `FFmpegAvailable`, i.e. 1,338 needless spawns on the current
auto-optimize backlog. Gated on the extension — negative-controlled by counting
real `soxLookPath` calls during one FLAC job: **1 unconditional, 0 gated**.

**Two review findings declined**, both recorded on the PR thread. The
`os.Stat` before `OpenStore` is NOT redundant: `OpenStore` on a missing path
*succeeds and creates the file* (verified), so without it a `bridge doctor`
run on a never-scanned host leaves an empty `bridge.db` and then answers "no
ALAC" from a database it just made — the rationale is now a comment, since the
check does look redundant. And "move the CLAUDE.md change to direct-to-main"
misreads that convention: it permits docs-ONLY changes to skip the branch, it
does not forbid shipping an invariant with the code that motivates it —
splitting them is how a rule ends up describing code that never landed.

**#127 is CLOSED, and its last two acceptance criteria were already moot when
this shipped.** They read "replace iOS's hard-coded
`upscaleSoxUnsupportedExtensions` with a bridge-reported `supportedDecoders`
flag" and "update the `UpscaleIneligibilityReason.detail` alert text" — but
**iOS PR #809 (2026-06-11) deleted the entire iOS-side upscale-REQUEST path**,
including both symbols. Upscaling is operator-driven on the bridge; iOS only
*consumes* variants, so it never asks "could the bridge decode this source?"
and a `supportedDecoders` field would have no consumer. (`operatorDrivenUpscale`
survives on iOS solely to HIDE stale upscale UI.)

The issue's scope note was revised **2026-07-22, six weeks AFTER #809**, and
still listed both as pending — and I relayed that checklist twice in one
session before checking the iOS source. Same class as the four stale claims
CLAUDE.md records, arriving from an issue tracker rather than a doc: **an
acceptance criterion naming a symbol is a claim that the symbol exists.**
Grep the other repo before quoting one.

## LOUPE on the lyrics surface (PRs #849 / #850 / #851, 2026-09-06)

The lyrics surface landed whole in ONE PR (#840) four days earlier and had no
`### ` section under CLAUDE.md's `## Things that have bitten before` — nothing
bound the code to any invariant. It was also the only untrusted-input parser
family in the repo with **zero fuzz targets** (34 in the tree, none in
`internal/lyrics`), against a stated policy that names the audio extractors as
one of the three surfaces that must be fuzzed. Both absences are why this ran.

**Prior, stated up front:** an unswept surface should refute far less than the
~70 % false-positive baseline hardened ground produces. It did — 7 of 8 external
findings and all 5 of my own held up.

**Two directions:** a read of every production file with CLAUDE.md in hand, and
an independent `consult.py` pass (gemini-3.8-flash, Go-framed, all four
production files attached, my three findings disclosed up front so they could
not be re-reported). `finish=STOP in=12231 think=62912 out=2272`.

### The delta-loss regression (#849)

`writeLyricsRowTx.bump()` shipped

```sql
SET indexed_at = CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END
```

which is quoted **verbatim** in `indexedAtAdvanceSQL`'s own docblock as the
older form that "advances strictly relative to the row's OWN prior value only;
its ELSE arm assigns the raw clock, so when the clock equals a value another row
already holds the bumped row lands EXACTLY ON a cursor equal to that value and
`indexed_at > since` excludes it." The docblock even records that this is why
`TestRestampDuplicates_PolicyFlipUnsuppressesViaDelta` failed only on the
windows-latest CI leg.

`StampExtractorVersionBatch` computes `now` ONCE (store.go:2724) and loops, so
every lyrics-changed row in a batch gets the identical raw clock while the
interleaved `UpsertTrackBatch` leg pushes the library max ahead via `MAX+1`.
That is the v7 lyrics backfill's own path, and home-pc is a Windows bridge.

**Why the existing guard missed it.** `TestIndexedAtAdvanceIsShared` walks a map
of six named CONSTS. This bump was an inline string literal inside a function
body, so it was never a candidate — the guard was well built for the shape it
was written against and blind to the shape a new writer actually takes.
`TestNoHandRolledIndexedAtBump` now sweeps every non-test `.go` in the package.

**Two review corrections to that guard, both taken:**

- Gemini: sweep the whole package, not `store.go`. Verified before accepting —
  `setBookletTagSQL` is in `booklets.go:114` and `applyDupeStampBumpSQL` in
  `dupe_stamps.go:65`, genuinely outside the swept file. Control: a `CASE WHEN`
  bump planted in `dupe_stamps.go` reddens the sweep.
- CodeRabbit: classify against the containing SQL literal, not a 320-byte
  lookahead, or a neighbour's `excluded.indexed_at` can vouch for an unapproved
  bump. **Measured before agreeing: the nearest such marker is 581 bytes from a
  plausible plant point, outside the old window, so the hole was structural
  rather than reachable at any current site.** Taken anyway — #840's own bump
  sat ~25 lines above that marker. Said so on the thread rather than
  overclaiming.
- CodeRabbit also caught a literal **U+201D** in a test comment where `''` was
  meant. It was real, and it came from the python apply script — the trap this
  repo already records ("a python heredoc writing Go can eat an escape level;
  watch for full-width lookalikes"). Every added line on all three branches was
  re-scanned; that was the only one.

The second defect in the same function: the nil branch gated its early return on
`oldTag == ""`, conflating "no row" with "a row that was never client-visible".
A `sidecar-rejected` row carries `tag = ''` by construction, so deleting the
sidecar left the row behind forever and `sidecarLyricsDrifted` then re-extracted
the audio file on every scan for the life of the library.

### Non-convergent skip gates (#850)

`sidecarLyricsDrifted`'s embedded-source arm asked *"could a sidecar with this
EXTENSION outrank the stored source?"* — a stateless test whose answer never
changes. Two permanent loops: an empty / oversized / legacy-encoded `.lrc`
yields no document but ranks 1 against an embedded `text` row's 5, and a
**tagless** `.lrc` is demoted to `sidecar-txt` (rank 6) and loses while still
ranking 1 by extension. Both re-open and re-parse the audio file on every scan.

**Invisible by construction:** the re-extract lands on `reExtractUnchanged` →
`versionStampOnly`, so there is no `indexed_at` churn, no client symptom, no log
line — only NAS I/O, forever. `sidecarLyricsFile` already made the extractor and
the gate agree about WHICH file; `readSidecarCandidate` now makes them agree
about WHAT IT IS WORTH.

**Rejected alternative:** adding `sidecar_mtime_ns` / `sidecar_size` columns
(migration v43) would also be exact, but it is a schema change for a case the
shared-helper form settles, and it would leave two implementations of "what does
this sidecar resolve to" — which is the class of divergence that caused the bug.

**Rejected finding (external, MEDIUM): "`sidecarLyricsDrifted` lacks an mtime
tolerance".** Its premise is wrong. `analysisSourceMTimeToleranceNS` is an
API-side concept for the 410 check; the scanner's own primary skip gate compares
the AUDIO file byte-exactly (`existing.MTimeNS == pi.info.ModTime().UnixNano()`).
If nanosecond jitter re-extracted files on this deployment the whole library
would already be re-extracting. Loosening only the sidecar half would be the
inconsistency, not the fix.

### The pick's total order, and what fuzzing found (#851)

`Pick`'s comparator ended at `(rank, priority, body length)` over a slice built
partly by `range m.Raw()`. Any pair it left equal was decided by Go's randomised
map order → the winner flips between scans → `lyricsTag` re-keys → `indexed_at`
bumps → the track re-enters every device's delta on every scan.

`m.Lyrics()`'s `Priority: 0` is **fabricated**: dhowden's
`metadataID3v2.Lyrics()` returns `m.frames["USLT"].(*Comm).Text`, the same frame
the raw walk re-reports with its real `DescriptorPriority`. Keeping the first
sighting let an "Amazon" descriptor launder itself back to the best rank, so
`junkExact` / `junkSubstring` did nothing whenever the frame's language was
absent or unmapped.

`lrcTime` could render `[1000:00.000]` from a raw uint32 past 999 minutes —
four minute digits, matching neither `lineTag` nor `hoursTag` — inside a
document `syltCandidate` stamps `synced: true` regardless. **Clamped, not
promoted to `[hh:mm:ss.xxx]`:** the hours form (the external suggestion) would
rewrite the rendering of every legitimately >1 h track, a delta wave for content
that works today, and `renderLine` uses the same helper for the enhanced
`<mm:ss.xxx>` WORD tags whose iOS grammar is not mirrored in this repo — an
unverifiable mirror risk for zero real-world gain.

**The three fuzz targets found two more defects within a minute of existing**,
and the second was in the fix for the first finding above:

1. `Normalize` was not idempotent, which matters because `resolveLyrics`
   normalises an already-normalised body — so the two passes disagreeing means a
   document accepted as a candidate is silently dropped at resolve time.
   `"\n﻿"` → `"﻿"` → `""`: the BOM is not at index 0 on the first
   pass, and Go's `unicode.IsSpace` does **not** count U+FEFF, so `TrimSpace`
   keeps it. Fixed with a plain `ReplaceAll`, whereupon the target found
   `"\xef\xbb" + BOM + "\xbf"` — **deleting a U+FEFF splices its neighbours into
   a new one.** Now stripped to a fixed point.
2. `mergeDuplicate` was order-dependent, in the code written for the
   junk-descriptor fix. It chose the surviving base with `lessCandidate`, which
   reads `Priority`, which the merge *raises*: the accumulator's own mutation
   fed back into the next comparison, and three sightings of one document folded
   to different answers under different arrival orders. Base selection now uses
   only fields the merge never mutates (synced, then format); `Priority` takes
   the max and `Language` the smallest non-empty — order-independent
   aggregations, which is what a fold over map-ordered input requires.

Post-fix fuzzing, `-fuzztime 150s -fuzzminimizetime 1s` each: **141.6M
executions** (37.1M / 62.1M / 42.3M), zero crashers. Both crashers are committed
as regression corpus under `internal/lyrics/testdata/fuzz/`.

### Process failures in this run, recorded because they are cheap to repeat

- **A negative control that proved nothing, twice.** The tie-break control
  passed because no fuzz seed carried an actual tie (same rank, same priority,
  same body *length*, different bodies) — every seed differed earlier in the
  comparator and never reached the tail. And the max-priority control failed to
  **BUILD** (`declared and not used: dup`), which a grep for `^--- FAIL` reads
  as a pass. Both are already named in the LOUPE doc; both happened anyway.
  Grep for `build failed` as well as `FAIL`.
- **A control's restore reverted an uncommitted fix.** `mergeDuplicate`'s
  canonicalisation was written, verified, and then destroyed by the next
  control's `git checkout --`. Commit before controlling — also already in the
  doc.
- **CodeRabbit hit its rolling budget mid-run.** #849 got both bots; #850 got
  Gemini only ("no feedback to provide"); #851 opened after the budget was
  spent. Say which PRs went unreviewed rather than implying coverage.

### Atlas cannot supply lyrics — measured, not assumed

Asked alongside the review: could the bridge get lyrics from Atlas? The plumbing
is better than expected and the answer is still no.

Atlas has exactly one lyrics column (`atlas_qobuz_track.lyrics`, migration
`0010_qobuz_enrichment.sql`), one extractor (`internal/ingest/qobuz/
extractors.go` `extractLyrics`), a converger arm, and a live endpoint
(`GET /v1/atlas/recording/{mbid}`, `AtlasRecordingResponse.Lyrics`). An
integration test asserts the read path — by seeding the table with SQL.

**Measured against the live instance, 2026-09-06:**

```
 with_lyrics | total  | raw_has_key
-------------+--------+-------------
           0 | 679296 |           0
```

Zero rows carry lyrics, and zero retained `raw_payload`s even carry the key — so
there is nothing to back-fill. The bulk ingest path is `album/get`'s nested
`tracks.items[]`, which does not return lyrics; only `handleTrack`'s `track/get`
would, and `disable_refresh: true` has been set on the live qobuz block since
2026-07-31. Filling it means ~679k `track/get` calls against a 240/min bucket.

Four further blockers, in order of hardness:

1. **Scope.** The bridge holds only a `bulk_harvest` bearer
   (`internal/atlasharvest/state.go`); `/v1/atlas/recording/{mbid}` requires
   `read:bridge`. The booklet endpoint is the precedent for accepting both.
2. **`track_lyrics` is file-shaped** — `source_path` PK with `source_mtime_ns` /
   `source_size` staleness columns that mean nothing for a network document, and
   `writeLyricsRowTx` DELETEs the row whenever an extraction finds nothing, so a
   network-sourced row would survive exactly until the next scan.
3. **No attribution.** Atlas carries per-field `source` / `sourceUrl` for bios
   and descriptions — its own code calls that "for CC-BY-SA / ToS compliance" —
   and a separate auth-layer distinction for premium cover art. Lyrics have
   neither, and their single source is a credentialed commercial DSP relaying a
   third-party provider.
4. **Plain text only.** No `lrc` / `ttml` / timing anywhere in Atlas, so it could
   never drive the synced UI that SYLT and `.lrc` power — it would rank below
   every local source by construction.

**Do not re-open this without re-running the count query first.** If
`raw_has_key` is still 0, the work is a new ingest path, not a new endpoint.

## LOUPE on the `cmd/bridge` surface (PRs #852 / #853 / #854 / #855, 2026-09-06)

Surface-shaped run, not window-shaped: one Go package, 52 production files,
19,375 lines, `main.go` alone 4,698. Plan and full triage in
[`plan-2026-09-06-loupe-cmdbridge.md`](plan-2026-09-06-loupe-cmdbridge.md).

### Phase 0 predicted the yield, and was right

The bridge-side structural question is whether the surface has a `### ` section
under `## Things that have bitten before`. **It did not** — eleven sections, none
for the package that holds every wiring decision in the process. Prior-art agreed:
`cmd/bridge` appears 20 / 12 / 2 / 6 times across the four `ops/audit-*.md` files,
against a 19k-line surface, and `engineering-log.md` cited only 10 distinct files
in it.

The stated prediction was that an unswept surface should refute *fewer* findings
than the ~70% baseline for hardened ground. It produced **five confirmed defects
and four stale claims in CLAUDE.md itself**, three of the five reachable on the
**default** configuration.

### The dominant defect class: a construction guard that stopped guarding

PR #781 ("always construct, never stop") converted `if analysisActive {` and
`if upscaleActive {` into bare blocks so both pools are always built — correct,
and the thing that makes those flags hot — and converted every READER to a live
predicate in the same commit. The WRITE paths had nothing to convert, because
**the enclosing `if` was their gate**:

- `runAnalysisSweeper` took no `enabled` parameter at all. On the default config
  (`analysis.enabled` is false) the bridge walked the library 90 s after every
  boot, forked a decode per track, wrote waveform sidecars, and — because
  `UpsertAnalysis` ends in `bumpIndexedAtByPathSQL` — pushed a **whole-library
  delta to every paired device**, repeating on every scan interval and post-scan
  nudge. `/v1/analysis/*` 404'd throughout and `bridge analyze` refused, so the
  feature looked absent while costing everything.
- `upscaleRequest` and `upscaleDelete` gated on `s.upscaleEnqueuer == nil` /
  `s.variantDeleter == nil`, with comments still claiming nil meant "feature off
  OR sox precheck failed". `upscale.enabled` also defaults false, so any
  bearer-token holder could enqueue real sox jobs writing `track_variants` rows
  and FLAC sidecars — with **no disk floor**, which lives only in the sweeper and
  `Coordinator.Submit` — on a bridge whose `/v1/health` said `upscaleEnabled:
  false`.

`git show a25c8ba` is the whole diagnosis: `-	if analysisActive {` → `+	{`.

**Why no test caught the API half**: `upscaleFixture` wired the enqueuer WITHOUT
the feature predicate, which is precisely the production state that shipped. A
fixture that cannot express "wired but disabled" cannot catch a missing gate.

**The iOS side was checked rather than assumed** before restoring the gate:
`DownloadCoordinator.swift:1607` states the dispatch precondition as
`share.bridgeUpscaleEnabled == true` and `BridgePairingPersistence.swift:158`
sets it from `health.upscaleEnabled`, so the shipped client cannot be broken by
a 503 it never provokes.

### Six commands could not find the platform config

Measured, from an empty directory on a host with a normal install:

```
$ bridge token list
config load failed: read config "": open : no such file or directory   (exit 2)
$ bridge status                     # the loadCLIConfig control
status: probe 127.0.0.1:7789: ...   # resolves fine
```

The empty `""` is the whole story. Sixteen files route through `loadCLIConfig`;
`openTokenStoreFromCfg` and `bootstrapTranscodeCmd` still handed the flag's empty
default to `config.Load`, taking `bridge token {list,rotate,expire,revoke}`,
`bridge upscale` and `bridge optimize` with them — while all six advertised the
`./bridge.yaml`-then-platform fallback in their own flag help. `bridge token
revoke` is the documented orphaned-token recovery path, i.e. the one command an
operator reaches for when something is already wrong.

Both were one call in a shared tail, invisible from the subcommand owning the
flag, so the fix carries a package-wide guard against a four-entry allowlist.

### Two fail-closed guards

**`runArtworkGC`** had no empty-referenced-set check: every file misses an empty
`known`, so the whole cache is orphaned, and the scanner-written
`local-<sha256>-500.jpg` covers never come back (the mtime skip gate stops
re-extraction). Its own docblock records this shipping once via a hardcoded wrong
DB path — that fix corrected the path and left the shape. The guard checks the
*directory* too, so an empty store with an empty cache stays a clean exit.

**`serverCacheHostResolver.LiveHost`** handed a `StableServerKey` (lowercased) to
a raw-UDN cache lookup — CLAUDE.md's own documented "carry BOTH spellings" class,
live on the byte path, where the admin badge had been fixed and the byte path had
not. An upstream with any uppercase in its UDN 503'd every fetch.

The `sync.Map` memo a reviewer proposed for the fallback was **declined on a
measurement**:

```
BenchmarkLiveHostFoldedFallback/upstreams=1-12    297.9 ns/op    304 B/op   2 allocs/op
BenchmarkLiveHostFoldedFallback/upstreams=5-12    410.6 ns/op    912 B/op   2 allocs/op
BenchmarkLiveHostFoldedFallback/upstreams=20-12   994.6 ns/op   3216 B/op   2 allocs/op
```

Sub-microsecond once per request, on a path that proxies multi-megabyte FLACs,
against a package-level memo that would outlive `runServe`'s in-process re-entry.

### CLAUDE.md's own top-of-file list had drifted from the sections that superseded it

Four claims corrected, all in **"Don't regress these cross-cutting invariants"** —
the shortest, oldest, most authoritative-looking list in the file:

| claim | reality |
|---|---|
| root flip calls `WipeAllTracks()` | calls `WipeFilesystemTracks`; `WipeAllTracks` has **zero** production callers and CASCADE-deletes `upnp_track_routing` — the exact destruction the Scanner section forbids and explains |
| two sanctioned `enriched_at` resets | four, two of the omitted ones live callers |
| "admin console is loopback-only, no auth" | true in loopback mode only; public mode is credentialed and is what the VPS runs |
| five subcommands | `run()` dispatches 28 |

The first is the dangerous shape this file already warns about: *a live imperative
whose literal implementation reintroduces a fixed defect.* A session following it
would compile, pass the suite, and destroy an upstream library on a root toggle.

### Process notes worth keeping

- **`relay.py` hardcoded the iOS primer.** Its `ROOT` pointed at
  `~/dev/gemini-review`, so every prior bridge-side relay reviewed **Go against
  Swift false-positive rules**. Fixed with a `--primer-dir` flag (default
  unchanged); the bridge's Go primer lives at `~/dev/gemini-review-bridge/`.
- **A negative control audits the tests you just wrote — including the ones you
  wrote on review advice.** Taking the "fail closed on a nil predicate" change,
  then re-running the control, showed that flipping it *back* to fail-open left
  the whole suite green: the disabled-gate test passes a real predicate and
  cannot see the nil arm. `TestAnalysisSweeperNilGateIsOff` closed that.
- **Control BEFORE commit loses work.** One PR's control ran on an uncommitted
  tree, and `git checkout --` took the fix with the mutation. The rule in
  `docs/LoupeReviewCycle.md` is there for a reason and it is easy to skip when
  the edit feels small.
- **A source-scanning guard fires on its own docblock.** Twice in one run — the
  `config.Load` sweep and the uninstall-prompt guard. This package's commentary
  names what it discusses. Strip comments *and* string-literal contents, or
  better, drive the real function.

## LOUPE cmd/bridge, batch 2 — the deferred set (PRs #856 / #857 / #858, 2026-09-06)

The ten findings the first batch recorded as deferred, plus the three PRISM
items it had not triaged in depth. All thirteen shipped the same day; none was
dropped. Plan and per-item detail in
[`plan-2026-09-06-loupe-cmdbridge.md`](plan-2026-09-06-loupe-cmdbridge.md).

### The two with real operator consequence

**`bridge enrichment retry Artist/Album` reset the whole library.** `flag.Parse`
stops at the first non-flag argument, so the positional form — the one an
operator reaches for — parsed cleanly with `--path` **empty**, and an empty
scope means every row. One album's intent, a whole-library `enriched_at` reset
and a delta to every paired device. `library remove` had guarded this shape with
`fs.NArg() != 1` since PR #78; four commands in the package had the guard and
this one did not.

**`integrity.VariantWatcher`'s `stopFn` signalled without waiting.** `runServe`
defers that stop ahead of `manifestStore.Close()` — an ordering that means
nothing unless the stop joins, because a tick can still be inside
`DeleteVariant` when the store closes. Both long-lived loops in
`internal/integrity` now join, grace-bounded. The adapters they call also passed
`context.Background()`, so the work the new wait waits for was not cancellable:
a wait in front of uninterruptible work only delays `Store.Close()` behind
something that was never going to stop. They carry `scanCtx` now.

### What the negative controls caught — three of seventeen, all test defects

This is the batch's most reusable result. Every failed control was a defect in
the TEST, not the fix:

1. **A fixture that could not observe the change.** The `--dry-run` test used the
   bare install fixture, which has no variants — and `variantsMoveCmd` returns
   early on an empty set, *before* the `MkdirAll` under test. It passed against
   unfixed code. CLAUDE.md's "a fixture must be a value the transformation would
   actually change", caught exactly where it should be. The fix plants a variant
   AND asserts the preview reached the move loop, so it cannot silently regress
   to proving nothing again.
2. **A mutation that hit the selector, not the effect.** Flipping
   `if *pathScope == ""` left the `else` branch clearing suppression anyway.
   Mutate what the code DOES, not the branch that chooses how.
3. **A guard that read only the first name per `case` arm.** Mutating
   `case "duplicates":` into `case "duplicates", "dupes":` left the
   dispatcher/usage parity test green — an alias added that way dispatches and
   stays undiscoverable, which is precisely the failure the guard exists for.

### Windows CI caught a rule already written down here

Both new source-scanning guards read `main.go` with `\n` literals. No
`.gitattributes` pins `eol`, so a Windows checkout has CRLF:
`TestIntegrityAdaptersCarryACancellableContext` failed outright and
`TestEveryDispatchedSubcommandAppearsInUsage` would have passed **vacuously**.
Normalized at read time; verified by converting `main.go` to CRLF locally and
re-running, then restoring it byte-identically.

**The rule was already in CLAUDE.md under "Build, CI, and test discipline" and
was read during this very session.** Knowing a rule and applying it to code you
are writing at that moment are different acts; the platform leg is what closes
the gap, which is the argument for keeping the Windows leg blocking.

### `probeBridge` cannot answer for an ephemeral admin port

`adminAddress: 127.0.0.1:0` names no fixed port to dial, and `probeBridge` fails
closed on anything but connection-refused — correct for a write gate, but it
produced *"a bridge is answering on 127.0.0.1:0"* about an address where nothing
answers and nothing can. The gate still refuses (unknowable liveness must fail
closed) with the true reason. CodeRabbit then found the check compared port TEXT
against `"0"`, so `:00` read as fixed — and `validatePort` runs `Atoi`, so every
spelling of zero is a legal ephemeral port. Parse, don't compare.

The config-precedence fixtures had been using `:0`, which the new gate correctly
refuses to guess about; they now reserve a real closed loopback port.

### Bot coverage, stated rather than implied

**Only #856 got an LLM review.** #857 and #858 received Gemini's daily-quota
notice and CodeRabbit's rate-limit notice. SonarCloud and CodeQL ran on all
three. Seven PRs in one day is what exhausts them; the doc's instruction is to
name the gap rather than let "no comments" read as approval.

### The field loop — bridge.ars.md, 2026-09-06

Deployed both batches together via `deploy/linux/deploy-bridge-vps.sh`, which
worked unmodified: SHA-gated upload, detached two-step swap, `setcap`,
health-polled verify, backup prune. Serving `v0.1.9-142-g19b00dc` from
18:45:00 UTC with **zero journal warnings**. Rollback backup
`bridge.old-20260906-184500`.

**Four fixes verified on the live host rather than only in tests** — this is the
half of LOUPE that a green suite cannot supply:

| | before | after, on the host |
|---|---|---|
| `bridge token list` (no `--config`) | `read config ""` | resolves, and names both paths it tried |
| `bridge enrichment retry Some/Album` | silent whole-library reset | refused, exit 2, points at `--path` |
| `bridge manifest clear-missing` (bridge up) | proceeded | refused, names the answering address |
| `bridge duplicates --json --tier identical-audio` | every tier | `['identical-audio']` |

**A prediction this corrected.** The batch-1 write-up said to expect the
auto-analysis sweep to go quiet after deploy. It did not, and should not have:
`analysis.enabled: true` on this host, so the restored gate ALLOWS — the journal
shows `auto-analysis sweep enqueued tracks count=30` two minutes after boot. The
"goes quiet" behaviour is the DEFAULT-config case the defect was about; stating
it as a deploy expectation without checking the target's config was sloppy, and
checking took one `grep` of the host's yaml.

`smart-playlist regeneration families=12` and a `carPlayOptimize` feature flag on
`/v1/health` confirm the other two gates still allow where they should.

## 2026-09-06 — LOUPE on the retention / compact / reclaim trio (PRs #859 / #860 / #861)

Surface, not a window: PRs **#819** (`a9258b4`, compaction + page stats),
**#822** (`a1a89d7`, the two reaps + the daily sweeper) and **#829**
(`c080f98`, the diagnostics retention panel), all merged 2026-09-02 and
untouched since. 578 production lines, every one of them destructive or
reporting on something destructive. Live on `bridge.ars.md` at
`v0.1.9-142-g19b00dc` with both knobs at their default 0.

**#829 was never in this log.** It merged after the *2026-09-01 improvement
batch* entry was written and nothing added it — which is the same shape as a
rule that lands in the log and never reaches `CLAUDE.md`.

### Phase 0 — the structural finding

The surface had **no `### ` section** under *Things that have bitten before*.
Its three rules were distributed by subject: the `wal_checkpoint` ordering under
**Scanner**, the fail-closed empty-token guard and the 90-day floor under
**Config, settings and process lifecycle**. All three claims were still true —
but a session about to edit `internal/manifest/retention.go` reads the Scanner
section, which opens by declaring deletion passes "the highest-stakes area in
the codebase", and finds nothing about the two most destructive `DELETE`
statements in the tree. The run added the section.

### The measurements

**The overflow.** `now.AddDate(0, 0, -days).UnixNano()` with no ceiling. Go
documents `UnixNano` as undefined outside 1678–2262. Sweeping `days ∈ [1,
400000]`: **145,092 values produce a cutoff at or after now**, in two contiguous
bands (`127455..213504` and `340959..400000`). Representative values, `now =
2026-09-06`:

```
days         date          UnixNano()             verdict
99,999       1752-11-22    -6851185200000000000   no-op (the <= 0 guard)
127,455      1677-09-20    +9223360473709551616   WIPES THE TABLE
200,000      1479-02-06    +2955472473709551616   WIPES
365,000      1027-05-07    +7146216547419103232   WIPES
999,999      -0712-10-10   +7622535168547758080   WIPES
2,147,483,647 -5877584-02-27 -3446479029329846272 no-op
```

Driven end to end through the real `retentionSweeper.sweep()` against a real
`manifest.Store`: `days=999999` took history 2→0 and registrations 1→0,
**including a registration bound to a live, never-revoked token**, and logged
`retention: reaped playback history past the window rows=2 days=999999` as a
success. `ErrNoLiveTokens` guards only the orphan reap, so the registration
window has no second line of defence; and the existing `beforeNS <= 0` no-op is
inverted with respect to this — it catches the harmless direction.

`MaxRetentionDays = 36500` (100 years) refuses rather than clamps, matching the
floor's own stated reason. Writing the property test corrected the constant's
rationale: a 100-year window reaches back past 1970, so its cutoff is *negative*
and the `<= 0` no-op turns it into "delete nothing" — correct behaviour, but it
means "the cutoff is positive" was the wrong property. The test sweeps the whole
accepted range for the one that matters.

**The freelist under-report.** `page_size × freelist_count` was documented as
"what a Compact would return". Reproduced on a 72,474,624-byte store:

```
deletion pattern           freelist_count   floor        VACUUM returned
every 2nd row                           0           0        36,233,216
9 of every 10                      15,536  63,635,456        65,220,608
the first 90% (contiguous)         15,920  65,208,320        65,216,512
```

On the real `tracks` schema the scattered case is milder and no better: floor
81,920 against 5,398,800 actually reclaimed, a **66× understatement** the panel
renders as "1 %". The console printed the literal string `"nothing to reclaim"`.
`compact_test.go` carried a fixture guard — `if pre.FreelistCount == 0 {
t.Fatal("fixture produced no free pages; the test would prove nothing") }` — so
this was hit during development and worked around in the test.

**The WAL-blind footprint.** `before` was `os.Stat` of the main file, taken
before the pre-VACUUM checkpoint; `after` was taken once the mandatory
post-checkpoint truncated the WAL to zero. Against a store whose WAL could not
be checkpointed:

```
pre : .db=4,096       -wal=222,088,632
post: .db=2,334,720   -wal=0
{"beforeBytes":4096,"afterBytes":2334720,"reclaimedBytes":-2330624}
```

The headroom guard — whose entire job is preventing an ENOSPC mid-VACUUM —
demanded 8,192 bytes free for a vacuum needing ~4.6 MB.

**And then the fixture that proved it falsified the fix's own assertion.** With
a reader parked on the old snapshot the post-checkpoint answers busy, the WAL is
not truncated, and the vacuum's output sits in it on top of the original:
`main=1,671,168 wal=131,151,992 before=132,823,160 after=133,638,920 busy=true`.
Peak disk really has risen — exactly as `compact.go`'s header says it does
without a post-checkpoint — so *"a compaction can never report growth"* is
false. The clamp PRISM filed as a tidy-up is the only truthful rendering of that
state, and it moved to `CompactResult.ReclaimedBytes()` so one definition sits
beside the measurement that justifies it.

**The Windows probe.** `Compact` handed `freeBytes` the database FILE.
`transcode.AvailableDiskSpaceNearest` advances only on `os.IsNotExist`, so an
existing file passes through. POSIX `statfs` accepts one — which is why macOS
and the Linux VPS never noticed. Gemini consult (documented vs inferred,
2026-09-06): Windows `GetDiskFreeSpaceExW` opens `lpDirectoryName` with
`FILE_DIRECTORY_FILE`, so a regular file returns `STATUS_NOT_A_DIRECTORY` →
`ERROR_DIRECTORY` (267), uniform across Windows versions, filesystems, `\\?\`
and UNC. `compact_test.go` passes `nil` or a stub, so the blocking Windows CI
leg had never driven the real probe. The new test asserts the CONTRACT, so it
fails everywhere rather than only where the symptom appears.

**The diagnostics poll.** `apiDiagnostics`'s docblock said *"It touches no
database"* with the body's own "Three PRAGMAs" and "Two COUNTs and a MIN"
comments directly beneath it, and `app.js` carried a second copy of the claim as
the stated justification for `DIAGNOSTICS_POLL_MS = 5000`. `playback_history`
has no index on `started_at` (deliberately — a `started_at` index would force a
filesort in `ListHistory`), so the `MIN` turns a covering-index count into a
full `SCAN`:

```
rows      COUNT(*)+MIN(started_at)   COUNT(*) alone   3 PRAGMAs
18,000                     1.02 ms            55 µs       7 µs
90,000                     9.02 ms           748 µs       8 µs
500,000                   39.55 ms          5.51 ms      12 µs
```

Honestly sized: 1 ms every 5 s is not an emergency. The load-bearing half is
that a false comment was the *justification for a design choice*, which is how
the next database read gets added on the same reasoning.

**The knob with no control.** The Diagnostics panel said "set a window in
Settings" from the day it shipped. There was no such control — not in
`settingsPatch`, `settings_apply.go`, `settings.html`, or
`ops/settings-apply-semantics.md`. An env override *does* exist
(`BRIDGE_RETENTION_PLAYBACK_HISTORY_DAYS`, derived reflectively — a claim I got
wrong from a literal grep and had corrected by probing the real function), and
`Save()` round-trips the block, but `EnvOverrideDocs()` has no production caller
so the derived names are printed nowhere. Because nothing but the settings PATCH
moves `cfgHolder` and there is no config-file watcher, the sweeper's own *"read
LIVE on every pass, so a settings change takes effect on the next tick rather
than at the next restart"* described a capability nothing could reach.
`settings_matrix_doc_test.go` could not catch it: it is bidirectional, but its
universe is `reflect.TypeOf(settingsPatch{})`, never `config.Config`.

### Tests that pinned nothing — six, all found by mutation

1. **`TestSweepSkipsTheOrphanReapWhenTheTokenSetCannotBeRead`.** #822's body
   records finding the *first* version green against a sweeper that skipped
   nothing, and fixing it with a `countingReaper`. The upgrade was real; the
   FIXTURE still could not tell the guard it names from the one beside it, because
   `(nil, error)` also satisfies `len(ids) == 0`. Counterfactual under one
   weakened guard (`case err != nil && len(ids) == 0`): the partial-read fixture
   FAILS, the shipped fixture PASSES.
2. **The whole stale-device-registration branch** — deleting it left the entire
   `cmd/bridge` suite green.
3. **`TestPageStatsIsInternallyConsistent`** recomputed `PageSize*FreelistCount`
   from `PageStats`' own output.
4. **`TestDatabaseCompactReclaimsAndReports`** asserted `ReclaimedBytes ==
   Before-After`, which the handler computes from those same two fields — it holds
   against a `Compact` that never vacuums, against removing the mandatory
   post-checkpoint, and against the negative figure above.
5. **`TestDiagnosticsCarriesDatabaseSize`** checked only that the floor's KEY
   existed; the field has no `omitempty`.
6. **`RetentionCountsAvailable` was only ever asserted `true`** — hoisting it out
   of its error branch leaves every test green while the field's whole reason
   evaporates and the JS branch becomes dead code. The **507** mapping had never
   executed at all: `newTestServer` leaves `Deps.DBFreeBytes` nil.

### Process, recorded because it cost something

- **A control whose mutation does not exercise the assertion proves nothing** —
  the same rule as a fixture, one level up. Three of this run's controls passed
  against tests that were genuinely weak: one mutation (`FreelistCount =
  PageCount`) never produced the overstatement the assertion tests for; one
  fixture never reached a WAL-heavy state; one branch had no test at all.
- **A control whose mutation does not COMPILE proves nothing** — hit twice
  (`case false:` removing `err`'s only use; `if false {` orphaning an import).
  Both were replaced with the realistic wrong implementation, which is a better
  control anyway.
- **Commit before the control.** The JS-guard test was written after PR B's
  commit and destroyed by the next control's `git checkout -- internal/`. It cost
  nothing only because the apply script, not the tree, was the source of truth.
- **Strip comments before scanning JS.** The JS guard's first run reported the
  bug as still present: it had found the explanatory comment beside the fix, which
  quotes the offending string. `stripJSNoise` is the wrong tool here — it blanks
  string literals too, and the literals are the subject.
- **A wedged gate was a DISK problem, and the recorded rule found it.** A
  `make check` run stalled with `internal/manifest` at **0.0 % CPU** after
  11 minutes. The stack dump put the wedge in
  `TestListVariantsByPathPrefix_exactPrefixOnly` -> `OpenStore` -> `migrate` —
  an unrelated test, blocked inside SQLite. `df -h` said **97 % full, 17 GB
  free**, with a **27 GB** Go build cache and 1.4 GB of leftover test temp dirs;
  my own new fixture was building a **131 MB WAL** on top of that. This is
  exactly the shape already written down as *"when unrelated tests start
  failing, check `df -h /` before reading the failures"* — reading the failure
  first would have sent me hunting a phantom deadlock in the compaction code.
  `go clean -cache -testcache` reclaimed 27 GB, and the fixture was cut to a
  11 MB WAL, which is all the assertion ever needed: a `before` measured from
  the main file alone is short by `walBytes` whatever `walBytes` is. Its ~10 s
  runtime is unchanged and is `busy_timeout(5000)` twice, once per checkpoint
  the held snapshot refuses — that wait is what reaches the busy branch, so it
  must not be "optimised" away.
- **Gemini hit its DAILY QUOTA on the first PR**, so no PR in this batch got a
  Gemini review. CodeRabbit reviewed #859; SonarCloud and CodeQL passed. Recorded
  rather than implied, per the rule that a rate-limited bot's silence is not
  approval.
