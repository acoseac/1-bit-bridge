package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// stopGuard bounds each wait of a test of a stopped ticker and catches only a
// hang: no bound a stop arms fires there but by the test's hand
// (heldStopBounds), so nothing those tests wait for is a clock's, which a busy
// runner moves (backlog B895).
const stopGuard = 60 * time.Second

// The steps of a verification the tests of a stopped ticker hold, in the
// words the backup package names them by.
const (
	stepWriteWords    = "its verdict's write"
	stepReadBackWords = "its read of the record after it"
)

// heldStopBounds stands in for every bound a stop arms on the verification of
// a snapshot in one backups folder (backup.SetStopBoundsForTest), its caller's
// expiry and the bound on its worker's step: each is held until the test
// fires it, and every one is fired at the test's end (end), from when a bound
// armed fires as it is armed.
type heldStopBounds struct {
	mu      sync.Mutex
	armed   []*heldBound
	changed chan struct{}
	ended   bool
}

// heldBound is one bound a stop armed: on the snapshot dir, at step ("" for
// its caller's expiry), its length as armed (after); fire fires it.
type heldBound struct {
	dir, step string
	after     time.Duration
	fire      func()
}

// holdStopBounds stands a heldStopBounds in for the bounds a stop arms on the
// verifications of the snapshots in root, put back once every cleanup
// registered after it has run.
func holdStopBounds(t *testing.T, root string) *heldStopBounds {
	t.Helper()
	b := &heldStopBounds{changed: make(chan struct{})}
	t.Cleanup(backup.SetStopBoundsForTest(func(dir, step string, after time.Duration) <-chan time.Time {
		if filepath.Dir(dir) != root {
			return nil
		}
		c := make(chan time.Time)
		bound := &heldBound{dir: dir, step: step, after: after, fire: sync.OnceFunc(func() { close(c) })}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.armed = append(b.armed, bound)
		close(b.changed)
		b.changed = make(chan struct{})
		if b.ended {
			bound.fire()
		}
		return c
	}))
	return b
}

// await answers the first bound armed at step ("" for a caller's expiry),
// failing the test where none is within stopGuard.
func (b *heldStopBounds) await(t *testing.T, step string) *heldBound {
	t.Helper()
	guard := time.After(stopGuard)
	for {
		b.mu.Lock()
		changed := b.changed
		for _, bound := range b.armed {
			if bound.step == step {
				b.mu.Unlock()
				return bound
			}
		}
		b.mu.Unlock()
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("no bound at %q was armed within %v", step, stopGuard)
		}
	}
}

// end fires every bound armed, and each armed from now on as it is armed.
func (b *heldStopBounds) end() {
	b.mu.Lock()
	b.ended = true
	armed := append([]*heldBound(nil), b.armed...)
	b.mu.Unlock()
	for _, bound := range armed {
		bound.fire()
	}
}

// stopPark is a hook's park: the hook's first call closes entered and waits
// until letGo, which the test's end calls too; every later call passes.
type stopPark struct {
	entered chan struct{}
	out     chan struct{}
	first   atomic.Bool
	letGo   func()
}

// newStopPark is a stopPark not yet entered.
func newStopPark() *stopPark {
	p := &stopPark{entered: make(chan struct{}), out: make(chan struct{})}
	p.letGo = sync.OnceFunc(func() { close(p.out) })
	return p
}

// park parks its first caller until letGo.
func (p *stopPark) park() {
	if p.first.CompareAndSwap(false, true) {
		close(p.entered)
		<-p.out
	}
}

// awaitGuarded waits for c to close, failing the test with what where it does
// not within stopGuard.
func awaitGuarded(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(stopGuard):
		t.Fatalf("%s within %v", what, stopGuard)
	}
}

// runStoppableTicker runs the backup ticker over src, writing to stdout and
// stderr, and answers its stop and its end. Its drain and its cleanup run
// before any seam its caller set first is put back: the cleanup lets go of
// what the test holds (release), fires every bound the stop armed, stops the
// ticker and waits for it, and for every verification of a snapshot in root,
// to end.
func runStoppableTicker(t *testing.T, src backup.Sources, root string, bounds *heldStopBounds, stdout, stderr io.Writer, release func()) (stop context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ended := make(chan struct{})
	drainLoopOnCleanup(t, cancel, ended, "the backup ticker")
	t.Cleanup(func() {
		release()
		bounds.end()
		cancel()
		waitClosed(t, ended, "the backup ticker")
		awaitVerificationsEnd(t, root)
	})
	go func() {
		defer close(ended)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, stdout, stderr, nil)
	}()
	return cancel, ended
}

// handoffs records which of a verification's two handoffs to its caller the
// verifications of the snapshots in root took: the worker's answer, or the
// caller's expiry (backup.SetVerifyHandoffForTest).
type handoffs struct {
	answered, expired atomic.Bool
}

// watchHandoffs records the handoffs of the verifications of the snapshots in
// root.
func watchHandoffs(t *testing.T, root string) *handoffs {
	t.Helper()
	h := &handoffs{}
	t.Cleanup(backup.SetVerifyHandoffForTest(func(dir string) {
		if filepath.Dir(dir) == root {
			h.answered.Store(true)
		}
	}, func(dir string) {
		if filepath.Dir(dir) == root {
			h.expired.Store(true)
		}
	}))
	return h
}

// requireTheWorkersAnswer fails the test unless the caller took its worker's
// answer, its expiry never having fired.
func (h *handoffs) requireTheWorkersAnswer(t *testing.T) {
	t.Helper()
	if !h.answered.Load() || h.expired.Load() {
		t.Fatalf("the caller did not take its worker's answer: answered %v, expired %v", h.answered.Load(), h.expired.Load())
	}
}

// TestATickerStoppedWhileAStepHangsSaysNothingWhicheverBoundFiresFirst is
// backlog B895's red (CI's macOS run 43470491773, #1460): stopped while a step
// of its verification hangs, the verdict's write in its sync or the read of
// its record after it, the ticker says nothing of the stop whichever of the
// two bounds the stop arms fires first: its caller's expiry, which abandons
// the worker, or the worker's bound on the step, which gives the step up, its
// cause the stop, the worker answering before the expiry. The test fires the
// bounds (backup.SetStopBoundsForTest), and requires the handoff it named.
// Red before B895 wherever the worker's bound came first: "backup (startup):
// the verification of <snap> was given up, and its step still holds the
// snapshots until it ends; its write did not finish, and its record reads
// database INCOMPLETE: unsynced, files INCOMPLETE: unsynced: …".
func TestATickerStoppedWhileAStepHangsSaysNothingWhicheverBoundFiresFirst(t *testing.T) {
	for _, step := range []string{stepWriteWords, stepReadBackWords} {
		for _, first := range []struct {
			name   string
			worker bool
		}{{"the worker's bound", true}, {"the caller's expiry", false}} {
			t.Run(step+", "+first.name+" first", func(t *testing.T) {
				_, dataDir, root := backupInstall(t)
				src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
				bounds := holdStopBounds(t, root)
				held := newStopPark()
				if step == stepWriteWords {
					t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { held.park(); return nil }))
				} else {
					t.Cleanup(backup.SetReadBackFailureForTest(func(string) error { held.park(); return nil }))
				}
				took := watchHandoffs(t, root)
				var stdout, stderr safeBuffer
				stop, done := runStoppableTicker(t, src, root, bounds, &stdout, &stderr, held.letGo)
				awaitGuarded(t, held.entered, "the verification did not reach "+step)
				stop()
				expiry, bound := bounds.await(t, ""), bounds.await(t, step)
				if first.worker {
					bound.fire()
				} else {
					expiry.fire()
				}
				awaitGuarded(t, done, "the ticker did not return once "+first.name+" fired")
				if first.worker {
					took.requireTheWorkersAnswer(t)
				} else if !took.expired.Load() || took.answered.Load() {
					t.Fatalf("the caller's expiry did not answer it: expired %v, answered %v", took.expired.Load(), took.answered.Load())
				}
				if got := stderr.String(); got != "" {
					t.Errorf("a stopped verification was reported: %s", got)
				}
			})
		}
	}
}

// TestATickerStoppedWhileItsCheckHangsSaysNothing: a check its caller's stop
// ended that does not end within half of backup.StopWriteBound of it is given
// up, its cause the stop, and its INCOMPLETE written; the ticker says nothing
// of it, which is the stop's (backlog B895). Red before B895, which said its
// verification was given up whenever its worker answered before its caller's
// expiry, as it does with half the bound to spare. The check's own give-up is
// half of StopWriteBound on its clock (B886 stands a seam in for it), its
// caller's expiry held.
func TestATickerStoppedWhileItsCheckHangsSaysNothing(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	bounds := holdStopBounds(t, root)
	check := newStopPark()
	t.Cleanup(backup.SetOnLowPriorityForTest(check.park))
	took := watchHandoffs(t, root)
	var stdout, stderr safeBuffer
	stop, done := runStoppableTicker(t, src, root, bounds, &stdout, &stderr, check.letGo)
	awaitGuarded(t, check.entered, "the verification's check did not begin")
	stop()
	bounds.await(t, "")
	awaitGuarded(t, done, "the ticker did not return once its check was given up")
	took.requireTheWorkersAnswer(t)
	if got := stderr.String(); got != "" {
		t.Errorf("a stopped verification was reported: %s", got)
	}
	dirs := snapshotDirs(t, root)
	if len(dirs) != 1 {
		t.Fatalf("%d snapshots, want the ticker's one", len(dirs))
	}
	if rec := recordOf(t, dirs[0]); backup.StateWords(rec.DatabaseState, rec.DatabaseReasons) != "INCOMPLETE: cancelled" {
		t.Errorf("the record reads database %s, want the INCOMPLETE a stop writes", backup.StateWords(rec.DatabaseState, rec.DatabaseReasons))
	}
}

// stopWhileTheWriteHangs runs the ticker over a fresh install, its verdict's
// write held in its sync, stops it and fires the worker's bound on that write,
// its caller's expiry held, and answers what it said on stderr once it
// returned. prepare, given the install's data dir and backups folder, sets
// what its case adds before the ticker starts.
func stopWhileTheWriteHangs(t *testing.T, prepare func(dataDir, root string)) string {
	t.Helper()
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	bounds := holdStopBounds(t, root)
	held := newStopPark()
	t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { held.park(); return nil }))
	prepare(dataDir, root)
	took := watchHandoffs(t, root)
	var stdout, stderr safeBuffer
	stop, done := runStoppableTicker(t, src, root, bounds, &stdout, &stderr, held.letGo)
	awaitGuarded(t, held.entered, "the verdict's write did not reach its sync")
	stop()
	bounds.await(t, "")
	bounds.await(t, stepWriteWords).fire()
	awaitGuarded(t, done, "the ticker did not return once its write's bound fired")
	took.requireTheWorkersAnswer(t)
	return stderr.String()
}

// TestATickerStoppedBesideAFailureStillSaysIt: what the ticker takes for its
// stop is a give-up the stop caused and nothing else (backup.AsTheStop,
// backlog B895). Stopped, the worker's bound on its hung verdict's write
// firing before its caller's expiry, it still says a check that found the
// database FAILED and a record whose read-back failed; stopped while its
// check hangs, an INCOMPLETE whose write failed; and a write given up at its
// own bound, its answer coming only once the ticker is stopped, it says as
// the deadline it is, never as the stop that came after it.
func TestATickerStoppedBesideAFailureStillSaysIt(t *testing.T) {
	t.Run("a check that found the database FAILED", func(t *testing.T) {
		said := stopWhileTheWriteHangs(t, func(dataDir, _ string) { makeStoreNewer(t, manifest.DefaultDBPath(dataDir)) })
		for _, want := range []string{"was given up", "database FAILED: schema-newer"} {
			if !strings.Contains(said, want) {
				t.Errorf("the stopped ticker did not say %q:\n%s", want, said)
			}
		}
	})
	t.Run("a record whose read-back failed", func(t *testing.T) {
		said := stopWhileTheWriteHangs(t, func(_, root string) {
			t.Cleanup(backup.SetReadBackFailureForTest(func(dir string) error {
				if filepath.Dir(dir) == root {
					return errors.New("injected: the read-back failed")
				}
				return nil
			}))
		})
		for _, want := range []string{"was given up", "its record could not be read back", "injected: the read-back failed"} {
			if !strings.Contains(said, want) {
				t.Errorf("the stopped ticker did not say %q:\n%s", want, said)
			}
		}
	})
	t.Run("an INCOMPLETE whose write failed", func(t *testing.T) {
		_, dataDir, root := backupInstall(t)
		src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
		bounds := holdStopBounds(t, root)
		check := newStopPark()
		t.Cleanup(backup.SetOnLowPriorityForTest(check.park))
		t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(dir string) error {
			if filepath.Dir(dir) == root {
				return errors.New("injected: the staging failed")
			}
			return nil
		}, nil, nil))
		took := watchHandoffs(t, root)
		var stdout, stderr safeBuffer
		stop, done := runStoppableTicker(t, src, root, bounds, &stdout, &stderr, check.letGo)
		awaitGuarded(t, check.entered, "the verification's check did not begin")
		stop()
		bounds.await(t, "")
		awaitGuarded(t, done, "the ticker did not return once its check was given up")
		took.requireTheWorkersAnswer(t)
		said := stderr.String()
		for _, want := range []string{"was given up", "its write failed", "its check did not end", "injected: the staging failed"} {
			if !strings.Contains(said, want) {
				t.Errorf("the stopped ticker did not say %q:\n%s", want, said)
			}
		}
	})
	t.Run("a write given up at its own bound", func(t *testing.T) {
		_, dataDir, root := backupInstall(t)
		src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
		bounds := holdStopBounds(t, root)
		write, readBack := newStopPark(), newStopPark()
		t.Cleanup(backup.SetSyncVerdictForTest(func(string) error { write.park(); return nil }))
		t.Cleanup(backup.SetReadBackFailureForTest(func(dir string) error {
			if filepath.Dir(dir) == root {
				readBack.park()
			}
			return nil
		}))
		// The write's own bound is set once its snapshot is named, and put
		// back after the ticker's cleanup, registered later, has run.
		var restoreBound func()
		t.Cleanup(func() {
			if restoreBound != nil {
				restoreBound()
			}
		})
		took := watchHandoffs(t, root)
		stdout := holdOn("wrote ")
		var stderr safeBuffer
		stop, done := runStoppableTicker(t, src, root, bounds, stdout, &stderr, func() { stdout.letGo(); write.letGo(); readBack.letGo() })
		awaitGuarded(t, stdout.held, "the ticker did not say it wrote its snapshot")
		dirs := snapshotDirs(t, root)
		if len(dirs) != 1 {
			t.Fatalf("%d snapshots, want the ticker's one", len(dirs))
		}
		// Its sync held, the write cannot answer within a millisecond: its own
		// bound gives it up, before the stop.
		restoreBound = backup.SetStepBoundForTest(dirs[0], time.Millisecond, stepWriteWords)
		stdout.letGo()
		awaitGuarded(t, readBack.entered, "the verification did not read its record back once its write was given up")
		stop()
		bounds.await(t, "")
		bounds.await(t, stepReadBackWords)
		readBack.letGo()
		awaitGuarded(t, done, "the ticker did not return once its read-back answered")
		took.requireTheWorkersAnswer(t)
		said := stderr.String()
		for _, want := range []string{"was given up", stepWriteWords + " did not end within its bound (" + context.DeadlineExceeded.Error() + ")"} {
			if !strings.Contains(said, want) {
				t.Errorf("the stopped ticker did not say %q:\n%s", want, said)
			}
		}
	})
}
