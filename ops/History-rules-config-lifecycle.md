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
  `.bridge.yaml.lock` beside it (`config.LockFile`), reads the file again in
  that turn, and writes only through `Save(l)` / `SaveIf(l, …)` with it; the
  wait is bounded at `config.LockWait` (5 s), then a value-free
  `*config.BusyError`; serve's console save (`RuntimeConfig.Update`) saves
  only over the bytes the holder last read (`RecordFile`, at boot) or wrote,
  and refuses a file another writer changed with a
  `*config.ChangedOutsideError` rather than merge** (2026-10-24, backlog
  B506). Until then several writers read the file, changed it and wrote it
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
  config file's base name and `.lock`, in the config file's directory,
  opened through `fsutil.OpenLockFile` (its macOS create-race retry, the
  directory's owner through `Precreate` when run as root, a link out of the
  directory refused). Not on the config file itself: Save replaces it by a
  rename, which a lock on the old file does not follow, and on Windows a
  locked range refuses every other handle's reads, and the config is read by
  everything. Not in the data dir: the config names its data dir, a write
  can change it, and init and the seed write a config before any data dir
  is known. The file is never removed; only the lock on it means anything,
  and the kernel drops that with the process however it exits. Run as root,
  the first writer's lock file takes the directory's owner, so the service
  can open it for its next console save
  (`TestSaveAsRootKeepsTheConfigOwner`,
  `TestSnapshotAndRestoreAsRootKeepTheInstallOwner`).
  `TestTheLockIsAFileBesideTheConfig`.

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

  **A filesystem that keeps no locks is no refusal**: where `TryLock`
  answers anything but contention (ENOLCK, EOPNOTSUPP, `ErrUnsupported`),
  the turn is taken without the lock (`FileLock.Unheld` says why), said once
  per file and process ("config: no lock can be taken beside the config
  file, so its writers do not take turns there"), and the writers there are
  back to their own byte comparisons (bind's `SaveIf`, init's
  `configChangedSinceRead`, the console's check of the bytes it last read or
  wrote), as before the lock. That is the data dir's lock's rule: a lock
  that cannot be taken must not stop a bridge from starting, and here a
  console from saving any setting. An error opening the lock file (a
  directory this user cannot write, where Save could not stage its file
  either) is returned as it is. `TestAFilesystemThatKeepsNoLocksIsNoRefusal`.

  **`Save` and `SaveIf` take the turn (`*config.FileLock`), never a path**,
  and write the file that turn is at; a nil turn, or one given back, is
  refused (`heldFor`): no writer can write the config without having taken
  the lock, by the compiler and at run time. `SaveIf`'s byte comparison
  stays where it was (bind), the second guard against a writer that takes no
  lock: an edit, or a bridge older than the lock. `Edit` is the
  read-modify-write in one call: the lock, a `Load` of the file as it is in
  the turn, the caller's change, and the save. `TestASaveNeedsATurnItHolds`,
  `TestEditReadsTheFileInItsTurn`, `TestSaveIfAsksJustBeforeTheRename`,
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

  **Every writer is named**: `cmd/bridge`'s
  `TestEveryConfigWriterTakesTheConfigLock` sweeps the Go files the module
  builds for a call of the config's `Save` or `SaveIf` (and, inside
  `internal/config`, its `save`), of `config.Edit`, of a config holder's
  `Update`, or for a file written in a function that names the config's
  path (`configPathNames`), and requires each such function to be in
  `configWriters` and to call the lock named there; an entry nothing writes
  any more fails it, and so does a method named Save or SaveIf on another
  type, which the sweep would read as the config's. `notConfigWriters`
  excuses `backup.Snapshot`, which reads the file into a snapshot, and the
  sweep requires it to still be what it excuses. The config's own save
  (`configSavePrimitive`) is not counted as a writer and must ask `heldFor`.
  `TestTheConfigWriterSweepReportsEveryShape` runs the sweep over synthetic
  source with each shape it catches and each it lets through.

  **Don't save a copy read before the lock, take the lock around a prompt
  or for a process's life, lock the config file itself or put the lock in
  the data dir, wait for it without a bound, put a value of the file in a
  refusal, give `Save` a path back, drop `SaveIf`'s comparison from bind,
  let a console save write over bytes it did not last read or write, or add
  a writer of `bridge.yaml` that `configWriters` does not name.**
