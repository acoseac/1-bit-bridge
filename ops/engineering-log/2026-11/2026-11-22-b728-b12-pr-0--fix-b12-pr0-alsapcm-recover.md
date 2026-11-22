## 2026-11-22 — B728 B12 PR 0: a panic on the native sink's writer or opener stays in the player

Branch `fix/b12-pr0-alsapcm-recover`, from main `a14a26db` (the base, recorded: later than the
brief's `afe11f29`, main having taken B12 PR 1, B972, B976 and Bridge lows 13 to 15 since; between
the two, of the files this PR touches or cites, only a test of B972's, the qualification version,
the runbook, the design and `output.md` changed, so every production line the brief cites stood
where it said). B12 is the bridge player's optional DSD listening check
(`ops/plan-b728-dsd-capability-design.md` §6.2 and §6.3); this PR comes before its PR 3 and adds no
check code: it hardens `internal/alsapcm`'s native DSD sink so that a panic on its goroutines is
contained, as the PCM sink's has been since B523. R1 to R6 and K1 to K6 below are the items of the
two binding reviews folded into its brief; C1 to C5 the brief's controls, and C6 to C10 this
entry's own.

### The hazard

At the base a panic on either of the native sink's goroutines ended the process, and with it all of
`bridge serve` (API, console, scanner, player) until its supervisor restarted it 2 s later
(`internal/packaging/systemd.service.tmpl:10-11`):

- The writer recovered before it closed: `loopNative`'s one deferred function asked `recover()` at
  `sink_dsd_writer.go:14`, then `s.closeNative()` at `:18`, so a panic in that close (`:486-499`;
  `releaseNative`, `device_native.go:61-71`) was a new panic in a deferred call that nothing
  recovered, `close(s.done)` (`:41`) never running. The red run's traces name
  `(*DSDSink).loopNative.func1` at `sink_dsd_writer.go:18` and the close at `:492`.
- The opening worker had no recover: its goroutine (`sink_dsd_open.go:37-50`) deferred only
  `close(done)` while it opens the node, reads its setup and runs the caller's check after open
  (`sink_dsd.go:270-286`; in serve the backend's `out.admit`, the DoP gain check included). The
  traces name `openNativeBounded.func1` at `sink_dsd_open.go:37` and `:39`, the check at
  `sink_dsd.go:284`, the setup at `device_native.go:46`.
- Both goroutines are alsapcm's, so no caller's recover could catch them. Every native DSD or DoP
  session in serve reaches them (the default opener is `alsapcm.OpenDSDSink`, `backend.go:609-611`,
  and serve sets no other, `cmd/bridge/serve_player.go:504-509`), and the harness CLIs; the
  console's test tone (PCM, its own recovers) and the silent probe do not.
- Two gaps beside them: the writer's own panic was untyped and unlogged (`sink_dsd_writer.go:15`,
  `native writer panic: %T`), so the backend faulted it `device_error` (the red run: "faults
  [{Reason:device_error Detail:native writer panic: string …}]"); and the native open closed its
  node at each refusal (`sink_dsd.go:275-299`) but not on a panic before the hand-off, while
  `openNative`'s own deferred close (`device_native.go:41-45`) lost a cleanup error to a setup's
  panic, and a close that panicked there replaced the setup's panic.

Severity Medium: the whole bridge goes down if one fires, at every DSD open if its trigger is
deterministic; no known input fires one (the readers most exposed to device data run first, under
the policy refresh's recover), and PR 3 widens the exposure.

### What was built

139 production lines, code and comments (110 and 29), 17 removed, counted from `git diff -U0
a14a26db` over the non-test Go files, the version constant's one apart; within the brief's 140.

- **The writer's own panic, typed** (`sink_dsd_writer.go`): its recover answers `nativePanicked`,
  an `*Error` typed `ReasonPanicked` that names no value (`Err` is `ErrPanicked`, a new sentinel in
  every error such a panic answers), logged once through `DSDConfig.Log` (new; nil is
  `logging.Component("player")`) with where (`the native sink's writer`), the endpoint and the
  stack, the value only for a `runtime.Error`. The backend's `faultReason` already maps
  `ReasonPanicked` to `backend_failed`. An activation the panic cut short (`running`, the request
  under way, set around `activateNative`) is answered the record, the panic and any cleanup's
  failure, as a fault: never `answerNeverRan` with `errNativeActivationCancelled`, the path for one
  that never ran (K5). The sink is revoked by `setNativeError`'s Immediate release.
- **Every close of the node under a recover of its own** (`releaseRecovered`, `sink_dsd.go`): a
  close that panics answers as its failure, logged once, never replacing a panic it runs after.
  `closeNative` takes it inside (K1, R3), so each of its callers has it: the writer's end, a pause's
  release, and the four refusals of a reopen (DoP precision, geometry, the check after open, the
  fence). `s.dev` is nil before the close, which is never tried again; the failure is recorded in
  `cleanupErr` beside `ErrReleaseUnconfirmed`, and every caller joins `ErrReleaseUnconfirmed` to the
  `*Error` it is answered, as it joins it to a close's error.
- **The opener's one recover** (`sink_dsd_open.go`, K3): `openNativeBounded`'s goroutine recovers a
  panic anywhere on it and answers `ReasonPanicked` (`the native sink's opener`) beside its node's
  close's failure, through `select { case reply <- …: case <-ctx.Done(): }`, never a `default:`, into
  `completed` either way (`CompletionResult`), skipped once the reply has handed the sink over
  (`sent`; `nativeSeams.delivered`, a test's seam, runs there). A sink made and not delivered (the
  cancellation arm) is revoked, its writer woken (`interrupt`) past a revocation the panic cut short,
  which can have marked the release begun and woken nothing, waited for, and its result joined,
  before `done` closes.
- **The node the open did not hand over** (`openDSDSink`): one deferred close under
  `releaseRecovered`, on every way out but the hand-off (`handed`, set before the writer starts), in
  place of the six refusals' explicit closes, each refusal's failure joined to its answer as before
  (named results). At a panic it reports the close's failure to the worker
  (`nativeSeams.unhanded`, set by the worker alone), which joins it with `ErrReleaseUnconfirmed`;
  `openDSDSink` never recovers the open's panic itself (K3).
- **The setup's own boundary** (`device_native.go`, R2): `openNative`'s deferred close goes through
  `releaseRecovered`, its logger the native sink's (`Config.log`, unexported, `device.go`); where the
  close failed and the setup panicked, it raises the panic again carrying that failure
  (`setupPanic`), which `nativePanicked` unwraps, so both are kept: the worker answers them on an
  open, and the writer at a reopen answers them and records the close's failure in `CleanupResult`.
- **The backend** (`native.go`): `Log: s.b.log` in its `DSDConfig` literal, as `session.go` passes
  `SinkConfig.Log`; and (R1) after an open whose answer holds `ErrPanicked` and
  `ErrReleaseUnconfirmed`, `releaseErr`, so the teardown sets `terminalUnconfirmed` and nothing opens
  the device until serve restarts. A refusal whose close failed without a panic fences nothing, as
  before (a residual, below).
- **The package doc** (`doc.go`): the native sink keeps the PCM sink's contract.

**One fault name** (K6): the two words are deliberately apart. An open's panic at a native
session's start fails its Start, and the engine faults a native session whose Start failed
`backend_error` (`engine.ReasonBackend`, a backend call that failed; `startDone`); a writer's panic
reaches the engine through the reporter as the sink's `ReasonPanicked`, which the backend maps to its
own fault, `backend_failed` (`playback.ReasonFailed`; `faultReason`). The tests assert each where it
comes.

**Deviations from the brief's shape**, each for a binding item: `device.go` gains the unexported
`Config.log` (R2's close in `openNative` logs its panic through the native sink's logger, and no
other parameter reaches `openNative` without changing a signature the existing tests call);
`ErrPanicked` is exported, so the backend's fence asks `errors.Is` rather than walking the chain;
`nativeSeams` gains `unhanded` (K3's report, since `openDSDSink`'s three-argument signature is what
28 calls in ten existing test files make) and `delivered` (the seam that makes K3's "after the hand-off"
testable); the R4 cases are a test of their own, `TestANativeWritersOwnPanicIsTypedAndLogged`, run
beside the brief's three; and the rig's `unit` gains `panics` (a one-shot panic in its check).

### The tests

Four tests, every case in a child process of its test (`RunEachCaseInAChild`, exported from
`sink_dsd_panic_test.go` for the backend's package): the parent runs `os.Args[0]` again with
`-test.run=^<test>$ -test.v -test.count=1 -test.timeout=2m -watchdog=<its own>` and the case named
in `BRIDGE_B12_PR0_CHILD_CASE`, which the child runs as a subtest; on a `panic:` line it fails "the
panic escaped <the goroutine its trace names> and ended the process" with the output, a timeout as
a timeout, any other failure or no `--- PASS:` with the child's output, and a child's skip as a
skip. It depends on nothing of Linux. The new internal test file has no flag and no `init`, so in
the native-integration overlay it is an ordinary file.

- `TestANativeSinksCleanupThatPanicsStaysInThePlayer` (nine cases): the close at a release; the close
  after the writer panicked (a reopen's check panics, its node's close too, armed after the pause's
  release); the close at a pause's release; the close of a reopen whose DoP precision is refused,
  whose geometry changed, its check refused, its fence refused (a pause in its check moves the
  epoch); a reopen's setup, its close failing, and its close panicking. Each: `Done` closes, the
  cleanup is unconfirmed and typed (the close failing: its error alone), the node closed once
  (`checkClosedEach`), a resume the panic cut short answered its failure and never as one that never
  ran, one log line a panic with its stack and endpoint and no value (two for two panics, K4), and a
  later Pause answers the end and reaches no kernel call.
- `TestANativeWritersOwnPanicIsTypedAndLogged` (R4, three cases): as it holds a pause, the runtime's
  own error there (its words in the line), and a reopen's check, each cleanup confirmed: a typed
  `Result`, a nil `CleanupResult`, one line.
- `TestANativeOpenThatPanicsStaysInThePlayer` (nine cases): the check after open; it and its close;
  the kernel's setup; it and its close failing; it and its close panicking (R2's open cases); a
  refusal's close; after its caller has gone (`Completion`, `CompletionResult`); in the cancellation
  arm, its sink made (the watcher held at `lifeEnded`, the clock's first read the worker's
  revocation); after the hand-off, its sink delivered (the sink plays on, never revoked).
- `TestABackendSurvivesAPanicOnANativeSinksGoroutines` (the DSD rig, four cases): a release whose
  close panics (the release unconfirmed, a later play opens nothing); an open whose check panics
  (the session faults `backend_error`, nothing written, its node closed once, the next play plays
  on); the same with its close failing (R1: the release unconfirmed, a later play opens nothing);
  a writer that panics as it holds a pause (`backend_failed`).

### Red first

The new tests over the base's production files, laid by `go test -overlay` with a names-only shim
joined onto existing lines so the base's line numbers hold (`DSDConfig.Log`,
`nativeSeams.unhanded` and `delivered` read by nothing, `ErrPanicked` in no chain,
`nativePanicMessage` logged by nothing) and, for the hand-off case alone, the `delivered` seam's
call, the test's hook: all 25 cases red in the parent, `-watchdog 10s`.

- Died, the panic escaping the writer (`(*DSDSink).loopNative.func1`): the close at a release
  ("panic: a value the close's panic carries…", the deferred close at `sink_dsd_writer.go:18`), the
  close after the writer panicked (the check's value "[recovered]", then the close's), and the
  backend's release whose close panics.
- Died, the panic escaping the opening worker (`openNativeBounded.func1`): every opener case (the
  check after open, it and its close, the kernel's setup and both of its close cases, a refusal's
  close, after its caller has gone, the cancellation arm, after the hand-off) and both backend open
  cases.
- Red by assertion, the base's writer recover catching what came on the writer: the close at a
  pause's release and the four reopen refusals ("the outcome is native writer panic: string", "the
  resume answered native activation cancelled", "the cleanup is <nil>"); a reopen's setup with its
  close failing and with its close panicking (the cleanup lost: "the cleanup is <nil>"); the three
  writer-only cases ("native writer panic: string", "runtime.boundsError", "the log holds 0 lines");
  and the backend's writer case ("faults [{Reason:device_error …}]"), R4's red by its typing and its
  log, never by death.

At HEAD every case is green.

### Controls

Each predicted in writing first, case by case, in a file written at `81c26b8c` before the batch ran,
an exact-once mutation of that commit's file laid by `go test -overlay`, no tracked file touched (the
tree clean after the batch), the four tests run once each (`-watchdog 10s`), every case's outcome
(green, died, red by assertion) held to the prediction. C1 to C7, K2, C9 and C10 went as predicted
and nowhere else; C8 is the last bullet:

- C1, `closeNative`'s close called outside its recover (`d.releaseNative()`): died, the close at a
  release, the close after the writer panicked and the backend's release; red by assertion, the
  pause's release and the four reopen refusals, whose close now panics inside the writer's loop and
  its recover (the cleanup not recorded, the line the writer's); every other case green.
- C2, the worker's recover removed: died, every opener case but a refusal's close and both backend
  open cases; a refusal's close green, its close's panic recovered by `releaseRecovered` and never
  reaching the worker.
- C3, the panic-time close removed, narrowed to a panic (`if !handed && err != nil`): red by
  assertion, the check after open ("opened 1 times and closed 0"), it and its close, after its
  caller has gone, and both backend open cases ("1 opens, 0 closes, open true"; with its close
  failing, a close never made never fails, and the wait for the release unconfirmed ran out); the
  kernel's setup and its two close cases green (`openNative`'s own close), a refusal's close green.
- C4, a close that panicked recorded as confirmed in `closeNative`: red by assertion, the seven
  writer-close cases and the backend's release ("waited 10s for stopped, the release unconfirmed":
  confirmed, the device would open again); all else green.
- C5, the gone caller's answer not recorded: red at `CompletionResult`, after its caller has gone
  and the cancellation arm; all else green.
- K2, the writer's own panic left untyped: red by assertion, the three writer-only cases, the close
  after the writer panicked (one line of two), a reopen's setup's two cases and the backend's writer
  case (`device_error`); never by death. More than K2's one case: every case whose panic the writer's
  own recover answers.
- C6 (the hand-off's guard dropped from the revocation): after the hand-off red, the delivered sink
  revoked. C7 (`native.go`'s fence disabled): the backend's open with its close failing red, its
  release confirmed ("waited 10s for stopped, the release unconfirmed"). C9 (`openNative` raising a setup's panic without its close's failure):
  the four setup-and-close cases red. C10 (the in-flight answer disabled): a reopen's check red, its
  resume answered as one that never ran; the unconfirmed cases green, the verdict answering the
  record either way.
- C8 (the recover's wake of the writer dropped) was green in the batch's one run, against its
  prediction: in that interleaving the writer's first loop reads the release the cut revocation
  began. Run alone 60 times under `-race` it was red 18 times, each at "waited 5s for the opening
  worker's end" (the writer idle before the revocation the panic cut short, nothing waking it), and
  HEAD green 60 of 60: the wake is what keeps that interleaving from hanging, and no test can force
  it without a seam of its own.

### Runs

- The four tests, `go test -race -count=20 -timeout 30m -run '^(TestANativeSinksCleanupThatPanicsStaysInThePlayer|TestANativeWritersOwnPanicIsTypedAndLogged|TestANativeOpenThatPanicsStaysInThePlayer|TestABackendSurvivesAPanicOnANativeSinksGoroutines)$' ./internal/alsapcm`:
  ok, 518.4 s, 500 case runs passing, none skipped (the rig runs, its TMPDIR on disk).
- alsapcm and every package that imports it, `go test -race -count=1 -timeout 45m ./internal/alsapcm ./internal/player/binding ./internal/player/playback/... ./internal/player/policy ./internal/player/engine ./internal/player/hotplug ./internal/config ./cmd/bridge`
  in the build slot: ok, the eight packages with tests (alsapcm 109.7 s, binding 3.4 s, playback 3.5 s,
  policy 1.3 s, engine 35.0 s, hotplug 2.4 s, config 17.4 s, cmd/bridge 1,637.0 s). The timeout was
  raised from the brief's 30m for cmd/bridge's margin under load; it finished inside 30m too.
- The native-integration overlays, as `gate.yml`'s two steps (`scripts/native-integration-overlay.py`,
  the new internal test file an ordinary file there): `./cmd/bridge -run '^TestNativeIntegration'
  -timeout 5m -watchdog 10s` ok, 16 passes, none skipped, every name the workflow checks passing;
  `./internal/player/playback` ok, 33 passes, none skipped, every named one passing.
- The qualification tool, `go run ./scripts/qualification-version -base a14a26db -head HEAD`:
  "qualification version OK: a14a26db23b463064ad3d2b4e8ca175ffad4420f..HEAD".
- `make fmt vet test build-all` in one slot session (P=4, go1.27.3, `TEST_PKGS='-count=1 -timeout 45m ./...'`):
  `make fmt` rewrote nothing; vet clean; `make test` ok in 2,109.8 s, 92 packages and none failed
  (cmd/bridge 1,732.3 s, internal/manifest 1,598.3 s, internal/admin 1,243.6 s, internal/backup
  658.9 s, internal/alsapcm 110.2 s); the release targets built (42.3 s). The whole session, with the
  race run and the overlays, took 3,874 s, the tree clean after it.
- `make docs-check`: ok (`output.md` 96,529 of its 100,000 bytes once the GET /v1/outputs MPD-gate
  entry's long form moved out, `verify_moves.py --base 4964fd88` passing with one block moved). The
  pinned gofmt (`go run cmd/gofmt -l`, go1.27.3) lists none of the changed Go files. This entry was
  added after that session; docs-check and the citation guard ran again over it.

### What stays open

- A panic while the writer holds `s.mu` still deadlocks its recovery, which takes the lock
  (`setNativeError`, `sink_dsd_writer.go`; the carrier's record too): `Done` never closes and the
  session's teardown waits on it. The PCM writer's `lockW` answers it there; a follow-up touches
  every writer lock site.
- The life's watcher (`sink_dsd.go`, the goroutine that revokes the sink once its life has ended)
  and the release's deadline goroutine keep no recover: a select, a revocation, a cancel and a close.
- A session's native open refused without a panic, whose node's close failed, fences nothing
  (`releaseErr` is set at an output's close alone), where the console's test tone fences it; the
  backend's, for a follow-up.
- A panic after the opening worker's reply has handed its sink over can come only from the test's
  seam: nothing on the worker runs there.
