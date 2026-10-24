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

- **Every release archive and Docker image carries freshly generated third-party licences from the union of the release builds; missing licences fail the release. Never replace build-derived discovery with the go.mod list or skip the before-hook. Require a licence at each effective module root; include compiled-package ancestry and the pinned Go toolchain notices. Unknown notice suffixes fail; never synthesize module grants. Omit unneeded Tailscale webclient and debugeventbus assets with the release tags in .goreleaser.yaml; keep all builds, deploys and CI tests/vet in step through the repository-wide reviewed-site census in check-build-tags. Discover with those same tags; refuse YAML null, boolean and numeric tag spellings. Refuse linked web packages and every non-main-module embed outside the exact module@version/package/file allowlist; exercise the real six-target graph in tests. The supplement registry is empty. Docker generates notices with the full go.mod patch toolchain and copies them into its final stage. Pin embedded-asset supplements to exact module versions and re-audit on upgrades or replacements. Discovery refuses vendor trees, a non-default GOFLAGS or workspace, and non-default architecture feature levels.** (B500) → History-rules-build-ci-tests.md

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
- **A running SQLite statement is parked from INSIDE it, by a Go collation** (#998) → History-rules-build-ci-tests.md
- **A test handler that holds a request until the client gives up must DRAIN a POST's body first** (#1003) → History-rules-build-ci-tests.md
- **A test whose request gets an answer with no body sends it on a transport of its own, never `http.DefaultTransport`** (2026-10-18, backlog B456) **Don't put such a client back on the default transport, and don't hunt the window by oversubscribing HTTP tests.** → History-rules-build-ci-tests.md
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
- **A field deliberately left unsynchronised binds TESTS too.** → History-rules-build-ci-tests.md
- **…and a test that starts the loop decides what the loop's own sends do** (2026-09-28) → History-rules-build-ci-tests.md
- **Putting back slog's previous default does not put back the `log` package, so a capture goes through `loggingtest.SetDefault`** (2026-09-28) → History-rules-build-ci-tests.md
- **A test helper that opens a store closes it in its own `t.Cleanup`.** → History-rules-build-ci-tests.md
- **A test that boots a server on a goroutine drains it in a `t.Cleanup`, never a `defer cancel()` plus a cancel-and-assert tail.** → History-rules-build-ci-tests.md
- **Two things the drain cannot fix by itself, both found converting the loop tests (#945).** → History-rules-build-ci-tests.md
- **A serve test waits for an EVENT until the test binary's deadline, never for a fixed time after a boot or a cancel** (2026-09-29, backlog B63) → History-rules-build-ci-tests.md
- **…and a watcher test waits for the watcher's walk, a watch and a row the same way** (2026-09-29, backlog B104) → History-rules-build-ci-tests.md
- **A serve test that needs a tool the live gate probes answers the probe, never with a process on PATH** (2026-09-29, backlog B105) → History-rules-build-ci-tests.md
- **The shared sox probe keeps its last answer through a probe that times out, and only its first snapshot waits for a probe** (2026-10-14, backlog B142) **Don't read a probe that could not answer as no sox, and don't probe on the request path once there is a verdict** → History-rules-build-ci-tests.md
- **Put a test seam on the instance it serves; where a package-level seam must remain, its restore runs after every goroutine that read it has FINISHED, and cleanups run last-registered-first** (2026-09-28) → History-rules-build-ci-tests.md
- **Windows CI catches wall-clock assumptions** — ~15.6 ms granularity means two stamps milliseconds apart are not reliably ordered: assert on counted events, and detect "was this rewritten?" by planted CONTENT, never by comparing mtimes. **An mtime compare is wrong in BOTH directions** (B52). → History-rules-build-ci-tests.md
- **A real timer a test fixture arms outlasts the test binary, unless the test is about that timer** (2026-10-23, the pairing fixtures, `pairingTestTTL`). **Don't shorten a shared fixture's timer to keep the suite fast: a test about expiry passes a short one of its own**. → History-rules-build-ci-tests.md
- **A port free on BOTH TCP and UDP cannot come from either allocator, so `drawLoopbackTCPAndUDPAddr` binds random numbers from 20000–32767 on both at once** (#1026) → History-rules-build-ci-tests.md
- **A serve test hands serve its listeners, never a closed port's number** (2026-10-09, backlog B313) → History-rules-build-ci-tests.md
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
