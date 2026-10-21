package alsapcm

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestSinkPausesWithTheKernelsPause pins a pause on hardware that offers
// PAUSE: the stream stops at once with its buffer kept, the position is
// frozen where the kernel's pointers say the DAC stopped, nothing is
// written while paused however long it lasts (short of the release), and
// the release of the pause plays on from there: the DAC hears every frame
// once, with no silence written.
func TestSinkPausesWithTheKernelsPause(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 24000)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 150*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	var consumed uint64
	var state int32
	k.inspect(func(k *fakeKernel) { consumed, state = k.consumed, k.state })
	p := s.Position()
	if state != statePaused || !slices.Equal(k.pauses, []uintptr{1}) {
		t.Errorf("after Pause the stream is %s with PAUSE given %v, want PAUSED by PAUSE 1", State(state), k.pauses)
	}
	if p.State != SinkPaused || p.Stream != State(statePaused) || p.Played != int64(consumed) {
		t.Errorf("paused: %+v, want paused at frame %d", p, consumed)
	}
	writes := k.callsOf(ioctlWriteIFrames)
	k.play(t, 5*time.Second)
	if q := s.Position(); q.Played != p.Played || q.State != SinkPaused || k.callsOf(ioctlWriteIFrames) != writes {
		t.Errorf("after 5 s paused: %+v and %d writes, want the position frozen and no write", q, k.callsOf(ioctlWriteIFrames)-writes)
	}
	k.inspect(func(k *fakeKernel) {
		if k.consumed != consumed || !k.isOpen {
			t.Errorf("after 5 s paused the DAC consumed %d more frames (open: %v)", k.consumed-consumed, k.isOpen)
		}
	})
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !slices.Equal(k.pauses, []uintptr{1, 0}) || s.Position().State != SinkPlaying {
		t.Errorf("after Resume: PAUSE given %v, state %s; want 1 then 0, playing", k.pauses, s.Position().State)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	if st := s.Stats(); st.PausedBy != "pause" || st.Pauses != 1 || st.Silence != 0 {
		t.Errorf("stats: paused by %q, %d pauses, %d frames of silence", st.PausedBy, st.Pauses, st.Silence)
	}
	k.checkClosedEach(t)
}

// TestSinkPausesAStreamThatHasNotStartedByWritingNothing pins a pause
// before the buffer has filled and started the stream: there is nothing to
// pause, so the sink neither issues PAUSE nor writes zeros, and writes
// nothing at all until it is resumed.
func TestSinkPausesAStreamThatHasNotStartedByWritingNothing(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 20000)
	submit(t, s, in[:1000*6])
	await(t, k.when(filledAndIdle(1000)), "the writer to write 1000 frames")
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	submit(t, s, in[1000*6:])
	await(t, k.when(starved), "the paused writer to wait")
	k.play(t, 3*time.Second)
	k.inspect(func(k *fakeKernel) {
		if k.appl != 1000 || len(k.pauses) != 0 || k.state != statePrepared {
			t.Errorf("paused before the start: %d frames written, PAUSE %v, %s; want 1000, none, PREPARED", k.appl, k.pauses, State(k.state))
		}
	})
	if p := s.Position(); p.State != SinkPaused || p.Played != 0 || s.Stats().PausedBy != "idle" {
		t.Errorf("paused before the start: %+v, by %q", p, s.Stats().PausedBy)
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
}

// TestSinkPausesWithZeros pins a pause by zeros, on hardware without PAUSE
// and when zeros are asked for: no PAUSE is issued, the frames already
// queued play out and the stream then runs on zeros, the position stops at
// the last content frame and never counts the zeros, and the resumed
// content follows the zeros: the DAC hears the content, the zeros the
// sink wrote, and the rest of the content, each frame once.
func TestSinkPausesWithZeros(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(*fakeKernel, *SinkConfig)
	}{
		{"hardware without PAUSE", func(k *fakeKernel, _ *SinkConfig) { k.hw.info &^= infoPause }},
		{"zeros asked for", func(_ *fakeKernel, c *SinkConfig) { c.Pause = PauseZeros }},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			cfg := sinkConfig()
			c.set(k, &cfg)
			s := openTestSink(t, k, cfg)
			in := content(0, 0, 48000)
			submit(t, s, in)
			await(t, k.when(starved), "the writer to fill the hardware buffer")
			k.play(t, 100*time.Millisecond)
			if err := s.Pause(); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			x := s.Position().Written
			k.play(t, time.Second)
			p := s.Position()
			if len(k.pauses) != 0 {
				t.Errorf("PAUSE given %v by a pause with zeros", k.pauses)
			}
			if p.State != SinkPaused || p.Played != x || p.Written != x || p.Stream != State(stateRunning) {
				t.Errorf("a second into the pause: %+v; want every one of the %d frames written before it played, and no more", p, x)
			}
			st := s.Stats()
			if st.PausedBy != "zeros" || st.Silence < 48000-9600 {
				t.Errorf("paused by %q after %d frames of silence; want zeros, all but the buffer's worth of a second", st.PausedBy, st.Silence)
			}
			if err := s.Resume(); err != nil {
				t.Fatal(err)
			}
			// The buffer is full of zeros: the first 100 ms after the
			// resume play zeros, and the content after them waits.
			k.play(t, 100*time.Millisecond)
			if p := s.Position(); p.Played != x || p.Written <= x {
				t.Errorf("100 ms into the resume, zeros still queued ahead of the content: %+v; want %d played, more written", p, x)
			}
			k.free()
			drain(t, s)
			silence := s.Stats().Silence
			sameFrames(t, "what the DAC heard", heard(k), join(in[:x*6], zeros(silence), in[x*6:]))
			if got := s.Position().Played; got != 48000 {
				t.Errorf("played %d after the drain, want the 48000 content frames: silence is not content", got)
			}
		})
	}
}

// TestSinkReleasesAPauseAfterThirtySeconds pins the release, after a pause
// by PAUSE, by zeros, and before the stream started: the device is held for
// thirty seconds and then closed, once; the position is kept; a resume
// opens it again with the same parameters and writes first the frames the
// DAC had not played, so the DAC hears every frame once across both opens.
func TestSinkReleasesAPauseAfterThirtySeconds(t *testing.T) {
	for _, c := range []struct {
		name     string
		set      func(*fakeKernel, *SinkConfig)
		started  bool
		pausedBy string
	}{
		{"paused by PAUSE", nil, true, "pause"},
		{"paused by zeros", func(_ *fakeKernel, c *SinkConfig) { c.Pause = PauseZeros }, true, "zeros"},
		{"paused before the start", nil, false, "idle"},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			cfg := sinkConfig()
			if c.set != nil {
				c.set(k, &cfg)
			}
			s := openTestSink(t, k, cfg)
			in := content(0, 0, 48000)
			if c.started {
				submit(t, s, in)
				await(t, k.when(starved), "the writer to fill the hardware buffer")
				k.play(t, 100*time.Millisecond)
			} else {
				submit(t, s, in[:3000*6])
				await(t, k.when(filledAndIdle(3000)), "the writer to write 3000 frames")
			}
			if err := s.Pause(); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			paused := s.Position()
			k.play(t, DefaultReleaseAfter-100*time.Millisecond)
			k.inspect(func(k *fakeKernel) {
				if !k.isOpen || k.closes != 0 {
					t.Errorf("released before thirty seconds: open %v, closes %d", k.isOpen, k.closes)
				}
			})
			if st := s.Stats(); st.PausedBy != c.pausedBy {
				t.Errorf("paused by %q, want %q", st.PausedBy, c.pausedBy)
			}
			k.grant(time.Second)
			p := waitState(t, s, SinkReleased)
			k.inspect(func(k *fakeKernel) {
				if k.isOpen || k.closes != 1 || k.opens != 1 {
					t.Errorf("released: open %v after %d opens and %d closes; want closed once", k.isOpen, k.opens, k.closes)
				}
			})
			want := paused.Played
			if c.pausedBy == "zeros" {
				want = paused.Written
			}
			if p.Played != want || p.Written != p.Played {
				t.Errorf("released: %+v; want %d played and written, the frames the DAC had not played to be written again", p, want)
			}
			if err := s.Resume(); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			if !c.started {
				submit(t, s, in[3000*6:])
			}
			k.free()
			drain(t, s)
			if c.pausedBy == "zeros" {
				// The zeros still queued when the device was released were
				// never heard: the DAC heard the content, some silence, and
				// the rest of the content.
				if n := aroundSilence(t, "what the DAC heard across both opens", heard(k), in[:paused.Written*6], in[paused.Written*6:]); n == 0 || n > s.Stats().Silence {
					t.Errorf("heard %d frames of silence of the %d written", n, s.Stats().Silence)
				}
			} else {
				sameFrames(t, "what the DAC heard across both opens", heard(k), in)
			}
			if st := s.Stats(); st.Releases != 1 || st.Reopens != 1 {
				t.Errorf("%d releases, %d reopens; want one each", st.Releases, st.Reopens)
			}
			k.inspect(func(k *fakeKernel) {
				if len(k.opened) != 2 || k.opened[1] != testEndpoint.Path() {
					t.Errorf("opened %q, want the endpoint's node twice", k.opened)
				}
			})
			k.checkClosedEach(t)
		})
	}
}

// TestSinkResumeAfterAReleaseTypesWhatRefusesIt pins a resume whose reopen
// fails: busy (MPD took the DAC while it was free), gone (unplugged), each
// typed, the sink still released with its position kept, until a reopen
// succeeds and plays on from where it stopped.
func TestSinkResumeAfterAReleaseTypesWhatRefusesIt(t *testing.T) {
	k := newModel(t)
	cfg := sinkConfig()
	cfg.ReleaseAfter = time.Second
	s := openTestSink(t, k, cfg)
	in := content(0, 0, 30000)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	k.grant(2 * time.Second)
	released := waitState(t, s, SinkReleased)
	for _, c := range []struct {
		err  error
		want Reason
	}{{errnoEBUSY, ReasonBusy}, {errnoENOENT, ReasonNoDevice}, {errnoEACCES, ReasonPermission}} {
		k.inspect(func(k *fakeKernel) { k.openErr = c.err })
		err := s.Resume()
		wantReason(t, err, c.want)
		if p := s.Position(); p.State != SinkReleased || p.Played != released.Played {
			t.Errorf("after a refused reopen (%v): %+v, want still released at %d", c.err, p, released.Played)
		}
	}
	k.inspect(func(k *fakeKernel) { k.openErr = nil })
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	k.checkClosedEach(t)
}

// TestSinkCommandsFollowTheState pins what each command does in each state
// it can meet: a second Pause and a Resume of a sink that plays change
// nothing; Recover with no xrun and Drain while paused are refused, typed,
// and change nothing; and while it drains the lookahead, a pause pauses
// the drain and a resume plays it on, while frames, a second Drain and
// Recover are refused, and the drain plays every frame.
func TestSinkCommandsFollowTheState(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 30000)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	if err := s.Resume(); err != nil {
		t.Errorf("Resume of a sink that plays: %v", err)
	}
	wantReason(t, s.Recover(), ReasonBadState)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(); err != nil {
		t.Errorf("a second Pause: %v", err)
	}
	wantReason(t, s.Drain(t.Context()), ReasonBadState)
	if p := s.Position(); p.State != SinkPaused || len(k.pauses) != 1 {
		t.Errorf("after refused commands: %s, PAUSE given %v", p.State, k.pauses)
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Drain(context.Background()) }()
	waitState(t, s, SinkDraining)
	if _, err := s.Write(context.Background(), content(0, 0, 1)); ReasonOf(err) != ReasonBadState {
		t.Errorf("a Write while draining: %v, want %s", err, ReasonBadState)
	}
	wantReason(t, s.Drain(context.Background()), ReasonBadState)
	wantReason(t, s.Recover(), ReasonBadState)
	if err := s.Pause(); err != nil {
		t.Fatalf("a Pause of a drain that writes its lookahead: %v", err)
	}
	if p := s.Position(); p.State != SinkPaused {
		t.Errorf("a paused drain is %s, want paused", p.State)
	}
	if _, err := s.Write(context.Background(), content(0, 0, 1)); ReasonOf(err) != ReasonBadState {
		t.Errorf("a Write to a paused drain: %v, want %s", err, ReasonBadState)
	}
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume of a paused drain: %v", err)
	}
	if p := s.Position(); p.State != SinkDraining {
		t.Errorf("a resumed drain is %s, want draining", p.State)
	}
	k.free()
	if err := receive(t, done, "the drain to end"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	sameFrames(t, "what the DAC heard", heard(k), in)
	if len(k.pauses) != 4 {
		t.Errorf("PAUSE given %v, want two pauses and two releases", k.pauses)
	}
}

// TestSinkLastBufferDrainsUninterrupted pins the last part of a drain: once
// the store is empty and the kernel plays out the buffer, a Pause or a
// Resume is refused, typed, rather than cut short a Device.Drain, which
// would drop the stream and cut off the end; the drain then plays every
// frame.
func TestSinkLastBufferDrainsUninterrupted(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 9600)
	submit(t, s, in)
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	done := make(chan error, 1)
	go func() { done <- s.Drain(context.Background()) }()
	await(t, k.when(func(k *fakeKernel) bool { return k.state == stateDraining && starved(k) }), "the kernel to drain the last buffer")
	wantReason(t, s.Pause(), ReasonBadState)
	wantReason(t, s.Resume(), ReasonBadState)
	k.free()
	if err := receive(t, done, "the drain to end"); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	sameFrames(t, "what the DAC heard", heard(k), in)
	if len(k.pauses) != 0 {
		t.Errorf("PAUSE given %v during the kernel's drain", k.pauses)
	}
	k.checkClosedEach(t)
}
