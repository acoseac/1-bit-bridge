package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The fingerprint sweeper and the smart-playlist regenerator run on runServe's
// scanCtx, and a shutdown that cancels it while a pass is under way is a pass
// that STOPPED, not one that failed (ctxerr.WithoutCancellation). Each pair
// below drives the real loop: the cancelled half cancels from the loop's
// `enabled` gate, the last thing it calls before the pass, so the pass itself
// runs on a context shutdown has just cancelled; the failing half closes the
// store under a live context, so a genuine failure is still reported.

const (
	fingerprintListFailed = "fingerprint sweep: list candidates"
	smartPlaylistFailed   = "smart-playlist regeneration failed"
)

// TestAFingerprintSweepStoppedByShutdownReportsNothing: the candidate listing
// runs on a cancelled context and fails with the cancellation, which is not a
// failed sweep.
func TestAFingerprintSweepStoppedByShutdownReportsNothing(t *testing.T) {
	store := openSweepCancelStore(t)
	rec := loggingtest.Record(t)
	ranPass := runOneFingerprintPass(t, store, true)
	if !ranPass {
		t.Fatal("the sweep's enabled gate was never asked, so no pass ran and nothing was tested")
	}
	if got := rec.Logged(fingerprintListFailed); len(got) != 0 {
		t.Errorf("a sweep stopped by shutdown was reported as a failure:\n%s", strings.Join(got, "\n"))
	}
}

// TestAFingerprintSweepThatFailsIsStillReported is the control: on a live
// context, a candidate listing that fails is reported.
func TestAFingerprintSweepThatFailsIsStillReported(t *testing.T) {
	store := openSweepCancelStore(t)
	_ = store.Close() // every query now fails, on a context nobody cancelled
	rec := loggingtest.Record(t)
	runOneFingerprintPass(t, store, false)
	if got := rec.Logged(fingerprintListFailed); len(got) != 1 {
		t.Errorf("a candidate listing that failed on a live context logged %d lines, want 1:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// TestASmartPlaylistRegenerationStoppedByShutdownReportsNothing: the
// regeneration's queries run on a cancelled context and fail with the
// cancellation, which is not a failed regeneration.
func TestASmartPlaylistRegenerationStoppedByShutdownReportsNothing(t *testing.T) {
	store := openSweepCancelStore(t)
	rec := loggingtest.Record(t)
	if !runOneSmartPlaylistPass(t, store, true) {
		t.Fatal("the regenerator's enabled gate was never asked, so no pass ran and nothing was tested")
	}
	if got := rec.Logged(smartPlaylistFailed); len(got) != 0 {
		t.Errorf("a regeneration stopped by shutdown was reported as a failure:\n%s", strings.Join(got, "\n"))
	}
}

// TestASmartPlaylistRegenerationThatFailsIsStillReported is the control.
func TestASmartPlaylistRegenerationThatFailsIsStillReported(t *testing.T) {
	store := openSweepCancelStore(t)
	_ = store.Close()
	rec := loggingtest.Record(t)
	runOneSmartPlaylistPass(t, store, false)
	if got := rec.Logged(smartPlaylistFailed); len(got) != 1 {
		t.Errorf("a regeneration that failed on a live context logged %d lines, want 1:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// openSweepCancelStore opens a manifest store the test closes at cleanup.
func openSweepCancelStore(t *testing.T) *manifest.Store {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup, not defer: the loops' drains below are cleanups too, and a
	// deferred Close would run before them (TestSmartPlaylistRegeneratorReadsAnalysisLive).
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// runOneFingerprintPass runs the fingerprint sweeper until one pass has
// finished, cancelling from the pass's own gate when stop is set. Reports
// whether the gate was asked at all.
func runOneFingerprintPass(t *testing.T, store *manifest.Store, stop bool) bool {
	t.Helper()
	oldSettle := fingerprintSweeperSettleDelay
	fingerprintSweeperSettleDelay = time.Millisecond
	t.Cleanup(func() { fingerprintSweeperSettleDelay = oldSettle })

	ctx, cancel := context.WithCancel(context.Background())
	asked := make(chan struct{}, 1)
	enabled := func() bool {
		select {
		case asked <- struct{}{}:
		default:
		}
		if stop {
			cancel()
		}
		return true
	}
	status := &sweepStatus[admin.FingerprintSweepCounts]{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFingerprintSweeper(ctx, &fingerprintSweeper{store: store, maxPerRun: 10, workers: 1},
			enabled, staticInterval(time.Hour), nil, nil, status)
	}()
	drainLoopOnCleanup(t, cancel, done, "the fingerprint sweeper")
	return awaitOnePass(t, asked, status.snapshot)
}

// runOneSmartPlaylistPass is runOneFingerprintPass for the regenerator.
func runOneSmartPlaylistPass(t *testing.T, store *manifest.Store, stop bool) bool {
	t.Helper()
	oldSettle := smartPlaylistSettleDelay
	smartPlaylistSettleDelay = time.Millisecond
	t.Cleanup(func() { smartPlaylistSettleDelay = oldSettle })

	ctx, cancel := context.WithCancel(context.Background())
	asked := make(chan struct{}, 1)
	enabled := func() bool {
		select {
		case asked <- struct{}{}:
		default:
		}
		if stop {
			cancel()
		}
		return true
	}
	status := &sweepStatus[struct{}]{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartPlaylistRegenerator(ctx, store, func() bool { return false },
			enabled, staticInterval(time.Hour), nil, status)
	}()
	drainLoopOnCleanup(t, cancel, done, "the smart-playlist regenerator")
	return awaitOnePass(t, asked, status.snapshot)
}

// awaitOnePass waits until the loop's gate has been asked and the pass it
// let through has finished. Finished means the run state's lastEnd is set,
// which sweepFinished stamps on a failed and a stopped pass alike.
func awaitOnePass[T any](t *testing.T, asked <-chan struct{}, snapshot func() (bool, time.Time, time.Time, time.Time, *T)) bool {
	t.Helper()
	select {
	case <-asked:
	case <-time.After(5 * time.Second):
		return false
	}
	deadline := time.After(5 * time.Second)
	for {
		if running, _, lastEnd, _, _ := snapshot(); !running && !lastEnd.IsZero() {
			return true
		}
		select {
		case <-deadline:
			t.Fatal("the pass started and did not finish within 5s")
		case <-time.After(time.Millisecond):
		}
	}
}
