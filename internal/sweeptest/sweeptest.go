// Package sweeptest holds what the tests that sweep this repository's own
// files have to agree on. Like net/http/httptest it is imported only by
// tests, so none of it reaches the binary.
//
// It exists so there is ONE definition of each rule. The sweeps that walk
// the tree from the module root live in several packages, and a rule copied
// into each of them is several rules the day one copy changes.
package sweeptest

import (
	"io/fs"
	"os"
	"path/filepath"
)

// IsOtherCheckout reports whether dir, a directory a walk from root has
// reached, is the top of another checkout: a directory below root that holds
// a `.git` entry of its own. A sweep skips such a directory before listing
// it. The root is never one, so a sweep of a worktree, whose `.git` is a
// file, still reads the worktree.
//
// Nothing below such a directory belongs to this checkout, and CI's clone
// never has it, so a sweep that read it answered differently in the one
// checkout that held it. Claude Code keeps its worktrees of other branches
// inside the tree, under .claude/worktrees/, and each is a whole checkout.
// With the three that were in the main checkout on 2026-09-25, 1,208 of the
// 1,617 files the hash-cost guard opened were theirs, and so were 3,366 of
// the 4,513 the flac-handle guard opened. A half-written file or a
// work-in-progress call in any of them failed this checkout's run.
//
// It is not the go tool's rule, which skips a directory holding its own
// go.mod: another module. A checkout has no go.mod while git is still
// writing it (in index order, cmd/ and .github/ come before go.mod), nor
// when it is another repository's, and a nested module need not be a
// checkout at all. The two rules answer different questions, so a sweep
// that needs both applies both.
//
// Any `.git` entry counts, and os.Lstat finds it: a directory, a worktree's
// or a submodule's `gitdir:` file, or a symlink, dangling or not. So a
// submodule is skipped too, as it should be: git tracks it here only as a
// commit id, and its files are another repository's. Any error from
// os.Lstat reads the directory, so one that cannot be searched fails the
// walk that lists it, which is the honest failure.
//
// Git will not add a file inside another repository, so in a clone this
// drops no tracked file. Git disagrees about three shapes only a local
// checkout can hold: a `.git` that is not a repository (an empty directory,
// or a `gitdir:` file pointing nowhere), and a repository made with `git
// init` in a directory whose files were already tracked. Git keeps tracking
// those files and a local sweep skips them. CI's clone has none of the
// three, so the gate still reads them.
func IsOtherCheckout(root, dir string) bool {
	if filepath.Clean(dir) == filepath.Clean(root) {
		return false
	}
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// MakeTestTmpDir is the directory below the root that make test and make
// test-fast give go test as TMPDIR when the caller sets none (the
// Makefile's TMPDIR line). While the suite runs, every package's tests make
// and remove their temporary files there, and the go command its work
// directories, so a sweep that read it took another package's temporary Go
// files for this tree's, and failed on a directory removed between its
// listing and its own (B567). Nothing there is the repository's: both
// ignore files exclude it.
//
// The build-tag census skips the same directory by a constant of its own,
// makeTestTmpDir in scripts/third-party-licenses (B740). It cannot share
// this one: the census is a program, and this package is imported by tests
// alone. Nor can this package reach a program's constant. So
// TestTheSweepsSkipTheDirectoryTheCensusSkips, beside the census, holds the
// two to one name, and TestTheCensusSkipsTheMakefilesTMPDIR holds the
// census's to the Makefile.
//
// Every walk in this module's Go code that can start at the checkout's root
// skips the directory there before it lists it, by one of two rules: by its
// name, as IsMakeTestTmpDir answers (the census by its own makeTestTmpDir),
// or as a dot-directory, with every directory whose name begins with a dot,
// as the go tool skips them (`go help packages`). Each is listed here by its
// file and the function that walks, then the tests that make that walk from
// the root, and its rule with the function that holds it:
//
//   - cmd/bridge/cited_test_names_test.go scanTestCitationsIn, from
//     TestEveryCitedTestNameExists: by its name, in skipsForCitations.
//   - cmd/bridge/itunes_unwired_test.go productionCallsIn, from
//     TestTheWireManifestIsTheProvidersAlone and
//     TestNoProductionPathAsksITunes: by its name, in skipsForCitations.
//   - internal/adminauth/hashcost_test.go hashCostSetterCallers, from
//     TestNoProductionCodeLowersTheHashCost: by its name, in hashCostDirRule.
//   - internal/manifest/flac_handle_discipline_test.go leakyFlacCalls, from
//     TestNoLeakyFlacConstructors: by its name, in flacDirRule.
//   - internal/sweeptest/rootwalks_test.go readModule, from
//     TestEveryWalkFromTheRootIsListed: by its name, in skipsDir.
//   - scripts/third-party-licenses/tags.go discoverBuildSites, from
//     TestRealTreeBuildSiteCensus and TestBuildTagsAgree: by its name, in
//     discoverBuildSites.
//   - cmd/bridge/blank_keepers_test.go findBlankKeepers, from
//     TestNoBlankKeepers: as a dot-directory, in moduleDirRule.
//   - cmd/bridge/config_writers_test.go findConfigWriters, from
//     TestEveryConfigWriterTakesTheConfigLock: as a dot-directory, in
//     moduleDirRule.
//   - cmd/bridge/default_logger_sweep_test.go findDefaultLoggerSetters, from
//     TestNoTestSetsTheDefaultLoggerByHand: as a dot-directory, in
//     moduleDirRule.
//   - cmd/bridge/player_identity_test.go
//     TestEveryPlayerUUIDMinterUsesFloorChecks, from
//     TestEveryPlayerUUIDMinterUsesFloorChecks: as a dot-directory, in
//     moduleDirRule.
//   - cmd/bridge/served_files_test.go TestEveryServedFileIsOpenedAsAFile,
//     from TestEveryServedFileIsOpenedAsAFile: as a dot-directory, in
//     moduleDirRule.
//   - cmd/bridge/store_path_pin_test.go parseProductionFiles, from
//     TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase: as a dot-directory,
//     in moduleDirRule.
//   - cmd/bridge/docblock_subject_test.go scanDocblockSubjects, from
//     TestNoDocblockNamesAnotherDeclaration: as a dot-directory, in
//     scanDocblockSubjects.
//   - cmd/bridge/server_errorlog_redacts_test.go parseModuleServerSources,
//     from TestEveryServeHTTPServerRedactsPeerAddresses: as a dot-directory,
//     in parseModuleServerSources.
//   - internal/fsutil/walkroot_sweep_test.go sweepTreeWalks, from
//     TestEveryWalkOfALibraryRootStartsFromWalkableRoot: as a
//     dot-directory, in sweepDirRule.
//   - internal/player/dsdconv/served_test.go moduleImports, from
//     TestNoConvertedStreamIsServed: as a dot-directory, in moduleImports.
//
// TestEveryWalkFromTheRootIsListed holds the module to this list. It finds
// every function of the module that walks a directory tree, test files and
// programs included, and a walk handed in as a parameter, and fails on one
// listed neither here nor among the walks it names as starting elsewhere,
// on an item here that names no such function, on a listed test that does
// not reach its walk, and on a rule its walk's branch for a directory does
// not reach through the function named. One more test starts at the root,
// by another road: TestEveryEmbedPatternRefusesALeadingDot runs go list over
// ./..., and the go command skips the directory as a dot-directory. The pin
// reads Go code, so a walk the go command makes for a test is not one it
// sees.
const MakeTestTmpDir = ".tmp"

// IsMakeTestTmpDir reports whether dir, a directory a walk from root has
// reached, is make test's TMPDIR: root's own MakeTestTmpDir. A sweep skips
// it by that name before listing it, as the census does. Only that one: a
// directory of the same name further down, or one whose name merely begins
// with it, is the tree's like any other, and the root itself never is,
// whatever it is called.
//
// The name is compared exactly, on a filesystem that folds case too (macOS's
// default APFS, Windows), where a .TMP at the root is read. The Makefile
// makes the directory itself, by the name its TMPDIR line gives (mkdir -p),
// so make test's TMPDIR is .tmp wherever make test made it, and the census
// compares the name exactly too (scripts/third-party-licenses/tags.go,
// makeTestTmpDir): a fold here alone would split this rule from the
// census's. What stays open is a .TMP that was there first, on a filesystem
// that folds: mkdir -p finds it under the Makefile's name and makes nothing,
// so make test's files land in a directory a walk lists as .TMP, which a
// sweep that skips the directory by its name reads, the census among them,
// and one that skips every dot-directory still skips.
func IsMakeTestTmpDir(root, dir string) bool {
	return filepath.Clean(dir) == filepath.Join(root, MakeTestTmpDir)
}

// Removal stands in for another process removing a directory while a sweep
// walks the tree that holds it, as go test removes a test's temporary
// directory and the go command a work directory. Its WalkDir walks as
// filepath.WalkDir does, and at the moment the walk reaches Dir, after the
// directory above it was listed and before Dir is, it removes Dir and
// everything under it. A sweep that reads Dir then fails to list it; one
// that skips what holds Dir never reaches it. The removal happens at that
// point of the walk, on the walk's own goroutine, never on a race's time.
type Removal struct {
	Dir string
	// Walked is whether a walk went through this Removal at all, so a test
	// can tell a sweep that never reached Dir from one that never used it.
	Walked bool
	// Reached is whether the walk reached Dir, and so removed it.
	Reached bool
}

// WalkDir is filepath.WalkDir but for removing r.Dir as the walk reaches it.
func (r *Removal) WalkDir(root string, fn fs.WalkDirFunc) error {
	r.Walked = true
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && !r.Reached && filepath.Clean(path) == filepath.Clean(r.Dir) {
			r.Reached = true
			if err := os.RemoveAll(r.Dir); err != nil {
				return err
			}
		}
		return fn(path, d, err)
	})
}
