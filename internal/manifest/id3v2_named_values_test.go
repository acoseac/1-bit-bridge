package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"unicode/utf16"
)

// The values testdata/gen/id3_txxx_fixtures.py tags the files in testdata/id3
// with.
const (
	picardAlbumMBID        = "4b8a2d4c-1c7a-4a9e-9d6f-3c2b1a0f9e8d"
	picardRecordingMBID    = "2a4c6e8f-1b3d-4f5a-8c7e-9d0b1a2c3e4f"
	picardReleaseTrackMBID = "7f3e2d1c-0b9a-4876-9543-210fedcba987"
	picardTrackGainDB      = -6.48
	picardAlbumGainDB      = -7.25
)

// id3Fixture reads one of the tagged files in testdata/id3.
func id3Fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "id3", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// id3Text is s as an ID3v2 frame holds it in encoding enc: ISO-8859-1 (0),
// UTF-16 with a byte order mark (1, little endian, as mutagen writes it) or
// UTF-8 (3), followed by the encoding's terminator.
func id3Text(enc byte, s string) []byte {
	switch enc {
	case 1:
		out := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			out = binary.LittleEndian.AppendUint16(out, u)
		}
		return append(out, 0, 0)
	case 0:
		out := make([]byte, 0, len(s)+1)
		for _, r := range s {
			out = append(out, byte(r))
		}
		return append(out, 0)
	default:
		return append([]byte(s), 0)
	}
}

// txxxFrame is a user text frame (TXXX, or TXX in version 2) named desc,
// holding values, each terminated as mutagen writes them.
func txxxFrame(version, enc byte, desc string, values ...string) []byte {
	p := append([]byte{enc}, id3Text(enc, desc)...)
	for _, v := range values {
		p = append(p, id3Text(enc, v)...)
	}
	id := "TXXX"
	if version == 2 {
		id = "TXX"
	}
	return id3v2FrameBytes(version, id, uint32(len(p)), 0, p)
}

// ufidFrame is a unique file identifier frame (UFID, or UFI in version 2):
// the owner, a terminator and the identifier's bytes, as Picard writes the
// recording id.
func ufidFrame(version byte, owner, identifier string) []byte {
	p := append(append([]byte(owner), 0), identifier...)
	id := "UFID"
	if version == 2 {
		id = "UFI"
	}
	return id3v2FrameBytes(version, id, uint32(len(p)), 0, p)
}

// extractID3Frames extracts an MP3 whose ID3v2 tag of version holds frames.
func extractID3Frames(t testing.TB, version byte, frames ...[]byte) Track {
	t.Helper()
	data := append(id3v2TagBytes(version, 0, bytes.Join(frames, nil)), mp3Audio()...)
	return requireBoundedExtraction(t, "x.mp3", data, &ExtractContext{})
}

// gainString is a ReplayGain field as a test compares it: "nil" or the value.
func gainString(g *float64) string {
	if g == nil {
		return "nil"
	}
	return strconv.FormatFloat(*g, 'f', -1, 64)
}

// TestPicardsID3v2IdsAndReplayGainReachTheirFields is the regression test for
// backlog B116, over real files: MusicBrainz Picard's tag (written through
// mutagen, as Picard writes it) on an MP3 in each of Picard's three encodings
// and on a DSF, an AIFF and a WAV, and ffmpeg's. Picard files the release id and
// ReplayGain in TXXX frames and the recording id in a UFID frame, and dhowden
// stores those under TXXX, TXXX_0, … and UFID, holding a *tag.Comm and a
// *tag.UFID, which no lookup by key reached: on main every one of these fields
// came out empty. Picard's TXXX "MusicBrainz Release Track Id" names the
// release's TRACK, not the recording, and must not fill MusicBrainzTrackID.
func TestPicardsID3v2IdsAndReplayGainReachTheirFields(t *testing.T) {
	for _, c := range []struct {
		file      string
		recording string
	}{
		{"picard_v24_utf16.mp3", picardRecordingMBID},
		{"picard_v23_utf16.mp3", picardRecordingMBID},
		{"picard_v24_utf8.mp3", picardRecordingMBID},
		{"picard_v24_utf16.dsf", picardRecordingMBID},
		{"picard_v24_utf16.aiff", picardRecordingMBID},
		{"picard_v24_utf16.wav", picardRecordingMBID},
		// ffmpeg writes no UFID; its ReplayGain names are lower case.
		{"ffmpeg_v23.mp3", ""},
	} {
		t.Run(c.file, func(t *testing.T) {
			tr := requireBoundedExtraction(t, c.file, id3Fixture(t, c.file), &ExtractContext{})
			if tr.Title != "Bohemian Rhapsody" {
				t.Fatalf("premise: Title = %q, want the tag's", tr.Title)
			}
			if tr.MusicBrainzAlbumID != picardAlbumMBID {
				t.Errorf("MusicBrainzAlbumID = %q, want %q (TXXX \"MusicBrainz Album Id\")", tr.MusicBrainzAlbumID, picardAlbumMBID)
			}
			if tr.MusicBrainzTrackID != c.recording {
				t.Errorf("MusicBrainzTrackID = %q, want %q (the recording id, from the MusicBrainz UFID; %q is the release track's)",
					tr.MusicBrainzTrackID, c.recording, picardReleaseTrackMBID)
			}
			if tr.ReplayGainTrackDB == nil || *tr.ReplayGainTrackDB != picardTrackGainDB {
				t.Errorf("ReplayGainTrackDB = %s, want %v", gainString(tr.ReplayGainTrackDB), picardTrackGainDB)
			}
			if tr.ReplayGainAlbumDB == nil || *tr.ReplayGainAlbumDB != picardAlbumGainDB {
				t.Errorf("ReplayGainAlbumDB = %s, want %v", gainString(tr.ReplayGainAlbumDB), picardAlbumGainDB)
			}
		})
	}
}

// TestAnID3v2NameAnswersAsAVorbisOrMP4NameDoes pins how a TXXX description
// and the MusicBrainz UFID are read: under the aliases a Vorbis comment and an
// MP4 freeform atom are, normalised the same way, with the same precedence.
// Each case runs twenty times, because dhowden's raw map is a Go map and a
// winner picked by ranging it changes from one extraction to the next.
func TestAnID3v2NameAnswersAsAVorbisOrMP4NameDoes(t *testing.T) {
	const a, b = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const mb = "http://musicbrainz.org"
	bom := string(rune(0xFEFF)) // a byte order mark as dhowden leaves one in a later UTF-16 value
	for _, c := range []struct {
		name      string
		version   byte
		frames    [][]byte
		album     string
		recording string
		trackGain string
	}{
		{
			name:    "the first alias answers wherever its frame sits",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "MusicBrainz Album Id", a),
				txxxFrame(4, 3, "MUSICBRAINZ_ALBUMID", b),
			},
			album: b,
		},
		{
			name:    "the first alias answers wherever its frame sits, the other way round",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "MUSICBRAINZ_ALBUMID", b),
				txxxFrame(4, 3, "MusicBrainz Album Id", a),
			},
			album: b,
		},
		{
			name:    "one name twice: the first frame in the tag",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "replaygain_track_gain", "-1.00 dB"),
				txxxFrame(4, 3, "REPLAYGAIN_TRACK_GAIN", "-2.00 dB"),
			},
			trackGain: "-1",
		},
		{
			name:    "one name twice: the first frame in the tag, the other way round",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "REPLAYGAIN_TRACK_GAIN", "-2.00 dB"),
				txxxFrame(4, 3, "replaygain_track_gain", "-1.00 dB"),
			},
			trackGain: "-2",
		},
		{
			name:    "a frame the tag leaves empty is no answer",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "MusicBrainz Album Id", ""),
				txxxFrame(4, 1, "MusicBrainz Album Id", " "+bom+" "),
				txxxFrame(4, 3, "MusicBrainz Album Id", a),
			},
			album: a,
		},
		{
			name:      "the first of several values, past an empty one and a byte order mark",
			version:   4,
			frames:    [][]byte{txxxFrame(4, 1, "MusicBrainz Album Id", "", a, b)},
			album:     a,
			recording: "",
		},
		{
			name:    "the MusicBrainz UFID is the recording id, ahead of a TXXX of the same name",
			version: 4,
			frames: [][]byte{
				txxxFrame(4, 3, "MUSICBRAINZ_TRACKID", b),
				ufidFrame(4, mb, a),
			},
			recording: a,
		},
		{
			name:      "a TXXX named as an MP4 freeform atom names the recording id",
			version:   3,
			frames:    [][]byte{txxxFrame(3, 1, "MusicBrainz Track Id", a)},
			recording: a,
		},
		{
			name:    "the release track id is not the recording id",
			version: 4,
			frames:  [][]byte{txxxFrame(4, 3, "MusicBrainz Release Track Id", a)},
		},
		{
			name:    "another owner's UFID is not a MusicBrainz id",
			version: 4,
			frames:  [][]byte{ufidFrame(4, "http://www.cddb.com/id3/taginfo1.html", a)},
		},
		{
			name:    "version 2.2's TXX and UFI",
			version: 2,
			frames: [][]byte{
				txxxFrame(2, 0, "MusicBrainz Album Id", a),
				ufidFrame(2, mb, b),
				txxxFrame(2, 0, "REPLAYGAIN_TRACK_GAIN", "+1.50 dB"),
			},
			album:     a,
			recording: b,
			trackGain: "1.5",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for range 20 {
				tr := extractID3Frames(t, c.version, c.frames...)
				if tr.MusicBrainzAlbumID != c.album {
					t.Fatalf("MusicBrainzAlbumID = %q, want %q", tr.MusicBrainzAlbumID, c.album)
				}
				if tr.MusicBrainzTrackID != c.recording {
					t.Fatalf("MusicBrainzTrackID = %q, want %q", tr.MusicBrainzTrackID, c.recording)
				}
				want := c.trackGain
				if want == "" {
					want = "nil"
				}
				if got := gainString(tr.ReplayGainTrackDB); got != want {
					t.Fatalf("ReplayGainTrackDB = %s, want %s", got, want)
				}
			}
		})
	}
}

// TestAFieldID3v2GivesAFrameOfItsOwnIsNotReadFromATXXX pins the scope of the
// named lookup: it serves the fields whose ID3v2 home is a TXXX or UFID frame
// (the MusicBrainz ids and ReplayGain), which are the ones the app's
// ID3v2Parser reads there too. A field ID3v2 gives a frame of its own is read
// from that frame alone, on both sides: the compilation flag (TCMP; the app's
// test_TXXXCompilationAndV22TCP_areNotRead pins that neither side reads
// TXXX:COMPILATION, since a flagged file with no album artist is keyed into
// "Various Artists" on both), the composer (TCOM), the conductor (TPE3), the
// work (TIT1), the original year (TDOR / TORY) and the tempo (TBPM).
func TestAFieldID3v2GivesAFrameOfItsOwnIsNotReadFromATXXX(t *testing.T) {
	tr := extractID3Frames(t, 4,
		textFrameBytes(4, "TIT2", "t"),
		txxxFrame(4, 3, "COMPILATION", "1"),
		txxxFrame(4, 3, "COMPOSER", "c"),
		txxxFrame(4, 3, "CONDUCTOR", "d"),
		txxxFrame(4, 3, "WORK", "w"),
		txxxFrame(4, 3, "originalyear", "1975"),
		txxxFrame(4, 3, "BPM", "120"),
		txxxFrame(4, 3, "MusicBrainz Album Id", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
	)
	if tr.Title != "t" || tr.MusicBrainzAlbumID == "" {
		t.Fatalf("premise: the tag was read and its named album id with it; got Title %q, MusicBrainzAlbumID %q",
			tr.Title, tr.MusicBrainzAlbumID)
	}
	if tr.Compilation || tr.AlbumArtist != "" {
		t.Errorf("TXXX:COMPILATION was read: Compilation %v, AlbumArtist %q", tr.Compilation, tr.AlbumArtist)
	}
	if tr.Composer != "" || tr.Conductor != "" || tr.Work != "" {
		t.Errorf("a TXXX filled a field ID3v2 has a frame for: Composer %q, Conductor %q, Work %q",
			tr.Composer, tr.Conductor, tr.Work)
	}
	if tr.OriginalYear != nil {
		t.Errorf("a TXXX filled OriginalYear: %d", *tr.OriginalYear)
	}
	if tr.BPM != nil {
		t.Errorf("a TXXX filled BPM: %d", *tr.BPM)
	}
}

// TestScanner_V19_ATagNamingItsIDsJoinsTheDelta_APlainID3RowOnlyStamps is the
// v19 upgrade end to end. Rows a v18 bridge indexed (a stale stamp, and no
// MusicBrainz ids or ReplayGain from the ID3 tag) re-extract once. The row
// whose tag carries them gains them, the tag's release id replacing one the
// enricher found by searching (the re-extract's value wins the diff-guard's
// merge), its indexed_at advances and its enriched_at goes back to 0, so the
// enricher runs again with the file's own release id. The row whose tag names
// nothing re-extracts byte-identical and is only stamped: no delta, no
// re-enrichment.
func TestScanner_V19_ATagNamingItsIDsJoinsTheDelta_APlainID3RowOnlyStamps(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "picard.mp3"), id3Fixture(t, "picard_v24_utf16.mp3"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalMP3(t, filepath.Join(root, "plain.mp3"), map[string]string{
		"title": "Plain", "artist": "Band", "album": "Own Album", "track": "1",
	})
	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	fresh, err := store.GetTrack(ctx, "picard.mp3")
	if err != nil || fresh == nil || fresh.MusicBrainzAlbumID != picardAlbumMBID {
		t.Fatalf("premise: the current extractor stores the tag's release id; got %+v err=%v", fresh, err)
	}

	// What a v18 bridge left behind: stamped 18, the tag's ids and gains
	// never read, a release id the enricher found by searching, enriched.
	const searched = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = 18, enriched_at = 1,
		tags_json = json_set(json_remove(tags_json, '$.musicBrainzTrackID', '$.replayGainTrackDB', '$.replayGainAlbumDB'),
		                     '$.musicBrainzAlbumID', ?, '$.artworkMBID', ?)
		WHERE path = ?`, searched, searched, "picard.mp3"); err != nil {
		t.Fatalf("munge picard: %v", err)
	}
	if _, err := store.db.Exec("UPDATE tracks SET extractor_version = 18, enriched_at = 1 WHERE path = ?", "plain.mp3"); err != nil {
		t.Fatalf("munge plain: %v", err)
	}
	beforePicard := trackIndexedAt(t, store, "picard.mp3")
	beforePlain := trackIndexedAt(t, store, "plain.mp3")

	scanOnce(t, sc, "v19")

	got, err := store.GetTrack(ctx, "picard.mp3")
	if err != nil || got == nil {
		t.Fatalf("GetTrack(picard): err=%v nil=%v", err, got == nil)
	}
	if got.MusicBrainzAlbumID != picardAlbumMBID || got.MusicBrainzTrackID != picardRecordingMBID {
		t.Errorf("ids after the re-extract: album %q, recording %q; want the tag's %q, %q",
			got.MusicBrainzAlbumID, got.MusicBrainzTrackID, picardAlbumMBID, picardRecordingMBID)
	}
	if got.ReplayGainTrackDB == nil || got.ReplayGainAlbumDB == nil {
		t.Errorf("ReplayGain after the re-extract: track %s, album %s", gainString(got.ReplayGainTrackDB), gainString(got.ReplayGainAlbumDB))
	}
	if got.ArtworkMBID != searched {
		t.Errorf("ArtworkMBID = %q: the merge keeps the enricher's cover until it runs again (want %q)", got.ArtworkMBID, searched)
	}
	if after := trackIndexedAt(t, store, "picard.mp3"); after <= beforePicard {
		t.Errorf("the tagged row's indexed_at did not advance (%d -> %d): iOS would never pull its ids", beforePicard, after)
	}
	if v := trackColumn(t, store, "picard.mp3", "enriched_at"); v != 0 {
		t.Errorf("the tagged row's enriched_at = %d: the enricher would not run again with the file's own release id", v)
	}
	if after := trackIndexedAt(t, store, "plain.mp3"); after != beforePlain {
		t.Errorf("the plain row's indexed_at moved (%d -> %d): v19 must not put every ID3 row in the delta", beforePlain, after)
	}
	if v := trackColumn(t, store, "plain.mp3", "enriched_at"); v != 1 {
		t.Errorf("the plain row's enriched_at = %d: v19 must not re-enrich a row whose tag names nothing", v)
	}
	for _, rel := range []string{"picard.mp3", "plain.mp3"} {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}
