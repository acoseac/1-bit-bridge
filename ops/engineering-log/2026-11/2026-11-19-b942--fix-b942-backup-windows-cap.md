## 2026-11-19 — B942: internal/backup's three many-start tests count their reservation's flush, and the native legs' measurement that decides it

Branch `fix/b942-backup-windows-cap`, from `e51c2b2b` (main with #1511, the tip when the work
began). Six commits and this record: `9e0ca732`, the measurement on the native legs, and
`0b51be76`, its revert; `39399c24`, the fix (test code in `internal/backup` alone); `71811c4c`,
the rule; `95efbac8`, the measurement again after the fix, and `875b1b12`, its revert. No
production code changes, and no cap: `internal/backup`'s 45 minutes on the native legs are
B897's (`fix/b897-make-test-headroom-v2`, not on main). The census, the Makefile's `test` recipe,
the legs and B897's lines are as main has them once each measurement is reverted. This session
ran on the Linux test host, which pushes nothing, so the measurement commits ran on CI only once
the branch was pushed: their numbers are under "The CI measurements".

### The hazard

On `test (windows-latest)` `internal/backup` is one test binary under `go test … -timeout 30m
./...` (`.github/workflows/gate.yml` 617-618), the cap per package binary. Of the last 30 gate
attempts the 28 that finished took 776.3-1,521.5 s there (median 1,058.2 s, 59 % of the cap);
44167989852/1 and 44362202450/1 were killed at 1,800.0 s, and B897's evidence run 44097405776/1
is a third, each on one of the slowest runners read (1.54-1.81 times the median over six other
packages), each dump an ordinary test seconds in. macOS takes 270.6-432.9 s, Linux `-race` (the
`rest` shard) 604.3-893.3 s. The package went from 61.9 s to 686.7 s on Windows (15.5 to 61.1 s
on macOS) at #1346 (`6be9487c`; main runs 40163643036 and 40167226878, one image and region, 59
minutes apart), and the only tests in it that loop thousands of times came with it:
`TestAdoptionBoundsReservedSpaceAcross4097Restarts` (4,097 starts),
`TestRestoreAdoptsUUIDOnlyAfter8193Starts` (8,193) and
`TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling` (8,193), 20,483 real engine starts
a run. Each start writes its reservation in `engine.New` (`owner.go`'s `start`, through
`WriteBytesDurable`, `engine.go` 266-268), synchronously and once: the engine writes another
only for an edit (`ahead`, after an insert or a delete), and these starts make none.
`reservationStorage` stands in for the directory barrier and the medium check only, so each
start's staged file was flushed for real.

### Step 1: the measurement (`9e0ca732`, again as `95efbac8`)

The native legs' `test` step runs every package uncached (`-count=1`), so the six packages the
runner factor reads (`cmd/bridge`, `internal/manifest`, `internal/admin`,
`scripts/third-party-licenses`, `internal/api`, `internal/transcode`) run beside
`internal/backup` on the same runner as in an ordinary run (the review: a factor from another
job cannot describe the measurement's runner); `-v` into a log under `$RUNNER_TEMP`, so
`internal/backup`'s top-level tests carry their seconds (the ordinary logs are not verbose);
`-timeout 45m` a package, so a slow runner's `internal/backup` ends with its time rather than at
the cap; and the job's `timeout-minutes` 120 for that commit, so vet and an uncached suite whose
packages may each take 45 minutes are not killed with the log's tail (the review). The
step prints every package's `ok`/`FAIL` line (the package's seconds, the share's denominator),
`internal/backup`'s 40 slowest top-level `--- PASS`/`--- FAIL` lines, slowest first, and the
three tests' lines, then exits with go test's code; the whole log is uploaded as the artifact
`b942-measure-<os>` (the pinned `actions/upload-artifact` fuzz.yml uses). The tags stay on the
command, and the census (`scripts/third-party-licenses/tags.go`) takes the changed line in the
same commit, so the Linux legs stay green: `go test ./scripts/third-party-licenses/` passed on
each of the four commits (21.6-21.9 s). Multi-package `-v` output is printed a package at a time
(go1.27.3 `src/cmd/go/internal/test/test.go` 1486), which the parse relies on. The step's shell
was run here under `bash -eo pipefail`, as Actions runs it, over a synthetic log with packages
before and after `internal/backup`, subtests and a failure: 40 lines in order, the other
packages' tests left out, the exit code passed through; macOS's BSD `awk`, `sort` and `grep` were
not run here (`/usr/bin/sort` is named, a Windows runner's `sort.exe` kept out of the way).

On CI this step ran once the branch was pushed ("The CI measurements" has its numbers).

Locally (the Linux test host, 16 cores, load 0.08; TMPDIR on ext4; go1.27.3 with the release tags), from
binaries built with `go test -c`, "real" from `0b51be76` (main's tests) and "counting" from the
fix, each run alone with `-test.run` the three tests and `-test.count=1`, three rounds in the
build slot with the variants interleaved:

| variant | round 1 | round 2 | round 3 | median | a start |
|---|---:|---:|---:|---:|---:|
| real flush | 139.56 s | 140.53 s | 139.24 s | 139.56 s | 6.81 ms |
| counting flush | 5.00 s | 5.01 s | 4.99 s | 5.00 s | 0.24 ms |
| real flush, `-race` | 157.68 s | 158.21 s | 158.15 s | 158.15 s | 7.72 ms |
| counting flush, `-race` | 11.63 s | 11.62 s | 11.66 s | 11.63 s | 0.57 ms |

The counting flush saves 96.4 % of the three's seconds without the race detector and 92.6 % with
it. Per test, real: 27.64-27.90, 55.80-56.18 and 55.65-56.45 s; counting: 0.99-1.00,
1.96-1.97 and 2.03-2.05 s. Every run passed 4,097, 8,193 and 8,193 numbered starts.

The package whole, without the race detector, one run each in the slot (311 top-level tests, the
sum of their seconds the binary's wall, no test in the package being parallel):

| variant | `internal/backup` | the three | their share |
|---|---:|---:|---:|
| real flush | 477.5 s | 139.44 s | 29.2 % |
| counting flush | 342.7 s | 5.03 s | 1.5 % |

The next slowest tests are the same in both, the first three 19.9 s
(`TestAVerifiedRecordLeftForAFailedOneIsVetoedWhenItsWriteFails`), 15.1 s
(`TestEveryStepUnderTheLockIsBoundedAndMarked`) and 14.5 s
(`TestARestoreWhoseSourceDoesNotAnswerIsRefusedWithinItsBound`). Under the race detector, one
run each at the end of the gate's session (below):

| variant | `internal/backup`, `-race` | the three | their share |
|---|---:|---:|---:|
| real flush | 770.2 s | 157.46 s | 20.4 % |
| counting flush | 624.1 s | 11.54 s | 1.8 % |

146.1 s less, 19.0 % of the package. None of this is windows-latest's: there the three took
589.2 s before the fix, 28.8 ms a start, and 37.2 s after it, 1.8 ms a start ("The CI
measurements").

### Step 2: the decision

The stop rule, as both reviews left it, was to stop at the first measurement if the three were
under 25 % of `internal/backup`'s seconds on windows-latest, and after the second, if their
Windows seconds, each divided by its run's runner factor, fell by under 70 %, to revert the fix
and the rule in new commits and stop there. Neither number existed when it was built; both are
under "The CI measurements" below. The fix was built and committed on what the Linux test host
measured (the three 29.2 % of the package on a disk whose flush costs 6.6 ms a start, the
counting flush saving 96.4 %) and on the premise's estimate for Windows (26-30 ms a start there,
plausibly 550-650 s of the median's 1,058 s), neither of which is the Windows measurement: the
decision was provisional until the CI measurements, which confirmed it. The review wanted the
rule line after the second measurement; it was committed before it here, so that a miss would
have reverted its commit with the fix's, and there was no miss.

### Step 3: the fix (`39399c24`)

`stubReservationFlush` (`internal/backup/restored_identity_test.go`), called first in each
numbered start of the three tests, installs for that start alone a file flush that counts each
staged file of the engine's reservation writer (its `.player-ids-` prefix, `engine.New`'s) and
answers nil, and flushes any other file as before; and a directory barrier that records each
directory the strict writer syncs once such a file was flushed, answering nil as
`reservationStorage`'s does. Its restores are registered before `startRestoredPlayer`'s cleanup,
so they run once the engine has stopped. After the token check, `requireOneDurableWrite` fails
the start unless exactly one reservation file's flush was asked for and the strict writer's
barrier of that file's directory ran after it: `atomicwrite.WriteBytes` asks for the same flush
through the same seam, and its own barrier is another (`commitDirEntry`, `syncParentDir`), so a
start that wrote through it fails as one that flushed nothing does (the review: a count alone
would take either writer). The hooks hold for a numbered start alone, so
`TestRestoreAdoptsUUIDOnlyAfter8193Starts`'s snapshot, restore and `firstValues` start keep their
real flushes, as does everything the three do outside their numbered starts (the adoption, the
starts after each loop), and the count is a start's, never a total, so no check counts what
those steps flush (the review); `reservationStorage`, which 114 calls in 29 files share, is
unchanged. No assertion, start,
subtest, iteration or bound changed, and every start still runs the real engine, its strict
writer staging, renaming and syncing, so `ops/History-rules-player.md` 396 ("runs the real engine
for every start") stays true.

What the three no longer exercise is the real flush of a start's reservation, 20,483 times a run.
It stays pinned where the helper's comment says: every other test here that starts an engine
(`startRestoredPlayer`, `firstValues`), these three's starts after their loops among them, and
the engine's `TestAReservationIsMadeOnlyWithItsDirectoryEntry` (`reserve_test.go` 142), which
drives the engine's own strict writer with the real flush and a test barrier, the engine-level
guard the review asked to cite; a flush that fails, in the restore's failure injection
(`TestRestoreRequiresTheReservationBarriersBeforeIdentity`, its file case, which answers an error
and calls no `Sync`); a crash on either side of a flush, in `crashRestore`, the crash seam that
still calls `f.Sync()`; and the strict writer's own barriers and checks, in atomicwrite's 34
`TestWriteBytesDurable_*` tests (config-lifecycle.md's WriteBytesDurable bullet lists 25 of
them). None of those holds the staged file's flush itself: under C4's mutation below (the strict
writer's staged file unflushed), the 29 of them that run on the Linux test host all passed, the other five
skipping there as they always do (four need a mount namespace, one a volume that folds case);
the restore's injection and the new check are what fail if the strict writer stops asking for
the flush.

### Step 4: the controls

Each predicted in writing before it ran (`.tmp/_b942/controls.predictions.txt` on the Linux test host), each
a `go test -overlay` build of a production file mutated in memory by an exact-once replacement,
the tree never edited (`git status --short` empty after every batch). "Fix" runs over `39399c24`'s
tests; "main" lays `0b51be76`'s four backup test files (main's) over the same tree.

| | mutation | predicted | seen, fix | seen, main |
|---|---|---|---|---|
| C1 | `startCounter`'s first value one later (`base + idBlock + 2`) | all three red at their first start by the token check, as before | as predicted: "startup token 4098 at restart 0" (R8, 4,097 starts failed), "… at start 0" (R9, stopping after it at its first-reservation check; R10, 8,193 failed) | the same three reds, the same words |
| C2 | `floorlessSnapshotCeiling` one more | R10 red after its loop, R8 and R9 green | as predicted: draw_0 and draw_16777215, "restored floor {… IDs:2181038081 …}, want 2181038080" | not run |
| C3 | the adoption's base one less, both counters | R9 red after its loop; R8 green (crypto/rand's draw, a range check that only a zero draw would leave); R10 green | as predicted: "restore skipped adoption before its jump: {… IDs:2164260863 …}" | not run |
| C4 | the strict writer's staged file unflushed, its rename and every barrier kept (`writeBytes`, where the write is durable) | the three red at their first start by the new check, tokens passing; the restore's file case red | as predicted: "the start flushed [] and then synced []: want one reservation file's flush, then its directory's barrier from the strict writer", every numbered start; `TestRestoreRequiresTheReservationBarriersBeforeIdentity/file` "restore did not refuse its barrier: <nil>" | the three green; only the restore's file case red |
| C5 | the engine's reservation through `atomicwrite.WriteBytes` | the three red by the new check, tokens passing; the engine's directory-entry test red | as predicted: "the start flushed [<dir>/.player-ids-<n>.tmp] and then synced []"; `TestAReservationIsMadeOnlyWithItsDirectoryEntry` "edits {Refused: Detail:} with the reservation's directory entry not durable, want refused reservation_not_durable" | the three green; the engine test red |
| C6 | a start that writes its reservation twice | the three red by the new check, tokens passing | as predicted: two staged files flushed, each followed by its barrier | the three green |

So the arithmetic still reds the three as it did (C1 alike over both; C2 and C3 their own
subjects), and the three gain what main's did not have: C4, C5 and C6 pass main's tests and fail
the fix's, with every token check green. C4a, C4's mutation over atomicwrite's own
`TestWriteBytesDurable_*`, predicted all green: 29 passed and 5 skipped, as above.

### The rule (`71811c4c`)

One line in `.claude/rules/build-ci-tests.md`, beside the bullet on fixtures that bulk-load
through the batch APIs, and its long form in `ops/History-rules-build-ci-tests.md` before the
build-tag fixture's entry: a test whose subject is not durability, and which makes thousands of
durable writes on its way, counts their flushes in the step that writes them, and asserts there
that the write asked for its flush and that the strict writer's barrier ran after it. B897's open
diff touches lines 52 and 224 of the rules file and appends to the history's end, so the two do
not meet.

### Step 5: the proof on CI

It ran on CI in this order (the review: the gate cancels a run in progress when the ref moves,
so each measurement was pushed alone and its runs finished before anything later was):

1. `9e0ca732` was pushed alone (the PR open to main), and both native legs finished. Each leg's
   step output and artifact held the six calibration packages' seconds (a line at its cap is no
   sample), `internal/backup`'s package line and its 40 slowest top-level tests, which give each
   one's share and the three's sum and share. The runner factor is the median, over the six, of
   each package's seconds over its median: on Windows 1,120.6, 789.8, 731.1, 104.2, 91.8 and
   91.3 s, on macOS 447.7, 156.2, 262.5, 53.1, 33.4 and 21.9 s. Had the three been under 25 % of
   the Windows package, the work would have stopped there, the branch at `0b51be76` and a record
   of the numbers; they were 53.7 %.
2. `95efbac8` was pushed next, and both legs finished. The three's Windows seconds, divided by
   that run's factor, were set against the first run's, divided by its own: a fall under 70 %
   would have reverted `39399c24` and `71811c4c` in new commits and stopped the work there; they
   fell 91.6 %. Each re-run of that run's `cross-platform` job is another fresh sample with a
   factor of its own (every package uncached).
3. `875b1b12` and the rest were pushed after it. From then on, an ordinary run's
   `internal/backup` line is a sample only when it carries seconds, never `(cached)` (the
   review), and only beside six calibration lines that do; the bar, the only one: the median of
   the normalised `internal/backup` seconds on windows-latest at 796 s or less (factor 1). The
   measurement run after the fix put the package at about 657 s normalised. At the slowest
   factor read so far (1.81), 796 s is 1,441 s: 80 % of 30 minutes, 53 % of 45.

Before, for the comparison (the premise's 30 attempts): `internal/backup` on windows-latest
1,058.2 s raw at the median (59 % of 30 minutes, 39 % of 45), 1,086 s normalised (60 %, 40 %),
963-1,143 s normalised over the 28 that finished; on macOS 311.9 s raw at the median.

### The CI measurements

Both measurement commits ran on the pull request's gate, on the Windows and macOS legs.

- Before the fix (`9e0ca732`, gate run 44442688618): on Windows `internal/backup` took 1,097.2 s;
  the three tests took 289.07 s (`TestRestoreAdoptsUUIDOnlyAfter8193Starts`), 170.73 s
  (`TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling`) and 129.44 s
  (`TestAdoptionBoundsReservedSpaceAcross4097Restarts`), 589.2 s, 53.7 % of the package: over
  the 25 % stop, so the fix went on. On macOS the package took 338.0 s.
- After the fix (`95efbac8`, gate run 44445826255): on Windows the package took 528.5 s and the
  three 15.41 s (`TestRestoreAdoptsUUIDOnlyAfter8193Starts`), 14.40 s
  (`TestLegacyFloorlessSnapshotRestoresAboveTheAdoptionCeiling`) and 7.37 s
  (`TestAdoptionBoundsReservedSpaceAcross4097Restarts`), 37.2 s.
- The runners' speed, the median over the six calibration packages of seconds against the
  premise's medians, was 1.076 before and 0.805 after: normalised, the three fell from 547.6 s to
  46.2 s, 91.6 %, past the 70 % bar, so the fix stays; the package fell from about 1,020 s to
  about 657 s.

### Runs

Before the gate, on the fix: the three tests at `-count=3` (14.7 s) and at `-race -count=3`
(35.1 s) passed; `go vet -tags ts_omit_webclient,ts_omit_debugeventbus ./internal/backup/` for
windows and for darwin was clean; and cmd/bridge's `TestEveryCitedTestNameExists`,
`TestNoDocblockNamesAnotherDeclaration` and `TestNoBlankKeepers` passed over the new test code and
again over the rule.

The gate ran on `875b1b12` in one build-slot session, the slot free at once (00:56:55-02:06:48
UTC): `make fmt`, which rewrote nothing (`git status --short` empty); `make vet`; `make vet` with
`GOOS=linux GOARCH=386`; `go vet` of `./...` with the release tags for windows and for darwin;
`make docs-check`; `make test TEST_PKGS=./internal/backup/`, ok in 624.8 s; `make test` (`P=4`,
`-race -timeout 30m`), ok, 91 packages, 2,108 s, `cmd/bridge` 1,723.5 s (inside its 30 minutes,
so it was not run again alone), `internal/manifest` 1,611.5 s, `internal/admin` 1,231.3 s,
`internal/backup` 661.4 s, `internal/transcode` 194.8 s, `internal/api` 151.3 s; `make
build-all`, six targets in 35 s; the tree clean after it; then the race table above. This record
changes one Markdown file; the checks run on its own commit are in the branch's report.

### Candidates, not changed

Twenty-five sites in the package's tests sleep 1.1 s; the two read (`backup_test.go` 429,
`verify_test.go` 814-823) wait for the clock's next second, which a snapshot's one-second name
and a manifest's time are read in, so they wait for something observable, and a shorter path
needs a clock in production code. None of the sleeps read waits for nothing.
