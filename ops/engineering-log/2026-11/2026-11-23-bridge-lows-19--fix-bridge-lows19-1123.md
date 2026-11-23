## 2026-11-23 — Bridge lows 19: Lows 17's and Lows 18's review Lows (a test and records)

Branch `fix/bridge-lows19-1123`, from main at `13469404` (Lows 18's merge, #1560). Three Lows of
the reviews of Lows 17 and Lows 18, the brief's binding notes (Z1 to Z7) deciding where they differ
from the items. No production behaviour changes: no production file is touched. Four commits, this
record the last:
- `6c0981b8`, item 1: the write recorder's calls under one mutex;
- `6b6e9787`, item 2: Lows 17's runs name its last commit of code;
- `dc200b6f`, item 3: the XMOS residual's "never" is the embedded list's;
- this record.

Every Go command here ran with `GOTOOLCHAIN=go1.27.3`, `GOPROXY=off`, `GOFLAGS=-mod=readonly`, the
release tags (`ts_omit_webclient,ts_omit_debugeventbus`) and `TMPDIR` on disk below the checkout's
`.tmp/`; `make fmt vet` ran in the shared build lock. Each control was predicted in writing before
it ran and is an overlay of its package (`go test -overlay`, `-vet=off`): nothing tracked was
written, and `git status` was read after each.

### 1. The write recorder's calls, under one mutex

`WriteCallRecorder` (`internal/admin/pairing_cancel_test.go`) appended each call to an exported
slice with no lock, and two checks read the slice: `requireNothingWritten` and the discovery case's
(`pairing_discovery_test.go`, in `package admin_test`). A write a handler left to a goroutine of its
own, during or after the handler, raced with that read, and under `-race` the case then failed as a
data race rather than by name. The slice is private now (`writes`) and one mutex guards it (Z1):
each of `WriteHeader`, `Write`, `WriteString` and `Flush` holds it across its note and its call into
the embedded `httptest.ResponseRecorder`, which is not safe for concurrent use either, and
`Writes()` returns a copy taken under it, which both checks read, their comments moved with them.
The calls counted are the four Lows 17 settled (its X5), never changes to the header map. The
embedded recorder's `Write` and `Flush` call its own `WriteHeader`, never the override, so no call
takes the mutex twice.

This is hardening against a later asynchronous write: today's cancellation cases already read after
the handler returned (the lock case and the discovery case once the handler's goroutine has closed
its `done`, the case of the paths that read no list after calling the handler directly), so none of
them raced.

`TestTheWriteCallRecorderNotesCallsMadeOnAnotherGoroutine` (Z2(b)) makes the four calls on a
goroutine while the test reads `Writes()` a thousand times, nothing ordering a read after any call:
every copy must be the calls made so far, in order, and once the goroutine has ended, `Writes()`
holds the four and the embedded recorder their code, body and flush.

The controls ran four tests each time, under `-race`: L,
`TestAPairingRequestEndedWhileItWaitsForTheLockWritesNothing` (2 subtests); N,
`TestAnEndedPairingRequestOnAPathThatReadsNoListWritesNothing` (6); D,
`TestAPairingRequestThatEndsWhileItsListWaitsChangesNoCredential` (2); R, the recorder's test. Each
came out as predicted:
- C1, write detection (Z2(a)): both pairing handlers, where their context has ended under the
  console's lock, start a goroutine they do not wait for, which writes `Write("late")`, closes a
  started seam, then writes `Write("again")`; L and N wait on the started seam alone (bounded)
  before the nothing-written check, and for the goroutine's end in `t.Cleanup`. L and N red, eight
  lines, each by name at that check: `the <route> wrote [Write("late") Write("again")] to a request
  that had ended, want nothing written`; no race reported; D and R green.
- C1b, C1's handlers and waits over the recorder as at `13469404`: the same eight lines, and the
  data race the item names, eight reports and four subtests failing "race detected during execution
  of test": a read in `requireNothingWritten` beside the late goroutine's `WriteCallRecorder.Write`.
- C2, synchronization (Z2(b)): the recorder's mutex removed (its lock and deferred unlock taken out
  of the five methods): R red by a data race alone, a write in `WriteHeader` beside an earlier read
  in `Writes`, its own assertions passing; L, N and D green.
- No mutation: all green, 14 passes, no race.

### 2. Lows 17's last commit of code

Lows 17's entry (`ops/engineering-log/2026-11/2026-11-23-bridge-lows-17--fix-bridge-lows17-1123.md`,
`:94`) said its runs were made "At `36d0cb76`, the last commit of code". `36d0cb76` changes B997's
entry alone, as Lows 17's `:11` says. `git log --no-merges --format='%h %s' a1c35128..36d0cb76 --
. ':!ops'` names `828225b6`, `f0bda46d` and `e76c307c`, so the last commit that changed code or
tests on that branch is `828225b6`; `git diff --quiet 828225b6 36d0cb76 -- . ':!ops'` holds, and
`git diff --stat` between them names B997's entry alone (Z5). The line, `:94-96` now, corrected in
place as the log's README allows an entry to be ("An entry is edited only to correct it"), says the
runs at `36d0cb76` held every change of code, since none after `828225b6` changed any, and marks the
correction as this PR's.

### 3. The XMOS residual's "never", the embedded list's

The residual (the design's section 3.2) said "Its IDs never make such a unit a generic board's,
since of `20b1`'s IDs the embedded `sharedReferenceIds` names `20b1:0010` alone". That holds for the
embedded database alone: `genericBridge` reads whatever `Inputs.Profiles` holds, which can be any
parsed database. Each living copy now says, in the same words:

> Under the embedded `profiles.json` that production passes (`dsdPolicyFor`), such a unit's IDs
> never make it a generic board's, since of `20b1`'s IDs its `sharedReferenceIds` names `20b1:0010`
> alone; `genericBridge` reads whatever `Inputs.Profiles` holds.

The copies were found with `git grep -n 'sharedReferenceIds'` and `git grep -n "generic board"`
(Z6) and read:
- Living, changed in `dc200b6f`: the design (`ops/plan-b728-dsd-capability-design.md:558-564`
  before, `:559-567` now; the sentence after it begins "In production" where it began "and in
  production"), and B990's long form (`ops/History-rules-player.md:1087`), whose sentence said "the
  embedded list". Each one's list of amendments names bridge lows 19 (the design's `:538-540`).
  B990's rules line (`.claude/rules/player.md`, the bullet "No first use is asked") carries no such
  sentence, so it is unchanged beside its long form; nor does any other rules file, CLAUDE.md,
  PROTOCOL.md, or a comment under `internal/player/` or `cmd/bridge/`, where "generic board" names
  the policy's condition.
- Merged entries, corrected in place: the log's README allows it, and
  `scripts/check-engineering-log.py` checks an entry's name, heading and form, never its edits.
  - B993's (`ops/engineering-log/2026-11/2026-11-23-b993--feat-b993-b990-lows.md`): `:59-62`
    before, `:60-66` now, the parenthetical in the same words; `:44-45` before, `:44-46` now, its
    account of whose words the list holds naming this scoping.
  - Lows 18's (`ops/engineering-log/2026-11/2026-11-23-bridge-lows-18--fix-bridge-lows18-1123.md`):
    `:40-43` before, `:40-46` now, the same words; `:75-76` before, `:78-80` now, its account of
    B993's line, "why such a unit's IDs never make it one", scoped the same way; `:133-134` before,
    `:137-141` now, "counts as a generic board's only with `Profiles` nil", now "in production, only
    with `Profiles` nil", saying why.
  - B999's (`ops/engineering-log/2026-11/2026-11-23-b999--feat-b999-b993-records.md`), read whole:
    no copy. Its one such claim, `:114-117`, says such a unit needs a clean answer "where it counts
    as a generic board's", which holds under any database; unchanged.

The design's lead (`:542-544` now: `profiles.json`'s `sharedReferenceIds` names `20b1:0010` alone,
so such a unit "is therefore on no generic board") and the long form's ("the list names `20b1:0010`
alone", that bullet's name for the shipped list) name the embedded list already, and are unchanged.

What was read (Z7), at `13469404`, whose files under `cmd/bridge` and `internal/player` this change
leaves as they were:
- `internal/player/policy/policy.go`: `Inputs`, 96-107, whose `Profiles` is "the profile database
  (dacprofile.Default's)", nil knowing none; `decide`'s `vouched`, 622-624, which asks
  `genericBridge` only of a unit no profile knows; `genericBridge`, 689-695: no database, or the
  unit's IDs among `rd.in.Profiles`' shared reference IDs.
- `internal/player/dacprofile/match.go:55-63`: `SharedReference`, the vendor and product IDs against
  the database it is asked of.
- `internal/player/dacprofile/dacprofile.go`: `Default`, 348-352, `Parse` of the embedded bytes
  (346); `Parse`, from 354, takes any bytes.
- `internal/player/dacprofile/profiles.json:3-29`: the shipped `sharedReferenceIds`, whose one
  `20b1` ID is `0010` (25-26).
- `internal/player/playback/dsd_policy.go`: `DSDPolicy.Profiles`, 59-60, nil knowing no DAC; the
  reading's `policy.Inputs`, 528, `Profiles: p.Profiles`.
- `cmd/bridge/serve_player_dsd.go`: `dsdPolicyFor`, 117-130, `Profiles` from `dacprofile.Default()`
  (123, 127), nil with a Warn line where that errs (125); its callers outside tests,
  `serve_player.go:551` and `output_play.go:471`.

What they give: under the embedded database, a `20b1` unit no profile knows whose product ID is not
`0010` is no generic board's; under a database whose `sharedReferenceIds` lists its IDs, it is one;
and production passes the embedded database or, where that does not load, none.

### Runs

On linux/amd64 (16 CPUs), each run to its end.

| Run | Result |
|---|---|
| `go vet ./internal/admin/`, item 1 in the tree | clean |
| L, N, D and R, `-race -count=1 -v`, item 1 in the tree, before its commit | ok: 14 passes, none failed or skipped, no race |
| `gofmt -l` (`go run cmd/gofmt`) of the two test files | nothing listed; a misformatted probe file beside them was listed |
| Controls C1, C1b and C2, at `6c0981b8` | as predicted, above; `git status --porcelain` empty after each |
| `go test -race -count=20 -run 'Pairing' ./internal/admin/` (`GOMAXPROCS=8`, in the background while the records were edited, which no test of the package reads), the 23 tests the pattern selects | ok, 343 s: 460 of 460 runs and 480 subtests passed, none failed or skipped, no race; L, N and D 20 runs each, their subtests 40, 120 and 40 |
| R alone, `-race -count=20` (the pattern does not select it) | ok: 20 of 20, no race |
| `go test -count=1 -v ./internal/player/policy/...`, item 3 in the tree | ok: 67 tests and 36 subtests passed, none failed or skipped, among them the two that parse section 3.2's table, `TestTheEffectivePolicyTable` (its six rows) and `TestAnExplicitChoiceNeverConvertsWrittenOrMigrated` |
| `make fmt vet`, in the shared build lock, at `dc200b6f` | exit 0; `make fmt` rewrote nothing (`git status` clean after it) |
| `make docs-check`, this entry staged as it stood before these last three rows | ok, its seven steps: the budgets' self-test and check (CLAUDE.md 92.6 %, `output.md` 99.9 %), the log's self-test and check (the archive frozen at 7,162,775 bytes, 47 entries since, this one among them), and the self-tests of `rules-for.py`, `move_entry.py` and `verify_rules.py` |
| cmd/bridge's three source guards (Z4), `TestEveryCitedTestNameExists`, `TestNoDocblockNamesAnotherDeclaration` and `TestNoBlankKeepers`, `-count=1 -v`, this entry staged as above | ok: the three passed |
| Control C3: this entry, staged as above, with one more line on disk citing a test that does not exist | Red as predicted: `TestEveryCitedTestNameExists` named that name and this entry's path alone (`cited_test_names_test.go:74`). The entry's bytes were written back from memory, the same by SHA-256 as the staged blob, and `git status --porcelain` showed this entry staged and nothing else |

PROTOCOL.md is not touched. No `make test` or `make build-all` ran: no production file changed, and
the test change is one package's, run above. What only CI verifies: the macOS and Windows legs.
