package alsapcm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// A fault must establish the release fence before entering parked cleanup.
func TestNativeWriterFaultTerminal(t *testing.T) {
	k := nativeKernel(t)
	a := &nativeAuditSys{sys: k, t: t, where: ioctlDrop, parked: make(chan struct{}), release: make(chan struct{})}
	s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { close(a.release); <-s.Done() }()
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	a.armed.Store(true)
	k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEPIPE}} })
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, 0, 32)
	answer := make(chan error, 1)
	go func() { answer <- s.Finish(context.Background()) }()
	await(t, a.parked, "fault cleanup")
	k.advanceNow(2 * time.Second)
	s.mu.Lock()
	terminal := s.releasing && s.revoked && s.mode == Immediate && !s.releaseDeadline.IsZero()
	s.mu.Unlock()
	if !terminal {
		t.Error("writer fault entered cleanup without terminal release fence")
	}
	select {
	case e = <-answer:
		if !errors.Is(e, ErrReleaseUnconfirmed) {
			t.Error(e)
		}
	case <-time.After(2500 * time.Millisecond):
		t.Error("Finish ignored fault cleanup deadline")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	e = s.Activate(ctx, 2)
	if errors.Is(e, context.DeadlineExceeded) || e == nil {
		t.Error("terminal activation did not refuse immediately", e)
	}
}

type nativeFinalStatusFault struct {
	sys
	drained  bool
	statuses int
}

func (a *nativeFinalStatusFault) ioctl(fd int, req uint32, p unsafe.Pointer) error {
	if req == ioctlDrain {
		a.drained = true
		return nil
	}
	if req == ioctlStatus && a.drained {
		a.statuses++
		if a.statuses == 1 {
			*(*pcmStatus)(p) = pcmStatus{state: stateSetup}
			return nil
		}
		return errnoENODEV
	}
	return a.sys.ioctl(fd, req, p)
}
func TestNativeFinalMeasurementFailure(t *testing.T) {
	k := nativeKernel(t)
	a := &nativeFinalStatusFault{sys: k}
	s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e = s.Finish(context.Background()); e == nil {
		t.Fatal("final STATUS failure reported complete")
	}
}

// A fake-clock deadline must fire even when cleanup makes no more kernel calls.
func TestNativeReleaseInjectedClock(t *testing.T) {
	k := nativeKernel(t)
	a := &nativeAuditSys{sys: k, t: t, where: ioctlDrop, parked: make(chan struct{}), release: make(chan struct{})}
	ticks := make(chan time.Time, 1)
	armed := make(chan time.Time, 1)
	s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: func(at time.Time) (<-chan time.Time, func()) { armed <- at; return ticks, func() {} }})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { close(a.release); <-s.Done() }()
	start := k.now()
	a.armed.Store(true)
	answer := make(chan error, 1)
	go func() { answer <- s.Release(context.Background(), TrackChange) }()
	await(t, a.parked, "cleanup")
	select {
	case at := <-armed:
		if !at.Equal(start.Add(2 * time.Second)) {
			t.Errorf("deadline=%v want %v", at, start.Add(2*time.Second))
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("injected timer was not armed")
	}
	s.BeginRelease(Immediate)
	k.advanceNow(2 * time.Second)
	ticks <- k.now()
	select {
	case e = <-answer:
		if !errors.Is(e, ErrReleaseUnconfirmed) {
			t.Error(e)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("advancing injected clock did not expire parked cleanup")
	}
}

// nativeAt follows fake time even while a writer is parked in cleanup. The
// stop function joins its helper, so test timer cancellation leaks no worker.
func (k *fakeKernel) nativeAt(at time.Time) (<-chan time.Time, func()) {
	reached := k.when(func(k *fakeKernel) bool { return !k.clock.Before(at) })
	ticks := make(chan time.Time, 1)
	stop, done := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(done)
		select {
		case <-reached:
			ticks <- at
		case <-stop:
		}
	}()
	return ticks, func() { once.Do(func() { close(stop) }); <-done }
}

func TestNativeCancelledPrefillRecovery(t *testing.T) {
	for _, name := range []string{"cancelled_empty", "cancelled_partial", "terminal_fault"} {
		t.Run(name, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Prefill = time.Millisecond
			s := nativeSink(t, k, c)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			k.grant(31 * time.Second)
			nativeWait(t, s, "released pause", func() bool { return s.released })
			k.revoke()
			before := len(given(k))
			k.inspect(func(k *fakeKernel) {
				if name != "terminal_fault" {
					k.polls = []fakePoll{{block: true}}
				}
				switch name {
				case "cancelled_empty":
					k.writes = []fakeWrite{{err: errnoEAGAIN}}
				case "cancelled_partial":
					k.writes = []fakeWrite{{take: 3}, {err: errnoEAGAIN}}
				case "terminal_fault":
					k.writes = []fakeWrite{{err: errnoEPIPE}}
				}
			})
			resumed := make(chan error, 1)
			go func() { resumed <- s.Resume(context.Background()) }()
			if name == "terminal_fault" {
				if e := <-resumed; e == nil {
					t.Fatal("prefill fault succeeded")
				}
				await(t, s.Done(), "fault cleanup")
				if e := s.Resume(context.Background()); e == nil {
					t.Fatal("terminal write resumed")
				}
				return
			}
			await(t, k.entered, "prefill wait")
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			if e := <-resumed; ReasonOf(e) != ReasonCancelled && !errors.Is(e, context.Canceled) {
				t.Fatalf("prefill cancellation: %v", e)
			}
			if !s.Position().Paused {
				t.Fatal("cancelled prefill lost paused state")
			}
			select {
			case <-s.Done():
				t.Fatal("Pause terminated sink")
			default:
			}
			accepted := len(given(k)) - before
			want := 0
			if name == "cancelled_partial" {
				want = 3 * s.Params().NativeFrameBytes()
			}
			if accepted != want {
				t.Fatalf("accepted prefill=%d want=%d", accepted, want)
			}
			if e := s.Resume(context.Background()); e != nil {
				t.Fatal("following Resume refused", e)
			}
			if s.Position().Paused {
				t.Fatal("following Resume remained paused")
			}
			if len(given(k))-before != accepted {
				t.Fatal("following Resume replayed interrupted prefill")
			}
			k.inspect(func(k *fakeKernel) {
				if k.opens != 2 || k.closes != 1 {
					t.Errorf("recovery reopened/closed transport: opens=%d closes=%d", k.opens, k.closes)
				}
			})
		})
	}
}

type nativeBudgetSys struct {
	sys
	k        *fakeKernel
	tailTime time.Duration
	draining bool
}

func (a *nativeBudgetSys) ioctl(fd int, req uint32, p unsafe.Pointer) error {
	if req == ioctlWriteIFrames {
		e := a.sys.ioctl(fd, req, p)
		a.k.advanceNow(a.tailTime)
		return e
	}
	if req == ioctlDrain {
		a.draining = true
		return errnoEAGAIN
	}
	if req == ioctlStatus && a.draining {
		*(*pcmStatus)(p) = pcmStatus{state: stateDraining}
		return nil
	}
	return a.sys.ioctl(fd, req, p)
}
func (a *nativeBudgetSys) poll(ctx context.Context, fd int, events int16, d time.Duration) (int16, error) {
	if a.draining {
		a.k.advanceNow(d)
		return 0, nil
	}
	return a.sys.poll(ctx, fd, events, d)
}
func TestNativeDrainRemainingBudget(t *testing.T) {
	for _, spent := range []time.Duration{time.Second, 1600 * time.Millisecond} {
		t.Run(spent.String(), func(t *testing.T) {
			k := nativeKernel(t)
			a := &nativeBudgetSys{sys: k, k: k, tailTime: spent}
			c := nativeConfig()
			c.Tail = time.Millisecond
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			start := k.now()
			e = s.Release(context.Background(), TrackChange)
			if !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatalf("failed tail/drain not unconfirmed: %v", e)
			}
			<-s.Done()
			if spent > 1500*time.Millisecond {
				if a.draining {
					t.Error("Drain began without close margin")
				}
			} else if elapsed := k.now().Sub(start); elapsed > 1500*time.Millisecond {
				t.Errorf("drain consumed close margin: %v", elapsed)
			}
		})
	}
}
func TestNativeTailFailureUnconfirmed(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Tail = time.Millisecond
	s := nativeSink(t, k, c)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEPIPE}} })
	if e := s.Release(context.Background(), TrackChange); !errors.Is(e, ErrReleaseUnconfirmed) {
		t.Fatal("tail error not unconfirmed", e)
	}
}

// Dirty carry is an injected impossible state: production resets the cursor
// only together with the whole carry. This pins the defensive replacement.
func TestNativeDirtyCarryDefensive(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	s.mu.Lock()
	s.carry = [2][4]byte{{255, 255, 255, 255}, {255, 255, 255, 255}}
	s.mu.Unlock()
	if n, e := s.WriteDSD(context.Background(), 1, []byte{0}, []byte{0}, 1, 1); n != 1 || e != nil {
		t.Fatal(n, e)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carry[0][0]&0x80 != 0 || s.carry[1][0]&0x80 != 0 {
		t.Fatal("unaligned zero retained a dirty destination bit")
	}
}

func TestNativeClosedFaultActivation(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEPIPE}} })
	_, _ = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}, 0, 32)
	await(t, s.Done(), "fault cleanup completed")
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 2) }()
	select {
	case e := <-answer:
		if e == nil {
			t.Fatal("closed terminal sink activated")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("activation blocked on an exited writer")
	}
}
func TestNativeReleaseErrorsUnconfirmed(t *testing.T) {
	for _, where := range []string{"tail", "drain"} {
		for _, kind := range []string{"deadline", "xrun"} {
			t.Run(where+"_"+kind, func(t *testing.T) {
				k := nativeKernel(t)
				c := nativeConfig()
				if where == "tail" {
					c.Tail = time.Millisecond
				}
				s := nativeSink(t, k, c)
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				var fault error = errnoEPIPE
				if kind == "deadline" {
					fault = context.DeadlineExceeded
				}
				k.inspect(func(k *fakeKernel) {
					req := uint32(ioctlDrain)
					if where == "tail" {
						req = ioctlWriteIFrames
					}
					k.fail[req] = fault
				})
				if e := s.Release(context.Background(), TrackChange); !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, fault) {
					t.Fatalf("release lost unconfirmed/cause: %v", e)
				}
			})
		}
	}
}
