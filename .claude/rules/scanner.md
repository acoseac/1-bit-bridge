---
paths:
  - "internal/manifest/scanner*.go"
  - "internal/manifest/extract*.go"
  - "internal/manifest/dhowden_*.go"
  - "internal/manifest/id3*.go"
  - "internal/manifest/mp3format*.go"
  - "internal/manifest/mp4*.go"
  - "internal/manifest/ogg_*.go"
  - "internal/manifest/codec*.go"
  - "internal/manifest/container_text*.go"
  - "internal/manifest/dsd_header*.go"
  - "internal/manifest/sacd*.go"
  - "internal/manifest/explicit*.go"
  - "internal/manifest/mbid*.go"
  - "internal/manifest/watcher*.go"
  - "internal/manifest/reconcile*.go"
  - "internal/manifest/deletion_journal*.go"
  - "internal/manifest/first_indexed*.go"
  - "internal/manifest/sample_format*.go"
  - "internal/manifest/name_collisions*.go"
  - "internal/manifest/eligibility*.go"
  - "internal/manifest/folder_art_state*.go"
  - "internal/manifest/provider*.go"
  - "internal/manifest/ladder_lock*.go"
  - "internal/manifest/store_open_busy*.go"
  - "internal/manifest/dupe_stamps*.go"
  - "internal/manifest/playlist_files*.go"
  - "internal/manifest/sqlfunc*.go"
  - "internal/manifest/store.go"
  - "internal/manifest/testdata/**"
  - "internal/fsutil/walkroot*.go"
  - "internal/fsutil/notafile*.go"
  - "internal/fsutil/resolve*.go"
  - "internal/fsutil/opendir*.go"
  - "internal/fsutil/symlink*.go"
  - "internal/fsutil/spelling*.go"
  - "internal/fsutil/lock*.go"
  - "internal/fs/**"
---
# Scanner, deletion passes and manifest writes

> **Condensed 2026-10-18.** Each long entry here is one line — its bold title plus its own bolded "Don't…/Never…" sentences — ending `→ History-rules-scanner.md`: that is `ops/History-rules-scanner.md`, where the entry sits verbatim with its reasoning, measurements and rejected alternatives. **Still binding.** Edit the line here and its long form there in the same commit; new long-form detail goes to that file, never back into this one.

The walk, the extractors and the tag and header parsers, the deletion and reconciliation passes, the watcher, and the store writes the scanner makes. The highest-stakes area of the codebase: a bug here deletes rows whose files are still on disk.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Scanner, deletion passes, and manifest writes

This is the highest-stakes area in the codebase: every bug here deletes rows
that still have files on disk, and the symptom reaches the user as "the bridge
lost my library."

- **"We could not see this path" dominates every "…but it looks like X"
  classification.** `isUnderErroredSubtree` must be evaluated BEFORE every other
  branch in the deletion loop that can delete. #549 added a case-fold rename
  reap ahead of it and reaped live rows of a case-twin directory during a
  permission flap; each new classification branch is a fresh chance to delete
  rows the walk never observed.
- **…and a read that did not complete is the same case: only a COMPLETED read may answer "not an SACD"** (2026-09-28) **Never `os.SameFile` against the walk's stat** **Never compare the walk's stat with an LSTAT** → History-rules-scanner.md
- **…and a container the walk SEES keeps its virtual rows' missing count at 0, whatever the scan writes for it** (2026-09-29, backlog B217) → History-rules-scanner.md
- **An SACD image's virtual rows carry the cover beside the image, and the container's skip gate watches that cover** (2026-10-14, backlog B146) → History-rules-scanner.md
- **An `.iso` that is not an SACD is probed once per process, not once per scan** (2026-10-14, backlog B157) **Don't record a probe that did not complete, don't key the verdict on the path alone, and don't move it into `tracks`** → History-rules-scanner.md
- **A route asked for a virtual track path answers 404 `not_found`, because `ResolveChecked` counts a stat's ENOTDIR as not found** (2026-10-16, backlog B414) **Never widen the answer past ENOTDIR** → History-rules-scanner.md
- **A file the walk reaches through a link is indexed under its TARGET's stat** (2026-09-28) → History-rules-scanner.md
- **…and a route that serves a file's bytes opens it with `fsutil.OpenAsFile`, never `os.Open`** (2026-09-28, B46) → History-rules-scanner.md
- **…and so does everything else that reads a library file in this process, and the listing opens its directory with `fsutil.OpenDir`** (2026-09-29, backlog B62) → History-rules-scanner.md
- **…and a worker writes a file's row only from a read that COMPLETED: a file it could not read whole keeps the row it had, and a new one gets none** (2026-09-29, backlog B134) **Don't count those as failed reads** → History-rules-scanner.md
- **A case-only rename reaps the old row only when this pass committed the new spelling** (2026-10-14, backlog B175) → History-rules-scanner.md
- **…and a folder's cover reaches the tracks beside it when IT changes, not only when they do** (2026-09-29, backlog B141) **never by "is a regular file"** → History-rules-scanner.md
- **…and a file its extractor REFUSES records the refusal, and the skip gate asks its row nothing more until the file or the extractor changes** (2026-09-29, backlog B145) **Don't rewrite the row** → History-rules-scanner.md
- **A library ROOT that is itself a link to a directory is walked THROUGH, and every walk of a root starts from `fsutil.WalkableRoot`** (2026-09-28, backlog B41) → History-rules-scanner.md
- **…and a link chain whose trailing-slash Lstat disagrees with Stat is still walked, under the configured spelling** (2026-10-12, backlog B257) → History-rules-scanner.md
- **The walk of the string `WalkableRoot` returns is `WalkConfigured`** (2026-10-13, backlog B358) → History-rules-scanner.md
- **A subtree scan OF the root runs the clean-empty guard Scan runs, and a line about a linked root names what it links to** (2026-09-28) → History-rules-scanner.md
- **…and the guard counts only LIBRARY CONTENT, by the walk's own rule** (2026-09-28, CodeRabbit on #1076) → History-rules-scanner.md
- **…and the rows it counts are the ROOT'S OWN: never a UPnP-routed row** (2026-09-29, backlog B51) → History-rules-scanner.md
- **A link to a directory BELOW a root is still not followed, and the scanner SAYS so** (2026-09-29, backlog B74) **Never the `ModeSymlink` bit** → History-rules-scanner.md
- **A configured root that is a link is WATCHED at the directory it resolves to, and every event under it is named back under the configured root** (2026-09-29, backlog B51) → History-rules-scanner.md
- **Every containment check and sidecar walk resolves a Windows junction as it resolves a symlink** (`fsutil.ResolveLinks`, 2026-09-29, backlog B51) → History-rules-scanner.md
- **The five post-scan reconciliation passes all exclude UPnP-routed rows, from ONE routed set computed at the reconciliation head** → History-rules-scanner.md
- **Reconciliation is directory-scoped and never crosses directories** → History-rules-scanner.md
- **A year before the phonograph is a composition date beside a recording year** (2026-10-13, backlog B342) → History-rules-scanner.md
- **A version-stale re-read that a reconciliation pass would rewrite is judged by the passes, at the scan's tail, with every such re-read in place** (2026-09-29, backlog B188) → History-rules-scanner.md
- **`Scan`'s duplicate-restamp tail runs from a `defer`, and `ScanSubtree` has
  the same tail.** Three exits reach `return count, nil` *after* the deletion
  pass has committed; reached inline, a reaped winner left its twin
  `dupe_suppressed = 1` with no served copy — an album invisible to every
  client. Best-effort is NOT symmetric here: an unstamped row is served
  (fail-open), a stale suppression hides one (fail-closed).
- **The restamp re-checks for an in-flight scan immediately before `ApplyDupeStamps`** **don't widen the public `IsScanning`** → History-rules-scanner.md
- **An external restamp that finds a scan in flight returns `ErrDupeRestampDeferred` and stamps nothing** (2026-10-14, backlog B184) → History-rules-scanner.md
- **A row deleted where no stamping pass follows reaches the duplicate stamps through a COUNT kept where every deletion passes** (2026-10-02, backlog B218) → History-rules-scanner.md
- **`tracks.first_indexed_at` is set once, on insert, and an update of the same path never moves it** ( 2026-10-07, sync phase 3; generation, the invariant carry key, and review round 3 on 2026-10-08) → History-rules-scanner.md
- **Every `indexed_at` bump goes through `indexedAtAdvanceSQL`** → History-rules-scanner.md
- **A writer that writes back a row it READ earlier writes it only while the row is still the one it read: `MarkEnriched` and `applyReconciledTracks` compare-and-set on `indexed_at`** (2026-09-29, backlog B187) **Don't compare the `tags_json` bytes** → History-rules-scanner.md
- **Any path predicate that writes, deletes, or bounds a scope MUST be a byte range, never `LIKE`.** → History-rules-scanner.md
- **`unicode_lower` does TWO jobs, and only the CASE half is unwanted on a destructive path — so the fold stays as the CANDIDATE GENERATOR and the strictness moves to the acceptance.** → History-rules-scanner.md
- **The threshold reap unlinks its sidecars**, and both DELETE arms and both sidecar enumerations are derived at compile time from one shared `where` const so the unlink set and the row set cannot diverge; **`ClearMissingCounts` does the same for the rows it deletes** (2026-10-14, backlog B186). → History-rules-scanner.md
- **Every writer holds `Store.mu`; reads stay un-mutexed** (WAL handles
  concurrent readers). The mutex also protects multi-statement transactions and
  `SELECT sidecars → DELETE rows → os.Remove` ordering — `busy_timeout` is a
  retry, not a serializer. Don't set `SetMaxOpenConns(1)`.
- **A manifest path names its own root — map it with `fs.Resolver`, never by joining onto one.** → History-rules-scanner.md
- **A path a CLIENT names is scanned, retired and recorded under the spelling its folders list, never the client's: `fsutil.Speller`** (2026-10-02, backlog B219) **Don't respell inside `ScanSubtree`** → History-rules-scanner.md
- **Two spellings of one name in one folder stay two rows and are REPORTED: `bridge doctor`'s `name-collisions`** (2026-10-16, backlog B420) **Don't give it another fold.** → History-rules-scanner.md
- **A single↔multi root flip calls `WipeFilesystemTracks`, never
  `WipeAllTracks`** — the latter CASCADE-deletes `upnp_track_routing`,
  destroying an entire upstream library and its cached enrichment on a mere
  root-count toggle. The folder wipe is part of it (folders flip form too).
- **A root add or remove is serialised with the scanner by cancelling the scan that already read the previous roots and waiting for it to stop writing** (2026-10-13, backlog B183) → History-rules-scanner.md
- **Migrations are append-only**; `sql` must be idempotent. A shipped migration
  is never rewritten — both live bridges already ran it, so editing it changes
  only fresh installs while diverging from what deployed DBs did.
- **One opener of a database runs its migration ladder at a time** (2026-10-14, backlog B185) **Don't fix such a race inside a shipped migration or the frozen column helper** → History-rules-scanner.md
- **…and an opener's open and ping are tried again while the error's primary code is SQLITE_BUSY, for 5 s from the first busy answer; any other error fails at once** (2026-10-22, `openStoreDB`): a plain code 5 there comes only once busy_timeout's own 5 s have run out, so a budget counted from the first try would never try again. **Don't raise `busy_timeout` instead, and don't widen the retry to the migration's statements without a failure there.**
- **Column-only columns must never gain `json:` tags or be spliced onto wire
  output**: the v25 format facts, `acoustid_match` (v28), `dupe_*` (v31),
  `audio_md5` (v32), `booklet_tag`, `artwork_version`. `tags_json` stays the Go
  readers' truth; the columns are query accelerators. `marshalForStorage` zeroes
  the spliced fields (`Enriched`, `BPMEstimated`, `Variants`) on the way in.
- **Bump `manifest.ExtractorVersion` on EVERY extraction-logic change** (const, must stay ≥ 1 — at 0 the `stored >= current` gate never re-extracts) → History-rules-scanner.md
- **A string stored in `tags_json` is valid UTF-8, decoded where the format says what the bytes are** (2026-10-14, backlog B181) → History-rules-scanner.md
- **`plausibleDuration` has BOTH ends, and "empty" has three spellings** (ExtractorVersion 13, #966) → History-rules-scanner.md
- **A fix placed IN FRONT of a check can destroy that check's input, and the AIFF sentinel is the case** (ExtractorVersion 14, #967) → History-rules-scanner.md
- **A range gate does not protect a value computed in a type that can wrap INTO the range.** → History-rules-scanner.md
- **`internal/upnpingest` is the one `Track.Duration` writer outside `internal/manifest`, and it passes the same gate** → History-rules-scanner.md
- **Every derived `Track.Duration` passes ONE gate, `plausibleDuration`, and the AIFF / WAV walkers add the DFF `payloadFits` rule through one `iffPayloadFits`** → History-rules-scanner.md
- **`enriched_at`'s sanctioned writers are a closed set** → History-rules-scanner.md
- **`enriched_at = 0` is NOT a tag-re-extraction reset**, and a zeroed
  `mtime_ns` IS: the skip gate reads the `size` and `mtime_ns` columns through
  `GetTrackStat` (measured 2026-09-28). This bullet said until then that
  `mtime_ns = 0` does not work because `GetTrack` reads the mtime from inside
  `tags_json`, which described the gate before #574 moved it to `GetTrackStat`. See
  `## Local test fixture` for the working paths.
- **The scanner excludes its own variant sidecars** via an ANCHORED filename
  match (`^(upscaled|optimized)-v\d+-\d+-\d+$` on the final dot-segment, with
  the part before it a supported audio ext). Don't loosen to a substring — it
  false-positives on a real `Song.optimized-Mix.flac`; don't skip a whole
  `variants/` directory by name.
- **`fillFromPath` is multiRoot-aware** — with a root-basename prefix the
  album/artist heuristics must strip `parts[0]` first, or an untagged file under
  a root named like an artist inherits the root basename as its Artist.
- **Per-iteration panic recovery lives INSIDE the per-file closure** → History-rules-scanner.md
- **A scan a shutdown stops reports no write the stop cancelled as a failure, and the final flush DROPS its batch on purpose** → History-rules-scanner.md
- **Anything walking FLAC metadata blocks SEEKS past a validated PICTURE payload, never drains it.** → History-rules-scanner.md
- **A picture dhowden reads is guarded by walking the file AS DHOWDEN DOES, at the one `tag.ReadFrom` call, for every extension** (2026-09-29, backlog B99) → History-rules-scanner.md
- **…and an ID3v2 tag dhowden reads is walked the same way, against two bounds** (2026-09-29, backlog B101) → History-rules-scanner.md
- **An ID3 genre is expanded only while it holds a handful of numeric references** (2026-10-12, backlog B246) → History-rules-scanner.md
- **An Ogg FLAC stream's tags are read from its own header packets, as a .flac file's are** (2026-09-29, backlog B102) → History-rules-scanner.md
- **An Ogg row carries the format facts its container holds, and an Ogg FLAC stream is classed as FLAC** (ExtractorVersion 25, 2026-10-14, backlog B118) → History-rules-scanner.md
- **An ID3v2 tag's MusicBrainz ids and ReplayGain are read BY NAME, from its TXXX frames and the MusicBrainz UFID** (ExtractorVersion 19, 2026-09-29, backlog B116) **Don't widen it without the app** → History-rules-scanner.md
- **An ID3v2 tag's TXXX:WORK is its work, and its content group (TIT1, TT1) only when it holds none** (ExtractorVersion 26, 2026-10-16, backlog B127, the owner's decision) **Don't put TIT1 back in front** **don't change the reading on one side alone** → History-rules-scanner.md
- **Every dhowden read goes through `dhowdenReadBuffer`, a page at a time** (2026-09-29, backlog B117) → History-rules-scanner.md
- **A tag a container keeps in chunks of its own outranks the path's guess, and a DFF's are read where its writers put them** (ExtractorVersion 20, 2026-09-29, backlog B140) **Don't write a path-guessed field only while it is empty** → History-rules-scanner.md
- **A compressed AIFF-C or WAV is named by its encoding, and counted lossy** (ExtractorVersion 21, 2026-09-29, backlog B124, B154) → History-rules-scanner.md
- **Extraction: presence-gate the integers, refuse bit depth on lossy codecs, and keep the work apart from TIT2→Title** (the work is TXXX:WORK, else TIT1: the B127 bullet above; this said "split TIT1→Work" until then) → History-rules-scanner.md
- **Two raw keys that normalise to one `stringOf` alias are the first non-empty value in ascending raw-key order** (2026-10-14, backlog B182) → History-rules-scanner.md
- **dhowden's Vorbis reader answers year 1 for a DATE it cannot parse, so a Vorbis 1 is re-read like a 0** (ExtractorVersion 22, backlog B222, B234) → History-rules-scanner.md
- **Explicit content reaches the wire as `explicit`** (ExtractorVersion 23) → History-rules-scanner.md
- **A PCM track's sample kind (`Track.SampleFormat`) is what its header states, and nothing else** (2026-10-23, backlog B513) **Don't read an extensible WAV's kind from the GUID's first two bytes, don't default an unknown to integer, don't change `codec` or `bitsPerSample` with it, and never put a kind on an isDSD row** → History-rules-scanner.md
- **…and it lives in `tracks.sample_format` (v59), spliced by the readers and never in `tags_json`** (2026-10-23, backlog B513) **Don't move it into `tags_json`, and don't leave it out of a track write or the stamp leg** → History-rules-scanner.md
- **…and a kind carries the stat it was read at, so an older binary's rewrite leaves a kind every open clears** (2026-10-23, backlog B513, rounds 3 and 4) **Never write `sample_format` without `sample_format_size` and `sample_format_mtime_ns`, nor at a stat its write did not compare with the row's; don't let the fill run before the open's clear; and don't move the stat columns into v59, which a build shipped without them** → History-rules-scanner.md
- **…and the rows extracted before it get it without a rescan: every open decides what a codec decides, and the header pass reads each WAV and AIFF row's format chunk** (2026-10-23, backlog B513; the fill's paging, round 3) **Don't stamp a routed row by its codec, don't read past the format chunk, don't record a kind from a file whose size and mtime are not the row's, don't let the header read and the walkers disagree on which chunk names the file, and don't start a fill chunk anywhere but past the path the last one ended on** → History-rules-scanner.md
- **…and the backfill is delivered once: nothing it writes moves `indexed_at` before a header pass has run to its end, which records the completion mark the health key reads, and every kind it records after the mark moves it** (2026-10-23, backlog B513, round 3) **Don't let the fill or the header pass move `indexed_at` before the mark, don't leave a kind they give a row after it unmoved, don't record the mark from a pass that did not reach its last page or while a row the fill decides is undecided, and never remove it** → History-rules-scanner.md
- **B513 changed extraction without an `ExtractorVersion` bump, and that is the bounded exception, not a precedent for a `tags_json` field** (2026-10-23) **Don't skip a bump for a field the version-stale diff compares** → History-rules-scanner.md
- **A repeated Vorbis comment for GENRE, ALBUM, COMPOSER, CONDUCTOR or WORK is joined with `"; "`** (ExtractorVersion 24, backlog B328) → History-rules-scanner.md
- **`normaliseRawTagKey` canonicalizes a LEADING `0xA9` before `ToLower`.** → History-rules-scanner.md
- **dhowden/tag does not read `gnre`, and keeps the data-atom locale on every MP4 freeform value** (ExtractorVersion 15, from a report that Rock / Pop listed only DSD) → History-rules-scanner.md
- **`skipID3v2` walks a STACK of prepended ID3v2 tags, at most `maxStackedID3v2Tags` (8), not only the first** (ExtractorVersion 16, from the iOS app's FLAC follow-up to #1935) **Don't stop after one tag, don't drop the cap, and don't change one side's walk without the other.** → History-rules-scanner.md
- **`Track.Compilation` puts the file's TCMP / `cpil` / COMPILATION flag on the wire** (ExtractorVersion 17, for the iOS app's "hide artists who appear only on compilations") → History-rules-scanner.md
- **`Track.Enriched` allocates per row; don't reintroduce package-level singleton
  bool pointers.** `Track` is exported, so a shared pointer lets any downstream
  write clobber every subsequent read for the process lifetime. The cost is
  ~450 KB across a 50k-track library — noise against the query and marshalling.
- **Default `ScanIntervalSec` is 21600 (6h), not 3600.** An hourly cadence on a
  mechanical NAS prevents idle spindown — an operator-facing wear hazard.
- **The fsnotify watcher is OFF by default and the periodic scan is the safety
  net.** Path containment uses `filepath.Rel`, never `strings.HasPrefix` — the
  byte-exact form is case-sensitive on macOS/Windows where the filesystem isn't.
  Linux watch-limit handling is two-layer (runtime fallback to periodic-only, plus
  a `bridge doctor` pre-flight at 80% of the budget).
- **…and on macOS every watch is an open file, so the watcher keeps to a budget and releases what fsnotify keeps** (2026-10-02, backlog B212) **Never Add a folder from its Create handler on kqueue** → History-rules-scanner.md
- **A folder on the watch list is watched before the event that flushed it is handled** (2026-10-13, backlog B337) → History-rules-scanner.md
- **A quiet kqueue flush writes a file outside the library, and that file's event is what watches the waiting folder** (2026-10-13, backlog B361) → History-rules-scanner.md
- **Carry the `acoseac/fsnotify` replace for the kqueue remove/Close order race until upstream ships a fix for fsnotify/fsnotify#795** (2026-10-20, backlog B401) **Don't drop the replace or bump fsnotify past it until a release carries that fix, and then remove the replace in the same change.** → History-rules-scanner.md
- **A WalkDir-resume cursor compares in TRAVERSAL order, not raw string order.** → History-rules-scanner.md
- **New shared scanner fixtures go in the untagged `scanner_fixture_test.go`**,
  never a build-tagged file — untagged siblings referencing them broke the
  Windows compile of the whole `manifest` test binary, invisibly.

- **Every byte-range prefix consumer trims the trailing slash, including
  the per-FOLDER twin.** `EligibleRollupByPrefix` did; `EligibleCountsForFolders`
  beside it did not — latent only because the browse handler passes bare
  paths, which is the exact state the rollup bug was in before a second
  caller forwarded a raw one (#536). The result stays keyed by the caller's
  spelling so a lookup by what was passed still hits. (#919)

- **A playlist file is indexed, never a track, and its members resolve when the feed is asked, not when it is read** (2026-10-17, backlog B431) **Don't resolve members at scan time, don't `path.Clean` a playlist line (a climb above the root is dropped even when it comes back down), don't let a failed read write the row, don't set or clear `complete` anywhere else, don't read every file before writing or load every stored line for a request, don't take the folder evidence from a running scan or from anything but the walk and `backed`, and don't edit `testdata/playlist-parse-cases.tsv`** → History-rules-scanner.md

- **ExtractorVersion 29 validates DSF fmt channels and persists DFF `raw`/`missing`/`unsupported` evidence.** Duplicate/conflicting/short CMPR, multiple PROP and malformed trailing properties are unsupported, never raw. Version 30 keeps the first parsed PROP's known format facts across duplicate PROP as well as malformed later properties; evidence still says unsupported. Stale scan updates full/delta once. → History-b470-native.md, “Owner approval (2c.2b)”.
