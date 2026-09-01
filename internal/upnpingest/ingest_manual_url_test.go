package upnpingest

import (
	"context"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// unresolvedResolver mimics the production discoveryServerResolver's
// "haven't seen this server" contract: ("", nil), never an error. The
// shared stubResolver can't express this — it converts an empty
// controlURL into an error, which takes the resolve-failure branch
// instead of the not-discoverable one.
type unresolvedResolver struct{}

func (unresolvedResolver) ResolveControlURL(context.Context, config.UPnPUpstreamServerConfig) (string, error) {
	return "", nil
}

// TestIngester_Run_ManualURLOnlyServerReportsNotYetSupported pins the
// honest per-server error split for unresolved servers (feature review
// P2-29, 2026-08-14). A UDN-less manual-URL entry can NEVER resolve —
// the discovery-cache resolver looks up by UDN only, and the manual-URL
// fetch path is unimplemented (see the TODO in
// cmd/bridge/upnp_upstream_wiring.go's ResolveControlURL) — so the old
// "server not discoverable this tick" wording implied a transient SSDP
// problem that doesn't exist and sent operators debugging discovery.
// A UDN-configured server that merely isn't in the cache yet keeps the
// transient wording: SSDP genuinely may find it next tick.
func TestIngester_Run_ManualURLOnlyServerReportsNotYetSupported(t *testing.T) {
	// The SOAP stub is never dialed — both servers fail at resolve.
	client := upnp.NewContentDirectoryClient(newStubSOAP())
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "Manual", ManualDescriptionURL: "http://h:8200/rootDesc.xml"},
			{Name: "Cached", UDN: "uuid:not-yet-seen"},
		},
	}
	ing, err := NewIngester(cfg, client, unresolvedResolver{}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.PerServer) != 2 {
		t.Fatalf("per-server len = %d; want 2", len(res.PerServer))
	}
	byName := make(map[string]ServerIngestResult, len(res.PerServer))
	for _, pr := range res.PerServer {
		byName[pr.Name] = pr
	}

	manual := byName["Manual"]
	if manual.Err == nil {
		t.Fatal("manual-URL-only server: Err = nil; want the not-yet-supported error")
	}
	if !strings.Contains(manual.Err.Error(), "has not answered yet") {
		t.Errorf("manual-URL-only server err = %q; want it to say the URL has not answered yet",
			manual.Err)
	}
	if strings.Contains(manual.Err.Error(), "not discoverable") {
		t.Errorf("manual-URL-only server err = %q; must not read as a discovery failure — the entry can never resolve",
			manual.Err)
	}

	cached := byName["Cached"]
	if cached.Err == nil {
		t.Fatal("UDN-configured server: Err = nil; want the transient not-discoverable error")
	}
	if !strings.Contains(cached.Err.Error(), "not discoverable this tick") {
		t.Errorf("UDN-configured server err = %q; want the transient not-discoverable wording",
			cached.Err)
	}
}
