// Derived serve-tier thumbnails for the admin web player.
//
// The cover cache stores ONE tier per image. `stampLocalArtwork` writes
// local covers at `localArtMaxDimensionPx` (1200 px) under a `-500`
// suffix that has been a misnomer since the scaling module landed, and
// `ArtistImagePath` writes portraits at whatever Deezer returned. The
// serve ladder then stats the requested size, misses, and falls back —
// so `?size=250` answered with a 1200 px / ~190 KB JPEG.
//
// That is right for iOS, which fetches a cover once and reuses it at
// every size. It is wrong for a browser album grid, where each of ~12
// visible tiles is a separate request for a ~190 px box: measured on the
// reference library, one grid paint pulled ~2.3 MB to paint ~0.5 MP of
// actual pixels.
//
// EnsureThumb derives the missing smaller tier once, on demand, and the
// normal `os.Open` + `http.ServeContent` path serves it forever after.
//
// WHY A SUBDIRECTORY. Derived tiers land in `<artworkDir>/thumbs/`, not
// beside their sources, and that separation is load-bearing three times
// over:
//
//   - `/v1/artwork` shares the artwork dir and stats the requested size
//     FIRST, so writing `local-<sha>-250.jpg` next to the original would
//     silently change the bytes iOS receives. artwork_scale.go's header
//     records a deliberate 2026-08-19 decision that clients get one
//     1200 px tier; this module must not reverse it as a side effect.
//   - `enrich.CachedArtistImageMBIDs` enumerates that directory to build
//     the artist-image coverage set, keying on the `artist-<uuid>.jpg`
//     shape. Size-suffixed portraits alongside it would need that parser
//     taught about them.
//   - `RunArtworkRescaleOnce` walks with a flat `os.ReadDir` and skips
//     directory entries, so a subdirectory is invisible to it.
//
// Extending derived tiers to `/v1` would be a real win for iOS list
// thumbnails, but it reverses the decision above and wants its own
// measurement plus an iOS-side look. Deliberately not done here.
package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// ThumbsDirName is the subdirectory under the artwork cache that holds
// derived serve tiers. Exported so the wiring in cmd/bridge can build
// thumb paths without duplicating the literal.
const ThumbsDirName = "thumbs"

// ArtworkLongestSide returns the longest side of the JPEG at path,
// reading only the header (a few KB), and whether it could be read.
//
// It exists because a cache filename is NOT evidence of a file's size.
// `stampLocalArtwork` writes every local cover under a `-500` suffix
// whatever its real dimensions are — the suffix predates the scaling
// module and has been a misnomer since. So a serve path that trusts an
// exact filename match answers `?size=500` with whatever that file
// happens to hold, which on the reference library is routinely 1200 px.
func ArtworkLongestSide(path string) (int, bool) {
	dims, ok := jpegHeaderDimensions(path)
	if !ok {
		return 0, false
	}
	if dims.X > dims.Y {
		return dims.X, true
	}
	return dims.Y, true
}

// errThumbNotNeeded means the source is already at or below the target
// size, so there is nothing smaller to derive. Callers fall through to
// the existing serve ladder, which will find the source tier.
var errThumbNotNeeded = errors.New("artwork already within the requested tier")

// ErrThumbNotNeeded reports whether err means "no derivation was
// warranted" rather than a failure. A caller that sees it should serve
// the source tier exactly as it did before this module existed.
func ErrThumbNotNeeded(err error) bool { return errors.Is(err, errThumbNotNeeded) }

// EnsureThumb makes dst a JPEG of src whose longest side is at most
// targetPx, and reports whether dst is usable.
//
// It is idempotent and cheap on the hot path: an existing, fresh dst
// costs two stats and no decode. A derivation costs one decode+encode
// under `artDecodeSem` (cap 1 package-wide — the same bound that keeps
// peak decode memory to a single RGBA matrix on a Pi-class host) and one
// atomic rename.
//
// FRESHNESS. An existing dst is reused only when it is at least as new
// as src. That check is redundant for album covers, whose filenames are
// content keys (`local-<sha256>`) — a changed cover is a changed name,
// so its thumb can never be stale. It is REQUIRED for artist portraits:
// `ArtistImagePath` is `artist-<mbid>.jpg`, a fixed key the enricher
// overwrites in place, so without it a refreshed portrait would serve
// its old thumbnail forever. `RunArtworkRescaleOnce` rewrites sources
// atomically and therefore bumps their mtime, so the first request after
// it re-derives each thumb once — correct, and self-limiting.
func EnsureThumb(src, dst string, targetPx int) error {
	if targetPx <= 0 {
		return fmt.Errorf("artwork thumb: non-positive target %d", targetPx)
	}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}
	if dstInfo, err := os.Stat(dst); err == nil && !srcInfo.ModTime().After(dstInfo.ModTime()) {
		return nil // fresh enough — the common path
	}

	// Header-only read: a source already within the tier has nothing
	// smaller to give, and saying so costs a few KB rather than a full
	// decode.
	if dims, ok := jpegHeaderDimensions(src); ok && dims.X <= targetPx && dims.Y <= targetPx {
		return errThumbNotNeeded
	}
	if srcInfo.Size() > maxArtworkBytes {
		// Larger than anything this cache's writers ever produce; treat
		// it as foreign and leave it to the serve ladder untouched — the
		// same posture rescaleOneArtworkFile takes.
		return errThumbNotNeeded
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	scaled, err := scaleLocalArtworkImpl(data, true, targetPx)
	if err != nil {
		return err
	}
	if len(scaled) >= len(data) {
		// scaleLocalArtworkImpl returns the input VERBATIM on its
		// passthrough paths (a JPEG whose pixels Go can't decode, or one
		// over the source-dimension caps). Writing those bytes under a
		// smaller tier's name would make the filename lie the same way
		// `-500` already does, so decline instead and let the ladder
		// serve the real source.
		return errThumbNotNeeded
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := atomicwrite.WriteBytes(dst, scaled, ".thumb-*.jpg.tmp"); err != nil {
		return err
	}
	// A derived tier must never look older than its source, or the
	// freshness check above would re-derive it on every request. Rename
	// carries the temp file's own mtime, which is `now` and therefore
	// already newer; the guard is here for a source stamped in the
	// future by clock skew or an rsync that preserved times.
	if srcInfo.ModTime().After(time.Now()) {
		_ = os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime().Add(time.Second))
	}
	return nil
}
