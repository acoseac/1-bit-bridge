package admin

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAConsoleLoginRefusalLogsItsSourceAndNothingElseOfTheRequest pins the
// console's one line that names a client, and what it names. A failed login,
// and one the limiter refused, are logged at Warn with the source address
// ExtractClientIP gives, which is the limiter's own key (the proxy's
// X-Real-IP, or the right-most valid X-Forwarded-For, where
// deployment.adminTLSTerminatedByProxy says a proxy terminates TLS; the
// connection's host otherwise), canonical and without a port, beside the
// username: a security log, the source an operator or fail2ban blocks.
// B604's second round kept it, as a documented exception to the rule that a
// log line names no client (Codex on the integrated branch asked for that or
// a redaction). Nothing else of the request reaches any line: not the
// password, not the connection's own address and port, not the header's own
// spelling. The last attempt waits out adminauth.RateLimitDelay.
func TestAConsoleLoginRefusalLogsItsSourceAndNothingElseOfTheRequest(t *testing.T) {
	logs := loggingtest.Record(t)
	srv, _, _ := newPublicTestServer(t, "test-password-123")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var mu sync.Mutex
	var conns []string
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			mu.Lock()
			conns = append(conns, c.LocalAddr().String())
			mu.Unlock()
		}
		return c, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	const password = "B604-not-the-password"
	login := func() {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/login",
			strings.NewReader(`{"username":"admin","password":"`+password+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://bridge.example.com")
		// The proxy's view of the client, in a spelling the line does not keep.
		req.Header.Set("X-Real-IP", "::ffff:203.0.113.7")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("the login answered %d, want 401", resp.StatusCode)
		}
	}
	for i := 0; i < adminauth.RateLimitMaxAttempts; i++ {
		login()
	}
	login() // the limiter's refusal

	counts := map[string]int{}
	for _, rec := range logs.Records() {
		if rec.Message != "admin login failed" && rec.Message != "admin login rate-limited" {
			continue
		}
		counts[rec.Message]++
		attrs := map[string]string{}
		rec.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.String()
			return true
		})
		if rec.Level != slog.LevelWarn || attrs["ip"] != "203.0.113.7" || attrs["username"] != "admin" {
			t.Errorf("%s %q: ip=%q username=%q, want WARN ip=203.0.113.7 username=admin", rec.Level, rec.Message, attrs["ip"], attrs["username"])
		}
	}
	if counts["admin login failed"] != adminauth.RateLimitMaxAttempts || counts["admin login rate-limited"] != 1 {
		t.Errorf("logged %v, want %d failed logins and one the limiter refused", counts, adminauth.RateLimitMaxAttempts)
	}
	mu.Lock()
	seen := append([]string(nil), conns...)
	mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no connection's address was recorded")
	}
	if got := logs.Carrying(append([]string{password, "::ffff:"}, seen...)...); len(got) != 0 {
		t.Errorf("a line carries the password, the header's own spelling or a connection's address:\n%s", strings.Join(got, "\n"))
	}
}
