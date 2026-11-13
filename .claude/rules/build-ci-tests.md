---
paths:
  - "**/*_test.go"
  - "**/testdata/**"
  - ".github/**"
  - "Makefile"
  - "Dockerfile"
  - ".goreleaser.yaml"
  - "scripts/third-party-licenses/**"
  - "deploy/**"
  - "scripts/alsa-abi/check.sh"
  - "THIRD_PARTY_LICENSES.md"
  - "compose.yaml"
  - "internal/sweeptest/**"
  - "internal/proctest/**"
  - "internal/sqlitetest/**"
  - "internal/pooltest/**"
  - "internal/flactest/**"
  - "internal/dnstest/**"
  - "internal/standintest/**"
  - "internal/backup/backuptest/**"
  - "internal/logging/loggingtest/**"
---
# Build, CI and test discipline

> **Condensed 2026-10-18.** Each long entry here is one line — its bold title plus its own bolded "Don't…/Never…" sentences — ending `→ History-rules-build-ci-tests.md`: that is `ops/History-rules-build-ci-tests.md`, where the entry sits verbatim with its reasoning, measurements and rejected alternatives. **Still binding.** Edit the line here and its long form there in the same commit; new long-form detail goes to that file, never back into this one.

How a test here is written, run and trusted, and the CI, SonarCloud and Docker rules. It loads with every test file. The PR-time bot-review rules are in CLAUDE.md.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## Build, CI, and test discipline

- **Every release archive and Docker image carries freshly generated third-party licences from the union of the release builds; missing licences fail the release. Never replace build-derived discovery with the go.mod list or skip the before-hook. Require a licence at each effective module root; include compiled-package ancestry and the pinned Go toolchain notices. Unknown notice suffixes fail; never synthesize module grants. Omit unneeded Tailscale webclient and debugeventbus assets with the release tags in .goreleaser.yaml; keep all builds, deploys and CI tests/vet in step through the repository-wide reviewed-site census in check-build-tags, over every operational file and every non-record Markdown file the walk reaches (records by basename: engineering-log.md, History-*, plan-* and release-notes-*). Scan all fence labels except text, plain, plaintext, txt, output, log, diff, patch, json, jsonc, yaml, yml, toml, ini, xml, html, css, sql, go, golang, swift and mermaid; recognize blockquote/list containers and end fences when their containers end, reject backtick openers with backticks in their info, and strip console/session/terminal prompts before comments. The census is the union of build/install/run/test/vet followed after optional flags by a package pattern regardless of executable spelling, unless the verb is in command position (line start; after ;, &, |, ( or !, a VAR=value assignment, or if, then, else, elif, do, while, until, sudo, exec, command, env, time, nohup, nice), where it is the command itself (coreutils install, the shell's test); the executable rule (a case-insensitive Go path or variable before a verb even without a package; includes go.exe, go1.27.3 and gotip); and the tool list (gotestsum, staticcheck and golangci-lint wherever named, its GitHub action included; dlv debug/test/exec after any flags). Match with quotes, backticks, commas and brackets as separators only on the matching copy. Keep original logical lines and multiplicities; review ambiguous matches. Static analysis cannot see a verb computed at run time, a package argument and a verb on different unjoined lines, or a tool not on the list that drives go. Discover with those same tags; refuse YAML null, boolean and numeric tag spellings. Refuse linked web packages and every non-main-module embed outside the exact module@version/package/file allowlist; exercise the real six-target graph in tests. The supplement registry is empty. Docker generates notices with the full go.mod patch toolchain and copies them into its final stage. Pin embedded-asset supplements to exact module versions and re-audit on upgrades or replacements. Discovery refuses vendor trees, a non-default GOFLAGS or workspace, and non-default architecture feature levels.** (B500) → History-rules-build-ci-tests.md

- **SonarCloud's `go:S2077` follows a named const to its concatenation.** → History-rules-build-ci-tests.md
- **A test server that redirects to the REQUEST'S OWN path is a BLOCKER for SonarCloud's gate.** → History-rules-build-ci-tests.md
- **A new exec site names its binary by an absolute path, or by `exec.LookPath`'s absolute answer, never a bare name.** → History-rules-build-ci-tests.md
- **A `needs` entry only makes a job WAIT; something has to READ its result.** → History-rules-build-ci-tests.md
- **A doc comment must be attached to the declaration it names.** → History-rules-build-ci-tests.md
- **…and it reads consts and vars, against a verb list DERIVED from the tree** (#989) → History-rules-build-ci-tests.md
- **…and test files, against their own census and a floor of their own** (#990) → History-rules-build-ci-tests.md
- **…and a doc that opens with a name NOTHING declares** (#994) → History-rules-build-ci-tests.md
- **Measure a new detector over sampled HISTORY, not only the tree it was written against** (#994) → History-rules-build-ci-tests.md
- **An import keeper is dead code, never documentation, and `TestNoBlankKeepers` fails on one** (#996) → History-rules-build-ci-tests.md
- **A test that sweeps this repo's own files decides from the NAME what it opens, before it opens anything** (#993) → History-rules-build-ci-tests.md
- **Every wildcard element of a `//go:embed` glob starts with `[^.]`, never `*`** (#1006) → History-rules-build-ci-tests.md
- **A test that sweeps this repo's own files skips a directory below its root that holds a `.git` entry: another checkout, which is not this tree** (#1007) → History-rules-build-ci-tests.md
- **A sweep from the root also skips a directory below the root whose name begins with `_`, and decides it from the NAME before listing it** (#1008) → History-rules-build-ci-tests.md
- **A timeout is not a failure, and the difference is one flag.** A local
  `go test -race` without `-timeout` uses Go's 10-minute default, while
  the Makefile passes `30m` — `internal/admin` reported `FAIL … 600.758s`
  under CPU contention and passes in **826s** with the flag. The signature
  is a duration landing on exactly 600s with a goroutine dump and no
  assertion. Don't report one as a code failure.
- **A package-local test run cannot see a tree-wide guard.** Renaming a
  test in `internal/manifest` left a docblock citing the old name;
  `TestEveryCitedTestNameExists` lives in `cmd/bridge` and caught it on
  CI's macOS leg after a local `go test ./internal/manifest/` had passed.
  After a rename, run the package that holds the sweeps.
- **A test that fails only where a tool is missing is a finding about the product on such hosts, until the code says otherwise.** → History-rules-build-ci-tests.md
- **kill(pid, 0) finds a ZOMBIE, so it cannot tell a test whether a process it did not reap has exited: who reaps that one is up to init.** → History-rules-build-ci-tests.md
- **A process a test holds on a release file must end by itself once nobody can release it, so hold it with `proctest.HoldUntilReleased`, never a hand-rolled loop.** → History-rules-build-ci-tests.md
- **A test that starts a child with `SysProcAttr.Credential` changes its OWN dumpable flag on Linux** (#1032) → History-rules-build-ci-tests.md
- **A test child that holds a listener keeps it reachable past its wait, with `runtime.KeepAlive` after the wait** (#1036) → History-rules-build-ci-tests.md
- **Time an event where it HAPPENS, and match interleaved runs by an id.** → History-rules-build-ci-tests.md
- **A window between two statements is reproduced by PARKING a goroutine in it, not by stress.** → History-rules-build-ci-tests.md
- **A loop's `select` picks at random among the cases ready, so a test that makes a stale tick and a notification ready together gives every wait the loop arms a channel of its own, holds the loop between its arming and its select until both are ready, and is shown red with the fix reverted under `-count=20`, never by one run** (2026-11-09, backlog B761's round 2: the updater's `tickSeam`; the both-ready case went red in 10 of 20 runs before its fix, and 11 of 20 under its control). **Don't hand every wait one shared channel, which cannot fire an obsolete wait and not the current one, or take one green run of a both-ready case for proof.** `TestAWakeAndAnOldTickReadyTogetherAskNothing` holds to it.
- **…and a request whose wait can find its writer's reply and its own context's end ready together is tested one arm at a time: a one-shot `armPick` (`nativeSeams.arm`), set just before that request, forces its wait alone, each arm asserting the outcome it makes, and the select's own pick (`armAny`) asserting whichever it took; what an arm leaves running (the Locate a Resume answered through its context did not wait for) is awaited on the package's watchdog** (2026-11-13, backlog B887, and B886's second round). **Don't wait for an outcome one arm alone makes, set the pick before an earlier request's wait, wait on a sink's `Done` with no bound, or time what an arm leaves running with a timer a writer scheduled late outlasts.** → History-rules-build-ci-tests.md
- **A running SQLite statement is parked from INSIDE it, by a Go collation** (#998) → History-rules-build-ci-tests.md
- **…and the window between a transaction's last statement and its commit is held at a seam of the store's there, and the rollback a stop makes is waited for at the driver, until the transaction answers `sql.ErrTxDone`; a park inside the last statement reproduces the cancellation instead** (2026-11-06, backlog B662). **Don't stand a park inside the last statement in for a stop after it, release the hold before the rollback has come (the context's Done is not the rollback: a cancel closes it before it cancels the transaction's context database/sql derives from it, and a commit made in between lands, backlog B844), or take a rollback on the file for the held transaction's without asking the transaction.** `TestAStopAfterTheUpscaleTargetsLastStatementIsErrTxDoneAtItsCommit` and `TestARollbackWatchCountsTheRollbacksOnItsFileAlone` pin it. → History-rules-build-ci-tests.md
- **A test handler that holds a request until the client gives up must DRAIN a POST's body first** (#1003) → History-rules-build-ci-tests.md
- **…and answers nothing once the client gives up: it aborts the handler (`panic(http.ErrAbortHandler)`) rather than return** (2026-10-24, B526). **The deliberate exception is `TestSubmitHTTPAnswerAfterShutdown` (B538), whose TLS handler answers after the hang-up to pin the production HTTP-leg fix.** **Don't let other held test handlers return once their client has hung up.** → History-rules-build-ci-tests.md
- **…and a test that waits for a held request to be stopped gives every client on its path a timeout past the wait** (2026-11-09, backlog B761): a 30 s client timeout ended held checks inside a one-minute wait, and two negative controls stayed green. **Don't let a client's own timeout pass for the stop a test waits for.** `TestUpdateChecksTurnedOffAndOnAgainQuickly` and `TestServeStopsAnUpdateCheckUnderWayWhenChecksGoOff` hold to it.
- **A test whose request gets an answer with no body sends it on a transport of its own, never `http.DefaultTransport`** (2026-10-18, backlog B456) **Don't oversubscribe HTTP tests to hunt it** → History-rules-build-ci-tests.md
- **A test connection recorder that an assertion reads after the client returns must record under the lock before it closes the connection** (2026-10-20, backlog B461). **Don't Close first and append after: the client's handshake fails as soon as Close lands, so the assertion can run while the append is still pending.** → History-rules-build-ci-tests.md
- **Dependabot's github-actions ecosystem groups `github/codeql-action*` so the three CodeQL steps arrive in one PR** (2026-10-20, backlog B459). **Don't drop the `codeql-action` group: a single-step PR fails alone because `init` stamps its version into the config `autobuild`/`analyze` refuse.** → History-rules-build-ci-tests.md

- **A test that never touches the wiring proves nothing.** Three shapes, all of
  which shipped a dead feature with a green suite: a helper nothing calls, a
  type nothing constructs (a stub implementing the interface directly, while
  production passes a different concrete type), and a handler nothing dispatches
  to. Drive the real entry point — `Handler()`, the real Provider, the real
  endpoint — at least once per feature.
- **A docblock that names a test is a claim, and six of them were false.** → History-rules-build-ci-tests.md
- **A guard that scans SOURCE must parse, or strip comments, when the thing it
  forbids is named in the commentary beside it.** `player_audio.go`'s docblock
  says "Deliberately NOT `dlna.defaultMIMEForExtension`" — a text scan finds that
  and reports the rule as broken, which it did on the first run. Walk the AST, or
  strip; and anchor on an IDENTIFIER, never a string literal, because
  `stripGoComments` blanks literals too.
- **A fixture that omits a live gate describes a different bridge than production.** → History-rules-build-ci-tests.md
- **A stand-in for an older release shows only what it was written to do: what that release does with something the stand-in never reads (an environment variable, a file it does not open) is shown by that release built from its tag's tree (`git archive <sha>`, its own make tags and version stamp), and the record names the runs** (2026-11-09, backlog B761's round 3: the run-as-bridge copy's `predatesUpdateSwitch` reads the settings file and nothing of the environment, so the claim that 0.2.3 ignores `BRIDGE_UPDATE_CHECK_ENABLED` took 0.2.3 itself, its settings check run with the variable off and with a garbage value, beside a variable it binds as the control). **Don't take a stand-in's green for a claim about what it does not model.**
- **Commit BEFORE the negative control.** `git checkout --` and
  `git restore --source=HEAD` revert the whole file, so a control run against an
  uncommitted round silently takes the round with it. This file already recorded
  the lesson; I hit it twice in one session anyway. And a control that fails to
  BUILD reads as "control invalid", never as a pass — revert the test fixture
  alongside the production line so the control compiles.
- **...and once you HAVE committed, `git stash` is a no-op, which is the same trap inverted.** → History-rules-build-ci-tests.md
- **Negative-control every load-bearing assertion**, and check what the mutation
  actually did. B514's filesystem tests use explicit mount tables and stat
  answers; unavailable Linux/Windows runtime checks are reported separately
  from cross-compilation. A real symlink with fixture filesystem identities
  pins upper-target evidence and nested overlay/FUSE lookups through
  `mediumOn`, the function Linux's check calls; its platform reader is stubbed, so it shows
  that `mountIDOf`'s Go wrapper resolves nothing, not statx's no-follow flag. A POSIX mode-0 permission fixture skips Windows
  and root; its ENOENT sibling still runs. → History-rules-build-ci-tests.md.
  A control that fails to BUILD reads as "control invalid", never
  as a pass — and most "just disable this branch" edits delete a variable's only
  use. A control that mutates the *wrong occurrence* is worse, because it
  passes: identical blocks in two tests in one file, or a first-occurrence
  replace, will silently prove nothing.
- **A fixture must be a value the transformation would actually change**, or the
  test pins nothing (an "upstream metadata always wins" test used a name that
  cleans to itself and passed against code that overwrote unconditionally).
- **Pin cross-repo and cross-cycle contracts with CAPTURED bytes**, never a
  second hand-written copy of the offsets — two copies can be wrong together,
  and were.
- **Assert omitempty absence by unmarshalling and checking key-absence**, never
  a substring probe on the raw body.
- **`hidden === false` says nothing about an ancestor.** UI assertions from JS
  can and did lie; seed a throwaway bridge and drive the console in a browser
  before believing a green suite about UI. `document.visibilityState` is
  `"hidden"` in an automated tab, so `loading="lazy"` images never load and any
  perceived-performance claim measured that way is suspect.
- **A node harness that `withReachedFunctions` assembles carries a function
  called only inside another call's parentheses (`stageFiles(collectFromInput(…))`)
  only when the harness names it as an entry: the call scan consumes the
  character before a name, so it misses the inner call, and node answers the
  first event that reaches it with a ReferenceError, at the defect and at the
  fix alike** (2026-11-08, backlog B797). `requireCarried` checks an assembled
  script before node is looked for (a top-level binding of app.js its functions
  read that the harness does not declare, a name it calls that it neither
  declares nor has as a global), so a host without node catches it. **Don't
  take a node test that skips for one whose script would run.**
- **A field deliberately left unsynchronised binds TESTS too.** → History-rules-build-ci-tests.md
- **…and a test that starts the loop decides what the loop's own sends do** (2026-09-28) → History-rules-build-ci-tests.md
- **Putting back slog's previous default does not put back the `log` package, so a capture goes through `loggingtest.SetDefault`** (2026-09-28) → History-rules-build-ci-tests.md
- **A test that a value reached no log line captures the logger the component uses, at every level, searches every string a handler could write for each record (`loggingtest.Strings`, `Recorder.Carrying`), and asserts the line it expects is there** (2026-10-30, B604): a rendered line is not all a handler writes (a LogValuer resolves, a struct prints its fields, `errors.Join` its errors), a component handed a logger never reaches `slog.Default` (`NewRecorder`, `Logger`), and a check that only looks for the value passes once the line is gone. **Don't assert a value's absence without the line's presence.**
- **…and the recorder keeps what a derived logger binds, as slog's handlers write it** (2026-10-31, B604's third round; CodeRabbit on #1369): its `WithAttrs` and `WithGroup` returned the handler itself, so a value bound with `With`, a group's name and every `logging.Component`'s `component=` reached no record. One handler serves `Record`, `NewRecorder`'s `Logger` and `ParkOn`: each derivation adds an op to a chain of its own, `Handle` folds the chain over the record's attributes from the innermost op outward, a bound value is resolved when it is bound and the record's own when it is handled (a value of another kind, a map or a struct, is read when `Strings` reads it), and what slog's Handler contract ignores (an empty attribute, a group holding nothing, an unnamed `WithGroup`) is dropped, an inline group flattened. It passes `testing/slogtest` and matches the Text and JSON handlers key for key and value for value, but for the one shape Go 1.27.3's handlers write against that contract, a group of only empty attributes with something after it (`TestSlogsHandlersWriteAnIgnoredGroupAgainstTheContract`). **Don't give a test handler a `WithAttrs` or `WithGroup` that drops what it is given, share a chain between derivations, or hold the Recorder's lock across a park.** `TestTheRecorderKeepsWhatTheHandlersWrite`, `TestTheRecorderPassesSlogsOwnHandlerTests`, `TestDerivedLoggersShareNoStorage` and `TestParkingKeepsItsInvariantsThroughDerivedLoggers` pin it.
- **A test helper that opens a store closes it in its own `t.Cleanup`.** → History-rules-build-ci-tests.md
- **A test that boots a server on a goroutine drains it in a `t.Cleanup`, never a `defer cancel()` plus a cancel-and-assert tail.** → History-rules-build-ci-tests.md
- **Two things the drain cannot fix by itself, both found converting the loop tests (#945).** → History-rules-build-ci-tests.md
- **A serve test waits for an EVENT until the test binary's deadline, never for a fixed time after a boot or a cancel** (2026-09-29, backlog B63) → History-rules-build-ci-tests.md
- **…and a watcher test waits for the watcher's walk, a watch and a row the same way** (2026-09-29, backlog B104) → History-rules-build-ci-tests.md
- **…and checks first that the event can come where a seam stands in for the host's disk, leaving to that disk no answer the stand-in contradicts** (2026-11-04, B651 round 8): a watcher test said through `foldingHook` that a folder folds case and left its root's hold to the disk, which held the root only where it did not fold; on macOS no hold came, the wait ran 533 s of `internal/manifest`'s 600 s, and three kqueue tests that ran in the 30 s left failed, each green alone. **Don't call a test that failed after another waited out the deadline broken before it fails alone.**
- **A test of what a volume mounted over a folder does to the watcher mounts one, never renames a folder over it** (2026-11-05, B651 round 9): on macOS a disk image, `hdiutil attach -mountpoint`, which needs no privileges (`newDiskImage`). A rename drops the covered folder's last link, which kqueue reports on the descriptor fsnotify keeps there, so fsnotify forgets the path, its next Add opens a new descriptor, and the stale watch a mount leaves on the covered folder never shows: the rename fixtures passed while a resumed nested root saw nothing made directly in it. **Don't stand a rename in for a mount.** A linked root's link repointed, which no backend sees either, stands in for a mount on a host that cannot mount only where the question is that the folder at a root's path changed with nothing said (2026-11-07, B777's identity check); what a mount does to the watches themselves (kqueue's revoked folders, a remount's identity) is a disk image's.
- **A hook that the code under test runs on a goroutine of its own, a pool's worker, never ends the case there: it hands its step's failure to the test's goroutine, which fails the case with it once the work the hook ran in has returned; and every wait on that work, and every stop of it, is bounded by a budget of its own, capped before the binary's deadline** (2026-11-09, backlog B652's round 4). **Don't call `t.Fatal`, `t.FailNow`, `t.Skip` or `t.SkipNow` from a hook another goroutine runs, and don't wait on a pool's job, or stop its pool, without a bound or until the binary's deadline.** → History-rules-build-ci-tests.md
- **A case that renames or deletes a directory a root from `os.OpenRoot` holds open skips on Windows, by `runtime.GOOS` as its first statement, before any setup, naming why and the backlog id of the coverage it leaves: that root's handle there shares no delete, so Windows refuses that rename while the root is open; a `Root.OpenRoot` sub-root shares delete and needs no skip** (2026-11-09, backlog B652's round 4; round 5's `TestAnOpenRootHoldsItsDirectory`, in `internal/atomicwrite`, pins the refusal on Windows). **Don't decide the skip by the rename's error, after the case's setup, or on another goroutine than the test's.** → History-rules-build-ci-tests.md
- **A serve test that needs a tool the live gate probes answers the probe, never with a process on PATH** (2026-09-29, backlog B105) → History-rules-build-ci-tests.md
- **A test that runs a tool's real probe against a stand-in, a copy of the test binary, installs one copy for the whole test and starts it once first, under a bound of its own that it asserts, and a run that stopped at the probe fails by the probe's name before anything else is asserted** (2026-11-07, backlog B617's round 5): Windows scans a new executable's first start, past the 2 s `fpcalc -version` gets, and windows-latest failed three cases of `TestBridgeFingerprintFollowsItsSwitchWhileItRuns` that each wrote a copy of their own, as "the switch was never changed"; a stand-in built with `-race` also sleeps a second as it exits (`atexit_sleep_ms`), which `installFpcalcStandIn` turns off. **Don't write a copy per case, raise the production bound to make CI pass, or let a probe that failed read as the assertion after it.** `installWarmFpcalcStandIn` and `requireTheProbePassed` do it.
- **The shared sox probe keeps its last answer through a probe that times out, and only its first snapshot waits for a probe** (2026-10-14, backlog B142) **Don't read a probe that could not answer as no sox, and don't probe on the request path once there is a verdict** → History-rules-build-ci-tests.md
- **Put a test seam on the instance it serves; where a package-level seam must remain, its restore runs after every goroutine that read it has FINISHED, and cleanups run last-registered-first** (2026-09-28) → History-rules-build-ci-tests.md
- **Windows CI catches wall-clock assumptions** — ~15.6 ms granularity means two stamps milliseconds apart are not reliably ordered: assert on counted events, and detect "was this rewritten?" by planted CONTENT, never by comparing mtimes. **An mtime compare is wrong in BOTH directions** (B52). → History-rules-build-ci-tests.md
- **A test that builds on every platform asserts the words of an errno it fakes through that errno (`syscall.ENOENT.Error()`), or its identity with `errors.Is`, never Unix's words spelled out** (2026-10-31, backlog B603 round 3). Windows' `syscall.ENOENT` is `ERROR_FILE_NOT_FOUND` and its `syscall.ENOTDIR` is `ERROR_PATH_NOT_FOUND`, which the system words ("The system cannot find the file specified."), while the errnos Go invents there carry Unix's words: `TestMediumRefusesWhatVolumiosLayoutDoesNotShow` spelled out "no such file or directory" after its fake statfs's `syscall.ENOENT`, and every case failed on Windows with the refusal right. **Don't constrain such a test to Unix, or skip it by `runtime.GOOS`, to get past an errno's words.**
- **Every package's tests are vetted for linux/arm and linux/386 too, a portability check in CI's vet job (`GOOS=linux GOARCH=386 make vet`; the bridge ships no 32-bit build), so a test's constant past 2^31 takes the type of what it meets (the field's, `int64`, `uint32`), never the `int` an untyped constant defaults to** (2026-11-04, backlog B694: nine such constants kept four packages' tests from building where an int is 32 bits). A value that feeds an `int` on purpose, out of its range, stays an `int64`, converted only where an int holds it; where one cannot, the case skips saying why, or expects the refusal a 32-bit build gives it (yaml.v3's, at the decode). **Don't shrink the value to fit, or convert it where an int is 32 bits: the conversion cuts it, and the case then tests another value (4294967297 cut to 1 is init, alive).**
- **A real timer a test fixture arms outlasts the test binary, unless the test is about that timer** (2026-10-23, the pairing fixtures, `pairingTestTTL`). **Don't shorten a shared fixture's timer to keep the suite fast: a test about expiry passes a short one of its own**. → History-rules-build-ci-tests.md
- **A port free on BOTH TCP and UDP cannot come from either allocator, so `drawLoopbackTCPAndUDPAddr` binds random numbers from 20000–32767 on both at once** (#1026) → History-rules-build-ci-tests.md
- **A serve test hands serve its listeners, never a closed port's number** (2026-10-09, backlog B313); **a shutdown assertion observes the original listener (for example, Accept returning net.ErrClosed), never a dial of its released port, and checks failed-Start rollback before Stop can mask it** (2026-10-25, backlog B547) → History-rules-build-ci-tests.md
- **`filepath.ToSlash` is a no-op on POSIX**, so a Windows-shaped path handed to
  it on a Mac keeps its backslashes.
- **A test asserting that a message NAMES A PATH must not substring-match the raw path.** → History-rules-build-ci-tests.md
- **CI's cost is SQLite under the race detector, not the tests' shape.** → History-rules-build-ci-tests.md
- **…and its WALL CLOCK is one package's SEQUENTIAL runtime, which no number of cores touches.** → History-rules-build-ci-tests.md
- **A sharded suite's whole risk is a test that runs in NO shard** → History-rules-build-ci-tests.md
- **`set -e` does not fire inside an `if` condition, so `[ "$x" -lt N ]` on caller input is not a guard.** → History-rules-build-ci-tests.md
- **A guard that fails closed but prints NOTHING is half a guard.** `grep` exits
  1 on no match, and under `pipefail` inside a command substitution that killed
  `test-shard.sh` before the caller could report anything — on the single case
  it most needed to report, a sharded group with no matrix legs left. It exited
  1, correctly, with an empty log. `|| true` on that pipeline.
- **A test fixture that bulk-loads or bulk-deletes rows uses the BATCH APIs.**
  `UpsertTrackBatch` / `DeleteTracksBatch` exist because the one-at-a-time calls
  each take `Store.mu` and run their own BEGIN/COMMIT/fsync; a fixture calling
  the singular form pays that per row, under the detector. Seeding 4,000 rows:
  19.0s against 6.8s.
- **Size a SQLite-heavy fixture by build tag when the test has no concurrency.** → History-rules-build-ci-tests.md
- **Fuzz targets need `-fuzzminimizetime 1s`** — the 60s default burns CPU
  without incrementing `execs` while the run still says PASS, so the failure
  mode is a target that looks like it ran and did not.
- The `test (windows-latest)` leg was non-blocking until 2026-09-01 — a
  permanently-red non-blocking leg hides every genuine regression behind it.
- **`TestEveryCitedTestNameExists` scans `_test.go` COMMENTS too.** → History-rules-build-ci-tests.md
- **…and the TRACKED `.md` docs, with three exemptions and one hard constraint.** → History-rules-build-ci-tests.md
- **…and every name `go test` runs, which is more than `Test` and an uppercase letter.** → History-rules-build-ci-tests.md
- **The Dockerfile REQUIRES BuildKit, and its builder `FROM` says so through an invalid fallback:** **Never replace it with a real default.** → History-rules-build-ci-tests.md
- **gofmt rewrites two single quotes in a Go doc comment as a closing double
  quotation mark (`”`), inside backticks too**: SQL's empty string in a
  docblock (`''`) is mangled on the first `make fmt`. Write it as words, or
  put the SQL in an indented code block (a tab after `//`), which gofmt
  keeps verbatim. Only the doc comment of a top-level declaration is
  rewritten: a struct field's comment, a comment inside a function body and
  SQL inside a string literal are left alone (each measured with go1.27.2's
  gofmt, 2026-10-23).
- **A plan cannot show a loop that reads the same rows again, so count the
  pages a connection reads, against a twin store, never a duration**
  (2026-10-23, backlog B513). Every chunk of the sample-format fill planned
  as a range search on its partial index while every chunk read the header
  pass's rows again from the front, and the plan test stayed green. Set the
  store's pool to one connection (`SetMaxOpenConns(1)`) and read
  `sqlite.DBStatus` cache hits plus misses through `Conn.Raw` before and
  after (`pagesRead`); compare two stores that differ only in where the rows
  the loop must pass sort, so the count calibrates itself
  (`TestTheOpenFillVisitsEachUndecidedRowOnce`: 4,161 pages against 946
  before the fix, 1,020 against 968 after).

- **Developer test targets (`test`, `test-fast`) use a target-specific exported TMPDIR, defaulting to absolute `$(CURDIR)/.tmp` when the caller's environment is unset or empty; keep `.tmp/` in both ignore files.** A non-empty caller value wins. Keep Unix-socket fixtures on short paths or bind by basename from their directory: deep checkouts exceed `sun_path` (B544). → History-rules-build-ci-tests.md
- **…and the build-site census skips that `.tmp` at the root, by its name (`makeTestTmpDir`), never by the environment's TMPDIR, which a bare `go test` sets elsewhere; `TestTheCensusSkipsTheMakefilesTMPDIR` holds the name to the Makefile's TMPDIR line** (2026-11-07, backlog B740). **Don't rename the Makefile's default or the constant alone, skip a `.tmp` below the root, or skip the environment's TMPDIR.** → History-rules-build-ci-tests.md
- **A test that asks a live mDNS responder takes only an answer carrying the address it expects** (2026-11-07, backlog B744): on Linux a socket joined to the group on one interface also takes the group's datagrams arriving on another where a socket joined (`IP_MULTICAST_ALL`, which hashicorp/mdns leaves on), so under a parallel run another test's responder publishing the same name answers the question too, with its own address (internal/mdns's live test and cmd/bridge's serve row both publish `xn--f1ai0a.local`, one on loopback and one on a LAN interface; measured both ways with a probe). **Don't take the first answer naming the host.**
- **A host-address test must establish self-dialability, not just bindability, without switching to loopback or an unspecified address when the specific-IP branch is its subject.** B520 tries every non-loopback IPv4 candidate with a bounded bind/dial and source-IP check, listing failures when none works; skip locally, fail when `CI` is set. Preserve the logging negative control in the engineering log. → History-rules-build-ci-tests.md

- **Fixture export provenance is required only while exporting.** Keep extraction and serialization assertions live in source archives without Git or HEAD; gate publication metadata dependencies on `BRIDGE_EXPORT_IOS_FIXTURES`. → History-b470-native.md, “Final delta review”.

- **Backup fixtures resolve directories before deriving paths compared by hooks. The handoff seam `SetVerifyHandoffForTest` alone keeps the caller's `dir` as passed: expiry may precede resolution. Pass a resolved directory into the caller to compare one form throughout. Cleanup releases stalls, cancels and joins outer callers, then drains retained holds before restoring seams; a result or a free verification slot alone proves neither. Admission hooks use atomic pointers and receive their hold; each callback ignores other holds. Observe mutex contention through the failed-try seam, never goroutine stack text. Bound a parked step by its named step, without shortening the real SQLite check's deadline. A slow-step test polls for marks throughout a real interval inside that bound.** (2026-11-10, B653 PR 1 round 9). → History-rules-build-ci-tests.md

- **A held backup caller whose cleanup must release stalls before cancellation derives from `context.Background()`, never `t.Context()`, which testing cancels before cleanups; register `t.Cleanup(cancel)` before `joinBackupCallers`. Reporter expiry cases use `VerifyExpiryErrorForTest`, backed by production's constructor; the negative control adds a non-cancellation cause there, never merely reverts the extraction. Drive shutdown panic cases through the real check and `SetAfterCheckForTest`, asserting bare `context.Canceled` before publication and stderr in both branches. A prune's answer precedes its deferred hold release; drain that hold before expecting a second prune to run.** (2026-11-10, B653 PR 1 rounds 10 and 11). → History-rules-build-ci-tests.md

- **Backup tests register hold drains immediately after fixture creation, before ticker cleanup so the ticker joins first; a legacy verification drains before the next subtest takes its lock. Wait boundedly for the prune line on stdout before cancelling, never sleep after the verdict on stderr.** (B653 PR 1 round 12). → History-rules-build-ci-tests.md

- **A verification answers before its worker lets go of what it holds (`verifyAndThen`), so a test step that needs the backups folder's lock, this process's slot or the ownership free once a verification has answered (a prune that must run, a lock tried as another process) waits for that worker's end, which `SetOnVerifyEndForTest`'s hook hears last, set just before that verification begins (a worker runs the hook that stood as its verification began, so no earlier worker, which a drain of the holds may have passed before its last step, runs it), never for the answer; a test that relies on that wait holds the worker's unlock from the answer until it waits (`unlockHold`, over `SetOnLockReleaseForTest`), and asks the lock where the end is heard** (2026-11-11, backlog B849 and its second round: `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds`'s last prune, run on the answer of a verification made as another process, found the lock held on Windows' CI). **Don't take a verification's answer, or a drain of the holds, for its worker's end, or a prune's nil error for its run: a prune that meets the lock answers `Skipped` with no error and keeps every snapshot, so a test that reads what a prune kept asserts `!Skipped` (backlog B851).** → History-rules-build-ci-tests.md

- **A test that gives a verification's check up past a short deadline parks the check where every check comes, on its thread before it reads anything (`parkCheck`, over `SetOnLowPriorityForTest`), never at SQLite's open (`SetOnDatabaseOpenForTest`), which comes after the database's paced hash, which a deadline that passed first cuts short with no park; and it waits for the park and the answer in either order, bounded, failing at once on an answer that came with no check parked (`awaitParkedAndGivenUp`), never calling the verification on the test's goroutine. A case whose verification waits its turn behind a check given up holds the first's lock until the second waits, fails at once on an answer before that wait, holds the second at its wait (`SetOnSlotWaitForTest`'s hook, which `takeSlot` runs once it holds the notice of the next give-up and has found the slot's holder not given up) until the first has answered its check given up, its hold marked, and requires that the second heard the notice of that give-up (`SetOnSlotNoticeForTest`, on `takeSlot`'s notice branch) before it answered: a waiter the notice woke and one that found the holder given up as it looked, refused and never told, answer the same `ErrCheckStillRunning`, and the notice alone tells them apart. The hold is bounded, says its overrun to the test's goroutine, and is let go at the test's end by an abort `joinBackupCallers`' release closes before the callers are joined, the seams put back once they and their holds have drained** (2026-11-12 and 13, backlog B872 and its second and third rounds: `TestAnAbandonedCheckKeepsItsHoldsUntilItsCheckerEnds` waited for that park with no bound until macOS's 30-minute timeout, and five more cases parked there; `TestAVerificationWaitingItsTurnHearsOfACheckGivenUp` passed with its second verification refused as it arrived, the hook having run before `takeSlot` asked whether the holder was given up, and with that order put back it kept passing until its wait was held and the notice required, then failed 50 runs of 50). **Don't park a check meant to be given up behind anything that asks its context, wait for a park or an answer with no bound, take a refusal as the waiter arrived for a wait, tell a waiter told from one refused by its answer alone, or hold a hook with no bound or past the test's end.**

- **…and a test that waits for a step under the backups folder's lock to be given up gives it up at its gate once the step has reached the park the test waits for (`SetStepGateForTest`), never by a bound counted from its admission (`SetStepBoundForTest`), which a slow host can pass before the step reaches that park; and a check that must answer past a short deadline is parked by its own snapshot's path (`parkCheckOf`, over `SetOnCheckEntryForTest`), never as the first check that comes, until its grace has begun, then let go, its answer awaited, and only then its grace ended (`SetCheckGraceForTest`), every other step that deadline bounds held at its gate** (2026-11-13, backlog B886 and B890, and B886's second round). **Don't bound a step a test waits in by a duration from its admission, give a check meant to answer past its deadline a timer's grace or end its grace before its answer, park a check by anything but its snapshot's path, or fire a gate before its step has reached that park.** → History-rules-build-ci-tests.md

- **A test that a converted occurrence's join window waits asserts it undecided only at a look of the producer's, held at its wait, while the lookahead the sink measured is above the floor, and fails at once on a decision whenever it comes (`undecidedAtLook`, which waits for the look or the decision, whichever comes first: a producer that decided early has gone on to wait for what follows, which no reporter's step wakes, so the look never comes); then it lets the DAC play past the floor and waits for the decision: the reporter wakes the producer at each of its steps (its tick, a change of the sink, the wake a write left), and the producer reads the lookahead itself at each look, so a decision once the DAC has played past the floor is the backend's, whatever the test's tick** (2026-11-12, backlog B872 and its second round: on macOS's CI the producer decided between the DAC's play past the floor and the test's tick, and the case called that early; a window decided early then left the case waiting out the watchdog for its look). **Don't assert a decision absent where its input allows it already, or wait for the look alone.** `TestConvertedTracksJoinContinuouslyAtOneRate`'s floor cases, one of which holds the producer until its first look is past the floor, hold to it.
