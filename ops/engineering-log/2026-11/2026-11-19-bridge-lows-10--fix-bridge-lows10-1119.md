## 2026-11-19 — Bridge lows 10: B894's bullet gives bridge lows 8 its field, and Lows 8's record says what it did not record

Branch `fix/bridge-lows10-1119`, from main `dd688b15` (#1521's merge, B945). The brief was written
at `b332bb6c`, #1519's merge; B898FU's #1518 (`fffa7f5a`) has merged since and moved the lines it
cites, so lines below are `dd688b15`'s unless a commit is named. Three commits:
- `e11405e9`, item 1: B894's bullet and its rule line;
- `cc453212`, item 2: bridge lows 8's record, corrected in place;
- this record.

No production code and no test logic changes. Each control was predicted in writing before it
ran, laid into the tree uncommitted after its item's commit, its mutation on disk for the whole
run, and its file then restored from memory and the tree checked clean. Every command below ran
from the checkout's root with `GOTOOLCHAIN=go1.27.3`, each `go test` with `TMPDIR` on disk in a
scratch folder below the checkout's `.tmp/`, where the line says nothing else.

### Item 1: B894's bullet gives `PreviousVersion` to bridge lows 8

What was true. In `ops/History-rules-config-lifecycle.md` the crash rollback's bullet (3877; 3808
at `b332bb6c`), credited "(2026-11-19, backlog B894, B898's F1; backlog B898FU's rounds 3 and
4)", gave the install over which `refuseCrashRollbackBeforeTheLock` records a refusal as "the same
install (its `TargetVersion`, `PreviousVersion` and `AttemptedAt`)", while the same section's
paragraph on recording such a refusal (3984-3989) says the helper compares `PreviousVersion` since
bridge lows 8 and B894 compared the other two. Its pin list, and the rule line's
(`.claude/rules/config-lifecycle.md:635`; 685 at `b332bb6c`), named B894's seven tests and
B898FU's eight, and none of bridge lows 8's three.

As built. The clause reads "the same install (its `TargetVersion` and `AttemptedAt`, which B894
compared, and since bridge lows 8 its `PreviousVersion`)". The bullet and its rule line credit
bridge lows 8 between B894 and B898FU, the order they merged in. Both lists add, after B894's
seven, `TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion` (the third field),
`TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld` (a lock another holder keeps)
and `TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle`, which guards the rule's "Don't fix
the identity by a bare Lstat". The rule line names no field of the same install, so its credit and
its list are all that change there.

The budget. `.claude/rules/config-lifecycle.md` was 99,783 bytes at `b332bb6c`, 98,897 at
`dd688b15` (#1518 moved two long forms out, backlog B190's and #1046's) and is 99,112 at
`e11405e9`, under its 100,000: no long form moves, a move being the budget rule's answer past the
limit alone. `make docs-check` over the edit: "check-doc-budgets: ok — CLAUDE.md 89.6%; player.md
100.0%; worst load, output_dac_bind_test.go 87.8%." (player.md's share is the base's).

The guard over the edit, green:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -run '^TestEveryCitedTestNameExists$' -v ./cmd/bridge/`:
"--- PASS: TestEveryCitedTestNameExists (0.81s)". The controls, at `e11405e9`, each that command
over one citation suffixed (`…Ghost`), the one in the list beside its two neighbours of this
commit:

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| C1 | the History bullet's citation of `TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion`, suffixed | the guard red at `cited_test_names_test.go:74`, naming the suffixed name "(cited by ops/History-rules-config-lifecycle.md)" and nothing else, exit 1 | as predicted: "no test function of this name exists: …Ghost  (cited by ops/History-rules-config-lifecycle.md)" |
| C2 | the same citation in the rule line, suffixed | as C1, "(cited by .claude/rules/config-lifecycle.md)" | as predicted |

Both read alike: `scanTestCitations` feeds every tracked `.md` file that is no plan and not the
frozen archive into one table, and `.claude/` is no directory its walk skips. The two controls show
each file read rather than infer it.

### Item 2: bridge lows 8's record says what it did not record

What was true. `ops/engineering-log/2026-11/2026-11-19-bridge-lows-8--fix-bridge-lows8-1119.md`,
as merged: its lanes ran "the release tags", no `-tags` value, and no timeout was recorded
(187-190); its reds and controls gave no command line or selector (44, 54-55, 91, 170-171, and the
guard's runs and G1 at 142-146); and its gate was "in its report" (192-194), which neither a record
nor #1519's body holds. #1519's body says "Each case went red first at the base", while that
bundle's items 2 and 4 pin what the base already did (88-89, 168), green there by construction:
their reds are their controls'. #1519's body stays as merged.

As corrected, in place:
- each red's and control's selector, from the tests it names: for its three tables, the tests the
  file defines at `9f77d9ff` (seven), `c5348201` (eight) and `d91bad24` (nine), as
  `cmd/bridge/serve_crash_rollback_lock_test.go`'s `func Test` lines at each commit give them,
  anchored; each command line said not recorded;
- the lanes' tags named, `-tags ts_omit_webclient,ts_omit_debugeventbus`, `Makefile:9`'s `GO_TAGS`;
  their binary said run directly, its timeout said not recorded, and its runs told apart from this
  record's `go test` reruns;
- items 2 and 4 said green at the base by construction: item 2's case pins what the base did (that
  record shows it green over `bc79885c`'s production file), and item 4's pin judges only the body
  of `recoverCrashedInstall`, which no commit of that bundle changed (from `bc79885c` to `d91bad24`
  the production file's diff is item 1's condition and two doc comments);
- the gate's command lines and results said not recorded, its steps as that record named them;
- a pointer to this record.

`b332bb6c`'s tree is that branch's head, `fbeec1ba`'s, unchanged. `make docs-check` and the guard
passed over the corrected record before its commit ("check-engineering-log: ok — … 15 entries
since"; "--- PASS: TestEveryCitedTestNameExists (0.80s)").

### Runs, at `cc453212`

The lanes' selector, its count first:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -list 'CrashRollback|Rollback|^TestABackupChangedWhileItWasCheckedIsNotPutBack$|^TestTheBackupIsNotPutBackOverSettingsItDidNotCheck$|^TestAStopAroundTheBackupsChecksWritesNothing$|^TestARefusalBeforeTheLock' ./cmd/bridge/`
lists 69 tests: 58 defined at `b332bb6c`, where the brief counted 58, and 11 that B898FU's #1518
added, eight in `serve_crash_rollback_lock_test.go`, two in `serve_held_binary_test.go`
(`TestTheBootRollbackKeepsItsMarkerWhereTheBackupDidNotSayItsVersion`,
`TestTheBootRollbackKeepsItsMarkerWhereTheBackupCannotBeInspected`) and one in
`serve_refused_install_probe_test.go` (`TestTheEarlyRollbackTellsABackupItCannotInspectFromNone`).

The lanes, in one build-slot session with the vets and the gate below (the slot free at once; load
0.55 as it began; 3,104 s in all). A build of the race test binary that runs nothing came first,
so that the lanes link it rather than compile it four times at once:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -race -count=1 -timeout 30m -run '^$' ./cmd/bridge/`,
29 s. Then four lanes started together, each with a `TMPDIR` of its own:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -race -count=5 -timeout 30m -v -run 'CrashRollback|Rollback|^TestABackupChangedWhileItWasCheckedIsNotPutBack$|^TestTheBackupIsNotPutBackOverSettingsItDidNotCheck$|^TestAStopAroundTheBackupsChecksWritesNothing$|^TestARefusalBeforeTheLock' ./cmd/bridge/`.
Each lane: ok, 345 top-level passes over the 69 tests, none failed or skipped, and no data race,
in 906.7 s, 883.4 s, 905.9 s and 887.3 s by `go test`'s own count; 1,380 top-level passes of
1,380 in all, twenty of each test. These are `go test` runs, each lane a process of its own under a
30-minute `-timeout`; Lows 8's were one race binary run directly, its timeout not recorded.

The controls again. K1, M1 and P1 as Lows 8's tables give them, each over every test
`serve_crash_rollback_lock_test.go` defines (its 17 `func Test` lines, anchored; `-list` gives
17), without `-race`:
`go test -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -run '^(TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock|TestTheCrashRollbackTellsABackupItCannotInspectFromNone|TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect|TestTheCrashRollbackTellsABackupItCannotOpenFromNone|TestTheCrashRollbackTellsABackupItCannotInspectUnderItsLockFromAChange|TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange|TestTheCrashRollbackPutsBackABackupAnotherHandleReads|TestTheCrashRollbackTellsALinkAtItsBackupFromABackupItCannotInspect|TestTheCrashRollbackPutsBackABackupRecordedAsChangedOnceItsIdentityReads|TestABackupChangedWhileItWasCheckedIsNotPutBack|TestTheBackupIsNotPutBackOverSettingsItDidNotCheck|TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock|TestAStopAroundTheBackupsChecksWritesNothing|TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked|TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion|TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld|TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle)$' -v ./cmd/bridge/`.
Unmutated at `cc453212`, 17 of 17 passed (78 with their subtests), none skipped. K1's and M1's
text is at this base as Lows 8 left it; P1's block holds B898FU's comment lines too, which go with
it.

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| K1 | the base's condition: the helper's same-install case without `updater.SameVersion(st.PreviousVersion, checked.PreviousVersion) &&` | `TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion` red in both subtests, each at `:1186` ("update-state.json changed (<nil>): it must be left as it was"), `:1188` (it spoke over a marker naming another replaced version) and `:1191` (a refusal recorded over another install); the other 16 green | as predicted, 16 of 17 passing |
| M1 | the helper's branch for a lock it cannot hold records the refusal: `recordCrashRollbackRefusal(dataDir, checked, recorded, save, stderr)` after its line | `TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld` red at `:1258`, and `TestAStopAroundTheBackupsChecksWritesNothing/in_the_backup's_version` at `:1063`, each "update-state.json changed"; the other 15 green | as predicted |
| P1 | B894's C8: the handle, its close and its compare replaced by `identity := fi` | the pin red at `:1374` (`fsutil.OpenAsFile` called 0 times) and `:1393`, a fatal (no handle's stat bound to a name); `TestTheCrashRollbackTellsABackupItCannotOpenFromNone` red in all five subtests, its unreadable backup now put back unopened: at `:443` in the three rows whose refusal is not kept (the auto-installer does not wait, the marker now the rollback's record), then at `:445`, a fatal, in every row (the backup's path is empty once it is put back); the other 15 green | as predicted |

K1 is Lows 8 item 1's red at the base again, its three assertions at today's lines (643, 645 and
648 then). P1's second red is new since Lows 8 and is no identity's: it comes from the open P1
takes away with the handle, which B898FU's case of a backup this user may not open needs; the bare
identity's own behavioural red is still Windows' alone. Each control ran in about 11 s, the tree
clean after each.

The vets: `GOOS=windows go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./cmd/bridge/` and
`GOOS=darwin go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./cmd/bridge/`, each exit 0 with
nothing said. Then `make fmt vet docs-check`, exit 0, the tree unchanged after it
("check-engineering-log: ok — … 15 entries since").

The gate: `make fmt vet test build-all`, at its defaults (`P` 4, `TMPDIR` the checkout's `.tmp`),
exit 0 in 2,148 s, the tree unchanged after it. Its test step,
`go test -tags "ts_omit_webclient,ts_omit_debugeventbus" -p 4 -race -timeout 30m ./...`: 91
packages ok, none failed; `cmd/bridge` 1,724.5 s of its 30 minutes, `internal/manifest` 1,610.3 s,
`internal/admin` 1,233.7 s, `internal/backup` 662.1 s. Its `build-all`: check-build-tags OK, the six
release binaries built. A record holds no run made on its own commit: the guard and
`make docs-check` on this record's commit are in its PR's body.

### What stays open

- Lows 8's own command lines, its lanes' timeout and its gate's results stay not recorded: its
  record now gives what its words establish, and the runs above are this bundle's, not those.
- B894's C8, a bare `Lstat` identity's behavioural red, is still unrun on Windows, as Lows 8's
  record leaves it: P1's one behavioural red here, on Linux, is the open P1 removes, not the
  identity.
- Beside this change: the bullet's two lists name every test of
  `serve_crash_rollback_lock_test.go` but B898FU's
  `TestTheCrashRollbackRecordsTheMarkersOwnRefusalBesideABackupItCannotInspect`, which that item's
  own record cites. Out of this bundle's budget, which names the three tests it adds.
