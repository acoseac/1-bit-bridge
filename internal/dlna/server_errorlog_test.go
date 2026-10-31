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

// TestAMediaServerPanicIsLoggedWithoutItsClient: net/http recovers a panic
// in a handler and logs it with the address of the client it was serving
// ("http: panic serving <addr>: …"), and the MediaServer's server had no
// ErrorLog, so the line went to the standard logger with the renderer's
// address in it. The loopback route's server took handshakelog's in B604's
// first round and this one did not (Codex on the integrated branch). It
// takes it too, so the line keeps the panic and stands
// handshakelog.ClientPlaceholder where the address was. The real server,
// over a real connection, its artwork route made to panic by the source it
// asks.
func TestAMediaServerPanicIsLoggedWithoutItsClient(t *testing.T) {
	log := handshaketest.CaptureStdLog(t)
	s := startLoopbackDLNA(t, ServerConfig{Artwork: panickingArtwork{}}, nil)
	requireAPanicLineWithoutItsClient(t, log, s.cfg.ServerURL+ArtworkPathPrefix+"0123456789abcdef")
}

// testPanicValue is what a handler these tests make panic panics with.
const testPanicValue = "the test's own panic"

// panickingArtwork is an artwork source whose every answer panics, so the
// MediaServer's artwork handler does.
type panickingArtwork struct{}

func (panickingArtwork) ServeArtwork(http.ResponseWriter, *http.Request, string) {
	panic(testPanicValue)
}

// requireAPanicLineWithoutItsClient GETs url, whose handler panics, over a
// connection whose local address it records, waits for net/http's line for
// the panic in log, and fails unless that line keeps the panic, stands
// handshakelog.ClientPlaceholder where the client was, and names the client
// nowhere.
func requireAPanicLineWithoutItsClient(t *testing.T, log *handshaketest.Buffer, url string) {
	t.Helper()
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
	if resp, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get(url); err == nil {
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
	if !strings.Contains(got, testPanicValue) || !strings.Contains(got, handshakelog.ClientPlaceholder) {
		t.Errorf("the panic line does not keep the panic with %q where the client was:\n%s", handshakelog.ClientPlaceholder, got)
	}
	if strings.Contains(got, addr) {
		t.Errorf("the panic line names the client %s:\n%s", addr, got)
	}
}
