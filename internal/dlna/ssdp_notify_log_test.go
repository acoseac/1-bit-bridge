package dlna

import (
	"bytes"
	"log/slog"
	"net"
	"strings"
	"testing"
)

// notifyAliveFailures runs one NOTIFY ssdp:alive burst for an advertiser at
// location through sender, and returns how many "NOTIFY alive send failed"
// lines it wrote at Debug, the level it logs them at.
func notifyAliveFailures(t *testing.T, location string, sender *net.UDPConn) int {
	t.Helper()
	var buf bytes.Buffer
	a := NewSSDPAdvertiser(SSDPConfig{UDN: "uuid:test", Location: location, ServerToken: "test"})
	a.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	a.sendAliveAll(sender)
	return strings.Count(buf.String(), "NOTIFY alive send failed")
}

// dialLoopbackSender is a sender like Start's, dialled at loopback's discard
// port rather than the SSDP group, so the test needs no multicast.
func dialLoopbackSender(t *testing.T) *net.UDPConn {
	t.Helper()
	sender, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
	if err != nil {
		t.Skipf("cannot dial a loopback UDP socket in this environment: %v", err)
	}
	t.Cleanup(func() { _ = sender.Close() })
	return sender
}

// Test_SSDPAdvertiser_NotifyAliveReportsOnlyFailuresItsStopDidNotCause pins
// the periodic burst's side of the M-SEARCH rule: Stop closes the sender,
// and a burst the periodic goroutine had begun then meets net.ErrClosed on
// every target it has left. That is the stop, not a failure, so it logs
// nothing and the burst ends. A write that fails for its own reason (here a
// datagram past the maximum size, refused on every platform) is still
// reported, one line per target, as before.
func Test_SSDPAdvertiser_NotifyAliveReportsOnlyFailuresItsStopDidNotCause(t *testing.T) {
	const location = "http://127.0.0.1:7790/dlna/description.xml"
	closed := dialLoopbackSender(t)
	_ = closed.Close() // Stop's close, landed before the burst's writes
	if got := notifyAliveFailures(t, location, closed); got != 0 {
		t.Errorf("a burst Stop's close cut short logged %d NOTIFY send failures, want 0", got)
	}

	oversized := location + "?" + strings.Repeat("x", 70_000)
	if got, want := notifyAliveFailures(t, oversized, dialLoopbackSender(t)), len(NotifyTargetsFor("uuid:test")); got != want {
		t.Errorf("a burst whose every write fails logged %d NOTIFY send failures, want one per target, %d", got, want)
	}
}
