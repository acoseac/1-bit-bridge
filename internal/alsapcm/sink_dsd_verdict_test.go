package alsapcm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

// cancelledAnywhere says whether an error of ReasonCancelled is anywhere in
// e's chain, the test's own walk: ReasonOf reads the first error's reason
// alone.
func cancelledAnywhere(e error) bool {
	if x, ok := e.(*Error); ok && x.Reason == ReasonCancelled {
		return true
	}
	if u, ok := e.(interface{ Unwrap() []error }); ok {
		for _, w := range u.Unwrap() {
			if cancelledAnywhere(w) {
				return true
			}
		}
	}
	if u, ok := e.(interface{ Unwrap() error }); ok && u.Unwrap() != nil {
		return cancelledAnywhere(u.Unwrap())
	}
	return false
}

func TestNativeTheVerdictOfARequestWhoseLifeEnded(t *testing.T) {
	ep := testEndpoint
	fault := &Error{Reason: ReasonWrongDevice, Op: "native activation", Endpoint: ep, Detail: "the check after open refused the node", Err: errors.New("the controls changed")}
	held := &Error{Reason: ReasonNotLocated, Op: "native reopen", Endpoint: ep, Detail: "the binding resolves to no unit"}
	xrun := &Error{Reason: ReasonXrun, Op: "native status", Endpoint: ep, Detail: "the stream underran"}
	closeFailed := errors.New("the node's close failed")
	cut := &Error{Reason: ReasonCancelled, Op: "drain", Endpoint: ep, Err: context.Canceled}
	cutByItsDeadline := &Error{Reason: ReasonCancelled, Op: "drain", Endpoint: ep, Err: context.DeadlineExceeded}
	prefillCut := &Error{Reason: ReasonCancelled, Op: "write", Endpoint: ep, Err: context.Canceled}
	entryRefusal := errors.New("native resume fenced")
	record := func(ended bool, e error) nativeRecord {
		return nativeRecord{err: e, ended: ended, end: context.Canceled, endpoint: ep}
	}
	for _, c := range []struct {
		name string
		rec  nativeRecord
		// failure is what of the record the answer carries; none: nothing
		// recorded that is a failure.
		failure []error
		// unconfirmed: the record carries ErrReleaseUnconfirmed.
		unconfirmed bool
		// dropped is what of the record no answer carries: a cancellation's
		// chain, gone whole.
		dropped []error
	}{
		{"nothing recorded", record(true, nil), nil, false, nil},
		{"the fence alone recorded", record(true, errors.Join(ErrActivationFenced)), nil, false, nil},
		{"a device's failure recorded, the release confirmed", record(true, errors.Join(xrun)), []error{xrun}, false, nil},
		{"the request's own fault recorded, the release confirmed", record(true, errors.Join(fault)), []error{fault}, false, nil},
		{"a close that failed", record(true, errors.Join(errors.Join(ErrReleaseUnconfirmed, closeFailed))), []error{ErrReleaseUnconfirmed, closeFailed}, true, nil},
		{"the fence recorded, then a close that failed", record(true, errors.Join(errors.Join(ErrActivationFenced), errors.Join(ErrReleaseUnconfirmed, closeFailed))), []error{ErrReleaseUnconfirmed, closeFailed}, true, nil},
		{"the request's own fault recorded, then a close that failed", record(true, errors.Join(errors.Join(fault), errors.Join(ErrReleaseUnconfirmed, closeFailed))), []error{fault, ErrReleaseUnconfirmed, closeFailed}, true, nil},
		{"a TrackChange drain the deadline cut", record(true, errors.Join(errors.Join(ErrReleaseUnconfirmed, cut))), []error{ErrReleaseUnconfirmed}, true, []error{cut}},
		{"a device's failure, then a drain cut by a deadline of its own", record(true, errors.Join(xrun, errors.Join(ErrReleaseUnconfirmed, cutByItsDeadline))), []error{xrun, ErrReleaseUnconfirmed}, true,
			[]error{cutByItsDeadline, context.DeadlineExceeded}},
		// The release out of time before its drain: no cancellation, a
		// failure whose deadline is its own, kept.
		{"a TrackChange release out of time before its drain", record(true, errors.Join(errors.Join(ErrReleaseUnconfirmed, context.DeadlineExceeded))), []error{ErrReleaseUnconfirmed, context.DeadlineExceeded}, true, nil},
		{"past the budget, nothing recorded yet", record(false, nil), nil, false, nil},
		{"past the budget, a device's failure recorded", record(false, errors.Join(xrun)), []error{xrun}, false, nil},
		{"past the budget, a TrackChange drain the deadline cut", record(false, errors.Join(errors.Join(ErrReleaseUnconfirmed, cut))), []error{ErrReleaseUnconfirmed}, true, []error{cut}},
	} {
		for _, r := range []struct {
			name string
			// own is the request's own failure, which the answer keeps beside
			// a failure; nil where the request was answered none.
			own error
			// not is what of the request's answer no failure answered may
			// carry.
			not error
			of  func(rec nativeRecord) nativeAnswer
		}{
			{"a success", nil, nil, func(nativeRecord) nativeAnswer { return nativeAnswer{kind: answerSuccess, op: "native activation"} }},
			{"a fault of its own", fault, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: fault, kind: answerFault, op: "native activation"}
			}},
			{"a refusal the writer held", held, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: held, kind: answerHeld, op: "native activation"}
			}},
			{"a cancellation the writer held", nil, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: context.Canceled, kind: answerHeld, op: "native activation"}
			}},
			{"the sink's fence", nil, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: ErrActivationFenced, kind: answerFenced, op: "native activation"}
			}},
			{"a prefill the release cut", nil, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: fmt.Errorf("%w: the prefill was cut short: %w", ErrActivationFenced, prefillCut), kind: answerFenced, op: "native activation"}
			}},
			{"a resume's fence", nil, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: errNativeResumeFenced, kind: answerFenced, op: "native activation"}
			}},
			{"the life's cancellation", nil, nil, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: lifeCancelledAt("native activation", ep, context.Canceled), kind: answerFenced, op: "native activation"}
			}},
			{"a refusal at its entry", nil, entryRefusal, func(nativeRecord) nativeAnswer {
				return nativeAnswer{err: entryRefusal, kind: answerFenced, op: "native resume"}
			}},
			{"a request never run", nil, errNativeActivationCancelled, func(rec nativeRecord) nativeAnswer {
				// The writer's reply as it ends: fenced, the record joined.
				e := error(lifeCancelledAt("native activation", ep, context.Canceled))
				if rec.err != nil {
					e = errors.Join(errNativeActivationCancelled, rec.err)
				}
				return nativeAnswer{err: e, kind: answerNeverRan, op: "native activation"}
			}},
			{"no answer", nil, nil, func(nativeRecord) nativeAnswer { return nativeAnswer{kind: answerNone, op: "native activation"} }},
		} {
			t.Run(r.name+", "+c.name, func(t *testing.T) {
				a := r.of(c.rec)
				e := verdict(a, c.rec)
				if c.rec.ended && c.failure == nil {
					switch a.kind {
					case answerSuccess:
						if e != nil {
							t.Errorf("answered %v; want the success it was", e)
						}
					case answerFault:
						if e != a.err {
							t.Errorf("answered %v; want the fault itself, %v", e, a.err)
						}
					default:
						var x *Error
						if !errors.As(e, &x) || x.Reason != ReasonCancelled || x.Op != a.op || x.Endpoint != ep || !errors.Is(e, context.Canceled) ||
							errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, ErrActivationFenced) || errors.Is(e, errNativeResumeFenced) || errors.Is(e, held) {
							t.Errorf("answered %v; want the ended life's typed cancellation of %s, alone", e, a.op)
						}
					}
					return
				}
				if e == nil {
					t.Fatal("answered nil; want the failure")
				}
				for _, f := range c.failure {
					if !errors.Is(e, f) {
						t.Errorf("answered %v; want %v in it", e, f)
					}
				}
				if want := c.unconfirmed || !c.rec.ended; errors.Is(e, ErrReleaseUnconfirmed) != want {
					t.Errorf("answered %v; want ErrReleaseUnconfirmed in it %v", e, want)
				}
				if errors.Is(e, context.Canceled) || cancelledAnywhere(e) || ReasonOf(e) == ReasonCancelled ||
					errors.Is(e, ErrActivationFenced) || errors.Is(e, errNativeResumeFenced) {
					t.Errorf("answered %v; want no cancellation and not the sink's fence", e)
				}
				if r.own != nil && (!errors.Is(e, r.own) || strings.Count(e.Error(), r.own.Error()) != 1) {
					t.Errorf("answered %v; want the request's own %v in it, once", e, r.own)
				}
				if r.not != nil && errors.Is(e, r.not) {
					t.Errorf("answered %v; want nothing of %v in it", e, r.not)
				}
				for _, d := range c.dropped {
					if errors.Is(e, d) {
						t.Errorf("answered %v; want the cancellation's chain dropped, %v not in it", e, d)
					}
				}
				if c.rec.err != nil && c.failure != nil && !strings.Contains(e.Error(), c.rec.err.Error()) {
					t.Errorf("answered %q; want the record's words, %q, in it", e, c.rec.err)
				}
			})
		}
	}
}

func TestNativeAHeldRefusalIsAnsweredByOneVerdictOnEveryArm(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	refused := errors.New("the controls changed")
	for _, entry := range []struct {
		name string
		// op is the command that asked, which its answer names.
		op string
		// refusal is the check after open the entry's admission is: the
		// open's is the first.
		refusal int32
		prepare func(*testing.T, *DSDSink)
		call    func(*DSDSink, context.Context) error
	}{
		{"a resume", "native resume", 3, func(t *testing.T, s *DSDSink) {
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
		}, func(s *DSDSink, ctx context.Context) error { return s.Resume(ctx) }},
		{"a paused activation", "native activation", 2, func(t *testing.T, s *DSDSink) {
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
					namesItsCommand(t, e, entry.op, what)
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
