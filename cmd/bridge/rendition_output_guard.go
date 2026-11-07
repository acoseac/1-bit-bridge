package main

import (
	"context"

	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// renditionOutputGuard is the guard every render asks before it makes or
// writes anything in its variants directory (transcode.OutputGuard, backlog
// B727's first item). It refuses a variants directory that reads as away by
// the probe every reaper asks first (integrity.VariantsDirSweepBlock: not
// there, holding no rendition, or not readable) while the catalog records a
// rendition under it (Store.VariantRecordedUnder): that directory held
// renditions, so it reads as away because its volume is not mounted, and one
// rendition published into the bare mount point read it as mounted to the
// watcher and to a download's reap, which then deleted the rows of the
// renditions on the volume (transcode/output_guard.go). A directory the
// catalog records nothing under, a fresh install's or a variants directory
// just set to a new one, is rendered into as before, and so is one that reads
// as mounted, without a read of the catalog. serve sets it on its pool
// (Pool.SetOutputGuard) and the CLI's worker on each job it runs.
type renditionOutputGuard struct {
	store *manifest.Store
}

// CheckOutput answers transcode.OutputGuard: nil, a
// *transcode.VariantsDirAwayError, or the catalog's failure to answer, which
// refuses too.
func (g renditionOutputGuard) CheckOutput(ctx context.Context, dir string) error {
	if g.store == nil || dir == "" {
		return nil
	}
	block := integrity.VariantsDirSweepBlock(dir)
	if block.Reason == "" {
		return nil
	}
	held, err := g.store.VariantRecordedUnder(ctx, dir)
	if err != nil {
		return err
	}
	if !held {
		return nil
	}
	found := "cannot be read"
	switch {
	case block.Missing:
		found = "is not there"
	case block.Empty:
		found = "holds no rendition"
	}
	return &transcode.VariantsDirAwayError{Found: found}
}
