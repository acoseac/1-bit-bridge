package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCSRFContentTypeRequired pins the contract: a body-bearing
// mutating request without Content-Type: application/json gets 415.
func TestCSRFContentTypeRequired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	cases := []struct {
		name        string
		contentType string
		wantStatus  int
	}{
		{"text/plain rejected", "text/plain", http.StatusUnsupportedMediaType},
		{"form-encoded rejected", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"multipart rejected", "multipart/form-data; boundary=----X", http.StatusUnsupportedMediaType},
		{"missing Content-Type rejected (body present)", "", http.StatusUnsupportedMediaType},
		{"application/json passes Content-Type gate", "application/json", -1},
		{"application/json with charset passes", "application/json; charset=utf-8", -1},
		{"Application/JSON case-insensitive passes", "Application/JSON", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.NewBufferString(`{"path":"/tmp/whatever"}`)
			r := httptest.NewRequest(http.MethodPost, "/api/roots", body)
			r.RemoteAddr = "127.0.0.1:54321"
			r.Host = testConsoleHost
			if tc.contentType != "" {
				r.Header.Set("Content-Type", tc.contentType)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if tc.wantStatus == -1 {
				// Just assert we got past the CSRF gate (not 415, not 403).
				if w.Code == http.StatusUnsupportedMediaType || w.Code == http.StatusForbidden {
					t.Errorf("expected to pass CSRF gate, got %d: %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body=%q)", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// TestCSRFOriginAllowlist pins: cross-origin POSTs with mismatched
// Origin → 403; missing Origin → allowed; same-origin Origin →
// allowed.
func TestCSRFOriginAllowlist(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	cases := []struct {
		name       string
		origin     string
		wantStatus int
	}{
		{"missing Origin allowed", "", -1},
		{"same-origin (127.0.0.1) allowed", "http://127.0.0.1:7789", -1},
		{"localhost allowed (resolves loopback)", "http://localhost:7789", -1},
		{"cross-origin attacker.com rejected", "https://attacker.com", http.StatusForbidden},
		{"loopback wrong-port rejected", "http://127.0.0.1:7777", http.StatusForbidden},
		{"non-loopback IP rejected", "http://192.168.1.5:7789", http.StatusForbidden},
		{"null Origin rejected (file:// drive-by)", "null", http.StatusForbidden},
		{"IPv6 [::1] same port allowed", "http://[::1]:7789", -1},
		{"IPv6 [::1] wrong port rejected", "http://[::1]:7777", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.NewBufferString(`{"path":"/tmp/whatever"}`)
			r := httptest.NewRequest(http.MethodPost, "/api/roots", body)
			r.RemoteAddr = "127.0.0.1:54321"
			r.Host = testConsoleHost
			r.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if tc.wantStatus == http.StatusForbidden {
				if w.Code != http.StatusForbidden {
					t.Errorf("status = %d, want 403; body=%s", w.Code, w.Body.String())
				}
				return
			}
			// Allowed by CSRF — assert we got past it (handler may have
			// returned its own status for other reasons).
			if w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "cross-origin") {
				t.Errorf("CSRF blocked when it should have allowed: %s", w.Body.String())
			}
		})
	}
}

// TestCSRFCrossOriginRefusalCarriesCodeAndTunnelHint pins the B471
// contract: a loopback Origin on another port is still refused (the
// allowlist is unchanged), the matching Origin is still admitted, and a
// refusal carries the typed `cross_origin` code plus the admin-listener
// port in the message the console shows — not a bare "403 Forbidden".
func TestCSRFCrossOriginRefusalCarriesCodeAndTunnelHint(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	post := func(origin string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"path":"/tmp/whatever"}`)
		r := httptest.NewRequest(http.MethodPost, "/api/roots", body)
		r.RemoteAddr = "127.0.0.1:54321"
		r.Host = testConsoleHost
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	wrong := post("http://127.0.0.1:17789")
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("loopback Origin on another port: status %d, want 403; body=%s", wrong.Code, wrong.Body.String())
	}
	var env map[string]string
	if err := json.Unmarshal(wrong.Body.Bytes(), &env); err != nil {
		t.Fatalf("refusal body must be JSON errorFromResponse can read, got %q: %v", wrong.Body.String(), err)
	}
	if env["error"] != errCodeCrossOrigin {
		t.Errorf("error code = %q, want %q", env["error"], errCodeCrossOrigin)
	}
	msg := env["message"]
	if !strings.Contains(msg, "admin refused: cross-origin request") {
		t.Errorf("message %q must keep the server's own refusal text", msg)
	}
	if !strings.Contains(msg, "http://127.0.0.1:7789") {
		t.Errorf("message %q must name the admin listener (7789), not the page Origin's port", msg)
	}
	if !strings.Contains(msg, "ssh -L 7789:127.0.0.1:7789") {
		t.Errorf("message %q must show an equal-port tunnel example using the admin port", msg)
	}
	if strings.Contains(msg, "17789") {
		t.Errorf("message %q must not take the port from the page URL", msg)
	}

	ok := post("http://127.0.0.1:7789")
	if ok.Code == http.StatusForbidden && strings.Contains(ok.Body.String(), "cross-origin") {
		t.Fatalf("matching loopback Origin must pass the guard; got %d %s", ok.Code, ok.Body.String())
	}
}

// TestCrossOriginRefusalHintUsesAdminListenHost pins that the loopback
// tunnel hint takes its host from the same bound/configured admin
// address as the port (B471 review): IPv6 gets brackets in the URL and
// ssh -L destination, localhost and IPv4 stay literal, and an empty or
// unspecified host falls back to 127.0.0.1. The Origin allowlist is
// unchanged — every case still refuses a wrong-port loopback Origin.
func TestCrossOriginRefusalHintUsesAdminListenHost(t *testing.T) {
	cases := []struct {
		name      string
		adminAddr string
		wantURL   string
		wantSSH   string
	}{
		{
			name:      "ipv4 loopback",
			adminAddr: "127.0.0.1:7789",
			wantURL:   "http://127.0.0.1:7789",
			wantSSH:   "ssh -L 7789:127.0.0.1:7789",
		},
		{
			name:      "ipv6 loopback",
			adminAddr: "[::1]:7789",
			wantURL:   "http://[::1]:7789",
			wantSSH:   "ssh -L 7789:[::1]:7789",
		},
		{
			name:      "localhost",
			adminAddr: "localhost:7789",
			wantURL:   "http://localhost:7789",
			wantSSH:   "ssh -L 7789:localhost:7789",
		},
		{
			name:      "unspecified host",
			adminAddr: "0.0.0.0:7789",
			wantURL:   "http://127.0.0.1:7789",
			wantSSH:   "ssh -L 7789:127.0.0.1:7789",
		},
		{
			name:      "empty host",
			adminAddr: ":7789",
			wantURL:   "http://127.0.0.1:7789",
			wantSSH:   "ssh -L 7789:127.0.0.1:7789",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.boundAdminAddr = tc.adminAddr
			body := bytes.NewBufferString(`{"path":"/tmp/whatever"}`)
			r := httptest.NewRequest(http.MethodPost, "/api/roots", body)
			r.RemoteAddr = "127.0.0.1:54321"
			r.Host = testConsoleHost
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", "http://127.0.0.1:17789")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403; body=%s", w.Code, w.Body.String())
			}
			var env map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("refusal body must be JSON, got %q: %v", w.Body.String(), err)
			}
			msg := env["message"]
			if !strings.Contains(msg, tc.wantURL) {
				t.Errorf("message %q must contain URL %q", msg, tc.wantURL)
			}
			if !strings.Contains(msg, tc.wantSSH) {
				t.Errorf("message %q must contain tunnel example %q", msg, tc.wantSSH)
			}
			if tc.name == "ipv6 loopback" && strings.Contains(msg, "127.0.0.1") {
				t.Errorf("IPv6-bound admin must not hardcode 127.0.0.1 in the hint; got %q", msg)
			}
		})
	}
}

// TestErrorFromResponseSurfacesCrossOriginRefusal runs the console's
// errorFromResponse under node against the typed refusal body, so a
// regression that drops the JSON (or the code) shows as "403 Forbidden"
// again instead of the server's message.
func TestErrorFromResponseSurfacesCrossOriginRefusal(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t, readFile(t, "static/app.js"), "errorFromResponse")
	script := t.TempDir() + "/cross_origin_err.js"
	src := fn + `
(async () => {
  const body = {
    error: "cross_origin",
    message: "admin refused: cross-origin request. This console must be opened at http://127.0.0.1:7789. An SSH tunnel has to keep that port on both ends, for example ssh -L 7789:127.0.0.1:7789 …",
  };
  const r = {
    ok: false,
    status: 403,
    statusText: "Forbidden",
    async json() { return body; },
  };
  const e = await errorFromResponse(r);
  if (e.code !== "cross_origin") {
    throw new Error("code=" + e.code);
  }
  if (!String(e.message).includes("admin refused: cross-origin request")) {
    throw new Error("message lost the server text: " + e.message);
  }
  if (!String(e.message).includes("http://127.0.0.1:7789")) {
    throw new Error("message lost the admin-port hint: " + e.message);
  }
  if (String(e.message) === "403 Forbidden") {
    throw new Error("fell back to status text");
  }
  process.stdout.write("ok");
})().catch((err) => { console.error(err); process.exit(1); });
`
	if err := os.WriteFile(script, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		t.Fatalf("unexpected output %q", out)
	}
}

// TestCSRFGetHeadAllowed pins: GET / HEAD pass through unconditionally
// (no body to abuse, no state mutation).
func TestCSRFGetHeadAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/api/stats", nil)
		r.RemoteAddr = "127.0.0.1:54321"
		r.Host = testConsoleHost
		// No Content-Type, malicious-looking Origin — must still pass.
		r.Header.Set("Origin", "https://attacker.com")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusUnsupportedMediaType || w.Code == http.StatusForbidden {
			t.Errorf("%s should pass CSRF unconditionally, got %d", method, w.Code)
		}
	}
}

// TestCSRFBodylessMutationAllowed pins: POST with no body (e.g. /api/scan,
// /api/restart) passes without Content-Type since there's nothing to
// abuse. Handlers that need a body still return 400 on empty input.
func TestCSRFBodylessMutationAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	r := httptest.NewRequest(http.MethodPost, "/api/scan", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Host = testConsoleHost
	// No Content-Type, no Origin — bodyless POST is allowed.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusUnsupportedMediaType {
		t.Errorf("bodyless POST should pass without Content-Type, got 415")
	}
}
