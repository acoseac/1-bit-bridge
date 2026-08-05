package manifest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestExtractFLACCapturesAudioMD5 pins the STREAMINFO capture (bytes
// 18..33) and the all-zero "encoder did not compute it" sentinel →
// unknown. Without the sentinel guard every checksum-less encode would
// collide with every other one — a library-wide false "bit-identical"
// claim in the one tier that asserts certainty.
func TestExtractFLACCapturesAudioMD5(t *testing.T) {
	dir := t.TempDir()
	md5 := [16]byte{0xAA, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0xFF}
	withMD5 := filepath.Join(dir, "with.flac")
	writeMinimalFLACWithMD5(t, withMD5, 44100, 16, map[string]string{"title": "X"}, md5)
	zeroMD5 := filepath.Join(dir, "zero.flac")
	writeMinimalFLAC(t, zeroMD5, 44100, 16, map[string]string{"title": "Y"})

	f, err := os.Open(withMD5)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var tr Track
	if err := extractFLACFormatFromReader(f, withMD5, &tr); err != nil {
		t.Fatal(err)
	}
	if tr.audioMD5 != hex.EncodeToString(md5[:]) {
		t.Fatalf("audioMD5 = %q, want %q", tr.audioMD5, hex.EncodeToString(md5[:]))
	}

	fz, err := os.Open(zeroMD5)
	if err != nil {
		t.Fatal(err)
	}
	defer fz.Close()
	var trz Track
	if err := extractFLACFormatFromReader(fz, zeroMD5, &trz); err != nil {
		t.Fatal(err)
	}
	if trz.audioMD5 != "" {
		t.Fatalf("all-zero STREAMINFO MD5 must read as unknown, got %q", trz.audioMD5)
	}
}

// TestAudioMD5NeverReachesTagsJSONOrTheWire: the field is unexported, so
// BOTH serialization paths — marshalForStorage (tags_json) and a plain
// json.Marshal (the wire) — must be byte-identical with and without it.
// That is what keeps the whole feature off the protocol.
func TestAudioMD5NeverReachesTagsJSONOrTheWire(t *testing.T) {
	base := Track{Path: "a/b/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC(), Title: "T"}
	withMD5 := base
	withMD5.audioMD5 = "deadbeefdeadbeefdeadbeefdeadbeef"

	s1, err := marshalForStorage(&base)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := marshalForStorage(&withMD5)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s1, s2) {
		t.Fatal("audioMD5 leaked into tags_json")
	}
	w1, _ := json.Marshal(base)
	w2, _ := json.Marshal(withMD5)
	if !bytes.Equal(w1, w2) {
		t.Fatal("audioMD5 leaked onto the wire")
	}
	if bytes.Contains(s2, []byte("deadbeef")) || bytes.Contains(w2, []byte("deadbeef")) {
		t.Fatal("audioMD5 value visible in serialized output")
	}
}

func audioMD5Column(t *testing.T, s *Store, path string) string {
	t.Helper()
	var v string
	if err := s.db.QueryRow(`SELECT audio_md5 FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("audio_md5(%q): %v", path, err)
	}
	return v
}

// TestStampAndUpsertMD5Semantics pins the two write paths' OPPOSITE
// update rules. Upserts run because the bytes CHANGED → unconditional
// (a stale hash would assert old audio for new bytes, and an empty fresh
// value must CLEAR). The version-stale stamp runs only when the row was
// PROVED byte-identical → COALESCE(NULLIF(…)) keeps a known value when
// the fresh one is empty. Reversing either direction fails silently.
func TestStampAndUpsertMD5Semantics(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	tr := &Track{Path: "a/x.flac", Size: 1, ModTime: time.Unix(0, 0).UTC()}
	tr.audioMD5 = "aaaa"
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if got := audioMD5Column(t, s, "a/x.flac"); got != "aaaa" {
		t.Fatalf("upsert must write the hash, got %q", got)
	}

	// Stamp with a fresh EMPTY value keeps the stored hash.
	empty := &Track{Path: "a/x.flac"}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{empty}); err != nil {
		t.Fatal(err)
	}
	if got := audioMD5Column(t, s, "a/x.flac"); got != "aaaa" {
		t.Fatalf("stamp with empty fresh value must keep the known hash, got %q", got)
	}

	// Stamp with a fresh value updates it.
	fresh := &Track{Path: "a/x.flac"}
	fresh.audioMD5 = "bbbb"
	if err := s.StampExtractorVersionBatch(ctx, []*Track{fresh}); err != nil {
		t.Fatal(err)
	}
	if got := audioMD5Column(t, s, "a/x.flac"); got != "bbbb" {
		t.Fatalf("stamp with fresh hash must update, got %q", got)
	}

	// A CHANGED-row upsert with an empty hash CLEARS — the bytes changed,
	// so the old audio claim no longer holds.
	tr2 := &Track{Path: "a/x.flac", Size: 2, ModTime: time.Unix(9, 0).UTC()}
	if err := s.UpsertTrack(ctx, tr2); err != nil {
		t.Fatal(err)
	}
	if got := audioMD5Column(t, s, "a/x.flac"); got != "" {
		t.Fatalf("changed-row upsert with empty hash must clear, got %q", got)
	}
}

// TestScanner_VersionStaleStampBackfillsAudioMD5 is the load-bearing
// integration pin (the OLD-PLAN "where the PR lands or doesn't" test):
// a row written by an older binary (stale extractor_version, empty
// audio_md5) whose file is UNCHANGED takes the versionStampOnly leg on
// the next scan — and that leg must carry the freshly-captured MD5 into
// the column while indexed_at and enriched_at stay unmoved (no iOS
// delta, no re-enrichment).
func TestScanner_VersionStaleStampBackfillsAudioMD5(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	md5 := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
		0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x01}
	writeMinimalFLACWithMD5(t, filepath.Join(album, "01 Song.flac"), 44100, 16,
		map[string]string{"title": "Song", "artist": "Artist", "album": "Album"}, md5)

	store := openTestStore(t)
	sc := NewScanner([]string{root}, store, "")
	ctx := context.Background()
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	const rel = "Artist/Album/01 Song.flac"
	want := hex.EncodeToString(md5[:])
	if got := audioMD5Column(t, store, rel); got != want {
		t.Fatalf("initial scan should capture MD5, got %q", got)
	}

	// Simulate rows written by a pre-v3 binary: stale stamp, empty hash.
	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = 0, audio_md5 = ''`); err != nil {
		t.Fatal(err)
	}
	beforeIndexed := indexedAtOf(t, store, rel)
	var beforeEnriched int64
	if err := store.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = ?`, rel).Scan(&beforeEnriched); err != nil {
		t.Fatal(err)
	}

	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("backfill scan: %v", err)
	}
	if got := audioMD5Column(t, store, rel); got != want {
		t.Fatalf("version-stale stamp must backfill audio_md5, got %q", got)
	}
	if got := indexedAtOf(t, store, rel); got != beforeIndexed {
		t.Fatalf("stamp leg must not move indexed_at (%d → %d) — that is the whole no-delta point", beforeIndexed, got)
	}
	var afterEnriched int64
	if err := store.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = ?`, rel).Scan(&afterEnriched); err != nil {
		t.Fatal(err)
	}
	if afterEnriched != beforeEnriched {
		t.Fatalf("stamp leg must not touch enriched_at (%d → %d)", beforeEnriched, afterEnriched)
	}
	var stamped int64
	if err := store.db.QueryRow(`SELECT extractor_version FROM tracks WHERE path = ?`, rel).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != ExtractorVersion {
		t.Fatalf("extractor_version = %d, want %d", stamped, ExtractorVersion)
	}
}
