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

// cleanAdvisoryNoTitle is an ID3v2.3 tag with TXXX:ITUNESADVISORY=2 and no
// TIT2. A clean EXPLICIT=0 tag is the same shape under the other field name.
func cleanAdvisoryNoTitle(value string) []byte {
	return advisoryTag(3, 1, "", unterminatedTXXX(3, 1, "ITUNESADVISORY", value))
}

func cleanExplicitFieldNoTitle(value string) []byte {
	return advisoryTag(3, 1, "", unterminatedTXXX(3, 1, "EXPLICIT", value))
}

func cleanAdvisoryWithTitle(title, value string) []byte {
	return advisoryTag(3, 1, title, unterminatedTXXX(3, 1, "ITUNESADVISORY", value))
}

// TestScanner_AnID3ChunkDecidesAndTheStemStandsInForAnAbsentTitle is round 2
// of B466. The end-of-walk helper now runs only when no ID3 chunk was read;
// when one was, populateFromTagMetadata decides with the v23 rule: the
// filename stem stands in for an absent title, and any signal wins, so a
// clean advisory (ITUNESADVISORY=2 or EXPLICIT=0) with no TIT2 on an
// [E]-named file is explicit, as TestExplicitVerdict's
// clean_advisory_does_not_cancel_a_title says for a title. A clean advisory
// with no marker anywhere stays clean, and a WAV with no ID3 whose LIST/INFO
// title is clean stays clean (a title was read, so the stem does not count),
// also beside an untitled ID3 chunk, whose advisory still counts.
func TestScanner_AnID3ChunkDecidesAndTheStemStandsInForAnAbsentTitle(t *testing.T) {
	id3 := func(tag []byte) []byte { return riffChunk("id3 ", tag) }
	files := []struct {
		name  string
		data  []byte
		title string
		want  bool
	}{
		// Clean advisory, no TIT2, stem has [E]: the stem stands in for the
		// absent title and any signal wins.
		{"WavClean [E].wav", wavWithChunks(id3(cleanAdvisoryNoTitle("2"))), "WavClean [E]", true},
		{"AiffClean [E].aiff", buildAIFFWithID3(t, cleanAdvisoryNoTitle("2")), "AiffClean [E]", true},
		{"DffClean [E].dff", dffWithChunks(t, 2822400, dffChunk("ID3 ", cleanAdvisoryNoTitle("2"))), "DffClean [E]", true},
		{"DsfClean [E].dsf", minimalDSFBytes(2822400, cleanAdvisoryNoTitle("2")), "DsfClean [E]", true},
		{"WavExplicit0 [E].wav", wavWithChunks(id3(cleanExplicitFieldNoTitle("0"))), "WavExplicit0 [E]", true},
		// Clean advisory, no TIT2, no marker on the stem: clean.
		{"WavCleanPlain.wav", wavWithChunks(id3(cleanAdvisoryNoTitle("2"))), "WavCleanPlain", false},
		{"DsfCleanPlain.dsf", minimalDSFBytes(2822400, cleanAdvisoryNoTitle("2")), "DsfCleanPlain", false},
		// Clean advisory, TIT2 carries [E]: the title marker still wins.
		{"WavTIT2 [E].wav", wavWithChunks(id3(cleanAdvisoryWithTitle("Song [E]", "2"))), "Song [E]", true},
		{"AiffTIT2 [E].aiff", buildAIFFWithID3(t, cleanAdvisoryWithTitle("Song [E]", "2")), "Song [E]", true},
		{"DffTIT2 [E].dff", dffWithChunks(t, 2822400, dffChunk("ID3 ", cleanAdvisoryWithTitle("Song [E]", "2"))), "Song [E]", true},
		{"DsfTIT2 [E].dsf", minimalDSFBytes(2822400, cleanAdvisoryWithTitle("Song [E]", "2")), "Song [E]", true},
		// No ID3; LIST/INFO title is clean on a [E]-named file.
		{"InfoClean [E].wav", wavWithChunks(listInfo("INAM", "Clean Title")), "Clean Title", false},
		// An untitled ID3 chunk beside a LIST/INFO title: the title was read,
		// so applyUnder recomputes without the stem; the ID3's advisory still
		// counts.
		{"Id3NoTitleInfoClean [E].wav", wavWithChunks(id3(cleanAdvisoryNoTitle("2")), listInfo("INAM", "Clean Title")), "Clean Title", false},
		{"Id3ExplicitInfoClean [E].wav", wavWithChunks(id3(cleanAdvisoryNoTitle("1")), listInfo("INAM", "Clean Title")), "Clean Title", true},
	}
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
		before[rel] = rewindStemExplicitRowToV26(t, store, f.name, rel, f.title, f.want)
	}

	scanOnce(t, sc, "v27")

	for _, f := range files {
		rel := rels[f.name]
		assertStemExplicitRowUpgraded(t, store, f.name, rel, f.title, f.want, before[rel])
	}
}

func rewindStemExplicitRowToV26(t *testing.T, store *Store, name, rel, title string, want bool) int64 {
	t.Helper()
	if got := storedTrack(t, store, rel); got.Explicit != want || got.Title != title {
		t.Fatalf("premise: %s stores title %q Explicit %v, want %q / %v",
			name, got.Title, got.Explicit, title, want)
	}
	q := `UPDATE tracks SET extractor_version = 26, enriched_at = 1,
			tags_json = json_remove(tags_json, '$.explicit') WHERE path = ?`
	if _, err := store.db.Exec(q, rel); err != nil {
		t.Fatalf("rewind %s to v26: %v", name, err)
	}
	if got := storedTrack(t, store, rel); got.Explicit {
		t.Fatalf("premise: the rewound %s still carries Explicit", name)
	}
	return trackIndexedAt(t, store, rel)
}

func assertStemExplicitRowUpgraded(t *testing.T, store *Store, name, rel, title string, want bool, before int64) {
	t.Helper()
	got := storedTrack(t, store, rel)
	if got.Explicit != want || got.Title != title {
		t.Errorf("%s: title %q Explicit %v after the upgrade scan, want %q / %v",
			name, got.Title, got.Explicit, title, want)
	}
	if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
		t.Errorf("%s: extractor_version = %d, want %d", name, v, ExtractorVersion)
	}
	after := trackIndexedAt(t, store, rel)
	if want {
		if after <= before {
			t.Errorf("%s: indexed_at did not advance (%d -> %d): iOS would never pull Explicit",
				name, before, after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 0 {
			t.Errorf("%s: enriched_at = %d, want 0: the full upsert re-queues a changed row's enrichment",
				name, v)
		}
		return
	}
	if after != before {
		t.Errorf("%s: indexed_at moved (%d -> %d): a row whose Explicit stays must only be stamped",
			name, before, after)
	}
	if v := trackColumn(t, store, rel, "enriched_at"); v != 1 {
		t.Errorf("%s: enriched_at = %d: a row whose Explicit stays must not be re-enriched", name, v)
	}
}
