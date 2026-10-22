package engine

import (
	"context"
	"errors"
	"testing"
)

// TestResumeContinuesOrReopens pins what the engine records of a resume:
// the backend's answer, continued while it held the device and reopened
// after the pause released it.
func TestResumeContinuesOrReopens(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	if got := s.opts.PauseRelease; got != DefaultPauseRelease {
		t.Fatalf("the session's pause release is %v, want the configured %v", got, DefaultPauseRelease)
	}
	h.must("pause", h.eng.Pause(bg))
	h.must("resume", h.eng.Play(bg))
	if how, why := h.snap().LastResume(); how != ResumeContinued || why != "" {
		t.Fatalf("resumed %s %q, want continued", how, why)
	}
	h.must("pause", h.eng.Pause(bg))
	s.deviceReleased()
	h.waitSnap("the device released", func(sn *Snapshot) bool { return sn.DeviceReleased() })
	h.must("resume", h.eng.Play(bg))
	sn := h.snap()
	if how, _ := sn.LastResume(); how != ResumeReopened || sn.State() != Playing || sn.DeviceReleased() {
		t.Fatalf("resumed %s into %s, device released %v; want reopened, playing, held", how, sn.State(), sn.DeviceReleased())
	}
}

// TestARefusedReopenStaysPausedWithTheReason pins a resume the backend
// refuses: the transport stays paused and says why.
func TestARefusedReopenStaysPausedWithTheReason(t *testing.T) {
	h := startHarness(t)
	h.playing(0, "a")
	h.must("pause", h.eng.Pause(bg))
	busy := errors.New("the device is busy: mpd holds it")
	h.be.script(func() {
		h.be.onResume = func(context.Context, *fakeSession) (ResumeResult, error) { return 0, busy }
	})
	err := h.eng.Play(bg)
	if !errors.Is(err, busy) || ReasonOf(err) != ReasonBackend {
		t.Fatalf("resume answered %v, want the backend's refusal", err)
	}
	sn := h.snap()
	if _, why := sn.LastResume(); sn.State() != Paused || why == "" {
		t.Fatalf("after the refusal %s, reason %q; want paused with the reason", sn.State(), why)
	}
	if h.be.count("release") != 0 {
		t.Fatal("a refused resume released the session")
	}
}

// TestAPausesReleaseThatNeverComesIsUnconfirmed pins the paused session's
// own release under the confirmed-release rule: past PauseRelease plus
// ReleaseTimeout without the report, nothing opens the device, and the
// late report clears it.
func TestAPausesReleaseThatNeverComesIsUnconfirmed(t *testing.T) {
	h := startHarness(t)
	h.playing(0, "a")
	// The session's Prepare (nothing follows a) answers at once; the clock
	// moves past its deadline only once the owner has taken that answer.
	h.waitApplied("handled call prepare", 0)
	h.must("pause", h.eng.Pause(bg))
	h.clk.Advance(DefaultPauseRelease + ReleaseTimeout)
	sn := h.waitSnap("the pause's release overdue", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return ok })
	if info, _ := sn.ReleaseUnconfirmed(); !info.Paused || sn.State() != Paused {
		t.Fatalf("unconfirmed %+v in %s", info, sn.State())
	}
	h.refused("resume while the pause's release is unconfirmed", h.eng.Play(bg), ReasonReleaseUnconfirmed)
	h.be.nth(1).deviceReleased()
	h.waitSnap("the late report", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return !ok })
	h.must("resume", h.eng.Play(bg))
	if how, _ := h.snap().LastResume(); how != ResumeReopened {
		t.Fatalf("resumed %s, want reopened", how)
	}
}
