package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := bytes.NewBufferString(`{"path":"/tmp/whatever"}`)
			r := httptest.NewRequest(http.MethodPost, "/api/roots", body)
			r.RemoteAddr = "127.0.0.1:54321"
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

// TestCSRFGetHeadAllowed pins: GET / HEAD pass through unconditionally
// (no body to abuse, no state mutation).
func TestCSRFGetHeadAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		r := httptest.NewRequest(method, "/api/stats", nil)
		r.RemoteAddr = "127.0.0.1:54321"
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
	// No Content-Type, no Origin — bodyless POST is allowed.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusUnsupportedMediaType {
		t.Errorf("bodyless POST should pass without Content-Type, got 415")
	}
}
