package albumgain

import (
	"context"
	"testing"
	"time"
)

// TestAMateWhoseMeasurementPanicsReleasesItsClaim pins that a survey
// releases the claim it holds on an album-mate on every exit, a panic
// included.
//
// A survey claims a mate while it measures it, and the measurement is a
// decode (MeasureDSDPeak: ffmpeg and sox plumbing). transcode.Pool's
// processJob recovers a panic in its runner and keeps the worker, so the
// process lives on past such a panic. A claim released by a plain call after
// the measurement stayed registered and its done channel stayed open: every
// later render whose album includes that mate found the claim held, waited
// on it until its own job deadline, failed, and did the same on retry, until
// the bridge restarted. The render's own claim has resolved on every exit
// since the album gain was written (renderDSD defers it); the survey's did
// not.
//
// The fake takes the survey's claim from inside the measurement (tryClaim
// answers the claim the survey holds), then panics, and the test recovers
// the panic the way the pool does.
func TestAMateWhoseMeasurementPanicsReleasesItsClaim(t *testing.T) {
	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	mate := refs[1].Path
	key := claimKey{path: mate, profile: j.DSDPeakProfile()}
	h.meas.peaks[refs[2].Path] = fp(-5.0)

	var (
		held       *claim
		heldByUs   bool
		panickedAt string
	)
	h.meas.hook = func(path string) {
		if path != mate {
			return
		}
		held, heldByUs = h.r.tryClaim(key)
		panickedAt = path
		panic("sox plumbing failed mid-measure")
	}

	recovered := func() (r any) {
		defer func() { r = recover() }()
		_, _, _ = h.r.AlbumGainDB(context.Background(), j, fp(-8.0))
		return nil
	}()
	if recovered == nil || panickedAt != mate {
		t.Fatalf("the measurement of %s never panicked (recovered %v): the survey did not reach the mate", mate, recovered)
	}
	if heldByUs || held == nil {
		t.Fatal("the survey did not hold the mate's claim while measuring it; the fixture no longer reaches the case")
	}

	select {
	case <-held.done:
	default:
		t.Error("the claim the survey held while measuring is still open after the panic: every survey waiting on it blocks until its job's deadline")
	}

	// What the defect cost: the next render of the album, a different track,
	// waits on the mate's claim. With the claim released it measures the mate
	// itself and answers well inside the deadline.
	h.meas.mu.Lock()
	h.meas.hook = nil
	h.meas.peaks[mate] = fp(-3.0)
	h.meas.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	g, ok, err := h.r.AlbumGainDB(ctx, compactSpec(refs[2].Path, dsd64), fp(-5.0))
	if err != nil || !ok {
		t.Fatalf("the next render of the album: ok=%v err=%v; it waited on the claim the panic left behind", ok, err)
	}
	if g != 2.0 {
		t.Errorf("gain = %.1f, want 2.0 (the -3 dBTP mate, measured by this render, constrains the album)", g)
	}
	if n := h.meas.callCount(mate); n != 2 {
		t.Errorf("the mate was measured %d times, want 2 (the panicking measure, then this render's)", n)
	}
}
