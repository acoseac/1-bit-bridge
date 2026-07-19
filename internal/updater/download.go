package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// archiveAndChecksumFor finds the goreleaser-conventional archive
// asset + checksums.txt asset for the host's GOOS/GOARCH within a
// Release.
//
// Naming convention (matches .goreleaser.yaml in this repo):
//
//	1-bit-bridge_<version>_macos_arm64.tar.gz   (darwin → "macos")
//	1-bit-bridge_<version>_linux_amd64.tar.gz
//	1-bit-bridge_<version>_windows_amd64.zip
//	checksums.txt
//
// Returns ErrNoMatchingAsset when nothing matches — the operator
// either downloaded a release missing assets for their platform, or
// goreleaser changed naming under us. The error mentions both the
// expected pattern and the assets the release actually published so
// the operator has enough to debug from the admin UI.
func archiveAndChecksumFor(rel *Release) (archive, checksums *ReleaseAsset, err error) {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos" // goreleaser's archive name template
	}
	arch := runtime.GOARCH
	ext := ".tar.gz"
	if runtime.GOOS == "windows" {
		ext = ".zip"
	}
	suffix := "_" + osName + "_" + arch + ext

	for i := range rel.Assets {
		a := &rel.Assets[i]
		switch {
		case a.Name == "checksums.txt":
			checksums = a
		case strings.HasSuffix(a.Name, suffix):
			archive = a
		}
	}
	if archive == nil {
		names := make([]string, 0, len(rel.Assets))
		for _, a := range rel.Assets {
			names = append(names, a.Name)
		}
		return nil, nil, fmt.Errorf("%w: looking for *%s, release published %v",
			ErrNoMatchingAsset, suffix, names)
	}
	if checksums == nil {
		return nil, nil, fmt.Errorf("%w: release published the archive but no checksums.txt",
			ErrNoMatchingAsset)
	}
	return archive, checksums, nil
}

// ErrNoMatchingAsset means a release's assets don't include
// something the install path can use — different to "GitHub is
// down" or "checksum mismatch", and surfaced separately to the
// operator.
var ErrNoMatchingAsset = errors.New("no matching asset")

// downloadVerified streams the archive into dst and verifies its
// SHA-256 matches the entry in checksums.txt. The two URLs are
// fetched in sequence on the same http.Client (TLS to github.com
// cached) so the checksum file is the immediate sibling of the
// asset, not a stale cache.
//
// Returns the verified archive's hex SHA-256 on success — used by
// install bookkeeping for diagnostic logging.
// maxArchiveDownloadBytes caps the release archive written to disk
// before its checksum is verified. The bridge archive is ~30 MiB; 1 GiB
// is generous headroom while bounding the damage from a hung CDN or a
// compromised release that streams endlessly — the checksum is only
// checked AFTER the full download, so without a bound the disk would
// fill before the mismatch is caught. Matches the extract-side ceiling
// in writeExecutable.
//
// A var (not a const) solely so tests can shrink it — same test-seam
// convention as renameFunc / commandContext elsewhere in the repo.
// Production code MUST NOT mutate it.
var maxArchiveDownloadBytes int64 = 1 << 30

func downloadVerified(ctx context.Context, hc *http.Client,
	archiveURL, archiveName, checksumsURL string,
	dst string,
) (string, error) {
	// Fetch checksums.txt first so we know the expected hash before
	// committing disk to the archive download.
	expected, err := fetchChecksum(ctx, hc, checksumsURL, archiveName)
	if err != nil {
		return "", fmt.Errorf("read checksum for %s: %w", archiveName, err)
	}

	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create archive dst: %w", err)
	}
	defer out.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, archiveURL, nil)
	if err != nil {
		return "", fmt.Errorf("build archive request: %w", err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch archive: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("archive fetch status %d", resp.StatusCode)
	}

	h := sha256.New()
	// Bound the write-to-disk. Read one byte past the cap so a legit
	// exactly-cap-sized archive isn't false-rejected; n > cap means the
	// source overran and we bail with an explicit error (before the
	// checksum compare, which would otherwise report a confusing generic
	// mismatch). The truncated dst is never committed — install swaps
	// only on a returned nil error.
	tee := io.TeeReader(io.LimitReader(resp.Body, maxArchiveDownloadBytes+1), h)
	n, err := io.Copy(out, tee)
	if err != nil {
		return "", fmt.Errorf("write archive: %w", err)
	}
	if n > maxArchiveDownloadBytes {
		return "", fmt.Errorf("archive exceeds size limit of %d bytes", maxArchiveDownloadBytes)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return "", fmt.Errorf("archive checksum mismatch: got %s, want %s",
			got, expected)
	}
	return got, nil
}

// fetchChecksum pulls checksums.txt and returns the SHA-256 hex for
// the row that mentions archiveName. The goreleaser format is one
// "<sha256> <name>" line per artifact (sha256sum-style).
func fetchChecksum(ctx context.Context, hc *http.Client, url, archiveName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("checksums fetch status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // checksums.txt is tiny
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// goreleaser writes "<hash>  <name>" (two spaces, but
		// strings.Fields handles any whitespace). Match by
		// basename so a checksums file using "./<name>" or
		// "binaries/<name>" prefixes still works.
		name := fields[1]
		if path.Base(name) == archiveName {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s", archiveName)
}

// extractBridgeBinary walks the verified archive and copies the
// "bridge" / "bridge.exe" binary into dst. Hardened against
// path-traversal (no ZipSlip / TarSlip): we ignore everything that
// isn't the bare binary name and refuse any entry whose name
// includes a directory separator pointing outside.
//
// archivePath must already have been verified by downloadVerified.
func extractBridgeBinary(archivePath, dst string) error {
	binaryName := "bridge"
	if runtime.GOOS == "windows" {
		binaryName = "bridge.exe"
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZipBinary(archivePath, binaryName, dst)
	}
	return extractTarGzBinary(archivePath, binaryName, dst)
}

func extractTarGzBinary(archivePath, binaryName, dst string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip open: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		if !isBinaryEntry(hdr.Name, binaryName) {
			continue
		}
		// Only a regular file is a real binary. A tampered/malformed
		// archive could carry a symlink, directory, or device node
		// sharing the basename; feeding one to writeExecutable would
		// install a 0-byte or bogus binary. Skip and keep scanning.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		return writeExecutable(dst, tr, hdr.Size)
	}
	return fmt.Errorf("archive %s: %s not found", filepath.Base(archivePath), binaryName)
}

func extractZipBinary(archivePath, binaryName, dst string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("zip open: %w", err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if !isBinaryEntry(zf.Name, binaryName) {
			continue
		}
		// archive/zip has no Typeflag; the file mode is the equivalent
		// guard. A directory or symlink entry named like the binary must
		// not be extracted (see the tar path's tar.TypeReg check).
		if !zf.Mode().IsRegular() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return fmt.Errorf("zip entry open: %w", err)
		}
		defer rc.Close()
		return writeExecutable(dst, rc, int64(zf.UncompressedSize64))
	}
	return fmt.Errorf("archive %s: %s not found", filepath.Base(archivePath), binaryName)
}

// isBinaryEntry tolerates the few archive layouts goreleaser
// produces (bare basename, sometimes nested under a release
// directory) while rejecting traversal attempts.
func isBinaryEntry(entry, binaryName string) bool {
	// Reject traversal on the RAW entry. The prior post-Clean check
	// (`strings.Contains(path.Clean("/"+entry), "/..")`) was dead code —
	// path.Clean resolves ".." away, so the cleaned form can never
	// contain "/..". path.Base below is the load-bearing guard (a
	// basename is never ".." and can't contain a separator), but a raw
	// reject fails fast and documents the intent. goreleaser archive
	// names never contain "..", so this can't false-reject a real asset.
	if strings.Contains(entry, "..") {
		return false
	}
	return path.Base(entry) == binaryName
}

// writeExecutable copies up to size bytes from src into dst, sets
// the executable bit, and fsyncs. The size cap defends against a
// tampered archive header lying about its content length.
func writeExecutable(dst string, src io.Reader, size int64) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create extracted binary: %w", err)
	}
	defer out.Close()
	// LimitReader provides the size cap; the caller's checksum
	// already verified the archive bytes match, but a malformed
	// header in the archive itself shouldn't blow up disk.
	//
	// Track "declared size present" with a bool rather than reusing
	// 1<<30 as a sentinel for "no declared size". Overloading the
	// ceiling value let a tar/zip entry declaring EXACTLY 1 GiB skip
	// the truncation check (size == 1<<30 suppressed the guard), so a
	// truncated 1 GiB-declared payload would be silently accepted.
	hasDeclaredSize := size > 0
	if !hasDeclaredSize {
		size = 1 << 30 // 1 GiB ceiling — bridge binary is ~30 MiB
	}
	n, err := io.Copy(out, io.LimitReader(src, size))
	if err != nil {
		return fmt.Errorf("write extracted binary: %w", err)
	}
	if hasDeclaredSize && n != size {
		return fmt.Errorf("extracted binary size mismatch: got %d, want %d", n, size)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("sync extracted binary: %w", err)
	}
	return nil
}
