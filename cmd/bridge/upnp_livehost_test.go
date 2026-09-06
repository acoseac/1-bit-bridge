package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// TestLiveHostResolvesRoutingKeySpelling pins the two-spellings rule on the
// BYTE path.
//
// `upnp_track_routing.server_udn` holds upnpingest.StableServerKey — a
// lowercased UDN. The SSDP cache is keyed on the UDN exactly as the device
// advertised it, and nothing on that path folds case. So an upstream whose UDN
// carries any uppercase character walked fine, landed routing rows and reached
// the phone, and then 503'd `upnp_server_offline` on every byte fetch —
// /v1/download, /dlna/file/{trackID} and the web player alike.
//
// The fixture builds both keys the way production does rather than hardcoding
// a lowercase twin: StableServerKey is the function the ingest actually calls,
// so if its folding rule ever changes this test changes with it instead of
// silently pinning a stale assumption.
func TestLiveHostResolvesRoutingKeySpelling(t *testing.T) {
	const advertisedUDN = "uuid:4D696E69-DLNA-1234-ABCD-0011223344FF"

	cache := upnp.NewServerCache()
	cache.Upsert(upnp.ServerInfo{
		UDN:                        advertisedUDN,
		FriendlyName:               "Chord 2Go",
		ContentDirectoryControlURL: "http://192.168.1.44:8200/ctl/ContentDir",
	})
	r := &serverCacheHostResolver{cache: cache}

	routingKey := upnpingest.StableServerKey(config.UPnPUpstreamServerConfig{UDN: advertisedUDN})
	if routingKey == advertisedUDN {
		t.Fatalf("fixture is not exercising the bug: StableServerKey(%q) returned the "+
			"advertised spelling unchanged, so the exact Get would have hit anyway", advertisedUDN)
	}

	host, ok := r.LiveHost(routingKey)
	if !ok {
		t.Fatalf("LiveHost(%q) missed: the routing key's folded spelling did not reach the "+
			"cache entry stored under %q — every byte fetch for this upstream 503s "+
			"upnp_server_offline while the upstream is up", routingKey, advertisedUDN)
	}
	if host != "192.168.1.44:8200" {
		t.Errorf("host = %q, want 192.168.1.44:8200", host)
	}
}

// TestLiveHostExactHitStillWins guards the fast path: an entry stored under a
// key that already matches must resolve without the fallback scan, and a key
// that matches nothing must still miss rather than returning some other
// upstream's host.
func TestLiveHostExactHitStillWins(t *testing.T) {
	cache := upnp.NewServerCache()
	cache.Upsert(upnp.ServerInfo{
		UDN:                        "manual:abc123",
		ContentDirectoryControlURL: "http://10.0.0.9:9000/ctl",
	})
	r := &serverCacheHostResolver{cache: cache}

	if host, ok := r.LiveHost("manual:abc123"); !ok || host != "10.0.0.9:9000" {
		t.Errorf("exact hit: got (%q, %v), want (10.0.0.9:9000, true)", host, ok)
	}
	if host, ok := r.LiveHost("uuid:some-other-server"); ok {
		t.Errorf("unknown key resolved to %q — the fallback must not match an unrelated entry", host)
	}
}
