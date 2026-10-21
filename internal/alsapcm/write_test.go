package alsapcm

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// frames48k returns n frames of S24_3LE stereo (six bytes each) whose
// bytes all differ from their neighbours', so a frame written twice, or
// skipped, changes what the kernel received.
func frames48k(n int) []byte {
	b := make([]byte, n*6)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// TestWriteContinuesAPartialWriteWhereItStopped pins that a write the
// kernel takes in pieces continues from the first frame it did not take,
// across a wait for room, so it received every frame once, in order.
func TestWriteContinuesAPartialWriteWhereItStopped(t *testing.T) {
	k := newFake(t)
	k.writes = []fakeWrite{{take: 100}, {take: 1}, {err: errnoEAGAIN}, {take: 250}, {take: -1}}
	d := mustOpen(t, k, cfg48k)
	in := frames48k(1000)
	n, err := d.Write(context.Background(), in)
	if err != nil || n != 1000 {
		t.Fatalf("Write = %d, %v; want 1000, nil", n, err)
	}
	if !bytes.Equal(k.played, in) {
		t.Fatalf("the kernel received %d bytes that are not the %d written, in order", len(k.played), len(in))
	}
	if c := d.Counters(); c != (Counters{Writes: 4, Partial: 3, Waits: 1}) {
		t.Errorf("counters %+v, want 4 writes, 3 partial, 1 wait", c)
	}
	if len(k.pollEvents) != 1 || k.pollEvents[0] != pollOut {
		t.Errorf("polled for %v, want POLLOUT once", k.pollEvents)
	}
	_ = d.Close()
}

// TestWriteRetriesAnInterruptedCall pins that EINTR is tried again at once,
// without a wait and without losing a frame.
func TestWriteRetriesAnInterruptedCall(t *testing.T) {
	k := newFake(t)
	k.writes = []fakeWrite{{err: errnoEINTR}, {take: 10}, {err: errnoEINTR}, {take: -1}}
	d := mustOpen(t, k, cfg48k)
	in := frames48k(64)
	n, err := d.Write(context.Background(), in)
	if err != nil || n != 64 || !bytes.Equal(k.played, in) {
		t.Fatalf("Write = %d, %v, received %d bytes; want all 64 frames", n, err, len(k.played))
	}
	if c := d.Counters(); c.Interrupted != 2 || c.Waits != 0 {
		t.Errorf("counters %+v, want 2 interrupted and no wait", c)
	}
	if len(k.pollEvents) != 0 {
		t.Errorf("polled %d times after EINTR", len(k.pollEvents))
	}
	_ = d.Close()
}

// TestWritePollsForRoomOnEAGAIN pins that a full buffer is waited for with
// poll, bounded by the stall timeout, and the write then goes on.
func TestWritePollsForRoomOnEAGAIN(t *testing.T) {
	k := newFake(t)
	k.writes = []fakeWrite{{err: errnoEAGAIN}, {err: errnoEAGAIN}, {take: -1}}
	d := mustOpen(t, k, cfg48k)
	in := frames48k(32)
	if n, err := d.Write(context.Background(), in); err != nil || n != 32 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if len(k.pollWaits) != 2 {
		t.Fatalf("polled %d times, want 2", len(k.pollWaits))
	}
	want := max(stallFactor*d.Params().BufferTime(), minStall)
	for _, w := range k.pollWaits {
		if w != want {
			t.Errorf("a wait for room is bounded by %v, want %v", w, want)
		}
	}
	_ = d.Close()
}

// TestWriteStopsWhenNoRoomComes pins that a wait for room that times out
// is a stalled stream, never an unbounded wait.
func TestWriteStopsWhenNoRoomComes(t *testing.T) {
	k := newFake(t)
	k.writes = []fakeWrite{{take: 5}, {err: errnoEAGAIN}}
	k.polls = []fakePoll{{timeout: true}}
	d := mustOpen(t, k, cfg48k)
	n, err := d.Write(context.Background(), frames48k(32))
	wantReason(t, err, ReasonStalled)
	if n != 5 {
		t.Errorf("Write reported %d frames, want the 5 taken", n)
	}
	_ = d.Close()
}

// TestWriteTypesAnXrunAndASuspend pins that EPIPE is an xrun and ESTRPIPE a
// suspend, each typed, each reporting the frames taken before it, and that
// neither is recovered here.
func TestWriteTypesAnXrunAndASuspend(t *testing.T) {
	for _, c := range []struct {
		err  errno
		want Reason
	}{{errnoEPIPE, ReasonXrun}, {errnoESTRPIPE, ReasonSuspended}} {
		k := newFake(t)
		k.writes = []fakeWrite{{take: 7}, {err: c.err}}
		d := mustOpen(t, k, cfg48k)
		n, err := d.Write(context.Background(), frames48k(32))
		wantReason(t, err, c.want)
		if !errors.Is(err, c.err) {
			t.Errorf("%v does not wrap %v", err, c.err)
		}
		if n != 7 {
			t.Errorf("%v: reported %d frames, want 7", c.err, n)
		}
		if k.callsOf(ioctlPrepare) != 1 || k.callsOf(ioctlDrop) != 0 {
			t.Errorf("%v: recovered the stream (%#x)", c.err, k.calls)
		}
		_ = d.Close()
	}
}

// TestWriteTypesADisconnect pins every way a device that went away shows
// itself to a write: ENODEV or ENXIO from the write, POLLHUP or POLLNVAL
// from the wait, POLLERR the kernel's status then explains (ENODEV, or the
// DISCONNECTED state), and EBADFD in that state.
func TestWriteTypesADisconnect(t *testing.T) {
	for name, set := range map[string]func(*fakeKernel){
		"ENODEV": func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoENODEV}} },
		"ENXIO":  func(k *fakeKernel) { k.writes = []fakeWrite{{err: errnoENXIO}} },
		"POLLHUP": func(k *fakeKernel) {
			k.writes, k.polls = []fakeWrite{{err: errnoEAGAIN}}, []fakePoll{{revents: pollOut | pollHup}}
		},
		"POLLNVAL": func(k *fakeKernel) {
			k.writes, k.polls = []fakeWrite{{err: errnoEAGAIN}}, []fakePoll{{revents: pollErr | pollNval}}
		},
		"POLLERR, ENODEV": func(k *fakeKernel) {
			k.writes, k.polls, k.fail = []fakeWrite{{err: errnoEAGAIN}}, []fakePoll{{revents: pollErr}}, map[uint32]error{ioctlStatus: errnoENODEV}
		},
		"POLLERR, state": func(k *fakeKernel) {
			k.writes, k.polls, k.states = []fakeWrite{{err: errnoEAGAIN}}, []fakePoll{{revents: pollOut | pollErr}}, []int32{stateDisconnected}
		},
		"EBADFD, state": func(k *fakeKernel) { k.writes, k.states = []fakeWrite{{err: errnoEBADFD}}, []int32{stateDisconnected} },
		"EBADFD, no status": func(k *fakeKernel) {
			k.writes, k.fail = []fakeWrite{{err: errnoEBADFD}}, map[uint32]error{ioctlStatus: errnoENODEV}
		},
	} {
		k := newFake(t)
		d := mustOpen(t, k, cfg48k)
		set(k)
		_, err := d.Write(context.Background(), frames48k(32))
		if ReasonOf(err) != ReasonDisconnected {
			t.Errorf("%s: got %v, want %s", name, err, ReasonDisconnected)
		}
		_ = d.Close()
		k.checkClosedOnce(t)
	}
}

// TestWritePollErrorIsWhatTheKernelSaysItIs pins that POLLERR alone is not
// a disconnect: the kernel's poll raises it for an xrun and a suspend too,
// so the write asks the stream's state, and goes on when the state is one
// that plays. A POLLERR the state does not explain twice in a row, with no
// frame taken between, is an error rather than a loop.
func TestWritePollErrorIsWhatTheKernelSaysItIs(t *testing.T) {
	for _, c := range []struct {
		state int32
		want  Reason
	}{{stateXrun, ReasonXrun}, {stateSuspended, ReasonSuspended}, {stateSetup, ReasonBadState}} {
		k := newFake(t)
		d := mustOpen(t, k, cfg48k)
		k.writes, k.polls, k.states = []fakeWrite{{err: errnoEAGAIN}}, []fakePoll{{revents: pollOut | pollErr}}, []int32{c.state}
		_, err := d.Write(context.Background(), frames48k(32))
		if ReasonOf(err) != c.want {
			t.Errorf("POLLERR in %s: got %v, want %s", State(c.state), err, c.want)
		}
		_ = d.Close()
	}

	t.Run("a state that plays", func(t *testing.T) {
		k := newFake(t)
		d := mustOpen(t, k, cfg48k)
		k.writes, k.polls, k.states = []fakeWrite{{err: errnoEAGAIN}, {take: -1}}, []fakePoll{{revents: pollErr}}, []int32{stateRunning}
		in := frames48k(32)
		if n, err := d.Write(context.Background(), in); err != nil || n != 32 || !bytes.Equal(k.played, in) {
			t.Errorf("Write = %d, %v; want all 32 frames after a POLLERR the state explains away", n, err)
		}
		_ = d.Close()
	})

	t.Run("twice unexplained", func(t *testing.T) {
		k := newFake(t)
		d := mustOpen(t, k, cfg48k)
		k.writes = []fakeWrite{{err: errnoEAGAIN}, {err: errnoEAGAIN}, {take: -1}}
		k.polls = []fakePoll{{revents: pollErr}, {revents: pollErr}}
		k.states = []int32{stateRunning, stateRunning}
		_, err := d.Write(context.Background(), frames48k(32))
		wantReason(t, err, ReasonKernel)
		_ = d.Close()
	})
}

// TestWriteCancelDuringAPollReturnsAndTheDeviceStillDrops pins that a
// cancel while a write waits for room ends the wait at once, reports the
// frames already taken, and leaves a device that drops and closes. The
// fake's poll says when it is waiting, so nothing here sleeps.
func TestWriteCancelDuringAPollReturnsAndTheDeviceStillDrops(t *testing.T) {
	k := newFake(t)
	k.writes = []fakeWrite{{take: 100}, {err: errnoEAGAIN}}
	k.polls = []fakePoll{{block: true}}
	d := mustOpen(t, k, cfg48k)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := d.Write(ctx, frames48k(1000))
		done <- result{n, err}
	}()
	<-k.entered
	cancel()
	r := <-done
	wantReason(t, r.err, ReasonCancelled)
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("%v does not wrap context.Canceled", r.err)
	}
	if r.n != 100 {
		t.Errorf("reported %d frames, want the 100 taken before the wait", r.n)
	}
	if err := d.Drop(); err != nil {
		t.Fatalf("Drop after a cancelled write: %v", err)
	}
	if k.callsOf(ioctlDrop) != 1 {
		t.Errorf("DROP issued %d times", k.callsOf(ioctlDrop))
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	k.checkClosedOnce(t)
}

// TestWriteChecksTheContextFirst pins that a write whose context is already
// done asks nothing of the kernel.
func TestWriteChecksTheContextFirst(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := d.Write(ctx, frames48k(8))
	wantReason(t, err, ReasonCancelled)
	if n != 0 || k.callsOf(ioctlWriteIFrames) != 0 {
		t.Errorf("wrote %d frames in %d calls on a cancelled context", n, k.callsOf(ioctlWriteIFrames))
	}
	_ = d.Close()
}

// TestWriteRefusesAPartFrame pins that bytes that are not whole frames of
// the committed format are refused before any reaches the kernel.
func TestWriteRefusesAPartFrame(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	_, err := d.Write(context.Background(), make([]byte, 6*10+4))
	wantReason(t, err, ReasonBadConfig)
	if k.callsOf(ioctlWriteIFrames) != 0 {
		t.Error("a part frame reached the kernel")
	}
	_ = d.Close()
}
