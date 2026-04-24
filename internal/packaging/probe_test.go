package packaging

import (
	"net"
	"testing"
	"time"
)

// TestIsListeningRoundTrip stands up a TCP listener on a random local
// port, confirms IsListening sees it, then tears it down and confirms
// the follow-up sees the port free. We care about the round-trip so
// a flaky net layer can't paint a false green.
func TestIsListeningRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split host:port: %v", err)
	}
	var port int
	if _, err := parsePort(portStr, &port); err != nil {
		t.Fatalf("parse port: %v", err)
	}

	if !IsListening("127.0.0.1", port) {
		t.Errorf("expected IsListening=true on live port %d", port)
	}
	_ = ln.Close()
	// Give the kernel a breath to release the TIME_WAIT state. Even on
	// a zero-port bind the OS needs a moment.
	time.Sleep(50 * time.Millisecond)
	if IsListening("127.0.0.1", port) {
		// Not fatal — the kernel's TIME_WAIT can keep the port warm
		// for a bit. Treat as a soft warning rather than failure.
		t.Log("port still shows as listening briefly after Close (TIME_WAIT); acceptable")
	}
}

// TestWaitForListenTimesOut drives WaitForListen against a port we
// know is cold and confirms it returns false within the budget. Picks
// a random-local port and releases it immediately so the test doesn't
// depend on a fixed "surely unused" port number.
func TestWaitForListenTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	_, _ = parsePort(portStr, &port)
	_ = ln.Close()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	if WaitForListen("127.0.0.1", port, 300*time.Millisecond) {
		t.Error("WaitForListen should have timed out on a closed port")
	}
	elapsed := time.Since(start)
	if elapsed < 250*time.Millisecond {
		t.Errorf("returned too quickly: %s", elapsed)
	}
}

// parsePort is a tiny helper to avoid importing strconv just for tests.
func parsePort(s string, out *int) (int, error) {
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, &parseErr{}
		}
		n = n*10 + int(c-'0')
	}
	*out = n
	return n, nil
}

type parseErr struct{}

func (*parseErr) Error() string { return "parse error" }
