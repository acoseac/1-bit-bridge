package engine

import (
	"sort"
	"sync"
	"time"
)

// fakeClock is the engine's time in a test: it moves only when the test
// advances it, and then fires every timer that has come due, in order, on
// the test's goroutine.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	seq    int
	timers []*fakeTimer
	armed  chan struct{}
}

// fakeTimer is one AfterFunc on a fakeClock.
type fakeTimer struct {
	c     *fakeClock
	at    time.Time
	seq   int
	f     func()
	done  bool
	fired bool
}

// newFakeClock starts at a fixed instant.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), armed: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armLocked(c.now.Add(d), f)
}

func (c *fakeClock) At(at time.Time, f func()) timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !at.After(c.now) {
		t := &fakeTimer{c: c, at: at, done: true, fired: true}
		go f()
		return t
	}
	return c.armLocked(at, f)
}

// armLocked arms a timer for at; c.mu is held.
func (c *fakeClock) armLocked(at time.Time, f func()) *fakeTimer {
	c.seq++
	t := &fakeTimer{c: c, at: at, seq: c.seq, f: f}
	c.timers = append(c.timers, t)
	close(c.armed)
	c.armed = make(chan struct{})
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	t.c.remove(t)
	return true
}

// remove drops t from the pending timers; c.mu is held.
func (c *fakeClock) remove(t *fakeTimer) {
	for i, p := range c.timers {
		if p == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return
		}
	}
}

// Advance moves the clock d on, firing each timer due by then at its own
// instant, earliest first; a timer a callback arms within the span fires in
// it too.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.Slice(c.timers, func(i, j int) bool {
			if !c.timers[i].at.Equal(c.timers[j].at) {
				return c.timers[i].at.Before(c.timers[j].at)
			}
			return c.timers[i].seq < c.timers[j].seq
		})
		if len(c.timers) == 0 || c.timers[0].at.After(target) {
			c.now = target
			c.mu.Unlock()
			return
		}
		t := c.timers[0]
		c.timers = c.timers[1:]
		if t.at.After(c.now) {
			c.now = t.at
		}
		t.done, t.fired = true, true
		c.mu.Unlock()
		t.f()
	}
}

// Pending is the number of timers not yet fired or stopped.
func (c *fakeClock) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// Armed returns a channel closed the next time a timer is armed.
func (c *fakeClock) Armed() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.armed
}
