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
// The same rule covers the console's derived tiers in `thumbs/`, filed
// `<key>-<size>.jpg`: a cover's under its artwork key, an artist
// portrait's under `artist-<mbid>` (manifest.ArtistThumbKey) for the
// `artistMBID` the enricher stamped on the track and fetched the portrait
// under, so the referenced set holds those too (artworkKeysInUse). The
// portraits themselves (`artist-<mbid>.jpg`, `artist-name-<sha>.jpg`)
// carry no size suffix and are out of the GC's scope.
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

	"github.com/acoseac/1-bit-bridge/internal/enrich"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/integrity"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// artworkDirName lives under cfg.DataDir. Single source of truth for
// the cache directory — kept in sync with the scanner's
// `artworkDirBridge` resolution in main.go.
const artworkDirName = "artwork"

// artworkCacheSuffixes are the trailing portions of every cached file
// the GC recognises — one per supported cover tier, DERIVED from
// enrich.SupportedCoverSizes (the writer-side contract) so a future
// tier added there is automatically visible to orphan GC. A hardcoded
// second list here is how the `-1200.jpg` tier would have gone
// invisible to GC forever. Files matching none are skipped — any
// future non-size cache shape still gets an explicit entry.
var artworkCacheSuffixes = func() []string {
	suffixes := make([]string, 0, len(enrich.SupportedCoverSizes))
	for _, s := range enrich.SupportedCoverSizes {
		suffixes = append(suffixes, fmt.Sprintf("-%d.jpg", s))
	}
	return suffixes
}()

// artworkCacheStem returns the filename minus its recognised cache
// suffix, or ("", false) for out-of-scope files.
func artworkCacheStem(base string) (string, bool) {
	for _, suffix := range artworkCacheSuffixes {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix), true
		}
	}
	return "", false
}

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
	configPath := fs.String("config", "", configFlagUsage)
	gc := fs.Bool("gc", false, "remove cached artwork files no longer referenced by any track row")
	dryRun := fs.Bool("dry-run", false, "list orphans without removing them (use with --gc)")
	allowEmpty := fs.Bool("allow-empty", false, "with --gc: proceed even when no track row references any artwork (the library really was emptied); refused by default, because an empty catalog makes every cached file look like an orphan")
	confirm := fs.String("confirm", "", "type "+artworkGCConfirmPhrase+" to authorize destructive deletion (required unless --dry-run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if !*gc {
		fmt.Fprintln(stderr, "Usage: bridge artwork --gc [--dry-run | --confirm "+artworkGCConfirmPhrase+"] [--config bridge.yaml]")
		fmt.Fprintln(stderr, "")
		fmt.Fprintln(stderr, "Removes cached artwork files (local-<hash>-500.jpg, <mbid>-{250,500,1200}.jpg, and the")
		fmt.Fprintln(stderr, "thumbnails in thumbs/) under <dataDir>/artwork/ that no track row references. Use --dry-run to preview")
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

	cfg, _, err := loadCLIConfig(*configPath)
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
	return runArtworkGC(ctx, stdout, stderr, store, artworkDir, *dryRun, *allowEmpty)
}

// artworkWalkRoot turns the configured cache directory into the path a walk
// of the cache hands filepath.WalkDir, and reports whether there is a cache
// to walk at all. Every walk of the cache (the GC, its empty-store guard and
// the size cap) goes through it, so they agree about what the tree is: a
// guard that read "no files" over a cache its GC then walks is the guard
// waving the whole cache through.
//
// filepath.WalkDir Lstats its root and follows no link, so a cache
// directory that is itself a link (`<dataDir>/artwork -> /mnt/big/artwork`,
// the ordinary way to give the cache a disk of its own, or on Windows a
// directory junction) reached the callback as ONE non-directory entry, and
// every walk ended there. Measured on 6dfba62c: `artwork --gc` over a
// symlinked cache holding an orphan said "removed 0 orphan(s), kept 0 known
// cache file(s), 1 skipped" and left it, and the size cap over 170 bytes of
// covers against a cap of 100 evicted nothing, on every pass.
// fsutil.ResolveLinks is the sidecar walks' resolution (integrity's
// resolveSidecarRoot), a junction's included: the walk starts at the link's
// target, so what a walk removes is the path it visited, in the tree it
// walked (#1063's rule), and what a walk REPORTS it names under the
// configured directory (artworkReportPath).
//
// A cache that is not there (never written, or a link to nothing) is
// (_, false, nil), which every walk reads as an empty cache, as it always
// read a missing one. A root that resolves to something other than a
// directory is an error: unresolved, WalkDir visited a link to a file as
// one entry named for the link, which no walk counts; resolved, the GC
// would judge the target by its own name, and could remove it. "" is
// refused before anything resolves it: filepath.EvalSymlinks("") answers
// ".", and the walk would be of the working directory.
func artworkWalkRoot(artworkDir string) (walkRoot string, present bool, err error) {
	if artworkDir == "" {
		return "", false, errors.New("no artwork cache directory")
	}
	walkRoot, err = fsutil.ResolveLinks(artworkDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve artwork cache %s: %w", artworkDir, err)
	}
	info, err := os.Stat(walkRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	if !info.IsDir() {
		return "", false, fmt.Errorf("artwork cache %s is not a directory", artworkDir)
	}
	return walkRoot, true, nil
}

// artworkReportPath names a path a walk visited under the configured cache
// directory rather than the directory a link resolved it to, which is the
// spelling an operator knows the cache by. Naming only: a walk removes the
// path it visited.
func artworkReportPath(artworkDir, walkRoot, path string) string {
	rel, err := filepath.Rel(walkRoot, path)
	if err != nil {
		return path
	}
	return filepath.Join(artworkDir, rel)
}

// artworkCacheHasOrphans reports whether the cache at walkRoot holds at
// least one file the GC would remove: a cache file whose key is not in
// known. Used only by the empty-referenced-set refusal, so it stops at the
// first hit rather than walking the whole cache.
//
// A directory inside the cache that it cannot list is stepped over,
// silently, as the GC's own walk steps over it (and reports it): the
// question is whether the GC would remove files, and it cannot remove what
// it cannot list (artworkUnlistedDir). A file whose key is known is one the
// GC keeps, so a cache holding only those is nothing to refuse over.
func artworkCacheHasOrphans(walkRoot string, known map[string]bool) (bool, error) {
	found := false
	err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			if unlisted, _ := artworkUnlistedDir(walkRoot, path, d, walkErr); unlisted {
				return nil
			}
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if stem, ok := artworkCacheStem(filepath.Base(path)); ok && !known[stem] {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// artworkKeysInUse returns the cache keys a track row references, which are
// the GC's keep set: every artwork key (ArtworkMBIDsInUse), and the key the
// console files an artist portrait's derived tiers under,
// manifest.ArtistThumbKey of every artist MBID a track row carries
// (DistinctArtistMBIDs). That artist MBID is what the enricher stamps on
// the track and fetches the portrait under (`artist-<mbid>.jpg`), and the
// one the console's catalog asks for, so a thumbnail filed under it is in
// use exactly while a track row names the artist; until 2026-09-29 the
// keep set held artwork keys alone and the GC removed every artist
// thumbnail as an orphan.
//
// It asks the store, never the file name: a GC that kept whatever is named
// like an artist's thumbnail would keep one whose artist no track names any
// longer, for ever. The two halves come back apart because the empty-set
// refusal reads the artwork keys alone: the covers it protects are keyed
// by them, and a scanner-extracted `local-` cover does not come back.
//
// A thumbnail filed under a 16-hex artworkVersion alias is not in the set,
// and is an orphan: the console resolves an alias to the artwork key it
// stands for before it derives anything, so no build files a thumbnail
// under one and nothing would read it
// (TestAnArtworkAliasFilesItsThumbUnderTheResolvedKey).
func artworkKeysInUse(ctx context.Context, store *manifest.Store) (artworkKeys []string, known map[string]bool, err error) {
	artworkKeys, err = store.ArtworkMBIDsInUse(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list referenced artwork ids: %w", err)
	}
	artistMBIDs, err := store.DistinctArtistMBIDs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list referenced artist ids: %w", err)
	}
	known = make(map[string]bool, len(artworkKeys)+len(artistMBIDs))
	for _, k := range artworkKeys {
		known[k] = true
	}
	for _, m := range artistMBIDs {
		known[manifest.ArtistThumbKey(m)] = true
	}
	return artworkKeys, known, nil
}

// artworkUnlistedDir reports whether a walk error is a directory inside the
// artwork cache that the walk could not list, which every walk of the cache
// steps over and goes on; and quiet when that directory is the filesystem's
// own lost+found (integrity.IsFilesystemLostFound), which the walks step
// over without a word. The cache directory itself (walkRoot, the directory
// the walk started at) is not inside the cache: a walk that cannot list it
// can do nothing, and fails as it always has.
//
// Stepping over is sound here, and would not be in the sidecar sweeps
// (2026-09-28). The GC's verdict about a file is that file's name against
// the referenced keys and nothing else: no count over the tree, no ratio,
// nothing a missing directory could change about a file it can see. So a
// directory it cannot list hides the files in it from the GC and changes
// nothing else. The sidecar sweeps decide from a mass-orphan ratio over
// the whole tree, which a directory they cannot list can flip, and refuse
// such a walk (integrity.PartialWalkRefusal). Until then this walk stopped
// at the first such directory with exit 1, the orphans before it already
// removed and the ones after it never examined, and no flag past it; on a
// cache that is an ext4 volume's mount root that was every run. The size
// cap steps over for a reason of its own (sweepArtworkCache).
func artworkUnlistedDir(walkRoot, path string, d os.DirEntry, walkErr error) (unlisted, quiet bool) {
	if d == nil || !d.IsDir() || path == walkRoot {
		return false, false
	}
	return true, integrity.IsFilesystemLostFound(walkRoot, path, d, walkErr)
}

// artworkErrWithoutPath is what a filesystem error says about a path the
// walk visited, without the path it carries: the walks name the path
// themselves, under the configured cache directory (artworkReportPath),
// where the error names it the way the walk visited it.
func artworkErrWithoutPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func runArtworkGC(ctx context.Context, stdout, stderr io.Writer, store *manifest.Store, artworkDir string, dryRun, allowEmpty bool) int {
	artworkKeys, known, err := artworkKeysInUse(ctx, store)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	walkRoot, present, err := artworkWalkRoot(artworkDir)
	if err != nil {
		fmt.Fprintf(stderr, "artwork gc: %v\n", err)
		return 1
	}

	// FAIL CLOSED on an empty referenced set. Every cover in the cache
	// misses a set that holds no artwork key, so the walk below would
	// classify the whole cover cache as orphaned and unlink it — and the
	// scanner-written `local-<sha256>-500.jpg` covers are NOT re-fetchable:
	// the mtime skip gate means the scanner never re-reads those files, so
	// the loss survives every subsequent scan until an ExtractorVersion bump
	// or a row wipe.
	//
	// This is not hypothetical. The docblock above records this exact
	// deletion shipping once, via a hardcoded DB path that opened an empty
	// store; that fix corrected the path and left the shape that made a
	// wrong path catastrophic rather than merely wrong. The routes that
	// reach an empty set today are ordinary: a --config naming an install
	// whose dataDir is not the one being walked, a run between a
	// single<->multi root flip's WipeFilesystemTracks and the rescan that
	// refills it, or simply a store that has never been scanned.
	//
	// Empty means no ARTWORK key, whatever the artist keys (artworkKeysInUse):
	// the covers are what this protects, and they are keyed by artwork keys
	// alone. A cache holding nothing the walk would remove (nothing at all,
	// or only thumbnails of artists a track row names) is not an error —
	// there is nothing to protect and nothing to do, so that exits 0 as
	// before. The refusal is only for "the store says no artwork is
	// referenced, but the cache holds files it would remove".
	// `--allow-empty` is the operator saying the library really is empty, the
	// same hatch the variant and waveform GCs offer. The refusal below has
	// been un-escapable since it was written, so an operator who genuinely
	// wiped their library could not reclaim the artwork cache at all — found
	// by the sweep test that pins every --gc command against this flag, which
	// is the shape that catches the site an enumeration misses.
	if len(artworkKeys) == 0 && !allowEmpty && present {
		populated, statErr := artworkCacheHasOrphans(walkRoot, known)
		if statErr != nil {
			fmt.Fprintf(stderr, "artwork gc: cannot inspect %s: %v\n", artworkDir, statErr)
			return 1
		}
		if populated {
			fmt.Fprintf(stderr, "artwork gc: refusing — no track row references any artwork, but %s holds cached files.\n", artworkDir)
			fmt.Fprintln(stderr, "  Every cover would be treated as an orphan and removed, and scanner-extracted")
			fmt.Fprintln(stderr, "  covers do not come back on the next scan (the mtime skip gate keeps the")
			fmt.Fprintln(stderr, "  scanner from re-reading unchanged files).")
			fmt.Fprintln(stderr, "  This usually means the wrong config/database: check that --config names the")
			fmt.Fprintln(stderr, "  install whose dataDir you meant, and that a scan has run.")
			fmt.Fprintln(stderr, "  If the library really is empty and you want the cache gone, re-run with --allow-empty.")
			return 1
		}
	}

	var removed, kept, failed, skipped, unlisted int
	var walkErr error
	if present {
		walkErr = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, walkErr error) error {
			// Honor ctx cancellation so SIGINT actually stops the
			// sweep mid-walk instead of churning through the rest of
			// the directory. CodeRabbit Major on PR #217.
			if err := ctx.Err(); err != nil {
				return err
			}
			if walkErr != nil {
				// A directory that went between its listing and its read.
				// A cache with no directory at all never gets here: that
				// is `present == false` (artworkWalkRoot), read as empty
				// as it has always been, the same shape as the upscale GC
				// (`runGC` in upscale.go).
				if errors.Is(walkErr, os.ErrNotExist) {
					return filepath.SkipDir
				}
				// A directory it cannot list is stepped over and named, and
				// the filesystem's lost+found without a word: see
				// artworkUnlistedDir for why this GC may go on.
				if isUnlisted, quiet := artworkUnlistedDir(walkRoot, path, d, walkErr); isUnlisted {
					if !quiet {
						unlisted++
						fmt.Fprintf(stderr, "artwork gc: could not list %s, so the files in it were neither examined nor removed: %v\n",
							artworkReportPath(artworkDir, walkRoot, path), artworkErrWithoutPath(walkErr))
					}
					return nil
				}
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			// Only consider files matching a recognised cache suffix.
			// Anything else (a stray README, a partial download, an
			// old-format thumb, an artist portrait) is treated as
			// out-of-scope and skipped — a future GC pass with broader
			// coverage can extend this.
			base := filepath.Base(path)
			stem, ok := artworkCacheStem(base)
			if !ok {
				skipped++
				return nil
			}
			if known[stem] {
				kept++
				return nil
			}
			// Named under the configured directory, removed where the walk
			// found it (artworkWalkRoot).
			reported := artworkReportPath(artworkDir, walkRoot, path)
			if dryRun {
				fmt.Fprintf(stdout, "would remove: %s\n", reported)
				removed++
				return nil
			}
			if err := os.Remove(path); err != nil {
				fmt.Fprintf(stderr, "remove %s: %v\n", reported, artworkErrWithoutPath(err))
				failed++
				return nil
			}
			removed++
			return nil
		})
	}
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
	// A directory it could not list is counted on the summary, and does not
	// fail the run: nothing the run decided depended on it (see
	// artworkUnlistedDir), and the walk named each one on stderr.
	unlistedNote := ""
	if unlisted > 0 {
		unlistedNote = fmt.Sprintf(", %d director(y/ies) it could not list", unlisted)
	}
	if dryRun {
		fmt.Fprintf(stdout, "GC dry-run: %d orphan(s) would be removed, %d kept, %d skipped (non-cache file)%s.\n",
			removed, kept, skipped, unlistedNote)
	} else {
		fmt.Fprintf(stdout, "GC: removed %d orphan(s), kept %d known cache file(s), %d skipped, %d failure(s)%s.\n",
			removed, kept, skipped, failed, unlistedNote)
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
// Eviction caveat: a still-referenced cover that gets evicted is not
// fetched again. Its tracks are enriched, so /v1/artwork/{mbid} answers the
// terminal 404 `no_image` (classifyArtworkMiss: nothing pending), and the
// enricher only takes rows at `enriched_at = 0`; a scanner-extracted
// `local-` cover does not come back either (the mtime skip gate). This
// comment said a 202 answered it until a later re-enrichment re-cached it,
// which nothing does (2026-09-29, backlog B84). Eviction is oldest-first,
// so an actively-served (recently re-written) cover is the last to go.
func runArtworkCacheSweeper(ctx context.Context, artworkDir string, capBytes int64, interval time.Duration) {
	if capBytes <= 0 {
		return
	}
	s := &artworkCapSweeper{dir: artworkDir, capBytes: capBytes}

	select {
	case <-ctx.Done():
		return
	case <-time.After(artworkCacheSweepSettleDelay):
	}
	s.pass(ctx, time.Now())
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
			s.pass(ctx, time.Now())
		}
	}
}

// artworkCapUnseenRepeat is how often a streak of size-cap passes that
// could not see part of the cache is logged again: once when the streak
// begins, then at most once per this interval while it lasts. A pass runs
// every artworkCacheSweepInterval, and a directory the bridge's user cannot
// list stays that way until someone changes it, so a line per pass is the
// same line 96 times a day, the M-SEARCH shape.
const artworkCapUnseenRepeat = 24 * time.Hour

// artworkCapUnseenExamples bounds how many of the directories a pass could
// not list its log line names.
const artworkCapUnseenExamples = 3

// artworkCapSweeper runs the size cap's passes, and remembers across them
// only what the log needs: when the current streak of passes that could
// not see the whole cache began, and when it was last logged.
type artworkCapSweeper struct {
	dir      string
	capBytes int64
	// unseenSince is zero while the passes see the whole cache.
	unseenSince, lastUnseenLog time.Time
}

// pass runs one pass of the size cap at now (the pass's start, on the
// monotonic clock in production) and logs what it did.
func (s *artworkCapSweeper) pass(ctx context.Context, now time.Time) {
	res, err := sweepArtworkCache(ctx, s.dir, s.capBytes)
	if err != nil {
		// A ctx cancel mid-sweep is a clean shutdown, not a fault. A pass
		// that decided nothing leaves the streak as it was.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		logger.Warn("artwork cache sweep", "err", err, "dir", s.dir)
		return
	}
	s.noteUnseen(now, res)
	if res.Evicted > 0 {
		logger.Info("artwork cache LRU eviction",
			"evicted", res.Evicted, "freedBytes", res.Freed, "capBytes", s.capBytes)
	}
}

// noteUnseen logs, through the streak, what a pass could not see: one WARN
// when a streak of passes that could not see the whole cache begins, then at
// most one per artworkCapUnseenRepeat while it lasts, and one Info line on
// the first pass that sees all of it again. Measured between pass starts,
// so a stepped wall clock neither repeats it early nor holds it back.
func (s *artworkCapSweeper) noteUnseen(now time.Time, res artworkCapResult) {
	if res.sawAll() {
		if !s.unseenSince.IsZero() {
			logger.Info("artwork cache sweep sees the whole cache again",
				"dir", s.dir, "unseenFor", now.Sub(s.unseenSince).Round(time.Second).String())
			s.unseenSince, s.lastUnseenLog = time.Time{}, time.Time{}
		}
		return
	}
	if s.unseenSince.IsZero() {
		s.unseenSince = now
	} else if now.Sub(s.lastUnseenLog) < artworkCapUnseenRepeat {
		return
	}
	s.lastUnseenLog = now
	examples := make([]string, 0, artworkCapUnseenExamples)
	for _, p := range res.Unlisted {
		if len(examples) == artworkCapUnseenExamples {
			break
		}
		if rel, err := filepath.Rel(s.dir, p); err == nil {
			p = rel
		}
		examples = append(examples, p)
	}
	logger.Warn("artwork cache sweep could not see part of the cache",
		"dir", s.dir,
		"unlistedDirs", len(res.Unlisted), "unlisted", examples,
		"unstattedFiles", res.Unstatted,
		"seenBytes", res.Seen, "capBytes", s.capBytes,
		"hint", "the size cap counts and evicts only the files it can see, so the cache can stay over the cap; "+
			"let the bridge's user read and search these directories (a command run under sudo can leave one owned by root)")
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
// unit-testable with a real temp dir. A cache directory that is a link (a
// junction on Windows) is walked where it resolves (artworkWalkRoot).
//
// A directory it cannot list, and a cache file it cannot stat, are stepped
// over: their files are neither counted toward the cap nor evicted, and the
// result names them (Unlisted, Unstatted) for the caller to report. The
// filesystem's own lost+found (the root-owned one at the top of an ext4
// volume mounted as the cache) is stepped over without a word, as the GC
// steps over it: nothing the bridge caches can be in it. Until 2026-09-29
// the sweep returned the first such error, so on a cache that is a
// volume's mount root it evicted nothing on any pass and warned on every
// one (measured on 6dfba62c: 180 bytes of covers against a cap of 100,
// nothing evicted).
//
// Going on over part of the cache, rather than refusing, is sound for a cap
// that evicts oldest-first. The files a pass cannot see are never evicted,
// and of the ones it can see it evicts only files a pass over the whole
// cache would evict too, never one that pass would keep. Both take files
// oldest first. When the partial pass takes a file, what it still sees is
// over the low-water mark; the whole-cache pass, arriving at the same file,
// has taken the same visible files and some unseen older ones, so what it
// still holds is what the partial pass sees plus the unseen files it has
// not taken, which is over the mark as well, and it takes that file too.
// The partial pass evicts fewer bytes (what it sees less the low-water
// mark, against the whole total less it), and it never evicts what it can
// see to make up for what it cannot: it stops once what it sees is under
// the mark, and starts again only once that is over the cap. So all a
// partial pass gets wrong is that the cache can stay over the cap, which
// the caller reports. A refusal would leave the disk to fill on every host
// whose cache holds such a directory, which is the defect.
func sweepArtworkCache(ctx context.Context, artworkDir string, capBytes int64) (res artworkCapResult, err error) {
	if capBytes <= 0 {
		return res, nil
	}
	walkRoot, present, err := artworkWalkRoot(artworkDir)
	if err != nil || !present {
		return res, err
	}
	type cacheFile struct {
		path string
		size int64
		mod  time.Time
	}
	var files []cacheFile
	var total int64
	walkErr := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, walkErr error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if walkErr != nil {
			// A directory that went between its listing and its read.
			if errors.Is(walkErr, os.ErrNotExist) {
				return filepath.SkipDir
			}
			// A directory it cannot list: stepped over and named, and the
			// filesystem's lost+found without a word (see above).
			if unlisted, quiet := artworkUnlistedDir(walkRoot, path, d, walkErr); unlisted {
				if !quiet {
					res.Unlisted = append(res.Unlisted, artworkReportPath(artworkDir, walkRoot, path))
				}
				return nil
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
			// that one entry. Any OTHER stat error (permission, I/O) hides
			// the file's bytes the way a directory it cannot list hides its
			// files: counted, stepped over, and reported by the caller
			// rather than silently undercounting. It ended the pass until
			// 2026-09-29, which kept the cap from being enforced at all
			// while the file stayed that way (a directory its user may list
			// and not search, say).
			if errors.Is(ierr, os.ErrNotExist) {
				return nil
			}
			res.Unstatted++
			return nil
		}
		files = append(files, cacheFile{path: path, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, os.ErrNotExist) {
			return artworkCapResult{}, nil
		}
		return res, walkErr
	}
	res.Seen = total
	if total <= capBytes {
		return res, nil
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
			return res, cerr
		}
		// Removed where the walk found it; named under the configured
		// directory (artworkWalkRoot).
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
			logger.Warn("artwork cache evict", "path", artworkReportPath(artworkDir, walkRoot, f.path),
				"err", artworkErrWithoutPath(rmErr))
			continue
		}
		total -= f.size
		res.Freed += f.size
		res.Evicted++
	}
	return res, nil
}

// artworkCapResult is what one pass of the size cap did, and what of the
// cache it could not see.
type artworkCapResult struct {
	Evicted int
	Freed   int64
	// Seen is the bytes of cache files the pass counted, before it evicted
	// any.
	Seen int64
	// Unlisted names each directory the pass could not list, under the
	// configured cache directory; the filesystem's own lost+found is not
	// one. Unstatted counts the cache files it listed and could not stat.
	// The files behind either were neither counted toward the cap nor
	// evicted.
	Unlisted  []string
	Unstatted int
}

// sawAll reports whether the pass saw the whole cache.
func (r artworkCapResult) sawAll() bool {
	return len(r.Unlisted) == 0 && r.Unstatted == 0
}
