package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

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

// TestArmUntilGivesUpAtItsInstant pins what a serve test arms a Park with:
// a wait gives up at the instant it was given, not Arm's 10 s after the
// wait began, and says when that was.
func TestArmUntilGivesUpAtItsInstant(t *testing.T) {
	rec := &fatalRecorder{TB: t}
	at := time.Now().Add(200 * time.Millisecond)
	p := ArmUntil(rec, at)
	defer p.Disarm()
	ended := make(chan struct{})
	var gaveUp time.Time
	go func() {
		defer close(ended)
		defer func() { gaveUp = time.Now() }() // runs as rec's Fatalf ends the goroutine
		p.Wait(rec)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a Park armed to give up in 200ms was still waiting 5s later")
	}
	// Against the instant, never against a clock read after it was set: a
	// pause between the two would let a wait that gave up on time read as
	// early (CodeRabbit on #1099).
	if gaveUp.Before(at) {
		t.Errorf("the wait gave up %v before the instant it was given", at.Sub(gaveUp))
	}
	if want := "sqlitetest: no statement compared a key by " + at.Format("15:04:05.000"); rec.fatal != want {
		t.Errorf("the wait failed with %q, want %q", rec.fatal, want)
	}
}

// TestArmUntilWithNoDeadlineNeverGivesUp: a test binary run with no
// -timeout has no deadline to give up at, and a Park armed for one waits
// as the binary would.
func TestArmUntilWithNoDeadlineNeverGivesUp(t *testing.T) {
	p := ArmUntil(t, time.Time{})
	defer p.Disarm()
	if p.giveUp() != nil {
		t.Error("a Park armed with no instant gives up")
	}
}

// TestWaitUnlessAnswersTheWorksEnd: WaitUnless answers true for a parked
// statement, and false, without failing, when the work that would have run
// it ended first, so the caller can report that end.
func TestWaitUnlessAnswersTheWorksEnd(t *testing.T) {
	t.Run("the work ended first", func(t *testing.T) {
		rec := &fatalRecorder{TB: t}
		p := ArmUntil(rec, time.Now().Add(2*time.Second))
		defer p.Disarm()
		stopped := make(chan struct{})
		close(stopped)
		parked := true
		ended := make(chan struct{})
		go func() { // rec's Fatalf ends the goroutine calling it
			defer close(ended)
			parked = p.WaitUnless(rec, stopped)
		}()
		<-ended
		if rec.fatal != "" {
			t.Fatalf("WaitUnless failed the test when the work ended first: %s", rec.fatal)
		}
		if parked {
			t.Error("WaitUnless answered true with no statement parked")
		}
	})
	t.Run("a statement parked", func(t *testing.T) {
		db := openParkTable(t)
		p := ArmUntil(t, time.Now().Add(time.Minute))
		done := make(chan struct{})
		var err error
		go func() {
			defer close(done)
			_, err = db.ExecContext(context.Background(), `UPDATE t SET n = n + 1 WHERE id = ?`, 3)
		}()
		if !p.WaitUnless(t, done) {
			t.Fatal("WaitUnless answered false for a statement that parks")
		}
		p.ReleaseUntil(t, done)
		<-done
		if err != nil {
			t.Fatalf("the statement failed with nothing cancelled: %v", err)
		}
	})
}

// TestARollbackWatchCountsTheRollbacksOnItsFileAlone pins what a
// RollbackWatch counts: a transaction rolled back on a connection to its
// file, whether by a Rollback or by database/sql once the transaction's
// context is done, and whether the file was opened by a DSN or by its path;
// never a commit, and never a rollback on another file.
func TestARollbackWatchCountsTheRollbacksOnItsFileAlone(t *testing.T) {
	dir := t.TempDir()
	watched, other := filepath.Join(dir, "watched.db"), filepath.Join(dir, "other.db")
	byDSN := openWAL(t, dsn.File(watched, "_pragma=journal_mode(WAL)"))
	byPath := openWAL(t, watched)
	elsewhere := openWAL(t, dsn.File(other, "_pragma=journal_mode(WAL)"))
	for _, db := range []*sql.DB{byDSN, elsewhere} {
		if _, err := db.Exec(`CREATE TABLE t (n INTEGER)`); err != nil {
			t.Fatal(err)
		}
	}
	w := WatchRollbacks(t, watched)
	write := func(db *sql.DB, ctx context.Context) *sql.Tx {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO t (n) VALUES (1)`); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	steps := []struct {
		name string
		end  func()
		want int
	}{
		{"a rollback by DSN", func() { _ = write(byDSN, context.Background()).Rollback() }, 1},
		{"a commit", func() { _ = write(byDSN, context.Background()).Commit() }, 1},
		{"a rollback on another file", func() { _ = write(elsewhere, context.Background()).Rollback() }, 1},
		{"a rollback by path", func() { _ = write(byPath, context.Background()).Rollback() }, 2},
	}
	for _, s := range steps {
		s.end()
		if got := w.Seen(); got != s.want {
			t.Fatalf("after %s the watch saw %d rollbacks, want %d", s.name, got, s.want)
		}
	}

	// database/sql's own rollback, once the transaction's context is done,
	// on a goroutine of its own: WaitPast sees it, and the transaction then
	// answers ErrTxDone.
	ctx, cancel := context.WithCancel(context.Background())
	tx := write(byDSN, ctx)
	cancel()
	if !w.WaitPast(t, 2, nil) {
		t.Fatal("WaitPast answered false with nothing stopped")
	}
	if _, err := tx.ExecContext(context.Background(), `SELECT 1`); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("the transaction whose rollback the watch saw answers %v, want sql.ErrTxDone", err)
	}
	if got := w.Seen(); got != 3 {
		t.Errorf("the watch saw %d rollbacks, want 3", got)
	}
}

// TestWaitPastAnswersTheWorksEndAndGivesUpAtItsInstant: WaitPast answers
// false, without failing, when the work it waits on ends first, and a watch
// armed with WatchRollbacksUntil gives up at its instant and says so.
func TestWaitPastAnswersTheWorksEndAndGivesUpAtItsInstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quiet.db")
	t.Run("the work ended first", func(t *testing.T) {
		w := WatchRollbacks(t, path)
		stopped := make(chan struct{})
		close(stopped)
		if w.WaitPast(t, 0, stopped) {
			t.Error("WaitPast answered true with nothing rolled back")
		}
	})
	t.Run("the wait gives up", func(t *testing.T) {
		rec := &fatalRecorder{TB: t}
		at := time.Now().Add(200 * time.Millisecond)
		w := WatchRollbacksUntil(rec, path, at)
		ended := make(chan struct{})
		go func() { // rec's Fatalf ends the goroutine calling it
			defer close(ended)
			w.WaitPast(rec, 0, nil)
		}()
		select {
		case <-ended:
		case <-time.After(5 * time.Second):
			t.Fatal("a watch armed to give up in 200ms was still waiting 5s later")
		}
		want := fmt.Sprintf("sqlitetest: no transaction rolled back on %s by %s (0 seen, waiting past 0)", path, at.Format("15:04:05.000"))
		if rec.fatal != want {
			t.Errorf("the wait failed with %q, want %q", rec.fatal, want)
		}
	})
}

// openWAL opens a WAL database by name, a DSN or a path, closed when the
// test ends.
func openWAL(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fatalRecorder is a testing.TB whose Fatalf records the message and ends
// the goroutine calling it, as FailNow would, without failing the test that
// holds it.
type fatalRecorder struct {
	testing.TB
	fatal string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
