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

## A whole-number setting takes no value yaml.v3 would change on the way in (B528, 2026-10-24; B533, 2026-10-26)

- **A whole-number setting takes no value yaml.v3 would change on the way in: a fraction, an infinity, NaN or a float outside its range is refused as a word on that line is.** **Don't decode the config but through `config.DecodeFile`, find the setting a value reaches with a resolver of your own (the decoder maps the shadow itself), or quote the value in the refusal.** yaml.v3 v3.0.1 decodes a `!!float` into an int kind by dropping what follows the point (`decode.go`'s `scalar`: `int64(resolved)` once the float is no more than `math.MaxInt64`), and into what the platform's conversion makes of `-.inf` and 2^63 (the smallest int64; on arm64 2^63 became the largest). Measured on main before the change: 42 of the config's 44 whole-number settings outside the player block loaded `2.75` as 2, and the other two (`enrich.coverSize`, `retention.playbackHistoryDays`) refused it only because 2 is outside what they take, so a fraction inside their range loaded the same way; `limits.manifest.requestsPerMinute: 0.5` loaded as 0, which turns the manifest limit off, and so did `-.inf`, as the smallest int64; in the player block, `openhomePort: 7792.9` bound 7792 and `pauseReleaseSeconds: 2.5` held the DAC for 2 s. A refusal follows its neighbours: Load refuses the file, as it refuses a word on that line, with the words `ValueFreeYAMLError` gives the out-of-range float yaml.v3 refuses itself, `line N: cannot unmarshal a !!float value into int` (the line, the tag and the Go type, nothing of the value); the player block's reader leaves the block unread, the player off with `output.player.<setting> (line N): must be a whole number`, and the load goes on. A float whose value the integer holds exactly is taken as written, `30.0`, `3e1`, `-0.0`, `!!float 6`, and, before B533, `08`, which yaml.v3 resolves as a float since `08` is no octal: refusing every `!!float` was rejected, since it would have stopped files loading over values that lose nothing (`21600.0` from a tool that writes its numbers as floats). B533 now refuses the ambiguous leading-zero spelling separately, after the exact-value check. `exactInteger` is the one test: anything but a `!!float` passes (an integer, a null, or a value the decoder refuses itself), and a `!!float` passes when it is whole and inside the integer's range, compared as powers of two (`math.Ldexp`), which float64 holds exactly, and when its text, read exactly as yaml.v3's resolve reads it (`writtenInteger`: its underscores dropped, an integer spelling through `strconv.ParseInt` with base 0 first, so `!!float 036` is 30 as `036` untagged is, then resolve's own fallback for a binary or octal prefix base 0 refuses, so `!!float 0b+11110` is 30, then a decimal float digit by digit, in time linear in the text), writes that very float64; a round trip through the integer was rejected, since arm64 saturates 2^63 to the largest int64, which converts back to 2^63. The exact reading is there because yaml.v3 reads a float through `strconv.ParseFloat`, which rounds to the nearest float64: before it, `30.0000000000000001` loaded as 30, `requestsPerMinute: 1e-400` as 0, the limit off, and `9007199254740993.0` as 9007199254740992, each whole and each a value nobody wrote (CodeRabbit on PR #1339, measured red first). The first exact reading, `big.Rat`, was rejected at review (Codex and Claude on PR #1339): it read `!!float 036` as 36 where yaml.v3 reads 30, refused `0e99999999999999999999`, which writes 0, and `30.` with a million and one zeros after its point, and took time quadratic in a long mantissa's digits (a million digits: 517 ms, against 0.74 ms for `writtenInteger`). Refused unread: a whole number of more than 20 digits, which no 64-bit integer holds, and a number other than zero with an exponent of a million or more, which yaml.v3 never decodes unchanged: strconv stops reading an exponent once it reaches 10000, so such a text overflows, and resolves as a string, or decodes to another value (`0.` and 9,998 zeros then `3e10000000` decodes as 30, and is refused).

  Load's decode is `config.DecodeFile`: yaml.v3's decoder with `KnownFields` (the typo catcher), and, once that decode succeeds, `checkWholeNumbers` over the same bytes. It decodes the file a second time into a shadow of the type (`shadowOf`): each integer field replaced by an `integerCheck` of its type, whose `UnmarshalYAML` decodes nothing and answers a `*yaml.TypeError`, which yaml.v3 adds to its own and goes on past, so every such value is named; every struct it rebuilds keeps its fields' names and tags, and every type that holds no integer is kept as it is, so yaml.v3 itself maps the document onto the shadow as onto `Config` and decodes the same nodes in the same order: the same keys, merge keys, aliases (named at the line of the node an alias names, once for each setting it reaches), inline fields and encoded keys, and the same count of aliases against its limit. A resolver of the config's own was rejected for the reason B505's round 2 records under the rewrite's rule (cli-serve.md): its copy of the decoder's merge and key rules disagreed with the decoder three ways. A type that reads its own YAML (`yaml.Unmarshaler`, the older form, `encoding.TextUnmarshaler`, `yaml.Node`) is not looked into, so `PlayerConfig` reads its own block; `time.Duration` decodes from text alone, and is no whole number there; a map's keys are not looked at, since no map in the config is keyed by a number. `ValueFreeYAMLError` keeps a line already without its value as it is (`valueFreeTagged`), since yaml.v3's own always start with the tag, and the check's lines pass through it. bridge init's rewrite reads the file through `DecodeFile` (`outputAsLoadDecodes`), so a file holding a fraction anywhere decodes whole there no more than in Load, and asks `CheckWholeNumbers` of the block it decodes on its own and of each setting it recovers (`outputAsLoaded`, `outputRecovered`): until then Load took a file holding `loopbackPort: 7795.5`, and the rewrite kept the 7795 it read. Out of scope, each checked: the `BRIDGE_*` overrides parse with `strconv.ParseInt`, which refuses a fraction (warned and ignored); the console's settings PATCH decodes JSON into `*int`, which `encoding/json` refuses with a fraction in it (400); and, at B528, yaml.v3's YAML 1.1 octal, `loopbackPort: 010` loading as 8, covered by B533 below. A config that holds a fraction stops loading after this change, where it loaded its whole part: serve, doctor and init name its line. `TestAWholeNumberSettingTakesNoFraction` (every whole-number setting `envBindings` knows outside the player: a fraction refused, value free, at its line, with its Go type; an integer it takes and that integer as a whole float loading as written; null loading as left out), `TestAWholeNumberSettingTakesWhatDecodesUnchanged` (the edges on `limits.manifest.requestsPerMinute`, a merge key, an alias, three fractions in one file), `TestAWholeNumberOfThePlayerTakesNoFraction`, `TestAnOpenHomeSettingIsValidatedLikeItsNeighbours`, and init's rows in `TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo`, `TestInitRewriteResolvesMergesBesideASettingItCannotKeep` and `TestPriorOutputOfAFileTheDecoderRefusesElsewhere` pin it.


  **B533 refuses an otherwise accepted leading-zero spelling; never reinterpret it as decimal.**
  yaml.v3 reads `010` as octal 8 and `0777` as 511, but `08` and `07792`
  as floats, 8 and 7792. Reading these ourselves as decimal was rejected:
  it would silently disagree with other YAML 1.1 tools. No whole-number
  setting is a permission or umask. `leadingZero` drops underscores and an
  optional sign, then checks for zero followed by a decimal digit, including
  integral float spellings (`010.0`, `08e0`) and explicit numeric tags.
  A lone zero, signed zero, `0.0`, `0e5`, and explicit bases (`0o`, `0x`,
  `0b`) retain their meaning. `writtenInteger` still models YAML's reading;
  its parser tests keep `036` and `08`, while the setting acceptance tests
  now refuse `08`, `!!float 036` and the player's `!!float 010`.

  **Keep existing decode and B528 refusals first, decoder order and multiplicity,
  and the player's unread-block contract.** The shadow calls the predicate
  only after `exactInteger` passes; its caller has already decoded successfully.
  Thus `00.5` retains only B528's fraction line, and an unknown key, quoted
  integer, out-of-range number or tag yaml.v3 refuses keeps the first decode's
  error unchanged. yaml.v3 still chooses which aliases, merge values and
  inline fields reach a setting: an overridden leading-zero base is ignored,
  a shared anchor gets one line per reached setting, and map keys are untouched.
  No line-order promise is made. `playerReader.whole` calls the same predicate
  after its existing checks; its first reason stops reading, leaves the block
  unread and the player disabled, and save/reload preserves the spelling.

  **Pass only the exact fixed diagnostic through `ValueFreeYAMLError`.**
  The sentence is `line N: a whole number is written with a leading zero,
  which YAML may read as octal; write it in decimal without the leading zero,
  or with 0o if you mean octal`. It quotes no value. Equality of the entire
  sentence after the line prefix admits it; appended file text is not kept.
  The player's reason gives the same remedy in its own style.
  A previously accepted `output.loopbackPort: 08080` (8080 at `a6393b1e`)
  now refuses to load; write `8080`, or an explicit octal prefix if intended.
  Init emits no such spelling and refuses it at all three existing checks:
  whole file, output block beside another error, and recovered setting.

  Proof: `TestLeadingZeroSettingsRefused`, `TestLeadingZeroDecoderReach`,
  `TestLeadingZeroBoundary`, `TestLeadingZeroAcceptedSpellings`,
  `TestLeadingZeroShadowKinds`, `TestLeadingZeroValueFreeDiagnostic`,
  `TestLeadingZeroPlayerBlock`, `TestLeadingZeroPlayerBoundary` and
  `TestLeadingZeroInitRecovery`. Real fields cover int, *int and int64;
  synthetic shadows cover every other integer width, inline fields and maps.
  Red runs against `a6393b1e` quote complete got/want diagnostics. Making only
  `leadingZero` return false admits each new refused value again, while
  B528's fractions and first-decode errors retain their previous diagnostics.
  Full run evidence and limits are recorded under B533 in the engineering log.

## The config file's lock (backlog B506, 2026-10-24)

- **Every read-modify-write of `bridge.yaml` takes the exclusive lock
  beside the file the OS opens for its name (`config.LockFile`,
  `fsutil.ResolveParent`: the directory a `..` after a link resolves to, on
  Windows named by the handle the OS opens, junctions included
  (`fsutil.ResolveLinks`), and refused only as a link still, its own entry
  a name surrogate, or as one whose resolution moves when asked again, a
  OneDrive or container folder being a directory (`checkResolvedDir`); a
  link at the last element followed to its target, whose name the lock
  takes, `.actual.yaml.lock` for `alias.yaml` → `actual.yaml`), a lock file
  a root writer makes taking the config's owner
  (`fsutil.OpenLockFileLike`); the name is resolved again after every read
  a decision of the turn rests on, last: once the turn has made its own
  read, before each attempt at a rename after the byte comparison and a
  writer's own comparison in `SaveIf`, and at `FileLock.Check` after its
  read, and a name that opens another file than the turn locked refuses
  with nothing written (`*config.NameMovedError`; a CLI writer says to run
  it again, and nothing is retried on the file it opens now); it decides on
  what its turn found and saves a value taken from it (`FileLock.LoadFound`,
  `Found`), or only where its turn found no file, or after an if that
  returns on a comparison of the turn's bytes `comparedSaves` lists (init's,
  a restore's, the holder's), at the top of the function or inside its
  check of the turn against nil, a save after that check then made through
  the name it tests, assigned nothing since, and writes only through `Save(l)` /
  `SaveIf(l, …)` / `l.SaveBytes`, which stage beside the target and, before
  each attempt at the rename, refuse a file that is no longer the one the
  turn read (`*config.ChangedInTurnError`), a refusal ending the retries and
  no rename made once `saveWindow` has ended (`atomicwrite.RenameWithRetryIf`);
  the turn's reads are asked again only while Windows answers
  `ERROR_SHARING_VIOLATION` or `ERROR_ACCESS_DENIED`; the wait is bounded at
  `config.LockWait` (5 s), then a value-free `*config.BusyError`, or, where
  every try answered ENOLCK, a `*config.NoLockManagerError` naming the
  remedy; only a lock whose answer says the filesystem keeps none
  (EOPNOTSUPP, ENOTSUP, `errors.ErrUnsupported`, Windows' ERROR_NOT_SUPPORTED
  and ERROR_INVALID_FUNCTION) leaves the turn unlocked, and any other
  failure is returned; a link to no file is refused before anything is
  written, a link into a directory this user cannot write names the remedy,
  and every entry resolves the config's relative paths against the
  directory `config.Load` takes from the name it was given, unmoved; serve's
  console save (`RuntimeConfig.Update`, `UpdateChecked`) takes its turn and
  compares before it runs its change, saves only into the file serve's boot
  read was of (`config.LoadFileAt`: the name resolved before each read, the
  file it resolved to read, the name resolved again after it, a read
  answered only when both resolutions agree, a name that never did refused,
  and one resolved at no side of any read booting with no file, every
  console save then refused before its turn until a restart,
  `*config.BootUnresolvedError`; `RecordFile` takes the file as it is, never
  `""`) and over the bytes the holder last read or wrote, refuses a file
  another writer changed, or a name repointed since, at its turn, its check
  or its save, with a `*config.ChangedOutsideError` rather than merge, and a
  change of its own a late refusal would leave done asks the turn's
  comparison again just before it (`check`); what is left is the window
  between the turn's final check and the rename, a writer that takes no
  lock, and the time between a check and the work it authorizes** (2026-10-24,
  backlog B506; the second round, the same day, took the first round's
  reviews, the third the second's, the fourth the third's, the fifth the
  fourth's, and the sixth, the next day, the fifth's, below). Until then
  several writers read the file, changed it and wrote it
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
  config file's base name and `.lock` (since the third round the base name
  of the file the config's name opens, a link's target's), beside the
  config file, opened
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
  path (a link there opens the same directory, so it is left as it is;
  until the fourth round, which names that directory by the handle the OS
  opens for it, since a junction left as it is spells a repointed junction's
  new directory the same way: below). The
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
  config 4242's; the lock was given 0:0, the directory's; the directory's
  is 4300:4301 since PR #1345's review, so that a run as root, whose new
  file is already 0:0, still sees the change), through
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
  `atomicwrite`'s 750 ms of rename retries; since the third round, on
  Windows, up to 750 ms of retries of the turn's first read, and up to
  `saveWindow`, 2 s, of a save's comparisons and rename attempts together,
  below); the long turns are a CLI's own,
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
  ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION from LockFileEx; since the
  third round the config's lock asks ENOLCK apart first, `noLockToGive`, and
  asks it again instead, below, so for it only the rest are that answer),
  the turn is taken without the lock (`FileLock.Unheld` says why), said once per file
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
  just before its rename** (the second round; since the third, before each
  attempt at its rename, below). `LockFile` reads the file once
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
  nothing; a save that fails after it, or that the comparison just before
  the rename refuses (a writer that takes no turns), retargets the carry and
  runs the compensating scan, as a failed save always did. **This said such
  a failure "lands where a scan heals", and that was true of the rows
  alone** (the third round, Codex's and the Claude review's findings on the
  second): the compensating scan puts the tracks' rows back, and nothing
  puts back the renditions and waveforms the wipe or the prune unlinked; and
  the window such a writer could land in was all of `fn`, the wipe or the
  prune itself included, and the save's staging, never the staging alone.
  The third round narrowed it to the index change itself and the staging,
  and says what a refusal there costs (below). Doing the change after `Update` returned was weighed: it
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
  not. (The third round replaced that rule with a stricter one: the turn's
  own read, below.) An entry nothing writes any more fails it, and so does a method
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

  **The third round** (the same day) took the reviews of the second
  (Codex's five findings, the Claude review's four), as the orchestrator
  decided them and then amended the decisions after two reviews of that
  brief. Each fix was red first, on the second round's code with only the
  seams of two commits that change no behaviour (e667797c: a hook just after
  a turn's read, `config.OnTurnTakenForTest`; the turn's read through one
  function a test replaces, `config.SetTurnReadForTest`, and
  `config.ReadFile`; what atomicwrite's read retry asks again on,
  `atomicwrite.SetReadRetryableForTest`; `RecordFile`'s path; `SaveIf`'s
  callback taking the save's context; a restore's `ConfigAsRead` and its
  hook before its first write. 5631bcb5: the two refusals the tests name,
  returned by nothing yet). The fixes are d61af204, and the rename loop's
  own tests 2fb0d5c4. The restore's half is cli-serve.md's.

  **ENOLCK is asked again, never taken for no locks** (Codex's first).
  Linux's flock answers ENOLCK when the kernel cannot allocate a lock
  record, on any filesystem and while another open holds the lock, and an
  NFS mount whose lock manager is down answers it too; the second round
  read it, with `fsutil.KeepsNoLocks`, as a filesystem that keeps none, so
  one such answer took the turn without the lock beside a writer that held
  it. `lockFile` now asks ENOLCK again every `lockRetry`, as it asks a held
  lock (a config-only classifier, `noLockToGive`, so `KeepsNoLocks` keeps
  its meaning; this said "for its other readers", and it has none: the
  fourth round's correction, below), and a wait whose every try answered
  ENOLCK ends in a `*NoLockManagerError` ("the filesystem holding <path>
  gave no lock beside it for 5s (…), so nothing was written: it has no
  working lock manager; mount it with local locking, or start its lock
  service, and try again"), matching `ErrNoLockManager` and the ENOLCK it
  wraps; a try in the same wait that found the lock held makes it a
  `*BusyError`. No verdict is kept: the brief's first form remembered one
  for the path for the process's life, which both reviews of the brief
  showed would leave a serve unlocked for good after a passing shortage of
  lock records. Red before: `TestAFilesystemThatGivesNoLockRefusesTheTurn`
  (a holder, every try ENOLCK: the turn taken, unheld),
  `TestNoLockToGiveThenAHeldLockIsBusy` and
  `TestNoLockToGiveOnceIsAskedAgain` (one ENOLCK, then a free lock: the turn
  taken unheld).

  **Every rename attempt is compared, and a refusal ends the retries**
  (Codex's second, and both reviews of the brief). On Windows
  `RenameWithRetry` tries the rename again for up to 750 ms while another
  process has the file open, and the second round compared the file once,
  before the first attempt: an editor that saved the file while the first
  attempt failed had its change renamed over by the next.
  `atomicwrite.RenameWithRetryIf(ctx, src, dst, before)` asks `before` ahead
  of every attempt, the first included, returns its refusal at once with no
  attempt after it (a refusal asked again could find the file put back as
  it was between attempts, and write over the change it refused), tries
  again only on the rename's own failures, on `RenameWithRetry`'s schedule,
  and stops when ctx ends. `FileLock.write` passes it the turn's comparison
  and `SaveIf`'s (bind's) under one context of `saveWindow` (2 s), which
  bounds each comparison's read retries too, so a slow turn stays inside
  another writer's `LockWait`; `SaveIf`'s callback takes that context. Red
  before: `TestAChangeBetweenTwoRenameAttemptsIsRefused` (the change renamed
  over at the second attempt), `TestARefusalBetweenRenameAttemptsEndsTheRetries`
  (the other writer puts the file back once the comparison has read its
  change), `TestSaveIfIsAskedBeforeEveryRenameAttempt` (asked once around
  two renames). `TestAnUnchangedFileSavesAcrossAFailedRenameAttempt` and
  the loop's own `TestRenameWithRetryIfEndsAtTheConditionsRefusal`,
  `TestRenameWithRetryIfIsBoundedByItsContext` and
  `TestRenameWithRetryIfRenames` are guards.

  **A root change compares the file once more just before its library
  index change, and a refusal after that change costs what the change
  costs** (Codex's third, the Claude review's third). What rebuilds what a
  wipe or a prune drops, read in the code: nothing adopts a sidecar file
  back into a row (the only writers of `track_variants` and
  `track_analysis` rows are the transcode pool, the CLI renderers and the
  analysis pool; the scanner skips sidecar-shaped names; `LocateSidecar`
  classifies a row that exists; the variants watcher works from listed
  rows), and deleting a track cascades its variant and analysis rows
  (`FOREIGN KEY … ON DELETE CASCADE` in the store's schema). The renditions
  come back as they first came: `optimized-*` and `optimized-dsd-*` through
  the auto-optimize pre-generation sweep (off by default; the compensating
  scan nudges it; at most `maxPerSweep`, 200, a sweep) and on request (the
  app's CarPlay generate, `POST /v1/upscale` with kind optimize, the
  batches, `bridge optimize`); `upscaled-*` and `pcm-*` on request only
  (`POST /v1/upscale`, `POST /v1/upscale/batch`, the console's Generate,
  `bridge upscale` and `bridge render`), no sweep rendering a first one;
  waveforms through the analysis sweep (off by default; every track without
  a fresh analysis row) and `bridge analyze`. A download makes none (a
  variant with no row is 404 `variant_not_found`). Sidecar paths are
  deterministic, so a render lands where the old file was. A successful 1→N
  add or collapse drops the same renditions (every row's path form changes),
  a successful remove the removed root's. The root handlers now save through
  `RuntimeConfig.UpdateChecked`, which hands `fn` the turn's comparison
  (`check`: `FileLock.Check`, which reads the file again), and ask it just
  before the wipe or the prune, so a file a writer that takes no turns
  changed after the turn read it refuses the change before anything is
  unlinked (the console's 500 `save-config`, the `*ChangedOutsideError`'s
  words). What is left is such a writer landing during the index change
  itself or the save's staging: refused after the change, at the cost above
  (the renditions and waveforms it unlinked, rebuilt as above, and the rows,
  which the compensating scan puts back). Deferring the unlinks until the
  save had landed was the brief's first form and was dropped (Codex's review
  of the brief): the cascade drops the rows that name the files, so nothing
  adopts them, the background orphan sweep reaps unreferenced files, and
  work after `Update` returns runs outside both locks. Red before:
  `TestARootChangeRefusedJustBeforeItsIndexChangeLeavesTheLibraryAlone`, an
  edit landed after the turn's read (`OnTurnTakenForTest`): the add, the
  prune and the collapse each unlinked the rendition.

  **One file for every entry, and the base of the config's relative paths
  does not move** (Codex's fourth, the Claude review's fourth, and both
  reviews of the brief). `fsutil.ResolveParent`, which `lockFile` and
  `turnKey` ask on every turn, now follows a link at the last element to its
  target (`filepath.EvalSymlinks` of the whole name; on Windows since the
  fourth round `fsutil.ResolveLinks`, below) and answers the
  target's directory and name: the lock sits beside the target, named after
  it (`alias.yaml` → `actual.yaml` locks `.actual.yaml.lock` whichever name
  a writer is given), and the save renames onto the target, so the link
  stays a link. Directories that do not exist yet resolve through their
  nearest existing ancestor, the missing ones after it as written (`bridge
  init --dir` below a linked ancestor). A link to no file is refused
  (`fsutil.ErrDanglingLink`), and `LockFile`, `config.CheckName` (init's,
  before it writes anything) and `LoadFile` give one refusal naming the link
  and what to do ("…is a symbolic link to a file that is not there, so no
  bridge command reads or writes it: point the link at the config file, or
  put the config file where the link is"): a writer there neither makes the
  file the link names nor replaces the link with one. A link into a
  directory this user cannot write (a home-manager store, a read-only
  mount: EACCES or EROFS at the lock or the save's staging) is refused
  saying to point the link at a writable copy or put the file in its place.
  Serve and init keep the config's name as written (`fsutil.AbsAsWritten`:
  on unix the working directory joined by hand, an empty element and "."
  dropped, a ".." kept; on Windows `filepath.Abs`), so they open the file
  the OS opens for it, which every CLI writer has since the second round:
  serve cleaned `cfg/link/../bridge.yaml` to `cfg/bridge.yaml` and loaded,
  locked and saved that file, and its console cleaned the name again for its
  saves; `bridge doctor` loads the name as written too. **The base of the
  relative paths is the one `config.Load` takes from the name as given, as it
  always was**: Load cleans the name for it (`filepath.Dir(filepath.Abs)`), so
  `dataDir: data` in a config reached through a link at its last element
  resolves beside the link, and through a `..` after a link against the
  directory the name cleans to. The brief's first form moved it to the
  target's directory, which would boot serve on a fresh database and identity
  for an install whose config is a link (the Claude review of the brief).
  Serve's holder records the file its name opened at boot (`RecordFile(path,
  raw)`, which resolved the name when it was called, a thousand lines of
  boot after the read, until the fourth round: below), and refuses a console
  save through a link repointed since (a dotfiles restow, a home-manager
  switch) with its `*ChangedOutsideError`, however alike the two files are. Red before:
  `TestResolveParentFollowsALinkAtTheLastElement`,
  `TestResolveParentResolvesMissingDirectoriesThroughTheirNearestAncestor`,
  `TestResolveParentRefusesALinkThatLeadsNowhere`;
  `TestALinkedConfigsWritersTakeTheLockBesideItsTarget` (no lock beside the
  target, and an Edit through the target's name went through while a turn
  through the link was held), `TestALinkThatLeadsNowhereIsRefused` and
  `TestALinkIntoADirectoryThisUserCannotWriteSaysWhatToDo` (a save renamed a
  file over the link), `TestARepointedLinkRefusesTheHoldersSave`; in
  `cmd/bridge`, `TestServeOpensTheConfigItsNameOpens` (serve saved the decoy
  `cfg/bridge.yaml`, and over the link), `TestLibraryAddWritesTheConfigItsNameOpens`
  (over the link; its `..` case passed, the second round's), and the seed's
  and init's refusals of a link to nothing (cli-serve.md's).
  `TestInitMakesANestedConfigDirUnderALinkedAncestor` is a guard.

  **The turn's reads ask again while Windows refuses them for a moment, and
  a named pipe is still refused at once** (the Claude review's second).
  `config.ReadFile(ctx, path)` reads through `fsutil.ReadAsFile` within
  `atomicwrite.ReadWithRetryCtx`, which asks again only on
  ERROR_SHARING_VIOLATION and ERROR_ACCESS_DENIED, never on a file that is
  not there; the turn's first read, every comparison and bind's own
  comparison read through it, and bind reports a read that fails for good as
  that read's failure, never as a change (Codex's review of the brief).
  Tested on every platform through the turn's read and atomicwrite's
  question, with an error of the test's own standing for Windows' codes;
  those codes are atomicwrite's `TestReadRetryWindowsErrors`, on CI's
  Windows leg. Red before: `TestATurnsReadsAskAgainWhileTheFileIsBusy` (each
  of the two reads failed the save),
  `TestBindsOwnComparisonAsksAgainWhileTheConfigIsBusy` and
  `TestBindsOwnComparisonSaysAReadFailedForWhatItIs` (bind said the config
  had changed). `TestATurnRefusesANamedPipeAtOnce` and
  `TestATurnsReadOfAMissingFileAnswersAtOnce` are guards.

  **The sweep asks for the turn's own read** (Codex's fifth, and the Claude
  review of the brief). A writer that saves itself must, between its lock
  and its first save, decide on what its turn found: `FileLock.LoadFound`
  (the config loaded from the turn's bytes, as `Load` loads the file, the
  base from the name), `FileLock.Found` (those bytes, and whether there was
  a file), or a helper `turnReadHelpers` names (init's
  `configChangedSinceRead`, a restore's `configStillAsRead`), which the
  sweep requires to make one of those reads itself; and no other read of the
  config may come between the lock and the save, a `Stat` and a `Load`
  among them. The second round's rule took any read after the lock, so
  `Load; LockFile; Stat; Save` passed (Codex's case), and one that counted
  the read inside `LockFile` would have let `Load; LockFile; Save` through
  (the Claude review of the brief): both are reported now, beside a positive
  case of each turn read. Bind decides on `LoadFound` and compares with
  `Found`, the seed on `Found` (a turn that found no file), init's check and
  a restore's on `Found`, `Edit` on `LoadFound`, and the holder's
  `UpdateChecked` on `Found`. Red before: `TestEveryConfigWriterTakesTheConfigLock`
  (the seed's `os.Stat` and bind's reads in the turn, `Edit`'s and
  `Update`'s reads of the turn's fields, a restore with no turn read, and
  `configChangedSinceRead` reading the file itself). The rule line and the
  log said the sweep held the save to the turn's own bytes; it held the
  writer to making one of the turn's reads, and a save of a copy read
  before the lock, beside a `Found` whose result it threw away, passed
  (both reviews of the third round): the fourth round holds the saved value
  to the turn, below.

  **The fourth round** (the same day) took the reviews of the third
  (Codex's three Mediums and two Lows, the Claude review's three Lows), as
  the orchestrator decided them, with the decisions it took on two reviews
  of its brief. Each fix was red first, on the third round's code with only
  the seams of two commits that change no behaviour (6d3b7d9b: a save window
  a test can shorten, `config.SetSaveWindowForTest`; the file a turn of a
  name locks, `config.ResolveFile`, which `turnKey` asks; `LoadFileAt`
  beside `LoadFile`, which nothing in production called yet; a hook after
  serve's boot read, `serveOpts.afterConfigRead`; and the tests that tell
  the holder a boot read handing it the resolved file. 3e502489: the two
  refusals the tests name, returned by nothing yet). The fixes are bfa8bd71
  (the rename loop's window) and 7c8a6d3b (the rest), and d3a58c41 pins a
  restore's early refusal of a moved name to a changed config's words. The
  rename-failure seam decision 6 asks for was already there,
  `atomicwrite.SetRenameFuncForTest`, which the third round's
  `failFirstRename` drives from `internal/config`'s tests; no second one was
  added. The restore's and serve's halves are cli-serve.md's.

  **A turn resolves the config's name again once it holds the lock, before
  each rename attempt and at its check** (Codex's first Medium, the Claude
  review's third Low, and both reviews of the brief). A turn resolved the
  name, waited for the lock beside the file it named, then read, compared
  and saved that file: a link at the name repointed while a writer waited
  (Codex's sequence: serve read `alias.yaml` → A, a console save resolved A
  and waited for A's lock, the alias was repointed to B, and A's lock was
  given back with A unchanged) had the save compare A, replace it, run the
  roots' side effects and answer success, while the name opened B,
  untouched. `lockFile` now resolves the name again once the lock is held,
  before its read (`stillNamed`), and the comparison before each rename
  attempt (`stillAsFound`, the one `FileLock.Check` asks too) resolves it
  again before it reads (each before the read it guards, which left a link
  repointed during that read unseen: the fifth round moved every one after
  it, below): a name that opens another file than the turn
  locked, or none (a link repointed to nothing), is a `*NameMovedError`
  (`ErrNameMoved`; "… now opens another file than the one this writer
  locked (a link on its way was repointed while it waited for its turn or
  had it), so nothing was written: run it again, to work on the file the
  name opens now"), the lock given back, nothing written, and nothing tried
  again on the new file: a CLI writer refuses with it, and serve's holder
  answers its `*ChangedOutsideError` at the turn, at fn's check and at the
  save (`changedOutside`, which maps `ErrChangedInTurn` and `ErrNameMoved`
  alike; the error's words now say "or now opens another file (a link at it
  repointed)"). The roots' wipe and prune ask the check just before they
  run, so a link repointed before that check stops them; one repointed
  during the change itself, or while the save stages, no check before the
  change can see, and the save refuses after it, at the cost the third
  round's paragraph above gives, the compensating scan bringing the rows
  back. Red before, every link repointed to a twin holding the same bytes:
  `TestAConsoleSaveRefusesANameRepointedWhileItWaited` and
  `TestAnEditRefusesANameRepointedWhileItWaited` (the change run and the old
  target saved), `TestARepointBeforeALaterRenameAttemptEndsTheSave` (a
  repoint while the first rename failed: the second attempt renamed onto the
  old target), `TestAConsoleCheckSaysAMovedNameIsAChangeOutside` and
  `TestAConsoleSaveSaysANameMovedDuringItsSaveIsAChangeOutside` (the
  mappings), and in `internal/admin`
  `TestARootChangeRefusesANameRepointedInItsTurn` (a repoint after the
  turn's read: the add, the prune and the collapse each unlinked the
  rendition and saved the old target) and
  `TestARootAddWhoseNameMovesAfterItsWipeIsCompensated` (a repoint while
  the save staged: 201 before, now 500 with the rows back through the
  compensating scan).

  **Serve's holder records the file its boot read opened, resolved at that
  read** (the Claude review's first Low). `RecordFile` resolved the name a
  thousand lines of boot after `LoadFile` read it, so a link repointed in
  between had the holder record a file it never read, and the first console
  save wrote into it however alike the two were. Serve now reads through
  `config.LoadFileAt`, which resolves the name just before and just after
  the read (`ResolveFile`), reads again when the two differ, up to three
  reads in all (`bootReads`), and refuses a name that moved across each
  (`*NameMovingError`, `ErrNameMoving`: "… opened another file before each
  of its 3 reads than after it (a link on its way was being repointed), so
  it was not read: start again once the link is settled"); the holder is
  told the file the answered read opened, and `RecordFile(target, raw)`
  takes it as it is, resolving nothing. The base of the config's relative
  paths stays the directory `config.Load` takes from the name as given,
  whichever read is answered (decision 5). Red before:
  `TestTheBootReadReadsAgainWhenTheNameMovesAcrossIt` (one file's bytes
  answered with another's name), `TestTheBootReadRefusesANameThatKeepsMoving`,
  and serve's `TestServeRecordsTheFileItsBootReadOpened` (cli-serve.md's).
  `TestTheBootReadResolvesRelativePathsAgainstTheNameAsGiven` (an alias and
  its target in different directories, relative data dir, library root and
  TLS pair, before the name moves and after) and the third round's
  `TestARepointedLinkRefusesTheHoldersSave`, now through `LoadFileAt`, are
  guards. The read itself still went through the name, and two failed
  resolutions compared equal as `""`: the fifth round reads the file it
  resolved and takes no failed resolution for a file, below.

  **No rename once the save's window has ended** (Codex's second Medium).
  `RenameWithRetryIf` asked its context only during the waits between
  attempts, so a comparison that answered nil after the window had ended (a
  read Windows held, a slow mount), or a first attempt made after it, still
  renamed. It now asks the context before each condition and again once the
  condition has answered, before each rename, and an ended context refuses
  with no rename after it, wrapping the last rename's failure when one
  failed (`renameStopped`); a config save says its comparisons and rename
  did not finish within the window, so nothing was written. Red before:
  `TestRenameWithRetryIfRenamesNothingOnceItsContextEnds` (a condition that
  ends the context and answers nil; one that answers past the deadline; a
  context ended by the condition after a failed rename),
  `TestRenameWithRetryIfAsksNothingOfAContextAlreadyEnded`, and through a
  config save `TestASaveRenamesNothingPastItsWindow`.

  **On Windows the turn works on the directory the OS opens, named by its
  handle** (Codex's third Medium, Codex's High on the brief, the Claude
  review of the brief). Since Go 1.23 a junction (`mklink /J`) is
  `ModeIrregular` and `filepath.EvalSymlinks` refuses one inside a path with
  ENOTDIR, so the third round's EvalSymlinks of the whole name refused a
  file link inside a junction'd directory (serve loaded such a config, and
  no writer could take its turn), and a plain file there was answered by
  the junction's own spelling, which a junction repointed after the lock
  spells the same way: the turn's reads and rename reached the new
  directory under the old one's lock. `resolveParent` on Windows now names
  the directory by the handle the OS opens for it (`physicalDir`, over
  `fsutil.ResolveLinks`: GetFinalPathNameByHandle, the `\\?\` prefix taken
  off, so two resolutions of one unmoved name compare equal), through its
  nearest existing ancestor for one not made yet, and refuses one that is
  not a directory or that ResolveLinks's fallback leaves a link (by
  `os.Lstat`'s `ModeSymlink` or `ModeIrregular`, which refused every
  reparse point that is no link as well, OneDrive's folders among them: the
  fifth round's correction, below);
  `followLast` follows a link at the last element with `ResolveLinks`
  everywhere (on unix that is `filepath.EvalSymlinks`, so unix is
  unchanged), a not-found answer refused as `ErrDanglingLink` and any other
  failure an error, never the link answered as itself. Windows only, and
  compiled and vetted here; the Windows host runs
  `TestResolveParentFollowsAJunctionToTheConfigsDirectory`,
  `TestResolveParentFollowsAFileLinkInsideAJunction`,
  `TestResolveParentRefusesADanglingLinkInsideAJunction`,
  `TestResolveParentAnswersOneSpellingForAnUnmovedName` and
  `TestResolveParentSeesAJunctionRepointed` (`internal/fsutil`), and
  `TestATurnThroughAJunctionLocksBesideItsTarget`,
  `TestAJunctionRepointedInATurnRefusesItsSave` and
  `TestADanglingLinkInsideAJunctionIsRefused` (`internal/config`); a test
  that needs a file symbolic link skips where Windows refuses this process
  one.

  **The sweep holds a writer's save to its turn's bytes** (Codex's Low, the
  Claude review's Low, and both reviews of the brief). A named writer that
  saves itself must now save a value taken from its turn: the config
  `LoadFound` gave, the bytes `Found` gave, or a value made from one of them
  (`fromTurn`, read through the last assignment to a name before the save,
  by name and source position), `SaveBytes`'s argument included; or save
  only where its turn found no file, after an if that returns when the
  second result of the turn's `Found` says a file was there (`createOnly`,
  serve's seed); or be listed in `comparedSaves` with the comparison of the
  turn's bytes it makes in its turn instead, which the sweep requires it to
  ask, of the turn or of what it found: init (`configChangedSinceRead`), a
  restore (`configStillAsRead`; putting the snapshot's bridge.yaml over the
  file is what a restore is for) and the holder's `UpdateChecked`
  (`bytes.Equal` of `Found`'s bytes and the holder's own). An entry no
  longer needed is reported, and so is one the sweep no longer finds.
  `RuntimeConfig.Update` saves through `UpdateChecked`; bind and `Edit` save
  what `LoadFound` gave (bind's `SaveIf` comparison is a second guard, not
  where its config comes from). The self-test reports
  `Load; LockFile; Found (thrown away); Save`, the same with `Found`'s
  result assigned and not used, and with it checked and the stale copy
  saved, `cfg := LoadFound; cfg = stale; Save`, `found := Found;
  SaveBytes(stale)`, a listed writer that never asks its comparison and a
  listed one that needs none, and lets through `SaveBytes` of a value made
  from `Found`, bind's shape, and a listed writer asking its comparison.
  Red before: `TestTheConfigWriterSweepReportsEveryShape` (21 problems of
  29). What it cannot see: a name shadowed in a block, or assigned in a
  loop after it is saved, is read as its last assignment in the source.
  Nor did it ask where a guard or the listed comparison stood, or whether
  the comparison's answer was used, and the log called it enforcing: the
  fifth round's rules and its list of what the sweep cannot see, below.

  **`fsutil.KeepsNoLocks` still counts ENOLCK** (the Claude review's second
  Low): this file said ENOLCK had left it in the third round, and it had
  not. Only the config's lock tells ENOLCK apart (`noLockToGive`), and it is
  `KeepsNoLocks`'s one caller. Serve's data-dir lock (`lockServeDataDir`)
  and the store's migration lock (`manifest`'s ladder lock) do not ask it:
  each goes on without its lock on any answer of `TryLock` but a held lock,
  ENOLCK included (serve says so on stderr and serves on, the migration
  logs a Warn and migrates), so a kernel out of lock records can let a
  second serve start beside the first. The brief named them as
  `KeepsNoLocks`'s readers; they are those two locks, with rules of their
  own. Changing them is out of scope here, and backlogged.

  **The fifth round** (the same day) took the reviews of the fourth
  (Codex's three Mediums and its Low, the Claude review's Medium and two
  Lows), as the orchestrator decided them, and ends the hunt for a link
  repointed under a turn. Each fix was red first, on the fourth round's code
  (6e03497f) with only the seams of three commits that change no behaviour
  (af59db9b: `LoadFileAt` answering a `BootFile`, which `RecordFile` takes,
  hooks before and after each boot read, `bootHooks`, and the boot read's
  resolution through one function a test replaces,
  `config.SetBootResolveForTest`; 6f6c4ffb: the Windows directory check as
  an untagged decision over the reparse tag of the directory's own entry,
  `checkResolvedDir`, with the tag reader and the resolver passed in,
  deciding as `os.Lstat`'s mode had; 7ee08187: the refusal the tests name,
  returned by nothing yet). The fixes are 48b2664a (the order of the name's
  resolution), 6f63cfef (the boot read), 38a046e5 (Windows) and 71d01baf
  with 91ca1e85 (the sweep). The restore's and serve's halves are
  cli-serve.md's.

  **The window left, stated once.** What no turn closes is *the time
  between the turn's final check (the name still opens the target, and the
  target's bytes are as found) and the rename syscall itself, plus any
  writer that takes no lock, and, for work a check authorizes (a root
  change's index change, a restore's writes, an Edit writer's own work), the
  time between that check and the work*. No rename can be made conditional
  on the file it replaces or on the file a name opens, and no check can
  follow the work it authorizes; every refusal before that check writes
  nothing, and one after the work refuses its save. A later finding inside
  that window is the window, not a new defect.

  **A turn resolves the name after every read its decision rests on, last**
  (Codex's first Medium; decision 2). The fourth round resolved the name
  before the reads: `stillNamed` ran at the start of `stillAsFound`, before
  its read, and `lockFile` asked it before `find`. A link repointed while
  one of those reads ran (or while Windows had it asked again) left the
  read answering the locked file, unchanged, and the name opening another:
  the save renamed onto the locked file and answered nil, a check let the
  root change's index change or the restore's writes run, and `Edit` ran
  its change on a turn whose read the name no longer opened. Now
  `stillAsFound` only reads and compares; the save's condition asks, before
  each rename attempt, the byte comparison, then the writer's own comparison
  (`unchanged`, bind's `SaveIf` read, which reads the config through its
  name), then `stillNamed`, last; `Check` reads, then resolves the name; and
  `lockFile` resolves it after `find`'s read, so an `Edit` writer's work
  runs only after a resolution that follows the read. The sweep requires
  `FileLock.write` to call `stillNamed` itself. Red before, each test
  repointing the link inside the nth read the turn makes (counted through
  `config.SetTurnReadForTest`) to a twin holding the same bytes, and asking
  which file that read was of, so that none passes by repointing elsewhere:
  `TestASaveRefusesANameMovedDuringItsComparisonRead` (the second read: the
  locked file written, nil), `TestASaveIfRefusesANameMovedDuringItsOwnComparison`
  (the third, the writer's own read through the name),
  `TestAConsoleCheckRefusesANameMovedDuringItsRead` (the change ran),
  `TestAnEditRefusesANameMovedDuringItsTurnsRead` (the first: the change
  ran), and through the writers `TestBindRefusesANameRepointedDuringItsOwnComparison`
  (exit 0, the locked file bound), `TestLibraryAddRefusesANameRepointedDuringItsTurnsRead`
  (the index wiped) and `TestARestoreRefusesANameRepointedDuringItsCheck`
  (every file before bridge.yaml restored). The controls put each
  resolution back before its read, and one puts it between the two
  comparisons, which reddens only the `SaveIf` cases.

  **The boot read reads the file it resolved** (Codex's third Medium;
  decision 3). `loadFileAt` resolved the name, read it through the name, and
  resolved it again, so a link pointed at B during the read and back at A
  before the second resolution answered B's bytes as A's, both resolutions
  agreeing. Each read now goes through the file resolved just before it
  (`readConfigTarget`, its failure said as `LoadFile` says it, of the name as
  given), and is answered only when the resolution after it agrees; the
  config's relative paths still resolve against the name as given. A file
  the agreeing resolutions name that is not there, or cannot be read, is
  the read's error at once. Red before: `TestTheBootReadReadsTheFileItResolved`
  (the hook between the first resolution and the read points the link at
  B, the one after it back at A: B's bytes answered with A).
  `TestTheBootReadOfAMissingConfigSaysItIsMissing` and
  `TestTheBootReadOfALinkToNothingSaysWhatToDo` are guards.

  **A failed resolution is never a file** (Codex's second Medium, the
  Claude review's first Low; decision 4). `resolvedOrNone` mapped a failure
  to `""`, two of them compared equal, the boot answered `""`, and the
  holder skipped `""` when it compared: a save went into whatever file the
  name opened then. Decision 4's table, after the retries: both resolutions
  agree, the read through the file is answered with it; they differ, or
  exactly one side resolved, at every read, a `*NameMovingError` (its words
  now "did not open one file both before and after any of its 3 reads"); both
  failed at every read, the boot keeps the last read, made through the
  name, with no file (`BootFile` with none). The holder keeps that state
  apart from "told nothing" (whose first save goes through, as before):
  `UpdateChecked` refuses at its top, before `LockFile` and before fn runs,
  with a `*BootUnresolvedError` (`ErrBootUnresolved`: "this bridge could not
  resolve the file <name> opens when it started, so it cannot say which file
  its settings were read from, and this change was not saved: restart the
  bridge once that name resolves, then make the change again"), and goes on
  refusing once the name resolves, since the file it resolves to then is
  not one it read; `RecordFile` never takes `""`, and the comparison no
  longer skips one. A CLI writer's own turn resolves the name itself and is
  not refused. Red before: `TestTheBootReadRefusesANameResolvedOnOneSideOfARead`
  (its third case, no side of the first read and one side of the others,
  answered at once with `""`), `TestABootReadThatResolvesNothingKeepsItsReadWithNoFile`
  (one read, not three), `TestAHolderBootedWithNoFileRefusesEverySave` (the
  save went through, its turn taken), and serve's
  `TestServeRefusesConsoleSavesWhenItsBootCouldNotResolveTheConfig`
  (cli-serve.md's). Its first two cases, one side at every read, were
  refused before and are guards.

  **On Windows a reparse point that is no link is a directory** (the Claude
  review's Medium, a regression of the fourth round; decision 1).
  `physicalDir` refused a resolved directory whose `os.Lstat` mode carried
  `ModeSymlink` or `ModeIrregular`, and since Go 1.23 `ModeIrregular` marks
  every reparse point that is not a link, a socket or a deduplicated file:
  a config under a OneDrive folder (`IO_REPARSE_TAG_CLOUD_*`) or in a
  container layer (WCI) had every writer refused and the boot resolving
  nothing. `checkResolvedDir`, untagged, reads the tag of the directory's own
  entry (on Windows `entryOf`: `GetFileAttributesEx`, and for a reparse point
  `GetFileInformationByHandleEx`'s FileAttributeTagInfo on a handle opened
  with FILE_FLAG_OPEN_REPARSE_POINT, as `os.Lstat` reads it), refuses a name
  surrogate (IsReparseTagNameSurrogate's bit: a symbolic link, a junction, a
  mounted folder, a WSL link) and a directory whose resolution, asked
  again, answers another, and takes every other tag for a directory. Red
  before, portable: `TestCheckResolvedDirRefusesOnlyALinkOrAMove` (cloud,
  placeholder and container tags refused; a moving resolution and a failing
  one taken), whose surrogate cases are red under a reading that refuses the
  symbolic link's tag alone (a control). On Windows only, compiled and
  vetted here: the new `TestEntryOfReadsTheEntryItself` (a plain directory,
  a junction and a file, read as their own entries), beside the fourth
  round's junction tests. No test makes a cloud-tagged folder natively: that
  needs a sync provider, so the decision over the tag is the portable test's.

  **The sweep holds a guard and a comparison to the save they guard**
  (Codex's Low, the Claude review's second Low; decision 6). `createOnly`
  took any guard after the lock and before the save, one in a nested block
  or an uncalled closure included, and `asksInTurn` any call of the listed
  comparison, its answer dropped included; and an assignment in a block
  counted for the name outside it. Now a guard counts only as a statement of
  the function's own body; a listed comparison only when it decides an if
  whose body returns before the save, at the top of the body or directly
  inside a top-level check of the turn against nil, in one of three forms:
  the call, or its negation, in a condition of `&&` and `||` (init's; the
  holder's `fileKnown && (… || !bytes.Equal(found, r.file))`), or an error
  the if's initializer takes from it that the condition finds non-nil (a
  restore's `if err := configStillAsRead(…); err != nil`); and a name's last
  assignment is that of the variable the parser resolved it to. **The
  restore fails the strict rule**: its comparison sits inside `if configTurn
  != nil { … }`. It is reported, not restructured: the sweep accepts that
  one nesting, a check of the turn itself, since a save through a nil turn
  writes nothing (the controls that drop the nesting, or the initializer's
  form, redden the tree's sweep on `RestoreWithOptions`; the one that drops
  `&&` and `||` reddens it on the holder). The self-test reports the nested
  guard, the guard in an uncalled closure, the shadowed turn read, the
  dropped comparison, the comparison in a nested block, in an uncalled
  closure, under another name's nil check, and one whose body does not
  return; it passes the three forms and the restore's nesting. Red before:
  `TestTheConfigWriterSweepReportsEveryShape` (29 problems of 36, and the
  new message; the shape under another name's nil check came after the fix,
  37, and its control is red), `TestEveryConfigWriterTakesTheConfigLock`
  (`FileLock.write` calling no `stillNamed`). **What it cannot see** was
  listed here as each pinned as passing in the self-test, when three of its
  seven shapes were, and with the nil-check gap described as a save inside
  the check rather than a save after it through another name: the sixth
  round's list, below, is the one, word for word `derivationProblem`'s.

  **The sixth round** (2026-10-25) took the reviews of the fifth (Codex's
  Low, the Claude review's Low; neither found a High or a Medium), as the
  orchestrator decided them. It changed test code alone, the writer sweep
  in `cmd/bridge/config_writers_test.go`, and these docs: no production code,
  and `internal/backup/backup.go` is as it was. The fix is 8d75e471.

  **A save past a nil-checked comparison counts as refused only through the
  checked name, unmoved since the check** (Codex's Low, the Claude review's
  Low; the orchestrator's decision, from the Claude review's recommendation,
  amended by the brief's own reviews). The fifth round accepted any save
  after a comparison inside a top-level nil check of a turn name, so a
  writer could check one name and save through another: acquire `l`, alias
  it as `optionalTurn` only when `compareEnabled`, compare inside `if
  optionalTurn != nil`, then `stale.Save(l)` (Codex's shape), and with
  `compareEnabled` false the comparison is skipped while `l` is held and
  saved through. The sweep now records each save's turn apart from the value
  it saves (`storeAt.turn`: a `Save`'s or `SaveIf`'s first argument, a
  `SaveBytes`'s receiver, none for a save through a method value; the saved
  value is still what the derivation checks read, and comparing it would
  have refused the restore's `configTurn.SaveBytes(data)`), and accepts a
  save after a nil-checked comparison only when it goes through the very
  name the check tests (the variable the parser resolved it to) and nothing
  assigns that name between the check and the save (`savesThroughUnmoved`);
  a save through a method value is reported. The restore passes as it is: it
  assigns `configTurn` once, before its nil check, and saves through
  `configTurn.SaveBytes(data)` with nothing assigned to it in between. Red
  before, on 5400d608: `TestTheConfigWriterSweepReportsEveryShape` (37
  problems of 44), whose seven new shapes all passed the old sweep: the
  optional alias; a nil-able alias (`t := l; if disabled { t = nil }`)
  checked and the save made through `l`; that alias assigned again after the
  check (`t = l`) and saved through; a save through another name after the
  check, for each of `Save`, `SaveIf` and `SaveBytes`; and a method value. A
  save through the checked name with nothing assigned to it after the check,
  for each of the three, passes before and after. The controls: dropping the
  name condition lets the two aliases and the three other-name saves pass
  again (39 problems); dropping the reassignment check lets the reassigned
  alias pass again (43); comparing the saved value in place of the save's
  turn reports the tree's `RestoreWithOptions`, and the self-test's saves
  through the checked name.

  **Why the allowance stays, and the restore's comparison is not hoisted.**
  Codex's review recommended hoisting the restore's comparison to the top
  level and dropping the allowance. The hoisted form, `if configTurn != nil
  && configStillAsRead(…) != nil`, is not one of the fifth round's three
  accepted forms (the initializer's form wants the condition to be the
  error it took, found non-nil, alone), and hoisting would move the nil case
  into a helper that takes a nil turn, which the sweep cannot read; and a
  writer that fails a rule is reported, not restructured. The allowance
  keeps the nil case in the writer, where the sweep reads it, and with the
  condition above, a save gets past a skipped comparison only through a nil
  turn, which writes nothing.

  **What it cannot see, listed exactly** (the Claude review's second point).
  The self-test now pins as passing every shape the list names, and the list
  names nothing else; the nil-check entry left it, and what the condition
  above leaves unseen joined it. Word for word as `derivationProblem`'s
  comment has it: What it cannot see, each pinned as passing in
  TestTheConfigWriterSweepReportsEveryShape, and nothing else: a variable
  written through a pointer (`p := &cfg; *p = stale`) keeps its last
  assignment as its value; a loop's later pass is read as its first (a save
  before `cfg = stale` in the loop's body); a call handed what the turn
  found is taken as derived from it whatever else it is handed
  (`pick(stale, found)`); which answer of a comparison means the file changed
  (an if that returns when configChangedSinceRead finds no change passes as
  one that returns when it finds one); a conjunct beside the comparison that
  switches its refusal off (`enabled && configChangedSinceRead(…)`, the shape
  of the holder's fileKnown, which lets a holder told nothing save); an
  assignment to the nil-checked name that is not one to the sweep, through a
  pointer (`*p = other`) or in a function literal written before the check
  and called after it; and a helper in another function that writes the path
  a caller hands it (`writeBlob(cfgPath, data)`), which names no config path
  itself while its caller makes no write the sweep knows. It is a check of
  shapes, not a proof.

  **A range's key and value count as assignments** (the sixth round's
  review, Codex's Low; 4f689a1d). The reassignment check read assignment
  statements and declarations only, so `for _, t = range
  []*config.FileLock{l} {}` after the check moved the nil-checked name
  unseen, and the save through it passed with the comparison skipped. The
  walker now records a range's key and value as assignments from the ranged
  expression. Red before: the self-test reported 44 problems of 45, with
  `reassignsItsNilCheckedTurnInARange` passing. The control, not counting
  them, brings that back.

  **Don't save a copy read before the lock or decide on a read beside the
  turn, save a value not taken from the turn without an if that refuses on
  a comparison `comparedSaves` lists, accept a save past a nil-checked
  comparison through another name than the checked one or through that name
  assigned since the check, take the lock around a prompt or for a
  process's life, lock the config file itself or put the lock in the data
  dir, clean a config's name before it resolves or move the base of its
  relative paths, lock or rename beside a link rather than its target,
  follow a Windows link with `filepath.EvalSymlinks` over the whole name,
  keep a junction's own spelling or refuse a reparse point that is no link,
  write through a link to no file, read, compare or replace a file the
  config's name no longer opens, resolve the name before a read a decision
  rests on rather than after it, or retry on the file it opens now, stage a
  save anywhere but where its rename lands, make a root writer's lock file
  root's beside another owner's config, wait for it without a bound, take
  ENOLCK or any failure but a definitive no-locks answer for an unlocked
  turn, rename an attempt without the turn's comparison, retry past its
  refusal or rename once the save's window has ended, put a value of the
  file in a refusal, give `Save` a path back, drop `SaveIf`'s comparison from
  bind, let a console save write over bytes it did not last read or write or
  into another file than serve's boot read was of, read the boot config
  through its name once it resolved it, take a resolution that failed for a
  file or compare `""` as one, resolve the holder's file anywhere but at
  that read, change anything besides the config before calling `Update` (do
  it in `fn`, after the comparison, asking `check` just before), or add a
  writer of `bridge.yaml` that `configWriters` does not name.**

## atomicwrite.ReplaceDurable: replacing an entry a rename cannot replace (B539, 2026-10-28)

**`atomicwrite.ReplaceDurable` replaces an entry a rename cannot replace in one step, a directory included, without a failure emptying its name: after WriteBytesDurable's refusals it stages and fsyncs the new bytes beside path, renames the entry aside (a name beside path, checked) and the staged file into its place with no write and no sync between the two renames, puts the entry back and syncs again when the second rename is refused, and syncs the chain once both have landed; between the two renames the name is empty, so its caller keeps a second durable record.** **Don't sync between the two renames, stage after the entry moves, take an aside outside path's directory, or drop the put-back.**

Its one caller is the player discard's `sealPlayerMarker`, which replaces a damaged seal record (`player-floor-discarded.json` as a directory, a named pipe or a link). Before B539 the discard renamed the damaged entry aside first and then wrote the fresh seal; a failed or interrupted write left the seal's name empty, and the discard printed only the bare error. `player-initialized`, sealed first since B524 round 11, kept the box sealed for the moment, but the next start spent it, and once that record was lost too the following start wrote an unsealed `{"version":1}` marker and accepted an identity restore. The order now is: stage and fsync the fresh bytes beside the path; rename the obstruction aside to a unique name beside the path (checked to be in the same directory, so the aside never lands elsewhere, and compared case-insensitively, since on APFS's default and NTFS a name differing only in case is the entry itself: Argon's review found `SEAL.JSON` passing as an aside for `seal.json`, whose second rename then failed; `aside_differing_only_in_case` pins the refusal); rename the staged file into place; fsync the directory chain. A refused second rename puts the obstruction back and syncs again, so the name ends as it was found or with the fresh seal. Nothing is written or synced between the two renames, which keeps that window as short as two renames; the window itself cannot be closed without an exchange syscall (a possible follow-up), which is why the caller keeps the second record. A discard that stops at its seal records says it discarded nothing and to run `bridge reset-player --discard-floor` again.

Reds on main 2c7be326: `TestAFailedHealKeepsTheSealEntryThroughTheNextStarts` (cmd/bridge), `TestEveryCrashPointOfTheSealsHealLeavesTheBoxSealed` and `TestAFailedStepOfTheSealsHealKeepsTheEntryItFound` (internal/backup). Negative controls NC4a–NC4f (the put-back, a barrier between the renames, staging after the move, the syncs after a put-back and after both renames, the aside's directory check) each turned a named test red; the full record, the state table and the walk of B539's four steps are in `ops/History-rules-player.md`, "B539: a fresh seal synced before a damaged one moves".

## The one-line installer (backlog B475, 2026-10-30)

**The one-line installer, `scripts/install.sh`, installs what `bridge init` installs and upgrades through `bridge update`.** **Don't download over an install, or with anything but curl held to HTTPS and to a speed floor, with no cap on a moving download's total time; don't replace anything at `~/.local/bin/bridge` (a new install is for a box with nothing of the bridge on it, files left for its unit included); don't take an install the service manager runs from another file, with another ExecStart or with an environment of its own, take its files for what the manager has loaded (a daemon-reload pending is refused), or run where this shell or the manager has a BRIDGE_ variable; don't run `--no-init` on an install; don't install the program where its service's user cannot write, run init through sudo, ask GitHub for `latest`, choose a folder, enable or bind an output, or delete anything for `--uninstall`, which prints the steps; don't print a command unquoted, or without `--config`, the default settings file's included; don't report a release running that `bridge status` did not read, or a start at boot that lingering does not give; don't end the script with a bare `main "$@"`, whose prefix `main` runs; don't name a function after a word a shell keeps for itself (ksh93's alias `stop` ran `kill -s STOP` in place of the installer's `stop`).**

`curl -fsSL https://1-bit.app/install.sh | sh` (the website redirects to `releases/latest/download/install.sh`) is the owner's ask for 0.2.2. Each part below was settled against the code at e242ce2c by a design note and two reviews (Codex, Argon), whose findings overrode the first brief where they differed; round 2 (2026-10-31) took Codex's eight findings on the integrated branch, 5dbf6f48, and the orchestrator's decisions on them, round 3 (the same day) Codex's three on round 2's head, 66cfeb59, and round 4 CodeRabbit's on the PR, #1370 (each part says which).

*Pinning.* The script a release attaches is stamped with that release's version by `scripts/stamp-install.sh`, a goreleaser before-hook that writes `install.sh` at the repository root (gitignored, as `release-meta.json` is) and fails unless exactly the one unstamped line, `STAMPED_VERSION=''`, changed; `release.extra_files` uploads it and `checksum.extra_files` gives it a line in `checksums.txt`. Every download is `releases/download/v<version>/<asset>`, an immutable tag: `latest` is resolved once, by the user's own curl, when it fetches the script, so a release published between the script's download and the archive's cannot mix two releases, and a copy saved to read first installs the release it came with. The repository's unstamped copy needs `--version`. `--version` (X.Y.Z, a leading v dropped, a -suffix allowed; anything else is exit 2) pins a new install only.

*Verification.* The archive's SHA-256 is compared with the one line of `checksums.txt` whose file name is the archive's exactly (none, two, or a hash that is not 64 hex digits fail); sha256sum or `shasum -a 256`, and curl alone, with `--proto '=https' --proto-redir '=https' --tlsv1.2`, so that every redirect stays HTTPS. A download is bounded by a speed floor, never a time cap (round 4, CodeRabbit on #1370: `--connect-timeout` alone had let a server that answered and then sent nothing hold the installer for good, silent after "Downloading the bridge"): `--speed-limit 1024 --speed-time 60` takes a transfer under 1 KiB/s for a minute for stalled, curl ends it (exit 28, which `--retry 3` retries, asking for the whole file again), and one that keeps above the floor has no cap on its total time (no `--max-time`), since a slow but moving download, a Pi on Wi-Fi, must finish, while one that moves steadily under 1 KiB/s for a minute is ended as if stalled. Without curl the run is refused (exit 3) with how to install it. Round 1 fell back to GNU wget, called `wget -q -O`, which follows a redirect from HTTPS to plain HTTP (Codex's finding 3), and refused BusyBox's wget, which does not check certificates; round 2 removed the fallback (the orchestrator's decision), and wget is never run. The extracted program is copied beside the target and must run and say it is the pinned release (`bridge version`) before it is renamed into place: that catches a program this system cannot run, whatever `uname` said. A 64-bit kernel with a 32-bit userland (Raspberry Pi OS's 32-bit image on a Pi 4 or 5) gets the arm64 build, which is static (`CGO_ENABLED=0`) and runs on the kernel; a 32-bit ARM or x86 kernel is refused, since the release builds amd64 and arm64 only.

*Service identity.* The service is the per-user systemd unit `bridge init` writes. The program goes to `~/.local/bin/bridge`, owned by the user, and not to /usr/local/bin: the updater (the console's and `bridge update`) replaces the binary in place as the service's user after `preflightWritable`, so a root-owned directory would break every later update, and `sudo bridge update` would read root's config and restart root's user services. `bridge init` runs as the invoking user, never through sudo, and root is refused (with the command to run instead) because root's own user unit would run a network server as root, and an init for another user needs that user's bus, HOME and XDG paths. Root is asked for once, after saying why: `loginctl enable-linger`, so the user manager starts at boot and outlives the login, tried first as the session's own (`--no-ask-password`, which polkit allows an active session), then with sudo (its password read from the terminal; `sudo -n` without one), then `su root -c` without sudo, else printed. On a box that runs systemd the user manager is asked what it has before anything changes, so its bus must be reached first: looked for at `/run/user/<uid>/bus` when the session did not say where (XDG_RUNTIME_DIR exported, which changes nothing on the box), and without it every run is refused (exit 3), naming the login, or the lingering, that starts the manager. Round 1 turned lingering on first, waited for the manager it started, and otherwise installed the program and deferred setup (exit 4); asking the manager before any change (Codex's amendment 4) took both away. The final line says what lingering is once it has run (round 2, Codex's finding 7): on, already or by this run, and the service starts at boot; off, and it stops when the user's last login session ends; no loginctl, and lingering could not be turned on. Round 1 said the service started at boot whatever lingering was. A HOME whose passwd home differs, a system without systemd, and a HOME that does not outlive a reboot (tmpfs, an overlay over tmpfs, `boot=overlay`) are told apart: the first and last refused, the second deferred (exit 4) with the commands to run the bridge by hand.

*Upgrades.* An install is the per-user unit, read from its ExecStart exactly as init writes it (`"<program>" serve --config "<config>"`): a unit in another shape, a program or config that is gone, a program in a directory the user cannot write, a system-level unit (the runbook's or the VPS doc's), and settings no unit runs are refused with nothing changed, since which program and config to update is then a guess. The unit is also asked of the user's manager, two ways, since they can differ: as the manager has it loaded, which a start or a restart runs (`systemctl --user show`: LoadState, FragmentPath, DropInPaths, NeedDaemonReload, Environment, EnvironmentFiles, PassEnvironment, UnsetEnvironment), and its files as they are on disk (`systemctl --user cat`: the unit file, then every drop-in, beside it, under /etc/systemd/user or /usr/lib/systemd/user, or in a `service.d` for every service; it reads the files, not what the manager loaded, systemctl(1)). `read_unit` stays the file's alone, which `internal/packaging`'s test runs with no manager. Refused: a unit the manager does not have loaded as it is (LoadState other than loaded), one loaded from a file other than `~/.config/systemd/user`'s (a transient or generated unit in its place), a daemon-reload pending (NeedDaemonReload=yes: a file changed since the manager read it, so a restart would run what the manager holds), any environment the loaded unit gives the service (Environment, EnvironmentFiles, PassEnvironment, UnsetEnvironment; init writes none), named by property and by the files that set it, never by value, and, with no reload pending, which makes the files what the manager loaded, an ExecStart they set more than once. That is round 2's finding 1 (Codex): a drop-in's `Environment=BRIDGE_DATA_DIR=…` passed, and bridge update, run from the installer's environment, then read another token store and wrote its marker in another data directory than the service's. Round 2 read the files alone (`cat`), and called that the unit as the manager runs it; round 3's finding 1 (Codex) was that a drop-in removed without a daemon-reload then passed while the service kept its environment. The installer's own environment is the other half (Codex's amendment 1): the bridge takes a BRIDGE_ variable over its bridge.yaml, `--config` or not (`applyEnvOverrides`, run by Load), and bridge init, bridge update and bridge status run from here take this shell's, where the service takes its manager's. So a BRIDGE_ variable this shell exports (BRIDGE_INSTALL_* excepted, the installer's own, which the bridge reads none of), or one the manager gives every service (`systemctl --user show-environment`), is refused before anything changes, by its name. Refusing was chosen over unsetting them (the orchestrator allowed either): an accepted unit has no environment of its own, so any such variable is one the service does not have, and a refusal tells the user rather than dropping a setting they may have meant. A manager that cannot answer `cat` is refused too; "No files found for" (its own words) is the one failure that means it has none. An install is upgraded by its own program's `bridge update --yes --config <config>`, never by the installer's download: that keeps the MinClientVersion check against the paired devices (a refusal is relayed, with the override bridge update names, and nothing changes), the staged swap, the `.bak` and the boot-time rollback marker, and its restart. bridge update's "no update" and "check failed" both exit 0, so the installer compares the program's version before and after and reads its output; the version the restarted service serves is read back from `bridge status`, and a service that runs but whose release status cannot read ends the run incomplete (exit 4), with the command that checks it (Codex's finding 4: round 1 took the version it expected and exited 0). A `--version` other than the installed one is refused: no downgrade, and no release chosen over an install. `--no-init` on an install is refused (exit 3) before lingering, bridge update or a start (finding 8: round 1 skipped only the lingering, then updated, restarted and started a stopped service).

*A new install only where nothing is.* A new install is for a box with nothing of the bridge on it: no unit the manager has (LoadState not-found), no file left for one in any directory the manager reads units from (`systemd-analyze --user unit-paths`: a unit file of its name, its drop-ins in `<unit>.d` and in the directories its dashes name, `com.acoseac.1-bit-.service.d` and `com.acoseac.1-.service.d` (systemd.unit(5)), and the drop-ins for every service, `service.d`, that set an environment or an ExecStart), no settings at the default path, nothing at `~/.local/bin/bridge`. The directories are read themselves: systemctl answers "No files found" for a unit the manager has not found, before it reads any drop-in (systemd's `unit_find_paths`, v252), so files left for the unit show nowhere else until bridge init makes it and they apply to its service. Round 2's stub printed drop-ins for a unit with no file, which systemd does not, and the test of a drop-in left elsewhere passed on that (round 3, Codex's finding 2); the stub now answers as systemd does. Search paths that cannot be listed are refused. Anything at that path that no service of the user runs, the bridge itself included, is refused (exit 3), naming its own `bridge update` and `bridge init` as the way on (round 2, Codex's finding 2 and the orchestrator's decision): round 1 replaced a file there that carried the module path, which bypassed bridge update's paired-device check, its `.bak` and its marker for a bridge served from settings elsewhere. So the runs that leave the program without a service (`--no-init`, a box without systemd, an init that failed) print the commands that finish (`bridge init`, and the lingering init does not turn on), never a second run of the installer, which would refuse what they left, and a run without a terminal and without `--library` is refused before anything is downloaded (exit 3), so that the run given a folder installs.

*Init and output.* With a terminal, the installer offers the music folders the kind of box keeps (Volumio's /mnt/USB, /mnt/NAS, /data/INTERNAL; moOde's /mnt/SDCARD, /media, /mnt/NAS; ~/Music anywhere) and runs init with its standard input on the terminal; without one, `--library` gives `init --yes --library`, and none is refused before anything is downloaded (exit 3; round 1 installed the program and deferred setup), naming the folders found: no folder is chosen without the user. Nothing reads the script's own standard input. MPD detection stays `auto`; the installer prints `bridge output player` and `bridge output dac` and binds nothing.

*The console and pairing.* The console stays loopback-only. The finish prints the console's address from `bridge status`, an SSH tunnel with the same local port (B471's Origin rule), and `bridge pair --name` in the terminal, naming the program by its full path while the PATH does not find it (on Ubuntu, `bridge` is also iproute2's).

*Printed commands.* Every command printed for the user to type stands on a line of its own, indented four spaces, and is built by `command_line` or `bridge_line`, which quote each word where it needs it (`sh_quote`), so a path with a space, an apostrophe or a semicolon stays one word and runs nothing else; those of the bridge's commands that read the settings (pair, status, serve, update) carry `--config`, the default settings file's too, and the others it prints (logs; init, which takes `--dir`) take none (round 2, Codex's findings 5 and 6 and amendment 5: round 1 wrote `$TARGET init --library $LIBRARY`, `$1 logs` and `$pp_bridge pair` out by hand, unquoted, and printed pair and status without `--config` after an upgrade of a bridge whose settings were elsewhere). Round 2 left a `--config` naming the default file out; round 3 (Codex's finding 3) keeps it, since a bridge command without one reads `./bridge.yaml` first where it is run (`resolveConfigPath`; `TestResolveConfigPathPrefersLocalOverPlatform`), so one pasted in another bridge's folder acted on that bridge's settings and its token store, while an explicit one always wins (`TestResolveConfigPathExplicitAlwaysWins`).

*Uninstall.* `--uninstall` prints the steps in their order (the service stopped and checked inactive before its unit and program go; the settings, its identity and paired phones, apart; the music never) and changes nothing, offline. A `bridge uninstall` CLI was weighed and not built: it would be a 33rd subcommand on three platforms, with `uninstallSystemd`'s ignored `disable --now` failure to fix under the menu's flow.

*Names.* No function of the script is named after a word a shell keeps for itself: the refusal function was `stop` until 64237098, and ksh93 predefines `stop` as an alias of `kill -s STOP`, which wins over a function, so under ksh every refusal sent SIGSTOP to a process numbered by its exit code (`stop 1`, process 1) and the installer went on. The tests under ksh found it; `TestNoInstallerFunctionIsNamedAfterAShellWord` holds the names off POSIX's builtins and reserved words and ksh93's predefined aliases.

*Truncation.* The script is function definitions and one final compound command, `{ [ -n "${BRIDGE_INSTALL_TEST_SOURCE:-}" ] || main "$@"; }`: a prefix ending before its closing brace is a syntax error at the end of the file, and a prefix ending between definitions defines and runs nothing. A bare `main "$@"` would not do: its prefix `main` runs the installer with no arguments, an `--uninstall` become an install.

*Tests.* `scripts/install-test` runs the real script against an HTTPS server of fake releases (which can stall a download, sending nothing, its headers or half its body and then holding the connection until the client gives up, or closing it mid-body, and can trickle one), in a session with no controlling terminal, with a temporary HOME and a PATH of real tools and stubs (systemctl, with `cat` over the directories a user manager reads units from, "No files found" for a unit with no file as systemd answers, `show` of a loaded unit that `daemon-reload` snapshots, so a file changed after it is one the manager has not read, and the manager's environment; systemd-analyze, listing those directories; loginctl, sudo, su, uname, id, getent, getconf, date, sleep, ip, hostname), a fake bridge that refuses any flag the real one does not define and records its calls, each word apart, and four test hooks in the script (`BRIDGE_INSTALL_TEST_SYSROOT`, `BRIDGE_INSTALL_TEST_TTY`, `BRIDGE_INSTALL_TEST_SOURCE`, and since round 4 `BRIDGE_INSTALL_TEST_SPEED_TIME`, which shortens the speed window and is taken only as a whole number from 1 to 60) beside the download base `BRIDGE_INSTALL_BASE_URL` (https only). The commands the script prints are run, under the shell under test, against a recorder of each word put in place of every program they can name (`testdata/argv`), with paths holding a space, an apostrophe and a semicolon, and two runs in a row hold each state a run leaves to what the next one does. The same recorder, put in place of curl, reads the words `fetch` gives it, the shipped speed window's included (round 4). Contract tests hold it to the real CLI: the flags it passes and prints, a bridge command printed only through `bridge_line` and with `--config` where it reads the settings, the fake's flags, the output of `bridge version` and `bridge status` read by the script's own parsers, the line of `bridge update` it matches, and the unit `internal/packaging` renders. `make installer-check` runs shellcheck and the tests under every shell; CI's `installer` job runs it with dash, `bash --posix`, BusyBox's sh and ksh, and installs no wget.
## B603 — the root overlay's upper layer that no path reaches is taken on Volumio alone (2026-10-30)

- **On Volumio alone, the root overlay's upper layer that no path reaches is taken, on the box's own evidence, an explicitly trusted Volumio boot-layout assumption** (2026-10-30, backlog B603; `volumioUpper`, one inspection the writer and `bridge doctor` share, `atomicwrite.InspectMedium`). Volumio's initramfs mounts the boot disk's partition 3 at `/mnt/ext`, the root overlay's upper layer `/mnt/ext/dyn`, and nothing mounts it after the pivot, so statfs of the upper path answers ENOENT; the fallback takes it only at the root's own mount, on that first ENOENT, with every fact read in order and the refusal naming the first that fails: VOLUMIO_VERSION in `/etc/os-release`; the overlay read-write, mount and superblock, over `/mnt/static` with upperdir `/mnt/ext/dyn`, the overlay at `/`; `/imgpart`'s line, by its mount ID, partition 2 of a disk in sysfs, its `dev` that line's major:minor; the disk's partition 3, the directory `/sys/dev/block` resolves its own major:minor to, ext4 labelled `volumio_data` in udev's database; its ext4 superblock in `/sys/fs/ext4` and its major:minor on no mountinfo line. **Don't take an unreachable upper layer on any other box, at any depth but the root's own mount, or for any statfs failure but ENOENT; don't let it outrank a volatile overlay, a reachable memory upper, inconclusive totals or the bind refusal, and don't return nil where `taken()` decides; don't find the partition by its label (the owner's NVMe shares it), edit a device name as a string, or pick `/imgpart`'s line by its spelling; and don't take a partition 3 that no live ext4 superblock is, or that a mountinfo line lists.**

The defect. The owner's Volumio 4.231 on a Raspberry Pi 5 (bridge e242ce2c, the
0.2.2 candidate; data directory `/data/1-bit-bridge/config/data`;
`output.player.enabled: true`) logged `player: not started` with the reason
`the player's identity: atomicwrite: whether the root's storage keeps what is
written to it through a reboot cannot be told: /data/1-bit-bridge/config/data
is on an overlay whose upper layer /mnt/ext/dyn, as the mount table names it,
cannot be read here: statfs /mnt/ext/dyn: no such file or directory`. So on a
standard Volumio box the bridge's own player never started. Volumio's
initramfs mounts the boot disk's partition 3 (ext4, label `volumio_data`;
`mmcblk0p3`, 179:3 on that box) at `/mnt/ext`, mounts the root overlay with
`lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work`, mounts the
image partition (2) at `/imgpart`, and switches root; nothing mounts `/mnt/ext`
again, so no mountinfo line of the running system lists 179:3 and statfs of
`/mnt/ext/dyn` answers ENOENT. With the orchestrator's workaround (`mount
/dev/mmcblk0p3 /mnt/ext`, then a read-only bind remount) the existing
reachable-path logic took the upper layer and the player started; it is gone
at reboot. Measured on the box with the workaround unmounted: `findmnt
/mnt/ext` answered nothing, `grep ' 179:3 ' /proc/self/mountinfo` counted no
line, and `/sys/fs/ext4/mmcblk0p3` still existed, so the kernel keeps the ext4
entry of the initramfs's hidden mount. The box also carries an NVMe imaged with
Volumio, whose partition 3 (`nvme0n1p3`, 259:3, mounted at `/media/NVME`) is
labelled `volumio_data` too, and `/dev/disk/by-label/volumio_data` points at
it, not at the card's partition that holds the upper layer.

The decision (the orchestrator, after Codex's and Argon's review): automatic,
evidence-based recognition, and no operator override in this PR, since Volumio
must work with no configuration. The fallback (`volumioUpper`) runs only where
the first statfs of the upper path of the root's own mount, depth 0, answers
ENOENT (`upperAsReached` gives that refusal as an `*upperUnreached`), and it
takes the upper layer only when every fact holds, in this order, the reason
naming the first that does not appended to main's refusal, whose sentinel
(`ErrMediumUnknown`) and statfs error it keeps:

1. The box is Volumio: `/etc/os-release` names a VOLUMIO_VERSION that is not
   empty, its last assignment unquoted as the shell unquotes it
   (`volumioVersion`).
2. The overlay, the line of the root's own mount ID, is the root overlay as
   Volumio mounts it: its mount's options and its superblock's both begin `rw`,
   it names exactly one `lowerdir`, `/mnt/static`, and one `upperdir`,
   `/mnt/ext/dyn`, and it is the overlay at `/`: the mount `/` lies on, by its
   mount ID, is an overlay of the same superblock (its mountinfo device) at
   that superblock's own root.
3. `/imgpart` is partition 2 of a disk: a mount of its own (not the one `/`
   lies on), its line chosen by its mount ID, never by its spelling, so an
   overmount's line is the one read; the line's major:minor resolves through
   `/sys/dev/block` to a device whose `dev` file gives that major:minor again,
   whose `partition` file reads 2, and whose directory's parent is a disk (a
   `dev` file, no `partition` file).
4. The disk's partition 3 is the data partition: the one directory under the
   disk in sysfs, never a link, whose `partition` file reads 3 (two are
   refused); its `dev` file gives a major:minor that `/sys/dev/block` resolves
   back to that very directory, the common-disk identity; and udev's database
   entry for it, `/run/udev/data/b<major>:<minor>`, gives `ID_FS_TYPE=ext4` and
   `ID_FS_LABEL=volumio_data` (an entry that gives a key twice, or has an `E:`
   line that is no assignment, does not read).
5. Its ext4 superblock is live and hidden: `/sys/fs/ext4/<kernel name>` exists,
   and no line of the mount table lists the major:minor (a line that does not
   read as one leaves that unknown, and refuses). A `/mnt/ext` that fell back to
   tmpfs, as on a rescue or factory-reset boot, leaves no live ext4 superblock
   of the partition and is refused; a partition 3 mounted where this process
   sees it is refused too, and the reachable-path logic decides once it is
   mounted at `/mnt/ext`, where the upper path reaches it.

A device name is never edited as a string (`mmcblk0p2` to `p3`, `sda2` to
`sda3`), which `/dev/root` and by-uuid spellings break, and the partition is
never found by its label. Taken, the check returns `taken()`'s verdict, so a
bind of a directory inside the root overlay is still `ErrOverlayBind`, and it
names the storage (`Medium.Storage`, "Volumio's data partition (mmcblk0p3,
ext4)"). It outranks no other refusal: an overlay mounted `volatile` or
`fsync=volatile`, an upper path that reaches tmpfs or ramfs, totals that stay
inconclusive, an upper path statfs refuses for another reason than ENOENT, and
a root on tmpfs or ramfs are refused as before, without the box's files being
read.

The write path is unchanged. The merged root is still classified as an overlay
by its own statfs (`copyUpsFrom`), so every directory above the data directory,
up to the root of its mount (`/` on Volumio), is synced on the merged mount
once a write lands and in the probe, and no sync reaches for `/mnt/ext/dyn`.
The production reader (`hostBootFiles`) reads from `/`: each file opened with
`fsutil.OpenAsFile`, so a named pipe is refused at once rather than waited on
(the console's doctor runs the check on a settings render), and read to at
most 64 KiB; a directory listed with `os.ReadDir`, a link to a directory being
no partition; links resolved with `filepath.EvalSymlinks`, one that leads out
of the reader's root refused. One verdict serves the writer and bridge doctor
(amendment 8): the medium check is an inspection (`inspectMedium`), whose
`Err` the writer's admission reads (`platformMedium`, `mediumOf`) and which
`atomicwrite.InspectMedium`, resolving a root as the writer does, gives the
doctor whole.

Rejected: an operator override that vouches for the medium (not in this PR);
the label lookup, which names the NVMe; editing device names; the fallback
below the root's own mount or for a statfs error other than ENOENT; a nil
straight from the fallback, which skipped the bind refusal (amendment 4).

Residuals, the operator's: a modified boot that keeps every one of these facts
and puts a memory filesystem at `/mnt/ext` (the check reads the box's files,
not the hidden mount); a partition 3 mounted only in another mount namespace
beside a tmpfs `/mnt/ext`, which keeps `/sys/fs/ext4`'s entry live while no line
of this namespace lists it; and a Volumio whose initramfs changes any of these
paths, which is refused until the new facts are taught.

Pinned by `TestMediumTakesVolumiosHiddenDataPartition`,
`TestMediumRefusesWhatVolumiosLayoutDoesNotShow`,
`TestMediumKeepsEveryOtherRefusalBesideVolumiosEvidence`,
`TestMediumRefusesWhatTheBoxsFilesLeaveUncertain`,
`TestTheVolumioCheckReadsNothingButWhatItNames`,
`TestVolumioVersionReadsOSReleaseAsTheShellDoes`,
`TestUdevPropertiesAreTheEntrysAssignments`, `TestFakeBootIsTheKernelsShape`,
`TestTheHostsOwnFilesShowVolumiosLayout`, `TestTheHostsOwnFilesAreNeverWaitedOn`,
`TestWriteBytesDurable_SyncsThroughTheMergedMountWhereVolumiosUpperIsHidden`,
`TestWriteBytesDurable_RefusesWhereTheBoxDoesNotShowVolumio`,
`TestWriteBytesDurable_RefusesAnOverlayWhoseUpperLayerIsHidden` (Linux, a
namespace) and `TestInspectMediumIsTheWritersOwnVerdict`. The record is in
`ops/engineering-log.md`, 2026-10-30, B603.

## B606 — the inspection refuses a root that is no directory (2026-11-02)

- **`atomicwrite.InspectMedium` refuses, as the strict writer's `checkPath` does, a root that is there and is no directory, before it reads the root's mount: a regular file or any other entry but a directory, a link that leads to one, and a link at the root whose links lead to nothing, each a `*RootNotADirectoryError` matching `ErrRootNotADirectory`, never `fs.ErrNotExist`** (2026-11-02, backlog B606; bridge doctor's player-storage line fails them, cli-serve.md). **Don't inspect the mount of a root that is no directory, or answer a link to nothing at the root as a root not made yet.**

The doctor's player-storage line takes its verdict from `InspectMedium`, the
inspection whose verdict the strict writer's admission reads (B603, above),
and the inspection checked the type of no root: a root that is a regular
file, or a link to one, resolved, and the mount it lies on was inspected as a
directory's would be, so the doctor called a regular file there storage it
takes; and a root that is a link to nothing answered EvalSymlinks's ENOENT,
the answer for a root not made yet. The writer itself refused both, at
`checkPath` (a resolved root that is no directory is ENOTDIR, one that does
not resolve EvalSymlinks's error), so the inspection and the writer
disagreed about such a root. `InspectMedium` now asks the root's own entry
(`os.Lstat`) whether it is a link, and the resolved root (`os.Stat`) whether
it is a directory, before `mountIDOf`: a link whose resolution answers ENOENT
is a `*RootNotADirectoryError` with `Link` set and no `Target`; a resolved
root that is no directory is one with its `Target` and its `Kind`
(`fsutil.EntryKind`'s, with its article: "a regular file", "a named pipe",
…), `Link` saying whether the root's entry led there. Its `Is` matches
`ErrRootNotADirectory` alone and it wraps nothing, so a link to nothing never
reads as `fs.ErrNotExist`. A root below a link to nothing, whose own entry
cannot be reached, is still `fs.ErrNotExist`, which bridge doctor's walk up
the path judges (cli-serve.md, B603). Any other error of the resolution or
the stat is returned as it is (a permission error among them, which the
doctor warns of). `TestInspectMediumRefusesARootThatIsNotADirectory` pins it:
a regular file, a link to one, a link to nothing and a chain of links to
nothing are refused, each refused by the writer's probe too
(`ProbeDurable`); a link to a directory is inspected; and a root below a link
to nothing is still not there. The record is in `ops/engineering-log.md`,
2026-11-02, B606.

## A candidate's settings check, and the early rollback of an install whose version refuses the settings (backlog B613, 2026-11-02)

- **A candidate is checked against this bridge's settings before its swap, and an install whose version refuses them at its first start is rolled back before serve writes anything** (2026-11-02, backlog B613, five rounds): 0.2.2 refuses a fraction or a leading zero in a whole-number setting (B528, B533), 0.2.1's updater swapped without asking, and the new serve exited 2 at the load, before the boot rollback, under `Restart=always`. Install stages the verified candidate beside the binary and runs `<candidate> serve --validate-config --config <the settings the bridge loaded>` (`updater.CheckConfig`: 30 s, stdin empty, 64 KiB of each output): 0 swaps; 78 refuses, one `config refused: ` line a reason, naming the line and no path (`config.RefusedError.Reasons`), and the version is recorded as rejected with its reason and a digest of the settings (the file's bytes and every BRIDGE_ variable, on Windows whatever its name's case, `updater.ConfigDigest`), which holds it from the auto-installer while the digest is unchanged, a newer version and a manual install checking again; a 2 carrying the flag package's refusal of the flag is a candidate that predates the check, held from the auto-installer for every setting and installed by hand with a warning; for anything else (a timeout, a cancellation, a candidate that will not start, another code), nothing is swapped, installed or recorded. The check is serve's own load (`checkServeConfig`: the boot read, Load, `serveConfigRefusal`), which writes, opens and binds nothing, and never a root's reach (`CheckLibraryRootsAccessible` is the host's, and the user's who runs it). Serve, refusing its settings (a `config.RefusedError`, or `serveConfigRefusal`, every one of them before the data dir's lock), reads the data dir from the dataDir key, the default, BRIDGE_DATA_DIR and the settings' directory alone (`config.DataDirOf`), and puts the backup back only for a recent `installing` marker of this version over another, or its own record of one (`State.RollbackStarted`), swapped, under which no other version has opened the data dir, with the data dir's lock its own and the backup a regular file whose `version` names the marker's PreviousVersion (`updater.EarlyRollback`, `updater.ProbeVersion`, a version check and not a proof of the bytes); it records the hold durably before the rename, in the form the restored binary's boot keeps, Status "" beside the install it undoes (`updater.RollbackRecord`; 0.2.1's and 0.2.2's boot clears an install marker it cannot finish), so a start of this version that finds it with the backup not back finishes the rollback; a hold it cannot record leaves the install as it is, and a rename that fails puts the install marker back; then it exits 75 (`supervision.RestartExitCode`), which systemd, launchd and the SCM's recovery all restart; otherwise it refuses as before, saying why when the marker is this version's. Before its first write, serve records in an armed marker that it opened the data dir (`State.DataDirOpenedBy`, `updater.AdmitBoot`; a marker it cannot read is removed) and refuses to start when it cannot, and every store open with a migration step pending does the same before the step, whichever command opens the store (`updater.DisarmBeforeMigrating`, `manifest.OpenStore` refusing); BootInstallFailed leaves the backup where a version other than the backup's opened the data dir, and every clear at boot keeps a hold, and a veto of a rolled-back version's store migrations, recorded beside the install (`updater.ClearInstall`); and a directory barrier that fails is the marker's write's error (`SaveState`, `ClearStateDurably`), but where the filesystem cannot fsync a directory (the next bullet). **Don't swap a candidate whose check did not answer 0, take a failed or stopped check for a refusal or a 2 for "predates" without the flag package's words, record a refusal against no digest or a predates hold against one, change the flag, the codes or the prefix (every later release answers this updater), let the check write, open a store, bind or ask a root's reach, name a path in a reason, find the marker's data dir by anything but Load's rules for that key, restore without the lock, the marker's proof and the backup's version, or before the hold is recorded durably, restore after another version opened the data dir (but the crash rollback, which asks instead that no migration of the store began: the B660 bullet below), migrate the store before recording that this version opened it and that it began a migration (`State.StoreMigratedBy`, B660), drop a recorded hold or veto at boot, ignore the marker's directory barrier, write the settings, or exit 0 after a restore.** `TestAnInstallWhoseCandidateRefusesTheSettingsSwapsNothing`, `TestAnAutomaticInstallHoldsARefusedVersionBackUntilTheSettingsChange`, `TestAnAutomaticInstallHoldsBackACandidateThatPredatesTheCheck`, `TestACheckThatFailsSwapsNothingAndRecordsNothing`, `TestCheckConfigReadsTheCandidatesAnswerByItsExitCode`, `TestTheSettingsCheckRefusesWhatServeRefuses`, `TestTheUpdaterReadsWhatThisBridgesSettingsCheckSays`, `TestDataDirOfIsTheDataDirLoadGives`, `TestServeRollsBackAnInstallWhoseVersionRefusesItsSettings`, `TestServeLeavesAnInstallItCannotProveAsItIs`, `TestServeDisarmsTheEarlyRollbackBeforeItsFirstStoreAccess`, `TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir`, `TestEveryInstallTheBridgeStartsNamesItsSettings`, `TestTheHoldIsRecordedBeforeTheBackupIsPutBack`, `TestServeLeavesTheBackupWhereItCannotRecordTheHold`, `TestAStartFinishesARollbackStoppedBeforeItsRename`, `TestACommandThatMigratesTheStoreDisarmsTheEarlyRollback`, `TestAStoreOpenThatMigratesRecordsItInAnArmedInstallMarker`, `TestTheBootKeepsARecordedHoldWhereItClearsAnInstall`, `TestSaveStateReportsADirectoryBarrierThatFails` and `TestTheSettingsDigestReadsTheBridgeVariablesAsThePlatformNamesThem` pin it.

*The defect.* 0.2.2 refuses at load a whole-number setting written as a
fraction (`21600.5`) or with a leading zero (`060`), which only a hand edit
or another tool writes (`internal/config/wholenumbers.go`, B528 and B533).
0.2.1's updater verified the candidate's checksum (and on macOS its
signature) and swapped it in without ever running it against the settings.
The new serve exited 2 at `config load failed`, which is before
`maybeRollbackOnBoot`, the one place that can put the backup back, and
`Restart=always` started it again every two seconds. `DecideBootAction`
takes a matching version for the install's success, so even a boot that
got that far would have confirmed it. The same shape follows any future
tightening of the settings, so the fix names no setting.

*The check's interface.* A flag of serve, not a subcommand: `bridge serve
--validate-config [--config PATH] [--addr ADDR]` runs `checkServeConfig`,
which resolves the config as runServe does (`resolveConfigPath`,
`fsutil.AbsAsWritten`), reads it through `loadServeConfigFile` (the boot
read, `config.LoadFileAt`), asks `serveConfigRefusal`, and with `--addr` the
override's own check, then exits. Its codes and its line prefix live in
internal/updater (`ConfigCheckFlag`, `ConfigCheckValidExit` 0,
`ConfigCheckRefusedExit` 78, sysexits.h's EX_CONFIG, `ConfigCheckFailedExit`
1, `ConfigRefusedPrefix`), where both sides read them, since this updater
runs every later release as a candidate. 1 is a config it could not read
(none there, unreadable, a name being repointed): a failed check, never a
refusal. `--init-if-missing` beside it is a usage error (2): the check
writes nothing. A release that predates the flag answers the flag
package's own words ("flag provided but not defined: -validate-config")
and 2, having run nothing. A refusal's lines come from
`config.RefusedError`, which `loadData` returns for a decode it refuses and
for Normalize and Validate: its Error is the text Load always gave (it
unwraps to it, so errors.Is and errors.As see what they saw), and its
Reasons are the same without the file's name, a yaml.v3 type error's lines
(`line 7: …`) or the refusal's own; `serveConfigRefusal`'s two-roots
refusal names the roots for serve's line and none for the check's.

*Where the candidate runs.* Install stages the verified binary beside the
one it would replace (`stageForCheck`, `.bridge-check-*`, `.exe` on
Windows, the destination's mode) and checks it there, then hands that file
to the swap, which moves it into place within the directory: a data dir
mounted noexec, or a policy that runs nothing from it (AppLocker), would
refuse the copy in the scratch dir, never the directory the bridge already
runs from. The run (`runCandidate`) has its own bound (30 s for the check,
10 s for a version), standard input empty, each output kept to 64 KiB and
the rest read and dropped, `Cmd.WaitDelay` 2 s, and starts again on ETXTBSY
(a file written moments ago can still be open in a child another goroutine
forked, golang/go#22315). The Tailscale CLI's rule against WaitDelay (under
The CLI and the serve wiring) is about a join that must outlast a writer;
a check and a version write nothing, so the bound is what keeps a broken
candidate from parking an install.

*What Install does with the answer* (`checkCandidate`). Valid: the install
goes on. Refused: nothing is installed, by hand or automatically, since
the candidate would not start with these settings; one Warn line names the
reasons, and the refusal is recorded (`recordRejection`, which keeps the
rest of the marker, so a refusal before an install leaves the last
install's "installed" stamp and its backup's cleanup in place) with its
reason and `ConfigDigest`: the file's bytes and every BRIDGE_ variable with
a value, since config's overrides read them over the file. The error is a
sentence naming the version and each reason by line, matching
`ErrConfigRefused`; the console answers it 409 `config-refused` with that
sentence alone (`mapUpdaterError`'s `detailError`), and `bridge update`
prints it and the file to fix. The auto-installer's gate (`HeldBack`) holds
the version and any older one while the digest is unchanged, so fixing the
settings lets it try again (Argon's design), a newer version is admitted,
and a manual install checks again whatever the record says; settings it
cannot read hold the version back, as an automatic install must not
guess. The console's Held update row shows a recorded hold through
`deferredReason` whether or not the auto-installer has run in this
process, and an operator's rollback keeps the auto-installer's words. A
check that did not answer installs and records nothing, so the next
attempt asks again; one its context stopped carries the cancellation,
which the auto-installer does not log as a failure.

*A candidate that predates the check.* Codex's choice, the brief's binding
decision: the auto-installer holds it back, recorded for every setting (no
digest), so it is not downloaded again each poll, and a manual install
takes it with a warning (`Status.InstallWarning`, which the console alerts
before its restart and `bridge update` prints), since the operator can
always choose a version by hand. Argon's alternative, to install it
automatically with a warning, was not taken: a version that predates the
check predates the early rollback too, so an automatic install of one that
refuses the settings would crash-loop with nothing to bring it back. Only
the flag package's words with an exit of 2 read as "predates": a Go panic
exits 2 as well, and is a failed check.

*The early rollback, for installs an updater without the check made.* Serve
asks `recoverRefusedInstall` when its load is a `config.RefusedError` or
`serveConfigRefusal` refuses; a file it cannot read, no config and a name
being repointed are not asked about. It finds the data dir without the
parse that failed (`config.DataDirOf`): the dataDir key as yaml.v3 decodes
it into the field, aliases, merge keys and tags included, from the file's
first document, `./data` for an empty or absent key, BRIDGE_DATA_DIR when
set and not empty, a relative value joined to the directory filepath.Abs
gives the config's name (never the directory a link there leads to); a
dataDir written twice, as a list or a map, or a document yaml.v3 cannot
decode is an error, and nothing is guessed (Codex: no regex, no search). It
reads the marker there and acts only on a recent install of this version
over another, which reached its swap, dated no more than a minute ahead of
the clock, under which no version but the backup's has opened the data dir
(`updater.EarlyRollback`); takes the data dir's lock (`lockServeDataDir`;
held by another open, or impossible on a filesystem that keeps no locks,
and it does nothing); reads the marker again under the lock; requires the
backup, `<binary>.bak` beside the resolved executable, to be a regular file
whose `version` (`updater.ProbeVersion`: bounded, stdin empty, its first
line parsed) names the marker's PreviousVersion. The marker records no
hash and no path of the backup (Codex, against v0.2.1), so this is a
version check and not a proof. Then it records the hold, durably, before
anything moves (round 2, below): the marker becomes `updater.RollbackRecord`,
Status "" so the restored binary's boot does nothing with it, RejectedVersion,
the reason with each refused line and the digest, beside the install it
undoes (TargetVersion, PreviousVersion, AttemptedAt, SwapStarted) and
`RollbackStarted`. A hold that cannot be recorded leaves everything as it
is: the backup put back without it would be free to install this version
again. Then `updater.RollbackBinary` renames the backup over the binary: one
rename on Unix; on Windows the running image is moved aside first and put
back if the second rename fails (`replaceRunningBinary`,
`TestRollbackReplaceRestoresDstWhenTheSecondRenameFails`), the SCM stop
skipped since this process is the service. A rename that fails puts the
install marker back as it was, so a start that gets past the settings
confirms the install as it would have (since B660, takes the install up as
it would have, on probation until a start of it has answered and stayed up:
B660's section). It says so in a line that names
both versions, and exits 75: systemd's `Restart=always` restarts any exit,
launchd's KeepAlive restarts an unsuccessful one, and the SCM's recovery
actions answer a service-specific exit code (B201's rule); one restart,
well inside systemd's default start limit. Every other case refuses to
start with the settings' error, as before, with one `updater: not rolling
back the install of …` line where the marker is an install of this
version (or the rollback's own record of one), and none otherwise. The
settings are never written. 0.2.1, once restored, reads only
RejectedVersion of the record, by its own rule: it holds the version back
until a newer one, with its own "rolled back" words, and a `bridge update`
by hand installs it.

*No downgrade past a migration, across starts* (Codex's binding). The early
rollback runs before the data dir's lock, and every write serve makes comes
after it, so no migration of this start can have run. An earlier start
could have migrated the store and crashed before `maybeRollbackOnBoot`, so
serve records in an armed marker, right after the lock and before its
first write (`disarmEarlyRollback`, before `backup.AdoptPlayerState`), that
a version other than the backup's opened the data dir (`DataDirOpenedBy`,
`ArmsDataDirRecord`), durably (`SaveState`), and refuses to start (1) when
it cannot. A marker it cannot read is removed, durably
(`ClearStateDurably`), since it cannot be told an armed install's or not
and a later start that could read it again must not take it for an install
no start of this version went past. The boot rollback (BootInstallFailed),
which restores after the store has opened, leaves the backup where the
record names a version, and clears the marker (since B705 the install
alone: the record stays beside the backup, B705's section). Since round 2
the record is `updater.DisarmEarlyRollback` (since round 3 two functions
over one disarm:
`updater.AdmitBoot` for serve, `updater.DisarmBeforeMigrating` for the
store's open), and the store's open asks it too, under the
ladder lock, before its first migration step and only while one is pending
(`disarmBeforeMigrating` in internal/manifest), refusing the open when it
cannot record: a command of the new version that opens the store before
any serve of it starts (`bridge doctor` after `bridge update`,
`clear-missing`, a scan, a backup) migrates it, and its settings can load
where serve's later do not (an edit, or the unit's environment beside a
shell's), so a rollback after it would have been a downgrade past the
migration (Codex's finding 1). An open that migrates nothing reads no
marker.

*The Tailscale mode.* Validate refuses a mode it does not know at load
already (Gemini on PR #249), so `serveConfigRefusal`'s check of it, which
ran after the stores had opened until B613 moved it before the lock, is a
second line that refuses nothing today; the first negative control showed
it (`TestTheSettingsCheckLoadsTheSettingsAsServeDoesAndWritesNothing`'s
Tailscale case went red with the load refusal), and the table's
two-library-roots case is the one that goes through `serveConfigRefusal`.

*Round 2* (the reviews of PR #1382, Codex's and Argon's). The hold is
recorded before the rename (Codex 5, Argon 5): round 1 renamed first and
recorded after, and the backup it restores is 0.2.1's or 0.2.2's as often
as not, whose boot reads the install marker it finds (its binary is not the
version the marker names, the backup is gone) as an install that failed:
"rollback failed … manual recovery needed", then the marker cleared, hold
and all. A crash between the rename and the record, or a record that could
not be written, so left that binary free to install the refused version
again, unchecked. The record goes first, in a form that boot keeps (Status
"": `DecideBootAction`'s BootNoop, on every version, and json drops none of
it on a read), and since a crash can now fall between the record and the
rename, the record keeps the install it undoes and `EarlyRollback` takes it
for the install still (`InstallArmed`), so the next start of the refused
version finishes the rollback; `ArmsDataDirRecord` arms on it alike, so a
start that gets past the settings meanwhile records that it opened the data
dir. Every store open that migrates disarms (Codex 1, above). Every clear at
boot keeps a recorded hold (`updater.ClearInstall`; Codex 7): the backup's
cleanup after a confirmed install dropped a refusal `recordRejection` had
recorded beside it, and so did the clears of an abandoned, an unswapped and
a failed install, the same shape (the rule's "grep for the pattern"). The
marker's directory barrier (`syncMarkerDir`, `fsutil.SyncParentDir`) failing
is its write's error, as the file's own fsync failing always was (Codex 4).
Round 2 said that a data dir on a filesystem that fails every directory
fsync (some FUSE and network mounts) then refused to arm an install, and an
install an older updater made there refused to start until the marker was
removed by hand; round 3 corrects it (below): a fsync that answers that the
filesystem cannot do one (EINVAL, ENOTSUP, EOPNOTSUPP) counts as done, and
Linux's NFS, CIFS/SMB and FUSE clients never answer one, so no install is
refused for it. And the digest reads a Windows variable's name in any case, upper-cased
(`bridgeEnvironment`; Codex 9): config's overrides read it so there. Left,
for the follow-up list: the console's and the CLI's manual Rollback do not
ask `DataDirOpenedBy` (Codex 2, operator-initiated, pre-existing; there is no CLI rollback,
and since B692 the console's asks it and refuses); the early
rollback's lock does not exclude a concurrent `bridge update` (Codex 3, the
updater keeps no cross-process lock); the digest is taken after the check
rather than before (Codex 6); a restored 0.2.1 or 0.2.2 ignores the digest
(Codex 8, the owner item); the check's copy and the swap's own staging are
two copies of the candidate beside the binary (Argon), `--addr` is not
passed to the check (Argon, no shipped unit passes one), and the backup's
version is read by running it, where `debug/buildinfo` could read it
without (Argon; it would need release-built test binaries, not taken here).

*Not here.* An install counted healthy only after readiness and a
probation, rather than at a matching version (B198's scenario, filed
separately, and done in B660 on 2026-11-06, its section below): B613 left
`DecideBootAction`'s success rule as it was, and B660 changes it
(`BootInstallProbation`) without touching the early rollback, which asks
about an install that could not start. Native Windows (the SCM, a mapped image, a
`.bak` run by CreateProcess under its own name) is cross-vetted only. A
crash during a check leaves a `.bridge-check-*` beside the binary, as one
during a swap leaves a `.bridge-swap-*`. The record is in
`ops/engineering-log.md`, 2026-11-02, B613 (and its rounds 2 and 3).

## A marker's write trusted once its barrier answered, one lock for the rollback and every migration, and the interrupted rollback reconciled (backlog B613's round 3, 2026-11-02)

- **…and a marker's write is trusted only once its barrier has answered, one lock (`<dataDir>/update-state.lock`, `updater.LockState`) keeps the early rollback and every store open that migrates apart, a directory fsync the filesystem cannot do (EINVAL, ENOTSUP, EOPNOTSUPP) is taken as done, said once, and the boot reconciles an interrupted rollback** (2026-11-02, backlog B613's round 3). **Don't trust a marker a failed barrier left without the barrier again, take any other fsync answer, or an open or a close failing, for a filesystem that cannot fsync a directory, publish the marker through a rename that runs a barrier of its own, lock `update-state.json` or remove the lock file, take the lock before server.lock or the ladder or either while holding it, write the marker in a guarded step without it, migrate under a rollback of the running version under way, stamp an interrupted rollback installed before the store opens, read it as an install marker or a hold alone, restore from it at boot, keep a later spelling of a Windows variable, or open a store anywhere but `manifest.DefaultDBPath(dataDir)`.** `TestADisarmRunsTheBarrierAgainOverARecordItCouldNotMakeDurable`, `TestADisarmRunsTheBarrierAgainOverARemovalItCouldNotMakeDurable`, `TestServeRefusesUntilTheMarkersBarrierAnswers`, `TestTheMarkersBarrierTakesOnlyADirectorySyncTheFilesystemCannotDo`, `TestDarwinsFullSyncFallsBackToAnOrdinaryFsync`, `TestTheMarkersWriteRunsOnlyItsOwnBarrier`, `TestADisarmWithoutTheLockWritesNothing`, `TestAMigrationThatHoldsTheMarkersLockFirstStopsTheRollback`, `TestARollbackThatHoldsTheMarkersLockFirstStopsTheMigration`, `TestAMigrationRefusesWhileARollbackOfItsVersionIsUnderWay`, `TestATargetWhoseSettingsAreFixedConfirmsAnInterruptedRollback`, `TestARefusedStartFinishesAnInterruptedRollbackOnlyOnItsProof`, `TestAPatchedPreviousVersionKeepsTheHoldOfARollbackThatFinished`, `TestTheSettingsDigestKeepsTheVariableTheOSReads` and `TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase` pin it.

*A failed barrier, trusted later* (Codex's and Argon's P1 on round 2). The
marker's write renames its new file into place and then fsyncs the data
dir; a barrier that fails after the rename leaves the new marker live and
not durable, and the write's caller is told it failed. Round 2 refused the
start there, and the next start read the record (`DataDirOpenedBy`) as
recorded and went on unchecked, so the store's migrations ran with no
durable record, and a power loss could bring back the armed marker and a
rollback past them; a removal whose barrier failed (a marker that could not
be read) had the same shape, the next start finding no marker. Every disarm
that finds nothing to record (a version recorded already, no marker, no
install armed, the backup's own version) now runs the barrier before it
trusts what it read, and refuses while it fails (`disarmHeld`,
`disarmUnlocked`); a second `ClearStateDurably` runs it too. The tombstone
before a removal was the alternative; the barrier on every short cut was
taken, since it covers both shapes and every later start with one rule, at
the cost of one directory fsync a start, and a data dir this user can write
and not read now refuses a start (the fsync's open fails), which the owner
should know. The marker's publication is its own
(`atomicwrite.RenameWithRetryUnsynced`, then `syncMarkerDir`): through
`RenameWithRetry`, atomicwrite's tolerant barrier ran first, logged the
data dir's path and the error once a process, and was a second barrier
beside the marker's, so a fault in the marker's own was not the only one
(`TestTheMarkersWriteRunsOnlyItsOwnBarrier`, a data dir mode 0300, read the
path in the log on round 2's code). The barrier is three seams, the open,
the fsync and the close, so a test fails each after the rename or the
unlink, and `SetMarkerBarrierForTest` lets cmd/bridge's boots do the same
on every platform. `fsutil.SyncParentDir` is unchanged: its other callers
want it strict.

*What a directory's fsync answers.* A fsync that answers EINVAL, ENOTSUP or
EOPNOTSUPP is a filesystem that cannot fsync a directory, and counts as
done, said once a process naming the errno and nothing else
(`cannotSyncADirectory`, `sayCannotSyncADirectory`), at the publication's,
the removal's and the disarm's barrier alike; EBADF, EIO, ENOSPC, EDQUOT,
EACCES, EPERM, EROFS, anything unlisted, and any open or close failure, are
the barrier's failure. On darwin, `(*os.File).Sync` asks for F_FULLFSYNC and
falls back to fsync on ENOTSUP alone (Go's #64215); a full sync a
filesystem does not support can answer EINVAL or ENOTTY too, so an ordinary
fsync is asked then, and its answer is decided (`syncWithFallback`,
`plainFsync`). Windows keeps its compile-time skip (`markerBarrier` nil).
Which filesystems this is for, from what the kernels do: Linux's NFS client
treats a directory's fsync as a successful no-op (`nfs_fsync_dir`: every
directory operation is synchronous there), so does its CIFS/SMB client, and
its FUSE turns a filesystem's unimplemented FSYNCDIR into success
(`fc->no_fsyncdir`); a filesystem with no fsync operation answers EINVAL
(`vfs_fsync_range`). So the degrade is for the rest: a FUSE daemon or a
network filesystem on another kernel that answers the fsync itself, and a
filesystem with no fsync for directories. There the marker's writes are
atomic and not durable: a power cut soon after a write can bring back the
marker as it was before it, and with it a rollback past what the start
after it wrote. No install is refused for that, and no data dir needs
`update-state.json` removed by hand, which round 2's refusal asked.

*One lock for the rollback and every migration.* The early rollback held
the data dir's lock (server.lock), and a migration the ladder (beside the
database), so a `bridge scan` or `bridge doctor` could record its migration
after the rollback's last read of the marker and migrate, and the rollback
then wrote its record from its stale read (`RollbackRecord(st, …)`) over it
and put the old binary back over a migrated store. `<dataDir>/update-state.lock`
(`LockState`: `fsutil.OpenLockFile`, `TryLock`, a 30 s wait asking every
50 ms) is the one lock: never `update-state.json` itself, which every write
renames over, and never removed. The order is server.lock, the ladder, then
it, each caller taking only what it needs, and nothing that holds it takes
either of the others: the early rollback takes server.lock then it, and
holds it from its marker read through the backup's rename and any write
that puts the install back; a migrating store open takes the ladder then
it, through its record and barrier (`DisarmBeforeMigrating`); serve's first
disarm takes server.lock then it, until its record is durable, and lets it
go before the store opens (`AdmitBoot`); since round 4 the
boot's housekeeping takes server.lock then it, once the store's open
has let the ladder go (`bootHousekeeping`), and since B692 the
updater's install, operator's rollback, hold and restamp take it
alone in `bridge update`'s process or under serve's server.lock
(`lockMarkerForUpdate`; B692's section, after round 5's). The helpers under it take no lock
(`disarmHeld`). Where it cannot be taken a guarded step never writes as if
it held it: a lock held past the wait, every try answering ENOLCK (which
does not show the filesystem keeps none), or any other failure refuses the
rollback, and refuses a disarm that would record or remove, while one with
nothing to write goes on as before (an ordinary start, no install armed).
A filesystem whose lock says it keeps none (EOPNOTSUPP, ENOTSUP,
`errors.ErrUnsupported`, Windows' two answers) is a hold that is not held:
the rollback refuses there (it refuses on server.lock first anyway), so no
rollback can run beside a migration, and the disarms go on and record, said
once. That last is this round's own call, flagged for the owner: refusing
there would stop every start after an update on such a data dir, which
round 3's degrade removes for the barrier. A migrating open of the version
an early rollback was undoing (`RollbackUnderWay`: the record, Status "",
`RollbackStarted`, this version its target, no version recorded over it)
refuses with `ErrRollbackUnderWay` (round 4: `MigrationVetoed` and `ErrMigrationVetoed`, the veto a field of
its own that outlives the record): the backup may be back over its binary
already, and a command still running from its old image must not migrate
the store past the version put back. Both acquisition orders are pinned:
the migration first, and the rollback waits, reads the record, and leaves
the install; the rollback first, and the migration waits, then refuses.

*The interrupted rollback, reconciled.* A crash after the rollback's record
and before the rename, then settings fixed: the target ran, and
`DecideBootAction` read Status "" as nothing to do for good, so the install
was never confirmed, and with the migration's refusal its own boot would
have refused its store. Serve's first disarm is the boot's admission: under
the lock, for a start whose settings loaded, it records that this version
opened the data dir over the record, which `ArmsDataDirRecord` arms on, and
that record resolves the rollback (`EarlyRollback` vetoes after it), so the
store's open migrates. The confirmation is left to the housekeeping after
the store opens (stamped at the admission, the same boot's housekeeping
would remove the backup). `DecideBootAction` reads the record as its own
case, before any window: the target booting is `BootRollbackOvertaken`,
confirmed however old the record (`ConfirmOverRollback`: installed now,
`RollbackStarted` cleared, the hold cleared only where it holds this
version back, the install and the data dir's record kept, so the next boot
removes `.bak`; since B660 `ProbationOverRollback`, which puts the install
back on probation instead, its count of failed starts kept, and `.bak` is
removed only at the boot after its confirmation); the backup's version booting is `BootRollbackFinished`
(the rollback done: the install cleared, the hold kept, nothing restored,
never `BootInstallFailed`); any other version, and a hold with no
rollback, is `BootNoop`. A start of the target still refused finishes the
rollback only on `EarlyRollback`'s proof (recent, swapped, the backup's
version, no version recorded over it). An expired interrupted rollback is
confirmed (since B660, put back on probation) when the target starts on settings it loads (as an `installing`
marker past the window is since B705, which until then cleared one as
abandoned), clears
its install when the backup's version starts, and is left as it is, with
`not rolling back … older than 6h0m0s`, when the target's settings are
still refused. Not reconcilable at all: a restored 0.2.1 or 0.2.2, whose
housekeeping reads Status "" as nothing to do and whose gate reads
`rejectedVersion` alone, so fixing the settings does not release its hold
until a newer release (`bridge update` by hand installs it).

*The digest's variable, and the store's data dir.* Two entries of one name
in two cases can stand in a Windows environment block, and config's
overrides read a variable through `os.LookupEnv`, which asks
`GetEnvironmentVariable` for the first entry its name matches in block
order (`os.Environ`'s order): the digest keeps each name's first entry,
folded, before it drops an empty value, so an empty winner is no setting
and its later twin is not taken (`bridgeEnvironment`). `exec.Cmd`'s
last-wins dedup builds a child's environment and is not how this process
reads its own. And `disarmBeforeMigrating` takes the data dir as the
database's directory, since `OpenStore` is never given it: the boundary is
documented on both, and `TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase`
follows every production open of the store, through locals and parameters,
to `manifest.DefaultDBPath`, and the one it cannot follow, the restore's
`ResetBackupEpoch`, to every `backup.Targets` naming the database by it
(doctor joined `bridge.db` by hand, and now asks `DefaultDBPath`).

## The migration veto past the rollback's record, the boot's housekeeping under the marker's lock, a stop in that lock's wait, and the confirmation on the install's own record (backlog B613's round 4, 2026-11-03)

- **…and the veto of a rolled-back version's store migrations (`State.MigrationVeto`) outlives the rollback's record, the boot's housekeeping holds the install marker's lock, that lock's wait ends with serve's stop, and an interrupted rollback is confirmed only on the install's own record** (2026-11-03, backlog B613's round 4). **Don't drop the veto at a clear or a refusal's record, lift it anywhere but at the vetoed version's own serve admission (never without the lock, but where the filesystem keeps none) or by a new install's fresh marker, let a store open of the vetoed version migrate with the lock or without, put a backup back before its veto is recorded, read, write or swap at boot without holding `update-state.lock` (a filesystem that keeps none, or a holder past the wait, leaves the marker and the binaries as they are), take a stop in that lock's wait for a lock that could not be taken, or confirm an interrupted rollback on less than the record its admission made.** `TestTheMigrationVetoOutlivesTheRollbacksHousekeeping`, `TestOnlyTheVetoedVersionsOwnStartLiftsTheVeto`, `TestANewInstallsMarkerCarriesNoVeto`, `TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots`, `TestAMigrationThatHoldsTheMarkersLockFirstStopsTheBootRollback`, `TestABootRollbackThatHoldsTheMarkersLockFirstStopsTheMigration`, `TestTheBootLeavesTheInstallWhereItCannotHoldTheMarkersLock`, `TestTheMarkersLockWaitEndsWithItsCallersContext`, `TestADisarmStoppedInItsWaitReturnsTheStop`, `TestAStopWhileServeWaitsForTheMarkersLockEndsTheStartThere`, `TestTheConfirmationNeedsTheInstallsOwnRecordOfTheDataDir` and `TestTheBootConfirmsAnInterruptedRollbackOnlyOnTheInstallsOwnRecord` pin it. → History-rules-config-lifecycle.md

*The veto that went with the record* (round 3's review, P1).
Round 3's migrating open refused where the marker was the early rollback's
record of its own version (`RollbackUnderWay`: Status "", `RollbackStarted`,
the target this version, no version recorded over it). That record is the
install the rollback undoes, and the previous version's first start, a
release with round 3, clears that install at its housekeeping
(`BootRollbackFinished`, `ClearInstall`) and keeps only the hold, so from
then on nothing refused the target's migrations: a command of the target
still running from its image (a `bridge doctor` or a scan started before the
rollback, held anywhere before its migration, as one waits for the ladder
behind another opener's migration) migrated the store past the version put
back. The veto is a field of its own now, `MigrationVeto`, naming the
version whose binary the rollback replaces: the early rollback's record
carries it (`RollbackRecord`), written durably before the rename as the hold
is, and every clear keeps it beside the hold (`ClearInstall`), as a refusal
recorded since does (`recordRejection` keeps the rest of the marker). A store
open of that version with a step pending refuses with `ErrMigrationVetoed`,
holding the marker's lock or not (`disarmHeld`, `disarmUnlocked`: Codex's
note), and the version put back, or any other, migrates; the previous
version's admission writes nothing over the veto. The hold alone could not
serve: it is the auto-installer's, follows the settings' digest, is cleared
by a confirmation and kept for an operator's rollback, and names a version
for another reason (a candidate's refused check) where no rollback ran. Only
the vetoed version's own serve admission lifts the veto (`AdmitBoot`, in one
write with its record where it makes one): a serve of that version whose
settings loaded is the start the rollback waited for, where a command's
settings say nothing of serve's. The lift is a write, so a lock that cannot
be taken refuses it, but on a filesystem that keeps none, as for a record. A
new install's marker is a fresh one (`Install` writes the whole struct), so
an explicit reinstall carries no veto; `TestANewInstallsMarkerCarriesNoVeto`
runs the real `Install` over a finished rollback, off Windows as every
install test. Keyed on the target version, not on
anything newer than the version put back (Argon's alternative): the
processes that can still be running from a replaced image are that
version's, a version newer than both arrives only by an install, whose fresh
marker is a decision of its own, and an exact name needs no ordering of
development and pre-release versions. Pinned end to end by the later
arrival: a command held before the ladder, the early rollback, the previous
version's start (its admission, a migration of its own, its housekeeping),
then the command's migration, refused, the store left at v59 with v60's
columns and index absent; then this version's admission lifts the veto and
the store migrates.

*The boot rollback vetoes too.* `BootInstallFailed` puts `.bak` back over the
running binary when the install did not come up as its version. Since round
2's admission records every other version that starts, it restores only
where the running version is the backup's (the swap never landed) or where
an install landed beside a running boot (a `bridge update` then), and in
that case the binary replaced is the install's: the housekeeping records the
veto of `TargetVersion` durably before the rename, and a veto it cannot
record leaves the install as it is. It names the marker's target, never the
version the replaced binary reports; a binary whose version is not its
release's tag is the residual.

*The housekeeping under the lock* (Argon's High). `maybeRollbackOnBoot` read
the marker, decided, wrote and renamed with no lock, so a migrating open
could record its migration between that read and the rename, and the boot
rollback put the backup back over a migrated store. `bootHousekeeping` takes
`update-state.lock` before its read, inside the data dir's lock and after
the store's open has let the ladder go, as the documented order has it
(server.lock, the ladder, then this one), holds it through the decision, its
writes and the backup's rename or removal, and lets it go when it returns;
nothing it calls takes another lock. A lock it cannot hold, for any reason
(another holder past the 30 s wait, ENOLCK, a failure, serve's stop, and a
filesystem that keeps none, where `LockState`'s hold is not held: Argon's and
Codex's notes), leaves the marker and both binaries as they are, said on one
line, and returns the refusal; a later start asks again. A marker that asks
nothing of the boot (`BootNoop` on a read before the lock) takes no lock, so
a filesystem that keeps none, or a lock another process holds, costs an
ordinary start no wait and no line; what the boot acts on is read again
under the lock and decided there. The cost of refusing where the filesystem
keeps no locks: there an install is not stamped installed and its `.bak`
stays until the data dir is on a filesystem that keeps them, which the
runbook says, and LockState's once-a-process line says too. Both
acquisition orders are pinned against a real migrating open: the migration
first, and the housekeeping waits, reads the record and puts nothing back;
the housekeeping first, and the migration waits, then refuses under the
veto, so the store is never migrated under the binary put back.

*A stop in that lock's wait* (Medium). `LockState` waited its 30 s whatever its
caller's state, and a disarm whose wait ran out took the unlocked path,
which goes on where it has nothing to write, so serve stopped in its
admission's wait went on to boot. The wait now ends with its caller's
context, returning the context's error and never a `*StateLockError`, and
keeps its bound (`sleepUnlessDone`); a context that has ended does not stop a
take that needs no wait. The callers that pass one are serve's: its
admission (`AdmitBoot` returns the stop as it is, never the unlocked
disarm's answer, and `runServe` exits 0 with nothing written), its early
rollback (the install left as it is, saying the start was stopped, and
serve's exit 2 as for any refused settings) and its housekeeping (the marker
left). A store's open has no context, so `DisarmBeforeMigrating` keeps the
bound alone.

*The confirmation on the install's own record* (Medium). `ConfirmOverRollback` confirmed whatever the record held. The target's
housekeeping reaches it only after its admission recorded that the target
opened the data dir, so a record without that line is one no admission of
the target wrote, and no proof the rollback is over. It confirms now only
where `DataDirOpenedBy` names the install's version and the version
confirming is that one, and otherwise returns an error with the marker as it
was, which the housekeeping leaves as it is, naming why. (B660 renamed it
`ProbationOverRollback`: on the same condition it puts the install back on
probation rather than confirming it, and the line says `not putting the
install of … back on probation`.)

*A store genuinely older* (P3). The restart fixture set `user_version`
back on a store this build had made, so v60's columns and index were there
before the restart and its migration ran over a schema that had them, which
no store a release left looks like. `manifest.WriteDatabaseAtVersionForTest`
writes a store as a ladder ending one step short leaves it (the ladder
test's own helper, moved to a file other packages' tests can reach), and the
restart test asserts v60's two columns and its index absent before and
present after.

*Not changed: `Updater.Install` and `Updater.Rollback`* (Codex's audit; B692 changed both, its section after round 5's). Both
write the marker without `update-state.lock`, in serve and in `bridge
update`'s own process, and swap with it unheld; they are serialised against
each other in-process (`installInFlight`) and against nothing else. Install
writes a fresh marker, which lifts a veto by design: a vetoed command
waiting at that moment then finds the install armed and records its
migration as an admission would, and in every flow the updater offers the
version it runs is the install's or older. Rollback is the operator's own,
a downgrade by choice after the new version ran. The residual is a marker
write an operator's install or rollback makes at the moment another process
records or decides. Two more stand beside it: a marker that cannot be read
is still removed by the disarm that finds it, a migration's included, and a
veto in it with it, as a malformed one reads as none (`LoadState`); and a
serve of the vetoed version started from its image before the rollback's
rename, whose settings load by the time it takes the data dir's lock, is
taken for that version's admission.

## An install marker's errors without their paths (backlog B613's round 5, 2026-11-03)

- **An install marker's read and removal errors name no path in a refusal, log line or stderr; the refusal keeps its causes through `Unwrap`, and `DisarmResult.Unreadable` keeps the original read error for code** (2026-11-03, B613 round 5). **Render `Unreadable` through `fsutil.ErrorWithoutPath` only where it is printed or logged; never replace it with a string error.** `TestDisarmRefusalsKeepCausesWithoutPaths`, `TestDisarmUnreadableKeepsTheOriginalPathError`, `TestMigrationMarkerDiagnosticsCarryNoPaths` and `TestServeMarkerDiagnosticsCarryNoPaths` pin it.

*The defect* (CodeRabbit on #1382, CWE-532). `LoadState` wraps the
`*fs.PathError` from reading `update-state.json`, and the marker's removal
wraps the unlink's, so the data dir's absolute path reached the store open's
warning (`internal/manifest/migration_disarm.go`), serve's stderr lines
(`cmd/bridge/serve_refused_install.go`, and the boot's housekeeping in
`cmd/bridge/main.go`) and the refusals `disarmUnlocked` and `disarmHeld`
return, which serve and every migrating store open print. The bridge logs
value-free.

*The fix.* `markerIOError` (`internal/updater/disarm.go`) renders a marker's
file error through `fsutil.ErrorWithoutPath` and keeps it for `errors.Is`
and `errors.As` through `Unwrap`; both refusals wrap their causes through
it, `disarmHeld`'s with two `%w`. `DisarmResult.Unreadable` keeps the read's
own error for code, and the lines that print or log it render it through
`fsutil.ErrorWithoutPath`. `StateLockError` already rendered its cause
without a path.

*Tests.* `TestDisarmRefusalsKeepCausesWithoutPaths` covers a read and a
removal failing with distinct causes (on Unix the fixture's read answers
EISDIR and its removal ENOTEMPTY, since `os.Remove` returns rmdir's error
unless it is ENOTDIR), a removal whose barrier fails, and the three lock
refusals: no path in the text, every cause kept.
`TestDisarmUnreadableKeepsTheOriginalPathError`,
`TestMigrationMarkerDiagnosticsCarryNoPaths` and
`TestServeMarkerDiagnosticsCarryNoPaths` pin the rest.

## B676 — the installer says what lets the player open the DAC (2026-11-03)

- **…and the installer says what lets the bridge's own player open the DAC, and runs none of it** (2026-11-03, backlog B676): on a box with an ALSA playback node (`/dev/snd/pcmC*D*p`) whose group (`ls -ld`'s) neither every user nor the account (`id -nG USER`, never this process's groups) is given, on systemd a new install, an upgrade and `--no-init` print `usermod -aG <group> <user>` and `systemctl restart user@<uid>.service   # or reboot`, through sudo where the box has it, each word quoted (`root_line`, `command_line`), and a line on what the restart costs; an account in the group whose login is not gets the restart alone; without systemd, deferred set-up and `--no-init` get a new login instead of a manager restart; `--no-init` prints `init --no-service` and no lingering advice there, even with elogind; an unresolved user name (failed, empty or numeric `id -un`) withholds usermod and asks to add the account to the group by name; a node every user may open, root's group, no node, and `--uninstall` get nothing. **Don't run usermod or the restart, write a user or a group, print a placeholder or a `$USER` a root shell would expand, judge the account by this process's groups, or advise on a box with no playback node.**

*Why.* The player opens the DAC's node for reading and writing; the audio
group owns it on Debian, Ubuntu, Raspberry Pi OS, Volumio and moOde, and a
desktop's seat ACL, which lets a desktop login open it, is not a lingering
service's. Nothing put the bridge's user in the group, and the installer
neither did it nor said to, so a box set up by the installer played nothing
on the bridge's own player, the reason in the log alone.

*The restart.* A systemd user service takes the groups of its user manager:
`user@.service` is a system unit with `User=%i` (`PAMName=systemd-user`), so
the system manager sets the manager's supplementary groups from the group
database (`initgroups`) when it starts it, and the manager, unprivileged,
cannot change them for the services it starts. A lingering manager is
started at boot and outlives every login, so a usermod after it reaches none
of its services, and restarting the bridge's own unit inside it would not
either; the user's next login does not restart it. Root's `systemctl restart
user@<uid>.service` starts a new manager, whose groups are read again, and
the bridge's unit, enabled for `default.target`
(`internal/packaging/systemd.service.tmpl`), starts with it. A `reload`
would not do: `user@.service`'s ReloadSignal re-executes the manager in
place, its credentials kept. The groups' half is systemd's exec code as the
brief's reviewers read it (a unit with `User=` gets its user's groups by
`initgroups` at its exec); the unit's half is
`/usr/lib/systemd/system/user@.service` on Ubuntu 26.04 (systemd 259):
`User=%i`, `PAMName=systemd-user`, and `ReloadSignal=RTMIN+25`, "reexecute
the manager on service reload". Neither was tried on a box: the session that
wrote it may not change a host's users, groups or services. The restart stops every service of the user's, a desktop
or terminal session under the manager included, so it is printed with that
cost and the advice to run it from another administrative session; a reboot
is the alternative printed beside it.

*Where.* A new install prints it after the box's sound cards are reported
(`report_outputs`), an upgrade after its service is seen serving, `--no-init`
after the lingering line on systemd. Without systemd, both `--no-init` and
the deferred set-up print a new login instead of the manager restart
(`audio_advice self`). `--no-init` prints `bridge init --no-service` there,
with no lingering advice even when elogind provides loginctl. `detect_install`
sets SYSTEMD before `fresh_install` reads it; no reordering is needed. `--uninstall` prints none of it.
Nothing is said on a box with no playback node: there is no DAC for the
player to open, and no node to read the group from (a group named on another
distribution's nodes would be a guess); a DAC plugged in later is the
doctor's (`player-dac`) and the console's Player page's to name.

*Conventions.* As the lingering line, print-only for what is root's, and
each command on a line of its own, built by `command_line` (through
`root_line`, sudo first only where the box has sudo) with the values
resolved: the user's name and uid, and the node's group as `ls -ld` names
it, never a placeholder and never a `$USER` a root shell would expand
(Codex). `TestTheInstallerSaysWhatLetsThePlayerOpenTheDAC` runs the printed
lines as printed against the recorder (`testdata/argv`, its allowlist with
`usermod` beside `systemctl`) and requires neither to have run (a `usermod`
stub logs any call, and `mutations` counts it); the `id` stub answers `-nG
USER` from the account's groups and `-nG` from this process's. The record
is in `ops/engineering-log.md`, 2026-11-03, B676.

*Round 2.* `check_user` distinguishes a resolved `id -un` name from its
numeric fallback, an empty answer and an all-numeric answer; it never trusts
`$USER`. Without a resolved name, the DAC advice keeps the uid and asks to
add the account to the node's group by its user name, withholding usermod.
A group lookup that fails for that unresolved account does not hide the
advice. A systemd manager is still named by uid. The socket-free
`TestNoInitAdviceMatchesTheHost` runs main with only download and installation
stubbed, exercises both systemd states and all four name answers, executes
printed commands against the argument recorder and requires no mutations.

## The updater's own install, rollback, hold and restamp under the install marker's lock (backlog B692, 2026-11-03)

- **The updater's own install, rollback, hold and restamp hold the install marker's lock: `Updater.Install` (`bridge update`, the console's, the auto-installer's) takes `update-state.lock` only after its download, verification and settings check, decides again under it (`decideUnderLock`: a swap of that release waiting for its restart, whichever process made it; for the auto-installer, a hold recorded since its gate) and holds it through its marker writes and swap, a failed swap putting back the marker it read; the console's `Updater.Rollback(ctx, …)` holds it from its read through its record and rename, refuses where `DataDirOpenedBy` names a version other than the backup's (`RollbackRefusal`), and records durably before the rename the marker it read with the version held back and its migrations vetoed (`OperatorRollbackRecord`); the refused candidate's hold and the restamp write only under it; a lock that cannot be held (another holder past the 30 s wait, ENOLCK, a failure, a filesystem that keeps none) refuses before any write or swap, naming no path (`ErrMarkerLockUnavailable`; the console's 409s `marker-locked` and `rollback-refused`), and a stop in its wait returns the stop** (2026-11-03, backlog B692). **Don't hold the lock through a download, take it twice on one path or take server.lock or the ladder under it, write the marker or swap on these paths without it (a filesystem that keeps none included), skip the decision under it, act on a context that ended before an uncontended take, roll back over a store a newer version opened or before the veto is recorded, drop the marker's other fields at a rollback, let serve's stop take the lock, or let `scmStopWait` pass half its wait.** `TestAnInstallThatHoldsTheMarkersLockFirstKeepsTheMigrationWaiting`, `TestAMigrationThatHoldsTheMarkersLockFirstIsReadByTheInstallsDecision`, `TestASecondProcesssInstallDecidesAgainUnderTheMarkersLock`, `TestAnAutomaticInstallDecidesAgainOnAHoldRecordedWhileItDownloaded`, `TestAnInstallRefusedForTheMarkersLockSwapsNothing`, `TestAnInstallWhoseSwapFailsPutsTheMarkerBackAsItReadIt`, `TestTheHoldOfARefusedCandidateIsRecordedOnlyUnderTheMarkersLock`, `TestTheRestampRefusedForTheMarkersLockWritesNothing`, `TestARollbackThatHoldsTheMarkersLockFirstVetoesTheMigration`, `TestAMigrationThatHoldsTheMarkersLockFirstRefusesTheRollback`, `TestARollbackIsRefusedOnceTheVersionItRollsBackFromOpenedTheDataDir`, `TestARollbackRecordsItsHoldAndVetoBeforeThePutBackAndKeepsTheRest`, `TestARollbackRefusedForTheMarkersLockWritesNothing`, `TestAManualRollbackThatHoldsTheMarkersLockFirstLeavesTheBootNothingToDo`, `TestABootThatHoldsTheMarkersLockFirstLeavesTheManualRollbackNothingToPutBack`, `TestServeStopsWhileAnotherProcessHoldsTheMarkersLock` and `TestAnSCMStopFitsInsideTheMarkersLockWait` pin it. → History-rules-config-lifecycle.md

*The residual.* B613 serialized four writers of `update-state.json` on one
persistent lock, `<dataDir>/update-state.lock` (`updater.LockState`): the
early rollback, the boot's housekeeping, serve's admission and every
migrating store open (`updater.DisarmBeforeMigrating`). The updater's own
paths still read and wrote the marker, and swapped the binary, without it:
`Updater.Install`, `Updater.Rollback`, the refused candidate's hold
(`recordHold`, `recordRejection`) and the auto-installer's restamp before
its restart (`refreshInstallAttemptedAt`). So an install could arm its
marker, a migrating open record that it opened the data dir over it, and
the install's record of its swap (`markSwapStarted`, written from its own
copy of the marker) take the record away, on Windows across a window of up
to `scmStopWait`; a second process's install (`bridge update` beside
serve's own; the two share no `installInFlight`) could swap again over the
first one's and replace its backup with a copy of what was live; a
rollback could rename the backup back while a migration of the version it
rolled back from recorded, or read the marker while the boot's
housekeeping decided on it. Round 4's long form named this as the
residual.

*Where each path runs, and what it holds.* Read from the code before the
change (Codex's table, the brief's binding note 4):

| Path | Process | Locks it holds already |
|---|---|---|
| `Install` from `bridge update` (`cmd/bridge/update.go`) | its own | none: its own Updater's `installInFlight` |
| `Install` from the console (`apiUpdatesInstall`, `updateInfoAdapter.Install`) | serve | `server.lock` for serve's life; the store's open at start has let the ladder go |
| `Install` from the auto-installer (`maybeAutoInstall`, on Run's `scanCtx`) | serve | `server.lock` |
| `Rollback` from the console (`apiUpdatesRollback`, `updateInfoAdapter.Rollback`) | serve | `server.lock`; there is no CLI rollback |
| the hold (`recordHold`, inside `Install`'s settings check) | the install's | as the install's |
| the restamp (`restartWhenDrained`) | serve | `server.lock` |

Taking `update-state.lock` on these paths inverts nothing, since the
documented order is server.lock, the ladder, then this one, and nothing
under it on these paths opens a store or takes `server.lock`: Install's
held section is a marker read, the decision (whose `HeldBack` reads the
settings file for its digest), two marker writes and the swap (a staged
rename, a link or two renames, a directory fsync, and on Windows the SCM's
stop first); Rollback's is a read, a stat of the backup, a write and the
rename; the hold's a read and a write; the restamp's a read and a write.
The console's install inside serve satisfies the order as it stands:
serve took `server.lock` first, for its life, and holds no ladder then.
Each path takes the lock once (`lockMarkerForUpdate`), and the helpers it
runs under it (`decideUnderLock`, `swapAwaitingRestart`, `HeldBack`,
`recordRejection`, `putMarkerBack`) take none.

*Decided again under the lock* (the brief's binding decision 2). Install
takes the lock only once the download, its verification and the settings
check are done: never through the download, which may run for
`downloadTimeout` (15 minutes) while every migrating store open and every
start waits at most 30 s for the lock. Under it, it reads the marker again
and makes the decisions that read it (`decideUnderLock`): a swap of the
same release waiting for its restart refuses (`swapAwaitingRestart`,
`ErrInstallPendingRestart`), whichever process made it, so the second of
two processes stops before it replaces the first one's backup; and for the
auto-installer, a hold of the candidate recorded since its gate read the
marker refuses as the gate would have (`HeldBack`, `ErrInstallHeldBack`, a
`holdError` the auto-installer logs as deferred). The checks before the
download stay, unlocked: they spare a download, and the decision that
binds is the one under the lock. A swap that fails puts back the marker
the install read under the lock (`putMarkerBack`), where it used to
remove it: the binaries are as they were, and a hold or a veto an earlier
rollback recorded must not go with a failed attempt (B613's "Don't drop
the veto at a clear"); where there was no marker, none is left, as
before.

*The rollback after a migration* (decision 3). The rollback refuses where
the marker records that a version other than the backup's opened the data
dir under the install (`RollbackRefusal`: `DataDirOpenedBy`, which serve's
admission writes at that version's start and a store open of it writes
before it migrates), since that version's migrations may have changed the
store beyond what the restored binary reads; the early rollback and the
boot's refuse the same. In every flow the updater makes, that version is
the one the rollback would roll back from. No confirmation flow and no
forced downgrade: that would be a brief of its own. Where it may go on, it
records, durably and before the rename, the marker it read with the
install ended (Status "", InstalledAt and RollbackStarted cleared, so no
boot acts on it), the version rolled back from held back as an operator's
rollback (no reason, no digest: the gate words it) and its store
migrations vetoed (`MigrationVeto`), so a command of that version still
running from its image cannot migrate the store past the version put back
(`OperatorRollbackRecord`); the install's versions, time and swap stay
beside the record. It used to replace the marker with `RejectedVersion`
alone, after the rename. A rename that fails puts the marker back as it was
read. A marker it cannot read refuses it (whether a newer version opened
the data dir cannot be told), and so does a missing backup, in words that
name no path where `RollbackBinary`'s own error names the backup's. One
veto is kept at a time: a rollback's replaces an earlier one.

*Where the lock cannot be held* (decision 1). Another holder past the
wait, every try answering ENOLCK, and any other failure refuse each path
before its first write or swap, with `ErrMarkerLockUnavailable` in words
that name no path (`markerLockError`, wrapping the `*StateLockError`); a
filesystem that keeps no locks (EOPNOTSUPP, ENOTSUP,
`errors.ErrUnsupported`: `LockState`'s hold that is not held) refuses them
too, an explicit install, the auto-installer, the operator's rollback, the
hold and the restamp alike, so these writes join the early rollback and the
boot's housekeeping, which refuse there since round 4, while a migrating
store open goes on unlocked there as round 3 decided. On such a mount the
bridge updates only with the service stopped, its binary replaced by hand
(the runbook says so, and LockState's once-a-process line). The hold and
the restamp were best-effort and stay so: a hold not recorded means the
next check asks again, a restamp not made leaves the pre-restamp
behaviour, and the restart goes on. The console answers the two refusals
409 `marker-locked` and `rollback-refused`, with the updater's words
(`mapUpdaterError`, `classifyUpdateError`); `bridge update` adds that the
running bridge is unchanged.

*A stop, and the uncontended take.* The wait keeps its 30 s bound and ends
with its caller's context (round 4): the console's install and rollback
pass the request's, the auto-installer, its hold and its restamp serve's
`scanCtx`, and `bridge update` its signal context. Since `LockState` takes
a lock no one holds whatever the context says, `lockMarkerForUpdate` asks
the context again once the lock is taken, so a caller stopped by then
mutates nothing. The console had no whole-handler timeout, and
`UpdateProvider.Rollback` took no context: it takes the request's now.

*Windows* (decision 5). A swap and a rollback stop the SCM service first
(`stopServiceIfRunning`, up to `scmStopWait`, 15 s) with the lock held, so
two things must hold, and both are pinned. Serve's stop never takes
`update-state.lock`: its takers in a serve process are its start (the early
rollback before the data dir's lock, the admission, the store's open, the
housekeeping) and the updater's paths, which run on a request's context
or on `scanCtx` and end with them, and serve's shutdown joins neither past
its grace; `TestServeStopsWhileAnotherProcessHoldsTheMarkersLock` holds the
lock, with a 10-minute wait, while serve stops, and fails if any taker
waits. And the stop fits the wait: `scmStopWait` moves to a file every
platform builds (`swapbound.go`), and
`TestAnSCMStopFitsInsideTheMarkersLockWait` holds it to at most half of
`defaultStateLockWait`, leaving the other half for the marker's writes and
the renames (every production install names its settings, so the
candidate is staged beside the binary for its check before the lock, and
the swap's staging is a rename in that directory). One wait has no
context: a store's open
(`DisarmBeforeMigrating`). A serve the SCM stops while its store's open
waits for `bridge update`'s lock stops when that wait ends, which is when
the CLI lets the lock go: its own SCM wait ends at `scmStopWait`, and an
install whose stop timed out fails, its marker put back. Not a deadlock;
the install fails, as it did before B692 for a service that stopped late.

*Tests.* Red first on the seams commit (bb6d5023: `Rollback` takes a
context, `InstallOptions.RestoreBackup` and a hold before Install's swap;
nothing changed), so as on main, for every interleaving in both orders:
the install that holds the lock and the migration that waits, then records
over the install's whole marker; the migration that holds it and the
second process's install that waits, then refuses as waiting for a
restart, the record and the backup intact; the second process's install
alone; the auto-installer over a hold recorded meanwhile; the rollback that
holds it and the migration that waits, then refuses under the veto; the
migration that holds it and the rollback that waits, then refuses; and
against the boot's housekeeping, the rollback held between its read and
its record (`InstallOptions.BeforeRollbackRecord`: held at its rename, the
housekeeping read the record and rightly did nothing) and the housekeeping
held at its rename, the rollback then finding no backup. The red for the
housekeeping pair was observed again on the red commit's rollback with the
hold moved before its record. Each lock outcome is a row on each path
(contention, ENOLCK, a failure, a filesystem that keeps none, a stop while
waiting, a stop before an uncontended take). Pins of the new order, green
before and after: `TestARollbackWhoseRenameFailsPutsTheMarkerBack` and
`TestServeStopsWhileAnotherProcessHoldsTheMarkersLock`;
`TestARollbackWithNoBackupRefusesWithoutAPath`,
`TestTheConsolesRollbackIsRefusedOverAStoreTheInstalledVersionOpened`,
`TestMapUpdaterError` and `TestClassifyUpdateError` carry the console's
words and codes. Eighteen negative controls, one a guarded path, each
observed exactly as predicted (the log's entry has the table).

*Left as it is.* The checks before the download, the console's reads for
display (`rejection`, `deferredReason`, `canRollback`; since B707
`readMarker`, one read for the release held back and the rollback offered,
and `deferredReason`) and `bridge update`'s "Held:" line read the marker
without the lock, deciding nothing. The console's Roll back button followed
`.bak`'s presence (`canRollback`), so it showed where the rollback now
refuses, and the click answered 409 with the reason, until B707 (its
section below): the panel offers it only where the rollback would run, and
says why not. PROTOCOL.md's Updates paragraph says a manual rollback stays
possible for one boot after a successful install; since B692 it is refused
once the new version opened the data dir, which its start records: that
sentence changes with the iOS mirror, not here. The console's 502 bodies
for a swap or a rename that fails still carry the binary's path from
`swapBinary`'s and `RollbackBinary`'s errors. The record is in
`ops/engineering-log.md`, 2026-11-03, B692.

## An operator's rollback under way until its rename, and the stops in the updater's lock waits (backlog B692's round 2, 2026-11-03)

- **…and an operator's rollback stays under way until its rename has landed, and a stop in the updater's lock waits is a stop: the rollback's record carries `RollbackStarted` where the marker names the install, and `Rollback` clears it only after the rename, so a rollback the process's end stops between the two is finished by the version that runs next, by B613's rule for a rollback's record: on the target still in place, its commands' migrations refuse under the veto and its serve admission records `DataDirOpenedBy` and lifts the veto in one write, after which every rollback refuses but the crash rollback, which needs no migration of the store begun (B660), and its housekeeping puts the install back on probation (`BootRollbackOvertaken`, `ProbationOverRollback`; until B660 it confirmed the install); on the previous version, the backup back, its housekeeping ends the record as a rollback done (`BootRollbackFinished`); a stop while the restamp waits or writes requests no restart, and one while a hold's record waits is the install's answer, the context's error** (2026-11-03, backlog B692's round 2). **Don't clear the under-way mark before the rename has landed, lift a veto at the target's admission without recording that it opened the data dir, request a restart after a stop, or answer a stop with a refusal.** `TestARollbackStoppedBeforeItsRenameStaysAnInstallTheTargetsStartResolves`, `TestARollbackStoppedAfterItsRenameIsFinishedByTheVersionThatRuns`, `TestARollbackClearsItsRecordOnceItsRenameHasLanded`, `TestAManualRollbackStoppedBeforeItsRenameKeepsTheStoreFromASecondRollback`, `TestAManualRollbackStoppedAfterItsRenameIsFinishedByThePreviousVersionsBoot`, `TestAStopWhileTheRestampWaitsRequestsNoRestart` (its write's stop since round 3) and `TestAStopWhileTheHoldWaitsIsTheInstallsAnswer` pin it. → History-rules-config-lifecycle.md

*The defect* (Codex on #1387, P1). Round 1's record (`OperatorRollbackRecord`)
cleared Status and `RollbackStarted` before the rename. A rollback the
process's end stopped after that write and before the rename left the
version it rolled back from, the install's target, in place under a record
no step read as an install: the target's next serve admission lifted the
veto, its own version's, and since `InstallArmed` was false neither the
admission nor a migrating open recorded `DataDirOpenedBy`, so the store
migrated, and a second manual rollback, finding no record of the data dir,
put the backup back over the migrated store.

*The fix.* The record says the rollback is under way (`RollbackStarted`,
where the marker names the install it undoes), and `Rollback` clears that
only once its rename has landed, in a second write under the lock. A clear
that fails is said and not returned: the rollback stands, its hold and its
veto with it, and the boot of the version put back finishes the record.
This is the early rollback's record form (B613's round 3), whose finishing
B613 had already built: no new boot code, and Argon's two cases are the
two the boot reads by the version that runs (`DecideBootAction`).

*On the still-new binary* (Argon's binding note: one rule, said and tested
step by step). The rule is B613's for a rollback's record, which the
operator's now is: until a start of the target goes on past it, the record
is the install still (`InstallArmed`), so
- a migrating store open of the target, a command running from its image,
  refuses under the veto (`ErrMigrationVetoed`): it cannot tell a rollback
  stopped before its rename from one whose rename landed and whose clear
  did not;
- the target's serve admission, the one start that shows the target in
  place and its settings loading, records that it opened the data dir and
  lifts the veto in the same write (`AdmitBoot`), after which its store
  migrates and every rollback refuses (`RollbackRefusal`; since B660 the
  crash rollback alone goes on over that record, and only where no
  migration of the store began);
- its housekeeping confirms the install on that record
  (`BootRollbackOvertaken`, `ConfirmOverRollback`), `.bak` kept for one
  more boot, as after any confirmed install (since B660 it puts the install
  back on probation, `ProbationOverRollback`, and `.bak` stays until a
  start of it is confirmed);
- a second manual rollback after that admission refuses.
Argon's first option, the veto kept through the target's admission, was not
taken: with a migration step pending the target's serve could not open its
store, and with no console up nothing would finish the rollback. Refusing
the commands' migrations and recording the serve's start keeps the store
from a downgrade in either order, and it is the rule B613's round 4 already
holds for every vetoed version's own admission.

*The mirror window* (Argon). The rename landed and the process ended before
the clear: the backup's version runs next, its housekeeping ends the record
as a rollback done (`BootRollbackFinished`: the install cleared, the hold
and the veto kept), its admission writes nothing, and a command of the
target meanwhile still refuses to migrate. Where the target runs instead (a
serve of it started from its image before the rename, B613's round 4
residual), its admission records and its housekeeping confirms (since B660,
puts the install back on probation), as above.

*Stops are stops* (Codex, the two P2s). `refreshInstallAttemptedAt` returned
nothing, so a stop while the restamp waited for the lock, or before an
uncontended take, was swallowed and `restartWhenDrained` requested the
restart: an operator's or the SCM's stop became exit 75 and a supervisor's
restart. It returns the stop now, and no restart is requested. `recordHold`
swallowed a stop the same way, and `Install` answered `ErrConfigRefused` or
`ErrConfigCheckUnsupported`; it returns the stop, and `checkCandidate`
answers with it, the context's error, which the auto-installer reads as no
failure. Other failures stay best-effort, as round 1 left them: a hold not
recorded is said and the next check asks again, a restamp not made is said
and the restart goes on.

*Tests.* Red first on fb0d3bbe, with no seams added: a rollback is stopped
where the process would end it, by `runtime.Goexit` inside `RestoreBackup`
(its deferred release of the lock runs, nothing after it does), before the
rename or just after it. The steps on the still-new binary in
internal/updater, and the restart test in cmd/bridge (the console's
rollback stopped before its rename; a command's store open vetoed; this
build's real serve started over the record, its admission's record, its
store's migration and its confirmation; a second rollback refused, the
binaries untouched), and the mirror window with the previous version's
real housekeeping. `TestARollbackClearsItsRecordOnceItsRenameHasLanded`, a
pin, was green before. Four negative controls, each observed exactly as
predicted (the log's round-2 entry).

*Left.* A rollback with no install recorded (a hand-staged `.bak`, or one
after the marker was cleared) is not marked under way: there is no target
to finish it and no record of the data dir to keep, as before. The words
of the housekeeping's confirmation and of the vetoed migration's refusal
now name any rollback ("begun before this start", "a rollback of the
install"). PROTOCOL.md says nothing round 2 changes.

*Round 3* (Codex on round 2, P2). The restamp returned the stop only from
its lock's wait: one that landed during its write, the lock held, was
swallowed, and the restart was requested, exit 75 and a supervisor's
restart of a service the operator stopped. `restartWhenDrained` asks the
context again once the restamp has returned, just before it requests the
restart, and requests none after a stop; what is left is the few
statements between that ask and the request (`serveRestart.request` marks
any request it gets). Red first on the seam that changes nothing
(`Updater.restampSave`, the write's stand-in, held until serve's context
ends): `TestAStopWhileTheRestampWaitsRequestsNoRestart`'s "stopped during
its write", one restart requested; and one negative control, the ask
taken out, red on that subtest alone (the log's round-3 entry).

## A late first boot of the installed version completes its install (backlog B705, 2026-11-03)

- **The version installed is taken up at its first boot however late it comes (on probation since B660, confirmed once a start of it has answered and stayed up), and no clear at boot drops the record that a version other than the backup's opened the data dir while `.bak` may stand: `DecideBootAction` asks the version before the window (any other version: the window, then `SwapStarted`, then `BootInstallFailed`); `ClearInstall` keeps `DataDirOpenedBy` and `PreviousVersion` where `RollbackRefusal` refuses on them; a removal of `.bak` that fails leaves the marker for the next boot, and one that lands drops the record** (2026-11-03, backlog B705). **Don't read the window before the version, drop the record in a clear that leaves `.bak`, clear the marker after a removal that failed, or keep the record once `.bak` is gone.** `TestALateFirstBootOfTheInstalledVersionConfirmsTheInstall`, `TestAClearAtBootKeepsTheRecordARollbackRefusesOn`, `TestDeferredRestartRefreshesInstallRecency`, `TestALateFirstStartOfTheInstalledVersionCompletesItsInstall`, `TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart`, `TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied`, `TestTheBootKeepsARecordedHoldWhereItClearsAnInstall` and `TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir` pin it. → History-rules-config-lifecycle.md

*The defect* (found while verifying a review finding on B692, read from
the code; this section's tests reproduce it). `DecideBootAction`'s
`installing` case checked the recency window (6 h) before the version, so
an install marker older than the window was `BootClearAbandoned` whatever
version booted. serve's start runs, in this order: the early rollback, only
where the settings are refused, before the data dir's lock
(`recoverRefusedInstall`); `server.lock`; the boot's admission
(`disarmEarlyRollback`, `updater.AdmitBoot`), which records
`DataDirOpenedBy` over any armed install whatever its age, since
`ArmsDataDirRecord` asks no window; the player's preflight, the TLS pair and
the token store; the store's open and its migrations (`manifest.OpenStore`,
whose `DisarmBeforeMigrating` finds the record made and adds nothing); and
last the housekeeping (`maybeRollbackOnBoot`, `bootHousekeeping`), under
`update-state.lock`. So the installed version's first start past the window
(`bridge update` with the service stopped and a start the next day, or a
crash between the swap and the restart) recorded that it opened the data
dir, migrated the store, then cleared the marker as abandoned, and
`ClearInstall` dropped the record with `TargetVersion` while `bridge.bak`
stayed. A missing marker loads as empty, so the console's Roll back passed
`RollbackRefusal` (B692) and put `.bak` back over the migrated store; and
the install was never stamped `installed`, so `BootCleanupBak` never
removed `.bak` and the console kept offering the rollback. A rollback's
record (`Status ""`, B613's round 3) already read the install's version
booting as the install working, however old the record.

*The reorder.* The version first: the install's version booting is
`BootInstallSucceeded` (since B660 `BootInstallProbation`, which writes
nothing: the confirmation is `ConfirmInstall`'s, once a start has answered
and stayed up, B660's section) whatever the marker's age, and before `SwapStarted`
as it already was (a marker an updater before `SwapStarted` wrote); any
other version keeps the window (abandoned past it, swapped or not), then
`SwapStarted` (Windows' SCM-stop window, `BootClearNotSwapped`), then
`BootInstallFailed`. A stale marker whose version matches is success on
purpose: the housekeeping runs only once serve's settings loaded and its
admission recorded. The existing abandonment cases run another version, so
they stand. The success write keeps the rest of the marker, its record
included; `RollbackRefusal` refuses on it while the status is `installed`;
and the boot after it removes `.bak` (`BootCleanupBak`) as after any
confirmed install. A success write that fails leaves the marker
`installing`, and the next boot, of any age now, confirms it again. (Since
B660 a matching version is probation, not success, at any age: the boot
writes nothing, and the write that stamps `installed`, keeping the rest of
the marker, is the confirmation's, asked again at its next round where it
fails.)

*The record, wherever `.bak` stays* (the brief's binding goal, Codex's
note: the manual rollback's refusal holds whenever `.bak` remains and a
version other than its own opened the data dir). Three more paths dropped
it, each keeping its action:
- the refused boot rollback (`BootInstallFailed` over `DataDirOpenedBy`):
  it refused because the record exists, then cleared the marker, record and
  all, so a later Roll back went on;
- the abandoned and the not-swapped clears, the same shape;
- a cleanup of `.bak` whose removal failed (`BootCleanupBak`), which went on
  to `ClearInstall` and left `.bak` with no record; on Windows
  `RemoveBackup` returns a deletion's failure (`swap_windows.go`).
`ClearInstall` keeps `DataDirOpenedBy` and `PreviousVersion` beside the hold
and the veto where `RollbackRefusal` refuses on them. That marker has
`Status ""` and no `RollbackStarted`: no boot acts on it (`BootNoop`), it
arms nothing (`InstallArmed` is false, so the early rollback and the
admission leave it), and an operator's rollback refuses on it. A cleanup
whose removal failed leaves the marker as it is, `installed`, says so, and
returns, so the next boot tries the removal again; one whose removal landed
drops the record before `ClearInstall`, since nothing is left for it to
guard. A new install's fresh marker drops it by design: its swap makes a new
`.bak` of the version running then. The restore paths (`BootInstallFailed`'s
rename, `BootRollbackFinished`) have no record to keep: the first runs only
where none is recorded, and a rollback's record holds none but in B613's
round-4 residual.

*The late start that dies before its housekeeping* (Argon). The version
installed starts late, its admission records, its store migrates, and it
ends before the housekeeping; the operator puts the old binary back by hand,
keeping `.bak`, and starts it: another version over the marker, inside the
window (`BootInstallFailed`, refused on the record) or past it
(`BootClearAbandoned`). Both keep the record now, so a Roll back of `.bak`
refuses. That holds where the version put back carries B705: an older
release's housekeeping clears the marker by its own rule.

*The other paths that can clear an install marker while its version runs*
(the brief's item 3):
- the restamp before a deferred auto-install restart
  (`refreshInstallAttemptedAt`): under the lock it reads the marker, touches
  only an `installing` one and writes it back with its time alone changed,
  so it cannot drop `DataDirOpenedBy`. After the reorder it is what keeps
  the rollbacks' windows (the boot rollback's, the early rollback's) running
  from the restart, and `TestDeferredRestartRefreshesInstallRecency` asserts
  that itself: the marker's time is the restart's, the rest is as it was,
  a record a migrating command of the new version made meanwhile included,
  and a boot of another version is still `BootInstallFailed`;
- the early rollback (`recoverRefusedInstall`, `EarlyRollback`): it refuses
  on a recorded admission, asks again under both locks, writes
  `RollbackRecord` only where no version is recorded, and puts back the
  marker it read when the rename fails, so it cannot drop one; its age and
  future-date checks are unchanged;
- `ClearInstall`'s callers: above.
Beside them, and unchanged: a marker that cannot be read is removed by the
disarm that finds it (B613), and a record in it with it.

*Tests.* Red first on the seam that changes nothing (1b3e4cc8:
`bootHousekeepingWith`, a removal `BootCleanupBak` takes as a parameter),
so as on main: `TestALateFirstBootOfTheInstalledVersionConfirmsTheInstall`
(6 of 13 subtests: the install's version past the window, swapped or not,
spelled with a v on either side, its admission recorded; its 7 controls
green), `TestAClearAtBootKeepsTheRecordARollbackRefusesOn` (2 of 4),
`TestALateFirstStartOfTheInstalledVersionCompletesItsInstall` (this build's
real serve over an install armed 30 h ago and a store a migration behind:
the admission, the migration, the housekeeping, then `Updater.Rollback`
refused with both binaries unchanged, then the next start's cleanup),
`TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart`,
`TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied` (2),
`TestTheBootKeepsARecordedHoldWhereItClearsAnInstall` (3 of 8) and
`TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir`.
`TestTheBootKeepsARecordedHoldWhereItClearsAnInstall`'s case "an install
that did not come up, the data dir opened since" asserted the loss (the
hold and nothing more) and is corrected on purpose. Five negative controls,
each observed exactly as predicted (the log's entry has the table).

*Left.* The console's Roll back button followed `.bak`'s presence
(`canRollback`), so where a clear kept the record it showed and answered
409 `rollback-refused` until `.bak` went, by hand or with a new install, as
B692 left it, until B707 (its section below): the panel offers it only
where the rollback would run, and says why not. `swapAwaitingRestart` still
expires at the window outside the process that swapped, as before: the
window bounds what a boot puts back, and the refusal expires with it.
PROTOCOL.md's Updates paragraph says the next boot confirms the install or
restores `.bak`, which is now true at any age: no change there. (Since B660
the boot no longer confirms it, and that paragraph is out of date; it
changes with the iOS mirror, B660's section.) The record
is in `ops/engineering-log.md`, 2026-11-03, B705.

## The console's Roll back offered only where the rollback would run (backlog B707, 2026-11-04)

- **The console's Updates panel offers Roll back only where the operator's rollback would run, from one read of the install marker per status: `CanRollback` is `.bak` present and no refusal from `updater.RollbackRefusalOn`, which `Rollback` asks itself of its own read (`RollbackRefusal`'s, or a fixed sentence for a marker that cannot be read), for `Status` and for an install's answer alike; where `.bak` is there and the rollback would refuse, Roll back stays hidden and a Rollback row beside Held update says why (`RollbackRefusal`, JSON `rollbackRefusal`), in the rollback's words and naming no path, at the first paint and in `renderUpdateTile`; with no `.bak` nothing is said, and the API keeps its 409** (2026-11-04, backlog B707). **Don't offer Roll back from `.bak` alone, decide it by any check but `RollbackRefusalOn`, read the marker twice for one status, take a marker that cannot be read for none, show a reason where there is no backup, or put an error's text or a path in it.** `TestTheUpdatesPanelOffersRollBackOnlyWhereTheRollbackWouldRun`, `TestTheConsoleOffersRollBackOnlyWhereTheRollbackWouldRun`, `TestTheRollbacksRefusalOnItsReadOfTheMarkerIsTheConsoles`, `TestTheSettingsPageSaysWhyRollBackIsNotOffered`, `TestSettingsUpdateRowsRenderOnceAndKeepDTAdjacency`, `TestTheUpdatesPanelScriptShowsWhyRollBackIsNotOffered` and `TestTheUpdatesPanelShowsRollBackOrWhyNot` (under node) pin it. → History-rules-config-lifecycle.md

*The defect* (B705's "Left", read from the code; this section's tests
reproduce it). Since B692 an operator's rollback refuses, 409
`rollback-refused` with its reason, where the install marker records that a
version other than the backup's opened the data dir (`RollbackRefusal`),
and where the marker cannot be read; since B705 that record outlives every
clear at boot that leaves `.bak` where it is. The console's Updates panel
still offered Roll back wherever `.bak` stood beside the binary
(`canRollback`, a stat of it alone). Once the version installed has
started, its admission has recorded that it opened the data dir, so in the
case the button exists for ("the version I just installed is broken") it
showed, and its click answered 409.

*One check, asked by both.* `updater.RollbackRefusalOn(st, readErr)` is
the refusal the rollback makes on its read of the marker, before it looks
for the backup: `RollbackRefusal(st)` for a marker it read, and for one it
cannot read a fixed sentence, "whether a version other than the backup's
has opened the data dir cannot be told, since the install marker cannot be
read", naming no path and no error's text. `Rollback` asks it itself, so
the panel and the rollback cannot decide apart; its 409 is those words with
the read's error after them, said without its path (`markerIOError`) and
kept for code through `Unwrap` (B613's round 5). Until B707 the 409 put the
cause inside the sentence, which the panel could not quote; the status, the
refusal's code and the order of the checks are unchanged.

*One read per status* (Codex on the brief). The adapter
(`updateInfoAdapter`, cmd/bridge) reads the marker once for each status it
builds (`readMarker`), and the release held back (`RejectedVersion`,
`RejectedReason`) and the rollback offered are both of that read, for
`Status` and for an install's answer alike: both filled `CanRollback`, and
an install's answer is a status too, so neither can bring the button back.
`CanRollback` is `.bak` present (`backupPresent`, the old stat) and no
refusal; `RollbackRefusal`, a new `admin.UpdateStatus` field (JSON
`rollbackRefusal`, omitempty), carries the refusal where `.bak` is there,
and nothing where it is not, the ordinary state with nothing to say (Argon
on the brief). A marker that cannot be read refuses (Codex: checking only a
nonempty `RollbackRefusal` would have left the defect there) and holds no
release back, as `rejection` read it. The read takes no lock: a read for
display decides nothing, and the rollback decides again under the lock, so
a version that opens the data dir between the two (a CLI command whose
store migrates) still gets the 409, and the panel catches up at the next
updates frame, 30 s on.

*Hidden, not disabled* (the brief's choice). This panel's way with an
action that cannot run is to leave it out and say why in a row: Install is
absent without an update or a platform that installs, Roll back without a
backup, and Held update says why an available update is not taken. So Roll
back stays hidden where the marker refuses it, and a Rollback row beside
Held update says why, badge `unavailable`, the reason as text: in the first
paint (`settings.html`, its label and its value hidden together, as the
other two rows', so `previousElementSibling` reaches the label) and in
`renderUpdateTile`, through `escapeHTML`. A disabled button would also
fight the click handler, which disables it around its request and enables
it again after a failure (`setDisabled`). The button's own first paint
stays hidden, as before; the first updates frame reveals it where the
rollback would run.

*Not changed.* `POST /api/updates/rollback` keeps its 409 for any other
caller, and the refusals a status cannot foresee answer as they did:
another install or rollback in flight, downloads in flight without
`?force=1`, the marker's lock held past its wait. A Check-now request that
failed still rendered a partial status (`{lastError}`), which hid Roll back
and every row until the next frame, as it hid the button before; since B720
(2026-11-04) it renders no status, and shows its error in the Last error row
beside the rows the last status rendered (the bullet at the end of
`.claude/rules/admin-console.md`).

*Tests.* Red first on a seam that changes nothing (cab93107: the field,
filled by nothing), so as on main:
`TestTheUpdatesPanelOffersRollBackOnlyWhereTheRollbackWouldRun` (3 of 9
subtests: the version installed opened the data dir, the record a clear at
boot kept beside the backup, a marker that cannot be read; its six allowed
and no-backup cases green),
`TestTheConsoleOffersRollBackOnlyWhereTheRollbackWouldRun` (this build's
real serve over its own install: GET /api/updates offered Roll back and the
Settings page had no row, while the POST answered 409; its other case, a
backup and no marker, green: offered, and the rollback runs),
`TestSettingsUpdateRowsRenderOnceAndKeepDTAdjacency` (5 of 5, on the new
row alone), `TestTheSettingsPageSaysWhyRollBackIsNotOffered` (3 of 3) and
`TestTheUpdatesPanelScriptShowsWhyRollBackIsNotOffered`.
`TestTheRollbacksRefusalOnItsReadOfTheMarkerIsTheConsoles` came with the
fix. `TestTheUpdatesPanelShowsRollBackOrWhyNot` runs the shipped
`renderUpdateTile` under node and skips without it. Six negative controls,
each observed exactly as predicted (the log's entry has the table). The
record is in `ops/engineering-log.md`, 2026-11-04, B707.

## An install counted healthy only once it has answered and stayed up (backlog B660, 2026-11-06)

- **An install counts as healthy only once one start of its version has answered as ready and stayed up for `updater.ProbationWindow`, and three of its starts that did not end cleanly roll it back at the next start, before it writes anything, unless a migration of the store has begun or the backup cannot be shown to run here** (2026-11-06, backlog B660; its second round, 2026-11-07). **Don't confirm on a version match, don't let a scan, a root or a certificate decide readiness, don't count a stop (take its count back as the stop arrives, on the caller's context or a restart request, never serve's own context, and join that take-back before the data dir's lock goes), a port another process holds or an address the host does not have, don't take back a permission refusal the backup would not meet (file capabilities the binary lacks) or a tailnet failure that is not the network's, don't roll back once a migration step has begun or where the backup refuses the settings, does not answer their check or predates it, don't let a command but serve's own start migrate the store while the install awaits its confirmation after a start that did not end cleanly, don't hold the auto-installer for a backup the crash rollback would not put back, don't promise a put-back, and don't compare `user_version` to excuse a failed step.** → History-rules-config-lifecycle.md

*The defect* (the self-healing study of 2026-11-02, read from the code;
B613's "Not here" above; B198's scenario). `DecideBootAction` answered
`BootInstallSucceeded` for the target at any age, and the housekeeping
stamped `installed` after the store opened and before anything listened, so
the stamp said nothing about serving, and the next start of any version
removed `.bak`. Under `Restart=always` (`RestartSec=2`) a new binary that
stamped and then crashed had its `.bak` deleted by its own second start,
about two seconds later, after `bridge update` had promised a rollback "if
the new bridge fails to come up at version X within 6h0m0s of restart",
which only a version mismatch triggered. And nothing rolled a version back
once it got past its settings: serve's admission records `DataDirOpenedBy`
before its first write, and that one record refuses the early rollback, the
boot rollback and the operator's, without saying whether the store changed.
Nobody had reported it; the owner wanted it covered by design.

*The confirmation.* The marker stays `installing` until it is confirmed:
not a new status, since `InstallArmed`, the admission, the migration's
record and the rollbacks' records key on it, and the backup's version,
whatever it is, reads it as before. The version match keeps its place ahead
of the window and `SwapStarted` (B705) and answers `BootInstallProbation`,
which writes nothing and takes no lock (the housekeeping's fast path).
Serve's confirmation (`serveProbation`, cmd/bridge/serve_probation.go)
starts once `ServeTLS` runs, only where the marker awaits this version's
confirmation, joined through `bgWriters` on `scanCtx`. It asks every 5 s
until the first ready answer, which starts the window (10 minutes, on the
monotonic clock: timers and `time.Since`, since a box with no RTC boots with
a stale date), asks once more at the window's end, and every 30 s after a
no. `updater.ConfirmInstall` takes the install marker's lock through
`lockMarkerForUpdate` on the confirmation's context, which serve's stop
ends (held, and the context asked again after the take, since `LockState`
takes a free lock on an ended context),
reads the marker again, and stamps only an `installing` marker of this
version: `installed`, `InstalledAt`, and `FailedStarts`, `RollbackRefused`
and `NotReady` cleared. Any other marker stays as it is (an install of a
newer version recorded while it waited). A lock it cannot hold is one line,
asked again at the next round; where the filesystem keeps no locks no
install is confirmed, as the housekeeping confirmed none there before.
`BootRollbackOvertaken` puts the install back on probation
(`ProbationOverRollback`), its count kept, rather than stamping it.

*Ready* is three answers in one round, each of this process: a connect to
the API listener's bound address (a wildcard dialled on loopback,
`probeHost`, as `bridge health` dials) that this process's accept loop took,
read from `acceptWatch`, the listener as serve hands it to `ServeTLS`, which
keeps the remote address of each connection it accepts while a probe is
under way, matched with the probe's own local address (a connect alone
completes in the kernel's backlog with nobody accepting); `GET /v1/health`
run in process through `httpSrv.Handler`, the handler every listener serves,
into a recorder of its own (`healthRecorder`: no `net/http/httptest` outside
tests), answering 200 with `serverVersion` this version; and a store read
(`CountServedTracks`). A round runs on a goroutine of its own, its checks on
a context that ends at 5 s, and the round is waited for a second past that
(`roundGrace`), so a check that takes its cancel says why it ended, and a
call that takes none (the in-process `ServeHTTP`, whose endpoint walk is a
singleflight that ignores its caller's context) is a no at the bound: no
second round starts beside it, its late answer counts for nothing, and the
stop does not wait for it (cli-serve.md's goroutine bullet names it). Not
ready's business: a scan or how far it got, enrichment, the library roots,
a TLS handshake (`health.go`'s reason: an expired or rotated certificate
reads as dead, and a restart cannot fix that), the tailnet, the player,
DLNA. So a slow first scan cannot hold the confirmation back, and a no
never rolls anything back: a start that never answers is never confirmed
and keeps running; at the window's first lapse it says so once, naming the
answer that was missing, and records `NotReady`, which the next admission
and the confirmation clear. Each round logs one `path=/v1/health` access
line.

*The count.* The admission adds one to `FailedStarts` where this version is
the install's target (`DisarmResult.Counted`, `CountTried`), in its one
write with the record where it makes one. Where the count is all it has to
write, the barrier still runs first and still refuses, and the count's own
write failing, or its lock not taken, is a line (`CountMissed`), never a
refusal: a missed count errs toward not rolling back. A crash before the
admission is not counted: a candidate that never runs is what the settings
check before the swap sees, and a start refused for its settings has the
early rollback. The take-back is registered right after the admission, so
it is joined while the data dir's lock is held. A stop's begins as the stop
arrives (`stopTakeBack`, the second round): `context.AfterFunc` on the
context runServe's caller gave it, which only a stop cancels (main's signal
context for SIGINT and SIGTERM, the SCM's Stop, the launcher's, a test's),
and the restart request's own hook (`serveRestart.onRequest`), never
runServe's own context, which it cancels on every exit; it asks for the
marker's lock again, a wait at a time, until the teardown ends, and is then
unregistered and joined. Until the second round the stop's take-back ran
after the whole teardown, which a supervisor that kills a slow stop
(launchd's `ExitTimeOut`, systemd's `TimeoutStopSec`, the SCM's wait) cut
short, leaving a clean stop counted. A crash in the teardown after a stop is
not counted, by the same rule: the stop came first. The other clean ends are
taken back after the teardown, reading a local flag (`notAFailedStart`) that
exactly they set: a bind refused for an address another process holds, one
the host does not have (`doctor.IsAddrNotAvail`, the second round: a static
address not up yet after a reboot made four counted starts in a row, and
the fourth put back a backup that met the same refusal), or a permission the
backup would meet too (`bindRefusalIsTheHosts`: not where `.bak` carries
file capabilities the binary does not), the API's or the console's
(`doctor.IsAddrInUse`, `IsAddrNotAvail` and `IsBindPermission`, exported,
the Windows arms reading Winsock's `WSAEADDRINUSE`, `WSAEADDRNOTAVAIL` and
`WSAEACCES`, since syscall's alone are always false there); and a failure of
the tailnet's network (`tailnetNetworkFailure`: a `*net.OpError` or
`net.ErrClosed`, which the line names; any other failure of the listener
counts, the second round). Never `code`: `runServe` does not recover, and a
panic on serve's goroutine runs the defers with `code` still 0. The
take-back after the teardown gives the marker's lock 2 s (`takeBackWait`),
and is also the last try of a stop's that never had its answer; each goes on
where the filesystem keeps none, as a disarm does, and writes only where the
marker is still an armed
install of this version holding the count this start wrote (`FailedStarts
== CountTried`): a stop after the confirmation and a restart into a newer
install touch nothing, and no earlier start's count goes with this one. A
start that tried to count tries to take back, since `SaveState` publishes
before its barrier can fail. A take-back that does not land leaves one
count, which the confirmation clears.

*The crash rollback.* `updater.ProbationFailedStarts` is 3, so one power cut
or OOM kill does not roll back, and the fourth start at `RestartSec=2` comes
inside systemd's default start limit (5 in 10 s). `recoverCrashedInstall`
(cmd/bridge/serve_crashed_install.go) runs after the data dir's lock, only
where it is this process's, and before the admission, so nothing of the
start has written. It follows the early rollback step for step: the marker's
lock held from its read through the rename (never without it; refused where
the filesystem keeps none), the marker read again under it, then three
checks in this order, the first starting no process, so that a migrated
store does not run the backup at every start: `updater.CrashRollback`
(pure: armed for this version, the count at the limit or past it,
`SwapStarted`, a `PreviousVersion` that is another version, no
`StoreMigratedBy`, `DataDirOpenedBy` empty or this version; no recency
window, since the count has no clock and an install first started the next
day deserves the same, B705); `.bak` a regular file whose `ProbeVersion` is
`PreviousVersion`; and `CheckConfig(.bak, the settings)`, valid going on,
refused, failed or `ConfigCheckUnsupported` refusing, so that a rollback does
not trade one crash loop for another over a key the new version saved. A
backup that predates the check (0.2.2 and older) cannot say whether it loads
the settings, and one that does not would stop at its load with this
version held back from the auto-installer, its strict decoder refusing a key
it does not know: it is refused, and recorded as `the backup predates the
settings check` (the second round; until then it was put back with a
warning). Then `CrashRollbackRecord` durably (status "",
`RollbackStarted`, the version held back with a reason naming the failed
starts and no digest, since this is no fault of the settings, its
migrations vetoed, `DataDirOpenedBy` and the count kept, `RollbackRefused`
cleared), `RollbackBinary`, the marker put back with a reason where the
rename fails, and exit 75; where nothing restarts it, the line tells the
operator to run `bridge serve` again. A refusal is said on stderr (the line
may name the backup's path, as the early rollback's do) and recorded in
`RollbackRefused` in words with no path, value or error text, and serve goes
on, since this start may be the one that works: it counts itself, so every
later start that does not end cleanly asks again, and a refusal that passes
(a settings check that did not answer) is no lasting one. A start that finds
the record with its rename not landed asks all three again before its
admission and finishes; only where it refuses does it go on, and
`BootRollbackOvertaken` then puts the install back on probation. The count
is what tells that record from the other two: the early rollback's record
carries none by design, and an operator's rollback refuses once a start has
been admitted. The version put back ends the record as a rollback done
(`BootRollbackFinished`, in 0.2.3 already, which a literal fixture of
0.2.3's marker and decision pins) and keeps the hold, so the auto-installer
waits for a newer release and a manual install still takes the held
version.

*Begun is changed.* `DisarmBeforeMigrating` writes `StoreMigratedBy` before
the first step wherever an install is armed and this version is not the
backup's, whether or not `DataDirOpenedBy` is there: before B660
`ArmsDataDirRecord` was false once the admission had recorded, so a
migration under serve left no record of its own. A record it cannot write
(its save, its barrier, its lock) refuses the open, as the admission
refuses: no step runs unrecorded. It is asked once, before the first step.
`user_version` is not compared to find that nothing landed: the ladder is
not one transaction (a step's SQL, its `post` and `PRAGMA user_version` are
separate statements), so a step can fail with statements landed and
`user_version` unmoved, and an older binary over a newer store does not
refuse it, but skips every step. This is the design's plainest cost: the
ladder's top moved in 6 of the last 7 releases (0.1.7 23, 0.1.8 26, 0.1.9
41, 0.2.0 44, 0.2.1 50, 0.2.2 60, 0.2.3 60), so an install of such a release
that crash-loops after its first store open gets its backup kept and a
report, not a rollback. Since the second round no command but serve's own
start begins one while the install awaits its confirmation after a start
that did not end cleanly (*The second round*, below).

*What the target wrote without a migration* goes back under the backup with
no check, and none of it stops the backup starting, so none is a refusal in
`CrashRollback`: `tokens.json`, the console's account and its login tickets
and the harvest state are JSON the backup reads with lenient decoders,
dropping a field it does not know at its next write, and so is the install
marker; the player's DSD state (`dsdstate`'s schema check) and its queue
refuse a field they do not know, which turns the player off with a reason,
never serve; a row the target extracted above the backup's
`ExtractorVersion` is served as indexed until its file changes, since the
skip gate's `stored >= current` passes it; any other row the target wrote
(a scan's, the enricher's, a playlist, a favorite) is a row of the schema
both versions share, which the backup reads as its own; the settings are
what `CheckConfig` asks.

*The other three rollbacks keep `DataDirOpenedBy`.* The early rollback, the
boot's and the operator's (the console's Roll back included) refuse on it as
before: a record `ClearInstall` kept from before B660 has no
`StoreMigratedBy` to read, so its absence proves nothing there. The crash
rollback is the one exception, because it asks what they cannot: whether a
migration began, which every migrating open records since B660.

*The auto-installer waits* while the backup still guards something: at its
gate in `maybeAutoInstall`, past its pending-restart path, and again under
the lock in `decideUnderLock`, as the hold is (`ErrInstallAwaitsConfirmation`,
a deferred reason), while the marker is an unconfirmed install of the running
version, the process has been up for less than twice the window
(`Options.Uptime`, monotonic), and the crash rollback would put the backup
beside the binary back (`ProbationHolds(st, current, binaryPath, uptime)`,
both callers passing the binary serve runs as: no `RollbackRefused`, `.bak`
a regular file, and `CrashRollbackAtLimit` passing; the second round added
the first two and the last beside the migration it asked alone, since a
hold kept for a backup no rollback would put back held a newer release,
perhaps the fix, for nothing). A swap then would replace
the last version known to run here with one not yet shown to. Past that
bound an install that never answered does not wait on itself, since a newer
release may be its fix; where a migration began the backup cannot be put
back anyway, so nothing is held. A manual install is not held, and its
`.bak` is the unconfirmed version. A manual install staged and not started,
then an automatic one over it, keeps the behaviour from before: `.bak` is
then the staged release, not `PreviousVersion`, so the crash rollback
refuses it at its second check.

*The report* has one source, `updater.ProbationStatus(st, current,
running)`, in words that name no path or value: `Says`, the starts that did
not end cleanly, less the one a serve holding the data dir's lock counts
until it stops cleanly (`running`, `updater.ServeHoldsDataDir`; the second
round: until then one was taken off whether or not a serve ran), graded by
`Level`, for `bridge doctor`'s `install-probation` line, one builder for the
CLI and the console's doctor (none where no install awaits; ok with none,
warn from one, fail at the limit, beside a refused rollback, or with
`NotReady`; a marker that cannot be read is a warn, not checked), the
surface that works while serve cannot start; and `ThisStart`, the running
start taken off the count, graded by `ThisStartLevel` on the starts before
it (the second round: the console graded the count with itself in it, so
two starts that crashed read as the failure a third makes), for serve's
stderr (a line per start with a count before it, and one for every refusal)
and the console's Updates panel, a Probation row beside the Rollback row.
Neither promises a put-back (the second round): the words follow the crash
rollback's own checks of the marker at the limit (`CrashRollbackAtLimit`),
saying why the backup is not put back where they refuse, and otherwise that
the next start tries to put it back, if the backup can run these settings,
since its version and its settings check are asked only by the start that
acts.

*Replaced, not only added*: `bridge update`'s promise (it says what is
checked now: answered, up for 10 minutes, the backup put back after three
failed starts unless a migration of the store began), `State`'s lifecycle
doc, `EarlyRollback`'s, the migration veto's refusal, `maybeRollbackOnBoot`'s
and the comments at the housekeeping's call and the auto-installer's
restart, the runbook's B613, B692 and B705 paragraphs and its paragraph for
B660, B613's "Not here" above, and the long forms of B613's rounds 3 and 4,
B692's round 2 and B705 here, each where it said a start confirms the
install. PROTOCOL.md's Updates paragraph said the next boot confirms the
install or restores `.bak` until the second round rewrote it; its iOS mirror
is the orchestrator's.

*Tests.* Red first on 99568a9d, the marker's fields and serve's seams added
inert: `TestACrashAfterTheFirstBootKeepsTheBackup` (the housekeeping stamped
`installed` with nothing listening, and the next removed `.bak`),
`TestAnInstallIsConfirmedOnlyOnceItAnsweredAndStayedUp` (installed once serve
listened), `TestThreeFailedStartsRollBackAnInstallThatMigratedNothing` (the
fourth start served, 0, and put nothing back) and
`TestARollbackIsRefusedAndReportedOnceAMigrationBegan` (no migration
recorded). The pure tables in internal/updater, and through the real serve
the readiness, the count, the crash rollback and its refusals, a
migration's record, the confirmation's lock, the doctor's line and the
panel's row; the six existing tests that stamped at the first boot follow
the confirmation now and keep what each pins. Twelve negative controls,
each observed as predicted, and a by-hand run with two real builds: the
log's entry has them.

*The second round* (2026-11-07, PR #1424's review). Besides the stop's
take-back as it arrives, the host's refusals by what the backup would meet,
the backup that predates the check, the hold's arms and the report's words
(each in its paragraph above):

- *A command's migration is held while the install awaits its
  confirmation after a start that did not end cleanly.* `bridge doctor`,
  run to see why starts fail, opened the store and migrated it, and its
  record (`StoreMigratedBy`) refused the crash rollback for good: the one
  backup that would have run, taken away by the diagnostic. A store open
  with a step pending and no permission now refuses, running no step and
  writing nothing (`updater.MigrationHeld`, `ErrMigrationHeld`), while the
  marker awaits this version's confirmation, the crash rollback's checks at
  the limit would put the backup back, and a start before now did not end
  cleanly: `FailedStarts` less the start a serve holding the data dir's lock
  counts until it stops cleanly (`updater.ServeHoldsDataDir`), so with none
  before now a command migrates as it did. Serve's own start carries its
  admission's permission through the open (`DisarmResult.Permit`,
  `updater.MigrationPermit`, `manifest.OpenStoreAdmitted`), which names the
  data dir and the version and which only `AdmitBoot` makes, so it admits
  that start alone. The gate is asked before the install marker's lock is
  taken, and asks whether a serve holds the lock only where the marker counts
  a start of an install of this version awaiting its confirmation
  (`RunningServeCounted`, `MigrationHeldIn`, which the doctor's line, its
  store probes and restore's check ask too), so a command or a doctor with no
  count to read tries no lock. `ServeHoldsDataDir` answers this process's
  own holds from what it noted (`NoteDataDirLock`: serve's as a serve's,
  restore's, reset-player's, clear-missing's and the DAC bind's as a
  command's), since a try of the lock cannot tell this process's serve from
  its restore, and another process's by a try of `server.lock` that never
  waits, letting go at once of one it finds free (what that leaves is
  backlog B760). `bridge doctor` runs no migration either: its probes
  that read the store say they wait for serve's start (`doctorOpenStore`),
  and its `store-migrations` line says how many are pending
  (`manifest.PendingMigrations`, a read-only open that asks the store's
  `user_version` alone and makes nothing). `bridge restore` refuses before
  its first write a snapshot whose store this version would migrate
  (`restoreMigrationHeld`): the restored store is opened at once to mint its
  backup epoch, which would refuse past the database's copy and leave the
  restore half done. Every other command that opens the store says the
  store's refusal: `bridge scan`, `analyze`, `artwork`, `upscale` (and
  `optimize` and `render`, which open the store there), `variants`,
  `manifest`, `library`, `enrichment` and `duplicates`.
- *The swap carries the binary's file capabilities where it may.* It
  carried the mode alone (`installedMode`): the new binary is the process's
  own file, with no extended attribute of `dst`'s, so an operator's setcap
  (cap_net_bind_service, for a port below 1024 as the service user) went
  with the update, the new binary's bind was refused for a permission, and
  that refusal was taken back as the host's, so no rollback ever put back
  the backup that kept it. `carryCapabilities` gives the staged binary
  `dst`'s `security.capability` before the rename, which only a process with
  CAP_SETFCAP may do (`sudo bridge update`); where it cannot (the service
  user's auto-installer, the console's install), the swap goes on, and a
  bind refused for a permission counts where `.bak` carries capabilities
  the binary does not (`bindRefusalIsTheHosts`, Linux's one small attribute
  read; elsewhere files carry none). An SELinux label is not carried: the
  staged binary takes the label the policy gives a new file in its
  directory, which is `restorecon`'s, so a label set by hand (`chcon`) goes
  with the update (backlog B760).
- *Tests that could not catch their defect* now can: a bind refused by the
  host itself (port 1, below the unprivileged floor) beside the errno each
  platform spells, an address no host holds (TEST-NET-1) for
  `IsAddrNotAvail`, serve over a permission refused and over a tailnet
  listener that fails, the confirmation through a listener on the wildcard
  (dialled on loopback, `GET /v1/health` asked with `Host: [::]:port`), and
  the auto-installer's hold tested on every platform.

`TestAStopTakesItsCountBackAsItArrives`,
`TestARestartRequestTakesItsCountBackAsItArrives`,
`TestAServeKilledWhileItDrainsHasTakenItsCountBack` (a real serve process
killed with SIGKILL while a held connection keeps its drain going),
`TestAStopThatFindsTheMarkersLockHeldTakesItsCountBackOnceItIsFree`,
`TestAnErrorExitStaysCountedWhateverItsCallerDoesAfter`,
`TestABackupThatPredatesTheCheckIsNotPutBack`,
`TestACommandDoesNotMigrateTheStoreWhileTheInstallAwaitsItsConfirmation`,
`TestTheDoctorLeavesThePendingMigrationsAndSaysSo`,
`TestServeMigratesTheStoreAfterAFailedStart`, `TestACommandBesideAStartingServe`,
`TestAPermissionRefusalTheBackupWouldNotMeetIsAFailedStart`,
`TestATailnetListenerThatFailsIsAFailedStart` (the third round's name),
`TestAnInstallListeningOnTheWildcardIsConfirmed`,
`TestTheConsoleGradesTheStartsBeforeTheRunningOne`,
`TestTheDoctorCountsEveryStartWhereNoServeRuns`,
`TestTheDoctorSaysTheBackupIsTriedNotPromised`,
`TestTheDoctorPromisesNoPutBackTheMarkerRefuses` and
`TestTheDoctorBesideARunningServeTakesItsStartOff` (cmd/bridge);
`TestServeHoldsDataDirReadsTheLockAndThisProcesssHolds`,
`TestMigrationHeldAsksTheStartsBeforeNow`,
`TestAMigratingOpenIsHeldWhileTheInstallAwaitsItsConfirmation`,
`TestThePermitAdmitsOnlyItsOwnStart`,
`TestCrashRollbackAtLimitAsksTheMarkerAtTheLimit`,
`TestTheAutoInstallerDoesNotWaitBesideARefusedRollback`,
`TestTheAutoInstallerDoesNotWaitWithoutABackup`,
`TestTheAutoInstallerDoesNotWaitWhereTheCrashRollbackWouldRefuse`,
`TestTheSwapCarriesTheBinarysFileCapabilities` and
`TestASwapThatCannotSetTheCapabilitiesStillSwaps` (internal/updater);
`TestPendingMigrationsReadsTheStoreWithoutMigratingIt` (internal/manifest);
`TestARestoreThatWouldMigrateIsRefusedWhileTheInstallAwaitsItsConfirmation`
(internal/backup); and `TestIsAddrNotAvailMatchesWhatTheHostAnswersAnAbsentAddress`
and `TestIsBindPermissionMatchesWhatTheHostAnswersARefusedBind`
(internal/doctor) pin it.

*Left.* What B660 leaves uncovered is filed as backlog B760. The records
are in `ops/engineering-log.md`, 2026-11-06, B660, and 2026-11-07, its
second round.

## Config, settings and process lifecycle

- **An install of a target already staged on disk is REFUSED, and the refusal
  reads the PERSISTED marker.** `swapBinary`'s EEXIST retry does
  `os.Remove(bak)` before re-linking, so a SECOND install of the same version
  destroys the operator's rollback target and replaces it with a copy of what is
  already live — while the console's `canRollback` keeps reporting true,
  because the backup is still there (since B707 it asks the marker too,
  which refuses nothing before the version installed starts). Measured, not
  argued: with the guard removed the test's `.bak` goes from `0.1.0` to
  `0.2.0`. Reachable through the ORDINARY
  console flow — `apiUpdatesInstall` does **not** restart (restart is a separate
  operator action), and `u.status.CurrentVersion` is written ONCE at
  construction (`Install` only decorates the local copy it returns), so
  `UpdateAvailable` stays true for the process lifetime and the console keeps
  inviting the click. Three terms, each load-bearing: **`Status=="installing"`
  AND `SwapStarted`** (an armed-but-unswapped marker mutated nothing — the
  Windows SCM-stop window — and `DecideBootAction` reads it as
  `BootClearNotSwapped`); **the SAME target**, because refusing a newer release
  would strand the host on a version it has not booted; and **within
  `recencyWindow`**, the same constant `DecideBootAction` uses for
  `BootClearAbandoned`, so the refusal expires exactly when a boot stops putting
  `.bak` back (one of another version clears the marker then, and since B705
  the target's confirms it at any age) and the two cannot disagree about
  whether it still guards a restore. **Marker, not the
  in-memory `pendingRestart` flag** — the flag records THAT a swap landed and
  not WHICH version, so refusing on it blocked a legitimate newer install (caught
  by the positive control), and `bridge update` is a separate PROCESS that sees
  no `atomic.Bool` anyway. `Install` now sets `pendingRestart` for EVERY path;
  it used to be set by `maybeAutoInstall` after the fact, so the admin and CLI
  installs — the two that never restart — left it false. An unreadable marker is
  NOT a refusal: failing closed there would block every install on the host.

- **…and a job or database CLI run as root gives every entry it CREATES
  the install's owner** (2026-09-28, backlog B17). Measured the same way
  (`TestJobCLIsRunAsRootKeepTheInstallOwner`, as root with sox and ffmpeg
  on dido), 51 entries came back root's on main: every directory and file
  `scan` (the artwork cache, 0700), `upscale` / `optimize` / `render` (the
  variants tree and each rendition, `--force` ones included), `analyze`
  (the waveform tree, 0700, and each curve, 0600), `backup` (a snapshot the
  service's own prune cannot remove) and `variants move` create; the five
  files `restore` replaces (config, store, token file, TLS pair, root's
  0600: the service does not start); a store a root CLI creates; and the
  render scratch `1-bit-bridge-render`, root's 0700 in the shared temp dir,
  where every later DSD render of the service failed. **SQLite's `-wal` and
  `-shm` were never part of it**: its unix VFS gives both the database
  file's owner whenever it opens them as root (`robustFchown`, carried in
  modernc's transpile), measured with the store open; the one SQLite hole
  is a database root CREATES. **A new entry takes the owner of the
  directory it is created in**, #1048's rule for a file with nothing to
  replace. `fsutil.MkdirAll` / `Mkdir` make each directory through an
  `os.Root` on its parent and read the owner through that descriptor, so a
  directory only ever goes to the owner of the directory actually holding
  it, who could have made it anyway, never to the owner of a path looked
  up beforehand; one it cannot give away (root_squash) is removed again.
  Files the bridge stages take `fsutil.KeepOwner` (a cover, a curve, a
  snapshot's copies and manifest, a restored file). **A file another writer
  creates is precreated** (`fsutil.Precreate`: `O_EXCL`, the owner looked
  up BEFORE the file exists; a render's sidecar and scratch since B211 by
  `transcode.createOutput`, `O_EXCL` then `KeepOwner`, which creates them
  for every user so a folder that refuses them fails by type, the
  output-fault bullet under **Job pools**): sox fills its output with `O_TRUNC`, SQLite
  opens an existing empty database, `VACUUM INTO` writes into an empty
  file, and each keeps the owner (all measured). **Two directories take a
  reference's owner, because there the parent's says nothing about who uses
  them**: the render scratch, when created in a directory anyone may create
  entries in (other-writable and -searchable, as /tmp is), takes the
  variants directory's (`fsutil.MkdirAllShared`; only there, since the temp
  dir comes from the config the service user can write), and a `variants
  move --to` directory takes the owner of the variants directory it
  replaces, as `mv` keeps it (`fsutil.MkdirAllLike`; the command line names
  that path). A cross-device copy in the move keeps its source's owner.
  `TestJobWritersKeepTheInstallOwner` sweeps the writers
  (`internal/{atomicwrite,manifest,transcode,analyze,backup}`,
  `cmd/bridge/variants.go`) and fails on `os.MkdirAll` / `Mkdir` /
  `MkdirTemp` / `WriteFile` / `Create`, or on `os.CreateTemp` /
  `os.OpenFile(O_CREATE)` in a function with no `fsutil.KeepOwner`: **a
  new writer there makes its directories through fsutil and gives its files
  an owner**. What a child process creates the sweep cannot see; that is
  `Precreate`'s, and the root test pins it. **Not covered**: `bridge tsnet
  auth` (internal/tsnet makes `<dataDir>/tailscale` with `os.MkdirAll`,
  `assertSecureDir` then requires it to be the RUNNING uid's, and the tsnet
  library writes its state there: a root run over the service's state
  refuses, and one on a fresh install leaves a state the service's node
  refuses; read from the code, not measured). **Root's by the rule**: a
  configured `variantsDir` or `tempDir` a root CLI creates under a parent
  root owns that is not shared, where the service's own attempt would fail
  too.

- **No refusal, warning or log line of the config path prints a value that
  can hold a secret or text pasted where it does not belong: a password, a
  token, a serial, a URL's credentials, a value a setting refused, an entry
  of a list this build does not know, or a key; a path the operator
  configured excepted** (2026-10-22, backlog B505 rounds 2 to 5). What a
  config refuses is often something pasted onto the
  wrong line, MPD's password or a DAC's serial, and `bridge serve`, `bridge
  doctor` and init print the refusal, which a service's journal keeps.
  **A path the operator configured is outside the rule** (the orchestrator's
  decision on round 4, declining Codex's finding that a password pasted onto
  `libraryRoots` reaches `LibraryRootError`): a library root that is missing
  or a data directory init cannot make is named by its path, with the
  filesystem's error, which repeats it (`LibraryRootError`, which `bridge
  serve` prints in loopback mode and logs in public mode; init's `mkdir
  data dir`), since the path is what the operator needs to find and fix,
  and a path is not a secret the config path guards. Round 2 made
  `OutputConfig.Problem` return an `*OutputProblem`, the setting and a
  reason with no value in it, after Validate's refusal quoted a refused mpd
  (`%q`) and printed a refused port (`%d`); init's rewrite builds its
  warning lines from its parts. Round 3 took yaml.v3's own errors, which
  Load printed (the Claude API and Codex on round 2): v3.0.1 quotes a value
  it cannot decode, whole up to ten bytes and its first seven past that,
  writes a value tagged `!!map` or `!!seq` whole and unquoted, prints whole
  a value its explicit tag does not fit, names the anchor of an alias that
  names none (an unquoted password starting with `*`), and names a key it
  does not take or finds twice. `config.ValueFreeYAMLError` rebuilds each
  message from its line, the Go type, a standard tag and the decoder's own
  words; the parser's own messages (a line and a problem from its fixed
  set) stay, and any other error, a type's own unmarshaler's, is replaced.
  Load and init's read of the config it replaces print it. A key goes as a
  value does, named by its line: a scalar written alone in a flow block is
  a key, and the player block's reasons say "that key" for the same reason.
  That line is the key's own, the second key's for one written twice:
  v3.0.1's `decoder.mappingStruct` prints `ni.Line`, which is 1-based, and
  the Claude API's reading on round 3 that it prints `ni.Line+1` was
  yaml.v2's (declined on round 4, measured). Validate's refusals name the
  setting and what it takes, never what it holds: no `got %d`, no quoted
  mode, address or quiet-hours window, no effective durations, no
  `net.SplitHostPort` error, which repeats the address (`splitHostPortWhy`
  keeps its reason), a library root and an upstream server by their index;
  Load's own refusals print no path either, though the rule does not ask
  it. An unparseable `BRIDGE_*` variable's warning names the variable alone,
  never its value or strconv's error, which repeats it. An entry of
  `deployment.managedControls` this build does not know is named by its
  place in the list, by serve's startup warning and by init's refusal
  (round 4, Codex); the names it knows are a fixed set. Left as they were:
  init's `--listen-address` and `--admin-address` refusals, which name what
  the operator typed on that command line; a URL's scheme and host
  (`urlFieldForLog`, B54); and the `libraryName` and autocert host Load
  serves, which are published. **Don't print a password, a token, a serial,
  a URL's user name, password, query or fragment, a value a setting
  refused, an entry of a list this build does not know, or a key, in a
  refusal, a warning or a log line, a path the operator configured
  excepted (`LibraryRootError` and init's directory errors name the path
  the operator must find and fix); and don't print yaml.v3's error without
  `ValueFreeYAMLError`.** `TestNoLoadRefusalOrWarningCarriesAValue` (a word
  and a serial of digits on every setting `envBindings` knows, and on each
  setting Validate checks only beside another, with that one set; it holds
  Load to more than the rule, paths included),
  `TestOutputRefusalsCarryNoValue` (each yaml.v3 shape, checked to carry the
  value in yaml.v3's own error first),
  `TestNoEnvironmentOverrideWarningCarriesAValue`,
  `TestARefusalOfAKeyWrittenTwiceNamesTheSecondKeysOwnLine`,
  `TestInitRefusalOfAConfigThatDoesNotDecodeQuotesNothingOfIt`,
  `TestInitRefusalNamesAManagedControlItDoesNotKnowByItsPlace`,
  `TestServeNamesAManagedControlItDoesNotKnowByItsPlace` and
  `TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo` pin it.
