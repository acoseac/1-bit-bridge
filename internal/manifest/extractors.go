package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	flac "github.com/mewkiz/flac"

	tag "github.com/dhowden/tag"
)

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
func Extract(absPath string, t *Track) error {
	ext := strings.ToLower(filepath.Ext(absPath))
	switch ext {
	case ".dsf":
		return extractDSF(absPath, t)
	case ".flac":
		if err := extractFLACFormat(absPath, t); err != nil {
			// Format parse failure is fine — tags may still work.
			_ = err
		}
		return extractViaDhowden(absPath, t)
	default:
		return extractViaDhowden(absPath, t)
	}
}

// extractViaDhowden uses github.com/dhowden/tag to read common tag formats.
// Supports ID3v1/2 in MP3, iTunes tags in M4A/MP4, Vorbis comments in FLAC
// and OGG. Unsupported extensions return nil without modifying t — a file
// indexed only by name is still useful, and enrichment (PR #8) can fill in.
func extractViaDhowden(absPath string, t *Track) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := tag.ReadFrom(f)
	if errors.Is(err, tag.ErrNoTagsFound) || err == tag.ErrNoTagsFound {
		return nil
	}
	if err != nil {
		// Corrupt/partial tag — skip but don't fail the whole scan.
		return nil
	}
	populateFromTagMetadata(m, t)
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
	if y := m.Year(); y != 0 {
		t.Year = y
	}
	if tn, _ := m.Track(); tn != 0 {
		t.TrackNumber = tn
	}
	if d, _ := m.Disc(); d != 0 {
		t.DiscNumber = d
	}
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
	t.IsDSD = bitsPerSample == 1
	if sampleRate > 0 && sampleCount > 0 {
		d := float64(sampleCount) / float64(sampleRate)
		t.Duration = &d
	}

	// Tags: ID3v2 at metadataPointer (if non-zero).
	if metadataPointer > 0 {
		if _, err := f.Seek(int64(metadataPointer), io.SeekStart); err != nil {
			return nil // tags optional
		}
		m, err := tag.ReadID3v2Tags(f)
		if err == nil && m != nil {
			populateFromTagMetadata(m, t)
		}
	}
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}
