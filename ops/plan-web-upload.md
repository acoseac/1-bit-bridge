# Plan — web upload and space management

Getting audio files *into* a bridge over HTTP, and letting a tenant manage space
once they are there, so neither requires shell access.

Three pieces, in dependency order: **upload** (PRs 1–4), **free-space
visibility** (PR 4, since the upload pre-flight already computes it), and
**delete** (PR 5, which breaks an invariant the bridge has held since it shipped
and therefore gets its own gate). **Move** is a follow-on — see the bottom.

**Audience:** whoever implements this, and the cloud control plane that will
depend on it. **Scope:** the admin surface only. Nothing here touches `/v1`;
`ProtocolVersion` is untouched, and there is no iOS mirror and no Mirror-PR pair.

Kept in `ops/` per the repo rule — it names no hosts, no keys and no unfixed
weaknesses, so the placement is convention rather than necessity.

---

## Why this exists, and what it is not

A cloud tenant has no filesystem. Today the only way bytes reach a library is
`rclone`/`scp`/a NAS mount, which is fine for `bridge.ars.md` (a B2 FUSE mount)
and impossible for a hosted customer. Upload is the missing on-ramp.

**It is deliberately not the bulk-ingest path.** A 20k-track library through a
browser tab is a bad trade: `webkitdirectory` hands the page a flat FileList it
has to hold in memory, Chromium's `showDirectoryPicker` isn't in Safari or
Firefox, and one flaky hour costs the whole batch. Two cheaper paths already
exist in outline and should carry real libraries:

- **Bring your own bucket.** `bridge.ars.md` already runs on an rclone FUSE mount
  over B2 — the cloud topology *is* the production topology. "Connect your
  S3/B2/Drive read-only" ingests any library size with no upload machinery.
- **`bridge push`.** The binary already cross-compiles to six targets. Resumable,
  parallel, reuses the existing auth. Tracked as a follow-on below, not in this
  stack.

Sizing target for this work: **an album to a few hundred tracks per session.**
Everything below is built so that the ceiling is the browser's, not the server's.

---

## Decisions

### D1 — Stage inside the target root, in a dot-directory

`<root>/.bridge-upload/<sessionID>/<fileID>.part`

The scanner's `shouldSkipDir` (`internal/manifest/scanner.go`) returns true for
**any** `.`-prefixed name and the walker returns `filepath.SkipDir` *before* the
`UpsertFolder` call, so a dot-directory inside a root is invisible to the walk:
no track rows, no folder row, and therefore no interaction with the bounded
deletion pass. Files are skipped by a `.`-prefix check too.

This is the load-bearing decision, because it makes commit a **same-filesystem
`os.Rename`**. The obvious alternative — staging under `<dataDir>/uploads` —
fails: on `bridge.ars.md` the data dir is the 29 GB root disk and the library is
a B2 FUSE mount, so every commit would be a cross-device `EXDEV` copy, writing
every byte twice. `updater.copyAndRename` exists for exactly that case and is the
wrong tool here.

Config escape hatch: `upload.stagingDir` for operators who object to a hidden
directory inside their music folder, with a documented cross-device copy
fallback. Default is inside the root.

**Partial files never carry an audio extension.** `.part` is not in
`manifest.Ext`, so even if a staging path were somehow walked,
`enqueueableAudioFile` rejects it. Belt and braces on top of D1 — and it matters,
because a truncated file landing in a library has already bitten this codebase
once (the partially-uploaded rclone-to-B2 m4a in `internal/analyze/decode.go`,
which passed the zero-byte skip and committed a permanently-wrong waveform keyed
to mtime+size, so the skip gate never re-analysed it).

### D2 — Raw `PUT` with `application/octet-stream`, never multipart

`csrfGuard` (`internal/admin/admin.go:1557`) wraps the **entire** mux and 415s any
body-bearing request whose Content-Type isn't `application/json`. It is not
per-route, so an upload endpoint is dead on arrival until the guard is amended
deliberately.

What the guard buys is a simple-request defense. CORS "simple" content types are
exactly `application/x-www-form-urlencoded`, `multipart/form-data` and
`text/plain` — those can be sent cross-origin with no preflight. **So
`multipart/form-data` is forgeable and `application/octet-stream` is not**, and a
`PUT` isn't a simple method either, so a raw-body PUT is preflight-forced twice
over. Using a multipart form here would give away the exact property the guard
exists to provide.

The amendment is therefore narrow: an allowlist of `(method, path-prefix)` pairs
inside `csrfGuard` that may carry `application/octet-stream`. Multipart stays 415
**even on upload routes** — that is a test pin, not a side effect. The Origin
check already runs and is unchanged.

### D3 — Rolling read deadline, not a bigger `ReadTimeout`

The admin server sets `ReadTimeout: 30 * time.Second`
(`internal/admin/admin.go:1655`), which caps reading the **entire** request body.
A 200 MB file dies at 30 s on anything under ~55 Mbps.

Raising it globally would give back the Slowloris protection PR #75 added. Instead
the upload handler extends its own deadline as bytes arrive, via
`http.NewResponseController(w).SetReadDeadline(...)` called every N bytes while
copying (a Go 1.20 API; this repo is on 1.26.5). A client that is genuinely
stalled still times
out; a client that is merely slow does not. The comment block above `ReadTimeout`
already explains why `WriteTimeout` is unset for long operations; this is the
same reasoning applied to the read side, scoped to one handler instead of the
server.

Chunking then becomes a resumability choice rather than a timeout workaround,
which is the right reason to have it.

### D4 — Opaque ids in URLs; user paths only in the session manifest

The client declares its file list up front and gets back an opaque `fileID` per
file. Nothing user-supplied ever appears in a URL path or query.

This sidesteps the `+`-decoding class outright (`url.Values` decodes `+` as a
space — the documented `/v1` variant-delete trap, and the reason `safeQuery`
exists in both `internal/api` and `internal/admin`). It also means a hostile
relative path cannot create anything anywhere on disk until commit, where it is
validated exactly once against exactly one root.

### D5 — Append-only, serialised per *file* — not per session

Chunks for one file must arrive in order, so a single file uploads serially. Two
parallel `PUT`s to the same `fileID` would both read the offset, both validate,
and both append — so the offset check needs a real lock, not just a comparison.

**The lock is per `(session, file)`, not per session.** Different files in a
session have different `.part` files and never contend; locking the session would
serialise the whole upload and throw away the parallelism that actually carries
browser throughput, since a folder upload is many files rather than one big one.

**The lock map is refcounted and reaped, and a naive `delete` is a race.** An
unmanaged `map[fileID]*sync.Mutex` leaks an entry per file for the lifetime of
the process, but deleting a key while another goroutine is about to acquire it
hands the two of them different mutexes for the same file. The shape that works:
the map is guarded by its own mutex, values are `*fileLock{mu; refs}`, acquire
does get-or-create-and-`refs++` under the map lock before taking `mu`, and
release does `refs--` under the map lock and deletes only at zero. The session
sweeper drops the whole map when it removes the staging directory, which covers
abandonment.

The tradeoff to record for future maintainers: per-file throughput is capped at
one in-flight chunk. If that ever binds, the fix is *not* to relax the offset
check — it is to move to fixed-size indexed slots (chunk N always writes at
`N * chunkSize`), which makes parallel chunks safe by construction at the cost of
a sparse file during transfer. Documented, not built.

### D6 — Auth inherits the admin posture; no new mechanism

- **Loopback mode:** no auth, per the documented posture. This adds nothing —
  anything on the host can already write to the library directly, and already owns
  the SQLite DB and the token store.
- **Public mode:** `sessionMiddleware` (`internal/admin/middleware_auth.go`)
  already gates every `/api/*` route with an adminauth session and returns JSON
  401. Upload gets this for free.

**The upload routes must not be added to `isAuthBypassPath`.**

**Do not put upload on `/v1`.** The `/v1` bearer is read-scoped by design across
every already-paired device, and the demo bridge's static token ships inside every
installed app (which is what `refuseUpscaleMutationInDemoMode` and
`refuseAtlasIngestInDemoMode` exist for). An upload endpoint there would be an
open file-drop. If a future "upload from your phone" feature is wanted, it needs
its own write-scoped credential and its own demo refusal.

### D7 — Default off

Consistent with the feature-default discipline: `upscale`, `autoOptimize`,
`analysis`, `fingerprint`, `atlas` and `libraryWatch` are all off because each
commits the operator to gigabytes, CPU, a third-party key, or an open endpoint.
Upload is an open write endpoint. The cloud control plane turns it on per tenant.

`upload.enabled` hot-applies: routes are wired unconditionally at boot and the
handler reads the live flag — the WIRED-vs-ACTIVE split from the settings
hot-apply work. Report `live` in the field report and add the row to
`ops/settings-apply-semantics.md`.

### D8 — Collisions skip by default

A destination that already exists is **skipped and reported**, never silently
overwritten. Per-session `overwrite: true` is opt-in. Overwriting a file the
operator already curated, because a browser re-sent a folder, is not a recoverable
mistake.

### D9 — Destination is verbatim; overlapping uploads are pre-flighted, not prevented

`webkitRelativePath` **includes the selected folder as its first segment**, so
selecting `Dark Side of the Moon/` yields `Dark Side of the Moon/01 Speak to
Me.flac` and selecting `Pink Floyd/` yields `Pink Floyd/Dark Side of the
Moon/01 Speak to Me.flac`. Destination is `<root>/<relativePath>` verbatim, with
an optional per-session destination prefix the UI exposes.

That means **uploading an album folder and then an artist folder containing it
produces two copies at different paths**, and D8's collision-skip never fires
because the paths differ. Traced through the rest of the system:

| Stage | Outcome |
|---|---|
| Scanner | Two rows — `path` is the PK |
| `dupes.KeyFor` | Same group. The key is `(albumID, disc, track, normTitle)` — **path is not in it** |
| `classify` | Not self-nested (`collapseNestedSegments` removes only *consecutive* duplicate segments, and these two collapse to themselves). Same geometry, both FLACs carry `audio_md5` and match → **`identical-audio`** |
| `PlanSuppression` | Ties through lossy/bits/rate/size, then `outranks` prefers the **shallower path** — so the flat `Dark Side of the Moon/…` wins and the properly-nested copy is suppressed |
| iOS | One copy. The suppressed path gets a `manifest_deletions` tombstone, so a synced client deletes it |
| Disk | **Both copies remain.** Suppression is serve-time only |

So nothing corrupts and clients stay correct, but the user pays double disk and
the *worse-organised* copy wins — and upload order doesn't help, because the
tie-break is on depth, not recency.

Two consequences for the design:

1. **The UI must show the resolved destination path before upload starts.** The
   leading-segment behaviour is the whole surprise; a preview of
   `<root>/Pink Floyd/Dark Side of the Moon/…` costs nothing and prevents it.
2. **Session create pre-flights duplicates.** The client already declares path and
   size per file, so a `(basename, size)` match against `tracks` — no hashing,
   one indexed query — catches this exactly. The response carries a `duplicateOf`
   hint per file naming the existing path; the UI **deselects those by default**
   and lets the user re-check them.

Pre-flight **warns, it does not decide.** Silently skipping would be wrong: a
track legitimately appearing on both an album and a compilation is a real library,
and that case is already serve-time dupe suppression's job, not upload's.

Re-uploading the *same* folder is the well-behaved case — identical paths, so D8
skips every file and nothing duplicates.

### D10 — The manifest offset is the truth; truncate the `.part` to it

A `PUT` that drops mid-chunk leaves trailing bytes in the `.part` file. If
"current staged size" were derived from the file, the client's resume from its
last acknowledged offset would either 409 forever or append *after* the garbage
tail, embedding it.

So the durable offset lives in the session manifest, and both the resume `GET`
and any offset-mismatch handler **truncate the `.part` down to it** before
answering. Bytes past the recorded offset were never acknowledged and are always
safe to discard.

**Staging files are opened `O_WRONLY|O_CREATE` — deliberately NOT `O_APPEND`.**
POSIX sets the file offset to end-of-file before *every* `O_APPEND` write, so an
explicit `Seek` is silently ignored. It would happen to produce the right bytes
here, since `Truncate(manifestOffset)` makes EOF equal the manifest offset — and
that is exactly the problem: the offset becomes an assumption the kernel
re-derives rather than an assertion the code makes. Open, `Truncate` to the
manifest offset, `Seek` to it, then write. A future bug that skips the truncate
then fails loudly instead of appending to garbage.

The write ordering is what makes that true, and it is load-bearing:

> write chunk bytes → fsync the part file → update the manifest offset → fsync the
> manifest

Recording the offset before the bytes are durable would let a crash advertise an
offset the file cannot honour, and truncate-on-open would then be discarding real
data instead of garbage.

### D11 — Delete is trash, not `os.Remove`

**The bridge has never removed a file from a library root.** Verified, not
assumed: every `os.Remove`/`os.RemoveAll` in the tree is a temp file, a sidecar
(variant / waveform / artwork / booklet / cover), a backup, the updater's own
binary, the config dir on uninstall, tsnet state, or a pidfile. `bridge
uninstall` promises this to the operator in as many words — *the user-supplied
`--library` path is read-only by design*.

Quota makes delete necessary anyway: at the limit, removing something is the only
move. But the shape matters, and the reason is specific rather than abstract.
This codebase has already shipped a prefix-scoping bug that deleted the wrong
things — `DeleteTracksByPrefix` under case-folding `LIKE` removed `/srv/music`'s
rows when the operator removed `/srv/Music`, and **the pre-confirm count
understated the damage**, because the count query was case-exact while the delete
folded. That was survivable only because it destroyed rows and sidecars, both of
which regenerate. The identical bug against library files does not.

So delete moves the file to `<root>/.bridge-trash/<ts>/<relPath>`. This is D1's
trick a second time, and it pays three ways: a same-filesystem rename is instant
even on the B2 FUSE mount; a dot-directory is invisible to `shouldSkipDir`, so
the scanner never re-indexes trashed content; and the file is recoverable for a
TTL window.

**Rows retire immediately, not at the missing-count threshold.** An explicit user
delete should not linger for three scans. Reuse the SACD shrink path —
`IncrementMissingTracksAndDeleteAtThreshold(…, 1)` — which already unlinks
sidecars and writes `manifest_deletions` tombstones, so synced clients drop the
tracks too. No new deletion machinery.

**Trash age comes from the `<ts>` directory name, NEVER from the file's `stat`.**
`os.Rename` preserves `mtime` — measured, not assumed: a file stamped 2019 and
trashed today reads as 2797 days old the instant it lands. An mtime-based TTL
sweeper would therefore purge it on the very next tick, and it would do so
*oldest-content-first* — destroying the recovery window for precisely the
material most likely to be irreplaceable. The timestamp directory exists for this
reason; the sweeper must parse it and nothing else.

**Restore must `MkdirAll` the destination parent first.** The directory a track
came from may have been removed after it was trashed (by the operator, or by
trashing its last sibling), so a bare `os.Rename` back returns `ENOENT` on
exactly the album-was-fully-deleted case restore exists for. Library content
directories are created `0o755` at both restore and upload-commit — the repo's
`0o700` hardening covers *its own* data directories, not the user's music, where
it would break a shared-mount setup.

**Trash does not free space, and that is the honest tension.** For a quota user
that is the entire point, so the free-space widget must count trash separately
and make purging it one click from where the problem was noticed. Restoring from
trash costs a re-enrichment (the row was retired), which is an acceptable price
for a recovery path.

Non-negotiables:

- **`library.allowDelete`, default off, and NOT folded into `upload.enabled`.**
  Enabling an additive feature must never silently enable a destructive one.
- **Byte-range path scoping, never `LIKE`** — the documented failure above.
- **The pre-confirm count and the delete must use the same bounds.** That
  divergence is what made the original bug invisible until after the fact.
- **Dot-segments are rejected**, so `.bridge-upload/` and `.bridge-trash/` can
  never themselves be targets.

Deleting the winner of a duplicate group needs no special handling — the next
restamp pass un-suppresses its twin.

---

## Free space

The upload pre-flight already computes this (`AvailableDiskSpaceNearest`). Today
the only display is `variants-free` on the Library page, which is the wrong volume
for this question and one navigation away from where it matters.

Surface it in the **sidebar**, which post-Hedwig is on every page — a quota user
needs it in peripheral vision, not behind a click. Three numbers: free, used by
the library, and **held in trash (reclaimable)** with an Empty action beside it.

**Progressive, not permanent.** Render it only when a floor or quota is configured
or free space is near the floor; a self-hoster with a 40 TB NAS should never see a
meter for a number that will never bind. Same instinct as the upscale stats card,
which stays hidden until the feature has something to say.

---

## Wire shape

All admin-only, all `/api/upload/*`.

| Method | Path | Body | Purpose |
|---|---|---|---|
| `POST` | `/api/upload/sessions` | JSON | Declare root + file list → session + per-file ids and offsets |
| `GET` | `/api/upload/sessions` | — | List active sessions (second tab, stale cleanup) |
| `GET` | `/api/upload/sessions/{sid}` | — | Per-file staged offsets — the resume read |
| `PUT` | `/api/upload/sessions/{sid}/files/{fid}?offset=N` | `application/octet-stream` | Append one chunk |
| `POST` | `/api/upload/sessions/{sid}/commit` | JSON | Rename staged files in, trigger one subtree scan |
| `DELETE` | `/api/upload/sessions/{sid}` | — | Abandon and clean staging |

**Append-only, no holes.** `offset` must equal the manifest-recorded offset (see
D10); a mismatch returns `409` carrying the actual offset so the client seeks
rather than guessing. That makes resume a single `GET` and makes a torn upload
impossible to mis-assemble.

**The server is chunk-size agnostic.** `chunkBytes` is a *hint* returned at
session create; `PUT` accepts any payload size. That keeps the default a tunable
number rather than a contract, and lets `bridge push` pick its own.

**Optional per-chunk integrity** via `Content-Digest: sha-256=:<base64>:`
(RFC 9530 structured field). Validated against the streaming hash before the
offset advances; a mismatch is `400` and the offset does not move, so the client
simply re-sends. Note the older `Digest:` header is deprecated by RFC 9530 —
don't "fix" this backward. Absent the header, the server-computed SHA-256 is
recorded in the manifest and reported at commit.

**Status codes worth pinning:** `409` offset mismatch (carries the real offset),
`400` digest mismatch, `413` declared session bytes exceed `maxSessionBytes`,
`507` free space fell below `minFreeBytes` mid-stream, `415` wrong Content-Type
(the csrfGuard path).

**The `507` body carries `reclaimableBytes`.** That is what lets the UI turn a
dead end into a recovery: intercept the 507 and offer *"Disk full — 2.1 GB in
trash. Empty trash and resume?"* as one action. It is the ergonomic half of
auto-purge without the part that silently destroys the safety net, and it is why
auto-purge is not needed for v1 (see *Resolved*).

**Session state persists** as `manifest.json` beside the parts, so resume survives
a bridge restart.

**Commit is per-file best-effort with an honest report.** Files that landed are
real files; a partial album is incomplete, not corrupt, and the response says
which failed and why. Sequence per file: validate the destination path *fresh* →
`os.MkdirAll` the parent → `atomicwrite.RenameWithRetry` (absorbs the Windows AV
scan-on-close window) → `fsutil.SyncParentDir`. Then remove the session directory,
then trigger **one** scan.

### Delete and trash (PR 5)

| Method | Path | Body | Purpose |
|---|---|---|---|
| `POST` | `/api/library/trash` | JSON | Move paths to trash; returns per-path outcome + bytes trashed |
| `GET` | `/api/library/trash` | — | List trashed entries with original path, size, age |
| `POST` | `/api/library/trash/restore` | JSON | Move entries back; triggers a subtree scan |
| `DELETE` | `/api/library/trash` | — | Purge (all, or a listed subset) — this is what frees space |
| `GET` | `/api/library/space` | — | free / library-used / trash-held, for the sidebar widget |

Trash and purge return the **byte counts they moved or reclaimed**, so the UI can
say what actually happened rather than re-probing and hoping the number moved.

---

## Path safety (write direction)

`fs.Resolver` is read-direction only. The client-supplied relative path needs its
own validator, applied at commit against the resolved root:

- reject `..` segments (raw, before any `Clean`), absolute paths, NUL, control
  characters, and `\` treated as a separator
- Windows: reserved device names (`CON`, `PRN`, `AUX`, `NUL`, `COM1`–`COM9`,
  `LPT1`–`LPT9`, including with extensions), trailing dots and spaces
- per-segment byte cap via `fsutil.TruncateUTF8AtMost` — note the existing
  `fsBasenameCap = 255 - len(sidecarTmpSuffix)`, since a committed file may later
  grow a sidecar
- total path length cap (Windows 260 unless long paths are enabled)
- a depth cap, so a pathological tree can't be constructed
- final containment check against the resolved root — the primary defense on
  Windows, where the raw `..` scan and `path.Clean` are both slash-based and only
  `filepath.Join` collapses a backslash traversal
- case-collision on case-insensitive volumes via `fsutil.IsUnderAny`'s empirical
  probe, never a `runtime.GOOS` check

**Accepted extensions:** `manifest.Ext` (16 audio extensions), plus `.jpg`/`.jpeg`
(local artwork), `.pdf` (booklets), and `.lrc`/`.cue` as inert companions.

The principle: a file that sits *beside* audio in a curated library is accepted;
archives and executables are not. `.lrc` and `.cue` are **accepted but not
consumed** — grep confirms the bridge has no lyrics or cue-sheet handling today —
so they survive a round-trip for other players and for whatever the bridge does
later, and the UI should say so rather than implying they light something up.

`.png` is deliberately excluded: the local-artwork path is JPEG-only by design, so
accepting a PNG cover would silently produce a file that is never used. Rejecting
with a reason is more honest than that.

---

## Resource limits

| Knob | Default | Note |
|---|---|---|
| `upload.maxFileBytes` | 8 GiB | Must clear a SACD ISO (~4.7 GB) |
| `upload.maxSessionFiles` | 2000 | Browser ceiling, not server |
| `upload.minFreeBytes` | 5 GiB | Disk-exhaustion floor, not a quota |
| `upload.sessionTTL` | 24h | Abandoned-session sweeper |
| `library.allowDelete` | **off** | Own gate — never folded into `upload.enabled` |
| `library.trashTTL` | 7d | Purge window; the same sweeper handles it |
| `upload.chunkBytes` | 4 MiB | Hint only — the server accepts any size |

Disk is pre-flighted at session create against the declared total via
`transcode.AvailableDiskSpaceNearest` (walks to the nearest existing ancestor —
the staging dir is created lazily) and re-checked periodically during upload with
a running budget, the `minFreeBytes` shape from auto-optimize. **A probe error
fails closed**: no free-space reading means no way to honour the floor, and a
refused upload costs a retry while a wrong guess fills the volume.

**Quota stays out of the bridge.** `POST /api/upload/sessions` accepts an optional
`maxSessionBytes` that the cloud control plane populates; if the declared total
plus already-staged bytes exceeds it, session creation is refused with `413`. That
is a request-scoped ceiling, not per-root disk accounting — the bridge never tries
to learn what a tenant is "allowed" to have. Mid-stream, dropping below
`minFreeBytes` fails with `507`.

**The sweeper walks the directory, not the session list.** A crash mid-commit, or a
manifest that fails to parse, orphans files that a state-driven sweeper would never
look at. It enumerates `.bridge-upload/*` physically and removes any session
directory that is past TTL *or* has no readable manifest. **Age comes from the
manifest's recorded `createdAt`**, falling back to the directory's own `mtime`
only for the orphan case where no manifest parses — never from a staged file's
`stat`, for the reason D11 spells out. It also **runs once at
startup** before entering its ticker — otherwise a crash leaves orphaned `.part`
files sitting for a full period. Joined to `bgWriters`.

---

## Scan trigger

`spawnBackgroundScan` runs a **full** `Scan`, which is the wrong cost after a
three-album upload. Add `spawnBackgroundSubtreeScan(label, dir)` mirroring the
same `bgScans` WaitGroup contract — never a raw `go func()`, per the standing
invariant — calling `Scanner.ScanSubtree(ctx, dir)`.

**Do not scan the common ancestor.** Files committed to `A/Album` and `Z/Album`
have the library root as their LCA, so an ancestor-based trigger silently
degrades to a full scan on exactly the sessions where a targeted one matters most.

But "one `ScanSubtree` per distinct directory" has its own cliff, and it is worse
than either review noticed: `ScanSubtree`'s tail calls
`restampDuplicatesNonFatal`, and that pass is **whole-library**, not
subtree-scoped. So *N* subtree scans cost *N* whole-library duplicate restamps —
which for even a modest *N* is more work than one full `Scan`, which pays it once.

The rule that handles both, and terminates:

1. Collect the distinct parent directories of the committed files.
2. Drop any directory that is a descendant of another in the set.
3. While the set is larger than `maxSubtreeScans` (default 8), replace every
   member with its parent — **but never above depth 1**, a direct child of the
   root — then re-apply *both* rules from step 2. A discography committed across
   twelve album folders collapses to the one artist folder on the first pass.
4. If the set is still over the cap once everything has bottomed out at depth 1,
   do a single full `Scan` instead.

**The depth-1 floor is load-bearing.** Without it, an upload spanning nine
top-level artist folders collapses all nine to `<root>` in one iteration and
escalates to a full scan — when nine discrete subtree scans were the whole point,
and the collapse threw away the information needed to do them. The floor also
makes step 4 meaningful: the fallback fires on genuine breadth (more than
`maxSubtreeScans` distinct top-level folders), not on an artefact of the loop.

**Re-applying step 2 after every collapse matters too**, and not only to remove
exact duplicates: collapsing can *create* a new ancestor relationship that didn't
exist before. Targets `A/B/C` and `A/X` collapse to `A/B` and `A`, and `A/B` is
now a descendant of `A` — so the descendant-drop has to run again, not just a
uniqueness pass.

**Log the restamp duration on every subtree scan.** `maxSubtreeScans = 8` is a
guess (see *Resolved*), and one logged number per scan turns it into a measured
one without any extra work. If step 4 turns out to fire often, the real fix is a
`ScanSubtree` variant that skips its restamp tail with one `RestampDuplicates`
after the batch — noted so it isn't rediscovered, deliberately not built now.

Catalog
invalidation is free: `postScanNudges` already fires after every successful scan
including `ScanSubtree`, and `startCatalogInvalidator` is lazy, so a bulk import
coalesces into one rebuild. `isScanning` on the existing `stats` SSE event already
surfaces the post-commit scan in the UI — no new SSE event needed for it.

---

## PR breakdown

Five stacked PRs. PRs 1–4 stack because each depends on the previous one's
surface; PR 5 depends only on PR 4's UI shell and could run parallel-off-main if
the delete design needs more soak time than the upload work does.

Merge bottom-up, retarget children to `main` before deleting the parent branch,
and **amend for a fresh SHA after retargeting** — `gate.yml`/`gofmt.yml` are
`pull_request: branches: [main]`, so a stacked child gets only SonarCloud until
it targets `main`, and a base change alone doesn't fire the workflows.

### PR 1 — transport floor (~150 lines)

`csrfGuard` allowlist + the rolling read-deadline helper. No upload logic. Small,
structural, independently reviewable — and it is where the security reasoning
lives, so it deserves its own review rather than being buried in a feature PR.

Pins:
- `TestCSRFGuardAllowlistIsExactlyUploadRoutes` — walks the registered routes; a
  new route that quietly joins the allowlist fails
- `TestCSRFGuardStillRejects{Multipart,FormURLEncoded,TextPlain}OnUploadRoutes`
- `TestCSRFGuardStillRejectsOctetStreamOffUploadRoutes`
- `TestUploadHandlerSurvivesSlowClientPastReadTimeout` — the 30 s pin
- `TestStalledClientStillTimesOut` — the negative control for the above; without
  it the deadline extension is indistinguishable from removing the timeout

### PR 2 — `internal/upload` (~400 lines)

Pure package, no HTTP: staging layout, session manifest persistence, the
path/extension validator, offset bookkeeping, the TTL sweeper.

Pins:
- `TestStagingDirIsInvisibleToScanner` — real temp root, real `Scan`, assert zero
  track rows **and** no folder row. Negative-control by renaming the staging dir
  without the dot.
- `TestPartialFilesNeverCarryAnAudioExtension`
- `TestPathSanitiser` — table: `..`, absolute, NUL, `\`, `CON`, `CON.flac`,
  `trailing.`, `trailing `, a 300-byte segment, depth overflow, a case twin
- `TestCommitIsSameFilesystemRename` — asserts no copy fallback was taken
- `TestSessionResumesAcrossRestart`
- `TestTornChunkTruncatesToManifestOffset` — write past the recorded offset, then
  resume; the tail must be discarded, not appended after. Negative-control by
  deriving the offset from the file size instead of the manifest.
- `TestManifestOffsetNeverPrecedesDurableBytes` — the D10 ordering
- `TestConcurrentChunksToOneFileSerialise` — two parallel `PUT`s at the same
  offset: exactly one wins, the loser gets `409`, and the file is never
  interleaved. Run under `-race`.
- `TestFileLockMapIsReapedOnCompletionAndTeardown` — no entry survives a finished
  or abandoned session. Run under `-race`, which is what catches a delete racing
  an acquire.
- `TestStagingOpenIsNotAppendMode` — write at a seeked offset over existing bytes
  and assert they were overwritten, not appended. Goes red under `O_APPEND`.
- `TestConcurrentChunksToDifferentFilesDoNotSerialise` — the control that keeps
  the lock per-file rather than per-session; it goes red if the lock is widened
- `TestSweeperRemovesOrphansWithNoReadableManifest`
- `TestSweeperRunsOnceAtStartup`

Consider a fuzz target for the sanitiser — it is an untrusted-input surface of
exactly the shape the existing 29 targets cover, and `FuzzResolveContainment`'s
asymmetric property (a success must land inside a root) transfers directly.

### PR 3 — admin endpoints + commit + scan (~350 lines)

Six routes, `spawnBackgroundSubtreeScan`, disk pre-flight, wiring in `cmd/bridge`.

Pins:
- `TestResumeReportsStagedOffsets`
- `TestOffsetMismatchReturns409WithActualOffset`
- `TestCollisionSkipsByDefaultAndOverwritesOnlyWhenAsked`
- `TestDiskFloorFailsClosedOnProbeError`
- `TestCommitFansOutToDistinctDirsNotTheCommonAncestor` — commit to `A/x` and
  `Z/y`; assert two subtree scans, not one scan of the root
- `TestSubtreeScanSetCollapsesToParentPastCap`
- `TestNineTopLevelFoldersDoNotCollapseToRoot` — the depth-1 floor; goes red on an
  unbounded walk-up
- `TestCollapseReappliesDescendantDrop` — targets `A/B/C` and `A/X`, which collapse
  to `A/B` and `A`; the set must end as just `A`
- `TestSubtreeScanFallsBackToFullScanOnGenuineBreadth`
- `TestDeclaredBytesOverSessionCapReturns413`
- `TestFreeSpaceExhaustionMidStreamReturns507`
- `TestContentDigestMismatchDoesNotAdvanceOffset`
- `TestSessionCreateFlagsExistingBasenameSizeMatches` — the D9 pre-flight
- `TestPreflightHintDoesNotSkipAutomatically` — the control: a flagged file the
  client still sends must upload normally
- `TestPartialCommitReportsPerFileFailures`
- `TestUploadRoutesAreNotInAuthBypassList`

### PR 4 — UI, config, docs (~400 lines)

Drop target and file/folder picker on the Library page's Roots tab (which already
owns roots and the transcoded cache), a feature tray toggle beside it per the
per-page tray pattern, `upload.*` config with `Enabled` default off, the field
report row, `ops/settings-apply-semantics.md`, and a CLAUDE.md invariants entry.

Pins:
- `TestUploadDisabledByDefault`
- `TestSpaceWidgetHiddenWithoutAFloorOrQuota` — the progressive-disclosure rule
- `TestDestinationPreviewIncludesTheSelectedFolderSegment` — pins the
  `webkitRelativePath` behaviour D9 is about, so a future "helpfully" stripped
  leading segment fails loudly
- `TestUploadFieldReportsLive` (and the tray-badge parity tests pick it up
  automatically)
- `TestEveryFeatureTrayFieldExistsOnBothSettingsStructs` already covers the tray
  row — a typo'd field name saves nothing while reporting 200

### PR 5 — delete as trash (~350 lines)

The five trash routes, `library.allowDelete` (default off) and `library.trashTTL`,
trash purging folded into the existing sweeper, and the folder-view affordance.

Reviewed on its own rather than folded into PR 4, because it is the first
destructive operation the bridge has ever exposed and the review should be about
that, not about a widget.

Pins:
- `TestNoLibraryFileIsRemovedOutsideTrash` — the invariant, asserted structurally:
  the delete path must produce a rename into `.bridge-trash`, never an unlink
- `TestTrashScopingIsByteRangedNotLIKE` — the case-twin fixture from
  `store_prefix_case_test.go`; skips on a case-insensitive volume
- `TestPreConfirmCountUsesTheSameBoundsAsTheDelete` — the divergence that made
  the original bug invisible. Negative-control by making the count case-exact.
- `TestTrashedRowsRetireImmediatelyWithTombstones` — threshold-1, not three scans
- `TestTrashRejectsDotSegments` — `.bridge-upload` / `.bridge-trash` unreachable
- `TestTrashTTLReadsTimestampDirNotFileMtime` — trash a file stamped years ago and
  assert it survives. Negative-control with a `stat`-based sweeper, which purges
  it immediately.
- `TestRestoreRecreatesMissingParentDirs` — trash an album's last track, remove the
  now-empty directory, restore
- `TestPurgeReclaimsAndReportsBytes`
- `Test507CarriesReclaimableBytes`
- `TestDeleteRefusedWhenAllowDeleteIsOff` — and the control that
  `upload.enabled` alone does **not** turn it on
- `TestDeletingDupeWinnerUnsuppressesTwinOnNextRestamp`

**PROTOCOL.md is untouched by all five PRs.** Admin-only, no `/v1`, no
`ProtocolVersion` bump, no iOS mirror.

---

## Follow-on, not in this stack

**`bridge push`.** `bridge push --library ~/Music --to https://tenant.example`
against the same session API: resumable, parallel, content-hashed, no browser
memory ceiling. This is what a real library should arrive through, and it reuses
PRs 1–3 verbatim — the only new surface is a write-scoped credential, since the
admin session cookie is a browser artifact.

Both review passes converged on the same shape for it: an admin-minted long-lived
token carrying a `write:library` scope, sent as `Authorization: Bearer`, accepted
on `/api/upload/*` and the trash routes and **explicitly rejected on `/v1`**.
Three things to get right when it is built:

- **It must not live in `auth.Store`.** That is the paired-*device* token store
  for `/v1`; a CLI credential landing there becomes a device token with read
  access to the whole library. Separate store, or a scope column plus a rejection
  at every `/v1` route.
- **Hashed at rest**, as `auth.Store` already does, with a distinguishable prefix
  so secret scanners and log redaction can spot one.
- **Refused in demo mode**, per the `refuseUpscaleMutationInDemoMode` precedent.

**Move / reorganise in the folder view.** Genuinely motivated — it is the remedy
for the D9 mess, where someone ends up with `Dark Side of the Moon/` loose at the
root and wants it under `Pink Floyd/`. Deferred because two costs hide in it:

1. **A moved track is a new path, so it is a new row with `enriched_at = 0`** —
   a full re-enrichment at MusicBrainz pacing, plus an iOS delta of deletes and
   adds. `caseOnlyRenames` detects case-only renames; a genuine move is not one.
2. **Variant sidecars mirror the source path**
   (`<OutputDir>/Artist/Album/Track.flac.<variantID>.flac`, test-pinned), so a
   move orphans every variant unless the files are relocated *and*
   `track_variants.source_path` is re-keyed.

So it is either simple-and-costly (accept re-enrichment plus variant
regeneration) or correct-and-complex (a `Store.MoveTracksByPrefix` carrying rows
and sidecars in one transaction, which is also the shape that would finally give
the scanner real rename detection). That is a design decision, not a detail —
it deserves its own plan rather than being smuggled into this one.

**Bring your own bucket.** Orthogonal to all of the above and probably higher
leverage for the cloud product. Worth its own plan.

---

## Resolved (first two review passes, 2026-08-29)

1. **Staging inside the root** — settled, both passes agreed. `upload.stagingDir`
   survives as an operator override only.
2. **Chunk size** — **4 MiB**, and the point is mostly moot because the server is
   chunk-agnostic (see **Wire shape**). The two passes split 8 vs 4; 4 wins on the
   arithmetic. On a relay at ~200 ms RTT, one round trip per chunk is negligible
   against the ~3 s a 4 MiB chunk takes at 10 Mbps, so RTT amortisation buys
   almost nothing above ~1 MiB — while re-send cost after a drop scales linearly,
   and a lossy relay is precisely where drops happen. Tunable, and worth measuring
   rather than arguing.
3. **Checksums** — optional, computed server-side on the fly, with `Content-Digest`
   accepted when a client supplies one. Both passes agreed.
4. **Quota** — `maxSessionBytes` supplied per-session by the control plane, `413`
   on refusal; `minFreeBytes` / `507` for disk exhaustion. This satisfies both
   passes: it is a request-scoped ceiling the caller supplies, not per-root
   accounting the bridge maintains.

## Resolved (third pass)

5. **`maxSubtreeScans` = 8** — keep it hardcoded for v1; both passes agreed. Their
   supporting numbers disagreed with each other and neither was measured, so
   nothing about *why* 8 is right is written into this plan as fact. What ships
   instead is the instrumentation: log the restamp duration on every subtree scan,
   so v2 tunes it from data rather than from another round of assertions.
6. **Auto-purge trash** — no, both passes agreed, and a better answer replaces it:
   `reclaimableBytes` on the `507` plus an "empty trash and resume" action (see
   **Wire shape**). Manual control, automatic ergonomics.
7. **Write-scoped credential** — both passes converged on an admin-minted
   `write:library` bearer rejected on `/v1`. Shape recorded under **Follow-on**;
   still built with the CLI, not before.

## Still open

Nothing blocking. The one number left to earn is `maxSubtreeScans`, and item 5
is how it gets earned.
