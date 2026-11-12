package sqlitetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// WaitProof is the schedule that tells a wait for one transaction's own
// rollback at the driver from one that answers too early (backlog B869), run
// on a goroutine of its own (WaitProofOf) while the test runs the wait. It
// holds that rollback twice: at its boundary, the driver's Rollback called
// and its ROLLBACK not yet run, while a reader's transaction rolls back on
// another connection to the file; and once SQLite has rolled the transaction
// back inside that ROLLBACK, before the statement ends and the driver's
// Rollback returns. It lets each hold go only once the wait under proof has
// decided again, after what came before, to wait on. A wait that takes the
// reader's rollback, or SQLite's, for the driver's return returns instead
// while the rollback is still held, and Returned fails it.
//
// No time decides anything. The schedule asks the wait to decide again and
// waits for its answer or its return (decides), and every wait of this
// package answers (proofAsks, stillWaiting): a Rollback's, and WaitPast,
// which the waits before B869's second round counted on. A wait asked after
// an event that ends it has seen that event when it decides: the ask is
// written after the event, and the wait reads the ask before what it waits on.
type WaitProof struct {
	w        *RollbackWatch
	boundary *RollbackHold // the driver's Rollback called, its ROLLBACK not run
	inSQLite *RollbackHold // SQLite's rollback of the transaction, before the driver's Rollback returns
	conn     *sql.Conn     // the reader's, to the watch's file, on a connection the watch's Connector did not make

	mu sync.Mutex
	// holding says what the held rollback is doing as the schedule goes on,
	// for the failure of a wait that returns meanwhile.
	holding  string
	released bool          // the last hold is let go: the wait may return
	returned bool          // Returned was called
	back     chan struct{} // closed by Returned
	asked    int           // the schedule's asks so far
	ask      chan struct{} // closed, and replaced, at each ask
	answered int           // the latest ask a wait answered
	answer   chan struct{} // closed, and replaced, at each answer

	stop chan struct{} // closed as the test ends
	done chan struct{}
	// err is what went wrong with the schedule itself, written before done
	// closes.
	err error
}

// What the held rollback is doing, as WaitProof.holding says it.
const (
	holdingBefore   = "the driver's Rollback of the transaction had not been called"
	holdingBoundary = "its ROLLBACK was held at the driver's boundary, before SQLite rolled it back"
	holdingReader   = "its ROLLBACK was held at the driver's boundary, and a reader's transaction had rolled back on another connection to the file"
	holdingLetGo    = "its ROLLBACK, let go at the driver's boundary, had not yet returned from the driver"
	holdingInSQLite = "SQLite had rolled it back inside its ROLLBACK, and the driver's Rollback had not yet returned"
)

// WaitProofOf holds the next rollback of the transaction tx, which must still
// be open and not yet stopped, at both of WaitProof's places (HoldRollbackOf,
// HoldReturnOf), opens the reader's connection to the watch's file, with
// plain sql.Open, before anything is held, and starts the schedule. The test
// then runs the wait under proof, its stop and then its wait, on its own
// goroutine, and calls Returned the moment the wait returns. One proof at a
// time on a watch, and one wait under it: every wait on the watch answers the
// proof's asks.
func (w *RollbackWatch) WaitProofOf(t testing.TB, tx *sql.Tx) *WaitProof {
	t.Helper()
	p := &WaitProof{
		w: w, holding: holdingBefore,
		back: make(chan struct{}), ask: make(chan struct{}), answer: make(chan struct{}),
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	p.boundary = w.HoldRollbackOf(t, tx)
	p.inSQLite = w.HoldReturnOf(t, tx)
	db, err := sql.Open("sqlite", dsn.File(w.path, ""))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	p.conn, err = db.Conn(context.Background())
	if err != nil {
		t.Fatalf("sqlitetest: the reader's connection: %v", err)
	}
	t.Cleanup(func() { _ = p.conn.Close() })
	if !w.proof.CompareAndSwap(nil, p) {
		t.Fatal("sqlitetest: a WaitProof is already under way on the watch")
	}
	go p.run(w.giveUp())
	// Registered after the holds' and the reader's, so run before them: the
	// schedule ends, and the held rollback goes, before the reader's
	// connection closes.
	t.Cleanup(func() {
		close(p.stop)
		p.boundary.Release()
		p.inSQLite.Release()
		select {
		case <-p.done:
		case <-time.After(perWaitBound):
			t.Error("sqlitetest: the proof's schedule never ended")
		}
		w.proof.CompareAndSwap(p, nil)
	})
	return p
}

// run is the schedule, giving up at giveUp, the watch's, for the rollback to
// reach a hold or the wait to answer. It makes no call on the test, which
// Returned does, and lets both holds go whatever it finds.
func (p *WaitProof) run(giveUp <-chan time.Time) {
	defer close(p.done)
	defer p.inSQLite.Release()
	defer p.boundary.Release()
	if !p.reach(p.boundary, giveUp) {
		return
	}
	p.hold(holdingBoundary)
	seen := p.w.Seen()
	p.hold(holdingReader)
	if err := readAndRollBack(p.conn); err != nil {
		p.err = fmt.Errorf("the reader beside the held rollback: %w", err)
		return
	}
	if p.w.Seen() <= seen {
		p.err = errors.New("the watch did not count the reader's rollback")
		return
	}
	if !p.decides(giveUp) {
		return
	}
	p.hold(holdingLetGo)
	p.boundary.Release()
	if !p.reach(p.inSQLite, giveUp) {
		return
	}
	p.hold(holdingInSQLite)
	if !p.decides(giveUp) {
		return
	}
	p.mu.Lock()
	p.released = true
	p.mu.Unlock()
	p.inSQLite.Release()
}

// hold records what the held rollback is doing now.
func (p *WaitProof) hold(what string) {
	p.mu.Lock()
	p.holding = what
	p.mu.Unlock()
}

// reach waits for the rollback to wait at h, and answers false where the wait
// under proof returned first, which Returned says, or the test ended.
func (p *WaitProof) reach(h *RollbackHold, giveUp <-chan time.Time) bool {
	select {
	case <-h.reached:
		return true
	case <-h.gone:
		p.err = fmt.Errorf("the held rollback can no longer come: %s", h.why)
	case <-giveUp:
		p.err = fmt.Errorf("the transaction's rollback never reached its hold %s %s", h.at, p.w.bound)
	case <-p.back:
	case <-p.stop:
	}
	return false
}

// decides asks the wait under proof to decide again, and waits until it
// answers that it waits on, having found what it waits for not come since
// the ask, or returns. It answers true for the answer, so that only a wait
// that took nothing that came before the ask for its rollback sees the hold
// let go.
func (p *WaitProof) decides(giveUp <-chan time.Time) bool {
	p.mu.Lock()
	p.asked++
	asked := p.asked
	close(p.ask)
	p.ask = make(chan struct{})
	p.mu.Unlock()
	for {
		p.mu.Lock()
		answered, answer, returned := p.answered, p.answer, p.returned
		p.mu.Unlock()
		switch {
		case returned:
			return false
		case answered >= asked:
			return true
		}
		select {
		case <-answer:
		case <-p.back:
		case <-giveUp:
			p.err = fmt.Errorf("the wait under proof neither answered nor returned %s: it waits on something this package's waits do not", p.w.bound)
			return false
		case <-p.stop:
			return false
		}
	}
}

// proofAsks returns the latest ask of the WaitProof under way on the watch,
// and the channel its next ask closes; with none under way, none. A wait
// reads it before what it waits on, so that it answers an ask only for what
// it found after it.
func (w *RollbackWatch) proofAsks() (int, <-chan struct{}) {
	p := w.proof.Load()
	if p == nil {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.asked, p.ask
}

// stillWaiting is a wait on the watch deciding to wait on, what it waits for
// not having come when it read the ask asked: it answers that ask.
func (w *RollbackWatch) stillWaiting(asked int) {
	p := w.proof.Load()
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if asked > p.answered {
		p.answered = asked
		close(p.answer)
		p.answer = make(chan struct{})
	}
}

// readAndRollBack runs a read-only transaction on conn that reads the file's
// schema and rolls back.
func readAndRollBack(conn *sql.Conn) error {
	tx, err := conn.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Rollback()
}

// Returned is the test saying that the wait under proof has returned, the
// moment it has: it fails the test where that wait returned before the
// schedule let the last hold go, saying what the held rollback was doing,
// and waits for the schedule to end first.
func (p *WaitProof) Returned(t testing.TB) {
	t.Helper()
	p.mu.Lock()
	released, holding := p.released, p.holding
	if !p.returned {
		p.returned = true
		close(p.back)
	}
	p.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(perWaitBound):
		t.Fatal("sqlitetest: the proof's schedule never ended")
	}
	if p.err != nil {
		t.Fatalf("sqlitetest: %v", p.err)
	}
	if !released {
		t.Errorf("the wait returned while %s", holding)
	}
}
