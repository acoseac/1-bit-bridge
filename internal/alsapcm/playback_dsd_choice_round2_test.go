package alsapcm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A write of the bound DAC's DSD choice beside playback (B728 B9 round 2),
// through the engine's Run, the backend and the real sinks over the fake
// kernel: an open beside a write is counted and waits for it (point 1); a
// refusal of the policy's a write lifts is asked again (point 2); a write is
// judged against the set published (point 5); and each wait for a pending
// write is reached through its real path (point 7).

// gate holds whoever passes it until letGo: in is closed once one is held.
type gate struct {
	in, out     chan struct{}
	held, freed sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{in: make(chan struct{}), out: make(chan struct{})}
	t.Cleanup(g.letGo)
	return g
}

// pass holds the caller until letGo.
func (g *gate) pass() {
	g.held.Do(func() { close(g.in) })
	<-g.out
}

func (g *gate) letGo() { g.freed.Do(func() { close(g.out) }) }

// reachedGate waits until g holds someone, failing t where res, a write's
// answer, comes first, or nothing comes within the watchdog.
func reachedGate(t *testing.T, g *gate, res <-chan playback.ChoiceResult, what string) {
	t.Helper()
	reachedGateWithin(t, alsapcm.Watchdog(), g, res, what)
}

// reachedGateWithin is reachedGate within bound (B728 B9 round 16).
func reachedGateWithin(t *testing.T, bound time.Duration, g *gate, res <-chan playback.ChoiceResult, what string) {
	t.Helper()
	select {
	case <-g.in:
	case r := <-res:
		t.Fatalf("the write ended (%s) before %s", r.Outcome, what)
	case <-time.After(bound):
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", bound, what)
	}
}

// writeSeams are a rig's seams for writes of the bound DAC's choice beside
// playback: the next write held at its pending window (ChoicePending), or
// inside the state's write (inWrite, armed there); an open of a content held
// once it returned (Opened); a step's wake from a write held (ChoiceWoke); a
// session's release held (Releasing); a step's wait for a reconciliation's
// verdict held (Unjudged, round 17); and each wait for a write, each wait
// for a verdict and each activation recorded.
type writeSeams struct {
	mu         sync.Mutex
	changed    chan struct{}
	pending    *gate
	inWrite    func()
	opened     map[string]*gate
	woke       map[engine.OccurrenceID]*gate
	activation map[engine.OccurrenceID]*gate
	unjudged   map[engine.OccurrenceID]*gate
	releasing  *gate
	awaited    []engine.OccurrenceID
	verdicts   []engine.OccurrenceID
	activated  []engine.OccurrenceID
}

func newWriteSeams() *writeSeams {
	return &writeSeams{changed: make(chan struct{}), opened: map[string]*gate{}, woke: map[engine.OccurrenceID]*gate{},
		activation: map[engine.OccurrenceID]*gate{}, unjudged: map[engine.OccurrenceID]*gate{}}
}

func (w *writeSeams) note(f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f()
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *writeSeams) ch() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.changed
}

// seams is the rig's seams: base's, the write's own set over them.
func (w *writeSeams) seams(base *playback.Seams) *playback.Seams {
	s := &playback.Seams{}
	if base != nil {
		*s = *base
	}
	s.ChoicePending = func() {
		w.mu.Lock()
		g, arm := w.pending, w.inWrite
		w.pending, w.inWrite = nil, nil
		w.mu.Unlock()
		if arm != nil {
			arm()
		}
		if g != nil {
			g.pass()
		}
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
	s.ChoiceWoke = func(o engine.OccurrenceID) {
		w.mu.Lock()
		g := w.woke[o]
		delete(w.woke, o)
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
	s.ChoiceAwaited = func(o engine.OccurrenceID) { w.note(func() { w.awaited = append(w.awaited, o) }) }
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

// holdActivation holds o's activation as it begins, its sink adopted.
func (w *writeSeams) holdActivation(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.activation[o] = g
	w.mu.Unlock()
	return g
}

// holdPending holds the next write at its pending window.
func (w *writeSeams) holdPending(t *testing.T) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.pending = g
	w.mu.Unlock()
	return g
}

// holdInWrite holds the next write inside the state's write: arm is what
// the write's pending window arms (heldStore.arm).
func (w *writeSeams) holdInWrite(arm func()) {
	w.mu.Lock()
	w.inWrite = arm
	w.mu.Unlock()
}

// holdOpened holds the open of ref's content once it returned.
func (w *writeSeams) holdOpened(t *testing.T, ref string) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.opened[ref] = g
	w.mu.Unlock()
	return g
}

// holdWoke holds o's next wake from a write.
func (w *writeSeams) holdWoke(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.woke[o] = g
	w.mu.Unlock()
	return g
}

// holdReleasing holds the next session's release once it is marked
// released.
func (w *writeSeams) holdReleasing(t *testing.T) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.releasing = g
	w.mu.Unlock()
	return g
}

// activatedNow reports whether o's activation has begun (B728 B9 round 16).
func (w *writeSeams) activatedNow(o engine.OccurrenceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.activated, o)
}

// awaits is how many times a step of o's waited for a write.
func (w *writeSeams) awaits(o engine.OccurrenceID) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, x := range w.awaited {
		if x == o {
			n++
		}
	}
	return n
}

// waitAwaited waits until steps of o's waited for a write n times.
func (w *writeSeams) waitAwaited(t *testing.T, o engine.OccurrenceID, n int) {
	t.Helper()
	w.waitAwaitedWithin(t, alsapcm.Watchdog(), o, n)
}

// waitAwaitedWithin is waitAwaited within bound (B728 B9 round 16).
func (w *writeSeams) waitAwaitedWithin(t *testing.T, bound time.Duration, o engine.OccurrenceID, n int) {
	t.Helper()
	waitWithin(t, bound, fmt.Sprintf("a step of occurrence %d to wait for the write (%d)", o, n), w.ch, func() bool { return w.awaits(o) >= n })
}

// holdUnjudged holds o's next wait for the reconciliation of a set published
// since o was admitted, at the Unjudged seam, the step's lock let go and
// before the wait (B728 B9 round 17).
func (w *writeSeams) holdUnjudged(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	w.mu.Lock()
	w.unjudged[o] = g
	w.mu.Unlock()
	return g
}

// verdictWaits is how many times a step of o's waited for a reconciliation
// to judge it (the Unjudged seam, B728 B9 round 17).
func (w *writeSeams) verdictWaits(o engine.OccurrenceID) int {
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

// waitVerdictWaits waits, within alsapcm.Ordered, until steps of o's waited
// for a reconciliation's verdict n times (B728 B9 round 17).
func (w *writeSeams) waitVerdictWaits(t *testing.T, o engine.OccurrenceID, n int, what string) {
	t.Helper()
	waitOrdered(t, fmt.Sprintf("%s (occurrence %d's wait for a verdict, %d)", what, o, n), w.ch, func() bool { return w.verdictWaits(o) >= n })
}

// waitOrderedEither is waitOnEither within alsapcm.Ordered, at an ordering
// the test forces (B728 B9 round 17).
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

// waitOnEither waits until cond holds, asked again whenever a or b changes.
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

// preparedOrRefused waits until the backend has reported o Prepared or
// Refused, and fails t, saying why it should not have been, on the refusal.
func (d *dsdRig) preparedOrRefused(o engine.OccurrenceID, why string) {
	d.t.Helper()
	evs := d.waitEvents("the successor prepared or refused", func(evs []event) bool { return count(evs, "prepared", o)+count(evs, "refused", o) > 0 })
	if count(evs, "refused", o) > 0 {
		d.t.Fatalf("occurrence %d was refused, though %s: %s", o, why, kinds(evs))
	}
}

// waitAwaitedOr waits until steps of o's waited for a write n times, or the
// fake kernel shows what cond, named what, asks, whichever comes first.
func (w *writeSeams) waitAwaitedOr(t *testing.T, d *dsdRig, o engine.OccurrenceID, n int, what string, cond func(alsapcm.KernelView) bool) {
	t.Helper()
	deadline := time.After(alsapcm.Watchdog())
	for {
		changed := w.ch()
		if w.awaits(o) >= n || cond(d.k.View()) {
			return
		}
		select {
		case <-changed:
		case <-d.k.When(cond):
		case <-deadline:
			alsapcm.LogGoroutines(t)
			t.Fatalf("waited %v for a step of occurrence %d to wait for the write (%d), or %s", alsapcm.Watchdog(), o, n, what)
		}
	}
}

// waitedUnactivated waits until a step of o's waited for a write, and fails
// t where o's activation had begun by then (B728 B9 round 3, point 2): the
// first such wait of an occurrence whose own sink opened beside the write is
// that sink's adoption's, which comes before the activation, so an adoption
// that did not wait reaches the activation first, whose own wait for the
// write (awaitWrite) comes after it, and the activation stays unreached while
// the write is held.
func (w *writeSeams) waitedUnactivated(t *testing.T, o engine.OccurrenceID) {
	t.Helper()
	w.waitAwaited(t, o, 1)
	w.mu.Lock()
	activated := slices.Contains(w.activated, o)
	w.mu.Unlock()
	if activated {
		t.Fatalf("occurrence %d's activation began before it waited for the write: its sink's adoption did not wait for a write pending", o)
	}
}

// heldStore is the player's DSD state in memory whose next write, once
// armed, holds inside the store's lock after its change is made, as a
// write's fsync holds it: the state's reads (State) wait for it as the real
// store's do (dsdstate.Store.State takes the lock its Update holds).
type heldStore struct {
	*playbacktest.MemStore
	mu   sync.Mutex
	hold *gate
}

// arm holds the next write at g.
func (h *heldStore) arm(g *gate) {
	h.mu.Lock()
	h.hold = g
	h.mu.Unlock()
}

func (h *heldStore) Update(fn func(*dsdstate.State) error) error {
	return h.MemStore.Update(func(st *dsdstate.State) error {
		err := fn(st)
		h.mu.Lock()
		g := h.hold
		h.hold = nil
		h.mu.Unlock()
		if g != nil {
			g.pass()
		}
		return err
	})
}

// glitchBox is the box whose card's controls, as many times as armed, fail
// to read: a reading of the gain that glitches.
type glitchBox struct {
	*playbacktest.Box
	glitches atomic.Int32
}

func (g *glitchBox) ReadControls(card int) (alsactl.Snapshot, error) {
	if g.glitches.Add(-1) >= 0 {
		return alsactl.Snapshot{}, errors.New("the control node did not answer")
	}
	return g.Box.ReadControls(card)
}

// capAt caps a write's choice at bitRate.
func capAt(bitRate int) func(*playback.ChoiceRequest) {
	return func(r *playback.ChoiceRequest) { r.Choice.MaxBitRate = bitRate }
}

// goAhead gives a write the go-ahead to interrupt what plays.
func goAhead(r *playback.ChoiceRequest) { r.Interrupt = true }

// choose starts req's write on a goroutine of its own, its answer on the
// channel returned.
func (d *dsdRig) choose(req playback.ChoiceRequest) <-chan playback.ChoiceResult {
	res := make(chan playback.ChoiceResult, 1)
	go func() { res <- d.be.ChooseDSD(bg, req) }()
	return res
}

// answer waits for a write's answer.
func answer(t *testing.T, res <-chan playback.ChoiceResult) playback.ChoiceResult {
	t.Helper()
	return answerWithin(t, alsapcm.Watchdog(), res)
}

// answerWithin is answer within bound (B728 B9 round 16).
func answerWithin(t *testing.T, bound time.Duration, res <-chan playback.ChoiceResult) playback.ChoiceResult {
	t.Helper()
	select {
	case r := <-res:
		return r
	case <-time.After(bound):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the write was not answered within %v", bound)
	}
	return playback.ChoiceResult{}
}

// wantPlayedAsItWas fails t unless both of the session's occurrences were
// heard natively, each in a sink of its own, nothing handed off again,
// faulted or skipped.
func wantPlayedAsItWas(t *testing.T, d *dsdRig, s *engine.Snapshot) {
	t.Helper()
	evs := d.rec.events()
	if n := count(evs, "audible", 0); n != 2 || count(evs, "replan", 0) != 0 || count(evs, "fault", 0) != 0 || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
		t.Errorf("%d occurrences heard, want a and b as they were, with no hand-off again, fault or skip: %s; faults %+v, skips %+v", n, kinds(evs), s.Faults(), s.Skips())
	}
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE, format.DSDU32BE}) {
		t.Errorf("the native opens asked %v, want a's and b's DSD_U32_BE", got)
	}
}

// TestASuccessorsOpenBesideAWriteIsCountedAndWaitsForIt (B728 B9 round 2,
// point 1): the producer opens a sink for a successor of a transport of its
// own once the sink before it drained, and an open under way while a write
// classifies what it would interrupt is the write's to count: the step that
// found no write pending claims the occurrence, so the write takes it as
// reaching a sink, and the sink's adoption waits for the write, the
// occurrence's activation unreached while the write is held (round 3, point
// 2), as the activation waits too. Without the go-ahead the write answers would_interrupt,
// whether the open returned before the state's write or inside it, where a
// refresh reading the state waits for that write's lock, and b plays as it
// was; with the go-ahead the write lands and b is handed off again before a
// frame reaches its node, nothing faulting.
func TestASuccessorsOpenBesideAWriteIsCountedAndWaitsForIt(t *testing.T) {
	for _, c := range []struct {
		name             string
		goAhead, inWrite bool
	}{
		{"its open returning before the state's write", false, false},
		{"its open returning inside the state's write", false, true},
		{"with the go-ahead", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWriteSeams()
			var held *heldStore
			d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) {
				held = &heldStore{MemStore: p.Store.(*playbacktest.MemStore)}
				p.Store = held
			}, func(o *rigOptions) { o.seams = w.seams(nil) })
			d.files.add("a", dsd(playbacktest.DSD64, 0.25))
			d.files.add("b", dsd(playbacktest.DSD128, 0.25))
			ids := d.insert("a", "b")
			d.play()
			ob := d.occurrenceOf(ids[1])
			waitOn(t, "a's sink open", d.rec.ch, func() bool { return d.rec.openedSinks() == 1 })
			d.waitState("a's driver_accepted", func(st dsdstate.State) bool {
				return playbacktest.Evidence(st, dsdstate.TransportNative, "DSD_U32_BE", playbacktest.DSD64) != nil
			})
			admit, letAdmit := d.unit.holdNextAdmit()
			t.Cleanup(letAdmit)
			d.k.Free()
			awaitHeld(t, "the check after open of b's node", admit)
			// A cap at DSD64 takes b's DSD128 away; a has played out.
			req := choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64))
			req.Interrupt = c.goAhead
			stop := newGate(t)
			if c.inWrite {
				w.holdInWrite(func() { held.arm(stop) })
			} else {
				w.mu.Lock()
				w.pending = stop
				w.mu.Unlock()
			}
			res := d.choose(req)
			reachedGate(t, stop, res, "the write's hold")
			letAdmit()
			w.waitedUnactivated(t, ob)
			stop.letGo()
			got := answer(t, res)
			switch {
			case c.goAhead && got.Outcome != playback.ChoiceWritten:
				t.Fatalf("the write with the go-ahead: %s, want written", got.Outcome)
			case !c.goAhead && got.Outcome != playback.ChoiceWouldInterrupt:
				t.Fatalf("the write beside b's open: %s, want would_interrupt, b's open counted as reaching its sink", got.Outcome)
			}
			s := d.waitEnded()
			d.rig.stop()
			if c.goAhead {
				if f := s.Faults(); len(f) != 0 || count(d.rec.events(), "fault", 0) != 0 {
					t.Errorf("the write's hand-off of b faulted: %+v", f)
				}
				if v := d.k.View(); len(v.OpenAt) != 2 || v.GivenBytes != v.OpenAt[1] {
					t.Errorf("b's node: %d opens, %d bytes given, b's from byte %v; want b's node opened and given nothing", v.Opens, v.GivenBytes, v.OpenAt)
				}
				if _, ok := s.Skipped(ids[1]); !ok {
					t.Errorf("b's entry was not skipped: the cap refuses its DSD128 once the write landed; skips %+v", s.Skips())
				}
				return
			}
			wantPlayedAsItWas(t, d, s)
			if c := d.store.State().DACs[0].Choice; c.MaxBitRate != 0 || c.Revision != 1 {
				t.Errorf("a write answered would_interrupt changed the choice: %+v", c)
			}
		})
	}
}

// TestAnActivationBesideAWriteWaitsForIt (B728 B9 round 2, point 1): a
// write that becomes pending once a successor's own sink was adopted, and
// before its activation, is waited for by the activation, no lock held, and
// the successor it hands off again by then (dropped for its replacement, or
// still to be) is taken back from its sink unwritten, never a fault: with the
// go-ahead, the write lands, b's node opened and given nothing, b's entry
// skipped under the cap.
func TestAnActivationBesideAWriteWaitsForIt(t *testing.T) {
	w := newWriteSeams()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	ob := d.occurrenceOf(ids[1])
	act := w.holdActivation(t, ob)
	d.k.Free()
	awaitHeld(t, "b's activation, its sink adopted", act.in)
	stop := w.holdPending(t)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
	reachedGate(t, stop, res, "the write's pending window")
	act.letGo()
	w.waitAwaited(t, ob, 1)
	stop.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
	s := d.waitEnded()
	d.rig.stop()
	if f := s.Faults(); len(f) != 0 || count(d.rec.events(), "fault", 0) != 0 {
		t.Errorf("the hand-off of b at its activation faulted: %+v", f)
	}
	if v := d.k.View(); len(v.OpenAt) != 2 || v.GivenBytes != v.OpenAt[1] {
		t.Errorf("b's node: %d opens, %d bytes given, b's from byte %v; want b's node opened and given nothing", v.Opens, v.GivenBytes, v.OpenAt)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped: the cap refuses its DSD128 once the write landed; skips %+v", s.Skips())
	}
}

// pausedBetween is a rig, its seams w's, paused between a, PCM, and b, DSD128
// natively: a's sink played out and closed and the pause's release of the
// device reported, so a resume opens b's sink itself
// (TestPlaybackReleaseWaitsForAResumesOpenBetweenTransports's way there). It
// answers b's occurrence.
func pausedBetween(t *testing.T, w *writeSeams) (*dsdRig, engine.OccurrenceID, []uint32) {
	t.Helper()
	tm := &timers{}
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) {
		o.seams = w.seams(nil)
		o.pauseRelease, o.afterFunc = time.Second, tm.afterFunc
	})
	d.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	ob := d.occurrenceOf(ids[1])
	d.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	if err := d.eng.Pause(bg); err != nil {
		t.Fatalf("a pause during the last frames of a transport: %v", err)
	}
	d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	d.k.Free()
	d.k.Await(t, "a's sink to close", func(v alsapcm.KernelView) bool { return v.Closes == 1 && !v.Open })
	tm.fire(t)
	d.waitSnap("the device released", func(s *engine.Snapshot) bool { return s.DeviceReleased() })
	d.be.RefreshDSD(true)
	return d, ob, ids
}

// resumeIn waits for a resume's answer.
func resumeIn(t *testing.T, resumed <-chan error) {
	t.Helper()
	select {
	case err := <-resumed:
		if err != nil {
			t.Fatalf("the resume: %v", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the resume was not answered within %v", alsapcm.Watchdog())
	}
}

// TestAResumesOpenBetweenTransportsBesideAWriteIsCounted (B728 B9 round 2,
// point 1): paused between two transports, a resume opens the next one's sink
// itself, under a claim it takes in the step that found no write pending, so
// a write classifying beside that open counts b as reaching its sink, and
// without the go-ahead answers would_interrupt; the sink the resume hands
// the producer is adopted, its adoption waiting while the write is held and
// b's activation unreached till then (round 3, point 2), and activated once
// the write settles, and b plays as it was.
func TestAResumesOpenBetweenTransportsBesideAWriteIsCounted(t *testing.T) {
	w := newWriteSeams()
	d, ob, _ := pausedBetween(t, w)
	admit, letAdmit := d.unit.holdNextAdmit()
	t.Cleanup(letAdmit)
	resumed := make(chan error, 1)
	go func() { resumed <- d.eng.Play(bg) }()
	awaitHeld(t, "the check after the resume's open of b's node", admit)
	stop := w.holdPending(t)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64)))
	reachedGate(t, stop, res, "the write's pending window")
	letAdmit()
	w.waitedUnactivated(t, ob)
	stop.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWouldInterrupt {
		t.Fatalf("the write beside the resume's open of b's sink: %s, want would_interrupt", got.Outcome)
	}
	resumeIn(t, resumed)
	s := d.waitEnded()
	d.rig.stop()
	evs := d.rec.events()
	if n := count(evs, "audible", 0); n != 2 || count(evs, "replan", 0) != 0 || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
		t.Errorf("%d occurrences heard, want a and b as they were: %s; faults %+v, skips %+v", n, kinds(evs), s.Faults(), s.Skips())
	}
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
		t.Errorf("the native opens asked %v, want b's DSD_U32_BE", got)
	}
}

// TestAResumeWaitsForEveryWriteThatChangesWhatItOpens (B728 B9 round 2,
// point 1): a resume between two transports waits for a write pending that
// changes its occurrence's rate, and asks again, under the lock that takes
// its claim, each time one settles: write A, failing, settles while write B
// is still to come; B becomes pending and classifies b with no sink and no
// open; and the resume, woken by A, opens nothing while B is pending. B lands
// and hands b off again, and no node is opened for it.
func TestAResumeWaitsForEveryWriteThatChangesWhatItOpens(t *testing.T) {
	w := newWriteSeams()
	d, ob, ids := pausedBetween(t, w)
	stopA := w.holdPending(t)
	d.store.Fail(errors.New("no space left on device"))
	resA := d.choose(choiceReq(t, d, dsdstate.ModeConvert, 1))
	reachedGate(t, stopA, resA, "write A's pending window")
	woke := w.holdWoke(t, ob)
	resumed := make(chan error, 1)
	go func() { resumed <- d.eng.Play(bg) }()
	w.waitAwaited(t, ob, 1)
	stopA.letGo()
	if got := answer(t, resA); got.Outcome != playback.ChoicePersistFailed {
		t.Fatalf("write A: %s, want persist_failed", got.Outcome)
	}
	awaitHeld(t, "the resume's wake from write A", woke.in)
	d.store.Fail(nil)
	stopB := w.holdPending(t)
	reqB := choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64))
	resB := d.choose(reqB)
	reachedGate(t, stopB, resB, "write B's pending window")
	opens := d.k.View().Opens
	woke.letGo()
	w.waitAwaitedOr(t, d, ob, 2, "b's node opened", func(v alsapcm.KernelView) bool { return v.Opens > opens })
	if v := d.k.View(); v.Opens != opens {
		t.Fatalf("the resume opened b's node while write B was pending: %d opens after %d", v.Opens, opens)
	}
	stopB.letGo()
	if got := answer(t, resB); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("write B: %s, want written", got.Outcome)
	}
	resumeIn(t, resumed)
	s := d.waitEnded()
	d.rig.stop()
	if f := s.Faults(); len(f) != 0 {
		t.Errorf("the hand-off of b faulted: %+v", f)
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want a's alone: no node opened for b", v.Opens)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped: the cap refuses its DSD128 once B landed; skips %+v", s.Skips())
	}
}

// cappedState is a state whose test DAC chose native capped at DSD64: DSD128
// and DSD256 refused.
func cappedState() dsdstate.State {
	st := playbacktest.ChoiceState(dsdstate.ModeNative)
	st.DACs[0].Choice.MaxBitRate = playbacktest.DSD64
	return st
}

// TestAPolicyRefusalAWriteLiftsIsAskedAgain (B728 B9 round 2, point 2): a
// successor its open refused by the policy's word (DSD128 under a cap at
// DSD64) is asked again where a write lifts the refusal, its content's own
// checks made then: one refused just before the write's pending window waits
// for the write at the end of its open, and one refused under a set the
// write's publish has replaced by the end of its open is asked again at
// once; each plays. A PCM session's first DSD successor so admitted has the
// session's DSD monitor started with its admission. An enabled content that
// gives no bits is refused as such, and a malformed one stays refused with
// its decoder's word.
func TestAPolicyRefusalAWriteLiftsIsAskedAgain(t *testing.T) {
	// refused is a rig over the capped state playing a, then b, b's open held
	// once it returned, refused.
	refused := func(t *testing.T, a, b []byte) (*dsdRig, *writeSeams, *gate, []uint32) {
		t.Helper()
		w := newWriteSeams()
		d := newDSDRig(t, cappedState(), func(o *rigOptions) { o.seams = w.seams(nil) })
		d.files.add("a", a)
		d.files.add("b", b)
		opened := w.holdOpened(t, "b")
		ids := d.insert("a", "b")
		d.play()
		awaitHeld(t, "b's open, refused at DSD128 under the cap", opened.in)
		return d, w, opened, ids
	}
	uncapped := func(t *testing.T, d *dsdRig) playback.ChoiceRequest {
		t.Helper()
		return choiceReq(t, d, dsdstate.ModeNative, 1)
	}
	written := func(t *testing.T, d *dsdRig) {
		t.Helper()
		if got := d.be.ChooseDSD(bg, uncapped(t, d)); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the write that lifts the cap: %s, want written", got.Outcome)
		}
	}
	wantBPlayed := func(t *testing.T, d *dsdRig, s *engine.Snapshot) {
		t.Helper()
		if len(s.Skips()) != 0 || len(s.Faults()) != 0 || count(d.rec.events(), "audible", 0) != 2 {
			t.Errorf("b did not play once the write lifted its refusal: skips %+v, faults %+v, events %s", s.Skips(), s.Faults(), kinds(d.rec.events()))
		}
	}
	t.Run("a refusal made before the write's pending window", func(t *testing.T) {
		d, w, opened, ids := refused(t, dsd(playbacktest.DSD64, 0.25), dsd(playbacktest.DSD128, 0.25))
		ob := d.occurrenceOf(ids[1])
		stop := w.holdPending(t)
		res := d.choose(uncapped(t, d))
		reachedGate(t, stop, res, "the write's pending window")
		opened.letGo()
		waitOnEither(t, "b to wait for the write, or its refusal reported", w.ch, d.rec.ch, func() bool {
			return w.awaits(ob) >= 1 || count(d.rec.events(), "refused", ob) > 0
		})
		if evs := d.rec.events(); count(evs, "refused", ob) > 0 {
			t.Fatalf("b was refused while the write that lifts its refusal was pending: %s", kinds(evs))
		}
		stop.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the write: %s, want written", got.Outcome)
		}
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		wantBPlayed(t, d, s)
		if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE, format.DSDU32BE}) {
			t.Errorf("the native opens asked %v, want a's and b's DSD_U32_BE", got)
		}
	})
	t.Run("a publish before the open ends", func(t *testing.T) {
		d, _, opened, _ := refused(t, dsd(playbacktest.DSD64, 0.25), dsd(playbacktest.DSD128, 0.25))
		written(t, d)
		opened.letGo()
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		wantBPlayed(t, d, s)
	})
	t.Run("a PCM session's first DSD successor", func(t *testing.T) {
		d, _, opened, ids := refused(t, wav(44100, 16, pattern(1, 44100, 16)), dsd(playbacktest.DSD128, 0.25))
		written(t, d)
		opened.letGo()
		d.preparedOrRefused(d.occurrenceOf(ids[1]), "a write lifted its refusal before its open ended")
		// The session's DSD monitor, started with b's admission, reads the
		// policy at its tick.
		d.monitorTick()
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		wantBPlayed(t, d, s)
	})
	t.Run("an enabled content that gives no bits", func(t *testing.T) {
		d, _, opened, ids := refused(t, dsd(playbacktest.DSD64, 0.25), playbacktest.DSFCut(playbacktest.DSD128, 0.25, 0, fill))
		written(t, d)
		opened.letGo()
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		if sk, ok := s.Skipped(ids[1]); !ok || sk.Reason != playback.ReasonEmpty {
			t.Errorf("b skipped %+v (%v), want %s: the policy plays its rate now, and its content gives no bits", sk, ok, playback.ReasonEmpty)
		}
	})
	t.Run("a malformed content stays refused", func(t *testing.T) {
		bad := dsd(playbacktest.DSD128, 0.25)[:40]
		_, err := decode.Open(bytes.NewReader(bad))
		r, ok := decode.AsRefusal(err)
		if !ok {
			t.Fatalf("the cut header opened (%v): want the decoder's refusal", err)
		}
		d, _, opened, ids := refused(t, dsd(playbacktest.DSD64, 0.25), bad)
		written(t, d)
		opened.letGo()
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		if sk, ok := s.Skipped(ids[1]); !ok || string(sk.Reason) != string(r.Reason) {
			t.Errorf("b skipped %+v (%v), want the decoder's %s", sk, ok, r.Reason)
		}
	})
}

// TestAWriteIsJudgedAgainstTheSetPublished (B728 B9 round 2, point 5): what
// a write would interrupt is decided against the set published, so a reading
// of the box that differs from it at a rate an occurrence plays is published
// and reconciled first, a refresh of its own, and the write reads again:
// a gain reading that glitches in the write's reading alone leaves the DoP
// occurrence heard counted, and the write without the go-ahead answers
// would_interrupt, DoP playing on; a gain that leaves 0 dB for good is the
// box's change, revoked by that refresh before the write, which then lands in
// its next round.
func TestAWriteIsJudgedAgainstTheSetPublished(t *testing.T) {
	// dopHeard is a rig whose DoP occurrence a, DSD64 past a gain stage at
	// unity, is heard, its open's evidence recorded, the box's control reads
	// glitching as armed.
	dopHeard := func(t *testing.T) (*dsdRig, *glitchBox) {
		t.Helper()
		var gb *glitchBox
		d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeDoP), func(p *playback.DSDPolicy) {
			gb = &glitchBox{Box: p.Box.(*playbacktest.Box)}
			p.Box = gb
		})
		unit := playbacktest.DACUnit()
		unit.Descriptors = playbacktest.VolumeDescriptors()
		d.box.SetUnit(card, unit)
		d.box.SetMixer(card, playbacktest.VolumeMixer())
		d.box.SetControls(card, playbacktest.VolumeControls(255)...)
		d.files.add("a", dsd(playbacktest.DSD64, 2))
		ids := d.insert("a")
		d.playedOut(dsdPrefill+200*time.Millisecond, "a")
		d.tick()
		d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[0] })
		d.waitState("the open's driver_accepted", func(st dsdstate.State) bool {
			return playbacktest.Evidence(st, dsdstate.TransportDoP, "S32_LE", playbacktest.DSD64) != nil
		})
		return d, gb
	}
	t.Run("a reading that glitches beside the write", func(t *testing.T) {
		d, gb := dopHeard(t)
		gb.glitches.Store(1)
		if got := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeConvert, 1)); got.Outcome != playback.ChoiceWouldInterrupt {
			t.Fatalf("convert beside the DoP occurrence heard: %s, want would_interrupt", got.Outcome)
		}
		if c, _ := d.store.State().DAC(playbacktest.DACKey()); c.Choice == nil || c.Choice.Mode != dsdstate.ModeDoP {
			t.Errorf("the choice after a write refused: %+v, want DoP as it was", c.Choice)
		}
		d.k.Free()
		s := d.waitEnded()
		d.rig.stop()
		if f := s.Faults(); len(f) != 0 || count(d.rec.events(), "fault", 0) != 0 {
			t.Errorf("DoP did not play on: faults %+v", f)
		}
	})
	t.Run("a change of the box that persists", func(t *testing.T) {
		d, _ := dopHeard(t)
		d.box.SetControls(card, playbacktest.VolumeControls(200)...)
		if got := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeConvert, 1)); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("convert once the gain left 0 dB: %s, want written", got.Outcome)
		}
		s := d.tickUntilStopped(engine.ReasonFault)
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the gain change's faults %+v, want DoP revoked", f)
		}
		if c, _ := d.store.State().DAC(playbacktest.DACKey()); c.Choice == nil || c.Choice.Mode != dsdstate.ModeConvert {
			t.Errorf("the choice %+v, want convert, written", c.Choice)
		}
		d.rig.stop()
	})
}

// TestASuccessorWhoseOpenEndsBesideAWriteWaitsForIt (B728 B9 round 2, point
// 7): a successor whose open ends while a write pending changes its rate
// waits for the write at the end of its open, so the end of its open is taken
// under the set the write leaves, and it is never handed off again for it:
// admitted natively at DSD128 as its open read the set, refused once the
// write's cap at DSD64 landed.
func TestASuccessorWhoseOpenEndsBesideAWriteWaitsForIt(t *testing.T) {
	w := newWriteSeams()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	opened := w.holdOpened(t, "b")
	ids := d.insert("a", "b")
	d.play()
	awaitHeld(t, "b's open, admitted natively", opened.in)
	ob := d.occurrenceOf(ids[1])
	stop := w.holdPending(t)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64)))
	reachedGate(t, stop, res, "the write's pending window")
	opened.letGo()
	w.waitAwaited(t, ob, 1)
	stop.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap: %s, want written", got.Outcome)
	}
	d.k.Free()
	s := d.waitEnded()
	d.rig.stop()
	if n := count(d.rec.events(), "replan", 0); n != 0 {
		t.Errorf("b was handed off again (%d): its open's end waited for the write, and took the set it left", n)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
	}
	if got := d.openFormats(); !slices.Equal(got, []format.ALSAFormat{format.DSDU32BE}) {
		t.Errorf("the native opens asked %v, want a's alone", got)
	}
}

// TestNoSinkOpensForASuccessorWhileAWriteChangesItsRate (B728 B9 round 2,
// point 7): a successor of a transport of its own, the sink before it
// drained, waits for a write pending that changes its rate before a sink
// opens for it, and once the write lands is handed off again, no node opened
// for it.
func TestNoSinkOpensForASuccessorWhileAWriteChangesItsRate(t *testing.T) {
	w := newWriteSeams()
	drained := newGate(t)
	var once sync.Once
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) {
		o.seams = w.seams(&playback.Seams{TransportDrained: func() { once.Do(drained.pass) }})
	})
	// The producer, held once a's sink drained, is let go before the rig
	// stops: the gate's own cleanup, registered before the rig's, runs after
	// it (round 17).
	t.Cleanup(drained.letGo)
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	ob := d.occurrenceOf(ids[1])
	d.k.Free()
	awaitHeld(t, "a's sink drained for b's transport", drained.in)
	stop := w.holdPending(t)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64)))
	reachedGate(t, stop, res, "the write's pending window")
	drained.letGo()
	w.waitAwaited(t, ob, 1)
	if v := d.k.View(); v.Opens != 1 {
		t.Fatalf("%d opens while the write was pending: a sink opened for b", v.Opens)
	}
	stop.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap: %s, want written", got.Outcome)
	}
	s := d.waitEnded()
	d.rig.stop()
	if f := s.Faults(); len(f) != 0 {
		t.Errorf("b's hand-off faulted: %+v", f)
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want a's alone", v.Opens)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped; skips %+v", s.Skips())
	}
}

// TestAConvertedJoinWindowWaitsForAWriteThatChangesItsSuccessor (B728 B9
// round 2, point 7): a converted occurrence at its join window, its
// successor converted at its rate and settled, holds the window while a
// write pending changes that rate, as for a successor that has not settled,
// until the write settles or the sink's lookahead falls to its floor. The
// wait breaks no window: one the write outlasts ends at the floor,
// zero-padded, and its successor follows as one that settled after the
// floor; here the write, the go-ahead given, stops the session's DSD, the
// window decided in no case before the write settled or the floor came.
// Round 17 forces two orderings. The write's reconciliation held where it has
// asked the engine to hand f off again and has not called Revoke, the order
// CI took on round 16's head (run 42619003055): the revocation's fault is the
// session's from the reconciliation's own step, so the engine's replacement
// of f finds the session ending and no end is named after a for the window to
// take; until round 17 the fault came in Revoke alone, after the hand-off,
// which the engine could answer first: f's replacement refused under the set
// the write published, its entry skipped, the end named after a and the
// window ended on that end. And the write's reconciliation held once its set
// is published, above the floor: the window waits for the reconciliation's
// verdict on f, admitted under the set the write replaced, and the floor
// still ends that wait.
func TestAConvertedJoinWindowWaitsForAWriteThatChangesItsSuccessor(t *testing.T) {
	// holdingOpts are holding's: the policy's log, whose line saying the
	// session's DSD stops a case can hold (stopsLog), and the publish a case
	// can hold (publishHold).
	type holdingOpts struct {
		log *stopsLog
		pub *publishHold
	}
	// holding is a rig whose converted a, DSD64, is at its window point, its
	// successor f settled, a write that turns DSD off, with the go-ahead,
	// pending.
	holding := func(t *testing.T, ho holdingOpts) (*dsdRig, *writeSeams, *convSteps, *gate, <-chan playback.ChoiceResult, engine.OccurrenceID, engine.OccurrenceID) {
		t.Helper()
		w := newWriteSeams()
		steps := newConvSteps(t, "holding")
		d := newDSDRigWith(t, dsdstate.Defaults(), func(p *playback.DSDPolicy) {
			p.Profiles = nil
			if ho.log != nil {
				p.Log = ho.log
			}
		}, func(o *rigOptions) {
			o.seams = w.seams(&playback.Seams{Converting: steps.seam})
			if ho.pub != nil {
				o.seams.Published = ho.pub.seam
			}
		})
		// The producer, held at a's window point until let go below, is let go
		// before the rig stops: the cleanup newConvSteps registered, before the
		// rig's, runs after it (round 17).
		t.Cleanup(steps.letGo)
		d.files.add("a", track(playbacktest.DSD64, 0.5, noise(1)))
		d.files.add("f", track(playbacktest.DSD64, 0.5, noise(2)))
		ids := d.insert("a", "f")
		d.play()
		of := d.preparedOccWithin(ids[1], alsapcm.Ordered())
		steps.heldWithin(t, alsapcm.Ordered())
		oa := d.firstOcc()
		stop := w.holdPending(t)
		res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 0, goAhead))
		reachedGateWithin(t, alsapcm.Ordered(), stop, res, "the write's pending window")
		steps.letGo()
		w.waitAwaitedWithin(t, alsapcm.Ordered(), of, 1)
		if steps.has("decided", oa) {
			t.Fatal("a's window was decided while the write was pending, its successor's rate changing")
		}
		return d, w, steps, stop, res, oa, of
	}
	revoked := func(t *testing.T, d *dsdRig, steps *convSteps, oa engine.OccurrenceID) {
		t.Helper()
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the write's faults %+v, want the session's DSD revoked", f)
		}
		if steps.has("decided", oa) {
			t.Error("a's window was decided once its DSD was revoked")
		}
	}
	t.Run("the write settling first", func(t *testing.T) {
		d, _, steps, stop, res, oa, _ := holding(t, holdingOpts{})
		stop.letGo()
		if got := answerWithin(t, alsapcm.Ordered(), res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		revoked(t, d, steps, oa)
	})
	t.Run("the engine's hand-off answered before the revocation", func(t *testing.T) {
		// The write's reconciliation held where it has asked the engine to hand
		// f off again and has not called Revoke (its line saying the session's
		// DSD stops), the boundary CI's red on round 16's head named (round 17):
		// the engine's replacement of f is prepared while the hold stands, and
		// its open waits for the refresh the write runs, whose lock its
		// admission takes, so no end is named after a, and nothing decides a's
		// window, before Revoke records the session's fault.
		log := &stopsLog{}
		d, _, steps, stop, res, oa, of := holding(t, holdingOpts{log: log})
		stops := newGate(t)
		log.arm(stops)
		stop.letGo()
		reachedGateWithin(t, alsapcm.Ordered(), stops, res, "the reconciliation's revocation, f handed off again, before its call to Revoke")
		d.waitCallWithin(alsapcm.Ordered(), "the engine's replacement of f, prepared while the revocation is held", func(c call) bool {
			return c.kind == "prepare" && c.after == oa && c.next != nil && c.next.ID != of
		})
		if steps.has("decided", oa) {
			t.Fatal("a's window was decided once the engine handed f off again, before the revocation's fault")
		}
		stops.letGo()
		if got := answerWithin(t, alsapcm.Ordered(), res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		revoked(t, d, steps, oa)
	})
	t.Run("the floor coming first", func(t *testing.T) {
		d, _, steps, stop, res, oa, _ := holding(t, holdingOpts{})
		awaitOrdered(t, d.k.When(runningAndWaiting), "the writer to fill the hardware buffer and wait on the DAC")
		// 450 ms of a's 500 played: the lookahead falls below its floor.
		d.k.Play(t, 450*time.Millisecond)
		d.tickWithin(alsapcm.Ordered())
		waitOrdered(t, "a's window decided at the floor, the write pending", steps.ch, func() bool { return steps.has("decided", oa) })
		stop.letGo()
		if got := answerWithin(t, alsapcm.Ordered(), res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		d.k.Free()
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		for _, f := range s.Faults() {
			if f.Reason == engine.ReasonXrun {
				t.Errorf("the sink ran dry: %+v", f)
			}
		}
	})
	t.Run("the reconciliation held above the floor, the floor coming", func(t *testing.T) {
		// The write's set published and its reconciliation held, the sink above
		// its floor: the write is no longer pending for f once the set it would
		// publish is the one published, and a's window waits for the
		// reconciliation's verdict on f, admitted under the set the write
		// replaced (round 17); the DAC played on to the floor, the window ends
		// there, zero-padded, while the reconciliation is still held, and the
		// sink never runs dry.
		pub := &publishHold{}
		d, w, steps, stop, res, oa, of := holding(t, holdingOpts{pub: pub})
		g := pub.arm(t)
		n := w.verdictWaits(of)
		stop.letGo()
		awaitOrdered(t, g.in, "the write's set published, its reconciliation held")
		// The reporter's step has the producer read a's window again.
		d.tickWithin(alsapcm.Ordered())
		w.waitVerdictWaits(t, of, n+1, "a's window, above the floor, to wait for the reconciliation's verdict on f")
		if steps.has("decided", oa) {
			t.Fatal("a's window was decided once the write published, above the floor, before its reconciliation judged f")
		}
		awaitOrdered(t, d.k.When(runningAndWaiting), "the writer to fill the hardware buffer and wait on the DAC")
		// 450 ms of a's 500 played: the lookahead falls below its floor.
		d.k.Play(t, 450*time.Millisecond)
		d.tickWithin(alsapcm.Ordered())
		waitOrdered(t, "a's window decided at the floor, the reconciliation held", steps.ch, func() bool { return steps.has("decided", oa) })
		g.letGo()
		if got := answerWithin(t, alsapcm.Ordered(), res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		d.k.Free()
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		for _, f := range s.Faults() {
			if f.Reason == engine.ReasonXrun {
				t.Errorf("the sink ran dry: %+v", f)
			}
		}
	})
}

// producerHold holds a session's producer at its second write step, the
// first its Start does not wait for (the first frames are Start's answer): a
// step of the first occurrence's, before the producer decides what follows
// it, held there until let go; and records every step the producer decides
// (playback.Seams.ProducerStep, B728 B9 round 16), counting its waits (round
// 17).
type producerHold struct {
	mu      sync.Mutex
	writes  int
	waitN   int
	steps   []string
	changed chan struct{}
	gate    *gate
}

func newProducerHold(t *testing.T) *producerHold {
	return &producerHold{changed: make(chan struct{}), gate: newGate(t)}
}

// note is the seam.
func (p *producerHold) note(_ engine.SessionID, step string, o engine.OccurrenceID) {
	p.mu.Lock()
	p.steps = append(p.steps, fmt.Sprintf("%s %d", step, o))
	switch step {
	case "write":
		p.writes++
	case "wait":
		p.waitN++
	}
	hold := step == "write" && p.writes == 2
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	if hold {
		p.gate.pass()
	}
}

func (p *producerHold) ch() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

// has reports whether the producer decided step for o.
func (p *producerHold) has(step string, o engine.OccurrenceID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.steps, fmt.Sprintf("%s %d", step, o))
}

// waits is how many wait steps the producer decided: for a write pending, a
// reconciliation's verdict or anything else (B728 B9 round 17).
func (p *producerHold) waits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitN
}

// publishHold holds the next refresh that publishes a set, once armed, where
// it has published and has not reconciled (playback.Seams.Published), until
// let go (B728 B9 round 16). It runs under the refreshes' lock, and calls
// nothing of the backend.
type publishHold struct {
	armed atomic.Bool
	gate  *gate
}

func (h *publishHold) seam() {
	if h.armed.CompareAndSwap(true, false) {
		h.gate.pass()
	}
}

// arm holds the next publish from now on at a gate made here, once the rig
// is built, so the gate's cleanup, which lets the publish go, runs before the
// rig's stop (B728 B9 round 17: made before the rig, it ran after the stop,
// and a case that failed while it held waited out the rig's watchdog).
func (h *publishHold) arm(t *testing.T) *gate {
	h.gate = newGate(t)
	h.armed.Store(true)
	return h.gate
}

// TestAGaplessSuccessorWaitsForAWriteNoLongerThanTheSinksLookahead (B728 B9
// round 2): a successor on its predecessor's transport (PCM at 176.4 kHz / 24
// bits, then DSD64 converted to it) waits out of the sink for a write pending
// that changes its rate (DSD off) only while the sink holds more than its
// floor of lookahead: at the floor the sink drains and plays out what it
// holds, an honest gap, rather than run dry beside a producer that waits, and
// the successor waits for the write before a sink of its own opens; the
// write, which interrupts nothing, lands and hands it off again, nothing
// faulting. Round 16 forces its orderings, each wait at them within a few
// seconds: the write pending before the producer decides what follows a (the
// producer held in a until then; unheld, it could hand b into a's sink first,
// and the wait for b's wait hung, CI on round 15's head); the DAC's time at
// the floor before the sink publishes the measurement that shows it (that
// STATUS held, Codex's review of round 16's brief): the floor is the sink's
// measurement's, which the producer reads at the reporter's steps, and never
// the DAC's time; and where the drain for b's transport ends beside the
// write's landing, which runs under load found unforced (a second open in
// about one run in a hundred, round 15's code and round 16's alike). The
// write answered before the drain ends, b is handed off again and no sink
// opens for it; the drain ending once the write has published the set that
// refuses b and before its reconciliation has judged b, b's transition waits
// for that reconciliation's verdict, and no sink opens for b either (round
// 17: until then b's own sink opened, and its activation, judging b under
// the set published, took it back unwritten). Round 17 forces five more
// orderings. A reporter's step that comes once the floor's measurement is
// established, as the one tickWithin's second send begins can (Codex's review
// of round 16): the producer reads the floor and the sink drains before the
// case looks for the writer back on the DAC, which a drain begun first never
// shows again, so the case looks for either. The write's reconciliation held
// above the floor: b, admitted under the set the write replaced, waits for
// its verdict out of a's sink, asked again at the reporter's steps, and the
// floor still ends that wait (round 16's waited for the reporter's wake
// alone, which no step gave it, so past the floor the sink would run dry).
// The reconciliation run while the producer is held at its wait for the
// verdict: the reconciliation never waits on the producer. The engine's stop
// while that wait stands and the reconciliation is held: the session's
// release ends the wait, which is the wait's bound once the drain has ended.
// And b handed into a's sink before the write (round 16's ordering C5): the
// write counts b, whose frames are in the sink, and without the go-ahead
// answers would_interrupt, b playing as it was.
func TestAGaplessSuccessorWaitsForAWriteNoLongerThanTheSinksLookahead(t *testing.T) {
	const rate = 176400
	const aFrames, floor = rate / 2, rate / 10 // a's 500 ms, the sink's floor's 100 ms
	// floorOpts are the cases' setup's: the publish a case can hold, and a
	// reporter's step forced once the floor's measurement is established.
	type floorOpts struct {
		pub      *publishHold
		lateStep bool
	}
	// started plays a and b, the producer held in a before it decides what
	// follows a: it answers the rig, its write seams, the producer's hold, the
	// entries and b's occurrence, prepared.
	started := func(t *testing.T, fo floorOpts) (*dsdRig, *writeSeams, *producerHold, []uint32, engine.OccurrenceID) {
		t.Helper()
		w, p := newWriteSeams(), newProducerHold(t)
		d := newConvRig(t, func(o *rigOptions) {
			o.seams = w.seams(nil)
			o.seams.ProducerStep = p.note
			if fo.pub != nil {
				o.seams.Published = fo.pub.seam
			}
		})
		// The producer, held in a until let go, is let go before the rig stops:
		// the cleanup newProducerHold registered, before the rig's, runs after
		// it (round 17).
		t.Cleanup(p.gate.letGo)
		d.files.add("a", wav(rate, 24, pattern(1, aFrames, 24)))
		d.files.add("b", track(playbacktest.DSD64, 0.25, noise(2)))
		ids := d.insert("a", "b")
		d.play()
		awaitOrdered(t, p.gate.in, "the producer, held in a before it decides what follows a")
		ob := d.preparedOccWithin(ids[1], alsapcm.Ordered())
		return d, w, p, ids, ob
	}
	// pending is started with the write of DSD off pending and the producer
	// let go: b held out of a's sink for the write, the sink's lookahead above
	// its floor, the writer waiting on the DAC. It answers the write's hold and
	// its answer too.
	pending := func(t *testing.T, fo floorOpts) (*dsdRig, *writeSeams, *producerHold, *gate, <-chan playback.ChoiceResult, []uint32, engine.OccurrenceID) {
		t.Helper()
		d, w, p, ids, ob := started(t, fo)
		stop := w.holdPending(t)
		res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 0))
		reachedGateWithin(t, alsapcm.Ordered(), stop, res, "the write's pending window")
		// Let go with the write pending, the producer writes the rest of a into
		// the sink and holds b out of it, the sink's lookahead above its floor.
		p.gate.letGo()
		w.waitAwaitedWithin(t, alsapcm.Ordered(), ob, 1)
		awaitOrdered(t, d.k.When(runningAndWaiting), "the writer to fill the hardware buffer and wait on the DAC")
		return d, w, p, stop, res, ids, ob
	}
	// toFloor has the DAC play a until the sink's measurement shows its floor,
	// b held out of a's sink until then (the producer's waits, for the write
	// pending or for a reconciliation's verdict), and the sink drain there for
	// b's own transport, the DAC holding that drain.
	toFloor := func(t *testing.T, d *dsdRig, p *producerHold, ob engine.OccurrenceID, lateStep bool) {
		t.Helper()
		// 400 ms of a's 500 played: the DAC at the floor, and the sink's STATUS
		// there, the measurement that shows it, held. a's last frames reached the
		// kernel at 300 ms (a buffer of 200 ms), and the writer, with nothing to
		// write, has measured each 50 ms period since: the producer, woken by
		// the reporter, reads the measurement before, 350 ms, and holds b out of
		// the sink still.
		statusing, status := d.k.ParkStatusAfterPlayed(aFrames - floor)
		defer status()
		d.k.Grant(400 * time.Millisecond)
		awaitOrdered(t, statusing, "the sink's STATUS once the DAC played 400 ms of a")
		if v := d.k.View(); v.Consumed != aFrames-floor || v.Appl != aFrames {
			t.Fatalf("the DAC had played %d of the %d frames written at the STATUS held; want %d of a's %d, the sink at its floor (the test's premise)", v.Consumed, v.Appl, aFrames-floor, aFrames)
		}
		n := p.waits()
		d.tickWithin(alsapcm.Ordered())
		waitOrdered(t, "the producer to hold b out of a's sink again at the reporter's step, the measurement at 350 ms", p.ch, func() bool { return p.waits() > n })
		if p.has("transition", ob) || d.k.View().Drains != 0 {
			t.Fatal("the producer let b's wait go before the sink's measurement showed its floor")
		}
		// The measurement established (the writer back on the DAC once it has),
		// the reporter's next step has the producer read it: at the floor the
		// sink drains for b's own transport, an honest gap, b still out of it. A
		// producer that waited past it would let the sink run dry.
		status()
		if lateStep {
			// A step of the reporter's that comes once the measurement is
			// established, as the one tickWithin's second send begins can: it
			// wakes the producer, which reads the floor and has the sink drain
			// before the case looks below.
			awaitOrdered(t, d.k.When(runningAndWaiting), "the writer back on the DAC, the measurement at a's last frames established")
			d.tickWithin(alsapcm.Ordered())
			awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return v.Drains >= 1 }), "the sink draining for b's own transport, the producer woken by the late step")
		}
		// The writer back on the DAC, or the sink draining already, the producer
		// woken by a reporter's step that came once the measurement was
		// established: each holds from when it first does to the case's next
		// step, so the case misses neither (round 17; it looked for the writer
		// alone, which a drain begun first never shows again).
		awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return runningAndWaiting(v) || v.Drains >= 1 }),
			"the writer back on the DAC, or the sink draining, the measurement at a's last frames established")
		d.tickWithin(alsapcm.Ordered())
		waitOrdered(t, "the producer's transition for b at the sink's measured floor", p.ch, func() bool { return p.has("transition", ob) })
		awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return v.Drains >= 1 }), "the sink to drain at its floor, b still out of it")
	}
	// atFloor is pending, then toFloor: b out of a's sink for the write
	// pending, the sink drained at its floor, the DAC holding the drain.
	atFloor := func(t *testing.T, fo floorOpts) (*dsdRig, *writeSeams, *gate, <-chan playback.ChoiceResult, []uint32, engine.OccurrenceID) {
		t.Helper()
		d, w, p, stop, res, ids, ob := pending(t, fo)
		toFloor(t, d, p, ob, fo.lateStep)
		return d, w, stop, res, ids, ob
	}
	// played takes the session's end, the write answered, and judges what
	// every order leaves alike: nothing faulting and b's entry skipped. It
	// answers the fake kernel as the rig's stop left it.
	played := func(t *testing.T, d *dsdRig, ids []uint32) alsapcm.KernelView {
		t.Helper()
		s := d.waitEnded()
		d.rig.stop()
		if f := s.Faults(); len(f) != 0 {
			t.Errorf("faults %+v: the sink ran dry while b waited for the write", f)
		}
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped; skips %+v", s.Skips())
		}
		return d.k.View()
	}
	written := func(t *testing.T, res <-chan playback.ChoiceResult) {
		t.Helper()
		if got := answerWithin(t, alsapcm.Ordered(), res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off beside a PCM track: %s, want written", got.Outcome)
		}
	}
	t.Run("the write answered before the drain ends", func(t *testing.T) {
		d, _, stop, res, ids, _ := atFloor(t, floorOpts{})
		// The write published, reconciled (b handed off again) and settled
		// while the DAC holds the drain; then the drain ends.
		stop.letGo()
		written(t, res)
		d.k.Free()
		if v := played(t, d, ids); v.Opens != 1 {
			t.Errorf("%d opens, want a's alone: b handed off again, no sink opened for it", v.Opens)
		}
	})
	t.Run("a reporter's step after the floor's measurement", func(t *testing.T) {
		// A reporter's step forced once the floor's measurement is established:
		// the sink drains before the case looks, which its look takes; then as
		// the write answered before the drain ends.
		d, _, stop, res, ids, _ := atFloor(t, floorOpts{lateStep: true})
		stop.letGo()
		written(t, res)
		d.k.Free()
		if v := played(t, d, ids); v.Opens != 1 {
			t.Errorf("%d opens, want a's alone: b handed off again, no sink opened for it", v.Opens)
		}
	})
	t.Run("the drain ending between the write's publish and its reconciliation", func(t *testing.T) {
		pub := &publishHold{}
		d, w, stop, res, ids, ob := atFloor(t, floorOpts{pub: pub})
		g := pub.arm(t)
		stop.letGo()
		awaitOrdered(t, g.in, "the write's set published, its reconciliation held")
		n := w.verdictWaits(ob)
		d.k.Free()
		// The drain ends with the set that refuses b published and its
		// reconciliation held: b's transition waits for the reconciliation's
		// verdict on b, its own sink unopened (round 17); until then b's own
		// sink opened there and its activation took it back.
		waitOrderedEither(t, "b's transition, the drain ended, to wait for the reconciliation's verdict, or b's own sink to open", w.ch,
			func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens >= 2 }) },
			func() bool { return w.verdictWaits(ob) > n || d.k.View().Opens >= 2 })
		if v := d.k.View(); v.Opens != 1 || w.activatedNow(ob) {
			t.Fatalf("with the reconciliation that judges b held: %d opens, b's activation begun %v; want a's open alone, b's transition waiting for its verdict", v.Opens, w.activatedNow(ob))
		}
		g.letGo()
		written(t, res)
		if v := played(t, d, ids); v.Opens != 1 {
			t.Errorf("%d opens, want a's alone: b handed off again by the reconciliation, no sink opened for it", v.Opens)
		}
	})
	t.Run("the write's reconciliation held above the floor", func(t *testing.T) {
		// The write's set published and its reconciliation held, the sink above
		// its floor: the write is no longer pending for b once the set it would
		// publish is the one published, and b, admitted under the set the write
		// replaced, waits for the reconciliation's verdict out of a's sink, asked
		// again at the reporter's steps; at the floor the sink drains, b's own
		// sink waiting for the verdict, and the sink never runs dry.
		pub := &publishHold{}
		d, w, p, stop, res, ids, ob := pending(t, floorOpts{pub: pub})
		g := pub.arm(t)
		n := w.verdictWaits(ob)
		stop.letGo()
		awaitOrdered(t, g.in, "the write's set published, its reconciliation held")
		// The reporter's steps have the producer ask b again, the set published
		// and its reconciliation held: toFloor's own waits for that come long
		// after the publish, so each is one the publish's verdict decides (a
		// wait for the write itself, which the reporter can wake the producer
		// into as the write publishes, is not counted there).
		d.tickWithin(alsapcm.Ordered())
		toFloor(t, d, p, ob, false)
		if got := w.verdictWaits(ob); got <= n {
			t.Errorf("b's waits out of a's sink once the write published were for no reconciliation's verdict (%d such waits)", got-n)
		}
		v := w.verdictWaits(ob)
		d.k.Free()
		w.waitVerdictWaits(t, ob, v+1, "b's transition, the drain ended, to wait for the reconciliation's verdict")
		if k := d.k.View(); k.Opens != 1 {
			t.Fatalf("%d opens with the reconciliation held, want a's alone: b's own sink waits for its verdict", k.Opens)
		}
		g.letGo()
		written(t, res)
		if k := played(t, d, ids); k.Opens != 1 || k.Drains != 1 {
			t.Errorf("%d opens and %d drains, want a's open, drained once at its floor: b handed off again, no sink opened for it", k.Opens, k.Drains)
		}
	})
	t.Run("the reconciliation beside a producer held at its wait", func(t *testing.T) {
		// The reconciliation never waits on the producer: b's transition, the
		// drain ended, held where it waits for the reconciliation's verdict (the
		// Unjudged seam, before the wait), the reconciliation runs, judges b and
		// the write is answered; let go, the producer finds b judged.
		pub := &publishHold{}
		d, w, stop, res, ids, ob := atFloor(t, floorOpts{pub: pub})
		g := pub.arm(t)
		stop.letGo()
		awaitOrdered(t, g.in, "the write's set published, its reconciliation held")
		held := w.holdUnjudged(t, ob)
		d.k.Free()
		awaitOrdered(t, held.in, "b's transition, the drain ended, held at its wait for the reconciliation's verdict")
		g.letGo()
		written(t, res)
		held.letGo()
		if v := played(t, d, ids); v.Opens != 1 {
			t.Errorf("%d opens, want a's alone: b handed off again by the reconciliation, no sink opened for it", v.Opens)
		}
	})
	t.Run("the engine's stop while the reconciliation is held", func(t *testing.T) {
		// The bound of b's wait for its verdict once the drain has ended is the
		// session's stop or release, its context: with the reconciliation held
		// past the case's look, the engine's stop releases the session and every
		// goroutine of it ends, its producer's wait with them, while the
		// reconciliation is still held.
		pub := &publishHold{}
		d, w, stop, res, _, ob := atFloor(t, floorOpts{pub: pub})
		g := pub.arm(t)
		stop.letGo()
		awaitOrdered(t, g.in, "the write's set published, its reconciliation held")
		n := w.verdictWaits(ob)
		d.k.Free()
		w.waitVerdictWaits(t, ob, n+1, "b's transition, the drain ended, to wait for the reconciliation's verdict")
		stopped, cancel := context.WithTimeout(bg, alsapcm.Ordered())
		defer cancel()
		if err := d.eng.Stop(stopped); err != nil {
			alsapcm.LogGoroutines(t)
			t.Fatalf("the engine's stop with the reconciliation held: %v", err)
		}
		joined, cancelJoin := context.WithTimeout(bg, alsapcm.Ordered())
		defer cancelJoin()
		if err := d.be.Wait(joined); err != nil {
			alsapcm.LogGoroutines(t)
			t.Fatalf("the session's goroutines once the engine stopped it, the reconciliation held: %v", err)
		}
		g.letGo()
		written(t, res)
		d.rig.stop()
	})
	t.Run("b handed into a's sink before the write", func(t *testing.T) {
		// Round 16's ordering C5, forced: the producer let go before the write,
		// b joins a's sink gaplessly (the producer's write step for b), no drain
		// and no open. The write of DSD off without the go-ahead counts b, its
		// frames in a's sink: it answers would_interrupt, the choice unchanged,
		// and b plays as it was, never waiting for the write.
		d, w, p, _, ob := started(t, floorOpts{})
		p.gate.letGo()
		waitOrdered(t, "b's gapless join into a's sink, the producer's write step for b", p.ch, func() bool { return p.has("write", ob) })
		if v := d.k.View(); v.Drains != 0 || v.Opens != 1 {
			t.Fatalf("b's join: %d drains and %d opens, want none and a's open alone: b in a's sink", v.Drains, v.Opens)
		}
		if got := answerWithin(t, alsapcm.Ordered(), d.choose(choiceReq(t, d, dsdstate.ModeOff, 0))); got.Outcome != playback.ChoiceWouldInterrupt {
			t.Fatalf("DSD off without the go-ahead, b in a's sink: %s, want would_interrupt", got.Outcome)
		}
		if c, ok := d.store.State().DAC(playbacktest.DACKey()); ok && c.Choice != nil {
			t.Errorf("a write answered would_interrupt wrote a choice: %+v", c.Choice)
		}
		if n := w.awaits(ob); n != 0 {
			t.Errorf("b waited %d times for the write, want none: in a's sink, it plays", n)
		}
		d.k.Free()
		s := d.waitStopped(engine.ReasonEndOfQueue)
		d.rig.stop()
		evs := d.rec.events()
		if n := count(evs, "audible", 0); n != 2 || count(evs, "replan", 0) != 0 || len(s.Faults()) != 0 || len(s.Skips()) != 0 {
			t.Errorf("%d occurrences heard, want a and b as they were: %s; faults %+v, skips %+v", n, kinds(evs), s.Faults(), s.Skips())
		}
		if v := d.k.View(); v.Opens != 1 {
			t.Errorf("%d opens, want a's alone: b in a's sink", v.Opens)
		}
	})
}

// TestAWriteBesideAnEngineThatHandsNothingOffAgainIsTold (B728 B9 round 2,
// point 7): where the engine hands nothing off again (no
// engine.ReplanEvents) a re-plan is a revocation of the session's DSD, so a
// successor opened at a rate a write changes and in no sink is what the
// write would interrupt: without the go-ahead the write answers
// would_interrupt, through the engine's real Run, changing nothing.
func TestAWriteBesideAnEngineThatHandsNothingOffAgainIsTold(t *testing.T) {
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.noReplan = true })
	d.files.add("a", dsd(playbacktest.DSD64, 1))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	d.preparedOcc(ids[1])
	if got := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64))); got.Outcome != playback.ChoiceWouldInterrupt {
		t.Fatalf("the cap beside b, which the engine would not hand off again: %s, want would_interrupt", got.Outcome)
	}
	if c := d.store.State().DACs[0].Choice; c.MaxBitRate != 0 || c.Revision != 1 {
		t.Errorf("a write answered would_interrupt changed the choice: %+v", c)
	}
	d.k.Free()
	s := d.waitEnded()
	d.rig.stop()
	wantPlayedAsItWas(t, d, s)
}

// TestAWriteBesideASessionBeingReleasedIsTold (B728 B9 round 2, point 7): a
// session being released holds a successor opened at a rate a write changes,
// in no sink; reconciliation revokes a releasing session's DSD for such a
// re-plan, the engine handing nothing off again to a session it released,
// so the write counts it: without the go-ahead it answers would_interrupt,
// through the engine's real Run and Stop, changing nothing.
func TestAWriteBesideASessionBeingReleasedIsTold(t *testing.T) {
	w := newWriteSeams()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", dsd(playbacktest.DSD64, 1))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	d.preparedOcc(ids[1])
	rel := w.holdReleasing(t)
	stopped := make(chan error, 1)
	go func() { stopped <- d.eng.Stop(bg) }()
	awaitHeld(t, "the session's release", rel.in)
	got := d.be.ChooseDSD(bg, choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64)))
	rel.letGo()
	if got.Outcome != playback.ChoiceWouldInterrupt {
		t.Fatalf("the cap beside a session being released: %s, want would_interrupt", got.Outcome)
	}
	if c := d.store.State().DACs[0].Choice; c.MaxBitRate != 0 || c.Revision != 1 {
		t.Errorf("a write answered would_interrupt changed the choice: %+v", c)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("the stop: %v", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the stop was not answered within %v", alsapcm.Watchdog())
	}
}
