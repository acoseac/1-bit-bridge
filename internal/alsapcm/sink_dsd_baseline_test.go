package alsapcm

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// An ended life cannot hide a failed close from any command entry point.
func TestNativeEndedLifePreservesCleanupAtEveryEntry(t *testing.T) {
	for _, command := range []struct {
		name string
		call func(*DSDSink) error
	}{
		{"activate", func(s *DSDSink) error { return s.Activate(context.Background(), 1) }},
		{"paused activation", func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
		{"resume", func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"pause", func(s *DSDSink) error { return s.Pause(context.Background()) }},
	} {
		t.Run(command.name, func(t *testing.T) {
			k := nativeKernel(t)
			life, end := context.WithCancel(context.Background())
			defer end()
			a := &nativeAuditSys{sys: k, t: t, closeErr: errors.New("close failed")}
			s, e := openDSDSink(life, nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			end()
			await(t, s.Done(), "the failed release")
			e = command.call(s)
			if !errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled {
				t.Errorf("command answered %v; want the failed cleanup without cancellation", e)
			}
			k.checkClosedEach(t)
		})
	}
}

// The queued command's answer must include cleanup even when the caller's
// own cancellation is ready before the writer can close its device.
func TestNativeQueuedResumePreservesCleanup(t *testing.T) {
	for _, onLife := range []bool{false, true} {
		t.Run(fmt.Sprint("caller on life ", onLife), func(t *testing.T) {
			k := nativeKernel(t)
			life, end := context.WithCancel(context.Background())
			defer end()
			a := &nativeAuditSys{sys: k, t: t, closeErr: errors.New("close failed")}
			s, e := openDSDSink(life, nativeConfig(), nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			held, status := k.parkNextStatus()
			defer status()
			paused := make(chan error, 1)
			go func() { paused <- s.Pause(context.Background()) }()
			await(t, held, "pause status")
			polling, poll := k.parkNextPoll()
			defer poll()
			status()
			if e = receive(t, paused, "pause"); e != nil {
				t.Fatal(e)
			}
			await(t, polling, "pause watch")
			ctx := context.Background()
			if onLife {
				ctx = life
			}
			answer := make(chan error, 1)
			go func() { answer <- s.Resume(ctx) }()
			nativeWait(t, s, "queued resume", func() bool { return s.activation != nil })
			end()
			s.Revoke()
			poll()
			await(t, s.Done(), "failed cleanup")
			e = receive(t, answer, "resume answer")
			if !errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled {
				t.Errorf("queued resume answered %v; want failed cleanup without cancellation", e)
			}
			k.checkClosedEach(t)
		})
	}
}
