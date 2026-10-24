# Config lifecycle history

## B514 — durable-writer evidence and advice (2026-10-23)

Three repeated A,B,A observations cannot establish that an overlay's upper
path reaches another filesystem: XFS inode totals and ZFS block totals can
change and return between reads. The writer retains three bounded retries;
upper {B,B,A} against overlay {A,A,A} is accepted on the third upper read.
A mismatch after all three stays ErrMediumUnknown and says inconclusive,
with advice to retry after filesystem activity settles and check the
service's mount namespace. This deliberately does not certify an XFS/ZFS
path whose relationship to the overlay cannot be established.

Linux reads st_dev with stat and renders its major:minor. The upper path's
own mount ID, following the final symlink as statfs/stat do, authoritatively
selects its mountinfo line and that line's fstype is named as evidence.
A decoy mount-point spelling is ignored.
Device numbers are detail only, not a second identity gate: a btrfs
subvolume's st_dev can be its anonymous device while the line names the
superblock's device. A difference, or unavailable st_dev, does not make a
valid mount-ID-selected line inconclusive. Failed or absent mount ID and
missing or malformed lines leave the visible filesystem unknown; the read
error is wrapped. Round 2 corrects round 1's device-equality requirement.
This evidence identifies the filesystem visible here; it cannot authenticate
an upper backing hidden in another mount namespace. Existing agreement and
moving-totals checks, volatile refusals and nested-overlay depth bound stay.
The current inner mount's ID also selects nested overlay and FUSE lines.
Round 3 fixes a follow/no-follow mismatch at both upper-path lookups:
upperMountID resolves symlinks before asking mountIDOf, so an upperdir link
on ext4 to an XFS directory names XFS, whose totals were sampled, rather
than ext4. Resolution errors leave the evidence unavailable and retain their
cause. The same reader follows symlinked uppers reaching nested overlays or
FUSE, so their target's mountinfo line supplies the next upper or subtype.
The strict writer's no-follow mount reader and its Linux statx/fdinfo
implementations stay unchanged; only platformMedium selects the following
reader. Resolving a path and reading its identity are separate observations,
as are statfs/stat and the mount table; this does not authenticate backing
storage across a concurrent mount or link replacement.

ErrOverlayBind still matches ErrMediumUnknown, but describes unreachable
copy-up parents on a writable overlay subtree bind: persistent storage alone
does not mend its crash risk. No upper on the root's own mount (depth 0)
means no copy-up, so a read-only bind passes and leaves EROFS to the write.
At depth > 0, a read-only overlay is ErrMediumUnknown: the visible upper
path cannot be a writable overlay's upper layer. This includes a writable
bind whose upper path has been covered by a read-only overlay, or mapped
there in the service's mount namespace. Round 1's unconditional nil bypassed
the bind refusal and also accepted this impossible upper on an ordinary
writable overlay. Both shapes are fixture regressions in
TestMediumRefusesWhatDoesNotOutliveAReboot. syncAbove names every
failed directory, advises read access or leaving the overlay only for
fs.ErrPermission, and wraps other open errors without that advice. Its doc
names copyUpsFrom, which computes the directories to sync.

The platform-neutral tests exercise the mount-table evidence and totals
sequences without host proc/sys reads. TestMediumMismatchesNeedStableEvidence
includes btrfs with differing device numbers: it names btrfs and both
numbers, never claims a device mismatch defeats the mount ID, and still
answers ErrMediumUnknown for A,B,A. Linux's adapter test uses only a temp
directory and compares its stat device with linuxStatfs's output.
TestMediumFollowsASymlinkedUpper uses a real symlink with fixture ext4 and
target filesystem identities, covering XFS evidence, a nested overlay over
tmpfs, and fuse-overlayfs; it also checks that mountIDOf's Go wrapper still
names the link's own mount (through the stubbed platform reader). These fixtures run on macOS without real cross-filesystem
mounts or host proc/sys reads, and skip Windows' privileged symlink creation.
The engine state machine, wire, and macOS/Windows mount readers are unchanged.

## Live backup reads across rename (B470 PR 5a0, Round 9)

- **A live backup retries Windows sharing/access failures while opening files replaced by rename, with atomicwrite’s bounded, cancellable backoff; persistent errors still refuse it.** **Only a reservation refused as data gets backup’s player reset advice; cancellation, an expired context and file-access errors never do. Exhausted sharing/access retries say the file was busy and to retry the backup.** `Snapshot` reads a reservation while the engine may refill it asynchronously (`ahead` → `reserveWrite` → `writeReservation` → `WriteBytesDurable`). The writer closes its staged descriptor before rename; neither it nor `readLimited` leaks a reservation handle. A rename or a Windows scan-on-close handle can nevertheless conflict with the reader's open. Closing the test's engine before backup would remove the live-backup case, not fix it. Production currently refuses the player on Windows because directory fsync is unavailable; the Windows fixture supplies that barrier, but backup also reads files other live writers replace there.

  `atomicwrite.ReadWithRetryCtx` shares `RenameWithRetry`'s five-attempt schedule (0, 50, 100, 200, 400 ms; 750 ms total sleeping). Only Windows `ERROR_SHARING_VIOLATION` and `ERROR_ACCESS_DENIED`, including wrapped errors, retry. Other platforms and other errors return at once. The context interrupts backoff; exhausted errors keep their cause. Snapshot uses it for the live reservation, the freshly renamed UUID copy it rereads to stamp that reservation, and source stats/opens, including player UUID, tokens, config and account files. The copy still streams and commits once; no queue is backed up. A persistent unreadable reservation refuses backup, and only absence omits its mark.

  `TestSnapshotRetriesAHandleHeldAcrossRename` uses the existing rename seam to hold a real Windows staged-file handle that permits rename and denies read. It verifies the unprotected open returns the sharing violation, then releases the handle only when the snapshot's context is consulted for backoff: no timer can release it before the failed read. Reservation, source UUID, captured UUID and tokens cases require captured bytes and both floors. A read error on the captured UUID refuses backup after retries rather than silently omitting the mark; a malformed UUID still keeps the existing no-mark policy. `TestSnapshotRefusesAReservationStillHeldOrCancelled` keeps the handle through exhaustion or cancellation and requires the failed snapshot to be removed. `TestReadRetryWindowsErrors` checks both Windows codes; `TestReadRetryBoundsAndCancellation` checks the shared schedule and cancellation on every platform; `TestReadRetryKeepsPermanentErrors` keeps absence and malformed data immediate.

### Round 10 — reset advice requires invalid reservation data

A shutdown cancellation in the Windows backoff used to reach the same advice
as malformed reservation data. An exhausted sharing retry also recommended
permanently giving up the identity, though neither established damaged data.
`ReadWithRetryCtx` now wraps `ErrReadBusy` and the last Windows error on
exhaustion; cancellation/deadline errors stay plain. `Snapshot` names
`player-ids.json` and the cause without reset advice for cancellation,
deadline expiry or a filesystem `PathError`. Busy errors also say to retry
the backup. A reservation read successfully but refused as data (malformed,
unsupported version, invalid UUID or excessive size) retains the reset advice.
Only absence omits the mark; every other error still refuses and removes the
partial snapshot. `ERROR_ACCESS_DENIED` remains retryable: replacement or
delete-pending files can return it during the swap, with the same bounded
schedule as sharing violations.

`TestSnapshotRefusesAReservationStillHeldOrCancelled` now checks held,
cancelled and expired cases for no reset advice, the file and cause, and
busy/retry wording on exhaustion. `TestUnreadableReservationAdviceResetsIdentityAndQueue`
requires no reset for a backup's file-access error and retains the complete
advice for malformed data and an unsupported version. Restore's existing
refusal policy is unchanged. `TestReadRetryWindowsErrors` requires the busy
marker on exhaustion of either code and its absence on cancellation/expiry.
Windows tests are cross-compiled here; their native execution remains a
Windows-runner check, distinct from portable race-test results.

## The config file's lock (backlog B506, 2026-10-24)

- **Every read-modify-write of `bridge.yaml` takes the exclusive lock on
  `.bridge.yaml.lock` beside the file, in the directory the OS writes it in
  as its name resolves (`config.LockFile`, `fsutil.ResolveParent`), a lock
  file a root writer makes taking the config's owner
  (`fsutil.OpenLockFileLike`); it decides on the file as its turn reads it,
  and writes only through `Save(l)` / `SaveIf(l, …)` / `l.SaveBytes`, which
  stage in that directory and, just before the rename, refuse a file that is
  no longer the one the turn read (`*config.ChangedInTurnError`); the wait
  is bounded at `config.LockWait` (5 s), then a value-free
  `*config.BusyError`; only a lock whose answer says the filesystem keeps
  none (`fsutil.KeepsNoLocks`) leaves the turn unlocked, and any other
  failure is returned; serve's console save (`RuntimeConfig.Update`) takes
  its turn and compares before it runs its change, saves only over the
  bytes the holder last read (`RecordFile`, at boot) or wrote, and refuses a
  file another writer changed with a `*config.ChangedOutsideError` rather
  than merge** (2026-10-24, backlog B506; the second round, the same day,
  took the reviews' findings, below). Until then several writers read the file, changed it and wrote it
  back, and nothing made them take turns: `bridge library add` and `remove`
  (their offline path) and `bridge init`'s save took no lock at all;
  `bridge output dac bind` held the data dir's lock and saved through
  `SaveIf`, whose comparison of the file's bytes with what it read narrowed
  the window to the instant before its rename and could not close it, since
  no rename can be made conditional on the file it replaces; `bridge
  restore` copied a snapshot's `bridge.yaml` over the file; serve's
  `--init-if-missing` seed wrote one after a stat; and a running serve's
  console saves (the settings PATCH, roots add and remove, the variants
  directory, UPnP server CRUD, every one through `RuntimeConfig.Update`)
  wrote the live snapshot, read at boot, over whatever the file held. Two
  writers could each lose the other's update. Measured on main with the
  tests named below: a settings save that landed between library add's read
  and its save was gone (`libraryName "Test Library"`), and so for remove; a
  library add that landed in bind's window made bind refuse its binding
  ("changed after bind read it"); a console save over a file another writer
  had changed answered 200 `live` and wrote serve's copy over it; each CLI
  writer went through in 18 to 149 ms while another open held the lock.

  **The lock** is an exclusive kernel lock (`fsutil.TryLock`: flock on
  unix, LockFileEx on Windows) on `config.LockFileName(path)`, a dot, the
  config file's base name and `.lock`, beside the config file, opened
  through `fsutil.OpenLockFileLike` (OpenLockFile's macOS create-race retry
  and its refusal of a link out of the directory). Not on the config file
  itself: Save replaces it by a rename, which a lock on the old file does
  not follow, and on Windows a locked range refuses every other handle's
  reads, and the config is read by everything. Not in the data dir: the
  config names its data dir, a write can change it, and init and the seed
  write a config before any data dir is known. The file is never removed;
  only the lock on it means anything, and the kernel drops that with the
  process however it exits. `TestTheLockIsAFileBesideTheConfig`.

  **The directory is the one the OS writes the file in** (the second
  round; Codex's review): `fsutil.ResolveParent` splits the config's name
  into its directory part, as written, and its last element. On unix the
  directory part is made absolute by the working directory without
  cleaning and handed to `filepath.EvalSymlinks`, which follows each link
  where it stands and takes a `..` after it from where the link leads, as
  the kernel walks the name; on Windows it is `filepath.Abs`'s, the
  normalisation Win32 makes, `..` by the name, before the filesystem sees a
  path (a link there opens the same directory, so it is left as it is). The
  first round took `filepath.Dir(path)`, which cleans lexically: with
  `cfg/link` leading to `real/sub`, `cfg/link/../bridge.yaml` is
  `real/bridge.yaml` to the OS, and its lock was taken in `cfg`, so two
  library commands given the two names took two locks, read the same file
  and each could write over the other's root. Save staged its file in that
  cleaned directory too and renamed it across into `real` (a rename that
  fails across filesystems), and so did a restore's copy of bridge.yaml.
  The turn now keeps the resolved directory and the file in it (`dir`,
  `target`), stages every save there and renames onto the file there; the
  messages keep the name the writer gave. A name whose last element names a
  directory (none, `.`, `..`) is refused. Red before the fix, on the second
  round's seams: `TestTwoNamesOfOneConfigTakeOneLock` ("an Edit through
  …/cfg/link/../bridge.yaml went through while a turn through
  …/real/bridge.yaml was held: it took another lock", and the first turn's
  change alone in the file), and `TestASaveThroughALinkStagesWhereItRenames`
  ("temp file: open …/cfg/.bridge-….yaml: permission denied", `cfg` holding
  no new entry and a lock file of its own). Unix only: Windows takes `..` by
  the name.

  **A lock file made as root takes the config file's owner** (the second
  round; both reviews): `OpenLockFileLike(dir, name, like)` precreates the
  lock with the owner `KeepOwner` would give a file staged for `like`, the
  config file: its owner when it exists, the directory's only while it does
  not, 0600, so that owner can open it. The first round gave it the
  directory's owner: a user's config in a directory root owns (`/tmp`, a
  mount point, a `root:bridge 0775` directory) had its lock made root's 0600
  by the first `sudo bridge library add`, and every later save of the
  config's own user failed at "open the lock beside …", though Save had kept
  the config theirs. A lock file already there is opened as it is (a root
  writer does not chown a file it did not make). Red before the fix:
  `TestALockFileMadeAsRootTakesTheConfigsOwner` (the directory root's, the
  config 4242's; the lock was given 0:0), through
  `fsutil.SimulateRootOwnersForTest`, which gives each path an owner of its
  own; `TestSaveAsRootKeepsTheConfigOwner`'s one owner could not show it.
  Run as root with a single owner, the lock is still among the files given
  the install's owner (`TestSaveAsRootKeepsTheConfigOwner`,
  `TestSnapshotAndRestoreAsRootKeepTheInstallOwner`).

  **It is held around one read-modify-write, never for a process's life**:
  init's turn is its final check and save (`saveInitConfig`), never its
  prompts; bind's runs from its read under the data dir's lock to its
  `SaveIf`; library add's and remove's is the whole of their offline change
  (`config.Edit`), the library index's wipe or prune included, since that
  change is decided on the copy read under the lock; a restore's is the
  whole of its writes, taken before the first (`backup.RestoreWithOptions`),
  so a busy config refuses a restore with nothing restored rather than half
  of it; serve's is one console save. So serve's console saves and a CLI
  writer beside a running serve take turns.

  **Taking it is bounded**: `LockWait` (5 s), asked again every 25 ms
  (`lockRetry`), then a `*BusyError`, which matches `ErrBusy` and says
  "another bridge command or the running bridge is changing <path>, and held
  its lock for longer than 5s, so nothing was written: try again", naming
  the file and nothing it holds. An ordinary turn is milliseconds (a read, a
  staged write and its fsync, a rename, and on Windows at most
  `atomicwrite`'s 750 ms of rename retries); the long turns are a CLI's own,
  library add's and remove's index change and a restore's copy, and a writer
  behind one of those for longer than that is better told to try again than
  left waiting: a console save is a request someone is watching. SQLite's
  busy wait is the same 5 s, so a console request waiting on either is
  bounded alike. Measured: each of the six CLI writers (library add and
  remove, init's rewrite, bind, restore, serve's seed), while another open
  held the lock, answered after 5.03 to 5.08 s, refused, with the file as it
  was; a console save answered after 5 s with the refusal (the console's 500
  `save-config` and the message) and the next one went through once the
  lock was free. `TestALockAnotherHoldsIsWaitedForThenRefused`,
  `TestALockFreedWhileItWaitsIsTaken`,
  `TestEveryConfigWriterWaitsForTheLockThenRefuses`,
  `TestAConsoleSaveWaitsForTheLockThenRefuses`,
  `TestUpdateWaitsForTheLockThenRefuses`.

  **A filesystem that keeps no locks is no refusal, and only that**: where
  the lock's own answer says no lock can be taken there
  (`fsutil.KeepsNoLocks`: ENOLCK, EOPNOTSUPP, ENOTSUP and
  `errors.ErrUnsupported`, which ENOSYS matches too, from flock; on Windows
  ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION from LockFileEx), the turn
  is taken without the lock (`FileLock.Unheld` says why), said once per file
  and process ("config: no lock can be taken beside the config file, so its
  writers do not take turns there"). That is the data dir's lock's rule: a
  lock that cannot be taken must not stop a bridge from starting, and here a
  console from saving any setting. Any other failure to take it (EIO, EBADF,
  a handle the platform refuses) is returned ("take the lock beside …"), and
  no turn is taken: the first round read every answer but contention as "no
  locks", so an I/O error dropped mutual exclusion, in silence after the
  first warning (the Claude review). An error opening the lock file (a
  directory this user cannot write, where Save could not stage its file
  either) is returned as it is. Red before the fix:
  `TestOnlyAFilesystemThatKeepsNoLocksTakesTheTurnUnlocked` ("input/output
  error: took the turn without the lock", and the same for EBADF, EINVAL and
  an error of no errno). `TestAFilesystemThatKeepsNoLocksIsNoRefusal`.

  **Every save in a turn compares the file with the one the turn found,
  just before its rename** (the second round). `LockFile` reads the file once
  it holds the turn, through `fsutil.ReadAsFile` (a named pipe refused, not
  waited on), or notes there is none (`found`, `absent`); the one write every
  save goes through (`FileLock.write`: `Save`, `SaveIf` and the turn's
  `SaveBytes`, which a restore writes the snapshot's copy with) stages the
  file, then reads the file again (`stillAsFound`) and refuses with a
  `*ChangedInTurnError` (matching `ErrChangedInTurn`, naming the file and
  nothing it holds) when its bytes are not those, when it is gone, or when
  one appeared where there was none, and once the write lands its bytes are
  what the turn compares with. With the lock held only a writer that takes
  no turns can change the file meanwhile (an edit, a bridge older than the
  lock); without it any writer can, and the comparison is then all that
  keeps another writer's save. It narrows that window to the instant
  between the read and the rename and does not close it. **The first round
  said the writers on such a filesystem "are back to their own byte
  comparisons", and that was true of bind's `SaveIf` and the console's
  check alone: `Edit` (library add and remove), a restore and serve's seed
  had none** (the Claude review); now every writer has the turn's own.
  What each makes of the refusal: the console's `Update` answers its
  `*ChangedOutsideError`, the seed writes nothing and says so, and serve
  loads the config that appeared, init says the config changed while it ran
  and that it did not write it, bind its own "changed after bind read it",
  a restore "restore bridge.yaml: … changed …", library add and remove
  "save config: …" (after a wipe, retargeting the carry, as for any save
  that failed after it). Red before the fix, a writer that takes no turns
  landing between the staging and the rename (`OnSaveStagedForTest`) or in
  a restore's turn (the restore's own hook): `TestUpdateComparesTheFileAgainJustBeforeItsRename`,
  `TestEditSavesOnlyOverTheFileItRead`, `TestAnUnlockedTurnSavesOnlyOverWhatItFound`
  (a turn taken without the lock, over a changed file and over one that
  appeared), `TestARestoreLeavesAConfigChangedInItsTurn`,
  `TestServesSeedLeavesAConfigThatAppearsWhileItSaves` and
  `TestInitsSaveLeavesAConfigChangedWhileItSaves`, each saving over the
  other writer's file.

  **`Save` and `SaveIf` take the turn (`*config.FileLock`), never a path**,
  and write the file that turn is at, staged in the directory it resolves
  to; a nil turn, or one given back, is refused (`heldFor`): no writer can
  write the config without having taken the lock, by the compiler and at
  run time. `SaveBytes` is the turn's own write of bytes as they are, which
  a restore puts the snapshot's copy back with. `SaveIf`'s byte comparison
  stays where it was (bind), beside the turn's, as the guard bind words
  itself: an edit, or a bridge older than the lock. `Edit` is the
  read-modify-write in one call: the lock, a load of the file as the turn
  read it (the turn's own read: `loadData` of `found`), the caller's change,
  and the save. `TestASaveNeedsATurnItHolds`, `TestEditReadsTheFileInItsTurn`,
  `TestSaveIfAsksJustBeforeTheRename`,
  `TestOutputDACBindNeverWritesOverAnUpdateAfterItsRead`.

  **Serve's console save reads the file again in its turn and saves only
  over the bytes the holder last read or wrote**: `runServe` hands the holder
  the bytes `config.LoadFile` read at boot (`RecordFile`), and each save
  records its own. Over a file another writer changed or removed since, it
  saves and stores nothing and answers a `*ChangedOutsideError` (matching
  `ErrChangedOutside`: "…was changed or removed by another writer since this
  bridge read it (a bridge command such as library add or init, or an edit),
  so this change was not saved, and the file keeps that writer's: restart
  the bridge to load the file, then make the change again"), which the
  console shows; serve loads that writer's change when it next starts.
  **Refused, not merged**: a merge of the console's change into the changed
  file was weighed and not taken. Serve's live snapshot differs from the
  file by more than another writer's changes (`--addr`, which every console
  test bridge passes; the defaults, the resolved paths and the environment's
  overrides `Load` puts in; an unread player block, whose reason names a
  line), so a field-by-field merge would either refuse saves that conflict
  with nothing or need a record of every setting the two disagree on, and a
  reload of the live snapshot is out of scope (nothing reloads the config).
  A holder told nothing (a test's `NewRuntimeConfig` alone) saves its first
  Update over whatever the file holds and records from then on.
  `TestUpdateRefusesAFileAnotherWriterChanged`,
  `TestServeRefusesASettingsSaveOverAConfigAnotherWriterChanged`.

  **The turn and the comparison come before `fn`** (the second round; the
  Claude review's finding 2 and Codex's 3). The first round cloned, ran
  `fn`, then took the lock and compared, and the console's root changes
  made their change to the library index before `Update` was called at all:
  the wipe a library going from one root to more takes (every filesystem
  row, and every rendition and waveform, unlinked once it commits), the
  removed root's prefix delete (its rows and sidecars), the wipe of a
  collapse to one root. A save the comparison refused, which the lock made
  routine after any outside write and which lasts until a restart, had
  already done that, and did it again on every retry. Now `Update` takes the
  turn, compares the turn's read with the bytes the holder last read or
  wrote, and only then clones and runs `fn`, and the root handlers
  (`applyRootAddition`, `applyRootRemoval`) run their wipe or prune inside
  `fn`: a refused save (the comparison, or a lock another writer held past
  the wait) runs none of it. The order of PR #22 stays, the manifest change
  first and the root list's save after it succeeds, so a failed wipe saves
  nothing and every failure after a wipe lands where a scan heals: a save
  that fails after it, or that the comparison just before the rename
  refuses (a writer that takes no turns, landing in the save's own staging),
  retargets the carry and runs the compensating scan, as a failed save
  always did. Doing the change after `Update` returned was weighed: it
  reverses PR #22's order, and a manifest change that failed after the save
  landed would need a second save to put the roots back, which can fail in
  turn; inside `fn` it is one turn of the lock with the save, as library
  add's and remove's offline change is in `Edit`. The cost is that the turn
  is held for the wipe (a CLI writer behind it waits, and past `LockWait`
  is told to try again). The settings PATCH, the variants directory and the
  UPnP server writer do nothing outside `fn` before `Update`. Red before the
  fix: `TestARefusedUpdateRunsNoneOfItsChange` ("Update ran its change
  before it compared the file, and refused the save after"),
  `TestUpdateWaitsForTheLockThenRefuses` (the same before a `BusyError`),
  and `TestARootChangeTheSaveRefusesLeavesTheLibraryAlone` (an add that
  would wipe, a remove that would prune, a remove that would collapse; the
  rendition unlinked by each refused change).

  **Every writer is named**: `cmd/bridge`'s
  `TestEveryConfigWriterTakesTheConfigLock` sweeps the Go files the module
  builds, each function and each package-level variable's value, for the
  config's `Save` or `SaveIf` or the turn's `SaveBytes` (and, inside
  `internal/config`, its `save`), called or taken as a value (`save :=
  cfg.Save`), for `config.Edit` or a config holder's `Update`, likewise, or
  for a file written while the function names the config's path
  (`configPathNames`, or the literal "bridge.yaml", alone or as a path's
  last element): a write is os's or an `*os.Root`'s `WriteFile`, `Create` or
  `Rename` on anything, `OpenFile` with a flag that writes (or one the sweep
  cannot read), any `atomicwrite` or `fsutil` function whose name begins
  Write or Rename, `fsutil.Precreate`, and restore's `copyFile` and
  `writeFrom` by name. It requires each such writer to be in `configWriters`
  and to call the lock named there, and one that saves itself to take it
  before it saves and to make the save from a read in the turn: the last
  read of the config before its first save (`configReadCalls`, and a read
  of a path that names the config's) must come after the lock, which a
  writer that reads before its turn to decide and again in it (bind,
  library add) passes and one that saves what it read before the lock does
  not. An entry nothing writes any more fails it, and so does a method
  named Save, SaveIf or SaveBytes on another type, which the sweep would
  read as the config's. `notConfigWriters` excuses `backup.Snapshot`, which
  reads the file into a snapshot, and the sweep requires it to still be
  what it excuses. The config's own save (`configSavePrimitive`) is not
  counted as a writer: `Config.save` must ask `heldFor`, and the one write
  every save goes through (`FileLock.write`) `heldFor`, the turn's
  comparison (`stillAsFound`) and its rename.
  `TestTheConfigWriterSweepReportsEveryShape` runs the sweep over synthetic
  source with each shape it catches and each it lets through. **The second
  round's shapes**, each missed before (the reviews: the literal, os.Root's
  writes, fsutil's, a package-level `var f = func(){…}`, a method value, and
  a lock taken after the read): bind's lock moved to just before its
  `SaveIf`, the first round's control 13, passed the first round's sweep and
  fails this one ("runDACBind reads the config before it takes
  config.LockFile and saves what it read"). **What it cannot see**: the sweep
  reads one function at a time, so a helper handed the path by a caller in
  another function and writing it is a writer it misses, the helper naming
  no config path and the caller writing nothing (`copyFile` and `writeFrom`
  are named for that reason); and its order check reads positions in the
  source, not the control flow, and knows only the reads `configReadCalls`
  names. It claims no more than that.

  **The tests wait on the lock's own word, never a time** (the second
  round; both reviews' timing findings): `config.LockWaitedForTest(path)`
  closes a channel the first time a writer of that file finds the lock held
  and starts to wait, keyed by the file as the turn resolves it, and
  `config.OnSaveStagedForTest(path, fn)` runs a test's writer between a
  save's staging and its rename. The first round's 300 ms in
  `TestBindKeepsALibraryAddThatLandsInItsWindow` passed whether or not the
  add reached the lock (a late add makes the same file), the 100 ms in
  `TestEditReadsTheFileInItsTurn` caught its control only when the goroutine
  ran in time, and `TestTheDataDirLockComesBeforeTheConfigLock` sampled the
  data dir's lock halfway through a 5 s wait, which a slow runner could miss
  in either direction. Each now waits for the writer to be waiting, and
  fails at once when it finishes first: the first round's controls 10
  (`Edit` loading before its turn) and 13 (bind's lock just before its
  `SaveIf`) went red 20 times in 20, and bind's lock order swapped 10 in 10.
  The tests that wait out the whole 5 s and share nothing run in parallel
  (`t.Parallel`); the console's, which boots a serve, does not, since no
  serve test in `cmd/bridge` runs beside another.

  **Don't save a copy read before the lock, take the lock around a prompt
  or for a process's life, lock the config file itself or put the lock in
  the data dir, clean a config's name before it resolves, stage a save
  anywhere but where its rename lands, make a root writer's lock file
  root's beside another owner's config, wait for it without a bound, take a
  failure other than no locks for an unlocked turn, rename without the
  turn's comparison, put a value of the file in a refusal, give `Save` a
  path back, drop `SaveIf`'s comparison from bind, let a console save write
  over bytes it did not last read or write, change anything besides the
  config before calling `Update` (do it in `fn`, after the comparison), or
  add a writer of `bridge.yaml` that `configWriters` does not name.**
