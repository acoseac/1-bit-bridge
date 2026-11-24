## 2026-11-24 — B728 B12 PR 4a: a stopped admission reads nothing, and every answer keeps its order

Branch `feat/b12-pr4-check-routes`, at `2fc0eed0` before this change. Seven places, each checked by
line at that head, where the DSD check's admission or its stop could still answer otherwise than the
backend decided, or where a test could pass with the code wrong. Three were defects: each was shown
red at `2fc0eed0`, made green and given a control. Four were tests that could not fail for what
they name: each was strengthened so that it passes at `2fc0eed0`, fails with its mutation, and
passes again without it. The production change is 54 added and 14 removed lines (19 of the added
carry comments), in `internal/player/playback` alone; nothing in production calls the check yet
(PR 4b).

### The changes

1. **A stopped admission's queued reading.** The admission's reading takes the refreshes' lock on a
   goroutine of the backend's; one still waiting for that lock when the hold's stop signal came was
   answered `stop` at once, then took the lock once it was free and read the box all the same.
   `refreshDSDWithin` now takes the stop signal and asks it once it holds the lock, before the box
   is read (`errCheckStopped`, nothing read).
2. **An empty slot's stop.** `StopCheck` with no check in the slot read the policy after the step
   that found the slot empty, so a check admitted between the two answered `none` beside that
   check's `preparing`. The pair is now taken in that step; a backend with no DSD policy, which
   admits no check, answers its own policy as before.
3. **The order of a reading's refusals.** The admission's reading answered its wakes in an order of
   its own: the stop's word before the request's end when both had come, the stop's word or
   `dac_unavailable` once serve's stop had closed the checks, and `dac_unavailable` beside a stop
   signal already come. Every wake but the reading's success is now refused in the slot's order:
   the request's error, serve's stop (`serve_stopping`, no epoch), the stop's word, then
   `dac_unavailable` for a reading that failed.
4. **The probe case's own open (a test).** `TestTheCheckOwnsTheSlotAgainstProbesAndTones`'s probe
   case closed its stop signal as soon as the check was admitted, so a check that stalled before its
   open until stopped passed it. It now keeps the signal open until the fake kernel sees the check's
   own open, after the probe's.
5. **The handle's Done (a test).** `TestEachCheckHasItsOwnHandle` asked `Done` again once the close
   was let go, so an accessor answering an orphan channel while the cleanup is pending passed it
   whenever the cleanup won. It now saves the channel while the close is held and waits on that one.
6. **`StopCheck`'s lock between its reads (a test).** The stop's test let its check's cleanup end
   before the seam it held was let go, so both reads saw the end with or without the lock.
   `TestAStopsTwoReadsHoldTheBackendsLock` holds `StopCheck` between its two reads, the check's run
   held short of its cleanup, and asks the backend's lock, which the publication of the check's end
   takes; `TestAStopsAnswerIsOneReadingOfItsCheck` keeps its case before the answer is read.
7. **The parked reading's publication (a test).** The case whose request ends while its reading is
   held checked the count, the join, no open and no record, but never that the reading let go
   publishes. The box now reports another firmware while the reading is held, and the case asks
   for its publication, under a token of its own, once the backend is joined.

One test seam, `Seams.DSDStep`, holds the three steps the tests need: "queued" (a reading that finds
the refreshes' lock held), "check read failed" (the admission's failed reading before its refusal)
and "stop read" (between `StopCheck`'s two reads, under `b.mu`).

### Red first

At `2fc0eed0`: the four changed production files as that head has them, with the seam as a name and
its calls where the base's steps are (and `CheckStopAnswering` in the empty branch, after its
unlock); each test alone, `-watchdog 10s`. The predictions were written before the run; every one
held.

| | Red at the base | Green at the base, as predicted |
|---|---|---|
| 1 | `TestAStoppedAdmissionsQueuedReadingReadsNothing` (the box read twice past the rig's setup) | |
| 2 | `TestAnEmptySlotsStopAnswersThePairItFound/a_check_admitted_after_its_step` (publication 3, the check in it, for the 2 found) | the backend with no DSD policy |
| 3 | `TestTheAdmissionsReadingKeepsTheRefusalsOrder`: the request ended beside the stop signal (`stop` at the first of 32 repetitions), serve's stop before the stop signal (`stop`), serve's stop before the reading fails and the stop signal once a failed reading answered (`dac_unavailable`) | a reading that fails |
| 4 to 7 | | `TestTheCheckOwnsTheSlotAgainstProbesAndTones`, `TestEachCheckHasItsOwnHandle`, `TestAStopsTwoReadsHoldTheBackendsLock`, `TestAnAdmissionsEndIsAnsweredWhileItsReadingIsHeld` (their reds are their controls') |

### Controls

Each a mutated copy of one production file at `6d3ec548` given to `go test` by `-overlay`, no
tracked file edited, run over every check test of both packages. A census first, an overlay that
changes nothing and prints wherever a mutation would decide otherwise, named the tests each one
reaches; the predictions were written from those tests' assertions before any control ran. Eleven
controls, each red exactly where predicted and nowhere else:

| | Mutation | Red |
|---|---|---|
| C1 | the stop signal no longer asked once the lock is held | the queued reading's test |
| C2 | an empty slot's pair read after its step, as at the base | the check admitted after that step |
| C2b | an empty slot's pair projected whatever it is, nil included | the backend with no DSD policy (a nil pointer's panic; the binary's other tests run again) |
| C3a | the reading's stop arm answering the stop's word at once, as at the base | the request ended beside the stop signal; serve's stop before the stop signal |
| C3b | the stop's word decided before serve's stop | serve's stop before the stop signal |
| C3c | a failed reading answering `dac_unavailable` at once, the request's error alone before it | serve's stop before the reading fails; the stop signal once a failed reading answered |
| C4 | a check admitted after waiting for the slot stalling just before its open until it is stopped | the probe case, at its wait for the check's own open (the stopped case passes) |
| C5 | `Done` answering an orphan channel while the cleanup is pending, `Cleanup` unchanged | `TestEachCheckHasItsOwnHandle`, at its wait on the saved channel |
| C6 | `StopCheck`'s two reads made without the backend's lock, the step between them kept | `TestAStopsTwoReadsHoldTheBackendsLock`, at its ask of the lock |
| C7 | a reading whose request ended between the box's reading and its publication publishing nothing | the parked reading's case, at its publication |

### Decisions

- **The stop is asked at the lock by the reading itself** (`refreshDSDWithin` takes the signal), not
  through a context derived from the request's and cancelled as the admission returns: that cancel
  comes after the signal, and a reading that took the lock between the two would read.
- **A refusal the reading's wake decides carries the pair under the backend's lock**, as the slot's
  step does, serve's stop with no epoch, as there.
- **One seam named by its step** holds the three steps the tests need, in place of three seams.
- **C4 stalls only a check admitted after waiting for the slot**, so the stopped case's completed
  check (the record it keeps) and the probe's own open are untouched, and the playback package's
  checks with no unit to locate end `output_gone` before the stall.

### Runs

On the build host, `go1.27.3` with the release tags, `TMPDIR` under the worktree's `.tmp`:

- The gate, in one build slot session at `83a052dc`: `make fmt` (nothing rewritten), `make vet`,
  `make test` (`-race -p 4 -count=1 ./...`: 2,128 s, 93 packages ok, none failed; cmd/bridge
  1,960 s, internal/manifest 1,619 s, internal/admin 1,407 s), `make build-all` (25 s), `make
  docs-check`, and `go vet ./...` for GOOS linux, darwin and windows: each exit 0, the tree clean
  after.
- Before it, alone: the playback, alsapcm and api packages whole, without the race detector (the
  gate ran them with it; alsapcm's tests drive the backend, api's censuses read playback's
  source), the changed tests by name, and cmd/bridge's source guards
  (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
  `TestEveryBackgroundGoroutineDrainsOnCleanup`) with `make docs-check` over the records.
- `-race -count=50` at `6d3ec548`, pinned to half the host's cores, each lane alone: five playback
  tests (250 passes: the two new ones, and `TestAnEndedRequestAdmitsNothing`,
  `TestAStopBeforeTheSlotIsTakenEndsTheAdmission` and `TestTheSlotsBoundKeepsTheRefusalsOrder`,
  on the paths the change reaches) and eight alsapcm tests (400: the two new ones, the three
  strengthened, and `TestAStopBeforeTheSlotEndsTheAdmission`, `TestAnEndedRequestOpensNothing` and
  `TestAStopsAnswerIsOneReadingOfItsCheck`); none failed.
- Both native-integration overlays as CI runs them, at `83a052dc`: 16 `TestNativeIntegration` tests
  in cmd/bridge and 33 in playback passed, every one CI requires among them.
- `scripts/qualification-version -base da3c1098 -head HEAD`: OK, at version 59.
- The iOS fixture exporters (manifest, API, cmd/bridge, `-p=1`) at `83a052dc` and at the base: the
  same six files, three byte-identical (`dsd_outputs.json`, `dsd_dac_outputs.json`,
  `dsd_choice_answers.json`) and the other three identical once their clock times are masked;
  against the export before this change, the same but for the provenance's commit. No golden was
  written.

### What is owed

As the entries before it: PR 4b (with the hold's release and 204 within DELETE's deadline while a
reading is held), the owner's listen of the chime as built, the hardware tier and the iOS mirror.
`.claude/rules/output.md` stands at 99,967 of its 100,000 bytes: PR 4b moves an entry's long form
out before it adds its own.
