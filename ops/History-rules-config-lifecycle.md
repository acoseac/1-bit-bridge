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

## A whole-number setting takes no value yaml.v3 would change on the way in (B528, 2026-10-24)

- **A whole-number setting takes no value yaml.v3 would change on the way in: a fraction, an infinity, NaN or a float outside its range is refused as a word on that line is.** **Don't decode the config but through `config.DecodeFile`, find the setting a value reaches with a resolver of your own (the decoder maps the shadow itself), or quote the value in the refusal.** yaml.v3 v3.0.1 decodes a `!!float` into an int kind by dropping what follows the point (`decode.go`'s `scalar`: `int64(resolved)` once the float is no more than `math.MaxInt64`), and into what the platform's conversion makes of `-.inf` and 2^63 (the smallest int64; on arm64 2^63 became the largest). Measured on main before the change: 42 of the config's 44 whole-number settings outside the player block loaded `2.75` as 2, and the other two (`enrich.coverSize`, `retention.playbackHistoryDays`) refused it only because 2 is outside what they take, so a fraction inside their range loaded the same way; `limits.manifest.requestsPerMinute: 0.5` loaded as 0, which turns the manifest limit off, and so did `-.inf`, as the smallest int64; in the player block, `openhomePort: 7792.9` bound 7792 and `pauseReleaseSeconds: 2.5` held the DAC for 2 s. A refusal follows its neighbours: Load refuses the file, as it refuses a word on that line, with the words `ValueFreeYAMLError` gives the out-of-range float yaml.v3 refuses itself, `line N: cannot unmarshal a !!float value into int` (the line, the tag and the Go type, nothing of the value); the player block's reader leaves the block unread, the player off with `output.player.<setting> (line N): must be a whole number`, and the load goes on. A float whose value the integer holds exactly is taken as written, `30.0`, `3e1`, `-0.0`, `!!float 6`, and `08`, which yaml.v3 resolves as a float since `08` is no octal: refusing every `!!float` was rejected, since it would have stopped files loading over values that lose nothing (`08`, or `21600.0` from a tool that writes its numbers as floats). `exactInteger` is the one test: anything but a `!!float` passes (an integer, a null, or a value the decoder refuses itself), and a `!!float` passes when it is whole and inside the integer's range, compared as powers of two (`math.Ldexp`), which float64 holds exactly; a round trip through the integer was rejected, since arm64 saturates 2^63 to the largest int64, which converts back to 2^63.

  Load's decode is `config.DecodeFile`: yaml.v3's decoder with `KnownFields` (the typo catcher), and, once that decode succeeds, `checkWholeNumbers` over the same bytes. It decodes the file a second time into a shadow of the type (`shadowOf`): each integer field replaced by an `integerCheck` of its type, whose `UnmarshalYAML` decodes nothing and answers a `*yaml.TypeError`, which yaml.v3 adds to its own and goes on past, so every such value is named; every struct it rebuilds keeps its fields' names and tags, and every type that holds no integer is kept as it is, so yaml.v3 itself maps the document onto the shadow as onto `Config` and decodes the same nodes in the same order: the same keys, merge keys, aliases (named at the line of the node an alias names, once for each setting it reaches), inline fields and encoded keys, and the same count of aliases against its limit. A resolver of the config's own was rejected for the reason B505's round 2 records under the rewrite's rule (cli-serve.md): its copy of the decoder's merge and key rules disagreed with the decoder three ways. A type that reads its own YAML (`yaml.Unmarshaler`, the older form, `encoding.TextUnmarshaler`, `yaml.Node`) is not looked into, so `PlayerConfig` reads its own block; `time.Duration` decodes from text alone, and is no whole number there; a map's keys are not looked at, since no map in the config is keyed by a number. `ValueFreeYAMLError` keeps a line already without its value as it is (`valueFreeTagged`), since yaml.v3's own always start with the tag, and the check's lines pass through it. bridge init's rewrite reads the file through `DecodeFile` (`outputAsLoadDecodes`), so a file holding a fraction anywhere decodes whole there no more than in Load, and asks `CheckWholeNumbers` of the block it decodes on its own and of each setting it recovers (`outputAsLoaded`, `outputRecovered`): until then Load took a file holding `loopbackPort: 7795.5`, and the rewrite kept the 7795 it read. Out of scope, each checked: the `BRIDGE_*` overrides parse with `strconv.ParseInt`, which refuses a fraction (warned and ignored); the console's settings PATCH decodes JSON into `*int`, which `encoding/json` refuses with a fraction in it (400); and yaml.v3's YAML 1.1 octal, `loopbackPort: 010` loading as 8, a change of another kind. A config that holds a fraction stops loading after this change, where it loaded its whole part: serve, doctor and init name its line. `TestAWholeNumberSettingTakesNoFraction` (every whole-number setting `envBindings` knows outside the player: a fraction refused, value free, at its line, with its Go type; an integer it takes and that integer as a whole float loading as written; null loading as left out), `TestAWholeNumberSettingTakesWhatDecodesUnchanged` (the edges on `limits.manifest.requestsPerMinute`, a merge key, an alias, three fractions in one file), `TestAWholeNumberOfThePlayerTakesNoFraction`, `TestAnOpenHomeSettingIsValidatedLikeItsNeighbours`, and init's rows in `TestInitRewriteDropsAnOutputSettingThatDoesNotReadAndSaysSo`, `TestInitRewriteResolvesMergesBesideASettingItCannotKeep` and `TestPriorOutputOfAFileTheDecoderRefusesElsewhere` pin it.
