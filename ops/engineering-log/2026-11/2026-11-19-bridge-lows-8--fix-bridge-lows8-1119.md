## 2026-11-19 — Bridge lows 8: the crash rollback's refusal decided before the install marker's lock, after B894

Branch `fix/bridge-lows8-1119`, from main `bc79885c` (#1517's merge, one commit past `cad3596d`,
B894's #1516). Five commits, one for each item and then this record:
- `9f77d9ff`, item 1: the one production condition, its case and its comments;
- `c5348201`, item 2: a case;
- `922bea8c`, item 3: the History's order and its same install;
- `d91bad24`, item 4: a source pin and one History sentence.

The four findings are Lows of B894's review, each confirmed by a second review. None is
reachable in production today. Lines cited of the old code are `bc79885c`'s. Each control was
predicted in writing before it ran. Each was laid into the tree uncommitted, after its item's
commit, and run; its file was then restored from memory, and the tree was checked clean.

Corrected on 2026-11-19 by Bridge lows 10 where this record could not be checked: each red's and
control's selector given and its command line said not recorded; the lanes' tags named, and their
timeout and the gate's command lines and results said not recorded; and items 2 and 4 said green at
the base by construction. Its record,
`ops/engineering-log/2026-11/2026-11-19-bridge-lows-10--fix-bridge-lows10-1119.md`, gives its
reruns of this record's runs, each with its command line verbatim and its result: the lanes'
tests again, the controls K1, M1 and P1 again, and its gate.

### Item 1: the same install is all three fields

What was true. `refuseCrashRollbackBeforeTheLock` (`cmd/bridge/serve_crashed_install.go`
413-435) recorded a refusal decided before the lock wherever the marker read again under it still
asked for the rollback with the checked marker's `TargetVersion` and `AttemptedAt` (431). It did
so whatever that marker's `PreviousVersion` said, while B894's design calls the same install all
three fields. Every production writer of an install marker writes the three together: `Install`'s
fresh marker (`internal/updater/install.go` 522-524) and the settings check's copy
(`configcheck.go` 543). A restamp moves `AttemptedAt` alone (`updater.go` 1046). So the third field
is defence in depth.

As built. The same-install case reads `updater.SameVersion(st.PreviousVersion,
checked.PreviousVersion)` beside the other two. A marker read again that names another replaced
version is another install's: nothing is said or recorded over it, and the next start asks again.
The main path read the same three already. It compares `TargetVersion` and `AttemptedAt` (255),
and holds `PreviousVersion` through the version the backup answered, which matched the marker
checked before the lock (177) and must match the marker read again under it (250). The function's
doc and the helper's now say so. The doc of
`TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock` no longer calls a marker naming
another replaced version the same install.

The case. `TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion` has two cases, one
for each run. The run's stand-in saves the marker with `PreviousVersion` `0.0.0-other`, its target,
attempt and count kept; the case requires that `CrashRollback` still asks for the rollback over
it. The stand-in then answers a refusal decided before the lock: the version's an error ("did not
say its version"), the settings check's `ConfigRefused`. The case wants the install left (no
rename, the binaries unchanged), the marker byte for byte as the stand-in saved it, no updater line
(`updaterLines` empty) and no refusal recorded.

Red first: the case over `bc79885c`'s production code. Both cases were red as predicted, at
`serve_crash_rollback_lock_test.go`:
- `:643`, "update-state.json changed (<nil>): it must be left as it was";
- `:645`, "the crash rollback spoke over a marker naming another replaced version: …". The
  version's case said "the backup at … did not say its version". The check's said "the backup,
  0.0.0-previous, refuses this bridge's settings, …", naming a version the marker read again no
  longer names;
- `:648`, "the marker's refusal = "the backup did not say its version", want none recorded over
  another install", and in the check's case its own why.

That run's command line was not recorded, nor how the base's production code was laid under the
case. Its selector, from the case it names, is
`-run '^TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion$'`. Control K1 below puts
the base's condition back, so Bridge lows 10's rerun of K1 is this red again, at that bundle's
lines, with its command line.

Both cases are green at `9f77d9ff`. Controls at `9f77d9ff`, each run over the seven crash-rollback
lock tests then in the file (B894's six and this one):

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| K1 | the new clause removed: the base's condition | both cases red at 643, 645 and 648; every other test green | as predicted |
| K2 | `st.PreviousVersion` compared with itself | as K1 | as predicted |

The controls' command lines were not recorded. The selector the table's words give, the seven tests
the file defines at `9f77d9ff`, is
`-run '^(TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock|TestABackupChangedWhileItWasCheckedIsNotPutBack|TestTheBackupIsNotPutBackOverSettingsItDidNotCheck|TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock|TestAStopAroundTheBackupsChecksWritesNothing|TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked|TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion)$'`.
Bridge lows 10's record gives its rerun of K1 with its command line.

### Item 2: a lock another holder keeps

What was true. When the helper cannot hold the lock (416-420: `err != nil || !lock.Held() ||
ctx.Err() != nil`), it says the line from the marker checked and records nothing. B894's cases
reached that branch only by a stop: `TestAStopAroundTheBackupsChecksWritesNothing` stops the
version's run, and the lock is then taken on an ended context. None reached it by a lock another
holder keeps.

The case. `TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld` holds
`update-state.lock` itself for the whole case, through an open of the lock file of its own
(`fsutil.OpenLockFile` and `fsutil.TryLock` on `updater.StateLockPath`, let go in its cleanup). It
shortens `LockState`'s wait to 100 ms (`updater.SetStateLockWaitForTest`) and counts `LockState`'s
waits on its data dir (`updater.SetStateLockWaitingForTest`). It runs the rollback on
`t.Context()`, live, with the settings check's stand-in answering `ConfigRefused`. It wants the
line said ("refuses this bridge's settings, so it would not start on them: line 3: …"), and
`update-state.json` and both binaries byte-identical.

The case also shows that the branch was reached by the take's `*updater.StateLockError`, and not
by a stop or an unheld lock:
- the same take on the same context, made first, answers a `*updater.StateLockError` matching
  `updater.ErrStateLockHeld`, after one wait, the context live;
- the rollback's own take is the second wait heard;
- the context has not ended once the rollback returns;
- the case's own `TryLock` took the lock, so the filesystem keeps locks.

Both runs' stand-ins found the lock held (`markersLockHeld`), so the case's hold stood throughout.
The case is green at `c5348201`. It is green over `bc79885c`'s production file too (laid over
the tree by `go test -overlay`), whose branch is the same: the case pins what the base already
did, so it is green at the base by construction and went red first nowhere, and its reds are its
controls' (M1 and M3 below). That run's command line was not recorded; its selector, from the case
it names, is `-run '^TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld$'`.

Controls at `c5348201`, each run over the eight lock tests (B894's six and the two new ones):

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| M1 | the branch records the refusal: `recordCrashRollbackRefusal(dataDir, checked, recorded, save, stderr)` after its line | the new case red at "update-state.json changed"; `TestAStopAroundTheBackupsChecksWritesNothing/in_the_backup's_version` red the same way, since its stop reaches the branch by `ctx.Err()`; the rest green | as predicted, at `:715` and `:520` |
| M2 | the condition alone: its first term, `err != nil`, dropped | every case green, since a nil lock is not `Held` and the branch is taken as before; this is why the control is M1 | as predicted: all eight tests green |
| M3 | the branch's line removed | the new case red at its "did not say"; the stop case red at its own | as predicted, at `:713` ("the start did not say …; its updater lines: """) and `:518` |

The controls' command lines were not recorded. The selector the table's words give, the eight tests
the file defines at `c5348201`, is
`-run '^(TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock|TestABackupChangedWhileItWasCheckedIsNotPutBack|TestTheBackupIsNotPutBackOverSettingsItDidNotCheck|TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock|TestAStopAroundTheBackupsChecksWritesNothing|TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked|TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion|TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld)$'`.
Bridge lows 10's record gives its rerun of M1 with its command line.

### Item 3: the History's order, and its same install

What was true. B760's long form in `ops/History-rules-config-lifecycle.md` (3013-3027) still gave
the crash rollback's checks "in their order" as they were before B894, all under the install
marker's lock: B894 had added a parenthesis about the lock's span and left the list. B660's long
form (2633-2651) stated the same old order, a twin found by searching for the claim. The same
install was `TargetVersion` and `AttemptedAt` in B894's bullet (3802) and in its paragraph on
recording a refusal decided before the lock (3892). Its paragraph on what the decision under the
lock catches (3839) called a marker naming another replaced version "a marker of the same
install".

B760's sentence, before:

> … then `recoverCrashedInstall` itself, never a copy, holding the install marker's lock from its read through the rename (since backlog B894 from the marker's read again: the backup's version and settings check run before that lock, and the rollback decides again under it, the section after B898's below), its checks in their order (`CrashRollback` again under the lock, the migration begun among them; the backup a regular file; its version, `ProbeVersionWithin`; its settings check, `CheckConfig`, of the settings as they are now, which may have changed since the count's starts, so a backup that refuses them too is not put back), the hold and the veto recorded before the rename, and exit 75.

After:

> … then `recoverCrashedInstall` itself, never a copy, its checks in the order backlog B894 gave them (the section after B898's below): `CrashRollback` on the marker read without the install marker's lock, the migration begun among them; the backup a regular file, its identity fixed through a handle on it and the settings' digest read; its version, `ProbeVersionWithin`; its settings check, `CheckConfig`, of the settings as they are now, which may have changed since the count's starts, so a backup that refuses them too is not put back; then the install marker's lock, held from the marker's read again through the rename, `CrashRollback` asked again under it, and the decision made again there on what the two runs answered for, neither run again (until B894 the lock was taken once the unlocked read's checks had passed, and `CrashRollback` again, the backup's file, its version and its settings check all ran under it, in that order); the hold and the veto recorded before the rename, and exit 75.

The same-install paragraph's sentence, before:

> The new helper takes the lock only where the main path does not hold it, reads the marker again, records the refusal where the same install (`TargetVersion`, `AttemptedAt`) still asks, the marker's own why where it refuses, nothing for another install, and says the line from the marker checked, recording nothing, where the lock cannot be held, the marker cannot be read again or the start was stopped.

After, the same sentence with the same install as (`TargetVersion`, `PreviousVersion`,
`AttemptedAt`), followed by three new sentences. They say that both paths read the same install as
those three fields; why the helper's third field is defence in depth (every writer writes the
three together, and a restamp moves `AttemptedAt` alone); that the main path holds
`PreviousVersion` through the version's answer; and which two new tests pin these.

B660's sentence, before: "… the marker read again under it, then three checks in this order, the
first starting no process, …". After: "… then three checks in this order (B660's: since backlog
B894 the backup's version and settings check run before the marker's lock, and the rollback
decides again under it, that section, after B898's), the first starting no process, …".

B894's bullet now gives the main path's same install as `TargetVersion`, `AttemptedAt`, and
`PreviousVersion` through the version's answer, and the helper's as all three. Line 3839 now
reads "a marker of the install's target and attempt that names another replaced version".

The rule line is unchanged. It states the order as B894 shipped it, and it names no field of the
same install:

> **…and the crash rollback asks its backup's version and settings check before the install marker's lock, and decides again under it on the marker read again, the backup's identity and the settings' digest** (2026-11-19, backlog B894, B898's F1). **Never run them with the lock held, never run them again under it, and never put back a backup whose install, identity or settings changed since they answered. Don't fix the identity by a bare Lstat, take two failed reads of the settings for the same settings, or take the lock again to record a refusal while holding it.** … pin it. → History-rules-config-lifecycle.md

The rules file, `config-lifecycle.md`, stays at 99,783 bytes. `make docs-check` and
`TestEveryCitedTestNameExists` passed over the edited file. Control G1, at `922bea8c`: the edited
paragraph's citation of item 1's test was renamed with a suffix (`…Ghost`). The guard went red as
predicted: "no test function of this name exists: … (cited by
ops/History-rules-config-lifecycle.md)". These runs' command lines were not recorded; the guard's
selector is `-run '^TestEveryCitedTestNameExists$'` in `./cmd/bridge/`, and Bridge lows 10's record
gives its own runs of it, a control of G1's kind among them, with their command lines.

### Item 4: the Windows identity, pinned on every host

What was true. B894's control of a bare `Lstat` identity (its C8) is red on Windows alone. There,
an `os.Lstat`'s file ID is read by path when `os.SameFile` first compares it. In go1.27.3's
`src/os/types_windows.go`, `sameFile` (353-363) calls `loadFileId` (287-299), which reads by
`fs.path` once. A handle's stat carries its ID as it is taken
(`newFileStatFromGetFileInformationByHandle`, 46-79, which leaves `path` empty). Elsewhere an
Lstat carries its device and inode as it is taken, so the behavioural cases cannot show the
defect there. B894's record leaves C8's red open, for a windows-latest run of a tree carrying
the mutation.

The pin. `TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle` reads
`serve_crashed_install.go`, normalises CRLF, and parses it. It requires of `recoverCrashedInstall`:
- one `fsutil.OpenAsFile` call, which returns the opened file's own stat
  (`internal/fsutil/notafile.go` 118-138), its second result bound to a name;
- that name bound nowhere else there, by an assignment or a declaration;
- every `os.SameFile` there taking that name, and one of them after the function's
  `updater.LockState`.

One sentence in the History's paragraph on the identity names the Windows-only behaviour the pin
stands for. The pin is green at `d91bad24`, and at the base by construction: it judges only the
body of `recoverCrashedInstall`, which no commit of this bundle changed (from `bc79885c` to `d91bad24`
the production file's diff is item 1's condition in `refuseCrashRollbackBeforeTheLock` and two doc
comments), so it went red first nowhere, and its reds are its controls' (P1 to P3 below).

Controls at `d91bad24`, each on disk while the pin read the file, each run over the pin and the
eight lock tests:

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| P1 | B894's C8: the handle, its close and its compare replaced by `identity := fi` | the pin red at its count of opens and at its name (a fatal); every behavioural case green on Linux | as predicted: `:804` ("calls fsutil.OpenAsFile 0 times") and `:807`; the eight green |
| P2 | the handle kept, the compare under the lock made against the Lstat: `os.SameFile(fi, now)` | the pin red at that `os.SameFile`; the rest green | as predicted: `:826`, naming `serve_crashed_install.go:262:77` |
| P3 | the handle kept, then `identity = fi` after the early compare | the pin red at its count of bindings; the rest green | as predicted: `:814`, "identity is bound 2 times … (serve_crashed_install.go:152:2, …:163:2)" |

The controls' command lines were not recorded. The selector the table's words give, the pin and the
eight lock tests, the nine tests the file defines at `d91bad24`, is
`-run '^(TestTheCrashRollbackFixesTheBackupsIdentityThroughAHandle|TestTheCrashRollbackAsksItsBackupBeforeItTakesTheMarkersLock|TestABackupChangedWhileItWasCheckedIsNotPutBack|TestTheBackupIsNotPutBackOverSettingsItDidNotCheck|TestTheCrashRollbackDecidesAgainOnTheMarkerItReadsUnderTheLock|TestAStopAroundTheBackupsChecksWritesNothing|TestTheCrashRollbackFollowsAnInstallMadeWhileItsBackupWasChecked|TestARefusalBeforeTheLockIsNotRecordedOverAnotherReplacedVersion|TestARefusalBeforeTheLockIsSaidAndRecordedNowhereWhileTheLockIsHeld)$'`.
Bridge lows 10's record gives its rerun of P1 with its command line.

### Runs

At `d91bad24`, in one build-slot session (the slot free at once), 58 tests ran under `-race`,
each named in the selector, which was not recorded verbatim:
- the 53 that `-list 'CrashRollback|Rollback'` lists in cmd/bridge;
- the three B894 tests whose names hold neither word;
- the two new ones, which hold neither either.

Twenty runs of each came from one race binary, built by `go test -c -race` with the release tags,
`-tags ts_omit_webclient,ts_omit_debugeventbus` (`Makefile:9`'s `GO_TAGS`), and the pinned
toolchain, `go.mod`'s go1.27.3. The binary was run directly, as four lanes of `-test.count=5`,
started 20 s apart, each lane with its TMPDIR on disk; the lanes' timeout (`-test.timeout`) was
not recorded. Result: 1,160 top-level passes of 1,160, none failed or skipped, and no data race.
Each lane took 853-856 s, the session 943 s. These were this record's runs. Bridge lows 10's
record gives its own reruns, each command line verbatim: through `go test` (`-race`, four
concurrent lanes of `-count=5 -timeout 30m`), selecting by these words (`CrashRollback|Rollback`,
the three B894 names anchored, the two new ones by their shared prefix) at its base, where they
select more tests.

The gate on the branch, which this record named as `make fmt vet docs-check`, `go vet` of
`./cmd/bridge/` for windows and darwin, and `make fmt vet test build-all`, is in no record and in
no PR body: its command lines and results were not recorded. A record holds no run made on its
own commit.

### What stays open

- The behavioural red of a bare `Lstat` identity on windows-latest (B894's C8) is still unrun. The
  pin guards the source on every host. The native Windows CI leg runs the behavioural cases with
  the chosen check.
- B894's open items stand as its record lists them.
