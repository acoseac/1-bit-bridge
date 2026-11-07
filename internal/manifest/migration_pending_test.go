package manifest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestPendingMigrationsReadsTheStoreWithoutMigratingIt: the steps a store
// has not run, read from its user_version without running one or making
// anything (backlog B660's second round): one behind is one, the ladder's
// top none, a store that is not there every step, and no file is made.
func TestPendingMigrationsReadsTheStoreWithoutMigratingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.db")
	if n, err := PendingMigrations(path); err != nil || n != len(migrations) {
		t.Errorf("a store that is not there = %d, %v; want every step, %d", n, err, len(migrations))
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the read made the store (%v)", err)
	}
	top := migrations[len(migrations)-1].version
	if err := WriteDatabaseAtVersionForTest(path, top-1); err != nil {
		t.Fatal(err)
	}
	if n, err := PendingMigrations(path); err != nil || n != 1 {
		t.Errorf("a store one step behind = %d, %v; want 1", n, err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := readUserVersion(t, s.db); v != top {
		t.Fatalf("the store's user_version after the read and an open = %d, want the open to have migrated it to %d", v, top)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := PendingMigrations(path); err != nil || n != 0 {
		t.Errorf("a store at the ladder's top = %d, %v; want none", n, err)
	}
}

// TestPendingMigrationsReadsNoStepItself: the read migrates nothing, the
// store's user_version as it was after it.
func TestPendingMigrationsReadsNoStepItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	top := migrations[len(migrations)-1].version
	if err := WriteDatabaseAtVersionForTest(path, top-1); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingMigrations(path); err != nil {
		t.Fatal(err)
	}
	if n, err := PendingMigrations(path); err != nil || n != 1 {
		t.Errorf("the second read = %d, %v; want the step still pending", n, err)
	}
}
