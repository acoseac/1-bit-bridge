package manifest

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// TestEveryWriteOfAKindRecordsTheStatItWasReadAt: every writer of a kind
// records with it the size and mtime of the file it was read from, which are
// the row's own: a scan's insert, its rewrite of a file that changed, the
// version-stale stamp leg (a kind already right, recorded at no stat,
// included), the open-time fill and the header pass. So the next open finds
// no kind to clear, and moves no row.
func TestEveryWriteOfAKindRecordsTheStatItWasReadAt(t *testing.T) {
	ctx := context.Background()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	root := t.TempDir()
	wavRel := writeAlone(t, root, "float.wav", fx("wav/float.wav"))
	flacRel := writeAlone(t, root, "lossless.flac", flacBytes(t, 44100, 16))
	store, sc := newScanFixture(t, root)
	recorded := func(when string, rels ...string) {
		t.Helper()
		for _, rel := range rels {
			var ok bool
			if err := store.db.QueryRow(`SELECT sample_format IS NOT NULL
					AND sample_format_size IS size AND sample_format_mtime_ns IS mtime_ns
				FROM tracks WHERE path = ?`, rel).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if !ok {
				t.Errorf("%s: %s's kind is not recorded with the row's size and mtime", when, rel)
			}
		}
	}

	scanOnce(t, sc, "the first scan")
	recorded("a scan's insert", wavRel, flacRel)

	abs := filepath.Join(root, filepath.FromSlash(wavRel))
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureBytes(t, abs, fx("wav/af_f64.wav"))
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "a scan after the file changed")
	recorded("a scan's rewrite of a changed file", wavRel)

	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = ?, sample_format_size = NULL,
		sample_format_mtime_ns = NULL WHERE path = ?`, ExtractorVersion-1, wavRel); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "a version-stale scan")
	recorded("the stamp leg, the kind already right", wavRel)

	rewindToBeforeSampleFormat(t, store, 42)
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	recorded("the open-time fill", flacRel)
	if _, err := store.BackfillSampleFormats(ctx, bridgefs.New([]string{root}).ResolveChecked); err != nil {
		t.Fatal(err)
	}
	recorded("the header pass", wavRel)

	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{wavRel: SampleFormatFloat, flacRel: SampleFormatInteger})
	for _, rel := range []string{wavRel, flacRel} {
		if got := trackIndexedAt(t, store, rel); got != 42 {
			t.Errorf("the next open moved %s's cursor 42 -> %d", rel, got)
		}
	}
}

// TestADelayedStampLeavesTheKindOfARowAnotherScanRewrote: a stamp certifies
// only the stat its extraction read, compared with the row's in its write. A
// version-stale scan (A) reads float.wav, and its stamp waits in the scan
// writer's batch; before it lands, another scan (B, a `bridge scan` beside
// serve, which opens the store and scans with no lock) indexes the integer
// render that replaced the file. A's stamp then leaves B's row as B wrote it,
// B's kind at B's stat, which the next open keeps and a later scan, finding
// the file unchanged, does not read again.
func TestADelayedStampLeavesTheKindOfARowAnotherScanRewrote(t *testing.T) {
	ctx := context.Background()
	fx := func(rel string) []byte { return compressedPCMFixture(t, rel) }
	root := t.TempDir()
	rel := writeAlone(t, root, "replaced.wav", fx("wav/float.wav"))
	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "the first scan")
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}

	// A: what reExtractUnchanged hands the stamp leg for an unchanged file,
	// the walk's stat and the extraction, with the row's folder-art key.
	a := &Track{Path: rel, Size: info.Size(), ModTime: info.ModTime().UTC()}
	if err := ExtractWithContext(abs, a, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COALESCE(folder_art_key, '') FROM tracks WHERE path = ?`, rel).Scan(&a.folderArtKey); err != nil {
		t.Fatal(err)
	}
	a.versionStampOnly = true
	if a.SampleFormat != SampleFormatFloat {
		t.Fatalf("A read the kind %q, want %q", a.SampleFormat, SampleFormatFloat)
	}

	// B: the file is replaced by an integer render, and another scan writes
	// its row.
	writeFixtureBytes(t, abs, fx("wav/af_i16.wav"))
	later := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, NewScanner([]string{root}, store, ""), "scan B")
	requireSampleFormats(t, store, map[string]string{rel: SampleFormatInteger})

	// A's stamp lands, then the next open and a later scan.
	if err := store.StampExtractorVersionBatch(ctx, []*Track{a}); err != nil {
		t.Fatal(err)
	}
	requireSampleFormats(t, store, map[string]string{rel: SampleFormatInteger})
	if err := store.migrate(); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "a later scan")
	requireSampleFormats(t, store, map[string]string{rel: SampleFormatInteger})
	if got := servedTracksByPath(t, store, root)[rel]["sampleFormat"]; got != SampleFormatInteger {
		t.Errorf("the manifest serves %s with sampleFormat %#v, want %q", rel, got, SampleFormatInteger)
	}
}

// TestAStoreTheFirstV59LeftOpensAndDecidesItsKindsAgain: the first form of
// v59 (round 2 of B513) added sample_format and idx_tracks_sample_format_null
// and nothing else, and a store a build of it opened stays at that v59. The
// stat columns and the stale-kind index are v60's, so such a store opens and
// gains them, and every kind it holds, recorded at no stat, is decided again
// rather than taken as current: the FLAC row by the open's fill, the WAV row
// left to the header pass.
func TestAStoreTheFirstV59LeftOpensAndDecidesItsKindsAgain(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	first := openStoreAt(t, dbPath)
	at := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := first.UpsertTrackBatch(ctx, []*Track{
		{Path: "a/one.flac", Size: 1, ModTime: at, Codec: "FLAC", SampleFormat: SampleFormatInteger},
		{Path: "a/two.wav", Size: 1, ModTime: at, Codec: "WAV", SampleFormat: SampleFormatFloat},
	}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX idx_tracks_sample_format_stale`,
		`ALTER TABLE tracks DROP COLUMN sample_format_mtime_ns`,
		`ALTER TABLE tracks DROP COLUMN sample_format_size`,
		`PRAGMA user_version = 59`,
	} {
		if _, err := first.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatalf("a store the first v59 left does not open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, col := range []string{"sample_format_size", "sample_format_mtime_ns"} {
		if ok, err := atlasColumnExists(s.db, "tracks", col); err != nil || !ok {
			t.Errorf("tracks.%s after the open: %v, %v", col, ok, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_tracks_sample_format_stale'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("idx_tracks_sample_format_stale after the open: %d, %v", n, err)
	}
	requireSampleFormats(t, s, map[string]string{"a/one.flac": SampleFormatInteger, "a/two.wav": "NULL"})
	var recorded bool
	if err := s.db.QueryRow(`SELECT sample_format_size IS size AND sample_format_mtime_ns IS mtime_ns
		FROM tracks WHERE path = 'a/one.flac'`).Scan(&recorded); err != nil || !recorded {
		t.Errorf("the FLAC row's kind is not recorded with its stat: %v, %v", recorded, err)
	}
}

// kindAssignment is a write of tracks.sample_format in a SET clause:
// sample_format_size and sample_format_mtime_ns do not match it.
var kindAssignment = regexp.MustCompile(`\bsample_format\s*=`)

// writesSampleFormat reports whether the statement q writes the kind: an
// INSERT INTO tracks whose column list names sample_format, or a SET clause
// (an UPDATE, an upsert's conflict arm) that assigns it.
func writesSampleFormat(q string) bool {
	if i := strings.Index(q, "INSERT INTO tracks("); i >= 0 {
		cols := q[i:]
		if j := strings.Index(cols, ")"); j >= 0 && regexp.MustCompile(`\bsample_format\b`).MatchString(cols[:j]) {
			return true
		}
	}
	for rest := q; ; {
		i := strings.Index(rest, "SET")
		if i < 0 {
			return false
		}
		set := rest[i+len("SET"):]
		if j := strings.Index(set, "WHERE"); j >= 0 {
			set = set[:j]
		}
		if kindAssignment.MatchString(set) {
			return true
		}
		rest = rest[i+len("SET"):]
	}
}

// TestEverySampleFormatWriteRecordsItsStat reads this package's SQL: every
// statement that writes tracks.sample_format also writes sample_format_size
// and sample_format_mtime_ns. A kind written without them reads as stale to
// the next open, which clears it and decides the row again; a kind written
// with stale ones would survive the change of file they describe.
func TestEverySampleFormatWriteRecordsItsStat(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var writes []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			q, err := strconv.Unquote(lit.Value)
			if err != nil || !writesSampleFormat(q) {
				return true
			}
			at := fset.Position(lit.Pos()).String()
			writes = append(writes, at)
			for _, col := range []string{"sample_format_size", "sample_format_mtime_ns"} {
				if !strings.Contains(q, col) {
					t.Errorf("%s writes sample_format without %s", at, col)
				}
			}
			return true
		})
	}
	// The upsert, the stamp leg, the fill, the header pass and the open-time
	// clear: a sweep that finds fewer has stopped reading the statements.
	if len(writes) < 5 {
		t.Fatalf("found %d statements that write sample_format, want at least 5: %v", len(writes), writes)
	}
}
