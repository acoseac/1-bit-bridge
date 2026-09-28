package upnp

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// floodServers announces n distinct MediaServers, each at its own address,
// whose descriptions (recordingDispatcher's) are valid, and joins every
// fetch: in batches of discovery.MaxPendingDetailFetches, so no dispatch is
// refused at the claims bound.
func floodServers(c *MediaServerDiscoveryClient, prefix string, n int) {
	for i := 0; i < n; i++ {
		announceFrom(c, fmt.Sprintf("uuid:%s-%d", prefix, i),
			fmt.Sprintf("http://198.51.100.%d:%d/desc.xml", 1+i%250, 8000+i/250))
		if (i+1)%discovery.MaxPendingDetailFetches == 0 {
			c.wg.Wait()
		}
	}
	c.wg.Wait()
}

func (c *MediaServerDiscoveryClient) hasLocationRecord(udn string) bool {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	_, ok := c.lastLocation[udn]
	return ok
}

func (c *MediaServerDiscoveryClient) locationRecordCount() int {
	c.locMu.Lock()
	defer c.locMu.Unlock()
	return len(c.lastLocation)
}

// TestAFloodOfFakeServersStaysBounded is B47's second item, measured: a
// MediaServer serving a valid description stayed cached for ServerTTL, or
// for as long as it kept announcing, however many a LAN peer invented (on
// main, 100,000 of them held 45 MB, and LiveHost's case-folded fallback
// copied all of them on every routed byte fetch it served). The cache takes
// discovery.MaxCachedDevices servers found through SSDP and refuses the
// next, never evicting one it holds, so the servers cached before a flood
// are the ones cached after it. A refused server leaves no location record,
// and is cached once room is made.
func TestAFloodOfFakeServersStaysBounded(t *testing.T) {
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, &recordingDispatcher{}, cache)
	floodServers(c, "early", 10)
	floodServers(c, "flood", 1000)

	if got := cache.Len(); got != discovery.MaxCachedDevices {
		t.Errorf("1,010 servers left %d cache entries, want the bound, %d", got, discovery.MaxCachedDevices)
	}
	for i := 0; i < 10; i++ {
		if _, ok := cache.Get(fmt.Sprintf("uuid:early-%d", i)); !ok {
			t.Errorf("uuid:early-%d, cached before the flood, is gone after it", i)
		}
	}
	if got := c.locationRecordCount(); got != discovery.MaxCachedDevices {
		t.Errorf("%d location records, want one per cached server, %d", got, discovery.MaxCachedDevices)
	}
	const refused = "uuid:flood-999"
	if _, ok := cache.Get(refused); ok || c.hasLocationRecord(refused) {
		t.Fatalf("precondition: %s is the flood's last server, past the bound, yet it was kept", refused)
	}

	cache.Remove("uuid:flood-0") // a server leaves (ssdp:byebye)
	announceFrom(c, refused, "http://198.51.100.250:8003/desc.xml")
	c.wg.Wait()
	if _, ok := cache.Get(refused); !ok || !c.hasLocationRecord(refused) {
		t.Error("a refused server's next announcement, with room made, was not cached")
	}
}

// TestAConfiguredServerIsCachedPastTheBound pins what the bound must not
// cost: the servers the operator configured are the ones the ingest walks
// and the proxy dials, so a cache full of fakes still takes one, whether the
// SSDP client found it by its UDN (DiscoveryConfig.Configured) or the manual
// poller by its URL.
func TestAConfiguredServerIsCachedPastTheBound(t *testing.T) {
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, &recordingDispatcher{}, cache)
	c.cfg.Configured = func(udn string) bool { return udn == "uuid:cellar" }
	floodServers(c, "flood", discovery.MaxCachedDevices)

	announceFrom(c, "uuid:not-configured", "http://192.0.2.60:8200/desc.xml")
	announceFrom(c, "uuid:cellar", "http://192.0.2.61:8200/desc.xml")
	c.wg.Wait()
	if _, ok := cache.Get("uuid:not-configured"); ok {
		t.Fatal("precondition: the full cache took a server nobody configured")
	}
	if info, ok := cache.Get("uuid:cellar"); !ok || info.ContentDirectoryControlURL == "" {
		t.Errorf("a full cache refused the server the operator configured by UDN: %+v (cached=%v)", info, ok)
	}

	desc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, descXML("uuid:attic-device", "Attic", "/ctl"))
	}))
	defer desc.Close()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{{Key: "manual:attic", DescriptionURL: desc.URL + "/rootDesc.xml", Name: "Attic"}}, nil, &buf)
	p.PollOnce(context.Background())
	if info, ok := cache.Get("manual:attic"); !ok || info.ContentDirectoryControlURL == "" {
		t.Errorf("a full cache refused the server the operator configured by URL: %+v (cached=%v)\n%s", info, ok, buf.String())
	}
}

// TestTouchStoresNothingNew pins the lookup-and-refresh the SSDP handler
// makes on every announcement. It was a Get and then an Upsert of `{UDN,
// LastSeenAt}` until 2026-09-28, so an entry EvictStale removed between the
// two came back with no control URL, which the handler never fetched again
// while the server kept announcing (measured on main: 2 of 200,000 such
// races, each still without a control URL ten announcements later). Touch
// is one step under the lock, and stores nothing for a server the cache
// does not hold.
func TestTouchStoresNothingNew(t *testing.T) {
	cache := NewServerCache()
	at := time.Unix(100, 0)
	if _, ok := cache.Touch("uuid:gone", at); ok {
		t.Error("Touch reported a server the cache does not hold")
	}
	if _, ok := cache.Get("uuid:gone"); ok {
		t.Fatal("Touch stored a server the cache did not hold")
	}

	cache.Upsert(ServerInfo{UDN: "uuid:x", FriendlyName: "Cellar", ContentDirectoryControlURL: "http://192.0.2.10:8200/ctl", LastSeenAt: at})
	later := at.Add(time.Minute)
	got, ok := cache.Touch("uuid:x", later)
	if !ok {
		t.Fatal("Touch did not report a server the cache holds")
	}
	stored, _ := cache.Get("uuid:x")
	for _, info := range []ServerInfo{got, stored} {
		if !info.LastSeenAt.Equal(later) || info.FriendlyName != "Cellar" || info.ContentDirectoryControlURL == "" {
			t.Errorf("after Touch: %+v, want LastSeenAt %v and every other field kept", info, later)
		}
	}
}
