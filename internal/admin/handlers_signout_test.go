package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// publicOrigin is the origin a browser sends to newPublicTestServer's
// console: the autocert domain, which the CSRF guard compares with.
const publicOrigin = "https://bridge.example.com"

// consoleRequest sends one request as a browser signed in with session would,
// with the Origin a browser's fetch sends, and returns the status and body.
// An empty origin sends none: a loopback test server listens on a port the
// guard does not know, and the guard refuses a mismatched Origin, never a
// missing one.
func consoleRequest(t *testing.T, ts *httptest.Server, origin, method, path, session string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// signIn mints a console session the way a login does, without the login's
// rate limiter or its bcrypt compare.
func signIn(t *testing.T, store *adminauth.Store) string {
	t.Helper()
	raw, err := store.CreateSession("admin")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestSignOutOtherSessionsEndsEveryOtherConsole drives the Devices page's
// "Sign out all other sessions" through the console's real handler chain: the
// CSRF guard, the session gate, the route. Every other browser is refused at
// its next request; the one that asked stays signed in.
func TestSignOutOtherSessionsEndsEveryOtherConsole(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	caller, other, another := signIn(t, store), signIn(t, store), signIn(t, store)

	status, body := consoleRequest(t, ts, publicOrigin, http.MethodPost, "/api/console-sessions/sign-out-others", caller)
	if status != http.StatusOK {
		t.Fatalf("POST sign-out-others = %d, want 200: %s", status, body)
	}
	var got signOutOthersResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.Ended != 2 {
		t.Errorf("ended = %d, want 2", got.Ended)
	}
	for _, s := range []string{other, another} {
		if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", s); status != http.StatusUnauthorized {
			t.Errorf("a signed-out browser's next request = %d, want 401", status)
		}
	}
	if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", caller); status != http.StatusOK {
		t.Errorf("the browser that signed the others out = %d on its next request, want 200", status)
	}
}

// TestSignOutOtherSessionsNeedsASession: the route is behind the session
// gate, like every /api route, so a browser that is not signed in cannot sign
// anyone out; and a loopback console, which has no sessions, answers that it
// has no auth.
func TestSignOutOtherSessionsNeedsASession(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	signedIn := signIn(t, store)

	if status, body := consoleRequest(t, ts, publicOrigin, http.MethodPost, "/api/console-sessions/sign-out-others", ""); status != http.StatusUnauthorized {
		t.Errorf("sign-out-others with no session = %d, want 401: %s", status, body)
	}
	if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", signedIn); status != http.StatusOK {
		t.Errorf("a refused sign-out-others signed a browser out: its next request = %d", status)
	}

	loopback, _, _ := newTestServer(t)
	lts := httptest.NewServer(loopback.Handler())
	defer lts.Close()
	if status, body := consoleRequest(t, lts, "", http.MethodPost, "/api/console-sessions/sign-out-others", ""); status != http.StatusServiceUnavailable {
		t.Errorf("sign-out-others on a loopback console = %d, want 503: %s", status, body)
	}
}

// TestAnUnreadableCredentialStoreRefusesWithoutALinePerRequest: a session
// check that cannot read the store refuses the request (the file may hold a
// sign-out), and the store logs that at most once a window. The middleware
// adds no line of its own, or every console request would add one while the
// file stays unreadable.
func TestAnUnreadableCredentialStoreRefusesWithoutALinePerRequest(t *testing.T) {
	srv, store, _, path := newPublicTestServerAt(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	signedIn := signIn(t, store)
	rec := loggingtest.Record(t)

	if err := os.WriteFile(path, []byte(`{"user": `), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		status, body := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", signedIn)
		if status != http.StatusServiceUnavailable || !strings.Contains(body, msgStoreUnreadable) {
			t.Fatalf("request %d with the store unreadable = %d %q, want 503 naming the store", i, status, body)
		}
	}
	if got := rec.Failures(); len(got) != 1 {
		t.Errorf("three refused requests logged %d lines at Warn or above, want the store's one:\n%s",
			len(got), strings.Join(got, "\n"))
	}
}

// TestDevicesPageOffersToSignOutTheOtherConsoles: in public mode the Devices
// page counts the browsers signed in, this one included, and the button is
// live only when there is another to sign out. A loopback console has no
// sessions and shows no panel.
func TestDevicesPageOffersToSignOutTheOtherConsoles(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	caller := signIn(t, store)

	_, alone := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/devices", caller)
	if !strings.Contains(alone, `id="console-sessions-panel"`) || !strings.Contains(alone, "Only this browser is signed in") {
		t.Errorf("the Devices page with one browser signed in has no panel saying so")
	}
	if !strings.Contains(alone, `id="sign-out-others" class="btn danger" disabled`) {
		t.Errorf("the sign-out button is live with no other browser to sign out")
	}

	signIn(t, store)
	signIn(t, store)
	_, three := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/devices", caller)
	if !strings.Contains(three, "3 browsers are signed in to this console, this one included.") {
		t.Errorf("the Devices page with three browsers signed in does not count them")
	}
	if strings.Contains(three, `id="sign-out-others" class="btn danger" disabled`) {
		t.Errorf("the sign-out button is disabled with two other browsers to sign out")
	}

	loopback, _, _ := newTestServer(t)
	lts := httptest.NewServer(loopback.Handler())
	defer lts.Close()
	if _, page := consoleRequest(t, lts, "", http.MethodGet, "/devices", ""); strings.Contains(page, "console-sessions-panel") {
		t.Errorf("a loopback console's Devices page shows the console sign-ins panel")
	}
}

// TestSignOutOtherSessionsSaysWhenItCouldNotSave: a write that fails after
// the sessions were ended answers 200 with saved false, not a failure: the
// other browsers are refused already, and the page says a restart before
// the bridge's next write would sign them back in. The store's directory is
// made read-only so the write cannot stage its file.
func TestSignOutOtherSessionsSaysWhenItCouldNotSave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores a directory's read-only attribute when creating a file in it")
	}
	if os.Geteuid() == 0 {
		t.Skip("root creates files in a directory whatever its mode")
	}
	srv, store, _, path := newPublicTestServerAt(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	caller, other := signIn(t, store), signIn(t, store)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	status, body := consoleRequest(t, ts, publicOrigin, http.MethodPost, "/api/console-sessions/sign-out-others", caller)
	if status != http.StatusOK {
		t.Fatalf("sign-out-others with a write that fails = %d, want 200: %s", status, body)
	}
	var got signOutOthersResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.Ended != 1 || got.Saved {
		t.Errorf("sign-out-others with a write that fails = %+v, want ended 1, saved false", got)
	}
	if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", other); status != http.StatusUnauthorized {
		t.Errorf("the other browser's next request = %d, want 401: it is signed out whether saved or not", status)
	}
}
