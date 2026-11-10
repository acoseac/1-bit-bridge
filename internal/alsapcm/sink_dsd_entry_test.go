package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestNativeEveryEntryOnAnEndedLifeAnswersByTheVerdict (B728 B9 round 10,
// Claude's review of this round's brief, and Codex's of round 9 for the
// fence): the entries of Activate, Resume and ActivatePaused answer a request
// on an ended life by the verdict every arm of a wait answers by: Activate
// refused at its entry and on a context already ended, Resume refused at its
// entry and of a sink not paused, ActivatePaused refused at its entry and
// handing an activated sink to Resume; with nothing recorded, the life's
// typed cancellation of its command, a sink not paused a success; with a
// device's failure recorded, that failure, no ErrReleaseUnconfirmed; with the
// fence alone recorded (the activation its own context fenced, the writer's
// record), the life's typed cancellation and not the fence; with the close
// failing, that failure; past the release's budget, the release unconfirmed.
// A sink not paused is one the writer has not recorded a failure for, so the
// device's failure and the fence leave those two entries refused, rows of
// the others. Until round 10 a refusal answered a recorded fence beside fixed
// words, and Activate on a context already ended its context's cancellation
// beside any failure.
func TestNativeEveryEntryOnAnEndedLifeAnswersByTheVerdict(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	ended, cancelled := context.WithCancel(context.Background())
	cancelled()
	for _, entry := range []struct {
		name string
		op   string
		// notPaused: the sink activated and not paused when the entry is
		// called, its life ended and no one having revoked it yet.
		notPaused bool
		call      func(*DSDSink) error
	}{
		{"Activate refused at its entry", "native activation", false, func(s *DSDSink) error { return s.Activate(context.Background(), 1) }},
		{"Activate on a context already ended", "native activation", false, func(s *DSDSink) error { return s.Activate(ended, 1) }},
		{"Resume refused at its entry", "native resume", false, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"Resume of a sink not paused", "native resume", true, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"ActivatePaused refused at its entry", "native activation", false, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
		{"ActivatePaused handing an activated sink to Resume", "native resume", true, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
	} {
		for _, cleanup := range []string{"nothing recorded", "a device's failure recorded", "the fence alone recorded", "its close failing", "past the release's budget"} {
			if entry.notPaused && (cleanup == "a device's failure recorded" || cleanup == "the fence alone recorded") {
				continue
			}
			t.Run(entry.name+", "+cleanup, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				c := nativeConfig()
				// The fence alone: the first activation's admission held, the
				// second check after open.
				admitting, gate := make(chan struct{}), make(chan struct{})
				var admitted sync.Once
				admit := func() { admitted.Do(func() { close(gate) }) }
				defer admit()
				var checks atomic.Int32
				if cleanup == "the fence alone recorded" {
					c.Check = func(NodeInfo) error {
						if checks.Add(1) == 2 {
							close(admitting)
							<-gate
						}
						return nil
					}
				}
				hold := newCallerHold(t)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, c, life, func(h *nativeSeams) { h.awaiting = hold.awaiting })
				if entry.notPaused {
					if e := s.Activate(context.Background(), 1); e != nil {
						t.Fatal(e)
					}
				}
				answer := make(chan error, 1)
				switch cleanup {
				case "a device's failure recorded":
					// An inert sink's writer makes no call until it is paused,
					// whose STATUS then fails: the writer ends on it, its release
					// confirmed.
					k.inspect(func(k *fakeKernel) { k.fail[ioctlStatus] = eio })
					if e := s.Pause(context.Background()); e != nil {
						t.Fatal(e)
					}
					await(t, s.Done(), "the writer's end on the device's EIO")
					end()
					answer <- entry.call(s)
				case "the fence alone recorded":
					// An activation on a context of its own, its caller held
					// where it begins to wait, its admission held while its
					// context ends: the writer records the fence and releases.
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					hold.armed.Store(true)
					first := make(chan error, 1)
					go func() { first <- s.Activate(ctx, 1) }()
					await(t, hold.in, "the first activation's caller, held")
					await(t, admitting, "the first activation's admission, held")
					cancel()
					admit()
					await(t, s.Done(), "the writer's own release, the fence recorded")
					if r := s.Result(); !errors.Is(r, ErrActivationFenced) {
						t.Fatalf("the sink recorded %v; want the fence, the case's premise", r)
					}
					end()
					answer <- entry.call(s)
					hold.release()
					receive(t, first, "the first activation's answer")
				case "past the release's budget":
					f.failing.Store(true)
					f.hold.Store(true)
					end()
					await(t, revoking, "the life's end, its revocation held")
					if !entry.notPaused {
						revoke()
					}
					go func() { answer <- entry.call(s) }()
					await(t, f.held, "the release's close, held")
					pastBudget(t, k, s)
				default:
					f.failing.Store(cleanup == "its close failing")
					end()
					await(t, revoking, "the life's end, its revocation held")
					if !entry.notPaused {
						revoke()
						await(t, s.Done(), "the life's release")
					}
					answer <- entry.call(s)
				}
				e := receive(t, answer, "the entry's answer")
				what := entry.name + ", " + cleanup
				switch cleanup {
				case "nothing recorded", "the fence alone recorded":
					if entry.notPaused {
						if e != nil {
							t.Errorf("%s answered %v; want the success it was", what, e)
						}
						break
					}
					theLifesCancellation(t, e, what)
					var x *Error
					if !errors.As(e, &x) || x.Op != entry.op || errors.Is(e, ErrActivationFenced) {
						t.Errorf("%s answered %v; want the ended life's cancellation of %s, not the sink's fence", what, e, entry.op)
					}
				case "a device's failure recorded":
					theSinksFailure(t, e, eio, what)
					if errors.Is(e, ErrReleaseUnconfirmed) {
						t.Errorf("%s answered %v; the release was confirmed, want no ErrReleaseUnconfirmed", what, e)
					}
				case "its close failing":
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
					if !errors.Is(e, f.err) {
						t.Errorf("%s answered %v; want the close's failure", what, e)
					}
				default:
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
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
				}
				await(t, s.Done(), "the sink's end")
				revoke()
				k.checkClosedEach(t)
			})
		}
	}
}
