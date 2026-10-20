package manifest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Backlog B466: the AIFF, WAV, DFF and DSF extractors never applied the
// filename-stem explicit marker when they read no ID3 tag (or, for WAV, no
// LIST/INFO title). readDhowdenTags does on its no-tags branch, and the v23
// rule says the stem counts when no title tag was read. These cases go through
// a real scan (fillFromPath before extract) and WriteManifest.

// dsfNoID3 is a minimal DSF whose metadata pointer is 0: no ID3 tag at all.
func dsfNoID3(sampleRate uint32) []byte {
	var fmtChunk [52]byte
	copy(fmtChunk[0:4], []byte("fmt "))
	binary.LittleEndian.PutUint64(fmtChunk[4:12], 52)
	binary.LittleEndian.PutUint32(fmtChunk[12:16], 1)
	binary.LittleEndian.PutUint32(fmtChunk[16:20], 0)
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 2)
	binary.LittleEndian.PutUint32(fmtChunk[24:28], 2)
	binary.LittleEndian.PutUint32(fmtChunk[28:32], sampleRate)
	binary.LittleEndian.PutUint32(fmtChunk[32:36], 1)
	binary.LittleEndian.PutUint64(fmtChunk[36:44], uint64(sampleRate*5))
	binary.LittleEndian.PutUint32(fmtChunk[44:48], 4096)

	var dataHeader [12]byte
	copy(dataHeader[0:4], []byte("data"))
	binary.LittleEndian.PutUint64(dataHeader[4:12], 12)

	totalSize := uint64(28 + 52 + 12)
	var dsd [28]byte
	copy(dsd[0:4], []byte("DSD "))
	binary.LittleEndian.PutUint64(dsd[4:12], 28)
	binary.LittleEndian.PutUint64(dsd[12:20], totalSize)
	// metadata pointer 0: no ID3 tag.
	binary.LittleEndian.PutUint64(dsd[20:28], 0)

	var buf bytes.Buffer
	buf.Write(dsd[:])
	buf.Write(fmtChunk[:])
	buf.Write(dataHeader[:])
	return buf.Bytes()
}

// stemExplicitFixtureFiles are the untagged chunk-walk cases B466 heals, plus
// a plain WAV that must only stamp across the v27 bump.
func stemExplicitFixtureFiles(t *testing.T) []struct {
	name  string
	data  []byte
	title string
	want  bool
} {
	t.Helper()
	return []struct {
		name  string
		data  []byte
		title string
		want  bool
	}{
		{"Song [E].wav", wavWithChunks(), "Song [E]", true},
		{"Aiff [E].aiff", buildAIFFWithID3(t, nil), "Aiff [E]", true},
		{"Dff [E].dff", dffWithChunks(t, 2822400), "Dff [E]", true},
		{"Dsf [E].dsf", dsfNoID3(2822400), "Dsf [E]", true},
		// LIST/INFO has an artist, no title: the stem stays and must count.
		{"Info [E].wav", wavWithChunks(listInfo("IART", "Info Artist")), "Info [E]", true},
		{"plain.wav", wavWithChunks(), "plain", false},
	}
}

// TestScanner_AStemMarkerCountsWhenTheChunkWalkReadsNoTitle is backlog B466's
// pin. A WAV, AIFF, DFF or DSF named with [E] and carrying no ID3 tag (and a
// WAV whose LIST/INFO has an artist but no title) stores Explicit true from
// the filename stem, as a FLAC with no TITLE tag does. A plain untagged WAV
// stays unset. Each file sits alone under writeAlone so the path's guess is
// the stem and no sibling unifies its album.
func TestScanner_AStemMarkerCountsWhenTheChunkWalkReadsNoTitle(t *testing.T) {
	files := stemExplicitFixtureFiles(t)
	root := t.TempDir()
	rels := map[string]string{}
	for _, f := range files {
		rels[f.name] = writeAlone(t, root, f.name, f.data)
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	wire := servedTracksByPath(t, store, root)
	for _, f := range files {
		t.Run(f.name, func(t *testing.T) {
			requireExplicitRow(t, store, wire, rels[f.name], f.title, f.want)
		})
	}
}

// TestScanner_V27_AStemMarkerOnAChunkWalkJoinsTheDelta_APlainFileOnlyStamps is
// the v27 upgrade end to end. Rows a v26 bridge indexed re-extract once. An
// untagged WAV/AIFF/DFF/DSF whose filename stem carries [E], and a WAV whose
// LIST/INFO has no title, gain Explicit, advance indexed_at (the iOS delta)
// and reset enriched_at. A plain untagged WAV re-extracts byte-identical and
// is only stamped.
func TestScanner_V27_AStemMarkerOnAChunkWalkJoinsTheDelta_APlainFileOnlyStamps(t *testing.T) {
	files := stemExplicitFixtureFiles(t)
	root := t.TempDir()
	rels := map[string]string{}
	for _, f := range files {
		rels[f.name] = writeAlone(t, root, f.name, f.data)
	}
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	before := map[string]int64{}
	for _, f := range files {
		rel := rels[f.name]
		if got := storedTrack(t, store, rel); got.Explicit != f.want || got.Title != f.title {
			t.Fatalf("premise: %s stores title %q Explicit %v, want %q / %v",
				f.name, got.Title, got.Explicit, f.title, f.want)
		}
		q := `UPDATE tracks SET extractor_version = 26, enriched_at = 1,
			tags_json = json_remove(tags_json, '$.explicit') WHERE path = ?`
		if _, err := store.db.Exec(q, rel); err != nil {
			t.Fatalf("rewind %s to v26: %v", f.name, err)
		}
		if got := storedTrack(t, store, rel); got.Explicit {
			t.Fatalf("premise: the rewound %s still carries Explicit", f.name)
		}
		before[rel] = trackIndexedAt(t, store, rel)
	}

	scanOnce(t, sc, "v27")

	for _, f := range files {
		rel := rels[f.name]
		got := storedTrack(t, store, rel)
		if got.Explicit != f.want || got.Title != f.title {
			t.Errorf("%s: title %q Explicit %v after the upgrade scan, want %q / %v",
				f.name, got.Title, got.Explicit, f.title, f.want)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", f.name, v, ExtractorVersion)
		}
		after := trackIndexedAt(t, store, rel)
		if f.want {
			if after <= before[rel] {
				t.Errorf("%s: indexed_at did not advance (%d -> %d): iOS would never pull Explicit",
					f.name, before[rel], after)
			}
			if v := trackColumn(t, store, rel, "enriched_at"); v != 0 {
				t.Errorf("%s: enriched_at = %d, want 0: the full upsert re-queues a changed row's enrichment",
					f.name, v)
			}
			continue
		}
		if after != before[rel] {
			t.Errorf("%s: indexed_at moved (%d -> %d): a row whose Explicit stays must only be stamped",
				f.name, before[rel], after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d: a row whose Explicit stays must not be re-enriched", f.name, v)
		}
	}
}
