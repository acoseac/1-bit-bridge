package dsdconv

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// prefetched is a converter of ch whose first chunk the backend's open has
// already read (B11 reads it to prove the content gives bits): the source
// is advanced past it, and the bytes are handed to New, which copies them;
// the caller's buffers are spoilt afterwards to prove it.
func prefetched(t *testing.T, rate int, ch [2][]byte, p Plan) (*Converter, *stubDSD) {
	t.Helper()
	s := stubOf(rate, ch)
	first := [][]byte{make([]byte, 4096), make([]byte, 4096)}
	n, err := s.ReadDSD(context.Background(), first)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	c, err := New(s, p, Options{Hold: true, First: [][]byte{first[0][:n], first[1][:n]}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range first[0] {
		first[0][i], first[1][i] = 0xAA, 0x55
	}
	return c, s
}

func eq(t *testing.T, what string, got, want []int32) {
	t.Helper()
	if !equal32(got, want) {
		at := -1
		for i := range got {
			if i >= len(want) || got[i] != want[i] {
				at = i
				break
			}
		}
		t.Errorf("%s: %d samples, want %d; first difference at sample %d", what, len(got), len(want), at)
	}
}

// TestTheJoinWindowRecomputesFromTheSnapshot is §4.7's join window. An
// occurrence held at its window point has given its frames but the last H,
// which depend on what follows; its snapshot is taken before any bit of a
// successor enters. A continuous successor computes the window with its
// bits and carries the delay lines on; Rewind goes back to the snapshot for
// a successor replaced or cut, and the window is computed again for the new
// one or for none, zero-padded, as often as asked, giving the same samples
// and the same counts each time: the window's frames are the predecessor's,
// replacing the earlier ones in its counts rather than adding to them. The
// end of A is overmodulated, so the window clips, by an amount that depends
// on what follows; at 16 bits, the dither must come back with the snapshot
// too. B and C were prefetched by their opens.
func TestTheJoinWindowRecomputesFromTheSnapshot(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {88200, 24}, {44100, 16}} {
			d, _ := p.Decimation(rate)
			h, _ := p.Window(rate)
			ka := 3*h + 7
			a := [2][]byte{concat(randomBits(71, (ka-2*h)*d), repeatByte(0xFF, 2*h*d)), randomBits(72, ka*d)}
			nb := (2*h+3)*d + 5
			b := [2][]byte{randomBits(73, nb), randomBits(74, nb)}
			c := [2][]byte{concat(repeatByte(0xFF, h*d), randomBits(75, nb-h*d)), randomBits(76, nb)}
			refA := referenceRun(t, a, rate, p, nil)
			refAB := referenceRun(t, [2][]byte{concat(a[0], b[0]), concat(a[1], b[1])}, rate, p, []int64{0, int64(ka)})
			refAC := referenceRun(t, [2][]byte{concat(a[0], c[0]), concat(a[1], c[1])}, rate, p, []int64{0, int64(ka)})
			if refA.count(ka-h, ka) == refAC.count(ka-h, ka) {
				t.Fatalf("%s %s: the window clips alike whatever follows: the fixture tests no count", rateName(rate), p)
			}
			name := rateName(rate) + " " + p.String()

			conv, err := New(stubOf(rate, a), p, Options{Hold: true})
			if err != nil {
				t.Fatal(err)
			}
			head, err := drain(t, conv, 37)
			if !errors.Is(err, ErrWindow) || !conv.Holding() || conv.Emitted() != int64(ka-h) {
				t.Fatalf("%s: the held read ended with %v, holding %v, after %d frames, want ErrWindow after %d", name, err, conv.Holding(), conv.Emitted(), ka-h)
			}
			eq(t, name+": the frames before the window", head, refA.frames(0, ka-h))
			atWindow := conv.Clipped()
			if atWindow != refA.count(0, ka-h) {
				t.Errorf("%s: %d clipped before the window, want %d", name, atWindow, refA.count(0, ka-h))
			}

			nextB, _ := prefetched(t, rate, b, p)
			if err := conv.Continue(nextB); err != nil {
				t.Fatalf("%s: Continue: %v", name, err)
			}
			winB, err := drain(t, conv, 13)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("%s: the window with B ended with %v", name, err)
			}
			eq(t, name+": the window with B", winB, refAB.frames(ka-h, ka))
			if conv.Emitted() != int64(ka) || conv.Clipped() != refAB.count(0, ka) {
				t.Errorf("%s: with B, Emitted %d and Clipped %d, want %d and %d", name, conv.Emitted(), conv.Clipped(), ka, refAB.count(0, ka))
			}
			bFrames, err := drain(t, nextB, 29)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("%s: B ended with %v", name, err)
			}
			eq(t, name+": B carried on from A's delay lines", bFrames, refAB.frames(ka, ka+nb/d))
			if nextB.Clipped() != refAB.count(ka, ka+nb/d) || nextB.Emitted() != int64(nb/d) {
				t.Errorf("%s: B counts %d clipped of %d frames, want %d of %d", name, nextB.Clipped(), nextB.Emitted(), refAB.count(ka, ka+nb/d), nb/d)
			}

			for round := 0; round < 4; round++ {
				if err := conv.Rewind(); err != nil {
					t.Fatalf("%s round %d: Rewind: %v", name, round, err)
				}
				if !conv.Holding() || conv.Emitted() != int64(ka-h) || conv.Clipped() != atWindow {
					t.Fatalf("%s round %d: after Rewind, holding %v, Emitted %d, Clipped %d; want the snapshot's %d and %d",
						name, round, conv.Holding(), conv.Emitted(), conv.Clipped(), ka-h, atWindow)
				}
				if _, err := nextB.ReadFrames(context.Background(), make([]int32, 8)); !errors.Is(err, ErrDetached) && round == 0 {
					t.Errorf("%s: B read after A went back to its window: %v, want ErrDetached", name, err)
				}
				nextC, _ := prefetched(t, rate, c, p)
				if err := conv.Continue(nextC); err != nil {
					t.Fatalf("%s round %d: Continue to C: %v", name, round, err)
				}
				winC, err := drain(t, conv, 1+round*7)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s round %d: the window with C ended with %v", name, round, err)
				}
				eq(t, name+": the window recomputed for C", winC, refAC.frames(ka-h, ka))
				if conv.Emitted() != int64(ka) || conv.Clipped() != refAC.count(0, ka) {
					t.Errorf("%s round %d: with C, Emitted %d and Clipped %d, want %d and %d", name, round, conv.Emitted(), conv.Clipped(), ka, refAC.count(0, ka))
				}
				cFrames, err := drain(t, nextC, 64)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s round %d: C ended with %v", name, round, err)
				}
				eq(t, name+": C carried on from A's delay lines", cFrames, refAC.frames(ka, ka+nb/d))

				if err := conv.Rewind(); err != nil {
					t.Fatal(err)
				}
				if err := conv.End(); err != nil {
					t.Fatalf("%s round %d: End: %v", name, round, err)
				}
				winNone, err := drain(t, conv, 5)
				if !errors.Is(err, io.EOF) {
					t.Fatalf("%s round %d: the window with nothing after ended with %v", name, round, err)
				}
				eq(t, name+": the window recomputed for none, zero-padded", winNone, refA.frames(ka-h, ka))
				if conv.Emitted() != int64(ka) || conv.Clipped() != refA.count(0, ka) {
					t.Errorf("%s round %d: with none, Emitted %d and Clipped %d, want %d and %d", name, round, conv.Emitted(), conv.Clipped(), ka, refA.count(0, ka))
				}
			}
		}
	}
}

// TestAnAbuttingJoinIsZeroPadded: a successor at another DSD rate on the
// same PCM transport (Argon's case), or one the predecessor ended for (the
// lookahead's floor reached, say), abuts: the predecessor's window is
// flushed with numeric zeros, a new converter primed with zeros starts the
// successor, and nothing is inserted, so each is what it is alone.
func TestAnAbuttingJoinIsZeroPadded(t *testing.T) {
	p := Plan{176400, 24}
	r64, r128 := format.DSD64RateHz, 2*format.DSD64RateHz
	d64, _ := p.Decimation(r64)
	h64, _ := p.Window(r64)
	d128, _ := p.Decimation(r128)
	a := [2][]byte{randomBits(81, (3*h64+1)*d64), randomBits(82, (3*h64+1)*d64)}
	b := [2][]byte{randomBits(83, 500*d128+9), randomBits(84, 500*d128+9)}
	fa := format.NativeDSD{BitRateHz: r64, Channels: 2, Container: format.ContainerDSF, Frames: int64(len(a[0]))}
	fb := format.NativeDSD{BitRateHz: r128, Channels: 2, Container: format.ContainerDSF, Frames: int64(len(b[0]))}
	if Continuous(fa, p, fb, p) {
		t.Fatal("DSD64 then DSD128 is continuous")
	}
	wantA, _ := reference(t, a, r64, p)
	wantB, _ := reference(t, b, r128, p)
	conv, err := New(stubOf(r64, a), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	head, err := drain(t, conv, 100)
	if !errors.Is(err, ErrWindow) {
		t.Fatalf("the held read ended with %v", err)
	}
	next, err := New(stubOf(r128, b), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Continue(next); !errors.Is(err, ErrJoin) || !conv.Holding() {
		t.Fatalf("Continue to DSD128: %v, holding %v; want ErrJoin and still holding", err, conv.Holding())
	}
	if err := conv.End(); err != nil {
		t.Fatal(err)
	}
	tail, err := drain(t, conv, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("A's end: %v", err)
	}
	eq(t, "A, ended for an abutting successor", append(head, tail...), wantA)
	got, err := drain(t, next, 100)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("B's end: %v", err)
	}
	eq(t, "B, primed with zeros", got, wantB)

	// A predecessor whose Frames is not a multiple of D, long as it is, does
	// not hold: there is no window point to continue from, and it ends
	// zero-padded as it does alone.
	for _, extra := range []int{1, 8, d64 - 1} {
		n := 10*h64*d64 + extra
		un := [2][]byte{randomBits(85, n), randomBits(86, n)}
		want, _ := reference(t, un, r64, p)
		c, err := New(stubOf(r64, un), p, Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(t, c, 100)
		if !errors.Is(err, io.EOF) || c.Holding() {
			t.Errorf("%d bits past alignment: the held read ended with %v, holding %v; want io.EOF, no hold", extra, err, c.Holding())
		}
		eq(t, "an unaligned predecessor", got, want)
	}
}

// TestTwoSeventeenBitTracksAtD16Abut: a predecessor whose Frames is not a
// multiple of D has no phase that keeps both its own k·D and the continuous
// stream's, so its join is abutting: two 17-bit tracks at D = 16 are one
// frame each, each what it is alone, and the first does not hold.
func TestTwoSeventeenBitTracksAtD16Abut(t *testing.T) {
	p, rate := Plan{176400, 24}, format.DSD64RateHz
	f := format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: 17}
	if Continuous(f, p, f, p) {
		t.Fatal("two 17-bit tracks at D = 16 are continuous")
	}
	for seed := uint64(1); seed <= 2; seed++ {
		ch := [2][]byte{randomBits(90+seed, 17), randomBits(92+seed, 17)}
		want, _ := reference(t, ch, rate, p)
		c, err := New(stubOf(rate, ch), p, Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		got, err := drain(t, c, 4)
		if !errors.Is(err, io.EOF) || len(got) != 2 {
			t.Fatalf("a 17-bit track gives %d samples and %v, want one frame and io.EOF (it cannot hold)", len(got), err)
		}
		eq(t, "a 17-bit track", got, want)
	}
}

// TestAShortSuccessorKeepsItsSuccessorOutOfThePredecessor: a successor
// shorter than H frames would let A's tail depend on B and on C, so A, a
// short B, then C abut: A's frames and B's are the same whether C is
// there, replaced or removed.
func TestAShortSuccessorKeepsItsSuccessorOutOfThePredecessor(t *testing.T) {
	p, rate := Plan{176400, 24}, 2*format.DSD64RateHz
	d, _ := p.Decimation(rate)
	h, _ := p.Window(rate)
	a := [2][]byte{randomBits(101, 4*h*d), randomBits(102, 4*h*d)}
	b := [2][]byte{randomBits(103, (h-1)*d), randomBits(104, (h-1)*d)}
	fa := format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: int64(len(a[0]))}
	fb := format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: int64(len(b[0]))}
	if Continuous(fa, p, fb, p) || Continuous(fb, p, fa, p) {
		t.Fatal("a successor or a predecessor shorter than H frames is continuous")
	}
	var firstA, firstB []int32
	for i, c := range [][2][]byte{{randomBits(105, 9*d), randomBits(106, 9*d)}, {repeatByte(0xFF, 9*d), repeatByte(0, 9*d)}, {}} {
		conv, err := New(stubOf(rate, a), p, Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		head, err := drain(t, conv, 64)
		if !errors.Is(err, ErrWindow) {
			t.Fatalf("A's held read: %v", err)
		}
		next, err := New(stubOf(rate, b), p, Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := conv.Continue(next); !errors.Is(err, ErrJoin) {
			t.Fatalf("Continue to a short B: %v, want ErrJoin", err)
		}
		if err := conv.End(); err != nil {
			t.Fatal(err)
		}
		tail, _ := drain(t, conv, 64)
		gotA := append(head, tail...)
		gotB, err := drain(t, next, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("a short B holds or fails: %v", err)
		}
		if c[0] != nil {
			third, err := New(stubOf(rate, c), p, Options{Hold: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := drain(t, third, 64); !errors.Is(err, io.EOF) {
				t.Fatalf("C: %v", err)
			}
		}
		if i == 0 {
			firstA, firstB = gotA, gotB
			wantA, _ := reference(t, a, rate, p)
			wantB, _ := reference(t, b, rate, p)
			eq(t, "A before a short B", gotA, wantA)
			eq(t, "a short B", gotB, wantB)
			continue
		}
		eq(t, "A, with C replaced or removed", gotA, firstA)
		eq(t, "B, with C replaced or removed", gotB, firstB)
	}
}

// TestAContinuousJoinNeedsTheSamePlanAlignmentAndLength is the one rule B11
// classifies a converted join by: the same plan and DSD rate, a
// predecessor whose Frames is a multiple of D, and both at least H frames
// long; everything else abuts. Continue takes only such a successor, fresh,
// once, at the window point.
func TestAContinuousJoinNeedsTheSamePlanAlignmentAndLength(t *testing.T) {
	p, rate := Plan{176400, 24}, format.DSD64RateHz
	d, _ := p.Decimation(rate)
	h, _ := p.Window(rate)
	f := func(rate int, frames int) format.NativeDSD {
		return format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDFF, Frames: int64(frames)}
	}
	long := 10 * h * d
	for _, c := range []struct {
		name       string
		pred, succ format.NativeDSD
		pp, sp     Plan
		want       bool
	}{
		{"same plan and rate, aligned, long", f(rate, long), f(rate, long+3), p, p, true},
		{"each exactly H frames", f(rate, h*d), f(rate, h*d), p, p, true},
		{"another plan's rate", f(rate, long), f(rate, long), p, Plan{88200, 24}, false},
		{"another plan's bits", f(rate, long), f(rate, long), p, Plan{176400, 16}, false},
		{"another DSD rate", f(rate, long), f(2*rate, long), p, p, false},
		{"a predecessor one bit past alignment", f(rate, long+1), f(rate, long), p, p, false},
		{"a predecessor a byte past alignment", f(rate, long+8), f(rate, long), p, p, false},
		{"a predecessor of H-1 frames", f(rate, (h-1)*d), f(rate, long), p, p, false},
		{"a successor of H-1 frames", f(rate, long), f(rate, h*d-1), p, p, false},
		{"a plan that is none", f(rate, long), f(rate, long), Plan{1, 24}, Plan{1, 24}, false},
		{"DSD512", f(8*rate, 8*long), f(8*rate, 8*long), p, p, false},
		{"mono", format.NativeDSD{BitRateHz: rate, Channels: 1, Frames: int64(long)}, f(rate, long), p, p, false},
	} {
		if got := Continuous(c.pred, c.pp, c.succ, c.sp); got != c.want {
			t.Errorf("%s: Continuous = %v, want %v", c.name, got, c.want)
		}
	}

	ch := [2][]byte{randomBits(111, 3*h*d), randomBits(112, 3*h*d)}
	conv, err := New(stubOf(rate, ch), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	early, err := New(stubOf(rate, ch), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Continue(early); !errors.Is(err, ErrJoin) {
		t.Errorf("Continue before the window point: %v, want ErrJoin", err)
	}
	if _, err := drain(t, conv, 64); !errors.Is(err, ErrWindow) {
		t.Fatalf("the held read: %v", err)
	}
	if err := conv.Continue(nil); !errors.Is(err, ErrJoin) {
		t.Errorf("Continue(nil): %v, want ErrJoin", err)
	}
	read, err := New(stubOf(rate, ch), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read.ReadFrames(context.Background(), make([]int32, 2)); err != nil {
		t.Fatal(err)
	}
	if err := conv.Continue(read); !errors.Is(err, ErrJoin) {
		t.Errorf("Continue to a successor already read: %v, want ErrJoin", err)
	}
	fresh, err := New(stubOf(rate, ch), p, Options{Hold: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := conv.Continue(fresh); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	other, _ := New(stubOf(rate, ch), p, Options{Hold: true})
	if err := conv.Continue(other); !errors.Is(err, ErrJoin) {
		t.Errorf("a second Continue: %v, want ErrJoin", err)
	}
	if err := conv.End(); !errors.Is(err, ErrJoin) {
		t.Errorf("End after Continue: %v, want ErrJoin", err)
	}
	if _, err := fresh.ReadFrames(context.Background(), make([]int32, 2)); !errors.Is(err, ErrJoin) {
		t.Errorf("the successor read before its predecessor's window: %v, want ErrJoin", err)
	}
}
