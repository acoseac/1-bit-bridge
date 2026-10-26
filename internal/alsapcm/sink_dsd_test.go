package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"strconv"
	"testing"
	"time"
)

func nativeConfig() DSDConfig {
	return DSDConfig{Endpoint: testEndpoint, Format: format.DSDU32BE, BitRate: 2822400, Pause: PauseKernel, BufferTime: 40 * time.Millisecond}
}
func nativeKernel(t *testing.T) *fakeKernel {
	k := newModel(t)
	k.hw.formats = []uint32{49, 50, 52}
	k.hw.rates = append(k.hw.rates, 705600)
	return k
}
func nativeSink(t *testing.T, k *fakeKernel, c DSDConfig) *DSDSink {
	t.Helper()
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Release(context.Background(), Immediate); e != nil {
			t.Log(e)
		}
	})
	return s
}

// TestNativeOpenAndActivation pins explicit activation and prefill commitment.
func TestNativeOpenAndActivation(t *testing.T) {
	for _, action := range []string{"activate", "pause", "revoke"} {
		t.Run(action, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Prefill = time.Millisecond
			s := nativeSink(t, k, c)
			if len(given(k)) != 0 {
				t.Fatal("open wrote")
			}
			switch action {
			case "pause":
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				_ = s.Activate(context.Background(), 1)
			case "revoke":
				s.Revoke()
				_ = s.Activate(context.Background(), 1)
			case "activate":
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				if !s.Committed() {
					t.Fatal("prefill not committed")
				}
			}
			if action != "activate" && len(given(k)) != 0 {
				t.Fatal("fenced activation wrote")
			}
		})
	}
}

// TestNativeAdmissionRecheck changes evidence between open and activation.
func TestNativeAdmissionRecheck(t *testing.T) {
	for _, mode := range []string{"changed", "read_error"} {
		t.Run(mode, func(t *testing.T) {
			k := nativeKernel(t)
			bad := false
			c := nativeConfig()
			c.Prefill = time.Millisecond
			c.Check = func(NodeInfo) error {
				if bad {
					return errors.New(mode)
				}
				return nil
			}
			s := nativeSink(t, k, c)
			bad = true
			if e := s.Activate(context.Background(), 1); e == nil {
				t.Fatal("admitted changed evidence")
			}
			if len(given(k)) != 0 {
				t.Fatal("wrote before admission")
			}
		})
	}
}

// TestNativeTinyCarry preserves bit counts across compatible short occurrences.
func TestNativeTinyCarry(t *testing.T) {
	for _, bits := range []int{0, 1, 7, 15, 31} {
		t.Run(strconv.Itoa(bits), func(t *testing.T) {
			k := nativeKernel(t)
			k.free()
			s := nativeSink(t, k, nativeConfig())
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			n, e := s.WriteDSD(context.Background(), 1, []byte{0x81, 0x23, 0x45, 0x67}, []byte{0x2a, 0xcd, 0xef, 0x19}, 0, bits)
			if e != nil || n != bits {
				t.Fatalf("accepted %d/%d: %v", n, bits, e)
			}
			if s.Position().Submitted != int64(bits) {
				t.Fatal("carry not submitted")
			}
			if e := s.Finish(context.Background()); e != nil {
				t.Fatal(e)
			}
			p := s.Position()
			if p.Played != int64(bits) {
				t.Fatalf("padding counted: %+v", p)
			}
			wantFrames := (bits + 31) / 32
			if len(given(k)) != wantFrames*8 {
				t.Fatalf("frames=%d", len(given(k))/8)
			}
		})
	}
}

// TestNativeWriteUnitsAndCancellation pins logical per-channel bit ownership.
func TestNativeWriteUnitsAndCancellation(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	for _, tc := range []struct {
		name   string
		l, r   []byte
		off, n int
	}{{"unequal", []byte{1}, []byte{1, 2}, 0, 1}, {"negative", []byte{1}, []byte{2}, 0, -1}, {"overflow", []byte{1}, []byte{2}, 7, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			n, e := s.WriteDSD(context.Background(), 1, tc.l, tc.r, tc.off, tc.n)
			if e == nil || n != 0 {
				t.Fatal("accepted invalid counts")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, e := s.WriteDSD(ctx, 1, []byte{1}, []byte{2}, 0, 1); n != 0 || e == nil {
		t.Fatal("cancelled input accepted")
	}
}

// TestNativeCarryJoins checks asymmetric one-bit and seven-bit track joining.
func TestNativeCarryJoins(t *testing.T) {
	k := nativeKernel(t)
	k.free()
	s := nativeSink(t, k, nativeConfig())
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	for i := range 4 {
		if _, e := s.WriteDSD(context.Background(), uint64(i+1), []byte{0x80}, []byte{0}, 0, 1); e != nil {
			t.Fatal(e)
		}
		if _, e := s.WriteDSD(context.Background(), uint64(i+1), []byte{0x02}, []byte{0x46}, 0, 7); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(given(k), []byte{0x81, 0x81, 0x81, 0x81, 0x23, 0x23, 0x23, 0x23}) {
		t.Fatalf("carry join: %x", given(k))
	}
}

// TestNativeBufferAndPolicyBounds refuses oversized readback and invalid policy.
func TestNativeBufferAndPolicyBounds(t *testing.T) {
	for _, mode := range []string{"request", "readback", "pause", "rate"} {
		t.Run(mode, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			switch mode {
			case "request":
				c.BufferTime = time.Second
			case "readback":
				k.commit = func(hw *hwParams) { hw.setInteger(hwParamBufferSize, 88200) }
			case "pause":
				c.Pause = PauseAuto
			case "rate":
				c.BitRate = 123
			}
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now})
			if e == nil {
				_ = s.Release(context.Background(), Immediate)
				t.Fatal("accepted invalid setup")
			}
			if len(given(k)) != 0 {
				t.Fatal("invalid open wrote")
			}
		})
	}
}

// TestNativeTrackChangeCutoff cancels a partial write, preserving only its prefix.
func TestNativeTrackChangeCutoff(t *testing.T) {
	k := nativeKernel(t)
	k.writes = []fakeWrite{{take: 1}, {err: errnoEAGAIN}}
	k.polls = []fakePoll{{block: true}}
	s := nativeSink(t, k, nativeConfig())
	left := bytes.Repeat([]byte{0x81}, 16)
	right := bytes.Repeat([]byte{0x23}, 16)
	if _, e := s.WriteDSD(context.Background(), 1, left, right, 0, 128); e != nil {
		t.Fatal(e)
	}
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	await(t, k.entered, "partial write EAGAIN")
	k.free()
	if e := s.Release(context.Background(), TrackChange); e != nil {
		t.Fatal(e)
	}
	if len(given(k)) != 8 {
		t.Fatalf("unaccepted suffix submitted: %x", given(k))
	}
	s.mu.Lock()
	pending, carry := s.ring.pending(), s.carryBits
	s.mu.Unlock()
	if pending != 0 || carry != 0 {
		t.Fatal("cutoff retained software queue")
	}
	if s.Position().Written != 32 {
		t.Fatalf("prefix not accounted: %+v", s.Position())
	}
}

// TestNativeReleaseEscalation holds the drain while Stop escalates release.
func TestNativeReleaseEscalation(t *testing.T) {
	k := nativeKernel(t)
	g := &nativePollGate{sys: k, entered: make(chan struct{}), gate: make(chan struct{})}
	s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(g, k.now, ep, c) }, now: k.now})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Release(context.Background(), Immediate)
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
	await(t, k.when(filledAndIdle(1)), "queued word")
	g.armed.Store(true)
	answer := make(chan error, 1)
	go func() { answer <- s.Release(context.Background(), TrackChange) }()
	await(t, g.entered, "cancellable drain seam")
	start := time.Now()
	e = s.Release(context.Background(), Immediate)
	if e != nil || time.Since(start) > time.Second {
		t.Errorf("escalation did not cancel drain promptly: %v after %v", e, time.Since(start))
	}
	await(t, s.Done(), "Immediate cleanup")
	close(g.gate)
	<-answer
	k.checkClosedEach(t)
}
