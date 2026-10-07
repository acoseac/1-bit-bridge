package manifest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExtractorVersion 23's iTunes content advisory. The bytes are built here:
// an MP4 `rtng` as mutagen writes it (class 21, one value byte), the freeform
// ITUNESADVISORY atom, a Vorbis comment, and an ID3v2 TXXX of that
// description. 1 and 4 are explicit; 2 is clean and 0 is none.

func TestExplicit_MP4Rtng(t *testing.T) {
	cases := []struct {
		name string
		item []byte
		want bool
	}{
		{"1", atomBytes("rtng", dataAtom(21, []byte{1})), true},
		{"2", atomBytes("rtng", dataAtom(21, []byte{2})), false},
		{"4", atomBytes("rtng", dataAtom(21, []byte{4})), true},
		{"0", atomBytes("rtng", dataAtom(21, []byte{0})), false},
		{"padded", atomBytes("rtng", dataAtom(21, []byte{0, 0, 0, 1})), true},
		{"absent", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var items [][]byte
			if tc.item != nil {
				items = [][]byte{tc.item}
			}
			got := extractBytesAsM4A(t, buildMP4WithILST(true, items...))
			if got.Explicit != tc.want {
				t.Fatalf("Explicit = %v, want %v", got.Explicit, tc.want)
			}
		})
	}
}

func TestExplicit_MP4Freeform(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"1", true},
		{"4", true},
		{"2", false},
		{"0", false},
		{" 1 ", true},
		{"01", false},
		{"true", false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			item := ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", tc.value)
			got := extractBytesAsM4A(t, buildMP4WithILST(true, item))
			if got.Explicit != tc.want {
				t.Fatalf("Explicit = %v, want %v", got.Explicit, tc.want)
			}
		})
	}
}

func TestExplicit_MP4RtngWinsOverTheFreeform(t *testing.T) {
	freeform := func(v string) []byte {
		return ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", v)
	}
	rtng := func(n byte) []byte {
		return atomBytes("rtng", dataAtom(21, []byte{n}))
	}
	cases := []struct {
		name  string
		items [][]byte
		want  bool
	}{
		{"clean_beats_freeform", [][]byte{rtng(2), freeform("1")}, false},
		{"explicit_beats_freeform_clean", [][]byte{rtng(1), freeform("2")}, true},
		{"zero_beats_freeform", [][]byte{rtng(0), freeform("1")}, false},
		{"text_atom_is_not_a_rating", [][]byte{atomBytes("rtng", dataAtom(1, []byte("1"))), freeform("1")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractBytesAsM4A(t, buildMP4WithILST(true, tc.items...))
			if got.Explicit != tc.want {
				t.Fatalf("Explicit = %v, want %v", got.Explicit, tc.want)
			}
		})
	}
}

func TestExplicit_Vorbis(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"1", map[string]string{"ITUNESADVISORY": "1"}, true},
		{"4", map[string]string{"ITUNESADVISORY": "4"}, true},
		{"2", map[string]string{"ITUNESADVISORY": "2"}, false},
		{"0", map[string]string{"ITUNESADVISORY": "0"}, false},
		{"absent", map[string]string{"TITLE": "T"}, false},
		{"trimmed", map[string]string{"ITUNESADVISORY": " 1 "}, true},
		{"odd_case", map[string]string{"ItunesAdvisory": "1"}, true},
		{"padded_digits", map[string]string{"ITUNESADVISORY": "01"}, false},
		{"word", map[string]string{"ITUNESADVISORY": "true"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "t.flac")
			writeMinimalFLAC(t, p, 44100, 16, tc.tags)
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			got := requireBoundedExtraction(t, "t.flac", raw, &ExtractContext{})
			if got.Explicit != tc.want {
				t.Fatalf("Explicit = %v, want %v", got.Explicit, tc.want)
			}
		})
	}
}

func TestExplicit_ID3TXXX(t *testing.T) {
	one := func(desc, value string) Track {
		t.Helper()
		return extractID3Frames(t, 4, txxxFrame(4, 3, desc, value))
	}
	if !one("ITUNESADVISORY", "1").Explicit {
		t.Fatal(`TXXX "1" is not explicit`)
	}
	if !one("ITUNESADVISORY", "4").Explicit {
		t.Fatal(`TXXX "4" is not explicit`)
	}
	if one("ITUNESADVISORY", "2").Explicit {
		t.Fatal(`TXXX "2" is explicit`)
	}
	if one("ITUNESADVISORY", "0").Explicit {
		t.Fatal(`TXXX "0" is explicit`)
	}
	if extractID3Frames(t, 4).Explicit {
		t.Fatal("a tag with no TXXX is explicit")
	}
	if !one("ItunesAdvisory", "1").Explicit {
		t.Fatal("the description is case-sensitive")
	}
	// The first TXXX in the tag wins.
	first := extractID3Frames(t, 4,
		txxxFrame(4, 3, "ITUNESADVISORY", "2"),
		txxxFrame(4, 3, "ITUNESADVISORY", "1"))
	if first.Explicit {
		t.Fatal("the second TXXX overrode the first")
	}
}

func TestExplicit_EmbeddedID3Chunks(t *testing.T) {
	id3 := id3v2TagBytes(4, 0, txxxFrame(4, 3, "ITUNESADVISORY", "1"))
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"wav", buildWAVWithID3(t, id3)},
		{"aiff", buildAIFFWithID3(t, id3)},
		{"dsf", minimalDSFBytes(2822400, id3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := requireBoundedExtraction(t, "x."+tc.name, tc.data, &ExtractContext{})
			if !got.Explicit {
				t.Fatalf("%s embedded TXXX did not mark the track explicit", tc.name)
			}
		})
	}
}

func TestExplicit_OmittedFromTheWireUnlessTrue(t *testing.T) {
	flagged, err := json.Marshal(Track{Explicit: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(flagged), `"explicit":true`) {
		t.Fatalf("flagged track JSON = %s", flagged)
	}
	plain, err := json.Marshal(Track{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), `"explicit"`) {
		t.Fatalf("unflagged track JSON carries the key: %s", plain)
	}
}

func TestExplicit_OtherSpellingsAreNotRead(t *testing.T) {
	for _, key := range []string{"EXPLICIT", "ITUNESRATING", "rating"} {
		p := filepath.Join(t.TempDir(), key+".flac")
		writeMinimalFLAC(t, p, 44100, 16, map[string]string{key: "1"})
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		got := requireBoundedExtraction(t, key+".flac", raw, &ExtractContext{})
		if got.Explicit {
			t.Fatalf("%s=1 marked the track explicit", key)
		}
	}
}

func TestScanner_V23_AnExplicitRowJoinsTheDelta_APlainRowOnlyStamps(t *testing.T) {
	root := t.TempDir()
	writeMinimalFLAC(t, filepath.Join(root, "explicit.flac"), 44100, 16, map[string]string{
		"TITLE": "T1", "ARTIST": "Band", "ALBUM": "Album", "ITUNESADVISORY": "1",
	})
	writeMinimalFLAC(t, filepath.Join(root, "plain.flac"), 44100, 16, map[string]string{
		"TITLE": "T2", "ARTIST": "Band", "ALBUM": "Album",
	})
	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	flagged, err := store.GetTrack(ctx, "explicit.flac")
	if err != nil || flagged == nil || !flagged.Explicit {
		t.Fatalf("premise: the current extractor stores the flag; got %+v err=%v", flagged, err)
	}

	// What a v22 bridge left behind: the same rows, stamped 22, no key.
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 22, tags_json = json_remove(tags_json, '$.explicit') WHERE path = ?",
		"explicit.flac"); err != nil {
		t.Fatalf("munge explicit: %v", err)
	}
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 22 WHERE path = ?", "plain.flac"); err != nil {
		t.Fatalf("munge plain: %v", err)
	}
	if old, err := store.GetTrack(ctx, "explicit.flac"); err != nil || old == nil || old.Explicit {
		t.Fatalf("premise: the munged row reads as pre-v23; got %+v err=%v", old, err)
	}
	beforeFlagged := trackIndexedAt(t, store, "explicit.flac")
	beforePlain := trackIndexedAt(t, store, "plain.flac")

	scanOnce(t, sc, "v23")

	got, err := store.GetTrack(ctx, "explicit.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack(explicit): err=%v nil=%v", err, got == nil)
	}
	if !got.Explicit {
		t.Errorf("explicit row did not gain Explicit across the version-stale re-extract")
	}
	if after := trackIndexedAt(t, store, "explicit.flac"); after <= beforeFlagged {
		t.Errorf("explicit row's indexed_at did not advance (%d -> %d) — iOS would never pull the flag",
			beforeFlagged, after)
	}
	if after := trackIndexedAt(t, store, "plain.flac"); after != beforePlain {
		t.Errorf("unflagged row's indexed_at moved (%d -> %d) — v23 must not put every row in the delta",
			beforePlain, after)
	}
	for _, rel := range []string{"explicit.flac", "plain.flac"} {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}
