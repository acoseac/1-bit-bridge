package alsapcm_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// waitAttempt waits until the engine's last play attempt is the seq'th.
func (r *rig) waitAttempt(seq uint64) engine.Attempt {
	r.t.Helper()
	return r.waitSnap(fmt.Sprintf("play attempt %d", seq), func(s *engine.Snapshot) bool { return s.LastAttempt().Seq >= seq }).LastAttempt()
}

// TestPlaybackStartOnABusyDeviceIsTheEnginesBusyAttempt pins the busy
// classification at a start, through the real chain (B470 PR 5b): the fake
// kernel refuses the open with EBUSY, as while MPD holds the device;
// alsapcm types it busy, the backend maps it to engine.ErrDeviceBusy, and
// the engine records the start as a busy attempt. A retry once the device is
// free succeeds and is no busy attempt, nor is the pause that follows it.
func TestPlaybackStartOnABusyDeviceIsTheEnginesBusyAttempt(t *testing.T) {
	r := newRig(t, func(o *rigOptions) { o.pauseRelease = time.Second })
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	r.k.Busy(true)
	err := r.eng.Play(bg)
	if !errors.Is(err, engine.ErrDeviceBusy) || alsapcm.ReasonOf(err) != alsapcm.ReasonBusy {
		t.Fatalf("a start on a busy device answered %v; want alsapcm's busy refusal and engine.ErrDeviceBusy", err)
	}
	if at := r.waitAttempt(1); !at.Failed || !at.Busy {
		t.Fatalf("the busy start's attempt: %+v, want failed and busy", at)
	}
	r.waitStopped(engine.ReasonBackend)

	r.k.Busy(false)
	r.play()
	if at := r.waitAttempt(2); at.Failed || at.Busy {
		t.Fatalf("the retry's attempt: %+v, want neither failed nor busy", at)
	}
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.Play(t, 100*time.Millisecond)
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("pause: %v", err)
	}
	s := r.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	if at := s.LastAttempt(); at.Seq != 2 || at.Failed || at.Busy {
		t.Fatalf("after the pause: %+v, want attempt 2, neither failed nor busy", at)
	}
	r.play()
	r.heardAll(format.S243LE, 24, a)
}

// TestPlaybackBusyResumeAfterTheReleaseIsTheEnginesBusyAttempt pins the
// classification at a resume after the pause released the DAC, through the
// real chain: another process took the device meanwhile, the reopen is
// refused busy, the backend's ResumeError maps the alsapcm error (not its
// code) to engine.ErrDeviceBusy, and the engine records a busy attempt with
// the session paused; the resume once the device is free plays every frame
// once and ends it.
func TestPlaybackBusyResumeAfterTheReleaseIsTheEnginesBusyAttempt(t *testing.T) {
	r, a := pausedRig(t)
	r.releaseDevice()
	start := r.eng.Snapshot().LastAttempt()
	if start.Failed {
		t.Fatalf("the start's attempt: %+v", start)
	}
	r.k.Busy(true)
	err := r.eng.Play(bg)
	var re *playback.ResumeError
	if !errors.As(err, &re) || re.Code != "busy" || !errors.Is(err, engine.ErrDeviceBusy) {
		t.Fatalf("a resume of a busy device answered %v; want a ResumeError, busy, and engine.ErrDeviceBusy", err)
	}
	if at := r.waitAttempt(start.Seq + 1); !at.Failed || !at.Busy {
		t.Fatalf("the busy resume's attempt: %+v, want failed and busy", at)
	}
	if s := r.eng.Snapshot(); s.State() != engine.Paused {
		t.Fatalf("after the busy resume: %s, want paused", s.State())
	}
	r.k.Busy(false)
	r.play()
	if at := r.waitAttempt(start.Seq + 2); at.Failed || at.Busy {
		t.Fatalf("the resume's attempt once the device is free: %+v, want neither failed nor busy", at)
	}
	r.heardAll(format.S243LE, 24, a)
}

// TestPlaybackARefusalThatSaysBusyIsNoBusyAttempt: the backend maps
// alsapcm's busy reason alone, never the error's words, so an open refused
// for another reason whose detail says busy is a failed attempt and not a
// busy one.
func TestPlaybackARefusalThatSaysBusyIsNoBusyAttempt(t *testing.T) {
	r := newRig(t, func(o *rigOptions) {
		o.openSink = func(open func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)) func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
			return func(_ context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				return nil, &alsapcm.Error{Reason: alsapcm.ReasonPermission, Op: "open", Endpoint: cfg.Endpoint,
					Detail: "the device is busy, says the decoy"}
			}
		}
	})
	r.files.add("a", wav(48000, 24, pattern(1, 4800, 24)))
	r.insert("a")
	if err := r.eng.Play(bg); err == nil || errors.Is(err, engine.ErrDeviceBusy) {
		t.Fatalf("a permission refusal that says busy answered %v; want a refusal that is not the busy error", err)
	}
	if at := r.waitAttempt(1); !at.Failed || at.Busy {
		t.Fatalf("its attempt: %+v, want failed and not busy", at)
	}
	r.waitStopped(engine.ReasonBackend)
}
