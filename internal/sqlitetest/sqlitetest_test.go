package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestAParkedWriteIsStoppedByItsCancel pins what the package promises: an
// UPDATE that moves a key in an index under the collation, an INSERT into
// it, and a DELETE from it each park inside the statement, and a cancel
// while parked comes back from the statement as context.Canceled. Unparked,
// the same statement completes (the control that the park is not the cause).
func TestAParkedWriteIsStoppedByItsCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		stmt string
		args []any
	}{
		{"an update that moves a key", `UPDATE t SET n = n + 1 WHERE id = ?`, []any{3}},
		{"an insert", `INSERT INTO t (id, n) VALUES (?, ?)`, []any{99, 7}},
		{"a delete", `DELETE FROM t WHERE id = ?`, []any{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openParkTable(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			park := Arm(t)
			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				_, err = db.ExecContext(ctx, tc.stmt, tc.args...)
			}()
			park.Wait(t)
			cancel()
			park.ReleaseUntil(t, done)
			<-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the parked statement returned %v, want context.Canceled", err)
			}
		})
		t.Run(tc.name+", not cancelled", func(t *testing.T) {
			db := openParkTable(t)
			park := Arm(t)
			done := make(chan struct{})
			var err error
			go func() {
				defer close(done)
				_, err = db.ExecContext(context.Background(), tc.stmt, tc.args...)
			}()
			park.Wait(t)
			park.ReleaseUntil(t, done)
			<-done
			if err != nil {
				t.Fatalf("the statement failed with nothing cancelled: %v", err)
			}
		})
	}
}

// openParkTable opens a WAL database holding a table whose index orders an
// integer column's text under the collation, with rows already in it, so a
// write that touches the index has keys to compare against.
func openParkTable(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(filepath.Join(t.TempDir(), "park.db"), "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)`,
		`CREATE INDEX t_n ON t ((CAST(n AS TEXT)) COLLATE ` + Collation + `)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for i := 1; i <= 8; i++ {
		if _, err := db.Exec(`INSERT INTO t (id, n) VALUES (?, ?)`, i, i*10); err != nil {
			t.Fatal(fmt.Errorf("insert %d: %w", i, err))
		}
	}
	return db
}
