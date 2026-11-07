package dsdconv

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"weak"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

func mustNew(t *testing.T, s *stubDSD, p Plan, opt Options) *Converter {
	t.Helper()
	c, err := New(s, p, opt)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wantErr(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Errorf("%s: %v, want %v", what, got, want)
	}
}

// chain is A continued into B and B into C, C adopted and held at its own
// window, then A gone back to its window (Rewind): B and C detached.
type chain struct {
	a, b, c    *Converter
	bs, cs     *stubDSD
	ka         int
	h, d       int
	streamA    [2][]byte
	rate       int
	plan       Plan
	readsCAtRw int
}

func detachedChain(t *testing.T) chain {
	t.Helper()
	rate, p := format.DSD64RateHz, Plan{176400, 24}
	d, _ := p.Decimation(rate)
	h, _ := p.Window(rate)
	ka, kb := 3*h+5, 3*h+7
	a := [2][]byte{randomBits(201, ka*d), randomBits(202, ka*d)}
	b := [2][]byte{randomBits(203, kb*d), randomBits(204, kb*d)}
	c := [2][]byte{randomBits(205, kb*d), randomBits(206, kb*d)}
	ch := chain{ka: ka, h: h, d: d, streamA: a, rate: rate, plan: p, bs: stubOf(rate, b), cs: stubOf(rate, c)}
	ch.a = mustNew(t, stubOf(rate, a), p, Options{Hold: true})
	if _, err := drain(t, ch.a, 64); !errors.Is(err, ErrWindow) {
		t.Fatalf("A's held read: %v", err)
	}
	ch.b = mustNew(t, ch.bs, p, Options{Hold: true})
	ch.c = mustNew(t, ch.cs, p, Options{Hold: true})
	if err := ch.a.Continue(ch.b); err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, ch.a, 64); !errors.Is(err, io.EOF) {
		t.Fatalf("A's window with B: %v", err)
	}
	if _, err := drain(t, ch.b, 64); !errors.Is(err, ErrWindow) || !ch.b.Holding() {
		t.Fatalf("B's held read: %v, holding %v", err, ch.b.Holding())
	}
	if err := ch.b.Continue(ch.c); err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, ch.b, 64); !errors.Is(err, io.EOF) {
		t.Fatalf("B's window with C: %v", err)
	}
	if _, err := drain(t, ch.c, 64); !errors.Is(err, ErrWindow) || !ch.c.Holding() {
		t.Fatalf("C's held read: %v, holding %v", err, ch.c.Holding())
	}
	if err := ch.a.Rewind(); err != nil {
		t.Fatal(err)
	}
	ch.readsCAtRw = ch.cs.calls
	for name, x := range map[string]*Converter{"B": ch.b, "C": ch.c} {
		if x.Holding() {
			t.Errorf("%s, detached, still says it holds", name)
		}
		_, err := x.ReadFrames(context.Background(), make([]int32, 8))
		wantErr(t, name+".ReadFrames, detached", err, ErrDetached)
	}
	return ch
}

// TestADetachedConverterTouchesNothingOfTheChain: once its predecessor goes
// back to its window (Rewind), a successor follows nothing, and it holds a
// reference to the chain it was handed, whose state is the predecessor's
// again: an End from it would mark the chain ended at the predecessor's
// boundary, so the successor joined next plays none of its frames, and a
// Continue from it would queue another occurrence's bytes into the
// predecessor's window. So a detached converter refuses both (ErrDetached),
// says it holds nothing, and its Close closes its own source alone; and the
// detachment reaches every occurrence after it, an adopted one two deep
// included. B and C each hold at their own window when A goes back.
func TestADetachedConverterTouchesNothingOfTheChain(t *testing.T) {
	t.Run("End, then the predecessor continues", func(t *testing.T) {
		ch := detachedChain(t)
		wantErr(t, "B.End, detached", ch.b.End(), ErrDetached)
		wantErr(t, "C.End, detached", ch.c.End(), ErrDetached)
		// D longer than the delay line holds, so its input has not all been
		// read when A's window hands the chain over.
		nd := 8*64*1024 + 3
		dd := [2][]byte{randomBits(209, nd), randomBits(210, nd)}
		ref := referenceRun(t, [2][]byte{concat(ch.streamA[0], dd[0]), concat(ch.streamA[1], dd[1])}, ch.rate, ch.plan, []int64{0, int64(ch.ka)})
		next := mustNew(t, stubOf(ch.rate, dd), ch.plan, Options{Hold: true})
		if err := ch.a.Continue(next); err != nil {
			t.Fatalf("A.Continue(D): %v", err)
		}
		win, err := drain(t, ch.a, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("A's window with D: %v", err)
		}
		eq(t, "A's window with D", win, ref.frames(ch.ka-ch.h, ch.ka))
		got, err := drain(t, next, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("D's end: %v", err)
		}
		eq(t, "D, every frame of it", got, ref.frames(ch.ka, ch.ka+nd/ch.d))
		if ch.cs.calls != ch.readsCAtRw {
			t.Errorf("C's source was read %d more times once C was detached", ch.cs.calls-ch.readsCAtRw)
		}
	})
	t.Run("Continue, then the predecessor ends", func(t *testing.T) {
		ch := detachedChain(t)
		e := [2][]byte{randomBits(207, 4*ch.h*ch.d), randomBits(208, 4*ch.h*ch.d)}
		es := stubOf(ch.rate, e)
		wantErr(t, "B.Continue, detached", ch.b.Continue(mustNew(t, es, ch.plan, Options{Hold: true})), ErrDetached)
		wantErr(t, "C.Continue, detached", ch.c.Continue(mustNew(t, es, ch.plan, Options{Hold: true})), ErrDetached)
		if err := ch.a.End(); err != nil {
			t.Fatalf("A.End: %v", err)
		}
		win, err := drain(t, ch.a, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("A's window, ended: %v", err)
		}
		ref := referenceRun(t, ch.streamA, ch.rate, ch.plan, nil)
		eq(t, "A's window, zero-padded", win, ref.frames(ch.ka-ch.h, ch.ka))
		if es.calls != 0 {
			t.Errorf("the occurrence a detached converter was asked to continue into was read %d times", es.calls)
		}
	})
	t.Run("Close", func(t *testing.T) {
		ch := detachedChain(t)
		for name, x := range map[string]struct {
			c *Converter
			s *stubDSD
		}{"B": {ch.b, ch.bs}, "C": {ch.c, ch.cs}} {
			if err := x.c.Close(); err != nil || x.s.closes != 1 {
				t.Errorf("%s.Close, detached: %v, its source closed %d times", name, err, x.s.closes)
			}
		}
		if err := ch.a.End(); err != nil {
			t.Fatal(err)
		}
		win, err := drain(t, ch.a, 64)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("A's window: %v", err)
		}
		ref := referenceRun(t, ch.streamA, ch.rate, ch.plan, nil)
		eq(t, "A's window after its successors closed", win, ref.frames(ch.ka-ch.h, ch.ka))
	})
}

// TestAPredecessorIsReleasedAtTheHandOver: continuous playback must not keep
// every occurrence it has played. Once a predecessor has handed the chain to
// its successor, nothing the successor holds reaches it, so a closed
// predecessor the caller drops is collected, its snapshot, its prefetch copy
// and its source with it, however long the chain. A weak pointer says so; no
// finalizer is installed (a saved converter's snapshot points into itself).
func TestAPredecessorIsReleasedAtTheHandOver(t *testing.T) {
	rate, p := 2*format.DSD64RateHz, Plan{88200, 24}
	next, gone, want := handedOver(t, rate, p)
	for i := 0; i < 8 && gone.Value() != nil; i++ {
		runtime.GC()
	}
	if gone.Value() != nil {
		t.Error("the predecessor stays reachable after it handed the chain on")
	}
	got, err := drain(t, next, 64)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("the successor's end: %v", err)
	}
	eq(t, "the successor after its predecessor was collected", got, want)
	runtime.KeepAlive(next)
}

// handedOver plays a predecessor's window into a successor, closes the
// predecessor and keeps nothing of it but a weak pointer.
func handedOver(t *testing.T, rate int, p Plan) (*Converter, weak.Pointer[Converter], []int32) {
	t.Helper()
	d, _ := p.Decimation(rate)
	h, _ := p.Window(rate)
	ka, kb := 2*h+3, 2*h+9
	a := [2][]byte{randomBits(211, ka*d), randomBits(212, ka*d)}
	b := [2][]byte{randomBits(213, kb*d+5), randomBits(214, kb*d+5)} // unaligned: B never holds
	ref := referenceRun(t, [2][]byte{concat(a[0], b[0]), concat(a[1], b[1])}, rate, p, []int64{0, int64(ka)})
	prev := mustNew(t, stubOf(rate, a), p, Options{Hold: true})
	if _, err := drain(t, prev, 64); !errors.Is(err, ErrWindow) {
		t.Fatalf("the predecessor's held read: %v", err)
	}
	next, _ := prefetched(t, rate, b, p)
	if err := prev.Continue(next); err != nil {
		t.Fatal(err)
	}
	if _, err := drain(t, prev, 64); !errors.Is(err, io.EOF) {
		t.Fatalf("the predecessor's window: %v", err)
	}
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}
	return next, weak.Make(prev), ref.frames(ka, ka+kb)
}

// TestAClosedPredecessorStillGuardsItsSuccessor: a successor is read only
// once its predecessor has handed it the chain. A predecessor closed after
// Continue and before its window was read never hands it over, and the
// successor must not start its own primed delay lines past its first byte
// as a fresh track: it keeps answering ErrJoin.
func TestAClosedPredecessorStillGuardsItsSuccessor(t *testing.T) {
	rate, p := format.DSD64RateHz, Plan{176400, 24}
	d, _ := p.Decimation(rate)
	h, _ := p.Window(rate)
	a := [2][]byte{randomBits(221, 2*h*d), randomBits(222, 2*h*d)}
	b := [2][]byte{randomBits(223, 2*h*d), randomBits(224, 2*h*d)}
	prev := mustNew(t, stubOf(rate, a), p, Options{Hold: true})
	if _, err := drain(t, prev, 64); !errors.Is(err, ErrWindow) {
		t.Fatalf("the predecessor's held read: %v", err)
	}
	next := mustNew(t, stubOf(rate, b), p, Options{Hold: true})
	if err := prev.Continue(next); err != nil {
		t.Fatal(err)
	}
	if err := prev.Close(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, err := next.ReadFrames(context.Background(), make([]int32, 64))
		wantErr(t, "the successor of a predecessor closed before its hand-over", err, ErrJoin)
	}
}
