package hotplug

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// TestAConsumerThatNeverReadsGetsTheNewest: a subscription nobody reads
// holds one snapshot, the newest: the watcher goes on reading and
// publishing past it, and when the consumer reads at last it gets the
// latest generation, once. A publish that waited on the consumer would
// stop the watcher, and this test fails at its watchdog: nothing here
// takes the feed's lock on the way out (the mailbox is closed only once
// the test has passed), so the binary does not hang with it.
func TestAConsumerThatNeverReadsGetsTheNewest(t *testing.T) {
	h := newHarness(t)
	idle := h.svc.Feed().Subscribe()
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 3), plugEvents(goBar, 5))
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	h.advance(periodic)
	h.reconciles(5)
	h.state(binding.StateDeviceMissing, -1, 4)
	select {
	case s := <-idle.C():
		if s.Generation() != 4 {
			t.Errorf("the idle consumer got generation %d, want the newest, 4", s.Generation())
		}
	case <-time.After(watchdog):
		t.Fatal("the idle consumer has nothing")
	}
	select {
	case s := <-idle.C():
		t.Errorf("a second snapshot was waiting: generation %d", s.Generation())
	default:
	}
	idle.Close()
}

// TestASnapshotIsTheSameForEveryConsumer: what one consumer does with what
// it was given (its cards, their endpoints and identities, the resolution
// and its matches) changes nothing another consumer reads, run side by
// side under the race detector.
func TestASnapshotIsTheSameForEveryConsumer(t *testing.T) {
	h := newHarness(t, func(o *options) { o.device = byModel("20b1", "3009") })
	a, b := h.svc.Feed().Subscribe(), h.svc.Feed().Subscribe()
	defer a.Close()
	defer b.Close()
	h.step(plug(usb1+"/1-2", 6, "20b1:3009", "", 3), plugEvents(usb1+"/1-2", 6))
	h.state(binding.StateDeviceAmbiguous, -1, 2)
	sa, sb := <-a.C(), <-b.C()
	before := describe(sb)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			cards := sa.Cards()
			for j := range cards {
				cards[j].Index = 99
				cards[j].Endpoints = append(cards[j].Endpoints[:0], binding.Endpoint{Device: 7})
				if cards[j].USB != nil {
					cards[j].USB.Port = "elsewhere"
					cards[j].USB.DevNum = 99
				}
			}
			r := sa.Resolution()
			r.Matches[0] = 42
			r.Identity.Port = "elsewhere"
			d := sa.Binding()
			d.VendorID = "ffff"
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = describe(sb)
		}
	}()
	wg.Wait()
	if after := describe(sb); after != before {
		t.Errorf("another consumer's snapshot changed:\n%s\n%s", before, after)
	}
	if again := describe(sa); again != before {
		t.Errorf("the snapshot itself changed:\n%s\n%s", before, again)
	}
}

// TestEachConsumerGetsItsOwnCopyOfTheBoundPort: under a port binding a
// snapshot's resolution carries the port the binding names, a usbid.Port
// whose anchor segments and port chain are slices, and what one consumer
// does to its copy changes nothing the snapshot holds for the next. Round
// 4: the port-binding fix added the field, and the accessor, which cloned
// the matches alone, handed every consumer the snapshot's own slices.
func TestEachConsumerGetsItsOwnCopyOfTheBoundPort(t *testing.T) {
	h := newHarness(t, func(o *options) { o.device = byPort(goBarPort) })
	s := h.state(binding.StateAvailable, 5, 1)
	before := s.Resolution().Port.String()
	if before != goBarPort {
		t.Fatalf("premise: the snapshot's resolution names the port %q", before)
	}
	mine := s.Resolution()
	mine.Port.Segments[0] = "pci-0000:00:00.0"
	mine.Port.Chain[0] = 9
	if after := s.Resolution().Port.String(); after != before {
		t.Errorf("one consumer's copy of the port reached the snapshot: %q, was %q", after, before)
	}
	if latest, _ := h.svc.Feed().Latest(); latest.Resolution().Port.String() != before {
		t.Errorf("the feed's newest snapshot names %q, was %q", latest.Resolution().Port.String(), before)
	}
}

// describe renders what a snapshot holds, to compare.
func describe(s Snapshot) string {
	var b strings.Builder
	r := s.Resolution()
	b.WriteString(string(r.State) + " " + r.Identity.Port)
	for _, m := range r.Matches {
		b.WriteString(" m" + string(rune('0'+m)))
	}
	b.WriteString(" " + s.Binding().VendorID)
	for _, c := range s.Cards() {
		b.WriteString(" | " + string(rune('0'+c.Index)) + " " + string(c.State))
		for _, e := range c.Endpoints {
			b.WriteString(" " + e.String())
		}
		if c.USB != nil {
			b.WriteString(" " + c.USB.Port + " " + string(rune('0'+c.USB.DevNum)))
		}
	}
	return b.String()
}

// TestEachStateChangeIsLoggedOnceWithoutASerial: every change of the
// binding's state is one INFO line naming the binding by its label, the
// card and the unit by 4a's description, and every other generation (a
// second DAC plugged in) one INFO line listing the cards by their models;
// nothing the watcher logs carries a serial.
func TestEachStateChangeIsLoggedOnceWithoutASerial(t *testing.T) {
	h := newHarness(t)
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 3), plugEvents(goBar, 5))
	h.step(plug(usb1+"/1-2", 6, "20b1:3009", "OTHER-UNIT-SERIAL-0099", 4), plugEvents(usb1+"/1-2", 6))
	h.advance(periodic)
	h.reconciles(5)
	logs := h.logs.String()
	for _, want := range []string{"player DAC: available", "player DAC: device_missing"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no %q line:\n%s", want, logs)
		}
	}
	if n := countLines(logs, "player DAC: "); n != 3 {
		t.Errorf("%d state lines, want 3:\n%s", n, logs)
	}
	if !strings.Contains(logs, "serial 20b1:3009 (…4471)") || !strings.Contains(logs, "level=INFO") {
		t.Errorf("the binding's label is not in the lines:\n%s", logs)
	}
	if n := countLines(logs, `level=INFO msg="player hotplug: the inventory changed" generation=4`); n != 1 ||
		!strings.Contains(logs, "5 usb 20b1:3009, 6 usb 20b1:3009") {
		t.Errorf("the second DAC's generation is not one INFO line listing the cards:\n%s", logs)
	}
	for _, serial := range []string{goBarSerial, "OTHER-UNIT-SERIAL-0099"} {
		for i := 0; i+6 <= len(serial); i++ {
			if strings.Contains(logs, serial[i:i+6]) {
				t.Fatalf("the log carries a serial:\n%s", logs)
			}
		}
	}
}

// TestSubscribersCanLeave: a closed subscription receives nothing more.
func TestSubscribersCanLeave(t *testing.T) {
	h := newHarness(t)
	s := h.svc.Feed().Subscribe()
	s.Close()
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	h.state(binding.StateDeviceMissing, -1, 2)
	select {
	case got := <-s.C():
		t.Errorf("a closed subscription received generation %d", got.Generation())
	default:
	}
}
