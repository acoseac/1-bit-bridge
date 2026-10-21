package alsapcm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestSinkReportsAnXrunAndRecoversOnlyWhenAsked pins an underrun: the
// producer is late, the buffer runs dry, and the sink stops with the xrun,
// counted as a gap and recorded with where it happened and what the sink
// still held, refuses frames, and never prepares the stream by itself.
// Recover prepares it and plays on from the next frame: the DAC hears every
// frame once, the gap in time and not in the music, and the gap stays
// counted. The late frames arrive before the kernel's wake-up reaches the
// writer's wait, so a write finds the xrun (the wait finding it is
// TestSinkWatchesTheStreamWhileTheProducerIsLate).
func TestSinkReportsAnXrunAndRecoversOnlyWhenAsked(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 20000)
	submit(t, s, in[:9600*6])
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait for frames")
	k.advanceUnwatched(300 * time.Millisecond)
	submit(t, s, in[9600*6:])
	p := waitState(t, s, SinkXrun)
	wantReason(t, p.Err, ReasonXrun)
	if p.Gaps != 1 || p.Played != 9600 || p.Written != 9600 || p.Submitted != 20000 {
		t.Errorf("at the xrun: %+v; want one gap, the 9600 frames written played, 20000 submitted", p)
	}
	if n, err := s.Write(context.Background(), content(0, 0, 10)); n != 0 || ReasonOf(err) != ReasonXrun {
		t.Errorf("a Write to a sink an xrun stopped: %d, %v; want nothing taken and the xrun", n, err)
	}
	gaps := s.Stats().Gaps
	if len(gaps) != 1 || gaps[0].At != 9600 || gaps[0].Lookahead != 10400 || gaps[0].Op != "write" || gaps[0].Noticed.IsZero() || !gaps[0].Recovered.IsZero() {
		t.Errorf("gaps %+v; want one at frame 9600, 10400 frames held, found by a write, not recovered", gaps)
	}
	if n := k.callsOf(ioctlPrepare); n != 1 {
		t.Errorf("PREPARE issued %d times before Recover: the sink recovered by itself", n)
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := k.callsOf(ioctlPrepare); n != 2 || s.Position().State != SinkPlaying {
		t.Errorf("after Recover: PREPARE %d times, %s; want twice, playing", n, s.Position().State)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
	p = s.Position()
	if p.Gaps != 1 || p.Played != 20000 {
		t.Errorf("after the drain: %+v, want the gap still counted and 20000 played", p)
	}
	if gaps = s.Stats().Gaps; len(gaps) != 1 || gaps[0].Recovered.Before(gaps[0].Noticed) || gaps[0].Recovered.IsZero() {
		t.Errorf("gaps %+v, want the one gap stamped recovered", gaps)
	}
	k.inspect(func(k *fakeKernel) {
		if k.epipes != 1 {
			t.Errorf("%d writes offered to a stopped stream, want the one that found the xrun", k.epipes)
		}
	})
	k.checkClosedEach(t)
}

// TestSinkWatchesTheStreamWhileTheProducerIsLate pins the writer's wait
// when it has nothing to write: the producer has submitted one buffer,
// which started the stream, and is late with the next frames. The writer
// waits on the stream, not only for frames: the position moves while the
// DAC plays what the buffer holds, and an underrun, a device gone or a
// suspend is found by the wait itself, with no write to find it (#1303's
// review: a starved producer stopped the device's monitoring).
func TestSinkWatchesTheStreamWhileTheProducerIsLate(t *testing.T) {
	for _, c := range []struct {
		name  string
		stop  func(*fakeKernel)
		state SinkState
		want  Reason
	}{
		{"an underrun", func(k *fakeKernel) { k.advanceNow(300 * time.Millisecond) }, SinkXrun, ReasonXrun},
		{"a device gone", (*fakeKernel).unplug, SinkEnded, ReasonDisconnected},
		{"a suspend", (*fakeKernel).suspend, SinkEnded, ReasonSuspended},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			s := openTestSink(t, k, sinkConfig())
			submit(t, s, content(0, 0, 9600))
			await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait on the stream")
			k.play(t, 100*time.Millisecond)
			var consumed uint64
			k.inspect(func(k *fakeKernel) { consumed = k.consumed })
			if p := s.Position(); p.State != SinkPlaying || consumed != 4800 || p.Played != 4800 || !p.Measured.Equal(k.now()) {
				t.Errorf("100 ms into a late producer: %+v with the DAC at frame %d; want 4800 played, measured now", p, consumed)
			}
			c.stop(k)
			p := waitState(t, s, c.state)
			wantReason(t, p.Err, c.want)
			if c.state != SinkXrun {
				k.checkClosedEach(t)
				return
			}
			if gaps := s.Stats().Gaps; p.Played != 9600 || p.Gaps != 1 || len(gaps) != 1 || gaps[0].Op != "wait" || gaps[0].At != 9600 || gaps[0].Lookahead != 0 {
				t.Errorf("at the xrun: %+v, gaps %+v; want all 9600 played, one gap at frame 9600 found by the wait, nothing held", p, gaps)
			}
		})
	}
}

// TestSinkAnswersCommandsAtAnXrunWithTheXrun pins what a sink an xrun
// stopped says to Pause, Resume and Drain: the xrun, as Write says it, so a
// caller that recovers on an xrun recovers whichever call found it
// (#1303's review: pcm-test's --recover gave up on an xrun the writer's
// wait had found before the Drain). None of them changes anything: Recover
// plays on, and the drain plays every frame.
func TestSinkAnswersCommandsAtAnXrunWithTheXrun(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 9600)
	submit(t, s, in)
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	k.advanceNow(300 * time.Millisecond)
	waitState(t, s, SinkXrun)
	for _, c := range []struct {
		name string
		op   func() error
	}{
		{"Pause", s.Pause}, {"Resume", s.Resume},
		{"Drain", func() error { return s.Drain(context.Background()) }},
	} {
		if err := c.op(); ReasonOf(err) != ReasonXrun {
			t.Errorf("%s of a sink an xrun stopped: %v, want %s", c.name, err, ReasonXrun)
		}
	}
	if p := s.Position(); p.State != SinkXrun || p.Gaps != 1 || len(k.pauses) != 0 {
		t.Errorf("after the commands: %+v, PAUSE given %v; want still stopped at the one xrun", p, k.pauses)
	}
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
}

// TestSinkRecoverKeepsAPause pins Recover of a sink an xrun stopped while
// it was paused (#1303's review: Recover played on). A pause by zeros
// whose zeros come too late underruns; Recover prepares the stream and
// keeps the pause, writing nothing until Resume, with the release still
// due when the pause began; Resume after the release plays on from the
// first content frame the DAC had not played.
func TestSinkRecoverKeepsAPause(t *testing.T) {
	k := newModel(t)
	cfg := sinkConfig()
	cfg.Pause = PauseZeros
	s := openTestSink(t, k, cfg)
	in := content(0, 0, 30000)
	submit(t, s, in)
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	x := s.Position().Written
	k.play(t, 300*time.Millisecond)
	k.advanceNow(300 * time.Millisecond)
	waitState(t, s, SinkXrun)
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if p := s.Position(); p.State != SinkPaused {
		t.Fatalf("after Recover: %+v, want still paused", p)
	}
	writes := k.callsOf(ioctlWriteIFrames)
	k.play(t, DefaultReleaseAfter-time.Second)
	p := s.Position()
	if p.State != SinkPaused || p.Written != x || p.Played != x || k.callsOf(ioctlWriteIFrames) != writes {
		t.Errorf("recovered while paused: %+v and %d writes since; want still paused at frame %d, nothing written", p, k.callsOf(ioctlWriteIFrames)-writes, x)
	}
	// The pause began 600 ms before the Recover (300 ms of zeros, then 300 ms
	// late), so 29 s on its release is 400 ms away; one counted from the
	// Recover would be a second away.
	k.grant(500 * time.Millisecond)
	waitState(t, s, SinkReleased)
	k.revoke()
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	k.free()
	drain(t, s)
	// The xrun played every frame written: the content to x and every zero.
	if n := aroundSilence(t, "what the DAC heard", heard(k), in[:x*6], in[x*6:]); n != s.Stats().Silence {
		t.Errorf("heard %d frames of silence, want the %d written", n, s.Stats().Silence)
	}
}

// TestSinkCountsEveryGapAndKeepsTheLastFew pins the bound on the
// diagnostics: every xrun is counted, and the details of the last eight
// are kept, oldest first.
func TestSinkCountsEveryGapAndKeepsTheLastFew(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	total := 0
	for i := range 10 {
		// Each round gives the kernel one buffer, lets it run dry, and
		// offers one more frame before the wake-up reaches the writer's
		// wait: the frame finds the xrun and is the first the next round
		// writes.
		n := 9599
		if i == 0 {
			n = 9600
		}
		submit(t, s, content(0, total, n))
		total += n
		await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
		k.advanceUnwatched(300 * time.Millisecond)
		submit(t, s, content(0, total, 1))
		total++
		waitState(t, s, SinkXrun)
		if err := s.Recover(); err != nil {
			t.Fatalf("round %d: Recover: %v", i, err)
		}
	}
	if p := s.Position(); p.Gaps != 10 {
		t.Errorf("%d gaps counted, want 10", p.Gaps)
	}
	gaps := s.Stats().Gaps
	if len(gaps) != maxGaps {
		t.Fatalf("%d gaps kept, want %d", len(gaps), maxGaps)
	}
	for j, g := range gaps {
		if want := int64(9600 * (j + 3)); g.At != want {
			t.Errorf("kept gap %d at frame %d, want %d (the xruns of rounds 3 to 10)", j, g.At, want)
		}
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), content(0, 0, total))
}

// TestSinkDrainReportsAnUnderrunBeforeIt pins an underrun after the last
// write that the Drain reaches before the kernel's wake-up reaches the
// writer's wait: the drain finds the stream stopped, reports the xrun and
// counts the gap rather than calling it a clean end, and Close still
// closes once.
func TestSinkDrainReportsAnUnderrunBeforeIt(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 9600)
	submit(t, s, in)
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	k.advanceUnwatched(300 * time.Millisecond)
	wantReason(t, s.Drain(context.Background()), ReasonXrun)
	p := s.Position()
	if p.State != SinkXrun || p.Gaps != 1 || p.Played != 9600 {
		t.Errorf("after the drain: %+v, want stopped at an xrun, one gap, all 9600 played", p)
	}
	if gaps := s.Stats().Gaps; len(gaps) != 1 || gaps[0].Op != "drain" {
		t.Errorf("gaps %+v, want one found by the drain", gaps)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	sameFrames(t, "what the DAC heard", heard(k), in)
	k.checkClosedEach(t)
}

// TestSinkTakesRecoverAsSoonAsADrainReportsAnXrun pins the order of a
// Drain's answer: everything the answer says is settled before it is
// sent, so a Recover sent the moment a Drain reports an xrun is taken, not
// refused as a drain still playing out its last frames. The writer is held
// where the kernel drain lets go of the Drain's context, after the answer,
// until a command pokes it.
func TestSinkTakesRecoverAsSoonAsADrainReportsAnXrun(t *testing.T) {
	k := newModel(t)
	var s *Sink
	var calls atomic.Int32
	held := make(chan struct{})
	s = openTestSinkWith(t, context.Background(), k, sinkConfig(), sinkSeams{afterFunc: func(ctx context.Context, f func()) func() bool {
		stop := context.AfterFunc(ctx, f)
		if calls.Add(1) != 2 {
			return stop
		}
		// The second is the kernel drain's own, let go of once that drain
		// has ended: on the writer's goroutine, after the answer.
		return func() bool {
			select {
			case <-s.kick:
			default:
			}
			close(held)
			<-s.kick
			return stop()
		}
	}})
	in := content(0, 0, 9600)
	submit(t, s, in)
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	k.advanceUnwatched(300 * time.Millisecond)
	wantReason(t, s.Drain(context.Background()), ReasonXrun)
	await(t, held, "the writer to let go of the drain's context")
	recovered := make(chan error, 1)
	go func() { recovered <- s.Recover() }()
	if err := receive(t, recovered, "Recover to be answered"); err != nil {
		t.Fatalf("Recover the moment the Drain reported the xrun: %v", err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in)
}

// TestSinkEndsOnADisconnectOrASuspendAndClosesOnce pins the end of a
// stream the device ends: gone under a write, gone while the writer waits
// for room, gone while paused, suspended under a write and while paused.
// Each is typed, the device is closed once, the position stays where it
// was established, and every call after it gets the same reason, Close
// and Drop excepted, which do nothing.
func TestSinkEndsOnADisconnectOrASuspendAndClosesOnce(t *testing.T) {
	for _, c := range []struct {
		name string
		// run plays until the device fails, from a sink whose producer has
		// submitted a second of frames.
		run  func(*testing.T, *fakeKernel, *Sink)
		want Reason
	}{
		{"gone under a write", func(t *testing.T, k *fakeKernel, s *Sink) {
			k.inspect(func(k *fakeKernel) { k.at = []fakeAt{{frames: 20000, do: (*fakeKernel).disconnect}} })
			k.free()
		}, ReasonDisconnected},
		{"gone while the writer waits for room", func(t *testing.T, k *fakeKernel, s *Sink) {
			await(t, k.when(starved), "the writer to wait for room")
			k.unplug()
		}, ReasonDisconnected},
		{"gone while paused", func(t *testing.T, k *fakeKernel, s *Sink) {
			await(t, k.when(starved), "the writer to wait for room")
			if err := s.Pause(); err != nil {
				t.Fatal(err)
			}
			await(t, k.when(starved), "the paused writer to wait")
			k.unplug()
		}, ReasonDisconnected},
		{"suspended under a write", func(t *testing.T, k *fakeKernel, s *Sink) {
			k.inspect(func(k *fakeKernel) {
				k.at = []fakeAt{{frames: 20000, do: func(k *fakeKernel) { k.state = stateSuspended }}}
			})
			k.free()
		}, ReasonSuspended},
		{"suspended while paused", func(t *testing.T, k *fakeKernel, s *Sink) {
			await(t, k.when(starved), "the writer to wait for room")
			if err := s.Pause(); err != nil {
				t.Fatal(err)
			}
			await(t, k.when(starved), "the paused writer to wait")
			k.suspend()
		}, ReasonSuspended},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			s := openTestSink(t, k, sinkConfig())
			submit(t, s, content(0, 0, 48000))
			c.run(t, k, s)
			p := waitState(t, s, SinkEnded)
			if ReasonOf(p.Err) != c.want {
				t.Fatalf("ended with %v, want %s", p.Err, c.want)
			}
			if p.Played > p.Written || p.Written > p.Submitted {
				t.Errorf("position %+v: played past written, or written past submitted", p)
			}
			if _, err := s.Write(context.Background(), content(0, 0, 1)); ReasonOf(err) != c.want {
				t.Errorf("a Write after the end: %v, want %s", err, c.want)
			}
			for name, op := range map[string]func() error{
				"Pause": s.Pause, "Resume": s.Resume, "Recover": s.Recover,
				"Drain": func() error { return s.Drain(context.Background()) },
			} {
				if err := op(); ReasonOf(err) != c.want {
					t.Errorf("%s after the end: %v, want %s", name, err, c.want)
				}
			}
			if err := s.Drop(); err != nil {
				t.Errorf("Drop after the end: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close after the end: %v", err)
			}
			if q := s.Position(); q.Played != p.Played || q.Written != p.Written || q.State != SinkEnded {
				t.Errorf("after Close: %+v, want the position the end left: %+v", q, p)
			}
			k.checkClosedEach(t)
		})
	}
}

// TestSinkWriteWaitsWhileTheStoreIsFullAndItsCancelEndsTheWait pins the
// producer's wait: the store full and nothing to make room, since the sink
// was paused before it wrote a frame and so writes none, the producer
// waits, and a cancel of its context is all that can end the wait, at once,
// with the frames taken counted; the sink plays on once resumed, and those
// frames play. (The writer's own pokes would wake a producer a moment after
// the cancel and hide a wait that ignores its context; the pause leaves
// none.)
func TestSinkWriteWaitsWhileTheStoreIsFullAndItsCancelEndsTheWait(t *testing.T) {
	k := newModel(t)
	waits := make(chan struct{}, 1)
	cfg := sinkConfig()
	cfg.Lookahead = 100 * time.Millisecond
	s := openTestSinkWith(t, context.Background(), k, cfg, sinkSeams{producerWaits: func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	}})
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := content(0, 0, 40000)
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := s.Write(ctx, in)
		done <- result{n, err}
	}()
	await(t, waits, "the producer to wait for room")
	cancel()
	r := receive(t, done, "the producer's Write to end at the cancel")
	wantReason(t, r.err, ReasonCancelled)
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("%v does not wrap context.Canceled", r.err)
	}
	// The store holds 100 ms ahead and the 200 ms buffer's frames kept.
	if r.n != 14400 {
		t.Errorf("Write took %d frames, want the 14400 the store holds", r.n)
	}
	if p := s.Position(); p.State != SinkPaused || p.Submitted != 14400 || p.Written != 0 {
		t.Errorf("after the producer's cancel: %+v, want still paused with 14400 submitted and none written", p)
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), in[:14400*6])
}

// TestSinkCancelUnderLoadDropsAndClosesOnce pins the end of the sink's
// life while it works: the writer waits on a full hardware buffer and the
// producer on a full store, and the life's cancel ends both waits at once,
// drops the stream, closes the device once and types the end as a cancel.
func TestSinkCancelUnderLoadDropsAndClosesOnce(t *testing.T) {
	k := newModel(t)
	life, end := context.WithCancel(context.Background())
	defer end()
	waits := make(chan struct{}, 1)
	cfg := sinkConfig()
	cfg.Lookahead = 100 * time.Millisecond
	s := openTestSinkWith(t, life, k, cfg, sinkSeams{producerWaits: func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	}})
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(context.Background(), content(0, 0, 40000))
		done <- err
	}()
	await(t, waits, "the producer to wait for room")
	await(t, k.when(starved), "the writer to wait on the DAC")
	end()
	wantReason(t, receive(t, done, "the producer's Write to end with the sink"), ReasonCancelled)
	p := waitState(t, s, SinkEnded)
	wantReason(t, p.Err, ReasonCancelled)
	if !errors.Is(p.Err, context.Canceled) {
		t.Errorf("%v does not wrap context.Canceled", p.Err)
	}
	k.inspect(func(k *fakeKernel) {
		if k.isOpen || k.closes != 1 {
			t.Errorf("after the cancel: open %v, closed %d times; want closed once", k.isOpen, k.closes)
		}
	})
	if n := k.callsOf(ioctlDrop); n != 1 {
		t.Errorf("DROP issued %d times, want once", n)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	k.checkClosedEach(t)
}

// TestSinkDropIsImmediate pins Drop: no wait on the DAC, no drain, the
// stream dropped and the device closed once, the position where the drop
// found it, and the sink done.
func TestSinkDropIsImmediate(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	submit(t, s, content(0, 0, 30000))
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	clock := k.now()
	before := s.Position()
	if err := s.Drop(); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	p := s.Position()
	if p.State != SinkStopped || p.Played != before.Played || p.Written != before.Written {
		t.Errorf("after Drop: %+v, want stopped where it was: %+v", p, before)
	}
	if !k.now().Equal(clock) || k.callsOf(ioctlDrain) != 0 || k.callsOf(ioctlDrop) == 0 {
		t.Errorf("Drop waited %v on the DAC, drained %d times, dropped %d times", k.now().Sub(clock), k.callsOf(ioctlDrain), k.callsOf(ioctlDrop))
	}
	if _, err := s.Write(context.Background(), content(0, 0, 1)); ReasonOf(err) != ReasonClosed {
		t.Errorf("a Write after Drop: %v, want %s", err, ReasonClosed)
	}
	for name, op := range map[string]func() error{
		"Pause": s.Pause, "Resume": s.Resume, "Recover": s.Recover,
		"Drain": func() error { return s.Drain(context.Background()) },
	} {
		if err := op(); ReasonOf(err) != ReasonClosed {
			t.Errorf("%s after Drop: %v, want %s", name, err, ReasonClosed)
		}
	}
	if err := s.Drop(); err != nil {
		t.Errorf("a second Drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close after Drop: %v", err)
	}
	k.checkClosedEach(t)
}

// TestSinkAnAnsweredDrainsCancelLeavesTheNextDrainAlone pins the cancel of
// a Drain's context to that Drain (#1303's review: an answered drain's
// cancel could cut the next one short). The first Drain's context ends as
// the drain reaches the kernel, and its cancel is held back while that
// drain ends with the underrun found before it. After Recover a second
// Drain reaches the kernel's drain, and only then does the first one's
// cancel run: the second drain plays out, and the DAC hears every frame.
func TestSinkAnAnsweredDrainsCancelLeavesTheNextDrainAlone(t *testing.T) {
	k := newModel(t)
	var first atomic.Bool
	first.Store(true)
	fired, release, ran := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := openTestSinkWith(t, context.Background(), k, sinkConfig(), sinkSeams{afterFunc: func(ctx context.Context, f func()) func() bool {
		if !first.CompareAndSwap(true, false) {
			return context.AfterFunc(ctx, f)
		}
		return context.AfterFunc(ctx, func() {
			close(fired)
			<-release
			f()
			close(ran)
		})
	}})
	in := content(0, 0, 19200)
	submit(t, s, in[:9600*6])
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	k.advanceUnwatched(300 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The next now is the kernel drain's deadline: the first drain's
	// context ends there, after the writer last looked at it.
	k.inspect(func(k *fakeKernel) {
		k.onNow = func() {
			cancel()
			<-fired
		}
	})
	wantReason(t, s.Drain(ctx), ReasonXrun)
	if err := s.Recover(); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	submit(t, s, in[9600*6:])
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer again")
	done := make(chan error, 1)
	go func() { done <- s.Drain(context.Background()) }()
	await(t, k.when(func(k *fakeKernel) bool { return k.state == stateDraining && starved(k) }), "the kernel to drain the second buffer")
	close(release)
	await(t, ran, "the first drain's cancel to run")
	k.free()
	if err := receive(t, done, "the second drain to end"); err != nil {
		t.Fatalf("the second Drain: %v", err)
	}
	sameFrames(t, "what the DAC heard", heard(k), in)
}

// TestSinkAPausedDrainsCancelIsNotLost pins a Drain's cancel that comes as
// the writer goes from looking at the drain to waiting, here the wait of a
// paused drain, which lasts until the release (#1303's review: the cancel
// ended the context of the wait before, and the wait took a fresh one).
// The Drain ends at once with the cancel, and the sink stops.
func TestSinkAPausedDrainsCancelIsNotLost(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	submit(t, s, content(0, 0, 30000))
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Drain(ctx) }()
	waitState(t, s, SinkDraining)
	// The paused writer reads the clock once it has looked at the drain's
	// context and before it takes its wait's: the cancel comes there, and
	// its callback, which pokes the writer last, runs to its end first.
	k.atNow(func() bool { return s.state == SinkPaused }, func() {
		select {
		case <-s.kick:
		default:
		}
		cancel()
		<-s.kick
	})
	if err := s.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	wantReason(t, receive(t, done, "the paused drain to end at its cancel"), ReasonCancelled)
	if p := waitState(t, s, SinkStopped); p.Played > p.Written {
		t.Errorf("after the cancel: %+v", p)
	}
	k.checkClosedEach(t)
}

// TestSinkDrainEndsEarlyOnACancelOrADrop pins a drain that does not run
// to its end: its context cancelled while it writes the lookahead, or
// while the kernel drains the buffer, or a Drop from another goroutine.
// Each drops the stream, closes the device once and leaves the sink
// stopped; the drain says why it ended.
func TestSinkDrainEndsEarlyOnACancelOrADrop(t *testing.T) {
	for _, c := range []struct {
		name   string
		frames int
		// stop ends the drain once it is under way.
		stop func(*testing.T, *Sink, context.CancelFunc)
	}{
		{"cancelled while it writes the lookahead", 30000, func(_ *testing.T, _ *Sink, cancel context.CancelFunc) { cancel() }},
		{"cancelled while the kernel drains", 9600, func(_ *testing.T, _ *Sink, cancel context.CancelFunc) { cancel() }},
		{"dropped", 30000, func(t *testing.T, s *Sink, _ context.CancelFunc) {
			if err := s.Drop(); err != nil {
				t.Errorf("Drop while draining: %v", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := newModel(t)
			s := openTestSink(t, k, sinkConfig())
			submit(t, s, content(0, 0, c.frames))
			if c.frames == 9600 {
				await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
			} else {
				await(t, k.when(starved), "the writer to fill the hardware buffer")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.Drain(ctx) }()
			waitState(t, s, SinkDraining)
			if c.frames == 9600 {
				await(t, k.when(func(k *fakeKernel) bool { return k.state == stateDraining && starved(k) }), "the kernel to drain")
			}
			c.stop(t, s, cancel)
			wantReason(t, receive(t, done, "the drain to end early"), ReasonCancelled)
			if p := waitState(t, s, SinkStopped); p.Played > p.Written {
				t.Errorf("after the drain ended early: %+v", p)
			}
			if n := k.callsOf(ioctlDrop); n == 0 {
				t.Error("the stream was not dropped")
			}
			if err := s.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			k.checkClosedEach(t)
		})
	}
}
