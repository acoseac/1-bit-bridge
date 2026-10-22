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
	if st := h.svc.Status(); !st.Next.Equal(h.clock.Now().Add(periodic)) {
		t.Errorf("next reading %v after the overflow's", st.Next.Sub(h.clock.Now()))
	}
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
