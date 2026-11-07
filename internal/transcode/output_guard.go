package transcode

import (
	"context"
	"errors"
	"fmt"
)

// A render's guard on its variants directory (backlog B727's first item).
//
// # The defect this exists for
//
// A variants directory on a volume mounted after the bridge starts is, while
// the volume is away, the bare mount point or a directory that is not there
// below it. A render made it there (Run's MkdirAll of the sidecar's directory)
// and published a rendition into it, on the disk under the mount point. The
// mount-loss probe every reaper asks first (integrity.VariantsDirSweepBlock)
// reads a directory holding a rendition as mounted, so that one file let the
// reapers through: the watcher's tick deleted the rows of every rendition on
// the volume below the mass-delete floor of ten rows, and a download's reap
// deleted the row of each rendition a client asked for, at any size of
// catalog, each with an upscale.deleted event (measured through the CLI's
// worker and serve's reapers, cmd/bridge's variants_away_render_unix_test.go).
//
// # The rule
//
// A render writes nothing into a variants directory that reads as away, by the
// reapers' own probe, while the catalog records renditions in it: the
// directory held renditions, so its reading as away is a volume not mounted,
// and a render there would make the evidence that defeats the probe. The job
// asks its OutputGuard first thing in Run, before a decoder probe, a scratch or
// a claim, and its refusal ends the job as a fault of the output side
// (outputAway), which strikes no source and is reported once per outage
// (output_fault.go). A catalog that records no rendition there (a fresh
// install, or a variants directory just set to a new one) is no evidence, and
// the render makes the directory as before. The guard lives in cmd/bridge,
// which can ask both the probe and the catalog; this package imports neither
// integrity, which imports it, nor a store of its own for the question.

// OutputGuard says whether a render may write into its variants directory.
type OutputGuard interface {
	// CheckOutput returns nil when a render may make or write anything in
	// the variants directory dir, and its refusal otherwise: a
	// *VariantsDirAwayError, or the error that kept it from deciding.
	CheckOutput(ctx context.Context, dir string) error
}

// VariantsDirAwayError is an OutputGuard's refusal of a render into a
// variants directory that reads as a volume not mounted while the catalog
// records renditions in it. It names the directory by its role, never by its
// path (the B273 rule: it reaches the batch row the app reads).
type VariantsDirAwayError struct {
	// Found is what the probe found, as words after "it": "is not there",
	// "holds no rendition", "cannot be read".
	Found string
}

func (e *VariantsDirAwayError) Error() string {
	return "the variants directory (upscale.variantsDir) reads as a volume that is not mounted: it " + e.Found +
		" while the catalog records renditions in it, so nothing is rendered into it until it reads as it did; " +
		"if its renditions were removed on purpose, `bridge upscale --gc --allow-mass-delete` drops their rows"
}

// checkOutput asks the job's OutputGuard whether the render may write into its
// variants directory, and marks a refusal, or a guard that could not decide,
// as a fault of the output side: a fact about this host, never about the
// source. Nil when the job carries no guard.
func (j JobSpec) checkOutput(ctx context.Context) error {
	if j.OutputGuard == nil {
		return nil
	}
	err := j.OutputGuard.CheckOutput(ctx, j.OutputDir)
	if err == nil {
		return nil
	}
	reason := "the variants directory could not be checked"
	var away *VariantsDirAwayError
	if errors.As(err, &away) {
		reason = "the variants directory " + away.Found
	}
	return &outputFaultError{
		fault: outputFault{where: outputVariants, kind: outputAway, dir: j.OutputDir, reason: reason},
		err:   fmt.Errorf("not rendered: %w", err),
	}
}
