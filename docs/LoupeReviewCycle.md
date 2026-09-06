# LOUPE — the recent-work review cycle (bridge)

**Invoke it by name**: *"run LOUPE on last week"*, *"LOUPE the last N commits"*,
*"LOUPE the enrich package"*, *"LOUPE since v0.1.9"*. A loupe is the magnifier
you hold over finished work to check it before it leaves the bench — which is
what this is.

This is the bridge-side twin of `1-bit/docs/LoupeReviewCycle.md`. **The loop is
the same; the measurements, the gate and the field evidence are not.** Where the
iOS side probes an SDK and drives a device, this side compiles a Go probe and
reads a production journal. Sections below say **[BRIDGE]** where the two repos
genuinely diverge.

It is the third named procedure beside **PRISM** (external bug review of source
slabs, `~/dev/gemini-review/`) and **VISTA** (external design/improvement briefs,
`~/dev/gemini-design/`). Both harnesses live OUTSIDE either repo and work from
here unchanged. LOUPE is the whole loop those two feed: from *"what landed
recently"* to *merged, gated, documented, and corrected by field evidence*.
PRISM is one phase of it.

**It runs END TO END without stopping for approval.** Invoking LOUPE authorises
the whole cycle: scope, review, triage, plan, **execute, merge, gate, document**.
Do not pause after the plan to ask whether to proceed — the plan is an artifact
of the run, not a gate on it. The stopping conditions are the ordinary ones: a
finding whose fix is a product decision rather than a correctness fix, a change
that needs a **deploy** to verify before it is safe to merge, a wire-protocol
change that needs its iOS Mirror-PR, or a refusal condition. Everything else is
carried to a merged PR in the same invocation.

**A question is not a stopping condition.** Uncertainty is the normal texture of
this work — *does this defer run on that path? is this map access actually
racy? does `x/sys/windows` really alias `syscall.Errno`?* — and parking the run
to ask converts a five-minute resolution into a day of latency. Resolve it
inside the run, in this order:

1. **Measure.** [BRIDGE] Almost every question that feels like a judgement call
   is checkable, and Go makes it cheap:
   - **Dependency behaviour** — read the actual source:
     `$(go env GOMODCACHE)/github.com/…`. It is on disk; do not infer it from
     docs. This is the bridge's equivalent of grepping the SDK, and it has the
     same authority.
   - **A three-line probe** — a scratch `main.go` (or a `_test.go` in a temp
     package) beats any amount of reasoning about types, aliases, interface
     satisfaction or zero values. The `windows.Errno` false positive in
     CLAUDE.md was settled exactly this way.
   - **Escape analysis / inlining** — `go build -gcflags='-m -m' ./internal/x`.
     Settles "does this allocate", "does `&local` escape", "is this inlined".
   - **Races** — `go test -race -run TestX ./internal/x -count=1`. A claimed
     race that `-race` cannot produce under a targeted stress test is a claim,
     not a finding.
   - **Fuzzing** — `go test ./internal/x -run XXX -fuzz FuzzY -fuzztime 60s
     -fuzzminimizetime 1s`. For a parser claim this is measurement, and the
     `-fuzzminimizetime` gotcha in CLAUDE.md is not optional.
   - **The SQL and the schema** — `sqlite3` against a real `bridge.db` answers
     data-shape questions that reading the writer cannot.

   **Reading a doc comment is inference; compiling a probe is evidence.**
2. **Consult Gemini 3.8 over the API** when measurement cannot settle it — a
   design trade-off, a "what does this library actually do at runtime" question,
   a sanity check on an architecture. See below. This replaces the older
   arrangement in which the agent drafted a question and the *user* pasted it
   into a chat; that hand-off is no longer necessary and must not be
   re-introduced as a reason to stop. Note the bridge CLAUDE.md's
   `## External consultation (Gemini)` still describes the relay-through-user
   shape — it predates the API path and stays accurate as a fallback.
3. **Decide, and record the decision** — in the PR body, the doc comment, or the
   CLAUDE.md entry, with the evidence that settled it. A resolved question that
   leaves no trace becomes the next run's finding.

Escalate to the user only for what is genuinely theirs: product direction, a
deploy to production hardware they own, a wire change that commits the iOS side,
or a change whose blast radius they should choose.

---

## Where the Gemini key lives, and how the harnesses find it

**The key is `~/dev/gemini.api`** — a Google AI Studio key, one line, no
newline-sensitivity issues. Every script below reads it from there by default
and sends it as an `x-goog-api-key` **header**.

- Override with `GEMINI_API_KEY_FILE=/path/to/key` if you ever need a different
  one. There is no in-repo copy and there must not be.
- **Never echo it, never put it in a URL, never let it reach a commit, a log, a
  PR body or a script you write into the repo.** The scratchpad is fine; the
  repo is not.
- Sanity check before a long run: `test -s ~/dev/gemini.api && echo "key present"`
  — which says it exists without printing it.

The three tools, all outside this repo and shared with the iOS side:

| tool | what it is for |
|---|---|
| `~/dev/gemini-review/consult.py` | ONE focused question, mid-run |
| `~/dev/gemini-review/relay.py` | send a prepared PRISM batch directory |
| `~/dev/gemini-review/prism.py` | plan the batches |

### Consulting mid-run

`consult.py` is the sibling of `relay.py`, not a mode of it: `relay.py` is
PRISM-framed (*review these source slabs and report defects*) and batch-shaped,
which is the wrong instruction and the wrong shape for a question.

```bash
python3 ~/dev/gemini-review/consult.py \
  --question-file /tmp/q.md \
  --context internal/manifest/scan.go \
  --out /tmp/answer.md
```

- **Same output-budget trap as the relay**, so the same settings:
  `maxOutputTokens: 65536` plus a `thinkingBudget`. Thinking counts against the
  total, so an answer can otherwise be truncated to a plausible-looking fragment.
- **Ask a real question.** Include what you tried, what you observed, your
  current hypothesis, and the alternatives you are weighing. A question with no
  hypothesis in it gets a survey back, which is the least useful answer.
- **Attach the code with `--context`** rather than describing it. Attaching the
  file is the whole point of doing this over the API.
- The system instruction asks it to separate *documented* from *inferred*, to
  name what would falsify its answer, and to say "measure this" when that is the
  honest response. Read for those markers first.

**A consult is evidence, not a verdict.** [BRIDGE] The bridge's own recorded
case: Gemini called `errors.Is(err, windows.WSAEADDRINUSE)` a HIGH cross-package
type mismatch and proposed `syscall.WSAEADDRINUSE` — and both halves were wrong,
because `x/sys/windows/aliases.go` declares `type Errno = syscall.Errno` (an
alias) and stdlib `syscall` has no such constant on Windows, so the suggested
fix would not compile. **If the module source contradicts a consult, the source
wins — and say so in the write-up rather than silently overriding it.**

---

## Phase 0 — scope the window (or the surface)

**Two shapes.** A *window* ("last week", "since v0.1.9") is the original: base
commit, file count, added lines, feature batches by name. A *surface* ("the
enrich package", "the DLNA/UPnP serving path") scopes to a subsystem instead.

[BRIDGE] A bridge surface is usually **one or more `internal/` packages**, which
makes enumeration exact in a way the iOS side never gets:

```bash
go list ./internal/enrich/... 
git ls-files 'internal/enrich/**/*.go' | grep -v _test.go | xargs wc -l | tail -1
git log --oneline --since=... -- internal/enrich | wc -l
```

Say which shape you are in, because it changes the prior — a surface with a long
hardening history and a big CLAUDE.md section should be expected to refute most
findings, whereas an unswept package should not.

**State the window in the plan.** A review whose scope is "recently" cannot be
checked for completeness later.

### [BRIDGE] The structural check has no rules files to measure

The iOS repo binds invariants to files with `.claude/rules/*.md` globs, and every
iOS LOUPE run since the lyrics one has opened by measuring how many files matched
no glob. **This repo has no `.claude/rules/` at all** — every invariant lives in
`CLAUDE.md`, organised under `## Things that have bitten before` by subsystem
heading. So the equivalent structural question is:

- Does the surface have a `### ` section under `## Things that have bitten
  before`? Name it. If it does not, that absence is itself a finding worth
  recording — it is the same "nothing binds this code to its rules" shape.
- Are that section's claims still true? Grep every symbol, file path, test name
  and count it cites. **Doc drift has been the highest-yield dimension on every
  iOS run, and the dangerous half is never a dead citation — it is a live
  imperative whose literal implementation reintroduces a fixed defect.**
- `ops/` carries prior review artifacts (`audit-*.md`, `bug-review-*.md`) and
  `ops/engineering-log.md`. Read the ones covering this surface before reviewing
  it, or you will re-derive their conclusions.

---

## Phase 1 — review, from more than one direction

Two independent sources, because they fail differently:

1. **PRISM batches, relayed over the API.** Topical slabs (new files whole,
   `-U12`/`-U25` diffs of modified ones, ~1.5–5.5k lines each) through the primer
   at `~/dev/gemini-review/00-PREAMBLE-paste-first.md`, with per-batch
   design-invariant notes so deliberate patterns are not re-reported. You send
   them yourself and triage what comes back.
2. **Targeted read-only agents**, one per package the window touched. They read
   the code with CLAUDE.md in hand, which the external reviewer cannot.

[BRIDGE] **Slab Go by package, not by file.** A Go package is the unit of
compilation and of most invariants, so a batch that carries `scan.go` without
`scan_delete.go` will produce findings about "missing" guards that live in the
sibling. Include the package's `_test.go` files when the finding class is
"is this actually covered" — but they are large here (146k test LOC against 136k
production), so budget them deliberately rather than by default.

### Relaying the batches

```bash
python3 ~/dev/gemini-review/relay.py \
  --batches ~/dev/gemini-review/batches-<scope> \
  --out     ~/dev/gemini-review/responses-<scope> \
  --model   gemini-3.8-flash --workers 3
```

- **The API key defaults to `~/dev/gemini.api`** (see above).
- **Pick the model by listing, not by memory.** `GET /v1beta/models` and read the
  ids back; the family moves. As of this writing `gemini-3.8-flash` is the 3.8
  model exposed (1M in / 64k out), and there is no 3.8 Pro.
- **The preamble + blueprint go in `systemInstruction`; the batch is the user
  turn.** Stateless-per-batch gives every batch an identical clean context, and
  makes the run parallel and resumable — unlike a long chat, where batch N is
  reviewed with 1..N-1 still in context.
- **Resume is keyed on `finish=STOP`, not on the file existing.** Both failure
  shapes — an `[HTTP …]` / `[ERROR …]` transport failure and a
  `finish=MAX_TOKENS` truncation — leave a `.response.md` behind, so an
  `exists()` check would skip them and quietly deliver a partial review.
- **Smoke-test on the smallest batch first** (`--only <stem> --workers 1`).
- **⚠️ `maxOutputTokens` bounds THINKING and visible output TOGETHER, and on
  Gemini 3.x thinking dominates.** Measured: a 1,400-line batch spent ~31.4k
  tokens thinking and emitted ~1.3k of review before hitting a 32,768 cap —
  `finish=MAX_TOKENS` on a review that looked superficially complete. Set
  `maxOutputTokens: 65536` **and** `thinkingConfig: {"thinkingBudget": 24576}`;
  the budget is a hint (59.8k observed against 24.6k), so the load-bearing half
  is the 65536 total. **Verify from `think=` in the response header.**
- **Read the response header line** — `finish=` and `in=`/`think=`/`out=`.
  `finish=MAX_TOKENS` means re-send with `--force`.
- Keep `--workers` small (3 is fine); retries with backoff are built in.

---

## Phase 2 — triage every finding before it reaches a plan

**The baseline is ~70 % false-positive on external review of hardened code, and
~0–20 % on ground nobody has swept.** Do not carry that number as a prior —
verify each finding against the current source and the documented invariants.

- **Refuted findings go in the plan by name**, per area, so the next run does not
  re-raise them.
- **Confirmed false-positive CLASSES** go to `~/dev/deepseek-review/known_fp.md`
  and the PRISM primer, which is what makes the next run quieter.
- **Severity labels are not evidence.** On iOS runs both "HIGH/HIGH" clusters
  were once false while the worst defect sat mid-report at MEDIUM.
- **When two reviewers disagree, the one who MEASURED wins** — neither severity
  nor confidence is the tiebreak.

### [BRIDGE] Go false-positive classes already recorded — cite, don't re-derive

From `CLAUDE.md` (`## Bot-review discipline`, `## External code review`) and
`~/dev/deepseek-review/known_fp.md`:

- **`&local` in an atomic is not a use-after-free** — escape analysis
  heap-promotes it. `-gcflags=-m` proves it in seconds.
- **Defers RUN during panic unwind** — "permanent deadlock on panic" claims that
  assume otherwise are wrong.
- **Builder / `With*` setters are construction-time, not racy.**
- **A partial-file chunk view cannot see the paired `Close`/shutdown elsewhere**
  — check the whole file before accepting a "leak".
- **Check multi-return signatures before "swallowed error".**
- **`windows.Errno` IS `syscall.Errno`** (an alias) — see above.
- **Go's `net/url` refuses `\` in a host**, so guards proposed "after
  `url.Parse`" against a backslash authority are unreachable dead code. A
  regression TEST asserting the refusal is still worth taking; the guard is not.
- **`os.Geteuid` exists on Windows** (returns -1). A `//go:build !windows` tag
  "fixing" a test failure there hides the test from a platform where its other
  branch still works; the fix is a runtime skip.

**Take the accurate half of a wrong finding when there is one** — the backslash
and Windows-fixture cases both yielded a useful test even though the proposed
code change was rejected.

---

## Phase 3 — one plan, written before any code

Per item: severity, the mechanism, the trigger, the fix shape, and what the test
would be. Plus, explicitly: what is **rejected** and why. Write it to the plan
file. Cross-PR invariants caught here cost minutes; caught after merge they cost
a batch.

[BRIDGE] Also record, per item: **does this touch the wire?** A change to
`/v1/*` shapes, DTOs, or `PROTOCOL.md` is a Mirror-PR pair with the iOS repo and
must say so in the plan — including whether `internal/version.ProtocolVersion`
moves. Additive fields do not bump it; breaking changes bump both sides in
lockstep.

## Phase 4 — plan review, and record the dispositions

Review the plan — a fresh adversarial pass over your own findings, or an external
one if the batch is large. Then write down, in the plan: accepted as written,
corrected before adoption, and rejected with the reason.

**This is a checkpoint, not a gate.** Record the dispositions and continue into
execution in the same invocation.

## Phase 5 — execution

**Execution begins automatically once the plan is written** — no separate
go-ahead. A LOUPE run produces MANY PRs; work down the plan by severity, merging
each before opening the next when they touch overlapping files (on a
package-scoped run they usually do — a stack is only worth it for genuinely
disjoint work).

**One PR per SCRIPT RUN. Never chain PRs in a single script.** Build and apply
one PR's changes per script invocation, so a mid-chain failure cannot cascade.
The iOS run that chained them had a mid-chain failure leave the tree dirty, which
made the next PR's `git checkout <branch>` abort, whose own build-failure cleanup
then ran `git checkout -- .` and discarded eleven finished files.

Per PR:

1. **A transactional, count-asserted apply script** in the scratchpad: stage
   every file in memory, assert each anchor's occurrence count, write at the end.
   **The scripts are the source of truth for a batch, not the tree** — that is
   what made the lost work recoverable. Dry-run them read-only first (patch
   `open()`, discard writes) so anchor drift is found before a build.
   [BRIDGE] A count assertion proves UNIQUENESS, not LOCATION. After applying,
   resolve each edit's enclosing function and check it landed where you meant:
   ```bash
   awk '/^func /{fn=$0} /MY_NEW_TEXT/{print NR": "fn}' internal/x/y.go
   ```
   An iOS run put a rollup 350 lines from its intended function this way, on an
   anchor that was genuinely unique.
2. [BRIDGE] `make check` → the affected packages → **commit** → negative control
   → restore → **re-run** → push → open the PR.
   - Inner loop is `make check` (fmt + vet + race test). `make build-all` once
     before pushing.
   - Targeted: `go test ./internal/x/... -run 'TestFoo|TestBar' -count=1 -race`.
   - **`-count=1` is not optional on a negative control.** Go caches test
     results, and a cached PASS read after a mutation is a false green. A source
     edit does invalidate the cache, but `-count=1` removes the question.
3. **Commit BEFORE the control, always.** A control's restore reverts the source,
   so anything uncommitted at that moment is gone. This has bitten in two
   different shapes on the iOS side.
4. **The negative control's own mutation must be count-asserted**, exactly like
   an apply script's, and **verify it is in the source** (`grep` for the mutated
   text) rather than trusting the script's exit code. A regex that silently does
   not match leaves the suite testing unmutated code, which looks identical to a
   clean run.
5. **Predict the red set BY NAME before running**, then reconcile. A prediction
   that misses is informative; an unreconciled count is not.
6. **A control whose BUILD fails proves nothing.** [BRIDGE] In Go a compile error
   makes `go test` report `FAIL … [build failed]` — which is loud, unlike the
   iOS `test-without-building` trap where a stale bundle runs green. Read the
   reason for a FAIL rather than counting them.
7. [BRIDGE] **Reconcile executed against declared.** `go test -v` prints one
   `=== RUN` per test and subtest; `--- PASS` / `--- FAIL` / `--- SKIP` per
   result. A **panic aborts the whole package's binary**, so the tests after it
   in that package never run and the package reports one FAIL — the Go analogue
   of a silently-skipped XCTest case, and the reason to compare counts rather
   than just look for reds:
   ```bash
   go test ./internal/x/... -count=1 -v 2>&1 | tee /tmp/t.log
   grep -c '^=== RUN' /tmp/t.log; grep -c '^\s*--- PASS' /tmp/t.log
   grep -nE 'panic:|FAIL' /tmp/t.log | head
   ```
8. Suite lists: when a rule changes, `grep` for **every** package that calls the
   function, not the one whose name matches the feature.
9. [BRIDGE] **If the change touches a fuzz target's subject, run that target.**
   `-fuzztime 60s -fuzzminimizetime 1s`. A parser change with a green unit suite
   and an unrun fuzz target is not verified.

---

## Phase 6 — bots

[BRIDGE] **Four bots here, not three**: `gemini-code-assist`, `coderabbitai`,
`sonarqubecloud`, and **`github-advanced-security` (CodeQL)**. CodeQL is a real
static analyser on a security ruleset, has no per-day quota, and its findings
deserve the same triage as the others — but it also produces the same class of
confident wrongness, so verify before acting.

Sweep **all** inline comments per PR with no timestamp filter, deduped by
content. Accept what is real. **Decline the rest with evidence in the reply** —
the module source, the failing/passing test, the CLAUDE.md invariant. Cite the
recorded FP classes rather than re-deriving them.

- **Wait for BOTH review bots, not for a timer.** Gemini is prompt; CodeRabbit
  is intermittent and routinely 1–3 minutes behind. Poll until `reviews` reaches
  2 rather than sleeping a fixed interval.
- **A push resets the clock.** Bot reviews carry `submitted_at`; a fix commit
  pushed after them has not been reviewed.
- **⚠️ The bots have quotas and a multi-PR run will exhaust them.** Gemini has a
  DAILY cap; CodeRabbit has a rolling 7-day attempt budget that translates to
  "1 review per hour" once spent. On 2026-09-06 an iOS session ran two batches
  and the second batch's five PRs got **zero** CodeRabbit reviews. Budget them
  as a finite resource, say explicitly which PRs went unreviewed rather than
  implying coverage you do not have, and re-request the next day.
- **Bots post in BURSTS; merging on the first one is how a CRITICAL gets past
  you.** An iOS PR merged ~4 minutes after opening on Gemini's single MEDIUM;
  CodeRabbit then posted a correct CRITICAL — an infinite recursion the PR had
  just wired in.
- **Merging with review comments outstanding is a process failure, not a
  shortcut.** [BRIDGE] CLAUDE.md records bridge PRs #562/#563/#564 each merging
  after one commit with no fix round, deferring one Major and one High into a
  separate remediation batch. Expect two rounds minimum.
- **Sweep again AFTER merging the batch.** Comments keep arriving on merged PRs,
  and a merged PR is the one nobody looks at again.
- **Check four surfaces, not one**: `pulls/N/comments` (inline),
  `pulls/N/reviews` (summaries, which sometimes carry findings the inline pass
  does not), `issues/N/comments`, and the `in_reply_to_id` chains that show which
  inline comments you actually answered.
- **When a bot asks you to make a comment less true, don't.** Name what it asked
  for, keep what is accurate.

---

## Phase 7 — merge

File-disjoint PRs off `main` merge in any order. A **stack** merges bottom-up,
and deleting a base does ONE of two things to its child: GitHub RETARGETS the
child to `main` when it still merges cleanly, or CLOSES it when it conflicts —
in which case the child is recreated as `-v2` by cherry-picking **only its own
commits**, in order, onto the fresh `main`. Decide which case you have BEFORE
touching the PR:

```bash
git merge-tree --write-tree origin/main <child-tip>   # exit 1 = conflicts
git diff --name-only origin/main <tree>               # must list only its files
```

The legacy three-argument `git merge-tree` prints markers as `+<<<<<<<`, so a
bare `^<<<<<<<` grep reads a conflict as clean. **Capture every branch's
own-commit range BEFORE the first merge deletes anything.**

## Phase 8 — the gate

[BRIDGE] On final `main`, the gate is the documented pre-push gate plus its CI
mirror:

```bash
make fmt vet test build-all
```

- `make fmt` / `gofmt.yml`, `make vet` + `make test` + `make build-all` /
  `gate.yml`. Running it locally on merged `main` is the gate; CI is the
  cross-check, not a substitute.
- **`make test` is race-enabled** and covers ~10 packages' worth of the suite in
  the inner loop; the full `./...` run is what the gate needs.
- **On a RAM-constrained box the `-race` + 6-target cross-compile peak can OOM.**
  `make test P=2` lowers Go's `-p` parallelism; `P=$(sysctl -n hw.ncpu)` raises
  it on a roomy box.
- **`make build-all` cross-compiles 6 targets** — it is the leg that catches a
  `//go:build`-guarded file that no longer compiles on a platform you never run.
  Do not skip it because the tests are green.
- **Fuzz targets run their SEED CORPORA under `make test`** and are only truly
  fuzzed nightly in CI. If the batch touched a parser, fuzz that target by hand
  before calling the gate done.
- **Report the numbers**: packages ok, total tests, any skips, and that
  `build-all` produced all six binaries. "Tests pass" is not a gate result.

## Phase 9 — write down what generalises

[BRIDGE] A dated entry in `CLAUDE.md` — there are no rules files here, so
everything lands in the subsystem section under `## Things that have bitten
before`, and a cross-cutting run gets its own `### <date> — LOUPE on <surface>`
entry. Include the invariants, the rejected-and-why list, and the **process**
failures. **Correct any existing claim the batch falsified — a stale fact in
that file is more dangerous than an absent one.** Docs-only changes go direct to
`main`, per the bridge's own convention.

Longer-form artifacts belong in `ops/` beside the existing `audit-*.md` /
`bug-review-*.md`, with a pointer from CLAUDE.md — an unlinked doc is nearly as
invisible as no doc.

## Phase 10 — the field loop, which is where the cycle actually ends

[BRIDGE] Merging is not the end, and the bridge's "device" is a **running
deployment**. Put a single consolidated verification checklist in
`~/Desktop/to-do/`, then read what the deployment sends back:

- **The journal is ground truth.** `journalctl -u 1-bit-bridge` status
  histograms beat screenshots and recollection. The bridge CLAUDE.md and
  `ops/deployment-runbook.md` carry the diagnosis recipes — including that
  `/dlna/file/...` requests do NOT appear in `serve.log` (they route through
  `TelemetryMiddleware`), so their absence is not evidence.
- **Deploy targets, in increasing blast radius**: the local test fixture
  (`/tmp/bridge-live`, see CLAUDE.md `## Local test fixture`), the Windows
  home-pc install, `bridge.ars.md`, and the public demo VM `bridge.1-bit.app`.
  The runbook owns the procedures; LOUPE only says *which* to use and that a
  fix touching the scan or deletion passes needs a REAL library behind it.
- **A deploy kicks a ~10 minute startup scan**, which silently defers client
  syncs. A verification run that ignores this reads as "the fix did nothing".
- **Measured data outranks the docs.** When a journal contradicts a CLAUDE.md
  claim, the journal wins and the claim gets corrected the same day.
- **An operator's plain-language report is evidence.**

---

## Tooling notes that cost real time

Most of these were paid for on the iOS side and apply verbatim; the
**[BRIDGE]** ones are Go/this-repo specific.

- **zsh does not word-split an unquoted expansion.** `run_pkgs $LIST` passes the
  whole list as ONE argument and silently runs nothing. Use an array, or
  `${=VAR}`.
- **`wc -l` pads with leading spaces**, so `[ "$(… | wc -l)" = "0" ]` never
  matches. Compare numerically.
- **Never guard a command with a process check whose own command line contains
  the pattern** — `pgrep -f "go test"` matches the wrapper shell. Wait on a
  captured PID (`while kill -0 $PID`) instead.
- **Never pipe a long build/test to `tail`** — nothing appears until it exits.
  Redirect to a **uniquely named** log and poll it; a reused name lets a stale
  file be read as a fresh result.
- **[BRIDGE] Polling tells you about PROGRESS, never COMPLETION.** Wait on the
  captured PID. `go test ./...` prints a per-package `ok`/`FAIL` line, so a grep
  for `FAIL` against a still-running log is answering a question about a partial
  run — the Go shape of the iOS `Executed N tests` trap that once produced a
  phantom 228-test regression.
- **A python heredoc writing Go can eat an escape level.** Prefer message
  strings with no inner quotes; watch for full-width lookalikes.
- **⚠️ Backticks inside a double-quoted `git commit -m "…"` are COMMAND
  SUBSTITUTION** — and Go prose is full of them. **Write the message to a file
  and use `git commit -F`**, then check `git log -1 --format=%B` before pushing.
- **[BRIDGE] Use triple-SINGLE-quotes for python apply scripts.** Go source
  contains `"` and backticks freely (raw string literals!) and never `'''`.
- **`timeout` does not exist on macOS** (it is `gtimeout`, from coreutils).
- **Design the negative control BEFORE running it — it audits the tests you just
  wrote.** Asking "what would the control do?" has caught brand-new tests that
  would have passed either way.
- **A block rewrite can swallow neighbours.** Rewrite by named anchor per test,
  not by span, and reconcile declared against executed afterwards.
- **[BRIDGE] Reconcile with `grep -c '^func Test'` per FILE, and remember
  subtests inflate the executed count.** `t.Run` produces its own `=== RUN`
  lines, so executed > declared is normal; what matters is that no package
  reports fewer than it declares, and that no package aborts on a panic.
- **[BRIDGE] Watch the disk.** A long multi-PR session accumulates Go build and
  test caches; `go clean -cache -testcache` reclaims them. On 2026-09-06 an iOS
  session filled the disk with per-PR derived data and the failure surfaced as
  two unrelated tests failing with "the file doesn't exist" — not as a disk
  error. **When unrelated tests start failing, check `df -h /` before reading
  the failures.**
- **[BRIDGE] `go vet` is part of the gate, not a nicety.** It catches the
  printf-arg and lock-copy classes that reviewers routinely file as findings —
  running it first makes several of those triage steps unnecessary.
