## 2026-11-24 — B728 B12 PR 4a: the DSD check's backend half of its routes

Branch `feat/b12-pr4-check-routes`, from B12 PR 3's head `da3c1098`. B12 PR 4 wires the player's
DSD check (`ops/plan-b728-dsd-capability-design.md` §6.2 to §7.2) as three routes. Its brief made
the backend changes it asked for count in the PR's budget, and had them stand alone as PR 4a once
they passed about 300 production lines: they came to 460 added and 140 removed (comments included,
150 of the added), so this PR is the backend half, and the routes, serve's adapter and
`PROTOCOL.md` follow as PR 4b. Nothing in production calls anything it adds.

### What it builds

The rules are `output.md`'s second and third bullets under "The player's DSD check (B728 B12)",
their long forms in `ops/History-rules-output.md` (PR 3's section, and PR 4a's).

- **The order.** `reserveCheck` asked the supervisor's gate, the fence and an occupied slot before
  the pair's identity and tuple, so a changed token beside a shut gate answered the gate's word,
  where the route's order (which PR 4b's PROTOCOL.md states) answers the fresh reading first. It
  now asks `serve_stopping`, then the pair published now (`checkAdmits`: `dac_unavailable`,
  `dac_changed`, the tuple's word),
  then the gate, the fence and the slot; a reading that fails answers `dac_unavailable`; every
  refusal past the reading carries the pair it was decided on (`CheckRefused.Policy` and
  `Epoch`).
- **The precheck.** `CheckAdmits` is the same predicate over the pair published now, with no
  I/O, for the routes' precheck: the candidate of a generic board's native is admitted, a native
  above the cap (no candidate) and DoP where native waits for a listener's answer are refused, as
  the admission answers them.
- **The handle and the stop's answer.** `Check` answers a `CheckHandle` of its own check (its
  record and pair, `Done`, `Cleanup`), and `StopCheck` how that check's cleanup ended
  (`CheckCleanup`: none, pending, confirmed, failed) with the pair as it answers. PR 3's stop
  answered true for a cleanup that failed and fenced the device.
- **The member.** `DSDDAC.Check` (id, transport, rate, phase, reason) is published at each
  transition under `b.mu` (`publishCheckLocked`), never behind the refreshes' lock, and attached
  at every store of the pair (`publishLocked`, `withCheckLocked`); a transition beside a choice's
  held reading stores nothing and is owed (`checkOwed`).
- **The answer.** `AnswerCheck` writes a listener's answer through a choice's path (`writeState`,
  which `ChooseDSD` now shares through `stateWrite`), its second decision the check answerable,
  noise and nothing taking the transport's first use away in the same write; a write that reached
  the file makes the check `answered`, a landed one held as a landed choice is.
- **Item 6.** A check is judged by the path that admitted it (`checkHold.byCandidate`): the
  candidate's alone, or the set's plan's alone, the identity first.
- **Item 8.** The no-list admission row in `TestTheCheckAdmitsOnlyWhatTheSetPlays`: native chosen
  on a DAC no profile knows while no list reads is its candidate's check, which plays to
  complete; the comment that claimed it is corrected.
- **Item 9.** The design's §3.2 names the function: decide's explicit native (`explicitNative`)
  asked again with vouched true.

### Decisions

- **The split** at the brief's line, the backend changes alone (above).
- **A DELETE while a check is being admitted is the adapter's**, PR 4b's: it cancels the
  admission's context and waits for that admission to return before it answers. Every wait of
  the admission (the refreshes' lock, the probe's or the tone's slot) ends with that context, and
  an admission that returns has either refused, holding nothing, or handed back a handle the
  adapter then stops. So the backend records no admitting check, and `StopCheck` stays the
  slot's.
- **`answered` is a `CheckPhase`**, after `complete` in `CheckPhases`; internal/api's census of
  the protocol's observation states is unchanged, since the member's field is `Phase`.
- **The activation's judgement keeps its path**: a `driver_accepted` the open records changes
  evidence alone, which neither `policy.Plan.Same` nor `dsdSet.same` reads, so the activation's
  fresh judgement meets the set the admission was judged by
  (`TestACandidatesCheckIsJudgedByItsCandidate/its_driver_accepted_recorded`).
- **The brief's open questions.** Publishing beside device I/O: the brief replaced its default,
  and its rule is built (every transition publishes under `b.mu`, never waiting for the
  refreshes' lock). The check's end: the brief replaced it by the handle, built. The answer's precedence beside a running check
  and the engine's unmapped refusals are the adapter's, PR 4b's; in the backend alone an answer
  that names the check in the slot is refused at the lease (`playback_not_stopped`) or as not
  answerable.
- **Item 7** (a migration retry landing mid-check) is serve's and needs the adapter, so it is PR
  4b's; this PR's rows of item 6 pin the mechanism it would exercise.
- **The census of the choice's outcomes.** `TestEveryChoiceOutcomeHasItsAnswer` (internal/api)
  holds every `ChoiceOutcome` of the backend to one of the API's, named by the PUT's handler. The
  answer shares the write's outcomes and adds one of its own, `check_not_answerable`, which
  `AnswerCheck` alone returns and no route maps until PR 4b's answer route: the census was red on
  it (playback's thirteen outcomes against the API's twelve), and now sets it apart by name,
  failing if it is gone, rather than give the PUT an answer, and PROTOCOL.md a row, for an outcome
  the PUT never meets.

### Red first

Each of the 21 tests run alone at the base: a copy of the tree whose `internal/player/playback`
production files are `da3c1098`'s, with a names-only shim (the new types, constants and methods,
over the base's behaviour). Predicted and seen alike:

- red: every new test (`TestACheckFromAnotherBootIsNotAnswerable`,
  `TestAnAnswerWhosePublicationPanicsKeepsItsWrite`,
  `TestACheckTransitionBesideAHeldChoiceReadingIsOwed`, `TestOnlyACompleteCheckIsAnswered`,
  `TestANoiseAnswerRefusesTheTransportInEveryMode`, `TestAnAnswerIsWrittenOnlyWhileStopped`,
  `TestALandedAnswerHoldsTheSetAndFencesTheWrites`, `TestTheAnswerGoesToTheIdentityTheCheckOpened`,
  `TestAFirmwareChangeBeforeTheAnswerMakesItUnanswerable`, `TestEveryCheckTransitionIsPublished`,
  `TestAStopsAnswerSaysHowItsCleanupEnded`, `TestEachCheckHasItsOwnHandle`,
  `TestTheRoutesPrecheckIsTheBackendsAdmission`); `TestTheChecksPhasesAndReasonsAreTheirLists`
  (`answered`); `TestAParkedMonitorReadingHoldsNoEndOfTheCheck` (its published member); every row
  of `TestTheChecksGateReadsTheDeviceSlot` past the reading (the base's refusals carry no pair, it
  answers a read side with no DAC `dac_changed`, and the three rows of two failures answer the
  slot's word); and `TestACheckIsJudgedByThePathThatAdmittedIt`'s two cross rows (the candidate's
  check once a clean answer makes its plan the set's, the set's plan's once its rate waits for an
  answer again), each ending complete.
- green: `TestACheckIsJudgedByThePathThatAdmittedIt`'s two same-path rows, item 8's row of
  `TestTheCheckAdmitsOnlyWhatTheSetPlays` (green now, as the brief said), and PR 3's tests moved to
  the new API (`TestTheCheckWaitsForAProbeOrAToneAmongTheWaiters`,
  `TestTheChecksCountIsGivenBackOnEveryWayOut`, `TestAnEndedRequestAdmitsNothing`).

### Controls

Each control is a mutated copy of one production file given to `go test` by `-overlay`, so no
tracked file was edited, run over every check test of `internal/alsapcm` and
`internal/player/playback` (each mutated function is reached only through a check, an answer or
the precheck, and those tests are every one that makes one). The predictions were written before
any control ran, from a census: an overlay that logged, for each check test run alone, each point
where a mutation changes what the code decides (both item 6 rules' answers at each judgement, a
store the member would change, a refusal of the pair beside one of the slot, a transition beside a
held reading, the answer's guards, a candidate given while no list reads). Thirteen controls, every
one red exactly where predicted and nowhere else:

| | Mutation | Red |
|---|---|---|
| C1 | item 6: the `\|\|` restored | the two cross rows of `TestACheckIsJudgedByThePathThatAdmittedIt` |
| C2 | item 6: the candidate's path rejected | that test's candidate row across a set that keeps it; `TestACandidatesCheckIsJudgedByItsCandidate/another_rate's_candidate_gone` |
| C3 | item 6: the set's plan's path rejected | that test's plan row across a set that keeps it; `TestAPolicyChangeEndsTheCheck/a_set_change_that_keeps_the_check's_plan` |
| C4 | item 8: no candidate's check without a list | item 8's row |
| C5 | the member built at transitions alone | `TestEveryCheckTransitionIsPublished/from_preparing_to_answered`, `TestOnlyACompleteCheckIsAnswered/a_complete_check,_then_again`, `TestACheckTransitionBesideAHeldChoiceReadingIsOwed`, `TestAnAnswerWhosePublicationPanicsKeepsItsWrite` |
| C6a | the answer's phase check removed | the answered check answered again (`a_complete_check,_then_again`; the second answer of `TestAnAnswerWhosePublicationPanicsKeepsItsWrite`) |
| C6b | an answer recorded for a stopped check (every end but answered admitted, every end dated) | `TestOnlyACompleteCheckIsAnswered/a_check_that_did_not_complete` |
| C7 | the first-use entry kept on noise | `TestANoiseAnswerRefusesTheTransportInEveryMode`'s `noise` and `nothing` |
| C8 | a failed cleanup's stop answered confirmed | `TestAStopsAnswerSaysHowItsCleanupEnded`'s close error and contained cleanup panic |
| C9 | the precedence restored | the three two-failure rows of `TestTheChecksGateReadsTheDeviceSlot`; `TestTheRoutesPrecheckIsTheBackendsAdmission/a_token_and_a_gate_changed_after_the_precheck` |
| C10 | a transition beside a held reading stores its pair | `TestACheckTransitionBesideAHeldChoiceReadingIsOwed` |
| C11a | the precheck admits every `native_check_required` | the precheck's above-cap and DoP rows |
| C11b | the precheck by the set's plan alone | the precheck's candidate row |

C6a showed that the phase check alone is not what refuses a stopped check: its completion time is
zero, set only for a complete check, so the answer's age refuses it too (the census's `A
phase=stopped zero=true`); "an answer recorded for a stopped check" needs both relaxed, C6b. C5's
predicted greens, whose runs store pairs the mutation changes, each read the member only where a
transition stored it last.

The first run of the controls found a defect of the tests: four helpers this PR added or reworked
(`refused`, `published`, `stopped`, and the gate test's request builders with the waiters test's
`asked` and `backend`) closed over the parent test's `t` and were called inside subtests, so C6a's
and C6b's reds were reported on the parent, and a `Fatal` there would have run off the parent's
goroutine. They take the subtest's `t` now (its own commit), and every control was run again on
that commit, all thirteen as predicted. The design's four route controls (the route without the
flag, the engine's hold skipped, the 202 before the grant, DELETE's 204 before the release) and item
7's are PR 4b's: there is no route or adapter here to mutate.

### Runs

On the build host, `go1.27.3` with the release tags, `TMPDIR` under the worktree's `.tmp`:

- The gate, in one build slot session at `1e48359d`: `make fmt` (nothing rewritten), `make vet`,
  `make test` (`-race -p 4 -count=1 ./...`: 2,134 s, 93 packages ok, none failed; cmd/bridge
  1,951 s, internal/manifest 1,619 s, internal/admin 1,408 s), `make build-all` (43 s), `make
  docs-check`, and `go vet ./...` for GOOS linux, darwin and windows: each exit 0, the tree clean
  after.
- Before it, alone: every package whose tests import a changed one (the library resolver,
  dsdstate, qualification and its two scripts, policy, playbacktest, playback, api, alsapcm,
  hotplug and binding), cmd/bridge's DSD, check, tone, golden, player, output and serve tests, and
  its source guards (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`,
  `TestNoBlankKeepers`, `TestEveryBackgroundGoroutineDrainsOnCleanup`). The api run is where the
  census of the choice's outcomes was red (Decisions).
- `-race -count=50`, pinned to half the host's cores: the eight playback tests (400 passes), the
  thirteen alsapcm tests (650) and the census (50); none failed.
- Both native-integration overlays as CI runs them: 16 `TestNativeIntegration` tests in
  cmd/bridge and 33 in playback passed, every one CI requires among them.
- `scripts/qualification-version -base da3c1098 -head HEAD`: OK, at version 59.
- The iOS fixture exporters (manifest, API, cmd/bridge, `-p=1`) at HEAD and at the base: the same
  six files, three byte-identical (`dsd_outputs.json`, `dsd_dac_outputs.json`,
  `dsd_choice_answers.json`), and the manifest's two and the provenance identical once their clock
  times are masked. No golden was
  written: nothing PR 4a adds reaches the wire.

### What is owed

- PR 4b: the three routes and their body checks, serve's adapter (the engine's hold before the
  202, `Hold.Stop`, the admission's cancel on DELETE, `CloseChecks` where tones close), the
  answer's route, `PROTOCOL.md` (§11's routes, member, tokens, error rows, the write limit's list,
  `bridgeDSDCheck`), item 7, the route controls, the guards (the protocol's tables, members and
  tokens, the routes' census, the health flags), the goldens and the fixture exporters
  (`dsd_check_answers.json`), and the routes' real-sink rows in the native-integration overlay's
  suite.
- The owner's listen of the chime as built, before any route exposes the check (the design's
  §6.2, the listening pass's paragraph).
- The hardware tier: a Pi 4 and a real DAC, the chime heard and the preparation timed.
- The iOS mirror (I5), with PR 4b.
