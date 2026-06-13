package upnp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

func TestServerCache_UpsertGetSnapshot(t *testing.T) {
	c := NewServerCache()
	now := time.Now()
	c.Upsert(ServerInfo{
		UDN:                        "uuid:abc",
		FriendlyName:               "Chord 2Go",
		ContentDirectoryControlURL: "http://192.168.0.62:8200/ctl/ContentDir",
		LastSeenAt:                 now,
	})
	got, ok := c.Get("uuid:abc")
	if !ok || got.FriendlyName != "Chord 2Go" {
		t.Fatalf("Get = (%+v, %v); want Chord 2Go", got, ok)
	}
	snap := c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot len = %d; want 1", len(snap))
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d; want 1", c.Len())
	}
}

func TestServerCache_UpsertMergesDescriptiveFields(t *testing.T) {
	// An "alive refresh" packet carries only UDN + LastSeenAt. The
	// cache must preserve the descriptive fields populated by the
	// first-time detail fetch — otherwise every refresh would blank
	// FriendlyName / controlURL and the proxy would lose its target.
	c := NewServerCache()
	first := time.Now()
	c.Upsert(ServerInfo{
		UDN:                        "uuid:abc",
		FriendlyName:               "Chord 2Go",
		ContentDirectoryControlURL: "http://192.168.0.62:8200/ctl/ContentDir",
		LastSeenAt:                 first,
	})
	refresh := first.Add(60 * time.Second)
	c.Upsert(ServerInfo{UDN: "uuid:abc", LastSeenAt: refresh})
	got, _ := c.Get("uuid:abc")
	if got.FriendlyName != "Chord 2Go" {
		t.Errorf("FriendlyName lost on refresh: %q", got.FriendlyName)
	}
	if got.ContentDirectoryControlURL == "" {
		t.Errorf("ContentDirectoryControlURL lost on refresh")
	}
	if !got.LastSeenAt.Equal(refresh) {
		t.Errorf("LastSeenAt = %v; want %v", got.LastSeenAt, refresh)
	}
}

func TestServerCache_EvictStale(t *testing.T) {
	c := NewServerCache()
	old := time.Now().Add(-10 * time.Minute)
	fresh := time.Now()
	c.Upsert(ServerInfo{UDN: "uuid:old", LastSeenAt: old})
	c.Upsert(ServerInfo{UDN: "uuid:fresh", LastSeenAt: fresh})

	if n := c.EvictStale(time.Now(), 5*time.Minute); n != 1 {
		t.Fatalf("EvictStale = %d; want 1", n)
	}
	if _, ok := c.Get("uuid:old"); ok {
		t.Errorf("uuid:old should be evicted")
	}
	if _, ok := c.Get("uuid:fresh"); !ok {
		t.Errorf("uuid:fresh should survive")
	}
}

func TestServerCache_RemoveAndClear(t *testing.T) {
	c := NewServerCache()
	c.Upsert(ServerInfo{UDN: "uuid:a", LastSeenAt: time.Now()})
	c.Upsert(ServerInfo{UDN: "uuid:b", LastSeenAt: time.Now()})
	c.Remove("uuid:a")
	if _, ok := c.Get("uuid:a"); ok {
		t.Errorf("Remove didn't drop uuid:a")
	}
	if c.Len() != 1 {
		t.Errorf("Len after Remove = %d; want 1", c.Len())
	}
	c.Clear()
	if c.Len() != 0 {
		t.Errorf("Len after Clear = %d; want 0", c.Len())
	}
}

func TestLookupContentDirectoryControlURL(t *testing.T) {
	services := map[string]discovery.ServiceURLs{
		"urn:schemas-upnp-org:service:ContentDirectory:1":  {ControlURL: "http://h:8200/ctl/CD"},
		"urn:schemas-upnp-org:service:ConnectionManager:1": {ControlURL: "http://h:8200/ctl/CM"},
	}
	if got := lookupContentDirectoryControlURL(services); got != "http://h:8200/ctl/CD" {
		t.Fatalf("got %q", got)
	}
}

func TestLookupContentDirectoryControlURL_VersionPrefixTolerant(t *testing.T) {
	// A future server speaking ContentDirectory:2 should still match.
	services := map[string]discovery.ServiceURLs{
		"urn:schemas-upnp-org:service:ContentDirectory:2": {ControlURL: "http://h/ctl"},
	}
	if got := lookupContentDirectoryControlURL(services); got != "http://h/ctl" {
		t.Fatalf("got %q", got)
	}
}

func TestLookupContentDirectoryControlURL_AbsentReturnsEmpty(t *testing.T) {
	services := map[string]discovery.ServiceURLs{
		"urn:schemas-upnp-org:service:AVTransport:1": {ControlURL: "http://h/AVT"},
	}
	if got := lookupContentDirectoryControlURL(services); got != "" {
		t.Fatalf("got %q; want empty (no CDS)", got)
	}
}

func TestBuildMSearchPacket_ShapeAndST(t *testing.T) {
	pkt := string(buildMSearchPacket(MediaServerDeviceType))
	for _, want := range []string{
		"M-SEARCH * HTTP/1.1\r\n",
		"HOST: 239.255.255.250:1900\r\n",
		`MAN: "ssdp:discover"` + "\r\n",
		"MX: 3\r\n",
		"ST: urn:schemas-upnp-org:device:MediaServer:1\r\n",
	} {
		if !strings.Contains(pkt, want) {
			t.Errorf("packet missing %q\n%s", want, pkt)
		}
	}
}

// ---- handlePacket location-change refresh (drives the cache directly) ----

// recordingDispatcher serves a MediaServer description whose
// ContentDirectory controlURL is derived from the requested location's
// host, and counts fetches.
type recordingDispatcher struct {
	mu      sync.Mutex
	fetches int
}

func (d *recordingDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.fetches++
	d.mu.Unlock()
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Test MS</friendlyName><UDN>uuid:ms</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>` +
		`<controlURL>http://` + req.URL.Host + `/ctl/ContentDir</controlURL></service>` +
		`</serviceList></device></root>`
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(xml)
	return rec.Result(), nil
}

func (d *recordingDispatcher) fetchCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.fetches
}

func newServerDiscoveryTestClient(t *testing.T, disp discovery.SOAPDispatcher, cache *ServerCache) *MediaServerDiscoveryClient {
	t.Helper()
	cfg := DiscoveryConfig{
		Interface:  &net.Interface{},
		Dispatcher: disp,
	}
	c, err := NewMediaServerDiscoveryClient(cfg, cache)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return c
}

func alivePacket(udn, location string) []byte {
	return []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: " + udn + "::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"LOCATION: " + location + "\r\n" +
		"\r\n")
}

// TestHandlePacket_KnownUDNSameHostRefreshesWithoutFetch pins the cheap
// path: an alive from the host the cached controlURL already points at
// refreshes LastSeenAt and does NOT re-fetch the description.
func TestHandlePacket_KnownUDNSameHostRefreshesWithoutFetch(t *testing.T) {
	disp := &recordingDispatcher{}
	cache := NewServerCache()
	earlier := time.Now().Add(-time.Hour)
	cache.Upsert(ServerInfo{
		UDN:                        "uuid:ms",
		FriendlyName:               "Test MS",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		LastSeenAt:                 earlier,
	})
	c := newServerDiscoveryTestClient(t, disp, cache)
	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
	if got := disp.fetchCount(); got != 0 {
		t.Errorf("same-host alive triggered %d description fetches, want 0", got)
	}
	info, _ := cache.Get("uuid:ms")
	if !info.LastSeenAt.After(earlier) {
		t.Error("LastSeenAt not refreshed on same-host alive")
	}
}

// TestHandlePacket_KnownUDNNewHostRefetchesControlURL pins the
// location-change path: a known UDN announcing from a NEW host (DHCP
// renew / interface move) re-fetches the description so the cached
// controlURL follows the server — pre-fix the bare LastSeenAt refresh
// kept the dead URL alive forever (TTL eviction never fired while the
// server kept answering M-SEARCH).
func TestHandlePacket_KnownUDNNewHostRefetchesControlURL(t *testing.T) {
	disp := &recordingDispatcher{}
	cache := NewServerCache()
	cache.Upsert(ServerInfo{
		UDN:                        "uuid:ms",
		FriendlyName:               "Test MS",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		LastSeenAt:                 time.Now().Add(-time.Hour),
	})
	c := newServerDiscoveryTestClient(t, disp, cache)
	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.99:8200/desc.xml"), nil)

	// The re-fetch runs on a goroutine — poll briefly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL == "http://192.0.2.99:8200/ctl/ContentDir" {
			return // refreshed to the new host — done
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := cache.Get("uuid:ms")
	t.Errorf("controlURL not refreshed after location change: %q (fetches=%d)",
		info.ContentDirectoryControlURL, disp.fetchCount())
}

// TestSameURLHost pins the comparator, including the fail-same posture
// on unparseable input (a malformed SSDP Location must not trigger
// refetch storms).
func TestSameURLHost(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://h:8200/desc.xml", "http://h:8200/ctl", true},
		{"http://h:8200/desc.xml", "http://other:8200/ctl", false},
		{"http://h:8200/x", "http://h:9000/x", false},
		{"://bad", "http://h:8200/ctl", true},
	}
	for _, c := range cases {
		if got := sameURLHost(c.a, c.b); got != c.want {
			t.Errorf("sameURLHost(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
