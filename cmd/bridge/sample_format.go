package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// runSampleFormatBackfill runs pass at once and again after every nudge,
// until ctx ends (backlog B513). pass is one header pass of the sample-format
// backfill (manifest.Store.BackfillSampleFormats), which reads the header of
// each WAV and AIFF row still undecided and finds none to read once the
// backfill is done; the first pass that runs to its end marks the backfill
// complete, which is when /v1/health advertises sampleFormat. serve appends
// nudge to postScanNudges, so a pass follows every successful scan: a row
// whose file could not be read, or had changed, is tried again once the
// library has answered a scan. The pass at once covers a startup scan that
// finished before serve registered its post-scan hook, which sends no nudge,
// and is the one that completes the backfill on the first start.
func runSampleFormatBackfill(ctx context.Context, nudge <-chan struct{}, pass func(context.Context)) {
	for {
		pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		}
	}
}

// logSampleFormatPass reports one header pass, by its counts alone, never a
// file: an Info line when it completed the backfill (once per library) or
// read a header, a Warn when it stopped for a reason other than the shutdown
// that cancelled it, and nothing when it found no row to read, which is every
// pass once the backfill is done.
func logSampleFormatPass(ctx context.Context, res manifest.SampleFormatBackfill, err error) {
	counts := []any{"read", res.Read, "gained", res.Gained, "none", res.None,
		"unreadable", res.Unreadable, "changed", res.Changed}
	if failure := ctxerr.WithoutCancellation(ctx, err); failure != nil {
		logger.Warn("sample format: the header pass stopped; the rows it did not reach are read after the next scan",
			append(counts, "err", failure)...)
		return
	}
	if res.Completed {
		logger.Info("sample format: the backfill is complete; /v1/health advertises sampleFormat", counts...)
		return
	}
	if res.Read == 0 {
		return
	}
	logger.Info("sample format: read the headers of WAV and AIFF rows indexed before the field", counts...)
}
