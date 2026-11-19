## 2026-11-19 — B945: a given-up verification's write and read-back wait at their gates in internal/backup

Branch `fix/b945-verify-hold-timing`, from main 9c7879a6. Commit b9cd7b00 changes two
test files of `internal/backup` (`verify_hold_test.go`, `verify_stuck_test.go`), and the
commit after it holds this record and the rule. No production code changes, so every
production line cited here is the same at 9c7879a6 and on the branch.

### The sighting

Main's gate run 44479563688, `test (windows-latest)`, attempt 1 (its rerun passed):
`TestAGivenUpVerificationReadsItsRecordBack/whose_INCOMPLETE_is_not_written` failed at
`verify_hold_test.go` 859, "an INCOMPLETE not written: recorded false, the record reads
PENDING, a step of the verification did not end and was given up, and holds the snapshots
until it does: its check did not end within its bound (context deadline exceeded); want
nothing recorded, the PENDING read and the write's failure said".

### What raced

Not the check's deadline: the case parks the check on its thread before it reads anything
(`SetOnLowPriorityForTest`), so the check is given up once its 300 ms deadline and half of
`StopWriteBound` have passed (`runCheck`, `finish.go` 780-811; `verifyHeld` 573-582), on
purpose and on every host. What follows it is the race. The verdict's write is refused at its
admission (`publishOf`, `hold.go` 426 and 462-466), and the INCOMPLETE is written in a step of
its own (`stepIncomplete`, `finish.go` 625-630) whose bound is `laterBound(deadline)`, a
second from its start (`hold.go` 539), armed by `runStep`'s timer (487, 526). The injected
failure comes from the staging (`stageAndRenameWithHook`, `verify_record.go` 536), after the
step has read the record (301) and named the snapshot by a stat, a listing of the backups
folder and a stat of each entry (`boundName`, 311). A step past its second is given up
(`expire`, `hold.go` 495-506) and its late answer dropped; `verifyHeld` joins the write's
error to its answer only where the step answered (`finish.go` 638-643, its `wrote`), and the
read-back after it (480-503) read PENDING, the capture's owner held. So
the answer named the check's give-up and nothing of the write: the sighting's words. Where the
worker's goroutine itself runs late, the step's answer and its timer can be ready together,
and the select takes either.

`TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` has the same step under the same
bound and reads the INCOMPLETE it lands (`verify_stuck_test.go` 421): a write past its second
skips its rename, which asks the step's ended signal first
(`atomicwrite.RenameWithRetryUnsyncedCtx`, `verify_record.go` 576), and leaves the record
PENDING.

### Not a production finding

The stop condition of both reviews of the brief: whether an expired bound can stop that write
or swallow its error in production and leave the snapshot PENDING with nothing said. The
write's signal is a context of its own, never the check's deadline or the caller's stop
(`hold.go` 457), so only its own bound gives it up. A write given up is `WriteUnfinished`
(`finish.go` 152, 631), which the command and the ticker word as a write that did not finish
(`cmd/bridge/backup.go` 346-347) and the console's job likewise
(`internal/admin/handlers_backups.go` 318-319), never one that failed or wrote nothing
(`TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads/whose_write_did_not_finish`
pins the command's words); the answer wraps `ErrCheckGivenUp`, naming the step the hold was
first given up at, and the record it reads back is what every reader reads then. A PENDING a
write given up leaves reads INCOMPLETE (interrupted) as soon as the worker has let the
capture's ownership go (the forced red below reads it so). The error such a write would answer
is not known when the verification answers, and the answer claims none; the case asserted an
error only a write that answers within its second gives.

### The red, forced

A scratch generator laid each test file over the package with `go test -overlay`, never
touching the tree, each forcing hook joined onto one existing line so every reported line is
the file's own; `-race`, the release tags.

- F1, the sighting's step: in `whose INCOMPLETE is not written` the staging's callback
  (`SetVerdictWriteFailuresForTest`'s stage) waits until the read-back begins
  (`SetReadBackFailureForTest`'s hook, which only a worker gone on past the write reaches, the
  write given up) or 3 s pass, then answers the injected failure. At 9c7879a6: red, that case
  alone, at 859 with the sighting's message word for word; the other two green. At b9cd7b00:
  green, the case 5.40 s, its write held past its second until the stall's own 3 s let it go.
- F2: in every case the read-back waits until the worker hands its answer on
  (`SetVerifyHandoffForTest`'s answer hook) or 3 s pass. At 9c7879a6 all three red, the record
  the zero value ("the record reads ;"); at b9cd7b00 green.
- F3: the sibling's INCOMPLETE staging waits until the read-back begins or 3 s pass, then goes
  on. At 9c7879a6 red at 421 ("the record of the check given up: database PENDING [], want
  INCOMPLETE "deadline"", the files likewise) and at 476 ("the record once the check ended:
  database INCOMPLETE [interrupted]"); at b9cd7b00 green.

Unforced, both tests are green at both commits: on this host the write ends within its second.

### The fix

Both tests hold `stepOfIncomplete` and `stepOfReadBack` (the package's words, constants of
the test package) at their gates (`SetStepGateForTest`), fired by `joinBackupCallers`' release
before the callers are joined, the seam put back after them, and require each gate to have
held its step (`requireGatesHeld`, over `StepGate.Entered`). The case requires each write's
outcome and its read-back too: `WriteRecorded`, `WriteUnchanged` and `WriteFailed`, and
`RecordRead` with no `ReadErr`, which `Recorded` alone does not tell apart. The check keeps its
300 ms deadline.

The bound moved. `awaitParkedAndGivenUp` timed the answer against the deadline, half of
`StopWriteBound` and two seconds: with the write held and its staging stalled 3 s (F1) the
answer came 4.30 s in, so a write slower than that slack would still fail the case, by its
time. `awaitParkedAndGivenUpAt` times the worker's arrival at the write's gate instead, which
follows the check's give-up at once and comes after only steps the code bounds (the slot, the
lock, the record's read, the deadline and the check's grace); it awaits the answer under the
watchdog alone, fails at once on an answer with no arrival, and is `awaitParkedAndGivenUp` with
no gate.

### Controls

Each predicted in writing before its run, at b9cd7b00 through the same overlay, every one as
predicted:

- the write's gate removed, F1: red, that case alone, "an INCOMPLETE not written: recorded
  false, its write unfinished, the record reads PENDING, … (context deadline exceeded); want
  …", the sighting;
- the helper timing the answer again, gates kept, F1: red at "answered 4.303536311s after it
  began; its bound is 3.3s" alone;
- the read-back's gate removed, F2: all three red at "the record was not read back (the
  read-back did not end within its bound and was given up)" and at each case's line;
- the gates scoped to a snapshot no verification makes: all three red at once, "… before it
  came to the step held at its gate: the case held nothing";
- the same, the helper given no gate: all three red at `requireGatesHeld`'s two lines alone,
  "no gate held its write of INCOMPLETE: it kept its bound of a second" and the read-back's;
- the check's grace held 4 s (`SetCheckGraceForTest`), the fix untouched: all three red at
  "came to the step held at its gate 4.30…s after it began; its bound is 3.3s" alone, so the
  bound still times the give-up;
- the sibling's write gate removed, F3: red at 471 and 526 (b9cd7b00's lines), as at the base.

### The siblings

Every test with the case's setup, a 300 ms deadline, the check parked on its thread
(`SetOnLowPriorityForTest`) and `awaitParkedAndGivenUp`:

- `TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries/a_retry_given_up` and
  `TestARecheckGivenUpLeavesTheVetoItMade` cannot race on these steps. Their record holds a
  VERIFIED, which an INCOMPLETE's merge keeps (`mergeVerdict`, `verify_record.go` 114-121), so
  the write writes nothing whether it answers or is given up (314-324), and removes no veto for
  an INCOMPLETE (`earnsClear`, 152-155); `Recorded` is false for `WriteUnchanged` and
  `WriteUnfinished` alike; neither reads the record the read-back returns (the retry asserts
  `Found`, set before the write); and each reads the veto and the readers only once the holds
  have ended (`awaitClean`), which waits for any step given up.
- `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` raced (above): gated as the case.
- `givenUpCheckOf` (`restore_source_test.go`) and the restore tests over it assert the check's
  give-up alone; a restore it holds refuses by the mark the check's give-up raised and the hold
  the parked check keeps, whatever the write does.

### Left as they are

- Each of these leaves the read of the snapshot's record, before the check, to its bound, the
  deadline and half of `StopWriteBound` from the lock (`finish.go` 461, 527), 1.3 s here, and a
  recheck's veto step likewise: a host slower than that gives the hold up before the check,
  which then never parks; the case fails at once on it, at "before it came to the step held at
  its gate". Held at a gate, that step would come before the arrival the bound times.
- `TestAVerificationPastItsDeadlineReturnsWithItsCallerStillWaiting/worker_answer` reads the
  INCOMPLETE once the verification has answered, under a bound (the deadline, the check's
  grace and one second) that is its claim, and `TestSetAsideRemovalFinishesAfterGiveUp`
  requires a read-back within its second.
- In `cmd/bridge`, `TestTheTickerSaysWhatAGivenUpVerificationsRecordReads`, which B910's
  record names, still leaves its write of INCOMPLETE and its read-back to their seconds, so its
  `over a PENDING` and `whose INCOMPLETE is not written` cases have the sighting's race; and
  `TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads` leaves the record's read
  before the check to its bound, as above. Neither is this change's.

### Rules

`.claude/rules/build-ci-tests.md` gains a line after B910's and bridge lows 6's, and
`ops/History-rules-build-ci-tests.md` its long form: a case that holds a verification's later
steps at their gates times its bound to the arrival at the first of them, never to the answer.

### Runs

On b9cd7b00, `-race` with the release tags, the build slot held throughout:

- the brief's command, `-count=200` of `TestAGivenUpVerificationReadsItsRecordBack` with
  `-timeout 30m`: 200 of 200 runs, 600 subtests, green in 1,485 s;
- `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` x50 from a prebuilt race binary: 50
  of 50 in 350 s;
- under CPU load, 16 busy loops beside four lanes of the case x50 and two of the sibling x25,
  each at GOMAXPROCS 2 (a load average near 19.5 on 16 CPUs): 200 of 200 case runs, the lanes
  644-646 s, and 50 of 50 sibling runs, 640-642 s, no failure.

`go vet` of the package for windows and darwin with the release tags is clean. The gate on
this record's own commit (`make fmt vet test build-all`, `make docs-check`) is in the
branch's report: a record cannot hold the runs made on its own commit.

### Round 2

Commit 22e4ee37 changes `internal/backup/seams.go` and the two test files; the commit after
it holds this section and the rule's edit. Round 1's gate on 98f0aefc was green: `make fmt`
changed nothing, then `make vet`, `make docs-check`, `make test` (P=8, `-timeout 45m`, 91
packages in 1,780 s, cmd/bridge 1,741 s) and `make build-all`.

What the round fixes. At 98f0aefc the helper timed the arrival by `time.Since(start)` in
`within` (`verify_stuck_test.go` 236-240) when the test's goroutine received the gate's entry,
at its gate arm (246-248) or at the answer arm's look at the gate (262-265): when the test saw
the worker at its gate, not when the worker came. A host that keeps the test's goroutine off
the CPU past the bound reads an arrival in time as late, the flake class this PR removes.

Where the time is taken. `StepGate` and `SetStepGateForTest` live in
`internal/backup/seams.go`, a production file of package `backup`, not a test file, and the
round changes it only to record the first arrival's time. `gateOf` (532), which `runStep`
calls on the holder's goroutine with the hold's lock held (`hold.go` 474), sets `stepGate.at`
(452) from `time.Now()` inside the entry's `sync.Once`, before it closes `entered` (539-540);
`StepGate.EnteredAt` (515) reads it only once `entered` is closed, the receive from the closed
channel ordering the read after the write. `gateOf` reaches that code only where a test has
installed a gate (`stepGateForTest`, nil in production), so production does nothing new.
`Entered` stays a closed broadcast, which the helper and `requireGatesHeld` both read; a time
sent on a channel would have held the worker under its hold's lock.

The helper's signature is now (`verify_stuck_test.go` 235):

`awaitParkedAndGivenUpAt(t *testing.T, entered <-chan struct{}, gate *backup.StepGate, step string, done <-chan T, read func(T) (backup.Verification, error), start time.Time, bound time.Duration) T`

`within` takes the measured duration: for a gated case the worker's `EnteredAt` minus
`start`, at either arm (258); for the nil-gate path `time.Since(start)` as the answer is
received (271). `awaitParkedAndGivenUp`'s signature is unchanged. Every call site, by grep:
the gated two,
`TestAGivenUpVerificationReadsItsRecordBack` (`verify_hold_test.go` 860) and
`TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` (`verify_stuck_test.go` 483), pass the
gate and `stepOfIncomplete`; the nil-gate three are unchanged:
`TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries/a_retry_given_up`
(`verify_hold_test.go` 492), `TestARecheckGivenUpLeavesTheVetoItMade` (592) and
`givenUpCheckOf` (`restore_source_test.go` 210), which
`TestARestoreRefusesASnapshotAGivenUpCheckHolds`,
`TestARestoreRefusedByTheSpellingItIsGivenResolvesNothing` and the two hold-given-up cases of
`TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven` call. Its closures mark themselves
helpers, so a failure names the caller's line.

### Round 2's controls

Temporary mutations, laid over the two test files by `go test -overlay` and never in the
tree, so nothing was left to restore; each predicted in writing before its run. A scratch
generator writes each control's overlay and runs, `seams.go` as committed:

`GOTOOLCHAIN=go1.27.3 go test -race -count=1 -v -overlay <the control's overlay.json> -tags ts_omit_webclient,ts_omit_debugeventbus -run '^(TestAGivenUpVerificationReadsItsRecordBack|TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds)$' -timeout 30m ./internal/backup/`

Each hook is joined onto one existing line, so every reported line is the file's own:

- the hold: `time.Sleep(bound + time.Second)` before the helper's loop, so the test's
  goroutine reaches either arm 4.3 s in, past the 3.3 s bound;
- the gate arm forced: the answer's case is a nil channel until the gate arm has fired; the
  answer arm forced: the outer gate arm's channel is nil, so only the answer arm's look at the
  gate sees the arrival; each forced arm logs its name;
- the premise: each gated case reads the worker's own time, `gate.EnteredAt(stepOfIncomplete)`
  minus `start`, logs it, and fails where it is past the bound.

What they did:

- 98f0aefc's helper, the hold, the gate arm forced: red in all four runs (the case's three
  subtests and the sibling), "came to the step held at its gate 4.302…s after it began; its
  bound is 3.3s", each run's forced arm logged and its premise 1.301-1.302 s, in time by the
  worker's own time. With the answer arm forced, the same.
- 22e4ee37's helper, the same hooks: green on each arm, four of four, the premise 1.301-1.302 s.
- An arrival late in fact, the check's grace held 4 s (`SetCheckGraceForTest`) and no hold,
  each arm forced: red on both arms at "came to the step held at its gate 4.304…s after it
  began".
- The nil-gate path, `TestARecheckGivenUpLeavesTheVetoItMade`: the grace held 4 s, red at
  "answered 4.306…s after it began" with either helper; the hold alone, red at "answered
  4.302…s": the nil-gate path still times the answer as the test receives it, which this round
  keeps. The new helper names the caller's line (`verify_hold_test.go` 592), the old its own
  (`verify_stuck_test.go` 238).
- Round 1's claims under the new helper: the INCOMPLETE's staging stalled 3 s past its second,
  green; the write's gate removed, red with the sighting's words; the gates scoped to a snapshot
  no verification makes, red at once.

### Round 2's runs

On 22e4ee37, inside the build slot, one race binary built with `GOTOOLCHAIN=go1.27.3` and the
release tags, every case that reaches the helper run 200 times without CPU load and 200 times
beside 16 busy loops, in shards, each shard its own invocation under `-test.timeout 30m`.
Durations below are each shard's own, from its log's last write.

- Without load, ten shards side by side: `TestAGivenUpVerificationReadsItsRecordBack` 2 x 100
  (793 s each), `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` 2 x 100 (736 s),
  `TestARecheckGivenUpLeavesTheVetoItMade` x200 (913 s), its `a_retry_given_up` case of
  `TestAVetoThatStoodBeforeAVerificationStaysOverTheVERIFIEDItCarries` x200 (909 s),
  `TestARestoreRefusesASnapshotAGivenUpCheckHolds` x200 (720 s),
  `TestARestoreRefusedByTheSpellingItIsGivenResolvesNothing` x200 (720 s), and the two
  hold-given-up cases of `TestEveryAdmissionRefusalNamesTheSnapshotAsItWasGiven` 2 x 100
  (757 s): every one of the 1,400 runs passed (600 subtests of the case, 400 of the admission
  cases, 200 retries), none skipped.
- Beside 16 busy loops, each shard at GOMAXPROCS 2: the case 4 x 50 (658-667 s) and the
  sibling 4 x 50 (1,175-1,234 s) side by side, a load average near 23: 400 of 400 passed;
  then twelve shards side by side, a load average near 28: the two restore tests 2 x 100 each
  (744-751 s) and the admission cases 4 x 50 (770-795 s), 600 of 600 passed. In that wave two
  shards of 100 did not fit their timeout: the recheck's second (98 runs passed, its 99th 4 s
  in) and the retry's first (97 passed, its 98th 16 s in) each ended at
  `panic: test timed out after 30m0s`, with no assertion failed in either; the other recheck
  and retry shards of 100 passed, 1,642 s and 1,714 s.
- The loaded recheck and retry again, 200 each, as shards of 50 beside 16 busy loops, eight
  shards side by side at a load average near 22: 400 of 400 passed (the 200 retries' subtests
  too), each shard within its timeout (840-922 s). With the first attempt's passing shards, the
  loaded runs of every case passed 200 of 200 in shards that fit.

`go vet` of `internal/backup` for linux and windows with the release tags under
`GOTOOLCHAIN=go1.27.3` is clean. The gate on this section's own commit is in the branch's
report.

### Left as it is after round 2

The nil-gate path's answer is still timed where the test receives it: a host that keeps the
test's goroutine off the CPU past the bound would still fail its three callers, as the bounds
`requireReturnedWithin` takes would. Timing that answer where its caller's goroutine has it is
a change of its own.
