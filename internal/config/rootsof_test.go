package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestLibraryRootsOfIsTheRootsLoadGives: the roots bridge doctor probes beside
// its load of the config (backlog B650) are the ones that load gives, whatever
// form the file writes them in, BRIDGE_LIBRARY_ROOTS over the file's, read
// without touching any root: a root that is not there is read as written.
func TestLibraryRootsOfIsTheRootsLoadGives(t *testing.T) {
	base := t.TempDir()
	sep := string(os.PathListSeparator)
	for _, c := range []struct {
		name  string
		roots string // the libraryRoots key's value lines
		env   string // BRIDGE_LIBRARY_ROOTS, "" for unset
	}{
		{"absolute roots", "  - " + filepath.Join(base, "Music") + "\n  - " + filepath.Join(base, "NAS", "music") + "\n", ""},
		{"relative roots, cleaned", "  - music\n  - ./a/../b\n", ""},
		{"a root that is not there", "  - " + filepath.Join(base, "gone", "away") + "\n", ""},
		{"the variable over the file's", "  - " + filepath.Join(base, "Music") + "\n", " " + filepath.Join(base, "x") + sep + "y" + sep + sep},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.env != "" {
				t.Setenv("BRIDGE_LIBRARY_ROOTS", c.env)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "bridge.yaml")
			data := []byte("libraryRoots:\n" + c.roots)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, err := LibraryRootsOf(path, data)
			if err != nil {
				t.Fatalf("LibraryRootsOf: %v", err)
			}
			if !slices.Equal(got, cfg.LibraryRoots) {
				t.Errorf("LibraryRootsOf = %q, Load gives %q", got, cfg.LibraryRoots)
			}
		})
	}
	// A file Load's decode refuses (a key it does not know) is refused here
	// too: it reads the file through DecodeFile, as Load does.
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	data := []byte("libraryRoots:\n  - " + filepath.Join(base, "Music") + "\nlibraryRoot: typo\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load took a key it does not know")
	}
	if roots, err := LibraryRootsOf(path, data); err == nil {
		t.Errorf("LibraryRootsOf = %q of a file Load's decode refuses, want it refused", roots)
	}
}

// TestFileLibraryRootsIsWhatTheFileSays: the roots a database-only restore
// compares (backlog B653 PR 2b) are the ones the file names, each resolved as
// LibraryRootsOf resolves it, whatever BRIDGE_LIBRARY_ROOTS holds: the
// variable changes LibraryRootsOf's answer and never this one's, and a file
// Load's decode refuses is refused here too.
func TestFileLibraryRootsIsWhatTheFileSays(t *testing.T) {
	base := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	data := []byte("libraryRoots:\n  - " + filepath.Join(base, "Music") + "\n  - nas\n")
	want := []string{filepath.Join(base, "Music"), filepath.Join(dir, "nas")}
	got, err := FileLibraryRoots(path, data)
	if err != nil {
		t.Fatalf("FileLibraryRoots: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("FileLibraryRoots = %q, want %q", got, want)
	}
	if of, err := LibraryRootsOf(path, data); err != nil || !slices.Equal(of, want) {
		t.Errorf("with no variable set LibraryRootsOf = %q, %v, want the file's %q", of, err, want)
	}

	other := filepath.Join(base, "Other")
	t.Setenv("BRIDGE_LIBRARY_ROOTS", other)
	if of, err := LibraryRootsOf(path, data); err != nil || !slices.Equal(of, []string{other}) {
		t.Errorf("with the variable set LibraryRootsOf = %q, %v, want the variable's %q", of, err, other)
	}
	if got, err := FileLibraryRoots(path, data); err != nil || !slices.Equal(got, want) {
		t.Errorf("with the variable set FileLibraryRoots = %q, %v, want the file's %q", got, err, want)
	}

	if roots, err := FileLibraryRoots(path, []byte("libraryRoots:\n  - nas\nlibraryRoot: typo\n")); err == nil {
		t.Errorf("FileLibraryRoots = %q of a file Load's decode refuses, want it refused", roots)
	}
}
