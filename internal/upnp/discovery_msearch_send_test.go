package upnp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// errRouteGone is what a send pinned to an interface with no IPv4 multicast
// route answers: measured on the dev Mac's utun0, every tick.
var errRouteGone = errors.New("write udp4 0.0.0.0:55325->239.255.255.250:1900: sendto: can't assign requested address")

// newSendTestClient returns a client whose sends are pinned to an interface
// named iface, at the default 60 s cadence, logging into the returned
// buffer. Its sends go to write, never to the wire.
func newSendTestClient(t *testing.T, iface string, write func() error) (*MediaServerDiscoveryClient, *bytes.Buffer) {
	t.Helper()
	c, err := NewMediaServerDiscoveryClient(DiscoveryConfig{
		Interface:  &net.Interface{Name: iface},
		Dispatcher: &recordingDispatcher{},
	}, NewServerCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	c.writeMSearch = func(*net.UDPConn, []byte, *net.UDPAddr) (int, error) {
		if err := write(); err != nil {
			return 0, err
		}
		return 1, nil
	}
	var buf bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&buf, nil)))
	return c, &buf
}

// TestSendMSearchReportsFailedUpstreamSends pins the report this client did
// not have: until 2026-09-28 it discarded every send error, so a dead
// multicast route left upstream discovery silent. The policy is the shared
// SendFailureLog's (the renderer client's tests pin its rules); this pins
// the wiring, down to the cadence: the Error lands ten minutes into the
// streak, the tenth failure at this client's default 60 s, where the
// renderer's cadence would put it at the twentieth.
func TestSendMSearchReportsFailedUpstreamSends(t *testing.T) {
	failing := true
	c, buf := newSendTestClient(t, "test0", func() error {
		if failing {
			return errRouteGone
		}
		return nil
	})
	// No run loop is started, so the test is the only sender, and the
	// socket sendMSearch snapshots is never written: the seam takes the send.
	c.conn = new(net.UDPConn)

	send := func(n int) {
		for range n {
			c.sendMSearch()
		}
	}
	const escalation = "failing persistently; upstream server discovery is degraded"
	send(9)
	if got := strings.Count(buf.String(), "M-SEARCH send failed"); got != 1 {
		t.Errorf("nine failed sends logged %d Warns, want 1:\n%s", got, buf.String())
	}
	if strings.Contains(buf.String(), "failing persistently") {
		t.Fatalf("escalated before ten minutes of the 60 s cadence:\n%s", buf.String())
	}
	send(1)
	if got := strings.Count(buf.String(), escalation); got != 1 {
		t.Fatalf("the tenth failed send logged %d Errors naming upstream discovery, want 1:\n%s", got, buf.String())
	}
	send(20)
	if got := strings.Count(buf.String(), escalation); got != 1 {
		t.Errorf("thirty failed sends logged %d Errors naming upstream discovery, want 1:\n%s", got, buf.String())
	}
	failing = false
	send(1)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[2], "M-SEARCH send recovered") ||
		!strings.Contains(lines[2], "consecutiveFailures=30") {
		t.Fatalf("want a Warn, an Error and the recovery carrying the streak's 30, got:\n%s", buf.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "component=upnp") || !strings.Contains(line, "interface=test0") {
			t.Errorf("line does not name this client's component and interface:\n%s", line)
		}
	}
	if !strings.Contains(lines[0], errRouteGone.Error()) {
		t.Errorf("the Warn does not carry the send's error:\n%s", lines[0])
	}
}

// TestUpstreamSendStreakResetsOnRestart pins that Start resets the streak:
// carried over from a run stopped mid-outage, the new run's first failure
// lands past both the Warn and the Error, and a restarted, still broken
// client logs nothing. The new run's first failure is the live tick loop's
// own first send, failing through the seam on every host, so the host's
// multicast route cannot decide the result.
func TestUpstreamSendStreakResetsOnRestart(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	c, buf := newSendTestClient(t, "test0", func() error {
		once.Do(func() { close(entered) })
		return errRouteGone
	})
	// Past both arms before Start, while no loop is live: the streak has
	// no lock because only the tick loop notes results.
	for range 15 {
		c.sendErrs.Note(errRouteGone)
	}
	buf.Reset()

	if err := c.Start(context.Background()); err != nil {
		t.Skipf("cannot bind a UDP socket in this environment: %v", err)
	}
	t.Cleanup(c.Stop)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the tick loop never reached its first M-SEARCH send")
	}
	c.Stop() // joins the loop, so its failure has been noted by now

	if got := strings.Count(buf.String(), "M-SEARCH send failed"); got != 1 {
		t.Errorf("a restarted client logged %d first-failure Warns, want 1 — with a carried-over "+
			"streak it logs NOTHING:\n%s", got, buf.String())
	}
	if got := c.sendErrs.Streak(); got != 1 {
		t.Errorf("streak = %d after the restarted run's first failure, want 1", got)
	}
}
