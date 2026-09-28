package upnp

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// heldDispatcher holds every description fetch until open is called, then
// serves recordingDispatcher's description (its ContentDirectory on the
// fetched host) and counts it there. A fetch still held when its context
// ends returns that error.
type heldDispatcher struct {
	recordingDispatcher
	release chan struct{}
	once    sync.Once
}

func newHeldDispatcher(t *testing.T) *heldDispatcher {
	t.Helper()
	d := &heldDispatcher{release: make(chan struct{})}
	t.Cleanup(d.open) // failure-path net: never leave a fetch parked
	return d
}

func (d *heldDispatcher) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	select {
	case <-d.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.recordingDispatcher.Do(ctx, req)
}

func (d *heldDispatcher) open() { d.once.Do(func() { close(d.release) }) }

// inFlightCount is how many detail fetches the client holds claimed.
func (c *MediaServerDiscoveryClient) inFlightCount() int {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	return len(c.inFlight)
}

// announceFrom hands c one MediaServer alive for udn at location, as the
// read loop does. No run loop is started by these tests, so c.wg counts only
// the fetches the announcements dispatch, and Wait joins exactly those.
func announceFrom(c *MediaServerDiscoveryClient, udn, location string) {
	c.handlePacket(context.Background(), alivePacket(udn, location), nil)
}

// TestHandlePacket_BurstForOneServerDispatchesOneFetch pins the dedup the
// renderer client already had. A fetch publishes nothing until it returns,
// so every packet of a new server's burst lands in the first-time branch,
// and each used to spawn a fetch of its own, queued on the two-slot
// semaphore: measured on 2026-09-28, 1,000 packets for one UDN cost 1,000
// goroutines and, once they ran, 1,000 GETs of the same description.
func TestHandlePacket_BurstForOneServerDispatchesOneFetch(t *testing.T) {
	disp := newHeldDispatcher(t)
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	for range 50 {
		announceFrom(c, "uuid:burst", "http://192.0.2.7:8200/desc.xml")
	}
	if got := c.inFlightCount(); got != 1 {
		t.Fatalf("%d fetches claimed for one server's burst of 50, want 1", got)
	}
	disp.open()
	c.wg.Wait()
	if got := disp.fetchCount(); got != 1 {
		t.Errorf("one server's burst of 50 cost %d description fetches, want 1", got)
	}
	if got := c.inFlightCount(); got != 0 {
		t.Errorf("%d claims left after the fetch returned, want 0", got)
	}
	if _, ok := cache.Get("uuid:burst"); !ok {
		t.Error("the one fetch did not cache the server")
	}
}

// TestHandlePacket_FloodOfNewServersHoldsAtMostTheBound pins the bound the
// dedup alone does not give: a flood of DISTINCT UDNs, which a LAN peer
// that knows the M-SEARCH's source port can send, cost a goroutine per UDN
// (10,000 goroutines and 35 MiB of stack for 10,000, measured 2026-09-28).
// At the bound a dispatch is dropped, not refused for good: the server's
// next announcement is fetched.
func TestHandlePacket_FloodOfNewServersHoldsAtMostTheBound(t *testing.T) {
	disp := newHeldDispatcher(t)
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	const flood = 1000
	before := runtime.NumGoroutine()
	for i := range flood {
		announceFrom(c, fmt.Sprintf("uuid:server-%d", i), "http://192.0.2.7:8200/desc.xml")
	}
	grew := runtime.NumGoroutine() - before
	if got := c.inFlightCount(); got != discovery.MaxPendingDetailFetches {
		t.Errorf("%d fetches claimed after %d new servers, want the bound, %d",
			got, flood, discovery.MaxPendingDetailFetches)
	}
	// A small allowance for goroutines of the runtime and of earlier tests
	// that come and go; the unbounded client grew by the whole flood.
	if grew > discovery.MaxPendingDetailFetches+8 {
		t.Errorf("%d new servers grew the goroutine count by %d, want at most the bound, %d",
			flood, grew, discovery.MaxPendingDetailFetches)
	}

	disp.open()
	c.wg.Wait()
	if got := cache.Len(); got != discovery.MaxPendingDetailFetches {
		t.Errorf("%d servers cached after the flood drained, want the %d claimed", got, discovery.MaxPendingDetailFetches)
	}
	const dropped = "uuid:server-999"
	announceFrom(c, dropped, "http://192.0.2.7:8200/desc.xml")
	c.wg.Wait()
	if _, ok := cache.Get(dropped); !ok {
		t.Errorf("%s's next announcement was not fetched after the flood drained", dropped)
	}
}

// TestHandlePacket_MovedServerRefetchesOnceForABurst pins the move detector
// through the claim: a server that moved re-fetches its description once for
// a burst from its new address (every packet reads as a move until that
// fetch records the address), and not again once it has.
func TestHandlePacket_MovedServerRefetchesOnceForABurst(t *testing.T) {
	disp := newHeldDispatcher(t)
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	cache.Upsert(ServerInfo{
		UDN:                        "uuid:ms",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		LastSeenAt:                 time.Now(),
	})
	c.recordLocation("uuid:ms", "http://192.0.2.7:8200/desc.xml")

	const moved = "http://192.0.2.99:8200/desc.xml"
	for range 20 {
		announceFrom(c, "uuid:ms", moved)
	}
	disp.open()
	c.wg.Wait()
	if got := disp.fetchCount(); got != 1 {
		t.Errorf("a moved server's burst of 20 cost %d re-fetches, want 1", got)
	}
	if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != "http://192.0.2.99:8200/ctl/ContentDir" {
		t.Fatalf("the control URL did not follow the move: %q", info.ContentDirectoryControlURL)
	}
	announceFrom(c, "uuid:ms", moved)
	c.wg.Wait()
	if got := disp.fetchCount(); got != 1 {
		t.Errorf("an announcement from the recorded address re-fetched (%d fetches), want none", got)
	}
}

// TestHandlePacket_FailedRefetchFreesItsClaim pins the claim's release on a
// fetch that fails: the move is not recorded, so the next announcement from
// the new address must dispatch again, which it cannot while a claim from
// the failed fetch is still held.
func TestHandlePacket_FailedRefetchFreesItsClaim(t *testing.T) {
	disp := &failingDispatcher{}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	cache.Upsert(ServerInfo{
		UDN:                        "uuid:ms",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		LastSeenAt:                 time.Now(),
	})
	c.recordLocation("uuid:ms", "http://192.0.2.7:8200/desc.xml")

	for attempt := 1; attempt <= 2; attempt++ {
		announceFrom(c, "uuid:ms", "http://192.0.2.99:8200/desc.xml")
		c.wg.Wait()
		if got := disp.fetchCount(); got != attempt {
			t.Fatalf("after announcement %d from the new address, %d re-fetches, want %d", attempt, got, attempt)
		}
		if got := c.inFlightCount(); got != 0 {
			t.Fatalf("a failed re-fetch left %d claims held", got)
		}
	}
}
