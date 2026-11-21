## 2026-11-20 — B728 B12 PR 2: the engine's check hold

Branch `feat/b12-pr2-check-hold`, from main `6ee6102a` (the base, recorded; main had not moved).
B12 is the bridge player's optional DSD listening check (`ops/plan-b728-dsd-capability-design.md`
§6.2 and §6.3); this PR adds its hold to the engine and nothing else. The backend's check (PR 3)
and the routes with serve's adapter (PR 4) come later, so nothing in production calls the new
command: it ships inert (below).

### The hazard

The design's hold was maintenance. Its entry asked whether a hold was held and whether the
caller's generation was current (`maintenance.go:28-35` at the base), and a configuration
reservation's grant moves no generation (`owner.go:643`), so nothing kept a check out of a DSD
choice's write in flight; nothing decided "something plays", the design's `would_interrupt`, but
the reservation's predicate (`owner.go:622-634`); and under maintenance a Stop answered success and
stopped nothing (`owner.go:583-588`), so an OpenHome Stop during a chime would have answered
success with the chime still playing (the surface's Stop is the engine's, `writes.go:185-209`).
Every premise the brief listed was re-read at `6ee6102a` and held as stated: no engine or OpenHome
file changed between the drafting base `3399fda6` and this one, and the cited lines of
`sink_dsd.go`, `serve_player_output_dsd.go` and `dsd_choice.go` (`takeLease`, `:585-626`) were
where the brief put them.

### What was built

`Engine.EnterCheck(ctx, interrupt)` (`engine.go`), a command of no lane dispatched beside
`EnterMaintenance`, ahead of the maintenance arm. Its one step (`enterCheck`, `maintenance.go`)
refuses, in this order: a hold held (`maintenance`), a configuration reservation held
(`configuring`), a release unconfirmed (`release_unconfirmed`), the output unknown or unavailable
(`outputRefusal`), and, without `interrupt`, anything but stopped with nothing under way
(`not_stopped`): the reservation's predicate, now one function the two ask (`unsettled`, in
`owner.go`; `reserveConfiguration`'s answer is unchanged), so a session starting, live, paused or
releasing, and a transport paused with no session, a paused seek's target among them, refuse it.
Granted, it is EnterMaintenance's step, which the two now share (`grantHold`): the reason reads
"check", mutations close, waiters and parked commands are answered, the session is released and
pending backend work dropped, and `settleSeek` drops a seek's target and its track. EnterMaintenance
is unchanged in behaviour, the harness's (`cmd/bridge/output_play.go:717`).

The grant is answered by `deliver` once its release settles, on the acknowledgement (`acked`) or
the deadline (`releaseDeadline`), the two paths that built a delayed grant before: a Hold, with
its stop signal, only with the release confirmed; otherwise the hold is ended first and the grant
refused, so an error leaves its caller nothing to let go (the review's decision): the deadline
`release_unconfirmed`, which outranks a Stop; a Stop that came while the grant waited `stopped`
(no hold is handed out already stopped); the hold's expiry `lease_invalid` (the existing reason
whose doc names a hold that expired; `MaintenanceTTL` can be one second, under `ReleaseTimeout`);
and the output's report its reason. Each is answered before `endMaintenance` could answer the same
command with a hold (a second answer is dropped, `command.replied`). No `engine.Reason` was added.

`Hold.Stop` (`snapshot.go`), a receive-only channel, nil for EnterMaintenance's, closes once at
the hold's first end of any kind (its exit, its expiry, the output going away or its unit
changing, which ends the hold, `reportOutput` in `edits.go`; the shutdown, `beginShutdown`) or its
first Stop. Under the hold a Stop (`checkStop`) closes the signal and parks in the hold's own list,
never as its one waiter; the first arms a timer of the hold's own on the engine's clock for
`ReleaseTimeout`; its message (`msgCheckStops`, `events.go`) carries the hold, so one from an ended
hold changes nothing under a later one. The Stops are answered success at the hold's end or at the
timer, which latches the deadline so a later Stop is answered at once, and `closed` at the
shutdown as it begins (`releaseStops`). The harness's plain maintenance keeps its Stop answered at
once.

The caller's context: the command carries it (the draft's `context.WithoutCancel` is not taken),
so one that ends before admission stops nothing (`ErrNotAdmitted`). Once admitted, the grant may
have released a session already, so the caller waits for the owner's answer whatever its context
does: the command is kept (`command.kept`), which `do` alone reads, after its admission's
compare-and-swap shows the owner admitted it, to go on waiting for the reply or the engine's end
(`e.done`); no timer of its own, the owner's wait being bounded by the release's deadline on the
engine's clock. `do` is unchanged for every other command. A reply without an error to a caller
whose context ended is exited (`ExitMaintenance` on a background context) before `EnterCheck`
answers `cancelled` without `ErrNotAdmitted`; a reply with an error is returned as it is. Round 2
replaced this wait with a bound of the caller's own and a handoff whose cleanup is the owner's
("Round 2: the caller's bound and the handoff", below).

Size: the production files of `internal/player/engine` gained 354 lines and lost 85 (`git diff
--numstat 6ee6102a`), net 269: 190 lines of code and 152 of comment added, 44 and 41 removed. The
budget was about 250 with the doc and residual comments; the excess is the package doc's
"Maintenance" paragraph and the doc of each new step. No other package's production code changed.

### Inertness

```
$ git grep -n EnterCheck -- . ':!internal/player/engine'
internal/player/openhome/check_test.go:11:// holds the engine (engine.EnterCheck), the surface's Stop is the engine's,
internal/player/openhome/check_test.go:36:	hold, err := p.eng.EnterCheck(bg, true)
internal/player/openhome/check_test.go:80:	hold, err := p.eng.EnterCheck(bg, false)
$ git grep -n EnterCheck -- '*.go' ':!*_test.go'
(internal/player/engine alone: the method, its op, its docs)
$ go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./cmd/...
github.com/acoseac/1-bit-bridge/cmd/bridge
```

The one main package imports the engine (serve's player), and no file of it, or of any package but
the engine, names `EnterCheck` outside a test: nothing in production calls it, and the
qualification version owes no bump (`go run -tags ts_omit_webclient,ts_omit_debugeventbus
./scripts/qualification-version -base 6ee6102a73b4e92497f7b3c3e3d93557387e7a1a -head HEAD`:
"qualification version OK").

### Red first

The new tests over the base's production files with a names-only shim, laid by `go test -overlay`
(`EnterCheck` as EnterMaintenance at the current generation, `Hold.Stop`, a `checkHold` and a
`msgCheckStops` that nothing reads), `-watchdog 5s`: thirteen of the fourteen engine tests red,
each for its own reason: granted beside a reservation, beside a start, playing, paused, a release
under way, paused with no session and at a paused seek, beside an unconfirmed release and with the
output unknown; a hold with no stop signal; the Stop answered at once; a hold handed over after
its expiry, and after its release passed its deadline; a hold left held by a caller that had gone
during its release ("held true, maintenance"); and the gone-caller case at the watchdog, which at
the base finds no command of the check's name to hold (its property is its control's to show).
`TestAPlayAfterTheCheckHoldsGrantIsRefused` is green at the base: maintenance already refused a
Play while its grant waited, which the check's hold keeps. Through OpenHome,
`TestAnOpenHomeStopEndsTheCheckThroughTheEngine` is red ("the Stop answered 200 and the check's
stop signal is open"); `TestOpenHomesOtherWritesAre501DuringACheck` is green, maintenance's 501s
being what the check's hold keeps. At HEAD every one is green.

### Controls

Each predicted in writing first, an exact-once mutation of one production file at `9d2289a5`,
the round's last commit of code (between it and the round's head, `5cc94537`, only records changed:
`git diff --stat 9d2289a5 5cc94537` names the two rules files, `CLAUDE.md`, the History file, the
design and this entry), laid by `go test -overlay`, no tracked file touched, the tree clean after
each. Each ran the fourteen engine tests `-race -count=50` and the whole engine package
`-race -count=1` (`-watchdog 10s`); C3 also the two OpenHome tests `-race -count=50`. Every one went
red exactly as predicted and nowhere else. Round 2 changed production files, and ran them again at
its own head ("Round 2: controls").

- C1, the reservation's refusal removed from `enterCheck`: `TestACheckHoldIsRefusedBesideAConfigurationReservation`
  50 of 50 ("the owner granted the check's hold, want a refusal configuring"); the package's other
  520 tests and subtests green.
- C2, the not-stopped predicate removed: `TestACheckHoldDecidesWouldInterruptInItsGrantsStep`'s six
  subtests but "stopped with nothing under way", 50 of 50 each, at their refusal without
  `interrupt`; 514 others green.
- C3, a Stop under the check's hold answered at once: the Stop test's three check subtests ("plain
  maintenance" green), the deadline's own hold, the shutdown's (answered success, not closed) and
  the expiry with a Stop first (`lease_invalid`, not `stopped`), 50 of 50 each; OpenHome's Stop 50
  of 50, its 501 test green; 513 others green.
- C4, the caller's context replaced by `context.WithoutCancel`: both subtests of
  `TestACheckHoldAskedByAGoneCallerStopsNothing` 50 of 50, at the watchdog (3 s in the x50 run):
  the caller waits on a context that never ends for an owner held at the seam; 518 others green.
- C5, the drain after admission removed: `TestTheCheckHoldsGrantIsNeverLostToItsCaller` 50 of 50,
  at "the check's hold whose caller left during its release: <nil>; want a cancellation of a
  command that ran" (the Hold handed to a caller that had gone, read first as the cancellation
  its caller was not answered); 520 others green.
- C6, the shutdown's answer of parked Stops removed (the signal still closed):
  `TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown` 50 of 50 at its bound (3 s in the x50
  run), the Stops answered only at the engine's end, which the held writer kept from coming; 520
  others green.

### Runs

- The ordering tests, in the build slot: `go test -race -count=50 -run
  '^(TestACheckHold.*|TestTheCheckHold.*|TestAStopUnderACheckHold.*|TestStopsParkedUnderACheckHold.*|TestAPlayAfterTheCheckHolds.*|TestAnInterruptWhoseRelease.*)$'
  ./internal/player/engine/`: ok, 68.0 s; and the two OpenHome tests the same way: ok, 100 passes,
  none skipped. The OpenHome rig skips on a TMPDIR on tmpfs (the engine refuses storage that does
  not outlive a reboot), so every OpenHome run here had TMPDIR under the worktree's `.tmp`, as
  `make test` gives it.
- The native-integration overlays (`gate.yml`'s two steps): `./cmd/bridge -run
  '^TestNativeIntegration' -timeout 5m -watchdog 10s`: ok, every named test passing;
  `./internal/player/playback`: ok, every named test passing.
- The qualification version: no bump owed (above).
- `make fmt vet test build-all` in one slot session (P=4, the Makefile's 30m, go1.27.3), the
  records staged: ok in 2,143 s, 91 packages and none failed; cmd/bridge 1,726.1 s (96 % of its
  30m), internal/manifest 1,610.9 s, internal/admin 1,230.9 s, internal/backup 662.4 s,
  internal/player/engine 31.5 s, internal/player/openhome 38.4 s; `make fmt` rewrote nothing, and
  the six release targets built.
- `make docs-check`: ok (`output.md` 99,519 of its 100,000 bytes, `openhome.md` 45,227). The
  pinned gofmt (`go run cmd/gofmt -l`, go1.27.3) lists none of the changed Go files.

### Round 2 (2026-11-21): the caller's bound, an expired grant, an owed release

Continued at `5cc94537`, round 1's head (verified, the tree clean). Three gaps remained:

- the caller's wait had no bound of its own: once its command was admitted, `do` disabled the
  context's arm for a kept command and waited for the reply or the engine's end, and the release's
  deadline needs the owner to take its message, so a stalled owner kept the caller waiting; the
  cleanup after a cancelled grant, `ExitMaintenance(context.Background(), …)`, was another
  unbounded wait;
- `deliver` never asked `m.expires`: with `MaintenanceTTL` at a second, a release acknowledged at
  1.5 s, before its 2 s deadline, handed over an expired hold with an open stop signal whenever the
  expiry's own message had not reached the owner yet;
- with `interrupt`, a grant found with no session held (`o.sess` nil) is answered at once with its
  release confirmed, never asking `o.sessions`.

Each premise was re-read at `5cc94537` and held as stated; `fakeClock.Skip` exists
(`fakeclock_test.go`, "Skip moves the clock d on and fires nothing").

### Round 2: the caller's bound and the handoff

`EnterCheck` sends its command (`send`, the sending half `do` shares; `do` is the base's again, its
`kept` arm gone) and waits for it in `awaitGrant`. Before the owner admits the command, the
context's end abandons it by the admission's compare-and-swap, as `do`'s does. As the owner admits
it, it sets the instant the caller's bound counts from and closes a channel the caller waits on
(`checkGrant.at` and `admitted`, in `admit`), and the caller arms one deadline, `ReleaseTimeout` and
`checkMargin` (500 ms) from that instant, with the engine's clock's `At`, so the tests drive it with
the fake clock. The margin's reason, in `checkMargin`'s doc: the grant is answered once the release
it made, or found under way, settles, which comes `ReleaseTimeout` from the admission at the
latest, and the margin is the owner's own time to take that message and send its answer, so an
owner that has not answered by then is taken for stalled. That bound, or the context's end,
answers `cancelled` at once, without `ErrNotAdmitted`: the command ran, and what it stopped stays
stopped. No `engine.Reason` was added, so OpenHome's mapping and the qualification check are as
they were.

The handoff is one compare-and-swap (`checkGrant.state`: waiting, answered, gone) between the owner
answering the caller and the caller giving up. Every answer of the check goes through `answer`,
which queues a reply only on winning it and now says whether it did, so a caller that gave up first
is answered nothing, by `deliver`, by `endMaintenance`'s waiter, by the shutdown or by a refusal,
and `deliver`, its grant unanswered, ends the hold itself, as an exit ends it (the timer, the Stops,
the state and the snapshot settled by `endMaintenance`). A caller that loses the swap has an answer
the owner has sent or will send: a refusal already sent it returns as it is, and any other answer,
sent or not yet sent (an owner can stall between its decision and the send), it gives back without
waiting (`giveBack`: a message, `msgCheckGone`, put in the owner's queue, or, that queue full,
posted from a goroutine of its own until the owner takes it or has ended); a grant read once the
caller's context has ended is given back the same way (`takeGrant`), so no hold is handed to a
caller whose context has ended. The owner takes the message in a later step than the one that
answered, and ends the hold granted to that command if it still stands (`checkGone`, by
`maintHold.grantee`); one ended already, or another's, changes nothing. So the cleanup is the
owner's and durable (a message the owner always takes, where a command bounded by its caller's
context can be abandoned and then refused by an owner that was stalled), the caller's whole wait
after the admission ends by one absolute deadline, the hold is ended once, by the owner, and no
caller is answered twice. `giveBack`'s fallback goroutine, started on the caller's side, is the one
goroutine of the engine's code that Run does not join: the tests never fill the queue (it holds
256), and it ends once the owner takes its message or the engine has ended.

Round 1's two cases of a caller gone after admission keep their properties and change their waits:
the caller is now answered at once, `cancelled` without `ErrNotAdmitted`, and the hold ended by the
owner once its release settles (`TestTheCheckHoldsGrantIsNeverLostToItsCaller`, which now waits for
that end, and finds the hold held and the release parked as its caller is answered); and a caller
gone during the wait for a release that is never confirmed is answered `cancelled` there rather
than `release_unconfirmed` at the deadline, which still ends the hold
(`TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo`'s second subtest).

The new cases:

- the owner held at the replying seam with the grant decided and not sent, granted at once and once
  its release settles, its caller giving up at its bound or at its context's end
  (`TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`): the caller answered within its bound
  with the owner held, the grant then sent and never read, the hold given back ended, its lease
  refused and its stop signal closed, the clock never moved in the context's cases;
- the owner stalled before the decision, held as it admits a barrier, the release acknowledged in
  time and the clock moved to the bound (`TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`): answered
  at the bound with the owner stalled, the hold ended once it runs, the release confirmed;
- the grant sent and not read when the caller's context ends, the caller held at its wait's first
  turn by a seam on its goroutine (`seams.checkWaits`), so its wait finds the reply and the context's
  end ready together and takes either (`TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, each
  outcome the same).

The rewritten `TestTheCheckHoldsGrantIsNeverLostToItsCaller` had a defect of its own as first
committed (`5d95445f`): it asserted the hold held as soon as the caller answered, having waited
only for the fake's record of the release, which the fake makes on the call's goroutine, before the
step that granted the hold has published it. A control's run found it (K6, below), and with no
mutation it was red 10 times in 200 at `5d95445f`, at the same line; `8a7e92d5` waits for the
published hold first: 1,000 runs green under `-race`, and the seventeen check tests 200 times each
green.

### Round 2: no expired hold handed over

`deliver` asks the hold's expiry third, after the release's confirmation and a Stop, the order the
expiry's own message keeps in `endMaintenance`: a grant whose expiry has come is refused
`lease_invalid` ("the hold expired before it was handed over", `expiredBeforeGrant`, which
`maintenanceExpiry` shares) and its hold ended, logged as that message logs it (`logExpiry`). Check
holds alone: a plain hold's grant keeps its contract. The case
(`TestACheckHoldExpiredAsItsReleaseSettlesIsNeverHandedOver`): `MaintenanceTTL` at its floor, the
check asked with interrupt while a session plays and its release parked, the clock moved past the
expiry without its timer (`fakeClock.Skip`), then the release acknowledged before its own deadline:
`lease_invalid` with no Stop, `stopped` with a Stop first.

### Round 2: an owed release beside no session held is unreachable

Sought through the engine's own transitions, never by setting the owner's fields, the state with
no session held (`o.sess` nil), a session's release owed (`o.sessions` not empty) and no release
unconfirmed is never reached, so no code changed:

- `startSession` sets the session held and the sessions owed in one step (`o.sess = s` beside
  `o.sessions[s.id] = s`);
- a session leaves the sessions owed only in `acked`, in its one step, the session deleted before
  `o.sess` is cleared, and an unconfirmed release that names it cleared, in both of its branches
  (in time and late);
- `o.sess` is cleared only there and in `releaseDeadline`, which sets `o.unconfirmed`, naming the
  session, in the same step, the session kept among those owed until its late acknowledgement;
- `o.unconfirmed` is otherwise set only by `pauseOverdue` and cleared only by `onDeviceReleased`,
  both for the session held (`e.current`), and `pauseOverdue` never replaces one already set;
- nothing starts while a release is unconfirmed (Play, PlayID, PlayIndex, Next, Previous and a seek
  that plays refuse it), so no second session is owed beside an unconfirmed one.

`TestASessionLeavesTheSessionsInTheStepItsReleaseEnds` pins it with a watch on the owner
(`sessionsWatch`: before each command it admits and after each step it notes, reading the owner's
fields and setting none): through a Stop, a Next released to play, a release past its deadline and
acknowledged late (where it must see the release owed and unconfirmed, and does), the check's hold
interrupting playback and its exit, the output going away and a fault, and the shutdown after each.

### Round 2: the OpenHome case and the rules

`engine.Seams` gains `Clock`, the engine's time in the tests of a package above it (`engine.Clock`
and `engine.Timer` name the engine's own interfaces for them); nil in production, as every field of
`Seams` is (`engine.Config.Seams` is set by no file but a test's). The OpenHome Stop's case runs its
engine on a clock that does not move (`stillClock`), so the deadline of the Stops the check holds
never answers its Stop, and the answer it asserts after the exit is the exit's: on the real clock a
slow run could have taken that answer from the deadline. Its read of the HTTP answer's channel before
the exit (`stop.pending()`) is gone, since an answer the engine sent and the handler had not yet
relayed leaves it empty too; that the engine does not answer the Stop before the exit is
`TestAStopUnderACheckHoldSignalsItAndAnswersAtItsExit`'s to show, from the replies its owner sends,
as round 1's C3 and round 2's C3r show red.

The rules overstated the Stop: `openhome.md`'s line said a Stop under the check answers once the
check has let go, and `output.md`'s forbade answering one before; the hold's own timer answers it at
`ReleaseTimeout` from the first parked, the hold still standing, which both lines and their long
forms now say (`openhome.md`'s line gains its first, at the end of "The write actions" in
`ops/History-rules-openhome.md`). `output.md`'s line gains the bound, the handoff, the expiry's
refusal and, as a residual, that the route which will call `EnterCheck` (PR 4) bounds its own call;
its test list moves to its long form, for the file's budget (99,353 of its 100,000 bytes), and the
long form is brought to round 2 (round 1's wait and its rejection of a bound replaced). `CLAUDE.md`'s
engine row says the caller's wait is bounded.

Size: round 2's production files of `internal/player/engine` gained 293 lines and lost 79
(`git diff --numstat 5cc94537` over them): 171 lines of code and 110 of comment added, 45 and 34
removed, most of it `engine.go`'s wait (`awaitGrant`, `giveUp`, `takeGrant`, `giveBack`) and
`send`'s split from `do`.

### Round 2: red first

Each new case over a base's production files by `go test -overlay`, predicted in writing first:

- at `5cc94537` with a names-only shim (`checkMargin`, and the `seams.checkWaits` field nothing
  calls), `-watchdog 5s`: every one of round 2's cases of the bound red, each at its own wait for an
  answer that round 1's caller, waiting for the reply whatever its context did, never gave:
  `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s four subtests, "waited 5s for the check
  whose caller gave up as its grant was sent to answer"; `TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`,
  "… for the check whose owner stalled to answer"; the rewritten
  `TestTheCheckHoldsGrantIsNeverLostToItsCaller` and `TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo`'s
  second subtest, "… for the check's hold whose caller left during its release to answer" (that
  test's first subtest green); and `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, "… for the caller held at its
  wait", a red of the seam's absence (round 1's caller kept no hold either: the property is its
  control's to show, K5);
- at `8a7e92d5`: `TestACheckHoldExpiredAsItsReleaseSettlesIsNeverHandedOver`'s case with no Stop red,
  "got <nil> (reason ""), want reason "lease_invalid"" (the expired hold handed over), its case
  with a Stop first green, the Stop's precedence being the base's;
- item 3 changed no code, so its test has no red at a base; its control (K7) shows what it pins.

### Round 2: controls

Each predicted in writing before its run, one exact-once mutation of one production file at the
commit that ended its item, laid by `go test -overlay` over a copy of that commit's tree
(`git archive`), so no tracked file was touched and no edit made meanwhile entered a control's
binary; the tree clean after each. Each ran the check tests `-race -count=50 -watchdog 3s`
(seventeen at `8a7e92d5`, nineteen at `41e52338` and `baabfc01`) and the whole engine package
`-race -count=1 -watchdog 10s`.

Item 1, at `8a7e92d5`:

- K1, the bound removed (`awaitGrant`'s deadline armed, its channel never selected): red 50 of 50
  at the watchdog, "waited 3s for … to answer", in `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s
  two cases given up at the bound and `TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`, each within
  its own bound, nothing hung; the cases given up at the context's end and the rest green (fifteen
  of seventeen; the package's other 197).
- K2, `deliver`'s end of a hold whose caller gave up removed: red 50 of 50 at the wait for that end,
  "waited 3s for the hold its caller left, ended by the owner"
  (`TestTheCheckHoldsGrantIsNeverLostToItsCaller`) and "… gave up, ended by the owner"
  (`TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`); the rest green.
- K3, `answer`'s compare-and-swap removed (the owner answers every check, and the caller's own swap
  always wins): red 50 of 50 in `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s four cases,
  at "waited 3s for the owner to apply handled check gone 1 times" (nothing given back), in
  `TestTheCheckHoldsGrantIsNeverLostToItsCaller` and `TestACheckWhoseOwnerStallsIsAnsweredAtItsBound`
  at their wait for the hold's end, and in `TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo`'s second
  case at "the owner answered a caller that had given up"; and in 18 of 50 runs of
  `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, its context's arm (predicted: about half);
  the rest green (twelve of seventeen; the package's other 195).
- K4, `giveUp`'s give-back removed: red 50 of 50 in `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s
  four cases at "… handled check gone 1 times", and in 26 of 50 runs of
  `TestAGrantReadAfterItsCallersContextEndedIsGivenBack` (its context's arm; its reply's arm gives
  the hold back through `takeGrant`); the rest green.
- K5, `takeGrant`'s give-back removed: red in 22 of 50 runs of
  `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, at "want no hold" (its reply's arm handing
  the hold over), and green in the package's one run, which took the other arm; the rest green.
- K6, `checkGone`'s end removed: red 50 of 50 in `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s
  four cases and `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`, at "once the owner took the
  grant given back: held true, maintenance; want no hold, stopped"; the rest green. Its first run, at
  `5d95445f`, went red as predicted and beyond its prediction:
  `TestTheCheckHoldsGrantIsNeverLostToItsCaller` 2 of 50 at "as the caller that left was answered:
  hold … false", the test's own defect (10 of 200 with no mutation, above) and not the mutation's.
  The other first runs were stopped unread, the test was fixed (`8a7e92d5`), and every item 1
  control was run again there, its prediction unchanged.

Item 2 and item 3, at `41e52338`:

- K8, `deliver`'s expiry refusal dropped: red 50 of 50 in
  `TestACheckHoldExpiredAsItsReleaseSettlesIsNeverHandedOver`'s case with no Stop, "got <nil>
  (reason ""), want reason "lease_invalid""; its case with a Stop first,
  `TestACheckHoldThatExpiresBeforeItsGrantIsNeverHandedOver` and the rest green (eighteen of
  nineteen; the package's other 200).
- K7, `acked`'s delete of a session acknowledged in time from the sessions owed removed (the late
  acknowledgement's own delete kept), run on the invariant's test alone: the mutation reaches every
  check that asks `unsettled()` and the reservation's tests, so the package's run is not this
  control's. Red 50 of 50 in five of its six cases at the watch's finding: a Stop, the check's hold
  interrupting playback, the output going away and a fault at the acknowledgement's step, "handled
  call release: no session held, 1 owed, the unconfirmed release <nil>"; and the Next released to
  play at the shutdown's release deadline, "handled release deadline: no session held, 2 owed, the
  unconfirmed release &{Session:2 …}", where the prediction said its acknowledgement and `<nil>`: the
  case red as predicted, its finding one step earlier, the second session's release past its
  deadline as the shutdown drives the clock. The release past its deadline and acknowledged late
  green, its acknowledgement the late one, whose delete stays.

Round 1's controls, again at `baabfc01`, round 2's last commit of code:

- C1r: `TestACheckHoldIsRefusedBesideAConfigurationReservation` 50 of 50, "the owner granted the
  check's hold, want a refusal "configuring""; the rest green (eighteen of nineteen; the package's
  other 200).
- C2r: `TestACheckHoldDecidesWouldInterruptInItsGrantsStep`'s six cases but "stopped with nothing
  under way", 50 of 50 each, at the refusal without `interrupt`; the rest green.
- C3r: the Stop test's three check cases, `TestACheckHoldsStopDeadlineIsItsOwn`,
  `TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown` and both expiry tests' cases with a Stop
  first (`lease_invalid`, not `stopped`: no Stop marked the hold), 50 of 50 each; OpenHome's
  `TestAnOpenHomeStopEndsTheCheckThroughTheEngine` 50 of 50, "the Stop answered 200 and the check's
  stop signal is open"; "plain maintenance", OpenHome's 501 test and the rest green.
- C4r, the check's command given `context.WithoutCancel(ctx)` (round 1's C4 on round 2's command):
  round 2's caller reads its context through the command alone, so every case whose caller's
  context ends now waits as if it had not, red 50 of 50: at the watchdog in
  `TestACheckHoldAskedByAGoneCallerStopsNothing`'s two cases (as round 1),
  `TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo`'s second case,
  `TestTheCheckHoldsGrantIsNeverLostToItsCaller` and `TestAGrantItsCallerGaveUpAsItWasSentIsEndedByTheOwner`'s
  two cases given up at the context's end; at "want no hold" in
  `TestAGrantReadAfterItsCallersContextEndedIsGivenBack`; the two cases given up at the bound and the
  rest green (fourteen of nineteen; the package's other 196).
- C5 was not run again: the code it removed, round 1's exit of a grant its context outlived, is
  gone, its property now `takeGrant`'s and `giveUp`'s give-back (K5, K4).
- C6r: `TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown` 50 of 50 at its bound, "waited 3s for
  stop to answer"; the rest green.

Every control went red where it was predicted to and nowhere else, but for K6's first run (the
test's defect, fixed before the controls were run again) and the shape of K7's finding in the
Next's case, both above.

### Round 2: runs

- The check tests, outside the build slot, each a single package's run: the nineteen engine
  tests `-race -count=50 -watchdog 10s`: ok, 110.7 s; OpenHome's two `-race -count=50`: ok, 100
  passes, none skipped, TMPDIR under the worktree's `.tmp`. Before them, at `8a7e92d5` the
  rewritten case 1,000 times and the seventeen check tests 200 times each, and the item 2 and
  item 3 tests 200 times each as they were committed: all green.
- The native-integration overlays (`gate.yml`'s two steps): `./cmd/bridge -run
  '^TestNativeIntegration' -timeout 5m -watchdog 10s`: ok, every named test passing;
  `./internal/player/playback`: ok, every named test passing.
- The qualification version: `go run -tags ts_omit_webclient,ts_omit_debugeventbus
  ./scripts/qualification-version -base "$(git merge-base main HEAD)" -head HEAD`, the
  merge-base `6ee6102a`: "qualification version OK"; no production file outside
  `internal/player/engine` names `EnterCheck`, as in round 1.
- `make fmt vet test build-all` in one slot session (P=4, the Makefile's 30m, go1.27.3), the
  records staged, nothing else running: ok in 2,105 s, 91 packages and none failed; cmd/bridge
  1,721.3 s (96 % of its 30m), internal/manifest 1,612.2 s, internal/admin 1,230.8 s,
  internal/backup 662.3 s, internal/player/engine 34.1 s, internal/player/openhome 38.5 s; `make fmt`
  rewrote nothing, and the six release targets built.
- `make docs-check`: ok (`output.md` 99,353 of its 100,000 bytes, `openhome.md` 45,388); before
  the gate, cmd/bridge's four source guards (`TestNoDocblockNamesAnotherDeclaration`,
  `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`, `TestEveryBackgroundGoroutineDrainsOnCleanup`)
  green over the round's code and records. The pinned gofmt (`go run cmd/gofmt -l`, go1.27.3) lists
  none of round 2's Go files.
