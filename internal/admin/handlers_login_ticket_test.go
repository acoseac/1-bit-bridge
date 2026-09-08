package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// noRedirect keeps the 302 visible so the test can assert on it.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

// A valid ticket logs the browser in: session cookie set, redirected onward,
// and the session actually works against an authenticated endpoint.
func TestLoginTicketAuthenticatesTheBrowser(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("redirected to %q, want /", loc)
	}
	c := sessionCookie(resp)
	if c == nil {
		t.Fatal("no session cookie was set")
	}
	// The cookie must actually authenticate.
	req, _ := http.NewRequest("GET", ts.URL+"/api/stats", nil)
	req.AddCookie(c)
	authed, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer authed.Body.Close()
	if authed.StatusCode != http.StatusOK {
		t.Errorf("the minted session did not authenticate: %d", authed.StatusCode)
	}
	// The ticket must not be reusable.
	replay, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Body.Close()
	if sessionCookie(replay) != nil {
		t.Error("replaying the ticket produced a second session")
	}
	if loc := replay.Header.Get("Location"); loc != "/login" {
		t.Errorf("a spent ticket redirected to %q, want /login", loc)
	}
}

// An unusable ticket must set no cookie and explain nothing.
func TestBadLoginTicketGrantsNothing(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, q := range []string{"", "?t=", "?t=nonsense", "?t=" + strings.Repeat("A", 43)} {
		resp, err := noRedirectClient().Get(ts.URL + "/login/ticket" + q)
		if err != nil {
			t.Fatal(err)
		}
		if c := sessionCookie(resp); c != nil && c.Value != "" {
			t.Errorf("query %q produced a session cookie", q)
		}
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login" {
			t.Errorf("query %q = %d -> %q, want a 302 to /login", q, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp.Body.Close()
	}
}

// The ticket rides in a URL, so the response must not let it leak onward or be
// cached by anything in between.
func TestLoginTicketResponseDoesNotLeakTheCredential(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, ticket) {
		t.Error("the redirect target carries the ticket onward")
	}
}

// A ticket link takes nobody anywhere but the console root. The `next`
// parameter was deliberately removed rather than validated: this would
// otherwise be the only handler feeding a caller-supplied value to
// http.Redirect, which is a question worth not having on a login path.
func TestLoginTicketIgnoresAnyRedirectTarget(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, next := range []string{"/library", "https://evil.example/x", "//evil.example/x", "not-a-path"} {
		ticket, err := store.MintLoginTicket("admin")
		if err != nil {
			t.Fatal(err)
		}
		resp, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket + "&next=" + next)
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("next=%q redirected to %q, want / — no caller-supplied target may be honoured", next, got)
		}
		resp.Body.Close()
	}
}
