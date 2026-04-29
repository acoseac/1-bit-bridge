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

	flac "github.com/mewkiz/flac"

	tag "github.com/dhowden/tag"
)

// maxArtworkBytes caps the per-image bytes the local-artwork extractor
// will hash + cache. Embedded ID3 APIC pictures larger than ~5 MB are
// nearly always misuse (lossless TIFFs in tags); 10 MB is a generous
// upper bound that protects RAM under the parallel-worker model
// (`runtime.NumCPU()` workers each hold at most one buffer this size).
// Overrun is logged + skipped; the track still gets indexed without an
// ArtworkMBID (the enricher's MusicBrainz path remains as fallback).
const maxArtworkBytes = 10 * 1024 * 1024 // 10 MiB

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
		if err := extractFLACFormat(absPath, t); err != nil {
			// Format parse failure is fine — tags may still work.
			_ = err
		}
		return extractViaDhowdenWithContext(absPath, t, ec)
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
// ExtractWithContext. After populating tag fields it hands the
// dhowden Metadata (which holds the embedded APIC bytes via its
// Picture() accessor) to extractLocalArtwork so embedded art and the
// folder.jpg fallback both run from the same code path.
func extractViaDhowdenWithContext(absPath string, t *Track, ec *ExtractContext) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
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
func populateFromTagMetadata(m tag.Metadata, t *Track) {
	if v := m.Title(); v != "" {
		t.Title = v
	}
	if v := m.Artist(); v != "" {
		t.Artist = v
	}
	if v := m.AlbumArtist(); v != "" {
		t.AlbumArtist = v
	}
	if v := m.Album(); v != "" {
		t.Album = v
	}
	if v := m.Genre(); v != "" {
		t.Genre = v
	}
	// `dhowden/tag` returns 0 for both "tag absent" and "tag value is 0"
	// — there's no way to distinguish at this layer. We propagate the
	// raw value as a non-nil pointer regardless, so a track legitimately
	// tagged with year 0 / track 0 round-trips as `Some(0)` to the
	// iOS decoder rather than getting silently dropped. iOS treats 0
	// as the same sentinel as nil for these fields (no track number,
	// no disc number, no year), so the user-visible behaviour is
	// unchanged; the wire shape just stops lying about which case the
	// extractor saw.
	y := m.Year()
	t.Year = &y
	tn, _ := m.Track()
	t.TrackNumber = &tn
	d, _ := m.Disc()
	t.DiscNumber = &d
	// MusicBrainz IDs — many tagged libraries carry these.
	if raw := m.Raw(); raw != nil {
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

// stringOf looks up keys in a raw tag map (case-and-spelling varies across
// formats) and returns the first non-empty string value.
func stringOf(raw map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := raw[k]; ok {
			switch s := v.(type) {
			case string:
				if s != "" {
					return s, true
				}
			case []string:
				if len(s) > 0 && s[0] != "" {
					return s[0], true
				}
			}
		}
	}
	return "", false
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

// extractFLACFormat reads STREAMINFO from a FLAC file and fills sample rate,
// bit depth, and duration. Duration is derived from total samples /
// sample rate, which is exact for FLAC.
func extractFLACFormat(absPath string, t *Track) error {
	stream, err := flac.ParseFile(absPath)
	if err != nil {
		return err
	}
	defer stream.Close()
	si := stream.Info
	if si == nil {
		return fmt.Errorf("flac: no STREAMINFO in %q", absPath)
	}
	sr := float64(si.SampleRate)
	bps := int(si.BitsPerSample)
	t.SampleRate = &sr
	t.BitsPerSample = &bps
	// FLAC is always PCM by spec — set the explicit false so the
	// iOS decoder can trust `isDSD: false` to mean "definitely PCM"
	// rather than "format unknown". Mirrors the explicit `true` set
	// in `extractDSF` below.
	isDSD := false
	t.IsDSD = &isDSD
	if si.SampleRate > 0 && si.NSamples > 0 {
		d := float64(si.NSamples) / float64(si.SampleRate)
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}
