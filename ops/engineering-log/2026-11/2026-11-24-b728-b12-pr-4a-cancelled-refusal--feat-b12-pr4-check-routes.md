## 2026-11-24 — B728 B12 PR 4a: a request ended before the refusal's lock is answered with its error, and the records carry no labels

Branch `feat/b12-pr4-check-routes`, at `845802a6` before this change, the last of PR 4a's. Findings
checked by line at that head named one defect in the admission's refusal, three gaps in its tests,
and labels in what this PR added. The defect was shown red at `845802a6`, made green and given its
controls; the tests were strengthened; the labels are gone. The production change is 9 added and 4
removed lines (6 of the added are comments), in `internal/player/playback` alone; nothing in
production calls the check yet (PR 4b).

### The changes

1. **A request ended before the refusal's lock.** `readForCheck`, woken by a failed reading or by
   the stop signal, checked its request and then took `b.mu` to decide its refusal, so a request
   ended while it waited for that lock was refused with the stop's word or `serve_stopping`. It
   now asks the request again once it holds `b.mu`, first in the order (the request's error,
   serve's stop, the stop's word, then `dac_unavailable`), after either wake; the earlier check
   stays as the early answer of a request already ended. A seam step, `Seams.DSDStep`'s "check
   refusing", holds the admission between its live request and the lock.
2. **Labels.** The comments, test texts and records this PR added that named a stage of its own
   history now state their facts alone, task ids kept (B728, B12 PR 3, B12 PR 4a); the loops it added count
   repetitions; the entry of the admission's, the stop's and the answer's races is
   `2026-11-24-b728-b12-pr-4a-races--feat-b12-pr4-check-routes.md`, its citation in
   `ops/History-rules-output.md` following, and its controls are named C1 to C6. Literal paths to
   records inherited from PR 3 stay. A survey of every line `da3c1098..HEAD` adds finds none left.
3. **The tests' fidelity.** In `TestTheAdmissionsReadingKeepsTheRefusalsOrder`'s "serve's stop
   before its stop signal" the refusal is received before the held reading is let go, so the case
   cannot pass by the reading's success; "serve's stop before its reading fails" keeps its order,
   since serve's stop wakes nothing there. The cases refused after a contained panic, that test's
   "serve's stop before its reading fails", "its stop signal once a failed reading has answered" and
   "a reading that fails" (`internal/alsapcm/playback_dsd_check_races_test.go`), assert that no
   handle was answered, which both helpers now capture (`checkCall.handle`, set by `ask` and
   `askAsync`); `TestAPolicyChangeEndsTheCheck`'s "a reading that panics"
   (`playback_dsd_check_test.go`) is a running check's end, its handle the check's own. And this
   PR's entries say each package's own invocation: alsapcm's runs with `-watchdog 10s`, the flag
   that package registers, playback's without it.

### Red first

At `845802a6`: its `dsd_check.go` with the step's call where the fixed code makes it; each test
alone, the alsapcm package's with `-watchdog 10s`. The predictions were written before the run;
every one held.

| | `TestARequestEndedBeforeTheRefusalsLockIsAnsweredWithItsError` at the base |
|---|---|
| red | a failed reading, the request's end then the stop signal (`stop`); the stop signal then the request's end (`stop`); serve's stop then the request's end (`serve_stopping`); the stop signal's wake, then the request's end (`stop`) |
| green, as predicted | the stop's word with the request live, after a failed reading and after the stop signal's wake |

`TestTheAdmissionsReadingKeepsTheRefusalsOrder`, strengthened, passed at the base in every case.

### Controls

Each a mutated copy of `dsd_check.go` at `07b200a7` given to `go test` by `-overlay`, no tracked
file edited, run over every check test of both packages, the alsapcm package's with `-watchdog
10s`. A census first, an overlay that changes nothing and prints wherever a mutation would decide
otherwise, named the tests each one reaches; the predictions were written from those tests'
assertions before any control ran. Four controls, each red exactly where predicted and nowhere else:

| | Mutation | Red |
|---|---|---|
| D1 | the request no longer asked again under `b.mu` | the four cases of the request's end |
| D2 | the request asked again under `b.mu`, below serve's stop | serve's stop then the request's end |
| D3 | `Check` answering a handle with no status beside every refusal its admission makes | every case that asserts no handle: the new test's six, five of the refusals' order test, and the 19 cases of `TestTheChecksGateReadsTheDeviceSlot` that the admission refuses |
| D4 | the reading's wait deaf to the stop signal | the new test's two stop-signal wakes, `TestAStoppedAdmissionsQueuedReadingReadsNothing`, the tone case of `TestAStopBeforeTheSlotEndsTheAdmission`, and serve's stop before the stop signal, its refusal now awaited while its reading is held |

### Decisions

- **The request is asked again in the locked step's switch, first**, as the slot's own step asks
  it: placed below serve's stop it would pass both stop-signal cases and answer `serve_stopping`
  to a request ended beside serve's stop (D2).
- **The handshake is a seam step after the live check and before the lock**: the failed reading's
  step comes before that check, so a request ended there is answered by the early check, with or
  without the fix.

### Residuals for PR 4b

- `StopCheck` with no check in the slot, on a backend with no DSD policy, answers `Policy()` read
  after the step that found the slot empty, not in it. It stays as it is: `Policy()` takes
  `policyMu`, and reading it under `b.mu` is not shown to keep the lock order. Such a backend admits
  no check, so nothing a check publishes can come between the two; PR 4b's route decides whether it
  needs that answer from the step.
- A test that can fail by timing: `slotAsIs` (`internal/player/playback/dsd_check_gate_test.go`)
  reads the backend's goroutine count as soon as a check is refused, while the admission's reading,
  which runs on a goroutine the backend counts, gives that count back only once it has sent its
  answer. Under `-race`, `TestTheSlotsBoundKeepsTheRefusalsOrder`'s "the bound and the probe's end
  ready together" failed once in 100 runs of the test (32 repetitions each), "1 goroutines and 0
  writes counted", and passed the other 99; a 50 ms sleep after that send, given by `-overlay`
  with nothing tracked edited, makes it fail every time. The count is right (`Backend.Wait` waits
  for that goroutine): the helper should join the backend's goroutines, bounded, before it reads
  the count, wherever a refusal comes after a reading. The path is not this change's: a reading
  that succeeded returns before the lines it adds.

### Runs

On the build host, `go1.27.3` with the release tags, `TMPDIR` under the worktree's `.tmp`:

- The gate, in one build slot session at `79af2ff5`: `make fmt` (nothing rewritten), `make vet`,
  `make test` (`-race -p 4 -count=1 ./...`: 2,127 s, 93 packages ok, none failed; cmd/bridge
  1,955 s, internal/manifest 1,617 s, internal/admin 1,409 s), `make build-all` (25 s), `make
  docs-check`, and `go vet ./...` for GOOS linux, darwin and windows: each exit 0, the tree clean
  after.
- Before it, alone: the playback and alsapcm packages whole, without the race detector (the gate
  ran them with it), the changed tests by name, and cmd/bridge's source guards
  (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
  `TestEveryBackgroundGoroutineDrainsOnCleanup`) with `make docs-check` over the records.
- `-race -count=50` at `79af2ff5`, pinned to half the host's cores, each lane alone: five alsapcm
  tests (250 passes: the new test, `TestTheAdmissionsReadingKeepsTheRefusalsOrder`,
  `TestAStopBeforeTheSlotEndsTheAdmission`, `TestAStoppedAdmissionsQueuedReadingReadsNothing` and
  `TestTheCheckAdmitsOnlyWhatTheSetPlays`) and three playback tests
  (`TestAStopBeforeTheSlotIsTakenEndsTheAdmission`, `TestTheSlotsBoundKeepsTheRefusalsOrder` and
  `TestAnEndedRequestAdmitsNothing`): 149 passes of 150, the one failure the residual above, then
  150 of 150 in a second lane of 50.
- Both native-integration overlays as CI runs them, at `79af2ff5`: 16 `TestNativeIntegration` tests
  in cmd/bridge and 33 in playback passed, every one CI requires among them.
- `scripts/qualification-version -base da3c1098 -head HEAD`, the candidate committed: OK, at
  version 59.
- The iOS fixture exporters (manifest, API, cmd/bridge, `-p=1`) at `79af2ff5` and at the base: the
  same six files, three byte-identical (`dsd_outputs.json`, `dsd_dac_outputs.json`,
  `dsd_choice_answers.json`) and the other three identical once their clock times are masked;
  against the export before this change, the same but for the provenance's commit.
  `dsd_check_answers.json` is PR 4b's. No golden was written.

### What is owed

As the entries before it: PR 4b (with the hold's release and 204 within DELETE's deadline while a
reading is held, and the residual above), the owner's listen of the chime as built, the hardware
tier and the iOS mirror. `.claude/rules/output.md` stands at 99,968 of its 100,000 bytes: PR 4b
moves an entry's long form out before it adds its own.
