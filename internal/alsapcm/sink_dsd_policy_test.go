package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"testing"
	"time"
)

// TestNativeSilenceAndCeil checks literal canonical silence, including EOF bits.
func TestNativeSilenceAndCeil(t *testing.T) {
	t.Run("rounding", func(t *testing.T) {
		if nativeFrames(88200, time.Millisecond) != 89 {
			t.Fatal("silence rounded down")
		}
	})
	t.Run("prefill", func(t *testing.T) {
		k := nativeKernel(t)
		c := nativeConfig()
		c.Prefill = time.Millisecond
		s := nativeSink(t, k, c)
		if e := s.Activate(context.Background(), 1); e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(given(k), bytes.Repeat([]byte{0x69}, 89*8)) {
			t.Fatal("native prefill is not 0x69")
		}
	})
	t.Run("padding", func(t *testing.T) {
		k := nativeKernel(t)
		k.free()
		s := nativeSink(t, k, nativeConfig())
		if _, e := s.WriteDSD(context.Background(), 1, []byte{0x80}, []byte{0}, 0, 1); e != nil {
			t.Fatal(e)
		}
		if e := s.Activate(context.Background(), 1); e != nil {
			t.Fatal(e)
		}
		if e := s.Finish(context.Background()); e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(given(k), []byte{0xe9, 0x69, 0x69, 0x69, 0x69, 0x69, 0x69, 0x69}) {
			t.Fatalf("EOF padding: %x", given(k))
		}
	})
}

// TestNativeOccurrenceBound refuses a fifth tiny occurrence without waiting for audibility.
func TestNativeOccurrenceBound(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.LedgerLimit = 4
	s := nativeSink(t, k, c)
	for i := range 4 {
		n, e := s.WriteDSD(context.Background(), uint64(i+1), []byte{0x80}, []byte{0}, 0, 1)
		if e != nil || n != 1 {
			t.Fatal(n, e)
		}
	}
	n, e := s.WriteDSD(context.Background(), 5, []byte{0x80}, []byte{0}, 0, 1)
	if e == nil || n != 0 || s.Position().Submitted != 4 {
		t.Fatal("occurrence bound did not refuse")
	}
}

// TestNativePartialRetry fills an inert ring, cancels, and retries only its suffix.
func TestNativePartialRetry(t *testing.T) {
	k := nativeKernel(t)
	k.free()
	c := nativeConfig()
	c.RingBytes = 40000
	s := nativeSink(t, k, c)
	left := bytes.Repeat([]byte{0x81}, 24000)
	right := bytes.Repeat([]byte{0x23}, 24000)
	ctx, cancel := context.WithCancel(context.Background())
	answer := make(chan int, 1)
	go func() {
		n, e := s.WriteDSD(ctx, 1, left, right, 0, len(left)*8)
		if !errors.Is(e, context.Canceled) {
			t.Error(e)
		}
		answer <- n
	}()
	for {
		s.mu.Lock()
		if s.ring.free() == 0 {
			s.mu.Unlock()
			break
		}
		ch := s.changed
		s.mu.Unlock()
		<-ch
	}
	cancel()
	n := <-answer
	if n != 40000/8*32 {
		t.Fatalf("accepted %d", n)
	}
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	rest, e := s.WriteDSD(context.Background(), 1, left, right, n, len(left)*8-n)
	if e != nil || n+rest != len(left)*8 {
		t.Fatal(rest, e)
	}
	if e := s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	want := bytes.Repeat([]byte{0x81, 0x81, 0x81, 0x81, 0x23, 0x23, 0x23, 0x23}, len(left)/4)
	if !bytes.Equal(given(k), want) {
		t.Fatal("retry duplicated or lost accepted bits")
	}
}

// TestNativeTransportIdentity keeps logical rate and packing version in the key.
func TestNativeTransportIdentity(t *testing.T) {
	base := Transport{Kind: format.KindNativeDSD, Format: format.DSDU32BE, RateHz: 88200, Channels: 2, ValidBits: 1, LogicalRate: 2822400, PackingVersion: 1}
	for _, field := range []string{"kind", "format", "rate", "channels", "precision", "logical", "packing"} {
		t.Run(field, func(t *testing.T) {
			next := base
			switch field {
			case "kind":
				next.Kind = format.KindPCM
			case "format":
				next.Format = format.DSDU32LE
			case "rate":
				next.RateHz++
			case "channels":
				next.Channels++
			case "precision":
				next.ValidBits++
			case "logical":
				next.LogicalRate++
			case "packing":
				next.PackingVersion++
			}
			if base.Join(next).Gapless {
				t.Fatal("different transport joined")
			}
		})
	}
}

// TestNativePauseReopen retains the unplayed ring and refuses a busy reopen.
func TestNativePauseReopen(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(29 * time.Second)
	await(t, k.when(func(k *fakeKernel) bool { return starved(k) || k.closes > 0 }), "29 s pause")
	closed := false
	k.inspect(func(k *fakeKernel) { closed = k.closes > 0 })
	if closed || s.Position().Released {
		t.Fatal("pause released before 30 s")
	}
	k.grant(2 * time.Second)
	for {
		s.mu.Lock()
		if s.released {
			s.mu.Unlock()
			break
		}
		ch := s.changed
		s.mu.Unlock()
		<-ch
	}
	k.revoke()
	k.inspect(func(k *fakeKernel) { k.openErr = errnoEBUSY })
	if e := s.Resume(context.Background()); ReasonOf(e) != ReasonBusy {
		t.Fatalf("resume: %v", e)
	}
	if p := s.Position(); !p.Paused || !p.Released {
		t.Fatalf("refused resume changed pause: %+v", p)
	}
	k.inspect(func(k *fakeKernel) { k.openErr = nil })
	if e := s.Resume(context.Background()); e != nil {
		t.Fatal(e)
	}
}

// TestNativeFaultNeverRecovers verifies xrun is terminal, including during final EOF.
func TestNativeFaultNeverRecovers(t *testing.T) {
	k := nativeKernel(t)
	k.writes = []fakeWrite{{err: errnoEPIPE}}
	s := nativeSink(t, k, nativeConfig())
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	await(t, s.Done(), "native xrun close")
	if s.result() == nil {
		t.Fatal("xrun concealed")
	}
	k.inspect(func(k *fakeKernel) {
		n := 0
		for _, r := range k.calls {
			if r == ioctlPrepare {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("Recover issued PREPARE: %d", n)
		}
	})
}

// TestNativeRateMatrix pins every requested format/logical-rate carrier pair.
func TestNativeRateMatrix(t *testing.T) {
	for _, f := range []format.ALSAFormat{format.DSDU32BE, format.DSDU32LE, format.DSDU16LE} {
		for _, rate := range []int{2822400, 5644800, 11289600} {
			t.Run(fmt.Sprintf("%s/%d", f, rate), func(t *testing.T) {
				k := nativeKernel(t)
				c := nativeConfig()
				c.Format = f
				c.BitRate = rate
				s := nativeSink(t, k, c)
				p := s.Params()
				k.inspect(func(k *fakeKernel) {
					code, _ := k.committed.maskOf(hwParamFormat).single()
					want := map[format.ALSAFormat]uint32{format.DSDU32BE: 52, format.DSDU32LE: 50, format.DSDU16LE: 49}[f]
					if code != want {
						t.Errorf("native format code %d, want %d", code, want)
					}
				})
				want := rate / (8 * format.DSDWordBytes(f))
				if p.RateHz != want || p.Format != f {
					t.Fatalf("opened %+v", p)
				}
				tr := s.Transport()
				if tr.LogicalRate != rate || tr.PackingVersion != 1 || tr.Kind != format.KindNativeDSD {
					t.Fatal(tr)
				}
			})
		}
	}
}

// TestNativeInertAdmission queues a word before activation without submitting it.
func TestNativeInertAdmission(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	_, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
	if e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active {
		t.Fatal("open activated writer")
	}
	if e := s.Release(context.Background(), Immediate); e != nil {
		t.Fatal(e)
	}
	if len(given(k)) != 0 {
		t.Fatal("inert sink wrote")
	}
}

// TestNativeRequestedBufferBound isolates requested geometry from readback.
func TestNativeRequestedBufferBound(t *testing.T) {
	c := nativeConfig()
	c.BufferTime = time.Second
	if _, e := c.validate(); e == nil {
		t.Fatal("oversized requested buffer")
	}
}

// TestNativeMemoryBound isolates the preallocation bound before an open.
func TestNativeMemoryBound(t *testing.T) {
	c := nativeConfig()
	c.RingBytes = MaxRingBytes
	if _, e := c.validate(); e == nil {
		t.Fatal("no room for ledger and scratch")
	}
}

// TestNativeTrackChangeTail sends only qualified silence after its fixed cutoff.
func TestNativeTrackChangeTail(t *testing.T) {
	k := nativeKernel(t)
	k.free()
	c := nativeConfig()
	c.Tail = time.Millisecond
	s := nativeSink(t, k, c)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Release(context.Background(), TrackChange); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(given(k), bytes.Repeat([]byte{0x69}, 89*8)) || s.Position().Played != 0 {
		t.Fatal("tail missing, corrupted or counted as content")
	}
}
