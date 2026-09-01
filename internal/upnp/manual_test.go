package upnp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// descXML is a minimal MediaServer device description carrying a
// ContentDirectory service — the shape MiniDLNA and friends serve.
func descXML(udn, friendly, ctrl string) string {
	return `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>` + friendly + `</friendlyName>
    <manufacturer>Acme</manufacturer>
    <modelName>Widget</modelName>
    <UDN>` + udn + `</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <controlURL>` + ctrl + `</controlURL>
        <eventSubURL>/evt</eventSubURL>
        <SCPDURL>/scpd.xml</SCPDURL>
      </service>
    </serviceList>
  </device>
</root>`
}

func manualTestPoller(t *testing.T, cache *ServerCache, srvs []ManualServer, known map[string]struct{}, buf *bytes.Buffer) *ManualPoller {
	t.Helper()
	p := NewManualPoller(ManualPollerConfig{
		Cache:     cache,
		Servers:   func() []ManualServer { return srvs },
		KnownUDNs: func() map[string]struct{} { return known },
		Logger:    slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if p == nil {
		t.Fatal("NewManualPoller returned nil for a valid config")
	}
	return p
}

func TestManualPollerCachesUnderTheStableServerKey(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, descXML("uuid:real-device-udn", "Kitchen Server", "/ctl/ContentDir"))
	}))
	defer srv.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	// The key is deliberately NOT the device's own UDN — routing rows,
	// telemetry, LiveHost and the online chip all key on this string.
	const key = "manual:deadbeef"
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: key, DescriptionURL: srv.URL + "/rootDesc.xml", Name: "Configured Name",
	}}, nil, &buf)

	p.PollOnce(context.Background())
	if hits.Load() != 1 {
		t.Fatalf("description fetched %d times, want 1", hits.Load())
	}

	info, ok := cache.Get(key)
	if !ok {
		t.Fatalf("no cache entry under the StableServerKey %q; entries: %+v", key, cache.Snapshot())
	}
	if _, wrong := cache.Get("uuid:real-device-udn"); wrong {
		t.Error("an entry was ALSO stored under the device's own UDN — routing would disagree with the cache")
	}
	if !strings.HasSuffix(info.ContentDirectoryControlURL, "/ctl/ContentDir") {
		t.Errorf("ContentDirectoryControlURL = %q, want the description's controlURL", info.ContentDirectoryControlURL)
	}
	if !strings.HasPrefix(info.ContentDirectoryControlURL, "http") {
		t.Errorf("ContentDirectoryControlURL = %q, want an absolute URL the client can dial",
			info.ContentDirectoryControlURL)
	}
	if info.FriendlyName != "Kitchen Server" {
		t.Errorf("FriendlyName = %q, want the description's", info.FriendlyName)
	}
	if info.DeviceUDN != "uuid:real-device-udn" {
		t.Errorf("DeviceUDN = %q, want the device's real UDN kept for display", info.DeviceUDN)
	}
	if info.DescriptionURL == "" {
		t.Error("DescriptionURL is empty; /v1/health advertises it on LAN bridges")
	}
	if info.LastSeenAt.IsZero() {
		t.Error("LastSeenAt is zero; EvictStale would reap the entry immediately")
	}
}

// TestManualPollerFallsBackToTheConfiguredName covers a description with
// no friendlyName — the operator's label is the only name available.
func TestManualPollerFallsBackToTheConfiguredName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, descXML("uuid:x", "", "/ctl"))
	}))
	defer srv.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:k", DescriptionURL: srv.URL, Name: "Operator Label",
	}}, nil, &buf)
	p.PollOnce(context.Background())

	info, ok := cache.Get("manual:k")
	if !ok {
		t.Fatal("no cache entry")
	}
	if info.FriendlyName != "Operator Label" {
		t.Errorf("FriendlyName = %q, want the configured label", info.FriendlyName)
	}
}

// TestManualPollerRefusesADeviceAlreadyConfiguredByUDN is the
// duplicate-config guard. Caching it under BOTH spellings would have the
// ingest walk one upstream twice under two routing prefixes.
func TestManualPollerRefusesADeviceAlreadyConfiguredByUDN(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, descXML("uuid:ALREADY-Configured", "Dup", "/ctl"))
	}))
	defer srv.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	known := map[string]struct{}{"uuid:already-configured": {}} // lowercased, as the wiring produces
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:dup", DescriptionURL: srv.URL, Name: "Dup",
	}}, known, &buf)

	p.PollOnce(context.Background())
	if _, ok := cache.Get("manual:dup"); ok {
		t.Fatal("a device already configured by UDN was cached under a manual key too; " +
			"the ingest would walk it twice under two routing prefixes")
	}
	if n := strings.Count(buf.String(), "already configured by UDN"); n != 1 {
		t.Errorf("want exactly 1 warning, got %d:\n%s", n, buf.String())
	}

	// A static misconfiguration must not warn on every tick.
	for i := 0; i < 5; i++ {
		p.PollOnce(context.Background())
	}
	if n := strings.Count(buf.String(), "already configured by UDN"); n != 1 {
		t.Errorf("warning repeated across ticks (%d lines); a static misconfiguration must warn once", n)
	}
}

func TestManualPollerLeavesNoEntryWhenUnreachable(t *testing.T) {
	cache := NewServerCache()
	var buf bytes.Buffer
	// A port nothing listens on. No entry means EvictStale never has one
	// to keep alive, which is how an unreachable manual server comes to
	// report offline.
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:dead", DescriptionURL: "http://127.0.0.1:1/rootDesc.xml", Name: "Dead",
	}}, nil, &buf)
	p.PollOnce(context.Background())
	if _, ok := cache.Get("manual:dead"); ok {
		t.Error("an unreachable manual URL produced a cache entry")
	}
}

func TestManualPollerSkipsADescriptionWithNoContentDirectory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0"><device>
  <friendlyName>Printer</friendlyName><UDN>uuid:p</UDN>
  <serviceList></serviceList>
</device></root>`)
	}))
	defer srv.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:printer", DescriptionURL: srv.URL, Name: "Printer",
	}}, nil, &buf)
	p.PollOnce(context.Background())
	if _, ok := cache.Get("manual:printer"); ok {
		t.Error("a device with no ContentDirectory service was cached as a MediaServer")
	}
}

// TestManualPollerDefaultDispatcherRelaysRedirects pins the SSRF guard.
// An operator-pasted URL is more trusted than an SSDP Location header,
// but "more trusted" is not "trusted".
func TestManualPollerDefaultDispatcherRelaysRedirects(t *testing.T) {
	var target atomic.Int64
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.Add(1)
		fmt.Fprint(w, descXML("uuid:victim", "Victim", "/ctl"))
	}))
	defer victim.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, victim.URL+"/rootDesc.xml", http.StatusFound)
	}))
	defer redirector.Close()

	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{{
		Key: "manual:redir", DescriptionURL: redirector.URL, Name: "Redir",
	}}, nil, &buf)
	p.PollOnce(context.Background())

	if target.Load() != 0 {
		t.Errorf("the redirect was FOLLOWED (%d hits on the target); a 3xx must be relayed verbatim, "+
			"or a pasted URL becomes an SSRF probe against the bridge's own no-auth admin API", target.Load())
	}
	if _, ok := cache.Get("manual:redir"); ok {
		t.Error("a redirect response produced a cache entry")
	}
}

func TestNewManualPollerNilWhenNothingToPoll(t *testing.T) {
	if p := NewManualPoller(ManualPollerConfig{}); p != nil {
		t.Error("want nil for an empty config so the caller can skip the goroutine")
	}
	if p := NewManualPoller(ManualPollerConfig{Cache: NewServerCache()}); p != nil {
		t.Error("want nil when no Servers closure is supplied")
	}
}

func TestManualPollerRunStopsOnContextCancel(t *testing.T) {
	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, nil, nil, &buf)
	p.interval = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
