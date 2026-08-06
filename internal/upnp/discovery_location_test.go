package upnp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// splitPortDispatcher serves a MediaServer description whose
// ContentDirectory controlURL is ABSOLUTE and on a DIFFERENT port than the
// description endpoint it was fetched from. This is legal UPnP — and it is
// what ParseDeviceDescription hands back verbatim, because
// base.ResolveReference returns an absolute <controlURL> unchanged — so it
// is the shape that made the old Location-vs-controlURL comparison report
// "moved" on every single announcement.
type splitPortDispatcher struct {
	mu      sync.Mutex
	fetches int
	// hosts records the host each fetch was issued against, so a test can
	// tell a re-fetch of the SAME address from a genuine move.
	hosts []string
}

func (d *splitPortDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.fetches++
	d.hosts = append(d.hosts, req.URL.Host)
	d.mu.Unlock()
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Test MS</friendlyName><UDN>uuid:ms</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>` +
		// Control endpoint on :9000; description was fetched from :8200.
		`<controlURL>http://` + req.URL.Hostname() + `:9000/ctl/ContentDir</controlURL>` +
		`</service></serviceList></device></root>`
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(xml)
	return rec.Result(), nil
}

func (d *splitPortDispatcher) fetchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fetches
}

func (d *splitPortDispatcher) fetchHosts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.hosts...)
}

// waitForFetchCount polls until the dispatcher has served at least want
// fetches or the window expires, returning the final count. Detail fetches
// run on spawned goroutines, so a bare read immediately after handlePacket
// races them and would under-report a re-fetch storm — the whole thing this
// file is about. The dispatcher is in-memory (microseconds per fetch), so
// the window is pure margin.
func waitForFetchCount(d *splitPortDispatcher, want int, window time.Duration) int {
	deadline := time.Now().Add(window)
	for {
		got := d.fetchCount()
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForControlURL polls until the async detail fetch has published want,
// so the tests below don't race the fetch goroutine.
func waitForControlURL(t *testing.T, cache *ServerCache, udn, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, ok := cache.Get(udn); ok && info.ContentDirectoryControlURL == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	info, _ := cache.Get(udn)
	t.Fatalf("controlURL never became %q (got %q)", want, info.ContentDirectoryControlURL)
}

// TestHandlePacket_SplitPortControlURL_NoRefetchStorm is the regression gate
// for comparing the announced Location against the CONTROL URL instead of
// against the previous Location.
//
// A device whose control endpoint sits on a different port than its
// description endpoint compared as "moved" on EVERY announcement, which cost
// a description GET per announcement forever AND — because that branch
// returned before the LastSeenAt refresh — meant LastSeenAt only advanced
// when the fetch succeeded. A description endpoint flaky for longer than
// ServerTTL then evicted a live, M-SEARCH-answering server, after which
// ResolveControlURL returns "" and upnpproxy 503s every play of its tracks.
func TestHandlePacket_SplitPortControlURL_NoRefetchStorm(t *testing.T) {
	disp := &splitPortDispatcher{}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	const descLocation = "http://192.0.2.7:8200/desc.xml"
	const wantCtrl = "http://192.0.2.7:9000/ctl/ContentDir"

	// First-time discovery: one fetch, controlURL lands on :9000 while the
	// Location it was resolved from is on :8200.
	c.handlePacket(context.Background(), alivePacket("uuid:ms", descLocation), nil)
	waitForControlURL(t, cache, "uuid:ms", wantCtrl)
	if got := disp.fetchCount(); got != 1 {
		t.Fatalf("initial discovery issued %d fetches; want 1", got)
	}

	// Subsequent announcements from the SAME address must be recognised as
	// unchanged — no re-fetch, and LastSeenAt must keep advancing.
	before, _ := cache.Get("uuid:ms")
	time.Sleep(2 * time.Millisecond) // ensure a strictly later wall clock
	for range 3 {
		c.handlePacket(context.Background(), alivePacket("uuid:ms", descLocation), nil)
	}
	if got := waitForFetchCount(disp, 2, 500*time.Millisecond); got != 1 {
		t.Errorf("steady-state announcements issued %d fetches (want 1); hosts=%v — "+
			"the device's control endpoint is on a different port than its "+
			"description endpoint, which is not a move", got, disp.fetchHosts())
	}
	after, _ := cache.Get("uuid:ms")
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Error("LastSeenAt not refreshed on a same-address announcement — " +
			"the entry will be evicted by EvictStale while the server is alive")
	}
	if after.ContentDirectoryControlURL != wantCtrl {
		t.Errorf("controlURL = %q; want %q (the LastSeenAt refresh must merge, not clobber)",
			after.ContentDirectoryControlURL, wantCtrl)
	}
}

// TestHandlePacket_MovedHostRefreshesLastSeenAt pins the second half of the
// fix: even on the moved-host branch — where a re-fetch is dispatched and
// may fail — the device just proved it is alive, so LastSeenAt MUST advance.
// Pre-fix that branch returned early, so a server whose description endpoint
// was down for longer than ServerTTL was evicted mid-move and its tracks
// became unplayable until the bridge restarted.
func TestHandlePacket_MovedHostRefreshesLastSeenAt(t *testing.T) {
	// A dispatcher that always fails: the move is detected, the re-fetch is
	// dispatched, and it never publishes anything. LastSeenAt must still
	// advance off the announcement alone.
	disp := &failingDispatcher{}
	cache := NewServerCache()
	earlier := time.Now().Add(-time.Hour)
	cache.Upsert(ServerInfo{
		UDN:                        "uuid:ms",
		FriendlyName:               "Test MS",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		LastSeenAt:                 earlier,
	})
	c := newServerDiscoveryTestClient(t, disp, cache)

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.99:8200/desc.xml"), nil)

	info, ok := cache.Get("uuid:ms")
	if !ok {
		t.Fatal("entry removed on the moved-host branch; the server twin re-fetches IN PLACE")
	}
	if !info.LastSeenAt.After(earlier) {
		t.Error("LastSeenAt not refreshed on the moved-host branch — a flaky " +
			"description endpoint will evict a live server (503 on every play)")
	}
}

// failingDispatcher fails every description fetch.
type failingDispatcher struct{}

func (d *failingDispatcher) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusServiceUnavailable)
	return rec.Result(), nil
}

// TestHandlePacket_GenuineMoveStillRefetches guards against over-correcting:
// once a Location has been recorded, an announcement from a DIFFERENT host
// must still re-fetch so the cached controlURL follows the server across a
// DHCP renew. This exercises the RECORDED-location path (the sibling test in
// discovery_test.go covers the cached-controlURL fallback for entries this
// client never fetched itself).
func TestHandlePacket_GenuineMoveStillRefetches(t *testing.T) {
	disp := &splitPortDispatcher{}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.7:9000/ctl/ContentDir")

	// Same UDN, new address.
	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.99:8200/desc.xml"), nil)
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.99:9000/ctl/ContentDir")
}

// TestPruneLocations_DropsUncachedUDNs pins the bookkeeping cleanup: an
// entry evicted from the cache must not leave its recorded Location behind,
// or the map grows one entry per distinct UDN ever seen (this client listens
// on an ephemeral unicast port, so ssdp:byebye is rarely received).
func TestPruneLocations_DropsUncachedUDNs(t *testing.T) {
	disp := &splitPortDispatcher{}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.7:9000/ctl/ContentDir")

	if got := c.previousLocation("uuid:ms", ""); got != "http://192.0.2.7:8200/desc.xml" {
		t.Fatalf("previousLocation = %q; want the fetched Location", got)
	}
	// A cached UDN must survive a prune.
	c.pruneLocations()
	if got := c.previousLocation("uuid:ms", ""); got == "" {
		t.Error("prune dropped the Location of a still-cached server")
	}
	// Once the cache entry ages out, the shadow entry must go too.
	cache.Remove("uuid:ms")
	c.pruneLocations()
	if got := c.previousLocation("uuid:ms", ""); got != "" {
		t.Errorf("prune left %q behind for an uncached UDN", got)
	}
}

// TestHandlePacket_ByeByeForgetsLocation pins the other removal path.
func TestHandlePacket_ByeByeForgetsLocation(t *testing.T) {
	disp := &splitPortDispatcher{}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.7:9000/ctl/ContentDir")

	c.handlePacket(context.Background(), byebyePacket("uuid:ms"), nil)
	if _, ok := cache.Get("uuid:ms"); ok {
		t.Error("byebye did not remove the cache entry")
	}
	if got := c.previousLocation("uuid:ms", ""); got != "" {
		t.Errorf("byebye left recorded Location %q behind", got)
	}
}

func byebyePacket(udn string) []byte {
	return []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: " + udn + "::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"\r\n")
}
