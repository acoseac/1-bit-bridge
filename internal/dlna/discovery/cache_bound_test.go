package discovery

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// floodRenderers announces n distinct renderer UDNs, each at its own address,
// and joins every fetch: in batches of MaxPendingDetailFetches, so no dispatch
// is refused at the claims bound and every UDN reaches the cache the way a
// slow flood's would.
func floodRenderers(t *testing.T, c *SSDPDiscoveryClient, prefix string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		c.handlePacket(context.Background(), alivePacket(fmt.Sprintf("uuid:%s-%d", prefix, i),
			fmt.Sprintf("http://198.51.100.%d:%d/description.xml", 1+i%250, 8000+i/250)), nil)
		if (i+1)%MaxPendingDetailFetches == 0 {
			c.wg.Wait()
		}
	}
	c.wg.Wait()
}

func (c *SSDPDiscoveryClient) locationRecordCount() int {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	return len(c.lastLocations)
}

func (c *SSDPDiscoveryClient) hasLocationRecord(udn string) bool {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	_, ok := c.lastLocations[udn]
	return ok
}

// notFoundDispatcher answers every request 404, a structural failure.
func notFoundDispatcher() *stubDispatcher {
	return &stubDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}}
}

// TestAFloodOfBrokenRenderersStaysBounded is B47's first item, measured: a
// structural failure cached a stub with a year-2999 LastSeenAt, which no
// eviction pass reached, and a location record beside it, so a flood of
// distinct UDNs whose LOCATION answers 4xx grew both by one per UDN for the
// life of the process (on main, 5,000 of 5,000 left after an eviction pass
// an hour later). The cache now holds MaxCachedDevices, the records at most
// maxLocationUDNs, and both empty once the stubs' hold is over.
func TestAFloodOfBrokenRenderersStaysBounded(t *testing.T) {
	c, clock, base := newSteppableClient(t, notFoundDispatcher())

	const flood = 5000
	floodRenderers(t, c, "broken", flood)
	if got := c.cache.Len(); got > MaxCachedDevices {
		t.Errorf("%d broken renderers left %d cache entries, want at most %d", flood, got, MaxCachedDevices)
	}
	if got := c.locationRecordCount(); got > maxLocationUDNs {
		t.Errorf("%d broken renderers left %d location records, want at most %d", flood, got, maxLocationUDNs)
	}

	clock.Store(base.Add(structuralStubHold + c.cfg.RendererTTL).UnixNano())
	c.evictStaleEntries()
	if got := c.cache.Len(); got != 0 {
		t.Errorf("an eviction pass after the stubs' hold left %d of them, want 0", got)
	}
	if got := c.locationRecordCount(); got != 0 {
		t.Errorf("an eviction pass after the stubs' hold left %d location records, want 0", got)
	}
}

// TestARendererWhoseDescriptionFailedOnceComesBackAfterTheHold pins the
// other face of the year-2999 stub: a real renderer whose description
// answered 404 once, say while it booted, was never fetched again from the
// same address, so it stayed out of /v1/renderers until the bridge restarted
// (measured on main: one GET, and nothing through two hours of healthy
// announcements). Its announcements fetch nothing while the stub holds, and
// the first one after the hold brings it back, under the default TTL and
// under an operator's TTL longer than the hold, which the first form of the
// hold stretched to the whole TTL (CodeRabbit on #1086).
func TestARendererWhoseDescriptionFailedOnceComesBackAfterTheHold(t *testing.T) {
	for _, ttl := range []time.Duration{DefaultDiscoveryConfig().RendererTTL, 20 * time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			var healthy atomic.Bool
			disp := &countingDispatcher{handler: func(w http.ResponseWriter, r *http.Request) {
				if !healthy.Load() {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				descriptionOnlyHandler(w, r)
			}}
			c, clock, base := newSteppableClient(t, disp)
			c.cfg.RendererTTL = ttl
			announceAt := func(at time.Time) {
				clock.Store(at.UnixNano())
				c.evictStaleEntries()
				c.handlePacket(context.Background(), alivePacket(movedUDN, locA), nil)
				c.wg.Wait()
			}
			announceAt(base)
			healthy.Store(true)
			for at := base.Add(30 * time.Second); at.Before(base.Add(structuralStubHold)); at = at.Add(30 * time.Second) {
				announceAt(at)
			}
			if got := disp.calls.Load(); got != 1 {
				t.Errorf("%d description fetches while the stub held, want the 1 that failed", got)
			}
			if n := len(c.cache.Snapshot()); n != 0 {
				t.Fatalf("precondition: %d renderers served while the stub held, want 0", n)
			}

			announceAt(base.Add(structuralStubHold + c.cfg.MSearchInterval))
			if info := mustBeVisible(t, c, "after the stub's hold"); info.ControlURL != ctrlA {
				t.Errorf("ControlURL = %q, want %q", info.ControlURL, ctrlA)
			}
		})
	}
}

// TestAStructuralStubGoesAtTheHoldWhateverTheTTL pins the hold against the
// TTL, which dlna.discovery.rendererTTLSeconds lets an operator set up to a
// year. Under a TTL longer than the hold, the first form of
// structuralStubLastSeen stamped the failure time itself, so EvictStale kept
// the stub for the whole TTL rather than the hold (CodeRabbit on #1086). A
// TTL equal to the hold is the boundary where the two forms agree.
func TestAStructuralStubGoesAtTheHoldWhateverTheTTL(t *testing.T) {
	for _, ttl := range []time.Duration{time.Minute, structuralStubHold, 20 * time.Minute, 365 * 24 * time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			c, clock, base := newSteppableClient(t, notFoundDispatcher())
			c.cfg.RendererTTL = ttl
			c.handlePacket(context.Background(), alivePacket(movedUDN, locA), nil)
			c.wg.Wait()
			if _, ok := c.cache.Get(movedUDN); !ok {
				t.Fatal("precondition: the failed fetch cached no stub")
			}

			clock.Store(base.Add(structuralStubHold - time.Second).UnixNano())
			c.evictStaleEntries()
			if _, ok := c.cache.Get(movedUDN); !ok {
				t.Error("the stub went a second before its hold ended")
			}
			clock.Store(base.Add(structuralStubHold + time.Second).UnixNano())
			c.evictStaleEntries()
			if _, ok := c.cache.Get(movedUDN); ok {
				t.Error("the stub outlived its hold")
			}
		})
	}
}

// TestAFloodNeverDisplacesACachedRenderer pins what the bound must not cost:
// a flood's stubs make room only by evicting each other, so the renderer
// /v1/renderers serves stays, and stays served.
func TestAFloodNeverDisplacesACachedRenderer(t *testing.T) {
	c, _, base := newSteppableClient(t, notFoundDispatcher())
	c.cache.Replace(RendererInfo{UDN: movedUDN, FriendlyName: "Chord 2go", ControlURL: ctrlA, LastSeenAt: base})

	floodRenderers(t, c, "broken", 5000)
	snap := c.cache.Snapshot()
	if len(snap) != 1 || snap[0].UDN != movedUDN {
		t.Fatalf("after a flood of 5,000 broken renderers /v1/renderers serves %v, want the one renderer cached before it", snap)
	}
	if got := c.cache.Len(); got != MaxCachedDevices {
		t.Errorf("the cache holds %d entries, want its bound, %d: the renderer and the newest stubs", got, MaxCachedDevices)
	}
}

// TestACacheFullOfRenderersRefusesANewOne pins the other side: once the cache
// holds MaxCachedDevices renderers it serves, a new UDN is refused, however
// healthy, rather than displacing one of them, and its fetch leaves no
// location record behind. When one of them goes, the next announcement is
// cached.
func TestACacheFullOfRenderersRefusesANewOne(t *testing.T) {
	disp := &countingDispatcher{handler: descriptionOnlyHandler}
	c, _, _ := newSteppableClient(t, disp)
	floodRenderers(t, c, "served", MaxCachedDevices)
	if got := len(c.cache.Snapshot()); got != MaxCachedDevices {
		t.Fatalf("precondition: %d renderers served, want %d", got, MaxCachedDevices)
	}

	const newcomer = "uuid:newcomer"
	announce := func() {
		c.handlePacket(context.Background(), alivePacket(newcomer, locB), nil)
		c.wg.Wait()
	}
	before := disp.calls.Load()
	announce()
	if disp.calls.Load() == before {
		t.Fatal("precondition: the new renderer's announcement fetched nothing")
	}
	if _, ok := c.cache.Get(newcomer); ok {
		t.Error("a cache full of served renderers took a new one")
	}
	if got := len(c.cache.Snapshot()); got != MaxCachedDevices {
		t.Errorf("%d renderers served after the refusal, want every one of the %d", got, MaxCachedDevices)
	}
	if c.hasLocationRecord(newcomer) {
		t.Error("the refused renderer's fetch left its location record behind")
	}

	c.cache.Remove("uuid:served-0") // a renderer leaves (ssdp:byebye)
	announce()
	if _, ok := c.cache.Get(newcomer); !ok {
		t.Error("the new renderer's next announcement, with room made, was not cached")
	}
}

// TestAStubMakesRoomInTheOrderItWouldExpire pins which stub a full cache
// evicts to take a new UDN: the one EvictStale would drop first. Under the
// default TTL a transient stub (stamped with its fail time, so it goes a TTL
// later) goes before a structural one (stamped to go structuralStubHold after
// its failure), and once only renderers the cache serves are left, a new UDN,
// stub or renderer, is refused.
func TestAStubMakesRoomInTheOrderItWouldExpire(t *testing.T) {
	c := NewRendererCache()
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < MaxCachedDevices-2; i++ {
		c.Replace(RendererInfo{UDN: fmt.Sprintf("uuid:served-%d", i), ControlURL: ctrlA, LastSeenAt: base})
	}
	c.Replace(RendererInfo{UDN: "uuid:structural", LastSeenAt: structuralStubLastSeen(base, time.Minute)})
	c.Replace(RendererInfo{UDN: "uuid:transient", LastSeenAt: base.Add(time.Minute)})

	admit := func(info RendererInfo, evicts string) {
		t.Helper()
		if !c.Replace(info) {
			t.Fatalf("a full cache still holding stubs refused %s", info.UDN)
		}
		if _, ok := c.Get(evicts); ok {
			t.Errorf("%s was admitted and %s, the stub that expires first, was kept", info.UDN, evicts)
		}
		if got := c.Len(); got != MaxCachedDevices {
			t.Errorf("the cache holds %d entries after admitting %s, want its bound, %d", got, info.UDN, MaxCachedDevices)
		}
	}
	admit(RendererInfo{UDN: "uuid:new-stub", LastSeenAt: base}, "uuid:transient")
	admit(RendererInfo{UDN: "uuid:new-renderer", ControlURL: ctrlB, LastSeenAt: base}, "uuid:new-stub")
	admit(RendererInfo{UDN: "uuid:another-renderer", ControlURL: ctrlB, LastSeenAt: base}, "uuid:structural")

	if c.Replace(RendererInfo{UDN: "uuid:refused-stub", LastSeenAt: base}) {
		t.Error("a cache full of served renderers took a new stub")
	}
	if c.Replace(RendererInfo{UDN: "uuid:refused-renderer", ControlURL: ctrlB, LastSeenAt: base}) {
		t.Error("a cache full of served renderers took a new renderer")
	}
	if !c.Replace(RendererInfo{UDN: "uuid:served-0", ControlURL: ctrlB, LastSeenAt: base.Add(time.Minute)}) {
		t.Error("a full cache refused to replace a renderer it already holds")
	}
	if got := len(c.Snapshot()); got != MaxCachedDevices {
		t.Errorf("%d renderers served, want %d: no served renderer is evicted to make room", got, MaxCachedDevices)
	}
}

// TestAStubMakesRoomInTheOrderItWouldExpireUnderALongTTL is the same order
// under a TTL longer than structuralStubHold, where a structural stub is
// stamped BEFORE its failure: it expires first even when it failed after a
// transient stub, so a full cache evicts it first. With the stamp clamped to
// the failure, the transient stub went and the structural one stayed for
// the whole TTL (CodeRabbit on #1086).
func TestAStubMakesRoomInTheOrderItWouldExpireUnderALongTTL(t *testing.T) {
	const ttl = 20 * time.Minute
	c := NewRendererCache()
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for i := 0; i < MaxCachedDevices-2; i++ {
		c.Replace(RendererInfo{UDN: fmt.Sprintf("uuid:served-%d", i), ControlURL: ctrlA, LastSeenAt: base})
	}
	c.Replace(RendererInfo{UDN: "uuid:transient", LastSeenAt: base})
	c.Replace(RendererInfo{UDN: "uuid:structural", LastSeenAt: structuralStubLastSeen(base.Add(time.Minute), ttl)})

	if !c.Replace(RendererInfo{UDN: "uuid:new-stub", LastSeenAt: base}) {
		t.Fatal("a full cache still holding stubs refused a new UDN")
	}
	if _, ok := c.Get("uuid:structural"); ok {
		t.Error("the structural stub, which expires first, was kept")
	}
	if _, ok := c.Get("uuid:transient"); !ok {
		t.Error("the transient stub was evicted, and the structural one, which expires first, kept")
	}
}
