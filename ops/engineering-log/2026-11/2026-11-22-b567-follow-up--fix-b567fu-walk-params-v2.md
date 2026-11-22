## 2026-11-22 — B567 follow-up: the sweeps from the root take their walk, every walk from the root is listed, and a rule holds only where its predicate skips the directory

The follow-up to B567's review. Test-only: the files changed are tests and
`internal/sweeptest`, which tests alone import. The branch starts at main
dfc926f1. Its first four commits are the first round's code, picked onto
this base (97817df4, bb3d0cbf, 83ec5225, 6e5fa340); 450c3d40 takes in the
walks the base added; c473d4a9
makes a rule hold only where its predicate skips the directory, and keeps a
call of another package's function from reaching; 9e99dc1f names #921 alone
in `scanTestCitationsIn`'s docblock; this record follows. The first round
wrote its record into the archive before the cut-over to one file per entry
(B888), and that record is not carried: this entry is the follow-up's.

### The walk as a parameter

B567 gave three packages a walk seam at package level that its removal tests
reassigned: `sweepWalkDir` (`cmd/bridge`), `hashCostWalkDir`
(`internal/adminauth`) and `flacWalkDir` (`internal/manifest`). The four
sweeps take their walk as a parameter instead, as `productionCalls` already
handed `productionCallsIn` its root: `scanTestCitationsIn`
(`cmd/bridge/cited_test_names_test.go`), `productionCallsIn`
(`cmd/bridge/itunes_unwired_test.go`), `hashCostSetterCallers`
(`internal/adminauth/hashcost_test.go`) and `leakyFlacCalls`
(`internal/manifest/flac_handle_discipline_test.go`). The whole-tree tests
and the fixtures hand them `filepath.WalkDir`, the removal tests a
`sweeptest.Removal`'s `WalkDir`, and the three variables are gone, so no
test changes its package's state. A pure refactor: every touched test
passed as before, and nothing it does changes an answer, so it has no red
step. The cherry-pick of 97817df4 conflicted in
`cmd/bridge/cited_test_names_test.go` alone, where main had added
`citationReadFile` beside the seam the pick removes: the resolution is main
plus the first round's own lines, proved by content (the lines it removes
from and adds to main are exactly those the first round's commit removes
from and adds to its own parent, file by file, for every file of the four
picks).

### The skip is decided from the name

Each removal test gained a row, "make test's TMPDIR itself", in which the
`Removal` removes the root's `.tmp` as the walk reaches it in the root's
listing: the walk reaches it (`Reached` must be true, where the other rows
require false), and it is gone before the sweep decides anything about it,
so the sweep finishes with the answer the tree gives without it only if it
skips the directory by its name, before listing it or asking anything of
it. The rows are `cmd/bridge/sweeps_tmpdir_test.go`'s (both walks),
`internal/adminauth/hashcost_test.go`'s,
`internal/manifest/flac_handle_discipline_test.go`'s and the pin's own,
`TestTheWalkSweepFinishesWhileMakeTestsTMPDIRChanges`.

### The exact match

`IsMakeTestTmpDir`'s doc (`internal/sweeptest/sweeptest.go`) says why the
name is compared exactly where the filesystem folds case too: the Makefile
makes the directory itself, by the name its TMPDIR line gives (`mkdir -p`),
so make test's TMPDIR is `.tmp` wherever make test made it, and the census
compares the name exactly (`scripts/third-party-licenses/tags.go`,
`makeTestTmpDir`), so a fold in one rule alone would split the two. The
residual is a `.TMP` that was there first on a filesystem that folds case:
`mkdir -p` finds it and makes nothing, so make test's files land in a
directory a walk lists as `.TMP`, which a sweep that skips the directory by
its name reads, the census among them, and one that skips every
dot-directory still skips. Not run: no volume that folds case can be made
on the Linux host the runs below used.

### Every walk from the root, listed and pinned

`MakeTestTmpDir`'s doc lists the walks in the module's Go code that can
start at the checkout's root, seventeen on this base, each by its file and
the function that walks, the tests that make it from the root (exact `func
Test…` names, which the citation guard reads in sweeptest.go whole), and its
rule with the function that holds it. By its name: the four B567 sweeps,
`TestEveryOpenerOfTheLiveDatabaseTakesTheStoreLock`'s walk, the census
(`discoverBuildSites`, by its own `makeTestTmpDir`) and the pin. As a
dot-directory: `moduleDirRule`'s six (`findBlankKeepers`,
`findConfigWriters`, `findDefaultLoggerSetters`,
`TestEveryPlayerUUIDMinterUsesFloorChecks`'s walk,
`TestEveryServedFileIsOpenedAsAFile`'s, `parseProductionFiles`),
`scanDocblockSubjects`, `parseModuleServerSources`, fsutil's
`sweepTreeWalks` and dsdconv's `moduleImports`. One more test starts at the
root by another road, named beside the list:
`TestEveryEmbedPatternRefusesALeadingDot` runs `go list ./...`, and the go
command skips the directory as a dot-directory.

`TestEveryWalkFromTheRootIsListed` (`internal/sweeptest/rootwalks_test.go`)
reads every Go file of the module, test files and the programs under
`scripts/` and `.github/` included, skipping the root's `.tmp` by
`IsMakeTestTmpDir`, `.git`, another checkout, a `_` name, testdata, vendor,
node_modules and another module. From each file's AST it finds every
function that walks a tree: a call of filepath.WalkDir, filepath.Walk,
fs.WalkDir or fsutil.WalkConfigured; a walk callback written in it; a call
through a walk it is handed or holds (a parameter, variable or field in
filepath.WalkDir's shape, a named type of that shape resolved in its
package); a callback its package declares, handed on; or a directory listed
by a function or closure that calls itself. A function that only hands a
walk on is no walk: the one it hands it to is. Each walk found must be an
item of the list, its tests in its directory reaching it through the
package's calls and its rule holding as the next sections say, or an entry
of `walksElsewhere` (68: the bridge's own walks, the fixtures', and those
below the root or outside the checkout), with what it walks. A stale item
or entry fails, a walk in both fails, and a walk a test file holds at
package level fails whatever the lists say; the one walk of the bridge's
own code held there, `internal/fsutil/walkroot.go`'s
`walkResolvedDirectory` (nil in production, which fsutil's tests set), is
an entry. What the pin cannot see: a walk a command makes (`go list ./...`),
or one kept as a queue of directories.

`TestTheWalkSweepFindsEveryShapeOfWalk`,
`TestTheWalkSweepFinishesWhileMakeTestsTMPDIRChanges`,
`TestTheInventoryReadsEachItemAsWritten`,
`TestCheckWalksNamesEveryWayTheListsCanBeWrong` and
`TestARuleHoldsOnlyWhereItsPredicateSkipsTheDirectory` pin the pin, each
over a module of its own. go/doc/comment dedents a doc by its common
indentation, so a fixture doc that is a list alone reads as prose: two
fixture rows carry a lead paragraph.

### The base's new walks

On dfc926f1 the pin found four walks the first round's lists did not hold.
`TestEveryOpenerOfTheLiveDatabaseTakesTheStoreLock`
(`internal/manifest/store_open_lock_sweep_test.go`) walks from the root and
skips the TMPDIR by `sweeptest.IsMakeTestTmpDir` in its branch for a
directory: an item now. Three walk a fixture's tree and are entries of
`walksElsewhere`: `entriesUnder` (`cmd/bridge/init_keep_data_dir_test.go`),
`filesBelow` (`cmd/bridge/serve_held_binary_test.go`) and `treeState`
(`internal/backup/store_exclusion_test.go`).

### A rule holds only where its predicate skips the directory

The first round's check took a rule as held once the walk's branch for a
directory called the predicate, anywhere, matched by a bare name, or
reached any function of the package that did. Three shapes passed it:
dsdconv's skip (`internal/player/dsdconv/served_test.go`, `moduleImports`)
with its `return filepath.SkipDir` made `return nil`, which walks into
`.tmp`; a function of the walk's own package named `IsMakeTestTmpDir` that
answers false; and a call whose answer is dropped (`_ =
sweeptest.IsMakeTestTmpDir(root, path); return nil`).

The check (`skipCheck`) now requires the effect: the predicate's true leads
to a return of `filepath.SkipDir` or `fs.SkipDir`. In the branch, that is a
condition holding the predicate whose body goes straight to that return (its
first return at its top level, no statement before it holding a return),
the predicate in the condition alone, in parentheses, or as a side of a
`||` or an `&&` (whose other side can only guard it), never under a `!`,
compared, or passed on as an argument. Or it is by way of a helper of the
walk's package, a function declared to return `bool` or `error` alone: a
boolean one in such a condition, whose own body returns true where the
predicate holds, in the same shapes or as the expression it returns
(`skipsForCitations`, `skipsDir`, `goToolIgnores`); or one whose error the
branch returns, whose body returns SkipDir where the predicate holds, a
boolean helper of its own in its condition included (`hashCostDirRule`,
`flacDirRule`, `moduleDirRule`, `sweepDirRule`). The branch is the if on the
entry's IsDir, its condition and its body; its else, which a file takes, is
not. The item's `in` must be on the way from the branch to the predicate,
any way where it is the walk's own function. Both rules are held to it: the
dot-directory rule's `strings.HasPrefix` of a name and "." as much as the
rule by name. What the check does not follow, and fails on, saying what it
wants: the predicate's answer kept in a variable, a negated form (`if !p {
return nil }`, then SkipDir), a switch on it.

The predicate by name is `sweeptest.IsMakeTestTmpDir` through the file's
import of `internal/sweeptest`, as `strings.HasPrefix` is matched through
the file's import of `strings`. Two directories name it in a form of their
own, each counting there alone, where the directory declares the function
or the constant: this package's own `IsMakeTestTmpDir`, called unqualified
in `internal/sweeptest` (`skipsDir`), and the census's comparison of a name
with its `makeTestTmpDir`, in `scripts/third-party-licenses`
(`discoverBuildSites`).

### A call into another package reaches nothing

`calleeNames` and `reached` took a selector's last name from any call, so a
test calling `other.walkTree(...)` reached the walk package's own
`walkTree`. Both, and the check's look into helpers, now leave out a
selector on a name the file imports a package as (`localCallee`), as the
search for walks does (`walkReasons`).

### The fixtures, red and green

`TestARuleHoldsOnlyWhereItsPredicateSkipsTheDirectory` runs `checkWalks`
over a module of one walk from the root, listed with its rule, a case for
each shape; `TestCheckWalksNamesEveryWayTheListsCanBeWrong` gained the row
for a listed test that calls only another package's function of its walk's
name. Each was run three ways with nothing tracked edited: against the old
check (the committed check at 450c3d40, by `go test -overlay` beside a shim
naming the two directory constants the cases use), against the control (the
new check with its effect requirement reverted alone: `skipsIn` replaced by
the first round's reach, the predicate anywhere in the branch or in any
function of the package it calls, whatever that function returns, keeping
the real predicate's match and the package filter), and against the new
check. The predictions were written before the runs, and every result was
as predicted. A row is green when `checkWalks` answers as the row expects:
one problem naming the rule for a shape that must fail, none for one that
must hold.

| Case | Expects | Old check | Control | New check |
|---|---|---|---|---|
| dsdconv's condition, as it stands | holds | green | green | green |
| dsdconv's condition, its SkipDir made nil | fails | red | red | green |
| a function of the walk's own package named IsMakeTestTmpDir | fails | red | green | green |
| the predicate called, its answer dropped | fails | red | red | green |
| sweeptest's own IsMakeTestTmpDir, in sweeptest | holds | green | green | green |
| the census's comparison with its own makeTestTmpDir | holds | green | green | green |
| another package's IsMakeTestTmpDir | fails | red | green | green |
| a constant named makeTestTmpDir outside the census | fails | red | green | green |
| a boolean helper's true, which the branch turns into SkipDir | holds | green | green | green |
| a boolean helper's answer, ignored | fails | red | red | green |
| a helper's SkipDir, which the branch returns | holds | green | green | green |
| a helper's SkipDir, dropped | fails | red | red | green |
| a helper's SkipDir where a boolean helper of its own holds | holds | green | green | green |
| another package's function of a helper's name | fails | red | green | green |
| a listed test that calls another package's function of its walk's name | fails | red | green | green |

Under the old check `checkWalks` answered nothing for each red row; under
the control, for the four rows the effect alone decides, among them the
first and the third of the three shapes above. The control fails dsdconv's
condition made nil. `TestEveryWalkFromTheRootIsListed` passed in all three
runs, and so did every other row of both tests.

On the real tree: an archive copy of c473d4a9 whose
`internal/player/dsdconv/served_test.go` returns nil where it returned
`filepath.SkipDir` failed the new check with one problem, "MakeTestTmpDir's
doc says internal/player/dsdconv/served_test.go moduleImports skips the
TMPDIR as a dot-directory, in moduleImports, and no branch its walk takes
for a directory (an if on the entry's IsDir) returns filepath.SkipDir where
strings.HasPrefix of a name and "." holds, by way of moduleImports", and
passed the old check (its rootwalks_test.go copied in, the new fixtures
left out); the copy unmutated passed the new check.

### The first round's controls

At the first round's code (6db77bcf, over main fe5e331d), predicted before
the run, each mutated file kept on disk while the tests ran (the pin reads
the tree as it runs), the bytes put back after; every one as predicted, the
baseline green:

| Mutation | Red |
|---|---|
| C1 `skipsForCitations` without the skip | both walks: the two TMPDIR rows each, all three removal rows each; the pin, for both walks' rule |
| C2 `hashCostDirRule` without the skip | its two TMPDIR rows, its three removal rows; the pin |
| C3 `flacDirRule` without the skip | its TMPDIR row, its three removal rows; the pin |
| C4 `hashCostSetterCallers` asks `d.Info()` of a directory before its rule | the "TMPDIR itself" row alone (`lstat …/checkout/.tmp: no such file or directory`) |
| C5 the pin's `skipsDir` without the skip | its three removal rows, the shapes fixture (the TMPDIR's half-written file fails the parse), the pin |
| C6 a root walker through a parameterized helper, planted in cmd/bridge | the pin, one problem: the walk named neither in the doc nor in `walksElsewhere` |
| C7 the same helper with a named callback | the pin, the same problem, naming the callback |
| C8 dsdconv's item removed from the doc | the pin: `moduleImports` named neither |
| C9 a `walksElsewhere` key misspelt | the pin, two problems: the walk named neither, the entry naming no walk |
| C10 the flac item's test renamed to `TestFixtureWritersLeaveNoOpenHandle` | the pin: no test of that name in its directory reaches it |
| C11 `var flacWalkDir = filepath.WalkDir` put back | the pin: a walk held at package level in a test file |
| C12 `parseModuleServerSources`' branch for a directory without its dot rule (its file branch keeps one) | the pin: no branch for a directory holds the dot rule |

### The runs

On Linux amd64 (Ubuntu 26.04, kernel 7.0.0-38-generic, 16 cores, 8 of them
used at a time), Go go1.27.3, go.mod's toolchain, from an offline module
cache (`GOPROXY=off`), TMPDIR outside the checkout, the Makefile's tags
(`ts_omit_webclient,ts_omit_debugeventbus`), at 9e99dc1f:

- The touched tests under `-race -count=50`, one single-package lane each at
  `GOMAXPROCS=2`, the four together: `internal/adminauth` (the five
  hash-cost sweep tests) ok in 94 s, `internal/manifest` (the four flac
  sweep tests) 273 s, `internal/sweeptest` (the package) 361 s, and
  `cmd/bridge` (the twelve tests of the citation guard's files and of the
  production-call walk) 616 s; no race reported.
- `go vet` with the Makefile's tags as linux, darwin and windows: clean.
- `make test` once (`P=4` at `GOMAXPROCS=2`, `-count=1`): exit 0 after
  2,077 s, 92 packages ok and 15 with no test files, none failed and no race
  reported; `cmd/bridge` took 1,747 s and `internal/manifest` 1,591 s of
  their 45 minutes.
- `./cmd/bridge`, `./internal/adminauth`, `./internal/manifest`,
  `./internal/sweeptest` and `./internal/player/dsdconv` once each under
  `-race` (`-p 2`, `GOMAXPROCS=4`): all ok, in 1,681 s, 58 s, 1,556 s, 8 s
  and 43 s; exit 0 after 1,684 s.
- The fixtures and the control by name, and the real tree's run, as above.
- `qualification.CheckVersion(dfc926f1, HEAD)` answers nil: no guarded path
  changed, so no `qualificationVersion` bump.

At this record's commit: `make fmt` changed nothing, `make vet` and `make
docs-check` (the engineering log's check among it) were clean, and
`TestEveryCitedTestNameExists` passed with this entry tracked.
