package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

type nativeAuditSys struct {
	sys
	mu              *sync.Mutex
	t               *testing.T
	where           uint32
	parked, release chan struct{}
	armed           atomic.Bool
	closeErr        error
}

func (a *nativeAuditSys) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	a.check()
	if req == a.where && a.armed.CompareAndSwap(true, false) {
		close(a.parked)
		<-a.release
	}
	return a.sys.ioctl(fd, req, arg)
}
func (a *nativeAuditSys) ioctlValue(fd int, req uint32, arg uintptr) error {
	a.check()
	return a.sys.ioctlValue(fd, req, arg)
}
func (a *nativeAuditSys) close(fd int) error {
	a.check()
	if a.where == 0 && a.armed.CompareAndSwap(true, false) {
		close(a.parked)
		<-a.release
	}
	return errors.Join(a.sys.close(fd), a.closeErr)
}
func (a *nativeAuditSys) check() {
	if a.mu != nil {
		if !a.mu.TryLock() {
			a.t.Error("ioctl under native mutex")
		} else {
			a.mu.Unlock()
		}
	}
}

// TestNativeTeardownOwnership parks below the cancellable write/drain layer.
func TestNativeTeardownOwnership(t *testing.T) {
	for _, tc := range []struct {
		name  string
		where uint32
		fail  bool
	}{{"drop", ioctlDrop, false}, {"hw_free", ioctlHWFree, false}, {"close", 0, false}, {"close_error", 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			k := nativeKernel(t)
			a := &nativeAuditSys{sys: k, t: t, where: tc.where, parked: make(chan struct{}), release: make(chan struct{})}
			if tc.fail {
				a.closeErr = errors.New("close failed")
			}
			s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			a.mu = &s.mu
			a.armed.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			answer := make(chan error, 1)
			go func() { answer <- s.Release(ctx, Immediate) }()
			await(t, a.parked, "cleanup parked")
			cancel()
			e = <-answer
			if !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatalf("unconfirmed: %v", e)
			}
			select {
			case <-s.Done():
				t.Fatal("cleanup falsely acknowledged")
			default:
			}
			if e = s.Release(ctx, Immediate); !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatal("escalation waited or acknowledged")
			}
			close(a.release)
			await(t, s.Done(), "actual cleanup")
			e = s.Release(context.Background(), Immediate)
			if tc.fail && !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatalf("lost close error: %v", e)
			}
			if !tc.fail && e != nil {
				t.Fatal(e)
			}
			k.checkClosedEach(t)
		})
	}
}

// TestNativePauseDuringActivation parks admission and lets Pause win its fence.
func TestNativePauseDuringActivation(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	parked, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int32
	c.Check = func(NodeInfo) error {
		if count.Add(1) == 2 {
			close(parked)
			<-release
		}
		return nil
	}
	s := nativeSink(t, k, c)
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 1) }()
	await(t, parked, "activation check")
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	close(release)
	if e := <-answer; e == nil {
		t.Fatal("activation ignored pause")
	}
	if len(given(k)) != 0 {
		t.Fatal("pause fence wrote")
	}
	select {
	case <-s.Done():
		t.Fatal("Pause closed an inert sink")
	default:
	}
}

// TestNativeReleaseDeadline keeps one deadline even while close remains parked.
func TestNativeReleaseDeadline(t *testing.T) {
	k := nativeKernel(t)
	a := &nativeAuditSys{sys: k, t: t, parked: make(chan struct{}), release: make(chan struct{})}
	s, e := openDSDSink(context.Background(), nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	a.armed.Store(true)
	defer func() { close(a.release); <-s.Done() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Release(ctx, TrackChange)
	<-a.parked
	s.mu.Lock()
	deadline := s.releaseDeadline
	s.mu.Unlock()
	k.advanceNow(2 * time.Second)
	// The fake clock expires the release while close stays parked. The test
	// binary's timeout detects a deadlock; scheduler speed is not an assertion.
	e = s.Release(context.Background(), Immediate)
	if !errors.Is(e, ErrReleaseUnconfirmed) {
		t.Fatalf("release did not expire: %v", e)
	}
	s.mu.Lock()
	same := s.releaseDeadline.Equal(deadline)
	s.mu.Unlock()
	if !same {
		t.Fatal("escalation reset shared deadline")
	}
}

// TestNativeMutexBoundary audits writes/status as well as teardown ioctls.
func TestNativeMutexBoundary(t *testing.T) {
	k := nativeKernel(t)
	a := &nativeAuditSys{sys: k, t: t}
	c := nativeConfig()
	c.Prefill = time.Millisecond
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	a.mu = &s.mu
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Release(context.Background(), Immediate); e != nil {
		t.Fatal(e)
	}
}

// TestNativeStopDuringActivation covers both permanent caller fences.
func TestNativeStopDuringActivation(t *testing.T) {
	for _, mode := range []string{"stop", "revoke"} {
		t.Run(mode, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Prefill = time.Millisecond
			parked, release := make(chan struct{}), make(chan struct{})
			var count atomic.Int32
			c.Check = func(NodeInfo) error {
				if count.Add(1) == 2 {
					close(parked)
					<-release
				}
				return nil
			}
			s := nativeSink(t, k, c)
			answer := make(chan error, 1)
			go func() { answer <- s.Activate(context.Background(), 1) }()
			await(t, parked, "activation")
			if mode == "stop" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_ = s.Release(ctx, Immediate)
			} else {
				s.Revoke()
			}
			close(release)
			if e := <-answer; e == nil {
				t.Fatal("revocation admitted activation")
			}
			if e := s.Release(context.Background(), Immediate); e != nil {
				t.Fatal(e)
			}
			if len(given(k)) != 0 {
				t.Fatal("permanent fence wrote")
			}
		})
	}
}

// TestNativeLateReleaseAcknowledgement gives actual completion priority over an expired caller timer.
func TestNativeLateReleaseAcknowledgement(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if e := s.Release(context.Background(), Immediate); e != nil {
		t.Fatal(e)
	}
	k.advanceNow(3 * time.Second)
	for range 100 {
		if e := s.Release(context.Background(), Immediate); e != nil {
			t.Fatalf("completed close became unconfirmed: %v", e)
		}
	}
}
