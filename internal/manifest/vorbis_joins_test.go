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

// TestVorbisJoinsMatchTheAppsMultiValueRule is backlog B328. A FLAC with
// GENRE=Rock and GENRE=Pop is "Rock; Pop" in the app and was "Pop" from
// the bridge, because dhowden keeps one value per name. GENRE, ALBUM,
// COMPOSER, CONDUCTOR and WORK follow the app. TITLE does not: the bridge
// keeps dhowden's last title, and the first non-empty TITLE is only the
// explicit-content verdict's. ARTIST stays joined with no per-block cap.
func TestVorbisJoinsMatchTheAppsMultiValueRule(t *testing.T) {
	t.Run("two genres", func(t *testing.T) {
		tr := flacAt(t, [2]string{"GENRE", "Rock"}, [2]string{"GENRE", "Pop"})
		if tr.Genre != "Rock; Pop" {
			t.Fatalf("Genre = %q, want %q", tr.Genre, "Rock; Pop")
		}
	})
	t.Run("two albums", func(t *testing.T) {
		tr := flacAt(t, [2]string{"ALBUM", "Studio"}, [2]string{"ALBUM", "Live"})
		if tr.Album != "Studio; Live" {
			t.Fatalf("Album = %q, want %q", tr.Album, "Studio; Live")
		}
	})
	t.Run("composer conductor and work", func(t *testing.T) {
		tr := flacAt(t,
			[2]string{"COMPOSER", "Bach"}, [2]string{"COMPOSER", "Mozart"},
			[2]string{"CONDUCTOR", "Abbado"}, [2]string{"CONDUCTOR", "Rattle"},
			[2]string{"WORK", "Mass"}, [2]string{"WORK", "Requiem"},
		)
		if tr.Composer != "Bach; Mozart" || tr.Conductor != "Abbado; Rattle" || tr.Work != "Mass; Requiem" {
			t.Fatalf("composer/conductor/work = %q / %q / %q", tr.Composer, tr.Conductor, tr.Work)
		}
	})
	t.Run("an empty value is skipped", func(t *testing.T) {
		tr := flacAt(t, [2]string{"GENRE", "Rock"}, [2]string{"GENRE", "  "}, [2]string{"GENRE", "Pop"})
		if tr.Genre != "Rock; Pop" {
			t.Fatalf("Genre = %q, want %q", tr.Genre, "Rock; Pop")
		}
	})
	t.Run("whitespace is trimmed", func(t *testing.T) {
		tr := flacAt(t, [2]string{"GENRE", " Rock "}, [2]string{"GENRE", "\tPop\n"})
		if tr.Genre != "Rock; Pop" {
			t.Fatalf("Genre = %q, want %q", tr.Genre, "Rock; Pop")
		}
	})
	t.Run("values are not de-duplicated", func(t *testing.T) {
		tr := flacAt(t, [2]string{"GENRE", "Rock"}, [2]string{"GENRE", "Rock"})
		if tr.Genre != "Rock; Rock" {
			t.Fatalf("Genre = %q, want %q", tr.Genre, "Rock; Rock")
		}
	})
	t.Run("a single value stays itself", func(t *testing.T) {
		tr := flacAt(t, [2]string{"GENRE", "Rock"})
		if tr.Genre != "Rock" {
			t.Fatalf("Genre = %q, want Rock", tr.Genre)
		}
	})
	t.Run("the 65th value of a block is dropped", func(t *testing.T) {
		tr := flacAt(t, numberedField("GENRE", "g", 65)...)
		want := appJoined("g", 64) // g0 .. g63; g64 is past the cap
		if tr.Genre != want {
			t.Fatalf("Genre has %d separators, want 63 (g0..g63)", strings.Count(tr.Genre, "; "))
		}
	})
	t.Run("an empty value does not consume a cap slot", func(t *testing.T) {
		// 63 non-empty, then an empty, then the 64th non-empty. Counting
		// the empty toward the cap would drop g63.
		tags := numberedField("GENRE", "g", 63)
		tags = append(tags, [2]string{"GENRE", ""}, [2]string{"GENRE", "g63"})
		tr := flacAt(t, tags...)
		if tr.Genre != appJoined("g", 64) {
			t.Fatalf("Genre has %d separators, want 63 (g0..g63, the empty skipped)", strings.Count(tr.Genre, "; "))
		}
	})
	t.Run("a later comment block is appended", func(t *testing.T) {
		tr := flacBlocks(t,
			[][2]string{{"GENRE", "Rock"}, {"ALBUM", "Studio"}},
			[][2]string{{"GENRE", "Pop"}, {"ALBUM", "Live"}},
		)
		if tr.Genre != "Rock; Pop" || tr.Album != "Studio; Live" {
			t.Fatalf("genre/album = %q / %q", tr.Genre, tr.Album)
		}
	})
	t.Run("the cap is per comment block", func(t *testing.T) {
		// Two blocks of 65. The app keeps 64 from each and appends, so
		// the file holds g0..g63 and then h0..h63. A cap across the file
		// would stop at g63.
		tr := flacBlocks(t, numberedField("GENRE", "g", 65), numberedField("GENRE", "h", 65))
		want := appJoined("g", 64) + "; " + appJoined("h", 64)
		if tr.Genre != want {
			t.Fatalf("Genre has %d separators, want 127 (64+64)", strings.Count(tr.Genre, "; "))
		}
	})
	t.Run("an ogg flac stream joins the same way", func(t *testing.T) {
		tr := extractBytesAs(t, "a.oga", oggFLACFile(255, 2,
			commentBlock(false, "GENRE=Rock"),
			commentBlock(true, "GENRE=Pop"),
		))
		if tr.Genre != "Rock; Pop" {
			t.Fatalf("Genre = %q, want %q", tr.Genre, "Rock; Pop")
		}
	})
	t.Run("a title is not joined", func(t *testing.T) {
		tr := flacAt(t, [2]string{"TITLE", "First"}, [2]string{"TITLE", "Second"})
		if tr.Title != "Second" {
			t.Fatalf("Title = %q, want Second (dhowden's last value; the app would join)", tr.Title)
		}
	})
	t.Run("artists stay uncapped", func(t *testing.T) {
		tr := flacAt(t, numberedField("ARTIST", "a", 65)...)
		if tr.Artist != appJoined("a", 65) {
			t.Fatalf("Artist has %d separators, want 64 (no per-block cap)", strings.Count(tr.Artist, "; "))
		}
	})
	t.Run("plain ogg vorbis keeps the last value", func(t *testing.T) {
		tr := extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(3, 255,
			vorbisIdent(), vorbisCommentPacket("GENRE=Rock", "GENRE=Pop"), []byte{0, 1, 2})...))
		if tr.Genre != "Pop" {
			t.Fatalf("Genre = %q, want Pop (the comment packet is not re-read)", tr.Genre)
		}
	})
	t.Run("opus keeps the last value", func(t *testing.T) {
		tr := extractBytesAs(t, "a.ogg", joinPages(oggLogicalStream(4, 255,
			append([]byte("OpusHead"), make([]byte, 11)...),
			opusCommentPacket("GENRE=Rock", "GENRE=Pop"), []byte{0, 1, 2})...))
		if tr.Genre != "Pop" {
			t.Fatalf("Genre = %q, want Pop (the comment packet is not re-read)", tr.Genre)
		}
	})
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
	writeMinimalFLACPairs(t, mkdir("A/01.flac"), 44100, 16, [][2]string{
		{"TITLE", "One"}, {"ALBUM", "Alpha"}, {"ARTIST", "X"}, {"TRACKNUMBER", "1"},
		{"GENRE", "Rock"}, {"GENRE", "Pop"},
	})
	writeMinimalFLACPairs(t, mkdir("A/02.flac"), 44100, 16, [][2]string{
		{"TITLE", "Two"}, {"ALBUM", "Alpha"}, {"ARTIST", "X"}, {"TRACKNUMBER", "2"},
		{"GENRE", "Rock"},
	})

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
