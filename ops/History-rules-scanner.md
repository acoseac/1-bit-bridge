# Long form of `.claude/rules/scanner.md`

> Moved verbatim from `.claude/rules/scanner.md` on 2026-10-18 so it stops loading with every matching file. **These entries are still binding.** The rules file keeps each one as a single line (its bold title plus its own "Don't…" sentences, ending `→ History-rules-scanner.md`); this file keeps the reasoning, measurements, rejected alternatives and the full don't-undo list. Change an entry here and its line in the rules file in the same commit. Each `##` below names the rules-file section the entries came from; entries inside a section keep their original order.

## Scanner, deletion passes, and manifest writes

- **…and a read that did not complete is the same case: only a COMPLETED read
  may answer "not an SACD"** (2026-09-28). `processSACDISO` retires every
  virtual row under an `.iso` container at threshold 1, journaled (a tombstone
  to every paired device), whenever the expansion answers `(nil, nil)`, and
  three reads in `sacd.go` answered exactly that when they FAILED: the
  master-signature probe and the DST probe dropped their error (`n, _ :=`),
  and `parseSACDArea` folded a failed area-TOC read into `ok=false`. An EIO,
  ETIMEDOUT or ESTALE from a NAS deleted the album while the file sat on
  disk, on any scan that missed the skip gate (a size or mtime change, every
  `ExtractorVersion` bump). `sacdReadOutcome` is the rule: a read is full when
  it returned the bytes the parse needs, judged BEFORE its error
  (`io.ReaderAt` permits `(len(p), io.EOF)`); a short read ending in `io.EOF`
  or `io.ErrUnexpectedEOF` is the end of the file, structural, so a truncated
  image still answers `(nil, nil)`; any other short read, a nil error
  included, is an error, and an error retires nothing. Each phase keeps its
  FIRST failure and returns it only if it ends with nothing found, so one bad
  copy still expands from the next (the doubled-TOC design); `parseSACDArea`
  returns `(area, ok, err)`, err only for a read. The iOS reader makes the
  same split (transport errors throw, past-EOF reads come back short). **A
  container written in place reads as one that ends early, which IS a
  completed read**, so the scanner also skips, retiring and writing nothing,
  a container that changed during the scan (`expandSACDContainer`): the
  handle's stat before the first read against a stat of the path after it
  (`os.SameFile`, size, mtime), and the walk's stat against that same STAT
  (size, mtime), since the walk's stat of a linked container is its TARGET's
  (the next bullet). **Never `os.SameFile` against the walk's stat**: on
  Windows a directory entry carries no file index on FAT or exFAT, so every
  container there would skip, forever. **Never compare the walk's stat with an
  LSTAT**: for a link the walk's stat is the target's, so every symlinked
  container would read as moved (`TestScanner_SACDSymlinkedContainer_Expands`).
  This bullet said the opposite, lstat, until 2026-09-28, when the walk's stat
  of a link was the link's own; that pairing could not see a write to a
  symlinked container's target before the open, and the album was retired
  (`TestScanner_ALinkedSACDContainerWrittenAfterTheWalkKeepsItsRows`).
  Residual: an in-place overwrite that keeps size and inode inside one coarse
  mtime tick (FAT's 2 s), and a link repointed after the walk at a file with
  the first one's size and mtime. **No `ExtractorVersion` bump for this**:
  readable files expand
  byte-identically, a wrongly retired container has no representative row so
  the gate re-expands it anyway, and a bump re-upserts every virtual row (that
  leg has no diff-guard). `TestScanner_SACDReadFailure_RetiresNothing` (a
  case per site) and `TestScanner_SACDContainerChangingDuringTheScan_KeepsItsRows`
  (a case per arm) drive it through a per-scanner opener seam
  (`Scanner.openSACD`), and
  `TestScanner_SACDReadWholeAsJunk_StillRetiresWithTombstones` is the positive
  control: a container read whole as junk still retires, with tombstones.
  **`FuzzSACDExpandUnderAReadFault` puts the rule under the fuzzer** (the other
  SACD targets' reader never fails): an image expanded through one failing
  read must answer what it answers fault-free, or an error. It holds only for
  an image carrying ONE answer, so the harness builds it with identical TOC
  copies, one stereo area and one geometry, and truncates it only BETWEEN
  structures (`sacdFaultCuts`): an image whose copies differ can legitimately
  expand from the second, and a later copy cut short is one that differs
  (measured: cut inside the second area copy's TTxt sector, it expands to
  "Track 1" where the first says "T1"). A reader that reports the end of the
  file early is a truncation nobody can tell apart, so only failures that say
  so are injected; `TestSACDFaultPropertySeesWhatTheHarnessKeepsOut` shows
  the property reporting both. Its seeds fault each of the three reads above,
  and they are what gives the fuzzer its reach: Go's mutator walks an integer
  by at most 100, one argument per step, so with the DST probe's failure
  dropped again it found the violation within half a second from the other
  seeds (on the first harness and on the final one), and not in 90 s
  (468,005 inputs) from one seed whose fault touched no read.

- **…and a container the walk SEES keeps its virtual rows' missing count
  at 0, whatever the scan writes for it** (2026-09-29, backlog B217). A
  virtual row (`<container>/st/NN.dff`) is in no walk: the deletion pass
  counts it seen whenever its container is (the container-seen branch),
  and never counts it missing then, but nothing reset its count, since the
  skip gate's early return for an unchanged container wrote nothing. So
  "missing on `threshold` consecutive scans" was "missing on `threshold`
  scans, ever": measured by the pre-v0.2.1 review at threshold 3, a
  container hidden for one scan on three separate occasions had its rows'
  count go 1, 1, 2, 2, and the third hide deleted them, a tombstone each
  to every paired device, while the `.iso` was back on disk the scan after
  (which re-created them unenriched); a plain file beside it went back to
  0 each time. Since v0.2.0. `keepSACDRowsSeen` resets the count of every
  row under the container at each exit of processSACDISO that writes no
  row, as the skip gate and `keepUnread` do for a plain file: the skip
  gate's, a read that did not complete (which still retires nothing) and a
  container that changed during the scan (still left for the next one). A
  re-expansion's rows are reset by the upserts that write them, and a
  container read whole as junk still retires its rows, journaled. The
  reset is `ResetTrackMissingCountsUnder`: a byte range, the trailing
  slash trimmed, an error on an empty prefix, never LIKE (a case-twin
  container keeps its count). **A subtree scan resets them too**, through
  the same exits. No `ExtractorVersion` bump and no wire change
  (`TestScanner_SACDRowsCountOnlyConsecutiveMisses`,
  `TestScanner_SACDRowsAreSeenWhereTheScanWritesNone`,
  `TestResetTrackMissingCountsUnderIsAByteRange`).

- **An SACD image's virtual rows carry the cover beside the image, and the
  container's skip gate watches that cover** (2026-10-14, backlog B146).
  `processSACDISO` expanded and never ran the folder-art lookup, so a
  `cover.jpg` beside the `.iso` left every virtual row with an empty
  `artworkMBID`. The container has no row of its own. The representative
  first virtual row is where the folder-art key lives, and the skip gate
  returned on size, mtime and extractor version before any cover was read,
  so a cover added after the image was indexed never reached the rows.
  `stampSACDFolderArt` runs the same lookup a folder's tracks use
  (`applyFolderArtwork`, the folder half of `extractLocalArtwork`) against
  the image's directory. A virtual path's directory is the area folder,
  which holds no cover, so the key is taken from the image path. Every
  virtual row records the same `local-` hash and the same key. On an
  unchanged image, `sacdCoverDrifted` asks `folderArtDrifted` and
  `needsLocalArtworkRecovery` of that representative row. A drift restamps
  without `expandSACD`. Artwork that changed is written whole
  (`UpsertTrackBatch` moves `indexed_at` and resets `enriched_at`). A key
  that moved while the artwork did not is `stampFolderArtKeys`
  (`folder_art_key` and `missing_count` only). `StampExtractorVersionBatch`
  would delete a local lyrics row: the expansion carries none, and
  `GetTrack` does not load lyrics. A removed local cover is dropped the
  way `mergePostScanFields` drops one; a non-local id is kept. An unsettled
  cover keeps the art it had and records the unsettled key, so the next
  scan retries the cover and does not re-expand. An empty artwork cache
  records no key, so the column stays empty and a later cache still sees
  the drift. The not-looked mark would make `folderArtDrifted` answer
  false for good. **No `ExtractorVersion` bump**: a coverless image keys
  to empty, the same key existing rows hold, so those containers stay on
  the size and mtime skip. An image whose folder already holds a cover
  restamps once, without re-expanding. The phone's album tile is the
  first track of the album with a non-empty `artworkMBID` (`artworkVersion`
  when the row has one, otherwise the id). The same hash on every virtual
  row is the cover that tile shows.
  (`TestAnSACDImageCarriesTheCoverBesideIt`,
  `TestAnSACDCoverChangeReachesTheRowsWithoutExpanding`,
  `TestAnSACDImageWithNoCoverScansAsBefore`,
  `TestAnSACDCoverThatCouldNotBeReadKeepsItsArtAndIsRetried`,
  `TestAnSACDCoverWhoseCacheIsGoneIsRestoredWithoutExpanding`.)

- **An `.iso` that is not an SACD is probed once per process, not once
  per scan** (2026-10-14, backlog B157). It expands to nothing, so it has
  no row for the skip gate to key on, and every scan opened it, ran
  `TrackPathsUnder` and probed its master TOC again (measured: three full
  scans of a zero-filled `dvd.iso`, three opens through `Scanner.openSACD`).
  A completed "nothing to expand" probe is recorded in `Scanner.notSACD`,
  by library-relative path, with the size and mtime it read, and
  `processSACDISO` returns before any open while both still match. **Only
  a completed probe records**: a read fault and a container that changed
  under the read return before the verdict (B134's line), and so does a
  stale-row retire that could not list or delete, or those rows would wait
  for the process to end. A verdict is dropped when the image expands.
  Kept in memory, where no manifest reader, retirement pass or deletion
  pass sees it, rather than in the store: the Scanner lives as long as
  `serve`, so a bridge probes each such image once per process, and
  `ExtractorVersion` is compiled in, so a reader that learns a new layout
  is a new process with an empty map. Residuals, accepted: an image a
  symlink is retargeted to with the same size and mtime as the one it
  replaced keeps the old verdict until the process ends, and the verdicts
  of images renamed or deleted stay in the map until then (one entry per
  `.iso` path seen). **Don't record a probe that did not
  complete, don't key the verdict on the path alone, and don't move it
  into `tracks`**, where it would reach the manifest
  (`TestScanner_AnISOThatIsNotAnSACDIsProbedOnce`,
  `TestScanner_ANotAnSACDVerdictFollowsTheFile`,
  `TestScanner_AnISOWhoseProbeFailedIsProbedAgain`).

- **A route asked for a virtual track path answers 404 `not_found`, because
  `ResolveChecked` counts a stat's ENOTDIR as not found** (2026-10-16,
  backlog B414). The stat of `<image>.iso/st/NN.dff` goes through the image,
  a regular file, and fails with ENOTDIR on unix, which `os.ErrNotExist`
  does not cover, so `/v1/read`, `/v1/download`, `/v1/stat` and the area's
  `/v1/list` answered 500 from v0.2.0 on, against PROTOCOL.md and an older
  app's missing-track affordance. On Windows the stat answers
  `ERROR_PATH_NOT_FOUND` (measured on the CI leg), which is `syscall.ENOTDIR`
  there and already not-exist, so Windows bridges gave the 404 all along
  (`TestAStatThroughARegularFileFailsWithENOTDIR`). **Never widen the answer past
  ENOTDIR** (an EACCES, EIO or stale handle could not look, and stays a
  500), **and never special-case the virtual grammar in a route**, which
  leaves every other path through a file at 500
  (`TestResolveCheckedThroughARegularFileIsNotFound`,
  `TestResolveCheckedKeepsAStatThatCouldNotLookAnError`,
  `TestAnSACDVirtualPathAnswers404NotFound`).

- **A file the walk reaches through a link is indexed under its TARGET's
  stat** (2026-09-28). `filepath.WalkDir` hands an entry its lstat, so a
  symlinked audio file was indexed under the LINK's size (the length of the
  path it stores: in one test run 123 bytes against a 116-byte FLAC, and 134
  against a 1,228,800-byte `.iso`) and the link's mtime, while its tags came
  from the target and every endpoint serves the target's bytes. The skip
  gate compared the same
  link stat, so a retagged target was never re-extracted (and a re-authored
  linked `.iso` never re-expanded); `/v1/lyrics` answered 410 `lyrics_stale`
  forever for its embedded lyrics (the row's stat against the resolver's
  `os.Stat`); and the phone, which stores the size as `Track.fileSize`, fails
  every offline download of the file (`validateDownloadedSize` wants an exact
  match), re-downloads it forever through the auto-cache, and cannot read a
  linked SACD container at all (its reads clamp to the container size).
  `walkedFileInfo` is the one decision, in `walkRoot` and `ScanSubtree` alike:
  a REGULAR file keeps its own stat and pays no second syscall; anything else
  is stat'ed through (the listing's "not a regular file" test,
  `resolveEntryInfo`, because a Windows junction is `ModeIrregular` with no
  `ModeDir`; a cloud placeholder stats to itself). **A link whose target
  cannot be stat'ed spares its own row, keyed on the entry itself, and mints
  none**: "could not see" dominates, so a mount that went away keeps its rows
  as they were, and the old walk's row minted from the path alone (which the
  skip gate then kept once the target came back) is gone. Keyed on the entry,
  not its directory, so a permanently dangling link spares nothing beside it.
  One Warn per scan names them (`links whose target could not be read`),
  never one per link: a mount takes every link into it at once. **A link to a
  DIRECTORY is not a track**, whatever its name, and the walk still follows no
  directory link (loops; what it says about one is the B74 bullet below).
  **Nor is a named pipe, a socket or a device, or a
  link to one** (`fsutil.NotAFile`, the list every byte route refuses by too:
  the next bullet): whatever stat the row would carry must describe
  something that opens as a file, and a regular file whose own stat says
  otherwise is judged through. A worker opens what the walk hands it, and
  opening a FIFO waits for a writer with nothing to cancel the wait, so a FIFO
  named `01.flac` held a worker, and the scan with it, forever (measured still
  running at 10 s, and at 15 s with its context expired at 5 s; an `.iso`
  FIFO blocked the next scan too, since no row ever let the skip gate pass
  it). By reading: `Scan` holds the scanner's mutex for its whole run, so
  every later scan waits on it, and the stuck `IsScanning` stands down the
  Atlas lyrics sweep, the booklet GC and the duplicate restamp and makes a
  compaction answer 409. When a writer did come, the row was replaced by one
  minted from the path (title "01", 0 bytes), and a link to a device or a
  socket was indexed that way outright. **The refusal is a list of kinds,
  never "is a regular file"**: a Windows cloud placeholder is
  `ModeIrregular` after `os.Stat` and opens, and hydrates, as a file, so a
  OneDrive library with files on demand would vanish. **A row at such a path
  is reaped like a deleted file's**, after the usual missing-count grace, a
  directory link's included: the walk stat'ed the entry and knows what is
  there. That is the dangling link's answer reversed on purpose: a stat that
  FAILED is "could not see", a stat that answered is a fact. One Warn per
  scan names them with the example's kind (`audio-named entries that are not
  files`), and a row the old walk minted for one is reaped the same way.
  **No `ExtractorVersion` bump**: the stored stat of
  each linked row no longer matches, so the first scan after the change
  re-extracts exactly those rows, once, on the full upsert leg (one delta row
  to every paired device, and a re-enrichment), and nothing else moves
  (`TestScanner_TheFirstScanAfterTheFixRewritesOnlyTheLinkedRows`). PROTOCOL.md
  needed no change: it already said a listing row describes the target, and
  a virtual row carries the container's size. A library ROOT that is itself
  a link is not this rule's shape: that is the `fsutil.WalkableRoot` bullet
  below (this one said "still open" until the same day).

- **…and a route that serves a file's bytes opens it with
  `fsutil.OpenAsFile`, never `os.Open`** (2026-09-28, B46). Keeping such
  entries out of the manifest stopped nothing a client names:
  `/v1/download`, `/v1/read`, the player's audio and download routes and the
  DLNA file route opened the path with `os.Open`, and opening a named pipe
  waits for a writer with nothing that can cancel the wait (a blocked
  open(2) cannot be interrupted from Go). Measured over the real
  `api.Server`: the client gave up at 2 s, and both handlers, and the two
  updater sessions they had begun (a pinned one keeps auto-install
  deferring on every poll), stayed until a writer came 7 s in; `/v1/stat`
  and `/v1/list` answered at once. A link to `/dev/null` was served as an
  empty 200, a socket as a 500. The DLNA route serves the MANIFEST's path,
  and a row outlives its file until the scan that reaps it. **One list**:
  `fsutil.NotAFile` is #1070's, moved out of the scanner, so the manifest and
  the routes cannot disagree about what a track can be; never a second copy.
  `OpenAsFile` opens `O_NONBLOCK|O_NOCTTY` on unix, refuses by the OPENED
  file's own stat (so a path replaced after a caller's stat is judged as
  what it is now), and clears `O_NONBLOCK` again for a file, because a FUSE
  daemon is handed the flags with every read. **Its EWOULDBLOCK fallback to
  a plain open is load-bearing**: a nonblocking open of a file under another
  process's write lease (Samba's kernel oplocks, an NFS delegation) fails
  where a plain open waits for the break (up to lease-break-time, 45 s),
  and a FIFO's nonblocking open never answers EWOULDBLOCK. The
  resolver-backed routes ALSO refuse on the resolver's stat before any open:
  that is what refuses a socket (the kernel refuses its open itself,
  EOPNOTSUPP on macOS, ENXIO on Linux, so `OpenAsFile` cannot name it) and
  keeps a device from being opened at all. The answers reuse existing codes,
  so there is no wire change and no Mirror-PR: 400 `bad_request` "path is a
  <kind>, not a file" (what a directory already got), 400 `bad_path` on the
  player, 404 on DLNA, 410 `variant_missing_on_disk` for a rendition whose
  sidecar is not a file. **`TestEveryServedFileIsOpenedAsAFile` fails on any
  production declaration that passes `http.ServeContent` a file it opened
  with `os.Open`/`os.OpenFile`**, so the cache routes (artwork, booklets,
  playlist covers, waveforms) open through it too and the rule has no
  exceptions; it reads one declaration at a time, so an open in one function
  served from another goes unseen. The scanner's reads and the jobs' open
  the same way since 2026-09-29 (the next bullet); this said they were
  still `os.Open` until then.

- **…and so does everything else that reads a library file in this
  process, and the listing opens its directory with `fsutil.OpenDir`**
  (2026-09-29, backlog B62). The walk judges what it hands the workers, and
  a worker opens the path later (on a large library, minutes later), so a
  file renamed over by a named pipe in between reached the extractors'
  `os.Open` and held the worker, the scan and the scanner's mutex until
  something wrote to the pipe. Measured on main: every extension the
  scanner reads (14) waited, as did an `.iso`, and a worker handed a path
  swapped for a pipe waited and then wrote a row minted from the path
  (title "01", the walk's size of a file that was gone); one swapped for a
  socket or a directory wrote that row at once. **The folder-art lookup
  needed no swap**: it read `cover.jpg` with `os.ReadFile` after a stat
  that judged only the size, and the walk judges only audio-named entries,
  so a named pipe called `cover.jpg` held every scan that extracted a track
  of its album without embedded art, and a link to `/dev/zero` called
  `cover.jpg` (0 bytes by its stat) grew the heap past 3 GiB in 0.85 s and
  was still reading, toward the out-of-memory throw no `recover` catches
  (B99's bullet). The lyrics sidecar read,
  the analysis job's STREAMINFO read and the fingerprint prefix read had the
  swap window too. Each now opens through `fsutil.OpenAsFile`, or
  `fsutil.ReadAsFile` for a whole read (`os.ReadFile`, opened as a file),
  and the folder art refuses on its stat first, so a device is never opened
  and a socket is named. **A worker whose path is no longer a file writes
  no row** (`notAFileNow`: the refusal's kind, or for the socket no open
  reaches, a stat after the failed open) and logs one Warn naming it
  library-relative; the next walk sees what is there and the row goes as a
  deleted file's, after the grace. **`/v1/list` opens with `OpenDir`**
  (`O_DIRECTORY` on unix, which the kernel checks before it opens anything;
  on Windows, whose pipe opens do not wait, `os.Open` and an `IsDir`
  check): a directory replaced by a named pipe between the resolver's stat
  and the open held the request, and now answers 500 "couldn't open this
  directory" at once. `os.ReadDir` and `filepath.WalkDir` open with
  `O_DIRECTORY` already (go1.26.6's `os.openDir`, measured), so only a
  hand-rolled directory open needs it. **The listing still lists a named
  pipe, socket or device, as an entry that is not a directory, on
  purpose**: the app's bridge share syncs from the manifest, which holds
  none (#1070), and reads the listing only to browse, where following such
  an entry gets #1082's 400 naming the kind; leaving it out would change
  what the app shows, and PROTOCOL.md's rule for what a listing holds (a
  Mirror-PR), for a row that harms nothing
  (`TestListingListsWhatIsNotAFileAsAnEntry`). A job that hands the path
  to a CHILD process (sox, ffmpeg, fpcalc's `Compute`) needs nothing: SIGKILL
  ends a child waiting to open a pipe, so the job's own timeout bounds it
  (measured on macOS and Linux). **`TestEveryLibraryReadOpensAsAFile` fails
  on a read-open (`os.Open`, `os.ReadFile`, a read-only `os.OpenFile`) in a
  package that reads library files** (`libraryReaders`: api, dlna,
  manifest, analyze, acoustid; a new such package joins the list). The
  cost is 1.5 µs an open (13.1 against 11.6 µs, APFS), and nothing dhowden,
  B117's buffer or the B99 and B101 guards see changes: they read the same
  `*os.File`, blocking again, from offset 0 (through `faultNotingSource`
  since B134, the next bullet, which forwards every call). No
  `ExtractorVersion` bump: what extraction produces for a file is unchanged.

- **…and a worker writes a file's row only from a read that COMPLETED: a
  file it could not read whole keeps the row it had, and a new one gets
  none** (2026-09-29, backlog B134). The full path wrote the track whatever
  `ExtractWithContext` returned, over `fillFromPath`'s guess (the file name
  as the title, the folders as the album and the artist), under the walk's
  stat, so a changed file whose open failed (EIO or ESTALE from a NAS,
  EACCES, a file gone since the walk) was written as that guess, a new one
  got a row by its name, and the skip gate, finding the stored size and
  mtime equal to the file's, kept the guess on every later scan until the
  file changed again (measured with chmod 0: titled "01" before and after
  the file was readable again). **A READ that failed did the same**, through
  the parsers that drop their errors (dhowden answers any failed read as a
  tag it cannot parse; the MP4 walks and the FLAC format read log theirs
  and read on), and on the version-stale leg that partial extract reached
  the diff as a changed row, replaced the stored one and was STAMPED
  CURRENT: an `ExtractorVersion` bump over a flaky mount made the guesses
  permanent. **Every extractor opens its audio file through
  `openAudioFile`**, which under `ExtractWithContext` hands back a
  `faultNotingSource` that notes the first open, read, seek or stat of the
  file that did not complete, and `ExtractWithContext` then answers
  `readIncompleteError` (`readFault`) whatever the parser made of it.
  `TestEveryAudioFileReadGoesThroughOpenAudioFile` fails on a function of
  the package that opens or reads a file through fsutil and is not one of
  the readers of OTHER files it names (the `.iso` containers, a folder's
  cover, a lyrics sidecar, the artwork cache): a new extractor opens
  through `openAudioFile`. **Two error answers complete a read**,
  being about what the file holds: the end of the file (a truncated file)
  and an offset the OS refuses before reading (`seekOffsetRefused`: EINVAL,
  on Windows ERROR_NEGATIVE_SEEK; a ReadAt at a negative offset), which a
  malformed file's own bytes ask for (dhowden's ID3v1 look 128 bytes back
  from the end of a shorter file, a DSF metadata pointer with its top bit
  set). **Don't count those as failed reads**: such files would never be
  indexed (`TestScanner_AFileReadWholeIsWrittenAsItAlwaysWas`, which also
  keeps a file a walk refuses as not its format written by name, as it
  always was). A refusal because the path is not a file is B62's answer
  (`notAFileNow`, asked first), never a fault. `keepUnread` writes nothing,
  keeps the row as it was (its old stat, or a version-stale row's old
  version, is what makes the next scan read it again), resets its missing
  count as the skip gate does (the walk saw the file), and one Warn per scan
  counts the files (`msgUnreadAudio`, with an example and the failed
  operation), never one per file: a mount that drops mid-scan fails every
  open after it. **A new file that never opens appears nowhere** (one the
  service user may not read, say) until a scan reads it: its download fails
  the same way, and a row by name is a guess the enricher would search
  MusicBrainz with; the dangling link's rule (keep, mint none) and the SACD
  one's. Driven through `Scanner.openAudio` (the `openSACD` seam's twin) on
  every platform, eight extractor families by four faults
  (`TestScanner_AChangedFileWhoseReadDidNotCompleteKeepsItsRow`, its
  new-file and version-stale twins), and with chmod 0 on unix. **No
  `ExtractorVersion` bump**: a file read whole extracts byte-identically,
  and the first scan after the change rewrites only the files it reads, as
  it always did (`TestScanner_AScanOverUnreadFilesRewritesOnlyWhatItRead`).
  The cost, accepted: a file that never reads whole (a permission, a bad
  sector under its tags) never gets a row, or keeps the one it had, with
  a line every scan; and a kept row of a changed file keeps its old size,
  so the app's size check fails a download of it until a scan reads it,
  the state the library was in before that scan. The guesses
  the defect already wrote keep a stat that matches, so they stay until
  their file changes or a later bump re-reads every row (its diff then sees
  the file's tags); a bump now re-reads the library and re-upserts every
  SACD virtual row to heal rows nothing can tell from a tagless file. To
  heal one by hand, zero its `mtime_ns` (`## Local test fixture`).

- **A case-only rename reaps the old row only when this pass committed the
  new spelling** (2026-10-14, backlog B175). `caseOnlyRenames` flags a
  stored path the walk did not see whose `renameFold` matches a path it
  did. The deletion pass used to reap that row at once, on the premise
  the new spelling was written this pass. Since B134 a worker that cannot
  read the file writes no row for a spelling that has none, so the old
  row was deleted and the new spelling never appeared, while
  `msgUnreadAudio` said the rows were kept. Measured on APFS: `Album/01.flac`
  indexed, the folder renamed to `album`, `openAudio` answering EIO; after
  the scan both `GetTrack`s were nil and the log carried the unread line
  and `reaped stale rows from case-only rename`. The workers and the writer
  fill one set (`backedPaths`), mutexed because both write it, and read
  after both have returned. The writer adds a path a successful upsert or
  stamp persisted. A worker adds a path the skip gate accepted, and a path
  `keepUnread` kept because that exact spelling already had a row. A new
  spelling the worker could not read is not added. `reapCaseRename` reaps
  the old spelling only when a path the walk saw with the same fold is in
  that set; otherwise the row is counted missing and the grace applies.
  `isUnderErroredSubtree` still runs first. A failed flush and a held
  reconcile record nothing. The unread line stays, and the reap line is
  logged only when a row was reaped. The scanner's unset threshold is 1,
  which deletes a first miss at once; the tests set
  `config.DefaultDeleteAfterMissingScans` (3), the value serve wires, so
  the kept row is visible at `missing_count` 1. A readable rename still
  writes the new spelling and reaps the old one in the same pass. An
  unchanged file still reaps a composition twin: the skip gate backs the
  live spelling, so the unseen spelling goes at once.
  (`TestAnUnreadCaseRenameKeepsTheOldRow`,
  `TestAReadableCaseRenameReapsTheOldRowAtOnce`,
  `TestScannerReapsTheUnseenCompositionOfOneFolder`.)

- **…and a folder's cover reaches the tracks beside it when IT changes,
  not only when they do** (2026-09-29, backlog B141). A cover is read only
  when a track is extracted, and the skip gate extracts an unchanged audio
  file only for a version bump, a missing `local-` cache file or lyrics
  drift, so a `cover.jpg` added after its tracks were indexed never reached
  them (measured: a full scan and a subtree scan both left the rows
  without it), nor did a replaced one, and a removed one's art stayed
  forever. **Every row records the identity of the folder art it was
  extracted against** (`tracks.folder_art_key`, v49, column-only: each
  candidate's name, size and mtime, a disc folder's parent's after a '|';
  `folderArtKey`; a candidate that is not a file is left out of the key by
  `fsutil.NotAFile`'s list of kinds, **never by "is a regular file"**, which
  would drop a OneDrive placeholder cover, and is still handed to the lookup,
  which refuses and names it as B62 made it), and `folderArtDrifted` re-extracts a row whose folder's
  identity changed, through `reExtractUnchanged`'s diff-guard, so only rows
  whose art changes reach the delta: a cover beside an embedded picture
  takes the stamp leg, which records the new key (**the stamp must write
  the key**, or the gate goes back to the row every scan). **The gate and
  the extraction read ONE per-scan state** (`folderArtDirStateOf`: the
  directory listing the lyrics check reads, now one per directory per scan
  shared by every worker, `dirListings`, plus a stat of each candidate),
  **taken BEFORE the lookup reads the cover** (`folderArtFor`), so no row
  records an identity newer than its cover: taken after, a cover replaced
  as it was read was recorded under the new identity beside the old bytes,
  and never read again (`TestScanner_ACoverReplacedWhileItIsReadIsReadAgain`,
  red with the order swapped). **A removed cover takes its art away**:
  `mergePostScanFields` no longer copies an old `local-` value onto a fresh
  extraction whose pipeline ran and completed (`localArtSettled`) and found
  none, since `local-` is the scanner's own value (the enricher writes
  MusicBrainz ids, never over one); the upsert's `enriched_at` reset lets
  the enricher give the row a network cover, as for a changed file. A
  folder cover still outranks the enricher's (fresh non-zero wins).
  **A cover the scan could not see, read or store is B134's rule for
  covers**: a stat or read that did not complete (`folderArtReadIncomplete`;
  gone, or not a file, is an answer), or a cover whose cache file could not
  be written (a full or read-only data directory), leaves the extraction
  `localArtUnsettled`, the row keeps the art it had
  (`keepArtOfUnsettledRead`, on the full path too, and over a partial answer
  from another candidate: taking it would change the row twice), records
  `folderArtUnsettledKey` ("?"), and the gate then retries only the COVER
  (`folderArtUnreadable`) and re-extracts the rows once it is stored, so a
  cover that stays unreadable costs one failed attempt per folder a scan,
  never a tag read of its album, and one Warn per scan counts its tracks
  (`msgUnreadFolderArt`). A folder whose listing or a candidate's stat fails
  keeps its rows untouched (an ELOOP'd cover read as "no cover" dropped the
  album's art). A cache write that failed was first read as settled, and a
  cover replaced while the data directory was full kept its rows on the old
  art under the new cover's key, for good (CodeRabbit on #1117,
  `TestScanner_AReplacedCoverWhoseCacheCannotBeWrittenIsStoredLater`). An
  EMBEDDED picture whose cache file could not be written is no verdict
  either (`localArtWriteFailed`), and the gate does not retry it, since that
  retry is a tag read: the merge keeps the row's old `local-` value for
  `needsLocalArtworkRecovery` to retry (read as "no cover", a wiped cache
  whose rewrite failed lost its art for good,
  `TestScanner_AWipedCacheThatCannotBeRewrittenKeepsTheArt`). **A
  file its extractor refuses** (read whole, not its format, written by
  name) records `folderArtNotLookedKey` ("-"), which the gate never
  re-checks, and one the gate re-reads records it through the stamp of
  its refusal (the next bullet), without which it was re-read every
  scan (`SetFolderArtKey`, which recorded it alone, went with B145). **The
  upgrade**: existing rows' key is '', which is also the key of a folder
  with no cover, so the first scan re-reads, once, the tracks in folders
  that hold one. **No `ExtractorVersion` bump**: what extraction produces
  for a file and its folder is unchanged. **Cost, measured** on dido (3,000
  FLACs in 300 albums with covers): an unchanged scan takes the same time
  (522 ± 26 against main's 528 ± 14 ms) and makes 1,262 directory reads
  (getdents64) where main made 6,360, since the listing is no longer read
  once per worker, plus one stat per cover. Residuals: a cover rewritten in
  place at the same size inside one mtime tick (the audio gate's own), and
  a stale `local-` value from a cover removed BEFORE the upgrade (its folder
  keys to '' like its row; the next `ExtractorVersion` bump drops it).

- **…and a file its extractor REFUSES records the refusal, and the skip
  gate asks its row nothing more until the file or the extractor changes**
  (2026-09-29, backlog B145). A refusal is a read that completed and was
  refused as not its format (the DSF, DFF, AIFF and WAV walks, for a header
  that is not one; every other extractor reads what it can and refuses
  nothing), indexed by its name as it always was. Such a file was re-read,
  with an ERROR line, on every scan, forever: after any `ExtractorVersion`
  bump, since `reExtractUnchanged` answered a refusal with nil and stamped
  nothing; and with no bump at all beside a lyrics sidecar (`bad.dsf` +
  `bad.lrc`) or when its row names a `local-` cache file that is gone,
  since those gate questions ask about what the extraction of a refused file
  never reaches (measured with the real scanner, all red on main). **Every
  row records whether the extraction it was written or stamped from refused
  the file** (`tracks.extract_refused`, v50, column-only,
  `Track.extractRefused`), and `rowIsCurrent` answers yes for a refused row
  at the current version without asking the lyrics, art-recovery or
  folder-art question. **A refusal on the version-stale leg is stamped,
  never written**: `reExtractUnchanged` hands the writer a
  `versionStampOnly` Track with `extractRefused`, and
  `StampExtractorVersionBatch` records the version, the mark, the
  missing-count reset and the refusal's folder-art key ("-"), and SKIPS
  the lyrics write, which reads a refusal as a file with no lyrics and
  deletes a row an older extractor wrote. Tags, `indexed_at` and
  `enriched_at` are untouched, so nothing reaches a paired device. **Don't
  rewrite the row**: it may hold tags an older extractor read before a
  stricter one refused the file
  (`TestScanner_ARefusalKeepsTheRowAnOlderExtractorWrote`). **Every other
  write clears the mark** (both upserts from the Track, the stamp from a
  Track that read), so a file that reads again, changed or under an
  extractor that learned to read it, goes back to the gate every file takes
  (`TestScanner_AFileTheExtractorNowReadsLosesItsRefusal`). **B134's line
  holds**: only a completed read can refuse, `readFault` is asked FIRST, and
  a read fault keeps the row at its stale version for the next scan
  (`TestScanner_ARefusedFileWhoseReadDidNotCompleteIsReadAgain`). **A
  refusal is logged once per path, size and mtime**, the analysis strike's
  dedup: the full path logs each refusal it writes (it reads only a file no
  row describes), and the version-stale leg only when the row had not
  recorded one, so a later bump re-reads a refused file once, silently.
  **No `ExtractorVersion` bump**: extraction is unchanged. v50 leaves
  existing rows unmarked, so a refused file the gate goes back to (after a
  bump, or for one of those questions) is read, and logged, once more, and
  marked then. Residual: a refused row holding a `local-` value an older
  extractor gave it, whose cache file is gone, keeps it (a refusal cannot
  recover it, and the row is not rewritten); it no longer costs a read per
  scan.

- **A library ROOT that is itself a link to a directory is walked THROUGH,
  and every walk of a root starts from `fsutil.WalkableRoot`** (2026-09-28,
  backlog B41). `filepath.WalkDir` Lstats its root and follows no link, so a
  configured `/music -> /mnt/nas/music`, and on Windows a junction or a
  volume mounted in a folder (`ModeIrregular` without `ModeDir` since Go
  1.23), was one entry that is not a directory. Measured on main: Scan and a
  subtree scan of the root indexed 0 rows (2 with the same root spelled with
  a trailing slash), a multi-root scan indexed nothing under the linked root,
  the watcher registered 0 watches and returned nil, the doctor's inotify
  count saw 0 directories, and `POST /v1/upscale` of the root enqueued the
  folder itself.
  **The half that deleted a library**: an install whose root BECAME a link
  after it was indexed logged `suspected clean-empty mount failure` every
  scan, with the hint to place `.bridge-allow-empty`; the sentinel, created
  through the link, lands beside the files, and at the production threshold
  the third scan deleted every row and sent the tombstones, every file on
  disk. A subtree scan of the root (the watcher's, an upload's, the trash
  tidy-scan) reaped every row with no line at all, and so it did when the
  link DANGLED, where Scan spared them. `WalkableRoot` answers the root with
  a separator appended when it is a link to a directory: POSIX resolves a
  trailing slash through a symlink, chains included, and Go's `os.Lstat` on
  Windows follows a name surrogate when the path ends in a separator. So
  every path below keeps the CONFIGURED spelling: `relPath` stores what it
  always stored, a multi-root prefix is the configured root's basename and
  never the target's, and `fs.Resolver`, which joins lexically, serves each
  row from the file the scanner read. **Compare the root entry against the
  WALKED string, never the root**: WalkDir hands its callback the string it
  was given, separator included, so `abs != root` counts the root as an
  entry and can prune it. **Not `filepath.EvalSymlinks`**: since Go 1.23 it
  resolves no Windows junction or mounted folder, which are not
  `ModeSymlink`, so the "resolved" root is the junction again. Nor the root
  resolved through them (`fsutil.ResolveLinks`, which the sidecar walks use
  because they unlink in the tree they walked): that respells every path,
  which each caller would have to map back. A root that cannot be stat'ed
  through (missing, a dangling link, a link into a mount that went away)
  is an error and "",
  never the unresolved root: Scan logs `root unreachable` and spares it,
  ScanSubtree of the root returns before its deletion pass, the watcher and
  the doctor report it. **Only the root is followed**: a link to a directory
  BELOW a root is still not walked by any of them (and the scanner says so:
  the B74 bullet below), and the upscale folder
  walk follows a folder only when it IS a root, so it enqueues nothing the
  manifest does not hold. `TestEveryWalkOfALibraryRootStartsFromWalkableRoot`
  requires every production function that calls `filepath.WalkDir`,
  `filepath.Walk`, `fs.WalkDir` or `fsutil.WalkConfigured` to be classified:
  the five root walks
  (`walkRoot`, `ScanSubtree`, the watcher's `addTree`, the doctor's
  `countDirs`, the upscale folder walk) must reach `WalkableRoot`, and every
  other names what it walks. It sees the call, not that its answer is what
  gets walked; the tests of each walk drive a linked root through it. No
  `ExtractorVersion` bump and no PROTOCOL.md change: an install whose root
  became a link rewrites nothing, and one whose root was always a link
  indexes its library for the first time (a whole-library delta and
  enrichment, once).

- **…and a link chain whose trailing-slash Lstat disagrees with Stat is
  still walked, under the configured spelling** (2026-10-12, backlog B257).
  `os.Lstat(root+"/")` has answered "not a directory" while `os.Stat(root)`
  answered a directory, once on CI macos-26-arm64, for a link to a link
  (`TestWalkableRootDescendsALinkedRoot`: `links to a directory the walk
  cannot enter`). The root then read as unreachable for that scan and its
  rows were spared. When those two answers disagree — the trailing-slash
  Lstat is not a directory, or it returns an error, while Stat of the same
  root is a directory — `WalkableRoot` resolves the chain with
  `ResolveLinks` and, when that names a directory, returns the configured
  root with a separator appended. The resolved path decides walkability
  only. It is never the path the walk is given, so every path below the
  root keeps the configured spelling. When the two answers agree, the chain
  is not resolved. A chain that ends at a file, a broken chain, and a
  separator that does not lead into a directory still refuse, as before.
  The disagreement is injected by `walkableRootSlashLstat` (nil in
  production). `TestWalkableRootDescendsALinkedRoot` logs both answers,
  each with its error and mode, when it fails.
  (`TestWalkableRootWalksAChainWhenTheTrailingSlashLstatDisagrees`,
  `TestWalkableRootRefusesAChainThatDoesNotNameADirectory`,
  `TestWalkableRootKeepsTheSlashLstatErrorWhenTheChainDoesNotResolve`.)

- **The walk of the string `WalkableRoot` returns is `WalkConfigured`**
  (2026-10-13, backlog B358). `filepath.WalkDir` Lstats that string itself
  and, when the Lstat is not a directory, invokes the callback for the root
  and returns without reading it. On the CI image that produced the
  disagreement above, that second Lstat answered "not a directory" after
  `WalkableRoot` had already returned the separator path, and the walk
  visited nothing (`TestWalkableRootWalksAChainWhenTheTrailingSlashLstatDisagrees`,
  run 38348380982). A walk that sees the root and nothing under it is an
  empty library to the scanner. `WalkConfigured`, when the path ends in a
  separator and `ResolveLinks` names a directory, walks that directory and
  reports every path under the configured spelling, the root callback
  included. The resolved directory keeps a trailing separator when removing
  it would leave nothing or only a volume name, so a chain that resolves to
  the filesystem root is walked at that root (`resolvedWalkRoot`). Removing
  the separator from that root leaves nothing, or the volume name, and the
  volume name is that drive's current directory
  (`TestAChainThatResolvesToTheVolumeRootIsWalkedThere`; the chain resolves
  through `resolveLinksForWalk` and the walk through `walkResolvedDirectory`,
  so the test does not list the volume). When that resolution does not name
  a directory, the callback is invoked once for the Lstat of the string, which is what
  `filepath.WalkDir` does for a file and for an Lstat that fails. The five
  root walks use it: `walkRoot`, `ScanSubtree`, the watcher's walk, the
  doctor's directory count, and the upscale folder walk. The watcher still
  registers watches at the resolved directory when `EvalSymlinks` names
  one; `WalkConfigured` is then a plain `WalkDir` of that directory. No
  `ExtractorVersion` bump.

- **A subtree scan OF the root runs the clean-empty guard Scan runs, and a
  line about a linked root names what it links to** (2026-09-28). The
  owning-root audit answers a SUBTREE that is not there; nothing covered the
  root itself, so a subtree scan of an emptied mount point reaped every row
  at the threshold while Scan spared them (measured on a plain root: three
  subtree scans, every row deleted, no line). `emptyRootMustBeSpared` runs
  there now, after a walk of the root that saw nothing and did not fail. For
  a linked root the directory found empty, and the one the sentinel is
  looked for in, is the link's target, so `suspected clean-empty mount
  failure` and `root unreachable` carry `links_to` (`rootLinkTarget`:
  `EvalSymlinks` for a live symlink, `os.Readlink` for a dangling one or a
  junction) and the hint says to check that volume is mounted. What the
  guard counts is the next bullet's.

- **…and the guard counts only LIBRARY CONTENT, by the walk's own rule**
  (2026-09-28, CodeRabbit on #1076). The walks counted every entry they were
  handed, dot-files included, so a `.DS_Store` Finder wrote into an emptied
  mount point, or a Synology `@eaDir`, made the root non-empty: no line, and
  the third scan deleted every row, in Scan and in a subtree scan of the
  root, single- and multi-root (measured, 8 cases of 8). The owning-root
  audit a subtree scan runs when its subtree is missing counted
  `len(entries)` the same way, and the bounded pass reaped the subtree.
  `isLibraryEntry` is the one predicate: a directory the walk descends into
  (`ShouldSkipDir` says no) or a file it indexes (not a dot-file, an audio
  file `enqueueableAudioFile` takes). Both walks skip by it and count by
  it, and the audit asks it of the root's listing (`holdsLibraryContent`).
  **Every file the walk does not index counts as nothing**, not a list of
  named ones (`Thumbs.db`, `desktop.ini`, a `NOT_MOUNTED` marker, a cover
  image): a second list is a second rule, and a file the walk ignores is no
  evidence the volume is there. The cost: a mounted root whose last audio
  file went, with only such files left, reads as emptied too and keeps its
  rows, with a line per scan, until the sentinel is placed, as an emptied
  root always did (`TestScannerThreshold1PreservesImmediateDelete` kept its
  database inside the root and passed only because that file counted; it
  keeps a second track now). **The sentinel is asked for by name, in the
  audit too**: it used to work by being an entry like any other, which is
  why a review on #289 called the audit's explicit check redundant, and it
  is now the only thing that says a root is empty on purpose. **`ShouldSkipDir`
  also names the directories operating systems and NAS firmware leave in a
  volume**, exactly, case included: `$RECYCLE.BIN`, `$Recycle.Bin`,
  `System Volume Information`, `lost+found`, Synology's `@eaDir`,
  `#recycle` and `#snapshot`, QNAP's `@Recycle` and `@Recently-Snapshot`,
  NetApp's `~snapshot`. The walk descended them, so a recycle bin's deleted
  files and a snapshot's copies were indexed as tracks of their own (the
  old walk indexed six of the test's fixtures on macOS), and their rows now
  go after the usual missing-count grace. It is exported so the doctor's
  inotify count skips by it: that count kept a copy of the old list, which
  would have gone on counting what the watcher now skips. So does the
  upscale folder walk (`POST /v1/upscale` of a folder, 2026-09-29), which
  descended them and offered every file in a snapshot to the enqueuer as a
  candidate; it walks the folder the request names whatever its name, as
  the watcher walks a dot-named root
  (`TestUpscaleFolderRequestSkipsWhatTheScannerSkips`,
  `TestUpscaleFolderRequestWalksADotNamedRoot`). So do the sidecar walks
  of the variants and waveform trees, all but lost+found, since 2026-10-02
  (the B207 bullet under **Job pools**).
  `TestScanner_AnEmptiedRootHoldingOnlyNoiseSparesItsRows`,
  `TestScanner_ASubtreeScanBelowARootHoldingOnlyNoiseIsRefused`,
  `TestScanner_OSAndNASDetritusIsNotLibraryContent`.

- **…and the rows it counts are the ROOT'S OWN: never a UPnP-routed row**
  (2026-09-29, backlog B51). `CountTracksUnderRoot` answered the whole
  table in single-root mode, so a bridge whose root is empty and which
  relays an upstream logged `suspected clean-empty mount failure` on every
  scan (measured: three lines in three scans, `rows_in_db=3`, all of them
  routed), and the owning-root audit refused every subtree scan below the
  root ("no library content on disk but 3 tracks in DB"). A routed row is
  no evidence a root held anything: no walk sees one and neither deletion
  pass reaps one (`routedPathSet`). In multi-root mode a routed path prefix
  spelled like a root's basename inflated that root's count the same way.
  Both statements anti-join `upnp_track_routing`; the byte range and the
  empty-base fail-safe are `CountTracksByPrefix`'s, which the admin's
  per-root rollups still read unfiltered
  (`TestScanner_AnEmptyRootBesideRoutedRowsIsNotAMountDrop`,
  `TestScanner_TheCleanEmptyGuardCountsOnlyTheRootsOwnRows`).

- **A link to a directory BELOW a root is still not followed, and the
  scanner SAYS so** (2026-09-29, backlog B74). Moving an album folder to
  another volume and leaving a link (on Windows a junction, `mklink /J`)
  took it off every paired device, measured on main 6dfba62c (macOS
  symlinks, Windows 11 junctions; full scans, subtree scans of the root and
  of the folder holding the link, multi-root). (a) A root whose only
  content was such links read as a suspected clean-empty mount: an ERROR
  every scan, rows kept, and the hint to place `.bridge-allow-empty`, which
  then deleted both rows, a tombstone each, while the files stat'ed through
  the link. (b) A root holding one beside real content lost the rows under
  it at the threshold, a tombstone each, with only an Info count naming
  nothing. Both walks now record every entry `dirLinkEntry` calls a link to
  a directory: listing type neither regular nor a directory, and a stat
  THROUGH it a directory. **Never the `ModeSymlink` bit**: a junction or a
  volume mounted in a folder is `ModeIrregular` since Go 1.23, and a
  symlink-only test turned every behavioural test red on Windows with real
  junctions while macOS stayed green. (a) The guard's line for a root
  holding only such links is its own, `library root holds no content but
  links to directories…; its rows are kept`, not the mount-failure one (a
  link stat'ed through to a directory says the volume is there), with the
  count, an example, `rows_under_links` (`CountOwnTracksUnderPrefix`, the
  one statement `CountTracksUnderRoot` now runs too), and a hint that the
  sentinel deletes those rows and that the way to index a linked directory
  is to make it a library root. The owning-root audit's refusal says the
  same. (b) The deletion pass Warns `rows under links to directories are
  counted missing…` **once per ROW's streak**: `TracksNotYetCountedMissing`
  asks, BEFORE the increment, which of the rows under a link are still at
  `missing_count` 0, so a restart mid-streak says nothing more and a row
  that came back and went again says it again; a pass whose increment
  failed says nothing. **A link to a directory is never library content,
  whatever its name**: counted by its name (`Live.flac -> dir`), a root
  holding nothing else passed the guard and its rows went with no line, (a)
  turned into (b). `isLibraryEntry` stays the NAME rule and is never asked
  about one: both walks and `holdsLibraryContent` ask `dirLinkEntry` first
  (a track-named one is still in the "entries that are not files" line). A
  line names a link library-relative (#1055), never the absolute path it
  leads to, and a scan's guard line names only ITS root's links. Whether the
  scanner should follow such links is a product decision this rule does
  not make: backlog B80 (loops, #1070)
  (`TestScanner_ARootHoldingOnlyLinksToDirectoriesNamesThem`,
  `TestScanner_RowsUnderALinkBesideContentAreAnnouncedOncePerStreak`,
  `TestScanner_ALinkStreakIsSaidOnceAcrossARestartAndAgainWhenItRestarts`,
  `TestScanner_ALinkToADirectoryNamedLikeATrackIsNotContent`,
  `TestScanner_ASubtreeMissBesideOnlyLinksNamesThem`,
  `TestDirLinkEntryTellsALinkToADirectoryByTheStatThroughIt`).

- **A configured root that is a link is WATCHED at the directory it
  resolves to, and every event under it is named back under the configured
  root** (2026-09-29, backlog B51). fsnotify's kqueue backend (macOS, the
  BSDs) follows ONE level of a link it is asked to watch (it `Readlink`s
  and `Lstat`s once), so a root that is a link to a link had its own watch
  registered as a watch on a FILE: no Create event for anything added to
  it, and each change named after the root itself, which the watcher took
  for a change in the root's PARENT. Measured on macOS: a file dropped into
  the root, and one dropped into a folder made in it after the watcher
  started, never reached the manifest through the watcher, and each change
  logged `ERROR subtree scan … is not under any configured library root`.
  `watchWalkStart` answers the directory `filepath.EvalSymlinks` resolves a
  linked root to when that is a plain directory, the walk registers every
  watch in that spelling, and `configuredName` renames an event's path
  (longest resolved prefix, separator-bounded) before the scan is asked
  for. **The Create branch watches a new directory as fsnotify named it**,
  the resolved spelling, because kqueue already holds an entry watch under
  that name and a configured spelling would be a second descriptor. This is
  on every platform, not only kqueue: inotify and ReadDirectoryChangesW
  followed the chain already, and renaming their events back changes
  nothing they did. A Windows junction, which `EvalSymlinks` leaves as it
  is, is walked through `WalkableRoot` and watched as the configured path,
  as before (ReadDirectoryChangesW follows it). A plain directory root with
  a linked ANCESTOR (`/var` on macOS) is not a linked root and is
  untouched. Cost: `WatchList()` and the watch lines name a linked root's
  tree by its resolved spelling
  (`TestWatcherWatchesARootThatIsALinkToALink`,
  `TestConfiguredNameRenamesOnlyWhatIsUnderAResolvedRoot`).

- **Every containment check and sidecar walk resolves a Windows junction
  as it resolves a symlink** (`fsutil.ResolveLinks`, 2026-09-29, backlog
  B51). Since Go 1.23 `filepath.EvalSymlinks` leaves a junction (and a
  mounted folder, both `ModeIrregular`) as it is at the end of a path and
  fails with ENOTDIR through one, so `fsutil.EvalSymlinksOrClean` and
  `IsUnderAny` compared a junction'd path by its own spelling: measured on
  Windows 11, a variants directory spelled by the target's path under a
  junction'd library root (`D:\real\variants` against
  `C:\lib -> D:\real`) read as NOT nested, which `validateVariantsDir`,
  the admin variants-dir handler
  and `bridge variants move` exist to refuse, and so did every other nested
  case through a junction or a chain of them. And `resolveSidecarRoot`
  walked a junction'd variants directory AT the junction, as one entry:
  the inventory found 0 files of 2, and `TreeHoldsVariantSidecars` read a
  tree full of sidecars as holding none, which is the reading that lets a
  mass reap through. On Windows `ResolveLinks` opens an absolute path
  following every reparse point and names it by the handle
  (`GetFinalPathNameByHandle`, `\\?\` taken off); a path that is not
  there is `fs.ErrNotExist`; one the call cannot name by a drive letter (a
  volume mounted only in a folder can be one) is named by its volume GUID
  path, `\\?\Volume{…}\…`, which Go's os and filepath functions take as it
  is; and one that is there but cannot be named at all (two junctions
  pointing at each other, measured) gets `EvalSymlinks`'s answer, as
  before, **unless that answer still ends at a link to a directory, which is
  an error**: answered as resolved it is one entry to a walk and its own
  spelling to a comparison (CodeRabbit on #1090). Elsewhere it IS
  `EvalSymlinks`. **Not
  `os.Readlink` component by component**: that is a fork of the stdlib's
  link walk, and it answers `\\?\Volume{…}\` for every mounted folder, one
  with a drive letter included. The
  sidecar walk now starts at the junction's TARGET, so the paths a sweep
  unlinks are in the tree it walked (#1063's rule, for junctions). A SUBST
  drive resolves to what it stands for (measured), a mapped drive by the
  same call; both sides of every comparison go through the same function,
  so they agree
  (`TestIsUnderAnySeesThroughAJunction`,
  `TestSidecarInventoryResolvesAJunctionedRoot`,
  `TestTreeHoldsVariantSidecarsThroughAJunction`, Windows only, with real
  `mklink /J` junctions, and `TestResolveLinksFallsBackOnAJunctionLoop`;
  `TestResolveLinksAgreesWithEvalSymlinksWhereThereIsNoLink` on every
  platform).

- **The five post-scan reconciliation passes all exclude UPnP-routed rows, from
  ONE routed set computed at the reconciliation head**, fail-closed (a fetch
  error skips all five) — never a per-pass `routedExclusionSet` call. Four of them didn't, and since `walkFieldsEqual` diffs
  exactly AlbumArtist/Album/Year, a disagreeing routed album flip-flopped every
  fs-scan ↔ UPnP-walk cycle — a perpetual re-enrich treadmill plus iOS delta
  churn.

- **Reconciliation is directory-scoped and never crosses directories**, picks
  the dominant EXISTING value (never MusicBrainz — MB's classical credits favour
  performers over composers and would shatter composer-sorted libraries), and
  **leaves `enriched_at` untouched**. Year reconciliation is FILL-MISSING ONLY;
  a present-but-different year is left alone. The one pass that crosses
  folders is the year fill by release id (`reconcileYearsByMBID`), bounded to
  strays (at most three year-0 tracks under one release); this bullet said
  none did until 2026-09-29.

- **A year before the phonograph is a composition date beside a recording
  year** (2026-10-13, backlog B342). `firstPhonographYear` is 1877, the
  year of Edison's phonograph, the first machine that recorded sound and
  played it back. Phonautograph recordings from the 1850s and 1860 could
  not be played back, so no recording a release carries predates it. In
  `reconcileYears` only, when a directory-and-album group has any year of
  1877 or later, the dominant recording year is `dominantYear` over those
  years alone (a tie goes to the larger year), and every missing year and
  every year from 1 through 1876 takes it. A group with no year of 1877
  or later keeps the earlier fill: the dominant present year fills only
  the missing ones, and the early years stay. A year of 1877 or later
  stays as tagged, so 1974 beside 2022 remains a split. The pass leaves
  `OriginalYear` unset. Grouping, `dupes.albumID` and
  `reconcileYearsByMBID` stay as they are, and `ProtocolVersion` stays 1.
  Extraction stays as it is and `ExtractorVersion` stays 24: `Scan`'s
  tail already runs the pass over every group (`streamReconcileTargets`),
  so a library that already stores these years is filled on the next full
  scan. `reExtractUnchanged` holds a re-read the pass would rewrite
  (`TestScanner_APreRecordingYearFilledFromTheAlbumDoesNotChurn`). A
  subtree scan runs no pass, so healing waits for the next full scan.

- **A version-stale re-read that a reconciliation pass would rewrite is
  judged by the passes, at the scan's tail, with every such re-read in
  place** (2026-09-29, backlog B188). The passes REWRITE values a file
  sets (an album that is its folder's name, a minority album artist, a
  year of 0), and `mergePostScanFields` keeps a stored value only where
  the fresh one is empty, so the diff guard called every reconciled row
  changed on every ExtractorVersion bump: the upsert served the file's
  value and reset `enriched_at`, and the tail reconciled it back (a second
  `indexed_at` bump). Measured on main 99f3353e: a bump over 11 rows moved
  and re-enriched exactly the 3 a pass had rewritten, and after a
  `cover.jpg` touch (B141's re-read) a subtree scan, which runs no pass,
  served "Some Folder" for "Real Album" until the next full scan. The
  v0.2.1 upgrade scan (ExtractorVersion 21) would have done it to every
  reconciled row of a library. `reExtractUnchanged` now HOLDS a re-read
  that still differs from its row in one of the four fields after the
  merge (`reconciledFieldsDiffer`, `Track.awaitsReconcile`, unmerged; at
  most `maxHeldReconciles`, 10,000, past which it is decided as before);
  the scan's writer collects it, and `settleHeldReconciles`, at the
  reconciliation head (after the deletion pass, before the passes, with the
  one routed set), merges each with its row as stored now, runs the passes
  IN MEMORY over the library with every held re-read's values in place of
  its row's (`runReconcileStepsInMemory`), and stamps a re-read that then
  marshals as its row, or writes it whole with the passes' values, so the
  passes after it find nothing. A subtree scan settles its held re-reads
  the same way (`settleSubtreeHeldReconciles`), and one that returns before
  either writes them as they stand (`settleHeldUnreconciled`, a shutdown
  nothing). **The tail and the settle run one table**, `reconcileSteps`, in
  one order: the five `run*Reconciliation` functions went into it.
  **Judge with every held re-read in place, never against the stored
  siblings**: a bump that reads a whole album's tag differently leaves no
  outlier, while a re-read judged against its siblings' stored rows is
  voted back to the old reading and the change never applied (with the
  overlay dropped, `TestScanner_ABumpStillAppliesAnExtractorChangeAcrossAWholeAlbum`
  keeps "Old Reading" on all three rows and moves none). **Not a record of
  the value each pass replaced** (the review's first suggestion): rows
  reconciled before the upgrade carry none, so the upgrade scan would
  churn as before. **Not the passes in ScanSubtree's tail** (its second):
  measured, a subtree scan followed by the passes wrote the file's value
  (a bump and an `enriched_at` reset) and then the reconciled one (a second
  bump), and a bump's full scan runs the passes already. **The enricher's
  two replacements of a file's id meet the same guard**: a file's release
  or recording id that is no MBID reads as no id in the merge
  (`manifest.IsValidMBID`, the one shape the enricher's `isValidMBID` now
  answers through), and the acoustic fallback fills a recording id only
  where the file carries no valid one, as the merge's docblock said it
  did. `TestScanner_ABumpOverReconciledRowsOnlyStampsThem`,
  `TestScanner_ASubtreeScanAfterACoverTouchKeepsAReconciledAlbumTitle`,
  `TestScanner_ABumpOverAnIDTheEnricherReplacedOnlyStampsIt`,
  `TestScanner_ReReadsPastTheHoldLimitAreWrittenAsBefore`,
  `TestApplyAcousticFallbackKeepsARecordingIDTheFileCarries`.

- **The restamp re-checks for an in-flight scan immediately before
  `ApplyDupeStamps`**, via `Scanner.activeScans`. Don't take `s.mu` instead —
  it deadlocks the in-scan callers and, for external ones, blocks the scan for
  two library walks; and **don't widen the public `IsScanning`** to cover subtree
  scans, which drives the admin badge, the SSE fast tick and the booklet-GC skip.
  The stand-down is `ErrDupeRestampDeferred` (the next bullet).

- **An external restamp that finds a scan in flight returns
  `ErrDupeRestampDeferred` and stamps nothing** (2026-10-14, backlog
  B184). That check used to return `(0, nil)`. The duplicates sweeper
  logged that as applied and did not re-arm: `IsScanning` sees a full
  scan only. `ScanSubtree`'s restamp tail runs only when that scan
  committed, settled, deleted or renamed a row, or a stamped row was
  deleted outside it. A subtree scan of an unchanged folder stays in
  `activeScans` for its whole body and then skips the tail, so a policy
  change from highest-quality to off left the losing copy
  `dupe_suppressed = 1` after the pass and after another subtree scan of
  the same folder. The sweeper treats the sentinel like its `IsScanning`
  branch: it puts the nudge back, waits `duplicatesDeferRetry`, and does
  not call `sweepFinished`, so the Jobs card stays on "re-evaluating now"
  and `dupe_summary` stays on the policy the last pass actually stamped.
  The in-scan caller passes `insideScan`, never reaches the guard and so
  never sees the sentinel. `noteRestampCovered` still runs only after
  stamps commit. Don't take `s.mu`. Don't widen `IsScanning`.
  (`TestAPolicyChangeDuringANoOpSubtreeScanIsDeferred`,
  `TestRestampDuplicates_AbandonsWhenAScanStartsMidPass`,
  `TestDuplicatesSweeperReArmsWhenThePassDefers`.)

- **A row deleted where no stamping pass follows reaches the duplicate stamps
  through a COUNT kept where every deletion passes** (2026-10-02, backlog
  B218). The console's delete retires its rows (`retireAndRescan`, threshold 1)
  BEFORE the subtree scan it runs, which then wrote and reaped nothing, so its
  restamp gate stayed shut: delete a duplicate's served copy and the copy it
  suppressed stayed hidden on every device, which had been sent the served
  copy's tombstone, until the next full scan (measured through the real
  `POST /api/library/trash` under the default filter: one copy of the track
  served before the delete, neither after, and an empty delta). Migration
  v51's trigger (`tracks_stamped_row_deleted`) counts each deleted row that
  carried a stamp into `dupe_stamp_deletions.deleted`, in the deleting
  statement's own transaction; a stamping pass reads the count BEFORE its
  snapshot and records
  it as `covered` only once its stamps commit (`noteRestampCovered`), and
  `deleted > covered` (`stampsBehindDeletions`) opens `ScanSubtree`'s tail gate
  and runs a pass at the HEAD of a full `Scan`, before the walk, where the
  root removal, the delete's full-scan fallback, `bridge manifest
  clear-missing` (while the bridge is stopped) and a scan whose tail pass did
  not finish all land: each was healed only by a full scan's tail, after a walk
  of minutes. **Count in the trigger, never in each deleter**: seven Store
  functions delete tracks rows, in this process and in the CLI's, and one added
  later is counted without anyone remembering to. **Only a row that carried a
  stamp counts**, so the delete of a track that is no copy of anything, and
  every UPnP-routed reap (never stamped), costs no pass
  (`TestTrashingATrackThatIsNoCopyRunsNoStampingPass`). **Read the count before
  the snapshot and record it only after the commit**: read at the commit, a
  deletion during the pass is recorded as seen
  (`TestADeletionDuringAStampingPassIsLeftForTheNext`); recorded by a pass that
  stood down for a scan (the external restamp still does, and records nothing)
  or whose stamps failed, it is never restamped for
  (`TestAStampingPassThatDoesNotApplyLeavesTheDeletionUncovered`).
  `WipeFilesystemTracks` deletes every stamped row, so it leaves nothing
  suppressed; its count costs the next scan one pass over what is left.
  `TestTrashingTheServedCopyOfADuplicateServesTheOther`,
  `TestEveryDeletionOfAStampedRowIsCounted`,
  `TestASubtreeScanRestampsAfterAStampedRowIsDeletedOutsideIt`,
  `TestAFullScanRestampsBeforeItWalksWhenAStampedRowWasDeleted`.

- **`tracks.first_indexed_at` is set once, on insert, and an update of the
 same path never moves it** ( 2026-10-07, sync phase 3; generation, the invariant carry key,
 and review round 3 on 2026-10-08). Migration v53 adds the
 column and `first_indexed_carry`. Every open fills remaining NULLs from
 the stored `mtime_ns` (a zero or future mtime becomes the open's clock)
 without assigning `indexed_at`; v54 adds `generation`, `target_multi`,
 and the partial index `idx_tracks_first_indexed_at_null`. Both upserts
 write the column only in the INSERT arm: the caller passes
 `carryFirstIndexedNS` when it has a date, otherwise the insert uses
 `s.now()`. The conflict arm, `StampExtractorVersionBatch`, and a rescan
 of an existing path leave it. The scanner copies the previous date into
 the new insert for a case-only rename (the before-set fold, on a full
 scan and a subtree scan, including a virtual row under a renamed
 container) and for a library-root flip. The saved key is the root's
 folder name plus the path within that root, the same string in both
 storage forms. The snapshot takes the existing root's folder name
 when a root is added and prefixes the stored path, and on a collapse
 keeps only the surviving root's rows, whose stored path already begins
 with that folder name. A single-root scan looks a date up by prefixing
 the folder name of the root that scan is walking. A multi-root scan
 looks the stored path up directly. A flip that changes the storage
 form records and wipes in one transaction
 (`RecordFirstIndexedCarryAndWipe`: admin add, admin remove that
 collapses to one root, `bridge library add`, `bridge library remove`).
 The scenario tests take that same call (`recordAndWipe`, and the add
 ahead of the cancelled collapse in
 `TestACancelledCollapseKeepsTheDatesTheAddRecorded`). The snapshot
 alone is `recordFirstIndexedCarry`, unexported, so a caller cannot
 record and wipe apart. A test that inserts a row between the snapshot
 and the wipe (`TestARootFlipCarriesTheFirstIndexedDateAndAFullScanClearsIt`)
 and the snapshot's own checks are its callers. Don't export it again.
 A prefix delete that stays multi-root records nothing.
 A record merges and bumps the generation. A snapshot with no rows writes
 nothing when no dates are saved yet; when dates are already saved it
 moves them onto the new generation and the form this flip is heading
 toward, and does not delete the keys. A full scan clears only the
 generation it loaded, and only when the library is already in the saved
 form. A subtree scan looks each insert up and does not clear. The
 subtree fold is one `unicode_lower` lookup on
 `idx_tracks_path_unicode_lower`. The in-memory date map folds with
 `pathFold`, the case-fold-then-NFC `unicode_lower` applies. The rename
 pairing folds with `renameFold` (NFC, then that same lowercase), held
 to the same answer by `TestRenameFoldMatchesPathFold`, so a decomposed
 name and its precomposed spelling take one date on a full scan and a
 subtree scan and the unseen spelling is reaped. Both spellings left in
 place are not (`TestCaseOnlyRenames`). A failed
 or cancelled wipe rolls that transaction back, so it does not delete
 dates an earlier flip still has pending. A save that fails after the
 wipe commits retargets that generation at the form the library still
 has, and the compensating scan copies the dates and clears them; a
 cancelled request is that failure. The carry upsert is prepared once for the
 snapshot. Both track upserts run one statement. The null-date fill does not order its
 rowid select, which is what lets that select use
 `idx_tracks_first_indexed_at_null`. A path with no recorded date uses
 the scan clock. UPnP-routed rows are not in the snapshot. A folder move
 made outside the bridge, and a file that returns after the missing-scan
 threshold, are re-adds and get a new date. That is the accepted
 behaviour. A case-sensitive volume that holds both spellings of one path
 can hand the new spelling the old date even though the rename reap does
 not retire the old row; that residual is accepted. The wire field is
 `firstIndexedAt`, a `*time.Time` spliced at read time and stripped by
 `marshalForStorage`, omitted until the row has a date. The list, the
 stream and the page splice it through one scan, `scanListedTrack`. `/v1/health`
 advertises `firstIndexedAt` only while no track row is NULL and the
 manifest serves the field, including on a demo bridge; the count is
 cached with the other health counts (ready sticks for the process, a miss
 is rechecked, a timeout is not stored). No `ExtractorVersion` bump and no
 `ProtocolVersion` bump. The `indexed_at` assignment sweep treats
 `first_indexed_at` as a different identifier.
 (`TestV53BackfillCopiesTheStoredMtimeAndLeavesTheCursor`,
 `TestAnUpdateOfTheSamePathLeavesTheFirstIndexedDate`,
 `TestANewPathCopiesTheDateOnlyWhenTheCallerPassesIt`,
 `TestACaseOnlyRenameKeepsTheFirstIndexedDate`,
 `TestASubtreeScanCopiesACaseOnlyRename`,
 `TestAnOutsideFolderMoveInsertsANewFirstIndexedDate`,
 `TestARootFlipCarriesTheFirstIndexedDateAndAFullScanClearsIt`,
 `TestCarryKeyIsTheRootAndThePathWithinIt`,
 `TestAnInFlightScanLeavesTheCarryForThePostFlipScan`,
 `TestAnAddedRootDoesNotInheritTheOldRootsDate`,
 `TestACollapseKeepsTheSurvivorsDateNotTheRemovedRoots`,
 `TestARolledBackNullDateIsFilledOnTheNextOpen`,
 `TestAFlipBackBeforeAnyRescanKeepsTheDate`,
 `TestACollapseDuringThePostAddRescanKeepsBothAlbums`,
 `TestACompensatingScanAfterAFailedSaveKeepsTheDate`,
 `TestTheNullDateFillUsesThePartialIndex`,
 `TestFoldedFirstIndexedUsesThePathIndex`,
 `TestMigrationV52GainsAFirstIndexedDate`,
 `TestManifestServesFirstIndexedAtOnTheFullAndPagedResponses`,
 `TestFirstIndexedAtReadySticksOnceTrueAndRechecksAMiss`,
 `TestAddingARootKeepsTheOldFilesDateAndDatesTheNewRootAtTheScan`,
 `TestCollapsingSeveralRootsKeepsTheSurvivorsOwnDate`,
 `TestLibraryAddRecordsTheExistingRootsFolderName`,
 `TestLibraryRemoveCollapseKeepsTheSurvivorsDate`,
 `TestADecomposedAlbumTakesThePrecomposedDate`,
 `TestASaveFailureAfterAddingARootDropsTheSavedDates`,
 `TestASaveFailureAfterCollapsingRootsDropsTheSavedDates`,
 `TestACancelledWipeDropsTheSavedDates`,
 `TestACancelledSaveAfterTheWipeDropsTheSavedDates`,
 `TestACancelledCollapseKeepsTheDatesTheAddRecorded`.)

- **Every `indexed_at` bump goes through `indexedAtAdvanceSQL`**, which is
  `nextDeltaStampSQL`: the later of the clock and one past each arm of the
  library watermark (`MAX(indexed_at)`, `MAX(deleted_at)`, and the
  deletion-journal coverage start). The clock term anchors to wall-clock
  because the cursor IS wall-clock. Each `+1` clears a stamp that landed in
  the same tick — on Windows that tick is 100 ns wide — and a delta filters
  with a strict `>`, so a stamp equal to a cursor the client already holds
  never arrives. The journal's `deleted_at` and a mass-op coverage start use
  the same expression, inside the write. The coverage start is read BEFORE
  the tombstone wipe: a stamp taken after it no longer sees the `deleted_at`
  the cursor may sit on, `DeltaSinceCovered` treats `since >= start` as
  covered, and the wipe left no tombstones. The track upserts bind it too:
  `readNextDeltaStamp` evaluates `selectNextDeltaStampSQL` once per
  transaction, before any row of that transaction is written, and both the
  fresh insert and the conflict arm store that integer. A 500-row batch
  pays one read. Those two transactions are `BEGIN IMMEDIATE`, so the
  write lock is reserved before the stamp read. A deferred transaction
  that reads and then writes fails `SQLITE_BUSY_SNAPSHOT` when another
  connection commits in between, and `busy_timeout` does not retry that
  code. `s.mu` serializes writers in this process only. The data-dir lock
  covers a second `bridge serve`; `bridge scan` and the other CLIs open
  the same database without it. Store-wide `_txlock=immediate` was not
  taken: the driver applies it to every non-readonly `Begin`, and these
  two are the transactions whose first statement is the stamp read.
  A failed `ROLLBACK` discards the connection (`driver.ErrBadConn` from
  `Conn.Raw`) before it would return to the pool. This driver's `IsValid`
  stays true for a connection left inside `BEGIN IMMEDIATE`, so `Close`
  alone hands that transaction, and its write lock, to the next checkout.
  A failed `COMMIT` whose follow-up rollback also fails marks the
  connection, and `Rollback` discards it after the prepared statement
  has closed. Discarding while that statement is open panics in
  `sqlite3_finalize`. The upserts register `Rollback` first and the
  statement close second, so the close runs first.
  Every row of the commit carries that one stamp, so
  `indexed_at > since` takes the batch whole or not at all: a reader sees
  the commit atomically, and two rows at one value cannot arrive as a
  prefix. `first_indexed_at` stays the scan clock. Two exclusions remain —
  migration v34's `post()`, and `StampExtractorVersionBatch` (not an
  `indexed_at` writer at all). Don't convert those. A bump-only writer uses
  `bumpIndexedAtByPathSQL`. `TestIndexedAtAdvanceIsShared` walks the named
  CONSTS and is blind to an inline literal in a function body — which is
  how #840 reintroduced the dead `CASE WHEN` form — so
  `TestNoHandRolledIndexedAtBump` sweeps every non-test file in the package
  and classifies each assignment against the SQL literal that contains it.
  An `indexed_at` assignment that starts with `CASE` is hand-rolled,
  with or without a space before `=`; `artwork_version`'s `CASE` later
  in the same literal is a different column.
  (`TestNextDeltaStampIsMaxOfTheClockAndOnePastEachWatermarkArm`,
  `TestADeleteInTheSameNanosecondReachesADelta`,
  `TestAMassOpCoverageStartInTheSameNanosecondIsNotCovered`,
  `TestAnIndexedAtBumpInTheSameNanosecondClearsATombstoneWatermark`,
  `TestAMassDeletePublishesAWatermarkThatMovedForward`,
  `TestAnUpsertClearsAWatermarkAboveTheRow`,
  `TestAnUpsertHoldsTheWriteLockBeforeItReadsTheStamp`,
  `TestAFailedRollbackDiscardsTheConnection`,
  `TestClosingAfterAFailedRollbackLeavesTheNextUserInsideTheTransaction`.)

- **A writer that writes back a row it READ earlier writes it only while the
  row is still the one it read: `MarkEnriched` and `applyReconciledTracks`
  compare-and-set on `indexed_at`** (2026-09-29, backlog B187). Both wrote
  back the WHOLE `tags_json` of an earlier read with no check, so whatever
  another writer did in between was overwritten for good. Measured through
  the real Store and Scanner on main 1f784879: a file retagged between the
  enricher's read (`UnenrichedTracks`) and its stamp kept its old title and
  a `size` of 124 inside `tags_json` against 161 on disk, `enriched_at` set,
  through three more scans; a `cover.jpg` added while the enricher worked
  lost the scanner's `local-` art (`ArtworkMBID` "") while `folder_art_key`
  recorded the cover as seen; and a reconcile pass that read a row before a
  stamp and wrote it after took every MBID away, `enriched_at` still set.
  Nothing healed any of it: the skip gate compares the `size` and
  `mtime_ns` COLUMNS, which neither writer touches, the enricher never
  revisits a stamped row, and the phone's exact-size check
  (`validateDownloadedSize`) fails every offline download of such a file.
  **`indexed_at` is the row's version**: every `tags_json` writer moves it
  (the two upserts' conflict arms, the two write-backs through
  `indexedAtAdvanceSQL`), so a write-back carries the version it read
  (`Track.rowVersion`, unexported, never marshaled) and writes `WHERE path
  = ? AND indexed_at = ?`. On a miss it writes nothing. `MarkEnriched`
  answers `ErrTrackChanged`, and the enricher counts nothing, logs one
  Debug line (`msgChangedWhileEnriched`) and leaves the row unenriched for
  its next batch, which reads it again and answers mostly from its caches;
  the pass skips the row, uncounted, and the next scan reconciles it from
  what it holds then. A row deleted in between is a miss too. **Every store
  reader that returns a Track records the version** (`GetTrack`,
  `LookupTrack`'s folded fallback, `UnenrichedTracks`, the list, stream and
  page readers, the UPnP baseline), and `UpsertTrack` and `MarkEnriched`
  the version they wrote. **A Track the store did not hand out is refused**
  (`errTrackNotRead`), never written unchecked: failing open is how a
  reader that forgets the version would reopen the race without a word, and
  a reconcile batch refuses before it writes anything. A writer that bumps
  `indexed_at` without changing `tags_json` (a rendition, lyrics, a booklet
  tag) costs at most a spurious miss, one more enrichment from the caches,
  never a livelock: each bump is a one-off, and no enricher path bumps the
  row it is enriching. **Don't compare the `tags_json` bytes** (older rows
  hold TEXT or BLOB, and it buys only fewer spurious misses), **and don't
  `json_set` only the fields a writer owns**: that stamps an enrichment of
  the old tags onto new ones and marks them done. `TestNoHandRolledIndexedAtBump`
  reads a WHERE clause's `indexed_at = ?` as a comparison, not an
  assignment (`TestTheIndexedAtSweepTellsAnAssignmentFromAComparison`).
  `TestAStampOverARowTheScannerRewroteWritesNothing`,
  `TestAStampOverARowWhoseCoverArrivedWritesNothing`,
  `TestAReconcileWriteOverAStampedRowWritesNothing`,
  `TestAWriteBackOfATrackTheStoreDidNotHandOutIsRefused`,
  `TestAStampRecordsTheVersionItWrote`, and the enricher's
  `TestAStampOverARowThatChangedMidEnrichmentIsNotCounted` and
  `TestASkipOverARowThatChangedMidEnrichmentIsNotCounted`.

- **Any path predicate that writes, deletes, or bounds a scope MUST be a byte
  range, never `LIKE`.** Nothing sets `case_sensitive_like`, so `path LIKE
  'p/%'` matches a case-twin sibling — a DIFFERENT directory on a case-sensitive
  filesystem. Use `path COLLATE BINARY >= ?||'/' AND < ?||'0'`, binding
  `TrimRight(prefix,"/")` twice; **always trim the trailing slash first**, or the
  range becomes `'Album//'` and matches nothing (a silently-empty rollup).
  `subtreeLikePattern` survives only for deliberately case-folding callers — if
  you can't say why folding is wanted, you want `subtreeRangeBase`.
  `DeleteTracksByPrefix` **errors on an empty base**; sidecar enumerations must
  use the SAME bounds as the DELETE.

- **`unicode_lower` does TWO jobs, and only the CASE half is unwanted on a
  destructive path — so the fold stays as the CANDIDATE GENERATOR and the
  strictness moves to the acceptance.** Both queries behind
  `DELETE /v1/upscale/variants` (`ListVariantsForPath`,
  `ListVariantsByPathPrefix`) selected with `unicode_lower` and
  `RunVariantDelete` unlinks every row it is handed, so `?prefix=Jazz` also
  reaped `jazz/` and `JAZZ/` — measured: `selected [JAZZ/c.flac Jazz/a.flac
  jazz/b.flac]`. Byte-exact SQL is the WRONG fix: `unicode_lower` also
  NFC-composes, the scanner stores the on-disk form (NFD from HFS+ or a
  Linux/NAS sync) while clients send NFC, and dropping it answers
  `deletedCount: 0` for every accented album, silently.
  `acceptCaseExactVariants` re-uses the same `NFCCompose` the scalar itself
  calls (`nfcCompose` until 2026-10-02, exported then for the sidecar walks'
  known set, `integrity.KnownSidecarKey`: the B206 bullet under **Job
  pools**), so generator and acceptance cannot disagree about composition and
  acceptance adds case and nothing else. This is the enricher's rule —
  *relaxations in the query, strictness in the acceptance* — on a delete.
  **Every OTHER `unicode_lower` predicate is a READ and already failed closed**
  (exact first, folded fallback, `LIMIT 2`, "refusing to pick a row"); the two
  that unlinked bytes had neither, and the asymmetry was the bug.
  `subtreeLikePattern`'s sanctioned exception named a variant-GC caller **that
  does not exist** — `--gc` drives off `AllVariants` — and the log repeats it
  under a third name that does not exist either. PROTOCOL.md already said "one
  exact source track", so the spec was right and the code was not: no spec
  change, no Mirror-PR.

- **The threshold reap unlinks its sidecars**, and both DELETE arms and both
  sidecar enumerations are derived at compile time from one shared `where`
  const so the unlink set and the row set cannot diverge. The ill-formed-UTF-8
  arm needs its own enumeration. Variant enumeration is strict (abort before the
  CASCADE); waveform is best-effort.
  **`ClearMissingCounts` does the same for the rows it deletes** (2026-10-14,
  backlog B186). Its rendition list, its waveform list and its DELETE embed
  `clearMissingTracksWhereSQL`. The files are removed after the commit. A
  remove the filesystem refuses is logged and left for the orphan sweep, and
  it does not fail the command. `dsd_peaks` has no file; those rows leave by
  the tracks cascade. Leftover renditions are what made the orphan sweep
  refuse: twelve of them beside one remaining rendition is the lost-index
  refusal, and one of them with no variant row left is the empty-catalog
  refusal. Both unlink nothing.
  (`TestClearMissingCountsRemovesTheSidecarsTheDeleteDrops`,
  `TestClearMissingCountsLogsASidecarItCannotRemove`,
  `TestClearMissingSidecarQueriesShareTheDeletePredicate`,
  `TestClearMissingLeavesNoRenditionForTheOrphanSweep`.)

- **A manifest path names its own root — map it with `fs.Resolver`, never by
  joining onto one.** In multi-root mode `relPath` prefixes every stored path
  with the root's basename, so `filepath.Join(roots[0], rel)` addresses a path
  under the WRONG root with the routing segment still in it. `internal/trash`
  hand-rolled exactly that and delete was broken on every multi-root bridge —
  and where two roots' directory names overlap it trashed a real, unrelated file
  and then reaped the manifest row of the file still on disk (`retireAndRescan`
  passes threshold **1**). `Resolver.SplitRoot` returns `(root, suffix)` from the
  same computation that produces the absolute path, past the same containment
  check, under ONE lock snapshot — so `Resolve` and `SplitRoot` cannot disagree.
  A path naming a root this bridge no longer has is a REFUSAL
  (`ErrRootUnavailable`), never a fallback; a caller-supplied root CONSTRAINS
  rather than selects. Anything that joins manifest-form dirs onto a
  caller-supplied root has the same bug one layer up — `spawnBackgroundSubtreeScan`
  did, so a batch spanning roots resolves per-dir instead.

- **A path a CLIENT names is scanned, retired and recorded under the spelling
  its folders list, never the client's: `fsutil.Speller`** (2026-10-02,
  backlog B219). The scanner makes each row's path from the spelling of the
  directory it is handed (B53's bullet under **Job pools**), and a volume
  that opens a name under more than one spelling (APFS: case and Unicode
  composition; NTFS, SMB: case) takes a client's spelling to a folder listed
  under another. Measured on main 7e324642 through the real handlers on
  APFS: an upload into `artist/ALBUM` (the folder `Artist/Album`), or into
  `Café` sent in NFD (listed NFC), landed in the existing folder, and its
  subtree scan indexed every file there a second time under the client's
  spelling, both copies served; a delete of `artist/ALBUM/01.flac` moved the
  track, retired no row (the retire is byte-exact) and indexed the folder's
  other files a second time, and its trash entry kept the client's spelling,
  so the restore did it again; an upload or a delete through a link to a
  directory scanned through the link (`Linked/Album/*` beside
  `Real/Album/*`). The upload commit (`spellScanDirs`, on the planned dirs,
  then planned again: two spellings of one folder are one scan) and the
  trash (`spellRel`: before the move for a delete, after it for a restore)
  map the path first; the B53 rescanner is handed a row's path and needs
  nothing. **Matched by identity, never by a fold of the name**: the entry
  the listing names exactly, else the one `os.SameFile` (both sides
  Lstat'd) says the client's spelling opens; a case-and-NFC fold only picks
  the candidates worth a stat, and a name it does not relate (a Windows
  short name) is still found over the whole listing. A fold alone calls a
  case-sensitive volume's `Artist` and `artist` one folder. **Every
  component above the last must be a directory by Lstat**: a walk does not
  descend a link or a junction, so a path through one has no on-disk
  spelling, and nothing is listed through a link. **A path that cannot be
  spelled is left to a full scan** (the commit's `fullScan`,
  `trash.Result.FullScan`), which finds its files as the walk spells them;
  the trash still retires it under the client's spelling, as before. A path
  spelled as listed costs one Lstat per component (the listing is read in
  directory order and stops at the exact name), and a Speller remembers
  each folder it spelled, so the folders above a path's last component are
  listed once per batch while the folder holding that component is listed
  per path. **Don't respell inside `ScanSubtree`**: its
  deletion scope is a byte range over the spelling it is handed, and the
  watcher and the rescanner hand it the walk's own. Rows an older build
  already wrote heal at the next full scan. The rename reap takes the
  case variants and the composition variants (NFC beside NFD, U+0130
  beside I+U+0307) on that scan: `renameFold` composes first, then the
  Unicode lowercase `pathFold` uses (backlog B267, 2026-10-12). A plain
  "i" beside U+0130 is a different folder and stays for the missing-count
  threshold. Two spellings the walk saw both of stay. An errored subtree
  is spared before the reap. The 2026-10-02 measurement that composition
  variants waited until the third scan described the fold before it
  composed. Tests: `TestScannerReapsTheUnseenCompositionOfOneFolder`,
  `TestScannerLeavesAPlainIBesideDottedCapitalI`,
  `TestScannerLeavesBothCompositionSiblings`,
  `TestScannerSparesCompositionTwinUnderWalkErrorSubtree`,
  `TestAnUploadRescanIndexesNoSecondSpellingOfTheFolder`,
  `TestAnUploadThroughALinkedFolderIsIndexedWhereTheWalkFindsIt`,
  `TestAnUploadOfOneFolderUnderTwoSpellingsScansItOnce`,
  `TestATrackTrashedUnderAnotherSpellingLeavesNoSecondSpelling`,
  `TestARestoreOfAnEntryTrashedUnderAnotherSpellingIndexesNoSecondSpelling`,
  `TestATrackTrashedThroughALinkedFolderLeavesNoRowsUnderTheLink`
  (the spelling variants skip on a volume that tells them apart: both on
  Linux, the composition one on Windows), and the `TestSpeller…` tests over
  a fake volume that folds as APFS does.

- **Two spellings of one name in one folder stay two rows and are REPORTED:
  `bridge doctor`'s `name-collisions`** (2026-10-16, backlog B420). The app
  folds bridge and UPnP shares, with no per-host detection, so the user fixes
  the copies. The check reads `TrackPathsLocal` (no routed row, no library
  walk, no write) and groups each folder's children by `renameFold`: a stray
  folder is one group, and the roots' own folder names are compared too. It
  names the first five groups. **Don't give it another fold.**
  (`TestDoctorNameCollisionsReportsAStrayFolderOnceNotPerTrack`.)

- **A root add or remove is serialised with the scanner by cancelling the
  scan that already read the previous roots and waiting for it to stop
  writing** (2026-10-13, backlog B183). `Scan` and `ScanSubtree` snapshot
  the roots under the scanner lock and keep that snapshot for the walk, so
  a `SetRoots` during the walk does not stop the old paths being written
  back after the wipe or the prefix delete. Holding the scanner lock across
  the change would block the console request for the rest of a multi-minute
  walk, and it deadlocks a scan parked in the extract hook (the hook runs
  while the scan holds the lock). `Scanner.CommitRootChange` bumps an
  epoch, cancels the scan that holds the scanner lock, and waits for that
  lock. A scan registers as in flight only once it holds the scanner lock,
  so a scan waiting on the lock is not the one cancelled: it sees the new
  epoch and returns without reading the roots. Then the wipe or prefix
  delete, the config save and `SetRoots` run. The handler's post-change
  scan starts inside that call and is not cancelled: the finishing scan
  clears the slot with compare-and-swap before it unlocks. A cancelled
  scan reports `context.Canceled` and does not log it as a failed
  background, initial or periodic scan. Rows the
  cancelled scan already committed are removed by the wipe or the prefix
  delete, which run after the scan returns. The request holds the admin
  lock for that drain; it does not hold the store lock while it waits.
  (`TestARootChangeDuringAScanDoesNotLeaveTheOldRows`,
  `TestARootChangeThroughTheHandlerDoesNotLeaveTheOldRows`,
  `TestAWaitingScanDoesNotHideTheScanARootChangeCancels`.)

- **One opener of a database runs its migration ladder at a time** (2026-10-14,
  backlog B185). `OpenStore` holds an exclusive kernel lock on
  `<db>-migrate.lock` (`lockLadder`, opened through `fsutil.OpenLockFile`) from
  before SQLite opens the database until it returns. A second opener waits up
  to `ladderLockWait` (5 min), then reads the `user_version` the first left
  and runs nothing. Two openers of an older database (a `bridge doctor` run
  with the new binary beside the new serve's first start) used to run the
  ladder side by side: each read the version once and ran every step from
  it, and a step that checks before it writes failed the slower one. That was
  the column helper's probe and ALTER ("duplicate column name", 78 of 120
  opens in a probe on a real v0.2.0 database) and, with that tolerated,
  migration 52's epoch row ("UNIQUE constraint failed: backup_epoch.id", one
  run in three). **Don't fix such a race inside a shipped migration or the
  frozen column helper**: the lock serializes every step, those two included.
  **A lock held past the bound fails the open** (`errLadderLockTimeout`):
  proceeding would run the ladder beside a holder that may still be
  migrating. A lock that cannot be taken at all is logged and the ladder runs
  without it, as serve's data-dir lock does. The file is a sidecar like -wal
  and -shm, so a symlink there is refused (`storeOpenPaths`), a link planted
  there after that check too (`lockLadder` answers `errStoreSymlink`, never
  "no locks here"), and it is never removed
  (`TestConcurrentOpensOfAnOlderDatabaseAllSucceed`,
  `TestAnOpenerWaitsForAnotherOpenersLadder`,
  `TestAnOpenerGivesUpOnALadderLockHeldPastTheBound`,
  `TestOpenStoreRefusesASymlinkAtTheDatabaseOrASidecar`,
  `TestOpenStoreRefusesASymlinkSwappedInBeforeTheOpen`).

- **Bump `manifest.ExtractorVersion` on EVERY extraction-logic change** (const,
  must stay ≥ 1 — at 0 the `stored >= current` gate never re-extracts), and keep
  `= excluded.extractor_version` in both upserts. A bump re-extracts the library
  once; the **version-stale diff-guard** (`reExtractUnchanged`) is what keeps the
  client delta bounded to rows that actually changed, and it stamps a file its
  extractor refuses too, or that file is re-read every scan after the bump
  (the B145 bullet above), and a re-read a reconciliation pass would rewrite
  when the passes leave it as it is stored (the B188 bullet above). **Derive that guard's
  merge set by grepping the actual `tags_json` writers, not from what a field
  "looks like"** — `MusicBrainzTrackID` was omitted on the belief it was
  extractor-owned when the acoustic fallback writes it.

- **A string stored in `tags_json` is valid UTF-8, decoded where the format
  says what the bytes are** (2026-10-14, backlog B181). Under go1.26.6
  `json.Marshal` writes a byte that is not UTF-8 as the `\ufffd` escape, and
  the decoded copy marshals that character raw, so `markStampIfUnchanged`
  never matches and every version-stale re-read takes the full upsert. ID3v1
  text is ISO-8859-1 (`latin1String`, the same reading as `diinText`) and a
  RIFF INFO value is Windows-1252 (`windows1252Text`); valid UTF-8 is kept
  in both, so a file that already stored UTF-8 does not change. A DSDIFF
  DIIN chunk was already decoded. An APE tag is not read. Every other text
  field is `strings.ToValidUTF8` before it is stored (`coerceStoredUTF8`,
  on the track a scan writes and again on `marshalForStorage`'s clone).
  The path column stays the bytes on disk; only the clone's path is
  coerced, because that is what the next read compares.   **No
  `ExtractorVersion` bump**: a row that was already valid UTF-8 marshals
  the same. A row that already stores the replacement character is not
  re-read by this change; the next re-read (a later bump, a changed file,
  a cover or a lyrics sidecar) upserts once, because the decoded character
  differs, and the scan after that stamps. Run the stamp test under
  `GOEXPERIMENT=nojsonv2`: since `go.mod` moved to go1.27.2 (2026-10-18,
  B452, B460) `GOTOOLCHAIN=go1.26.6` no longer builds the module, and that
  experiment keeps the encoder that writes the `\ufffd` escape. go1.27's
  default `encoding/json` is built on json/v2 and marshals the invalid bytes
  as the raw replacement, so the stamp half of the bug is invisible there
  (measured 2026-10-18 on go1.27.1, whose `encoding/json/jsontext` is
  1.27.2's too, with the decodes and the coercion reverted: the
  default build failed only the four title checks; `nojsonv2` also moved
  `indexed_at` and reset `enriched_at` on both files).

- **`plausibleDuration` has BOTH ends, and "empty" has three spellings**
  (ExtractorVersion 13, #966). The gate defended the ceiling alone until
  then: `d > 0` admits anything positive, and a forged header reaches
  absurdly SMALL as easily as large — an `mvhd` with
  `timescale = 0xFFFFFFFF, duration = 1` is 2.3e-10 s, finite, positive,
  under a week, stamped onto the wire where nothing re-derives it and the
  phone renders "0:00" instead of the absence it is. The floor is 0.1 s.
  **The docblock matched the code exactly**, which is why neither a test
  nor a review caught it — the gap was in the POLICY — and
  `TestPlausibleDuration_TruthTable` asserted `{"tiny positive", 1e-9,
  true}`, the missing half written down as intended behaviour. A test can
  encode the bug and pass forever. On the IFF side `iffPayloadFits` took a
  declared size of ZERO as fitting (`0 <= physicalSize - offset` holds for
  any bound), and an AIFF SSND body opens with an 8-byte `offset` /
  `blockSize` prefix, so **8 is as empty as 0** — a COMM claiming ten
  minutes beside either one stamped ten minutes. `ssndSoundSpan` NARROWS
  the span past the prefix rather than testing it: subtracting from `size`
  alone leaves the offset on the prefix and weakens the physical bound by
  those bytes. Three unseen-span cases — declares nothing, holds only its
  prefix, cannot hold its prefix — and a positive control, because "reject
  the empty ones" must not become "reject everything".

- **A fix placed IN FRONT of a check can destroy that check's input, and
  the AIFF sentinel is the case** (ExtractorVersion 14, #967).
  `iffPayloadFits` refuses the streaming writer's `0xFFFFFFFF` — v12's
  rule, written for BOTH IFF walkers. v13 then gave AIFF a narrowing step
  ahead of it: `ssndSoundSpan` subtracts the 8-byte SSND prefix, so the
  sentinel arrived as `0xFFFFFFF7`, an ordinary number to the arm that
  refuses `0xFFFFFFFF`. It reaches the unknown-bound case v12 was written
  for (where `iffPayloadFits` fails OPEN) and any file of 4 GiB or more.
  **A small fixture cannot see it** — below 4 GiB the narrowed value fails
  the physical bound anyway — so the pin drives the SPAN, as the
  sentinel's own test does. Refuse the sentinel at the TOP of
  `ssndSoundSpan`, before any narrowing. WAV does not narrow.

- **A range gate does not protect a value computed in a type that can wrap
  INTO the range.** `parseDurationSeconds` did `float64(h*3600+m*60)`:
  `Atoi` accepts the hours of `"1152921504606846977:00:00"` on a 64-bit
  build, `2^60*3600` is a multiple of `2^64`, and the product is **exactly
  3600** — one hour, from an attribute claiming 131 billion years, which
  `PlausibleDuration` then accepts (max int64 lands on -3600 the same way).
  Widen BEFORE multiplying. Found by a bot against the gate the same PR was
  adding.

- **`internal/upnpingest` is the one `Track.Duration` writer outside
  `internal/manifest`, and it passes the same gate** via the exported
  `manifest.PlausibleDuration` — not a second copy, which is the copy that
  drifts. A DIDL `res@duration` is an untrusted header by another name:
  `0:00:00.001` and `10000:00:00` both parse cleanly and both reached
  `tags_json`, the wire and the phone's track list. `walkFieldsEqual`
  compares `Duration`, so an affected routed row re-upserts once and then
  stabilises.

- **Every derived `Track.Duration` passes ONE gate, `plausibleDuration`, and
  the AIFF / WAV walkers add the DFF `payloadFits` rule through one
  `iffPayloadFits`** (ExtractorVersion 11, #935 — MP4 `mvhd`, MP3 Xing / Info /
  VBRI or the first-frame CBR estimate, AIFF COMM frames, WAV `data` bytes over
  `nAvgBytesPerSec`; the v11 docblock in `extractors.go` carries the per-format
  reasoning). Don't stamp a duration at a new site without both. **The fuzzer
  found the first draft's bug within 16 s: a `largesize` mvhd declaring ~2^63
  bytes converted its payload length to a NEGATIVE `int` and sliced a buffer by
  it** — compare a declared length in `uint64` BEFORE any `int` conversion, and
  treat a box declaring past its PARENT as absent (`findAtom` only checks that
  the header sits inside the bound). The crash input is committed as the
  regression seed; run the whole-file fuzz targets for a minute after any
  parser change, before the PR, not after. (And this PR re-learned the "Commit BEFORE the negative control" rule
  recorded further down: a control's `git checkout --` took an uncommitted
  refactor with it. Commit before EVERY control, including the small
  confirmatory one.)

- **`enriched_at`'s sanctioned writers are a closed set**: the enricher, the
  operator "Retry missing" resets (`ResetEnrichedMisses`,
  `ResetEnrichedByArtistMBIDs`, `ResetEnrichedMissesUnderPrefix`), and
  `ResetEnrichedByPaths` (the fingerprint sweeper's narrow, explicit-path form).
  The upsert reset to 0 is load-bearing. Never touch it anywhere else — the
  `WHERE enriched_at = 0` query drives the worker, and a broad reset pushes a
  whole-library delta to every paired device.

- **Per-iteration panic recovery lives INSIDE the per-file closure** in
  `runScanWorker`, not around the loop — a panicking file must skip and let the
  worker continue. A crash found by the extractor fuzz targets is a REAL defect:
  the recover means a panicking file silently never reaches the manifest.
  **Nothing recovers the WALK's callback** (`walkRoot`, `ScanSubtree`), so a
  panic there ends the scan and the process with it: what the walk logs about
  an error goes through `walkErrReason`, which cannot panic. An `*fs.PathError`
  without a cause panics in its own `Error()`, so a guard on the cause does not
  help once the text is taken first, which is where the suggested fix put it
  (Gemini on #1070; measured, the suggestion still panicked).

- **A scan a shutdown stops reports no write the stop cancelled as a
  failure, and the final flush DROPS its batch on purpose**: the skip gate
  redoes exactly what was dropped, and flushing on a context detached from
  the cancel would hold shutdown for rows the next scan writes anyway
  (`TestAScanStoppedInItsFinalWriteLosesNothingTheNextScanCannotRedo`). A
  write that fails for any other reason is still reported. The rule is
  under **The CLI and the serve wiring**. (#1000)

- **Anything walking FLAC metadata blocks SEEKS past a validated PICTURE payload,
  never drains it.** The single-open FLAC path exists because a 5–25 MiB embedded
  cover crossing the wire twice per track halved scanner throughput on NAS-mounted
  libraries; a verdict needs ~30 bytes of header. This is safe ONLY because
  `meta.New` reads the 4-byte header directly from the reader and wraps it in a
  plain `io.LimitReader` — check that still holds before adding a seek anywhere
  else in that walk. A misaligned walk bails fail-open and silently disables the
  allocation guard, so alignment needs its own pin (that guard walked mewkiz's
  framing until 2026-09-29; it walks dhowden's now, by offset: the next bullet). **Verified against
  mewkiz/flac v1.0.14** (2026-09-23): `meta.New` is byte-identical to 1.0.13,
  still reads exactly 4 header bytes through a non-buffering reader, still wraps
  the body in a plain `io.LimitReader` — and upstream's new `readString` now
  type-asserts `*io.LimitedReader` itself, so it depends on the same property.
  **"Anything" was aspirational until then**: there are THREE walks over the
  same handle, and `applyFLACMultiValueArtists` drained. `block.Skip()` ALWAYS
  drains — it checks for an `io.Seeker` and `*io.LimitedReader` never is — so
  "skipped via Skip()" is a claim about ALLOCATION, not I/O. That distinction
  was already drawn and paid for in #165, whose docblock on the STREAMINFO walk
  says `Skip()` "CONSUMES the bytes from the underlying reader, NOT JUST THE
  BUFIO BUFFER"; PR #208 then reintroduced it 30 lines from the walk that does
  it right.   **Latent, not live, and the reason is worth knowing**: until
  ExtractorVersion 24 the walk returned at the first `TypeVorbisComment`, so
  a `Skip()` would only have drained blocks BEFORE it, and the canonical
  `flac`/`metaflac` layout puts the comment block first (measured 0/4
  on real files). The walk now continues through later comment blocks and
  still SEEKS every other block, a PICTURE after the first comment included.
  A fixture in comment-first order proves nothing — the pin puts PICTURE
  first.

- **A picture dhowden reads is guarded by walking the file AS DHOWDEN DOES, at the
  one `tag.ReadFrom` call, for every extension** (2026-09-29, backlog B99).
  dhowden's `readPictureBlock` does `make([]byte, dataLen)` from a 32-bit field
  BEFORE the read that would fail, and the runtime THROWS on the out-of-memory:
  `recover()` cannot catch a throw, so one file takes `bridge serve` down on every
  scan that reaches it. The old guard, `flacPictureBlocksSane`, sat in the `.flac`
  branch alone and walked the blocks by their DECLARED lengths, which dhowden does
  not: it reads a VORBIS_COMMENT or PICTURE block field by field and takes the next
  header from where the fields end. So it passed a PICTURE block declaring 0 or 6
  bytes (its own read of the fields failed, and it failed open), a PICTURE header in
  the unused tail of a VORBIS_COMMENT or PICTURE block, every METADATA_BLOCK_PICTURE
  comment (base64 of the same structure, decoded at the end of every VORBIS_COMMENT
  block and in an Ogg comment packet), and a FLAC- or Ogg-shaped file under any other
  extension (dhowden picks its parser by the first bytes). Measured on main, one
  extraction each: 13 shapes of 50 to 868 bytes allocated about 1 GiB apiece, the
  fuzzer's 40-byte input 4.28 GB. `dhowdenPicturesWithinBudget` (dhowden_picture_guard.go) mirrors
  `ReadFrom`'s dispatch, `ReadFLACTags` and `ReadOGGTags`, sums what dhowden would
  allocate for pictures (a PICTURE block's MIME type, description and data; for each
  METADATA_BLOCK_PICTURE decode, the decoded bytes, the MIME type, the description
  and the data), and refuses the file when the sum passes `pictureBudget`: twice the
  file's size plus dhowden's own 10 MB up-front allowance (a well-formed file needs
  at most 1.5 times its size; the allowance lets a picture truncated within it be
  read and dropped, as it always was). **Count the strings, not only the picture**:
  a `readString` whose length is within that 10 MB allocates all of it up front,
  there or not (`readStringCost`), and the fuzzer broke the first version, which
  counted the picture alone, in under six minutes: a 787-byte file whose picture
  declared a 2.9 MB MIME type allocated 68 MB. **A sum, not a per-picture bound**:
  dhowden's comment map outlives the block, so one METADATA_BLOCK_PICTURE is decoded
  again at every later VORBIS_COMMENT block (41 decodes of a 9 MB declared picture,
  or of a 9 MB MIME length, 387 MB from under 800 bytes). A refused file is one
  whose tags dhowden could not read: a Warn naming the picture kind, the folder-art
  fallback, no tags. **Stop only where dhowden stops**: where it would fail with an
  error the walk may read on (a comment with no '=' past the 67 bytes scanned for the
  key, invalid base64 past the decoded header, an Ogg page with a bad CRC, which the
  walk does not check), which only adds to the sum, and a file dhowden fails on has
  no tags either way. It fails
  OPEN on a read it cannot complete (dhowden's fails there too), I/O errors included.
  **It reads no picture payload**: fields by offset, a METADATA_BLOCK_PICTURE's header
  through base64's streaming decoder, Ogg page headers and segment tables and no
  segment data it does not need (`TestThePictureGuardDoesNotReadAPicturePayload`).
  **Lowercase as dhowden does**: U+212A KELVIN SIGN lowers to 'k', so a key spelled
  with it is decoded (`TestNoRuneLongerThanThreeBytesLowersIntoThePictureKey` pins
  the 67-byte scan). `TestDhowdenStillAllocatesAPictureBeforeReadingIt` fails the day
  dhowden bounds the allocation itself; retire the guard then, and keep the seeds.
  **No `ExtractorVersion` bump**: every picture of a well-formed file fits the
  budget, so no such file's output moves, and a refused file either made dhowden fail
  anyway (a PICTURE block whose data is not there: no tags before or after) or
  declared a METADATA_BLOCK_PICTURE more than 10 MB past its bytes, whose stored row
  keeps the tags it has until the file changes, which a bump would take away. **Every
  whole-file extractor fuzz target carries the allocation property**
  (`fuzzExtractOnce`), so a length that sizes a buffer the file cannot back is a
  crasher, on any platform, with the size in its message.

- **…and an ID3v2 tag dhowden reads is walked the same way, against two bounds**
  (2026-09-29, backlog B101). dhowden gives every repeat of a frame id a key of its
  own by counting up from `id_0` (`readID3v2Frames`), so n copies of one id cost
  n(n-1)/2 map lookups, each building a string: through `tag.ReadFrom`, 4,000
  copies of a 12-byte TIT2 took 337 ms, and 16,000 (192 KB) 6.0 s and 1.9 GB of
  allocation. A 5 MB tag of one repeated frame is an hour of one scan worker, and
  the size field (whose synchsafe bytes dhowden does not mask) admits about
  512 MiB, while `Scan` holds the scanner's mutex.
  `dhowdenID3v2WithinBudget` (at the one `tag.ReadFrom` call, through
  `dhowdenReaderFor`, ReadFrom's dispatch in its order, the "DSD " pointer
  included) and `id3v2TagWithinBudget` (the two `tag.ReadID3v2Tags` calls: the
  DSF extractor and `applyEmbeddedID3`) walk the tag as `ReadID3v2Tags` reads it
  and refuse one past either bound. **The renaming lookups, SUMMED over every id**
  (`maxID3v2RenameLookups`, 2^21: one id 2,048 times): a bound per id lets n ids
  each repeat up to it. **And the frames stored** (`maxID3v2Frames`, 65,536):
  linear, but 1,000,000 distinct 12-byte frames took 3.7 s to extract and held
  160 MB, most of it `populateFromTagMetadata`'s lookups walking the raw map.
  **The renaming bound is held to the allocation property**: the renaming builds
  about 15 bytes a lookup, so at the bound 32 MB, half the 64 MiB
  `extractionAllocLimit` allows any extraction, and a tag the guard passes meets the
  property the fuzz targets assert (the first draft's 2^23 allocated 127 MB against
  a limit of 70 MB; `TestATagTheID3v2GuardPassesMeetsTheAllocationProperty`, which
  skips its allocation check under -race: the race runtime turns the tiny allocator
  off and doubles it). **Walk the reads, never the declared sizes**: dhowden reads
  the next header from where the payload it read ends, and version 3 compression
  takes 4 from the size (a frame declaring under 4 wraps, and `readBytes` then reads
  nothing: `io.CopyN` of a negative count), a version 4 data length indicator
  replaces it, encryption takes 1 (a zero indicator wraps), a version 4 extended
  header counts its own 4 length bytes (a length under 4 wraps), and under
  unsynchronisation every FF 00 is a byte the sizes do not count (the filter's state
  runs across frames). Each is a shape in `id3v2Shapes`, and
  `FuzzID3v2WalkAgreesWithDhowden` fuzzes the walk against dhowden itself: wherever
  dhowden reads a tag, the walk counts what it stores, per id. The loop ends on
  dhowden's `offset` (the header's 10 bytes, the extended header, every DECLARED
  size) against the size field, never on the stream; the one place the walk reads
  on where dhowden may not is the last frame, dropped by dhowden for an id it does
  not know (`validID3Frame`), one frame at most. **It reads no payload without
  unsynchronisation**; with it, where a frame ends depends on every FF 00 before
  it, so it reads through the filter (dhowden reads such a tag one byte per Read
  call). A refused tag is one dhowden could not read: a Warn naming the bound, the
  frame id and the counts, the folder-art fallback, no tags. **No
  `ExtractorVersion` bump**, for B99's reason: no real tag comes near either bound
  (taggers repeat an id a handful of times, chapters a few hundred), and a stored
  row keeps its tags until its file changes. `TestEveryDhowdenReadIsGuarded`
  requires every call of a dhowden reader in the package to sit in a function that
  asks the guards for what it reads, and refuses any other reader;
  `TestDhowdenStillRenamesRepeatedID3v2FramesOneLookupAtATime` fails the day dhowden
  stops counting up, and the renaming bound can go then.

- **An ID3 genre is expanded only while it holds a handful of numeric
  references** (2026-10-12, backlog B246). dhowden's `metadataID3v2.Genre`
  expands one `(n)` reference per pass and copies the string each time
  (`id3v2genre`, unexported; the bridge reaches it only through `Genre()`).
  A TCON of 4,000 copies of `(0)` took 6.5 s, and the unfixed
  `genreFromTag` path 7.8 s. `genreFromTag` is the one call, from
  `populateFromTagMetadata` and from `containerText.applyUnder`. When the
  raw TCON (v2.3/v2.4) or TCO (v2.2) holds more than `maxID3v2GenreRefs`
  (32) `(digits)` groups it returns "" and does not call `Genre()`. A
  count at or under the bound stores `strings.TrimSpace(m.Genre())`, so a
  real tag is unchanged. The vendored module is not patched. **No
  `ExtractorVersion` bump**: a well-formed genre is byte-identical, and an
  over-budget frame was not a genre anyone tagged. The app twin is B239
  (linear in what the parser reads). The app's `GenreNormalizer` joins
  references differently; the bridge still stores dhowden's expansion for
  a normal tag.
  (`TestAnID3GenreOfThousandsOfReferencesFinishesAtOnce`,
  `TestAnOrdinaryID3GenreIsWhatDhowdenReturns`.)

- **An Ogg FLAC stream's tags are read from its own header packets, as a .flac
  file's are** (2026-09-29, backlog B102). dhowden's `ReadOGGTags` looks only for a
  `\x03vorbis` or `OpusTags` packet, and a FLAC stream in Ogg carries its comments as
  a FLAC VORBIS_COMMENT block in a header packet, so an `.oga` (or such a stream under
  any name: dhowden picks its reader by the first bytes) had no tags, and dhowden read
  every page to the end of the file, CRC-checking each, on every extraction (measured
  on main: an ffmpeg `.oga` and a `flac --ogg` one, both tagless, both read to their
  last byte). `oggFLACMetadata` (ogg_flac.go), at the top of
  `extractViaDhowdenFromReader`, finds a FLAC stream's first packet among the leading
  BOS pages (another stream's first page may come ahead of it) and joins that mapping
  packet, from its `fLaC` on, with the header packets after it: the metadata of a
  .flac file, read in place from the file. `readDhowdenTags` (both guards and
  `tag.ReadFrom`) and then `applyFLACMultiValueArtists` read the join, so an Ogg FLAC
  file's tags, pictures, multi-value artists and lyrics are a .flac file's. **The
  header packets end at the first of**: the count the mapping declares, the block
  flagged last, an empty packet or one opening `0xFF` (a frame's sync code: no block
  type is 127), the stream's EOS page, and `maxOggFLACHeaderPackets` (65,535, the most
  the count can declare: with no count and no last flag a stream would hold a
  packetSource per packet). Removing one of these leaves the join, and so the tags,
  unchanged, so each is pinned by how far the extraction READS
  (`TestAnOggFLACStreamIsReadNoFurtherThanItsHeaderPackets`). **An empty PADDING block
  flagged last ends the join** (`oggFLACTerminator`): in a .flac file the first audio
  frame's `0xFF` reads to dhowden as a header flagged last, and the join has no frame,
  so a stream whose last block is not flagged read no tags without it. **It declines a
  stream beside a Vorbis or Opus one**: dhowden reads those (it returns the first
  comment packet it meets), so no file dhowden could read changes. It reads no page
  after the last header packet and no segment data but the first bytes of a packet
  (the fixtures: 360 of 2,117 bytes, 610 of 1,441), and it wants mapping major version
  1, which libFLAC and ffmpeg both require (measured: both refuse 2). **ExtractorVersion
  18** is the tag read. Format facts are ExtractorVersion 25 (the next bullet).
  The iOS app lists no `.ogg` or `.oga` from SMB or the device and takes a bridge
  row's tags as strings.
  `FuzzOggFLACReadsBackTheMetadataItCarries` lays blocks out in pages of any size and
  requires them back byte for byte, and `FuzzExtractOGG` carries Ogg FLAC seeds, both
  picture bombs among them: the guards meet them in the join.

- **An Ogg row carries the format facts its container holds, and an Ogg FLAC
  stream is classed as FLAC** (ExtractorVersion 25, 2026-10-14, backlog B118).
  The `.ogg` / `.oga` branch used to stamp codec `OGG` and nothing else, so
  Vorbis, Opus and Ogg FLAC all arrived with no sample rate, bit depth,
  channel count or duration, and an Ogg FLAC file sat on every lossy list.
  `extractOggWithContext` opens the file once. Vorbis and Opus are read from
  the identification header (Vorbis rate and channels; Opus channels and the
  48 kHz clock, duration minus the pre-skip) and stay codec `OGG`, with no
  bit depth. Their duration is the last page's granule, and that page is
  read from a window of one maximum Ogg page at the end of the file, never
  the whole file: the page must end on the last byte, carry the same serial
  as the first page, a non-negative granule and a matching CRC. Ogg FLAC
  takes STREAMINFO from the join `oggFLACMetadata` already builds, through
  the same `extractFLACFormatFromReader` a `.flac` file uses, plus the
  channel count that reader does not store: codec `FLAC`, so
  `canSetBitsPerSample` writes the depth and `IsLossyCodec` is false, the
  audio MD5 when STREAMINFO carries one, and the duration from the total
  sample count. Vorbis and Opus store the rate and the channels when the
  header parses and a page of that stream ends at EOF with a non-negative
  granule. A duration is stored only when `plausibleDuration` accepts it,
  so a granule of 0 or one past a week leaves the duration unset. A
  truncated or malformed file keeps the codec alone. A library with no Ogg
  file re-reads each version-stale row once and stamps it. SACD ISO virtual
  rows re-expand, as on every bump. `ProtocolVersion` stays 1. The app
  already treats codec `FLAC` as lossless and still drops `bitsPerSample`
  for an `.ogg` or `.oga` extension, and it does not index or play an Ogg
  container the way it plays a `.flac` file, so the codec change is a
  Mirror note and not an app change.
  (`TestAnOggFLACStreamCarriesAFlacFilesFacts`,
  `TestVorbisAndOpusCarryRateChannelsAndDuration`,
  `TestACutOrGarbageOggKeepsTheCodecOnly`,
  `TestVorbisDurationIsReadFromTheTail`,
  `TestOggFormatLeavesFLACMP3AndM4AAlone`,
  `TestScanner_V25_AnOggRowJoinsTheDelta_OtherRowsOnlyStamp`.)

- **An ID3v2 tag's MusicBrainz ids and ReplayGain are read BY NAME, from its TXXX
  frames and the MusicBrainz UFID** (ExtractorVersion 19, 2026-09-29, backlog B116).
  dhowden files a TXXX frame as a `*tag.Comm` under `TXXX`, `TXXX_0`, … (the renaming
  B101 bounds) and a UFID as a `*tag.UFID` under `UFID`, and `stringOf` matches the raw
  map's KEYS, so no MP3, DSF, AIFF or WAV ever gave up TXXX "MusicBrainz Album Id",
  the recording id (the UFID owned by http://musicbrainz.org) or TXXX
  "REPLAYGAIN_TRACK_GAIN" / "…_ALBUM_GAIN": the enricher searched by text for releases
  the file names exactly. The call site's comment and `stringOf`'s docblock said
  otherwise, and the lookup's test (`…StringOfMatchesVorbisAndID3v2Spellings`, now
  `TestStringOfMatchesVorbisAndMP4Spellings`) passed because it keyed a synthetic map
  by the description, which is MP4's freeform shape. `id3v2NamedValues` and `namedValueOf`
  (id3v2_named_values.go) read them under the aliases a Vorbis comment and an MP4
  freeform atom answer, normalised the same way, with the same precedence: per alias
  in order, the raw map, then the named values. **Their order is never the map's**:
  the MusicBrainz UFIDs, then the TXXX frames, each in the tag's order from dhowden's
  suffix, the key breaking a tie. **The UFID answers `musicbrainz_trackid`, ahead of a
  TXXX of that name**, and Picard's TXXX "MusicBrainz Release Track Id" names the
  release's TRACK and answers nothing: `MusicBrainzTrackID` is the RECORDING id (the
  Atlas lyrics tier asks `/v1/atlas/recording/{it}`). **Every writer measured
  (mutagen, so Picard; ffmpeg) ends a TXXX value with a NUL, which dhowden keeps**, a
  2.4 field holds its values NUL-separated, and a later UTF-16 value keeps its byte
  order mark: `id3v2TextValue` takes the first value not empty after trimming space
  and U+FEFF. Kept, the NUL makes the release id no UUID (the enricher drops it with a
  Warn) and a gain no number. **Only these fields, never one ID3v2 gives a frame of
  its own**: the compilation flag (TCMP; the app pins that neither side reads
  TXXX:COMPILATION, `test_TXXXCompilationAndV22TCP_areNotRead`), the composer, the
  conductor, the original year and the tempo stay frame-only on both sides, and the
  app's ID3v2Parser reads its MusicBrainz ids and ReplayGain from TXXX and UFID too
  (`TestAFieldID3v2GivesAFrameOfItsOwnIsNotReadFromATXXX`). The work was on that list
  until B127 made TXXX:WORK the work (the next bullet). **Don't widen it without the
  app**, and don't let `hasAnyRawKey`'s presence gates see a TXXX: they gate
  dhowden's accessors, which read frames (a TXXX "TRACKNUMBER" would make a nil track
  number Some(0)). The app has read these names as this reader does since B126
  (acoseac/1-bit #2024, 2026-09-30); this bullet went on saying the two differed
  there until 2026-10-16. **v19 changes
  only files whose tag carries such a frame**: they take the full-upsert leg (the tag's
  release id replaces one the enricher found by searching, since the re-extract wins
  `mergePostScanFields`; the enricher's cover stays until it runs again), their
  enrichment is re-queued once, and with a release
  id in hand the enricher does not search MusicBrainz for the release but fetches its
  cover (unless the file has local art) and resolves the artist. A tag ReplayGain now
  outranks the analysis loudness spliced in for a file with none, which PROTOCOL.md
  has always promised. No wire change and no Mirror-PR: the four fields exist and the
  app decodes them. The fixtures are real files (`testdata/gen/id3_txxx_fixtures.py`:
  mutagen writing as Picard's `_save` does, in its three encodings and four
  containers, and ffmpeg); `TestPicardsID3v2IdsAndReplayGainReachTheirFields`,
  `TestAnID3v2NameAnswersAsAVorbisOrMP4NameDoes` (each case twenty times: a map-order
  winner flaps) and `TestScanner_V19_ATagNamingItsIDsJoinsTheDelta_APlainID3RowOnlyStamps`.

- **An ID3v2 tag's TXXX:WORK is its work, and its content group (TIT1, TT1) only when
  it holds none** (ExtractorVersion 26, 2026-10-16, backlog B127, the owner's decision).
  MusicBrainz Picard writes the work to TXXX:WORK and the grouping to TIT1 by default,
  and the work to TIT1 and the grouping to GRP1 under "Save iTunes compatible grouping
  and work", deleting TXXX:WORK (`picard/formats/id3.py` at release-3.0.2), while the
  work was read from TIT1 alone: every default-mode MP3, DSF, AIFF, WAV and DFF stored
  its grouping as its work (measured on files mutagen wrote as Picard's `_save` does, in
  both modes). `workOf` takes the first named value called `work`, a TXXX or TXX whose
  description B116's normalisation makes `work` (so case does not matter) and whose
  value is not empty once trimmed (an empty one counts as absent, and a later frame
  answers), and only a tag with none takes TIT1, or TT1 in a version 2.2 tag, which was
  never read before (the app's ID3v2Parser reads TT1). **Don't put TIT1 back in
  front**, and **don't change the reading on one side alone**: the app's ID3v2Parser
  must read the work the same way (its paired PR), or one file is two works. A Vorbis
  comment's WORK is unchanged. The `©wrk` alias never answers an M4A's work atom,
  which dhowden's MP4 reader does not keep, so an M4A's work has never been read
  (measured; not this rule's). **v26 changes only the rows whose work changes**: a
  tag whose TXXX:WORK differs from its TIT1 or stands alone, and a version 2.2 tag
  holding TT1 or TXX:WORK. They take the full-upsert leg (their enrichment is
  re-queued once) and are the iOS delta; every other row is stamped, and SACD ISO
  virtual rows re-expand as on every bump. No wire change and `ProtocolVersion` stays
  1; PROTOCOL.md's classical-metadata coverage line now names TXXX:WORK, so the app's
  mirror copies it.
  (`TestTXXXWorkIsTheWorkAndTIT1OnlyWhenThereIsNone`,
  `TestPicardsWorkIsReadInEveryContainerAnID3v2TagReaches`,
  `TestScanner_V26_ATXXXWorkJoinsTheDelta_AWorkThatStaysOnlyStamps`.)

- **Every dhowden read goes through `dhowdenReadBuffer`, a page at a time**
  (2026-09-29, backlog B117). dhowden's `unsynchroniser.Read` reads its stream ONE BYTE
  per Read call, and the extractors handed dhowden the `*os.File`, so every byte of an
  ID3v2 tag carrying the unsynchronisation flag was a read(2): measured on the dev Mac, a
  10 MB such tag took 3.8 s to extract (6.5 s in a Linux container), and the 512 MiB
  the size field admits would hold a scan worker for minutes while `Scan` holds the
  scanner's mutex. Now 62 ms (72 ms there). Each call (`tag.ReadFrom` in `readDhowdenTags`,
  `tag.ReadID3v2Tags` in the DSF extractor and in `applyEmbeddedID3`) reads the stream
  `newDhowdenReadBuffer` returns and calls its release after. **The buffer is the stream,
  read ahead**: it holds the stream's bytes from where the stream was, answers a Seek
  landing within them without touching the stream (ReadFrom's `Seek(-11,
  io.SeekCurrent)`, dhowden's short skips), hands any other Seek to the stream with its
  result and error, and hands a Read of a page or more, once drained, straight to the
  stream (a cover is not copied twice), an empty Read too. **release is load-bearing**:
  it puts the stream where dhowden's reading got to, so what reads the file after (the
  FLAC multi-value pass, the MP4 genre walk) finds it as before. **The guards still walk
  the bare stream**, before the buffer exists, and `TestDhowdenReadsTheSameThroughTheBuffer`
  is what keeps them seeing what dhowden reads: over every kind of stream dhowden reads
  (the committed Picard, ffmpeg, iTunes and Ogg FLAC fixtures, the B101 shapes, a FLAC,
  an ID3v1-only file, a DSF, unsynchronised tags) it answers through the buffer exactly as
  bare: error, raw map, picture and final offset. **The common path got faster, not
  slower**: dhowden reads a frame's id, size and flags a read each, and those now come
  from memory (through dhowden alone, 50 reads to 1 on a Picard MP3, 104 to 3 on an M4A;
  through `ExtractWithContext`, 95 to 80 µs and 144 to 108 µs), for 4 KB more allocated and
  up to a page more read after a far seek. `TestEveryDhowdenReadIsGuarded` requires each
  dhowden read to take the stream a `newDhowdenReadBuffer` in its function returned, the
  in-memory AIFF/WAV chunk included (one rule, and a buffer costs it nothing), and
  `FuzzDhowdenReadBufferReadsAsItsStreamDoes` holds the buffer to a bare `bytes.Reader`
  over a tape of reads and seeks (a seek window one byte too wide, a control, fell to it
  in 4 s; that input is a seed). No `ExtractorVersion` bump: dhowden reads the same bytes.

- **A tag a container keeps in chunks of its own outranks the path's guess, and a
  DFF's are read where its writers put them** (ExtractorVersion 20, 2026-09-29,
  backlog B140). The scanner fills a track's title, album and artist from its path
  (`fillFromPath`) BEFORE it extracts, and two walkers wrote a field only while it
  was EMPTY, which in a scan is never: the DSDIFF DIIN walk and the WAV LIST/INFO
  walk. Their extractor tests start from an empty Track and passed; B134's
  read-fault test told its DFF versions apart by sample rate because the title
  never moved. **The DIIN walk also read a layout no writer makes**: PR #223 read
  DITI / DIAR as a 1-byte length where DSDIFF 1.5 has a 4-byte big-endian count,
  and took DIAL and DIGN, chunks no specification defines, for an album and a
  genre, so it read nothing from a real DIIN even into an empty Track. Measured
  on a Philips reference DFF given a spec-layout DIIN: ffmpeg 9.0.2 and TagLib
  2.0.2 read its title and artist, MediaInfo 25.04 its title, the bridge nothing;
  on the 1-byte layout ffmpeg read "ring Title" and the others nothing. **And the
  DFF walk skipped the "ID3 " chunk**, which is where mutagen, so Picard, tags a
  DSDIFF file. Now `parseDIINChunks` reads DITI and DIAR as TagLib writes them
  (ISO-8859-1 when the bytes are not UTF-8: TagLib writes 0xE9 for é), the ID3
  chunk goes through `applyEmbeddedID3` (its picture is the cover), and
  `containerText.applyUnder`, once the walk is over, writes the container's own
  text beneath the ID3 tag: the tag answers each field it has a value for and the
  DIIN or INFO the rest, whichever chunk comes first, as TagLib reads a DSDIFF
  file (measured on `testdata/dff/diin_then_id3.dff` and `id3_then_diin.dff`).
  **An ID3 chunk nested in PROP counts too**, where the file holds no root one:
  TagLib 2.0.2 reads that placement (and rewrites a tag it found there in
  place), and a root tag wins whole wherever the chunks sit (`prop_id3.dff`,
  `prop_and_root_id3.dff`; CodeRabbit on #1118). So PROP's cap is 1 MiB plus
  the ID3 chunk cap, behind a fit check: at 1 MiB a nested tag holding a cover
  refused the whole file. **A DIIN holds a title and an artist and no album**:
  TagLib drops an album set on its DIIN tag when it saves, EMID is an opaque id
  and MARK a position in the audio. A DIIN or ID3 chunk the file ends inside is
  never allocated
  (`readDFFTagChunk`, the payloadFits rule): the walk ends there with the format
  stamped, where a truncated DIIN failed the file (indexed by name alone, no
  sample rate). **Don't write a path-guessed field only while it is empty**:
  `TestNoExtractorFillsAPathGuessedFieldOnlyWhenEmpty` fails on a comparison of
  Title, Artist or Album with "" anywhere in the package but `fillFromPath` and
  `mergePostScanFields`, and `TestScanner_EveryFormatsOwnTitleOutranksThePathsGuess`
  scans one file of every extractor family, since a test on an empty Track cannot
  see this. The fixtures are real writers' output (`testdata/gen/dff_tag_fixtures.sh`:
  TagLib 2 and mutagen in a Debian 13 container). The iOS app reads no DFF tags
  (`MetadataExtractionRoute` gives `.dff` no extractor and `DFFHeadScan` types the
  file only), so there is no Mirror-PR; an app reader added later takes this
  precedence (backlog B149). v20 changes only rows of files carrying such text.

- **A compressed AIFF-C or WAV is named by its encoding, and counted lossy**
  (ExtractorVersion 21, 2026-09-29, backlog B124, B154). The AIFF walker named
  every AIFF-C "AIFF" and the RIFF walker every WAV "WAV", names on every lossless
  list (librarycat's quality sets, the CarPlay optimize gate, the app's), so a
  µ-law AIFF-C at 44.1 kHz with no depth was filed CD Quality and an ADPCM WAV or
  IMA4 AIFF-C at 96 kHz Hi-Res and a CarPlay render candidate (measured on
  afconvert's, ffmpeg's and sox's files). `aifcEncodingOf` names an AIFF-C by its
  COMM compression type and `wavEncodingOf` a WAV by its fmt format code (or an
  extensible header's subformat: ffmpeg writes one for every ADPCM WAV above
  48 kHz), as the app names them since its #2014 and #2028: the linear types keep
  "AIFF" or "WAV" and their depth; the compressed ones are "ULAW", "ALAW" and
  "IMA4" in an AIFF-C and "ADPCM", "GSM", "ALAW", "ULAW", "MP3" and "MP2" in a
  WAV, with no depth. **An AIFF-C the bridge cannot read is "AIFC"** (an unknown
  compression, or no COMM), the app's name for it, on neither list, with no depth
  and no duration; **a WAV of a format code it does not name keeps "WAV" and loses
  its rate** (default-deny, as for a DFF of an unknown compression: with the rate,
  the lossless name claims a tier). **An IMA4 COMM counts 64-frame packets**: the
  duration was 64 times too short (a 30 s file read 0.4688 s). **The first COMM
  and fmt chunk the walk can read names the file**, a later one skipped whole, as
  TagLib (and ffmpeg, for a WAV) reads it and as the walkers read their first SSND
  and data chunk: parsed one on top of the other, a second chunk's codec landed
  beside the first one's depth (CodeRabbit on #1122). A WAV of code 0x0050 is
  "MP3" where its MPEG1WAVEFORMAT `fwHeadLayer` names layer III, read under that
  tag alone (an extensible header's subformat has other fields there). **The six names are
  one vocabulary in four places**, `manifest.IsLossyCodec`, its SQL mirror in
  `upscaleEligibleSQL` (the admin lockstep matrix pins the pair), librarycat's
  `lossyCodecs` and the dupes ranking set; a new compressed name joins all four.
  The dupes lockstep test checked one direction until then, so a name reaching
  `IsLossyCodec` alone passed it. The app's lossy list lacks "MP2" (backlog B158).
  `.aifc` is in the DLNA MIME table, the UPnP walker's audio set, the ingest's
  codec table ("AIFC": a DIDL cannot say what one holds) and the console player's
  MIME and playability tables (a compressed WAV plays engine-dependent there).
  **The optimize gate's codec-empty extension fallback leaves `.aifc` out on
  purpose**, in Go and SQL alike: the extension cannot say whether one is
  compressed. v21 changes those rows, and a linear AIFF-C of a type the old depth
  list lacked (42ni, FL32, FL64), which gains its depth.
  `TestACompressedAIFCOrWAVIsNamedByItsEncoding` (twenty real files,
  `testdata/gen/compressed_pcm_fixtures.sh`), `TestAIFCEncodingOf`,
  `TestScanner_V21_ACompressedAIFCOrWAVJoinsTheDelta_ALinearOneOnlyStamps`.

- **Extraction: presence-gate the integers, refuse bit depth on lossy codecs, and
  keep the work apart from TIT2→Title** (the work is TXXX:WORK, else TIT1: the B127
  bullet above; this said "split TIT1→Work" until then). dhowden returns 0 for both
  "tag absent" and "an explicit 0", so Year/TrackNumber/DiscNumber need a raw-map presence check to
  keep `Some(0)` distinguishable from `nil`. `canSetBitsPerSample`, an allowlist
  of the lossless names, gates every extractor's `BitsPerSample` write
  structurally (this said `isLossyCodec`, a denylist #225's review replaced
  because it failed open on an empty codec, until 2026-09-29). `Year()==0` (and a
  Vorbis 1, the next bullet) falls back to `rawDateYear`, `parseYearPrefix` over
  each raw date tag in turn — a valid ISO `2023-06-09` otherwise
  indexes as year 0. `stringOf` asks the requested aliases in the caller's
  order, so DATE before YEAR stays stable. Two raw keys that normalise to
  one of those aliases are chosen by the next bullet.

- **Two raw keys that normalise to one `stringOf` alias are the first non-empty value in ascending raw-key order**
  (2026-10-14, backlog B182). `stringOf` walked the raw map for each alias
  and returned the first key whose `normaliseRawTagKey` matched. Go 1.26
  starts every `range` at a random slot (`Iter.Init` sets `entryOffset`
  and `dirOffset` from `rand()`), so fifty extractions of one two-key map
  saw both values: MP4 `REPLAYGAIN_TRACK_GAIN` beside
  `replaygain_track_gain` (47 and 3), Vorbis `MusicBrainz Album Id` beside
  `MUSICBRAINZ_ALBUM_ID` (42 and 8). The comment beside the loop called
  that order deterministic, which held across aliases only.
  `stringOfAlias` collects every matching key, sorts those raw keys, and
  returns the first value `coerceRawTagValue` accepts. A value that trims
  to nothing leaves the next key. Ascending byte order is the same
  tie-break `id3v2NamedValues` uses on the key. An exact-case canonical
  key was declined: it would crown one tagger's spelling, and no field
  has a single canonical spelling. A file with one matching key is
  unchanged, so `ExtractorVersion` stays 24. The other raw-map ranges
  were already decided: `hasAnyRawKey` is presence,
  `extractMultiValueTagFromRaw` keeps the longest list then the
  lexicographically smaller slice, `id3v2NamedValues` sorts before it
  walks, `stripMP4FreeformLocales` rewrites every value, and lyrics
  `Pick` is a total order. `namedValueOf` and `rawDateYear` go through
  `stringOf`.
  (`TestTwoSpellingsOfOneAliasReadOneValueOnEveryExtraction`,
  `TestAnEmptyEarlierRawKeyLeavesTheLaterValue`,
  `TestOneSpellingOfAnAliasReadsTheValueItCarries`.)

- **dhowden's Vorbis reader answers year 1 for a DATE it cannot parse, so a
  Vorbis 1 is re-read like a 0** (ExtractorVersion 22, backlog B222, B234).
  `metadataVorbis.Year()` (FLAC, Ogg Vorbis, Opus, Ogg FLAC: every reader whose
  `Format()` is `tag.VORBIS`) picks a `time.Parse` layout by the DATE's length
  (4, 7 or 10 bytes) and drops the error, so any other length or a value the
  layout refuses is the zero time's year: an ISO timestamp
  (`2017-01-27T12:00:00Z`, store downloads), `1974?`, `2019/03/22`, and a DATE
  holding no year (`unknown`, `22.03.2019`) all stored 1, which the year pass
  never fills (it is positive), so the phone showed year 1 and split the album
  (year is in its album identity). `rawDateYear` re-reads it: the first tag in
  `yearTagAliases` order that `parseYearPrefix` reads, else 0 (present, no
  year: the pass fills it from the folder). **Each tag in turn**, not the first
  present: `DATE=22.03.2019` beside `YEAR=2019` reads 2019, as the app's
  `VorbisCommentParser` does. **Only the Vorbis reader**: ID3v2's `Year()` is
  `strconv.Atoi` of the whole value or a `DateOnly` parse, else 0, and MP4's
  `Atoi` of the first four bytes, 0 when that fails, so neither makes a 1 the
  tag does not say; don't widen the rule to them. A literal `0001` still reads
  1. The app reads `Int(value.prefix(4))` and has no year-1 gap (no Mirror-PR).
  Measured: on main 24 of 40 `Extract` subtests (four formats, six shapes)
  stored 1, and the v22 upgrade scan moved only the rows whose year changes
  (an `unknown` DATE took its sibling's 2019 through #1128's hold) and stamped
  the rest (`TestADateDhowdenCannotParseIsReadByItsYearPrefix`,
  `TestAnID3OrMP4DateHoldsNoYearOne`,
  `TestScanner_V22_AnUnparseableDateJoinsTheDelta_AReadableOneOnlyStamps`).

- **Explicit content reaches the wire as `explicit`**
  (ExtractorVersion 23). Any signal wins. An MP4 `rtng` of 1 or 4, which
  dhowden's atom map skips; `ITUNESADVISORY` or `EXPLICIT` as an MP4
  freeform atom of any mean, a Vorbis comment or an ID3v2 TXXX, valued
  `1`, `4`, `true`, `yes`, `explicit` or `e` after trim and case-folding;
  or a marker on the raw track title (`©nam` / `TITLE` / `TIT2`), before
  display cleaning: `[E]`, `[Explicit]` or `[Explicit Version]` anywhere,
  or a title that ends in `(Explicit)` or `(Explicit Version)`. The field
  name is exact, ignoring case, with no separator folding
  (`ITUNES_ADVISORY` is not read). A present `rtng` of 0 or 2 does not
  cancel another signal. Within one field the first value wins; the two
  fields are asked separately. A FLAC comment block is read in order,
  because dhowden's map keeps the last duplicate (Ogg Vorbis and Opus
  still take that last value). The title is the title tag when one was
  read, and the filename stem when it was not. Clean forms, the bare
  word and an album title are not explicit. `ITUNESRATING` and `rating`
  are not read. A UPnP row stays unset (the client applies the title
  markers to the DIDL title, and to a raw wire title when an older bridge
  omitted the field). `omitempty`, not a pointer. Persisted in
  `tags_json`, like `Compilation`; not a column, and `marshalForStorage`
  does not zero it. The verdict is one function, `ExplicitVerdict`. The
  shared rows are `testdata/explicit-verdict-cases.tsv`
  (`TestExplicitVerdictCases`, the table shared with the app's
  `ExplicitContent.verdict`); `TestExplicitVerdict` keeps the combination
  cases that file does not. Nothing shipped at 23 before the title
  markers, the filename stem and the first Vorbis value were included, so
  the v23 re-extract is the backfill: a tagged row, a title-marker row and
  a filename stem that carries a marker join the delta, a plain row only
  stamps (`TestScanner_V23_AnExplicitRowJoinsTheDelta_APlainRowOnlyStamps`,
  `TestScanner_V23_ATitleMarkerJoinsTheDelta_APlainTitleOnlyStamps`).
  SACD ISO virtual rows re-expand as on every bump. `ProtocolVersion`
  stays 1.

- **A PCM track's sample kind (`Track.SampleFormat`) is what its header
  states, and nothing else** (2026-10-23, backlog B513). `"integer"` or
  `"float"`, set by the reads that see the header: `parseWAVFmtChunk`
  (`wavSampleFormat`: `WAVE_FORMAT_PCM` integer, `WAVE_FORMAT_IEEE_FLOAT`
  float, `WAVE_FORMAT_EXTENSIBLE` by its subformat GUID compared whole, 16
  bytes, against `KSDATAFORMAT_SUBTYPE_PCM` and
  `KSDATAFORMAT_SUBTYPE_IEEE_FLOAT`, anything else none),
  `parseAIFFCOMMChunk` (`aifcEncoding.sampleFormat`: a plain AIFF and `NONE`,
  `twos`, `sowt`, `raw `, `in24`, `42ni`, `in32`, `23ni` integer; `fl32`,
  `FL32`, `fl64`, `FL64` float; compressed and unknown types none),
  `extractFLACFormatFromReader` (FLAC and Ogg FLAC, integer-only) and
  `extractMP4WithContext` (an `alac` sample entry, integer-only). Every other
  format, a header that could not be read, and a UPnP-routed row carry none.
  The codec walk names a WAV by the GUID's first two bytes (`wavEncodingOf`,
  TagLib's reading); the kind is the stricter reading, so the Ambisonic
  B-format subformat (first two bytes PCM's) is "WAV" with a depth and no
  kind. **Don't read an extensible WAV's kind from the GUID's first two bytes,
  don't default an unknown to integer** (absent is unknown on the wire),
  **don't change `codec` or `bitsPerSample` with it** (a float WAV is "WAV" at
  32 or 64 bits, clients match the codec by equality), **and never put a kind
  on an isDSD row**: three guards, the write (`sampleFormatColumn`), the
  splice (`spliceSampleFormat`) and the open-time fill (`COALESCE(is_dsd, 0) =
  0`). (`TestSampleFormatIsWhatTheHeaderStates`, 56 shapes, the captured
  ffmpeg and afconvert headers among them; `TestAIFCEncodingOf`;
  `TestADSDRowNeverCarriesASampleFormat`, whose three halves each went red
  with its guard removed.)

- **…and it lives in `tracks.sample_format` (v59), spliced by the readers and
  never in `tags_json`** (2026-10-23, backlog B513). The `firstIndexedAt`
  shape: `GetTrack` and `scanListedTrack` splice the column,
  `marshalForStorage` zeroes the field, both upserts bind
  `sampleFormatColumn(t)` and the conflict arm assigns it unconditionally (the
  row runs because the file changed), and the version-stale stamp leg records
  the kind its re-read found (`writeSampleFormatTx`), advancing `indexed_at`
  through `bumpIndexedAtByPathSQL` only when a client would see the change
  (NULL and `''` both read as absent). A refused file keeps its kind, as it
  keeps its lyrics. NULL is a row nothing has decided; `''` is decided, no
  kind. **Don't move it into `tags_json`**: the version-stale diff
  (`markStampIfUnchanged`) compares `tags_json`, so every row gaining a kind
  would take the full upsert and re-queue its enrichment (the control that
  left it there turned the stamp test red on exactly that), and a backfill
  writing `tags_json` without advancing `indexed_at` breaks `rowVersion` (an
  enricher stamp read before it writes the old blob back). **Don't leave it
  out of a track write or the stamp leg**: the three production upsert
  callers (the scan writer, `settleHeldReconciles`, the UPnP ingest) write
  fresh extractions or routed rows, and a new caller that writes back a Track
  read without the splice would clear the kind.
  (`TestSampleFormatRidesItsColumnAndNeverTagsJSON`,
  `TestTheVersionStaleStampRecordsTheKindItReads`.)

- **…and a kind carries the stat it was read at, so an older binary's
  rewrite leaves a kind every open clears** (2026-10-23, backlog B513, round
  3). `sample_format_size` and `sample_format_mtime_ns` (v59) are the size
  and mtime of the file the kind was decided from, written with every kind:
  both upserts bind the row's own `size` and `mtime_ns` again, the stamp leg
  writes them (and rewrites a right kind recorded at another stat), and the
  fill and the header pass copy the row's. A writer that does not know the
  columns, the upsert of a binary from before them run after a rollback,
  rewrites `size`, `mtime_ns` and `tags_json` for a changed file and keeps
  the kind, which then describes the bytes before; both binaries stamp
  `ExtractorVersion` 27, so the skip gate never read such a file again (both
  round-3 reviews, reproduced with main's upsert captured in
  `rolledBackUpsertSQL`). `clearStaleSampleFormats` runs at the end of every
  `migrate`, before the fill: a row whose recorded stat is not its own is
  undecided again, read through `idx_tracks_sample_format_stale` (its
  predicate, verbatim), which holds no row on a store this binary alone has
  written (57 µs at 50,000 rows). **Never write `sample_format` without
  `sample_format_size` and `sample_format_mtime_ns`, and don't let the fill
  run before the open's clear.**
  (`TestARollbackLeavesNoKindItsHeaderDoesNotState`,
  `TestEveryWriteOfAKindRecordsTheStatItWasReadAt`,
  `TestEverySampleFormatWriteRecordsItsStat`, which reads the package's SQL.)

- **…and the rows extracted before it get it without a rescan: every open
  decides what a codec decides, and the header pass reads each WAV and AIFF
  row's format chunk** (2026-10-23, backlog B513; the fill's paging, round
  3). `backfillSampleFormatByCodec` runs at the end of every `migrate`, after
  the open's clear (`sampleFormatFillSQL`, 2,000 rows a transaction): a FLAC
  or ALAC row read from a file and not DSD is `'integer'`, every other row it
  reaches `''`; it reads no file. Each chunk starts after the last path the
  one before decided (`UPDATE … RETURNING path, sample_format`), so a fill
  reads each undecided row once: until round 3 every chunk started again at
  the front of the partial index, where the header pass's rows stay, and read
  them again (W × ceil(F / 2,000); 80,000 rows half of them WAV, 525 ms
  against 218 ms). `Store.BackfillSampleFormats` reads the undecided WAV and
  AIFF rows read from a file, 200 a page in path order: the API's resolver,
  the row's size and mtime against the file's before and after the read,
  `openAudioFile`, and `readHeaderSampleFormat`, which walks the chunks with
  the walkers' own `fmt `/`COMM` size bounds (`wavFmtChunkMin`… package consts
  both share) and seeks over the rest; it writes a page in one transaction
  under `Store.mu`, each row only while it is still undecided and still has
  that stat (`sampleFormatDecideSQL`). When either moves `indexed_at` is the
  next bullet's. Measured: the fill over 50,000 rows about 205 ms, once;
  2,000 WAV headers 113 ms on an SSD; a pass with nothing to read 35 µs (the
  record, 2026-10-23). **Don't stamp a routed row by its codec** (its codec is
  its upstream's file name and no header is read), **don't read past the
  format chunk** (a header pass that read the chunks it passed read 2 MiB for
  a header), **don't record a kind from a file whose size and mtime are not
  the row's** (the scan that re-reads it writes the kind), **don't let the
  header read and the walkers disagree on which chunk names the file**
  (`TestTheHeaderReadAgreesWithTheExtractor` runs all 45 WAV and AIFF shapes
  through both), **and don't start a fill chunk anywhere but past the path
  the last one ended on.**
  (`TestTheOpenFillDecidesWhatACodecDecidesAndLeavesTheCursor`,
  `TestTheHeaderBackfillReadsOnlyTheUnreadWAVAndAIFFRows`,
  `TestTheHeaderReadStopsAtTheFormatChunk`,
  `TestTheSampleFormatQueriesUseThePartialIndex`,
  `TestTheOpenFillVisitsEachUndecidedRowOnce`, and one test per guard of the
  header pass's write: `TestTheHeaderPassLeavesTheKindAScanWroteMidPass`,
  `TestTheHeaderPassLeavesARowWhoseStatMovedMidPass` and
  `TestTheHeaderPassReadOfAFileReplacedAfterItsStatIsChanged`.)

- **…and the backfill is delivered once: nothing it writes moves
  `indexed_at` before a header pass has run to its end, which records the
  completion mark the health key reads, and every kind it records after the
  mark moves it** (2026-10-23, backlog B513, round 3). Round 2's header pass
  advanced every row it gave a kind, so a library of WAV files sent a delta
  that size beside the one full sync (the Codex review). Now a pass that
  reads its last page records `sample_format_backfill_complete_ns` in
  `scan_state` (`markSampleFormatBackfillComplete`, an `INSERT OR IGNORE`
  only while no row the fill decides is undecided), and `SampleFormatReady`
  is that row: the health key appears only then, and the clients' one full
  sync carries every kind recorded before it (wire.md). A pass a shutdown
  stops, or one that fails, records nothing, and the next pass starts again;
  the rows it decided stay decided, so the rows hold the progress and the
  mark is the only other state, and it survives a restart. A file the
  finishing pass could not read does not hold the mark back. After the mark,
  a kind the fill or the header pass gives a row moves its `indexed_at`
  (`advanceGainedSampleFormatsTx`, through `bumpIndexedAtByPathSQL`, in the
  transaction that writes the kind): the full sync is once per bridge, and a
  key that comes back after a rollback starts none, so a rollback's rows
  (cleared or inserted) and a file read late reach a client only by a delta.
  A row decided as having none never moves. **Don't let the fill or the
  header pass move `indexed_at` before the mark, don't leave a kind they give
  a row after it unmoved, don't record the mark from a pass that did not
  reach its last page or while a row the fill decides is undecided, and never
  remove it.**
  (`TestTheBackfillMovesNoCursorAndTheKeyWaitsForAPassToItsEnd`,
  `TestSampleFormatIsReadyOnlyOnceEveryRowTheFillDecidesIsDecided`,
  `TestARollbackLeavesNoKindItsHeaderDoesNotState`.)

- **B513 changed extraction without an `ExtractorVersion` bump, and that is the
  bounded exception, not a precedent for a `tags_json` field** (2026-10-23).
  The bump rule exists so the skip gate re-extracts rows whose stored answer
  is stale, at the cost of re-opening every file once. The sample kind lives
  in a column the version-stale diff does not compare, and its own backfill
  (the bullet above) reaches every row extracted before it, reading only the
  WAV and AIFF headers, so the bump would have bought nothing but that cost,
  which the owner's brief ruled out. A later change to what the kind reads IS
  a bump: the stamp leg then records the new answer for every unchanged file.
  `ExtractorVersion`'s docblock says the same at the const. **Don't skip a
  bump for a field the version-stale diff compares**: such a field has no
  backfill but the bump.

- **A repeated Vorbis comment for GENRE, ALBUM, COMPOSER, CONDUCTOR or
  WORK is joined with `"; "`** (ExtractorVersion 24, backlog B328).
  dhowden's map keeps the last value, so a FLAC the app reads as
  "Rock; Pop" arrived from the bridge as "Pop". The join is
  `VorbisCommentParser.MultiValueFields`: empty values (whitespace-only
  after `strings.TrimSpace`, the mirror of `.whitespacesAndNewlines`)
  are skipped before they count, comment order is kept, there is no
  de-duplication, each comment block keeps 64 values, and a later block
  is appended with the same `"; "`. The walk is
  `applyFLACMultiValueArtists`, which now continues past the first
  comment block (a non-Vorbis block is still seeked, a parse error
  applies what an earlier block collected, and no comment parsed leaves
  the explicit verdict populate recorded). One non-empty value overrides
  dhowden, including when dhowden's last value was empty. **TITLE is not
  joined**: `t.Title` stays dhowden's last value, and the first
  non-empty TITLE is only the explicit verdict's. **ARTIST and
  ALBUMARTIST stay uncapped** (the app caps them at 64 per block); later
  blocks now join them too, because the same walk continues. A plain Ogg
  Vorbis or Opus file still takes dhowden's last value; Ogg FLAC is the
  joined metadata and takes the same walk. ID3 and MP4 are unchanged.
  A joined album string is one album title: `dupes.Resolve` and
  `albumID` use the whole string, and the `"; "` split is artists only.
  `genreSegments` splits that string on `;`, so "Rock; Pop" is two genre
  groups; `composerSegments` splits on `;` and `/` as well. Conductor
  and Work are stored whole. A row whose joined field differs takes the
  full upsert; every other row is stamped. SACD ISO virtual rows
  re-expand, as on every bump. `ProtocolVersion` stays 1. No wire-field
  rename, so no Mirror-PR.
  (`TestVorbisJoinsMatchTheAppsMultiValueRule`,
  `TestScanner_V24_AJoinedGenreJoinsTheDelta_ASingleGenreOnlyStamps`.)

- **`normaliseRawTagKey` canonicalizes a LEADING `0xA9` before `ToLower`.**
  dhowden surfaces MP4 ilst atoms under a single-byte `\xa9day` key, which is
  invalid UTF-8, and `ToLower` rewrites it to U+FFFD — so source-literal `©day`
  aliases never matched and year / composer / multi-value artist were silently
  dropped for every M4A. Don't reintroduce a raw-byte alias; it would be compared
  against the mangled key and still miss. **Tests must use the real `\xa9…` byte
  keys** — UTF-8 `©ART` fixtures match the alias without reproducing the bug.

- **dhowden/tag does not read `gnre`, and keeps the data-atom locale on every
  MP4 freeform value** (ExtractorVersion 15, from a report that Rock / Pop listed
  only DSD). iTunes and Music write a STANDARD genre as `gnre` (uint16, ID3v1
  index PLUS ONE) and only a custom genre as `©gen`; `extractMP4PredefinedGenre`
  reads it when no text genre was found, from `id3v1Genres` — dhowden's table
  VERBATIM, pinned index by index against dhowden's own `"(n)"` expansion so an
  M4A `gnre` and an MP3 numeric TCON name one genre (the iOS
  `GenreNormalizer.id3GenreTable` mirrors the same table). dhowden's
  `readCustomAtom` slices a `data` body past the type indicator but not the
  locale, so every `----` value arrived `"\x00\x00\x00\x00<value>"` —
  NUL-prefixed MusicBrainz ids on the wire, unparseable ReplayGain / original
  year. `stripMP4FreeformLocales` removes it ONCE, right after `tag.ReadFrom`,
  before any reader of `m.Raw()`; only a value STARTING with the locale is
  touched (standard atoms never carry it). Two premise tests fail the day
  dhowden fixes either — delete the workaround then, not before. **The moov
  search is bounded by the FILE SIZE, never a byte span**: `findMoov` stopped at
  4 MiB until v15, costing every moov-after-mdat M4A (ffmpeg's default) its
  codec, rate, bits and duration. The M4A fixtures in `testdata/m4a` are
  byte-identical to the iOS app's `AVTagFixtures`; regenerate both or neither.
  The sixteen `explicit-*.m4a` files are a separate set, byte-identical to the
  iOS app's `ExplicitAdvisoryFixtures`; that copy is canonical, and so is
  `testdata/explicit-verdict-cases.tsv`.

- **`skipID3v2` walks a STACK of prepended ID3v2 tags, at most
  `maxStackedID3v2Tags` (8), not only the first** (ExtractorVersion 16, from the
  iOS app's FLAC follow-up to #1935). A tagger that prepends a new tag without
  removing the old leaves two, and Core Audio plays such a FLAC (measured
  2026-09-25); stopping after one left the cursor on the second, so the fLaC
  check failed ("bad magic ID3") and the track lost its rate, depth, duration
  and multi-value artists, and an MP3's frame search began inside a tag. Its
  iOS twin, `FLACStreamInfo.magicOffset(reading:)` (the engine's STREAMINFO
  read and the scanner's `streamStart` share it), has the same cap and the same
  v2.4-only footer, and also refuses a size byte that isn't synchsafe (Core
  Audio refuses that file) where this masks it, as it always has. **Don't stop
  after one tag, don't drop the cap, and don't change one side's walk without
  the other.**

- **`Track.Compilation` puts the file's TCMP / `cpil` / COMPILATION flag on the
  wire** (ExtractorVersion 17, for the iOS app's "hide artists who appear only on
  compilations"). It is set by the SAME `stringOf(raw, "tcmp", "cpil",
  "compilation") == "1"` read that fills AlbumArtist "Various Artists" for a
  flagged file with no album artist, so the flag and that fill can never
  disagree about which files are compilations; the iOS enrichers apply the same
  "only 1 counts" rule to local files. The fill's condition did not change — a
  flagged file with a TAGGED album artist (a DJ mix, a label sampler) keeps it,
  and was indistinguishable from an ordinary album on iOS until v17. **Don't make
  it a `*bool` or emit `false`**: `omitempty` keeps every unflagged row
  byte-identical to its pre-v17 form, which is what limits the v17 re-extract's
  delta to the flagged files (plus the SACD ISO rows every bump re-expands).
  Those take the full-upsert leg, so their enrichment is re-queued once
  (`mergePostScanFields` keeps their MBIDs and art meanwhile). An app older than
  the field consumes that delta and drops it, so the iOS app's first sync after
  it gains the field reaches back to 2026-09-27, once per bridge.

- **…and on macOS every watch is an open file, so the watcher keeps to a
  budget and releases what fsnotify keeps** (2026-10-02, backlog B212).
  fsnotify watches through kqueue there, never FSEvents (the watcher's
  docstring and the doctor said FSEvents until then): asked to watch a folder,
  it opens the folder and every entry in it, files included (measured: 132
  descriptors for 12 folders and 120 files). They come out of the open-file
  limit every socket, SQLite file and pipe comes out of, so a library past it
  broke every later open and accept, the periodic scan's included, and "the
  periodic scan covers the gap" was false there. **Two layers, as on Linux**:
  `Run` counts the library first (`CountWatchSet`, the walk doctor counts
  with) and stays off, with one warning naming the counts and the limits, when
  it needs more than the budget (`WatchFDLimits.Budget`: half of the effective
  RLIMIT_NOFILE, which Go raises at start to min(hard−1,
  kern.maxfilesperproc), 61,440 by default, and a quarter of kern.maxfiles at
  most); a library that grows past it, or a process that runs out of files
  while the watcher adds watches, has every watch released (`stopWatching`,
  fsnotify's Close) with one warning, and `Run` then waits for its context, so
  a scan an event already asked for still runs; doctor's `watcher-fd-budget`
  row warns past the budget and over 80% of it. **Not Linux's 80%**: inotify's
  watches are a budget of their own, kqueue's are slots in the table
  everything else opens into (the idle bridge holds 41 open files, its pools
  bound the rest). **fsnotify v1.10.1 leaks on its own, and the watcher works
  around it** (no upstream fix): a renamed or moved-away folder's watch is
  dropped and its entries' kept, under paths that are gone (`readEvents`'
  `remove(name, false)`), so the watcher keeps an account of what fsnotify
  holds, by folder (`watchFDs`), and removes the subtree on a Rename or Remove
  (measured: 130 descriptors leaked per round of renaming 10 folders of 12
  files, 0 after); and fsnotify sends a new folder's Create and THEN watches
  the folder itself, on its own goroutine, so an Add from the handler races it
  and fsnotify loses one descriptor for good, its Close included (9 or 10 new
  folders of 10). **Never Add a folder from its Create handler on kqueue**: it
  waits (`pending`) until fsnotify sends another event, which it sends only
  once it has watched the folder, or `deferredAddWait` (250 ms, under the
  shortest scan debounce, so the folder is watched before the scan its Create
  asked for walks it). And fsnotify SENDS NOTHING for a folder renamed after
  an entry of it changed and before it read that change (measured: no event at
  all), so `reconcileWatches` releases, every 30 s, what is recorded under a
  folder fsnotify no longer watches (its `WatchList`). Linux and Windows keep
  no account (`kqueueBackend`). `TestKqueueWatcherReleasesARenamedFoldersFiles`,
  `TestKqueueWatcherOpensAFolderMovedInOnce` (200 small folders: a watcher
  that walks a folder before adding it at once still loses the race in some),
  `TestKqueueWatcherReleasesAFolderFsnotifyDroppedWithoutAnEvent`,
  `TestKqueueWatcherStopsWhenFilesTakeItPastItsBudget`,
  `TestKqueueWatcherLeavesTheProcessOpenFiles` (children that lower their own
  hard limit to 160: on main each could no longer open a file once the watcher
  had run).

- **A folder on the watch list is watched before the event that flushed it
  is handled** (2026-10-13, backlog B337). `receive` flushes pending adds
  before `handleEvent`. On kqueue a new folder is added from that flush, so
  `WatchList` can name it while that event's `releaseTree` has not closed
  the descriptors fsnotify kept (`remove(name, false)` closes only the
  renamed path). `TestKqueueWatcherReleasesARenamedFoldersFiles` waits for
  `afterReleaseForTests` of each renamed path. `releaseTree` fires that
  hook after `Remove` returns, and the test then wants the exact open-file
  total. The only bound is the suite deadline. kqueue does not order that
  rename against a
  later create, and `internalWatch` of the new path opens the directory
  before the old descriptor is closed, so a count taken when the loop is
  merely idle can be one high for that directory. A descriptor left open
  after the hook stays above the total
  (`TestALeakedWatchPerRenameDoesNotSettle`: one per directory release, the
  account already having forgotten the path, counted after the same hook).
  `TestKqueueWatcherSamplesARenameBeforeItsReleaseCloses` holds the release
  after the sync folder's Create has been handled and samples while `Remove`
  has not run. That sample can already equal the settled total: fsnotify has
  closed the renamed path, and the new watches are not open yet. After the
  hold the test waits until the loop is idle and counts once. The wait
  still fails when a descriptor is left open.

- **A quiet kqueue flush writes a file outside the library, and that
  file's event is what watches the waiting folder** (2026-10-13, backlog
  B361). The 250 ms timer used to call `flushPendingAdds` itself. `Add`
  and fsnotify's `internalWatch` can both pass `byPath` before either
  reaches `watches.add`. `add` overwrites the path's descriptor and does
  not close the one it replaced, so the first stays open in the `wd` map.
  `Close` and reconcile close only the descriptor the path map still
  names. An empty folder leaks exactly one. A rename that has not been
  read yet is also one high, for a different reason: the new path's
  directory is already open and the old directory descriptor is still
  open, and `Remove` closes it. The CI count 136 against 135
  (`TestKqueueWatcherReleasesARenamedFoldersFiles` on macos-latest, run
  38367213087) stayed one high for the whole 2 s poll B337 added, which
  cannot tell the two apart. A longer poll does not close a descriptor
  nothing releases, and it is the wrong wait for a release that has not
  been read. The rename test waits for `afterReleaseForTests`. The timer
  writes one byte of a file `openFlushSignal` creates outside the
  library and gives it `fsutil.KeepOwner` while it is open. A chown that
  fails removes the file. `register`'s `Kevent` does not drain, so the
  write's event is read after `internalWatch` returns. A write of that
  file flushes and returns before `handleEvent`. `libraryFiles` does not
  count that file. When the write cannot be made, the timer flushes
  directly. `flushAsked` keeps the timer from arming again until that
  event arrives. The file exists only where `fds` is set, which is
  kqueue. inotify and ReadDirectoryChangesW keep the loop they had: no
  signal file, and a new folder is watched from its Create. The watcher
  has no data directory, so the file is
  `bridge-watch-flush-<pid>` in the temp directory. Close removes it. A
  crash leaves that one name, and the next open of this process removes
  it first. A write to a file that is gone succeeds and wakes nothing, so
  the timer stats the path before it writes. A Remove or Rename of the
  path replaces the file and leaves the waiting folders pending; the
  next write of the new file is what watches them. A replacement that
  cannot be made flushes directly. `loopGate` does nothing unless a test
  arms it before Run, so production pays a bool read per event.
  `TestKqueueFlushDoesNotOpenAFolderTwice` writes the signal in the same
  turn (`immediateFlushForTests`, false in production) for forty empty
  folders, one at a time, at `GOMAXPROCS` 1, and wants one open file
  each. A direct flush from that turn held 42 or 43 where 41 was wanted,
  9 runs of 20. The signal write held 41, 20 runs of 20.
  `TestADeletedFlushSignalStillWatchesANewFolder` deletes the file and
  wants the same name back, then one new folder watched as one open
  file.

- **Carry the `acoseac/fsnotify` replace for the kqueue remove/Close
  order race until upstream ships a fix for fsnotify/fsnotify#795**
  (2026-10-20, backlog B401). Stock fsnotify v1.10.1's kqueue `remove`
  closes a watch's descriptor before dropping its table entry, so a
  concurrent `addWatch` can be handed the same number and the delete that
  follows drops the NEW watch's entry, which nothing closes afterwards.
  Upstream issue fsnotify/fsnotify#795 is open and unfixed. Measured under
  twelve `yes >/dev/null` hogs: `TestKqueueWatcherReleasesARenamedFoldersFiles`
  `-count=600` failed twice on main (`after round 2 … holds 135 … want 134`
  and `after round 3 … holds 136 … want 135`, 538 s, load ~12–16) and
  passed 600/600 on the fork (537 s, load ~8–14). The bridge `replace`s
  `github.com/fsnotify/fsnotify` with `github.com/acoseac/fsnotify` at
  `bda6368c20de` (branch `kqueue-remove-order-v1.10.1`: v1.10.1 plus the
  reorder in `remove` and in `Close`'s teardown loop). The require stays
  on v1.10.1. **Don't drop the replace or bump fsnotify past it until a
  release carries that fix, and then remove the replace in the same
  change.**

- **A WalkDir-resume cursor compares in TRAVERSAL order, not raw string order.**
  `WalkDir` orders by base name and visits a directory before its children, so
  `A-Bonus/…` sorts BEFORE `A/…` as a raw string while being walked after —
  pruning on a raw `<` permanently skipped a still-unwalked subtree. Use the
  segment-wise comparison, and keep it allocation-free (it runs per entry on the
  walk hot path). The one such cursor, the orphan sweeper's, went on
  2026-09-28 with `pathWalkCompare` and `dirEntirelyBehindCursor`: every tick
  walks the whole tree now (under **Job pools**). The rule is for any cursor
  that comes back.

- **A playlist file is indexed, never a track, and its members resolve when
  the feed is asked, not when it is read** (2026-10-17, backlog B431). Both
  walks hand an `.m3u` / `.m3u8` (any case, not a dot-file) to
  `notePlaylistFile` before the audio gate, so it is no track and no content
  to the clean-empty guard. `indexPlaylistFiles`, after the deletion passes
  (Scan's whole library, ScanSubtree's scope), reads a file through
  `fsutil.OpenAsFile` only when its size, mtime or `playlistFileParserVersion`
  moved, and stores the parsed lines (`playlist_files`, v56) under the WALK's
  stat. A failed read keeps the row, a file the walk could not stat is kept,
  and a row the walk did not see takes the tracks' rules in their order
  (errored subtree spared first, a backed case-only rename reaped, else the
  threshold). The root flip's wipe, a root's prefix delete and
  `clear-missing` take the rows too. `Store.PlaylistFilesPage` places and
  resolves the stored lines of one page in one read transaction at each
  request, so a member indexed later resolves with no new read. Neither side
  holds the whole index: the pass reads and writes a window at a time
  (`nextPlaylistReadWindow`, the write batch's bounds by the walk's sizes),
  and a page resolves 4 MiB of stored lines at a time in path order
  (`playlistFeedBatchBytes`), keeping its matches for later batches, so the
  answer is a single pass's (one request on 37 MiB of lines peaked at 219 MiB
  of heap in one batch, 54 MiB in batches; a page of it, since B442, at 41
  MiB, and of five times that index at 47). Every file the walk saw gets the
  pass's evidence about its folder (`playlistFolderEvidence`, the row's
  `folder_whole`): whole only when no errored subtree covers it, nothing
  directly in it errored, and every audio file the walk saw directly in it is
  in `backed`; a row the walk did not see keeps its mark. The feed's `complete` is
  `playlistFilesCompleteKey`: set by a full scan's pass that backs every file
  its walk saw (a file past a cap is a completed read with `tooLarge`, so it
  counts), deleted only with the rows by the wipe, absent after the
  migration, so a client can tell a list not yet rebuilt from one with files
  removed. **Don't resolve members
  at scan time, don't `path.Clean` a playlist line (a climb above the root is
  dropped even when it comes back down), don't let a failed read write the
  row, don't set or clear `complete` anywhere else, don't read every file
  before writing or load every stored line for a request, don't take the
  folder evidence from a running scan or from anything but the walk and
  `backed`, and don't edit
  `testdata/playlist-parse-cases.tsv`**: it is the app's
  table, copied byte for byte (iOS `a9a16365`) and pinned to LF in
  `.gitattributes`; change it there and copy it. The parse is the app's down
  to Foundation's trim set (U+200B too) and line ends (U+2029 too); it treats
  "#", "/" and "\" before a combining mark as plain characters where Swift
  does not, a measured, accepted difference (the log). Tests:
  `TestPlaylistParseCaseTable`, `TestAPlaylistMemberIndexedLaterResolvesWithoutARead`,
  `TestAPlaylistFileReadFailureKeepsItsRow`,
  `TestARemovedPlaylistFileLeavesTheFeedAtTheThreshold`,
  `TestASubtreeScanCoversOnlyItsOwnPlaylistFiles`,
  `TestAMultiRootLibraryResolvesFromTheManifestRoot`,
  `TestThePlaylistIndexIsCompleteOnlyOnceAFullScanBacksEveryFile`,
  `TestThePlaylistPassWritesEachWindowBeforeReadingTheNext`,
  `TestThePlaylistFeedAnswersTheSameInAnyBatchSize`,
  `TestPlaylistFolderEvidenceIsWholeOnlyWhenTheWalkSawAllOfIt`.
