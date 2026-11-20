## 2026-11-20 — Bridge lows 12: the lost refresh's record says what the code did; the tone beside GET; #1526's record Lows; one case's cleanups

Branch `fix/bridge-lows12`, from main `d4c5d062` (#1526's merge, B951), with #1525's merge,
`6ee6102a`, below it: the brief required both on main first. Six commits:
- `3ada6d76`: item 1, the record of the lost refresh;
- `ec27b608`: item A2, the tone's long form;
- `2940ee07`: item A3(a), the boot rollback's stop guarantee;
- `7ebba334`: items A3(b) and (c), the reader's case on Windows, and two pins;
- `512d99f7`: item A3(d), the stop case's cleanups;
- this record.

Records, rule lines and one test's order; no production code. Lines cited are `d4c5d062`'s unless
a commit is named. Every command below ran from the checkout's root with `GOTOOLCHAIN=go1.27.3`,
each `go test` with the release tags (`-tags ts_omit_webclient,ts_omit_debugeventbus`,
`Makefile:9`) and `TMPDIR` on disk in a scratch folder below the checkout's `.tmp/`. Each premise
was read again by line at the base; none was skipped.

### Item 1: the refresh a write asks, before its fix

What the records said. The long form of `output.md`'s choice-write bullet
(`ops/History-rules-output.md:868`) and #1525's record
(`ops/engineering-log/2026-11/2026-11-20-b9p3bfu--fix-b9p3b-followups.md:234-246`) said that a
tick that replaced a write's kept run while another refresh was held left the write's refresh
unmade, and that a run begun between the ask's mark and its force took the write's refresh
unforced, which the idle rule skipped; the record added that two more ways out lost it too.

The code before the fix. The merged run takes the mark before it asks its own context
(`internal/player/playback/dsd_work.go:131-139` at `6ee6102a`, the same at the base), so it
cannot show the bug. The lines before it are `cb763203`'s, the commit before `61018df8` on
#1525's branch:
- `coalesced.ask` keeps the work last asked (`dsd_work.go:36`);
- the refreshes' run did nothing while its own context had ended, and took the mark only inside
  that check (`:128-129`);
- `askChoiceRefresh` set the mark (`:142`), then asked with its force (`:143`), which
  `RequestRefreshDSD` stores (`:120-122`);
- the mark's one reader ran the `ChoiceRefreshed` seam (`dsd_choice.go:691-695`), and `Seams`
  is nil in production (`backend.go:71-73`);
- in serve every refresh but a write's is asked on the supervisor's `work`
  (`cmd/bridge/serve_player.go:593-608`; a migration's part through `dsdPartChanged`,
  `serve_player_dsd.go:95`; a probe's through its own ask, `dsd_work.go:92` at `cb763203`), which
  ends as `watch` returns (`serve_player.go:594`): at its context's end, serve's player stopping
  (`:621-622`), or once the engine has stopped on its own, which only a panic in it does
  (`:631-633`).

What it did, run. A throwaway internal test of the package, laid over it with `go test -overlay`
(nothing written into the tree), once with `dsd_work.go` and `backend.go` from `cb763203` (they
differ from the merge's in `dsd_work.go`'s run and in comments alone) and once over the tree as
it is, reading the published choice and both flags after each step, on the rig of
`TestTheRefreshAWriteAsksIsMadeForcedAndHeardEnding` (the native state published, then the
store's state set to convert):
`go test -overlay <overlay.json> -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -v -timeout 120s -run '<the two throwaway functions>' ./internal/player/playback/`.

| interleaving | at `cb763203` | at the merge |
| --- | --- | --- |
| the write's kept run replaced, while a refresh was held in its read of the card's controls, by an ask on a context that had ended | `native` still published, both flags set, the seam never run; one more ask, unforced, on a live context then made it: `convert`, both flags clear, the seam run once | `convert`, both flags clear, the seam run once |
| the same, the later ask's context live | `convert`, both flags clear, the seam run once | the same |
| a run begun between the mark and the force (the mark set, a run asked unforced and held in the seam, then the ask's second half) | the seam ran with `native` still published, the idle rule having skipped the run's reading; the write's own run, kept behind it, then published `convert`, the seam not run again | the seam ran with `convert` published |

As corrected. Both records now say that a later ask's run made the write's refresh, forced, where
its context was live as the run began, and skipped it only where that context had ended, leaving
the refresh unmade, the mark and the force set and the seam not run: in serve, a refresh asked on
`work` once the supervisor had ended. The flag left set was nothing a user could see: it drove a
seam production leaves unset. The refresh skipped was a real reading and publication of the
policy, and once the supervisor has ended no refresh it asks runs: a later run on a live context
took both flags and made it, and where none came, what it would have published stayed
unpublished. The run between the marks said the seam early, for a reading the idle rule could
skip, and the write's own run made the refresh. The History's long form (868) cites the fix at
`6ee6102a` and the bug at `cb763203`; #1525's record is corrected in place (234-260 at
`3ada6d76`), its panic way as it was, a closing note saying what it said and naming the lines.
The rule line, `.claude/rules/output.md:107`, states the fixed behaviour and its Don't ("let a
later ask's ended context or the idle rule skip the refresh a write asks"), not the bug, so it is
unchanged, and so is the long form's bold head.

### Item A2: the console's tone beside GET

What was true. B601's long form of the tone's level bullet (`ops/History-rules-output.md:888`)
said that, no observation being published, "GET /v1/outputs reads the player stopped while the
tone sounds". A play beside the tone reads otherwise: the engine marks the transport `Playing`
and publishes it (`internal/player/engine/transport.go:634`, `:640`) before it calls the
backend's Start (`:649`), which ends the tone (`internal/player/playback/backend.go:980`) and
waits for its close, at most `ToneYield` (from `:993`), while GET maps `Playing` to `playing`
(`cmd/bridge/serve_player_output.go:403-404`). PROTOCOL.md's bridge transport line (952) says the
tone itself changes neither `state` nor `playback`.

As corrected (888 at `ec27b608`): the tone itself changes neither `state` nor `playback` in GET
/v1/outputs, as PROTOCOL.md's bridge section says, and a play that ends the tone reads `playing`
from its start, before the tone has let the DAC go. `.claude/rules/output.md`'s tone bullet (118)
says the tone publishes no observation and nothing of GET, so it is unchanged. #1525's record
named this sentence as a follow-up (289-293 at the base).

### Item A3(a): the boot rollback's stop guarantee

What was true. B898's rule line forbade acting at boot "on what the backup answered once serve's
stop has come" (`.claude/rules/config-lifecycle.md:624`, and its twin, the bold head of the
History's B898 section, `ops/History-rules-config-lifecycle.md:3641`), and its long form said
that a stop "once it has answered and before anything is written, leaves the install as it is
too" (3795-3796). `bootHousekeepingWith` asks serve's context once after the backup's answer
(`cmd/bridge/main.go:2744`, the look at `:2754`), whatever it answered, and never again before
the clear for a backup of another version (`:2783`) or the veto's write (`:2796`) and the
restore. The comment above the look says so (2748-2753), as B951's record does under "What stays
open".

As corrected, in one commit (`2940ee07`): the Don't forbids acting on the answer "once the look
after its answer has found serve's stop", in the rule line and its twin, and the long form
(3795-3809 at `2940ee07`) says that a stop that look finds, one that came while the backup
answered among them, leaves the install as it is, and that one coming after it is not asked
again: the clear, or the veto's write and the restore, go on.

### Item A3(b): the reader's case on Windows

What was true. The History's paragraph on an identity that cannot be read (4081-4089) and B898FU's
record (`ops/engineering-log/2026-11/2026-11-19-b898fu--fix-b898-followup-1119.md`: its head note,
14-16; its reds, 539-541; its Windows paragraph, 569-571) said that
`TestTheCrashRollbackPutsBackABackupAnotherHandleReads` passed on Windows at `2534dc60`, before
the fix, from CI run 44590032503 being red on
`TestTheCrashRollbackTellsABackupWhoseIdentityCannotBeReadFromAChange` alone, as round 4's
review reports it. B951's record says that run's log was not to hand (145-151), and B951 took
the reader's case out of B894's two pin lists on that premise.

The run, asked again, from GitHub's REST API, unauthenticated, through a scratch script
(`python3 -I -B .tmp/_bridgelows12/scripts/ci_fetch.py`, which writes only below the checkout's
`.tmp/`). The run (the repository's `actions/runs/44590032503`) is `gate` on `2534dc60`, branch
`fix/b898-followup-1119`, attempt 1, a failure. Of its jobs (`…/jobs`), `test (windows-latest)`,
`test (macos-latest)` and `test -race (bridge 2/4)` failed, each at its `test` step, and every
other succeeded. The Windows job's log (`actions/jobs/135793232187/logs`) answered 403, "Must
have admin rights to Repository.", and each failed job's check-run annotations hold one failure,
"Process completed with exit code 1.". So which tests failed on Windows could not be read here;
and the native leg runs `go test` without `-v` (`.github/workflows/gate.yml:637`), so even its log
would name only the failures, never a pass.

As corrected (`7ebba334`), at all four places: the run's review reports only the identity case
red, and whether its Windows leg ran the reader's case is not recorded. B898FU's record is corrected in
place (14-18, 540-545 and 573-578 at `7ebba334`), each note saying what B951's words said. The
reader's case is back in B894's two pin lists, between the identity case and the link case, where
B951 took it out (`.claude/rules/config-lifecycle.md:635`, and the History's B894 bullet, 3896 at
`7ebba334`); the History (4087-4097 at `7ebba334`) says that it guards that a reader changes
nothing, that it stands among the pins, and that B951 took it out on an unconfirmed premise. The
paragraph's own reasoning, that an open asking no access takes part in no sharing, so a reader
did not make the old read fail, stands, and the History now says that no run confirms it either.

### Item A3(c): the window case's pin

`TestTheBootRollbackSaysItsWindowEndedWhereTheBackupsAskOutlastedIt` was cited in the History's
prose alone (3823), while its sibling, the stop case, sat in B898's two pin lists. It pins the
rule's "clear an install at boot over a backup that did not say its version": the start whose
window ended during the backup's ask keeps the marker, and says so. It joins both lists beside its
sibling (`.claude/rules/config-lifecycle.md:624`, History 3641). The rules file is 99,888 bytes at
`7ebba334`, under its 100,000.

### Item A3(d): the stop case's cleanups

What was true. `TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered` registered
`drainLoopOnCleanup` and the park's release (`cmd/bridge/serve_held_binary_test.go:875-876`) after
its housekeeping's goroutine had started (855), so a check added between the two would return
from the case with nothing watching the housekeeping.

The change (`512d99f7`): both are registered just before the `go` statement, in the same order,
the drain first, so the cleanups still run the release first; the comment says why. Nothing else
in the case moved. `TestEveryBackgroundGoroutineDrainsOnCleanup` asks only that the function that
launches the goroutine call a drain helper (`cmd/bridge/background_drain_test.go:232`), wherever.

The controls, predicted in writing before any ran, laid over the file with `go test -overlay`
(nothing written into the tree), the case alone:
`go test -overlay <overlay.json> -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 120s -v -run '^TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered$' ./cmd/bridge/`.
Each mutated variant has two insertions: E, a cleanup registered right after the case's channels
are made, so run after the drain and the release, that errors when the housekeeping has not ended
within 5 s; and F, a `t.Fatal` right after the `go` statement.

| | the file | predicted | seen |
| --- | --- | --- | --- |
| K0 | `512d99f7`'s, no mutation | pass | as predicted, 0.11 s |
| K1 | `512d99f7`'s, with E and F | F's line alone; E silent, no drain give-up, within seconds | as predicted, 0.06 s |
| K2 | `7ebba334`'s, the order before the change, with E and F | F's line, then E's after its 5 s: nothing watched the housekeeping | as predicted, 5.07 s |

The tree was clean after each.

### Runs, before this record's commit

- The case and the drain guard under the race detector, at `512d99f7`:
  `go test -tags ts_omit_webclient,ts_omit_debugeventbus -race -count=20 -timeout 30m -v -run '^(TestTheBootRollbackWritesNothingForAStopOnceItsBackupHasAnswered|TestEveryBackgroundGoroutineDrainsOnCleanup)$' ./cmd/bridge/`:
  each passed 20 times, none failed or skipped, no data race; `cmd/bridge` 45.4 s.
- Item 1's run and A3(d)'s controls, above.
- With this record staged on `512d99f7`: the log checker, `python3 -I -B
  scripts/check-engineering-log.py`, ok, "21 entries since"; the budgets, `python3 -I -B
  scripts/check-doc-budgets.py`, ok; and the citation guard,
  `go test -tags ts_omit_webclient,ts_omit_debugeventbus -count=1 -timeout 30m -run '^TestEveryCitedTestNameExists$' -v ./cmd/bridge/`:
  "--- PASS: TestEveryCitedTestNameExists (0.88s)".

Controls that the two checks read the edited files, predicted in writing before any ran, each
mutation on disk for its run alone, the bytes restored from memory after it and the tree checked
after each. The guard ran from one prebuilt binary (`go test -c -tags
ts_omit_webclient,ts_omit_debugeventbus -o <binary> ./cmd/bridge/`, then `<binary> -test.run
'^TestEveryCitedTestNameExists$' -test.count=1 -test.timeout 120s` from `cmd/bridge`):

| | mutation | predicted | seen |
| --- | --- | --- | --- |
| G0 | none | pass | as predicted |
| G1 | B894's restored pin in the rule line (635) suffixed `Ghost` | the guard red, exit 1, one error, naming it "(cited by .claude/rules/config-lifecycle.md)" | as predicted, at `cited_test_names_test.go:74` |
| G2 | B898's new pin in the History's bold head (3641) suffixed `Ghost` | as G1, "(cited by ops/History-rules-config-lifecycle.md)" | as predicted |
| G3 | this record's citation of the window case suffixed `Ghost` | as G1, naming this record | as predicted |
| L1 | this record with a second heading line of the entry's level at its end | the log checker exit 1, naming this record alone | as predicted, "holds 2 … lines outside fenced code, not one" |

The gate on this record's commit is in the branch's report: a record holds no run made on its own
commit.
