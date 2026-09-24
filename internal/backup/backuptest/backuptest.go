// Package backuptest parks a SQLite VACUUM in the middle of its copy, so a
// test can cancel a snapshot while the copy is running rather than before it
// starts. Like net/http/httptest it is imported only by tests, so none of it
// reaches the binary.
//
// The park is internal/sqlitetest's collation. VACUUM copies an index with an
// append fast path that compares no keys, EXCEPT an index with a column whose
// collation is not BINARY: SQLite rebuilds that one with ordinary seeks,
// because a collation may have been redefined since the keys were written
// (insert.c, xferOptimization). Every seek calls the collation, and that
// collation is a Go function. So a VACUUM of a database WriteSource wrote
// calls back into sqlitetest mid-copy, with its destination file already
// created, and a test can stop it there with no hook in production code.
package backuptest

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/sqlitetest"
)

// sourceRows is how many rows WriteSource writes. A VACUUM compares keys only
// from the second row on, and ReleaseUntil needs enough comparisons to give
// the driver's interrupt several chances to land (see its docblock).
const sourceRows = 32

// WriteSource adds to the SQLite database at path, creating it if it is
// absent, a table whose index is ordered under the park's collation, so a
// VACUUM of that database compares keys through sqlitetest. It opens the
// database in WAL mode, as the bridge opens its own manifest database.
//
// Call it before ParkVacuum: writing the rows compares keys too.
func WriteSource(t testing.TB, path string) {
	t.Helper()
	if sqlitetest.Armed() {
		t.Fatal("backuptest: WriteSource with a Park armed would park its own inserts")
	}
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatalf("backuptest: open %s: %v", path, err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE parked (k TEXT)`,
		`CREATE INDEX parked_k ON parked (k COLLATE ` + sqlitetest.Collation + `)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("backuptest: %s: %v", stmt, err)
		}
	}
	for i := 0; i < sourceRows; i++ {
		if _, err := db.Exec(`INSERT INTO parked (k) VALUES (?)`, fmt.Sprintf("key-%02d", i)); err != nil {
			t.Fatalf("backuptest: insert row %d: %v", i, err)
		}
	}
}

// ParkVacuum arms a sqlitetest Park, and disarms it when the test ends. Wait
// then returns once a VACUUM is in the middle of copying WriteSource's index,
// with its destination file on disk.
func ParkVacuum(t testing.TB) *sqlitetest.Park {
	t.Helper()
	return sqlitetest.Arm(t)
}
