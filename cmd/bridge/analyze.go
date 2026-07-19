package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// analyzeCmd implements `bridge analyze` — the offline driver that
// computes a peak waveform sidecar per library track (the iOS scrubber
// feature). Opt-in: refuses unless `analysis.enabled: true`. Decode
// runs through sox (the same dependency upscaling uses). `--gc` reaps
// orphan sidecars instead of converting.
//
// Exit codes: 0 clean, 1 runtime error, 2 usage / config error, 130
// interrupted mid-batch (POSIX 128+SIGINT) so scripts can tell an
// interrupted run from a clean one.
func analyzeCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	workers := fs.Int("workers", 0, "concurrent decoders; 0 = max(1, NumCPU/2)")
	filter := fs.String("filter", "", "case-sensitive substring filter on track path (empty = all)")
	dryRun := fs.Bool("dry-run", false, "list how many tracks would be analyzed without doing it")
	force := fs.Bool("force", false, "re-analyze even if a fresh sidecar already exists")
	gc := fs.Bool("gc", false, "remove orphan waveform sidecars (files with no DB row); skips analysis")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return 2
	}
	if !cfg.Analysis.Enabled {
		fmt.Fprint(stderr, "Audio analysis is disabled in bridge.yaml.\n"+
			"Set `analysis.enabled: true` and restart `bridge serve`, then re-run this command.\n")
		return 2
	}
	// sox is only needed to actually decode; --gc is a pure DB+FS sweep.
	if !*gc && !soxCLIReady(ctx, stderr, "audio analysis needs to decode tracks") {
		return 1
	}

	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()

	outputDir := analyze.WaveformDirFor(cfg.DataDir)
	if *gc {
		return runAnalyzeGC(ctx, stdout, stderr, store, outputDir)
	}

	resolver := bridgefs.New(cfg.LibraryRoots)
	workerCount := *workers
	if workerCount <= 0 {
		workerCount = cfg.Analysis.EffectiveWorkers()
	}
	return runAnalyzeBatch(ctx, stdout, stderr, store, resolver, analyzeBatchParams{
		outputDir: outputDir,
		workers:   workerCount,
		queueCap:  cfg.Analysis.EffectiveQueueCap(),
		filter:    *filter,
		dryRun:    *dryRun,
		force:     *force,
	})
}

type analyzeBatchParams struct {
	outputDir string
	workers   int
	queueCap  int
	filter    string
	dryRun    bool
	force     bool
}

// runAnalyzeBatch enumerates library tracks, applies the scan-skip gate
// (an up-to-date sidecar is skipped unless --force), and feeds the rest
// through an analyze.Pool.
func runAnalyzeBatch(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, resolver *bridgefs.Resolver, p analyzeBatchParams) int {
	res, err := collectAnalysisCandidates(ctx, store, resolver, p.outputDir, p.filter, p.force)
	if err != nil {
		fmt.Fprintf(stderr, "list tracks: %v\n", err)
		return 1
	}
	candidates := res.candidates

	fmt.Fprintf(stdout, "analyze: %d tracks, %d to analyze, %d up-to-date, %d skipped (DSD), %d empty, %d unreadable\n",
		res.total, len(candidates), res.skipped, res.dsdSkipped, res.emptySkipped, res.missing)
	if p.dryRun {
		return 0
	}
	if len(candidates) == 0 {
		return 0
	}

	pool := analyze.NewPool(store, p.workers, p.queueCap)
	total := len(candidates)
	interrupted := false
producer:
	for _, c := range candidates {
		for {
			select {
			case <-ctx.Done():
				interrupted = true
				break producer
			default:
			}
			err := pool.Enqueue(c)
			if err == nil {
				break
			}
			if errors.Is(err, analyze.ErrQueueFull) {
				// Queue is draining — back off briefly and retry.
				select {
				case <-ctx.Done():
					interrupted = true
					break producer
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			// ErrPoolClosed shouldn't happen here; stop dispatching.
			break producer
		}
	}

	// Drain: wait until queued + inflight reach zero (or interrupted).
	for !interrupted {
		st := pool.Stats()
		if st.QueueLen == 0 && st.Inflight == 0 {
			break
		}
		select {
		case <-ctx.Done():
			interrupted = true
		case <-time.After(250 * time.Millisecond):
			fmt.Fprintf(stdout, "\ranalyze: %d/%d done, %d failed   ", st.Done, total, st.Failed)
		}
	}
	pool.Stop()
	st := pool.Stats()
	fmt.Fprintf(stdout, "\ranalyze: %d done, %d failed%s\n", st.Done, st.Failed, strings.Repeat(" ", 12))
	if interrupted {
		fmt.Fprintln(stderr, "analyze: interrupted")
		return 130
	}
	if st.Failed > 0 {
		return 1
	}
	return 0
}

// runAnalyzeGC removes orphan waveform sidecars — files under the
// waveform output dir that no `track_analysis` row points at (plus
// stale `.tmp` debris from interrupted runs). Mirrors the forward sweep
// of `bridge upscale --gc`.
func runAnalyzeGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, outputDir string) int {
	rows, err := store.AllAnalysisRows(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list analysis rows: %v\n", err)
		return 1
	}
	// Key on the lowercased clean path so a case difference between the
	// DB-recorded path and the on-disk path (case-insensitive macOS /
	// Windows filesystems) can't make `--gc` delete a live waveform.
	// On case-sensitive Linux the worst case is a false-keep of a rare
	// same-name-different-case orphan — safe (no data loss). Gemini on #395.
	known := make(map[string]bool, len(rows))
	for _, r := range rows {
		if r.WaveformPath != "" {
			known[strings.ToLower(filepath.Clean(r.WaveformPath))] = true
		}
	}

	if _, statErr := os.Stat(outputDir); errors.Is(statErr, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "analyze --gc: no waveform dir at %s; nothing to do\n", outputDir)
		return 0
	}

	var removed, kept int
	walkErr := filepath.WalkDir(outputDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip unreadable entries
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		isWaveform := strings.HasSuffix(name, ".waveform.bin")
		isTmp := strings.HasSuffix(name, ".waveform.bin.tmp")
		if !isWaveform && !isTmp {
			return nil
		}
		if isWaveform && known[strings.ToLower(filepath.Clean(path))] {
			kept++
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "analyze --gc: remove %s: %v\n", name, rmErr)
			return nil
		}
		removed++
		return nil
	})
	if walkErr != nil && (errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded)) {
		fmt.Fprintln(stderr, "analyze --gc: interrupted")
		return 130
	}
	fmt.Fprintf(stdout, "analyze --gc: removed %d orphan sidecar(s), kept %d\n", removed, kept)
	return 0
}

// analysisScanResult bundles the enumeration outcome shared by the CLI
// batch path and the serve-side auto-analysis sweeper.
type analysisScanResult struct {
	candidates   []analyze.AnalyzeSpec
	total        int
	skipped      int // up-to-date sidecar (scan-skip gate hit)
	dsdSkipped   int // DSD source (sox can't decode)
	emptySkipped int // zero-byte source (unanalyzable — failed/incomplete upload)
	missing      int // unresolvable / directory
}

// collectAnalysisCandidates enumerates library tracks that need a
// waveform: filtered by `filter` (substring; "" = all), DSD skipped,
// unreadable skipped, and — unless `force` — up-to-date sidecars skipped
// via the scan-skip gate (matching source mtime + size + schema). Shared
// by `bridge analyze` and the serve-side sweeper so the two can't drift
// on what "needs analysis" means.
func collectAnalysisCandidates(ctx context.Context, store *manifest.Store, resolver *bridgefs.Resolver, outputDir, filter string, force bool) (analysisScanResult, error) {
	paths, err := store.TrackPaths(ctx)
	if err != nil {
		return analysisScanResult{}, err
	}
	res := analysisScanResult{total: len(paths)}
	for _, rel := range paths {
		if filter != "" && !strings.Contains(rel, filter) {
			continue
		}
		// DSD is out of scope — sox can't decode 1-bit DSD streams.
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".dsf", ".dff":
			res.dsdSkipped++
			continue
		}
		abs, info, rerr := resolver.ResolveChecked(rel)
		if rerr != nil || info.IsDir() {
			res.missing++
			continue
		}
		// A zero-byte source can never produce a waveform: sox can't probe
		// it and the ffmpeg fallback fails with "Cannot determine format …
		// after EOF". These are failed/incomplete uploads (e.g. a truncated
		// B2 sync), so skip them at collection time — otherwise the sweeper
		// re-enqueues + re-fails them on every tick (recurring "analyze:
		// failed" log noise). A re-upload makes size > 0 and the file flows
		// through normally on the next sweep. Skipped even under --force,
		// since force bypasses the freshness gate, not unanalyzability — and
		// the check stays mtime/size-driven so it can't suppress a real file
		// that's only TRANSIENTLY failing (those keep a non-zero size).
		if info.Size() == 0 {
			res.emptySkipped++
			continue
		}
		if !force {
			if existing, gerr := store.GetAnalysis(ctx, rel); gerr == nil && existing != nil &&
				existing.SourceMTimeNS == info.ModTime().UnixNano() &&
				existing.SourceSize == info.Size() &&
				existing.SchemaVersion == analyze.WaveformSchemaVersion {
				res.skipped++
				continue
			}
		}
		res.candidates = append(res.candidates, analyze.AnalyzeSpec{
			SourceAbsPath:    abs,
			SourceLibraryRel: rel,
			SourceMTimeNS:    info.ModTime().UnixNano(),
			SourceSize:       info.Size(),
			OutputDir:        outputDir,
		})
	}
	return res, nil
}

// runAnalysisSweeper is the serve-side auto-analysis loop. After an
// initial settle delay (let any startup scan land) and then on every
// `interval` tick, it enqueues tracks missing a fresh waveform to the
// long-lived pool. Idempotent — the scan-skip gate means already-
// analyzed tracks are skipped, so a re-sweep over an unchanged library
// enqueues nothing. Generation also stays available via the
// `bridge analyze` CLI. Honors ctx for clean shutdown; a saturated
// queue just defers the rest to the next tick.
func runAnalysisSweeper(ctx context.Context, store *manifest.Store, resolver *bridgefs.Resolver, outputDir string, pool *analyze.Pool, interval time.Duration) {
	sweep := func() {
		res, err := collectAnalysisCandidates(ctx, store, resolver, outputDir, "", false)
		if err != nil {
			logger.Warn("auto-analysis sweep: list tracks", "err", err)
			return
		}
		enqueued := 0
		for _, c := range res.candidates {
			if ctx.Err() != nil {
				return
			}
			switch err := pool.Enqueue(c); {
			case err == nil:
				enqueued++
			case errors.Is(err, analyze.ErrQueueFull), errors.Is(err, analyze.ErrPoolClosed):
				// Queue saturated (or shutting down) — leave the rest
				// for the next tick rather than spinning.
				if enqueued > 0 {
					logger.Info("auto-analysis sweep enqueued tracks (queue now full)", "count", enqueued)
				}
				return
			}
		}
		if enqueued > 0 {
			logger.Info("auto-analysis sweep enqueued tracks", "count", enqueued)
		}
	}

	// Settle delay so the sweep doesn't compete with startup work.
	const settleDelay = 90 * time.Second
	select {
	case <-ctx.Done():
		return
	case <-time.After(settleDelay):
	}
	sweep()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
