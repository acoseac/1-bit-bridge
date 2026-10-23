package hotplug

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// idleBackend is a backend that plays nothing and counts the Starts: the
// engine's side of the reports is what these tests read.
type idleBackend struct {
	mu     sync.Mutex
	starts int
}

func (b *idleBackend) Start(context.Context, engine.StartRequest) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.starts++
	return errors.New("no device in the reporter's tests")
}

func (b *idleBackend) Prepare(context.Context, engine.SessionID, engine.OccurrenceID, *engine.Occurrence) error {
	return errors.New("no session")
}

func (b *idleBackend) Cut(context.Context, engine.SessionID, engine.OccurrenceID) (bool, error) {
	return false, errors.New("no session")
}
func (b *idleBackend) Pause(context.Context, engine.SessionID) error { return errors.New("no session") }
func (b *idleBackend) Resume(context.Context, engine.SessionID) (engine.ResumeResult, error) {
	return 0, errors.New("no session")
}
func (b *idleBackend) Release(context.Context, engine.SessionID) error { return nil }
func (b *idleBackend) Discard(context.Context) error                   { return nil }

func (b *idleBackend) started() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.starts
}

// reporterEngine is an engine the reporter feeds, over an idle backend,
// with one entry queued. It stops when the test ends.
func reporterEngine(t *testing.T) (*engine.Engine, *idleBackend) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	be := &idleBackend{}
	data := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{Backend: be, QueuePath: filepath.Join(data, "queue.json"), DataDir: data})
	if err != nil {
		t.Skipf("the engine does not start on this test's data directory: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() {
		defer close(ran)
		_ = eng.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ran:
		case <-time.After(watchdog):
			t.Errorf("the engine did not stop within %v", watchdog)
		}
	})
	ref, err := engine.NewReference("entry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.InsertAfter(ctx, 0, ref, nil); err != nil {
		t.Fatal(err)
	}
	return eng, be
}

// follow runs rep on sub into eng until the test ends.
func follow(t *testing.T, rep *Reporter, sub *Subscription, eng *engine.Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rep.Run(ctx, sub, eng.ReportOutput)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// output waits until the engine's output satisfies cond.
func output(t *testing.T, eng *engine.Engine, what string, cond func(engine.OutputState) bool) engine.OutputState {
	t.Helper()
	timer := time.NewTimer(watchdog)
	defer timer.Stop()
	for {
		ch := eng.Changed()
		if o := eng.Snapshot().Output(); cond(o) {
			return o
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatalf("waited %v for %s: the output is %+v", watchdog, what, eng.Snapshot().Output())
		}
	}
}

// TestTheReporterIsUnknownUntilTheFirstSnapshot pins the input's start: the
// reporter sends nothing until the watcher has published, so the engine
// holds the output unknown, and refuses Play, until then; and the first
// report is the first snapshot's, whatever it says: here the binding names a
// unit the box does not have, so it is device_missing at generation 1, never
// an availability no reading established.
func TestTheReporterIsUnknownUntilTheFirstSnapshot(t *testing.T) {
	h := newHarness(t, func(o *options) { o.enabled = false })
	eng, _ := reporterEngine(t)
	follow(t, &Reporter{}, h.svc.Feed().Subscribe(), eng)
	if err := eng.Play(context.Background()); engine.ReasonOf(err) != engine.ReasonOutputUnknown {
		t.Fatalf("Play before any snapshot: %v, want %s", err, engine.ReasonOutputUnknown)
	}
	other := bySerial()
	other.Serial = usbid.New("OTHER-UNIT-SERIAL-0099")
	h.settings.set(Settings{Enabled: true, Device: other})
	o := output(t, eng, "the first report", func(o engine.OutputState) bool { return o.Known })
	if o.Available || o.Reason != string(binding.StateDeviceMissing) || o.Generation != 1 {
		t.Errorf("the first report: %+v; want the first snapshot's, device_missing at generation 1", o)
	}
}

// TestTheReporterIsMonotonicAcrossAReplacedWatcher pins the generation: a
// watcher replaced by another, whose feed numbers its generations from 1
// again, reaches the engine with a generation above every one before, so the
// engine takes it: the unit back after the first watcher saw it go is
// available, not frozen at the old watcher's device_missing.
func TestTheReporterIsMonotonicAcrossAReplacedWatcher(t *testing.T) {
	eng, _ := reporterEngine(t)
	rep := &Reporter{}
	first := newHarness(t)
	follow(t, rep, first.svc.Feed().Subscribe(), eng)
	output(t, eng, "the first watcher's first report", func(o engine.OutputState) bool { return o.Available && o.Generation == 1 })
	first.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	first.state(binding.StateDeviceMissing, -1, 2)
	output(t, eng, "the unplug", func(o engine.OutputState) bool { return !o.Available && o.Reason == string(binding.StateDeviceMissing) })
	first.stop()

	second := newHarness(t)
	if s := second.latest(); s.Generation() != 1 {
		t.Fatalf("the second watcher's first generation is %d", s.Generation())
	}
	follow(t, rep, second.svc.Feed().Subscribe(), eng)
	o := output(t, eng, "the second watcher's first report", func(o engine.OutputState) bool { return o.Generation == 3 })
	if !o.Available {
		t.Errorf("the second watcher's report: %+v; want the DAC available, taken over the first watcher's generations", o)
	}
}

// TestTheReporterFlagsAUnitThatChangedOrCameBack pins UnitChanged: the bound
// unit back after an unplug (a new enumeration), another unit swapped into a
// bound port, and the bound unit re-enumerated within one generation (a fast
// replug) each set it; an unplug, and a change elsewhere on the box, do not.
func TestTheReporterFlagsAUnitThatChangedOrCameBack(t *testing.T) {
	t.Run("unplugged and plugged in again", func(t *testing.T) {
		h := newHarness(t)
		rep := &Reporter{}
		if r, ok := rep.Report(h.latest()); !ok || !r.Available || r.UnitChanged {
			t.Fatalf("the first report %+v %v", r, ok)
		}
		h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
		if r, ok := rep.Report(h.state(binding.StateDeviceMissing, -1, 2)); !ok || r.Available || r.UnitChanged || r.Reason != "device_missing" {
			t.Errorf("the unplug: %+v %v; want unavailable, device_missing, no unit change", r, ok)
		}
		h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 3), plugEvents(goBar, 5))
		if r, ok := rep.Report(h.state(binding.StateAvailable, 5, 3)); !ok || !r.Available || !r.UnitChanged {
			t.Errorf("the replug: %+v %v; want available, the unit back flagged", r, ok)
		}
	})
	t.Run("another unit in the bound port", func(t *testing.T) {
		h := newHarness(t, func(o *options) { o.device = byPort(goBarPort) })
		rep := &Reporter{}
		rep.Report(h.latest())
		h.step("unplug "+goBar+"\n"+plug(goBar, 5, "1234:0f03", "", 3), append(unplugEvents(goBar, 5), plugEvents(goBar, 5)...))
		if r, ok := rep.Report(h.state(binding.StateAvailable, 5, 2)); !ok || !r.Available || !r.UnitChanged {
			t.Errorf("the swap: %+v %v; want available, another unit flagged", r, ok)
		}
	})
	t.Run("re-enumerated in one generation", func(t *testing.T) {
		h := newHarness(t)
		rep := &Reporter{}
		rep.Report(h.latest())
		h.apply("unplug " + goBar + "\n" + plug(goBar, 5, "20b1:3009", goBarSerial, 3))
		for _, e := range plugEvents(goBar, 5) {
			h.send(e)
		}
		h.advance(quiet)
		h.reconciles(2)
		if r, ok := rep.Report(h.state(binding.StateAvailable, 5, 2)); !ok || !r.Available || !r.UnitChanged {
			t.Errorf("the fast replug: %+v %v; want available, re-enumerated flagged", r, ok)
		}
	})
	t.Run("another DAC elsewhere", func(t *testing.T) {
		h := newHarness(t)
		rep := &Reporter{}
		rep.Report(h.latest())
		other := "sys/devices/platform/axi/1000120000.pcie/1f00300000.usb/xhci-hcd.1/usb3/3-1"
		h.step(plug(other, 6, "1234:0f03", "", 2), plugEvents(other, 6))
		if r, ok := rep.Report(h.state(binding.StateAvailable, 5, 2)); !ok || !r.Available || r.UnitChanged {
			t.Errorf("another DAC on the box: %+v %v; want available, no unit change", r, ok)
		}
	})
}

// TestTheReporterSkipsARepublishedGeneration pins a snapshot republished at
// its generation because only its health changed (a reading failed: stale):
// no report, since it says nothing new about the unit.
func TestTheReporterSkipsARepublishedGeneration(t *testing.T) {
	h := newHarness(t)
	rep := &Reporter{}
	if _, ok := rep.Report(h.latest()); !ok {
		t.Fatal("the first snapshot made no report")
	}
	h.setObserve(func(context.Context, func(context.Context) (binding.Inventory, error)) (binding.Inventory, error) {
		return binding.Inventory{}, errors.New("sysfs went away")
	})
	h.advance(periodic)
	h.reconciles(2)
	h.published(2)
	s := h.latest()
	if _, stale := s.Stale(); !stale || s.Generation() != 1 {
		t.Fatalf("the snapshot after the failure: generation %d, stale %v", s.Generation(), stale)
	}
	if r, ok := rep.Report(s); ok {
		t.Errorf("a republished generation made a report: %+v", r)
	}
}

// TestTheReporterSendsNothingButReports pins the output's return: the bound
// DAC unplugged and plugged in again reaches the engine as two reports, and
// the engine, stopped with its queue, starts nothing: the return of the
// output plays nothing, and the reporter has no way to ask it to.
func TestTheReporterSendsNothingButReports(t *testing.T) {
	h := newHarness(t)
	eng, be := reporterEngine(t)
	follow(t, &Reporter{}, h.svc.Feed().Subscribe(), eng)
	output(t, eng, "available", func(o engine.OutputState) bool { return o.Available })
	h.step("unplug "+goBar+"\n", unplugEvents(goBar, 5))
	output(t, eng, "unavailable", func(o engine.OutputState) bool { return !o.Available })
	h.step(plug(goBar, 5, "20b1:3009", goBarSerial, 3), plugEvents(goBar, 5))
	o := output(t, eng, "available again", func(o engine.OutputState) bool { return o.Available && o.Generation == 3 })
	if s := eng.Snapshot(); s.State() != engine.Stopped || be.started() != 0 {
		t.Errorf("after the output came back (%+v): %s, %d starts; want stopped, none", o, s.State(), be.started())
	}
}
