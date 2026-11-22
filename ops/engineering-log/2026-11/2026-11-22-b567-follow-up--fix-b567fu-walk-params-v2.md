## 2026-11-22 — B567 follow-up: the sweeps from the root take their walk, and each skips make test's TMPDIR by its name

The follow-up to B567's review. Test-only: the files changed are the four
sweeps' tests and this record; `internal/sweeptest`, which tests alone
import, ships as item 3 left it. The branch starts at main dfc926f1. Its
first four commits are the first round's code, picked onto this base
(97817df4, bb3d0cbf, 83ec5225, 6e5fa340). The first round wrote its record
into the archive before the cut-over to one file per entry (B888), and that
record is not carried: this entry is the follow-up's.

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
passed as before. The cherry-pick of the first of the four conflicted in
`cmd/bridge/cited_test_names_test.go` alone, where main had added
`citationReadFile` beside the seam the pick removes: the resolution is main
plus the first round's own lines, proved by content (the lines it removes
from and adds to main are exactly those the first round's commit removes
from and adds to its own parent, 13 and 12, and so for every file of the
four picks).

### The skip is decided from the name

Each sweep's `.tmp` cases plant what make test leaves in its TMPDIR, the
root's `.tmp`, and the same files where the walks must still read them
(`TestTheCitationGuardsWalksSkipMakeTestsTMPDIRAtTheRoot`, both walks;
`TestHashCostSweepSkipsMakeTestsTMPDIRAtTheRoot`;
`TestFlacSweepSkipsMakeTestsTMPDIRAtTheRoot`), and remove a directory as
the walk reaches it (`TestTheCitationGuardsWalksFinishWhileMakeTestsTMPDIRChanges`,
`TestHashCostSweepFinishesWhileMakeTestsTMPDIRChanges`,
`TestFlacSweepFinishesWhileMakeTestsTMPDIRChanges`). Each of the removal
tests gained a row, "make test's TMPDIR itself", in which the `Removal`
removes the root's `.tmp` as the walk reaches it in the root's listing: the
walk reaches it (`Reached` must be true, where the other rows require
false), and it is gone before the sweep decides anything about it, so the
sweep finishes with the answer the tree gives without it only if it skips
the directory by its name, before listing it or asking anything of it.

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

### A root-walk pin, tried and parked

A root-walk pin was tried and parked. It listed, in `MakeTestTmpDir`'s doc,
every walk in the module's Go code that can start at the checkout's root,
with the tests that make it from there and the rule by which it skips make
test's TMPDIR, and held the module to that list from each file's AST; a
static check of each walk's control flow drew a new false pass in each
round (an earlier return, an `&&` guard that defeats the skip, a scope
exception for one directory, a branch for a directory that never reaches
`filepath.SkipDir`), so its two files and the list are removed, the commits
that built and extended it (6e5fa340, 450c3d40, c473d4a9) staying in the
branch's history. Its last run, on 378c0d2c just before the removal,
passed: every walk it found was listed, or named as walking elsewhere, with
its rule held as it then checked it. That is history, not a verdict on the
tree: by its own account it did not see a walk a command makes (`go list
./...`) or one kept as a queue of directories, and the false passes above
were found in it. So present coverage is unverified, beside the residual: a
new walk from the checkout's root can miss make test's TMPDIR until a
reviewer sees it, while the four sweeps here keep their own `.tmp` cases.
Reopen it if a test is found walking into that directory.

9e99dc1f's docblock change, `scanTestCitationsIn`'s naming #921 alone,
stays.

### Item 1's controls

At a5c3d3c9, the pin removed, predicted before the runs. Each control is a
file of that commit with one replacement made once (two files for (a′),
whose two callbacks share `skipsForCitations`), handed to `go test` by
`-overlay`, so nothing tracked was edited while the runs below built from
the tree; each selection ran `-count=1 -timeout 45m`, green at the commit
first. Every result was as predicted:

| Mutation | Selection | Red | Green |
|---|---|---|---|
| (a) `skipsForCitations` without its TMPDIR check | `TestTheCitationGuardsWalksSkipMakeTestsTMPDIRAtTheRoot` | both walks' two TMPDIR rows: each walk read `.tmp`'s planted citation or call, or failed on its half-written files | each walk's other three rows |
| (b) `hashCostDirRule` without its check | `TestHashCostSweepSkipsMakeTestsTMPDIRAtTheRoot` | its two TMPDIR rows | the other three |
| (c) `flacDirRule` without its check | `TestFlacSweepSkipsMakeTestsTMPDIRAtTheRoot` | its TMPDIR row | the other three |
| (a′) both citation callbacks ask `d.Info()` of a directory before `skipsForCitations`, returning its error | each walk's "make test's TMPDIR itself" row of `TestTheCitationGuardsWalksFinishWhileMakeTestsTMPDIRChanges` | both: `lstat …/.tmp: no such file or directory` | none selected |
| (b′) `hashCostSetterCallers` asks `d.Info()` before `hashCostDirRule` | that row of `TestHashCostSweepFinishesWhileMakeTestsTMPDIRChanges` | it, on the same lstat | none selected |
| (c′) `leakyFlacCalls` asks `d.Info()` before `flacDirRule` | that row of `TestFlacSweepFinishesWhileMakeTestsTMPDIRChanges` | it, on the same lstat | none selected |

So each sweep's own cases fail without its skip, and its "TMPDIR itself"
row fails for a skip that asks anything of the directory first: the cases
stand without the pin.

### The runs

On Linux amd64 (Ubuntu 26.04, kernel 7.0.0-38-generic, 16 cores, 8 of them
used at a time), Go go1.27.3, go.mod's toolchain, from an offline module
cache (`GOPROXY=off`), TMPDIR outside the checkout, the Makefile's tags
(`ts_omit_webclient,ts_omit_debugeventbus`), at a5c3d3c9:

- the touched tests under `-race -count=20 -timeout 45m`, three
  single-package lanes at `GOMAXPROCS=2`, by the selectors
  `TestTheCitationGuardsWalks(SkipMakeTestsTMPDIRAtTheRoot|FinishWhileMakeTestsTMPDIRChanges)`,
  `TestHashCostSweep(SkipsMakeTestsTMPDIRAtTheRoot|FinishesWhileMakeTestsTMPDIRChanges)`
  and `TestFlacSweep(SkipsMakeTestsTMPDIRAtTheRoot|FinishesWhileMakeTestsTMPDIRChanges)`:
  each of the six tests passed 20 times, no race reported;
- `go vet` with the tags as linux, darwin and windows: clean;
- `make test` once, uncached (`GOFLAGS=-count=1`, `P=4` at
  `GOMAXPROCS=2`): exit 0 after 2,056 s, 92 packages ok and 15 with no test files, none failed, none cached and no race reported; `cmd/bridge` took 1,752 s and `internal/manifest` 1,589 s of their 45 minutes;
- `./internal/sweeptest`, `./cmd/bridge`, `./internal/adminauth` and
  `./internal/manifest` once each under `-race` (`-count=1 -timeout 45m`,
  `-p 2`, `GOMAXPROCS=4`): all ok, in 1 s, 1,681 s, 58 s and 1,558 s; exit 0 after 1,683 s;
- the controls above;
- `qualification.CheckVersion(dfc926f1, HEAD)` answers nil: no guarded
  path changed, so no `qualificationVersion` bump.

At this record's commit: `make fmt` changed nothing, `make vet` and `make
docs-check` (the engineering log's check among it) were clean, and
`TestEveryCitedTestNameExists` passed with this entry tracked.
