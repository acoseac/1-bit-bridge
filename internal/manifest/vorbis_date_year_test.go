package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commentPairs splits "KEY=value" comments into the pairs the FLAC fixture
// writer takes.
func commentPairs(comments []string) [][2]string {
	pairs := make([][2]string, 0, len(comments))
	for _, c := range comments {
		k, v, _ := strings.Cut(c, "=")
		pairs = append(pairs, [2]string{k, v})
	}
	return pairs
}

// extractFile extracts the file at p as a scan would.
func extractFile(t *testing.T, p string) Track {
	t.Helper()
	tr := Track{Path: filepath.Base(p)}
	if err := ExtractWithContext(p, &tr, &ExtractContext{}); err != nil {
		t.Fatal(err)
	}
	return tr
}

// vorbisCommentFormats are the files dhowden reads through its Vorbis comment
// reader (metadataVorbis, Format() == tag.VORBIS), each holding the given
// "KEY=value" comments.
var vorbisCommentFormats = []struct {
	name    string
	extract func(t *testing.T, comments ...string) Track
}{
	{"FLAC", func(t *testing.T, comments ...string) Track {
		p := filepath.Join(t.TempDir(), "a.flac")
		writeMinimalFLACPairs(t, p, 44100, 16, commentPairs(comments))
		return extractFile(t, p)
	}},
	{"Ogg Vorbis", func(t *testing.T, comments ...string) Track {
		return extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(3, 255,
			vorbisIdent(), vorbisCommentPacket(comments...), []byte{0, 1, 2})...))
	}},
	{"Opus", func(t *testing.T, comments ...string) Track {
		return extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(4, 255,
			append([]byte("OpusHead"), make([]byte, 11)...), opusCommentPacket(comments...), []byte{0, 1, 2})...))
	}},
	{"Ogg FLAC", func(t *testing.T, comments ...string) Track {
		return extractBytesAs(t, "a.oga", oggFLACFile(255, 1, commentBlock(true, comments...)))
	}},
}

func yearOf(p *int) string {
	if p == nil {
		return "absent"
	}
	return fmt.Sprint(*p)
}

func yearPtr(y int) *int { return &y }

// TestADateDhowdenCannotParseIsReadByItsYearPrefix is backlog B222 (B234 from
// the app's side). dhowden's Vorbis reader picks a time layout by the DATE's
// length (4, 7 or 10 bytes) and answers the zero time's year, 1, for a value
// of any other length or one time.Parse refuses, and the extractor fell back
// to parseYearPrefix only on a 0: an ISO timestamp, "1974?" or "2019/03/22"
// indexed as year 1, and so did a DATE holding no year at all, which the year
// pass never fills (1 is positive). A DATE holding no year beside a YEAR that
// holds one reads the YEAR, as the app's reader does: the fallback asks each
// date tag in turn for one parseYearPrefix can read.
func TestADateDhowdenCannotParseIsReadByItsYearPrefix(t *testing.T) {
	cases := []struct {
		name     string
		comments []string
		want     *int
	}{
		{"an ISO timestamp", []string{"DATE=2017-01-27T12:00:00Z"}, yearPtr(2017)},
		{"a year with a mark after it", []string{"DATE=1974?"}, yearPtr(1974)},
		{"a date with slashes", []string{"DATE=2019/03/22"}, yearPtr(2019)},
		{"a day-first date", []string{"DATE=22.03.2019"}, yearPtr(0)},
		{"no year in it", []string{"DATE=unknown"}, yearPtr(0)},
		{"a day-first date beside a YEAR", []string{"DATE=22.03.2019", "YEAR=2019"}, yearPtr(2019)},
		{"a year dhowden reads", []string{"DATE=2019"}, yearPtr(2019)},
		{"a full date dhowden reads", []string{"DATE=2019-03-22"}, yearPtr(2019)},
		{"a YEAR alone", []string{"YEAR=2019"}, yearPtr(2019)},
		{"no date tag", nil, nil},
	}
	for _, f := range vorbisCommentFormats {
		for _, tc := range cases {
			t.Run(f.name+"/"+tc.name, func(t *testing.T) {
				tr := f.extract(t, append([]string{"TITLE=T"}, tc.comments...)...)
				if !sameIntPtr(tr.Year, tc.want) {
					t.Errorf("Year = %s, want %s", yearOf(tr.Year), yearOf(tc.want))
				}
			})
		}
	}
}

// TestAnID3OrMP4DateHoldsNoYearOne: the year-1 rule is the Vorbis reader's
// alone. ID3v2's Year() answers strconv.Atoi of the whole value or a DateOnly
// parse, else 0, and MP4's strconv.Atoi of the first four bytes, 0 when that
// fails: neither makes a 1 the tag does not say, and the fallback has always
// read their 0.
func TestAnID3OrMP4DateHoldsNoYearOne(t *testing.T) {
	for _, tc := range []struct {
		name, date string
		want       int
	}{
		{"an ISO timestamp", "2017-01-27T12:00:00Z", 2017},
		{"a year with a mark after it", "1974?", 1974},
		{"a date with slashes", "2019/03/22", 2019},
		{"a day-first date", "22.03.2019", 0},
		{"no year in it", "unknown", 0},
	} {
		t.Run("MP3/"+tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "a.mp3")
			writeMinimalMP3(t, p, map[string]string{"title": "T", "year": tc.date})
			if tr := extractFile(t, p); !sameIntPtr(tr.Year, &tc.want) {
				t.Errorf("Year = %s, want %d", yearOf(tr.Year), tc.want)
			}
		})
		t.Run("M4A/"+tc.name, func(t *testing.T) {
			tr := extractBytesAsM4A(t, buildMP4WithILST(true, ilstText("\xa9nam", "T"), ilstText("\xa9day", tc.date)))
			if !sameIntPtr(tr.Year, &tc.want) {
				t.Errorf("Year = %s, want %d", yearOf(tr.Year), tc.want)
			}
		})
	}
}

// TestScanner_V22_AnUnparseableDateJoinsTheDelta_AReadableOneOnlyStamps is the
// upgrade, end to end: rows a v21 bridge indexed re-extract once. A row whose
// DATE dhowden could not parse was stored with year 1; it now takes the year
// its DATE holds, or, holding none, the one the year pass fills from its
// folder, and its indexed_at advances, which is what puts it in every paired
// device's delta. Every other row re-extracts byte-identical and is only
// stamped (the version-stale diff guard), and a third scan moves nothing.
func TestScanner_V22_AnUnparseableDateJoinsTheDelta_AReadableOneOnlyStamps(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	flac := func(rel string, comments ...string) {
		writeMinimalFLACPairs(t, mkdir(rel), 44100, 16, commentPairs(comments))
	}
	flac("A/01.flac", "TITLE=One", "ALBUM=Alpha", "ARTIST=X", "TRACKNUMBER=1", "DATE=2017-01-27T12:00:00Z")
	flac("A/02.flac", "TITLE=Two", "ALBUM=Alpha", "ARTIST=X", "TRACKNUMBER=2", "DATE=2017")
	flac("B/01.flac", "TITLE=One", "ALBUM=Beta", "ARTIST=Y", "TRACKNUMBER=1", "DATE=unknown")
	flac("B/02.flac", "TITLE=Two", "ALBUM=Beta", "ARTIST=Y", "TRACKNUMBER=2", "DATE=2019")
	writeFixtureBytes(t, mkdir("C/01.ogg"), joinPages(oggLogicalStream(3, 255, vorbisIdent(),
		vorbisCommentPacket("TITLE=One", "ALBUM=Gamma", "ARTIST=Z", "DATE=1974?"), []byte{0, 1, 2})...))
	writeMinimalMP3(t, mkdir("D/01.mp3"), map[string]string{"title": "One", "album": "Delta", "artist": "W", "year": "1974?"})

	rows := []struct {
		rel       string
		v21, want int // the year a v21 bridge stored, and the one v22 stores
	}{
		{"A/01.flac", 1, 2017},
		{"A/02.flac", 2017, 2017},
		{"B/01.flac", 1, 2019}, // no year in its DATE: the year pass fills its sibling's
		{"B/02.flac", 2019, 2019},
		{"C/01.ogg", 1, 1974},
		{"D/01.mp3", 1974, 1974}, // dhowden answered 0, and the fallback read it
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	before := map[string]int64{}
	for _, r := range rows {
		q, args := "UPDATE tracks SET extractor_version = 21, enriched_at = 1 WHERE path = ?", []any{r.rel}
		if r.v21 != r.want {
			q = `UPDATE tracks SET extractor_version = 21, enriched_at = 1,
				tags_json = json_set(tags_json, '$.year', ?) WHERE path = ?`
			args = []any{r.v21, r.rel}
		}
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("rewind %s to v21: %v", r.rel, err)
		}
		before[r.rel] = trackIndexedAt(t, store, r.rel)
	}

	scanOnce(t, sc, "v22")

	settled := map[string]int64{}
	for _, r := range rows {
		got := storedTrack(t, store, r.rel)
		if v := trackColumn(t, store, r.rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", r.rel, v, ExtractorVersion)
		}
		if !sameIntPtr(got.Year, &r.want) {
			t.Errorf("%s: Year = %s, want %d", r.rel, yearOf(got.Year), r.want)
		}
		after := trackIndexedAt(t, store, r.rel)
		settled[r.rel] = after
		if r.v21 != r.want {
			if after <= before[r.rel] {
				t.Errorf("%s: indexed_at did not advance (%d -> %d): the phone would keep year %d", r.rel, before[r.rel], after, r.v21)
			}
			continue
		}
		if after != before[r.rel] {
			t.Errorf("%s: indexed_at moved (%d -> %d): v22 must not put a row whose year it reads alike in the delta", r.rel, before[r.rel], after)
		}
		if v := trackColumn(t, store, r.rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: v22 must not re-enrich a row whose year it reads alike", r.rel, v)
		}
	}

	scanOnce(t, sc, "after")
	for _, r := range rows {
		if after := trackIndexedAt(t, store, r.rel); after != settled[r.rel] {
			t.Errorf("%s: indexed_at moved on the scan after the upgrade (%d -> %d)", r.rel, settled[r.rel], after)
		}
	}
}
