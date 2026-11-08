package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// regenClock is a clock a test moves.
type regenClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *regenClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *regenClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// regenFixture is a library of analysed tracks whose curves sit at their
// canonical places under a waveform directory, the /v1/waveform lookup serve
// wires over them with its regenerator, and an analysis pool whose repair
// runner the test decides (writeRepairedCurve by default: the curve written
// where the spec says, as RunWaveformRepair writes it).
type regenFixture struct {
	t        *testing.T
	ctx      context.Context
	dbPath   string
	lib, dir string
	tracks   []string
	store    *manifest.Store
	resolver *bridgefs.Resolver
	adapter  *analysisStoreAdapter
	regen    *waveformRegenerator
	pool     *analyze.Pool
	clock    *regenClock
	build    string
	on       atomic.Bool

	mu     sync.Mutex
	runner func(ctx context.Context, spec analyze.RepairSpec) (analyze.RepairResult, error)
	specs  []analyze.RepairSpec
}

// seededMeasurements are values no decode of a fixture's source produces: a
// repair that rewrote them would show.
func seededMeasurements(row *manifest.AnalysisRow) {
	rg, peak := -3.25, 0.75
	key, bpm, dr, bw := 9, 171, 3, 15001
	row.ReplayGainTrackDB, row.TruePeakDB = &rg, &peak
	row.KeyRoot, row.KeyMode, row.BPM, row.DRScore, row.BandwidthHz = &key, "minor", &bpm, &dr, &bw
	row.AudioMD5State = "mismatch"
	row.Spectrum = []byte("1BSP-seeded-curve")
}

func tagOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:8]
}

// newRegenFixture builds n analysed tracks (n >= 2: a directory of one
// curve deleted reads as a volume away).
func newRegenFixture(t *testing.T, n int) *regenFixture {
	t.Helper()
	root := t.TempDir()
	f := &regenFixture{
		t: t, ctx: context.Background(),
		dbPath: filepath.Join(root, "data", "bridge.db"),
		lib:    filepath.Join(root, "Music"),
		dir:    analyze.WaveformDirFor(filepath.Join(root, "data")),
		clock:  &regenClock{now: time.Unix(1_800_000_000, 0)},
		build:  "build-a",
	}
	f.on.Store(true)
	store, err := manifest.OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	for i := 1; i <= n; i++ {
		rel := fmt.Sprintf("Artist/Album/%02d One.flac", i)
		f.tracks = append(f.tracks, rel)
		f.addTrack(t, rel, []byte(fmt.Sprintf("audio of track %d", i)))
	}
	f.resolver = bridgefs.New([]string{f.lib})
	f.start(t)
	t.Cleanup(f.stop)
	return f
}

// addTrack writes rel's source, indexes it, writes its curve at its
// canonical place and records an analysis row of it with seeded
// measurements.
func (f *regenFixture) addTrack(t *testing.T, rel string, body []byte) {
	t.Helper()
	src := filepath.Join(f.lib, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertTrack(f.ctx, &manifest.Track{Path: rel, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		t.Fatal(err)
	}
	curve := []byte("1BWF-first-" + rel)
	path := f.canonical(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, curve, 0o600); err != nil {
		t.Fatal(err)
	}
	row := manifest.AnalysisRow{
		SourcePath: rel, WaveformPath: path, WaveformTag: tagOf(curve), WaveformSize: int64(len(curve)),
		SourceMTimeNS: info.ModTime().UnixNano(), SourceSize: info.Size(),
		SchemaVersion: analyze.WaveformSchemaVersion, CreatedAt: 1111,
	}
	seededMeasurements(&row)
	if err := f.store.UpsertAnalysis(f.ctx, row); err != nil {
		t.Fatal(err)
	}
}

func (f *regenFixture) canonical(rel string) string {
	return analyze.AnalyzeSpec{OutputDir: f.dir, SourceLibraryRel: rel}.SidecarPath()
}

// start builds the pool, the regenerator and the lookup over the store, as
// serve wires them.
func (f *regenFixture) start(t *testing.T) {
	t.Helper()
	f.pool = analyze.NewPool(f.store, 1, 16,
		analyze.WithFsync(func(string) error { return nil }),
		analyze.WithLiveGate(f.on.Load),
		analyze.WithRepairRunner(f.runRepair))
	f.regen = newWaveformRegenerator(f.ctx, waveformRegenConfig{
		store: f.store, resolver: f.resolver, enabled: f.on.Load,
		dir: func() string { return f.dir }, queue: f.pool, build: f.build,
		interval: time.Hour, now: f.clock.Now,
	})
	f.adapter = &analysisStoreAdapter{provider: manifest.NewProvider(f.store, nil), store: f.store,
		waveformDir: func() string { return f.dir }, regen: f.regen}
}

func (f *regenFixture) stop() {
	if f.regen != nil {
		f.regen.Close()
	}
	if f.pool != nil {
		f.pool.Stop()
	}
	if f.store != nil {
		_ = f.store.Close()
	}
}

// restart stops everything, reopens the store from its file and starts again
// under build: what a bridge restarted keeps is what the store keeps.
func (f *regenFixture) restart(t *testing.T, build string) {
	t.Helper()
	f.stop()
	store, err := manifest.OpenStore(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.build = store, build
	f.start(t)
}

// writeRepairedCurve is the default repair: the curve written where the spec says.
func writeRepairedCurve(_ context.Context, spec analyze.RepairSpec) (analyze.RepairResult, error) {
	curve := []byte("1BWF-again-" + spec.SourceLibraryRel)
	final := spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
		return analyze.RepairResult{}, err
	}
	if err := os.WriteFile(final, curve, 0o600); err != nil {
		return analyze.RepairResult{}, err
	}
	return analyze.RepairResult{WaveformPath: final, WaveformTag: tagOf(curve), WaveformSize: int64(len(curve))}, nil
}

func (f *regenFixture) runRepair(ctx context.Context, spec analyze.RepairSpec) (analyze.RepairResult, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	run := f.runner
	f.mu.Unlock()
	if run == nil {
		run = writeRepairedCurve
	}
	return run(ctx, spec)
}

func (f *regenFixture) setRunner(run func(context.Context, analyze.RepairSpec) (analyze.RepairResult, error)) {
	f.mu.Lock()
	f.runner = run
	f.mu.Unlock()
}

func (f *regenFixture) repairSpecs() []analyze.RepairSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]analyze.RepairSpec(nil), f.specs...)
}

func (f *regenFixture) deleteCurve(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(f.canonical(rel)); err != nil {
		t.Fatal(err)
	}
}

// lookup is a /v1/waveform request for rel's curve.
func (f *regenFixture) lookup(t *testing.T, rel string) {
	t.Helper()
	if _, err := f.adapter.LookupWaveform(f.ctx, rel); err != nil {
		t.Fatalf("lookup %s: %v", rel, err)
	}
}

// settle waits for the guard's judgment and the admissions it tells, then
// for the pool to have finished every job it took, until the test's
// deadline: on a tree that regenerates nothing, at once.
func (f *regenFixture) settle(t *testing.T) analyze.PoolStats {
	t.Helper()
	if f.regen != nil && f.regen.guard != nil {
		f.regen.guard.Wait()
	}
	deadline := serveGiveUpTime(t)
	for {
		st := f.pool.Stats()
		if st.QueueLen == 0 && st.Inflight == 0 && st.Done+st.Failed+st.Dropped >= st.Enqueued {
			return st
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			t.Fatalf("the analysis pool never settled: %+v", st)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (f *regenFixture) row(t *testing.T, rel string) manifest.AnalysisRow {
	t.Helper()
	row, err := f.store.GetAnalysis(f.ctx, rel)
	if err != nil || row == nil {
		t.Fatalf("row %s: %v %v", rel, row, err)
	}
	return *row
}

// indexedAt reads rel's delta stamp from the store's file.
func (f *regenFixture) indexedAt(t *testing.T, rel string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(f.dbPath, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, rel).Scan(&n); err != nil {
		t.Fatalf("indexed_at of %s: %v", rel, err)
	}
	return n
}

// heldRender renders once hold is closed, or ends with the job's context.
func heldRender(hold <-chan struct{}) func(context.Context, analyze.RepairSpec) (analyze.RepairResult, error) {
	return func(ctx context.Context, spec analyze.RepairSpec) (analyze.RepairResult, error) {
		select {
		case <-hold:
		case <-ctx.Done():
			return analyze.RepairResult{}, ctx.Err()
		}
		return writeRepairedCurve(ctx, spec)
	}
}

// requireMeasurementsAsSeeded requires every column of rel's row but the
// waveform's to be what the fixture seeded.
func (f *regenFixture) requireMeasurementsAsSeeded(t *testing.T, rel, when string) {
	t.Helper()
	got := f.row(t, rel)
	want := got
	seededMeasurements(&want)
	same := func(a, b *float64) bool { return a != nil && b != nil && *a == *b }
	samei := func(a, b *int) bool { return a != nil && b != nil && *a == *b }
	if !same(got.ReplayGainTrackDB, want.ReplayGainTrackDB) || !same(got.TruePeakDB, want.TruePeakDB) ||
		!samei(got.KeyRoot, want.KeyRoot) || got.KeyMode != "minor" || !samei(got.BPM, want.BPM) ||
		!samei(got.DRScore, want.DRScore) || !samei(got.BandwidthHz, want.BandwidthHz) ||
		got.AudioMD5State != "mismatch" || got.AudioMD5Attempts != 0 || string(got.Spectrum) != "1BSP-seeded-curve" ||
		got.SchemaVersion != analyze.WaveformSchemaVersion || got.CreatedAt != 1111 {
		t.Errorf("%s: the row's measurements are not as seeded: %+v", when, got)
	}
}

// countedFailure is a repair that rendered the curve and could not write it.
func countedFailure(context.Context, analyze.RepairSpec) (analyze.RepairResult, error) {
	return analyze.RepairResult{}, fmt.Errorf("%w: no space left on device", analyze.ErrCurveNotWritten)
}

// TestADeletedCurveIsRenderedAgainOnItsLookupKeepingItsMeasurements: a curve
// deleted from its row's place is rendered again on its track's next
// /v1/waveform lookup, enqueued exactly once however many lookups ask while
// it runs, waveform only: the row's measurements, values no decode of the
// source would produce, survive the repair that succeeds and the one that
// fails, and the curve's tag moving advances indexed_at. Red on 3d0a0d05,
// which renders nothing again: the curve stays missing and the lookups
// enqueue nothing.
func TestADeletedCurveIsRenderedAgainOnItsLookupKeepingItsMeasurements(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	f.deleteCurve(t, rel)
	before := f.indexedAt(t, rel)

	hold := make(chan struct{})
	f.setRunner(heldRender(hold))
	f.lookup(t, rel)
	f.regen.guard.Wait()
	// Asked again while the repair runs, under the spellings a phone sends:
	// one job renders it.
	f.lookup(t, rel)
	f.lookup(t, strings.ToLower(rel))
	close(hold)
	st := f.settle(t)
	if st.Enqueued != 1 || st.Done != 1 {
		t.Fatalf("pool = %+v, want the curve's repair enqueued exactly once and done", st)
	}
	row := f.row(t, rel)
	curve, err := os.ReadFile(f.canonical(rel))
	if err != nil {
		t.Fatalf("the curve is not where its row says after the repair: %v", err)
	}
	if row.WaveformPath != f.canonical(rel) || row.WaveformTag != tagOf(curve) || row.WaveformSize != int64(len(curve)) {
		t.Errorf("row's waveform = %s %s %d, want the curve rendered again", row.WaveformPath, row.WaveformTag, row.WaveformSize)
	}
	f.requireMeasurementsAsSeeded(t, rel, "after a repair that succeeded")
	if after := f.indexedAt(t, rel); after <= before {
		t.Errorf("indexed_at %d after a repair whose tag changed, was %d: clients would keep a curve no longer served", after, before)
	}

	// Present now: a lookup asks for nothing.
	f.lookup(t, rel)
	if st := f.settle(t); st.Enqueued != 1 {
		t.Errorf("a lookup of a curve that is there enqueued a repair (%+v)", st)
	}

	// And a repair that fails leaves the measurements as they are, and is
	// counted once.
	f.setRunner(countedFailure)
	f.deleteCurve(t, rel)
	f.lookup(t, rel)
	st = f.settle(t)
	if st.Enqueued != 2 || st.Failed != 1 {
		t.Fatalf("pool = %+v, want the second repair enqueued once and failed", st)
	}
	f.requireMeasurementsAsSeeded(t, rel, "after a repair that failed")
	if rec, err := f.store.GetWaveformRegenFailure(f.ctx, rel); err != nil || rec == nil || rec.Failures != 1 {
		t.Errorf("record after one counted failure = %+v (%v), want one failure", rec, err)
	}
}

// TestTwoCountedFailuresStopTheCurveAcrossARestart: two repairs that failed
// on the curve's own account stop its regeneration, a restart included, until
// the bridge is another build or the source another version. Red on
// 3d0a0d05, which neither repairs nor counts.
func TestTwoCountedFailuresStopTheCurveAcrossARestart(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	f.setRunner(countedFailure)
	f.deleteCurve(t, rel)
	for i := 1; i <= 3; i++ {
		f.lookup(t, rel)
		f.settle(t)
	}
	if st := f.pool.Stats(); st.Enqueued != 2 || st.Failed != 2 {
		t.Fatalf("pool = %+v after three lookups, want exactly two repairs, the third lookup stopped", st)
	}
	if rec, err := f.store.GetWaveformRegenFailure(f.ctx, rel); err != nil || !rec.StopsFor(f.build, f.row(t, rel).SourceMTimeNS, f.row(t, rel).SourceSize) {
		t.Fatalf("record = %+v (%v), want two failures that stop %s", rec, err, f.build)
	}

	f.restart(t, "build-a")
	f.lookup(t, rel)
	if st := f.settle(t); st.Enqueued != 0 {
		t.Fatalf("after a restart under the same build the stopped curve was enqueued again (%+v)", st)
	}

	f.setRunner(nil)
	f.restart(t, "build-b")
	f.lookup(t, rel)
	if st := f.settle(t); st.Enqueued != 1 || st.Done != 1 {
		t.Fatalf("under a new build the curve was not rendered again (%+v)", st)
	}
	if rec, err := f.store.GetWaveformRegenFailure(f.ctx, rel); err != nil || rec != nil {
		t.Errorf("the record stands after the repair succeeded: %+v (%v)", rec, err)
	}
}

// TestAStoppedCurveOfAChangedSourceIsAnalysedAgain: a stop counted against
// one version of the source holds nothing once the source is another version
// and its row is analysed again for it.
func TestAStoppedCurveOfAChangedSourceIsAnalysedAgain(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	f.setRunner(countedFailure)
	f.deleteCurve(t, rel)
	for i := 0; i < 2; i++ {
		f.lookup(t, rel)
		f.settle(t)
	}
	// The source changes and an ordinary analysis records the new version
	// (a curve and the rest); its curve is then deleted too.
	f.setRunner(nil)
	src := filepath.Join(f.lib, filepath.FromSlash(rel))
	if err := os.WriteFile(src, []byte("audio of track 1, retagged"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	next := f.row(t, rel)
	next.SourceMTimeNS, next.SourceSize, next.CreatedAt = info.ModTime().UnixNano(), info.Size(), 2222
	if err := f.store.UpsertAnalysis(f.ctx, next); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	f.lookup(t, rel)
	if st := f.settle(t); st.Enqueued != 3 || st.Done != 1 {
		t.Fatalf("pool = %+v, want the new version's curve rendered again once its source changed", st)
	}
}

// TestAPartialCopyAtTheCanonicalPlaceIsReplaced: a row whose recorded path
// is gone and whose canonical place holds a file of another size (a copy cut
// short) is repaired once, its curve rendered at the canonical place. Red on
// 3d0a0d05. The atomic replacement itself is analyze's
// TestTheRepairReplacesAPartialCopyAtomically.
func TestAPartialCopyAtTheCanonicalPlaceIsReplaced(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	old := filepath.Join(t.TempDir(), "old-host", "waveforms", "01.waveform.bin")
	if err := f.store.UpdateAnalysisWaveformPath(f.ctx, rel, old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.canonical(rel), []byte("cut"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.lookup(t, rel)
	st := f.settle(t)
	specs := f.repairSpecs()
	if st.Enqueued != 1 || st.Done != 1 || len(specs) != 1 || specs[0].SidecarPath() != f.canonical(rel) {
		t.Fatalf("pool = %+v, specs = %d: want one repair rendering the canonical place", st, len(specs))
	}
	if row := f.row(t, rel); row.WaveformPath != f.canonical(rel) {
		t.Errorf("row's waveform path = %s, want the canonical place", row.WaveformPath)
	}
}

// TestARepairCarriesTheWaveformDirectoryItsJudgmentSaw: the job is keyed on
// the row's own source path and carries the waveform directory's identity as
// the judgment saw it, which the repair asks again before it writes.
func TestARepairCarriesTheWaveformDirectoryItsJudgmentSaw(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	f.deleteCurve(t, rel)
	f.lookup(t, strings.ToUpper(rel))
	f.settle(t)
	specs := f.repairSpecs()
	if len(specs) != 1 {
		t.Fatalf("%d repairs, want 1", len(specs))
	}
	now, err := fsutil.DirIdentity(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].SourceLibraryRel != rel || specs[0].OutputDir != f.dir || !os.SameFile(specs[0].OutputDirIdentity, now) {
		t.Errorf("repair = %s in %s (identity %v), want the row's own path in the waveform directory the judgment saw",
			specs[0].SourceLibraryRel, specs[0].OutputDir, specs[0].OutputDirIdentity)
	}
	if specs[0].Base.SourcePath != rel || specs[0].Build != f.build {
		t.Errorf("repair base %q build %q, want the row and the build", specs[0].Base.SourcePath, specs[0].Build)
	}
}

// requireNothingRegenerated requires no repair queued or run and no curve
// written for rel.
func (f *regenFixture) requireNothingRegenerated(t *testing.T, rel, why string) {
	t.Helper()
	st := f.settle(t)
	if st.Enqueued != 0 || len(f.repairSpecs()) != 0 {
		t.Errorf("%s: %d repair(s) enqueued, %d run; want none", why, st.Enqueued, len(f.repairSpecs()))
	}
	if _, err := os.Lstat(f.canonical(rel)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: a curve is at %s (%v), want none written", why, f.canonical(rel), err)
	}
}

// TestNoCurveIsRenderedAgainOverAWaveformDirectoryGoneAwayOrEmptied: the
// catalog's guard refuses while the waveform directory is not there, holds no
// curve (a volume that is away, the local directory under its mountpoint
// again; or the last curve deleted), or has lost more than a fifth of the
// catalog's curves while it holds others. No repair is queued or run and no
// curve or directory written, one lookup at a time. A guard, green on
// 3d0a0d05; its negative controls (the probe or the ratio left out) show the
// repairs.
func TestNoCurveIsRenderedAgainOverAWaveformDirectoryGoneAwayOrEmptied(t *testing.T) {
	t.Run("the directory removed", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		if err := os.RemoveAll(f.dir); err != nil {
			t.Fatal(err)
		}
		f.lookup(t, f.tracks[0])
		f.requireNothingRegenerated(t, f.tracks[0], "directory removed")
		if _, err := os.Lstat(f.dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the waveform directory was made again (%v)", err)
		}
	})
	t.Run("a volume away", func(t *testing.T) {
		f := newRegenFixture(t, 3)
		if err := os.Rename(f.dir, f.dir+".volume"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		f.lookup(t, f.tracks[0])
		f.requireNothingRegenerated(t, f.tracks[0], "volume away")
		if left, err := os.ReadDir(f.dir); err != nil || len(left) != 0 {
			t.Errorf("the mountpoint's own directory holds %v (%v), want nothing made there", left, err)
		}
	})
	t.Run("the last curve deleted", func(t *testing.T) {
		f := newRegenFixture(t, 2)
		f.deleteCurve(t, f.tracks[1])
		f.deleteCurve(t, f.tracks[0])
		f.lookup(t, f.tracks[0])
		f.requireNothingRegenerated(t, f.tracks[0], "last curve deleted")
	})
	t.Run("emptied past the threshold", func(t *testing.T) {
		f := newRegenFixture(t, 30)
		for _, rel := range f.tracks[:11] {
			f.deleteCurve(t, rel)
		}
		for _, rel := range f.tracks[:11] {
			f.lookup(t, rel)
		}
		f.requireNothingRegenerated(t, f.tracks[0], "emptied")
	})
}

// TestMixedCaseConcurrentLookupsEnqueueOnce: lookups of one deleted curve
// under several spellings at once ask for one repair: the job is keyed on the
// row's own source path, and the pool and the guard hold it once. A guard,
// green on 3d0a0d05 (which enqueues nothing); its negative control (the
// pool's dedup removed) shows the second job.
func TestMixedCaseConcurrentLookupsEnqueueOnce(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	f.deleteCurve(t, rel)
	hold := make(chan struct{})
	f.setRunner(heldRender(hold))
	spellings := []string{rel, strings.ToLower(rel), strings.ToUpper(rel), "/" + strings.ToLower(rel)}
	var wg sync.WaitGroup
	for round := 0; round < 3; round++ {
		for _, p := range spellings {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				_, _ = f.adapter.LookupWaveform(f.ctx, p)
			}(p)
		}
		wg.Wait()
		f.regen.guard.Wait()
	}
	if st := f.pool.Stats(); st.Enqueued > 1 {
		t.Errorf("%d repairs enqueued for one curve asked under %d spellings, want at most one", st.Enqueued, len(spellings))
	}
	close(hold)
	if st := f.settle(t); st.Enqueued > 1 {
		t.Errorf("%d repairs enqueued, want at most one", st.Enqueued)
	}
}

// TestNoCurveIsRenderedAgainWithAnalysisOff: the live analysis gate closed,
// a lookup of a deleted curve renders nothing and judges nothing. A guard,
// green on 3d0a0d05.
func TestNoCurveIsRenderedAgainWithAnalysisOff(t *testing.T) {
	f := newRegenFixture(t, 2)
	f.on.Store(false)
	f.deleteCurve(t, f.tracks[0])
	f.lookup(t, f.tracks[0])
	f.requireNothingRegenerated(t, f.tracks[0], "analysis off")
	if _, judged := f.regen.guard.Last(); judged {
		t.Error("a lookup with analysis off made the guard judge the catalog")
	}
}

// TestNoCurveOfASourceTheAnalysisSkipsIsRenderedAgain: collectAnalysisCandidates'
// rules hold for a repair, the fresh-row skip alone set aside: no DSD,
// WavPack or Monkey's Audio file, no zero-byte file, no source the decoders'
// verdicts suppress, and no source that changed since its row (ordinary
// analysis is its way back). A guard, green on 3d0a0d05.
func TestNoCurveOfASourceTheAnalysisSkipsIsRenderedAgain(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		body []byte
		prep func(t *testing.T, f *regenFixture, rel string)
	}{
		{name: "a DSF file", rel: "Artist/Album/03 DSD.dsf", body: []byte("DSD ")},
		{name: "a DFF file", rel: "Artist/Album/03 DSD.dff", body: []byte("FRM8")},
		{name: "a WavPack file", rel: "Artist/Album/03 Pack.wv", body: []byte("wvpk")},
		{name: "a Monkey's Audio file", rel: "Artist/Album/03 Monkey.ape", body: []byte("MAC ")},
		{name: "a zero-byte file", rel: "Artist/Album/03 Empty.flac", body: []byte{}},
		{name: "a suppressed source", rel: "Artist/Album/03 Broken.flac", body: []byte("truncated"),
			prep: func(t *testing.T, f *regenFixture, rel string) {
				for i := 0; i < manifest.AnalysisFailureThreshold(); i++ {
					if _, err := f.store.RecordAnalysisFailure(f.ctx, rel, "sox: truncated"); err != nil {
						t.Fatal(err)
					}
				}
			}},
		{name: "a source that changed since its row", rel: "Artist/Album/03 Retagged.flac", body: []byte("first"),
			prep: func(t *testing.T, f *regenFixture, rel string) {
				src := filepath.Join(f.lib, filepath.FromSlash(rel))
				if err := os.WriteFile(src, []byte("retagged, and longer"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegenFixture(t, 2)
			f.addTrack(t, tc.rel, tc.body)
			if tc.prep != nil {
				tc.prep(t, f, tc.rel)
			}
			f.deleteCurve(t, tc.rel)
			f.lookup(t, tc.rel)
			f.requireNothingRegenerated(t, tc.rel, tc.name)
		})
	}
}

// TestARelocatedCurveIsAdoptedNotRenderedAgain: a curve at its canonical
// place, of the recorded size, under a row that records another place, is
// adopted by the lookup as before (#954), and no repair is queued. A guard,
// green on 3d0a0d05; its negative control (a relocation read as missing)
// shows the repair.
func TestARelocatedCurveIsAdoptedNotRenderedAgain(t *testing.T) {
	f := newRegenFixture(t, 2)
	rel := f.tracks[0]
	old := filepath.Join(t.TempDir(), "old-host", "waveforms", "01.waveform.bin")
	if err := f.store.UpdateAnalysisWaveformPath(f.ctx, rel, old); err != nil {
		t.Fatal(err)
	}
	rec, err := f.adapter.LookupWaveform(f.ctx, rel)
	if err != nil || rec == nil || rec.WaveformPath != f.canonical(rel) {
		t.Fatalf("lookup = %+v (%v), want the canonical place served", rec, err)
	}
	if row := f.row(t, rel); row.WaveformPath != f.canonical(rel) {
		t.Errorf("row = %s, want the relocated curve adopted", row.WaveformPath)
	}
	if st := f.settle(t); st.Enqueued != 0 {
		t.Errorf("a relocated curve was queued for a repair (%+v)", st)
	}
}

// TestASpectrumLookupRendersNothing: /v1/spectrum's lookup (LookupAnalysis)
// asks for no curve to be rendered again, a deleted one included. A guard,
// green on 3d0a0d05.
func TestASpectrumLookupRendersNothing(t *testing.T) {
	f := newRegenFixture(t, 2)
	f.deleteCurve(t, f.tracks[0])
	if _, err := f.adapter.LookupAnalysis(f.ctx, f.tracks[0]); err != nil {
		t.Fatal(err)
	}
	f.requireNothingRegenerated(t, f.tracks[0], "spectrum lookup")
}

// TestTransientRepairFailuresEarnNoStrikeThroughTheLookup: a repair the
// lookup asked for that fails for a reason that is not the curve's (the
// waveform directory gone since the judgment, the source changed or
// unreachable, a decoder that could not run) records nothing, so the next
// lookup asks again. A guard, green on 3d0a0d05.
func TestTransientRepairFailuresEarnNoStrikeThroughTheLookup(t *testing.T) {
	for _, err := range []error{analyze.ErrWaveformDirAway, analyze.ErrSourceChanged, analyze.ErrSourceUnreachable,
		errors.New(`start sox: exec: "sox": executable file not found`)} {
		t.Run(err.Error(), func(t *testing.T) {
			f := newRegenFixture(t, 2)
			f.setRunner(func(context.Context, analyze.RepairSpec) (analyze.RepairResult, error) {
				return analyze.RepairResult{}, err
			})
			f.deleteCurve(t, f.tracks[0])
			f.lookup(t, f.tracks[0])
			f.settle(t)
			if rec, gerr := f.store.GetWaveformRegenFailure(f.ctx, f.tracks[0]); gerr != nil || rec != nil {
				t.Errorf("a transient failure was counted: %+v (%v)", rec, gerr)
			}
			f.requireMeasurementsAsSeeded(t, f.tracks[0], "after a transient failure")
		})
	}
}

// TestTheDoctorWordsEveryRefusalKind: every refusal the guard can make is one
// the doctor words, spelled alike in both packages.
func TestTheDoctorWordsEveryRefusalKind(t *testing.T) {
	kinds := integrity.WaveformRefusalKinds()
	worded := map[string]bool{}
	for _, k := range doctor.WaveformRefusalKinds() {
		worded[k] = true
	}
	if len(worded) != len(kinds) {
		t.Errorf("the doctor words %d refusal kinds, the guard makes %d", len(worded), len(kinds))
	}
	for _, k := range kinds {
		if !worded[string(k)] {
			t.Errorf("the guard refuses as %q, which the doctor does not word", k)
		}
	}
}
