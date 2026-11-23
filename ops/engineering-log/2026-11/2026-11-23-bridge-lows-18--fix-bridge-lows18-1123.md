## 2026-11-23 — Bridge lows 18: the XMOS residual's rates and its generic-board clause

Branch `fix/bridge-lows18-1123`, from main `7ccc1ec7` (B999, #1558, merged).
Documentation only: no code, test, catalog, state schema, protocol or wire
change. Two commits: `821070d2`, the corrections, and the one that adds this
entry. Line numbers in code are `7ccc1ec7`'s, the same as `218163fa`'s, which
B993's and B999's entries cite: no file under `cmd/bridge` or
`internal/player` changed between the two.

### The five points on B999's records

Each was checked against the code and the files before anything changed.

1. **The rates: done.** The residual's list of what keeps native off said "at
   a rate", and its lead "wherever the kernel offers it", with no rate named,
   while B999 compared it at DSD64, DSD128 and DSD256 alone. Those are the
   rates `Effective` decides (`dsdRates`), and `Set.At` refuses DSD512 and
   every other rate as `rate_not_supported`, whatever the mode. The design's
   section 3.2 (`ops/plan-b728-dsd-capability-design.md:537-568` at
   `821070d2`), B990's long form (`ops/History-rules-player.md:1087`) and
   item 6 of B993's entry now state that scope and that refusal. B999's entry
   already does
   (`ops/engineering-log/2026-11/2026-11-23-b999--feat-b999-b993-records.md:67-69`),
   so it is unchanged. Nothing of DSD512 was confirmed by calling
   `nativeOffered` or `decide` directly, which would step past the production
   rate boundary: `Effective` asks neither at such a rate, and the backend
   refuses it before it asks the set.
2. **A mode the list may omit: kept apart, in one sentence.** `decide`'s last
   branch refuses a mode none of the five as `dsd_state_unreadable`. It guards
   against a state built in memory: `dsdstate`'s check refuses any other mode
   at each decode and each write, so a state that reads holds none. The list
   holds once the state reads, so the branch is added to no copy of it; the
   design and the long form say so in one sentence apart from the list. B993's
   entry is not given that sentence: its list, scoped to a state that reads,
   is not wrong without it, and an entry is edited only to correct it.
3. **The generic-board clause's reach: done.** The clause "the unit counting
   as a generic board's (its IDs among `sharedReferenceIds`, or no profile
   database reading)", as B990's long form had it and the other two copies in
   their words, named two ways where, for a `20b1` unit no profile knows whose
   product ID is not `0010`, the shipped code leaves one. Under the embedded
   `profiles.json` that production passes (`dsdPolicyFor`), such a unit's IDs
   never make it a generic board's, since of `20b1`'s IDs its
   `sharedReferenceIds` names `20b1:0010` alone; `genericBridge` reads
   whatever `Inputs.Profiles` holds (that sentence as bridge lows 19 scoped
   it); and the database is absent only where the embedded one does not load,
   `dsdPolicyFor` giving the backend
   `dacprofile.Default()` for serve's player and for `bridge output play`, and
   leaving it nil, with a Warn line, only on that function's error. Each copy
   now names that one way, no profile database reading, and says that in
   production it comes only in a build whose embedded database does not load.
4. **A cited line range: dropped.** B999's entry pins its line numbers to
   `218163fa` (its `:5`), and there
   `ops/plan-b728-dsd-capability-design.md:537-549` is the residual paragraph
   exactly: its first line is 537 and its last 549, a blank line after it. The
   historical citation stays.
5. **The reruns' provenance: dropped.** B993's entry (`:90-98` now) says the
   reruns' facts are as reported and that B999 did not repeat them, and B999's
   (`:46-54`) that B999 took them as given and did not repeat them: both
   already say the facts were given and that B999 did not reproduce them.
   Naming who gave them would add a name these records do not carry. Nothing
   there changed.

### B993's entry, corrected in place

The log's README allows an entry to be edited only to correct it, and
`scripts/check-engineering-log.py` checks an entry's name, heading and form,
never its edits. In item 6 of
`ops/engineering-log/2026-11/2026-11-23-b993--feat-b993-b990-lows.md`:

- `:40-45` before, `:40-49` now: the unit "can play native under `auto` at
  DSD64, DSD128 or DSD256", and the controls are named "at DSD64, DSD128 and
  DSD256, the rates `Effective` decides (`Set.At` refuses DSD512 and every
  other rate as `rate_not_supported`, whatever the mode)", the words marked as
  this PR's beside B999's.
- `:54-56` before, `:58-63` now: "the unit counting as a generic board's (its
  IDs in the shared-reference list, or no profile database reading)" is now
  "no profile database reading, under which every unit counts as a generic
  board's", with why such a unit's IDs never make it one under the embedded
  database production passes (scoped so by bridge lows 19) and where
  production meets an absent database.

Its other lines are unchanged.

### What was read

In `internal/player/policy/policy.go`:

- `dsdRates`, 82-83, and `DSDRates`' doc, 85-88: DSD64, DSD128 and DSD256;
  DSD512 and every other rate refused in every mode.
- `Set.At`, 314-324: any other rate refused `rate_not_supported`
  (`ReasonRateNotSupported`, 35), whatever the set holds.
- `Effective`, 326-350: the gate (334-341) and the loop (344-348) run over
  `dsdRates` alone.
- `decide`, 612-687: the five modes (632-683), then the last branch (684-686),
  `dsd_state_unreadable` (`ReasonStateUnreadable`, 34, which is
  `dsdstate.RefusalStateUnreadable`, `internal/player/dsdstate/state.go:104`).
- `genericBridge`, 689-695: no profile database, or the unit's IDs among the
  shared reference IDs.

Beside them:

- `internal/player/playback/dsd_policy.go`: `selectFor`, 327-345, refuses a
  rate other than DSD64, DSD128 and DSD256 as `rate_not_supported`
  (`ReasonRate`, `open.go:34`; `nativeLogicalRate`, `native.go:110-112`)
  before it asks the set (`d.set.At`, 337); `dsdState`, 366-373, the store's
  state, or `dsdstate.Refusing` where none opened; the policy's inputs, 528,
  whose `Profiles` is the backend's `DSDPolicy.Profiles`; and `setFor`, 545,
  the one call of `policy.Effective` outside tests.
- `cmd/bridge/serve_player_dsd.go`: `dsdPolicyFor`, 110-130, sets `Profiles`
  from `dacprofile.Default()` (123-128) and leaves it nil with a Warn line
  where that errs; its callers outside tests are `serve_player.go:551`,
  serve's player, and `output_play.go:471`, `bridge output play`.
- `internal/player/dacprofile/dacprofile.go`: `Default`, 345-352, parses the
  embedded `profiles.json` at each call (`Parse`, 357-371: a strict decode and
  the lint, nothing else read).
- `internal/player/dacprofile/match.go:55-63`: `SharedReference`, the vendor
  and product IDs alone.
- `internal/player/dacprofile/profiles.json:3-29`: five shared reference IDs,
  `0d8c:0316`, `16d0:071a`, `16d0:0a23`, `20a0:4143` and `20b1:0010`, the one
  `20b1` ID among them; the wildcard profile `kernel.20b1-3089` (826) makes a
  `20b1:3089` unit known, so it is no part of the residual.
- `internal/player/dsdstate/state.go`: `Decode` checks the state it read
  (631), `encode` the state it writes (674); the check refuses a
  `defaultMode` but `auto` or `off` (778-780) and a choice's mode outside the
  five (833-834); `normalized`, 733-753, drops empty entries and sorts,
  changing no mode; `Defaults`, 341, holds `auto`, and `Refusing`, 349-355,
  `off` with the warning that refuses every rate.
- `internal/player/dsdstate/store.go`: `open`, 142-181, holds the defaults,
  `Refusing` or a decoded state; `updateLocked` encodes before it writes or
  holds (337).
- `internal/player/playback/dsd_choice.go:482`: the state a write that landed
  left, which that write's encoding checked first.

What they give: the list is the policy's at DSD64, DSD128 and DSD256, and at
any other rate both the backend and `Set.At` refuse every transport whatever
the mode; a `20b1` unit no profile knows whose product ID is not `0010` counts
as a generic board's, in production, only with `Profiles` nil, which is a build
whose embedded database does not load: `genericBridge` reads whatever
`Inputs.Profiles` holds, and production passes the embedded `profiles.json`
(`dsdPolicyFor`), or nothing where it does not load (scoped so by bridge lows
19); and no state the store holds, nor one
a write left, names a mode outside the five, so `decide`'s last branch guards
only a state built in memory.

### Runs

Each run went to its end in the foreground on go1.27.3 with
`GOTOOLCHAIN=local`, `GOPROXY=off` and `GOFLAGS=-mod=readonly`. Each
control's prediction was written before its run.

| Run | Result |
|---|---|
| `go test -count=1 -v ./internal/player/policy/...`, the corrections in the tree, before the first commit | ok: 67 tests and 36 subtests passed, none failed or skipped, among them the two that parse section 3.2's table, `TestTheEffectivePolicyTable` (its six rows) and `TestAnExplicitChoiceNeverConvertsWrittenOrMigrated` |
| `make docs-check`, the corrections in the tree | ok, its seven steps: the budgets' self-test and check (CLAUDE.md 92.6 %, `output.md` 99.9 %), the log's self-test and check (the archive frozen at 7,162,775 bytes, 44 entries since), and the self-tests of `rules-for.py`, `move_entry.py` and `verify_rules.py` |
| Control, at `821070d2`: the `native` row's otherwise cell without `native_check_required` | Red as predicted, in `TestTheEffectivePolicyTable/native` alone, at `table_test.go:322`, twice: `a clause failing gives "native_check_required", which the otherwise cell does not list`. Its other five rows and the package's other 66 tests passed. The design's bytes were written back from memory, the same by SHA-256 as `821070d2`'s blob, and `git status --porcelain` was empty |
| `make docs-check`, this entry staged | ok, its seven steps, as above, the log's check now counting 45 entries |
| `scripts/check-engineering-log.py`, this entry staged, alone and as one of those steps | ok: the archive frozen at 7,162,775 bytes, 45 entries since |
| `go test -count=1 -run '^TestEveryCitedTestNameExists$' -v ./cmd/bridge`, this entry staged | ok: the guard passed over every tracked doc it reads, the History, B993's entry and this one among them (it exempts `ops/plan-*.md`) |
| Control: this entry, staged, with one more line on disk citing a test that does not exist | Red as predicted: the guard named that name and this entry's path alone (`cited_test_names_test.go:74`). The entry's bytes were written back from memory, the same by SHA-256 as the staged blob, and `git status --porcelain` showed this entry staged and nothing else |

No build, `make test` or `make build-all` ran: the change holds no code. The
tests that read a file it touches ran above, the policy package's (section
3.2's table) and the citation guard (the History and the entries); the
build-site census skips every file it touches as a record, by name
(`scripts/third-party-licenses/tags.go:212-214`).
