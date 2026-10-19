package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Backlog B446: a WAV's ID3 chunk carries its explicit advisory into the
// manifest. Mp3tag writes a WAV's tag as ID3v2.3 into the RIFF "id3 " chunk,
// with the advisory as TXXX:ITUNESADVISORY. The WAV walk hands that chunk to
// populateFromTagMetadata, which reads the TXXX by its description, and when
// the file's LIST/INFO title answers because the ID3 tag has none,
// containerText.applyUnder decides the flag again from the same TXXX and that
// title. TestExplicit_EmbeddedID3Chunks already read one shape of it (version
// 2.4, UTF-8, "id3 ", the value 1) from an empty Track. These read every
// shape through a real scan, which fills the path's guess before it extracts,
// and through WriteManifest, which writes the body /v1/manifest serves.

// unterminatedID3Text is id3Text without its terminator: the last string of a
// frame as the ID3v2 specification lays it out, where a frame's size ends it.
func unterminatedID3Text(enc byte, s string) []byte {
	b := id3Text(enc, s)
	if enc == 1 {
		return b[:len(b)-2]
	}
	return b[:len(b)-1]
}

// unterminatedTextFrame is a text frame of version holding s in encoding enc,
// with no terminator.
func unterminatedTextFrame(version, enc byte, id, s string) []byte {
	p := append([]byte{enc}, unterminatedID3Text(enc, s)...)
	return id3v2FrameBytes(version, id, uint32(len(p)), 0, p)
}

// unterminatedTXXX is a TXXX frame of version in encoding enc: desc and its
// terminator, then value with none, as the specification lays the frame out.
// txxxFrame is the same frame with the value terminated, as mutagen writes it.
func unterminatedTXXX(version, enc byte, desc, value string) []byte {
	p := append([]byte{enc}, id3Text(enc, desc)...)
	p = append(p, unterminatedID3Text(enc, value)...)
	return id3v2FrameBytes(version, "TXXX", uint32(len(p)), 0, p)
}

// advisoryTag is an ID3v2 tag of version whose text frames are in encoding enc
// with no terminator: a title (TIT2, left out when title is empty), an artist,
// an album and advisory, a TXXX frame, then 256 bytes of padding, which the
// header's size counts.
func advisoryTag(version, enc byte, title string, advisory []byte) []byte {
	var frames [][]byte
	if title != "" {
		frames = append(frames, unterminatedTextFrame(version, enc, "TIT2", title))
	}
	frames = append(frames,
		unterminatedTextFrame(version, enc, "TPE1", "Tagger Artist"),
		unterminatedTextFrame(version, enc, "TALB", "Tagger Album"),
		advisory)
	return id3v2TagBytes(version, 0, append(bytes.Join(frames, nil), make([]byte, 256)...))
}

// servedTracksByPath is what WriteManifest writes for store's root, decoded
// with each track's keys as the wire carries them, by path.
func servedTracksByPath(t *testing.T, store *Store, root string) map[string]map[string]any {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteManifest(context.Background(), &buf, store, []string{root}, time.Time{}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	var mf struct {
		Tracks []map[string]any `json:"tracks"`
	}
	if err := json.Unmarshal(buf.Bytes(), &mf); err != nil {
		t.Fatalf("decode the manifest: %v", err)
	}
	byPath := make(map[string]map[string]any, len(mf.Tracks))
	for _, tr := range mf.Tracks {
		if p, ok := tr["path"].(string); ok {
			byPath[p] = tr
		}
	}
	return byPath
}

// TestScanner_AWAVsID3ChunkCarriesItsAdvisoryIntoTheManifest is backlog B446's
// pin. A WAV whose ID3 chunk holds TXXX:ITUNESADVISORY=1 is explicit in the
// stored row and on the wire, in either ID3v2 version and each encoding a
// version defines for it (ISO-8859-1 and UTF-16 behind a byte order mark in
// 2.3, UTF-8 and UTF-16 in 2.4), with the value unterminated or terminated,
// and under either spelling of the chunk's id. A clean value (0 or 2) is not.
// Beside a LIST/INFO title, the ID3 title outranks it and the advisory still
// marks the row. Where the INFO title answers because the ID3 tag has no
// title, the advisory still decides, either chunk first, and the filename's
// marker no longer counts once that title replaced the stem. A WAV with no
// tag is not explicit. Each row's title says which branch named it, and every title
// differs, so no row is held back from the wire as another's duplicate.
func TestScanner_AWAVsID3ChunkCarriesItsAdvisoryIntoTheManifest(t *testing.T) {
	advisory := func(version, enc byte, value string) []byte {
		return unterminatedTXXX(version, enc, "ITUNESADVISORY", value)
	}
	id3 := func(tag []byte) []byte { return riffChunk("id3 ", tag) }
	files := []struct {
		name   string
		chunks [][]byte
		title  string // the title the row carries
		want   bool
	}{
		{"v23_utf16.wav", [][]byte{id3(advisoryTag(3, 1, "Two Three UTF-16", advisory(3, 1, "1")))},
			"Two Three UTF-16", true},
		{"v23_utf16_terminated.wav", [][]byte{id3(advisoryTag(3, 1, "Two Three Terminated",
			txxxFrame(3, 1, "ITUNESADVISORY", "1")))},
			"Two Three Terminated", true},
		{"v23_latin1.wav", [][]byte{id3(advisoryTag(3, 0, "Two Three Latin-1", advisory(3, 0, "1")))},
			"Two Three Latin-1", true},
		{"v24_utf8.wav", [][]byte{id3(advisoryTag(4, 3, "Two Four UTF-8", advisory(4, 3, "1")))},
			"Two Four UTF-8", true},
		{"v24_utf16.wav", [][]byte{id3(advisoryTag(4, 1, "Two Four UTF-16", advisory(4, 1, "1")))},
			"Two Four UTF-16", true},
		{"upper_v23.wav", [][]byte{riffChunk("ID3 ", advisoryTag(3, 1, "Upper Two Three", advisory(3, 1, "1")))},
			"Upper Two Three", true},
		{"upper_v24.wav", [][]byte{riffChunk("ID3 ", advisoryTag(4, 3, "Upper Two Four", advisory(4, 3, "1")))},
			"Upper Two Four", true},
		{"clean_0.wav", [][]byte{id3(advisoryTag(3, 1, "Clean Zero", advisory(3, 1, "0")))},
			"Clean Zero", false},
		{"clean_2.wav", [][]byte{id3(advisoryTag(3, 1, "Clean Two", advisory(3, 1, "2")))},
			"Clean Two", false},
		// LIST/INFO beside the ID3 chunk. The ID3 title answers where the tag
		// has one; the INFO title where it has none (applyUnder).
		{"info_and_id3.wav", [][]byte{listInfo("INAM", "Info Outranked"),
			id3(advisoryTag(3, 1, "ID3 Outranks", advisory(3, 1, "1")))},
			"ID3 Outranks", true},
		{"info_title.wav", [][]byte{listInfo("INAM", "Info Title"),
			id3(advisoryTag(3, 1, "", advisory(3, 1, "1")))},
			"Info Title", true},
		{"info_title_id3_first.wav", [][]byte{id3(advisoryTag(3, 1, "", advisory(3, 1, "1"))),
			listInfo("INAM", "Info Title Second")},
			"Info Title Second", true},
		{"info_title_clean.wav", [][]byte{listInfo("INAM", "Info Title Clean"),
			id3(advisoryTag(3, 1, "", advisory(3, 1, "0")))},
			"Info Title Clean", false},
		{"Marked [E].wav", [][]byte{listInfo("INAM", "Info Title Unmarked"),
			id3(advisoryTag(3, 1, "", advisory(3, 1, "0")))},
			"Info Title Unmarked", false},
		{"untagged.wav", nil, "untagged", false},
	}
	root := t.TempDir()
	rels := map[string]string{}
	for _, f := range files {
		rels[f.name] = writeAlone(t, root, f.name, wavWithChunks(f.chunks...))
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

// requireExplicitRow fails unless the row the scan stored at rel carries title
// and the explicit verdict want, and the manifest (wire, servedTracksByPath's
// answer) serves the row with "explicit": true when want is true and without
// the key when it is not.
func requireExplicitRow(t *testing.T, store *Store, wire map[string]map[string]any, rel, title string, want bool) {
	t.Helper()
	row := storedTrack(t, store, rel)
	if row.Title != title {
		t.Errorf("Title = %q, want %q", row.Title, title)
	}
	if row.Explicit != want {
		t.Errorf("the stored row's Explicit = %v, want %v", row.Explicit, want)
	}
	served, ok := wire[rel]
	if !ok {
		t.Fatalf("the manifest does not serve %s", rel)
	}
	got, present := served["explicit"]
	switch {
	case want && got != true:
		t.Errorf(`the manifest's "explicit" = %v (present %v), want true`, got, present)
	case !want && present:
		t.Errorf(`the manifest carries "explicit": %v, want the key absent`, got)
	}
}
