package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"

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
// context is done, and whether the file was opened by a DSN, by its path or
// through the watch's Connector, whose connections count with a rollback
// hook of their own; never a commit, and never a rollback on another file.
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
	through := openThrough(t, w, dsn.File(watched, "_pragma=journal_mode(WAL)"))
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
		{"a rollback through the watch's Connector", func() { _ = write(through, context.Background()).Rollback() }, 3},
		{"a commit through the watch's Connector", func() { _ = write(through, context.Background()).Commit() }, 3},
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
	if !w.WaitPast(t, 3, nil) {
		t.Fatal("WaitPast answered false with nothing stopped")
	}
	if _, err := tx.ExecContext(context.Background(), `SELECT 1`); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("the transaction whose rollback the watch saw answers %v, want sql.ErrTxDone", err)
	}
	if got := w.Seen(); got != 4 {
		t.Errorf("the watch saw %d rollbacks, want 4", got)
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

// heldWrite opens a WAL database in a folder of its own, arms a watch on it,
// on connections the watch's Connector makes (openThrough), and begins a
// write transaction on ctx that inserts a row, open; the test's end rolls it
// back, if nothing has, before the database closes. The table's column is
// its primary key, so a second insert of the same value conflicts.
func heldWrite(t *testing.T, ctx context.Context) (*sql.DB, *RollbackWatch, *sql.Tx) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "held.db")
	w := WatchRollbacks(t, path)
	db := openThrough(t, w, dsn.File(path, "_pragma=journal_mode(WAL)"))
	if _, err := db.Exec(`CREATE TABLE t (n INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(ctx, `INSERT INTO t (n) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	return db, w, tx
}

// TestErrTxDoneAnswersBeforeTheDriverRollsBack pins why a count of the
// file's rollbacks and sql.ErrTxDone cannot tell a wait that the held
// transaction's rollback has run (backlog B869): database/sql's Tx.rollback
// marks the transaction done before it calls the driver's Rollback, so with
// that ROLLBACK held at its boundary and a reader's transaction rolled back on
// another connection, the count has moved and the transaction answers
// sql.ErrTxDone, while the transaction's own rollback has not run.
func TestErrTxDoneAnswersBeforeTheDriverRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, w, tx := heldWrite(t, ctx)
	hold := w.HoldRollbackOf(t, tx)
	rollback := w.RollbackOf(t, tx)
	reader, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	seen := w.Seen()
	cancel()
	hold.Reached(t)
	if err := readAndRollBack(reader); err != nil {
		t.Fatal(err)
	}
	if !w.WaitPast(t, seen, nil) {
		t.Fatal("WaitPast answered false with nothing stopped")
	}
	if _, err := tx.ExecContext(context.Background(), `SELECT 1`); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("the transaction whose ROLLBACK is held answers %v, want sql.ErrTxDone", err)
	}
	select {
	case <-rollback.ended:
		t.Fatal("the transaction's rollback came to an end while its ROLLBACK was held")
	default:
	}
	hold.Release()
	rollback.Wait(t)
	if got := w.Seen(); got != seen+2 {
		t.Errorf("the watch saw %d rollbacks past %d, want the reader's and the transaction's own", got-seen, seen)
	}
}

// TestARollbackOfIsTheTransactionsOwnAtTheDriver: RollbackOf, taken before
// the stop, answers once the transaction's own rollback has run at the
// driver, and never while that rollback is held, at its boundary once a
// reader's transaction has rolled back on another connection to the file, or
// once SQLite has rolled it back inside the driver's ROLLBACK, before the
// driver's Rollback has returned (WaitProof's schedule, which lets each hold
// go only once the wait has answered that it waits on). Nothing the
// transaction wrote lands.
func TestARollbackOfIsTheTransactionsOwnAtTheDriver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, w, tx := heldWrite(t, ctx)
	proof := w.WaitProofOf(t, tx)
	rollback := w.RollbackOf(t, tx)
	cancel()
	rollback.Wait(t)
	proof.Returned(t)
	if _, err := tx.ExecContext(context.Background(), `SELECT 1`); !errors.Is(err, sql.ErrTxDone) {
		t.Errorf("the transaction answers %v once its rollback ran, want sql.ErrTxDone", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the table holds %d rows (%v) after the rollback, want none", n, err)
	}
}

// TestARollbackThatRanBeforeItsWaitIsKept: a rollback that has run before its
// wait begins is answered at once, never missed, and first where the wait's
// stop is ready too: a hundred waits, each on a stop of its own closed before
// it, so that a wait that left the choice to a select (which picks at random
// among the cases ready) fails within one run. The test synchronizes on the
// rollback's one publication of its end, as its waits do.
func TestARollbackThatRanBeforeItsWaitIsKept(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, w, tx := heldWrite(t, ctx)
	rollback := w.RollbackOf(t, tx)
	cancel()
	select {
	case <-rollback.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the transaction's rollback never came to an end")
	}
	if !rollback.hasRun {
		t.Fatalf("the transaction's rollback came to an end without running: %v%s", rollback.err, rollback.why)
	}
	for i := 1; i <= 100; i++ {
		stopped := make(chan struct{})
		close(stopped)
		if !rollback.WaitUnless(t, stopped) {
			t.Fatalf("wait %d of 100: a rollback that had run was answered as its wait's stop", i)
		}
	}
	rollback.Wait(t)
}

// TestATransactionThatCommitsCanNoLongerRollBack: the rollback of a
// transaction that commits instead is lost, and a wait for it fails at once,
// saying so; a later transaction's rollback on the same connection runs none
// of it.
func TestATransactionThatCommitsCanNoLongerRollBack(t *testing.T) {
	db, w, tx := heldWrite(t, context.Background())
	db.SetMaxOpenConns(1) // the later transaction runs on the same connection
	rollback := w.RollbackOf(t, tx)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	later, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := later.Rollback(); err != nil {
		t.Fatal(err)
	}
	rec := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() { // rec's Fatalf ends the goroutine calling it
		defer close(ended)
		rollback.Wait(rec)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait for a rollback its transaction committed past was still waiting 5s later")
	}
	if want := "its transaction committed first"; !strings.Contains(rec.fatal, want) {
		t.Errorf("the wait failed with %q, want it to say %q", rec.fatal, want)
	}
}

// TestARollbackOfRefusesATransactionOnAnotherFile: a transaction on another
// file than the watch's, on a connection the watch's Connector did not make,
// is refused, and the binding's token is not left pending.
func TestARollbackOfRefusesATransactionOnAnotherFile(t *testing.T) {
	_, w, _ := heldWrite(t, context.Background())
	other := openWAL(t, filepath.Join(t.TempDir(), "other.db"))
	tx, err := other.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rec := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		w.RollbackOf(rec, tx)
	}()
	<-ended
	if want := "the watch on " + w.path + " did not make"; !strings.Contains(rec.fatal, want) {
		t.Errorf("RollbackOf failed with %q, want it to say %q", rec.fatal, want)
	}
	if n := pendingBindings(); n != 0 {
		t.Errorf("%d bindings were left pending", n)
	}
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

// openThrough opens the database by name, a DSN or a path, on connections
// the watch's Connector makes over the registered driver's, closed when the
// test ends.
func openThrough(t *testing.T, w *RollbackWatch, name string) *sql.DB {
	t.Helper()
	base, err := sqlite.NewConnector(name)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(w.Connector(base))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pendingBindings counts the bindings no connection has taken and no bindTo
// has given up yet.
func pendingBindings() int {
	pending.Lock()
	defer pending.Unlock()
	return len(pending.binds)
}

// fatalRecorder is a testing.TB whose Fatal and Fatalf record the message
// and end the goroutine calling it, as FailNow would, and whose Errorf
// records the message, without failing the test that holds it, so that code
// run under it on a goroutine of its own never calls the test's own Fatal
// there: the test's goroutine reads what it recorded.
type fatalRecorder struct {
	testing.TB
	fatal  string
	errors []string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatal(args ...any) {
	r.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func (r *fatalRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}
