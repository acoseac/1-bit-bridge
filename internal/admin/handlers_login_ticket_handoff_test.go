package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A GET that is not a navigation must not spend the ticket.
//
// HEAD is refused elsewhere, and that covers a prober which ASKS about the URL.
// This is the one that FETCHES it: an unfurler, a prefetcher, a mail-security
// scanner. Because redemption deletes before judging, such a GET spends the
// credential and the human's real click then lands on a bare login form.
//
// The assertion that matters is the SECOND one — the same ticket must still
// work afterwards. A 403 alone would pass against a handler that refused the
// prefetch after already redeeming it.
func TestAPrefetchDoesNotConsumeTheLoginTicket(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/login/ticket?t=" + ticket

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Mode", "cors")
	pre, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	pre.Body.Close()
	if pre.StatusCode != http.StatusForbidden {
		t.Errorf("prefetch status = %d, want 403", pre.StatusCode)
	}
	if sessionCookie(pre) != nil {
		t.Error("a prefetch was handed a session cookie")
	}

	nav, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	nav.Header.Set("Sec-Fetch-Mode", "navigate")
	get, err := noRedirectClient().Do(nav)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if sessionCookie(get) == nil {
		t.Fatal("the human's navigation after a prefetch got no session cookie — " +
			"the prefetch burned the ticket")
	}
}

// The other spellings of "I am fetching this speculatively", so the guard is
// not one header wide.
func TestSpeculativeFetchHeadersDoNotConsumeTheLoginTicket(t *testing.T) {
	for _, h := range []struct{ key, value string }{
		{"Sec-Purpose", "prefetch"},
		{"Sec-Purpose", "prefetch;prerender"},
		{"Purpose", "prefetch"},
		{"X-Purpose", "preview"},
		{"X-Moz", "prefetch"},
		{"Sec-Fetch-Mode", "no-cors"},
		{"Sec-Fetch-Mode", "same-origin"},
	} {
		t.Run(h.key+"="+h.value, func(t *testing.T) {
			srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			ticket, err := store.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}
			url := ts.URL + "/login/ticket?t=" + ticket

			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(h.key, h.value)
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}

			// The ticket survived, which is the whole point.
			get, err := noRedirectClient().Get(url)
			if err != nil {
				t.Fatal(err)
			}
			defer get.Body.Close()
			if sessionCookie(get) == nil {
				t.Error("the ticket was burned by a speculative fetch")
			}
		})
	}
}

// Fails OPEN, deliberately. curl, an older browser and the operator's own
// `bridge admin login-link` flow send no fetch-metadata headers at all, and
// turning those away would break the path the guard exists to serve. Absent is
// not a declaration.
func TestABareGetWithNoFetchMetadataStillRedeems(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	get, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", get.StatusCode)
	}
	if sessionCookie(get) == nil {
		t.Error("a header-less GET — curl, or the operator's shell flow — was refused")
	}
}

// The natural recovery is to re-open the same link, and that can NEVER work
// once any touch has spent it. Without a marker the form gives a user no reason
// to think otherwise, which is how a one-off expiry reads as "it is broken".
func TestAFailedRedeemPointsAtAStaleLink(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=never-existed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login?link=stale" {
		t.Errorf("Location = %q, want /login?link=stale", got)
	}
	if sessionCookie(resp) != nil {
		t.Error("a bogus ticket was handed a session cookie")
	}
}

// And the form renders it — but only when it applies, so an ordinary visit is
// not told something went wrong.
func TestTheLoginFormExplainsAStaleLinkOnlyWhenItApplies(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body := func(path string) string {
		resp, err := noRedirectClient().Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	stale := body("/login?link=stale")
	if !strings.Contains(stale, "expired or was already used") {
		t.Error("the stale-link form does not explain what happened")
	}
	if !strings.Contains(stale, "login-notice") {
		t.Error("the notice is not rendered in its own element")
	}

	plain := body("/login")
	if strings.Contains(plain, "expired or was already used") {
		t.Error("an ordinary visit is told a link went stale")
	}
}
