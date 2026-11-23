## 2026-11-23 — B728 B12 PR 3, round 3: a generic board's native check can start

Round 3 of B12 PR 3 (branch `feat/b12-pr3-backend-check`), at round 2's head `10fea278`. It merges
main, then fixes a hazard B990 brought to the check: since B990 an explicit `native` on a DAC no row
knows whose USB IDs are a generic board's is refused `native_check_required` at each rate until a
listener's latest answer there is clean, while the check admitted only a tuple the effective set
plays. That answer could never be earned. The refusal now carries the native plan the answer would
give, its candidate, which the check alone may play.

### The merge

Main at `13469404` (B990, B634 PR 1, B989, B993, B997, B999, and two batches of review Lows) was
merged with a merge commit, `2e0028a1`, its parents `10fea278` and `13469404`. Two files
conflicted, and each was resolved to main plus this PR's own delta:

- `internal/player/qualification/version.go`: 58, this PR's bump, kept above main's 57 (B990's).
- `ops/b470-native-qualification.md`: every version reference at 58; the history keeps main's
  "Version 57 covers B990" paragraph, then B12 PR 0's and this PR's, whose last sentence says both
  B12 PRs took 57 after main's and are renumbered above main's at their merges, after the 0.2.4 tag.

Two more were reconciled with B990's policy, with no textual conflict, after the merged tree's
tests failed on them:

- `TestTheCheckAdmitsOnlyWhatTheSetPlays`: the row of a tuple whose first use was not acknowledged
  went (B990 asks no first use), and native chosen on a DAC no profile knows, which played, is
  refused `native_check_required` (no list reads, so its IDs are a generic board's).
- `publishedBackend`, the white-box gate tests' backend, takes the shipped DAC list, as main's
  `fixtureSessionBackend` does, so the fixture DAC's native plays.

After the merge and before any change, `make fmt vet test` in one build-slot session: ok in
2,132 s. `make fmt` rewrote nothing and vet was clean; 93 packages passed and none failed (cmd/bridge
1,966 s, internal/manifest 1,630 s, internal/admin 1,413 s, internal/alsapcm 145 s,
internal/player/playback 5.3 s, internal/player/policy 3.0 s).

### The premises, by line, on the merged tree

- `decide`'s explicit native (`policy.go` `:649-660` before the round): `!offered`
  (`native_not_offered`), `n.Rejected` (`listener_rejected`), `!vouched` (`native_check_required`),
  `!under` (`dsd_rate_above_choice`), then the native plan; `vouched` is
  `s.Known || n.Clean || !rd.genericBridge()` (`:624`), and `genericBridge` is a missing list or
  one of its shared reference IDs (`:693-695`). Under `auto` an unvouched rate goes to DoP or to
  conversion and is never `native_check_required`.
- The check's admission read the set's plan at the rate alone (`dsd_check.go` `:307`), and the
  rate's refusal word answered every refused rate (`:311-312`).
- The check's run: `nodeIdentity` (`dsd_policy.go` `:1000-1028`) reads the admitted set's identity and, for
  DoP, its profile; `noteDriverAccepted` (`:1102-1115`) the set's key, stamp and firmware and the
  plan it is handed; `enginePlan` (`:313-320`) and `tupleTransport` (`:1043-1048`) the plan they are
  handed. None reads the set's plan at the rate: in production only `selectFor` (`:337`, a session's)
  and the check's admission do. So the check runs by a candidate as by any plan, and no helper
  changed.
- Where an answer is recorded: a listener's answer is written through the state's one writer, `dsdstate.Store.Update`
  (`store.go` `:296`): `updateLocked` (`:311-357`) runs the caller's change on a copy (`:332-335`),
  normalizes and encodes it (`:336-337`), and the encoding checks the state (`state.go` `:674`);
  `DAC.check`'s rules for an answer (`:877-893`) are its transport, a DSD rate (`validRate`, DSD64 to
  DSD256), its firmware's form, its word, its date and one answer each, and `dsdstate` imports no
  policy package. Nothing asks whether the rate is in the effective set. No production path writes a
  listener's answer yet: B12 PR 4's route will, so the tests simulate the answer through the store.

### The change

- `policy.Plan.Candidate`: on a `native_check_required` refusal alone, what the explicit native
  decides at the rate with the DAC vouched for, attached only where that decision is native. The
  native branch is `explicitNative` now, its refusals in the table's order, and asks itself again
  with vouched true; since the blind spot is asked before the cap, a rate above the cap is refused
  with the same word and carries none.
- `policy.Intersect`, the set a landed choice's write holds to, keeps a candidate only where the
  landed state's is the same plan, so the check plays nothing either state would not let it.
- `dsdSet.same` compares each rate's candidate, its presence and its plan by `policy.Plan.Same`, so a
  change of one alone, the refusal's word the same, publishes a new set; evidence alone is none.
- `admitCheckLocked` takes the candidate for a request of its transport, native. A DoP request at
  that rate keeps the rate's own word, `native_check_required`.
- `judgeCheckLocked` keeps a check while `policy.Reconcile` keeps its plan or the candidate
  published at its rate is that plan (`candidateKeeps`), the admission identity compared first, and
  ends it `policy_changed` otherwise. `candidateKeeps` compares no opened identity: the admission
  identity is compared first, and `checkNode` records the identity opened only once it is that one,
  so the clause could never decide, and a control of it could not go red.
- No session reads a candidate: `selectFor` and `policy.Reconcile` are as they were.

Production: +70 -24 lines, comments included (`policy.go` +29 -11, `choice.go` +9 -3,
`dsd_policy.go` +9 -4, `dsd_check.go` +23 -6), under the round's about 120. No route, protocol
field, mode or state was added, and how a check runs changed only in its judgement.

The design's section 3.2 says what a candidate is, below its table; sections 6.2 and 6.4 say that
admission takes it; `output.md`'s check bullet and its long form, and CLAUDE.md's playback and
policy rows, say so in the same commit; the runbook's version 58 paragraph names the round's guarded
test file.

### The tests

- `TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned` (internal/alsapcm, through the engine's Run,
  the backend and the native sink over the fake kernel): on a generic board's IDs under an explicit
  native, a session's DSD64 is refused `native_check_required` before the check and after it; the
  check of native at DSD64 is admitted by the candidate and plays to complete on the identity it was
  admitted under, its driver_accepted recorded; a listener's clean answer, simulated through the
  state's store, then a refresh, and a session plays DSD64 natively, its sink opened by the same
  plan as the check's (kind, endpoint, format, rate, pause, tail and prefill) and the kernel asked
  the same format and frame rate, DSD_U32_BE at 88,200 frames a second.
- `TestAGenericBoardsCheckTakesItsCandidateAlone` (internal/alsapcm): nothing else let in, each
  refused by its word with the record and the opens as they were: DoP where native waits for a
  listener's answer (`native_check_required`), native above the cap (`native_check_required`), native
  a listener rejected, native at a rate the DAC does not offer, auto (`converted`) and a token
  another reading made.
- `TestTheCandidateIsThePlanACleanAnswerGives` (internal/player/policy): at each rate, the candidate
  is the plan a clean answer at the rate gives, by `Plan.Same` and field by field (outcome, rate,
  transport, format, the tuple's rate, pause, tail and prefill), with the sinks' frame rate the
  one-bit rate over the format's frame from a table of the test's own; four ways the inputs choose
  the tuple: the kernel's pause, none probed, DSD_U32_LE where the probe refused DSD_U32_BE, and
  DSD_U16_LE. No altset is compared.
- `TestOnlyANativeCheckRequiredRefusalCarriesACandidate` (internal/player/policy): thirteen states,
  each rate's word and whether it carries one; `Intersect` over three landed states; and a session's
  reconciliation keeps no occurrence of the candidate's plan.
- `TestACandidatesChangeIsANewSet` (internal/player/playback, white-box): the cap down and up, the
  kernel's pause found, DSD_U32_BE refused where DSD_U32_LE is offered, each a new set at the next
  generation; a driver_accepted alone, none.
- `TestACandidatesCheckIsJudgedByItsCandidate` (internal/alsapcm): another rate's candidate gone
  keeps the check playing to complete; a refresh at its monitor's tick once its driver_accepted is
  recorded publishes no new set and keeps it, and a session's DSD at the rate stays refused after
  it; its candidate gone (the cap below its rate), its pause changed once its driver_accepted was
  recorded, its format changed, and another maker's unit offered the same candidate each end it
  `policy_changed`.
- `TestTheCheckAdmitsOnlyWhatTheSetPlays` drops its row of native chosen on a DAC no profile knows,
  which the candidate now admits: that is the first test's case.

### Red first

At the merged base `2e0028a1`, with a names-only shim (the `Candidate` field, never set) and the
round's test files overlaid (`go test -overlay`, nothing tracked), the predictions written before
the run:

- Red: `TestTheCandidateIsThePlanACleanAnswerGives`, its 12 cases ("the refusal carries no
  candidate"); `TestOnlyANativeCheckRequiredRefusalCarriesACandidate`'s seven cases that want a
  candidate (green: the eight that want none); `TestACandidatesChangeIsANewSet` (no candidate as the
  backend starts, and the four changes publish no new set: at the base the cap moved down to DSD64
  leaves the set as it was, its words the same); `TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned`
  at the check's admission ("refused: native_check_required"); `TestACandidatesCheckIsJudgedByItsCandidate`,
  its six cases at admission.
- Green: `TestAGenericBoardsCheckTakesItsCandidateAlone`, since the base admits none of its rows,
  refused by the same words (its controls are C2 and C3); and the trimmed
  `TestTheCheckAdmitsOnlyWhatTheSetPlays`.

Every prediction matched.

### Controls

Each control is an exact-once replacement of the committed bytes of one production file, overlaid,
the round's seven tests run (internal/alsapcm with `-watchdog 15s`), its predictions written in a
file before the pass.

| Control | Mutation | Red | Result |
|---|---|---|---|
| C1 | admission takes no candidate | `TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned`; `TestACandidatesCheckIsJudgedByItsCandidate`, all six cases | as predicted |
| C2 | admission takes the candidate whatever the transport | `TestAGenericBoardsCheckTakesItsCandidateAlone`: DoP where native waits for a listener's answer (`other_transport`) | as predicted |
| C3 | the candidate ignores the cap | `TestOnlyANativeCheckRequiredRefusalCarriesACandidate`: both capped states, the landed states; `TestAGenericBoardsCheckTakesItsCandidateAlone`: native above the cap (admitted); `TestACandidatesChangeIsANewSet`; `TestACandidatesCheckIsJudgedByItsCandidate`: its candidate gone, another rate's candidate gone | one red more than predicted: another rate's candidate gone, whose premise (the cap moved to DSD128 publishes a new set) the mutation removes; it fails at that premise |
| C4 | `dsdSet.same` compares no candidate | `TestACandidatesChangeIsANewSet`; `TestACandidatesCheckIsJudgedByItsCandidate`: another rate's candidate gone, its candidate gone, its pause changed, its format changed (no new set) | as predicted |
| C5 | the check's reconciliation reads no candidate | `TestACandidatesCheckIsJudgedByItsCandidate`: another rate's candidate gone (the check ended) | as predicted |
| C6 | `Intersect` keeps the current state's candidate | `TestOnlyANativeCheckRequiredRefusalCarriesACandidate`: the landed states | as predicted |
| C7 | the candidate's prefill a millisecond off | `TestTheCandidateIsThePlanACleanAnswerGives`, all 12 cases; `TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned` (the check's open and the session's differ) | as predicted |
| C8 | a session's selection takes the candidate | `TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned` (the first session plays); `TestACandidatesCheckIsJudgedByItsCandidate`: its driver_accepted recorded (the session after it plays) | as predicted |
| C9 | `policy.Reconcile` keeps an occurrence of the candidate's plan | `TestOnlyANativeCheckRequiredRefusalCarriesACandidate`: a session's reconciliation | as predicted |

Every other test of each pass was green. No tracked file was written; the tree was clean after the
passes.

### The token census

The first gate (at `1112ee7b`) failed one test: internal/api's `TestDSDOutputProtocolTokens`,
`refused_reason`. Its census of the words a rate is refused with reads the `refused` calls of the
policy's deciding functions by name, `Effective` and `decide`, and the round moved an explicit
native's refusals into `explicitNative`, so it missed `native_not_offered` and
`native_check_required`, which PROTOCOL.md documents and the policy still sends. `c30e44e7` names
`explicitNative` beside them: the guard red at `1112ee7b` in that gate and green after it. Asking
`explicitNative` again with vouched true, rather than restating its tail in `decide`, keeps the
candidate the same decision as the explicit native's, whatever that decision gains later.

### Runs

- Before the round's commit: the three changed packages' suites under `-race` (internal/player/policy
  2.9 s, internal/player/playback 4.8 s, internal/alsapcm 140.3 s); cmd/bridge's
  `TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers` and
  `TestEveryBackgroundGoroutineDrainsOnCleanup`; and `make docs-check`: each ok.
- Session 1 in the build slot, at `1112ee7b` (go1.27.3, the release tags, TMPDIR under the
  worktree's `.tmp`):
  - `-race -count=50`: internal/alsapcm's 14 check tests (the check file's eleven and this round's three)
    in five lanes of ten, ok in 306 to 316 s each; internal/player/playback's ten check tests, ok in
    8.0 s; internal/player/policy's package, ok in 112.6 s.
  - `make fmt vet test build-all docs-check`: failed in 2,110 s on the census (above). `make fmt`
    rewrote nothing and vet was clean; 92 packages passed and one failed, 15 without tests
    (cmd/bridge 1,968.5 s, internal/manifest 1,622.3 s, internal/admin 1,411.4 s, internal/backup
    658.8 s, internal/alsapcm 147.7 s); build-all and docs-check did not run, make stopping at test.
- Session 2, at `c30e44e7`, 2,163 s:
  - `make fmt vet test build-all docs-check`: ok in 2,142 s. `make fmt` rewrote nothing (`git
    status` clean after it) and vet was clean; 93 packages passed and none failed, 15 without tests
    (cmd/bridge 1,962.1 s, internal/manifest 1,625.4 s, internal/admin 1,409.1 s, internal/backup
    658.9 s, internal/api 150.3 s, internal/alsapcm 147.9 s, internal/player/playback 5.3 s,
    internal/player/policy 3.1 s); `check-build-tags: OK`; the six release targets built; the doc
    checks ok.
  - `go vet` with the release tags as linux/amd64, darwin/arm64, windows/amd64 and linux/386: ok,
    nothing reported (1, 6, 5 and 10 s, the go command replaying a package's cached vet result where
    its inputs are unchanged).
- The qualification tool: `-base 13469404 -head HEAD`, main's tip and the PR's merge base, "qualification
  version OK"; `-base 2e0028a1 -head HEAD`, the merge: "guarded code changed without
  qualificationVersion increase (58 -> 58)", the round's guarded change being internal/alsapcm's two
  test files; no guarded production file changed, and the version stays 58.
- gate.yml's two native-integration steps, through the overlay its script makes, under `-race`:
  `./cmd/bridge` ok in 20.2 s, 16 passes, none skipped, every name the workflow checks passing;
  `./internal/player/playback` ok in 3.1 s, 33 passes, none skipped, every named one passing.
- Once this entry was written: `make docs-check` ok, and cmd/bridge's four source guards green; the
  tree clean.

### What stays open

- No production path records a listener's answer yet: B12 PR 4's route will, through
  `dsdstate.Store.Update`. The tests simulate it.
- A candidate change while a check holds the slot needs a state or probe change the slot otherwise
  refuses (a choice's write is refused `playback_not_stopped` and a probe skipped `check_holds`): the
  tests stand a store's write and a probe's cached answer in for one, as round 1's policy-change
  cases do.
- The line a new set is said with (`logDSDSet`) names each rate's word, not its candidate, so a set
  that moved by a candidate alone is said with the words it had.
- Rounds 1 and 2's residuals, unchanged.
