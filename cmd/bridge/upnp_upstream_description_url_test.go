package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// A phone that cannot see the upstream itself can still be TOLD where it
// is. iOS already accepts a description URL in its manual add-server
// flow, so this advertisement saves the operator hunting for the address
// — it does not enable anything that was impossible before.
//
// The URL is not derivable client-side: the path is vendor-specific
// (MiniDLNA /rootDesc.xml, others /description.xml or /dd.xml), so
// host:port from the control URL is not enough.

const (
	testDescURL = "http://192.168.0.62:8200/rootDesc.xml"
	testCtrlURL = "http://192.168.0.62:8200/ctl/ContentDir"
	testUDN     = "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a"
)

func cacheWithServer(t *testing.T) *upnp.ServerCache {
	t.Helper()
	c := upnp.NewServerCache()
	c.Upsert(upnp.ServerInfo{
		UDN:                        testUDN,
		FriendlyName:               "Chord 2Go:2go-ars",
		ContentDirectoryControlURL: testCtrlURL,
		DescriptionURL:             testDescURL,
		LastSeenAt:                 time.Now(),
	})
	return c
}

// TestAliveRefreshKeepsTheDescriptionURL pins the merge rule.
//
// An SSDP alive-refresh carries only UDN + LastSeenAt, and Upsert
// REPLACES the stored value. Without the preserve, every M-SEARCH cycle
// would erase the URL and the advertisement would flicker on and off at
// the discovery interval — present right after a description fetch,
// gone moments later.
func TestAliveRefreshKeepsTheDescriptionURL(t *testing.T) {
	c := cacheWithServer(t)
	c.Upsert(upnp.ServerInfo{UDN: testUDN, LastSeenAt: time.Now()})

	info, ok := c.Get(testUDN)
	if !ok {
		t.Fatal("server vanished from the cache on an alive-refresh")
	}
	if info.DescriptionURL != testDescURL {
		t.Errorf("DescriptionURL = %q after an alive-refresh, want %q — it would "+
			"flicker on and off at the M-SEARCH interval", info.DescriptionURL, testDescURL)
	}
}

// TestLANBridgeAdvertisesTheDescriptionURL is the feature.
func TestLANBridgeAdvertisesTheDescriptionURL(t *testing.T) {
	got := publicServersFor(t, false)
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].DescriptionURL != testDescURL {
		t.Errorf("DescriptionURL = %q, want %q", got[0].DescriptionURL, testDescURL)
	}
}

// TestPublicBridgeWithholdsTheDescriptionURL is the half that matters
// more.
//
// /v1/health is UNAUTHENTICATED. A publicly-reachable bridge that
// advertised this would hand every caller on the internet a private LAN
// address — useless to them, and a small disclosure of the operator's
// internal topology. Everything else about the entry still ships.
func TestPublicBridgeWithholdsTheDescriptionURL(t *testing.T) {
	got := publicServersFor(t, true)
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].DescriptionURL != "" {
		t.Errorf("a public bridge advertised %q to unauthenticated callers",
			got[0].DescriptionURL)
	}
	// The withholding is scoped: it must not blank the rest of the entry.
	if got[0].FriendlyName == "" || got[0].Name == "" {
		t.Errorf("public mode dropped more than the URL: %+v", got[0])
	}
}

// TestUnseenServerAdvertisesNoURL — nothing to report is reported as
// nothing, rather than as a stale or invented address.
func TestUnseenServerAdvertisesNoURL(t *testing.T) {
	a := &upnpPublicAdapter{
		cfgHolder: holderFor(t, false, config.UPnPUpstreamServerConfig{
			Name: "Never Seen", UDN: "uuid:not-in-the-cache", PathPrefix: "x",
		}),
		cache: cacheWithServer(t),
	}
	got := a.PublicServers(context.Background())
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].DescriptionURL != "" {
		t.Errorf("a server absent from the discovery cache advertised %q",
			got[0].DescriptionURL)
	}
}

func holderFor(t *testing.T, public bool, servers ...config.UPnPUpstreamServerConfig) *config.RuntimeConfig {
	t.Helper()
	cfg := &config.Config{}
	cfg.UPnPUpstream.Enabled = true
	cfg.UPnPUpstream.Servers = servers
	if public {
		cfg.Deployment.Mode = string(config.DeploymentModePublic)
	}
	if cfg.IsPublic() != public {
		t.Fatalf("fixture did not produce a public=%v config — the gate under test "+
			"would be exercised in the wrong direction", public)
	}
	return config.NewRuntimeConfig(cfg)
}

func publicServersFor(t *testing.T, public bool) []api.UPnPUpstreamPublicServer {
	t.Helper()
	a := &upnpPublicAdapter{
		cfgHolder: holderFor(t, public, config.UPnPUpstreamServerConfig{
			Name: "Chord 2Go", UDN: testUDN, PathPrefix: "2go",
		}),
		cache: cacheWithServer(t),
	}
	return a.PublicServers(context.Background())
}

// TestResolveControlURLResolvesAManualServer pins the wiring half of the
// manual-URL path: the cache entry the ManualPoller writes under the
// StableServerKey must be what ResolveControlURL finds, because that is
// the same string routing rows, telemetry and LiveHost all key on.
func TestResolveControlURLResolvesAManualServer(t *testing.T) {
	cache := upnp.NewServerCache()
	srv := config.UPnPUpstreamServerConfig{
		Name:                 "Cellar",
		ManualDescriptionURL: "http://192.168.0.62:8200/rootDesc.xml",
	}
	key := upnpingest.StableServerKey(srv)
	if !strings.HasPrefix(key, "manual:") {
		t.Fatalf("fixture: StableServerKey = %q, want the manual: form", key)
	}

	r := &discoveryServerResolver{cache: cache}

	// Before the poller has answered, a miss is honest: the ingest
	// reports "has not answered yet" rather than claiming the feature is
	// missing.
	if got, err := r.ResolveControlURL(context.Background(), srv); err != nil || got != "" {
		t.Fatalf("cold cache: got %q err=%v; want an empty resolution", got, err)
	}

	cache.Upsert(upnp.ServerInfo{
		UDN:                        key,
		FriendlyName:               "Cellar",
		ContentDirectoryControlURL: "http://192.168.0.62:8200/ctl/ContentDir",
		DescriptionURL:             srv.ManualDescriptionURL,
		DeviceUDN:                  "uuid:cellar",
		LastSeenAt:                 time.Now(),
	})

	got, err := r.ResolveControlURL(context.Background(), srv)
	if err != nil {
		t.Fatalf("ResolveControlURL: %v", err)
	}
	if got != "http://192.168.0.62:8200/ctl/ContentDir" {
		t.Errorf("ResolveControlURL = %q, want the cached control URL", got)
	}

	// The device's OWN udn must not resolve it — nothing keys on that.
	if _, ok := cache.Get("uuid:cellar"); ok {
		t.Error("an entry exists under the device UDN; routing would disagree with the cache")
	}
}

// TestLiveHostResolvesAManualServer pins the second surface: playback
// proxying derives host:port from the same cached control URL, so it
// works for a manual server without knowing manual servers exist.
func TestLiveHostResolvesAManualServer(t *testing.T) {
	cache := upnp.NewServerCache()
	const key = "manual:abc123"
	cache.Upsert(upnp.ServerInfo{
		UDN:                        key,
		ContentDirectoryControlURL: "http://192.168.0.62:8200/ctl/ContentDir",
		LastSeenAt:                 time.Now(),
	})
	h := &serverCacheHostResolver{cache: cache}
	got, ok := h.LiveHost(key)
	if !ok {
		t.Fatal("LiveHost missed a manual server; playback would 503 with no fallback")
	}
	if got != "192.168.0.62:8200" {
		t.Errorf("LiveHost = %q, want the host:port from the cached control URL", got)
	}
}
