package dlna

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/handshakelog"
	"github.com/acoseac/1-bit-bridge/internal/handshakelog/handshaketest"
)

// TestALoopbackRoutePanicIsLoggedWithoutItsClient: net/http recovers a
// panic in a handler and logs it with the address of the client it was
// serving ("http: panic serving <addr>: …"), and the loopback track route's
// server had no ErrorLog, so the line went to the standard logger with
// MPD's address in it. It takes handshakelog's, as serve's servers and the
// OpenHome surface do, so the line keeps the panic and stands
// handshakelog.ClientPlaceholder where the address was. The real server,
// over a real connection, its handler made to panic by the gate it asks
// first.
func TestALoopbackRoutePanicIsLoggedWithoutItsClient(t *testing.T) {
	log := handshaketest.CaptureStdLog(t)
	s := NewLoopbackServer(LoopbackConfig{Enabled: func() bool { panic("the test's own panic") }})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})

	var mu sync.Mutex
	var client string
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			mu.Lock()
			client = c.LocalAddr().String()
			mu.Unlock()
		}
		return c, err
	}}
	t.Cleanup(transport.CloseIdleConnections)
	if resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get(s.BaseURL() + FilePathPrefix + "0123456789abcdef"); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the panicking handler answered %d, want the connection aborted", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(log.String(), "panic serving") {
		if time.Now().After(deadline) {
			t.Fatalf("no panic line in the standard logger:\n%s", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	addr := client
	mu.Unlock()
	if addr == "" {
		t.Fatal("the client's address was not recorded")
	}
	got := log.String()
	if !strings.Contains(got, "the test's own panic") || !strings.Contains(got, handshakelog.ClientPlaceholder) {
		t.Errorf("the panic line does not keep the panic with %q where the client was:\n%s", handshakelog.ClientPlaceholder, got)
	}
	if strings.Contains(got, addr) {
		t.Errorf("the panic line names the client %s:\n%s", addr, got)
	}
}
