package admin

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// foreignHosts are Host values a loopback console must refuse (backlog
// B170): names a page's author controls, whatever they resolve to, and
// literals that are not this machine's loopback.
var foreignHosts = []string{
	"evil.example:7789",
	"evil.example",
	"EVIL.EXAMPLE:7789",
	"127.0.0.1.nip.io:7789",       // a public name that resolves to 127.0.0.1
	"localhost.evil.example:7789", // localhost as a label, not the name
	"0.0.0.0:7789",
	"[::]:7789",
	"192.168.1.5:7789",
	"[fe80::1]:7789",
	"127.1:7789", // inet_aton's spelling: no browser sends it, and it is not a literal ParseIP takes
	"localhost@evil.example:7789",
}

// TestTheLoopbackConsoleRefusesARequestThatNamesAnotherHost is backlog
// B170: a request from 127.0.0.1 that names another host in its Host is what
// a page that has pointed its own name at 127.0.0.1 sends, and before the
// fix every GET of it was answered: the settings (the enrich base URLs with
// any password), any library file, the metrics. Each must be 421 and carry
// nothing the route would have served. A POST with the page's own Origin
// was refused by csrfGuard already (403); the Host check runs first now.
func TestTheLoopbackConsoleRefusesARequestThatNamesAnotherHost(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	seedPlayerTrack(t, cfg, srv.deps.Manifest, "Artist/Album/01.flac", "Probe")
	h := srv.Handler()

	routes := []struct {
		method, target, origin string
	}{
		{http.MethodGet, "/api/settings", ""},
		{http.MethodGet, "/api/player/download?path=Artist/Album/01.flac", ""},
		{http.MethodGet, "/api/player/audio?path=Artist/Album/01.flac", ""},
		{http.MethodGet, "/api/stats", ""},
		{http.MethodGet, "/metrics", ""},
		{http.MethodGet, "/", ""},
		{http.MethodGet, "/healthz", ""},
		{http.MethodPost, "/api/scan", "http://evil.example:7789"},
	}
	for _, host := range foreignHosts {
		for _, rt := range routes {
			req := httptest.NewRequest(rt.method, rt.target, nil)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Host = host
			if rt.origin != "" {
				req.Header.Set("Origin", rt.origin)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusMisdirectedRequest {
				t.Errorf("Host %q, %s %s: status %d, want 421", host, rt.method, rt.target, w.Code)
			}
			body := w.Body.String()
			for _, served := range []string{"source", cfg.LibraryName} {
				if strings.Contains(body, served) {
					t.Errorf("Host %q, %s %s: the refusal carries %q, which only the route serves",
						host, rt.method, rt.target, served)
				}
			}
		}
	}
}

// TestTheLoopbackConsoleRefusesAStreamThatNamesAnotherHost is the held-open
// case: a same-origin EventSource sends no Origin either, so the SSE
// stream's own Origin gate lets it through; the Host check must refuse it
// before the stream starts. The context bounds the old code, which served
// the stream until the client left.
func TestTheLoopbackConsoleRefusesAStreamThatNamesAnotherHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = "evil.example:7789"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusMisdirectedRequest {
		t.Errorf("/api/events with a foreign Host: status %d, want 421", w.Code)
	}
	if strings.Contains(w.Body.String(), "data:") {
		t.Errorf("/api/events with a foreign Host streamed frames: %.200s", w.Body.String())
	}
}

// TestTheLoopbackConsoleAnswersEveryLoopbackHost is the other half: every
// Host that names this machine's loopback is answered, with any port or
// none. `localhost:17789` is an `ssh -L 17789:127.0.0.1:7789` tunnel, whose
// browser names the local end's port; an empty Host is an HTTP/1.0 client,
// which no browser is.
func TestTheLoopbackConsoleAnswersEveryLoopbackHost(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	seedPlayerTrack(t, cfg, srv.deps.Manifest, "Artist/Album/01.flac", "Probe")
	h := srv.Handler()
	for _, host := range []string{
		"127.0.0.1:7789",
		"127.0.0.1",
		"localhost:7789",
		"localhost",
		"LocalHost:7789",
		"localhost.:7789",
		"[::1]:7789",
		"[::1]",
		"127.0.0.2:7789",
		"127.255.255.254:9",
		"localhost:17789",
		"[::ffff:127.0.0.1]:7789",
		"",
	} {
		for _, target := range []string{"/api/stats", "/api/player/download?path=Artist/Album/01.flac"} {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.RemoteAddr = "127.0.0.1:54321"
			req.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("Host %q, GET %s: status %d, want 200 (%.120s)", host, target, w.Code, w.Body.String())
			}
		}
	}
}

// TestEveryAdminAddressLoopbackModeTakesIsALoopbackHost pins the claim
// loopbackHostOnly's docblock rests on: the configured admin host needs no
// case of its own, because every adminAddress config's loopback rule
// accepts names a host the Host check accepts. A config that learns to take
// another loopback name must teach the Host check the same name.
func TestEveryAdminAddressLoopbackModeTakesIsALoopbackHost(t *testing.T) {
	accepted := 0
	for _, addr := range []string{
		"127.0.0.1:7789", "[::1]:7789", "localhost:7789", "127.9.9.9:1",
		"[::ffff:127.0.0.1]:7789", "127.0.0.1:0",
		// Refused by the config, so they bind no loopback console; listed
		// so the sweep covers what the config refuses too.
		"0.0.0.0:7789", ":7789", "evil.example:7789", "LOCALHOST:7789", "[::]:7789",
	} {
		if config.ValidateLoopbackAddress("adminAddress", addr) != nil {
			continue
		}
		accepted++
		if !hostIsLoopback(addr) {
			t.Errorf("adminAddress %q passes the config's loopback rule and fails the Host check: "+
				"a browser at the configured address would be refused", addr)
		}
	}
	if accepted < 5 {
		t.Fatalf("only %d addresses passed the config's loopback rule; the sweep proves nothing", accepted)
	}
}

// TestPublicModeLeavesTheHostToItsOwnRules pins that the Host check is the
// loopback posture's alone. A public console is reached under its domain,
// and a tenant console behind the host's proxy arrives FROM 127.0.0.1 under
// the tenant's name: both must be answered as before.
func TestPublicModeLeavesTheHostToItsOwnRules(t *testing.T) {
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	h := srv.Handler()
	for _, c := range []struct{ remote, host string }{
		{"203.0.113.9:4000", "bridge.example.com"},
		{"127.0.0.1:4000", "bridge.example.com:7789"},
		{"127.0.0.1:4000", "t1.cloud.example:7789"},
	} {
		for _, target := range []string{"/login", "/healthz"} {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req.RemoteAddr = c.remote
			req.Host = c.host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Errorf("public mode, from %s as %q, GET %s: status %d, want 200", c.remote, c.host, target, w.Code)
			}
		}
	}
}

// TestAForeignHostIsLoggedOncePerName pins noteForeignHost: one Warn per
// refused name, however many requests carry it, and never more than
// foreignHostSeenCap names, so a page that rebinds a fresh name per
// request cannot fill the journal.
func TestAForeignHostIsLoggedOncePerName(t *testing.T) {
	rec := loggingtest.Record(t)
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	send := func(host string) {
		req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		req.Host = host
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	const msg = "console refused a request that names another host"
	for i := 0; i < 3; i++ {
		send("evil.example:7789")
		send("evil.example:8000")
	}
	send("other.example")
	lines := rec.Failures(msg)
	if len(lines) != 2 {
		t.Fatalf("got %d refusal lines for two names, want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for _, want := range []string{"host=evil.example ", "host=other.example "} {
		found := false
		for _, l := range lines {
			if strings.Contains(l+" ", want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no refusal line names %q:\n%s", strings.TrimSpace(want), strings.Join(lines, "\n"))
		}
	}
	for i := 0; i < 3*foreignHostSeenCap; i++ {
		send(fmt.Sprintf("rebind-%d.example", i))
	}
	if n := len(rec.Failures(msg)); n != foreignHostSeenCap {
		t.Errorf("got %d refusal lines after %d names, want the cap %d", n, 3*foreignHostSeenCap+2, foreignHostSeenCap)
	}
}

// TestTheHostCheckHoldsOverARealListener drives the check through net/http's
// own request parsing, which is where a Host reaches r.Host in production:
// a Host header naming another host is refused, the client's own loopback
// Host is answered, and an HTTP/1.0 request with no Host at all is answered
// too (no browser sends one).
func TestTheHostCheckHoldsOverARealListener(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	_, port, err := net.SplitHostPort(ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	get := func(host string) int {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/stats", nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("evil.example:" + port); code != http.StatusMisdirectedRequest {
		t.Errorf("Host evil.example:%s over a real listener: status %d, want 421", port, code)
	}
	if code := get(""); code != http.StatusOK {
		t.Errorf("the client's own Host over a real listener: status %d, want 200", code)
	}
	if code := get("localhost:" + port); code != http.StatusOK {
		t.Errorf("Host localhost:%s over a real listener: status %d, want 200", port, code)
	}

	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, "GET /api/stats HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an HTTP/1.0 request with no Host: status %d, want 200", resp.StatusCode)
	}
}
