package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// The compilation flag on the wire (ExtractorVersion 17). The file's own
// TCMP / cpil / COMPILATION flag has long been read to synthesize
// AlbumArtist "Various Artists" for a flagged file with no album artist;
// v17 also reports it as Track.Compilation, so iOS can tell a compilation
// whose album artist is a DJ or a label from an ordinary album.

// A flagged FLAC whose album artist is a label keeps that album artist and
// still reports the flag — the case the synth never covered.
func TestCompilationFlag_ExplicitAlbumArtist_StillReachesTheWire(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sampler.flac")
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE":       "Track 1",
		"ARTIST":      "Performer A",
		"ALBUMARTIST": "Ministry of Sound",
		"ALBUM":       "The Annual",
		"COMPILATION": "1",
	})
	tr := &Track{Path: "sampler.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if !tr.Compilation {
		t.Errorf("Compilation = false, want true for COMPILATION=1")
	}
	if tr.AlbumArtist != "Ministry of Sound" {
		t.Errorf("AlbumArtist = %q, want the tagged %q (the synth only fills a blank one)",
			tr.AlbumArtist, "Ministry of Sound")
	}
}

// Only "1" means yes — the rule the synth has always used, and the one the
// iOS app's own enrichers apply, so a file reads the same on both paths.
func TestCompilationFlag_OnlyOneMeansYes(t *testing.T) {
	cases := []struct {
		name string
		tags map[string]string
		want bool
	}{
		{"one", map[string]string{"COMPILATION": "1"}, true},
		{"one padded", map[string]string{"COMPILATION": " 1 "}, true},
		{"zero", map[string]string{"COMPILATION": "0"}, false},
		{"true is not the convention", map[string]string{"COMPILATION": "true"}, false},
		{"absent", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tags := map[string]string{"TITLE": "T", "ARTIST": "A", "ALBUMARTIST": "AA", "ALBUM": "Al"}
			for k, v := range tc.tags {
				tags[k] = v
			}
			p := filepath.Join(t.TempDir(), "t.flac")
			writeMinimalFLAC(t, p, 44100, 16, tags)
			tr := &Track{Path: "t.flac", Size: 1, ModTime: time.Now()}
			if err := Extract(p, tr); err != nil {
				t.Fatal(err)
			}
			if tr.Compilation != tc.want {
				t.Errorf("Compilation = %v, want %v", tr.Compilation, tc.want)
			}
		})
	}
}

// ID3v2 carries the flag as TCMP — in a DSF's trailing tag and at the head
// of an MP3 alike, both through populateFromTagMetadata.
func TestCompilationFlag_ID3v2TCMP_DSFAndMP3(t *testing.T) {
	frames := map[string]string{"title": "T", "artist": "A", "album": "Al", "compilation": "1"}

	dsf := filepath.Join(t.TempDir(), "t.dsf")
	writeMinimalDSF(t, dsf, 2822400, frames)
	trDSF := &Track{Path: "t.dsf", Size: 1, ModTime: time.Now()}
	if err := Extract(dsf, trDSF); err != nil {
		t.Fatal(err)
	}
	if !trDSF.Compilation {
		t.Errorf("DSF: Compilation = false, want true for TCMP=1")
	}
	if trDSF.AlbumArtist != "Various Artists" {
		t.Errorf("DSF: AlbumArtist = %q, want the synthesized \"Various Artists\"", trDSF.AlbumArtist)
	}

	mp3 := filepath.Join(t.TempDir(), "t.mp3")
	writeMinimalMP3(t, mp3, frames)
	trMP3 := &Track{Path: "t.mp3", Size: 1, ModTime: time.Now()}
	if err := Extract(mp3, trMP3); err != nil {
		t.Fatal(err)
	}
	if !trMP3.Compilation {
		t.Errorf("MP3: Compilation = false, want true for TCMP=1")
	}
}

// MP4 carries it as the `cpil` atom, which dhowden surfaces as an INT, not a
// string — the shape stringOf coerces (bridge #166).
func TestCompilationFlag_MP4CpilAtom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value byte
		want  bool
	}{
		{"cpil 1", 1, true},
		{"cpil 0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := buildMP4WithILST(true,
				ilstText("\xa9nam", "T"),
				ilstText("\xa9ART", "Performer A"),
				ilstText("aART", "Ministry of Sound"),
				atomBytes("cpil", dataAtom(21, []byte{tc.value})),
			)
			tr := extractBytesAsM4A(t, raw)
			if tr.AlbumArtist != "Ministry of Sound" {
				t.Fatalf("premise: the ilst was read; AlbumArtist = %q", tr.AlbumArtist)
			}
			if tr.Compilation != tc.want {
				t.Errorf("Compilation = %v, want %v", tr.Compilation, tc.want)
			}
		})
	}
}

// Unflagged rows must serialize byte-identically to a pre-v17 row: that is
// what keeps the v17 re-extract's iOS delta to the flagged files alone.
func TestCompilationFlag_OmittedFromTheWireUnlessTrue(t *testing.T) {
	base := Track{Path: "a.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "T"}
	plain, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte(`"compilation"`)) {
		t.Errorf("an unflagged track serialized the key: %s", plain)
	}
	flagged := base
	flagged.Compilation = true
	out, err := json.Marshal(flagged)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"compilation":true`)) {
		t.Errorf("a flagged track did not serialize \"compilation\":true: %s", out)
	}
}

// The v17 upgrade, end to end: rows indexed by a pre-v17 binary (stale stamp,
// no compilation key) re-extract once. The FLAGGED row gains the key and its
// indexed_at advances — it is exactly what iOS must pull. The unflagged
// sibling re-extracts byte-identical and only gets stamped.
func TestScanner_V17_FlaggedRowJoinsTheDelta_PlainRowOnlyStamps(t *testing.T) {
	root := t.TempDir()
	writeMinimalFLAC(t, filepath.Join(root, "flagged.flac"), 44100, 16, map[string]string{
		"TITLE": "T1", "ARTIST": "Performer A", "ALBUMARTIST": "Ministry of Sound",
		"ALBUM": "The Annual", "DATE": "2000", "TRACKNUMBER": "1", "COMPILATION": "1",
	})
	writeMinimalFLAC(t, filepath.Join(root, "plain.flac"), 44100, 16, map[string]string{
		"TITLE": "T2", "ARTIST": "Band", "ALBUMARTIST": "Band",
		"ALBUM": "Own Album", "DATE": "2001", "TRACKNUMBER": "1",
	})
	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	flagged, err := store.GetTrack(ctx, "flagged.flac")
	if err != nil || flagged == nil || !flagged.Compilation {
		t.Fatalf("premise: the current extractor stores the flag; got %+v err=%v", flagged, err)
	}

	// What a v16 bridge left behind: the same rows, stamped 16, no key.
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 16, tags_json = json_remove(tags_json, '$.compilation') WHERE path = ?",
		"flagged.flac"); err != nil {
		t.Fatalf("munge flagged: %v", err)
	}
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 16 WHERE path = ?", "plain.flac"); err != nil {
		t.Fatalf("munge plain: %v", err)
	}
	if old, err := store.GetTrack(ctx, "flagged.flac"); err != nil || old == nil || old.Compilation {
		t.Fatalf("premise: the munged row reads as pre-v17; got %+v err=%v", old, err)
	}
	beforeFlagged := trackIndexedAt(t, store, "flagged.flac")
	beforePlain := trackIndexedAt(t, store, "plain.flac")

	scanOnce(t, sc, "v17")

	got, err := store.GetTrack(ctx, "flagged.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack(flagged): err=%v nil=%v", err, got == nil)
	}
	if !got.Compilation {
		t.Errorf("flagged row did not gain Compilation across the version-stale re-extract")
	}
	if after := trackIndexedAt(t, store, "flagged.flac"); after <= beforeFlagged {
		t.Errorf("flagged row's indexed_at did not advance (%d -> %d) — iOS would never pull the flag",
			beforeFlagged, after)
	}
	if after := trackIndexedAt(t, store, "plain.flac"); after != beforePlain {
		t.Errorf("unflagged row's indexed_at moved (%d -> %d) — v17 must not put every row in the delta",
			beforePlain, after)
	}
	for _, rel := range []string{"flagged.flac", "plain.flac"} {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}
