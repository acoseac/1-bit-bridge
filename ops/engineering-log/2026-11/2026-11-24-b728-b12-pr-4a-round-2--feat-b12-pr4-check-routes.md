## 2026-11-24 — B728 B12 PR 4a round 2: the DSD check's admission, stop and answer where an end meets a step

Branch `feat/b12-pr4-check-routes`, round 1's head `4a52775c`. Round 1's findings, each checked by
line at that head, named six places where an end of the request or of the check meets a step of
the backend and the two can disagree. Each item was shown red at `4a52775c`, made green, and given
a control. The production change is 119 added and 42 removed lines (49 of the added are comments),
in `internal/player/playback` alone; nothing in production calls the check yet (PR 4b).

### The items

1. **A request's end during the admission's reading.** The admission read the policy on its
   caller's goroutine, holding the refreshes' lock across a box reading no context interrupts, so a
   cancellation (PR 4b's DELETE cancels the admission's context and waits for it) waited for that
   reading. `readForCheck` now runs it on a goroutine of the backend's, counted until it returns,
   and answers the request's end at once; the reading finishes and publishes as a refresh does and
   takes no slot. A request that has ended starts none. Cancellation and the slot's take are
   ordered by the locked step, which asks the request first: once the check is admitted, a later
   end leaves it the slot's, and the adapter's DELETE ends it through `StopCheck`.
2. **The hold's stop signal before the slot is taken.** Only the watcher an admitted check starts
   read `req.Stop`, so a Stop while the admission waited for a probe let it take the slot, replace
   the last record and race its run to the open, or preempt a tone. The reading's wait, the slot's
   wait and the locked step now read it, refusing with the stop's word (`stop`, the reason a
   running check stopped by its hold ends with), before anything is taken or preempted.
3. **A stop's answer and its pair.** `endCheck` published the check's end, let `b.mu` go and then
   closed `done`, so `StopCheck` past its bound read `pending` beside a published end. `done` now
   closes in the step that publishes the end, and `StopCheck` reads its answer and the pair in one
   step under `b.mu`.
4. **The slot's bound and the refusals' order.** The wait answered its bound in its select,
   `device_busy` with the pair as it stood, past a token that changed meanwhile, and a slot freed
   beside the bound could be taken. Every wake of the wait (the slot, the bound, the request's end,
   the stop) now goes back to the locked step, which decides in the refusals' order against the pair
   it reads, and refuses `device_busy` once the bound has passed, before the slot is looked at.
5. **The answer's age.** It was decided at the answer's admission, before the store's lock, which
   another writer can hold across the ten minutes. The check's completion time is now taken under
   `b.mu` and the age decided inside the state's one write by the policy's clock read there
   (`errCheckNotAnswerable`, `check_not_answerable`, nothing written, the check left complete).
6. **The handle test.** It read the first check's `Done` only after joining the backend, so an
   accessor that answers a channel closed from the start passed it. It now holds a check's close in
   the kernel, asks that check's own `Done` open and its `Cleanup` pending, then the same `Done`
   closed and its cleanup confirmed once the close answers. Test-only: the accessor was right.

Two test seams hold the steps item 3's cases need: `Seams.CheckEnded` (endCheck past its locked
step) and `Seams.CheckStopAnswering` (StopCheck past its wait, before its answer is read).

### Red first

At `4a52775c`: the playback package's production files as that head has them, with the two seams
as names and their calls where the base's steps are (`CheckEnded` in `endCheck`'s gap between its
publication and `done`'s close, `CheckStopAnswering` before `StopCheck`'s reads); each test alone,
`-watchdog 10s`. The predictions were written before the run; every one held.

| Item | Red at the base | Green at the base, as predicted |
|---|---|---|
| 1 | `TestAnAdmissionsEndIsAnsweredWhileItsReadingIsHeld/the_request's_end_while_the_reading_is_held` (no answer before the reading is let go); `TestAnEndedRequestAdmitsNothing/during_its_reading_of_the_policy,_the_console's_tone_in_the_slot` (likewise) | the request's end once the check is admitted |
| 2 | `TestAStopBeforeTheSlotEndsTheAdmission`'s probe and tone cases (no answer while held); `TestAStopBeforeTheSlotIsTakenEndsTheAdmission`'s two cases (no answer; admitted in round 0) | |
| 3 | `TestAStopsAnswerIsOneReadingOfItsCheck/its_bound_at_a_failed_cleanup's_published_end` (`pending` beside `failed` `release_unconfirmed`) | the end between its wait and its answer (the two-reads control's case) |
| 4 | `TestTheSlotsBoundKeepsTheRefusalsOrder`'s changed token (`device_busy` for `dac_changed`) and the bound beside the freed slot (admitted in round 0) | the token it asked with |
| 5 | `TestAnAnswersAgeIsDecidedInTheStoresWrite/past_CheckAnswerable_at_the_write` (written, the check answered) | exactly `CheckAnswerable` at the write |
| 6 | | `TestEachCheckHasItsOwnHandle` (its red is its control's) |

### Controls

Each a mutated copy of `dsd_check.go` at `123d9f27` given to `go test` by `-overlay`, no tracked
file edited, run over every check test of both packages (round 1's and round 2's), predictions
written first. Ten controls, each red exactly where predicted and nowhere else:

| | Mutation | Red |
|---|---|---|
| R1 | item 1: the reading on the request's goroutine again | the reading-held cases of items 1 and 2's tone case |
| R2a | item 2: the slot's wait deaf to the stop | the probe cases |
| R2b | item 2: the reading's wait deaf to the stop | the tone case |
| R2c | item 2: the locked step blind to the stop (the wait then wakes on it again and again) | the probe cases and the stop beside the probe's end |
| R3a | item 3: `done` closed after the end's step, as at the base | the bound at the failed cleanup's published end |
| R3b | item 3: the answer read in two steps, the seam between | the end between the wait and the answer |
| R4a | item 4: the bound answered in the wait, as at the base | the changed token |
| R4b | item 4: no deadline in the locked step (the wait wakes on the bound again and again) | the bound cases of both packages' slot tests and of round 2's |
| R5 | item 5: the age decided at the admission | past `CheckAnswerable` at the write |
| R6 | item 6: `Done` answering a channel closed from the start, `Cleanup` unchanged | `TestEachCheckHasItsOwnHandle` |

### Decisions

- **The stop's word is `stop`**, the reason a running check stopped by its hold's signal ends
  with: no new word. PR 4b maps it, as it maps the engine's own `stopped`.
- **Item 1's PR 4b half** stays the adapter's: cancel the admission, wait for `Check` to return,
  which it now does within the deadline while the reading is held, then answer DELETE, ending an
  admitted check through `StopCheck`.

### Runs

On the build host, `go1.27.3` with the release tags, `TMPDIR` under the worktree's `.tmp`:

- The gate, in one build slot session at `7f771d9a`: `make fmt` (nothing rewritten), `make vet`,
  `make test` (`-race -p 4 -count=1 ./...`: 2,127 s, 93 packages ok, none failed; cmd/bridge
  1,957 s, internal/manifest 1,618 s, internal/admin 1,408 s), `make build-all` (26 s), `make
  docs-check`, and `go vet ./...` for GOOS linux, darwin and windows: each exit 0, the tree clean
  after.
- Before it, alone: the playback, alsapcm and api packages whole, without the race detector (the
  gate ran them with it; alsapcm's tests drive the backend, api's censuses read playback's
  source), the round's tests by name, and cmd/bridge's source guards
  (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
  `TestEveryBackgroundGoroutineDrainsOnCleanup`) with `make docs-check` over the records.
- `-race -count=50`, pinned to half the host's cores, each lane alone: five playback tests (250
  passes: the round's three, and `TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters` and
  `TestTheChecksCountIsGivenBackOnEveryWayOut`, whose admission path changed) and eight alsapcm
  tests (400: the round's five, and `TestAnEndedRequestOpensNothing`,
  `TestServesStopWaitsForEveryCheckItCounted` and `TestTheCheckOwnsTheSlotAgainstProbesAndTones`);
  none failed.
- Both native-integration overlays as CI runs them, at `7f771d9a`: 16 `TestNativeIntegration`
  tests in cmd/bridge and 33 in playback passed, every one CI requires among them.
- `scripts/qualification-version -base da3c1098 -head HEAD`: OK, at version 59.
- The iOS fixture exporters (manifest, API, cmd/bridge, `-p=1`) at `7f771d9a` and at the base: the
  same six files, three byte-identical (`dsd_outputs.json`, `dsd_dac_outputs.json`,
  `dsd_choice_answers.json`) and the other three identical once their clock times are masked;
  against round 1's export, the same but for the provenance's commit. No golden was written.

### What is owed

As round 1's entry: PR 4b (with item 1's half, the hold's release and 204 within DELETE's
deadline while a reading is held), the owner's listen of the chime as built, the hardware tier and
the iOS mirror.
