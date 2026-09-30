package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// statKey is the key part folderArtStateOfListing writes for the candidate
// file at p, by its listed name.
func statKey(t *testing.T, p string) string {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s:%d:%d", filepath.Base(p), info.Size(), info.ModTime().UnixNano())
}

// writeArtFile writes data at p with the mtime mtime.
func writeArtFile(t *testing.T, p string, data []byte, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestFolderArtKeyNamesWhatTheLookupReads pins the key's shape: every
// candidate the lookup would read, by its listed name, size and mtime, in
// listing order (whatever its case), and nothing else in the folder; a disc
// folder's parent after a '|', unless the parent holds none or the disc
// folder is a library root.
func TestFolderArtKeyNamesWhatTheLookupReads(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	writeArtFile(t, filepath.Join(album, "Cover.JPG"), coverBytes("a"), t0)
	writeArtFile(t, filepath.Join(album, "folder.png"), []byte("\x89PNG"), t0.Add(time.Minute))
	writeArtFile(t, filepath.Join(album, "back.jpg"), coverBytes("b"), t0)
	writeArtFile(t, filepath.Join(album, "notes.txt"), []byte("x"), t0)
	if err := os.Mkdir(filepath.Join(album, "folder.jpg"), 0o755); err != nil { // a directory is no cover
		t.Fatal(err)
	}
	track := filepath.Join(album, "01.flac")

	key, seen := folderArtKey(track, &ExtractContext{})
	want := statKey(t, filepath.Join(album, "Cover.JPG")) + "," + statKey(t, filepath.Join(album, "folder.png"))
	if !seen || key != want {
		t.Errorf("folderArtKey = %q (seen %v), want %q", key, seen, want)
	}

	disc := filepath.Join(album, "Disc 1")
	if err := os.MkdirAll(disc, 0o755); err != nil {
		t.Fatal(err)
	}
	if key, seen := folderArtKey(filepath.Join(disc, "a.flac"), &ExtractContext{}); !seen || key != "|"+want {
		t.Errorf("a disc folder with no cover of its own: key %q (seen %v), want %q", key, seen, "|"+want)
	}
	writeArtFile(t, filepath.Join(disc, "cover.jpg"), coverBytes("disc"), t0)
	own := statKey(t, filepath.Join(disc, "cover.jpg"))
	if key, seen := folderArtKey(filepath.Join(disc, "a.flac"), &ExtractContext{}); !seen || key != own+"|"+want {
		t.Errorf("a disc folder with its own cover: key %q (seen %v), want %q", key, seen, own+"|"+want)
	}
	asRoot := &ExtractContext{LibraryRootDirs: map[string]struct{}{disc: {}}}
	if key, seen := folderArtKey(filepath.Join(disc, "a.flac"), asRoot); !seen || key != own {
		t.Errorf("a disc folder that is a library root: key %q (seen %v), want %q (its parent is outside the library)", key, seen, own)
	}

	bare := filepath.Join(root, "Bare", "Disc 2")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	if key, seen := folderArtKey(filepath.Join(bare, "b.flac"), &ExtractContext{}); !seen || key != "" {
		t.Errorf("no cover anywhere: key %q (seen %v), want \"\", the key of a row from before the column", key, seen)
	}
}

// TestFolderArtKeyIsTakenOncePerDirectoryPerScan pins the memo: with the
// scan's index, a folder's state is the one the scan first took, so every
// row a scan writes in it records the same key, and the key is never newer
// than the cover the lookup read.
func TestFolderArtKeyIsTakenOncePerDirectoryPerScan(t *testing.T) {
	album := t.TempDir()
	cover := filepath.Join(album, "cover.jpg")
	writeArtFile(t, cover, coverBytes("first"), t0)
	ec := &ExtractContext{SidecarIndex: new(sync.Map)}
	first, _ := folderArtKey(filepath.Join(album, "01.flac"), ec)

	writeArtFile(t, cover, coverBytes("second!"), t0.Add(time.Hour))
	if again, _ := folderArtKey(filepath.Join(album, "02.flac"), ec); again != first {
		t.Errorf("the same scan answered %q, then %q", first, again)
	}
	if fresh, _ := folderArtKey(filepath.Join(album, "02.flac"), &ExtractContext{SidecarIndex: new(sync.Map)}); fresh == first {
		t.Errorf("the next scan answered the old key %q", fresh)
	}
}

// TestFolderArtKeyOfAFolderThatCannotBeListed pins the unseen state: a
// directory the scan cannot read says nothing, and the gate keeps its rows.
func TestFolderArtKeyOfAFolderThatCannotBeListed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone")
	if _, seen := folderArtKey(filepath.Join(missing, "01.flac"), &ExtractContext{}); seen {
		t.Error("a folder that could not be listed was seen")
	}
}
