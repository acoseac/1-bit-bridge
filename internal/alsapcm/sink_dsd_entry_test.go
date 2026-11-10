package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestNativeEveryEntryOnAnEndedLifeAnswersByTheVerdict: every return of every
// native command, its life ended, answers the cleanup's verdict; and, in the
// columns "the life running" names, a cleanup failure recorded, or a release
// past its budget, with the life still running answers that failure too,
// never a cancellation or the sink's fence (B728 B9 PR 1 round 3: a
// TrackChange drain its deadline cut records ErrReleaseUnconfirmed beside the
// drain's cancellation without ending the life). A command that meets a
// release under way waits for its outcome within the release's budget, its
// life running or ended, so the budget column's answers come only once the
// budget has passed, the close still held.
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
		{"Pause refused at its entry", "native pause", false, func(s *DSDSink) error { return s.Pause(context.Background()) }},
		{"Pause on a context already ended", "native pause", false, func(s *DSDSink) error { return s.Pause(ended) }},
		{"WriteDSD refused at its entry", "native write", false, func(s *DSDSink) error {
			_, err := s.WriteDSD(context.Background(), 1, []byte{1}, []byte{2}, 0, 8)
			return err
		}},
		{"WriteDSD on a context already ended", "native write", false, func(s *DSDSink) error {
			_, err := s.WriteDSD(ended, 1, []byte{1}, []byte{2}, 0, 8)
			return err
		}},
		// Its bit counts refused before its producer's lock: the first return.
		{"WriteDSD of planes of different lengths", "native write", false, func(s *DSDSink) error {
			_, err := s.WriteDSD(context.Background(), 1, []byte{1}, []byte{2, 3}, 0, 8)
			return err
		}},
		// No bits skip the loop: the completion's return, a success where the
		// sink's cleanup recorded nothing.
		{"WriteDSD of no bits", "native write", true, func(s *DSDSink) error {
			_, err := s.WriteDSD(context.Background(), 1, nil, nil, 0, 0)
			return err
		}},
		{"Activate refused at its entry", "native activation", false, func(s *DSDSink) error { return s.Activate(context.Background(), 1) }},
		{"Activate on a context already ended", "native activation", false, func(s *DSDSink) error { return s.Activate(ended, 1) }},
		{"Resume refused at its entry", "native resume", false, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"Resume of a sink not paused", "native resume", true, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"ActivatePaused refused at its entry", "native activation", false, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
		{"ActivatePaused handing an activated sink to Resume", "native resume", true, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
	} {
		for _, cleanup := range []string{"nothing recorded", "a device's failure recorded", "the fence alone recorded", "its close failing", "past the release's budget", "a deadline-cut drain",
			"past the release's budget, the life running", "a deadline-cut drain, the life running"} {
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
				// The entry's command waiting for the release's outcome.
				meeting := make(chan struct{}, 1)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, c, life, func(h *nativeSeams) {
					h.awaiting = hold.awaiting
					h.meeting = func() {
						select {
						case meeting <- struct{}{}:
						default:
						}
					}
				})
				if entry.notPaused {
					if e := s.Activate(context.Background(), 1); e != nil {
						t.Fatal(e)
					}
				}
				answer := make(chan error, 1)
				switch cleanup {
				case "a deadline-cut drain", "a deadline-cut drain, the life running":
					if !entry.notPaused {
						if err := s.Activate(context.Background(), 1); err != nil {
							t.Fatal(err)
						}
					}
					statusing, status := k.parkNextStatus()
					defer status()
					if _, err := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); err != nil {
						t.Fatal(err)
					}
					await(t, statusing, "content STATUS before TrackChange")
					s.BeginRelease(TrackChange)
					polling, poll := k.parkNextPoll()
					defer poll()
					status()
					await(t, polling, "TrackChange drain in its poll")
					pastBudget(t, k, s)
					poll()
					await(t, s.Done(), "deadline-cut drain cleanup")
					if rec := s.Result(); !errors.Is(rec, context.Canceled) || !errors.Is(rec, ErrReleaseUnconfirmed) {
						t.Fatalf("record %v; want unconfirmed drain with cancellation, the premise", rec)
					}
					if cleanup == "a deadline-cut drain" {
						end()
					} else if life.Err() != nil {
						t.Fatal("the drain ended the sink's life; the premise is a life still running")
					}
					answer <- entry.call(s)
				case "past the release's budget, the life running":
					// The session's own release, its close failing and held,
					// the life running.
					f.failing.Store(true)
					f.hold.Store(true)
					s.BeginRelease(Immediate)
					await(t, f.held, "the release's close, held")
					go func() { answer <- entry.call(s) }()
					// Only a command that waits for the release's outcome is
					// there before its budget passes.
					receive(t, meeting, "the entry waiting for the release's outcome, its life running")
					pastBudget(t, k, s)
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
						// The watcher's revocation in the close before the call,
						// so the entry meets the release it names: called first,
						// a Pause or a WriteDSD went past its entry's refusal to
						// another return (round 3).
						revoke()
						await(t, f.held, "the release's close, held")
						go func() { answer <- entry.call(s) }()
					} else {
						// Its sink's revocation is the entry's own.
						go func() { answer <- entry.call(s) }()
						await(t, f.held, "the release's close, held")
					}
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
				case "a deadline-cut drain", "a deadline-cut drain, the life running":
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
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
