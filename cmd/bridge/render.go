package main

// `bridge render` — the faithful DSD → PCM tier. Third sibling of
// `bridge upscale` and `bridge optimize`: all three invoke
// `runUpscaleBatch` over the same classifier and worker pool, and the
// kind on `runUpscaleParams` switches the eligibility predicate, the
// target resolver and the JobSpec.Kind that drives `VariantID()`.
//
// Eligibility is `transcode.PCMRenderEligible` — DSF / DFF only, on a
// family rate, not an SACD virtual path, and DST-compressed only when
// this ffmpeg carries the `dst` decoder. The target is
// `TargetRateForPCMRender` (44.1-family → 176400, 48-family → 192000) at
// a uniform 24 bits, mirroring `pcm-v1-<rate>-24`.
//
// The COMPACT tier is not here: `optimized-dsd-v1-<44100|48000>-16` is
// built by `bridge optimize`, which admits DSD sources under the same
// `upscale.dsdRender.enabled` flag. One decode recipe, two commands,
// split by which tier the operator wants — the same split the server has
// between the auto-optimize sweep (compact) and the app's on-demand
// request (faithful).
//
// Operator usage:
//   bridge render                        # every eligible DSD track
//   bridge render --filter "Kind of Blue" # path-scoped
//   bridge render --dry-run              # preview without decoding
//   bridge render --gc                   # shares the upscale GC sweep,
//                                          which is prefix-agnostic and
//                                          preserves every family

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

func renderCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	quality := fs.String("quality", "very-high", "SoX resampler preset (very-high|high|medium)")
	workers := fs.Int("workers", 0, "concurrent render pipelines; 0 = min(NumCPU-1, 4)")
	filter := fs.String("filter", "", "case-sensitive substring filter on track path (empty = all)")
	dryRun := fs.Bool("dry-run", false, "list candidates without rendering")
	force := fs.Bool("force", false, "re-render even if a fresh sidecar already exists")
	gc := fs.Bool("gc", false, "remove orphan sidecars (files with no DB row) AND orphan DB rows (rows with no on-disk sidecar); skips rendering. Shares the upscale GC path, which is prefix-agnostic — every variant family is preserved.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	r, exitCode := bootstrapTranscodeCmd(ctx, stderr, *configPath, *quality, *gc)
	if r == nil {
		return exitCode
	}
	defer r.store.Close()

	if *gc {
		// Prefix-agnostic path-equality against the DB rows, so upscaled-,
		// optimized-, optimized-dsd- and pcm- sidecars are all preserved.
		return runGC(ctx, stdout, stderr, r.store, r.outputDir, r.tempDir)
	}

	// The operator flag first — it is the one the deploy sets, and its
	// absence is a configuration answer, not a toolchain one.
	if !r.cfg.Upscale.DSDRender.Enabled {
		fmt.Fprint(stderr, "DSD → PCM renditions are disabled in bridge.yaml (`upscale.dsdRender.enabled: false`).\n"+
			"Set `upscale.dsdRender.enabled: true` and re-run. Enabling it also lets the\n"+
			"auto-optimize sweep build the compact DSD tier, which reads every DSD source once.\n")
		return 2
	}
	// Then the toolchain. Refusing HERE — before the library walk — is
	// the difference between one honest message and one failed job per
	// DSD track.
	caps, ok := ffmpegDSDCLIReady(ctx, stderr)
	if !ok {
		return 1
	}

	return runUpscaleBatch(ctx, stdout, stderr, r.store, r.cfg, r.resolver, runUpscaleParams{
		// targetRateFlag + targetBits are ignored for pcm: the classifier
		// derives both per track from the source's DSD family. Zero
		// values so a future fall-through surfaces as a regression
		// rather than silently rendering at a default.
		targetRateFlag: "",
		targetBits:     0,
		quality:        r.quality,
		workers:        effectiveWorkerCount(*workers),
		filter:         *filter,
		dryRun:         *dryRun,
		force:          *force,
		outputDir:      r.outputDir,
		kind:           transcode.JobKindPCMRender,
		dsdCaps:        caps,
		tempDir:        r.tempDir,
	})
}
