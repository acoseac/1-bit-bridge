package discovery

import (
	"errors"
	"log/slog"
	"net"
	"time"
)

// sendErrEscalateAfter is how long a discovery client's M-SEARCH sends must
// keep failing before its SendFailureLog writes the one Error: long enough
// that a Wi-Fi transition or a sleep/wake cycle has resolved itself, short
// enough that a genuinely dead multicast route is on record within one
// coffee break.
//
// A duration, which each client turns into a count of its own sends
// (sendErrEscalateAt). It was a count, 20, until 2026-09-28, which is ten
// minutes only at the renderer client's default 30 s cadence: the upstream
// MediaServer client sends every 60 s by default, and both cadences are the
// operator's to change.
const sendErrEscalateAfter = 10 * time.Minute

// sendErrEscalateAt is the consecutive failure at which a client sending
// every interval has been failing for sendErrEscalateAfter, rounded up: 20
// at the renderer client's default 30 s and 10 at the upstream client's
// default 60 s. It is never below 2, so the streak's first failure (the
// Warn) and the Error stay two lines however long the cadence; a cadence
// that is not positive, which no running client has since both constructors
// default it, takes that floor.
func sendErrEscalateAt(interval time.Duration) int {
	if interval <= 0 {
		return 2
	}
	n := int(sendErrEscalateAfter / interval)
	if sendErrEscalateAfter%interval != 0 {
		n++
	}
	if n < 2 {
		return 2
	}
	return n
}

// SendFailureLog is the streak-suppressed report of one sender's failed
// multicast sends on a ticker. Both discovery clients keep one for their
// M-SEARCH sends, this package's renderer client and internal/upnp's
// MediaServer client, and internal/dlna's SSDP advertiser one for its
// periodic NOTIFY ssdp:alive bursts (one result per burst), so the policy
// has one definition and the three cannot drift, as HandleReadErr is one
// definition for the read loops. The MediaServer client discarded every
// send error until 2026-09-28, so a dead multicast route left upstream
// discovery silent in the log, and the advertiser wrote its failures at
// Debug, which the default level does not show.
//
// # Why the failure log is streak-suppressed
//
// A client sends on a ticker, so a send failure is not a one-off: when it
// fails it fails on EVERY tick, forever, and the renderer client logged on
// every one of them. The failure mode is persistent by nature ("can't assign
// requested address" means the multicast route is gone, not that the packet
// was unlucky), so the second line adds nothing the first did not say.
// Measured on the author's Mac before the streak: 12 lines/minute, unbroken,
// producing 199,078 of the last 200,000 log lines and ~99.5% of a 301 MB log
// spanning 72 days. That is not merely wasted disk: it buries every other
// line, so the log stops being usable for the diagnosis it exists for.
//
// So the streak's first failure is a Warn, the failure that completes
// sendErrEscalateAfter of them is one Error, and every other failure is
// silent. HandleReadErr has the same escalation shape but suppresses
// nothing between: its errors are bounded by a 250 ms backoff and,
// empirically, it logged zero lines across those same 72 days, while a
// ticker has no backoff to bound it. Recovery logs once, carrying the
// streak's length, so the gap in the log is explained rather than
// mysterious. Every line names the interface the sends are pinned to: each
// LAN-eligible interface has a client of its own, and a route can be gone on
// one of them while the others send.
//
// # A send Stop cut short is a stop, not a failure
//
// A client snapshots its socket and then writes to it, and its Stop can
// close the socket in between: the write then fails with net.ErrClosed.
// That error is the stop's own doing and says nothing about the multicast
// route, so Note drops it before the streak sees it: no Warn, and no count a
// restart would have to reset. Measured on the renderer client (2026-09-28),
// a plain Start then Stop logged "M-SEARCH send failed … use of closed
// network connection" in 4 of 6,000 cycles on macOS, 43 of 6,000 under
// -race, and 24 of 4,000 on Linux under -race.
//
// Only that error is dropped, never a failure that merely lands while a
// shutdown is under way: nothing here consults a context. A client's socket
// is its own and only its Stop closes it, which is what makes net.ErrClosed
// name the stop exactly; a sender whose socket something else can close
// cannot use this. A write takes no context and fails for the same reasons
// whether or not the run is ending, so a route that is gone at shutdown is
// still gone, and still reported (the #998 rule the serve loops follow).
// HandleReadErr's read side also exits on the context, but that decides
// whether its LOOP returns; this decides what a result means.
//
// # The streak has no lock
//
// Only the client's tick loop notes results, and Start spawns that loop
// once and refuses to spawn it again while it runs, so the streak belongs to
// that one goroutine. (The advertiser also notes its initial burst, in Start
// before it spawns the loop, and builds a fresh log there, which is its
// Reset; its Stop's byebye burst notes nothing, since the loop may be
// mid-burst when it runs.) That makes it a real constraint on TESTS, not just a
// note about production: a test that calls Note or Reset directly must do so
// while no tick loop is live, before Start or after Stop (which joins the
// loop). One that did neither raced under -race on CI and was not
// reproducible locally in 26 runs, which is the shape this kind of bug takes.
// Adding a mutex to make that test safe would be paying production for a
// test's convenience; ordering the test correctly costs nothing.
type SendFailureLog struct {
	log      *slog.Logger
	what     string
	iface    string
	degraded string

	// escalateAt is the consecutive failure that writes the one Error:
	// sendErrEscalateAt of the client's cadence.
	escalateAt int

	// streak counts consecutive failed sends: the tick loop's alone (see
	// the type docblock).
	streak int
}

// NewSendFailureLog returns the failure log for one sender's sends. log is
// the sender's logger; what names the sends in every line ("M-SEARCH",
// "NOTIFY"); iface names the interface the sends are pinned to; degraded
// names what stops working while they fail, in the Error line ("renderer
// discovery"); interval is the send cadence, which places that Error
// sendErrEscalateAfter into the streak.
func NewSendFailureLog(log *slog.Logger, what, iface, degraded string, interval time.Duration) SendFailureLog {
	return SendFailureLog{
		log:        log,
		what:       what,
		iface:      iface,
		degraded:   degraded,
		escalateAt: sendErrEscalateAt(interval),
	}
}

// Note applies the policy to one send's result, nil for a send that went
// out.
func (l *SendFailureLog) Note(err error) {
	if errors.Is(err, net.ErrClosed) {
		return
	}
	if err == nil {
		if l.streak > 0 {
			// `consecutiveFailures`, not `suppressedFailures`: this is the
			// whole streak, and up to two of those DID produce a line (the
			// first, and the escalation), so calling it "suppressed" was off
			// by two. Reporting the outage LENGTH is also the more useful
			// number, it is what an operator wants, and it matches the
			// `consecutive` key on the escalation line rather than inventing
			// a second vocabulary. (CodeRabbit, PR #708.)
			l.log.Info(l.what+" send recovered",
				"interface", l.iface, "consecutiveFailures", l.streak)
			l.streak = 0
		}
		return
	}
	l.streak++
	switch l.streak {
	case 1:
		l.log.Warn(l.what+" send failed", "interface", l.iface, "err", err.Error())
	case l.escalateAt:
		// One Error, then silence until recovery. Repeating it would
		// reintroduce exactly the flood this exists to stop.
		l.log.Error(l.what+" send failing persistently; "+l.degraded+" is degraded",
			"interface", l.iface, "consecutive", l.streak, "err", err.Error(),
			"note", "further identical failures are suppressed until it recovers")
	}
}

// Reset starts a new streak, and Start calls it for every run. A client
// stopped mid-outage keeps its streak, and carried into a new run the new
// run's FIRST failure would land past both the Warn and the Error, so a
// restarted and still broken client would log nothing at all, the opposite
// of what the suppression is for. (Gemini, PR #708.)
func (l *SendFailureLog) Reset() { l.streak = 0 }

// Streak is how many sends in a row have failed since the last one that
// went out, or the last Reset.
func (l *SendFailureLog) Streak() int { return l.streak }
