package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// subtreeScanOutcome is what the parked ScanSubtree reports back to the
// pass that started it.
type subtreeScanOutcome struct {
	wrote int
	err   error
}

// A policy change that meets a subtree scan which commits nothing is
// deferred, not applied. That scan is in flight (activeScans) for its
// whole body, and its tail skips the restamp when nothing was committed,
// deleted or renamed. Returning a zero count with a nil error is what
// the sweeper reads as applied, so the losing copy stays suppressed
// until the next full scan.
//
// The scan is started from beforeApplyDupeStampsHookForTests and held
// on the store lock, which the skip path takes in ResetTrackMissingCount,
// until the external pass has returned. Releasing the lock at the hook
// would let the scan finish before the re-check.
func TestAPolicyChangeDuringANoOpSubtreeScanIsDeferred(t *testing.T) {
	store, sc, mode, stable := stableFileWithASuppressedCopy(t)
	ctx := context.Background()
	loser := "CopyA/Album/01 Song.flac"
	*mode = dupes.FilterOff
	unlock, scanDone, collected := parkNoOpSubtreeScan(t, sc, stable)

	n, err := sc.RestampDuplicates(ctx)
	unlock()
	parked := mustTakeSubtreeScan(t, scanDone, collected)
	requireNoOpSubtree(t, parked)
	requireDeferredKeepsSuppression(t, store, loser, n, err)
	requireLaterNoOpKeepsSuppression(t, sc, store, stable, loser)
	requireRetryClearsSuppression(t, sc, store, loser)
}

// parkNoOpSubtreeScan starts a ScanSubtree of stable from the restamp
// hook and holds the store lock until unlock runs, so the scan is in
// flight at the commit-time re-check and has not finished.
func parkNoOpSubtreeScan(t *testing.T, sc *Scanner, stable string) (unlock func(), done <-chan subtreeScanOutcome, collected *atomic.Bool) {
	t.Helper()
	scanDone := make(chan subtreeScanOutcome, 1)
	var storeLocked atomic.Bool
	var scanStarted atomic.Bool
	collected = &atomic.Bool{}
	unlock = func() {
		if storeLocked.CompareAndSwap(true, false) {
			sc.store.mu.Unlock()
		}
	}
	beforeApplyDupeStampsHookForTests = func() {
		beforeApplyDupeStampsHookForTests = nil
		sc.store.mu.Lock()
		storeLocked.Store(true)
		scanStarted.Store(true)
		go func() {
			wrote, err := sc.ScanSubtree(context.Background(), stable)
			scanDone <- subtreeScanOutcome{wrote: wrote, err: err}
		}()
		waitUntilSubtreeScanIsInFlight(t, sc, scanDone)
	}
	t.Cleanup(func() {
		beforeApplyDupeStampsHookForTests = nil
		unlock()
		if scanStarted.Load() && collected.CompareAndSwap(false, true) {
			select {
			case <-scanDone:
			case <-time.After(5 * time.Second):
			}
		}
	})
	return unlock, scanDone, collected
}

func mustTakeSubtreeScan(t *testing.T, done <-chan subtreeScanOutcome, collected *atomic.Bool) subtreeScanOutcome {
	t.Helper()
	parked, ok := takeSubtreeScan(done, collected)
	if !ok {
		t.Fatal("cleanup collected the parked scan")
	}
	return parked
}

func requireNoOpSubtree(t *testing.T, parked subtreeScanOutcome) {
	t.Helper()
	if parked.err != nil {
		t.Fatalf("no-op subtree scan: %v", parked.err)
	}
	if parked.wrote != 0 {
		t.Fatalf("in-flight subtree scan wrote %d rows, so its tail would have stamped", parked.wrote)
	}
}

func requireDeferredKeepsSuppression(t *testing.T, store *Store, loser string, n int, err error) {
	t.Helper()
	if !errors.Is(err, ErrDupeRestampDeferred) || n != 0 {
		t.Fatalf("pass during a no-op subtree scan = (%d, %v), want (0, deferred)", n, err)
	}
	if st := stampOf(t, store, loser); !st.Suppressed {
		t.Fatalf("deferred pass cleared suppression: %+v", st)
	}
}

func requireLaterNoOpKeepsSuppression(t *testing.T, sc *Scanner, store *Store, stable, loser string) {
	t.Helper()
	wrote, err := sc.ScanSubtree(context.Background(), stable)
	if err != nil {
		t.Fatal(err)
	}
	if wrote != 0 {
		t.Fatalf("second subtree scan wrote %d rows; it has to stay a no-op so its tail does not stamp", wrote)
	}
	if st := stampOf(t, store, loser); !st.Suppressed {
		t.Fatalf("a later no-op subtree scan applied the policy: %+v", st)
	}
}

func requireRetryClearsSuppression(t *testing.T, sc *Scanner, store *Store, loser string) {
	t.Helper()
	n, err := sc.RestampDuplicates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("retry after the scan changed %d rows, want 1", n)
	}
	if st := stampOf(t, store, loser); st.Suppressed {
		t.Fatalf("retry must clear suppression once no scan is in flight: %+v", st)
	}
}

func stableFileWithASuppressedCopy(t *testing.T) (*Store, *Scanner, *dupes.FilterMode, string) {
	t.Helper()
	root := t.TempDir()
	stable := filepath.Join(root, "Stable")
	if err := os.MkdirAll(stable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stable, "song.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, sc := newScanFixture(t, root)
	mode := dupes.FilterHighestQuality
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: mode} })
	scanOnce(t, sc, "index the unchanged file")
	seedDupePair(t, store)
	if _, err := sc.RestampDuplicates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := stampOf(t, store, "CopyA/Album/01 Song.flac"); !st.Suppressed {
		t.Fatalf("fixture: highest-quality must suppress the smaller copy, got %+v", st)
	}
	return store, sc, &mode, stable
}

func takeSubtreeScan(done <-chan subtreeScanOutcome, collected *atomic.Bool) (subtreeScanOutcome, bool) {
	if !collected.CompareAndSwap(false, true) {
		return subtreeScanOutcome{}, false
	}
	return <-done, true
}

func waitUntilSubtreeScanIsInFlight(t *testing.T, sc *Scanner, done <-chan subtreeScanOutcome) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := t.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for sc.activeScans.Load() == 0 {
		select {
		case result := <-done:
			t.Fatalf("subtree scan ended before it was in flight: wrote %d: %v", result.wrote, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("subtree scan never became in flight")
		}
		runtime.Gosched()
	}
}
