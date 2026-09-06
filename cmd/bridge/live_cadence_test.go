package main

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
)

// --- the UPnP ingest cadence ------------------------------------------------

// TestIngestIntervalOfFallsBackToTheDefault pins the resolver's two guards.
//
// Unlike the sweepers in jobstate.go, a zero here must NOT park the loop:
// scanIntervalSec has a validated floor, and this walk has no nudge channel to
// wake it, so a parked ingest loop would be indistinguishable from a dead one.
func TestIngestIntervalOfFallsBackToTheDefault(t *testing.T) {
	if got := ingestIntervalOf(nil); got != 6*time.Hour {
		t.Errorf("nil provider = %v, want the 6h default", got)
	}
	if got := ingestIntervalOf(func() time.Duration { return 0 }); got != 6*time.Hour {
		t.Errorf("zero interval = %v, want the 6h default (a parked walk reads as a dead one)", got)
	}
	if got := ingestIntervalOf(func() time.Duration { return -time.Second }); got != 6*time.Hour {
		t.Errorf("negative interval = %v, want the 6h default", got)
	}
	if got := ingestIntervalOf(func() time.Duration { return 90 * time.Second }); got != 90*time.Second {
		t.Errorf("live interval = %v, want 90s", got)
	}
}

// testLogger discards output — these tests are about cadence, and the ingest
// loop logs a line per pass.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type stubServerResolver struct{}

func (stubServerResolver) ResolveControlURL(context.Context, config.UPnPUpstreamServerConfig) (string, error) {
	return "", nil
}

// disabledIngester returns an Ingester whose Run exits immediately (the config
// is disabled), so the loop's CADENCE can be exercised without any network or
// walk work.
func disabledIngester(t *testing.T) *upnpingest.Ingester {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ing, err := upnpingest.NewIngester(
		config.UPnPUpstreamConfig{Enabled: false},
		&upnp.ContentDirectoryClient{}, stubServerResolver{}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ing
}

// TestRunIngestLoopRereadsItsIntervalEveryIteration pins the half of the cadence
// rule this loop was missing.
//
// It used to capture a time.Duration and feed one time.NewTicker built before
// the loop — so scanIntervalSec was frozen at boot for the THIRD consumer of
// that field, while RunPeriodic and the analysis sweeper both honoured a change
// immediately and PATCH /api/settings answered {"scanIntervalSec":
// {"status":"live"}}.
//
// Counting PROVIDER CALLS is the assertion, not observing a faster tick: a
// captured duration calls the provider once, a re-read calls it once per
// iteration, and that difference is exact rather than timing-dependent.
func TestRunIngestLoopRereadsItsIntervalEveryIteration(t *testing.T) {
	old := upnpIngestWarmup
	upnpIngestWarmup = time.Millisecond
	t.Cleanup(func() { upnpIngestWarmup = old })

	var calls atomic.Int64
	interval := func() time.Duration {
		calls.Add(1)
		return 5 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &upnpUpstreamLifecycle{log: testLogger()}
	l.ingestWg.Add(1)
	done := make(chan struct{})
	go func() { defer close(done); l.runIngestLoop(ctx, disabledIngester(t), interval, nil) }()

	deadline := time.After(5 * time.Second)
	for calls.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("the interval provider was called %d time(s) in 5s; a captured duration "+
				"calls it once, which is the boot-frozen shape this pins", calls.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ingest loop did not exit on ctx cancel")
	}
}

// TestRunIngestLoopRearmDoesNotIngest pins that a cadence change re-reads the
// schedule WITHOUT walking every upstream. A settings save is not a request to
// do the work — on a large upstream library that walk is minutes.
func TestRunIngestLoopRearmDoesNotIngest(t *testing.T) {
	old := upnpIngestWarmup
	upnpIngestWarmup = time.Millisecond
	t.Cleanup(func() { upnpIngestWarmup = old })

	var providerCalls atomic.Int64
	// Long enough that the timer cannot fire during the test, so any
	// re-arm observed can only have come from the rearm channel.
	interval := func() time.Duration {
		providerCalls.Add(1)
		return time.Hour
	}
	rearm := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &upnpUpstreamLifecycle{log: testLogger()}
	l.ingestWg.Add(1)
	done := make(chan struct{})
	go func() { defer close(done); l.runIngestLoop(ctx, disabledIngester(t), interval, rearm) }()

	// Wait for the loop to arm its first timer.
	deadline := time.After(5 * time.Second)
	for providerCalls.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("the loop never armed a timer")
		case <-time.After(time.Millisecond):
		}
	}
	before := providerCalls.Load()

	rearm <- struct{}{}
	// The rearm must cause exactly one more provider read (the next
	// iteration's), and no ingest — with a 1h interval nothing else can wake
	// the loop, so a second read is proof the rearm was honoured.
	for providerCalls.Load() <= before {
		select {
		case <-deadline:
			t.Fatalf("the rearm did not re-read the interval (%d calls); a cadence change "+
				"would wait out the OLD schedule, which on 6h is a lie with a straight face",
				providerCalls.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the ingest loop did not exit on ctx cancel")
	}
}

// --- the smart-playlist regenerator's analysis gate --------------------------

// TestSmartPlaylistRegeneratorReadsAnalysisLive pins the other boot-frozen
// value.
//
// The parameter used to be a plain bool — the VALUE of analysisActiveFn(),
// evaluated once at wiring time — sitting beside an `enabled` closure whose own
// docstring explains why capturing a boot value is wrong. Both gate the same
// cache. POST /api/smart-playlists/regenerate reads the gate live, so the
// harmonic families appeared on a manual regenerate and were dropped again by
// the next scheduled one, with nothing logged.
//
// Again the assertion is on the predicate being CALLED per run rather than on
// the generated content: it is exact, and it is the property that was missing.
func TestSmartPlaylistRegeneratorReadsAnalysisLive(t *testing.T) {
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	oldSettle := smartPlaylistSettleDelay
	smartPlaylistSettleDelay = time.Millisecond
	t.Cleanup(func() { smartPlaylistSettleDelay = oldSettle })

	var analysisReads atomic.Int64
	analysisActive := func() bool { analysisReads.Add(1); return false }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := &sweepStatus[struct{}]{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSmartPlaylistRegenerator(ctx, store, analysisActive,
			func() bool { return true }, staticInterval(5*time.Millisecond), nil, status)
	}()

	deadline := time.After(5 * time.Second)
	for analysisReads.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("the analysis gate was read %d time(s) across repeated runs; a captured "+
				"boot value reads it once", analysisReads.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the regenerator did not exit on ctx cancel")
	}
}
