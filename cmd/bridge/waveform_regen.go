package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// waveformRepairQueue is the analysis pool as the regenerator uses it.
type waveformRepairQueue interface {
	EnqueueRepair(analyze.RepairSpec) error
}

// waveformRegenerator asks for a waveform curve proven missing to be rendered
// again, waveform only (backlog B652): the enqueue the /v1/waveform lookup
// makes (analysisStoreAdapter.LookupWaveform), after every check a whole
// analysis of the source would pass and the catalog's guard.
//
// In order, at the lookup and again when the guard admits a row it was asked
// for while it judged (repairFor):
//
//   - the live analysis gate (analysisActiveFn): analysis off renders
//     nothing, as it analyses nothing;
//   - the row, read again by its own source path, and DecideWaveform's
//     verdict on it again: a curve that came back, or was adopted, is left;
//   - collectAnalysisCandidates' rules for the source (analysisSourceSkip):
//     no DSD, WavPack or Monkey's Audio file, nothing that does not resolve
//     or is a directory, no zero-byte file;
//   - its freshness (analysisRowFresh): only a row the walk skips as fresh
//     is repaired, since nothing else offers it again; a row the walk offers
//     (a changed source, another schema, an audio MD5 still to ask) goes
//     through ordinary analysis, which writes its curve with the rest;
//   - the analysis-failure suppression, as the walk reads it against the
//     live stat (analysisSuppressedNow);
//   - its own stop: two regenerations that failed on the curve's account
//     against this build and this version of the source
//     (manifest.WaveformRegenFailure.StopsFor);
//   - the catalog's guard (integrity.WaveformRegenGuard), which judges the
//     whole catalog before the first regeneration and then at most once an
//     interval, and admits only a row its judgment found to be regenerated.
//
// The job is keyed on the row's source path, so the pool's dedup holds one
// repair (or one analysis) of a source at a time whatever spelling the
// lookups used, and it carries the waveform directory the judgment saw.
type waveformRegenerator struct {
	store    *manifest.Store
	resolver *bridgefs.Resolver
	// enabled is the live analysis gate.
	enabled func() bool
	// dir is the waveform directory now (liveWaveformDir).
	dir   func() string
	queue waveformRepairQueue
	guard *integrity.WaveformRegenGuard
	// build is the build a failed regeneration is counted against.
	build string
	// ctx is the context an admission the guard makes reads the store on.
	ctx context.Context
}

// waveformRegenConfig builds a waveformRegenerator.
type waveformRegenConfig struct {
	store    *manifest.Store
	resolver *bridgefs.Resolver
	enabled  func() bool
	dir      func() string
	queue    waveformRepairQueue
	build    string
	// interval and now are the guard's (zero: integrity's defaults).
	interval time.Duration
	now      func() time.Time
	// onJudged, a test seam, is the guard's.
	onJudged func(integrity.WaveformJudgment)
}

// newWaveformRegenerator builds the regenerator and its guard, whose
// judgments run on a context derived from ctx; Close ends them.
func newWaveformRegenerator(ctx context.Context, c waveformRegenConfig) *waveformRegenerator {
	r := &waveformRegenerator{
		store: c.store, resolver: c.resolver, enabled: c.enabled, dir: c.dir,
		queue: c.queue, build: c.build, ctx: ctx,
	}
	r.guard = integrity.NewWaveformRegenGuard(ctx, integrity.WaveformRegenConfig{
		List:              r.listCatalog,
		Dir:               c.dir,
		Admit:             r.admit,
		Interval:          c.interval,
		MaxMissingPercent: integrity.WaveformRegenMaxMissingPercent,
		Now:               c.now,
		OnJudged:          c.onJudged,
	})
	return r
}

// Close ends the guard's judgments, joined within their grace, and every
// admission after them.
func (r *waveformRegenerator) Close() {
	if r != nil && r.guard != nil {
		r.guard.Close()
	}
}

// listCatalog is the guard's catalog: every row that records a curve.
func (r *waveformRegenerator) listCatalog(ctx context.Context) ([]integrity.WaveformSnapshot, error) {
	rows, err := r.store.RecordedWaveforms(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]integrity.WaveformSnapshot, len(rows))
	for i, w := range rows {
		out[i] = integrity.WaveformSnapshot{SourcePath: w.SourcePath, WaveformPath: w.WaveformPath, SizeBytes: w.WaveformSize}
	}
	return out, nil
}

// request is a /v1/waveform lookup's ask, for the row keyed on sourcePath
// whose curve it proved missing: everything checked, then the guard asked.
// An admitted row is enqueued now; one asked while the guard judges is
// enqueued when its judgment admits it (admit); a refused one is the guard's
// to log, once a streak.
func (r *waveformRegenerator) request(ctx context.Context, sourcePath string) {
	if _, ok := r.repairFor(ctx, sourcePath); !ok {
		return
	}
	if ans := r.guard.Ask(sourcePath); ans.Kind == integrity.WaveformAdmitted {
		r.enqueue(ctx, sourcePath, ans.Dir, ans.Identity)
	}
}

// admit is the guard's Admit: a row asked for while its judgment ran, which
// it admits.
func (r *waveformRegenerator) admit(sourcePath, dir string, identity os.FileInfo) {
	r.enqueue(r.ctx, sourcePath, dir, identity)
}

// enqueue checks everything again, as it stands now, and hands the repair to
// the pool with the directory the judgment saw.
func (r *waveformRegenerator) enqueue(ctx context.Context, sourcePath, dir string, identity os.FileInfo) {
	spec, ok := r.repairFor(ctx, sourcePath)
	if !ok || spec.OutputDir != dir {
		// A row no longer to be repaired, or a waveform directory that is
		// another than the one judged: the next lookup asks again.
		return
	}
	spec.OutputDirIdentity = identity
	switch err := r.queue.EnqueueRepair(spec); {
	case err == nil:
		logger.Info("waveform lookup: rendering a missing curve again",
			slog.String("source_path", sourcePath))
	case errors.Is(err, analyze.ErrDuplicateInflight):
		// Already queued or running: that job renders it.
	default:
		// A full queue or a pool shutting down: the next lookup asks again.
		logger.Debug("waveform lookup: the repair of a missing curve was not queued",
			slog.String("source_path", sourcePath), slog.Any("err", err))
	}
}

// repairFor is the repair of sourcePath's curve, when every check before the
// guard passes (the type's docblock lists them).
func (r *waveformRegenerator) repairFor(ctx context.Context, sourcePath string) (analyze.RepairSpec, bool) {
	if r.enabled == nil || !r.enabled() {
		return analyze.RepairSpec{}, false
	}
	row, err := r.store.GetAnalysis(ctx, sourcePath)
	if err != nil || row == nil || row.WaveformPath == "" {
		return analyze.RepairSpec{}, false
	}
	dir := r.dir()
	d := integrity.DecideWaveform(dir, integrity.WaveformSnapshot{
		SourcePath: row.SourcePath, WaveformPath: row.WaveformPath, SizeBytes: row.WaveformSize,
	})
	if !d.Regenerates() {
		return analyze.RepairSpec{}, false
	}
	skip, abs, info := analysisSourceSkip(row.SourcePath, r.resolver)
	if skip != analysisSourceEligible || !analysisRowFresh(row, info) {
		return analyze.RepairSpec{}, false
	}
	sup, found, err := r.store.SuppressedAnalysisVersion(ctx, row.SourcePath)
	if err != nil || analysisSuppressedNow(sup, found, info) {
		// A read that failed is not "nothing is suppressed": that reading
		// is the unbounded retry the debounce exists to close.
		return analyze.RepairSpec{}, false
	}
	f, err := r.store.GetWaveformRegenFailure(ctx, row.SourcePath)
	if err != nil || f.StopsFor(r.build, row.SourceMTimeNS, row.SourceSize) {
		return analyze.RepairSpec{}, false
	}
	return analyze.RepairSpec{
		AnalyzeSpec: analyze.AnalyzeSpec{
			SourceAbsPath:    abs,
			SourceLibraryRel: row.SourcePath,
			SourceMTimeNS:    row.SourceMTimeNS,
			SourceSize:       row.SourceSize,
			OutputDir:        dir,
		},
		Base:  *row,
		Build: r.build,
	}, true
}

// waveformRegenBuild is the build a failed regeneration is counted against:
// the release this binary is (version.ServerVersion) and, where the build
// recorded one, the commit it was built from, marked when its tree was
// modified. A new release, or a build from another commit, starts every
// curve's count again.
func waveformRegenBuild() string {
	b := version.ServerVersion
	if sha, modified := vcsBuild(); sha != "" {
		b += "+" + sha
		if modified {
			b += "-modified"
		}
	}
	return b
}
