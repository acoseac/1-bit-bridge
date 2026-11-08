package alsapcm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lifeSink opens a native sink on k whose life is life, its watcher held
// between the life's end and its revocation until revoke: the moment a
// scheduler can leave between the two (B765). revoking closes once the
// watcher holds there; the test's end releases it too.
func lifeSink(t *testing.T, k *fakeKernel, c DSDConfig, life context.Context) (s *DSDSink, revoking <-chan struct{}, revoke func()) {
	t.Helper()
	in, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	revoke = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(revoke)
	s, e := openDSDSink(life, c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt,
		lifeEnded: func() { close(in); <-gate }})
	if e != nil {
		t.Fatal(e)
	}
	return s, in, revoke
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
// not run). The activation is fenced, and that refusal is the revocation's:
// the writer begins the Immediate release before it classifies the error,
// and records no fault (B765 round 2: it recorded "native activation
// fenced", which the harness's Stop reports as an exit 1).
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
	if e := receive(t, answer, "the activation's answer"); e == nil {
		t.Fatal("an activation whose life ended was admitted")
	}
	await(t, s.Done(), "the writer's own release, its watcher still held")
	noFailure(t, s, "the activation the ended life fenced")
	revoke()
	k.checkClosedEach(t)
}

// TestNativeResumeFencedByItsEndedLifeIsNoFailure: a resume of a kernel
// pause, the writer held at its STATUS while the life ends, its watcher
// held and the caller on a context the life does not end. The resume is
// fenced after that STATUS, and the refusal is the revocation's, as an
// activation's is (B765 round 2: the writer recorded "native resume fenced
// after status", the resume having already marked the sink unpaused).
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
	if e := receive(t, answer, "the resume's answer"); e == nil {
		t.Fatal("a resume whose life ended went on")
	}
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
