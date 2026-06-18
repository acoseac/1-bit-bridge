// `bridge artwork` CLI subcommand: maintenance for the on-disk
// artwork cache at <dataDir>/artwork/. Today the only subaction is
// `--gc` (garbage-collect orphaned cache files).
//
// Two cleanup targets:
//   - scanner-side `local-<sha256>-500.jpg` files written by
//     `stampLocalArtwork` for embedded ID3 APIC bytes / folder-level
//     cover.jpg. Track rows reference these via the
//     `local-<hash>` sentinel in `artworkMBID`.
//   - enricher-side `<mbid>-500.jpg` files written by the
//     MusicBrainz / CAA fetch path. Track rows reference these via
//     the raw MBID UUID in `artworkMBID`.
//
// Both shapes share `artworkMBID` as the JSON-tag pointer, and the
// store's `ArtworkMBIDsInUse()` returns the distinct set of
// referenced ids. Any file in the artwork dir whose stem (filename
// minus the `-500.jpg` suffix) isn't in that set is an orphan.
//
// **Manual subcommand, NOT auto-tail-of-Scan()**: per Gemini A10 /
// iOS bug review #10. Operators on low-IOPS hosts (Pi SD cards)
// want predictable maintenance windows; running GC at the tail of
// every periodic Scan() would spike disk I/O exactly when the user
// expected the scan to "finish".
//
// Per Gemini A10 / iOS bug review #10.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// artworkDirName lives under cfg.DataDir. Single source of truth for
// the cache directory — kept in sync with the scanner's
// `artworkDirBridge` resolution in main.go.
const artworkDirName = "artwork"

// artworkCacheSuffix is the trailing portion of every cached file
// (`-500.jpg`). Files in the artwork dir that don't end with this are
// skipped — any future cache shape (`-300.jpg`, `-thumb.jpg`) gets
// added here without touching the orphan-detection loop.
const artworkCacheSuffix = "-500.jpg"

// artworkGCConfirmPhrase is the exact string operators must pass via
// `--confirm` to authorize a destructive `--gc` run. Typed-phrase
// confirmation matches the project convention for destructive CLI
// surfaces (e.g. `bridge tsnet logout` requires typing `WIPE`); a
// boolean `--yes` flag would be too easy to typo into a real
// deletion. Per CodeRabbit Major round-1 on PR #167.
const artworkGCConfirmPhrase = "GC-ARTWORK"

func artworkCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("artwork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	gc := fs.Bool("gc", false, "remove cached artwork files no longer referenced by any track row")
	dryRun := fs.Bool("dry-run", false, "list orphans without removing them (use with --gc)")
	confirm := fs.String("confirm", "", "type "+artworkGCConfirmPhrase+" to authorize destructive deletion (required unless --dry-run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*gc {
		fmt.Fprintln(stderr, "Usage: bridge artwork --gc [--dry-run | --confirm "+artworkGCConfirmPhrase+"] [--config bridge.yaml]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Removes cached artwork files (local-<hash>-500.jpg, <mbid>-500.jpg) under")
		fmt.Fprintln(stderr, "<dataDir>/artwork/ that no track row references. Use --dry-run to preview")
		fmt.Fprintln(stderr, "or --confirm "+artworkGCConfirmPhrase+" to authorize destructive deletion.")
		return 2
	}

	// Typed-phrase confirmation gate (CodeRabbit Major round-1 on PR
	// #167). `--gc` without `--dry-run` requires the operator to pass
	// the exact phrase via `--confirm`. Exact match (no prefix
	// tolerance) — fat-fingered yes/y/Y must NOT permit a destructive
	// sweep. Mirrors the existing `bridge tsnet logout` pattern that
	// requires typing `WIPE`.
	if !*dryRun && *confirm != artworkGCConfirmPhrase {
		fmt.Fprintf(stderr, "refusing to delete without --confirm %s (or use --dry-run)\n", artworkGCConfirmPhrase)
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load: %v\n", err)
		return 2
	}

	// Use the shared `manifest.DefaultDBPath` constructor to keep CLI
	// behaviour aligned with `serveCmd` / `tokenCmd` / etc. Pre-fix
	// `<dataDir>/data/bridge.db` was a hardcoded path that didn't
	// match production layout (= `<dataDir>/bridge.db`) and would
	// have opened an empty store on every operator run, with the GC
	// pass then deleting every cached artwork file as "orphan".
	// CodeRabbit Major round-1 on PR #167.
	storePath := manifest.DefaultDBPath(cfg.DataDir)
	store, err := manifest.OpenStore(storePath)
	if err != nil {
		fmt.Fprintf(stderr, "open store at %q: %v\n", storePath, err)
		return 1
	}
	defer store.Close()

	artworkDir := filepath.Join(cfg.DataDir, artworkDirName)
	return runArtworkGC(ctx, stdout, stderr, store, artworkDir, *dryRun)
}

func runArtworkGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, artworkDir string, dryRun bool) int {
	mbidsInUse, err := store.ArtworkMBIDsInUse(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list referenced artwork ids: %v\n", err)
		return 1
	}
	known := make(map[string]bool, len(mbidsInUse))
	for _, m := range mbidsInUse {
		known[m] = true
	}

	var removed, kept, failed, skipped int
	walkErr := filepath.WalkDir(artworkDir, func(path string, d os.DirEntry, walkErr error) error {
		// Honor ctx cancellation so SIGINT actually stops the
		// sweep mid-walk instead of churning through the rest of
		// the directory. CodeRabbit Major on PR #217.
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// Cache dir may not exist yet (no scan ever ran). Treat
			// as empty rather than erroring — same shape as the
			// upscale GC (`runGC` in upscale.go).
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// Only consider files matching the cache suffix. Anything
		// else (a stray README, a partial download, an old-format
		// thumb) is treated as out-of-scope and skipped — a
		// future GC pass with broader coverage can extend this.
		base := filepath.Base(path)
		if !strings.HasSuffix(base, artworkCacheSuffix) {
			skipped++
			return nil
		}
		stem := strings.TrimSuffix(base, artworkCacheSuffix)
		if known[stem] {
			kept++
			return nil
		}
		if dryRun {
			fmt.Fprintf(stdout, "would remove: %s\n", path)
			removed++
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
		// Operator-interrupt path: surface a clean "interrupted"
		// message instead of the raw `context canceled` error so
		// the SIGINT case reads as intentional rather than a bug.
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			fmt.Fprintln(stderr, "artwork gc interrupted")
			return 1
		}
		fmt.Fprintf(stderr, "walk artwork dir: %v\n", walkErr)
		return 1
	}
	if dryRun {
		fmt.Fprintf(stdout, "GC dry-run: %d orphan(s) would be removed, %d kept, %d skipped (non-cache file).\n",
			removed, kept, skipped)
	} else {
		fmt.Fprintf(stdout, "GC: removed %d orphan(s), kept %d known cache file(s), %d skipped, %d failure(s).\n",
			removed, kept, skipped, failed)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// artworkCacheSweepInterval is how often runArtworkCacheSweeper re-checks
// the cache size when a cap is configured. A constant (not config-exposed)
// to keep the artwork config surface to the single cap knob. 15 min bounds
// the over-cap overshoot during a bulk premium-cover harvest (rate-limited
// upstream to ~1 cover/s) to a few hundred MB, while keeping the directory
// walk — O(files), a few thousand entries on a real library — negligible.
const artworkCacheSweepInterval = 15 * time.Minute

// artworkCacheSweepSettleDelay defers the first sweep after startup so it
// doesn't compete with the initial scan + enrichment I/O. Mirrors the
// analysis sweeper's settle pattern.
const artworkCacheSweepSettleDelay = 90 * time.Second

// runArtworkCacheSweeper enforces config.ArtworkConfig.CacheMaxBytes by
// periodically evicting the least-recently-modified files from the artwork
// cache. It is a no-op (and never spawned by runServe) when capBytes <= 0 —
// the historical "unbounded" default. Lives off the shared scanCtx so a
// SIGINT cancels it alongside the other periodic workers.
//
// Recency is the file mtime, NOT atime: atime needs per-OS syscall code and
// is frozen on noatime mounts, while mtime is portable and meaningful (it's
// when the cover entered / was last rewritten in the cache). The sweeper
// only READS timestamps — it never bumps mtime on serve, which would break
// http.ServeContent's Last-Modified / 304 conditional caching.
//
// Eviction caveat: a still-referenced cover that gets evicted makes
// /v1/artwork/{mbid} answer 202-pending (the track still references the
// MBID) until a later re-enrichment re-caches it; iOS renders a placeholder
// on 202. That's the accepted tradeoff of a disk-pressure valve — the
// alternative (a full disk) is worse. Eviction is oldest-first, so an
// actively-served (recently re-written) cover is the last to go.
func runArtworkCacheSweeper(ctx context.Context, artworkDir string, capBytes int64, interval time.Duration) {
	if capBytes <= 0 {
		return
	}
	sweep := func() {
		evicted, freed, err := sweepArtworkCache(ctx, artworkDir, capBytes)
		if err != nil {
			// A ctx cancel mid-sweep is a clean shutdown, not a fault.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			logger.Warn("artwork cache sweep", "err", err, "dir", artworkDir)
			return
		}
		if evicted > 0 {
			logger.Info("artwork cache LRU eviction",
				"evicted", evicted, "freedBytes", freed, "capBytes", capBytes)
		}
	}

	select {
	case <-ctx.Done():
		return
	case <-time.After(artworkCacheSweepSettleDelay):
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

// sweepArtworkCache enforces the artwork-cache size cap via
// least-recently-modified eviction. It walks artworkDir, sums the size of
// every cache file (final `*.jpg`; in-flight `*.jpg.tmp` atomic-write temps
// and any non-cache stray are ignored), and — if the total exceeds capBytes
// — removes files oldest-mtime-first until the total is back under a 90%
// low-water mark. Returns the count of files evicted and the bytes freed.
//
// capBytes <= 0 is a no-op (unbounded). A missing cache dir (no scan has run
// yet) is not an error. Pure except for the os.Remove side effect, so it's
// unit-testable with a real temp dir.
func sweepArtworkCache(ctx context.Context, artworkDir string, capBytes int64) (evicted int, freed int64, err error) {
	if capBytes <= 0 {
		return 0, 0, nil
	}
	type cacheFile struct {
		path string
		size int64
		mod  time.Time
	}
	var files []cacheFile
	var total int64
	walkErr := filepath.WalkDir(artworkDir, func(path string, d os.DirEntry, walkErr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if walkErr != nil {
			// Cache dir absent (no scan/enrich has run yet) — treat as empty.
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		// Only final cache files count. Every cached cover ends in `.jpg`;
		// the atomic-write temp files end in `.jpg.tmp` (excluded) and any
		// stray non-cache file is left alone — same scoping rationale as the
		// GC's suffix gate. Match on the full path (a separator can't appear
		// in the `.jpg` suffix) to skip a filepath.Base alloc per file.
		if !strings.HasSuffix(path, ".jpg") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			// File vanished mid-walk (concurrent eviction / rename) — skip
			// that one entry. Any OTHER stat error (permission, I/O) would
			// silently undercount the cache and let the sweep report success
			// while still over cap, so surface it and let the next tick
			// retry rather than swallowing it.
			if errors.Is(ierr, os.ErrNotExist) {
				return nil
			}
			return ierr
		}
		files = append(files, cacheFile{path: path, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, os.ErrNotExist) {
			return 0, 0, nil
		}
		return evicted, freed, walkErr
	}
	if total <= capBytes {
		return 0, 0, nil
	}

	// Oldest first. Evict down to a 90% low-water mark so the next cover
	// write doesn't immediately re-trip the cap (batches the work). Tie-break
	// on path so the order is deterministic when two files share an mtime.
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod.Equal(files[j].mod) {
			return files[i].path < files[j].path
		}
		return files[i].mod.Before(files[j].mod)
	})
	lowWater := capBytes - capBytes/10
	for _, f := range files {
		if total <= lowWater {
			break
		}
		if cerr := ctx.Err(); cerr != nil {
			return evicted, freed, cerr
		}
		if rmErr := os.Remove(f.path); rmErr != nil {
			if errors.Is(rmErr, os.ErrNotExist) {
				// Already removed concurrently (e.g. an operator-run
				// `bridge artwork --gc`): the bytes are gone, so keep the
				// running total accurate to avoid over-evicting live entries
				// to reach the low-water mark. Not counted as our eviction.
				total -= f.size
				continue
			}
			// Best-effort: a file held open by an in-flight serve (Windows)
			// is non-fatal — leave it for the next pass.
			logger.Warn("artwork cache evict", "path", f.path, "err", rmErr)
			continue
		}
		total -= f.size
		freed += f.size
		evicted++
	}
	return evicted, freed, nil
}
