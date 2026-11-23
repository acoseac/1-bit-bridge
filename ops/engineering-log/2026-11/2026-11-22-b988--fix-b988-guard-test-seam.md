## 2026-11-22 — B988: the waveform guard's test holds its judgment while it asks during it

Branch `fix/b988-guard-test-seam`, from main `9f98e0c1` (#1551's merge). Two commits, test only, no
production file changed:
- `95ae2f1c`: the hold in the guard's test harness, the test that raced asking while its judgment is
  held, and a case that ends with its judgment held;
- this record.

Every test run below was built with `GOTOOLCHAIN=go1.27.3`, the release tags (`-tags
ts_omit_webclient,ts_omit_debugeventbus`) and `-race`, with `TMPDIR` on disk below the checkout's
`.tmp/`, on Linux amd64 with 16 CPUs. Lines cited are `9f98e0c1`'s unless a commit is named. Each
forced run and each control was predicted in writing before it ran. Each is an overlay (`go test
-overlay`) of `internal/integrity/waveform_regen_test.go` alone, edited on its own lines so that every
line a run reports is the file's own; nothing tracked was written.

### The red

`TestTheGuardJudgesBeforeTheFirstRegenerationAndThenOnceAnInterval` failed in `test -race (rest)` on
Linux, at the first attempt, on #1548's run 44985544107 and #1550's run 45060224227, neither of which
touches `internal/integrity`, at its second `Ask` (`waveform_regen_test.go:306-307`, "an ask while
judging: …, want it to wait for the judgment"). The runs' logs are not read from here; the forced red
and the loaded runs below give the message with its value.

### The premise, by line

`Ask` (`waveform_regen.go:387-410`) answers from the judgment that stands (`:394-402`). With none, it
records the row as pending (`:403`), starts a judgment on a goroutine of its own unless one runs
(`:404-408`), and answers `WaveformJudging` (`:409`), whose doc is that case (`:319-321`). The judgment
(`judge`, `:431-483`) lists the catalog (`:434`), judges it (`:442`) and stamps it (`:444`); only then,
under the lock, does it set `judging` false (`:447`), take the pending rows (`:448-449`) and publish
`last` (`:456`). It logs once the lock is let go (`:465-469`), tells `OnJudged` (`:471-473`), and then
tells `Admit` each pending row it admits (`:474-482`).

So an ask while a judgment is under way, its List not yet returned, answers `WaveformJudging`. That
is B652's design, "a lookup is answered from the judgment that stands, or is told to the next one's
Admit" (`ops/engineering-log.md:88464-88465`), and no defect of the guard. The race is the test's: the
first `Ask` starts the judgment (`:407`), the test asks again at once (`waveform_regen_test.go:306`),
and nothing kept the judgment from publishing (`waveform_regen.go:456`) between the two. Once it has, the second ask is
answered from it: this fixture (4 rows, one curve gone, below the floor of ten) admits `rows[0]`, so
the answer is `WaveformAdmitted`, which prints 0.

`OnJudged` cannot hold a judgment open: it runs after `judging` is false and `last` published
(`waveform_regen.go:446-473`). The hold is in the harness's `List`, before it returns the catalog.

### The fix

At `95ae2f1c`, in `waveform_regen_test.go`:
- `listHold` (`:245-278`): `wait`, on the judgment's goroutine, closes `entered` once and waits for
  `released` outside that Once, and `release` has a Once of its own (`build-ci-tests.md`, the bullet on
  a test that parks a call in a seam); `waitEntered` is bounded at a minute, as `waitJudged` is.
- `newGuardHarness(t, c, hold)` (`:280-314`): a nil hold lets each judgment go at once, as before, so
  the hold is opt-in per harness. Its release is registered after the guard's `Close` (`:305`,
  `:311`), so the cleanups, last-registered-first, let the judgment go before `Close` cancels and joins
  it. `Close` alone, with the judgment parked on a hold that watches no context, gives up once its
  grace (`stopGrace`, 5 s) runs out and leaves it parked.
- The test waits for its judgment to reach the hold (`:359`), asks (`:360-362`), and only then
  releases it (`:363`).
- `TestACaseEndingWhileItsJudgmentIsHeldLetsItGo` (`:393-426`) ends a case while its judgment is held:
  the case's cleanup must take less than `stopGrace` (`:416-418`), and the judgment must have ended
  once the case has, within a minute (`:419-425`).

This applies two of `build-ci-tests.md`'s rules already there (a test waits only for what every
ordering the code allows gives it, and forces the ordering it judges by a barrier; a window between two
statements is reproduced by parking a goroutine in it), so no rule changes.

### The audit

The file's other guard tests, and `waveform_regen_r2_test.go`'s, were read for the same assumption, a
judgment not yet done. None makes it, so none takes the hold:
- `TestTheGuardsRefusalStandsForItsInterval` asserts nothing of its first ask and asks again only
  after the judgment's `OnJudged`; `TestACatalogThatCannotBeReadIsRefused` asks once, asserting nothing
  of the answer, and reads the judgment `OnJudged` is told;
- `TestAClosedGuardAdmitsNothing` holds its judgment in a List of its own (`listed`, `release`);
- `TestAGuardWhoseLogIsBlockedStillAnswersAndCloses` asks once the judgment is published, its line
  held in a blocked log sink after the lock is let go;
- in the test fixed, the first ask (`waveform_regen_test.go:303`) and the ask past the interval
  (`:330`) answer
  `WaveformJudging` whatever the scheduler does, since each finds no judgment that stands in the ask's
  own step.

### Red first, at 9f98e0c1

| run | edit | predicted | seen |
| --- | --- | --- | --- |
| `-count=20` | line 305: `}; h.waitJudged(t)`, so the first judgment is published before the second ask | 20 of 20 red at `:307`, "an ask while judging: 0" | 20 of 20 red at `:307`, "an ask while judging: 0, want it to wait for the judgment" |

### Controls, at 95ae2f1c

| control | edit | predicted | seen |
| --- | --- | --- | --- |
| C1, `-count=20` | `:359`: `hold.release(); h.waitJudged(t)`, so the judgment is released and published before the second ask | 20 of 20 red at the original assertion, now `:361`, with 0 | 20 of 20 red at `:361`, "an ask while judging: 0, want it to wait for the judgment" |
| C2 | `:311`: the release removed | the held case red at `:417` (5 s or more) and, a minute on, at `:424`; the fixed test green | as predicted: `:417`, "the case's cleanup took 5.000876948s, the guard's grace is 5s"; `:424`, "the held judgment is still parked after its case ended"; the fixed test green |
| C3 | the release registered before `Close` (`:305`), so it runs after it | the held case red at `:417` alone; the fixed test green | as predicted: `:417`, 5.00322778 s, and no `:424`; the fixed test green |

Green at `95ae2f1c` with no load, `-count=200`: the fixed test, the held case and the file's three other
guard tests, 200 of 200 each.

### Under load

Eight busy loops and the test pinned together to CPUs 0-7 (`taskset -c 0-7`), half of the 16, the test
at `GOMAXPROCS=4`, its binaries built before the load:

| run | command | seen |
| --- | --- | --- |
| the base's test | `go test -race -tags ts_omit_webclient,ts_omit_debugeventbus -overlay <9f98e0c1's file> -timeout 30m -run '^TestTheGuardJudgesBeforeTheFirstRegenerationAndThenOnceAnInterval$' -count=2000 -v ./internal/integrity/` | 17 of 2,000 red at `:307`, "an ask while judging: 0, want it to wait for the judgment"; 11.0 s |
| the fix's test | the same, without the overlay | 2,000 of 2,000 green; 12.3 s |
| the package | `go test -race -tags ts_omit_webclient,ts_omit_debugeventbus -timeout 45m -p 2 -count=200 -v ./internal/integrity/...` | exit 0 in 2,024 s (the package's own time 2,022 s, under its 45 minutes); 27,400 of 27,400 PASS lines, its 137 tests 200 times each, the fixed test and the held case among them |

The load alone reproduces CI's red at the base, 17 times in 2,000, with the message the forced red
gives. With no load, one run of the package took 5.6 s.

### Checks

- `make fmt vet`, through the shared build lock, at `95ae2f1c` with this record staged: clean, in
  12.7 s, and `go fmt ./...` rewrote nothing (`git status` the same before and after it); `GOOS=linux
  GOARCH=386 make vet`, `GOOS=windows GOARCH=amd64 make vet` and `GOOS=darwin GOARCH=arm64 make vet`
  clean too.
- The source guards, in the same session: `go test -tags ts_omit_webclient,ts_omit_debugeventbus
  -count=1 -run
  '^(TestNoDocblockNamesAnotherDeclaration|TestEveryCitedTestNameExists|TestNoBlankKeepers|TestEveryBackgroundGoroutineDrainsOnCleanup)$'
  -v ./cmd/bridge/`, each passing once.
- `make docs-check`'s seven steps, each run alone: `python3 -B scripts/check-doc-budgets.py
  --self-test`, `python3 -B scripts/check-doc-budgets.py`, `python3 -B scripts/check-engineering-log.py
  --self-test`, `python3 -B scripts/check-engineering-log.py`, `python3 -B scripts/rules-for.py
  --self-test`, `python3 -B scripts/doc-split/move_entry.py --self-test` and `python3 -B
  scripts/doc-split/verify_rules.py --self-test`, each passing, the log's checker reading this entry.
- The qualification tool, as CI's `native-qualification` job runs it: `go run -tags
  ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base 9f98e0c1 -head HEAD`,
  "qualification version OK": no path it guards changed, so `qualificationVersion` does not move.
- go1.27.3's gofmt (`go run cmd/gofmt -l internal/integrity/`): nothing listed.
