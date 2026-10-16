package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The values the work tests tag with: a work and a grouping a Picard-tagged
// classical file carries side by side, and a movement as the title.
const (
	picardWork     = "Symphony No. 5 in C minor, Op. 67"
	picardGrouping = "Beethoven Symphonies"
	picardMovement = "I. Allegro con brio"
)

// textFrameIn is a text frame (TIT1, GRP1, TIT2, …; TT1 and TT2 in version 2)
// in encoding enc holding values, each terminated as mutagen writes them
// (id3Text): mutagen, the library Picard writes with, ends every text value.
func textFrameIn(version, enc byte, id string, values ...string) []byte {
	p := []byte{enc}
	for _, v := range values {
		p = append(p, id3Text(enc, v)...)
	}
	return id3v2FrameBytes(version, id, uint32(len(p)), 0, p)
}

// id3v2MP3 is an MP3 whose ID3v2 tag of version holds frames.
func id3v2MP3(version byte, frames ...[]byte) []byte {
	return append(id3v2TagBytes(version, 0, bytes.Join(frames, nil)), mp3Audio()...)
}

// titleFrameIn is the title frame of version (TIT2, or TT2 in version 2)
// holding picardMovement, the premise every work case checks first.
func titleFrameIn(version, enc byte) []byte {
	if version == 2 {
		return textFrameIn(2, enc, "TT2", picardMovement)
	}
	return textFrameIn(version, enc, "TIT2", picardMovement)
}

// picardDefaultModeFrames is what MusicBrainz Picard writes for the work and
// the grouping by default (picard/formats/id3.py: _save_grouping_tag writes
// the grouping to TIT1, and the work falls through to _save_freetext_tag,
// which writes it to TXXX:WORK), after the title.
func picardDefaultModeFrames(version, enc byte) [][]byte {
	return [][]byte{
		titleFrameIn(version, enc),
		textFrameIn(version, enc, "TIT1", picardGrouping),
		txxxFrame(version, enc, "WORK", picardWork),
	}
}

// picardITunesModeFrames is what Picard writes with "Save iTunes compatible
// grouping and work" on: the grouping to GRP1 and the work to TIT1
// (_save_work_tag, which also deletes TXXX:WORK).
func picardITunesModeFrames(version, enc byte) [][]byte {
	return [][]byte{
		titleFrameIn(version, enc),
		textFrameIn(version, enc, "GRP1", picardGrouping),
		textFrameIn(version, enc, "TIT1", picardWork),
	}
}

// TestTXXXWorkIsTheWorkAndTIT1OnlyWhenThereIsNone is backlog B127's rule over
// MP3 tags built in the test. Picard writes an ID3v2 file's work to TXXX:WORK
// and its grouping to TIT1, unless "Save iTunes compatible grouping and work"
// is on, which puts the work in TIT1 and the grouping in GRP1; the bridge read
// the work from TIT1 alone, so a default-mode file stored its grouping as its
// work. A TXXX (TXX in version 2.2) whose description is WORK, matched as the
// B116 named values are (case folded, spaces as underscores), is the work, the
// first such frame with a value winning; one whose value is empty after
// trimming counts as absent; and only with no TXXX:WORK is TIT1 (TT1) the work.
// Each case runs twenty times, as the B116 cases do: dhowden's raw map is a Go
// map, and a winner picked by ranging it changes between extractions.
func TestTXXXWorkIsTheWorkAndTIT1OnlyWhenThereIsNone(t *testing.T) {
	const mass, requiem = "Mass in B minor, BWV 232", "Requiem in D minor, K. 626"
	bom := string(rune(0xFEFF)) // a byte order mark, as dhowden leaves one in a later UTF-16 value
	for _, c := range []struct {
		name    string
		version byte
		frames  [][]byte
		want    string
	}{
		{
			name:    "Picard's default mode, version 2.4 in UTF-8: the grouping in TIT1, the work in TXXX:WORK",
			version: 4,
			frames:  picardDefaultModeFrames(4, 3),
			want:    picardWork,
		},
		{
			name:    "Picard's default mode, version 2.4 in UTF-16",
			version: 4,
			frames:  picardDefaultModeFrames(4, 1),
			want:    picardWork,
		},
		{
			name:    "Picard's default mode, version 2.3 in UTF-16",
			version: 3,
			frames:  picardDefaultModeFrames(3, 1),
			want:    picardWork,
		},
		{
			name:    "TXXX:WORK ahead of TIT1 in the tag",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "WORK", picardWork),
				textFrameIn(4, 3, "TIT1", picardGrouping),
			},
			want: picardWork,
		},
		{
			name:    "the iTunes-compatible mode, version 2.4: the work in TIT1, the grouping in GRP1",
			version: 4,
			frames:  picardITunesModeFrames(4, 3),
			want:    picardWork,
		},
		{
			name:    "the iTunes-compatible mode, version 2.3 in UTF-16",
			version: 3,
			frames:  picardITunesModeFrames(3, 1),
			want:    picardWork,
		},
		{
			name:    "TXXX:WORK alone",
			version: 4,
			frames:  [][]byte{titleFrameIn(4, 3), txxxFrame(4, 3, "WORK", picardWork)},
			want:    picardWork,
		},
		{
			name:    "with no TXXX:WORK, TIT1 is the work, whatever the tagger meant by it",
			version: 4,
			frames:  [][]byte{titleFrameIn(4, 3), textFrameIn(4, 3, "TIT1", picardGrouping)},
			want:    picardGrouping,
		},
		{
			name:    "version 2.2: TXX:WORK beside TT1",
			version: 2,
			frames: [][]byte{
				titleFrameIn(2, 0),
				textFrameIn(2, 0, "TT1", picardGrouping),
				txxxFrame(2, 0, "WORK", picardWork),
			},
			want: picardWork,
		},
		{
			name:    "version 2.2: TT1 alone is the work",
			version: 2,
			frames:  [][]byte{titleFrameIn(2, 0), textFrameIn(2, 0, "TT1", picardWork)},
			want:    picardWork,
		},
		{
			name:    "version 2.2: TXX:WORK alone",
			version: 2,
			frames:  [][]byte{titleFrameIn(2, 1), txxxFrame(2, 1, "WORK", picardWork)},
			want:    picardWork,
		},
		{
			name:    "a TXXX:WORK whose value is empty is no work, so TIT1 is",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "WORK", ""),
				txxxFrame(4, 1, "WORK", " "+bom+" \t"),
				textFrameIn(4, 3, "TIT1", picardGrouping),
			},
			want: picardGrouping,
		},
		{
			name:    "an empty TXXX:WORK, then one with a value",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "WORK", "  "),
				textFrameIn(4, 3, "TIT1", picardGrouping),
				txxxFrame(4, 3, "WORK", picardWork),
			},
			want: picardWork,
		},
		{
			name:    "two TXXX:WORK frames: the first in the tag",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "WORK", mass),
				textFrameIn(4, 3, "TIT1", picardGrouping),
				txxxFrame(4, 3, "WORK", requiem),
			},
			want: mass,
		},
		{
			name:    "two TXXX:WORK frames: the first in the tag, the other way round",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "WORK", requiem),
				textFrameIn(4, 3, "TIT1", picardGrouping),
				txxxFrame(4, 3, "WORK", mass),
			},
			want: requiem,
		},
		{
			name:    "two TXXX:WORK frames and no TIT1, version 2.3 in UTF-16",
			version: 3,
			frames: [][]byte{
				titleFrameIn(3, 1),
				txxxFrame(3, 1, "WORK", requiem),
				txxxFrame(3, 1, "WORK", mass),
			},
			want: requiem,
		},
		{
			name:    "the description is matched without regard to case: Picard's obsolete Work",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				textFrameIn(4, 3, "TIT1", picardGrouping),
				txxxFrame(4, 3, "Work", picardWork),
			},
			want: picardWork,
		},
		{
			name:    "the description is matched without regard to case: work",
			version: 3,
			frames: [][]byte{
				titleFrameIn(3, 1),
				textFrameIn(3, 1, "TIT1", picardGrouping),
				txxxFrame(3, 1, "work", picardWork),
			},
			want: picardWork,
		},
		{
			name:    "a TXXX of another name beside TIT1 is not the work",
			version: 4,
			frames: [][]byte{
				titleFrameIn(4, 3),
				txxxFrame(4, 3, "MusicBrainz Work Id", "1f3c4a2b-5d6e-4f70-8a91-b2c3d4e5f607"),
				txxxFrame(4, 3, "WORK TITLE", mass),
				txxxFrame(4, 3, "WORK ", requiem),
				txxxFrame(4, 3, "GROUPING", "x"),
				textFrameIn(4, 3, "TIT1", picardGrouping),
			},
			want: picardGrouping,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			for run := range 20 {
				tr := extractID3Frames(t, c.version, c.frames...)
				if tr.Title != picardMovement {
					t.Fatalf("premise: extraction %d read Title %q, want the tag's %q", run+1, tr.Title, picardMovement)
				}
				if tr.Work != c.want {
					t.Fatalf("extraction %d read Work %q, want %q", run+1, tr.Work, c.want)
				}
			}
		})
	}
}

// TestPicardsWorkIsReadInEveryContainerAnID3v2TagReaches holds the rule in
// every container whose ID3v2 tag populateFromTagMetadata reads: an MP3, a
// DSF (the tag at its metadata pointer), an AIFF's "ID3 " chunk, a WAV's
// "id3 " chunk, a DSDIFF file's "ID3 " chunk (at the root, where mutagen and so
// Picard put it, and nested in PROP, where TagLib reads one), and a FLAC whose
// first bytes are a prepended ID3v2 tag, which dhowden reads in place of its
// Vorbis comments. Picard tags the first five through one class
// (picard/formats/id3.py's ID3File), in both of its modes.
func TestPicardsWorkIsReadInEveryContainerAnID3v2TagReaches(t *testing.T) {
	for _, mode := range []struct {
		name   string
		frames [][]byte
	}{
		{"the default mode", picardDefaultModeFrames(4, 3)},
		{"the iTunes-compatible mode", picardITunesModeFrames(4, 3)},
	} {
		tag := id3v2TagBytes(4, 0, bytes.Join(mode.frames, nil))
		for _, c := range []struct {
			name, file string
			data       []byte
		}{
			{"an MP3", "x.mp3", append(append([]byte{}, tag...), mp3Audio()...)},
			{"a DSF", "x.dsf", dsfWithID3(tag)},
			{"an AIFF", "x.aiff", buildAIFFWithID3(t, tag)},
			{"a WAV", "x.wav", buildWAVWithID3(t, tag)},
			{"a DSDIFF file, the tag at the root", "x.dff", dffWithChunks(t, 2822400, dffChunk("ID3 ", tag))},
			{"a DSDIFF file, the tag in PROP", "x.dff", dffWithPropAndChunks(t, 2822400, dffChunk("ID3 ", tag))},
		} {
			t.Run(mode.name+"/"+c.name, func(t *testing.T) {
				tr := requireBoundedExtraction(t, c.file, c.data, &ExtractContext{})
				if tr.Title != picardMovement {
					t.Fatalf("premise: Title = %q, want the tag's %q", tr.Title, picardMovement)
				}
				if tr.Work != picardWork {
					t.Errorf("Work = %q, want %q (the grouping is %q)", tr.Work, picardWork, picardGrouping)
				}
			})
		}
		t.Run(mode.name+"/a FLAC behind a prepended ID3v2 tag", func(t *testing.T) {
			// The base FLAC's Vorbis comment holds no WORK, so the Vorbis join
			// leaves the work the ID3v2 tag gave.
			if tr := extractFLACBehind(t, tag); tr.Work != picardWork {
				t.Errorf("Work = %q, want %q (the grouping is %q)", tr.Work, picardWork, picardGrouping)
			}
		})
	}
}

// TestScanner_V26_ATXXXWorkJoinsTheDelta_AWorkThatStaysOnlyStamps is the v26
// upgrade end to end. Rows a v25 bridge indexed re-extract once. A Picard
// default-mode MP3, which v25 stored with its grouping as its work, and a
// version 2.2 MP3 whose TT1 v25 never read, take the full-upsert leg: they
// gain their work, their indexed_at advances (the iOS delta) and their
// enriched_at goes back to 0. A Picard iTunes-mode MP3, an MP3 tagged with
// TIT1 alone (writeMinimalMP3's "work"), a plain MP3 and a FLAC with a Vorbis
// WORK re-extract byte-identical and are only stamped.
func TestScanner_V26_ATXXXWorkJoinsTheDelta_AWorkThatStaysOnlyStamps(t *testing.T) {
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("default.mp3", id3v2MP3(4, picardDefaultModeFrames(4, 3)...))
	write("itunes.mp3", id3v2MP3(4, picardITunesModeFrames(4, 3)...))
	write("v22.mp3", id3v2MP3(2, titleFrameIn(2, 0), textFrameIn(2, 0, "TT1", picardWork)))
	writeMinimalMP3(t, filepath.Join(root, "tit1.mp3"), map[string]string{"title": "Kyrie", "work": "Mass"})
	writeMinimalMP3(t, filepath.Join(root, "plain.mp3"), map[string]string{"title": "Plain", "artist": "Band", "album": "Own Album"})
	writeMinimalFLAC(t, filepath.Join(root, "vorbis.flac"), 44100, 16, map[string]string{"TITLE": "Agnus Dei", "WORK": "Requiem"})
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	rows := []struct {
		rel, v25, want string
	}{
		{"default.mp3", picardGrouping, picardWork},
		{"v22.mp3", "", picardWork},
		{"itunes.mp3", picardWork, picardWork},
		{"tit1.mp3", "Mass", "Mass"},
		{"plain.mp3", "", ""},
		{"vorbis.flac", "Requiem", "Requiem"},
	}
	for _, r := range rows {
		if got := storedTrack(t, store, r.rel).Work; got != r.want {
			t.Fatalf("premise: the current extractor stores %s's work as %q, want %q", r.rel, got, r.want)
		}
	}
	before := map[string]int64{}
	for _, r := range rows {
		q, args := "UPDATE tracks SET extractor_version = 25, enriched_at = 1 WHERE path = ?", []any{r.rel}
		switch {
		case r.v25 == r.want:
		case r.v25 == "":
			q = `UPDATE tracks SET extractor_version = 25, enriched_at = 1,
				tags_json = json_remove(tags_json, '$.work') WHERE path = ?`
		default:
			q = `UPDATE tracks SET extractor_version = 25, enriched_at = 1,
				tags_json = json_set(tags_json, '$.work', ?) WHERE path = ?`
			args = []any{r.v25, r.rel}
		}
		if _, err := store.db.Exec(q, args...); err != nil {
			t.Fatalf("rewind %s to v25: %v", r.rel, err)
		}
		if got := storedTrack(t, store, r.rel).Work; got != r.v25 {
			t.Fatalf("premise: the rewound %s stores work %q, want v25's %q", r.rel, got, r.v25)
		}
		before[r.rel] = trackIndexedAt(t, store, r.rel)
	}

	scanOnce(t, sc, "v26")

	for _, r := range rows {
		if got := storedTrack(t, store, r.rel).Work; got != r.want {
			t.Errorf("%s: Work = %q after the upgrade scan, want %q", r.rel, got, r.want)
		}
		if v := trackColumn(t, store, r.rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", r.rel, v, ExtractorVersion)
		}
		after := trackIndexedAt(t, store, r.rel)
		if r.v25 != r.want {
			if after <= before[r.rel] {
				t.Errorf("%s: indexed_at did not advance (%d -> %d): iOS would never pull its work", r.rel, before[r.rel], after)
			}
			if v := trackColumn(t, store, r.rel, "enriched_at"); v != 0 {
				t.Errorf("%s: enriched_at = %d, want 0: the full upsert re-queues a changed row's enrichment", r.rel, v)
			}
			continue
		}
		if after != before[r.rel] {
			t.Errorf("%s: indexed_at moved (%d -> %d): a row whose work stays must only be stamped", r.rel, before[r.rel], after)
		}
		if v := trackColumn(t, store, r.rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: a row whose work stays must not be re-enriched", r.rel, v)
		}
	}
}
