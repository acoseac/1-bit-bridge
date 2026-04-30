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
//   - `--gc`: walk the on-disk transcoded/ directory and remove
//     files with no matching DB row (orphan recovery — see PROJECT
//     CLAUDE.md "DeleteTrack sidecar-cleanup contract" for context).
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
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// transcodedDirName lives under cfg.DataDir. Centralised here so a
// future relocation (e.g. operator-controllable storage path)
// touches one constant.
const transcodedDirName = "transcoded"

func upscaleCmd(_ context.Context, args []string, stdout, stderr io.Writer) int {
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
	gc := fs.Bool("gc", false, "remove orphan sidecars (files with no matching DB row); skips conversion")
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
		return runGC(stdout, stderr, store, outputDir)
	}

	return runUpscaleBatch(stdout, stderr, store, cfg, runUpscaleParams{
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
func runUpscaleBatch(stdout, stderr io.Writer, store *manifest.Store, cfg *config.Config, p runUpscaleParams) int {
	allTracks, err := store.ListTracks(nil)
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
		// Find the absolute path. The Track.Path is library-
		// relative; we need the on-disk path for SoX.
		absPath := absolutePathFor(cfg.LibraryRoots, t.Path)
		if absPath == "" {
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
			if existing, _ := store.GetVariant(t.Path, spec.VariantID()); existing != nil {
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
				size, err := transcode.RunSox(c.spec)
				if err != nil {
					atomic.AddUint64(&failCount, 1)
					fmt.Fprintf(stderr, "FAIL %s: %v\n", c.spec.SourceLibraryRel, err)
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
				if err := store.UpsertVariant(row); err != nil {
					atomic.AddUint64(&failCount, 1)
					fmt.Fprintf(stderr, "FAIL %s (db write): %v\n", c.spec.SourceLibraryRel, err)
					// Best-effort: remove the orphan sidecar so a
					// retry from a clean slate succeeds.
					_ = os.Remove(row.SidecarPath)
					continue
				}
				atomic.AddUint64(&doneCount, 1)
			}
		}()
	}

	startedAt := time.Now()
	for _, c := range candidates {
		jobsCh <- c
	}
	close(jobsCh)
	wg.Wait()
	elapsed := time.Since(startedAt).Round(time.Millisecond)

	done := atomic.LoadUint64(&doneCount)
	failed := atomic.LoadUint64(&failCount)
	fmt.Fprintf(stdout, "\nDone in %s. Converted %d, failed %d.\n", elapsed, done, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// runGC walks <dataDir>/transcoded/ and removes any file that
// doesn't have a matching row in `track_variants`. Companion to
// the proactive sidecar deletion in DeleteTrack — catches sidecars
// that escape the proactive path (interrupted DeleteTrack, manual
// SQL tampering, restored-from-backup mismatch).
func runGC(stdout, stderr io.Writer, store *manifest.Store, outputDir string) int {
	allRows, err := store.AllVariants()
	if err != nil {
		fmt.Fprintf(stderr, "list variants: %v\n", err)
		return 1
	}
	known := make(map[string]bool, len(allRows))
	for _, r := range allRows {
		known[r.SidecarPath] = true
	}

	var removed, kept, failed int
	walkErr := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Output dir may not exist yet (no upscales ever
			// run on this bridge). Treat as empty rather than
			// erroring out.
			if os.IsNotExist(walkErr) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if info.IsDir() {
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
		fmt.Fprintf(stderr, "walk transcoded dir: %v\n", walkErr)
		return 1
	}
	fmt.Fprintf(stdout, "GC: removed %d orphan(s), kept %d known sidecar(s), %d failure(s).\n", removed, kept, failed)
	if failed > 0 {
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

// absolutePathFor walks the configured library roots and returns
// the absolute filesystem path corresponding to the manifest's
// library-relative track path. Returns "" if the path doesn't
// match any root or the candidate file isn't readable.
func absolutePathFor(roots []string, libraryRelative string) string {
	for _, root := range roots {
		base := filepath.Base(root) + "/"
		if strings.HasPrefix(libraryRelative, base) {
			rel := libraryRelative[len(base):]
			candidate := filepath.Join(root, rel)
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
	}
	return ""
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
