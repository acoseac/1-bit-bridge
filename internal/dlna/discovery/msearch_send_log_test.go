package discovery

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs redirects the default slog handler into a buffer for the test.
//
// packageLogger resolves slog.Default() at log time (the dynamicHandler shim),
// so swapping the default is enough — no re-construction needed.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func countLines(buf *bytes.Buffer, needle string) int {
	n := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// TestSendMSearchSuppressesRepeatedFailures is the whole point of the streak:
// what must NOT appear in the log.
//
// sendMSearch runs on a ticker, so a persistent failure ("can't assign
// requested address" — the multicast route is gone) recurs on every tick
// forever. Logging each one produced 12 lines/minute unbroken on a real host:
// 199,078 of the last 200,000 lines, ~99.5% of a 301 MB log spanning 72 days.
// The cost is not disk, it is that every other line becomes unfindable.
func TestSendMSearchSuppressesRepeatedFailures(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	sendErr := errors.New("write udp4 0.0.0.0:52175->239.255.255.250:1900: sendto: can't assign requested address")

	// A full day of ticks at the default 30s interval.
	const ticks = 2 * 60 * 24
	for i := 0; i < ticks; i++ {
		c.noteSendResult(sendErr)
	}

	if got := countLines(buf, "M-SEARCH send failed"); got != 1 {
		t.Errorf("first-failure Warn appeared %d times, want exactly 1", got)
	}
	if got := countLines(buf, "failing persistently"); got != 1 {
		t.Errorf("sustained Error appeared %d times, want exactly 1 — repeating it "+
			"would reintroduce the flood", got)
	}
	// The real assertion: O(1) lines for an outage of ANY length.
	total := strings.Count(strings.TrimRight(buf.String(), "\n"), "\n") + 1
	if buf.Len() == 0 {
		total = 0
	}
	if total > 2 {
		t.Errorf("%d log lines for %d consecutive failures, want 2 — pre-fix this "+
			"was one line per tick", total, ticks)
	}
}

// TestSendMSearchEscalatesOnceSustained pins that the Error lands at the
// threshold and not before: a Wi-Fi transition or a sleep/wake cycle resolves
// well inside it, and escalating on the second tick would cry wolf.
func TestSendMSearchEscalatesOnceSustained(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	err := errors.New("boom")

	for i := 1; i < ssdpSendErrEscalateAt; i++ {
		c.noteSendResult(err)
	}
	if got := countLines(buf, "failing persistently"); got != 0 {
		t.Errorf("escalated after %d failures, before the %d threshold",
			ssdpSendErrEscalateAt-1, ssdpSendErrEscalateAt)
	}
	c.noteSendResult(err) // the threshold tick
	if got := countLines(buf, "failing persistently"); got != 1 {
		t.Errorf("sustained Error appeared %d times at the threshold, want 1", got)
	}
}

// TestSendMSearchLogsRecoveryOnce explains the gap. Without this line the log
// shows a failure, then silence, and a reader cannot tell recovery from the
// bridge having stopped trying.
func TestSendMSearchLogsRecoveryOnce(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	for i := 0; i < 50; i++ {
		c.noteSendResult(errors.New("boom"))
	}
	c.noteSendResult(nil)

	if got := countLines(buf, "M-SEARCH send recovered"); got != 1 {
		t.Fatalf("recovery logged %d times, want exactly 1", got)
	}
	if !strings.Contains(buf.String(), "suppressedFailures=50") {
		t.Error("recovery line does not carry the suppressed count, so the silent " +
			"stretch in the log is unexplained")
	}
	// A second success must be silent — the streak is reset.
	c.noteSendResult(nil)
	if got := countLines(buf, "M-SEARCH send recovered"); got != 1 {
		t.Errorf("recovery logged %d times after a second success, want 1", got)
	}
}

// TestSendMSearchSteadyStateIsSilent: the overwhelmingly common case is that
// sends succeed, and that must cost nothing.
func TestSendMSearchSteadyStateIsSilent(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	for i := 0; i < 1000; i++ {
		c.noteSendResult(nil)
	}
	if buf.Len() != 0 {
		t.Errorf("healthy sends produced log output:\n%s", buf.String())
	}
}

// TestSendMSearchReFailsAfterRecovery: a flapping interface must get a fresh
// Warn per outage rather than being permanently muted by one recovery.
func TestSendMSearchReFailsAfterRecovery(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	for round := 0; round < 3; round++ {
		c.noteSendResult(errors.New("boom"))
		c.noteSendResult(nil)
	}
	if got := countLines(buf, "M-SEARCH send failed"); got != 3 {
		t.Errorf("got %d first-failure Warns across 3 separate outages, want 3 — "+
			"a recovered streak must re-arm", got)
	}
}
