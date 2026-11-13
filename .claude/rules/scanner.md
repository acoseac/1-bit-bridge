---
paths:
  - "internal/manifest/scanner*.go"
  - "internal/manifest/extract*.go"
  - "internal/manifest/wavpack*.go"
  - "internal/manifest/ape*.go"
  - "internal/manifest/correction*.go"
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
  - "internal/manifest/nested_roots*.go"
  - "internal/manifest/reconcile*.go"
  - "internal/manifest/deletion_journal*.go"
  - "internal/manifest/first_indexed*.go"
  - "internal/manifest/sample_format*.go"
  - "internal/manifest/name_collisions*.go"
  - "internal/manifest/eligibility*.go"
  - "internal/manifest/folder_art_state*.go"
  - "internal/manifest/provider*.go"
  - "internal/manifest/ladder_lock*.go"
  - "internal/manifest/ladder_schema*.go"
  - "internal/manifest/migration_disarm*.go"
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
  - "internal/fsutil/casefold*.go"
  - "internal/fsutil/spelling*.go"
  - "internal/fsutil/lock*.go"
  - "internal/fs/**"
  - "cmd/bridge/serve_data_dir_record*.go"
  - "cmd/bridge/serve_data_dir_refusal*.go"
  - "cmd/bridge/serve_data_dir_identity*.go"
  - "cmd/bridge/data_dir_away*.go"
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
- **…and the watcher's look reads a root holding only links to directories as the scan's guard does, through the one reading they and the owning-root audit share (`lookAtContentlessRoot`)** (2026-11-07, backlog B742) **Don't classify a root holding no content anywhere but `lookAtContentlessRoot`, or clear `absent` for the links case: the parent scans read it.** → History-rules-scanner.md
- **A configured root that is a link is WATCHED at the directory it resolves to, and every event under it is named back under the configured root** (2026-09-29, backlog B51) → History-rules-scanner.md
- **Every containment check and sidecar walk resolves a Windows junction as it resolves a symlink** (`fsutil.ResolveLinks`, 2026-09-29, backlog B51) → History-rules-scanner.md
- **The five post-scan reconciliation passes all exclude UPnP-routed rows, from ONE routed set computed at the reconciliation head** → History-rules-scanner.md
- **Reconciliation is directory-scoped and never crosses directories** → History-rules-scanner.md
- **A year before the phonograph is a composition date beside a recording year** (2026-10-13, backlog B342) → History-rules-scanner.md
- **A physical album has one year: every track of a directory's album takes the dominant recording year of what its tracks' tags say** (2026-11-06, backlog B756; the tags' vote, the missing copy and the separators from its review's round 2, the record-only write and the subtree election from its round 3) **Don't cross a directory, and don't let a recording year split a physical album.** **Don't write a track already at its album's year, run the passes over a subtree scan's changed files, or change the vote, its tie, its group key or `testdata/album-one-year-cases.json` on one side alone.** **Don't let a year the pass wrote vote, put the record on the wire, give a change in it alone a revision (an `indexed_at` advance or an enrichment reset), write it alone at any version but the one its re-read was merged with, let a write-back of an earlier read put an older record back, keep it through a re-read whose tag reads otherwise, leave a group's election as it was when a scan moves a missing count across zero, or split a stored path on a backslash outside Windows.** → History-rules-scanner.md
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
- **`manifest.RequiredSchema(v)`, the tables and columns a store at `user_version` `v` holds, which a snapshot's verification requires of its database (B653, `cli-serve.md`), is generated from the ladder itself: `TestTheLadderSchemaIsTheLadders` runs the steps one at a time over an empty database, as `migrate` runs them, and compares what each leaves with `ladder_schema_gen.go`, so a step that adds, drops or renames a table or a column fails it until the file is written again, by that test with `BRIDGE_UPDATE_LADDER_SCHEMA=1` set; rerun it after integrating a branch that changes the ladder too (B653 round 8: migration v62 arrived while the generated schema still ended at v61)** (2026-11-09, backlog B653 PR 1). **Don't edit the generated file by hand, or make it name more than what must be there: a store holding a table or a column more, one a rolled-back binary's step added, still holds every one its version requires.**
- **A new migration names what it adds in `cmd/bridge`'s `latestSchemaPresent`** (2026-11-12, backlog B761 PR 2; B652 met it too): four serve tests require a store one step behind the ladder (`storeOneMigrationBehind`) to lack what the last step adds, and ask it; once the ladder grows, that store holds what the helper names, and they fail until it names the new step's own table or column, which a run of `internal/manifest` alone does not show. **Don't leave it naming the step before.** `TestAMigrationWhoseRecordCannotBeWrittenDoesNotRun`, `TestALateFirstStartOfTheInstalledVersionCompletesItsInstall`, `TestATargetWhoseSettingsAreFixedConfirmsAnInterruptedRollback` and `TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots` ask it.
- **One opener of a database runs its migration ladder at a time, and one whose ladder lock cannot be taken runs it without, saying which kind of failure it met** (2026-10-14, backlog B185; the kinds 2026-11-02, B537 item 1) **Don't fix such a race inside a shipped migration or the frozen column helper, drop the timeout's or a link's refusal, or word the no-locks line as proof that the filesystem keeps none (ENOLCK does not show it)** `TestTheMigrationSaysWhichKindOfLockItCouldNotTake` and fsutil's `TestKeepsNoLocksReadsOnlyTheNoLocksAnswers` pin the kinds. → History-rules-scanner.md
- **…and an opener's open and ping are tried again while the error's primary code is SQLITE_BUSY, for 5 s from the first busy answer; any other error fails at once** (2026-10-22, `openStoreDB`): a plain code 5 there comes only once busy_timeout's own 5 s have run out, so a budget counted from the first try would never try again. **Don't raise `busy_timeout` instead, and don't widen the retry to the migration's statements without a failure there.**
- **A store open with a migration step pending first records, in an armed install marker in the database's directory, that this version opened the data dir and, since B660, that it began a migration (`State.StoreMigratedBy`, wherever an install is armed and this version is not the backup's, after serve's admission too: begun is changed, so no crash rollback passes it), and refuses the open before any step when it cannot; an open with no step pending asks nothing of the marker** (2026-11-02, backlog B613's rounds 2 to 4; `disarmBeforeMigrating`, `updater.DisarmBeforeMigrating`, which takes the install marker's lock inside the ladder's): `bridge doctor`, `clear-missing`, a scan or a backup can be the new version's first open of the store, and a rollback to the backup past its migration would be a downgrade. It refuses to migrate, too, where the marker vetoes its version's store migrations (`updater.ErrMigrationVetoed`, `State.MigrationVeto`: a rollback put the backup back over that version's binary, and the veto outlives the rollback's record until that version's own serve gets past its settings or a new install replaces the marker; round 4), with the marker's lock or without, and the database's directory is the data dir, so the bridge opens its store at `DefaultDBPath(dataDir)` alone. It refuses too, running no step and writing nothing, while the install of its version awaits its confirmation after a start that did not end cleanly and the crash rollback's checks at the limit would still put the backup back (`updater.MigrationHeld`, `updater.ErrMigrationHeld`; B660's second round: `bridge doctor`, run to see why starts fail, migrated the store and took the backup away for good), the start running now left out, read of the marker the open reads under the install marker's lock (`updater.RunningServeCounted`: the start the marker's token names holding its own lock, tried only where a start is counted, never the data dir's lock, which a command holds too; B660's third round), but for serve's own start's open, which carries its admission's permission (`manifest.OpenStoreAdmitted`, `updater.MigrationPermit`); `bridge doctor` then reads the store's pending steps without running them (`manifest.PendingMigrations`), and `bridge restore` refuses before its first write a snapshot it would migrate. **Don't run a step before the record, record on an open that migrates nothing, migrate under a veto of this version's migrations, migrate for a command while the install awaits its confirmation after a start that did not end cleanly, open serve's own store without its admission's permission, open a store anywhere but `DefaultDBPath(dataDir)`, or move it out of `migrate`.** `TestAStoreOpenThatMigratesRecordsItInAnArmedInstallMarker`, `TestAStoreOpenThatMigratesNothingLeavesTheMarkerAsItIs`, `TestAStoreOpenThatCannotRecordItsMigrationMigratesNothing`, `TestAStoreOpenRefusesToMigrateWhileARollbackOfItsVersionIsUnderWay`, `TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots`, `TestAMigratingOpenIsHeldWhileTheInstallAwaitsItsConfirmation`, `TestACommandDoesNotMigrateTheStoreWhileTheInstallAwaitsItsConfirmation`, `TestServeMigratesTheStoreAfterAFailedStart`, `TestAMigrationReadsTheRunningStartUnderTheMarkersLock`, `TestTheDoctorBesideACommandHoldingTheDataDirsLockCountsEveryStart` and `TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase` pin it. Rules in `config-lifecycle.md` (the settings-check bullet, and B660's).
- **Column-only columns must never gain `json:` tags or be spliced onto wire
  output**: the v25 format facts, `acoustid_match` (v28), `dupe_*` (v31),
  `audio_md5` (v32), `booklet_tag`, `artwork_version`, `deezer_pending_query`
  (v63, backlog B761), `deezer_pending_mbid` (v64). `tags_json` stays the Go
  readers' truth; the columns are query accelerators. `marshalForStorage`
  zeroes the spliced fields (`Enriched`, `BPMEstimated`, `Variants`) on the way
  in.
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
- **An M4A work already read keeps its answer; otherwise the ilst walk reads ©wrk and freeform WORK only under the exact com.apple.iTunes mean, WORK first, with non-empty class-1 UTF-8 values up to 64 KiB; oversized values are absent, never truncated or replaced by a sentinel** (ExtractorVersion 33, B428). **Don’t trim or fold the mean, change workOf’s precedence, or replace an existing answer: dhowden also reads com.mixedinkey.mixedinkey and com.serato.dj WORK, a pre-existing difference from the app. Conflicting WORK and ©wrk can differ from the app’s item-order winner; trimming is not its display cleaning.** → History-rules-scanner.md
- **Every dhowden read goes through `dhowdenReadBuffer`, a page at a time** (2026-09-29, backlog B117) → History-rules-scanner.md
- **A tag a container keeps in chunks of its own outranks the path's guess, and a DFF's are read where its writers put them** (ExtractorVersion 20, 2026-09-29, backlog B140) **Don't write a path-guessed field only while it is empty** → History-rules-scanner.md
- **A compressed AIFF-C or WAV is named by its encoding, and counted lossy** (ExtractorVersion 21, 2026-09-29, backlog B124, B154) → History-rules-scanner.md
- **Extraction: presence-gate the integers, refuse bit depth on lossy codecs, and keep the work apart from TIT2→Title** (the work is TXXX:WORK, else TIT1: the B127 bullet above; this said "split TIT1→Work" until then) → History-rules-scanner.md
- **Two raw keys that normalise to one `stringOf` alias are the first non-empty value in ascending raw-key order** (2026-10-14, backlog B182) → History-rules-scanner.md
- **dhowden's Vorbis reader answers year 1 for a DATE it cannot parse, so a Vorbis 1 is re-read like a 0** (ExtractorVersion 22, backlog B222, B234) → History-rules-scanner.md
- **A track or disc number is the leading number of the value the tag writes, followed by nothing, by "/" (whitespace before it or none) and what follows, or by whitespace, "of" in any case and what follows; an ID3v2 frame keeps dhowden's answer unless it is 0; and the presence gates ask for ID3v2.2's TRK and TPA in an ID3v2.2 tag alone, and for ID3v1.1's track byte** (ExtractorVersion 31, 2026-11-06, backlog B743) **Don't re-read a raw value under any key but the one the accessor read, replace an ID3v2 answer that is not 0, count an ID3v1 track of 0 as present, or ask for TRK and TPA outside an ID3v2.2 tag.** → History-rules-scanner.md
- **WavPack (`.wv`) and Monkey's Audio (`.ape`) are indexed from their own headers and their APEv2 tag, decoding nothing, and a length a file holds is checked against the file and a cap of its own before anything is read or allocated on its account** (ExtractorVersion 32, 2026-11-06, backlog B489, and its rounds 2 and 5) **Don't read a header past what the reference decoders take, take a first block or an APE header the file does not hold, a bitstream the reference decoder refuses or a seek table that names fewer entries than frames, call a stream lossless without the extended bits its samples need, trust a length for an allocation, keep more than `apeTagMaxValues` of a tag's values, walk a block's sub-blocks a read each, hand dhowden more than the last 128 bytes, take a read that ends short of the stat's size for a file cut as it was written, give a lossy hybrid or a DSD stream a sample kind, or a lossy hybrid a bit depth.** → History-rules-scanner.md
- **A `.wvc` is never a track; a hybrid `.wv`'s row depends on it in five states kept apart, recorded in `tracks.correction_key` (v61), and the skip gate asks it again with the walk's own stat comparison** (2026-11-06, backlog B489, and its rounds 2 and 5) **Don't index a `.wvc`, read one beside a lossless or DSD stream, call a `.wvc` matching from its first block alone, without the stream's flags, or without the stream's own last sample where its header states no usable length, take a total the `.wvc` states and the `.wv` does not, or two that differ, stop the look for a stream's end at a block with no samples, take a look that did not complete for absence or a refused open or read for a look that did not complete, compare its stat more loosely than the walk compares an audio file's, or put the key in `tags_json`.** → History-rules-scanner.md
- **Explicit content reaches the wire as `explicit`** (ExtractorVersion 23) → History-rules-scanner.md
- **A PCM track's sample kind (`Track.SampleFormat`) is what its header states, and nothing else** (2026-10-23, backlog B513) **Don't read an extensible WAV's kind from the GUID's first two bytes, don't default an unknown to integer, don't change `codec` or `bitsPerSample` with it, and never put a kind on an isDSD row** → History-rules-scanner.md
- **…and it lives in `tracks.sample_format` (v59), spliced by the readers and never in `tags_json`** (2026-10-23, backlog B513) **Don't move it into `tags_json`** **Don't leave it out of a track write or the stamp leg** → History-rules-scanner.md
- **…and a kind carries the stat it was read at, so an older binary's rewrite leaves a kind every open clears** (2026-10-23, backlog B513, rounds 3 and 4) **Never write `sample_format` without `sample_format_size` and `sample_format_mtime_ns`, nor at a stat its write did not compare with the row's; don't let the fill run before the open's clear; and don't move the stat columns into v59, which a build shipped without them.** → History-rules-scanner.md
- **…and the rows extracted before it get it without a rescan: every open decides what a codec decides, and the header pass reads each WAV and AIFF row's format chunk** (2026-10-23, backlog B513; the fill's paging, round 3) **Don't stamp a routed row by its codec, don't read past the format chunk, don't record a kind from a file whose size and mtime are not the row's, don't let the header read and the walkers disagree on which chunk names the file, and don't start a fill chunk anywhere but past the path the last one ended on** → History-rules-scanner.md
- **…and the backfill is delivered once: nothing it writes moves `indexed_at` before a header pass has run to its end, which records the completion mark the health key reads, and every kind it records after the mark moves it** (2026-10-23, backlog B513, round 3) **Don't let the fill or the header pass move `indexed_at` before the mark, don't leave a kind they give a row after it unmoved, don't record the mark from a pass that did not reach its last page or while a row the fill decides is undecided, and never remove it.** → History-rules-scanner.md
- **B513 changed extraction without an `ExtractorVersion` bump, and that is the bounded exception, not a precedent for a `tags_json` field** (2026-10-23) **Don't skip a bump for a field the version-stale diff compares** → History-rules-scanner.md
- **A repeated Vorbis comment for GENRE, ALBUM, COMPOSER, CONDUCTOR or WORK is joined with `"; "`** (ExtractorVersion 24, backlog B328) → History-rules-scanner.md
- **`normaliseRawTagKey` canonicalizes a LEADING `0xA9` before `ToLower`.** → History-rules-scanner.md
- **dhowden/tag does not read `gnre`, and keeps the data-atom locale on every MP4 freeform value** (ExtractorVersion 15, from a report that Rock / Pop listed only DSD) → History-rules-scanner.md
- **`skipID3v2` walks a STACK of prepended ID3v2 tags, at most `maxStackedID3v2Tags` (8), not only the first** (ExtractorVersion 16, from the iOS app's FLAC follow-up to #1935) **Don't stop after one tag, don't drop the cap, and don't change one side's walk without the other.** → History-rules-scanner.md
- **`Track.Compilation` puts the file's TCMP / `cpil` / COMPILATION flag on the wire** (ExtractorVersion 17, for the iOS app's "hide artists who appear only on compilations") **Don't make it a `*bool` or emit `false`** → History-rules-scanner.md
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

- **An extractor's warning names its file by `trackLogPath` and a read's failure by `walkErrReason`, never by the error whole** (2026-10-30, B604, the 0.2.2 logging audit). `faultNotingSource` hands each parser the file's own read, seek or stat error, an `*fs.PathError` naming the absolute path, and keeps it as it is, since the read faults decide on it (`readIncompleteError`); the MP4 walks' seven warnings (codec, ALAC bit depth, sample rate, duration, genre, advisory, rating) logged it whole, beside a relative path in four and the absolute path in three. They log `walkErrReason` now, which is `fsutil.ErrorWithoutPath`: the operation and the errno. The other extractors' older warnings still name the absolute path, which the privacy page says (`auth-tls.md`, #1055). **Don't log a read's error whole beside a file, and don't flatten it into a string where it is made: the read faults decide on it.** `TestAnMP4WalkThatCannotReadItsFileNamesNoAbsolutePath` pins it.

- **A library root or data dir on a volume mounted after the bridge starts is never taken for an empty library or a fresh install: the scan spares the rows of a root it cannot see or that holds no library content while the index holds its tracks; the library watcher holds such a root (`Scanner.lookAtRoot`, read as the scan's guards read it) and watches it again, with a scan of it, only once a look finds it as it was, and again a watched root whose folder is found another than its watch was added on (B777); the console writes nothing under such a root (B776); and serve, and every other command that makes, opens or writes the data dir (`refuseDataDirAway`), refuses, before it makes or writes anything there, a data dir the record beside the config names (`.bridge.yaml.datadir`) that holds neither `bridge.db` nor `server.lock`, and since B741 one that does not hold the identity the record holds (cli-serve.md's B741 bullet)** (2026-11-04, backlog B651; B727, 2026-11-07; B776 and B777, 2026-11-07, the two lines after this one; B741, 2026-11-08, the line after them). What stays open is named by backlog B776, B777 and B813. **Don't persist a device number or judge a mount by a device or the immediate parent, write into a library root, dispatch a scan for a root that reads as not mounted or watch one before a look finds it as it was, the walk of a root it is nested in included, watch a held root again over a watch opened at its path before (kqueue's entry watch on its bare mount point, which a mount since covers), watch a nested root under another spelling than the walk of the root it is nested in reaches its folder by (kqueue keys a watch by its spelling), take a Create of a watched root for a new folder, or ignore one once the root's own watch is gone (on kqueue its look waits for the flush a new folder's watch waits for), keep a scan armed under a root the watcher holds, give a path to any root but the deepest that holds it (a share's root, spelled with its separator or without, holds the paths below it), let a scan walk into or reap under a nested root that reads as not mounted (decided before the walk, which may never reach its folder), judge how a folder compares names by anything but the names in it (not its own name in the folder above, not one folder for a whole path, not a write, not one file under two names its listing holds, not lookups or a listing read again that do not agree with themselves or with each other), fold a case-only difference for which root a path is where its folder is not established to fold, a lookup there failed otherwise than for a name not being there, or one spelling was found alone, or keep one exact for what is spared where its folder may fold (an Unknown answer, a folder not there, or one that holds neither the name, another spelling of it nor library content), turn the check of the roots' own watches off on kqueue (it drops a renamed root's watch without a word too), keep the record in the data dir or in bridge.yaml, or of one spelling of the data dir alone (it holds the configured one and where it led), check or write serve's record beside any file but the one its boot read was of, refuse a data dir its record does not name, or one holding the identity its record holds or, under a record written before B741, which holds none, its lock file (the `rm bridge.db*` reset keeps both), or prepare the data dir before the check.** → History-rules-scanner.md
- **A console write under a library root asks first, of the deepest configured root that owns each path it would write as the path's elements spell it and of the deepest that owns where the write lands, the look the watcher holds a root by, and a request one of whose roots reads as absent is refused whole, nothing written, in the hold's words, but for a root whose path is not there at all and that the index holds no rows under, whose first write makes its folder (`Scanner.RefuseWritesUnder`, `RootAwayError`, `firstWriteMakesRoot`)** (2026-11-07, backlog B776, and its review's rounds 2 and 3, the last closing backlog B794; the console's half is `admin-console.md`'s B776 bullet) **Don't ask any root but the deepest that owns each path and the deepest that owns where it lands, resolve a link in `rootIn`, resolve the roots for a write with no link on its way, compare a root in another form than where the write lands (one whose path is not there resolved through its nearest ancestor that exists, as the write's folder is), read a root holding no content as absent without asking the index, or let a write make the folder of a root the index holds rows under, or of one unreachable but for its absence.** → History-rules-scanner.md
- **A watched root whose folder changes under its own watch is watched again: every check of the roots' own watches compares, where that watch is still fsnotify's, the identity the root's folder had just before the watch was added with the folder at its path now (`os.SameFile`), the start confirms each root's own watch once its walks are done, a root recorded with no identity takes one once its folder reads, its watch added again around the read, and a root that is there whose own watch fsnotify would not add is tried again on a backoff of its own, never held** (2026-11-07, backlog B777, and its review's rounds 2 and 3) **Don't take a root's identity after the walk that adds its watch, keep it past a watch added again, take an identity that cannot be read for a change, drop `rootIdentity`'s comparison of the stat with itself (on Windows it reads the file ID as the identity is taken), record one at a check without adding its watch again, watch a root that reads as a volume not mounted again at every check (it goes to the hold), leave a root whose own watch the start could not add unrecorded or hold it without a look, hold a root that is there whose own watch could not be added, or try it at every check, or walk its tree, scan it or say its refusal at a try.** → History-rules-scanner.md
- **A data dir's record keeps how each folder on the way of each of its two spellings compared the names in it when serve wrote it, one letter a folder from the volume's root down (`dataDirFolding`, `resolvedFolding`: `fsutil.FormatFolding` of `fsutil.FoldingOnTheWay` over `fsutil.FoldingIn`, CaseUnknown included), so that the record grows with its paths alone, however deep, and the check compares a spelling of the data dir in another case by that, probing nothing (`fsutil.SameAsRecorded`, `SamePathAsRecorded`): once the volume is away every folder below its mount point is gone, and a probe answers for the folder left at the mount point, on the volume above; a folder not read as case-sensitive takes two spellings for one, the direction that refuses, and so does every folder of a spelling whose folding is not one of its folders, a folding checked against its own spelling alone; the two spellings are compared below the deepest two of their folders, as many names above each end, that resolve to one directory, each name below by the letter of the folder the record's own spelling looks it up in, counted from its end, the resolved folders compared exactly, and where no two do, as written and with the config's as it resolves, name by name from the root (`fsutil.SamePathAsRecorded`), so that a link on the way to another depth, macOS's /var or a Windows short name, moves no letter off its folder; a folder that is there spelled in another case is one folder on Windows, whose resolution names it as it is on disk, whatever its letter, and on Linux and macOS is the letter's; a record holding an identity or a folding is never compared by the probe, and one written before B741, which holds neither, is (`fsutil.SamePath`) until a start with the volume there writes it again; the reader takes 2 MiB, past the record of any path an OS takes, and a record whose folding alone would take it past that keeps the letter u, every folder unknown, one longer without it never written** (2026-11-08, backlog B741's gap 3, and its round 2, Codex on #1441: keyed by each folder's whole path the record grew as the square of the depth, and past its reader's 64 KiB was written without its folding, which the check compared by the probe; and its round 4, CI's macOS and Windows legs on #1441: the letters laid over a resolution of another depth read a spelling through a link to the mount point as another data dir; the identity is cli-serve.md's B741 bullet). **Don't probe a record's folders at the check where the record holds an identity or a folding, key a folder's folding by its path, take a folder read as case-sensitive for one that folds, read a folding that is not of its spelling's folders as any of them, check a folding against any spelling but its own, lay its letters over a resolution, compare two resolved folders but exactly, or write a record its reader refuses.** `TestARecordedFoldingDecidesACaseOnlyDifference` (its case-sensitive sibling the control), `TestFoldingOnTheWayAsksEachFolderOnce`, `TestSamePathAsRecordedProbesNoVolume`, `TestARecordedPathComparesThroughAnAliasOfItsAncestors`, `TestARecordedPathComparesAsWindowsResolvesIt`, `TestARecordedPathComparesThroughAShortName` (Windows), `TestACaseOnlyDifferenceInAFolderThatIsThere`, `TestFoldersThatDifferInCaseAloneResolveApart`, `TestAPathNoFolderOfWhichResolvesIsComparedAsWritten`, `TestParseFoldingReadsWhatFormatFoldingWrites`, `TestServeRefusesTheDataDirItRecordedInAnotherCaseWhereItsFolderFoldsCase`, `TestServeRecordsHowEachFolderOnItsDataDirsWayComparesNames`, `TestServeKeepsTheFoldingOfADataDirHoweverDeep`, `TestARecordItsReaderWouldRefuseKeepsNoFolding` and, on a volume that folds case, `TestServeRefusesTheDataDirInAnotherCaseOnACaseInsensitiveVolume` (`BRIDGE_TEST_CASE_INSENSITIVE_DIR`) pin it. → History-rules-scanner.md
