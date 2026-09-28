package discovery

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

// captureLogs redirects the default slog handler into a buffer for the test.
//
// packageLogger resolves slog.Default() at log time (the dynamicHandler shim),
// so swapping the default is enough — no re-construction needed. It goes
// through loggingtest.SetDefault, which puts back the log package's output
// and flags as well as the previous default. Putting back the default alone
// left the log package writing into the first test's buffer, and slog's own
// default handler writes through it, so every later line in the binary went
// there too, a failing test's diagnostics included.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return &buf
}

// msearchWriter is the shape of SSDPDiscoveryClient.writeMSearch.
type msearchWriter func(conn *net.UDPConn, b []byte, dst *net.UDPAddr) (int, error)

// sendFails returns a writeMSearch that fails every send with err and puts
// nothing on the wire: a host whose multicast route is gone, on every host.
func sendFails(err error) msearchWriter {
	return func(*net.UDPConn, []byte, *net.UDPAddr) (int, error) { return 0, err }
}

// signalFirst closes entered when the tick loop first reaches send, and
// then runs it. The test waits on entered before it calls Stop, so the
// loop's first send has certainly begun: a test that called Stop first
// would find sendMSearch returning on a nil socket and assert nothing.
func signalFirst(entered chan struct{}, send msearchWriter) msearchWriter {
	var once sync.Once
	return func(conn *net.UDPConn, b []byte, dst *net.UDPAddr) (int, error) {
		once.Do(func() { close(entered) })
		return send(conn, b, dst)
	}
}

// holdPastStop holds the tick loop's send until Stop has closed the socket,
// and then finishes it with send. sendMSearch has taken its snapshot of the
// socket by then, so this is the window a send loses to Stop's close by a
// hair, held open on every run instead of a few in a thousand.
func holdPastStop(c *SSDPDiscoveryClient, send msearchWriter) msearchWriter {
	return func(conn *net.UDPConn, b []byte, dst *net.UDPAddr) (int, error) {
		c.runMu.RLock()
		ctx := c.runCtx
		c.runMu.RUnlock()
		<-ctx.Done()
		// Stop cancels the run and closes the socket in one critical
		// section under runMu, so once this read lock is granted the close
		// has happened.
		c.snapshotConn()
		return send(conn, b, dst)
	}
}

// waitEntered fails the test if the tick loop has not reached its send.
func waitEntered(t *testing.T, entered chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the tick loop never reached its first M-SEARCH send")
	}
}

// startClient starts c on a socket of its own, and stops it when the test
// ends, which a test that fails before its own Stop needs: the held sends
// above wait on the run's context, and only Stop cancels it.
func startClient(t *testing.T, c *SSDPDiscoveryClient) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Skipf("cannot bind a UDP socket in this environment: %v", err)
	}
	t.Cleanup(c.Stop)
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
		c.sendErrs.Note(sendErr)
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
//
// The threshold is ten minutes of the client's own cadence, which at the
// renderer's default 30 s is the 20 failures it was a constant of until the
// policy moved into SendFailureLog, so the literal also pins that this
// client hands its interval to the log.
func TestSendMSearchEscalatesOnceSustained(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	err := errors.New("boom")

	const at = 20
	for i := 1; i < at; i++ {
		c.sendErrs.Note(err)
	}
	if got := countLines(buf, "failing persistently"); got != 0 {
		t.Errorf("escalated after %d failures, before the %d threshold", at-1, at)
	}
	c.sendErrs.Note(err) // the threshold tick
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
		c.sendErrs.Note(errors.New("boom"))
	}
	c.sendErrs.Note(nil)

	if got := countLines(buf, "M-SEARCH send recovered"); got != 1 {
		t.Fatalf("recovery logged %d times, want exactly 1", got)
	}
	if !strings.Contains(buf.String(), "consecutiveFailures=50") {
		t.Error("recovery line does not carry the outage length, so the silent " +
			"stretch in the log is unexplained")
	}
	// A second success must be silent — the streak is reset.
	c.sendErrs.Note(nil)
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
		c.sendErrs.Note(nil)
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
		c.sendErrs.Note(errors.New("boom"))
		c.sendErrs.Note(nil)
	}
	if got := countLines(buf, "M-SEARCH send failed"); got != 3 {
		t.Errorf("got %d first-failure Warns across 3 separate outages, want 3 — "+
			"a recovered streak must re-arm", got)
	}
}

// TestSendMSearchStreakResetsOnRestart pins the restart case, whose failure
// mode is SILENCE — the worst kind for a diagnostic.
//
// A client stopped mid-outage keeps its streak. Carried into a new run, the
// first failure lands past BOTH arms of SendFailureLog.Note's switch (it is
// neither 1 nor exactly the threshold), so a restarted-and-still-broken
// client would log nothing at all — the opposite of what the suppression
// exists for. Reported by Gemini on PR #708.
//
// The new run's first failure is the live loop's own first send, which
// fails through writeMSearch on every host. It used to be a failure the
// test drove itself after Stop, with the loop's real send left to the
// host, and that failed 10 of 200 runs on the dev Mac and 17 of 1,000 on
// Linux under -race (2026-09-28): the loop's send lost a race with Stop's
// close, logged the new run's first-failure Warn before the test captured
// anything, and made the driven failure the second. Moving the capture
// before Start does not fix that where sends go through, because there
// the loop's SUCCESS resets the streak, which hides a Start that did not.
func TestSendMSearchStreakResetsOnRestart(t *testing.T) {
	c := newTestClient(t, &stubDispatcher{})

	// Fail through the escalation so both arms are already spent. No loop
	// is live yet, and the streak is deliberately unsynchronised because
	// runTickLoop is its only production toucher (see SendFailureLog), so
	// driving it directly is safe only here, before Start, or after Stop,
	// which joins the loop.
	for i := 0; i < c.sendErrs.escalateAt+5; i++ {
		c.sendErrs.Note(errors.New("boom"))
	}

	// Restart, still broken. Capture from here, so the first run's lines
	// cannot satisfy the assertion.
	routeGone := errors.New("sendto: can't assign requested address")
	entered := make(chan struct{})
	c.writeMSearch = signalFirst(entered, sendFails(routeGone))
	buf := captureLogs(t)
	startClient(t, c)
	waitEntered(t, entered)
	c.Stop() // joins the loop, so its failure has been noted by now

	if got := countLines(buf, "M-SEARCH send failed"); got != 1 {
		t.Errorf("a restarted client logged %d first-failure Warns, want 1 — with a "+
			"carried-over streak it logs NOTHING, so a still-broken bridge looks healthy", got)
	}
	if !strings.Contains(buf.String(), routeGone.Error()) {
		t.Errorf("the Warn does not carry the failure the restarted loop hit:\n%s", buf.String())
	}
	if c.sendErrs.Streak() != 1 {
		t.Errorf("streak = %d after the restarted run's first failure, want 1", c.sendErrs.Streak())
	}
}

// TestSendMSearchCutShortByStopIsNotAFailure pins that a send Stop's close
// interrupted is a stop: no Warn, and the streak left as it was.
//
// sendMSearch takes a snapshot of the socket and then writes to it, and
// Stop can close the socket between the two. The write then fails with
// net.ErrClosed, which is the stop's own doing and says nothing about the
// multicast route. Measured on main (2026-09-28), a plain Start then Stop
// logged "M-SEARCH send failed … use of closed network connection" in 4 of
// 6,000 cycles on the dev Mac and 43 of 6,000 under -race, and in 0 of
// 2,000 and 24 of 4,000 on Linux. holdPastStop makes that window certain,
// and the write is the socket's own, not a fabricated error.
func TestSendMSearchCutShortByStopIsNotAFailure(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	entered := make(chan struct{})
	c.writeMSearch = signalFirst(entered, holdPastStop(c, (*net.UDPConn).WriteToUDP))
	startClient(t, c)
	waitEntered(t, entered)
	c.Stop()

	if got := countLines(buf, "M-SEARCH send failed"); got != 0 {
		t.Errorf("a send Stop cut short logged %d send-failure Warns, want 0:\n%s", got, buf.String())
	}
	if c.sendErrs.Streak() != 0 {
		t.Errorf("streak = %d after a send Stop cut short, want 0: the stop counted "+
			"as a failure", c.sendErrs.Streak())
	}
}

// TestSendMSearchReportsAFailureThatLandsDuringStop is the other half: only
// the error Stop's close produces is a stop. A send whose own failure lands
// while Stop runs still reports it, as a live loop's would, since the
// multicast route being gone is true whether or not a shutdown is under
// way (the #998 rule: a stopped pass reports no failure the stop CAUSED,
// and every other failure as before). A classification by the run's
// context instead of by the error would make this test fail.
func TestSendMSearchReportsAFailureThatLandsDuringStop(t *testing.T) {
	buf := captureLogs(t)
	c := newTestClient(t, &stubDispatcher{})
	routeGone := errors.New("sendto: can't assign requested address")
	entered := make(chan struct{})
	c.writeMSearch = signalFirst(entered, holdPastStop(c, sendFails(routeGone)))
	startClient(t, c)
	waitEntered(t, entered)
	c.Stop()

	if got := countLines(buf, "M-SEARCH send failed"); got != 1 {
		t.Errorf("a genuine failure during Stop logged %d send-failure Warns, want 1:\n%s", got, buf.String())
	}
	if c.sendErrs.Streak() != 1 {
		t.Errorf("streak = %d after a genuine failure during Stop, want 1", c.sendErrs.Streak())
	}
}
