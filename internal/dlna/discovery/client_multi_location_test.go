package discovery

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// One UDN legitimately announces from more than one Location — a dual-homed
// renderer (Wi-Fi + Ethernet) answers an M-SEARCH on both, and duplicate
// responses inside a single cycle are expected (fetchAndCacheDetails' stub
// rationale says so outright). Against a single remembered Location that read
// as a move on EVERY packet: each one Removed the cache entry and re-fetched,
// so `/v1/renderers` intermittently omitted a fully healthy renderer at the
// M-SEARCH cadence — forever, and with no cooldown.
//
// These tests cover the two halves of the fix independently: the entry must
// stay visible WHILE a re-fetch runs, and an A/B/A/B sequence must settle to
// one fetch per distinct address instead of one per packet.

const (
	locA = "http://192.0.2.7:8080/description.xml"
	locB = "http://192.0.2.99:8080/description.xml"

	ctrlA = "http://192.0.2.7:8080/avtransport/control"
	ctrlB = "http://192.0.2.99:8080/avtransport/control"
)

// descriptionFetchCounter serves the Chord description and counts
// device-description GETs — the request every detail fetch opens with. The
// follow-on GetProtocolInfo POST belongs to the same fetch, so counting every
// request would conflate the two.
type descriptionFetchCounter struct {
	gets atomic.Int32
}

func (d *descriptionFetchCounter) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		d.gets.Add(1)
	}
	rec := httptest.NewRecorder()
	descriptionOnlyHandler(rec, req)
	return rec.Result(), nil
}

// newSteppableClient builds a client whose clock the test advances by hand,
// so the RendererTTL freshness window can be crossed without sleeping.
// Returns the clock (UnixNano) and its starting instant.
func newSteppableClient(t *testing.T, disp SOAPDispatcher) (*SSDPDiscoveryClient, *atomic.Int64, time.Time) {
	t.Helper()
	base := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	clock := &atomic.Int64{}
	clock.Store(base.UnixNano())

	cfg := DefaultDiscoveryConfig()
	cfg.Interface = &net.Interface{}
	cfg.Dispatcher = disp
	cfg.NowFunc = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	c, err := NewSSDPDiscoveryClient(cfg, NewRendererCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return c, clock, base
}

// mustBeVisible asserts the renderer is the single row `/v1/renderers` would
// serve right now, and returns it.
func mustBeVisible(t *testing.T, c *SSDPDiscoveryClient, when string) RendererInfo {
	t.Helper()
	snap := c.cache.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d %s, want 1 — the renderer must not blink out of /v1/renderers", len(snap), when)
	}
	return snap[0]
}

func TestHandlePacket_MoveRefetchKeepsRendererVisible(t *testing.T) {
	// A re-fetch triggered by a genuinely new address must not take the
	// renderer out of the output picker while it runs. Pre-fix handlePacket
	// called cache.Remove SYNCHRONOUSLY before dispatching, so the entry was
	// gone the moment handlePacket returned — and stayed gone for the fetch's
	// whole duration (a failed fetch widened that to a full M-SEARCH cycle,
	// since Snapshot also hides ControlURL-less stubs). iOS re-reading
	// /v1/renderers in that window loses the row mid-selection.
	//
	// The gate dispatcher holds the fetch, so this is a fact about the cache
	// DURING the re-fetch rather than a race with an in-process round trip.
	disp := &gateDispatcher{release: make(chan struct{})}
	t.Cleanup(disp.open) // failure-path net: never leave the fetch parked
	c := newTestClient(t, disp)
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   ctrlA,
		LastSeenAt:   time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC),
	})
	c.recordLocation(movedUDN, locA)

	c.handlePacket(context.Background(), alivePacket(movedUDN, locB), nil)

	info := mustBeVisible(t, c, "while the re-fetch for the new address is in flight")
	if info.ControlURL != ctrlA {
		t.Errorf("ControlURL = %q mid-re-fetch, want the pre-move %q — the old address is the best answer available until the fetch lands",
			info.ControlURL, ctrlA)
	}

	disp.open()
	c.wg.Wait()

	// …and the move is still honoured once the fetch completes.
	info = mustBeVisible(t, c, "after the re-fetch completed")
	if info.ControlURL != ctrlB {
		t.Errorf("ControlURL = %q, want %q (the re-fetch must still adopt the new host)", info.ControlURL, ctrlB)
	}
	if n := disp.fetches.Load(); n != 1 {
		t.Errorf("description fetches = %d, want 1", n)
	}
}

func TestHandlePacket_TwoLiveAddressesForOneUDNDoNotFlap(t *testing.T) {
	// The A/B/A sequence. Each address is fetched exactly once; after that
	// both count as live, so an announcement from either is just a LastSeenAt
	// refresh. Pre-fix every alternation was a fresh "move": Remove +
	// re-fetch, one per packet, forever.
	disp := &descriptionFetchCounter{}
	c := newTestClient(t, disp)
	ctx := context.Background()

	// A — first sight.
	c.handlePacket(ctx, alivePacket(movedUDN, locA), nil)
	c.wg.Wait()
	if info := mustBeVisible(t, c, "after first discovery at A"); info.ControlURL != ctrlA {
		t.Fatalf("precondition: ControlURL = %q, want %q", info.ControlURL, ctrlA)
	}

	// B — a host we have not seen, so ONE re-fetch is correct…
	c.handlePacket(ctx, alivePacket(movedUDN, locB), nil)
	mustBeVisible(t, c, "while the re-fetch for B is in flight")
	c.wg.Wait()
	if info := mustBeVisible(t, c, "after the re-fetch for B"); info.ControlURL != ctrlB {
		t.Fatalf("ControlURL = %q, want %q", info.ControlURL, ctrlB)
	}

	// A again — the renderer is still answering there, so this is a second
	// live address, NOT a move back.
	c.handlePacket(ctx, alivePacket(movedUDN, locA), nil)
	mustBeVisible(t, c, "after A announces again")
	// …and B again, to prove it settles rather than alternating.
	c.handlePacket(ctx, alivePacket(movedUDN, locB), nil)
	mustBeVisible(t, c, "after B announces again")
	c.wg.Wait()

	if n := disp.gets.Load(); n != 2 {
		t.Errorf("description fetches = %d, want 2 (one per distinct address; an A/B/A/B alternation must not re-fetch per packet)", n)
	}
	// The ControlURL stays wherever the last real fetch put it — the point is
	// that no further fetch was dispatched, not which address won.
	if info := mustBeVisible(t, c, "at the end of the alternation"); info.ControlURL != ctrlB {
		t.Errorf("ControlURL = %q, want %q (no fetch ran after B's, so nothing should have changed it)", info.ControlURL, ctrlB)
	}
}

func TestHandlePacket_AddressThatWentQuietIsAMoveWhenItReturns(t *testing.T) {
	// The freshness window is what keeps "two live addresses" from degrading
	// into "never re-fetch a host we have ever seen". An address that stops
	// announcing for longer than RendererTTL is no longer live, so a renderer
	// that really did move away and later came back IS re-fetched — without
	// this the cached ControlURL would point at the address it left.
	disp := &descriptionFetchCounter{}
	c, clock, base := newSteppableClient(t, disp)
	ctx := context.Background()

	c.handlePacket(ctx, alivePacket(movedUDN, locA), nil)
	c.wg.Wait()
	c.handlePacket(ctx, alivePacket(movedUDN, locB), nil)
	c.wg.Wait()
	if info := mustBeVisible(t, c, "after moving to B"); info.ControlURL != ctrlB {
		t.Fatalf("precondition: ControlURL = %q, want %q", info.ControlURL, ctrlB)
	}
	if n := disp.gets.Load(); n != 2 {
		t.Fatalf("precondition: description fetches = %d, want 2", n)
	}

	// Nothing announces for well past RendererTTL (60s), then A comes back.
	clock.Store(base.Add(10 * time.Minute).UnixNano())
	c.handlePacket(ctx, alivePacket(movedUDN, locA), nil)
	mustBeVisible(t, c, "while the re-fetch for the returning address is in flight")
	c.wg.Wait()

	if n := disp.gets.Load(); n != 3 {
		t.Errorf("description fetches = %d, want 3 — an address unseen for longer than RendererTTL is not live, so its return is a move", n)
	}
	if info := mustBeVisible(t, c, "after the returning address was re-fetched"); info.ControlURL != ctrlA {
		t.Errorf("ControlURL = %q, want %q", info.ControlURL, ctrlA)
	}
}

func TestHandlePacket_StructuralStubStillRecoversAfterLongSilence(t *testing.T) {
	// The time-domain twin of TestHandlePacket_StructuralStubRecoversAfterHostChange,
	// and the reason the freshness sweep keeps a floor of one record.
	//
	// A structural stub carries NO ControlURL and NEVER ages out of the cache
	// (year-2999 sentinel), so its recorded Location is the only reference a
	// later announcement can be compared against. If the sweep were allowed to
	// drop that last record after RendererTTL, a renderer that failed
	// structurally at A, went quiet, then came back healthy at B would be
	// stuck nameless and undrivable forever.
	var serveGood atomic.Bool
	disp := &countingDispatcher{handler: func(w http.ResponseWriter, r *http.Request) {
		if !serveGood.Load() {
			w.WriteHeader(http.StatusNotFound) // 4xx → structural
			return
		}
		descriptionOnlyHandler(w, r)
	}}
	c, clock, base := newSteppableClient(t, disp)
	ctx := context.Background()

	c.handlePacket(ctx, alivePacket(movedUDN, locA), nil)
	c.wg.Wait()
	stub, ok := c.cache.Get(movedUDN)
	if !ok || !stub.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Fatalf("precondition: want a structural stub, got %+v (cached=%v)", stub, ok)
	}

	// Silent for far longer than RendererTTL (60s), then back at a new
	// address, healthy this time.
	clock.Store(base.Add(10 * time.Minute).UnixNano())
	serveGood.Store(true)
	c.handlePacket(ctx, alivePacket(movedUDN, locB), nil)
	c.wg.Wait()

	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("entry vanished")
	}
	if info.ControlURL != ctrlB {
		t.Errorf("ControlURL = %q, want %q — the move must still be detected after the freshness window lapsed", info.ControlURL, ctrlB)
	}
	if info.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Error("recovered entry still carries the structural sentinel")
	}
}

func TestNoteLocation_BoundedPerUDN(t *testing.T) {
	// A buggy or spoofed source announcing a fresh host every packet must not
	// grow one UDN's record set without bound. Bounded independently of the
	// freshness window (all records here share one timestamp) and of
	// pruneLocations (which reaps whole UDNs, not records within one).
	c := newTestClient(t, &countingDispatcher{})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	for i := 0; i < maxTrackedLocations*3; i++ {
		// Distinct host per call, oldest first, so the drop-the-oldest rule
		// has a well-defined victim each time.
		c.noteLocation(movedUDN, "http://192.0.2.7:8080/d.xml", now.Add(time.Duration(i)*time.Second))
		c.noteLocation(movedUDN, "http://198.51.100."+string(rune('0'+i%10))+":8080/d.xml", now.Add(time.Duration(i)*time.Second))
	}

	c.locMu.Lock()
	got := len(c.lastLocations[movedUDN])
	c.locMu.Unlock()
	if got > maxTrackedLocations {
		t.Errorf("tracked locations = %d, want <= %d", got, maxTrackedLocations)
	}
	if got == 0 {
		t.Error("tracked locations = 0; the cap must not empty the set")
	}
}
