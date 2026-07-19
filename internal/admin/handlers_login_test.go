package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// newPublicTestServer extends newTestServer with public-mode auth
// wiring: deployment.mode=public, an AdminTLSTerminatedByProxy
// flag (so config validation accepts the posture), and an
// AdminAuth store seeded with a known password.
func newPublicTestServer(t *testing.T, password string) (*Server, *adminauth.Store, *adminauth.RateLimiter) {
	t.Helper()
	srv, cfg, _ := newTestServer(t)

	// Flip to public mode in the live config. The Server reads the
	// holder per-request, so the middleware sees the new posture
	// immediately.
	cfg.Deployment.Mode = "public"
	cfg.Deployment.AdminTLSTerminatedByProxy = true
	cfg.Autocert.Domain = "bridge.example.com"
	srv.deps.CfgHolder.Store(cfg)

	store, err := adminauth.OpenStore(t.TempDir() + "/adminauth.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetPassword("admin", password); err != nil {
		t.Fatal(err)
	}
	limiter := adminauth.NewRateLimiter()
	t.Cleanup(limiter.Stop)

	srv.deps.AdminAuth = store
	srv.deps.LoginLimiter = limiter
	return srv, store, limiter
}

// TestLoopbackModeIsUnauthenticated regression-guards the
// historical invariant: a loopback-mode admin install MUST stay
// auth-free even after the auth machinery lands. The session
// middleware must short-circuit when cfg.IsPublic() is false.
func TestLoopbackModeIsUnauthenticated(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback /api/stats: status %d, want 200 (auth must not kick in for loopback)", resp.StatusCode)
	}
}

// TestPublicModeApiRequiresSession pins the JSON-401 path: a
// public-mode /api/* request without a valid session cookie must
// return 401 with a JSON envelope (not redirect — APIs don't
// follow redirects by default and an HTML 302 would confuse the
// caller).
func TestPublicModeApiRequiresSession(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// No cookie → 401.
	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("public /api/stats no-cookie: status %d, want 401", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("public /api/stats no-cookie: Content-Type %q, want application/json", ct)
	}
}

// TestPublicModePageRedirectsToLogin pins the HTML-302 path: a
// public-mode page request without a valid session cookie must
// redirect to /login with the original path as `next=`.
func TestPublicModePageRedirectsToLogin(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Disable automatic redirect-following so we can inspect the 302.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(ts.URL + "/library")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("public /library no-cookie: status %d, want 302", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location = %q, want /login?next=...", loc)
	}
	if !strings.Contains(loc, "%2Flibrary") {
		t.Errorf("Location %q should carry the original path as URL-encoded next=", loc)
	}
}

// TestLoginPageBypassesAuth pins that the login route itself is
// reachable without a session — otherwise no operator could ever
// log in.
func TestLoginPageBypassesAuth(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/login no-cookie: status %d, want 200", resp.StatusCode)
	}
}

// TestStaticAssetsBypassAuth pins the asset bypass — the login
// page references /static/app.css and /static/bridge-mark.png, so
// those MUST be reachable without a session or the unauthenticated
// page renders broken.
func TestStaticAssetsBypassAuth(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/static/app.css no-cookie: status %d, want 200", resp.StatusCode)
	}
}

// TestLoginSuccessSetsSessionCookieAndUnlocksApi: end-to-end happy
// path through the auth surface. Post valid credentials, get a
// Secure session cookie, then use it to access /api/stats.
func TestLoginSuccessSetsSessionCookieAndUnlocksApi(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Need a client with a cookie jar AND an Origin header (csrfGuard).
	jar, _ := newCookieJar()
	client := &http.Client{Jar: jar}

	body := `{"username":"admin","password":"test-password-123"}`
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := readResponseSnippet(resp)
		t.Fatalf("/login status %d, want 200; body: %s", resp.StatusCode, buf)
	}

	// Cookie jar should now carry the session cookie.
	u := mustParseURL(t, ts.URL)
	cookies := jar.Cookies(u)
	found := false
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			found = true
			if c.Value == "" {
				t.Errorf("session cookie has empty value")
			}
			break
		}
	}
	if !found {
		t.Fatalf("login did not set %s cookie (cookies: %v)", sessionCookieName, cookies)
	}

	// Now /api/stats should be reachable.
	req, _ = http.NewRequest("GET", ts.URL+"/api/stats", nil)
	req.Header.Set("Origin", "https://bridge.example.com")
	apiResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusOK {
		t.Errorf("/api/stats post-login: status %d, want 200", apiResp.StatusCode)
	}
}

// TestLoginFailureKeepsSessionLocked pins the error path: wrong
// password returns 401, doesn't set a cookie, doesn't unlock /api/*.
func TestLoginFailureKeepsSessionLocked(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := newCookieJar()
	client := &http.Client{Jar: jar}

	body := `{"username":"admin","password":"wrong"}`
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/login wrong password: status %d, want 401", resp.StatusCode)
	}
	// No session cookie should be set.
	u := mustParseURL(t, ts.URL)
	for _, c := range jar.Cookies(u) {
		if c.Name == sessionCookieName && c.Value != "" {
			t.Errorf("/login failure leaked a session cookie: %v", c)
		}
	}
}

// TestLoginRateLimitCountsFailures pins the B43 wiring: the login handler
// must count each failed attempt via AllowAndReserve, so after
// RateLimitMaxAttempts failures the (IP, user) bucket is full and the next
// attempt is locked out. A regressed wiring (check-only Allow that never
// reserves, or a dropped counting call) would leave the bucket empty and
// never lock out. Asserted against the injected limiter directly (Allow is
// read-only) so the test stays fast — it doesn't pay the 5 s throttle sleep.
func TestLoginRateLimitCountsFailures(t *testing.T) {
	srv, _, limiter := newPublicTestServer(t, "correct-horse-battery")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for i := 0; i < adminauth.RateLimitMaxAttempts; i++ {
		req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://bridge.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed login %d: %v", i+1, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failed login %d: status %d, want 401", i+1, resp.StatusCode)
		}
	}
	// httptest binds loopback, so the handler's ExtractClientIP resolves to
	// 127.0.0.1. After MaxAttempts reservations the bucket is at the ceiling.
	if limiter.Allow("127.0.0.1", "admin") {
		t.Error("after MaxAttempts failed logins the (IP, user) is NOT locked out — the handler stopped counting failures (B43 wiring regressed)")
	}
}

// TestLoginGenericErrorMessage pins the response shape: the wire
// must say "invalid credentials" regardless of whether the
// username or password was wrong. Disclosing which half failed
// gives an attacker a username-enumeration oracle.
func TestLoginGenericErrorMessage(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, body := range []string{
		`{"username":"admin","password":"wrong"}`,
		`{"username":"who-knows","password":"test-password-123"}`,
	} {
		req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://bridge.example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var envelope map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		// The error message MUST be the same generic shape for
		// both failure modes — otherwise the response leaks
		// whether the username exists.
		if envelope["error"] != "invalid_credentials" {
			t.Errorf("body %q: error = %v, want invalid_credentials", body, envelope["error"])
		}
	}
}

// TestLoginRefusesUnsafeNextRedirect pins the open-redirect guard
// at the login response: a posted `next=//attacker.com` must be
// rewritten to "/" rather than echoed back verbatim (which the JS
// would then navigate to).
func TestLoginRefusesUnsafeNextRedirect(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"username":"admin","password":"test-password-123","next":"//attacker.com/path"}`
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var envelope map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	if envelope["next"] != "/" {
		t.Errorf("unsafe next echoed back: got %v, want /", envelope["next"])
	}
}

// TestPublicModeOriginAllowlistAcceptsConfiguredDomain pins the
// public-mode Origin allowlist: a request from the operator's
// configured Autocert.Domain must pass csrfGuard.
func TestPublicModeOriginAllowlistAcceptsConfiguredDomain(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Reverse-proxy posture: AdminTLSTerminatedByProxy=true means
	// the browser carries Origin: https://bridge.example.com (no
	// port = 443), but the bridge's admin listener is on
	// :7789. Without the proxy-aware host-only match, csrfGuard
	// would refuse with a port mismatch.
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
		t.Errorf("origin from configured domain: status %d, want 200", resp.StatusCode)
	}
}

// TestPublicModeOriginAllowlistRefusesAttacker pins the rejection
// path: an Origin from anywhere else must be refused with 403.
func TestPublicModeOriginAllowlistRefusesAttacker(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := `{"username":"admin","password":"test-password-123"}`
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://attacker.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("attacker origin: status %d, want 403", resp.StatusCode)
	}
}

// TestLogoutInvalidatesSession: post-logout, the same session
// cookie returns 401 from /api/*.
func TestLogoutInvalidatesSession(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jar, _ := newCookieJar()
	client := &http.Client{Jar: jar}
	loginAndExpectOK(t, client, ts.URL, "admin", "test-password-123")

	// Logout.
	req, _ := http.NewRequest("POST", ts.URL+"/logout", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/logout: status %d, want 200", resp.StatusCode)
	}

	// /api/stats should now be unauthenticated again.
	req, _ = http.NewRequest("GET", ts.URL+"/api/stats", nil)
	req.Header.Set("Origin", "https://bridge.example.com")
	apiResp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer apiResp.Body.Close()
	if apiResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("/api/stats post-logout: status %d, want 401", apiResp.StatusCode)
	}
}

// TestLoginCookieAttributes pins the security attributes on the
// session cookie: HttpOnly, SameSite=Strict, Secure (in public
// mode). Without these the cookie can be exfiltrated via JS or
// sent over plain HTTP.
func TestLoginCookieAttributes(t *testing.T) {
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
		t.Fatalf("/login: status %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	var sess *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionCookieName {
			sess = c
			break
		}
	}
	if sess == nil {
		t.Fatal("no session cookie in /login response")
	}
	if !sess.HttpOnly {
		t.Error("session cookie missing HttpOnly")
	}
	if !sess.Secure {
		t.Error("session cookie missing Secure (public mode)")
	}
	if sess.SameSite != http.SameSiteStrictMode {
		t.Errorf("session cookie SameSite = %v, want Strict", sess.SameSite)
	}
}
