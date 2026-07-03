package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDownloadVerified_RejectsOversizeArchive is the F24 regression: the
// write-to-disk is bounded by maxArchiveDownloadBytes, so a source that
// streams past the cap is rejected with an explicit size error BEFORE
// the checksum compare (which the checksum below would otherwise fail
// with a confusing generic mismatch), and the truncated dst is never
// returned as success.
func TestDownloadVerified_RejectsOversizeArchive(t *testing.T) {
	orig := maxArchiveDownloadBytes
	maxArchiveDownloadBytes = 16
	t.Cleanup(func() { maxArchiveDownloadBytes = orig })

	body := strings.Repeat("A", 1024) // >> the 16-byte cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "checksums.txt") {
			_, _ = io.WriteString(w, "deadbeef  archive.tar.gz\n")
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "archive.tar.gz")
	_, err := downloadVerified(context.Background(), srv.Client(),
		srv.URL+"/archive.tar.gz", "archive.tar.gz", srv.URL+"/checksums.txt", dst)
	if err == nil || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("oversize archive: err = %v; want an 'exceeds size limit' error", err)
	}
}

// writeTarGzSymlink builds a .tar.gz whose sole entry is a symlink with
// the given name, returning the archive path.
func writeTarGzSymlink(t *testing.T, name, linkTarget string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "archive.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Typeflag: tar.TypeSymlink, Linkname: linkTarget, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestExtractTarGzBinary_SkipsNonRegularEntry is the F26 regression: a
// symlink entry named like the binary must NOT be extracted (pre-fix it
// was fed to writeExecutable, installing a 0-byte binary). The extractor
// must skip it and report the binary as not found, leaving no dst file.
func TestExtractTarGzBinary_SkipsNonRegularEntry(t *testing.T) {
	arc := writeTarGzSymlink(t, "bridge", "/etc/passwd")
	dst := filepath.Join(t.TempDir(), "bridge")
	err := extractTarGzBinary(arc, "bridge", dst)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("symlink entry: err = %v; want a 'not found' error", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Errorf("dst was written from a non-regular entry; want no file at %s", dst)
	}
}

// TestExtractTarGzBinary_ExtractsRegularFile is the positive companion:
// a real regular-file entry still extracts.
func TestExtractTarGzBinary_ExtractsRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "archive.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	content := []byte("#!/bin/true\n")
	if err := tw.WriteHeader(&tar.Header{Name: "bridge", Typeflag: tar.TypeReg, Size: int64(len(content)), Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	f.Close()

	dst := filepath.Join(t.TempDir(), "bridge")
	if err := extractTarGzBinary(p, "bridge", dst); err != nil {
		t.Fatalf("regular-file entry: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("extracted content = %q, want %q", got, content)
	}
}

// TestExtractZipBinary_SkipsNonRegularEntry mirrors the tar test for the
// Windows zip path: a non-regular (symlink) entry named like the binary
// must be skipped via the FileInfo mode check.
func TestExtractZipBinary_SkipsNonRegularEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "archive.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	hdr := &zip.FileHeader{Name: "bridge.exe"}
	hdr.SetMode(os.ModeSymlink | 0o777)
	if _, err := zw.CreateHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dst := filepath.Join(t.TempDir(), "bridge.exe")
	err = extractZipBinary(p, "bridge.exe", dst)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("zip symlink entry: err = %v; want a 'not found' error", err)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Errorf("dst was written from a non-regular zip entry; want no file at %s", dst)
	}
}
