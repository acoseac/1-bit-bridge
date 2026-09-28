package api

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// hangingStat returns a stat stand-in that parks until released, a counter
// of how many times it was entered, and the release. The caller puts the
// stand-in on c (c.stat), where only this test's probes can reach it, so
// nothing is ever put back. The cleanup registered here releases the parked
// stats and waits until c has no stat goroutine running, so none outlives
// the test, and a stat that never returns fails it rather than leaking.
func hangingStat(t *testing.T, c *reachabilityCache) (fn func(string) (os.FileInfo, error), entered *atomic.Int64, release func()) {
	t.Helper()
	var n atomic.Int64
	gate := make(chan struct{})
	var closed atomic.Bool
	rel := func() {
		if closed.CompareAndSwap(false, true) {
			close(gate)
		}
	}
	t.Cleanup(func() {
		rel()
		if !statsReturned(c, 2*time.Second) {
			t.Errorf("a probe's stat had not returned 2s after its release")
		}
	})
	return func(string) (os.FileInfo, error) {
		n.Add(1)
		<-gate
		return nil, os.ErrNotExist
	}, &n, rel
}

// statsReturned polls c.inflight under c.mu until no stat goroutine c
// started is still running, and reports whether that happened within the
// given time. Every such goroutine deletes its flag under c.mu once its
// stat returns, so seeing the map empty means each has finished its stat.
func statsReturned(c *reachabilityCache, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		c.mu.Lock()
		running := len(c.inflight)
		c.mu.Unlock()
		if running == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	c := newReachabilityCache()
	fn, entered, release := hangingStat(t, c)
	c.stat = fn
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
	if !statsReturned(c, 2*time.Second) {
		t.Fatal("in-flight flag never cleared after the stat returned")
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
	c := newReachabilityCache()
	healthy := t.TempDir()
	hung, _, _ := hangingStat(t, c)
	c.stat = func(p string) (os.FileInfo, error) {
		if p == healthy {
			return os.Stat(p)
		}
		return hung(p)
	}

	if st := c.probe(context.Background(), "/mnt/hung-nfs"); st.Reachable {
		t.Fatalf("hung root must be unreachable, got %+v", st)
	}
	if st := c.probe(context.Background(), healthy); !st.Reachable {
		t.Errorf("healthy root must stay reachable while a sibling is hung, got %+v", st)
	}
}
