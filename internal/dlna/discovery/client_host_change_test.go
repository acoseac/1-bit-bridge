package discovery

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// sameURLHost truth table
// -----------------------------------------------------------------------------

func TestSameURLHost(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "http://192.0.2.7:8080/desc.xml", "http://192.0.2.7:8080/desc.xml", true},
		{"same host different path", "http://192.0.2.7:8080/desc.xml", "http://192.0.2.7:8080/avt/control", true},
		{"same host different scheme", "http://192.0.2.7:8080/x", "https://192.0.2.7:8080/x", true},
		{"different host", "http://192.0.2.7:8080/x", "http://192.0.2.9:8080/x", false},
		{"different port", "http://192.0.2.7:8080/x", "http://192.0.2.7:9090/x", false},
		{"port added", "http://192.0.2.7/x", "http://192.0.2.7:8080/x", false},
		{"hostname vs ip", "http://renderer.local:80/x", "http://192.0.2.7:80/x", false},
		// Unparseable input MUST compare as "same" — a malformed SSDP
		// Location can't be allowed to trigger a re-fetch storm against a
		// healthy entry.
		{"unparseable a", "://nope", "http://192.0.2.7/x", true},
		{"unparseable b", "http://192.0.2.7/x", "://nope", true},
		{"both unparseable", "://nope", "://also-nope", true},
		{"empty vs empty", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameURLHost(tc.a, tc.b); got != tc.want {
				t.Errorf("sameURLHost(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// countingDispatcher records how many fetches were dispatched and serves a
// caller-supplied handler. `calls` is atomic because the fetch runs on its
// own goroutine while the test body reads the counter.
type countingDispatcher struct {
	calls   atomic.Int32
	handler func(w http.ResponseWriter, r *http.Request)
}

func (d *countingDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.calls.Add(1)
	if d.handler == nil {
		return nil, errors.New("countingDispatcher: fetch not expected")
	}
	rec := httptest.NewRecorder()
	d.handler(rec, req)
	return rec.Result(), nil
}

// descriptionOnlyHandler serves the Chord device description for GETs and a
// GetProtocolInfo SOAP body for POSTs — the shape a real renderer answers
// with, so the fetch resolves a full RendererInfo.
func descriptionOnlyHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
	default:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chordDeviceXML))
	}
}

const movedUDN = "uuid:abcd1234-5678-90ab-cdef-1234567890ab"

func alivePacket(udn, location string) []byte {
	return []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: " + udn + "::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"LOCATION: " + location + "\r\n" +
		"\r\n")
}

// waitFor polls cond until it holds or the timeout elapses. Only ever used
// for upper-bound negative assertions ("this never became true"), so it
// can't flake in the passing direction.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func (c *SSDPDiscoveryClient) inFlightCount() int {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	return len(c.inFlight)
}

// -----------------------------------------------------------------------------
// host change → re-fetch
// -----------------------------------------------------------------------------

func TestHandlePacket_HostChangeRefetchesControlURL(t *testing.T) {
	// A renderer whose DHCP lease moved it from .7 to .99 must end up with a
	// ControlURL pointing at the NEW address. Pre-fix the exists-branch only
	// bumped LastSeenAt, so the dead URL survived forever (LastSeenAt keeps
	// advancing, EvictStale never fires, and the client hears no byebye).
	disp := &countingDispatcher{handler: descriptionOnlyHandler}
	c := newTestClient(t, disp)
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt:   time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC),
	})

	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.99:8080/description.xml"), nil)

	// Join the re-fetch. NOT waitForCacheEntry: the re-fetch now runs IN
	// PLACE, so the pre-move entry (same FriendlyName) is still cached while
	// it does — polling for "an entry named Chord 2go" would return the OLD
	// one immediately and assert nothing. spawnDetailFetch's wg.Add is
	// synchronous, and the run loops aren't started, so this Wait is exact.
	c.wg.Wait()
	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("entry must exist after the re-fetch")
	}
	want := "http://192.0.2.99:8080/avtransport/control"
	if info.ControlURL != want {
		t.Errorf("ControlURL = %q, want %q (re-fetch must adopt the new host)", info.ControlURL, want)
	}
	if info.RenderingControlURL != "http://192.0.2.99:8080/rc/control" {
		t.Errorf("RenderingControlURL = %q, want the new host", info.RenderingControlURL)
	}
	if n := len(c.cache.Snapshot()); n != 1 {
		t.Errorf("Snapshot len = %d, want 1 (the moved renderer stays usable)", n)
	}
}

func TestHandlePacket_HostChangeTransientFailureDropsDeadControlURL(t *testing.T) {
	// THE regression guard for the trap this fix exists to avoid: a re-fetch
	// that fails TRANSIENTLY must leave a genuine stub, NOT a merge into the
	// old entry. mergeRendererInfo is non-empty-wins, so a re-fetch that
	// UPSERTED its stub (the shape the server-side twin uses) would keep the
	// DEAD ControlURL while refreshing LastSeenAt — pinning the bad entry
	// forever. The fetch REPLACES, which is what lets it re-fetch in place
	// without pre-deleting the entry.
	c := newTestClient(t, &boomDispatcher{err: errors.New("connection refused")})
	dead := "http://192.0.2.7:8080/avtransport/control"
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   dead,
		LastSeenAt:   time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC),
	})

	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.99:8080/description.xml"), nil)

	// Join the failing re-fetch — the pre-move entry is still cached while it
	// runs, so waitForStub ("any entry") would return that one and assert
	// nothing.
	c.wg.Wait()
	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("a failed re-fetch must still leave a stub")
	}
	if info.ControlURL != "" {
		t.Fatalf("ControlURL = %q, want empty — the dead URL must not survive a failed re-fetch", info.ControlURL)
	}
	if info.FriendlyName != "" {
		t.Errorf("FriendlyName = %q, want empty (the stub REPLACES the pre-move entry, it must not merge into it)", info.FriendlyName)
	}
	// Hidden from /v1/renderers …
	if n := len(c.cache.Snapshot()); n != 0 {
		t.Errorf("Snapshot len = %d, want 0 (a stub must not be advertised)", n)
	}
	// … and it ages out so a later cycle retries (transient stubs keep their
	// real fail-time LastSeenAt).
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	if !info.LastSeenAt.Equal(now) {
		t.Errorf("stub LastSeenAt = %v, want %v (fail-time, so it ages out)", info.LastSeenAt, now)
	}
	if evicted := c.cache.EvictStale(now.Add(2*time.Minute), 60*time.Second); evicted != 1 {
		t.Errorf("transient stub should age out; evicted=%d want 1", evicted)
	}
}

func TestHandlePacket_HostChangeStructuralFailureUsesFreshSentinel(t *testing.T) {
	// A re-fetch that fails STRUCTURALLY at the new address earns the
	// year-2999 sentinel for THAT attempt — carrying the old entry's
	// LastSeenAt across would be wrong in both directions (a healthy old
	// timestamp would age the new stub out into a retry storm; a stale
	// sentinel would suppress retries the new address never earned).
	disp := &countingDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 4xx → structural
	}}
	c := newTestClient(t, disp)
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt:   time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC),
	})

	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.99:8080/description.xml"), nil)

	// Join the failing re-fetch — the pre-move entry is still cached while it
	// runs (the re-fetch is in place), so "any entry" would be the old one.
	c.wg.Wait()
	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("a failed re-fetch must still leave a stub")
	}
	if !info.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Errorf("LastSeenAt = %v, want the far-future sentinel %v", info.LastSeenAt, structuralStubLastSeen)
	}
	if info.ControlURL != "" {
		t.Errorf("ControlURL = %q, want empty", info.ControlURL)
	}
}

func TestHandlePacket_StructuralStubRecoversAfterHostChange(t *testing.T) {
	// The case that makes lastLocations load-bearing rather than a nicety: a
	// renderer that failed STRUCTURALLY at address A holds a stub with NO
	// ControlURL and the never-ages-out sentinel. When it reappears at
	// address B, the cached entry offers nothing to compare hosts against —
	// only the recorded Location does. Without it the stub is immortal and
	// the renderer never comes back.
	var serveGood atomic.Bool
	disp := &countingDispatcher{handler: func(w http.ResponseWriter, r *http.Request) {
		if !serveGood.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		descriptionOnlyHandler(w, r)
	}}
	c := newTestClient(t, disp)

	// Discovery at address A fails structurally.
	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.7:8080/description.xml"), nil)
	stub := waitForStub(t, c, movedUDN, 2*time.Second)
	if !stub.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Fatalf("precondition: want a structural stub, got LastSeenAt=%v", stub.LastSeenAt)
	}

	// Same renderer, new address, healthy this time.
	serveGood.Store(true)
	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.99:8080/description.xml"), nil)

	info := waitForCacheEntry(t, c, movedUDN, "Chord 2go", 2*time.Second)
	if info.ControlURL != "http://192.0.2.99:8080/avtransport/control" {
		t.Errorf("ControlURL = %q, want the new host", info.ControlURL)
	}
	if info.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Error("recovered entry still carries the structural sentinel")
	}
}

// -----------------------------------------------------------------------------
// no host change → no re-fetch
// -----------------------------------------------------------------------------

func TestHandlePacket_SameHostDoesNotRefetch(t *testing.T) {
	// An announcement from the SAME host must keep the cheap path: refresh
	// LastSeenAt, dispatch nothing. The counting dispatcher fails the test
	// if a fetch is attempted.
	disp := &countingDispatcher{} // nil handler → any call errors AND is counted
	c := newTestClient(t, disp)
	earlier := time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC)
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt:   earlier,
	})

	// Same host:port, different path — a device description URL, not the
	// control URL.
	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.7:8080/description.xml"), nil)

	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("entry must survive a same-host announcement")
	}
	if info.FriendlyName != "Chord 2go" || info.ControlURL == "" {
		t.Errorf("entry mutated on a same-host refresh: %+v", info)
	}
	want := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) // newTestClient's fixed clock
	if !info.LastSeenAt.Equal(want) {
		t.Errorf("LastSeenAt = %v, want %v (refresh must still happen)", info.LastSeenAt, want)
	}
	if n := disp.calls.Load(); n != 0 {
		t.Errorf("dispatcher called %d times; a same-host announcement must not re-fetch", n)
	}
}

func TestHandlePacket_UnparseableLocationDoesNotRefetch(t *testing.T) {
	// A malformed Location compares as "same" (sameURLHost's unparseable
	// rule), so a renderer emitting garbage can't drive a re-fetch storm.
	disp := &countingDispatcher{}
	c := newTestClient(t, disp)
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt:   time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC),
	})

	c.handlePacket(context.Background(), alivePacket(movedUDN, "://nope"), nil)

	info, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("entry must survive a malformed-Location announcement")
	}
	if info.ControlURL == "" {
		t.Error("entry was removed on a malformed Location")
	}
	if n := disp.calls.Load(); n != 0 {
		t.Errorf("dispatcher called %d times; a malformed Location must not re-fetch", n)
	}
}

// -----------------------------------------------------------------------------
// in-flight guard
// -----------------------------------------------------------------------------

// gateDispatcher blocks every fetch until released, counting entries. Unlike
// blockingDispatcher it also serves the description once released, so the
// caller can observe the completed fetch.
type gateDispatcher struct {
	// fetches counts DEVICE-DESCRIPTION requests (the GET every detail
	// fetch opens with). The follow-on GetProtocolInfo POST belongs to the
	// same fetch, so counting every request would conflate the two.
	fetches atomic.Int32
	release chan struct{}

	closeOnce sync.Once
}

func (d *gateDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		d.fetches.Add(1)
	}
	<-d.release
	rec := httptest.NewRecorder()
	descriptionOnlyHandler(rec, req)
	return rec.Result(), nil
}

func (d *gateDispatcher) open() { d.closeOnce.Do(func() { close(d.release) }) }

func TestHandlePacket_BurstDuringInFlightFetchDispatchesOnce(t *testing.T) {
	// A fetch publishes nothing until it finishes, so a brand-new UDN has no
	// cache entry and every packet in a burst lands in the first-time-UDN
	// branch — without the in-flight guard a bursty renderer (or a LAN-wide
	// power-up NOTIFY storm) fans out one fetch per packet until the first
	// one writes.
	disp := &gateDispatcher{release: make(chan struct{})}
	t.Cleanup(disp.open) // failure-path net: never leave the fetch parked
	c := newTestClient(t, disp)

	pkt := alivePacket(movedUDN, "http://192.0.2.99:8080/description.xml")
	// handlePacket claims the slot SYNCHRONOUSLY, so after these sequential
	// calls the claim count is deterministic — no polling required.
	for i := 0; i < 5; i++ {
		c.handlePacket(context.Background(), pkt, nil)
	}
	if got := c.inFlightCount(); got != 1 {
		t.Fatalf("in-flight claims = %d, want 1 (burst must collapse to one fetch)", got)
	}
	if !waitFor(func() bool { return disp.fetches.Load() >= 1 }, 2*time.Second) {
		t.Fatal("the single fetch never reached the dispatcher")
	}
	if n := disp.fetches.Load(); n != 1 {
		t.Errorf("dispatched %d description fetches, want exactly 1", n)
	}

	disp.open()
	c.wg.Wait() // the only Adds are this test's fetches; the run loops aren't started
	if got := c.inFlightCount(); got != 0 {
		t.Errorf("in-flight claims = %d after the fetch finished, want 0 (leaked claim)", got)
	}
	if n := disp.fetches.Load(); n != 1 {
		t.Errorf("dispatched %d description fetches overall, want exactly 1", n)
	}
}

func TestEvictStaleEntries_PrunesLastLocation(t *testing.T) {
	// lastLocations must not accumulate one entry per distinct UDN ever seen:
	// byebye is the only other removal path and this client is M-SEARCH-only
	// in production, so a buggy/spoofed source announcing many UDNs would
	// grow it without bound. The eviction sweep is the reaper — it keeps only
	// what's still cached or mid-fetch.
	c := newTestClient(t, &countingDispatcher{})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) // newTestClient's fixed clock

	// Live: a fully-populated entry.
	c.recordLocation("uuid:live-full", "http://192.0.2.7:8080/desc.xml")
	c.cache.Upsert(RendererInfo{
		UDN:        "uuid:live-full",
		ControlURL: "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt: now,
	})
	// Live: a stub (no ControlURL) — its Location is the ONLY host-change
	// reference it has, so it must survive.
	c.recordLocation("uuid:live-stub", "http://192.0.2.8:8080/desc.xml")
	c.cache.Upsert(RendererInfo{UDN: "uuid:live-stub", LastSeenAt: now})
	// Mid-fetch: removed from the cache by the host-change path, fetch still
	// running. Nothing to find in the cache, but the Location must survive.
	c.recordLocation("uuid:inflight", "http://192.0.2.9:8080/desc.xml")
	if !c.claimFetch("uuid:inflight") {
		t.Fatal("claimFetch should succeed on a fresh UDN")
	}
	t.Cleanup(func() { c.releaseFetch("uuid:inflight") })
	// Stale: cached, but old enough that this sweep's EvictStale drops it —
	// the Location must go in the same pass.
	c.recordLocation("uuid:stale", "http://192.0.2.10:8080/desc.xml")
	c.cache.Upsert(RendererInfo{
		UDN:        "uuid:stale",
		ControlURL: "http://192.0.2.10:8080/avtransport/control",
		LastSeenAt: now.Add(-10 * time.Minute), // past RendererTTL (60s)
	})
	// Gone: no cache entry, no fetch — pure residue.
	c.recordLocation("uuid:gone-1", "http://192.0.2.11:8080/desc.xml")
	c.recordLocation("uuid:gone-2", "http://192.0.2.12:8080/desc.xml")

	c.evictStaleEntries()

	want := map[string]bool{"uuid:live-full": true, "uuid:live-stub": true, "uuid:inflight": true}
	c.locMu.Lock()
	got := make(map[string]bool, len(c.lastLocations))
	for udn := range c.lastLocations {
		got[udn] = true
	}
	c.locMu.Unlock()
	for udn := range want {
		if !got[udn] {
			t.Errorf("lastLocations dropped %q; live + in-flight UDNs must be retained", udn)
		}
	}
	for udn := range got {
		if !want[udn] {
			t.Errorf("lastLocations retained %q; it is neither cached nor in-flight", udn)
		}
	}
	// The stale entry's cache row went in the same pass — the prune must run
	// AFTER EvictStale, not before.
	if _, ok := c.cache.Get("uuid:stale"); ok {
		t.Error("precondition: the stale entry should have been evicted")
	}
}

func TestHandlePacket_ByeByeForgetsRecordedLocation(t *testing.T) {
	// ssdp:byebye drops the renderer, so its recorded Location goes too —
	// the map tracks live devices only.
	disp := &countingDispatcher{handler: descriptionOnlyHandler}
	c := newTestClient(t, disp)
	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.7:8080/description.xml"), nil)
	waitForCacheEntry(t, c, movedUDN, "Chord 2go", 2*time.Second)

	byebye := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: " + movedUDN + "::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), byebye, nil)

	c.locMu.Lock()
	_, still := c.lastLocations[movedUDN]
	c.locMu.Unlock()
	if still {
		t.Error("byebye must drop the recorded Location")
	}
}
