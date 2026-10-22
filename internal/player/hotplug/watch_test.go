package hotplug

import (
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// The default timing, as the tests step it.
const (
	quiet    = DefaultQuiet
	maxDelay = DefaultMaxDelay
	periodic = DefaultPeriodic
	followUp = DefaultFollowUpEvery
)

// waitNext waits until the watcher says it reads the box next at due: the
// state it reaches once it has taken every input a test gave it.
func (h *harness) waitNext(due time.Time) Status {
	h.t.Helper()
	return h.waitStatus("the next reading at "+due.Format("15:04:05.000"), func(s Status) bool { return s.Next.Equal(due) })
}

// TestTheFirstReadingIsGenerationOne: the watcher reads the box once at
// start, at once, and publishes generation 1 with this serve's instance,
// the reading's time, the cards, the binding's resolution and the source's
// mode; the next reading is the periodic one.
func TestTheFirstReadingIsGenerationOne(t *testing.T) {
	h := newHarness(t)
	t0 := h.clock.Now()
	s := h.state(binding.StateAvailable, 5, 1)
	if s.Instance() == "" || s.Instance() != h.svc.Feed().Instance() || !s.Observed().Equal(t0) || len(s.Cards()) != 3 ||
		s.Source().Mode != ModeNetlink || !s.Change().First {
		t.Errorf("the first snapshot: instance %q, observed %v, %d cards, source %s, change %+v",
			s.Instance(), s.Observed(), len(s.Cards()), s.Source(), s.Change())
	}
	if _, stale := s.Stale(); stale {
		t.Error("the first snapshot is stale")
	}
	h.waitNext(t0.Add(periodic))
	if other := NewFeed().Instance(); other == s.Instance() || len(other) < 32 {
		t.Errorf("two feeds' instances: %q and %q", other, s.Instance())
	}
}

// TestABurstOfEventsIsOneReconcile: twenty events 50 ms apart, the burst a
// plug or an unplug makes, are read once, 300 ms after the last, and
// published once.
func TestABurstOfEventsIsOneReconcile(t *testing.T) {
	h := newHarness(t)
	h.apply("unplug " + goBar + "\n")
	var burst []Uevent
	for len(burst) < 20 {
		burst = append(burst, unplugEvents(goBar, 5)...)
	}
	start := h.clock.Now()
	for _, e := range burst[:20] {
		h.send(e)
		h.advance(50 * time.Millisecond)
	}
	last := start.Add(19 * 50 * time.Millisecond)
	if st := h.waitNext(last.Add(quiet)); st.Stats.Reconciles != 1 {
		t.Fatalf("the burst was read %d times before it settled", st.Stats.Reconciles-1)
	}
	h.advance(last.Add(quiet).Sub(h.clock.Now()))
	h.reconciles(2)
	h.state(binding.StateDeviceMissing, -1, 2)
	if st := h.waitNext(last.Add(quiet).Add(periodic)); st.Stats.Reconciles != 2 || st.Stats.Publishes != 2 {
		t.Errorf("after the burst: %+v", st.Stats)
	}
}

// TestTheQuietWindowHolds: a reading waits until the events have been
// quiet for 300 ms: not at 299 ms, at 300.
func TestTheQuietWindowHolds(t *testing.T) {
	h := newHarness(t)
	h.apply("unplug " + goBar + "\n")
	at := h.clock.Now()
	h.send(unplugEvents(goBar, 5)[0])
	h.waitNext(at.Add(quiet))
	h.advance(quiet - time.Millisecond)
	if st := h.waitNext(at.Add(quiet)); st.Stats.Reconciles != 1 {
		t.Fatalf("read before the window was quiet: %+v", st.Stats)
	}
	h.advance(time.Millisecond)
	h.reconciles(2)
	h.state(binding.StateDeviceMissing, -1, 2)
}

// TestTheMaxDelayHolds: events that never stop for 300 ms are read 2 s
// after the first, while they go on, and the next event opens a window of
// its own.
func TestTheMaxDelayHolds(t *testing.T) {
	h := newHarness(t)
	h.apply("unplug " + goBar + "\n")
	first := h.clock.Now()
	for i := 0; i < 10; i++ {
		h.send(Uevent{Action: "change", DevPath: "/devices/platform/axi/x/usb1", Subsystem: "usb"})
		at := first.Add(time.Duration(i) * 200 * time.Millisecond)
		due := at.Add(quiet)
		if cap := first.Add(maxDelay); due.After(cap) {
			due = cap
		}
		h.waitNext(due)
		h.advance(200 * time.Millisecond)
	}
	h.reconciles(2)
	if now := h.clock.Now(); !now.Equal(first.Add(maxDelay)) {
		t.Fatalf("the clock is at %v", now.Sub(first))
	}
	h.state(binding.StateDeviceMissing, -1, 2)
	h.send(Uevent{Action: "change", DevPath: "/devices/platform/axi/x/usb1", Subsystem: "usb"})
	h.waitNext(first.Add(maxDelay).Add(quiet))
}

// TestATickAndAnOverflowMidBurstFoldIntoItsReading: a periodic tick and a
// kernel overflow that land while a burst's window is open are folded into
// that window's reading, the one reading and the one publish after it.
func TestATickAndAnOverflowMidBurstFoldIntoItsReading(t *testing.T) {
	h := newHarness(t)
	t0 := h.clock.Now()
	h.advance(periodic - 100*time.Millisecond)
	h.apply("unplug " + goBar + "\n")
	h.send(unplugEvents(goBar, 5)[:2]...)
	open := t0.Add(periodic - 100*time.Millisecond)
	h.waitNext(open.Add(quiet))
	h.advance(100 * time.Millisecond) // the periodic tick is due now, inside the window
	h.source().overflow()
	h.waitStats("the overflow taken", func(s Stats) bool { return s.Lost == 1 })
	if st := h.waitNext(open.Add(quiet)); st.Stats.Reconciles != 1 {
		t.Fatalf("the tick or the overflow was read on its own: %+v", st.Stats)
	}
	h.advance(open.Add(quiet).Sub(h.clock.Now()))
	h.reconciles(2)
	h.state(binding.StateDeviceMissing, -1, 2)
	if st := h.waitNext(open.Add(quiet).Add(periodic)); st.Stats.Reconciles != 2 || st.Stats.Publishes != 2 {
		t.Errorf("after the window: %+v", st.Stats)
	}
	if n := countLines(h.logs.String(), "dropped"); n != 1 {
		t.Errorf("the overflow was logged %d times:\n%s", n, h.logs.String())
	}
}

// TestAnOverflowWithNoWindowReadsAtOnce: an overflow with no burst going
// on is read at once, with no window: the events it lost may be the only
// ones there were.
func TestAnOverflowWithNoWindowReadsAtOnce(t *testing.T) {
	h := newHarness(t)
	h.advance(time.Second)
	h.apply("unplug " + goBar + "\n")
	h.source().overflow()
	h.reconciles(2)
	h.state(binding.StateDeviceMissing, -1, 2)
	// The watcher counts a reading as it ends and sets its next deadline
	// only after, so the deadline is waited for, never read beside the
	// count (round 3: read at once, it was the overflow's own, one run in
	// a hundred under -race -cpu 1,2,8).
	h.waitNext(h.clock.Now().Add(periodic))
}

// TestThePeriodicReadingCatchesAChangeNoEventAnnounced: with no event at
// all, the periodic reading, every 10 s, still finds a DAC that went.
func TestThePeriodicReadingCatchesAChangeNoEventAnnounced(t *testing.T) {
	h := newHarness(t)
	h.apply("unplug " + goBar + "\n")
	h.advance(periodic - time.Millisecond)
	if st := h.waitNext(h.clock.Now().Add(time.Millisecond)); st.Stats.Reconciles != 1 {
		t.Fatalf("read before the period: %+v", st.Stats)
	}
	h.advance(time.Millisecond)
	h.reconciles(2)
	h.state(binding.StateDeviceMissing, -1, 2)
}

// TestThePeriodicReadingIsDueAPeriodAfterTheLastReadingEnds: the periodic
// reading comes a period after the last reading ended. Until round 3 the
// period ran from the reading's start, so one that took longer than a
// period, as a read stuck on a USB device's lock does, left the next due at
// once, another reading that may block as it did.
func TestThePeriodicReadingIsDueAPeriodAfterTheLastReadingEnds(t *testing.T) {
	h := newHarness(t)
	p := h.parkNextReading()
	h.advance(periodic)
	p.wait(t)
	h.advance(periodic + time.Second)
	ended := h.clock.Now()
	p.release()
	st := h.waitStatus("the reading taken and the watcher waiting", func(s Status) bool {
		return s.Stats.Reconciles >= 2 && s.Next.After(h.clock.Now())
	})
	if st.Stats.Reconciles != 2 || !st.Next.Equal(ended.Add(periodic)) {
		t.Errorf("after a reading that took %v: %+v, the next due %v after it ended; want no other reading yet and the next %v after",
			periodic+time.Second, st.Stats, st.Next.Sub(ended), periodic)
	}
}

// TestAnUnchangedInventoryLeavesTheGenerationAlone: events that change
// nothing the snapshot holds (another device's, a keyboard's) are read,
// and nothing is published.
func TestAnUnchangedInventoryLeavesTheGenerationAlone(t *testing.T) {
	h := newHarness(t)
	h.apply("usbdev " + usb1 + "/1-4 vendor=046d product=c31c devnum=7\n")
	h.send(Uevent{Action: "add", DevPath: devpath(usb1 + "/1-4"), Subsystem: "usb", DevType: "usb_device"})
	h.advance(quiet)
	h.reconciles(2)
	h.advance(periodic)
	st := h.reconciles(3)
	if st.Publishes != 1 {
		t.Errorf("an unchanged inventory was published: %+v", st)
	}
	h.state(binding.StateAvailable, 5, 1)
}

// TestAReplugThatQueuedDuringAReadingIsOneGeneration: a fast replug of the
// bound unit that finishes while a reading runs, the kernel's events
// queued behind the reading, is one generation, the bound unit
// re-enumerated. The reading takes those events before it publishes, and
// as a removal among them reaches a card it is set aside for the window
// they open. Until round 2 it published, its queued removals were taken
// after it, and the window's reading marked the unchanged replacement
// re-enumerated again at a second generation, which would stop a play 4b
// started in between. The source never waits for the watcher, so on a box
// the kernel's events queue behind whatever reading they come during.
func TestAReplugThatQueuedDuringAReadingIsOneGeneration(t *testing.T) {
	h := newHarness(t)
	p := h.parkNextReading()
	h.advance(periodic)
	p.wait(t)
	h.apply("unplug " + goBar + "\n" + plug(goBar, 5, "20b1:3009", goBarSerial, 3))
	h.queue(append(unplugEvents(goBar, 5), plugEvents(goBar, 5)...)...)
	p.release()
	h.reconciles(2)
	h.waitStats("the queued events taken", func(s Stats) bool { return s.Events == h.events })
	h.advance(quiet)
	h.reconciles(3)
	s := h.state(binding.StateAvailable, 5, 2)
	if c := s.Change(); !c.ReEnumerated || c.State || c.Unit || c.Swap {
		t.Errorf("change %+v; want the bound unit re-enumerated and nothing else", c)
	}
	h.advance(periodic)
	if st := h.reconciles(4); st.Publishes != 2 {
		t.Errorf("one replug was published %d times: %+v", st.Publishes-1, st)
	}
	h.state(binding.StateAvailable, 5, 2)
}

// TestAnUnplugThatQueuedDuringAReadingIsOneGeneration: removals that queue
// behind a reading which still shows the bound unit as it was are not that
// reading's: it is set aside, and the window's reading finds the unit
// gone, one generation, device_missing, never a re-enumeration of a unit
// the reading showed unchanged, which taking the queued removals into the
// reading's own publish would claim.
func TestAnUnplugThatQueuedDuringAReadingIsOneGeneration(t *testing.T) {
	h := newHarness(t)
	p := h.parkNextReading()
	h.advance(periodic)
	p.wait(t)
	h.queue(unplugEvents(goBar, 5)...)
	p.release()
	h.reconciles(2)
	h.waitStats("the queued events taken", func(s Stats) bool { return s.Events == h.events })
	h.apply("unplug " + goBar + "\n")
	h.advance(quiet)
	h.reconciles(3)
	s := h.state(binding.StateDeviceMissing, -1, 2)
	if c := s.Change(); c.ReEnumerated || !c.State {
		t.Errorf("change %+v; want the state alone", c)
	}
	if st := h.svc.Status(); st.Stats.Publishes != 2 {
		t.Errorf("one unplug was published %d times: %+v", st.Stats.Publishes-1, st.Stats)
	}
}

// TestAReadingIsSetAsideOnceInARow: a reading that removals reaching a card
// queued behind is set aside once; the window's reading is published
// whatever queued behind it, the removals in it, so events that keep
// coming during readings cannot hold a change back.
func TestAReadingIsSetAsideOnceInARow(t *testing.T) {
	h := newHarness(t)
	first := h.parkNextReading()
	h.advance(periodic)
	first.wait(t)
	h.apply("unplug " + goBar + "\n" + plug(goBar, 5, "20b1:3009", goBarSerial, 3))
	h.queue(append(unplugEvents(goBar, 5), plugEvents(goBar, 5)...)...)
	second := h.parkNextReading()
	first.release()
	h.reconciles(2)
	h.waitStats("the queued events taken", func(s Stats) bool { return s.Events == h.events })
	h.advance(quiet)
	second.wait(t)
	h.queue(unplugEvents(goBar, 5)[1])
	second.release()
	h.reconciles(3)
	s := h.state(binding.StateAvailable, 5, 2)
	if c := s.Change(); !c.ReEnumerated {
		t.Errorf("change %+v; want the bound unit re-enumerated", c)
	}
	h.advance(quiet)
	if st := h.reconciles(4); st.Publishes != 2 {
		t.Errorf("the removal taken into the published reading was laid on the next: %+v", st)
	}
}

// TestAReadingAtAHeldAnswersBoundIsNeverSetAside: the reading a held
// answer's bound brings is published whatever queued behind it, a removal
// reaching a card included, so setting a reading aside never moves the
// hold's bound.
func TestAReadingAtAHeldAnswersBoundIsNeverSetAside(t *testing.T) {
	h := newHarness(t)
	h.apply("remove sys/class/sound/card5\nremove sys/bus/usb/devices/1-1\nremove sys/bus/usb/devices/1-1:1.0\nremove " + goBar + "\n")
	storm := Uevent{Action: "change", DevPath: devpath(usb1) + "/1-4", Subsystem: "usb"}
	const every = 400 * time.Millisecond
	held := h.clock.Now().Add(quiet)
	deadline := held.Add(maxDelay + DefaultFollowUps*followUp)
	reads := uint64(1)
	at := h.clock.Now()
	for ; at.Add(quiet).Before(deadline); at = at.Add(every) {
		h.advance(at.Sub(h.clock.Now()))
		h.send(storm)
		h.advance(quiet)
		reads++
		h.reconciles(reads)
	}
	// The step whose window holds the bound: the reading the bound brings
	// parks, and a removal reaching card 1 queues behind it.
	h.advance(at.Sub(h.clock.Now()))
	h.send(storm)
	p := h.parkNextReading()
	h.advance(deadline.Sub(h.clock.Now()))
	p.wait(t)
	h.queue(Uevent{Action: "remove", DevPath: "/devices/platform/axi/107c706400.hdmi/sound/card1", Subsystem: "sound"})
	p.release()
	reads++
	h.reconciles(reads)
	if s := h.state(binding.StateIdentityTransient, -1, 2); !s.Observed().Equal(deadline) {
		t.Errorf("published %v after the first held reading; want at its bound, %v", s.Observed().Sub(held), deadline.Sub(held))
	}
}

// TestAFastReplugOfTheBoundUnitIsAChange: the GO bar unplugged and plugged
// back into its port within one window reads back the same unit at the
// same card, yet a node opened before is dead: a new devnum, or a removal
// the watcher saw even when the devnum came back the same, is a change,
// published as the bound unit re-enumerated, the binding still available.
func TestAFastReplugOfTheBoundUnitIsAChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		// devnum is the replugged unit's; seen says the watcher hears the
		// removal, where otherwise the kernel's removes were lost and only
		// the adds came.
		devnum int
		seen   bool
	}{
		{"a new devnum, its removal lost", 3, false},
		{"its removal seen, the devnum the same", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			start := h.clock.Now()
			if tc.seen {
				for _, e := range unplugEvents(goBar, 5) {
					h.send(e)
					h.advance(10 * time.Millisecond)
				}
			}
			h.apply("unplug " + goBar + "\n" + plug(goBar, 5, "20b1:3009", goBarSerial, tc.devnum))
			for _, e := range plugEvents(goBar, 5) {
				h.send(e)
				h.advance(10 * time.Millisecond)
			}
			if d := h.clock.Now().Sub(start); d >= quiet {
				t.Fatalf("the replug took %v, not inside one window", d)
			}
			h.advance(quiet)
			h.reconciles(2)
			s := h.state(binding.StateAvailable, 5, 2)
			c := s.Change()
			if !c.ReEnumerated || c.State || c.Unit || c.Swap || !c.Inventory {
				t.Errorf("change %+v; want the bound unit re-enumerated and nothing else", c)
			}
			for _, card := range s.Cards() {
				if card.ReEnumerated != (card.Index == 5) {
					t.Errorf("card %d re-enumerated: %v", card.Index, card.ReEnumerated)
				}
			}
		})
	}
}
