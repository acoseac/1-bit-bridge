package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/analyze"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestCollectAnalysisCandidatesSkipsZeroByteSource pins that a zero-byte
// source file (a failed/incomplete upload — sox can't probe it and the ffmpeg
// fallback hits EOF) is skipped at collection time instead of being enqueued
// and re-failed on every sweep. A non-empty sibling still becomes a candidate,
// and the skip self-heals on re-upload (size > 0). Field-reported on
// bridge.ars.md: 26 zero-byte FLACs from truncated B2 syncs spammed
// "analyze: failed" each sweep.
func TestCollectAnalysisCandidatesSkipsZeroByteSource(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.flac"), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.flac"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, tr := range []struct {
		path string
		size int64
	}{{"good.flac", 18}, {"empty.flac", 0}} {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: tr.path, Size: tr.size, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.path, err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.emptySkipped != 1 {
		t.Errorf("emptySkipped = %d, want 1", res.emptySkipped)
	}
	var gotGood, gotEmpty bool
	for _, c := range res.candidates {
		switch c.SourceLibraryRel {
		case "good.flac":
			gotGood = true
		case "empty.flac":
			gotEmpty = true
		}
	}
	if !gotGood {
		t.Error("good.flac (non-empty) should be a candidate")
	}
	if gotEmpty {
		t.Error("empty.flac (zero-byte) must NOT be enqueued for analysis")
	}
}

// TestAnalysisCoverageLockstepWithCollector pins the predicate-parity
// contract between Store.AnalysisCoverage's SQL buckets (the Jobs
// page's approximate whole-library tile) and
// collectAnalysisCandidates' control flow (the sweeper's exact truth):
// same DSD-by-extension rule, same zero-byte rule, same precedence
// (DSD before zero-byte). If either side's predicate drifts, this
// fails before an operator sees disagreeing numbers.
func TestAnalysisCoverageLockstepWithCollector(t *testing.T) {
	root := t.TempDir()
	files := []struct {
		name string
		data []byte
	}{
		{"good.flac", []byte("fLaC-nonzero")},
		{"more.mp3", []byte("ID3-nonzero")},
		{"disc.dsf", []byte("DSD-nonzero")},
		{"disc.dff", []byte("FRM8-nonzero")},
		{"empty.flac", nil},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(root, f.name), f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, f := range files {
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: f.name, Size: int64(len(f.data)), ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", f.name, err)
		}
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	cov, err := store.AnalysisCoverage(ctx, analyze.WaveformSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}

	if cov.TotalLocal != res.total {
		t.Errorf("TotalLocal = %d, collector total = %d", cov.TotalLocal, res.total)
	}
	if cov.DSDExcluded != res.dsdSkipped {
		t.Errorf("DSDExcluded = %d, collector dsdSkipped = %d", cov.DSDExcluded, res.dsdSkipped)
	}
	if cov.ZeroByteExcluded != res.emptySkipped {
		t.Errorf("ZeroByteExcluded = %d, collector emptySkipped = %d", cov.ZeroByteExcluded, res.emptySkipped)
	}
	// Sanity: the fixture has no missing/routed rows, so eligible ==
	// candidates + fresh-skipped (all zero analysis rows here).
	eligible := cov.TotalLocal - cov.DSDExcluded - cov.ZeroByteExcluded
	if eligible != len(res.candidates)+res.skipped {
		t.Errorf("eligible = %d, collector candidates+skipped = %d", eligible, len(res.candidates)+res.skipped)
	}
}

// waitForSweep polls the recorder until a completed sweep newer than
// `after` is visible or the deadline hits. Returns the lastEnd observed.
// alwaysAnalysisEnabled is the live-gate predicate for the cadence tests,
// which are about WHEN a sweep runs rather than whether it may.
func alwaysAnalysisEnabled() bool { return true }

// TestRunAnalysisSweeperRespectsDisabledGate pins the gate that PR #781's
// always-construct-never-stop conversion removed.
//
// Before that conversion the sweeper was wired inside `if analysisActive {`
// and the block WAS the gate. The conversion moved every READ surface to the
// live `analysisActiveFn` predicate and left the WRITE path ungated, so a
// bridge on the DEFAULT config (analysis.enabled is false) still walked the
// library 90 s after boot, forked a decode per track, and — because
// Store.UpsertAnalysis advances indexed_at — pushed a whole-library delta to
// every paired device, repeating on every scan interval and post-scan nudge.
// /v1/analysis/* answered 404 throughout, so nothing surfaced it.
//
// Asserting on the STATUS rather than on the absence of waveform files is
// deliberate: a disabled pass must record nothing at all (the same rule
// runFingerprintSweeper follows), so the Jobs card keeps the last real
// breakdown instead of being overwritten by an empty one. A sweep that ran
// and found nothing would still stamp sweepStarted, which is what
// distinguishes "gated" from "ran against an empty library" — and an
// empty-library assertion would pass either way.
func TestRunAnalysisSweeperRespectsDisabledGate(t *testing.T) {
	oldSettle := analysisSweeperSettleDelay
	analysisSweeperSettleDelay = 5 * time.Millisecond
	t.Cleanup(func() { analysisSweeperSettleDelay = oldSettle })

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := analyze.NewPool(store, 1, 8)
	defer pool.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := &sweepStatus[admin.AnalysisSweepCounts]{}
	nudge := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnalysisSweeper(ctx, store, bridgefs.New([]string{t.TempDir()}), t.TempDir(),
			pool, func() bool { return false }, staticInterval(time.Hour), nudge, nil, status)
	}()

	// Past the settle delay AND past a nudge — both sweep triggers, both of
	// which must be refused while the feature is off.
	time.Sleep(80 * time.Millisecond)
	nudge <- struct{}{}
	time.Sleep(80 * time.Millisecond)

	if _, lastStart, lastEnd, _, _ := status.snapshot(); !lastStart.IsZero() || !lastEnd.IsZero() {
		t.Errorf("a disabled sweeper recorded a sweep: lastStart=%v lastEnd=%v — the analysis.enabled gate is not being consulted", lastStart, lastEnd)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not exit on ctx cancel")
	}
}

func waitForSweep(t *testing.T, status *sweepStatus[admin.AnalysisSweepCounts], after time.Time, deadline time.Duration) time.Time {
	t.Helper()
	stop := time.Now().Add(deadline)
	for time.Now().Before(stop) {
		running, _, lastEnd, _, _ := status.snapshot()
		if !running && lastEnd.After(after) {
			return lastEnd
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no sweep completed after %v within %v", after, deadline)
	return time.Time{}
}

// TestRunAnalysisSweeperNudgeTriggersImmediateSweep pins the nudge
// contract: after the settle-delay initial sweep, a nudge (post-scan
// hook / admin "Analyze now") fires a sweep immediately instead of
// waiting out the periodic interval — and a nudge that arrived DURING
// the settle window is drained by the initial sweep, not double-run.
func TestRunAnalysisSweeperNudgeTriggersImmediateSweep(t *testing.T) {
	oldSettle := analysisSweeperSettleDelay
	analysisSweeperSettleDelay = 5 * time.Millisecond
	t.Cleanup(func() { analysisSweeperSettleDelay = oldSettle })

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := analyze.NewPool(store, 1, 8)
	defer pool.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	nudge := make(chan struct{}, 1)
	status := &sweepStatus[admin.AnalysisSweepCounts]{}

	// A nudge sent BEFORE the sweeper wakes (i.e. during settle) must be
	// swallowed by the initial sweep — asserted below via lastStart count.
	nudge <- struct{}{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 1h interval: the ticker can't fire inside this test — any sweep
		// after the initial one can only come from a nudge.
		runAnalysisSweeper(ctx, store, bridgefs.New([]string{t.TempDir()}), t.TempDir(),
			pool, alwaysAnalysisEnabled, staticInterval(time.Hour), nudge, nil, status)
	}()

	firstEnd := waitForSweep(t, status, time.Time{}, 5*time.Second)

	// Settle-window nudge was drained: give a stray follow-up sweep a
	// moment to (not) happen, then confirm nothing moved.
	time.Sleep(50 * time.Millisecond)
	if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.Equal(firstEnd) {
		t.Fatalf("settle-window nudge triggered an extra sweep: lastEnd %v → %v", firstEnd, lastEnd)
	}

	// A post-settle nudge fires a sweep promptly (no 1h ticker wait).
	nudge <- struct{}{}
	secondEnd := waitForSweep(t, status, firstEnd, 5*time.Second)
	if !secondEnd.After(firstEnd) {
		t.Fatalf("nudge did not trigger a new sweep (lastEnd %v)", secondEnd)
	}

	// Counts recorded for the empty library: everything zero.
	_, _, _, _, last := status.snapshot()
	if last == nil || last.Total != 0 || last.Enqueued != 0 {
		t.Errorf("sweep counts = %+v, want zeroed for empty library", last)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sweeper did not exit on ctx cancel")
	}
}

// TestRunAnalysisSweeperRecordsNextDue — with a periodic interval the
// recorder carries a nextDue in the future; the admin card derives its
// "next sweep in …" countdown from it browser-side.
func TestRunAnalysisSweeperRecordsNextDue(t *testing.T) {
	oldSettle := analysisSweeperSettleDelay
	analysisSweeperSettleDelay = 5 * time.Millisecond
	t.Cleanup(func() { analysisSweeperSettleDelay = oldSettle })

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pool := analyze.NewPool(store, 1, 8)
	defer pool.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := &sweepStatus[admin.AnalysisSweepCounts]{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAnalysisSweeper(ctx, store, bridgefs.New([]string{t.TempDir()}), t.TempDir(),
			pool, alwaysAnalysisEnabled, staticInterval(time.Hour), make(chan struct{}, 1), nil, status)
	}()
	waitForSweep(t, status, time.Time{}, 5*time.Second)
	_, _, _, nextDue, _ := status.snapshot()
	if nextDue.IsZero() || !nextDue.After(time.Now().Add(30*time.Minute)) {
		t.Errorf("nextDue = %v, want ~1h out", nextDue)
	}
	cancel()
	<-done
}

// TestCollectAnalysisCandidatesExcludesUPnPRoutedRows is the end-to-end
// half of the UPnP-exclusion fix, asserted on the numbers the operator
// actually reads on the Jobs page rather than on the store method.
//
// A routed row describes media on an upstream device. It has no local
// file, so `ResolveChecked` misses by construction and every one landed
// in `res.missing` — the sweep reported `total 15372, missing 13553`
// beside a coverage tile correctly reading `totalLocal 89`. Two numbers
// for one library, disagreeing, with the alarming one attached to the
// field that looks like an error count.
//
// The routed fixture path deliberately does NOT exist on disk, which is
// the whole point: pre-fix it is counted as missing; post-fix it is
// never enumerated. The `missing` assertion is therefore the sharp one
// — a fix that merely stopped COUNTING routed rows while still
// resolving them would pass a total-only check.
func TestCollectAnalysisCandidatesExcludesUPnPRoutedRows(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "local.flac"), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "local.flac", Size: 18, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack local: %v", err)
	}
	const routedPath = "Chord 2Go/Music/ABBA/Gold/01 Dancing Queen.flac"
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: routedPath, Size: 42_000_000, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack routed: %v", err)
	}
	if err := store.UpsertUPnPRouting(ctx, &manifest.UPnPRouting{
		SourcePath: routedPath,
		ServerUDN:  "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
		ObjectID:   "64$0$0$0",
		ResURL:     "http://192.168.0.62:8200/MediaItems/25.flac",
		LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertUPnPRouting: %v", err)
	}

	res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.total != 1 {
		t.Errorf("total = %d, want 1 (the filesystem track only) — the sweep "+
			"must describe the same library as the coverage tile", res.total)
	}
	if res.missing != 0 {
		t.Errorf("missing = %d, want 0 — a routed row must never be resolved "+
			"against local disk, let alone reported to the operator as missing",
			res.missing)
	}
	for _, c := range res.candidates {
		if c.SourceLibraryRel == routedPath {
			t.Error("routed track was enqueued for analysis; it has no local file to decode")
		}
	}
}

// TestCollectAnalysisCandidatesRetriesTransientMD5Failure pins the half
// of the transient-MD5 fix that actually causes a retry.
//
// Everything the skip gate normally looks at — mtime, size, schema
// version — is unchanged for a row whose audio-MD5 pass failed for a
// reason that says nothing about the file. So without
// WantsAudioMD5Retry the row is skipped forever, and a one-second I/O
// blip is permanently recorded as "unverifiable", indistinguishable from
// a file that genuinely carries no checksum.
//
// The paired case is the one that keeps this bounded: at the cap the row
// must go quiet again. Each retry is a full re-analysis, so a gate that
// re-opened rows indefinitely would trade a wrong scalar for an hourly
// re-decode of the library.
func TestCollectAnalysisCandidatesRetriesTransientMD5Failure(t *testing.T) {
	root := t.TempDir()
	const name = "track.flac"
	if err := os.WriteFile(filepath.Join(root, name), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: name, Size: info.Size(), ModTime: info.ModTime(),
	}); err != nil {
		t.Fatal(err)
	}

	// A row that is fresh by every other measure.
	base := manifest.AnalysisRow{
		SourcePath:    name,
		WaveformPath:  filepath.Join(t.TempDir(), "wf"),
		WaveformTag:   "deadbeef",
		WaveformSize:  10,
		SourceMTimeNS: info.ModTime().UnixNano(),
		SourceSize:    info.Size(),
		SchemaVersion: analyze.WaveformSchemaVersion,
		CreatedAt:     1,
	}

	isCandidateFor := func(t *testing.T, want string) bool {
		t.Helper()
		res, err := collectAnalysisCandidates(ctx, store, bridgefs.New([]string{root}), t.TempDir(), "", false)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range res.candidates {
			if c.SourceLibraryRel == want {
				return true
			}
		}
		return false
	}
	isCandidate := func(t *testing.T) bool {
		t.Helper()
		return isCandidateFor(t, name)
	}

	// Transient failures, under the cap: must keep being re-enqueued.
	for i := 1; i < manifest.AudioMD5MaxAttempts; i++ {
		row := base
		row.AudioMD5Retryable = true
		if err := store.UpsertAnalysis(ctx, row); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !isCandidate(t) {
			t.Fatalf("after %d transient MD5 failure(s) the track must be "+
				"re-analysed — mtime, size and schema are all unchanged, so "+
				"nothing else in the gate can re-open it", i)
		}
	}

	// At the cap: must go quiet. Each retry is a full re-analysis.
	for i := manifest.AudioMD5MaxAttempts; i <= manifest.AudioMD5MaxAttempts+1; i++ {
		row := base
		row.AudioMD5Retryable = true
		if err := store.UpsertAnalysis(ctx, row); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if isCandidate(t) {
		t.Errorf("at the %d-attempt cap the track must stop being re-enqueued; "+
			"an unbounded retry re-decodes the whole library every sweep for as "+
			"long as the condition lasts", manifest.AudioMD5MaxAttempts)
	}

	// A file that simply cannot be verified must never be re-enqueued at
	// all — there is nothing to learn by asking again.
	//
	// A SECOND track, never previously failed. Reusing the one above
	// would make this vacuous: the loops already drove it to the cap, so
	// it stops being a candidate whatever the permanent branch does —
	// verified by deleting that branch and watching this assertion still
	// pass (CodeRabbit on PR #632). The store-level tests do cover the
	// branch, but an assertion that cannot fail reads as coverage it
	// does not provide.
	const permName = "permanent.flac"
	if err := os.WriteFile(filepath.Join(root, permName), []byte("fLaC-nonzero-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	permInfo, err := os.Stat(filepath.Join(root, permName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: permName, Size: permInfo.Size(), ModTime: permInfo.ModTime(),
	}); err != nil {
		t.Fatal(err)
	}
	permanent := base
	permanent.SourcePath = permName
	permanent.SourceMTimeNS = permInfo.ModTime().UnixNano()
	permanent.SourceSize = permInfo.Size()
	permanent.AudioMD5Retryable = false
	if err := store.UpsertAnalysis(ctx, permanent); err != nil {
		t.Fatal(err)
	}
	if isCandidateFor(t, permName) {
		t.Error("a permanently-unverifiable file must not be re-analysed — " +
			"nothing about it will change until the file itself does")
	}
}
