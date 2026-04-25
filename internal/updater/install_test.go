//go:build !windows

package updater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// installFixture stands up a fake GitHub releases server serving:
//   - GET /releases/latest → release JSON pointing at our archive + checksums
//   - GET /<archiveName>   → tar.gz containing a single "bridge" binary
//   - GET /checksums.txt   → goreleaser-style "<sha256>  <name>"
//
// The archive's bridge binary writes the latestVersion as its sole
// content so the test can verify the swap landed the right bytes.
type installFixture struct {
	server         *httptest.Server
	latestVersion  string
	archiveName    string
	expectedHash   string
	releaseJSON    []byte
	archiveBytes   []byte
	checksumsBytes []byte
}

func newInstallFixture(t *testing.T, latestVersion string) *installFixture {
	t.Helper()
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	archiveName := fmt.Sprintf("1-bit-bridge_%s_%s_%s.tar.gz",
		latestVersion, osName, runtime.GOARCH)

	// Build the tar.gz containing one file named "bridge" whose
	// contents identify the version. The post-swap test reads the
	// live binary and compares against this body.
	binaryBody := []byte("bridge-binary-" + latestVersion)
	var tarBuf bytes.Buffer
	gz := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "bridge",
		Mode: 0o755,
		Size: int64(len(binaryBody)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binaryBody); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	archiveBytes := tarBuf.Bytes()

	hash := sha256.Sum256(archiveBytes)
	expectedHash := hex.EncodeToString(hash[:])
	checksumsBytes := []byte(fmt.Sprintf("%s  %s\n", expectedHash, archiveName))

	fix := &installFixture{
		latestVersion:  latestVersion,
		archiveName:    archiveName,
		expectedHash:   expectedHash,
		archiveBytes:   archiveBytes,
		checksumsBytes: checksumsBytes,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fake/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fix.releaseJSON)
	})
	mux.HandleFunc("/asset/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archiveBytes)
	})
	mux.HandleFunc("/asset/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write(checksumsBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fix.server = srv

	rel := Release{
		TagName: "v" + latestVersion,
		HTMLURL: srv.URL + "/release/v" + latestVersion,
		Assets: []ReleaseAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/asset/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/asset/checksums.txt"},
		},
	}
	fix.releaseJSON, _ = json.Marshal(rel)
	return fix
}

// install plumbs an Updater + tempdir + fake binary, runs Install,
// and returns the live binary path so the test can assert on it.
func (f *installFixture) install(t *testing.T, currentVersion string) (string, *Updater, error) {
	t.Helper()
	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-"+currentVersion), 0o755); err != nil {
		t.Fatal(err)
	}

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(f.server.URL),
	})
	// Force-set the cached status to "update is available" so Install
	// proceeds without first requiring a poll. (Tests that exercise
	// the poll path do so separately in updater_test.go.)
	upd.mu.Lock()
	upd.status.CurrentVersion = currentVersion
	upd.status.LatestVersion = f.latestVersion
	upd.status.UpdateAvailable = true
	upd.status.ReleaseNotesURL = f.server.URL + "/release/v" + f.latestVersion
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
		Verifier:   noopVerifier, // unsigned test archive
	})
	return livePath, upd, err
}

// noopVerifier skips the codesign / Team-ID check. Tests use it
// because the fake-archive bridge binary isn't Apple-signed.
// Production InstallOptions leave Verifier nil so verifyBinary runs.
func noopVerifier(_ context.Context, _ string) error { return nil }

func TestInstallReplacesBinaryAndArmsMarker(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, _, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Live binary now contains the new version's body.
	got, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bridge-binary-0.2.0" {
		t.Errorf("post-install live = %q, want bridge-binary-0.2.0", string(got))
	}

	// .bak holds the previous version.
	bak, err := os.ReadFile(livePath + ".bak")
	if err != nil {
		t.Fatalf("read .bak: %v", err)
	}
	if string(bak) != "bridge-binary-0.1.0" {
		t.Errorf(".bak = %q, want bridge-binary-0.1.0", string(bak))
	}

	// State marker recorded.
	st, err := LoadState(filepath.Dir(livePath))
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != "installing" {
		t.Errorf("state.Status = %q, want installing", st.Status)
	}
	if st.TargetVersion != "0.2.0" {
		t.Errorf("state.TargetVersion = %q, want 0.2.0", st.TargetVersion)
	}
}

func TestInstallRefusesWithActiveSessions(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	os.WriteFile(livePath, []byte("OLD"), 0o755)

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
	})
	upd.mu.Lock()
	upd.status.LatestVersion = "0.2.0"
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()

	tracker := NewTracker()
	tracker.Begin() // simulate an inflight download
	defer tracker.End()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Sessions:   tracker,
		Force:      false,
		Verifier:   noopVerifier,
	})
	if !errors.Is(err, ErrActiveSessions) {
		t.Fatalf("expected ErrActiveSessions, got %v", err)
	}
	// Live binary untouched.
	got, _ := os.ReadFile(livePath)
	if string(got) != "OLD" {
		t.Errorf("Install with active session mutated live binary: %q", string(got))
	}
}

func TestInstallForceOverridesActiveSessions(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	os.WriteFile(livePath, []byte("OLD"), 0o755)

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
	})
	upd.mu.Lock()
	upd.status.LatestVersion = "0.2.0"
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()

	tracker := NewTracker()
	tracker.Begin()
	defer tracker.End()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Sessions:   tracker,
		Force:      true,
		Verifier:   noopVerifier,
	})
	if err != nil {
		t.Fatalf("Install with Force=true: %v (should succeed)", err)
	}
	got, _ := os.ReadFile(livePath)
	if !strings.HasPrefix(string(got), "bridge-binary-0.2.0") {
		t.Errorf("post-force-install live = %q", string(got))
	}
}

func TestInstallRefusesWhenNoUpdateAvailable(t *testing.T) {
	upd := New(Options{RepoOverride: "fake/repo"})
	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	os.WriteFile(livePath, []byte("v"), 0o755)

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
	})
	if !errors.Is(err, ErrNoUpdate) {
		t.Errorf("expected ErrNoUpdate, got %v", err)
	}
}

func TestInstallRejectsCorruptArchive(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	// Tamper: rewrite the archive bytes to break the SHA-256 match
	// after the fixture has computed the expected hash. The fake
	// server still serves whatever the new bytes are, but its
	// /checksums.txt response still claims the original hash.
	fix.archiveBytes = append(fix.archiveBytes, []byte("tampered")...)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/fake/repo/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fix.releaseJSON)
	})
	mux.HandleFunc("/asset/"+fix.archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(fix.archiveBytes)
	})
	mux.HandleFunc("/asset/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write(fix.checksumsBytes)
	})
	tampered := httptest.NewServer(mux)
	defer tampered.Close()

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	os.WriteFile(livePath, []byte("OLD"), 0o755)

	rel := Release{
		TagName: "v0.2.0",
		Assets: []ReleaseAsset{
			{Name: fix.archiveName, BrowserDownloadURL: tampered.URL + "/asset/" + fix.archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: tampered.URL + "/asset/checksums.txt"},
		},
	}
	fix.releaseJSON, _ = json.Marshal(rel)

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(tampered.URL),
	})
	upd.mu.Lock()
	upd.status.LatestVersion = "0.2.0"
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
	})
	if err == nil {
		t.Fatal("Install with bad checksum should fail")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("expected checksum-mismatch error, got %v", err)
	}
	// Live binary untouched.
	got, _ := os.ReadFile(livePath)
	if string(got) != "OLD" {
		t.Errorf("failed install mutated live binary: %q", string(got))
	}
	// State marker NOT armed (we cleared it on swap failure — and
	// in the checksum case we never even saved it).
	st, _ := LoadState(dir)
	if st.Status == "installing" {
		t.Error("checksum failure should not have armed the install marker")
	}
}

func TestArchiveAndChecksumForRequiresMatchingPlatformAsset(t *testing.T) {
	rel := &Release{
		TagName: "v0.2.0",
		Assets: []ReleaseAsset{
			{Name: "1-bit-bridge_0.2.0_haiku_riscv.tar.gz"},
			{Name: "checksums.txt"},
		},
	}
	if _, _, err := archiveAndChecksumFor(rel); err == nil {
		t.Error("expected ErrNoMatchingAsset for unrelated platform asset")
	}
}
