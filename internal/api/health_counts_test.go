package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHealthCountsProvider counts TracksIndexed calls and can block inside it,
// so a flood of concurrent callers can be piled onto the singleflight before
// the first fetch completes.
type fakeHealthCountsProvider struct {
	calls   atomic.Int64
	tracks  int
	pending int64
	block   chan struct{} // if non-nil, TracksIndexed blocks until it's closed
}

func (f *fakeHealthCountsProvider) TracksIndexed(context.Context) int {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.tracks
}

func (f *fakeHealthCountsProvider) PendingDeletions(context.Context) int64 {
	return f.pending
}

// TestHealthCountsCache_CollapsesConcurrentFloodToOneFetch pins A1's core
// claim: an unauthenticated /v1/health flood arriving at the expiry boundary
// must collapse to ONE COUNT(*) fetch, not one per request.
func TestHealthCountsCache_CollapsesConcurrentFloodToOneFetch(t *testing.T) {
	block := make(chan struct{})
	p := &fakeHealthCountsProvider{tracks: 42, pending: 7, block: block}
	c := newHealthCountsCache()

	const n = 50
	type result struct {
		tracks  int
		pending int64
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tr, pd := c.counts(context.Background(), p)
			results[i] = result{tracks: tr, pending: pd}
		}(i)
	}
	// Let the goroutines pile onto the in-flight fetch, then release it.
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()

	if got := p.calls.Load(); got != 1 {
		t.Errorf("TracksIndexed called %d times under a %d-caller flood; want 1 (singleflight collapse)", got, n)
	}
	for i := range results {
		if results[i].tracks != 42 || results[i].pending != 7 {
			t.Fatalf("caller %d got (%d,%d); want (42,7)", i, results[i].tracks, results[i].pending)
		}
	}
}

// TestHealthCountsCache_ServesFromCacheWithinTTL confirms a second call within
// the TTL window is served from the snapshot (no new fetch).
func TestHealthCountsCache_ServesFromCacheWithinTTL(t *testing.T) {
	p := &fakeHealthCountsProvider{tracks: 5, pending: 3}
	c := newHealthCountsCache()
	if tr, pd := c.counts(context.Background(), p); tr != 5 || pd != 3 {
		t.Fatalf("first call = (%d,%d); want (5,3)", tr, pd)
	}
	if tr, pd := c.counts(context.Background(), p); tr != 5 || pd != 3 {
		t.Fatalf("second call = (%d,%d); want (5,3)", tr, pd)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("provider called %d times within TTL; want 1", got)
	}
}

// TestHealthCountsCache_NilSafe covers the &Server{} test-harness path.
func TestHealthCountsCache_NilSafe(t *testing.T) {
	var c *healthCountsCache
	if tr, pd := c.counts(context.Background(), nil); tr != 0 || pd != 0 {
		t.Errorf("nil cache/provider = (%d,%d); want (0,0)", tr, pd)
	}
	live := newHealthCountsCache()
	if tr, pd := live.counts(context.Background(), nil); tr != 0 || pd != 0 {
		t.Errorf("nil provider = (%d,%d); want (0,0)", tr, pd)
	}
}
