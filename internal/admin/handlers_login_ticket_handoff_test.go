package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
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
// not one header wide — and so it survives the PARAMETERS these headers really
// carry. `Sec-Purpose` is a structured field that arrives as
// `prefetch;anonymous-client-ip` in the wild, and the three legacy spellings
// are specified nowhere, so a proxy may hand back a list or a q-value. Each
// such case is one an exact match misses, and a miss here fails OPEN: the
// ticket is spent by the machine and the human's click lands on a bare form.
func TestSpeculativeFetchHeadersDoNotConsumeTheLoginTicket(t *testing.T) {
	for _, h := range []struct{ key, value string }{
		{"Sec-Purpose", "prefetch"},
		{"Sec-Purpose", "prefetch;prerender"},
		{"Sec-Purpose", "prefetch;anonymous-client-ip"},
		{"Sec-Purpose", `"prefetch"`},
		{"Purpose", "prefetch"},
		{"Purpose", "prefetch;q=0.8"},
		{"X-Purpose", "preview"},
		{"X-Purpose", "preview, prefetch"},
		{"X-Moz", "prefetch"},
		{"Sec-Fetch-Mode", "no-cors"},
		{"Sec-Fetch-Mode", "same-origin"},
	} {
		t.Run(h.key+"="+h.value, func(t *testing.T) {
			assertTicketSurvives(t, func(req *http.Request) {
				req.Header.Set(h.key, h.value)
			})
		})
	}
}

// A real navigation must still get through, so the guard cannot be "refuse
// anything that mentions one of these words". These are the near-misses a bare
// substring match would turn away.
func TestANavigationIsNotMistakenForSpeculation(t *testing.T) {
	for _, h := range []struct{ key, value string }{
		{"Sec-Purpose", "prefetching-is-not-a-token"},
		{"X-Purpose", "previewer"},
		{"Purpose", "instantiate"},
	} {
		t.Run(h.key+"="+h.value, func(t *testing.T) {
			srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			ticket, err := store.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/login/ticket?t="+ticket, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(h.key, h.value)
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if sessionCookie(resp) == nil {
				t.Errorf("a navigation carrying %s: %s was refused as speculative",
					h.key, h.value)
			}
		})
	}
}

// assertTicketSurvives drives one speculative request, then the human's real
// navigation to the SAME url.
//
// The second half is the assertion that matters: a 403 alone would pass against
// a handler that refused the prefetch after already redeeming it.
func assertTicketSurvives(t *testing.T, speculative func(*http.Request)) {
	t.Helper()
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
	speculative(req)
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if sessionCookie(resp) != nil {
		t.Error("a speculative fetch was handed a session cookie")
	}

	get, err := noRedirectClient().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if sessionCookie(get) == nil {
		t.Error("the ticket was burned by a speculative fetch")
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

// A store that cannot be written is a 500, never `?link=stale`.
//
// `link=stale` is an instruction as much as an explanation: it tells the user
// the link was the problem and that a fresh one will work. When the redeem
// failed because the ticket file could not be written, both halves are false —
// the record is still on disk, nothing was determined about the ticket, and a
// fresh link fails identically. It is also the only place an operator would
// learn the store has gone read-only, which is why the branch logs.
func TestAnUnwritableTicketStoreIsNotAStaleLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory mode does not gate file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this fixture depends on")
	}
	srv, _, _ := newPublicTestServer(t, "correct horse battery staple")

	// This store's directory is the fixture, so it is built here rather than
	// taken from the helper — which keeps its own temp dir to itself.
	dir := t.TempDir()
	store, err := adminauth.OpenStore(filepath.Join(dir, "adminauth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MintInitial("admin"); err != nil {
		t.Fatal(err)
	}
	srv.deps.AdminAuth = store

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Two, so the redeem's write takes the stage-and-rename path.
	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MintLoginTicket("admin"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	resp, err := noRedirectClient().Get(ts.URL + "/login/ticket?t=" + ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "link=stale") {
		t.Errorf("a store failure was reported to the user as a stale link (%q)", loc)
	}
	if sessionCookie(resp) != nil {
		t.Error("a failed redeem was handed a session cookie")
	}
}
