package manifest

import (
	"context"
	"database/sql"
	"errors"
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
// transaction back (Tx.awaitDone, the rollback the watch sees at the
// driver), and only then does SetUpscaleTarget reach tx.Commit(), which
// answers sql.ErrTxDone from its own state without calling the driver's
// Commit. That error carries no cancellation, so the seed's check, which
// took only context.Canceled for a stop, called it a failed start. Neither
// key is written, on a fresh seed or over a target already set.
func TestAStopAfterTheUpscaleTargetsLastStatementIsErrTxDoneAtItsCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool // a target set and committed first
	}{
		{"a fresh seed", false},
		{"over a target already set", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTempStore(t)
			if tc.before {
				if err := s.SetUpscaleTarget(context.Background(), 96000, 16); err != nil {
					t.Fatal(err)
				}
			}
			rollbacks := sqlitetest.WatchRollbacks(t, s.path)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			held := holdUpscaleTargetAtItsCommit(t, s)
			errc := make(chan error, 1)
			go func() { errc <- s.SetUpscaleTarget(ctx, 192000, 24) }()

			tx := held.reached(t)
			seen := rollbacks.Seen()
			cancel()
			waitForTheRollbackOf(t, rollbacks, seen, tx)
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

// waitForTheRollbackOf waits until database/sql has rolled tx back at the
// driver: the watch counts the rollbacks on the store's file, and tx's own
// leaves tx answering sql.ErrTxDone. A probe of a transaction still open runs
// a SELECT, which changes nothing, and the wait goes on past the next
// rollback.
func waitForTheRollbackOf(t *testing.T, w *sqlitetest.RollbackWatch, seen int, tx *sql.Tx) {
	t.Helper()
	for {
		w.WaitPast(t, seen, nil)
		seen = w.Seen()
		if _, err := tx.ExecContext(context.Background(), `SELECT 1`); errors.Is(err, sql.ErrTxDone) {
			return
		}
	}
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
