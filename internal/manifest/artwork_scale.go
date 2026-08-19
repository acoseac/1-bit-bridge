// Scan-time right-sizing for locally-extracted album artwork.
//
// Root cause this module exists for (2026-08-19, bridge.ars.md journal):
// `stampLocalArtwork` used to write RAW embedded/folder-art bytes VERBATIM
// into `local-<sha256>-500.jpg` — up to the 25 MiB `maxArtworkBytes` cap,
// with the `-500` suffix a lie for scanner-extracted covers. A first sync
// of the ~20k-track production library shipped 1.3 GB of full-resolution
// covers (max 19 MB single file) that every client then decoded and
// downscaled per-device. The governing principle (user-confirmed): the
// bridge does the heavy lifting — image work happens ONCE, server-side, and
// clients receive right-sized bytes.
//
// `scaleLocalArtwork` normalizes candidate bytes to a JPEG whose longest
// side is at most `localArtMaxDimensionPx`:
//
//   - JPEG already within bounds → returned VERBATIM (zero decode cost —
//     the overwhelmingly common case for pre-scaled rips, and it preserves
//     the original bytes' EXIF/ICC exactly).
//   - JPEG over bounds → decoded, CatmullRom-downscaled, re-encoded at
//     `localArtJPEGQuality`.
//   - PNG → decoded + transcoded to JPEG (scaled if over bounds). PNG
//     covers were SKIPPED entirely before this module (JPEG-only V1
//     scheme); transcoding — never a verbatim PNG write — is what keeps
//     the `*-500.jpg` path + `Content-Type: image/jpeg` contract honest.
//   - Anything else (GIF, junk, truncated headers) → error; callers skip
//   - warn exactly as they always did.
//
// Source-safety caps (`localArtMaxSourcePixels` / `localArtMaxSourceAxisPx`)
// reject forged tiny-file/huge-dimension payloads BEFORE the pixel decode
// (the `processCoverImage` precedent in internal/admin/handlers_covers.go).
// An over-cap JPEG falls back to VERBATIM passthrough — before this module
// those bytes were stored as-is, so refusing them now would regress a
// previously-served (if oversized) cover to a grey tile. An over-cap PNG is
// refused outright: it was never served before (the JPEG-only gate), and a
// verbatim PNG write would put PNG bytes behind an image/jpeg label.
//
// The package-level decode semaphore (`artDecodeSem`, cap 1) bounds peak
// decode memory to ONE RGBA matrix (~192 MB at the 48 MP cap) regardless
// of how many scanner workers or the rescale one-shot run concurrently.
// Decodes are once-per-unique-cover (the folder-art single-flight + the
// SHA-256 stat-before-write dedupe), so the serialization costs nothing
// in steady state. HOLD IT ACROSS DECODE+ENCODE ONLY, never across file
// I/O — scan-time extraction and `RunArtworkRescaleOnce` must interleave.
// Serving (`/v1/artwork` = os.Open + http.ServeContent) never decodes and
// never touches this semaphore.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // register the PNG decoder for image.Decode / DecodeConfig

	"golang.org/x/image/draw"
)

// localArtMaxDimensionPx is the longest-side ceiling for scan-written local
// covers. 1200 px (user-decided 2026-08-19): iPad Pro hero / Now Playing
// surfaces want ~1200–1400 px at @2x, and 1200 matches the Cover Art
// Archive "large" tier the enricher fetches, so local and enriched covers
// land in the same quality class. Never upscaled — a smaller original stays
// at its native size.
//
// LOCKSTEP: the iOS `ArtworkCache` verbatim-store fast path accepts
// already-JPEG bridge bytes up to `verbatimMaxSide` = 1440 px, NOT 1200 —
// because `RunArtworkRescaleOnce`'s hysteresis (rescaleMaxDimensionPx)
// deliberately skips legacy files ≤ 1440 px, so those arrive as-is. If this
// constant or the rescale hysteresis changes, revisit the iOS bound in the
// same change (ArtworkCache.swift, `verbatimMaxSide`).
const localArtMaxDimensionPx = 1200

// localArtJPEGQuality is the re-encode quality for scaled covers. q82 lands
// a 1200 px cover at ~120–250 KB — visually transparent for album art while
// cutting the measured first-sync artwork transfer by ~5×.
const localArtJPEGQuality = 82

// localArtMaxSourceAxisPx rejects absurd per-axis source dimensions at the
// header stage (image.DecodeConfig reads only the signature + dimensions),
// before any pixel matrix is allocated.
const localArtMaxSourceAxisPx = 16000

// localArtMaxSourcePixels caps the TOTAL decoded pixel count (~48 MP ≈
// 192 MB RGBA). The per-axis cap alone still admits a 16000×16000 source
// (~1 GB RGBA); a highly-compressible PNG encodes those dimensions in a
// tiny body — an OOM vector on a RAM-constrained host. 48 MP is far above
// any real cover scan (a 6000×8000 booklet page fits).
const localArtMaxSourcePixels = 48 * 1000 * 1000

// artDecodeSem serializes cover decodes package-wide (see the module
// docblock: cap 1 bounds peak decode memory to one RGBA matrix; hold per
// decode+encode only, never across file I/O).
var artDecodeSem = make(chan struct{}, 1)

// pngMagic is the 4-byte PNG signature prefix (\x89PNG). GIF and
// everything else stay rejected — see folderArtCandidates for the
// candidate-set contract.
var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47}

// looksLikePNG reports whether data starts with the PNG signature.
func looksLikePNG(data []byte) bool {
	return len(data) >= len(pngMagic) && bytes.HasPrefix(data, pngMagic)
}

// looksLikeLocalArtCandidate reports whether data carries one of the two
// magic prefixes `scaleLocalArtwork` accepts (JPEG SOI or PNG signature).
// The scan-time gates use it in place of the old JPEG-only sniff.
func looksLikeLocalArtCandidate(data []byte) bool {
	return looksLikeJPEG(data) || looksLikePNG(data)
}

// errArtworkNotDecodable marks bytes that are neither a decodable JPEG nor
// PNG (or whose header dimensions are degenerate). Callers skip + warn.
var errArtworkNotDecodable = errors.New("artwork bytes not a decodable JPEG/PNG")

// scaleLocalArtwork normalizes candidate cover bytes per the module
// docblock and returns JPEG bytes whose longest side is at most
// localArtMaxDimensionPx — or, for a JPEG source that exceeds the decode
// safety caps, the original bytes verbatim (passthrough, no regression
// vs the pre-scaling behaviour). The returned slice aliases `data` on
// the verbatim paths; callers must not mutate it.
func scaleLocalArtwork(data []byte) ([]byte, error) {
	return scaleLocalArtworkImpl(data, false)
}

// scaleLocalArtworkImpl is the shared body. forceReencode skips the
// "already right-sized JPEG → verbatim" fast path — the rescale
// one-shot uses it for its byte-size trigger, where the dimensions are
// fine but the encode is archival-heavy and the point IS the q82
// re-encode. Every safety path (decode caps, unparseable-JPEG
// passthrough) is identical in both modes.
func scaleLocalArtworkImpl(data []byte, forceReencode bool) ([]byte, error) {
	isJPEG := looksLikeJPEG(data)
	if !isJPEG && !looksLikePNG(data) {
		return nil, errArtworkNotDecodable
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if isJPEG {
			// A JPEG whose header Go's decoder can't parse (exotic
			// progressive/CMYK variants) was served verbatim before this
			// module existed; keep doing that rather than dropping the
			// album's only cover.
			return data, nil
		}
		return nil, errArtworkNotDecodable
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, errArtworkNotDecodable
	}
	overCaps := cfg.Width > localArtMaxSourceAxisPx || cfg.Height > localArtMaxSourceAxisPx ||
		int64(cfg.Width)*int64(cfg.Height) > localArtMaxSourcePixels
	if overCaps {
		if isJPEG {
			// Passthrough: decoding is unsafe at these dimensions, and
			// these bytes were stored verbatim before scaling existed.
			return data, nil
		}
		return nil, fmt.Errorf("png artwork dimensions out of range (%dx%d)", cfg.Width, cfg.Height)
	}
	if !forceReencode && format == "jpeg" &&
		cfg.Width <= localArtMaxDimensionPx && cfg.Height <= localArtMaxDimensionPx {
		// Already right-sized JPEG — the common case; zero decode cost.
		return data, nil
	}

	// Decode + scale + encode under the package semaphore (cap 1 —
	// bounds peak to one RGBA matrix; see artDecodeSem).
	artDecodeSem <- struct{}{}
	defer func() { <-artDecodeSem }()

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		if isJPEG {
			return data, nil // header parsed but pixels didn't — verbatim, as before
		}
		return nil, errArtworkNotDecodable
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errArtworkNotDecodable
	}
	nw, nh := w, h
	if w > localArtMaxDimensionPx || h > localArtMaxDimensionPx {
		if w >= h {
			nw = localArtMaxDimensionPx
			nh = int(float64(h) * float64(localArtMaxDimensionPx) / float64(w))
		} else {
			nw = int(float64(w) * float64(localArtMaxDimensionPx) / float64(h))
			nh = localArtMaxDimensionPx
		}
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: localArtJPEGQuality}); err != nil {
		return nil, fmt.Errorf("re-encode artwork: %w", err)
	}
	return buf.Bytes(), nil
}
