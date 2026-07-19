package api

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUPnPPublicProvider counts PublicServers calls and can block inside the
// call, so a flood of concurrent callers can be piled onto the singleflight
// before the first fetch completes. Each PublicServers call stands in for the
// per-server `SELECT COUNT(*) FROM upnp_track_routing` fan-out B12 protects.
type fakeUPnPPublicProvider struct {
	calls   atomic.Int64
	servers []UPnPUpstreamPublicServer
	block   chan struct{} // if non-nil, PublicServers blocks until it's closed
}

func (f *fakeUPnPPublicProvider) PublicServers(context.Context) []UPnPUpstreamPublicServer {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.servers
}

// TestPublicServersCache_CollapsesConcurrentFloodToOneFetch pins B12's core
// claim: an unauthenticated /v1/health flood arriving at the expiry boundary
// must collapse to ONE PublicServers fetch (and thus ONE per-server COUNT(*)
// fan-out), not one per request.
func TestPublicServersCache_CollapsesConcurrentFloodToOneFetch(t *testing.T) {
	block := make(chan struct{})
	want := []UPnPUpstreamPublicServer{
		{Name: "Chord 2Go", PathPrefix: "2go", RoutedTracks: 15283},
	}
	p := &fakeUPnPPublicProvider{servers: want, block: block}
	c := newPublicServersCache()

	const n = 50
	results := make([][]UPnPUpstreamPublicServer, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.servers(context.Background(), p)
		}(i)
	}
	// Let the goroutines pile onto the in-flight fetch, then release it.
	time.Sleep(50 * time.Millisecond)
	close(block)
	wg.Wait()

	if got := p.calls.Load(); got != 1 {
		t.Errorf("PublicServers called %d times under a %d-caller flood; want 1 (singleflight collapse)", got, n)
	}
	for i := range results {
		if len(results[i]) != 1 || results[i][0].Name != "Chord 2Go" || results[i][0].RoutedTracks != 15283 {
			t.Fatalf("caller %d got %+v; want one Chord 2Go row", i, results[i])
		}
	}
}

// TestPublicServersCache_ServesFromCacheWithinTTL confirms a second call within
// the TTL window is served from the snapshot (no new fetch).
func TestPublicServersCache_ServesFromCacheWithinTTL(t *testing.T) {
	p := &fakeUPnPPublicProvider{servers: []UPnPUpstreamPublicServer{{Name: "A", PathPrefix: "a"}}}
	c := newPublicServersCache()
	if got := c.servers(context.Background(), p); len(got) != 1 || got[0].Name != "A" {
		t.Fatalf("first call = %+v; want one row named A", got)
	}
	if got := c.servers(context.Background(), p); len(got) != 1 || got[0].Name != "A" {
		t.Fatalf("second call = %+v; want one row named A", got)
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("provider called %d times within TTL; want 1", got)
	}
}

// TestPublicServersCache_RefreshesAfterTTL confirms the snapshot expires: a
// call after the TTL window drives a fresh provider fetch.
func TestPublicServersCache_RefreshesAfterTTL(t *testing.T) {
	p := &fakeUPnPPublicProvider{servers: []UPnPUpstreamPublicServer{{Name: "A", PathPrefix: "a"}}}
	c := newPublicServersCache()
	_ = c.servers(context.Background(), p)
	// Age the snapshot past the TTL without sleeping the whole window.
	c.mu.Lock()
	c.snap.fetchedAt = time.Now().Add(-2 * publicServersTTL)
	c.mu.Unlock()
	_ = c.servers(context.Background(), p)
	if got := p.calls.Load(); got != 2 {
		t.Errorf("provider called %d times across the TTL boundary; want 2 (one per window)", got)
	}
}

// TestHealth_UPnPUpstreamServers_HandlerUsesTTLCache locks the wiring: the
// /v1/health handler must route the UPnP-upstream advertisement through
// publicServersCache, so repeated polls within the TTL share one PublicServers
// fetch. Catches a future refactor that reverts to a per-request
// s.upnpPublicProvider.PublicServers(...) call. Uses newUPnPHealthTestServer /
// fetchUPnPHealthBody from api_test.go (same package).
func TestHealth_UPnPUpstreamServers_HandlerUsesTTLCache(t *testing.T) {
	p := &fakeUPnPPublicProvider{
		servers: []UPnPUpstreamPublicServer{{Name: "2Go", PathPrefix: "2go", RoutedTracks: 15283}},
	}
	hs, _ := newUPnPHealthTestServer(t, p)
	defer hs.Close()

	// Sequential hits within the TTL window (no goroutines → no cross-goroutine
	// t.Fatal hazard) must still flow the wire shape while collapsing to one
	// underlying fetch.
	for i := 0; i < 5; i++ {
		var resp HealthResponse
		if err := json.Unmarshal(fetchUPnPHealthBody(t, hs), &resp); err != nil {
			t.Fatalf("call %d: decode /v1/health: %v", i, err)
		}
		if len(resp.UPnPUpstreamServers) != 1 || resp.UPnPUpstreamServers[0].PathPrefix != "2go" {
			t.Fatalf("call %d: UPnPUpstreamServers = %+v; want one 2go row", i, resp.UPnPUpstreamServers)
		}
	}
	if got := p.calls.Load(); got != 1 {
		t.Errorf("PublicServers called %d times across 5 sequential /v1/health hits; want 1 (handler must use publicServersCache)", got)
	}
}

// TestPublicServersCache_NilSafe covers the &Server{} test-harness path and
// the pre-feature "no provider wired" shape (nil in → nil out).
func TestPublicServersCache_NilSafe(t *testing.T) {
	var c *publicServersCache
	if got := c.servers(context.Background(), nil); got != nil {
		t.Errorf("nil cache/provider = %+v; want nil", got)
	}
	live := newPublicServersCache()
	if got := live.servers(context.Background(), nil); got != nil {
		t.Errorf("nil provider = %+v; want nil", got)
	}
}
