## 2026-11-19 — Bridge lows 6: the backup case judges its step as it stood at the answer, and the connection registry clears the slots its prune frees

Branch `fix/bridge-lows6-1119`, from main `037f838c` (#1512's merge). Three commits: `0ba9b389`,
item 1, the backup case's wait before its gate (test code alone), with its rule line and long
form; `52ee6d46`, item 2, the connection registry's prune (`internal/sqlitetest`, which tests
alone import), with its rule line, its long form and two sentences of the park's long form; and
this record. Both findings are #1512's review's. No production code changes. Line numbers are
`037f838c`'s where the old code is cited.

### Item 1: the step judged as it stood at the answer

What was true. `reachedBeforeTheAnswer` (`cmd/bridge/backup_round4_test.go` 343-356), the wait
`TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads` makes before it fires a step's
gate in its two cases that hold a step (`whose write did not finish`, at the write's rename,
`landed`; `whose record was not read back`, at the read-back's park, `parked`), selected on the
seam, the answer and a minute, and where it took the answer it read the seam again (348-352): a
seam closed by then passed. So an answer before the step passed wherever the seam had closed by
that read, as it does where a worker the verification abandoned reaches the step after the
answer, and with both channels closed when the helper looked Go's `select` took either arm, and
both passed. The case could not reliably catch the regression it guards, a verification that
answers before its write's step.

As built. The case chooses the seam it holds, that step's gate and its words before the command
starts (`held`, `heldStep`, `heldWhat`; none in its other three cases), and the command's
goroutine defers `noteReached(held, &heldAtAnswer)` after `defer close(answered)`, so the note
is taken as `backupCmdContext` returns and before the answer closes; `noteReached` stores
`closedNow(reached)` into an `atomic.Bool`. The helper takes `t testing.TB` and the note. It
selects on the seam, the answer and the minute as before, and returns at once where the seam
came with no answer in: the command answers only once the case fires the step's gate, after the
helper returns, so a helper that waited there for the answer would hold the case until the
verification's caller gave its worker up (K3f, below). Wherever the answer is in as it looks
(`closedNow(answered)`), whichever arm its select took, it fails unless the note says the seam
had closed. After its join the case reads the note again, which catches an answer published
after the helper took the seam with no answer in.

One departure from the brief's binding review, which has the helper read the note only when its
select takes the answer: it reads it wherever the answer is in as it looks, after either arm.
With both channels closed, a helper that read the note on the answer's arm alone passed the
regression in about half its runs (K4, below), and the review's red, the seam closed before the
call, needs a deterministic answer.

The unit case. `TestReachedBeforeTheAnswerJudgesTheStepAsItStoodAtTheAnswer` makes each case's
events in turn ("step" closes the seam; "answer" calls `noteReached` with the seam as it stands
and closes the answer, as the command's goroutine does), then runs the helper on a goroutine of
its own through `fatalRecorder`, a `testing.TB` whose `Helper` does nothing and whose `Fatalf`
records the message and ends its goroutine by `runtime.Goexit`, joined within 10 s, its drain
registered (`drainLoopOnCleanup`, which `TestEveryBackgroundGoroutineDrainsOnCleanup` requires
of every launch that closes a channel on its way out). The step then the answer, and the step
with no answer, pass; the answer then the step, and the answer with no step, fail with "bridge
backup --verify answered before the step".

Red first. The unit case over the helper's old body under the new signature (`t testing.TB`, the
note taken and never read), laid over the file by `go test -overlay`: `the answer, then the
step` red 20 of 20, the helper failing nothing where the case wanted "bridge backup --verify
answered before the step"; the other three cases green 20 of 20. At `0ba9b389` all four are
green 20 of 20.

Controls, each predicted in writing before it ran, each an exact-once mutation of
`backup_round4_test.go` laid over the tree by `go test -overlay`, the tree never edited:

| | mutation | predicted | seen |
|---|---|---|---|
| K1 | the note's read replaced by a fresh read of the seam | the answer then the step red 20 of 20, as the old body; the rest green | as predicted |
| K2 | `noteReached` notes false | unit: the step then the answer red; backup test: its two step cases red at the check after the join; its other three green | unit 3 of 3; each step case 3 of 3, "bridge backup --verify answered before the write's rename, which came after it" and "… before the read-back's park, which came after it"; the rest green |
| K2b | K2, and the check after the join removed | the backup test green: its helper took the seam with no answer in and read no note | as predicted, all five cases |
| K3 | the helper waits for the answer once the seam has come | unit: the step with no answer red at the 10 s join | as predicted; the run took 68.6 s, its drain waiting out the helper's minute |
| K3f | K3, in the backup test | its two step cases red at about 21-23 s, the verification's caller answering without its worker, at "bridge backup --verify = 1, want 1 and its verification given up" | both at 21.7 s, the command saying "the verification was left to end on its own 2s after its deadline"; `whose write did not finish` also said "no gate held its read of the record after it", which the prediction left out |
| K4 | the note read only on the select's answer arm | the answer then the step red in some runs and green in others, never 0 or 20 of 20 | red 11 of 20 |

### Item 2: the prune clears the slots it frees

What was true. `trackConnection` (`internal/sqlitetest/sqlitetest.go` 359-386) dropped from each
file's list the connections closed or interrupted since by filtering into the list's own start
(`kept := list[:0]`, 372-377) and storing the shorter slice: the slots past its length kept the
dropped connections' `*tracked`, each holding its driver connection, until an append to that
file's list wrote over them. An open of another file never does, so they stayed for as long as
the file's list kept anything: memory of the test process alone.

As built. `slices.DeleteFunc(list, (*tracked).gone)` asks `gone` of each connection once and in
order, as the filter did (`IndexFunc` up to the first it drops, then one call for each after
it), and clears the slots it frees: go1.27.3 `src/slices/slices.go` 241-255, `clear(s[i:])` at
253. The registry's lock, `gone`'s `TryLock` and `liveConns`' copy are unchanged; the hook's doc
says it.

The case. `TestThePruneClearsTheSlotsItDrops` opens two connections to one database through the
package's real open path (`openWAL`, a `Ping` each), the list's array then two slots wide;
closes the second; opens another database, whose prune runs over the first list and appends
nothing to it; and, under the registry's lock, clones the list and the slots past its length to
its capacity: the open connection kept alone, and nil past it. A list with no slot past what it
keeps fails the case on its premise, a prune that no longer compacts in place. Its cleanup,
registered first, removes both files' entries from the registry, under its lock, once its
connections have closed.

Red first: the case over the old filter (`go test -overlay`), red 20 of 20, "slot 1, past the
list's length, still holds a connection (the closed one: true)"; at `52ee6d46`, green 20 of 20.
Controls, predicted first, each as predicted: M1, a prune that drops nothing, red 3 of 3 at "the
prune kept 2 connections for … , want the open one alone", and
`TestOpeningAConnectionWaitsOnNoOtherConnectionsLock` red 3 of 3 at "1 connections recorded for
… once its one was closed, want 0"; M2, a prune that keeps a fresh copy of what it keeps, red at
its premise.

What it changes in B909's account. B909's second round controlled the look of
`TestCancelsLookWaitsOnAConnectionAndHoldsUpNoOpen` with L3, a look that reads the registry's own
slices once its lock is let go, red at "Cancel's look found 3 connections open and not
interrupted, want 2": the other database's open compacted the list of three, its second closed,
to the first and the third, and left the third in the third slot too. With the slots cleared,
that look reads a nil there: L3 over `52ee6d46` ends the test binary with a nil pointer
dereference in `(*tracked).live`, called from `liveConns`, as predicted; over the old filter
(L3u) it is red at the count again, 3 for 2. L3u's first run did not build (both of the
mutated file's uses of `slices` gone), so it was run again with the look's outer slice clipped
(`slices.Clip`), which copies nothing. That round's record
(`2026-11-18-b909-b910--fix-bridge-flakes-1118.md`) stays as written; the park's long form in
`ops/History-rules-build-ci-tests.md` now says both.

### Rules

`.claude/rules/build-ci-tests.md` gains two lines, after B910's and after B909's, and
`ops/History-rules-build-ci-tests.md` their long forms in sections of their own, each in its
fix's commit; B909's long form says what a look over the registry's own slices reads now.

### Runs

At `52ee6d46`, on the pinned toolchain (`GOTOOLCHAIN=go1.27.3`), the release tags
(`ts_omit_webclient,ts_omit_debugeventbus`) and `-timeout 30m`: cmd/bridge's two tests,
`TestReachedBeforeTheAnswerJudgesTheStepAsItStoodAtTheAnswer` and
`TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads`, `-race -count=20`, 220 passes
of 220 (the two tests and their nine cases, each 20 times), none skipped, 257.7 s; the whole of
`internal/sqlitetest` `-race -count=20`, 480 of 480, none skipped, 59.1 s. The gate on the
branch (`make fmt vet docs-check`, `go vet` of both packages for windows and darwin, and `make
fmt vet test build-all`) is in its report: a record holds no run made on its own commit.
