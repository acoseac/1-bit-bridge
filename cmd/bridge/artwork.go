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
	"strings"

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
