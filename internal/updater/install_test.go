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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
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
	archiveFetches atomic.Int32
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
		fix.archiveFetches.Add(1)
		w.Write(archiveBytes)
	})
	mux.HandleFunc("/asset/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write(checksumsBytes)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fix.server = srv
	allowTestAssetHost(t, srv.URL)

	rel := Release{
		TagName: "v" + latestVersion,
		HTMLURL: srv.URL + "/release/v" + latestVersion,
		Assets: []ReleaseAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/asset/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/asset/checksums.txt"},
		},
	}
	releaseJSON, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}
	fix.releaseJSON = releaseJSON
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
	// F2: the marker has to say the swap actually happened, or the
	// boot-time rollback can't tell a real failed install from one that
	// died before touching the filesystem — and refuses the restore.
	if !st.SwapStarted {
		t.Error("state.SwapStarted = false after a completed swap; the boot rollback would refuse to restore .bak")
	}
}

// TestInstallSwapAbortsBeforeMutatingWhenMarkerWriteFails pins the
// hook's placement: it fires BEFORE the first destructive filesystem
// operation, so a failure there leaves the binary and any pre-existing
// .bak exactly as they were. That ordering is what makes SwapStarted
// mean "restoring .bak is a rollback, not a downgrade".
func TestInstallSwapAbortsBeforeMutatingWhenMarkerWriteFails(t *testing.T) {
	dir := t.TempDir()
	live := fakeBinary(t, dir, "bridge", "CURRENT")
	_ = fakeBinary(t, dir, "bridge.bak", "PREVIOUS")
	newBin := fakeBinary(t, dir, "bridge.new", "NEW")

	wantErr := errors.New("simulated marker write failure")
	err := swapBinary(live, newBin, ".bak", func() error { return wantErr })
	if !errors.Is(err, wantErr) {
		t.Fatalf("swapBinary with a failing swap-started hook: err = %v, want it to wrap %v", err, wantErr)
	}
	if got, _ := os.ReadFile(live); string(got) != "CURRENT" {
		t.Errorf("live = %q after an aborted swap, want CURRENT (untouched)", string(got))
	}
	if got, _ := os.ReadFile(live + ".bak"); string(got) != "PREVIOUS" {
		t.Errorf(".bak = %q after an aborted swap, want PREVIOUS (untouched)", string(got))
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
	allowTestAssetHost(t, tampered.URL)

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
	releaseJSON, err := json.Marshal(rel)
	if err != nil {
		t.Fatalf("marshal tampered release: %v", err)
	}
	fix.releaseJSON = releaseJSON

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(tampered.URL),
	})
	upd.mu.Lock()
	upd.status.LatestVersion = "0.2.0"
	upd.status.UpdateAvailable = true
	upd.mu.Unlock()

	_, err = upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
		Verifier:   noopVerifier,
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

// Phase C compat-gate end-to-end: when the fixture's release-meta
// .json declares a MinClientVersion floor and the operator-supplied
// TokenSnapshot includes a token at a lower version, Install must
// refuse with ErrCompatGateRefused. The gate runs BEFORE download
// + extract, so the fixture's archive isn't even fetched —
// mux-counter not strictly necessary, the error type is the proof.
func TestInstall_CompatGateRefusesOrphanRelease(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")

	// Augment the fixture: serve release-meta.json under /asset/,
	// and re-publish the release JSON with the new asset attached.
	metaBody := []byte(`{"version":"0.2.0","minClientVersion":"1.5.0","protocolVersion":1}`)
	fix.server.Config.Handler.(*http.ServeMux).HandleFunc(
		"/asset/release-meta.json",
		func(w http.ResponseWriter, r *http.Request) { w.Write(metaBody) },
	)
	rel := Release{
		TagName: "v" + fix.latestVersion,
		HTMLURL: fix.server.URL + "/release/v" + fix.latestVersion,
		Assets: []ReleaseAsset{
			{Name: fix.archiveName, BrowserDownloadURL: fix.server.URL + "/asset/" + fix.archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: fix.server.URL + "/asset/checksums.txt"},
			{Name: ReleaseMetaAssetName, BrowserDownloadURL: fix.server.URL + "/asset/release-meta.json"},
		},
	}
	releaseJSON, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	fix.releaseJSON = releaseJSON

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	// TokenSnapshot returns an old token at 1.0.0 — below the
	// fixture's 1.5.0 floor.
	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
		TokenSnapshot: func() []auth.Token {
			return []auth.Token{
				{ID: "ABCDEF", Name: "iPhone Old", LastClientVersion: "1.0.0"},
			}
		},
	})
	upd.mu.Lock()
	upd.status.CurrentVersion = "0.1.0"
	upd.status.LatestVersion = fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()

	_, err = upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
		Verifier:   noopVerifier,
	})
	if !errors.Is(err, ErrCompatGateRefused) {
		t.Fatalf("Install: err = %v, want ErrCompatGateRefused", err)
	}

	// Live binary unchanged (gate blocked download/swap).
	got, _ := os.ReadFile(livePath)
	if string(got) != "bridge-binary-0.1.0" {
		t.Errorf("live binary changed despite refused install: %q", string(got))
	}

	// Status surfaces the deferred reason.
	st := upd.Status()
	if st.DeferredReason == "" {
		t.Errorf("Status.DeferredReason should explain the gate refusal")
	}
}

// OverrideCompatGate must let the install proceed despite the
// orphan-risking floor. Operator-only path; the auto-installer
// never sets this flag.
func TestInstall_CompatGateOverrideAllowsOrphanRelease(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	metaBody := []byte(`{"version":"0.2.0","minClientVersion":"1.5.0","protocolVersion":1}`)
	fix.server.Config.Handler.(*http.ServeMux).HandleFunc(
		"/asset/release-meta.json",
		func(w http.ResponseWriter, r *http.Request) { w.Write(metaBody) },
	)
	rel := Release{
		TagName: "v" + fix.latestVersion,
		HTMLURL: fix.server.URL + "/release/v" + fix.latestVersion,
		Assets: []ReleaseAsset{
			{Name: fix.archiveName, BrowserDownloadURL: fix.server.URL + "/asset/" + fix.archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: fix.server.URL + "/asset/checksums.txt"},
			{Name: ReleaseMetaAssetName, BrowserDownloadURL: fix.server.URL + "/asset/release-meta.json"},
		},
	}
	releaseJSON, _ := json.Marshal(rel)
	fix.releaseJSON = releaseJSON

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
		TokenSnapshot: func() []auth.Token {
			return []auth.Token{
				{ID: "ABCDEF", Name: "iPhone Old", LastClientVersion: "1.0.0"},
			}
		},
	})
	upd.mu.Lock()
	upd.status.CurrentVersion = "0.1.0"
	upd.status.LatestVersion = fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:            dir,
		BinaryPath:         livePath,
		Force:              true,
		Verifier:           noopVerifier,
		OverrideCompatGate: true, // operator override
	})
	if err != nil {
		t.Fatalf("Install with override should succeed: %v", err)
	}

	// Live binary swapped to the new version despite orphan risk.
	got, _ := os.ReadFile(livePath)
	if string(got) != "bridge-binary-0.2.0" {
		t.Errorf("override didn't swap binary: live = %q", string(got))
	}
}

// Backwards compat: a release that doesn't ship release-meta.json
// (any pre-Phase-C build) must NOT trip the gate. The fetcher
// returns ErrReleaseMetaMissing and Install treats it as no floor.
func TestInstall_NoReleaseMetaIsNoFloor(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
		// Even with an old token in the snapshot, the absent meta
		// means "no floor" so the gate stays permissive.
		TokenSnapshot: func() []auth.Token {
			return []auth.Token{
				{ID: "ABCDEF", Name: "iPhone Old", LastClientVersion: "0.1.0"},
			}
		},
	})

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-0.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	upd.mu.Lock()
	upd.status.CurrentVersion = "0.1.0"
	upd.status.LatestVersion = fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()

	_, err := upd.Install(context.Background(), InstallOptions{
		DataDir:    dir,
		BinaryPath: livePath,
		Force:      true,
		Verifier:   noopVerifier,
	})
	if err != nil {
		t.Fatalf("Install (no release-meta) should succeed: %v", err)
	}
}

// Test_sanitizeAssetName pins the path-component sanitiser the Install download
// path relies on: filepath.Base must strip directory + traversal segments, and
// the "."/".." residue plus any leftover path separator (which would escape the
// scratch dir on Join — including a Windows "\" that survives filepath.Base on
// non-Windows hosts) must be rejected with an error. Gemini security-MEDIUM on
// PR #368; cross-platform separator reject per the r2 review.
func Test_sanitizeAssetName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain_asset", "bridge-linux-amd64.tar.gz", "bridge-linux-amd64.tar.gz", false},
		{"strips_traversal_prefix", "../../etc/passwd", "passwd", false},
		{"strips_dir_prefix", "dir/sub/asset.zip", "asset.zip", false},
		{"strips_absolute_prefix", "/var/tmp/asset.tar", "asset.tar", false},
		{"dotdot_rejected", "..", "", true},
		{"dot_rejected", ".", "", true},
		{"empty_rejected", "", "", true}, // filepath.Base("") == "."
		{"trailing_dotdot_rejected", "foo/..", "", true},
		{"trailing_dot_rejected", "foo/.", "", true},
		{"backslash_segments_rejected", "..\\..\\x", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeAssetName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("sanitizeAssetName(%q) = %q, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeAssetName(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("sanitizeAssetName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Test_isBinaryEntry pins the archive-entry matcher: bare basename or a
// nested-under-release-dir layout matches, traversal is rejected on the
// RAW entry (the prior post-Clean check was dead code), and basename
// mismatches are skipped.
func Test_isBinaryEntry(t *testing.T) {
	const bin = "bridge"
	cases := []struct {
		entry string
		want  bool
	}{
		{"bridge", true},
		{"dist/bridge", true},
		{"bridge_1.2.3_linux_amd64/bridge", true},
		{"bridge.exe", false},       // wrong basename for a unix binary
		{"notbridge", false},        // basename mismatch
		{"../bridge", false},        // traversal rejected
		{"../../etc/bridge", false}, // traversal rejected
		{"foo/../bridge", false},    // raw ".." rejected (stricter than pre-fix)
	}
	for _, c := range cases {
		if got := isBinaryEntry(c.entry, bin); got != c.want {
			t.Errorf("isBinaryEntry(%q, %q) = %v, want %v", c.entry, bin, got, c.want)
		}
	}
}

// Test_preflightWritable_FailsWhenCannotDelete pins the delete-permission
// probe: if the scratch file can't be removed (a "create but not delete"
// ACL), preflight must fail with ErrPathNotWritable rather than passing and
// letting the later binary swap fail.
func Test_preflightWritable_FailsWhenCannotDelete(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bridge")
	orig := removeFunc
	t.Cleanup(func() { removeFunc = orig })
	removeFunc = func(string) error { return os.ErrPermission } // always fail

	err := preflightWritable(bin)
	if !errors.Is(err, ErrPathNotWritable) {
		t.Errorf("preflightWritable with un-deletable scratch = %v, want ErrPathNotWritable", err)
	}
}

// Test_preflightWritable_SucceedsAfterTransientRemoveFailure pins the retry
// backoff that absorbs a transient AV oplock on a freshly-closed temp file:
// a first-attempt failure followed by success must NOT fail the preflight.
func Test_preflightWritable_SucceedsAfterTransientRemoveFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bridge")
	orig := removeFunc
	t.Cleanup(func() { removeFunc = orig })
	calls := 0
	removeFunc = func(name string) error {
		calls++
		if calls == 1 {
			return os.ErrPermission // transient on first try
		}
		return orig(name) // real removal on retry
	}

	if err := preflightWritable(bin); err != nil {
		t.Errorf("preflightWritable should succeed after a transient remove failure, got %v", err)
	}
	if calls < 2 {
		t.Errorf("expected a retry after the first failure, got %d call(s)", calls)
	}
}

// Test_preflightWritable_LeavesAtMostOneProbeFile pins the F4 fix: the
// probe's whole purpose is to detect a "can create but not delete"
// directory, and in exactly that case the probe file stays on disk. With
// os.CreateTemp's random suffix every attempt leaked a fresh undeletable
// file — and Install runs the preflight on EVERY attempt, so auto-install
// at the 6 h cadence accumulated ~4/day in e.g. /usr/local/bin,
// indefinitely. A fixed name caps the leak at one.
func Test_preflightWritable_LeavesAtMostOneProbeFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bridge")
	orig := removeFunc
	t.Cleanup(func() { removeFunc = orig })
	removeFunc = func(string) error { return os.ErrPermission } // never deletable

	for i := 0; i < 3; i++ {
		if err := preflightWritable(bin); !errors.Is(err, ErrPathNotWritable) {
			t.Fatalf("attempt %d: err = %v, want ErrPathNotWritable", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var probes []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), writeProbeName) {
			probes = append(probes, e.Name())
		}
	}
	if len(probes) != 1 {
		t.Errorf("3 failed preflights left %d probe file(s) %v; want exactly 1", len(probes), probes)
	}
	if len(probes) > 0 && probes[0] != writeProbeName {
		t.Errorf("probe file = %q, want the fixed name %q", probes[0], writeProbeName)
	}
}

// parkedInstall is an Install parked mid-flight holding the
// installInFlight try-lock, arranged so concurrency tests can
// exercise a second caller against the held lock.
type parkedInstall struct {
	livePath string
	upd      *Updater
	release  func()       // lets the parked install run to completion
	waitErr  func() error // blocks until the install goroutine returns
}

// parkInstallMidVerify builds the whole arrange half (fake release
// server, temp data dir with a live binary at oldVersion, updater
// seeded "newVersion is available") and starts an Install whose
// blocking verifier parks it — lock held — until release is called.
func parkInstallMidVerify(t *testing.T, oldVersion, newVersion string) *parkedInstall {
	t.Helper()
	fix := newInstallFixture(t, newVersion)

	dir := t.TempDir()
	livePath := filepath.Join(dir, "bridge")
	if err := os.WriteFile(livePath, []byte("bridge-binary-"+oldVersion), 0o755); err != nil {
		t.Fatal(err)
	}

	upd := New(Options{
		RepoOverride: "fake/repo",
		Client:       NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL),
	})
	upd.mu.Lock()
	upd.status.CurrentVersion = oldVersion
	upd.status.LatestVersion = fix.latestVersion
	upd.status.UpdateAvailable = true
	upd.status.LastCheck = time.Now().UTC()
	upd.mu.Unlock()

	// The blocking verifier parks the install mid-flight (lock held)
	// until the test releases it.
	started := make(chan struct{})
	released := make(chan struct{})
	var once sync.Once
	blockingVerifier := func(context.Context, string) error {
		once.Do(func() { close(started) })
		<-released
		return nil
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := upd.Install(context.Background(), InstallOptions{
			DataDir:    dir,
			BinaryPath: livePath,
			Force:      true,
			Verifier:   blockingVerifier,
		})
		errCh <- err
	}()
	<-started // install now holds the lock, parked in verify

	return &parkedInstall{
		livePath: livePath,
		upd:      upd,
		release:  func() { close(released) },
		waitErr:  func() error { return <-errCh },
	}
}

// TestInstallConcurrentCallsSerialized pins the install-in-flight
// try-lock: while one Install holds the Updater, a second caller must
// fail fast with ErrInstallInFlight rather than race the first on the
// scratch dir, the .bak rename target, and update-state.json (one
// installer's deferred cleanScratch could otherwise delete the other's
// verified binary mid-swap). Once the first completes, the lock is
// released and a fresh attempt succeeds.
func TestInstallConcurrentCallsSerialized(t *testing.T) {
	parked := parkInstallMidVerify(t, "0.1.0", "0.2.0")
	retryOpts := InstallOptions{
		DataDir:    filepath.Dir(parked.livePath),
		BinaryPath: parked.livePath,
		Force:      true,
		Verifier:   noopVerifier,
	}

	if _, err := parked.upd.Install(context.Background(), retryOpts); !errors.Is(err, ErrInstallInFlight) {
		t.Errorf("concurrent Install: err = %v, want ErrInstallInFlight", err)
	}
	parked.release()
	if err := parked.waitErr(); err != nil {
		t.Fatalf("first Install: %v", err)
	}

	// Lock released: a follow-up attempt proceeds.
	if _, err := parked.upd.Install(context.Background(), retryOpts); err != nil {
		t.Fatalf("Install after the first completed: %v (lock not released?)", err)
	}
}

// TestInstallScratchIsPerAttempt pins the scratch-dir shape: every
// attempt works inside its own MkdirTemp directory directly under
// DataDir (no persistent shared parent) and cleanScratch removes it —
// DataDir must never carry an attempt's leftover "install-*" files, so
// a stray cleanup from a concurrent caller (incl. the CLI's separate
// process) can never delete an in-flight attempt's verified binary.
func TestInstallScratchIsPerAttempt(t *testing.T) {
	fix := newInstallFixture(t, "0.2.0")
	livePath, _, err := fix.install(t, "0.1.0")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	dataDir := filepath.Dir(livePath)
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "install-") {
			t.Errorf("data dir holds leftover attempt dir %q — per-attempt scratch not cleaned", e.Name())
		}
	}
}

// TestInstallConcurrentRollbackSerialized pins the same try-lock on
// the rollback path: while an Install holds the Updater, an operator
// Rollback (the admin "Roll back" button) must fail fast with
// ErrInstallInFlight rather than race the install on the .bak rename
// target and update-state.json. Once the install completes, the lock
// is released and the rollback proceeds.
func TestInstallConcurrentRollbackSerialized(t *testing.T) {
	parked := parkInstallMidVerify(t, "0.1.0", "0.2.0")
	rollbackOpts := InstallOptions{
		DataDir:    filepath.Dir(parked.livePath),
		BinaryPath: parked.livePath,
		Force:      true,
	}

	if err := parked.upd.Rollback(rollbackOpts); !errors.Is(err, ErrInstallInFlight) {
		t.Errorf("Rollback during Install: err = %v, want ErrInstallInFlight", err)
	}
	parked.release()
	if err := parked.waitErr(); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Lock released: the rollback proceeds (the just-completed install
	// left a .bak) and restores the previous binary.
	if err := parked.upd.Rollback(rollbackOpts); err != nil {
		t.Fatalf("Rollback after Install completed: %v (lock not released?)", err)
	}
	got, err := os.ReadFile(parked.livePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bridge-binary-0.1.0" {
		t.Errorf("post-rollback live = %q, want bridge-binary-0.1.0", string(got))
	}
}
