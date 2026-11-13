package alsapcm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type escalationFaultSys struct {
	sys
	escalate func()
	where    uint32
}

func (a *escalationFaultSys) ioctl(fd int, req uint32, p unsafe.Pointer) error {
	if req == a.where && a.escalate != nil {
		// A genuine device error is established before Stop wins the release fence.
		fault := errnoEPIPE
		a.escalate()
		return fault
	}
	return a.sys.ioctl(fd, req, p)
}
func TestNativeEscalationPreservesDeviceFault(t *testing.T) {
	for _, where := range []string{"tail", "drain"} {
		t.Run(where, func(t *testing.T) {
			k := nativeKernel(t)
			a := &escalationFaultSys{sys: k, where: ioctlDrain}
			c := nativeConfig()
			if where == "tail" {
				c.Tail = time.Millisecond
				a.where = ioctlWriteIFrames
			}
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(a, k.now, ep, c) }, now: k.now, at: k.nativeAt})
			if e != nil {
				t.Fatal(e)
			}
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			a.escalate = func() { s.BeginRelease(Immediate) }
			e = s.Release(context.Background(), TrackChange)
			<-s.Done()
			if !errors.Is(e, errnoEPIPE) || !errors.Is(e, ErrReleaseUnconfirmed) {
				t.Fatalf("escalation hid device fault: %v", e)
			}
		})
	}
}

func TestNativeResumeRelocates(t *testing.T) {
	for _, mode := range []string{"moved", "old_reused", "busy", "refused"} {
		t.Run(mode, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			located := 0
			checked := 0
			c.Locate = func(context.Context) (Endpoint, error) { located++; return Endpoint{Card: 7, Device: 0}, nil }
			c.Check = func(n NodeInfo) error {
				checked++
				if checked > 2 && (mode == "refused" || (mode == "old_reused" && n.Card != 7)) {
					return errors.New("wrong device")
				}
				return nil
			}
			var s *DSDSink
			if mode == "old_reused" {
				var err error
				s, err = openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) {
					// Both nodes remain openable; INFO describes the node actually opened.
					k.inspect(func(k *fakeKernel) { k.info.card = int32(ep.Card) })
					return openNative(k, k.now, ep, c)
				}, now: k.now, at: k.nativeAt})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Release(context.Background(), Immediate) })
			} else {
				s = nativeSink(t, k, c)
			}
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			k.grant(31 * time.Second)
			nativeWait(t, s, "released", func() bool { return s.released })
			k.revoke()
			k.inspect(func(k *fakeKernel) {
				k.info.card = 7
				if mode == "busy" {
					k.openErr = errnoEBUSY
				}
			})
			e := s.Resume(context.Background())
			if located != 1 {
				t.Errorf("fresh resolutions=%d want 1", located)
			}
			if mode == "busy" || mode == "refused" {
				if e == nil || !s.Position().Paused {
					t.Fatalf("refusal lost pause: %v %+v", e, s.Position())
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if s.Params().Endpoint.Card != 7 {
				t.Fatalf("reopened old endpoint: %+v", s.Params().Endpoint)
			}
		})
	}
}

func TestNativePausedInitialActivation(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if err := s.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Resume(context.Background()); err == nil {
		t.Fatal("inert Resume authorized activation")
	}
	if err := s.ActivatePaused(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Position().Paused {
		t.Fatal("explicit initial authority stayed paused")
	}
}

// TestNativeResumeCancelsLocate: a Resume cancelled while its reopen's
// Locate runs cancels that Locate, whose context is the request's, and
// answers its cancellation. Its wait (await) then finds the writer's reply
// and its own context's end ready together. Through its context, the Resume
// revokes the sink, so the abandoned caller cannot authorize sound later
// (revokeFound), and the sink's writer ends with no other release; through
// the writer's reply, a refused reopen held for the next resume, the sink
// stays paused, its writer alive, until a release ends it, here the test's.
// Each arm is forced (armPick), and the select's own pick (armAny) asserts
// whichever outcome it took: left to the select alone, the test waited on
// Done beside a sink kept paused, until the package's timeout (backlog
// B887). Which outcome a cancelled Resume should have is the product's
// question, left open.
func TestNativeResumeCancelsLocate(t *testing.T) {
	for _, tc := range []struct {
		name string
		arm  awaitArm
	}{{"through the writer's reply", armReply}, {"through its caller's context", armContext}, {"through either, as the select picks", armAny}} {
		t.Run(tc.name, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			entered, exited := make(chan struct{}), make(chan struct{})
			abandon := make(chan struct{})
			letLocateGo := sync.OnceFunc(func() { close(abandon) })
			c.Locate = func(ctx context.Context) (Endpoint, error) {
				close(entered)
				select {
				case <-ctx.Done():
				case <-abandon:
				}
				close(exited)
				return Endpoint{}, ctx.Err()
			}
			pick := &armPick{}
			s := nativeSink(t, k, c, pick.hook)
			if err := s.Activate(context.Background(), 1); err != nil {
				t.Fatal(err)
			}
			if err := s.Pause(context.Background()); err != nil {
				t.Fatal(err)
			}
			k.grant(31 * time.Second)
			nativeWait(t, s, "released", func() bool { return s.released })
			ctx, cancel := context.WithCancel(context.Background())
			done, resumed := make(chan error, 1), make(chan struct{})
			// Registered after the sink's release, so it runs first: the
			// Resume and its Locate let go, and the Resume joined.
			t.Cleanup(func() {
				cancel()
				letLocateGo()
				await(t, resumed, "the Resume's return at the test's end")
			})
			// The arm of the Resume's wait alone: Activate's, whose context
			// never ends, would wait on nil channels under armContext.
			pick.set(tc.arm)
			go func() {
				defer close(resumed)
				done <- s.Resume(ctx)
			}()
			await(t, entered, "the reopen's Locate")
			cancel()
			err := receive(t, done, "the cancelled Resume's answer")
			select {
			case <-exited:
			case <-time.After(time.Second):
				letLocateGo()
				await(t, exited, "the reopen's Locate, let go")
				t.Error("resume context did not cancel locator")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("the cancelled Resume answered %v, want its cancellation", err)
			}
			revoked := nativeRevoked(s)
			switch {
			case tc.arm == armContext && !revoked:
				t.Fatal("through its context, the cancelled Resume did not revoke the sink")
			case tc.arm == armReply && revoked:
				t.Fatal("through the writer's reply, the cancelled Resume revoked the sink")
			}
			if !revoked {
				if p := s.Position(); !p.Paused || !p.Released {
					t.Errorf("the sink a cancelled Resume kept reads %+v, want it paused and released", p)
				}
				select {
				case <-s.Done():
					t.Fatal("the sink a cancelled Resume kept ended with no release")
				default:
				}
				// Only a release ends it.
				s.BeginRelease(Immediate)
			}
			await(t, s.Done(), "the sink's end")
		})
	}
}

// nativeRevoked says whether s has been revoked, read under the sink's lock,
// where its revocation is made.
func nativeRevoked(s *DSDSink) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
}

func TestNativeTailHasNoOccurrenceTime(t *testing.T) {
	k := nativeKernel(t)
	k.free()
	c := nativeConfig()
	c.Tail = time.Millisecond
	s := nativeSink(t, k, c)
	// Only one carrier word reaches hardware. The rest stays in software at
	// the release fence; tail drain must never turn those discarded bits into time.
	data := make([]byte, 8000)
	if _, err := s.WriteDSD(context.Background(), 1, data, data, 0, len(data)*8); err != nil {
		t.Fatal(err)
	}
	entered, allow := make(chan struct{}), make(chan struct{})
	k.inspect(func(k *fakeKernel) { k.writeParked = entered; k.writeGate = allow })
	if err := s.Activate(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	<-entered
	s.BeginRelease(TrackChange)
	close(allow)
	if err := s.Release(context.Background(), TrackChange); err != nil {
		t.Fatal(err)
	}
	p := s.Position()
	if p.Written >= p.Submitted || p.Played != p.Written {
		t.Fatalf("tail counted discarded software content: %+v", p)
	}
}
