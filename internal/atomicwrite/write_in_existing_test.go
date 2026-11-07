package atomicwrite

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestWriteBytesInExistingMakesNoDirectory: into a directory that is there,
// the file is written whole, as WriteBytes writes it; into one that is not,
// the write fails with fs.ErrNotExist and makes nothing, the directories
// above it included, where WriteBytes would make them.
func TestWriteBytesInExistingMakesNoDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note")
	if err := WriteBytesInExisting(path, []byte("one\n"), ".note-*.tmp"); err != nil {
		t.Fatal(err)
	}
	if err := WriteBytesInExisting(path, []byte("two\n"), ".note-*.tmp"); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "two\n" {
		t.Errorf("the note holds %q (%v), want the second write's bytes", got, err)
	}

	gone := filepath.Join(dir, "went", "config", "note")
	if err := WriteBytesInExisting(gone, []byte("three\n"), ".note-*.tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a write into a directory that is not there answered %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "went")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the write made the directory above its file's (%v)", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v (%v), want the note alone", names, err)
	}
}
