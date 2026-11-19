package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestAParkedWriteIsStoppedByItsCancel pins what the package promises: an
// UPDATE that moves a key in an index under the collation, an INSERT into
// it, and a DELETE from it each park inside the statement, and a cancel
// while parked, made through Cancel, comes back from the statement as
// context.Canceled. Unparked, the same statement completes (the control that
// the park is not the cause).
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
			db, path := openParkTable(t)
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
			park.Cancel(t, path, cancel)
			park.ReleaseUntil(t, done)
			<-done
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the parked statement returned %v, want context.Canceled", err)
			}
		})
		t.Run(tc.name+", not cancelled", func(t *testing.T) {
			db, _ := openParkTable(t)
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
// write that touches the index has keys to compare against, and answers it
// and its path. Its pool keeps one connection, so the connection a parked
// statement runs on is the one Cancel finds.
func openParkTable(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "park.db")
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
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
	return db, path
}

// TestACancelThatReachesTheDriverLateIsWaitedFor tells the driver of the
// parked UPDATE's cancel late, as a loaded host can: the cancel Cancel makes
// stands at once, before anything lets the statement go, and the driver hears
// of it a second later (toldLate), while Cancel waits. A park that let the
// statement go without waiting for the interrupt let it end with no error, as
// CI's -race job saw (backlog B909); Cancel holds the comparison until the
// interrupt has come, and the statement comes back as the cancellation.
func TestACancelThatReachesTheDriverLateIsWaitedFor(t *testing.T) {
	db, path := openParkTable(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := toldLate{Context: base, told: make(chan struct{})}
	park := Arm(t)
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = db.ExecContext(ctx, `UPDATE t SET n = n + 1 WHERE id = ?`, 3)
	}()
	park.Wait(t)
	park.Cancel(t, path, func() {
		cancel()
		time.AfterFunc(time.Second, func() { close(ctx.told) })
	})
	park.ReleaseUntil(t, done)
	<-done
	told := false
	select {
	case <-ctx.told:
		told = true
	default:
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the parked statement returned %v, want context.Canceled (as it ended, its context's Err was %v, and the driver had been told: %t)",
			err, base.Err(), told)
	}
}

// toldLate is a context whose cancel stands as its base is cancelled, its Err
// context.Canceled from then on, and reaches the driver only once told is
// closed: Done is told. No context of the standard library answers Err before
// its Done closes; this one does on purpose. The driver's goroutine that stops
// a running statement watches Done alone (modernc's interruptOnDone), and the
// statement asks Err only once that goroutine has marked its call stopped, so
// until told closes the driver is as one whose goroutine a loaded host keeps
// off the CPU.
type toldLate struct {
	context.Context
	told chan struct{}
}

func (c toldLate) Done() <-chan struct{} { return c.told }

// TestCancelsWaitEnds pins how Cancel's wait ends but by the interrupt: where
// none comes, it fails the test when the Park's waits give up, rather than
// let the statement go as if one had; once the Park is disarmed it returns,
// failing nothing; and where more than one connection to the database is
// open, so that none is singled out as the statement's, it fails the test
// before it cancels anything.
func TestCancelsWaitEnds(t *testing.T) {
	const update = `UPDATE t SET n = n + 1 WHERE id = ?`
	for _, tc := range []struct {
		name string
		// open opens what else the case needs on the database at path.
		open func(t *testing.T, path string)
		// waiting is Cancel's awaiting hook, given the Park; nil is none.
		waiting func(p *Park) func()
		// fatal is what Cancel fails the test with, given the database's
		// path; nil for nothing.
		fatal func(path string) string
	}{
		{"no interrupt comes", nil, func(p *Park) func() {
			giveUp := make(chan time.Time)
			p.giveUp, p.bound = func() <-chan time.Time { return giveUp }, "at the test's give-up"
			return sync.OnceFunc(func() { close(giveUp) })
		}, func(string) string {
			return "sqlitetest: the cancel did not interrupt the parked statement at the test's give-up"
		}},
		{"the Park is disarmed", nil, func(p *Park) func() { return p.Disarm }, nil},
		{"another connection to the database is open", func(t *testing.T, path string) {
			if err := openWAL(t, path).Ping(); err != nil {
				t.Fatal(err)
			}
		}, nil, func(path string) string {
			return "sqlitetest: Cancel waits for the interrupt of the one connection to " + path +
				" that is open and not interrupted, and 2 are"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, path := openParkTable(t)
			if tc.open != nil {
				tc.open(t, path)
			}
			park := Arm(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = db.ExecContext(context.Background(), update, 3)
			}()
			park.Wait(t)
			if tc.waiting != nil {
				park.awaiting = tc.waiting(park)
			}
			rec := &fatalRecorder{TB: t}
			cancelled := false
			ended := make(chan struct{})
			go func() { // rec's Fatalf ends the goroutine calling it
				defer close(ended)
				park.Cancel(rec, path, func() { cancelled = true })
			}()
			endsWithin(t, ended, "Cancel")
			want := ""
			if tc.fatal != nil {
				want = tc.fatal(path)
			}
			if rec.fatal != want {
				t.Errorf("Cancel failed the test with %q, want %q", rec.fatal, want)
			}
			if tc.open != nil && cancelled {
				t.Error("Cancel cancelled a statement whose connection it could not single out")
			}
			park.Disarm()
			endsWithin(t, done, "the statement the Park let go")
		})
	}
}

// endsWithin fails the test unless ch closes within 10 s.
func endsWithin(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not end within 10s", what)
	}
}

// TestOpeningAConnectionWaitsOnNoOtherConnectionsLock: the connection hook
// drops a connection recorded only where its lock is free at once, so an open
// completes while another connection's lock is held, as a close holds it
// while SQLite checkpoints the file; and the next open after that close drops
// the connection it closed.
func TestOpeningAConnectionWaitsOnNoOtherConnectionsLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "held.db")
	held := openWAL(t, path)
	if err := held.Ping(); err != nil {
		t.Fatal(err)
	}
	conns.mu.Lock()
	recorded := append([]*tracked(nil), conns.byFile[path]...)
	conns.mu.Unlock()
	if len(recorded) != 1 {
		t.Fatalf("%d connections recorded for %s, want 1", len(recorded), path)
	}
	other := openWAL(t, filepath.Join(dir, "other.db"))
	held0 := recorded[0]
	held0.lock.Lock()
	// Registered after both closes, so it runs first: a close takes the lock.
	unlock := sync.OnceFunc(held0.lock.Unlock)
	t.Cleanup(unlock)
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		_ = other.Ping()
	}()
	endsWithin(t, opened, "an open beside another connection's held lock")
	unlock()
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := openWAL(t, filepath.Join(dir, "third.db")).Ping(); err != nil {
		t.Fatal(err)
	}
	conns.mu.Lock()
	n := len(conns.byFile[path])
	conns.mu.Unlock()
	if n != 0 {
		t.Errorf("%d connections recorded for %s once its one was closed, want 0", n, path)
	}
}

// TestThePruneClearsTheSlotsItDrops: an open's prune compacts each file's
// list in place and leaves no connection it dropped in the slots past what it
// keeps. Of two connections to one database, the first open and the second
// closed, an open of another database keeps the first alone, and the list's
// array holds nothing past it. A prune that filtered into the list's own
// start left the closed connection's record there until an append to that
// file's list wrote over it, which no open of another database makes.
func TestThePruneClearsTheSlotsItDrops(t *testing.T) {
	dir := t.TempDir()
	path, other := filepath.Join(dir, "kept.db"), filepath.Join(dir, "other.db")
	// Registered first, so run last, once every connection here has closed:
	// the registry keeps nothing of this test's.
	t.Cleanup(func() {
		conns.mu.Lock()
		defer conns.mu.Unlock()
		delete(conns.byFile, path)
		delete(conns.byFile, other)
	})
	open, closed := openWAL(t, path), openWAL(t, path)
	for _, db := range []*sql.DB{open, closed} {
		if err := db.Ping(); err != nil {
			t.Fatal(err)
		}
	}
	conns.mu.Lock()
	recorded := slices.Clone(conns.byFile[path])
	conns.mu.Unlock()
	if len(recorded) != 2 {
		t.Fatalf("%d connections recorded for %s, want 2", len(recorded), path)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := openWAL(t, other).Ping(); err != nil {
		t.Fatal(err)
	}
	conns.mu.Lock()
	list := conns.byFile[path]
	kept, past := slices.Clone(list), slices.Clone(list[len(list):cap(list)])
	conns.mu.Unlock()
	if len(kept) != 1 || kept[0] != recorded[0] {
		t.Fatalf("the prune kept %d connections for %s, want the open one alone", len(kept), path)
	}
	if len(past) == 0 {
		t.Fatal("the list has no slot past what it keeps: the prune no longer compacts it in place, which this case is written for")
	}
	for i, c := range past {
		if c != nil {
			t.Errorf("slot %d, past the list's length, still holds a connection (the closed one: %t)", len(kept)+i, c == recorded[1])
		}
	}
}

// TestCancelsLookWaitsOnAConnectionAndHoldsUpNoOpen: Cancel's look for the
// statement's connection (liveConns) waits for each connection's lock, as a
// close holds it across SQLite's close, rather than pass a connection over;
// it holds the registry's lock across none of those waits, so a connection
// opened meanwhile opens; and it asks a copy of the registry's list, which
// that open compacts in place as it drops the connection closed before the
// look, so the look counts each connection once: of three to the database,
// the first's lock held and the second closed, two.
func TestCancelsLookWaitsOnAConnectionAndHoldsUpNoOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "looked.db")
	var dbs []*sql.DB
	for range 3 {
		db := openWAL(t, path)
		if err := db.Ping(); err != nil {
			t.Fatal(err)
		}
		dbs = append(dbs, db)
	}
	if err := dbs[1].Close(); err != nil {
		t.Fatal(err)
	}
	conns.mu.Lock()
	recorded := append([]*tracked(nil), conns.byFile[path]...)
	var held *enteredLock
	if len(recorded) == 3 {
		held = &enteredLock{connLock: recorded[0].lock, entered: make(chan struct{})}
		recorded[0].lock = held
	}
	conns.mu.Unlock()
	if held == nil {
		t.Fatalf("%d connections recorded for %s, want 3", len(recorded), path)
	}
	t.Cleanup(func() {
		conns.mu.Lock()
		recorded[0].lock = held.connLock
		conns.mu.Unlock()
	})
	held.connLock.Lock()
	// Registered after the closes, so it runs first: a close takes the lock.
	unlock := sync.OnceFunc(held.connLock.Unlock)
	t.Cleanup(unlock)
	answered := make(chan []*tracked, 1)
	go func() { answered <- liveConns(path) }()
	select {
	case <-held.entered:
	case live := <-answered:
		t.Fatalf("Cancel's look answered %d connections and waited for none whose lock is held", len(live))
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel's look did not ask for the held lock within 10s")
	}
	other := openWAL(t, filepath.Join(dir, "other.db"))
	opened := make(chan struct{})
	go func() {
		defer close(opened)
		_ = other.Ping()
	}()
	endsWithin(t, opened, "an open while Cancel's look waits on a connection's held lock")
	select {
	case live := <-answered:
		t.Fatalf("Cancel's look answered %d connections while one's lock was held", len(live))
	default:
	}
	unlock()
	select {
	case live := <-answered:
		if len(live) != 2 {
			t.Errorf("Cancel's look found %d connections open and not interrupted, want 2", len(live))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Cancel's look did not answer within 10s of the lock's release")
	}
}

// enteredLock is a connection's lock that says when a Lock begins: it closes
// entered, once, then waits for the lock.
type enteredLock struct {
	connLock
	entered chan struct{}
	once    sync.Once
}

func (l *enteredLock) Lock() {
	l.once.Do(func() { close(l.entered) })
	l.connLock.Lock()
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
		db, _ := openParkTable(t)
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
