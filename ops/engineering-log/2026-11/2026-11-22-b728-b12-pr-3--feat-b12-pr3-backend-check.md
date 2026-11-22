## 2026-11-22 — B728 B12 PR 3: the backend's DSD check, run and ended

Branch `feat/b12-pr3-backend-check`, stacked on B12 PR 0 (`fix/b12-pr0-alsapcm-recover` at
`6a2c0fa3`, tree `30dffec3`), with main `edc93946` merged in by a merge commit, `c8340a51`, whose
tree is `a21fca5b`, the tree the brief expected: the merged base every count and run below is
taken against. B12 is the bridge player's optional DSD listening check
(`ops/plan-b728-dsd-capability-design.md` §6.2 to §6.4); this PR adds it to the player's backend,
inert: PR 4 wires the routes, serve's adapter, the answer and the read side. W1 to W9 are the
binding review folded into the resumed brief; C1 to C6 the brief's controls (C7, the sink's own
recover, is PR 0's and dropped with C1); P1, P2, P3 and P5 the policy half's own controls (its
reconciliation against the admission identity is C5's); W1, W2, W3a, W3b and W8 this entry's
controls of those W items; and A1 the console's word's.

### The base and the draft

The saved draft (`wip-backend-check.patch`, the unreviewed input) applied to the merged base with no
conflict: it touches only `internal/player/playback` (`backend.go`, `dsd_check.go`, `dsd_choice.go`,
`dsd_contain.go`, `dsd_policy.go`, `probe.go`, `tone.go`), none of PR 0's files
(`internal/alsapcm`'s `sink_dsd.go`, `sink_dsd_open.go`, `sink_dsd_writer.go`, `device_native.go`,
and `playback/native.go`). Its semantic overlaps with PR 0, and how each was resolved:

- The cleanup's predicate. The draft fenced on its own reading of a cleanup's failure
  (`cleanupFailed`, which took any recovered panic for one). With PR 0 in, `CleanupResult` is the
  sink's cleanup failures alone (`s.cleanupErr`, `closeNative`'s, each joined with
  `ErrReleaseUnconfirmed`), a pending open's `CompletionResult` joins the open's own error with a
  revoked sink's result, and a panic PR 0 recovered whose release was confirmed carries
  `ErrPanicked` without `ErrReleaseUnconfirmed`. W1's predicate replaced the draft's: the device is
  fenced exactly where the cleanup's own result wraps `ErrReleaseUnconfirmed`
  (`cleanCheck`), as `native.go` reads it.
- `DSDConfig.Log`, PR 0's: the draft's open passed none, so a panic its sink recovered was logged
  to the player's component and not the backend's logger; the check passes `b.log`.

V1's premises, re-read against PR 0: `CleanupResult` (`sink_dsd.go:1281`, `s.cleanupErr`) carries
only the cleanup's failures, `result()` (`:1082`, `s.err`) the playback's and the cleanup's both, and
`CompletionResult` the open's own error joined with any revoked sink's `result()`
(`sink_dsd_open.go:69`, `:79`). So a refused open whose cleanup succeeded answers a non-nil
`CompletionResult` that wraps no `ErrReleaseUnconfirmed`: W1's predicate leaves its slot reusable
(`TestAStopDuringAParkedOpenWaitsForItsCompletion/its_node_closed_cleanly`).

The draft was then revised to W1 to W9. W2's count and W8's record were the draft's already; the
revisions were W1's predicate (above), W3's mapping in `ToneOutput` (the draft ended a check
`stopped` `serve_stopping` on a shut that came after `CloseTones`; W3 makes every shut but a unit
change `output_unavailable`), W9's fresh reading before the open, `Locate`'s failure as
`output_gone`, the admission identity's compare at the open, and `DSDConfig.Log`.

### What the check is

The rule is `output.md`'s bullet under "The player's DSD check (B728 B12)", its long form in
`ops/History-rules-output.md` (B728 B12 PR 3's section). In short: the device slot's fourth holder
(`playback.Backend.Check`), admitted by the pair published now and the set's plan at the rate,
refused by the supervisor's gate's word, the fence, a lease, a session or another check, waiting for
a probe or a preempted tone as a Start's hold does; its run a fresh reading, the locate, the open by
the plan with the unit's, the node's and the admission identity's checks, its activation judged, the
prefill, the chime, the tail and the drain, bounded 2 s and 10 s; judged by the reconciliation after
the sessions (the admission identity first), containment and a monitor of its own; ended by a cause
on its context; its slot given back once its cleanup has answered, the device fenced where that
cleanup's own result wraps `ErrReleaseUnconfirmed`.

### The budget

`git diff --numstat c8340a51` over the non-test Go files, comments and blanks included: +792
lines, -54 (`dsd_check.go` 658 new; `backend.go` +31/-11, `dsd_policy.go` +44/-24, `dsd_contain.go`
+23/-3, `tone.go` +21/-9, `probe.go` +8/-2, `dsd_choice.go` +3/-2, `doc.go` +4/-3), and one line of
`internal/admin/static/app.js` (the console's sentence for `check_running`): under the resumed
brief's about 800, its exit at about 900.

### The preparation's bound, measured

A scratch test overlaid into `internal/player/dsdchime` (`go test -overlay`, nothing tracked) timed
one cold `Get` per rate, a fresh process each run, on this PR's amd64 build host (16 CPUs,
go1.27.3): DSD64 32.6 to 33.4 ms, DSD128 64.5 to 66.0 ms, DSD256 127.4 to 129.0 ms over three
runs; under `-race` 396 ms, 797 ms and 1.593 s once. Three times the largest plain build is about
387 ms, under 2 s, so the bound stays 2 s (B12 PR 1 measured 31, 62 and 123 ms on its host). A Pi 4
is not measured: the bound holds there while a build is less than about five times slower than
here. The tests hold every bound on a timer of their own, so the race detector's build time races
nothing.

### Inertness

- No `init`: `grep -n "^func init" internal/player/playback/dsd_check.go` finds none.
- No production caller: `git grep -n -E "CheckRequest\b|\.CheckStatus\(\)|StopCheck\(|CloseChecks\(|CheckPreparing|CheckRefused\b|checkHold" -- '*.go' ':!*_test.go'`
  outside `internal/player/playback/` finds only `internal/atlasharvest`'s unrelated
  `bookletsCheckRequest` and the engine's own private `checkHold` (B12 PR 2's), and
  `git grep -n -E "\bb\.Check\(|\.Check\(ctx, req|\bbe\.Check\(" -- 'internal/player/playback/*.go' ':!*_test.go'`
  finds no caller of `Check` inside the package: no path from serve, the CLI or the console reaches
  it (a check needs a `CheckRequest`, and no production file builds one).
- Nothing started at construction: `TestABackendBuiltStartsNothingOfTheCheck` builds a backend with
  a counting `AfterFunc` and an effective policy: no timer armed, `Running()` (0, 0), no record.
- Every branch added to a shared path is taken only while a check is active: each reads
  `b.checking`, set in `admitCheckLocked` alone and cleared in `endCheck` alone, both under `b.mu`
  (`hold`'s `slotHeldLocked`, `reserveProbe`, `reserveTone`, `takeLease`, `ToneOutput`,
  `reconcileDSD`, `containDSD`); the two refactors in shared paths are no-ops otherwise:
  `refreshDSD` split into `refreshHeld` with the same body, and `checkDSDNode`'s body moved into
  `nodeIdentity` unchanged. And by coverage: the pre-existing tests of both packages, the new ones skipped
  (`go test -coverpkg=…/internal/player/playback -skip '^(<the new tests>)$'` over
  `./internal/alsapcm/` and over `./internal/player/playback/`, the profiles merged), executed none
  of `dsd_check.go`'s 282 statements in 179 blocks, and each branch the check adds to a shared path
  (`slotHeldLocked`'s case, `reserveProbe`'s and `reserveTone`'s, `ToneOutput`'s end of the check,
  `containDSD`'s and `reconcileDSD`'s call of `reconcileCheck`) has every block at count 0, while
  the suites themselves passed.

### The tests, and where they live

W6 asks that the check's tests be `internal/player/playback`'s, "driving the fake kernel through
`alsapcm`'s existing exported test constructors, as the tone's tests do". The fake kernel's
constructors are exported by `internal/alsapcm/export_test.go`, which only the test binary of
`internal/alsapcm` compiles; outside the `nativeintegration` overlay no other package can reach
them, and the tone's backend tests are `internal/alsapcm/playback_tone_test.go`, package
`alsapcm_test`, for that reason. So the tests are split as the tone's are:

- `internal/alsapcm`, package `alsapcm_test` (`playback_dsd_check_test.go`), through the engine's
  `Run`, the backend and the real native and DoP sinks over the fake kernel:
  `TestTheCheckAdmitsOnlyWhatTheSetPlays`, `TestTheCheckOwnsTheSlotAgainstProbesAndTones`,
  `TestTheChimeReachesTheDACAsItsPlanSays`, `TestEveryEndOfTheCheckEndsItWithinItsBound`,
  `TestAPolicyChangeEndsTheCheck`, `TestAnotherMakersUnitDuringPreparationEndsTheCheck`,
  `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice` (each case in a child process,
  `RunEachCaseInAChild`), `TestAStopDuringAParkedOpenWaitsForItsCompletion` (C2),
  `TestAParkedMonitorReadingHoldsNoEndOfTheCheck` (C5) and
  `TestServesStopWaitsForEveryCheckItCounted` (W2). The DSD rig gains an optional opener stand-in
  (`dsdRig.openNative`, test code); no seam was added to `internal/alsapcm`.
- `internal/player/playback`, package `playback` (`dsd_check_gate_test.go`), white-box, no fake
  kernel: `TestTheChecksGateReadsTheDeviceSlot`, `TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters`,
  `TestWhatTheSlotRefusesWhileTheCheckHoldsIt`, `TestTheTonesAndTheProbesNewWordsAreWorded`,
  `TestTheChecksPhasesAndReasonsAreTheirLists` and `TestABackendBuiltStartsNothingOfTheCheck`.
- `cmd/bridge`'s `TestEveryToneWordIsWorded`, unchanged, holds `check_running` to the console's
  sentence.

No test sleeps: the check's bounds and the slot's waits are held timers fired by the test, the
preparation is held at `Seams.CheckPreparing`, a reading of the box at a box stand-in, the open and
the close in the fake kernel, the unit's check at `holdNextAdmit`.

What the chime test compares: every byte the kernel was given, for native `DSD_U32_BE` and
`DSD_U16_LE` and DoP `S32_LE`, against an oracle that assembles each channel's word one bit at a
time from the chime's planes and the 0x69 silence and stores it as the format says (big- or
little-endian; DoP's 00, low byte, high byte and the 05/FA marker alternating from the run's first
frame), never through the bridge's packers: the plan's prefill (89 frames for U32 at 88.2 kHz, 177
for U16 and DoP at 176.4 kHz, both odd, so the chime's first word sits at an odd frame index), the
chime, the plan's tail (100 ms) and nothing after. The writes are partial: the fake takes only what
fits as the buffer fills with the DAC held, and the DAC then plays five 3 ms stretches before it
plays freely.

Three of the brief's rows took a shape the code at the base required:

- A DoP level control moved "during preparation": W9's fresh reading before the open now refuses
  it, so nothing is opened (`a level control moved during the preparation`); the open's own gain
  check is a row of its own, the control moved between that reading and the node's check
  (`a level control moved before the open's gain check`), one open refused, nothing written, no
  `driver_accepted` recorded.
- Another maker's unit "during preparation" is likewise refused at that reading (`policy_changed`,
  nothing opened), and one swapped in at the open (`wrong_device`, nothing written, nothing
  recorded) is the open's check's.
- A native sink unplugged while it plays answers its DROP and HW_FREE `ENODEV`, which `closeNative`
  keeps as a cleanup's failure, so that disconnect fences the device (`a disconnect while the chime
  plays`, `failed` `release_unconfirmed`); `output_gone` is the word of a disconnect the cleanup
  confirms: `Locate`'s failure and the node gone at its open (`TestEveryEndOfTheCheckEndsItWithinItsBound`).

The sink's writer's panic is not among the cleanup's rows: on the check's path the native writer
reads the fake's clock first under the sink's lock (`beginReleaseLocked`, from `setNativeError` or
`beginRelease`), so `PanicAtNextClockRead` fires with `s.mu` held, `setNativeError` (no deferred
unlock) leaves it held, and `loopNative`'s recover deadlocks on it in `setNativeError(fault)` while
the check's `WriteDSD` waits on the same lock: two such rows hung to their 20 s watchdog in the first
run, their goroutine dumps showing both goroutines in `sync.Mutex.Lock` on the sink's lock beneath
the panic's frame in `beginReleaseLocked`. That is B12 PR 0's residual, not this PR's to change; W1's panic rows
take the opening worker's panic (`unit.panics`), which PR 0 contains, with a confirmed release and
with a failed one.

### Red first, against the draft

The new tests ran against the saved draft's seven files overlaid on the merged base
(`go test -overlay`, nothing tracked): in `internal/alsapcm` four cases were red, each one a W
revision, every other case green:

- `TestEveryEndOfTheCheckEndsItWithinItsBound/Locate_fails`: the draft ended it `output_unavailable`
  (W9: `output_gone`);
- `TestAPolicyChangeEndsTheCheck/a_level_control_moved_during_the_preparation`: the draft opened the
  node (W9's fresh reading before the open);
- `TestAnotherMakersUnitDuringPreparationEndsTheCheck/swapped_in_as_the_check_prepares`: the draft
  answered `wrong_device` after an open (W9: `policy_changed`, nothing opened);
- `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice/an_open's_panic,_its_release_confirmed`: the
  draft fenced the device (W1), and its panic was logged where the backend's logger never saw it
  (no `DSDConfig.Log`).

The six `internal/player/playback` tests were green on the draft, whose slot logic was right
already; their red evidence is the controls'.

### The controls

Each control is an exact-once replacement of HEAD's bytes in one production file, overlaid at test
time (`go test -overlay`), so no tracked file changed and nothing needed restoring; its predicted
red leaves were written to a file before any control ran, and a control passes only where its red
leaves are exactly those. Each ran every new test of both packages (`-race`, `internal/alsapcm`
with `-watchdog 20s`).

The first pass found two predictions wrong, and both found the suite short:

- C2, the slot given back before `Done` (the slot cleared in `cleanCheck` before its wait for the
  sink's `Done`), went red nowhere: both held-close rows stayed green. With PR 0's native sink every
  command the run makes after the check's life ends answers only with the cleanup's verdict, so the
  run was held inside `Finish` until `Done`, and `cleanCheck`'s own wait is the only one that keeps
  the slot where no command is under way: a check ended between its open and its first command, at
  the activation's judgement. No case held a close there. The case was added
  (`a check ended at its activation keeps the slot until its close answers`), and C2 re-predicted
  on it.
- C3, the open's identity check skipped (which skips the open's DoP gain check with it), left the
  gain row green: the activation's fresh reading refuses the same gain change before any byte, so
  the outcome was the same. The open's own check is still load-bearing in one observable: an open
  it refuses records no `driver_accepted`, where an open that passes records it under the admitted
  identity, another maker's unit included. Both open-time rows now assert that nothing was recorded,
  and C3 was re-predicted on them.

Every control then ran again on the tests as committed, against predictions written before that
pass (C2's and C3's as above, and the new case added to C6's and P3's, which hold it at `u.Admit`
and need the activation's judgement):

| Control | Mutation | Red leaves | Final pass | First pass |
|---|---|---|---|---|
| C1 | the check dropped from reconcileDSD | 2 | matched | matched |
|  | red: `TestAPolicyChangeEndsTheCheck`: a level control moved once audio reached the DAC, the state made unreadable |  |  |  |
| C2 | the slot given back before Done | 1 | matched | missed: 0 of 2 predicted red |
|  | red: `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice`: a check ended at its activation keeps the slot until its close answers |  |  |  |
| C3 | the identity check at open skipped | 2 | matched | missed: 1 of 2 predicted red |
|  | red: `TestAPolicyChangeEndsTheCheck`: a level control moved before the open's gain check; `TestAnotherMakersUnitDuringPreparationEndsTheCheck`: swapped in at the open |  |  |  |
| C4 | the take not moving b.starts | 1 | matched | matched |
|  | red: `TestTheCheckOwnsTheSlotAgainstProbesAndTones`: the check's take voids an ending probe's answers |  |  |  |
| C5 | the admission identity's compare removed | 2 | matched | matched |
|  | red: `TestAnotherMakersUnitDuringPreparationEndsTheCheck`: another firmware offered an identical tuple, another maker offered an identical tuple |  |  |  |
| C6 | u.Admit skipped | 9 | matched | matched |
|  | red: `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice`: a check ended at its activation keeps the slot until its close answers, a refused open whose close fails, a refused open whose close panics, an open's panic, its close failing, an open's panic, its release confirmed; `TestAPolicyChangeEndsTheCheck`: a level control moved before the open's gain check, the state made unreadable between the open and its activation; `TestAnotherMakersUnitDuringPreparationEndsTheCheck`: another serial or port of the model at the open, swapped in at the open |  |  |  |
| P1 | containment does not end the check | 1 | matched | matched |
|  | red: `TestAPolicyChangeEndsTheCheck`: a reading that panics |  |  |  |
| P2 | the check's own monitor not started | 6 | matched | matched |
|  | red: `TestAParkedMonitorReadingHoldsNoEndOfTheCheck`; `TestAPolicyChangeEndsTheCheck`: a level control moved once audio reached the DAC, a reading that panics, a set change that keeps the check's plan, the state made unreadable; `TestServesStopWaitsForEveryCheckItCounted`: a monitor held in its reading |  |  |  |
| P3 | the activation's judgement skipped | 2 | matched | matched |
|  | red: `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice`: a check ended at its activation keeps the slot until its close answers; `TestAPolicyChangeEndsTheCheck`: the state made unreadable between the open and its activation |  |  |  |
| P5 | the reading before the open skipped | 2 | matched | matched |
|  | red: `TestAPolicyChangeEndsTheCheck`: a level control moved during the preparation; `TestAnotherMakersUnitDuringPreparationEndsTheCheck`: swapped in as the check prepares |  |  |  |
| W1 | any cleanup error fences | 6 | matched | matched |
|  | red: `TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice`: an open's panic, its release confirmed; `TestAPolicyChangeEndsTheCheck`: a level control moved before the open's gain check; `TestAStopDuringAParkedOpenWaitsForItsCompletion`: its node closed cleanly; `TestAnotherMakersUnitDuringPreparationEndsTheCheck`: another serial or port of the model at the open, swapped in at the open; `TestEveryEndOfTheCheckEndsItWithinItsBound`: the node gone at its open |  |  |  |
| W2 | the admission not counted | 5 | matched | matched |
|  | red: `TestServesStopWaitsForEveryCheckItCounted`: an admission held in its reading; `TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters`: a probe past ProbeWait, a probe that ends, a tone, ended and past ToneYield, its request's end |  |  |  |
| W3a | a unit change ends the check output_unavailable | 2 | matched | matched |
|  | red: `TestEveryEndOfTheCheckEndsItWithinItsBound`: playing/the gate shut output changed, preparing/the gate shut output changed |  |  |  |
| W3b | admission ignores the gate | 13 | matched | matched |
|  | red: `TestTheCheckAdmitsOnlyWhatTheSetPlays`: the gate as it starts, the gate shut for a report the engine did not take, the gate shut for a stale snapshot, the gate shut for a unit that changed, the gate shut for an output gone, the gate shut for the player turned off; `TestTheChecksGateReadsTheDeviceSlot`: the gate as it starts, the gate closed by the tones' stop, the gate shut for a report the engine did not take, the gate shut for a stale snapshot, the gate shut for a unit that changed, the gate shut for an output gone, the gate shut withheld |  |  |  |
| W8 | a refusal that reaches the admission makes a record | 9 | matched | matched |
|  | red: `TestTheCheckAdmitsOnlyWhatTheSetPlays`: DoP unconfirmed on a DAC no profile knows, a key another DAC has, a rate the set converts, a rate the set plays the other way, a token another reading made, an offered tuple whose first use is not acknowledged, an unknown DAC under auto; `TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters`: a probe that ends; `TestTheChecksGateReadsTheDeviceSlot`: a free slot whose read side names no DAC |  |  |  |

Beside them, A1, the console's sentence for `check_running` removed from `app.js` in place (the one
control an overlay cannot make, since both tests read the file at run time; the file put back from
HEAD after, its bytes checked equal): red, as predicted, in cmd/bridge's
`TestEveryToneWordIsWorded` ("PLAYER_TONE_WORDS has no sentence for check_running") and in
`TestTheTonesAndTheProbesNewWordsAreWorded`, and nowhere else run.

C7 of the first brief, the sink's own recover removed, is B12 PR 0's control and was not run here.

### The runs before the gate

- Every new test of both packages, `go test -race -count=1`, `internal/alsapcm` with
  `-watchdog 20s`: green at each commit of the tests; the full suites of both packages,
  `-race`, green at the tests' first commit (`internal/alsapcm` 136.6 s, `internal/player/playback`
  3.2 s).
- `internal/api`'s `TestDSDOutputProtocolTokens`, V4's census of the playback package's
  observation states, by name: green.
- `go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base
  c8340a51 -head HEAD`: "guarded code changed without qualificationVersion increase (57 -> 57)"
  before the bump, the guarded changes being `internal/alsapcm/playback_dsd_check_test.go` and
  `internal/alsapcm/playback_dsd_policy_test.go` alone (no guarded production file: `native.go`,
  `edges.go` and `internal/alsapcm`'s production files unchanged); "qualification version OK:
  c8340a51..HEAD" after it (W5).
- The pinned gofmt (`go run cmd/gofmt -l` under go1.27.3) over the changed packages: clean.

### PR 4's dependency

PR 4 wires the check (its three routes, serve's adapter, which enters and exits the engine's hold
and hands the backend its stop signal, serve's calls of `CloseChecks` at its stop, the answer's
write, the read side's member and `PROTOCOL.md`) only on PR 3's guarantees as this entry records
them: the complete admission (the gate, the fence, the slot, the pair published now and the set's
plan), the identity (the admission identity compared before any `Keep`, the open's identity and
unit checks), the containment (reconciliation, `containDSD`, the monitor) and the cleanup (the slot
given back only once the cleanup has answered, the device fenced where its own result wraps
`ErrReleaseUnconfirmed`). The design's §8 row says the same.

### Residuals

- Nothing in serve can start, stop or answer a check until PR 4.
- A listen of the chime as built on a real DAC is owed before any route exposes the check.
- The preparation's bound is unmeasured on a Pi 4 (above).
- The native writer's panic under the sink's lock (above) is B12 PR 0's, to go with backlog B987's
  residuals.
- A DAC unplugged while the chime plays fences the device until serve restarts, as a native
  session's unplug does (`releaseNative` keeps the DROP's and HW_FREE's `ENODEV`): a scratch test
  overlaid into `internal/alsapcm` (not kept) played native DSD64 through the engine, unplugged the
  fake DAC, and read the session's stop `fault` (`disconnected`), its release unconfirmed and a tone
  after it refused `release_unconfirmed`. Whether a disconnect's teardown errors should count as a
  release confirmed is the native sink's question, not this PR's.
