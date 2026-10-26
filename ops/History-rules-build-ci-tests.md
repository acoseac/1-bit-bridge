# Long form of `.claude/rules/build-ci-tests.md`

> Moved verbatim from `.claude/rules/build-ci-tests.md` on 2026-10-18 so it stops loading with every matching file. **These entries are still binding.** The rules file keeps each one as a single line (its bold title plus its own "Don't…" sentences, ending `→ History-rules-build-ci-tests.md`); this file keeps the reasoning, measurements, rejected alternatives and the full don't-undo list. Change an entry here and its line in the rules file in the same commit. Each `##` below names the rules-file section the entries came from; entries inside a section keep their original order.

## Build, CI, and test discipline

- **SonarCloud's `go:S2077` follows a named const to its concatenation.** A
  query argument stays quiet only as ONE string literal, or as a function
  parameter; a const assembled with `+` is flagged even behind a name. `main`
  carries about 30 open S2077s of exactly that shape, and several comments in
  `internal/manifest` say a named const is enough: they are wrong. Measured on
  #1053 / #1054 (2026-09-27): a two-statement fix built from a shared SELECT
  was flagged, while one literal statement (`freshDSDPeaksSQL`) and helpers that
  take the statement as a parameter (`listRenditionCandidates`,
  `countRenditionCandidates`) were not. **Prefer one literal statement;** when
  a statement must share predicate constants, run it through such a helper.
  SonarCloud is not a required check, but a MEDIUM "vulnerability" on a
  constant query is noise that buries a real one.

- **A test server that redirects to the REQUEST'S OWN path is a BLOCKER for
  SonarCloud's gate.** `http.Redirect(w, r, r.URL.Path, …)` is
  `gosecurity:S5146`, an open redirect, and one such line takes a PR's
  Security Rating on New Code to E, which fails the quality gate (#1091's
  redirect-loop test server; the nine code smells beside it did not). Measured
  in the same file: a bare request path is flagged, while a path appended to a
  prefix (`target+r.URL.Path`, `"/moved"+r.URL.Path`) is not. A loop server
  redirects to a fixed path. It is test code and a false positive by
  construction, and a red gate on a PR still buries a real finding.

- **A new exec site names its binary by an absolute path, or by
  `exec.LookPath`'s absolute answer, never a bare name.** SonarCloud's
  `go:S4036` ("Make sure the PATH variable only contains fixed, unwriteable
  directories") flags `exec.Command("name", …)` as a VULNERABILITY, and one
  on new code takes Security Rating on New Code to B, which fails the gate:
  B201's `runLaunchctl` did (#1130), where `launchctlBin` (`/bin/launchctl`,
  on the sealed system volume) now names it, as `resolveBin` does for the
  audio tools. Moving an old bare call into a new function counts as new.

- **A `needs` entry only makes a job WAIT; something has to READ its
  result.** `gate`'s `needs` listed six jobs and its verification step
  checked five, so with `if: always()` a failing `dsd-measure` produced a
  GREEN gate — in the one job that exists BECAUSE those tests had silently
  never run in CI. The echo was honest (it named five), which is how
  nobody noticed. The verdict now iterates `toJSON(needs)`, because adding
  the sixth name would leave the seventh to be forgotten the same way;
  `skipped` and `cancelled` count as not-passing, and an EMPTY `needs` is
  refused — the `checked == 0` floor, applied to a gate. Verify by
  EXECUTING the extracted `run:` block against each case, not by reading
  it. **This does not cover a job absent from `needs` altogether**, which
  is not waited on and cannot be seen from inside. (#961)

- **A doc comment must be attached to the declaration it names.** Glued —
  no blank line — onto a different one, `go doc Y` prints X's prose and X
  has none. It happens when something is INSERTED between a comment and
  its subject: #840 and #953 each did it to a docblock carrying a live
  invariant, and `advertisedEndpoints`' orphaned paragraph had two
  cross-references still pointing at it. Sixteen across the tree, all
  pre-existing. `TestNoDocblockNamesAnotherDeclaration` flags only where
  the named identifier is DECLARED in the same package and has NO doc of
  its own — both conditions load-bearing, taking 467 raw candidates to 21
  to 16. It reads `ast.CommentGroup.Text()`, so `/* */` and a leading bare
  `//` are seen. **Re-run the control after refactoring a detector**: the
  `doc.Text()` change was verified to find the same sixteen, since that is
  exactly where a guard quietly stops guarding. (#964)

- **…and it reads consts and vars, against a verb list DERIVED from the
  tree** (#989). #964 inspected func and type docs only, against 31
  hand-picked verbs, and a 2026-09-24 census found 36 more: 18 glued onto a
  const or var (`handleBrowse`'s on `maxSOAPBodyBytes`, `VariantWatcher`'s
  43 lines on `stopGrace`), and 28 opening with a verb
  the list lacked ("GetTrack fetches", "Extract reads"). **#964's 5-of-5
  spot check measured PRECISION; nothing measured RECALL**, and the list
  recognised 61% of the openers correctly-attached docs use, which is the
  share of displaced blocks it could see. A closed list in a detector is an
  enumeration, and its reach has to be measured: a verb is now listed when
  five correctly-attached docs open with it or when it opened a real
  misattachment, and `docVerbCoverageFloor` fails below 85% (it reads 90%),
  naming the words to add. Don't trim the list for tidiness. A grouped
  `const ( … )` doc's subject is every member, but a member counts as
  documented only by its OWN spec doc or line comment, which is what lets a
  spec doc displaced inside a documented group be caught. Still unseen: a
  subject that has since grown a second doc of its own. (`_test.go` files
  headed this list until #990, and a doc opening with a name nothing
  declares until #994.)

- **…and test files, against their own census and a floor of their own**
  (#990). The 716 `_test.go` files held eleven more, and #989's list would
  have seen four of them. A test's doc opens "… pins …": 1,033 of their
  2,843 subject-first openers, against 2 of the 4,522 elsewhere, so
  `docVerbs` recognised only 48% of them. `testDocVerbs` is derived by
  #989's rule over the test files ALONE and read only there, beside
  `docVerbs`, so non-test files read exactly as #989 measured them. **A
  coverage floor over two populations is measured per population.** One
  85% floor over both passed with "pins" alone added (88.1% overall) while
  the test files sat at 84.5%: the larger population carried the smaller.
  Names resolve the way the compiler scopes them, keyed by directory,
  package NAME and test-ness. An external `foo_test` file sees only its own
  package, and an internal test file also sees its package's non-test names.
  Both directions were negative-controlled on synthetic shapes, since
  nothing collides across the tree's one external test package. A name an
  internal test file sees in both counts as documented if EITHER declaration
  is: answering from the test scope first let a test fake's undocumented
  method, recorded by bare name like every method, strip the production doc
  from under a test's prose about it (a Gemini consult found it; NC7
  reproduces it). Seeing the package's names has a measured price: 14 test
  docs open by naming the production declaration they test, with a listed
  verb ("loadCLIConfig is …"), and only the no-doc condition keeps them
  quiet. Delete one of those production docs and its test's prose is
  reported; restore the production doc rather than moving the test's. This
  guard's walk skips `.`- and `_`-prefixed files, as the go tool does: an
  emacs `.#name.go` lock is a dangling symlink, and one failed the guard
  with a parse error over a file no build reads. Still unseen: a title-line
  doc (`// Name.`, 29 in test files, none misattached).

- **…and a doc that opens with a name NOTHING declares** (#994). "Declared
  in the same package" left that half unread by design, and #989's census
  had seen three (`expectedTeamID`, `ensurePathExists`, `errITunesNoMatch`).
  Measured before building anything, the class was 25 wide: one rename the
  doc did not follow (`routesToForegroundLane`'s said
  `routesToOptimizeChannel`, which #863 retired) and 24 docs written under a
  name no commit on main ever declared, six of them import keepers
  documented as helpers that never existed. **What makes it precise is that
  an English sentence opens with a capital.** An opener that starts
  lowercase or has a camelCase hump (`identifierShaped`) can only be a name.
  A capitalised one-hump word or an all-caps one can be either, and every
  such opener a listed verb followed was prose, 10 of 10 ("It is",
  "Removal is", "DST is"). Followed by anything but a listed verb, 26 of 31
  identifier-shaped openers were prose (JS and CSS names, config keys,
  brands, "silence …" notes), so the arm reads the verb shape only. **The
  lookup is the DIRECTORY, not the file's scope and not the module**: an
  external `foo_test` file's prose names `foo`'s declarations
  (`LooksLikeSnapshotDir`), while a name only another package declares is
  still reported, because a doc opens with its own subject. Predeclared
  names (`nil`, `error`) and a documented function's own parameters count as
  declared (`namesNothingDeclared`). **A test function's doc is read only
  for a test-shaped name** (`testFuncName`, go test's rule). It states a
  premise, and a premise's subject is often a library or a writer: the first
  test file merged after the census (#991's M4A tests) opened four docs
  "dhowden does …", "iTunes writes …" and "QuickTime writes …", which
  failed #994's CI. Brand, tool and unit names ("iOS", "sox", "dBFS") are
  still identifier-shaped wherever else they open a doc; reword the
  sentence rather than exempt the word. **On a clean tree an arm counts
  zero, so the tree cannot show it still works**: with either arm's report
  deleted, the tree scan PASSED (CodeRabbit, round 2), which is "a helper
  nothing calls" and had been true of the misattachment arm since #964. The
  scan is `scanDocblockSubjects(r, root, wholeTree)`, and
  `TestDocblockScanReportsBothArmsOnAFixture` runs it over a synthetic tree
  with exact findings for both arms. Table tests pin `identifierShaped` and
  `namesNothingDeclared` beside it. (The import-keeper rule this bullet
  carried until #996 has its own bullet now, after the next one.)

- **Measure a new detector over sampled HISTORY, not only the tree it was
  written against** (#994). The undeclared-name arm read 20 of 20 on its
  census tree and then met four false positives in the first test file
  merged after it: one tree is one snapshot of the vocabulary. The detector,
  copied into a standalone program, ran over 18 trees sampled along main's
  first-parent history (`git archive <sha> | tar -x`, April to September,
  minutes). It found 29 distinct hits, and the history had already judged
  most of them: a hit a later commit fixed was real by that fix. That run
  showed which condition each false positive needed and what each would
  cost, one real finding. A census of today's tree says what a detector
  finds now; the history says what it would have found, and how often a
  shape it has never seen turns up. And merge main before pushing a
  tree-wide guard: its verdict depends on code the branch did not write,
  and here that code merged while the census ran.

- **An import keeper is dead code, never documentation, and
  `TestNoBlankKeepers` fails on one** (#996). A keeper is a blank reference
  that only names something: a top-level `var _ = pkg.X`, or `_ = pkg.X` /
  `var _ = pkg.X` in a function. Imports are per FILE, so it does nothing
  when the file uses `pkg` elsewhere and keeps an import nothing needs when
  it does not (one kept `io` in a test file because a helper in another
  file uses `io.EOF`). A top-level `var _ = logger` keeps a package-level
  name Go never reports unused anyway. Delete it, and the import too when
  nothing else in the file uses it. **Three hand sweeps each fixed only
  their own scope** (c062ac95 one, #855 three in cmd/bridge, #994 five);
  #825 added one between two of them, and #996 found fourteen more. Five of
  those were in the statement form no census had counted, and one,
  `var _ = (*manifest.Store)(nil)`, sat under "Statically assert the
  Manifest store has the helpers we need. A missing method here will fail
  the build". A nil conversion checks only that a type exists, and every
  real use makes that check. A keeper's premise decays unseen, too: two
  began as their import's only use and turned redundant as their files
  grew. **The sweep reads the four shapes keepers took, and nothing else**
  (`keeperName`): `N`, `N{}`, `&N{}` and `(*N)(nil)`, where N is an
  unshadowed import selector or, at the top level only, a bare identifier.
  All 24 keepers in 37 sampled history trees took one of them. A general
  "has no effect" classifier came first, and six review rounds each found
  a construct it misread: an operator that can panic, a compile-time
  assertion hidden in an array length, a call through a function pointer
  spelled like a conversion. Syntax cannot tell those apart without the
  type checker, so a keeper spelled any other way goes unseen, which is the
  safe direction; the rule above still applies to it. `(*N)(nil)` keeps one
  ambiguity (a call through a pointer-to-function variable with a nil
  argument), and no such exported variable exists in this module, the
  standard library or any dependency. The statement form reads imports
  only (`_ = cfg` marks a local used; the tree has 23), and a selector
  names an import only where no local of that name is in scope, scoped as
  Go scopes it, from the end of the declaring statement (a whole-function
  approximation missed `_ = path.Join` above `path := …`; CodeRabbit). The
  local form is not read in a file with a dot import, and cgo's `C` is
  never read as an import (its import carries the preamble). Two keepers of
  one package are not each other's use (update.go's pair).
  **The one allowance stands on its stated CONDITION, not its path**:
  `allowedKeepers` holds internal/tsnet's `var _ = errors.Is` while its doc
  says "Don't remove until typed errors land.", `typedErrorIn` finds no
  sentinel, `Error() string` method or error interface there, and the file
  uses `errors` nowhere else. Any of those failing is reported, and so is
  an entry whose keeper is gone. On a clean tree the sweep reports nothing, so
  `TestBlankKeeperScanOnFixtures` pins every shape, refused shape, scope
  rule, skip rule and allowance state: 49 of 51 mutations turn it red, and
  five turn the tree red. Of the two that stay green, one drops the CRLF
  normalisation (the scan is CRLF-safe without it) and one widens a check
  that `keep` makes again.

- **A test that sweeps this repo's own files decides from the NAME what it
  opens, before it opens anything** (#993). Emacs locks a file it is editing
  with `.#<name>` beside it. Where it can, the lock is a DANGLING symlink.
  Where it cannot, which is always on Windows (its `filelock.c`), it is a
  REGULAR file holding `user@host.pid:boot`. The symlink fails
  `os.ReadFile`, and the regular file fails `parser.ParseFile` or `node`, so
  a file-type check or tolerating ENOENT each misses one shape. A census
  planted both shapes for eleven extensions in every tracked directory and
  ran the suite. The symlink shape failed **19 tests in four packages** and
  the regular-file shape 6. All were sweeps that picked files by suffix
  alone, among them `TestEveryCitedTestNameExists` and eight tests of the
  console's static JS. `TestNoProductionCodeLowersTheHashCost` reported a
  lock as a production caller of `SetTestHashCost`. Each population takes
  its own rule. A Go sweep skips a name the go tool ignores, `.` or `_`
  (`goToolIgnores`): that file is never compiled, so a test defined in it
  never runs and must not satisfy a citation. `internal/admin`'s static
  sweeps skip `isEditorDetritus` (`.` or `~`) but NOT `_`, because
  `static/[^.]*` embeds a top-level `_x.js` and it ships. The citation guard
  asks git whether a doc is tracked BEFORE opening it; it used to discard an
  untracked doc only after reading it, so an unreadable gitignored doc
  failed the run. It also never opens a doc whose name begins with ".",
  which is what covers a tree with no `.git` (a fixture, or a source
  archive), where there is no tracked set. **No directory rule was
  widened**: the guard reads a tracked doc and a tracked Go file under
  `.github/`, and the go tool's `.`-directory rule would drop both. **Plant
  the census, not one probe.** A grep for `filepath.WalkDir` reached 3 of
  the 14 files; the rest used `os.ReadDir`, `filepath.Walk` or
  `filepath.Glob`, whose `*` matches a leading dot. A probe lock named
  `…_test.go` passed the hash-cost guard for the wrong reason, since that
  guard reads only non-test files. One failure is out of reach of test
  code: Go's fuzz seed-corpus reader fails on a lock inside
  `testdata/fuzz/<Name>/`. (This bullet listed a second until #1006: a
  lock at the top of an embedded directory breaking the BUILD. The
  patterns were the defect, and the next bullet is the rule.)

- **Every wildcard element of a `//go:embed` glob starts with `[^.]`,
  never `*`** (#1006). A
  glob's `*` matches a leading dot (`go doc embed`: "image/*" embeds
  "image/.tempfile"), so `static/*`, `templates/*.html` and `*.tmpl`
  matched an emacs lock beside the file it locks. As a DANGLING symlink
  (macOS, Linux) the lock broke the build of the package and of
  `./cmd/bridge`: `cannot embed irregular file static/.#app.js`. As the
  REGULAR file it is on Windows it was EMBEDDED, and the console served
  its `user@host.pid:boot` at `/static/.%23app.js` with a 200. A Finder
  `.DS_Store` at the top of `static/` shipped the same way.
  `static/[^.]*`, `templates/[^.]*.html` and `[^.]*.tmpl` embed the same
  files as before and refuse the leading dot and nothing else. Two
  tempting alternatives are wrong. The bare `static` is lock-safe too, but
  it drops a top-level `_name`, which ships and which the static sweeps
  above read for that reason. `all:static` embeds the dot files below the
  top level. `TestEveryEmbedPatternRefusesALeadingDot` plants a lock
  beside every file and a `.DS_Store` in every directory of every
  embedding package, through `go list -overlay`, and requires the embedded
  sets to be unchanged. A source file planted the same way must show up in
  the package's `GoFiles`, because an overlay the go command did not read
  leaves the two listings equal and the test green over nothing. It never
  writes the tree, since a real lock would
  break the build of the test itself. An overlay file is always regular,
  and that shape answers for both: the go command asks a file's type only
  after a glob has matched its name. **It lists with `-test`**: plain `go
  list` resolves a test file's embed patterns but neither their files nor
  their errors, and without the flag the probe passed over an unsafe
  test-file embed. **A backup (`app.js~`) is the one kind of detritus
  `isEditorDetritus` names that the embed cannot refuse inside a directory
  a pattern WALKS** (`static/[^.]*` walks `static/player`), because the go
  tool's walk skips only `.` and `_` names. A glob bound to an extension,
  `[^.]*.html`, never matches one. An auto-save, `#app.js#`, is embedded
  too, but neither side of the comparison skips it, so the two agree. So
  `embedDiskProblems` tolerates a backup on the embedded side. Its disk
  side skips a dot-directory, which the embed refuses at every level, and
  walks a `_` one, whose files are the 404 the comparison exists to
  report. An embedded file that is on disk but outside what the FS holds
  is reported as a pattern too wide, never as a stale cache. Until #1006
  `TestEmbeddedStaticTreeMatchesDisk` reported one as "embedded but
  missing from disk (stale build cache?)" while it sat on disk, every time
  emacs saved an asset. `TestEmbeddedTemplatesMatchDisk` and
  `TestEmbeddedUnitTemplatesMatchDisk` pin the other two FSes the same way.

- **A test that sweeps this repo's own files skips a directory below its
  root that holds a `.git` entry: another checkout, which is not this
  tree** (#1007). Claude Code keeps its worktrees of other branches under
  `.claude/worktrees/`, each a whole checkout that git does not track and
  CI never has. With the three the main checkout held, 1,208 of the 1,617
  files the hash-cost guard opened were theirs, and 3,366 of the 4,513 the
  flac-handle guard opened, so a half-written file or a call in progress in
  any of them failed this checkout's run. `sweeptest.IsOtherCheckout` is
  the ONE definition, in a package only tests import: `os.Lstat` of
  `<dir>/.git`, whether a directory, a `gitdir:` file or a symlink,
  dangling or not, and never the root. The five sweeps from the module root
  apply it (the citation, docblock and blank-keeper guards,
  `TestNoProductionCodeLowersTheHashCost` and
  `TestNoLeakyFlacConstructors`), and **a new sweep from the root applies
  it too**. It is not the go tool's rule and does not replace it. #995's
  go.mod rule sees another MODULE, and a checkout git is still writing has
  no go.mod yet (in index order `cmd/` comes before `go.mod`): there a
  nested test satisfied a stale citation, a false pass. The docblock
  guard's `.` rule sees `.claude/` and not a checkout at a plain path,
  which doubled the files it read and failed it. **It drops no tracked
  file**: listing every file each walk opened, before and after, showed
  that, and in a clone it holds by construction, since git will not add a
  file inside another repository. The shapes where git and `os.Lstat`
  disagree (an empty `.git`, a `gitdir:` file pointing nowhere, a `git
  init` inside a tracked directory) exist only in a local checkout. **A
  skip that can drop a whole subtree needs a floor that catches the
  largest one going missing.** The hash-cost floor was `visited == 0`,
  which a walk that lost `internal/` passed, and is `>= 100` now; the flac
  walk had none, and needs 100 files of each kind plus one that imports
  the package, because only importers are judged. **Demonstrate on the
  geometry, not on the report**: of the three failures this was filed
  with, only the work-in-progress one still reproduced on main, since #995
  had closed the false pass for a checkout with a go.mod and #993 the
  locks. A scratch clone holding the same three worktrees at the same
  commits measured all of it, and the main checkout was never written.
  Two of the five still read a `_` directory until #1008, the next bullet.

- **A sweep from the root also skips a directory below the root whose name
  begins with `_`, and decides it from the NAME before listing it**
  (#1008). The go tool ignores one at any depth, so no build compiles it
  and `go test ./...` runs no test in it, and `## Local test fixture`
  tells an operator to put a throwaway helper in exactly one. The
  hash-cost and citation walks read it, and on main a `_reextract/` beat
  them five ways: a helper in mid-edit ("could not parse"), a test there
  satisfying a stale citation (a FALSE PASS), a comment there naming a
  test nothing defines, a test file in mid-edit, and a mode-000 directory
  ("permission denied", both walks). **The last is why the rule answers
  before the listing**: `filepath.Walk` lists a directory before its
  callback can skip it, so the hash-cost walk, the one root sweep still on
  it, moved to `filepath.WalkDir`; and a per-file rule (descend, open no
  Go file below a `_` directory) handles four of the five, which is what
  the unlistable fixture rows pin. **Only the `_` rule is borrowed**: the
  go tool's `.` rule would drop the doc and the Go program git tracks
  under `.github/`. The root stays exempt, so a checkout cloned into `_x`
  is read. It is spelled inline, as the other three root sweeps spell it,
  not in `sweeptest`: a one-line name test below each walk's one root
  exemption. **It drops no tracked file, and here that is a census, not a
  construction**: unlike a file inside another checkout, git would track
  one below a `_` directory, and a doc there would go unread by the
  citation guard. None is tracked, and the files each walk opened, listed
  before and after over a clean tree and over the main checkout, are the
  same 410 and 1,172.

- **A test that fails only where a tool is missing is a finding about the
  product on such hosts, until the code says otherwise.** #1010 saw
  `TestInitRefusesToSaveAPortItNeverGraded` and
  `TestInitDoesNotExcuseAChangedPortWithItsOwnLivePID` fail in the stock
  `golang` image, on main too, and put it down to the container, which
  has no lsof. The tests were right: `bridge init` saved a port another
  process held on every host without lsof (#1021). Before calling a
  failure environmental, read what the missing tool changes in the code.
  A failure message that names a cause is a claim too: "…excused because
  our own recorded pid is alive" was false there, since that pass had
  cleared the pid file. **And a seam forced both ways by two tests pins
  the dependence it controls.** `TestPortCheck_BusyFailsWithoutOwnPID`
  forced `portProbeAvailable` true "so the verdict doesn't depend on
  whether lsof happens to be installed", and a sibling forced it false
  and pinned the warn: one set of facts, two verdicts, both asserted.

- **kill(pid, 0) finds a ZOMBIE, so it cannot tell a test whether a
  process it did not reap has exited: who reaps that one is up to init.**
  `TestServeLeavesNoTailscaleCLIRunning` and
  `TestCancelStopsTheWholeCLIProcessTree` kill a grandchild, the fake CLI
  behind a wrapper shell, and polled kill(pid, 0) for ESRCH. The orphan
  goes to the pid namespace's init, and systemd, launchd and the tini of
  `docker run --init` each reap it within milliseconds, so every host CI
  has passed. In a container run WITHOUT `--init`, PID 1 is the
  container's command, `go test` in the stock golang image, which collects
  only its own children: both tests failed there on dido, as root and as
  uid 1000, with the fake at `State: Z (zombie)`, `PPid: 1`. The previous
  bullet's question came out the other way here: serve had killed the
  CLI, and a zombie runs nothing and holds no files, so it writes nothing,
  which is all the tests guard. Both now ask `internal/proctest.Exited`:
  kill(pid, 0)'s ESRCH, and on Linux a process it still finds reads as
  exited when EVERY task in `/proc/<pid>/task` is Z or X and a second
  listing, taken after those reads, shows no task it did not read. **Not
  `/proc/<pid>/stat` alone**: a process whose leader thread exited while
  another runs reads Z there and in `status` (measured: `Threads: 2`, the
  other task S). Whatever /proc cannot answer reads as running, including
  a /proc numbered for another pid namespace, whose `self` is not this
  process (under `unshare --pid` without `--mount-proc`, pid 1 read
  `/proc/self` as 480456), so /proc can delay an exit and never invent
  one. **A control asked once, straight after `Start`, sees the child
  still being exec'd** (state R, 8 of 8) and passed a mutation that took
  every state but R for exited, so `TestARunningProcessHasNotExited` asks
  over 500 ms, as the callers poll, and on Linux requires /proc's S. Probe
  a child you DO reap by reaping it (`cmd.Wait`, then ESRCH), as doctor's
  `pidAlive` tests do. (#1024)

- **A process a test holds on a release file must end by itself once
  nobody can release it, so hold it with `proctest.HoldUntilReleased`,
  never a hand-rolled loop.** Both tailscale shutdown fakes looped
  `while [ ! -e release ]` on a file in a `t.TempDir`. When the fake
  survived, the defect those tests catch, the test's cleanup created the
  file and t.TempDir's own cleanup, registered earlier and so run next,
  removed the directory straight after. Polling every 20 ms, the fake
  almost never saw it and looped forever, reparented to init: under a
  mutation that let the CLI survive, 23 of 25 fakes stayed on macOS and 6
  of 8 on Linux. A file that exists for microseconds is a signal a poller
  misses; a directory that is gone stays gone. So the hold's wait also
  ends, with status 3 and the fake's work not run, once the release
  file's directory is gone or the test binary that built the script is.
  The second covers a run that dies before its cleanups: a
  `-test.timeout` panic or a SIGINT left a fake on main and none with the
  hold. Neither is true while the test runs, so neither weakens the tests:
  under the mutation both stay red on every host, and with
  `proctest.Exited` made to lie both fail on their "wrote after the
  cancel" check. Three traps met on the way. **Where the hold is what is
  under test, its failure path cannot lean on it**:
  `TestAHeldShellEndsWithItsTestBinary` released its shell and failed,
  and under the control that restored the old loop it left that shell
  looping, the same defect one level down; it now releases and waits.
  **A child that holds a shell must reap it**, or a hold that ends at
  once leaves a zombie that kill(pid, 0) finds on macOS, and the check
  that it holds passed that control. **A harness that means to kill a
  test binary with SIGINT must reset the signal first**: a background job
  of a non-interactive shell starts with SIGINT ignored and Go keeps it
  that way, so `kill -INT` did nothing, and what read as a leak on main
  was a test still running. (#1025)

- **A test that starts a child with `SysProcAttr.Credential` changes
  its OWN dumpable flag on Linux** (#1032). Go forks with
  `CLONE_VFORK|CLONE_VM` (unless a user namespace is asked for), so the
  child's setuid or setgid runs `commit_creds` on memory it still shares
  with the test process, and the kernel resets that memory's dumpable
  flag to `fs.suid_dumpable` (2 on Ubuntu). Root without CAP_SYS_PTRACE (a
  container's) then cannot read the test process's descriptors, and every
  later test that attributes a port to it fails: only as root, only where
  lsof is installed, and never alone (measured on dido: two #1028 tests
  after the first form of #1032's L7 kernel test). **Have the child drop
  to its uid itself, after exec** (`dropToChildUID`), and assert the test
  process's `PR_GET_DUMPABLE` did not move. No production code uses
  `Credential`; one that did would make the BRIDGE non-dumpable the same
  way, hiding it from every same-user owner probe.

- **A test child that holds a listener keeps it reachable past its wait,
  with `runtime.KeepAlive` after the wait** (#1036). net closes a listener
  nothing references from a finalizer, and `runUndumpable` held its
  listener only in a local nothing read after the port, so a collection
  while it waited would have freed the port a test records as the
  bridge's or the holder's (`runListeningChild` had the KeepAlive since
  #1034). **It was latent for a reason the timer hides**: the runtime
  forces a collection two minutes after the last one only once one has
  run, and on Linux the child's startup runs none at the default GOGC
  (macOS's does), so it took a bigger init or a lower GOGC to arm it.
  Both children now collect before they say they are ready
  (`collectBeforeReady`), so a child that drops its listener fails every
  test that uses it at once, on every platform. The rule is not about
  listeners: a file or a conn a child or a test keeps only in a local is
  closed by a collection the same way, and only something that still
  refers to it keeps it open (a `defer` or a `t.Cleanup` that names it, a
  KeepAlive after the wait).

- **Time an event where it HAPPENS, and match interleaved runs by an id.**
  Both errors were made measuring #997. A "serve has returned" marker printed
  from a `t.Cleanup` registered after `drainServeOnCleanup` runs BEFORE the
  drain (LIFO), so it marked the start of teardown. It put a figure into a
  commit message and a docblock that the right marker, printed where `run()`
  returns, then contradicted. And under `-count=N` a late line from one run
  prints during the next, so reading lines by position mixed runs up. Tag
  each line with something unique to its run (its data dir) and match on
  that.

- **A window between two statements is reproduced by PARKING a goroutine in
  it, not by stress.** The analysis pool's count-before-release window passed
  900 idle runs on the dev Mac, failed 9 in 720 under 3× CPU
  oversubscription, and failed on every run once the worker was held inside
  it. When a log line sits in the window, holding it there needs no
  production hook: `logging.Component` resolves `slog.Default` at log time,
  so a test handler that blocks on one message stops the worker right there
  (`loggingtest.ParkOn`, a test-only package both pools share; it was
  `parkOnLog` in `internal/analyze`). **When the window ENDS in a lock
  acquisition, hold that lock instead**: the worker stops exactly there
  whatever it did on the way, and something it stores lock-free just before
  (the transcode pool's worker slot) says when it has arrived. That reaches
  an exit with no log line in it, the transcode pool's success path (#988).
  **A window that ends in a PRINT to serve's own writer is parked by that
  writer**: runServe takes stdout and stderr as `io.Writer`s, so a test's
  writer that holds the first line containing a marker stops serve right
  there (`holdingWriter`, #1009: serve's exit reason, `tsnet close:`,
  `Shutting down`). With none of these, oversubscription is the fallback:
  build with `go test -c -race`, then run about three processes per core.
  Idle stress passing is not evidence the window is absent. (#987)

- **A running SQLite statement is parked from INSIDE it, by a Go collation**
  (#998). `VACUUM` copies an index with an append fast path that compares
  no keys, except an index with a non-BINARY collation, which it rebuilds by
  seeks (SQLite's `insert.c`, `xferOptimization`). `internal/sqlitetest`
  registers such a collation in Go (it lived in `internal/backup/backuptest`
  until the scanner needed it), so a VACUUM of a database `WriteSource`
  wrote calls back mid-copy with its destination file already created, and
  `ParkVacuum` holds it there. The same collation stops ANY statement that
  maintains an index a test created under it: an UPDATE that moves a key,
  an INSERT, a DELETE. The key is compared only against keys already in the
  index, so it needs a second row, and a partial index keeps the statements
  the test does not want to stop out of it. A cancel then reaches the statement only
  through modernc's own `interruptOnDone` goroutine, which has to be
  scheduled before the copy finishes, so `ReleaseUntil` lets comparisons go
  one at a time after a `runtime.Gosched`. Measured over 200 runs under
  `-race`: letting them all go at once let the copy finish before the
  cancel landed in 2 runs at `GOMAXPROCS=1`; one at a time without the
  yield, the cancel landed as late as the last comparison. Two traps met on
  the way. A held LOCK is not a park inside the statement: a VACUUM of a
  DELETE-mode source behind `BEGIN EXCLUSIVE` waits before the VACUUM
  starts. (That wait was `Ping`'s `select 1` under `busy_timeout(5000)`,
  and a cancel there came back as `database is locked` 5 s later, until
  B63 took the Ping out and gave the wait to `retryWhileBusy`, which
  answers it as `context.Canceled`: the snapshot bullet under The CLI and
  the serve wiring.) And modernc's `Driver.Open` reads its collation and
  hook lists without a lock, so register in `init`. **A Park's waits give
  up: `Arm`'s 10 s after each begins, for a statement the test itself
  started, and `ArmUntil`'s at the instant it is given.** A statement a
  serve runs is parked with `ArmUntil(t, serveGiveUpTime(t))`, waited for
  with `WaitUnless(t, exited)`, which reports serve's own exit rather than
  a timeout, and let go by a `Disarm` registered after the drain (B63: the
  seed test's `Arm` failed "no statement compared a key within 10s" on a
  Windows leg whose boot took longer, and its release loop's 10 s also
  bounded serve's whole teardown).

- **A test handler that holds a request until the client gives up must
  DRAIN a POST's body first** (#1003). net/http notices a client hanging up
  only once the request body is consumed, so a POST held unread sits out the
  handler's whole bound, and the test measures the timeout rather than the
  cancel. A cancel meant for a response's WRITE step is taken on the body's
  first `Read`, through a wrapping transport: one taken in the handler raced
  the client reading the headers and stopped the fetch instead in about half
  the runs, where the transport form stayed on the write in 50 of 50 under
  `-race` (#1001).

- **…and answers nothing once the client gives up: it aborts the handler
  (`panic(http.ErrAbortHandler)`) rather than return** (2026-10-24, B526).
  A handler that returns has net/http write an empty 200, after the
  client's close_notify made its request context end, and a client in the
  moment between its close_notify and its TCP close takes an answer that
  races that close (`persistConn.roundTrip`: "Use the response"): the
  atlas harvest's `TestATickStoppedByShutdownReportsNothingAndStopsThere`
  then decoded the empty body as EOF and reported `tick_error phase=submit
  error=EOF` for a tick the shutdown stopped (linux `test -race (rest)`).
  An aborted handler writes nothing and the server closes the connection,
  which the client reads as the cancellation it recorded before it hung
  up. The window is TLS's: `tls.Conn.Close` sends close_notify and then
  closes the socket, while over plain TCP the client's close is one step,
  and the server sees the hang-up only once the client can read nothing
  (the held handlers over `httptest.NewServer` in `internal/enrich` and
  `internal/updater` return after it and are not exposed; the rule holds
  for them all the same). **Reproduce it by holding that moment open, not by stress**:
  `racingHangUpClient` (internal/atlasharvest) sends close_notify when the
  request's context ends and takes whatever the server sends before the
  connection ends; against the returning handler it reported EOF twenty
  runs of twenty, against the aborting one never. The production rule did
  not move: `ctxerr.WithoutCancellation` quiets the cancellation alone, and
  quieting every error that came after the loop's context ended would hide
  a store write that failed as the shutdown began. **Don't let a held test
  handler return once its client has hung up.**

- **A test whose request gets an answer with no body sends it on a transport
  of its own, never `http.DefaultTransport`** (2026-10-18, backlog B456).
  Every `httptest.Server.Close` calls
  `http.DefaultTransport.CloseIdleConnections`. For an answer with no body (a
  200 with none, a bare status, a 204, a 304, any HEAD) net/http's read loop
  puts the connection in the idle pool BEFORE it hands the response over, so
  a Close another test makes in that window closes the request's connection,
  and the request fails with `net/http: HTTP/1.x transport connection broken:
  http: CloseIdleConnections called`. `TestFetchImage_RejectsEmptyBody` did,
  once, in #1260's `test -race (rest)`, beside the three parallel Deezer tests
  that close servers. An answer with a body is safe: its connection is pooled
  only after the caller has read it. Give the client `srv.Client().Transport`,
  a transport per server that another test's Close never touches. **Reproduce
  it by putting the Close in the window, not by stress**:
  `siblingCloseInTheIdleWindow` (internal/enrich) calls it from an
  `httptrace.ClientTrace.PutIdleConn` hook, which net/http runs on the read
  loop inside the window, and when that took the request's own connection it
  holds the loop until the request has returned. The old client failed 5 of
  5 that way, with CI's message; alone it passed 50 of 50, and its parallel
  group 300 of 300 under 24 CPU hogs. **Don't oversubscribe HTTP tests to hunt
  it**: 36 processes of that group exhausted the ephemeral ports (22,240
  sockets in TIME_WAIT, which other sessions on the Mac share), and every
  failure was `can't assign requested address` or httptest falling back to
  `[::1]`, none of them this one.

- **A test connection recorder that an assertion reads after the client
  returns must record under the lock before it closes the connection**
  (2026-10-20, backlog B461). `newConnRecorder`
  (`internal/atlasharvest/stored_base_test.go`) accepted, read the first
  bytes, `Close()`d, and only then appended to `seen`. The client's TLS
  handshake fails with EOF as soon as Close lands, so the tick can finish
  both legs and the subtest can call `connections()` before the second
  append: CI on Dependabot #1263 failed once with `the harvest client made
  1 connections, want one per due leg (submit, poll)`, and a re-run of the
  same commit passed. The same race can let the shape cases above that
  subtest pass when they should fail: they assert no connection reached
  the recorder, and a connection the client did make can still be
  unrecorded. A 50 ms sleep between Close and the append failed the
  subtest 20 of 20; recording under the lock before Close passed 200 of
  200 under `-race`. Grep of the repo's other test Accept loops found no
  second close-before-record: HTTP handlers (`authRecorder`, `genaSink`)
  record inside the handler before the response returns; `fakeMPD` records
  commands before its deferred Close; `handshakelog` waits on
  `StateClosed` after the line is written.

- **Dependabot's github-actions ecosystem groups `github/codeql-action*`
  so the three CodeQL steps arrive in one PR** (2026-10-20, backlog
  B459). `init` stamps its own version into a config file that
  `autobuild` and `analyze` refuse when the versions differ ("Loaded a
  configuration file for version '4.38.3', but running version
  '4.38.2'"). Without a group, Dependabot treats the three
  `uses:` lines as independent dependencies and opens one PR per step;
  the 4.38.3 batch (#1264 init, #1265 analyze, #1268 autobuild) each
  failed `analyze (go)` alone and was superseded by one combined PR
  (#1278). `.github/dependabot.yml` therefore has a `codeql-action`
  group under the github-actions ecosystem with
  `patterns: ["github/codeql-action*"]`; `update-types` is left unset so
  major, minor and patch all join the same PR (Dependabot's default for
  groups). The three SHAs in `codeql.yml` must still be equal — a hand
  edit can diverge them — and the workflow comment names the group
  instead of telling a maintainer to close singles by hand.

- **A docblock that names a test is a claim, and six of them were false.** Two
  stood in for an invariant nothing pinned at all; one was a false safety claim
  on a security boundary (`managed_controls.go` said a test "walks the registered
  routes so a new one cannot be added without a decision" — no test of that name
  had ever existed). `TestEveryCitedTestNameExists` sweeps the tree for cited
  `Test…` names with no definition. Write the guard or name the test that
  actually covers the invariant; do not leave prose asserting a check that is
  not there.

- **A fixture that omits a live gate describes a different bridge than
  production.** The gate belongs in the BASE fixture with a test overriding it
  both ways, and a fixture that wires a dependency the production path does not
  cannot see that gap at all — `upload.WithReclaimable` had two tests passing the
  option and zero production callers, so every 507 answered
  `reclaimableBytes: 0`. When only the wiring can be wrong, check the wiring.

- **...and once you HAVE committed, `git stash` is a no-op, which is the same
  trap inverted.** `git stash && go test && git stash pop` against a clean tree
  stashes nothing and runs the test against the FIXED build, so the control
  passes for the wrong reason and reports the opposite of the truth — the only
  tell is `pop`'s "No stash entries found", and it comes at the END. Check out
  the pre-fix commit instead (a throwaway `git worktree add <dir> <sha>`, removed
  after), and **assert you are on the old code** — grep the tree for the
  identifier the fix introduced and require zero — rather than assuming the
  stash took. Found on #941, where a "plain file works too" measurement was
  entirely an artifact of this. (Also: `grep -c … && next` exits 1 on zero
  matches and silently ends the chain.)

- **A field deliberately left unsynchronised binds TESTS too.**
  A `discovery.SendFailureLog`'s streak (each discovery client's `sendErrs`,
  the renderer's `sendErrStreak` until 2026-09-28) is only ever touched from
  its own tick loop, so a test calling its `Note` or `Reset` directly must do
  so with no loop live — before `Start`, or after `Stop` (which joins it).
  One that did neither raced under `-race` on CI and was not reproducible
  locally in 26 runs. Adding a mutex would pay production for a test's
  convenience.

- **…and a test that starts the loop decides what the loop's own sends do**
  (2026-09-28). `TestSendMSearchStreakResetsOnRestart` kept that ordering and
  still failed 10 of 200 runs on the dev Mac and 17 of 1,000 on Linux under
  `-race`: `Start` spawns the tick loop, whose first send lost a race with
  `Stop`'s close and took the streak to 1 before the failure the test drove,
  so that one logged nothing. Moving the capture before `Start` is not the
  fix: where a send goes through, the loop's SUCCESS resets the streak, and
  that version passed 5 of 5 on both hosts with `Start`'s reset deleted. The
  per-client `writeMSearch` seam makes the restarted loop's own first send
  fail on every host, and the test asserts that send's Warn; with the reset
  deleted it fails 20 of 20 on both. A test whose subject a live loop also
  moves cannot leave that loop's I/O to the host. The upstream client got
  the same seam and the same restart test
  (`TestUpstreamSendStreakResetsOnRestart`) when it gained the report.

- **Putting back slog's previous default does not put back the `log`
  package, so a capture goes through `loggingtest.SetDefault`** (2026-09-28).
  `slog.SetDefault` points the log package's output at the new handler and
  zeroes its flags, and `slog.SetDefault(prev)` with slog's own default (the
  one every test binary starts with) undoes neither, while that handler
  writes THROUGH the log package. So after the first capture in a binary,
  every later default-logger line went into the finished test's buffer: of
  200 runs of one discovery test, 1 printed its lines (200 after), and a
  failing test's diagnostics are what that swallows. It is also why the
  restart flake above showed no Warn. `SetDefault` puts back the default, the
  output and the flags, **the output and flags AFTER the default**: putting
  back a default whose handler is NOT slog's own points the log package at
  that handler again and zeroes its flags, so the other order ends on that
  handler, and only a test whose prior default is one it set can see it
  (`TestInstallersRestoreTheStandardLogger`, written in a parallel session
  and adopted here; swapped, its two such cases go red and every test over
  slog's own default stays green). `Record`, `ParkOn` and both capture
  helpers in `internal/dlna` use it (`handshaketest`, which redirects the
  log package itself, already put back its output, flags and prefix), and so
  does every capture outside the logging packages' own tests (the sweep
  below). **It refuses a parallel test, and a refusal changes nothing**: it
  calls `t.Setenv` before anything else, so a parallel test (or one with a
  parallel ancestor) panics there, and a later `t.Parallel` panics too. Two
  overlapping captures put back each other's state and leave the default on
  a finished test's handler, and `-race` cannot see it, since slog's default
  is an atomic pointer and the log package locks its output. Gemini on
  #1064 asked for a docblock warning; a rule stated only in prose (the
  `omitempty` time rule) was broken in ten fields before a guard went in, so
  this one is enforced. **And outside the logging packages' own tests, a
  test file may not swap the default by hand** (#1075): twelve files still
  put back only the default (29 references), and after each one's test a
  later test's `slog.Info` and `log.Print` both went into the finished
  test's handler, 0 of 2 lines reaching the output against 2 of 2 run alone
  (14 tests, go1.26.6); 2 of 2 once they went through `SetDefault`.
  `TestNoTestSetsTheDefaultLoggerByHand`
  (cmd/bridge) fails on a TEST file naming `slog.SetDefault` outside
  loggingtest's own tests (which build a prior default by hand, since the
  restore is their subject) or `logging.Init` outside internal/logging:
  through any import name, called or not (a `defer`, a method value), and on
  a dot import of either, whose calls carry no package name to read. `Init`
  counts because it is `slog.SetDefault` behind a once, with no restore; the
  metrics test called it. A test that wants a handler of its own installs it
  with `SetDefault`, never `Init`. **internal/logging's own tests call `Init`
  unqualified, which the scan cannot read**, so there `resetOnce(t)` puts
  back the default, the log package and Init's once, and the package's
  `TestMain` fails the run when a test left any of the four changed: a test
  that calls `Init` without `resetOnce(t)` passes itself and every other
  test, and fails only there.

- **A test helper that opens a store closes it in its own `t.Cleanup`.**
  `openTempStoreForBatch` opened a SQLite store under `t.TempDir()` and
  returned it; each caller was supposed to close it. A test that forgot
  left `bridge.db` open when TempDir's cleanup removed the directory.
  macOS deletes an open file and hides it; Windows fails the run with
  "The process cannot access the file because it is being used by another
  process" (B210, #1172, run 37882497814). The close is registered inside
  the helper, after `TempDir`, so it runs first (cleanups are
  last-registered first). `database/sql`'s `Close` returns nil when the
  DB is already closed, so a second close is harmless; the callers of
  these helpers only ignored that error, and those lines are gone. The
  same close went into `openTempStoreForPool`, `openTempStore` and
  `openBenchStore` (`b.Cleanup`). `TestAStoreHelperClosesBeforeItsTempDirGoes`
  registers its check before the helper and requires
  `sql: database is closed`. A helper that already closed, and one that
  `defer`s `Close` before it returns, were left. An `auth.Store` or
  `adminauth.Store` holds a JSON file it does not keep open and has no
  `Close`.

- **A test that boots a server on a goroutine drains it in a `t.Cleanup`, never
  a `defer cancel()` plus a cancel-and-assert tail.** The tail runs only when
  the body completes: a `t.Fatalf` above it Goexits, the deferred cancel fires,
  and the test returns without waiting. Measured on the failing path — the
  serve goroutine had NOT finished when the test binary exited, while
  `t.TempDir`'s cleanup (registered by the fixture earlier, so running later)
  removes the data dir from under a store still checkpointing, and that removal
  is then what gets reported instead of the assertion that failed. macOS hides
  it, because `RemoveAll` over an open file succeeds silently; it surfaces on
  the Windows leg, or as a leaked server racing the next test.
  `drainServeOnCleanup` is the ONE definition: wait on a channel the goroutine
  CLOSES, never `done` (a failure path may already have consumed the exit code
  — `waitForAdminReady` does), and report with `t.Errorf`, since `FailNow` from
  a cleanup skips the very removals the drain sequences itself against. Three
  such tests, written months apart, and the surviving shape had reached only
  the newest — so `TestEveryBackgroundGoroutineDrainsOnCleanup` pins the
  POPULATION, by AST: a text scan for the marker is satisfied by the comments
  that merely NAME a helper, and one for the old `defer cancel()` shape misses
  the sites that never had one. (#944; extended to the in-process loops, and
  `drainLoopOnCleanup` added beside it, in #945 — where a hand-written list of
  five files missed a sixth site that the shape match found.) **It reads
  every function in the test files, helpers included** (2026-09-28): it read
  Test functions alone, so a launch factored into a helper went unaudited,
  and two such helpers already existed. It wants the drain in the function
  that launches. **A serve test boots through a helper**: `bootServe`
  (main_test.go), which runs a command line, or `startConsoleBridge`
  (served_bridge_test.go), which writes the config, calls `runServe` with
  a `serveOpts` hook no flag carries, and builds the console and phone
  clients. Both start serve through `launchServe`, the one launch and the
  one drain, and with that drain deleted the Test-only guard stayed green.
  SonarCloud's gate fails a PR past 3% duplicated new lines and counts new
  lines that repeat OLD code, and an inline boot block was 16 to 23 lines
  of exactly that.

- **Two things the drain cannot fix by itself, both found converting the loop
  tests (#945).** A **`defer` beats EVERY `t.Cleanup`**, so a fixture that tears
  down with `defer store.Close()` can have no drain ordered behind it — the
  Close runs first, by construction, on the failing path. Measured on the
  smart-playlist test: pre-fix the regenerator was still running against a
  CLOSED SQLite handle (`store already closed when the loop returned = true`);
  as a `t.Cleanup` registered before the drain, false. And **a fixture that
  takes `*testing.T` must be built on the TEST goroutine** — `disabledIngester(t)`
  was called inside the `go func`, so `t.TempDir`, `t.Fatal` and `t.Cleanup` all
  ran off it; `FailNow` from a non-test goroutine is documented misuse (it
  Goexits that goroutine, not the test), and the `store.Close` registration
  landed at whatever moment the goroutine was scheduled, which can be AFTER the
  drain and so invert the very ordering the drain establishes. Neither is
  visible to an AST shape check, so the guard does not claim them.

- **A serve test waits for an EVENT until the test binary's deadline, never
  for a fixed time after a boot or a cancel** (2026-09-29, backlog B63).
  Serve's boot migrates and writes the store and its teardown checkpoints
  and closes it: disk writes with no bound a starved host keeps to. On
  nomos (Windows 11) with 24 writers syncing to its disk and 16 CPU hogs on
  the test's four CPUs, the store close took up to 16.4 s after the grace
  and a boot more than 30 s, and main's bounds failed 12 of 24 runs of the
  three tests CI had failed ("no statement compared a key within 10s" 4 of
  8, "serve never reached the tsnet start within 30s" and "runServe did not
  return" 6 of 8, "serve never reached the tailnet listen within 30s" 2 of
  8). With the waits below none of 24 failed, the seed test taking up to
  74 s.
  `serveGiveUp` (serve_wait_test.go) fires at the test's deadline less
  `serveWaitReserve` (30 s), and every wait on serve in a serve test uses
  it: the boot milestones (`waitForListening`, `waitForAdminReady`,
  `waitForServe`, the tailscale fakes' starts), the exits (`waitServeExit`,
  `waitBoundedServeExit`, `stopLiveServe`) and serve's work (the orphan
  sweep's refusal, the sox gate's sweep and scan). A wait that gives up
  prints serve's goroutines (`serveStacks`), and one on a serve that exits
  first reports its exit code. **The drains give up LATER**
  (`serveDrainGiveUp`, the deadline less 10 s): a drain after a wait that
  gave up still sees serve out once the cleanups after it let go, and with
  one give-up for both it reported a second failure about a serve on its
  way out. **So a hold the test takes on serve is released in a cleanup
  registered AFTER the drain**, or the drain waits on it to the deadline:
  a gate, a held print, a held mint, a sqlitetest Park (its own `Disarm`
  runs after the drain). **And nothing waits BEFORE the drain on what
  only the drain's cancel ends**: the wedge test's cleanup waited for its
  start there, on a start the drain's cancel releases, and every failed
  boot also reported "the released start did not return" (5 of 8). **A
  hang is still caught, at the deadline**: with the tsnet join unbounded,
  the wedge test failed at 60 s under `-timeout 90s`, its report naming
  `tsnetFront.stop`; debug a hang with a shorter `-timeout`. What a test
  pins about serve's pace stays on what serve does before its teardown's
  disk writes, from what it bounds: the drains-together test's order (a
  grace), a give-up's lower bound, and the tsnet join test's "no give-up
  line", asserted only when its start finished unwinding a second or more
  inside the grace (it measures when, and a 1 s start still fails the
  control). **Not converted** (backlog B107): the in-process tests' waits
  (the rescanner, the sweep passes, the album-gain render, sqlitetest's
  `Arm` in internal/backup and internal/manifest), loggingtest's 3 s
  `Park.Wait`, the tsnet front's unit tests, and clients' per-request
  timeouts. **A doctor test that calls the real lsof gives the probe the time
  the test has left** (`probeLimit`, `realProbeUsesTheTestDeadline`) and
  does not race the production 2s bound (backlog B338, 2026-10-12).
  Lengthening `probeTimeout` would hold `bridge doctor` on a wedged mount,
  and the timeout-contract tests leave the default so they still end at 2s.

- **…and a watcher test waits for the watcher's walk, a watch and a row
  the same way** (2026-09-29, backlog B104). The watcher tests slept 100
  or 150 ms for Run's initial walk and gave the row 3 s, and a file created
  before its directory is watched makes no event at all:
  `TestWatcherWatchesARootThatIsALinkToALink` failed that way on the macOS
  CI leg ("…never reached the manifest through the watcher"), and a drop
  with no sleep fails it 10 of 10. On a Linux host starved by a CPU hog
  (one CPU, cgroup weight 1 against 100) the walk ended up to 288 ms after
  Run started, the test failed 11 of 110 runs and the four other watcher
  tests with the same sleep 26 of 50; after, none of 100 and none of 50.
  `startWatcher` returns once `afterInitialWalkHookForTests` says every
  watch is registered (per instance, set before Run, as
  `afterDispatchHookForTests` is), and `watchWaitUntil` waits for a row or
  a folder's watch until the test binary's deadline less
  `watchWaitReserve`, or less half of what is left when that is less: a
  whole reserve under `-timeout 20s` put every give-up in the past, and
  each wait failed at once (CodeRabbit on #1115; B63's serve helpers keep
  the whole reserve). **A wait that can end on a failure event does**:
  the link-chain test stops on a subtree scan outside the root (#1090's
  defect, red in 0.09 s), and the dot-named and linked-root tests read the
  watch list once the walk is done. An absence has no event of its own, so
  `TestWatcherIgnoresDotfiles` drops a track after its dotfiles and waits
  for a scan that indexed the track to return
  (`afterDispatchHookForTests`): that scan listed the dotfiles too. Its
  dotfile was `.DS_Store`, which the scan's extension filter keeps out
  whatever the dot rule says; `._track.flac` (macOS's AppleDouble twin on
  exFAT or SMB) is kept out by the dot alone, and goes red without it.
  That test said until then that a dotfile's event triggers no scan;
  handleEvent filters by operation, not by name, so it always did.

- **A serve test that needs a tool the live gate probes answers the
  probe, never with a process on PATH** (2026-09-29, backlog B105).
  `TestServeProjectionFollowsTheLiveUpscaleGate` opened the upscale gate
  with a stand-in sox, a shell script first on PATH. `ProbeSox` gives
  `sox --help` 2 s, a timed-out probe reads as no sox, and the shared
  cache keeps that 30 s, so under sibling sessions' load the dev Mac
  failed step 1 ("/v1/health says upscaleEnabled=false with the flag at
  true…") in two sessions' gate runs. A stand-in slower than 2 s fails it
  every time, and on a Linux host starved by a CPU hog, under -race, the
  health request that ran the probe took up to 2.08 s. `serveOpts.soxProbe`
  stands in for the probe inside `soxToolchainCache`, the one sox probe
  serve makes (`withUsableSox`), which also puts the test's health check
  on Windows, where no shell script runs. With every `/bin/sh` exec
  delayed 2.2 s, main failed 10 of 10 and the branch none. **Serve's boot
  line reads that probe too**: it probed for itself (`soxFeatureReady`),
  the one consumer the stand-in did not reach, and a second fork at boot
  (`TestServeBootLineReadsTheSharedSoxProbe`). The product's 2 s is not
  the test's to raise.

- **The shared sox probe keeps its last answer through a probe that times
  out, and only its first snapshot waits for a probe** (2026-10-14,
  backlog B142). It ran `sox --help` under its lock on the request path
  whenever its 30 s TTL had run out, and cached a timed-out probe as no
  sox for the TTL: on a host whose sox is only slow to start (a swapping
  host, a PATH wrapper on a slow mount), the live upscale and analysis
  gates closed, `/v1/health` said `upscaleEnabled: false` and every
  manifest page lost its tracks' `variants` for 30 s, and the request that
  ran the probe waited up to 2 s with every concurrent one behind it. Now
  a snapshot after the TTL answers from the cache at once and starts ONE
  background probe (`soxToolchainCache.refresh`), which replaces the
  verdict when it answers; a timeout (`transcode.ErrSoxProbeTimeout`) keeps
  the last answered verdict and logs once per run of timeouts. **A sox
  that is not on PATH, or that runs and fails, is the verdict the moment
  its probe answers; with no verdict yet a timeout is one** (the gates stay
  closed until a probe answers). `ProbeSox` stops waiting for its output
  pipe `soxProbeWaitDelay` (1 s) after its deadline (`exec.Cmd.WaitDelay`):
  a PATH wrapper's child that outlives the killed wrapper held the pipe
  open, so the probe, and the background one with it, waited as long as
  that child lived. The probe stays the one serve makes
  (`soxProbeCallers` names `refresh`). **Don't read a probe that could not
  answer as no sox, and don't probe on the request path once there is a
  verdict** (`TestASoxProbeTimeoutKeepsTheLastAnswer`,
  `TestASnapshotAfterTheTTLDoesNotWaitForTheProbe`,
  `TestASoxThatLeftThePathClosesTheGatesWhenItsProbeAnswers`,
  `TestWithNoAnswerYetATimeoutIsTheVerdict`,
  `TestConcurrentFirstSnapshotsShareOneProbe`,
  `TestProbeSoxTimeoutDoesNotWaitForAChildHoldingItsOutput`).

- **Put a test seam on the instance it serves; where a package-level seam
  must remain, its restore runs after every goroutine that read it has
  FINISHED, and cleanups run last-registered-first** (2026-09-28). The
  first half is the per-SERVER rule under the 2026-09-09 LOUPE entries,
  met a second time. `statFunc`, the reachability probe's `os.Stat` seam,
  was a package variable, and `TestReachabilityProbe_InflightGuardIsPerRoot`
  restored it in a cleanup registered after `hangingStat`'s release, so
  the restore ran FIRST, while the probe goroutine that had read the seam
  was still parked in the stand-in; CI reported the race once and the rerun
  passed. **A released goroutine has not finished**: the release is the
  test's own close, which puts the test before the goroutine and not after
  it, so release-then-restore raced 5 runs in 5, as did release, a 50 ms
  sleep, then restore; only something the goroutine does AFTER its read,
  seen through synchronisation (its delete of its in-flight flag, under
  `c.mu`), orders the read. The first fix waited for exactly that, and
  CodeRabbit found the flaw a wait keeps: past its deadline it still had
  to restore, so it traded the race for a bound. The seam is now
  `reachabilityCache.stat`, set to `os.Stat` by `newReachabilityCache`
  and read by `probeLocked` under `c.mu` into a local the stat goroutine
  calls, so a test's stand-in reaches only its own cache and nothing is
  put back. **A race report is only as good as the detector's history**:
  it keeps four accesses per memory word, and a racing read was reported
  in 10 runs of 10 when two synchronised reads from other goroutines
  followed it, and in 0 of 10 when three did (a 30-line probe, go1.26.6).
  Here the healthy probe's read displaced the hung one's once the sibling
  test's accesses were in the word: 0 of 10 runs after the sibling and 0
  of 3 whole-package runs reported it, against 10 of 10 run alone.
  **Reproduce and negative-control such a race with the one test alone**
  (`-run '^Name$'`). The tree restores about fifty other package-level
  seams in one-line cleanups; they were not audited for this shape.

- **Windows CI catches wall-clock assumptions** — ~15.6 ms granularity means two
  stamps milliseconds apart are not reliably ordered. Assert on counted events,
  and detect "was this rewritten?" by planted CONTENT, never by comparing mtimes
  (two writes in one tick leave them equal, so the check silently passes on the
  platform most likely to break). Normalize CRLF before any `\n`-literal scan of
  a static file — there is no `.gitattributes` pinning `eol`. **A time decoded
  from JSON has no monotonic reading**, so `After` against a local
  `time.Now()` compares wall clocks, which is where the tick bites:
  `TestServeWithoutSoxReportsUpscalingOffOnEverySurface` waited for the Jobs
  card's `lastFinishedAt` to pass the instant of its nudge, a sweep the gate
  refuses finished inside the tick, and the wait ran out on the Windows leg
  (2026-09-28; 6 runs of 6 under a simulated 15.625 ms clock). A serve test
  counts through a `serveOpts` hook instead (`autoOptimizeSwept`). **An mtime
  compare is wrong in BOTH directions** (B52, 2026-09-28): "was it written?"
  FAILS correct code when both writes land in one tick, and "was it NOT
  rewritten?" passes with the guard removed. Five tests held the pattern with
  this rule in front of them (four in internal/auth, the tls reload test).
  They count writes (`inCommitWindow`), read the file back through a fresh
  store, and compare identity (`os.SameFile`): every write here stages a new
  file and renames it, so a rewritten path is another file, which bytes
  cannot tell for a rewrite of the same bytes. **A FAT disk image reproduces
  the class on any Mac**: `hdiutil create -fs MS-DOS` keeps 2 s mtimes, and
  `TMPDIR` on it puts every `t.TempDir()` there.

- **A real timer a test fixture arms outlasts the test binary, unless the
  test is about that timer** (2026-10-23, the pairing fixtures). A pairing
  store expires a request on a real `time.AfterFunc` armed at
  `CreateRequest`, which its injectable clock does not move, so a fixture's
  TTL takes part in every test that creates a request and races the test's
  own round trips: past the TTL, Approve and Decline refuse the request and
  a poll or a stream reads it as expired; past the grace, the row is gone.
  `pairingEventsTestSetup` had 500 ms and a 300 ms grace, and
  `TestPairingApprovalSharedBusRedactsSecrets`, which opens two streams and
  drains one before it approves, failed on a loaded windows-latest runner
  ("Approve: pairing: request not found", CI run 39930976048);
  `pairingTestSetup` defaulted to 100 ms with no test on it about the timer.
  With a second's delay after each create, 11 tests on the two fixtures
  failed on those values and none on `pairingTestTTL`, an hour, past the
  30-minute `-timeout` every test binary here runs under. The third time
  the class bit: `internal/admin`'s fixture went to 5 s on PR #104, and
  `TestPairingCreateQueueFull` to a minute on #674. **Don't shorten a shared
  fixture's timer to keep the suite fast: a test about expiry passes a
  short one of its own** (`pairingTestSetupTTL`, or a store of its own; the
  store's timer tests are in `internal/pairing`).
- **A port free on BOTH TCP and UDP cannot come from either allocator, so
  `drawLoopbackTCPAndUDPAddr` binds random numbers from 20000–32767 on both at
  once** (#1026). Windows hands ephemeral ports out IN SEQUENCE, TCP and UDP
  each from a cursor of its own (macOS does the same for TCP; Linux draws both
  at random), so asking an allocator again tests the NEXT number, not another
  one: the old helper's twenty TCP draws were twenty consecutive numbers on
  UDP. CI's Windows runners carry per-protocol WinNAT reservations: 200 UDP
  numbers the TCP cursor still hands out (starting anywhere from 49509 to
  58788) and 200 TCP numbers the UDP cursor still hands out (49698–49897), on
  six of seven VMs sampled. A draw started in front of either block fails
  twenty times with WSAEACCES, reproduced on three runners of three: the shape
  of the one CI failure, twenty refusals in 10 ms. **So "draw UDP first" moves
  the failure to the other block, more draws buy numbers the 200-long block
  still covers, and parsing `netsh … excludedportrange` misses a run of held
  sockets.** Random numbers are independent draws, and 20000–32767 lies below
  every target platform's ephemeral range (Linux 32768, Windows and macOS
  49152), so no allocator can hand the number out before serve binds it:
  30,000 calls on six runners rejected no draw.   A failure names every address
  tried and its error; the old message said only "20 draws", which is why
  finding this took a probe on the runner.

- **A serve test hands serve its listeners, never a closed port's number**
  (2026-10-09, backlog B313). `freeLoopbackPort` and
  `drawLoopbackTCPAndUDPAddr` bind `127.0.0.1:0` (or a drawn number), read
  the port and close. Between that close and serve's own bind another
  listener takes the number, and serve exits `address already in use`.
  Bridge #1166 CI (run 37865927261, `test -race (rest)`) lost
  `TestServeRedeemsThePairingLinksCode` that way on the admin port; the
  same close-then-reuse is what the HTTP/3 tests did with the TCP-and-UDP
  draw. A port serve, a served bridge or a probe will bind stays open
  (`holdLoopback`, `holdLoopbackTCPAndUDP`) and is handed in on
  `serveOpts` (`adminListener`, `lanListener`, `lanPacket`). Serve adopts
  one only when `Addr` is the address the config names (`--addr` for the
  LAN pair); a mismatch is a startup error and the listener stays with
  the caller. Nil is production, which listens itself. `admin.Server.Serve`
  still listens; `ServeListener` is the handed path. `freeLoopbackPort`
  remains for a port that is only written into a config and compared.
  A handed UDP socket is validated before the LAN TCP listen
  (`refuseHandedPacket`): HTTP/3 disabled with a socket handed in, or an
  address that is not the config's, returns 1 while nothing is bound. A
  return after that listen closes the listener this function bound.
  `waitForAdminReady` is GET /healthz, any response, and retries until
  one arrives or serve exits. A dial is not readiness: a handed admin
  listener already accepts before ServeListener. The API port stays a
  dial (`waitForPortAccepting`); it speaks TLS.
  (`TestAHandedListenerCannotBeTakenBeforeServeBinds`,
  `TestAHandedListenerOnAnotherAddressIsAStartupError`,
  `TestARefusedHandedUDPSocketLeavesTheLANPortFree`,
  `TestTheAdminReadinessCheckWaitsUntilServeAdoptsTheListener`.)

  **A shutdown assertion observes the original listener (for example,
  Accept returning net.ErrClosed), never a dial of its released port, and
  checks failed-Start rollback before Stop can mask it** (2026-10-25,
  backlog B547). The MediaServer's none-binds golden dialled its old port
  after Stop: a parallel test could already own it. Its fixture also used
  listen-then-close before Start. The DLNA fixtures now hand Start a live
  listener, and the golden retains that TCPListener and requires Accept to
  return net.ErrClosed before Stop. A wrapper signals after delegating Close
  to the real socket; its wait is bounded, without changing HTTP's Accept
  deadline and making Serve close the socket itself. Serve may still be
  entering on its goroutine when Start returns, and closes it as it observes
  Shutdown. Stop remains as cleanup.
  The production failure path's Shutdown, rather than Stop, owns rollback.
  Refusal-only dial fixtures can use port zero, which no listener can own;
  binding port zero always allocates a nonzero port.

- **A test asserting that a message NAMES A PATH must not substring-match the
  raw path.** `%q` escapes backslashes, so a Windows `C:\Users\…` renders as
  `C:\\Users\\…` and `strings.Contains(out, dir)` fails on that platform
  alone. Accept either rendering — the property is that the path is named, not
  the verb it is named with; pinning `%q` instead would go red on a benign
  change to `%s`. **And a quick follow-up push CANCELS the in-flight platform
  legs**, so a green tick on an older SHA is not evidence Windows ran: #941's
  code commit had its Windows and race legs cancelled by a docs-only push
  twenty minutes later, and the defect surfaced on the leg's first real run. (#941)

- **CI's cost is SQLite under the race detector, not the tests' shape.**
  `modernc.org/sqlite` is pure Go, so every page operation is Go code `-race`
  instruments, and the multiplier is ~48x, not the usual eight: deleting 2,000
  rows through `DeleteTracksBatch` measured **1.43s normally against 69s under
  `-race`**. `internal/manifest` was 1392s of a 25m18s race job; nothing else was
  close. **Before optimising a test, measure which PACKAGE the job is spending
  its time in** — a batch that fixed `internal/adminauth` (297s → 40s, real) was
  first written up as fixing the job, which it does not.

- **…and its WALL CLOCK is one package's SEQUENTIAL runtime, which no number
  of cores touches.** `-p $(nproc)` parallelises across PACKAGES; inside one,
  tests run in a single binary unless they call `t.Parallel()`, and that appears
  in **0 of `internal/manifest`'s 138 test files and 0 of `internal/admin`'s
  131**. Measured 2026-09-21: 895s wall over ~1,970 CPU-seconds — the scheduler
  was packing four cores well and the floor was `manifest` alone at 760s
  (`admin` 668s, `cmd/bridge` 218s, everything else under 140s). So the race job
  is SHARDED BY TEST NAME (`.github/scripts/test-shard.sh`, four legs each for
  those two plus one `rest` leg): **895s → 287s**, near-linear, with
  `cmd/bridge` at 218s the new floor — splitting `rest` further buys nothing.
  The gate's critical path is now `test (windows-latest)` at 353s, so measure
  again before optimising the race job any further. **A self-hosted runner is
  the WORSE answer here**: measured at ~1.85x (the dev Mac runs `manifest` in
  410s against the runner's 760s), it is less than sharding, GitHub's own
  guidance is that self-hosted runners do not belong on a PUBLIC repo (a fork PR
  runs arbitrary code on the machine), and standard hosted runners are free for
  public repos so the extra legs cost nothing billed.

- **A sharded suite's whole risk is a test that runs in NO shard** — every leg
  reports PASS, the gate is green, and nothing in the tree notices. Four guards,
  each of which caught something: the partition is verified as **SET EQUALITY**
  against the full list, never a count (a matching sum is also what you get when
  one name lands in two shards and another in none); `-run` is **ANCHORED**
  `^(A|B)$`, because it matches each `/`-part UNANCHORED and `internal/manifest`
  really has **23 name pairs where one is a prefix of the other** (unanchored
  `TestAnalysisCoverage` selects 2, anchored selects 1); discovery runs under
  **the same `-race`**, because `-race` defines the `race` build tag, so a
  listing taken WITHOUT it describes a different build — and a `//go:build race`
  test would then be in neither the listing nor any shard. Not hypothetical:
  this repo already carries `racefixture_race_test.go`. (It is also strictly
  less work — the non-race listing builds a second binary nothing uses.) And the
  `rest` leg asserts the WORKFLOW MATRIX schedules the exact index set
  `0..n-1` of every group it skips, because checking that the group is merely
  MENTIONED passes when one leg is deleted and 208 tests then run nowhere.
  **Fuzz targets belong in the partition** — without `-fuzz` they run their seed
  corpora as ordinary tests, so dropping `Fuzz*` retires every corpus from CI
  with no red X. The shard COUNT lives in the script
  (`SHARDED=(manifest:4 admin:4)`), never in the matrix: two places that must
  agree about a partition can disagree silently, since a leg passing a different
  count partitions the same names differently.

- **`set -e` does not fire inside an `if` condition, so `[ "$x" -lt N ]` on
  caller input is not a guard.** A non-numeric value makes both halves error,
  the condition evaluate FALSE, and execution carry on — `test-shard.sh manifest
  abc` printed two lines of raw `[: abc: integer expression expected` and then
  failed with "is empty (more shards than tests?)", a confident wrong diagnosis.
  Compare against the admitted STRING set instead, which also disposes of `007`
  (bash `test` reads a leading zero as OCTAL while awk reads decimal, so the
  bounds check and the partition disagree about which shard it is) and of a
  21-digit value (bash rejects it as not an integer at all, so a numeric guard
  could never bound it). Same family as the grep below.

- **Size a SQLite-heavy fixture by build tag when the test has no concurrency.**
  The compaction tests seed, delete and vacuum on one goroutine, so `-race` can
  find nothing in them — but the code paths must still RUN there, so shrink the
  fixture rather than skipping the test. The property that needs the full size is
  asserted in the `!race` build, which is exactly what the macOS and Windows legs
  run (`go test ./...`, whole suite, no `-race`, ~3 min). **Gate the
  "does this fixture still reproduce the hazard" assertion on the full size**, or
  it passes vacuously at the small one.

- **`TestEveryCitedTestNameExists` scans `_test.go` COMMENTS too.** It read
  only non-test source, so a docblock in a test file naming a renamed
  sibling was invisible — fifteen were, including a first sentence on the
  fsync contract claiming a property the test beneath it does not pin.
  Test files are parsed with go/parser and only their comment groups are
  scanned (a string literal can spell a test name and is not a citation).
  A historical note naming a removed test is REWORDED rather than exempted,
  and the guard scans its own file, so its examples cannot name one either.
  (#921)

- **…and the TRACKED `.md` docs, with three exemptions and one hard
  constraint.** It read Go only, which is how #945's rename left three stale
  citations in `CLAUDE.md` and the log — the auto-loaded file being the most
  expensive place for a stale guard name. Exempt: `ops/plan-*.md` (a plan
  names coverage it INTENDS — thirty of the forty stale names were in one,
  all correct as written), three metasyntactic `-run` placeholders, and one
  test owned by the private conductor repo. **TRACKED is load-bearing**:
  `ops/*audit*.md` and friends are gitignored and differ per machine, so the
  first draft failed on this one and would have passed on CI and a fresh
  clone — a verdict that depends on untracked local files is not a guard. The
  discriminator is `.git`: absent means a fixture tree with no ignore rules,
  present means git must answer and a failing git is reported, never quietly
  widened back. **Elide the `Test` prefix** (`…ServeDrainsOnCleanup`, or
  `…_FileHandler_…` for an underscore name) when a note must name a test that
  no longer exists: the pattern needs the prefix itself, so the name stays
  readable without claiming to exist. Four stale
  citations were corrected, not exempted — three plain renames and the guard
  name #945 retired — plus one FALSE POSITIVE, a UA string literal that was
  being discussed rather than cited. Keep the two apart: a stale citation is
  repointed or elided, a false positive means the prose should stop spelling a
  token it is only talking about. (#946)

- **…and every name `go test` runs, which is more than `Test` and an
  uppercase letter.** The pattern took only that shape, so no citation of the
  223 tests named `Test_…` (internal/dlna's convention) was ever checked, and
  five names of tests that did not exist were cited, one of them the
  engineering log's record of what guards a DLNA invariant. It is `go help
  testfunc`'s rule now: `Test`, then a letter of any script that is not
  lowercase, a digit, or underscores and a letter or digit. **Measure the
  population a pattern must cover, not the example that prompted it**: 32 of
  the 223 continue in lowercase after the underscore, so "`Test_` then
  uppercase" would have left them unguarded. **A `Test_` citation passes as a prefix only if it stops on
  a word boundary**, ending on an underscore or just before one
  (`…_CDS_Search_` names that family). The prefix rule had been "verifying" a
  fixture FOLDER name quoted in a comment against eighteen unrelated DLNA
  tests. camelCase has no delimiter to check and keeps its leniency. **The
  walk skips a directory below the root that has its own `go.mod`**: another
  module, which `go test ./...` never runs. `.claude/worktrees/` is the case
  that exists. Three leftover worktrees still held the stale citations, so
  the extended guard went red in the main checkout alone, and an old copy's
  tests can satisfy a citation this tree no longer backs, which passes. That
  was the one directory rule #995 added (#1007 added the `.git` rule, for a
  checkout with no go.mod); #993's reason for not borrowing the go tool's
  `.`-directory rule (`.github/`) still holds. (#995)

- **The Dockerfile REQUIRES BuildKit, and its builder `FROM` says so through
  an invalid fallback:**
  `--platform=${BUILDPLATFORM:-this-Dockerfile-requires-BuildKit--build-with-docker-buildx}`.
  BuildKit always sets `BUILDPLATFORM`, so the fallback is inert there
  (measured: amd64 and cross-compiled arm64 binaries byte-identical to the
  bare form's, `--check` clean). The legacy builder — what `docker build`
  falls back to when the buildx plugin is missing, which is the DEFAULT with
  Ubuntu's and Debian's `docker.io` — sets nothing, and the bare form died
  with a platform-regex dump naming neither BuildKit nor buildx. **Never
  replace it with a real default.** Under BuildKit a declared `ARG` default
  does not fill a gap, it REPLACES the automatic value, globally and in a
  stage (`ARG BUILDPLATFORM=linux/s390x` echoes `linux/s390x`; `ARG
  TARGETARCH=bogus` echoes `bogus`), so `ARG BUILDPLATFORM=linux/amd64` puts
  every arm64 host's Go compile under QEMU and a `TARGETARCH` default ships
  one arch's binary in every leg. `:-linux` did build a working image on the
  legacy builder (measured on amd64) and was refused anyway: a second path no
  CI runs, for a builder Docker has deprecated. #451 had already shipped one
  such path — `ARG TARGETOS=linux` "so a non-BuildKit `docker build` still
  builds", dead from the day it landed because the `FROM` failed first. `:?`
  would read better; `:-` is the form every Dockerfile lexer parses. `make
  docker` is `docker buildx build --load` behind a `check-buildx` guard.
  **`docker.yml` runs only on tags and dispatch**, so a Dockerfile PR gets no
  CI build at all: verify on a real daemon under BOTH builders
  (`DOCKER_BUILDKIT=0` forces the legacy one). (#983)

## B514 — deterministic medium evidence and controls (2026-10-23)

Mount tables, device identities and statfs sequences are explicit fixtures.
Repeated A,B,A and a third-read agreement exercise different outcomes:
inconclusive after the retry bound, and accepted at the last permitted read.
No new test reads the host's proc/sys. A Linux-only adapter assertion is
cross-compiled and vetted on macOS, not claimed to have executed there.
The mode-0 permission fixture skips Windows and root; the ENOENT case remains
runnable on both. Windows returns -1 from os.Geteuid and ignores Mkdir's
POSIX mode bits, so root's skip alone is insufficient. Round 2 audited every
round-1 test addition: only this fixture needed that guard; the engine's
new test already skips Windows because its probe refuses before the medium
check. Its negative control removes only the Windows guard (and unused
runtime import), predicting failures of the two permission assertions on
Windows; cross-compilation on macOS is not evidence of those assertions
running. The nested read-only-overlay and btrfs-device regressions run on
macOS with fixtures, failing before their fixes and passing afterward.
The requested uncommitted tree is preserved using exact file snapshots for
negative controls, never git restore or stash. Each patch applies alone to
the final tree and each predicted failing assertion is named in the scratch
manifest. Compile failures do not count as successful negative controls.
Round 3's TestMediumFollowsASymlinkedUpper uses a real symlink with fixture
mount IDs for ext4 and the target filesystem, and stubbed statfs/mountinfo.
It covers XFS evidence, nested overlay traversal and FUSE classification on
macOS without a real cross-filesystem mount or host proc/sys reads. The old
no-follow behavior fails all three subtests; the following upper reader
passes them. The test calls `mediumOn`, the function Linux's
`platformMedium` calls, so a change to the reader `mediumOn` runs with fails
it (an edit that made `platformMedium` call `mediumFrom` with another reader
would not: keep the one call).
Its check that `mountIDOf` still names the link's mount goes through the
stubbed platform reader, so it shows that the Go wrapper resolves nothing,
not that statx keeps its no-follow flag.
Windows skips only this test's privileged symlink creation.


## B500 — third-party licences in release archives (2026-10-24)

**Every release archive and Docker image carries freshly generated third-party licences from the union of the release builds; missing licences fail the release. Never replace build-derived discovery with the go.mod list or skip the before-hook. Require a licence at each effective module root; include compiled-package ancestry and the pinned Go toolchain notices. Unknown notice suffixes fail; never synthesize module grants. Omit unneeded Tailscale webclient and debugeventbus assets with the release tags in .goreleaser.yaml; keep all builds, deploys and CI tests/vet in step through the repository-wide reviewed-site census in check-build-tags, over every operational file and every non-record Markdown file the walk reaches (records by basename: engineering-log.md, History-*, plan-* and release-notes-*). Scan all fence labels except text, plain, plaintext, txt, output, log, diff, patch, json, jsonc, yaml, yml, toml, ini, xml, html, css, sql, go, golang, swift and mermaid; recognize blockquote/list containers and end fences when their containers end, reject backtick openers with backticks in their info, and strip console/session/terminal prompts before comments. The census is the union of build/install/run/test/vet followed after optional flags by a package pattern regardless of executable spelling, unless the verb is in command position (line start; after ;, &, |, ( or !, a VAR=value assignment, or if, then, else, elif, do, while, until, sudo, exec, command, env, time, nohup, nice), where it is the command itself (coreutils install, the shell's test); the executable rule (a case-insensitive Go path or variable before a verb even without a package; includes go.exe, go1.27.3 and gotip); and the tool list (gotestsum, staticcheck and golangci-lint wherever named, its GitHub action included; dlv debug/test/exec after any flags). Match with quotes, backticks, commas and brackets as separators only on the matching copy. Keep original logical lines and multiplicities; review ambiguous matches. Static analysis cannot see a verb computed at run time, a package argument and a verb on different unjoined lines, or a tool not on the list that drives go. Discover with those same tags; refuse YAML null, boolean and numeric tag spellings. Refuse linked web packages and every non-main-module embed outside the exact module@version/package/file allowlist; exercise the real six-target graph in tests. The supplement registry is empty. Docker generates notices with the full go.mod patch toolchain and copies them into its final stage. Pin embedded-asset supplements to exact module versions and re-audit on upgrades or replacements. Discovery refuses vendor trees, a non-default GOFLAGS or workspace, and non-default architecture feature levels.**

`go run ./scripts/third-party-licenses` reads the explicit matrix in
`.goreleaser.yaml`, then runs `go list -mod=readonly -deps -json -tags=<release-tags> ./cmd/bridge`
for each GOOS/GOARCH with CGO disabled, as the release build does. The Go
executable belongs to the running toolchain. Test-only and unused modules
are excluded; modules used on only one platform still enter the union.
The small stdlib configuration reader accepts the current builds stanza
only and refuses unfamiliar settings, exclusions, flags and YAML forms.
Tags must be a block list of unique literal Go tag names; templates, aliases,
flow/scalar forms, blank lists and duplicate keys or values refuse. YAML null
and boolean spellings (1.1 and 1.2, case-insensitively) refuse too; a tag
must start with a letter or underscore, excluding all numeric spellings. Extend
discovery and its tests together before changing that stanza.
Discovery refuses a vendor/ directory at the module root: GoReleaser would
automatically build its sources while -mod=readonly discovers the cache.
Discovery refuses a non-default GOFLAGS or workspace. It reads inherited
`go env GOFLAGS GOWORK GOAMD64 GOARM64`, including persisted `go env -w`
settings, and also refuses non-default architecture feature levels. It
preserves that checked environment for discovery so GoReleaser inherits
the same build settings. Empty values and the defaults (GOWORK=off,
GOAMD64=v1, GOARM64=v8.0) are accepted.

Every entry identifies the original module path/version and its replacement
path/version (without a trailing space for an unversioned local replacement),
but reads files from the replacement's directory. For every compiled package,
collect files from its directory and every ancestor through the module root;
deduplicate by module-relative path and label each file with that path. Never
walk unlinked subtrees. Include full bytes of LICENSE*, LICENCE*, COPYING*,
COPYRIGHT*, NOTICE*, PATENTS* and UNLICENSE* (case-insensitive). Require a
licence-class file at the effective module root; a copied package's nested
licence is additional coverage, never a grant for the rest of the module.
Accept extensionless names, text extensions (.txt, .md, .rst, .markdown,
.html, .htm) and the explicit licence-name suffix allow-list. This includes
.LESSER and .Apache-2.0: these identify licence variants, not source code.
Match the whole dotted suffix so Apache-2.0 is not mistaken for extension .0.
Skip known source extensions such as license_test.go; fail on every other
suffix, naming the file even when another valid licence is present. Extend
the allow-list deliberately. NOTICE or PATENTS alone is not a licence.
Empty files and read failures are errors too.

GoReleaser's builds.tags is authoritative. It sets `ts_omit_webclient` and
`ts_omit_debugeventbus`.
The Makefile GO_TAGS value, Dockerfile build flags, all three deploy scripts,
and every CI test/vet invocation mirror that list. check-build-tags uses the
same fail-closed reader. It scans operational files across the repository
(shell, PowerShell, Python, batch, workflow YAML, Makefiles, Dockerfiles and
extensionless scripts). Markdown scope is every .md file the walk reaches.
Records are excluded by basename wherever they live: engineering-log.md,
History-*, plan-* and release-notes-*.
Generated output trees (.git, bin, dist, vendor, node_modules, underscore-prefixed
directories) and nested checkouts are excluded, as before.

Markdown prose stays outside the census. All fences are scanned, including
unknown and unlabelled ones, except these explicit non-executable labels:
text, plain, plaintext, txt, output, log, diff, patch, json, jsonc, yaml, yml,
toml, ini, xml, html, css, sql, go, golang, swift, mermaid. These labels identify
data, output, markup or source examples rather than build commands; Python,
Dockerfile and Make are executable and deliberately absent from the skip list.
The first whitespace-delimited info word is the label, case-insensitively;
an unfamiliar attribute-bearing spelling is scanned. Before fence detection,
strip leading blockquotes and list markers (-, *, +, or 1–9 digits followed by
. or ) and whitespace), repeatedly for nested containers. A backtick opener
with a backtick in its info string is inline code, not a fence (CommonMark
0.31 section 4.5). Closers must have the opener's marker, at least its length,
and no info string. An unterminated fence ends with its container: a
blockquote fence ends on the first line without its quote prefix (including
a blank line); a list-item fence ends on the first non-blank line indented
less than the item's content. Nested containers end independently. Without
a container ending, an unterminated fence runs to EOF. In labels containing
console, session or terminal, strip a leading $, #, % or > prompt and keep the
command before removing full-line # comments: # there is a root prompt.

Join shell/Docker and PowerShell continuations. Match on a copy where quotes,
backticks, commas and brackets are separators. The census is the union of:

- The verb-with-package rule: build, install, run, test or vet followed, after
  optional flags, by a package pattern, whatever precedes the verb. Patterns
  are `.`, paths beginning `./` or `.\`, `all`, `std`, patterns ending `/...`,
  and this module's import path or a path below it. Flag values can span
  normalized tokens, including quoted paths and argv punctuation. This catches
  nested expansions, workflow templates and Python argv with a variable or
  shutil.which call for the executable without parsing those expressions. A
  verb in command position is the command itself and no site: at the start
  of a line, after ;, &, |, ( or !, after a VAR=value assignment, or after
  if, then, else, elif, do, while, until, sudo, exec, command, env, time,
  nohup or nice, it is the coreutils install or the shell's test
  (`sudo install -m 0755 ./bridge /usr/local/bin`, `if test -f ./deploy.env`),
  never an argument of a Go executable (round 8's review).
- The executable rule: a possible Go executable or variable expansion before
  a verb, with optional global flags, even without a package argument. It is
  case-insensitive and accepts go, go.exe, version wrappers such as go1.27.3,
  and gotip, paths and variable suffixes (go${EXE}, go$(EXE)). Its expansion
  grammar covers $X, ${X…}, $(X), $env:X and %X%; it does not parse arbitrary
  nested expressions. The verb-with-package branch covers those expressions
  when the verb and package are visible together.
- The tool list: gotestsum, staticcheck and golangci-lint wherever named
  (golangci/golangci-lint-action, which runs golangci-lint with its govet
  analyzers, included), and dlv with debug, test or exec after any flags
  (dlv --headless debug), including paths and .exe spellings,
  case-insensitively.

Keep the original logical line (whitespace collapsed) for the explicit
reviewedBuildSites comparison, including multiplicities. Ambiguity is a site
requiring review: $(MAKE) vet/test call tagged Make targets; a firewall
command's description, two Docker builds and migration text are reviewed
non-Go matches. New files and additional recognized commands fail until
reviewed. The same union applies to every operational file and every
non-record Markdown file described above.

The static census is not a shell, Python, PowerShell or Make interpreter.
It cannot see a verb computed at run time, a package argument and a verb on
different unjoined lines (unless the executable rule independently finds the
verb), or a tool not on the list that drives go. Commands sourced from
external/generated files outside the scope also need manual review. Extend
discovery with regression cases before adopting a new build form. Go source,
Markdown prose and unfenced Markdown (including indented code blocks) remain
outside this operational census. The untagged Go exceptions remain the notice
generator, temp-directory helper and two printed test instructions.
TestRealTreeBuildSiteCensus prints every in-scope Markdown file, even one with
zero sites, and every discovered site, then requires exact agreement with the
reviewed census. Round 8 has 42 sites and 51 Markdown files (six with sites),
adding three reviewed non-Go sites to round 7's 39; no newly discovered bridge
command needed tags.

The shard's RACE_FLAGS is separately pinned so it cannot override the explicit
tags. Make's override assignment
prevents command-line or environment drift. make build and build-all run the
check, as does check-go-version, already part of gate.yml. That gate was chosen
to keep docs-check Python-only and to reuse the release-config gate; that job
now installs the pinned Go toolchain for the shared reader.
Make's test, test-fast, vet and run also use GO_TAGS. The notice generator passes
the parsed tags to every target's go list, including fixture builds.

The bridge uses neither Tailscale's web UI nor its event-bus debugger HTTP
surface. The prebuilt UI strips notices from several bundled npm components;
an incomplete offline reconstruction is not a release notice. Omit these
features instead. The six tagged dependency graphs include neither
web-client-prebuilt nor client/web, and no debugger HTMX/CSS/templates.
The two former supplements were removed; tailscale.com itself stays linked
and retains its module notices. Protobuf edition defaults and Tailscale's
first-party metadata, DNS data and resolvconf workaround are the only reviewed
third-party embeds: seven files, keyed by module@version and import path in
reviewedEmbeds. Every target's go list decodes ImportPath and EmbedFiles.
An unknown package, changed module version, any replacement, or a changed
nonempty file set fails before notices are emitted. Main-module and standard
library embeds are outside this dependency census. client/web and the
web-client-prebuilt module/packages are explicitly forbidden even without
embeds. TestRealReleaseGraphEmbeds runs discovery on the real six-target graph,
without an opt-in or fixture substitute.

The registry in main.go remains, currently empty, exercised with fixture
components. It pins each supplement to an exact module path and version;
upgrades, local replacements and changed replacement identities/versions
require review when entries exist. Today there are no real supplements.
The separate reviewedEmbeds allowlist is the fail-closed gate for new assets;
a future embedded third-party asset must either be omitted or receive a full
notice review, an explicit embed allowlist entry and any needed supplement. Audit go-list EmbedFiles across all six targets after
upgrades, including compressed JS/CSS and fonts; a module-root licence does
not automatically cover third-party code bundled as assets.

Reproduce only the PATENTS a module or its replacement actually ships; never
supply a missing grant from another source. In particular, x/oauth2 has no
PATENTS. Emit one separate "Go standard library and runtime (go1.x.y)" section
with the release toolchain's GOROOT/LICENSE and GOROOT/PATENTS when present.
A missing or empty Go LICENSE fails generation. The section covers the
standard library, runtime and toolchain-vendored code; the module counts
exclude it. Require the running toolchain to match go.mod's go directive,
which release CI's setup-go uses, and label the section with that version.
Module and filename order is sorted, without dates or cache paths.

The before-hook runs after `go mod tidy`, writes the ignored root file
`THIRD_PARTY_LICENSES.md`, and every archive includes it. Weekly dependency
bumps therefore refresh module notices automatically on the next release.
Modules with reviewed embeds instead fail on a version bump until the new
embedded files and their coverage are reviewed and reviewedEmbeds is updated.
The empty supplement registry alone would not detect a newly linked bundle. The generated aggregate stays
untracked; any future supplementary source notices must be checked in. Do not seed dist/
from the hook: GoReleaser requires it empty at that stage. The release
playbook verifies every snapshot archive's notice bytes before removing dist/.

Tests build a local fixture graph through the actual go-list entry point,
including target-specific imports, unused and test-only requirements,
replacements, missing licences, mixed-case filenames, multiple licence
files, nested ancestry and deduplication, COPYRIGHT, LICENCE, rejected source
files, module-owned PATENTS, UNLICENSE, Go toolchain notices and deterministic
ordering. Environment tests cover inherited flags, explicit and automatically
found workspaces, persisted settings and architecture levels. The wiring test
pins the before-hook and the archive file entry. Round-3 fixtures also pin
nested-only root refusal (including replacements), COPYING.LESSER beside
COPYING, HTML and unknown suffix handling, supplementary PATENTS/UNLICENSE,
vendor refusal, embedded notice content and version/replacement rejection.

The Docker builder runs the generator after COPY of the source, on its native
build platform, before the target-specific binary build. Its final stage copies
/src/THIRD_PARTY_LICENSES.md to
/usr/share/doc/1-bit-bridge/THIRD_PARTY_LICENSES.md. Docker's GO_VERSION pins
the complete go.mod version (1.27.3), since the generator requires equality
with runtime.Version(), not merely a compatible major.minor. check-go-version
compares full versions, and check-build-tags requires the generator, final-stage
COPY and full toolchain pin. Release validation compares the image's notice
bytes with a fresh generation, for both published image architectures.

## B544 — developer test TMPDIR (2026-10-26)

- **Developer test targets (`test`, `test-fast`) use a target-specific exported TMPDIR, defaulting to absolute `$(CURDIR)/.tmp` when the caller's environment is unset or empty; keep `.tmp/` in both ignore files.** A non-empty caller value wins. Keep Unix-socket fixtures on short paths or bind by basename from their directory: deep checkouts exceed `sun_path` (B544).

The player refuses volatile storage, so a developer host with `/tmp` on
tmpfs needs test files on disk too. Only `test` and `test-fast` export the
fallback, creating it with quoted `mkdir -p`; a non-empty caller TMPDIR
wins. `.gitignore` and `.dockerignore` exclude `.tmp/`. CI keeps its own
`runner.temp`. A deep checkout can exceed Unix `sun_path` (104/108 bytes):
the socket census confirmed MPD uses short `/tmp/mpd*` directories and
fsutiltest binds a basename after Chdir. Keep those protections.

Proof recorded in `ops/engineering-log.md` under B544/B520: a real
unset-TMPDIR `make test` of config passed under `-race` with Go 1.27.3;
dry runs for unset, empty, caller-set and space-containing values showed
the correct quoted paths. This is a default location, not a claim that
every checkout is on persistent storage.

## B520 — self-dial candidate search (2026-10-26)

- **A host-address test must establish self-dialability, not just bindability, without switching to loopback or an unspecified address when the specific-IP branch is its subject.** B520 tries every non-loopback IPv4 candidate with a bounded bind/dial and source-IP check, listing failures when none works; skip locally, fail when `CI` is set. Preserve the logging negative control in the engineering log.

Binding a host address does not prove that it can dial itself. Try every
non-loopback, non-unspecified IPv4 candidate until bind, dial and the
source-IP check succeed, allowing at most 200 ms per candidate and its
share of a two-second network budget. Retain the successful listener and
connection for the silent probe, avoiding a second dial or rebind. If none
works, list every attempted address and its failure; skip locally and fail
when `CI` is present. Loopback or an unspecified address would miss the
specific-IP branch the test is meant to exercise.

`TestAProbeOfASpecificBoundIPIsNotLogged` passed without skipping on
darwin/arm64 with Go 1.27.3. The logging negative control in
`ops/engineering-log.md` adds `&& remote.IP.IsLoopback()` to
`fromThisHost`'s matching-IP return: the test then failed because it logged
the bound-IP probe, and passed after restoration. A separate restricted
run with all four binds denied exercised the local-skip and CI-fatal paths.
