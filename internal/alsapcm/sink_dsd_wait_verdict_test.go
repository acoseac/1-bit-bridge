package alsapcm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// verdictWaitContext installs the cleanup record exactly when the command
// evaluates its wait. These are select-arm unit cases, without a writer or
// hardware: the entry matrix separately creates these records on real sinks.
// The context and done cases each make only that arm ready; the changed case
// also covers the successful loop exit beside done.
type verdictWaitContext struct {
	context.Context
	atWait func()
	done   <-chan struct{}
}

func (c verdictWaitContext) Done() <-chan struct{} {
	c.atWait()
	return c.done
}

func TestNativeWaitingCommandsAnswerTheEndedLifesFailure(t *testing.T) {
	for _, entry := range []string{"WriteDSD ring context", "Pause context", "Pause done", "Pause changed"} {
		t.Run(entry, func(t *testing.T) {
			life, end := context.WithCancel(context.Background())
			defer end()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// An inert full ring for WriteDSD; an active sink waiting for
			// pauseApplied for Pause. No goroutine owns this staged sink.
			s := &DSDSink{life: life, cfg: DSDConfig{LedgerLimit: 1}, active: true,
				changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}),
				releaseExpired: make(chan struct{}), seams: nativeSeams{now: time.Now}}
			calls := 0
			wait := verdictWaitContext{Context: ctx, done: ctx.Done()}
			if entry == "Pause done" || entry == "Pause changed" {
				wait.done = nil
			}
			wait.atWait = func() {
				calls++
				if calls != 1 {
					t.Fatal("command revisited its staged wait")
				}
				end()
				s.mu.Lock()
				s.releasing, s.revoked, s.mode = true, true, Immediate
				s.err = errors.Join(ErrReleaseUnconfirmed, &Error{Reason: ReasonCancelled, Op: "drain", Err: context.Canceled})
				s.mu.Unlock()
				switch entry {
				case "Pause done":
					close(s.done)
				case "Pause changed":
					s.mu.Lock()
					s.active = false
					s.notify()
					s.mu.Unlock()
					close(s.done)
					// Done is also ready here; both returns must settle.
				default:
					close(s.releaseExpired)
					cancel()
				}
			}
			var err error
			if entry == "WriteDSD ring context" {
				n, e := s.WriteDSD(wait, 1, []byte{1}, []byte{2}, 0, 8)
				err = e
				if n != 0 {
					t.Errorf("full ring accepted %d bits", n)
				}
			} else {
				err = s.Pause(wait)
			}
			if calls != 1 {
				t.Fatal("command never entered its wait")
			}
			theSinksFailure(t, err, ErrReleaseUnconfirmed, entry)
		})
	}
}

func TestNativePausedInitialActivationKeepsItsOwnFence(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	if err := s.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Revoke()
	await(t, s.Done(), "revoked inert sink cleanup")
	if err := s.ActivatePaused(context.Background()); !errors.Is(err, ErrActivationFenced) {
		t.Errorf("initial paused activation answered %v; want its take-back fence", err)
	}
	k.checkClosedEach(t)
}

func TestOrderedWaitHonorsALargerWatchdog(t *testing.T) {
	previous := *watchdog
	defer func() { *watchdog = previous }()
	*watchdog = 17 * time.Second
	if got := ordered(); got != *watchdog {
		t.Errorf("ordering wait is %v with watchdog %v", got, *watchdog)
	}
}
