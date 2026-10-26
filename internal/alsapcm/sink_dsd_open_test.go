package alsapcm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestNativeCancelledOpen retains cleanup ownership after a cancelled opener.
func TestNativeCancelledOpen(t *testing.T) {
	k := nativeKernel(t)
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answer := make(chan error, 1)
	go func() {
		_, e := openNativeBounded(ctx, nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) {
			close(entered)
			<-gate
			return openNative(k, k.now, ep, c)
		}, now: k.now})
		answer <- e
	}()
	select {
	case <-entered:
	case e := <-answer:
		t.Fatalf("opener not entered: %v", e)
	case <-time.After(time.Second):
		t.Fatal("open not started")
	}
	cancel()
	select {
	case e := <-answer:
		if !errors.Is(e, ErrReleaseUnconfirmed) {
			t.Fatalf("unconfirmed open: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled open held caller")
	}
	release()
	await(t, k.when(func(k *fakeKernel) bool { return k.closes == 1 }), "late open cleaned up")
}

// TestNativeInertPauseDeadline releases a device paused before activation too.
func TestNativeInertPauseDeadline(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(31 * time.Second)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		s.mu.Lock()
		released := s.released
		ch := s.changed
		s.mu.Unlock()
		if released {
			break
		}
		select {
		case <-ch:
		case <-timer.C:
			t.Fatal("inert pause never released")
		}
	}
	if len(given(k)) != 0 {
		t.Fatal("inert pause wrote")
	}
}

// TestNativeCountOverflow refuses an unrepresentable count without indexing data.
func TestNativeCountOverflow(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	defer func() {
		if r := recover(); r != nil {
			s.mu.Unlock()
			t.Errorf("count overflow panicked: %v", r)
		}
	}()
	n, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 1, int(^uint(0)>>1))
	if e == nil || n != 0 {
		t.Fatal("overflowing bit count accepted")
	}
}

// TestNativeReopenGeometry refuses a changed buffer/period before any prefill.
func TestNativeReopenGeometry(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(31 * time.Second)
	for {
		s.mu.Lock()
		released := s.released
		ch := s.changed
		s.mu.Unlock()
		if released {
			break
		}
		<-ch
	}
	k.revoke()
	k.inspect(func(k *fakeKernel) {
		k.commit = func(p *hwParams) { p.setInteger(hwParamBufferSize, 7056); p.setInteger(hwParamPeriodSize, 1764) }
	})
	if e := s.Resume(context.Background()); e == nil {
		t.Fatal("changed reopen geometry admitted")
	}
	if len(given(k)) != 0 {
		t.Fatal("changed reopen wrote")
	}
}
