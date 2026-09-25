// Package sweeptest holds what the tests that sweep this repository's own
// files have to agree on. Like net/http/httptest it is imported only by
// tests, so none of it reaches the binary.
//
// It exists so there is ONE definition of each rule. The sweeps that walk
// the tree from the module root live in several packages, and a rule copied
// into each of them is several rules the day one copy changes.
package sweeptest

import (
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
// or a submodule's `gitdir:` file, or a symlink, dangling or not. Any error
// from os.Lstat reads the directory, so one that cannot be searched fails
// the walk that lists it, which is the honest failure.
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
