package upnpingest

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// TestARunStoppedByShutdownStartsNoFurtherServer: the shutdown lands in the
// first configured server's first SOAP call. That server's error is the
// cancellation, and the run starts no further server: each would only fail
// on the same cancelled context, one error each.
func TestARunStoppedByShutdownStartsNoFurtherServer(t *testing.T) {
	store := openIngestTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resolver := &countingResolver{}
	cfg := config.UPnPUpstreamConfig{Enabled: true, Servers: []config.UPnPUpstreamServerConfig{
		{Name: "A", UDN: "uuid:a"}, {Name: "B", UDN: "uuid:b"},
	}}
	ing, err := NewIngester(cfg, upnp.NewContentDirectoryClient(cancellingDoer{cancel: cancel}), resolver, store, nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ing.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := resolver.calls.Load(); got != 1 {
		t.Errorf("resolved %d servers after the run was stopped in the first, want 1", got)
	}
	if len(res.PerServer) != 1 {
		t.Errorf("the run reported %d servers, want only the one it was stopped in: %+v", len(res.PerServer), res.PerServer)
	}
}

// countingResolver resolves every server and counts the calls.
type countingResolver struct{ calls atomic.Int32 }

func (r *countingResolver) ResolveControlURL(context.Context, config.UPnPUpstreamServerConfig) (string, error) {
	r.calls.Add(1)
	return "http://192.0.2.1:8200/ctl/ContentDir", nil
}

// cancellingDoer is where the shutdown lands: its first call cancels, and
// every call answers with the cancellation, as a real transport does on a
// cancelled context.
type cancellingDoer struct{ cancel context.CancelFunc }

func (d cancellingDoer) Do(ctx context.Context, _ *http.Request) (*http.Response, error) {
	d.cancel()
	return nil, fmt.Errorf("soap: %w", ctx.Err())
}
