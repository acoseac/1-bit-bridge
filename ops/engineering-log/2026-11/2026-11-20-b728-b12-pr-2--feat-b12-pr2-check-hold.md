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
answers `cancelled` without `ErrNotAdmitted`; a reply with an error is returned as it is.

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

Each predicted in writing first, an exact-once mutation of one production file at HEAD
(`9d2289a5`) laid by `go test -overlay`, no tracked file touched, the tree clean after each. Each
ran the thirteen engine tests `-race -count=50` and the whole engine package `-race -count=1`
(`-watchdog 10s`); C3 also the two OpenHome tests `-race -count=50`. Every one went red exactly as
predicted and nowhere else.

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
- C5, the drain after admission removed: `TestTheCheckHoldsGrantIsNeverLostToItsCaller` 50 of 50
  (the Hold handed to a caller that had gone); 520 others green.
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
