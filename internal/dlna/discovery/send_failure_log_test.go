package discovery

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSendFailureLogEscalatesTenMinutesIntoAStreak pins where the one Error
// lands at each cadence: after ten minutes' worth of the client's own sends,
// rounded up, and never on the streak's first failure. It was a count of 20
// until 2026-09-28, which is ten minutes only at the renderer client's
// default 30 s; the upstream MediaServer client sends every 60 s, and both
// cadences are configurable.
func TestSendFailureLogEscalatesTenMinutesIntoAStreak(t *testing.T) {
	cases := []struct {
		interval time.Duration
		want     int
	}{
		{30 * time.Second, 20}, // the renderer client's default
		{60 * time.Second, 10}, // the upstream client's default
		{45 * time.Second, 14}, // 13.3 rounds up
		{time.Second, 600},
		{7 * time.Minute, 2},
		{10 * time.Minute, 2}, // one tick is ten minutes, but the Warn and the Error stay two lines
		{time.Hour, 2},
		{0, 2},
		{-time.Second, 2},
	}
	for _, tc := range cases {
		if got := sendErrEscalateAt(tc.interval); got != tc.want {
			t.Errorf("sendErrEscalateAt(%v) = %d, want %d", tc.interval, got, tc.want)
		}
	}
}

// TestSendFailureLogNamesTheInterfaceOnEveryLine pins the attribute that
// makes a line actionable where it matters: each LAN-eligible interface has
// a client of its own, so a host with several logs a failure without saying
// which route is gone unless the line names it.
func TestSendFailureLogNamesTheInterfaceOnEveryLine(t *testing.T) {
	buf := captureLogs(t)
	l := NewSendFailureLog(packageLogger, "M-SEARCH", "utun3", "renderer discovery", time.Minute)
	for i := 0; i < l.escalateAt; i++ {
		l.Note(errors.New("sendto: can't assign requested address"))
	}
	l.Note(nil)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines for one streak and its recovery, want 3 (Warn, Error, recovery):\n%s",
			len(lines), buf.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "interface=utun3") {
			t.Errorf("line does not name the interface:\n%s", line)
		}
	}
	if !strings.Contains(lines[1], "renderer discovery is degraded") {
		t.Errorf("the Error does not say what is degraded:\n%s", lines[1])
	}
}

// TestSendFailureLogDropsOnlyTheErrorOfItsOwnStop pins the drop at the one
// place both clients take it: net.ErrClosed, in the *net.OpError a real
// write returns, is the client's own Stop and costs neither a line nor a
// count, while any other failure starts a streak as before.
func TestSendFailureLogDropsOnlyTheErrorOfItsOwnStop(t *testing.T) {
	buf := captureLogs(t)
	l := NewSendFailureLog(packageLogger, "M-SEARCH", "en0", "renderer discovery", time.Minute)
	closed := &net.OpError{Op: "write", Net: "udp4", Err: net.ErrClosed}
	l.Note(closed)
	if buf.Len() != 0 || l.Streak() != 0 {
		t.Fatalf("a send Stop's close cut short logged %q and left a streak of %d, want nothing",
			buf.String(), l.Streak())
	}
	l.Note(fmt.Errorf("write udp4: %w", errors.New("sendto: network is unreachable")))
	if got := strings.Count(buf.String(), "M-SEARCH send failed"); got != 1 || l.Streak() != 1 {
		t.Errorf("a genuine failure logged %d Warns and left a streak of %d, want 1 and 1:\n%s",
			got, l.Streak(), buf.String())
	}
}
