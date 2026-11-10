package alsapcm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func namesItsCommand(t *testing.T, e error, op, what string) {
	t.Helper()
	var x *Error
	if !errors.As(e, &x) || x.Op != op {
		t.Errorf("%s answered %v; want it to name %q, the command that asked", what, e, op)
	}
}

func TestNativeAReplyTheWaitTookIsJudgedWithTheRecordReadAgain(t *testing.T) {
	for _, asked := range []struct {
		name   string
		onLife bool
		arm    awaitArm
	}{{"at the release's deadline", false, armDeadline}, {"through its caller's context", true, armContext}} {
		for _, closeFails := range []bool{true, false} {
			cleanup := "its close confirmed late"
			if closeFails {
				cleanup = "its close failing late"
			}
			t.Run(asked.name+", "+cleanup, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				pick := &armPick{}
				recorded := newCallerHold(t)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, pick.hook, func(h *nativeSeams) { h.recorded = recorded.awaiting })
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				// The pause's first STATUS: the writer's next poll is then its
				// pause's watch.
				statusing, status := k.parkNextStatus()
				defer status()
				paused := make(chan error, 1)
				go func() { paused <- s.Pause(context.Background()) }()
				await(t, statusing, "the pause's STATUS")
				polling, poll := k.parkNextPoll()
				defer poll()
				status()
				if e := receive(t, paused, "the pause"); e != nil {
					t.Fatal(e)
				}
				await(t, polling, "the writer in its pause's watch")
				pick.set(asked.arm)
				recorded.armed.Store(true)
				answer := queueResume(t, s, life, asked.onLife)
				f.failing.Store(closeFails)
				f.hold.Store(true)
				end()
				revokeEnded(t, s, revoking, revoke, asked.onLife)
				poll()
				await(t, f.held, "the writer's release in its close, held")
				pastBudget(t, k, s)
				await(t, recorded.in, "the resume's caller, its wait over, held before it reads the reply")
				f.release()
				await(t, s.Done(), "the writer's late close, and its answer to the resume as it ended")
				recorded.release()
				e := receive(t, answer, "the resume's answer")
				what := "the resume the writer never ran, " + asked.name + ", " + cleanup
				if closeFails {
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
					if !errors.Is(e, f.err) {
						t.Errorf("%s answered %v; want the late close's failure in it", what, e)
					}
				} else {
					theLifesCancellation(t, e, what)
					noFailure(t, s, what)
				}
				k.checkClosedEach(t)
			})
		}
	}
}

func TestNativeAnAnswerNamesTheCommandThatAsked(t *testing.T) {
	refused := errors.New("the controls changed")
	for _, command := range []struct {
		name string
		op   string
		// activated: the sink activated before it was paused, so the
		// request's admission is the third check after open, not the second.
		activated bool
		call      func(*DSDSink) error
	}{
		{"a resume", "native resume", true, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"ActivatePaused of a sink never activated", "native activation", false, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
		{"ActivatePaused handing an activated sink to Resume", "native resume", true, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
	} {
		for _, overtaken := range []bool{false, true} {
			path := "refused at its admission, the life running"
			if overtaken {
				path = "overtaken by the release, the life ended"
			}
			t.Run(command.name+", "+path, func(t *testing.T) {
				k := nativeKernel(t)
				c := nativeConfig()
				admission := int32(2)
				if command.activated {
					admission = 3
				}
				var checks atomic.Int32
				c.Check = func(NodeInfo) error {
					if checks.Add(1) == admission && !overtaken {
						return refused
					}
					return nil
				}
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, k, c, life)
				if command.activated {
					if e := s.Activate(context.Background(), 1); e != nil {
						t.Fatal(e)
					}
				}
				what := command.name + ", " + path
				if !overtaken {
					if e := s.Pause(context.Background()); e != nil {
						t.Fatal(e)
					}
					e := command.call(s)
					if !errors.Is(e, refused) {
						t.Fatalf("%s answered %v; want its admission's refusal", what, e)
					}
					namesItsCommand(t, e, command.op, what)
					end()
					await(t, revoking, "the life's end, its revocation held")
					revoke()
				} else {
					// The pause's first STATUS: the writer's next poll is then its
					// pause's watch, where it is held while the request is asked
					// and the release overtakes it.
					statusing, status := k.parkNextStatus()
					defer status()
					paused := make(chan error, 1)
					go func() { paused <- s.Pause(context.Background()) }()
					await(t, statusing, "the pause's STATUS")
					polling, poll := k.parkNextPoll()
					defer poll()
					status()
					if e := receive(t, paused, "the pause"); e != nil {
						t.Fatal(e)
					}
					await(t, polling, "the writer in its pause's watch")
					answer := make(chan error, 1)
					go func() { answer <- command.call(s) }()
					nativeWait(t, s, "the request, the writer's to answer", func() bool { return s.activation != nil })
					end()
					await(t, revoking, "the life's end, its revocation held")
					revoke()
					nativeWait(t, s, "the revocation", func() bool { return s.releasing })
					poll()
					e := receive(t, answer, "the answer the writer gave as it ended")
					theLifesCancellation(t, e, what)
					namesItsCommand(t, e, command.op, what)
				}
				await(t, s.Done(), "the sink's release")
				noFailure(t, s, what)
				k.checkClosedEach(t)
			})
		}
	}
}

func TestNativeAnAnswerTheWriterSentAsItEndedWaitsForItsEnd(t *testing.T) {
	for _, arm := range []awaitArm{armReply, armDeadline} {
		for _, boundFirst := range []bool{false, true} {
			name := armName[arm] + ", the writer's end within the bound"
			if boundFirst {
				name = armName[arm] + ", the bound passing first"
			}
			t.Run(name, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				pick := &armPick{}
				recorded, ending := newCallerHold(t), newCallerHold(t)
				// The clock's second deadline asked is the bound's: the first is
				// the release's. The bound is registered before it is signalled, so the
				// test moves the clock only past a deadline the fake already holds.
				boundAsked := make(chan struct{})
				var asks atomic.Int32
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, pick.hook, func(h *nativeSeams) {
					h.recorded, h.ending = recorded.awaiting, ending.awaiting
					at := h.at
					h.at = func(when time.Time) (<-chan time.Time, func()) {
						ch, stop := at(when)
						if asks.Add(1) == 2 {
							close(boundAsked)
						}
						return ch, stop
					}
				})
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				// The pause's first STATUS: the writer's next poll is then its
				// pause's watch.
				statusing, status := k.parkNextStatus()
				defer status()
				paused := make(chan error, 1)
				go func() { paused <- s.Pause(context.Background()) }()
				await(t, statusing, "the pause's STATUS")
				polling, poll := k.parkNextPoll()
				defer poll()
				status()
				if e := receive(t, paused, "the pause"); e != nil {
					t.Fatal(e)
				}
				await(t, polling, "the writer in its pause's watch")
				pick.set(arm)
				recorded.armed.Store(arm == armDeadline)
				ending.armed.Store(true)
				answer := queueResume(t, s, life, false)
				f.hold.Store(true)
				end()
				revokeEnded(t, s, revoking, revoke, false)
				poll()
				await(t, f.held, "the writer's release in its close, held")
				pastBudget(t, k, s)
				if arm == armDeadline {
					await(t, recorded.in, "the resume's caller, its wait over, held before it reads the reply")
				}
				f.release()
				await(t, ending.in, "the writer, its answer to the resume sent, held before its end")
				recorded.release()
				await(t, boundAsked, "the resume's caller, its answer taken, waiting for the writer's end within the bound")
				what := "the resume the writer never ran, " + name
				if boundFirst {
					k.advanceNow(nativeEndAfterReply)
					e := receive(t, answer, "the resume's answer at the bound")
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
					select {
					case <-s.Done():
						t.Error("the writer ended before it was let go")
					default:
					}
					ending.release()
					await(t, s.Done(), "the writer's end")
				} else {
					ending.release()
					theLifesCancellation(t, receive(t, answer, "the resume's answer"), what)
				}
				noFailure(t, s, what)
				k.checkClosedEach(t)
			})
		}
	}
}
