package upnpingest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// TestAWalkErrorNamesNoControlURLUserInformation drives the ingest against a
// server whose control URL carries the user information of the manual URL
// it was found through (backlog B66), over the real SOAP client, with the
// server refusing every request. The per-server error is what serve logs at
// Warn ("UPnP upstream: per-server error", printed by default) and what the
// console shows as the server's last walk error, and it named the URL whole
// until B66: `upnp: POST http://user:<password>@host/ctl: status 401`.
func TestAWalkErrorNamesNoControlURLUserInformation(t *testing.T) {
	const secret = "s3cret-Pw"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	for _, userinfo := range []string{"user:" + secret + "@", secret + "@"} {
		client := upnp.NewContentDirectoryClient(&discovery.HTTPClientDispatcher{Client: &http.Client{}})
		cfg := config.UPnPUpstreamConfig{
			Enabled: true,
			Servers: []config.UPnPUpstreamServerConfig{{Name: "NAS", UDN: "uuid:nas", PathPrefix: "NAS"}},
		}
		ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://" + userinfo + host + "/ctl"},
			openIngestTestStore(t), nil)
		if err != nil {
			t.Fatalf("NewIngester: %v", err)
		}
		res, err := ing.Run(context.Background(), Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if len(res.PerServer) != 1 || res.PerServer[0].Err == nil {
			t.Fatalf("want one per-server error for a server that refuses every request: %+v", res.PerServer)
		}
		msg := res.PerServer[0].Err.Error()
		if strings.Contains(strings.ToLower(msg), strings.ToLower(secret)) || strings.Contains(msg, "@") {
			t.Errorf("the per-server error names the control URL's user information: %s", msg)
		}
		if !strings.Contains(msg, host) {
			t.Errorf("the per-server error does not name the server it failed on (%s): %s", host, msg)
		}
	}
}
