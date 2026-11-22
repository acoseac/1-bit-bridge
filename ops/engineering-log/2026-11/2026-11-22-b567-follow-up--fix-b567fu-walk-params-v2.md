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
