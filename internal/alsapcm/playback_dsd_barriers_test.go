package alsapcm_test

import (
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pausedBetween(t *testing.T, w *lifecycleSeams, pubs ...*publishHold) (*dsdRig, engine.OccurrenceID, []uint32) {
	t.Helper()
	tm := &timers{}
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) {
		o.seams = w.seams(nil)
		for _, p := range pubs {
			o.seams.Published = p.seam
		}
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
func resumeOrEnd(t *testing.T, resumed <-chan error) {
	t.Helper()
	select {
	case err := <-resumed:
		var e *engine.Error
		if err != nil && !(errors.As(err, &e) && e.Reason == engine.ReasonSuperseded && strings.HasSuffix(e.Detail, "the last entry played out")) {
			t.Fatalf("the resume: %v; want it answered, or superseded by the end of the queue", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the resume was not answered within %v", alsapcm.Watchdog())
	}
}

type producerHold struct {
	mu       sync.Mutex
	writes   int
	waitN    int
	steps    []string
	changed  chan struct{}
	gate     *gate
	waitHold *gate
}

func newProducerHold(t *testing.T) *producerHold {
	return &producerHold{changed: make(chan struct{}), gate: newGate(t)}
}

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
	var wg *gate
	if step == "wait" {
		wg, p.waitHold = p.waitHold, nil
	}
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	if hold {
		p.gate.pass()
	}
	if wg != nil {
		wg.pass()
	}
}

func (p *producerHold) holdWait(t *testing.T) *gate {
	g := newGate(t)
	p.mu.Lock()
	p.waitHold = g
	p.mu.Unlock()
	return g
}

func (p *producerHold) ch() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.changed
}

func (p *producerHold) has(step string, o engine.OccurrenceID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Contains(p.steps, fmt.Sprintf("%s %d", step, o))
}

func (p *producerHold) waits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitN
}

type publishHold struct {
	armed, panics atomic.Bool
	gate          *gate
}

func (h *publishHold) seam() {
	if !h.armed.CompareAndSwap(true, false) {
		return
	}
	if h.panics.Load() {
		panic(errPublishPanic)
	}
	h.gate.pass()
}

var errPublishPanic = errors.New("the test's panic between a refresh's publish and its reconciliation")

func (h *publishHold) armPanic() {
	h.panics.Store(true)
	h.armed.Store(true)
}

func (h *publishHold) arm(t *testing.T) *gate {
	h.gate = newGate(t)
	h.armed.Store(true)
	return h.gate
}

func TestAResumeBetweenTransportsWaitsForTheReconciliationsVerdict(t *testing.T) {
	w, pub := newLifecycleSeams(), &publishHold{}
	d, ob, ids := pausedBetween(t, w, pub)
	g := pub.arm(t)
	res := d.refreshAfterHotplug(false)
	awaitOrdered(t, g.in, "the refreshed set published, its reconciliation held")
	n, opens := w.verdictWaits(ob), d.k.View().Opens
	resumed := make(chan error, 1)
	go func() { resumed <- d.eng.Play(bg) }()
	waitOrderedEither(t, "the resume to wait for the reconciliation's verdict on b, or b's node to open", w.ch,
		func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens > opens }) },
		func() bool { return w.verdictWaits(ob) > n || d.k.View().Opens > opens })
	if v := d.k.View(); v.Opens != opens {
		t.Fatalf("the resume opened b's node (%d opens after %d) while the reconciliation that judges b was held", v.Opens, opens)
	}
	g.letGo()
	refreshed(t, res)
	resumeOrEnd(t, resumed)
	s := d.waitEnded()
	d.rig.stop()
	if f := s.Faults(); len(f) != 0 {
		t.Errorf("the hand-off of b faulted: %+v", f)
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want a's alone: no node opened for b", v.Opens)
	}
	if _, ok := s.Skipped(ids[1]); !ok {
		t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
	}
}
func TestNoSinkOpensBeforeReconciliationJudgesItsSuccessor(t *testing.T) {
	w, pub := newLifecycleSeams(), &publishHold{}
	drained := newGate(t)
	var once sync.Once
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) {
		o.seams = w.seams(&playback.Seams{Published: pub.seam, TransportDrained: func() { once.Do(drained.pass) }})
	})
	t.Cleanup(drained.letGo)
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("a", "b")
	d.play()
	ob := d.occurrenceOf(ids[1])
	d.k.Free()
	awaitHeld(t, "a's sink drained for b's transport", drained.in)
	stop := pub.arm(t)
	res := d.refreshAfterHotplug(false)
	reachedGate(t, stop, res, "the publish barrier")
	drained.letGo()
	waitOrderedEither(t, "judgement wait or premature open", w.ch, func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens > 1 }) }, func() bool { return w.verdictWaits(ob) > 0 || d.k.View().Opens > 1 })
	if v := d.k.View(); v.Opens != 1 {
		t.Fatalf("%d opens before reconciliation: a sink opened for b", v.Opens)
	}
	stop.letGo()
	refreshed(t, res)
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
func TestAGaplessSuccessorWaitsForJudgementOnlyAboveTheSinksFloor(t *testing.T) {
	const rate = 176400
	const aFrames, floor = rate / 2, rate / 10 // a's 500 ms, the sink's floor's 100 ms
	type floorOpts struct {
		pub      *publishHold
		lateStep bool
	}
	started := func(t *testing.T, fo floorOpts) (*dsdRig, *lifecycleSeams, *producerHold, []uint32, engine.OccurrenceID) {
		t.Helper()
		w, p := newLifecycleSeams(), newProducerHold(t)
		d := newConvRig(t, func(o *rigOptions) {
			o.seams = w.seams(nil)
			o.seams.ProducerStep = p.note
			if fo.pub != nil {
				o.seams.Published = fo.pub.seam
			}
		})
		t.Cleanup(p.gate.letGo)
		d.files.add("a", wav(rate, 24, pattern(1, aFrames, 24)))
		d.files.add("b", track(playbacktest.DSD64, 0.25, noise(2)))
		ids := d.insert("a", "b")
		d.play()
		awaitOrdered(t, p.gate.in, "the producer, held in a before it decides what follows a")
		ob := d.preparedOccWithin(ids[1], alsapcm.Ordered())
		return d, w, p, ids, ob
	}
	toFloor := func(t *testing.T, d *dsdRig, p *producerHold, ob engine.OccurrenceID, lateStep bool) {
		t.Helper()
		statusing, status := d.k.ParkStatusAfterPlayed(aFrames - floor)
		defer status()
		d.k.Grant(400 * time.Millisecond)
		awaitOrdered(t, statusing, "the sink's STATUS once the DAC played 400 ms of a")
		if v := d.k.View(); v.Consumed != aFrames-floor || v.Appl != aFrames {
			t.Fatalf("the DAC had played %d of the %d frames written at the STATUS held; want %d of a's %d, the sink at its floor (the test's premise)", v.Consumed, v.Appl, aFrames-floor, aFrames)
		}
		held := p.holdWait(t)
		d.tickOnce(alsapcm.Ordered())
		awaitOrdered(t, held.in, "the producer to hold b out of a's sink again at the reporter's step, the measurement at 350 ms, held at its wait")
		if p.has("transition", ob) || d.k.View().Drains != 0 {
			t.Fatal("the producer let b's wait go before the sink's measurement showed its floor")
		}
		status()
		awaitOrdered(t, d.k.When(runningAndWaiting), "the writer back on the DAC, the measurement at a's last frames established")
		if v := d.k.View(); v.Drains != 0 || p.has("transition", ob) {
			t.Fatalf("the sink drained (%d) before the producer, held at its wait, was let go", v.Drains)
		}
		if lateStep {
			d.tickOnce(alsapcm.Ordered())
			held.letGo()
			awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return v.Drains >= 1 }), "the sink draining for b's own transport, the producer woken by the late step")
		}
		awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return runningAndWaiting(v) || v.Drains >= 1 }),
			"the writer back on the DAC, or the sink draining, the measurement at a's last frames established")
		switch v := d.k.View(); {
		case lateStep && (v.Drains == 0 || runningAndWaiting(v)):
			t.Fatalf("the late step taken, the case's look found the writer back on the DAC (%v) and %d drains: want the sink draining", runningAndWaiting(v), v.Drains)
		case !lateStep && (v.Drains != 0 || !runningAndWaiting(v)):
			t.Fatalf("the producer held at its wait, the case's look found the writer back on the DAC %v and %d drains: want the writer waiting, nothing drained", runningAndWaiting(v), v.Drains)
		}
		held.letGo()
		d.tickWithin(alsapcm.Ordered())
		waitOrdered(t, "the producer's transition for b at the sink's measured floor", p.ch, func() bool { return p.has("transition", ob) })
		awaitOrdered(t, d.k.When(func(v alsapcm.KernelView) bool { return v.Drains >= 1 }), "the sink to drain at its floor, b still out of it")
	}
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

	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late reporter ", late), func(t *testing.T) {
			pub := &publishHold{}
			d, w, p, ids, ob := started(t, floorOpts{pub: pub, lateStep: late})
			g := pub.arm(t)
			done := d.refreshAfterHotplug(true)
			awaitOrdered(t, g.in, "the capabilities published before reconciliation")
			p.gate.letGo()
			w.waitVerdictWaits(t, ob, 1, "the gapless successor held out")
			awaitOrdered(t, d.k.When(runningAndWaiting), "the writer on the DAC")
			toFloor(t, d, p, ob, late)
			g.letGo()
			refreshed(t, done)
			d.k.Free()
			v := played(t, d, ids)
			if v.Opens != 1 {
				t.Fatalf("%d opens; want PCM alone", v.Opens)
			}
		})
	}
}
func TestAConvertedWindowWaitsForJudgementAndEndsZeroPaddedAtTheFloor(t *testing.T) {
	const (
		floorFrames = uint64(176400 / 10)
		aFrames     = uint64(176400 / 2)
		floorPlayed = uint64(4 * 176400 / 10)
	)
	type holdingOpts struct {
		log *stopsLog
		pub *publishHold
	}
	type window struct {
		d      *dsdRig
		w      *lifecycleSeams
		steps  *convSteps
		marks  *windowMarks
		p      *producerHold
		fileA  []byte
		oa, of engine.OccurrenceID
	}
	start := func(t *testing.T, ho holdingOpts) *window {
		t.Helper()
		h := &window{w: newLifecycleSeams(), steps: newConvSteps(t, "holding"), marks: newWindowMarks(), p: newProducerHold(t)}
		h.p.gate.letGo()
		h.d = newDSDRigWith(t, dsdstate.Defaults(), func(p *playback.DSDPolicy) {
			p.Profiles = nil
			if ho.log != nil {
				p.Log = ho.log
			}
		}, func(o *rigOptions) {
			o.seams = h.w.seams(&playback.Seams{Converting: h.steps.seam, ProducerStep: h.p.note})
			if ho.pub != nil {
				o.seams.Published = ho.pub.seam
			}
			h.marks.seams(o.seams)
		})
		t.Cleanup(h.steps.letGo)
		h.fileA = track(playbacktest.DSD64, 0.5, noise(1))
		h.d.files.add("a", h.fileA)
		h.d.files.add("f", track(playbacktest.DSD64, 0.5, noise(2)))
		ids := h.d.insert("a", "f")
		h.d.play()
		h.of = h.d.preparedOccWithin(ids[1], alsapcm.Ordered())
		h.steps.heldWithin(t, alsapcm.Ordered())
		h.oa = h.d.firstOcc()
		return h
	}
	aboveFloor := func(t *testing.T, h *window) {
		t.Helper()
		awaitOrdered(t, h.d.k.When(runningAndWaiting), "the writer to fill the hardware buffer and wait on the DAC")
		if v := h.d.k.View(); v.Appl-v.Consumed <= floorFrames {
			t.Fatalf("the kernel holds %d frames the DAC has not played, the floor %d: the sink's lookahead is not above its floor (the case's premise)", v.Appl-v.Consumed, floorFrames)
		}
	}
	atFloorEnded := func(t *testing.T, h *window, held string) {
		t.Helper()
		awaitOrdered(t, h.d.k.When(runningAndWaiting), "the writer to fill the hardware buffer and wait on the DAC")
		wantA, _ := converted(t, h.fileA, convPlan)
		c, err := dsdconv.New(dsdOf(t, h.fileA), convPlan, dsdconv.Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		given := aFrames - uint64(c.Window())
		_ = c.Close()
		if len(wantA) != 2*int(aFrames) {
			t.Fatalf("a converted alone is %d frames, want %d (the case's premise)", len(wantA)/2, aFrames)
		}
		statusing, status := h.d.k.ParkStatusAfterPlayed(floorPlayed)
		defer status()
		h.d.k.Grant(400 * time.Millisecond)
		awaitOrdered(t, statusing, "the sink's STATUS once the DAC played 400 ms of a")
		if v := h.d.k.View(); v.Consumed != floorPlayed || v.Appl != given || given-floorPlayed > floorFrames || given-(floorPlayed-floorPlayed/8) <= floorFrames {
			t.Fatalf("the DAC had played %d of the %d frames written at the STATUS held; want %d of the %d a gave before its window, under the floor %d there and above it at the measurement before (the case's premise)", v.Consumed, v.Appl, floorPlayed, given, floorFrames)
		}
		waitHeld := h.p.holdWait(t)
		h.d.tickOnce(alsapcm.Ordered())
		awaitOrdered(t, waitHeld.in, "the producer at its wait again, the measurement at 350 ms read, held there")
		if h.marks.decided(h.oa) {
			t.Fatalf("a's window was decided at the measurement before the floor's, above it; marks %v", h.marks.list())
		}
		status()
		awaitOrdered(t, h.d.k.When(runningAndWaiting), "the writer back on the DAC, the floor's measurement established")
		if h.marks.decided(h.oa) {
			t.Fatalf("a's window was decided while the producer was held at its wait; marks %v", h.marks.list())
		}
		waitHeld.letGo()
		h.d.tickOnce(alsapcm.Ordered())
		waitOrdered(t, "a's window ended at the first reporter step after the floor's measurement, "+held, h.marks.ch, func() bool { return h.marks.decided(h.oa) })
		awaitOrdered(t, h.d.k.When(func(v alsapcm.KernelView) bool { return v.Appl >= aFrames }), "a's window's frames given to the kernel")
		got := unpack(t, h.d.k.Given(), format.S243LE, 24)
		same(t, "what the kernel was given of a, against a converted alone, its window zero-padded", got[:len(wantA)], wantA)
	}

	pub := &publishHold{}
	h := start(t, holdingOpts{pub: pub})
	g := pub.arm(t)
	done := h.d.refreshAfterHotplug(true)
	awaitOrdered(t, g.in, "the changed capabilities published, reconciliation held")
	aboveFloor(t, h)
	h.steps.letGo()
	h.w.waitVerdictWaits(t, h.of, 1, "the window to wait for its successor's judgement")
	if h.marks.decided(h.oa) {
		t.Fatal("window decided before its successor was judged")
	}
	atFloorEnded(t, h, "reconciliation held")
	g.letGo()
	refreshed(t, done)
	h.d.k.Free()
	h.d.tickUntilStopped(engine.ReasonFault)
	h.d.rig.stop()
}

type windowMarks struct {
	mu      sync.Mutex
	changed chan struct{}
	ids     []engine.OccurrenceID
}

func newWindowMarks() *windowMarks         { return &windowMarks{changed: make(chan struct{})} }
func (m *windowMarks) ch() <-chan struct{} { m.mu.Lock(); defer m.mu.Unlock(); return m.changed }
func (m *windowMarks) decided(o engine.OccurrenceID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Contains(m.ids, o)
}
func (m *windowMarks) list() []engine.OccurrenceID {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.ids)
}
func (m *windowMarks) seams(s *playback.Seams) {
	prev := s.Converting
	s.Converting = func(step string, o engine.OccurrenceID) {
		if prev != nil {
			prev(step, o)
		}
		if step == "decided" {
			m.mu.Lock()
			m.ids = append(m.ids, o)
			close(m.changed)
			m.changed = make(chan struct{})
			m.mu.Unlock()
		}
	}
}
