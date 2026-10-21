---
paths:
  - "internal/admin/**"
  - "internal/upload/**"
  - "internal/trash/**"
  - "cmd/bridge/status*.go"
---
# Admin console and the web player

The console's pages, static JavaScript and CSS, its JSON API and its guards, the web player, uploads and the trash.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Admin console and the web player

- **A cross-origin admin refusal must reach the operator as the server's
  typed message, never a bare "403 Forbidden".** `csrfGuard` (and the SSE
  Origin gate) answer with JSON `error: cross_origin` via
  `writeCrossOriginRefusal`; `errorFromResponse` prefers `j.message` and
  exposes `e.code`. In loopback mode that message names the admin
  listener's host and port — never the page URL's — and an equal-port SSH
  tunnel example (`[::1]` when bound there; empty/unspecified host falls
  back to `127.0.0.1`). **Don't widen `originMatchesLoopbackMode` to admit
  another local port** (B193 / B471); fix the tunnel instead.
- **The cert tiles grade and PHRASE from one number.** #952 gave all three
  one ladder (`certExpiryBadge`, which returns `expired` for `left <= 0`)
  and left each tile's wording alone — wording written against the
  PREVIOUS ladder, which had no expired arm — so two of them clamped with
  `Math.max(0, …)` and an expired certificate rendered `expired · expires
  in 0 days`. The self-signed tile reached the same place from the other
  side, displaying the `daysUntilExpiry` sentinel `Inspect` forces
  NEGATIVE past NotAfter as `(-40 days)`. `certValidityText` is the
  sibling of that one ladder and derives from the same signed `left`.
  **"Today" is a CALENDAR question**, not a 24-hour one: a duration test
  called an expiry at 00:30 tomorrow "expires today" beside a date saying
  otherwise. `now` comes from the caller's own `left` rather than a second
  clock read, so the badge and the phrase cannot drift and a test can
  drive any instant. The Go suite cannot run it, so the guard is
  structural — it requires the calendar comparison to be present. (#962)
- **…and so is the COUNT after it** (#971). #962 gave "today" the calendar
  and left the days on `Math.round(Math.abs(left)/86_400_000)`. All three
  tiles print that beside `toLocaleDateString()`, and a rounded duration
  disagrees with the date in BOTH directions: 47 h from 00:30 read "in 2
  days" beside TOMORROW, 25.5 h from 23:30 read "in 1 day" beside the day
  after. `Date.UTC` over the local Y/M/D is DST-safe for the reason the
  same-day test is, and the `Math.max(1, …)` clamp the duration form needed
  becomes unreachable — keeping it would only suggest the arithmetic can
  produce a zero. `left` carries the TENSE, never the count, and the
  structural guard now says so: it rejects any expression dividing `left` by
  a day, because requiring the Y/M/D getters to appear SOMEWHERE was
  satisfied by `Math.round` and passed throughout.


- **The catalog is computed, not stored.** Album identity is
  `dupes.AlbumIDOf(dupes.Resolve(row))` — the same value the iOS client computes
  — so the browser's partition equals the phone's by construction. **Don't add
  album/artist columns "for speed"**: genres and composers are multi-value axes
  with fold rules SQL can't express, so the Go pass is required either way.
  Invalidation is LAZY (an epoch bump; the next reader rebuilds) — that IS the
  debounce, because an eager rebuild re-folds the library on every watcher event
  during a bulk import.
- **An album is a SET of tracks, never a path prefix.** `FolderPath` is the
  common directory, and on a real library ~8% of albums share one — so a prefix
  submit enqueues every neighbour and a prefix delete reclaims their sidecars. A
  single track is the mirror image: the subtree pattern matches strict
  DESCENDANTS, so a file path projects zero rows. Identity scopes travel as IDs
  and are expanded server-side.
- **A present-but-EMPTY scope must never read as an ABSENT one** — absence means
  the folder form, and an empty folder path means everything. `{"albumIds": []}`
  would upscale the whole library.
- **The browser MIME table is NOT the DLNA one** (`audio/x-flac` is right for
  hardware renderers and unplayable in browsers). Playability reports FACTS
  (`universal` / `engine-dependent` / `none`), never a verdict — `canPlayType`
  answers `""` for codecs an engine can actually decode.
- **`//go:embed static/[^.]*` skips `.`/`_`-prefixed names inside matched
  subdirectories** — a `_util.js` compiles, embeds nothing, and 404s only in a
  release build. Don't "fix" it with `all:static` (that ships `.DS_Store`), and
  don't take the glob back to `static/*`, whose `*` matched a top-level dot
  name (the embed bullet under **Build, CI, and test discipline**, #1006).
  `/static/` must force Content-Type + `nosniff` (module scripts are MIME-checked
  and hard-fail; Windows serves `.js` as `text/plain` from the registry) and
  `no-cache` (a `?v=` busts only the entry module — relative import specifiers
  don't inherit the query).
- **Partial-boost works only because the `<audio>` element and now-playing bar
  are parented to `<body>`, outside `<main>`.** Every page's init registers
  document/window listeners scoped to an `AbortController` aborted before the
  next page's init; `boostSwap` claims a generation up front and discards a
  superseded response; the post-render scroll restore is generation-guarded
  (the stale offset lands LAST, measured, not first). `PLAYER_HEADS`,
  `playerRoutes` and boot.js's route table are a three-way parity contract, each
  pair pinned.
- **`boot();` must be the last statement in `boot.js`** — it reaches most of the
  module, so a call near the top puts every later `const`/`let` in the temporal
  dead zone. This emptied the whole player twice with no failing test and no
  symptom but a console `ReferenceError`.
- **A settings control that renders but isn't in the PATCH allowlist saves
  nothing while the page still says "Saved."** — worse than not offering it. The
  same class covers apply-semantics stated in three places (badge, hint prose,
  server report); each surface is walked from its own side, because a test that
  walks one proves nothing about the others.
- **`.small` had no rule in either stylesheet for months**, and `.hint.warn` /
  `.error` were inert class combinations — everything asking to recede rendered
  at body size. A test connects emitted classes to rules and records which are
  BORROWED from app.css. **Strip CSS comments before scanning**: this repo's
  commentary names the classes it discusses.
- **player.css must not style operator classes**, and a bare class rule there
  hijacks anything app.css qualifies with an element (a bare `.rows` collapsed
  every operator table).
- **Deleting scattered CSS or JS needs a selector-set / declaration-aware diff**,
  not a brace or docblock scan. A comment containing a literal `{` swallowed the
  global `[hidden]` rule; a cut ending at "the next `function`" swallowed five
  unrelated functions; and `/*` inside a `//` JS comment opened a fake block
  comment that hid 46 KB of app.js from a guard test.
- **Dynamically-composed class names (`class="status-${x}"`) are not dead
  because no literal exists.** Check composition before deleting.
- **`/api/jobs` is guarded in BOTH directions, down to the LEAF.**
  `TestSettingsPrereqsOnlyReadRealJobsFields` walks JS→Go (every `jobs.<field>`
  names a real field) and cannot see a field nobody reads;
  `TestEveryJobsFieldIsRenderedSomewhere` walks Go→JS. #891's stated purpose was
  "a Jobs card" and it touched no template and no JS — the endpoint ran a
  full-table scan every thirty seconds for numbers no pixel consumed, and the DTO
  test passed. **The first version of that guard walked the twelve CONTAINERS
  and was satisfied by `j.lyrics` alone** — so "`lyrics` was the only field with
  zero reads" was true at the only level it looked, and the nine numbers inside
  were as unguarded as `lyrics` had been. Walked recursively it found six more
  leaves marshalled for nobody (`scanner.isScanning`, `scanner.lastFullScan`,
  `enrichment.source` — all read off `/api/stats` instead — plus
  `analysis.intervalSec`, `coverage.totalLocal`, `smartMixes.analysisAssisted`).
  Two mechanisms make the recursive form work, and both are negative-controlled:
  it resolves the JS's local and parameter aliases (`const lyr = j.lyrics`;
  `renderAnalysisCoverage(an.coverage)` → `cov.eligible`), and it strips
  COMMENTS ONLY — most nested reads sit inside `${…}` template interpolations,
  which `stripJSNoise` blanks. Recursion stops at the exported shared types
  (`*JobRunState` and kin), which have other consumers. **A guard that checks
  containers proves nothing about their contents.** One of those stops is
  `*AnalysisSweepState`, and a field added to its `last` breakdown and never
  rendered leaves this guard green (measured on #992), so that breakdown is
  pinned the other way: `TestDescribeAnalysisSweepAccountsForEveryTrack`
  executes the line that renders it. **A leaf the server sends as a KEY
  for the console to word needs its wording pinned too**: the guard proves
  `maintenance.orphanSidecarGCRefusal` is read, not that every value it
  can take has words, so `TestEveryOrphanRefusalKindIsWorded` runs
  `describeOrphanGCRefusal` under node for each
  `integrity.OrphanRefusalKinds()` entry (2026-09-28), and refuses one that
  comes back blank, `undefined` or `null` (what `console.log` prints for a
  case that returns nothing), as its key, as the fallback, or in another
  kind's words. **And a list that stands for every value is pinned to the
  declarations**: `TestEveryOrphanRefusalKindIsListed` reads the integrity
  package's source for every constant of type `OrphanRefusalKind` and
  requires `OrphanRefusalKinds()` to hold exactly those, since a kind the
  list omits is worded by nobody and the wording test passes over it.
  The variant watcher's key (`maintenance.variantIntegrityRefusal`,
  2026-09-29) has the same pair, `TestEveryVariantRefusalKindIsWorded` and
  `TestEveryVariantRefusalKindIsListed`, over the same helpers
  (`requireEveryRefusalKindIsWorded`, `requireEveryKindIsListed`): **a
  third refusing sweep takes both helpers, not copies of them.**
- **`/api/stats` is guarded in both directions too, and there "read" means the
  console OR `bridge status`.** Unlike `/api/jobs` this payload has a SECOND
  consumer — `cmd/bridge/status.go` decodes it into a `map[string]any` and
  prints ten fields — so an app.js-only sweep reports seven false positives, and
  a missing KEY there is nil, formats to `""`, and silently drops a line from
  `bridge status` (`TestBridgeStatusOnlyReadsRealStatsFields`, by AST: the
  subject is a string literal, which this repo's comment strippers blank). **A
  `bridge status --json` dump does NOT count as a read** — it passes the whole
  decoded map through verbatim, so it would exonerate every field, which is the
  same as having no guard. Four had no reader at all: `tracksUnreadable` (the
  one query in `readStatsDBPart` run for a single field — a SELECT per snapshot
  for nobody, #947's own addition, now the dashboard's alarm row, hidden at zero
  and pointing at the Jobs list built from the same predicate), plus three
  duplicates now GONE rather than exempted — `startedAt` (`uptimeSec` on the
  same payload), `dbBytes` (an `os.Stat` per snapshot for what
  `/api/diagnostics` serves) and `upnpRoutedTracks` (the COUNT is earned,
  `trackSourceCounts` hands it to `/api/sources`; the scalar was
  `sources.routedTotal` one payload over). **No exemption list, on purpose** —
  it is the frictionless way to put a fifth one back, which is the failure the
  jobs guard exists for. `statsResponse` is FLAT, so the recursion question does
  not arise, and that is ASSERTED rather than assumed so a future NON-SCALAR
  field forces the decision — an allowlist of scalar kinds plus `time.Time`,
  not a list of the shapes to reject, because a map or a slice marshals nested
  leaves exactly as a struct does and `reads` counts every PREFIX of a path.
  The JS root is scoped to ONE function: `applyStats`'s
  parameter is `s` and app.js has seven one-argument functions whose parameter
  is `s`, so an unscoped root is a false PASS. (#948)
- **An SSE list handler needs an explicit empty-list teardown branch.** A restart
  wipes the in-memory pairing store, so the next snapshot is `[]`, and
  `applyPairing([])` must clear the optimistic-action latch and hide the panel —
  otherwise a stale latch entry is inherited by whatever new request lands in the
  same id slot. The snapshot must marshal as `[]`, not `null`, for the array
  check to reach that branch.
- **The SSE stream byte-diff-suppresses per CONNECTION**, so a freshly-injected
  tile gets nothing until a value changes — recycle the EventSource after a swap
  to force a full snapshot. Anything monotonic (uptime) must be zeroed in the
  SSE DTO (`statsResponse` has a dedicated SSE wrapper for exactly this) or it
  poisons the diff every tick — and never add a server-computed `elapsedSec`. Expensive snapshots go behind a TTL
  + singleflight and never on a fast tick; a busy/idle gate needs a latch to fire
  one final frame.
- **Refresh on the pool's DONE+FAILED counters, not a busy→idle edge** — a short
  batch starts and finishes between two frames, so the edge never fires.
- **A back link is navigation and does not belong in an action row**; a heading
  takes slack with an auto margin, not a `justify-content` flip (a page-scoped
  `.panel-head` selector silently outranks a two-class `:has()`).
- **`.sr-only` is `position: absolute`** — inside a horizontally-scrolling
  container it escapes the clip and extends the document's scroll width.
- **Icon presentation lives on the `<use>` host, not the sprite's source `<g>`**
  (a CSS rule matching the original element does not cross the shadow boundary;
  inheritance does), and `viewBox` is an HTML attribute, not a CSS property.
- **A badge inside a link must carry no `aria-label`** — accname replaces the
  subtree with it, dropping the live count the badge exists to say.
- **Whole-library destructive actions live behind a typed exact phrase** (never
  prefix or case-fold), re-checked in the handler rather than trusting
  `disabled`.
- **`WriteTimeout` stays UNSET on the admin server** (long synchronous endpoints
  and SSE); the upload path rolls the READ deadline forward on elapsed time, not
  bytes — a byte threshold starves the slow client it exists to protect.
- **Staging and trash live INSIDE the target root as dot-directories**, skipped
  before the walker upserts anything, which is what makes commit and delete
  same-filesystem renames. Staging under `dataDir` is a cross-device copy
  wherever the library is a separate mount — the normal case.
- **The durable upload offset is the meta record, never the staged file's
  size**; ordering is bytes → fsync → offset → fsync, and every open truncates
  back to the recorded offset. Locks are refcounted per `(session, file)`; a
  session-wide lock passes every locking test while quietly serialising a folder
  upload.
- **An upload session is aged from its last accepted chunk** (2026-10-09,
  backlog B213). `fileState.UpdatedAt` is stamped on the `.meta` the chunk
  already rewrites; the manifest stays immutable. The idle window is
  `SessionTTL` (default 24h). An absolute cap of seven days, or the
  configured TTL when that is longer, still sweeps a session that keeps
  receiving bytes: uncommitted staged bytes do not outlive the library trash
  window, and a longer idle window the operator set is honoured. A meta
  written before the field existed falls back to that meta file's mtime, then
  to `CreatedAt`. An orphan whose manifest does not parse stays on the
  directory mtime. Trash stays on the stamp directory name. The recorded
  field outranks a touched or back-dated meta mtime, and an idempotent
  resend of a finished file does not move the activity. An empty write to
  a file that is still short does not refresh it either: `io.Copy` of an
  empty reader returns 0 and no error, and that chunk leaves `.meta`
  alone after the digest check and the sync. A zero-length file's first
  empty chunk records the empty digest, which is what completes it.
  (`TestSweepKeepsASessionThatKeepsReceivingChunks`,
  `TestSweepRemovesASessionIdlePastTheTTL`,
  `TestSweepRemovesASessionPastTheAgeCapWhileItIsStillActive`,
  `TestSweepCapFollowsALongerIdleTTL`,
  `TestSweepReadsMetaMtimeWhenNoActivityWasRecorded`,
  `TestSweepTrustsRecordedActivityOverTheMetaMtime`,
  `TestSweepDoesNotRefreshActivityOnAnIdempotentResend`,
  `TestSweepRemovesASessionThatReceivedOnlyEmptyChunks`,
  `TestAZeroLengthFileCompletesOnItsFirstEmptyChunk`.)
- **A resumed upload matches a version of the file** (2026-10-09, backlog
  B176). The console matched a stopped session to a new pick by path and
  size, and sent no whole-file digest, so the check in `WriteChunk`
  (`fd.Digest != ""`) never ran. A file retagged in place, same size,
  kept the staged prefix and took the new bytes for the rest. Each
  chunk's `Content-Digest` matched the bytes in that request, the running
  hash was of the splice, and with nothing declared the commit reported
  `committed`. The bridge then serves that file bit-exact. The session
  records, at create, the client's `File.lastModified` in unix
  milliseconds (`modified`) and a fingerprint, `hex(sha256(head))`
  concatenated with `hex(sha256(tail))` over a 64 KiB window at each end
  (`fingerprint`, 128 hex characters). `sessionKey` is path, size,
  modified and fingerprint, and it returns null when any entry lacks a
  positive modified and a 128-character fingerprint, so a session written
  before those fields, or a pick the browser could not identify, starts
  over. The fingerprint is a 128 KiB read. The owner declined an up-front
  whole-file hash (about 82 MB/s in the page, about 13 s per GB): a new
  session declares no `digest`. A client that does declare one still gets
  the completion check (`digest_mismatch`), and chunk `Content-Digest`
  is unchanged. `modified` (`File.lastModified`) is a separate identity
  field `sessionKey` checks, so an mtime change starts a new session on
  its own. The fingerprint covers the head and the tail: a FLAC tag at
  the head, a DSF or WAV tag at the tail, and an M4A `moov` at either
  end. The residual is a change only in the middle, with the size and
  the mtime preserved: that pick resumes, and with no declared digest the
  splice commits. (`TestASameSizeEditDoesNotResume`,
  `TestUploadSHA256MatchesNode`,
  `TestANewSessionReadsOnlyTheFingerprint`,
  `TestCreateKeepsTheResumeIdentity`,
  `TestASplicedUploadWithADeclaredDigestIsNotCommitted`,
  `TestUploadSessionListsTheResumeIdentity`.)
- **Committed files are 0644** (the staged mode survives the rename), and trash
  age comes from the `<stamp>` DIRECTORY NAME — `os.Rename` preserves mtime, so
  an mtime-driven sweeper purges oldest-content-first the instant it lands.
- **Deleting takes an explicit path list, never a prefix** — that sidesteps the
  case-fold class of a PREFIX (a `LIKE` scope reaching a case-twin folder)
  rather than getting it right. It did not sidestep a path in the list
  spelled otherwise than its row, which a case- or normalization-insensitive
  volume moves all the same: this bullet said "entirely" until backlog B219,
  whose delete of `artist/ALBUM/01.flac` retired no row and rescanned the
  folder a second time. The trash spells each path as its folders list it
  first (the B219 bullet under **Scanner**).
- **A present empty trash-id list purges nothing** (2026-10-09, backlog B180).
  `Purge(nil)` and a bodyless `DELETE /api/library/trash` still empty the
  trash — that is the console's "Empty trash". `{"ids":[]}` is a present
  scope, the same rule as `{"albumIds":[]}`: `Purge` returns
  `ErrInvalidPath` ("no entries given") and the handler answers 400. The
  gate is inside `Purge`, so a handler check in front of it would leave a
  reverted `Purge` green. `{"ids":null}` and `{}` decode to nil and stay
  purge-all. (`TestAnEmptyIDListDoesNotEmptyTheTrash`,
  `TestPurgeOfAnEmptySliceRefuses`, `TestPurgeWithNoIDsEmptiesEverything`,
  `TestPurgeIsWhatActuallyReclaims`.)
- **A library path is never trimmed** (2026-10-09, backlog B178).
  `validRel` and `splitID` refuse a path or id that `strings.TrimSpace`
  would change, and `ValidateRelPath` refuses the same shape, so an upload
  cannot create a file the trash API cannot name. The refusal is for the
  whole trash request, before any move: one padded path answers 400 and
  leaves every file where it was. `validRel` removes one leading `/` and
  then checks, so `"/ Various/Album/01.flac"` is the padded name
  `" Various/..."`. Checking before the strip let it through, Trash moved
  that file, and `List`'s id failed `splitID`, so Restore and `Purge(nil)`
  could not name it. `ValidateRelPath` refuses a leading `/` as absolute
  and does not strip one, so its padding check stays on the raw string.
  A space inside a segment (`Artist/ Album/01.flac`) is that segment's
  own name. Upload can create it (a segment that ends in a space or a dot
  is what Windows strips, and a leading space inside a segment is kept),
  and trash must still be able to name it. (`TestAPaddedPathDoesNotTrashTheUnpaddedFile`,
  `TestALeadingSlashThenASpaceIsRefused`, `TestValidateRelPath`.)
- **A destination that cannot be stated is not "nothing there"**
  (2026-10-09, backlog B179). `commitOne` (when not overwriting) and
  `Restore` proceed only on `fs.ErrNotExist`. Any other stat error fails
  that file, with a reason that names the error and not the absolute path.
  A directory at the path stays "already exists". `Restore` takes the
  commit's destination lock (`LockDestination` / `WithDestinationLock`,
  bound in `runServe` after the upload manager exists) and releases it
  before the next entry — a `defer` inside the loop would hold every
  earlier path until `Restore` returns. (`TestACommitWhoseExistenceCheckHitsEIOLeavesTheFile`,
  `TestARestoreWhoseExistenceCheckHitsEIOLeavesTheFile`,
  `TestARestoreAndACommitToOnePathTakeTheSameDestinationLock`,
  `TestUploadReclaimableIsWiredInProduction`.)
- **A restore puts the file back under the root it was trashed from**
  (2026-10-09, backlog B177). `Restore` mapped the stored manifest path
  through `SplitRoot` of the roots configured now. That path is
  `<basename>/…` with more than one root and the path within the root
  with one, so after a collapse `Music/Artist/Album/01.flac` landed at
  `<surviving>/Music/Artist/Album/01.flac` and still reported restored,
  and after a second root was added a file that lived in a top folder
  named like the root landed one level too high. Trash writes a sidecar
  beside the audio file, `.<name>.bridge-root`, holding the absolute
  root `SplitRoot` returned and the path relative to that root. Restore
  builds the manifest path for the root count in force now from that
  pair and takes the destination from `Resolver.Resolve`. A recorded
  root that `EvalSymlinksOrClean` does not match to a configured root
  answers `ErrRootUnavailable` ("the library root this path belongs to
  is not configured") and the file stays in the trash; a renamed root
  path is that refusal. A record that does not parse, or whose root and
  suffix do not resolve back to themselves, is refused as unreadable.
  An entry with no sidecar was written before the root was recorded.
  When its first segment equals the basename of the root the file sits
  under, restore refuses: that segment is either the old multi-root
  prefix or a real folder of that name, and the entry does not say
  which. The same refusal covers a single-root library whose top folder
  is named exactly like the root. When the first segment equals another
  configured root's basename, the older "different library root" refusal
  stays. Otherwise the whole stored path is the path within the root,
  and it is restored under the root the file sits under, through the
  resolver, in the current layout. List still shows a refused entry.
  The cost is that a trash written before this change under more than
  one root, and one whose path begins with its root's name, is put back
  by hand from `.bridge-trash`. The sidecar's name begins with a dot, so
  List and the sweep's byte count skip it, and a successful restore or
  purge removes it before the empty stamp is pruned. The sidecar path
  that is written or removed is the trash directory joined with the
  relative remainder of the sidecar, and a remainder that leaves the
  trash directory is refused. A recorded root that is not an absolute
  path is an unreadable record. A trash whose record cannot be written
  attempts to move the file back. When that move succeeds, the empty
  stamp directory is removed and the entry fails with the record reason.
  When the move back fails, the file stays in `.bridge-trash`, a warning
  is logged, and the entry fails with a reason that says it is still in
  the trash. That result still carries the spelled path and the folder,
  as a completed trash does, so the handler retires the row and rescans;
  a spelling that could not be read is a full scan. A file that came back
  records neither.
  (`TestRestoreAfterARootCollapsePutsTheFileBackUnderTheSurvivingRoot`,
  `TestRestoreAfterARootIsAddedKeepsAFolderNamedLikeTheRoot`,
  `TestRestoreThroughTheAPIFollowsTheRootTheFileWasTrashedUnder`,
  `TestRestoreRefusesAnEntryWhoseRecordedRootIsGone`,
  `TestRestoreRefusesAnEntryRecordedUnderADifferentRoot`,
  `TestAnOldEntryWhosePathStartsWithTheRootNameIsRefused`,
  `TestAnOldEntryThatNamesNoRootBasenameRestoresUnderTheRootItSitsUnder`,
  `TestATornRootRecordIsRefused`,
  `TestAFailedRootRecordMovesTheFileBackAndRemovesTheStamp`,
  `TestAFailedRootRecordWhoseMoveBackFailsLeavesTheFileInTheTrash`,
  `TestAFileLeftInTheTrashRetiresItsRowAndRescansItsFolder`,
  `TestAnOriginRecordOutsideTheTrashIsNotWritten`,
  `TestAnOriginRecordOutsideTheTrashIsNotRemoved`,
  `TestARecordedRootThatIsNotAbsoluteCannotBeRead`.)

- **A CSS grid with no `grid-template-columns` sizes its track to the WIDEST
  item's max-content, and no Go guard can see the result.** `.deleted-list`
  copied `.unresolved-list`'s `display: grid; gap: 2px` and one long playlist
  name resolved the implicit `auto` column to **925 px inside a 317 px panel**
  at 375 px: rows overflowed, every action button left the viewport, and the
  list grew a horizontal scrollbar. `min-width: 0` on the child does NOT fix
  it — the TRACK is what grew; `minmax(0, 1fr)` on the list plus `min-width: 0`
  on the grid ITEM is what lets an ellipsis apply.
  `TestPlayerEmittedClassesAreStyled` was green throughout, because the class
  HAD a rule: the markup was correct and the page rendered wrong, which is the
  exact failure that test describes and cannot detect. **Drive the real console
  at 375 px with a deliberately long fixture string**, and check
  `scrollWidth == clientWidth` rather than eyeballing it. (2026-09-20)

- **A control the server will refuse must not look live, and the panel
  reads the refusal's own predicate** (2026-09-28). With upscaling on and
  `upscale.optimizeEnabled` off, "Generate CarPlay" stayed enabled on the
  album and artist Variants panels and a click answered 503
  `optimize-disabled`: #1060 made the console's submit refuse the kind, and
  the summary the panel reads carried no switch. The summary now carries
  `optimizeActive`, which is `Server.optimizeActive`, the ONE predicate the
  batch submit, the projection endpoint and the summary read (nil reads as
  on, as `Deps.OptimizeActive` always has), so the button is disabled
  exactly where the submit refuses.
  `TestTheVariantSummaryCarriesTheSwitchTheSubmitReads` compares the served
  value with a real submit for every state of the switch, and pins that
  submit BOTH ways (a 202 that reached the coordinator once, or the
  switch's own 503 that reached it never): read as merely "not
  `optimize-disabled`", a submit failing some other way passed as an
  accepted one (CodeRabbit on #1068). And
  `TestTheVariantPanelDisablesGenerateCarPlayWhereTheSubmitRefusesIt` runs
  the SHIPPED panel under node on the summary the album detail serves,
  kind by kind against the submit, with the switches moved through the
  settings PATCH. **Don't serve the configured `optimizeEnabled` instead**:
  it agrees with the submit only while the gate is wired from the same
  config, and a control that served it went red on the summary test alone.
  The reason sits in the kind's own row with a gear for that one switch (a
  block that stops both kinds stays one note above them), and the panel
  tests `=== false`: an unknown leaves the button live and lets the
  endpoint answer, the folder view's rule, since `/api/library/browse`
  carries no feature state. The #1060 AST sweep counts a call of
  `optimizeActive` as reading the CarPlay switch. A save from either tray
  redraws the panel: the next bullet. **The panel-wide note must close
  wherever the upscale gate does**, because `Deps.OptimizeActive` includes
  that gate (the flag and a usable sox: `carPlayOptimizeActiveFn`, under The
  CLI and the serve wiring), and a block the panel-wide note does not
  show falls through to the CarPlay row's "switched off". The summary's
  `soxAvailable` is therefore `Server.soxUsable`, the gate's sox half
  (found, and FLAC when the build's formats are known): with the precheck
  alone a sox without FLAC read as available, and the panel named a switch
  that was on while the hi-res Generate stayed live over a refusal
  (`TestTheVariantSummaryReadsSoxAsTheGateDoes`, and the panel test's
  fourth state).
- **A tray redraws the page only through its spec's `onSaved`, and a page
  redraws only for a switch it draws from** (2026-09-28, CodeRabbit on
  #1068). A feature tray saves one switch and repaints nothing else, so
  after the CarPlay kind's tray said "Saved." the row beside it still said
  "switched off" over a disabled button until the next render.
  `saveTrayField` calls `onSaved(field)` once for a save the server
  answered `live` (with a reason or without), after the snapshot, the
  status line and every other tray have the new value, and outside the
  `try`, so a callback that throws cannot turn a save that landed into
  "Save failed". Never for `restart` or `unchanged`: nothing on the page
  moved, and a redraw that took the tray would take the restart
  instruction in it with it. The variant panel's trays call its own
  redraw (the bullet on the panel's in-place redraw, below): the CarPlay
  kind's tray for its one switch, and the panel-wide tray for
  `upscaleEnabled`, and for `optimizeEnabled` once generation is on:
  while it is off nothing the panel draws depends on the CarPlay switch,
  so a redraw for it would fetch and repaint the same panel for nothing,
  the reason Generate does not redraw either. **Don't make a tray redraw
  by itself, or a page redraw for every field.**
  `TestATrayCallsOnSavedOnlyAfterASaveTheServerAppliedLive` runs the
  shipped `buildFeatureTray` and save under node, and
  `TestAVariantTraySaveRedrawsThePanelWhereTheSwitchChangesIt` the shipped
  panel on the summaries the album detail serves.
- **…and a page that redraws after a save reads what it draws from the
  server, never the page seed** (2026-09-28, backlog B35). The Smart mixes
  page drew its off state from `seed.mixesEnabled`, read once per page load,
  and handed its tray no `onSaved`, so after its own gear saved the switch
  on the page said "Smart mixes are off" beside "Saved.", and the player's
  own navigation back to it said it again, until a reload (seen in a
  browser). The seed is read once per page load and neither a save nor the
  player's navigation reloads it, so **nothing a gear on a player page can
  save belongs in it**. `GET /api/player/mixes` answers `enabled` per
  request, and `managed` when the control plane owns the switch, so the off
  state points at the gear only where the gear can turn it on. The page
  redraws for `smartPlaylistsEnabled` alone, and IN PLACE: toolbar and tray
  are built once per route, the redraw repaints the view and the bar ahead
  of the gear, and is refused once the route has moved on, so the tray and
  its "Saved." survive, as the variant panel's do since 2026-09-29 (its
  bullet, below). **The player also drops the
  trays' shared settings snapshot on every route**
  (`BridgeFeatureTray.invalidate`, from boot.js's `route()`), the drop an
  operator page gets from its page init, which the player never runs: a
  player tray showed a switch as it was at page load, beside a view that
  now reads the server fresh.
  `TestTheSmartMixesPageRedrawsInPlaceAfterItsGearSavesTheSwitch` runs the
  shipped view under node; `TestThePlayerRouteDropsTheTraySnapshot` pins
  the drop structurally, since `route()` cannot run without booting the
  player. **A drop also discards the answer to a request made before it**
  (CodeRabbit on #1088): `invalidateTraySettings` nulled the promise and
  left the request running, so its answer still became the snapshot, over
  a newer answer or in place of the one the new page waited for, and its
  failure dropped the newer request. In a browser, with the older answer
  held back: the Jobs page's Smart mixes switch rendered off at the next
  tray sync while the server held on. `traySettingsSnapshot` caches an
  answer, and drops a failed request, only while its request is still
  `traySettingsPromise`
  (`TestADroppedTraySnapshotIsNotCachedWhenItsAnswerArrives`).
- **…and a tray save gives focus back to its switch** (2026-09-28). A save
  disables its switch while the PATCH is out, and a browser moves focus off
  a focused control that becomes disabled (the focus fixup rule) and does
  not give it back when the control is enabled again (measured on Chrome
  152). So every tray save left a keyboard user's focus on the body,
  whatever the answer and whether or not the page redrew: on the Smart
  mixes page the focus was gone before the in-place redraw ran, and
  equally for the Audio analysis switch beside it, whose save redraws
  nothing. `saveTrayField` notes whether the switch had focus before it
  disables it and gives focus back once the save is over, unless something
  else took focus meanwhile, and never takes focus the switch did not have.
  `TestATraySaveGivesFocusBackToItsSwitch` runs the shipped save under node
  with the fixup rule modelled in the harness. **Every other control the
  console disables while a request is out lost focus the same way** (the
  Jobs page's `wireJobButton` buttons among them, measured on "Scan now"),
  until 2026-09-29: the next bullet is the rule for all of them.
- **Every control that disables itself around a request goes through
  `setDisabled`, which gives focus back** (2026-09-29, backlog B68). The
  Jobs page's "Scan now" read `document.activeElement` as the body while its
  handler had it disabled, and still 4.5 s later once it was enabled again
  (Chrome 152), so a keyboard or screen-reader user lost their place after
  every "Scan now", "Retry missing" and the like; `saveTrayField` had given
  focus back for a tray's switch alone. `setDisabled(control, disabled)` in
  app.js is that rule for the 36 sites that disabled a control by a literal
  (app.js's 29 and the player's 7), and the enables that ended them:
  disabling notes on the control (`dataset.refocus`) whether it
  had focus, and enabling gives focus back to a control that had it, unless
  something else took focus meanwhile (`document.body` or nothing has it),
  never to a control that did not (Safari focuses no button on a click), and
  to nothing a redraw replaced. The note is on the control so the enabling
  call, a `finally` or a 4 s timer away, needs no variable carried to it,
  and `saveTrayField` no longer holds a copy of the rule. **The player
  reaches it through `window.BridgeControls`** (app.js is a classic script
  and the modules cannot import it, the `BridgeFeatureTray` handshake), with
  `ui.js`'s `setDisabled` as the forwarding wrapper whose fallback is a plain
  assignment: one implementation, and a page without app.js only loses the
  focus. **Never write `.disabled = true` or `= false`, or the attribute
  forms, outside the helper**: `TestNoConsoleControlDisablesItselfOutsideSetDisabled`
  sweeps app.js and every player module for them (a value computed from state,
  `gen.disabled = !actionable`, is not a request in flight, and the variant
  panel routes even those through the helper because an in-place repaint can
  disable a control the reader is on). `TestSetDisabledGivesFocusBackToAControlThatHadIt`
  and `TestAJobButtonGivesFocusBackAfterItsRequest` run the shipped helper and
  `wireJobButton` under node with the fixup rule modelled in the DOM.
  **Not covered: a control that VANISHES on success** (the fingerprint Enable
  button hides once the switch is on, a Delete re-renders the route), which
  has no control left to give focus to.
- **A tray offers no switch the control plane owns** (2026-09-28, backlog
  B35). Trays ignored `deployment.managedSettings`, so on a managed bridge
  they offered switches the settings PATCH refuses whole: the album page's
  Variants gear offered PCM upscaling and CarPlay, and a click answered
  "Save failed: these settings are managed by the control plane…", and
  the Jobs page's Backups and Update checks gears offered nothing else
  (seen in a browser). `applyTrayManaged` HIDES a managed row and disables
  its input, as `hideManagedSettings` hides the field and the library page
  leaves out the roots form, and hides the gear once no field row and no
  note row is left (a note is written for the reader whatever the
  switches). **Hidden, not greyed**: a greyed switch on a hosted bridge
  reads as something to earn. A change dispatched to a managed row anyway
  sends nothing (`saveTrayField`'s guard). The managed set is the
  snapshot's EFFECTIVE `managedSettings` and outlives a snapshot drop, so a
  tray built after the first snapshot leaves the row out from its first
  paint; on a fresh page load it is unknown until the tray's settings
  fetch lands (2 ms on loopback), and every row shows disabled meanwhile,
  the Settings page's own window. `TestATrayOffersNoSwitchTheControlPlaneOwns`
  runs the shipped tray under node. **Two controls outside the trays
  offered a managed field** until 2026-09-29 (the Jobs page's fingerprint
  Enable button, the Duplicates page's policy radios): the next bullet.
- **A control on the page that offers a managed field is hidden like a
  tray's row, and one that only offers a change waits to know** (2026-09-29,
  backlog B68). On a public-mode bridge whose `deployment.managedSettings`
  listed `fingerprintEnabled` and `duplicatesFilter`, the Jobs page offered
  the fingerprint Enable button and its click answered "Enable failed —
  retry", and the Duplicates page offered the policy radios and a click
  answered an alert, "these settings are managed by the control plane…":
  B35's trays on a control beside them. There are exactly four console
  callers of `PATCH /api/settings` (the Settings form, which hides managed
  fields and drops unoffered keys, the trays, and these two) and the PATCH
  is the only handler that reads the managed set. Both read the trays'
  snapshot (`trayManaged`, kept across page inits). **The Enable button
  waits for it**: `syncFingerprintEnable` shows it only for a switch that is
  off, a managed set that is known and does not name the field
  (`trayManagedKnown`; a card's button on a managed bridge must never
  flash, which a tray's rows in a closed gear can afford), and `initJobs`
  runs it again the moment the snapshot lands, not at the next 10 s poll.
  **The policy radios stay until the set says otherwise**, since they also
  say which policy is in force (`applyDupesPolicyManaged`, first from the
  set an earlier page's snapshot left, so a boosted navigation never shows
  them): the fieldset and the prose about saving one are hidden, the inputs
  disabled, and a line says which policy is set for the bridge, named as its
  radio names it (`dupesPolicyName`). Both handlers send nothing for a
  managed field (`enableFingerprint`, `saveDupesPolicy`), as `saveTrayField`
  does. **A tray blurb that counts switches is written from the rows the
  reader can see**: `spec.blurb` may be a function of the shown fields,
  which `applyTrayManaged` calls whenever the managed set may have changed,
  and the CarPlay pre-generation tray's "All three switches have to be on
  for anything to run" says how many are set for the bridge instead of
  standing beside the two rows a hosted bridge leaves it (`carPlayBlurb`;
  the DSD row is not one of the three).
  `TestTheFingerprintEnableButtonIsOnlyOfferedWhereTheOperatorOwnsTheSwitch`,
  `TestTheDuplicatesPolicyIsOnlyOfferedWhereTheOperatorOwnsIt` and
  `TestTheCarPlayTrayBlurbCountsOnlySwitchesTheReaderCanSee` run the shipped
  functions under node.
- **The variant panel redraws IN PLACE after a tray save** (2026-09-29,
  backlog B68). A save in the panel's gear called `onChanged`, the whole
  route's re-render: after turning PCM upscaling on from the Variants tab,
  `document.activeElement` was `#player-title`, the panel had no gear left
  and the tray's "Saved." was gone (Chrome 152). The panel now takes
  `refresh` (a fresh `variants` block: the album view and the artist view
  fetch their detail again) and `alive` (`gen() === at`, whether the route
  is still the one it was drawn for), and repaints around its gears.
  **Every node the panel owns is built once** and `paint` updates text,
  disabled states and attributes on them; `reconcile` adds and removes only
  the nodes a state needs and **never moves one that is already in place,
  because detaching a node takes the focus out of its subtree**, and the
  reader is in the tray's switch. The gears stay once built (a note, and the
  gear beside it, are `switchNote`; the note's text follows the state and
  the gear outlives it), so the panel's tray answers for the CarPlay switch
  too once generation is on, and a redraw for that field is skipped while
  generation is off. Concurrent redraws end on the newest one's answer
  (`seq`), and a FAILURE a newer redraw has overtaken is left to it, since
  handing over to the route there would re-render it away under the newer
  redraw (CodeRabbit on #1094); an answer after the route moved on, an
  aborted fetch, and an answer with no summary paint nothing; the newest
  redraw's failed fetch hands over to `onChanged`, whose re-render fetches
  again and shows its own error state; a panel given no `refresh` (the
  folder view has no trays) calls `onChanged` as it always did, and an
  `alive` that is not a function reads as no route check. **Not redrawn in place, still whole-route:
  Delete (the numbers are true when it answers, and the track marks beside
  the panel change with them) and the live refresh
  (`onVariantChange`, every 8 s while a batch runs).**
  `TestAVariantTraySaveRedrawsThePanelInPlace` runs the shipped panel on a
  DOM that models the focus rules (a `reconcile` that rebuilds its children
  is red on the focus lines alone), and
  `TestTheAlbumAndArtistViewsLetTheirVariantPanelRedrawInPlace` the shipped
  views: a panel that can redraw and is never handed the fetch falls back to
  the re-render without a word.
- **A job card says why a switched-on job is inactive in a note of its
  own, never over its description, and never asks for a restart a live
  gate does not need** (2026-09-28, backlog B45). The analysis and
  fingerprint cards wrote "Enabled but inactive: … Restart after fixing."
  over their description, false since #781 made both gates live, and
  nothing wrote the description back: in a browser, with sox taken off the
  running bridge's PATH and put back, the analysis card read "active" with
  "Analyze now" beside that sentence until a reload. `showJobDegraded` is
  the one note for the three cards whose gate probes a tool (analysis,
  fingerprinting, CarPlay pre-generation), hidden again once the card reads
  active. The fingerprint surfaces said "restart" three more times (the
  Enable button latched "Enabled — restart to apply" for good, its hint,
  the tray's "degrades to off at startup"), and the Settings chip beside
  the switch read `enabled` as running, so it said "active" beside a card
  that said degraded: #1067's upscale-chip defect, one switch over. **A
  chip or badge reporting a gate reads the gate (`active`), never the
  switch.** `TestAJobCardSaysWhyItIsInactiveBesideItsDescriptionAndClearsWhenActive`
  and `TestTheFingerprintChipReadsTheGateNotTheSwitch` run the shipped
  `renderJobCards` and `renderSettingsPrereqs` under node.
- **…and a card's LIVE counters follow the gate its badge reads, never
  the pool's existence** (2026-09-29, backlog B113). The analysis pool is
  built on every bridge (#781), and `GET /api/analysis/stats` (the SSE
  `analysis` event, which paints the Jobs card's Queue line) set `pool`
  whenever `Deps.AnalysisPoolStats` was wired, which serve does on every
  bridge: with analysis off the card read "off" beside "0 queued · 0 in
  flight · 4 done · 0 failed (6 workers)" (measured on a real serve after
  four analyses and a switch-off), where the upscale tile leaves its pool
  out by the live gate. `getAnalysisStatsSnapshot` sets `pool` only while
  `enabled`, from the SAME read of `AnalysisActive`, so the two cannot
  disagree within a snapshot (the closure answers whatever the gate
  says); the line reads "—", as the upscale tile's live fields do, and the
  sweeper's lines stay, being the last run's (a pass the gate refuses
  records nothing). **Gate on the live gate, never the flag**: a flag
  switched on over a missing sox is the degraded card, and a handler that
  read `cfg.Analysis.Enabled` put the counters back there
  (`TestTheAnalysisPoolLineFollowsTheGate` moves the two apart;
  `TestServeAnalysisPoolLineFollowsTheLiveGate` boots serve both ways).
  **Residual: a job already RUNNING when the gate closed finishes**, and
  neither tile shows it: one per worker, seconds each (a DSD render
  minutes). A job still QUEUED is dropped since backlog B155 (the
  switch-off bullet under The CLI and the serve wiring); until then a pool
  drained its whole queue whatever the gate said (measured: 37 analyses in
  114 s after a switch-off, each an `indexed_at` bump), which this tile
  hid. **A comment that explains a gate by a pool, a
  sweeper or a card being ABSENT with the feature off describes the code
  before #770 and #781**: the /v1/health gating comment said no
  `upscale.complete` event can arrive because a disabled bridge has no
  pool and no `SetOnJobComplete` (both exist on every bridge; the flag
  goes with the feature because no job can be asked for, and a job
  already rendering at the switch-off still publishes one), `WithUpscaleEnqueuer` said
  it was wired "IFF" the flag and sox, and some thirty more said the same
  in the stats docblocks, the auto-optimize card, its PATCH reason ("the
  upscale pool is absent"), the fingerprint cache and a Settings hint
  ("the Jobs card stays hidden"). `api.AnalysisStats` carried a `pool`
  that nothing ever set and PROTOCOL.md says is not there; it went (no
  byte changed). **Every pool, sweeper and closure beside them exists on
  every bridge, and only the live gates move**: a test that "disables" a
  feature by wiring nothing tests a bridge serve never runs.
  `TestHealthOmitsUpscaleCompleteEventsWhenUpscaleDisabled` wired no
  `WithUpscale` and passed with the flag advertised on the gate's WIRING
  rather than its answer; it wires a gate answering false now and flips it.
- **…and a card says when its next sweep is due only while its gate is
  open** (2026-09-29, backlog B156). Every sweeper loop runs on every
  bridge whatever its gate says (#781), and `runSweepLoop` arms the next
  pass from the interval alone, so with the features at their defaults the
  analysis, fingerprint and CarPlay cards read "off" beside "Next sweep:
  in 5h", and the smart mixes card, switched off, "Next run: in 23h" (seen
  in a browser on a real serve), each for a pass its gate refuses:
  `runSweepLoop`'s own dormant branch clears the time for that reason.
  `nextSweepWhileOpen` (handlers_jobs.go) leaves a gated card's
  `nextDueAt` out while the card's own gate is closed, read once for the
  snapshot (the analysis, fingerprint and CarPlay `active`, the smart
  mixes' switch), in `/api/jobs` and in the SSE `analysis` frame, which
  paints the same analysis line; the console renders the absence as "—".
  **The gate, never the switch**: a card switched on over a missing tool
  is closed too, since its next pass runs only if the tool is back by
  then, and it turns active, time and all, within a minute of that. The
  recorder keeps the time; only the payload leaves it out. **A card added
  with a next-run field is classified, or the build fails**:
  `TestEveryNextRunOnTheJobsCardsIsClassified` walks the snapshot type for
  every `next*` field and requires `nextSweepPaths` to say whether a closed
  gate withholds it (the scanner's, the backups' and the duplicates' are
  ungated, each with its reason).
  `TestAJobsCardSendsItsNextSweepOnlyWhileItsGateIsOpen` moves each switch
  apart from its gate, and `TestTheJobsCardsSayNoNextSweepWhileTheirGateIsClosed`
  runs the shipped renderJobCards and applyAnalysisStats under node over
  the payloads served.
- **A gate on a query parameter reads the PARSED predicate, never the
  parameter's presence.** The player sends `needs=all` on every default
  grid load (its default is the literal `all`, and `qs()` drops only the
  empty string), and `parseVariantFilter` maps `all` to no filter — so
  #913's variant-free coverage skip, gated on `q.Get("needs") != ""`,
  never ran for the live grid on exactly the libraries it was built for,
  and its test passed because it sent no query at all. Parse first, gate on
  `pred != nil`, and **test with the query the client actually sends**. An
  active filter with no snapshot is REFUSED (503 `coverage_unavailable`),
  never dropped: `filterAlbums` served the unfiltered library under a
  plausible total beneath a comment calling that the safe direction. (#915)
- **An `<a class="btn">` inside running text needs `display: inline-block`.**
  An inline box's vertical padding does not grow its line box, so the
  pill's padding spills over the line above — the post-upload "Browse your
  library" link overlapped its sentence by 4.4 px with the UA underline
  showing through. Every other `.btn` is a `<button>` or sits in a flex
  row, which is why only this one did it. Measured in a browser. (#920)
- **Below 1024px, a rail child that is not inside `.sidebar-drawer` is in
  the TOP BAR.** `header.sidebar` is a flex ROW there, and the space meter
  was added as a plain rail child: on a public bridge with uploads enabled
  the bar at 375px held "42 GB free", the live dot, the theme pill and
  Sign out beside the 44px hamburger, so the head shrank to 57px, the mark
  sat under the button and the library name rendered at 0px of its 70.
  The meter's gate is `upload.enabled || upload.minFreeBytes > 0`, NOT the
  deployment mode — which is why the demo tenant, same binary, looked
  fine. The nav, the meter and the foot block now share one wrapper that
  the breakpoint hides and reveals; it is `display: contents` on the
  desktop rail, so that column is unchanged (measured identical against a
  main build), and `TestEverythingBelowTheBrandIsInTheDrawer` pins the
  header to exactly two element children — put a new rail element inside
  the wrapper unless it has a case for the bar. The live dot stays in the
  bar as a SECOND `.conn-badge` inside the brand — dot-only there, hidden
  on the desktop rail — because the foot's `#conn-status` is inside the
  hidden drawer and its `aria-live` region would announce nothing on a
  phone; `applyConnState` updates every `.conn-badge`, so each viewport
  shows exactly one live region. (The first form was a `:has()`-coloured
  pseudo-element; CodeRabbit caught the silent live region.) Verified in a
  browser at 375 / 430 / 1280, dark and light, with and without the
  meter; the Go suite sees only the containment. (#934)

- **A playlist's M3U8 export can write library paths (backlog B339, question 5; the owner's decision of 2026-10-18).** `GET /api/playlists/export?…&format=m3u8` writes each local item's absolute path on this host, as before; `&paths=relative` writes its manifest path instead, which the iOS app's own M3U8 export writes for a bridge source: relative to the library's root, the root's folder name first in a multi-root library, so there the file resolves only beside the root folders (the button's tooltip says so). A path that starts with `#` or whitespace is written `./`-prefixed (`m3uRelativeLocation`, the app's `locationLine` rule), an item that no longer resolves stays an `# unresolved:` comment in both modes, and any other `paths` value is a 400. The playlist page's export row has a fourth button for it. **Don't change the default (a player on this host opens the absolute form), don't write an unresolvable item as a location, and don't let a relative line start with `#` or whitespace.** Locked by `TestM3U8ExportRelativePaths`, `TestM3U8ExportRelativePathsEscapeWhatAnImporterWouldMisread` and `TestM3URelativeLocation`.
- **The dashboard's player checklist panel shows `GET /api/output/checklist` and is hidden unless the bridge is a player; its Phone line opens the Devices page's own pairing dialog** (B473 B2c). The dialog is one template, `layout.html`'s `pair-dialog`, filled by `pairDialogData` and rendered by `devices.html` and `dashboard.html`, with one opener in app.js (`initPairModal`), which is the one place the console mints a pairing token. The panel loads with the page and on Check again, never on a poll or the SSE tick, and paints with `textContent`. The checklist's lines and words are `internal/outputcheck`'s (output.md). **Don't copy the dialog or its submit into a second place, and don't poll the checklist.** `TestTheDashboardCarriesThePlayerChecklistAndThePairingQR`, `TestThePlayerChecklistScriptIsWired`, `TestOutputChecklistIsInactiveOffAPlayer` and `TestOutputChecklistPassesTheLinesThrough` pin it; the script is pinned structurally, so a node-run test of `renderPlayerChecklist` is still open.
