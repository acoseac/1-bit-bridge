package manifest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	meta "github.com/mewkiz/flac/meta"
)

// The expected strings in this file are the app's VorbisCommentParser
// output (MultiValueFields, maxValuesPerField 64): empty values skipped
// before they count, comment order kept, no de-duplication, values joined
// with "; ", and a later comment block appended the same way. They are
// written out here, not taken from the extractor.

func writeFLACCommentBlocks(t *testing.T, path string, blocks ...[][2]string) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	writeMetaBlockHeader(&buf, meta.TypeStreamInfo, false, 34)
	writeStreamInfo(&buf, &meta.StreamInfo{
		BlockSizeMin:  4096,
		BlockSizeMax:  4096,
		SampleRate:    44100,
		NChannels:     2,
		BitsPerSample: 16,
		NSamples:      44100 * 5,
	})
	for i, tags := range blocks {
		body := encodeVorbisComment(&meta.VorbisComment{Vendor: "1-bit-bridge test fixture", Tags: tags})
		writeMetaBlockHeader(&buf, meta.TypeVorbisComment, i == len(blocks)-1, uint32(len(body)))
		buf.Write(body)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// numberedField is n comments NAME=p0 .. NAME=p(n-1), in that order.
func numberedField(name, prefix string, n int) [][2]string {
	out := make([][2]string, n)
	for i := range out {
		out[i] = [2]string{name, fmt.Sprintf("%s%d", prefix, i)}
	}
	return out
}

// appJoined is the app's join of prefix0 .. prefix(n-1).
func appJoined(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return strings.Join(parts, "; ")
}

func flacAt(t *testing.T, tags ...[2]string) Track {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, tags)
	return extractFile(t, p)
}

func flacBlocks(t *testing.T, blocks ...[][2]string) Track {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.flac")
	writeFLACCommentBlocks(t, p, blocks...)
	return extractFile(t, p)
}

// vorbisJoinSrc is which fixture builder a join case uses. The expected
// strings stay in the case table, written as the app's output.
type vorbisJoinSrc int

const (
	vorbisPairs vorbisJoinSrc = iota
	vorbisBlocks
	vorbisOggFLAC
	vorbisOgg
	vorbisOpus
)

// vorbisJoinCase is one subtest. wants is the assertion list: each pair
// is a Track field and the string the app would store.
type vorbisJoinCase struct {
	name   string
	src    vorbisJoinSrc
	tags   [][2]string
	blocks [][][2]string
	wants  [][2]string
}

func (c vorbisJoinCase) track(t *testing.T) Track {
	t.Helper()
	switch c.src {
	case vorbisPairs:
		return flacAt(t, c.tags...)
	case vorbisBlocks:
		return flacBlocks(t, c.blocks...)
	case vorbisOggFLAC:
		return extractBytesAs(t, "a.oga", oggFLACFile(255, 2,
			commentBlock(false, "GENRE=Rock"),
			commentBlock(true, "GENRE=Pop"),
		))
	case vorbisOgg:
		return extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(3, 255,
			vorbisIdent(), vorbisCommentPacket("GENRE=Rock", "GENRE=Pop"), []byte{0, 1, 2})...))
	case vorbisOpus:
		return extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(4, 255,
			append([]byte("OpusHead"), make([]byte, 11)...),
			opusCommentPacket("GENRE=Rock", "GENRE=Pop"), []byte{0, 1, 2})...))
	default:
		t.Fatalf("unknown vorbis fixture %d", c.src)
		return Track{}
	}
}

func vorbisTrackField(tr Track, field string) (string, bool) {
	switch field {
	case "Genre":
		return tr.Genre, true
	case "Album":
		return tr.Album, true
	case "Composer":
		return tr.Composer, true
	case "Conductor":
		return tr.Conductor, true
	case "Work":
		return tr.Work, true
	case "Title":
		return tr.Title, true
	case "Artist":
		return tr.Artist, true
	default:
		return "", false
	}
}

func assertVorbisField(t *testing.T, tr Track, field, want string) {
	t.Helper()
	got, ok := vorbisTrackField(tr, field)
	if !ok {
		t.Fatalf("unknown field %s", field)
	}
	if got == want {
		return
	}
	// A joined cap is hundreds of bytes. The separator count names which
	// end was dropped; the comparison above is still the whole string.
	if strings.Count(want, "; ") > 8 {
		t.Fatalf("%s has %d separators, want %d", field, strings.Count(got, "; "), strings.Count(want, "; "))
	}
	t.Fatalf("%s = %q, want %q", field, got, want)
}

// TestVorbisJoinsMatchTheAppsMultiValueRule is backlog B328. A FLAC with
// GENRE=Rock and GENRE=Pop is "Rock; Pop" in the app and was "Pop" from
// the bridge, because dhowden keeps one value per name. GENRE, ALBUM,
// COMPOSER, CONDUCTOR and WORK follow the app. TITLE does not: the bridge
// keeps dhowden's last title, and the first non-empty TITLE is only the
// explicit-content verdict's. ARTIST stays joined with no per-block cap.
func TestVorbisJoinsMatchTheAppsMultiValueRule(t *testing.T) {
	g64 := appJoined("g", 64) // g0 .. g63; g64 is past the cap
	// 63 non-empty, then an empty, then the 64th non-empty. Counting
	// the empty toward the cap would drop g63.
	emptySlot := append(numberedField("GENRE", "g", 63),
		[2]string{"GENRE", ""}, [2]string{"GENRE", "g63"})
	// Two blocks of 65. The app keeps 64 from each and appends, so the
	// file holds g0..g63 and then h0..h63. A cap across the file would
	// stop at g63.
	perBlock := g64 + "; " + appJoined("h", 64)
	cases := []vorbisJoinCase{
		{
			name: "two genres", src: vorbisPairs,
			tags:  [][2]string{{"GENRE", "Rock"}, {"GENRE", "Pop"}},
			wants: [][2]string{{"Genre", "Rock; Pop"}},
		},
		{
			name: "two albums", src: vorbisPairs,
			tags:  [][2]string{{"ALBUM", "Studio"}, {"ALBUM", "Live"}},
			wants: [][2]string{{"Album", "Studio; Live"}},
		},
		{
			name: "composer conductor and work", src: vorbisPairs,
			tags: [][2]string{
				{"COMPOSER", "Bach"}, {"COMPOSER", "Mozart"},
				{"CONDUCTOR", "Abbado"}, {"CONDUCTOR", "Rattle"},
				{"WORK", "Mass"}, {"WORK", "Requiem"},
			},
			wants: [][2]string{
				{"Composer", "Bach; Mozart"},
				{"Conductor", "Abbado; Rattle"},
				{"Work", "Mass; Requiem"},
			},
		},
		{
			name: "an empty value is skipped", src: vorbisPairs,
			tags: [][2]string{
				{"GENRE", "Rock"}, {"GENRE", "  "}, {"GENRE", "Pop"},
			},
			wants: [][2]string{{"Genre", "Rock; Pop"}},
		},
		{
			name: "whitespace is trimmed", src: vorbisPairs,
			tags: [][2]string{
				{"GENRE", " Rock "}, {"GENRE", "\tPop\n"},
			},
			wants: [][2]string{{"Genre", "Rock; Pop"}},
		},
		{
			name: "values are not de-duplicated", src: vorbisPairs,
			tags:  [][2]string{{"GENRE", "Rock"}, {"GENRE", "Rock"}},
			wants: [][2]string{{"Genre", "Rock; Rock"}},
		},
		{
			name: "a single value stays itself", src: vorbisPairs,
			tags:  [][2]string{{"GENRE", "Rock"}},
			wants: [][2]string{{"Genre", "Rock"}},
		},
		{
			name: "the 65th value of a block is dropped", src: vorbisPairs,
			tags:  numberedField("GENRE", "g", 65),
			wants: [][2]string{{"Genre", g64}},
		},
		{
			name: "an empty value does not consume a cap slot", src: vorbisPairs,
			tags:  emptySlot,
			wants: [][2]string{{"Genre", g64}},
		},
		{
			name: "a later comment block is appended", src: vorbisBlocks,
			blocks: [][][2]string{
				{{"GENRE", "Rock"}, {"ALBUM", "Studio"}},
				{{"GENRE", "Pop"}, {"ALBUM", "Live"}},
			},
			wants: [][2]string{{"Genre", "Rock; Pop"}, {"Album", "Studio; Live"}},
		},
		{
			name: "the cap is per comment block", src: vorbisBlocks,
			blocks: [][][2]string{
				numberedField("GENRE", "g", 65),
				numberedField("GENRE", "h", 65),
			},
			wants: [][2]string{{"Genre", perBlock}},
		},
		{
			name:  "an ogg flac stream joins the same way",
			src:   vorbisOggFLAC,
			wants: [][2]string{{"Genre", "Rock; Pop"}},
		},
		{
			name: "a title is not joined", src: vorbisPairs,
			tags:  [][2]string{{"TITLE", "First"}, {"TITLE", "Second"}},
			wants: [][2]string{{"Title", "Second"}},
		},
		{
			name: "artists stay uncapped", src: vorbisPairs,
			tags:  numberedField("ARTIST", "a", 65),
			wants: [][2]string{{"Artist", appJoined("a", 65)}},
		},
		{
			name:  "plain ogg vorbis keeps the last value",
			src:   vorbisOgg,
			wants: [][2]string{{"Genre", "Pop"}},
		},
		{
			name:  "opus keeps the last value",
			src:   vorbisOpus,
			wants: [][2]string{{"Genre", "Pop"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := tc.track(t)
			for _, w := range tc.wants {
				assertVorbisField(t, tr, w[0], w[1])
			}
		})
	}
}

// TestScanner_V24_AJoinedGenreJoinsTheDelta_ASingleGenreOnlyStamps is the
// upgrade. A row a v23 bridge stored as the last GENRE ("Pop") is re-read
// as "Rock; Pop" and takes the full-upsert leg. A row whose single genre
// the join leaves as it was is only stamped.
func TestScanner_V24_AJoinedGenreJoinsTheDelta_ASingleGenreOnlyStamps(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, f := range []struct {
		rel  string
		tags [][2]string
	}{
		{"A/01.flac", [][2]string{
			{"TITLE", "One"}, {"ALBUM", "Alpha"}, {"ARTIST", "X"}, {"TRACKNUMBER", "1"},
			{"GENRE", "Rock"}, {"GENRE", "Pop"},
		}},
		{"A/02.flac", [][2]string{
			{"TITLE", "Two"}, {"ALBUM", "Alpha"}, {"ARTIST", "X"}, {"TRACKNUMBER", "2"},
			{"GENRE", "Rock"},
		}},
	} {
		writeMinimalFLACPairs(t, mkdir(f.rel), 44100, 16, f.tags)
	}

	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	rows := []struct {
		rel, v23, want string
	}{
		{"A/01.flac", "Pop", "Rock; Pop"},
		{"A/02.flac", "Rock", "Rock"},
	}
	before := map[string]int64{}
	for _, r := range rows {
		q := "UPDATE tracks SET extractor_version = 23, enriched_at = 1 WHERE path = ?"
		args := []any{r.rel}
		if r.v23 != r.want {
			q = `UPDATE tracks SET extractor_version = 23, enriched_at = 1,
				tags_json = json_set(tags_json, '$.genre', ?) WHERE path = ?`
			args = []any{r.v23, r.rel}
		}
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("rewind %s: %v", r.rel, err)
		}
		before[r.rel] = trackIndexedAt(t, store, r.rel)
	}

	scanOnce(t, sc, "v24")

	for _, r := range rows {
		got := storedTrack(t, store, r.rel)
		if v := trackColumn(t, store, r.rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", r.rel, v, ExtractorVersion)
		}
		if got.Genre != r.want {
			t.Errorf("%s: Genre = %q, want %q", r.rel, got.Genre, r.want)
		}
		after := trackIndexedAt(t, store, r.rel)
		if r.v23 != r.want {
			if after <= before[r.rel] {
				t.Errorf("%s: indexed_at did not advance (%d -> %d)", r.rel, before[r.rel], after)
			}
			continue
		}
		if after != before[r.rel] {
			t.Errorf("%s: indexed_at moved (%d -> %d): a single genre must only stamp", r.rel, before[r.rel], after)
		}
		if v := trackColumn(t, store, r.rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: a single genre must not re-enrich", r.rel, v)
		}
	}
}
