## 2026-11-21 — Bridge lows 16: a lock file this user cannot open (B537 item 2), an off behind another process's swap on the bridge's Windows service (B831), and a release put back by hand (B708)

Branch `fix/bridge-lows16`, from main `ebfdba20` (#1542's merge, one past the brief's `af8f9a72`;
#1542 touches none of the files this bundle cites, so the brief's lines are the base's). Seven
commits, this record the last:
- `2afac607`, Part A red: its tests, and a names-only shim (`config.ErrLockInaccessible` and
  `*config.LockInaccessibleError` declared, never returned);
- `d504c677`, Part A: the refusal, the console's class, the B506 line and its long form, the
  console's save-config line;
- `0c1c454d`, Part C red: its tests, and a names-only shim (`updater.ChecksOffWait` and
  `LockForChecksOff`'s new parameter, ignored; `ErrChecksOffBehindAnInstall`, never returned;
  `Updater.Armed`, false; `noteArmed` and `NoteArmedForTest`, no-ops; serve's two seams, wired to the
  ignored parameter);
- `d6c90812`, Part C: the refusal, the updater's record of its own markers, the seven texts, the B761
  line and its long form, the console's updates line, cli-serve's long form;
- `1ada853c`, Part D: the runbook's line and B705's long form;
- `c2bb80b4`, Part C: a waits case's log assertion, which a control showed could not fail;
- this record.

Every Go command here ran with `GOTOOLCHAIN=go1.27.3`, the release tags (`-tags
ts_omit_webclient,ts_omit_debugeventbus`) and `TMPDIR` on disk in a scratch folder below the
checkout's `.tmp/`. Each control was predicted in writing before it ran and is an overlay (`go test
-overlay`, `-vet=off`) of HEAD's files, nothing tracked written, `git status` clean after each; the
one control whose test reads a text from disk changed it in place and put its bytes back from memory.
No item touches a qualification-guarded path (`internal/player/qualification/version_gate.go`), and
`qualificationVersion` stays 56 (below).

### The premises, at the base

Each was read again by line at `ebfdba20` and holds as the brief states it. Part A: `lockFile`'s open
through `fsutil.OpenLockFileLike` and its one answer (`internal/config/lock.go:302-305`),
`linkIntoReadOnly`'s remedy for a link alone (`:595-609`), a root writer's lock file given the
config's owner (`:233-237`) and one already there opened as it is (`internal/fsutil/lock.go:143-166`),
the console's "the log says why" (`internal/admin/handlers_api.go:120`, `saveConfigRefusal` at
`:137-156`), the link case beside which the new one sits (`lock_link_unix_test.go:230-233`) and the
directory at the lock's name (`runtime_test.go:99-103`). Part C: `bridge update`'s hold of the
marker's lock from before its decision through its swap (`internal/updater/install.go:495-504`), the
marker armed before the swap (`:528-536`, the swap at `:554`); the swap's SCM stop
(`swap_windows.go:65`, `:159-221`: none where the process swapping is the service, none without the
SCM, none for a service not registered or not running); the SCM's stop cancelling serve
(`cmd/bridge/service_windows.go:113-116`) and every console request with it
(`internal/admin/admin.go:2284`, `checksoff.go:92-93`); the off's wait (`LockForChecksOff`,
`checksoff.go:104-118`, in `LockState`'s retries, `statelock.go:201-251`) and its answers
(`handlers_api.go:4120-4151`); `isWindowsService` (`service_windows.go:26-32`); the marker's
writes as renames (`state.go:344-415`) and `LoadState`'s Warn (`:310`); `installInFlight` and
`swapPending` (`updater.go:302`, `:337`). Part D: the record made only over an armed install
(`configcheck.go:512-529`), kept by every clear at boot while `.bak` may stand (`state.go:444-472`)
and refused on (`:673-692`); the store's version (`internal/manifest/ladder_schema.go:18-21`), the
backup's (`configcheck.go:134-160`), the crash rollback's probe before its lock
(`statelock.go:38-42`) and the abandoned clear (`state.go:509-514`); the runbook's B705 paragraph
(`ops/deployment-runbook.md:852-877`) and B705's long form (`ops/History-rules-config-lifecycle.md:2630-2637`).
The root agent-instruction file names none of B831, B537, B708 or `save-config`.

### Part A: a lock file this user cannot open (B537 item 2)

`lockFile` asks, of an open of the config's lock that fails (`lockInaccessible`), whether the open
was refused for a permission (`errors.Is(err, fs.ErrPermission)`) over a regular file at the lock's
name, by an `os.Lstat` that follows no link; then it refuses with `*config.LockInaccessibleError`,
matching `config.ErrLockInaccessible`, its cause the open's error, in words that name the config file
and the lock file and give the two ways out: give the lock file the config file's owner and let that
owner read and write it, or remove it while the bridge is stopped and no bridge command runs, since
only the lock taken on it means anything and the next writer makes it again. The brief's review asked
the words to cover a mode taken away beside another owner, since the open asks to read and write
whoever owns the file. The refusal comes before `linkIntoReadOnly`'s remedy, which stays for a lock
file that is not there; every other failure keeps its words: no lock file in a directory this user
cannot write, anything but a regular file at the lock's name, and an Lstat that answers an error of
its own. Nothing is chowned or removed, and no value of the config is in the words. The console
answers the class in words of its own that name no path (`errMsgSaveConfigLockInaccessible`), 500
`save-config` as every other class.

Tests: `TestALockFileThisUserCannotOpenSaysWhatToDo` (unix, skipped as root): `config.Edit` and the
holder's `Update` each refused with the sentinel, both names and both ways out, nothing written and
the holder's change not run; the mode given back, and the file removed, each let the next save land;
a directory at the lock's name, and no lock file in a directory this user cannot write, keep "open the
lock beside <config>", the second its permission error too. Mode 0 stands for another user's file: a
test that is not root can give a file no other owner, a test run as root opens any file, and the
refusal reads the open's answer and the entry's kind, never its owner. The console's
`TestAConfigSaveRefusalSaysWhatToDoAndNamesNoPath` row and
`TestAConsoleSaveBesideALockFileItCannotOpenSaysWhatToDo`, the settings PATCH through the real holder,
whose own save had made the lock file already (so the test takes its mode away by `os.Chmod`, as
`WriteFile`'s mode applies to a file it makes alone; the first draft's case went through for that, and
a premise now opens the file first). Red at `2afac607`: both writers' cases ("open the lock beside …:
openat .bridge.yaml.lock: permission denied, want ErrLockInaccessible"), the row and the end-to-end
case ("the bridge could not save the config file, and the bridge's log says why"); the two
keep-their-words cases passed.

### B537 item 3, not taken

The orchestrator's decision of 2026-11-21: the console keeps answering 500 `save-config` for a busy
lock and for a file changed outside the holder. Its reason, with what every known client does with a
409 or a 503, is in B506's long form; the tests that pin 500 are as they were.

### Part C: the off behind another process's install, on the bridge's Windows service (B831)

The decision is the brief's: refuse the off at once, never persist it. Persisting it would put a new
durable request in the data dir, which a later start, possibly of another version, would have to
apply before its first check, under the marker's lock and the probation's decision, and drop once
stale (a rollback to a release that does not know it, a hand edit of the key); every other off
refusal saves nothing and says to save again.

`LockForChecksOff(ctx, dataDir, w)` waits through `lockStateWhileHeld`, which asks `w`
(`ChecksOffWait.anotherInstall`) at each held answer of the lock, the first and every retry, never
once the lock is taken: where `w.Service` (serve is the bridge's Windows service) and the install
marker, read without the lock and quietly (`markerAsSeen`: its writes are renames, one that is not
there, does not parse or cannot be read is no install, and no Warn is logged at a retry), is an install
of a version other than `w.Current` (`SameVersion`) that `w.Updater` did not arm (`Updater.Armed`),
the off is refused at once: a `*ChecksOffLockError` whose cause matches
`ErrChecksOffBehindAnInstall`, which the console answers 409 `marker-locked` in its words: "update
checks were not turned off: an install of <target> by another process holds the install marker's
lock and is replacing the bridge's binary, which can stop this service and this save with it, so
nothing was saved; once the bridge answers, turn the checks off again if they are on". A stop observed
before the refusal is decided, or while it is, keeps ctx's error, which the console answers 503
`stopping` in words that claim no cause (the brief's review, L6). `LockState`'s other callers pass no
question and are unchanged; the zero `ChecksOffWait` waits as before.

The updater tells its own markers by their identity, the target and the time each arming wrote, never
by a flag (the brief's review, L4: `installInFlight` covers a download, during which another process
may install, and `swapPending` says a swap landed and not which): Install records its marker
(`noteArmed`) just before it writes it, so no read finds it written and not yet the updater's, and the
auto-installer's restamp of a marker of its own records the new time. Serve builds the wait beside its
updater (`checksOffWait`: `serveOpts.asWindowsService`, which is `isWindowsService` but where a boot
test stands in for the service through `serveOpts.windowsService`; `version.ServerVersion`; `upd`,
which `serveOpts.updaterBuilt` hands a boot test).

What it leaves, the orchestrator's decisions: a swap run without the rights to stop the service, or
finding none to stop, stops nothing, and an off it refused would have landed after it (a false
refusal, answered by turning the checks off again); a stop that lands within one retry (50 ms) of the
marker's arming keeps today's 503; and a marker another process armed and left (a `bridge update`
killed before its swap, a swap that stopped nothing awaiting its restart) refuses, beside any other
holder of the lock (a migrating store open, a start's housekeeping, this process's own install or
rollback before it writes its marker), an off that would have landed, until a start resolves it.

Tests, red first (the brief's review, L5: a held lock already ends in 409 `marker-locked` after the
lock's 30 s wait, so each refused case requires the cause and the target's words, and counts the
lock's held answers through `SetStateTryLockForTest`, so the refusal must come at the first answer
after the marker was there): internal/updater (no build tag, so CI's `test (windows-latest)` leg runs
them) `TestAnOffBehindAnotherProcesssInstallOnTheWindowsServiceIsRefusedAtOnce` (the marker armed
before the first try, refused at held answer 1; armed between the first try and the first retry,
which the try seam holds, refused at answer 2), `TestAnOffBehindTheInstallMarkersLockWaitsWhereItIsNoOtherProcesssInstall`
(not the service, an install of the running version, one confirmed, a rollback's record, a marker that
does not parse, none, an install this updater armed: still waiting at the third held answer, then the
lock taken once it is let go), `TestAStopBeforeTheOffsRefusalKeepsItsError` (a request stopped before
it reads no marker; a stop during the marker's read answers the stop),
`TestARestampKeepsWhatThisUpdaterArmed`, and `TestAnOffBehindAnotherProcesssInstallIsRefusedWhateverThisProcessInstalls`
over a real Install (an archive of this OS's form, a held download, a swap stand-in): another process's
install during this process's download refused at answer 1, and after this process's swap, which
waits for its restart, a hold over its own marker still waiting at answer 3 and refused at answer 4,
once another process armed an install of another target. cmd/bridge, in the shape of
`TestTheConsolesOffWaitsForTheInstallMarkersLockAndLandsAfter`, the marker armed once serve is up and
while the test holds the lock: `TestTheConsolesOffBehindAnotherProcesssInstallOnTheWindowsServiceIsRefusedAtOnce`
(409 at held answer 1, the words, the file and the switch as they were),
`TestTheConsolesOffBehindTheInstallMarkersLockWaitsWhereItIsNoOtherProcesssInstall` (without the stand-in
over another process's install; with it over a swap of serve's own, the marker serve's updater was
told was its own, `NoteArmedForTest`, since a real install there would download from GitHub's hosts
alone), `TestAStoppedOffBehindAnotherProcesssInstallIsAStop` and `TestATestProcessIsNoWindowsService`
(the real check answers false in a test process; on Windows it is CI's leg that runs it). Every wait is
bounded from `t.Deadline()` with a reserve, none is a sleep, and the tests' holds are let go in
`t.Cleanup`. Red at `0c1c454d`, each refused case in the lock's ordinary refusal ("another process
held the install marker's lock for 30s", and "refused after 594 held answers, want 1" in cmd/bridge),
the stop during the read, the restamp's own case and the seven texts; the waiting cases passed there,
as they must after the fix.

The seven texts that named B831 as open, README, PROTOCOL.md's paragraph, the Settings hint,
`updateChecksOffLost`'s comment, the docs of `writeUpdateChecksOffRefusal` and `LockForChecksOff` and
the settings matrix's row, say that the bridge's Windows service refuses such an off at once.
`TestEveryTextSaysTheWindowsServiceRefusesAnOffBehindAnotherProcesssSwap` replaces the words test
that held them to B831 (`…ASwapThatStopsTheWindowsServiceIsNamedOnlyAsB831`, which only the frozen
archive cites): each text says it of the bridge's Windows service, none of Windows as a whole, none
names B831 as open; the 503's and `updateChecksOffLost`'s words still claim no cause. PROTOCOL.md changes in prose
only, so the app's mirror copy, and its test's pin of the file's digest, follow in a docs-only PR of
the app's.

### Part D: a release from before B705 put back by hand (B708)

No code; the runbook's line and B705's long form, its rule's line unchanged (the brief's review, L2).
Each point was verified. The field and B705 first shipped together in 0.2.3: the archive records
v0.2.3's tag as `0ddb8c72` (`ops/engineering-log.md:90390`), of which B613's `e362a21f`, the field's,
and B705's `ffb4b3c1` are ancestors, while `rc/v0.2.2` holds neither; 0.2.2's `DecideBootAction`
reads the window before the version, and its `State` has no `DataDirOpenedBy` (its `state.go`). The
store records no version, only `PRAGMA user_version`, and a build knows only its ladder's top
(`LadderTop`). A backup can say its version (`ProbeVersion`), but a record made from it at a start
with no install armed would be a new writer at serve's admission, which the crash rollback keeps short
by asking its backup before it takes the lock, and it would refuse the Roll back that the abandoned
clear leaves to the operator on purpose; which rollbacks run is the owner's. An install records again
at its first start, so the case needs a release from before B705 put back by hand and then a newer
one started by hand. The runbook says, after its B705 paragraph, what such a release does to the
record, to remove `.bak` or reach the newer version with `bridge update` first, and where to read the
record.

### Negative controls

Predicted in writing before they ran; each turned exactly the named cases red, the rest green:
- A1, `lockInaccessible` answering nil: both writers' cases and the end-to-end console case; the
  table row (a built error) and the keep-their-words cases green.
- A2, its Lstat check dropped: "no lock file in a directory this user cannot write" alone.
- A3, the console's class dropped: the row and the end-to-end case.
- C1, the service check dropped: the waits cases "not the Windows service" (updater) and "another
  process's install, not the Windows service" (cmd/bridge).
- C2, `Armed` false: the own-marker waits case, the restamp's own case, the after-swap case (refused at
  answer 1 naming this process's 0.9.0), and cmd/bridge's own-swap case; the during-download case
  green.
- C3, `Armed` by the flags (`installInFlight || swapPending`): both real-Install cases (the off waited
  out the lock's 30 s), the own-marker waits case, the restamp's own case, cmd/bridge's own-swap case.
- C4, the first stop check dropped: "a request stopped before it" (the marker read once, want 0).
- C5, the second dropped: "a stop during the marker's inspection" (the refusal, not the stop).
- C6, the quiet read replaced by `LoadState`: "a marker that does not parse" (its Warn at every
  retry). It first stayed green: the case asked `Lines`, which matches a message exactly, for
  "malformed update-state.json", where LoadState's message runs on; `c2bb80b4` asks `Carrying`.
- C7, the version check dropped: "an install of the running version".
- C8, the status check dropped: "an install confirmed" and "a rollback's record".
- C9, the cause not wrapped in `*ChecksOffLockError`: both updater refused cases, and cmd/bridge's
  answered 500 `save-config`.
- C10, the words' branch dropped: the words of the refused cases, in both packages.
- C11, serve's wait without its updater: cmd/bridge's own-swap case alone.
- C12, serve's wait from `isWindowsService()` alone: cmd/bridge's refused case (593 held answers, the
  ordinary refusal); the waits cases green.
- W1, README's sentence on B831 put back: the words test, for README alone.
C11 and C12 first ran no test, the runner's `-run` pattern cut a letter from each end; rerun with it
right, they turned red as predicted.

### Runs

On linux/amd64, before this record, at `c2bb80b4`:
- `-race -count=20`, four lanes at once outside the build slot: `internal/config` whole (355 tests,
  302 s); `internal/updater`'s off, lock and restamp tests (52 tests, `-run
  'Off|Behind|Restamp|RefreshesInstallRecency|LockState|StateLock|MarkersLock|InstallMarker'`, 260 s);
  `internal/admin`'s save-refusal and update-switch tests (18 tests, 437 s); cmd/bridge's console-off
  tests (11 tests, `-run` the alternation of `TheConsolesOff`, `TheOffsAsk`, `StoppedOff`,
  `CrashRollbackWithTheVariableOff` and the no-service check's name, 417 s): all ok.
- one session in the shared build slot: `make fmt` (nothing rewritten), `make
  vet`, `make docs-check`, `go vet ./...` for windows and darwin, `TestEveryCitedTestNameExists`, the
  qualification check (`go run ./scripts/qualification-version -base ebfdba20 -head HEAD`: "qualification
  version OK", no raise), `make test P=8 TEST_PKGS='-count=1 -timeout 45m ./...'` (92 packages ok in
  1,778 s; cmd/bridge 1,762 s, manifest 1,653 s, admin 1,292 s, backup 711 s; the 45 m per package
  because cmd/bridge runs within a minute of the Makefile's 30 m here), and `make build-all` (34 s):
  each exit 0, `git status` clean after.
CI's `test (windows-latest)` leg, Part C's Windows case, runs on the PR; it was not run here.
