package manifest

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/dhowden/tag"
)

// Tests for backlog B743: a track or disc number written with its total
// ("3/12", "3 of 12") or padded (" 3 ") indexed as 0, so its album played out
// of order. ExtractorVersion 31.

// numberPtr is n as an optional track or disc number.
func numberPtr(n int) *int { return &n }

// TestLeadingTagNumberReadsTheFormsTaggersWrite is the rule's table (backlog
// B743): the forms whose leading number is read, every other tail, and the
// values strconv.Atoi reads whole, which keep its answer, dhowden's.
func TestLeadingTagNumberReadsTheFormsTaggersWrite(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{
		// The forms: after trimming, the number alone, "/" and what follows
		// it, or whitespace, "of" and what follows that.
		{"1/2", 1},
		{"01/12", 1},
		{"3/", 3},
		{" 3 ", 3},
		{"3 of 10", 3},
		{"+3/12", 3},
		{"-3/12", -3}, // what an ID3 tag reads it as
		{"\t3/12\n", 3},
		{"3/12/2", 3},
		{"3/A", 3},
		{"3 of", 3},
		{"3\tof 10", 3},
		{" -3 ", -3},
		// Any other tail reads 0, and so does a value with no number.
		{"A1", 0},
		{"1.2", 0},
		{"2-1", 0},
		{"3-A", 0},
		{"2024-01-01", 0},
		{"3 / 12", 0}, // dhowden's ID3 reader reads 3; numberAsWritten keeps that
		{"3of10", 0},
		{"3 OF 10", 0},
		{"3 10", 0},
		{"/5", 0},
		{"+", 0},
		{"- 3", 0},
		{"+ 3/12", 0},
		{"", 0},
		{"   ", 0},
		{"0", 0},
		{"0/12", 0},
		// Whole numbers: strconv.Atoi's answer, sign and all.
		{"+3", 3},
		{"-3", -3},
		{"007", 7},
		{"12", 12},
	} {
		if got := leadingTagNumber(c.in); got != c.want {
			t.Errorf("leadingTagNumber(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestLeadingTagNumberPastAnInt32: the answers that depend on the size of an
// int, as int64s, so the table builds where an int is 32 bits (CI vets
// linux/386 and linux/arm). A whole number keeps strconv.Atoi's answer,
// saturated where it overflows, as dhowden's Vorbis reader gave it; a leading
// number that overflows in any other form reads 0. (dhowden's own
// strconv.Atoi of "99999999999999999999/2" saturates too: ParseUint meets the
// overflow before the "/". So that one value changes, from the saturated
// number to 0.)
func TestLeadingTagNumberPastAnInt32(t *testing.T) {
	for _, c := range []struct {
		in             string
		want64, want32 int64
	}{
		{"99999999999999999999", math.MaxInt64, math.MaxInt32},
		{"-99999999999999999999", math.MinInt64, math.MinInt32},
		{"4294967296", 4294967296, math.MaxInt32},
		{"4294967296/2", 4294967296, 0},
		{"99999999999999999999/2", 0, 0},
		{" 99999999999999999999 ", 0, 0},
		{"99999999999999999999 of 2", 0, 0},
	} {
		want := c.want64
		if strconv.IntSize == 32 {
			want = c.want32
		}
		if got := leadingTagNumber(c.in); int64(got) != want {
			t.Errorf("leadingTagNumber(%q) = %d, want %d (int of %d bits)", c.in, got, want, strconv.IntSize)
		}
	}
}

// requireNumbers fails unless tr carries the track and disc numbers want, nil
// for absent.
func requireNumbers(t *testing.T, tr Track, track, disc *int) {
	t.Helper()
	if !sameIntPtr(tr.TrackNumber, track) {
		t.Errorf("TrackNumber = %s, want %s", intPtrString(tr.TrackNumber), intPtrString(track))
	}
	if !sameIntPtr(tr.DiscNumber, disc) {
		t.Errorf("DiscNumber = %s, want %s", intPtrString(tr.DiscNumber), intPtrString(disc))
	}
}

// TestScanner_AFLACNumberedWithItsTotalIndexesItsTrackAndDisc is backlog B743,
// found by the 0.2.3 Docker upgrade test on 0.2.2 and the candidate alike: a
// FLAC tagged TRACKNUMBER=3/12 and DISCNUMBER=2/2 indexed as track 0 and disc 0.
// dhowden's Vorbis reader answers strconv.Atoi of the whole value and drops its
// error. The file's name holds no number, so neither the filename backfill nor
// the track-number pass can give it a 3, and nothing defaults to a 2.
func TestScanner_AFLACNumberedWithItsTotalIndexesItsTrackAndDisc(t *testing.T) {
	root := t.TempDir()
	const rel = "Band/Album/Song.flac"
	libraryFLAC(t, root, rel, map[string]string{
		"TITLE": "Song", "ARTIST": "Band", "ALBUM": "Album",
		"TRACKNUMBER": "3/12", "DISCNUMBER": "2/2",
	})
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "index")
	requireNumbers(t, *readTrack(t, store, rel), numberPtr(3), numberPtr(2))
}

// TestEveryVorbisReaderReadsTheNumberATaggerWrote: FLAC, Ogg Vorbis, Opus and
// Ogg FLAC all reach the extractor through dhowden's Vorbis comment reader, so
// each reads a number with its total, a number "of" its total and a padded
// number. What extraction tells apart stays apart: a value that is present but
// empty, or holds no number the rule reads, is a present 0, and no TRACKNUMBER
// or DISCNUMBER at all is absent. The files' names hold no number.
func TestEveryVorbisReaderReadsTheNumberATaggerWrote(t *testing.T) {
	for _, tc := range []struct {
		name        string
		comments    []string
		track, disc *int
	}{
		{"a number with its total", []string{"TRACKNUMBER=3/12", "DISCNUMBER=2/2"}, numberPtr(3), numberPtr(2)},
		{"a number of its total", []string{"TRACKNUMBER=3 of 12", "DISCNUMBER=2 of 2"}, numberPtr(3), numberPtr(2)},
		{"a padded number", []string{"TRACKNUMBER= 3 ", "DISCNUMBER=\t2\t"}, numberPtr(3), numberPtr(2)},
		{"a signed number with its total", []string{"TRACKNUMBER=+3/12", "DISCNUMBER=-2/2"}, numberPtr(3), numberPtr(-2)},
		{"a plain number", []string{"TRACKNUMBER=03", "DISCNUMBER=2"}, numberPtr(3), numberPtr(2)},
		{"a present empty value", []string{"TRACKNUMBER=", "DISCNUMBER="}, numberPtr(0), numberPtr(0)},
		{"a value holding no number the rule reads", []string{"TRACKNUMBER=A1", "DISCNUMBER=1.2"}, numberPtr(0), numberPtr(0)},
		{"a number with a space before its total", []string{"TRACKNUMBER=3 / 12", "DISCNUMBER=2 / 2"}, numberPtr(0), numberPtr(0)},
		// dhowden's strconv.Atoi saturates this one, meeting the overflow
		// before the "/"; the rule reads an overflowing number as 0.
		{"an overflowing number with its total", []string{"TRACKNUMBER=99999999999999999999/2", "DISCNUMBER=99999999999999999999 of 2"}, numberPtr(0), numberPtr(0)},
		{"no number", nil, nil, nil},
	} {
		for _, f := range vorbisCommentFormats {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				requireNumbers(t, f.extract(t, append([]string{"TITLE=T"}, tc.comments...)...), tc.track, tc.disc)
			})
		}
	}
}

// id3v2NumberTag is an ID3v2 tag of version (2, 3 or 4) holding a title and
// frames, each an id and its text, then 16 bytes of padding. dhowden reads a
// frame only while its offset, which counts the 10-byte header, is short of the
// tag's size, which does not: the last 10 bytes of a tag's frames are never
// read, and a version 2.2 frame of 10 bytes or fewer at the very end is lost.
// Real tags end in padding.
func id3v2NumberTag(version byte, frames ...[2]string) []byte {
	title := "TIT2"
	if version == 2 {
		title = "TT2"
	}
	body := textFrameBytes(version, title, "Song")
	for _, f := range frames {
		body = append(body, textFrameBytes(version, f[0], f[1])...)
	}
	return id3v2TagBytes(version, 0, append(body, make([]byte, 16)...))
}

// TestAnID3TagsNumberIsDhowdensReadingUnlessThatIsZero: dhowden's ID3v2 reader
// splits the frame on "/" and reads each side with strconv.Atoi, the side
// trimmed when there are two, so "3/12" and "3 / 12" read 3 there and " 3 "
// and "3 of 12" read 0. A 0 is read again by the rule a Vorbis comment is read
// by; any other answer stands, "3 / 12" included, which the rule alone would
// read as 0.
func TestAnID3TagsNumberIsDhowdensReadingUnlessThatIsZero(t *testing.T) {
	for _, tc := range []struct {
		name         string
		track, disc  string
		wantT, wantD *int
	}{
		{"a number of its total", "3 of 12", "2 of 2", numberPtr(3), numberPtr(2)},
		{"a padded number", " 3 ", " 2 ", numberPtr(3), numberPtr(2)},
		{"a number with its total and two slashes", "3/12/2", "2/2/1", numberPtr(3), numberPtr(2)},
		{"a number with its total", "3/12", "2/2", numberPtr(3), numberPtr(2)},
		{"a number with its total, spaced", "3 / 12", "2 / 2", numberPtr(3), numberPtr(2)},
		{"a signed number with its total", "-3/12", "+2/2", numberPtr(-3), numberPtr(2)},
		// dhowden saturates it, an answer that is not 0, so it stands.
		{"an overflowing number with its total", "99999999999999999999/2", "99999999999999999999/2", numberPtr(math.MaxInt), numberPtr(math.MaxInt)},
		{"a present empty value", "", "", numberPtr(0), numberPtr(0)},
		{"a value holding no number", "A1", "2-1", numberPtr(0), numberPtr(0)},
	} {
		for _, v := range []struct {
			version byte
			ids     [2]string
		}{{3, [2]string{"TRCK", "TPOS"}}, {4, [2]string{"TRCK", "TPOS"}}} {
			t.Run(tc.name+"/ID3v2."+strconv.Itoa(int(v.version)), func(t *testing.T) {
				data := append(id3v2NumberTag(v.version, [2]string{v.ids[0], tc.track}, [2]string{v.ids[1], tc.disc}), mp3Audio()...)
				requireNumbers(t, extractBytesAs(t, "a.mp3", data), tc.wantT, tc.wantD)
			})
		}
	}
	t.Run("no number", func(t *testing.T) {
		data := append(id3v2NumberTag(3), mp3Audio()...)
		requireNumbers(t, extractBytesAs(t, "a.mp3", data), nil, nil)
	})
}

// TestAnID3v22TagsTrackAndDiscAreRead: an ID3v2.2 tag (iTunes wrote them for
// years) names its track and disc TRK and TPA, which dhowden reads and the
// presence gates did not ask for, so every such file indexed with neither.
func TestAnID3v22TagsTrackAndDiscAreRead(t *testing.T) {
	for _, tc := range []struct {
		name         string
		track, disc  string
		wantT, wantD *int
	}{
		{"a number with its total", "3/12", "2/2", numberPtr(3), numberPtr(2)},
		{"a number of its total", "3 of 12", "2 of 2", numberPtr(3), numberPtr(2)},
		{"a plain number", "3", "2", numberPtr(3), numberPtr(2)},
		{"a present empty value", "", "", numberPtr(0), numberPtr(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(id3v2NumberTag(2, [2]string{"TRK", tc.track}, [2]string{"TPA", tc.disc}), mp3Audio()...)
			requireNumbers(t, extractBytesAs(t, "a.mp3", data), tc.wantT, tc.wantD)
		})
	}
	t.Run("no number", func(t *testing.T) {
		requireNumbers(t, extractBytesAs(t, "a.mp3", append(id3v2NumberTag(2), mp3Audio()...)), nil, nil)
	})
}

// id3v1OnlyMP3 is one MPEG frame and an ID3v1 tag, with no ID3v2 header, its
// comment's byte 28 a NUL and byte 29 track: ID3v1.1's track number, which a
// 0 leaves out (ID3v1.0's comment ran on there).
func id3v1OnlyMP3(track byte) []byte {
	v1 := make([]byte, 128)
	copy(v1[0:3], "TAG")
	copy(v1[3:33], "Song")
	v1[126] = track
	v1[127] = 255
	return append(mp3Audio(), v1...)
}

// TestAnID3v11TrackIsRead: dhowden reads an ID3v1 tag's track byte into the
// raw map's "track" key, which the presence gate did not ask for, so an MP3
// tagged with ID3v1.1 alone indexed with no track number. ID3v1.0 has none,
// and ID3v1 no disc.
func TestAnID3v11TrackIsRead(t *testing.T) {
	requireNumbers(t, extractBytesAs(t, "a.mp3", id3v1OnlyMP3(5)), numberPtr(5), nil)
	requireNumbers(t, extractBytesAs(t, "a.mp3", id3v1OnlyMP3(0)), nil, nil)
}

// TestEveryFileWithAnID3TagReadsTheNumber: WAV and AIFF keep an ID3v2 tag in a
// chunk, DSF at its metadata pointer and DSDIFF in an "ID3 " chunk, and each is
// read by the populate an MP3's tag is. An MP4's trkn and disk atoms are
// binary, which no form reaches.
func TestEveryFileWithAnID3TagReadsTheNumber(t *testing.T) {
	id3 := id3v2NumberTag(3, [2]string{"TRCK", "3 of 12"}, [2]string{"TPOS", " 2 "})
	trkn := atomBytes("trkn", dataAtom(0, []byte{0, 0, 0, 3, 0, 12, 0, 0}))
	disk := atomBytes("disk", dataAtom(0, []byte{0, 0, 0, 2, 0, 2}))
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"a.wav", buildWAVWithID3(t, id3)},
		{"a.aiff", buildAIFFWithID3(t, id3)},
		{"a.dsf", dsfWithID3(id3)},
		{"a.dff", dffWithChunks(t, 2822400, dffChunk("ID3 ", id3))},
		{"a.m4a", buildMP4WithILST(true, ilstText("\xa9nam", "Song"), trkn, disk)},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireNumbers(t, extractBytesAs(t, c.name, c.data), numberPtr(3), numberPtr(2))
		})
	}
}

// vorbisNilRaw is a Vorbis tag whose Raw() is nil, which no reader of dhowden's
// returns.
type vorbisNilRaw struct{ nilRawMetadata }

func (vorbisNilRaw) Format() tag.Format { return tag.VORBIS }

// TestATagWithNoRawMapKeepsItsAccessorsAnswers: with no raw map there is
// nothing to read again or to ask for presence, so the accessors' answers
// stand as they always did, a 0 included, whatever the tag's format.
func TestATagWithNoRawMapKeepsItsAccessorsAnswers(t *testing.T) {
	var tr Track
	populateFromTagMetadata(vorbisNilRaw{nilRawMetadata{track: 0, disc: 7}}, &tr)
	requireNumbers(t, tr, numberPtr(0), numberPtr(7))
}

// syncedThrough is the store's LibraryWatermark, the cursor library.changed
// hands a device: what a device that has synced everything there is asks a
// delta since, the strictest cursor a delta must clear.
func syncedThrough(t *testing.T, store *Store) time.Time {
	t.Helper()
	ns, ok, err := store.LibraryWatermark(context.Background())
	if err != nil || !ok {
		t.Fatalf("LibraryWatermark: %d, %v, %v", ns, ok, err)
	}
	return time.Unix(0, ns)
}

// manifestNumbers is the track and disc number of each track in m, by path.
func manifestNumbers(m *Manifest) map[string][2]string {
	out := make(map[string][2]string, len(m.Tracks))
	for i := range m.Tracks {
		tr := &m.Tracks[i]
		out[tr.Path] = [2]string{intPtrString(tr.TrackNumber), intPtrString(tr.DiscNumber)}
	}
	return out
}

// TestScanner_V31_ANumberReadAnewJoinsTheDelta_AnUnchangedOneOnlyStamps is the
// upgrade, end to end. Rows a v30 bridge indexed are seeded at the literal
// version 30, their files' size and mtime unchanged, with the numbers v30
// stored: 0 for a number written with its total, and for "05 - Named.flac",
// whose TRACKNUMBER of 4/12 read 0, the 5 its name gave it (the extractor's
// filename backfill, and the track-number pass for a row indexed before that).
// One scan re-reads each once: a row whose numbers change is in the full
// manifest and the delta with the numbers its tags write, its enrichment
// re-queued; a row re-read alike is only stamped, so it stays out of the delta
// and keeps its enrichment. The scan after that moves nothing. With
// ExtractorVersion left at 30 no row is re-read, and every changed row keeps
// v30's numbers.
func TestScanner_V31_ANumberReadAnewJoinsTheDelta_AnUnchangedOneOnlyStamps(t *testing.T) {
	root := t.TempDir()
	rows := []struct {
		rel             string
		v30Track, track int // the track number a v30 bridge stored, and v31's
		v30Disc, disc   int
	}{
		{"Band/Album/Song.flac", 0, 3, 0, 2},
		{"Band/Album/05 - Named.flac", 5, 4, 0, 2},
		{"Band/Album/Disc.flac", 6, 6, 0, 2}, // only the disc, which no pass writes, so not held
		{"Band/Album/Plain.flac", 7, 7, 2, 2},
		{"Band/Album/Spaced.mp3", 8, 8, 2, 2}, // dhowden reads "8 / 12" as 8
	}
	libraryFLAC(t, root, rows[0].rel, map[string]string{"TITLE": "Song", "ALBUM": "Album", "TRACKNUMBER": "3/12", "DISCNUMBER": "2/2"})
	libraryFLAC(t, root, rows[1].rel, map[string]string{"TITLE": "Named", "ALBUM": "Album", "TRACKNUMBER": "4/12", "DISCNUMBER": "2/2"})
	libraryFLAC(t, root, rows[2].rel, map[string]string{"TITLE": "Disc", "ALBUM": "Album", "TRACKNUMBER": "6", "DISCNUMBER": "2/2"})
	libraryFLAC(t, root, rows[3].rel, map[string]string{"TITLE": "Plain", "ALBUM": "Album", "TRACKNUMBER": "7", "DISCNUMBER": "2"})
	writeMinimalMP3(t, filepath.Join(root, filepath.FromSlash(rows[4].rel)),
		map[string]string{"title": "Spaced", "album": "Album", "track": "8 / 12", "disc": "2/2"})
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "index")

	for _, r := range rows {
		if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = 30, enriched_at = 1,
			tags_json = json_set(tags_json, '$.trackNumber', ?, '$.discNumber', ?) WHERE path = ?`,
			r.v30Track, r.v30Disc, r.rel); err != nil {
			t.Fatalf("seed %s at v30: %v", r.rel, err)
		}
	}
	ctx := context.Background()
	since := syncedThrough(t, store)

	scanOnce(t, sc, "v31")

	full, err := BuildManifest(ctx, store, []string{root}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	delta, err := BuildManifest(ctx, store, []string{root}, since)
	if err != nil {
		t.Fatal(err)
	}
	inFull, inDelta := manifestNumbers(full), manifestNumbers(delta)
	var changed []string
	for _, r := range rows {
		want := [2]string{strconv.Itoa(r.track), strconv.Itoa(r.disc)}
		if got := inFull[r.rel]; got != want {
			t.Errorf("%s: the full manifest says track/disc %v, want %v", r.rel, got, want)
		}
		if v := trackColumn(t, store, r.rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d: the row was not re-read", r.rel, v, ExtractorVersion)
		}
		got, delivered := inDelta[r.rel]
		if r.v30Track == r.track && r.v30Disc == r.disc {
			if delivered {
				t.Errorf("%s is in the delta: a row re-read alike must only be stamped", r.rel)
			}
			if at := enrichedAtOf(t, store, r.rel); at != 1 {
				t.Errorf("%s: enriched_at = %d, want 1: a row re-read alike must not be re-enriched", r.rel, at)
			}
			continue
		}
		changed = append(changed, r.rel)
		if !delivered {
			t.Errorf("%s is not in the delta: the phone would keep track/disc %d/%d", r.rel, r.v30Track, r.v30Disc)
		} else if got != want {
			t.Errorf("%s: the delta says track/disc %v, want %v", r.rel, got, want)
		}
		if at := enrichedAtOf(t, store, r.rel); at != 0 {
			t.Errorf("%s: enriched_at = %d, want 0: a changed row is enriched again once", r.rel, at)
		}
	}
	if len(inDelta) != len(changed) {
		got := make([]string, 0, len(inDelta))
		for p := range inDelta {
			got = append(got, p)
		}
		sort.Strings(got)
		t.Errorf("the delta holds %v, want exactly %v", got, changed)
	}
	if len(delta.Deleted) != 0 || delta.DeltaIncomplete {
		t.Errorf("the delta deletes %v (incomplete %v), want nothing", delta.Deleted, delta.DeltaIncomplete)
	}

	settled := syncedThrough(t, store)
	scanOnce(t, sc, "after")
	after, err := BuildManifest(ctx, store, []string{root}, settled)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tracks) != 0 || len(after.Deleted) != 0 {
		t.Errorf("the scan after the upgrade put %v in the delta and deleted %v, want nothing", manifestNumbers(after), after.Deleted)
	}
}
