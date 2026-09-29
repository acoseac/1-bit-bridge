package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveLinksAgreesWithEvalSymlinksWhereThereIsNoLink: on a path with no
// link in it, ResolveLinks spells what filepath.EvalSymlinks spells. On
// Windows the two get there differently (a handle's final path against a
// component walk), and a temp directory can carry 8.3 short names, which
// both turn into the long ones; an answer that differed here would move
// every containment check that compares a resolved path with an unresolved
// neighbour's.
func TestResolveLinksAgreesWithEvalSymlinksWhereThereIsNoLink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Album")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "01.flac")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, file} {
		want, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ResolveLinks(p)
		if err != nil || got != want {
			t.Errorf("ResolveLinks(%s) = %q, %v; want %q, as filepath.EvalSymlinks answers", p, got, err, want)
		}
	}
}

// TestResolveLinksSaysAMissingPathIsNotThere: a path that is not there is an
// error errors.Is reads as fs.ErrNotExist, as filepath.EvalSymlinks's is, so
// EvalSymlinksOrClean resolves its nearest ancestor instead and the sidecar
// inventory reads it as nothing to inventory.
func TestResolveLinksSaysAMissingPathIsNotThere(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-made-yet", "variants")
	if got, err := ResolveLinks(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ResolveLinks(%s) = %q, %v; want an error that is fs.ErrNotExist", missing, got, err)
	}
}
