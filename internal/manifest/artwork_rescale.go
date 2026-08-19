// One-shot in-place rescale of the pre-existing local-artwork cache.
//
// Before artwork right-sizing (see artwork_scale.go), `stampLocalArtwork`
// wrote raw embedded/folder-art bytes verbatim — the production VPS cache
// measured 4,408 files / 1.3 GB with 239 files over 1 MB and a 19 MB
// maximum. New scans write ≤1200 px bytes; this pass heals what is
// already on disk, once, in the background.
//
// NOT a schema migration on purpose: image decode work must never block
// OpenStore (migrations run synchronously inside it, before the API can
// serve). It is instead a scan_state-marker-gated background pass wired
// beside the scanner boot in cmd/bridge/main.go (bgWriters-joined, runs
// on scanCtx).
//
// Resume semantics: the marker is written ONLY after a completed walk, so
// an interrupt (shutdown mid-pass) re-runs on next boot and the
// stat-cheap skip conditions make the already-processed prefix free. A
// per-file persistent failure (decode error, Windows rename-over-open-fd
// exhaustion) is logged + skipped and does NOT hold the marker hostage —
// the walk completing WITH skips still sets it (the header probe is
// cheap, and stragglers heal on a future marker-key bump). Files are
// never deleted on failure.
//
// Re-run policy is encoded in the KEY NAME: if the thresholds below ever
// change materially, bump the key to `artwork_rescale_v2` so deployed
// bridges run the new pass once; the old marker rows stay inert.
package manifest

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// artworkRescaleMarkerKey is the scan_state run-once gate.
const artworkRescaleMarkerKey = "artwork_rescale_v1"

// rescaleMaxDimensionPx is the longest-side HYSTERESIS threshold: files
// at or under it are left alone even though new writes target 1200 px.
// The band between 1200 and 1440 deliberately survives — re-encoding a
// 1250 px cover down to 1200 buys ~nothing and costs a generation of
// JPEG loss on every borderline file.
//
// LOCKSTEP: the iOS `ArtworkCache` verbatim-store bound (`verbatimMaxSide`
// in ArtworkCache.swift) is 1440 BECAUSE of this hysteresis — legacy
// 1200–1440 px covers arrive as-is and must stay on the client's
// no-recompress fast path. Change one, revisit the other.
const rescaleMaxDimensionPx = 1440

// rescaleMaxBytes triggers a re-encode even when the dimensions pass:
// a dimensionally-fine cover stored at archival quality (q95+) can
// still weigh multiple MB; q82 at ≤1200 px lands at ~120–250 KB.
const rescaleMaxBytes = 700 * 1024

// localArtworkFilePrefix / localArtworkFileSuffix bound the walk to the
// scanner-written population (`local-<sha256>-500.jpg`). Enricher covers
// (`<uuid>-<size>.jpg`) were always fetched pre-sized from CAA/Atlas and
// are deliberately out of scope.
const (
	localArtworkFilePrefix = "local-"
	localArtworkFileSuffix = "-500.jpg"
)

// RunArtworkRescaleOnce walks <artworkDir> and rewrites every
// scanner-written local cover whose longest side exceeds
// rescaleMaxDimensionPx or whose size exceeds rescaleMaxBytes down to
// ≤1200 px JPEG at q82, atomically (`.rescale-*.jpg.tmp` + rename), in
// place under its original name — key stability: the `local-<hash>`
// sentinel hashes the ORIGINAL bytes and lives in every track row and
// every paired client's cache keys, so the filename must not change.
//
// Ctx-cancel is honored between files; see the module docblock for the
// marker/resume/skip contract.
func RunArtworkRescaleOnce(ctx context.Context, store *Store, artworkDir string) {
	log := scanLogger.With("pass", "artwork-rescale")
	marker, err := store.GetScanState(ctx, artworkRescaleMarkerKey)
	if err != nil {
		// A cancelled ctx during shutdown surfaces here as a DB error;
		// don't turn a normal teardown into a misleading warning.
		if ctx.Err() == nil {
			log.Warn("read rescale marker; skipping pass", "err", err)
		}
		return
	}
	if marker != "" {
		return // already ran to completion
	}
	entries, err := os.ReadDir(artworkDir)
	if err != nil {
		if os.IsNotExist(err) {
			// No cache yet (fresh install / pre-first-scan). Nothing to
			// heal — new writes are right-sized by construction. Mark done
			// so subsequent boots skip the ReadDir.
			markArtworkRescaleDone(ctx, store, log)
			return
		}
		log.Warn("read artwork dir; skipping pass", "err", err)
		return
	}
	start := time.Now()
	var examined, rewritten, skippedFine, failed int
	var reclaimedBytes int64
	for _, entry := range entries {
		if ctx.Err() != nil {
			// Interrupted — NO marker, so the next boot resumes. The
			// already-rewritten prefix re-checks stat-cheap and skips.
			log.Info("artwork rescale interrupted; will resume next boot",
				"examined", examined, "rewritten", rewritten)
			return
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, localArtworkFilePrefix) || !strings.HasSuffix(name, localArtworkFileSuffix) {
			continue
		}
		examined++
		full := filepath.Join(artworkDir, name)
		outcome, saved := rescaleOneArtworkFile(full, log)
		switch outcome {
		case rescaleRewritten:
			rewritten++
			reclaimedBytes += saved
		case rescaleSkippedFine:
			skippedFine++
		case rescaleFailed:
			failed++
		}
	}
	markArtworkRescaleDone(ctx, store, log)
	log.Info("artwork rescale pass complete",
		"examined", examined, "rewritten", rewritten, "alreadyFine", skippedFine,
		"failed", failed, "reclaimedMB", reclaimedBytes/(1024*1024),
		"elapsed", time.Since(start).Round(time.Second).String())
}

type rescaleOutcome int

const (
	rescaleSkippedFine rescaleOutcome = iota
	rescaleRewritten
	rescaleFailed
)

// rescaleOneArtworkFile applies the rewrite decision + atomic write for a
// single cache file. Returns the outcome plus the byte delta reclaimed on
// a rewrite. Never deletes: every failure path leaves the original file
// serving exactly as before.
func rescaleOneArtworkFile(path string, log interface {
	Warn(msg string, args ...any)
}) (rescaleOutcome, int64) {
	info, err := os.Stat(path)
	if err != nil {
		log.Warn("rescale stat; skipping file", "path", path, "err", err)
		return rescaleFailed, 0
	}
	if info.Size() > maxArtworkBytes {
		// Larger than anything stampLocalArtwork ever wrote (25 MiB cap)
		// — foreign file; leave it alone.
		log.Warn("rescale: file exceeds artwork cap; skipping", "path", path, "bytes", info.Size())
		return rescaleFailed, 0
	}
	needsRewrite := info.Size() > rescaleMaxBytes
	dims, dimsKnown := jpegHeaderDimensions(path)
	if dimsKnown && (dims.X > rescaleMaxDimensionPx || dims.Y > rescaleMaxDimensionPx) {
		needsRewrite = true
	}
	if !needsRewrite {
		return rescaleSkippedFine, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Warn("rescale read; skipping file", "path", path, "err", err)
		return rescaleFailed, 0
	}
	// forceReencode: the size trigger alone means "dimensions fine, bytes
	// heavy" — scaleLocalArtwork's verbatim fast path would return the
	// input unchanged, so the rescale pass asks for an unconditional
	// decode + q82 re-encode (still capped + passthrough-guarded).
	scaled, err := scaleLocalArtworkImpl(data, true)
	if err != nil {
		log.Warn("rescale decode; skipping file", "path", path, "err", err)
		return rescaleFailed, 0
	}
	if int64(len(scaled)) >= info.Size() {
		// Re-encode didn't help (or passthrough returned the original) —
		// keep the bytes that are already there; rewriting would churn
		// mtime/ETag for zero gain.
		return rescaleSkippedFine, 0
	}
	if err := atomicwrite.WriteBytes(path, scaled, ".rescale-*.jpg.tmp"); err != nil {
		// Windows AV-window / rename-over-open-fd exhaustion lands here
		// after atomicwrite's own retry ladder. Log + skip; the original
		// keeps serving and a future marker-key bump can retry.
		log.Warn("rescale write; skipping file", "path", path, "err", err)
		return rescaleFailed, 0
	}
	return rescaleRewritten, info.Size() - int64(len(scaled))
}

// jpegHeaderDimensions reads only the image header (a few KB) and
// returns the pixel dimensions. ok=false when the header can't be
// parsed — the caller then decides on the size trigger alone.
func jpegHeaderDimensions(path string) (image.Point, bool) {
	f, err := os.Open(path)
	if err != nil {
		return image.Point{}, false
	}
	defer func() { _ = f.Close() }()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return image.Point{}, false
	}
	return image.Point{X: cfg.Width, Y: cfg.Height}, true
}

func markArtworkRescaleDone(ctx context.Context, store *Store, log interface {
	Warn(msg string, args ...any)
}) {
	stamp := fmt.Sprintf("done@%s", time.Now().UTC().Format(time.RFC3339))
	if err := store.SetScanState(ctx, artworkRescaleMarkerKey, stamp); err != nil {
		// Shutdown-cancelled ctx → expected failure, not a warning.
		if ctx.Err() == nil {
			log.Warn("write rescale marker (pass will re-run next boot)", "err", err)
		}
	}
}
