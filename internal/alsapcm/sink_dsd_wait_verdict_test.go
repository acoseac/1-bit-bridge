package alsapcm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// verdictWaitContext installs the cleanup record exactly when the command
// evaluates its wait. These are select-arm unit cases, without a writer or
// hardware: the entry matrix separately creates these records on real sinks.
// Each case makes one arm of the command's wait alone ready: its context's
// end, the writer's end (done), or a change (changed), the release's budget
// passed so that the answer can settle with done still open (B728 B9 PR 1
// round 3: "Pause changed" closed done beside changed, so its select took
// either, and Pause's completion return reverted to nil still passed 17 runs
// in 20 on 7c2601a9). A change wakes Pause to its completion's return, and
// WriteDSD to its loop's next turn, whose refusal (admission closed) answers.
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
	for _, entry := range []string{"WriteDSD ring context", "WriteDSD ring woken, then refused", "Pause context", "Pause done", "Pause changed"} {
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
			if entry != "WriteDSD ring context" && entry != "Pause context" {
				wait.done = nil // its context's end is never ready
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
				case "Pause changed", "WriteDSD ring woken, then refused":
					// A change alone is ready: done stays open, so the
					// settling waits on the release's budget, passed here.
					s.mu.Lock()
					s.active = false
					s.notify()
					s.mu.Unlock()
					close(s.releaseExpired)
				default:
					close(s.releaseExpired)
					cancel()
				}
			}
			var err error
			if strings.HasPrefix(entry, "WriteDSD") {
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

// TestNativeAWriteOverItsOccurrenceBudgetAnswersTheCleanup: WriteDSD's
// occurrence budget is checked after its admission (a release, a revocation,
// a Finish or a record refuse first), so the ledger is filled while
// admission is open, the life then ends with its watcher held before the
// revocation, and a new occurrence is written on a context still running: the
// budget's refusal settles, beginning the release itself, and answers that
// release's close's failure, never its own words alone (B728 B9 PR 1 round 3).
func TestNativeAWriteOverItsOccurrenceBudgetAnswersTheCleanup(t *testing.T) {
	k := nativeKernel(t)
	f := newCleanupFault(t, k)
	f.failing.Store(true)
	c := nativeConfig()
	c.LedgerLimit = 1
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSinkOn(t, k, f, c, life)
	if n, err := s.WriteDSD(context.Background(), 1, []byte{1}, []byte{2}, 0, 8); n != 8 || err != nil {
		t.Fatalf("the first occurrence's write took %d bits, %v; want all 8, the ledger full", n, err)
	}
	end()
	await(t, revoking, "the life's end, its revocation held")
	n, err := s.WriteDSD(context.Background(), 2, []byte{3}, []byte{4}, 0, 8)
	if n != 0 {
		t.Errorf("the occurrence over the budget took %d bits", n)
	}
	theSinksFailure(t, err, ErrReleaseUnconfirmed, "the occurrence over the budget, its release's close failing")
	if !errors.Is(err, f.err) {
		t.Errorf("the write answered %v; want the close's failure", err)
	}
	revoke()
	await(t, s.Done(), "the sink's release")
	k.checkClosedEach(t)
}

// TestNativeAWaitOnALiveLifeAnswersItsReleasesFailure: an activation's caller
// waits on its request, the sink's life running, while the session's own
// release (an Immediate BeginRelease) cuts the activation's prefill short, the
// writer answering the sink's fence, and the release's close fails. Answered
// at the release's deadline, that close still held, the activation is the
// release's unconfirmed failure; answered through its own context's end once
// the release has ended, it is the close's failure: never the fence, nor that
// cancellation beside the record (B728 B9 PR 1 round 3: the deadline's arm
// answered the writer's fence, the context's arm its cancellation, while the
// life ran).
func TestNativeAWaitOnALiveLifeAnswersItsReleasesFailure(t *testing.T) {
	for _, arm := range []awaitArm{armDeadline, armContext} {
		t.Run(armName[arm], func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			f.failing.Store(true)
			f.hold.Store(arm == armDeadline)
			c := nativeConfig()
			c.Prefill = 100 * time.Millisecond // past the 40 ms buffer, so it waits for room
			pick := &armPick{}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, _, revoke := lifeSinkOn(t, k, f, c, life, pick.hook)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pick.set(arm)
			answer := make(chan error, 1)
			go func() { answer <- s.Activate(ctx, 1) }()
			await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "the prefill waiting for room")
			s.BeginRelease(Immediate)
			if arm == armDeadline {
				await(t, f.held, "the release's close, held")
				pastBudget(t, k, s)
			} else {
				await(t, s.Done(), "the release, its close failing")
				cancel()
			}
			e := receive(t, answer, "the activation's answer")
			theSinksFailure(t, e, ErrReleaseUnconfirmed, "the activation the release cut short, its close failing")
			if arm == armContext && !errors.Is(e, f.err) {
				t.Errorf("the activation answered %v; want the close's failure", e)
			}
			if life.Err() != nil {
				t.Fatal("the sink's life ended; the premise is a life still running")
			}
			if arm == armDeadline {
				select {
				case <-s.Done():
					t.Error("the writer ended before its close was let go")
				default:
				}
			}
			f.release()
			await(t, s.Done(), "the sink's release")
			if !errors.Is(s.CleanupResult(), f.err) {
				t.Errorf("the cleanup is %v; want the close's failure", s.CleanupResult())
			}
			revoke()
			k.checkClosedEach(t)
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
