package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
	for _, lossless := range []string{"FLAC", "ALAC", "DSF", "DFF", "WAV", "AIFF"} {
		if strings.EqualFold(codec, lossless) {
			return true
		}
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
var Ext = map[string]bool{
	".flac": true,
	".dsf":  true,
	".dff":  true, // DSDIFF — rarer but found in audiophile libraries
	".mp3":  true,
	".m4a":  true, // AAC / ALAC
	".mp4":  true, // lossy audio inside MP4 container, uncommon but valid
	".ogg":  true,
	".wav":  true,
	".aif":  true,
	".aiff": true,
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

// ExtractWithContext is the context-aware variant of Extract. When ec
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
func ExtractWithContext(absPath string, t *Track, ec *ExtractContext) error {
	ext := strings.ToLower(filepath.Ext(absPath))
	switch ext {
	case ".dsf":
		return extractDSFWithContext(absPath, t, ec)
	case ".dff":
		return extractDFFWithContext(absPath, t, ec)
	case ".m4a", ".mp4", ".m4b", ".m4p":
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
		// Seek to head before handing the reader to dhowden/tag —
		// extractMP4Codec consumed bytes during the atom walk.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extractViaDhowdenFromReader(f, absPath, t, ec)
	case ".mp3":
		// MP3 is unambiguous; stamp the codec at the path level so
		// the iOS `Track.codec` filter matches "MP3" without
		// fallback. dhowden's `tag.FileType()` IS reliable for MP3
		// (returns `tag.MP3`), but we set it directly here to avoid
		// an extra step.
		t.Codec = "MP3"
		return extractViaDhowdenWithContext(absPath, t, ec)
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
			// Format parse failure is fine — tags may still work.
			_ = err
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
	// `dhowden/tag` returns 0 for both "tag absent" and "tag value is 0"
	// — there's no way to distinguish at this layer. We propagate the
	// raw value as a non-nil pointer regardless, so a track legitimately
	// tagged with year 0 / track 0 round-trips as `Some(0)` to the
	// iOS decoder rather than getting silently dropped.
	//
	// **Pointer-zero correctness pass** (the bridge-side companion
	// to the iOS-side `MetadataNormalizer.albumID` year-zero guard)
	// is deferred to a later release: shipping the bridge fix
	// before the iOS guard has propagated via App Store / TestFlight
	// would have legacy clients suddenly see `null` where they
	// expected `0` and trigger mass library re-grouping. See
	// `internal/manifest/types.go` doc-comment.
	y := m.Year()
	t.Year = &y
	tn, _ := m.Track()
	t.TrackNumber = &tn
	d, _ := m.Disc()
	t.DiscNumber = &d
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
	}
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
// `Skip()` method — which uses `Seek` on `io.Seeker` inputs and
// falls back to `io.Copy(io.Discard, ...)` otherwise.
//
// Best-effort: malformed / corrupt FLAC streams silently no-op
// rather than failing the scan. Single-value tags also no-op (the
// dhowden-populated value is correct in that case). Non-FLAC
// formats are NOT covered by this helper — ID3v2 TPE1 multi-value
// (MP3 / DSF / M4A) is a deferred follow-up.
func applyFLACMultiValueArtists(r io.Reader, t *Track) {
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
		// skip the body without reading it. For `*os.File` the
		// underlying `Skip()` uses `Seek` and never touches the
		// payload bytes.
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
	for mapKey, v := range raw {
		norm := normaliseRawTagKey(mapKey)
		matched := false
		for _, k := range keys {
			if norm == k {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
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
	return &v
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
func extractFLACFormatFromReader(r io.Reader, absPath string, t *Track) error {
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

	// Walk top-level chunks looking for PROP. Each chunk: 4 bytes
	// FOURCC, 8 bytes BE size, payload, then a single pad byte if
	// size is odd (DSDIFF / IFF chunk-pad rule). The DSD audio chunk
	// holds the actual samples — for high-resolution DSD256+ stereo
	// this routinely exceeds 1 GiB. We don't allocate for it (we
	// just Seek past), so no sanity limit is needed at the outer
	// walk; only the PROP body allocation has a size cap.
	for {
		var chunkHeader [12]byte
		if _, err := io.ReadFull(f, chunkHeader[:]); err != nil {
			// EOF before we hit PROP — DFF malformed but not a hard
			// scan-aborting error. Codec already stamped.
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("dff: chunk header read: %w", err)
		}
		fourcc := string(chunkHeader[0:4])
		size := be64(chunkHeader[4:12])
		if fourcc == "PROP" {
			// PROP body: 4 bytes form type ("SND "), then nested chunks.
			// 1 MiB cap on the body allocation — real PROP chunks hold
			// a handful of small property chunks (FS, CHNL, CMPR, ABSS,
			// LSCO) totalling well under 1 KiB; a multi-MiB declared
			// PROP is a corruption signal, not a high-res DSD payload.
			if size < 4 {
				return nil
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
				return nil
			}
			if dstCompressed := parsePropChunks(body[4:], t); dstCompressed {
				scanLogger.Warn("dff: DST-compressed DSDIFF not supported; skipping format stamp",
					"path", absPath)
			}
			// `extractLocalArtwork` mirrors the DSF path — even though
			// DFF doesn't carry embedded artwork in a parsed-here
			// chunk, the folder-level `cover.jpg` / `folder.jpg`
			// fallback should still fire for DFF albums. Pass `nil`
			// for the metadata since we don't decode embedded
			// pictures from DFF (DIIN parsing deferred). Caller
			// matches DSF's ec gating.
			if ec != nil && ec.ArtworkCacheDir != "" {
				extractLocalArtwork(absPath, t, nil, ec)
			}
			return nil
		}
		// Skip the chunk payload + odd-byte pad. Use int64 directly —
		// we don't allocate, so a multi-GiB DSD audio chunk passes
		// through unchallenged. `int64(size)` is safe because os.File.Seek
		// takes int64 and Linux/macOS file sizes max at int64.
		skip := int64(size)
		if skip%2 == 1 {
			skip++
		}
		if _, err := f.Seek(skip, io.SeekCurrent); err != nil {
			return fmt.Errorf("dff: seek past %q: %w", fourcc, err)
		}
		// PROP must precede the DSD audio chunk per the spec — once we
		// hit DSD without seeing PROP we won't find it later. Stop.
		if fourcc == "DSD " {
			return nil
		}
	}
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
	//    or `image/jpg` (some taggers emit the variant), AND the
	//    bytes must start with the JPEG SOI marker so an APIC frame
	//    that misdeclares its MIME doesn't smuggle PNG/GIF bytes
	//    into a `*-500.jpg` cache file. See folderArtCandidates and
	//    looksLikeJPEG for the matching contract on the folder-level
	//    branch.
	if m != nil {
		if pic := m.Picture(); pic != nil {
			switch {
			case len(pic.Data) > maxArtworkBytes:
				scanLogger.Warn("embedded artwork too large; skipping",
					"path", absPath, "bytes", len(pic.Data), "cap", maxArtworkBytes)
			case len(pic.Data) == 0:
				// nothing to stamp
			case pic.MIMEType != "image/jpeg" && pic.MIMEType != "image/jpg":
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
		info, err := entry.Info()
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
				"path", full, "first", fmt.Sprintf("%x", data[:min4(len(data))]))
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

// min4 clamps to 4 to keep the warn-log preview short and avoid an
// out-of-range slice on the rare 0-3-byte garbage file.
func min4(n int) int {
	if n < 4 {
		return n
	}
	return 4
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

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

// DSDIFF (DFF) chunks use big-endian sizes (IFF/AIFF dialect), unlike
// DSF's little-endian header. Kept narrowly scoped to extractor needs.
func be32(b []byte) uint32 {
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16 | uint32(b[0])<<24
}

func be64(b []byte) uint64 {
	return uint64(b[7]) | uint64(b[6])<<8 | uint64(b[5])<<16 | uint64(b[4])<<24 |
		uint64(b[3])<<32 | uint64(b[2])<<40 | uint64(b[1])<<48 | uint64(b[0])<<56
}
