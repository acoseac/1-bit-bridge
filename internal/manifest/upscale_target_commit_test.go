package manifest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/sqlitetest"
)

// TestAStopAfterTheUpscaleTargetsLastStatementIsErrTxDoneAtItsCommit pins,
// at the store, where backlog B662's failed serve start came from: a serve
// stopped as its upscale seed ran exited 1, printing "seed upscale target:
// sql: transaction has already been committed or rolled back". The cancel
// lands after SetUpscaleTarget's last statement has run at the driver, so
// neither statement sees it. database/sql's own goroutine then rolls the
// transaction back (Tx.awaitDone, the rollback the test waits for at the
// driver, on the transaction's own connection), and only then does
// SetUpscaleTarget reach tx.Commit(), which answers sql.ErrTxDone from its
// own state without calling the driver's Commit. That error carries no
// cancellation, so the seed's check, which took only context.Canceled for a
// stop, called it a failed start. Neither key is written, on a fresh seed or
// over a target already set.
func TestAStopAfterTheUpscaleTargetsLastStatementIsErrTxDoneAtItsCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool // a target set and committed first
	}{
		{"a fresh seed", false},
		{"over a target already set", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rollbacks := openWatchedStore(t)
			if tc.before {
				if err := s.SetUpscaleTarget(context.Background(), 96000, 16); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			held := holdUpscaleTargetAtItsCommit(t, s)
			errc := make(chan error, 1)
			go func() { errc <- s.SetUpscaleTarget(ctx, 192000, 24) }()

			tx := held.reached(t)
			stopAndWaitForTheRollbackOf(t, rollbacks, tx, cancel)
			held.release(nil)
			err := <-errc

			if !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("SetUpscaleTarget after a stop at its commit = %v, want sql.ErrTxDone", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Errorf("the ErrTxDone carries the cancellation (%v); the seed's old check would have taken it", err)
			}
			wantTargetUnchanged(t, s, tc.before)
		})
	}
}

// TestTheUpscaleTargetsHoldAtItsCommitCommitsWithNothingCancelled is the
// control that the hold is not what fails the write: held at the same point,
// on a context nothing cancels, the transaction commits both keys.
func TestTheUpscaleTargetsHoldAtItsCommitCommitsWithNothingCancelled(t *testing.T) {
	s := openTempStore(t)
	held := holdUpscaleTargetAtItsCommit(t, s)
	errc := make(chan error, 1)
	go func() { errc <- s.SetUpscaleTarget(context.Background(), 192000, 24) }()
	held.reached(t)
	held.release(nil)
	if err := <-errc; err != nil {
		t.Fatalf("SetUpscaleTarget held at its commit with nothing cancelled = %v, want nil", err)
	}
	rate, bits, err := s.GetUpscaleTarget(context.Background())
	if err != nil || rate != 192000 || bits != 24 {
		t.Errorf("after the commit: (%d, %d, %v), want (192000, 24, nil)", rate, bits, err)
	}
}

// TestAnErrorAtTheUpscaleTargetsCommitRollsBothKeysBack: an error the hold
// returns is SetUpscaleTarget's, and the transaction it ends writes neither
// key, fresh or over a target already set.
func TestAnErrorAtTheUpscaleTargetsCommitRollsBothKeysBack(t *testing.T) {
	refused := errors.New("refused by the test")
	for _, before := range []bool{false, true} {
		s := openTempStore(t)
		if before {
			if err := s.SetUpscaleTarget(context.Background(), 96000, 16); err != nil {
				t.Fatal(err)
			}
		}
		held := holdUpscaleTargetAtItsCommit(t, s)
		errc := make(chan error, 1)
		go func() { errc <- s.SetUpscaleTarget(context.Background(), 192000, 24) }()
		held.reached(t)
		held.release(refused)
		if err := <-errc; !errors.Is(err, refused) {
			t.Fatalf("SetUpscaleTarget whose hold refused = %v, want %v", err, refused)
		}
		wantTargetUnchanged(t, s, before)
	}
}

// heldUpscaleTarget is SetUpscaleTarget held between its last statement and
// its commit (Store.upscaleTargetBeforeCommit).
type heldUpscaleTarget struct {
	atCommit chan *sql.Tx
	answer   chan error
	once     sync.Once
}

// holdUpscaleTargetAtItsCommit holds every SetUpscaleTarget of s once both
// keys are written, until release. A test that fails first lets it go in a
// cleanup, which runs before openTempStore's close of s.
func holdUpscaleTargetAtItsCommit(t *testing.T, s *Store) *heldUpscaleTarget {
	t.Helper()
	h := &heldUpscaleTarget{atCommit: make(chan *sql.Tx, 1), answer: make(chan error, 1)}
	s.upscaleTargetBeforeCommit = func(tx *sql.Tx) error {
		h.atCommit <- tx
		return <-h.answer
	}
	t.Cleanup(func() { h.release(nil) })
	return h
}

// reached returns the held transaction once SetUpscaleTarget has written
// both keys and stopped at its commit, within the 10 s a statement the test
// started itself is given to reach a point (sqlitetest's per-wait bound).
func (h *heldUpscaleTarget) reached(t *testing.T) *sql.Tx {
	t.Helper()
	select {
	case tx := <-h.atCommit:
		return tx
	case <-time.After(10 * time.Second):
		t.Fatal("SetUpscaleTarget never reached its commit within 10s")
		return nil
	}
}

// release lets the held SetUpscaleTarget go on, its hold answering err.
func (h *heldUpscaleTarget) release(err error) {
	h.once.Do(func() { h.answer <- err })
}

// openWatchedStore opens a store in a folder of its own whose connections a
// RollbackWatch armed on its database makes (the store's connector seam, set
// before the open), so that a test can take the rollback of one of its
// transactions at the driver (backlog B869). As the test ends the store
// closes, then the seam is put back and the watch disarmed.
func openWatchedStore(t *testing.T) (*Store, *sqlitetest.RollbackWatch) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	w := sqlitetest.WatchRollbacks(t, path)
	t.Cleanup(setStoreConnector(path, w.Connector))
	return openTestStoreAt(t, path), w
}

// stopAndWaitForTheRollbackOf stops the transaction tx through stop and waits
// until database/sql has rolled tx back at the driver, on tx's own
// connection: the rollback is taken before the stop (RollbackOf), so one that
// runs before the wait begins is kept, and has run once the driver's
// Rollback there has returned no error, SQLite having rolled tx back inside
// it. Neither a rollback counted on the store's file, which another
// connection's makes too, nor tx answering sql.ErrTxDone, which database/sql
// answers from the moment it marks tx done, before it calls the driver's
// Rollback, nor SQLite's own rollback, inside the driver's ROLLBACK before
// that statement ends and the Rollback returns, says that tx's own has run
// (backlog B869).
func stopAndWaitForTheRollbackOf(t *testing.T, w *sqlitetest.RollbackWatch, tx *sql.Tx, stop func()) {
	t.Helper()
	rollback := w.RollbackOf(t, tx)
	stop()
	rollback.Wait(t)
}

// TestAStopAtTheUpscaleTargetsCommitWaitsForItsOwnRollback proves
// stopAndWaitForTheRollbackOf (backlog B869): with the write's rollback held
// at the driver, at its boundary while a reader's transaction rolls back on
// another connection to the store's file, and then once SQLite has rolled it
// back inside the driver's ROLLBACK, before the driver's Rollback returns,
// the wait does not answer, and answers once the held rollback has run
// (sqlitetest's WaitProof, which lets each hold go only once the wait has
// answered that it waits on); the commit then answers sql.ErrTxDone and
// writes neither key.
func TestAStopAtTheUpscaleTargetsCommitWaitsForItsOwnRollback(t *testing.T) {
	s, rollbacks := openWatchedStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held := holdUpscaleTargetAtItsCommit(t, s)
	errc := make(chan error, 1)
	go func() { errc <- s.SetUpscaleTarget(ctx, 192000, 24) }()

	tx := held.reached(t)
	proof := rollbacks.WaitProofOf(t, tx)
	stopAndWaitForTheRollbackOf(t, rollbacks, tx, cancel)
	proof.Returned(t)
	held.release(nil)
	if err := <-errc; !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("SetUpscaleTarget after a stop at its commit = %v, want sql.ErrTxDone", err)
	}
	wantTargetUnchanged(t, s, false)
}

// wantTargetUnchanged requires the target the store held before the write:
// none, or the 96000/16 set first.
func wantTargetUnchanged(t *testing.T, s *Store, before bool) {
	t.Helper()
	rate, bits, err := s.GetUpscaleTarget(context.Background())
	if !before {
		if !errors.Is(err, ErrUpscaleTargetUnset) {
			t.Errorf("a fresh seed that did not commit left (%d, %d, %v), want ErrUpscaleTargetUnset", rate, bits, err)
		}
		return
	}
	if err != nil || rate != 96000 || bits != 16 {
		t.Errorf("a write that did not commit left (%d, %d, %v), want the target set first (96000, 16)", rate, bits, err)
	}
}
