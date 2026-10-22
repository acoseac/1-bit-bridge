package hotplug

import (
	"sync"
	"time"
)

// fakeClock is a Clock that moves only when a test advances it. A timer
// fires when the clock reaches its deadline, at once when it is armed with
// a deadline already passed, as a real one does. Its channel holds one
// value, and Reset and Stop take back a value not yet received, as Go's
// timers have since 1.23, so the watcher never sees a stale fire.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c     *fakeClock
	ch    chan time.Time
	at    time.Time
	armed bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) NewTimer() Timer {
	t := &fakeTimer{c: c, ch: make(chan time.Time, 1)}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return t
}

// Advance moves the clock by d and fires every armed timer whose deadline
// it reached, earliest first.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for {
		var next *fakeTimer
		for _, t := range c.timers {
			if t.armed && !t.at.After(c.now) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			return
		}
		next.fireLocked()
	}
}

func (t *fakeTimer) C() <-chan time.Time { return t.ch }

func (t *fakeTimer) Reset(deadline time.Time) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.drainLocked()
	t.at, t.armed = deadline, true
	if !t.at.After(t.c.now) {
		t.fireLocked()
	}
}

func (t *fakeTimer) Stop() {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	t.armed = false
	t.drainLocked()
}

func (t *fakeTimer) fireLocked() {
	t.armed = false
	select {
	case t.ch <- t.c.now:
	default:
	}
}

func (t *fakeTimer) drainLocked() {
	select {
	case <-t.ch:
	default:
	}
}
