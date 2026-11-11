package alsapcm_test

import (
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"slices"
	"sync"
	"testing"
	"time"
)

type gate struct {
	in, out     chan struct{}
	held, freed sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{in: make(chan struct{}), out: make(chan struct{})}
	t.Cleanup(g.letGo)
	return g
}

func (g *gate) pass() {
	g.held.Do(func() { close(g.in) })
	<-g.out
}

func (g *gate) letGo() { g.freed.Do(func() { close(g.out) }) }

func reachedGate(t *testing.T, g *gate, res <-chan struct{}, what string) {
	t.Helper()
	reachedGateWithin(t, alsapcm.Watchdog(), g, res, what)
}

func reachedGateWithin(t *testing.T, bound time.Duration, g *gate, res <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-g.in:
	case <-res:
		t.Fatalf("the refresh ended before %s", what)
	case <-time.After(bound):
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", bound, what)
	}
}

type lifecycleSeams struct {
	mu         sync.Mutex
	changed    chan struct{}
	opened     map[string]*gate
	activation map[engine.OccurrenceID]*gate
	unjudged   map[engine.OccurrenceID]*gate
	releasing  *gate
	verdicts   []engine.OccurrenceID
	activated  []engine.OccurrenceID
}

func newLifecycleSeams() *lifecycleSeams {
	return &lifecycleSeams{changed: make(chan struct{}), opened: map[string]*gate{},
		activation: map[engine.OccurrenceID]*gate{}, unjudged: map[engine.OccurrenceID]*gate{}}
}

func (w *lifecycleSeams) note(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *lifecycleSeams) ch() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changed
}

func (w *lifecycleSeams) seams(base *playback.Seams) *playback.Seams {
	s := &playback.Seams{}
	if base != nil {
		*s = *base
	}
	s.Opened = func(_ engine.OccurrenceID, ref engine.Reference) {
		w.mu.Lock()
		g := w.opened[ref.String()]
		delete(w.opened, ref.String())
		w.mu.Unlock()
		if g != nil {
			g.pass()
		}
	}
	s.Releasing = func(engine.SessionID) {
		w.mu.Lock()
		g := w.releasing
		w.releasing = nil
		w.mu.Unlock()
		if g != nil {
			g.pass()
		}
	}
	s.Unjudged = func(o engine.OccurrenceID) {
		w.note(func() { w.verdicts = append(w.verdicts, o) })
		w.mu.Lock()
		g := w.unjudged[o]
		delete(w.unjudged, o)
		w.mu.Unlock()
		if g != nil {
			g.pass()
		}
	}
	s.Activation = func(o engine.OccurrenceID) {
		w.note(func() { w.activated = append(w.activated, o) })
		w.mu.Lock()
		g := w.activation[o]
		delete(w.activation, o)
		w.mu.Unlock()
		if g != nil {
			g.pass()
		}
	}
	return s
}

func (w *lifecycleSeams) holdActivation(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.activation[o] = g
	w.mu.Unlock()
	return g
}

func (w *lifecycleSeams) holdOpened(t *testing.T, ref string) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.opened[ref] = g
	w.mu.Unlock()
	return g
}

func (w *lifecycleSeams) holdReleasing(t *testing.T) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.releasing = g
	w.mu.Unlock()
	return g
}

func (w *lifecycleSeams) activatedNow(o engine.OccurrenceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.activated, o)
}

func (w *lifecycleSeams) holdUnjudged(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.unjudged[o] = g
	w.mu.Unlock()
	return g
}

func (w *lifecycleSeams) verdictWaits(o engine.OccurrenceID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, x := range w.verdicts {
		if x == o {
			n++
		}
	}
	return n
}

func (w *lifecycleSeams) waitVerdictWaits(t *testing.T, o engine.OccurrenceID, n int, what string) {
	t.Helper()
	waitOrdered(t, fmt.Sprintf("%s (occurrence %d's wait for a verdict, %d)", what, o, n), w.ch, func() bool { return w.verdictWaits(o) >= n })
}

func waitOrderedEither(t *testing.T, what string, a, b func() <-chan struct{}, cond func() bool) {
	t.Helper()
	bound := alsapcm.Ordered()
	timer := time.NewTimer(bound)
	defer timer.Stop()
	for {
		ca, cb := a(), b()
		if cond() {
			return
		}
		select {
		case <-ca:
		case <-cb:
		case <-timer.C:
			alsapcm.LogGoroutines(t)
			t.Fatalf("waited %v for %s", bound, what)
		}
	}
}

func waitOnEither(t *testing.T, what string, a, b func() <-chan struct{}, cond func() bool) {
	t.Helper()
	deadline := time.After(alsapcm.Watchdog())
	for {
		ca, cb := a(), b()
		if cond() {
			return
		}
		select {
		case <-ca:
		case <-cb:
		case <-deadline:
			alsapcm.LogGoroutines(t)
			t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
		}
	}
}

// initialPolicy admits the fixture's transports before any playback begins.
func initialPolicy(mode string) dsdstate.State {
	return dsdstate.State{DefaultMode: mode, DACs: []dsdstate.DAC{{
		Key:          playbacktest.DACKey(),
		DoPConfirmed: &dsdstate.DoPConfirmation{BCDDevice: playbacktest.DACBCD, ConfirmedAt: playbacktest.T0, SetBy: dsdstate.ByApp},
		FirstUse: []dsdstate.FirstUse{
			{Transport: dsdstate.TransportNative, BCDDevice: playbacktest.DACBCD, AcknowledgedAt: playbacktest.T0},
			{Transport: dsdstate.TransportDoP, BCDDevice: playbacktest.DACBCD, AcknowledgedAt: playbacktest.T0},
		},
	}}}
}

// refreshAfterHotplug replaces the device's advertised capabilities, then
// runs the real policy refresh and reconciliation on its own goroutine.
func (d *dsdRig) refreshAfterHotplug(all bool) <-chan struct{} {
	if all {
		d.box.SetAltsets(card, 0)
	} else {
		a := playbacktest.NativeAltset()
		a.Rates = []int{88200}
		d.box.SetAltsets(card, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), a)
	}
	done := make(chan struct{})
	go func() { defer close(done); d.be.RefreshDSD(true) }()
	return done
}

func refreshed(t *testing.T, done <-chan struct{}) {
	t.Helper()
	awaitOrdered(t, done, "the hotplug refresh")
}
