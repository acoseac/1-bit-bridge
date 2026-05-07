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
	"time"

	tag "github.com/dhowden/tag"
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
		return extractViaDhowdenFromReader(f, absPath, t, ec)
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
			if comp, ok := stringOf(raw, "TCMP", "CPIL", "COMPILATION"); ok && comp == "1" {
				t.AlbumArtist = "Various Artists"
			}
		}
		if v, ok := stringOf(raw, "MUSICBRAINZ_TRACKID", "MUSICBRAINZ TRACK ID", "musicbrainz_trackid"); ok {
			t.MusicBrainzTrackID = v
		}
		if v, ok := stringOf(raw, "MUSICBRAINZ_ALBUMID", "MUSICBRAINZ ALBUM ID", "musicbrainz_albumid"); ok {
			t.MusicBrainzAlbumID = v
		}
		// ReplayGain.
		if v, ok := stringOf(raw, "REPLAYGAIN_TRACK_GAIN", "replaygain_track_gain"); ok {
			if g := parseReplayGain(v); g != nil {
				t.ReplayGainTrackDB = g
			}
		}
		if v, ok := stringOf(raw, "REPLAYGAIN_ALBUM_GAIN", "replaygain_album_gain"); ok {
			if g := parseReplayGain(v); g != nil {
				t.ReplayGainAlbumDB = g
			}
		}
	}
}

// stringOf looks up keys in a raw tag map. Case-insensitive and
// space/underscore-agnostic so ID3v2 TXXX frames (which dhowden
// surfaces with their human-readable description, e.g.
// "MusicBrainz Album Id") match the same lookup that hits Vorbis
// comments (`MUSICBRAINZ_ALBUMID`). Pre-fix the lookup did exact
// case-sensitive map subscripts, so MBID extraction silently failed
// for any ID3v2-tagged album — Cover-Art-Archive enrichment fell
// back to the lower-quality iTunes path, and the iOS app couldn't
// fall through to bridge-served local-art for those files. Per
// Gemini A6 / iOS bug review #6d.
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
	// Pre-normalise the search keys once so the per-map-key inner
	// loop is just a string compare.
	wanted := make([]string, 0, len(keys))
	for _, k := range keys {
		wanted = append(wanted, normaliseRawTagKey(k))
	}
	for mapKey, v := range raw {
		norm := normaliseRawTagKey(mapKey)
		matched := false
		for _, w := range wanted {
			if norm == w {
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
func parseReplayGain(s string) *float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, " dB")
	s = strings.TrimSuffix(s, " db")
	s = strings.TrimSuffix(s, "dB")
	s = strings.TrimSuffix(s, "db")
	s = strings.TrimSpace(s)
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

	sr := float64(sampleRate)
	bps := bitsPerSample
	t.SampleRate = &sr
	t.BitsPerSample = &bps
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

	sr := float64(sampleRate)
	bps := int(bitsPerSample)
	t.SampleRate = &sr
	t.BitsPerSample = &bps
	isDSD := bitsPerSample == 1
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

// writeArtworkAtomicScan writes data to path via tmp file + rename so
// a concurrent reader (the API handler) never sees a torn file.
// Mirrors enrich.writeArtworkAtomic; the duplication is deliberate —
// extracting to a shared package would add a third internal/* import
// path for a 30-line helper that's only called from two sites. Tmp
// prefix is `.scan-*.jpg.tmp` so a stale temp tells you scanner-side
// (not enricher-side) was the writer.
//
// Cache directory perms are 0o700 (owner-only) — application-owned
// caches shouldn't be world-readable on POSIX. The enricher's mirror
// helper at internal/enrich/enricher.go:writeArtworkAtomic uses the
// same constant; whichever writer creates the dir first wins, and
// upgrades from prior 0o755 deployments are accepted (existing dirs
// stay at their previous mode until a clean install / rmdir).
func writeArtworkAtomicScan(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".scan-*.jpg.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close (LIFO order — runs before Remove). See
	// internal/auth/auth.go for the rationale.
	defer func() { _ = tmp.Close() }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		// Race / AV scan window may have produced a valid destination
		// already. Verify byte-equivalence by reading the existing
		// file and comparing — size alone isn't proof, and a stricter
		// check costs one read of <= maxArtworkBytes on a rare-fallback
		// path (CodeRabbit on PR #100). The scanner-side filename
		// embeds the SHA-256 of the bytes we tried to write so size
		// match + filename match would be sufficient evidence in
		// practice, but the byte-comparison is symmetric with the
		// enricher's mbid-keyed path (where the filename does NOT
		// embed a content hash) and removes the need to reason about
		// caller invariants.
		//
		// Don't clear tmpName here — the rename failed, so the tmp
		// file is still on disk; the defer at line 570 must remove it
		// (otherwise we leak a `.scan-NNN.jpg.tmp` per race/AV-window
		// hit, accumulating over a long uptime).
		if existing, readErr := os.ReadFile(path); readErr == nil && bytes.Equal(existing, data) {
			return nil
		}
		return err
	}
	tmpName = "" // rename succeeded — suppress the deferred os.Remove
	return nil
}

// renameFunc is the rename implementation called by renameWithRetry.
// Wrapped in a var so tests can inject a deterministic failure
// without waiting out the full retry backoff budget. Production
// code MUST NOT mutate this.
var renameFunc = os.Rename

// renameWithRetry retries renameFunc a few times to absorb the
// transient "Access is denied" / sharing-violation that Windows
// produces on the tmp-file-then-rename pattern: Defender / Search
// Indexer scan-on-close briefly hold a handle on freshly-written
// files, and concurrent scanner workers writing the same content
// hash race on the same destination. Caller is responsible for
// post-failure semantics (see writeArtworkAtomicScan's stat-and-
// accept). On Unix the first attempt always succeeds, so the loop
// is a no-op on non-Windows platforms — keeping a single code path
// is simpler than a `_windows.go` build-tagged helper.
//
// Backoff schedule sums to 750 ms; that's the time budget a non-
// transient permission error on the parent directory will burn
// before failing — acceptable for a per-album-once code path.
func renameWithRetry(src, dst string) error {
	backoffs := []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond}
	var err error
	for _, d := range backoffs {
		if d > 0 {
			time.Sleep(d)
		}
		err = renameFunc(src, dst)
		if err == nil {
			return nil
		}
	}
	return err
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}
