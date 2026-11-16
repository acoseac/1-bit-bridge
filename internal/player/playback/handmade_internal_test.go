package playback

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A session a case makes by hand (newSession, Backend.session) is ended as a
// release ends it (session.release and its teardown): its end (down) closed
// once under its lock, then the backend's goroutines joined (Wait). Each wait
// is bounded, the lock's included, so a lock the code under test leaked fails
// the case and never hangs it, its cleanups included: a goroutine wedged on
// that lock never ends (backlog B853, round 4).

// handMadeBound bounds each wait of a hand-made session's end.
const handMadeBound = 5 * time.Second

// lockWithin takes mu within d: false, holding nothing, once d has passed
// with the lock held. A holder that lets go of it (a live monitor's reading)
// is waited for; one that never does is a leak, which the caller says once d
// has passed. The lock is taken on a goroutine of its own, which hands it
// over, or lets go of it at once where the caller has given up: the wait
// polls nothing and sleeps nowhere (the backend's tests never sleep), and a
// lock never let go keeps only that goroutine.
func lockWithin(mu *sync.Mutex, d time.Duration) bool {
	took, gaveUp := make(chan struct{}), make(chan struct{})
	go func() {
		mu.Lock()
		select {
		case took <- struct{}{}:
		case <-gaveUp:
			mu.Unlock()
		}
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-took:
		return true
	case <-timer.C:
		close(gaveUp)
		return false
	}
}

// handMadeBackend is a backend a case builds by hand over cfg, with New's
// completion bookkeeping: zero and wzero closed while no goroutine runs, so
// its Wait answers at once where nothing was spawned, where a nil zero would
// wait to its context's end.
func handMadeBackend(cfg Config) *Backend {
	zero := make(chan struct{})
	close(zero)
	return &Backend{cfg: cfg, sessions: map[engine.SessionID]*session{}, zero: zero, wzero: zero}
}

// closeSession ends s's life (its context) and its end (down), closed once
// under its lock: a release, or its teardown, may have closed it already. The
// lock is taken at once where it is free; a case that failed already (on a
// lock its code kept, as it most likely did) is not kept waiting for it, and
// one that has not is given handMadeBound, for a holder that lets go. It
// answers whether the session's DSD monitor had started, read in that hold,
// and false for ok, the case failed, where the lock stayed held: then nothing
// more is done, since the close would wait for the lock, and the session's
// goroutines may wait for it too.
func closeSession(t *testing.T, s *session) (monitor, ok bool) {
	t.Helper()
	s.cancel()
	if !s.mu.TryLock() && (t.Failed() || !lockWithin(&s.mu, handMadeBound)) {
		t.Errorf("session %d's lock was held after the case: its end is left open and its goroutines unjoined", s.id)
		return false, false
	}
	monitor = s.dsdMonitor
	select {
	case <-s.down:
	default:
		close(s.down)
	}
	s.mu.Unlock()
	return monitor, true
}

// joinBackend joins b's goroutines within handMadeBound.
func joinBackend(t *testing.T, b *Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), handMadeBound)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Errorf("the backend's goroutines: %v", err)
	}
}

// endSession ends s, a session a case made by hand on b, as a release ends
// it: closeSession, then joinBackend where the lock was let go.
func endSession(t *testing.T, b *Backend, s *session) {
	t.Helper()
	if _, ok := closeSession(t, s); ok {
		joinBackend(t, b)
	}
}
