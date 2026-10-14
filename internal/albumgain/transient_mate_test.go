package albumgain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// Backlog B215: a failed album-mate measurement left the mate out of the
// album whatever the reason, so a transient fault (a full scratch volume, an
// I/O error) published a boost decided without it: too high when it is the
// hottest, and kept by the rendition for good. A transient failure now
// decides no album figure (transcode.ErrAlbumMateUnmeasured); only a fact
// about the mate leaves it out.

// transientErr is a measurement failure that says nothing about the file.
var transientErr = errors.New("write render scratch: no space left on device")

// TestATransientMateFailureDecidesNoAlbumFigure is the defect: the hottest
// mate's measurement fails for want of scratch space, and the call decides
// no figure rather than one without it.
func TestATransientMateFailureDecidesNoAlbumFigure(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.meas.errs[refs[1].Path] = transientErr
	h.meas.peaks[refs[2].Path] = fp(-5.0)

	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err == nil || ok {
		t.Fatalf("gain=%.1f ok=%v err=%v: a boost was decided without the mate whose measurement failed", g, ok, err)
	}
	if !errors.Is(err, transcode.ErrAlbumMateUnmeasured) || !errors.Is(err, transientErr) {
		t.Errorf("err = %v, want ErrAlbumMateUnmeasured wrapping the mate's own error", err)
	}
	// The other mate was still measured, so the next attempt has its peak.
	if n := h.meas.callCount(refs[2].Path); n != 1 {
		t.Errorf("the measurable mate was measured %d times, want 1", n)
	}
	if len(h.peaks.upserts) != 1 || h.peaks.upserts[0].SourcePath != refs[2].Path {
		t.Errorf("recorded %+v, want the measurable mate's peak alone", h.peaks.upserts)
	}
}

// TestATransientMateIsMeasuredOncePerCall: a mate given up on for a
// transient reason is not decoded again by a later pass of the same call,
// one that ran because another render held a claim the call waited on.
func TestATransientMateIsMeasuredOncePerCall(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.meas.errs[refs[1].Path] = transientErr
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	// Another render holds refs[2]'s claim, so the first pass waits on it.
	held, mine := h.r.tryClaim(claimKey{path: refs[2].Path, profile: j.DSDPeakProfile()})
	if !mine {
		t.Fatal("premise: the claim was free")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.r.release(claimKey{path: refs[2].Path, profile: j.DSDPeakProfile()}, held)
	}()

	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); !errors.Is(err, transcode.ErrAlbumMateUnmeasured) {
		t.Fatalf("err = %v, want ErrAlbumMateUnmeasured", err)
	}
	if n := h.meas.callCount(refs[1].Path); n != 1 {
		t.Errorf("the transient mate was measured %d times in one call, want 1", n)
	}
	if n := h.meas.callCount(refs[2].Path); n != 1 {
		t.Errorf("premise: the released mate was measured %d times, want 1 (a second pass ran)", n)
	}
}

// TestATransientMateRecordedSinceDecidesTheFigure: when the mate's peak is
// recorded by the time a later pass of the call reads the store (its own
// render finished meanwhile), the call decides the figure with it.
func TestATransientMateRecordedSinceDecidesTheFigure(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	profile := j.DSDPeakProfile()
	h.meas.errs[refs[1].Path] = transientErr
	h.meas.peaks[refs[2].Path] = fp(-9.0)
	held, mine := h.r.tryClaim(claimKey{path: refs[2].Path, profile: profile})
	if !mine {
		t.Fatal("premise: the claim was free")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		// The transient mate's own render records its peak, then the
		// claim the survey waits on resolves.
		h.peaks.put(refs[1].Path, profile, fp(-2.0))
		h.r.release(claimKey{path: refs[2].Path, profile: profile}, held)
	}()

	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok {
		t.Fatalf("gain=%.1f ok=%v err=%v, want a figure: the mate's peak was recorded since", g, ok, err)
	}
	if g != 1.0 {
		t.Errorf("gain = %.1f, want 1.0 (the recorded −2 dBTP mate constrains the album)", g)
	}
}

// TestAnIneligibleMateDoesNotConstrain: a mate this bridge would not render
// on the profile (SpecFor's ErrMateIneligible) is a fact about it.
func TestAnIneligibleMateDoesNotConstrain(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	inner := h.r.cfg.SpecFor
	h.r.cfg.SpecFor = func(ctx context.Context, path string, like transcode.JobSpec) (transcode.JobSpec, error) {
		if path == refs[1].Path {
			return transcode.JobSpec{}, fmt.Errorf("%w: %s is not renderable", ErrMateIneligible, path)
		}
		return inner(ctx, path, like)
	}
	h.meas.peaks[refs[2].Path] = fp(-5.0)

	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok || g != 4.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want 4.0 from the mates it renders", g, ok, err)
	}
}

// TestAMateOnAnotherProfileDoesNotConstrain: a mate whose row moved to
// another rate family since the index was built is no album-mate on this
// profile; the index is rebuilt for the next call.
func TestAMateOnAnotherProfileDoesNotConstrain(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	// SpecFor now builds refs[1] on the 48 kHz family's target, which is
	// what the profile is decided by.
	inner := h.r.cfg.SpecFor
	h.r.cfg.SpecFor = func(ctx context.Context, path string, like transcode.JobSpec) (transcode.JobSpec, error) {
		s, err := inner(ctx, path, like)
		if err == nil && path == refs[1].Path {
			s.SourceSampleRate, s.TargetSampleRate = dsd64x48, transcode.TargetRateForOptimize(dsd64x48)
		}
		return s, err
	}
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	streams := h.cat.streams.Load()

	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok || g != 4.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want 4.0 without the mate on the other profile", g, ok, err)
	}
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
		t.Fatal(err)
	}
	if h.cat.streams.Load() <= streams+1 {
		t.Errorf("the index was not rebuilt after the profile mismatch (%d catalog streams)", h.cat.streams.Load()-streams)
	}
}

// TestASuppressedMateDoesNotConstrain: a mate whose renders the pool has
// suppressed will not render, so its transient-looking failure leaves it
// out.
func TestASuppressedMateDoesNotConstrain(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	h.meas.errs[refs[1].Path] = transientErr
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	h.peaks.suppressed = map[string]bool{refs[1].Path: true}

	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok || g != 4.0 {
		t.Errorf("gain=%.1f ok=%v err=%v, want 4.0 without the suppressed mate", g, ok, err)
	}
}

// TestATransientMateStopsHoldingTheAlbumAfterTheGrace: a mate that keeps
// failing transiently holds its album back for mateTransientGrace from its
// first failure, and is left out after it. A recorded peak ends the grace.
func TestATransientMateStopsHoldingTheAlbumAfterTheGrace(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	mate := refs[1].Path
	h.meas.errs[mate] = transientErr
	h.meas.peaks[refs[2].Path] = fp(-5.0)

	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); !errors.Is(err, transcode.ErrAlbumMateUnmeasured) {
		t.Fatalf("first failure: err = %v, want ErrAlbumMateUnmeasured", err)
	}
	h.now = h.now.Add(mateTransientGrace - time.Second)
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); !errors.Is(err, transcode.ErrAlbumMateUnmeasured) {
		t.Fatalf("inside the grace: err = %v, want ErrAlbumMateUnmeasured", err)
	}
	h.now = h.now.Add(time.Second)
	g, ok, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
	if err != nil || !ok || g != 4.0 {
		t.Fatalf("after the grace: gain=%.1f ok=%v err=%v, want 4.0 without the mate", g, ok, err)
	}

	// A peak recorded for the mate ends its grace: a later failure on a new
	// version starts a new one.
	h.r.record(context.Background(), compactSpec(mate, dsd64), j.DSDPeakProfile(), fp(-3.0))
	h.peaks.mu.Lock()
	delete(h.peaks.rows, claimKey{path: mate, profile: j.DSDPeakProfile()})
	h.peaks.mu.Unlock()
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); !errors.Is(err, transcode.ErrAlbumMateUnmeasured) {
		t.Errorf("after a recorded peak, a new failure: err = %v, want ErrAlbumMateUnmeasured (a new grace)", err)
	}
}
