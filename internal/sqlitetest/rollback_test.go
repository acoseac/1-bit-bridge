package sqlitetest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestTheConnectorsConnectionsAreTheDriversToDatabaseSQL pins what a database
// opened through a watch's Connector keeps of the driver (backlog B869's
// third round): each connection implements every interface database/sql asks
// of a connection that the driver's implements, and none it does not, so
// database/sql converts arguments, resets sessions and keeps connections as
// it would without the wrapper (a connection a transaction's context rolled
// back is kept, not discarded); and it is the registered driver's, carrying
// the collation registered on it.
func TestTheConnectorsConnectionsAreTheDriversToDatabaseSQL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kept.db")
	w := WatchRollbacks(t, path)
	base, err := sqlite.NewConnector(dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := base.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	wrapped, err := w.Connector(base).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer wrapped.Close()
	for _, iface := range []reflect.Type{
		reflect.TypeFor[driver.ConnBeginTx](),
		reflect.TypeFor[driver.ConnPrepareContext](),
		reflect.TypeFor[driver.ExecerContext](),
		reflect.TypeFor[driver.QueryerContext](),
		reflect.TypeFor[driver.Pinger](),
		reflect.TypeFor[driver.SessionResetter](),
		reflect.TypeFor[driver.Validator](),
		reflect.TypeFor[driver.NamedValueChecker](),
	} {
		if got, want := reflect.TypeOf(wrapped).Implements(iface), reflect.TypeOf(plain).Implements(iface); got != want {
			t.Errorf("%v: the watch's connection implements it %v, the driver's %v", iface, got, want)
		}
	}

	db := openThrough(t, w, dsn.File(path, "_pragma=journal_mode(WAL)"))
	db.SetMaxOpenConns(1) // a connection database/sql discards is opened again
	if _, err := db.Exec(`CREATE TABLE t (n TEXT)`); err != nil {
		t.Fatal(err)
	}
	made := w.conns.Load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	rollback := w.RollbackOf(t, tx)
	cancel()
	rollback.Wait(t)
	if _, err := db.Exec(`CREATE INDEX t_n ON t (n COLLATE ` + Collation + `)`); err != nil {
		t.Fatalf("an index under the collation registered on the driver: %v", err)
	}
	if got := w.conns.Load(); got != made {
		t.Errorf("the watch's Connector made %d connections more once a transaction's context rolled it back: database/sql discarded the connection", got-made)
	}
}

// TestARollbackHasRunOnlyOnceTheDriversRollbackReturns: SQLite's rollback
// hook fires inside the driver's ROLLBACK, before SQLite has ended the
// statement and the driver has finalized it and returned (backlog B869's
// third round), so a rollback held there, between SQLite's callback and the
// driver's return, has not run: it comes to an end only once the hold lets
// it go and the driver's Rollback returns.
func TestARollbackHasRunOnlyOnceTheDriversRollbackReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, w, tx := heldWrite(t, ctx)
	hold := w.HoldReturnOf(t, tx)
	rollback := w.RollbackOf(t, tx)
	cancel()
	hold.Reached(t)
	select {
	case <-rollback.inSQLite:
	default:
		t.Fatal("the hold held the rollback before SQLite had rolled the transaction back")
	}
	select {
	case <-rollback.ended:
		t.Fatal("the rollback came to an end inside the driver's ROLLBACK, before its Rollback returned")
	default:
	}
	hold.Release()
	rollback.Wait(t)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 0 {
		t.Errorf("the table holds %d rows (%v) after the rollback, want none", n, err)
	}
}

// TestARollbackComesToAnEndInOnePublication (backlog B869's fourth round):
// what a rollback came to is published once, after the driver's Rollback has
// returned: written, and then ended closed, its one completion signal. Held
// just before that publication, with a stop ready, nothing a test or a wait
// synchronizes on says that the rollback has run, and the wait answers its
// stop; once ended is closed, the wait answers that the rollback ran, a
// hundred times over with the stop ready. Until this round a success closed
// a second signal, ran, before ended, and a test that synchronized on ran,
// as TestARollbackThatRanBeforeItsWaitIsKept did, met a wait that answered
// its stop for a rollback that had succeeded, held between the two closes in
// every run (the record's red at 49fab8d5).
func TestARollbackComesToAnEndInOnePublication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, w, tx := heldWrite(t, ctx)
	rollback := w.RollbackOf(t, tx)
	held, release := make(chan struct{}), make(chan struct{})
	var letGo sync.Once
	t.Cleanup(func() { letGo.Do(func() { close(release) }) })
	w.beforeEnded = func(r *Rollback) {
		if r == rollback {
			close(held)
			<-release
		}
	}
	cancel()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the transaction's rollback never reached its publication")
	}
	stopped := make(chan struct{})
	close(stopped)
	select {
	case <-rollback.ended:
		if !rollback.WaitUnless(t, stopped) {
			t.Fatal("a wait answered its stop for a rollback whose success was signalled")
		}
	default:
		if rollback.WaitUnless(t, stopped) {
			t.Fatal("a wait answered that the rollback had run before its end was published")
		}
	}
	letGo.Do(func() { close(release) })
	select {
	case <-rollback.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the rollback's end was never published")
	}
	for i := 1; i <= 100; i++ {
		if !rollback.WaitUnless(t, stopped) {
			t.Fatalf("wait %d of 100: a rollback published as run was answered as its wait's stop", i)
		}
	}
}

// TestARollbackWhoseDriverReturnsAnErrorFailsItsWait: the driver's Rollback
// returning is not its succeeding. A Rollback that returns an error (here its
// ROLLBACK finds no transaction, one a COMMIT the driver's Rollback never saw
// ended) fails the wait at once with the driver's error, and the rollback is
// never answered as run.
func TestARollbackWhoseDriverReturnsAnErrorFailsItsWait(t *testing.T) {
	_, w, tx := heldWrite(t, context.Background())
	rollback := w.RollbackOf(t, tx)
	if _, err := tx.ExecContext(context.Background(), `COMMIT`); err != nil {
		t.Fatal(err)
	}
	err := tx.Rollback()
	if err == nil {
		t.Fatal("the driver's Rollback of a transaction a COMMIT ended returned no error")
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
		t.Fatal("the wait for a rollback whose driver returned an error was still waiting 5s later")
	}
	if want := "returned an error: " + err.Error(); !strings.Contains(rec.fatal, want) {
		t.Errorf("the wait failed with %q, want it to say %q", rec.fatal, want)
	}
	<-rollback.ended // published, as the wait found it
	if rollback.hasRun {
		t.Error("a rollback whose driver returned an error was published as run")
	}
}

// TestRawTransactionSQLIsOutsideTheContract pins the limit of what a watch
// tracks (backlog B869's fourth round): an expectation is bound to a *sql.Tx
// and ends with that transaction's Commit or Rollback, an error's rollback of
// it, or its connection's close, and transaction-control SQL run directly on
// a watched connection is outside that. A raw COMMIT, run through the
// transaction, commits its write and ends nothing: the rollback expected of
// it stays bound, not begun and not ended, until the transaction's own
// Commit, which the driver then refuses, ends it. A raw ROLLBACK reaches
// SQLite's rollback hook outside the driver's Rollback, which takes it for an
// error's rollback. Nothing refuses either statement.
func TestRawTransactionSQLIsOutsideTheContract(t *testing.T) {
	t.Run("a raw COMMIT leaves the expectation bound", func(t *testing.T) {
		db, w, tx := heldWrite(t, context.Background())
		rollback := w.RollbackOf(t, tx)
		if _, err := tx.ExecContext(context.Background(), `COMMIT`); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the table holds %d rows (%v) after the raw COMMIT, want the transaction's one", n, err)
		}
		for _, c := range []struct {
			what string
			ch   chan struct{}
		}{{"began", rollback.began}, {"came to an end", rollback.ended}} {
			select {
			case <-c.ch:
				t.Fatalf("the rollback expected of the transaction %s at a raw COMMIT", c.what)
			default:
			}
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("the transaction's own Commit, after a raw COMMIT, returned no error")
		}
		select {
		case <-rollback.ended:
		default:
			t.Fatal("the transaction's own Commit did not end the rollback expected of it")
		}
		if want := "its transaction's COMMIT failed"; rollback.hasRun || !strings.Contains(rollback.why, want) {
			t.Errorf("the rollback ended as run %v, why %q; want it lost, saying %q", rollback.hasRun, rollback.why, want)
		}
	})
	t.Run("a raw ROLLBACK is taken for an error's", func(t *testing.T) {
		_, w, tx := heldWrite(t, context.Background())
		rollback := w.RollbackOf(t, tx)
		if _, err := tx.ExecContext(context.Background(), `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
		select {
		case <-rollback.ended:
		default:
			t.Fatal("a raw ROLLBACK, which reaches SQLite's rollback hook, left the expectation bound")
		}
		if want := "its transaction rolled back by an error before its ROLLBACK"; rollback.hasRun || rollback.why != want {
			t.Errorf("the rollback ended as run %v, why %q; want it lost, saying %q", rollback.hasRun, rollback.why, want)
		}
	})
}

// TestAnErrorThatRollsTheTransactionBackLosesItsRollbackAtOnce: a statement
// error that rolls the transaction back inside SQLite (an INSERT OR ROLLBACK
// that conflicts) ends it before any ROLLBACK, so the rollback expected of it
// can no longer come at the driver: that and its holds are lost as the
// statement returns, before database/sql's ROLLBACK of it and before the
// connection begins another transaction, and the wait fails at once, saying
// so.
func TestAnErrorThatRollsTheTransactionBackLosesItsRollbackAtOnce(t *testing.T) {
	_, w, tx := heldWrite(t, context.Background())
	rollback := w.RollbackOf(t, tx)
	holds := []*RollbackHold{w.HoldRollbackOf(t, tx), w.HoldReturnOf(t, tx)}
	if _, err := tx.ExecContext(context.Background(), `INSERT OR ROLLBACK INTO t (n) VALUES (1)`); err == nil {
		t.Fatal("an INSERT OR ROLLBACK of a key the transaction holds raised no error")
	}
	select {
	case <-rollback.ended:
	default:
		t.Fatal("the rollback of a transaction an error rolled back was still awaited once the statement returned")
	}
	for _, h := range holds {
		select {
		case <-h.gone:
		default:
			t.Errorf("the hold %s on a transaction an error rolled back was still armed once the statement returned", h.at)
		}
	}
	rec := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() { // rec's Fatalf ends the goroutine calling it
		defer close(ended)
		rollback.Wait(rec)
	}()
	<-ended
	if want := "its transaction rolled back by an error before its ROLLBACK"; !strings.Contains(rec.fatal, want) {
		t.Errorf("the wait failed with %q, want it to say %q", rec.fatal, want)
	}
}

// TestAConnectionAnErrorRolledBackCompletesNoOldRollbackWhenReused: once an
// error has rolled a transaction back, nothing its connection does next
// completes the rollback that was expected of it or stops at its holds: not
// database/sql's ROLLBACK of it, which finds no transaction, not an
// autocommit statement an error rolls back before any BEGIN, and not a later
// transaction's rollback, each on the same connection. Until B869's third
// round the failed ROLLBACK left the connection marked as rolling back, and
// the next rollback there, an autocommit statement's before any BEGIN,
// answered the old wait.
func TestAConnectionAnErrorRolledBackCompletesNoOldRollbackWhenReused(t *testing.T) {
	db, w, tx := heldWrite(t, context.Background())
	db.SetMaxOpenConns(1) // every step below runs on the transaction's connection
	rollback := w.RollbackOf(t, tx)
	w.HoldRollbackOf(t, tx)
	w.HoldReturnOf(t, tx)
	if _, err := tx.ExecContext(context.Background(), `INSERT OR ROLLBACK INTO t (n) VALUES (1)`); err == nil {
		t.Fatal("an INSERT OR ROLLBACK of a key the transaction holds raised no error")
	}
	steps := []struct {
		name  string
		run   func() error
		fails bool
	}{
		{"database/sql's ROLLBACK of the transaction", tx.Rollback, true},
		{"an autocommit INSERT OR ROLLBACK that conflicts", func() error {
			_, err := db.Exec(`INSERT OR ROLLBACK INTO t (n) VALUES (2)`)
			return err
		}, true},
		{"a later transaction's rollback", func() error {
			later, err := db.Begin()
			if err != nil {
				return err
			}
			if _, err := later.Exec(`INSERT INTO t (n) VALUES (3)`); err != nil {
				return err
			}
			return later.Rollback()
		}, false},
	}
	for i, s := range steps {
		if i == 1 {
			// The row the autocommit INSERT conflicts with.
			if _, err := db.Exec(`INSERT INTO t (n) VALUES (2)`); err != nil {
				t.Fatal(err)
			}
		}
		errc := make(chan error, 1)
		go func() { errc <- s.run() }()
		select {
		case err := <-errc:
			if (err != nil) != s.fails {
				t.Fatalf("%s returned %v", s.name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was still held 5s later, by a hold on the transaction an error rolled back", s.name)
		}
	}
	select {
	case <-rollback.ended:
		if rollback.hasRun {
			t.Fatal("a rollback on the reused connection answered the rollback of the transaction an error rolled back")
		}
	default:
		t.Fatal("the rollback of the transaction an error rolled back was never published as ended")
	}
	rec := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() { // rec's Fatalf ends the goroutine calling it
		defer close(ended)
		rollback.Wait(rec)
	}()
	<-ended
	if want := "its transaction rolled back by an error before its ROLLBACK"; !strings.Contains(rec.fatal, want) {
		t.Errorf("the wait failed with %q, want it to say %q", rec.fatal, want)
	}
}

// TestASavepointsRollbackIsNotTheTransactions: a savepoint's ROLLBACK TO rolls
// back part of the transaction and leaves it open. SQLite fires no rollback
// hook for it (the watch's count stays), so it neither reaches a hold on the
// transaction's rollback nor ends the rollback expected of it, nor is it taken
// for an error's rollback; the transaction's own rollback, after it, passes
// both holds and runs.
func TestASavepointsRollbackIsNotTheTransactions(t *testing.T) {
	_, w, tx := heldWrite(t, context.Background())
	rollback := w.RollbackOf(t, tx)
	atBoundary, inSQLite := w.HoldRollbackOf(t, tx), w.HoldReturnOf(t, tx)
	seen := w.Seen()
	for _, stmt := range []string{`SAVEPOINT s`, `INSERT INTO t (n) VALUES (2)`, `ROLLBACK TO s`, `RELEASE s`} {
		if _, err := tx.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if got := w.Seen(); got != seen {
		t.Errorf("the watch counted %d rollbacks for a savepoint's ROLLBACK TO", got-seen)
	}
	for _, c := range []struct {
		what string
		ch   chan struct{}
	}{
		{"the boundary hold was reached", atBoundary.reached},
		{"the boundary hold was lost", atBoundary.gone},
		{"the return hold was reached", inSQLite.reached},
		{"the return hold was lost", inSQLite.gone},
		{"the rollback began", rollback.began},
		{"the rollback came to an end", rollback.ended},
	} {
		select {
		case <-c.ch:
			t.Errorf("%s at a savepoint's ROLLBACK TO", c.what)
		default:
		}
	}
	errc := make(chan error, 1)
	go func() { errc <- tx.Rollback() }()
	atBoundary.Reached(t)
	atBoundary.Release()
	inSQLite.Reached(t)
	inSQLite.Release()
	rollback.Wait(t)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

// TestAConnectionClosedBeforeItsRollbackFailsTheWaitAtOnce: database/sql
// calls no Rollback on a connection it closes, so the rollback expected of
// the transaction a connection of the watch's runs can no longer come once
// the connection closes, and its wait fails at once, naming the connection,
// where it would otherwise wait out the watch's bound. Driven at the driver,
// with the expectation bound as RollbackOf's binding binds it: database/sql
// never closes a connection under its transaction.
func TestAConnectionClosedBeforeItsRollbackFailsTheWaitAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closed.db")
	w := WatchRollbacks(t, path)
	base, err := sqlite.NewConnector(dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	dc, err := w.Connector(base).Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := dc.(*conn)
	ctx := context.Background()
	if _, err := c.ExecContext(ctx, `CREATE TABLE t (n INTEGER)`, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.BeginTx(ctx, driver.TxOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, `INSERT INTO t (n) VALUES (1)`, nil); err != nil {
		t.Fatal(err)
	}
	rollback := newRollback(w)
	c.mu.Lock()
	c.rollbacks = append(c.rollbacks, rollback)
	c.mu.Unlock()
	if err := c.Close(); err != nil {
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
		t.Fatal("the wait for a rollback whose connection closed was still waiting 5s later")
	}
	if want := fmt.Sprintf("the watch's connection %d, closed before the driver rolled it back", c.id); !strings.Contains(rec.fatal, want) {
		t.Errorf("the wait failed with %q, want it to say %q", rec.fatal, want)
	}
}

// TestABindingAsksTheContextTheTransactionBeganOn (backlog B869's third
// round): database/sql hands the driver no context of a transaction's but
// the one it began on, which a connection of the watch's keeps from its
// BeginTx and a binding asks as it binds, where it ran on
// context.Background until then. A binding that lands leaves no token
// pending; a cancel of that context made during the binding refuses it,
// saying so, and leaves nothing bound to the connection and no token
// pending.
func TestABindingAsksTheContextTheTransactionBeganOn(t *testing.T) {
	t.Run("a binding that lands", func(t *testing.T) {
		_, w, tx := heldWrite(t, context.Background())
		w.RollbackOf(t, tx)
		if n := pendingBindings(); n != 0 {
			t.Errorf("%d bindings were left pending", n)
		}
	})
	t.Run("a cancel during the binding", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, w, tx := heldWrite(t, ctx)
		var bound *conn
		w.beforeBind = func(c *conn) {
			bound = c
			cancel()
		}
		rec := &fatalRecorder{TB: t}
		ended := make(chan struct{})
		go func() { // rec's Fatalf ends the goroutine calling it
			defer close(ended)
			w.RollbackOf(rec, tx)
		}()
		<-ended
		w.beforeBind = nil
		if want := "the context the transaction began on is done (context canceled)"; !strings.Contains(rec.fatal, want) {
			t.Errorf("RollbackOf failed with %q, want it to say %q", rec.fatal, want)
		}
		if bound == nil {
			t.Fatal("no connection of the watch's took the binding")
		}
		bound.mu.Lock()
		n := len(bound.rollbacks)
		bound.mu.Unlock()
		if n != 0 {
			t.Errorf("a refused binding left %d rollbacks bound to the connection", n)
		}
		if n := pendingBindings(); n != 0 {
			t.Errorf("%d bindings were left pending", n)
		}
	})
}

// TestTheProofFailsAWaitThatTakesAReadersRollbackForItsOwn: the wait the
// three waits ran before B869's second round, a count past the one read
// before the stop followed by sql.ErrTxDone, returns under WaitProof's
// schedule once the reader's transaction has rolled back, with the
// transaction's ROLLBACK still held at the driver's boundary, and Returned
// fails it, saying so, in every run: the schedule lets the hold go only on
// the wait's answer, which this wait never gives.
func TestTheProofFailsAWaitThatTakesAReadersRollbackForItsOwn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, w, tx := heldWrite(t, ctx)
	proof := w.WaitProofOf(t, tx)
	seen := w.Seen()
	cancel()
	for {
		w.WaitPast(t, seen, nil)
		seen = w.Seen()
		if _, err := tx.ExecContext(context.Background(), `SELECT 1`); errors.Is(err, sql.ErrTxDone) {
			break
		}
	}
	wantEarly(t, proof, holdingReader)
}

// TestTheProofFailsAWaitThatAnswersOnSQLitesRollback: a wait that ends where
// B869's second round ended it, once SQLite has rolled the transaction back
// inside the driver's ROLLBACK (its rollback hook), answering the schedule's
// asks as this package's waits do, returns under WaitProof's schedule while
// that ROLLBACK is still held inside the driver, before its Rollback has
// returned, and Returned fails it, saying so, in every run.
func TestTheProofFailsAWaitThatAnswersOnSQLitesRollback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, w, tx := heldWrite(t, ctx)
	proof := w.WaitProofOf(t, tx)
	rollback := w.RollbackOf(t, tx)
	cancel()
	giveUp := w.giveUp()
	for answered := false; !answered; {
		asked, ask := w.proofAsks()
		select {
		case <-rollback.inSQLite:
			answered = true
			continue
		default:
		}
		w.stillWaiting(asked)
		select {
		case <-rollback.inSQLite:
			answered = true
		case <-ask:
		case <-giveUp:
			t.Fatal("SQLite never rolled the transaction back")
		}
	}
	wantEarly(t, proof, holdingLetGo, holdingInSQLite)
}

// wantEarly calls the proof's Returned for a wait that returned too early,
// and requires that it fail the wait, once, saying the rollback was doing
// one of what.
func wantEarly(t *testing.T, proof *WaitProof, what ...string) {
	t.Helper()
	rec := &fatalRecorder{TB: t}
	ended := make(chan struct{})
	go func() { // rec's Fatalf ends the goroutine calling it
		defer close(ended)
		proof.Returned(rec)
	}()
	<-ended
	if rec.fatal != "" {
		t.Fatalf("the proof failed on its own account: %s", rec.fatal)
	}
	for _, w := range what {
		if len(rec.errors) == 1 && rec.errors[0] == "the wait returned while "+w {
			return
		}
	}
	t.Errorf("Returned reported %q, want one failure saying the wait returned while one of %q", rec.errors, what)
}
