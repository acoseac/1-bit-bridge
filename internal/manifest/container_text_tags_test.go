package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the tags a container keeps in chunks of its own (a DSDIFF file's
// DIIN and "ID3 " chunks, a RIFF/WAVE file's LIST/INFO) against the path's
// guess the scanner fills before it extracts (backlog B140, ExtractorVersion
// 20).

// dffFixture reads one of the tagged DSDIFF files in testdata/dff, written by
// TagLib 2 and mutagen (testdata/gen/dff_tag_fixtures.sh).
func dffFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dff", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dffTags is the text a track carries, and its track number and year (0 for
// none).
type dffTags struct {
	title, artist, album, genre string
	track, year                 int
}

// dffFixtures is each fixture and what its writers put in it, as TagLib 2.0.2
// reads it back: its DSDIFF tag answers from the ID3 chunk first and from the
// DIIN where that has nothing, in either chunk order. A DIIN holds a title and
// an artist and nothing else.
var dffFixtures = []struct {
	name string
	want dffTags
}{
	{"taglib_diin.dff", dffTags{title: "Prélude à la nuit, première", artist: "Ensemble DIIN"}},
	{"picard_id3.dff", dffTags{title: "Picard Title", artist: "Picard Artist", album: "Picard Album",
		genre: "Picard Genre", track: 3, year: 2019}},
	{"diin_then_id3.dff", dffTags{title: "ID3 Title", artist: "DIIN Artist", album: "ID3 Album"}},
	{"id3_then_diin.dff", dffTags{title: "ID3 Title", artist: "DIIN Artist", album: "ID3 Album"}},
}

// requireTags fails unless tr carries want.
func requireTags(t *testing.T, tr *Track, want dffTags) {
	t.Helper()
	got := dffTags{title: tr.Title, artist: tr.Artist, album: tr.Album, genre: tr.Genre}
	if tr.TrackNumber != nil {
		got.track = *tr.TrackNumber
	}
	if tr.Year != nil {
		got.year = *tr.Year
	}
	if got != want {
		t.Errorf("the track carries %+v, want %+v", got, want)
	}
}

// dffWithChunks is a minimal DSDIFF file (FRM8/DSD: PROP/SND holding FS at
// rate, two channels and CMPR "DSD ", then a 4-byte DSD chunk) followed by
// chunks, the FRM8 size counting them.
func dffWithChunks(t testing.TB, rate uint32, chunks ...[]byte) []byte {
	t.Helper()
	fs := make([]byte, 4)
	binary.BigEndian.PutUint32(fs, rate)
	prop := append([]byte("SND "), dffChunk("FS  ", fs)...)
	prop = append(prop, dffChunk("CHNL", []byte("\x00\x02SLFTSRGT"))...)
	prop = append(prop, dffChunk("CMPR", []byte("DSD \x00"))...) // an empty compression name
	form := append([]byte("DSD "), dffChunk("PROP", prop)...)
	form = append(form, dffChunk("DSD ", []byte{0x69, 0x69, 0x69, 0x69})...)
	for _, c := range chunks {
		form = append(form, c...)
	}
	out := append([]byte("FRM8"), make([]byte, 8)...)
	binary.BigEndian.PutUint64(out[4:12], uint64(len(form)))
	return append(out, form...)
}

// riffChunk is one RIFF chunk: its id, a 4-byte little-endian size, the body,
// and a pad byte when the size is odd.
func riffChunk(id string, body []byte) []byte {
	out := append([]byte(id), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// wavWithChunks is a minimal RIFF/WAVE file (a 16-byte PCM fmt chunk, two
// channels at 44.1 kHz and 16 bits, and 4 bytes of data) followed by chunks.
func wavWithChunks(chunks ...[]byte) []byte {
	form := append([]byte("WAVE"),
		riffChunk("fmt ", []byte("\x01\x00\x02\x00\x44\xAC\x00\x00\x10\xB1\x02\x00\x04\x00\x10\x00"))...)
	form = append(form, riffChunk("data", []byte{0, 0, 0, 0})...)
	for _, c := range chunks {
		form = append(form, c...)
	}
	out := append([]byte("RIFF"), make([]byte, 4)...)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(form)))
	return append(out, form...)
}

// listInfo is a LIST/INFO chunk holding each id and value of pairs, in order,
// as NUL-terminated text.
func listInfo(pairs ...string) []byte {
	body := []byte("INFO")
	for i := 0; i+1 < len(pairs); i += 2 {
		body = append(body, riffChunk(pairs[i], append([]byte(pairs[i+1]), 0))...)
	}
	return riffChunk("LIST", body)
}

// TestDFFReadsTheTagsItsWritersWrite is the extractor half of backlog B140:
// the tags real writers put in a DSDIFF file reach the track. TagLib writes a
// DIIN chunk in the DSDIFF 1.5 layout (a 4-byte count before each text, the
// text in ISO-8859-1), which the bridge read as a 1-byte length until
// ExtractorVersion 20 and so read nothing from; mutagen (the library Picard
// writes with) appends an "ID3 " chunk, which the bridge did not read at all.
// Where a file holds both, the ID3 tag answers each field it has a value for
// and the DIIN the rest, in either chunk order, as TagLib reads them. The
// format the walk gathers is stamped beside them.
func TestDFFReadsTheTagsItsWritersWrite(t *testing.T) {
	for _, fx := range dffFixtures {
		t.Run(fx.name, func(t *testing.T) {
			path := writeTempDFF(t, dffFixture(t, fx.name))
			var tr Track
			if err := ExtractWithContext(path, &tr, nil); err != nil {
				t.Fatalf("extract: %v", err)
			}
			requireTags(t, &tr, fx.want)
			if tr.SampleRate == nil || *tr.SampleRate != 2822400 || tr.IsDSD == nil || !*tr.IsDSD {
				t.Errorf("the format is not stamped: SampleRate %v, IsDSD %v", tr.SampleRate, tr.IsDSD)
			}
		})
	}
}

// storedTrack is the row the store holds at rel.
func storedTrack(t *testing.T, store *Store, rel string) *Track {
	t.Helper()
	tr, err := store.GetTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("%s: no row (%v)", rel, err)
	}
	return tr
}

// TestScanner_ADFFsOwnTagsOutrankThePathsGuess is the defect backlog B140
// names, through a real scan. The scanner fills a track's title, album and
// artist from its path (fillFromPath) before it extracts, and the DIIN walk
// filled only an empty field, so a scanned DFF kept its file name as its title
// whatever its DIIN said (and its ID3 chunk was never read). Its own tags now
// replace each guess they have a value for; the guesses stay for the rest, and
// for a DFF with no tags.
func TestScanner_ADFFsOwnTagsOutrankThePathsGuess(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Guess Artist", "Guess Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, fx := range dffFixtures {
		writeFixtureBytes(t, filepath.Join(album, fx.name), dffFixture(t, fx.name))
	}
	writeFixtureBytes(t, filepath.Join(album, "untagged.dff"), dffWithChunks(t, 2822400))
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")

	for _, fx := range dffFixtures {
		t.Run(fx.name, func(t *testing.T) {
			want := fx.want
			if want.artist == "" {
				want.artist = "Guess Artist"
			}
			if want.album == "" {
				want.album = "Guess Album"
			}
			requireTags(t, storedTrack(t, store, "Guess Artist/Guess Album/"+fx.name), want)
		})
	}
	requireTags(t, storedTrack(t, store, "Guess Artist/Guess Album/untagged.dff"),
		dffTags{title: "untagged", artist: "Guess Artist", album: "Guess Album"})
}

// TestScanner_AWAVsListInfoOutranksThePathsGuess is the same defect in the one
// other extractor that filled a field only when it was empty: a WAV's RIFF
// LIST/INFO title, artist and album (INAM, IART, IPRD) lost to the path's
// guess in every scan. An ID3 chunk in the same file still answers each field
// it has a value for, whichever chunk comes first, and a WAV with neither
// keeps the guess.
func TestScanner_AWAVsListInfoOutranksThePathsGuess(t *testing.T) {
	info := listInfo("INAM", "Info Title", "IART", "Info Artist", "IPRD", "Info Album", "IGNR", "Info Genre")
	id3 := riffChunk("id3 ", buildID3v2_3(map[string]string{"title": "ID3 Title", "genre": "ID3 Genre"}))
	files := []struct {
		name   string
		chunks [][]byte
		want   dffTags
	}{
		{"info.wav", [][]byte{info},
			dffTags{title: "Info Title", artist: "Info Artist", album: "Info Album", genre: "Info Genre"}},
		{"info_then_id3.wav", [][]byte{info, id3},
			dffTags{title: "ID3 Title", artist: "Info Artist", album: "Info Album", genre: "ID3 Genre"}},
		{"id3_then_info.wav", [][]byte{id3, info},
			dffTags{title: "ID3 Title", artist: "Info Artist", album: "Info Album", genre: "ID3 Genre"}},
		{"plain.wav", nil, dffTags{title: "plain", artist: "Guess Artist", album: "Guess Album"}},
	}
	root := t.TempDir()
	album := filepath.Join(root, "Guess Artist", "Guess Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		writeFixtureBytes(t, filepath.Join(album, f.name), wavWithChunks(f.chunks...))
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			requireTags(t, storedTrack(t, store, "Guess Artist/Guess Album/"+f.name), f.want)
		})
	}
}

// TestScanner_EveryFormatsOwnTitleOutranksThePathsGuess is the sweep behind
// backlog B140: for every family of extractor (readFaultFormats), a file whose
// tags hold a title gets that title from a real scan, where the scanner fills
// the path's guess first. A DFF's DIIN and a WAV's LIST/INFO lost theirs to the
// file name until ExtractorVersion 20, while their extractor tests, run on an
// empty Track, passed.
func TestScanner_EveryFormatsOwnTitleOutranksThePathsGuess(t *testing.T) {
	for _, format := range readFaultFormats {
		t.Run(format.name, func(t *testing.T) {
			f := newLinkedFixture(t)
			format.write(t, filepath.Join(f.album, format.file), "The Tag's Title")
			scanOnce(t, f.sc, "scan")
			requireVersion(t, f.store, "Music/Album/"+format.file, "The Tag's Title")
		})
	}
}

// TestExtractDFF_AChunkPastTheEndOfTheFileKeepsTheFormat: a DIIN or ID3 chunk
// declaring more than the file holds (a copy cut short, whose tags come last)
// ends the walk where the file ends, the format the walk gathered is stamped,
// and the chunk's body is never allocated. Until ExtractorVersion 20 the walk
// allocated a DIIN body the file could not back, failed to read it, and its
// error left the file indexed by name alone, with no sample rate or DSD flag.
func TestExtractDFF_AChunkPastTheEndOfTheFileKeepsTheFormat(t *testing.T) {
	cases := []struct {
		id       string
		declared int // what the chunk says its body holds
	}{
		{"DIIN", 900 << 10}, // under the DIIN cap (1 MiB)
		{"ID3 ", 30 << 20},  // under the ID3 cap (32 MiB)
	}
	for _, c := range cases {
		t.Run(strings.TrimSpace(c.id), func(t *testing.T) {
			// A chunk header declaring c.declared bytes, then 100 of them.
			hdr := dffChunk(c.id, nil)
			binary.BigEndian.PutUint64(hdr[4:12], uint64(c.declared))
			data := dffWithChunks(t, 2822400, append(hdr, bytes.Repeat([]byte{'x'}, 100)...))
			path := writeTempDFF(t, data)
			var tr Track
			before := heapAllocated()
			err := ExtractWithContext(path, &tr, nil)
			allocated := heapAllocated() - before
			if err != nil {
				t.Fatalf("extract: %v (a chunk the file cannot hold is no reason to refuse the file)", err)
			}
			if tr.SampleRate == nil || *tr.SampleRate != 2822400 || tr.IsDSD == nil || !*tr.IsDSD {
				t.Errorf("the format is not stamped: SampleRate %v, IsDSD %v", tr.SampleRate, tr.IsDSD)
			}
			if allocated >= uint64(c.declared) {
				t.Errorf("extracting a %d-byte file allocated %d bytes: the %q chunk's declared %d-byte body was allocated",
					len(data), allocated, c.id, c.declared)
			}
		})
	}
}

// TestExtractDFF_TheCoverInItsID3ChunkIsItsCover: a picture in a DFF's ID3
// chunk is the track's cover, as a DSF's or an AIFF's is, where the DFF walk
// only ever looked for a cover.jpg beside the file.
func TestExtractDFF_TheCoverInItsID3ChunkIsItsCover(t *testing.T) {
	jpg := encodeSolidImage(t, 64, 64, 80)
	id3 := buildID3v2_3WithAPIC(map[string]string{"title": "Covered"}, "image/jpeg", jpg)
	path := writeTempDFF(t, dffWithChunks(t, 2822400, dffChunk("ID3 ", id3)))
	var tr Track
	if err := ExtractWithContext(path, &tr, &ExtractContext{ArtworkCacheDir: t.TempDir()}); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if tr.Title != "Covered" {
		t.Errorf("Title = %q, want the ID3 chunk's %q", tr.Title, "Covered")
	}
	if !strings.HasPrefix(tr.ArtworkMBID, "local-") {
		t.Errorf("ArtworkMBID = %q, want the local cover the ID3 chunk's picture makes", tr.ArtworkMBID)
	}
}

// TestScanner_V20_ARowWhoseOwnTagsWereLostJoinsTheDelta_APlainRowOnlyStamps is
// the v20 upgrade end to end. A v19 bridge stored the path's guess for a DFF
// whose DIIN or ID3 chunk names a title and for a WAV whose LIST/INFO does,
// under a stale stamp: such a row re-extracts once, gains its own tags,
// advances its indexed_at (the iOS delta) and goes back to the enricher, which
// had searched MusicBrainz with the folder names. A DFF or WAV with no tags
// re-extracts byte-identical and is only stamped: no delta, no re-enrichment.
func TestScanner_V20_ARowWhoseOwnTagsWereLostJoinsTheDelta_APlainRowOnlyStamps(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Guess Artist", "Guess Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	own := map[string]string{
		"diin.dff": "Prélude à la nuit, première",
		"id3.dff":  "Picard Title",
		"info.wav": "Info Title",
	}
	writeFixtureBytes(t, filepath.Join(album, "diin.dff"), dffFixture(t, "taglib_diin.dff"))
	writeFixtureBytes(t, filepath.Join(album, "id3.dff"), dffFixture(t, "picard_id3.dff"))
	writeFixtureBytes(t, filepath.Join(album, "info.wav"), wavWithChunks(listInfo("INAM", "Info Title", "IART", "Info Artist")))
	writeFixtureBytes(t, filepath.Join(album, "plain.dff"), dffWithChunks(t, 2822400))
	writeFixtureBytes(t, filepath.Join(album, "plain.wav"), wavWithChunks())
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")
	for name, title := range own {
		if got := storedTrack(t, store, "Guess Artist/Guess Album/"+name).Title; got != title {
			t.Fatalf("premise: the current extractor stores %s's own title %q; got %q", name, title, got)
		}
	}

	before := map[string]int64{}
	for _, name := range []string{"diin.dff", "id3.dff", "info.wav", "plain.dff", "plain.wav"} {
		rel := "Guess Artist/Guess Album/" + name
		rewindToV19(t, store, rel, own[name] != "")
		before[name] = trackIndexedAt(t, store, rel)
	}

	scanOnce(t, sc, "v20")

	for name, title := range own {
		rel := "Guess Artist/Guess Album/" + name
		if got := storedTrack(t, store, rel).Title; got != title {
			t.Errorf("%s: title %q after the re-extract, want its own %q", name, got, title)
		}
		if after := trackIndexedAt(t, store, rel); after <= before[name] {
			t.Errorf("%s: indexed_at did not advance (%d -> %d): iOS would never pull its tags", name, before[name], after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 0 {
			t.Errorf("%s: enriched_at = %d: the enricher would keep what it found from the folder names", name, v)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", name, v, ExtractorVersion)
		}
	}
	for _, name := range []string{"plain.dff", "plain.wav"} {
		rel := "Guess Artist/Guess Album/" + name
		if after := trackIndexedAt(t, store, rel); after != before[name] {
			t.Errorf("%s: indexed_at moved (%d -> %d): v20 must not put a row without tags in the delta", name, before[name], after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: v20 must not re-enrich a row without tags", name, v)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", name, v, ExtractorVersion)
		}
	}
}

// rewindToV19 puts a row back where a v19 bridge left it: stamped 19, and
// enriched. A row whose own tags v19 lost gets v19's tags too: the path's
// guess (its file name as the title, its folders as the album and the artist),
// and none of the genre, track number or year its tags hold.
func rewindToV19(t *testing.T, store *Store, rel string, lostItsTags bool) {
	t.Helper()
	q, args := "UPDATE tracks SET extractor_version = 19, enriched_at = 1 WHERE path = ?", []any{rel}
	if lostItsTags {
		stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		q = `UPDATE tracks SET extractor_version = 19, enriched_at = 1,
			tags_json = json_set(json_remove(tags_json, '$.genre', '$.trackNumber', '$.year'),
			                     '$.title', ?, '$.artist', 'Guess Artist', '$.album', 'Guess Album')
			WHERE path = ?`
		args = []any{stem, rel}
	}
	if _, err := store.db.Exec(q, args...); err != nil {
		t.Fatalf("rewind %s to v19: %v", rel, err)
	}
}
