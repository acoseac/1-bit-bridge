package main

// `bridge optimize` — CarPlay-targeted batch downsample. Sibling to
// `bridge upscale`. Both invoke `runUpscaleBatch` over the same
// classifier/worker infrastructure; the kind discriminator on
// `runUpscaleParams` switches the eligibility predicate, the
// target-rate resolver, and the JobSpec.Kind that drives `VariantID()`.
//
// Eligibility is `transcode.OptimizeEligible` (PCM hi-res only, with
// legacy-codec fallback for pre-codec-column rows). The target rate
// is family-preserving via `transcode.TargetRateForOptimize` —
// 44.1k family → 44.1k, 48k family → 48k — so SoX stays on the
// integer-ratio fast path (`192k → 48k` is exact /4, no fractional
// resample CPU). Target bits is uniformly 16 (CarPlay floor).
//
// Operator usage:
//   bridge optimize                       # all hi-res PCM tracks
//   bridge optimize --filter Krall       # path-scoped
//   bridge optimize --dry-run            # preview without running
//   bridge optimize --gc                 # reuses upscale's GC (path-
//                                          equality-against-DB-row, so
//                                          optimize sidecars are preserved
//                                          alongside upscale sidecars).

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

func optimizeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("optimize", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	quality := fs.String("quality", "very-high", "SoX resampler preset (very-high|high|medium)")
	workers := fs.Int("workers", 0, "concurrent sox processes; 0 = min(NumCPU-1, 4)")
	filter := fs.String("filter", "", "case-sensitive substring filter on track path (empty = all)")
	dryRun := fs.Bool("dry-run", false, "list candidates without converting")
	force := fs.Bool("force", false, "re-convert even if a fresh sidecar already exists")
	gc := fs.Bool("gc", false, "remove orphan sidecars (files with no DB row) AND orphan DB rows (rows with no on-disk sidecar); skips conversion. Shares the upscale GC path — preserves BOTH optimized-* and upscaled-* sidecars.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Bootstrap-time gate: `cfg.Upscale.EffectiveOptimizeEnabled()`
	// can only be checked after config load, so the
	// optimize-specific opt-out happens here rather than inside
	// `bootstrapTranscodeCmd` (which is shared with `upscale`).
	// Pre-load the config just to read the gate; the rest of the
	// bootstrap re-loads via the same helper — cheap (single YAML
	// file decode) and keeps the shared helper signature simple.
	r, exitCode := bootstrapTranscodeCmd(ctx, stderr, *configPath, *quality, *gc)
	if r == nil {
		return exitCode
	}
	defer r.store.Close()
	if !r.cfg.Upscale.EffectiveOptimizeEnabled() {
		fmt.Fprint(stderr, "CarPlay optimization is disabled in bridge.yaml (`upscale.optimizeEnabled: false`).\n"+
			"Set `upscale.optimizeEnabled: true` (or remove the line — it defaults to true) and re-run.\n")
		return 2
	}

	if *gc {
		// Same GC sweep — path-equality against the DB rows is
		// prefix-agnostic, so both upscaled-* and optimized-* rows
		// are preserved together.
		return runGC(ctx, stdout, stderr, r.store, r.outputDir)
	}

	return runUpscaleBatch(ctx, stdout, stderr, r.store, r.cfg, r.resolver, runUpscaleParams{
		// targetRateFlag + targetBits are ignored for optimize
		// (classifier branches on Kind and derives per-track).
		// Pass zero values to surface a future regression if the
		// branch ever falls through.
		targetRateFlag: "",
		targetBits:     0,
		quality:        r.quality,
		workers:        effectiveWorkerCount(*workers),
		filter:         *filter,
		dryRun:         *dryRun,
		force:          *force,
		outputDir:      r.outputDir,
		kind:           transcode.JobKindOptimize,
	})
}
