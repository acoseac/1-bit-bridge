package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMetricsLoopbackBypassesSessionInPublicMode is the F1 regression:
// in public mode `boundaryMiddleware` bypasses its loopbackOnly wrap, so
// /metrics is gated by its OWN loopbackOnly at registration + sits on
// isAuthBypassPath. A same-host scraper with no session cookie must get
// 200, not a 302 to /login (the pre-fix behaviour that broke local
// Prometheus scraping on public bridges).
func TestMetricsLoopbackBypassesSessionInPublicMode(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Errorf("public-mode loopback /metrics: got %d, want 200 (loopbackOnly allows + session bypass)", rw.Code)
	}
}

// TestMetricsRejectsNonLoopbackInPublicMode pins the other half: even
// though the boundary loopback gate is bypassed in public mode, the
// registration-level loopbackOnly still refuses a non-loopback scrape
// (403), so /metrics never leaks off-host. Pre-fix this path fell to
// sessionMiddleware and returned a 302, not a hard refusal.
func TestMetricsRejectsNonLoopbackInPublicMode(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "192.168.1.5:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("public-mode LAN /metrics: got %d, want 403 (registration-level loopbackOnly rejects)", rw.Code)
	}
}
