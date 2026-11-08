package alsapcm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A request whose sink's life has ended is answered by one verdict (B728 B9
// round 10), whichever arm of its wait answers it (the writer's reply, the
// writer's end, the request's own context's end, the release's deadline) and
// at the commands' entries alike: by what the request was answered and what
// the cleanup recorded, its answer never a cancellation or the sink's fence
// where a failure is answered. The cases fix the arm their caller's wait
// takes (nativeSeams.arm), so each arm is the one asked, whatever else is
// ready by then.

// armPick fixes the arm of the next request's wait (nativeSeams.arm): set,
// the next wait to ask takes that arm alone; every wait after it selects
// over every arm, as production's does.
type armPick struct{ next atomic.Int32 }

func (p *armPick) set(a awaitArm) { p.next.Store(int32(a)) }

func (p *armPick) pick() awaitArm { return awaitArm(p.next.Swap(int32(armAny))) }

// hook is lifeSinkOn's hook that hands the sink the pick.
func (p *armPick) hook(h *nativeSeams) { h.arm = p.pick }

// armName names an arm in a case's name.
var armName = map[awaitArm]string{
	armReply:    "through the writer's reply",
	armDone:     "through the writer's end",
	armContext:  "through its caller's context",
	armDeadline: "at the release's deadline",
}

// TestNativeAHeldRefusalIsAnsweredByOneVerdictOnEveryArm (B728 B9 round 10,
// Codex's review of round 9): a paused sink's resume, or a paused inert
// sink's first activation (ActivatePaused), refused by its admission while
// the life runs, a refusal the writer holds for the next resume and records
// nowhere, its caller on the life held where it begins to wait while the life
// ends and the release ends as the case has it. Let go, the caller takes the
// arm the case fixes, and every arm answers alike: with nothing recorded,
// the life's typed cancellation (B765: the ended life fences what was held);
// with a device's failure recorded and the release confirmed, that failure
// beside the refusal, and no ErrReleaseUnconfirmed; with the close failing,
// that failure (ErrReleaseUnconfirmed) beside the refusal; past the release's
// budget, the writer in its close, the release unconfirmed beside the
// refusal; and with that close let go, confirmed, before the caller takes the
// deadline's arm, the completed cleanup's answer, as with nothing recorded.
// Until round 10 the caller's own context's arm answered its cancellation
// beside the failure, the refusal lost, and the writer's reply and its end
// answered the refusal as it was where nothing was recorded.
func TestNativeAHeldRefusalIsAnsweredByOneVerdictOnEveryArm(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	refused := errors.New("the controls changed")
	for _, entry := range []struct {
		name string
		// refusal is the check after open the entry's admission is: the
		// open's is the first.
		refusal int32
		prepare func(*testing.T, *DSDSink)
		call    func(*DSDSink, context.Context) error
	}{
		{"a resume", 3, func(t *testing.T, s *DSDSink) {
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
		}, func(s *DSDSink, ctx context.Context) error { return s.Resume(ctx) }},
		{"a paused activation", 2, func(t *testing.T, s *DSDSink) {
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
		}, func(s *DSDSink, ctx context.Context) error { return s.ActivatePaused(ctx) }},
	} {
		for _, tc := range []struct {
			cleanup string
			arm     awaitArm
		}{
			{"nothing recorded", armReply}, {"nothing recorded", armDone}, {"nothing recorded", armContext},
			{"a device's failure recorded", armReply}, {"a device's failure recorded", armDone}, {"a device's failure recorded", armContext},
			{"its close failing", armReply}, {"its close failing", armDone}, {"its close failing", armContext},
			{"past the release's budget", armReply}, {"past the release's budget", armContext}, {"past the release's budget", armDeadline},
			{"past the budget, then confirmed", armDeadline},
		} {
			t.Run(entry.name+", "+tc.cleanup+", "+armName[tc.arm], func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				// Armed, its next STATUS answers EIO at once.
				st := &statusFault{sys: f, err: eio, held: make(chan struct{}), letGo: make(chan struct{})}
				close(st.letGo)
				c := nativeConfig()
				var checks atomic.Int32
				c.Check = func(NodeInfo) error {
					if checks.Add(1) == entry.refusal {
						return refused
					}
					return nil
				}
				hold := newCallerHold(t)
				pick := &armPick{}
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, st, c, life, func(h *nativeSeams) { h.awaiting = hold.awaiting }, pick.hook)
				entry.prepare(t, s)
				hold.armed.Store(true)
				pick.set(tc.arm)
				answer := make(chan error, 1)
				go func() { answer <- entry.call(s, life) }()
				await(t, hold.in, "the caller, held as it begins to wait")
				nativeWait(t, s, "the writer's reply, the refusal it holds", func() bool { return s.activation == nil })
				switch tc.cleanup {
				case "a device's failure recorded":
					// The writer, back in its pause's watch, meets the device's
					// EIO at the STATUS after its poll, which the period granted
					// here ends: the writer's end, its release confirmed.
					st.armed.Store(true)
					k.grant(s.Params().PeriodTime())
					await(t, s.Done(), "the writer's end on the device's EIO")
					end()
				case "past the release's budget", "past the budget, then confirmed":
					f.failing.Store(tc.cleanup == "past the release's budget")
					f.hold.Store(true)
					end()
					await(t, revoking, "the life's end, its revocation held")
					revoke()
					await(t, f.held, "the writer's release in its close, held")
					pastBudget(t, k, s)
					if tc.cleanup == "past the budget, then confirmed" {
						f.release()
						await(t, s.Done(), "the writer's late release, confirmed")
					}
				default:
					f.failing.Store(tc.cleanup == "its close failing")
					end()
					await(t, revoking, "the life's end, its revocation held")
					revoke()
					await(t, s.Done(), "the life's release")
				}
				hold.release()
				e := receive(t, answer, "the answer, taken once the life has ended")
				what := entry.name + ", " + tc.cleanup + ", " + armName[tc.arm]
				switch tc.cleanup {
				case "nothing recorded", "past the budget, then confirmed":
					theLifesCancellation(t, e, what)
					if errors.Is(e, refused) || errors.Is(e, ErrActivationFenced) {
						t.Errorf("%s answered %v; want the ended life's cancellation alone, not the refusal it fenced or the sink's fence", what, e)
					}
					noFailure(t, s, what)
				case "a device's failure recorded":
					theSinksFailure(t, e, eio, what)
					if !errors.Is(e, refused) || errors.Is(e, ErrReleaseUnconfirmed) {
						t.Errorf("%s answered %v; want the device's EIO beside the refusal, the release confirmed", what, e)
					}
				case "its close failing":
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
					if !errors.Is(e, refused) || !errors.Is(e, f.err) {
						t.Errorf("%s answered %v; want the close's failure beside the refusal", what, e)
					}
				default:
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what)
					if !errors.Is(e, refused) {
						t.Errorf("%s answered %v; want the release unconfirmed beside the refusal", what, e)
					}
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
				k.checkClosedEach(t)
			})
		}
	}
}

// TestNativeARecordedFenceIsAnsweredByOneVerdictOnEveryArm (B728 B9 round
// 10, Codex's review of round 9): an inert sink's first activation, its
// caller on a context of its own held where it begins to wait and its
// admission held, its caller's context then ended with the life running: let
// go, the admission meets the fence its context's end makes, and the writer,
// which no revocation has reached, records that fence as the sink's (s.err,
// sink_dsd_writer.go's classification) and begins its own Immediate
// release, which ends as the case has it; the life then ends and the caller,
// let go, takes the arm the case fixes. Every arm answers alike: with the
// release confirmed, the fence the only thing recorded, which is no
// failure, the life's typed cancellation; with its close failing, that
// failure (ErrReleaseUnconfirmed); past the release's budget, the writer in
// its close, the release unconfirmed. Never the sink's fence. Until round 10
// every arm answered the recorded fence, beside the failure where there was
// one, and the context's arm its cancellation too.
func TestNativeARecordedFenceIsAnsweredByOneVerdictOnEveryArm(t *testing.T) {
	for _, tc := range []struct {
		cleanup string
		arm     awaitArm
	}{
		{"its release confirmed", armReply}, {"its release confirmed", armDone}, {"its release confirmed", armContext},
		{"its close failing", armReply}, {"its close failing", armDone}, {"its close failing", armContext},
		{"past the release's budget", armReply}, {"past the release's budget", armContext}, {"past the release's budget", armDeadline},
	} {
		t.Run(tc.cleanup+", "+armName[tc.arm], func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			f.failing.Store(tc.cleanup != "its release confirmed")
			f.hold.Store(tc.cleanup == "past the release's budget")
			c := nativeConfig()
			admitting, gate := make(chan struct{}), make(chan struct{})
			var admitted sync.Once
			admit := func() { admitted.Do(func() { close(gate) }) }
			defer admit()
			var checks atomic.Int32
			c.Check = func(NodeInfo) error {
				if checks.Add(1) == 2 { // the open's check, then the activation's
					close(admitting)
					<-gate
				}
				return nil
			}
			hold := newCallerHold(t)
			pick := &armPick{}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, _, _ := lifeSinkOn(t, k, f, c, life, func(h *nativeSeams) { h.awaiting = hold.awaiting }, pick.hook)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hold.armed.Store(true)
			pick.set(tc.arm)
			answer := make(chan error, 1)
			go func() { answer <- s.Activate(ctx, 1) }()
			await(t, hold.in, "the activation's caller, held as it begins to wait")
			await(t, admitting, "the activation's admission, held")
			cancel()
			admit()
			nativeWait(t, s, "the writer's reply, the fence it met", func() bool { return s.activation == nil })
			if tc.cleanup == "past the release's budget" {
				await(t, f.held, "the writer's own release in its close, held")
				pastBudget(t, k, s)
			} else {
				await(t, s.Done(), "the writer's own release")
			}
			if r := s.Result(); !errors.Is(r, ErrActivationFenced) {
				t.Fatalf("the sink recorded %v; want the fence its activation met, the case's premise", r)
			}
			end()
			hold.release()
			e := receive(t, answer, "the activation's answer, taken once the life has ended")
			what := "the activation fenced by its own context, " + tc.cleanup + ", " + armName[tc.arm]
			switch tc.cleanup {
			case "its release confirmed":
				theLifesCancellation(t, e, what)
				if errors.Is(e, ErrActivationFenced) {
					t.Errorf("%s answered %v; want the ended life's cancellation, not the sink's fence", what, e)
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
			k.checkClosedEach(t)
		})
	}
}

// TestNativeAReleaseTheDeadlineCutIsAnsweredByOneVerdictOnEveryArm (B728 B9
// round 10, Claude's review of this round's brief): an activation asked while
// the writer, held at the STATUS after its first content write, has a frame
// queued, and a TrackChange release begun before the writer runs it: the
// writer drains, its drain held in its poll past the release's deadline,
// which cancels the drain. That cut is no Immediate's, so the writer records
// it as the release's failure, ErrReleaseUnconfirmed beside the drain's
// cancellation, closes the node and ends with the activation never run. The
// life then ends, and the activation's caller, on the life and held where it
// began to wait, takes the arm the case fixes: every arm answers the release
// unconfirmed, the recorded failure's words kept and its cancellation's
// chain dropped, so the answer is no cancellation (ReasonOf, context.Canceled)
// and its caller never takes it for a stop. Until round 10 every arm answered
// it with that chain, ReasonOf the drain's cancellation.
func TestNativeAReleaseTheDeadlineCutIsAnsweredByOneVerdictOnEveryArm(t *testing.T) {
	for _, arm := range []awaitArm{armReply, armDone, armContext, armDeadline} {
		t.Run(armName[arm], func(t *testing.T) {
			k := nativeKernel(t)
			hold := newCallerHold(t)
			pick := &armPick{}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, _, _ := lifeSinkOn(t, k, k, nativeConfig(), life, func(h *nativeSeams) { h.awaiting = hold.awaiting }, pick.hook)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			// The STATUS after the writer's first content write, held: the
			// activation asked and the TrackChange release begun there, the
			// writer meets the release first and never runs the activation.
			statusing, status := k.parkNextStatus()
			defer status()
			if _, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); e != nil {
				t.Fatal(e)
			}
			await(t, statusing, "the writer's STATUS after its content write")
			hold.armed.Store(true)
			pick.set(arm)
			answer := make(chan error, 1)
			go func() { answer <- s.Activate(life, 2) }()
			await(t, hold.in, "the activation's caller, held as it begins to wait")
			s.BeginRelease(TrackChange)
			polling, poll := k.parkNextPoll()
			defer poll()
			status()
			await(t, polling, "the release's drain in its poll")
			pastBudget(t, k, s)
			poll()
			await(t, s.Done(), "the writer's end, the activation never run")
			end()
			hold.release()
			e := receive(t, answer, "the activation's answer, taken once the life has ended")
			theSinksFailure(t, e, ErrReleaseUnconfirmed, "the activation the cut release never ran, "+armName[arm])
			recorded := s.Result()
			if ReasonOf(recorded) != ReasonCancelled || !errors.Is(recorded, context.Canceled) {
				t.Fatalf("the sink recorded %v; want the drain the deadline cut, the case's premise", recorded)
			}
			if !strings.Contains(e.Error(), recorded.Error()) {
				t.Errorf("the activation answered %q; want the recorded failure's words, %q, in it", e, recorded)
			}
			if e := s.CleanupResult(); e != nil {
				t.Errorf("the node's close failed: %v", e)
			}
			k.checkClosedEach(t)
		})
	}
}

// TestNativeTheReleasesDeadlineIsArmedOnce (B728 B9 round 10, Claude's review
// of round 9): releaseExpired, made once at the sink's construction, is
// closed by the timer of the release that begins first, which
// beginReleaseLocked arms only while no release has begun (!s.releasing),
// and nothing ends a release once begun; so a TrackChange, its deadline
// passed with the writer held in its close, then two Immediate releases,
// close it once, ask the clock for one deadline and panic nowhere, the
// second Immediate answered unconfirmed while the close is held. A regression
// pin of that guard, not a red.
func TestNativeTheReleasesDeadlineIsArmedOnce(t *testing.T) {
	k := nativeKernel(t)
	f := newCleanupFault(t, k)
	var deadlines atomic.Int32
	life, end := context.WithCancel(context.Background())
	defer end()
	s, _, _ := lifeSinkOn(t, k, f, nativeConfig(), life, func(h *nativeSeams) {
		at := h.at
		h.at = func(when time.Time) (<-chan time.Time, func()) {
			deadlines.Add(1)
			return at(when)
		}
	})
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	f.hold.Store(true)
	s.BeginRelease(TrackChange)
	await(t, f.held, "the TrackChange release in its close, held")
	pastBudget(t, k, s)
	s.Revoke()
	if e := s.Release(context.Background(), Immediate); !errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("the second Immediate release, past the deadline, answered %v; want it unconfirmed, the close still held", e)
	}
	f.release()
	await(t, s.Done(), "the release's close, let go")
	if n := deadlines.Load(); n != 1 {
		t.Errorf("the releases asked the clock for %d deadlines; want one, the TrackChange's", n)
	}
	select {
	case <-s.releaseExpired:
	default:
		t.Error("releaseExpired is open past its deadline")
	}
	noFailure(t, s, "the releases")
	k.checkClosedEach(t)
}
