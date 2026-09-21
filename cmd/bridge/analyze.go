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
	"github.com/acoseac/1-bit-bridge/internal/integrity"
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
	retryFailed := fs.Bool("retry-failed", false, "clear recorded decode failures (honours --filter) so sources the decoder refused are offered again, then analyze")
	gc := fs.Bool("gc", false, "remove orphan waveform sidecars (files with no DB row); skips analysis")
	allowEmpty := fs.Bool("allow-empty", false, "with --gc: proceed even when no analysis row references any waveform (the library really was emptied); refused by default, because an empty catalog makes every file on disk look like an orphan")
	allowMassOrphans := fs.Bool("allow-mass-orphans", false, "with --gc: unlink waveform files no row references even when there are more of them than the catalog has rows in total (the files really are junk); refused by default, because that shape is a catalog that lost its index")
	if !parseTranscodeArgs(fs, "analyze", args, stderr) {
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
		return runAnalyzeGC(ctx, stdout, stderr, store, outputDir, *allowEmpty, *allowMassOrphans)
	}

	resolver := bridgefs.New(cfg.LibraryRoots)
	workerCount := *workers
	if workerCount <= 0 {
		workerCount = cfg.Analysis.EffectiveWorkers()
	}
	if *retryFailed {
		if code := runAnalyzeRetryFailed(ctx, stdout, stderr, store, *filter, *dryRun); code != 0 {
			return code
		}
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

// runAnalyzeRetryFailed clears the analysis-failure debounce so refused
// sources are offered to the walk again. Runs BEFORE the walk in the same
// invocation, so `bridge analyze --retry-failed` both re-opens and retries.
//
// Under --dry-run it COUNTS and clears nothing. A dry run that quietly
// re-opened 30 suppressed sources would be the one thing a dry run must not
// do, and refusing the combination outright would make the operator run the
// destructive form to find out how much it would touch.
//
// Honours --filter EXACTLY, which is why it goes through the explicit-path
// form rather than a prefix range: --filter is a case-sensitive SUBSTRING
// match, and no byte range expresses that. The paths come from the recorded
// set, so the substring is applied to the same spelling the walk applies it
// to.
//
// An empty scope is the whole library and says so — that is the operator
// typing `bridge analyze --retry-failed` with no filter, which is the
// documented way to re-open everything. A filter matching nothing clears
// nothing, and the two cases are different functions in the store so they
// cannot be reached by the same argument.
func runAnalyzeRetryFailed(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, filter string, dryRun bool) int {
	scope := "whole library"
	if filter != "" {
		scope = fmt.Sprintf("matching %q", filter)
	}
	// The listing is needed for the dry run either way, and for the filtered
	// clear it is what turns a substring into the explicit path set.
	rows, err := store.ListUnreadableTracksForAdmin(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list recorded decode failures: %v\n", err)
		return 1
	}
	var paths []string
	for _, r := range rows {
		if filter == "" || strings.Contains(r.Path, filter) {
			paths = append(paths, r.Path)
		}
	}
	if dryRun {
		fmt.Fprintf(stdout, "analyze: would clear %d recorded decode failure(s) (%s)\n", len(paths), scope)
		for _, p := range paths {
			fmt.Fprintf(stdout, "  %s\n", p)
		}
		// Say which library the summary below describes. Nothing was cleared,
		// so the walk still applies these suppressions and its `to analyze`
		// count excludes the very paths just listed — accurate about the
		// library as it stands, and easy to read as a prediction of the real
		// run if nobody says otherwise. Threading a bypass set into
		// collectAnalysisCandidates would make the number predictive, at the
		// cost of a new parameter on the function the CLI and the serve-side
		// sweeper share — and that function's whole job is that the two
		// cannot drift on what "needs analysis" means. A sentence is the
		// cheaper honest answer. (CodeRabbit on #947.)
		if len(paths) > 0 {
			fmt.Fprintf(stdout, "analyze: the summary below describes the library AS IT STANDS — "+
				"those %d are still suppressed, so they are counted unreadable rather than "+
				"to-analyze. Re-run without --dry-run to clear them.\n", len(paths))
		}
		return 0
	}
	var n int64
	if filter == "" {
		// The whole-library form is its own store call, not the by-paths one
		// with everything listed: "clear the library" and "clear these
		// paths" must not be spellable the same way, and a list built from a
		// read that raced a concurrent write would silently miss rows.
		n, err = store.ClearAllAnalysisFailures(ctx)
	} else {
		n, err = store.ClearAnalysisFailuresByPaths(ctx, paths)
	}
	if err != nil {
		fmt.Fprintf(stderr, "clear analysis failures: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "analyze: cleared %d recorded decode failure(s) (%s)\n", n, scope)
	return 0
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

	// `missing` is reported as "unresolvable", not "unreadable". It used to
	// carry the latter word and now cannot: `unreadable` is a different set
	// with a different remedy (files to replace, vs paths this bridge cannot
	// address at all), and two counts sharing one label is how an operator
	// reads the wrong number.
	fmt.Fprintf(stdout, "analyze: %d tracks, %d to analyze, %d up-to-date, %d skipped (DSD), %d empty, %d unresolvable, %d unreadable\n",
		res.total, len(candidates), res.skipped, res.dsdSkipped, res.emptySkipped, res.missing, res.unreadable)
	if res.unreadable > 0 {
		subject, verb, object := "sources", "are", "the files"
		if res.unreadable == 1 {
			subject, verb, object = "source", "is", "the file"
		}
		fmt.Fprintf(stdout, "analyze: %d %s the decoder refused %d times running %s no longer retried; "+
			"re-run with --retry-failed, or replace %s (see the console's unreadable list)\n",
			res.unreadable, subject, manifest.AnalysisFailureThreshold(), verb, object)
	}
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
func runAnalyzeGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, outputDir string, allowEmpty, allowMassOrphans bool) int {
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
	//
	// BOTH spellings of every row go in: the recorded `waveform_path` and
	// the canonical path under outputDir (analyze.AnalyzeSpec.SidecarPath,
	// the layout the pool writes). `waveform_path` is absolute under the
	// dataDir that wrote it, so a dataDir moved to a new host leaves every
	// row naming the old one — and a known set of recorded paths alone
	// then reads the whole moved waveform tree as orphans and unlinks it.
	// Same class as `upscale --gc`'s 2026-09-20 relocation hazard, the
	// cheaper-to-rebuild half; the serve path has no adoption for waveforms
	// yet (see the doctor's sidecar-paths check), so at least the files
	// survive for the day it does.
	known := make(map[string]struct{}, 2*len(rows))
	for _, r := range rows {
		if r.WaveformPath != "" {
			known[strings.ToLower(filepath.Clean(r.WaveformPath))] = struct{}{}
		}
		if r.SourcePath != "" {
			canonical := analyze.AnalyzeSpec{OutputDir: outputDir, SourceLibraryRel: r.SourcePath}.SidecarPath()
			known[strings.ToLower(filepath.Clean(canonical))] = struct{}{}
		}
	}

	if _, statErr := os.Stat(outputDir); errors.Is(statErr, fs.ErrNotExist) {
		fmt.Fprintf(stdout, "analyze --gc: no waveform dir at %s; nothing to do\n", outputDir)
		return 0
	}
	// Same refusal as `upscale --gc`, for the same reason: every file misses an
	// empty `known`, so the walk below would classify the whole waveform cache
	// as orphaned. Cheaper to rebuild than a PCM rendition, which is why this
	// is the milder of the two — not a different rule.
	if code := gcRefuseEmptyKnownSetOverPopulatedDir(stderr, outputDir,
		"analysis row", "waveform directory", len(known), allowEmpty); code != 0 {
		return code
	}

	// One classification pass, shared with `upscale --gc` and the doctor's
	// variants-index check, so the three cannot disagree about what a tree
	// that lost its index looks like. It also brings the dot-directory
	// prune this walk never had: `<dataDir>/waveforms` is bridge-owned, but
	// the rule ("a sidecar walk prunes dot-directories AT THE WALK") is not
	// one to hold in one of two places.
	inv, invErr := integrity.TakeSidecarInventory(ctx, outputDir, known, integrity.SidecarInventoryOptions{
		// analyze's own constants, not a literal: a sweep whose idea of
		// the extension drifts from the writer's manages nothing, and
		// says so by reporting zero of everything.
		Consider: func(name string) bool { return strings.HasSuffix(name, analyze.WaveformExt) },
		Scratch: func(name string) bool {
			return strings.HasSuffix(name, analyze.WaveformExt+analyze.AnalysisTmpSuffix)
		},
	})
	if invErr != nil {
		if errors.Is(invErr, context.Canceled) || errors.Is(invErr, context.DeadlineExceeded) {
			fmt.Fprintln(stderr, "analyze --gc: interrupted")
			return 130
		}
		// Fail closed rather than the pre-#940 "skip the unreadable entry
		// and keep deleting": a tree the walk could not read is not
		// evidence its files are junk.
		fmt.Fprintf(stderr, "analyze --gc: walk %s: %v\n", outputDir, invErr)
		return 1
	}
	if inv.Unreadable > 0 {
		fmt.Fprintf(stderr, "analyze --gc: %d director(y/ies) under %s could not be read; their contents were neither counted nor removed.\n",
			inv.Unreadable, outputDir)
	}
	if !allowMassOrphans {
		if reason := integrity.MassOrphanRefusal(inv.Orphans, inv.Files, len(rows), analysisGCMaxOrphanPercent); reason != "" {
			fmt.Fprintf(stderr, "analyze --gc: refusing to run — %s.\n", reason)
			fmt.Fprintln(stderr, "  A catalog this much smaller than the tree it describes usually means the INDEX was lost —")
			fmt.Fprintln(stderr, "  a bridge.db restored from an older snapshot, or a --config naming another install.")
			fmt.Fprintf(stderr, "  The tree is %s. Nothing was unlinked.\n", outputDir)
			fmt.Fprintln(stderr, "  If the files really are junk, re-run with --allow-mass-orphans; `bridge analyze --force` rebuilds")
			fmt.Fprintln(stderr, "  waveforms from source, so this side is recoverable in a way the variant tree is not.")
			return 1
		}
	}

	var removed, kept, failed int
	// The scratch half is unconditional and outside the ratio: a
	// `.waveform.bin.tmp` is this sweep's own half-written litter, never
	// the operator's data, so a crashed run must not be able to trip the
	// guard on the next one.
	for _, set := range [][]string{inv.ScratchPaths, inv.OrphanPaths} {
		for _, path := range set {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "analyze --gc: interrupted")
				return 130
			}
			if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				fmt.Fprintf(stderr, "analyze --gc: remove %s: %v\n", filepath.Base(path), rmErr)
				failed++
				continue
			}
			removed++
		}
	}
	kept = inv.Known
	fmt.Fprintf(stdout, "analyze --gc: removed %d orphan sidecar(s), kept %d, %d failure(s)\n", removed, kept, failed)
	// Exit 0 even with per-file failures, as this command always has —
	// unlike `upscale --gc`, which exits 1. Reported rather than silent
	// (it was neither counted nor printed before), but promoting it to a
	// non-zero exit would change what a cron'd analyze --gc reports, which
	// is a decision for whoever wants it, not a side effect of this guard.
	return 0
}

// analysisGCMaxOrphanPercent is the mass-orphan threshold for waveforms.
// The same 20 as integrity.variantSweepMaxDeletePercent's default, but a
// constant rather than a config read: the `integrity.*` knob is about the
// variants catalog the watcher sweeps, and waveforms have no watcher —
// borrowing the number keeps one answer to "how much of a tree is too
// much" without pretending the setting covers a table it never named.
const analysisGCMaxOrphanPercent = 20

// analysisScanResult bundles the enumeration outcome shared by the CLI
// batch path and the serve-side auto-analysis sweeper.
type analysisScanResult struct {
	candidates   []analyze.AnalyzeSpec
	total        int
	skipped      int // up-to-date sidecar (scan-skip gate hit)
	dsdSkipped   int // DSD source (sox can't decode)
	emptySkipped int // zero-byte source (unanalyzable — failed/incomplete upload)
	missing      int // unresolvable / directory
	// unreadable is how many sources the decoders have refused enough
	// consecutive times, against the version currently on disk, to stop
	// being offered (manifest's analysis-failure debounce). Reported
	// separately from emptySkipped because the remedy differs: a zero-byte
	// file is an upload to finish, these are files to replace.
	unreadable int
}

// collectAnalysisCandidates enumerates library tracks that need a
// waveform: filtered by `filter` (substring; "" = all), DSD skipped,
// unresolvable skipped, zero-byte skipped, sources the decoders have
// repeatedly refused skipped, and — unless `force` — up-to-date sidecars
// skipped via the scan-skip gate (matching source mtime + size + schema).
// Shared by `bridge analyze` and the serve-side sweeper so the two can't
// drift on what "needs analysis" means.
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
	// One query for the whole suppressed set rather than a question per
	// path: this walk already runs a GetAnalysis per track, and the set is
	// bounded by how many files are broken.
	//
	// A read failure aborts the walk instead of degrading to "nothing is
	// suppressed". Degrading would silently restore the unbounded retry loop
	// this gate exists to close, and on the serve side it would do so on
	// every tick with no operator in the loop.
	suppressed, err := store.SuppressedAnalysisPaths(ctx)
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
		// A source the decoders have refused
		// manifest.AnalysisFailureThreshold() times running, against the
		// version on disk, stops being offered. The zero-byte skip above
		// could not cover these — its own comment says so: it "stays
		// mtime/size-driven so it can't suppress a real file that's only
		// TRANSIENTLY failing (those keep a non-zero size)", and a truncated
		// file keeps a non-zero size. What makes THIS skip safe is that the
		// decoder reached a verdict (analyze.ErrSourceUnreadable) three
		// separate times.
		//
		// AFTER the freshness gate and OUTSIDE the --force guard, which is
		// two decisions. After, because a track that already has a fresh
		// waveform is up-to-date, not unreadable, and Store.AnalysisCoverage
		// makes the same call ("suppressed AND NOT analysed-fresh") — the
		// state is unreachable today, since a success clears the strikes,
		// but two surfaces that agree only by unreachability agree by luck.
		// Outside, because --force bypasses the FRESHNESS gate, not
		// unanalyzability: the same posture the zero-byte gate takes.
		// `bridge analyze --retry-failed` is the way past this one, and
		// repairing the file is the way that needs no flag.
		//
		// The live stat has the last word. `sup` is the version the manifest
		// row described when the verdicts landed; between an operator
		// replacing the file and the next scan it describes the old one, so
		// a mismatch means the thing that was refused is not the thing on
		// disk, and the file is analysed.
		if sup, ok := suppressed[rel]; ok &&
			sup.SizeBytes == info.Size() && sup.MTimeNS == info.ModTime().UnixNano() {
			res.unreadable++
			continue
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
	runSweepLoop(ctx, status, analysisSweeperSettleDelay, interval, nudge, rearm, func() {
		if !s.active() {
			return
		}
		status.sweepStarted()
		status.sweepFinished(s.sweep(ctx))
	})
}

// sweep runs one pass: collect candidates, enqueue them, return the per-run
// counts for the admin recorder — nil on failure or cancellation, so the
// recorder keeps the last successful breakdown rather than replacing it with
// an empty one.
//
// A method rather than a closure inside runAnalysisSweeper, matching
// fingerprintSweeper.sweep. The loop is about WHEN a pass runs and this is
// about what a pass DOES; keeping them in one function put both concerns past
// the complexity ceiling and, more to the point, made the gate easy to miss
// among the scheduling.
func (s *analysisSweeper) sweep(ctx context.Context) *admin.AnalysisSweepCounts {
	res, err := collectAnalysisCandidates(ctx, s.store, s.resolver, s.outputDir, "", false)
	if err != nil {
		// A cancelled context here is a normal shutdown, not a fault —
		// same suppression the fingerprint sweeper applies (Gemini on
		// PR #619).
		if ctx.Err() == nil {
			logger.Warn("auto-analysis sweep: list tracks", "err", err)
		}
		return nil
	}
	enqueued, saturated, cancelled := s.enqueueAll(ctx, res.candidates)
	if cancelled {
		return nil
	}
	if enqueued > 0 {
		if saturated {
			logger.Info("auto-analysis sweep enqueued tracks (queue now full)", "count", enqueued)
		} else {
			logger.Info("auto-analysis sweep enqueued tracks", "count", enqueued)
		}
	}
	return &admin.AnalysisSweepCounts{
		Total:          res.total,
		UpToDate:       res.skipped,
		DSDExcluded:    res.dsdSkipped,
		ZeroByte:       res.emptySkipped,
		Missing:        res.missing,
		Unreadable:     res.unreadable,
		Enqueued:       enqueued,
		QueueSaturated: saturated,
	}
}

// enqueueAll offers every candidate to the pool. `cancelled` reports a ctx
// cancel mid-loop, which the caller turns into a nil count — a partial pass is
// not a breakdown worth showing on the Jobs card.
func (s *analysisSweeper) enqueueAll(ctx context.Context, candidates []analyze.AnalyzeSpec) (enqueued int, saturated, cancelled bool) {
	for _, c := range candidates {
		if ctx.Err() != nil {
			return enqueued, saturated, true
		}
		switch err := s.pool.Enqueue(c); {
		case err == nil:
			enqueued++
		case errors.Is(err, analyze.ErrQueueFull), errors.Is(err, analyze.ErrPoolClosed):
			// Queue saturated (or shutting down) — leave the rest for the
			// next tick rather than spinning.
			return enqueued, true, false
		}
	}
	return enqueued, saturated, false
}
