## 2026-11-21 — Bridge lows 14: the chime package's Lows (B975), the store case's cutoff (B977), and B976's collision claim

Branch `fix/bridge-lows14`, from main `124eef53` (#1540's merge, one merge past the brief's floor
`04db6164`; it touches neither `internal/player/dsdchime` nor `cmd/bridge`, so the brief's lines are
the base's). Eleven commits, this record the last:
- `7ea2c996`, item 6 red: `TestEachCallerHasAChimeOfItsOwn` alone, over `Get`'s cached pointer;
- `0b805450`, item 6: `Get` answers a copy;
- `d0bdf239`, item 8 red: the asks counted again and a build on an ended context's allocations;
- `15c238af`, item 8: a build asks its context first;
- `b09b0876`, item 4: the own-rate share counts each tone section alone;
- `0a91bce2`, item 5: each edge held to its floor;
- `d6775cf7`, items 2, 3 and item 8's second nit: comments, and the plan's B12 row;
- `49e7c5a2`, items 9 to 11: the store case's 20 s cutoff removed, its comment and its messages,
  the B972 entry corrected;
- `a1582b68`, item 9: the loop's own give-up, which `49e7c5a2` added, removed again;
- `abc6fc9e`, Part C: the B976 entry corrected;
- this record.

Every Go command here ran with `GOTOOLCHAIN=go1.27.3`, the release tags (`-tags
ts_omit_webclient,ts_omit_debugeventbus`) and `TMPDIR` on disk in a scratch folder below the
checkout's `.tmp/`. Each control was predicted in writing before it ran and is an overlay (`go test
-overlay`) of the files it changes, nothing tracked written, `git status` unchanged after each; a
control's hook in a test file replaces one line with one line, so every line a run reports is the
file's own. No item touches a qualification-guarded path (below).

### The premises, at the base

Each was read again by line at `124eef53` and holds as the brief states it, item 1's as a premise
that does not hold (confirmed below): `ReadAt`'s refusal in `int64` and its slice (`chime.go:132`,
`:135`); the libm calls and the products that may fuse in `listen_test.go` (`:105`, `:126`, `:185`,
`:240`, `:375-379`, `:425`); `TestTheChimeIsPacked`'s `TotalAlloc` (`chime_test.go:342`); the
own-rate share over whole planes (`listen_test.go:306`, `:308`'s 0.4, `:280`'s words); the edges
only logged (`:387`, then a `t.Logf`); `Get`'s cached pointer (`chime.go:159`, `:31`'s words); the
cancelled build's contract (`:27-28`); `build`'s planes before its first ask (`:232`, `:288`) and
`TestTheChimeStopsWithItsContext`'s count (`chime_test.go:273`), which calls `build`, so the counts
become 63, 123 and 243 (K2); the plan's B12 row (`ops/plan-b728-dsd-capability-design.md:1926`);
the store case's cutoff (`serve_probation_test.go:729`, `:731`, `:740`, `:743`), `waitForCount`'s
bound (`:73-80`, `serve_wait_test.go:34`), its comment (`:716`) and its two messages (`:742`,
`:744`); the fixture's port 0 (`serve_refused_install_test.go:81`) and `ReachableEndpoints`' return
before its walk (`internal/api/api.go:2407-2408`); and, #1540 being merged, B976's claim
(`ops/engineering-log/2026-11/2026-11-21-b976--feat-b976-kiwi-ears-ad1-row.md:111-113`). One
premise stated beside item 5 holds only in part (item 5, below).

### Item 1: 32-bit offsets, nothing to change

`ReadAt` compares `off` with the plane's length in `int64` before it slices, and a slice expression
whose index is an `int64` is checked by its whole value on a 32-bit platform, never cut to an `int`.
Run natively as linux/386 (`GOARCH=386 go test`, an `int` of 32 bits), a scratch test overlaid on the
package read at 846,720 (the plane's length), 2^31, 2^32, 2^32 + 5 (which a cut to 32 bits would
read as 5), 2^32 + 846,712, 2^62 and the largest `int64`: each answered 0 and `io.EOF` and wrote
nothing into its planes; at 5 it read the plane's bytes 5 to 12. Nothing was changed.

### Item 2: the listening tests' floats, a comment and the margins observed (R2)

A comment above `fft` (`d6775cf7`) says why `listen_test.go` is not held to `chime.go`'s arithmetic
(it calls libm, and its products may fuse, so pinning them would leave the libm calls), what it reads
exactly everywhere (the bits measured are the goldens', the joins' offsets integer sums and a
division, the own-rate share a count), and that its dB margins are observed, not a bound. No claim
is made of the numerical error's size beyond what was measured here, and no portability bound.

Measured on linux/amd64, by a scratch test overlaid on the package that prints each figure the
tests decide by, the tail search's candidates included:
- the joins the tail search chose, against its −101 dB SA-CD: DSD64 880 Hz with no tail −104.00
  (3.00 dB inside), DSD64 1320 Hz at 4 frames −102.59 (1.59, the smallest; it is the join at 1.9 s),
  DSD128 880 Hz with none −108.55 (7.55), DSD128 1320 Hz at 1 frame −106.59 (5.59), DSD256 880 and
  1320 Hz with none −110.19 and −117.05 (9.19, 16.05); each chosen end offset within 0.0008 of
  `0x69`'s, against 0.01;
- the candidates it rejected, each by its level alone, its end offset within 0.0015 (R2: the chosen
  tail's margin says nothing of these): DSD64 1320 Hz at 0, 1, 2 and 3 frames −97.99, −81.70,
  −85.26 and −87.91 (3.01, 19.30, 15.74 and 13.09 dB outside −101), and DSD128 1320 Hz with none
  −98.46 (2.54 outside, the closest);
- the joins test's own bound, −100: the closest join, DSD64's right channel at 1.9 s, 2.59 dB inside;
- the residuals: DSD64 1320 Hz 37.26 dB under the tone, 2.26 dB over its floor of 35, the other five
  3.84 to 7.92 dB over theirs; the edges, item 5's: 6.12 dB inside at the least.

Across builds, the same scratch test, every figure at full precision: linux/386, run natively, gave
every one bit for bit as linux/amd64; linux/amd64 built at `GOAMD64=v3`, whose compiler fuses `x*y +
z` into `VFMADD231SD` (in `go build -gcflags=-S` of a two-line function; at `GOAMD64=v1` it emits
`MULSD` and `ADDSD`), changed 70 of the 158 fractional figures, by 8.6e-11 dB at most (a join's
level), 3.3e-11 for a tail candidate's, 2.8e-13 for an edge's and 1.3e-13 for a residual; every
share and offset was the same. arm64 was not run here; GitHub's check runs for #1538's head
(`e40debd3`) record its arm64 leg, `test (macos-latest)`, as a success. These are three builds'
differences, observed, and no bound on another platform's.

### Item 3: `TestTheChimeIsPacked`'s constraint, a comment

Its comment (`d6775cf7`) says its bytes are `runtime.MemStats.TotalAlloc`'s, which counts what every
goroutine of the process allocates (`testing.AllocsPerRun` counts allocations, not bytes), so the
count holds only while no test in the package runs in parallel or leaves a goroutine running. No
test of the package calls `t.Parallel`, and each goroutine a test starts returns before it ends.

### Item 4: the own-rate share, each tone section alone

`TestTheChimeIsModulatedAtItsOwnRate` (`b09b0876`) counts the bits that differ from a lower rate's
chime repeated over each channel's tone section alone, bytes `(padFrames+ch*sectionFrames)*bpf` on,
`sectionFrames*bpf` long, `bpf` the higher rate's, and asserts and logs each (K3): DSD128 left and
right against DSD64's, 0.4996 and 0.4996; DSD256 left and right against DSD64's, 0.4998 and 0.5000,
and against DSD128's, 0.5002 and 0.5000. Over whole planes they had read 0.4999 to 0.5001: the pads
and each tone's other channel, 70.8 % of a plane, are `0x69` in both chimes, and `0x69` differs from
itself repeated in half its bits.

The control (predicted in writing; `15c238af`'s `chime.go` with DSD128's left section's first half
written from DSD64's bits each repeated twice): the old count (`15c238af`'s test) passed, 0.427 for
DSD128's left; the new one failed once, "DSD128 channel 0: only 0.2497 of its section's samples
differ from DSD64's repeated", every other pair about 0.5 and passing. Over the unchanged chime the
new count passes with the six shares above. The test's comment, the long form and the B12 PR 1
entry (`ops/engineering-log/2026-11/2026-11-21-b728-b12-pr-1--feat-b12-pr1-chime.md:162-169`,
corrected in place) no longer say a repetition would differ in none.

### Item 5: each edge held to its floor

`TestTheChimesInBandResidualIsUnderItsFloor` (`0a91bce2`) holds each 20 ms edge's largest in-band
component, `m.level - 20*log10(spur)`, to `floors[rate]`, as each tone's residual is held. On the
unchanged chime the twelve edges lie 41.12 to 106.61 dB under the tone, the smallest margin 6.12 dB
(DSD64 1320 Hz's fall), so the exit condition did not arise.

The control (predicted in writing; `b09b0876`'s `chime.go` without the fall's reset, `if j ==
t.n-t.ramp { ey, ex = 0, 1 }`): the new case failed at all six falls, "the 880 Hz tone's fall leaves
its largest in-band component 11.8 dB under the tone, not 35" and the same at every rate and tone,
and at no rise; the old case passed. Run against `b09b0876`'s eighteen tests one at a time, the
control turned five red: the goldens' (`TestTheChimeIsTheSameOnEveryArchitecture`, the cache half of
`TestTheChimeStopsWithItsContext`, `TestNoCallerCanChangeTheChime`,
`TestGetOnAnEndedContextIsTheChimeKeptOrNothing`) and `TestTheChimesJoinsToSilenceAreClean`, as
predicted. The brief's premise that dropping the reset reddens only the goldens and the tails table
holds in part: the joins test failed on the end joins themselves too, at every rate (offsets up to
1.08 from `0x69`'s, −54.9 to −84.8 dB SA-CD, at 1.2 and 1.9 s), and its search found no tail at DSD64
within its 400 frames (396 and 21 at DSD128, 194 and 167 at DSD256), so regenerating the table
alone would not have turned it green. Before this, nothing named the edge.

### Item 6: `Get` answers a copy (R3, K1)

`Get` (`0b805450`) answers a `Chime` value: the cache keeps its pointer, an error answers the zero
`Chime`, asked before anything is dereferenced, and a build that made neither a chime nor an error,
which `build` never does, answers `errNoChime`; `Format` and `ReadAt` take a value receiver, so a
caller needs no `&c`. A chime kept is answered whatever the caller's context, as before.

- Red first (`7ea2c996`, the test alone, which compiles against both signatures):
  `TestEachCallerHasAChimeOfItsOwn` writes over `bitRate` and `planes` of what `Get` answered, puts
  them back in its cleanup and asks a later `Get`'s `Format`; over the cached pointer it failed at
  `chime_test.go:608`, "after a caller wrote over its chime, the next caller's is {BitRateHz:0
  Channels:2 Container: Frames:0}, want {BitRateHz:2822400 Channels:2 Container:
  Frames:6773760}", and every other test of the package passed beside it, the cache's chime put back.
- R3: `TestGetOnAnEndedContextIsTheChimeKeptOrNothing` asks the public `Get` on a cold cache (a fresh
  one stood in for the package's, the package's put back in the cleanup) with its context ended,
  then the chime kept with it ended; `TestGetAnswersNoChimeWhereItsBuildMadeNone` asks it over a build
  that answers neither.
- Controls (predicted in writing; mutations of `0b805450`'s `chime.go`, five tests each run alone):
  `Get` dereferencing before its error check panicked in the cold-cache case, the no-chime case and
  `TestTheChimeHasItsShapeAtEveryRate` (its refused rates), the others passing; the nil guard
  removed panicked in the no-chime case alone; `Get` asking its context before the cache failed the
  cold-cache case at its kept chime ("had context canceled and a chime hashing to e3b0c442…").
- `go test -count=1 -run '^$' -bench '^BenchmarkChimeDSD(64|128|256)$/^cached$' -benchmem -benchtime
  200000x`: 0 B/op and 0 allocs/op at every rate (10.7, 7.0 and 6.7 ns/op).

### Item 7: a cancelled build keeps nothing, nothing to change

A build runs on its first caller's context and publishes nothing once that context ends, and the
next caller builds again from nothing (`chime.go:27-28`; `TestACancelledBuildPublishesNothing`,
`TestACancellationBeforePublicationPublishesNothing`). Whether the backend's check (B12 PR 3) builds
the chime on a context of its own, so that a check stopped during its preparation costs the next one
no rebuild, is that PR's to decide; nothing outside the package's tests calls it, so nothing was
changed.

### Item 8: a build asks its context first; the plan's B12 row

`build` (`15c238af`) asks its context right after its rate is checked and before it allocates, so a
build on an ended context answers the context's error having allocated nothing. Red first
(`d0bdf239`, the tests alone): `TestTheChimeStopsWithItsContext` read "a build asked its context 62
times, once and then every 65,536 samples is 63", and 122 for 123 and 242 for 243;
`TestABuildOnAnEndedContextAllocatesNothing` (`testing.AllocsPerRun(10, …)` over `build(ended,
format.DSD64RateHz)`, asserting `context.Canceled` and no allocation) read "allocated 3 times, want
none"; the other sixteen tests passed. Both are green at `15c238af`. The plan's B12 row
(`d6775cf7`) credits `TestTheChimeHasItsShapeAtEveryRate` with the pads, which it checks, and the
own-rate test with no repeated bits alone.

### Items 9 to 11: the store case's cutoff (R1), its comment and its messages

R1 replaced the brief's correction of the record: the cutoff goes. `TestAStoreThatDoesNotAnswerIsNotReady`
failed once a round it passed over came after 20 s (`withinTheTest`), though the next round could
have been entered live. `49e7c5a2` removed it and gave the loop a give-up of its own at
`serveGiveUpTime`, its message saying that rounds that keep coming never let `waitForCount` give up;
a control showed otherwise, and `a1582b68` removed that check again (below), so `waitForCount`, which
gives up at the test binary's deadline less `serveWaitReserve`, bounds the retries, as R1 asked. A
round passed over "named no read entered live" (item 11: one whose answer came past its bound may
have waited on a live read), and the case's comment says what the handler's first `/v1/health` runs
in this fixture (item 10: the store's readiness probes and the roots' reachability probe, which the
round's end does not end; port 0 leaves it no endpoint walk), so a round unanswered for its whole
bound is such a `/v1/health` past a second, or a round left unscheduled as long, as the B972 entry
says (`:92-100`). The B972 entry (`ops/engineering-log/2026-11/2026-11-21-b972--fix-b972-test-races.md:115-122`)
is corrected in place.

The controls, the case alone (`-run '^TestAStoreThatDoesNotAnswerIsNotReady$'`), each predicted in
writing:
- every round's read entered with its context ended for 30 s from just before the boot: `d6775cf7`'s
  case failed at its cutoff at 20.47 s, 63 rounds passed over ("no round waited on a read entered
  live within 20s"); the new one, at `49e7c5a2` and again at `a1582b68`, passed at 30.4 s, 93 passed
  over, round 94 judged;
- no round's read ever entered live, `-timeout 90s`: the case failed at 60.05 s, the binary's
  deadline less 30 s, at `waitForCount`'s give-up ("the confirmation's round never came 187
  time(s)"), with the loop's own check (`49e7c5a2`) and without it, where a panic at 90 s had been
  predicted: the loop always waits in `waitForCount` for its next round, whose timer is the
  deadline, so its own check could fire only in the moment between a round's arrival and that
  deadline. `a1582b68` removed it, and the case's run without it (`a1582b68`) failed the same way;
- a round that ignores its store read's error (`serve_probation.go`'s `r.store` answer dropped): the
  old case and the new failed at round 1, "round 1 over a store that does not answer =
  {kind:1 ready:true why:}, want not ready".

### Part C: B976's collision claim

The entry said no catalog key, generic name, known sibling or corpus name lies inside `AD1` or holds
it; the declared sibling `AD1 Pro` holds it, which is why the row is strict-only and matches by its
exact port name alone. Corrected in place (`abc6fc9e`, the entry's `:111-116`): no known sibling
resolves to the row through either matcher, at the base and with the row
(`TestAnAddedRowAndEveryOtherRowKeepTheirOwnNames`, and the lint); the substring claim is left out.
That test, `TestEveryStrictOnlyDeclarationHoldsItsRow` and the lint's
`TestNoKnownSiblingReachesAModelRow` pass at `a1582b68`.

### The records (K4)

`.claude/rules/output.md`: no line changed; its line 125's bold sentences hold as they stand.
`ops/History-rules-output.md`: line 938 alone, the long form of that line, each change in the commit
of its item:
- item 6 (`0b805450`): "a waiter's own end ends only its wait, and `Chime.ReadAt` copies into the
  caller's buffer, so no caller changes the audio" became "a waiter's own end ends only its wait,
  and a chime kept is answered whatever the caller's context; `Get` answers each caller a copy of
  the chime kept (the zero `Chime` with an error, and an error for a build that made none, which
  `build` never does), so what a caller writes over its own leaves every other caller's as it was,
  and `Chime.ReadAt` copies into the caller's buffer, so no caller changes the audio another reads",
  and the list of tests gained `TestEachCallerHasAChimeOfItsOwn`,
  `TestGetOnAnEndedContextIsTheChimeKeptOrNothing` and `TestGetAnswersNoChimeWhereItsBuildMadeNone`;
- item 8 (`15c238af`): "The loop asks the context every 65,536 samples, and an integrator past 4"
  became "A build asks the context before it allocates anything, so one on an ended context
  allocates nothing, and the loop asks it every 65,536 samples; an integrator past 4", and the list
  gained `TestABuildOnAnEndedContextAllocatesNothing`;
- item 4 (`b09b0876`): "against a lower rate's so repeated, half its samples differ, as two streams
  modulated apart do" became "against a lower rate's so repeated, about half the samples of each
  channel's tone section differ, as two streams modulated apart do, counted over the sections
  alone, since the pads and each tone's other channel are `0x69` at every rate, which differs from
  itself repeated in half its bits";
- item 5 (`0a91bce2`): "held to 35, 65 and 95" gained "and each 20 ms edge, the converter's samples
  less the intended programme, leaves its largest in-band component under the same floor: 41.1 to
  106.6 dB under the tone, at least 6.1 dB inside it, where a fall whose rotation is not begun
  afresh reads 11.8 dB under the tone at every rate".

The B12 PR 1 entry (item 4), the B972 entry (item 9) and the B976 entry (Part C) are corrected in
place, as `ops/engineering-log/README.md` lets an entry be edited, each saying what it said before.

### The qualification version

`go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base
124eef530b16886bd988685a642ac38b0a59eabd -head HEAD` at `abc6fc9e`: "qualification version OK". None
of the nine paths the branch changes is among `GuardedPaths` or under `guardedPrefixes`
(`internal/player/qualification/version_gate.go:20`, `:36`), and `qualificationVersion` stays 56.

### Runs, before this record

- `internal/player/dsdchime`: `-race`, 20 runs of the package, a race test binary (`go test -c
  -race`) run as three lanes at once from the package's folder (`-test.count 7`, `7` and `6`,
  `-test.timeout 30m -test.v`): 360 passes, none failed, no race reported, 152 s.
- `go test -race -count=20 -v -run '^TestAStoreThatDoesNotAnswerIsNotReady$' -timeout 30m
  ./cmd/bridge/`, beside them: 20 of 20, 1.31 to 1.37 s a run, 27.9 s.
- `go vet` of both packages, the pinned gofmt (`go run cmd/gofmt -l`) on every changed Go file,
  each package's tests at each commit, and cmd/bridge's source guards
  (`TestNoDocblockNamesAnotherDeclaration`, `TestEveryCitedTestNameExists`, `TestNoBlankKeepers`,
  `TestEveryBackgroundGoroutineDrainsOnCleanup`): clean.
- A record holds no run made on its own commit: the vets for linux, darwin and windows, `make fmt
  vet docs-check`, the citation test with the release tags and `make fmt vet test build-all` on this
  record's commit are in its PR's body.
