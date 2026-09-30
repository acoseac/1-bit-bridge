package fsutil

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenAsFileOpensAFileRefusesADirectoryAndKeepsTheOpenError pins
// OpenAsFile on every platform: a file opens with the opened file's own
// stat, a directory is refused with a *NotAFileError inside the
// *fs.PathError that names it, and an open that fails comes back as
// os.OpenFile gave it, which is what keeps every caller's os.IsNotExist
// answering as it did (the artwork, booklet and cover routes' 404 and 202
// arms all hang on it).
func TestOpenAsFileOpensAFileRefusesADirectoryAndKeepsTheOpenError(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "01 Track.flac")
	if err := os.WriteFile(file, []byte("fLaC and the rest"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, info, err := OpenAsFile(file)
	if err != nil {
		t.Fatalf("OpenAsFile(a file): %v", err)
	}
	body, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(body) != "fLaC and the rest" {
		t.Errorf("read %q, %v; want the file's bytes", body, err)
	}
	if info.Size() != int64(len(body)) || info.Name() != "01 Track.flac" || !info.Mode().IsRegular() {
		t.Errorf("stat %s, %d bytes, %v; want the opened file's own", info.Name(), info.Size(), info.Mode())
	}

	f, info, err = OpenAsFile(dir)
	var pe *fs.PathError
	switch {
	case f != nil || info != nil:
		t.Errorf("OpenAsFile(a directory) returned a file")
	case NotAFileKind(err) != "directory":
		t.Errorf("OpenAsFile(a directory): %v; want a *NotAFileError naming a directory", err)
	case !errors.As(err, &pe) || pe.Path != dir:
		t.Errorf("OpenAsFile(a directory): %v; want an *fs.PathError naming %s", err, dir)
	}

	_, _, err = OpenAsFile(filepath.Join(dir, "missing.flac"))
	if !os.IsNotExist(err) || NotAFileKind(err) != "" {
		t.Errorf("OpenAsFile(a missing file): %v; want os.OpenFile's not-exist error, as it was", err)
	}
}

// TestReadAsFileReadsAFileWholeAndRefusesADirectory pins ReadAsFile on every
// platform: a file's bytes come back whole, as os.ReadFile gives them, an
// empty one and one longer than a read's first grab alike, and a directory
// is refused as OpenAsFile refuses it.
func TestReadAsFileReadsAFileWholeAndRefusesADirectory(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string][]byte{
		"empty.lrc":  nil,
		"short.lrc":  []byte("[00:01.00]A line\n"),
		"cover.jpg":  bytes.Repeat([]byte{0xFF, 0xD8, 0xFF, 0x00}, 3000),
		"exact.txt":  bytes.Repeat([]byte("x"), bytes.MinRead),
		"odd-12.bin": bytes.Repeat([]byte{1, 2, 3}, 4),
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := ReadAsFile(p)
		if err != nil || !bytes.Equal(got, body) {
			t.Errorf("ReadAsFile(%s): %d bytes, %v; want its %d bytes", name, len(got), err, len(body))
		}
	}
	if got, err := ReadAsFile(dir); got != nil || NotAFileKind(err) != "directory" {
		t.Errorf("ReadAsFile(a directory): %d bytes, %v; want it refused as a directory", len(got), err)
	}
	if _, err := ReadAsFile(filepath.Join(dir, "missing.lrc")); !os.IsNotExist(err) {
		t.Errorf("ReadAsFile(a missing file): %v; want os.OpenFile's not-exist error", err)
	}
}
