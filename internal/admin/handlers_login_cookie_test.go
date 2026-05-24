package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// TestSessionCookieMaxAgeIsHardCap pins the Gemini medium fix
// post-PR-#292: the session cookie's MaxAge is the
// SessionHardCap (7 days), not SessionIdleTimeout (24 hours).
// Pre-fix, the browser deleted the cookie at 24h regardless of
// activity, making the 24h idle timeout an effective hard cap
// for the client — the server's 7-day hard cap was unreachable.
//
// Now the cookie persists for 7 days; server-side
// ValidateSession enforces the 24h idle window by returning
// ErrSessionExpired on the next request after activity stops.
// Active operators get their LastUsedAt bumped on every API
// call so the effective hard cap is reachable.
func TestSessionCookieMaxAgeIsHardCap(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"username":"admin","password":"test-password-123"}`
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/login: status %d, want 200", resp.StatusCode)
	}

	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			sess = c
			break
		}
	}
	if sess == nil {
		t.Fatal("no session cookie in /login response")
	}

	wantMaxAge := int(adminauth.SessionHardCap.Seconds())
	if sess.MaxAge != wantMaxAge {
		t.Errorf("MaxAge = %d sec (%.0fh), want %d sec (%.0fh) — SessionHardCap, not SessionIdleTimeout",
			sess.MaxAge, float64(sess.MaxAge)/3600,
			wantMaxAge, adminauth.SessionHardCap.Hours())
	}
	// Defensive: also pin that MaxAge is NOT the (smaller) idle
	// timeout — catches a future refactor that swaps the
	// constants accidentally.
	idleSeconds := int(adminauth.SessionIdleTimeout.Seconds())
	if sess.MaxAge == idleSeconds {
		t.Errorf("MaxAge regressed back to SessionIdleTimeout (%d sec) — browser would delete cookie at the idle bound, defeating the hard cap",
			idleSeconds)
	}
}
