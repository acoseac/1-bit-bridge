package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// `?link=stale` rather than a bare /login: the form has to be able to say
	// the LINK was the problem, because re-opening the same one — the obvious
	// recovery — can never work once any touch has spent it. What must not be
	// revealed is WHICH of unknown/expired/already-used applies, and this
	// marker is the union of all three; see TestBadLoginTicketGrantsNothing,
	// which pins that indistinguishability directly.
	if loc := replay.Header.Get("Location"); loc != "/login?link=stale" {
		t.Errorf("a spent ticket redirected to %q, want /login?link=stale", loc)
	}
}

// An unusable ticket must set no cookie and DISTINGUISH NOTHING.
//
// This used to be phrased "explain nothing" and pinned by comparing Location
// against a bare "/login". The redirect now carries `?link=stale`, which does
// explain something — and the distinction matters, so it is stated rather than
// quietly reinterpreted. What would be a leak is revealing WHICH of unknown,
// expired or already-used applies, because that tells a caller whether a given
// ticket ever existed. `?link=stale` is the union of the three and is returned
// for a ticket that never existed, so it reveals nothing about any of them.
//
// So the assertion is now the property the original was reaching for through a
// literal: every unusable shape must produce the IDENTICAL response. That is
// strictly stronger — the old form would have passed a handler that returned
// "/login" for one shape and "/login?why=expired" for another, as long as the
// first case it happened to check was the bare one.
func TestBadLoginTicketGrantsNothing(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// An EXPIRED ticket is the one of the three shapes that needs a real mint
	// to produce, and without it this table proves the property only for
	// tickets that never existed — while the notice speaks for all three.
	// Milliseconds, then a sleep well past the ~15.6 ms wall-clock granularity
	// the Windows leg has, so the expiry is a fact and not a race.
	expired, err := store.MintLoginTicketTTL("admin", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	seen := map[string]int{}
	for _, q := range []string{
		"", "?t=", "?t=nonsense", "?t=" + strings.Repeat("A", 43), "?t=" + expired,
	} {
		resp, err := noRedirectClient().Get(ts.URL + "/login/ticket" + q)
		if err != nil {
			t.Fatal(err)
		}
		if c := sessionCookie(resp); c != nil && c.Value != "" {
			t.Errorf("query %q produced a session cookie", q)
		}
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusFound || loc != "/login?link=stale" {
			t.Errorf("query %q = %d -> %q, want a 302 to /login?link=stale",
				q, resp.StatusCode, loc)
		}
		seen[loc]++
		resp.Body.Close()
	}
	if len(seen) != 1 {
		t.Errorf("unusable tickets are distinguishable by their redirect: %v", seen)
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
