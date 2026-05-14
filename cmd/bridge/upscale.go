// `bridge upscale` CLI subcommand: walks the manifest store for
// PCM tracks below the target rate and runs SoX to produce sidecar
// FLACs cached under <dataDir>/transcoded/. Records each conversion
// in `track_variants` so the manifest provider can advertise the
// new variants on the next iOS sync.
//
// Two modes:
//   - default: enqueue every eligible track (or filtered subset
//     via `--filter`), run them through a worker pool, exit.
//   - `--dry-run`: print the candidate list without invoking sox.
//
// Maintenance modes:
//   - `--gc`: symmetric garbage collection. Walks the on-disk
//     `<dataDir>/transcoded/` directory and removes files with no
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

	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// transcodedDirName lives under cfg.DataDir. The literal name is
// owned by `transcode.OutputDirSubdir` (single source of truth so
// the CLI's `--gc` walker and the runtime pool's `OutputDir` can't
// drift); the alias below stays for in-package readability.
const transcodedDirName = transcode.OutputDirSubdir

// gcInterruptedMessage is the stderr line printed when an upscale GC
// pass is cancelled via ctx (Ctrl+C, parent shutdown). Three call sites
// — forward sweep, reverse sweep, and the per-row inner loop.
const gcInterruptedMessage = "GC interrupted"

func upscaleCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("upscale", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
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

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return 2
	}

	// Feature flag gate. Disabled bridges refuse to run the CLI
	// even if all the inputs are valid — operators must
	// consciously opt in.
	if !cfg.Upscale.Enabled {
		fmt.Fprint(stderr, "Upscaling is disabled in bridge.yaml.\n"+
			"Set `upscale.enabled: true` and restart `bridge serve`, then re-run this command.\n")
		return 2
	}

	// SoX-on-PATH probe. The pure-function part of the feature
	// (manifest queries, GC) doesn't need sox, so we defer this
	// check until just before a conversion would actually fire.
	if !*gc {
		if err := transcode.PrecheckSox(); err != nil {
			if errors.Is(err, transcode.ErrSoxMissing) {
				fmt.Fprintf(stderr, "%v\n\nInstall sox:\n", err)
				printSoxInstallHint(stderr)
			} else {
				fmt.Fprintf(stderr, "sox precheck: %v\n", err)
			}
			return 1
		}
	}

	q := transcode.Quality(*quality)
	switch q {
	case transcode.QualityVeryHigh, transcode.QualityHigh, transcode.QualityMedium:
	default:
		fmt.Fprintf(stderr, "invalid --quality %q (want very-high|high|medium)\n", *quality)
		return 2
	}
	if *targetBits != 16 && *targetBits != 24 && *targetBits != 32 {
		fmt.Fprintf(stderr, "invalid --target-bits %d (want 16/24/32)\n", *targetBits)
		return 2
	}

	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()

	outputDir := filepath.Join(cfg.DataDir, transcodedDirName)

	if *gc {
		return runGC(ctx, stdout, stderr, store, outputDir)
	}

	// Build a Resolver from the config roots. Same routing
	// engine the api uses for /v1/list / /v1/download — handles
	// single-root (rootless paths) AND multi-root (basename-
	// prefixed paths) uniformly. Pre-fix, this CLI had its own
	// hand-rolled basename-stripping helper that silently
	// returned "" for every track in single-root layouts —
	// nothing got converted (CodeRabbit second-pass on PR #108,
	// continuation of the api-side fix).
	resolver := bridgefs.New(cfg.LibraryRoots)

	return runUpscaleBatch(ctx, stdout, stderr, store, cfg, resolver, runUpscaleParams{
		targetRateFlag: *targetRate,
		targetBits:     *targetBits,
		quality:        q,
		workers:        effectiveWorkerCount(*workers),
		filter:         *filter,
		dryRun:         *dryRun,
		force:          *force,
		outputDir:      outputDir,
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
func runUpscaleBatch(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, cfg *config.Config, resolver *bridgefs.Resolver, p runUpscaleParams) int {
	allTracks, err := store.ListTracks(ctx, nil)
	if err != nil {
		fmt.Fprintf(stderr, "list tracks: %v\n", err)
		return 1
	}

	// First pass: build the candidate list. Decoupled from
	// conversion so dry-run can print without spawning workers,
	// and so the count is known upfront for the progress meter.
	type candidate struct {
		spec     transcode.JobSpec
		needsRun bool   // false → already-fresh sidecar, will skip unless --force
		skipNote string // empty when needsRun is true
	}
	candidates := make([]candidate, 0, 64)
	skippedNotPCM := 0
	skippedSourceMissing := 0
	skippedAlreadyAtTarget := 0

	for _, t := range allTracks {
		if !matchesFilter(t.Path, p.filter) {
			continue
		}
		if t.IsDSD != nil && *t.IsDSD {
			skippedNotPCM++
			continue
		}
		if t.SampleRate == nil {
			// No rate metadata → can't decide a target; skip
			// silently. The scanner sets this for every PCM file
			// it parses successfully; absence usually means the
			// extractor failed to identify the format.
			skippedNotPCM++
			continue
		}
		sourceRateHz := int(*t.SampleRate)
		target, err := transcode.ResolveTargetRate(p.targetRateFlag, sourceRateHz)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", t.Path, err)
			return 2
		}
		if target == 0 {
			skippedAlreadyAtTarget++
			continue
		}
		// Find the absolute path via the canonical resolver —
		// handles single-root (rootless paths) and multi-root
		// (basename-prefixed paths) uniformly. A resolution
		// error here means the manifest row points at a path
		// that's no longer routable (root removed at runtime,
		// renamed off-disk, etc.); treat as missing-source.
		absPath, resolveErr := resolver.Resolve(t.Path)
		if resolveErr != nil {
			skippedSourceMissing++
			continue
		}
		if _, statErr := os.Stat(absPath); statErr != nil {
			skippedSourceMissing++
			continue
		}
		spec := transcode.JobSpec{
			SourceAbsPath:    absPath,
			SourceLibraryRel: t.Path,
			SourceSampleRate: sourceRateHz,
			TargetSampleRate: target,
			TargetBits:       p.targetBits,
			Quality:          p.quality,
			OutputDir:        p.outputDir,
		}
		if err := spec.FreshnessFromFile(); err != nil {
			skippedSourceMissing++
			continue
		}
		// Resumability check: sidecar already there + DB row's
		// freshness matches current source → no work needed.
		needsRun := true
		var skipNote string
		if !p.force {
			if existing, _ := store.GetVariant(ctx, t.Path, spec.VariantID()); existing != nil {
				if existing.SourceMTimeNS == spec.SourceMTimeNS && existing.SourceSize == spec.SourceSize {
					if _, err := os.Stat(existing.SidecarPath); err == nil {
						needsRun = false
						skipNote = "already up-to-date"
					}
				}
			}
		}
		candidates = append(candidates, candidate{spec: spec, needsRun: needsRun, skipNote: skipNote})
	}

	totalCandidates := len(candidates)
	toRun := 0
	for _, c := range candidates {
		if c.needsRun {
			toRun++
		}
	}

	fmt.Fprintf(stdout, "Found %d candidate track(s); %d need conversion.\n", totalCandidates, toRun)
	if skippedAlreadyAtTarget > 0 {
		fmt.Fprintf(stdout, "Skipped %d track(s) already at or above target rate.\n", skippedAlreadyAtTarget)
	}
	if skippedNotPCM > 0 {
		fmt.Fprintf(stdout, "Skipped %d non-PCM or unparseable track(s).\n", skippedNotPCM)
	}
	if skippedSourceMissing > 0 {
		fmt.Fprintf(stdout, "Skipped %d track(s) with missing source files (run `bridge scan` to reconcile).\n", skippedSourceMissing)
	}

	if p.dryRun {
		for _, c := range candidates {
			tag := "WILL CONVERT"
			if !c.needsRun {
				tag = "SKIP (" + c.skipNote + ")"
			}
			fmt.Fprintf(stdout, "  %s  %s → %d/%d FLAC\n", tag, c.spec.SourceLibraryRel, c.spec.TargetBits, c.spec.TargetSampleRate)
		}
		return 0
	}

	if toRun == 0 {
		return 0
	}

	// Worker pool. SoX is single-threaded per invocation; we run
	// `workers` parallel sox processes and let the OS scheduler
	// fan them across cores. A bounded channel keeps memory
	// predictable on a 50k-track library.
	jobsCh := make(chan candidate, p.workers*2)
	var wg sync.WaitGroup
	var doneCount, failCount uint64

	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
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
					// Drop cancellation noise — operator-driven
					// SIGINT shouldn't print one FAIL line per
					// in-flight worker.
					if ctx.Err() == nil {
						atomic.AddUint64(&failCount, 1)
						fmt.Fprintf(stderr, "FAIL %s: %v\n", c.spec.SourceLibraryRel, err)
					}
					continue
				}
				_, settings := c.spec.SoxArgs()
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
						atomic.AddUint64(&failCount, 1)
						fmt.Fprintf(stderr, "FAIL %s (db write): %v\n", c.spec.SourceLibraryRel, err)
						// Best-effort: remove the orphan sidecar so a
						// retry from a clean slate succeeds.
						_ = os.Remove(row.SidecarPath)
					}
					continue
				}
				atomic.AddUint64(&doneCount, 1)
			}
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
// Both sweeps run unconditionally under `--gc` because they share
// the same semantic: keep the on-disk inventory and the DB-row
// inventory consistent with each other. Splitting into separate
// flags would let operators run them out of order, which is fine
// for the forward sweep but the reverse sweep would re-introduce
// the very mismatch we're fixing.
func runGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, outputDir string) int {
	allRows, err := store.AllVariants(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list variants: %v\n", err)
		return 1
	}
	known := make(map[string]bool, len(allRows))
	for _, r := range allRows {
		known[r.SidecarPath] = true
	}

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
		if known[path] {
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
			return 1
		}
		fmt.Fprintf(stderr, "walk transcoded dir: %v\n", walkErr)
		return 1
	}
	fmt.Fprintf(stdout, "GC forward sweep: removed %d orphan file(s), kept %d known sidecar(s), %d failure(s).\n", removed, kept, failed)

	// Guard against mass-delete on a disappeared transcoded root.
	// If `outputDir` is missing but `track_variants` has rows, the
	// per-row `os.Stat` below would return ENOENT for every row and
	// the reverse sweep would nuke the entire variant catalog —
	// even though the rows are almost certainly fine and the
	// underlying issue is environmental (external drive
	// disconnected, mount point gone, filesystem error). Refuse to
	// proceed; restore access and re-run. Per CodeRabbit on PR #207.
	//
	// `len(allRows) == 0` is the LEGITIMATELY-empty case (no
	// upscales ever generated on this bridge) and the forward
	// sweep's `WalkDir` already handles a missing `outputDir`
	// gracefully via `filepath.SkipDir`, so this guard only fires
	// when there's something to lose.
	if len(allRows) > 0 {
		_, statErr := os.Stat(outputDir)
		switch {
		case statErr == nil:
			// Healthy state — proceed to the per-row loop below.
		case errors.Is(statErr, os.ErrNotExist):
			fmt.Fprintf(stderr, "GC reverse sweep: transcoded directory %q is missing but %d variant row(s) exist; refusing to delete rows en masse (likely a disconnected mount or filesystem issue — restore access and re-run).\n", outputDir, len(allRows))
			return 1
		default:
			// Any other stat failure (permission denied, I/O
			// error, stale NFS handle, etc.) means the per-row
			// `os.Stat(SidecarPath)` below would almost certainly
			// fail the same way for every row — accumulating N
			// per-row "stat failure" log lines and burning the
			// operator's terminal output without making progress.
			// Bail upfront with a single distinct message.
			// CodeRabbit on PR #207 round 3.
			fmt.Fprintf(stderr, "GC reverse sweep: cannot stat transcoded directory %q (%v); refusing to proceed with %d variant row(s) at risk.\n", outputDir, statErr, len(allRows))
			return 1
		}
	}
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
			return 1
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
					return 1
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
