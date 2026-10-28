package alsapcm

import (
	"context"
	"errors"
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

func TestNativeResumeCancelsLocate(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	entered, exited := make(chan struct{}), make(chan struct{})
	abandon := make(chan struct{})
	c.Locate = func(ctx context.Context) (Endpoint, error) {
		close(entered)
		select {
		case <-ctx.Done():
		case <-abandon:
		}
		close(exited)
		return Endpoint{}, ctx.Err()
	}
	s := nativeSink(t, k, c)
	if err := s.Activate(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	k.grant(31 * time.Second)
	nativeWait(t, s, "released", func() bool { return s.released })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Resume(ctx) }()
	<-entered
	cancel()
	<-done
	select {
	case <-exited:
	case <-time.After(time.Second):
		close(abandon)
		<-exited
		t.Error("resume context did not cancel locator")
	}
	<-s.Done()
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
