package packaging

import (
	"net"
	"testing"
	"time"
)

// TestIsListeningRoundTrip checks both dial outcomes: an owned listener
// accepts, and port zero refuses (no TCP listener can have that port).
func TestIsListeningRoundTrip(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
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
	// Never probe the listener's address after releasing it: another test
	// can own that port by then. Port zero cannot be allocated to one.
	if IsListening("127.0.0.1", 0) {
		t.Error("expected IsListening=false on port zero")
	}
}

// TestWaitForListenTimesOut drives WaitForListen against port zero, which
// no listener can own, and confirms it returns false within the budget.
func TestWaitForListenTimesOut(t *testing.T) {
	start := time.Now()
	if WaitForListen("127.0.0.1", 0, 300*time.Millisecond) {
		t.Error("WaitForListen should have timed out on port zero")
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
