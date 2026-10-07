package manifest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ExtractorVersion 23's explicit mark. The bytes are built here: an MP4
// `rtng` as mutagen writes it (class 21, one value byte), the freeform
// ITUNESADVISORY and EXPLICIT atoms, a Vorbis comment, an ID3v2 TXXX of
// either description, and a marker on the raw track title. Any signal
// wins. The shared truth table is TestExplicitVerdict.

// One built ilst and the explicit verdict it must produce. The MP4 cases
// share this so each test states its atoms and its want, and the extract
// plus the comparison live once.
type mp4ExplicitCase struct {
	name  string
	items [][]byte
	want  bool
}

type flacExplicitCase struct {
	name string
	tags map[string]string
	want bool
}

func runMP4ExplicitCases(t *testing.T, cases []mp4ExplicitCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertMP4Explicit(t, tc.want, tc.items...)
		})
	}
}

func assertMP4Explicit(t *testing.T, want bool, items ...[]byte) *Track {
	t.Helper()
	got := extractBytesAsM4A(t, buildMP4WithILST(true, items...))
	if got.Explicit != want {
		t.Fatalf("Explicit = %v, want %v", got.Explicit, want)
	}
	return got
}

func runFLACExplicitCases(t *testing.T, cases []flacExplicitCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertFLACExplicit(t, tc.tags, tc.want)
		})
	}
}

// extractFLACTags writes a minimal FLAC, reads it back and extracts it.
// name is the file the extractor sees, so the extension stays the dispatch.
func extractFLACTags(t *testing.T, name string, tags map[string]string) Track {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	writeMinimalFLAC(t, p, 44100, 16, tags)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return requireBoundedExtraction(t, name, raw, &ExtractContext{})
}

func assertFLACExplicit(t *testing.T, tags map[string]string, want bool) Track {
	t.Helper()
	got := extractFLACTags(t, "t.flac", tags)
	if got.Explicit != want {
		t.Fatalf("Explicit = %v, want %v (title %q)", got.Explicit, want, got.Title)
	}
	return got
}

func assertID3AdvisoryExplicit(t *testing.T, desc, value string, want bool) {
	t.Helper()
	got := extractID3Frames(t, 4, txxxFrame(4, 3, desc, value))
	if got.Explicit != want {
		t.Fatalf("Explicit = %v, want %v", got.Explicit, want)
	}
}

func TestExplicit_MP4Rtng(t *testing.T) {
	rtng := func(b []byte) [][]byte {
		return [][]byte{atomBytes("rtng", dataAtom(21, b))}
	}
	runMP4ExplicitCases(t, []mp4ExplicitCase{
		{"1", rtng([]byte{1}), true},
		{"2", rtng([]byte{2}), false},
		{"4", rtng([]byte{4}), true},
		{"0", rtng([]byte{0}), false},
		{"padded", rtng([]byte{0, 0, 0, 1}), true},
		{"absent", nil, false},
	})
}

func TestExplicit_MP4Freeform(t *testing.T) {
	advisory := func(v string) [][]byte {
		return [][]byte{ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", v)}
	}
	runMP4ExplicitCases(t, []mp4ExplicitCase{
		{"1", advisory("1"), true},
		{"4", advisory("4"), true},
		{"2", advisory("2"), false},
		{"0", advisory("0"), false},
		{" 1 ", advisory(" 1 "), true},
		{"01", advisory("01"), false},
		{"true", advisory("true"), true},
	})
}

func TestExplicit_MP4AnySignalWins(t *testing.T) {
	freeform := func(v string) []byte {
		return ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", v)
	}
	rtng := func(n byte) []byte {
		return atomBytes("rtng", dataAtom(21, []byte{n}))
	}
	runMP4ExplicitCases(t, []mp4ExplicitCase{
		{"rtng2_does_not_cancel_advisory", [][]byte{rtng(2), freeform("1")}, true},
		{"rtng1_with_clean_advisory", [][]byte{rtng(1), freeform("2")}, true},
		{"rtng0_does_not_cancel_advisory", [][]byte{rtng(0), freeform("1")}, true},
		{"text_atom_is_not_a_rating", [][]byte{atomBytes("rtng", dataAtom(1, []byte("1"))), freeform("1")}, true},
	})
}

// The first value of one freeform name wins, whichever mean wrote it.
// dhowden drops a mean outside its allowlist, so an earlier clean value
// there must not lose to a later com.apple.iTunes value of the same name.
// A different name is a different signal.
func TestExplicit_MP4FirstFreeformValueWins(t *testing.T) {
	runMP4ExplicitCases(t, []mp4ExplicitCase{
		{"earlier_clean_mean_wins", [][]byte{
			ilstFreeform("org.example", "ITUNESADVISORY", "2"),
			ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", "1"),
		}, false},
		{"earlier_explicit_mean_wins", [][]byte{
			ilstFreeform("org.example", "ITUNESADVISORY", "1"),
			ilstFreeform("com.apple.iTunes", "ITUNESADVISORY", "2"),
		}, true},
		{"other_field_still_counts", [][]byte{
			ilstFreeform("org.example", "ITUNESADVISORY", "2"),
			ilstFreeform("org.example", "EXPLICIT", "1"),
		}, true},
	})
}

func TestExplicit_Vorbis(t *testing.T) {
	runFLACExplicitCases(t, []flacExplicitCase{
		{"1", map[string]string{"ITUNESADVISORY": "1"}, true},
		{"4", map[string]string{"ITUNESADVISORY": "4"}, true},
		{"2", map[string]string{"ITUNESADVISORY": "2"}, false},
		{"0", map[string]string{"ITUNESADVISORY": "0"}, false},
		{"absent", map[string]string{"TITLE": "T"}, false},
		{"trimmed", map[string]string{"ITUNESADVISORY": " 1 "}, true},
		{"odd_case", map[string]string{"ItunesAdvisory": "1"}, true},
		{"padded_digits", map[string]string{"ITUNESADVISORY": "01"}, false},
		{"true", map[string]string{"ITUNESADVISORY": "true"}, true},
	})
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
	// A different field is a different signal. A clean advisory does not
	// hide an explicit EXPLICIT frame.
	both := extractID3Frames(t, 4,
		txxxFrame(4, 3, "ITUNESADVISORY", "2"),
		txxxFrame(4, 3, "EXPLICIT", "1"))
	if !both.Explicit {
		t.Fatal("EXPLICIT did not win over a clean ITUNESADVISORY")
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
	for _, key := range []string{"ITUNESRATING", "rating"} {
		got := extractFLACTags(t, key+".flac", map[string]string{key: "1"})
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

func TestExplicit_AdvisoryText(t *testing.T) {
	values := []struct {
		name  string
		value string
		want  bool
	}{
		{"1", "1", true},
		{"4", "4", true},
		{"true", "true", true},
		{"YES", "YES", true},
		{"Explicit", "Explicit", true},
		{"e", "e", true},
		{"0", "0", false},
		{"2", "2", false},
		{"false", "false", false},
		{"no", "no", false},
		{"clean", "clean", false},
		{"empty", "", false},
		{"01", "01", false},
	}
	for _, field := range []struct {
		name string
		mean string
	}{
		{"ITUNESADVISORY", "com.apple.iTunes"},
		{"EXPLICIT", "org.example"},
	} {
		for _, tc := range values {
			t.Run("mp4/"+field.name+"/"+tc.name, func(t *testing.T) {
				assertMP4Explicit(t, tc.want, ilstFreeform(field.mean, field.name, tc.value))
			})
			t.Run("vorbis/"+field.name+"/"+tc.name, func(t *testing.T) {
				assertFLACExplicit(t, map[string]string{
					field.name: tc.value, "TITLE": "Plain",
				}, tc.want)
			})
			t.Run("id3/"+field.name+"/"+tc.name, func(t *testing.T) {
				assertID3AdvisoryExplicit(t, field.name, tc.value, tc.want)
			})
		}
	}
}

func TestExplicit_TitleMarker(t *testing.T) {
	runFLACExplicitCases(t, []flacExplicitCase{
		{"vorbis_[E]", map[string]string{"TITLE": "Song [E]"}, true},
		{"vorbis_[Explicit]", map[string]string{"TITLE": "Song [Explicit]"}, true},
		{"vorbis_[Explicit_Version]", map[string]string{"TITLE": "Song [Explicit Version]"}, true},
		{"vorbis_(Explicit)", map[string]string{"TITLE": "Song (Explicit)"}, true},
		{"vorbis_(Explicit_Version)", map[string]string{"TITLE": "Song (Explicit Version)"}, true},
		{"vorbis_mid_[E]", map[string]string{"TITLE": "Live [E] Cut"}, true},
		{"vorbis_case", map[string]string{"TITLE": "song [explicit]"}, true},
		{"vorbis_[Clean]", map[string]string{"TITLE": "Song [Clean]"}, false},
		{"vorbis_(Clean)", map[string]string{"TITLE": "Song (Clean)"}, false},
		{"vorbis_clean_versions", map[string]string{"TITLE": "Song [Clean Version]"}, false},
		{"vorbis_(Clean_Version)", map[string]string{"TITLE": "Song (Clean Version)"}, false},
		{"vorbis_bare_word", map[string]string{"TITLE": "The Explicit Song"}, false},
		{"vorbis_paren_mid", map[string]string{"TITLE": "Song (Explicit) Live"}, false},
		{"vorbis_album_marker", map[string]string{"TITLE": "Song", "ALBUM": "Hits [Explicit]"}, false},
		{"vorbis_advisory_with_plain_title", map[string]string{"TITLE": "Song", "ITUNESADVISORY": "1"}, true},
	})

	t.Run("mp4_nam", func(t *testing.T) {
		got := extractBytesAsM4A(t, buildMP4WithILST(true, ilstText("\xa9nam", "Cut [E]")))
		if !got.Explicit || got.Title != "Cut [E]" {
			t.Fatalf("title %q explicit %v", got.Title, got.Explicit)
		}
	})
	t.Run("id3_tit2", func(t *testing.T) {
		got := extractID3Frames(t, 4, textFrameBytes(4, "TIT2", "Live [Explicit Version]"))
		if !got.Explicit || got.Title != "Live [Explicit Version]" {
			t.Fatalf("title %q explicit %v", got.Title, got.Explicit)
		}
	})
	t.Run("mp4_rtng2_with_title", func(t *testing.T) {
		got := extractBytesAsM4A(t, buildMP4WithILST(true,
			atomBytes("rtng", dataAtom(21, []byte{2})),
			ilstText("\xa9nam", "Cut [E]"),
		))
		if !got.Explicit {
			t.Fatal("a clean rtng cancelled the title marker")
		}
	})
}

func TestExplicit_SharedM4AFixtures(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"explicit-rtng1.m4a", true},
		{"explicit-rtng2.m4a", false},
		{"explicit-rtng4.m4a", true},
		{"explicit-advisory1.m4a", true},
		{"explicit-rtng2-advisory1.m4a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFixture(t, tc.name)
			if got.Title != "One" || got.Artist != "Performer A" || got.Album != "The Record" {
				t.Fatalf("fixture tags = title %q artist %q album %q", got.Title, got.Artist, got.Album)
			}
			if got.Explicit != tc.want {
				t.Fatalf("Explicit = %v, want %v", got.Explicit, tc.want)
			}
		})
	}
}

func TestScanner_V23_ATitleMarkerJoinsTheDelta_APlainTitleOnlyStamps(t *testing.T) {
	root := t.TempDir()
	writeMinimalFLAC(t, filepath.Join(root, "marker.flac"), 44100, 16, map[string]string{
		"TITLE": "Song [E]", "ARTIST": "Band", "ALBUM": "Album",
	})
	writeMinimalFLAC(t, filepath.Join(root, "plain.flac"), 44100, 16, map[string]string{
		"TITLE": "The Explicit Song", "ARTIST": "Band", "ALBUM": "Album [Explicit]",
	})
	// No TITLE tag. fillFromPath names it from the file, and that guess
	// must not count as a title marker.
	writeMinimalFLAC(t, filepath.Join(root, "Song [E].flac"), 44100, 16, map[string]string{
		"ARTIST": "Band", "ALBUM": "Album",
	})
	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	marker, err := store.GetTrack(ctx, "marker.flac")
	if err != nil || marker == nil || !marker.Explicit || marker.Title != "Song [E]" {
		t.Fatalf("premise: the title marker is stored; got %+v err=%v", marker, err)
	}
	plain, err := store.GetTrack(ctx, "plain.flac")
	if err != nil || plain == nil || plain.Explicit {
		t.Fatalf("premise: a bare word and an album marker are not explicit; got %+v err=%v", plain, err)
	}
	guessed, err := store.GetTrack(ctx, "Song [E].flac")
	if err != nil || guessed == nil || guessed.Title != "Song [E]" || guessed.Explicit {
		t.Fatalf("premise: a path-derived title is not explicit; got %+v err=%v", guessed, err)
	}

	for _, rel := range []string{"marker.flac", "plain.flac", "Song [E].flac"} {
		if _, err := store.db.Exec(
			"UPDATE tracks SET extractor_version = 22, tags_json = json_remove(tags_json, '$.explicit') WHERE path = ?",
			rel); err != nil {
			t.Fatalf("munge %s: %v", rel, err)
		}
	}
	beforeMarker := trackIndexedAt(t, store, "marker.flac")
	beforePlain := trackIndexedAt(t, store, "plain.flac")
	beforeGuessed := trackIndexedAt(t, store, "Song [E].flac")

	scanOnce(t, sc, "v23")

	got, err := store.GetTrack(ctx, "marker.flac")
	if err != nil || got == nil || !got.Explicit {
		t.Fatalf("title-marker row did not gain Explicit; got %+v err=%v", got, err)
	}
	if after := trackIndexedAt(t, store, "marker.flac"); after <= beforeMarker {
		t.Errorf("title-marker row's indexed_at did not advance (%d -> %d)", beforeMarker, after)
	}
	if after := trackIndexedAt(t, store, "plain.flac"); after != beforePlain {
		t.Errorf("plain title's indexed_at moved (%d -> %d)", beforePlain, after)
	}
	if after := trackIndexedAt(t, store, "Song [E].flac"); after != beforeGuessed {
		t.Errorf("path-derived title's indexed_at moved (%d -> %d)", beforeGuessed, after)
	}
	for _, rel := range []string{"marker.flac", "plain.flac", "Song [E].flac"} {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}
