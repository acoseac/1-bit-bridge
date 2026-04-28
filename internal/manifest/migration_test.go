package manifest

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrationLadderFreshDB asserts a freshly created DB lands at
// the highest migration version after OpenStore.
func TestMigrationLadderFreshDB(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	v := readUserVersion(t, s.db)
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Errorf("user_version = %d, want %d", v, want)
	}
}

// TestMigrationLadderIdempotent re-opens the same DB and asserts
// migrations don't re-run (no error, version stays).
func TestMigrationLadderIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "twice.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (1): %v", err)
	}
	v1 := readUserVersion(t, s.db)
	s.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore (2): %v", err)
	}
	defer s2.Close()
	v2 := readUserVersion(t, s2.db)
	if v1 != v2 {
		t.Errorf("user_version changed across reopen: %d -> %d", v1, v2)
	}
}

// TestMigrationLadderPreLadderUpgrade simulates a pre-ladder DB:
// schema applied via the legacy code path (CREATE TABLE IF NOT
// EXISTS + swallowed ALTER) but user_version still 0. Re-opening
// it should idempotently bump to the current head.
func TestMigrationLadderPreLadderUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "preLadder.db")

	// Simulate the pre-ladder schema by hand-applying migration 1
	// and then explicitly clearing user_version.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(migrations[0].sql); err != nil {
		t.Fatalf("apply baseline schema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatalf("clear user_version: %v", err)
	}
	db.Close()

	// Now open through the migration ladder. Should run migration 1
	// (idempotent, no-op effects on already-existent tables) and
	// bump user_version.
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()
	v := readUserVersion(t, s.db)
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Errorf("user_version = %d, want %d (current head)", v, want)
	}
}

func readUserVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
