package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// A request's answer once the sink's life has ended carries its cleanup's
// verdict (B728 B9 round 9): the writer's reply and every refusal of
// Activate, Resume and ActivatePaused wait for the writer's end, within the
// release's budget, the Immediate release the life's end begins being the
// one under test here.

// cleanupFault is the fake's node whose closes, once failing is set, fail
// with err beside the fake's own close, and whose next close, once hold is
// set, waits in the call, at the end of releaseNative, until let go: a
// release the writer cannot confirm, and one it confirms late.
type cleanupFault struct {
	sys
	err         error
	failing     atomic.Bool
	hold        atomic.Bool
	held, letGo chan struct{}
	once        sync.Once
}

func newCleanupFault(t *testing.T, k *fakeKernel) *cleanupFault {
	f := &cleanupFault{sys: k, err: errors.New("the node's close failed"), held: make(chan struct{}), letGo: make(chan struct{})}
	t.Cleanup(f.release)
	return f
}

// release lets a held close go on.
func (f *cleanupFault) release() { f.once.Do(func() { close(f.letGo) }) }

func (f *cleanupFault) close(fd int) error {
	if f.hold.CompareAndSwap(true, false) {
		close(f.held)
		<-f.letGo
	}
	if e := f.sys.close(fd); e != nil || !f.failing.Load() {
		return e
	}
	return f.err
}

// callerHold holds, once armed, the next request's caller where it begins to
// wait for its answer (nativeSeams.awaiting), until let go.
type callerHold struct {
	armed     atomic.Bool
	in, letGo chan struct{}
	once      sync.Once
}

func newCallerHold(t *testing.T) *callerHold {
	h := &callerHold{in: make(chan struct{}), letGo: make(chan struct{})}
	t.Cleanup(h.release)
	return h
}

func (h *callerHold) release() { h.once.Do(func() { close(h.letGo) }) }

func (h *callerHold) awaiting() {
	if h.armed.CompareAndSwap(true, false) {
		close(h.in)
		<-h.letGo
	}
}

// pastBudget moves the fake clock past the release's deadline, the shared
// one its begin armed (nativeReleaseBudget from then), and waits until the
// release's timer has said so.
func pastBudget(t *testing.T, k *fakeKernel, s *DSDSink) {
	t.Helper()
	s.mu.Lock()
	expired := s.releaseExpired
	s.mu.Unlock()
	k.advanceNow(nativeReleaseBudget)
	await(t, expired, "the release's budget to pass")
}

// TestNativeAnActivationOnAnEndedLifeAnswersItsCleanup (B728 B9 round 9,
// Codex's review of round 8 and Claude's of this round's brief): an Activate
// whose context is the sink's life, already ended, revokes the sink, and is
// answered once its writer has ended with what that end's release recorded:
// with its close failing, ErrReleaseUnconfirmed beside the close's own
// failure; with its close confirmed, the life's typed cancellation. With the
// life running, a caller whose own context ended is answered that end at
// once, the writer still in its release. And an Activate on a context the
// life does not end, refused at its entry while the release is past its
// budget, is answered the release unconfirmed. Until round 9 the first two
// answered the plain cancellation at once, before the release, and the last
// the life's cancellation. And with a device's failure recorded before the
// life ended, its release confirmed, the activation answers that failure,
// with no ErrReleaseUnconfirmed (round 10); until round 10 that answer, and
// the one with the close failing, carried the activation's own context's
// cancellation beside the failure.
func TestNativeAnActivationOnAnEndedLifeAnswersItsCleanup(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	for _, cleanup := range []string{"its close failing", "its close confirmed", "a device's failure recorded"} {
		t.Run(cleanup, func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			f.failing.Store(cleanup == "its close failing")
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, _ := lifeSinkOn(t, k, f, nativeConfig(), life)
			if cleanup == "a device's failure recorded" {
				// An inert sink's writer makes no call until it is paused, whose
				// STATUS then fails: the writer ends on it, its release
				// confirmed, and its watcher ends with it.
				k.inspect(func(k *fakeKernel) { k.fail[ioctlStatus] = eio })
				if e := s.Pause(context.Background()); e != nil {
					t.Fatal(e)
				}
				await(t, s.Done(), "the writer's end on the device's EIO")
				end()
			} else {
				end()
				await(t, revoking, "the life's end, its revocation held")
			}
			e := s.Activate(life, 1)
			await(t, s.Done(), "the release the activation began")
			switch cleanup {
			case "its close failing":
				theSinksFailure(t, e, ErrReleaseUnconfirmed, "the activation on the ended life, its close failing")
				if !errors.Is(e, f.err) || !errors.Is(s.CleanupResult(), f.err) {
					t.Errorf("the answer %v, the cleanup %v: want the close's failure in both", e, s.CleanupResult())
				}
			case "its close confirmed":
				theLifesCancellation(t, e, "the activation on the ended life, its close confirmed")
				noFailure(t, s, "the release the activation began")
			default:
				theSinksFailure(t, e, eio, "the activation on the ended life, a device's failure recorded")
				if errors.Is(e, ErrReleaseUnconfirmed) || s.CleanupResult() != nil {
					t.Errorf("the answer %v, the cleanup %v: the release was confirmed, want no ErrReleaseUnconfirmed in either", e, s.CleanupResult())
				}
			}
			k.checkClosedEach(t)
		})
	}
	t.Run("the life running", func(t *testing.T) {
		k := nativeKernel(t)
		f := newCleanupFault(t, k)
		f.hold.Store(true)
		life, end := context.WithCancel(context.Background())
		defer end()
		s, _, _ := lifeSinkOn(t, k, f, nativeConfig(), life)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		answer := make(chan error, 1)
		go func() { answer <- s.Activate(ctx, 1) }()
		if e := receive(t, answer, "the answer of an activation whose own context ended, the life running"); !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) {
			t.Errorf("the activation answered %v; want its context's end", e)
		}
		await(t, f.held, "the writer's release in its close: the revocation the activation made ends the writer")
		select {
		case <-s.Done():
			t.Error("the writer ended before its close was let go")
		default:
		}
		f.release()
		await(t, s.Done(), "the release the activation began")
		noFailure(t, s, "the release the activation began")
		k.checkClosedEach(t)
	})
	t.Run("refused at its entry past its release's budget", func(t *testing.T) {
		k := nativeKernel(t)
		f := newCleanupFault(t, k)
		f.failing.Store(true)
		f.hold.Store(true)
		life, end := context.WithCancel(context.Background())
		defer end()
		s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life)
		end()
		await(t, revoking, "the life's end, its revocation held")
		revoke()
		await(t, f.held, "the writer's release in its close, held")
		pastBudget(t, k, s)
		answer := make(chan error, 1)
		go func() { answer <- s.Activate(context.Background(), 1) }()
		e := receive(t, answer, "the answer of the activation refused at its entry, the close still held")
		theSinksFailure(t, e, ErrReleaseUnconfirmed, "the activation refused at its entry past its release's budget")
		select {
		case <-s.Done():
			t.Error("the writer ended before its close was let go")
		default:
		}
		f.release()
		await(t, s.Done(), "the writer's late release")
		if !errors.Is(s.CleanupResult(), f.err) {
			t.Errorf("the cleanup is %v; want the late close's failure", s.CleanupResult())
		}
		k.checkClosedEach(t)
	})
}

// TestNativeAReplyTakenAfterTheLifeEndsCarriesItsCleanup (B728 B9 round 9,
// Claude's review of round 8 and Codex's of this round's brief): the writer
// answers an activation, a success or its admission's refusal, while its
// caller is held where it begins to wait; then the life ends and the
// release's close fails. The caller, let go, takes the reply with the life
// ended, and its answer carries the close's failure beside the refusal it
// was, by every arm alike: the writer's reply and the writer's end (its
// caller on a context the life does not end) and its own context's end (its
// caller on the life), each fixed (nativeSeams.arm). Until round 9 the reply
// was answered as it was sent, the close's failure lost: always so off the
// life, and on it whenever the select took the reply or the writer's end
// over the context's arm; until round 10 the context's arm answered the
// failure beside its cancellation, the refusal lost.
func TestNativeAReplyTakenAfterTheLifeEndsCarriesItsCleanup(t *testing.T) {
	refused := errors.New("the controls changed")
	for _, success := range []bool{true, false} {
		reply := "an admission's refusal"
		if success {
			reply = "a success"
		}
		for _, asked := range []struct {
			name   string
			onLife bool
			arm    awaitArm
		}{{"through its reply", false, armReply}, {"through the writer's end", false, armDone}, {"through its caller's context", true, armContext}} {
			t.Run(reply+" "+asked.name, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				f.failing.Store(true)
				c := nativeConfig()
				if !success {
					var checks atomic.Int32
					c.Check = func(NodeInfo) error {
						if checks.Add(1) == 2 { // the open's check, then the activation's
							return refused
						}
						return nil
					}
				}
				hold := newCallerHold(t)
				pick := &armPick{}
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, c, life, func(h *nativeSeams) { h.awaiting = hold.awaiting }, pick.hook)
				ctx := context.Background()
				if asked.onLife {
					ctx = life
				}
				hold.armed.Store(true)
				pick.set(asked.arm)
				answer := make(chan error, 1)
				go func() { answer <- s.Activate(ctx, 1) }()
				await(t, hold.in, "the activation's caller, held as it begins to wait")
				nativeWait(t, s, "the writer's reply to the activation", func() bool { return s.activation == nil })
				if success {
					// The writer plays on: the life's end is its release.
					end()
					await(t, revoking, "the life's end, its revocation held")
					revoke()
					await(t, s.Done(), "the life's release, its close failing")
				} else {
					// The refusal ended the writer: its own release, its close
					// failing, came before the life's end.
					await(t, s.Done(), "the writer's release after the refusal, its close failing")
					end()
				}
				hold.release()
				e := receive(t, answer, "the activation's answer, taken once the life has ended")
				theSinksFailure(t, e, ErrReleaseUnconfirmed, "the activation's "+reply+", its close failing after it")
				if !errors.Is(e, f.err) || !success && !errors.Is(e, refused) {
					t.Errorf("the activation answered %v; want the close's failure beside what the reply said", e)
				}
				k.checkClosedEach(t)
			})
		}
	}
}

// TestNativeAReopenFencedByTheEndedLifeAnswersItsFailedClose (B728 B9 round
// 9, Claude's review of this round's brief): a resume after the pause's
// release reopens the node with the sink's life ended, its watcher held, and
// the reopened node's close fails. Met by the fence, the resume answers that
// failure, ErrReleaseUnconfirmed, never the sink's fence, which a caller
// takes for its drop's: until round 9 the fence's own close failing was
// joined to ErrActivationFenced. Refused by DoP's precision check, it
// answered the failure already: nativeCleanupError joins ErrReleaseUnconfirmed,
// so the writer records the answer as terminal (s.err), beside the close's
// own record (cleanupErr).
func TestNativeAReopenFencedByTheEndedLifeAnswersItsFailedClose(t *testing.T) {
	for _, dop := range []bool{false, true} {
		name := "the fence"
		if dop {
			name = "the DoP precision refusal"
		}
		t.Run(name, func(t *testing.T) {
			k, c := nativeKernel(t), nativeConfig()
			if dop {
				k, c = dopKernel(t), dopConfig()
			}
			f := newCleanupFault(t, k)
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, _ := lifeSinkOn(t, k, f, c, life)
			pauseUntilReleased(t, k, s)
			if dop {
				k.inspect(func(k *fakeKernel) { k.commit = func(p *hwParams) { p.msbits = 20 } })
			}
			f.failing.Store(true)
			end()
			await(t, revoking, "the life's end, its revocation held")
			e := s.Resume(context.Background())
			theSinksFailure(t, e, ErrReleaseUnconfirmed, "the resume whose reopened node's close failed")
			if !errors.Is(e, f.err) {
				t.Errorf("the resume answered %v; want the reopened node's close's failure", e)
			}
			await(t, s.Done(), "the writer's release")
			if !errors.Is(s.Result(), f.err) || !errors.Is(s.CleanupResult(), f.err) {
				t.Errorf("the outcome %v, the cleanup %v: want the close's failure in both", s.Result(), s.CleanupResult())
			}
			k.checkClosedEach(t)
		})
	}
}

// TestNativeACallerWaitsNoLongerThanTheReleasesBudget (B728 B9 round 9, both
// reviews of round 8 and Codex's of this round's brief): a resume asked while
// the writer sits in its pause's watch, the life ended, and the writer held in
// its release's close past the release's budget. The resume is answered at
// that deadline, ErrReleaseUnconfirmed, the writer still in its cleanup and
// the sink not done, by its caller on a context the life does not end and on
// the life alike, through the deadline's arm and its own context's, each
// fixed (nativeSeams.arm); let go, the close fails late, into cleanupErr, the
// sink is done, and the writer answered the request once. Until round 9 the
// caller off the life had no arm the deadline reached, and waited for the
// close; until round 10 the one on the life answered the release unconfirmed
// beside its context's cancellation.
func TestNativeACallerWaitsNoLongerThanTheReleasesBudget(t *testing.T) {
	for _, asked := range []struct {
		name   string
		onLife bool
		arm    awaitArm
	}{{"at the release's deadline", false, armDeadline}, {"through its caller's context", true, armContext}} {
		t.Run(asked.name, func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			pick := &armPick{}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, pick.hook)
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
			answer := queueResume(t, s, life, asked.onLife)
			s.mu.Lock()
			r := s.activation
			s.mu.Unlock()
			f.failing.Store(true)
			f.hold.Store(true)
			end()
			revokeEnded(t, s, revoking, revoke, asked.onLife)
			poll()
			await(t, f.held, "the writer's release in its close, held")
			pastBudget(t, k, s)
			e := receive(t, answer, "the resume's answer at the release's deadline")
			theSinksFailure(t, e, ErrReleaseUnconfirmed, "the resume past its release's budget")
			select {
			case <-s.Done():
				t.Error("the writer ended before its close was let go")
			default:
			}
			f.release()
			await(t, s.Done(), "the writer's late release")
			if !errors.Is(s.CleanupResult(), f.err) {
				t.Errorf("the cleanup is %v; want the late close's failure", s.CleanupResult())
			}
			if n := len(r.reply); n != 1 {
				t.Errorf("the writer answered the resume %d times, want once", n)
			}
			k.checkClosedEach(t)
		})
	}
}
