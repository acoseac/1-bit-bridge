package alsapcm

import (
	"context"
	"testing"
	"time"
)

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
	revoking, revoke := make(chan struct{}), make(chan struct{})
	defer close(revoke)
	s, e := openDSDSink(life, c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt,
		lifeEnded: func() { close(revoking); <-revoke }})
	if e != nil {
		t.Fatal(e)
	}
	written, write := k.parkNextWrite()
	defer write()
	if e = s.Activate(context.Background(), 1); e != nil {
		t.Fatal(e)
	}
	if _, e = s.WriteDSD(context.Background(), 1, []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}, 0, 32); e != nil {
		t.Fatal(e)
	}
	await(t, written, "the writer in its first write")
	s.BeginRelease(TrackChange)
	end()
	await(t, revoking, "the life's end, its revocation held")
	write()
	await(t, s.Done(), "the writer's release, its watcher still held")
	if e = s.Result(); e != nil {
		t.Errorf("the tail write the ended life cancelled faulted the release: %v", e)
	}
	if e = s.CleanupResult(); e != nil {
		t.Errorf("the release's cleanup failed: %v", e)
	}
	k.checkClosedEach(t)
}
