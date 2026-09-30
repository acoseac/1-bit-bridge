package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// scrapeMetrics sends GET /metrics through the console's real handler chain
// (the boundary, the CSRF guard, the session gate, the route) from remote,
// naming host, with header added, and with session's cookie unless session
// is "".
func scrapeMetrics(t *testing.T, srv *Server, remote, host string, header http.Header, session string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = remote
	req.Host = host
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	}
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	return rw
}

// withMetricsAllowCIDRs stores a copy of the server's live config whose
// metrics.allowCidrs is cidrs.
func withMetricsAllowCIDRs(srv *Server, cidrs ...string) {
	c := *srv.deps.CfgHolder.Load()
	c.Metrics.AllowCIDRs = cidrs
	srv.deps.CfgHolder.Store(&c)
}

// isExposition reports whether a /metrics answer is the Prometheus text the
// route serves, rather than a refusal.
func isExposition(rw *httptest.ResponseRecorder) bool {
	return rw.Code == http.StatusOK && strings.Contains(rw.Body.String(), "# TYPE go_goroutines")
}

// assertMetricsRefused requires a refusal that a scraper can read as one: a
// 403, never the login redirect every other console page answers with, and
// none of the exposition.
func assertMetricsRefused(t *testing.T, rw *httptest.ResponseRecorder, why string) {
	t.Helper()
	if rw.Code != http.StatusForbidden {
		t.Errorf("%s: got %d, want 403", why, rw.Code)
	}
	if loc := rw.Header().Get("Location"); loc != "" {
		t.Errorf("%s: redirected to %q; a scraper follows that to the login form", why, loc)
	}
	if strings.Contains(rw.Body.String(), "go_goroutines") {
		t.Errorf("%s: the refusal carries the exposition", why)
	}
}

// proxiedHeaders are the headers a TLS-terminating reverse proxy on the
// bridge's own host (HAProxy's `option forwardfor`, say) sets on every
// request it relays to the console on 127.0.0.1 (backlog B171).
func proxiedHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Forwarded-For", "203.0.113.9")
	h.Set("X-Forwarded-Proto", "https")
	return h
}

// proxiedHost is the public name such a proxy's clients reach it by.
const proxiedHost = "bridge.example.com:443"

// TestPublicMetricsThroughASameHostProxyNeedsASession is backlog B171: a proxy
// on the bridge's own host relays every request from 127.0.0.1, so a source
// address cannot vouch for a scrape. A relayed request with no session is
// refused whatever the config lists, loopback included.
func TestPublicMetricsThroughASameHostProxyNeedsASession(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")

	rw := scrapeMetrics(t, srv, "127.0.0.1:54321", proxiedHost, proxiedHeaders(), "")
	assertMetricsRefused(t, rw, "relayed by a same-host proxy, no session")

	withMetricsAllowCIDRs(srv, "127.0.0.1/32", "::1/128")
	rw = scrapeMetrics(t, srv, "127.0.0.1:54321", proxiedHost, proxiedHeaders(), "")
	assertMetricsRefused(t, rw, "relayed by a same-host proxy whose address metrics.allowCidrs lists")
}

// TestPublicMetricsWithoutASessionNeedsAnAddressTheConfigNames: in public mode
// a scrape without a session is answered only from an address
// metrics.allowCidrs lists, over a connection that carries no forwarding
// header. Loopback is not implied: the operator lists it, which is the
// operator saying nothing on this host relays connections to the console.
func TestPublicMetricsWithoutASessionNeedsAnAddressTheConfigNames(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")

	// No list: nothing is vouched for, loopback included (the pre-B171
	// "F1" bypass answered this one).
	assertMetricsRefused(t, scrapeMetrics(t, srv, "127.0.0.1:54321", "127.0.0.1:7789", nil, ""),
		"loopback scrape, metrics.allowCidrs empty")

	withMetricsAllowCIDRs(srv, "127.0.0.1/32", "10.42.0.0/16", "not-a-cidr")
	direct := []struct {
		remote string
		ok     bool
		why    string
	}{
		{"127.0.0.1:54321", true, "a listed loopback address"},
		{"10.42.7.9:5000", true, "inside a listed monitoring range"},
		{"[::ffff:10.42.7.9]:5000", true, "the same address, IPv4-mapped"},
		{"[::1]:54321", false, "IPv6 loopback, which the list does not name"},
		{"10.43.7.9:5000", false, "outside every listed range"},
		{"192.168.1.5:5000", false, "a LAN address nobody listed"},
		{"203.0.113.5:5000", false, "the internet"},
		{"garbage", false, "an unparseable remote address"},
	}
	for _, c := range direct {
		rw := scrapeMetrics(t, srv, c.remote, "bridge.example.com:7789", nil, "")
		if c.ok {
			if !isExposition(rw) {
				t.Errorf("%s (%s): got %d, want the exposition", c.why, c.remote, rw.Code)
			}
			continue
		}
		assertMetricsRefused(t, rw, c.why+" ("+c.remote+")")
	}

	// A listed address that says a proxy forwarded the request is not the
	// scraper the entry names: any of the headers a proxy adds refuses it.
	for _, name := range []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP", "Via"} {
		h := http.Header{}
		h.Set(name, "x")
		for _, remote := range []string{"127.0.0.1:54321", "10.42.7.9:5000"} {
			assertMetricsRefused(t, scrapeMetrics(t, srv, remote, "bridge.example.com:7789", h, ""),
				"a listed address carrying "+name+" ("+remote+")")
		}
	}
}

// TestPublicMetricsAnswersASignedInSession: /metrics is a console route like
// any other in public mode, so a signed-in session reads it from wherever the
// console answers it, a same-host proxy's relay included.
func TestPublicMetricsAnswersASignedInSession(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "test-password-123")
	session := signIn(t, store)

	for _, c := range []struct {
		remote, host string
		header       http.Header
		why          string
	}{
		{"127.0.0.1:54321", proxiedHost, proxiedHeaders(), "through a same-host proxy"},
		{"192.168.1.5:5000", "bridge.example.com:7789", nil, "from the LAN"},
		{"203.0.113.5:5000", "bridge.example.com", nil, "from the internet"},
	} {
		if rw := scrapeMetrics(t, srv, c.remote, c.host, c.header, session); !isExposition(rw) {
			t.Errorf("signed in, %s: got %d, want the exposition", c.why, rw.Code)
		}
	}
	assertMetricsRefused(t, scrapeMetrics(t, srv, "127.0.0.1:54321", proxiedHost, proxiedHeaders(), "not-a-session"),
		"a cookie that names no session")
}

// TestLoopbackMetricsFollowsTheConsolesLoopbackRule: in loopback mode
// /metrics is behind the console's own boundary, which admits this host and
// nothing else, so metrics.allowCidrs plays no part there.
func TestLoopbackMetricsFollowsTheConsolesLoopbackRule(t *testing.T) {
	srv, _, _ := newTestServer(t)
	withMetricsAllowCIDRs(srv, "10.42.0.0/16")
	for _, remote := range []string{"127.0.0.1:54321", "[::1]:54321"} {
		if rw := scrapeMetrics(t, srv, remote, "127.0.0.1:7789", nil, ""); !isExposition(rw) {
			t.Errorf("loopback mode, %s: got %d, want the exposition", remote, rw.Code)
		}
	}
	for _, remote := range []string{"10.42.7.9:5000", "192.168.1.5:5000", "203.0.113.5:5000"} {
		if rw := scrapeMetrics(t, srv, remote, "127.0.0.1:7789", nil, ""); rw.Code != http.StatusForbidden {
			t.Errorf("loopback mode, %s: got %d, want 403", remote, rw.Code)
		}
	}
}

// TestAPublicMetricsRefusalFromThisHostOrTheLANIsLoggedOnce: the refusal a
// local scraper meets after an upgrade (it was answered from loopback before
// B171) says, once, what the config needs. A refusal from the internet (a
// scanner) and one a proxy relayed log nothing.
func TestAPublicMetricsRefusalFromThisHostOrTheLANIsLoggedOnce(t *testing.T) {
	const msg = "a /metrics scrape without a session was refused"
	rec := loggingtest.Record(t)
	srv, _, _ := newPublicTestServer(t, "test-password-123")

	scrapeMetrics(t, srv, "203.0.113.5:5000", "bridge.example.com", nil, "")
	scrapeMetrics(t, srv, "127.0.0.1:54321", proxiedHost, proxiedHeaders(), "")
	if lines := rec.Lines(msg); len(lines) != 0 {
		t.Fatalf("a refusal from the internet or through a proxy logged:\n%s", strings.Join(lines, "\n"))
	}
	for i := 0; i < 3; i++ {
		scrapeMetrics(t, srv, "127.0.0.1:54321", "127.0.0.1:7789", nil, "")
		scrapeMetrics(t, srv, "192.168.1.5:5000", "bridge.example.com:7789", nil, "")
	}
	lines := rec.Failures(msg)
	if len(lines) != 1 {
		t.Fatalf("got %d lines for repeated local refusals, want 1:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "metrics.allowCidrs") {
		t.Errorf("the line does not say what the config needs: %s", lines[0])
	}
}

// TestTheDiagnosticsMetricsPointerSaysWhoMayScrape: the Diagnostics page's
// paragraph offering /metrics describes the gate this bridge's mode applies.
func TestTheDiagnosticsMetricsPointerSaysWhoMayScrape(t *testing.T) {
	page := func(t *testing.T, srv *Server, session string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/diagnostics", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Host = "127.0.0.1:7789"
		if session != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
		}
		rw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rw, req)
		b, _ := io.ReadAll(rw.Body)
		if rw.Code != http.StatusOK || !strings.Contains(string(b), `href="/metrics"`) {
			t.Fatalf("/diagnostics: %d, pointer present %v", rw.Code, strings.Contains(string(b), `href="/metrics"`))
		}
		// The template wraps its prose, so compare with runs of white
		// space folded to one.
		return strings.Join(strings.Fields(string(b)), " ")
	}

	loop, _, _ := newTestServer(t)
	if got := page(t, loop, ""); !strings.Contains(got, "loopback listener only") {
		t.Error("loopback-mode pointer no longer says the listener is loopback-only")
	}

	pub, store, _ := newPublicTestServer(t, "test-password-123")
	got := page(t, pub, signIn(t, store))
	if strings.Contains(got, "loopback listener only") {
		t.Error("public-mode pointer says /metrics is loopback-only")
	}
	if !strings.Contains(got, "metrics.allowCidrs") {
		t.Error("public-mode pointer does not name metrics.allowCidrs")
	}
	// The address alone does not vouch: metricsScrapeVouched also refuses a
	// request that carries a forwarding header, so the pointer says so.
	if !strings.Contains(got, "no forwarding header") {
		t.Error("public-mode pointer does not say the scrape must carry no forwarding header")
	}
}
