package api

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// hangingStat returns a statFunc stand-in that parks forever, plus a
// counter of how many times it was entered. Callers release the parked
// goroutines via the returned close func so the test doesn't leak them
// into the rest of the package's run.
func hangingStat(t *testing.T) (fn func(string) (os.FileInfo, error), entered *atomic.Int64, release func()) {
	t.Helper()
	var n atomic.Int64
	gate := make(chan struct{})
	var closed atomic.Bool
	rel := func() {
		if closed.CompareAndSwap(false, true) {
			close(gate)
		}
	}
	t.Cleanup(rel)
	return func(string) (os.FileInfo, error) {
		n.Add(1)
		<-gate
		return nil, os.ErrNotExist
	}, &n, rel
}

// TestReachabilityProbe_HungMountDoesNotStackGoroutines pins the
// in-flight guard.
//
// A hard-mount NFS (or a vanished SMB server) parks os.Stat in the
// kernel indefinitely. The 2 s timeout retires the singleflight flight
// but NOT the stat goroutine, so pre-fix every lapsed TTL window
// launched another one that also parked — unbounded growth on exactly
// the mount failure this cache exists to survive.
//
// The assertion is on stat ENTRIES rather than runtime.NumGoroutine():
// goroutine counts are noisy under a parallel package run, whereas
// "how many stats did we start" is precisely the quantity the guard
// bounds, and it fails loudly on the pre-fix code (which would enter
// once per call).
func TestReachabilityProbe_HungMountDoesNotStackGoroutines(t *testing.T) {
	fn, entered, release := hangingStat(t)
	orig := statFunc
	statFunc = fn
	t.Cleanup(func() { statFunc = orig })

	c := newReachabilityCache()
	const root = "/mnt/hung-nfs"

	// First probe parks a stat and times out into an offline verdict.
	if st := c.probe(context.Background(), root); st.Reachable {
		t.Fatalf("hung mount must report unreachable, got %+v", st)
	}
	if got := entered.Load(); got != 1 {
		t.Fatalf("first probe entered stat %d times, want 1", got)
	}

	// Now hammer it the way iOS polling does, forcing past the TTL each
	// time so the cache can't be what's absorbing the calls.
	for i := 0; i < 25; i++ {
		c.mu.Lock()
		if e, ok := c.entries[root]; ok {
			e.checkedAt = time.Now().Add(-2 * reachabilityTTL)
			c.entries[root] = e
		}
		c.mu.Unlock()

		st := c.probe(context.Background(), root)
		if st.Reachable {
			t.Fatalf("iteration %d: want unreachable while the mount is hung, got %+v", i, st)
		}
	}

	if got := entered.Load(); got != 1 {
		t.Errorf("stat entered %d times across 26 probes; want 1 — the parked "+
			"goroutine must suppress every subsequent probe", got)
	}

	// Self-healing: once the kernel releases the stat, the guard clears
	// and a later probe is allowed to test the mount for real.
	release()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		stillParked := c.inflight[root]
		c.mu.Unlock()
		if !stillParked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("in-flight flag never cleared after the stat returned")
		}
		time.Sleep(5 * time.Millisecond)
	}

	c.mu.Lock()
	if e, ok := c.entries[root]; ok {
		e.checkedAt = time.Now().Add(-2 * reachabilityTTL)
		c.entries[root] = e
	}
	c.mu.Unlock()

	_ = c.probe(context.Background(), root)
	if got := entered.Load(); got != 2 {
		t.Errorf("after the mount unblocked, stat entered %d times; want 2 "+
			"(the guard must not latch permanently)", got)
	}
}

// TestReachabilityProbe_InflightGuardIsPerRoot pins that a hung root
// doesn't suppress probing of a healthy sibling — multi-root installs
// where one NAS is down must still report the local disk correctly.
func TestReachabilityProbe_InflightGuardIsPerRoot(t *testing.T) {
	hung, _, _ := hangingStat(t)
	healthy := t.TempDir()

	orig := statFunc
	statFunc = func(p string) (os.FileInfo, error) {
		if p == healthy {
			return orig(p)
		}
		return hung(p)
	}
	t.Cleanup(func() { statFunc = orig })

	c := newReachabilityCache()

	if st := c.probe(context.Background(), "/mnt/hung-nfs"); st.Reachable {
		t.Fatalf("hung root must be unreachable, got %+v", st)
	}
	if st := c.probe(context.Background(), healthy); !st.Reachable {
		t.Errorf("healthy root must stay reachable while a sibling is hung, got %+v", st)
	}
}
