package transcode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestA32BitBatchIsNotPlannedWhenTheHostCannotEncodeIt is the planning
// gate: a 32-bit batch whose probe says no enqueues nothing and warns
// once, and the same batch whose probe says yes enqueues.
func TestA32BitBatchIsNotPlannedWhenTheHostCannotEncodeIt(t *testing.T) {
	prev := PlanFLAC32
	t.Cleanup(func() { PlanFLAC32 = prev })
	can := false
	PlanFLAC32 = func(context.Context) (bool, error) { return can, nil }

	s := openTempStoreForBatch(t)
	seedOnePCMFLAC(t, s)
	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	rec := loggingtest.Record(t)

	_, err := c.Submit(context.Background(), "Album", 192000, 32, t.TempDir())
	if !errors.Is(err, ErrFLAC32Unencodable) || p.Stats().Enqueued != 0 {
		t.Fatalf("Submit = %v, enqueued %d, want the refusal and nothing queued", err, p.Stats().Enqueued)
	}
	rows, lerr := s.ListUpscaleBatches(context.Background(), 10)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(rows) != 0 {
		t.Fatalf("batch rows %d, want none queued", len(rows))
	}

	_, err = c.Submit(context.Background(), "Album", 192000, 32, t.TempDir())
	if !errors.Is(err, ErrFLAC32Unencodable) {
		t.Fatalf("second Submit = %v, want ErrFLAC32Unencodable", err)
	}
	warns := rec.Failures(logDepthMismatch)
	if len(warns) != 1 || !strings.Contains(warns[0], FLAC32EncodeFix) {
		t.Fatalf("warnings %d, want one outage report naming the fix:\n%v", len(warns), rec.Lines(logDepthMismatch))
	}
	if p.Stats().Enqueued != 0 {
		t.Fatalf("enqueued %d after the second refusal, want 0", p.Stats().Enqueued)
	}

	can = true
	res, err := c.Submit(context.Background(), "Album", 192000, 32, t.TempDir())
	if err != nil {
		t.Fatalf("Submit with a probe that says yes: %v", err)
	}
	if res.EnqueuedCount != 1 {
		t.Fatalf("enqueued %d, want the one eligible file", res.EnqueuedCount)
	}
}

// TestA32BitProbeThatDidNotRunStillPlans keeps a probe error off the
// refusal. Missing sox and a cancelled context stay on the missing-tool path.
func TestA32BitProbeThatDidNotRunStillPlans(t *testing.T) {
	prev := PlanFLAC32
	t.Cleanup(func() { PlanFLAC32 = prev })
	PlanFLAC32 = func(context.Context) (bool, error) { return false, ErrSoxMissing }

	s := openTempStoreForBatch(t)
	seedOnePCMFLAC(t, s)
	c, _, _ := newTestCoordinatorWithStubbedPool(t, s)
	res, err := c.Submit(context.Background(), "Album", 192000, 32, t.TempDir())
	if errors.Is(err, ErrFLAC32Unencodable) {
		t.Fatal("a probe that did not run was treated as a refusal")
	}
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.EnqueuedCount != 1 {
		t.Fatalf("enqueued %d, want the file planned", res.EnqueuedCount)
	}
}

// clearFLAC32Answer drops a cached probe without copying the mutex.
func clearFLAC32Answer() {
	flac32Answers.mu.Lock()
	flac32Answers.at = time.Time{}
	flac32Answers.can = false
	flac32Answers.err = nil
	flac32Answers.have = false
	flac32Answers.mu.Unlock()
}

func seedOnePCMFLAC(t *testing.T, s *manifest.Store) {
	t.Helper()
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Album"}); err != nil {
		t.Fatal(err)
	}
	rate := float64(44100)
	bits := 16
	isDSD := false
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path:          "Album/01.flac",
		Size:          1000,
		SampleRate:    &rate,
		BitsPerSample: &bits,
		Codec:         "FLAC",
		IsDSD:         &isDSD,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFLAC32PlanningProbeIsCachedForAShortLifetime(t *testing.T) {
	prev := flac32Probe
	t.Cleanup(func() {
		flac32Probe = prev
		clearFLAC32Answer()
	})
	clearFLAC32Answer()
	var n int
	flac32Probe = func(context.Context) (bool, error) {
		n++
		return false, nil
	}
	for range 2 {
		can, err := askFLAC32Cached(context.Background())
		if err != nil || can {
			t.Fatalf("can=%v err=%v", can, err)
		}
	}
	if n != 1 {
		t.Fatalf("probes %d, want 1 inside the window", n)
	}
	flac32Answers.mu.Lock()
	flac32Answers.at = time.Now().Add(-flac32AnswerTTL - time.Second)
	flac32Answers.mu.Unlock()
	if _, err := askFLAC32Cached(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("probes %d, want a fresh probe after the window", n)
	}
}

func TestACancelledFLAC32ProbeIsNotCached(t *testing.T) {
	prev := flac32Probe
	t.Cleanup(func() {
		flac32Probe = prev
		clearFLAC32Answer()
	})
	clearFLAC32Answer()
	var n int
	flac32Probe = func(ctx context.Context) (bool, error) {
		n++
		return false, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := askFLAC32Cached(ctx); err == nil {
		t.Fatal("cancelled probe returned a nil error")
	}
	if _, err := askFLAC32Cached(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("probes %d, want the cancelled answer left uncached", n)
	}
}
