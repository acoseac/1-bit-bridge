package alsapcm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// TestPlaybackRefusesAStartBesideAnotherSession pins the backend's own
// guard of the contract's first term: a Start while another session still
// holds the device is refused (ErrOverlap, counted), and opens nothing, so
// an engine that broke the order fails a test rather than meet the kernel's
// busy refusal of the bridge's second open of its own DAC; once the first
// session's release is done, a Start opens.
func TestPlaybackRefusesAStartBesideAnotherSession(t *testing.T) {
	d := newDirect(t, nil)
	d.files.add("a", wav(48000, 24, pattern(1, 30000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	if err := d.start(2, "a"); !errors.Is(err, playback.ErrOverlap) {
		t.Fatalf("a Start beside session 1: %v, want ErrOverlap", err)
	}
	if st, v := d.be.Stats(), d.k.View(); st.Overlaps != 1 || v.Opens != 1 {
		t.Errorf("after the refused Start: %+v and %d opens; want one overlap and session 1's open alone", st, v.Opens)
	}
	d.release(1)
	d.release(2)
	if err := d.start(3, "a"); err != nil {
		t.Fatalf("a Start once session 1 released: %v", err)
	}
	d.release(3)
	d.k.CheckClosedEach(t)
	d.files.allClosed(t)
}

// TestPlaybackReleaseDuringStartsOpenClosesItUnwritten pins a Release that
// comes while Start opens the device (a Stop during the open): what Start
// opened is closed at once and never written to, and the release is
// acknowledged only once it is closed.
func TestPlaybackReleaseDuringStartsOpenClosesItUnwritten(t *testing.T) {
	opened, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	d := newDirect(t, func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
		return func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			s, err := open(ctx, cfg)
			once.Do(func() { close(opened) })
			<-gate
			return s, err
		}
	})
	d.files.add("a", wav(48000, 24, pattern(1, 30000, 24)))
	started := make(chan error, 1)
	go func() { started <- d.start(1, "a") }()
	receiveIn(t, opened, "the device to open")
	released := make(chan error, 1)
	go func() { released <- d.be.Release(bg, 1) }()
	// The release ends the session's life, and the sink it opened with it.
	d.k.Await(t, "the opened sink to close with the session's life", func(v alsapcm.KernelView) bool { return v.Closes == 1 })
	select {
	case err := <-released:
		t.Fatalf("the release was acknowledged (%v) while Start still held what it opened", err)
	default:
	}
	close(gate)
	if err := receiveIn(t, started, "Start to return"); err == nil {
		t.Errorf("Start answered nil for a session released during its open")
	}
	if err := receiveIn(t, released, "the release"); err != nil {
		t.Errorf("Release: %v", err)
	}
	if v := d.k.View(); v.Writes != 0 || v.GivenBytes != 0 || v.Opens != 1 || v.Closes != 1 {
		t.Errorf("a sink opened as its session was released: %d writes, %d bytes, %d opens, %d closes; want one open, closed, never written to", v.Writes, v.GivenBytes, v.Opens, v.Closes)
	}
}

// TestPlaybackReleaseBeforeItsStartOpensNothing pins a Release that reaches
// the backend before its Start (each call is a goroutine of its own): it is
// acknowledged at once, and the Start that follows opens nothing, not even
// the content.
func TestPlaybackReleaseBeforeItsStartOpensNothing(t *testing.T) {
	d := newDirect(t, nil)
	d.files.add("a", wav(48000, 24, pattern(1, 3000, 24)))
	d.release(7)
	if err := d.start(7, "a"); err == nil {
		t.Fatal("a Start after its release answered nil")
	}
	d.files.mu.Lock()
	contents := len(d.files.opened)
	d.files.mu.Unlock()
	if v := d.k.View(); v.Opens != 0 || contents != 0 {
		t.Errorf("a Start after its release opened %d nodes and %d contents, want none", v.Opens, contents)
	}
	if st := d.be.Stats(); st.Sessions != 0 {
		t.Errorf("%d sessions left after the Start that came after its release", st.Sessions)
	}
}

// TestPlaybackReleaseThatCannotFinishAcknowledgesLater pins a release the
// kernel holds up: the device's write is stuck in the kernel call, so the
// sink cannot close; Release answers its context's error at its end, holds
// the teardown (counted), and calls Events.Released once, when the kernel
// lets go and the device is closed.
func TestPlaybackReleaseThatCannotFinishAcknowledgesLater(t *testing.T) {
	d := newDirect(t, nil)
	parked, release := d.k.ParkNextWrite()
	t.Cleanup(release)
	d.files.add("a", wav(48000, 24, pattern(1, 30000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	receiveIn(t, parked, "the writer's first write to stick in the kernel")
	ctx, cancel := context.WithCancel(bg)
	cancel()
	if err := d.be.Release(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Release past its context: %v, want the context's error", err)
	}
	if n := count(d.rec.events(), "released", 0); n != 0 {
		t.Fatalf("Released before the device was let go")
	}
	if st := d.be.Stats(); st.Goroutines == 0 {
		t.Errorf("the teardown held by the kernel is not counted: %+v", st)
	}
	release()
	waitOn(t, "the late acknowledgement", d.rec.ch, func() bool { return count(d.rec.events(), "released", 0) == 1 })
	if v := d.k.View(); v.Open {
		t.Errorf("the device is still open after the late acknowledgement")
	}
	d.k.CheckClosedEach(t)
}

// TestPlaybackReportsEachStageOnceInOrder pins the stages, through the
// engine: each successor is Prepared, Submitted and Audible once each, in
// that order; the first occurrence, whose Prepared and Submitted are its
// Start's answer, is Audible once; Finished comes once, after them all.
func TestPlaybackReportsEachStageOnceInOrder(t *testing.T) {
	r := newRig(t)
	r.k.Free()
	for i, frames := range []int{3000, 4000, 5000} {
		r.files.add(string(rune('a'+i)), wav(48000, 24, pattern(i, frames, 24)))
	}
	ids := r.insert("a", "b", "c")
	r.play()
	s := r.waitStopped(engine.ReasonEndOfQueue)
	if s.Selected() != ids[2] {
		t.Errorf("stopped on entry %d, want the last, %d", s.Selected(), ids[2])
	}
	evs := r.rec.events()
	var occs []engine.OccurrenceID
	for _, e := range evs {
		if e.kind == "audible" {
			occs = append(occs, e.o)
		}
	}
	if len(occs) != 3 {
		t.Fatalf("audible %v, want three occurrences: %s", occs, kinds(evs))
	}
	at := func(kind string, o engine.OccurrenceID) []int {
		var is []int
		for i, e := range evs {
			if e.kind == kind && e.o == o {
				is = append(is, i)
			}
		}
		return is
	}
	if p, sub := at("prepared", occs[0]), at("submitted", occs[0]); len(p) != 0 || len(sub) != 0 {
		t.Errorf("the first occurrence was reported prepared %v and submitted %v; its Start answers both", p, sub)
	}
	for _, o := range occs[1:] {
		p, sub, a := at("prepared", o), at("submitted", o), at("audible", o)
		if len(p) != 1 || len(sub) != 1 || len(a) != 1 || !(p[0] < sub[0] && sub[0] < a[0]) {
			t.Errorf("occurrence %d: prepared %v, submitted %v, audible %v; want once each, in that order", o, p, sub, a)
		}
	}
	if f := at("finished", 0); len(f) != 1 || f[0] < at("audible", occs[2])[0] {
		t.Errorf("finished at %v; want once, after the last audible: %s", f, kinds(evs))
	}
}

// TestPlaybackCrossesSeveralShortEntriesInOneMeasurement pins the boundaries
// that one measurement crosses at once: three entries of 100 frames each and
// the start of a fourth, all inside the first period the DAC plays, are
// each reported Audible, in order, in one step (no position between them),
// and the engine's selection ends on the fourth. Nothing is audible while
// the DAC has played nothing, though every one of their decoders has ended.
func TestPlaybackCrossesSeveralShortEntriesInOneMeasurement(t *testing.T) {
	r := newRig(t)
	for i := range 3 {
		r.files.add(string(rune('a'+i)), wav(44100, 16, pattern(i, 100, 16)))
	}
	r.files.add("d", wav(44100, 16, pattern(3, 20000, 16)))
	ids := r.insert("a", "b", "c", "d")
	r.play()
	r.waitEvents("d's first frames in the sink", func(evs []event) bool { return count(evs, "submitted", 0) == 3 })
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.tick()
	if n := count(r.rec.events(), "audible", 0); n != 0 {
		t.Fatalf("%d boundaries reported before the DAC played a frame", n)
	}
	r.k.Play(t, 50*time.Millisecond)
	r.tick()
	evs := r.rec.events()
	var run []event
	for i, e := range evs {
		if e.kind == "audible" {
			run = evs[i:]
			break
		}
	}
	if len(run) < 5 || run[0].kind != "audible" || run[1].kind != "audible" || run[2].kind != "audible" || run[3].kind != "audible" || run[4].kind != "position" {
		t.Fatalf("the events from the first boundary: %s; want four audible, then the position", kinds(run))
	}
	if !slices.IsSorted([]engine.OccurrenceID{run[0].o, run[1].o, run[2].o, run[3].o}) || run[4].o != run[3].o {
		t.Errorf("crossed out of order: %s", kinds(run[:5]))
	}
	r.waitSnap("the selection on the fourth entry", func(s *engine.Snapshot) bool { return s.Selected() == ids[3] })
}

// TestPlaybackRepeatsAOneEntryQueueAsTwoOccurrences pins repeat with one
// entry: the engine hands the same entry off again as another occurrence,
// which the backend opens afresh and joins gaplessly, so the DAC hears the
// entry twice back to back, and each occurrence is audible once.
func TestPlaybackRepeatsAOneEntryQueueAsTwoOccurrences(t *testing.T) {
	r := newRig(t)
	r.k.Free()
	a := pattern(1, 5000, 16)
	r.files.add("a", wav(44100, 16, a))
	r.insert("a")
	if err := r.eng.SetRepeat(bg, true); err != nil {
		t.Fatal(err)
	}
	r.play()
	evs := r.waitEvents("two occurrences audible", func(evs []event) bool { return count(evs, "audible", 0) >= 2 })
	r.k.Await(t, "the DAC to play both occurrences", func(v alsapcm.KernelView) bool { return v.Consumed >= 2*5000 })
	if err := r.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	var audible []engine.OccurrenceID
	for _, e := range evs {
		if e.kind == "audible" {
			audible = append(audible, e.o)
		}
	}
	if audible[0] == audible[1] {
		t.Fatalf("one occurrence audible twice: %s", kinds(evs))
	}
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	if len(got) < 2*len(a) || sumOf(got[:len(a)]) != sumOf(a) || sumOf(got[len(a):2*len(a)]) != sumOf(a) {
		t.Errorf("the DAC did not hear the entry twice back to back (%d samples)", len(got))
	}
}

// TestPlaybackRetiresWhatHasPlayed pins what a session keeps as it plays: a
// queue of two entries of different transports on repeat changes transport
// at every entry, and every change opens a sink, whose ring is megabytes at
// high rates. The sinks closed and the occurrences played, their events
// reported, are let go, so however long the session plays it holds the
// sink that plays and the one before it at most, and the occurrences from
// the one heard on.
//
// That is what a report step leaves, not the count at every instant: once
// a sink has drained, the producer opens the next transport's before the
// step that hears what the drained one played, and a count read between
// the two finds a third sink (B532, CI's "3 sinks and 3 occurrences"). So
// the reporter is held once the engine has a Submitted until that sink is
// open, which makes the moment certain, and the count is read where the
// bound holds: the DAC stopped (Hold) and the producer draining the sink it
// wrote last with no time to do it in. It drains only once the engine has
// named what follows, which it does on the Submitted of a report step that
// retired everything before the one heard.
func TestPlaybackRetiresWhatHasPlayed(t *testing.T) {
	r := newRig(t, func(o *rigOptions) { o.realTick = true })
	r.k.Free()
	r.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	r.files.add("b", wav(48000, 24, pattern(2, 1000, 24)))
	r.insert("a", "b")
	if err := r.eng.SetRepeat(bg, true); err != nil {
		t.Fatal(err)
	}
	r.play()
	evs := r.waitEvents("ten boundaries, each a change of transport", func(evs []event) bool { return count(evs, "audible", 0) >= 10 })
	h := r.rec.holdAfterNext("submitted")
	t.Cleanup(h.letGo)
	receiveIn(t, h.held, "the reporter to hold once the engine has a Submitted")
	waitOn(t, "the next transport's sink to open while no report step runs", r.rec.ch, func() bool { return r.rec.openedSinks() > h.opened })
	r.k.Hold()
	h.letGo()
	r.k.Await(t, "the producer to drain the sink it wrote last, with the DAC stopped", func(v alsapcm.KernelView) bool {
		return v.State == "DRAINING" && v.Starved
	})
	if st := r.be.Stats(); st.Sinks > 2 || st.Occurrences > 3 {
		t.Errorf("ten entries in, the backend holds %d sinks and %d occurrences; want the sink that plays and the one before it at most, and the occurrences from the one heard on", st.Sinks, st.Occurrences)
	}
	// The first occurrence is retired: a cut of it is too late, never in time.
	var first event
	for _, e := range evs {
		if e.kind == "audible" {
			first = e
			break
		}
	}
	if ok, err := r.be.Cut(bg, first.s, first.o); ok || err != nil {
		t.Errorf("a cut of the retired occurrence %d: %v, %v; want false: it played", first.o, ok, err)
	}
	if err := r.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
}

// TestPlaybackRetiresNothingAnOpenerStillHolds pins retirement beside an
// open that does not end: b, a successor whose open is held in a read of its
// content that no context ends (a file on a mount that hangs), is replaced by
// c, so b is dropped and its open cancelled, which the held read does not
// notice. c, then d and e after a change of transport, are heard while b's
// opener still holds b, and each step that hears one retires everything
// before it but b: until an open ends the fields it sets are its opener's,
// written outside the session's lock, and retirement reads none of them. So
// an open that never ends holds back nothing else: the session holds b and
// what plays, and lets go of the sink a and c played through once d's
// plays. b's read then returns while e plays, and b leaves the session with
// its opener.
func TestPlaybackRetiresNothingAnOpenerStillHolds(t *testing.T) {
	d := newDirect(t, nil)
	for i, ref := range []string{"a", "b", "c"} {
		d.files.add(ref, wav(44100, 16, pattern(i+1, 22050, 16)))
	}
	// After c, a transport of its own: d's half second, then e, long enough
	// to play on while the test looks.
	d.files.add("d", wav(48000, 24, pattern(4, 24000, 24)))
	d.files.add("e", wav(48000, 24, pattern(5, 480000, 24)))
	parked, release := d.files.stall("b", 0)
	t.Cleanup(release)
	holds := func(when string, occurrences, sinks int) {
		t.Helper()
		if st := d.be.Stats(); st.Occurrences != occurrences || st.Sinks != sinks {
			t.Errorf("%s: the backend holds %d occurrences and %d sinks; want %d and %d", when, st.Occurrences, st.Sinks, occurrences, sinks)
		}
	}
	heard := func(o engine.OccurrenceID) {
		t.Helper()
		if n := count(d.rec.events(), "audible", o); n != 1 {
			t.Fatalf("occurrence %d audible %d times: %s", o, n, kinds(d.rec.events()))
		}
	}
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	ob, oc := occurrence(102, 2, "b"), occurrence(103, 3, "c")
	if err := d.be.Prepare(bg, 1, 101, &ob); err != nil {
		t.Fatal(err)
	}
	receiveIn(t, parked, "b's open to be held in a read of its content")
	if err := d.be.Prepare(bg, 1, 101, &oc); err != nil {
		t.Fatalf("c in b's place: %v", err)
	}
	d.files.fed(t, "c")
	d.k.Await(t, "the stream to run and its writer to wait on the DAC", runningAndWaiting)
	// a's 0.5 s and a tenth of c's.
	d.k.Play(t, 600*time.Millisecond)
	d.tick()
	heard(103)
	holds("c heard, b's open under way: a retired, b its opener's, c playing", 2, 1)
	od, oe := occurrence(104, 4, "d"), occurrence(105, 5, "e")
	if err := d.be.Prepare(bg, 1, 103, &od); err != nil {
		t.Fatal(err)
	}
	if err := d.be.Prepare(bg, 1, 104, &oe); err != nil {
		t.Fatal(err)
	}
	// c plays out for d's transport, then d and e play on d's sink, as fast
	// as the writer's waits need: once every frame of e is in the sink, the
	// played content is past d's.
	d.k.Free()
	d.files.fed(t, "e")
	d.tick()
	heard(104)
	heard(105)
	holds("d and e heard, b's open still under way: c, d and the sink before d's let go", 2, 1)
	// The next step hears e again and passes over b once more, the last
	// occurrence before e: the watermark it leaves must stay at d, never fall
	// back to b, or a cut of what already played would be answered as one
	// that could still be made.
	d.tick()
	for _, o := range []engine.OccurrenceID{103, 104} {
		if ok, err := d.be.Cut(bg, 1, o); ok || err != nil {
			t.Errorf("a cut of %d, retired, answered %v, %v; want false, nil", o, ok, err)
		}
	}
	release()
	waitOn(t, "b's opener to close what it opened", func() <-chan struct{} {
		d.files.mu.Lock()
		defer d.files.mu.Unlock()
		return d.files.changed
	}, func() bool { return d.files.isClosed("b") })
	holds("b's open ended while e plays: b gone with its opener", 1, 1)
	d.release(1)
	d.files.allClosed(t)
	d.k.CheckClosedEach(t)
}

// TestPlaybackAnswersSessionEndingOncePlayingOutItsLastContent pins
// ErrSessionEnding: once nothing follows the last occurrence and the
// producer plays out what it gave the sink, a Prepare is answered
// ErrSessionEnding, and the session reports Finished when the drain ends.
func TestPlaybackAnswersSessionEndingOncePlayingOutItsLastContent(t *testing.T) {
	d := newDirect(t, nil)
	d.files.add("a", wav(48000, 24, pattern(1, 1000, 24)))
	d.files.add("b", wav(48000, 24, pattern(2, 1000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	if err := d.be.Prepare(bg, 1, 101, nil); err != nil {
		t.Fatal(err)
	}
	d.k.Await(t, "the producer to play out its last content", func(v alsapcm.KernelView) bool { return v.Drains >= 1 })
	next := occurrence(102, 2, "b")
	if err := d.be.Prepare(bg, 1, 101, &next); !errors.Is(err, engine.ErrSessionEnding) {
		t.Fatalf("a Prepare while the session plays out its last content: %v, want ErrSessionEnding", err)
	}
	d.k.Free()
	waitOn(t, "Finished", d.rec.ch, func() bool { return count(d.rec.events(), "finished", 0) == 1 })
	d.release(1)
	got := unpack(t, d.k.Heard(), format.S243LE, 24)
	if sumOf(got) != sumOf(pattern(1, 1000, 24)) {
		t.Errorf("the DAC heard %d samples, not exactly a's", len(got))
	}
}

// refusals are content the backend refuses before any of it plays, each
// with its kind's reason.
func refusals(t *testing.T) []struct {
	name, ref string
	data      []byte
	fail      error
	formats   []format.ALSAFormat
	want      engine.Reason
} {
	t.Helper()
	read := func(p ...string) []byte {
		b, err := os.ReadFile(filepath.Join(append([]string{"..", "player", "decode", "testdata"}, p...)...))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return []struct {
		name, ref string
		data      []byte
		fail      error
		formats   []format.ALSAFormat
		want      engine.Reason
	}{
		{name: "native DSD", ref: "dsd", data: read("dsd-goldens", "dsd64.dff"), want: playback.ReasonDSD},
		{name: "DSF", ref: "dsf", data: read("dsd-goldens", "lsb_dsd64_whole_blocks.dsf"), want: playback.ReasonDSD},
		{name: "floating-point PCM", ref: "float", data: wavFormat(3, 48000, 32, 1000, pattern(1, 1000, 32)), want: engine.Reason(decode.ReasonFloatPCM)},
		{name: "an unknown container", ref: "noise", data: []byte("this is no audio container at all, just bytes"), want: engine.Reason(decode.ReasonUnknownContainer)},
		{name: "AAC", ref: "aac", data: read("alac-goldens", "afconvert_aac_44100.m4a"), want: engine.Reason(decode.ReasonLossy)},
		{name: "20-bit ALAC", ref: "alac20", data: read("alac-goldens", "afconvert_20bit_48000.m4a"), want: engine.Reason(decode.ReasonUnsupportedBits)},
		{name: "mono", ref: "mono", data: read("alac-goldens", "afconvert_mono_44100.m4a"), want: engine.Reason(decode.ReasonUnsupportedChannels)},
		{name: "a reference the resolver cannot open", ref: "gone", fail: errors.New("open /music/gone.flac: no such file or directory"), want: engine.ReasonMissing},
		{name: "no frames", ref: "empty", data: wav(48000, 24, nil), want: playback.ReasonEmpty},
		{name: "a rate the backend does not open", ref: "slow", data: wav(4000, 16, pattern(1, 1000, 16)), want: playback.ReasonRate},
		{name: "a transport the DAC does not take", ref: "s16", data: wav(48000, 16, pattern(1, 1000, 16)), formats: []format.ALSAFormat{format.S243LE, format.S32LE}, want: playback.ReasonTransport},
	}
}

// TestPlaybackRefusesBeforeAnyContentPlays pins the refusals as a session's
// first occurrence: Start answers an *engine.Unplayable with the kind's
// reason, nothing reaches the DAC, and the device is not touched, but for
// the transport the DAC does not take, which the backend learns by offering
// it (each refused open closed, nothing written).
func TestPlaybackRefusesBeforeAnyContentPlays(t *testing.T) {
	for i, c := range refusals(t) {
		t.Run(c.name, func(t *testing.T) {
			d := newDirect(t, nil)
			if c.formats != nil {
				d.k.Formats(c.formats...)
			}
			if c.fail != nil {
				d.files.fail[c.ref] = c.fail
			} else {
				d.files.add(c.ref, c.data)
			}
			s := engine.SessionID(i + 1)
			err := d.start(s, c.ref)
			var u *engine.Unplayable
			if !errors.As(err, &u) || u.Reason != c.want {
				t.Fatalf("Start of %s: %v; want an *engine.Unplayable, %s", c.name, err, c.want)
			}
			v := d.k.View()
			if v.Writes != 0 || v.GivenBytes != 0 || (c.formats == nil && v.Opens != 0) {
				t.Errorf("refused content: %d opens, %d writes, %d bytes; want nothing written, and the device untouched", v.Opens, v.Writes, v.GivenBytes)
			}
			d.release(s)
			d.files.allClosed(t)
			d.k.CheckClosedEach(t)
		})
	}
}

// TestPlaybackRefusesDSDInEveryMode pins DSD's refusal by a backend with no
// authorization, whatever the session's DSD mode: the mode the engine passes
// decides nothing (SessionOptions.DSD), and with no owner-approved record to
// read DSD content is refused as not in this release, before the device is
// touched. An approved record's effective set is playback's (B470 2d.2).
func TestPlaybackRefusesDSDInEveryMode(t *testing.T) {
	dsf, err := os.ReadFile(filepath.Join("..", "player", "decode", "testdata", "dsd-goldens", "lsb_dsd64_whole_blocks.dsf"))
	if err != nil {
		t.Fatal(err)
	}
	for i, mode := range []string{"off", "native", "dop", "auto"} {
		t.Run(mode, func(t *testing.T) {
			d := newDirect(t, nil)
			d.files.add("dsd", dsf)
			s := engine.SessionID(i + 1)
			err := d.be.Start(bg, engine.StartRequest{Session: s, First: occurrence(engine.OccurrenceID(s)*100+1, 1, "dsd"),
				Options: engine.SessionOptions{PauseRelease: time.Second, DSD: mode}, Events: &recEvents{r: d.rec, next: nopEvents{}}})
			var u *engine.Unplayable
			if !errors.As(err, &u) || u.Reason != playback.ReasonDSD {
				t.Fatalf("Start of DSD under mode %s: %v; want an *engine.Unplayable, %s", mode, err, playback.ReasonDSD)
			}
			if v := d.k.View(); v.Opens != 0 {
				t.Errorf("DSD under mode %s opened the device %d times", mode, v.Opens)
			}
			d.release(s)
			d.files.allClosed(t)
		})
	}
}

// TestPlaybackSkipsASuccessorRefusedBeforeAnyOfItPlayed pins the refusals as
// successors, through the engine: each is reported Refused with its reason,
// never Submitted, the engine skips it with that reason, and what follows
// plays straight after the entry before it: the DAC hears a, then b, and
// nothing of what was refused between them.
func TestPlaybackSkipsASuccessorRefusedBeforeAnyOfItPlayed(t *testing.T) {
	for _, c := range refusals(t) {
		if c.formats != nil {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.k.Free()
			a, b := pattern(1, 3000, 24), pattern(2, 3000, 24)
			r.files.add("a", wav(48000, 24, a))
			r.files.add("b", wav(48000, 24, b))
			if c.fail != nil {
				r.files.fail[c.ref] = c.fail
			} else {
				r.files.add(c.ref, c.data)
			}
			ids := r.insert("a", c.ref, "b")
			r.play()
			s := r.waitStopped(engine.ReasonEndOfQueue)
			if sk, ok := s.Skipped(ids[1]); !ok || sk.Reason != c.want {
				t.Errorf("the refused entry's skip: %+v %v, want %s", sk, ok, c.want)
			}
			evs := r.rec.events()
			var refusedOcc engine.OccurrenceID
			for _, e := range evs {
				if e.kind == "refused" {
					refusedOcc = e.o
					if e.why.Reason != c.want {
						t.Errorf("refused %s, want %s", e.why.Reason, c.want)
					}
				}
			}
			if refusedOcc == 0 || count(evs, "submitted", refusedOcc) != 0 || count(evs, "audible", refusedOcc) != 0 {
				t.Errorf("the refused occurrence %d: %s", refusedOcc, kinds(evs))
			}
			got := unpack(t, r.k.Heard(), format.S243LE, 24)
			if want := append(slices.Clone(a), b...); sumOf(got) != sumOf(want) {
				t.Errorf("the DAC heard %d samples, not a then b (%d)", len(got), len(want))
			}
		})
	}
}

// TestPlaybackFaultsOnADecodeFailureAfterContentPlayed pins a decode that
// fails after content played: a file cut short of the frames it declares
// plays every frame it holds, then the session faults (decode_failed) with
// the occurrence and its frames played, and the engine stops; nothing is
// skipped and nothing restarts.
func TestPlaybackFaultsOnADecodeFailureAfterContentPlayed(t *testing.T) {
	r := newRig(t)
	r.k.Free()
	a := pattern(1, 10000, 24)
	r.files.add("cut", wavFormat(1, 48000, 24, 20000, a))
	r.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
	ids := r.insert("cut", "b")
	r.play()
	s := r.waitStopped(engine.ReasonFault)
	faults := s.Faults()
	if len(faults) != 1 || faults[0].Reason != engine.ReasonDecodeFailed || faults[0].Frames != 10000 || faults[0].Entry != ids[0] {
		t.Fatalf("faults %+v; want one decode failure of entry %d after its 10000 frames", faults, ids[0])
	}
	if _, skipped := s.Skipped(ids[0]); skipped {
		t.Errorf("a decode failure after content played was taken for a skip")
	}
	if got := unpack(t, r.k.Heard(), format.S243LE, 24); sumOf(got) != sumOf(a) {
		t.Errorf("the DAC heard %d samples, not the %d the file held", len(got), len(a))
	}
}

// TestPlaybackReportsAudibleWhenThePlayedContentCrosses pins the boundary:
// Audible comes when the sink's played content passes the occurrence's first
// frame, never when its decoder ends. b's decoder ends long before the DAC
// reaches it: no Audible then, nor while the DAC plays a; Audible once it
// plays past a's last frame. (b is long: the sink measures nothing during
// the kernel's drain of the session's last hardware buffer, so a boundary
// there is reported when that drain ends.)
func TestPlaybackReportsAudibleWhenThePlayedContentCrosses(t *testing.T) {
	r := newRig(t)
	r.files.add("a", wav(44100, 16, pattern(1, 20000, 16)))
	r.files.add("b", wav(44100, 16, pattern(2, 20000, 16)))
	r.insert("a", "b")
	r.play()
	evs := r.waitEvents("b's frames in the sink", func(evs []event) bool { return count(evs, "submitted", 0) == 1 })
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	var b engine.OccurrenceID
	for _, e := range evs {
		if e.kind == "submitted" {
			b = e.o
		}
	}
	r.tick()
	if n := count(r.rec.events(), "audible", b); n != 0 {
		t.Fatalf("b reported audible with nothing played")
	}
	// 400 ms is 17640 frames, short of a's 20000.
	r.k.Play(t, 400*time.Millisecond)
	r.tick()
	if n := count(r.rec.events(), "audible", b); n != 0 {
		t.Fatalf("b reported audible while the DAC still played a")
	}
	r.k.Play(t, 100*time.Millisecond)
	r.tick()
	if n := count(r.rec.events(), "audible", b); n != 1 {
		t.Fatalf("b audible %d times once the DAC played past a's end: %s", n, kinds(r.rec.events()))
	}
}

// TestPlaybackDiscardForgetsThePackingTheUnitTook pins Discard: a DAC that
// takes 24 bits as S32_LE alone costs a session two refused offers first
// (S24_3LE, S24_LE); the next session offers what the unit took first and
// opens once; after Discard, which the engine calls when the output changes
// while nothing plays, the offers start again from the table.
func TestPlaybackDiscardForgetsThePackingTheUnitTook(t *testing.T) {
	d := newDirect(t, nil)
	d.k.Formats(format.S32LE)
	d.files.add("a", wav(48000, 24, pattern(1, 1000, 24)))
	opens := func(s engine.SessionID) int {
		before := d.k.View().Opens
		if err := d.start(s, "a"); err != nil {
			t.Fatal(err)
		}
		d.release(s)
		return d.k.View().Opens - before
	}
	if n := opens(1); n != 3 {
		t.Errorf("the first session opened %d times, want 3: two refused packings, then S32_LE", n)
	}
	if n := opens(2); n != 1 {
		t.Errorf("the second session opened %d times, want once: the packing the unit took", n)
	}
	if err := d.be.Discard(bg); err != nil {
		t.Fatal(err)
	}
	if n := opens(3); n != 3 {
		t.Errorf("after Discard a session opened %d times, want 3: the offers from the table again", n)
	}
	d.k.CheckClosedEach(t)
}

// receiveIn waits for ch under the watchdog and returns what it gave.
func receiveIn[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(alsapcm.Watchdog())
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
	}
	var zero T
	return zero
}
