package upnp

// External audit 2026-09-23, finding M3, on the upstream side. LiveHost (the
// api and DLNA byte proxies' target) is derived from the cached
// ContentDirectory CONTROL URL's host:port, and upnpproxy rewrites every
// stored <res> URL onto it. So a discovered server's control URL decides
// where the bridge sends every byte fetch of that server's routed tracks, and
// a description found through SSDP may only name its own host.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// controlURLByHost serves a MediaServer description whose ContentDirectory
// control URL is chosen by the host the description was requested from, and
// records every request, so a test can stage a server that answers from one
// address honestly and from another with a hostile description.
type controlURLByHost struct {
	mu   sync.Mutex
	ctrl map[string]string // req.URL.Host → the <controlURL> value served
	seen []string          // "METHOD url"
}

func (d *controlURLByHost) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req.Method+" "+req.URL.String())
	ctrl := d.ctrl[req.URL.Host]
	d.mu.Unlock()
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(descXML("uuid:ms", "Test MS", ctrl))
	return rec.Result(), nil
}

func (d *controlURLByHost) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// TestDiscoveredServerWithAnOffHostControlURLIsNotCached drives the real
// SSDP path (handlePacket → fetchAndCacheDetails → FetchDeviceDescription)
// for a first-time server whose description names a ContentDirectory on
// another host: the bridge's own no-auth console, the target the audit
// named, and another LAN host. The second is the case only the same-host
// rule refuses (the console is also refused by the host-kind rule added
// after it, backlog B14), so it is what pins that this path parses with the
// strict source. Nothing is cached, so nothing can resolve a LiveHost from
// either.
func TestDiscoveredServerWithAnOffHostControlURLIsNotCached(t *testing.T) {
	for _, offHost := range []string{"http://127.0.0.1:7789/api/stats", "http://192.0.2.200:8200/ctl/ContentDir"} {
		disp := &controlURLByHost{ctrl: map[string]string{
			"192.0.2.7:8200": offHost,
		}}
		cache := NewServerCache()
		c := newServerDiscoveryTestClient(t, disp, cache)
		c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
		c.wg.Wait() // the detail fetch is the only goroutine: no run loops were started

		if reqs := disp.requests(); len(reqs) != 1 {
			t.Fatalf("control URL %s: requests = %q, want the one description GET (the fetch must have run)", offHost, reqs)
		}
		if info, ok := cache.Get("uuid:ms"); ok {
			t.Errorf("cached %+v: a discovered description named a ContentDirectory on another host", info)
		}
	}
}

// TestAMovedServerCannotSteerTheCachedControlURLToAnotherHost is the audit's
// attack end to end: a server known at one address re-announces its UDN from
// another (which the move detector must follow), and the description served
// there names a ContentDirectory on a THIRD host. Before the fix that URL
// replaced the cached one, and LiveHost then pointed every byte fetch of the
// server's routed tracks, /dlna/file/{trackID} on the unauthenticated DLNA
// listener included, at the bridge's own console with a path chosen at
// ingest. A genuine move, whose new description stays on its own host, is
// still followed: TestHandlePacket_KnownUDNNewHostRefetchesControlURL.
func TestAMovedServerCannotSteerTheCachedControlURLToAnotherHost(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{
		"192.0.2.7:8200":  "/ctl/ContentDir",
		"192.0.2.99:8200": "http://127.0.0.1:7789/api/stats",
	}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), nil)
	c.wg.Wait()
	const honest = "http://192.0.2.7:8200/ctl/ContentDir"
	if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != honest {
		t.Fatalf("first discovery cached %q, want %q", info.ContentDirectoryControlURL, honest)
	}

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.99:8200/desc.xml"), nil)
	c.wg.Wait()
	if reqs := disp.requests(); len(reqs) != 2 {
		t.Fatalf("requests = %q, want the move to have re-fetched the description", reqs)
	}
	info, _ := cache.Get("uuid:ms")
	if info.ContentDirectoryControlURL != honest {
		t.Errorf("ContentDirectoryControlURL = %q after the move, want %q kept: "+
			"LiveHost derives every routed byte fetch's host:port from it",
			info.ContentDirectoryControlURL, honest)
	}
}

// TestServerLocationThatIsNotHTTPWithAHostIsNeverFetched pins that a
// MediaServer announcement whose LOCATION the bridge must not fetch costs no
// request and leaves no entry.
func TestServerLocationThatIsNotHTTPWithAHostIsNeverFetched(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	for _, location := range []string{"file:///etc/passwd", "ftp://192.0.2.7/desc.xml", "http:///desc.xml"} {
		c.handlePacket(context.Background(), alivePacket("uuid:ms", location), nil)
		c.wg.Wait()
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none", reqs)
	}
	if info, ok := cache.Get("uuid:ms"); ok {
		t.Errorf("cached %+v", info)
	}
}

// TestManualPollerKeepsAControlURLOnAnotherHost pins the user-chosen half: a
// description URL the operator configured is the approval the same-host rule
// stands in for, so its ContentDirectory may live on another host. This is
// the escape hatch for a real server that spans hosts.
func TestManualPollerKeepsAControlURLOnAnotherHost(t *testing.T) {
	const crossHost = "http://192.0.2.50:8200/ctl/ContentDir"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, descXML("uuid:cross", "Cross Host", crossHost))
	}))
	defer srv.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:cross", DescriptionURL: srv.URL + "/rootDesc.xml", Name: "Cross",
	}}, nil, &buf)
	p.PollOnce(context.Background())

	info, ok := cache.Get("manual:cross")
	if !ok {
		t.Fatalf("no cache entry; log:\n%s", buf.String())
	}
	if info.ContentDirectoryControlURL != crossHost {
		t.Errorf("ContentDirectoryControlURL = %q, want the operator-approved %q", info.ContentDirectoryControlURL, crossHost)
	}
}

// TestManualPollerRefusesAControlURLThatIsNotHTTPWithAHost pins that the
// operator's choice vouches for another HOST, never another scheme. An
// ftp:// control URL matters even though no Go client speaks ftp: LiveHost
// reads only its host:port, and upnpproxy forces http onto it, so
// ftp://127.0.0.1:7789/ would have sent byte fetches to the console.
func TestManualPollerRefusesAControlURLThatIsNotHTTPWithAHost(t *testing.T) {
	for _, ctrl := range []string{"file:///etc/passwd", "ftp://127.0.0.1:7789/ctl", "http:///ctl"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprint(w, descXML("uuid:scheme", "Scheme", ctrl))
		}))
		cache := NewServerCache()
		var buf bytes.Buffer
		p := manualTestPoller(t, cache, []ManualServer{{
			Key: "manual:scheme", DescriptionURL: srv.URL + "/rootDesc.xml", Name: "Scheme",
		}}, nil, &buf)
		p.PollOnce(context.Background())
		srv.Close()
		if info, ok := cache.Get("manual:scheme"); ok {
			t.Errorf("control URL %q: cached %+v", ctrl, info)
		}
	}
}
