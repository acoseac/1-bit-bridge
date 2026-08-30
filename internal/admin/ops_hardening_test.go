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

// TestMetricsGateAllowsLoopbackAndConfiguredCIDRsOnly.
//
// The default posture must be byte-for-byte what it was: loopback in,
// everything else out. The CIDR list exists because that default is
// UNREACHABLE in a container — a Prometheus outside the network
// namespace gets a 403, and the only workaround was a sidecar whose
// whole job is to be on the right side of the check.
func TestMetricsGateAllowsLoopbackAndConfiguredCIDRsOnly(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.Metrics.AllowCIDRs = []string{"10.42.0.0/16", "not-a-cidr"}
	srv.deps.CfgHolder.Store(cfg)

	h := srv.metricsGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		remote string
		want   int
		why    string
	}{
		{"127.0.0.1:5000", http.StatusOK, "loopback is always allowed and needs no entry"},
		{"[::1]:5000", http.StatusOK, "IPv6 loopback too"},
		{"10.42.7.9:5000", http.StatusOK, "inside the configured monitoring range"},
		{"10.43.7.9:5000", http.StatusForbidden, "outside it"},
		{"203.0.113.5:5000", http.StatusForbidden, "the internet is never allowed"},
		{"garbage", http.StatusForbidden, "an unparseable RemoteAddr fails closed"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = c.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("remote %s = %d, want %d — %s", c.remote, w.Code, c.want, c.why)
		}
	}
}

// TestMetricsGateDefaultIsLoopbackOnly is the control: with no CIDRs
// configured, nothing outside loopback gets in. A widening that
// happened by default would be the worst possible outcome here.
func TestMetricsGateDefaultIsLoopbackOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.metricsGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, remote := range []string{"10.42.7.9:5000", "192.168.1.5:5000", "203.0.113.5:5000"} {
		req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		req.RemoteAddr = remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Errorf("remote %s = %d with no CIDRs configured, want 403", remote, w.Code)
		}
	}
}
