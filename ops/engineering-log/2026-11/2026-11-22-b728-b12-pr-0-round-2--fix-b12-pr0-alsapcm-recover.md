## 2026-11-22 — B728 B12 PR 0 round 2: the two containment paths the cases missed, the delivered case's order, a cancellation arm's failed close, and the records

Branch `fix/b12-pr0-alsapcm-recover` (PR #1547), round 1's head `d97bfe6b`, base `a14a26db`.
Round 1's entry is `2026-11-22-b728-b12-pr-0--fix-b12-pr0-alsapcm-recover.md`; this one records
round 2 alone. Its items are numbered as its brief numbers them, 1 to 7, beside Z1 to Z5, the
binding review folded into that brief, which win where they differ; each was checked against
`d97bfe6b` by line.

### What was built

43 production lines added (30 code, 13 comments) and 19 removed, by `git diff -U0 d97bfe6b` over
the non-test Go files; the qualification version stays 57 (the tool: "qualification version OK:
a14a26db..HEAD").

- **A refused reopen's failed close is its cleanup's** (item 1, `sink_dsd_writer.go`): a reopen
  whose setup `openNative` refused, its node's close then failing or panicking, answers that failure
  beside `ErrReleaseUnconfirmed` and carries no panic. `activateNative` returned it and `loopNative`
  joined it into `s.err` alone, so `Close`, which answers `CleanupResult`, answered a clean cleanup,
  and the backend's `closeOutput`, which fences on that answer, let a successor open the device while
  the node's close was unknown. An ordinary close error on the path had the same gap before this PR.
  `activateNative` now joins an open's error that carries `ErrReleaseUnconfirmed` into `cleanupErr`,
  under the sink's lock.
- **The node is closed past an ioctl's panic** (item 2 and Z1, `device_native.go`, `sink_dsd.go`):
  `releaseNative` marked the node closed before DROP and HW_FREE, so a panic in either skipped the
  close and kept the descriptor open, the mark keeping any later close from trying. The close now
  runs from a defer registered right after the mark, once, whatever the ioctls do. Given
  `releaseRecovered`'s callback (`recovered`, variadic, so the one test that calls `releaseNative`
  itself, `TestNativeIdlePauseFence`, compiles as it was), a panic of the ioctls and one of the
  close each answer as their failure through `nativePanicked`, logged once with its own stack,
  joined beside the other's. A deferred close under round 1's one recover, `releaseRecovered`'s,
  would have kept only the last of two panics, a close's made while an ioctl's was in flight, and
  lost a close's error to the ioctl's panic (Z1). `releaseRecovered` keeps no recover of its own:
  the two in `releaseNative` cover every panic there. A scratch program checked the panic semantics
  first: each shape closes the node once, and an outer setup panic in flight (`openNative`'s) stays
  in flight past an idle recover and past a nested panic recovered inside, which its own recover
  then finds.
- **The delivered sink's case waits for its opener's end** (item 4, `sink_dsd_open.go`):
  `nativeSeams.openEnded`, nil but in a test, runs on the opening worker once its recover has run (a
  defer registered after `close(done)`'s and before the recover's). The case after the hand-off
  waited for the panic's log line, which `nativePanicked` writes before the recover's guard of the
  sink it delivered, so with that guard dropped (C6) the case's activation and release could finish
  before the revocation came; it now waits for `openEnded` before it uses the sink.
- **A cancellation arm whose close fails** (item 5): a case of its own. Only the clean case reached
  the recover's revocation in the cancellation arm, and its completion asserts a confirmed cleanup,
  so deleting the line that joins the revoked sink's result (`sink_dsd_open.go:54` at `d97bfe6b`)
  passed every case.
- **The panic's line** (Z5): `nativePanicMessage` says only "player: a panic was recovered in the
  native DSD sink". Its "it ends typed and its node is closed" was not true of a delivered sink that
  plays on, nor of a close that panicked; the backend tests' copy reads the same words.

The cases, each in a child process of its test (`RunEachCaseInAChild`), 32 in all:
`TestANativeSinksCleanupThatPanicsStaysInThePlayer` gains five (a release whose DROP panics, whose
HW_FREE panics, whose DROP panics with its close failing and with its close panicking, the last
logging two lines; a reopen's setup refused, its close panicking), each with the node closed once
(`checkClosedEach`) and the cleanup unconfirmed; `TestANativeOpenThatPanicsStaysInThePlayer` gains
one (the cancellation arm, its sink made, its close failing: `CompletionResult` carries
`ErrPanicked`, `ErrReleaseUnconfirmed` and the close's failure);
`TestABackendSurvivesAPanicOnANativeSinksGoroutines` gains one (a reopen's setup refused, its close
panicking, through the engine's `Run` and the real native sink: the session faults, its release is
unconfirmed, and a later play opens nothing). The panic in DROP or HW_FREE comes from a test wrapper
of the fake's node (`ioctlPanic`), a refused setup from `FakeKernel.RefuseNextSetUp` (a commit of one
channel, which the read-back refuses once the hardware is set).

### Item 3, stopped by the brief's exit

The cancellation arm's control, C8 (the recover's wake of the writer dropped), can fail only when the
writer is idle at its `!active` select before the opening worker's revocation panics: a writer that
reads the sink's state after the revocation marked its release begun ends without the wake. The
brief asked for a seam that holds the writer idle there. No seam on the writer orders it: the worker
revokes the sink the moment `openDSDSink` returns, with no point it waits at, and the revocation's
panic comes under the sink's lock, which the writer's read of its state takes, so a hold there would
deadlock a writer not yet past that read. A red on every run needs a second hook, on the worker
before its cancellation arm's revocation, beside the writer's: a mechanism beyond the lines the brief
named, where its exit stops. Nothing was added for it. At `2e1df70c`, C8's case run alone 60 times
under `-race` was red 18 times, each "waited 5s for the opening worker's end", and HEAD's green 60 of
60, as in round 1.

### Red first

The round's tests over `d97bfe6b`'s production files, laid by `go test -overlay` with a names-only
shim joined onto existing lines (the `openEnded` field and its call, the delivered case's hook alone,
and the panic line's new words, which the backend tests' copy reads), the predictions written first:
the six predicted cases red, each by assertion, never by death, and every other case green.

- A release whose DROP panics, whose HW_FREE panics, and whose DROP panics with its close failing
  and with its close panicking: "the node was opened 1 times and closed 0 (open now: true)"; beside
  it the close's failure missing from the cleanup, and one line of a recovered panic where two are
  wanted.
- A reopen's setup refused, its close panicking: "the cleanup is <nil>".
- The backend's case: "waited 10s for stopped, the release unconfirmed" (the release confirmed, the
  device not fenced).

The new cancellation-arm case and the delivered case, its wait shimmed, were green there, as
predicted: item 5 is coverage, and item 4 an order of the test's, each shown by its control.

### Controls

Each predicted in writing first, case by case (green, died, red by assertion), an exact-once
mutation of `2e1df70c`'s file laid by `go test -overlay`, the four tests run once (`-watchdog 10s`),
no tracked file touched; all six went as predicted and nowhere else.

- C6, the hand-off's guard dropped from the recover's revocation (`if s != nil && !sent` to `if s !=
  nil`): after the hand-off red by assertion, the delivered sink revoked. Alone under `-race`, red 60
  times of 60; HEAD's case green 60 of 60.
- C54, the line joining the revoked sink's result deleted alone (`sink_dsd_open.go:54` at
  `d97bfe6b`, `:57` now): the cancellation arm with its close failing red by assertion, alone under
  `-race` 20 times of 20; the clean cancellation-arm case green.
- R2C2, the node's close called after the ioctls rather than deferred, the recovers kept: the four
  ioctl cases red by assertion, the node closed 0 times.
- R2C3, the ioctls' recover removed: the four ioctl cases died, the panic escaping the writer once
  the deferred close had run.
- R2C4, the close's own recover removed: four cases died (the close at a release, after the writer
  panicked, after DROP panicked, and the backend's release); eleven red by assertion, each whose
  close's panic another recover then caught as its own (the writer's or the opener's), its line or
  its cleanup not the close's: the pause's release, the four reopen refusals, a reopen's setup with
  its close panicking, a refused reopen's, the check after open and its close, the kernel's setup
  and its close, a refusal's close, and the backend's refused reopen.
- C8 for the record (item 3): both cancellation-arm cases green in the batch, as predicted for an
  order the case cannot force; its count is under item 3.

### The records

- `.claude/rules/output.md`: the B12 PR 0 line says each close of the node, DROP and HW_FREE
  included, closes it once whatever they do, and that a refused reopen's failed close is
  `CleanupResult`'s, with two Don'ts for them and the residuals' entry; its long form in
  `ops/History-rules-output.md` says the same and adds round 2's two paths, and no longer says a
  panic on these goroutines ended the process whole (Z3): the writer's own was already recovered,
  untyped and unlogged, and a close's or an opener's ended it.
- `output.md` line 44's pointer to the MPD gate's long form ended "**Don't dial from `/v1/health`:**
  →", its colon stranded (Z4, by hand, `move_entry.py` refusing a line that is a pointer already):
  it ends with a full stop, and its long form's bold reads the same, its explanation a sentence after.
- `ops/b470-native-qualification.md`: the version 57 paragraph says the same of what came before,
  and adds round 2's paths; the version stays 57.
- Round 1's entry, corrected: its hazard's opening said a panic on either goroutine ended the process
  (the writer's own was recovered already, its fourth bullet said); alsapcm's external test package
  where it said the backend's; and its residuals named by their backlog entry alone (Z2).
- `verify_moves.py --base 2e1df70c`, with the line 44 pointer, its long form and the runbook's
  paragraph named by `--edit`: those three shown as declared, 0 citations broken; the B12 PR 0 line
  and its long form, one block each (a list item and its paragraphs), open alike, so no `--edit`
  names one apart from the other: their two MISSING and two new blocks were compared word by word,
  each change the one intended.

### Runs

- Every child-process case alone by its name under `-race` (`-test.run=^<test>$/^<case>$` on one
  prebuilt test binary): 32 cases, each passing alone, no other case run.
- One slot session at `95b2c7c2` (go1.27.3, the release tags), 4,478 s, the tree clean after it:
  - `make fmt` rewrote nothing; `make vet` clean, and `go vet ./...` clean with `GOOS=darwin` and
    with `GOOS=windows`.
  - alsapcm and its importers but `cmd/bridge`, `go test -race -count=20 -timeout 150m
    ./internal/alsapcm ./internal/player/binding ./internal/player/playback/...
    ./internal/player/policy ./internal/player/engine ./internal/player/hotplug ./internal/config`:
    ok, alsapcm 2,301.2 s, binding 42.9 s, playback 52.4 s, policy 5.9 s, engine 658.3 s, hotplug
    29.6 s, config 324.3 s. `cmd/bridge`, which imports alsapcm too, ran once beside it under
    `-race` (1,716.9 s) and once more in `make test`: twenty runs of it, near half an hour each,
    would have gone past the round's four hours.
  - The native-integration overlays, as `gate.yml`'s two steps: `./cmd/bridge` 16 passes and
    `./internal/player/playback` 33, none skipped, every name the workflow checks passing.
  - `make test` (P=4, `TEST_PKGS='-count=1 -timeout 45m ./...'`): ok in 2,087.8 s, 92 packages and
    none failed (cmd/bridge 1,731.9 s, internal/manifest 1,594.6 s, internal/admin 1,239.6 s,
    internal/backup 657.9 s, internal/alsapcm 117.4 s); `make build-all` built the release targets
    (25.2 s); `make docs-check` ok.
- Outside the slot: the qualification tool, `go run -tags ts_omit_webclient,ts_omit_debugeventbus
  ./scripts/qualification-version -base a14a26db -head HEAD`, "qualification version OK:
  a14a26db..HEAD" (version 57, raised no further); the citation guard, `TestEveryCitedTestNameExists`,
  ok before the session and again with this entry staged; and `make docs-check` again over it. The
  pinned gofmt (`go run cmd/gofmt -l`, go1.27.3) lists none of the round's Go files.

### What stays open

The residuals are backlog B987. Item 3's second hook is for the owner to decide.
