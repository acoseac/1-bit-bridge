package integrity

import "time"

// sweepRefusalRepeat is how often a refusal that goes on is logged again:
// once when a streak of refused ticks starts, then at most once per this
// interval while it lasts.
const sweepRefusalRepeat = 24 * time.Hour

// refusalLatch is the log latch of a background sweep that refuses to
// delete: the one state that crosses its ticks. Both sweeps in this
// package keep one, OrphanSidecarSweeper for its three kinds of refusal
// and VariantWatcher for its relocation refusal, so they cannot disagree
// about when a refusal is logged.
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
// Owned by the sweep's run goroutine: not safe for concurrent use. A
// reader on another goroutine reads a snapshot the sweep publishes
// (OrphanSidecarSweeper.Status).
type refusalLatch[K comparable] struct {
	// kind is the current streak's kind; the zero K outside a streak.
	kind K
	// since is when the current streak started; zero outside one.
	since time.Time
	// lastLog is when the refusal was last logged. Kept across streaks:
	// a new streak logs at once whatever it says.
	lastLog time.Time
}

// refuse records a tick refused as kind at now. logIt reports whether the
// tick logs its WARN: the first tick of a streak, of a kind other than the
// running streak's, or the first a sweepRefusalRepeat after the last WARN.
// started reports whether this tick started a streak, so a sweep that
// publishes the latch knows when it moved.
func (l *refusalLatch[K]) refuse(now time.Time, kind K) (logIt, started bool) {
	if l.kind != kind {
		l.kind, l.since, l.lastLog = kind, now, now
		return true, true
	}
	if now.Sub(l.lastLog) < sweepRefusalRepeat {
		return false, false
	}
	l.lastLog = now
	return true, false
}

// lift ends a streak and reports whether one was running, so the tick
// that proceeds after a streak logs its Info line exactly once and the
// ticks outside one say nothing.
func (l *refusalLatch[K]) lift() bool {
	var none K
	if l.kind == none {
		return false
	}
	l.kind, l.since = none, time.Time{}
	return true
}
