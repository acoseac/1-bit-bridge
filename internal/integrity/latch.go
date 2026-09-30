package integrity

import (
	"sync/atomic"
	"time"
)

// sweepRefusalRepeat is how often a refusal that goes on is logged again:
// once when a streak of refused ticks starts, then at most once per this
// interval while it lasts.
const sweepRefusalRepeat = 24 * time.Hour

// RefusalStatus is what a background sweep's refusal latch says, for a
// reader on another goroutine: the console's Jobs card, which said "on"
// for a sweep that refused every tick until each sweep published this
// (the orphan sweep on 2026-09-28, the variant watcher on 2026-09-29,
// backlog B131). K is the sweep's kind of refusal, a key the console words.
type RefusalStatus[K comparable] struct {
	// Refusing is the kind of refusal the current streak is; the zero K
	// when the sweep is not refusing.
	Refusing K
	// Since is when the streak started: the first refused tick of it.
	// Zero when Refusing is the zero K.
	Since time.Time
}

// refusalLatch is the log latch of a background sweep that refuses to
// delete: the one state that crosses its ticks. Both sweeps in this
// package keep one, OrphanSidecarSweeper for its three kinds of refusal
// and VariantWatcher for its two (a relocation, and a variants directory
// that reads as unmounted), so they cannot disagree about when a refusal
// is logged, or about when the Jobs card hears of it.
//
// A refusal of this kind lasts until someone acts on it, and a sweep ticks
// every few minutes to hours, so an identical WARN on every tick is the
// M-SEARCH shape: identical lines forever make every other line
// unfindable. So a refusal WARNs when a streak of refused ticks starts,
// again at most once per sweepRefusalRepeat while it lasts, and the first
// tick that proceeds after it says so once, at Info. A tick that decided
// nothing (a listing that failed, a walk that did not finish, a tick the
// shutdown stopped) touches no latch: it is evidence of nothing, so it
// neither ends a streak nor starts one.
//
// A streak is of one KIND of refusal, K, whose zero value means "not
// refusing": a tick refused for another kind starts a new streak and logs
// at once, since its advice differs. Times are the ticks' starts, read from
// time.Now, so the repeat is measured on the monotonic clock and a stepped
// wall clock neither repeats a WARN early nor holds it back.
//
// The latch publishes itself (status) whenever a streak starts or ends, so
// a sweep cannot move the latch and forget to tell the card: the orphan
// sweep did that publishing by hand until 2026-09-29.
//
// Owned by the sweep's run goroutine: not safe for concurrent use, but for
// status, which any goroutine may call.
type refusalLatch[K comparable] struct {
	// kind is the current streak's kind; the zero K outside a streak.
	kind K
	// since is when the current streak started; zero outside one.
	since time.Time
	// lastLog is when the refusal was last logged. Kept across streaks:
	// a new streak logs at once whatever it says.
	lastLog time.Time
	// published is the latch as status reports it: a fresh value stored
	// whenever a streak starts or ends, so a reader takes no lock the tick
	// holds and never sees a value being written.
	published atomic.Pointer[RefusalStatus[K]]
}

// refuse records a tick refused as kind at now, and reports whether the
// tick logs its WARN: the first tick of a streak, of a kind other than the
// running streak's, or the first a sweepRefusalRepeat after the last WARN.
func (l *refusalLatch[K]) refuse(now time.Time, kind K) (logIt bool) {
	if l.kind != kind {
		l.kind, l.since, l.lastLog = kind, now, now
		l.publish()
		return true
	}
	if now.Sub(l.lastLog) < sweepRefusalRepeat {
		return false
	}
	l.lastLog = now
	return true
}

// lift ends a streak and reports the kind it was, the zero K when none was
// running, so the tick that proceeds after a streak logs its Info line
// exactly once, naming what ended, and the ticks outside one say nothing.
func (l *refusalLatch[K]) lift() (ended K) {
	var none K
	if l.kind == none {
		return none
	}
	ended = l.kind
	l.kind, l.since = none, time.Time{}
	l.publish()
	return ended
}

// publish stores the latch for status, as a fresh value each time.
func (l *refusalLatch[K]) publish() {
	l.published.Store(&RefusalStatus[K]{Refusing: l.kind, Since: l.since})
}

// status reports the latch as its owner last left it. Safe from any
// goroutine; the zero status before any streak started.
func (l *refusalLatch[K]) status() RefusalStatus[K] {
	if p := l.published.Load(); p != nil {
		return *p
	}
	return RefusalStatus[K]{}
}
