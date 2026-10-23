package alsapcm

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"
)

// cut cuts s at frame at and fails the test unless the answer is want and
// no error.
func cut(t *testing.T, s *Sink, at int64, want bool) {
	t.Helper()
	ok, err := s.Cut(at)
	if err != nil || ok != want {
		t.Fatalf("Cut(%d) = %v, %v; want %v (position %+v)", at, ok, err, want, s.Position())
	}
}

// TestSinkCutDropsWhatHasNotReachedTheKernel pins the cut at a boundary
// (B470 PR 4b2): the frames the producer submitted from the marker on that
// the writer has not handed the kernel are dropped, none of them is ever
// written, and what the producer submits next follows the frames before the
// marker. A marker inside the write the writer has under way, a chunk the
// kernel takes as soon as it has room, is refused, and drops nothing.
func TestSinkCutDropsWhatHasNotReachedTheKernel(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	a, b := content(0, 0, 20000), content(1, 0, 20000)
	submit(t, s, a)
	submit(t, s, b)
	// The hardware buffer holds a's first 9600 frames, and the writer waits
	// for room with its next period, frames 9600 to 12000, in its write.
	await(t, k.when(starved), "the writer to fill the hardware buffer and wait for room")
	cut(t, s, 9000, false)
	cut(t, s, 11999, false)
	if p := s.Position(); p.Submitted != 40000 {
		t.Fatalf("a refused cut changed the store: submitted %d, want 40000", p.Submitted)
	}
	cut(t, s, 20000, true)
	if p := s.Position(); p.Submitted != 20000 || p.Written != 9600 {
		t.Fatalf("after the cut: %+v; want 20000 submitted, 9600 written", p)
	}
	c := content(2, 0, 5000)
	submit(t, s, c)
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), join(a, c))
	sameFrames(t, "what the kernel was given", given(k), join(a, c))
	if p := s.Position(); p.Played != 25000 {
		t.Errorf("played %d after the drain, want 25000", p.Played)
	}
	k.checkClosedEach(t)
}

// TestSinkCutAnswersFalseOnceAFrameOfItIsTheKernels pins the cut that comes
// too late: once a frame at or after the marker is in a write the writer has
// under way, or in the kernel's buffer, the cut drops nothing and says so,
// and every frame plays; with every frame in the kernel's buffer and no
// write under way too.
func TestSinkCutAnswersFalseOnceAFrameOfItIsTheKernels(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	a, b := content(0, 0, 12000), content(1, 0, 20000)
	submit(t, s, join(a, b))
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	// 50 ms frees a period: the write under way, a's last 2400 frames, is
	// taken, and the next, b's first 2400, waits for room in its turn.
	k.play(t, 50*time.Millisecond)
	cut(t, s, 12000, false)
	k.play(t, 50*time.Millisecond)
	if p := s.Position(); p.Written < 14400 {
		t.Fatalf("written %d, want b's first period taken", p.Written)
	}
	cut(t, s, 12000, false)
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), join(a, b))

	// 2000 frames go in one write, short of the start threshold: the writer
	// has nothing left to write, and the kernel holds every frame unplayed.
	k = newModel(t)
	s = openTestSink(t, k, sinkConfig())
	c, d := content(2, 0, 1000), content(3, 0, 1000)
	submit(t, s, join(c, d))
	await(t, k.when(filledAndIdle(2000)), "the writer to hand the kernel every frame and watch the stream")
	cut(t, s, 1000, false)
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), join(c, d))
}

// TestSinkCutNeverDropsAWrittenFrameNorWritesADroppedOne pins the cut
// against the writer, which hands the kernel the store's frames on a
// goroutine of its own, with the write held inside the kernel call: a cut
// whose marker falls in the frames the held write carries is refused and
// they all play; one whose marker follows them drops what follows and none
// of it is written; and a producer that cuts while the writer runs, a
// thousand times, under -race, finds every frame the kernel was given in
// the order its cuts left, none dropped that was handed over and none
// handed over that was dropped.
func TestSinkCutNeverDropsAWrittenFrameNorWritesADroppedOne(t *testing.T) {
	t.Run("the marker inside the write held in the kernel", func(t *testing.T) {
		k := newModel(t)
		s := openTestSink(t, k, sinkConfig())
		// Registered after the sink's Close, so a failing test lets the write
		// go before the Close waits for the writer.
		parked, release := k.parkNextWrite()
		t.Cleanup(release)
		a, b := content(0, 0, 100), content(1, 0, 100)
		submit(t, s, join(a, b))
		await(t, parked, "the writer's first write, a's and b's 200 frames, to park in the kernel")
		cut(t, s, 100, false)
		release()
		k.free()
		drain(t, s)
		sameFrames(t, "what the DAC heard", heard(k), join(a, b))
	})
	t.Run("the marker after the write held in the kernel", func(t *testing.T) {
		k := newModel(t)
		s := openTestSink(t, k, sinkConfig())
		parked, release := k.parkNextWrite()
		t.Cleanup(release)
		a, b := content(0, 0, 100), content(1, 0, 100)
		submit(t, s, a)
		await(t, parked, "the writer's first write, a's 100 frames, to park in the kernel")
		submit(t, s, b)
		cut(t, s, 100, true)
		release()
		k.free()
		drain(t, s)
		sameFrames(t, "what the DAC heard", heard(k), a)
		sameFrames(t, "what the kernel was given", given(k), a)
	})
	t.Run("cuts racing the writer", func(t *testing.T) {
		k := newModel(t)
		k.free()
		cfg := sinkConfig()
		cfg.Lookahead = 100 * time.Millisecond
		s := openTestSink(t, k, cfg)
		rng := rand.New(rand.NewPCG(4, 2))
		var want []byte
		track, from := byte(0), 0
		for range 1000 {
			n := 1 + rng.IntN(600)
			b := content(track, from, n)
			submit(t, s, b)
			want = append(want, b...)
			from += n
			sub := s.Position().Submitted
			at := sub - int64(rng.IntN(int(min(sub, 1200))+1))
			ok, err := s.Cut(at)
			if err != nil {
				t.Fatalf("Cut(%d): %v", at, err)
			}
			if ok {
				// What follows the marker is another track's, so a dropped
				// frame that reached the kernel shows where it landed.
				want = want[:at*6]
				track, from = (track+1)%100, int(at)
			}
		}
		drain(t, s)
		sameFrames(t, "what the kernel was given", given(k), want)
		sameFrames(t, "what the DAC heard", heard(k), want)
	})
}

// TestSinkCutsAPausedStoreAndAReleasedOne pins the cut while the sink holds
// a pause, by PAUSE, and once it has released the device: the store's
// frames past the kernel's buffer are cut while paused, and after the
// release, which takes back what the DAC had not played, a cut at a frame
// the release took back drops it and everything after it, so the reopen
// writes only the frames before the marker. A frame the DAC played is past
// cutting.
func TestSinkCutsAPausedStoreAndAReleasedOne(t *testing.T) {
	k := newModel(t)
	cfg := sinkConfig()
	cfg.ReleaseAfter = time.Second
	s := openTestSink(t, k, cfg)
	a, b := content(0, 0, 30000), content(1, 0, 20000)
	submit(t, s, join(a, b))
	await(t, k.when(starved), "the writer to fill the hardware buffer")
	k.play(t, 100*time.Millisecond)
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	cut(t, s, 30000, true)
	k.grant(2 * time.Second)
	released := waitState(t, s, SinkReleased)
	k.revoke()
	if released.Written != released.Played || released.Submitted != 30000 {
		t.Fatalf("released: %+v; want what the DAC had not played taken back, 30000 submitted", released)
	}
	cut(t, s, released.Played-1, false)
	at := released.Played + 1000
	cut(t, s, at, true)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	k.free()
	drain(t, s)
	sameFrames(t, "what the DAC heard", heard(k), a[:at*6])
	k.checkClosedEach(t)
}

// TestSinkCutRefusesAMarkerItDoesNotHold pins the refusals: a marker before
// the first frame or past the frames submitted, and any cut of a sink that
// has stopped, each typed.
func TestSinkCutRefusesAMarkerItDoesNotHold(t *testing.T) {
	k := newModel(t)
	s := openTestSink(t, k, sinkConfig())
	submit(t, s, content(0, 0, 1000))
	for _, at := range []int64{-1, 1001} {
		ok, err := s.Cut(at)
		if ok || ReasonOf(err) != ReasonBadConfig {
			t.Errorf("Cut(%d) = %v, %v; want refused, %s", at, ok, err, ReasonBadConfig)
		}
	}
	cut(t, s, 1000, true)
	if err := s.Drop(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Cut(1000); ok || ReasonOf(err) != ReasonClosed {
		t.Errorf("Cut of a dropped sink = %v, %v; want %s", ok, err, ReasonClosed)
	}
	if _, err := s.Write(context.Background(), content(0, 0, 1)); ReasonOf(err) != ReasonClosed {
		t.Errorf("a Write after the drop: %v", err)
	}
}
