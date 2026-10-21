package alsapcm

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// fillAndStart writes a whole buffer of frames into a freshly opened model
// device, which starts its stream.
func fillAndStart(t *testing.T, k *fakeKernel, d *Device) {
	t.Helper()
	n := d.Params().BufferFrames
	if got, err := d.Write(context.Background(), frames48k(n)); err != nil || got != n {
		t.Fatalf("filling the buffer: %d, %v", got, err)
	}
	if k.state != stateRunning {
		t.Fatalf("a full buffer left the stream %s, want RUNNING", State(k.state))
	}
}

// TestPauseUsesTheKernelsPAUSE pins Device.Pause on hardware that offers
// it: PAUSE with 1 pauses the running stream, PAUSE with 0 releases it, and
// the value reaches the kernel as the argument itself, never through a
// pointer (snd_pcm_common_ioctl reads (unsigned long)arg).
func TestPauseUsesTheKernelsPAUSE(t *testing.T) {
	k := newModel(t)
	d := mustOpen(t, k, cfg48k)
	fillAndStart(t, k, d)
	if !d.Params().CanPause() {
		t.Fatal("the setup of hardware that offers PAUSE says it cannot pause")
	}
	if err := d.Pause(true); err != nil {
		t.Fatalf("Pause(true): %v", err)
	}
	if k.state != statePaused {
		t.Errorf("after Pause(true) the stream is %s, want PAUSED", State(k.state))
	}
	if err := d.Pause(false); err != nil {
		t.Fatalf("Pause(false): %v", err)
	}
	if k.state != stateRunning {
		t.Errorf("after Pause(false) the stream is %s, want RUNNING", State(k.state))
	}
	if !slices.Equal(k.pauses, []uintptr{1, 0}) || k.callsOf(ioctlPause) != 2 {
		t.Errorf("PAUSE was given %v in %d calls, want 1 then 0", k.pauses, k.callsOf(ioctlPause))
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestPauseIsRefusedByHardwareThatCannot pins that hardware without PAUSE,
// whose setup lacks SNDRV_PCM_INFO_PAUSE and whose kernel answers ENOSYS,
// is a typed refusal, not a kernel error, and leaves the stream running.
func TestPauseIsRefusedByHardwareThatCannot(t *testing.T) {
	k := newModel(t)
	k.hw.info &^= infoPause
	d := mustOpen(t, k, cfg48k)
	fillAndStart(t, k, d)
	if d.Params().CanPause() {
		t.Error("the setup of hardware without PAUSE says it can pause")
	}
	err := d.Pause(true)
	wantReason(t, err, ReasonNotSupported)
	if !errors.Is(err, errnoENOSYS) {
		t.Errorf("%v does not wrap ENOSYS", err)
	}
	if k.state != stateRunning {
		t.Errorf("a refused pause left the stream %s", State(k.state))
	}
	_ = d.Close()
}

// TestPauseTypesAStreamItCannotPause pins that a pause of a stream that is
// not running (still filling), and a release of one that is not paused,
// are typed by the stream's state.
func TestPauseTypesAStreamItCannotPause(t *testing.T) {
	k := newModel(t)
	d := mustOpen(t, k, cfg48k)
	wantReason(t, d.Pause(true), ReasonBadState)
	fillAndStart(t, k, d)
	wantReason(t, d.Pause(false), ReasonBadState)
	k.unplug()
	wantReason(t, d.Pause(true), ReasonDisconnected)
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestPrepareStartsTheStreamAgain pins Device.Prepare: after an xrun it
// prepares the stream, whose pointers start from zero and which takes
// frames again; on a running stream the kernel's EBUSY comes back as an
// error.
func TestPrepareStartsTheStreamAgain(t *testing.T) {
	k := newModel(t)
	d := mustOpen(t, k, cfg48k)
	fillAndStart(t, k, d)
	if err := d.Prepare(); err == nil {
		t.Error("Prepare on a running stream returned nil, want the kernel's EBUSY")
	}
	k.advanceNow(time.Second)
	if k.state != stateXrun {
		t.Fatalf("a second of play from 200 ms of frames left the stream %s, want XRUN", State(k.state))
	}
	if err := d.Prepare(); err != nil {
		t.Fatalf("Prepare after an xrun: %v", err)
	}
	if k.state != statePrepared || k.appl != 0 || k.consumed != 0 {
		t.Errorf("after Prepare: %s, appl %d, hw %d; want PREPARED from zero", State(k.state), k.appl, k.consumed)
	}
	if n, err := d.Write(context.Background(), frames48k(10)); err != nil || n != 10 {
		t.Errorf("a write after Prepare: %d, %v", n, err)
	}
	_ = d.Close()
	k.checkClosedOnce(t)
}

// TestWaitPollsForNoEvent pins the wait a paused or filling stream is
// watched with: a poll that asks for no event, bounded by its timeout, so
// only an error condition wakes it early.
func TestWaitPollsForNoEvent(t *testing.T) {
	k := newFake(t)
	d := mustOpen(t, k, cfg48k)
	k.polls = []fakePoll{{timeout: true}, {revents: pollErr}}
	if rev, err := d.wait(context.Background(), 3*time.Second); err != nil || rev != 0 {
		t.Errorf("a wait that times out: %#x, %v", rev, err)
	}
	if rev, err := d.wait(context.Background(), time.Second); err != nil || rev != pollErr {
		t.Errorf("a wait an error condition ends: %#x, %v", rev, err)
	}
	if !slices.Equal(k.pollEvents, []int16{0, 0}) || !slices.Equal(k.pollWaits, []time.Duration{3 * time.Second, time.Second}) {
		t.Errorf("polled for %#x with %v, want no event with each timeout", k.pollEvents, k.pollWaits)
	}
	_ = d.Close()
	if _, err := d.wait(context.Background(), time.Second); ReasonOf(err) != ReasonClosed {
		t.Errorf("a wait on a closed device: %v", err)
	}
	for name, op := range map[string]func() error{"Pause": func() error { return d.Pause(true) }, "Prepare": d.Prepare} {
		if err := op(); ReasonOf(err) != ReasonClosed {
			t.Errorf("%s after Close: %v, want %s", name, err, ReasonClosed)
		}
	}
	k.checkClosedOnce(t)
}
