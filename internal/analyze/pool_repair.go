package analyze

import (
	"context"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// repairFailedMsg is the one message every failed waveform-only repair logs,
// whatever its classification, for analyzeFailedMsg's reason: one string to
// grep.
const repairFailedMsg = "analyze: waveform repair failed"

// processRepair runs one waveform-only repair (EnqueueRepair) and returns its
// outcome, for processJob's one deferred finishJob, which counts it and
// releases its path after this has returned, its bookkeeping (the commit, the
// failure's count and its WARN) landed.
//
// Shutdown reads p.closed, as processJob does, so a repair a Stop cancelled
// counts nothing and strikes nothing. The per-job timeout strikes nothing
// either, before the classifier is asked: a hung mount and a pathological
// file look the same from here (processJob's own rule). The rest is
// CountsAgainstTheCurve's.
//
// A failure is counted apart from the analysis-failure debounce: it records
// a regeneration failure against the repair's build and the source's
// version (manifest.Store.RecordWaveformRegenFailure), never an analysis
// strike, so a curve that cannot be rendered again stops after
// manifest.WaveformRegenStopAfter tries while the source stays offered for
// analysis as it was.
func (p *Pool) processRepair(spec RepairSpec) jobOutcome {
	jobCtx, cancel := context.WithTimeout(p.stopCtx, p.jobTimeout)
	defer cancel()

	res, err := p.repairRunner(jobCtx, spec)
	if err == nil {
		// Durability first, as for an analysis: the row must never name a
		// curve a crash can take back. The file is NOT removed on a failure:
		// its path is the curve's canonical one, which a row may name.
		if ferr := p.fsyncFn(res.WaveformPath); ferr != nil {
			err = curveNotWritten{ferr}
		}
	}
	if err != nil {
		if p.closed.Load() {
			return outcomeUncounted
		}
		p.noteRepairFailure(jobCtx, spec, err)
		return outcomeFailed
	}
	bumped, err := p.store.CommitWaveformRepair(jobCtx, spec.Base, res.WaveformPath, res.WaveformTag, res.WaveformSize)
	if err != nil {
		if p.closed.Load() {
			return outcomeUncounted
		}
		if errors.Is(err, manifest.ErrAnalysisChanged) {
			// Another writer moved the row since the repair began (an
			// analysis, an adoption, a deletion): what it wrote stands, and
			// the next lookup judges the row as it is then. No strike: the
			// curve did nothing wrong.
			logger.Info("analyze: waveform repair not recorded, the analysis row changed since it began",
				"path", spec.SourceLibraryRel)
			return outcomeFailed
		}
		logger.Error("analyze: store waveform repair", "path", spec.SourceLibraryRel, "err", err)
		return outcomeFailed
	}
	// DETACHED and bounded, as processJob's clear is: the commit has landed,
	// so the curve is written whatever jobCtx does next.
	clearCtx, clearCancel := context.WithTimeout(context.WithoutCancel(p.stopCtx), failureRecordTimeout)
	defer clearCancel()
	if err := p.store.ClearWaveformRegenFailure(clearCtx, spec.SourceLibraryRel); err != nil {
		logger.Warn("analyze: clear waveform regeneration failures", "path", spec.SourceLibraryRel, "err", err)
	}
	logger.Info("analyze: rendered a missing waveform again",
		"path", spec.SourceLibraryRel, "tagChanged", bumped)
	return outcomeDone
}

// noteRepairFailure logs one failed repair and, when it failed on the curve's
// own account, counts it toward the curve's stop. The count is the log gate,
// as an analysis strike's is: the first counted failure WARNs, the one that
// stops the curve WARNs that it stopped, and a transient failure WARNs every
// time, since it has no record to deduplicate against and a fault that
// repeats is the operator's to see.
func (p *Pool) noteRepairFailure(jobCtx context.Context, spec RepairSpec, err error) {
	if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
		logger.Warn("analyze: waveform repair timed out", "path", spec.SourceLibraryRel, "timeout", p.jobTimeout)
		return
	}
	if !CountsAgainstTheCurve(err) {
		logger.Warn(repairFailedMsg, "path", spec.SourceLibraryRel, "err", err, "counted", false)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.stopCtx), failureRecordTimeout)
	defer cancel()
	n, rerr := p.store.RecordWaveformRegenFailure(ctx, spec.SourceLibraryRel, spec.Build,
		spec.SourceMTimeNS, spec.SourceSize, err.Error())
	switch {
	case rerr != nil:
		logger.Warn(repairFailedMsg, "path", spec.SourceLibraryRel, "err", err, "markerErr", rerr)
	case n == 1:
		logger.Warn(repairFailedMsg, "path", spec.SourceLibraryRel, "err", err,
			"triesLeft", manifest.WaveformRegenStopAfter-1)
	case n == manifest.WaveformRegenStopAfter:
		logger.Warn(repairFailedMsg+"; its regeneration stopped until a new build or a changed source",
			"path", spec.SourceLibraryRel, "err", err, "failures", n)
	default:
		logger.Debug("analyze: waveform repair failed again", "path", spec.SourceLibraryRel, "failures", n, "err", err)
	}
}
