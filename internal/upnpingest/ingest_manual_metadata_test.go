package upnpingest

import (
	"context"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// TestIngester_Run_AManualURLOnAMetadataAddressSaysWhy: the manual poller
// never fetches a manual URL on a cloud metadata address (backlog B54), so
// such a server never enters the cache, and the ingest's per-server error,
// which the console shows as the server's last walk error and the journal
// logs every tick, must say that rather than "has not answered yet", which
// sends the operator to the network. A manual server elsewhere keeps the
// old wording, the control.
func TestIngester_Run_AManualURLOnAMetadataAddressSaysWhy(t *testing.T) {
	client := upnp.NewContentDirectoryClient(newStubSOAP())
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "Metadata", ManualDescriptionURL: "http://169.254.169.254/latest/meta-data/"},
			{Name: "Elsewhere", ManualDescriptionURL: "http://nas.example:8200/rootDesc.xml"},
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
	byName := make(map[string]ServerIngestResult, len(res.PerServer))
	for _, pr := range res.PerServer {
		byName[pr.Name] = pr
	}
	if e := byName["Metadata"].Err; e == nil || !strings.Contains(e.Error(), "cloud metadata address") {
		t.Errorf("a manual URL on a metadata address: err = %v, want one saying the bridge does not fetch it", e)
	}
	if e := byName["Elsewhere"].Err; e == nil || !strings.Contains(e.Error(), "has not answered yet") {
		t.Errorf("a manual URL elsewhere: err = %v, want the not-answered-yet wording", e)
	}
}
