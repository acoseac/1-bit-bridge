package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A save's rename is tried again, on Windows for up to 750 ms, while another
// process holds the file it replaces (a sharing violation): an editor saving
// it, an indexer, a virus scanner. Until B506's third round the comparison
// with the file the turn found, and bind's own, ran once, before the first
// attempt, so a change that landed between two attempts was renamed over.
// These tests make the first attempt fail through the rename seam, which is
// process-global: none of them calls t.Parallel.

// errSharingForTest stands for the sharing violation a rename on Windows is
// answered with while another process has the file open.
var errSharingForTest = errors.New("the process cannot access the file because it is being used by another process")

// failFirstRename makes the first rename onto target fail after running
// first, as a rename does while another process has the file open, and
// every later one rename as os.Rename does; it returns how many renames onto
// target were tried.
func failFirstRename(t *testing.T, target string, first func()) (attempts *int) {
	t.Helper()
	n := 0
	prev := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
		if dst != target {
			return os.Rename(src, dst)
		}
		n++
		if n == 1 {
			if first != nil {
				first()
			}
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errSharingForTest}
		}
		return os.Rename(src, dst)
	})
	t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(prev) })
	return &n
}

// requireNothingStaged fails the test if a save left a staged file in dir.
func requireNothingStaged(t *testing.T, dir string) {
	t.Helper()
	left, err := filepath.Glob(filepath.Join(dir, ".bridge-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the save left %q staged", left)
	}
}

// TestAChangeBetweenTwoRenameAttemptsIsRefused: a file another writer changes
// while a save's first rename fails is refused before the next attempt, and
// left as that writer left it, with no further attempt, the staged file
// removed, and what the turn compares with as it was.
func TestAChangeBetweenTwoRenameAttemptsIsRefused(t *testing.T) {
	dir := realTempDir(t)
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	const theirs = "libraryName: Theirs\n"
	attempts := failFirstRename(t, path, func() { writeMeanwhile(t, path, theirs) })
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	found := append([]byte(nil), l.found...)
	err = (&Config{LibraryName: "Mine"}).Save(l)
	var changed *ChangedInTurnError
	if !errors.As(err, &changed) || changed.Path != path {
		t.Errorf("a save whose first rename failed while another writer changed the file = %v, want a *ChangedInTurnError for %s", err, path)
	}
	if got, _ := os.ReadFile(path); string(got) != theirs {
		t.Errorf("the save's next attempt wrote over the other writer's file:\n%s", got)
	}
	if *attempts != 1 {
		t.Errorf("%d renames were tried, want none after the comparison refused", *attempts)
	}
	requireNothingStaged(t, dir)
	if l.absent || !bytes.Equal(l.found, found) {
		t.Errorf("the refused save moved what the turn compares with to %q", l.found)
	}
}

// TestARefusalBetweenRenameAttemptsEndsTheRetries: a refusal ends the
// retries at once. Here the other writer puts the file back as it was once
// the comparison has read its change, so a rename tried after the refusal
// would find the file unchanged and write over it.
func TestARefusalBetweenRenameAttemptsEndsTheRetries(t *testing.T) {
	dir := realTempDir(t)
	const before = "libraryName: Before\n"
	path := writeConfigFile(t, dir, before)
	const theirs = "libraryName: Theirs\n"
	attempts := failFirstRename(t, path, func() { writeMeanwhile(t, path, theirs) })
	t.Cleanup(SetTurnReadForTest(func(p string) ([]byte, error) {
		data, err := fsutil.ReadAsFile(p)
		if p == path && string(data) == theirs {
			writeMeanwhile(t, path, before)
		}
		return data, err
	}))
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := (&Config{LibraryName: "Mine"}).Save(l); !errors.Is(err, ErrChangedInTurn) {
		t.Errorf("a save whose comparison saw another writer's change between two renames = %v, want a refusal", err)
	}
	if got, _ := os.ReadFile(path); string(got) != before {
		t.Errorf("a rename after the refusal wrote over the file the other writer put back:\n%s", got)
	}
	if *attempts != 1 {
		t.Errorf("%d renames were tried, want none after the comparison refused", *attempts)
	}
	requireNothingStaged(t, dir)
}

// TestAnUnchangedFileSavesAcrossAFailedRenameAttempt: a file nobody changed
// is saved by the next attempt after a failed one: the comparison asked
// before each attempt refuses only a change.
func TestAnUnchangedFileSavesAcrossAFailedRenameAttempt(t *testing.T) {
	dir := realTempDir(t)
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	attempts := failFirstRename(t, path, nil)
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if err := (&Config{LibraryName: "Mine"}).Save(l); err != nil {
		t.Fatalf("a save whose first rename failed, over a file nobody changed: %v", err)
	}
	if *attempts != 2 {
		t.Errorf("%d renames were tried, want the failed one and the one that landed", *attempts)
	}
	if got, _ := os.ReadFile(path); !bytes.Contains(got, []byte("libraryName: Mine")) {
		t.Errorf("the save did not land:\n%s", got)
	}
	requireNothingStaged(t, dir)
}

// TestSaveIfIsAskedBeforeEveryRenameAttempt: SaveIf's own comparison (bind's)
// is asked before each attempt as the turn's is, and its refusal before the
// second attempt leaves the file as it is.
func TestSaveIfIsAskedBeforeEveryRenameAttempt(t *testing.T) {
	dir := realTempDir(t)
	const before = "libraryName: Before\n"
	path := writeConfigFile(t, dir, before)
	attempts := failFirstRename(t, path, nil)
	l, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	asked := 0
	refused := errors.New("the writer's own comparison refused")
	err = (&Config{LibraryName: "Mine"}).SaveIf(l, func(context.Context) error {
		asked++
		if asked > 1 {
			return refused
		}
		return nil
	})
	if err != refused {
		t.Errorf("SaveIf whose comparison refused before the second attempt = %v, want that refusal as it is", err)
	}
	if asked != 2 || *attempts != 1 {
		t.Errorf("SaveIf's comparison was asked %d times around %d renames, want twice around one", asked, *attempts)
	}
	if got, _ := os.ReadFile(path); string(got) != before {
		t.Errorf("SaveIf wrote over the file its comparison refused:\n%s", got)
	}
	requireNothingStaged(t, dir)
}
