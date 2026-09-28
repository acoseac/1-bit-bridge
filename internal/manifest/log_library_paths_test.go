package manifest

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// captureScanLogs points slog's default at a buffer for the rest of the
// test, through loggingtest.SetDefault, which puts back the previous
// default and the log package's output and flags. scanLogger resolves the
// default at log time, so the scanner's and the extractors' lines land
// here.
func captureScanLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return buf
}

// TestTrackLogPath pins how an extractor names its file: the track's
// library-relative path when the scanner set one, the base name otherwise,
// never the absolute path.
func TestTrackLogPath(t *testing.T) {
	abs := filepath.Join("/Users/operator/Music", "Artist", "Album", "01.flac")
	for _, tc := range []struct {
		name string
		t    *Track
		want string
	}{
		{"the scanner's relative path", &Track{Path: "Artist/Album/01.flac"}, "Artist/Album/01.flac"},
		{"no path set", &Track{}, "01.flac"},
		{"no track", nil, "01.flac"},
	} {
		if got := trackLogPath(abs, tc.t); got != tc.want {
			t.Errorf("%s: trackLogPath = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTheDSTLineNamesTheTrackByItsLibraryPath pins the one extractor line
// logged in normal operation, once per DST-compressed file: it names the
// file library-relative, as the privacy page promises. ExtractorVersion 17
// re-extracts the whole library once, so an absolute path here would have
// reached the journal for every DST file on the first scan of v0.2.1.
func TestTheDSTLineNamesTheTrackByItsLibraryPath(t *testing.T) {
	logs := captureScanLogs(t)
	path := writeTempDFF(t, buildDFF(t, 2_822_400, "DST "))
	track := &Track{Path: "Artist/Album/01.dff"}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "DST-compressed") {
		t.Fatalf("no DST line was logged; the fixture no longer reaches it:\n%s", out)
	}
	if !strings.Contains(out, "path=Artist/Album/01.dff") {
		t.Errorf("the DST line does not name the library-relative path:\n%s", out)
	}
	if strings.Contains(out, filepath.Dir(path)) {
		t.Errorf("the DST line carries the absolute path:\n%s", out)
	}
}

// TestSubtreeRemovedNamesTheSubtreeByItsLibraryPath pins the Info line a
// subtree scan logs when the directory is gone (the watcher, an upload, a
// delete): library-relative, not the absolute path under the root.
func TestSubtreeRemovedNamesTheSubtreeByItsLibraryPath(t *testing.T) {
	root := t.TempDir()
	doomed := filepath.Join(root, "doomed")
	sibling := filepath.Join(root, "sibling")
	for _, d := range []string{doomed, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(doomed, "song.flac"), filepath.Join(sibling, "other.flac")} {
		if err := os.WriteFile(f, audioBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	if err := os.RemoveAll(doomed); err != nil {
		t.Fatal(err)
	}

	logs := captureScanLogs(t)
	if _, err := sc.ScanSubtree(context.Background(), doomed); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "subtree removed") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no subtree-removed line; the fixture no longer reaches it:\n%s", logs.String())
	}
	if !strings.Contains(line, "path=doomed") {
		t.Errorf("the line does not name the library-relative path: %q", line)
	}
	if strings.Contains(line, root) {
		t.Errorf("the line carries the absolute path: %q", line)
	}
}
