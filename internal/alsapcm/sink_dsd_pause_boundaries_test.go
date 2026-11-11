package alsapcm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stagedPauseSink is a native sink no goroutine owns, its life running, for a
// Pause whose waits a test stages: active is whether it plays, so that Pause
// waits for the writer to apply its pause, and releasing whether a release is
// under way as Pause is called.
func stagedPauseSink(life context.Context, active, releasing bool, seams nativeSeams) *DSDSink {
	seams.now = time.Now
	s := &DSDSink{life: life, cfg: DSDConfig{LedgerLimit: 1}, active: active,
		changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}),
		releaseExpired: make(chan struct{}), seams: seams}
	if releasing {
		s.releasing, s.mode = true, Immediate
	}
	return s
}

// TestNativeAPauseStoppedInItsEndedLifesCleanupNeverAnswersSuccess: the
// sink's life ends, its watcher held before its revocation, so the sink is
// not yet releasing; a Pause on a context of its own, running, registers its
// pause on the inert sink and reaches settle's ended-life path, which revokes
// the sink and waits for that cleanup (endOfLife), its close held. The Pause's
// context ends in that wait, and the close then completes. Confirmed, the
// answer is the ended life's cancellation, never the success it is on a
// context running; failed, it is the cleanup's failure, which wins (B728 B9
// PR 1 round 6: settle's cleanup wait does not read the context, and the
// success answered nil).
func TestNativeAPauseStoppedInItsEndedLifesCleanupNeverAnswersSuccess(t *testing.T) {
	for _, cleanup := range []string{"its close confirmed", "its close failing"} {
		t.Run(cleanup, func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			f.failing.Store(cleanup == "its close failing")
			meeting := make(chan struct{}, 1)
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, func(h *nativeSeams) { h.meeting = signal(meeting) })
			end()
			await(t, revoking, "the life's end, its revocation held")
			f.hold.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			answer := make(chan error, 1)
			go func() { answer <- s.Pause(ctx) }()
			receive(t, meeting, "the Pause waiting for the cleanup its ended life began")
			await(t, f.held, "the release's close, held")
			cancel()
			f.release()
			e := receive(t, answer, "the Pause's answer")
			if cleanup == "its close failing" {
				theSinksFailure(t, e, ErrReleaseUnconfirmed, "a Pause stopped in a cleanup whose close failed")
				if !errors.Is(e, f.err) {
					t.Errorf("the Pause answered %v; want the close's failure", e)
				}
			} else {
				if e == nil {
					t.Fatal("a Pause stopped in its ended life's cleanup answered success")
				}
				theLifesCancellation(t, e, "a Pause stopped in its ended life's cleanup, its close confirmed")
			}
			revoke()
			await(t, s.Done(), "the sink's release")
			k.checkClosedEach(t)
		})
	}
}

// TestNativeAPauseStoppedInSettleAnswersItsStop: settle's other exits, the
// sink's life running. The writer's release ends clean while Pause waits for
// it, so the wait takes the writer's end and settles a refusal over a clean
// result; or a release is under way as Pause is called, which settles its
// refusal. Pause's context ends in settle, after settle's read of it for the
// release's outcome (staged: that read is never ready), and settle's wait
// then takes the release's end. The answer is Pause's own stop, never nil nor
// the refusal (B728 B9 PR 1 round 6: the writer's clean end answered nil, the
// release under way "native releasing").
func TestNativeAPauseStoppedInSettleAnswersItsStop(t *testing.T) {
	for _, exit := range []string{"the writer's end, clean", "a release under way at its entry"} {
		t.Run(exit, func(t *testing.T) {
			life, end := context.WithCancel(context.Background())
			defer end()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entry := exit == "a release under way at its entry"
			s := stagedPauseSink(life, !entry, entry, nativeSeams{})
			// settle reads the context once (its stop); the writer's end is
			// read before it, in Pause's wait for the writer.
			settleRead := 2
			if entry {
				settleRead = 1
			}
			calls := 0
			wait := verdictWaitContext{Context: ctx} // done nil: never ready
			wait.atWait = func() {
				calls++
				switch calls {
				case settleRead:
					if entry {
						close(s.done)
					}
					cancel()
				case 1:
					// Pause's wait for the writer: the writer's release ends
					// clean, and its end is the one arm ready.
					s.mu.Lock()
					s.releasing, s.mode = true, Immediate
					s.mu.Unlock()
					close(s.done)
				default:
					t.Fatalf("the Pause read its context's Done %d times", calls)
				}
			}
			e := s.Pause(wait)
			if calls != settleRead {
				t.Fatalf("the Pause read its context's Done %d times; want %d", calls, settleRead)
			}
			wantOwnCancellation(t, e, "a Pause whose context ended in settle, "+exit)
		})
	}
}

// TestNativeAPauseWhoseContextEndsWhileItWaitsForTheLockChangesNothing: a
// Pause on a context running finds it running at its entry, and then waits
// for the sink's lock, which the writer or another command holds; its
// context ends while it waits. Taking the lock, it reads its context again
// before it changes anything: it answers its stop, the pause, its epoch and
// its moment as they were and the writer not interrupted, on an inert sink
// and on one playing (B728 B9 PR 1 round 6: it paused the sink, moved the
// epoch and interrupted the writer, then answered its stop).
func TestNativeAPauseWhoseContextEndsWhileItWaitsForTheLockChangesNothing(t *testing.T) {
	for _, sink := range []string{"inert", "playing"} {
		t.Run(sink, func(t *testing.T) {
			life, end := context.WithCancel(context.Background())
			defer end()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reached := make(chan struct{})
			s := stagedPauseSink(life, sink == "playing", false, nativeSeams{pauseLocking: func() { close(reached) }})
			interrupted := false
			s.waitCancel = func() { interrupted = true }
			before, changed := marksOf(s), s.changed
			s.mu.Lock() // the lock's holder, the writer or another command
			answer := make(chan error, 1)
			go func() { answer <- s.Pause(ctx) }()
			await(t, reached, "the Pause past its first read of its context, about to take the lock")
			cancel()
			s.mu.Unlock()
			wantOwnCancellation(t, receive(t, answer, "the Pause's answer"), "a Pause whose context ended while it waited for the lock")
			if after := marksOf(s); !after.same(before) {
				t.Errorf("the Pause changed the sink's pause from %+v to %+v", before, after)
			}
			s.mu.Lock()
			notified := s.changed != changed || isClosed(changed)
			s.mu.Unlock()
			if notified || len(s.kick) != 0 || interrupted {
				t.Errorf("the Pause interrupted the writer: changed %v, kicked %v, its wait cancelled %v", notified, len(s.kick) != 0, interrupted)
			}
		})
	}
}

// TestNativeAPauseWhoseContextEndsInItsWaitDoesNotWaitAgain: a Pause waits
// for the writer to apply its pause, and its context ends beside a change
// that does not apply it, both ready: the wait takes the change (staged: the
// context's Done is never ready there). Taking the lock again, Pause reads
// its context and answers its stop, rather than waiting again (B728 B9 PR 1
// round 6: it waited once more, answering only when a second wait ended).
func TestNativeAPauseWhoseContextEndsInItsWaitDoesNotWaitAgain(t *testing.T) {
	life, end := context.WithCancel(context.Background())
	defer end()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := stagedPauseSink(life, true, false, nativeSeams{})
	calls := 0
	wait := verdictWaitContext{Context: ctx} // done nil: the wait takes the change
	wait.atWait = func() {
		calls++
		s.mu.Lock()
		// A wait after the first finds the pause applied, so a Pause that
		// waits again answers rather than hangs.
		s.pauseApplied = calls > 1
		s.notify()
		s.mu.Unlock()
		cancel()
	}
	wantOwnCancellation(t, s.Pause(wait), "a Pause whose context ended in its wait, beside a change that did not apply its pause")
	if calls != 1 {
		t.Errorf("the Pause waited %d times; its context's end, read as it took the lock again, answers at once", calls)
	}
	if !marksOf(s).paused {
		t.Error("the premise is a pause registered before the wait")
	}
}
