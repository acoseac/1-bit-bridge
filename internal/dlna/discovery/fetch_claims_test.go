package discovery

import (
	"context"
	"fmt"
	"runtime"
	"testing"
)

// TestDetailFetchClaimsDeduplicateAndBound pins the two refusals both
// discovery clients rely on: a second claim for a UDN that holds one, and
// any claim once MaxPendingDetailFetches are held. A refused claim changes
// nothing, and a release makes room again.
func TestDetailFetchClaimsDeduplicateAndBound(t *testing.T) {
	f := make(DetailFetchClaims)
	if !f.Claim("uuid:a") || f.Claim("uuid:a") {
		t.Fatal("the first claim for a UDN must succeed and the second be refused")
	}
	for i := 1; i < MaxPendingDetailFetches; i++ {
		if !f.Claim(fmt.Sprintf("uuid:%d", i)) {
			t.Fatalf("claim %d refused below the bound of %d", i+1, MaxPendingDetailFetches)
		}
	}
	if f.Claim("uuid:one-too-many") {
		t.Fatalf("a claim past MaxPendingDetailFetches (%d) was granted", MaxPendingDetailFetches)
	}
	if f.Held("uuid:one-too-many") || len(f) != MaxPendingDetailFetches {
		t.Fatalf("a refused claim changed the set: %d held", len(f))
	}
	f.Release("uuid:a")
	if f.Held("uuid:a") || !f.Claim("uuid:one-too-many") {
		t.Error("a release did not make room for the next claim")
	}
}

// TestHandlePacket_FloodOfNewRenderersHoldsAtMostTheBound is the renderer
// half of the flood bound. Its per-UDN claim already collapsed a burst for
// ONE renderer to one fetch, but every distinct UDN took a claim of its own,
// and each was a goroutine queued for the four-slot semaphore: measured on
// 2026-09-28, 10,000 distinct UDNs cost 10,000 goroutines and 36 MiB of
// stack, while the fetches of the first four held the rest back.
func TestHandlePacket_FloodOfNewRenderersHoldsAtMostTheBound(t *testing.T) {
	disp := &gateDispatcher{release: make(chan struct{})}
	t.Cleanup(disp.open) // failure-path net: never leave a fetch parked
	c := newTestClient(t, disp)
	announce := func(udn string) {
		c.handlePacket(context.Background(), alivePacket(udn, "http://192.0.2.7:8080/description.xml"), nil)
	}

	const flood = 1000
	before := runtime.NumGoroutine()
	for i := 0; i < flood; i++ {
		announce(fmt.Sprintf("uuid:flood-%d", i))
	}
	grew := runtime.NumGoroutine() - before
	if got := c.inFlightCount(); got != MaxPendingDetailFetches {
		t.Errorf("%d fetches claimed after %d new renderers, want the bound, %d", got, flood, MaxPendingDetailFetches)
	}
	// A small allowance for goroutines of the runtime and of earlier tests
	// that come and go; the unbounded client grew by the whole flood.
	if grew > MaxPendingDetailFetches+8 {
		t.Errorf("%d new renderers grew the goroutine count by %d, want at most the bound, %d",
			flood, grew, MaxPendingDetailFetches)
	}

	disp.open()
	c.wg.Wait() // the only Adds are this test's fetches; the run loops aren't started
	if got := c.inFlightCount(); got != 0 {
		t.Errorf("%d claims left after the fetches returned, want 0", got)
	}
	// A dropped dispatch refuses nothing for good: the renderer's next
	// announcement is fetched.
	const dropped = "uuid:flood-999"
	announce(dropped)
	c.wg.Wait()
	if _, ok := c.cache.Get(dropped); !ok {
		t.Errorf("%s's next announcement was not fetched after the flood drained", dropped)
	}
	if n := disp.fetches.Load(); n != MaxPendingDetailFetches+1 {
		t.Errorf("%d description fetches, want the %d claimed plus the one re-announced", n, MaxPendingDetailFetches)
	}
}
