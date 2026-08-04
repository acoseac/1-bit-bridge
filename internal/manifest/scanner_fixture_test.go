package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Fixture helpers shared by the scanner tests: stage dirs with a track
// each, open a store + scanner over a root, scan, assert what got
// indexed, and ask whether the filesystem is case-sensitive. Kept small
// and explicit rather than one do-everything harness — each test still
// reads as its own scenario.
//
// This file is deliberately UNTAGGED. The helpers were originally
// defined in scanner_walk_error_test.go, which is `//go:build !windows`
// because its own tests simulate a transient I/O error with `chmod
// 0000` — meaningless on Windows. Untagged siblings then started using
// them (scanner_extractor_version_test.go, and scanner_disc_art_heal_
// test.go from PR #606), which made the whole `manifest` test binary
// fail to COMPILE for windows/amd64:
//
//	vet: scanner_disc_art_heal_test.go:83:2: undefined: scanOnce
//
// Nothing caught it because nothing had ever compiled this package for
// Windows — every workflow ran on ubuntu-latest and `build-all` only
// cross-compiles the non-test tree.
//
// So: everything portable lives here, and scanner_walk_error_test.go
// keeps only `breakWalk`, which is genuinely POSIX-only. Put new shared
// scanner fixtures in THIS file, not in a build-tagged one — a helper
// behind `!windows` is one untagged caller away from taking the
// package's entire test binary out on a platform CI now covers.
// CLAUDE.md points future authors at these by name.

// seedTrackDirs creates each dir and drops a stub track into it. The body
// isn't valid FLAC on purpose: these tests exercise the walk and the
// deletion pass, not tag extraction.
func seedTrackDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "song.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// newScanFixture opens a temp-dir-backed store and a scanner rooted at
// root, registering the store's Close.
func newScanFixture(t *testing.T, root string) (*Store, *Scanner) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, NewScanner([]string{root}, s, "")
}

// scanOnce runs a full scan, failing the test on error. label names the
// pass so a failure says which one broke.
func scanOnce(t *testing.T, sc *Scanner, label string) {
	t.Helper()
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// mustIndexed asserts every path is present in the manifest. Fatal, not
// Error: a fixture that didn't index is not a meaningful base state for
// the assertions that follow.
func mustIndexed(t *testing.T, s *Store, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if got, _ := s.GetTrack(context.Background(), p); got == nil {
			t.Fatalf("scan didn't index %q", p)
		}
	}
}

// caseSensitiveFS reports whether dir lives on a case-sensitive
// filesystem. macOS APFS/HFS+ default to case-INsensitive and Windows
// always is, so a case-twin fixture can only be staged on Linux (and
// on a case-sensitive macOS volume). Probing rather than branching on
// runtime.GOOS: the answer is a property of the volume under test, not
// of the platform — a case-sensitive APFS volume on macOS says yes.
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "CaseProbe")
	if err := os.Mkdir(probe, 0o755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(probe) }()
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	return os.IsNotExist(err)
}
