package alsapcm_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// A Stop the engine has taken writes nothing, at every step of the session
// that would acquire, open, adopt, resume or write (B674 round 3): each test
// lands the Stop in its step's window, holding the step there and the
// engine's Release call short of the backend, so the backend learns of the
// release only through engine.StartRequest.Released, as a scheduler can leave
// it; the backend then refuses the step, which playback.Seams.Fenced says.

// stopWithItsReleaseHeld takes a Stop with its Release call held short of
// the backend: it returns once the engine's snapshot shows the session
// releasing toward stopped and the call waits, with the Stop's answer and
// what lets the call on.
func (r *rig) stopWithItsReleaseHeld() (stop <-chan error, letRelease func()) {
	r.t.Helper()
	held, letGo := r.rec.holdNextRelease()
	r.t.Cleanup(letGo)
	ch := make(chan error, 1)
	go func() { ch <- r.eng.Stop(bg) }()
	r.waitSnap("the stop taken", func(s *engine.Snapshot) bool {
		return s.State() == engine.Releasing && s.Pending() == engine.IntentStop
	})
	receiveIn(r.t, held, "the engine's release held short of the backend")
	return ch, letGo
}

// finishStop lets the held Release on, and waits for the Stop's answer and
// playback stopped.
func (r *rig) finishStop(stop <-chan error, letRelease func()) {
	r.t.Helper()
	letRelease()
	if err := receiveIn(r.t, stop, "the stop"); err != nil {
		r.t.Fatalf("stop: %v", err)
	}
	r.waitStopped(engine.ReasonStopped)
}

// wantFenced fails the test unless the backend refused exactly steps, in
// order.
func (r *rig) wantFenced(steps ...string) {
	r.t.Helper()
	if got := r.rec.fencedSteps(); !slices.Equal(got, steps) {
		r.t.Errorf("the steps refused for the release: %q, want %q", got, steps)
	}
}

// wantFencedWith fails the test unless the backend refused step, and nothing
// else but the steps also names: a producer still handing a's later chunks to
// the store when the Stop comes refuses its next write too, which the timing
// of a test's setup decides.
func (r *rig) wantFencedWith(step string, also ...string) {
	r.t.Helper()
	got := r.rec.fencedSteps()
	ok := slices.Contains(got, step)
	for _, g := range got {
		ok = ok && (g == step || slices.Contains(also, g))
	}
	if !ok {
		r.t.Errorf("the steps refused for the release: %q, want %q and none but %q beside it", got, step, also)
	}
}

// waitFencedOr waits until the backend has refused a step for the release,
// or cond holds.
func (r *rig) waitFencedOr(what string, cond func() bool) {
	r.t.Helper()
	waitOn(r.t, what, r.rec.ch, func() bool { return len(r.rec.fencedSteps()) > 0 || cond() })
}

// successorSubmitted reports whether a successor's frames reached a sink:
// the reporter reports Submitted for successors alone.
func (r *rig) successorSubmitted() bool { return count(r.rec.events(), "submitted", 0) > 0 }

// noteHold holds the n-th note of kind a backend makes, on the goroutine
// that makes it, until letGo: a rig takes its note as rigOptions.notify or
// rigOptions.installed.
type noteHold struct {
	kind       playback.NoteKind
	n          int
	mu         sync.Mutex
	seen       int
	held, gate chan struct{}
	once       sync.Once
}

func newNoteHold(kind playback.NoteKind, n int) *noteHold {
	return &noteHold{kind: kind, n: n, held: make(chan struct{}), gate: make(chan struct{})}
}

func (h *noteHold) note(nt playback.Note) {
	if nt.Kind != h.kind {
		return
	}
	h.mu.Lock()
	h.seen++
	hold := h.seen == h.n
	h.mu.Unlock()
	if hold {
		close(h.held)
		<-h.gate
	}
}

func (h *noteHold) letGo() { h.once.Do(func() { close(h.gate) }) }

// oneShot holds the first call that comes after it is armed, until letGo.
type oneShot struct {
	mu         sync.Mutex
	armed      bool
	held, gate chan struct{}
	once       sync.Once
}

func newOneShot() *oneShot { return &oneShot{held: make(chan struct{}), gate: make(chan struct{})} }

func (h *oneShot) arm() {
	h.mu.Lock()
	h.armed = true
	h.mu.Unlock()
}

func (h *oneShot) take() {
	h.mu.Lock()
	hold := h.armed
	h.armed = false
	h.mu.Unlock()
	if hold {
		close(h.held)
		<-h.gate
	}
}

func (h *oneShot) letGo() { h.once.Do(func() { close(h.gate) }) }

// transitionRig is a rig playing a, 1,000 frames at 44.1 kHz in 16 bits,
// shorter than the hardware buffer, before b, 3,000 frames at 48 kHz in 24
// bits, another transport, with a's sink playing out its last frames.
func transitionRig(t *testing.T, opts ...func(*rigOptions)) *rig {
	t.Helper()
	r := newRig(t, opts...)
	r.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	r.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
	r.insert("a", "b")
	r.play()
	r.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	return r
}

// TestPlaybackStartAcquiresNothingOnceItsReleaseIsTaken pins Start's outset:
// the engine's Start held short of the backend, the Stop taken, and the Start
// let on acquires nothing: no content opened, no device.
func TestPlaybackStartAcquiresNothingOnceItsReleaseIsTaken(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(44100, 16, pattern(1, 44100, 16)))
	r.insert("a")
	started, letStart := r.rec.holdNextStart()
	t.Cleanup(letStart)
	play := make(chan error, 1)
	go func() { play <- r.eng.Play(bg) }()
	receiveIn(t, started, "the engine's start held short of the backend")
	stop, letRelease := r.stopWithItsReleaseHeld()
	letStart()
	if start := r.waitCall("the start answered", func(c call) bool { return c.kind == "start" }); start.err == nil {
		t.Errorf("the start answered success once the engine took its release")
	}
	if n, v := r.files.openedCount(), r.k.View(); n != 0 || v.Opens != 0 {
		t.Errorf("the start opened %d contents and the device %d times once its release was taken; want nothing acquired", n, v.Opens)
	}
	r.wantFenced("start")
	r.finishStop(stop, letRelease)
	receiveIn(t, play, "the play")
}

// TestPlaybackStartOpensNoDeviceOnceItsReleaseIsTaken pins Start's open: the
// content's open held, the Stop taken, and the Start let on opens no device.
func TestPlaybackStartOpensNoDeviceOnceItsReleaseIsTaken(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(44100, 16, pattern(1, 44100, 16)))
	r.insert("a")
	opening, letOpen := r.files.holdOpen("a")
	t.Cleanup(letOpen)
	play := make(chan error, 1)
	go func() { play <- r.eng.Play(bg) }()
	receiveIn(t, opening, "the start's open of the content held")
	stop, letRelease := r.stopWithItsReleaseHeld()
	letOpen()
	if start := r.waitCall("the start answered", func(c call) bool { return c.kind == "start" }); start.err == nil {
		t.Errorf("the start answered success once the engine took its release")
	}
	if v := r.k.View(); v.Opens != 0 {
		t.Errorf("the start opened the device %d times once its release was taken; want none", v.Opens)
	}
	r.wantFenced("open")
	r.finishStop(stop, letRelease)
	receiveIn(t, play, "the play")
}

// TestPlaybackStartClosesItsSinkAtOnceOnceItsReleaseIsTaken pins Start's
// adoption: its sink opened, checked and noted, the Stop taken there, and the
// Start closes the sink at once, unwritten, with its release still held.
func TestPlaybackStartClosesItsSinkAtOnceOnceItsReleaseIsTaken(t *testing.T) {
	opened := newNoteHold(playback.NoteOpened, 1)
	r := newRig(t, func(o *rigOptions) { o.notify = opened.note })
	t.Cleanup(opened.letGo)
	r.files.add("a", wav(44100, 16, pattern(1, 44100, 16)))
	r.insert("a")
	play := make(chan error, 1)
	go func() { play <- r.eng.Play(bg) }()
	receiveIn(t, opened.held, "the start's sink opened and noted, before the start adopts it")
	if v := r.k.View(); !v.Open || v.Opens != 1 || v.GivenBytes != 0 {
		t.Fatalf("the start's sink held: open %v after %d opens, given %d bytes; want open and unwritten", v.Open, v.Opens, v.GivenBytes)
	}
	stop, letRelease := r.stopWithItsReleaseHeld()
	opened.letGo()
	if start := r.waitCall("the start answered", func(c call) bool { return c.kind == "start" }); start.err == nil {
		t.Errorf("the start answered success once the engine took its release")
	}
	if v := r.k.View(); v.Open || v.Closes != 1 || v.GivenBytes != 0 {
		t.Errorf("the start's sink, its release held: open %v after %d closes, given %d bytes; want closed at once, unwritten", v.Open, v.Closes, v.GivenBytes)
	}
	r.wantFenced("adopt")
	r.finishStop(stop, letRelease)
	receiveIn(t, play, "the play")
}

// TestPlaybackOpensNoNextTransportOnceTheReleaseIsTaken pins the producer's
// open of another transport's sink: a's sink played out and held there, the
// Stop taken, and the producer let on opens nothing for b.
func TestPlaybackOpensNoNextTransportOnceTheReleaseIsTaken(t *testing.T) {
	drained := newOneShot()
	drained.arm()
	r := transitionRig(t, func(o *rigOptions) {
		o.seams = &playback.Seams{TransportDrained: drained.take}
	})
	t.Cleanup(drained.letGo)
	r.k.Grant(time.Second)
	receiveIn(t, drained.held, "the producer holding a's played-out sink, before b's open")
	r.k.Revoke()
	stop, letRelease := r.stopWithItsReleaseHeld()
	drained.letGo()
	r.waitFencedOr("the producer's open of b", r.successorSubmitted)
	if v := r.k.View(); v.Opens != 1 {
		t.Errorf("the producer opened the device %d times once the engine took the stop; want a's open alone", v.Opens)
	}
	r.wantFenced("open")
	r.finishStop(stop, letRelease)
}

// TestPlaybackClosesTheNextTransportsSinkAtOnceOnceTheReleaseIsTaken pins the
// producer's adoption of another transport's sink: b's sink opened, checked
// and noted, the Stop taken there, and the producer closes it at once,
// unwritten, with the release still held.
func TestPlaybackClosesTheNextTransportsSinkAtOnceOnceTheReleaseIsTaken(t *testing.T) {
	opened := newNoteHold(playback.NoteOpened, 2)
	r := transitionRig(t, func(o *rigOptions) { o.notify = opened.note })
	t.Cleanup(opened.letGo)
	r.k.Grant(time.Second)
	receiveIn(t, opened.held, "b's sink opened and noted, before the producer adopts it")
	r.k.Revoke()
	if v := r.k.View(); !v.Open || v.Opens != 2 || len(v.OpenAt) != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Fatalf("b's sink held: open %v after %d opens, given %d bytes from %v; want open and unwritten", v.Open, v.Opens, v.GivenBytes, v.OpenAt)
	}
	stop, letRelease := r.stopWithItsReleaseHeld()
	opened.letGo()
	r.waitFencedOr("the producer's adoption of b's sink", r.successorSubmitted)
	if v := r.k.View(); v.Open || v.Closes != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Errorf("b's sink, the release held: open %v after %d closes, given %d bytes from %v; want closed at once, unwritten", v.Open, v.Closes, v.GivenBytes, v.OpenAt)
	}
	r.wantFenced("adopt")
	r.finishStop(stop, letRelease)
}

// TestPlaybackReopensNothingForAResumeOnceTheReleaseIsTaken pins a resume's
// reopen of a sink its pause released: the reopen held in its check after
// open, the node open and unwritten, the Stop taken, and the check refuses
// the node, which the sink closes unwritten; the resume answers refused.
func TestPlaybackReopensNothingForAResumeOnceTheReleaseIsTaken(t *testing.T) {
	r, _ := pausedRig(t)
	r.releaseDevice()
	held, admit := r.unit.holdNextAdmit()
	t.Cleanup(admit)
	resume := make(chan error, 1)
	go func() { resume <- r.eng.Play(bg) }()
	receiveIn(t, held, "the resume's reopen held in its check after open")
	if v := r.k.View(); !v.Open || v.Opens != 2 || len(v.OpenAt) != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Fatalf("the reopen held: open %v after %d opens, given %d bytes from %v; want open and unwritten", v.Open, v.Opens, v.GivenBytes, v.OpenAt)
	}
	stop, letRelease := r.stopWithItsReleaseHeld()
	admit()
	if c := r.waitCall("the resume answered", func(c call) bool { return c.kind == "resume" }); c.err == nil {
		// It wrote: the reopened sink writes the frames it kept first, which
		// the device takes while the release is held.
		r.k.Await(t, "the reopened device written", func(v alsapcm.KernelView) bool { return len(v.OpenAt) == 2 && v.GivenBytes > v.OpenAt[1] })
		t.Errorf("the resume answered %s once the engine took the stop", c.how)
	}
	if v := r.k.View(); v.Open || v.Closes != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Errorf("the reopen, the release held: open %v after %d closes, given %d bytes from %v; want closed unwritten", v.Open, v.Closes, v.GivenBytes, v.OpenAt)
	}
	r.wantFencedWith("admit", "write")
	r.finishStop(stop, letRelease)
	receiveIn(t, resume, "the resume's play")
}

// TestPlaybackResumesNoSinkOnceTheReleaseIsTaken pins a resume of a sink its
// pause holds: the resume's command held where it is called, before it runs
// on its own goroutine, the Stop taken, and the command let on resumes
// nothing: the stream stays paused, given nothing more.
func TestPlaybackResumesNoSinkOnceTheReleaseIsTaken(t *testing.T) {
	cmd := newOneShot()
	r, _ := pausedRig(t, func(o *rigOptions) {
		o.seams = &playback.Seams{SinkCall: func(ctx context.Context) func(error) {
			// Only a call the engine bounds, the resume's: the producer's
			// drain, which a pause refuses and the producer asks again, runs
			// on the session's life, which has no deadline, and can come
			// after the hold is armed.
			if _, ok := ctx.Deadline(); ok {
				cmd.take()
			}
			return nil
		}}
	})
	t.Cleanup(cmd.letGo)
	v := r.k.View()
	if v.State != "PAUSED" {
		t.Fatalf("paused: the stream is %s", v.State)
	}
	given := v.GivenBytes
	cmd.arm()
	resume := make(chan error, 1)
	go func() { resume <- r.eng.Play(bg) }()
	receiveIn(t, cmd.held, "the resume's command held before it runs")
	stop, letRelease := r.stopWithItsReleaseHeld()
	cmd.letGo()
	if c := r.waitCall("the resume answered", func(c call) bool { return c.kind == "resume" }); c.err == nil {
		t.Errorf("the resume answered %s once the engine took the stop", c.how)
	}
	if v := r.k.View(); v.State != "PAUSED" || v.GivenBytes != given {
		t.Errorf("the stream, the release held: %s, given %d bytes more; want still paused, given nothing", v.State, v.GivenBytes-given)
	}
	r.wantFencedWith("resume", "write")
	r.finishStop(stop, letRelease)
	receiveIn(t, resume, "the resume's play")
}

// TestPlaybackResumeBetweenTransportsAdoptsNothingOnceTheReleaseIsTaken pins
// a resume's open of the next transport's sink, paused between two
// transports with the device let go: b's sink opened, checked and noted, the
// Stop taken there, and the resume closes it at once, unwritten, hands the
// producer nothing and answers refused.
func TestPlaybackResumeBetweenTransportsAdoptsNothingOnceTheReleaseIsTaken(t *testing.T) {
	tm := &timers{}
	opened := newNoteHold(playback.NoteOpened, 2)
	r := transitionRig(t, func(o *rigOptions) { o.pauseRelease, o.afterFunc, o.notify = time.Second, tm.afterFunc, opened.note })
	t.Cleanup(opened.letGo)
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("a pause during the last frames of a transport: %v", err)
	}
	r.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	r.k.Free()
	r.k.Await(t, "a's sink to close", func(v alsapcm.KernelView) bool { return v.Closes == 1 && !v.Open })
	tm.fire(t)
	r.waitSnap("the device released", func(s *engine.Snapshot) bool { return s.DeviceReleased() })
	resume := make(chan error, 1)
	go func() { resume <- r.eng.Play(bg) }()
	receiveIn(t, opened.held, "b's sink opened by the resume and noted, before the resume hands it over")
	if v := r.k.View(); !v.Open || v.Opens != 2 || len(v.OpenAt) != 2 || v.OpenAt[1] != v.GivenBytes {
		t.Fatalf("b's sink held: open %v after %d opens, given %d bytes from %v; want open and unwritten", v.Open, v.Opens, v.GivenBytes, v.OpenAt)
	}
	stop, letRelease := r.stopWithItsReleaseHeld()
	opened.letGo()
	if c := r.waitCall("the resume answered", func(c call) bool { return c.kind == "resume" }); c.err == nil {
		t.Errorf("the resume answered %s once the engine took the stop", c.how)
	}
	if v := r.k.View(); v.Open || v.Closes != 2 || v.GivenBytes != 1000*2*2 {
		t.Errorf("b's sink, the release held: open %v after %d closes, %d bytes given; want closed at once, a's frames alone given", v.Open, v.Closes, v.GivenBytes)
	}
	r.wantFenced("adopt")
	r.finishStop(stop, letRelease)
	receiveIn(t, resume, "the resume's play")
}

// TestPlaybackHandsTheSinkNothingOnceTheReleaseIsTaken pins the producer's
// writes after the first chunk: b, a's gapless successor, held at its install
// before its first write, the Stop taken, and the producer let on hands the
// sink nothing of b.
func TestPlaybackHandsTheSinkNothingOnceTheReleaseIsTaken(t *testing.T) {
	installed := newNoteHold(playback.NoteInstalled, 2)
	r := newRig(t, func(o *rigOptions) { o.installed = installed.note })
	t.Cleanup(installed.letGo)
	a, b := pattern(1, 1000, 16), pattern(2, 1000, 16)
	r.files.add("a", wav(44100, 16, a))
	r.files.add("b", wav(44100, 16, b))
	r.insert("a", "b")
	r.play()
	receiveIn(t, installed.held, "the producer holding b's install, before its first write")
	stop, letRelease := r.stopWithItsReleaseHeld()
	installed.letGo()
	r.waitFencedOr("the producer's write of b", r.successorSubmitted)
	if r.successorSubmitted() {
		t.Errorf("the producer handed the sink b's frames once the engine took the stop")
	}
	r.wantFenced("write")
	r.finishStop(stop, letRelease)
	if got := unpack(t, r.k.Given(), format.S16LE, 16); len(got) > len(a) {
		t.Errorf("the device was given %d samples; want a's %d at most", len(got), len(a))
	}
}

// TestPlaybackCutsNothingOnceTheReleaseIsTaken pins a Cut the engine asked
// before it took the release: b submitted behind a, a second long, so b's
// frames are in the sink's store and none in the kernel, its deletion's Cut
// held short of the backend, the Stop taken, and the Cut let on cuts nothing.
func TestPlaybackCutsNothingOnceTheReleaseIsTaken(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(44100, 16, pattern(1, 44100, 16)))
	r.files.add("b", wav(44100, 16, pattern(2, 1000, 16)))
	ids := r.insert("a", "b")
	r.play()
	ob := r.submittedOcc(ids[1])
	// The engine has taken b's Submitted, so the deletion asks for a Cut:
	// one that came first would replace b, not cut it.
	r.handedOff(ob)
	held, letCut := r.rec.holdNextCut()
	t.Cleanup(letCut)
	deleted := make(chan error, 1)
	go func() { deleted <- r.eng.Delete(bg, ids[1]) }()
	receiveIn(t, held, "the engine's cut of b held short of the backend")
	stop, letRelease := r.stopWithItsReleaseHeld()
	letCut()
	if c := r.waitCall("the cut answered", func(c call) bool { return c.kind == "cut" && c.from == ob }); c.ok || c.err == nil {
		t.Errorf("the cut answered ok %v (%v) once the engine took the stop; want refused, nothing cut", c.ok, c.err)
	}
	r.wantFenced("cut")
	r.finishStop(stop, letRelease)
	receiveIn(t, deleted, "the deletion")
}

// TestPlaybackResumesASessionWhoseReleaseIsNotTaken pins that the fence is
// the release's alone: a session paused and resumed, then paused, let go of
// the device and resumed again, plays every frame once, and the backend
// refuses nothing.
func TestPlaybackResumesASessionWhoseReleaseIsNotTaken(t *testing.T) {
	r, a := pausedRig(t)
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s := r.waitSnap("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if how, _ := s.LastResume(); how != engine.ResumeContinued {
		t.Errorf("the resume went on %s, want continued", how)
	}
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("pause: %v", err)
	}
	r.waitSnap("paused again", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	r.releaseDevice()
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume after the release: %v", err)
	}
	s = r.waitSnap("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if how, _ := s.LastResume(); how != engine.ResumeReopened {
		t.Errorf("the resume after the release went on %s, want reopened", how)
	}
	r.heardAll(format.S243LE, 24, a)
	r.wantFenced()
}
