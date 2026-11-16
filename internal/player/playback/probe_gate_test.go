package playback

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// gateBackend is a backend whose Locator places no unit: its device slot is
// all these tests drive.
func gateBackend(t *testing.T, afterFunc func(time.Duration, func()) func() bool) *Backend {
	t.Helper()
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, AfterFunc: afterFunc})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheProbesGateReadsTheDeviceSlot pins the gate a probe passes before
// it opens anything (B728 B4; the design's section 5, "When it runs"), one
// step under the backend's lock, the lock a Start's hold takes the slot
// under: a release never confirmed, a probe under way, and a session that
// holds the slot each refuse it, the session's word saying whether its
// release is pending (its Release has come, or the engine has taken the
// release before the call came), it is paused (a pause holds the slot
// whether or not its release has let the device go), or it holds the device
// otherwise (it plays, or it is opening the device); and so does a Start
// that waited for a probe and has not taken the slot yet (round 2). A free
// slot is taken, and the probe's end gives it back and says so.
func TestTheProbesGateReadsTheDeviceSlot(t *testing.T) {
	taken := make(chan struct{})
	close(taken)
	for _, c := range []struct {
		name string
		set  func(b *Backend)
		want ProbeSkip
	}{
		{"a session that plays or opens the device", func(b *Backend) { b.holder = newSession(b, 1) }, ProbeSessionHolds},
		{"a paused session", func(b *Backend) {
			s := newSession(b, 1)
			s.pausedFlag.Store(true)
			b.holder = s
		}, ProbeSessionPaused},
		{"a paused session whose Release has come", func(b *Backend) {
			s := newSession(b, 1)
			s.pausedFlag.Store(true)
			s.cancel()
			b.holder = s
		}, ProbeReleasePending},
		{"a session whose release the engine has taken", func(b *Backend) {
			s := newSession(b, 1)
			s.released = taken
			b.holder = s
		}, ProbeReleasePending},
		{"a release never confirmed", func(b *Backend) { b.terminalUnconfirmed = true }, ProbeReleaseUnconfirmed},
		{"a probe under way", func(b *Backend) { b.probing = &probeHold{done: make(chan struct{})} }, ProbeRunning},
		{"a Start that waited for a probe and has not taken the slot yet", func(b *Backend) { b.waiters = 1 }, ProbeSessionHolds},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := gateBackend(t, nil)
			c.set(b)
			before := b.probing
			if p, why := b.reserveProbe(); p != nil || why != c.want || b.probing != before {
				t.Errorf("the gate: taken %v, %q; want %q and the slot left as it was", p != nil, why, c.want)
			}
		})
	}
	b := gateBackend(t, nil)
	p, why := b.reserveProbe()
	if p == nil || why != "" || b.probing != p {
		t.Fatalf("a free slot: taken %v, %q; want it taken by the probe", p != nil, why)
	}
	b.endProbe(p)
	if b.probing != nil {
		t.Error("the probe's end left the slot taken")
	}
	select {
	case <-p.done:
	default:
		t.Error("the probe's end does not say it ended")
	}
}

// waitTimer stands in for the timer a Start's wait for a probe arms
// (Config.AfterFunc): armed is closed once it is armed, fire runs its
// function, and stopped is closed once the wait stops it.
type waitTimer struct {
	armed, stopped chan struct{}
	d              time.Duration
	fire           func()
}

func newWaitTimer() *waitTimer {
	return &waitTimer{armed: make(chan struct{}), stopped: make(chan struct{})}
}

func (w *waitTimer) afterFunc(d time.Duration, f func()) func() bool {
	w.d, w.fire = d, f
	close(w.armed)
	return func() bool {
		close(w.stopped)
		return true
	}
}

// receiveWithin waits for ch, at most a minute: a watchdog, never a timing
// the test depends on.
func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(time.Minute):
		t.Fatalf("waited a minute for %s", what)
	}
	var zero T
	return zero
}

// TestAStartWaitsForTheProbeThatHoldsTheSlot pins hold's side of the slot
// (B728 B4): a Start that finds a probe holding it waits, holding nothing,
// and takes the slot as the probe ends; its wait is armed at ProbeWait, the
// design's bound, and past it the Start answers the device busy
// (engine.ErrDeviceBusy, which the engine records as a busy attempt),
// holding nothing; its context's end and its session's release end the wait
// too. A Start that gave its wait up no longer counts among the waiters
// (round 2), so the next probe takes the slot once the first ends.
func TestAStartWaitsForTheProbeThatHoldsTheSlot(t *testing.T) {
	type start struct {
		b     *Backend
		s     *session
		p     *probeHold
		timer *waitTimer
		done  chan error
	}
	begin := func(t *testing.T, ctx context.Context) start {
		t.Helper()
		st := start{timer: newWaitTimer(), done: make(chan error, 1)}
		st.b = gateBackend(t, st.timer.afterFunc)
		var why ProbeSkip
		if st.p, why = st.b.reserveProbe(); st.p == nil {
			t.Fatalf("the probe could not take a free slot: %q", why)
		}
		st.s = newSession(st.b, 1)
		go func() { st.done <- st.b.hold(ctx, st.s) }()
		select {
		case <-st.timer.armed:
		case err := <-st.done:
			t.Fatalf("the Start did not wait for the probe that holds the slot: it answered %v", err)
		case <-time.After(time.Minute):
			t.Fatal("waited a minute for the Start's wait for the probe to be armed")
		}
		if st.timer.d != ProbeWait {
			t.Errorf("the wait is armed for %v; want ProbeWait, %v", st.timer.d, ProbeWait)
		}
		st.b.mu.Lock()
		holder := st.b.holder
		st.b.mu.Unlock()
		if holder != nil {
			t.Error("the Start took the slot while the probe held it")
		}
		return st
	}
	// gaveUp checks a Start that gave its wait up: it holds nothing, no
	// longer counts among the waiters (round 2), and so leaves the slot to
	// the next probe once the one it waited for ends.
	gaveUp := func(t *testing.T, st start) {
		t.Helper()
		// The first acquisition after the Start's hold gave its wait up, its
		// answer received, which nothing else can hold here (no goroutine
		// runs): asserted free at once, so a lock the give-up kept fails the
		// case, where a Lock waited to the package's timeout (backlog B853,
		// round 4).
		if !st.b.mu.TryLock() {
			t.Fatal("the backend's lock is held once the Start that gave its wait up returned")
		}
		holder, waiters := st.b.holder, st.b.waiters
		st.b.mu.Unlock()
		if holder != nil {
			t.Error("a Start that gave its wait up took the slot")
		}
		if waiters != 0 {
			t.Errorf("a Start that gave its wait up still counts among the waiters (%d)", waiters)
		}
		st.b.endProbe(st.p)
		if p, why := st.b.reserveProbe(); p == nil {
			t.Errorf("a probe after the one the Start gave up on was refused %q; want the slot", why)
		} else {
			st.b.endProbe(p)
		}
	}
	t.Run("the probe ends", func(t *testing.T) {
		st := begin(t, context.Background())
		st.b.endProbe(st.p)
		if err := receiveWithin(t, st.done, "the Start's hold"); err != nil {
			t.Fatalf("the Start after the probe: %v", err)
		}
		if st.b.holder != st.s {
			t.Error("the Start does not hold the slot once the probe ended")
		}
		receiveWithin(t, st.timer.stopped, "the wait's timer stopped")
	})
	t.Run("the probe outlasts the bound", func(t *testing.T) {
		st := begin(t, context.Background())
		st.timer.fire()
		err := receiveWithin(t, st.done, "the Start's hold")
		if !errors.Is(err, engine.ErrDeviceBusy) {
			t.Fatalf("a Start past the probe's bound: %v; want the device busy", err)
		}
		if st.b.holder != nil || st.b.probing != st.p {
			t.Error("a Start past the bound took the slot from the probe")
		}
		gaveUp(t, st)
	})
	t.Run("the Start's context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		st := begin(t, ctx)
		cancel()
		if err := receiveWithin(t, st.done, "the Start's hold"); !errors.Is(err, context.Canceled) {
			t.Fatalf("a Start whose context ended: %v", err)
		}
		gaveUp(t, st)
	})
	t.Run("the session's release", func(t *testing.T) {
		st := begin(t, context.Background())
		st.s.cancel()
		if err := receiveWithin(t, st.done, "the Start's hold"); !errors.Is(err, errReleased) {
			t.Fatalf("a Start whose session was released: %v", err)
		}
		gaveUp(t, st)
	})
}

// TestAStartThatWaitedTakesTheSlotBeforeAnotherProbe pins the slot's hand
// from a probe to the Start that waited for it (B728 B4 round 2): two
// probes in a row, and a Start that came during the first. The first probe
// ends, and the Start, woken, is held before it takes the slot (the
// StartWoke seam): a second probe asked then is refused, as a session that
// holds the device (ProbeSessionHolds), since the waiting Start counts as
// one, and the Start then takes the slot.
func TestAStartThatWaitedTakesTheSlotBeforeAnotherProbe(t *testing.T) {
	timer := newWaitTimer()
	woke, goOn := make(chan struct{}), make(chan struct{})
	var wokeOnce, goOnce sync.Once
	letGo := func() { goOnce.Do(func() { close(goOn) }) }
	t.Cleanup(letGo)
	b, err := New(Config{Resolver: nopResolver{}, Locator: noUnit{}, AfterFunc: timer.afterFunc,
		Seams: &Seams{StartWoke: func() {
			wokeOnce.Do(func() { close(woke) })
			<-goOn
		}}})
	if err != nil {
		t.Fatal(err)
	}
	first, why := b.reserveProbe()
	if first == nil {
		t.Fatalf("the first probe could not take a free slot: %q", why)
	}
	s := newSession(b, 1)
	done := make(chan error, 1)
	go func() { done <- b.hold(context.Background(), s) }()
	receiveWithin(t, timer.armed, "the Start's wait for the first probe")
	b.endProbe(first)
	receiveWithin(t, woke, "the Start woken as the first probe ended")
	if second, why := b.reserveProbe(); second != nil {
		t.Error("a second probe took the slot from the Start that waited for the first")
		b.endProbe(second)
	} else if why != ProbeSessionHolds {
		t.Errorf("the second probe was refused %q; want %q", why, ProbeSessionHolds)
	}
	letGo()
	if err := receiveWithin(t, done, "the Start's hold"); err != nil {
		t.Fatalf("the Start that waited: %v", err)
	}
	b.mu.Lock()
	holder, waiters := b.holder, b.waiters
	b.mu.Unlock()
	if holder != s || waiters != 0 {
		t.Errorf("after the Start took the slot: it holds it %v, %d Starts still counted waiting", holder == s, waiters)
	}
}
