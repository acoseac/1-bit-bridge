package admin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHSTSOnlyInPublicModeOverTLS.
//
// The loopback exclusion is the important half. The loopback console is
// plain http://127.0.0.1:7789, and pinning HSTS for `localhost` poisons
// that host name in the operator's browser for every other local
// service they run — an unrelated dev server on 127.0.0.1 starts
// failing, and the fix is buried in chrome://net-internals.
func TestHSTSOnlyInPublicModeOverTLS(t *testing.T) {
	t.Run("public over TLS sends it", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		cfg.Deployment.Mode = "public"
		srv.deps.CfgHolder.Store(cfg)
		ts := httptest.NewTLSServer(srv.Handler())
		defer ts.Close()

		client := ts.Client()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		res, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("Strict-Transport-Security"); got == "" {
			t.Error("no HSTS on a public TLS response")
		}
	})

	t.Run("public over plain http does not", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		cfg.Deployment.Mode = "public"
		srv.deps.CfgHolder.Store(cfg)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()

		res, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS = %q on a plain-http response. A bridge behind a "+
				"TLS-terminating proxy serves the console over http on a private "+
				"interface; asserting HTTPS-only there is a claim it cannot back.", got)
		}
	})

	t.Run("loopback mode never sends it", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		ts := httptest.NewTLSServer(srv.Handler())
		defer ts.Close()
		client := ts.Client()
		client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		res, err := client.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if got := res.Header.Get("Strict-Transport-Security"); got != "" {
			t.Errorf("HSTS = %q in loopback mode — this pins `localhost` in the "+
				"operator's browser and breaks every other local service they run", got)
		}
	})
}
