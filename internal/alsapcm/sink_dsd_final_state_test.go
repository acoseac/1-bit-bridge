package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// signal is a hook that marks each call on ch without waiting.
func signal(ch chan struct{}) func() {
	return func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// TestNativeAnAnswerIsReadAfterItsWait: a command that meets a release under
// way, the sink's life running, waits for its outcome (releaseOutcome, held
// there by the meeting seam); the life ends while it waits, and the release's
// close then completes, clean, with nothing recorded or with a device's fault
// recorded beside it. The answer is the cleanup's verdict of that final state:
// the ended life's typed cancellation, or the device's fault, never the
// refusal, the fence or the success read before the wait (B728 B9 PR 1 round
// 4). A write of no bits whose cleanup recorded nothing is the success it was.
func TestNativeAnAnswerIsReadAfterItsWait(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	ended, stop := context.WithCancel(context.Background())
	stop()
	for _, entry := range []struct {
		name, op string
		success  bool // a success, where nothing was recorded
		call     func(*DSDSink) error
	}{
		{"Activate refused at its entry", "native activation", false, func(s *DSDSink) error { return s.Activate(context.Background(), 1) }},
		// Its own context ended: it revokes, and meets the release begun before.
		{"Activate on a context already ended", "native activation", false, func(s *DSDSink) error { return s.Activate(ended, 1) }},
		{"Pause refused at its entry", "native pause", false, func(s *DSDSink) error { return s.Pause(context.Background()) }},
		{"WriteDSD of no bits", "native write", true, func(s *DSDSink) error {
			_, err := s.WriteDSD(context.Background(), 1, nil, nil, 0, 0)
			return err
		}},
	} {
		for _, faulted := range []bool{false, true} {
			name := entry.name + ", nothing recorded"
			if faulted {
				name = entry.name + ", a device's fault recorded"
			}
			t.Run(name, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				hold := newCallerHold(t)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, func(h *nativeSeams) { h.meeting = hold.awaiting })
				// The session's release, its close held: under way.
				f.hold.Store(true)
				s.BeginRelease(Immediate)
				await(t, f.held, "the release's close, held")
				hold.armed.Store(true)
				answer := make(chan error, 1)
				go func() { answer <- entry.call(s) }()
				await(t, hold.in, "the entry waiting for the release's outcome, its life running")
				end()
				await(t, revoking, "the life's end, its revocation held")
				if faulted {
					s.mu.Lock()
					s.err = errors.Join(s.err, eio)
					s.mu.Unlock()
				}
				f.release()
				await(t, s.Done(), "the release, its close clean")
				hold.release()
				e := receive(t, answer, "the entry's answer")
				switch {
				case faulted:
					theSinksFailure(t, e, eio, name)
					if errors.Is(e, ErrReleaseUnconfirmed) {
						t.Errorf("%s answered %v; the release was confirmed, want no ErrReleaseUnconfirmed", name, e)
					}
				case entry.success:
					if e != nil {
						t.Errorf("%s answered %v; want the success it was, its cleanup clean", name, e)
					}
				default:
					theLifesCancellation(t, e, name)
					var x *Error
					if !errors.As(e, &x) || x.Op != entry.op || errors.Is(e, ErrActivationFenced) {
						t.Errorf("%s answered %v; want the ended life's cancellation of %s, not the sink's fence", name, e, entry.op)
					}
				}
				revoke()
				k.checkClosedEach(t)
			})
		}
	}
}

// TestNativeAWaitsLastArmsAnswerFromTheFinalState: an activation's caller
// takes its wait's deadline arm, the release's budget passed, or its done
// arm, the writer ended, each with the sink's life read running; the life then
// ends before the answer is read (the overBudget seam, or the meeting seam
// where the done arm settles), and the release's close completes, clean, with
// nothing recorded or a device's fault beside it. The answer is the cleanup's
// verdict of the writer's reply, the fence the release made of the
// activation: the ended life's typed cancellation, or the fault (B728 B9 PR 1
// round 4: the deadline arm answered the fence once the life had ended).
func TestNativeAWaitsLastArmsAnswerFromTheFinalState(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	for _, arm := range []awaitArm{armDeadline, armDone} {
		for _, faulted := range []bool{false, true} {
			name := armName[arm] + ", nothing recorded"
			if faulted {
				name = armName[arm] + ", a device's fault recorded"
			}
			t.Run(name, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				f.hold.Store(arm == armDeadline)
				c := nativeConfig()
				c.Prefill = 100 * time.Millisecond // past the 40 ms buffer, so it waits for room
				pick := &armPick{}
				hold := newCallerHold(t)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, f, c, life, pick.hook, func(h *nativeSeams) {
					if arm == armDeadline {
						h.overBudget = hold.awaiting
					} else {
						h.meeting = hold.awaiting
					}
				})
				pick.set(arm)
				answer := make(chan error, 1)
				go func() { answer <- s.Activate(context.Background(), 1) }()
				await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "the prefill waiting for room")
				hold.armed.Store(true)
				s.BeginRelease(Immediate)
				if arm == armDeadline {
					await(t, f.held, "the release's close, held")
					pastBudget(t, k, s)
				}
				await(t, hold.in, "the caller in its arm, the life read running, its answer not yet read")
				end()
				if arm == armDeadline {
					// The writer still in its close: the watcher takes the
					// life's end. Done, the writer may have ended it first.
					await(t, revoking, "the life's end, its revocation held")
				}
				if faulted {
					s.mu.Lock()
					s.err = errors.Join(s.err, eio)
					s.mu.Unlock()
				}
				f.release()
				await(t, s.Done(), "the release, its close clean")
				hold.release()
				e := receive(t, answer, "the activation's answer")
				if faulted {
					theSinksFailure(t, e, eio, name)
					if errors.Is(e, ErrReleaseUnconfirmed) {
						t.Errorf("%s answered %v; the release was confirmed, want no ErrReleaseUnconfirmed", name, e)
					}
				} else {
					theLifesCancellation(t, e, name)
					var x *Error
					if !errors.As(e, &x) || x.Op != "native activation" || errors.Is(e, ErrActivationFenced) {
						t.Errorf("%s answered %v; want the ended life's cancellation of the activation, not the sink's fence", name, e)
					}
				}
				revoke()
				k.checkClosedEach(t)
			})
		}
	}
}

// TestNativeARevocationKnowsTheReleaseBegunBeforeIt: a command whose own
// context has ended revokes the sink, and decides with that revocation, in
// one hold of the sink's lock, whether the release had begun (revokeFound): a
// TrackChange another party begins just before (the revoking seam) is the
// release the command meets, so it waits for its outcome within the budget,
// which passes with its close held, and answers the release's unconfirmed
// failure (B728 B9 PR 1 round 4: read apart from the revocation, a release
// begun between the two was the command's own, answered at once with bare
// cancellation). An activation refused at its entry, and a resume's caller in
// its wait.
func TestNativeARevocationKnowsTheReleaseBegunBeforeIt(t *testing.T) {
	for _, command := range []string{"an activation on a context already ended", "a resume whose caller's context ends"} {
		t.Run(command, func(t *testing.T) {
			k := nativeKernel(t)
			f := newCleanupFault(t, k)
			f.failing.Store(true)
			f.hold.Store(true)
			meeting, began := make(chan struct{}, 1), make(chan struct{})
			pick := &armPick{}
			life, end := context.WithCancel(context.Background())
			defer end()
			var s *DSDSink
			s, _, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, pick.hook, func(h *nativeSeams) {
				h.revoking = func() { s.BeginRelease(TrackChange); close(began) }
				h.meeting = signal(meeting)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var answer <-chan error
			poll := func() {}
			if command == "a resume whose caller's context ends" {
				if e := s.Activate(context.Background(), 1); e != nil {
					t.Fatal(e)
				}
				// The pause's first STATUS: the writer's next poll is then its
				// pause's watch, parked, so the resume stays the writer's to take.
				statusing, status := k.parkNextStatus()
				defer status()
				paused := make(chan error, 1)
				go func() { paused <- s.Pause(context.Background()) }()
				await(t, statusing, "the pause's STATUS")
				var polling <-chan struct{}
				polling, poll = k.parkNextPoll()
				defer poll()
				status()
				if e := receive(t, paused, "the pause"); e != nil {
					t.Fatal(e)
				}
				await(t, polling, "the writer in its pause's watch")
				pick.set(armContext)
				answer = queueResume(t, s, ctx, true)
				cancel()
			} else {
				cancel()
				activated := make(chan error, 1)
				go func() { activated <- s.Activate(ctx, 1) }()
				answer = activated
			}
			await(t, began, "a release begun just before the command's revocation")
			receive(t, meeting, "the command waiting for that release's outcome")
			poll()
			await(t, f.held, "that release's close, held")
			pastBudget(t, k, s)
			e := receive(t, answer, "the command's answer, the release past its budget")
			theSinksFailure(t, e, ErrReleaseUnconfirmed, command+", a release begun before its revocation")
			f.release()
			await(t, s.Done(), "the writer's late release")
			if !errors.Is(s.CleanupResult(), f.err) {
				t.Errorf("the cleanup is %v; want the late close's failure", s.CleanupResult())
			}
			revoke()
			k.checkClosedEach(t)
		})
	}
}

// TestNativeAWriteOrAPauseStoppedByItsOwnContextAnswersAtOnce: a release
// under way, its close held and its deadline not passed, the sink's life
// running: a WriteDSD and a Pause whose own context ends, before the call or
// while it waits for that release, answer that cancellation at once, waiting
// for no release and revoking nothing, the close still held when they answer
// (B728 B9 PR 1 round 4: they waited for the release, as a command that meets
// one does, so playback's producer, stopped once a release began, waited out
// that release). The watchdog catches a hang; nothing is timed.
func TestNativeAWriteOrAPauseStoppedByItsOwnContextAnswersAtOnce(t *testing.T) {
	for _, command := range []struct {
		name string
		call func(context.Context, *DSDSink) error
	}{
		{"WriteDSD", func(ctx context.Context, s *DSDSink) error {
			_, err := s.WriteDSD(ctx, 1, []byte{1}, []byte{2}, 0, 8)
			return err
		}},
		{"Pause", func(ctx context.Context, s *DSDSink) error { return s.Pause(ctx) }},
	} {
		for _, when := range []string{"before the call", "while it waits"} {
			t.Run(command.name+", its context ended "+when, func(t *testing.T) {
				k := nativeKernel(t)
				f := newCleanupFault(t, k)
				meeting := make(chan struct{}, 1)
				life, end := context.WithCancel(context.Background())
				defer end()
				s, _, revoke := lifeSinkOn(t, k, f, nativeConfig(), life, func(h *nativeSeams) { h.meeting = signal(meeting) })
				f.hold.Store(true)
				s.BeginRelease(Immediate)
				await(t, f.held, "the release's close, held")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if when == "before the call" {
					cancel()
				}
				answer := make(chan error, 1)
				go func() { answer <- command.call(ctx, s) }()
				if when == "while it waits" {
					receive(t, meeting, "the command waiting for the release's outcome")
					cancel()
				}
				e := receive(t, answer, "its answer, the close held and the budget not passed")
				if !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, ErrActivationFenced) {
					t.Errorf("%s answered %v; want its own context's cancellation", command.name, e)
				}
				select {
				case <-s.Done():
					t.Error("the release ended before the answer; the premise is a release still under way")
				default:
				}
				f.release()
				await(t, s.Done(), "the release")
				revoke()
				k.checkClosedEach(t)
			})
		}
	}
}

// TestNativeAWriteOfNoBitsChecksItsContext: a write of no bits skips the
// write's loop, and with nothing under way and the life running it answers
// its own context's end all the same, and the success it is on a context
// running (B728 B9 PR 1 round 4: it answered nil on an ended context).
func TestNativeAWriteOfNoBitsChecksItsContext(t *testing.T) {
	k := nativeKernel(t)
	s := nativeSink(t, k, nativeConfig())
	ended, stop := context.WithCancel(context.Background())
	stop()
	if _, e := s.WriteDSD(ended, 1, nil, nil, 0, 0); !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("a write of no bits on its own context ended answered %v; want that cancellation", e)
	}
	if _, e := s.WriteDSD(context.Background(), 1, nil, nil, 0, 0); e != nil {
		t.Errorf("a write of no bits answered %v; want the success it is", e)
	}
	s.Revoke()
	await(t, s.Done(), "the sink's release")
	k.checkClosedEach(t)
}

// TestNativeAWriteStoppedOnceATrackChangeBeganKeepsItsPrefixAndTail: the
// session begins a TrackChange release, its tail held in the kernel's poll,
// and ends its producer's context, as playback's release does: the
// producer's WriteDSD, stopped before it is called or while it waits for that
// release, answers the cancellation at once, revokes nothing and escalates
// nothing, so the track change keeps its accepted prefix and plays its whole
// tail (B728 B9 PR 1 round 4).
func TestNativeAWriteStoppedOnceATrackChangeBeganKeepsItsPrefixAndTail(t *testing.T) {
	prefix := []byte{1, 2, 3, 4, 5, 6, 7, 8} // one DSD_U32_BE frame: left's word, then right's
	for _, when := range []string{"before the call", "while it waits"} {
		t.Run("its context ended "+when, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			c.Tail = time.Millisecond
			g := &nativePollGate{sys: k, entered: make(chan struct{}), gate: make(chan struct{})}
			meeting := make(chan struct{}, 1)
			s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(g, k.now, ep, c) },
				now: k.now, at: k.nativeAt, meeting: signal(meeting)})
			if e != nil {
				t.Fatal(e)
			}
			defer s.Release(context.Background(), Immediate)
			if e = s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if _, e = s.WriteDSD(context.Background(), 1, prefix[:4], prefix[4:], 0, 32); e != nil {
				t.Fatal(e)
			}
			await(t, k.when(filledAndIdle(1)), "the accepted prefix written")
			k.inspect(func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoEAGAIN}} })
			g.armed.Store(true)
			s.BeginRelease(TrackChange)
			await(t, g.entered, "the tail, held in its poll")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if when == "before the call" {
				cancel()
			}
			answer := make(chan error, 1)
			go func() {
				_, err := s.WriteDSD(ctx, 1, []byte{9}, []byte{9}, 0, 8)
				answer <- err
			}()
			if when == "while it waits" {
				receive(t, meeting, "the producer waiting for the track change's outcome")
				cancel()
			}
			if e := receive(t, answer, "the producer's answer, the tail held"); !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) {
				t.Errorf("the producer answered %v; want its own context's cancellation", e)
			}
			if s.immediate() {
				t.Error("the producer's stop escalated the track change to Immediate")
			}
			k.free()
			close(g.gate)
			if e := s.Release(context.Background(), TrackChange); e != nil {
				t.Fatal(e)
			}
			if want := append(append([]byte(nil), prefix...), bytes.Repeat([]byte{0x69}, 89*8)...); !bytes.Equal(given(k), want) {
				t.Fatalf("the kernel was given %d bytes; want the accepted prefix and the whole tail, %d", len(given(k)), len(want))
			}
		})
	}
}

// TestTheVerdictOfALiveLifeNeedsNoLifeEnd: while the sink's life runs, an
// answer is the verdict's only where the release failed or is unconfirmed
// (running), and the verdict then attaches ErrReleaseUnconfirmed to every
// answer of every kind, never reaching the life's typed cancellation, whose
// end (rec.end) is nil; a record that has not failed leaves the answer as it
// was (B728 B9 PR 1 round 4).
func TestTheVerdictOfALiveLifeNeedsNoLifeEnd(t *testing.T) {
	const eio errno = 5
	drain := errors.Join(ErrReleaseUnconfirmed, &Error{Reason: ReasonCancelled, Op: "drain", Err: context.Canceled})
	own := errors.New("the request's own failure")
	answers := []nativeAnswer{
		{kind: answerNone, op: "native resume"},
		{kind: answerSuccess, op: "native write"},
		{kind: answerFault, err: own, op: "native activation"},
		{kind: answerHeld, err: own, op: "native resume"},
		refusedAnswer("native pause", fmt.Errorf("%w: refused", ErrActivationFenced)),
		refusedAnswer("native write", context.Canceled),
		{kind: answerNeverRan, err: errNativeActivationCancelled, op: "native activation"},
	}
	failed := []nativeRecord{
		{unconfirmed: true},
		{unconfirmed: true, err: eio},
		{ended: true, err: drain},
		{err: drain},
		{ended: true, err: errors.Join(ErrReleaseUnconfirmed, eio)},
	}
	for i, rec := range failed {
		if !rec.failed() {
			t.Fatalf("record %d did not fail; the premise is a failed or unconfirmed release", i)
		}
		for _, a := range answers {
			e := running(a, rec)
			if !errors.Is(e, ErrReleaseUnconfirmed) || errors.Is(e, context.Canceled) || ReasonOf(e) == ReasonCancelled || cancelledIn(e) || errors.Is(e, ErrActivationFenced) {
				t.Errorf("record %d, answer kind %d: %v; want ErrReleaseUnconfirmed, no cancellation and no fence", i, a.kind, e)
			}
		}
	}
	for i, rec := range []nativeRecord{{}, {ended: true}, {ended: true, err: eio}, {err: eio}} {
		for _, a := range answers {
			if e := running(a, rec); e != a.err {
				t.Errorf("record %d, answer kind %d: %v; a release that has not failed leaves the answer %v", i, a.kind, e, a.err)
			}
		}
	}
}

// TestNativeATrackChangeItsDeviceFailedIsACleanupFailure: a TrackChange whose
// tail write or drain the device fails records ErrReleaseUnconfirmed beside
// the device's error, so a command answered while the life runs answers that
// cleanup failure, whichever its return (B728 B9 PR 1 round 4).
func TestNativeATrackChangeItsDeviceFailedIsACleanupFailure(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	ended, stop := context.WithCancel(context.Background())
	stop()
	for _, where := range []string{"its tail", "its drain"} {
		t.Run(where, func(t *testing.T) {
			k := nativeKernel(t)
			c := nativeConfig()
			if where == "its tail" {
				c.Tail = time.Millisecond
			}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, _, revoke := lifeSink(t, k, c, life)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if _, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); e != nil {
				t.Fatal(e)
			}
			await(t, k.when(filledAndIdle(1)), "the accepted prefix written")
			k.inspect(func(k *fakeKernel) {
				if where == "its tail" {
					k.writes = []fakeWrite{{err: eio}}
				} else {
					k.fail[ioctlDrain] = eio
				}
			})
			s.BeginRelease(TrackChange)
			await(t, s.Done(), "the track change its device failed")
			if rec := s.Result(); !errors.Is(rec, ErrReleaseUnconfirmed) || !errors.Is(rec, eio) {
				t.Fatalf("the sink recorded %v; want the device's failure beside ErrReleaseUnconfirmed", rec)
			}
			for _, command := range []struct {
				name string
				call func() error
			}{
				{"a pause", func() error { return s.Pause(context.Background()) }},
				{"a write", func() error {
					_, err := s.WriteDSD(context.Background(), 1, []byte{1}, []byte{2}, 0, 8)
					return err
				}},
				{"a write on its own context ended", func() error {
					_, err := s.WriteDSD(ended, 1, []byte{1}, []byte{2}, 0, 8)
					return err
				}},
				{"an activation", func() error { return s.Activate(context.Background(), 2) }},
				{"a resume", func() error { return s.Resume(context.Background()) }},
			} {
				e := command.call()
				theSinksFailure(t, e, ErrReleaseUnconfirmed, command.name+" after "+where+" failed")
				if !errors.Is(e, eio) {
					t.Errorf("%s after %s failed answered %v; want the device's failure", command.name, where, e)
				}
			}
			if life.Err() != nil {
				t.Fatal("the sink's life ended; the premise is a life still running")
			}
			revoke()
			k.checkClosedEach(t)
		})
	}
}
