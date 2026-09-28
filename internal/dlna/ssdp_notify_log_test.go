package dlna

import (
	"bytes"
	"context"
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

// Test_SSDPAdvertiser_FailedBurstsReachTheDefaultLevelOncePerStreak pins
// backlog B47's fourth item: a burst whose writes fail logged at Debug
// alone, so an advertiser whose interface lost its route announced nothing
// and said nothing at the default level. The bursts now report through
// discovery.SendFailureLog, ONE result per burst: a Warn when a streak
// starts, the one Error at its second failed burst (ten minutes of a
// 14-minute cadence), nothing more however long it lasts, and a line when a
// burst goes out again. Counted per write, the Error would land inside the
// first burst.
//
// The first burst is Start's own, on the loopback interface, so the log
// Start builds is the one under test; every write fails because the
// Location makes each NOTIFY larger than a datagram can be.
func Test_SSDPAdvertiser_FailedBurstsReachTheDefaultLevelOncePerStreak(t *testing.T) {
	const location = "http://127.0.0.1:7790/dlna/description.xml"
	var buf bytes.Buffer
	a := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    location + "?" + strings.Repeat("x", 70_000),
		ServerToken: "test",
		Interface:   loopbackInterface(t),
		Logger:      slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err := a.Start(context.Background()); err != nil {
		t.Skipf("multicast unavailable on the loopback interface: %v", err)
	}
	a.Stop() // joins the periodic goroutine, so the test is the only one noting results now
	if got := strings.Count(buf.String(), "NOTIFY send failed"); got != 1 {
		t.Fatalf("Start's failed burst logged %d NOTIFY Warns at the default level, want 1:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "failing persistently") {
		t.Fatalf("the streak escalated inside its first burst, which counted writes, not bursts:\n%s", buf.String())
	}

	sender := dialLoopbackSender(t)
	a.announceAlive(sender)
	const escalation = "NOTIFY send failing persistently; DLNA advertising is degraded"
	if got := strings.Count(buf.String(), escalation); got != 1 {
		t.Fatalf("the second failed burst logged %d Errors, want 1:\n%s", got, buf.String())
	}
	for range 10 {
		a.announceAlive(sender)
	}
	a.cfg.Location = location
	a.announceAlive(sender)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var reports []string
	for _, line := range lines {
		if strings.Contains(line, "NOTIFY send") {
			reports = append(reports, line)
		}
	}
	if len(reports) != 3 || !strings.Contains(reports[2], "NOTIFY send recovered") ||
		!strings.Contains(reports[2], "consecutiveFailures=12") {
		t.Fatalf("want a Warn, an Error and the recovery carrying the streak's 12, got:\n%s", buf.String())
	}
	for _, line := range reports {
		if !strings.Contains(line, "interface="+a.cfg.Interface.Name) {
			t.Errorf("line does not name the advertiser's interface:\n%s", line)
		}
	}
	if strings.Contains(buf.String(), "NOTIFY alive send failed") {
		t.Errorf("a per-write Debug line reached the default level:\n%s", buf.String())
	}
}
