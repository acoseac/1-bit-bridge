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
