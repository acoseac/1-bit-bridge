package engine

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// The configuration reservation's tests (B728 B9 PR 3): a write that must not
// run beside playback, a DAC's DSD choice, holds it, and nothing starts while
// it is held. Every wait is for a signal the engine or the fake publishes,
// bounded by the watchdog; nothing sleeps.

// reserveWithin is ReserveConfiguration on bg, answered under the watchdog.
func (h *harness) reserveWithin(what string) (ConfigLease, error) {
	h.t.Helper()
	type answer struct {
		l   ConfigLease
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		l, err := h.eng.ReserveConfiguration(bg)
		ch <- answer{l, err}
	}()
	select {
	case a := <-ch:
		return a.l, a.err
	case <-time.After(*watchdog):
		h.t.Fatalf("waited %v for %s to answer", *watchdog, what)
	}
	return ConfigLease{}, nil
}

// releaseWithin is ReleaseConfiguration of l, returned under the watchdog.
func (h *harness) releaseWithin(what string, l ConfigLease) {
	h.t.Helper()
	h.must(what, h.result(what, async(func() error {
		h.eng.ReleaseConfiguration(l)
		return nil
	})))
}

// refusedReservation requires a reservation asked now to be refused for
// reason, changing nothing: the snapshot's generation as it was, and no
// reservation shown.
func refusedReservation(t *testing.T, h *harness, what string, reason Reason) {
	t.Helper()
	before := h.snap().Generation()
	_, err := h.reserveWithin(what)
	if got := ReasonOf(err); got != reason {
		t.Fatalf("%s: %v (reason %q), want a refusal %q", what, err, got, reason)
	}
	sn := h.snap()
	if sn.Generation() != before {
		t.Errorf("%s: the refusal moved the generation from %v to %v", what, before, sn.Generation())
	}
	if sn.ConfigurationReserved() {
		t.Errorf("%s: a reservation is shown after its refusal", what)
	}
}

// letGoAtEnd opens g in the test's cleanup, unless the test has: registered
// after the harness boots, it runs before the harness's shutdown waits on what
// g holds.
func letGoAtEnd(t *testing.T, g gate) {
	t.Cleanup(func() {
		select {
		case <-g:
		default:
			g.open()
		}
	})
}

// findConfigLease walks v for the lease l, as a ConfigLease, a map's key or
// its bytes, and returns where it is: the lease is its holder's alone.
func findConfigLease(v reflect.Value, l ConfigLease, path string) string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return findConfigLease(v.Elem(), l, path)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if p := findConfigLease(v.Field(i), l, path+"."+v.Type().Field(i).Name); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			for i := range b {
				b[i] = byte(v.Index(i).Uint())
			}
			if bytes.Contains(b, l.b[:]) {
				return path
			}
			return ""
		}
		for i := 0; i < v.Len(); i++ {
			if p := findConfigLease(v.Index(i), l, path); p != "" {
				return p
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if p := findConfigLease(k, l, path+"[key]"); p != "" {
				return p
			}
			if p := findConfigLease(v.MapIndex(k), l, path); p != "" {
				return p
			}
		}
	}
	return ""
}

// TestAConfigurationReservationIsGrantedOnlyWhileStopped pins the grant (B728
// B9 PR 3): stopped with entries, the owner grants it, the snapshot shows it
// held, the generation stays where it was, no snapshot holds the lease, and
// once it is let go none is shown; in every other state the transport can be
// in (a start the backend holds, playing, paused with its session or with
// none, a release to stop, to pause or to play under way, a release left
// unconfirmed past ReleaseTimeout) it is refused ReasonNotStopped, changing
// nothing; under maintenance ReasonMaintenance; and once the shutdown has
// begun, and after Run, ReasonClosed.
func TestAConfigurationReservationIsGrantedOnlyWhileStopped(t *testing.T) {
	t.Run("stopped with entries", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a", "b")
		h.available()
		before := h.snap().Generation()
		l, err := h.reserveWithin("a reservation while stopped")
		h.must("a reservation while stopped", err)
		sn := h.snap()
		if !sn.ConfigurationReserved() {
			t.Fatal("granted, and the snapshot shows no reservation held")
		}
		if sn.Generation() != before {
			t.Errorf("the grant moved the generation from %v to %v", before, sn.Generation())
		}
		if path := findConfigLease(reflect.ValueOf(sn), l, "snapshot"); path != "" {
			t.Errorf("the snapshot holds the lease at %s", path)
		}
		h.releaseWithin("the release", l)
		if h.snap().ConfigurationReserved() {
			t.Fatal("let go, and the snapshot still shows a reservation held")
		}
	})
	t.Run("a start the backend holds", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b")
		h.available()
		g := newGate()
		letGoAtEnd(t, g)
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				if err := g.wait(ctx, h.be, true); err != nil {
					return err
				}
				return s.open()
			}
		})
		play := async(func() error { return h.eng.PlayID(bg, ids[0]) })
		h.be.nth(1)
		h.waitSnap("the start under way", func(sn *Snapshot) bool { return sn.State() == Playing && sn.Starting() })
		refusedReservation(t, h, "a reservation while a start is held", ReasonNotStopped)
		g.open()
		h.must("the play whose start was held", h.result("play", play))
	})
	t.Run("playing", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a", "b")
		refusedReservation(t, h, "a reservation while playing", ReasonNotStopped)
	})
	t.Run("paused with its session", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		refusedReservation(t, h, "a reservation while paused", ReasonNotStopped)
	})
	t.Run("paused with no session", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		h.must("delete the paused entry", h.eng.Delete(bg, ids[0]))
		h.waitSnap("paused at b with no session", func(sn *Snapshot) bool { return sn.State() == Paused && sn.Selected() == ids[1] })
		refusedReservation(t, h, "a reservation while paused with no session", ReasonNotStopped)
	})
	t.Run("releasing to stop", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.playing(0, "a", "b")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		stop := async(func() error { return h.eng.Stop(bg) })
		h.be.waitFor("the stop's release", func() bool { return s.releaseAsked })
		h.waitSnap("releasing to stop", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentStop })
		refusedReservation(t, h, "a reservation while a release to stop is under way", ReasonNotStopped)
		g.open()
		h.must("the stop", h.result("stop", stop))
	})
	t.Run("releasing to pause", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		g := holdReleases(h)
		letGoAtEnd(t, g)
		seek := async(func() error { return h.eng.SeekAbsolute(bg, 30) })
		h.be.waitFor("the seek's release", func() bool { return s.releaseAsked })
		h.waitSnap("releasing to pause", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentPause })
		refusedReservation(t, h, "a reservation while a seek's release to pause is under way", ReasonNotStopped)
		g.open()
		h.must("the seek", h.result("seek", seek))
	})
	t.Run("releasing to play", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.playing(0, "a", "b")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		next := async(func() error { return h.eng.Next(bg) })
		h.be.waitFor("the next's release", func() bool { return s.releaseAsked })
		h.waitSnap("releasing to play", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentPlay })
		refusedReservation(t, h, "a reservation while a release to play is under way", ReasonNotStopped)
		g.open()
		h.must("the next", h.result("next", next))
	})
	t.Run("a release left unconfirmed", func(t *testing.T) {
		h := startHarness(t)
		mark := h.appliedMark()
		_, s := h.playing(0, "a", "b")
		// The clock is moved past the release's deadline, and so past the
		// successor's Prepare's: its answer is the owner's first.
		h.waitApplied("handled call prepare", mark)
		never := newGate()
		letGoAtEnd(t, never)
		h.be.script(func() {
			h.be.onRelease = func(ctx context.Context, _ *fakeSession) error { return never.wait(ctx, h.be, true) }
		})
		stop := async(func() error { return h.eng.Stop(bg) })
		h.be.waitFor("the stop's release", func() bool { return s.releaseAsked })
		h.clk.Advance(ReleaseTimeout)
		h.must("the stop past its release's deadline", h.result("stop", stop))
		h.waitSnap("the release unconfirmed", func(sn *Snapshot) bool {
			_, ok := sn.ReleaseUnconfirmed()
			return ok && sn.State() == Stopped
		})
		refusedReservation(t, h, "a reservation while a release is unconfirmed", ReasonNotStopped)
	})
	t.Run("maintenance", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a")
		hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
		h.must("enter", err)
		refusedReservation(t, h, "a reservation under maintenance", ReasonMaintenance)
		h.must("exit", h.eng.ExitMaintenance(bg, hold.Lease))
	})
	t.Run("the shutdown", func(t *testing.T) {
		h := startHarness(t)
		_, s := h.playing(0, "a")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		h.cancel()
		h.be.waitFor("the shutdown's release", func() bool { return s.releaseAsked })
		h.waitSnap("the shutdown begun", func(sn *Snapshot) bool { return sn.Closed() })
		refusedReservation(t, h, "a reservation once the shutdown began", ReasonClosed)
		g.open()
		h.drive(h.ran)
		h.down = true
		refusedReservation(t, h, "a reservation after Run", ReasonClosed)
	})
}

// TestWhateverWouldStartASessionIsRefusedWhileAReservationIsHeld pins the
// refusal (B728 B9 PR 3): stopped, a reservation held, Play, Next, Previous,
// PlayID of another entry and PlayIndex of another index are each refused
// ReasonConfiguring, the selection, the generation and the backend's starts as
// they were; Stop, Pause, SetRepeat, SetShuffle(false), an insert, a deletion,
// the output's reports, a seek (refused for having no track to seek in, as
// ever), Next past the end with repeat off and DeleteAll run as they would
// with none, the reservation still held; once it is let go, Play plays.
func TestWhateverWouldStartASessionIsRefusedWhileAReservationIsHeld(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b", "c")
	h.available()
	h.must("play b", h.eng.PlayID(bg, ids[1]))
	h.must("stop", h.eng.Stop(bg))
	l, err := h.reserveWithin("the reservation")
	h.must("the reservation", err)
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"play", func() error { return h.eng.Play(bg) }},
		{"next", func() error { return h.eng.Next(bg) }},
		{"previous", func() error { return h.eng.Previous(bg) }},
		{"play id c", func() error { return h.eng.PlayID(bg, ids[2]) }},
		{"play index 0", func() error { return h.eng.PlayIndex(bg, 0) }},
	} {
		before, starts := h.snap(), h.be.starts()
		if err := c.run(); ReasonOf(err) != ReasonConfiguring {
			t.Fatalf("%s while a reservation is held: %v, want %s", c.name, err, ReasonConfiguring)
		}
		if sn := h.snap(); sn.Selected() != before.Selected() || sn.Generation() != before.Generation() || sn.State() != Stopped {
			t.Errorf("%s refused changed the engine: %s at %d, generation %v; was %s at %d, generation %v",
				c.name, sn.State(), sn.Selected(), sn.Generation(), before.State(), before.Selected(), before.Generation())
		}
		if n := h.be.starts(); n != starts {
			t.Errorf("%s refused started %d sessions", c.name, n-starts)
		}
	}

	starts := h.be.starts()
	h.must("stop beside the reservation", h.eng.Stop(bg))
	h.must("pause beside the reservation", h.eng.Pause(bg))
	h.must("repeat on beside the reservation", h.eng.SetRepeat(bg, true))
	if !h.snap().Repeat() {
		t.Error("repeat on beside the reservation did not take")
	}
	h.must("repeat off beside the reservation", h.eng.SetRepeat(bg, false))
	h.must("shuffle off beside the reservation", h.eng.SetShuffle(bg, false))
	d, err := h.eng.InsertAfter(bg, ids[2], ref("d"), nil)
	h.must("an insert beside the reservation", err)
	h.must("a deletion beside the reservation", h.eng.Delete(bg, d))
	h.report(OutputReport{Available: false, Reason: "device_missing"})
	h.available()
	h.refused("a seek beside the reservation", h.eng.SeekAbsolute(bg, 30), ReasonSeekUnavailable)
	h.must("delete c, leaving b the last", h.eng.Delete(bg, ids[2]))
	h.must("next past the end beside the reservation", h.eng.Next(bg))
	if sn := h.snap(); sn.Selected() != ids[1] || sn.State() != Stopped {
		t.Errorf("next past the end beside the reservation: %s at %d, want stopped at b", sn.State(), sn.Selected())
	}
	h.must("delete all beside the reservation", h.eng.DeleteAll(bg))
	if n := h.be.starts(); n != starts {
		t.Errorf("the commands beside the reservation started %d sessions", n-starts)
	}
	if !h.snap().ConfigurationReserved() {
		t.Fatal("the commands beside the reservation let it go")
	}
	h.releaseWithin("the release", l)
	h.playing(0, "x")
}

// injectingHarness is a booted harness whose owner, as it admits a
// SetShuffle that put asked for, puts a lease among the reservations held as
// no grant could, whatever the transport; put returns once the owner has.
func injectingHarness(t *testing.T) (h *harness, put func(l ConfigLease)) {
	h = newHarness(t)
	var pending atomic.Pointer[ConfigLease]
	h.tweak = func(c *Config) {
		admitted := c.seams.admitted
		c.seams.admitted = func(cmd *command) {
			admitted(cmd)
			if cmd.op.String() != "set shuffle" {
				return
			}
			if l := pending.Swap(nil); l != nil {
				h.eng.o.configs[*l] = struct{}{}
				h.eng.touched()
			}
		}
	}
	h.boot()
	return h, func(l ConfigLease) {
		t.Helper()
		pending.Store(&l)
		h.must("the shuffle the lease is put in with", h.eng.SetShuffle(bg, false))
	}
}

// TestTheReservationIsAskedWhereverASessionWouldStart pins the two places
// whatever starts a session asks the reservation besides a transport
// command's own (B728 B9 PR 3): a seek that would play, and the play a release
// carries once it is acknowledged. Neither can meet a reservation a grant
// made, which needs the transport stopped with nothing under way, so a hook
// on the owner's goroutine puts a lease in as no grant could: a seek while
// playing is refused ReasonConfiguring, the session still playing and no
// release asked; a Next's play, its release acknowledged while the lease is
// held, is refused so, the Next's answer, the transport stopped with one start
// in all. Each lease let go, the seek and Play are admitted.
func TestTheReservationIsAskedWhereverASessionWouldStart(t *testing.T) {
	t.Run("a seek that would play", func(t *testing.T) {
		h, put := injectingHarness(t)
		_, s := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
		l := ConfigLease{b: [16]byte{1}}
		put(l)
		before, mark := h.snap(), h.be.mark()
		h.refused("a seek while a lease is held", h.eng.SeekAbsolute(bg, 30), ReasonConfiguring)
		unchanged(t, "the seek refused", before, h.snap())
		if calls := h.be.callsSince(mark); len(calls) != 0 {
			t.Errorf("the seek refused made the calls %v", calls)
		}
		h.releaseWithin("the lease's release", l)
		h.must("the seek once the lease is let go", h.eng.SeekAbsolute(bg, 30))
		if s2 := h.be.nth(2); s2.first.ID != s.first.ID || s2.from != 30*44100 {
			t.Errorf("the seek once the lease is let go started %d from %d, want %d from %d", s2.first.ID, s2.from, s.first.ID, 30*44100)
		}
		h.must("play once the lease is let go", h.eng.Play(bg))
	})
	t.Run("the play a release carries", func(t *testing.T) {
		h, put := injectingHarness(t)
		ids, s := h.playing(0, "a", "b")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		next := async(func() error { return h.eng.Next(bg) })
		h.be.waitFor("the next's release", func() bool { return s.releaseAsked })
		h.waitSnap("releasing to play b", func(sn *Snapshot) bool {
			return sn.State() == Releasing && sn.Pending() == IntentPlay && sn.Selected() == ids[1]
		})
		l := ConfigLease{b: [16]byte{2}}
		put(l)
		g.open()
		h.refused("the next whose release was acknowledged while a lease is held", h.result("next", next), ReasonConfiguring)
		if sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped }); sn.Selected() != ids[1] {
			t.Errorf("the next refused at its release's end left %d selected, want b", sn.Selected())
		}
		if n := h.be.starts(); n != 1 {
			t.Fatalf("%d sessions started, want the first alone", n)
		}
		h.releaseWithin("the lease's release", l)
		h.must("play once the lease is let go", h.eng.Play(bg))
		if n := h.be.starts(); n != 2 {
			t.Errorf("play once the lease is let go: %d sessions started, want 2", n)
		}
	})
}

// TestAReservationGrantedAsItsCallerLeavesIsLetGo pins a grant never lost to
// its caller (B728 B9 PR 3, review finding D4): the owner, having granted the
// reservation and published it, is held as it sends the reply, and the
// caller's context ends there. A command context that could end would let its
// caller give up and the lease stand for ever, so where the command's context
// (the admitted seam's) can end, the test waits for the caller to return
// before letting the reply go. The caller is answered ReasonCancelled
// wrapping ErrNotAdmitted and its context's error, with WithAdmission's
// channel closed (the owner did admit the command), and no reservation is
// held once it returns: the grant was let go, and Play is admitted.
func TestAReservationGrantedAsItsCallerLeavesIsLetGo(t *testing.T) {
	h := newHarness(t)
	captured := make(chan context.Context, 1)
	parked, release := make(chan *Snapshot, 1), newGate()
	var armed atomic.Bool
	armed.Store(true)
	h.tweak = func(c *Config) {
		admitted := c.seams.admitted
		c.seams.admitted = func(cmd *command) {
			admitted(cmd)
			if cmd.op.String() == "reserve configuration" {
				select {
				case captured <- cmd.ctx:
				default:
				}
			}
		}
		c.seams.replying = func(op string, _ reply, s *Snapshot) {
			if op == "reserve configuration" && armed.CompareAndSwap(true, false) {
				parked <- s
				<-release
			}
		}
	}
	h.boot()
	letGoAtEnd(t, release)
	h.insert("a")
	h.available()
	base, cancel := context.WithCancel(bg)
	defer cancel()
	ctx, admission := WithAdmission(base)
	answered := make(chan error, 1)
	go func() {
		_, err := h.eng.ReserveConfiguration(ctx)
		answered <- err
	}()
	var at *Snapshot
	select {
	case at = <-parked:
	case err := <-answered:
		t.Fatalf("the reservation answered %v before the owner sent its reply", err)
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to reply to the reservation", *watchdog)
	}
	if !at.ConfigurationReserved() {
		t.Fatal("the step that granted the reservation published none")
	}
	var cmdCtx context.Context
	select {
	case cmdCtx = <-captured:
	default:
		t.Fatal("the owner admitted the reservation unseen")
	}
	cancel()
	var err error
	returned := false
	if cmdCtx.Done() != nil {
		// A command context that can end: its caller gives up as it ends,
		// before the reply goes.
		err, returned = h.result("the reservation whose caller left", answered), true
	}
	release.open()
	if !returned {
		err = h.result("the reservation whose caller left", answered)
	}
	if !errors.Is(err, ErrNotAdmitted) || ReasonOf(err) != ReasonCancelled || !errors.Is(err, context.Canceled) {
		t.Errorf("a reservation granted as its caller left: %v; want a cancellation wrapping ErrNotAdmitted and the context's error", err)
	}
	if h.snap().ConfigurationReserved() {
		t.Fatal("the reservation granted as its caller left is still held")
	}
	select {
	case <-admission:
	default:
		t.Error("the reservation's admission channel is open, and the owner admitted it")
	}
	h.must("play once the grant was let go", h.eng.Play(bg))
}

// TestTwoReservationsHoldUntilBothAreLetGo pins the leases (B728 B9 PR 3):
// two grants are two leases; a Play is refused while either is held, the first
// let go and the second not, and admitted once both are; a lease let go twice
// changes nothing, the second release publishing no snapshot.
func TestTwoReservationsHoldUntilBothAreLetGo(t *testing.T) {
	h := startHarness(t)
	mark := h.appliedMark()
	h.insert("a")
	h.available()
	first, err := h.reserveWithin("the first reservation")
	h.must("the first reservation", err)
	second, err := h.reserveWithin("the second reservation")
	h.must("the second reservation", err)
	if first == second {
		t.Fatal("two grants share one lease")
	}
	h.releaseWithin("the first's release", first)
	if !h.snap().ConfigurationReserved() {
		t.Fatal("the first let go, and no reservation is shown while the second is held")
	}
	h.refused("play while the second is held", h.eng.Play(bg), ReasonConfiguring)
	// The insert's ID left less than a block reserved ahead, so it began a
	// reservation write on a goroutine of its own, and the owner publishes a
	// snapshot as it handles that write's report. The window below opens once
	// it has, so a snapshot published in it is the second release's (B916).
	h.waitApplied("handled reservation", mark)
	before, changed := h.snap(), h.eng.Changed()
	h.releaseWithin("the first's release again", first)
	select {
	case <-changed:
		t.Error("the first let go again published a snapshot")
	default:
	}
	if sn := h.snap(); sn != before || !sn.ConfigurationReserved() {
		t.Errorf("the first let go again changed the engine: reserved %v", sn.ConfigurationReserved())
	}
	h.refused("play once the first was let go twice", h.eng.Play(bg), ReasonConfiguring)
	h.releaseWithin("the second's release", second)
	if h.snap().ConfigurationReserved() {
		t.Fatal("both let go, and a reservation is still shown")
	}
	h.must("play once both are let go", h.eng.Play(bg))
	if n := h.be.starts(); n != 1 {
		t.Errorf("play once both are let go started %d sessions, want 1", n)
	}
}

// TestAReleasedReservationIsGoneWhenItsReleaseReturns pins the release as a
// command (B728 B9 PR 3, H19): held, the reservation is shown and a Play is
// refused; the owner, held as it admits the release, has not let the lease go
// and the release has not returned; once it returns, at once, no reservation
// is shown and a Play is admitted. A release posted as a message would return
// before the owner took it, and a Play sent after it could be taken first.
func TestAReleasedReservationIsGoneWhenItsReleaseReturns(t *testing.T) {
	h := newHarness(t)
	reached, admit := make(chan struct{}, 1), newGate()
	var armed atomic.Bool
	h.tweak = func(c *Config) {
		admitted := c.seams.admitted
		c.seams.admitted = func(cmd *command) {
			admitted(cmd)
			if cmd.op.String() == "release configuration" && armed.CompareAndSwap(true, false) {
				reached <- struct{}{}
				<-admit
			}
		}
	}
	h.boot()
	letGoAtEnd(t, admit)
	h.insert("a")
	h.available()
	l, err := h.reserveWithin("the reservation")
	h.must("the reservation", err)
	if !h.snap().ConfigurationReserved() {
		t.Fatal("granted, and the snapshot shows no reservation held")
	}
	h.refused("play while the reservation is held", h.eng.Play(bg), ReasonConfiguring)
	armed.Store(true)
	released := make(chan struct{})
	go func() {
		h.eng.ReleaseConfiguration(l)
		close(released)
	}()
	select {
	case <-reached:
	case <-released:
		t.Fatal("the release returned before the owner took it as a command")
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to take the release", *watchdog)
	}
	select {
	case <-released:
		t.Fatal("the release returned while the owner held it before its step")
	default:
	}
	if !h.snap().ConfigurationReserved() {
		t.Fatal("the lease is gone before the owner stepped its release")
	}
	admit.open()
	select {
	case <-released:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the release to return", *watchdog)
	}
	if h.snap().ConfigurationReserved() {
		t.Fatal("the release returned, and the snapshot still shows the reservation")
	}
	h.must("play at once after the release", h.eng.Play(bg))
	if n := h.be.starts(); n != 1 {
		t.Errorf("play after the release started %d sessions, want 1", n)
	}
}

// TestAReservationAfterRunIsClosedAndItsReleaseReturns pins the shutdown's
// two sides (B728 B9 PR 3): with a lease held, once Run's shutdown has begun
// (held in its closing phase, the queue file's writer held as it waits) a
// reservation is refused ReasonClosed, and the lease's release returns, the
// snapshot then published showing none held; after Run both answer at once.
func TestAReservationAfterRunIsClosedAndItsReleaseReturns(t *testing.T) {
	h := newHarness(t)
	held, letWriterGo := make(chan struct{}, 1), newGate()
	var armed atomic.Bool
	h.tweak = func(c *Config) {
		c.seams.writerWaits = func(uint64) {
			if armed.CompareAndSwap(true, false) {
				held <- struct{}{}
				<-letWriterGo
			}
		}
	}
	h.boot()
	letGoAtEnd(t, letWriterGo)
	l, err := h.reserveWithin("the reservation")
	h.must("the reservation", err)
	armed.Store(true)
	// A change the writer is handed, so it waits again: held there, it can
	// write no flush, and the shutdown cannot pass its closing phase.
	h.insert("a")
	select {
	case <-held:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the queue file's writer to wait", *watchdog)
	}
	h.cancel()
	h.waitSnap("the shutdown begun", func(sn *Snapshot) bool { return sn.Closed() })
	if _, err := h.reserveWithin("a reservation once the shutdown began"); ReasonOf(err) != ReasonClosed {
		t.Fatalf("a reservation once the shutdown began: %v, want %s", err, ReasonClosed)
	}
	h.releaseWithin("the release once the shutdown began", l)
	if sn := h.snap(); !sn.Closed() || sn.ConfigurationReserved() {
		t.Fatalf("the release once the shutdown began returned: closed %v, reserved %v; want closed and none held", sn.Closed(), sn.ConfigurationReserved())
	}
	letWriterGo.open()
	h.drive(h.ran)
	h.down = true
	if _, err := h.reserveWithin("a reservation after Run"); ReasonOf(err) != ReasonClosed {
		t.Fatalf("a reservation after Run: %v, want %s", err, ReasonClosed)
	}
	h.releaseWithin("a release after Run", l)
}
