package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	tag "github.com/dhowden/tag"
	"github.com/mewkiz/flac/meta"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// maxArtworkBytes caps the per-image bytes the local-artwork extractor
// will hash + cache. Modern audiophile rips routinely embed 10–20 MiB
// digital-booklet scans (the previous 10 MiB cap silently rejected
// near-boundary cases by ~30 KiB and lost the entire album's
// `ArtworkMBID` to the enricher's MusicBrainz fallback). 25 MiB
// accommodates ~99% of those cases while still rejecting genuine
// misuse like lossless TIFFs in tags or a misnamed 4K wallpaper saved
// as `cover.jpg`. RAM headroom under the parallel-worker model
// (`runtime.NumCPU()` workers each hold at most one buffer this size):
// 8–16 cores × 25 MiB ≈ 200–400 MiB peak — comfortable on any machine
// running the bridge (PC/Mac, not iOS). Overrun is logged + skipped;
// the track still gets indexed without an ArtworkMBID.
const maxArtworkBytes = 25 * 1024 * 1024 // 25 MiB

// canSetBitsPerSample reports whether `codec` is one of the canonical
// lossless codecs the bridge tracks AND for which `t.BitsPerSample`
// carries a meaningful integer bit-depth on the wire.
//
// **Allowlist by design** (CodeRabbit Major on PR #225): a lossy
// denylist (`isLossyCodec`) would fail open on `t.Codec == ""` —
// reachable today via the MP4 codec-walk error branch
// (`extractMP4Codec` returns "" on a truncated atom tree). Inverting
// to an allowlist closes that hole: any future enricher addition
// that writes `t.BitsPerSample` with an unset / unrecognised codec
// is correctly refused, since the iOS PR #371 "M4A 32-bit Now
// Playing chip" regression's root cause was exactly the
// container-width misclassification a fail-open gate would re-admit.
//
// Defense-in-depth contract: every site in this file that writes
// `t.BitsPerSample` MUST first check `canSetBitsPerSample(t.Codec)`.
// Today only FLAC + DSF + DFF actually assign bitsPerSample (ALAC
// flows through the MP4 dhowden path which doesn't surface
// BitsPerSample today); the allowlist is structural insurance
// against a future addition with an unknown codec slipping through.
//
// Case-folded via strings.EqualFold so the gate works regardless
// of how a caller-supplied codec string is cased.
func canSetBitsPerSample(codec string) bool {
	switch {
	case strings.EqualFold(codec, "FLAC"),
		strings.EqualFold(codec, "ALAC"),
		strings.EqualFold(codec, "DSF"),
		strings.EqualFold(codec, "DFF"),
		strings.EqualFold(codec, "WAV"),
		strings.EqualFold(codec, "AIFF"):
		return true
	}
	return false
}

// isValidDSDSampleRate reports whether `sr` (Hz) is a recognised
// DSD sample rate — any positive multiple of either 2,822,400 Hz
// (64 × 44.1 kHz, the DSD64 base) or 3,072,000 Hz (64 × 48 kHz,
// the alternate DSD64 base). Covers DSD64 / 128 / 256 / 512 / 1024
// and the 48k-derived variants.
//
// Used by `extractDSFWithContext` as a sanity floor on the
// default-true `IsDSD` flip: a `.dsf` file structurally IS DSD, so
// the default-true stance is correct for well-formed files; but a
// fmt chunk that reports a PCM-like rate (44100, 48000, …) is
// almost certainly a mislabeled PCM-in-DSF container and should
// classify as non-DSD to avoid the iOS decoder attempting a DoP
// lock that will fail.
func isValidDSDSampleRate(sr uint32) bool {
	if sr == 0 {
		return false
	}
	return sr%2_822_400 == 0 || sr%3_072_000 == 0
}

// ExtractContext carries the side channels Extract needs to perform
// local-artwork extraction (write cached JPEGs, dedupe per-directory
// folder.jpg lookups). nil → tag-only extraction (the original Extract
// behaviour); non-nil with empty ArtworkCacheDir → same effect.
//
// Why a struct instead of two positional params: callers (the scanner's
// runScanWorker) already build one per-worker and reuse it across every
// track — passing two args at every call site would be ergonomic noise
// and any future addition (e.g. a metrics counter) becomes a viral
// signature change.
type ExtractContext struct {
	ArtworkCacheDir string    // <dataDir>/artwork; "" disables local-art
	FolderArtCache  *sync.Map // dir-path string -> *folderArtPromise
}

// Ext enumerates the file extensions the scanner considers. Case-
// insensitive; leading "." included for filepath.Ext matching.
//
// KEEP IN SYNC with extractByFormat's switch: the scanner's WalkDir
// gates discovery on this map (scanner.go — `if !Ext[ext]`), so an
// extension the dispatcher can parse but that's missing here is
// silently skipped at scan time (the dispatcher case becomes dead
// code). TestExtCoversDispatcher pins the intersection.
var Ext = map[string]bool{
	".flac": true,
	".dsf":  true,
	".dff":  true, // DSDIFF — rarer but found in audiophile libraries
	".mp3":  true,
	".m4a":  true, // AAC / ALAC
	".m4b":  true, // MPEG-4 audiobook — same MP4 container/atoms as .m4a
	".m4p":  true, // legacy iTunes DRM AAC; tags parse even if playback can't decrypt
	".mp4":  true, // lossy audio inside MP4 container, uncommon but valid
	".ogg":  true,
	".oga":  true, // Ogg audio (Vorbis/Opus/FLAC-in-Ogg); same dhowden path as .ogg
	".wav":  true,
	".aif":  true,
	".aiff": true,
	".aifc": true, // AIFC = compressed-AIFF FORM type; same chunk-walker shape, accepted by extractAIFFWithContext
}

// Extract reads as much metadata as it can from the file at absPath and
// fills in the Track at t. Path, Size, ModTime on t MUST already be set by
// the scanner; Extract only fills tag/format fields.
//
// Missing or unparseable tags are NOT an error — a file with no metadata
// still gets indexed (we fall back to path-derived heuristics later). Only
// read/open errors propagate.
//
// Equivalent to ExtractWithContext(absPath, t, nil) — preserved for
// callers (existing tests, anyone with a one-shot tag read) that don't
// run the local-artwork extraction pipeline.
func Extract(absPath string, t *Track) error {
	return ExtractWithContext(absPath, t, nil)
}

// ExtractWithContext extracts tags for absPath into t (format-dispatched via
// extractByFormat), then backfills a MISSING track number from the filename's
// leading "NN" (e.g. ".../06. Congeniality.flac" → 6). Many files carry the
// number in the name but no track-number tag, which leaves them unordered on
// iOS (album order is disc+track). The backfill fires ONLY when the tag is
// absent — it never overrides a real tag — and parseLeadingTrackNumber's
// bounded, punctuation-anchored pattern keeps a year/title prefix from being
// misread. Bit-exact: a manifest-level fill, not a file edit.
func ExtractWithContext(absPath string, t *Track, ec *ExtractContext) error {
	if err := extractByFormat(absPath, t, ec); err != nil {
		return err
	}
	fillTrackNumberFromFilename(absPath, t)
	return nil
}

// parseLeadingTrackNumber extracts a leading 1–3 digit track number from a
// filename stem (extension already stripped). The digits must be followed by
// a punctuation separator — '.', '-', '_', or ')' — optionally spaced
// ("06. Title", "06 - Title", "06_Title"). Requiring punctuation (not a bare
// space) keeps a numeric title ("12 Monkeys") from being misread; the
// >3-digit guard rejects a bare year prefix ("1984 - …"). Returns (n, true)
// for 1..999, else (0, false).
func parseLeadingTrackNumber(stem string) (int, bool) {
	i := 0
	for i < len(stem) && i < 3 && stem[i] >= '0' && stem[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, false // no leading digit
	}
	if i < len(stem) && stem[i] >= '0' && stem[i] <= '9' {
		return 0, false // >3-digit run (e.g. a "1984" year) — not a track
	}
	j := i
	for j < len(stem) && stem[j] == ' ' {
		j++
	}
	if j >= len(stem) {
		return 0, false // digits with no following title — too ambiguous
	}
	switch stem[j] {
	case '.', '-', '_', ')':
		// punctuation separator — looks like a track prefix
	default:
		return 0, false
	}
	n, err := strconv.Atoi(stem[:i])
	if err != nil || n < 1 || n > 999 {
		return 0, false
	}
	return n, true
}

// fillTrackNumberFromFilename backfills t.TrackNumber from the filename's
// leading "NN" when the tag-derived value is ABSENT (nil, or the 0 sentinel
// the dhowden fallback writes for "no tag"). A real (>0) tag value is never
// overridden.
func fillTrackNumberFromFilename(absPath string, t *Track) {
	if t.TrackNumber != nil && *t.TrackNumber > 0 {
		return
	}
	stem := filepath.Base(absPath)
	stem = strings.TrimSuffix(stem, filepath.Ext(stem))
	if n, ok := parseLeadingTrackNumber(stem); ok {
		t.TrackNumber = &n
	}
}

// extractMP4WithContext handles the MP4 container family (.m4a/.mp4/.m4b/.m4p):
// distinguish ALAC from AAC, capture ALAC source bit depth, then hand the
// rewound reader to dhowden for tags. Split out of extractByFormat so that
// dispatcher's cognitive complexity stays in budget (SonarCloud go:S3776) —
// the logic is unchanged from the inlined case.
func extractMP4WithContext(absPath string, t *Track, ec *ExtractContext) error {
	// MP4 container — distinguish ALAC from AAC via the sample-
	// description box (`tag.FileType()` doesn't actually do this
	// for MP4 in dhowden/tag, despite the documented promise; their
	// source carries a `FIXME: actually detect this` for the
	// ALAC FileType constant). Open the file once for the codec
	// walk + tag read; rewind in between. Per Gemini A1 / iOS
	// bug review #1.
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	// Log walker errors at Warn so a corrupted container, truncated
	// atom tree, or NFS glitch mid-seek surfaces in the operator's
	// scanner log instead of being silently swallowed. Per
	// CodeRabbit Trivial round-1 on PR #168 — degraded-but-functional
	// outcomes use Warn per the project logging convention. Codec
	// stays unset on failure so downstream classification falls
	// through to the extension-derived name.
	if codec, err := extractMP4Codec(f); err != nil {
		scanLogger.Warn("mp4 codec walk failed; falling back to extension classification",
			"path", absPath, "err", err)
	} else if codec != "" {
		t.Codec = codec
	}
	// When the codec walk identified ALAC, descend one level into
	// the inner `alac` config atom to capture the source bit depth
	// from ALACSpecificConfig (the `dhowden/tag` MP4 path does NOT
	// surface it). `canSetBitsPerSample` keeps the gate aligned
	// with the wider lossless set; for MP4 today that's effectively
	// ALAC only — `mp4a` paths fall through. Logged at Warn on
	// walker failure with manifest carrying nil bits (consistent
	// with the codec-walk Warn path above).
	if canSetBitsPerSample(t.Codec) {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if bits, err := extractALACBitDepth(f); err != nil {
			scanLogger.Warn("mp4 ALAC bit-depth walk failed; manifest will carry nil bits",
				"path", absPath, "err", err)
		} else if bits > 0 {
			t.BitsPerSample = &bits
		}
	}
	// Sample rate: ALAC reads the authoritative rate from
	// ALACSpecificConfig (so hi-res 96/192 kHz survives), AAC reads the
	// AudioSampleEntry 16.16 rate. Unlike bits, rate is meaningful for
	// the lossy `mp4a` path too, so this runs regardless of codec — it
	// moves AAC tracks out of the composition bar's "Unknown" bucket.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if rate, err := extractMP4SampleRate(f); err != nil {
		scanLogger.Warn("mp4 sample-rate walk failed; manifest will carry nil sampleRate",
			"path", absPath, "err", err)
	} else if rate > 0 {
		t.SampleRate = &rate
	}
	// Seek to head before handing the reader to dhowden/tag —
	// the codec / bit-depth / sample-rate walks all consumed bytes.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractViaDhowdenFromReader(f, absPath, t, ec)
}

// extractByFormat is the context-aware variant of Extract. When ec
// is non-nil and ec.ArtworkCacheDir is non-empty, after tag extraction
// the local-artwork pipeline runs: an embedded ID3 APIC picture (or a
// directory-level cover.jpg / folder.jpg) is hashed (SHA-256), atomic-
// written to <ec.ArtworkCacheDir>/local-<hash>-500.jpg, and
// t.ArtworkMBID is stamped with `local-<hash>`. The /v1/artwork
// handler serves the file transparently via its relaxed MBID regex.
//
// JPEG-only by design. Embedded APIC frames must declare
// `image/jpeg` MIME (or the `image/jpg` variant) AND start with the
// JPEG SOI marker; folder-level fallback only matches `cover.jpg`
// and `folder.jpg` (case-insensitive). PNG support would require
// path-scheme + Content-Type changes done together; that's a follow-
// up, not V1 scope. See folderArtCandidates and looksLikeJPEG.
func extractByFormat(absPath string, t *Track, ec *ExtractContext) error {
	ext := strings.ToLower(filepath.Ext(absPath))
	// KEEP IN SYNC with the Ext map: every extension routed to a
	// dedicated case below must also be registered in Ext, or the
	// scanner's discovery gate skips those files before they ever
	// reach this dispatcher. TestExtCoversDispatcher pins it.
	switch ext {
	case ".dsf":
		return extractDSFWithContext(absPath, t, ec)
	case ".dff":
		return extractDFFWithContext(absPath, t, ec)
	case ".aif", ".aiff", ".aifc":
		return extractAIFFWithContext(absPath, t, ec)
	case ".wav":
		return extractWAVWithContext(absPath, t, ec)
	case ".m4a", ".mp4", ".m4b", ".m4p":
		return extractMP4WithContext(absPath, t, ec)
	case ".mp3":
		// MP3 is unambiguous; stamp the codec at the path level so
		// the iOS `Track.codec` filter matches "MP3" without
		// fallback. dhowden's `tag.FileType()` IS reliable for MP3
		// (returns `tag.MP3`), but we set it directly here to avoid
		// an extra step.
		t.Codec = "MP3"
		// Open once: read the sample rate from the first MPEG frame
		// header (dhowden surfaces tags but not frame geometry), then
		// rewind and hand the same handle to the tag reader — the
		// single-open-then-rewind pattern the FLAC branch uses. Bit
		// depth stays nil (not meaningful for a lossy codec).
		f, err := os.Open(absPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if rate, err := extractMP3SampleRate(f); err != nil {
			scanLogger.Warn("mp3 sample-rate parse failed; manifest will carry nil sampleRate",
				"path", absPath, "err", err)
		} else if rate > 0 {
			t.SampleRate = &rate
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractViaDhowdenFromReader(f, absPath, t, ec)
	case ".ogg", ".oga":
		// OGG container; for v1.2 we conservatively report "OGG"
		// rather than trying to distinguish Vorbis vs Opus — both
		// are lossy, both bin under the iOS `.lossy` filter via the
		// `hasPrefix("OGG")` check (the legacy "M4A" branch covers
		// this codepath today; "OGG" added to be explicit).
		t.Codec = "OGG"
		return extractViaDhowdenWithContext(absPath, t, ec)
	case ".flac":
		// Pre-fix the FLAC branch opened the file twice: once for
		// `extractFLACFormat` (`flac.ParseFile`) and once for
		// `extractViaDhowdenWithContext` (`os.Open` + `tag.ReadFrom`).
		// On NAS-mounted libraries (or low-RAM Pi hosts where pages
		// get evicted between reads), the embedded artwork — usually
		// 5–10 MiB JPEGs — went over the wire twice per track, halving
		// scanner throughput. Per Gemini A8 / iOS bug review #7.
		//
		// Open once, hand the *os.File to both layers with a
		// Seek(0,Start) between. flac.New buffers internally via
		// bufio.NewReader so the file's read pointer is somewhere
		// inside the FLAC blocks when it returns; the Seek rewinds
		// for tag.ReadFrom (which is io.ReadSeeker-shaped). On local
		// disk this is a no-op (kernel page cache absorbed the second
		// open before too); on a NAS mount it halves the per-track
		// network read.
		f, err := os.Open(absPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := extractFLACFormatFromReader(f, absPath, t); err != nil {
			// Format parse failure is non-fatal — the tag pass below may
			// still populate the track — but a corrupt STREAMINFO leaves
			// it with no sampleRate/bits/duration, so surface a breadcrumb
			// (DSF returns the err → scanner logs; MP3/MP4 log at Warn).
			// Kept non-fatal: don't fail the whole extraction.
			scanLogger.Warn("flac format parse failed; sampleRate/bits/duration will be nil", "path", absPath, "err", err)
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := extractViaDhowdenFromReader(f, absPath, t, ec); err != nil {
			return err
		}
		// dhowden/tag's Vorbis reader collapses multi-value tags into a
		// single value (last-wins map insert) — a FLAC tagged with
		// `ARTIST=Abdullah Ibrahim` + `ARTIST=Ekaya` arrives at
		// `m.Artist() == "Ekaya"`, losing the primary credit. A third
		// pass over the same file handle reads the raw Vorbis Comment
		// block via mewkiz/flac and overrides `t.Artist` /
		// `t.AlbumArtist` with `"; "`-joined strings when multi-value
		// is detected. Cheap: only the Vorbis Comment block body is
		// actually parsed (typically a few hundred bytes); the
		// PICTURE block (the heavy 5-10 MiB JPEG) is skipped via the
		// block header's length field. No extra `os.Open` — uses the
		// same `*os.File` after a rewind.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		applyFLACMultiValueArtists(f, t)
		return nil
	default:
		return extractViaDhowdenWithContext(absPath, t, ec)
	}
}

// extractViaDhowden uses github.com/dhowden/tag to read common tag formats.
// Supports ID3v1/2 in MP3, iTunes tags in M4A/MP4, Vorbis comments in FLAC
// and OGG. Unsupported extensions return nil without modifying t — a file
// indexed only by name is still useful, and enrichment (PR #8) can fill in.
func extractViaDhowden(absPath string, t *Track) error {
	return extractViaDhowdenWithContext(absPath, t, nil)
}

// extractViaDhowdenWithContext is the variant called from
// ExtractWithContext for non-FLAC paths. Opens the file itself and
// hands it to extractViaDhowdenFromReader. The FLAC path uses
// extractViaDhowdenFromReader directly — see ExtractWithContext for
// the single-open-then-rewind pattern.
func extractViaDhowdenWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return extractViaDhowdenFromReader(f, absPath, t, ec)
}

// extractViaDhowdenFromReader is the underlying tag-reading worker.
// Takes an already-open io.ReadSeeker so the FLAC branch in
// ExtractWithContext can hand off the same *os.File previously used
// by extractFLACFormatFromReader (after a Seek(0,Start)) — single
// open per FLAC scan instead of two. After populating tag fields it
// hands the dhowden Metadata (which holds the embedded APIC bytes
// via its Picture() accessor) to extractLocalArtwork so embedded art
// and the folder.jpg fallback both run from the same code path.
//
// Per Gemini A8 / iOS bug review #7.
func extractViaDhowdenFromReader(f io.ReadSeeker, absPath string, t *Track, ec *ExtractContext) error {
	m, err := tag.ReadFrom(f)
	if errors.Is(err, tag.ErrNoTagsFound) {
		// No embedded tags — but a folder.jpg next to the file is
		// still possible. Run extractLocalArtwork with m=nil so the
		// embedded branch is skipped and only the folder-level
		// fallback fires.
		if ec != nil && ec.ArtworkCacheDir != "" {
			extractLocalArtwork(absPath, t, nil, ec)
		}
		return nil
	}
	if err != nil {
		// Corrupt/partial tag — skip but don't fail the whole scan.
		// Folder fallback is still worth attempting.
		if ec != nil && ec.ArtworkCacheDir != "" {
			extractLocalArtwork(absPath, t, nil, ec)
		}
		return nil
	}
	populateFromTagMetadata(m, t)
	// ID3v2 / MP4 multi-value pickup. Reads m.Raw() for embedded
	// NULL-separators (ID3v2.4) or []string slices (MP4 multiple-
	// data-atom), joins with "; " and overrides Artist /
	// AlbumArtist on hit. Runs AFTER populateFromTagMetadata so the
	// override sees dhowden's last-wins value as the baseline.
	applyMultiValueArtistsFromRaw(m, t)
	if ec != nil && ec.ArtworkCacheDir != "" {
		extractLocalArtwork(absPath, t, m, ec)
	}
	return nil
}

// populateFromTagMetadata copies known fields out of a dhowden/tag Metadata
// into our Track, leaving empty what the file didn't have.
//
// **Codec invariant** (DO NOT VIOLATE): this function MUST NOT set
// `t.Codec` from `m.FileType()` or any other source. Format-specific
// identification runs upstream — mp4codec.go's stsd FourCC walk
// discriminates ALAC vs AAC for M4A, the extension routing pins
// MP3 / OGG / FLAC / DSF / DFF / WAV / AIFF — and is authoritative.
// Overwriting from `m.FileType()` here would erase the ALAC-vs-AAC
// discrimination the `canSetBitsPerSample` gate at every `t.BitsPerSample`
// write site depends on, re-introducing the iOS PR #371 "M4A 32-bit"
// chip regression by a different path.
//
// **Whitespace hygiene** (Gemini A6 / iOS bug review #6e): every string
// tag is `strings.TrimSpace`'d before persisting. iOS already
// re-trims + collapses whitespace via `MetadataNormalizer.normalize`
// at album-id-build time, so this is defensive cleanup — but the
// trim happens once at the bridge and benefits any future consumer
// of the manifest that doesn't normalize.
//
// **Compilation safety net** (Gemini A6 / iOS bug review #6b): when
// the file carries a `COMPILATION` / `TCMP` / `cpil` flag set to "1"
// AND has no explicit `AlbumArtist`, force `AlbumArtist = "Various
// Artists"`. Pre-fix the iOS bridge upsert fell back to per-track
// `Artist` for albumArtist on missing-tag, which produced one Album
// row per artist on multi-artist compilations. iOS doesn't see the
// compilation flag (it's not in BridgeTrack), so the synth has to
// happen here.
func populateFromTagMetadata(m tag.Metadata, t *Track) {
	if v := strings.TrimSpace(m.Title()); v != "" {
		t.Title = v
	}
	if v := strings.TrimSpace(m.Artist()); v != "" {
		t.Artist = v
	}
	if v := strings.TrimSpace(m.AlbumArtist()); v != "" {
		t.AlbumArtist = v
	}
	if v := strings.TrimSpace(m.Album()); v != "" {
		t.Album = v
	}
	if v := strings.TrimSpace(m.Genre()); v != "" {
		t.Genre = v
	}
	// **Pointer-zero correctness pass** (PR-B): `dhowden/tag` returns
	// 0 for BOTH "tag absent" and "tag value is 0" — at the parsed-
	// value layer they're indistinguishable. To preserve the
	// `*int` "absent" semantic (nil vs Some(0)), we probe the raw
	// tag map BEFORE calling m.Year() / m.Track() / m.Disc() and
	// only assign the pointer when at least one alias for the
	// underlying tag is actually present in the raw map.
	//
	// stringOf already returns (string, bool) with case-folded +
	// space-to-underscore normalisation via normaliseRawTagKey, so
	// the presence signal is the second return value. Aliases are
	// passed in lowercase to match the normalised lookup form.
	// A returned "0" still counts as present (the user-intent case
	// the original docblock was protecting); only absence drops
	// to nil so the iOS client can distinguish "no tag" from
	// "explicit year=0" and surface "Unknown" cleanly.
	//
	// When raw is nil (defensive — most format parsers return a
	// non-nil map even when empty), fall back to the legacy
	// always-Some-pointer shape so partial-tag scenarios stay
	// observable from the wire.
	if raw := m.Raw(); raw != nil {
		// CodeRabbit Major on PR #226: `stringOf` returns `ok=false`
		// when the key IS present but the underlying `m.Raw()` value
		// type isn't one of `string` / `[]string` / `int` (e.g. MP4's
		// `trkn` atom surfaces as `*tag.Position` via dhowden,
		// `[]byte`, etc.). Using stringOf-based presence here would
		// silently drop `Year` / `TrackNumber` / `DiscNumber` even
		// though the dhowden accessor would happily parse the value.
		// `hasAnyRawKey` is key-only — value shape is irrelevant.
		if hasAnyRawKey(raw, "tyer", "tdrc", "tdrl", "date", "year", "©day", "©yyy") {
			y := m.Year()
			// dhowden's `Year()` returns 0 for an ISO-8601 date value like
			// "2023-06-09" (a valid DATE / TDRC tag — Melody Gardot's
			// "Entre eux deux (The Paris Sessions)" is tagged that way).
			// Recover the 4-digit year from the raw tag the same way
			// `OriginalYear` already does (`parseYearPrefix`), so a
			// full-date release tag doesn't surface as year 0. Only on the
			// 0 case — a clean `m.Year()` (plain "2022") is left untouched.
			if y == 0 {
				if v, ok := stringOf(raw, "tdrc", "tdrl", "tyer", "date", "year", "©day", "©yyy"); ok {
					if py, perr := parseYearPrefix(v); perr == nil {
						y = py
					}
				}
			}
			t.Year = &y
		}
		if hasAnyRawKey(raw, "trck", "tracknumber", "trkn") {
			tn, _ := m.Track()
			t.TrackNumber = &tn
		}
		if hasAnyRawKey(raw, "tpos", "discnumber", "disk") {
			d, _ := m.Disc()
			t.DiscNumber = &d
		}
	} else {
		y := m.Year()
		t.Year = &y
		tn, _ := m.Track()
		t.TrackNumber = &tn
		d, _ := m.Disc()
		t.DiscNumber = &d
	}
	// MusicBrainz IDs — many tagged libraries carry these. The
	// case/space-agnostic stringOf below catches both Vorbis-flavour
	// keys (`MUSICBRAINZ_ALBUMID`) AND ID3v2 TXXX descriptions
	// (`MusicBrainz Album Id` — Picard's canonical form).
	if raw := m.Raw(); raw != nil {
		// Compilation safety net (CLAUDE.md / Gemini A6 / iOS bug
		// review #6b). Only fires when albumArtist is empty AND a
		// compilation flag is set to "1" — preserves the user's
		// explicit albumArtist when one is tagged. TCMP (ID3v2),
		// CPIL (iTunes / MP4), COMPILATION (Vorbis / FLAC) all
		// carry the same semantic.
		if t.AlbumArtist == "" {
			if comp, ok := stringOf(raw, "tcmp", "cpil", "compilation"); ok && comp == "1" {
				t.AlbumArtist = "Various Artists"
			}
		}
		// Pass BOTH underscore-joined ("musicbrainz_trackid") AND
		// space-derived ("musicbrainz_track_id") variants — they
		// normalise differently and both are valid spellings on the
		// wire (Vorbis vs ID3v2 TXXX). See stringOf docstring.
		if v, ok := stringOf(raw, "musicbrainz_trackid", "musicbrainz_track_id"); ok {
			t.MusicBrainzTrackID = v
		}
		if v, ok := stringOf(raw, "musicbrainz_albumid", "musicbrainz_album_id"); ok {
			t.MusicBrainzAlbumID = v
		}
		// ReplayGain.
		if v, ok := stringOf(raw, "replaygain_track_gain"); ok {
			if g := parseReplayGain(v); g != nil {
				t.ReplayGainTrackDB = g
			}
		}
		if v, ok := stringOf(raw, "replaygain_album_gain"); ok {
			if g := parseReplayGain(v); g != nil {
				t.ReplayGainAlbumDB = g
			}
		}
		// Classical-metadata fields (PR-D). Each is presence-gated
		// via stringOf's (string, bool) return — present-but-empty
		// raw values are dropped at the populate site below by the
		// non-empty check, but the bool gate also lets explicit
		// zero values surface for OriginalYear / BPM (the iOS-side
		// absent-vs-zero discipline established in PR-B).
		if v, ok := stringOf(raw, "tcom", "composer", "©wrt"); ok && v != "" {
			t.Composer = v
		}
		if v, ok := stringOf(raw, "tpe3", "conductor", "©con"); ok && v != "" {
			t.Conductor = v
		}
		// Work title: classical taggers store the work in TIT1
		// (Content Group Description) while keeping the movement
		// in TIT2 (Title). Both ID3v2 spelling AND the Vorbis /
		// MP4 equivalents need to map to t.Work.
		if v, ok := stringOf(raw, "tit1", "work", "©wrk"); ok && v != "" {
			t.Work = v
		}
		// OriginalYear: parsed via strconv to mirror dhowden's
		// year parse. Accepts both 4-digit "YYYY" and ISO-8601
		// "YYYY-MM-DD" prefixes (TDOR is ISO-8601 in ID3v2.4).
		if v, ok := stringOf(raw, "tory", "tdor", "originalyear", "originaldate"); ok && v != "" {
			if y, err := parseYearPrefix(v); err == nil {
				t.OriginalYear = &y
			}
		}
		// BPM: TBPM is an integer-valued text frame in ID3v2; the
		// MP4 `tmpo` atom is a uint16. Both surface via stringOf
		// as the parsed-int's string form.
		if v, ok := stringOf(raw, "tbpm", "bpm", "tmpo"); ok && v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				t.BPM = &n
			}
		}
	}
}

// applyMultiValueArtistsFromRaw mirrors applyFLACMultiValueArtists
// for MP4 (M4A / M4B / M4P) iTunes-style multi-value tagging. Reads
// dhowden/tag's `m.Raw()` map for entries surfaced as `[]string`
// (multiple `data` sub-atoms inside one ilst tag atom — `©ART` /
// `aART`) and overrides Artist / AlbumArtist with `"; "`-joined
// strings.
//
// Also includes a defensive NULL-separated-string code path that
// fires when ANY consumer (dhowden, a future tag library) surfaces
// a tag value with embedded `\x00` separators in m.Raw(). Today's
// dhowden ID3v2 text-frame reader strips NULLs internally (see
// readTFrame in dhowden/id3v2frames.go: `strings.Join(strings.Split
// (txt, string([]byte{0})), "")`), so this branch is forward-
// compatible against a future dhowden release that preserves NULLs
// rather than load-bearing for v2.4 today.
//
// Limitations (documented honestly):
//   - ID3v2.3 multi-FRAME (two separate TPE1 frames): NOT covered.
//     dhowden's map collapses repeated frame IDs to last-wins, and
//     there's no public dhowden API to recover prior frame
//     instances. A custom ID3v2 frame walker could close this;
//     deferred until field reports require it.
//   - ID3v2.4 NULL-separated within one TPE1 frame: NOT effectively
//     covered today (dhowden strips NULLs). The same custom walker
//     would close this. The NULL-detection path stays as forward-
//     compat insurance — it costs nothing when no NULLs are seen.
//
// In practice, this PR primarily benefits MP4-tagged libraries
// (where dhowden DOES surface multi-data atoms as []string).
// FLAC / OGG already have the FLAC-specific multi-value pass via
// applyFLACMultiValueArtists. MP3 / DSF multi-value remains
// last-wins until a custom ID3v2 walker lands.
//
// Best-effort: a single-value tag (no NULLs, single-string raw)
// no-ops — dhowden's last-wins value already populated the field
// via populateFromTagMetadata. Detected multi-value with only one
// non-empty entry after trim also no-ops (the override would be
// a no-op rewrite of the same single value).
func applyMultiValueArtistsFromRaw(m tag.Metadata, t *Track) {
	if m == nil {
		return
	}
	raw := m.Raw()
	if raw == nil {
		return
	}
	// ID3v2: tpe1 (artist), tpe2 (album artist).
	// MP4 ilst: ©art (artist), aart (album artist).
	// Vorbis (FLAC/OGG): artist, albumartist, album_artist —
	// included for the case where applyFLACMultiValueArtists
	// missed (it's only invoked on the FLAC branch; OGG paths
	// can still benefit from the generic NULL-separated detection).
	if values := extractMultiValueTagFromRaw(raw, "tpe1", "©art", "artist"); len(values) > 1 {
		t.Artist = strings.Join(values, "; ")
	}
	if values := extractMultiValueTagFromRaw(raw, "tpe2", "aart", "albumartist", "album_artist"); len(values) > 1 {
		t.AlbumArtist = strings.Join(values, "; ")
	}
}

// extractMultiValueTagFromRaw iterates m.Raw() looking for entries
// whose normalised key matches any of the supplied aliases. On
// match, the entry's value is converted to a `[]string` from
// either:
//   - a `string` with embedded `\x00` NULL-separators (split into
//     pieces — ID3v2.4 convention), OR
//   - a `[]string` (each element treated as one value — MP4
//     multiple-data-atom convention).
//
// **Scans all matching aliases and returns the longest list**
// rather than returning on first match (CodeRabbit Minor on
// PR #227). Map iteration order is non-deterministic in Go, so an
// early-return on the first match would make override behaviour
// order-dependent: if `raw` happened to surface a single-value
// alias before a truly-multi-value one, the multi-value alias
// would be silently masked. The "longest wins" tie-break is the
// least-surprising shape — it always picks the genuinely multi-
// valued entry when one is present, regardless of map order.
//
// Returns nil when no match OR every match collapses to one
// non-empty entry; caller checks `len(values) > 1` before
// overriding so a single-value tag doesn't get pointlessly
// rewritten.
//
// Aliases passed in lowercase to match normaliseRawTagKey output.
func extractMultiValueTagFromRaw(raw map[string]any, keys ...string) []string {
	var best []string
	for rawKey, v := range raw {
		if !rawKeyMatches(normaliseRawTagKey(rawKey), keys) {
			continue
		}
		var candidate []string
		switch s := v.(type) {
		case string:
			if strings.Contains(s, "\x00") {
				candidate = trimNonEmpty(strings.Split(s, "\x00"))
			}
		case []string:
			candidate = trimNonEmpty(s)
		}
		if multiValueCandidateWins(candidate, best) {
			best = candidate
		}
	}
	return best
}

// rawKeyMatches reports whether a normalised raw-tag key equals any of the
// wanted aliases. Extracted so extractMultiValueTagFromRaw stays within the
// cognitive-complexity budget (the inline match loop plus the tie-break
// pushed it over).
func rawKeyMatches(norm string, keys []string) bool {
	for _, k := range keys {
		if norm == k {
			return true
		}
	}
	return false
}

// multiValueCandidateWins reports whether `candidate` should replace `best`
// as the chosen multi-value result: a longer slice wins, and on a length
// tie the lexicographically-smaller slice wins so the choice is
// deterministic regardless of Go's randomised map iteration order
// (honouring the "regardless of map order" contract documented on
// extractMultiValueTagFromRaw). Inputs are already trimmed to non-empty
// entries by the caller; equal-length equal-content (or both empty) keeps
// `best`.
func multiValueCandidateWins(candidate, best []string) bool {
	if len(candidate) != len(best) {
		return len(candidate) > len(best)
	}
	for i := range candidate {
		if candidate[i] != best[i] {
			return candidate[i] < best[i]
		}
	}
	return false
}

// trimNonEmpty trims whitespace from each entry and drops empties.
// Used by extractMultiValueTagFromRaw to filter trailing-empty
// segments (a trailing NULL or a `data` atom with empty payload).
func trimNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// skipID3v2 advances r past a leading ID3v2 tag if present, leaving the
// cursor at the real payload start (the fLaC magic for our FLAC callers).
// Some taggers prepend an ID3v2 tag to FLAC — out of spec but common
// enough that dhowden/tag tolerates it; our STREAMINFO + Vorbis passes
// read the magic at the current offset and would otherwise bail,
// silently dropping hi-res format fields and multi-value artists.
// No-op (rewinds to the original offset) when no ID3v2 tag is present or
// the stream is too short to hold a header.
func skipID3v2(r io.ReadSeeker) error {
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	var h [10]byte
	if _, err := io.ReadFull(r, h[:]); err != nil || string(h[0:3]) != "ID3" {
		// Not an ID3 tag, or the stream is too short to hold a header.
		// Rewind and let the caller's fLaC-magic read produce the real
		// verdict; only a failed rewind is worth surfacing.
		if _, serr := r.Seek(start, io.SeekStart); serr != nil {
			return serr
		}
		return nil
	}
	// 28-bit synchsafe size (7 bits/byte), excludes the 10-byte header.
	size := int64(h[6]&0x7f)<<21 | int64(h[7]&0x7f)<<14 | int64(h[8]&0x7f)<<7 | int64(h[9]&0x7f)
	skip := start + 10 + size
	// The footer flag (bit 4) is only defined in ID3v2.4 — in v2.2/v2.3
	// that bit is unused, so a non-conforming tagger setting it must not
	// make us over-skip into the payload. Gate on the major version.
	if h[3] >= 4 && h[5]&0x10 != 0 {
		skip += 10 // optional ID3v2.4 footer
	}
	_, err = r.Seek(skip, io.SeekStart)
	return err
}

// applyFLACMultiValueArtists scans the FLAC's Vorbis Comment block for
// repeated `ARTIST` / `ALBUMARTIST` entries (which dhowden/tag's
// last-wins map collapses to a single value) and overrides
// `t.Artist` / `t.AlbumArtist` with a `"; "`-joined string when
// multi-value is detected. The separator matches the iOS-side
// `VorbisCommentParser.appendMulti` convention so the iOS picker /
// upsert split logic activates without any wire-protocol change.
//
// Cost: parses the block header for every metadata block (cheap —
// 4 bytes each) and the BODY of only the Vorbis Comment block.
// PICTURE blocks (the 5–10 MiB JPEGs the existing single-open
// optimization was protecting) get skipped via the block's
// `Skip()` method — which checks whether the body
// reader is an `io.Seeker`. `meta.New` wraps the body in an
// `io.LimitReader`, and `*io.LimitedReader` is never a Seeker — so
// `Skip()` always reads+discards the body via `io.Copy(io.Discard,
// ...)`, advancing `r` past the block without materialising the
// large payload into any buffer of ours.
//
// Best-effort: malformed / corrupt FLAC streams silently no-op
// rather than failing the scan. Single-value tags also no-op (the
// dhowden-populated value is correct in that case). Non-FLAC
// formats are NOT covered by this helper — ID3v2 TPE1 multi-value
// (MP3 / DSF / M4A) is a deferred follow-up.
func applyFLACMultiValueArtists(r io.ReadSeeker, t *Track) {
	// Skip a leading ID3v2 tag if some tagger prepended one (out of
	// spec but common); otherwise the fLaC magic check below fails and
	// the multi-value artists silently collapse to dhowden's last-wins
	// single value.
	if err := skipID3v2(r); err != nil {
		return
	}
	// FLAC magic.
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return
	}
	if string(magic[:]) != "fLaC" {
		return
	}
	for {
		block, err := meta.New(r)
		if err != nil {
			return
		}
		if block.Type == meta.TypeVorbisComment {
			if err := block.Parse(); err != nil {
				return
			}
			vc, ok := block.Body.(*meta.VorbisComment)
			if !ok {
				return
			}
			var artists, albumArtists []string
			for _, tg := range vc.Tags {
				switch strings.ToLower(tg[0]) {
				case "artist":
					if v := strings.TrimSpace(tg[1]); v != "" {
						artists = append(artists, v)
					}
				case "albumartist", "album artist":
					// Both spellings exist in the wild — Picard /
					// dBpoweramp use `ALBUMARTIST`, older taggers +
					// some manual edits use `ALBUM ARTIST` (with
					// space). dhowden/tag normalises both internally
					// when querying via `AlbumArtist()` but on raw
					// iteration we have to accept either spelling
					// or multi-value `ALBUM ARTIST` Vorbis files
					// would silently fall through this branch.
					// Gemini HIGH on PR #208.
					if v := strings.TrimSpace(tg[1]); v != "" {
						albumArtists = append(albumArtists, v)
					}
				}
			}
			// Override whenever we found at least one non-empty entry
			// in the raw block. Catches the edge case where dhowden
			// saw `ARTIST=Name` followed by `ARTIST=` (a trailing
			// empty multi-value): dhowden's last-wins map stored "",
			// `populateFromTagMetadata` skipped the empty assignment,
			// and `t.Artist` was left at whatever the path-derived
			// fallback produced. Our multi-value pass filters empties
			// out per segment, so a single non-empty entry in
			// `artists` is the correct ground-truth result —
			// asserting it over dhowden's possibly-empty value is
			// strictly an improvement. Gemini medium on PR #208.
			if len(artists) > 0 {
				t.Artist = strings.Join(artists, "; ")
			}
			if len(albumArtists) > 0 {
				t.AlbumArtist = strings.Join(albumArtists, "; ")
			}
			return
		}
		// Non-Vorbis block (STREAMINFO, PICTURE, PADDING, etc.) —
		// skip the body without buffering it. `block.Skip()` reads the
		// body through the block's internal `io.LimitReader` (bounded to
		// `block.Length`) and discards it via `io.Copy`, advancing `r` to
		// the next block header without touching the payload bytes.
		if err := block.Skip(); err != nil {
			return
		}
		if block.IsLast {
			return
		}
	}
}

// stringOf looks up keys in a raw tag map. Caller-provided keys
// MUST already be in normalised form: lowercased AND with spaces
// replaced by underscores (the `normaliseRawTagKey` shape). The
// function normalises each map key once per iteration and compares
// directly against the caller's set — no per-call allocation, no
// repeated normalisation of the static literals at call sites.
//
// To match BOTH Vorbis-style ("MUSICBRAINZ_ALBUMID") and ID3v2-TXXX-
// style ("MusicBrainz Album Id") shapes, callers MUST pass BOTH
// normalised variants where they differ — e.g. "musicbrainz_album_id"
// (from the space-separated form) AND "musicbrainz_albumid" (from
// the underscore-separated form). The normaliser bridges minor case
// + space-vs-underscore variants WITHIN one spelling but NOT across
// fundamentally different word-boundary shapes.
//
// Pre-fix the lookup did exact case-sensitive map subscripts, so
// MBID extraction silently failed for any ID3v2-tagged album —
// Cover-Art-Archive enrichment fell back to the lower-quality iTunes
// path, and the iOS app couldn't fall through to bridge-served
// local-art for those files. Per Gemini A6 / iOS bug review #6d.
//
// Trimmed return: every matched value is `strings.TrimSpace`'d
// before being returned, defending against trailing-space tagging
// hygiene issues at the consuming layer (Vorbis comments rarely
// carry leading/trailing whitespace, but ID3v2 TXXX frames
// occasionally do).
func stringOf(raw map[string]any, keys ...string) (string, bool) {
	if len(keys) == 0 {
		return "", false
	}
	// Coerce a raw tag value to a non-empty trimmed string, mirroring the
	// value shapes dhowden/tag surfaces.
	coerce := func(v any) (string, bool) {
		switch s := v.(type) {
		case string:
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed, true
			}
		case []string:
			// Scan the WHOLE slice — Vorbis comments can carry the
			// same key multiple times (e.g. `COMMENT="" / COMMENT="real"`).
			// Pre-fix, only `s[0]` was inspected so a leading blank
			// entry treated the tag as absent. CodeRabbit Minor
			// round-1 on PR #166.
			for _, item := range s {
				if trimmed := strings.TrimSpace(item); trimmed != "" {
					return trimmed, true
				}
			}
		case int:
			// dhowden/tag surfaces MP4/M4A `cpil` (compilation atom)
			// as int (0 or 1) via getInt(b[:1]) — NOT a string. Pre-fix
			// the compilation safety net's `stringOf(... ) == "1"`
			// check silently failed for every M4A compilation, leaving
			// the AlbumArtist synth dead on that codepath. Coerce to
			// string so downstream value comparisons work uniformly.
			// CodeRabbit Critical round-1 on PR #166.
			return strconv.Itoa(s), true
		case int64:
			return strconv.FormatInt(s, 10), true
		case uint8:
			// Defensive: dhowden's atomTypes class 21 (uint8) is the
			// MP4 path most cpil values surface through; some library
			// versions return uint8 directly rather than promoting to
			// int. Both shapes mean the same thing on the wire.
			return strconv.FormatUint(uint64(s), 10), true
		case bool:
			// Some tag libraries (and a future dhowden refactor) might
			// surface boolean atoms directly. "1"/"0" matches the
			// existing string-comparison conventions at call sites.
			if s {
				return "1", true
			}
			return "0", true
		}
		return "", false
	}
	// Iterate the REQUESTED keys in priority order — NOT the raw map, whose
	// Go iteration order is randomized. When a file carries more than one
	// matching tag (e.g. both DATE and YEAR, or TDRC + TYER), the earliest-
	// listed key wins DETERMINISTICALLY, so the resolved value (and any year
	// parsed from it) is stable across scans instead of flapping with map
	// order. Gemini HIGH on PR #447. The inner scan is O(map) but the map is
	// a dozen tags and `keys` is a handful, so the nesting is negligible.
	for _, k := range keys {
		for mapKey, v := range raw {
			if normaliseRawTagKey(mapKey) != k {
				continue
			}
			if out, ok := coerce(v); ok {
				return out, true
			}
		}
	}
	return "", false
}

// normaliseRawTagKey lowercases and replaces spaces with underscores
// so the search keys passed to stringOf can match both Vorbis-style
// (`MUSICBRAINZ_ALBUMID`) and ID3v2-TXXX-style (`MusicBrainz Album Id`)
// shapes uniformly. The replacement is space → underscore only —
// other punctuation (slash, colon, etc.) stays intact since dhowden's
// raw map keys preserve them on the surfaces we care about.
func normaliseRawTagKey(k string) string {
	return strings.ReplaceAll(strings.ToLower(k), " ", "_")
}

// hasAnyRawKey reports whether `raw` contains a tag entry whose
// normalised key (lowercased + space→underscore) matches any of the
// supplied aliases. Value-shape agnostic — unlike `stringOf` which
// peeks at the value and returns `ok=false` for unsupported types,
// this helper only checks for key presence.
//
// **Why a key-only helper exists alongside `stringOf`** (CodeRabbit
// Major on PR #226): the `populateFromTagMetadata` presence gates for
// `Year` / `TrackNumber` / `DiscNumber` need to know whether the
// underlying ID3v2 / Vorbis / MP4 tag was actually present, not
// whether stringOf can decode its value. dhowden surfaces MP4's
// `trkn` atom as `*tag.Position` (NOT a string / []string / int), so
// a stringOf-based gate would treat a tagged MP4 with explicit track
// number as "absent" and drop `t.TrackNumber` — even though
// `m.Track()` would parse it correctly. The same risk applies to any
// future raw value shape dhowden emits (e.g. `[]byte` for binary
// frames).
//
// Aliases are passed in their normalised lowercase form to match
// what `normaliseRawTagKey` produces.
func hasAnyRawKey(raw map[string]any, keys ...string) bool {
	if raw == nil {
		return false
	}
	for mapKey := range raw {
		norm := normaliseRawTagKey(mapKey)
		for _, k := range keys {
			if norm == k {
				return true
			}
		}
	}
	return false
}

// parseReplayGain parses a Vorbis/ID3 ReplayGain string like "-7.32 dB".
//
// Lowercase + suffix strip is case-insensitive (handles "dB", "db",
// "Db", "DB"). Two TrimSpace calls are load-bearing: the leading one
// strips any trailing space AFTER the suffix (e.g. "-7.32 dB ") so
// TrimSuffix("db") can match; the trailing one removes the inner
// space the suffix removal exposed (e.g. "-7.32 db" → "-7.32 ").
func parseReplayGain(s string) *float64 {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSpace(strings.TrimSuffix(s, "db"))
	if s == "" {
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	// strconv.ParseFloat accepts "nan"/"inf" — real ReplayGain
	// scanners emit those for digital-silence tracks. A non-finite
	// gain would survive marshalForStorage (tag-derived values are
	// never scrubbed) and fail json.Marshal at batch-write time,
	// poisoning the whole 500-track scan batch on EVERY rescan.
	// Mirror of the parseAIFFExtended guard.
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// parseYearPrefix extracts a 4-digit year from the leading characters
// of `s`. Accepts both bare year strings ("1985") and ISO-8601 dates
// ("1985-06-22", "1985-06", "1985"). Returns an error if the first
// four characters aren't a parseable integer in the realistic
// year range [1, 9999].
//
// Used by populateFromTagMetadata for `OriginalYear` parsing: ID3v2.3
// `TORY` is a 4-digit year, ID3v2.4 `TDOR` is an ISO-8601 timestamp,
// Vorbis `ORIGINALDATE` is operator-discretion (often ISO-8601). The
// prefix parse handles all three uniformly.
//
// **5+ digit prefix rejection** (CodeRabbit Minor post-merge on
// PR #228 → #231): a malformed value like `"10000-01-01"` used to
// silently truncate to `1000`, corrupting OriginalYear. Now refuse
// the parse when the 5th character (if present) is also a digit —
// real ISO-8601 years are bounded at 4 digits in this format, and
// real bare-year strings end at 4 digits too. A 5th-position
// non-digit (separator, end-of-string) confirms the prefix is the
// year.
func parseYearPrefix(s string) (int, error) {
	s = strings.TrimSpace(s)
	if len(s) < 4 {
		return 0, fmt.Errorf("year prefix too short: %q", s)
	}
	if len(s) > 4 && s[4] >= '0' && s[4] <= '9' {
		return 0, fmt.Errorf("year prefix too long (5+ digit): %q", s)
	}
	y, err := strconv.Atoi(s[:4])
	if err != nil {
		return 0, err
	}
	if y < 1 || y > 9999 {
		return 0, fmt.Errorf("year out of range: %d", y)
	}
	return y, nil
}

// extractFLACFormat reads STREAMINFO from a FLAC file and fills sample
// rate, bit depth, and duration. Path-based shim — opens the file and
// hands it to extractFLACFormatFromReader. Used by anything outside
// ExtractWithContext (e.g. tests calling Extract directly).
func extractFLACFormat(absPath string, t *Track) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return extractFLACFormatFromReader(f, absPath, t)
}

// extractFLACFormatFromReader reads STREAMINFO from an already-open
// FLAC file. Duration is derived from total samples / sample rate,
// which is exact for FLAC. Used by ExtractWithContext's FLAC branch
// in the single-open-then-rewind path so we don't pull the file's
// embedded artwork twice over the network on NAS-mounted libraries.
//
// **STREAMINFO-only parse** (CodeRabbit Major round-1 on PR #165): the
// previous implementation called `flac.New(r)`, which after parsing
// STREAMINFO walks every remaining metadata block via `block.Skip()`
// — which CONSUMES the bytes from the underlying reader, not just
// the bufio buffer. For a track with embedded ~5–10 MiB PICTURE
// blocks, that meant the FLAC scan still pulled the picture bytes
// over the network, and the subsequent `tag.ReadFrom` re-pulled them
// — so the only thing the single-open path had actually saved was
// the second `os.Open` syscall. This parser stops AFTER the first
// metadata block (STREAMINFO is mandatory and must be first per the
// FLAC spec), so the metadata-tail bytes (PICTURE / VORBIS_COMMENT /
// etc.) are pulled exactly once, by the downstream `tag.ReadFrom`.
//
// Caller MUST `Seek(0, io.SeekStart)` after this returns before
// handing the reader to `tag.ReadFrom` — we read the FLAC magic +
// STREAMINFO header + body sequentially, so the read pointer is
// inside the metadata region when this returns.
//
// Per Gemini A8 / iOS bug review #7 + CodeRabbit Major round-1 on PR #165.
func extractFLACFormatFromReader(r io.ReadSeeker, absPath string, t *Track) error {
	// Skip a leading ID3v2 tag if present (some taggers prepend one to
	// FLAC — out of spec but common; dhowden tolerates it downstream).
	// Without this the magic check below fails on those files and the
	// hi-res format fields (sample rate, bit depth, duration) are
	// silently dropped.
	if err := skipID3v2(r); err != nil {
		return fmt.Errorf("flac: %q: skip id3v2: %w", absPath, err)
	}
	// FLAC magic: 4 bytes "fLaC".
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("flac: %q: read magic: %w", absPath, err)
	}
	if string(magic[:]) != "fLaC" {
		return fmt.Errorf("flac: %q: bad magic %q", absPath, magic[:])
	}
	// First metadata block header: 1 byte (is_last+type) + 3 bytes
	// (length, big-endian).
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("flac: %q: read block header: %w", absPath, err)
	}
	blockType := hdr[0] & 0x7F
	if blockType != 0 {
		// STREAMINFO is mandatory and MUST be the first metadata
		// block per the FLAC spec — refusing to recover from a
		// non-conforming file rather than silently skipping it.
		return fmt.Errorf("flac: %q: first metadata block is type %d, expected STREAMINFO (0)", absPath, blockType)
	}
	bodySize := uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	if bodySize != 34 {
		return fmt.Errorf("flac: %q: STREAMINFO body size %d, expected 34", absPath, bodySize)
	}
	// STREAMINFO body — 34 bytes. Layout (big-endian, bit-packed):
	//   16 bits: min block size
	//   16 bits: max block size
	//   24 bits: min frame size
	//   24 bits: max frame size
	//   20 bits: sample rate (Hz)
	//    3 bits: channels - 1
	//    5 bits: bits per sample - 1
	//   36 bits: total samples
	//  128 bits: MD5 checksum (ignored)
	var body [34]byte
	if _, err := io.ReadFull(r, body[:]); err != nil {
		return fmt.Errorf("flac: %q: read STREAMINFO body: %w", absPath, err)
	}
	// Skip min/max block + min/max frame sizes (bytes 0..9).
	// Sample rate occupies bytes 10..12 (top 20 bits).
	sampleRate := uint32(body[10])<<12 | uint32(body[11])<<4 | uint32(body[12])>>4
	// Channels: byte 12 bits 4..6 (lower 4 of byte 12 + … we just
	// want bits-per-sample). Bit layout of bytes 12..13 (16 bits):
	//   bits 0..3 (top of byte 12): low 4 bits of sample rate
	//   bits 4..6 (mid of byte 12): channels - 1
	//   bits 7..11: bits per sample - 1   ← spans byte 12 bit 7 and byte 13 bits 0..3
	//   bits 12..15: top 4 bits of total samples
	// Extract bits-per-sample (5 bits, packed across byte 12 bit 7
	// and byte 13 top 4 bits):
	bitsPerSample := int(((uint32(body[12])&0x01)<<4)|(uint32(body[13])>>4)) + 1
	// Total samples (36 bits, packed across byte 13 low 4 bits +
	// bytes 14..17):
	totalSamples := (uint64(body[13])&0x0F)<<32 |
		uint64(body[14])<<24 |
		uint64(body[15])<<16 |
		uint64(body[16])<<8 |
		uint64(body[17])

	// v1.2 additive: stamp the canonical codec for the iOS-side
	// `Track.codec` column. FLAC files are unambiguously FLAC at
	// this point. Codec MUST be stamped BEFORE BitsPerSample so the
	// `canSetBitsPerSample` gate sees the authoritative value (FLAC is
	// lossless → gate allows the bits write).
	t.Codec = "FLAC"

	sr := float64(sampleRate)
	bps := bitsPerSample
	t.SampleRate = &sr
	if canSetBitsPerSample(t.Codec) {
		t.BitsPerSample = &bps
	}
	// FLAC is always PCM by spec — set the explicit false so the
	// iOS decoder can trust `isDSD: false` to mean "definitely PCM"
	// rather than "format unknown". Mirrors the explicit `true` set
	// in `extractDSF` below.
	isDSD := false
	t.IsDSD = &isDSD
	if sampleRate > 0 && totalSamples > 0 {
		d := float64(totalSamples) / float64(sampleRate)
		t.Duration = &d
	}
	return nil
}

// extractDSF reads a DSD Stream File: "DSD " chunk → "fmt " chunk (sample
// rate, bit depth, sample count) → optional ID3v2 metadata at the pointer
// in the DSD chunk header. The iOS scanner's DSF parser does roughly the
// same walk; we mirror it so manifest data matches what the app would
// extract locally.
func extractDSF(absPath string, t *Track) error {
	return extractDSFWithContext(absPath, t, nil)
}

// extractDSFWithContext is the variant called from ExtractWithContext.
// Identical format-parse logic; after tag extraction it runs the
// local-artwork hook so embedded APIC art inside DSF's ID3v2 chunk gets
// cached the same way as MP3 / FLAC / M4A. Folder-level cover.jpg
// fallback fires whether or not the DSF carried embedded tags.
func extractDSFWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, 28)
	if _, err := io.ReadFull(f, header); err != nil {
		return fmt.Errorf("dsf: short header: %w", err)
	}
	if string(header[0:4]) != "DSD " {
		return fmt.Errorf("dsf: bad magic %q", header[0:4])
	}
	metadataPointer := le64(header[20:28])

	fmtChunk := make([]byte, 52)
	if _, err := io.ReadFull(f, fmtChunk); err != nil {
		return fmt.Errorf("dsf: short fmt chunk: %w", err)
	}
	if string(fmtChunk[0:4]) != "fmt " {
		return fmt.Errorf("dsf: bad fmt magic %q", fmtChunk[0:4])
	}
	sampleRate := le32(fmtChunk[28:32])
	bitsPerSample := le32(fmtChunk[32:36]) // 1 for DSD
	// DSF v1.5 fmt chunk layout: sampleCount is bytes [36:44] and
	// blockSizePerChannel is [44:48]. Reading [40:48] straddles both
	// and yields garbage on every real-encoder (Korg/Sony/dCS) DSF.
	sampleCount := le64(fmtChunk[36:44])

	// v1.2 additive: canonical codec for iOS-side `Track.codec`.
	// Codec MUST be stamped BEFORE BitsPerSample so the
	// `canSetBitsPerSample` gate sees the authoritative value (DSF is
	// lossless → gate allows the bits write).
	t.Codec = "DSF"

	sr := float64(sampleRate)
	bps := int(bitsPerSample)
	t.SampleRate = &sr
	if canSetBitsPerSample(t.Codec) {
		t.BitsPerSample = &bps
	}
	// IsDSD policy (PR-A2):
	//   - `.dsf` is structurally a DSD container — default `IsDSD`
	//     to true rather than the pre-PR strict `bitsPerSample == 1`
	//     gate. The strict gate left well-formed files alone but
	//     mis-classified any anomalous-fmt-chunk DSF (e.g. a Korg
	//     encoder bug declaring bitsPerSample=8) as non-DSD even
	//     though the audio data structurally IS DSD.
	//   - BUT: a fmt chunk reporting a PCM-like sample rate
	//     (44100, 48000, etc.) is almost certainly a mislabeled
	//     PCM-in-DSF container; in that case force IsDSD=false to
	//     avoid the iOS decoder attempting a DoP lock that will
	//     fail. `isValidDSDSampleRate` accepts any multiple of
	//     64×44.1kHz or 64×48kHz (DSD64/128/256/512/1024).
	//   - Anomalous-bits-but-valid-DSD-rate case logs a warn so
	//     operators can correlate against offending files without
	//     blocking playback.
	isDSD := true
	if !isValidDSDSampleRate(sampleRate) {
		isDSD = false
		scanLogger.Warn("dsf: fmt chunk reports non-DSD sampleRate; classifying as non-DSD",
			"path", absPath, "sampleRate", sampleRate, "bitsPerSample", bitsPerSample)
	} else if bitsPerSample != 1 {
		scanLogger.Warn("dsf: fmt chunk reports non-1 bitsPerSample on valid DSD rate; keeping IsDSD=true",
			"path", absPath, "bitsPerSample", bitsPerSample, "sampleRate", sampleRate)
	}
	t.IsDSD = &isDSD
	if sampleRate > 0 && sampleCount > 0 {
		d := float64(sampleCount) / float64(sampleRate)
		t.Duration = &d
	}

	// Tags: ID3v2 at metadataPointer (if non-zero).
	var dsfMeta tag.Metadata
	if metadataPointer > 0 {
		if _, err := f.Seek(int64(metadataPointer), io.SeekStart); err != nil {
			// Tags optional — but the folder-level fallback is still
			// worth attempting since the directory-level cache
			// doesn't depend on tag-read success.
			if ec != nil && ec.ArtworkCacheDir != "" {
				extractLocalArtwork(absPath, t, nil, ec)
			}
			return nil
		}
		m, err := tag.ReadID3v2Tags(f)
		if err == nil && m != nil {
			populateFromTagMetadata(m, t)
			dsfMeta = m
		}
	}
	if ec != nil && ec.ArtworkCacheDir != "" {
		extractLocalArtwork(absPath, t, dsfMeta, ec)
	}
	return nil
}

// extractDFFWithContext walks just enough of a DSDIFF (.dff) container
// to populate Codec / IsDSD / SampleRate. Unlike DSF (a fixed binary
// header), DFF is chunk-based (FRM8 outer container with PROP and DSD
// children) and uses BIG-endian sizes. We don't decode tags — DFF's
// DIIN/COMT chunks aren't widely populated in the wild, and dhowden/tag
// doesn't recognize the container at all. Path-derived defaults +
// future enrichment fill in the rest.
//
// Compression handling: only `DSD ` (uncompressed) is supported by the
// iOS player. `DST ` (Direct Stream Transfer) is the lossless DSD
// compressor used in some SACD rips; iOS would refuse to play it. We
// log + return without populating IsDSD/SampleRate so iOS-side surface
// classifies the row as "an unknown audio file" rather than "a DSD
// track that fails to load". Codec stays "DFF" so `TrackQualityChip`
// still renders something sensible.
func extractDFFWithContext(absPath string, t *Track, ec *ExtractContext) error {
	t.Codec = "DFF"

	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// FRM8 outer chunk: 4 bytes magic, 8 bytes BE size, 4 bytes form
	// type. The "DSD " form-type tag is what disambiguates a DSDIFF
	// audio file from any other FRM8-based IFF dialect (e.g. AIFF
	// shares the chunk-walker shape but a different form magic).
	var frm8 [16]byte
	if _, err := io.ReadFull(f, frm8[:]); err != nil {
		return fmt.Errorf("dff: short outer header: %w", err)
	}
	if string(frm8[0:4]) != "FRM8" {
		return fmt.Errorf("dff: bad FRM8 magic %q", frm8[0:4])
	}
	if string(frm8[12:16]) != "DSD " {
		return fmt.Errorf("dff: not a DSDIFF DSD form (got %q)", frm8[12:16])
	}

	// Walk top-level chunks looking for PROP and DIIN. Each chunk: 4
	// bytes FOURCC, 8 bytes BE size, payload, then a single pad byte
	// if size is odd (DSDIFF / IFF chunk-pad rule). The DSD audio
	// chunk holds the actual samples — for high-resolution DSD256+
	// stereo this routinely exceeds 1 GiB. We don't allocate for it
	// (we just Seek past), so no sanity limit is needed at the outer
	// walk; only the PROP / DIIN body allocations have size caps.
	//
	// Chunk ORDER in DSDIFF is FRM8 → FVER → PROP → DSD → DIIN (DIIN
	// typically follows the audio payload per the spec). The walker
	// continues past every recognised chunk until EOF so DIIN is
	// reached regardless of where it lands.
	for {
		var chunkHeader [12]byte
		if _, err := io.ReadFull(f, chunkHeader[:]); err != nil {
			// EOF — normal terminator regardless of whether we found
			// PROP / DIIN. Codec already stamped.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				if ec != nil && ec.ArtworkCacheDir != "" {
					extractLocalArtwork(absPath, t, nil, ec)
				}
				return nil
			}
			return fmt.Errorf("dff: chunk header read: %w", err)
		}
		fourcc := string(chunkHeader[0:4])
		size := be64(chunkHeader[4:12])
		switch fourcc {
		case "PROP":
			// PROP body: 4 bytes form type ("SND "), then nested
			// chunks. 1 MiB cap on the body allocation — real PROP
			// chunks hold a handful of small property chunks (FS,
			// CHNL, CMPR, ABSS, LSCO) totalling well under 1 KiB; a
			// multi-MiB declared PROP is a corruption signal, not a
			// high-res DSD payload.
			if size < 4 {
				// Malformed: PROP payload too short to hold the
				// "SND " form-type. Pre-fix the bare `continue`
				// left the cursor at the start of the payload —
				// next iteration read 12 bytes from INSIDE the
				// PROP body and mis-interpreted them as a chunk
				// header, mis-aligning the rest of the walk.
				// Seek past whatever's declared (+ pad if odd) so
				// the walker stays aligned for later valid chunks.
				// CodeRabbit Critical post-merge on PR #223.
				skip, err := safeSeekSkip(size)
				if err != nil {
					return fmt.Errorf("dff: PROP short payload unsafe to skip: %w", err)
				}
				if err := seekPastChunk(f, skip); err != nil {
					return fmt.Errorf("dff: PROP short seek: %w", err)
				}
				continue
			}
			const maxPROPSize = 1 << 20
			if size > maxPROPSize {
				return fmt.Errorf("dff: PROP chunk size %d exceeds %d-byte sanity limit", size, maxPROPSize)
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("dff: PROP body read: %w", err)
			}
			if len(body) < 4 || string(body[0:4]) != "SND " {
				// Body consumed (size bytes), but the odd-size pad
				// byte still belongs to this chunk's footprint and
				// must be consumed before the next iteration reads
				// the next chunk header. Pre-fix the bare `continue`
				// left the pad byte unread on odd-size payloads,
				// shifting every subsequent chunk header by one
				// byte. CodeRabbit Critical post-merge on PR #223.
				if size%2 == 1 {
					if _, err := f.Seek(1, io.SeekCurrent); err != nil {
						return fmt.Errorf("dff: PROP non-SND pad seek: %w", err)
					}
				}
				continue
			}
			if dstCompressed := parsePropChunks(body[4:], t); dstCompressed {
				scanLogger.Warn("dff: DST-compressed DSDIFF not supported; skipping format stamp",
					"path", absPath)
			}
			// Pad byte after odd-size payload — the body slice already
			// consumed `size` bytes, but the file cursor needs a pad
			// advance to stay aligned for the next chunk.
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("dff: PROP pad seek: %w", err)
				}
			}
		case "DIIN":
			// DIIN container body: nested sub-chunks (DITI, DIAR,
			// DIAL, DIGN, COMT, …). 1 MiB cap mirrors PROP — real
			// DIIN bodies are well under 1 KiB.
			const maxDIINSize = 1 << 20
			if size == 0 {
				continue
			}
			if size > maxDIINSize {
				scanLogger.Warn("dff: DIIN chunk size exceeds sanity limit; skipping",
					"path", absPath, "size", size, "limit", maxDIINSize)
				// Guard against uint64 → int64 overflow on a
				// malformed-but-plausible DIIN size: maxDIINSize is
				// 1 MiB so a normal oversize lands well below
				// math.MaxInt64, but a corrupt header declaring
				// `size = 0xFFFFFFFFFFFFFFFF` would convert to -1
				// and seek BACKWARD by one byte. Refuse the
				// conversion rather than re-read the same chunk
				// header in a loop (CodeRabbit Major on PR #223).
				skip, err := safeSeekSkip(size)
				if err != nil {
					return fmt.Errorf("dff: oversized DIIN unsafe to skip: %w", err)
				}
				// safeSeekSkip already applied the odd-byte pad.
				if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
					return fmt.Errorf("dff: seek past oversized DIIN: %w", err)
				}
				continue
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(f, body); err != nil {
				return fmt.Errorf("dff: DIIN body read: %w", err)
			}
			parseDIINChunks(body, t, absPath)
			if size%2 == 1 {
				if _, err := f.Seek(1, io.SeekCurrent); err != nil {
					return fmt.Errorf("dff: DIIN pad seek: %w", err)
				}
			}
		default:
			// Skip the chunk payload + odd-byte pad. Real DSD audio
			// chunks reach single-digit GB for DSD512+ — well below
			// int64 max (~9.2 EiB). But a malformed header declaring
			// `size = 0xFFFFFFFFFFFFFFFF` would convert to int64(-1)
			// and seek BACKWARD by one byte, causing the walker to
			// re-read the same chunk header in an infinite loop.
			// Guard the conversion (CodeRabbit Major on PR #223).
			skip, err := safeSeekSkip(size)
			if err != nil {
				return fmt.Errorf("dff: chunk %q unsafe to skip: %w", fourcc, err)
			}
			// safeSeekSkip already applied the odd-byte pad.
			if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
				return fmt.Errorf("dff: seek past %q: %w", fourcc, err)
			}
			// DSDIFF spec allows DIIN to follow the audio payload, so
			// the walker continues past every unrecognised / non-PROP
			// / non-DIIN chunk (including the DSD audio chunk itself).
			// Natural termination is the EOF branch at the top of the
			// loop.
		}
	}
}

// safeSeekSkip converts a `uint64` chunk size to the even-aligned
// `int64` byte-count `os.File.Seek` requires, applying the IFF/DSDIFF
// odd-byte pad itself and refusing the conversion when the result
// wouldn't fit `int64`. Without this guard, a malformed DSDIFF header
// declaring `size = 0xFFFFFFFFFFFFFFFF` would convert to `int64(-1)`
// and seek BACKWARD by one byte instead of skipping the chunk — the
// next iteration would re-read the same chunk header in an infinite
// loop.
//
// The pad MUST happen here, in `uint64`, BEFORE the `int64` check, and
// the reject MUST use `>=` not `>`: a caller that took `int64(size)`
// first and then did `skip++` would overflow when `size == MaxInt64`
// (odd) — `MaxInt64 + 1` wraps to `MinInt64` and seeks ~9.2 EiB
// backward (the exact bug this rewrite closes). Rejecting `size >=
// MaxInt64` up front means `size + 1` can never overflow `int64` and
// never wraps `uint64` (`MaxUint64 + 1 == 0`, which would otherwise
// pass as a bogus 0-skip). Callers therefore MUST NOT re-pad the
// returned value — it is already even.
//
// Real DSD audio chunks (single-digit GB at DSD512+) sit comfortably
// below `int64` max (~9.2 EiB), so this guard never fires for
// well-formed files. CodeRabbit Major on PR #223.
func safeSeekSkip(size uint64) (int64, error) {
	if size >= math.MaxInt64 {
		return 0, fmt.Errorf("chunk size %d exceeds int64 seek limit", size)
	}
	// size < MaxInt64 here, so size+1 fits int64 and can't wrap uint64.
	if size%2 == 1 {
		size++
	}
	return int64(size), nil
}

// parseDIINChunks walks the body of a DSDIFF DIIN container chunk and
// pulls title (DITI), artist (DIAR), album (DIAL), and genre (DIGN)
// from their respective sub-chunks. Each text sub-chunk's payload is
// a DSDIFF Pascal-string: 1-byte length + N bytes ASCII/UTF-8 text,
// with one pad byte if (1 + N) is odd (the 16-bit chunk alignment
// rule). COMT (Comments) sub-chunks are recognised but skipped — the
// Track struct has no Comment field today, and COMT's structured
// per-comment layout differs from the pstring text chunks.
//
// Bounds: every read is gated on `remaining` — a pstring declaring
// length > remaining is logged + the rest of the sub-chunk skipped,
// matching parsePropChunks's defensive posture. The outer DIIN
// container's size has already been validated against the file
// bounds by the caller.
func parseDIINChunks(body []byte, t *Track, absPath string) {
	for len(body) >= 12 {
		fourcc := string(body[0:4])
		size := be64(body[4:12])
		if size > uint64(len(body)-12) {
			break
		}
		payload := body[12 : 12+size]
		switch fourcc {
		case "DITI":
			if s, ok := readDIINPString(payload, fourcc, absPath); ok && t.Title == "" {
				t.Title = s
			}
		case "DIAR":
			if s, ok := readDIINPString(payload, fourcc, absPath); ok && t.Artist == "" {
				t.Artist = s
			}
		case "DIAL":
			if s, ok := readDIINPString(payload, fourcc, absPath); ok && t.Album == "" {
				t.Album = s
			}
		case "DIGN":
			if s, ok := readDIINPString(payload, fourcc, absPath); ok && t.Genre == "" {
				t.Genre = s
			}
		case "COMT":
			// Comments chunk — structured (2-byte count + per-
			// comment timestamps + text). No Track.Comment surface
			// today, so skip past correctly via the chunk-header
			// size and continue the walk.
		}
		advance := 12 + size
		if advance%2 == 1 {
			advance++
		}
		if advance > uint64(len(body)) {
			break
		}
		body = body[advance:]
	}
}

// readDIINPString parses a DSDIFF text sub-chunk payload as
// (1 byte length, N bytes text). Returns the decoded string and a
// presence bool. A malformed declared length (zero-payload or
// length > available) returns ("", false) so the caller leaves the
// Track field untouched. Logs a warn for the malformed case so
// operators can correlate against the offending file path.
//
// The pad byte after odd `1+length` totals is consumed by the outer
// walker's `advance%2` adjustment in parseDIINChunks — not here, so
// the helper stays focused on the single concern of decoding one
// pstring.
func readDIINPString(payload []byte, fourcc, absPath string) (string, bool) {
	if len(payload) < 1 {
		scanLogger.Warn("dff: DIIN sub-chunk empty payload",
			"path", absPath, "chunk", fourcc)
		return "", false
	}
	length := int(payload[0])
	if length == 0 {
		return "", false
	}
	if length > len(payload)-1 {
		scanLogger.Warn("dff: DIIN sub-chunk pstring overruns declared size",
			"path", absPath, "chunk", fourcc, "length", length, "available", len(payload)-1)
		return "", false
	}
	return string(payload[1 : 1+length]), true
}

// parsePropChunks walks the body of a DSDIFF PROP chunk (after the
// leading "SND " form-type) and pulls FS (sample rate) + CMPR
// (compression). Other property chunks (CHNL, ABSS, LSCO) aren't
// needed for the iOS Track row.
//
// FS values are held in locals during the walk and committed to the
// Track only after CMPR has been confirmed as "DSD " (uncompressed).
// Any other CMPR FOURCC — including "DST " (Direct Stream Transfer)
// AND any future / unknown variant — leaves IsDSD/SampleRate nil so
// iOS classifies the row as unknown audio rather than DSD that
// fails to load. Pre-this-fix, the parser stamped the DSD fields as
// soon as it saw FS and only rolled back for "DST " specifically;
// any other compression code (corrupt encoder, future variant) was
// left as playable uncompressed DFF (Greptile + CodeRabbit on PR #186).
//
// Returns true when CMPR == "DST " so the caller can log a specific
// "DST not supported" message; an unknown CMPR (or absent CMPR)
// returns false but still leaves the DSD fields nil unless the
// compression was confirmed as "DSD ". Chunk-walking errors aren't
// surfaced because PROP is well-bounded by its parent and partial
// reads stay inside the buffer.
func parsePropChunks(body []byte, t *Track) (dstCompressed bool) {
	var (
		haveFS         bool
		fsRate         uint32
		haveCMPR       bool
		isUncompressed bool
	)
	for len(body) >= 12 {
		fourcc := string(body[0:4])
		size := be64(body[4:12])
		// Bound size against the remaining slice — a corrupt header
		// could over-read.
		if size > uint64(len(body)-12) {
			break
		}
		payload := body[12 : 12+size]
		switch fourcc {
		case "FS  ":
			if len(payload) >= 4 {
				fsRate = be32(payload[0:4])
				haveFS = true
			}
		case "CMPR":
			// CMPR payload: 4 bytes compression FOURCC + variable-
			// length compression name. "DSD " = uncompressed,
			// "DST " = DST compressed, anything else = unknown.
			if len(payload) >= 4 {
				haveCMPR = true
				compression := string(payload[0:4])
				switch compression {
				case "DSD ":
					isUncompressed = true
				case "DST ":
					dstCompressed = true
				}
			}
		}
		// Advance past chunk + odd-byte pad. Use uint64 arithmetic
		// against a capped size to avoid signed overflow on a
		// pathological header.
		advance := 12 + size
		if advance%2 == 1 {
			advance++
		}
		if advance > uint64(len(body)) {
			break
		}
		body = body[advance:]
	}
	// Commit DSD stamps only when (a) FS was present, AND (b) CMPR
	// was either absent (DSDIFF spec allows omission, treat as
	// uncompressed) OR explicitly "DSD ". This keeps unknown CMPR
	// FOURCCs (DST and any future variant) out of the playable-DSD
	// classification path.
	if haveFS && fsRate > 0 && (!haveCMPR || isUncompressed) {
		rate := float64(fsRate)
		t.SampleRate = &rate
		isDSD := true
		t.IsDSD = &isDSD
		// `t.Codec` is stamped as "DFF" at the top of
		// extractDFFWithContext, before this function is called —
		// the lossy-codec gate sees the authoritative value (DFF is
		// lossless → gate allows the bits write).
		if canSetBitsPerSample(t.Codec) {
			bits := 1
			t.BitsPerSample = &bits
		}
	}
	return dstCompressed
}

// extractLocalArtwork stamps t.ArtworkMBID with `local-<sha256>` when
// embedded artwork (preferred) or folder-level art (cover.jpg /
// folder.jpg, case-insensitive — JPEG-only) is found. Caller must
// guarantee ec != nil and ec.ArtworkCacheDir != "".
//
// The embedded branch wins on a per-track basis — two tracks in the
// same album with different embedded APIC images each get their own
// hash-keyed cache file. Two tracks with byte-identical embedded
// artwork share one cache file via SHA-256 dedup (desired efficiency,
// not a bug). The folder-level branch is single-flighted so a
// 15-track album with no embedded art does ReadDir + read + hash +
// write exactly once total.
func extractLocalArtwork(absPath string, t *Track, m tag.Metadata, ec *ExtractContext) {
	// 1) Embedded picture, when the dhowden Metadata is available
	//    and carries one. dhowden's Picture() returns nil for ID3v1
	//    (no image support) and for any other format whose parser
	//    didn't surface a picture frame — both safe to skip.
	//
	//    JPEG-only by design (PR #98 follow-up): MIME `image/jpeg`
	//    or `image/jpg` (some taggers emit the variant), matched
	//    case-insensitively (a non-standard `image/JPEG` is still a
	//    JPEG label), AND the bytes must start with the JPEG SOI
	//    marker so an APIC frame that misdeclares its MIME doesn't
	//    smuggle PNG/GIF bytes into a `*-500.jpg` cache file. See
	//    folderArtCandidates and looksLikeJPEG for the matching
	//    contract on the folder-level branch.
	if m != nil {
		if pic := m.Picture(); pic != nil {
			switch {
			case len(pic.Data) > maxArtworkBytes:
				scanLogger.Warn("embedded artwork too large; skipping",
					"path", absPath, "bytes", len(pic.Data), "cap", maxArtworkBytes)
			case len(pic.Data) == 0:
				// nothing to stamp
			case !strings.EqualFold(pic.MIMEType, "image/jpeg") && !strings.EqualFold(pic.MIMEType, "image/jpg"):
				scanLogger.Debug("embedded artwork non-JPEG MIME; skipping",
					"path", absPath, "mime", pic.MIMEType)
			case !looksLikeJPEG(pic.Data):
				scanLogger.Warn("embedded artwork MIME claimed JPEG but bytes are not; skipping",
					"path", absPath, "mime", pic.MIMEType)
			default:
				if mbid, ok := stampLocalArtwork(pic.Data, ec.ArtworkCacheDir); ok {
					t.ArtworkMBID = mbid
					return
				}
			}
		}
	}

	// 2) Folder-level fallback, single-flighted per directory so a
	//    15-track album doesn't ReadDir + hash 15 times.
	dir := filepath.Dir(absPath)
	if ec.FolderArtCache == nil {
		// Defensive: caller should always pass a cache, but a nil
		// cache means we'd race on every track. Skip rather than
		// reintroduce the stampede.
		return
	}
	promiseI, _ := ec.FolderArtCache.LoadOrStore(dir, &folderArtPromise{})
	promise := promiseI.(*folderArtPromise)
	promise.once.Do(func() {
		promise.res = scanFolderArtwork(dir, ec.ArtworkCacheDir)
	})
	if promise.res.found {
		t.ArtworkMBID = promise.res.mbid
	}
}

// folderArtCandidates is the set of filenames the folder-level
// fallback recognises. Compared case-insensitively (Linux
// filesystems are case-sensitive — Windows-tagger output
// `Cover.JPG`, `FOLDER.JPG`, etc. would silently miss a hardcoded
// lowercase os.Stat).
//
// JPEG-only by design (PR #98 follow-up): the cache file path is
// `<dir>/local-<hash>-500.jpg` and the API serves it with
// `Content-Type: image/jpeg`. Mixing PNG bytes into that scheme
// would force either a rename of the path convention OR a
// per-request content-type sniff in the API handler — both
// out-of-scope for V1. Operators with PNG-only artwork can
// re-save as JPEG; PNG support is a follow-up that needs the
// path-scheme + handler changes done together.
var folderArtCandidates = []string{"cover.jpg", "folder.jpg"}

// jpegSOI is the 3-byte JPEG Start-Of-Image marker (FF D8 FF). All
// real JPEGs begin with these three bytes regardless of the APP0 /
// APP1 / EXIF marker that follows; PNG begins with `89 50 4E 47`,
// GIF with `47 49 46 38`. Sniffing the magic guards against ID3
// APIC frames that lie about their MIME type AND folder-level files
// misnamed `cover.jpg` while actually carrying PNG/GIF bytes.
var jpegSOI = []byte{0xFF, 0xD8, 0xFF}

// looksLikeJPEG reports whether data starts with the JPEG SOI
// marker. Used as a defense-in-depth check before stampLocalArtwork
// commits bytes to a `*-500.jpg` cache file — see folderArtCandidates
// for why JPEG-only is the V1 contract.
func looksLikeJPEG(data []byte) bool {
	return len(data) >= len(jpegSOI) && bytes.HasPrefix(data, jpegSOI)
}

// scanFolderArtwork does a single os.ReadDir(dir) and matches entries
// against folderArtCandidates via strings.EqualFold. On hit, reads
// the file (capped at maxArtworkBytes), hashes the bytes, atomically
// writes <cacheDir>/local-<hash>-500.jpg, and returns
// folderArtResult{found: true, mbid: "local-<hash>"}. On miss or any
// I/O error, returns folderArtResult{found: false}.
func scanFolderArtwork(dir, cacheDir string) folderArtResult {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return folderArtResult{}
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		matched := false
		for _, candidate := range folderArtCandidates {
			if strings.EqualFold(name, candidate) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		full := filepath.Join(dir, name)
		// Stat to enforce the size cap before we slurp the file —
		// otherwise a misnamed huge image (someone called their
		// 4K wallpaper cover.jpg) lands in RAM before we reject it.
		// os.Stat (not entry.Info) so a symlinked cover.jpg is measured
		// by its TARGET size: entry.Info() reports the link itself
		// (lstat) — a small size that passes the cap — while os.ReadFile
		// below follows the link and would slurp the full target.
		info, err := os.Stat(full)
		if err != nil {
			scanLogger.Warn("folder-art stat", "path", full, "err", err)
			continue
		}
		if info.Size() > maxArtworkBytes {
			scanLogger.Warn("folder-art too large; skipping",
				"path", full, "bytes", info.Size(), "cap", maxArtworkBytes)
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			scanLogger.Warn("folder-art read", "path", full, "err", err)
			continue
		}
		// Magic-byte sniff: V1 cache scheme is JPEG-only. A user can
		// name a PNG `cover.jpg` and the file will pass extension
		// match — but committing those bytes to a `*-500.jpg` path
		// served as `Content-Type: image/jpeg` would produce a
		// misdeclared response. Skip + warn so the operator can
		// re-save as JPEG.
		if !looksLikeJPEG(data) {
			scanLogger.Warn("folder-art bytes not JPEG; skipping",
				"path", full, "first", fmt.Sprintf("%x", data[:min(len(data), 4)]))
			continue
		}
		if mbid, ok := stampLocalArtwork(data, cacheDir); ok {
			return folderArtResult{found: true, mbid: mbid}
		}
		// stampLocalArtwork already logged the failure; fall through
		// in case the directory has another candidate (rare).
	}
	return folderArtResult{}
}

// stampLocalArtwork hashes data, computes the local-<hash> sentinel,
// and writes <cacheDir>/local-<hash>-500.jpg atomically. If the file
// already exists (idempotent across re-scans / cache-dir wipe
// recovery on the next pass), the write is skipped. Returns
// (mbid, true) on success, ("", false) on any I/O error (logged).
func stampLocalArtwork(data []byte, cacheDir string) (string, bool) {
	sum := sha256.Sum256(data)
	mbid := "local-" + hex.EncodeToString(sum[:])
	path := filepath.Join(cacheDir, mbid+"-500.jpg")
	if _, err := os.Stat(path); err == nil {
		// Already on disk — no-op write, return the mbid so the
		// track gets stamped. Stat-before-write also recovers
		// transparently from a wiped cache-dir on the next scan.
		return mbid, true
	}
	if err := writeArtworkAtomicScan(path, data); err != nil {
		scanLogger.Error("write local artwork", "path", path, "err", err)
		return "", false
	}
	return mbid, true
}

// writeArtworkAtomicScan writes data to path via tmp-file + rename
// so a concurrent reader (the API handler) never sees a torn file.
// Routes through the shared `internal/atomicwrite.WriteBytes`
// helper so the tmp-file + fsync + rename-with-retry +
// byte-equality-fallback contract stays defined in exactly one
// place. The `.scan-*.jpg.tmp` prefix is preserved as the
// diagnostic shape: a stale tmp left on disk after a crash tells
// operators the scanner (not the enricher) was the writer.
func writeArtworkAtomicScan(path string, data []byte) error {
	return atomicwrite.WriteBytes(path, data, ".scan-*.jpg.tmp")
}

// renameFunc + renameWithRetry used to live here. The
// `os.Rename` retry loop now lives in
// internal/atomicwrite.RenameWithRetry; tests that need to inject
// a failing rename swap the package-level seam via
// `atomicwrite.SetRenameFuncForTest`.

// le32/le64 read little-endian integers (DSF's fmt-chunk dialect);
// callers pass slices already bounds-guaranteed to hold the width.
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }

func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// DSDIFF (DFF) chunks use big-endian sizes (IFF/AIFF dialect), unlike
// DSF's little-endian header. Kept narrowly scoped to extractor needs.
func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func be64(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
