package upnp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The SSDP LOCATION is the one string a client needs to add this server
// on its own, and it is NOT derivable from anything else the cache
// holds: the path is vendor-specific (MiniDLNA /rootDesc.xml, others
// /description.xml or /dd.xml), so the control URL's host:port is not
// enough. The discovery path receives it and used to discard it.

// descURLDispatcher serves a minimal MediaServer description whose
// control endpoint sits on a DIFFERENT port than the description — the
// real split-port shape, so a test cannot accidentally pass by deriving
// one URL from the other.
type descURLDispatcher struct{}

func (descURLDispatcher) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Test MS</friendlyName><UDN>uuid:ms</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>` +
		`<controlURL>http://` + req.URL.Hostname() + `:9000/ctl/ContentDir</controlURL>` +
		`</service></serviceList></device></root>`
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(xml)
	return rec.Result(), nil
}

// TestDiscoveryStoresTheDescriptionURL drives the REAL discovery path.
//
// A test that seeds the cache by hand pins only the readers; it stays
// green if the discovery path stops recording the URL at all, and the
// feature ships advertising an empty string. This one goes through
// handlePacket → fetchAndCacheDetails.
func TestDiscoveryStoresTheDescriptionURL(t *testing.T) {
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, descURLDispatcher{}, cache)

	const location = "http://192.0.2.7:8200/rootDesc.xml"
	c.handlePacket(context.Background(), alivePacket("uuid:ms", location), nil)

	// The fetch runs on a spawned goroutine; wait for the entry to land.
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.7:9000/ctl/ContentDir")

	info, ok := cache.Get("uuid:ms")
	if !ok {
		t.Fatal("server never reached the cache")
	}
	if info.DescriptionURL != location {
		t.Errorf("DescriptionURL = %q, want %q — the discovery path receives the "+
			"LOCATION and must keep it; it cannot be reconstructed from the "+
			"control URL, whose port and path both differ",
			info.DescriptionURL, location)
	}
}

// TestDescriptionURLSurvivesSteadyStateAnnouncements is the same
// invariant across time rather than at first sight.
//
// An ssdp:alive for an address already known is recognised as unchanged
// and does NOT re-fetch the description — it refreshes LastSeenAt
// through a merge that carries no descriptive fields. If the merge
// dropped the URL, the advertisement would appear once and then vanish
// at the next announcement, which is a far more confusing bug than
// never having worked.
func TestDescriptionURLSurvivesSteadyStateAnnouncements(t *testing.T) {
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, descURLDispatcher{}, cache)

	const location = "http://192.0.2.7:8200/rootDesc.xml"
	c.handlePacket(context.Background(), alivePacket("uuid:ms", location), nil)
	waitForControlURL(t, cache, "uuid:ms", "http://192.0.2.7:9000/ctl/ContentDir")

	time.Sleep(2 * time.Millisecond)
	for range 3 {
		c.handlePacket(context.Background(), alivePacket("uuid:ms", location), nil)
	}

	info, _ := cache.Get("uuid:ms")
	if info.DescriptionURL != location {
		t.Errorf("DescriptionURL = %q after steady-state announcements, want %q",
			info.DescriptionURL, location)
	}
}
