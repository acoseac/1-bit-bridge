// `bridge upscale` CLI subcommand: walks the manifest store for
// PCM tracks below the target rate and runs SoX to produce sidecar
// FLACs cached under `upscale.variantsDir` (falling back to
// <dataDir>/transcoded/ when that setting is empty — same resolution
// the serve-side pool uses, via config.UpscaleConfig.EffectiveVariantsDir).
// Records each conversion in `track_variants` so the manifest provider
// can advertise the new variants on the next iOS sync.
//
// Two modes:
//   - default: enqueue every eligible track (or filtered subset
//     via `--filter`), run them through a worker pool, exit.
//   - `--dry-run`: print the candidate list without invoking sox.
//
// Maintenance modes:
//   - `--gc`: symmetric garbage collection. Walks the on-disk
//     variants directory (`upscale.variantsDir` or the default
//     `<dataDir>/transcoded/`) and removes files with no
//     matching DB row (forward sweep — orphan file recovery), AND
//     walks `track_variants` rows and removes rows whose
//     `sidecar_path` does not exist on disk (reverse sweep — orphan
//     row recovery). The reverse sweep closes the "phantom variant"
//     loop where the bridge advertises a variant in `/v1/manifest`,
//     iOS clients persist the ID, then every play attempt hits
//     `410 Gone` on `/v1/download` because the sidecar was already
//     removed (e.g. by a prior upscale generation pass that
//     replaced v1 with v2 files but didn't migrate the DB rows).
//     See PROJECT CLAUDE.md "DeleteTrack sidecar-cleanup contract".
//
// Feature gate: refuses to run when `cfg.Upscale.Enabled == false`
// — operators must explicitly opt in via bridge.yaml. Sox-on-PATH
// probe runs at startup; missing sox produces a friendly error
// with install hints per OS.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// gcInterruptedMessage is the stderr line printed when an upscale GC
// pass is cancelled via ctx (Ctrl+C, parent shutdown). Three call sites
// — forward sweep, reverse sweep, and the per-row inner loop.
const gcInterruptedMessage = "GC interrupted"

// transcodeBootstrapResult bundles the prepared state shared by the
// `upscale` and `optimize` CLI subcommands: validated quality preset,
// loaded config, opened manifest store, output directory, and the
// bridgefs resolver. Caller is responsible for `store.Close()` via
// defer.
type transcodeBootstrapResult struct {
	cfg       *config.Config
	store     *manifest.Store
	quality   transcode.Quality
	outputDir string
	resolver  *bridgefs.Resolver
}

// bootstrapTranscodeCmd runs the shared CLI scaffolding both
// `upscaleCmd` and `optimizeCmd` need: config load + upscale feature
// gate + sox precheck (gated on `gcMode`) + quality validation +
// manifest store open + outputDir resolve + bridgefs resolver
// construction. Returns the populated result + 0 on success, or
// `(nil, exitCode)` on any failure (caller `return exitCode`s).
//
// `gcMode == true` skips the sox precheck — GC sweeps only consult
// the DB and the filesystem, no sox required.
func bootstrapTranscodeCmd(ctx context.Context, stderr io.Writer, configPath, qualityFlag string, gcMode bool) (*transcodeBootstrapResult, int) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return nil, 2
	}

	// Feature flag gate. Disabled bridges refuse to run the CLI
	// even if all the inputs are valid — operators must consciously
	// opt in.
	if !cfg.Upscale.Enabled {
		fmt.Fprint(stderr, "Transcoding (upscale + optimize) is disabled in bridge.yaml.\n"+
			"Set `upscale.enabled: true` and restart `bridge serve`, then re-run this command.\n")
		return nil, 2
	}

	// SoX-on-PATH + FLAC probe. The pure-function part of the feature
	// (manifest queries, GC) doesn't need sox, so we defer this check
	// until just before a conversion would actually fire.
	if !gcMode && !soxCLIReady(ctx, stderr, "the upscaler needs for its internal pipeline") {
		return nil, 1
	}

	q := transcode.Quality(qualityFlag)
	switch q {
	case transcode.QualityVeryHigh, transcode.QualityHigh, transcode.QualityMedium:
	default:
		fmt.Fprintf(stderr, "invalid --quality %q (want very-high|high|medium)\n", qualityFlag)
		return nil, 2
	}

	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return nil, 1
	}
	// Build a Resolver from the config roots. Same routing engine
	// the api uses for /v1/list / /v1/download — handles
	// single-root (rootless paths) AND multi-root (basename-
	// prefixed paths) uniformly. Pre-fix, the CLI had its own
	// hand-rolled basename-stripping helper that silently returned
	// "" for every track in single-root layouts (CodeRabbit
	// second-pass on PR #108).
	return &transcodeBootstrapResult{
		cfg:     cfg,
		store:   store,
		quality: q,
		// Honor `upscale.variantsDir` so the CLI writes sidecars to the
		// SAME location the serve-side pool uses (e.g. a relocated /
		// network-mounted variants dir on a host whose data disk is too
		// small for the full variant set). EffectiveVariantsDir falls
		// through to `<dataDir>/transcoded/` when the field is unset, so
		// installs without the setting are byte-for-byte unchanged. The
		// `--gc` walker shares this outputDir too, so cleanup stays
		// consistent with where conversions actually land.
		outputDir: cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
		resolver:  bridgefs.New(cfg.LibraryRoots),
	}, 0
}

func upscaleCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upscale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	targetRate := fs.String("target-rate", "auto", "output sample rate in Hz, or 'auto' (44.1-family→176400, 48-family→192000)")
	targetBits := fs.Int("target-bits", 24, "output bit depth (16/24/32)")
	quality := fs.String("quality", "very-high", "SoX resampler preset (very-high|high|medium)")
	workers := fs.Int("workers", 0, "concurrent sox processes; 0 = min(NumCPU-1, 4)")
	filter := fs.String("filter", "", "case-sensitive substring filter on track path (empty = all)")
	dryRun := fs.Bool("dry-run", false, "list candidates without converting")
	force := fs.Bool("force", false, "re-convert even if a fresh sidecar already exists")
	gc := fs.Bool("gc", false, "remove orphan sidecars (files with no DB row) AND orphan DB rows (rows with no on-disk sidecar); skips conversion")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *targetBits != 16 && *targetBits != 24 && *targetBits != 32 {
		fmt.Fprintf(stderr, "invalid --target-bits %d (want 16/24/32)\n", *targetBits)
		return 2
	}

	r, exitCode := bootstrapTranscodeCmd(ctx, stderr, *configPath, *quality, *gc)
	if r == nil {
		return exitCode
	}
	defer r.store.Close()

	if *gc {
		return runGC(ctx, stdout, stderr, r.store, r.outputDir)
	}
	return runUpscaleBatch(ctx, stdout, stderr, r.store, r.cfg, r.resolver, runUpscaleParams{
		targetRateFlag: *targetRate,
		targetBits:     *targetBits,
		quality:        r.quality,
		workers:        effectiveWorkerCount(*workers),
		filter:         *filter,
		dryRun:         *dryRun,
		force:          *force,
		outputDir:      r.outputDir,
	})
}

type runUpscaleParams struct {
	targetRateFlag string
	targetBits     int
	quality        transcode.Quality
	workers        int
	filter         string
	dryRun         bool
	force          bool
	outputDir      string

	// soxInfo is this run's ProbeSox snapshot, used to refuse sources the
	// installed sox cannot decode. Probed ONCE per run (soxCLIReady already
	// pays for it) rather than per track — the CLI is a one-shot process, so
	// a TTL cache would buy nothing. Zero value (FormatsKnown false) fails
	// OPEN via SoxInfo.CanDecode, matching the server-side gates.
	soxInfo transcode.SoxInfo

	// Kind discriminates upscale (zero-value / JobKindUpscale,
	// legacy CLI behavior) from optimize (CarPlay-targeted
	// downsample). Drives the classifier's eligibility predicate +
	// target-rate resolution + JobSpec.Kind. For optimize, the
	// global targetRateFlag is ignored (each track's family
	// dictates target via TargetRateForOptimize) and targetBits is
	// uniformly 16 (CarPlay floor).
	kind transcode.JobKind
}

// upscaleCandidate carries the resolved JobSpec plus the
// resumability decision for one track. `needsRun = false` is the
// "already up-to-date sidecar" path; `skipNote` is the human-readable
// reason rendered in dry-run mode.
type upscaleCandidate struct {
	spec     transcode.JobSpec
	needsRun bool
	skipNote string
}

// upscaleSkipCounters tallies the per-track filter outcomes during
// candidate classification. Surfaced verbatim by reportUpscaleSummary.
type upscaleSkipCounters struct {
	notPCM          int
	sourceMissing   int
	alreadyAtTarget int
}

// resolveCLITargetForKind picks the per-track (targetRate, targetBits)
// for the CLI batch path based on `p.kind`. Returns:
//
//	(target, bits, skip=false, 0)     → enqueue this track
//	(0,      0,    skip=true,  0)     → silent skip; counter already
//	                                     bumped by this function
//	(0,      0,    -,          2)     → fatal CLI error; caller returns
//
// Split out of `classifyUpscaleTrack` so the kind branch doesn't
// push the parent function's cognitive complexity over the repo
// gate. SonarCloud go:S3776 enforcer caught the unconsolidated form
// at complexity 19; this helper extraction drops the parent to ~13.
func resolveCLITargetForKind(
	stderr io.Writer,
	t manifest.Track,
	p runUpscaleParams,
	sourceRateHz int,
	counters *upscaleSkipCounters,
) (target, bits int, skip bool, exitCode int) {
	if p.kind == transcode.JobKindOptimize {
		sourceBits := 0
		if t.BitsPerSample != nil {
			sourceBits = *t.BitsPerSample
		}
		if !transcode.OptimizeEligible(t.Path, t.Codec, sourceRateHz, sourceBits) {
			counters.alreadyAtTarget++
			return 0, 0, true, 0
		}
		r, err := transcode.ResolveTargetRateForOptimize(sourceRateHz)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", t.Path, err)
			return 0, 0, false, 2
		}
		// `OptimizeEligible` above is the authoritative gate; the
		// resolver always returns a real target now (does NOT
		// re-evaluate "is source at the floor" — a 44.1/24 candidate
		// flows through). Don't reintroduce a `r == 0` skip here.
		return r, 16, false, 0
	}
	r, err := transcode.ResolveTargetRate(p.targetRateFlag, sourceRateHz)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", t.Path, err)
		return 0, 0, false, 2
	}
	if r == 0 {
		counters.alreadyAtTarget++
		return 0, 0, true, 0
	}
	return r, p.targetBits, false, 0
}

// classifyUpscaleTrack evaluates one manifest track against the run
// params and returns either (a) an `upscaleCandidate` for the worker
// pool / dry-run output, (b) a non-nil exitCode for a fatal CLI error
// (e.g. ResolveTargetRate refused the flag), or (c) bumps one of the
// `upscaleSkipCounters` and returns `(nil, 0)` for a silent skip.
//
// Split out of runUpscaleBatch so the inner filter loop reads as a
// linear dispatch rather than an 80-line nested-control block (the
// pre-refactor body tripped SonarCloud go:S3776 with cognitive
// complexity 81).
func classifyUpscaleTrack(
	ctx context.Context,
	stderr io.Writer,
	store *manifest.Store,
	resolver *bridgefs.Resolver,
	t manifest.Track,
	p runUpscaleParams,
	counters *upscaleSkipCounters,
) (*upscaleCandidate, int) {
	if !matchesFilter(t.Path, p.filter) {
		return nil, 0
	}
	if t.IsDSD != nil && *t.IsDSD {
		counters.notPCM++
		return nil, 0
	}
	// Lossy sources are never upscaled OR optimized (PROTOCOL.md
	// documents the gate as "PCM"; upscaling decoded lossy audio adds
	// no fidelity). Mirrors Coordinator.Submit / EnqueueOne via the
	// shared manifest.IsLossyCodec. Gating here (vs relying on
	// OptimizeEligible below for kind=optimize) also counts lossy
	// under the accurate `notPCM` bucket instead of alreadyAtTarget.
	if manifest.IsLossyCodec(t.Codec) {
		counters.notPCM++
		return nil, 0
	}
	// Refuse what this sox build cannot decode, mirroring the server-side
	// gates (Coordinator's walks, EnqueueOne, the auto-optimize sweeper).
	// ALAC is the case that matters: lossless, so IsLossyCodec lets it
	// through, and it carries real PCM geometry since PR #440 — so without
	// this it reached a sox with no MP4 demuxer and failed per file.
	// Counted under notPCM: the accurate bucket for "this pipeline cannot
	// take it", the same one DSD and lossy use.
	if !p.soxInfo.CanDecode(t.Path) {
		counters.notPCM++
		return nil, 0
	}
	if t.SampleRate == nil || *t.SampleRate <= 0 {
		// No usable rate metadata → can't decide a target; skip
		// silently. The scanner sets this for every PCM file it
		// parses successfully; absence (nil) usually means the
		// extractor failed to identify the format. A non-nil but
		// non-positive rate (0 / negative) is a corrupt or bogus tag:
		// without this guard it reaches OptimizeEligible (which returns
		// true for a PCM source with bits > 16 regardless of rate) and
		// then ResolveTargetRateForOptimize(0), whose "rate must be
		// positive" error propagates exitCode 2 up through
		// runUpscaleBatch and ABORTS the whole optimize run — where
		// every other per-track anomaly is a graceful skip + counter.
		counters.notPCM++
		return nil, 0
	}
	sourceRateHz := int(*t.SampleRate)
	srcBits := 0 // display-only (worker grid signal chain); nil → 0 (unknown)
	if t.BitsPerSample != nil {
		srcBits = *t.BitsPerSample
	}
	target, targetBitsForJob, skip, exitCode := resolveCLITargetForKind(stderr, t, p, sourceRateHz, counters)
	if exitCode != 0 || skip {
		return nil, exitCode
	}
	// Find the absolute path via the canonical resolver —
	// handles single-root (rootless paths) and multi-root
	// (basename-prefixed paths) uniformly. A resolution
	// error here means the manifest row points at a path
	// that's no longer routable (root removed at runtime,
	// renamed off-disk, etc.); treat as missing-source.
	absPath, resolveErr := resolver.Resolve(t.Path)
	if resolveErr != nil {
		counters.sourceMissing++
		return nil, 0
	}
	if _, statErr := os.Stat(absPath); statErr != nil {
		counters.sourceMissing++
		return nil, 0
	}
	spec := transcode.JobSpec{
		SourceAbsPath:    absPath,
		SourceLibraryRel: t.Path,
		SourceSampleRate: sourceRateHz,
		SourceBits:       srcBits,
		TargetSampleRate: target,
		TargetBits:       targetBitsForJob,
		Quality:          p.quality,
		OutputDir:        p.outputDir,
		Kind:             p.kind, // zero-value preserves upscale for legacy callers
	}
	if err := spec.FreshnessFromFile(); err != nil {
		counters.sourceMissing++
		return nil, 0
	}
	needsRun, skipNote := upscaleResumeDecision(ctx, store, t.Path, spec, p.force)
	return &upscaleCandidate{spec: spec, needsRun: needsRun, skipNote: skipNote}, 0
}

// upscaleResumeDecision implements the `--force`-aware resumability
// check: if a fresh sidecar already covers this (source, variant) at
// the same source mtime + size, return needsRun=false with a
// dry-run-suitable reason string.
func upscaleResumeDecision(ctx context.Context, store *manifest.Store, trackPath string, spec transcode.JobSpec, force bool) (bool, string) {
	if force {
		return true, ""
	}
	existing, _ := store.GetVariant(ctx, trackPath, spec.VariantID())
	if existing == nil {
		return true, ""
	}
	if existing.SourceMTimeNS != spec.SourceMTimeNS || existing.SourceSize != spec.SourceSize {
		return true, ""
	}
	if _, err := os.Stat(existing.SidecarPath); err != nil {
		return true, ""
	}
	return false, "already up-to-date"
}

// reportUpscaleSummary prints the pre-conversion candidate /
// already-at-target / not-PCM / source-missing counters. Pulled out
// of runUpscaleBatch as the second of the cognitive-complexity
// refactor's helpers — pure I/O over the tally values.
func reportUpscaleSummary(stdout io.Writer, totalCandidates, toRun int, counters upscaleSkipCounters) {
	fmt.Fprintf(stdout, "Found %d candidate track(s); %d need conversion.\n", totalCandidates, toRun)
	if counters.alreadyAtTarget > 0 {
		fmt.Fprintf(stdout, "Skipped %d track(s) already at or above target rate.\n", counters.alreadyAtTarget)
	}
	if counters.notPCM > 0 {
		fmt.Fprintf(stdout, "Skipped %d non-PCM or unparseable track(s).\n", counters.notPCM)
	}
	if counters.sourceMissing > 0 {
		fmt.Fprintf(stdout, "Skipped %d track(s) with missing source files (run `bridge scan` to reconcile).\n", counters.sourceMissing)
	}
}

// printUpscaleDryRun emits the per-candidate "WILL CONVERT" / "SKIP"
// table the operator sees with `--dry-run`. Pure I/O; no side effects
// beyond stdout.
func printUpscaleDryRun(stdout io.Writer, candidates []upscaleCandidate) {
	for _, c := range candidates {
		tag := "WILL CONVERT"
		if !c.needsRun {
			tag = "SKIP (" + c.skipNote + ")"
		}
		fmt.Fprintf(stdout, "  %s  %s → %d/%d FLAC\n", tag, c.spec.SourceLibraryRel, c.spec.TargetBits, c.spec.TargetSampleRate)
	}
}

// runUpscaleWorker is the body of one sox / DB-upsert goroutine
// spawned by runUpscaleBatch. Drains `jobsCh`, runs sox, persists the
// resulting `track_variants` row, bumps the success / failure
// counters. Cancellation noise (SIGINT) is suppressed so a Ctrl-C run
// doesn't emit one FAIL line per in-flight worker.
func runUpscaleWorker(
	ctx context.Context,
	stderr io.Writer,
	store *manifest.Store,
	jobsCh <-chan upscaleCandidate,
	doneCount, failCount *uint64,
) {
	for c := range jobsCh {
		if !c.needsRun {
			continue
		}
		// Cooperative cancel check before spending sox CPU.
		// exec.CommandContext below is the harder gate
		// (kills an in-flight process), but checking here
		// avoids spawning a sox we'll immediately kill.
		if ctx.Err() != nil {
			return
		}
		size, err := transcode.RunSox(ctx, c.spec)
		if err != nil {
			if ctx.Err() == nil {
				atomic.AddUint64(failCount, 1)
				fmt.Fprintf(stderr, "FAIL %s: %v\n", c.spec.SourceLibraryRel, err)
			}
			continue
		}
		_, settings, _, _ := c.spec.SoxArgs()
		row := manifest.VariantRow{
			SourcePath:    c.spec.SourceLibraryRel,
			VariantID:     c.spec.VariantID(),
			SidecarPath:   c.spec.SidecarPath(),
			Format:        "flac",
			SampleRate:    c.spec.TargetSampleRate,
			BitsPerSample: c.spec.TargetBits,
			SizeBytes:     size,
			SourceMTimeNS: c.spec.SourceMTimeNS,
			SourceSize:    c.spec.SourceSize,
			SoxSettings:   settings,
			CreatedAt:     transcode.CreatedAtNow(),
		}
		if err := store.UpsertVariant(ctx, row); err != nil {
			// Suppress the failure log + counter increment
			// when the ctx itself is what cancelled the
			// write — mirrors the RunSox-cancelled-by-SIGINT
			// branch above so an operator Ctrl-C run doesn't
			// produce a flood of `context canceled` error
			// lines. Gemini Medium on PR #217.
			if ctx.Err() == nil {
				atomic.AddUint64(failCount, 1)
				fmt.Fprintf(stderr, "FAIL %s (db write): %v\n", c.spec.SourceLibraryRel, err)
				// Best-effort: remove the orphan sidecar so a
				// retry from a clean slate succeeds.
				_ = os.Remove(row.SidecarPath)
			}
			continue
		}
		atomic.AddUint64(doneCount, 1)
	}
}

// runUpscaleBatch is the main per-track loop. Walks every track in
// the manifest store, decides eligibility per the params, dispatches
// eligible jobs to the worker pool, prints progress, returns 0 on
// success / 1 on partial failure. Resumable: a job whose sidecar
// already exists with matching freshness is silently skipped (unless
// `--force`).
//
// `ctx` is honored for clean SIGINT shutdown — a cancellation
// stops dispatching new jobs AND signals running sox processes
// via exec.CommandContext (Gemini bot review on PR #108). Without
// it, Ctrl-C on a 50-track album would block until the largest
// file's sox finished.
//
// Refactored from a single 230-line body (CC 81 vs the 15 limit)
// into a series of single-purpose helpers above
// (`classifyUpscaleTrack` / `upscaleResumeDecision` /
// `reportUpscaleSummary` / `printUpscaleDryRun` /
// `runUpscaleWorker`). Behaviour and exit codes are byte-identical;
// locked by the existing cmd/bridge upscale test suite.
func runUpscaleBatch(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, cfg *config.Config, resolver *bridgefs.Resolver, p runUpscaleParams) int {
	// One probe for the whole run, feeding the classifier's decodability
	// gate. A probe FAILURE leaves the zero value, whose FormatsKnown=false
	// makes CanDecode fail open — the documented posture everywhere else.
	// bootstrapTranscodeCmd's soxCLIReady has already run by here, so this
	// is the second and last fork-exec of sox for the command.
	if info, perr := transcode.ProbeSox(ctx); perr == nil {
		p.soxInfo = info
	}
	allTracks, err := store.ListTracks(ctx, nil)
	if err != nil {
		fmt.Fprintf(stderr, "list tracks: %v\n", err)
		return 1
	}

	// First pass: build the candidate list. Decoupled from
	// conversion so dry-run can print without spawning workers,
	// and so the count is known upfront for the progress meter.
	candidates := make([]upscaleCandidate, 0, 64)
	var counters upscaleSkipCounters
	for _, t := range allTracks {
		c, exit := classifyUpscaleTrack(ctx, stderr, store, resolver, t, p, &counters)
		if exit != 0 {
			return exit
		}
		if c == nil {
			continue
		}
		candidates = append(candidates, *c)
	}

	totalCandidates := len(candidates)
	toRun := 0
	for _, c := range candidates {
		if c.needsRun {
			toRun++
		}
	}
	reportUpscaleSummary(stdout, totalCandidates, toRun, counters)

	if p.dryRun {
		printUpscaleDryRun(stdout, candidates)
		return 0
	}

	if toRun == 0 {
		return 0
	}

	// Worker pool. SoX is single-threaded per invocation; we run
	// `workers` parallel sox processes and let the OS scheduler
	// fan them across cores. A bounded channel keeps memory
	// predictable on a 50k-track library.
	jobsCh := make(chan upscaleCandidate, p.workers*2)
	var wg sync.WaitGroup
	var doneCount, failCount uint64

	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runUpscaleWorker(ctx, stderr, store, jobsCh, &doneCount, &failCount)
		}()
	}

	startedAt := time.Now()
	// Producer must honor cancellation too — otherwise a SIGINT
	// during dispatch (jobsCh full + workers slow) blocks here
	// until a worker drains a job, defeating prompt shutdown.
producerLoop:
	for _, c := range candidates {
		select {
		case jobsCh <- c:
		case <-ctx.Done():
			break producerLoop
		}
	}
	close(jobsCh)
	wg.Wait()
	elapsed := time.Since(startedAt).Round(time.Millisecond)

	done := atomic.LoadUint64(&doneCount)
	failed := atomic.LoadUint64(&failCount)
	// Cancellation surfaces with its own exit status so scripts
	// can tell "interrupted" apart from "completed cleanly with
	// no failures" — a partial run that returned 0 to the shell
	// would silently look successful even when most candidates
	// never ran (CodeRabbit second-pass on PR #108).
	if ctx.Err() != nil {
		fmt.Fprintf(stdout, "\nInterrupted after %s. Converted %d of %d, failed %d.\n", elapsed, done, toRun, failed)
		return 130 // POSIX convention: 128 + SIGINT(2)
	}
	fmt.Fprintf(stdout, "\nDone in %s. Converted %d, failed %d.\n", elapsed, done, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// runGC performs symmetric garbage collection on the variant store:
//
//  1. **Forward sweep** — walks `<dataDir>/transcoded/` and removes
//     any file that doesn't have a matching row in `track_variants`.
//     Companion to the proactive sidecar deletion in DeleteTrack;
//     catches sidecars that escape the proactive path (interrupted
//     DeleteTrack, manual SQL tampering, restored-from-backup
//     mismatch).
//
//  2. **Reverse sweep** — walks `track_variants` and removes any
//     row whose `sidecar_path` does not exist on disk. Closes the
//     "phantom variant" loop where the bridge advertises a variant
//     in `/v1/manifest`, iOS clients persist the ID and request it
//     on play, then every download attempt hits `410 Gone` because
//     the file was already removed (e.g. an earlier `bridge upscale`
//     pass switched the sidecar naming scheme — v1 64-char-hash to
//     v2 16-char-hash — without cleaning up the v1 DB rows). Without
//     this sweep, iOS clients pay a 410 round-trip on every fresh
//     play even after the stale-variant fallback ships (acoseac/1-bit
//     PR #351) — the next manifest rescan re-pulls the same dead ID
//     and the loop restarts.
//
// runGCForwardSweep walks `outputDir` and removes every file whose
// path is not in the `known` set built from `track_variants` rows. Returns
// `(removed, kept, failed, exitCode)` — `exitCode != 0` signals a
// fatal sweep error (or a SIGINT) and runGC bails immediately. The
// pre-refactor inline closure inflated cognitive complexity to 36; the
// extraction makes runGC a flat sequence of three named steps.
func runGCForwardSweep(ctx context.Context, stdout, stderr io.Writer, outputDir string, known map[string]bool) (int, int, int, int) {
	var removed, kept, failed int
	// Forward sweep: WalkDir over Walk avoids the per-file os.Lstat —
	// DirEntry already carries IsDir(), so a flat directory of N
	// sidecars pays N fewer syscalls (Gemini bot review on PR #108).
	walkErr := filepath.WalkDir(outputDir, func(path string, d os.DirEntry, walkErr error) error {
		// Stop the forward sweep promptly on SIGINT. Without
		// the check, a Ctrl-C mid-walk would let the GC keep
		// deleting files until it finished iterating the
		// directory. CodeRabbit Major on PR #217.
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Output dir may not exist yet (no upscales ever
			// run on this bridge). Treat as empty rather than
			// erroring out.
			if os.IsNotExist(walkErr) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if known[strings.ToLower(filepath.Clean(path))] {
			kept++
			return nil
		}
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "remove %s: %v\n", path, err)
			failed++
			return nil
		}
		removed++
		return nil
	})
	if walkErr != nil {
		// Operator-interrupt path: distinguish from a real
		// walk error so the SIGINT case reads cleanly.
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			fmt.Fprintln(stderr, gcInterruptedMessage)
			return removed, kept, failed, 1
		}
		fmt.Fprintf(stderr, "walk transcoded dir: %v\n", walkErr)
		return removed, kept, failed, 1
	}
	fmt.Fprintf(stdout, "GC forward sweep: removed %d orphan file(s), kept %d known sidecar(s), %d failure(s).\n", removed, kept, failed)
	return removed, kept, failed, 0
}

// gcCheckOutputDirBeforeReverseSweep enforces the "don't mass-delete
// rows on a disappeared transcoded root" guard documented in PR #207.
// Returns 0 on healthy state (proceed) or a non-zero exit code on
// missing / empty / unreadable root with extant rows. Split from
// runGC so the cognitive-complexity refactor reads as a flat
// sequence of guards rather than an inline switch.
//
// The exists-but-empty case (2026-07-21 review M15, prior audit B4's
// unfixed half) shares VariantWatcher's probe: a cleanly-unmounted
// mountpoint reverts to an EMPTY local dir, so an outputDir that
// stats fine but holds zero entries is the same mass-delete hazard
// as a missing one — every per-row sidecar stat would ENOENT.
func gcCheckOutputDirBeforeReverseSweep(stderr io.Writer, outputDir string, rowCount int) int {
	if rowCount == 0 {
		// LEGITIMATELY-empty case (no upscales ever generated on
		// this bridge); the forward sweep's WalkDir handles a
		// missing outputDir via filepath.SkipDir, so the guard
		// only protects against mass-delete when there's
		// something to lose.
		return 0
	}
	if reason := integrity.VariantsDirSweepBlockReason(outputDir); reason != "" {
		fmt.Fprintf(stderr, "GC reverse sweep: %s (%q) but %d variant row(s) exist; refusing to delete rows en masse (likely a disconnected mount or filesystem issue — restore access and re-run).\n", reason, outputDir, rowCount)
		return 1
	}
	return 0
}

// runGCReverseSweep is the per-row sweep: every track_variants row
// whose `sidecar_path` is missing on disk is deleted via the store's
// DeleteVariant (which bumps indexed_at). Returns
// `(rowsRemoved, rowsKept, rowsFailed, exitCode)`. exitCode is 1 on
// SIGINT-during-sweep, 0 otherwise — bot-reviewed cancellation shape
// from PR #217 (ctx cancel during the inner DeleteVariant surfaces as
// interrupted, real DB fault is logged and counted).
func runGCReverseSweep(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, allRows []manifest.VariantRow) (int, int, int, int) {
	// Reverse sweep: each row in `track_variants` whose `sidecar_path`
	// is missing on disk is a phantom variant. `DeleteVariant` is the
	// store API designed for this exact case — it bumps the parent
	// track's `indexed_at` so the next iOS delta sync sees the row
	// disappear, closing the loop. Use `os.Stat` (NOT `os.Lstat`) so
	// a symlink pointing at a missing target is correctly treated as
	// a phantom: the bridge's `/v1/download` path opens the file
	// through the symlink and would 410 on a broken target, so the
	// gc should treat that case identically to a directly-missing
	// file. Per Gemini on PR #207.
	//
	// Per-row `DeleteVariant` (one transaction per orphan) over a
	// bulk-delete API: `--gc` is operator-initiated and infrequent,
	// orphan counts are typically <100 in practice, and a new bulk
	// path on the Store would duplicate the `indexed_at`-bump
	// machinery `DeleteVariant` already provides. CLAUDE.md "no
	// premature abstractions" — revisit if a future call site
	// proves the volume out.
	var rowsRemoved, rowsKept, rowsFailed int
	for _, r := range allRows {
		// Stop the reverse sweep promptly on SIGINT — same
		// rationale as the forward-walk gate. CodeRabbit Major
		// + Gemini Medium on PR #217.
		if err := ctx.Err(); err != nil {
			fmt.Fprintln(stderr, gcInterruptedMessage)
			return rowsRemoved, rowsKept, rowsFailed, 1
		}
		_, statErr := os.Stat(r.SidecarPath)
		switch {
		case statErr == nil:
			rowsKept++
		case errors.Is(statErr, os.ErrNotExist):
			if err := store.DeleteVariant(ctx, r.SourcePath, r.VariantID); err != nil {
				// Two cancellation shapes get different
				// treatment (CodeRabbit Major round-3 on
				// PR #217):
				//
				//   - ctx-cancellation: return interrupted
				//     status immediately. Falling through to
				//     the success summary would hide the
				//     interrupt — the operator's Ctrl-C
				//     wouldn't show up in the exit code on
				//     the last-row case. The top-of-loop gate
				//     catches THIS row's cancellation on the
				//     next iteration, but if this IS the last
				//     row the loop exits and the summary
				//     reports success.
				//
				//   - Real DB fault: log + count + continue
				//     (same legacy degrade policy).
				if ctx.Err() != nil {
					fmt.Fprintln(stderr, gcInterruptedMessage)
					return rowsRemoved, rowsKept, rowsFailed, 1
				}
				fmt.Fprintf(stderr, "delete orphan row %s / %s: %v\n", r.SourcePath, r.VariantID, err)
				rowsFailed++
				continue
			}
			rowsRemoved++
		default:
			// Permission denied, I/O error, etc. — log and keep
			// the row rather than risk a destructive delete on a
			// transient failure. Operator re-runs `--gc` after
			// fixing the environment.
			fmt.Fprintf(stderr, "stat %s: %v\n", r.SidecarPath, statErr)
			rowsFailed++
		}
	}
	fmt.Fprintf(stdout, "GC reverse sweep: removed %d orphan row(s), kept %d row(s) with live sidecar, %d failure(s).\n", rowsRemoved, rowsKept, rowsFailed)
	return rowsRemoved, rowsKept, rowsFailed, 0
}

// Both sweeps run unconditionally under `--gc` because they share
// the same semantic: keep the on-disk inventory and the DB-row
// inventory consistent with each other. Splitting into separate
// flags would let operators run them out of order, which is fine
// for the forward sweep but the reverse sweep would re-introduce
// the very mismatch we're fixing.
//
// Refactored from a single 170-line body (CC 36 vs the 15 limit) into
// three helpers (`runGCForwardSweep` / `gcCheckOutputDirBeforeReverseSweep`
// / `runGCReverseSweep`). Behaviour is byte-identical; locked by the
// existing GC test suite.
func runGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, outputDir string) int {
	allRows, err := store.AllVariants(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list variants: %v\n", err)
		return 1
	}
	// Key the known-set on a case-folded + cleaned path so the forward
	// sweep can't delete a live sidecar over a casing delta between the
	// DB SidecarPath and the on-disk WalkDir path on a case-insensitive
	// FS (Windows / macOS). Mirrors runAnalyzeGC (PR #395); the reverse
	// sweep stats DB paths directly and needs no normalization.
	known := make(map[string]bool, len(allRows))
	for _, r := range allRows {
		known[strings.ToLower(filepath.Clean(r.SidecarPath))] = true
	}

	_, _, failed, exitCode := runGCForwardSweep(ctx, stdout, stderr, outputDir, known)
	if exitCode != 0 {
		return exitCode
	}

	if exitCode := gcCheckOutputDirBeforeReverseSweep(stderr, outputDir, len(allRows)); exitCode != 0 {
		return exitCode
	}

	_, _, rowsFailed, exitCode := runGCReverseSweep(ctx, stdout, stderr, store, allRows)
	if exitCode != 0 {
		return exitCode
	}
	if failed > 0 || rowsFailed > 0 {
		return 1
	}
	return 0
}

func matchesFilter(path, filter string) bool {
	if filter == "" {
		return true
	}
	return strings.Contains(path, filter)
}

func effectiveWorkerCount(flagValue int) int {
	if flagValue > 0 {
		return flagValue
	}
	n := runtime.NumCPU() - 1
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// printSoxInstallHint surfaces per-OS install one-liners so the
// operator doesn't have to hunt for the right package name. Falls
// back to the generic README pointer when we don't know the OS
// hint by heart.
func printSoxInstallHint(w io.Writer) {
	switch runtime.GOOS {
	case "darwin":
		fmt.Fprint(w, "  brew install sox\n")
	case "linux":
		fmt.Fprint(w, "  Debian/Ubuntu:  sudo apt install sox\n")
		fmt.Fprint(w, "  Fedora:         sudo dnf install sox\n")
		fmt.Fprint(w, "  Arch:           sudo pacman -S sox\n")
	case "windows":
		fmt.Fprint(w, "  choco install sox.portable\n")
		fmt.Fprint(w, "  (or download from https://sourceforge.net/projects/sox/)\n")
	default:
		fmt.Fprint(w, "  Install `sox` via your platform's package manager, or see https://sox.sourceforge.net\n")
	}
}

// printSoxFormatHint surfaces per-OS one-liners for the narrower failure
// where sox IS installed but its build lacks FLAC support — the bridge
// forces `-t flac` for every conversion, so a FLAC-less sox fails at
// runtime. On Debian/Ubuntu FLAC ships in a separate plugin package
// (libsox-fmt-all); Fedora/Arch/brew/choco bundle it, so the fix there is a
// reinstall. Mirror of soxFormatHintForCurrentOS in internal/admin — keep
// the two in sync.
func printSoxFormatHint(w io.Writer) {
	switch runtime.GOOS {
	case "darwin":
		fmt.Fprint(w, "  brew reinstall sox   # the Homebrew bottle includes FLAC\n")
	case "linux":
		fmt.Fprint(w, "  Debian/Ubuntu:  sudo apt install libsox-fmt-all\n")
		fmt.Fprint(w, "  Fedora:         sudo dnf install sox        # bundles FLAC\n")
		fmt.Fprint(w, "  Arch:           sudo pacman -S sox          # bundles FLAC\n")
	case "windows":
		fmt.Fprint(w, "  choco install sox.portable\n")
		fmt.Fprint(w, "  (or download a full build from https://sourceforge.net/projects/sox/)\n")
	default:
		fmt.Fprint(w, "  Reinstall `sox` with FLAC support, or see https://sox.sourceforge.net\n")
	}
}

// soxFeatureReady reports whether sox is usable for an offline-decode
// feature (upscale / analysis) at `bridge serve` startup: present on PATH
// AND its build has FLAC support (the bridge forces `-t flac`, so a
// FLAC-less sox would fail every job at runtime). On any disqualifying
// condition it writes an operator-facing reason to stderr and returns false
// so the caller degrades the feature to "off" in-memory — the rest of the
// server keeps running. An unparseable `sox --help` is treated
// conservatively as "FLAC present": never disable a working install over a
// help-output reword. ctx is propagated so a SIGINT during startup aborts
// the probe; the 2 s cap lives inside ProbeSox regardless.
func soxFeatureReady(ctx context.Context, feature string, stderr io.Writer) bool {
	info, err := transcode.ProbeSox(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: feature is enabled in bridge.yaml but sox is not available — disabling: %v\n", feature, err)
		return false
	}
	if info.FormatsKnown && !info.HasFLAC {
		fmt.Fprintf(stderr, "%s: feature is enabled in bridge.yaml but the installed sox build lacks FLAC support (needed for the internal pipeline) — disabling\n", feature)
		return false
	}
	return true
}

// fingerprintFeatureReady reports whether the acoustic-fingerprinting fallback
// can actually run at `bridge serve` startup: fpcalc on PATH AND an AcoustID
// application key configured.
//
// Same contract as soxFeatureReady — on any disqualifying condition it writes
// an operator-facing reason to stderr and returns false, so the caller
// degrades the feature to off in-memory and the rest of the server keeps
// running. A missing optional credential must never be an outage.
//
// Both prerequisites are checked here rather than at config-validation time
// for that reason: Validate() is a pure predicate and a host with
// BRIDGE_FINGERPRINT_ENABLED set but no key still has to boot.
// The degradedReason return is a bounded key ("fpcalc_missing" /
// "no_api_key", "" when ready) the admin Jobs card renders remediation
// copy from — same bounded-key discipline as the enricher's skip
// reasons.
func fingerprintFeatureReady(ctx context.Context, hasAPIKey bool, stderr io.Writer) (ok bool, degradedReason string) {
	if _, err := acoustid.Probe(ctx); err != nil {
		fmt.Fprintf(stderr, "fingerprint: feature is enabled in bridge.yaml but fpcalc is not available — disabling: %v\n", err)
		return false, "fpcalc_missing"
	}
	if !hasAPIKey {
		fmt.Fprint(stderr, "fingerprint: feature is enabled in bridge.yaml but no AcoustID API key is configured — disabling.\n"+
			"  Register a free application key at https://acoustid.org/new-application,\n"+
			"  then set ACOUSTID_API_KEY (preferred) or fingerprint.apiKey in bridge.yaml.\n")
		return false, "no_api_key"
	}
	return true, ""
}

// soxCLIReady is the CLI-side sox preflight shared by `bridge upscale`,
// `bridge optimize`, and `bridge analyze`: it checks sox is present AND its
// build has FLAC support, printing the appropriate install / format hint to
// stderr on failure and returning false so the caller can exit. featureNeed
// completes the sentence "…lacks FLAC support, which <featureNeed>." The
// conservative FormatsKnown gate matches soxFeatureReady. Extracted so the
// per-subcommand call sites stay one line (and keeps analyzeCmd under the
// cognitive-complexity budget).
func soxCLIReady(ctx context.Context, stderr io.Writer, featureNeed string) bool {
	info, err := transcode.ProbeSox(ctx)
	if err != nil {
		if errors.Is(err, transcode.ErrSoxMissing) {
			fmt.Fprintf(stderr, "%v\n\nInstall sox:\n", err)
			printSoxInstallHint(stderr)
		} else {
			fmt.Fprintf(stderr, "sox precheck: %v\n", err)
		}
		return false
	}
	if info.FormatsKnown && !info.HasFLAC {
		fmt.Fprintf(stderr, "sox is installed but its build lacks FLAC support, which %s.\n\nFix:\n", featureNeed)
		printSoxFormatHint(stderr)
		return false
	}
	return true
}
