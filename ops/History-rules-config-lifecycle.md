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
