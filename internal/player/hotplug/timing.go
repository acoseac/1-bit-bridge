package hotplug

import "time"

// The watcher's timing, named here and nowhere else. Serve runs with
// DefaultTiming; a test steps a fake Clock through the same values.
const (
	// DefaultQuiet is how long the kernel's events must stop before a
	// burst is read: one plug sends the USB device, each interface, the
	// card and its PCM and control nodes over a few hundred milliseconds.
	DefaultQuiet = 300 * time.Millisecond
	// DefaultMaxDelay is the longest a burst waits after its first event,
	// however long it goes on.
	DefaultMaxDelay = 2 * time.Second
	// DefaultPeriodic is the reading taken with no event: the safety net
	// for events the kernel dropped or never sent.
	DefaultPeriodic = 10 * time.Second
	// DefaultFollowUps and DefaultFollowUpEvery are the extra readings a
	// card the kernel is still adding or removing gets before it is
	// published as it stands.
	DefaultFollowUps     = 3
	DefaultFollowUpEvery = 500 * time.Millisecond
	// DefaultBackoffMin and DefaultBackoffMax bound the wait after a
	// failed reading, doubling from one to the other.
	DefaultBackoffMin = time.Second
	DefaultBackoffMax = 30 * time.Second
	// DefaultStopWait is how long a stop waits for the watcher's goroutines
	// before it abandons a read stuck in the kernel.
	DefaultStopWait = 2 * time.Second
)

// Timing is the watcher's timing (DefaultTiming).
type Timing struct {
	Quiet, MaxDelay, Periodic time.Duration
	FollowUps                 int
	FollowUpEvery             time.Duration
	BackoffMin, BackoffMax    time.Duration
	StopWait                  time.Duration
}

// DefaultTiming is the timing serve runs the watcher with.
func DefaultTiming() Timing {
	return Timing{Quiet: DefaultQuiet, MaxDelay: DefaultMaxDelay, Periodic: DefaultPeriodic, FollowUps: DefaultFollowUps,
		FollowUpEvery: DefaultFollowUpEvery, BackoffMin: DefaultBackoffMin, BackoffMax: DefaultBackoffMax, StopWait: DefaultStopWait}
}

// holdMax is the longest the binding's answer is held for a card that may
// be the bound unit, from the first reading that held it: a burst's
// longest wait and a full run of follow-ups after it (3.5 s by default).
// Each window renews the follow-ups, so without this an unrelated device
// sending an event every second held the answer for as long as it went on
// (B470 PR 4a2 round 2).
func (t Timing) holdMax() time.Duration {
	return t.MaxDelay + time.Duration(t.FollowUps)*t.FollowUpEvery
}

// backoff is the wait after the n-th failed reading in a row.
func (t Timing) backoff(n int) time.Duration {
	d := t.BackoffMin
	for i := 1; i < n && d < t.BackoffMax; i++ {
		d *= 2
	}
	return min(d, t.BackoffMax)
}

// Clock is the watcher's time: the system's in serve, a fake one a test
// moves by hand.
type Clock interface {
	Now() time.Time
	// NewTimer is a timer not yet armed.
	NewTimer() Timer
}

// Timer is a Clock's timer, armed at a deadline, never for a duration: a
// deadline turned into a duration by one read of the clock and armed after
// another would land late by whatever passed in between (with a fake
// clock, a whole step of the test's, the fire taken back by the re-arm). A
// deadline already passed fires at once. Reset and Stop leave no stale
// value to receive, as time.Timer's have since Go 1.23.
type Timer interface {
	C() <-chan time.Time
	Reset(deadline time.Time)
	Stop()
}

// SystemClock is the system's time.
var SystemClock Clock = systemClock{}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) NewTimer() Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return systemTimer{t}
}

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }

func (t systemTimer) Reset(deadline time.Time) { t.t.Reset(time.Until(deadline)) }

func (t systemTimer) Stop() { t.t.Stop() }
