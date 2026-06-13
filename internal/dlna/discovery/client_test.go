package discovery

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// buildMSearchRequest (pure helper — golden packet shape)
// -----------------------------------------------------------------------------

func TestBuildMSearchRequest_GoldenShape(t *testing.T) {
	pkt := buildMSearchRequest("urn:schemas-upnp-org:device:MediaRenderer:1")
	s := string(pkt)
	// Pin the request-line.
	if !strings.HasPrefix(s, "M-SEARCH * HTTP/1.1\r\n") {
		t.Errorf("missing request line\ngot: %s", s)
	}
	wantHeaders := []string{
		"HOST: 239.255.255.250:1900\r\n",
		`MAN: "ssdp:discover"` + "\r\n",
		"MX: 3\r\n",
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n",
		"USER-AGENT: 1-bit-bridge/discovery UPnP/1.0\r\n",
	}
	for _, w := range wantHeaders {
		if !strings.Contains(s, w) {
			t.Errorf("missing header %q\ngot: %s", w, s)
		}
	}
	// Pin terminating blank line.
	if !strings.HasSuffix(s, "\r\n\r\n") {
		t.Errorf("M-SEARCH must end with CRLFCRLF")
	}
}

// -----------------------------------------------------------------------------
// NewSSDPDiscoveryClient construction
// -----------------------------------------------------------------------------

func TestNewSSDPDiscoveryClient_RejectsNilInterface(t *testing.T) {
	_, err := NewSSDPDiscoveryClient(
		DiscoveryConfig{}, // nil Interface
		NewRendererCache(),
	)
	if err == nil {
		t.Fatal("expected error for nil Interface")
	}
}

func TestNewSSDPDiscoveryClient_RejectsNilCache(t *testing.T) {
	_, err := NewSSDPDiscoveryClient(
		DiscoveryConfig{Interface: &net.Interface{}},
		nil,
	)
	if err == nil {
		t.Fatal("expected error for nil cache")
	}
}

func TestNewSSDPDiscoveryClient_AppliesDefaults(t *testing.T) {
	c, err := NewSSDPDiscoveryClient(
		DiscoveryConfig{Interface: &net.Interface{}},
		NewRendererCache(),
	)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	if c.cfg.MSearchInterval != 30*time.Second {
		t.Errorf("default MSearchInterval = %v", c.cfg.MSearchInterval)
	}
	if c.cfg.RendererTTL != 60*time.Second {
		t.Errorf("default RendererTTL = %v", c.cfg.RendererTTL)
	}
	if c.cfg.DetailFetchTimeout != 5*time.Second {
		t.Errorf("default DetailFetchTimeout = %v", c.cfg.DetailFetchTimeout)
	}
}

// -----------------------------------------------------------------------------
// handlePacket dispatch (drives the cache directly; no socket needed)
// -----------------------------------------------------------------------------

// newTestClient constructs a client with stub dispatcher + fixed
// clock for deterministic testing of handlePacket dispatch.
func newTestClient(t *testing.T, dispatcher SOAPDispatcher) *SSDPDiscoveryClient {
	t.Helper()
	cfg := DefaultDiscoveryConfig()
	cfg.Interface = &net.Interface{}
	cfg.Dispatcher = dispatcher
	cfg.NowFunc = func() time.Time {
		return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	}
	c, err := NewSSDPDiscoveryClient(cfg, NewRendererCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return c
}

func TestHandlePacket_NotifyByeByeRemovesEntry(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	// Pre-populate the cache.
	c.cache.Upsert(RendererInfo{
		UDN:          "uuid:bye-test",
		FriendlyName: "Will Depart",
		LastSeenAt:   now,
	})
	pkt := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: uuid:bye-test::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	if _, ok := c.cache.Get("uuid:bye-test"); ok {
		t.Error("ssdp:byebye should remove the entry")
	}
}

func TestHandlePacket_FiltersNonMediaRendererTarget(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	// MediaServer NOTIFY — must NOT trigger any cache mutation.
	pkt := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: uuid:server::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"LOCATION: http://x/y\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	if c.cache.Len() != 0 {
		t.Errorf("MediaServer announcement should be ignored, got %d entries", c.cache.Len())
	}
}

func TestHandlePacket_KnownUDNAliveRefreshesLastSeen(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	earlier := time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC)
	c.cache.Upsert(RendererInfo{
		UDN:          "uuid:known",
		FriendlyName: "Already Known",
		ControlURL:   "http://192.0.2.7/ctrl", // complete entry → refresh path
		LastSeenAt:   earlier,
	})
	pkt := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: uuid:known::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"LOCATION: http://x/y\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	info, _ := c.cache.Get("uuid:known")
	if info.FriendlyName != "Already Known" {
		t.Errorf("FriendlyName dropped on refresh: %q", info.FriendlyName)
	}
	// Our fixed clock returns 12:00; the refresh should advance the timestamp.
	want := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	if !info.LastSeenAt.Equal(want) {
		t.Errorf("LastSeenAt = %v, want %v", info.LastSeenAt, want)
	}
}

func TestHandlePacket_NewUDNTriggersDetailFetch(t *testing.T) {
	// Stub serves both the device description AND the
	// GetProtocolInfo SOAP response. The test polls the cache
	// (rather than waiting on a WaitGroup of handler Done calls)
	// because the cache write happens AFTER the handler returns
	// — the fetcher parses the body + Upserts post-Do, so a
	// WaitGroup tied to handler completion can race the cache
	// write.
	disp := &stubDispatcher{
		handler: func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(chordDeviceXML))
			case http.MethodPost:
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
			default:
				t.Errorf("unexpected method %s", r.Method)
			}
		},
	}
	c := newTestClient(t, disp)
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://192.168.1.42:8080/description.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:abcd1234-5678-90ab-cdef-1234567890ab::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	info := waitForCacheEntry(t, c, "uuid:abcd1234-5678-90ab-cdef-1234567890ab", "Chord 2go", 2*time.Second)
	if info.FriendlyName != "Chord 2go" {
		t.Errorf("FriendlyName = %q", info.FriendlyName)
	}
	if len(info.SinkProtocolInfos) != 3 {
		t.Errorf("SinkProtocolInfos count = %d, want 3", len(info.SinkProtocolInfos))
	}
	if info.ControlURL == "" {
		t.Error("ControlURL empty — AVTransport service URL not resolved")
	}
}

// waitForCacheEntry polls c.Cache for an entry with the given UDN
// AND the wantFriendlyName populated (so it doesn't return early
// on the stub-entry write the fetcher does on failure paths).
// Returns the populated entry or fails the test on timeout.
func waitForCacheEntry(t *testing.T, c *SSDPDiscoveryClient, udn, wantFriendlyName string, timeout time.Duration) RendererInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, ok := c.cache.Get(udn); ok && info.FriendlyName == wantFriendlyName {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := c.cache.Get(udn)
	t.Fatalf("cache entry for %s with FriendlyName=%q not populated within %v (got %+v)", udn, wantFriendlyName, timeout, info)
	return RendererInfo{}
}

func TestHandlePacket_FetchFailureStillCachesStub(t *testing.T) {
	// Description fetch fails → cache holds a stub entry with just
	// UDN + lastSeenAt so the next M-SEARCH cycle's "exists?"
	// check fires the refresh path, NOT a re-fetch storm.
	disp := &boomDispatcher{err: errors.New("connection refused")}
	c := newTestClient(t, disp)
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://offline/desc.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:offline::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	// Wait up to 1s for the goroutine to complete its failed fetch.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := c.cache.Get("uuid:offline"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, ok := c.cache.Get("uuid:offline")
	if !ok {
		t.Fatal("cache should hold stub entry after fetch failure")
	}
	// Stub entry: UDN + LastSeenAt populated; metadata empty.
	if info.FriendlyName != "" {
		t.Errorf("stub entry should have empty FriendlyName, got %q", info.FriendlyName)
	}
	if info.LastSeenAt.IsZero() {
		t.Error("stub entry should carry LastSeenAt")
	}
}

// waitForStub polls for ANY cached entry with the given UDN (unlike
// waitForCacheEntry, it doesn't require a populated FriendlyName) — used
// to observe the stub the detail-fetch failure path writes.
func waitForStub(t *testing.T, c *SSDPDiscoveryClient, udn string, timeout time.Duration) RendererInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, ok := c.cache.Get(udn); ok {
			return info
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stub entry for %s not cached within %v", udn, timeout)
	return RendererInfo{}
}

func TestHandlePacket_IncompleteStubNotRefreshedOrRetried(t *testing.T) {
	// A cached INCOMPLETE stub (no ControlURL) must NOT have its LastSeenAt
	// refreshed on a later alive packet, and must NOT trigger a re-fetch in
	// the same cycle — so transient stubs can age out + retry. (bridge-12:
	// pre-fix the exists-branch refreshed LastSeenAt forever, so the stub
	// never aged out and the renderer stayed nameless until restart.) The
	// boomDispatcher would error if a fetch were wrongly attempted; the
	// frozen LastSeenAt is the real assertion (no Upsert ran).
	earlier := time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC)
	c := newTestClient(t, &boomDispatcher{err: errors.New("must not be called")})
	c.cache.Upsert(RendererInfo{UDN: "uuid:stub", LastSeenAt: earlier}) // no ControlURL
	pkt := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: uuid:stub::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"LOCATION: http://x/y\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	info, ok := c.cache.Get("uuid:stub")
	if !ok {
		t.Fatal("stub should remain cached")
	}
	if !info.LastSeenAt.Equal(earlier) {
		t.Errorf("incomplete stub LastSeenAt advanced to %v; want frozen at %v", info.LastSeenAt, earlier)
	}
}

func TestHandlePacket_TransientFailureStubAgesOut(t *testing.T) {
	// A transient detail-fetch failure (network error) caches a stub with
	// the real fail-time LastSeenAt, so EvictStale ages it out → a later
	// M-SEARCH re-discovers it as new and retries. (bridge-12.)
	c := newTestClient(t, &boomDispatcher{err: errors.New("connection refused")})
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://offline/desc.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:transient::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	info := waitForStub(t, c, "uuid:transient", 1*time.Second)
	if !info.LastSeenAt.Equal(now) {
		t.Errorf("transient stub LastSeenAt = %v, want now %v (so it ages out)", info.LastSeenAt, now)
	}
	if evicted := c.cache.EvictStale(now.Add(2*time.Minute), 60*time.Second); evicted != 1 {
		t.Errorf("transient stub should age out; evicted=%d want 1", evicted)
	}
}

func TestHandlePacket_StructuralFailureStubPersistsAndIsHidden(t *testing.T) {
	// A structural failure (HTTP 404 — no description at that URL) caches a
	// stub with the far-future sentinel LastSeenAt, so EvictStale never
	// ages it out → it never retries (no storm) — and it stays hidden from
	// /v1/renderers. (bridge-12.)
	disp := &stubDispatcher{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}}
	c := newTestClient(t, disp)
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://broken/desc.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:structural::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	info := waitForStub(t, c, "uuid:structural", 1*time.Second)
	if !info.LastSeenAt.Equal(structuralStubLastSeen) {
		t.Errorf("structural stub LastSeenAt = %v, want far-future sentinel %v", info.LastSeenAt, structuralStubLastSeen)
	}
	if evicted := c.cache.EvictStale(now.Add(10*time.Minute), 60*time.Second); evicted != 0 {
		t.Errorf("structural stub must NOT age out; evicted=%d want 0", evicted)
	}
	if n := len(c.cache.Snapshot()); n != 0 {
		t.Errorf("structural stub must be hidden from Snapshot; got %d", n)
	}
}

func TestHandlePacket_DropsMalformedPacketSilently(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	// Malformed — no LF in first line.
	c.handlePacket(context.Background(), []byte("garbage"), nil)
	if c.cache.Len() != 0 {
		t.Error("malformed packet should be silently dropped")
	}
}

func TestHandlePacket_DropsEmptyUDN(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://x/y\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		// USN missing entirely
		"\r\n")
	c.handlePacket(context.Background(), pkt, nil)
	if c.cache.Len() != 0 {
		t.Error("packet without USN should be dropped")
	}
}

// -----------------------------------------------------------------------------
// Cache accessor sanity
// -----------------------------------------------------------------------------

func TestCacheAccessor_ReturnsConstructedCache(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})
	if c.Cache() == nil {
		t.Fatal("Cache() should not return nil")
	}
}

// -----------------------------------------------------------------------------
// End-to-end fetcher round-trip via real httptest.Server
// -----------------------------------------------------------------------------

// Verifies the production HTTPClientDispatcher works end-to-end
// against a real net listener — covers the path the production
// wiring takes (NewSSDPDiscoveryClient without a stub dispatcher).
func TestFetcherRoundTrip_ViaHTTPClientDispatcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chordDeviceXML))
		case http.MethodPost:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
		}
	}))
	defer srv.Close()
	disp := &HTTPClientDispatcher{
		Client: &http.Client{Timeout: 2 * time.Second},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	desc, err := FetchDeviceDescription(ctx, disp, srv.URL+"/desc.xml")
	if err != nil {
		t.Fatalf("FetchDeviceDescription: %v", err)
	}
	if desc.FriendlyName != "Chord 2go" {
		t.Errorf("FriendlyName = %q", desc.FriendlyName)
	}
	sinks, err := FetchGetProtocolInfo(ctx, disp, srv.URL+"/cm/control")
	if err != nil {
		t.Fatalf("FetchGetProtocolInfo: %v", err)
	}
	if len(sinks) != 3 {
		t.Errorf("len(sinks) = %d, want 3", len(sinks))
	}
}

// -----------------------------------------------------------------------------
// Stop() quiesces in-flight detail fetches before clearing the cache (PR A)
// -----------------------------------------------------------------------------

// blockingDispatcher signals when a fetch enters and blocks until released,
// deliberately IGNORING ctx — it simulates a detail fetch that's slow to
// honour cancellation, which is exactly the case where Stop()'s wg.Wait()
// must hold the teardown back. The entered channel is buffered + sent
// non-blocking so a stray second fetch can't panic on a closed channel.
type blockingDispatcher struct {
	entered chan struct{}
	release chan struct{}
}

func (d *blockingDispatcher) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	<-d.release
	return nil, errors.New("blockingDispatcher released")
}

// TestStopWaitsForInFlightFetch pins the Stop()/Upsert race fix: Stop() must
// block in wg.Wait() until an in-flight fetch goroutine has exited, so the
// fetch can't Upsert into the cache after Clear(). Deterministic via channel
// synchronization — no time.Sleep lower-bound; the only timeout is an
// upper-bound negative assertion ("Stop did not return while the fetch was
// still blocked"), which can't flake in the passing direction.
func TestStopWaitsForInFlightFetch(t *testing.T) {
	disp := &blockingDispatcher{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	c := newTestClient(t, disp)

	parent, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent) // failure-path net: cancels run loops even if Stop isn't reached
	if err := c.Start(parent); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Snapshot the live runCtx (the one Stop cancels) under the lock so the
	// fetch's cancellation guards observe Stop()'s runCancel().
	c.runMu.RLock()
	runCtx := c.runCtx
	c.runMu.RUnlock()

	// Trigger one detail fetch deterministically — no reliance on a real
	// renderer answering the socket. handlePacket spawns the tracked fetch.
	pkt := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://192.0.2.10:8080/desc.xml\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:inflight::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	c.handlePacket(runCtx, pkt, nil)

	select {
	case <-disp.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("detail fetch never entered the dispatcher")
	}

	stopReturned := make(chan struct{})
	go func() {
		c.Stop()
		close(stopReturned)
	}()

	// Stop() must NOT return while the fetch is still blocked — wg.Wait() holds it.
	select {
	case <-stopReturned:
		t.Fatal("Stop() returned before the in-flight fetch finished — wg.Wait() missing/misplaced")
	case <-time.After(150 * time.Millisecond):
		// Expected: Stop is parked in wg.Wait().
	}

	// Release the fetch; it returns an error, sees runCtx cancelled, exits without Upsert.
	close(disp.release)

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return after the fetch was released")
	}

	if n := c.cache.Len(); n != 0 {
		t.Errorf("cache not empty after Stop(): %d entries (a fetch Upserted past Clear())", n)
	}
}
