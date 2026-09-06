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

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/analyze"
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
	configPath := fs.String("config", "", "path to config file (default: ./bridge.yaml, else the platform config dir)")
	workers := fs.Int("workers", 0, "concurrent decoders; 0 = max(1, NumCPU/2)")
	filter := fs.String("filter", "", "case-sensitive substring filter on track path (empty = all)")
	dryRun := fs.Bool("dry-run", false, "list how many tracks would be analyzed without doing it")
	force := fs.Bool("force", false, "re-analyze even if a fresh sidecar already exists")
	gc := fs.Bool("gc", false, "remove orphan waveform sidecars (files with no DB row); skips analysis")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, _, err := loadCLIConfig(*configPath)
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
//
// It enumerates LOCAL tracks only. Analysis decodes a file with
// sox/ffmpeg, so a row routed from a UPnP upstream has nothing to
// analyse — `ResolveChecked` cannot resolve it by construction, and
// every one of them landed in `res.missing`. On the hybrid fixture (89
// local tracks + 15,283 routed from a Chord 2Go) that meant 15,283
// futile resolve calls per hourly sweep, reported to the operator as
// `total 15372, missing 13553` next to a coverage block reading
// `totalLocal 89` — two numbers for the same library, disagreeing,
// with the alarming one attached to the thing that looks like an error
// count. Store.TrackPathsLocal carries the same UPnP anti-join as
// Store.AnalysisCoverage, so the sweep and the coverage tile now
// describe the same set.
func collectAnalysisCandidates(ctx context.Context, store *manifest.Store, resolver *bridgefs.Resolver, outputDir, filter string, force bool) (analysisScanResult, error) {
	paths, err := store.TrackPathsLocal(ctx)
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
			// WantsAudioMD5Retry is the one thing here that is not a
			// freshness check. mtime, size and schema version are all
			// unchanged for a row whose audio-MD5 pass failed for a
			// reason that says nothing about the file — a pipe or spawn
			// failure under load, a faulted read, a killed child — so
			// without it the row is skipped forever and a one-second
			// blip is permanently recorded as "unverifiable". The
			// counter behind it is capped (AudioMD5MaxAttempts), so
			// this re-enqueues a bounded number of times and then stops
			// asking; each retry is a full re-analysis, since the
			// pipeline is one decode rather than resumable stages.
			if existing, gerr := store.GetAnalysis(ctx, rel); gerr == nil && existing != nil &&
				existing.SourceMTimeNS == info.ModTime().UnixNano() &&
				existing.SourceSize == info.Size() &&
				existing.SchemaVersion == analyze.WaveformSchemaVersion &&
				!existing.WantsAudioMD5Retry() {
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

// analysisSweeperSettleDelay is the serve-side sweeper's startup settle
// window (let any startup scan land before the first candidate walk).
// A var (not const) purely as the test seam — production never mutates.
var analysisSweeperSettleDelay = 90 * time.Second

// analysisSweeper holds what one auto-analysis pass needs. A struct rather
// than a parameter list for the reason fingerprintSweeper and
// autoOptimizeSweeper are: the loop and the pass want different things, and
// threading nine values through the loop to reach the pass is how a gate goes
// missing without anyone noticing.
type analysisSweeper struct {
	store     *manifest.Store
	resolver  *bridgefs.Resolver
	outputDir string
	pool      *analyze.Pool
	// enabled is the LIVE analysis gate.
	//
	// The pool is constructed unconditionally (see runServe, "always
	// construct, never stop"), which is what makes analysis.enabled hot for
	// every READ surface. Before that conversion the sweeper sat inside
	// `if analysisActive {` and the block WAS its gate; the conversion moved
	// every reader to the live predicate and left the WRITE path with none.
	// So on a default config (analysis.enabled is false) the bridge still
	// forked a decode per track 90 s after every boot and — because
	// Store.UpsertAnalysis advances indexed_at — pushed a whole-library
	// delta to every paired device, while /v1/analysis/* went on 404ing.
	enabled func() bool
}

// active reports whether a pass may run. A nil predicate reads as OFF, not
// on: this sweeper's whole failure mode was doing unrequested work, so the
// direction to fail in is settled. (runFingerprintSweeper's nil arm reads the
// other way; changing it belongs with its own tests, not here.)
func (s *analysisSweeper) active() bool { return s != nil && s.enabled != nil && s.enabled() }

// runAnalysisSweeper is the serve-side auto-analysis loop. After an
// initial settle delay (let any startup scan land) and then on every
// `interval` tick — or immediately on a `nudge` (the scanner's
// post-scan hook and the admin "Analyze now" button both send one) —
// it enqueues tracks missing a fresh waveform to the long-lived pool.
// Idempotent — the scan-skip gate means already-analyzed tracks are
// skipped, so a re-sweep over an unchanged library enqueues nothing.
// Generation also stays available via the `bridge analyze` CLI. Honors
// ctx for clean shutdown; a saturated queue just defers the rest to
// the next tick.
//
// nudge is a buffered-1 channel; senders use a non-blocking send so a
// pending nudge coalesces with the next sweep. status (nil-safe)
// records the sweep lifecycle for the admin Jobs surface.
//
// Same rule as runFingerprintSweeper: a disabled pass records NO status, so
// the Jobs card keeps the last real breakdown instead of overwriting it with
// an empty one.
//
// The dependencies travel as an analysisSweeper — the same shape
// fingerprintSweeper and autoOptimizeSweeper already use in this package,
// and what keeps this entry point inside the parameter budget.
func runAnalysisSweeper(ctx context.Context, s *analysisSweeper, interval func() time.Duration, nudge, rearm <-chan struct{}, status *sweepStatus[admin.AnalysisSweepCounts]) {
	sweep := func() {
		if !s.active() {
			return
		}
		status.sweepStarted()
		// counts stays nil on failure/cancel so sweepFinished keeps the
		// previous successful breakdown (see sweepStatus.sweepFinished).
		var counts *admin.AnalysisSweepCounts
		defer func() { status.sweepFinished(counts) }()

		res, err := collectAnalysisCandidates(ctx, s.store, s.resolver, s.outputDir, "", false)
		if err != nil {
			// A cancelled context here is a normal shutdown, not a fault —
			// same suppression the fingerprint sweeper applies (Gemini on
			// PR #619).
			if ctx.Err() == nil {
				logger.Warn("auto-analysis sweep: list tracks", "err", err)
			}
			return
		}
		enqueued := 0
		saturated := false
	enqueueLoop:
		for _, c := range res.candidates {
			if ctx.Err() != nil {
				return
			}
			switch err := s.pool.Enqueue(c); {
			case err == nil:
				enqueued++
			case errors.Is(err, analyze.ErrQueueFull), errors.Is(err, analyze.ErrPoolClosed):
				// Queue saturated (or shutting down) — leave the rest
				// for the next tick rather than spinning.
				saturated = true
				break enqueueLoop
			}
		}
		if enqueued > 0 {
			if saturated {
				logger.Info("auto-analysis sweep enqueued tracks (queue now full)", "count", enqueued)
			} else {
				logger.Info("auto-analysis sweep enqueued tracks", "count", enqueued)
			}
		}
		counts = &admin.AnalysisSweepCounts{
			Total:          res.total,
			UpToDate:       res.skipped,
			DSDExcluded:    res.dsdSkipped,
			ZeroByte:       res.emptySkipped,
			Missing:        res.missing,
			Enqueued:       enqueued,
			QueueSaturated: saturated,
		}
	}

	// Cadence (settle delay, one-drain semantics, tick-or-nudge) lives in
	// the shared runSweepLoop — see its docstring for why the nudge is
	// drained exactly once.
	runSweepLoop(ctx, status, analysisSweeperSettleDelay, interval, nudge, rearm, sweep)
}
