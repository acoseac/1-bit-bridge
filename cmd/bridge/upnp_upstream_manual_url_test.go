package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// TestHealthDoesNotPublishTheOperatorsManualURL drives the chain a server
// configured with BOTH a UDN and a manual URL takes. The real manual poller
// fetches the operator's URL and caches the result under the server's key,
// which is its UDN (upnpingest.StableServerKey lowercases it), and the
// public adapter behind /v1/health's upnpUpstreamServers reads the cache
// entry under the configured UDN: for a UDN written in lowercase, the same
// entry. /v1/health answers any caller on a LAN bridge, and it published
// the operator's URL as descriptionURL, a user name, a password and a token
// in its query with it (backlog B54), where PROTOCOL.md and the DTO's
// docblock both keep a manual URL off the wire ("the operator already has
// the URL"). The control: the device's own SSDP LOCATION, cached under the
// same key, is still published as the device gave it, query included,
// which is the shape a Windows device host announces.
func TestHealthDoesNotPublishTheOperatorsManualURL(t *testing.T) {
	const secret = "s3cret-Pw"
	const udn = "uuid:4d696e69-444c-164e-9d41-00b78f5ae46b"
	desc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprint(w, `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>Cellar</friendlyName>
    <UDN>`+udn+`</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <controlURL>/ctl/ContentDir</controlURL>
      </service>
    </serviceList>
  </device>
</root>`)
	}))
	t.Cleanup(desc.Close)
	host := strings.TrimPrefix(desc.URL, "http://")
	srv := config.UPnPUpstreamServerConfig{
		Name: "Cellar", UDN: udn, PathPrefix: "cellar",
		ManualDescriptionURL: "http://user:" + secret + "@" + host + "/rootDesc.xml?token=" + secret,
	}
	holder := holderFor(t, false, srv)
	cache := upnp.NewServerCache()
	poller := upnp.NewManualPoller(upnp.ManualPollerConfig{
		Cache:     cache,
		Servers:   func() []upnp.ManualServer { return manualServersFrom(holder.Load()) },
		KnownUDNs: func() map[string]struct{} { return foreignConfiguredUDNs(holder.Load()) },
	})
	if poller == nil {
		t.Fatal("NewManualPoller returned nil for a valid config")
	}
	poller.PollOnce(context.Background())
	if info, ok := cache.Get(udn); !ok || info.DescriptionURL != srv.ManualDescriptionURL {
		t.Fatalf("the poller did not cache the operator's URL under the UDN (%+v, cached %v), "+
			"so the chain under test is not the one serve runs", info, ok)
	}

	a := &upnpPublicAdapter{cfgHolder: holder, cache: cache}
	got := a.PublicServers(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].DescriptionURL != "" {
		t.Errorf("/v1/health would publish the operator's manual URL as descriptionURL: %q", got[0].DescriptionURL)
	}
	if s := fmt.Sprintf("%+v", got[0]); strings.Contains(strings.ToLower(s), strings.ToLower(secret)) {
		t.Errorf("the public entry carries the manual URL's secret: %s", s)
	}
	if got[0].FriendlyName != "Cellar" || !got[0].Online {
		t.Errorf("leaving the URL out took more of the entry with it: %+v", got[0])
	}

	// The control: the device's own LOCATION, announced over SSDP and
	// cached under the same key, is published verbatim.
	location := "http://" + host + "/upnphost/udhisapi.dll?content=" + udn
	cache.Upsert(upnp.ServerInfo{UDN: udn, DescriptionURL: location, LastSeenAt: time.Now()})
	got = a.PublicServers(context.Background())
	if len(got) != 1 || got[0].DescriptionURL != location {
		t.Errorf("the device's own LOCATION was not published as it gave it: %+v, want descriptionURL %q", got, location)
	}
}
