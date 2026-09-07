package transcode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestJobTimeoutFor pins the per-spec deadline: the base for anything
// without a known duration, 2× the duration (4× for DST) when it is known,
// never below the base, never above the 4 h cap, and a size-derived
// duration for a DSD source the manifest did not time — at the NOMINAL
// rate, so a DSD64 hour reads as an hour, not eight.
func TestJobTimeoutFor(t *testing.T) {
	base := 10 * time.Minute
	dsd64Hour := JobSpec{SourceIsDSD: true, SourceSampleRate: 2822400, SourceChannels: 2,
		SourceSize: 2822400 / 8 * 2 * 3600} // 2.54 GB — an hour of stereo DSD64
	cases := []struct {
		name string
		spec JobSpec
		want time.Duration
	}{
		{"no duration keeps the base", JobSpec{}, base},
		{"PCM with a short duration keeps the base", JobSpec{SourceDurationSec: 60}, base},
		{"PCM with a long duration widens", JobSpec{SourceDurationSec: 3600}, 2 * time.Hour},
		{"DSD with a manifest duration widens", JobSpec{SourceIsDSD: true, SourceDurationSec: 1800}, time.Hour},
		{"DST doubles the budget", JobSpec{SourceIsDSD: true, SourceDurationSec: 1800, SourceCompression: "DST"}, 2 * time.Hour},
		{"the cap binds", JobSpec{SourceIsDSD: true, SourceDurationSec: 4 * 3600}, maxJobTimeout},
		{"DST cannot exceed the cap either", JobSpec{SourceIsDSD: true, SourceDurationSec: 4 * 3600, SourceCompression: "DST"}, maxJobTimeout},
		{"a DSD hour derived from size at the nominal rate", dsd64Hour, 2 * time.Hour},
		{"unknown channels assume stereo", JobSpec{SourceIsDSD: true, SourceSampleRate: 2822400,
			SourceSize: 2822400 / 8 * 2 * 3600}, 2 * time.Hour},
		{"a PCM source is never size-derived", JobSpec{SourceSampleRate: 44100, SourceSize: 1 << 30}, base},
		{"negative duration keeps the base", JobSpec{SourceDurationSec: -5}, base},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobTimeoutFor(base, tc.spec); got != tc.want {
				t.Errorf("jobTimeoutFor = %v, want %v", got, tc.want)
			}
		})
	}
	// A base larger than the widened value wins (tests shrink the base to
	// milliseconds; production never lowers it below the default).
	if got := jobTimeoutFor(3*time.Hour, JobSpec{SourceDurationSec: 60}); got != 3*time.Hour {
		t.Errorf("a larger base is kept: %v", got)
	}
}

// TestPoolPCMRenderUsesForegroundLane is the end-to-end half of the pcm
// routing row: an on-demand faithful rendition lands in the foreground
// channel, the sweeper's background one does not. Same no-drain shape as
// TestPoolBackgroundOptimizeUsesUpscaleLane.
func TestPoolPCMRenderUsesForegroundLane(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	p := NewPool(store, 1, 8)
	p.fsyncFn = noopFsync
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p.runner = func(ctx context.Context, _ JobSpec) (RunResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return RunResult{}, context.Canceled
	}
	t.Cleanup(p.Stop)

	if err := p.Enqueue(JobSpec{SourceLibraryRel: "occupy.flac", TargetSampleRate: 44100, TargetBits: 16, Kind: JobKindUpscale}); err != nil {
		t.Fatalf("occupy enqueue: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(p.upscaleJobs) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Enqueue(JobSpec{SourceLibraryRel: "a.dsf", SourceIsDSD: true, TargetSampleRate: 176400, TargetBits: 24, Kind: JobKindPCMRender}); err != nil {
		t.Fatalf("foreground pcm enqueue: %v", err)
	}
	if err := p.Enqueue(JobSpec{SourceLibraryRel: "b.dsf", SourceIsDSD: true, TargetSampleRate: 176400, TargetBits: 24, Kind: JobKindPCMRender, Background: true}); err != nil {
		t.Fatalf("background pcm enqueue: %v", err)
	}
	if got := len(p.optimizeJobs); got != 1 {
		t.Errorf("optimizeJobs (foreground lane) = %d, want 1 (the on-demand pcm job)", got)
	}
	if got := len(p.upscaleJobs); got != 1 {
		t.Errorf("upscaleJobs (background lane) = %d, want 1 (the background pcm job)", got)
	}
}

// TestActiveWorkersReportSourceIsDSD pins the worker-grid field and its
// wire key: the admin grid needs it to say "DSD64 → 176.4/24".
func TestActiveWorkersReportSourceIsDSD(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	p := NewPool(store, 2, 4)
	t.Cleanup(p.Stop)
	p.activeJobs[0].Store(&ActiveJob{SourceRel: "a.dsf", SourceIsDSD: true, SourceSampleRate: 2822400,
		TargetSampleRate: 176400, TargetBits: 24, Kind: JobKindPCMRender})
	views := p.ActiveWorkers()
	if len(views) != 2 || !views[0].Busy || !views[0].SourceIsDSD || views[0].Kind != "pcm" {
		t.Fatalf("views = %+v", views)
	}
	if views[1].SourceIsDSD {
		t.Error("an idle slot carries no DSD flag")
	}
	b, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"sourceIsDSD":true`) || strings.Count(string(b), "sourceIsDSD") != 1 {
		t.Errorf("wire shape: %s", b)
	}
}

// TestProcessJobUsesTheWidenedTimeout drives the timeout branch through a
// spec whose duration widens the deadline: with the base shrunk to
// milliseconds a runner that parks until ctx is cancelled must be given
// the widened budget, not the base.
func TestProcessJobUsesTheWidenedTimeout(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })
	p := NewPool(store, 1, 4)
	p.fsyncFn = noopFsync
	p.jobTimeout = 20 * time.Millisecond
	deadlines := make(chan time.Duration, 1)
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Error("processJob must set a deadline")
		}
		deadlines <- time.Until(dl)
		<-ctx.Done()
		return RunResult{}, ctx.Err()
	}
	t.Cleanup(p.Stop)
	// 1 s of DSD widens 20 ms to max(20 ms, min(4 h, 2 s)) = 2 s.
	if err := p.Enqueue(JobSpec{SourceLibraryRel: "a.dsf", SourceIsDSD: true, SourceDurationSec: 1,
		TargetSampleRate: 176400, TargetBits: 24, Kind: JobKindPCMRender}); err != nil {
		t.Fatal(err)
	}
	select {
	case remaining := <-deadlines:
		if remaining < 500*time.Millisecond || remaining > 2*time.Second {
			t.Errorf("deadline %v from now, want ≈ 2 s (the widened budget), not the 20 ms base", remaining)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the runner never ran")
	}
}
