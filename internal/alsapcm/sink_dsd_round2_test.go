package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"sync/atomic"
	"testing"
	"time"
)

func nativeWait(t *testing.T, s *DSDSink, what string, cond func() bool) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		s.mu.Lock()
		ok := cond()
		ch := s.changed
		s.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-ch:
		case <-timeout.C:
			t.Fatal(what)
		}
	}
}

func TestNativeInertResume(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Prefill = time.Millisecond
	s := nativeSink(t, k, c)
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	e := s.Resume(context.Background())
	if e == nil || len(given(k)) != 0 {
		t.Fatalf("inert Resume activated: err=%v bytes=%d", e, len(given(k)))
	}
}

func TestNativeResumeStatusFence(t *testing.T) {
	for _, action := range []string{"pause", "stop"} {
		t.Run(action, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			var count atomic.Int32
			parked, release := make(chan struct{}), make(chan struct{})
			c.Check = func(NodeInfo) error {
				if count.Add(1) == 3 {
					k.inspect(func(k *fakeKernel) { k.statusParked = parked; k.statusGate = release })
				}
				return nil
			}
			s := nativeSink(t, k, c)
			// Ensure running before kernel PAUSE.
			data := bytes.Repeat([]byte{0x81}, s.params.BufferFrames*4)
			_, _ = s.WriteDSD(context.Background(), 1, data, data, 0, len(data)*8)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "running")
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			answer := make(chan error, 1)
			go func() { answer <- s.Resume(context.Background()) }()
			await(t, parked, "resume STATUS")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if action == "pause" {
				_ = s.Pause(ctx)
			} else {
				_ = s.Release(ctx, Immediate)
			}
			close(release)
			if e := <-answer; e == nil {
				t.Error("stale Resume succeeded")
			}
			_ = s.Release(context.Background(), Immediate)
			k.inspect(func(k *fakeKernel) {
				for _, p := range k.pauses {
					if p == 0 {
						t.Error("stale Resume unpaused kernel")
					}
				}
			})
		})
	}
}

func TestNativeFencedReopenCloses(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	var count atomic.Int32
	parked, release := make(chan struct{}), make(chan struct{})
	c.Check = func(NodeInfo) error {
		if count.Add(1) == 3 {
			close(parked)
			<-release
		}
		return nil
	}
	s := nativeSink(t, k, c)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(31 * time.Second)
	nativeWait(t, s, "pause release", func() bool { return s.released })
	k.revoke()
	answer := make(chan error, 1)
	go func() { answer <- s.Resume(context.Background()) }()
	await(t, parked, "reopen admission")
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-answer; e == nil {
		t.Fatal("fenced reopen admitted")
	}
	k.inspect(func(k *fakeKernel) {
		if k.isOpen {
			t.Error("fenced reopen retained open device")
		}
	})
}

func TestNativeReopenCleanupSticky(t *testing.T) {
	for _, mode := range []string{"direct", "parked"} {
		t.Run(mode, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			var fail atomic.Bool
			c.Check = func(NodeInfo) error {
				if fail.Load() {
					return errors.New("admission rejected")
				}
				return nil
			}
			a := &nativeAuditSys{sys: k, t: t, where: ioctlHWFree, parked: make(chan struct{}), release: make(chan struct{})}
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Release(context.Background(), Immediate)
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e = s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			k.grant(31 * time.Second)
			nativeWait(t, s, "released", func() bool { return s.released })
			k.revoke()
			a.closeErr = errors.New("cleanup failed")
			fail.Store(true)
			answer := make(chan error, 1)
			if mode == "parked" {
				a.armed.Store(true)
			}
			go func() { answer <- s.Resume(context.Background()) }()
			if mode == "parked" {
				await(t, a.parked, "failed reopen cleanup")
				if e = s.Resume(context.Background()); e == nil {
					t.Error("activation admitted while cleanup parked")
				}
				select {
				case <-s.Done():
					t.Error("cleanup ownership lost")
				default:
				}
				close(a.release)
			}
			if e = <-answer; !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatal(e)
			}
			fail.Store(false)
			if e = s.Resume(context.Background()); e == nil {
				t.Error("reactivated after failed cleanup")
			}
			if e = s.Release(context.Background(), Immediate); !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Errorf("release lost cleanup fault: %v", e)
			}
			k.inspect(func(k *fakeKernel) {
				if k.opens != 2 {
					t.Errorf("later activation opened: %d", k.opens)
				}
			})
		})
	}
}

func TestNativeFinishReleaseBudget(t *testing.T) {
	for _, where := range []struct {
		name string
		req  uint32
	}{{"drop", ioctlDrop}, {"hw_free", ioctlHWFree}, {"close", 0}} {
		t.Run(where.name, func(t *testing.T) {
			k := nativeKernel(t)
			k.free()
			a := &nativeAuditSys{sys: k, t: t, where: where.req, parked: make(chan struct{}), release: make(chan struct{})}
			s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			a.armed.Store(true)
			answer := make(chan error, 1)
			go func() { answer <- s.Finish(context.Background()) }()
			await(t, a.parked, "natural cleanup")
			k.advanceNow(2 * time.Second)
			select {
			case e = <-answer:
				if !errors.Is(e, ErrReleaseUnconfirmed) {
					t.Errorf("Finish: %v", e)
				}
			case <-time.After(2500 * time.Millisecond):
				t.Error("Finish exceeded shared release deadline")
			}
			select {
			case <-s.Done():
				t.Error("cleanup ownership lost")
			default:
			}
			close(a.release)
			await(t, s.Done(), "eventual cleanup")
		})
	}
}

type nativePollGate struct {
	sys
	armed         atomic.Bool
	entered, gate chan struct{}
}

func (g *nativePollGate) poll(ctx context.Context, fd int, events int16, timeout time.Duration) (int16, error) {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-g.gate:
		}
	}
	return g.sys.poll(ctx, fd, events, timeout)
}
func TestNativeRepeatedReleaseJoins(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Tail = time.Millisecond
	g := &nativePollGate{sys: k, entered: make(chan struct{}), gate: make(chan struct{})}
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(g, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Release(context.Background(), Immediate)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	// Wait for the idle writer before arming the tail's write wait.
	await(t, k.when(func(k *fakeKernel) bool { return starved(k) }), "idle writer")
	k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEAGAIN}} })
	g.armed.Store(true)
	first := make(chan error, 1)
	go func() { first <- s.Release(context.Background(), TrackChange) }()
	await(t, g.entered, "tail poll")
	s.mu.Lock()
	ctx := s.releaseCtx
	deadline := s.releaseDeadline
	s.mu.Unlock()
	second := make(chan error, 1)
	go func() { second <- s.Release(context.Background(), TrackChange) }()
	time.Sleep(20 * time.Millisecond)
	s.mu.Lock()
	same := ctx == s.releaseCtx && deadline == s.releaseDeadline
	s.mu.Unlock()
	k.free()
	close(g.gate)
	if e := <-first; e != nil {
		t.Errorf("same-mode interrupted original release: %v", e)
	}
	if e := <-second; e != nil {
		t.Errorf("same-mode join: %v", e)
	}
	if !same {
		t.Error("same-mode reset deadline")
	}
	if !bytes.Equal(given(k), bytes.Repeat([]byte{0x69}, 89*8)) {
		t.Errorf("same-mode lost qualified tail: %d bytes", len(given(k)))
	}
}

type nativeCleanupContext struct {
	context.Context
	done <-chan struct{}
}

func (c nativeCleanupContext) Done() <-chan struct{} { <-c.done; return nil }

func TestNativeActivationFailureDelivered(t *testing.T) {
	for range 100 {
		k := nativeKernel(t)
		c := nativeConfig()
		var calls atomic.Int32
		want := errors.New("admission failure")
		c.Check = func(NodeInfo) error {
			if calls.Add(1) > 1 {
				return want
			}
			return nil
		}
		s := nativeSink(t, k, c)
		if e := s.Activate(nativeCleanupContext{context.Background(), s.Done()}, 1); !errors.Is(e, want) {
			t.Fatalf("waiter lost activation failure: %v", e)
		}
		await(t, s.Done(), "failed activation cleanup")
	}
}

func TestNativePauseRetainsWords(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Prefill = time.Millisecond
	s := nativeSink(t, k, c)
	left := bytes.Repeat([]byte{0x81, 0x23, 0x45, 0x67}, s.params.BufferFrames+17)
	right := bytes.Repeat([]byte{0x2a, 0xcd, 0xef, 0x19}, s.params.BufferFrames+17)
	_, e := s.WriteDSD(context.Background(), 1, left, right, 0, len(left)*8)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "held native buffer")
	if e = s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(31 * time.Second)
	nativeWait(t, s, "pause release", func() bool { return s.released })
	k.revoke()
	before := len(given(k))
	if e = s.Resume(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.free()
	if e = s.Finish(context.Background()); e != nil {
		t.Fatal(e)
	}
	want := append(bytes.Repeat([]byte{0x69}, 89*8), bytes.Repeat([]byte{0x81, 0x23, 0x45, 0x67, 0x2a, 0xcd, 0xef, 0x19}, len(left)/4)...)
	if !bytes.Equal(given(k)[before:], want) {
		t.Fatalf("resume lost retained words: got %d want %d bytes", len(given(k))-before, len(want))
	}
}

func TestNativeFrameWidths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
	}{{"DSD_U16_LE", 4}, {"DSD_U32_LE", 8}, {"DSD_U32_BE", 8}} {
		t.Run(tc.name, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Format = format.ALSAFormat(tc.name)
			if got := (Params{Format: c.Format, Channels: 2}).NativeFrameBytes(); got != tc.width {
				t.Fatalf("native frame width=%d want=%d", got, tc.width)
			}
			s := nativeSink(t, k, c)
			if got := s.Params().NativeFrameBytes(); got != tc.width {
				t.Fatalf("native frame width=%d want=%d", got, tc.width)
			}
		})
	}
}

func TestNativeInitialCleanupErrors(t *testing.T) {
	for _, stage := range []string{"setup", "admission", "geometry"} {
		for _, where := range []string{"drop", "hw_free", "close"} {
			t.Run(stage+"/"+where, func(t *testing.T) {
				k := nativeKernel(t)
				c := nativeConfig()
				want := errors.New("cleanup failed")
				a := &nativeAuditSys{sys: k, t: t}
				if where == "close" {
					a.closeErr = want
				} else {
					req := ioctlDrop
					if where == "hw_free" {
						req = ioctlHWFree
					}
					k.fail[req] = want
				}
				if stage == "admission" {
					c.Check = func(NodeInfo) error { return errors.New("admission rejected") }
				}
				if stage == "setup" {
					k.commit = func(h *hwParams) { h.rateNum++ }
				}
				if stage == "geometry" {
					k.hw.info &^= infoPause
				}
				_, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
				if !errors.Is(e, want) || !errors.Is(e, ErrReleaseUnconfirmed) {
					t.Fatalf("initial refusal lost cleanup error: %v", e)
				}
				k.checkClosedEach(t)
			})
		}
	}
}

func TestNativeInitialCleanupParked(t *testing.T) {
	for _, stage := range []string{"setup", "admission"} {
		t.Run(stage, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			a := &nativeAuditSys{sys: k, t: t, where: ioctlHWFree, parked: make(chan struct{}), release: make(chan struct{})}
			if stage == "setup" {
				k.commit = func(h *hwParams) { h.rateNum++ }
			} else {
				c.Check = func(NodeInfo) error { return errors.New("refused") }
			}
			a.armed.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answer := make(chan error, 1)
			go func() {
				_, e := openNativeBounded(ctx, c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
				answer <- e
			}()
			await(t, a.parked, "initial refusal cleanup")
			cancel()
			select {
			case e := <-answer:
				if !errors.Is(e, ErrReleaseUnconfirmed) {
					t.Fatal(e)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled initial refusal held caller")
			}
			k.inspect(func(k *fakeKernel) {
				if !k.isOpen {
					t.Error("lost cleanup ownership")
				}
			})
			close(a.release)
			await(t, k.when(func(k *fakeKernel) bool { return k.closes == 1 }), "eventual close")
		})
	}
}

func TestNativeFinishCancellationKeepsCutoff(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Tail = time.Millisecond
	g := &nativePollGate{sys: k, entered: make(chan struct{}), gate: make(chan struct{})}
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(g, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	defer s.Release(context.Background(), Immediate)
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	await(t, k.when(func(k *fakeKernel) bool { return starved(k) }), "idle")
	k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEAGAIN}} })
	g.armed.Store(true)
	s.beginRelease(TrackChange)
	await(t, g.entered, "tail")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Finish(ctx)
	if s.immediate() {
		t.Error("Finish cancellation overrode TrackChange")
	}
	k.free()
	close(g.gate)
	if e = s.Release(context.Background(), TrackChange); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(given(k), bytes.Repeat([]byte{0x69}, 89*8)) {
		t.Fatalf("qualified tail lost: %d bytes", len(given(k)))
	}
}

// TestNativeIdlePauseFence models the window between the loop's state read and
// installing an idle wait: a newly requested Pause must return to the writer.
func TestNativeIdlePauseFence(t *testing.T) {
	k := nativeKernel(t)
	d, e := openNative(k, k.now, testEndpoint, Config{RateHz: 88200, Format: format.DSDU32BE})
	if e != nil {
		t.Fatal(e)
	}
	defer d.releaseNative()
	s := &DSDSink{dev: d, paused: true, changed: make(chan struct{}), kick: make(chan struct{}, 1)}
	answer := make(chan error, 1)
	go func() { answer <- s.watchNative(time.Millisecond) }()
	select {
	case e = <-answer:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("idle wait ignored unapplied Pause")
		s.mu.Lock()
		s.waitCancel()
		s.mu.Unlock()
		<-answer
	}
}
