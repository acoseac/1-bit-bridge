package engine

import "time"

// clock is the engine's time: every deadline, expiry, coalescing delay and
// backoff is a timer on it, so a test decides when each one fires.
type clock interface {
	Now() time.Time
	// AfterFunc runs f on a goroutine of the clock's own once d has passed,
	// unless the returned timer is stopped first.
	AfterFunc(d time.Duration, f func()) timer
	// At is AfterFunc to an instant: f runs once the clock reaches t, at
	// once when it has already. A deadline computed from an earlier reading
	// of the clock is armed with At, so the clock moving between the
	// reading and the arming cannot make the timer late.
	At(t time.Time, f func()) timer
}

// timer is a pending AfterFunc.
type timer interface {
	// Stop reports whether it stopped the timer before it fired.
	Stop() bool
}

// realClock is the time package's.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) timer { return time.AfterFunc(d, f) }

func (realClock) At(t time.Time, f func()) timer { return time.AfterFunc(time.Until(t), f) }
