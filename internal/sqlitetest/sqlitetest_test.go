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
