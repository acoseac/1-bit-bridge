## 2026-11-20 — B951: the boot rollback asks the stop again once its backup has answered and says a window that ended during the ask; a failed clear's words; the crash rollback's records

Branch `fix/b951-rollback-lows`, from main `6ee6102a` (#1525's merge, past `dd688b15`, the brief's
floor). Six commits:
- `6aa9dd6d`: the cases of items (a) and (b), red, with their two seams;
- `cb22c22c`: their fix;
- `8e0360bf`: item (c), its words and its case;
- `77418c94`: the records, items (d) to (i);
- `273fa3bc`: the stop case's drain, which the gate at `77418c94` refused;
- this record.

The items are Lows of B898FU's rounds 3 and 4 (#1518) left for a follow-up, and two Lows of the
records' later reviews ((h) and (i)). Lines cited are `6ee6102a`'s unless a commit is named. Each
red and control was predicted in writing before it ran. Every command below ran from the
checkout's root with `GOTOOLCHAIN=go1.27.3` and the release tags (`-tags
ts_omit_webclient,ts_omit_debugeventbus`, `Makefile:9`), each `go test` with `-timeout 30m` and
`TMPDIR` on disk in a scratch folder below the checkout's `.tmp/`, where the line says nothing else.

### The premises, at `6ee6102a`

Each held, so none was skipped:
- (a) `bootHousekeepingWith` asked serve's stop only inside `if why != "" || unanswered != nil`
  (`cmd/bridge/main.go:2734-2738`). A backup that said the install's `PreviousVersion` went on to
  the veto's `SaveState` (2763) and the restore (2771). The probe's last look at the context is
  `runCandidate`'s, after the run and before its answer is read
  (`internal/updater/configcheck.go:222`), so a stop that landed after it was seen nowhere on that
  path. `runServe` acts only on `updater.ErrBinaryMissing` from `maybeRollbackOnBoot`
  (`main.go:4604`); its own select sees a stop (`main.go:7378`).
- (b) The line for a backup that did not say its version (`main.go:2740-2745`) named the window's
  end and promised a later ask, on the clock the decision read before the ask (2666). Past the
  window a fresh updater's `swapAwaitingRestart` refuses no install (`internal/updater/install.go:969`),
  and the next boot of another version clears the install as abandoned without asking the backup
  (`internal/updater/state.go:608`).
- (c) `clearInspectionRefusal` said, for any error of its save, that the marker still recorded
  the refusal (`cmd/bridge/serve_crashed_install.go:437`), while `SaveState` publishes the marker
  by its rename (`state.go:389`) before its directory's barrier (`state.go:411`).
- (d) B894's rule line (`.claude/rules/config-lifecycle.md:635`) said "only a start that answers
  as ready and stays up `ProbationWindow` ends it". Its long form names two more ends
  (`ops/History-rules-config-lifecycle.md:4104-4106`), and `ProbationHolds` holds nothing beside
  any recorded refusal (`internal/updater/probation.go:453`).
- (e) Round 4's room commit, `01bd7af0`, left `config-lifecycle.md:150` its title alone. The block
  sat last in the History (4510-4538), under the section of B190's container bullet, its "(the
  next bullet)" naming nothing there.
- (f) B898FU's record said round 4's red was the next push's to run on CI (525-527) and left open
  whether a reader had made the old read fail (555-557); the History said the same (4062-4065);
  the rule line and its long form listed the reader's case among the pins; and the record's
  bound line said "(this commit)" (455) of a section whose commit changed the record alone.
- (g) The History said why the two opens by path are not `fsutil.OpenAsFile`'s (4065-4070), not
  that round 4's brief had asked for them.
- (h) Lows 8's record, as Bridge lows 10 corrected it, called Lows 10's runs reruns of its own
  (19-21 and 65, and 79, 124, 214 and 229-233 alike).
- (i) The History's crash-rollback bullet (3877) gave `fsutil.EntryIdentity` as "an `os.Lstat`
  whose identity is read as it is taken". That is its POSIX path alone
  (`internal/fsutil/entry_identity_other.go:12-14`). On Windows it opens the entry itself
  (`entry_identity_windows.go:15-27`: `CreateFile`, no access asked, every kind of sharing,
  `FILE_FLAG_OPEN_REPARSE_POINT` and `FILE_FLAG_BACKUP_SEMANTICS`), reads its type and identity
  from that handle, and closes it before it returns (31-38).

### (a) A stop once the backup has answered

The seam. `bootSeams.answered`, nil in production, hears what `backupNotPrevious` answered as it
returns, before anything is decided on it. It lives on the per-call `bootSeams`, and the case
drives `bootHousekeepingWith` itself.

The case. `TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered` asks the backup, a
copy of the test binary that answers as `0.0.0-previous`, through the production probe. It parks
the housekeeping where the answer is heard, requires the answer to be the install's
`PreviousVersion` (its premise), stops serve's context, and lets it go. It wants the stop's error,
the binary, the backup and the marker byte for byte, the restore never called (a counting wrapper
of `updater.RollbackBinary`), and the stop's line alone. Its doc says that `runServe` acts on that
answer no further, its own select seeing the stop.

Red first, at `6aa9dd6d`, as predicted: `serve_held_binary_test.go:895` ("the housekeeping =
<nil>, want the stop's error", the housekeeping having said "rolling back to .bak"), `:897` three
times (`bridge` changed; `bridge.bak` gone; `update-state.json` changed), `:899` ("the backup was
put back 1 time(s), want never") and `:903` (no stop line).

The fix, `cb22c22c`. The stop is asked once the backup has answered, whatever it answered,
before anything is written, in one look that replaces the look the refusal's branch made. Its
line says "updater: stopped as the backup said its version; the install is left as it is, for a
later start", and the row of a stop during the probe in
`TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion` takes "as" for "while". What
is left is the moment between that look and the veto's write, which holds no wait. No decision
of the rollback changed.

The drain, `273fa3bc`. The gate at `77418c94` failed on `TestEveryBackgroundGoroutineDrainsOnCleanup`
alone: the case launched its housekeeping as `go func(){ defer close(ended); … }()` and waited for
it in a cleanup of its own, where the guard asks the launching function for `drainLoopOnCleanup`
or `drainServeOnCleanup`. The case now registers `drainLoopOnCleanup`, and the park's release after
it, so a case that fails while the housekeeping is parked lets it go before the drain cancels it
and waits. Controls D1 and D2 below show the order. The case's assertions kept their lines.

### (b) The window, asked again after the ask

The seam. `bootSeams.now`, nil in production (`time.Now`), is the clock both decisions read.

The case. `TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt` decides a minute
before the window's end, its probe stand-in moving the clock a second past the end and answering
"it did not answer within 2m0s". It wants the whole line for a window that ended, nothing written
or renamed (the marker kept by this call), the backup asked once and never put back. A next start
then runs, its clock past the window and its probe counting: the backup asked nothing, the
install cleared, the binary and the backup unchanged.

Red first, at `6aa9dd6d`, as predicted, at `:947` alone: the line within the window, "… the
install's marker is kept, for the next start to ask again; until the backup says its version or
the window ends at 2026-11-20T09:04:05Z, an install of 9.9.9 is refused as waiting for its
restart, and a restart only asks the backup again".

The fix, `cb22c22c`. The branch asks `updater.DecideBootAction` again on the clock as it reads
once the ask has ended. Where that is `BootClearAbandoned` the line is: "updater: install of
<target> did not reach the expected version (running <current>); not rolling back to
<binary>.bak at this start, since the backup did not say its version: <why>; the install's marker
is kept by this start, but its window ended at <end> while the backup was asked: the next start
of <current> clears the install as abandoned, without asking the backup, and leaves the backup
where it is". The marker is kept as before.

### (c) A failed clear's words

`8e0360bf`. `clearInspectionRefusal`'s line for a save that answered an error says "updater: the
clear of the install marker's refusal recorded for a backup that could not be inspected was
attempted, and its durability is unconfirmed: <error>". `backupRuns` gains the marker's write,
`serveOpts.saveUpdateState`, which the clear takes. No test asserted the old words.

`TestAClearOfAnInspectionRefusalWhoseWriteFailsIsSaidUnconfirmed` lays the refusal a lookup's
failure was recorded in until B898FU's round 3 and fails the identity's first read. Its two rows:
- a save through `updater.SaveState` whose barrier's sync fails once
  (`updater.SetMarkerBarrierForTest`, armed in the save): the clear has landed, the refusal
  cleared and nothing else of the marker changed;
- a save that fails before any rename: the marker is unchanged.

Each row wants the line with the save's own error, and no "still records". Nothing is put back.

### (d) to (i): the records, `77418c94`

- (d) B894's rule line and its long form give round 4's bound whole. The hold is per process and
  renewed at each start of a crash loop, and it does not hold beside a refusal recorded before
  (one recorded as a backup that changed among them, which no start clears for its words). The
  confirmation ends it, as do a migration of the store begun and a put-back once a start's reads
  succeed; a manual install is never held. *The hold's bound* says the refusal's exception, and
  that the line said "only" until now.
- (e) #1046's rule is back in its line: the cap of 256 runes, refusing what is typed and
  repairing what is stored, and its three Don'ts. Its long form stands under a heading of its
  own, its pointer to B744 names B744's entry, and a closing paragraph says why the rule came
  back.
- (f) The reader's case leaves B894's pins in both lists, and the History's paragraph calls it a
  guard. CI run 44590032503, of `2534dc60` pushed alone, was red on
  `TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange` alone, as round 4's
  review reports it. That run's log was not to hand for this record, so its legs and its lines
  are not given. B898FU's record is corrected in place: a note at its head, the run at 525 and
  555, the bound's commit at 455 (and the rule line's "only" beside it), and what the room
  commit moved, with the rules file's size then (97,705 bytes, 98,897 at `37f06665`).
- (g) The History's paragraph on the identity says that round 4's brief asked for both opens
  through `fsutil.OpenAsFile`'s handle, a requirement mis-specified. Through that handle
  `TestEntryIdentitysHandleRefusesNoRenameOrRemoval` (whose premise is that handle refusing the
  rename) and `TestEntryIdentityReadsAJunctionAsNoRegularFile` (the junction followed to its
  directory) would fail, and so would the symbolic link's cases. So the shipped helper opens a
  handle of its own.
- (h) Lows 8's record says, at each of its six places, that Bridge lows 10's runs are its own, at
  `cc453212`, where its lanes' words select 69 tests (58 at `b332bb6c` and 11 of #1518's) and
  its P1 went red on `TestTheCrashRollbackTellsABackupItCannotOpenFromNone` too. The README's rule
  for a per-entry file is followed: corrected in place.
- (i) The History's bullet names both of `EntryIdentity`'s paths. The rule's ban on a bare Lstat
  stands as it was.
- (a) to (c): B898's rule line gains the stop's Don't ("act at boot on what the backup answered
  once serve's stop has come") and the stop's case among its pins. Its long form's *The starts.*
  says the stop asked again and the line for a window that ended. B894's long form says the
  clear's words.

`config-lifecycle.md` was 99,112 bytes at the base and is 99,734 at `77418c94`, under its
100,000. `make docs-check` passed over the edits, and so did cmd/bridge's three doc guards.

### Controls

Each was predicted in writing before it ran. A1 to C1 ran at `8e0360bf`, laid over the
committed file by `go test -overlay` (no tracked file written), each over the 13 tests the
mutations can reach:
`go test -overlay <json> -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -v -run '^(TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered|TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt|TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion|TestTheBootRollbackKeepsItsMarkerWhereTheBackupCannotBeInspected|TestTheBootRollbackKeepsItsMarkerWhereTheBinaryWasInUse|TestTheBootKeepsARecordedHoldWhereItClearsAnInstall|TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir|TestAClearOfAnInspectionRefusalWhoseWriteFailsIsSaidUnconfirmed|TestTheCrashRollbackTellsABackupItCannotInspectFromNone|TestTheCrashRollbackTellsABackupItCannotOpenFromNone|TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange|TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange|TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect)$' ./cmd/bridge/`.
G1 and G2 ran at `77418c94`, written on disk and restored from memory, each over
`-run '^TestEveryCitedTestNameExists$'`. D1 and D2 ran at `273fa3bc` through the overlay, the stop
case alone, `-run '^TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered$'` with
`-timeout 60s`. The tree was clean after each.

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| A1 | the stop's look back inside the refusal's branch, as at the base | the stop case red at `:895`, `:897` three times, `:899` and `:903`; the row of a stop during the probe green; the other 12 green | as predicted |
| A2 | the stop's look before the answer is heard | as A1 | as predicted |
| B1 | the window not asked again (the branch's test against `BootNoop`) | the window case red at `:947` alone; the other 12 green | as predicted |
| C1 | the old warning back | the clear's case red in both rows at `serve_crash_rollback_lock_test.go:689` alone; the other 12 green | as predicted |
| G1 | B898's new pin in the rule line suffixed `Ghost` | the guard red, naming it "(cited by .claude/rules/config-lifecycle.md)" alone | as predicted |
| G2 | the History's citation of the clear's case suffixed `Ghost` | the guard red, naming it "(cited by ops/History-rules-config-lifecycle.md)" alone | as predicted |
| D1 | the stop case's premise inverted, so it fails while its housekeeping is parked | red at `:885` alone, in under a second, no drain message | as predicted, 0.07 s |
| D2 | D1, and the park's release registered before the drain | red at `:885`, then the drain's give-up ("the boot's housekeeping did not exit on ctx cancel before the test's deadline", `background_drain_test.go:121`) about 50 s in: the 60 s deadline less `serveDrainReserve` | as predicted, 50.05 s |

### Runs

Two build-slot sessions of one script, each step's command line and answer logged, the slot free
at once both times.

The touched tests, the 41 each session ran `-race -count=20`: every test of cmd/bridge that calls
the boot's housekeeping (`bootHousekeeping`, `bootHousekeepingWith`, `maybeRollbackOnBoot`), and
every test of `serve_crash_rollback_lock_test.go`, whose rig and clear changed:
`^(TestTheBootRollbackKeepsItsMarkerWhereTheBinaryWasInUse|TestTheBootRollbackLeavesABackupThatIsNotThePreviousVersion|TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion|TestTheBootRollbackKeepsItsMarkerWhereTheBackupCannotBeInspected|TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered|TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt|TestEveryRollbackAtAStartFitsInsideTheMarkersLockWait|TestAFailedRemovalOfTheBackupLeavesTheInstallForTheNextStart|TestAStartOfAnotherVersionKeepsTheRecordOfALateStartThatDied|TestACrashAfterTheFirstBootKeepsTheBackup|TestThreeFailedStartsRollBackAnInstallThatMigratedNothing|TestServeMarkerDiagnosticsCarryNoPaths|TestALaterCommandOfTheVersionRolledBackRefusesToMigrateOnceThePreviousVersionBoots|TestAMigrationThatHoldsTheMarkersLockFirstStopsTheBootRollback|TestABootRollbackThatHoldsTheMarkersLockFirstStopsTheMigration|TestTheBootLeavesTheInstallWhereItCannotHoldTheMarkersLock|TestAStopWhileServeWaitsForTheMarkersLockEndsTheStartThere|TestTheBootConfirmsAnInterruptedRollbackOnlyOnTheInstallsOwnRecord|TestTheBootKeepsARecordedHoldWhereItClearsAnInstall|TestTheBootRollbackLeavesABackupOnceAnotherVersionOpenedTheDataDir|TestAManualRollbackStoppedAfterItsRenameIsFinishedByThePreviousVersionsBoot|TestAManualRollbackThatHoldsTheMarkersLockFirstLeavesTheBootNothingToDo|TestABootThatHoldsTheMarkersLockFirstLeavesTheManualRollbackNothingToPutBack|TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock|TestTheCrashRollbackTellsABackupItCannotInspectFromNone|TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect|TestTheCrashRollbackTellsABackupItCannotOpenFromNone|TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange|TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange|TestAClearOfAnInspectionRefusalWhoseWriteFailsIsSaidUnconfirmed|TestTheCrashRollbackPutsBackABackupAnotherHandleReads|TestTheCrashRollbackTellsALinkAtItsBackupFromABackupItCannotInspect|TestTheCrashRollbackPutsBackABackupRecordedAsChangedOnceItsIdentityReads|TestABackupChangedWhileItWasCheckedIsNotPutBack|TestTheBackupIsNotPutBackOverSettingsItDidNotCheck|TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock|TestAStopAroundTheBackupsChecksWritesNothing|TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked|TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion|TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld|TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle)$`.

Each session, in order:
1. `GOOS=windows go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./cmd/bridge/` and the same
   with `GOOS=darwin`: exit 0, nothing said.
2. `make fmt vet docs-check`: exit 0, the tree unchanged after it.
3. The race binary built by `go test -tags ts_omit_webclient,ts_omit_debugeventbus -race -count=1
   -timeout 30m -run '^$' ./cmd/bridge/`, then four concurrent lanes of `go test -tags
   ts_omit_webclient,ts_omit_debugeventbus -race -count=5 -timeout 30m -v -run '<the 41>'
   ./cmd/bridge/`, each with a `TMPDIR` of its own.
4. `make fmt vet test build-all` at its defaults (`P` 4, `TMPDIR` the checkout's `.tmp`), whose
   test step is `go test -tags "ts_omit_webclient,ts_omit_debugeventbus" -p 4 -race -timeout 30m
   ./...`.

At `77418c94` (04:49, 2,440 s in all): steps 1 and 2 clean; the lanes 205 top-level passes each,
820 of 820, none failed or skipped and no data race, in 283.3 s, 283.2 s, 267.9 s and 270.5 s; the
gate exit 2 in 2,109.2 s, 90 packages ok and `cmd/bridge` FAIL in 1,728.9 s on
`TestEveryBackgroundGoroutineDrainsOnCleanup` alone, `build-all` not reached.

At `273fa3bc` (05:33, 2,401 s in all): steps 1 and 2 clean; the lanes 820 of 820 again, none
failed or skipped and no data race, in 280.7 s, 275.1 s, 270.8 s and 281.0 s; the gate exit 0 in
2,090.9 s, the tree unchanged after it. Its test step: 91 packages ok, none failed; `cmd/bridge`
1,729.8 s of its 30 minutes, `internal/manifest` 1,611.0 s, `internal/admin` 1,230.5 s,
`internal/backup` 661.3 s. Its `build-all`: check-build-tags OK, the six release binaries built.

A record holds no run made on its own commit: `make docs-check` and the citation guard on this
record's commit are in its PR's body.

### What stays open

- A stop that lands between the stop's look and the veto's write still lets the write and the
  restore run, the restore taking no context: the moment holds no wait.
- The clear's line does not tell a clear that landed from one that did not. `SaveState`'s error
  could (a barrier's failure follows the rename); the brief kept the item to its words.
- B894's two lists still leave out
  `TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect`, which Bridge
  lows 10's record names as open, beside this change.
