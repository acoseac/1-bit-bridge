package admin

import (
	"io"
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

// redeemTicket is the interstitial's one button, submitted the way a browser
// submits a form with no successful controls: `Content-Type:
// application/x-www-form-urlencoded` with an EMPTY body (Content-Length: 0),
// the ticket in the action's query. The content type is deliberate — a nil
// body with no content type would pass csrfGuard even if the form grew a
// named control and a real browser started sending a urlencoded body the
// guard 415s (CodeRabbit on #909); assertInterstitial pins the other half,
// that the form has no such control.
func redeemTicket(t *testing.T, base, query string, decorate ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/login/ticket"+query, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, d := range decorate {
		d(req)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// openLink is the human's click on the link itself: a plain GET.
func openLink(t *testing.T, base, query string, decorate ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/login/ticket"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range decorate {
		d(req)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// assertInterstitial checks a GET rendered the one-button page for `ticket`:
// 200, no session, and a form whose POST action carries the ticket — the only
// place it may appear.
func assertInterstitial(t *testing.T, resp *http.Response, ticket string) {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("opening the link: status = %d, want 200 (the interstitial)", resp.StatusCode)
	}
	if sessionCookie(resp) != nil {
		t.Error("opening the link handed out a session cookie — the GET must redeem nothing")
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	action := `action="/login/ticket?t=` + ticket + `"`
	if !strings.Contains(body, `method="post"`) || !strings.Contains(body, action) {
		t.Errorf("the interstitial does not post the ticket back: want %s in\n%s", action, body)
	}
	if strings.Contains(body, "<script") {
		t.Error("the interstitial carries a script — a previewer runs scripts; only a click may redeem")
	}
	if strings.Count(body, ticket) != 1 {
		t.Errorf("the ticket appears %d times in the page, want exactly once (the form action)",
			strings.Count(body, ticket))
	}
	// The form must have NO successful control: a browser encodes any named
	// field into a urlencoded body, and csrfGuard refuses a body-bearing POST
	// that is not JSON — the click would 415. The empty body is the contract
	// redeemTicket submits under.
	start := strings.Index(body, "<form")
	end := strings.Index(body, "</form>")
	if start < 0 || end < start {
		t.Fatal("no form in the interstitial")
	}
	if form := body[start:end]; strings.Contains(form, " name=") {
		t.Errorf("the form carries a named control — a browser would post a body the CSRF guard refuses:\n%s", form)
	}
}

// A valid link: the GET shows the interstitial and redeems nothing; the
// button's POST logs the browser in — session cookie set, redirected to the
// console root, and the session actually works against an authenticated
// endpoint. The ticket is then spent.
func TestLoginTicketAuthenticatesTheBrowser(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	page := openLink(t, ts.URL, "?t="+ticket)
	assertInterstitial(t, page, ticket)
	page.Body.Close()

	resp := redeemTicket(t, ts.URL, "?t="+ticket)
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
	replay := redeemTicket(t, ts.URL, "?t="+ticket)
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

// Opening the link is not redeeming it: the GET may be repeated freely, by a
// previewer or a human, and the ticket still works afterwards. This is the
// property the whole split exists for — a link preview is a navigation-shaped
// GET no header guard can tell from the click.
func TestOpeningTheLinkDoesNotSpendIt(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	// Three previews, each shaped exactly like a browser navigation — the
	// share sheet's LinkPresentation load, Messages on the sender, Messages
	// on the receiver.
	for i := 0; i < 3; i++ {
		page := openLink(t, ts.URL, "?t="+ticket, func(req *http.Request) {
			req.Header.Set("Sec-Fetch-Mode", "navigate")
			req.Header.Set("Sec-Fetch-Dest", "document")
			req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 26_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1")
		})
		assertInterstitial(t, page, ticket)
		page.Body.Close()
	}
	resp := redeemTicket(t, ts.URL, "?t="+ticket)
	defer resp.Body.Close()
	if sessionCookie(resp) == nil {
		t.Fatal("three navigation-shaped previews spent the ticket — the human's click got no session")
	}
}

// The interstitial is served for a ticket that never existed exactly as for a
// live one: the GET consults no store, so it is not an oracle for "is this
// link live" and puts no file read under the console's mutex.
func TestTheInterstitialIsNotAnOracle(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	page := openLink(t, ts.URL, "?t=never-existed")
	assertInterstitial(t, page, "never-existed")
	page.Body.Close()
}

// An unusable ticket must set no cookie and DISTINGUISH NOTHING.
//
// The redirect carries `?link=stale`, which does explain something — and the
// distinction matters, so it is stated rather than quietly reinterpreted.
// What would be a leak is revealing WHICH of unknown, expired or already-used
// applies, because that tells a caller whether a given ticket ever existed.
// `?link=stale` is the union of the three and is returned for a ticket that
// never existed, so it reveals nothing about any of them.
//
// So the assertion is the property: every unusable shape must produce the
// IDENTICAL response. That is strictly stronger than pinning a literal — it
// fails a handler that returned "/login" for one shape and
// "/login?why=expired" for another.
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
		resp := redeemTicket(t, ts.URL, q)
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

	// A link with no ticket at all has nothing to continue with — the GET
	// sends it where a spent link goes, rather than rendering a button that
	// can only fail.
	for _, q := range []string{"", "?t="} {
		resp := openLink(t, ts.URL, q)
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/login?link=stale" {
			t.Errorf("GET %q = %d -> %q, want a 302 to /login?link=stale",
				q, resp.StatusCode, resp.Header.Get("Location"))
		}
		resp.Body.Close()
	}
}

// The ticket rides in a URL, so neither half may let it leak onward or be
// cached by anything in between.
func TestLoginTicketResponseDoesNotLeakTheCredential(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	check := func(what string, resp *http.Response) {
		t.Helper()
		// `strict-origin`: the Referer carries the origin alone, never this
		// page's address (which holds the ticket). NOT `no-referrer` — that
		// keeps the address out of the Referer too, and also makes the
		// browser send the button's POST with `Origin: null`, which the CSRF
		// guard refuses; TestLoginTicketRedeemsUnderTheOriginABrowserSends
		// is the pin on that half.
		if got := resp.Header.Get("Referrer-Policy"); got != loginTicketReferrerPolicy {
			t.Errorf("%s: Referrer-Policy = %q, want %q", what, got, loginTicketReferrerPolicy)
		}
		if got := resp.Header.Get("Referrer-Policy"); strings.EqualFold(got, "no-referrer") {
			t.Errorf("%s: Referrer-Policy is no-referrer — a form POST from this page arrives as Origin: null", what)
		}
		if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s: Cache-Control = %q, want no-store", what, got)
		}
		if loc := resp.Header.Get("Location"); strings.Contains(loc, ticket) {
			t.Errorf("%s: the redirect target carries the ticket onward", what)
		}
	}
	page := openLink(t, ts.URL, "?t="+ticket)
	check("the interstitial", page)
	if got := page.Header.Get("Content-Security-Policy"); !strings.Contains(got, "frame-ancestors 'none'") {
		t.Errorf("the interstitial may be framed: CSP = %q", got)
	}
	page.Body.Close()

	resp := redeemTicket(t, ts.URL, "?t="+ticket)
	check("the redeem", resp)
	resp.Body.Close()
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
		resp := redeemTicket(t, ts.URL, "?t="+ticket+"&next="+next)
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("next=%q redirected to %q, want / — no caller-supplied target may be honoured", next, got)
		}
		resp.Body.Close()
	}
}

// browserFormPostOrigin is the `Origin` a browser puts on a plain form
// submission from a page whose document referrer policy is `policy`, per the
// Fetch standard's "append a request `Origin` header" (§4.1): for a non-GET
// request whose mode is not `cors` — a navigation is one — the serialized
// origin becomes `null` under `no-referrer`, `null` under the strict/downgrade
// family only when the request is an https→http downgrade, `null` under
// `same-origin` only when the target is cross-origin, and the real origin
// otherwise. The interstitial's Continue is same-origin and not a downgrade,
// so only `no-referrer` can null it — which is exactly what the field saw.
//
// A test that submits the button WITHOUT an Origin (the older shape here)
// cannot see this: csrfGuard checks the header only when it is present, and a
// browser always sends one on a POST.
func browserFormPostOrigin(policy, pageOrigin string, downgrade, crossOrigin bool) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "no-referrer":
		return "null"
	case "no-referrer-when-downgrade", "strict-origin", "strict-origin-when-cross-origin":
		if downgrade {
			return "null"
		}
	case "same-origin":
		if crossOrigin {
			return "null"
		}
	}
	return pageOrigin
}

// interstitialReferrerPolicy reads the policy the interstitial actually
// delivers — the `<meta name="referrer">` if the page carries one (parsed
// after the header, it is what the document keeps), else the header — and
// insists the two agree, because a header the meta silently overrides is how
// this regresses without either pin noticing.
func interstitialReferrerPolicy(t *testing.T, resp *http.Response, body string) string {
	t.Helper()
	header := resp.Header.Get("Referrer-Policy")
	const open = `<meta name="referrer" content="`
	i := strings.Index(body, open)
	if i < 0 {
		return header
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated referrer meta in the interstitial")
	}
	meta := rest[:j]
	if !strings.EqualFold(meta, header) {
		t.Errorf("the interstitial's <meta name=\"referrer\"> says %q but its header says %q — the meta wins, and they must agree", meta, header)
	}
	return meta
}

// The interstitial's one button must redeem under the Origin a REAL browser
// puts on it — which the page's own referrer policy decides. Served with
// `no-referrer` (as it was for one build) the button's POST arrives with
// `Origin: null`, csrfGuard refuses it as cross-origin, and the human who
// clicked Continue gets a 36-byte text file instead of the console (2026-09-12,
// Safari on iOS and on macOS alike). So this test does not choose an Origin:
// it reads the policy the GET serves, derives the Origin the Fetch standard
// says a same-origin, non-downgrade form POST carries under it, and submits
// exactly that. A regression to `no-referrer` turns it red by construction.
func TestLoginTicketRedeemsUnderTheOriginABrowserSends(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The public origin the fixture's allowlist admits (bridge.example.com,
	// proxy-terminated so the port is opaque) — the address bar of the
	// browser that opened the link.
	const pageOrigin = "https://bridge.example.com:7789"

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	page := openLink(t, ts.URL, "?t="+ticket)
	b, err := io.ReadAll(page.Body)
	page.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	policy := interstitialReferrerPolicy(t, page, string(b))
	origin := browserFormPostOrigin(policy, pageOrigin, false, false)

	resp := redeemTicket(t, ts.URL, "?t="+ticket, func(req *http.Request) {
		req.Header.Set("Origin", origin)
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Dest", "document")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Continue under the browser's Origin %q (policy %q) = %d -> %q %q, want a 302 to / with a session",
			origin, policy, resp.StatusCode, resp.Header.Get("Location"), strings.TrimSpace(string(body)))
	}
	if sessionCookie(resp) == nil {
		t.Fatal("Continue under the browser's Origin set no session cookie")
	}

	// And the shape the field saw, stated directly so the refusal it hits is
	// legible: an opaque origin is still refused — that is the CSRF guard
	// doing its job for a sandboxed or cross-site page — which is WHY the
	// page must not be served under a policy that produces one.
	nulled := redeemTicket(t, ts.URL, "?t="+ticket, func(req *http.Request) {
		req.Header.Set("Origin", "null")
	})
	defer nulled.Body.Close()
	if nulled.StatusCode != http.StatusForbidden {
		t.Errorf("a POST with Origin: null = %d, want 403 — the allowlist must keep refusing an opaque origin", nulled.StatusCode)
	}
}
