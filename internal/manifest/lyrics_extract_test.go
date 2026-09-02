package manifest

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// id3Frame is a raw ID3v2.3 frame for fixtures buildID3v2_3 can't express
// (USLT / SYLT carry binary bodies, not plain text).
type id3Frame struct {
	id   string
	data []byte
}

func id3v23Raw(frames []id3Frame) []byte {
	var body bytes.Buffer
	for _, f := range frames {
		body.WriteString(f.id)
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(len(f.data)))
		body.Write(sz[:])
		body.Write([]byte{0, 0})
		body.Write(f.data)
	}
	var header [10]byte
	copy(header[0:3], "ID3")
	header[3] = 3
	writeSyncSafeSize(header[6:10], uint32(body.Len()))
	return append(header[:], body.Bytes()...)
}

func usltFrame(descriptor, text string) id3Frame {
	d := []byte{3, 'e', 'n', 'g'}
	d = append(d, []byte(descriptor)...)
	d = append(d, 0)
	d = append(d, []byte(text)...)
	return id3Frame{id: "USLT", data: d}
}

func syltFrame(entries []lyrics.SYLTEntry) id3Frame {
	d := []byte{3, 'e', 'n', 'g', 2, 1, 0}
	for _, e := range entries {
		d = append(d, []byte(e.Text)...)
		d = append(d, 0)
		var ts [4]byte
		binary.BigEndian.PutUint32(ts[:], uint32(e.Millis))
		d = append(d, ts[:]...)
	}
	return id3Frame{id: "SYLT", data: d}
}

func writeMP3WithFrames(t *testing.T, path string, frames []id3Frame) {
	t.Helper()
	all := append([]id3Frame{{id: "TIT2", data: append([]byte{0}, []byte("Title")...)}}, frames...)
	frame := append([]byte{0xFF, 0xFB, 0x90, 0x64}, bytes.Repeat([]byte{0}, 140)...)
	if err := os.WriteFile(path, append(id3v23Raw(all), frame...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func extractLyrics(t *testing.T, abs string) *extractedLyrics {
	t.Helper()
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	tr := &Track{Path: filepath.Base(abs), Size: info.Size(), ModTime: info.ModTime()}
	ec := &ExtractContext{SidecarIndex: new(sync.Map)}
	if err := ExtractWithContext(abs, tr, ec); err != nil {
		t.Fatalf("extract: %v", err)
	}
	if len(tr.lyricsCandidates) != 0 || tr.lyricsSidecar != nil {
		t.Fatal("resolveLyrics must clear the scratch fields")
	}
	return tr.lyrics
}

const lrcBody = "[00:01.000]First line\n[00:04.500]Second line"

func TestFLACVorbisLyricsAreExtracted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{{"TITLE", "T"}, {"LYRICS", lrcBody}})
	l := extractLyrics(t, p)
	if l == nil || l.Format != lyrics.FormatLRC || !l.Synced || l.Source != string(lyrics.SourceTextLRC) {
		t.Fatalf("LRC-shaped LYRICS → synced text-lrc: %+v", l)
	}
	if l.Body != lrcBody || l.Tag != lyrics.Tag(lrcBody) || len(l.Tag) != 8 {
		t.Fatalf("body/tag: %+v", l)
	}
	if l.SidecarName != "" || l.SourceSize == 0 {
		t.Fatalf("embedded provenance uses the audio file's stat: %+v", l)
	}

	p2 := filepath.Join(dir, "b.flac")
	writeMinimalFLACPairs(t, p2, 44100, 16, [][2]string{{"UNSYNCEDLYRICS", "Just words\nMore words"}})
	l = extractLyrics(t, p2)
	if l == nil || l.Format != lyrics.FormatText || l.Synced || l.Source != string(lyrics.SourceTextPlain) {
		t.Fatalf("UNSYNCEDLYRICS → plain: %+v", l)
	}

	p3 := filepath.Join(dir, "c.flac")
	writeMinimalFLACPairs(t, p3, 44100, 16, [][2]string{{"SYNCEDLYRICS", lrcBody}, {"LYRICS", "plain"}})
	l = extractLyrics(t, p3)
	if l == nil || l.Source != string(lyrics.SourceVorbisSynced) {
		t.Fatalf("SYNCEDLYRICS outranks LYRICS: %+v", l)
	}

	p4 := filepath.Join(dir, "d.flac")
	writeMinimalFLACPairs(t, p4, 44100, 16, [][2]string{{"TITLE", "no lyrics"}})
	if l := extractLyrics(t, p4); l != nil {
		t.Fatalf("no lyrics → nil, got %+v", l)
	}
}

func TestMP3USLTAndSYLTAreExtracted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "u.mp3")
	writeMP3WithFrames(t, p, []id3Frame{usltFrame("", "Verse one\nVerse two")})
	l := extractLyrics(t, p)
	if l == nil || l.Format != lyrics.FormatText || l.Language != "en" || l.Body != "Verse one\nVerse two" {
		t.Fatalf("USLT: %+v", l)
	}

	p2 := filepath.Join(dir, "s.mp3")
	writeMP3WithFrames(t, p2, []id3Frame{
		usltFrame("", "Plain fallback"),
		syltFrame([]lyrics.SYLTEntry{{Millis: 1000, Text: "\nFirst"}, {Millis: 2500, Text: "\nSecond"}}),
	})
	l = extractLyrics(t, p2)
	if l == nil || l.Source != string(lyrics.SourceSYLT) || !l.Synced || l.Body != "[00:01.000]First\n[00:02.500]Second" {
		t.Fatalf("SYLT beats USLT and renders LRC: %+v", l)
	}
}

func TestSidecarLRCWinsAndDriftIsDetected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Track One.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{{"LYRICS", lrcBody}})
	side := filepath.Join(dir, "track one.LRC") // case-folded stem + ext
	if err := os.WriteFile(side, []byte("\uFEFF[00:02.000]Sidecar\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := extractLyrics(t, p)
	if l == nil || l.Source != string(lyrics.SourceSidecarLRC) || l.SidecarName != "track one.LRC" || l.Body != "[00:02.000]Sidecar" {
		t.Fatalf("sidecar .lrc wins over embedded, BOM/CRLF normalized: %+v", l)
	}
	info, _ := os.Stat(side)
	if l.SourceMTimeNS != info.ModTime().UnixNano() || l.SourceSize != info.Size() {
		t.Fatalf("sidecar provenance stat: %+v vs %v/%d", l, info.ModTime().UnixNano(), info.Size())
	}

	st := &TrackStat{LyricsSource: l.Source, LyricsSidecarName: l.SidecarName,
		LyricsSourceMTimeNS: l.SourceMTimeNS, LyricsSourceSize: l.SourceSize}
	if sidecarLyricsDrifted(p, st, nil) {
		t.Fatal("unchanged sidecar must not read as drift")
	}
	if err := os.WriteFile(side, []byte("[00:02.000]Edited sidecar"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sidecarLyricsDrifted(p, st, nil) {
		t.Fatal("an edited sidecar (size moved) must read as drift")
	}
	if err := os.Remove(side); err != nil {
		t.Fatal(err)
	}
	if !sidecarLyricsDrifted(p, st, nil) {
		t.Fatal("a vanished sidecar must read as drift")
	}
	// Embedded-sourced row: a .txt beside it never outranks it; a .lrc does.
	emb := &TrackStat{LyricsSource: string(lyrics.SourceSYLT)}
	if err := os.WriteFile(filepath.Join(dir, "Track One.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sidecarLyricsDrifted(p, emb, nil) {
		t.Fatal("a .txt beside a SYLT row is not drift (it would lose the pick)")
	}
	if err := os.WriteFile(side, []byte("[00:02.000]Back"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !sidecarLyricsDrifted(p, emb, nil) {
		t.Fatal("a .lrc appearing beside a SYLT row is drift (it wins the pick)")
	}
	none := &TrackStat{}
	if !sidecarLyricsDrifted(p, none, nil) {
		t.Fatal("a sidecar beside a lyric-less row is drift")
	}
	// Memoized index: a directory read once per ExtractContext.
	ec := &ExtractContext{SidecarIndex: new(sync.Map)}
	if _, _, ok := sidecarLyricsFile(p, ec); !ok {
		t.Fatal("memoized lookup")
	}
	if err := os.Remove(side); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := sidecarLyricsFile(p, ec); !ok {
		t.Fatal("the memo serves the scan-start listing; a later removal is the next scan's business")
	}
}

func TestSidecarLegacyEncodingIsSkippedAndTaglessLRCIsPlain(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{{"TITLE", "T"}})
	// Invalid UTF-8 (GBK bytes) — served by the phone's own tier, not here.
	if err := os.WriteFile(filepath.Join(dir, "x.lrc"), []byte{0xC4, 0xE3, 0xBA, 0xC3, '\n'}, 0o644); err != nil {
		t.Fatal(err)
	}
	if l := extractLyrics(t, p); l != nil {
		t.Fatalf("legacy-encoded sidecar must be skipped, got %+v", l)
	}
	if err := os.WriteFile(filepath.Join(dir, "x.lrc"), []byte("no tags here\nsecond"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := extractLyrics(t, p)
	if l == nil || l.Source != string(lyrics.SourceSidecarText) || l.Format != lyrics.FormatText {
		t.Fatalf("a tagless .lrc is plain text: %+v", l)
	}
	// UTF-16 LE with BOM decodes.
	u16 := []byte{0xFF, 0xFE}
	for _, r := range "[00:01.000]Wide" {
		u16 = append(u16, byte(r), byte(r>>8))
	}
	if err := os.WriteFile(filepath.Join(dir, "x.lrc"), u16, 0o644); err != nil {
		t.Fatal(err)
	}
	l = extractLyrics(t, p)
	if l == nil || l.Body != "[00:01.000]Wide" || !l.Synced {
		t.Fatalf("UTF-16 sidecar: %+v", l)
	}
}

func TestStoreLyricsRoundTripAndIndexedAtBump(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	clock := time.Unix(1_000, 0).UTC()
	s.now = func() time.Time { return clock }

	doc := &extractedLyrics{Format: "lrc", Synced: true, Body: lrcBody, Language: "en",
		Source: "sylt", Tag: lyrics.Tag(lrcBody), SourceMTimeNS: 5, SourceSize: 6}
	tr := &Track{Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc}
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetLyrics(ctx, "a/x.flac")
	if err != nil || got == nil || got.Body != lrcBody || !got.Synced || got.Language != "en" || got.Tag != doc.Tag {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	rows, err := s.ListTracks(ctx, nil)
	if err != nil || len(rows) != 1 || rows[0].LyricsTag != doc.Tag {
		t.Fatalf("manifest splices lyricsTag: %+v %v", rows, err)
	}
	indexedAt := func() int64 {
		var v int64
		if err := s.db.QueryRowContext(ctx, `SELECT indexed_at FROM tracks WHERE path = ?`, "a/x.flac").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	base := indexedAt()

	// Stamp leg with an UNCHANGED tag: provenance refreshes, indexed_at holds.
	clock = clock.Add(time.Hour)
	same := &Track{Path: "a/x.flac", lyrics: &extractedLyrics{Format: "lrc", Synced: true, Body: lrcBody,
		Source: "sidecar-lrc", SidecarName: "x.lrc", Tag: doc.Tag, SourceMTimeNS: 77, SourceSize: 88}}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{same}); err != nil {
		t.Fatal(err)
	}
	if indexedAt() != base {
		t.Fatal("an unchanged lyrics tag must not bump indexed_at")
	}
	got, _ = s.GetLyrics(ctx, "a/x.flac")
	if got.SidecarName != "x.lrc" || got.SourceMTimeNS != 77 || got.Source != "sidecar-lrc" {
		t.Fatalf("provenance refresh on an unchanged tag: %+v", got)
	}

	// Stamp leg with a CHANGED body: row replaced, indexed_at advances.
	clock = clock.Add(time.Hour)
	changed := &Track{Path: "a/x.flac", lyrics: &extractedLyrics{Format: "text", Body: "new words",
		Source: "text", Tag: lyrics.Tag("new words"), SourceMTimeNS: 1, SourceSize: 2}}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{changed}); err != nil {
		t.Fatal(err)
	}
	if indexedAt() <= base {
		t.Fatal("a changed lyrics tag must bump indexed_at")
	}
	got, _ = s.GetLyrics(ctx, "a/x.flac")
	if got.Body != "new words" || got.Synced {
		t.Fatalf("replaced row: %+v", got)
	}
	base = indexedAt()

	// nil lyrics on a later extraction DELETES the row and bumps.
	clock = clock.Add(time.Hour)
	if err := s.UpsertTrackBatch(ctx, []*Track{{Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC()}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetLyrics(ctx, "a/x.flac"); got != nil {
		t.Fatalf("nil lyrics must delete the row, got %+v", got)
	}
	if indexedAt() <= base {
		t.Fatal("a vanished lyrics row must bump indexed_at")
	}
	rows, _ = s.ListTracks(ctx, nil)
	if rows[0].LyricsTag != "" {
		t.Fatal("manifest lyricsTag empties with the row")
	}

	// Batch upsert carries lyrics; DeleteTrack cascades.
	if err := s.UpsertTrackBatch(ctx, []*Track{{Path: "b/y.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetLyrics(ctx, "b/y.flac"); got == nil {
		t.Fatal("batch upsert writes the lyrics row")
	}
	if err := s.DeleteTrack(ctx, "b/y.flac"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetLyrics(ctx, "b/y.flac"); got != nil {
		t.Fatal("deleting the track must cascade to track_lyrics")
	}
}

func TestLookupLyricsFoldsCaseUnambiguously(t *testing.T) {
	ctx := context.Background()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	doc := &extractedLyrics{Format: "text", Body: "w", Source: "text", Tag: lyrics.Tag("w")}
	if err := s.UpsertTrack(ctx, &Track{Path: "Artist/Song.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.LookupLyrics(ctx, "artist/song.flac"); l == nil {
		t.Fatal("case-folded lookup")
	}
	if l, _ := s.LookupLyrics(ctx, "/Artist/Song.flac"); l == nil {
		t.Fatal("normalized (leading slash) lookup")
	}
	if l, _ := s.LookupLyrics(ctx, "nope.flac"); l != nil {
		t.Fatal("miss → nil")
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "artist/SONG.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), lyrics: doc}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.LookupLyrics(ctx, "ARTIST/song.flac"); l != nil {
		t.Fatal("two case-variants → ambiguous → nil")
	}
	_ = strings.ToLower
}
