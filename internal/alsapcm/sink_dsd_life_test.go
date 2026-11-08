package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// lifeSink opens a native sink on k whose life is life, its watcher held
// between the life's end and its revocation until revoke: the moment a
// scheduler can leave between the two (B765). revoking closes once the
// watcher holds there; the test's end releases it too.
func lifeSink(t *testing.T, k *fakeKernel, c DSDConfig, life context.Context) (s *DSDSink, revoking <-chan struct{}, revoke func()) {
	t.Helper()
	return lifeSinkOn(t, k, k, c, life)
}

// lifeSinkOn is lifeSink with the node reached through sy, a seam over k:
// a close that fails, a STATUS held and failed (B728 B9 round 8).
func lifeSinkOn(t *testing.T, k *fakeKernel, sy sys, c DSDConfig, life context.Context) (s *DSDSink, revoking <-chan struct{}, revoke func()) {
	t.Helper()
	in, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	revoke = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(revoke)
	s, e := openDSDSink(life, c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(sy, k.now, ep, c) }, now: k.now, at: k.nativeAt,
		lifeEnded: func() { close(in); <-gate }})
	if e != nil {
		t.Fatal(e)
	}
	return s, in, revoke
}

// statusFault is the fake's node with its next STATUS, once armed, held in
// the call until letGo is closed, then answered err without reaching the
// fake, as a device that fails the STATUS a wait of the writer's ended in
// (B728 B9 round 8): the fake's own failure of a request (fail) answers
// before any hold. failed says that STATUS answered err.
type statusFault struct {
	sys
	err         error
	armed       atomic.Bool
	held, letGo chan struct{}
	failed      atomic.Bool
}

func (f *statusFault) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	if req == ioctlStatus && f.armed.CompareAndSwap(true, false) {
		close(f.held)
		<-f.letGo
		f.failed.Store(true)
		return f.err
	}
	return f.sys.ioctl(fd, req, arg)
}

// theSinksFailure fails the test unless e carries want, a failure the sink
// recorded (its device's, or its cleanup's, ErrReleaseUnconfirmed), and is
// not the ended life's cancellation alone: a caller the life stopped takes
// that for the stop it is, and the failure with it (B728 B9 round 8).
func theSinksFailure(t *testing.T, e, want error, what string) {
	t.Helper()
	if !errors.Is(e, want) || ReasonOf(e) == ReasonCancelled {
		t.Errorf("%s answered %v; want the failure the sink recorded (%v), not the life's cancellation", what, e, want)
	}
}

// noFailure fails the test unless the sink's release recorded no fault and
// its cleanup no failure.
func noFailure(t *testing.T, s *DSDSink, what string) {
	t.Helper()
	if e := s.Result(); e != nil {
		t.Errorf("%s faulted the sink: %v", what, e)
	}
	if e := s.CleanupResult(); e != nil {
		t.Errorf("%s: the release's cleanup failed: %v", what, e)
	}
}

// theLifesCancellation fails the test unless e is the answer of a command
// the sink's ended life fenced or cancelled: that life's cancellation,
// typed as the package's cancellations are, never ErrReleaseUnconfirmed nor
// a refusal of the command's own, which a caller stopped by that life takes
// for a failure (B765 round 3).
func theLifesCancellation(t *testing.T, e error, what string) {
	t.Helper()
	if !errors.Is(e, context.Canceled) || ReasonOf(e) != ReasonCancelled || errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("%s answered %v; want the ended life's cancellation", what, e)
	}
}

// TestNativeTailCancelledByItsEndedLifeIsNoFailure: a sink's life (the
// open's context) is its revocation, and its watcher revokes the sink on
// the goroutine the life's end wakes, which a scheduler can run after the
// writer. Here the writer is held in a write a TrackChange cutoff has
// interrupted while the life ends, its watcher held before the revocation,
// so the writer reaches the tail write with its life ended and no Immediate
// release begun: the cancellation the life makes there is the revocation's,
// expected, and the release confirms with no fault. The harness's Stop ends
// the sink's life (B765: the tail write took that cancellation for a release
// failure, ErrReleaseUnconfirmed, and a Stop's run exited 1).
func TestNativeTailCancelledByItsEndedLifeIsNoFailure(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Tail = time.Millisecond
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	written, write := k.parkNextWrite()
	defer write()
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); e != nil {
		t.Fatal(e)
	}
	await(t, written, "the writer in its first write")
	s.BeginRelease(TrackChange)
	end()
	await(t, revoking, "the life's end, its revocation held")
	write()
	await(t, s.Done(), "the writer's release, its watcher still held")
	noFailure(t, s, "the tail write the ended life cancelled")
	revoke()
	k.checkClosedEach(t)
}

// endingLife is a sink's life a test ends right after one of its reads:
// armed, the next Err reads the life live and ends it before returning, as
// a Stop landing just after that read would.
type endingLife struct {
	context.Context
	end   context.CancelFunc
	armed atomic.Bool
}

func (l *endingLife) Err() error {
	e := l.Context.Err()
	if l.armed.CompareAndSwap(true, false) {
		l.end()
	}
	return e
}

// TestNativeTailWhoseLifeEndsAfterItsReadIsNoFailure: nativeContext decides
// a write's context on one read of the sink's life. The life ending right
// after the tail write's read, its watcher held, leaves the tail write as
// that read decided: it waits on releaseCtx, which the life does not end,
// through the drain, until the revocation cancels it as Immediate's,
// expected, and the release confirms with no fault (B765 round 2: a second
// read saw the end the first had not, cancelled the tail write with the
// release still TrackChange, and the writer recorded ErrReleaseUnconfirmed).
func TestNativeTailWhoseLifeEndsAfterItsReadIsNoFailure(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Tail = time.Millisecond
	ctx, end := context.WithCancel(context.Background())
	defer end()
	life := &endingLife{Context: ctx, end: end}
	s, revoking, revoke := lifeSink(t, k, c, life)
	written, write := k.parkNextWrite()
	defer write()
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); e != nil {
		t.Fatal(e)
	}
	await(t, written, "the writer in its first write")
	s.BeginRelease(TrackChange)
	// The writer's next read of its life is the tail write's: the content
	// write's context was made before it parked in the kernel.
	life.armed.Store(true)
	write()
	await(t, revoking, "the life ended right after the tail write's read, its revocation held")
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	select {
	case <-s.Done():
	case <-k.when(func(k *fakeKernel) bool { return k.state == stateDraining && starved(k) }):
	case <-timer.C:
		LogGoroutines(t)
		t.Fatalf("waited %v for the tail write's release to fail or to drain", *watchdog)
	}
	revoke()
	await(t, s.Done(), "the revocation's release")
	noFailure(t, s, "the tail write whose life ended after its read")
	k.checkClosedEach(t)
}

// TestNativeActivationFencedByItsEndedLifeIsNoFailure: the life ends while
// the activation's admission runs, its watcher held, and the caller waits on
// a context the life does not end (one whose cancellation the scheduler has
// not run), so the sink's reply is its answer. The activation is fenced, and
// that refusal is the revocation's: the writer begins the Immediate release
// before it classifies the error, records no fault, and answers the life's
// cancellation (B765 round 2: it recorded "native activation fenced", which
// the harness's Stop reports as an exit 1; round 3: it still answered that
// fence, which a caller whose select took the reply returned, the same exit
// 1).
func TestNativeActivationFencedByItsEndedLifeIsNoFailure(t *testing.T) {
	k := nativeKernel(t)
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
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 1) }()
	await(t, admitting, "the activation's admission")
	end()
	await(t, revoking, "the life's end, its revocation held")
	admit()
	theLifesCancellation(t, receive(t, answer, "the activation's answer"), "the activation the ended life fenced")
	await(t, s.Done(), "the writer's own release, its watcher still held")
	noFailure(t, s, "the activation the ended life fenced")
	revoke()
	k.checkClosedEach(t)
}

// TestNativeResumeFencedByItsEndedLifeIsNoFailure: a resume of a kernel
// pause, the writer held at its STATUS while the life ends, its watcher
// held and the caller on a context the life does not end, so the sink's
// reply is its answer. The resume is fenced after that STATUS, and the
// refusal is the revocation's, as an activation's is: no fault, and the
// life's cancellation for the answer (B765 round 2: the writer recorded
// "native resume fenced after status", the resume having already marked the
// sink unpaused; round 3: it answered that fence).
func TestNativeResumeFencedByItsEndedLifeIsNoFailure(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	// The pause's own STATUS calls came before it was applied: the next one
	// is the resume's, after its admission.
	statusing, status := k.parkNextStatus()
	defer status()
	answer := make(chan error, 1)
	go func() { answer <- s.Resume(context.Background()) }()
	await(t, statusing, "the resume's STATUS")
	end()
	await(t, revoking, "the life's end, its revocation held")
	status()
	theLifesCancellation(t, receive(t, answer, "the resume's answer"), "the resume the ended life fenced")
	await(t, s.Done(), "the writer's own release, its watcher still held")
	noFailure(t, s, "the resume the ended life fenced")
	revoke()
	k.checkClosedEach(t)
}

// TestNativePrefillCancelledByItsEndedLifeIsNoFailure: the first prefill,
// longer than the buffer, waits for room when the life ends, its watcher
// held. The prefill's context descends from the life, so the activation
// answers with the life's cancellation, a plain one and never
// ErrReleaseUnconfirmed; the writer begins the Immediate release before it
// classifies it, and records no fault (B765 round 2: it recorded that
// cancellation as the sink's fault, which the harness strips: a Stop there
// exited 0 all the same).
func TestNativePrefillCancelledByItsEndedLifeIsNoFailure(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	c.Prefill = 100 * time.Millisecond // past the 40 ms buffer, so it waits for room
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 1) }()
	await(t, k.when(func(k *fakeKernel) bool { return k.state == stateRunning && starved(k) }), "the prefill waiting for room")
	end()
	await(t, revoking, "the life's end, its revocation held")
	if e := receive(t, answer, "the activation's answer"); !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("the prefill's activation answered %v; want the life's plain cancellation", e)
	}
	await(t, s.Done(), "the writer's own release, its watcher still held")
	noFailure(t, s, "the prefill the ended life cancelled")
	revoke()
	k.checkClosedEach(t)
}

// TestNativeReopenWhoseLifeEndsInLocateReleases: a resume after the pause's
// release reopens, and its Locate waits when the life ends, its watcher held.
// The life's end cancels Locate, the resume answers that plain cancellation,
// and the writer, which held a refused reopen paused for the next resume,
// takes the life's end for the revocation it is: it begins the Immediate
// release itself and the sink ends, no fault recorded, without its watcher
// (B765 round 2: the reopen's refusal was held, recording nothing, and the
// sink waited, paused, for the watcher).
func TestNativeReopenWhoseLifeEndsInLocateReleases(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	locating := make(chan struct{})
	c.Locate = func(ctx context.Context) (Endpoint, error) {
		close(locating)
		<-ctx.Done()
		return Endpoint{}, ctx.Err()
	}
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	k.grant(31 * time.Second)
	nativeWait(t, s, "the pause's release", func() bool { return s.released })
	k.revoke()
	answer := make(chan error, 1)
	go func() { answer <- s.Resume(context.Background()) }()
	await(t, locating, "the reopen locating its endpoint")
	end()
	await(t, revoking, "the life's end, its revocation held")
	if e := receive(t, answer, "the resume's answer"); !errors.Is(e, context.Canceled) || errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("the reopen's resume answered %v; want the life's plain cancellation", e)
	}
	await(t, s.Done(), "the writer's own release, its watcher still held")
	noFailure(t, s, "the reopen the ended life cancelled")
	revoke()
	k.checkClosedEach(t)
}

// TestNativeActivationErrorAfterItsLifeEndsIsTheSinksFault: the life ends
// while the activation's admission runs, its watcher held, and the check
// after open then fails for a reason of its own (a control that could not be
// read, a snapshot that changed), which no Stop causes: that failure is the
// sink's fault, recorded and answered, though the ended life began the
// release (B765 round 3: once the life had ended the writer recorded no
// error at all, so a device failing its admission beside a Stop left no
// trace, and the run exited 0).
func TestNativeActivationErrorAfterItsLifeEndsIsTheSinksFault(t *testing.T) {
	k := nativeKernel(t)
	c := nativeConfig()
	failed := errors.New("the controls could not be read: input/output error")
	admitting, gate := make(chan struct{}), make(chan struct{})
	var admitted sync.Once
	admit := func() { admitted.Do(func() { close(gate) }) }
	defer admit()
	var checks atomic.Int32
	c.Check = func(NodeInfo) error {
		if checks.Add(1) == 2 { // the open's check, then the activation's
			close(admitting)
			<-gate
			return failed
		}
		return nil
	}
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, c, life)
	answer := make(chan error, 1)
	go func() { answer <- s.Activate(context.Background(), 1) }()
	await(t, admitting, "the activation's admission")
	end()
	await(t, revoking, "the life's end, its revocation held")
	admit()
	if e := receive(t, answer, "the activation's answer"); !errors.Is(e, failed) {
		t.Errorf("the activation answered %v; want its admission's failure", e)
	}
	await(t, s.Done(), "the writer's own release, its watcher still held")
	if e := s.Result(); !errors.Is(e, failed) {
		t.Errorf("the sink's outcome is %v; want the admission's failure, its fault", e)
	}
	if e := s.CleanupResult(); e != nil {
		t.Errorf("the release's cleanup failed: %v", e)
	}
	revoke()
	k.checkClosedEach(t)
}

// TestNativeResumeErrorAfterItsLifeEndsIsTheSinksFault: a resume of a kernel
// pause passes its fence on a read of the life a Stop ends right after (its
// watcher held), and its STATUS then fails with EIO, the device's own
// failure: the sink's fault, recorded and answered, though the ended life
// began the release (B765 round 3, Codex's example: the writer recorded no
// error once the life had ended).
func TestNativeResumeErrorAfterItsLifeEndsIsTheSinksFault(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	k := nativeKernel(t)
	ctx, end := context.WithCancel(context.Background())
	defer end()
	life := &endingLife{Context: ctx, end: end}
	s, revoking, revoke := lifeSink(t, k, nativeConfig(), life)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if e := s.Pause(context.Background()); e != nil {
		t.Fatal(e)
	}
	// The pause's own STATUS calls came before it was applied: the next is
	// the resume's, after its fence, the writer's next read of the life.
	k.inspect(func(k *fakeKernel) { k.fail[ioctlStatus] = eio })
	life.armed.Store(true)
	answer := make(chan error, 1)
	go func() { answer <- s.Resume(context.Background()) }()
	await(t, revoking, "the life ended right after the resume's fence read it, its revocation held")
	if e := receive(t, answer, "the resume's answer"); !errors.Is(e, eio) {
		t.Errorf("the resume answered %v; want its STATUS's EIO", e)
	}
	await(t, s.Done(), "the writer's own release, its watcher still held")
	if e := s.Result(); !errors.Is(e, eio) {
		t.Errorf("the sink's outcome is %v; want the STATUS's EIO, its fault", e)
	}
	if e := s.CleanupResult(); e != nil {
		t.Errorf("the release's cleanup failed: %v", e)
	}
	revoke()
	k.checkClosedEach(t)
}

// TestNativeResumeTheEndedLifeNeverRanAnswersItsCancellation: a resume whose
// request the writer has not taken when the life ends and its watcher
// revokes the sink (the writer held in its pause's watch): the writer
// releases without running it and answers it the life's cancellation, which
// the caller returns whichever arm of its select takes it (B765 round 3: it
// answered "native activation cancelled", which a harness stopped by that
// life reports as an exit 1).
func TestNativeResumeTheEndedLifeNeverRanAnswersItsCancellation(t *testing.T) {
	k := nativeKernel(t)
	life, end := context.WithCancel(context.Background())
	defer end()
	s, revoking, revoke := lifeSink(t, k, nativeConfig(), life)
	if e := s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	// The pause's first STATUS: the writer's next poll is then its pause's
	// watch.
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
	go func() { answer <- s.Resume(context.Background()) }()
	nativeWait(t, s, "the resume's request", func() bool { return s.activation != nil })
	end()
	await(t, revoking, "the life's end")
	revoke()
	nativeWait(t, s, "the revocation", func() bool { return s.releasing })
	poll()
	theLifesCancellation(t, receive(t, answer, "the resume's answer"), "the resume the ended life never ran")
	await(t, s.Done(), "the revocation's release")
	noFailure(t, s, "the resume the ended life never ran")
	k.checkClosedEach(t)
}

// endedRequest is how a resume the ended life never ran is answered (B728
// B9 round 8): by the writer's reply, its caller on a context the life does
// not end, or by its caller's own context's arm, that caller on the life
// itself, as the harness's is.
var endedRequest = []struct {
	name   string
	onLife bool
}{{"through its reply", false}, {"through its caller's context", true}}

// queueResume asks s to resume, its caller on the life where onLife, and
// waits for the request to be the writer's to answer.
func queueResume(t *testing.T, s *DSDSink, life context.Context, onLife bool) <-chan error {
	t.Helper()
	ctx := context.Background()
	if onLife {
		ctx = life
	}
	answer := make(chan error, 1)
	go func() { answer <- s.Resume(ctx) }()
	nativeWait(t, s, "the resume's request", func() bool { return s.activation != nil })
	return answer
}

// revokeEnded begins the release of the sink whose life has ended, its
// watcher held at revoking: where onLife the resume's caller does, its own
// context's arm, the writer being held in a call and the watcher before its
// revocation, and otherwise the watcher, let go.
func revokeEnded(t *testing.T, s *DSDSink, revoking <-chan struct{}, revoke func(), onLife bool) {
	t.Helper()
	await(t, revoking, "the life's end, its revocation held")
	if onLife {
		nativeWait(t, s, "the resume's caller revoking the sink", func() bool { return s.releasing })
		return
	}
	revoke()
	nativeWait(t, s, "the revocation", func() bool { return s.releasing })
}

// answeredBy fails the test unless e came the way it was asked: the reply
// carries no cancellation once the sink recorded a failure, and the caller's
// own context's arm joins that failure to its context's end.
func answeredBy(t *testing.T, e error, onLife bool, what string) {
	t.Helper()
	if errors.Is(e, context.Canceled) != onLife {
		t.Errorf("%s answered %v: want it answered by its caller's context %v", what, e, onLife)
	}
}

// TestNativeResumeTheEndedLifeNeverRanAnswersAFailedClose (B728 B9 round 8):
// the resume of TestNativeResumeTheEndedLifeNeverRanAnswersItsCancellation,
// asked while the writer sits in its pause's watch and never run, its life
// ended, but the node's close at the writer's end fails, the device still
// open until then: that release cannot be confirmed (ErrReleaseUnconfirmed),
// and the resume answers it, never the life's cancellation alone, through
// the writer's reply and through its caller's own context's arm alike, which
// waits for the writer's end once the life has ended. Until round 8 the reply
// was the life's cancellation (fenced), and the context's arm answered that
// cancellation before the close.
func TestNativeResumeTheEndedLifeNeverRanAnswersAFailedClose(t *testing.T) {
	for _, asked := range endedRequest {
		t.Run(asked.name, func(t *testing.T) {
			k := nativeKernel(t)
			a := &nativeAuditSys{sys: k, t: t, closeErr: errors.New("the node's close failed")}
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, revoke := lifeSinkOn(t, k, a, nativeConfig(), life)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
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
			answer := queueResume(t, s, life, asked.onLife)
			end()
			revokeEnded(t, s, revoking, revoke, asked.onLife)
			var open bool
			var closes int
			k.inspect(func(k *fakeKernel) { open, closes = k.isOpen, k.closes })
			if !open || closes != 0 {
				t.Fatalf("the node before the writer's end: open %v, closed %d times; want it open, for the writer's end to close", open, closes)
			}
			poll()
			e := receive(t, answer, "the resume's answer")
			theSinksFailure(t, e, ErrReleaseUnconfirmed, "the resume the ended life never ran, its close failing")
			answeredBy(t, e, asked.onLife, "the resume")
			await(t, s.Done(), "the revocation's release")
			revoke()
			k.inspect(func(k *fakeKernel) { closes = k.closes })
			if closes != 1 || !errors.Is(s.CleanupResult(), ErrReleaseUnconfirmed) {
				t.Errorf("the node closed %d times, the cleanup %v: want the writer's end's one close, unconfirmed", closes, s.CleanupResult())
			}
			k.checkClosedEach(t)
		})
	}
}

// TestNativeResumeTheEndedLifeNeverRanAnswersTheDevicesFailure (B728 B9 round
// 8): the writer in its pause's watch, past a poll that ended, is held in that
// watch's STATUS while a resume is asked and the life ends, its watcher held;
// the STATUS then answers EIO, the device's own failure, which the writer
// records before it ends. The resume, never run, answers that EIO, never the
// life's cancellation alone, through the writer's reply and through its
// caller's own context's arm alike. Until round 8 the reply was the life's
// cancellation (fenced), and the context's arm answered that cancellation
// while the STATUS was still held.
func TestNativeResumeTheEndedLifeNeverRanAnswersTheDevicesFailure(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	for _, asked := range endedRequest {
		t.Run(asked.name, func(t *testing.T) {
			k := nativeKernel(t)
			f := &statusFault{sys: k, err: eio, held: make(chan struct{}), letGo: make(chan struct{})}
			var once sync.Once
			letGo := func() { once.Do(func() { close(f.letGo) }) }
			defer letGo()
			life, end := context.WithCancel(context.Background())
			defer end()
			s, revoking, revoke := lifeSinkOn(t, k, f, nativeConfig(), life)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(context.Background()); e != nil {
				t.Fatal(e)
			}
			// The pause applied, its STATUS calls made: the writer's next STATUS
			// is its watch's, once the poll before it ends, which the period
			// granted here does.
			f.armed.Store(true)
			k.grant(s.Params().PeriodTime())
			await(t, f.held, "the pause watch's STATUS, its poll ended")
			answer := queueResume(t, s, life, asked.onLife)
			end()
			revokeEnded(t, s, revoking, revoke, asked.onLife)
			letGo()
			e := receive(t, answer, "the resume's answer")
			theSinksFailure(t, e, eio, "the resume the ended life never ran, its STATUS failing")
			answeredBy(t, e, asked.onLife, "the resume")
			await(t, s.Done(), "the writer's release")
			revoke()
			if !f.failed.Load() {
				t.Error("the pause watch's STATUS never answered EIO")
			}
			if e := s.Result(); !errors.Is(e, eio) {
				t.Errorf("the sink's outcome is %v; want the STATUS's EIO, its fault", e)
			}
			if e := s.CleanupResult(); e != nil {
				t.Errorf("the release's cleanup failed: %v", e)
			}
			k.checkClosedEach(t)
		})
	}
}

// TestNativeCommandsAfterTheLifeEndsAnswerItsCancellation: a command the
// sink's ended life fences before the writer sees it, its caller's own check
// of its context having passed just before the life ended (a context the
// life does not end, here): Activate, Resume, ActivatePaused, Pause and
// WriteDSD each answer the life's cancellation, never a refusal of their own
// (B765 round 3: "native activation fenced", "native resume fenced",
// "native initial paused activation fenced", "native releasing" and "native
// admission closed". The harness's console cancels the run on an s at once
// while a space read just before still reaches Pause or Resume, and the
// feeder's WriteDSD reads its context before it takes the sink's lock, so a
// Stop typed beside a command exited 1). The same commands after a failure
// the sink recorded, a device's EIO the writer met before the life ended or
// the release's close failing once it had, answer that failure joined to
// their refusal, never the life's cancellation alone (B728 B9 round 8: they
// answered the cancellation, the failure dropped).
func TestNativeCommandsAfterTheLifeEndsAnswerItsCancellation(t *testing.T) {
	const eio errno = 5 // EIO, asm-generic's: an errno this package does not tell apart
	activate := func(t *testing.T, s *DSDSink) {
		t.Helper()
		if e := s.Activate(context.Background(), 1); e != nil {
			t.Fatal(e)
		}
	}
	pause := func(t *testing.T, s *DSDSink) {
		t.Helper()
		if e := s.Pause(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	// failStatus makes every STATUS answer EIO from now on.
	failStatus := func(k *fakeKernel) { k.inspect(func(k *fakeKernel) { k.fail[ioctlStatus] = eio }) }
	// watchFails ends a writer that watches its stream on the device's EIO:
	// its next STATUS fails, and the period granted ends its poll before it.
	watchFails := func(t *testing.T, k *fakeKernel, s *DSDSink) {
		failStatus(k)
		k.grant(s.Params().PeriodTime())
	}
	for _, command := range []struct {
		name    string
		prepare []func(*testing.T, *DSDSink)
		// fail has the writer meet the device's EIO, after prepare.
		fail func(*testing.T, *fakeKernel, *DSDSink)
		call func(*DSDSink) error
	}{
		// An inert sink's writer makes no call until it is paused, whose
		// STATUS then fails.
		{"activate", nil, func(t *testing.T, k *fakeKernel, s *DSDSink) { failStatus(k); pause(t, s) },
			func(s *DSDSink) error { return s.Activate(context.Background(), 1) }},
		{"resume", []func(*testing.T, *DSDSink){activate, pause}, watchFails, func(s *DSDSink) error { return s.Resume(context.Background()) }},
		{"paused activation", []func(*testing.T, *DSDSink){pause}, watchFails, func(s *DSDSink) error { return s.ActivatePaused(context.Background()) }},
		{"pause", []func(*testing.T, *DSDSink){activate}, watchFails, func(s *DSDSink) error { return s.Pause(context.Background()) }},
		{"write", []func(*testing.T, *DSDSink){activate}, watchFails, func(s *DSDSink) error {
			_, e := s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32)
			return e
		}},
	} {
		for _, fault := range []string{"", "a device's failure", "a failed cleanup"} {
			name := command.name
			if fault != "" {
				name += " after " + fault
			}
			t.Run(name, func(t *testing.T) {
				k := nativeKernel(t)
				var node sys = k
				if fault == "a failed cleanup" {
					node = &nativeAuditSys{sys: k, t: t, closeErr: errors.New("the node's close failed")}
				}
				life, end := context.WithCancel(context.Background())
				defer end()
				s, revoking, revoke := lifeSinkOn(t, k, node, nativeConfig(), life)
				for _, prepare := range command.prepare {
					prepare(t, s)
				}
				if fault == "a device's failure" {
					// The writer ends on the failure first, so its watcher ends
					// with it, and the life's end then revokes nothing.
					command.fail(t, k, s)
					await(t, s.Done(), "the writer's end on the device's EIO")
					end()
				} else {
					end()
					await(t, revoking, "the life's end")
					revoke()
					await(t, s.Done(), "the revocation's release")
				}
				e := command.call(s)
				what := "the " + command.name + " after the sink's life ended"
				switch fault {
				case "":
					theLifesCancellation(t, e, what)
					noFailure(t, s, "the revocation")
				case "a device's failure":
					theSinksFailure(t, e, eio, what+", the device's EIO recorded")
					if r := s.Result(); !errors.Is(r, eio) {
						t.Errorf("the sink's outcome is %v; want the device's EIO", r)
					}
				default:
					theSinksFailure(t, e, ErrReleaseUnconfirmed, what+", its release's close failed")
					if r := s.CleanupResult(); !errors.Is(r, ErrReleaseUnconfirmed) {
						t.Errorf("the release's cleanup is %v; want it unconfirmed", r)
					}
				}
				k.checkClosedEach(t)
			})
		}
	}
}
