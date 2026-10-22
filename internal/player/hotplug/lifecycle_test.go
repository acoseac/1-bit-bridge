package hotplug

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/binding"
)

// TestAChangedBindingForcesAReading: a binding changed in the live config
// while the watcher runs (another kind, another endpoint) is read at once,
// with no event, and advances the generation, saying the binding changed.
func TestAChangedBindingForcesAReading(t *testing.T) {
	h := newHarness(t)
	h.settings.set(Settings{Enabled: true, Device: byModel("20b1", "3009")})
	h.reconciles(2)
	s := h.state(binding.StateAvailable, 5, 2)
	if c := s.Change(); !c.Binding || c.State {
		t.Errorf("change %+v", c)
	}
	if s.Binding().Binding != "uniqueModel" {
		t.Errorf("the snapshot's binding: %+v", s.Binding())
	}
	dev := byModel("20b1", "3009")
	dev.PCMDevice = 1
	h.settings.set(Settings{Enabled: true, Device: dev})
	h.reconciles(3)
	h.state(binding.StateEndpointMissing, 5, 3)
	// The same binding again (a settings save that left the player alone)
	// reads nothing.
	h.settings.set(Settings{Enabled: true, Device: dev})
	h.waitStats("the save taken", func(s Stats) bool { return s.ConfigChanges >= 3 })
	if st := h.svc.Status(); st.Stats.Reconciles != 3 {
		t.Errorf("an unchanged binding was read: %+v", st.Stats)
	}
}

// TestEnablingThePlayerStartsTheWatcher: a serve whose player is off runs no
// watch and publishes nothing; turning the player on in the live config
// starts one, which reads at once and publishes generation 1.
func TestEnablingThePlayerStartsTheWatcher(t *testing.T) {
	h := newHarness(t, func(o *options) { o.enabled = false })
	if st := h.svc.Status(); st.Running || st.Stats.Sessions != 0 || st.Source.Mode != ModeOff {
		t.Fatalf("a disabled player's status: %+v", st)
	}
	if _, ok := h.svc.Feed().Latest(); ok {
		t.Fatal("a disabled player published")
	}
	h.settings.set(Settings{Enabled: true, Device: bySerial()})
	h.reconciles(1)
	h.state(binding.StateAvailable, 5, 1)
	if st := h.svc.Status(); !st.Running || st.Stats.Sessions != 1 {
		t.Errorf("status %+v", st)
	}
}

// TestADisableAndReEnableKeepsTheGenerationIncreasing: disabling the
// player stops the watch (its source is done, its goroutines joined) and
// publishes disabled; enabling it again starts another, and the
// generation, which belongs to the serve, goes on from where it was.
func TestADisableAndReEnableKeepsTheGenerationIncreasing(t *testing.T) {
	h := newHarness(t)
	first := h.source()
	h.settings.set(Settings{Enabled: false})
	h.published(2)
	s := h.state(binding.StateDisabled, -1, 2)
	if c := s.Change(); !c.State || c.Previous != binding.StateAvailable || len(s.Cards()) != 0 {
		t.Errorf("the disabled snapshot: %+v, %d cards", c, len(s.Cards()))
	}
	select {
	case <-first.Done():
	default:
		t.Error("the stopped watch's source is not done")
	}
	if st := h.svc.Status(); st.Running {
		t.Errorf("status %+v", st)
	}
	h.settings.set(Settings{Enabled: true, Device: bySerial()})
	h.published(3)
	again := h.state(binding.StateAvailable, 5, 3)
	if again.Instance() != s.Instance() || h.source() == first {
		t.Errorf("the instance changed (%s, %s), or no new watch started", again.Instance(), s.Instance())
	}
}

// TestShutdownJoinsTheWatcherWhileItWaitsForTheKernel: a watch idle in its
// source's wait and its own select stops as soon as serve's context ends,
// with no clock moved: no deadline is needed for a watch that is not stuck.
func TestShutdownJoinsTheWatcherWhileItWaitsForTheKernel(t *testing.T) {
	h := newHarness(t)
	src := h.source()
	h.cancel()
	select {
	case <-h.stopped:
	case <-time.After(watchdog):
		t.Fatal("Run did not return")
	}
	select {
	case <-src.Done():
	default:
		t.Error("the source is not done")
	}
}

// TestShutdownStopsAReconcileBetweenFiles: a reading in progress stops at
// the next card or file once serve's context ends, which the readers check,
// and Run returns without waiting out the deadline.
func TestShutdownStopsAReconcileBetweenFiles(t *testing.T) {
	h := newHarness(t)
	entered := make(chan struct{})
	var once sync.Once
	// The second reading waits inside, as a reading between two files does
	// until the context ends, and then returns the context's error, as the
	// readers do.
	h.setObserve(func(ctx context.Context, _ func(context.Context) (binding.Inventory, error)) (binding.Inventory, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return binding.Inventory{}, ctx.Err()
	})
	h.send(unplugEvents(goBar, 5)[0])
	h.advance(quiet)
	<-entered
	h.cancel()
	select {
	case <-h.stopped:
	case <-time.After(watchdog):
		t.Fatal("Run did not return")
	}
	if st := h.svc.Status(); st.Stats.Failures != 0 || st.Stats.Publishes != 1 {
		t.Errorf("a stopped reading counted as a failure or published: %+v", st.Stats)
	}
}

// TestShutdownAbandonsAReadStuckInTheKernel: a reading stuck inside one
// file, as a read of a USB serial is while a disconnect holds the device's
// lock, cannot be interrupted: serve's shutdown waits for it no longer
// than the deadline, says so, and returns; when the read comes back, that
// reading is discarded, never published.
func TestShutdownAbandonsAReadStuckInTheKernel(t *testing.T) {
	h := newHarness(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.setObserve(func(_ context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error) {
		once.Do(func() { close(entered) })
		<-release // the kernel holds the read; the context does not reach it
		return read(context.Background())
	})
	h.apply("unplug " + goBar + "\n")
	h.send(unplugEvents(goBar, 5)[0])
	h.advance(quiet)
	<-entered
	h.cancel()
	h.waitStatus("the stop waiting on the stuck read", func(s Status) bool { return s.Stopping })
	select {
	case <-h.stopped:
		t.Fatal("Run returned before the deadline with the read still stuck")
	default:
	}
	h.advance(DefaultStopWait)
	select {
	case <-h.stopped:
	case <-time.After(watchdog):
		t.Fatal("Run did not return at the deadline")
	}
	if !strings.Contains(h.logs.String(), "abandoned") {
		t.Errorf("the abandoned read was not logged:\n%s", h.logs.String())
	}
	close(release)
	h.waitStats("the abandoned reading discarded", func(s Stats) bool { return s.Discarded == 1 })
	if st := h.svc.Status(); st.Stats.Publishes != 1 {
		t.Errorf("the abandoned reading was published: %+v", st.Stats)
	}
	h.state(binding.StateAvailable, 5, 1)
}

// TestThePollingPathReadsOnTheClockAndSaysSo: a source that could not open
// (a container, seccomp) leaves the watcher on its periodic readings alone,
// which still find a change, and the snapshot and the status say so and why.
func TestThePollingPathReadsOnTheClockAndSaysSo(t *testing.T) {
	const why = "the netlink socket could not be opened: operation not permitted"
	h := newHarness(t, func(o *options) { o.mode, o.reason = ModePolling, why })
	h.apply("unplug " + goBar + "\n")
	h.advance(periodic)
	h.reconciles(2)
	s := h.state(binding.StateDeviceMissing, -1, 2)
	if src := s.Source(); src.Mode != ModePolling || src.Reason != why || src.String() != "polling only: "+why {
		t.Errorf("the snapshot's source: %+v", src)
	}
	if st := h.svc.Status(); st.Source.Mode != ModePolling {
		t.Errorf("status %+v", st.Source)
	}
}

// TestTheModeSaysNoEventsYetUntilTheKernelSpeaks: the snapshot carries the
// source's mode, and a change of mode alone (the kernel's first message)
// republishes it at the same generation.
func TestTheModeSaysNoEventsYetUntilTheKernelSpeaks(t *testing.T) {
	h := newHarness(t, func(o *options) { o.mode = ModeNetlinkQuiet })
	if m := h.state(binding.StateAvailable, 5, 1).Source().Mode; m != ModeNetlinkQuiet {
		t.Fatalf("mode %s", m)
	}
	h.source().setMode(ModeNetlink)
	h.published(2)
	if s := h.state(binding.StateAvailable, 5, 1); s.Source().Mode != ModeNetlink {
		t.Errorf("mode %s", s.Source().Mode)
	}
}

// TestAReadErrorKeepsTheLastSnapshotStale: a reading that fails (the root
// gone, a cap hit, which the readers mark partial) publishes no state: the
// last good snapshot stands, marked stale, at its generation. The next
// attempts back off, so a failing tree is not read in a loop, and an event
// does not hurry them; once a reading works the mark goes.
func TestAReadErrorKeepsTheLastSnapshotStale(t *testing.T) {
	var mu sync.Mutex
	fail := errors.New("read /proc/asound/cards: no such file or directory")
	failing := false
	h := newHarness(t, func(o *options) {
		o.observe = func(ctx context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error) {
			mu.Lock()
			defer mu.Unlock()
			if failing {
				return binding.Inventory{}, fail
			}
			return read(ctx)
		}
	})
	setFailing := func(f bool) {
		mu.Lock()
		failing = f
		mu.Unlock()
	}
	setFailing(true)
	h.apply("unplug " + goBar + "\n")
	h.send(unplugEvents(goBar, 5)[0])
	h.advance(quiet)
	h.reconciles(2)
	h.published(2)
	s := h.state(binding.StateAvailable, 5, 1)
	f, stale := s.Stale()
	if !stale || f.Err != fail.Error() || !f.Since.Equal(h.clock.Now()) {
		t.Fatalf("after a failed reading: stale %v, %+v", stale, f)
	}
	r2 := h.clock.Now()
	h.waitNext(r2.Add(DefaultBackoffMin))
	h.send(unplugEvents(goBar, 5)[1])
	h.waitNext(r2.Add(DefaultBackoffMin))
	h.advance(DefaultBackoffMin)
	h.reconciles(3)
	h.waitNext(r2.Add(DefaultBackoffMin).Add(2 * DefaultBackoffMin))
	if st := h.svc.Status(); st.Failure == nil || st.Stats.Failures != 2 || st.Stats.Publishes != 2 {
		t.Errorf("after two failures: %+v", st)
	}
	setFailing(false)
	h.advance(2 * DefaultBackoffMin)
	h.reconciles(4)
	s = h.state(binding.StateDeviceMissing, -1, 2)
	if _, stale := s.Stale(); stale {
		t.Error("the snapshot is still stale once a reading worked")
	}
	if st := h.svc.Status(); st.Failure != nil {
		t.Errorf("status %+v", st)
	}
}

// TestAPartialReadingIsAFailure: a sysfs reading a cap cut, or one whose
// root could not be opened, says it is partial, and the watcher takes it
// for a failed reading: never for a box whose cards went.
func TestAPartialReadingIsAFailure(t *testing.T) {
	h := newHarness(t, func(o *options) {
		o.observe = func(ctx context.Context, read func(context.Context) (binding.Inventory, error)) (binding.Inventory, error) {
			inv, err := read(ctx)
			inv.Partial = true
			inv.Cards = nil
			return inv, err
		}
	})
	if _, ok := h.svc.Feed().Latest(); ok {
		t.Fatal("a partial first reading was published")
	}
	if st := h.svc.Status(); st.Failure == nil || !strings.Contains(st.Failure.Err, "partial") {
		t.Errorf("status %+v", st)
	}
}
