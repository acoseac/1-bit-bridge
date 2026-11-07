package alsapcm_test

import (
	"bytes"
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
	select {
	case <-g.in:
	case r := <-res:
		t.Fatalf("the write ended (%s) before %s", r.Outcome, what)
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
	}
}

// writeSeams are a rig's seams for writes of the bound DAC's choice beside
// playback: the next write held at its pending window (ChoicePending), or
// inside the state's write (inWrite, armed there); an open of a content held
// once it returned (Opened); a step's wake from a write held (ChoiceWoke); a
// session's release held (Releasing); and each wait for a write and each
// activation recorded.
type writeSeams struct {
	mu         sync.Mutex
	changed    chan struct{}
	pending    *gate
	inWrite    func()
	opened     map[string]*gate
	woke       map[engine.OccurrenceID]*gate
	activation map[engine.OccurrenceID]*gate
	releasing  *gate
	awaited    []engine.OccurrenceID
	activated  []engine.OccurrenceID
}

func newWriteSeams() *writeSeams {
	return &writeSeams{changed: make(chan struct{}), opened: map[string]*gate{}, woke: map[engine.OccurrenceID]*gate{},
		activation: map[engine.OccurrenceID]*gate{}}
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
	waitOn(t, fmt.Sprintf("a step of occurrence %d to wait for the write (%d)", o, n), w.ch, func() bool { return w.awaits(o) >= n })
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

// waitedOrActivated waits until a step of o's waited for a write or o was
// activated, whichever comes first.
func (w *writeSeams) waitedOrActivated(t *testing.T, o engine.OccurrenceID) {
	t.Helper()
	waitOn(t, fmt.Sprintf("occurrence %d to wait for the write or be activated", o), w.ch, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return slices.Contains(w.awaited, o) || slices.Contains(w.activated, o)
	})
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
	select {
	case r := <-res:
		return r
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the write was not answered within %v", alsapcm.Watchdog())
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
// reaching a sink, and the sink's adoption and the occurrence's activation
// wait for the write. Without the go-ahead the write answers would_interrupt,
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
			w.waitedOrActivated(t, ob)
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
// the producer is adopted and activated once the write settles, and b plays
// as it was.
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
	w.waitedOrActivated(t, ob)
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
func TestAConvertedJoinWindowWaitsForAWriteThatChangesItsSuccessor(t *testing.T) {
	// holding is a rig whose converted a, DSD64, is at its window point, its
	// successor f settled, a write that turns DSD off, with the go-ahead,
	// pending.
	holding := func(t *testing.T) (*dsdRig, *convSteps, *gate, <-chan playback.ChoiceResult, engine.OccurrenceID, engine.OccurrenceID) {
		t.Helper()
		w := newWriteSeams()
		steps := newConvSteps(t, "holding")
		d := newConvRig(t, func(o *rigOptions) { o.seams = w.seams(&playback.Seams{Converting: steps.seam}) })
		d.files.add("a", track(playbacktest.DSD64, 0.5, noise(1)))
		d.files.add("f", track(playbacktest.DSD64, 0.5, noise(2)))
		ids := d.insert("a", "f")
		d.play()
		of := d.preparedOcc(ids[1])
		steps.heldNow(t)
		oa := d.firstOcc()
		stop := w.holdPending(t)
		res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 0, goAhead))
		reachedGate(t, stop, res, "the write's pending window")
		steps.letGo()
		w.waitAwaited(t, of, 1)
		if steps.has("decided", oa) {
			t.Fatal("a's window was decided while the write was pending, its successor's rate changing")
		}
		return d, steps, stop, res, oa, of
	}
	t.Run("the write settling first", func(t *testing.T) {
		d, steps, stop, res, oa, _ := holding(t)
		stop.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the write's faults %+v, want the session's DSD revoked", f)
		}
		if steps.has("decided", oa) {
			t.Error("a's window was decided once its DSD was revoked")
		}
	})
	t.Run("the floor coming first", func(t *testing.T) {
		d, steps, stop, res, oa, _ := holding(t)
		d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
		// 450 ms of a's 500 played: the lookahead falls below its floor.
		d.k.Play(t, 450*time.Millisecond)
		d.tick()
		steps.wait(t, "decided", oa)
		stop.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
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

// TestAGaplessSuccessorWaitsForAWriteNoLongerThanTheSinksLookahead (B728 B9
// round 2): a successor on its predecessor's transport (PCM at 176.4 kHz / 24
// bits, then DSD64 converted to it) waits out of the sink for a write pending
// that changes its rate (DSD off) only while the sink holds more than its
// floor of lookahead: at the floor the sink drains and plays out what it
// holds, an honest gap, rather than run dry beside a producer that waits, and
// the successor waits for the write before a sink of its own opens; the
// write, which interrupts nothing, lands and hands it off again, nothing
// faulting.
func TestAGaplessSuccessorWaitsForAWriteNoLongerThanTheSinksLookahead(t *testing.T) {
	w := newWriteSeams()
	d := newConvRig(t, func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", wav(176400, 24, pattern(1, 176400/2, 24)))
	d.files.add("b", track(playbacktest.DSD64, 0.25, noise(2)))
	ids := d.insert("a", "b")
	d.play()
	ob := d.preparedOcc(ids[1])
	stop := w.holdPending(t)
	res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 0))
	reachedGate(t, stop, res, "the write's pending window")
	w.waitAwaited(t, ob, 1)
	// 400 ms of a's 500 played: the sink's lookahead at its floor, which the
	// reporter's next measurement reads. A producer that waited past it would
	// let the sink run dry; this one drains it for b's own transport.
	d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	d.k.Play(t, 400*time.Millisecond)
	d.tick()
	d.k.Await(t, "the sink to drain once its lookahead fell to its floor, b still waiting for the write", func(v alsapcm.KernelView) bool { return v.Drains >= 1 })
	d.k.Free()
	stop.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("DSD off beside a PCM track: %s, want written", got.Outcome)
	}
	s := d.waitEnded()
	d.rig.stop()
	if f := s.Faults(); len(f) != 0 {
		t.Errorf("faults %+v: the sink ran dry while b waited for the write", f)
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want a's alone: b handed off again, no sink opened for it", v.Opens)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped; skips %+v", s.Skips())
	}
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
