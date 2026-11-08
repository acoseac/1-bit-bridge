package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheActivationsOwnFenceIsErrActivationFenced (B728 B9 round 4): an
// activation the sink's own fence ends answers ErrActivationFenced wherever
// the fence finds it: the sink revoked before the activation was asked,
// revoked or paused during its admission, revoked while its prefill was
// written (the cut's cancellation beside it, and nothing but silence given),
// or ended with the activation asked and its writer not yet running it. The
// device's own answers are told from it: an admission the check refused is
// that refusal, the sink revoked beside it or not, and a sink that failed
// before the activation refuses it, never fenced, as its writer's end then
// answers with the failure.
func TestTheActivationsOwnFenceIsErrActivationFenced(t *testing.T) {
	// admitting is a sink whose activation's admission, the second check after
	// open, is held until release is closed, and then answers refuse.
	admitting := func(t *testing.T, k *fakeKernel, refuse error) (s *DSDSink, answer chan error, parked, release chan struct{}) {
		t.Helper()
		c := nativeConfig()
		c.Prefill = time.Millisecond
		parked, release = make(chan struct{}), make(chan struct{})
		var count atomic.Int32
		c.Check = func(NodeInfo) error {
			if count.Add(1) == 2 {
				close(parked)
				<-release
				return refuse
			}
			return nil
		}
		s = nativeSink(t, k, c)
		answer = make(chan error, 1)
		go func() { answer <- s.Activate(context.Background(), 1) }()
		await(t, parked, "the activation's admission")
		return s, answer, parked, release
	}
	t.Run("revoked before it was asked", func(t *testing.T) {
		k := nativeKernel(t)
		s := nativeSink(t, k, nativeConfig())
		s.Revoke()
		if e := s.Activate(context.Background(), 1); !errors.Is(e, ErrActivationFenced) {
			t.Fatalf("the activation of a revoked sink: %v, want ErrActivationFenced", e)
		}
		if len(given(k)) != 0 {
			t.Fatal("a fenced activation wrote")
		}
	})
	t.Run("revoked during its admission", func(t *testing.T) {
		k := nativeKernel(t)
		s, answer, _, release := admitting(t, k, nil)
		s.Revoke()
		close(release)
		if e := receive(t, answer, "the activation's answer"); !errors.Is(e, ErrActivationFenced) {
			t.Fatalf("an activation revoked during its admission: %v, want ErrActivationFenced", e)
		}
		if len(given(k)) != 0 {
			t.Fatal("a fenced activation wrote")
		}
	})
	t.Run("paused during its admission", func(t *testing.T) {
		k := nativeKernel(t)
		s, answer, _, release := admitting(t, k, nil)
		if e := s.Pause(context.Background()); e != nil {
			t.Fatal(e)
		}
		close(release)
		if e := receive(t, answer, "the activation's answer"); !errors.Is(e, ErrActivationFenced) {
			t.Fatalf("an activation paused during its admission: %v, want ErrActivationFenced", e)
		}
		if len(given(k)) != 0 {
			t.Fatal("a fenced activation wrote")
		}
	})
	t.Run("revoked while its prefill is written", func(t *testing.T) {
		k := nativeKernel(t)
		c := nativeConfig()
		// A prefill past the 40 ms buffer: the stream starts once the buffer is
		// full, and the writer waits on the DAC for room.
		c.Prefill = 200 * time.Millisecond
		s := nativeSink(t, k, c)
		answer := make(chan error, 1)
		go func() { answer <- s.Activate(context.Background(), 1) }()
		await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "the prefill filling the buffer, the writer waiting on the DAC")
		s.Revoke()
		e := receive(t, answer, "the activation's answer")
		if !errors.Is(e, ErrActivationFenced) || ReasonOf(e) != ReasonCancelled {
			t.Fatalf("an activation revoked mid-prefill: %v (reason %q), want ErrActivationFenced beside the write's cancellation", e, ReasonOf(e))
		}
		if b := given(k); len(b) == 0 || !bytes.Equal(b, bytes.Repeat([]byte{0x69}, len(b))) {
			t.Fatalf("the node was given %d bytes, want the prefill's DSD silence alone", len(b))
		}
	})
	t.Run("ended with the activation asked and not yet run", func(t *testing.T) {
		k := nativeKernel(t)
		s := nativeSink(t, k, nativeConfig())
		// The activation asked, as Activate asks it, and the sink's release
		// begun before its writer took it: the writer ends without running it.
		r := &nativeRequest{ctx: context.Background(), gen: 1, reply: make(chan error, 1)}
		s.mu.Lock()
		s.activation = r
		s.beginReleaseLocked(Immediate)
		s.mu.Unlock()
		if e := receive(t, r.reply, "the activation's answer as the writer ends"); !errors.Is(e, ErrActivationFenced) {
			t.Fatalf("an activation its writer ended before running: %v, want ErrActivationFenced", e)
		}
	})
	t.Run("ended by a failure with the activation asked and not yet run", func(t *testing.T) {
		k := nativeKernel(t)
		s := nativeSink(t, k, nativeConfig())
		failed := errors.New("the writer failed")
		r := &nativeRequest{ctx: context.Background(), gen: 1, reply: make(chan error, 1)}
		s.mu.Lock()
		s.activation = r
		s.err = failed
		s.beginReleaseLocked(Immediate)
		s.mu.Unlock()
		if e := receive(t, r.reply, "the activation's answer as the writer ends"); errors.Is(e, ErrActivationFenced) || !errors.Is(e, failed) {
			t.Fatalf("an activation a failing writer ended before running: %v, want the failure, not the fence", e)
		}
	})
	t.Run("the admission's refusal beside a revocation", func(t *testing.T) {
		k := nativeKernel(t)
		refused := errors.New("the node is another unit's")
		s, answer, _, release := admitting(t, k, refused)
		s.Revoke()
		close(release)
		if e := receive(t, answer, "the activation's answer"); errors.Is(e, ErrActivationFenced) || !errors.Is(e, refused) {
			t.Fatalf("an admission refused beside a revocation: %v, want the refusal, not the fence", e)
		}
	})
	t.Run("a sink that failed before it", func(t *testing.T) {
		k := nativeKernel(t)
		s := nativeSink(t, k, nativeConfig())
		s.mu.Lock()
		s.err = errors.New("the writer failed")
		s.mu.Unlock()
		if e := s.Activate(context.Background(), 1); e == nil || errors.Is(e, ErrActivationFenced) {
			t.Fatalf("the activation of a sink that failed: %v, want a refusal, not the fence", e)
		}
	})
}
