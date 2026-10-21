package alsapcm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSinkReportsAnXrunAndRecoversOnlyWhenAsked pins an underrun: the
// producer is late, the buffer runs dry, and the sink stops with the xrun,
// counted as a gap and recorded with where it happened and what the sink
// still held, refuses frames, and never prepares the stream by itself.
// Recover prepares it and plays on from the next frame: the DAC hears every
// frame once, the gap in time and not in the music, and the gap stays
// counted.
func TestSinkReportsAnXrunAndRecoversOnlyWhenAsked(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 20000)
	submit(t, s, in[:9600*6])
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer and wait for frames")
	k.advanceNow(300 * time.Millisecond)
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

// TestSinkCountsEveryGapAndKeepsTheLastFew pins the bound on the
// diagnostics: every xrun is counted, and the details of the last eight
// are kept, oldest first.
func TestSinkCountsEveryGapAndKeepsTheLastFew(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	total := 0
	for i := range 10 {
		// Each round gives the kernel one buffer, lets it run dry, and
		// offers one more frame, which finds the xrun and is the first the
		// next round writes.
		n := 9599
		if i == 0 {
			n = 9600
		}
		submit(t, s, content(0, total, n))
		total += n
		await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
		k.advanceNow(300 * time.Millisecond)
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
// write: the drain finds the stream stopped, reports the xrun and counts
// the gap rather than calling it a clean end, and Close still closes once.
func TestSinkDrainReportsAnUnderrunBeforeIt(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	in := content(0, 0, 9600)
	submit(t, s, in)
	await(t, k.when(filledAndIdle(9600)), "the writer to fill the hardware buffer")
	k.advanceNow(300 * time.Millisecond)
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
// producer's wait: with the DAC playing nothing the store fills, the
// producer waits for room, and a cancel of its context ends the wait at
// once with the frames taken counted; the sink plays on, and those frames
// play.
func TestSinkWriteWaitsWhileTheStoreIsFullAndItsCancelEndsTheWait(t *testing.T) {
	k := newModel(t)
	waits := make(chan struct{}, 1)
	cfg := sinkConfig()
	cfg.Lookahead = 100 * time.Millisecond
	s := openTestSinkWith(t, context.Background(), k, cfg, func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	})
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
	await(t, k.when(starved), "the writer to wait on the DAC")
	cancel()
	r := <-done
	wantReason(t, r.err, ReasonCancelled)
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("%v does not wrap context.Canceled", r.err)
	}
	// The store holds 100 ms ahead and the 200 ms buffer's frames kept.
	if r.n != 14400 {
		t.Errorf("Write took %d frames, want the 14400 the store holds", r.n)
	}
	if p := s.Position(); p.State != SinkPlaying || p.Submitted != 14400 {
		t.Errorf("after the producer's cancel: %+v, want playing with 14400 submitted", p)
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
	s := openTestSinkWith(t, life, k, cfg, func() {
		select {
		case waits <- struct{}{}:
		default:
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := s.Write(context.Background(), content(0, 0, 40000))
		done <- err
	}()
	await(t, waits, "the producer to wait for room")
	await(t, k.when(starved), "the writer to wait on the DAC")
	end()
	wantReason(t, <-done, ReasonCancelled)
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
			wantReason(t, <-done, ReasonCancelled)
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
